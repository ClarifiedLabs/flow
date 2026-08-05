package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// newReviewFixtureFlow builds the planning shape: a task_set agent node, a
// human review gate with the three classic outcomes, and the loop back.
func newReviewFixtureFlow(t *testing.T, fixture testFixture, name string) coordinator.Flow {
	t.Helper()
	ctx := context.Background()
	planner, err := fixture.Registry.GlobalAgentDefs().GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("resolve task-planner agent def: %v", err)
	}
	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name:      name,
		StartNode: "plan",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "plan", Name: "Write task plan", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: planner.ID, Workspace: coordinator.WorkspaceBase, Artifact: coordinator.ArtifactTaskSet}}},
			{Key: "review", Name: "Review plan", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Instructions: "Review the proposed implementation tasks.", Outcomes: []string{"approved", "changes_requested", "rejected"}}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionRejected}}},
		},
		Edges: []coordinator.FlowEdgeInput{
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

// newMultiGateFixtureFlow builds a two-gate change shape with an author phase
// between the gates: gate1 -> work2 -> gate2 -> done. Approving gate1 records
// a decision on it and moves the run to the work2 phase, so a verdict arriving
// while the run is working toward gate2 must still succeed — the second gate
// has not been reached, even though gate1's decision is already recorded.
func newMultiGateFixtureFlow(t *testing.T, fixture testFixture, name string) coordinator.Flow {
	t.Helper()
	ctx := context.Background()
	planner, err := fixture.Registry.GlobalAgentDefs().GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("resolve task-planner agent def: %v", err)
	}
	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name:      name,
		StartNode: "gate1",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "gate1", Name: "First approval", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Instructions: "First review.", Outcomes: []string{"approved", "rejected"}}}},
			{Key: "work2", Name: "Revise for second review", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: planner.ID, Workspace: coordinator.WorkspaceBase, Artifact: coordinator.ArtifactHandoff}}},
			{Key: "gate2", Name: "Second approval", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Instructions: "Second review.", Outcomes: []string{"approved", "changes_requested"}}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "dropped", Name: "Dropped", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCancelled}}},
		},
		Edges: []coordinator.FlowEdgeInput{
			{From: "gate1", Outcome: "approved", To: "work2"},
			{From: "gate1", Outcome: "rejected", To: "dropped"},
			{From: "work2", Outcome: "completed", To: "gate2"},
			{From: "gate2", Outcome: "approved", To: "done"},
			{From: "gate2", Outcome: "changes_requested", To: "done"},
		},
	})
	if err != nil {
		t.Fatalf("create multi-gate flow: %v", err)
	}
	return flow
}

// newRevisitFixtureFlow builds a changes_requested loop: the human gate's
// changes_requested outcome sends the run back through a rework phase and then
// into the same gate again. A verdict for the revisited gate is not late even
// though an earlier visit of the same gate already recorded a decision.
func newRevisitFixtureFlow(t *testing.T, fixture testFixture, name string) coordinator.Flow {
	t.Helper()
	ctx := context.Background()
	planner, err := fixture.Registry.GlobalAgentDefs().GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("resolve task-planner agent def: %v", err)
	}
	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name:      name,
		StartNode: "gate",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "gate", Name: "Review", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Instructions: "Review the change.", Outcomes: []string{"approved", "changes_requested", "rejected"}}}},
			{Key: "rework", Name: "Revise", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: planner.ID, Workspace: coordinator.WorkspaceBase, Artifact: coordinator.ArtifactHandoff}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionRejected}}},
		},
		Edges: []coordinator.FlowEdgeInput{
			{From: "gate", Outcome: "approved", To: "done"},
			{From: "gate", Outcome: "changes_requested", To: "rework"},
			{From: "gate", Outcome: "rejected", To: "rejected"},
			{From: "rework", Outcome: "completed", To: "gate"},
		},
	})
	if err != nil {
		t.Fatalf("create revisit flow: %v", err)
	}
	return flow
}

