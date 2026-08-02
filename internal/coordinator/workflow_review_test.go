package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
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

	result, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "changes_requested", "split the storage task", ActorHuman)
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

	result, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "approved", "looks good", ActorHuman)
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
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "rejected", "not worth it", ActorHuman); err != nil {
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

// TestRespondReplayReportsCommittedRunState pins the same-outcome replay
// contract: retrying a terminal verdict replays the recorded decision, and the
// replay result's Run and Done must describe the committed state (the run
// completed) rather than the pre-decision snapshot (which would still show the
// run waiting). This is the non-interactive sibling of the RespondReview
// replay path; both answer their gate through CompleteNode.
func TestRespondReplayReportsCommittedRunState(t *testing.T) {
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

	// Advance the agent node into the human gate, then answer the gate with a
	// terminal verdict — the same shape RespondReview uses for an approval.
	advanced, err := runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID, Outcome: "completed", ArtifactID: artifact.ID, Actor: ActorSystem,
	})
	if err != nil || advanced.Next == nil || advanced.Next.NodeKey != "review" {
		t.Fatalf("advance to review gate = %+v err=%v", advanced, err)
	}
	decided, err := runs.Respond(ctx, task.ID, advanced.Next.ID, "approved", "looks good", ActorHuman)
	if err != nil || !decided.Done || decided.Run.State != WorkflowRunCompleted {
		t.Fatalf("gate decision = %+v err=%v, want completed run", decided, err)
	}

	// A same-outcome retry replays the recorded decision; its metadata must
	// reflect the committed state, not the pre-decision snapshot.
	replayed, err := runs.Respond(ctx, task.ID, advanced.Next.ID, "approved", "looks good", ActorHuman)
	if err != nil {
		t.Fatalf("replayed gate decision: %v", err)
	}
	if !replayed.Replayed || !replayed.Done || replayed.Run.State != WorkflowRunCompleted {
		t.Fatalf("replayed gate decision = %+v, want a completed replay", replayed)
	}
}

// TestCompleteNodeReplayWithStaleSnapshotReportsCommittedRun pins the fix for
// the same-outcome replay path: a replay transaction whose snapshot predates
// the original terminal decision must still report the committed run. The
// replay branch of completeNodeTx — the shared state machine RespondReview's
// terminal verdict flows through via CompleteNode — returns the run scanned
// inside its transaction; when that transaction observed the run before the
// decision committed, the pre-fix code returned the stale waiting snapshot
// with Done=false even though the committed decision completed the run. This
// test builds exactly that interleaving: it pins a real read transaction to
// the pre-terminal snapshot (agent node run already committed succeeded, run
// still waiting at the gate), commits the terminal gate decision from a
// second connection to the same database file, and only then runs the replay
// from the stale transaction. The replay must reload the committed run and
// report Done=true.
func TestCompleteNodeReplayWithStaleSnapshotReportsCommittedRun(t *testing.T) {
	ctx := context.Background()

	// A second handle on the same database file provides the connection that
	// commits the original decision while the replay transaction stays open
	// (each store pool is a single connection; WAL allows a concurrent reader
	// and writer).
	path := filepath.Join(t.TempDir(), "flow.db")
	store1, err := flowdb.Open(ctx, path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store1.Close() })
	store2, err := flowdb.Open(ctx, path)
	if err != nil {
		t.Fatalf("open second database handle: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	flows := NewFlowService(store1.DB())
	tasks := NewTaskService(store1.DB(), "p-test")
	runs := NewWorkflowRunService(store1.DB(), flows, tasks)
	artifacts := NewWorkflowArtifactService(store1.DB(), tasks)
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

	// Advance the agent node into the human gate: the agent node run is
	// committed succeeded and the run waits at the gate — the pre-terminal
	// state a concurrent replay transaction may observe.
	advanced, err := runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID, Outcome: "completed", ArtifactID: artifact.ID, Actor: ActorSystem,
	})
	if err != nil || advanced.Next == nil || advanced.Next.NodeKey != "review" {
		t.Fatalf("advance to review gate = %+v err=%v", advanced, err)
	}
	if advanced.Run.State != WorkflowRunWaiting {
		t.Fatalf("run before the decision = %+v, want waiting at the gate", advanced.Run)
	}

	// Open the replay transaction and pin it to the pre-terminal snapshot
	// before the terminal decision commits. From this snapshot the agent node
	// run is already succeeded (committed above) but the run is still waiting
	// at the gate — the exact stale state the replay branch can observe.
	replayTx, err := store1.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin replay transaction: %v", err)
	}
	defer replayTx.Rollback()
	var snapshotState string
	if err := replayTx.QueryRowContext(ctx, `SELECT state FROM workflow_runs WHERE id = ?`, run.ID).Scan(&snapshotState); err != nil {
		t.Fatalf("pin replay snapshot: %v", err)
	}
	if snapshotState != string(WorkflowRunWaiting) {
		t.Fatalf("replay snapshot state = %q, want waiting", snapshotState)
	}

	// The original terminal decision commits on the second connection while
	// the replay transaction is still open.
	runs2 := NewWorkflowRunService(store2.DB(), flows, tasks)
	decided, err := runs2.Respond(ctx, task.ID, advanced.Next.ID, "approved", "looks good", ActorHuman)
	if err != nil || !decided.Done || decided.Run.State != WorkflowRunCompleted {
		t.Fatalf("gate decision = %+v err=%v, want completed run", decided, err)
	}

	// The replay transaction still observes the pre-terminal snapshot even
	// though the decision committed.
	var staleState string
	if err := replayTx.QueryRowContext(ctx, `SELECT state FROM workflow_runs WHERE id = ?`, run.ID).Scan(&staleState); err != nil {
		t.Fatalf("re-read replay snapshot: %v", err)
	}
	if staleState != string(WorkflowRunWaiting) {
		t.Fatalf("replay snapshot after decision = %q, want the stale waiting state", staleState)
	}

	// The retry from the stale transaction replays the recorded agent-node
	// decision. Its transaction still sees the run waiting, so the replay must
	// reload the committed run: Done must be true and Result.Run completed.
	replayed, err := runs.completeNodeTx(ctx, replayTx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID, Outcome: "completed", ArtifactID: artifact.ID, Actor: ActorSystem,
	}, true, replayTx.Commit)
	if err != nil {
		t.Fatalf("stale-snapshot replay: %v", err)
	}
	if !replayed.Replayed || !replayed.Done || replayed.Run.State != WorkflowRunCompleted {
		t.Fatalf("stale-snapshot replay = %+v, want a completed replay reporting the committed run", replayed)
	}
}

