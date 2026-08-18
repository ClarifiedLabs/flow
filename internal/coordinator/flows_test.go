package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newFlowTestServices(t *testing.T) (*FlowService, *AgentDefService) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(root, "global.db"))
	if err != nil {
		t.Fatalf("open global test database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}

	projectStore, err := flowdb.Open(ctx, filepath.Join(root, "project.db"))
	if err != nil {
		t.Fatalf("open project test database: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })
	defs := NewInheritedAgentDefService(projectStore.DB(), globals)
	return NewFlowServiceWithAgentDefs(projectStore.DB(), defs), defs
}

func TestFlowUpdateIncrementsPersistedRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, _ := newFlowTestServices(t)
	input := FlowInput{
		Name:      "revisioned flow",
		StartNode: "done",
		Nodes: []FlowNodeInput{
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
	}
	created, err := flows.Create(ctx, input)
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	if created.Revision != 1 {
		t.Fatalf("created revision = %d, want 1", created.Revision)
	}
	initialSnapshot, err := flows.ResolveSnapshot(ctx, created.ID)
	if err != nil {
		t.Fatalf("resolve initial snapshot: %v", err)
	}
	if initialSnapshot.FlowRevision != created.Revision {
		t.Fatalf("initial snapshot revision = %d, want %d", initialSnapshot.FlowRevision, created.Revision)
	}

	input.Description = "updated definition"
	updated, err := flows.Update(ctx, created.ID, input)
	if err != nil {
		t.Fatalf("update flow: %v", err)
	}
	if updated.Revision != created.Revision+1 {
		t.Fatalf("updated revision = %d, want %d", updated.Revision, created.Revision+1)
	}
	reloaded, err := flows.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("reload flow: %v", err)
	}
	if reloaded.Revision != updated.Revision {
		t.Fatalf("persisted revision = %d, want %d", reloaded.Revision, updated.Revision)
	}
}

func TestFlowCreateRejectsConflictingTaskSetMaterializerPolicies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	planner, err := defs.GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("GetByName task-planner: %v", err)
	}
	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("GetByName coding: %v", err)
	}
	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("GetByName planning: %v", err)
	}

	_, err = flows.Create(ctx, FlowInput{
		Name:      "conflicting-materializers",
		StartNode: "plan",
		Nodes: []FlowNodeInput{
			{Key: "plan", Name: "Plan", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: planner.ID, Workspace: WorkspaceBase, Artifact: ArtifactTaskSet}}},
			{Key: "select", Name: "Select", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Instructions: "Select a policy", Outcomes: []string{"coding", "planning"}}}},
			{Key: "materialize-coding", Name: "Materialize coding", Kind: NodeMaterializeTaskSet, Config: FlowNodeConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: coding.ID, AllowChildFlowOverride: true}}},
			{Key: "materialize-planning", Name: "Materialize planning", Kind: NodeMaterializeTaskSet, Config: FlowNodeConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: planning.ID, AllowChildFlowOverride: true}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{
			{From: "plan", Outcome: "completed", To: "select"},
			{From: "select", Outcome: "coding", To: "materialize-coding"},
			{From: "select", Outcome: "planning", To: "materialize-planning"},
			{From: "materialize-coding", Outcome: "completed", To: "done"},
			{From: "materialize-planning", Outcome: "completed", To: "done"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting policies") {
		t.Fatalf("Create branched conflicting materializers error = %v, want conflicting policies", err)
	}

	_, err = flows.Create(ctx, FlowInput{
		Name:      "sequential-conflicting-materializers",
		StartNode: "plan",
		Nodes: []FlowNodeInput{
			{Key: "plan", Name: "Plan", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: planner.ID, Workspace: WorkspaceBase, Artifact: ArtifactTaskSet}}},
			{Key: "materialize-coding", Name: "Materialize coding", Kind: NodeMaterializeTaskSet, Config: FlowNodeConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: coding.ID, AllowChildFlowOverride: true}}},
			{Key: "materialize-planning", Name: "Materialize planning", Kind: NodeMaterializeTaskSet, Config: FlowNodeConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: planning.ID, AllowChildFlowOverride: true}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{
			{From: "plan", Outcome: "completed", To: "materialize-coding"},
			{From: "materialize-coding", Outcome: "completed", To: "materialize-planning"},
			{From: "materialize-planning", Outcome: "completed", To: "done"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting policies") {
		t.Fatalf("Create sequential conflicting materializers error = %v, want conflicting policies", err)
	}
}

