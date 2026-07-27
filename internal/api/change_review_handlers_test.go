package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
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
		reviewVerdictRequest{Verdict: "approve"}, http.StatusOK, &review)
	if review.Check == nil || review.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("review check = %+v, want satisfied", review.Check)
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
