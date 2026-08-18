package coordinator

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func seedOrganizerFlowForTest(t *testing.T, flows *FlowService) {
	t.Helper()
	ctx := context.Background()
	organizerDef, err := flows.agentDefs.Create(ctx, AgentDefInput{Name: "organizer-test", Harness: "harness", Prompt: "Organize every proposal."})
	if err != nil {
		t.Fatal(err)
	}
	child, err := flows.Create(ctx, FlowInput{
		Name: "organizer-child-test", StartNode: "done",
		Nodes: []FlowNodeInput{{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reviewFollowUpOrganizerFlowInput(map[string]string{"review-follow-up-organizer": organizerDef.ID}, child.ID)
	if _, err := flows.Create(ctx, input); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFollowUpOrganizerReconcilerCreatesOneDurablePlanningTask(t *testing.T) {
	ctx := context.Background()
	f := newScopeDecisionFixture(t)
	seedOrganizerFlowForTest(t, f.runs.flows)
	tasks := NewTaskService(f.runs.db, "p-test")
	accepted, err := tasks.ApplyReviewFollowUpBatch(ctx, batchInput(f, followUpBatchReport(f.change.HeadSHA, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.MarkReviewFollowUpOrganizerPending(ctx, f.task.ID, f.change.ID, f.run.ID); err != nil {
		t.Fatal(err)
	}
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{Database: f.runs.db, Runs: f.runs, Tasks: tasks})
	if err := executor.ReconcileReviewFollowUpOrganizers(ctx); err != nil {
		t.Fatal(err)
	}

	var state, organizerTaskID, planState, planTaskID, organizerRunID string
	var revision int
	if err := f.runs.db.QueryRow(`
SELECT s.state, s.revision, COALESCE(s.organizer_task_id,''), pr.state,
       COALESCE(pr.organizer_task_id,''), COALESCE(pr.organizer_workflow_run_id,'')
FROM review_follow_up_sets s
JOIN review_follow_up_plan_revisions pr ON pr.set_id = s.id AND pr.set_revision = s.revision
WHERE s.id = ?`, accepted.SetID).Scan(&state, &revision, &organizerTaskID, &planState, &planTaskID, &organizerRunID); err != nil {
		t.Fatal(err)
	}
	if state != "organizing" || revision != 1 || organizerTaskID == "" || planTaskID != organizerTaskID ||
		planState != "organizing" || organizerRunID == "" {
		t.Fatalf("set/plan = state %q rev %d task %q / state %q task %q run %q", state, revision, organizerTaskID, planState, planTaskID, organizerRunID)
	}
	organizerTask, err := tasks.GetTask(ctx, organizerTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if organizerTask.CreatedBy != ActorSystem || organizerTask.CreatedBySessionID != nil ||
		organizerTask.SourceTaskID == nil || *organizerTask.SourceTaskID != f.task.ID ||
		organizerTask.SourceChangeID == nil || *organizerTask.SourceChangeID != f.change.ID ||
		!strings.Contains(organizerTask.Body, accepted.SetID) || !strings.Contains(organizerTask.Body, "active_proposals") {
		t.Fatalf("organizer task = %+v", organizerTask)
	}
	flow, err := f.runs.flows.Get(ctx, organizerTask.FlowID)
	if err != nil {
		t.Fatal(err)
	}
	if flow.Name != ReviewFollowUpOrganizerFlowName {
		t.Fatalf("organizer flow = %q", flow.Name)
	}
	if run, active, err := f.runs.ActiveForTask(ctx, organizerTaskID); err != nil || !active || run.ID != organizerRunID {
		t.Fatalf("active organizer run = %+v, active=%v err=%v", run, active, err)
	}

	if err := executor.ReconcileReviewFollowUpOrganizers(ctx); err != nil {
		t.Fatal(err)
	}
	var taskCount, planCount, runCount int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE source_task_id = ? AND source_change_id = ? AND created_by = 'system'`, f.task.ID, f.change.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_plan_revisions WHERE set_id = ?`, accepted.SetID).Scan(&planCount); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, organizerTaskID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || planCount != 1 || runCount != 1 {
		t.Fatalf("tasks/plans/runs after replay = %d/%d/%d, want 1/1/1", taskCount, planCount, runCount)
	}
}

func TestReviewFollowUpOrganizerReconcilerHealsApprovedTransitionMarker(t *testing.T) {
	ctx := context.Background()
	f := newScopeDecisionFixture(t)
	seedOrganizerFlowForTest(t, f.runs.flows)
	tasks := NewTaskService(f.runs.db, "p-test")
	accepted, err := tasks.ApplyReviewFollowUpBatch(ctx, batchInput(f, followUpBatchReport(f.change.HeadSHA, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runs.db.Exec(`UPDATE workflow_node_runs SET state = 'succeeded', outcome = 'approved', completed_at = created_at WHERE id = ?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{Database: f.runs.db, Runs: f.runs, Tasks: tasks})
	if err := executor.ReconcileReviewFollowUpOrganizers(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.runs.db.QueryRow(`SELECT state FROM review_follow_up_sets WHERE id = ?`, accepted.SetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "organizing" {
		t.Fatalf("healed set state = %q, want organizing", state)
	}
}

func TestReviewFollowUpOrganizerFailureDoesNotChangeSourceTask(t *testing.T) {
	ctx := context.Background()
	f := newScopeDecisionFixture(t)
	tasks := NewTaskService(f.runs.db, "p-test")
	accepted, err := tasks.ApplyReviewFollowUpBatch(ctx, batchInput(f, followUpBatchReport(f.change.HeadSHA, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.MarkReviewFollowUpOrganizerPending(ctx, f.task.ID, f.change.ID, f.run.ID); err != nil {
		t.Fatal(err)
	}
	// Remove the dedicated flow to force an organizer-only failure.
	if _, err := f.runs.db.Exec(`DELETE FROM flows WHERE name = ?`, ReviewFollowUpOrganizerFlowName); err != nil {
		t.Fatal(err)
	}
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{Database: f.runs.db, Runs: f.runs, Tasks: tasks})
	if err := executor.ReconcileReviewFollowUpOrganizers(ctx); err == nil {
		t.Fatal("expected organizer reconciliation error")
	}
	var setState string
	if err := f.runs.db.QueryRow(`SELECT state FROM review_follow_up_sets WHERE id = ?`, accepted.SetID).Scan(&setState); err != nil {
		t.Fatal(err)
	}
	if setState != "attention" {
		t.Fatalf("set state = %q, want attention", setState)
	}
	var sourceState sql.NullString
	if err := f.runs.db.QueryRow(`SELECT lifecycle_state FROM tasks WHERE id = ?`, f.task.ID).Scan(&sourceState); err != nil {
		t.Fatal(err)
	}
	if !sourceState.Valid || sourceState.String != string(LifecycleInProgress) {
		t.Fatalf("source task state = %+v, want unchanged in_progress", sourceState)
	}

	// Repair the transient setup dependency. Attention sets without an active
	// organizer task are retried on the next reconciliation tick.
	seedOrganizerFlowForTest(t, f.runs.flows)
	if err := executor.ReconcileReviewFollowUpOrganizers(ctx); err != nil {
		t.Fatalf("retry organizer reconciliation: %v", err)
	}
	var organizerTaskID, lastError string
	if err := f.runs.db.QueryRow(`SELECT state, COALESCE(organizer_task_id,''), last_error FROM review_follow_up_sets WHERE id = ?`, accepted.SetID).
		Scan(&setState, &organizerTaskID, &lastError); err != nil {
		t.Fatal(err)
	}
	if setState != "organizing" || organizerTaskID == "" || lastError != "" {
		t.Fatalf("retried set = state %q task %q error %q", setState, organizerTaskID, lastError)
	}
}

func TestReviewFollowUpOrganizerPropagatesStalePlanWriteFailure(t *testing.T) {
	ctx := context.Background()
	f := newScopeDecisionFixture(t)
	seedOrganizerFlowForTest(t, f.runs.flows)
	tasks := NewTaskService(f.runs.db, "p-test")
	accepted, err := tasks.ApplyReviewFollowUpBatch(ctx, batchInput(f, followUpBatchReport(f.change.HeadSHA, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runs.db.Exec(`
CREATE TRIGGER fail_stale_review_follow_up_plan
BEFORE UPDATE OF state ON review_follow_up_plan_revisions
WHEN NEW.state = 'stale'
BEGIN SELECT RAISE(ABORT, 'forced stale plan failure'); END`); err != nil {
		t.Fatal(err)
	}
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{Database: f.runs.db, Runs: f.runs, Tasks: tasks})
	err = executor.reconcileReviewFollowUpOrganizer(ctx, reviewFollowUpOrganizerSet{
		ID: accepted.SetID, SourceTaskID: f.task.ID, SourceChangeID: f.change.ID,
		WorkflowRunID: f.run.ID, Revision: accepted.SetRevision,
	})
	if err == nil || !strings.Contains(err.Error(), "forced stale plan failure") {
		t.Fatalf("stale plan write error = %v", err)
	}
	var stalePlans int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_plan_revisions WHERE set_id = ? AND state = 'stale'`, accepted.SetID).Scan(&stalePlans); err != nil {
		t.Fatal(err)
	}
	if stalePlans != 0 {
		t.Fatalf("stale plans after failed state write = %d, want 0", stalePlans)
	}
}
