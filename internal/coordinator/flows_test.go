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

	direct, err := flows.GetByName(ctx, "direct")
	if err != nil {
		t.Fatalf("GetByName direct: %v", err)
	}
	if !direct.Default {
		t.Error("direct flow is not the default")
	}
	if len(direct.Phases) != 1 || direct.Phases[0].Name != "implement" || direct.Phases[0].Gate != FlowGateAuto {
		t.Errorf("direct phases = %+v, want single auto implement phase", direct.Phases)
	}
	if len(direct.ReviewAgents) != 2 {
		t.Errorf("direct review agents = %+v, want reviewer+verifier", direct.ReviewAgents)
	}

	planned, err := flows.GetByName(ctx, "planned")
	if err != nil {
		t.Fatalf("GetByName planned: %v", err)
	}
	if len(planned.Phases) != 2 {
		t.Fatalf("planned phases = %+v, want plan+implement", planned.Phases)
	}
	if planned.Phases[0].Name != "plan" || planned.Phases[0].Gate != FlowGateHuman {
		t.Errorf("planned first phase = %+v, want human-gated plan", planned.Phases[0])
	}
	if planned.Phases[1].Name != "implement" || planned.Phases[1].Gate != FlowGateAuto {
		t.Errorf("planned second phase = %+v, want auto implement", planned.Phases[1])
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

func TestFlowCRUDAndSnapshot(t *testing.T) {
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)

	planner, err := defs.Create(ctx, AgentDefInput{Name: "spec-writer", Harness: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Prompt: "Write a spec."})
	if err != nil {
		t.Fatalf("create planner def: %v", err)
	}
	builder, err := defs.Create(ctx, AgentDefInput{Name: "builder", Harness: "claude", Prompt: "Implement."})
	if err != nil {
		t.Fatalf("create builder def: %v", err)
	}
	reviewer, err := defs.Create(ctx, AgentDefInput{Name: "strict-reviewer", Harness: "claude", Model: "claude-opus-4-8", ReasoningEffort: "max", Prompt: "Review."})
	if err != nil {
		t.Fatalf("create reviewer def: %v", err)
	}

	if _, err := flows.Create(ctx, FlowInput{Name: "empty"}); err == nil {
		t.Fatal("expected error creating flow with no phases")
	}

	flow, err := flows.Create(ctx, FlowInput{
		Name: "feature",
		Phases: []FlowPhaseInput{
			{Name: "spec", AgentDefID: planner.ID, Gate: FlowGateHuman},
			{Name: "implement", AgentDefID: builder.ID},
		},
		ReviewAgents: []FlowReviewAgentInput{
			{Role: FlowReviewRoleReviewer, AgentDefID: reviewer.ID},
		},
	})
	if err != nil {
		t.Fatalf("Create flow: %v", err)
	}
	if flow.Phases[1].Gate != FlowGateAuto {
		t.Errorf("unset gate = %q, want default auto", flow.Phases[1].Gate)
	}

	// Referenced agent defs cannot be deleted.
	if err := defs.Delete(ctx, planner.ID); !errors.Is(err, ErrAgentDefInUse) {
		t.Fatalf("delete referenced def = %v, want ErrAgentDefInUse", err)
	}

	if err := flows.SetDefaultFlow(ctx, flow.ID); err != nil {
		t.Fatalf("SetDefaultFlow: %v", err)
	}
	if err := flows.Delete(ctx, flow.ID); !errors.Is(err, ErrFlowIsDefault) {
		t.Fatalf("delete default flow = %v, want ErrFlowIsDefault", err)
	}

	// Empty flow id resolves the project default.
	snapshot, err := flows.ResolveSnapshot(ctx, "")
	if err != nil {
		t.Fatalf("ResolveSnapshot: %v", err)
	}
	if snapshot.FlowName != "feature" || len(snapshot.Phases) != 2 {
		t.Fatalf("snapshot = %+v, want feature flow with 2 phases", snapshot)
	}
	if snapshot.Phases[0].Agent.Model != "gpt-5.5" || snapshot.Phases[0].Agent.Prompt != "Write a spec." {
		t.Errorf("snapshot phase agent = %+v, want frozen spec-writer", snapshot.Phases[0].Agent)
	}
	if len(snapshot.ReviewAgents) != 1 || !snapshot.ReviewAgents[0].Required {
		t.Errorf("snapshot review agents = %+v, want one required reviewer", snapshot.ReviewAgents)
	}

	// No fix agent declared: falls back to the last work phase's agent.
	fix, err := snapshot.FixAgentOrLastPhase()
	if err != nil {
		t.Fatalf("FixAgentOrLastPhase: %v", err)
	}
	if fix.Name != "builder" {
		t.Errorf("fix agent = %s, want builder", fix.Name)
	}

	// The snapshot is frozen: editing the live def afterwards must not leak in.
	if _, err := defs.Update(ctx, planner.ID, AgentDefInput{Name: "spec-writer", Harness: "codex", Model: "gpt-5.4", Prompt: "Changed."}); err != nil {
		t.Fatalf("update def: %v", err)
	}
	if snapshot.Phases[0].Agent.Model != "gpt-5.5" {
		t.Error("snapshot leaked a live agent def edit")
	}

	// Update replaces phases and review agents wholesale.
	updated, err := flows.Update(ctx, flow.ID, FlowInput{
		Name:          "feature",
		FixAgentDefID: reviewer.ID,
		Phases: []FlowPhaseInput{
			{Name: "implement", AgentDefID: builder.ID, Gate: FlowGateAuto},
		},
	})
	if err != nil {
		t.Fatalf("Update flow: %v", err)
	}
	if len(updated.Phases) != 1 || len(updated.ReviewAgents) != 0 {
		t.Fatalf("updated flow = %+v, want single phase and no review agents", updated)
	}

	resnap, err := flows.ResolveSnapshot(ctx, flow.ID)
	if err != nil {
		t.Fatalf("ResolveSnapshot after update: %v", err)
	}
	fix, err = resnap.FixAgentOrLastPhase()
	if err != nil {
		t.Fatalf("FixAgentOrLastPhase after update: %v", err)
	}
	if fix.Name != "strict-reviewer" {
		t.Errorf("declared fix agent = %s, want strict-reviewer", fix.Name)
	}
}