// newFinalGateFixtureFlow builds the passed-final-gate shape: one human gate
// followed by an agent phase and then the terminal. Approving the gate records
// the flow's only decision and moves the run to the work phase, so a verdict
// arriving while the run is working has no reachable future or revisited human
// gate to belong to: it contradicts the recorded decision and must be refused.
func newFinalGateFixtureFlow(t *testing.T, fixture testFixture, name string) coordinator.Flow {
	t.Helper()
	ctx := context.Background()
	planner, err := fixture.Registry.GlobalAgentDefs().GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("resolve task-planner agent def: %v", err)
	}
	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name:      name,
		StartNode: "gate",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "gate", Name: "Review", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Instructions: "Review the change.", Outcomes: []string{"approved", "rejected"}}}},
			{Key: "work", Name: "Revise", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: planner.ID, Workspace: coordinator.WorkspaceBase, Artifact: coordinator.ArtifactHandoff}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionRejected}}},
		},
		Edges: []coordinator.FlowEdgeInput{
			{From: "gate", Outcome: "approved", To: "work"},
			{From: "gate", Outcome: "rejected", To: "rejected"},
			{From: "work", Outcome: "completed", To: "done"},
		},
	})
	if err != nil {
		t.Fatalf("create final-gate flow: %v", err)
	}
	return flow
}

// submitHandoffArtifact files a handoff artifact for the node run, the way an
// agent hands its draft to the workflow, and returns the artifact id.
func submitHandoffArtifact(t *testing.T, fixture testFixture, taskID, nodeRunID, clientKey, summary string) string {
	t.Helper()
	var artifact workflowArtifactResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/workflow/artifacts",
		map[string]any{
			"node_run_id":      nodeRunID,
			"kind":             string(coordinator.ArtifactHandoff),
			"summary_markdown": summary,
			"payload":          json.RawMessage(`{"note":"draft"}`),
			"client_key":       clientKey,
		}, http.StatusCreated, &artifact)
	return artifact.Artifact.ID
}

// completeAgentNode completes the active agent node run with the given
// artifact, advancing the run to its next node.
func completeAgentNode(t *testing.T, fixture testFixture, taskID, nodeRunID, artifactID string) {
	t.Helper()
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/workflow/complete",
		workflowCompleteRequest{NodeRunID: nodeRunID, ArtifactID: artifactID}, http.StatusOK, nil)
}

