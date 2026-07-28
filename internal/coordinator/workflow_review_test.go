package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// reviewFlowFixture builds the planning shape: an agent node that produces a
// task_set, a human review gate, and the three classic outcomes.
func reviewFlowFixture(t *testing.T, ctx context.Context, flows *FlowService) Flow {
	t.Helper()
	agent, err := NewAgentDefService(flows.db).Create(ctx, AgentDefInput{
		Name: "planner", Harness: "harness", Prompt: "Plan the task.",
	})
	if err != nil {
		t.Fatalf("create agent definition: %v", err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "planning",
		StartNode: "plan",
		Nodes: []FlowNodeInput{
			{Key: "plan", Name: "Write task plan", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceBase, Artifact: ArtifactTaskSet}}},
			{Key: "review", Name: "Review plan", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Instructions: "Review the proposed implementation tasks.", Outcomes: []string{"approved", "changes_requested", "rejected"}}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionRejected}}},
		},
		Edges: []FlowEdgeInput{
			{From: "plan", Outcome: "completed", To: "review"},
			{From: "review", Outcome: "approved", To: "done"},
			{From: "review", Outcome: "changes_requested", To: "plan"},
			{From: "review", Outcome: "rejected", To: "rejected"},
		},
	})
	if err != nil {
		t.Fatalf("create review flow: %v", err)
	}
	return flow
}

func startPlanNode(t *testing.T, ctx context.Context, runs *WorkflowRunService, runID string) WorkflowNodeRun {
	t.Helper()
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, runID)
	if err != nil || !created {
		t.Fatalf("create plan node: created=%t err=%v", created, err)
	}
	started, err := runs.MarkNodeRunning(ctx, nodeRun.ID)
	if err != nil {
		t.Fatalf("start plan node: %v", err)
	}
	return started
}

func createPlanArtifact(t *testing.T, ctx context.Context, artifacts *WorkflowArtifactService, run WorkflowRun, nodeRun WorkflowNodeRun, sessionID, clientKey, title string) WorkflowArtifact {
	t.Helper()
	payload, err := json.Marshal(TaskSetManifest{
		SchemaVersion: 1,
		Tasks:         []TaskSetItem{{Key: "one", Title: title, Body: "do the thing"}},
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	artifact, _, err := artifacts.Create(ctx, CreateWorkflowArtifactInput{
		WorkflowRunID:   run.ID,
		NodeRunID:       nodeRun.ID,
		SessionID:       sessionID,
		CreatorKey:      "test",
		Kind:            ArtifactTaskSet,
		SummaryMarkdown: "# Plan\n\n" + title,
		Payload:         payload,
		ClientKey:       clientKey,
	})
	if err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	return artifact
}

func TestSubmitForReviewParksAgentNodeOnInteractiveGate(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "First draft")

	wait, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID, Actor: ActorAgent})
	if err != nil {
		t.Fatalf("submit for review: %v", err)
	}
	if wait.Kind != WorkflowWaitHumanGate || wait.NodeRunID != nodeRun.ID {
		t.Fatalf("review wait = %+v, want human gate on the plan node", wait)
	}
	details := ParseReviewWaitDetails(wait.Details)
	if !details.Interactive || details.ArtifactID != artifact.ID || details.GateNodeKey != "review" {
		t.Fatalf("wait details = %+v", details)
	}
	if len(details.Outcomes) != 3 || details.Outcomes[0] != "approved" {
		t.Fatalf("wait outcomes = %v", details.Outcomes)
	}
	if !strings.Contains(wait.Message, "Review the proposed implementation tasks") {
		t.Fatalf("wait message = %q", wait.Message)
	}

	status, err := runs.ReviewStatus(ctx, task.ID, nodeRun.ID)
	if err != nil || status.State != ReviewStateWaiting || status.ArtifactID != artifact.ID {
		t.Fatalf("review status = %+v err=%v", status, err)
	}
	latest, err := runs.Get(ctx, run.ID)
	if err != nil || latest.State != WorkflowRunWaiting {
		t.Fatalf("run = %+v err=%v, want waiting", latest, err)
	}

	// A second submit while the wait is open conflicts: one review at a time.
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("duplicate submit err = %v, want conflict", err)
	}
}

func TestRespondReviewChangesRequestedResumesSameSession(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "First draft")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); err != nil {
		t.Fatalf("submit for review: %v", err)
	}

	result, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "changes_requested", "split the storage task", ActorHuman)
	if err != nil {
		t.Fatalf("respond review: %v", err)
	}
	if result.Outcome != "changes_requested" || result.ArtifactID != artifact.ID || result.SessionID != "" {
		t.Fatalf("respond result = %+v", result)
	}
	if result.Result.Run.State != WorkflowRunRunning || result.Result.Run.CurrentNodeRunID != nodeRun.ID {
		t.Fatalf("run after revise = %+v, want running on the same node", result.Result.Run)
	}
	resumedNode, found, err := runs.GetNodeRun(ctx, nodeRun.ID)
	if err != nil || !found {
		t.Fatalf("load node: found=%t err=%v", found, err)
	}
	if resumedNode.State != WorkflowNodeRunning || resumedNode.Visit != nodeRun.Visit {
		t.Fatalf("node after revise = %+v, want same running visit", resumedNode)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || waiting {
		t.Fatalf("open wait after revise = %t err=%v", waiting, err)
	}

	status, err := runs.ReviewStatus(ctx, task.ID, nodeRun.ID)
	if err != nil {
		t.Fatalf("review status: %v", err)
	}
	if status.State != ReviewStateResolved || status.Outcome != "changes_requested" || status.Feedback != "split the storage task" {
		t.Fatalf("resolved status = %+v", status)
	}

	// The agent revises in the same session and submits a new draft: a new
	// wait opens on the same node run.
	revised := createPlanArtifact(t, ctx, artifacts, result.Result.Run, resumedNode, "", "v2", "Second draft")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: revised.ID}); err != nil {
		t.Fatalf("resubmit for review: %v", err)
	}
	status, err = runs.ReviewStatus(ctx, task.ID, nodeRun.ID)
	if err != nil || status.State != ReviewStateWaiting || status.ArtifactID != revised.ID {
		t.Fatalf("status after resubmit = %+v err=%v", status, err)
	}
}

