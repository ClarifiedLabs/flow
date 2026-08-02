package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	sqlite3 "github.com/mattn/go-sqlite3"
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

func TestManualConvergenceHoldShowsAsBlockedOnTheBoardAndSidebar(t *testing.T) {
	fixture := newTestFixture(t)
	makeExchangeHooksInert(t, fixture.Project.ExchangePath)
	seedAPIMain(t, fixture.Project.ExchangePath)

	const branch = "task/manual-convergence"
	clonePath := filepath.Join(t.TempDir(), "manual-convergence")
	runAPIGit(t, "", "clone", fixture.Project.ExchangePath, clonePath)
	runAPIGit(t, clonePath, "config", "user.name", "Flow Test")
	runAPIGit(t, clonePath, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, clonePath, "checkout", "-b", branch, "origin/main")
	writeAPIFile(t, clonePath, "scope.txt", "small manual review\n")
	runAPIGit(t, clonePath, "add", "scope.txt")
	runAPIGit(t, clonePath, "commit", "-m", "small scoped change")
	headSHA := apiGitOutput(t, clonePath, "rev-parse", "HEAD")
	runAPIGit(t, clonePath, "push", "origin", "HEAD:refs/heads/"+branch)

	flow := newBoardFixtureFlow(t, fixture, "board manual convergence hold")
	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Manually reviewed change", FlowID: flow.ID}, http.StatusCreated, &created)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, nil)

	run, active, err := fixture.Bundle.WorkflowRuns.ActiveForTask(context.Background(), created.Task.ID)
	if err != nil || !active {
		t.Fatalf("active workflow run = %+v active=%t err=%v", run, active, err)
	}
	nodeRunID := run.CurrentNodeRunID
	if nodeRunID == "" {
		nodeRun, _, err := fixture.Bundle.WorkflowRuns.EnsureCurrentNode(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("ensure current convergence node: %v", err)
		}
		nodeRunID = nodeRun.ID
	}
	const changeID = "ch-board-convergence"
	const artifactID = "wa-board-convergence"
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, 'main', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, created.Task.ID, run.ID, branch, headSHA); err != nil {
		t.Fatalf("insert convergence change projection: %v", err)
	}
	if _, err := fixture.DB.ExecContext(context.Background(), `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-owner', 'change', 'Small change', ?, 'digest', 'manual-convergence',
	'2026-01-01T00:00:00Z')`, artifactID, run.ID, nodeRunID,
		fmt.Sprintf(`{"change_id":%q,"head_sha":%q}`, changeID, headSHA)); err != nil {
		t.Fatalf("insert convergence artifact: %v", err)
	}
	if _, err := fixture.DB.ExecContext(context.Background(), `
UPDATE workflow_runs SET current_artifact_id = ? WHERE id = ?;
UPDATE workflow_node_runs SET input_artifact_id = ? WHERE id = ?`,
		artifactID, run.ID, artifactID, nodeRunID); err != nil {
		t.Fatalf("project convergence artifact: %v", err)
	}

	requestPath := "/v2/tasks/" + created.Task.ID + "/workflow/convergence/request"
	var held workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, requestPath,
		map[string]any{}, http.StatusOK, &held)
	if !held.Run.Held() || held.Run.HeldBy != string(coordinator.ActorSystem) {
		t.Fatalf("manual convergence hold = %+v, want system-enforced typed hold", held.Run)
	}
	var replay workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, requestPath,
		map[string]any{}, http.StatusOK, &replay)
	if replay.Run.ID != held.Run.ID || !replay.Run.Held() {
		t.Fatalf("replayed manual convergence hold = %+v, want held run %s", replay.Run, held.Run.ID)
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

	var taskDetail taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID,
		nil, http.StatusOK, &taskDetail)
	if taskDetail.Detail == nil || taskDetail.Detail.ConvergenceEvidence == nil || taskDetail.Detail.ConvergenceEvidence.WorkflowRunID != run.ID {
		t.Fatalf("task detail convergence evidence = %+v, want typed run %s evidence", taskDetail.Detail, run.ID)
	}
	var workflowDetail workflowDetailResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/workflow",
		nil, http.StatusOK, &workflowDetail)
	if workflowDetail.Detail.ConvergenceEvidence == nil || workflowDetail.Detail.ConvergenceEvidence.Fingerprint == "" {
		t.Fatalf("workflow convergence evidence = %+v, want fingerprint", workflowDetail.Detail.ConvergenceEvidence)
	}
	if evidence := workflowDetail.Detail.ConvergenceEvidence; evidence.Files > coordinator.DefaultReviewScopeFileLimit || evidence.Additions+evidence.Deletions > coordinator.DefaultReviewScopeLineLimit {
		t.Fatalf("manual convergence evidence = %+v, want a below-threshold change", evidence)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/release",
		workflowReleaseRequest{Edge: string(coordinator.ReleaseResume)}, http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/convergence",
		workflowConvergenceRequest{Disposition: coordinator.ConvergenceRepairBranch, ExpectedEvidenceFingerprint: "stale-reviewed-fingerprint"}, http.StatusConflict, nil)
	var resolved coordinator.ConvergenceReviewResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/workflow/convergence",
		workflowConvergenceRequest{Disposition: coordinator.ConvergenceRepairBranch, Note: "reduce the patch", ExpectedEvidenceFingerprint: workflowDetail.Detail.ConvergenceEvidence.Fingerprint}, http.StatusOK, &resolved)
	if !resolved.Run.Held() || resolved.Evidence.Fingerprint == "" {
		t.Fatalf("resolved convergence review = %+v, want repair disposition to preserve the typed hold", resolved)
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

func TestBoardExposesAwaitingWorkerLaneState(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Waiting on a worker"}, http.StatusCreated, &created)
	taskID := created.Task.ID

	// Park the task in progress on an active agent node whose author job is
	// queued: the board must say awaiting_worker, not working.
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, transition_budget, created_at, started_at
) VALUES ('wr-await', ?, 1, '{}', 'running', 'author', 'nr-await', 10,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, taskID); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES ('nr-await', 'wr-await', 'author', 1, 1, 'running',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert workflow node run: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO jobs (
	id, task_id, workflow_run_id, node_run_id, role, state, capacity_bucket,
	created_at, updated_at
) VALUES ('j-await', ?, 'wr-await', 'nr-await', 'author', 'queued',
	'persistent_agent', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, taskID); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	board := fetchProjectBoard(t, fixture)
	if lane := board.LaneStates[taskID]; lane != coordinator.LaneStateAwaitingWorker {
		t.Fatalf("per-project lane state = %q, want %q", lane, coordinator.LaneStateAwaitingWorker)
	}

	var aggregate aggregateBoardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/board", nil, http.StatusOK, &aggregate)
	var aggregateLane coordinator.LaneState
	for _, projectBoard := range aggregate.Boards {
		if projectBoard.ProjectID == fixture.Project.ID {
			aggregateLane = projectBoard.LaneStates[taskID]
		}
	}
	if aggregateLane != coordinator.LaneStateAwaitingWorker {
		t.Fatalf("aggregate lane state = %q, want %q", aggregateLane, coordinator.LaneStateAwaitingWorker)
	}

	// The sidebar keeps counting the task as in progress: it is not blocked,
	// awaiting-worker is a refinement of working, not a new chip.
	var sidebar sidebarResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/sidebar", nil, http.StatusOK, &sidebar)
	if sidebar.Board.InProgress != 1 || sidebar.Board.Blocked != 0 {
		t.Fatalf("sidebar board counts = %+v, want one in-progress task", sidebar.Board)
	}
}

// TestBoardCardNamesItsUnresolvedBlocker covers the on-card "waiting on"
// read model: a scheduled task with a live blocks blocker carries the
// blocker's id and title, and a task with no blocker carries none.
func TestBoardCardNamesItsUnresolvedBlocker(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	blocker, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Finish dependency"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, blocked.ID); err != nil {
		t.Fatalf("schedule blocked task: %v", err)
	}

	board := fetchProjectBoard(t, fixture)

	card, ok := board.TaskCards[blocked.ID]
	if !ok {
		t.Fatalf("board cards = %+v, missing %s", board.TaskCards, blocked.ID)
	}
	if card.Blockers.Count != 1 || len(card.Blockers.Tasks) != 1 {
		t.Fatalf("blocker summary = %+v, want one unresolved blocker", card.Blockers)
	}
	if card.Blockers.Tasks[0].ID != blocker.ID || card.Blockers.Tasks[0].Title != "Finish dependency" {
		t.Fatalf("blocker = %+v, want %s \"Finish dependency\"", card.Blockers.Tasks[0], blocker.ID)
	}

	// The blocker itself has no blockers, so its card must carry none — the
	// indicator only appears on the task that is actually waiting.
	blockerCard, ok := board.TaskCards[blocker.ID]
	if !ok {
		t.Fatalf("board cards = %+v, missing %s", board.TaskCards, blocker.ID)
	}
	if blockerCard.Blockers.Count != 0 || len(blockerCard.Blockers.Tasks) != 0 {
		t.Fatalf("blocker card blockers = %+v, want none", blockerCard.Blockers)
	}
}

