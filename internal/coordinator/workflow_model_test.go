package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newWorkflowModelServices(t *testing.T) (*FlowService, *TaskService, *WorkflowRunService) {
	t.Helper()
	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open workflow model database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	flows := NewFlowService(store.DB())
	tasks := NewTaskService(store.DB(), "p-test")
	return flows, tasks, NewWorkflowRunService(store.DB(), flows, tasks)
}

func TestWorkflowModelHumanGateCanUseCustomOutcomeAndComplete(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)

	flow, err := flows.Create(ctx, FlowInput{
		Name:             "release decision",
		StartNode:        "decision",
		TransitionBudget: 5,
		Nodes: []FlowNodeInput{
			{Key: "decision", Name: "Release decision", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"ship_it", "stop"}}}},
			{Key: "shipped", Name: "Shipped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "stopped", Name: "Stopped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCancelled}}},
		},
		Edges: []FlowEdgeInput{
			{From: "decision", Outcome: "ship_it", To: "shipped"},
			{From: "decision", Outcome: "stop", To: "stopped"},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Release", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.State != nil {
		t.Fatalf("new task state = %v, want Unscheduled/null", task.State)
	}

	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	if run.State != WorkflowRunScheduled {
		t.Fatalf("scheduled run state = %q", run.State)
	}
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("enter human gate: %v", err)
	}
	if !created || nodeRun.State != WorkflowNodeWaiting {
		t.Fatalf("human gate node = %+v, created=%t", nodeRun, created)
	}
	state, wait, err := runs.Substate(ctx, task.ID)
	if err != nil {
		t.Fatalf("derive substate: %v", err)
	}
	if state != InProgressBlocked || wait == nil || wait.NodeRunID != nodeRun.ID {
		t.Fatalf("substate = %q wait=%+v, want Blocked on active node", state, wait)
	}

	result, err := runs.Respond(ctx, task.ID, nodeRun.ID, "ship_it", "release approved", ActorHuman)
	if err != nil {
		t.Fatalf("respond to human gate: %v", err)
	}
	if !result.Done || result.Run.State != WorkflowRunCompleted || result.Run.TransitionsUsed != 1 {
		t.Fatalf("completion result = %+v", result)
	}
	completed, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load completed task: %v", err)
	}
	if completed.State == nil || *completed.State != LifecycleDone || completed.DoneResolution == nil || *completed.DoneResolution != ResolutionCompleted {
		t.Fatalf("completed task = %+v", completed)
	}

	reopened, err := runs.Reopen(ctx, task.ID, ActorHuman)
	if err != nil {
		t.Fatalf("reopen task: %v", err)
	}
	if reopened.State != nil || reopened.DoneResolution != nil || reopened.DoneAt != nil {
		t.Fatalf("reopened task = %+v, want Unscheduled without resolution", reopened)
	}
}

func TestWorkflowModelSchedulingWaitsForTaskDependencies(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "finish",
		StartNode: "done",
		Nodes: []FlowNodeInput{
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	blocker, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Blocker", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Blocked", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	if err := tasks.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	run, err := runs.Schedule(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("schedule blocked task: %v", err)
	}
	if node, created, err := runs.EnsureCurrentNode(ctx, run.ID); err != nil || created || node.ID != "" {
		t.Fatalf("dependency-gated node = %+v created=%t err=%v", node, created, err)
	}
	if _, err := runs.ForceDone(ctx, blocker.ID, ResolutionCompleted, "dependency satisfied", ActorHuman); err != nil {
		t.Fatalf("complete blocker: %v", err)
	}
	if _, completed, err := runs.EnsureCurrentNode(ctx, run.ID); err != nil || !completed {
		t.Fatalf("resume after dependency: completed=%t err=%v", completed, err)
	}
}

func TestWorkflowModelAgentInputWaitResumesSameNodeVisit(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agent, err := NewAgentDefService(flows.db).Create(ctx, AgentDefInput{
		Name: "implementation agent", Harness: "codex", Prompt: "Implement the task.",
	})
	if err != nil {
		t.Fatalf("create agent definition: %v", err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "agent with questions",
		StartNode: "implement",
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "implement", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Needs a decision", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil || !created {
		t.Fatalf("create agent node: created=%t err=%v", created, err)
	}
	if _, err := runs.MarkNodeRunning(ctx, nodeRun.ID); err != nil {
		t.Fatalf("start agent node: %v", err)
	}
	if err := runs.RequestAgentInput(ctx, nodeRun.ID, "Which API shape should I use?", ActorAgent); err != nil {
		t.Fatalf("request agent input: %v", err)
	}
	state, wait, err := runs.Substate(ctx, task.ID)
	if err != nil {
		t.Fatalf("derive blocked state: %v", err)
	}
	if state != InProgressBlocked || wait == nil || wait.Kind != WorkflowWaitAgentRequest {
		t.Fatalf("blocked state = %q wait=%+v", state, wait)
	}
	resumed, err := runs.ResumeAgentRequest(ctx, task.ID, "Use the v2 shape.", ActorHuman)
	if err != nil || !resumed {
		t.Fatalf("resume agent input: resumed=%t err=%v", resumed, err)
	}
	resumedNode, found, err := runs.GetNodeRun(ctx, nodeRun.ID)
	if err != nil || !found {
		t.Fatalf("load resumed node: found=%t err=%v", found, err)
	}
	if resumedNode.State != WorkflowNodeRunning || resumedNode.Visit != nodeRun.Visit {
		t.Fatalf("resumed node = %+v, want same running visit", resumedNode)
	}
	state, wait, err = runs.Substate(ctx, task.ID)
	if err != nil || state != InProgressWorking || wait != nil {
		t.Fatalf("working state = %q wait=%+v err=%v", state, wait, err)
	}
}