func submitPlanArtifact(t *testing.T, fixture testFixture, taskID, nodeRunID, clientKey, title string) string {
	t.Helper()
	payload, err := json.Marshal(coordinator.TaskSetManifest{
		SchemaVersion: 1,
		Items:         []coordinator.TaskSetItem{{Key: "one", Kind: coordinator.WorkItemTask, Title: title, Body: "do the thing"}},
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	var artifact workflowArtifactResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/workflow/artifacts",
		map[string]any{
			"node_run_id":      nodeRunID,
			"kind":             string(coordinator.ArtifactTaskSet),
			"summary_markdown": "# Plan\n\n" + title,
			"payload":          json.RawMessage(payload),
			"client_key":       clientKey,
		}, http.StatusCreated, &artifact)
	return artifact.Artifact.ID
}

func TestInteractivePlanReviewLifecycle(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newReviewFixtureFlow(t, fixture, "interactive plan review")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Plan the storage split", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)
	nodeRunID := scheduled.Run.CurrentNodeRunID
	if nodeRunID == "" {
		t.Fatalf("scheduled run = %+v, want a current plan node", scheduled.Run)
	}
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, nodeRunID); err != nil {
		t.Fatalf("mark plan node running: %v", err)
	}

	// The planner hands its draft to the human without ending the node.
	artifactID := submitPlanArtifact(t, fixture, created.Task.ID, nodeRunID, "v1", "First draft")
	var submitted workflowSubmitReviewResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/submit-review",
		workflowSubmitReviewRequest{NodeRunID: nodeRunID, ArtifactID: artifactID}, http.StatusCreated, &submitted)
	if submitted.Wait.Kind != coordinator.WorkflowWaitHumanGate || submitted.Wait.NodeRunID != nodeRunID {
		t.Fatalf("review wait = %+v, want human gate on the plan node", submitted.Wait)
	}
	details, err := coordinator.ParseReviewWaitDetails(submitted.Wait.Details)
	if err != nil {
		t.Fatalf("parse review wait details: %v", err)
	}
	if !details.Interactive || details.ArtifactID != artifactID || len(details.Outcomes) != 3 {
		t.Fatalf("wait details = %+v, want interactive review contract", details)
	}

	// An ordinary agent completion has no review-round capability and must not
	// consume an interactive wait, even when it presents the reviewed artifact.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/complete",
		workflowCompleteRequest{NodeRunID: nodeRunID, ArtifactID: artifactID}, http.StatusConflict, nil)
	var afterRejectedComplete workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/workflow",
		nil, http.StatusOK, &afterRejectedComplete)
	if afterRejectedComplete.Detail.OpenWait == nil || afterRejectedComplete.Detail.OpenWait.ID != submitted.Wait.ID || afterRejectedComplete.Detail.OpenWait.NodeRunID != nodeRunID {
		t.Fatalf("interactive wait after ordinary complete = %+v, want original wait %q", afterRejectedComplete.Detail.OpenWait, submitted.Wait.ID)
	}

	var detail workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/workflow",
		nil, http.StatusOK, &detail)
	if detail.Detail.OpenWait == nil || detail.Detail.Run.State != coordinator.WorkflowRunWaiting {
		t.Fatalf("workflow detail = %+v, want waiting on the review", detail.Detail)
	}
	if len(detail.Artifacts) == 0 || detail.Artifacts[0].SummaryMarkdown == "" {
		t.Fatalf("artifacts = %+v, want the plan summary", detail.Artifacts)
	}

	var waiting coordinator.ReviewStatusResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v2/tasks/"+created.Task.ID+"/workflow/review?node_run_id="+nodeRunID, nil, http.StatusOK, &waiting)
	if waiting.State != coordinator.ReviewStateWaiting || waiting.ArtifactID != artifactID {
		t.Fatalf("review status = %+v, want waiting on the draft", waiting)
	}

	// Commenting records feedback without touching the gate.
	var comment workflowCommentResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/comment",
		workflowCommentRequest{Message: "Can the middle task shrink?"}, http.StatusCreated, &comment)
	if comment.Queued {
		t.Fatal("no live session: comment should not report queued delivery")
	}
	var withStatus taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID, nil, http.StatusOK, &withStatus)
	foundComment := false
	for _, entry := range withStatus.StatusLog {
		if strings.Contains(entry.Message, "Can the middle task shrink?") {
			foundComment = true
		}
	}
	if !foundComment {
		t.Fatalf("status log = %+v, want the recorded comment", withStatus.StatusLog)
	}

	// An outcome the gate does not offer is rejected without consuming the wait.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, ReviewWaitID: submitted.Wait.ID, Outcome: "ship_it"}, http.StatusConflict, nil)

	// Changes requested resumes the same node run — the agent revises in its
	// session and submits a new draft.
	var revised coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, ReviewWaitID: submitted.Wait.ID, Outcome: "changes_requested", Feedback: "split the storage task"},
		http.StatusOK, &revised)
	if revised.Run.State != coordinator.WorkflowRunRunning || revised.Run.CurrentNodeRunID != nodeRunID {
		t.Fatalf("run after revise = %+v, want running on the same node", revised.Run)
	}
	var resolved coordinator.ReviewStatusResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v2/tasks/"+created.Task.ID+"/workflow/review?node_run_id="+nodeRunID, nil, http.StatusOK, &resolved)
	if resolved.State != coordinator.ReviewStateResolved || resolved.Outcome != "changes_requested" || resolved.Feedback != "split the storage task" {
		t.Fatalf("review status after revise = %+v", resolved)
	}

	revisedArtifactID := submitPlanArtifact(t, fixture, created.Task.ID, nodeRunID, "v2", "Second draft")
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/submit-review",
		workflowSubmitReviewRequest{NodeRunID: nodeRunID, ArtifactID: revisedArtifactID}, http.StatusCreated, &submitted)

	// Approval completes the plan node with the reviewed artifact, answers the
	// gate, and lands the run at its terminal in one motion.
	var approved coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, ReviewWaitID: submitted.Wait.ID, Outcome: "approved", Feedback: "looks good"}, http.StatusOK, &approved)
	if !approved.Done || approved.Run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("approved run = %+v, want completed", approved.Run)
	}
	planNode, ok, err := fixture.Bundle.WorkflowRuns.GetNodeRun(ctx, nodeRunID)
	if err != nil || !ok {
		t.Fatalf("load plan node: found=%t err=%v", ok, err)
	}
	if planNode.State != coordinator.WorkflowNodeSucceeded || planNode.OutputArtifactID != revisedArtifactID {
		t.Fatalf("plan node = %+v, want completed with the revised artifact", planNode)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v2/tasks/"+created.Task.ID+"/workflow/review?node_run_id="+nodeRunID, nil, http.StatusOK, &resolved)
	if resolved.State != coordinator.ReviewStateResolved || resolved.Outcome != "approved" {
		t.Fatalf("review status after approval = %+v", resolved)
	}
}

