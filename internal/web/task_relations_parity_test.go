package web

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// The task-relations verdict (blockerVerdict in assets/task-model.js) keeps
// its own copy of the server's lifecycle vocabulary: LIFECYCLE_UNFINISHED
// lists every state that confirms a blocker ("" is the wire encoding of a
// valid unscheduled task — the relation payload's SourceState is a non-pointer
// LifecycleState), and LIFECYCLE_DONE is the one state that clears a blocker.
// The server side of the vocabulary is enumerated in coordinator.AllLifecycleStates
// (kept exhaustive by TestAllLifecycleStatesExhaustive in internal/coordinator,
// which parses every LifecycleState constant in the coordinator package with
// go/parser and evaluates constant expressions with go/constant). These tests
// extract the JS exports with a syntax-aware tokenizer
// — comments never contribute tokens, template literals and regular-expression
// literals are consumed whole (so a regex or template that quotes an export
// declaration cannot be mistaken for the real one, including regex statements
// in dead branches like `if (false) /.../` or `if (false) {} /.../`, where a /
// starts a regex exactly as ECMAScript lexes it), string literals are
// decoded exactly as JavaScript evaluates them (strict-mode escapes), and
// every Set element must be exactly a string-literal array element, so
// "paused".toUpperCase(), ...["unsupported"], or an elision cannot pass parity
// for a vocabulary the runtime does not have — nor can a later mutation of the
// exported Set (`LIFECYCLE_UNFINISHED.delete("paused")`) change the runtime
// vocabulary after the extractor has read the initializer — iterate the
// enumeration in both
// directions, and fail if the two vocabularies drift, so a new server-side
// lifecycle state cannot silently render as neutral unknown instead of
// confirmed blocking.

type jsTokenKind int

const (
	jsString   jsTokenKind = iota // a '...' or "..." literal; text holds its decoded contents
	jsOther                       // any other token; text holds the raw source
	jsTemplate                    // a template literal; consumed whole, text holds the raw source
	jsRegex                       // a regular-expression literal; consumed whole, text holds the raw source
)

type jsToken struct {
	kind jsTokenKind
	text string
}

// jsScanner is the low-level cursor for jsTokens. It understands // and /* */
// comments, backslash escapes inside string literals, template literals with
// ${...} interpolations, and regular-expression literals. last is the most
// recent significant token (templates and regexes included), which decides
// whether a following / starts a regex or is division, and the paren/brace
// stacks track which ) closes a control-flow header and which } closes a
// statement block, so a / after `if (false)` or `if (false) {}` starts a
// regex exactly as ECMAScript lexes it.
type jsScanner struct {
	src               string
	pos               int
	last              *jsToken
	parenKinds        []bool // per ( : true when it opens a control-flow header (if/while/for/with/switch)
	braceKinds        []bool // per { : true when it opens a statement block rather than an object literal
	afterHeaderParen  bool   // the most recent ) closed a control-flow header, so the next / is a regex
	lastBraceWasBlock bool   // the most recent } closed a statement block, so the next / is a regex
}

// record makes tok the most recent significant token for regex/division
// disambiguation and maintains the paren/brace stacks. A `(` directly after a
// control-flow keyword opens a header; the matching `)` marks the scanner so a
// / right after it starts a regex (`if (false) /regex/` is a regex statement,
// `f() / 2` is division). A `{` after a statement position (a header, else,
// do, try, finally, ;, }, :, >, or another {) opens a statement block; a / right
// after its `}` starts a regex too (`if (false) {} /regex/`), while a `}` that
// closed an object literal keeps / as division (`({a: 1}) / 2`).
func (s *jsScanner) record(tok jsToken) {
	prev := s.last
	s.last = &tok
	if tok.kind != jsOther {
		return
	}
	switch tok.text {
	case "(":
		header := prev != nil && prev.kind == jsOther && isHeaderKeyword(prev.text)
		s.parenKinds = append(s.parenKinds, header)
		s.afterHeaderParen = false
	case ")":
		if len(s.parenKinds) > 0 {
			s.afterHeaderParen = s.parenKinds[len(s.parenKinds)-1]
			s.parenKinds = s.parenKinds[:len(s.parenKinds)-1]
		}
	case "{":
		block := prev == nil || prev.kind != jsOther || isBlockIntroducer(prev.text)
		s.braceKinds = append(s.braceKinds, block)
		s.afterHeaderParen = false
	case "}":
		if len(s.braceKinds) > 0 {
			s.lastBraceWasBlock = s.braceKinds[len(s.braceKinds)-1]
			s.braceKinds = s.braceKinds[:len(s.braceKinds)-1]
		}
	default:
		s.afterHeaderParen = false
	}
}

// isHeaderKeyword reports whether an identifier opens a control-flow header
// whose closing parenthesis is followed by a statement — so a / right after
// that ) starts a regex. `function f()` is not a header: the / after it is
// division or a block follows, never a regex statement.
func isHeaderKeyword(text string) bool {
	switch text {
	case "if", "while", "for", "with", "switch":
		return true
	}
	return false
}

