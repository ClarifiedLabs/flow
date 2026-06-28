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

// TestCuratedCatalogsAvailableModelsStampHarness checks the curated claude/codex
// catalogs normalize cleanly and AvailableModels stamps each entry with the
// owning harness, with the right provider and effort reasoning options.
func TestCuratedCatalogsAvailableModelsStampHarness(t *testing.T) {
	cases := []struct {
		harness  string
		provider string
		wantIDs  []string
		efforts  map[string][]string
	}{
		{
			harness:  Claude,
			provider: "anthropic",
			wantIDs:  []string{"claude-fable-5", "claude-haiku-4-5", "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6"},
			efforts: map[string][]string{
				"claude-opus-4-8":  {"low", "medium", "high", "xhigh", "max"},
				"claude-haiku-4-5": {"low", "medium", "high"},
			},
		},
		{
			harness:  Codex,
			provider: "openai",
			wantIDs:  []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.5"},
			efforts: map[string][]string{
				"gpt-5.5": {"low", "medium", "high", "xhigh"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			def, ok := Lookup(tc.harness)
			if !ok {
				t.Fatalf("lookup %q", tc.harness)
			}
			models, err := def.AvailableModels()
			if err != nil {
				t.Fatalf("AvailableModels: %v", err)
			}
			var gotIDs []string
			byID := map[string]Model{}
			for _, m := range models {
				if m.Harness != tc.harness {
					t.Fatalf("model %q harness = %q, want %q", m.ModelID, m.Harness, tc.harness)
				}
				if m.ProviderID != tc.provider {
					t.Fatalf("model %q provider = %q, want %q", m.ModelID, m.ProviderID, tc.provider)
				}
				if m.QualifiedID != tc.provider+":"+m.ModelID {
					t.Fatalf("model %q qualified id = %q", m.ModelID, m.QualifiedID)
				}
				if !m.Reasoning.Supported || len(m.Reasoning.Options) != 1 || m.Reasoning.Options[0].Type != "effort" {
					t.Fatalf("model %q reasoning = %#v, want one effort option", m.ModelID, m.Reasoning)
				}
				gotIDs = append(gotIDs, m.ModelID)
				byID[m.ModelID] = m
			}
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("model ids = %v, want %v (sorted)", gotIDs, tc.wantIDs)
			}
			for id, wantEfforts := range tc.efforts {
				got := byID[id].Reasoning.Options[0].Values
				if strings.Join(got, ",") != strings.Join(wantEfforts, ",") {
					t.Fatalf("%s efforts = %v, want %v", id, got, wantEfforts)
				}
			}
		})
	}
}
