package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// reviewGateRequest binds a change-review verdict to the exact persisted human
// gate the test observed. Production callers receive the same fields from the
// change read model's open_wait; tests must not preserve the old node-only
// response path by fabricating a verdict without them.
func reviewGateRequest(t *testing.T, fixture testFixture, taskID string, request reviewVerdictRequest) reviewVerdictRequest {
	t.Helper()
	wait, waiting, err := fixture.Bundle.WorkflowRuns.OpenWait(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load open review wait: %v", err)
	}
	if !waiting || wait.Kind != coordinator.WorkflowWaitHumanGate || wait.ID == "" || wait.NodeRunID == "" {
		t.Fatalf("open review wait = %+v waiting=%t, want a persisted human gate", wait, waiting)
	}
	request.NodeRunID = wait.NodeRunID
	request.ReviewWaitID = wait.ID
	return request
}

func TestSubmitReviewApprovalAdvancesHumanGate(t *testing.T) {
	t.Parallel()
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
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  "1111111111111111111111111111111111111111",
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
		}), http.StatusOK, &review)
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

func TestSubmitReviewRequiresExactPersistedReviewWait(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review exact wait")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Exact review wait from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-exact-wait"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-exact-wait",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	var change changeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/changes/"+changeID,
		nil, http.StatusOK, &change)
	if change.OpenWait == nil || change.OpenWait.Kind != coordinator.WorkflowWaitHumanGate || change.OpenWait.ID == "" || change.OpenWait.NodeRunID == "" {
		t.Fatalf("change open wait = %+v, want the persisted human gate identity", change.OpenWait)
	}
	wait := *change.OpenWait
	path := "/v2/changes/" + changeID + "/review"
	assertNoVerdictWrites := func() {
		t.Helper()
		var threads, checks int
		if err := fixture.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threads); err != nil {
			t.Fatalf("count review threads: %v", err)
		}
		if err := fixture.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checks); err != nil {
			t.Fatalf("count review checks: %v", err)
		}
		if threads != 0 || checks != 0 {
			t.Fatalf("rejected verdict persisted threads=%d checks=%d, want neither", threads, checks)
		}
		current, currentWaiting, currentErr := fixture.Bundle.WorkflowRuns.OpenWait(context.Background(), created.Task.ID)
		if currentErr != nil || !currentWaiting || current.ID != wait.ID || current.NodeRunID != wait.NodeRunID {
			t.Fatalf("review wait after rejected verdict = %+v waiting=%t err=%v, want unchanged %+v", current, currentWaiting, currentErr, wait)
		}
	}

	for _, reviewWaitID := range []string{"", "   "} {
		var response errorResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, reviewVerdictRequest{
			Verdict:      "approve",
			HeadSHA:      headSHA,
			NodeRunID:    wait.NodeRunID,
			ReviewWaitID: reviewWaitID,
			Comments:     []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
		}, http.StatusBadRequest, &response)
		if response.Error.Code != "review_wait_id_required" {
			t.Fatalf("missing review wait ID error = %+v, want review_wait_id_required", response.Error)
		}
		assertNoVerdictWrites()
	}

	var stale errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, reviewVerdictRequest{
		Verdict:      "approve",
		HeadSHA:      headSHA,
		NodeRunID:    wait.NodeRunID,
		ReviewWaitID: "rw-stale",
		Comments:     []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
	}, http.StatusConflict, &stale)
	if stale.Error.Code != "workflow_conflict" {
		t.Fatalf("stale review wait error = %+v, want workflow_conflict", stale.Error)
	}
	assertNoVerdictWrites()

	var approved reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path,
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
		}), http.StatusOK, &approved)
	if approved.Check == nil || approved.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("approved review check = %+v, want satisfied", approved.Check)
	}
}

