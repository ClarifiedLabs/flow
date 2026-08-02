package coordinator

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The task-relations client keeps its own copy of the lifecycle vocabulary
// (LIFECYCLE_UNFINISHED in internal/web/assets/task-model.js), and the web
// parity test iterates AllLifecycleStates to prove the client covers every
// server state. These tests are the first link in that chain: they parse every
// non-test Go file in this package with go/parser and fail unless
// AllLifecycleStates is exactly the set of declared LifecycleState constants.
//
// The extraction is syntax-aware — a real parser rather than a line regex —
// and type-aware: every package-level constant is classified the way Go's type
// inference would, and constant expressions are evaluated with go/constant. A
// valid declaration like `const LifecyclePaused = LifecycleScheduled +
// "_paused"` has inferred type LifecycleState (the binary expression's first
// operand is a LifecycleState constant) and is therefore found, exactly as the
// compiler would type it — as are trailing comments, `LifecycleState("...")`
// conversions, grouped blocks, multi-name specs, redundant parentheses, type
// aliases (`type lifecycleAlias = LifecycleState`, an exact synonym, used as a
// constant's type or conversion), *generic* aliases instantiated with type
// arguments (`type lifecycleAlias[T any] = LifecycleState; const X
// lifecycleAlias[int] = "v"`, whose type parses to an IndexExpr), a constant
// moved to another file in the
// package, and Go's implicit repetition of the previous spec in a const group.
// A LifecycleState constant whose value
// cannot be read — an iota-based value, an unresolved or imported reference,
// an unsupported expression — is a hard error rather than a silent skip, so no
// future declaration form can evade the exhaustive check either.
//
// A new constant that is not added to AllLifecycleStates fails here; once it
// is added, the web parity test fails until the client allowlist catches up.
// Without this chain, a developer could add a LifecycleState constant and
// leave both lists untouched, and nothing would notice the new state silently
// rendering as neutral unknown.

// declaredLifecycleStates parses one Go source file and returns the value of
// every LifecycleState constant it declares, in declaration order. It is a
// convenience wrapper over the package-wide extractor used by the exhaustive
// test; see declaredLifecycleStatesInFiles.
func declaredLifecycleStates(source string) ([]string, error) {
	return declaredLifecycleStatesInFiles(map[string]string{"source.go": source})
}

// declaredLifecycleStatesInFiles parses Go sources (keyed by filename) and
// returns the value of every LifecycleState constant the package declares, in
// declaration order. The whole package is scanned as one table because
// constants may reference each other across files.
func declaredLifecycleStatesInFiles(files map[string]string) ([]string, error) {
	sc, err := newLifecycleScanner(files)
	if err != nil {
		return nil, err
	}
	return sc.collect()
}

// constValue is the result of evaluating one constant expression: its value
// and whether the expression has type LifecycleState (by Go's inference: an
// explicit LifecycleState(...) conversion, a reference to a LifecycleState
// typed constant, or an arithmetic binary expression over one).
type constValue struct {
	val constant.Value
	lc  bool
}

// specRef identifies which declaration supplies a constant's value: the spec
// (with inherited type and values for a spec that repeats the previous one)
// and the name's index within it.
type specRef struct {
	file string
	spec *ast.ValueSpec
	idx  int
}

// typeAlias is one package-level type alias declaration: the aliased type
// expression and, for a generic alias (`type lifecycleAlias[T any] =
// LifecycleState`), the type parameter names in declaration order. The
// parameters are needed to resolve an *instantiated* alias used as a
// constant's type or conversion (`const X lifecycleAlias[int] = "v"`): the
// type arguments are substituted for the parameters in the aliased expression
// before it is resolved.
type typeAlias struct {
	params []string
	rhs    ast.Expr
}

// lifecycleScanner extracts LifecycleState constant values from a package.
type lifecycleScanner struct {
	refs      map[string]specRef
	aliases   map[string]typeAlias // type alias name -> aliased type
	order     []string             // constant names in declaration order
	values    map[string]constValue
	resolving map[string]bool // cycle detection during evaluation
}

