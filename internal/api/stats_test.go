package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// completionStatLabels are the exact window labels /v2/stats/completions must
// return, in ascending order.
var completionStatLabels = []string{"15m", "30m", "1h", "2h", "4h", "6h", "12h", "24h"}

// finishCompletedTask creates a task in the bundle, force-done it, and pins its
// done_at so completion-stats windows are deterministic.
func finishCompletedTask(t *testing.T, bundle *ProjectBundle, title string, resolution coordinator.DoneResolution, doneAt time.Time) coordinator.Task {
	t.Helper()
	ctx := context.Background()
	task, err := bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: title})
	if err != nil {
		t.Fatalf("create task %s: %v", title, err)
	}
	if _, err := bundle.WorkflowRuns.ForceDone(ctx, task.ID, coordinator.ResolutionCompleted, "done", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish task %s: %v", title, err)
	}
	if _, err := bundle.Store.DB().ExecContext(ctx, `UPDATE tasks SET done_resolution = ? WHERE id = ?`, string(resolution), task.ID); err != nil {
		t.Fatalf("stamp resolution %s on %s: %v", resolution, task.ID, err)
	}
	setTaskDoneAtForTest(t, bundle.Store.DB(), task.ID, sqlitex.FormatTime(doneAt))
	return task
}

func completionBucketCount(t *testing.T, resp completionStatsResponse, label string) int {
	t.Helper()
	for _, bucket := range resp.Buckets {
		if bucket.Window == label {
			return bucket.Count
		}
	}
	t.Fatalf("bucket %q not found in %+v", label, resp.Buckets)
	return 0
}

func TestCompletionStatsReturnsEightBucketsInOrderAndCountsOnlySuccessfulOutcomes(t *testing.T) {
	fixture := newTestFixture(t)
	now := time.Now().UTC()

	finishCompletedTask(t, fixture.Bundle, "completed work", coordinator.ResolutionCompleted, now.Add(-time.Minute))
	finishCompletedTask(t, fixture.Bundle, "merged work", coordinator.ResolutionMerged, now.Add(-10*time.Minute))
	finishCompletedTask(t, fixture.Bundle, "rejected work", coordinator.ResolutionRejected, now.Add(-time.Minute))

	var resp completionStatsResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &resp)

	if len(resp.Buckets) != len(completionStatLabels) {
		t.Fatalf("buckets = %d, want %d", len(resp.Buckets), len(completionStatLabels))
	}
	for i, want := range completionStatLabels {
		if resp.Buckets[i].Window != want {
			t.Fatalf("bucket %d window = %q, want %q (ascending order)", i, resp.Buckets[i].Window, want)
		}
		if resp.Buckets[i].Count != 2 {
			t.Fatalf("bucket %s count = %d, want 2 (rejected must not count)", want, resp.Buckets[i].Count)
		}
	}

	byOutcome := resp.ByOutcome["24h"]
	if byOutcome == nil {
		t.Fatalf("by_outcome missing 24h: %+v", resp.ByOutcome)
	}
	if byOutcome["completed"] != 1 || byOutcome["merged"] != 1 {
		t.Fatalf("24h by outcome = %+v, want completed 1 merged 1", byOutcome)
	}
	if len(resp.ByOutcome) != len(completionStatLabels) {
		t.Fatalf("by_outcome windows = %d, want %d", len(resp.ByOutcome), len(completionStatLabels))
	}
}

func TestCompletionStatsSumsAcrossProjectsAndNarrowsByProjectFilter(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	alpha, beta := bundles[0], bundles[1]
	now := time.Now().UTC()

	finishCompletedTask(t, alpha, "alpha one", coordinator.ResolutionCompleted, now.Add(-time.Minute))
	finishCompletedTask(t, alpha, "alpha two", coordinator.ResolutionCompleted, now.Add(-5*time.Minute))
	finishCompletedTask(t, alpha, "alpha rejected", coordinator.ResolutionRejected, now.Add(-time.Minute))
	finishCompletedTask(t, beta, "beta completed", coordinator.ResolutionCompleted, now.Add(-time.Minute))
	finishCompletedTask(t, beta, "beta merged", coordinator.ResolutionMerged, now.Add(-time.Minute))

	var all completionStatsResponse
	doJSONRequest(t, server, http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &all)
	for _, label := range completionStatLabels {
		if got := completionBucketCount(t, all, label); got != 4 {
			t.Fatalf("aggregate bucket %s count = %d, want 4 (alpha 2 + beta 2)", label, got)
		}
	}

	var alphaOnly completionStatsResponse
	doJSONRequest(t, server, http.MethodGet, "/v2/stats/completions?project="+alpha.Project.ID, nil, http.StatusOK, &alphaOnly)
	if got := completionBucketCount(t, alphaOnly, "24h"); got != 2 {
		t.Fatalf("alpha bucket 24h count = %d, want 2", got)
	}
	if byOutcome := alphaOnly.ByOutcome["24h"]; byOutcome["completed"] != 2 || byOutcome["merged"] != 0 {
		t.Fatalf("alpha 24h by outcome = %+v, want completed 2 merged 0", byOutcome)
	}

	var betaOnly completionStatsResponse
	doJSONRequest(t, server, http.MethodGet, "/v2/stats/completions?project="+beta.Project.ID, nil, http.StatusOK, &betaOnly)
	if got := completionBucketCount(t, betaOnly, "24h"); got != 2 {
		t.Fatalf("beta bucket 24h count = %d, want 2", got)
	}
	if byOutcome := betaOnly.ByOutcome["24h"]; byOutcome["completed"] != 1 || byOutcome["merged"] != 1 {
		t.Fatalf("beta 24h by outcome = %+v, want completed 1 merged 1", byOutcome)
	}
}

