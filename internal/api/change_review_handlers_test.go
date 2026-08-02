package api

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

func TestSubmitReviewApprovalAdvancesHumanGate(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review approval")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Approve from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-approval"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-approval",
		"1111111111111111111111111111111111111111", timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	var review reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  "1111111111111111111111111111111111111111",
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
		}, http.StatusOK, &review)
	if review.Check == nil || review.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("review check = %+v, want satisfied", review.Check)
	}
	if len(review.Threads) != 1 || review.Threads[0].AnchorCommitSHA != "1111111111111111111111111111111111111111" {
		t.Fatalf("review threads = %+v, want one thread anchored to the submitted head", review.Threads)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("workflow state = %q, want completed after approval", run.State)
	}
	gate, ok, err := fixture.Bundle.WorkflowRuns.GetNodeRun(context.Background(), scheduled.Run.CurrentNodeRunID)
	if err != nil || !ok {
		t.Fatalf("load human gate: found=%t err=%v", ok, err)
	}
	if gate.State != coordinator.WorkflowNodeSucceeded || gate.Outcome != "approved" {
		t.Fatalf("human gate = %+v, want succeeded with approved outcome", gate)
	}
}

func TestSubmitReviewCommentLeavesHumanGateWaiting(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review comment")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Comment from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-comment"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-comment",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// A matching-head bare comment must record the notes without moving the
	// review forward: no verdict check is reported and the run keeps waiting at
	// the human gate with no outcome recorded. Before the gate call was guarded
	// the comment verdict completed the gate as changes_requested, sending the
	// task back to the author.
	var review reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "comment",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "inline note"}},
		}, http.StatusOK, &review)
	if review.Check != nil {
		t.Fatalf("comment review recorded check %+v, want none", review.Check)
	}
	if len(review.Threads) != 1 || review.Threads[0].AnchorCommitSHA != headSHA {
		t.Fatalf("review threads = %+v, want one thread anchored to the submitted head", review.Threads)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("comment review recorded %d human-review checks, want 0", checkCount)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a bare comment", run.State)
	}
	gate, ok, err := fixture.Bundle.WorkflowRuns.GetNodeRun(context.Background(), scheduled.Run.CurrentNodeRunID)
	if err != nil || !ok {
		t.Fatalf("load human gate: found=%t err=%v", ok, err)
	}
	if gate.State == coordinator.WorkflowNodeSucceeded || gate.Outcome != "" {
		t.Fatalf("human gate = %+v, want not completed with no outcome after a bare comment", gate)
	}
}

