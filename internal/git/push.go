package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// PushBranch publishes the worktree's current HEAD to the issue branch on the
// exchange remote (origin). It runs as the worker principal unless the caller's
// environment already names one, mirroring how the merge worktree stamps
// FLOW_GIT_PRINCIPAL on its base-branch push (see merge.go).
//
// The push targets refs/heads/<branch> from HEAD, so re-running it after the
// branch is already published is a no-op success ("Everything up-to-date"). That
// idempotency is what lets `flow ready` own the push and be safe to re-run.
func PushBranch(ctx context.Context, worktree string, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return errors.New("branch is required")
	}

	var env []string
	if strings.TrimSpace(os.Getenv("FLOW_GIT_PRINCIPAL")) == "" {
		env = append(env, "FLOW_GIT_PRINCIPAL=worker")
	}
	if err := gitRun(ctx, worktree, env, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push branch %s to origin: %w", branch, err)
	}

	return nil
}
