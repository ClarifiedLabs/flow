package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// newBoardFixtureFlow builds gate -> merged, enough graph for the board's step
// rail to have a position to report.
func newBoardFixtureFlow(t *testing.T, fixture testFixture, name string) coordinator.Flow {
	t.Helper()
	flow, err := fixture.Bundle.Flows.Create(context.Background(), coordinator.FlowInput{
		Name:      name,
		StartNode: "gate",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "gate", Name: "Wait for approval", Kind: coordinator.NodeHumanGate, Config: coordinator.FlowNodeConfig{HumanGate: &coordinator.HumanGateNodeConfig{Outcomes: []string{"approved", "rejected"}}}},
			{Key: "merged", Name: "Merge the change", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
			{Key: "dropped", Name: "Dropped", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCancelled}}},
		},
		Edges: []coordinator.FlowEdgeInput{
			{From: "gate", Outcome: "approved", To: "merged"},
			{From: "gate", Outcome: "rejected", To: "dropped"},
		},
	})
	if err != nil {
		t.Fatalf("create board fixture flow: %v", err)
	}
	return flow
}

// fetchProjectBoard reads the single-project board. /v2/board is the aggregate
// shape (one entry per project), which is not what these assertions want.
func fetchProjectBoard(t *testing.T, fixture testFixture) boardResponse {
	t.Helper()
	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v2/projects/"+fixture.Project.ID+"/board", nil, http.StatusOK, &board)
	return board
}

func TestBoardCardCarriesDwellStepPositionAndWait(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "board read model")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Needs a human", FlowID: flow.ID}, http.StatusCreated, &created)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, nil)

	board := fetchProjectBoard(t, fixture)
	card, ok := board.TaskCards[created.Task.ID]
	if !ok {
		t.Fatalf("board cards = %+v, want a card for %s", board.TaskCards, created.Task.ID)
	}

	if card.StepCount != 3 {
		t.Fatalf("step count = %d, want 3", card.StepCount)
	}
	if card.StepIndex != 1 {
		t.Fatalf("step index = %d, want 1", card.StepIndex)
	}
	if card.DwellSince == nil || card.DwellSince.IsZero() {
		t.Fatalf("dwell_since = %v, want the time the task entered this state", card.DwellSince)
	}
	// The strip renders the real question, not a generic "blocked", so the
	// wait's kind has to survive onto the card.
	if card.Wait == nil || card.Wait.Kind != coordinator.WorkflowWaitHumanGate {
		t.Fatalf("wait = %+v, want the open human gate", card.Wait)
	}
	if card.Held {
		t.Fatal("card should not report held before a hold")
	}
}

func TestHoldEndpointStopsTheRunAndShowsOnTheBoard(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "board hold")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Taken over", FlowID: flow.ID}, http.StatusCreated, &created)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, nil)

	var held workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/hold",
		nil, http.StatusOK, &held)
	if held.Run.HeldAt == nil {
		t.Fatalf("run = %+v, want held_at set", held.Run)
	}

	board := fetchProjectBoard(t, fixture)
	if lane := board.LaneStates[created.Task.ID]; lane != coordinator.LaneStateHeld {
		t.Fatalf("lane state = %q, want held — the board must say who owns the task", lane)
	}
	if card := board.TaskCards[created.Task.ID]; !card.Held {
		t.Fatalf("card = %+v, want held", card)
	}

	var released coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/release",
		workflowReleaseRequest{Edge: string(coordinator.ReleaseResume)}, http.StatusOK, &released)
	if released.Run.HeldAt != nil {
		t.Fatalf("released run = %+v, want the hold cleared", released.Run)
	}

	// Releasing a run nobody holds is a conflict, not a silent success.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/release",
		workflowReleaseRequest{Edge: string(coordinator.ReleaseResume)}, http.StatusConflict, nil)
}

func TestConvergenceHoldShowsAsBlockedOnTheBoardAndSidebar(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "board convergence hold")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Oversized change", FlowID: flow.ID}, http.StatusCreated, &created)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, nil)

	if _, _, err := fixture.Bundle.WorkflowRuns.HoldForConvergence(context.Background(), created.Task.ID, 8, 420, 160, 5, 500); err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}

	board := fetchProjectBoard(t, fixture)
	if lane := board.LaneStates[created.Task.ID]; lane != coordinator.LaneStateHeld {
		t.Fatalf("lane state = %q, want held", lane)
	}
	blocked := false
	for _, id := range board.BlockedIDs {
		if id == created.Task.ID {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("blocked ids = %+v, want the convergence-held task %s", board.BlockedIDs, created.Task.ID)
	}

	var sidebar sidebarResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/sidebar", nil, http.StatusOK, &sidebar)
	want := uiSidebarBoardSummary{InProgress: 0, Blocked: 1}
	if sidebar.Board != want {
		t.Fatalf("sidebar board counts = %+v, want %+v", sidebar.Board, want)
	}
}