// TestBoardCardDropsResolvedBlocker is the regression the task calls out: once
// the blocking task reaches done it stops counting as a blocker, so the card no
// longer names it.
func TestBoardCardDropsResolvedBlocker(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	blocker, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Finish dependency"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, blocked.ID); err != nil {
		t.Fatalf("schedule blocked task: %v", err)
	}

	if card := fetchProjectBoard(t, fixture).TaskCards[blocked.ID]; card.Blockers.Count != 1 {
		t.Fatalf("blockers before done = %+v, want the live blocker", card.Blockers)
	}

	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, blocker.ID, coordinator.ResolutionCompleted, "done", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish blocker: %v", err)
	}

	card := fetchProjectBoard(t, fixture).TaskCards[blocked.ID]
	if card.Blockers.Count != 0 || len(card.Blockers.Tasks) != 0 {
		t.Fatalf("blockers after done = %+v, want the resolved blocker gone", card.Blockers)
	}
}

// TestBlockerSummaryRanksByPriorityAndDisclosesOverflow pins the "waiting on"
// ordering: when a task has more live blockers than the card can display, the
// shown ones are the most important (highest priority, then most recently
// updated, then a stable id tiebreak) and the rest are disclosed as a count
// rather than dropped silently.
func TestBlockerSummaryRanksByPriorityAndDisclosesOverflow(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-time.Hour)

	relation := func(id string, priority int, updated time.Time) coordinator.TaskRelation {
		return coordinator.TaskRelation{
			SourceTaskID:    id,
			TargetTaskID:    "t-blocked",
			Kind:            coordinator.RelationBlocks,
			SourceTitle:     "Blocker " + id,
			SourceState:     coordinator.LifecycleScheduled,
			SourcePriority:  priority,
			SourceUpdatedAt: updated,
		}
	}

	// Feed the relations in creation-time order (lowest priority first) to prove
	// the summary re-ranks rather than trusting the input order.
	relations := []coordinator.TaskRelation{
		relation("t-low", 0, now),
		relation("t-mid", 5, stale),
		relation("t-high", 9, stale),
		relation("t-tie-new", 5, now),
		relation("t-done", 99, now),
	}
	relations[4].SourceState = coordinator.LifecycleDone

	summary := uiBlockerSummaryFromRelations("t-blocked", relations)

	if summary.Count != 4 {
		t.Fatalf("count = %d, want 4 live blockers (the done one excluded)", summary.Count)
	}
	wantOrder := []string{"t-high", "t-tie-new", "t-mid"}
	if len(summary.Tasks) != len(wantOrder) {
		t.Fatalf("shown tasks = %+v, want %d", summary.Tasks, len(wantOrder))
	}
	for i, want := range wantOrder {
		if summary.Tasks[i].ID != want {
			t.Fatalf("task[%d] = %q, want %q (full order %+v)", i, summary.Tasks[i].ID, want, summary.Tasks)
		}
	}
	if summary.Omitted != 1 {
		t.Fatalf("omitted = %d, want 1 (the priority-0 blocker past the display limit)", summary.Omitted)
	}
}

