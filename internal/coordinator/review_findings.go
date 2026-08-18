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
	// Legacy marks rows written by the pre-batch singular endpoint. Those rows do
	// not carry full finding text, source-job provenance, or organizer state.
	Legacy bool   `json:"legacy"`
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

// TaskReviewFollowUpDisposition is the human-reviewed terminal mapping for one
// durable proposal occurrence. Target metadata is read live so the findings
// ledger remains useful after the materialized task moves in the work graph.
type TaskReviewFollowUpDisposition struct {
	PlanRevisionID    string    `json:"plan_revision_id"`
	SetRevision       int       `json:"set_revision"`
	Disposition       string    `json:"disposition"`
	ItemKey           string    `json:"item_key,omitempty"`
	TargetTaskID      string    `json:"target_task_id,omitempty"`
	TargetTaskTitle   string    `json:"target_task_title,omitempty"`
	TargetFeatureID   string    `json:"target_feature_id,omitempty"`
	TargetParentID    string    `json:"target_parent_id,omitempty"`
	TargetBlockerIDs  []string  `json:"target_blocker_ids,omitempty"`
	CanonicalProposal string    `json:"canonical_proposal_id,omitempty"`
	Rationale         string    `json:"rationale"`
	CreatedAt         time.Time `json:"created_at"`
}

// TaskReviewFollowUpProposal preserves the exact accepted aggregation finding
// and its suggestion. Disposition is absent while organizer review is pending.
type TaskReviewFollowUpProposal struct {
	ID                 string                         `json:"id"`
	CommentIndex       int                            `json:"comment_index"`
	FindingHash        string                         `json:"finding_hash"`
	SHA                string                         `json:"sha"`
	FilePath           string                         `json:"file_path"`
	Line               int                            `json:"line"`
	Body               string                         `json:"body"`
	Severity           string                         `json:"severity"`
	IntroducedByChange bool                           `json:"introduced_by_change"`
	Requirement        string                         `json:"requirement"`
	RequirementSource  string                         `json:"requirement_source"`
	FindingBasis       string                         `json:"finding_basis"`
	RemediationScope   string                         `json:"remediation_scope"`
	ScopeRationale     string                         `json:"scope_rationale"`
	FollowUp           string                         `json:"follow_up,omitempty"`
	SuggestedAction    string                         `json:"suggested_action"`
	SuggestedTitle     string                         `json:"suggested_title,omitempty"`
	SuggestedBody      string                         `json:"suggested_body,omitempty"`
	SuggestedTaskID    string                         `json:"suggested_task_id,omitempty"`
	State              string                         `json:"state"`
	Disposition        *TaskReviewFollowUpDisposition `json:"disposition,omitempty"`
	CreatedAt          time.Time                      `json:"created_at"`
}

// TaskReviewFollowUpBatch is one immutable accepted aggregation delivery.
type TaskReviewFollowUpBatch struct {
	ID              string                       `json:"id"`
	CheckName       string                       `json:"check_name"`
	SourceJobID     string                       `json:"source_job_id"`
	ReviewedHeadSHA string                       `json:"reviewed_head_sha"`
	ReportSHA256    string                       `json:"report_sha256"`
	State           string                       `json:"state"`
	Proposals       []TaskReviewFollowUpProposal `json:"proposals"`
	CreatedAt       time.Time                    `json:"created_at"`
}

// TaskReviewFollowUpPlan is the organizer plan bound to the set's active
// revision. Historical dispositions retain their own plan revision IDs.
type TaskReviewFollowUpPlan struct {
	ID                     string `json:"id"`
	SetRevision            int    `json:"set_revision"`
	OrganizerWorkflowRunID string `json:"organizer_workflow_run_id,omitempty"`
	ArtifactID             string `json:"artifact_id,omitempty"`
	ArtifactSHA256         string `json:"artifact_sha256,omitempty"`
	State                  string `json:"state"`
	MaterializationError   string `json:"materialization_error,omitempty"`
}

