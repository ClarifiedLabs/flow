package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ModelCatalog is the versioned JSON shape emitted by:
//
//	harness --models --format json
type ModelCatalog struct {
	Version       int     `json:"version"`
	ProviderCount int     `json:"provider_count,omitempty"`
	ModelCount    int     `json:"model_count,omitempty"`
	Models        []Model `json:"models,omitempty"`
}

type Model struct {
	TargetID                 string        `json:"target_id,omitempty"`
	DisplayName              string        `json:"display_name,omitempty"`
	ProviderLabel            string        `json:"provider_label,omitempty"`
	ModelLabel               string        `json:"model_label,omitempty"`
	ProviderID               string        `json:"provider_id"`
	ProviderName             string        `json:"provider_name,omitempty"`
	ModelID                  string        `json:"model_id"`
	QualifiedID              string        `json:"qualified_id"`
	ModelName                string        `json:"model_name,omitempty"`
	ContextWindow            int           `json:"context_window,omitempty"`
	InputModalities          []string      `json:"input_modalities,omitempty"`
	ServerTools              []string      `json:"server_tools,omitempty"`
	PricePerMillionTokensUSD *ModelPrice   `json:"price_per_million_tokens_usd,omitempty"`
	Reasoning                ReasoningInfo `json:"reasoning"`
	// Harness is the agent harness this model belongs to (lowercased). It is
	// stamped by Definition.AvailableModels so a per-harness model catalog can be
	// aggregated, persisted, and intersected without re-deriving the owner.
	Harness string `json:"harness,omitempty"`
}

type ModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type ReasoningInfo struct {
	Supported bool              `json:"supported"`
	Options   []ReasoningOption `json:"options,omitempty"`
}

var portableReasoningProfiles = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func portableReasoningProfileOption() ReasoningOption {
	return ReasoningOption{Type: "profile", Values: append([]string(nil), portableReasoningProfiles...)}
}

func (info *ReasoningInfo) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "true":
		*info = ReasoningInfo{Supported: true, Options: []ReasoningOption{portableReasoningProfileOption()}}
		return nil
	case "false", "null":
		*info = ReasoningInfo{}
		return nil
	}

	type reasoningInfo ReasoningInfo
	var decoded reasoningInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*info = ReasoningInfo(decoded)
	return nil
}

type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
	Min    *int     `json:"min,omitempty"`
	Max    *int     `json:"max,omitempty"`
}

func AvailableHarnessModels() ([]Model, error) {
	executable, err := exec.LookPath("harness")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityCheckTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "--models", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	catalog, err := DecodeModelCatalog(output)
	if err != nil {
		return nil, err
	}
	return CloneModels(catalog.Models), nil
}

func DecodeModelCatalog(data []byte) (ModelCatalog, error) {
	var catalog ModelCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return ModelCatalog{}, fmt.Errorf("decode harness model catalog: %w", err)
	}
	return NormalizeModelCatalog(catalog)
}

func NormalizeModelCatalog(catalog ModelCatalog) (ModelCatalog, error) {
	if catalog.Version != 1 {
		return ModelCatalog{}, fmt.Errorf("unsupported harness model catalog version %d", catalog.Version)
	}
	models, err := NormalizeModels(catalog.Models)
	if err != nil {
		return ModelCatalog{}, err
	}
	catalog.Models = models
	catalog.ProviderCount = countModelProviders(models)
	catalog.ModelCount = len(models)
	return catalog, nil
}

func NormalizeModels(models []Model) ([]Model, error) {
	if len(models) == 0 {
		return nil, nil
	}
	normalized := make([]Model, 0, len(models))
	seen := map[string]bool{}
	for i, model := range models {
		item, err := normalizeModel(model)
		if err != nil {
			return nil, fmt.Errorf("harness model %d: %w", i+1, err)
		}
		if seen[item.QualifiedID] {
			continue
		}
		seen[item.QualifiedID] = true
		normalized = append(normalized, item)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].ProviderID != normalized[j].ProviderID {
			return normalized[i].ProviderID < normalized[j].ProviderID
		}
		return normalized[i].ModelID < normalized[j].ModelID
	})
	return normalized, nil
}

