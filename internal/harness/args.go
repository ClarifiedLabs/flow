package harness

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Args holds additive argv tokens for Flow-managed harness commands.
type Args struct {
	Harness []string `json:"harness,omitempty" yaml:"harness,omitempty"`
}

// ArgsPatch is used by partial updates: a nil slice pointer leaves the harness
// args unchanged, while a non-nil empty slice clears them.
type ArgsPatch struct {
	Harness *[]string `json:"harness,omitempty" yaml:"harness,omitempty"`
}

func NormalizeArgs(args Args) (Args, error) {
	harness, err := normalizeArgList(Harness, args.Harness)
	if err != nil {
		return Args{}, err
	}
	return Args{Harness: harness}, nil
}

func NormalizeArgsPatch(patch ArgsPatch) (ArgsPatch, error) {
	var normalized ArgsPatch
	if patch.Harness != nil {
		harness, err := normalizeArgList(Harness, *patch.Harness)
		if err != nil {
			return ArgsPatch{}, err
		}
		normalized.Harness = &harness
	}
	return normalized, nil
}

func (args Args) For(name string) []string {
	switch NormalizeName(name) {
	case Harness:
		return copyArgs(args.Harness)
	default:
		return nil
	}
}

func (args Args) Add(other Args) Args {
	return Args{
		Harness: append(copyArgs(args.Harness), other.Harness...),
	}
}

func (args Args) ApplyPatch(patch ArgsPatch) Args {
	out := Args{
		Harness: copyArgs(args.Harness),
	}
	if patch.Harness != nil {
		out.Harness = copyArgs(*patch.Harness)
	}
	return out
}

func (args Args) Empty() bool {
	return len(args.Harness) == 0
}

// ArgsFor returns an Args carrying tokens under the named harness's slot,
// e.g. the serialized model/effort selection for an agent definition.
func ArgsFor(name string, tokens []string) Args {
	switch NormalizeName(name) {
	case Harness:
		return Args{Harness: copyArgs(tokens)}
	default:
		return Args{}
	}
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

func copyArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	return append([]string(nil), args...)
}
