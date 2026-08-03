package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrRebaseConflict reports a rebase that stopped on conflicting changes.
var ErrRebaseConflict = errors.New("rebase conflict")

// RebaseConflictError carries git's conflict summary so the caller can hand
// the work to an agent with the conflicting paths already named.
type RebaseConflictError struct {
	Output string
}

func (e *RebaseConflictError) Error() string {
	output := strings.TrimSpace(e.Output)
	if output == "" {
		return ErrRebaseConflict.Error()
	}

	return fmt.Sprintf("%s: %s", ErrRebaseConflict, output)
}

func (e *RebaseConflictError) Unwrap() error {
	return ErrRebaseConflict
}

type RebaseOntoInput struct {
	// ExchangePath is the project's bare exchange repository.
	ExchangePath string
	// Branch is the branch to rebase (a feature branch).
	Branch string
	// Onto is the base branch the branch is replayed onto.
	Onto string
	// ExpectedOldSHA is the branch head the rebase must start from; a
	// mismatch means the branch moved after the rebase was planned.
	ExpectedOldSHA string
	// ExpectedOntoSHA pins the integration target resolved by the caller. It is
	// optional for general callers and required by feature rebases.
	ExpectedOntoSHA string
	// CommitName and CommitEmail set the replayed commits' committer identity.
	// When empty, DefaultMergeCommitName/DefaultMergeCommitEmail are used.
	CommitName  string
	CommitEmail string
}

type RebaseOntoResult struct {
	PreviousSHA string
	HeadSHA     string
}

// RebaseOnto replays Branch onto the Onto tip inside a throwaway clone of the
// exchange and, on success, pushes the new head back to the branch ref with a
// compare-and-swap lease on ExpectedOldSHA as the coordinator. A conflict
// aborts the in-clone rebase — leaving the exchange untouched — and returns a
// *RebaseConflictError.
func RebaseOnto(ctx context.Context, input RebaseOntoInput) (RebaseOntoResult, error) {
	input.ExchangePath = strings.TrimSpace(input.ExchangePath)
	input.Branch = strings.TrimSpace(input.Branch)
	input.Onto = strings.TrimSpace(input.Onto)
	input.ExpectedOldSHA = strings.TrimSpace(input.ExpectedOldSHA)
	input.ExpectedOntoSHA = strings.TrimSpace(input.ExpectedOntoSHA)
	if input.ExchangePath == "" {
		return RebaseOntoResult{}, errors.New("exchange repo path is required")
	}
	if input.Branch == "" {
		return RebaseOntoResult{}, errors.New("branch is required")
	}
	if input.Onto == "" {
		return RebaseOntoResult{}, errors.New("onto branch is required")
	}
	if input.ExpectedOldSHA == "" {
		return RebaseOntoResult{}, errors.New("expected branch sha is required")
	}

	root, err := os.MkdirTemp("", "flow-rebase-*")
	if err != nil {
		return RebaseOntoResult{}, fmt.Errorf("create rebase temp dir: %w", err)
	}
	defer os.RemoveAll(root)

	worktree := filepath.Join(root, "worktree")
	if err := gitRun(ctx, "", nil, "clone", input.ExchangePath, worktree); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("clone exchange for rebase: %w", err)
	}
	identity := CommitIdentity{Name: input.CommitName, Email: input.CommitEmail}.WithDefaults(CommitIdentity{
		Name:  DefaultMergeCommitName,
		Email: DefaultMergeCommitEmail,
	})
	if err := gitRun(ctx, worktree, nil, "config", "user.name", identity.Name); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("configure rebase user name: %w", err)
	}
	if err := gitRun(ctx, worktree, nil, "config", "user.email", identity.Email); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("configure rebase user email: %w", err)
	}
	if err := gitRun(ctx, worktree, nil, "checkout", "-B", input.Branch, "origin/"+input.Branch); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("checkout branch: %w", err)
	}
	previousSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return RebaseOntoResult{}, fmt.Errorf("resolve branch head: %w", err)
	}
	if previousSHA != input.ExpectedOldSHA {
		return RebaseOntoResult{}, fmt.Errorf("%w: branch head is %s", ErrHeadMismatch, previousSHA)
	}
	ontoSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "origin/"+input.Onto)
	if err != nil {
		return RebaseOntoResult{}, fmt.Errorf("resolve onto branch head: %w", err)
	}
	if input.ExpectedOntoSHA != "" && ontoSHA != input.ExpectedOntoSHA {
		return RebaseOntoResult{}, fmt.Errorf("%w: %s, expected %s", ErrBaseMismatch, ontoSHA, input.ExpectedOntoSHA)
	}

	if err := gitRun(ctx, worktree, nil, "-c", "core.editor=true", "rebase", "origin/"+input.Onto); err != nil {
		if rebaseInProgress(worktree) {
			output := rebaseConflictOutput(ctx, worktree, err)
			// The rebase state lives only in the throwaway clone; abort so the
			// temp dir removes cleanly and the exchange stays untouched.
			_ = gitRun(ctx, worktree, nil, "rebase", "--abort")
			return RebaseOntoResult{}, &RebaseConflictError{Output: output}
		}
		return RebaseOntoResult{}, fmt.Errorf("rebase onto %s: %w", input.Onto, err)
	}

	headSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return RebaseOntoResult{}, fmt.Errorf("resolve rebased head: %w", err)
	}
	branchRef := "refs/heads/" + input.Branch
	if err := gitRun(ctx, worktree, []string{"FLOW_GIT_PRINCIPAL=" + coordinatorActor},
		"push", "--force-with-lease="+branchRef+":"+input.ExpectedOldSHA, "origin", "HEAD:"+branchRef); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("push rebased branch: %w", err)
	}

	return RebaseOntoResult{PreviousSHA: previousSHA, HeadSHA: headSHA}, nil
}

// rebaseInProgress reports whether a rebase stopped mid-way in the worktree
// (as opposed to git failing outright, e.g. on a missing object).
func rebaseInProgress(worktree string) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(worktree, ".git", dir)); err == nil {
			return true
		}
	}
	return false
}

func rebaseConflictOutput(ctx context.Context, worktree string, rebaseErr error) string {
	var builder strings.Builder
	builder.WriteString(rebaseErr.Error())
	status, err := gitOutput(ctx, worktree, nil, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return builder.String()
	}
	var unmerged []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 3 {
			continue
		}
		switch line[:2] {
		case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
			unmerged = append(unmerged, strings.TrimSpace(line[3:]))
		}
	}
	if len(unmerged) > 0 {
		builder.WriteString("\nconflicting paths:\n")
		builder.WriteString(strings.Join(unmerged, "\n"))
	}

	return builder.String()
}
