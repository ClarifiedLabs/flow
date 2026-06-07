package api

import (
	"testing"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func harnessModel(harness, provider, id string) flowharness.Model {
	return flowharness.Model{
		ProviderID:  provider,
		ModelID:     id,
		QualifiedID: provider + ":" + id,
		Harness:     harness,
	}
}

// TestLiveHarnessModelIntersectionPerHarness verifies that each harness's catalog
// is intersected independently across the workers that offer that harness, and
// keyed by harness name for attachment to the matching option.
func TestLiveHarnessModelIntersectionPerHarness(t *testing.T) {
	claudeLabel := flowharness.AgentHarnessLabel(flowharness.Claude)
	codexLabel := flowharness.AgentHarnessLabel(flowharness.Codex)

	opus := harnessModel(flowharness.Claude, "anthropic", "claude-opus-4-8")
	sonnet := harnessModel(flowharness.Claude, "anthropic", "claude-sonnet-4-6")
	gpt := harnessModel(flowharness.Codex, "openai", "gpt-5.5")

	workers := []worker.Worker{
		{
			ID:            "w-1",
			Labels:        map[string]string{claudeLabel: "true", codexLabel: "true"},
			HarnessModels: []flowharness.Model{opus, sonnet, gpt},
		},
		{
			// Offers claude and codex but only advertises opus + gpt, so sonnet
			// drops out of the claude intersection while opus and gpt survive.
			ID:            "w-2",
			Labels:        map[string]string{claudeLabel: "true", codexLabel: "true"},
			HarnessModels: []flowharness.Model{opus, gpt},
		},
	}

	got := liveHarnessModelIntersection(workers)

	claude := got[flowharness.Claude]
	if len(claude) != 1 || claude[0].QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("claude intersection = %+v, want only opus", claude)
	}
	if claude[0].Harness != flowharness.Claude {
		t.Fatalf("claude model harness = %q", claude[0].Harness)
	}
	codex := got[flowharness.Codex]
	if len(codex) != 1 || codex[0].QualifiedID != "openai:gpt-5.5" {
		t.Fatalf("codex intersection = %+v, want only gpt-5.5", codex)
	}
	if _, ok := got[flowharness.Harness]; ok {
		t.Fatalf("did not expect a harness entry: %+v", got)
	}
}

// TestLiveHarnessModelIntersectionIgnoresUnofferedHarness ensures a stray model
// stamped for a harness the worker does not offer is dropped.
func TestLiveHarnessModelIntersectionIgnoresUnofferedHarness(t *testing.T) {
	workers := []worker.Worker{
		{
			ID:            "w-claude-only",
			Labels:        map[string]string{flowharness.AgentHarnessLabel(flowharness.Claude): "true"},
			HarnessModels: []flowharness.Model{harnessModel(flowharness.Codex, "openai", "gpt-5.5")},
		},
	}
	got := liveHarnessModelIntersection(workers)
	if len(got) != 0 {
		t.Fatalf("intersection = %+v, want empty (codex model on a claude-only worker)", got)
	}
}
