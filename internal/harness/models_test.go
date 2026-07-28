package harness

import (
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
	got, err := normalizeModel(Model{ProviderID: "anthropic", ModelID: "claude-opus-4-8", Harness: "HARNESS"})
	if err != nil {
		t.Fatalf("normalizeModel: %v", err)
	}
	if got.Harness != "harness" {
		t.Fatalf("normalized harness = %q, want harness", got.Harness)
	}
	if got.QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("qualified id = %q, want anthropic:claude-opus-4-8", got.QualifiedID)
	}
}