// TestRespondReviewRetryReusesRecordedOutcome pins the recorded-outcome
// contract for the interactive review path: a round's verdict is durable, and
// a retry of a decided round — same outcome or contradictory — is rejected
// instead of re-deciding the gate, so the recorded gate outcome always
// stands. (The replay of a recorded outcome itself lives in the shared
// CompleteNode idempotency path, covered by TestRespondReplayReportsCommittedRunState
// and TestCompleteNodeReplayWithStaleSnapshotReportsCommittedRun.)
func TestRespondReviewRetryReusesRecordedOutcome(t *testing.T) {
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

	result, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "approved", "looks good", ActorHuman)
	if err != nil {
		t.Fatalf("respond review: %v", err)
	}
	if !result.Result.Done || result.Result.Run.State != WorkflowRunCompleted {
		t.Fatalf("run = %+v, want completed", result.Result.Run)
	}

	// A same-outcome retry of the decided round is rejected instead of
	// re-deciding the gate; the recorded outcome stands either way.
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "approved", "looks good", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("same-outcome retry err = %v, want conflict", err)
	}
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "rejected", "changed my mind", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("contradictory retry err = %v, want conflict", err)
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
		t.Fatalf("gate visit after retries = %+v, want the recorded approved", gateVisit)
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
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "approved", "", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
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
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "", "ship_it", "", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("unoffered outcome err = %v, want conflict", err)
	}
	// The wait is still open: a bad verdict must not consume the review.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || !waiting {
		t.Fatalf("wait after bad outcome = %t err=%v, want open", waiting, err)
	}
}

