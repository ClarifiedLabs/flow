package harness

// Curated model catalogs for the agent CLIs that have no machine-readable
// `--models` output (claude and codex). The harness CLI keeps its dynamic
// catalog via AvailableHarnessModels; these static lists give claude and codex
// the same Flow-selectable model/reasoning UI.
//
// Reasoning options use the "effort" type whose Values are the exact tokens the
// CLI accepts:
//   - claude: `--effort <value>` (low|medium|high|xhigh|max)
//   - codex:  `-c model_reasoning_effort=<value>` (low|medium|high|xhigh)
//
// Model IDs are the canonical strings `<cli> --model <id>` accepts. They are
// curated and may need owner confirmation as new models ship.

var claudeEffortFull = []string{"low", "medium", "high", "xhigh", "max"}

// claudeEffortNoTop covers models (e.g. Haiku) that do not offer the xhigh/max
// effort tiers.
var claudeEffortNoTop = []string{"low", "medium", "high"}

// codexEffortLevels are the reasoning tiers the listed gpt-5.x models advertise
// via `codex debug models` (no "minimal" tier for these models).
var codexEffortLevels = []string{"low", "medium", "high", "xhigh"}

func curatedModel(provider, providerName, id, name string, efforts []string) Model {
	return Model{
		ProviderID:   provider,
		ProviderName: providerName,
		ModelID:      id,
		ModelName:    name,
		Reasoning: ReasoningInfo{
			Supported: true,
			Options:   []ReasoningOption{{Type: "effort", Values: append([]string(nil), efforts...)}},
		},
	}
}

// CuratedClaudeModels is the curated Anthropic model catalog selectable for the
// claude harness (`claude --model <id> --effort <level>`).
func CuratedClaudeModels() ([]Model, error) {
	return []Model{
		curatedModel("anthropic", "Anthropic", "claude-opus-4-8", "Claude Opus 4.8", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-opus-4-7", "Claude Opus 4.7", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-sonnet-4-6", "Claude Sonnet 4.6", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-haiku-4-5", "Claude Haiku 4.5", claudeEffortNoTop),
		curatedModel("anthropic", "Anthropic", "claude-fable-5", "Claude Fable 5", claudeEffortFull),
	}, nil
}

// CuratedCodexModels is the curated OpenAI model catalog selectable for the codex
// harness (`codex --model <id> -c model_reasoning_effort=<level>`). The slugs and
// effort tiers mirror `codex debug models` (list-visible, API-available models).
func CuratedCodexModels() ([]Model, error) {
	return []Model{
		curatedModel("openai", "OpenAI", "gpt-5.5", "GPT-5.5", codexEffortLevels),
		curatedModel("openai", "OpenAI", "gpt-5.4", "GPT-5.4", codexEffortLevels),
		curatedModel("openai", "OpenAI", "gpt-5.4-mini", "GPT-5.4 mini", codexEffortLevels),
	}, nil
}