// isBlockIntroducer reports whether a token at statement position opens a
// statement block rather than an object literal: after a control-flow header
// or else/do/try/finally, a statement terminator (; or another }), a {, a :
// (a case clause), or a > (an arrow function body). Object literals appear
// after =, (, [, ,, return, and operators, which are not block introducers.
func isBlockIntroducer(text string) bool {
	switch text {
	case ")", "else", "do", "try", "finally", ";", "}", "{", ":", ">":
		return true
	}
	return false
}

// jsTokens breaks JavaScript source into a flat token stream. Comments never
// contribute tokens; string literals become jsString tokens holding their
// decoded contents; template literals and regular-expression literals become
// single jsTemplate/jsRegex tokens holding their raw source (their contents
// never contribute tokens, so a regex or template that quotes an export
// declaration cannot be mistaken for the real one); and everything else is an
// opaque jsOther token of its raw text. The parity checks match token
// sequences, so a documentation comment that quotes an export declaration can
// never be mistaken for the real one either.
func jsTokens(source string) ([]jsToken, error) {
	sc := &jsScanner{src: source}
	var tokens []jsToken
	for {
		tok, ok, err := sc.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return tokens, nil
		}
		tokens = append(tokens, tok)
	}
}

// next returns the next significant token, or ok=false at end of input.
// Whitespace and comments are consumed without producing tokens; template and
// regular-expression literals are consumed whole and returned as single tokens
// so their contents can never satisfy a token-sequence match.
func (s *jsScanner) next() (jsToken, bool, error) {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '"' || c == '\'':
			value, err := s.readString()
			if err != nil {
				return jsToken{}, false, err
			}
			tok := jsToken{kind: jsString, text: value}
			s.record(tok)
			return tok, true, nil
		case c == '`':
			start := s.pos
			if err := s.skipTemplate(); err != nil {
				return jsToken{}, false, err
			}
			tok := jsToken{kind: jsTemplate, text: s.src[start:s.pos]}
			s.record(tok)
			return tok, true, nil
		case c == '/' && s.pos+1 < len(s.src) && (s.src[s.pos+1] == '/' || s.src[s.pos+1] == '*'):
			if err := s.skipSpaceAndComments(); err != nil {
				return jsToken{}, false, err
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.pos++
		case c == '/' && s.regexAllowed():
			start := s.pos
			if err := s.skipRegex(); err != nil {
				return jsToken{}, false, err
			}
			tok := jsToken{kind: jsRegex, text: s.src[start:s.pos]}
			s.record(tok)
			return tok, true, nil
		default:
			start := s.pos
			if isJSIdentStart(c) {
				for s.pos < len(s.src) && isJSIdentPart(s.src[s.pos]) {
					s.pos++
				}
			} else if (c == '+' || c == '-') && s.pos+1 < len(s.src) && s.src[s.pos+1] == c {
				s.pos += 2 // ++ and -- are single tokens; a / right after them is division
			} else {
				s.pos++ // one punctuation rune per token
			}
			tok := jsToken{kind: jsOther, text: s.src[start:s.pos]}
			s.record(tok)
			return tok, true, nil
		}
	}
	return jsToken{}, false, nil
}

// skipSpaceAndComments advances past whitespace and // and /* */ comments.
func (s *jsScanner) skipSpaceAndComments() error {
	for s.pos < len(s.src) {
		switch {
		case s.src[s.pos] == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '/':
			if i := strings.IndexByte(s.src[s.pos:], '\n'); i >= 0 {
				s.pos += i + 1
			} else {
				s.pos = len(s.src)
			}
		case s.src[s.pos] == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '*':
			end := strings.Index(s.src[s.pos+2:], "*/")
			if end < 0 {
				return errors.New("unterminated block comment")
			}
			s.pos += 2 + end + 2
		case s.src[s.pos] == ' ' || s.src[s.pos] == '\t' || s.src[s.pos] == '\n' || s.src[s.pos] == '\r':
			s.pos++
		default:
			return nil
		}
	}
	return nil
}

// readString reads a single- or double-quoted string literal at the cursor and
// returns its contents with ECMAScript escape decoding. The module is strict
// (it uses import/export), so the strict-mode escape grammar applies: the
// single-character escapes, \0 (not followed by a digit), \xHH, \uHHHH,
// \u{...}, line continuations, and identity escapes; octal escapes (\1-\9, \0
// followed by a digit) are syntax errors in a module and therefore errors here
// too. Decoding must match the runtime exactly: a misdecoded escape (for
// example \n read as "n") would make the extractor report a different token
// than the runtime Set contains, so the parity check would compare against a
// vocabulary the runtime does not have.
func (s *jsScanner) readString() (string, error) {
	quote := s.src[s.pos]
	s.pos++
	var b strings.Builder
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == quote:
			s.pos++
			return b.String(), nil
		case c == '\n' || c == '\r':
			return "", errors.New("unterminated string literal")
		case c == '\\' && s.pos+1 < len(s.src):
			esc := s.src[s.pos+1]
			switch esc {
			case 'b':
				b.WriteByte('\b')
				s.pos += 2
			case 't':
				b.WriteByte('\t')
				s.pos += 2
			case 'n':
				b.WriteByte('\n')
				s.pos += 2
			case 'v':
				b.WriteByte('\v')
				s.pos += 2
			case 'f':
				b.WriteByte('\f')
				s.pos += 2
			case 'r':
				b.WriteByte('\r')
				s.pos += 2
			case '\n': // line continuation
				s.pos += 2
			case '\r': // line continuation, optionally \r\n
				s.pos += 2
				if s.pos < len(s.src) && s.src[s.pos] == '\n' {
					s.pos++
				}
			case '0':
				if s.pos+2 < len(s.src) && s.src[s.pos+2] >= '0' && s.src[s.pos+2] <= '9' {
					return "", errors.New("\\0 followed by a digit is an octal escape, a syntax error in a module")
				}
				b.WriteByte(0)
				s.pos += 2
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				return "", errors.New("octal escape is a syntax error in a module")
			case 'x':
				if s.pos+3 >= len(s.src) {
					return "", errors.New("unterminated \\x escape")
				}
				h, err := strconv.ParseUint(s.src[s.pos+2:s.pos+4], 16, 8)
				if err != nil {
					return "", errors.New("\\x escape needs exactly two hexadecimal digits")
				}
				b.WriteByte(byte(h))
				s.pos += 4
			case 'u':
				r, width, err := s.readUnicodeEscape()
				if err != nil {
					return "", err
				}
				b.WriteRune(r)
				s.pos += width
			default:
				// Identity escape (NonEscapeCharacter): \q is q, \" is ", \\ is \\.
				b.WriteByte(esc)
				s.pos += 2
			}
		default:
			b.WriteByte(c)
			s.pos++
		}
	}
	return "", errors.New("unterminated string literal")
}