func TestFlowDeleteRejectsActiveSnapshotReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, _ := newFlowTestServices(t)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("GetByName coding: %v", err)
	}
	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("GetByName planning: %v", err)
	}
	tasks := NewTaskService(flows.db, "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Active planning task", FlowID: planning.ID})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	runs := NewWorkflowRunService(flows.db, flows, tasks)
	if _, err := runs.Schedule(ctx, task.ID); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := flows.db.ExecContext(ctx, `UPDATE tasks SET flow_id = ? WHERE id = ?`, coding.ID, task.ID); err != nil {
		t.Fatalf("reassign task flow: %v", err)
	}
	if err := flows.Delete(ctx, planning.ID); !errors.Is(err, ErrFlowInUse) {
		t.Fatalf("Delete active snapshot flow error = %v, want ErrFlowInUse", err)
	}
}

func TestSeedDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)

	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	allDefs, err := defs.List(ctx)
	if err != nil {
		t.Fatalf("List agent defs: %v", err)
	}
	if len(allDefs) != 9 {
		t.Fatalf("inherited default agent defs = %d, want 9", len(allDefs))
	}
	defsByName := make(map[string]AgentDef, len(allDefs))
	for _, def := range allDefs {
		defsByName[def.Name] = def
		if !def.Builtin || !def.Inherited {
			t.Errorf("default agent def %s = %+v, want inherited global built-in", def.Name, def)
		}
		if def.Prompt == "" {
			t.Errorf("default agent def %s has empty prompt", def.Name)
		}
	}
	var localDefCount int
	if err := defs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_defs`).Scan(&localDefCount); err != nil {
		t.Fatalf("count project agent definitions: %v", err)
	}
	if localDefCount != 0 {
		t.Fatalf("project agent definitions = %d, want none", localDefCount)
	}

	allFlows, err := flows.List(ctx)
	if err != nil {
		t.Fatalf("List flows: %v", err)
	}
	if len(allFlows) != 4 {
		t.Fatalf("seeded flows = %d, want 4", len(allFlows))
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
	if gate := coding.Nodes[4].Config.HumanGate; gate == nil || !gate.TaskOptIn || gate.SkipOutcome != "approved" {
		t.Errorf("coding human-review gate = %+v, want task opt-in with approved bypass", gate)
	}
	codeReviewer, hasCodeReviewer := defsByName["code-reviewer"]
	securityReviewer, hasSecurityReviewer := defsByName["security-reviewer"]
	reviewAggregator, hasReviewAggregator := defsByName["review-aggregator"]
	if !hasCodeReviewer || !hasSecurityReviewer || !hasReviewAggregator ||
		!strings.Contains(strings.ToLower(securityReviewer.Prompt), "security focus") ||
		!strings.Contains(strings.ToLower(reviewAggregator.Prompt), "aggregation focus") {
		t.Fatalf("seeded review agents = code:%+v security:%+v aggregator:%+v", codeReviewer, securityReviewer, reviewAggregator)
	}
	reviewConfig := coding.Nodes[2].Config.ChangeReview
	if reviewConfig.AggregatorAgentDefID != reviewAggregator.ID {
		t.Fatalf("default review aggregator = %q, want %q", reviewConfig.AggregatorAgentDefID, reviewAggregator.ID)
	}
	if !flowNodeConfigUsesAgent(coding.Nodes[2].Config, reviewAggregator.ID) {
		t.Fatal("review aggregator is not tracked as a flow agent-definition reference")
	}
	reviewAgents := reviewConfig.Agents
	if len(reviewAgents) != 2 || reviewAgents[0].AgentDefID != codeReviewer.ID || reviewAgents[1].AgentDefID != securityReviewer.ID {
		t.Fatalf("default review agents = %+v", reviewAgents)
	}
	for _, agent := range reviewAgents {
		if agent.Blocking == nil || !*agent.Blocking {
			t.Errorf("default reviewer is not blocking: %+v", agent)
		}
	}

	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("GetByName planning: %v", err)
	}
	if planning.StartNode != "write-plan" || len(planning.Nodes) != 5 {
		t.Fatalf("planning graph = start %q, nodes %+v", planning.StartNode, planning.Nodes)
	}
	if planning.Nodes[1].Kind != NodeHumanGate || planning.Nodes[2].Kind != NodeMaterializeTaskSet {
		t.Errorf("planning nodes = %+v", planning.Nodes)
	}
	organizer, err := flows.GetByName(ctx, ReviewFollowUpOrganizerFlowName)
	if err != nil {
		t.Fatalf("GetByName organizer: %v", err)
	}
	if organizer.StartNode != "organize" || len(organizer.Nodes) != 5 ||
		organizer.Nodes[1].Kind != NodeHumanGate || organizer.Nodes[2].Kind != NodeMaterializeTaskSet {
		t.Fatalf("organizer graph = start %q, nodes %+v", organizer.StartNode, organizer.Nodes)
	}
	organizerDef := defsByName["review-follow-up-organizer"]
	if organizer.Nodes[0].Config.Agent == nil || organizer.Nodes[0].Config.Agent.AgentDefID != organizerDef.ID ||
		!strings.Contains(organizerDef.Prompt, "Required accounting") {
		t.Fatalf("organizer agent/definition = %+v / %+v", organizer.Nodes[0].Config.Agent, organizerDef)
	}

	// Idempotent: a second seed pass changes nothing.
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("second SeedDefaults: %v", err)
	}
	allFlows, err = flows.List(ctx)
	if err != nil {
		t.Fatalf("List flows after reseed: %v", err)
	}
	if len(allFlows) != 4 {
		t.Fatalf("flows after reseed = %d, want 4", len(allFlows))
	}
}

func TestTaskSetWorkflowContractAdvertisesCodingAndPlanning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, _ := newFlowTestServices(t)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("get planning flow: %v", err)
	}
	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("get coding flow: %v", err)
	}
	snapshot, err := flows.ResolveSnapshot(ctx, planning.ID)
	if err != nil {
		t.Fatalf("resolve planning snapshot: %v", err)
	}
	contract, found, err := flows.TaskSetWorkflowContractForNode(ctx, snapshot, "write-plan")
	if err != nil {
		t.Fatalf("resolve task-set workflow contract: %v", err)
	}
	if !found {
		t.Fatal("task-set workflow contract not found")
	}
	if contract.DefaultChildFlowID != coding.ID || !contract.AllowChildFlowOverride || contract.MaxItems != 25 {
		t.Fatalf("task-set workflow contract = %+v", contract)
	}
	options := map[string]TaskSetFlowOption{}
	for _, option := range contract.AvailableFlows {
		options[option.Name] = option
	}
	if options["coding"].ID != coding.ID || options["planning"].ID != planning.ID {
		t.Fatalf("task-set workflow options = %+v", contract.AvailableFlows)
	}
	if !strings.Contains(options["planning"].Description, "narrower planning") {
		t.Fatalf("planning workflow description = %q", options["planning"].Description)
	}
}

func TestParallelReviewGraphUsesCanonicalBlockingAndFreezesSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)

	author, err := defs.Create(ctx, AgentDefInput{Name: "author-test", Harness: "harness", Prompt: "Implement the task."})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	codeReview, err := defs.Create(ctx, AgentDefInput{
		Name: "code-review", Harness: "harness", Model: "openai:gpt-5", ReasoningEffort: "high", Prompt: "Review correctness.",
	})
	if err != nil {
		t.Fatalf("create code reviewer: %v", err)
	}
	securityReview, err := defs.Create(ctx, AgentDefInput{
		Name: "security-review", Harness: "harness", Model: "anthropic:claude-sonnet-4-6", Prompt: "Review security.",
	})
	if err != nil {
		t.Fatalf("create security reviewer: %v", err)
	}
	reviewAggregator, err := defs.Create(ctx, AgentDefInput{
		Name: "review-aggregator-test", Harness: "harness", Model: "openai:gpt-5-mini", ReasoningEffort: "medium", Prompt: "Synthesize review reports.",
	})
	if err != nil {
		t.Fatalf("create review aggregator: %v", err)
	}
	advisory := false
	created, err := flows.Create(ctx, FlowInput{
		Name: "parallel-review", StartNode: "implement",
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: author.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "review", Name: "Review", Kind: NodeChangeReview, Config: FlowNodeConfig{ChangeReview: &ChangeReviewNodeConfig{Agents: []ReviewAgentConfig{
				{AgentDefID: codeReview.ID},
				{AgentDefID: securityReview.ID, Blocking: &advisory},
			}, AggregatorAgentDefID: reviewAggregator.ID}}},
			{Key: "verify", Name: "Verify", Kind: NodeVerifyChange, Config: FlowNodeConfig{VerifyChange: &VerifyChangeNodeConfig{Agents: []ReviewAgentConfig{{AgentDefID: securityReview.ID}}}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{
			{From: "implement", Outcome: "completed", To: "review"},
			{From: "review", Outcome: "approved", To: "verify"},
			{From: "review", Outcome: "changes_requested", To: "implement"},
			{From: "verify", Outcome: "passed", To: "done"},
			{From: "verify", Outcome: "changes_requested", To: "implement"},
		},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}

	if got := created.Nodes[1].Config.ChangeReview.Agents; len(got) != 2 || got[0].AgentDefID != codeReview.ID || got[1].AgentDefID != securityReview.ID {
		t.Fatalf("review agent order = %+v", got)
	} else if got[0].Blocking == nil || !*got[0].Blocking || got[1].Blocking == nil || *got[1].Blocking {
		t.Fatalf("review blocking values = %+v, want true then false", got)
	}
	if got := created.Nodes[1].Config.ChangeReview.AggregatorAgentDefID; got != reviewAggregator.ID {
		t.Fatalf("review aggregator = %q, want %q", got, reviewAggregator.ID)
	}
	verifyAgents := created.Nodes[2].Config.VerifyChange.Agents
	if len(verifyAgents) != 1 || verifyAgents[0].Blocking == nil || !*verifyAgents[0].Blocking {
		t.Fatalf("verify agents = %+v, want default blocking", verifyAgents)
	}
	encodedFlow, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal flow: %v", err)
	}
	if !strings.Contains(string(encodedFlow), `"blocking":true`) || !strings.Contains(string(encodedFlow), `"blocking":false`) || strings.Contains(string(encodedFlow), `"required"`) {
		t.Fatalf("canonical flow JSON = %s", encodedFlow)
	}

	snapshot, err := flows.ResolveSnapshot(ctx, created.ID)
	if err != nil {
		t.Fatalf("resolve snapshot: %v", err)
	}
	reviewNode, ok := snapshot.Node("review")
	if !ok || reviewNode.Config.ChangeReview == nil || len(reviewNode.Config.ChangeReview.Agents) != 2 {
		t.Fatalf("review snapshot node = %+v ok=%v", reviewNode, ok)
	}
	frozen := reviewNode.Config.ChangeReview.Agents
	if !frozen[0].Blocking || frozen[1].Blocking || frozen[0].Agent.Model != "openai:gpt-5" || frozen[0].Agent.ReasoningEffort != "high" || frozen[0].Agent.Prompt != "Review correctness." || frozen[1].Agent.Prompt != "Review security." {
		t.Fatalf("frozen review agents = %+v", frozen)
	}
	frozenAggregator := reviewNode.Config.ChangeReview.Aggregator
	if frozenAggregator.ID != reviewAggregator.ID || frozenAggregator.Model != "openai:gpt-5-mini" || frozenAggregator.ReasoningEffort != "medium" || frozenAggregator.Prompt != "Synthesize review reports." {
		t.Fatalf("frozen review aggregator = %+v", frozenAggregator)
	}
	verifyNode, ok := snapshot.Node("verify")
	if !ok || verifyNode.Config.VerifyChange == nil || len(verifyNode.Config.VerifyChange.Agents) != 1 || !verifyNode.Config.VerifyChange.Agents[0].Blocking {
		t.Fatalf("verify snapshot node = %+v ok=%v", verifyNode, ok)
	}

	if _, err := defs.Update(ctx, codeReview.ID, AgentDefInput{Name: "code-review", Harness: "harness", Model: "openai:gpt-5-mini", Prompt: "Changed live prompt."}); err != nil {
		t.Fatalf("update live reviewer: %v", err)
	}
	if _, err := defs.Update(ctx, reviewAggregator.ID, AgentDefInput{Name: "review-aggregator-test", Harness: "harness", Model: "openai:gpt-5", Prompt: "Changed aggregator prompt."}); err != nil {
		t.Fatalf("update live aggregator: %v", err)
	}
	if frozen[0].Agent.Model != "openai:gpt-5" || frozen[0].Agent.Prompt != "Review correctness." || !frozen[0].Blocking || frozen[1].Blocking {
		t.Fatalf("snapshot changed after live edit: %+v", frozen)
	}
	if frozenAggregator.Model != "openai:gpt-5-mini" || frozenAggregator.Prompt != "Synthesize review reports." {
		t.Fatalf("aggregator snapshot changed after live edit: %+v", frozenAggregator)
	}
}

func TestReviewAgentUsesBlockingOnlyAndRejectsRequired(t *testing.T) {
	t.Parallel()
	// blocking is the only accepted spelling; an omitted blocking defaults to
	// blocking, and false is advisory.
	cfg, err := decodeNodeConfig(`{"change_review":{"agents":[{"agent_def_id":"ad-code"},{"agent_def_id":"ad-security","blocking":false}]}}`)
	if err != nil {
		t.Fatalf("decode review config: %v", err)
	}
	agents := cfg.ChangeReview.Agents
	if len(agents) != 2 || agents[0].Blocking == nil || !*agents[0].Blocking || agents[1].Blocking == nil || *agents[1].Blocking {
		t.Fatalf("review agents = %+v", agents)
	}
	canonical, err := encodeNodeConfig(cfg)
	if err != nil {
		t.Fatalf("encode canonical review config: %v", err)
	}
	if strings.Contains(canonical, `"required"`) || !strings.Contains(canonical, `"blocking":false`) {
		t.Fatalf("canonical review config = %s", canonical)
	}

	// The removed `required` alias is rejected as an unknown field, not
	// silently accepted.
	if _, err := decodeNodeConfig(`{"change_review":{"agents":[{"agent_def_id":"ad-code","required":true}]}}`); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("required-alias error = %v, want rejection of unknown field", err)
	}
	if _, err := decodeNodeConfig(`{"verify_change":{"agents":[{"agent_def_id":"ad-verifier","required":false}]}}`); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("verify required-alias error = %v, want rejection", err)
	}
	for _, raw := range []string{
		`{"change_review":{"agents":[{"agent_def_id":"ad-code","blocking":null}],"aggregator_agent_def_id":"ad-aggregate"}}`,
		`{"verify_change":{"agents":[{"agent_def_id":"ad-verifier","blocking":null}]}}`,
	} {
		if _, err := decodeNodeConfig(raw); err == nil || !strings.Contains(err.Error(), "blocking") {
			t.Fatalf("null blocking config %s error = %v, want rejection", raw, err)
		}
	}

	if _, err := normalizeReviewAgents("review", nil); err == nil || !strings.Contains(err.Error(), "at least one agent") {
		t.Fatalf("empty group error = %v", err)
	}
	if _, err := normalizeReviewAgents("review", []ReviewAgentConfig{{AgentDefID: "ad-code"}, {AgentDefID: "ad-code"}}); err == nil || !strings.Contains(err.Error(), "repeats agent definition") {
		t.Fatalf("duplicate group error = %v", err)
	}
	if _, _, err := normalizeNodeConfig("review", NodeChangeReview, FlowNodeConfig{ChangeReview: &ChangeReviewNodeConfig{
		Agents: []ReviewAgentConfig{{AgentDefID: "ad-code"}},
	}}); err == nil || !strings.Contains(err.Error(), "requires an aggregator agent definition") {
		t.Fatalf("missing aggregator error = %v", err)
	}

	// Snapshots serialize an explicit Boolean blocking and round-trip it.
	snapshotAgent := SnapshotReviewAgent{Blocking: false, Agent: AgentDefSnapshot{Name: "advisory", Harness: "harness"}}
	snapshotJSON, err := json.Marshal(snapshotAgent)
	if err != nil {
		t.Fatalf("marshal snapshot agent: %v", err)
	}
	if strings.Contains(string(snapshotJSON), `"required"`) || !strings.Contains(string(snapshotJSON), `"blocking":false`) {
		t.Fatalf("canonical snapshot JSON = %s", snapshotJSON)
	}
	var roundTrip SnapshotReviewAgent
	if err := json.Unmarshal(snapshotJSON, &roundTrip); err != nil || roundTrip.Blocking {
		t.Fatalf("snapshot round-trip = %+v err=%v", roundTrip, err)
	}
	// Persisted snapshots do not inherit the live graph's omitted-blocking
	// default. Legacy required-only snapshots must fail closed rather than
	// silently turning a reviewer advisory.
	for _, raw := range []string{
		`{"agent":{"name":"advisory","harness":"harness"},"required":true}`,
		`{"agent":{"name":"advisory","harness":"harness"}}`,
		`{"agent":{"name":"advisory","harness":"harness"},"blocking":null}`,
	} {
		var legacy SnapshotReviewAgent
		if err := json.Unmarshal([]byte(raw), &legacy); err == nil {
			t.Fatalf("legacy snapshot %s decoded as %+v, want strict rejection", raw, legacy)
		}
	}
}

func TestDecodeFlowSnapshotRejectsLegacyFieldsUnknownNestedFieldsAndInvalidGraph(t *testing.T) {
	t.Parallel()
	valid := `{"flow_id":"fl-current","flow_name":"current graph","start_node":"done","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[]}`
	if _, err := decodeFlowSnapshot([]byte(valid)); err != nil {
		t.Fatalf("decode valid current snapshot: %v", err)
	}
	// Frozen agent payloads are self-contained: decoding a persisted run must
	// not consult the current agent catalog (the id deliberately need not exist).
	frozenAgent := `{"flow_id":"fl-frozen","flow_name":"frozen graph","start_node":"author","transition_budget":5,"nodes":[{"key":"author","name":"Author","kind":"agent","config":{"agent":{"agent":{"id":"ad-deleted","name":"Deleted live agent","harness":"harness","prompt":"Frozen prompt"},"workspace":"base","artifact":"handoff"}}},{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[{"from":"author","outcome":"completed","to":"done"}]}`
	decoded, err := decodeFlowSnapshot([]byte(frozenAgent))
	if err != nil || decoded.Nodes[0].Config.Agent == nil || decoded.Nodes[0].Config.Agent.Agent.ID != "ad-deleted" {
		t.Fatalf("decode frozen agent snapshot = %+v err=%v, want self-contained frozen agent", decoded, err)
	}
	cases := []struct {
		name string
		raw  string
	}{
		{name: "retired ordered phases", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":"done","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[],"phases":[]}`},
		{name: "retired top-level review agents", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":"done","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[],"review_agents":[]}`},
		{name: "retired fix agent", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":"done","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[],"fix_agent_def_id":"ad-fix"}`},
		{name: "unknown nested node config", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":"done","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed","legacy":true}}}],"edges":[]}`},
		{name: "noncanonical graph value", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":" done ","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[]}`},
		{name: "missing flow identity", raw: strings.Replace(valid, `"flow_id":"fl-current"`, `"flow_id":""`, 1)},
		{name: "missing frozen agent id", raw: strings.Replace(frozenAgent, `"id":"ad-deleted",`, "", 1)},
		{name: "noncanonical frozen agent", raw: strings.Replace(frozenAgent, `"harness":"harness"`, `"harness":" harness "`, 1)},
		{name: "multiple JSON values", raw: valid + `{}`},
		{name: "invalid graph shape", raw: `{"flow_id":"fl-current","flow_name":"current graph","start_node":"missing","transition_budget":1,"nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"edges":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeFlowSnapshot([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeFlowSnapshot(%s) succeeded, want rejection", tc.raw)
			}
		})
	}
}

