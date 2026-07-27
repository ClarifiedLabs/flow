package harness

import "strings"

// AgentSelection is a harness + model + reasoning-effort choice. It carries
// the coordinator's configurable default agent (config file `default_agent`
// block) through the config, coordinator, and api packages; the zero value
// resolves to the built-in default harness with no model override.
type AgentSelection struct {
	Harness         string
	Model           string
	ReasoningEffort string
}

// DefaultAgentSelection returns the built-in fallback: the default agent
// harness with the harness CLI's own default model.
func DefaultAgentSelection() AgentSelection {
	return AgentSelection{Harness: DefaultAgentName()}
}

// ResolveAgentSelection normalizes and validates a selection: the harness name
// is normalized (empty falls back to the default agent harness), the model and
// effort are trimmed, and the combination is validated by the same serializer
// that later renders the job argv tokens.
func ResolveAgentSelection(sel AgentSelection) (AgentSelection, error) {
	resolved := AgentSelection{
		Harness:         NormalizeName(sel.Harness),
		Model:           strings.TrimSpace(sel.Model),
		ReasoningEffort: strings.TrimSpace(sel.ReasoningEffort),
	}
	if resolved.Harness == "" {
		resolved.Harness = DefaultAgentName()
	}
	if err := ValidateAgentName(resolved.Harness); err != nil {
		return AgentSelection{}, err
	}
	if _, err := SerializeModelSelection(resolved.Harness, resolved.Model, resolved.ReasoningEffort); err != nil {
		return AgentSelection{}, err
	}
	return resolved, nil
}

// ModelArgs renders the selection's model/effort choice as harness argv tokens
// (empty when neither is set).
func (s AgentSelection) ModelArgs() ([]string, error) {
	return SerializeModelSelection(s.Harness, s.Model, s.ReasoningEffort)
}
