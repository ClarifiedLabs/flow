package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// ResetIssue discards every artifact of the issue's authoring attempts so the
// next author job starts from the base branch: live author jobs are canceled,
// active session tokens revoked, the issue's branches removed from the
// exchange, and the change projections (with their sessions, review threads,
// handoff snapshots, and merge intents, via FK cascade) plus the issue's check
// rows deleted. The issue's schedule and triage state are left untouched;
// re-enqueueing a fresh author job is the lifecycle engine's follow-up.
func (s *SessionService) ResetIssue(ctx context.Context, issueID string) (Issue, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return Issue{}, errors.New("issue id is required")
	}
	issue, err := s.issues.GetIssue(ctx, issueID)
	if err != nil {
		return Issue{}, err
	}
	if issue.ScheduleState == ScheduleClosed {
		return Issue{}, errors.New("closed issues cannot be reset")
	}
	if issue.TriageState != TriageAccepted {
		return Issue{}, errors.New("reset requires an accepted issue")
	}

	changes, err := s.changesForIssue(ctx, issueID)
	if err != nil {
		return Issue{}, err
	}
	for _, change := range changes {
		if change.MergedAt != nil {
			return Issue{}, errors.New("issues with a merged change cannot be reset")
		}
	}
	if len(changes) > 0 && strings.TrimSpace(s.project.ExchangePath) == "" {
		return Issue{}, errors.New("project exchange path is required to reset issue branches")
	}

	if _, err := s.workers.CancelLiveJobsForIssue(ctx, issueID, flowworker.RoleAuthor); err != nil {
		return Issue{}, fmt.Errorf("cancel live author jobs: %w", err)
	}

	// Token hashes are collected before the change delete cascades the session
	// rows away; the tokens themselves live in the global credential store.
	tokenHashes, err := s.activeSessionTokenHashesForIssue(ctx, issueID)
	if err != nil {
		return Issue{}, err
	}

	for _, change := range changes {
		if err := flowgit.DeleteBranch(ctx, flowgit.DeleteBranchInput{
			ExchangeRepoPath: s.project.ExchangePath,
			Branch:           change.Branch,
		}); err != nil {
			return Issue{}, err
		}
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM changes WHERE issue_id = ?`, issueID); err != nil {
		return Issue{}, fmt.Errorf("delete change projections: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE issue_id = ?`, issueID); err != nil {
		return Issue{}, fmt.Errorf("delete issue checks: %w", err)
	}

	var revokeErr error
	for _, tokenHash := range tokenHashes {
		revokeErr = errors.Join(revokeErr, s.revokeSessionTokenHash(ctx, tokenHash))
	}
	if revokeErr != nil {
		return Issue{}, fmt.Errorf("revoke session tokens: %w", revokeErr)
	}

	return s.issues.GetIssue(ctx, issueID)
}

func (s *SessionService) changesForIssue(ctx context.Context, issueID string) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE issue_id = ?
ORDER BY created_at, id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("select changes for issue: %w", err)
	}
	defer rows.Close()

	var changes []Change
	for rows.Next() {
		change, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate changes for issue: %w", err)
	}

	return changes, nil
}

func (s *SessionService) activeSessionTokenHashesForIssue(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT token_hash
FROM sessions
WHERE issue_id = ?
	AND runtime_state IN (?, ?, ?)`,
		issueID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	if err != nil {
		return nil, fmt.Errorf("select session tokens for issue: %w", err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan session token hash: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session token hashes: %w", err)
	}

	return hashes, nil
}
