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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	at := clock.Add(time.Hour)

	// Two tasks closed at the exact same instant: id desc breaks the tie.
	first := finishTask(t, service, clock, "first", at, ResolutionAbandoned)   // t-test-0001
	second := finishTask(t, service, clock, "second", at, ResolutionAbandoned) // t-test-0002

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestCountClosedTasksByWindowCumulativeBucketsAtBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	// Completed tasks at every boundary: exactly at the edge, one second
	// inside, and one second outside.
	finishTask(t, service, clock, "completed inside 15m", base.Add(-15*time.Minute+time.Second), ResolutionCompleted)
	finishTask(t, service, clock, "completed exact 15m", base.Add(-15*time.Minute), ResolutionCompleted)
	finishTask(t, service, clock, "completed outside 15m", base.Add(-15*time.Minute-time.Second), ResolutionCompleted)
	finishTask(t, service, clock, "completed outside 30m", base.Add(-30*time.Minute-time.Second), ResolutionCompleted)
	finishTask(t, service, clock, "completed outside 1h", base.Add(-time.Hour-time.Second), ResolutionCompleted)
	finishTask(t, service, clock, "completed exact 24h", base.Add(-24*time.Hour), ResolutionCompleted)
	finishTask(t, service, clock, "completed outside 24h", base.Add(-24*time.Hour-time.Second), ResolutionCompleted)
	// Merged is a successful outcome and counts like completed.
	finishTask(t, service, clock, "merged inside 15m", base.Add(-10*time.Minute), ResolutionMerged)
	// Unsuccessful resolutions never count.
	finishTask(t, service, clock, "rejected", base.Add(-5*time.Minute), ResolutionRejected)
	finishTask(t, service, clock, "abandoned", base.Add(-5*time.Minute), ResolutionAbandoned)
	finishTask(t, service, clock, "cancelled", base.Add(-5*time.Minute), ResolutionCancelled)
	finishTask(t, service, clock, "failed", base.Add(-5*time.Minute), ResolutionFailed)
	*clock = base // restore the fixed now for the window math

	windows := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 24 * time.Hour}
	counts, err := service.CountClosedTasksByWindow(ctx, windows, []DoneResolution{ResolutionCompleted, ResolutionMerged})
	if err != nil {
		t.Fatalf("count closed tasks by window: %v", err)
	}

	want := map[time.Duration]int{
		15 * time.Minute: 3, // inside + exact 15m + merged at 10m
		30 * time.Minute: 4, // + outside 15m
		time.Hour:        5, // + outside 30m
		24 * time.Hour:   7, // + outside 1h + exact 24h (outside 24h never counts)
	}
	if len(counts) != len(windows) {
		t.Fatalf("bucket count = %d, want %d", len(counts), len(windows))
	}
	for i, window := range windows {
		if counts[i].Window != window {
			t.Fatalf("bucket %d window = %s, want %s", i, counts[i].Window, window)
		}
		if counts[i].Count != want[window] {
			t.Fatalf("bucket %s count = %d, want %d", window, counts[i].Count, want[window])
		}
	}

	// The per-outcome breakdown is cumulative too: 2 completed (inside 15m +
	// exact 15m) and 1 merged inside the 15m window.
	byOutcome := counts[0].ByOutcome
	if byOutcome[ResolutionCompleted] != 2 || byOutcome[ResolutionMerged] != 1 {
		t.Fatalf("15m by outcome = %+v, want completed 2 merged 1", byOutcome)
	}
	if _, ok := byOutcome[ResolutionRejected]; ok {
		t.Fatalf("15m by outcome includes rejected: %+v", byOutcome)
	}
}

func TestCountClosedTasksByWindowUsesOneClockSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	// A completion exactly at now-15m must count in the 15m bucket even when
	// the injected clock moves forward during the request. Pre-fix, the SQL
	// lower bound came from an earlier s.now() snapshot than the in-memory
	// bucket cutoffs, so this task passed the query but was skipped as Before
	// the later cutoff.
	finishTask(t, service, clock, "completed exact 15m", base.Add(-15*time.Minute), ResolutionCompleted)

	// The clock stays at base for the first read (the SQL bound) and then jumps
	// 10s forward for every later read (the bucket cutoffs): with two snapshots
	// the boundary task falls out of the 15m bucket, with one it does not.
	reads := 0
	service.now = func() time.Time {
		reads++
		if reads == 1 {
			return base
		}
		return base.Add(10 * time.Second)
	}

	counts, err := service.CountClosedTasksByWindow(ctx, []time.Duration{15 * time.Minute, time.Hour}, []DoneResolution{ResolutionCompleted})
	if err != nil {
		t.Fatalf("count closed tasks by window: %v", err)
	}
	if counts[0].Count != 1 {
		t.Fatalf("15m count = %d, want 1 (task at exactly now-15m must count under one stable now)", counts[0].Count)
	}
	if counts[1].Count != 1 {
		t.Fatalf("1h count = %d, want 1", counts[1].Count)
	}

	// The whole point of the fix is one stable snapshot: the SQL lower bound
	// and every in-memory bucket cutoff must come from a single s.now() read.
	if reads != 1 {
		t.Fatalf("s.now() reads = %d, want exactly 1: the query bound and every bucket cutoff must share one snapshot", reads)
	}
}

func TestCountClosedTasksByWindowExcludesOpenAndNullDoneAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, clock := newClosedTaskFixture(t)
	base := *clock

	finishTask(t, service, clock, "completed", base.Add(-time.Minute), ResolutionCompleted)

	// Open tasks have a NULL done_at and a non-done lifecycle (the schema CHECK
	// ties lifecycle_state = 'done' to a non-NULL done_at/done_resolution, so
	// those invariants cannot drift apart); they must never count.
	createTasks(t, service, "open one", "open two")

	*clock = base
	counts, err := service.CountClosedTasksByWindow(ctx, []time.Duration{time.Hour}, []DoneResolution{ResolutionCompleted})
	if err != nil {
		t.Fatalf("count closed tasks by window: %v", err)
	}
	if len(counts) != 1 || counts[0].Count != 1 {
		t.Fatalf("counts = %+v, want exactly one (only the properly finished task)", counts)
	}
}

func TestCountClosedTasksByWindowValidatesWindowsAndOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _ := newClosedTaskFixture(t)

	if _, err := service.CountClosedTasksByWindow(ctx, nil, []DoneResolution{ResolutionCompleted}); err == nil {
		t.Fatal("nil windows accepted, want error")
	}
	if _, err := service.CountClosedTasksByWindow(ctx, []time.Duration{0}, []DoneResolution{ResolutionCompleted}); err == nil {
		t.Fatal("zero window accepted, want error")
	}
	if _, err := service.CountClosedTasksByWindow(ctx, []time.Duration{-time.Minute}, []DoneResolution{ResolutionCompleted}); err == nil {
		t.Fatal("negative window accepted, want error")
	}
	if _, err := service.CountClosedTasksByWindow(ctx, []time.Duration{time.Hour}, nil); err == nil {
		t.Fatal("nil outcomes accepted, want error")
	}
	if _, err := service.CountClosedTasksByWindow(ctx, []time.Duration{time.Hour}, []DoneResolution{"bogus"}); err == nil {
		t.Fatal("unknown outcome accepted, want error")
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
