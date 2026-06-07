package git

import (
	"context"
	"testing"
)

func TestPushBranchPublishesHeadAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath, project := initializedRepoWithIssueBranch(t)
	if err := gitRun(ctx, repoPath, nil, "remote", "add", "origin", project.ExchangeURL); err != nil {
		t.Fatalf("add origin remote: %v", err)
	}
	if err := gitRun(ctx, repoPath, nil, "checkout", "-b", "issue/i-0001"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	headSHA := writeAndCommit(t, repoPath, "feature.txt", "work\n", "feat: add feature")

	if err := PushBranch(ctx, repoPath, "issue/i-0001"); err != nil {
		t.Fatalf("push branch: %v", err)
	}

	content, present, err := ReadTextFileAtRef(ctx, project.ExchangePath, "refs/heads/issue/i-0001", "feature.txt")
	if err != nil {
		t.Fatalf("read pushed file: %v", err)
	}
	if !present || content != "work\n" {
		t.Fatalf("pushed file present=%t content=%q, want published work", present, content)
	}
	exchangeHead, err := gitBareOutput(ctx, project.ExchangePath, nil, "rev-parse", "refs/heads/issue/i-0001")
	if err != nil {
		t.Fatalf("read exchange head: %v", err)
	}
	if exchangeHead != headSHA {
		t.Fatalf("exchange head = %s, want readied HEAD %s", exchangeHead, headSHA)
	}

	// A re-run after the branch is already published must be a no-op success:
	// `flow ready` re-runs the push, so it cannot fail when nothing changed.
	if err := PushBranch(ctx, repoPath, "issue/i-0001"); err != nil {
		t.Fatalf("idempotent push branch: %v", err)
	}
	exchangeHeadAgain, err := gitBareOutput(ctx, project.ExchangePath, nil, "rev-parse", "refs/heads/issue/i-0001")
	if err != nil {
		t.Fatalf("read exchange head after re-run: %v", err)
	}
	if exchangeHeadAgain != headSHA {
		t.Fatalf("exchange head after re-run = %s, want unchanged %s", exchangeHeadAgain, headSHA)
	}
}

func TestPushBranchRequiresBranch(t *testing.T) {
	t.Parallel()
	if err := PushBranch(context.Background(), t.TempDir(), "  "); err == nil {
		t.Fatal("PushBranch with blank branch was accepted")
	}
}
