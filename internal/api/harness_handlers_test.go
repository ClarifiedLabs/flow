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

// TestLiveHarnessModelIntersectionAcrossWorkers verifies that the harness
// catalog is intersected across the workers that offer the harness, and keyed
// by harness name for attachment to the matching option.
func TestLiveHarnessModelIntersectionAcrossWorkers(t *testing.T) {
	t.Parallel()
	harnessLabel := flowharness.AgentHarnessLabel(flowharness.Harness)

	opus := harnessModel(flowharness.Harness, "anthropic", "claude-opus-4-8")
	sonnet := harnessModel(flowharness.Harness, "anthropic", "claude-sonnet-4-6")

	workers := []worker.Worker{
		{
			ID:            "w-1",
			Labels:        map[string]string{harnessLabel: "true"},
			HarnessModels: []flowharness.Model{opus, sonnet},
		},
		{
			// Only advertises opus, so sonnet drops out of the intersection
			// while opus survives.
			ID:            "w-2",
			Labels:        map[string]string{harnessLabel: "true"},
			HarnessModels: []flowharness.Model{opus},
		},
	}

	got := liveHarnessModelIntersection(workers)

	if len(got) != 1 {
		t.Fatalf("intersection = %+v, want only the harness entry", got)
	}
	models := got[flowharness.Harness]
	if len(models) != 1 || models[0].QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("harness intersection = %+v, want only opus", models)
	}
	if models[0].Harness != flowharness.Harness {
		t.Fatalf("harness model harness = %q", models[0].Harness)
	}
}

// TestLiveHarnessModelIntersectionIgnoresUnofferedHarness ensures a stray model
// stamped for a harness the worker does not offer is dropped.
func TestLiveHarnessModelIntersectionIgnoresUnofferedHarness(t *testing.T) {
	t.Parallel()
	workers := []worker.Worker{
		{
			ID:            "w-harness",
			Labels:        map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
			HarnessModels: []flowharness.Model{harnessModel("bogus", "openai", "gpt-5.5")},
		},
	}
	got := liveHarnessModelIntersection(workers)
	if len(got) != 0 {
		t.Fatalf("intersection = %+v, want empty (bogus-harness model on a harness-only worker)", got)
	}
}
