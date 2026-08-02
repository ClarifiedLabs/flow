package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSquashMergeToBaseUsesConfiguredCommitIdentity(t *testing.T) {
	_, project, _, _ := initializedTaskBranch(t)

	result, err := SquashMergeToBase(context.Background(), SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
		CommitName:       "Flow Bot",
		CommitEmail:      "flow-bot@example.com",
	})
	if err != nil {
		t.Fatalf("squash merge: %v", err)
	}

	assertMergeCommitAuthor(t, project.ExchangePath, result.MergeSHA, "Flow Bot", "flow-bot@example.com")
}

func TestSquashMergeToBaseRejectsIgnoredAddedPaths(t *testing.T) {
	ctx := context.Background()
	repoPath, project, _, _ := initializedTaskBranch(t)
	if err := gitRun(ctx, repoPath, nil, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	writeAndCommit(t, repoPath, ".gitignore", "/SUMMARY.md\n", "ignore workflow summary")
	if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=owner"}, "push", project.ExchangePath, "main:main"); err != nil {
		t.Fatalf("push base ignore rule: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "checkout", "task/t-test-0001"); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	writeAndCommit(t, repoPath, "SUMMARY.md", "temporary workflow summary\n", "add workflow summary")
	if err := gitRun(ctx, repoPath, nil, "push", project.ExchangePath, "task/t-test-0001:task/t-test-0001"); err != nil {
		t.Fatalf("push task summary: %v", err)
	}

	_, err := SquashMergeToBase(ctx, SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if !errors.Is(err, ErrMergeContainsIgnoredPaths) {
		t.Fatalf("squash merge error = %v, want %v", err, ErrMergeContainsIgnoredPaths)
	}
	baseSHA, err := gitBareOutput(ctx, project.ExchangePath, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read base sha: %v", err)
	}
	if treePathExists(ctx, project.ExchangePath, baseSHA, "SUMMARY.md") {
		t.Fatal("rejected merge added ignored workflow summary to base")
	}
}

func TestSquashMergeToBasePreservesTrackedIgnoredChanges(t *testing.T) {
	ctx := context.Background()
	repoPath := createGitRepo(t)
	writeAndCommit(t, repoPath, "tracked.log", "base\n", "add tracked log")
	writeAndCommit(t, repoPath, ".gitignore", "*.log\n", "ignore logs")
	project := createServerProjectForTest(t)
	if _, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangePath,
	}); err != nil {
		t.Fatalf("seed exchange: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "checkout", "-b", "task/t-test-0001"); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	writeAndCommit(t, repoPath, "tracked.log", "changed\n", "change tracked log")
	if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=owner"}, "push", project.ExchangePath, "task/t-test-0001:task/t-test-0001"); err != nil {
		t.Fatalf("push task branch: %v", err)
	}

	result, err := SquashMergeToBase(ctx, SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if err != nil {
		t.Fatalf("squash merge: %v", err)
	}
	contents, err := gitBareOutput(ctx, project.ExchangePath, nil, "show", result.MergeSHA+":tracked.log")
	if err != nil {
		t.Fatalf("read merged tracked log: %v", err)
	}
	if contents != "changed" {
		t.Fatalf("merged tracked log = %q, want changed", contents)
	}
}

func TestSquashMergeToBaseUsesOnlyCommittedIgnoreRules(t *testing.T) {
	ctx := context.Background()
	repoPath, project, _, _ := initializedTaskBranch(t)
	writeAndCommit(t, repoPath, "fixture.tmp", "legitimate fixture\n", "add fixture")
	if err := gitRun(ctx, repoPath, nil, "push", project.ExchangePath, "task/t-test-0001:task/t-test-0001"); err != nil {
		t.Fatalf("push task branch: %v", err)
	}

	excludesPath := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(excludesPath, []byte("*.tmp\n"), 0o600); err != nil {
		t.Fatalf("write global excludes: %v", err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := gitRun(ctx, "", nil, "config", "--file", globalConfig, "core.excludesFile", excludesPath); err != nil {
		t.Fatalf("configure global excludes: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	result, err := SquashMergeToBase(ctx, SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if err != nil {
		t.Fatalf("squash merge: %v", err)
	}
	if !treePathExists(ctx, project.ExchangePath, result.MergeSHA, "fixture.tmp") {
		t.Fatal("coordinator-global ignore rule dropped legitimate fixture")
	}
}

func TestSquashMergeToBaseRejectsRenameIntoIgnoredPath(t *testing.T) {
	ctx := context.Background()
	repoPath := createGitRepo(t)
	writeAndCommit(t, repoPath, "app.txt", "app\n", "add app")
	writeAndCommit(t, repoPath, ".gitignore", "*.log\n", "ignore logs")
	project := createServerProjectForTest(t)
	if _, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangePath,
	}); err != nil {
		t.Fatalf("seed exchange: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "checkout", "-b", "task/t-test-0001"); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "mv", "app.txt", "app.log"); err != nil {
		t.Fatalf("rename app: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "commit", "-m", "rename app"); err != nil {
		t.Fatalf("commit rename: %v", err)
	}
	if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=owner"}, "push", project.ExchangePath, "task/t-test-0001:task/t-test-0001"); err != nil {
		t.Fatalf("push task branch: %v", err)
	}

	_, err := SquashMergeToBase(ctx, SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if !errors.Is(err, ErrMergeContainsIgnoredPaths) {
		t.Fatalf("squash merge error = %v, want %v", err, ErrMergeContainsIgnoredPaths)
	}
}

func TestSquashMergeToBaseRestoresExcludedLiteralPaths(t *testing.T) {
	ctx := context.Background()
	repoPath, project, _, _ := initializedTaskBranch(t)
	writeAndCommit(t, repoPath, ".flow/session/*.json", "{}\n", "add session state")
	if err := gitRun(ctx, repoPath, nil, "push", project.ExchangePath, "task/t-test-0001:task/t-test-0001"); err != nil {
		t.Fatalf("push task branch: %v", err)
	}

	result, err := SquashMergeToBase(ctx, SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if err != nil {
		t.Fatalf("squash merge: %v", err)
	}
	if treePathExists(ctx, project.ExchangePath, result.MergeSHA, ".flow/session") {
		t.Fatal("merge commit contains excluded session state")
	}
	if !treePathExists(ctx, project.ExchangePath, result.MergeSHA, "task.txt") {
		t.Fatal("merge commit dropped task source change")
	}
}

func TestSquashMergeToBaseDefaultsCommitIdentity(t *testing.T) {
	_, project, _, _ := initializedTaskBranch(t)

	result, err := SquashMergeToBase(context.Background(), SquashMergeInput{
		ExchangeRepoPath: project.ExchangePath,
		BaseBranch:       "main",
		Branch:           "task/t-test-0001",
		Message:          "merge task",
	})
	if err != nil {
		t.Fatalf("squash merge: %v", err)
	}

	assertMergeCommitAuthor(t, project.ExchangePath, result.MergeSHA, DefaultMergeCommitName, DefaultMergeCommitEmail)
}

func assertMergeCommitAuthor(t *testing.T, exchangePath string, sha string, wantName string, wantEmail string) {
	t.Helper()

	ctx := context.Background()
	name, err := gitBareOutput(ctx, exchangePath, nil, "show", "-s", "--format=%an", sha)
	if err != nil {
		t.Fatalf("read merge commit author name: %v", err)
	}
	if name != wantName {
		t.Fatalf("merge commit author name = %q, want %q", name, wantName)
	}
	email, err := gitBareOutput(ctx, exchangePath, nil, "show", "-s", "--format=%ae", sha)
	if err != nil {
		t.Fatalf("read merge commit author email: %v", err)
	}
	if email != wantEmail {
		t.Fatalf("merge commit author email = %q, want %q", email, wantEmail)
	}
}