func newLifecycleScanner(files map[string]string) (*lifecycleScanner, error) {
	sc := &lifecycleScanner{
		refs:      map[string]specRef{},
		aliases:   map[string]typeAlias{},
		values:    map[string]constValue{},
		resolving: map[string]bool{},
	}
	fset := token.NewFileSet()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, files[name], 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			if gen.Tok == token.TYPE {
				// A type alias (`type X = Y`) is an exact synonym: a constant
				// declared with the alias as its type — or converted through it —
				// is a LifecycleState constant when the alias resolves to
				// LifecycleState. A *defined* type (`type X LifecycleState`) is a
				// distinct type and is deliberately not an alias. Generic aliases
				// (`type X[T any] = Y`) are stored with their type parameters so
				// an instantiation (`X[int]`) can be resolved by substituting the
				// arguments into the aliased expression.
				for _, specNode := range gen.Specs {
					spec := specNode.(*ast.TypeSpec)
					if !spec.Assign.IsValid() {
						continue
					}
					sc.aliases[spec.Name.Name] = typeAlias{
						params: typeParamNames(spec.TypeParams),
						rhs:    spec.Type,
					}
				}
				continue
			}
			if gen.Tok != token.CONST {
				continue
			}
			var prev *ast.ValueSpec // last spec with values, for implicit repetition
			for _, specNode := range gen.Specs {
				spec := specNode.(*ast.ValueSpec)
				effective := spec
				if len(spec.Values) == 0 {
					// A spec without values repeats the previous spec's type and
					// expression list (Go's implicit repetition in const groups);
					// the repeated constant is just as much a LifecycleState
					// constant as the one it repeats.
					if prev == nil {
						continue // invalid Go; nothing to extract
					}
					effective = prev
				} else {
					prev = spec
				}
				for i, ident := range spec.Names {
					if _, dup := sc.refs[ident.Name]; dup {
						continue // invalid Go; keep the first declaration
					}
					sc.refs[ident.Name] = specRef{file: name, spec: effective, idx: i}
					sc.order = append(sc.order, ident.Name)
				}
			}
		}
	}
	return sc, nil
}