// readUnicodeEscape reads the characters after a \u at the cursor and returns
// the rune and the total escape width (including the \u), or an error for a
// malformed \uHHHH or \u{...} escape.
func (s *jsScanner) readUnicodeEscape() (rune, int, error) {
	if s.pos+2 < len(s.src) && s.src[s.pos+2] == '{' {
		end := strings.IndexByte(s.src[s.pos+3:], '}')
		if end < 0 {
			return 0, 0, errors.New("unterminated \\u{...} escape")
		}
		hex := s.src[s.pos+3 : s.pos+3+end]
		if len(hex) == 0 || len(hex) > 6 {
			return 0, 0, errors.New("\\u{...} needs one to six hexadecimal digits")
		}
		cp, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			return 0, 0, errors.New("\\u{...} needs hexadecimal digits")
		}
		if cp > 0x10FFFF {
			return 0, 0, errors.New("\\u{...} code point is out of range")
		}
		return rune(cp), 3 + end + 1, nil
	}
	if s.pos+5 >= len(s.src) {
		return 0, 0, errors.New("unterminated \\u escape")
	}
	cp, err := strconv.ParseUint(s.src[s.pos+2:s.pos+6], 16, 32)
	if err != nil {
		return 0, 0, errors.New("\\u escape needs exactly four hexadecimal digits")
	}
	return rune(cp), 6, nil
}

// skipTemplate advances past a template literal (backtick string). Template
// literals never contribute Set members, so their contents — including any
// embedded quotes — are skipped wholesale; ${...} interpolations are consumed
// by skipTemplateExpression, which understands nested templates, strings,
// comments, and regular expressions, so a backtick inside a nested template
// cannot close the outer literal early.
func (s *jsScanner) skipTemplate() error {
	s.pos++ // opening backtick
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '\\' && s.pos+1 < len(s.src):
			s.pos += 2
		case c == '`':
			s.pos++
			return nil
		case c == '$' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '{':
			s.pos += 2
			if err := s.skipTemplateExpression(); err != nil {
				return err
			}
		default:
			s.pos++
		}
	}
	return errors.New("unterminated template literal")
}

// skipTemplateExpression consumes a ${...} interpolation. Tokens inside the
// expression are lexed exactly as in the surrounding source (strings,
// comments, regular expressions, and nested templates are single units), and
// the braces that open and close the interpolation are counted, so the scan
// ends at the interpolation's own closing brace.
func (s *jsScanner) skipTemplateExpression() error {
	// The interpolation's own braces and any tokens inside it belong to the
	// template literal, not to the surrounding source: the paren/brace stacks
	// and the regex-context flags must not leak across the interpolation, or a
	// later ) or } would be misclassified. The tokens inside are still lexed
	// with the full scanner (strings, comments, regexes, and nested templates
	// are single units), and the braces that open and close the interpolation
	// are counted, so the scan ends at the interpolation's own closing brace.
	parens := s.parenKinds
	braces := s.braceKinds
	header := s.afterHeaderParen
	block := s.lastBraceWasBlock
	last := s.last
	defer func() {
		s.parenKinds = parens
		s.braceKinds = braces
		s.afterHeaderParen = header
		s.lastBraceWasBlock = block
		s.last = last
	}()
	depth := 1
	for depth > 0 {
		tok, ok, err := s.next()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("unterminated template literal")
		}
		if tok.kind != jsOther {
			continue
		}
		switch tok.text {
		case "{":
			depth++
		case "}":
			depth--
		}
	}
	return nil
}

