package harness

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeModelCatalogNormalizesAndSortsModels(t *testing.T) {
	raw := []byte(`{
		"version": 1,
		"models": [
			{
				"provider_id": "google",
				"provider_name": "Google",
				"model_id": "gemini-3.5-flash",
				"qualified_id": "google:gemini-3.5-flash",
				"context_window": 1048576,
				"reasoning": {"supported": true, "options": [{"type": "toggle"}]}
			},
			{
				"provider_id": "anthropic",
				"model_id": "claude-opus-4-8",
				"reasoning": {"supported": true, "options": [{"type": "effort", "values": [" low ", "high"]}]}
			}
		]
	}`)

	catalog, err := DecodeModelCatalog(raw)
	if err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if catalog.ProviderCount != 2 || catalog.ModelCount != 2 {
		t.Fatalf("counts = providers:%d models:%d, want 2/2", catalog.ProviderCount, catalog.ModelCount)
	}
	if got := catalog.Models[0].QualifiedID; got != "anthropic:claude-opus-4-8" {
		t.Fatalf("first model qualified id = %q, want anthropic:claude-opus-4-8", got)
	}
	values := catalog.Models[0].Reasoning.Options[0].Values
	if len(values) != 2 || values[0] != "low" || values[1] != "high" {
		t.Fatalf("effort values = %#v, want trimmed low/high", values)
	}
}

func TestDecodeModelCatalogAcceptsHarnessTargetModels(t *testing.T) {
	raw := []byte(`{
		"version": 1,
		"model_count": 1,
		"models": [
			{
				"target_id": "openrouter:openai/gpt-5.5",
				"display_name": "OpenAI GPT-5.5",
				"provider_label": "OpenRouter",
				"model_label": "openai/gpt-5.5",
				"context_window": 256000,
				"input_modalities": ["text", "image"],
				"server_tools": ["web_search"],
				"reasoning": true
			}
		]
	}`)

	catalog, err := DecodeModelCatalog(raw)
	if err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if catalog.ProviderCount != 1 || catalog.ModelCount != 1 {
		t.Fatalf("counts = providers:%d models:%d, want 1/1", catalog.ProviderCount, catalog.ModelCount)
	}
	got := catalog.Models[0]
	if got.TargetID != "openrouter:openai/gpt-5.5" || got.QualifiedID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("target/qualified id = %q/%q, want openrouter:openai/gpt-5.5", got.TargetID, got.QualifiedID)
	}
	if got.ProviderID != "openrouter" || got.ModelID != "openai/gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want openrouter/openai/gpt-5.5", got.ProviderID, got.ModelID)
	}
	if got.ProviderName != "OpenRouter" || got.ModelName != "OpenAI GPT-5.5" {
		t.Fatalf("display names = %q/%q, want OpenRouter/OpenAI GPT-5.5", got.ProviderName, got.ModelName)
	}
	if len(got.InputModalities) != 2 || got.InputModalities[1] != "image" {
		t.Fatalf("input modalities = %#v", got.InputModalities)
	}
	if len(got.ServerTools) != 1 || got.ServerTools[0] != "web_search" {
		t.Fatalf("server tools = %#v", got.ServerTools)
	}
	if !got.Reasoning.Supported || len(got.Reasoning.Options) != 1 || got.Reasoning.Options[0].Type != "profile" {
		t.Fatalf("reasoning = %#v, want portable profile option", got.Reasoning)
	}
	values := got.Reasoning.Options[0].Values
	if strings.Join(values, ",") != "none,minimal,low,medium,high,xhigh,max" {
		t.Fatalf("reasoning profiles = %#v", values)
	}
}

func TestDecodeModelCatalogRejectsUnsupportedVersion(t *testing.T) {
	if _, err := DecodeModelCatalog([]byte(`{"version":2}`)); err == nil {
		t.Fatal("DecodeModelCatalog accepted unsupported version")
	}
}

func TestNormalizeModelLowercasesHarness(t *testing.T) {
	got, err := normalizeModel(Model{ProviderID: "anthropic", ModelID: "claude-opus-4-8", Harness: "Claude"})
	if err != nil {
		t.Fatalf("normalizeModel: %v", err)
	}
	if got.Harness != "claude" {
		t.Fatalf("normalized harness = %q, want claude", got.Harness)
	}
	if got.QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("qualified id = %q, want anthropic:claude-opus-4-8", got.QualifiedID)
	}
}

