package webassetbuild

import (
	"bytes"
	"fmt"
	"strings"
)

// Module is one component stylesheet plus the element its selectors are scoped
// to. An empty Scope leaves selectors alone, which is what the token sheet
// wants: :root and the theme media queries must reach the whole document.
type Module struct {
	Name   string
	Scope  string
	Source []byte
}

// BuildModules concatenates component stylesheets, each scoped to its own
// element. Element tag names are unique, so tag-name scoping isolates a
// component's rules without hashing class names.
func BuildModules(modules []Module) []byte {
	var output bytes.Buffer
	for _, module := range modules {
		fmt.Fprintf(&output, "/* Generated from internal/web/src/%s. */\n", module.Name)
		output.Write(buildScoped(module.Source, module.Scope))
		output.WriteByte('\n')
	}
	return output.Bytes()
}

// BuildCSS scopes module selectors to the Flow custom element.
func BuildCSS(source []byte) []byte {
	var output bytes.Buffer
	output.WriteString("/* Generated from internal/web/src/app.module.css. */\n")
	output.Write(buildScoped(source, "flow-app"))
	return output.Bytes()
}

func buildScoped(source []byte, scope string) []byte {
	var output bytes.Buffer

	var stack []string
	var selector []string
	for _, line := range strings.Split(strings.TrimRight(string(source), " \t\r\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flushSelector(&output, selector)
			selector = nil
			output.WriteByte('\n')
			continue
		}

		if strings.HasPrefix(trimmed, "@") && strings.Contains(trimmed, "{") && !inRule(stack) && !inRawAtRule(stack) {
			flushSelector(&output, selector)
			selector = nil
			output.WriteString(line)
			output.WriteByte('\n')
			if isConditionalGroupAtRule(trimmed) {
				stack = append(stack, "at")
			} else {
				stack = append(stack, "atraw")
			}
			continue
		}

		if trimmed == "}" {
			flushSelector(&output, selector)
			selector = nil
			output.WriteString(line)
			output.WriteByte('\n')
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}

		if inRule(stack) {
			output.WriteString(line)
			output.WriteByte('\n')
			continue
		}

		if inRawAtRule(stack) {
			output.WriteString(line)
			output.WriteByte('\n')
			if strings.Contains(trimmed, "{") && !strings.HasSuffix(trimmed, "}") {
				stack = append(stack, "rule")
			}
			continue
		}

		selector = append(selector, line)
		if strings.Contains(trimmed, "{") {
			flushScopedSelector(&output, selector, scope)
			selector = nil
			stack = append(stack, "rule")
		}
	}
	flushSelector(&output, selector)

	return output.Bytes()
}

func inRule(stack []string) bool {
	return len(stack) > 0 && stack[len(stack)-1] == "rule"
}

// inRawAtRule reports whether the innermost open block is an at-rule whose
// children are not selectors (e.g. @keyframes steps, @font-face descriptors)
// and must therefore pass through unscoped.
func inRawAtRule(stack []string) bool {
	return len(stack) > 0 && stack[len(stack)-1] == "atraw"
}

// isConditionalGroupAtRule reports whether the at-rule nests ordinary style
// rules whose selectors still need flow-app scoping.
func isConditionalGroupAtRule(trimmed string) bool {
	for _, prefix := range []string{"@media", "@supports", "@container"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func flushSelector(output *bytes.Buffer, lines []string) {
	for _, line := range lines {
		output.WriteString(line)
		output.WriteByte('\n')
	}
}

func flushScopedSelector(output *bytes.Buffer, lines []string, scope string) {
	for _, line := range lines {
		output.WriteString(scopeSelectorLine(line, scope))
		output.WriteByte('\n')
	}
}

func scopeSelectorLine(line string, scope string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimSpace(line)
	if scope == "" || body == "" || strings.HasPrefix(body, scope+" ") || body == scope || strings.HasPrefix(body, "@") {
		return line
	}

	suffix := ""
	if open := strings.Index(body, "{"); open >= 0 {
		suffix = body[open:]
		body = strings.TrimSpace(body[:open])
	}
	trailingComma := strings.HasSuffix(body, ",")
	body = strings.TrimSuffix(body, ",")
	body = strings.TrimSpace(body)

	if isRootSelector(body) || body == "body" {
		return line
	}

	switch {
	case body == "*":
		body = scope + ", " + scope + " *"
	case body == ":host":
		// A component sheet styles its own element box. Custom elements are
		// display: inline by default, so every component needs this.
		body = scope
	case strings.HasPrefix(body, ":host("):
		// :host(:hover) -> flow-x:hover, and :host(:hover) .y -> flow-x:hover .y.
		// These are light-DOM elements, so :host is only borrowed notation for
		// "this element"; there is no shadow boundary to cross.
		body = expandHostSelector(body, scope)
	default:
		body = scope + " " + body
	}
	if trailingComma {
		body += ","
	}
	if suffix != "" {
		body += " " + suffix
	}
	return indent + body
}

// expandHostSelector rewrites a :host(<inner>) prefix into scope<inner>,
// keeping whatever descendant selector follows.
func expandHostSelector(body string, scope string) string {
	rest := body[len(":host("):]
	depth := 1
	for index := range len(rest) {
		switch rest[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return scope + rest[:index] + rest[index+1:]
			}
		}
	}
	return scope + " " + body
}

func isRootSelector(selector string) bool {
	return selector == ":root" || strings.HasPrefix(selector, ":root[") || strings.HasPrefix(selector, ":root:")
}