func CloneModels(models []Model) []Model {
	if len(models) == 0 {
		return nil
	}
	cloned := make([]Model, len(models))
	for i, model := range models {
		cloned[i] = cloneModel(model)
	}
	return cloned
}

func normalizeModel(model Model) (Model, error) {
	model.TargetID = strings.TrimSpace(model.TargetID)
	model.DisplayName = strings.TrimSpace(model.DisplayName)
	model.ProviderLabel = strings.TrimSpace(model.ProviderLabel)
	model.ModelLabel = strings.TrimSpace(model.ModelLabel)
	model.ProviderID = strings.TrimSpace(model.ProviderID)
	model.ProviderName = strings.TrimSpace(model.ProviderName)
	model.ModelID = strings.TrimSpace(model.ModelID)
	model.QualifiedID = strings.TrimSpace(model.QualifiedID)
	model.ModelName = strings.TrimSpace(model.ModelName)
	model.Harness = strings.ToLower(strings.TrimSpace(model.Harness))
	if model.TargetID == "" && strings.TrimSpace(model.QualifiedID) != "" {
		model.TargetID = model.QualifiedID
	}
	targetProvider, targetModel := splitModelTarget(model.TargetID)
	if model.ProviderID == "" {
		model.ProviderID = firstNonEmpty(targetProvider, model.ProviderLabel)
	}
	if model.ModelID == "" {
		model.ModelID = firstNonEmpty(model.ModelLabel, targetModel)
	}
	if model.ProviderName == "" {
		model.ProviderName = firstNonEmpty(model.ProviderLabel, model.ProviderID)
	}
	if model.ModelName == "" {
		model.ModelName = firstNonEmpty(model.DisplayName, model.ModelLabel, model.ModelID)
	}
	if model.ProviderID == "" {
		return Model{}, errors.New("provider_id is required")
	}
	if model.ModelID == "" {
		return Model{}, errors.New("model_id is required")
	}
	if model.QualifiedID == "" {
		model.QualifiedID = firstNonEmpty(model.TargetID, model.ProviderID+":"+model.ModelID)
	}
	if model.ContextWindow < 0 {
		return Model{}, errors.New("context_window cannot be negative")
	}
	model.Reasoning = normalizeReasoningInfo(model.Reasoning)
	return cloneModel(model), nil
}

func normalizeReasoningInfo(info ReasoningInfo) ReasoningInfo {
	if len(info.Options) == 0 {
		return info
	}
	options := make([]ReasoningOption, 0, len(info.Options))
	for _, option := range info.Options {
		option.Type = strings.TrimSpace(option.Type)
		if option.Type == "" {
			continue
		}
		values := make([]string, 0, len(option.Values))
		for _, value := range option.Values {
			value = strings.TrimSpace(value)
			if value != "" {
				values = append(values, value)
			}
		}
		option.Values = values
		options = append(options, cloneReasoningOption(option))
	}
	info.Options = options
	return info
}

func cloneModel(model Model) Model {
	model.InputModalities = append([]string(nil), model.InputModalities...)
	model.ServerTools = append([]string(nil), model.ServerTools...)
	if model.PricePerMillionTokensUSD != nil {
		price := *model.PricePerMillionTokensUSD
		model.PricePerMillionTokensUSD = &price
	}
	model.Reasoning = cloneReasoningInfo(model.Reasoning)
	return model
}

func splitModelTarget(target string) (string, string) {
	before, after, ok := strings.Cut(strings.TrimSpace(target), ":")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(before), strings.TrimSpace(after)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func cloneReasoningInfo(info ReasoningInfo) ReasoningInfo {
	if len(info.Options) == 0 {
		return info
	}
	info.Options = append([]ReasoningOption(nil), info.Options...)
	for i := range info.Options {
		info.Options[i] = cloneReasoningOption(info.Options[i])
	}
	return info
}

func cloneReasoningOption(option ReasoningOption) ReasoningOption {
	option.Values = append([]string(nil), option.Values...)
	if option.Min != nil {
		value := *option.Min
		option.Min = &value
	}
	if option.Max != nil {
		value := *option.Max
		option.Max = &value
	}
	return option
}

func countModelProviders(models []Model) int {
	providers := map[string]bool{}
	for _, model := range models {
		providers[model.ProviderID] = true
	}
	return len(providers)
}
