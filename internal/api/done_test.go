package api

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func setIssueDoneAtForTest(t *testing.T, db *sql.DB, issueID, doneAt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `UPDATE issues SET done_at = ? WHERE id = ?`, doneAt, issueID); err != nil {
		t.Fatalf("set done_at for %s: %v", issueID, err)
	}
}

func insertMergedChangeForTest(t *testing.T, db *sql.DB, issueID, changeID string) {
	t.Helper()
	const ts = "2026-01-01T00:00:00.000000000Z"
	if _, err := db.ExecContext(context.Background(), `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?, ?)`,
		changeID, issueID, "feat/"+changeID, "1111111111111111111111111111111111111111",
		ts, ts, ts, ts); err != nil {
		t.Fatalf("insert merged change %s: %v", changeID, err)
	}
}

// seedClosedIssues creates three terminal issues with deterministic done_at
// ordering (merged newest).
func seedClosedIssues(t *testing.T, fixture testFixture) (merged, rejected, abandoned coordinator.Issue) {
	t.Helper()
	ctx := context.Background()

	merged, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "merged work"})
	if err != nil {
		t.Fatalf("create merged issue: %v", err)
	}
	insertMergedChangeForTest(t, fixture.DB, merged.ID, "c-0001")
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, merged.ID, coordinator.ResolutionCompleted, "merged", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish merged issue: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE issues SET done_resolution = ? WHERE id = ?`, string(coordinator.ResolutionMerged), merged.ID); err != nil {
		t.Fatalf("stamp merged resolution: %v", err)
	}

	rejected, err = fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "rejected work"})
	if err != nil {
		t.Fatalf("create rejected issue: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, rejected.ID, coordinator.ResolutionRejected, "rejected", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish rejected issue: %v", err)
	}

	abandoned, err = fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "abandoned work"})
	if err != nil {
		t.Fatalf("create abandoned issue: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, abandoned.ID, coordinator.ResolutionAbandoned, "abandoned", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish abandoned issue: %v", err)
	}

	setIssueDoneAtForTest(t, fixture.DB, merged.ID, "2026-03-03T03:00:00.000000000Z")
	setIssueDoneAtForTest(t, fixture.DB, rejected.ID, "2026-02-02T02:00:00.000000000Z")
	setIssueDoneAtForTest(t, fixture.DB, abandoned.ID, "2026-01-01T01:00:00.000000000Z")
	return merged, rejected, abandoned
}

func TestDoneAggregateSurfacesClosedIssuesWithOutcomesAndMergedChange(t *testing.T) {
	fixture := newTestFixture(t)
	merged, rejected, abandoned := seedClosedIssues(t, fixture)

	var done aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done", nil, http.StatusOK, &done)

	if len(done.Done) != 1 {
		t.Fatalf("aggregate projects = %d, want 1", len(done.Done))
	}
	project := done.Done[0]

	gotIDs := make([]string, len(project.Issues))
	for i, issue := range project.Issues {
		gotIDs[i] = issue.ID
	}
	want := []string{merged.ID, rejected.ID, abandoned.ID}
	if !equalStringSlices(gotIDs, want) {
		t.Fatalf("done issue order = %v, want %v (newest closed first)", gotIDs, want)
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

	mergedCard, ok := project.IssueCards[merged.ID]
	if !ok || mergedCard.Change == nil || mergedCard.Change.MergedAt == nil {
		t.Fatalf("merged card = %+v, want a change with MergedAt set", mergedCard)
	}
	if mergedCard.Change.ID != "c-0001" {
		t.Fatalf("merged card change id = %q, want c-0001", mergedCard.Change.ID)
	}
	if card := project.IssueCards[abandoned.ID]; card.Change != nil {
		t.Fatalf("abandoned card change = %+v, want nil", card.Change)
	}
}

func TestDoneAggregateKeysetPagination(t *testing.T) {
	fixture := newTestFixture(t)
	merged, rejected, abandoned := seedClosedIssues(t, fixture)

	var page1 aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done?limit=2", nil, http.StatusOK, &page1)
	first := page1.Done[0]
	if got := issueIDsFromAPI(first.Issues); !equalStringSlices(got, []string{merged.ID, rejected.ID}) {
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
	if got := issueIDsFromAPI(second.Issues); !equalStringSlices(got, []string{abandoned.ID}) {
		t.Fatalf("page 2 ids = %v, want [%s]", got, abandoned.ID)
	}
	if second.NextBefore != "" {
		t.Fatalf("page 2 cursor = %q, want empty", second.NextBefore)
	}
}

func TestDoneAggregateFiltersByOutcome(t *testing.T) {
	fixture := newTestFixture(t)
	merged, _, _ := seedClosedIssues(t, fixture)

	var done aggregateDoneResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/done?outcome=merged", nil, http.StatusOK, &done)
	if got := issueIDsFromAPI(done.Done[0].Issues); !equalStringSlices(got, []string{merged.ID}) {
		t.Fatalf("outcome=merged ids = %v, want [%s]", got, merged.ID)
	}
}

func TestSidebarReportsClosedCount(t *testing.T) {
	fixture := newTestFixture(t)
	seedClosedIssues(t, fixture)

	var sidebar sidebarResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/sidebar", nil, http.StatusOK, &sidebar)
	if sidebar.Done != 3 {
		t.Fatalf("sidebar done count = %d, want 3", sidebar.Done)
	}
}

func issueIDsFromAPI(issues []coordinator.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
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
