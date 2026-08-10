package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
)

// TestReconcileRestoresChangeProjectionWithoutReadingHandoffRef proves the
// change projection is rebuilt from the branch tip while a committed handoff
// file on that branch is ignored: the coordinator's handoff snapshot is written
// solely by the coordinator's artifact path, never re-read from git.
func TestReconcileRestoresChangeProjectionWithoutReadingHandoffRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	store := fixture.store
	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Reconciled task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	branch := "task/" + task.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	// Commit a stray handoff file on the branch: reconcile must NOT read it.
	handoffContents := handoff.RenderTemplate(handoff.TemplateInput{
		TaskID:                task.ID,
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
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "work on task"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	headSHA, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read head sha: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("push task branch: %v", err)
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
WHERE task_id = ? AND branch = ?`, task.ID, branch).Scan(&changeID, &head); err != nil {
		t.Fatalf("load reconciled change: %v", err)
	}
	if changeID != "ch-test-0001" {
		t.Fatalf("reconciled change ID = %q, want ch-test-0001", changeID)
	}
	if head != headSHA {
		t.Fatalf("change head = %s, want %s", head, headSHA)
	}
	// The committed handoff file is never projected into a snapshot.
	if _, err := reconciler.GetHandoffSnapshot(ctx, changeID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get handoff snapshot err = %v, want sql.ErrNoRows (reconcile must ignore committed handoff)", err)
	}
}

// TestReconcileConcurrentCreationUsesExistingLogicalChange exercises parallel
// projections of the same task branch.
func TestReconcileConcurrentCreationUsesExistingLogicalChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	task, err := NewTaskService(fixture.store.DB(), testProjectID).CreateTask(ctx, CreateTaskInput{Title: "Concurrent reconciliation"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	branch := "task/" + task.ID
	if err := runReconcileGit(fixture.repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	writeReconcileFile(t, fixture.repoPath, "concurrent.txt", "work\n")
	if err := runReconcileGit(fixture.repoPath, nil, "add", "concurrent.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "concurrent work"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("push task branch: %v", err)
	}

	const passes = 12
	type reconcileOutput struct {
		result ReconcileResult
		err    error
	}
	start := make(chan struct{})
	outputs := make(chan reconcileOutput, passes)
	var wg sync.WaitGroup
	for i := 0; i < passes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := NewReconcileService(fixture.store.DB()).Reconcile(ctx, fixture.project)
			outputs <- reconcileOutput{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outputs)

	created := 0
	for output := range outputs {
		if output.err != nil {
			t.Errorf("concurrent reconcile: %v", output.err)
		}
		if output.result.BranchesScanned != 1 {
			t.Errorf("branches scanned = %d, want 1", output.result.BranchesScanned)
		}
		created += output.result.ChangesCreated
	}
	if created != 1 {
		t.Fatalf("changes reported created = %d, want 1", created)
	}

	var changeID string
	var changeCount int
	if err := fixture.store.DB().QueryRowContext(ctx, `
SELECT MIN(id), COUNT(*)
FROM changes
WHERE task_id = ? AND branch = ?`, task.ID, branch).Scan(&changeID, &changeCount); err != nil {
		t.Fatalf("load reconciled change: %v", err)
	}
	if changeCount != 1 || changeID != "ch-test-0001" {
		t.Fatalf("reconciled changes = count %d id %q, want one ch-test-0001", changeCount, changeID)
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
		ExchangePath: poisonPath,
	}

	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Reconciled task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	branch := "task/" + task.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	writeReconcileFile(t, repoPath, "feature.txt", "work\n")
	if err := runReconcileGit(repoPath, nil, "add", "feature.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "work on task"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("push task branch: %v", err)
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
WHERE task_id = ? AND branch = ?`, task.ID, branch).Scan(&changeCount); err != nil {
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

// newProjectFixture clones the per-binary seeded project template with file
// copies and opens the per-project database. Building the repo + exchange
// from scratch costs ~20 git subprocesses per test, which dominated fixture
// setup time; the template itself mirrors the production onboarding flow
// (CreateServerProject + SeedExchangeFromWorktree) exactly once per binary.
// The per-project DB lives at the server project's DatabasePath;
// projects/tokens/workers live in the global DB and are opened separately by
// tests that need them.
func newProjectFixture(t *testing.T) projectFixture {
	t.Helper()
	ctx := context.Background()

	seededProjectTemplate.once.Do(func() {
		seededProjectTemplate.repoPath, seededProjectTemplate.projectDir, seededProjectTemplate.err = buildSeededProjectTemplate()
	})
	if seededProjectTemplate.err != nil {
		t.Fatalf("build seeded project template: %v", seededProjectTemplate.err)
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "flow-data")
	repoPath := filepath.Join(root, "repo")
	projectDir := flowgit.ProjectDir(dataDir, testProjectID)
	copyDirTree(t, seededProjectTemplate.repoPath, repoPath)
	copyDirTree(t, seededProjectTemplate.projectDir, projectDir)
	// The seeded worktree remote points at the template exchange; retarget it
	// at this test's copy so pushes stay fixture-local.
	exchangePath := filepath.Join(projectDir, "exchange.git")
	retargetFixtureRemote(t, repoPath, filepath.Join(seededProjectTemplate.projectDir, "exchange.git"), exchangePath)

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(dataDir, testProjectID))
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
			ExchangePath: exchangePath,
		},
	}
}