func TestReleaseSkipToMergeClosesTheRun(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "board skip to merge")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Handed back", FlowID: flow.ID}, http.StatusCreated, &created)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/hold",
		nil, http.StatusOK, nil)

	var released coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/release",
		workflowReleaseRequest{Edge: string(coordinator.ReleaseMerge)}, http.StatusOK, &released)
	if !released.Done {
		t.Fatalf("released run = %+v, want the run completed at its terminal", released.Run)
	}
}

func TestWorkflowDetailResolvesNodeNamesAndEdgeCounts(t *testing.T) {
	fixture := newTestFixture(t)
	flow := newBoardFixtureFlow(t, fixture, "workflow detail")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Detailed", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/respond",
		workflowRespondRequest{NodeRunID: scheduled.Run.CurrentNodeRunID, Outcome: "approved"}, http.StatusOK, nil)

	var detail workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/workflow",
		nil, http.StatusOK, &detail)

	// The run spine shows a node's name once and its kind as a tag, so both
	// arrive resolved rather than being re-joined against the snapshot.
	var gate *coordinator.WorkflowNodeRun
	for i := range detail.Detail.NodeRuns {
		if detail.Detail.NodeRuns[i].NodeKey == "gate" {
			gate = &detail.Detail.NodeRuns[i]
		}
	}
	if gate == nil {
		t.Fatalf("node runs = %+v, want the gate visit", detail.Detail.NodeRuns)
	}
	if gate.Name != "Wait for approval" || gate.Kind != coordinator.NodeHumanGate {
		t.Fatalf("gate node run = %+v, want the frozen name and kind", gate)
	}

	if len(detail.Detail.TransitionCounts) == 0 {
		t.Fatal("transition counts are empty; the graph cannot mark taken edges")
	}
	var found bool
	for _, count := range detail.Detail.TransitionCounts {
		if count.From == "gate" && count.To == "merged" && count.Outcome == "approved" && count.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("transition counts = %+v, want gate -approved-> merged x1", detail.Detail.TransitionCounts)
	}
}

func TestEpicRollupReportsMembersAndCriticalPath(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	flow := newBoardFixtureFlow(t, fixture, "epic rollup")

	var epic taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Lifecycle hardening", FlowID: flow.ID}, http.StatusCreated, &epic)

	ids := make([]string, 0, 3)
	for _, title := range []string{"First", "Second", "Third"} {
		var member taskResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
			createTaskRequest{Title: title, FlowID: flow.ID}, http.StatusCreated, &member)
		if err := fixture.Bundle.Tasks.LinkTasks(ctx, epic.Task.ID, member.Task.ID,
			coordinator.RelationParentOf, coordinator.ActorHuman); err != nil {
			t.Fatalf("link epic member: %v", err)
		}
		ids = append(ids, member.Task.ID)
	}

	// First blocks Second blocks Third: a three-long chain the footer should
	// surface as the critical path.
	for i := 0; i+1 < len(ids); i++ {
		if err := fixture.Bundle.Tasks.LinkTasks(ctx, ids[i], ids[i+1],
			coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
			t.Fatalf("link blocker: %v", err)
		}
	}

	var response epicResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+epic.Task.ID+"/epic",
		nil, http.StatusOK, &response)

	if response.TotalCount != 3 || len(response.Members) != 3 {
		t.Fatalf("epic = %+v, want 3 members", response)
	}
	if response.Epic.Title != "Lifecycle hardening" {
		t.Fatalf("epic title = %q", response.Epic.Title)
	}
	if len(response.CriticalPath) != 3 {
		t.Fatalf("critical path = %v, want the full three-task chain", response.CriticalPath)
	}
	if response.CriticalPath[0] != ids[0] || response.CriticalPath[2] != ids[2] {
		t.Fatalf("critical path = %v, want %v in dependency order", response.CriticalPath, ids)
	}

	var third *epicMember
	for i := range response.Members {
		if response.Members[i].ID == ids[2] {
			third = &response.Members[i]
		}
	}
	if third == nil || len(third.BlockedBy) != 1 || third.BlockedBy[0] != ids[1] {
		t.Fatalf("third member = %+v, want blocked by %s", third, ids[1])
	}
}