func TestRespondReviewApprovedCompletesNodeAndAnswersGate(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "Ship it")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); err != nil {
		t.Fatalf("submit for review: %v", err)
	}

	result, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "approved", "looks good", ActorHuman)
	if err != nil {
		t.Fatalf("respond review: %v", err)
	}
	// Owner-created artifacts carry no session, so there is nothing to finish;
	// session-owned artifacts return their session id for the API layer.
	if result.SessionID != "" {
		t.Fatalf("session to finish = %q, want empty for an owner artifact", result.SessionID)
	}
	if !result.Result.Done || result.Result.Run.State != WorkflowRunCompleted {
		t.Fatalf("run = %+v, want completed", result.Result.Run)
	}

	planNode, _, err := runs.GetNodeRun(ctx, nodeRun.ID)
	if err != nil {
		t.Fatalf("load plan node: %v", err)
	}
	if planNode.State != WorkflowNodeSucceeded || planNode.Outcome != "completed" || planNode.OutputArtifactID != artifact.ID {
		t.Fatalf("plan node = %+v, want completed with the reviewed artifact", planNode)
	}
	detail, err := runs.Detail(ctx, run.ID)
	if err != nil {
		t.Fatalf("load run detail: %v", err)
	}
	var gateVisit *WorkflowNodeRun
	for i := range detail.NodeRuns {
		if detail.NodeRuns[i].NodeKey == "review" {
			gateVisit = &detail.NodeRuns[i]
		}
	}
	if gateVisit == nil || gateVisit.State != WorkflowNodeSucceeded || gateVisit.Outcome != "approved" {
		t.Fatalf("gate visit = %+v, want answered approved", gateVisit)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || waiting {
		t.Fatalf("open wait = %t err=%v, want none", waiting, err)
	}

	completed, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if completed.State == nil || *completed.State != LifecycleDone || completed.DoneResolution == nil || *completed.DoneResolution != ResolutionCompleted {
		t.Fatalf("task = %+v, want done/completed", completed)
	}

	status, err := runs.ReviewStatus(ctx, task.ID, nodeRun.ID)
	if err != nil || status.State != ReviewStateResolved || status.Outcome != "approved" {
		t.Fatalf("status = %+v err=%v", status, err)
	}
}

func TestRespondReviewRejectedEndsRejected(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "Nope")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); err != nil {
		t.Fatalf("submit for review: %v", err)
	}
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "rejected", "not worth it", ActorHuman); err != nil {
		t.Fatalf("respond review: %v", err)
	}
	completed, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if completed.DoneResolution == nil || *completed.DoneResolution != ResolutionRejected {
		t.Fatalf("task = %+v, want rejected", completed)
	}
}

func TestSubmitForReviewValidatesContract(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "Draft")

	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID, SessionID: "s-other"}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("foreign session submit err = %v, want conflict", err)
	}
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: "wa-missing"}); !errors.Is(err, ErrWorkflowArtifactNotFound) {
		t.Fatalf("missing artifact err = %v, want artifact not found", err)
	}
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: "wnr-missing", ArtifactID: artifact.ID}); err == nil {
		t.Fatal("missing node run should fail")
	}
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "approved", "", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("respond without a wait err = %v, want conflict", err)
	}
}

func TestSubmitForReviewRequiresDownstreamGate(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	agent, err := NewAgentDefService(flows.db).Create(ctx, AgentDefInput{
		Name: "worker", Harness: "harness", Prompt: "Work.",
	})
	if err != nil {
		t.Fatalf("create agent definition: %v", err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "gateless",
		StartNode: "work",
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceBase, Artifact: ArtifactTaskSet}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create gateless flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "No gate", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "Draft")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("submit without downstream gate err = %v, want conflict", err)
	}
}

func TestRespondReviewRejectsUnofferedOutcome(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	artifacts := NewWorkflowArtifactService(flows.db, tasks)
	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Plan me", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	nodeRun := startPlanNode(t, ctx, runs, run.ID)
	artifact := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "Draft")
	if _, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: artifact.ID}); err != nil {
		t.Fatalf("submit for review: %v", err)
	}
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "ship_it", "", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("unoffered outcome err = %v, want conflict", err)
	}
	// The wait is still open: a bad verdict must not consume the review.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || !waiting {
		t.Fatalf("wait after bad outcome = %t err=%v, want open", waiting, err)
	}
}
