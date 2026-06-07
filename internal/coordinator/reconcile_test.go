package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
)

// TestReconcileRestoresChangeProjectionWithoutReadingHandoffRef proves the
// change projection is rebuilt from the branch tip while a committed handoff
// file on that branch is ignored: the coordinator's handoff snapshot is written
// solely by PutHandoff (flow ready / flow handoff write), never re-read from git.
func TestReconcileRestoresChangeProjectionWithoutReadingHandoffRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	store := fixture.store
	issues := NewIssueService(store.DB())
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Reconciled issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	// Commit a stray handoff file on the branch: reconcile must NOT read it.
	handoffContents := handoff.RenderTemplate(handoff.TemplateInput{
		IssueID:               issue.ID,
		Branch:                branch,
		Base:                  "main",
		UpdatedAt:             time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC),
		CurrentGoal:           "Restore branch projection.",
		CompletedWork:         "Created WIP branch.",
		RemainingWork:         "Run reconcile.",
		TestsRun:              "Not yet.",
		FailedApproaches:      "None.",
		ImportantFiles:        "internal/coordinator/reconcile.go",
		NextRecommendedAction: "Run reconciliation.",
	})
	writeReconcileFile(t, repoPath, "feature.txt", "work\n")
	writeReconcileFile(t, repoPath, ".handoff.md", handoffContents)
	if err := runReconcileGit(repoPath, nil, "add", "feature.txt", ".handoff.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "work on issue"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	headSHA, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read head sha: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}

	reconciler := NewReconcileService(store.DB())
	reconcileResult, err := reconciler.Reconcile(ctx, fixture.project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconcileResult.BranchesScanned != 1 || reconcileResult.ChangesCreated != 1 {
		t.Fatalf("reconcile result = %+v", reconcileResult)
	}

	var changeID string
	var head string
	if err := store.DB().QueryRowContext(ctx, `
SELECT id, head_sha
FROM changes
WHERE issue_id = ? AND branch = ?`, issue.ID, branch).Scan(&changeID, &head); err != nil {
		t.Fatalf("load reconciled change: %v", err)
	}
	if head != headSHA {
		t.Fatalf("change head = %s, want %s", head, headSHA)
	}
	// The committed handoff file is never projected into a snapshot.
	if _, err := reconciler.GetHandoffSnapshot(ctx, changeID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get handoff snapshot err = %v, want sql.ErrNoRows (reconcile must ignore committed handoff)", err)
	}
}

// TestReconcileIsolatesPoisonedProjectAndScansOthers exercises the
// coordinator-wide pass: each project is reconciled independently and the
// results are merged, so a project whose exchange is unreadable surfaces a
// joined error and a skip while the healthy project's branches still project.
func TestReconcileIsolatesPoisonedProjectAndScansOthers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	store := fixture.store
	reconciler := NewReconcileService(store.DB())

	// A poisoned project whose exchange_path points at a non-repo directory:
	// reconciling it fails to list refs and skips the project.
	poisonPath := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(poisonPath, 0o755); err != nil {
		t.Fatalf("create poison dir: %v", err)
	}
	poisonProject := Project{
		ID:           "p-0",
		Name:         "poison",
		RepoPath:     poisonPath,
		BaseBranch:   "main",
		ExchangeName: "flow",
		ExchangeURL:  "ssh://example.com/poison.git",
		ExchangePath: poisonPath,
	}

	issues := NewIssueService(store.DB())
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Reconciled issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	writeReconcileFile(t, repoPath, "feature.txt", "work\n")
	if err := runReconcileGit(repoPath, nil, "add", "feature.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "work on issue"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}

	// The coordinator-wide pass: reconcile each project, joining errors and
	// merging results. The poisoned project must not starve the healthy one.
	var merged ReconcileResult
	var joinedErr error
	for _, project := range []Project{poisonProject, fixture.project} {
		result, err := reconciler.Reconcile(ctx, project)
		merged.Merge(result)
		joinedErr = errors.Join(joinedErr, err)
	}
	if joinedErr == nil {
		t.Fatal("reconcile returned nil error, want poisoned-project error")
	}
	if merged.ProjectsScanned != 1 {
		t.Fatalf("projects scanned = %d, want 1 (the healthy project)", merged.ProjectsScanned)
	}
	if merged.ChangesCreated != 1 {
		t.Fatalf("changes created = %d, want 1 from the healthy project", merged.ChangesCreated)
	}
	foundPoison := false
	for _, id := range merged.SkippedProjects {
		if id == "p-0" {
			foundPoison = true
		}
	}
	if !foundPoison {
		t.Fatalf("skipped projects = %v, want poisoned project p-0", merged.SkippedProjects)
	}
	if !strings.Contains(joinedErr.Error(), "p-0") {
		t.Fatalf("error = %v, want it to reference poisoned project p-0", joinedErr)
	}

	var changeCount int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*)
FROM changes
WHERE issue_id = ? AND branch = ?`, issue.ID, branch).Scan(&changeCount); err != nil {
		t.Fatalf("count reconciled change: %v", err)
	}
	if changeCount != 1 {
		t.Fatalf("reconciled change count = %d, want 1 despite poisoned project", changeCount)
	}
}

func TestReconcileSkipsNonLocalExchangeProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	// A project whose exchange is not a local path (no ExchangePath) is skipped
	// rather than scanned.
	nonLocal := Project{
		ID:           "p-remote",
		Name:         "remote",
		RepoPath:     "/tmp/remote-flow-project",
		BaseBranch:   "main",
		ExchangeName: "flow",
		ExchangeURL:  "ssh://example.com/flow.git",
	}

	result, err := NewReconcileService(store.DB()).Reconcile(ctx, nonLocal)
	if err != nil {
		t.Fatalf("reconcile non-local project: %v", err)
	}
	if result.ProjectsScanned != 0 || result.ProjectsSkipped != 1 || len(result.SkippedProjects) != 1 || result.SkippedProjects[0] != "p-remote" {
		t.Fatalf("reconcile result = %+v, want skipped non-local project", result)
	}
}

// projectFixture is a per-test project: a git repo, a bare exchange seeded
// with the base branch, the resulting coordinator Project value, and an open
// per-project database.
type projectFixture struct {
	repoPath string
	project  Project
	store    *flowdb.Store
}

const testProjectID = "p-test"

// newProjectFixture creates a repo + seeded exchange and opens the per-project
// database, mirroring the production onboarding flow (CreateServerProject +
// SeedExchangeFromWorktree). The per-project DB lives at the server project's
// DatabasePath; projects/tokens/workers live in the global DB and are opened
// separately by tests that need them.
func newProjectFixture(t *testing.T) projectFixture {
	t.Helper()
	ctx := context.Background()

	repoPath := createReconcileGitRepo(t)
	dataDir := filepath.Join(t.TempDir(), "flow-data")
	server, err := flowgit.CreateServerProject(ctx, flowgit.ServerProjectOptions{
		DataDir:     dataDir,
		ProjectID:   testProjectID,
		BaseBranch:  "main",
		HookCommand: inertReconcileHookCommand(),
	})
	if err != nil {
		t.Fatalf("create server project: %v", err)
	}
	if _, err := flowgit.SeedExchangeFromWorktree(ctx, flowgit.SeedOptions{
		RepoPath:     repoPath,
		BaseBranch:   "main",
		ExchangeName: flowgit.DefaultExchangeName,
		ExchangeURL:  server.ExchangeURL,
	}); err != nil {
		t.Fatalf("seed exchange: %v", err)
	}

	store, err := flowdb.Open(ctx, server.DatabasePath)
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return projectFixture{
		repoPath: repoPath,
		store:    store,
		project: Project{
			ID:           testProjectID,
			Name:         "test",
			RepoPath:     repoPath,
			BaseBranch:   "main",
			ExchangeName: flowgit.DefaultExchangeName,
			ExchangeURL:  server.ExchangeURL,
			ExchangePath: server.ExchangePath,
		},
	}
}

func createReconcileGitRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := runReconcileGit("", nil, "-c", "init.defaultBranch=main", "init", repoPath); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "config", "user.name", "Flow Test"); err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "config", "user.email", "flow-test@example.com"); err != nil {
		t.Fatalf("config user.email: %v", err)
	}
	writeReconcileFile(t, repoPath, "README.md", "initial\n")
	if err := runReconcileGit(repoPath, nil, "add", "README.md"); err != nil {
		t.Fatalf("git add README: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "initial commit"); err != nil {
		t.Fatalf("git initial commit: %v", err)
	}
	return repoPath
}

func writeReconcileFile(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()

	absolutePath := filepath.Join(repoPath, relativePath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatalf("create parent directory: %v", err)
	}
	if err := os.WriteFile(absolutePath, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
}

func runReconcileGit(dir string, env []string, args ...string) error {
	_, err := reconcileGitOutput(dir, env, args...)
	return err
}

func reconcileGitOutput(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", errWithOutput(err, output)
	}

	return strings.TrimSpace(string(output)), nil
}

func errWithOutput(err error, output []byte) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err
	}

	return &gitTestError{err: err, output: message}
}

type gitTestError struct {
	err    error
	output string
}

func (e *gitTestError) Error() string {
	return e.output + ": " + e.err.Error()
}

func (e *gitTestError) Unwrap() error {
	return e.err
}

func inertReconcileHookCommand() flowgit.HookCommand {
	return flowgit.HookCommand{
		Path: "/bin/sh",
		Args: []string{"-c", "cat >/dev/null"},
	}
}
