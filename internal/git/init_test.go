package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateServerProjectCreatesExchangeWithHooksAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "flow-data")

	created, err := CreateServerProject(ctx, ServerProjectOptions{
		DataDir:     dataDir,
		ProjectID:   "p-test",
		BaseBranch:  "main",
		HookCommand: inertHookCommand(),
	})
	if err != nil {
		t.Fatalf("create server project: %v", err)
	}
	if created.Dir != ProjectDir(dataDir, "p-test") {
		t.Fatalf("Dir = %q, want %q", created.Dir, ProjectDir(dataDir, "p-test"))
	}
	if created.DatabasePath != ProjectDatabasePath(dataDir, "p-test") {
		t.Fatalf("DatabasePath = %q", created.DatabasePath)
	}
	if !strings.HasPrefix(created.ExchangeURL, "file://") {
		t.Fatalf("ExchangeURL = %q, want file:// url", created.ExchangeURL)
	}

	isBare, err := gitBareOutput(ctx, created.ExchangePath, nil, "rev-parse", "--is-bare-repository")
	if err != nil || isBare != "true" {
		t.Fatalf("exchange is not a bare repository: %q %v", isBare, err)
	}
	for _, hook := range []string{"pre-receive", "post-receive"} {
		if _, err := os.Stat(filepath.Join(created.ExchangePath, "hooks", hook)); err != nil {
			t.Fatalf("missing %s hook: %v", hook, err)
		}
	}

	again, err := CreateServerProject(ctx, ServerProjectOptions{
		DataDir:     dataDir,
		ProjectID:   "p-test",
		BaseBranch:  "main",
		HookCommand: inertHookCommand(),
	})
	if err != nil {
		t.Fatalf("re-create server project: %v", err)
	}
	if again.ExchangePath != created.ExchangePath {
		t.Fatalf("re-create exchange path = %q, want %q", again.ExchangePath, created.ExchangePath)
	}
}

func TestCreateServerProjectRejectsInvalidBaseBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if _, err := CreateServerProject(ctx, ServerProjectOptions{
		DataDir:     filepath.Join(t.TempDir(), "flow-data"),
		ProjectID:   "p-test",
		BaseBranch:  "-bad",
		HookCommand: inertHookCommand(),
	}); err == nil {
		t.Fatal("invalid base branch was accepted")
	}
}

func TestSeedExchangeFromWorktreeSeedsBaseAndPreservesOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath := createGitRepo(t)
	originURL := "https://example.com/upstream.git"
	if err := gitRun(ctx, repoPath, nil, "remote", "add", "origin", originURL); err != nil {
		t.Fatalf("add origin: %v", err)
	}

	project := createServerProjectForTest(t)

	result, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangeURL,
	})
	if err != nil {
		t.Fatalf("seed exchange: %v", err)
	}
	if !result.Seeded {
		t.Fatal("Seeded = false, want true")
	}
	if result.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want main", result.BaseBranch)
	}
	if result.Warning != "" {
		t.Fatalf("Warning = %q, want empty", result.Warning)
	}

	currentOrigin, err := gitOutput(ctx, repoPath, nil, "remote", "get-url", "origin")
	if err != nil || currentOrigin != originURL {
		t.Fatalf("origin URL = %q (%v), want %q", currentOrigin, err, originURL)
	}
	flowRemote, err := gitOutput(ctx, repoPath, nil, "remote", "get-url", DefaultExchangeName)
	if err != nil || flowRemote != project.ExchangeURL {
		t.Fatalf("flow remote = %q (%v), want %q", flowRemote, err, project.ExchangeURL)
	}

	localHead, err := gitOutput(ctx, repoPath, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read local main: %v", err)
	}
	exchangeHead, err := gitBareOutput(ctx, project.ExchangePath, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read exchange main: %v", err)
	}
	if exchangeHead != localHead {
		t.Fatalf("exchange main = %q, want %q", exchangeHead, localHead)
	}

	rerun, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangeURL,
	})
	if err != nil {
		t.Fatalf("re-seed exchange: %v", err)
	}
	if rerun.Seeded {
		t.Fatal("re-run Seeded = true, want false (base already present)")
	}
}

func TestSeedExchangeFromWorktreeWarnsOnDirtyWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath := createGitRepo(t)
	project := createServerProjectForTest(t)

	if err := os.WriteFile(filepath.Join(repoPath, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	result, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangeURL,
	})
	if err != nil {
		t.Fatalf("seed with dirty worktree: %v", err)
	}
	if result.Warning == "" {
		t.Fatal("dirty worktree should produce a warning")
	}
	if !result.Seeded {
		t.Fatal("dirty worktree seed should still push committed HEAD")
	}

	exchangeHead, err := gitBareOutput(ctx, project.ExchangePath, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read exchange main: %v", err)
	}
	localHead, err := gitOutput(ctx, repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read local HEAD: %v", err)
	}
	if exchangeHead != localHead {
		t.Fatalf("exchange seeded %s, want committed HEAD %s", exchangeHead, localHead)
	}
}

func TestSeedExchangeFromWorktreeFailsWhenFlowRemoteDiffers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath := createGitRepo(t)
	project := createServerProjectForTest(t)

	if err := gitRun(ctx, repoPath, nil, "remote", "add", DefaultExchangeName, "https://example.com/elsewhere.git"); err != nil {
		t.Fatalf("add conflicting flow remote: %v", err)
	}

	if _, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangeURL,
	}); err == nil {
		t.Fatal("conflicting flow remote was accepted")
	}

	// An alternate exchange name sidesteps the conflict.
	if _, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:     repoPath,
		BaseBranch:   "main",
		ExchangeName: "flow-local",
		ExchangeURL:  project.ExchangeURL,
	}); err != nil {
		t.Fatalf("seed with alternate exchange name: %v", err)
	}
}

func createServerProjectForTest(t *testing.T) ServerProject {
	t.Helper()

	project, err := CreateServerProject(context.Background(), ServerProjectOptions{
		DataDir:     filepath.Join(t.TempDir(), "flow-data"),
		ProjectID:   "p-test",
		BaseBranch:  "main",
		HookCommand: inertHookCommand(),
	})
	if err != nil {
		t.Fatalf("create server project: %v", err)
	}

	return project
}

func createGitRepo(t *testing.T) string {
	t.Helper()

	repoPath := createGitRepoWithoutFlowInit(t)
	return repoPath
}

func createGitRepoWithoutFlowInit(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := gitRun(ctx, "", nil, "-c", "init.defaultBranch=main", "init", repoPath); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "config", "user.name", "Flow Test"); err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "config", "user.email", "flow-test@example.com"); err != nil {
		t.Fatalf("config user.email: %v", err)
	}

	writeAndCommit(t, repoPath, "README.md", "initial\n", "initial commit")
	return repoPath
}

func writeAndCommit(t *testing.T, repoPath string, relativePath string, contents string, message string) string {
	t.Helper()

	ctx := context.Background()
	fullPath := filepath.Join(repoPath, relativePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("create parent dir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
	if err := gitRun(ctx, repoPath, nil, "add", relativePath); err != nil {
		t.Fatalf("git add %s: %v", relativePath, err)
	}
	if err := gitRun(ctx, repoPath, nil, "commit", "-m", message); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	sha, err := gitOutput(ctx, repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}

func inertHookCommand() HookCommand {
	return HookCommand{
		Path: "/bin/sh",
		Args: []string{"-c", "cat >/dev/null"},
	}
}