// skipRegex advances past a regular-expression literal: the body ends at the
// first unescaped / outside a character class, and the flags (identifier
// characters) are consumed with it. A regex literal is one token — its
// contents never contribute tokens, so a regex that quotes an export
// declaration cannot be mistaken for the real one.
func (s *jsScanner) skipRegex() error {
	s.pos++ // opening /
	inClass := false
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '\\' && s.pos+1 < len(s.src):
			s.pos += 2 // an escaped character, including \/ and \]
		case c == '[':
			inClass = true
			s.pos++
		case c == ']':
			inClass = false
			s.pos++
		case c == '/' && !inClass:
			s.pos++
			for s.pos < len(s.src) && isJSIdentPart(s.src[s.pos]) {
				s.pos++ // flags
			}
			return nil
		case c == '\n' || c == '\r':
			return errors.New("unterminated regular expression literal")
		default:
			s.pos++
		}
	}
	return errors.New("unterminated regular expression literal")
}

// regexAllowed reports whether a / at the cursor starts a regular-expression
// literal rather than division, following the ECMAScript lexical rule: / is
// division after a complete expression (an identifier, a numeric or string
// literal, a template or regex literal, or ), ], }, ++, --) and starts a regex
// after operators, punctuation, and the keywords that introduce an expression
// (return, typeof, new, ...). The distinction matters because a misread regex
// would leak its contents into the token stream, and a regex that quotes an
// export declaration could then satisfy the parity anchor.
func (s *jsScanner) regexAllowed() bool {
	if s.last == nil {
		return true
	}
	switch s.last.kind {
	case jsString, jsTemplate, jsRegex:
		return false
	}
	text := s.last.text
	if text == "" {
		return true
	}
	if isJSIdentStart(text[0]) {
		switch text {
		case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "case", "do", "else", "yield", "await", "throw", "extends":
			return true
		}
		return false // identifiers and value keywords (this, null, true, false)
	}
	if text[0] >= '0' && text[0] <= '9' {
		return false // a numeric literal
	}
	switch text {
	case ")":
		// A / after the ) of a control-flow header starts a regex statement
		// (`if (false) /regex/`); after any other ) — a call, a grouped
		// expression — it is division (`f() / 2`, `(a + b) / 2`).
		return s.afterHeaderParen
	case "}":
		// A / after the } of a statement block starts a regex statement
		// (`if (false) {} /regex/`); after an object literal it is division.
		return s.lastBraceWasBlock
	case "]", "++", "--":
		return false // an array literal or a postfix expression is complete
	}
	return true
}

func isJSIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isJSIdentPart(c byte) bool {
	return isJSIdentStart(c) || c >= '0' && c <= '9'
}

// tokenSequenceMatches reports whether tokens begins with the given raw texts
// in order. Only jsOther tokens match — a string, template, or regex token
// never impersonates an identifier or punctuation, so quoted or regex-embedded
// text cannot satisfy an anchor sequence.
func tokenSequenceMatches(tokens []jsToken, seq []string) bool {
	for i, want := range seq {
		if tokens[i].kind != jsOther || tokens[i].text != want {
			return false
		}
	}
	return true
}

// jsExportedSetMembers finds `export const <anchor> = new Set([...])` and
// returns the contents of the string literals that are the array's elements.
// The tokenizer never emits tokens from comments, template literals, or
// regular expressions, so a quoted lifecycle token in a comment or a regex
// cannot satisfy parity. The Set argument must be exactly a plain array of
// string literals: every element must be exactly a string-literal array
// element (a member expression like "paused".toUpperCase() or "a" + "b"
// would not put the literal's own text in the runtime Set), and non-string
// elements — a spread (...["unsupported"]), an elision, a nested array, a
// call, a number, a template, a regex — are loud errors, because each would
// put a value in the runtime Set that no string literal represents. A
// trailing expression — `new Set([...].filter(...))` or
// `new Set([...]).add(...)` — would change the runtime vocabulary, so the
// literal members alone would be a false positive and the extractor rejects
// those forms loudly instead of checking a vocabulary the runtime does not
// have. The same applies to a *later* mutation of the exported Set
// (`LIFECYCLE_UNFINISHED.delete("paused")` after the initializer): the
// runtime vocabulary would no longer be the literal members, so the
// extractor rejects the mutation rather than checking an allowlist the
// runtime does not have.
func jsExportedSetMembers(source, anchor string) ([]string, error) {
	tokens, err := jsTokens(source)
	if err != nil {
		return nil, err
	}
	seq := []string{"export", "const", anchor, "=", "new", "Set", "(", "["}
	start := -1
	for i := 0; i+len(seq) <= len(tokens); i++ {
		if tokenSequenceMatches(tokens[i:i+len(seq)], seq) {
			start = i + len(seq)
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("assets/task-model.js is missing `export const %s = new Set([...])`; the verdict vocabulary has no anchor to check", anchor)
	}
	// The Set argument must be exactly a plain array whose elements are all
	// string literals: after the opening bracket or a comma the only legal
	// token is a string literal, and after a member the only legal tokens are
	// a comma or the closing bracket. Anything else — a spread
	// (...["unsupported"]), a member expression ("paused".toUpperCase()), an
	// elision ([, "x"]), a nested array, a call, a number, a template, a
	// regex — would make the runtime Set differ from the literals read here
	// (a spread adds its elements, an elision adds undefined), so it is a loud
	// error instead of a silently wrong extraction.
	expectMember := true // true after "[" or ","
	var members []string
	for i := start; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind == jsString {
			if !expectMember {
				return nil, fmt.Errorf("unsupported Set member %q in `export const %s = new Set(...)`: expected a comma between elements", tok.text, anchor)
			}
			members = append(members, tok.text)
			expectMember = false
			continue
		}
		switch tok.text {
		case ",":
			if expectMember {
				return nil, fmt.Errorf("unsupported Set element in `export const %s = new Set(...)`: an empty (elided) element would put undefined in the runtime Set, which no string literal can represent", anchor)
			}
			expectMember = true
		case "]":
			// The array literal closed. The `]` must be followed directly by
			// the `)` of `new Set(...)` and then the end of the statement; any
			// other token means the runtime value is not the literal we just
			// read (a .filter()/ .map() / .add() call, a concatenation, ...)
			// and parity would be checking a vocabulary the runtime does not
			// have.
			if i+1 >= len(tokens) || tokens[i+1].text != ")" {
				after := "end of file"
				if i+1 < len(tokens) {
					after = fmt.Sprintf("%q", tokens[i+1].text)
				}
				return nil, fmt.Errorf("unsupported form for `export const %s = new Set(...)`: the argument must be a plain array literal, but %s follows the closing `]`; a trailing expression (e.g. .filter(...)) would change the runtime vocabulary, so it cannot be checked", anchor, after)
			}
			if i+2 < len(tokens) && tokens[i+2].text != ";" {
				return nil, fmt.Errorf("unsupported form for `export const %s = new Set(...)`: expected `;` after the Set expression, found %q; a member call (e.g. .add(...)) would change the runtime vocabulary, so it cannot be checked", anchor, tokens[i+2].text)
			}
			if err := rejectLaterSetMutation(tokens, i+3, anchor); err != nil {
				return nil, err
			}
			return members, nil
		default:
			return nil, fmt.Errorf("unsupported Set element %q in `export const %s = new Set(...)`: a member must be exactly a string-literal array element; an expression (e.g. a spread or a call) would change the runtime vocabulary, so it cannot be checked", tok.text, anchor)
		}
	}
	return nil, fmt.Errorf("unterminated Set array in `export const %s = new Set([...])`", anchor)
}

