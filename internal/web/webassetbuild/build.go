package webassetbuild

import (
	"bytes"
	"strings"
)

const generatedCSSHeader = "/* Generated from internal/web/src/app.module.css. */\n"

// BuildCSS scopes module selectors to the Flow custom element.
func BuildCSS(source []byte) []byte {
	var output bytes.Buffer
	output.WriteString(generatedCSSHeader)

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
			flushScopedSelector(&output, selector)
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

func flushScopedSelector(output *bytes.Buffer, lines []string) {
	for _, line := range lines {
		output.WriteString(scopeSelectorLine(line))
		output.WriteByte('\n')
	}
}

func scopeSelectorLine(line string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimSpace(line)
	if body == "" || strings.HasPrefix(body, "flow-app ") || strings.HasPrefix(body, "@") {
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

	switch body {
	case "*":
		body = "flow-app, flow-app *"
	default:
		body = "flow-app " + body
	}
	if trailingComma {
		body += ","
	}
	if suffix != "" {
		body += " " + suffix
	}
	return indent + body
}

func isRootSelector(selector string) bool {
	return selector == ":root" || strings.HasPrefix(selector, ":root[") || strings.HasPrefix(selector, ":root:")
}
