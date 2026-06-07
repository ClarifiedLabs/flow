package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type DeleteBranchInput struct {
	ExchangeRepoPath string
	Branch           string
}

// DeleteBranch removes a branch ref directly from the bare exchange via
// update-ref, bypassing the push hooks: the coordinator both owns the policy
// the hooks enforce (issue-branch deletes are reserved for the coordinator
// principal) and rewrites the projections a push would refresh, so re-entering
// the hook pipeline from inside the coordinator would only invite recursion.
// A missing ref counts as already deleted.
func DeleteBranch(ctx context.Context, input DeleteBranchInput) error {
	path := strings.TrimSpace(input.ExchangeRepoPath)
	branch := strings.TrimSpace(input.Branch)
	if path == "" {
		return errors.New("exchange repo path is required")
	}
	if branch == "" {
		return errors.New("branch is required")
	}

	ref := "refs/heads/" + branch
	exitCode, err := gitExitCode(ctx, "", path, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return fmt.Errorf("check branch %s: %w", branch, err)
	}
	if exitCode != 0 {
		return nil
	}
	if err := gitBareRun(ctx, path, nil, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}

	return nil
}
