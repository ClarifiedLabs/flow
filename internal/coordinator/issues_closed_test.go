package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// closedFixture builds a service with a controllable clock and returns it so
// tests can stamp deterministic closed_at values.
func newClosedIssueFixture(t *testing.T) (*IssueService, *time.Time) {
	t.Helper()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := &now
	service.now = func() time.Time { return *clock }
	return service, clock
}

// closeAbandoned closes an accepted issue with no merged change (-> abandoned).
func closeAbandoned(t *testing.T, service *IssueService, clock *time.Time, title string, at time.Time) Issue {
	t.Helper()
	issue := createIssues(t, service, title)[0]
	*clock = at
	closed, err := service.CloseIssue(context.Background(), issue.ID)
	if err != nil {
		t.Fatalf("close issue %s: %v", issue.ID, err)
	}
	return closed
}

func TestListClosedIssuesReturnsClosedNewestFirstAndExcludesOpen(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	base := *clock

	first := closeAbandoned(t, service, clock, "first", base)
	second := closeAbandoned(t, service, clock, "second", base.Add(time.Hour))
	third := closeAbandoned(t, service, clock, "third", base.Add(2*time.Hour))
	// An open issue must never appear.
	createIssues(t, service, "still open")

	issues, next, err := service.ListClosedIssues(ctx, ClosedIssueQuery{})
	if err != nil {
		t.Fatalf("list closed issues: %v", err)
	}
	if next != nil {
		t.Fatalf("next cursor = %+v, want nil (no more pages)", next)
	}
	gotIDs := issueIDs(issues)
	wantIDs := []string{third.ID, second.ID, first.ID}
	if !equalStrings(gotIDs, wantIDs) {
		t.Fatalf("closed issue ids = %v, want %v (newest closed first)", gotIDs, wantIDs)
	}
}

func TestListClosedIssuesKeysetPagination(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	base := *clock

	first := closeAbandoned(t, service, clock, "first", base)
	second := closeAbandoned(t, service, clock, "second", base.Add(time.Hour))
	third := closeAbandoned(t, service, clock, "third", base.Add(2*time.Hour))

	page1, next, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Limit: 2})
	if err != nil {
		t.Fatalf("list closed issues page 1: %v", err)
	}
	if got := issueIDs(page1); !equalStrings(got, []string{third.ID, second.ID}) {
		t.Fatalf("page 1 ids = %v, want [%s %s]", got, third.ID, second.ID)
	}
	if next == nil {
		t.Fatal("page 1 next cursor = nil, want a cursor (more pages remain)")
	}
	if next.ID != second.ID {
		t.Fatalf("next cursor ID = %s, want %s (last returned row)", next.ID, second.ID)
	}

	page2, next2, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Limit: 2, Before: &next.ClosedAt, BeforeID: next.ID})
	if err != nil {
		t.Fatalf("list closed issues page 2: %v", err)
	}
	if got := issueIDs(page2); !equalStrings(got, []string{first.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s]", got, first.ID)
	}
	if next2 != nil {
		t.Fatalf("page 2 next cursor = %+v, want nil", next2)
	}
}

func TestListClosedIssuesTieBreaksByIDWithoutSkipOrDuplicate(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	at := clock.Add(time.Hour)

	// Two issues closed at the exact same instant: id desc breaks the tie.
	first := closeAbandoned(t, service, clock, "first", at)   // i-0001
	second := closeAbandoned(t, service, clock, "second", at) // i-0002

	page1, next, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Limit: 1})
	if err != nil {
		t.Fatalf("list closed issues page 1: %v", err)
	}
	if got := issueIDs(page1); !equalStrings(got, []string{second.ID}) {
		t.Fatalf("page 1 ids = %v, want [%s] (higher id first on tie)", got, second.ID)
	}
	if next == nil || next.ID != second.ID {
		t.Fatalf("next cursor = %+v, want ID %s", next, second.ID)
	}

	page2, next2, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Limit: 1, Before: &next.ClosedAt, BeforeID: next.ID})
	if err != nil {
		t.Fatalf("list closed issues page 2: %v", err)
	}
	if got := issueIDs(page2); !equalStrings(got, []string{first.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s] (no skip, no duplicate across the tie)", got, first.ID)
	}
	if next2 != nil {
		t.Fatalf("page 2 next cursor = %+v, want nil", next2)
	}
}

func TestListClosedIssuesFiltersByOutcome(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	base := *clock

	// merged_closed: accepted issue with a merged change, then closed.
	merged := createIssues(t, service, "merged")[0]
	insertChangeForTest(t, service.db, merged.ID, "c-0001", "feat/merged", true)
	*clock = base.Add(3 * time.Hour)
	if _, err := service.CloseIssue(ctx, merged.ID); err != nil {
		t.Fatalf("close merged issue: %v", err)
	}

	// rejected_closed.
	rejected := createIssues(t, service, "rejected")[0]
	*clock = base.Add(2 * time.Hour)
	if _, err := service.RejectTriage(ctx, rejected.ID); err != nil {
		t.Fatalf("reject issue: %v", err)
	}

	// abandoned: closed with no merged change.
	abandoned := closeAbandoned(t, service, clock, "abandoned", base.Add(time.Hour))

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
		issues, _, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Outcome: tc.outcome})
		if err != nil {
			t.Fatalf("list closed issues outcome=%q: %v", tc.outcome, err)
		}
		if got := issueIDs(issues); !equalStrings(got, tc.want) {
			t.Fatalf("outcome=%q ids = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}

func TestListClosedIssuesWithinWindow(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	base := *clock

	old := closeAbandoned(t, service, clock, "old", base)
	recent := closeAbandoned(t, service, clock, "recent", base.Add(48*time.Hour))

	cutoff := base.Add(24 * time.Hour)
	issues, _, err := service.ListClosedIssues(ctx, ClosedIssueQuery{Within: &cutoff})
	if err != nil {
		t.Fatalf("list closed issues within: %v", err)
	}
	if got := issueIDs(issues); !equalStrings(got, []string{recent.ID}) {
		t.Fatalf("within ids = %v, want [%s] (only recent), old=%s", got, recent.ID, old.ID)
	}
}

func TestCountClosedIssuesCountsOnlyClosed(t *testing.T) {
	ctx := context.Background()
	service, clock := newClosedIssueFixture(t)
	base := *clock

	closeAbandoned(t, service, clock, "closed one", base)
	closeAbandoned(t, service, clock, "closed two", base.Add(time.Hour))
	createIssues(t, service, "open one", "open two", "open three")

	count, err := service.CountClosedIssues(ctx)
	if err != nil {
		t.Fatalf("count closed issues: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountClosedIssues = %d, want 2", count)
	}
}

func issueIDs(issues []Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
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