func TestSubmitReviewCommentLeavesHumanGateWaiting(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	// 400 invalid_request response, before any thread or verdict check can be
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
		if resp.Error.Code != "invalid_request" {
			t.Fatalf("empty head (%q): error code = %q, want invalid_request", submittedHead, resp.Error.Code)
		}
		if resp.Error.Message != "head_sha is required" {
			t.Fatalf("empty head (%q): error message = %q, want %q", submittedHead, resp.Error.Message, "head_sha is required")
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
	t.Parallel()
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
	// submission landed: the server must refuse the whole review with a genuine
	// head mismatch conflict (as opposed to the 400 invalid_request a missing
	// head_sha gets).
	var conflict errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  "2222222222222222222222222222222222222222",
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "comment on inspected code"}},
		}, http.StatusConflict, &conflict)
	if conflict.Error.Code != "head_moved" {
		t.Fatalf("stale-head conflict code = %q, want head_moved", conflict.Error.Code)
	}

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

// TestSubmitReviewLateForDecidedGatePersistsNothing guards the atomic review
// submission against a gate that rejects the verdict: a verdict that lands
// after the task's human gate already recorded a real decision — the run
// answered the gate and moved on, so no gate is waiting for the new verdict —
// is late and contradictory. The submission must 409 without persisting its
// inline threads or overwriting the recorded human-review check. The prior
// decision is made through the real review endpoint (not by flipping node
// state), so the run advances exactly as it does in production: once the
// first approval commits, the run is completed and respondToReviewGateTx
// finds no open gate, which used to let the late verdict commit its threads
// and check beside the decision it contradicts.
func TestSubmitReviewLateForDecidedGatePersistsNothing(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review gate conflict")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Contradictory review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-gate-conflict"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-gate-conflict",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// A first reviewer approves: the inline thread and the human-review check
	// are filed, the gate records the approved decision, and the run completes
	// — the real state a later verdict finds when it arrives after the gate
	// was already answered.
	var first reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "looks good"}},
		}), http.StatusOK, &first)
	if first.Check == nil || first.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("first review check = %+v, want satisfied", first.Check)
	}

	// A second, late reviewer requests changes after the gate already recorded
	// the approved decision: the verdict contradicts the decision the run moved
	// on from, so the submission must be refused wholesale with 409.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 2, Body: "inline note on a contradictory review"}},
		}, http.StatusConflict, nil)

	// The refused verdict persisted nothing: only the first review's thread
	// exists, and the human-review check still records the approved decision
	// instead of being overwritten with the blocked request_changes verdict.
	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 1 {
		t.Fatalf("late review left %d threads, want only the first review's thread", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 1 {
		t.Fatalf("late review left %d human-review checks, want only the first review's check", checkCount)
	}
	var checkVerdict string
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT verdict FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkVerdict); err != nil {
		t.Fatalf("load check verdict: %v", err)
	}
	if checkVerdict != string(coordinator.CheckSatisfied) {
		t.Fatalf("check verdict = %q, want the recorded approved decision to stand", checkVerdict)
	}

	// The recorded decision stands: the gate keeps its approved outcome and the
	// run stays completed rather than being moved by the refused verdict.
	var gateOutcome string
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT outcome FROM workflow_node_runs WHERE id = ?`, scheduled.Run.CurrentNodeRunID).Scan(&gateOutcome); err != nil {
		t.Fatalf("load gate outcome: %v", err)
	}
	if gateOutcome != "approved" {
		t.Fatalf("gate outcome = %q, want the recorded approved decision to stand", gateOutcome)
	}
	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("workflow state = %q, want completed after the refused late review", run.State)
	}
}

// TestSubmitReviewGateConflictPersistsNothing guards the atomic review
// submission against a gate that rejects the verdict: when a contradictory or
// late decision arrives (the gate already recorded the opposite outcome while
// the run still appears to wait on it), the submission must 409 with the
// workflow_conflict error code without persisting the inline threads or the
// human-review check. The error-code assertion keeps the test pinned to the
// gate-conflict path: the stale-head path also maps to 409, so a future
// refactor that failed this submission with head_moved before any gate check
// would otherwise pass vacuously. Before the review became one transaction the
// handler filed threads first and only then answered the gate, so a rejected
// verdict left orphaned threads beside a decision that was never recorded.
func TestSubmitReviewGateConflictPersistsNothing(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review gate conflict")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Contradictory review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-gate-conflict"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-gate-conflict",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// The workflow is waiting at the human gate, but the gate has already
	// recorded the opposite decision (approved) — the state a contradictory or
	// late review finds when it arrives after the decision was made but while
	// the run still appears to wait on it. The submission must be refused with
	// workflow_conflict (not the head_moved 409) without leaving its threads or
	// verdict check behind.
	if _, err := fixture.DB.ExecContext(context.Background(), `
UPDATE workflow_node_runs SET state = ?, outcome = ?, completed_at = ? WHERE id = ?`,
		string(coordinator.WorkflowNodeSucceeded), "approved", timestamp, scheduled.Run.CurrentNodeRunID); err != nil {
		t.Fatalf("record contradictory gate decision: %v", err)
	}

	var resp errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "inline note on a contradictory review"}},
		}), http.StatusConflict, &resp)
	if resp.Error.Code != "workflow_conflict" {
		t.Fatalf("error code = %q, want workflow_conflict", resp.Error.Code)
	}

	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("gate-rejected review created %d threads, want 0", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("gate-rejected review recorded %d human-review checks, want 0", checkCount)
	}

	// The contradictory decision stands: the gate keeps its recorded outcome
	// and the run stays waiting on it.
	var gateOutcome string
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT outcome FROM workflow_node_runs WHERE id = ?`, scheduled.Run.CurrentNodeRunID).Scan(&gateOutcome); err != nil {
		t.Fatalf("load gate outcome: %v", err)
	}
	if gateOutcome != "approved" {
		t.Fatalf("gate outcome = %q, want the recorded approved decision to stand", gateOutcome)
	}
	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a refused contradictory review", run.State)
	}
}

