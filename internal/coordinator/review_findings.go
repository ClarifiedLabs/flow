package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskReviewFinding is one review thread in a task's findings registry: the
// blocking concern a reviewer filed against any of the task's changes, with
// its current resolution state and the original finding body (the thread's
// opening comment).
type TaskReviewFinding struct {
	ID             string            `json:"id"`
	ChangeID       string            `json:"change_id"`
	FilePath       string            `json:"file_path"`
	Line           int               `json:"line"`
	State          ReviewThreadState `json:"state"`
	ClaimKind      *ReviewClaimKind  `json:"claim_kind,omitempty"`
	ClaimCommitSHA *string           `json:"claim_commit_sha,omitempty"`
	ClaimedBy      *string           `json:"claimed_by,omitempty"`
	ClaimedAt      *time.Time        `json:"claimed_at,omitempty"`
	CertifiedBy    *string           `json:"certified_by,omitempty"`
	CertifiedAt    *time.Time        `json:"certified_at,omitempty"`
	ReopenedBy     *string           `json:"reopened_by,omitempty"`
	ReopenedAt     *time.Time        `json:"reopened_at,omitempty"`
	// Finding is the original finding body: the thread's first review comment.
	Finding   string    `json:"finding"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskReviewFollowUp is one non-blocking finding the review aggregator
// dispositioned to a follow-up task: the action taken (create_task or
// use_existing_task), the target task, the originating check, and the finding
// hash recorded when the action was applied.
type TaskReviewFollowUp struct {
	Action string `json:"action"`
	// CheckName is the check whose review reported the finding.
	CheckName string `json:"check_name"`
	// FindingHash is the deterministic digest of the finding the aggregator
	// deferred; the finding text itself is not stored with the action.
	FindingHash string `json:"finding_hash"`
	// TargetTaskID is the follow-up task the finding was deferred to.
	TargetTaskID string `json:"target_task_id"`
	// TargetTaskTitle denormalizes the follow-up task's title at read time.
	TargetTaskTitle string `json:"target_task_title"`
	// RelatedAt is when the related_to task relation backing the deferral was
	// recorded; it is absent if the relation has since been unlinked.
	RelatedAt *time.Time `json:"related_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// TaskFindingsSummary counts the task's findings by resolution bucket. The
// buckets are mutually exclusive: a certified thread counts as certified, not
// as resolved-fixed, even though it was claimed fixed before verification.
// Unresolved covers open and reopened threads; deferred-to-task counts
// follow-up actions.
type TaskFindingsSummary struct {
	ResolvedFixed        int `json:"resolved_fixed"`
	ResolvedNotWarranted int `json:"resolved_not_warranted"`
	ResolvedSuperseded   int `json:"resolved_superseded"`
	Certified            int `json:"certified"`
	Unresolved           int `json:"unresolved"`
	DeferredToTask       int `json:"deferred_to_task"`
}

// TaskFindingsRegistry is the complete read-only registry of a task's review
// findings: every review thread across all of the task's changes, every
// deferred follow-up action, and bucket counts over both.
type TaskFindingsRegistry struct {
	TaskID    string
	Findings  []TaskReviewFinding
	FollowUps []TaskReviewFollowUp
	Summary   TaskFindingsSummary
}

// TaskFindingsRegistry aggregates the review findings registry for one task:
// all review threads across the task's changes (not only the latest change),
// all review follow-up actions joined to their target tasks and backing
// relations, and resolution-bucket counts. It is a pure read surface; unknown
// task ids surface as sql.ErrNoRows.
func (s *ThreadService) TaskFindingsRegistry(ctx context.Context, taskID string) (TaskFindingsRegistry, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TaskFindingsRegistry{}, errors.New("task id is required")
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, taskID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskFindingsRegistry{}, sql.ErrNoRows
		}
		return TaskFindingsRegistry{}, fmt.Errorf("check findings task existence: %w", err)
	}

	registry := TaskFindingsRegistry{
		TaskID:    taskID,
		Findings:  []TaskReviewFinding{},
		FollowUps: []TaskReviewFollowUp{},
	}

	rows, err := s.db.QueryContext(ctx, taskReviewFindingSelectSQL+`
WHERE task_id = ?
ORDER BY created_at, id`, taskID)
	if err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("list review findings: %w", err)
	}
	for rows.Next() {
		finding, err := scanTaskReviewFinding(rows)
		if err != nil {
			return TaskFindingsRegistry{}, err
		}
		registry.Findings = append(registry.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("iterate review findings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("close review finding rows: %w", err)
	}

	followUpRows, err := s.db.QueryContext(ctx, taskReviewFollowUpSelectSQL+`
WHERE f.source_task_id = ?
ORDER BY f.created_at, f.finding_hash`, string(RelationRelatedTo), taskID)
	if err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("list review follow-ups: %w", err)
	}
	for followUpRows.Next() {
		followUp, err := scanTaskReviewFollowUp(followUpRows)
		if err != nil {
			return TaskFindingsRegistry{}, err
		}
		registry.FollowUps = append(registry.FollowUps, followUp)
	}
	if err := followUpRows.Err(); err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("iterate review follow-ups: %w", err)
	}
	if err := followUpRows.Close(); err != nil {
		return TaskFindingsRegistry{}, fmt.Errorf("close review follow-up rows: %w", err)
	}

	registry.Summary = summarizeTaskFindings(registry.Findings, registry.FollowUps)
	return registry, nil
}