// collect returns the lifecycle state values in declaration order, failing
// loudly for any LifecycleState constant whose value cannot be read.
func (s *lifecycleScanner) collect() ([]string, error) {
	var states []string
	var errs []error
	for _, name := range s.order {
		ref := s.refs[name]
		value, err := s.evalName(name)
		if err != nil {
			// A constant with an explicit LifecycleState type, or one whose
			// expression is derived from the lifecycle vocabulary, must be a
			// loud error: skipping it would let a real server state evade the
			// exhaustive check. Anything else is an unrelated constant.
			if s.isLifecycleStateType(ref.spec.Type) || s.lifecycleDerived(ref.spec, ref.idx, map[string]bool{}) {
				errs = append(errs, fmt.Errorf("LifecycleState constant %s (%s): %w", name, ref.file, err))
			}
			continue
		}
		if !s.isLifecycleStateType(ref.spec.Type) && !value.lc {
			continue // a plain constant of some other type, not a lifecycle state
		}
		if value.val.Kind() != constant.String {
			errs = append(errs, fmt.Errorf("LifecycleState constant %s (%s) has a %s value, not a string; a lifecycle state must be a string constant", name, ref.file, value.val.Kind()))
			continue
		}
		states = append(states, constant.StringVal(value.val))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return states, nil
}

// evalName evaluates one package constant by name, memoizing the result. The
// lc flag of a constant with an explicit LifecycleState type is forced on, so
// a reference to it from another expression propagates the lifecycle type.
func (s *lifecycleScanner) evalName(name string) (constValue, error) {
	if v, ok := s.values[name]; ok {
		return v, nil
	}
	ref, ok := s.refs[name]
	if !ok {
		if name == "iota" {
			return constValue{}, fmt.Errorf("iota-based value is opaque to the lifecycle extractor; declare the state as a string")
		}
		return constValue{}, fmt.Errorf("identifier %q is not a package constant (imported or undefined); declare the state as a string", name)
	}
	if s.resolving[name] {
		return constValue{}, fmt.Errorf("cyclic constant reference")
	}
	s.resolving[name] = true
	defer delete(s.resolving, name)
	v, err := s.evalSpecValue(ref.spec, ref.idx)
	if err != nil {
		return constValue{}, fmt.Errorf("%s: %w", ref.file, err)
	}
	if s.isLifecycleStateType(ref.spec.Type) {
		v.lc = true
	}
	s.values[name] = v
	return v, nil
}

// evalSpecValue evaluates the value expression that supplies name idx of spec,
// applying Go's rule that a single expression supplies every name.
func (s *lifecycleScanner) evalSpecValue(spec *ast.ValueSpec, idx int) (constValue, error) {
	expr := specValueExpr(spec, idx)
	if expr == nil {
		return constValue{}, fmt.Errorf("%d values for %d names is not supported; give each constant a literal", len(spec.Values), len(spec.Names))
	}
	return s.evalExpr(expr)
}

// specValueExpr returns the value expression for name idx of spec, or nil when
// the spec's expression list cannot be mapped onto its names.
func specValueExpr(spec *ast.ValueSpec, idx int) ast.Expr {
	switch {
	case len(spec.Values) == len(spec.Names):
		return spec.Values[idx]
	case len(spec.Values) == 1:
		return spec.Values[0] // Go repeats the single value for every name.
	}
	return nil
}

// evalExpr evaluates a constant expression with go/constant, classifying its
// type the way Go would: a LifecycleState(...) conversion, a reference to a
// LifecycleState-typed constant, or an arithmetic binary expression over one
// has type LifecycleState; comparisons yield bool; conversions to other types
// are readable but not LifecycleState.
func (s *lifecycleScanner) evalExpr(expr ast.Expr) (constValue, error) {
	expr = unwrapParens(expr)
	switch e := expr.(type) {
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		if v.Kind() == constant.Unknown {
			return constValue{}, fmt.Errorf("cannot evaluate literal %s", e.Value)
		}
		return constValue{val: v}, nil
	case *ast.Ident:
		return s.evalName(e.Name)
	case *ast.CallExpr:
		fun := unwrapParens(e.Fun)
		if s.isLifecycleStateType(fun) {
			if len(e.Args) != 1 {
				return constValue{}, fmt.Errorf("LifecycleState conversion must take one argument, has %d", len(e.Args))
			}
			arg, err := s.evalExpr(e.Args[0])
			if err != nil {
				return constValue{}, err
			}
			return constValue{val: arg.val, lc: true}, nil
		}
		// A conversion to some other type (string, time.Duration, ...): the
		// value is readable, but the result is not LifecycleState.
		if len(e.Args) == 1 {
			arg, err := s.evalExpr(e.Args[0])
			if err != nil {
				return constValue{}, err
			}
			return constValue{val: arg.val}, nil
		}
		return constValue{}, fmt.Errorf("unsupported call expression %s", goExpr(expr))
	case *ast.BinaryExpr:
		left, err := s.evalExpr(e.X)
		if err != nil {
			return constValue{}, err
		}
		right, err := s.evalExpr(e.Y)
		if err != nil {
			return constValue{}, err
		}
		switch e.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			// Comparisons yield bool, never LifecycleState.
			cmp, err := safeCompare(left.val, e.Op, right.val)
			if err != nil {
				return constValue{}, err
			}
			return constValue{val: constant.MakeBool(cmp)}, nil
		case token.SHL, token.SHR:
			// go/constant's BinaryOp panics on shift counts that are not
			// unsigned, while Go permits any integer constant count, so shifts
			// go through Shift with the count read as uint.
			if left.val.Kind() != constant.Int || right.val.Kind() != constant.Int {
				return constValue{}, fmt.Errorf("cannot evaluate %s: shift operands are not integers", goExpr(expr))
			}
			shift, exact := constant.Uint64Val(right.val)
			if !exact {
				return constValue{}, fmt.Errorf("cannot evaluate %s: shift count is not an unsigned integer", goExpr(expr))
			}
			return constValue{val: constant.Shift(left.val, e.Op, uint(shift)), lc: left.lc || right.lc}, nil
		}
		v, err := safeBinaryOp(left.val, e.Op, right.val)
		if err != nil {
			return constValue{}, err
		}
		return constValue{val: v, lc: left.lc || right.lc}, nil
	case *ast.UnaryExpr:
		x, err := s.evalExpr(e.X)
		if err != nil {
			return constValue{}, err
		}
		v, err := safeUnaryOp(e.Op, x.val)
		if err != nil {
			return constValue{}, err
		}
		return constValue{val: v}, nil // unary operators never yield LifecycleState
	default:
		return constValue{}, fmt.Errorf("unsupported constant expression %s; declare the value as a string literal", goExpr(expr))
	}
}

// safeBinaryOp computes constant.BinaryOp, converting the panics go/constant
// uses for unrepresentable operations (for example a shift count that is not
// an unsigned integer) into evaluation errors. A constant that cannot be read
// is either skipped (if unrelated to the lifecycle vocabulary) or a loud
// failure (if lifecycle-derived); it is never a panic.
func safeBinaryOp(left constant.Value, op token.Token, right constant.Value) (v constant.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			v = constant.MakeUnknown()
			err = fmt.Errorf("cannot evaluate %v %s %v", left, op, right)
		}
	}()
	return constant.BinaryOp(left, op, right), nil
}

