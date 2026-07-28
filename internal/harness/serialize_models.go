package harness

import (
	"fmt"
	"strings"
)

// SerializeModelSelection renders the harness argv tokens for a Flow-managed
// model + reasoning-effort choice. Empty model or effort omits that half of the
// selection; both empty yields no tokens. It is the Go twin of
// serializeHarnessModelSelection in internal/web/assets/harness-models.js — the
// two must stay in agreement:
//   - harness: --model <target> --reasoning <profile>
func SerializeModelSelection(name, model, effort string) ([]string, error) {
	if NormalizeName(name) != Harness {
		return nil, fmt.Errorf("unsupported harness %q for model selection", name)
	}

	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	if strings.ContainsAny(model, " \t\n") {
		return nil, fmt.Errorf("model %q contains whitespace", model)
	}
	if strings.ContainsAny(effort, " \t\n") {
		return nil, fmt.Errorf("reasoning effort %q contains whitespace", effort)
	}

	var args []string
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--reasoning", effort)
	}

	return args, nil
}
