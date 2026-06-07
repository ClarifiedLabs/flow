package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// errRecordMergedConflict means the merged_at latch did not apply: the change
// is already merged, missing, or its head moved since the merge was decided.
var errRecordMergedConflict = errors.New("change is already merged, missing, or head changed")

type MergeResult struct {
	Issue           Issue  `json:"issue"`
	Change          Change `json:"change"`
	PreviousBaseSHA string `json:"previous_base_sha"`
	HeadSHA         string `json:"head_sha"`
	MergeSHA        string `json:"merge_sha"`
}

type MergeService struct {
	db       *sql.DB
	issues   *IssueService
	sessions *SessionService
	project  Project
	now      func() time.Time

	// recoveryBackoff throttles per-intent recovery retries: resolving a
	// base-advanced intent clones the exchange, so a persistently-failing
	// intent must not re-clone on every 5s tick. In-memory only — a restart
	// costs at most one extra attempt per intent.
	recoveryBackoff map[string]mergeRecoveryBackoff
}

type mergeRecoveryBackoff struct {
	failures    int
	nextAttempt time.Time
}

func NewMergeService(database *sql.DB, issues *IssueService, sessions *SessionService, project Project) *MergeService {
	if issues == nil {
		issues = NewIssueService(database)
	}
	if sessions == nil {
		sessions = NewSessionService(database, issues, nil)
	}
	return &MergeService{
		db:              database,
		issues:          issues,
		sessions:        sessions,
		project:         project,
		now:             sqlitex.UTCNow,
		recoveryBackoff: map[string]mergeRecoveryBackoff{},
	}
}

func (s *MergeService) MergeIssue(ctx context.Context, issueID string) (MergeResult, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return MergeResult{}, errors.New("issue id is required")
	}
	issue, err := s.issues.GetIssue(ctx, issueID)
	if err != nil {
		return MergeResult{}, err
	}
	change, ok, err := s.sessions.ReadyUnmergedChangeForIssue(ctx, issue.ID)
	if err != nil {
		return MergeResult{}, err
	}
	if !ok {
		return MergeResult{}, errors.New("issue has no ready unmerged change")
	}

	return s.mergeApprovedChange(ctx, issue, change)
}

func (s *MergeService) MergeChange(ctx context.Context, changeID string) (MergeResult, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return MergeResult{}, errors.New("change id is required")
	}
	change, err := s.sessions.GetChange(ctx, changeID)
	if err != nil {
		return MergeResult{}, err
	}
	if change.ReadyAt == nil {
		return MergeResult{}, errors.New("change is not ready")
	}
	if change.MergedAt != nil {
		return MergeResult{}, errors.New("change is already merged")
	}
	issue, err := s.issues.GetIssue(ctx, change.IssueID)
	if err != nil {
		return MergeResult{}, err
	}
	currentChange, ok, err := s.sessions.ReadyUnmergedChangeForIssue(ctx, issue.ID)
	if err != nil {
		return MergeResult{}, err
	}
	if !ok || currentChange.ID != change.ID {
		return MergeResult{}, errors.New("change is not the current ready unmerged change")
	}

	return s.mergeApprovedChange(ctx, issue, change)
}

func (s *MergeService) ChangeMergeEligibility(ctx context.Context, issue Issue, change Change) (bool, string, error) {
	if issue.ScheduleState == ScheduleClosed {
		return false, "issue is closed", nil
	}
	if change.ReadyAt == nil {
		return false, "change is not ready", nil
	}
	if change.MergedAt != nil {
		return false, "change is already merged", nil
	}
	currentChange, ok, err := s.sessions.ReadyUnmergedChangeForIssue(ctx, issue.ID)
	if err != nil {
		return false, "", err
	}
	if !ok || currentChange.ID != change.ID {
		return false, "change is not the current ready unmerged change", nil
	}
	reviewState, err := s.issues.reviewState(ctx, issue.ID)
	if err != nil {
		return false, "", err
	}
	if reviewState != ReviewApproved {
		return false, fmt.Sprintf("merge requires approved review state, got %s", reviewState), nil
	}
	if strings.TrimSpace(change.HeadSHA) == "" {
		return false, "change head sha is required", nil
	}
	if _, err := s.exchangePathForChange(ctx, change); err != nil {
		return false, err.Error(), nil
	}

	return true, "", nil
}

func (s *MergeService) ExchangePathForChange(ctx context.Context, change Change) (string, error) {
	return s.exchangePathForChange(ctx, change)
}