// TestSubmitReviewCommentDoesNotStartScheduledRun guards the executor nudge
// after a successful submission: handleSubmitReview used to call
// advanceWorkflowForTask for every verdict, so a matching-head bare comment on
// a scheduled run — whose first node is a human gate — would start the
// workflow and park it waiting at the gate, moving the review forward even
// though a comment must not. The run must stay scheduled with no node created.
func TestSubmitReviewCommentDoesNotStartScheduledRun(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review comment scheduled")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Comment on scheduled change", FlowID: flow.ID}, http.StatusCreated, &created)

	// Schedule through the service rather than the HTTP endpoint (which
	// advances the run), so the run is created in the scheduled state with no
	// current node — exactly the state where an executor nudge would start the
	// workflow and create the human gate.
	scheduled, err := fixture.Bundle.WorkflowRuns.Schedule(context.Background(), created.Task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	if scheduled.State != coordinator.WorkflowRunScheduled {
		t.Fatalf("scheduled run state = %q, want scheduled", scheduled.State)
	}

	const (
		changeID  = "ch-change-review-comment-scheduled"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-comment-scheduled",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	var review reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "comment",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "inline note"}},
		}, http.StatusOK, &review)
	if len(review.Threads) != 1 || review.Threads[0].AnchorCommitSHA != headSHA {
		t.Fatalf("review threads = %+v, want one thread anchored to the submitted head", review.Threads)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunScheduled {
		t.Fatalf("workflow state = %q, want still scheduled after a bare comment", run.State)
	}
	if run.CurrentNodeRunID != "" {
		t.Fatalf("scheduled run gained current node %q, want none", run.CurrentNodeRunID)
	}
	var nodeRunCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workflow_node_runs WHERE workflow_run_id = ?`, scheduled.ID).Scan(&nodeRunCount); err != nil {
		t.Fatalf("count node runs: %v", err)
	}
	if nodeRunCount != 0 {
		t.Fatalf("bare comment started the workflow: %d node runs created, want 0", nodeRunCount)
	}
}

func TestSubmitReviewEmptyHeadRejected(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review empty head")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Empty-head review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-empty-head"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-empty-head",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// An omitted or empty head_sha is rejected up front with a stable
	// 400 head_sha_required response, before any thread or verdict check can be
	// created. Both the omitted field and a whitespace-only value must fail the
	// same way (the coordinator trims the submitted head, so a whitespace-only
	// head can never match either).
	for _, submittedHead := range []string{"", "   "} {
		var resp errorResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
			reviewVerdictRequest{
				Verdict:  "approve",
				HeadSHA:  submittedHead,
				Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "comment on inspected code"}},
			}, http.StatusBadRequest, &resp)
		if resp.Error.Code != "head_sha_required" {
			t.Fatalf("empty head (%q): error code = %q, want head_sha_required", submittedHead, resp.Error.Code)
		}
		if resp.Error.Message != "head sha is required" {
			t.Fatalf("empty head (%q): error message = %q, want %q", submittedHead, resp.Error.Message, "head sha is required")
		}

		var threadCount int
		if err := fixture.DB.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
			t.Fatalf("count threads: %v", err)
		}
		if threadCount != 0 {
			t.Fatalf("empty-head review created %d threads, want 0", threadCount)
		}
		var checkCount int
		if err := fixture.DB.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
			t.Fatalf("count checks: %v", err)
		}
		if checkCount != 0 {
			t.Fatalf("empty-head review recorded %d human-review checks, want 0", checkCount)
		}
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a refused empty-head review", run.State)
	}
}

func TestSubmitReviewStaleHeadConflicts(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review stale head")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Stale review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-stale-head"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-stale-head",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// The reviewer inspected headSHA, but the change advanced before the
	// submission landed: the server must refuse the whole review.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  "2222222222222222222222222222222222222222",
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "comment on inspected code"}},
		}, http.StatusConflict, nil)

	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("stale-head review created %d threads, want 0", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("stale-head review recorded %d human-review checks, want 0", checkCount)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a refused stale review", run.State)
	}
}

// TestSubmitReviewMismatchedCommentAnchorRejected guards the review-integrity
// invariant against a hand-crafted request: the submission's head validates
// against the change's current head, but a per-comment anchor that names a
// different commit must be refused with a clear 400 rather than binding the
// thread to an arbitrary or older commit. The web client never sends a
// per-comment anchor, so this is API-surface hardening.
func TestSubmitReviewMismatchedCommentAnchorRejected(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review mismatched anchor")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Mismatched anchor review", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-mismatched-anchor"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-mismatched-anchor",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// The submission validates against the inspected head, but the comment
	// names a different commit: the server must refuse the whole review with a
	// clear 400 instead of honoring the arbitrary anchor.
	var rejected errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, AnchorCommitSHA: "2222222222222222222222222222222222222222", Body: "comment on older code"}},
		}, http.StatusBadRequest, &rejected)
	if rejected.Error.Code != "invalid_comment_anchor" {
		t.Fatalf("rejection code = %q, want invalid_comment_anchor", rejected.Error.Code)
	}

	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("mismatched-anchor review created %d threads, want 0", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("mismatched-anchor review recorded %d human-review checks, want 0", checkCount)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a refused mismatched-anchor review", run.State)
	}
}

func TestSubmitReviewHeadUpdateCannotInterleave(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review head race")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Racing head review", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-head-race"
		headSHA   = "1111111111111111111111111111111111111111"
		newHead   = "2222222222222222222222222222222222222222"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-head-race",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// The submission must be atomic against a head update: the change's head is
	// re-read inside the same transaction that writes the threads and verdict,
	// so an update cannot land between the comparison and the writes.
	// AfterHeadCheck fires inside that transaction right after the comparison:
	// a head update attempted there over a second connection with busy_timeout 0
	// must fail immediately (SQLITE_BUSY) while the review holds the write
	// transaction, and the whole submission must abort with nothing created.
	// Before the atomic fix the comparison ran outside any transaction and the
	// racing update would succeed in the gap, letting the stale review record
	// threads and a verdict for the newer head.
	var raceErr error
	fixture.Bundle.Threads.AfterHeadCheck = func() error {
		raceDB, err := sql.Open("sqlite3", flowgit.ProjectDatabasePath(fixture.DataDir, fixture.Project.ID))
		if err != nil {
			raceErr = err
			return err
		}
		defer raceDB.Close()
		conn, err := raceDB.Conn(context.Background())
		if err != nil {
			raceErr = err
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout = 0"); err != nil {
			raceErr = err
			return err
		}
		_, raceErr = conn.ExecContext(context.Background(),
			`UPDATE changes SET head_sha = ?, updated_at = ? WHERE id = ?`,
			newHead, timestamp, changeID)
		return raceErr
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "comment on inspected code"}},
		}, http.StatusBadRequest, nil)
	if raceErr == nil {
		t.Fatal("racing head update succeeded inside the review transaction: the head comparison is not atomic with the writes")
	}

	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("interleaved review created %d threads, want 0", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("interleaved review recorded %d human-review checks, want 0", checkCount)
	}
	var currentHead string
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT head_sha FROM changes WHERE id = ?`, changeID).Scan(&currentHead); err != nil {
		t.Fatalf("load change head: %v", err)
	}
	if currentHead != headSHA {
		t.Fatalf("change head = %q, want the inspected head %q (the racing update must not have landed)", currentHead, headSHA)
	}
	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after an aborted review", run.State)
	}
}
