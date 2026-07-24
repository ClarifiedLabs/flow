package api

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func setTaskDoneAtForTest(t *testing.T, db *sql.DB, taskID, doneAt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET done_at = ? WHERE id = ?`, doneAt, taskID); err != nil {
		t.Fatalf("set done_at for %s: %v", taskID, err)
	}
}

func insertMergedChangeForTest(t *testing.T, db *sql.DB, taskID, changeID string) {
	t.Helper()
	const ts = "2026-01-01T00:00:00.000000000Z"
	if _, err := db.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?, ?)`,
		changeID, taskID, "feat/"+changeID, "1111111111111111111111111111111111111111",
		ts, ts, ts, ts); err != nil {
		t.Fatalf("insert merged change %s: %v", changeID, err)
	}
}

// seedClosedTasks creates three terminal tasks with deterministic done_at
// ordering (merged newest).
func seedClosedTasks(t *testing.T, fixture testFixture) (merged, rejected, abandoned coordinator.Task) {
	t.Helper()
	ctx := context.Background()

	merged, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "merged work"})
	if err != nil {
		t.Fatalf("create merged task: %v", err)
	}
	insertMergedChangeForTest(t, fixture.DB, merged.ID, "c-0001")
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, merged.ID, coordinator.ResolutionCompleted, "merged", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish merged task: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE tasks SET done_resolution = ? WHERE id = ?`, string(coordinator.ResolutionMerged), merged.ID); err != nil {
		t.Fatalf("stamp merged resolution: %v", err)
	}

	rejected, err = fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "rejected work"})
	if err != nil {
		t.Fatalf("create rejected task: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, rejected.ID, coordinator.ResolutionRejected, "rejected", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish rejected task: %v", err)
	}

	abandoned, err = fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "abandoned work"})
	if err != nil {
		t.Fatalf("create abandoned task: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, abandoned.ID, coordinator.ResolutionAbandoned, "abandoned", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish abandoned task: %v", err)
	}

	setTaskDoneAtForTest(t, fixture.DB, merged.ID, "2026-03-03T03:00:00.000000000Z")
	setTaskDoneAtForTest(t, fixture.DB, rejected.ID, "2026-02-02T02:00:00.000000000Z")
	setTaskDoneAtForTest(t, fixture.DB, abandoned.ID, "2026-01-01T01:00:00.000000000Z")
	return merged, rejected, abandoned
}

func TestDoneAggregateSurfacesClosedTasksWithOutcomesAndMergedChange(t *testing.T) {
	fixture := newTestFixture(t)
	merged, rejected, abandoned := seedClosedTasks(t, fixture)

	var done aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done", nil, http.StatusOK, &done)

	if len(done.Done) != 1 {
		t.Fatalf("aggregate projects = %d, want 1", len(done.Done))
	}
	project := done.Done[0]

	gotIDs := make([]string, len(project.Tasks))
	for i, task := range project.Tasks {
		gotIDs[i] = task.ID
	}
	want := []string{merged.ID, rejected.ID, abandoned.ID}
	if !equalStringSlices(gotIDs, want) {
		t.Fatalf("done task order = %v, want %v (newest closed first)", gotIDs, want)
	}

	if project.Outcomes[merged.ID] != coordinator.Phase(coordinator.ResolutionMerged) {
		t.Fatalf("merged outcome = %q, want %q", project.Outcomes[merged.ID], coordinator.ResolutionMerged)
	}
	if project.Outcomes[rejected.ID] != coordinator.Phase(coordinator.ResolutionRejected) {
		t.Fatalf("rejected outcome = %q, want %q", project.Outcomes[rejected.ID], coordinator.ResolutionRejected)
	}
	if project.Outcomes[abandoned.ID] != coordinator.Phase(coordinator.ResolutionAbandoned) {
		t.Fatalf("abandoned outcome = %q, want %q", project.Outcomes[abandoned.ID], coordinator.ResolutionAbandoned)
	}

	mergedCard, ok := project.TaskCards[merged.ID]
	if !ok || mergedCard.Change == nil || mergedCard.Change.MergedAt == nil {
		t.Fatalf("merged card = %+v, want a change with MergedAt set", mergedCard)
	}
	if mergedCard.Change.ID != "c-0001" {
		t.Fatalf("merged card change id = %q, want c-0001", mergedCard.Change.ID)
	}
	if card := project.TaskCards[abandoned.ID]; card.Change != nil {
		t.Fatalf("abandoned card change = %+v, want nil", card.Change)
	}
}

func TestDoneAggregateKeysetPagination(t *testing.T) {
	fixture := newTestFixture(t)
	merged, rejected, abandoned := seedClosedTasks(t, fixture)

	var page1 aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done?limit=2", nil, http.StatusOK, &page1)
	first := page1.Done[0]
	if got := taskIDsFromAPI(first.Tasks); !equalStringSlices(got, []string{merged.ID, rejected.ID}) {
		t.Fatalf("page 1 ids = %v, want [%s %s]", got, merged.ID, rejected.ID)
	}
	if first.NextBefore == "" || first.NextBeforeID != rejected.ID {
		t.Fatalf("page 1 cursor = (%q, %q), want a timestamp and id %s", first.NextBefore, first.NextBeforeID, rejected.ID)
	}

	var page2 aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet,
		"/v2/done?limit=2&before="+first.NextBefore+"&before_id="+first.NextBeforeID,
		nil, http.StatusOK, &page2)
	second := page2.Done[0]
	if got := taskIDsFromAPI(second.Tasks); !equalStringSlices(got, []string{abandoned.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s]", got, abandoned.ID)
	}
	if second.NextBefore != "" {
		t.Fatalf("page 2 cursor = %q, want empty", second.NextBefore)
	}
}

func TestDoneAggregateFiltersByOutcome(t *testing.T) {
	fixture := newTestFixture(t)
	merged, _, _ := seedClosedTasks(t, fixture)

	var done aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done?outcome=merged", nil, http.StatusOK, &done)
	if got := taskIDsFromAPI(done.Done[0].Tasks); !equalStringSlices(got, []string{merged.ID}) {
		t.Fatalf("outcome=merged ids = %v, want [%s]", got, merged.ID)
	}
}

func TestSidebarReportsClosedCount(t *testing.T) {
	fixture := newTestFixture(t)
	seedClosedTasks(t, fixture)

	var sidebar sidebarResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/sidebar", nil, http.StatusOK, &sidebar)
	if sidebar.Done != 3 {
		t.Fatalf("sidebar done count = %d, want 3", sidebar.Done)
	}
}

func TestSidebarBoardCountsSeparateBlockedFromInProgress(t *testing.T) {
	response := sidebarResponse{}
	addSidebarBoardCounts(&response, coordinator.BoardResult{
		Board: coordinator.Board{
			Unscheduled: make([]coordinator.Task, 2),
			Scheduled:   make([]coordinator.Task, 3),
			InProgress:  make([]coordinator.Task, 5),
		},
		BlockedIDs: []string{"t-0001", "t-0002"},
	})

	want := uiSidebarBoardSummary{Unscheduled: 2, Scheduled: 3, InProgress: 3, Blocked: 2}
	if response.Board != want {
		t.Fatalf("sidebar board counts = %+v, want %+v", response.Board, want)
	}
}

func taskIDsFromAPI(tasks []coordinator.Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
