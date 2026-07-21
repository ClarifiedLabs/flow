package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// closedFixture builds a service with a controllable clock and returns it so
// tests can stamp deterministic done_at values.
func newClosedTaskFixture(t *testing.T) (*TaskService, *time.Time) {
	t.Helper()
	_, service := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := &now
	service.now = func() time.Time { return *clock }
	return service, clock
}

func finishTask(t *testing.T, service *TaskService, clock *time.Time, title string, at time.Time, resolution DoneResolution) Task {
	t.Helper()
	task := createTasks(t, service, title)[0]
	*clock = at
	stamp := formatTime(at)
	if _, err := service.db.ExecContext(context.Background(), `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(resolution), stamp, stamp, task.ID); err != nil {
		t.Fatalf("finish task %s: %v", task.ID, err)
	}
	finished, err := service.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("load finished task %s: %v", task.ID, err)
	}
	return finished
}

func TestListClosedTasksReturnsClosedNewestFirstAndExcludesOpen(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	first := finishTask(t, service, clock, "first", base, ResolutionAbandoned)
	second := finishTask(t, service, clock, "second", base.Add(time.Hour), ResolutionAbandoned)
	third := finishTask(t, service, clock, "third", base.Add(2*time.Hour), ResolutionAbandoned)
	// An open task must never appear.
	createTasks(t, service, "still open")

	tasks, next, err := service.ListClosedTasks(ctx, ClosedTaskQuery{})
	if err != nil {
		t.Fatalf("list closed tasks: %v", err)
	}
	if next != nil {
		t.Fatalf("next cursor = %+v, want nil (no more pages)", next)
	}
	gotIDs := taskIDs(tasks)
	wantIDs := []string{third.ID, second.ID, first.ID}
	if !equalStrings(gotIDs, wantIDs) {
		t.Fatalf("closed task ids = %v, want %v (newest closed first)", gotIDs, wantIDs)
	}
}

func TestListClosedTasksKeysetPagination(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	first := finishTask(t, service, clock, "first", base, ResolutionAbandoned)
	second := finishTask(t, service, clock, "second", base.Add(time.Hour), ResolutionAbandoned)
	third := finishTask(t, service, clock, "third", base.Add(2*time.Hour), ResolutionAbandoned)

	page1, next, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Limit: 2})
	if err != nil {
		t.Fatalf("list closed tasks page 1: %v", err)
	}
	if got := taskIDs(page1); !equalStrings(got, []string{third.ID, second.ID}) {
		t.Fatalf("page 1 ids = %v, want [%s %s]", got, third.ID, second.ID)
	}
	if next == nil {
		t.Fatal("page 1 next cursor = nil, want a cursor (more pages remain)")
	}
	if next.ID != second.ID {
		t.Fatalf("next cursor ID = %s, want %s (last returned row)", next.ID, second.ID)
	}

	page2, next2, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Limit: 2, Before: &next.ClosedAt, BeforeID: next.ID})
	if err != nil {
		t.Fatalf("list closed tasks page 2: %v", err)
	}
	if got := taskIDs(page2); !equalStrings(got, []string{first.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s]", got, first.ID)
	}
	if next2 != nil {
		t.Fatalf("page 2 next cursor = %+v, want nil", next2)
	}
}

func TestListClosedTasksTieBreaksByIDWithoutSkipOrDuplicate(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	at := clock.Add(time.Hour)

	// Two tasks closed at the exact same instant: id desc breaks the tie.
	first := finishTask(t, service, clock, "first", at, ResolutionAbandoned)   // i-0001
	second := finishTask(t, service, clock, "second", at, ResolutionAbandoned) // i-0002

	page1, next, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Limit: 1})
	if err != nil {
		t.Fatalf("list closed tasks page 1: %v", err)
	}
	if got := taskIDs(page1); !equalStrings(got, []string{second.ID}) {
		t.Fatalf("page 1 ids = %v, want [%s] (higher id first on tie)", got, second.ID)
	}
	if next == nil || next.ID != second.ID {
		t.Fatalf("next cursor = %+v, want ID %s", next, second.ID)
	}

	page2, next2, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Limit: 1, Before: &next.ClosedAt, BeforeID: next.ID})
	if err != nil {
		t.Fatalf("list closed tasks page 2: %v", err)
	}
	if got := taskIDs(page2); !equalStrings(got, []string{first.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s] (no skip, no duplicate across the tie)", got, first.ID)
	}
	if next2 != nil {
		t.Fatalf("page 2 next cursor = %+v, want nil", next2)
	}
}

func TestListClosedTasksFiltersByOutcome(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	merged := createTasks(t, service, "merged")[0]
	insertChangeForTest(t, service.db, merged.ID, "c-0001", "feat/merged", true)
	*clock = base.Add(3 * time.Hour)
	mergedAt := formatTime(*clock)
	if _, err := service.db.ExecContext(ctx, `UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(ResolutionMerged), mergedAt, mergedAt, merged.ID); err != nil {
		t.Fatalf("finish merged task: %v", err)
	}

	rejected := finishTask(t, service, clock, "rejected", base.Add(2*time.Hour), ResolutionRejected)
	abandoned := finishTask(t, service, clock, "abandoned", base.Add(time.Hour), ResolutionAbandoned)

	cases := []struct {
		outcome ClosedOutcome
		want    []string
	}{
		{ClosedOutcomeAll, []string{merged.ID, rejected.ID, abandoned.ID}},
		{ClosedOutcomeMerged, []string{merged.ID}},
		{ClosedOutcomeRejected, []string{rejected.ID}},
		{ClosedOutcomeAbandoned, []string{abandoned.ID}},
	}
	for _, tc := range cases {
		tasks, _, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Outcome: tc.outcome})
		if err != nil {
			t.Fatalf("list closed tasks outcome=%q: %v", tc.outcome, err)
		}
		if got := taskIDs(tasks); !equalStrings(got, tc.want) {
			t.Fatalf("outcome=%q ids = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}

func TestListClosedTasksWithinWindow(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	old := finishTask(t, service, clock, "old", base, ResolutionAbandoned)
	recent := finishTask(t, service, clock, "recent", base.Add(48*time.Hour), ResolutionAbandoned)

	cutoff := base.Add(24 * time.Hour)
	tasks, _, err := service.ListClosedTasks(ctx, ClosedTaskQuery{Within: &cutoff})
	if err != nil {
		t.Fatalf("list closed tasks within: %v", err)
	}
	if got := taskIDs(tasks); !equalStrings(got, []string{recent.ID}) {
		t.Fatalf("within ids = %v, want [%s] (only recent), old=%s", got, recent.ID, old.ID)
	}
}

func TestCountClosedTasksCountsOnlyClosed(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	finishTask(t, service, clock, "closed one", base, ResolutionAbandoned)
	finishTask(t, service, clock, "closed two", base.Add(time.Hour), ResolutionAbandoned)
	createTasks(t, service, "open one", "open two", "open three")

	count, err := service.CountClosedTasks(ctx)
	if err != nil {
		t.Fatalf("count closed tasks: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountClosedTasks = %d, want 2", count)
	}
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
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