func TestSubmitReviewRejectsWrongCredentialsAndState(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newReviewFixtureFlow(t, fixture, "review credential checks")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Guarded plan", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)
	nodeRunID := scheduled.Run.CurrentNodeRunID
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, nodeRunID); err != nil {
		t.Fatalf("mark plan node running: %v", err)
	}
	artifactID := submitPlanArtifact(t, fixture, created.Task.ID, nodeRunID, "v1", "Draft")

	// The worker-join token is not a review credential.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/submit-review",
		workflowSubmitReviewRequest{NodeRunID: nodeRunID, ArtifactID: artifactID}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet,
		"/v2/tasks/"+created.Task.ID+"/workflow/review?node_run_id="+nodeRunID, nil, http.StatusForbidden, nil)
	// Comments require the owner.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/comment",
		workflowCommentRequest{Message: "hi"}, http.StatusForbidden, nil)
	// A node that is not waiting cannot be answered through the review path.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, ReviewWaitID: "ww-not-open", Outcome: "approved"}, http.StatusConflict, nil)
}

func TestRespondReviewRejectsStaleRoundBinding(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newReviewFixtureFlow(t, fixture, "review wait round binding")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Bound plan", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)
	nodeRunID := scheduled.Run.CurrentNodeRunID
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, nodeRunID); err != nil {
		t.Fatalf("mark plan node running: %v", err)
	}

	// Round one opens an interactive wait on the agent node run.
	artifactID := submitPlanArtifact(t, fixture, created.Task.ID, nodeRunID, "v1", "First draft")
	var roundOne workflowSubmitReviewResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/submit-review",
		workflowSubmitReviewRequest{NodeRunID: nodeRunID, ArtifactID: artifactID}, http.StatusCreated, &roundOne)

	// A response bound to the open round's wait id is accepted.
	var revised coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, Outcome: "changes_requested", Feedback: "revise the draft", ReviewWaitID: roundOne.Wait.ID},
		http.StatusOK, &revised)

	// The agent resubmits: round two reopens a fresh wait on the same node run.
	revisedArtifactID := submitPlanArtifact(t, fixture, created.Task.ID, nodeRunID, "v2", "Second draft")
	var roundTwo workflowSubmitReviewResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/submit-review",
		workflowSubmitReviewRequest{NodeRunID: nodeRunID, ArtifactID: revisedArtifactID}, http.StatusCreated, &roundTwo)
	if roundTwo.Wait.ID == roundOne.Wait.ID {
		t.Fatalf("round two wait id = %q, want a fresh wait per review round", roundTwo.Wait.ID)
	}

	// The stale round-one response — bound to round one's wait id — is rejected
	// with 409 and cannot decide round two; a foreign binding is rejected too.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, Outcome: "approved", Feedback: "stale verdict", ReviewWaitID: roundOne.Wait.ID},
		http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, Outcome: "approved", ReviewWaitID: "ww-foreign"},
		http.StatusConflict, nil)

	// A node-only response is rejected rather than rebound to the current wait.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, Outcome: "approved", Feedback: "looks good"},
		http.StatusBadRequest, nil)

	// Round two remains actionable only through its own immutable wait id.
	var approved coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: nodeRunID, Outcome: "approved", Feedback: "looks good", ReviewWaitID: roundTwo.Wait.ID},
		http.StatusOK, &approved)
	if !approved.Done || approved.Run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("round two run = %+v, want completed", approved.Run)
	}
}