// safeCompare computes constant.Compare, converting go/constant's panics for
// invalid comparisons into evaluation errors.
func safeCompare(left constant.Value, op token.Token, right constant.Value) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cannot compare %v %s %v", left, op, right)
		}
	}()
	return constant.Compare(left, op, right), nil
}

// safeUnaryOp computes constant.UnaryOp, converting go/constant's panics for
// invalid operations into evaluation errors.
func safeUnaryOp(op token.Token, x constant.Value) (v constant.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			v = constant.MakeUnknown()
			err = fmt.Errorf("cannot evaluate unary %s%v", op, x)
		}
	}()
	return constant.UnaryOp(op, x, 0), nil
}

// lifecycleDerived reports whether a constant's value expression is derived
// from the lifecycle vocabulary by syntax alone: it references a
// LifecycleState-typed constant (transitively) or calls the LifecycleState
// conversion. It is the fail-closed backstop used when evaluation fails, so an
// unreadable LifecycleState constant is a loud error instead of a silent skip.
func (s *lifecycleScanner) lifecycleDerived(spec *ast.ValueSpec, idx int, seen map[string]bool) bool {
	expr := specValueExpr(spec, idx)
	if expr == nil {
		return false
	}
	derived := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if s.isLifecycleStateType(n.Fun) {
				derived = true
				return false
			}
		case *ast.Ident:
			if s.nameIsLifecycleDerived(n.Name, seen) {
				derived = true
				return false
			}
		}
		return !derived
	})
	return derived
}

// nameIsLifecycleDerived reports whether the named constant is derived from
// the lifecycle vocabulary, following references through the package's
// declarations without evaluating anything. seen tracks only the current
// reference chain, so a name consulted twice is re-checked each time.
func (s *lifecycleScanner) nameIsLifecycleDerived(name string, seen map[string]bool) bool {
	if seen[name] {
		return false // cyclic reference; invalid Go anyway
	}
	seen[name] = true
	defer delete(seen, name)
	ref, ok := s.refs[name]
	if !ok {
		return false
	}
	if s.isLifecycleStateType(ref.spec.Type) {
		return true
	}
	return s.lifecycleDerived(ref.spec, ref.idx, seen)
}

