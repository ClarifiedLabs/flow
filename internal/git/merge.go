package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrNoMergeChanges = errors.New("squash merge has no included changes")
	ErrMergeConflict  = errors.New("squash merge conflict")
	ErrHeadMismatch   = errors.New("issue branch head does not match expected head")
)

type MergeConflictError struct {
	Output string
}

func (e *MergeConflictError) Error() string {
	output := strings.TrimSpace(e.Output)
	if output == "" {
		return ErrMergeConflict.Error()
	}

	return fmt.Sprintf("%s: %s", ErrMergeConflict, output)
}

func (e *MergeConflictError) Unwrap() error {
	return ErrMergeConflict
}

type SquashMergeInput struct {
	ExchangeRepoPath string
	BaseBranch       string
	Branch           string
	ExpectedHeadSHA  string
	Message          string
}

type SquashMergeResult struct {
	PreviousBaseSHA string
	HeadSHA         string
	MergeSHA        string
}

func SquashMergeToBase(ctx context.Context, input SquashMergeInput) (SquashMergeResult, error) {
	return squashMergeToBase(ctx, input, true)
}

// SquashMergeIsNoop reports whether squashing the issue branch onto the current
// base stages no changes — i.e. the branch's content is already contained in
// the base. Nothing is committed or pushed. Merge-recovery uses this as the
// content-equivalence proof that a crashed merge's push landed before marking
// the change merged.
func SquashMergeIsNoop(ctx context.Context, input SquashMergeInput) (bool, error) {
	_, err := squashMergeToBase(ctx, input, false)
	if errors.Is(err, ErrNoMergeChanges) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func squashMergeToBase(ctx context.Context, input SquashMergeInput, push bool) (SquashMergeResult, error) {
	input.ExchangeRepoPath = strings.TrimSpace(input.ExchangeRepoPath)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	input.Branch = strings.TrimSpace(input.Branch)
	input.Message = strings.TrimSpace(input.Message)
	if input.ExchangeRepoPath == "" {
		return SquashMergeResult{}, errors.New("exchange repo path is required")
	}
	if input.BaseBranch == "" {
		return SquashMergeResult{}, errors.New("base branch is required")
	}
	if input.Branch == "" {
		return SquashMergeResult{}, errors.New("issue branch is required")
	}
	if input.Message == "" && push {
		return SquashMergeResult{}, errors.New("merge message is required")
	}

	root, err := os.MkdirTemp("", "flow-merge-*")
	if err != nil {
		return SquashMergeResult{}, fmt.Errorf("create merge temp dir: %w", err)
	}
	defer os.RemoveAll(root)

	worktree := filepath.Join(root, "worktree")
	if err := gitRun(ctx, "", nil, "clone", input.ExchangeRepoPath, worktree); err != nil {
		return SquashMergeResult{}, fmt.Errorf("clone exchange for merge: %w", err)
	}
	if err := gitRun(ctx, worktree, nil, "config", "user.name", "Flow Coordinator"); err != nil {
		return SquashMergeResult{}, fmt.Errorf("configure merge user name: %w", err)
	}
	if err := gitRun(ctx, worktree, nil, "config", "user.email", "flow@example.invalid"); err != nil {
		return SquashMergeResult{}, fmt.Errorf("configure merge user email: %w", err)
	}
	if err := gitRun(ctx, worktree, nil, "checkout", "-B", input.BaseBranch, "origin/"+input.BaseBranch); err != nil {
		return SquashMergeResult{}, fmt.Errorf("checkout base branch: %w", err)
	}

	previousBaseSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return SquashMergeResult{}, fmt.Errorf("read base sha: %w", err)
	}
	headSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "origin/"+input.Branch)
	if err != nil {
		return SquashMergeResult{}, fmt.Errorf("read issue branch sha: %w", err)
	}
	if expected := strings.TrimSpace(input.ExpectedHeadSHA); expected != "" && headSHA != expected {
		return SquashMergeResult{}, fmt.Errorf("%w: %s, expected %s", ErrHeadMismatch, headSHA, expected)
	}
	if err := gitRun(ctx, worktree, nil, "merge", "--squash", "--no-commit", "origin/"+input.Branch); err != nil {
		if output, ok := mergeConflictOutput(err); ok {
			return SquashMergeResult{}, fmt.Errorf("squash merge issue branch: %w", &MergeConflictError{Output: output})
		}
		return SquashMergeResult{}, fmt.Errorf("squash merge issue branch: %w", err)
	}
	if err := restoreExcludedMergePaths(ctx, worktree); err != nil {
		return SquashMergeResult{}, err
	}
	exitCode, err := gitExitCode(ctx, worktree, "", nil, "diff", "--cached", "--quiet")
	if err != nil {
		return SquashMergeResult{}, fmt.Errorf("inspect staged merge diff: %w", err)
	}
	if exitCode == 0 {
		return SquashMergeResult{}, ErrNoMergeChanges
	}
	if exitCode != 1 {
		return SquashMergeResult{}, fmt.Errorf("git diff --cached returned exit code %d", exitCode)
	}
	if !push {
		return SquashMergeResult{
			PreviousBaseSHA: previousBaseSHA,
			HeadSHA:         headSHA,
		}, nil
	}
	if err := gitRun(ctx, worktree, nil, "commit", "-m", input.Message); err != nil {
		return SquashMergeResult{}, fmt.Errorf("commit squash merge: %w", err)
	}
	mergeSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return SquashMergeResult{}, fmt.Errorf("read merge sha: %w", err)
	}
	if err := gitRun(ctx, worktree, []string{"FLOW_GIT_PRINCIPAL=coordinator"}, "push", "origin", "HEAD:refs/heads/"+input.BaseBranch); err != nil {
		return SquashMergeResult{}, fmt.Errorf("push merged base branch: %w", err)
	}

	return SquashMergeResult{
		PreviousBaseSHA: previousBaseSHA,
		HeadSHA:         headSHA,
		MergeSHA:        mergeSHA,
	}, nil
}

func mergeConflictOutput(err error) (string, bool) {
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) {
		return "", false
	}

	var parts []string
	for _, part := range []string{gitErr.stdout, gitErr.stderr} {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	output := strings.Join(parts, "\n")
	if !strings.Contains(output, "CONFLICT") && !strings.Contains(output, "Automatic merge failed") {
		return output, false
	}

	return output, true
}

func restoreExcludedMergePaths(ctx context.Context, worktree string) error {
	args := append([]string{"diff", "--cached", "--name-only", "--"}, excludedMergePathRoots...)
	output, err := gitOutput(ctx, worktree, nil, args...)
	if err != nil {
		return fmt.Errorf("list excluded merge paths: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		path := strings.TrimSpace(line)
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	restoreArgs := append([]string{"restore", "--staged", "--worktree", "--source=HEAD", "--"}, paths...)
	if err := gitRun(ctx, worktree, nil, restoreArgs...); err != nil {
		return fmt.Errorf("restore excluded merge paths: %w", err)
	}

	return nil
}
