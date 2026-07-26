package coordinator

import (
	"context"
	"errors"
	"testing"
)

// newHoldFlow builds gate -> done, a graph small enough that every hold test
// can reach an interesting state in two moves.
func newHoldFlow(t *testing.T, flows *FlowService, tasks *TaskService, runs *WorkflowRunService) (Task, WorkflowRun, WorkflowNodeRun) {
	t.Helper()
	ctx := context.Background()
	flow, err := flows.Create(ctx, FlowInput{
		Name:             "hold fixture",
		StartNode:        "gate",
		TransitionBudget: 5,
		Nodes: []FlowNodeInput{
			{Key: "gate", Name: "Wait for approval", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved", "rejected"}}}},
			{Key: "done", Name: "Merge the change", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "dropped", Name: "Dropped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCancelled}}},
		},
		Edges: []FlowEdgeInput{
			{From: "gate", Outcome: "approved", To: "done"},
			{From: "gate", Outcome: "rejected", To: "dropped"},
		},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Held task", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule run: %v", err)
	}
	nodeRun, _, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("enter first node: %v", err)
	}
	return task, run, nodeRun
}

func TestHoldStopsTheExecutorAndComposesWithAnOpenWait(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, nodeRun := newHoldFlow(t, flows, tasks, runs)

	// The gate already opened a wait. A hold has to survive alongside it —
	// this is the case a workflow_waits row could not model, because
	// idx_workflow_waits_one_open allows only one open row per run.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait: %v", err)
	} else if !waiting {
		t.Fatal("fixture should be parked on a human gate")
	}

	held, err := runs.Hold(ctx, task.ID, ActorHuman)
	if err != nil {
		t.Fatalf("hold run: %v", err)
	}
	if !held.Held() {
		t.Fatal("run should report held")
	}
	if held.HeldBy != string(ActorHuman) {
		t.Fatalf("held_by = %q, want %q", held.HeldBy, ActorHuman)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait after hold: %v", err)
	} else if !waiting {
		t.Fatal("holding must not resolve the gate's wait")
	}

	// Holding twice is a no-op rather than a second transition, so a double
	// click cannot stack audit rows.
	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("re-hold run: %v", err)
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	holds := 0
	for _, transition := range transitions {
		if transition.EventKind == "workflow_hold_requested" {
			holds++
		}
	}
	if holds != 1 {
		t.Fatalf("hold transitions = %d, want 1", holds)
	}
	_ = nodeRun
}

func TestReleaseResumeClearsTheHoldWithoutMovingTheRun(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseResume, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release run: %v", err)
	}
	if result.Run.Held() {
		t.Fatal("released run should not report held")
	}
	if result.Run.CurrentNodeKey != "gate" {
		t.Fatalf("current node = %q, want gate — resume must not advance", result.Run.CurrentNodeKey)
	}
	if result.Done {
		t.Fatal("resume must not complete the run")
	}
}

func TestReleaseSatisfyTakesTheSuccessEdgeWithoutAnArtifact(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseSatisfy, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release satisfy: %v", err)
	}
	// "approved" is the gate's first configured outcome, so satisfy routes to
	// the done terminal rather than the dropped one.
	if !result.Done {
		t.Fatalf("run state = %q, want the run completed through the success edge", result.Run.State)
	}
	reloaded, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloaded.DoneResolution == nil || *reloaded.DoneResolution != ResolutionCompleted {
		t.Fatalf("resolution = %v, want completed", reloaded.DoneResolution)
	}
}

func TestReleaseMergeJumpsToTheTerminalNode(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseMerge, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release merge: %v", err)
	}
	if !result.Done {
		t.Fatalf("run state = %q, want completed", result.Run.State)
	}
	// The gate's wait must not outlive the jump, or the task stays blocked
	// forever on a node the run already left.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait: %v", err)
	} else if waiting {
		t.Fatal("jumping to the terminal should resolve the open wait")
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	var jumped bool
	for _, transition := range transitions {
		if transition.EventKind == "node_jumped" && transition.ToNodeKey == "done" {
			jumped = true
		}
	}
	if !jumped {
		t.Fatalf("transitions = %+v, want a node_jumped audit row", transitions)
	}
}

func TestReleaseWithoutAHoldIsRejected(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	_, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseResume, Actor: ActorHuman})
	if !errors.Is(err, ErrWorkflowNotHeld) {
		t.Fatalf("release err = %v, want ErrWorkflowNotHeld", err)
	}
}

func TestReleaseSubmitRequiresAnArtifact(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	if _, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseSubmit, Actor: ActorHuman}); err == nil {
		t.Fatal("submit without an artifact should fail")
	}
}

func TestCardStateReportsStepPositionDwellAndWait(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	state, ok, err := runs.CardState(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("card state: %v (ok=%t)", err, ok)
	}
	if state.StepCount != 3 {
		t.Fatalf("step count = %d, want 3", state.StepCount)
	}
	if state.StepIndex != 1 {
		t.Fatalf("step index = %d, want 1 (gate is the first node)", state.StepIndex)
	}
	if state.Wait == nil || state.Wait.Kind != WorkflowWaitHumanGate {
		t.Fatalf("wait = %+v, want the open human gate", state.Wait)
	}
	// An open wait is the most specific answer to "how long has it been
	// here", so dwell tracks the wait rather than the run.
	if !state.DwellSince.Equal(state.Wait.CreatedAt) {
		t.Fatalf("dwell = %v, want the wait's created_at %v", state.DwellSince, state.Wait.CreatedAt)
	}
	if state.Held {
		t.Fatal("card state should not report held before a hold")
	}

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	state, _, err = runs.CardState(ctx, task.ID)
	if err != nil {
		t.Fatalf("card state after hold: %v", err)
	}
	if !state.Held || state.HeldBy != string(ActorHuman) {
		t.Fatalf("card state = %+v, want held by human", state)
	}
}
