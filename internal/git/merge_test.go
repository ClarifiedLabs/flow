package git

import (
	"context"
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
