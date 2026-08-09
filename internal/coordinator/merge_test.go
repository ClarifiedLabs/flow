package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/db"
)

func TestEnsureMergeIntentReplacesIntentForPriorRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath := createReconcileGitRepo(t)
	exchangePath := filepath.Join(t.TempDir(), "exchange.git")
	if err := runReconcileGit("", nil, "init", "--bare", exchangePath); err != nil {
		t.Fatalf("initialize exchange: %v", err)
	}
	if err := runReconcileGit("", nil, "--git-dir", exchangePath, "fetch", repoPath, "main:main"); err != nil {
		t.Fatalf("seed exchange main: %v", err)
	}
	firstBase, err := reconcileGitOutput("", nil, "--git-dir", exchangePath, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve first base: %v", err)
	}

	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open coordinator database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Replace stale merge intent"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	const changeID = "ch-stale-intent"
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, 'task/t-test-0001/run-1', 'main', 'old-head',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	service := NewMergeService(store.DB(), tasks, nil, Project{
		ID: "p-test", BaseBranch: "main", ExchangePath: exchangePath,
	})
	change := Change{ID: changeID, TaskID: task.ID, Base: "main", HeadSHA: "old-head"}
	first, err := service.ensureMergeIntent(ctx, task.ID, change, exchangePath)
	if err != nil {
		t.Fatalf("create first merge intent: %v", err)
	}
	if first.HeadSHA != "old-head" || first.PreviousBaseSHA != firstBase {
		t.Fatalf("first merge intent = %+v", first)
	}

	writeReconcileFile(t, repoPath, "README.md", "advanced base\n")
	if err := runReconcileGit(repoPath, nil, "add", "README.md"); err != nil {
		t.Fatalf("stage base advance: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "advance base"); err != nil {
		t.Fatalf("commit base advance: %v", err)
	}
	if err := runReconcileGit("", nil, "--git-dir", exchangePath, "fetch", repoPath, "+main:main"); err != nil {
		t.Fatalf("advance exchange main: %v", err)
	}
	secondBase, err := reconcileGitOutput("", nil, "--git-dir", exchangePath, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve second base: %v", err)
	}

	change.HeadSHA = "new-head"
	second, err := service.ensureMergeIntent(ctx, task.ID, change, exchangePath)
	if err != nil {
		t.Fatalf("replace stale merge intent: %v", err)
	}
	if second.ID == first.ID || second.HeadSHA != "new-head" || second.PreviousBaseSHA != secondBase {
		t.Fatalf("replacement merge intent = %+v, first = %+v", second, first)
	}

	var count int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM merge_intents WHERE change_id = ? AND completed_at IS NULL`, changeID).Scan(&count); err != nil {
		t.Fatalf("count open merge intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("open merge intents = %d, want 1", count)
	}
}
