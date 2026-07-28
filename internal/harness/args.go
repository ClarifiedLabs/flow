package harness

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// NormalizeArgs validates and normalizes additive argv tokens for Flow-managed
// harness commands: each entry may be a shell-style string containing several
// tokens (split with quote handling), and no resulting token may override a
// Flow-managed flag.
func NormalizeArgs(args []string) ([]string, error) {
	return normalizeArgList(Harness, args)
}

func normalizeArgList(name string, args []string) ([]string, error) {
	normalized := make([]string, 0, len(args))
	for i, arg := range args {
		fields, err := parseShellArgFields(arg)
		if err != nil {
			return nil, fmt.Errorf("%s harness arg %d: %w", name, i+1, err)
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("%s harness arg %d is empty", name, i+1)
		}
		for _, field := range fields {
			if strings.TrimSpace(field) == "" {
				return nil, fmt.Errorf("%s harness arg %d contains an empty argv token", name, i+1)
			}
			normalized = append(normalized, field)
		}
	}
	if err := validateManagedArgOverrides(name, normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func parseShellArgFields(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	var fields []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if quote != 0 {
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"':
				escaped = true
			default:
				current.WriteRune(r)
			}
			started = true
			continue
		}

		switch {
		case unicode.IsSpace(r):
			if started {
				fields = append(fields, current.String())
				current.Reset()
				started = false
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\\':
			escaped = true
			started = true
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("ends with an unfinished escape")
	}
	if quote != 0 {
		return nil, errors.New("contains an unmatched quote")
	}
	if started {
		fields = append(fields, current.String())
	}
	return fields, nil
}

func validateManagedArgOverrides(name string, args []string) error {
	definition, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("unsupported harness %q", name)
	}
	return definition.validateArgs(args)
}

// validateArgs rejects user-supplied harness args that would override a
// Flow-managed flag.
func (d Definition) validateArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if d.flagManaged(arg) {
			return fmt.Errorf("%s harness arg %q overrides a Flow-managed flag", d.Name, arg)
		}
	}
	return nil
}