func TestCompletionStatsSessionTokenCannotReadOtherProject(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	alpha, beta := bundles[0], bundles[1]
	now := time.Now().UTC()

	finishCompletedTask(t, alpha, "alpha one", coordinator.ResolutionCompleted, now.Add(-time.Minute))
	finishCompletedTask(t, beta, "beta one", coordinator.ResolutionCompleted, now.Add(-time.Minute))
	finishCompletedTask(t, beta, "beta two", coordinator.ResolutionCompleted, now.Add(-5*time.Minute))

	ctx := context.Background()
	if err := server.registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
		Token:     "alpha-session-token",
		Scope:     coordinator.TokenScopeSession,
		Subject:   "s-alpha",
		ProjectID: &alpha.Project.ID,
	}); err != nil {
		t.Fatalf("store alpha session token: %v", err)
	}

	// A project-bound token asking for another project's completions is denied.
	doJSONRequestAs(t, server, "alpha-session-token", http.MethodGet,
		"/v2/stats/completions?project="+beta.Project.ID, nil, http.StatusForbidden, nil)

	// The same token still reads its own project and never sees beta's counts,
	// whether it asks for its project explicitly or reads the aggregate.
	var alphaScoped completionStatsResponse
	doJSONRequestAs(t, server, "alpha-session-token", http.MethodGet,
		"/v2/stats/completions?project="+alpha.Project.ID, nil, http.StatusOK, &alphaScoped)
	if got := completionBucketCount(t, alphaScoped, "24h"); got != 1 {
		t.Fatalf("session alpha bucket 24h count = %d, want 1 (alpha only)", got)
	}
	if byOutcome := alphaScoped.ByOutcome["24h"]; byOutcome["completed"] != 1 {
		t.Fatalf("session alpha 24h by outcome = %+v, want completed 1", byOutcome)
	}

	var alphaAggregate completionStatsResponse
	doJSONRequestAs(t, server, "alpha-session-token", http.MethodGet,
		"/v2/stats/completions", nil, http.StatusOK, &alphaAggregate)
	if got := completionBucketCount(t, alphaAggregate, "24h"); got != 1 {
		t.Fatalf("session aggregate bucket 24h count = %d, want 1 (beta hidden)", got)
	}
}

func TestCompletionStatsUpdatesWhenTaskTransitionsToDoneAndIsReadScoped(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	open, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "open work"})
	if err != nil {
		t.Fatalf("create open task: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "stats-session-token",
		Scope:        coordinator.TokenScopeSession,
		Subject:      "s-stats",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &open.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	var before completionStatsResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &before)
	for _, label := range completionStatLabels {
		if got := completionBucketCount(t, before, label); got != 0 {
			t.Fatalf("bucket %s before transition = %d, want 0", label, got)
		}
	}

	if _, err := fixture.Bundle.WorkflowRuns.ForceDone(ctx, open.ID, coordinator.ResolutionCompleted, "done", coordinator.ActorHuman); err != nil {
		t.Fatalf("finish task: %v", err)
	}

	var after completionStatsResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &after)
	for _, label := range completionStatLabels {
		if got := completionBucketCount(t, after, label); got != 1 {
			t.Fatalf("bucket %s after transition = %d, want 1", label, got)
		}
	}

	// Move the completion just inside the 2h window: the 15m..1h buckets empty
	// out while 2h..24h keep the task.
	now := time.Now().UTC()
	setTaskDoneAtForTest(t, fixture.DB, open.ID, sqlitex.FormatTime(now.Add(-2*time.Hour+time.Minute)))
	var shifted completionStatsResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &shifted)
	for _, label := range []string{"15m", "30m", "1h"} {
		if got := completionBucketCount(t, shifted, label); got != 0 {
			t.Fatalf("bucket %s after shift = %d, want 0", label, got)
		}
	}
	for _, label := range []string{"2h", "4h", "6h", "12h", "24h"} {
		if got := completionBucketCount(t, shifted, label); got != 1 {
			t.Fatalf("bucket %s after shift = %d, want 1", label, got)
		}
	}

	// The endpoint is read-scoped: a session token may read it too.
	var viaSession completionStatsResponse
	doJSONRequestAs(t, fixture.Server, "stats-session-token", http.MethodGet, "/v2/stats/completions", nil, http.StatusOK, &viaSession)
	if got := completionBucketCount(t, viaSession, "24h"); got != 1 {
		t.Fatalf("session bucket 24h count = %d, want 1", got)
	}
}