func TestRespondReviewStaleRoundBindingCannotDecideReopenedWait(t *testing.T) {
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
	draft := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "First draft")

	// Round one: the agent submits for review and a wait opens on the node run.
	// The response handler observes this wait id before taking the lock.
	roundOne, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: draft.ID})
	if err != nil {
		t.Fatalf("submit round one: %v", err)
	}

	// The concurrent decision: changes_requested resolves round one and the
	// agent resubmits, reopening a FRESH wait on the same node run.
	decision, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "changes_requested", "revise the draft", ActorHuman)
	if err != nil {
		t.Fatalf("decide round one: %v", err)
	}
	revised := createPlanArtifact(t, ctx, artifacts, decision.Result.Run, nodeRun, "", "v2", "Second draft")
	roundTwo, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: revised.ID})
	if err != nil {
		t.Fatalf("submit round two: %v", err)
	}
	if roundTwo.ID == roundOne.ID {
		t.Fatalf("round two wait id = %q, want a fresh wait per round", roundTwo.ID)
	}

	// The stale round-one response now reaches RespondReview bound to round
	// one's wait id: it must be rejected inside the transaction and must not
	// decide round two. A foreign binding is rejected the same way.
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "approved", "stale verdict", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("stale round-one response err = %v, want conflict", err)
	}
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, "ww-foreign", "approved", "", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("foreign binding err = %v, want conflict", err)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || !waiting {
		t.Fatalf("round two wait after stale responses = %t err=%v, want still open", waiting, err)
	}

	// Round two stays actionable: a response bound to round two's own wait id
	// decides it.
	approved, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundTwo.ID, "approved", "looks good", ActorHuman)
	if err != nil {
		t.Fatalf("decide round two: %v", err)
	}
	if !approved.Result.Done || approved.Result.Run.State != WorkflowRunCompleted {
		t.Fatalf("run after round two = %+v, want completed", approved.Result.Run)
	}
}

func TestRespondReviewStaleRoundRacesConcurrentReopen(t *testing.T) {
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
	draft := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "First draft")
	roundOne, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: draft.ID})
	if err != nil {
		t.Fatalf("submit round one: %v", err)
	}

	// The stale round-one response is already in flight: it has observed round
	// one's wait id (as the respond handler does before routing) and is parked
	// at the exact pre-lock point of RespondReview — the test seam — when the
	// concurrent decision resolves round one and reopens round two on the same
	// node run.
	entered := make(chan struct{})
	release := make(chan struct{})
	runs.reviewLockGate = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { runs.reviewLockGate = nil })
	staleErr := make(chan error, 1)
	go func() {
		_, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "approved", "stale verdict", ActorHuman)
		staleErr <- err
	}()
	<-entered

	// Let the concurrent decision pass the seam while the stale response stays
	// parked, then the agent resubmits, reopening a fresh wait on the same node
	// run.
	runs.reviewLockGate = nil
	decision, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "changes_requested", "revise the draft", ActorHuman)
	if err != nil {
		t.Fatalf("decide round one: %v", err)
	}
	revised := createPlanArtifact(t, ctx, artifacts, decision.Result.Run, nodeRun, "", "v2", "Second draft")
	roundTwo, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: revised.ID})
	if err != nil {
		t.Fatalf("submit round two: %v", err)
	}
	if roundTwo.ID == roundOne.ID {
		t.Fatalf("round two wait id = %q, want a fresh wait per round", roundTwo.ID)
	}
	close(release)

	// The stale response now reaches the per-task review lock bound to round
	// one. The wait read under the lock is round two's, so the binding is
	// rejected and the stale verdict cannot decide round two.
	if err := <-staleErr; !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("stale round-one response racing the reopen err = %v, want conflict", err)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil || !waiting {
		t.Fatalf("round two wait after stale race = %t err=%v, want still open", waiting, err)
	}
	// The reopened round stays actionable for its own wait id.
	if _, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundTwo.ID, "approved", "looks good", ActorHuman); err != nil {
		t.Fatalf("decide round two after stale race: %v", err)
	}
}