// TestBoardCardOrdersBlockersAndDisclosesOverflow exercises the full read model:
// a scheduled card with more live blockers than the display limit shows the
// highest-priority titles in order, discloses the overflow count, and keeps the
// resolved blocker out of both.
func TestBoardCardOrdersBlockersAndDisclosesOverflow(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	blocked, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}

	// Create the blockers in ascending priority order so the relations'
	// creation-time order is the reverse of the priority order: if the card
	// shows the right titles, the read model really did re-rank them.
	for _, spec := range []struct {
		title    string
		priority int
	}{
		{"Low priority blocker", 0},
		{"Mid priority blocker", 5},
		{"High priority blocker", 9},
		{"Overflow blocker", 1},
	} {
		blocker, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: spec.title, Priority: spec.priority})
		if err != nil {
			t.Fatalf("create blocker %q: %v", spec.title, err)
		}
		if err := fixture.Tasks.LinkTasks(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
			t.Fatalf("link blocker %q: %v", spec.title, err)
		}
	}

	// A resolved blocker with the highest priority must not steal a display slot.
	resolved, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Resolved blocker", Priority: 99})
	if err != nil {
		t.Fatalf("create resolved blocker: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, resolved.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link resolved blocker: %v", err)
	}

	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, blocked.ID); err != nil {
		t.Fatalf("schedule blocked task: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, resolved.ID, coordinator.ResolutionCompleted, "done", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish resolved blocker: %v", err)
	}

	card := fetchProjectBoard(t, fixture).TaskCards[blocked.ID]
	if card.Blockers.Count != 4 {
		t.Fatalf("blocker count = %d, want 4 live blockers", card.Blockers.Count)
	}
	wantTitles := []string{"High priority blocker", "Mid priority blocker", "Overflow blocker"}
	if len(card.Blockers.Tasks) != len(wantTitles) {
		t.Fatalf("shown blockers = %+v, want %d", card.Blockers.Tasks, len(wantTitles))
	}
	for i, want := range wantTitles {
		if card.Blockers.Tasks[i].Title != want {
			t.Fatalf("blocker[%d] = %q, want %q (full list %+v)", i, card.Blockers.Tasks[i].Title, want, card.Blockers.Tasks)
		}
	}
	if card.Blockers.Omitted != 1 {
		t.Fatalf("omitted = %d, want 1 (the priority-0 blocker past the limit)", card.Blockers.Omitted)
	}
}

