//go:build !sqlite_fts5

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

func TestSearchTasksLikeFallbackWithoutFTS5(t *testing.T) {
	if fts5Available {
		t.Fatalf("fts5Available = true under the !sqlite_fts5 build")
	}
	tasks, ctx := openSearchTestDB(t)
	if _, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Fix login redirect", Body: "bounced users", CreatedBy: ActorHuman},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	hits, err := tasks.SearchTasks(ctx, "redirect", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Title != "Fix login redirect" {
		t.Fatalf("hits = %+v, want the redirect task", hits)
	}

	// substring across body
	body, err := tasks.SearchTasks(ctx, "bounced", 0)
	if err != nil || len(body) != 1 {
		t.Fatalf("body substring = %+v, %v", body, err)
	}

	// no match
	if none, err := tasks.SearchTasks(ctx, "nonexistent", 0); err != nil || len(none) != 0 {
		t.Fatalf("no-match = %+v, %v", none, err)
	}
}
