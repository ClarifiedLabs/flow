//go:build sqlite_fts5

package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func openSearchTestDB(t *testing.T) (*TaskService, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewTaskService(store.DB(), "p-test"), ctx
}

func TestSearchTasksFTSRanksAndMatches(t *testing.T) {
	tasks, ctx := openSearchTestDB(t)

	mk := func(title, body string) Task {
		task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
			Task: CreateTaskInput{Title: title, Body: body, CreatedBy: ActorHuman},
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return task
	}
	mk("Fix login redirect loop", "users get bounced between /login and /")
	mk("Improve dashboard performance", "the metrics page is slow to load")
	mk("Redirect old blog URLs", "301 the legacy posts")

	hits, err := tasks.SearchTasks(ctx, "redirect", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("redirect hits = %d, want 2: %+v", len(hits), hits)
	}

	// prefix match: "perfor" should hit "performance".
	prefix, err := tasks.SearchTasks(ctx, "perfor", 0)
	if err != nil {
		t.Fatalf("prefix search: %v", err)
	}
	if len(prefix) != 1 || prefix[0].Title != "Improve dashboard performance" {
		t.Fatalf("prefix hits = %+v", prefix)
	}

	// body-only match.
	body, err := tasks.SearchTasks(ctx, "bounced", 0)
	if err != nil {
		t.Fatalf("body search: %v", err)
	}
	if len(body) != 1 || body[0].Title != "Fix login redirect loop" {
		t.Fatalf("body hits = %+v", body)
	}
}

func TestSearchTasksFTSSanitizesAndFallsBack(t *testing.T) {
	tasks, ctx := openSearchTestDB(t)
	if _, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Quarterly report (Q3)", Body: "numbers", CreatedBy: ActorHuman},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// FTS operator/injection attempts must not error or match everything.
	for _, evil := range []string{`" OR "1`, `report AND (`, `*`, `NEAR(`, `"unclosed`, `Q3:*`} {
		if _, err := tasks.SearchTasks(ctx, evil, 0); err != nil {
			t.Fatalf("search %q errored: %v", evil, err)
		}
	}

	// A query that tokenizes to nothing falls back to LIKE substring.
	hits, err := tasks.SearchTasks(ctx, "(Q3)", 0)
	if err != nil {
		t.Fatalf("fallback search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("LIKE fallback found no hits for (Q3)")
	}
}

func TestSearchTasksTriggerStaysInSync(t *testing.T) {
	tasks, ctx := openSearchTestDB(t)
	task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Original title", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update title: the FTS index must reflect the new text, not the old.
	if _, err := tasks.EditTask(ctx, task.ID, EditTaskInput{Title: strPtr("Renamed widget")}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if hits, _ := tasks.SearchTasks(ctx, "widget", 0); len(hits) != 1 {
		t.Fatalf("after rename search 'widget' = %+v, want 1", hits)
	}
	if hits, _ := tasks.SearchTasks(ctx, "Original", 0); len(hits) != 0 {
		t.Fatalf("after rename search 'Original' = %+v, want 0", hits)
	}
}

func strPtr(s string) *string { return &s }
