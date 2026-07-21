package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newFlowTestServices(t *testing.T) (*FlowService, *AgentDefService) {
	t.Helper()
	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewFlowService(store.DB()), NewAgentDefService(store.DB())
}

func TestSeedDefaults(t *testing.T) {
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)

	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	allDefs, err := defs.List(ctx)
	if err != nil {
		t.Fatalf("List agent defs: %v", err)
	}
	if len(allDefs) != 4 {
		t.Fatalf("seeded agent defs = %d, want 4", len(allDefs))
	}
	for _, def := range allDefs {
		if !def.Builtin {
			t.Errorf("seeded agent def %s not marked builtin", def.Name)
		}
		if def.Prompt == "" {
			t.Errorf("seeded agent def %s has empty prompt", def.Name)
		}
	}

	allFlows, err := flows.List(ctx)
	if err != nil {
		t.Fatalf("List flows: %v", err)
	}
	if len(allFlows) != 2 {
		t.Fatalf("seeded flows = %d, want 2", len(allFlows))
	}

	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("GetByName coding: %v", err)
	}
	if !coding.Default {
		t.Error("coding flow is not the default")
	}
	if coding.StartNode != "implement" || len(coding.Nodes) != 8 {
		t.Errorf("coding graph = start %q, nodes %+v", coding.StartNode, coding.Nodes)
	}
	if coding.Nodes[0].Kind != NodeAgent || coding.Nodes[len(coding.Nodes)-1].Kind != NodeTerminal {
		t.Errorf("coding nodes = %+v", coding.Nodes)
	}

	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("GetByName planning: %v", err)
	}
	if planning.StartNode != "write-plan" || len(planning.Nodes) != 5 {
		t.Fatalf("planning graph = start %q, nodes %+v", planning.StartNode, planning.Nodes)
	}
	if planning.Nodes[1].Kind != NodeHumanGate || planning.Nodes[2].Kind != NodeMaterializeIssueSet {
		t.Errorf("planning nodes = %+v", planning.Nodes)
	}

	// Idempotent: a second seed pass changes nothing.
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("second SeedDefaults: %v", err)
	}
	allFlows, err = flows.List(ctx)
	if err != nil {
		t.Fatalf("List flows after reseed: %v", err)
	}
	if len(allFlows) != 2 {
		t.Fatalf("flows after reseed = %d, want 2", len(allFlows))
	}
}

func TestAgentDefCRUD(t *testing.T) {
	ctx := context.Background()
	_, defs := newFlowTestServices(t)

	created, err := defs.Create(ctx, AgentDefInput{
		Name:            "opus-reviewer",
		Harness:         "claude",
		Model:           "claude-opus-4-8",
		ReasoningEffort: "xhigh",
		Prompt:          "Review carefully.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Builtin {
		t.Error("user-created def marked builtin")
	}

	args, err := created.ModelSelectionArgs()
	if err != nil {
		t.Fatalf("ModelSelectionArgs: %v", err)
	}
	want := []string{"--model", "claude-opus-4-8", "--effort", "xhigh"}
	if len(args) != len(want) {
		t.Fatalf("model args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("model args = %v, want %v", args, want)
		}
	}

	if _, err := defs.Create(ctx, AgentDefInput{Name: "opus-reviewer", Harness: "codex"}); !errors.Is(err, ErrAgentDefNameTaken) {
		t.Fatalf("duplicate name error = %v, want ErrAgentDefNameTaken", err)
	}
	if _, err := defs.Create(ctx, AgentDefInput{Name: "bad", Harness: "shell"}); err == nil {
		t.Fatal("expected error for unsupported harness")
	}

	updated, err := defs.Update(ctx, created.ID, AgentDefInput{
		Name:    "opus-reviewer",
		Harness: "claude",
		Model:   "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Model != "claude-sonnet-4-6" || updated.ReasoningEffort != "" {
		t.Errorf("updated def = %+v, want sonnet model and cleared effort", updated)
	}

	byName, err := defs.GetByName(ctx, "opus-reviewer")
	if err != nil || byName.ID != created.ID {
		t.Fatalf("GetByName = %+v, %v", byName, err)
	}

	if err := defs.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := defs.Get(ctx, created.ID); !errors.Is(err, ErrAgentDefNotFound) {
		t.Fatalf("Get after delete = %v, want ErrAgentDefNotFound", err)
	}
}