// TestRespondReviewTerminalVerdictSerializesWithConcurrentReopen parks a
// terminal (approved) verdict in the gap between its marker transaction and
// its terminal CompleteNode calls and proves a concurrent changes_requested
// decision cannot resolve the validated round and reopen a fresh wait in that
// gap: the per-task review lock serialises the whole RespondReview, and the
// delayed decision is rejected once the round is already decided.
func TestRespondReviewTerminalVerdictSerializesWithConcurrentReopen(t *testing.T) {
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
	draft := createPlanArtifact(t, ctx, artifacts, run, nodeRun, "", "v1", "First draft")
	roundOne, err := runs.SubmitForReview(ctx, SubmitForReviewInput{NodeRunID: nodeRun.ID, ArtifactID: draft.ID})
	if err != nil {
		t.Fatalf("submit round one: %v", err)
	}

	// Park the terminal verdict after its marker transaction commits, exactly
	// where a concurrent decision could resolve round one and reopen round two
	// before the verdict's CompleteNode calls run.
	entered := make(chan struct{})
	release := make(chan struct{})
	runs.reviewTerminalGate = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { runs.reviewTerminalGate = nil })
	verdictErr := make(chan error, 1)
	go func() {
		_, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "approved", "stale verdict", ActorHuman)
		verdictErr <- err
	}()
	<-entered

	// A concurrent changes_requested decision on the same task must not run
	// while the terminal verdict is between its marker transaction and its
	// completions.
	decided := make(chan error, 1)
	go func() {
		_, err := runs.RespondReview(ctx, task.ID, nodeRun.ID, roundOne.ID, "changes_requested", "revise the draft", ActorHuman)
		decided <- err
	}()
	select {
	case err := <-decided:
		t.Fatalf("concurrent decision ran while the terminal verdict was in flight: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	close(release)

	if err := <-verdictErr; err != nil {
		t.Fatalf("terminal verdict after serialization: %v", err)
	}
	if err := <-decided; !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("concurrent decision after the round was decided err = %v, want conflict", err)
	}
	completed, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if completed.DoneResolution == nil || *completed.DoneResolution != ResolutionCompleted {
		t.Fatalf("task = %+v, want completed by the terminal verdict", completed)
	}
}

// TestReviewLockQueuedWaiterAndThirdAcquirerDuringCleanup drives the per-task
// review lock cleanup race: a holder releases while a second acquirer is
// queued but not yet blocked, and a third acquirer arrives during the cleanup.
// The cleanup must not drop the task's lock entry while the queued acquirer
// still references it — dropping it and handing the third acquirer a second,
// fresh mutex would let the queued request and the third request run their
// review decisions concurrently for the same task.
func TestReviewLockQueuedWaiterAndThirdAcquirerDuringCleanup(t *testing.T) {
	_, _, runs := newWorkflowModelServices(t)
	taskID := "t-lock-cleanup-race"

	// A holds the task's review lock.
	releaseA := runs.reviewLock(taskID)

	// B queues for the same lock and pauses at the pre-block seam: it has
	// looked the entry up (and, with the reference-counted cleanup, taken a
	// reference) but is not yet a registered waiter on the mutex.
	acquireEntered := make(chan struct{})
	acquireRelease := make(chan struct{})
	runs.reviewLockAcquireGate = func() {
		close(acquireEntered)
		<-acquireRelease
	}
	t.Cleanup(func() { runs.reviewLockAcquireGate = nil })
	bDone := make(chan struct{})
	var releaseB func()
	go func() {
		releaseB = runs.reviewLock(taskID)
		close(bDone)
	}()
	<-acquireEntered

	// A releases and pauses its cleanup right after unlocking, before it
	// decides whether the entry can be dropped.
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	runs.reviewLockCleanupGate = func() {
		close(cleanupEntered)
		<-cleanupRelease
	}
	t.Cleanup(func() { runs.reviewLockCleanupGate = nil })
	aReleased := make(chan struct{})
	go func() {
		releaseA()
		close(aReleased)
	}()
	<-cleanupEntered

	// A's cleanup completes while B is still paused before blocking. B's queued
	// reference must keep the entry alive. (A TryLock cleanup would instead win
	// the just-unlocked mutex before B wakes, delete the entry, and let a later
	// acquirer build a second mutex for the same task.)
	close(cleanupRelease)
	<-aReleased
	runs.reviewLockCleanupGate = nil

	// C — a third acquirer — arrives after the cleanup. It must queue on the
	// SAME entry as B, so the two queued decisions stay serialized.
	runs.reviewLockAcquireGate = nil
	releaseC := runs.reviewLock(taskID)

	// Let B proceed: with correct cleanup it stays blocked on the entry C
	// holds; with the TryLock bug it acquires a second, distinct mutex while C
	// holds the first.
	close(acquireRelease)
	select {
	case <-bDone:
		t.Fatalf("queued acquirer entered the review lock while the third acquirer held it: per-task serialization broken")
	case <-time.After(250 * time.Millisecond):
	}
	releaseC()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("queued acquirer never entered the review lock after the holder released")
	}
	releaseB()
}