// seededProjectTemplate holds the per-binary fixture project newProjectFixture
// clones. The inert hook command ignores its --repo argument, so the copied
// hook scripts are valid for every clone even though they embed the template
// path.
var seededProjectTemplate struct {
	once       sync.Once
	repoPath   string
	projectDir string
	err        error
}

func buildSeededProjectTemplate() (repoPath, projectDir string, err error) {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "flow-project-template-")
	if err != nil {
		return "", "", err
	}
	repoPath = filepath.Join(root, "repo")
	if err := initReconcileGitRepo(repoPath); err != nil {
		return "", "", err
	}
	dataDir := filepath.Join(root, "flow-data")
	server, err := flowgit.CreateServerProject(ctx, flowgit.ServerProjectOptions{
		DataDir:     dataDir,
		ProjectID:   testProjectID,
		BaseBranch:  "main",
		HookCommand: inertReconcileHookCommand(),
	})
	if err != nil {
		return "", "", fmt.Errorf("create server project: %w", err)
	}
	if _, err := flowgit.SeedExchangeFromWorktree(ctx, flowgit.SeedOptions{
		RepoPath:     repoPath,
		BaseBranch:   "main",
		ExchangeName: flowgit.DefaultExchangeName,
		ExchangeURL:  server.ExchangePath,
	}); err != nil {
		return "", "", fmt.Errorf("seed exchange: %w", err)
	}
	return repoPath, flowgit.ProjectDir(dataDir, testProjectID), nil
}

// copyDirTree recursively copies a fixture tree, preserving file modes. The
// trees are small (a handful of git objects), so copying beats rebuilding
// them with git subprocesses for every test.
func copyDirTree(t *testing.T, source, target string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(destination, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case entry.Type().IsRegular():
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, contents, info.Mode().Perm())
		default:
			return nil
		}
	}); err != nil {
		t.Fatalf("copy fixture tree %s: %v", source, err)
	}
}

// retargetFixtureRemote rewrites the worktree remote URL that still points at
// the template exchange after the tree copy.
func retargetFixtureRemote(t *testing.T, repoPath, oldURL, newURL string) {
	t.Helper()
	configPath := filepath.Join(repoPath, ".git", "config")
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read fixture git config: %v", err)
	}
	if !strings.Contains(string(contents), oldURL) {
		t.Fatalf("fixture git config does not reference template exchange %q", oldURL)
	}
	updated := strings.ReplaceAll(string(contents), oldURL, newURL)
	if err := os.WriteFile(configPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("retarget fixture git remote: %v", err)
	}
}

func createReconcileGitRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := initReconcileGitRepo(repoPath); err != nil {
		t.Fatalf("create git repo: %v", err)
	}
	return repoPath
}

func initReconcileGitRepo(repoPath string) error {
	if _, err := reconcileGitOutput("", nil, "-c", "init.defaultBranch=main", "init", repoPath); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if _, err := reconcileGitOutput(repoPath, nil, "config", "user.name", "Flow Test"); err != nil {
		return fmt.Errorf("config user.name: %w", err)
	}
	if _, err := reconcileGitOutput(repoPath, nil, "config", "user.email", "flow-test@example.com"); err != nil {
		return fmt.Errorf("config user.email: %w", err)
	}
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("initial\n"), 0o644); err != nil {
		return fmt.Errorf("write README.md: %w", err)
	}
	if _, err := reconcileGitOutput(repoPath, nil, "add", "README.md"); err != nil {
		return fmt.Errorf("git add README: %w", err)
	}
	if _, err := reconcileGitOutput(repoPath, nil, "commit", "-m", "initial commit"); err != nil {
		return fmt.Errorf("git initial commit: %w", err)
	}
	return nil
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
