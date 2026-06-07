package harness

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Args holds additive argv tokens for Flow-managed harness commands.
type Args struct {
	Codex   []string `json:"codex,omitempty" yaml:"codex,omitempty"`
	Claude  []string `json:"claude,omitempty" yaml:"claude,omitempty"`
	Harness []string `json:"harness,omitempty" yaml:"harness,omitempty"`
}

// ArgsPatch is used by partial updates: a nil slice pointer leaves that harness
// unchanged, while a non-nil empty slice clears it.
type ArgsPatch struct {
	Codex   *[]string `json:"codex,omitempty" yaml:"codex,omitempty"`
	Claude  *[]string `json:"claude,omitempty" yaml:"claude,omitempty"`
	Harness *[]string `json:"harness,omitempty" yaml:"harness,omitempty"`
}

func NormalizeArgs(args Args) (Args, error) {
	codex, err := normalizeArgList(Codex, args.Codex)
	if err != nil {
		return Args{}, err
	}
	claude, err := normalizeArgList(Claude, args.Claude)
	if err != nil {
		return Args{}, err
	}
	harness, err := normalizeArgList(Harness, args.Harness)
	if err != nil {
		return Args{}, err
	}
	return Args{Codex: codex, Claude: claude, Harness: harness}, nil
}

func NormalizeArgsPatch(patch ArgsPatch) (ArgsPatch, error) {
	var normalized ArgsPatch
	if patch.Codex != nil {
		codex, err := normalizeArgList(Codex, *patch.Codex)
		if err != nil {
			return ArgsPatch{}, err
		}
		normalized.Codex = &codex
	}
	if patch.Claude != nil {
		claude, err := normalizeArgList(Claude, *patch.Claude)
		if err != nil {
			return ArgsPatch{}, err
		}
		normalized.Claude = &claude
	}
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
	case Codex:
		return copyArgs(args.Codex)
	case Claude:
		return copyArgs(args.Claude)
	case Harness:
		return copyArgs(args.Harness)
	default:
		return nil
	}
}

func (args Args) Add(other Args) Args {
	return Args{
		Codex:   append(copyArgs(args.Codex), other.Codex...),
		Claude:  append(copyArgs(args.Claude), other.Claude...),
		Harness: append(copyArgs(args.Harness), other.Harness...),
	}
}

func (args Args) ApplyPatch(patch ArgsPatch) Args {
	out := Args{
		Codex:   copyArgs(args.Codex),
		Claude:  copyArgs(args.Claude),
		Harness: copyArgs(args.Harness),
	}
	if patch.Codex != nil {
		out.Codex = copyArgs(*patch.Codex)
	}
	if patch.Claude != nil {
		out.Claude = copyArgs(*patch.Claude)
	}
	if patch.Harness != nil {
		out.Harness = copyArgs(*patch.Harness)
	}
	return out
}

func (args Args) Empty() bool {
	return len(args.Codex) == 0 && len(args.Claude) == 0 && len(args.Harness) == 0
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
// Flow-managed flag or (for harnesses that reserve config keys, i.e. codex) a
// Flow-managed configuration key passed via -c/--config.
func (d Definition) validateArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if d.flagManaged(arg) {
			return fmt.Errorf("%s harness arg %q overrides a Flow-managed flag", d.Name, arg)
		}
		if len(d.ManagedConfigKeys) == 0 {
			continue
		}
		switch {
		case arg == "-c" || arg == "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("%s harness arg %q requires a value", d.Name, arg)
			}
			if d.configKeyManaged(args[i+1]) {
				return fmt.Errorf("%s harness config %q overrides Flow-managed configuration", d.Name, args[i+1])
			}
			i++
		case strings.HasPrefix(arg, "--config="):
			value := strings.TrimPrefix(arg, "--config=")
			if value == "" {
				return fmt.Errorf("%s harness arg %q requires a value", d.Name, "--config")
			}
			if d.configKeyManaged(value) {
				return fmt.Errorf("%s harness config %q overrides Flow-managed configuration", d.Name, value)
			}
		case strings.HasPrefix(arg, "-c="):
			value := strings.TrimPrefix(arg, "-c=")
			if value == "" {
				return fmt.Errorf("%s harness arg %q requires a value", d.Name, "-c")
			}
			if d.configKeyManaged(value) {
				return fmt.Errorf("%s harness config %q overrides Flow-managed configuration", d.Name, value)
			}
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
