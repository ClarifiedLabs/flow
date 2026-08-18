package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// disputeTestEnv drives the reopen-escalation behavior through the real review
// flow: a human gate whose changes_requested verdict is applied by
// RespondReview, the same path a submitted review verdict takes.
type disputeTestEnv struct {
	flows   *FlowService
	tasks   *TaskService
	runs    *WorkflowRunService
	threads *ThreadService
	task    Task
	run     WorkflowRun
	nodeRun WorkflowNodeRun
	wait    WorkflowWait
}

func newDisputeTestEnv(t *testing.T) *disputeTestEnv {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	flows := NewFlowService(store.DB())
	tasks := NewTaskService(store.DB(), "p-test")
	runs := NewWorkflowRunService(store.DB(), flows, tasks)
	threads := NewThreadService(store.DB())
	threads.Runs = runs
	threads.Checks = NewCheckService(store.DB())
	runs.Threads = threads

	flow := reviewFlowFixture(t, ctx, flows)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Disputed fix", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	planNode := startPlanNode(t, ctx, runs, run.ID)
	insertChangeForTest(t, store.DB(), task.ID, "ch-dispute", "task/"+task.ID+"/run-1", false)
	// Bind the change to the run and publish the change artifact the run
	// carries, so the convergence projection checks hold for the dispute hold.
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes SET workflow_run_id = ? WHERE id = ?`, run.ID, "ch-dispute"); err != nil {
		t.Fatalf("bind change to run: %v", err)
	}
	// Publish the change artifact against the plan agent node run — only agent
	// nodes may create artifacts — and then complete the plan node so the run
	// advances to its ordinary human gate. The artifact stays the run's current
	// artifact through the transition, which is what the dispute evidence
	// projects.
	artifacts := NewWorkflowArtifactService(store.DB(), tasks)
	artifact, _, err := artifacts.Create(ctx, CreateWorkflowArtifactInput{
		WorkflowRunID: run.ID, Kind: ArtifactTaskSet,
		SummaryMarkdown: "Disputed task set",
		Payload:         json.RawMessage(`{"schema_version":1,"items":[{"key":"a","kind":"task","title":"Disputed fix","body":"Fix the race"}]}`),
		NodeRunID:       planNode.ID,
		CreatorKey:      "dispute-test-plan",
		ClientKey:       "dispute-test-plan",
	})
	if err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	// The plan agent node's output artifact is what completes it.
	if _, err := store.DB().ExecContext(ctx, `
UPDATE workflow_node_runs SET output_artifact_id = ? WHERE id = ?`, artifact.ID, planNode.ID); err != nil {
		t.Fatalf("attach plan artifact: %v", err)
	}
	nodeRun := planNode
	// Complete the agent node so the run advances to its ordinary human gate;
	// the gate's open wait is the review round a submitted verdict answers.
	if _, err := runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID, Outcome: "completed", Actor: ActorSystem, ArtifactID: artifact.ID,
	}); err != nil {
		t.Fatalf("complete plan node: %v", err)
	}
	advanced, err := runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("load advanced run: %v", err)
	}
	// Seed a change artifact row directly for the gate node so the convergence
	// projection's artifact check sees the change identity the dispute evidence
	// carries. The artifact service restricts creation to agent nodes, but the
	// projection only reads the row.
	changeArtifactID := "wa-dispute-change"
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, base_revision, client_key, created_at
) VALUES (?, ?, ?, 'dispute-test', 'change', 'Disputed change under review', ?, ?, '', 'dispute-change', ?)`,
		changeArtifactID, run.ID, advanced.CurrentNodeRunID,
		`{"change_id":"ch-dispute","head_sha":"1111111111111111111111111111111111111111"}`,
		"dispute-digest", formatTime(sqlitex.UTCNow())); err != nil {
		t.Fatalf("insert change artifact: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE workflow_runs SET current_artifact_id = ? WHERE id = ?`, changeArtifactID, run.ID); err != nil {
		t.Fatalf("set current artifact: %v", err)
	}
	advanced, err = runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload advanced run: %v", err)
	}
	gateWait, waiting, err := runs.OpenWait(ctx, task.ID)
	if err != nil || !waiting {
		t.Fatalf("open gate wait: waiting=%t err=%v", waiting, err)
	}
	if advanced.CurrentNodeKey != "review" || gateWait.NodeRunID != advanced.CurrentNodeRunID {
		t.Fatalf("advanced run = %q at %q, gate wait node = %q", advanced.State, advanced.CurrentNodeKey, gateWait.NodeRunID)
	}
	return &disputeTestEnv{
		flows: flows, tasks: tasks, runs: runs, threads: threads,
		task: task, run: advanced, nodeRun: WorkflowNodeRun{ID: advanced.CurrentNodeRunID}, wait: gateWait,
	}
}