func TestCuratedClaudeModelsIncludePinnedVersionsAndLatestAliases(t *testing.T) {
	def, ok := Lookup(Claude)
	if !ok {
		t.Fatalf("lookup %q", Claude)
	}
	models, err := def.AvailableModels()
	if err != nil {
		t.Fatalf("AvailableModels: %v", err)
	}

	wantIDs := []string{
		"claude-fable-5",
		"claude-haiku-4-5",
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"fable",
		"haiku",
		"opus",
		"sonnet",
	}
	var gotIDs []string
	byID := map[string]Model{}
	for _, model := range models {
		if model.Harness != Claude || model.ProviderID != "anthropic" {
			t.Fatalf("model %q owner = %q/%q, want claude/anthropic", model.ModelID, model.Harness, model.ProviderID)
		}
		if model.QualifiedID != "anthropic:"+model.ModelID {
			t.Fatalf("model %q qualified id = %q", model.ModelID, model.QualifiedID)
		}
		gotIDs = append(gotIDs, model.ModelID)
		byID[model.ModelID] = model
	}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("model ids = %v, want %v (sorted)", gotIDs, wantIDs)
	}

	wantEfforts := map[string][]string{
		"claude-opus-4-6":  {"low", "medium", "high", "max"},
		"claude-opus-4-8":  {"low", "medium", "high", "xhigh", "max"},
		"claude-haiku-4-5": nil,
		"opus":             {"low", "medium", "high", "xhigh", "max"},
		"haiku":            nil,
	}
	for id, want := range wantEfforts {
		model := byID[id]
		if len(want) == 0 {
			if model.Reasoning.Supported || len(model.Reasoning.Options) != 0 {
				t.Fatalf("%s reasoning = %#v, want unsupported", id, model.Reasoning)
			}
			continue
		}
		if !model.Reasoning.Supported || len(model.Reasoning.Options) != 1 {
			t.Fatalf("%s reasoning = %#v, want one effort option", id, model.Reasoning)
		}
		got := model.Reasoning.Options[0].Values
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s efforts = %v, want %v", id, got, want)
		}
	}
	for _, alias := range []string{"fable", "haiku", "opus", "sonnet"} {
		if !strings.Contains(byID[alias].ModelName, "(latest)") {
			t.Fatalf("alias %q name = %q, want latest label", alias, byID[alias].ModelName)
		}
	}
}

func TestAvailableCodexModelsUsesInstalledCatalog(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeExecutableScript(t, filepath.Join(toolDir, Codex), `#!/bin/sh
if [ "$*" = "debug models" ]; then
  printf '%s\n' '{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"max"},{"effort":"ultra"}]},{"slug":"hidden-review","display_name":"Hidden Review","visibility":"hide","supported_reasoning_levels":[{"effort":"high"}]}]}'
  exit 0
fi
exit 12
`)
	t.Setenv("PATH", toolDir)

	def, ok := Lookup(Codex)
	if !ok {
		t.Fatalf("lookup %q", Codex)
	}
	models, err := def.AvailableModels()
	if err != nil {
		t.Fatalf("AvailableModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one list-visible model", models)
	}
	model := models[0]
	if model.ModelID != "gpt-5.6-sol" || model.ModelName != "GPT-5.6-Sol" {
		t.Fatalf("model = %#v", model)
	}
	if model.Harness != Codex || model.ProviderID != "openai" || model.QualifiedID != "openai:gpt-5.6-sol" {
		t.Fatalf("model owner = %#v", model)
	}
	wantEfforts := []string{"low", "max", "ultra"}
	if got := model.Reasoning.Options[0].Values; strings.Join(got, ",") != strings.Join(wantEfforts, ",") {
		t.Fatalf("efforts = %v, want %v", got, wantEfforts)
	}
}

func TestAvailableCodexModelsFallsBackToBundledCatalog(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeExecutableScript(t, filepath.Join(toolDir, Codex), `#!/bin/sh
if [ "$*" = "debug models" ]; then
  printf '{'
  exit 0
fi
if [ "$*" = "debug models --bundled" ]; then
  printf '%s\n' '{"models":[{"slug":"gpt-bundled","display_name":"GPT Bundled","visibility":"list","supported_reasoning_levels":[]}]}'
  exit 0
fi
exit 12
`)
	t.Setenv("PATH", toolDir)

	models, err := AvailableCodexModels()
	if err != nil {
		t.Fatalf("AvailableCodexModels: %v", err)
	}
	if len(models) != 1 || models[0].ModelID != "gpt-bundled" {
		t.Fatalf("models = %#v, want bundled catalog", models)
	}
	if models[0].Reasoning.Supported {
		t.Fatalf("bundled model reasoning = %#v, want unsupported", models[0].Reasoning)
	}
}