// rejectLaterSetMutation scans the tokens after the export statement for a
// mutation of the exported Set: a member call on the anchor
// (`LIFECYCLE_UNFINISHED.delete("paused")`, `.add(...)`, `.clear()` — dot or
// bracket form). A mutation after the initializer would leave the runtime Set
// with a different vocabulary than the literal members the extractor reads,
// so parity would pass for an allowlist the runtime does not have. Reads
// (`.has(...)`) and other references are allowed; the anchor is a module-level
// const binding, so any reference to it in the module is the exported Set
// itself.
func rejectLaterSetMutation(tokens []jsToken, after int, anchor string) error {
	for i := after; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind != jsOther || tok.text != anchor {
			continue
		}
		if i+1 >= len(tokens) {
			continue
		}
		switch {
		case tokens[i+1].kind == jsOther && tokens[i+1].text == ".":
			if i+2 < len(tokens) && tokens[i+2].kind == jsOther && isSetMutationMethod(tokens[i+2].text) {
				return fmt.Errorf("export const %s is mutated after the initializer (%s.%s(...)); the runtime vocabulary would differ from the literal members, so it cannot be checked", anchor, anchor, tokens[i+2].text)
			}
		case tokens[i+1].kind == jsOther && tokens[i+1].text == "[":
			if i+3 < len(tokens) && tokens[i+2].kind == jsString && isSetMutationMethod(tokens[i+2].text) && tokens[i+3].kind == jsOther && tokens[i+3].text == "]" {
				return fmt.Errorf("export const %s is mutated after the initializer (%s[%q](...)); the runtime vocabulary would differ from the literal members, so it cannot be checked", anchor, anchor, tokens[i+2].text)
			}
		}
	}
	return nil
}

// isSetMutationMethod reports whether a method name mutates the Set it is
// called on. delete, add, and clear change the runtime vocabulary; has and
// the iteration/read methods do not.
func isSetMutationMethod(name string) bool {
	switch name {
	case "delete", "add", "clear":
		return true
	}
	return false
}

// jsExportedString finds `export const <anchor> = "..."` and returns the value
// of the string literal after the equals sign. The value must be exactly that
// literal: a trailing expression (`"..." + x`, `"...".toUpperCase()`) would
// change the runtime value, so the extractor rejects those forms loudly
// instead of checking a value the runtime does not have.
func jsExportedString(source, anchor string) (string, error) {
	tokens, err := jsTokens(source)
	if err != nil {
		return "", err
	}
	seq := []string{"export", "const", anchor, "="}
	for i := 0; i+len(seq) < len(tokens); i++ {
		if !tokenSequenceMatches(tokens[i:i+len(seq)], seq) {
			continue
		}
		next := tokens[i+len(seq)]
		if next.kind != jsString {
			return "", fmt.Errorf("export const %s is not assigned a string literal", anchor)
		}
		if i+len(seq)+1 < len(tokens) && tokens[i+len(seq)+1].text != ";" {
			return "", fmt.Errorf("unsupported form for `export const %s`: the value must be exactly a string literal, but %q follows it; a trailing expression (e.g. \"...\" + x or .toUpperCase()) would change the runtime value, so it cannot be checked", anchor, tokens[i+len(seq)+1].text)
		}
		return next.text, nil
	}
	return "", fmt.Errorf("assets/task-model.js is missing `export const %s = \"...\"`; the done marker has no anchor to check", anchor)
}

