package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

// TestReviewCycleBudgetGetDefaultsWithoutPersistedRow covers callers that only
// read a task's budget. Get must find the migrated table, return the configured
// default for a task without a row, and remain read-only.
func TestReviewCycleBudgetGetDefaultsWithoutPersistedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	task, err := NewTaskService(store.DB(), "p-test").CreateTask(ctx, CreateTaskInput{Title: "Read review cycle budget"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	budget, err := NewReviewCycleService(store.DB(), 3).Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get review cycle budget: %v", err)
	}
	if budget.TaskID != task.ID || budget.GrantedCycles != 3 || budget.UsedCycles != 0 || budget.RemainingCycles != 3 || budget.Exhausted {
		t.Fatalf("default review cycle budget = %+v", budget)
	}

	var rows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_cycle_budgets WHERE task_id = ?`, task.ID).Scan(&rows); err != nil {
		t.Fatalf("count persisted review cycle budgets: %v", err)
	}
	if rows != 0 {
		t.Fatalf("persisted review cycle budgets = %d, want 0 after Get", rows)
	}
}
