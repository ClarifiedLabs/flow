package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestWorkflowModelV2HumanGateLifecycle(t *testing.T) {
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

	var created issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/issues",
		createIssueRequest{Title: "Release v2", FlowID: flow.ID}, http.StatusCreated, &created, protocolHeader, "2")
	if created.Issue.State != nil {
		t.Fatalf("created issue state = %v, want Unscheduled/null", created.Issue.State)
	}

	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/issues/"+created.Issue.ID+"/schedule",
		nil, http.StatusOK, &scheduled, protocolHeader, "2")
	if scheduled.Run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("scheduled workflow state = %q, want waiting", scheduled.Run.State)
	}

	var detail workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/issues/"+created.Issue.ID+"/workflow",
		nil, http.StatusOK, &detail, protocolHeader, "2")
	if detail.Detail.Substate != coordinator.InProgressBlocked || detail.Detail.OpenWait == nil {
		t.Fatalf("workflow detail = %+v, want In Progress / Blocked", detail.Detail)
	}

	var completed coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/issues/"+created.Issue.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: scheduled.Run.CurrentNodeRunID, Outcome: "ship"}, http.StatusOK, &completed, protocolHeader, "2")
	if !completed.Done || completed.Run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("completed workflow = %+v", completed)
	}

	var done issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/issues/"+created.Issue.ID,
		nil, http.StatusOK, &done, protocolHeader, "2")
	if done.Issue.State == nil || *done.Issue.State != coordinator.LifecycleDone || done.Issue.DoneResolution == nil || *done.Issue.DoneResolution != coordinator.ResolutionCompleted {
		t.Fatalf("done issue = %+v", done.Issue)
	}
}