func readTaskModelJS(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("assets/task-model.js")
	if err != nil {
		t.Fatalf("read assets/task-model.js: %v", err)
	}
	return string(source)
}

func TestTaskRelationsLifecycleParity(t *testing.T) {
	source := readTaskModelJS(t)

	unfinished, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatal(err)
	}
	done, err := jsExportedString(source, "LIFECYCLE_DONE")
	if err != nil {
		t.Fatal(err)
	}

	// Every non-done server lifecycle state must confirm a blocker. The loop
	// iterates coordinator.AllLifecycleStates — the server's enumerated
	// vocabulary, kept exhaustive by TestAllLifecycleStatesExhaustive — rather
	// than a hardcoded list, so a newly added LifecycleState constant surfaces
	// here as a missing client entry and fails the build until the
	// task-relations verdict covers it.
	for _, state := range coordinator.AllLifecycleStates {
		if state == coordinator.LifecycleDone {
			continue // done is the single finished state; it must not confirm a blocker.
		}
		if !containsString(unfinished, string(state)) {
			t.Errorf("client LIFECYCLE_UNFINISHED is missing server state %q; the task-relations verdict would render it unknown instead of blocking", state)
		}
	}

	// The done state must be the client's single finished marker.
	if done != string(coordinator.LifecycleDone) {
		t.Errorf("client LIFECYCLE_DONE = %q, want server state %q", done, coordinator.LifecycleDone)
	}

	// And the client vocabulary must not invent states: every unfinished token
	// is either the unscheduled wire encoding or a state the server knows.
	serverStates := make(map[coordinator.LifecycleState]bool, len(coordinator.AllLifecycleStates))
	for _, state := range coordinator.AllLifecycleStates {
		serverStates[state] = true
	}
	for _, state := range unfinished {
		if state == "" {
			continue // the wire encoding of a valid unscheduled task.
		}
		if !serverStates[coordinator.LifecycleState(state)] {
			t.Errorf("client LIFECYCLE_UNFINISHED contains %q, which is not a server LifecycleState; the verdict would confirm a blocker the server cannot emit", state)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// The JS extraction is regression-tested against the cases that defeated the
// previous regex scan: a quoted lifecycle token in a comment must not count as
// a Set member, a comment that quotes the whole export must not be mistaken
// for the declaration, a regular-expression literal that quotes the export
// must not be mistaken for it either (regex contents never become tokens), and
// strings containing comment markers, escaped quotes, or escape sequences must
// still be read exactly as JavaScript evaluates them. Trailing expressions
// that would change the runtime vocabulary or value (`new Set([...].filter(...))`,
// `"..." + x`) and Set elements that are not exactly string-literal array
// elements (`"paused".toUpperCase()`, a spread, an elision, a call, a number,
// a template) must be loud errors — never a silently wrong extraction.

func TestJSCommentCannotSatisfyParity(t *testing.T) {
	// Commenting a state out of the Set but leaving a quoted mention in the
	// comment must fail the check, never silently keep the state covered.
	source := `export const LIFECYCLE_UNFINISHED = new Set([
	  "", "scheduled",
	  // "in_progress" is temporarily disabled for the rollout
	  /* "done" is the finished state, never unfinished */
	]);`
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q (quoted comment tokens must not be members)", members, want)
	}
}

func TestJSCommentedExportIsNotTheAnchor(t *testing.T) {
	// A documentation comment quoting the export cannot be the anchor; the
	// real declaration must still be found.
	source := `// Example: export const LIFECYCLE_UNFINISHED = new Set(["scheduled"]);
export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);`
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q", members, want)
	}

	// With only the commented-out export there is no anchor at all.
	onlyComment := `// export const LIFECYCLE_UNFINISHED = new Set(["scheduled"]);`
	if _, err := jsExportedSetMembers(onlyComment, "LIFECYCLE_UNFINISHED"); err == nil {
		t.Error("jsExportedSetMembers accepted a commented-out export as the anchor")
	}
}

func TestJSStringLiteralsAreReadExactly(t *testing.T) {
	source := "export const LIFECYCLE_UNFINISHED = new Set([\n" +
		`  "with//slash",   // a string containing comment markers is still a member` + "\n" +
		`  "esc\"aped",` + "\n" +
		`  'single\'quoted',` + "\n" +
		"]);"
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	want := []string{`with//slash`, `esc"aped`, `single'quoted`}
	if !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q", members, want)
	}

	// A call or a template as an element is not a plain string literal: the
	// runtime Set contains a value no listed literal represents, so the element
	// must be a loud error, never silently ignored.
	for _, bad := range []string{
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", someFn("nested-call-arg")]);`,
		"export const LIFECYCLE_UNFINISHED = new Set([\"\", \"scheduled\", `ignored \"template\"`]);",
	} {
		if _, err := jsExportedSetMembers(bad, "LIFECYCLE_UNFINISHED"); err == nil {
			t.Errorf("jsExportedSetMembers accepted %s; a call/template element is not a plain string literal", bad)
		}
	}

	src2 := `export const LIFECYCLE_DONE = "done"; // "not this"`
	done, err := jsExportedString(src2, "LIFECYCLE_DONE")
	if err != nil {
		t.Fatalf("jsExportedString: %v", err)
	}
	if done != "done" {
		t.Errorf("jsExportedString = %q, want %q", done, "done")
	}
}