// TestSubmitReviewLateAfterFinalGatePassedPersistsNothing pins the passed-
// final-gate case: the flow is gate -> work -> terminal, so approving the gate
// records the flow's only decision and moves the run to the work phase, which
// is active but has no reachable future or revisited human gate. A late
// request_changes verdict arriving there must be refused with 409 and persist
// no inline thread and no check — the run is still active, which used to make
// humanGateDecidedTx treat every active run as still holding a valid gate
// ahead and let the contradictory verdict commit beside the decision it
// contradicts.
func TestSubmitReviewLateAfterFinalGatePassedPersistsNothing(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newFinalGateFixtureFlow(t, fixture, "change review passed final gate")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Late review after the final gate", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-passed-final-gate"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-passed-final-gate",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// A first reviewer approves: the thread and the satisfied check are filed,
	// the gate records the approved decision, and the run advances past its
	// only gate to the work phase — still active, but with no gate left ahead.
	var first reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "final gate approved"}},
		}), http.StatusOK, &first)
	if first.Check == nil || first.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("first review check = %+v, want satisfied", first.Check)
	}
	run, err := fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "work" {
		t.Fatalf("workflow = state %q at %q, want the run active at work after the approval", run.State, run.CurrentNodeKey)
	}
	gateNodeRunID := scheduled.Run.CurrentNodeRunID
	if gateNodeRunID == "" {
		t.Fatalf("scheduled run = %+v, want a current gate node run", scheduled.Run)
	}

	// A late reviewer requests changes while the run works toward the terminal:
	// the gate already recorded the only decision this flow will ever record,
	// so the verdict is contradictory and the submission must be refused
	// wholesale with 409, leaving no thread and no check behind.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 2, Body: "inline note contradicting the approved gate"}},
		}, http.StatusConflict, nil)

	// The refused verdict persisted nothing: only the first review's thread
	// exists, and the human-review check still records the approved decision
	// instead of being overwritten with the blocked request_changes verdict.
	var threadCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 1 {
		t.Fatalf("late review left %d threads, want only the first review's thread", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 1 {
		t.Fatalf("late review left %d human-review checks, want only the first review's check", checkCount)
	}
	var checkVerdict string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT verdict FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkVerdict); err != nil {
		t.Fatalf("load check verdict: %v", err)
	}
	if checkVerdict != string(coordinator.CheckSatisfied) {
		t.Fatalf("check verdict = %q, want the recorded approved decision to stand", checkVerdict)
	}

	// The recorded decision and the run both stand: the gate keeps its approved
	// outcome and the run stays active at work rather than being moved by the
	// refused verdict.
	var gateOutcome string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT outcome FROM workflow_node_runs WHERE id = ?`, gateNodeRunID).Scan(&gateOutcome); err != nil {
		t.Fatalf("load gate outcome: %v", err)
	}
	if gateOutcome != "approved" {
		t.Fatalf("gate outcome = %q, want the recorded approved decision to stand", gateOutcome)
	}
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "work" {
		t.Fatalf("workflow = state %q at %q, want the run untouched at work", run.State, run.CurrentNodeKey)
	}
}

// TestSubmitReviewMismatchedCommentAnchorRejected guards the review-integrity
// invariant against a hand-crafted request: the submission's head validates
// against the change's current head, but a per-comment anchor that names a
// different commit must be refused with a clear 400 rather than binding the
// thread to an arbitrary or older commit. The web client never sends a
// per-comment anchor, so this is API-surface hardening.
func TestSubmitReviewMismatchedCommentAnchorRejected(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// doRawJSONRequestAs sends a raw JSON body — e.g. one that omits a field the
// request struct cannot omit when encoded — and asserts the response status.
func doRawJSONRequestAs(t *testing.T, server *Server, token string, method string, path string, body string, wantStatus int, target any) {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

// TestSubmitReviewMissingHeadSHABadRequest guards the fail-closed distinction
// between a malformed request and a moved head: a review submission without a
// head_sha must be rejected with 400 invalid_request (nothing moved), not
// with the 409 head_moved conflict that a genuine mismatch gets, and must
// create nothing either way.
func TestSubmitReviewMissingHeadSHABadRequest(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review missing head")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Review without head from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-missing-head"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-missing-head",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// A request that simply omits the field (or sends blank whitespace) is
	// malformed: 400 invalid_request with a head-sha-required message, never
	// the misleading 409 head_moved.
	for _, missing := range []string{"", "   "} {
		var bad errorResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
			reviewVerdictRequest{
				Verdict:  "approve",
				HeadSHA:  missing,
				Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "comment on inspected code"}},
			}, http.StatusBadRequest, &bad)
		if bad.Error.Code != "invalid_request" {
			t.Fatalf("missing head_sha %q error code = %q, want invalid_request", missing, bad.Error.Code)
		}
		if bad.Error.Message != "head_sha is required" {
			t.Fatalf("missing head_sha %q error message = %q, want %q", missing, bad.Error.Message, "head_sha is required")
		}
	}

	// The struct form above always serializes head_sha: even when the field is
	// empty it encodes as "head_sha":"" because the tag has no omitempty, so
	// the truly omitted-field payload must be sent as raw JSON without the key.
	// It must be refused exactly like the empty/whitespace values.
	var omitted errorResponse
	doRawJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		`{"verdict":"approve","comments":[{"file_path":"a.go","line":1,"body":"comment on inspected code"}]}`,
		http.StatusBadRequest, &omitted)
	if omitted.Error.Code != "invalid_request" {
		t.Fatalf("omitted head_sha error code = %q, want invalid_request", omitted.Error.Code)
	}
	if omitted.Error.Message != "head_sha is required" {
		t.Fatalf("omitted head_sha error message = %q, want %q", omitted.Error.Message, "head_sha is required")
	}

	var threadCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("missing-head review created %d threads, want 0", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("missing-head review recorded %d human-review checks, want 0", checkCount)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(context.Background(), scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow state = %q, want still waiting after a refused missing-head review", run.State)
	}
}

// TestSubmitReviewMultiGatePreservesWaitingGateVerdict pins the gate-scoped
// conflict check: a workflow with two human gates reaches its second gate
// while the first gate's decision is already recorded. A verdict for the
// second gate is not late — it is the gate the run is currently waiting on —
// so it must succeed, file its threads and check, and answer that gate,
// despite the earlier decision on the first gate. Before the fix,
// humanGateDecidedTx treated any succeeded human-gate node run in the latest
// run as a prior decision and refused the second-gate verdict with 409,
// dropping its threads and check even though the relevant gate had not been
// reached yet.
func TestSubmitReviewMultiGatePreservesWaitingGateVerdict(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newMultiGateFixtureFlow(t, fixture, "multi-gate change review")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Multi-gate review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-multi-gate"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-multi-gate",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// First gate: a real approval records a decision on gate1 and moves the run
	// to the work2 phase between the two gates.
	var first reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "first gate looks good"}},
		}), http.StatusOK, &first)
	if first.Check == nil || first.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("first review check = %+v, want satisfied", first.Check)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "work2" {
		t.Fatalf("workflow = state %q at %q, want running at work2 after the first approval", run.State, run.CurrentNodeKey)
	}
	work2NodeRunID := run.CurrentNodeRunID
	if work2NodeRunID == "" {
		t.Fatalf("work2 has no open node run: %+v", run)
	}

	// A verdict while the run is working toward the second gate must not be
	// refused just because gate1's decision is recorded: gate2 has not been
	// reached, so the verdict files its threads and check and leaves the run
	// where it is. Before the fix, humanGateDecidedTx counted gate1's decision
	// and rejected this verdict with 409, dropping its threads and check even
	// though the relevant gate was still ahead.
	var second reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 2, Body: "notes for the second gate"}},
		}, http.StatusOK, &second)
	if second.Check == nil || second.Check.Verdict != coordinator.CheckBlocked {
		t.Fatalf("second review check = %+v, want blocked for request_changes", second.Check)
	}
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "work2" {
		t.Fatalf("workflow = state %q at %q, want the run untouched at work2", run.State, run.CurrentNodeKey)
	}

	// The run reaches gate2; a verdict for the waiting second gate is answered
	// and completes the run.
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, work2NodeRunID); err != nil {
		t.Fatalf("mark work2 node running: %v", err)
	}
	artifactID := submitHandoffArtifact(t, fixture, created.Task.ID, work2NodeRunID, "work2-v1", "Revised for second review")
	completeAgentNode(t, fixture, created.Task.ID, work2NodeRunID, artifactID)
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting || run.CurrentNodeKey != "gate2" {
		t.Fatalf("workflow = state %q at %q, want waiting at gate2 after work2", run.State, run.CurrentNodeKey)
	}
	var third reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 3, Body: "second gate approved"}},
		}), http.StatusOK, &third)
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("workflow state = %q, want completed after the second approval", run.State)
	}

	// The run is finished now, so a further verdict is late and refused.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 4, Body: "late contradiction"}},
		}, http.StatusConflict, nil)

	// The two successful verdicts' threads survive and the refused verdict left
	// none: three threads total. The human-review check is a single per-task
	// row whose verdict reflects the last successful verdict, and both gates
	// keep their recorded decisions.
	var threadCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 3 {
		t.Fatalf("multi-gate reviews left %d threads, want 3 (the late verdict must leave none)", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 1 {
		t.Fatalf("multi-gate reviews left %d human-review checks, want the single per-task check", checkCount)
	}
	var checkVerdict string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT verdict FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkVerdict); err != nil {
		t.Fatalf("load check verdict: %v", err)
	}
	if checkVerdict != string(coordinator.CheckSatisfied) {
		t.Fatalf("check verdict = %q, want satisfied after the final approval", checkVerdict)
	}
	var gate1Outcome, gate2Outcome string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT outcome FROM workflow_node_runs WHERE workflow_run_id = ? AND node_key = 'gate1' AND state = ?`,
		scheduled.Run.ID, string(coordinator.WorkflowNodeSucceeded)).Scan(&gate1Outcome); err != nil {
		t.Fatalf("load gate1 outcome: %v", err)
	}
	if gate1Outcome != "approved" {
		t.Fatalf("gate1 outcome = %q, want approved", gate1Outcome)
	}
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT outcome FROM workflow_node_runs WHERE workflow_run_id = ? AND node_key = 'gate2' AND state = ?`,
		scheduled.Run.ID, string(coordinator.WorkflowNodeSucceeded)).Scan(&gate2Outcome); err != nil {
		t.Fatalf("load gate2 outcome: %v", err)
	}
	if gate2Outcome != "approved" {
		t.Fatalf("gate2 outcome = %q, want approved", gate2Outcome)
	}
}

// TestSubmitReviewRevisitAfterChangesRequestedPreservesVerdict pins the
// gate-scoped conflict check across a changes_requested loop: after a real
// revision cycle the run revisits the same human gate and waits there again.
// A verdict for that revisited gate is not late even though an earlier visit
// of the same gate recorded a decision; it must succeed, file its threads and
// check, and answer the gate. Before the fix, humanGateDecidedTx counted any
// succeeded human-gate node run in the latest run as a prior decision and
// refused the revisited-gate verdict with 409, dropping its threads and check
// even though the relevant visit was still open.
func TestSubmitReviewRevisitAfterChangesRequestedPreservesVerdict(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newRevisitFixtureFlow(t, fixture, "change review revisit")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Revisited gate review from change view", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-revisit"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-revisit",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	// First visit: request changes records a decision on the gate and sends the
	// run back through the rework phase toward the same gate again.
	var first reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 1, Body: "revise this"}},
		}), http.StatusOK, &first)
	if first.Check == nil || first.Check.Verdict != coordinator.CheckBlocked {
		t.Fatalf("first review check = %+v, want blocked for request_changes", first.Check)
	}

	run, err := fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "rework" {
		t.Fatalf("workflow = state %q at %q, want running at rework after changes_requested", run.State, run.CurrentNodeKey)
	}
	reworkNodeRunID := run.CurrentNodeRunID
	if reworkNodeRunID == "" {
		t.Fatalf("rework has no open node run: %+v", run)
	}

	// A verdict for the new revision while the run is back in the rework phase
	// must not be refused just because the earlier visit of the same gate is
	// decided: the revisited gate has not been reached yet. Before the fix,
	// humanGateDecidedTx counted the earlier visit's decision and rejected this
	// verdict with 409, dropping its threads and check.
	var second reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 2, Body: "approved on revision"}},
		}, http.StatusOK, &second)
	if second.Check == nil || second.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("second review check = %+v, want satisfied", second.Check)
	}
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunRunning || run.CurrentNodeKey != "rework" {
		t.Fatalf("workflow = state %q at %q, want the run untouched at rework", run.State, run.CurrentNodeKey)
	}

	// The revision lands and the run revisits the gate; the new visit is
	// answered and completes the run.
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, reworkNodeRunID); err != nil {
		t.Fatalf("mark rework node running: %v", err)
	}
	artifactID := submitHandoffArtifact(t, fixture, created.Task.ID, reworkNodeRunID, "rework-v1", "Revised draft")
	completeAgentNode(t, fixture, created.Task.ID, reworkNodeRunID, artifactID)
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunWaiting || run.CurrentNodeKey != "gate" || run.CurrentNodeRunID == scheduled.Run.CurrentNodeRunID {
		t.Fatalf("workflow = %+v, want a fresh visit waiting at the revisited gate", run)
	}
	revisitNodeRunID := run.CurrentNodeRunID
	var third reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{
			Verdict:  "approve",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 3, Body: "final approval"}},
		}), http.StatusOK, &third)
	run, err = fixture.Bundle.WorkflowRuns.Get(ctx, scheduled.Run.ID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("workflow state = %q, want completed after the revisited approval", run.State)
	}

	// The finished run's decisions are stale: a further verdict is refused.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewVerdictRequest{
			Verdict:  "request_changes",
			HeadSHA:  headSHA,
			Comments: []reviewInlineComment{{FilePath: "a.go", Line: 4, Body: "late contradiction"}},
		}, http.StatusConflict, nil)

	// Three threads total (the refused verdict left none), the single per-task
	// human-review check reflects the last successful verdict, and both visits
	// keep their recorded decisions.
	var threadCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, changeID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 3 {
		t.Fatalf("revisit reviews left %d threads, want 3 (the late verdict must leave none)", threadCount)
	}
	var checkCount int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 1 {
		t.Fatalf("revisit reviews left %d human-review checks, want the single per-task check", checkCount)
	}
	var checkVerdict string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT verdict FROM checks WHERE task_id = ? AND name = ?`, created.Task.ID, humanReviewCheckName).Scan(&checkVerdict); err != nil {
		t.Fatalf("load check verdict: %v", err)
	}
	if checkVerdict != string(coordinator.CheckSatisfied) {
		t.Fatalf("check verdict = %q, want satisfied after the final approval", checkVerdict)
	}
	var visit1Outcome, visit2Outcome string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT outcome FROM workflow_node_runs WHERE id = ?`, scheduled.Run.CurrentNodeRunID).Scan(&visit1Outcome); err != nil {
		t.Fatalf("load first-visit outcome: %v", err)
	}
	if visit1Outcome != "changes_requested" {
		t.Fatalf("first visit outcome = %q, want changes_requested", visit1Outcome)
	}
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT outcome FROM workflow_node_runs WHERE id = ?`, revisitNodeRunID).Scan(&visit2Outcome); err != nil {
		t.Fatalf("load revisited outcome: %v", err)
	}
	if visit2Outcome != "approved" {
		t.Fatalf("revisited outcome = %q, want approved", visit2Outcome)
	}
}

