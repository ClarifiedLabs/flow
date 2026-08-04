package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
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

func TestForceDoneCleansTaskScopedConsoleWithoutWorkflowRun(t *testing.T) {
	ctx := context.Background()
	_, tasks, runs := newWorkflowModelServices(t)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Unscheduled task with console"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	workers := flowworker.NewService(runs.db)
	job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, Role: flowworker.RoleConsole, CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{"console_harness": "harness"},
	})
	if err != nil {
		t.Fatalf("enqueue task console: %v", err)
	}
	now := time.Now().UTC()
	if _, err := runs.db.ExecContext(ctx, `UPDATE jobs SET state = 'claimed', updated_at = ? WHERE id = ?`, formatTime(now), job.ID); err != nil {
		t.Fatalf("claim task console: %v", err)
	}
	const leaseID = "l-force-done-task-console"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-force-done', 'persistent_agent', ?, ?)`, leaseID, job.ID,
		formatTime(now), formatTime(now.Add(time.Minute))); err != nil {
		t.Fatalf("lease task console: %v", err)
	}
	if _, err := workers.MarkJobRunning(ctx, leaseID); err != nil {
		t.Fatalf("start task console: %v", err)
	}
	const sessionID = "s-force-done-task-console"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO sessions (
	id, task_id, job_id, lease_id, worker_id, role, workspace_mode, runtime_state,
	branch, base, harness, token_hash, created_at, updated_at
) VALUES (?, ?, ?, ?, 'w-force-done', 'console', 'change', 'working',
	'task/force-done', 'main', 'shell', 'force-done-token-hash', ?, ?)`,
		sessionID, task.ID, job.ID, leaseID, formatTime(now), formatTime(now)); err != nil {
		t.Fatalf("insert task console session: %v", err)
	}

	if _, err := runs.ForceDone(ctx, task.ID, ResolutionCancelled, "operator cancellation", ActorHuman); err != nil {
		t.Fatalf("force done task: %v", err)
	}
	canceledJob, err := workers.GetJob(ctx, job.ID)
	if err != nil || canceledJob.State != flowworker.JobCanceled {
		t.Fatalf("task console job after force done = %+v err=%v, want canceled", canceledJob, err)
	}
	releasedLease, err := workers.GetLease(ctx, leaseID)
	if err != nil || releasedLease.ReleasedAt == nil {
		t.Fatalf("task console lease after force done = %+v err=%v, want released", releasedLease, err)
	}
	var runtimeState string
	if err := runs.db.QueryRowContext(ctx, `SELECT runtime_state FROM sessions WHERE id = ?`, sessionID).Scan(&runtimeState); err != nil {
		t.Fatalf("load task console session: %v", err)
	}
	if runtimeState != string(SessionAbandoned) {
		t.Fatalf("task console session state after force done = %q, want abandoned", runtimeState)
	}
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
		Name: "implementation agent", Harness: "harness", Prompt: "Implement the task.",
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