// TaskReviewFollowUpSet groups all accepted review visits for one source
// task/change/workflow lineage and carries the active organizer state.
type TaskReviewFollowUpSet struct {
	ID                   string                    `json:"id"`
	SourceChangeID       string                    `json:"source_change_id"`
	WorkflowRunID        string                    `json:"workflow_run_id"`
	Revision             int                       `json:"revision"`
	State                ReviewFollowUpSetState    `json:"state"`
	OrganizerTaskID      string                    `json:"organizer_task_id,omitempty"`
	OrganizerTaskTitle   string                    `json:"organizer_task_title,omitempty"`
	ActivePlanArtifactID string                    `json:"active_plan_artifact_id,omitempty"`
	LastError            string                    `json:"last_error,omitempty"`
	Plan                 *TaskReviewFollowUpPlan   `json:"plan,omitempty"`
	Batches              []TaskReviewFollowUpBatch `json:"batches"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
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
	TaskID       string
	Findings     []TaskReviewFinding
	FollowUps    []TaskReviewFollowUp
	FollowUpSets []TaskReviewFollowUpSet
	Summary      TaskFindingsSummary
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
		TaskID:       taskID,
		Findings:     []TaskReviewFinding{},
		FollowUps:    []TaskReviewFollowUp{},
		FollowUpSets: []TaskReviewFollowUpSet{},
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

	registry.FollowUpSets, err = s.taskReviewFollowUpSets(ctx, taskID)
	if err != nil {
		return TaskFindingsRegistry{}, err
	}

	registry.Summary = summarizeTaskFindings(registry.Findings, registry.FollowUps, registry.FollowUpSets)
	return registry, nil
}

// summarizeTaskFindings buckets every finding exactly once. Certified threads
// are their own bucket; claimed threads split by claim kind; open and reopened
// threads are unresolved; deferred-to-task counts the follow-up actions.
func summarizeTaskFindings(findings []TaskReviewFinding, followUps []TaskReviewFollowUp, followUpSets []TaskReviewFollowUpSet) TaskFindingsSummary {
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
	for _, set := range followUpSets {
		for _, batch := range set.Batches {
			for _, proposal := range batch.Proposals {
				if proposal.Disposition != nil && proposal.Disposition.Disposition != string(ReviewFollowUpDispositionCoveredBySource) {
					summary.DeferredToTask++
				}
			}
		}
	}
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
	ON r.kind = ?
	AND ((r.source_item_id = f.source_task_id AND r.target_item_id = f.task_id)
		OR (r.source_item_id = f.task_id AND r.target_item_id = f.source_task_id))`

func scanTaskReviewFollowUp(scanner taskScanner) (TaskReviewFollowUp, error) {
	var followUp TaskReviewFollowUp
	followUp.Legacy = true
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

func (s *ThreadService) taskReviewFollowUpSets(ctx context.Context, taskID string) ([]TaskReviewFollowUpSet, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT s.id, s.source_change_id, s.workflow_run_id, s.revision, s.state,
       COALESCE(s.organizer_task_id,''), COALESCE(ot.title,''),
       COALESCE(s.active_plan_artifact_id,''), s.last_error,
       COALESCE(pr.id,''), COALESCE(pr.set_revision,0),
       COALESCE(pr.organizer_workflow_run_id,''), COALESCE(pr.plan_artifact_id,''),
       COALESCE(pr.plan_sha256,''), COALESCE(pr.state,''), COALESCE(pr.materialization_error,''),
       s.created_at, s.updated_at
FROM review_follow_up_sets s
LEFT JOIN tasks ot ON ot.id = s.organizer_task_id
LEFT JOIN review_follow_up_plan_revisions pr ON pr.set_id = s.id AND pr.set_revision = s.revision
WHERE s.source_task_id = ?
ORDER BY s.created_at, s.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review follow-up sets: %w", err)
	}
	sets := []TaskReviewFollowUpSet{}
	setIndexes := map[string]int{}
	for rows.Next() {
		var set TaskReviewFollowUpSet
		var state, createdAt, updatedAt string
		var plan TaskReviewFollowUpPlan
		if err := rows.Scan(
			&set.ID, &set.SourceChangeID, &set.WorkflowRunID, &set.Revision, &state,
			&set.OrganizerTaskID, &set.OrganizerTaskTitle, &set.ActivePlanArtifactID, &set.LastError,
			&plan.ID, &plan.SetRevision, &plan.OrganizerWorkflowRunID, &plan.ArtifactID,
			&plan.ArtifactSHA256, &plan.State, &plan.MaterializationError, &createdAt, &updatedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan review follow-up set: %w", err)
		}
		set.State = ReviewFollowUpSetState(state)
		set.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			rows.Close()
			return nil, err
		}
		set.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if plan.ID != "" {
			set.Plan = &plan
		}
		set.Batches = []TaskReviewFollowUpBatch{}
		setIndexes[set.ID] = len(sets)
		sets = append(sets, set)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate review follow-up sets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close review follow-up set rows: %w", err)
	}
	if len(sets) == 0 {
		return sets, nil
	}

	batchRows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.set_id, b.check_name, b.source_job_id, b.reviewed_head_sha,
       b.report_sha256, b.state, b.created_at
FROM review_follow_up_batches b
WHERE b.source_task_id = ?
ORDER BY b.created_at, b.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review follow-up batches: %w", err)
	}
	batchLocations := map[string][2]int{}
	for batchRows.Next() {
		var batch TaskReviewFollowUpBatch
		var setID, createdAt string
		if err := batchRows.Scan(&batch.ID, &setID, &batch.CheckName, &batch.SourceJobID,
			&batch.ReviewedHeadSHA, &batch.ReportSHA256, &batch.State, &createdAt); err != nil {
			batchRows.Close()
			return nil, fmt.Errorf("scan review follow-up batch: %w", err)
		}
		batch.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			batchRows.Close()
			return nil, err
		}
		batch.Proposals = []TaskReviewFollowUpProposal{}
		setIndex, ok := setIndexes[setID]
		if !ok {
			batchRows.Close()
			return nil, fmt.Errorf("review follow-up batch %s references unprojected set %s", batch.ID, setID)
		}
		batchIndex := len(sets[setIndex].Batches)
		sets[setIndex].Batches = append(sets[setIndex].Batches, batch)
		batchLocations[batch.ID] = [2]int{setIndex, batchIndex}
	}
	if err := batchRows.Err(); err != nil {
		batchRows.Close()
		return nil, fmt.Errorf("iterate review follow-up batches: %w", err)
	}
	if err := batchRows.Close(); err != nil {
		return nil, fmt.Errorf("close review follow-up batch rows: %w", err)
	}

	proposalRows, err := s.db.QueryContext(ctx, `
SELECT p.id, p.batch_id, p.comment_index, p.finding_hash, p.sha, p.file_path, p.line,
       p.body, p.severity, p.introduced_by_change, p.requirement, p.requirement_source,
       p.finding_basis, p.remediation_scope, p.scope_rationale, p.follow_up,
       p.suggested_action, p.suggested_title, p.suggested_body, p.suggested_task_id,
       p.state, p.created_at,
       COALESCE(d.plan_revision_id,''), COALESCE(pr.set_revision,0), COALESCE(d.disposition,''),
       COALESCE(d.item_key,''), COALESCE(d.target_task_id,''), COALESCE(tt.title,''),
       COALESCE(tt.feature_id,''),
       COALESCE((SELECT r.source_item_id FROM work_item_relations r WHERE r.kind = 'parent_of' AND r.target_item_id = d.target_task_id LIMIT 1), ''),
       COALESCE(d.canonical_proposal_id,''), COALESCE(d.rationale,''), COALESCE(d.created_at,'')
FROM review_follow_up_proposals p
JOIN review_follow_up_batches b ON b.id = p.batch_id
LEFT JOIN review_follow_up_dispositions d ON d.proposal_id = p.id
LEFT JOIN review_follow_up_plan_revisions pr ON pr.id = d.plan_revision_id
LEFT JOIN tasks tt ON tt.id = d.target_task_id
WHERE b.source_task_id = ?
ORDER BY b.created_at, b.id, p.comment_index`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review follow-up proposals: %w", err)
	}
	for proposalRows.Next() {
		var proposal TaskReviewFollowUpProposal
		var batchID, createdAt string
		var disposition TaskReviewFollowUpDisposition
		var dispositionCreatedAt string
		if err := proposalRows.Scan(
			&proposal.ID, &batchID, &proposal.CommentIndex, &proposal.FindingHash, &proposal.SHA,
			&proposal.FilePath, &proposal.Line, &proposal.Body, &proposal.Severity,
			&proposal.IntroducedByChange, &proposal.Requirement, &proposal.RequirementSource,
			&proposal.FindingBasis, &proposal.RemediationScope, &proposal.ScopeRationale,
			&proposal.FollowUp, &proposal.SuggestedAction, &proposal.SuggestedTitle,
			&proposal.SuggestedBody, &proposal.SuggestedTaskID, &proposal.State, &createdAt,
			&disposition.PlanRevisionID, &disposition.SetRevision, &disposition.Disposition,
			&disposition.ItemKey, &disposition.TargetTaskID, &disposition.TargetTaskTitle,
			&disposition.TargetFeatureID, &disposition.TargetParentID,
			&disposition.CanonicalProposal, &disposition.Rationale, &dispositionCreatedAt,
		); err != nil {
			proposalRows.Close()
			return nil, fmt.Errorf("scan review follow-up proposal: %w", err)
		}
		proposal.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			proposalRows.Close()
			return nil, err
		}
		if disposition.PlanRevisionID != "" {
			disposition.CreatedAt, err = parseTime(dispositionCreatedAt)
			if err != nil {
				proposalRows.Close()
				return nil, err
			}
			proposal.Disposition = &disposition
		}
		location, ok := batchLocations[batchID]
		if !ok {
			proposalRows.Close()
			return nil, fmt.Errorf("review follow-up proposal %s references unprojected batch %s", proposal.ID, batchID)
		}
		batch := &sets[location[0]].Batches[location[1]]
		batch.Proposals = append(batch.Proposals, proposal)
	}
	if err := proposalRows.Err(); err != nil {
		proposalRows.Close()
		return nil, fmt.Errorf("iterate review follow-up proposals: %w", err)
	}
	if err := proposalRows.Close(); err != nil {
		return nil, fmt.Errorf("close review follow-up proposal rows: %w", err)
	}

	blockerRows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT d.target_task_id, r.source_item_id
