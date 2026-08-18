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

// RebaseBranchResult is a rebased head plus the throwaway clone that produced
// it. The clone exists only when the caller asked to retain it, so the rebased
// objects can be pushed from it before the caller removes the directory.
type RebaseBranchResult struct {
	PreviousSHA string
	HeadSHA     string
	Worktree    string
}

// RebaseOnto replays Branch onto the Onto tip inside a throwaway clone of the
// exchange and, on success, pushes the new head back to the branch ref with a
// compare-and-swap lease on ExpectedOldSHA as the coordinator. A conflict
// aborts the in-clone rebase — leaving the exchange untouched — and returns a
// *RebaseConflictError.
//
// Callers that must record durable publication intent before the ref can move
// (the feature-rebase coordinator) use RebaseBranch plus their own
// compare-and-swap push so the intent row is committed first.
func RebaseOnto(ctx context.Context, input RebaseOntoInput) (RebaseOntoResult, error) {
	result, err := rebaseBranchCloned(ctx, input)
	if result.Worktree != "" {
		defer os.RemoveAll(result.Worktree)
	}
	if err != nil {
		return RebaseOntoResult{}, err
	}
	branchRef := "refs/heads/" + input.Branch
	if err := gitRun(ctx, result.Worktree, []string{"FLOW_GIT_PRINCIPAL=" + coordinatorActor},
		"push", "--force-with-lease="+branchRef+":"+input.ExpectedOldSHA, input.ExchangePath, "HEAD:"+branchRef); err != nil {
		return RebaseOntoResult{}, fmt.Errorf("push rebased branch: %w", err)
	}
	return RebaseOntoResult{PreviousSHA: result.PreviousSHA, HeadSHA: result.HeadSHA}, nil
}

// RebaseBranchCloned is RebaseBranch with the throwaway clone retained. The
// caller owns the returned Worktree directory: the rebased head's objects live
// only there, so the compare-and-swap push must run from it, and the caller
// must os.RemoveAll it afterwards. The exchange ref is untouched by this call.
func RebaseBranchCloned(ctx context.Context, input RebaseOntoInput) (RebaseBranchResult, error) {
	result, err := rebaseBranchCloned(ctx, input)
	if err != nil {
		if result.Worktree != "" {
			_ = os.RemoveAll(result.Worktree)
		}
		return RebaseBranchResult{}, err
	}
	return result, nil
}

// RebaseBranch replays Branch onto the Onto tip inside a throwaway clone of
// the exchange and returns the rebased head without touching any exchange ref:
// the clone is discarded, so the exchange still points at PreviousSHA when this
// returns. A conflict aborts the in-clone rebase and returns a
// *RebaseConflictError.
//
// RebaseOnto wraps this with the coordinator's compare-and-swap push.
func RebaseBranch(ctx context.Context, input RebaseOntoInput) (RebaseOntoResult, error) {
	result, err := rebaseBranchCloned(ctx, input)
	if result.Worktree != "" {
		defer os.RemoveAll(result.Worktree)
	}
	if err != nil {
		return RebaseOntoResult{}, err
	}
	return RebaseOntoResult{PreviousSHA: result.PreviousSHA, HeadSHA: result.HeadSHA}, nil
}

// rebaseBranchCloned performs the clone/rebase/verify sequence and returns the
// rebased head with the clone directory retained for the caller to clean up.
func rebaseBranchCloned(ctx context.Context, input RebaseOntoInput) (RebaseBranchResult, error) {
	input.ExchangePath = strings.TrimSpace(input.ExchangePath)
	input.Branch = strings.TrimSpace(input.Branch)
	input.Onto = strings.TrimSpace(input.Onto)
	input.ExpectedOldSHA = strings.TrimSpace(input.ExpectedOldSHA)
	input.ExpectedOntoSHA = strings.TrimSpace(input.ExpectedOntoSHA)
	if input.ExchangePath == "" {
		return RebaseBranchResult{}, errors.New("exchange repo path is required")
	}
	if input.Branch == "" {
		return RebaseBranchResult{}, errors.New("branch is required")
	}
	if input.Onto == "" {
		return RebaseBranchResult{}, errors.New("onto branch is required")
	}
	if input.ExpectedOldSHA == "" {
		return RebaseBranchResult{}, errors.New("expected branch sha is required")
	}

	root, err := os.MkdirTemp("", "flow-rebase-*")
	if err != nil {
		return RebaseBranchResult{}, fmt.Errorf("create rebase temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	worktree := filepath.Join(root, "worktree")
	// fail closes over cleanup so every error path removes the clone; the
	// success path hands the worktree to the caller to own.
	fail := func(err error) (RebaseBranchResult, error) {
		cleanup()
		return RebaseBranchResult{}, err
	}

	if err := gitRun(ctx, "", nil, "clone", input.ExchangePath, worktree); err != nil {
		return fail(fmt.Errorf("clone exchange for rebase: %w", err))
	}
	identity := CommitIdentity{Name: input.CommitName, Email: input.CommitEmail}.WithDefaults(CommitIdentity{
		Name:  DefaultMergeCommitName,
		Email: DefaultMergeCommitEmail,
	})
	if err := gitRun(ctx, worktree, nil, "config", "user.name", identity.Name); err != nil {
		return fail(fmt.Errorf("configure rebase user name: %w", err))
	}
	if err := gitRun(ctx, worktree, nil, "config", "user.email", identity.Email); err != nil {
		return fail(fmt.Errorf("configure rebase user email: %w", err))
	}
	if err := gitRun(ctx, worktree, nil, "checkout", "-B", input.Branch, "origin/"+input.Branch); err != nil {
		return fail(fmt.Errorf("checkout branch: %w", err))
	}
	previousSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return fail(fmt.Errorf("resolve branch head: %w", err))
	}
	if previousSHA != input.ExpectedOldSHA {
		return fail(fmt.Errorf("%w: branch head is %s", ErrHeadMismatch, previousSHA))
	}
	ontoSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "origin/"+input.Onto)
	if err != nil {
		return fail(fmt.Errorf("resolve onto branch head: %w", err))
	}
	if input.ExpectedOntoSHA != "" && ontoSHA != input.ExpectedOntoSHA {
		return fail(fmt.Errorf("%w: %s, expected %s", ErrBaseMismatch, ontoSHA, input.ExpectedOntoSHA))
	}

	if err := gitRun(ctx, worktree, nil, "-c", "core.editor=true", "rebase", "origin/"+input.Onto); err != nil {
		if rebaseInProgress(worktree) {
			output := rebaseConflictOutput(ctx, worktree, err)
			// The rebase state lives only in the throwaway clone; abort so the
			// temp dir removes cleanly and the exchange stays untouched.
			_ = gitRun(ctx, worktree, nil, "rebase", "--abort")
			return fail(&RebaseConflictError{Output: output})
		}
		return fail(fmt.Errorf("rebase onto %s: %w", input.Onto, err))
	}

	headSHA, err := gitOutput(ctx, worktree, nil, "rev-parse", "HEAD")
	if err != nil {
		return fail(fmt.Errorf("resolve rebased head: %w", err))
	}

	return RebaseBranchResult{PreviousSHA: previousSHA, HeadSHA: headSHA, Worktree: worktree}, nil
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