// summarizeTaskFindings buckets every finding exactly once. Certified threads
// are their own bucket; claimed threads split by claim kind; open and reopened
// threads are unresolved; deferred-to-task counts the follow-up actions.
func summarizeTaskFindings(findings []TaskReviewFinding, followUps []TaskReviewFollowUp) TaskFindingsSummary {
	var summary TaskFindingsSummary
	for _, finding := range findings {
		switch finding.State {
		case ThreadCertified:
			summary.Certified++
		case ThreadClaimed:
			switch {
			case finding.ClaimKind != nil && *finding.ClaimKind == ClaimFixed:
				summary.ResolvedFixed++
			case finding.ClaimKind != nil && *finding.ClaimKind == ClaimNotWarranted:
				summary.ResolvedNotWarranted++
			case finding.ClaimKind != nil && *finding.ClaimKind == ClaimSuperseded:
				summary.ResolvedSuperseded++
			default:
				// A claimed thread always carries a claim kind (the claim write
				// path requires one); treat an unexpected row as unresolved so
				// the buckets stay exhaustive rather than dropping it.
				summary.Unresolved++
			}
		default:
			summary.Unresolved++
		}
	}
	summary.DeferredToTask = len(followUps)
	return summary
}

// taskReviewFindingSelectSQL selects one review thread plus its original
// finding body — the thread's first review comment, which is the opening
// comment the reviewer filed.
const taskReviewFindingSelectSQL = `
SELECT
	t.id,
	t.change_id,
	t.file_path,
	t.line,
	t.state,
	t.claim_kind,
	t.claim_commit_sha,
	t.claimed_by,
	t.claimed_at,
	t.certified_by,
	t.certified_at,
	t.reopened_by,
	t.reopened_at,
	t.created_at,
	t.updated_at,
	COALESCE((
		SELECT c.body
		FROM review_comments c
		WHERE c.thread_id = t.id
		ORDER BY c.id
		LIMIT 1
	), '')
FROM review_threads t`

func scanTaskReviewFinding(scanner taskScanner) (TaskReviewFinding, error) {
	var finding TaskReviewFinding
	var state string
	var claimKind sql.NullString
	var claimCommitSHA sql.NullString
	var claimedBy sql.NullString
	var claimedAt sql.NullString
	var certifiedBy sql.NullString
	var certifiedAt sql.NullString
	var reopenedBy sql.NullString
	var reopenedAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&finding.ID,
		&finding.ChangeID,
		&finding.FilePath,
		&finding.Line,
		&state,
		&claimKind,
		&claimCommitSHA,
		&claimedBy,
		&claimedAt,
		&certifiedBy,
		&certifiedAt,
		&reopenedBy,
		&reopenedAt,
		&createdAt,
		&updatedAt,
		&finding.Finding,
	); err != nil {
		return TaskReviewFinding{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return TaskReviewFinding{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return TaskReviewFinding{}, err
	}
	finding.State = ReviewThreadState(state)
	finding.CreatedAt = parsedCreatedAt
	finding.UpdatedAt = parsedUpdatedAt
	if claimKind.Valid {
		value := ReviewClaimKind(claimKind.String)
		finding.ClaimKind = &value
	}
	if claimCommitSHA.Valid {
		finding.ClaimCommitSHA = &claimCommitSHA.String
	}
	if claimedBy.Valid {
		finding.ClaimedBy = &claimedBy.String
	}
	if claimedAt.Valid {
		parsed, err := parseTime(claimedAt.String)
		if err != nil {
			return TaskReviewFinding{}, err
		}
		finding.ClaimedAt = &parsed
	}
	if certifiedBy.Valid {
		finding.CertifiedBy = &certifiedBy.String
	}
	if certifiedAt.Valid {
		parsed, err := parseTime(certifiedAt.String)
		if err != nil {
			return TaskReviewFinding{}, err
		}
		finding.CertifiedAt = &parsed
	}
	if reopenedBy.Valid {
		finding.ReopenedBy = &reopenedBy.String
	}
	if reopenedAt.Valid {
		parsed, err := parseTime(reopenedAt.String)
		if err != nil {
			return TaskReviewFinding{}, err
		}
		finding.ReopenedAt = &parsed
	}
	return finding, nil
}

// taskReviewFollowUpSelectSQL selects every review follow-up action for a task
// joined to its target task's title and the related_to relation the deferral
// recorded. The relation join is a LEFT JOIN so a follow-up whose relation was
// later unlinked is still part of the registry.
const taskReviewFollowUpSelectSQL = `
SELECT
	f.action,
	f.check_name,
	f.finding_hash,
	f.task_id,
	f.created_at,
	t.title,
	r.created_at
FROM review_follow_up_actions f
JOIN tasks t ON t.id = f.task_id
LEFT JOIN work_item_relations r
	ON r.source_item_id = f.source_task_id
	AND r.target_item_id = f.task_id
	AND r.kind = ?`

func scanTaskReviewFollowUp(scanner taskScanner) (TaskReviewFollowUp, error) {
	var followUp TaskReviewFollowUp
	var createdAt string
	var relatedAt sql.NullString
	if err := scanner.Scan(
		&followUp.Action,
		&followUp.CheckName,
		&followUp.FindingHash,
		&followUp.TargetTaskID,
		&createdAt,
		&followUp.TargetTaskTitle,
		&relatedAt,
	); err != nil {
		return TaskReviewFollowUp{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return TaskReviewFollowUp{}, err
	}
	followUp.CreatedAt = parsedCreatedAt
	if relatedAt.Valid {
		parsed, err := parseTime(relatedAt.String)
		if err != nil {
			return TaskReviewFollowUp{}, err
		}
		followUp.RelatedAt = &parsed
	}
	return followUp, nil
}
