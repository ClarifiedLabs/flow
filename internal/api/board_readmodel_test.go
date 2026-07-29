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

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
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