func TestJSSetTrailingExpressionIsRejected(t *testing.T) {
	// A trailing expression changes the runtime vocabulary, so the literal
	// members alone would be a false positive: `new Set([..., "paused"].filter(...))`
	// excludes the listed token at runtime, and `new Set([...]).add(...)` adds
	// one the literal never lists. Both must fail loudly.
	filtered := `export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"].filter(state => state !== "in_progress"));`
	if _, err := jsExportedSetMembers(filtered, "LIFECYCLE_UNFINISHED"); err == nil {
		t.Error("jsExportedSetMembers accepted `new Set([...].filter(...))`; the runtime vocabulary excludes the filtered token, so the literal members are a false positive")
	}
	memberCall := `export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled"]).add("in_progress");`
	if _, err := jsExportedSetMembers(memberCall, "LIFECYCLE_UNFINISHED"); err == nil {
		t.Error("jsExportedSetMembers accepted `new Set([...]).add(...)`; the runtime vocabulary has members the literal never lists")
	}
}

func TestJSStringTrailingExpressionIsRejected(t *testing.T) {
	// A trailing expression changes the runtime value: `"done" + "x"` and
	// `"done".toUpperCase()` are not the literal the extractor would read.
	for _, source := range []string{
		`export const LIFECYCLE_DONE = "done" + "x";`,
		`export const LIFECYCLE_DONE = "done".toUpperCase();`,
	} {
		if _, err := jsExportedString(source, "LIFECYCLE_DONE"); err == nil {
			t.Errorf("jsExportedString accepted %s; the runtime value is not the literal", source)
		}
	}
}

func TestJSSetMemberMustBeExactlyALiteral(t *testing.T) {
	// A member expression does not put the literal's own text in the runtime
	// Set: "paused".toUpperCase() yields "PAUSED", "paused" + "_x" yields
	// "paused_x", and a spread of a string yields its characters. The literal
	// alone would be a false positive for parity, so each form must fail loudly.
	for _, source := range []string{
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused".toUpperCase()]);`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused" + "_x"]);`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", ..."paused"]);`,
	} {
		if _, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED"); err == nil {
			t.Errorf("jsExportedSetMembers accepted %s; the runtime Set does not contain the literal's own text", source)
		}
	}

	// A plain string element — even with a trailing comma or comments — is the
	// only accepted member form.
	ok := `export const LIFECYCLE_UNFINISHED = new Set([
		"", // the unscheduled wire encoding
		"scheduled",
		"in_progress",
	]);`
	members, err := jsExportedSetMembers(ok, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q", members, want)
	}
}

func TestJSRegexCannotSatisfyParity(t *testing.T) {
	// A regular-expression literal earlier in the file that quotes the export
	// must not be mistaken for the declaration: the runtime Set omits "paused",
	// so if the extractor reported the regex's contents, parity would pass for a
	// vocabulary the runtime does not have. Regex literals are consumed whole
	// and never contribute tokens.
	source := `const lifecyclePattern = /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/;
export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);`
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q (regex contents must not be members)", members, want)
	}

	// Without a real declaration, a regex quoting the export is not an anchor.
	onlyRegex := `const r = /export const LIFECYCLE_UNFINISHED = new Set(["paused"]);/;`
	if _, err := jsExportedSetMembers(onlyRegex, "LIFECYCLE_UNFINISHED"); err == nil {
		t.Error("jsExportedSetMembers accepted a regex literal as the export anchor")
	}
}

func TestJSRegexAfterControlFlowHeader(t *testing.T) {
	// A regex statement in a dead branch — `if (false) /regex/` and
	// `if (false) {} /regex/` — is valid ECMAScript: a / right after the
	// header's ) or a block's } starts a regex, not division. If the scanner
	// read the / as division, the regex's contents would leak into the token
	// stream and a fake export quoted inside the dead regex would be selected
	// ahead of the real declaration (the runtime Set omits "paused", so the
	// parity check would then pass for a vocabulary the runtime does not have).
	for _, dead := range []string{
		`if (false) /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/.test("x");`,
		`if (false) {} /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/.test("x");`,
		`if (false) { if (false) {} /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/.test("x"); }`,
		`while (false) /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/.test("x");`,
		`for (;;) /export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "paused"]);/.test("x");`,
	} {
		source := dead + "\nexport const LIFECYCLE_UNFINISHED = new Set([\"\", \"scheduled\", \"in_progress\"]);"
		members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
		if err != nil {
			t.Fatalf("jsExportedSetMembers: %v", err)
		}
		if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
			t.Errorf("jsExportedSetMembers = %q, want %q (a dead regex must not contribute tokens)", members, want)
		}
	}
}