func TestReviewAggregationBlocksApprovalWhenAnySourceIsBlocking(t *testing.T) {
	t.Parallel()
	agents := []SnapshotReviewAgent{
		{Blocking: false, Agent: AgentDefSnapshot{Name: "advisory"}},
		{Blocking: true, Agent: AgentDefSnapshot{Name: "blocker"}},
	}
	if !ReviewAggregationBlocksApproval(agents) {
		t.Fatal("blocking discovery source did not make aggregation blocking")
	}
	if ReviewAggregationBlocksApproval(agents[:1]) {
		t.Fatal("all-advisory discovery sources made aggregation blocking")
	}
	if ReviewAggregationBlocksApproval(nil) {
		t.Fatal("empty discovery sources made aggregation blocking")
	}
}

func TestAgentDefCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, defs := newFlowTestServices(t)

	created, err := defs.Create(ctx, AgentDefInput{
		Name:            "opus-reviewer",
		Harness:         "harness",
		Model:           "anthropic:claude-opus-4-8",
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
	want := []string{"--model", "anthropic:claude-opus-4-8", "--reasoning", "xhigh"}
	if len(args) != len(want) {
		t.Fatalf("model args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("model args = %v, want %v", args, want)
		}
	}

	if _, err := defs.Create(ctx, AgentDefInput{Name: "opus-reviewer", Harness: "harness"}); !errors.Is(err, ErrAgentDefNameTaken) {
		t.Fatalf("duplicate name error = %v, want ErrAgentDefNameTaken", err)
	}
	if _, err := defs.Create(ctx, AgentDefInput{Name: "bad", Harness: "shell"}); err == nil {
		t.Fatal("expected error for unsupported harness")
	}

	updated, err := defs.Update(ctx, created.ID, AgentDefInput{
		Name:    "opus-reviewer",
		Harness: "harness",
		Model:   "anthropic:claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Model != "anthropic:claude-sonnet-4-6" || updated.ReasoningEffort != "" {
		t.Errorf("updated def = %+v, want sonnet model and cleared effort", updated)
	}

	byName, err := defs.GetByName(ctx, "opus-reviewer")
	if err != nil || byName.ID != created.ID {
		t.Fatalf("GetByName = %+v, %v", byName, err)
	}

	referencingFlow, err := flows.Create(ctx, FlowInput{
		Name: "agent-def-delete-reference", StartNode: "review",
		Nodes: []FlowNodeInput{
			{Key: "review", Name: "Review", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: created.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "review", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create referencing flow: %v", err)
	}
	if err := defs.Delete(ctx, created.ID); !errors.Is(err, ErrAgentDefInUse) {
		t.Fatalf("Delete referenced definition = %v, want ErrAgentDefInUse", err)
	}
	if err := flows.Delete(ctx, referencingFlow.ID); err != nil {
		t.Fatalf("delete referencing flow: %v", err)
	}
	if err := defs.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := defs.Delete(ctx, created.ID); !errors.Is(err, ErrAgentDefNotFound) {
		t.Fatalf("second Delete = %v, want ErrAgentDefNotFound", err)
	}
	if _, err := defs.Get(ctx, created.ID); !errors.Is(err, ErrAgentDefNotFound) {
		t.Fatalf("Get after delete = %v, want ErrAgentDefNotFound", err)
	}
}
