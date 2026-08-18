package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// PushBranch publishes the worktree's current HEAD to the task branch on the
// exchange remote (origin). It runs as the worker principal unless the caller's
// environment already names one, mirroring how the merge worktree stamps
// FLOW_GIT_PRINCIPAL on its base-branch push (see merge.go).
//
// The push targets refs/heads/<branch> from HEAD, so re-running it after the
// branch is already published is a no-op success ("Everything up-to-date"). That
// idempotency is what lets `flow complete` own the push and be safe to re-run.
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

// PushRef publishes src (any commit-ish already present in the exchange) to
// ref on the exchange remote at exchangePath, as the coordinator principal.
// Feature refs are coordinator-only (see hooks.go), so every coordinator
// write to them — creation, rebase finalize, land — flows through here.
func PushRef(ctx context.Context, exchangePath, src, ref string) error {
	exchangePath = strings.TrimSpace(exchangePath)
	src = strings.TrimSpace(src)
	ref = strings.TrimSpace(ref)
	if exchangePath == "" {
		return errors.New("exchange repo path is required")
	}
	if src == "" || ref == "" {
		return errors.New("push source and ref are required")
	}
	if err := gitRun(ctx, exchangePath, []string{"FLOW_GIT_PRINCIPAL=" + coordinatorActor}, "push", exchangePath, src+":"+ref); err != nil {
		return fmt.Errorf("push %s to %s: %w", src, ref, err)
	}

	return nil
}

// PushBranchCompareAndSwap force-updates ref on the exchange remote only when
// it still points at expectedSHA, pushing the worktree's current HEAD (the
// rebased objects live only in the worktree, so the push must originate there).
// It runs as the coordinator principal, mirroring PushRefCompareAndSwap.
func PushBranchCompareAndSwap(ctx context.Context, worktree, ref, expectedSHA string) error {
	worktree = strings.TrimSpace(worktree)
	ref = strings.TrimSpace(ref)
	expectedSHA = strings.TrimSpace(expectedSHA)
	if worktree == "" {
		return errors.New("worktree is required")
	}
	if ref == "" || expectedSHA == "" {
		return errors.New("ref and expected sha are required")
	}
	if err := gitRun(ctx, worktree, []string{"FLOW_GIT_PRINCIPAL=" + coordinatorActor},
		"push", "--force-with-lease="+ref+":"+expectedSHA, "origin", "HEAD:"+ref); err != nil {
		return fmt.Errorf("compare-and-swap push HEAD to %s: %w", ref, err)
	}
	return nil
}

// PushRefCompareAndSwap force-updates ref on the exchange remote only when it
// still points at expectedSHA. This is the guard for the coordinator's
// rewrites of shared feature refs (rebase finalize): a concurrent update to
// the feature branch fails the lease and git rejects the push.
func PushRefCompareAndSwap(ctx context.Context, exchangePath, ref, newSHA, expectedSHA string) error {
	exchangePath = strings.TrimSpace(exchangePath)
	ref = strings.TrimSpace(ref)
	newSHA = strings.TrimSpace(newSHA)
	expectedSHA = strings.TrimSpace(expectedSHA)
	if exchangePath == "" {
		return errors.New("exchange repo path is required")
	}
	if ref == "" || newSHA == "" || expectedSHA == "" {
		return errors.New("ref, new sha, and expected sha are required")
	}
	if err := gitRun(ctx, exchangePath, []string{"FLOW_GIT_PRINCIPAL=" + coordinatorActor},
		"push", "--force-with-lease="+ref+":"+expectedSHA, exchangePath, newSHA+":"+ref); err != nil {
		return fmt.Errorf("compare-and-swap push %s to %s: %w", newSHA, ref, err)
	}

	return nil
}