// queryRecorder captures the SQL text of every query that reaches the driver,
// so a read model can prove it batches a given table into a constant number of
// queries no matter how many cards it builds. The board read model runs on a
// single connection and the tests never build cards concurrently, so a plain
// slice is enough.
type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *queryRecorder) record(query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, query)
}

func (r *queryRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = nil
}

func (r *queryRecorder) countMatching(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, query := range r.queries {
		if strings.Contains(query, substr) {
			count++
		}
	}
	return count
}

// countingDriver wraps the real sqlite driver and records query text.
type countingDriver struct {
	driver.Driver
	recorder *queryRecorder
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, recorder: d.recorder}, nil
}

type countingConn struct {
	driver.Conn
	recorder *queryRecorder
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return countingStmt{Stmt: stmt, query: query, recorder: c.recorder}, nil
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.recorder.record(query)
	return queryer.QueryContext(ctx, query, args)
}

// ExecContext delegates to the wrapped driver so multi-statement migrations
// still apply; only reads are recorded.
func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

type countingStmt struct {
	driver.Stmt
	query    string
	recorder *queryRecorder
}

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.recorder.record(s.query)
	return s.Stmt.Query(args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	stmt, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	s.recorder.record(s.query)
	return stmt.QueryContext(ctx, args)
}

var countingDriverSeq int64

// openCountingDB opens a project database (migrations applied) through a
// recording driver and returns a handle plus the recorder it writes to. Each
// call registers its own driver name so the recorder is not shared across
// databases.
func openCountingDB(t *testing.T) (*sql.DB, *queryRecorder) {
	t.Helper()
	recorder := &queryRecorder{}
	driverName := fmt.Sprintf("sqlite3-counting-%d", atomic.AddInt64(&countingDriverSeq, 1))
	sql.Register(driverName, countingDriver{Driver: &sqlite3.SQLiteDriver{}, recorder: recorder})
	store, err := flowdb.OpenWithDriver(context.Background(), driverName, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open counting database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.DB(), recorder
}

// TestBoardCardsLoadRelationsWithConstantQueries asserts the read model does
// not regress to a per-task blocker query: building cards for any number of
// tasks loads relations in a single batched query.
func TestBoardCardsLoadRelationsWithConstantQueries(t *testing.T) {
	ctx := context.Background()

	build := func(taskCount int) int {
		db, recorder := openCountingDB(t)
		tasks := coordinator.NewTaskService(db, "p-test")
		var blocker coordinator.Task
		for i := 0; i < taskCount; i++ {
			task, err := tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: fmt.Sprintf("Task %d", i)})
			if err != nil {
				t.Fatalf("create task %d: %v", i, err)
			}
			if i == 0 {
				blocker = task
				continue
			}
			if err := tasks.LinkTasks(ctx, blocker.ID, task.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
				t.Fatalf("link blocker for task %d: %v", i, err)
			}
		}

		all, err := tasks.ListTasks(ctx, coordinator.TaskFilter{})
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}

		server := &projectServer{tasks: tasks}
		// Only count what building the cards does; task creation and linking
		// above touch task_relations too and would drown the signal.
		recorder.reset()
		cards, err := server.buildUITaskCards(ctx, all)
		if err != nil {
			t.Fatalf("build cards: %v", err)
		}
		if len(cards) != taskCount {
			t.Fatalf("built %d cards, want %d", len(cards), taskCount)
		}
		// Sanity: the blocked tasks still report their blocker, so the counting
		// run exercised the same code path the board serves.
		if taskCount > 1 && cards[all[1].ID].Blockers.Count != 1 {
			t.Fatalf("card blockers = %+v, want the live blocker", cards[all[1].ID].Blockers)
		}
		return recorder.countMatching("task_relations")
	}

	one := build(1)
	many := build(6)
	if one != 1 {
		t.Fatalf("relation queries for 1 task = %d, want a single batched query", one)
	}
	if many != one {
		t.Fatalf("relation queries grew with card count: 1 task = %d, 6 tasks = %d", one, many)
	}
}