// respondChangesRequested applies the verdict through the atomic review
// submission path — the same transaction a human's request_changes verdict
// takes, and the one the escalation hook guards.
func (env *disputeTestEnv) respondChangesRequested(t *testing.T, feedback string) {
	t.Helper()
	if _, err := env.threads.SubmitReview(t.Context(), SubmitReviewInput{
		ChangeID:     "ch-dispute",
		HeadSHA:      "1111111111111111111111111111111111111111",
		Verdict:      "request_changes",
		NodeRunID:    env.nodeRun.ID,
		ReviewWaitID: env.wait.ID,
		Body:         feedback,
		CheckName:    "human-review",
		Actor:        "owner",
	}); err != nil {
		t.Fatalf("submit review: %v", err)
	}
}

// seedDisputedThread files a thread, has the author claim it, certifies it, and
// then reopens it — the certification-invalidating reopen that demands an
// operator ruling.
func seedDisputedThread(t *testing.T, env *disputeTestEnv) ReviewThread {
	t.Helper()
	ctx := context.Background()
	thread, err := env.threads.CreateThread(ctx, CreateThreadInput{
		ChangeID:        "ch-dispute",
		AnchorCommitSHA: "1111111111111111111111111111111111111111",
		FilePath:        "internal/app.go",
		Line:            10,
		Body:            "Race remains.",
		Actor:           "reviewer:alice",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := env.threads.ClaimThread(ctx, ClaimThreadInput{
		ThreadID: thread.ID, Kind: ClaimFixed, Actor: "author", ClaimCommitSHA: "2222222222222222222222222222222222222222",
	}); err != nil {
		t.Fatalf("claim thread: %v", err)
	}
	if _, err := env.threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: thread.ID, Actor: "verifier"}); err != nil {
		t.Fatalf("certify thread: %v", err)
	}
	reopened, err := env.threads.ReopenThread(ctx, VerifyThreadInput{
		ThreadID: thread.ID, Actor: "verifier", Body: "The fix did not hold.",
	})
	if err != nil {
		t.Fatalf("reopen thread: %v", err)
	}
	if reopened.ReopenCount != 1 {
		t.Fatalf("reopen count = %d, want 1", reopened.ReopenCount)
	}
	return reopened
}

// TestDisputedReopenBlocksAuthorCycle proves the escalation: a
// changes_requested verdict over a thread reopened after certification does
// not start another author cycle; it parks the run on an operator wait with a
// convergence request, and the author-job path refuses to enqueue.
func TestDisputedReopenBlocksAuthorCycle(t *testing.T) {
	t.Parallel()
	env := newDisputeTestEnv(t)
	ctx := context.Background()
	seedDisputedThread(t, env)

	disputed, err := env.threads.DisputedOpenThreads(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("list disputed: %v", err)
	}
	if len(disputed) != 1 {
		t.Fatalf("disputed threads = %+v, want one", disputed)
	}

	env.respondChangesRequested(t, "fix the race")

	detail, err := env.runs.Detail(ctx, env.run.ID)
	if err != nil {
		t.Fatalf("load run detail: %v", err)
	}
	if detail.OpenWait == nil ||
		detail.OpenWait.Kind != WorkflowWaitOperatorIntervention ||
		detail.OpenWait.Reason != WorkflowWaitReasonReviewThreadDispute {
		t.Fatalf("open wait = %+v, want review-thread-dispute operator wait", detail.OpenWait)
	}
	if detail.Run.State != WorkflowRunWaiting || detail.Run.CurrentNodeKey != "review" {
		t.Fatalf("run = %q at %q, want waiting at review", detail.Run.State, detail.Run.CurrentNodeKey)
	}
	// No review -> implement transition happened.
	if detail.Run.ReviewCyclesUsed != 0 {
		t.Fatalf("review cycles used = %d, want 0 (no cycle started)", detail.Run.ReviewCyclesUsed)
	}
	// The convergence request is recorded for the operator's disposition flow.
	evidence, err := env.runs.ActiveConvergenceEvidenceForTask(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("load convergence evidence: %v", err)
	}
	if evidence == nil {
		t.Fatal("no convergence evidence recorded for the dispute hold")
	}

	// Belt and braces: the author-job path suppresses enqueueing.
	sessions := NewSessionService(env.runs.db, env.tasks, nil)
	if _, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{TaskID: env.task.ID}); !errors.Is(err, ErrAuthorJobSuppressed) {
		t.Fatalf("EnsureAuthorJob error = %v, want ErrAuthorJobSuppressed", err)
	}
}