// TestSubmitReviewResponseCarriesTransactionCheck pins the response-fidelity
// invariant from the t-flow-0073 advisory: the submit-review response must
// carry the exact check row the gate decision transaction upserted, not a
// post-commit reload that a concurrent report for the same check name could
// overwrite. SubmitReview captures the row reportCheckTx returns inside its
// own transaction and handleSubmitReview never re-reads the check, so the
// response is a snapshot of that upsert: it matches the committed row field
// for field, and a later report that overwrites the row does not change what
// the response already carries.
func TestSubmitReviewResponseCarriesTransactionCheck(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "change review check fidelity")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Check fidelity review", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	const (
		changeID  = "ch-change-review-check-fidelity"
		headSHA   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, created.Task.ID, "task/change-review-check-fidelity",
		headSHA, timestamp, timestamp, timestamp); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	const body = "this verdict was written inside the gate decision transaction"
	var review reviewVerdictResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/review",
		reviewGateRequest(t, fixture, created.Task.ID, reviewVerdictRequest{Verdict: "approve", HeadSHA: headSHA, Body: body}), http.StatusOK, &review)
	if review.Check == nil {
		t.Fatal("review check = nil, want the transaction-upserted check in the response")
	}

	// The response carries the exact row the decision transaction upserted: it
	// matches the committed row field for field (a post-commit reload could
	// diverge if a concurrent report landed in the gap between commit and read).
	committed, err := fixture.Checks.GetCheck(context.Background(), created.Task.ID, humanReviewCheckName)
	if err != nil {
		t.Fatalf("load committed check: %v", err)
	}
	if review.Check.ID != committed.ID || review.Check.TaskID != committed.TaskID ||
		review.Check.Name != committed.Name || review.Check.Kind != committed.Kind ||
		review.Check.Required != committed.Required || review.Check.Verdict != committed.Verdict ||
		review.Check.Details != committed.Details || review.Check.Reporter != committed.Reporter ||
		review.Check.ExitCode != nil || committed.ExitCode != nil ||
		review.Check.SourceJobID != nil || committed.SourceJobID != nil ||
		!review.Check.CreatedAt.Equal(committed.CreatedAt) || !review.Check.UpdatedAt.Equal(committed.UpdatedAt) {
		t.Fatalf("response check = %+v, committed check = %+v, want the exact row the transaction upserted", review.Check, committed)
	}
	if review.Check.Details != body {
		t.Fatalf("response check details = %q, want the submitted verdict body %q", review.Check.Details, body)
	}

	// A concurrent report that commits after the decision overwrites the row;
	// the response still carries the transaction's own snapshot, proving the
	// check was not reloaded after the commit.
	required := true
	if _, err := fixture.Checks.ReportCheck(context.Background(), coordinator.ReportCheckInput{
		TaskID:   created.Task.ID,
		Name:     humanReviewCheckName,
		Kind:     coordinator.CheckKindHuman,
		Required: &required,
		Verdict:  coordinator.CheckSatisfied,
		Details:  "a later report overwrote the row",
		Reporter: "later-report",
	}); err != nil {
		t.Fatalf("report later check: %v", err)
	}
	if review.Check.Details != body {
		t.Fatalf("response check details = %q, want the transaction's %q after the row was overwritten", review.Check.Details, body)
	}
	after, err := fixture.Checks.GetCheck(context.Background(), created.Task.ID, humanReviewCheckName)
	if err != nil {
		t.Fatalf("load overwritten check: %v", err)
	}
	if after.Details != "a later report overwrote the row" {
		t.Fatalf("committed details after later report = %q, want the later report's row", after.Details)
	}
}