// TestBoardCardsLoadLatestSessionsWithConstantQueries asserts the read model
// does not regress to a per-task latest-session query: building cards for any
// number of tasks loads the latest sessions in a single batched query.
func TestBoardCardsLoadLatestSessionsWithConstantQueries(t *testing.T) {
	ctx := context.Background()

	build := func(taskCount int) int {
		db, recorder := openCountingDB(t)
		tasks := coordinator.NewTaskService(db, "p-test")
		sessions := coordinator.NewSessionService(db, tasks, flowworker.NewService(db))
		taskIDs := make([]string, 0, taskCount)
		for i := 0; i < taskCount; i++ {
			task, err := tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: fmt.Sprintf("Task %d", i)})
			if err != nil {
				t.Fatalf("create task %d: %v", i, err)
			}
			taskIDs = append(taskIDs, task.ID)
		}
		// Give every task one finished session so the batched lookup has rows to
		// return. Finished sessions never match the active-session reads, so the
		// only per-card session work left is the latest-session batch.
		now := time.Now().UTC()
		for i, taskID := range taskIDs {
			jobID := fmt.Sprintf("j-session-%d", i)
			leaseID := fmt.Sprintf("l-session-%d", i)
			if _, err := db.ExecContext(ctx, `
INSERT INTO jobs (id, task_id, role, state, capacity_bucket, created_at, updated_at)
VALUES (?, ?, 'author', 'finished', 'persistent_agent', ?, ?)`,
				jobID, taskID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
				t.Fatalf("insert session job %d: %v", i, err)
			}
			if _, err := db.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-test', 'persistent_agent', ?, ?)`,
				leaseID, jobID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
				t.Fatalf("insert session lease %d: %v", i, err)
			}
			if _, err := db.ExecContext(ctx, `
INSERT INTO sessions (
	id, task_id, job_id, lease_id, worker_id, role, workspace_mode, runtime_state,
	branch, base, harness, token_hash, created_at, updated_at, last_agent_activity_at
) VALUES (?, ?, ?, ?, 'w-test', 'author', 'change', 'finished',
	'task/latest', 'main', 'harness', ?, ?, ?, ?)`,
				fmt.Sprintf("s-session-%d", i), taskID, jobID, leaseID, fmt.Sprintf("tok-%d", i),
				now.Add(-time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
				t.Fatalf("insert session %d: %v", i, err)
			}
		}

		all, err := tasks.ListTasks(ctx, coordinator.TaskFilter{})
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}

		server := &projectServer{tasks: tasks, sessions: sessions}
		// Only count what building the cards does; task and session creation
		// above touch the same tables and would drown the signal.
		recorder.reset()
		cards, err := server.buildUITaskCards(ctx, all)
		if err != nil {
			t.Fatalf("build cards: %v", err)
		}
		if len(cards) != taskCount {
			t.Fatalf("built %d cards, want %d", len(cards), taskCount)
		}
		// Sanity: every card still carries its session's activity, so the
		// counting run exercised the same code path the board serves.
		for _, taskID := range taskIDs {
			if cards[taskID].LastAgentActivityAt == nil {
				t.Fatalf("card %s last agent activity = nil, want the session's timestamp", taskID)
			}
		}
		return recorder.countMatching("MAX(updated_at")
	}

	one := build(1)
	many := build(6)
	if one != 1 {
		t.Fatalf("latest-session queries for 1 task = %d, want a single batched query", one)
	}
	if many != one {
		t.Fatalf("latest-session queries grew with card count: 1 task = %d, 6 tasks = %d", one, many)
	}
}