// TestSingleReopenOfClaimedThreadKeepsCycling is the regression guard: a
// single reopen of a merely-claimed thread (never certified) keeps today's
// behavior — the verdict applies and the author cycle proceeds.
func TestSingleReopenOfClaimedThreadKeepsCycling(t *testing.T) {
	t.Parallel()
	env := newDisputeTestEnv(t)
	ctx := context.Background()

	thread, err := env.threads.CreateThread(ctx, CreateThreadInput{
		ChangeID:        "ch-dispute",
		AnchorCommitSHA: "1111111111111111111111111111111111111111",
		FilePath:        "internal/app.go",
		Line:            10,
		Body:            "Concern.",
		Actor:           "reviewer:alice",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := env.threads.ClaimThread(ctx, ClaimThreadInput{
		ThreadID: thread.ID, Kind: ClaimFixed, Actor: "author", ClaimCommitSHA: "2222222222222222222222222222222222222222",
	}); err != nil {
		t.Fatalf("claim thread: %v", err)
	}
	reopened, err := env.threads.ReopenThread(ctx, VerifyThreadInput{
		ThreadID: thread.ID, Actor: "verifier", Body: "Not fixed yet.",
	})
	if err != nil {
		t.Fatalf("reopen thread: %v", err)
	}
	if reopened.ReopenCount != 1 {
		t.Fatalf("reopen count = %d, want 1", reopened.ReopenCount)
	}
	disputed, err := env.threads.DisputedOpenThreads(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("list disputed: %v", err)
	}
	if len(disputed) != 0 {
		t.Fatalf("disputed threads = %+v, want none for a single claim reopen", disputed)
	}

	env.respondChangesRequested(t, "please fix")

	detail, err := env.runs.Detail(ctx, env.run.ID)
	if err != nil {
		t.Fatalf("load run detail: %v", err)
	}
	if detail.OpenWait != nil {
		t.Fatalf("open wait = %+v, want none (verdict applied normally)", detail.OpenWait)
	}
	if detail.Run.CurrentNodeKey != "plan" {
		t.Fatalf("current node = %q, want plan (send-back applied)", detail.Run.CurrentNodeKey)
	}
}

// TestDisputedReopenResumeAfterAcceptScope proves the operator can still move
// the task forward: resolving the convergence hold with accept_scope releases
// it.
func TestDisputedReopenResumeAfterAcceptScope(t *testing.T) {
	t.Parallel()
	env := newDisputeTestEnv(t)
	ctx := context.Background()
	seedDisputedThread(t, env)

	env.respondChangesRequested(t, "fix the race")

	evidence, err := env.runs.ActiveConvergenceEvidenceForTask(ctx, env.task.ID)
	if err != nil || evidence == nil {
		t.Fatalf("load convergence evidence: %+v, %v", evidence, err)
	}
	result, err := env.runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: env.task.ID, Disposition: ConvergenceAcceptScope,
		Actor: ActorHuman, ExpectedEvidenceFingerprint: evidence.Fingerprint,
		Note: "Scope accepted; proceed with the send-back.",
	})
	if err != nil {
		t.Fatalf("resolve convergence: %v", err)
	}
	if result.Run.Held() {
		t.Fatalf("run still held after accept_scope: %+v", result.Run)
	}
}