func TestJSTemplateInterpolationRegex(t *testing.T) {
	// A regex inside a template interpolation — including a second interpolation
	// that starts with the regex — must lex as a regex (the ${ starts an
	// expression), so its contents never contribute tokens and a fake export
	// quoted inside it cannot be mistaken for the real declaration.
	source := "const t = `x${/export const LIFECYCLE_UNFINISHED = new Set([\"\", \"scheduled\", \"paused\"]);/.test(\"x\")}${/export const LIFECYCLE_DONE = \"done\"/}`;\n" +
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);`
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q (regexes inside template interpolations must not contribute tokens)", members, want)
	}
}

func TestJSDivisionIsNotARegex(t *testing.T) {
	// Division must still lex as punctuation: after a numeric literal a / is
	// division, so `4 / 2 / 1` must not be read as an unterminated regex or
	// swallow the real export that follows. The same holds after a call's ) —
	// `f() / 2` is division, not a regex statement — and after an object
	// literal's }.
	source := `const ratio = 4 / 2 / 1;
function f() { return 4; }
const half = f() / 2;
const quarter = ({ value: 4 }) / 4;
export const LIFECYCLE_DONE = "done";`
	done, err := jsExportedString(source, "LIFECYCLE_DONE")
	if err != nil {
		t.Fatalf("jsExportedString: %v", err)
	}
	if done != "done" {
		t.Errorf("jsExportedString = %q, want %q", done, "done")
	}
}

func TestJSSetRejectsLaterMutation(t *testing.T) {
	// A mutation of the exported Set after the initializer — the reviewer's
	// `.delete("paused")`, plus .add/.clear and bracket-form calls — leaves the
	// runtime vocabulary different from the literal members the extractor reads.
	// Parity must fail loudly instead of checking an allowlist the runtime does
	// not have. A read (.has) stays fine.
	mutations := []string{
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);
LIFECYCLE_UNFINISHED.delete("paused");`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);
LIFECYCLE_UNFINISHED.add("reviewing");`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);
LIFECYCLE_UNFINISHED.clear();`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);
LIFECYCLE_UNFINISHED["delete"]("in_progress");`,
	}
	for _, source := range mutations {
		if _, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED"); err == nil {
			t.Errorf("jsExportedSetMembers accepted %s; a later mutation changes the runtime vocabulary", source)
		}
	}

	// A read of the exported Set — the real file's `.has(state)` — and uses of
	// the same token in other contexts must not be rejected.
	ok := `export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);
export function blockerVerdict(relation) {
  const state = relation == null ? undefined : relation["source_state"] ?? relation["SourceState"];
  if (state === "done") return false;
  if (typeof state !== "string") return null;
  if (LIFECYCLE_UNFINISHED.has(state)) return true;
  return null;
}
export const LIFECYCLE_UNFINISHED_COPY = "not the set";`
	members, err := jsExportedSetMembers(ok, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	if want := []string{"", "scheduled", "in_progress"}; !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q", members, want)
	}
}

func TestJSStringEscapesDecodeExactly(t *testing.T) {
	// The extractor must report the same token JavaScript evaluates: \u0075 is
	// u, \n is a newline (not the letter n), \x64 is d, \\ is a backslash, and
	// a backslash-newline line continuation joins the lines. A misdecoded escape
	// would make the parity check compare against a vocabulary the runtime does
	// not have.
	source := "export const LIFECYCLE_UNFINISHED = new Set([\n" +
		"  \"sched\\u0075led\",\n" +
		"  \"in_progress\\n\",\n" +
		"  \"\\x64one\",\n" +
		"  \"line\\\\continuation\",\n" +
		"  \"joined\\\nnext\",\n" +
		"]);"
	members, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED")
	if err != nil {
		t.Fatalf("jsExportedSetMembers: %v", err)
	}
	want := []string{"scheduled", "in_progress\n", "done", "line\\continuation", "joinednext"}
	if !slicesEqual(members, want) {
		t.Errorf("jsExportedSetMembers = %q, want %q (escapes must decode exactly as JavaScript evaluates them)", members, want)
	}
}

func TestJSStringRejectsInvalidEscapes(t *testing.T) {
	// Octal escapes and malformed \x/\u escapes are syntax errors in a module,
	// so the extractor must reject them rather than decode them to something
	// the runtime cannot evaluate.
	for _, source := range []string{
		`export const LIFECYCLE_DONE = "\1";`,
		`export const LIFECYCLE_DONE = "\08";`,
		`export const LIFECYCLE_DONE = "\x4";`,
		`export const LIFECYCLE_DONE = "\u12";`,
	} {
		if _, err := jsExportedString(source, "LIFECYCLE_DONE"); err == nil {
			t.Errorf("jsExportedString accepted %s; the escape is a syntax error in a module", source)
		}
	}
}

func TestJSSetRejectsNonLiteralElements(t *testing.T) {
	// The exact-member rule must be fail-closed: a spread of an array literal
	// (...["unsupported"]) adds "unsupported" to the runtime Set while the
	// extractor would otherwise report nothing for it, an elision puts undefined
	// in the Set, and a number or a call is not a string literal at all. Each
	// must be a loud error, never a silently ignored element.
	for _, source := range []string{
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", ...["unsupported"]]);`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", , "in_progress"]);`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", 42]);`,
		`export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", someFn("x")]);`,
	} {
		if _, err := jsExportedSetMembers(source, "LIFECYCLE_UNFINISHED"); err == nil {
			t.Errorf("jsExportedSetMembers accepted %s; the runtime Set contains a value no string literal represents", source)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