// MergeBaseForChange returns the pre-merge base tip (previous_base_sha) for a
// merged change by looking up its most recent completed merge intent. After a
// squash merge the base ref advances to a commit whose tree equals the branch
// content, so diffing the current base ref against the head is empty; the
// recorded previous_base_sha is the pre-merge tip that produces the real diff.
// ok is false (with a nil error) when no completed intent exists for the change.
func (s *MergeService) MergeBaseForChange(ctx context.Context, change Change) (string, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT previous_base_sha
FROM merge_intents
WHERE change_id = ? AND completed_at IS NOT NULL
ORDER BY completed_at DESC
LIMIT 1`, change.ID)
	var baseSHA string
	switch err := row.Scan(&baseSHA); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("load merge base for change: %w", err)
	}
	if strings.TrimSpace(baseSHA) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(baseSHA), true, nil
}

func (s *MergeService) mergeApprovedChange(ctx context.Context, issue Issue, change Change) (MergeResult, error) {
	if issue.ScheduleState == ScheduleClosed {
		return MergeResult{}, errors.New("closed issues cannot be merged")
	}
	reviewState, err := s.issues.reviewState(ctx, issue.ID)
	if err != nil {
		return MergeResult{}, err
	}
	if reviewState != ReviewApproved {
		return MergeResult{}, fmt.Errorf("merge requires approved review state, got %s", reviewState)
	}
	if strings.TrimSpace(change.HeadSHA) == "" {
		return MergeResult{}, errors.New("change head sha is required")
	}
	exchangePath, err := s.exchangePathForChange(ctx, change)
	if err != nil {
		return MergeResult{}, err
	}
	intent, err := s.ensureMergeIntent(ctx, issue.ID, change, exchangePath)
	if err != nil {
		return MergeResult{}, err
	}
	gitResult, err := flowgit.SquashMergeToBase(ctx, flowgit.SquashMergeInput{
		ExchangeRepoPath: exchangePath,
		BaseBranch:       change.Base,
		Branch:           change.Branch,
		ExpectedHeadSHA:  strings.TrimSpace(change.HeadSHA),
		Message:          mergeCommitMessage(issue, change),
	})
	if errors.Is(err, flowgit.ErrNoMergeChanges) {
		// An empty squash against a base that advanced past the intent's
		// recorded tip is the signature of a crashed merge whose push landed
		// but whose recordMerged never committed: the just-failed squash is
		// itself the proof the branch's content is already in the base.
		healed, result, healErr := s.healStrandedMerge(ctx, intent, change)
		if healErr != nil {
			return MergeResult{}, healErr
		}
		if healed {
			return result, nil
		}
		// A genuine no-op merge (base never moved): drop the intent litter.
		if delErr := s.deleteMergeIntent(ctx, intent.ID); delErr != nil {
			return MergeResult{}, errors.Join(err, delErr)
		}
		return MergeResult{}, err
	}
	if err != nil {
		// The push may or may not have landed (e.g. a timeout mid-push). Leave
		// the intent open: the recovery pass completes it if the base advanced
		// with this branch's content, and deletes it otherwise.
		return MergeResult{}, err
	}
	mergedChange, mergedIssue, err := s.recordMerged(ctx, issue.ID, change.ID, strings.TrimSpace(change.HeadSHA))
	if err != nil {
		return MergeResult{}, err
	}
	if err := s.completeMergeIntent(ctx, intent.ID); err != nil {
		return MergeResult{}, err
	}

	return MergeResult{
		Issue:           mergedIssue,
		Change:          mergedChange,
		PreviousBaseSHA: gitResult.PreviousBaseSHA,
		HeadSHA:         gitResult.HeadSHA,
		MergeSHA:        gitResult.MergeSHA,
	}, nil
}

// healStrandedMerge completes a merge whose push landed but whose recordMerged
// was lost: the intent is open, the base advanced past the tip recorded before
// the push, and the caller just proved the branch squashes to a no-op against
// the current base. Reports healed=false when the base never moved (a genuine
// empty merge).
func (s *MergeService) healStrandedMerge(ctx context.Context, intent mergeIntent, change Change) (bool, MergeResult, error) {
	baseTip, ok, err := flowgit.BranchTip(ctx, intent.ExchangePath, intent.BaseBranch)
	if err != nil {
		return false, MergeResult{}, err
	}
	if !ok || baseTip == intent.PreviousBaseSHA {
		return false, MergeResult{}, nil
	}
	headSHA := strings.TrimSpace(change.HeadSHA)
	mergedChange, mergedIssue, err := s.recordMerged(ctx, intent.IssueID, intent.ChangeID, headSHA)
	if err != nil {
		return false, MergeResult{}, fmt.Errorf("complete stranded merge: %w", err)
	}
	if err := s.completeMergeIntent(ctx, intent.ID); err != nil {
		return false, MergeResult{}, err
	}

	return true, MergeResult{
		Issue:           mergedIssue,
		Change:          mergedChange,
		PreviousBaseSHA: intent.PreviousBaseSHA,
		HeadSHA:         headSHA,
		MergeSHA:        baseTip,
	}, nil
}

// RecoverPendingMerges resolves merge intents stranded by a crash between the
// exchange push and recordMerged. An intent whose base advanced AND whose
// branch now squashes to a no-op against that base is completed (the push
// landed); any other open intent is deleted so the normal merge path can
// re-attempt cleanly. Intents are fault-isolated: one failure is joined and
// the rest still resolve. Returns the number of merges completed.
func (s *MergeService) RecoverPendingMerges(ctx context.Context) (int, error) {
	intents, err := s.openMergeIntents(ctx)
	if err != nil {
		return 0, err
	}
	recovered := 0
	var errs error
	now := s.now().UTC()
	for _, intent := range intents {
		if backoff, ok := s.recoveryBackoff[intent.ID]; ok && now.Before(backoff.nextAttempt) {
			continue
		}
		done, err := s.recoverMergeIntent(ctx, intent)
		if err != nil {
			backoff := s.recoveryBackoff[intent.ID]
			backoff.failures++
			delay := mergeRecoveryBaseDelay << min(backoff.failures-1, 5)
			backoff.nextAttempt = now.Add(delay)
			s.recoveryBackoff[intent.ID] = backoff
			errs = errors.Join(errs, fmt.Errorf("recover merge intent %s: %w", intent.ID, err))
			continue
		}
		delete(s.recoveryBackoff, intent.ID)
		if done {
			recovered++
		}
	}
	return recovered, errs
}

// mergeRecoveryBaseDelay seeds the doubling backoff for failing intent
// recovery (30s, 1m, ... capped at 16m).
const mergeRecoveryBaseDelay = 30 * time.Second

func (s *MergeService) recoverMergeIntent(ctx context.Context, intent mergeIntent) (bool, error) {
	change, err := s.sessions.GetChange(ctx, intent.ChangeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, s.deleteMergeIntent(ctx, intent.ID)
		}
		return false, err
	}
	if change.MergedAt != nil {
		return false, s.completeMergeIntent(ctx, intent.ID)
	}
	baseTip, ok, err := flowgit.BranchTip(ctx, intent.ExchangePath, intent.BaseBranch)
	if err != nil {
		return false, err
	}
	if !ok || baseTip == intent.PreviousBaseSHA {
		// The push never landed; the normal merge path re-attempts from a
		// clean slate.
		return false, s.deleteMergeIntent(ctx, intent.ID)
	}
	// The base advanced. Complete the merge only on proof that this branch's
	// content (at the head the intent was written for) is already contained:
	// the squash against the current base must stage nothing.
	noop, err := flowgit.SquashMergeIsNoop(ctx, flowgit.SquashMergeInput{
		ExchangeRepoPath: intent.ExchangePath,
		BaseBranch:       intent.BaseBranch,
		Branch:           change.Branch,
		ExpectedHeadSHA:  intent.HeadSHA,
	})
	if err != nil {
		var conflict *flowgit.MergeConflictError
		if errors.As(err, &conflict) || errors.Is(err, flowgit.ErrHeadMismatch) {
			// The branch moved on or conflicts with the new base — whatever
			// advanced the base, it was not this intent's merge. Let the
			// normal path deal with the current state.
			return false, s.deleteMergeIntent(ctx, intent.ID)
		}
		return false, err
	}
	if !noop {
		return false, s.deleteMergeIntent(ctx, intent.ID)
	}
	if _, _, err := s.recordMerged(ctx, intent.IssueID, intent.ChangeID, intent.HeadSHA); err != nil {
		if errors.Is(err, errRecordMergedConflict) {
			return false, s.deleteMergeIntent(ctx, intent.ID)
		}
		return false, err
	}
	if err := s.completeMergeIntent(ctx, intent.ID); err != nil {
		return true, err
	}
	return true, nil
}

// mergeIntent mirrors a merge_intents row (see migration 0017).
type mergeIntent struct {
	ID              string
	IssueID         string
	ChangeID        string
	BaseBranch      string
	ExchangePath    string
	HeadSHA         string
	PreviousBaseSHA string
}

// ensureMergeIntent durably records the upcoming push, or adopts the open
// intent a crashed earlier attempt left behind (whose previous_base_sha must be
// preserved — it is the pre-push base tip that recovery and healing compare
// against).
func (s *MergeService) ensureMergeIntent(ctx context.Context, issueID string, change Change, exchangePath string) (mergeIntent, error) {
	if existing, ok, err := s.openMergeIntentForChange(ctx, change.ID); err != nil {
		return mergeIntent{}, err
	} else if ok {
		return existing, nil
	}
	baseTip, ok, err := flowgit.BranchTip(ctx, exchangePath, change.Base)
	if err != nil {
		return mergeIntent{}, err
	}
	if !ok {
		return mergeIntent{}, fmt.Errorf("base branch %s not found in exchange", change.Base)
	}
	id, err := randomPrefixedID("mi")
	if err != nil {
		return mergeIntent{}, err
	}
	intent := mergeIntent{
		ID:              id,
		IssueID:         issueID,
		ChangeID:        change.ID,
		BaseBranch:      change.Base,
		ExchangePath:    exchangePath,
		HeadSHA:         strings.TrimSpace(change.HeadSHA),
		PreviousBaseSHA: baseTip,
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO merge_intents
	(id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.ID, intent.IssueID, intent.ChangeID, intent.BaseBranch,
		intent.ExchangePath, intent.HeadSHA, intent.PreviousBaseSHA, formatTime(s.now().UTC())); err != nil {
		// The partial unique index admits one open intent per change; losing
		// that race means another attempt just wrote it — adopt theirs.
		if existing, ok, lookupErr := s.openMergeIntentForChange(ctx, change.ID); lookupErr == nil && ok {
			return existing, nil
		}
		return mergeIntent{}, fmt.Errorf("record merge intent: %w", err)
	}
	return intent, nil
}

func (s *MergeService) openMergeIntentForChange(ctx context.Context, changeID string) (mergeIntent, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha
FROM merge_intents
WHERE change_id = ? AND completed_at IS NULL`, changeID)
	var intent mergeIntent
	err := row.Scan(&intent.ID, &intent.IssueID, &intent.ChangeID, &intent.BaseBranch,
		&intent.ExchangePath, &intent.HeadSHA, &intent.PreviousBaseSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return mergeIntent{}, false, nil
	}
	if err != nil {
		return mergeIntent{}, false, fmt.Errorf("load open merge intent: %w", err)
	}
	return intent, true, nil
}

