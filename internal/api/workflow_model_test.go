package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestWorkflowModelV2HumanGateLifecycle(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name:      "release decision v2",
		StartNode: "decision",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "decision", Name: "Release decision", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Outcomes: []string{"ship", "stop"}}}},
			{Key: "shipped", Name: "Shipped", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "stopped", Name: "Stopped", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCancelled}}},
		},
		Edges: []coordinator.FlowEdgeInput{
			{From: "decision", Outcome: "ship", To: "shipped"},
			{From: "decision", Outcome: "stop", To: "stopped"},
		},
	})
	if err != nil {
		t.Fatalf("create v2 workflow: %v", err)
	}

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Release v2", FlowID: flow.ID}, http.StatusCreated, &created)
	if created.Task.State != nil {
		t.Fatalf("created task state = %v, want Unscheduled/null", created.Task.State)
	}

	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)
	if scheduled.Run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("scheduled workflow state = %q, want waiting", scheduled.Run.State)
	}

	var detail workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/workflow",
		nil, http.StatusOK, &detail)
	if detail.Detail.Substate != coordinator.InProgressBlocked || detail.Detail.OpenWait == nil {
		t.Fatalf("workflow detail = %+v, want In Progress / Blocked", detail.Detail)
	}

	var completed coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: scheduled.Run.CurrentNodeRunID, ReviewWaitID: detail.Detail.OpenWait.ID, Outcome: "ship"}, http.StatusOK, &completed)
	if !completed.Done || completed.Run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("completed workflow = %+v", completed)
	}

	var done taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID,
		nil, http.StatusOK, &done)
	if done.Task.State == nil || *done.Task.State != coordinator.LifecycleDone || done.Task.DoneResolution == nil || *done.Task.DoneResolution != coordinator.ResolutionCompleted {
		t.Fatalf("done task = %+v", done.Task)
	}
}
