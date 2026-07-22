package git

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPreReceiveProtectsBaseBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath, result := initializedRepoWithTaskBranch(t)
	baseSHA, err := gitOutput(ctx, repoPath, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read base sha: %v", err)
	}
	nextSHA := writeAndCommit(t, repoPath, "next.txt", "next\n", "next commit")
	if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=coordinator"}, "push", result.ExchangePath, nextSHA+":refs/heads/tmp-next"); err != nil {
		t.Fatalf("push next object to exchange: %v", err)
	}

	// Creating the base ref (the initial seed, old sha all-zero) is the one
	// base write the client may perform: flow init pushes the seed as the
	// owner principal.
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(zeroSHA, baseSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal("worker:w-1"),
	}); err == nil {
		t.Fatal("worker base branch creation was accepted")
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(zeroSHA, baseSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal("owner"),
	}); err != nil {
		t.Fatalf("owner base branch creation rejected: %v", err)
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(zeroSHA, baseSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal(""),
	}); err != nil {
		t.Fatalf("missing-principal base branch creation rejected: %v", err)
	}

	// Updates to an existing base ref require an authenticated owner or
	// coordinator principal.
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(baseSHA, nextSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal("owner"),
	}); err != nil {
		t.Fatalf("owner base branch update rejected: %v", err)
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(baseSHA, nextSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal("worker:w-1"),
	}); err == nil {
		t.Fatal("worker base branch update was accepted")
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(baseSHA, nextSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal(""),
	}); err == nil {
		t.Fatal("missing-principal base branch update was accepted")
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(baseSHA, nextSHA, "refs/heads/main")),
		Principal:        hookTestPrincipal("coordinator"),
	}); err != nil {
		t.Fatalf("coordinator base branch update rejected: %v", err)
	}
}

func TestPreReceiveRejectsForbiddenBasePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath, result := initializedRepoWithTaskBranch(t)

	if err := gitRun(ctx, repoPath, nil, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	sessionSHA := writeAndCommit(t, repoPath, ".flow/session/state.json", "{}\n", "add session state")

	for _, principal := range []string{"coordinator", "owner"} {
		for _, sha := range []string{sessionSHA} {
			if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=coordinator"}, "push", "--force", result.ExchangePath, sha+":refs/heads/tmp-forbidden"); err != nil {
				t.Fatalf("push forbidden object to exchange: %v", err)
			}
			if err := HandlePreReceive(ctx, HookOptions{
				ExchangeRepoPath: result.ExchangePath,
				BaseBranch:       "main",
				Stdin:            strings.NewReader(refLine(zeroSHA, sha, "refs/heads/main")),
				Principal:        hookTestPrincipal(principal),
			}); err == nil {
				t.Fatalf("base branch creation by %s with forbidden path at %s was accepted", principal, sha)
			}
		}
	}
}

func TestPreReceiveTaskBranchPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, result, firstTaskSHA, secondTaskSHA := initializedTaskBranch(t)

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(zeroSHA, firstTaskSHA, "refs/heads/task/t-test-0002")),
		Principal:        hookTestPrincipal("owner"),
	}); err != nil {
		t.Fatalf("owner task branch create rejected: %v", err)
	}

	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(firstTaskSHA, secondTaskSHA, "refs/heads/task/t-test-0001")),
		Principal:        hookTestPrincipal(""),
	}); err != nil {
		t.Fatalf("local fast-forward task update rejected: %v", err)
	}
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(secondTaskSHA, firstTaskSHA, "refs/heads/task/t-test-0001")),
		Principal:        hookTestPrincipal(""),
	}); err == nil {
		t.Fatal("local non-fast-forward task update was accepted")
	}
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(firstTaskSHA, secondTaskSHA, "refs/heads/topic")),
		Principal:        hookTestPrincipal(""),
	}); err == nil {
		t.Fatal("unknown ref namespace was accepted")
	}
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(firstTaskSHA, secondTaskSHA, "refs/heads/task/not-an-task-id")),
		Principal:        hookTestPrincipal(""),
	}); err == nil {
		t.Fatal("invalid task branch namespace was accepted")
	}
	if err := HandlePreReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Stdin:            strings.NewReader(refLine(secondTaskSHA, zeroSHA, "refs/heads/task/t-test-0001")),
		Principal:        hookTestPrincipal(""),
	}); err == nil {
		t.Fatal("local task branch deletion was accepted")
	}
}

func TestPostReceiveSpoolsEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, result, firstTaskSHA, secondTaskSHA := initializedTaskBranch(t)

	if err := HandlePostReceive(ctx, HookOptions{
		ExchangeRepoPath: result.ExchangePath,
		BaseBranch:       "main",
		Principal:        hookTestPrincipal("owner"),
		Stdin: bytes.NewBufferString(
			refLine(firstTaskSHA, secondTaskSHA, "refs/heads/task/t-test-0001") +
				refLine(zeroSHA, firstTaskSHA, "refs/tags/test-tag"),
		),
	}); err != nil {
		t.Fatalf("post-receive: %v", err)
	}

	events, err := ReadSpooledEvents(result.ExchangePath)
	if err != nil {
		t.Fatalf("read spooled events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	if events[0].OldSHA != firstTaskSHA || events[0].NewSHA != secondTaskSHA || events[0].Ref != "refs/heads/task/t-test-0001" {
		t.Fatalf("first event mismatch: %+v", events[0])
	}
	if events[0].Actor != "owner" {
		t.Fatalf("Actor = %q, want owner", events[0].Actor)
	}
}

func hookTestPrincipal(value string) *string {
	return &value
}

func initializedRepoWithTaskBranch(t *testing.T) (string, ServerProject) {
	t.Helper()

	ctx := context.Background()
	repoPath := createGitRepo(t)
	project := createServerProjectForTest(t)
	if _, err := SeedExchangeFromWorktree(ctx, SeedOptions{
		RepoPath:    repoPath,
		BaseBranch:  "main",
		ExchangeURL: project.ExchangePath,
	}); err != nil {
		t.Fatalf("seed exchange: %v", err)
	}

	return repoPath, project
}

func initializedTaskBranch(t *testing.T) (string, ServerProject, string, string) {
	t.Helper()

	ctx := context.Background()
	repoPath, project := initializedRepoWithTaskBranch(t)
	if err := gitRun(ctx, repoPath, nil, "checkout", "-b", "task/t-test-0001"); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	firstTaskSHA := writeAndCommit(t, repoPath, "task.txt", "first\n", "first task commit")
	secondTaskSHA := writeAndCommit(t, repoPath, "task.txt", "second\n", "second task commit")
	if err := gitRun(ctx, repoPath, []string{"FLOW_GIT_PRINCIPAL=owner"}, "push", project.ExchangePath, "refs/heads/task/t-test-0001:refs/heads/task/t-test-0001"); err != nil {
		t.Fatalf("push task branch to exchange: %v", err)
	}

	return repoPath, project, firstTaskSHA, secondTaskSHA
}

func refLine(oldSHA, newSHA, ref string) string {
	return oldSHA + " " + newSHA + " " + ref + "\n"
}