func (s *MergeService) openMergeIntents(ctx context.Context) ([]mergeIntent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha
FROM merge_intents
WHERE completed_at IS NULL
ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list open merge intents: %w", err)
	}
	defer rows.Close()
	var intents []mergeIntent
	for rows.Next() {
		var intent mergeIntent
		if err := rows.Scan(&intent.ID, &intent.IssueID, &intent.ChangeID, &intent.BaseBranch,
			&intent.ExchangePath, &intent.HeadSHA, &intent.PreviousBaseSHA); err != nil {
			return nil, fmt.Errorf("scan merge intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate merge intents: %w", err)
	}
	return intents, nil
}

func (s *MergeService) completeMergeIntent(ctx context.Context, intentID string) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE merge_intents SET completed_at = ? WHERE id = ? AND completed_at IS NULL`,
		formatTime(s.now().UTC()), intentID); err != nil {
		return fmt.Errorf("complete merge intent: %w", err)
	}
	return nil
}

func (s *MergeService) deleteMergeIntent(ctx context.Context, intentID string) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM merge_intents WHERE id = ? AND completed_at IS NULL`, intentID); err != nil {
		return fmt.Errorf("delete merge intent: %w", err)
	}
	return nil
}

func (s *MergeService) exchangePathForChange(_ context.Context, _ Change) (string, error) {
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return "", errors.New("project exchange path is not local")
	}

	return exchangePath, nil
}