FROM review_follow_up_dispositions d
JOIN review_follow_up_proposals p ON p.id = d.proposal_id
JOIN review_follow_up_batches b ON b.id = p.batch_id
JOIN work_item_relations r ON r.target_item_id = d.target_task_id AND r.kind = 'blocks'
WHERE b.source_task_id = ?
ORDER BY d.target_task_id, r.source_item_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review follow-up target blockers: %w", err)
	}
	blockersByTarget := map[string][]string{}
	for blockerRows.Next() {
		var targetID, blockerID string
		if err := blockerRows.Scan(&targetID, &blockerID); err != nil {
			blockerRows.Close()
			return nil, fmt.Errorf("scan review follow-up target blocker: %w", err)
		}
		blockersByTarget[targetID] = append(blockersByTarget[targetID], blockerID)
	}
	if err := blockerRows.Err(); err != nil {
		blockerRows.Close()
		return nil, fmt.Errorf("iterate review follow-up target blockers: %w", err)
	}
	if err := blockerRows.Close(); err != nil {
		return nil, fmt.Errorf("close review follow-up target blocker rows: %w", err)
	}
	for setIndex := range sets {
		for batchIndex := range sets[setIndex].Batches {
			for proposalIndex := range sets[setIndex].Batches[batchIndex].Proposals {
				disposition := sets[setIndex].Batches[batchIndex].Proposals[proposalIndex].Disposition
				if disposition != nil {
					disposition.TargetBlockerIDs = append([]string(nil), blockersByTarget[disposition.TargetTaskID]...)
				}
			}
		}
	}
	return sets, nil
}
