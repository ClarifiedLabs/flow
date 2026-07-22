package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Claude does not expose its model picker as machine-readable output. Keep its
// pinned model IDs curated so users can deliberately select older versions,
// and offer family aliases for users who want Claude Code's latest resolution.
var claudeEffortFull = []string{"low", "medium", "high", "xhigh", "max"}

// Claude 4.6 models support max effort but not xhigh.
var claudeEffortNoXHigh = []string{"low", "medium", "high", "max"}

type codexModelCatalog struct {
	Models []codexCatalogModel `json:"models"`
}

type codexCatalogModel struct {
	Slug                     string                       `json:"slug"`
	DisplayName              string                       `json:"display_name"`
	Visibility               string                       `json:"visibility"`
	SupportedReasoningLevels []codexCatalogReasoningLevel `json:"supported_reasoning_levels"`
}

type codexCatalogReasoningLevel struct {
	Effort string `json:"effort"`
}

func curatedModel(provider, providerName, id, name string, efforts []string) Model {
	model := Model{
		ProviderID:   provider,
		ProviderName: providerName,
		ModelID:      id,
		ModelName:    name,
	}
	if len(efforts) > 0 {
		model.Reasoning = ReasoningInfo{
			Supported: true,
			Options:   []ReasoningOption{{Type: "effort", Values: append([]string(nil), efforts...)}},
		}
	}
	return model
}

// CuratedClaudeModels is the curated Anthropic model catalog selectable for the
// claude harness (`claude --model <id> --effort <level>`).
func CuratedClaudeModels() ([]Model, error) {
	return []Model{
		curatedModel("anthropic", "Anthropic", "fable", "Claude Fable (latest)", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "opus", "Claude Opus (latest)", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "sonnet", "Claude Sonnet (latest)", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "haiku", "Claude Haiku (latest)", nil),
		curatedModel("anthropic", "Anthropic", "claude-fable-5", "Claude Fable 5", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-opus-4-8", "Claude Opus 4.8", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-opus-4-7", "Claude Opus 4.7", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-opus-4-6", "Claude Opus 4.6", claudeEffortNoXHigh),
		curatedModel("anthropic", "Anthropic", "claude-sonnet-5", "Claude Sonnet 5", claudeEffortFull),
		curatedModel("anthropic", "Anthropic", "claude-sonnet-4-6", "Claude Sonnet 4.6", claudeEffortNoXHigh),
		curatedModel("anthropic", "Anthropic", "claude-haiku-4-5", "Claude Haiku 4.5", nil),
	}, nil
}

// AvailableCodexModels returns the list-visible catalog exposed by the installed
// Codex CLI. The default command refreshes account-visible availability; the
// bundled catalog is a fail-soft fallback when refresh or decoding fails.
func AvailableCodexModels() ([]Model, error) {
	executable, err := exec.LookPath(Codex)
	if err != nil {
		return nil, err
	}

	models, refreshErr := loadCodexModels(executable, false)
	if refreshErr == nil {
		return models, nil
	}
	models, bundledErr := loadCodexModels(executable, true)
	if bundledErr != nil {
		return nil, fmt.Errorf("load codex model catalog: refreshed: %v; bundled: %w", refreshErr, bundledErr)
	}
	return models, nil
}

func loadCodexModels(executable string, bundled bool) ([]Model, error) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityCheckTimeout)
	defer cancel()

	args := []string{"debug", "models"}
	if bundled {
		args = append(args, "--bundled")
	}
	output, err := exec.CommandContext(ctx, executable, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("run codex model discovery: %w", err)
	}
	return decodeCodexModelCatalog(output)
}

func decodeCodexModelCatalog(data []byte) ([]Model, error) {
	var catalog codexModelCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode codex model catalog: %w", err)
	}
	if len(catalog.Models) == 0 {
		return nil, errors.New("decode codex model catalog: no models")
	}

	models := make([]Model, 0, len(catalog.Models))
	for _, item := range catalog.Models {
		if strings.TrimSpace(item.Visibility) != "list" {
			continue
		}
		slug := strings.TrimSpace(item.Slug)
		if slug == "" {
			return nil, errors.New("decode codex model catalog: list-visible model has no slug")
		}
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = slug
		}

		efforts := make([]string, 0, len(item.SupportedReasoningLevels))
		seenEfforts := map[string]bool{}
		for _, level := range item.SupportedReasoningLevels {
			effort := strings.TrimSpace(level.Effort)
			if effort == "" || seenEfforts[effort] {
				continue
			}
			seenEfforts[effort] = true
			efforts = append(efforts, effort)
		}
		models = append(models, curatedModel("openai", "OpenAI", slug, name, efforts))
	}
	if len(models) == 0 {
		return nil, errors.New("decode codex model catalog: no list-visible models")
	}
	return models, nil
}