// unwrapParens strips redundant parentheses from an expression. Go permits
// `const X (LifecycleState) = "v"` and `const X = (LifecycleState)("v")`, which
// parse to ParenExpr nodes at exactly the anchors this extractor inspects (the
// type, the conversion call's function, and its argument); without unwrapping,
// those valid forms would silently skip the exhaustive check.
func unwrapParens(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isLifecycleStateType reports whether an explicit type expression names the
// LifecycleState type — directly, through a type alias (type lifecycleAlias =
// LifecycleState, including alias chains), through a *generic* alias
// instantiated with any type arguments (type lifecycleAlias[T any] =
// LifecycleState; const X lifecycleAlias[int] = "v"), or with redundant
// parentheses. A *defined* type (type LifecycleNote LifecycleState) is a
// distinct type, not LifecycleState, and its constants are unrelated to the
// lifecycle vocabulary.
func (s *lifecycleScanner) isLifecycleStateType(expr ast.Expr) bool {
	return s.typeIsLifecycleState(expr, map[string]bool{})
}

// typeIsLifecycleState resolves a type expression to LifecycleState: an
// identifier that is LifecycleState itself or a package type alias of it, and
// an instantiation of a generic alias. Type arguments are substituted into the
// aliased expression before it is resolved, so `type A[T any] = T` with
// `A[LifecycleState]` is recognized (the argument carries the lifecycle type)
// while `A[int]` is not. seen guards cyclic alias chains (invalid Go anyway).
func (s *lifecycleScanner) typeIsLifecycleState(expr ast.Expr, seen map[string]bool) bool {
	switch e := unwrapParens(expr).(type) {
	case *ast.Ident:
		if e.Name == "LifecycleState" {
			return true
		}
		alias, ok := s.aliases[e.Name]
		if !ok || seen[e.Name] {
			return false
		}
		seen[e.Name] = true
		return s.typeIsLifecycleState(alias.rhs, seen)
	case *ast.IndexExpr:
		return s.typeIsLifecycleState(s.instantiate(e.X, []ast.Expr{e.Index}), seen)
	case *ast.IndexListExpr:
		return s.typeIsLifecycleState(s.instantiate(e.X, e.Indices), seen)
	}
	return false
}

// instantiate returns the aliased expression of a generic type alias with its
// type parameters replaced by the given type arguments. When base is not one
// of this package's aliases — an instantiated *defined* type, a generic
// function name, or an arity mismatch (invalid Go) — it is returned unchanged,
// and the caller's resolution then fails unless the base itself names
// LifecycleState.
func (s *lifecycleScanner) instantiate(base ast.Expr, args []ast.Expr) ast.Expr {
	ident, ok := unwrapParens(base).(*ast.Ident)
	if !ok {
		return base
	}
	alias, ok := s.aliases[ident.Name]
	if !ok || len(alias.params) != len(args) {
		return base
	}
	sub := make(map[string]ast.Expr, len(alias.params))
	for i, param := range alias.params {
		sub[param] = args[i]
	}
	return substituteTypeParams(alias.rhs, sub)
}

// substituteTypeParams replaces the type parameter names in a type expression
// with their arguments. Only type expressions reach this point (an alias's
// right-hand side), so every identifier is a type name or a type parameter.
func substituteTypeParams(expr ast.Expr, sub map[string]ast.Expr) ast.Expr {
	switch e := expr.(type) {
	case *ast.Ident:
		if repl, ok := sub[e.Name]; ok {
			return repl
		}
		return e
	case *ast.ParenExpr:
		return &ast.ParenExpr{X: substituteTypeParams(e.X, sub)}
	case *ast.IndexExpr:
		return &ast.IndexExpr{
			X:     substituteTypeParams(e.X, sub),
			Index: substituteTypeParams(e.Index, sub),
		}
	case *ast.IndexListExpr:
		indices := make([]ast.Expr, len(e.Indices))
		for i, index := range e.Indices {
			indices[i] = substituteTypeParams(index, sub)
		}
		return &ast.IndexListExpr{X: substituteTypeParams(e.X, sub), Indices: indices}
	case *ast.StarExpr:
		return &ast.StarExpr{X: substituteTypeParams(e.X, sub)}
	case *ast.SelectorExpr:
		return &ast.SelectorExpr{X: substituteTypeParams(e.X, sub), Sel: e.Sel}
	case *ast.ArrayType:
		return &ast.ArrayType{Lbrack: e.Lbrack, Len: substituteTypeParams(e.Len, sub), Elt: substituteTypeParams(e.Elt, sub)}
	case *ast.MapType:
		return &ast.MapType{Key: substituteTypeParams(e.Key, sub), Value: substituteTypeParams(e.Value, sub)}
	}
	return expr
}

// typeParamNames returns the names of a TypeSpec's type parameters in
// declaration order, or nil for a non-generic type.
func typeParamNames(params *ast.FieldList) []string {
	if params == nil {
		return nil
	}
	var names []string
	for _, field := range params.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

// goExpr renders an expression compactly for error messages.
func goExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.BasicLit:
		return e.Value
	case *ast.CallExpr:
		return fmt.Sprintf("%s(...)", goExpr(e.Fun))
	case *ast.ParenExpr:
		return "(" + goExpr(e.X) + ")"
	case *ast.BinaryExpr:
		return fmt.Sprintf("%s %s %s", goExpr(e.X), e.Op, goExpr(e.Y))
	case *ast.SelectorExpr:
		return goExpr(e.X) + "." + e.Sel.Name
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// TestAllLifecycleStatesExhaustive parses every non-test Go file in this
// package and fails unless AllLifecycleStates contains exactly the declared
// LifecycleState constants, in both directions. The whole package is scanned
// as one table, so a constant may reference another in any file.
func TestAllLifecycleStatesExhaustive(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	files := map[string]string{}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files[name] = string(source)
		names = append(names, name)
	}
	sort.Strings(names)

	states, err := declaredLifecycleStatesInFiles(files)
	if err != nil {
		t.Fatalf("extract LifecycleState constants: %v", err)
	}
	declared := map[string]bool{}
	for _, state := range states {
		declared[state] = true
	}

	listed := map[string]bool{}
	for _, state := range AllLifecycleStates {
		listed[string(state)] = true
	}

	for state := range declared {
		if !listed[state] {
			t.Errorf("LifecycleState constant %q in %s is missing from AllLifecycleStates; add it there so the task-relations parity test can require client coverage", state, names)
		}
	}
	for state := range listed {
		if !declared[state] {
			t.Errorf("AllLifecycleStates lists %q, but this package declares no such LifecycleState constant; remove the stale entry", state)
		}
	}
}

// The extractor itself is regression-tested against the declaration forms that
// defeated the previous line-regex scan: trailing comments, conversion
// declarations, grouped blocks, multi-name specs, parenthesized types or
// conversions, and implicit repetition must all be found; constant expressions
// must be evaluated the way the compiler types them; and an opaque value (iota,
// an unresolved reference) or an unevaluable lifecycle-derived expression must
// be a loud error, never a silent skip.

func TestDeclaredLifecycleStatesRecognizesEveryConstForm(t *testing.T) {
	source := `package coordinator

type LifecycleState string

const (
	// A trailing comment or a grouped block must not hide a constant.
	LifecycleScheduled  LifecycleState = "scheduled" // confirmed blocker until done
	LifecycleInProgress LifecycleState = "in_progress"
)

// A conversion without the explicit type is still a LifecycleState constant.
const LifecycleReviewing = LifecycleState("reviewing")

// An explicit-type conversion is a LifecycleState constant too.
const LifecycleDone LifecycleState = LifecycleState("done")

// One value for several names applies to all of them.
const LifecycleA, LifecycleB LifecycleState = "a"
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"scheduled", "in_progress", "reviewing", "done", "a", "a"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesEvaluatesConstantExpressions covers the
// type-inference forms: a constant declared without the explicit type but with
// a LifecycleState-typed expression — such as `const LifecyclePaused =
// LifecycleScheduled + "_paused"` — is a valid LifecycleState constant and
// must be read, not silently skipped, or a future server state could bypass
// the parity guard and render unknown.
func TestDeclaredLifecycleStatesEvaluatesConstantExpressions(t *testing.T) {
	source := `package coordinator

type LifecycleState string

const LifecycleScheduled LifecycleState = "scheduled"

// An untyped spec whose expression has inferred type LifecycleState.
const LifecyclePaused = LifecycleScheduled + "_paused"

// An explicit-type concatenation is a LifecycleState constant too.
const LifecycleReviewing LifecycleState = LifecycleScheduled + "_reviewing"

// A reference to a shared untyped suffix resolves through the package table.
const pausedSuffix = "_paused"
const LifecycleHeld = LifecycleScheduled + pausedSuffix

// String concatenation without a LifecycleState operand stays a plain string.
const greeting = "scheduled" + "_paused"

// A comparison over lifecycle constants yields bool, not LifecycleState.
const sameState = LifecycleScheduled == LifecycleDone

const LifecycleDone LifecycleState = "done"
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"scheduled", "scheduled_paused", "scheduled_reviewing", "scheduled_paused", "done"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesResolvesAcrossFiles: constants may reference each
// other across files, so the exhaustive check must scan the whole package as
// one table rather than one file at a time.
func TestDeclaredLifecycleStatesResolvesAcrossFiles(t *testing.T) {
	files := map[string]string{
		"tasks.go": `package coordinator

type LifecycleState string

const LifecycleScheduled LifecycleState = "scheduled"
`,
		"extras.go": `package coordinator

const LifecyclePaused = LifecycleScheduled + "_paused"
`,
	}
	states, err := declaredLifecycleStatesInFiles(files)
	if err != nil {
		t.Fatalf("declaredLifecycleStatesInFiles: %v", err)
	}
	// The package-level declaration order across files is not specified, so
	// compare as sets; within a file the order is preserved.
	got := slices.Clone(states)
	sort.Strings(got)
	want := []string{"scheduled", "scheduled_paused"}
	if !slices.Equal(got, want) {
		t.Errorf("declaredLifecycleStatesInFiles = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesRepeatsPreviousSpec: Go's implicit repetition in
// a const group — a spec without values repeats the previous spec's type and
// expression list — must not hide a LifecycleState constant.
func TestDeclaredLifecycleStatesRepeatsPreviousSpec(t *testing.T) {
	source := `package coordinator

type LifecycleState string

const (
	LifecycleA LifecycleState = "a"
	LifecycleB
	LifecycleC = LifecycleState("c")
	LifecycleD
)
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"a", "a", "c", "c"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

func TestDeclaredLifecycleStatesRejectsOpaqueValues(t *testing.T) {
	for _, source := range []string{
		`package coordinator

type LifecycleState string

const LifecycleA LifecycleState = iota
`,
		`package coordinator

type LifecycleState string

const LifecycleA LifecycleState = LifecycleScheduled
`,
		// An unevaluable expression derived from the lifecycle vocabulary must
		// fail too: the binary expression's first operand is a LifecycleState
		// constant, so the constant is LifecycleState-typed even though the
		// imported suffix cannot be read.
		`package coordinator

type LifecycleState string

const LifecycleScheduled LifecycleState = "scheduled"

const LifecycleA = LifecycleScheduled + missing.Suffix
`,
		// A LifecycleState conversion of an unreadable argument is
		// lifecycle-derived and must not be skipped.
		`package coordinator

type LifecycleState string

const LifecycleA = LifecycleState(missing.Suffix)
`,
		// Parentheses do not make an opaque value readable; the extractor must
		// still reject it rather than skip it.
		`package coordinator

type LifecycleState string

const LifecycleA LifecycleState = (LifecycleScheduled)
`,
		// An alias of LifecycleState is an exact synonym: an iota-based constant
		// with the alias as its type is LifecycleState-typed and unreadable, and
		// must be a loud error rather than a silent skip.
		`package coordinator

type LifecycleState string
type lifecycleAlias = LifecycleState

const LifecycleA lifecycleAlias = iota
`,
		// A conversion through the alias is a LifecycleState conversion, so an
		// unreadable argument is lifecycle-derived and must not be skipped.
		`package coordinator

type LifecycleState string
type lifecycleAlias = LifecycleState

const LifecycleA = lifecycleAlias(missing.Suffix)
`,
		// Alias chains resolve transitively, so an iota through a chain is still
		// a LifecycleState constant.
		`package coordinator

type LifecycleState string
type lifecycleAlias = LifecycleState
type aliasChain = lifecycleAlias

const LifecycleA aliasChain = iota
`,
		// An instantiated generic alias is still a LifecycleState type, so an
		// unreadable value under it is a loud error.
		`package coordinator

type LifecycleState string
type lifecycleAlias[T any] = LifecycleState

const LifecycleA lifecycleAlias[int] = iota
`,
		// A conversion through an instantiated generic alias is a
		// LifecycleState conversion, so an unreadable argument is
		// lifecycle-derived and must not be skipped.
		`package coordinator

type LifecycleState string
type lifecycleAlias[T any] = LifecycleState

const LifecycleA = lifecycleAlias[int](missing.Suffix)
`,
	} {
		if _, err := declaredLifecycleStates(source); err == nil {
			t.Errorf("declaredLifecycleStates accepted an opaque constant value:\n%s\nwant a hard error so the constant cannot evade the exhaustive check", source)
		}
	}
}

// Parenthesized forms are valid Go and parse to ParenExpr nodes at exactly the
// anchors the extractor inspects: the type (`const X (LifecycleState) = "v"`),
// the conversion's function (`const X = (LifecycleState)("v")`), and the
// conversion's argument (`const X = LifecycleState(("v"))`). Each must be
// recognized, or adding `const LifecycleX (LifecycleState) = "new"` would
// silently leave AllLifecycleStates stale and the registry bypassed.
func TestDeclaredLifecycleStatesRecognizesParenthesizedForms(t *testing.T) {
	source := `package coordinator

type LifecycleState string

// Parentheses around the explicit type are valid Go.
const LifecyclePaused (LifecycleState) = "paused"

// Parentheses around the conversion are valid Go too.
const LifecycleReviewing = (LifecycleState)("reviewing")

// An explicit-type conversion with parenthesized argument.
const LifecycleDone LifecycleState = (LifecycleState)("done")
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"paused", "reviewing", "done"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesRecognizesTypeAliases: a type alias (`type
// lifecycleAlias = LifecycleState`) is an exact type synonym, so a constant
// declared with the alias as its type — or converted through the alias — is a
// LifecycleState constant. The reviewer's exact form is `const LifecyclePaused
// lifecycleAlias = "paused"`: without alias resolution the extractor would
// skip it, a future server state could bypass the exhaustive check, and the
// web parity test would never see it.
func TestDeclaredLifecycleStatesRecognizesTypeAliases(t *testing.T) {
	source := `package coordinator

type LifecycleState string

// An alias is an exact synonym: the constant's type IS LifecycleState.
type lifecycleAlias = LifecycleState

const LifecyclePaused lifecycleAlias = "paused"

// A conversion through the alias is a LifecycleState conversion.
const LifecycleReviewing = lifecycleAlias("reviewing")

// Alias chains resolve transitively.
type aliasChain = lifecycleAlias
const LifecycleDone aliasChain = "done"

// An explicit LifecycleState type stays recognized alongside the aliases.
const LifecycleScheduled LifecycleState = "scheduled"

// A *defined* type is a distinct type, not LifecycleState, and its constants
// are unrelated to the lifecycle vocabulary — even when the underlying type
// is LifecycleState.
type LifecycleNote LifecycleState
const LifecycleNoteDraft LifecycleNote = "draft"
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"paused", "reviewing", "done", "scheduled"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesResolvesAliasesAcrossFiles: alias declarations
// are package scope, so an alias used in one file may be declared in another;
// the package-wide scan must resolve them the same way.
func TestDeclaredLifecycleStatesResolvesAliasesAcrossFiles(t *testing.T) {
	files := map[string]string{
		"types.go": `package coordinator

type LifecycleState string
type lifecycleAlias = LifecycleState
`,
		"tasks.go": `package coordinator

const LifecyclePaused lifecycleAlias = "paused"

const LifecycleReviewing = lifecycleAlias("reviewing")
`,
	}
	states, err := declaredLifecycleStatesInFiles(files)
	if err != nil {
		t.Fatalf("declaredLifecycleStatesInFiles: %v", err)
	}
	got := slices.Clone(states)
	sort.Strings(got)
	want := []string{"paused", "reviewing"}
	if !slices.Equal(got, want) {
		t.Errorf("declaredLifecycleStatesInFiles = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesRecognizesGenericTypeAliases: a *generic* type
// alias (`type lifecycleAlias[T any] = LifecycleState`) is an exact synonym
// for every instantiation, so `const LifecyclePaused lifecycleAlias[int] =
// "paused"` — the reviewer's exact form — is a valid LifecycleState
// declaration whose type parses to an *ast.IndexExpr. The extractor must
// resolve the instantiation (substituting the type argument into the aliased
// expression) instead of skipping it, or a future server state could bypass
// the exhaustive check and render unknown.
func TestDeclaredLifecycleStatesRecognizesGenericTypeAliases(t *testing.T) {
	source := `package coordinator

type LifecycleState string

// The reviewer's exact form: an instantiated generic alias as the type.
type lifecycleAlias[T any] = LifecycleState
const LifecyclePaused lifecycleAlias[int] = "paused"

// A conversion through an instantiated generic alias is a
// LifecycleState conversion.
const LifecycleReviewing = lifecycleAlias[int]("reviewing")

// Multi-parameter aliases and alias chains resolve too.
type multiAlias[A any, B any] = LifecycleState
const LifecycleDone multiAlias[int, string] = "done"

type aliasChain[T any] = lifecycleAlias[T]
const LifecycleBlocked aliasChain[bool] = "blocked"

// A *defined* generic type is a distinct type, not LifecycleState.
type LifecycleNote[T any] LifecycleState
const LifecycleNoteDraft LifecycleNote[int] = "draft"

// A plain non-generic alias still works alongside the generic ones.
type lifecyclePlain = LifecycleState
const LifecycleScheduled lifecyclePlain = "scheduled"
`
	states, err := declaredLifecycleStates(source)
	if err != nil {
		t.Fatalf("declaredLifecycleStates: %v", err)
	}
	want := []string{"paused", "reviewing", "done", "blocked", "scheduled"}
	if !slices.Equal(states, want) {
		t.Errorf("declaredLifecycleStates = %q, want %q", states, want)
	}
}

// TestDeclaredLifecycleStatesResolvesGenericAliasesAcrossFiles: generic alias
// declarations are package scope, so an instantiated alias used in one file
// may be declared in another; the package-wide scan must resolve it the same
// way.
func TestDeclaredLifecycleStatesResolvesGenericAliasesAcrossFiles(t *testing.T) {
	files := map[string]string{
		"types.go": `package coordinator

type LifecycleState string
type lifecycleAlias[T any] = LifecycleState
`,
		"tasks.go": `package coordinator

const LifecyclePaused lifecycleAlias[int] = "paused"

const LifecycleReviewing = lifecycleAlias[int]("reviewing")
`,
	}
	states, err := declaredLifecycleStatesInFiles(files)
	if err != nil {
		t.Fatalf("declaredLifecycleStatesInFiles: %v", err)
	}
	got := slices.Clone(states)
	sort.Strings(got)
	want := []string{"paused", "reviewing"}
	if !slices.Equal(got, want) {
		t.Errorf("declaredLifecycleStatesInFiles = %q, want %q", states, want)
	}
}