func (s *MergeService) recordMerged(ctx context.Context, issueID string, changeID string, headSHA string) (Change, Issue, error) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return Change{}, Issue{}, errors.New("head sha is required")
	}
	nowText := formatTime(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Change{}, Issue{}, fmt.Errorf("begin merge transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE changes
SET merged_at = COALESCE(merged_at, ?),
	updated_at = ?
WHERE id = ?
	AND issue_id = ?
	AND head_sha = ?
	AND merged_at IS NULL`,
		nowText,
		nowText,
		changeID,
		issueID,
		headSHA,
	)
	if err != nil {
		return Change{}, Issue{}, fmt.Errorf("mark change merged: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Change{}, Issue{}, fmt.Errorf("read merged rows affected: %w", err)
	}
	if rows == 0 {
		return Change{}, Issue{}, errRecordMergedConflict
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE issues
SET schedule_state = ?,
	closed_at = COALESCE(closed_at, ?),
	updated_at = ?
WHERE id = ?`,
		string(ScheduleClosed),
		nowText,
		nowText,
		issueID,
	); err != nil {
		return Change{}, Issue{}, fmt.Errorf("close merged issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Change{}, Issue{}, fmt.Errorf("commit merge transaction: %w", err)
	}

	change, err := s.sessions.GetChange(ctx, changeID)
	if err != nil {
		return Change{}, Issue{}, err
	}
	issue, err := s.issues.GetIssue(ctx, issueID)
	if err != nil {
		return Change{}, Issue{}, err
	}

	return change, issue, nil
}

func mergeCommitMessage(issue Issue, change Change) string {
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = issue.ID
	}

	return fmt.Sprintf("Merge %s: %s\n\nSquash merge change %s from %s.", issue.ID, title, change.ID, change.Branch)
}
