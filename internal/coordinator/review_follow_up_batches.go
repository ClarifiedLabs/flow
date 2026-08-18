package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrReviewFollowUpBatchForbidden = errors.New("review follow-up batch is not authorized")
	ErrReviewFollowUpBatchConflict  = errors.New("review follow-up batch conflicts with durable state")
)

type ReviewFollowUpSetState string

const (
	ReviewFollowUpSetOpen             ReviewFollowUpSetState = "open"
	ReviewFollowUpSetOrganizerPending ReviewFollowUpSetState = "organizer_pending"
	ReviewFollowUpSetOrganizing       ReviewFollowUpSetState = "organizing"
	ReviewFollowUpSetAwaitingReview   ReviewFollowUpSetState = "awaiting_review"
	ReviewFollowUpSetMaterializing    ReviewFollowUpSetState = "materializing"
	ReviewFollowUpSetMaterialized     ReviewFollowUpSetState = "materialized"
	ReviewFollowUpSetAttention        ReviewFollowUpSetState = "attention"
	ReviewFollowUpSetClosed           ReviewFollowUpSetState = "closed"
)

type ApplyReviewFollowUpBatchInput struct {
	SourceTaskID string
	LeaseID      string
	WorkerID     string
	ReportJSON   []byte
	ReportSHA256 string
}

type ApplyReviewFollowUpBatchResult struct {
	Accepted      bool                   `json:"accepted"`
	BatchID       string                 `json:"batch_id,omitempty"`
	SetID         string                 `json:"set_id,omitempty"`
	SetRevision   int                    `json:"set_revision,omitempty"`
	SetState      ReviewFollowUpSetState `json:"set_state,omitempty"`
	ProposalCount int                    `json:"proposal_count"`
	Replayed      bool                   `json:"replayed"`
}

// ApplyReviewFollowUpBatch authenticates and stores one complete final review
// aggregation report in the same BEGIN IMMEDIATE transaction. Provenance comes
// exclusively from the lease-bound job; callers provide only the reviewed task,
// exact sealed report bytes, and their digest.
func (s *TaskService) ApplyReviewFollowUpBatch(ctx context.Context, input ApplyReviewFollowUpBatchInput) (result ApplyReviewFollowUpBatchResult, err error) {
	defer func() {
		if s.metrics == nil {
			return
		}
		switch {
		case err != nil:
			s.metrics.ReviewFollowUpBatches.Inc(map[string]string{"outcome": "rejected"})
		case result.Accepted && result.Replayed:
			s.metrics.ReviewFollowUpBatches.Inc(map[string]string{"outcome": "replayed"})
		case result.Accepted:
			s.metrics.ReviewFollowUpBatches.Inc(map[string]string{"outcome": "accepted"})
			s.metrics.ReviewFollowUpProposals.Add(float64(result.ProposalCount), nil)
		}
	}()
	input.SourceTaskID = strings.TrimSpace(input.SourceTaskID)
	input.LeaseID = strings.TrimSpace(input.LeaseID)
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	input.ReportSHA256 = strings.TrimSpace(input.ReportSHA256)
	if input.SourceTaskID == "" || input.LeaseID == "" || input.WorkerID == "" {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: task, lease, and worker are required", ErrReviewFollowUpBatchForbidden)
	}
	if len(input.ReportJSON) == 0 {
		return ApplyReviewFollowUpBatchResult{}, errors.New("review follow-up report_json is required")
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	defer tx.Rollback()

	actualDigest := sha256.Sum256(input.ReportJSON)
	actualDigestText := hex.EncodeToString(actualDigest[:])
	if len(input.ReportSHA256) != sha256.Size*2 || input.ReportSHA256 != actualDigestText {
		return ApplyReviewFollowUpBatchResult{}, errors.New("review follow-up report_sha256 does not match report_json")
	}
	validated, err := checkverdict.Validate(input.ReportJSON, checkverdict.ModeReviewAggregation)
	if err != nil {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("validate review aggregation report: %w", err)
	}
	if validated.DecisionRequest != nil {
		return ApplyReviewFollowUpBatchResult{}, errors.New("review aggregation decision_request report cannot create a follow-up batch")
	}

	var sourceJobID, leaseWorkerID, leaseExpires string
	var leaseReleased sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT l.job_id, l.worker_id, l.expires_at, l.released_at
FROM leases l WHERE l.id = ?`, input.LeaseID).Scan(&sourceJobID, &leaseWorkerID, &leaseExpires, &leaseReleased); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: lease not found", ErrReviewFollowUpBatchForbidden)
		}
		return ApplyReviewFollowUpBatchResult{}, err
	}
	expiresAt, err := sqlitex.ParseTime(leaseExpires)
	if err != nil || leaseWorkerID != input.WorkerID || leaseReleased.Valid || !s.now().UTC().Before(expiresAt) {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: worker does not own a live aggregation lease", ErrReviewFollowUpBatchForbidden)
	}

	var jobTaskID, sourceChangeID, workflowRunID, nodeRunID, role, jobState, payloadJSON string
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(task_id,''), COALESCE(change_id,''), COALESCE(workflow_run_id,''),
       COALESCE(node_run_id,''), role, state, payload_json
FROM jobs WHERE id = ?`, sourceJobID).Scan(
		&jobTaskID, &sourceChangeID, &workflowRunID, &nodeRunID, &role, &jobState, &payloadJSON,
	); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("decode aggregation job payload: %w", err)
	}
	aggregation, _ := payload["review_aggregation"].(bool)
	checkName, _ := payload["check_name"].(string)
	reviewedHead, _ := payload["head_sha"].(string)
	checkName = strings.TrimSpace(checkName)
	reviewedHead = strings.TrimSpace(reviewedHead)
	if jobTaskID != input.SourceTaskID || sourceChangeID == "" || workflowRunID == "" || nodeRunID == "" ||
		role != "reviewer" || (jobState != "claimed" && jobState != "running") || !aggregation ||
		!strings.HasPrefix(checkName, ReviewAggregationCheckName+".node.") {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: source job is not the live final aggregation job", ErrReviewFollowUpBatchForbidden)
	}

	var runTaskID, runState, currentNodeRunID string
	if err := tx.QueryRowContext(ctx, `
SELECT task_id, state, COALESCE(current_node_run_id,'') FROM workflow_runs WHERE id = ?`, workflowRunID).
		Scan(&runTaskID, &runState, &currentNodeRunID); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	var nodeWorkflowRunID, nodeState string
	if err := tx.QueryRowContext(ctx, `SELECT workflow_run_id, state FROM workflow_node_runs WHERE id = ?`, nodeRunID).
		Scan(&nodeWorkflowRunID, &nodeState); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	if runTaskID != input.SourceTaskID || runState != string(WorkflowRunRunning) || currentNodeRunID != nodeRunID ||
		nodeWorkflowRunID != workflowRunID || nodeState != string(WorkflowNodeRunning) {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: aggregation workflow node is no longer active", ErrReviewFollowUpBatchConflict)
	}

	var changeTaskID, changeWorkflowRunID, currentHead string
	if err := tx.QueryRowContext(ctx, `
SELECT task_id, COALESCE(workflow_run_id,''), head_sha FROM changes WHERE id = ?`, sourceChangeID).
		Scan(&changeTaskID, &changeWorkflowRunID, &currentHead); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	if changeTaskID != input.SourceTaskID || changeWorkflowRunID != workflowRunID || reviewedHead == "" || reviewedHead != strings.TrimSpace(currentHead) {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: aggregation source head or lineage is stale", ErrReviewFollowUpBatchConflict)
	}
	var currentArtifactID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_artifact_id FROM workflow_runs WHERE id = ?`, workflowRunID).Scan(&currentArtifactID); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	if currentArtifactID.Valid {
		if err := verifyPinnedChangeHeadTx(ctx, tx, currentArtifactID.String); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
	}

	var checkVerdict string
	var checkSourceJob sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT verdict, source_job_id FROM checks WHERE task_id = ? AND name = ?`, input.SourceTaskID, checkName).
		Scan(&checkVerdict, &checkSourceJob); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	if checkVerdict != string(CheckPending) || !checkSourceJob.Valid || checkSourceJob.String != sourceJobID {
		return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: aggregation check is no longer pending for this job", ErrReviewFollowUpBatchConflict)
	}

	var replayBatchID, replaySetID, storedDigest, setState string
	var replayRevision, replayCount int
	replayErr := tx.QueryRowContext(ctx, `
SELECT b.id, b.set_id, b.report_sha256, s.revision, s.state,
       (SELECT COUNT(*) FROM review_follow_up_proposals p WHERE p.batch_id = b.id)
FROM review_follow_up_batches b
JOIN review_follow_up_sets s ON s.id = b.set_id
WHERE b.source_job_id = ?`, sourceJobID).Scan(
		&replayBatchID, &replaySetID, &storedDigest, &replayRevision, &setState, &replayCount,
	)
	if replayErr == nil {
		if storedDigest != input.ReportSHA256 {
			return ApplyReviewFollowUpBatchResult{}, fmt.Errorf("%w: source job was already recorded with a different report digest", ErrReviewFollowUpBatchConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
		return ApplyReviewFollowUpBatchResult{
			Accepted: true, BatchID: replayBatchID, SetID: replaySetID, SetRevision: replayRevision,
			SetState: ReviewFollowUpSetState(setState), ProposalCount: replayCount, Replayed: true,
		}, nil
	}
	if !errors.Is(replayErr, sql.ErrNoRows) {
		return ApplyReviewFollowUpBatchResult{}, replayErr
	}

	proposalIndexes := make([]int, 0, len(validated.Comments))
	for index, comment := range validated.Comments {
		if comment.TaskAction != nil {
			proposalIndexes = append(proposalIndexes, index)
		}
	}
	if len(proposalIndexes) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
		return ApplyReviewFollowUpBatchResult{}, nil
	}

	nowText := sqlitex.FormatTime(s.now().UTC())
	var setID, existingSetState string
	var setRevision int
	err = tx.QueryRowContext(ctx, `
SELECT id, revision, state FROM review_follow_up_sets
WHERE source_task_id = ? AND source_change_id = ? AND workflow_run_id = ?`,
		input.SourceTaskID, sourceChangeID, workflowRunID).Scan(&setID, &setRevision, &existingSetState)
	if errors.Is(err, sql.ErrNoRows) {
		setID, err = randomPrefixedID("rfus")
		if err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
		setRevision = 1
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_sets (
    id, source_task_id, source_change_id, workflow_run_id, revision, state, created_at, updated_at
) VALUES (?, ?, ?, ?, 1, 'open', ?, ?)`, setID, input.SourceTaskID, sourceChangeID, workflowRunID, nowText, nowText); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
	} else if err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	} else {
		setRevision++
		if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET revision = ?, state = 'open', organizer_task_id = NULL, active_plan_artifact_id = NULL,
    last_error = '', updated_at = ?
WHERE id = ?`, setRevision, nowText, setID); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
	}

	batchID, err := randomPrefixedID("rfub")
	if err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_batches (
    id, set_id, source_task_id, source_change_id, workflow_run_id, node_run_id,
    check_name, source_job_id, reviewed_head_sha, report_sha256, report_json,
    state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'accepted', ?, ?)`,
		batchID, setID, input.SourceTaskID, sourceChangeID, workflowRunID, nodeRunID,
		checkName, sourceJobID, reviewedHead, input.ReportSHA256, string(input.ReportJSON), nowText, nowText,
	); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	for _, index := range proposalIndexes {
		comment := validated.Comments[index]
		proposalID, err := randomPrefixedID("rfp")
		if err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
		findingHash, err := reviewFollowUpProposalHash(comment)
		if err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
		introduced := comment.IntroducedByChange != nil && *comment.IntroducedByChange
		action := comment.TaskAction
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_proposals (
    id, batch_id, comment_index, finding_hash, sha, file_path, line, body, severity,
    introduced_by_change, requirement, requirement_source, finding_basis, remediation_scope,
    scope_rationale, follow_up, suggested_action, suggested_title, suggested_body,
    suggested_task_id, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
			proposalID, batchID, index, findingHash, comment.SHA, comment.File, comment.Line,
			comment.Body, comment.Severity, introduced, comment.Requirement, comment.RequirementSource,
			comment.FindingBasis, comment.RemediationScope, comment.ScopeRationale, comment.FollowUp,
			action.Action, action.Title, action.Body, action.TaskID, nowText, nowText,
		); err != nil {
			return ApplyReviewFollowUpBatchResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyReviewFollowUpBatchResult{}, err
	}
	return ApplyReviewFollowUpBatchResult{
		Accepted: true, BatchID: batchID, SetID: setID, SetRevision: setRevision,
		SetState: ReviewFollowUpSetOpen, ProposalCount: len(proposalIndexes),
	}, nil
}

func reviewFollowUpProposalHash(comment checkverdict.ReviewCommentReport) (string, error) {
	introduced := comment.IntroducedByChange != nil && *comment.IntroducedByChange
	return reviewFollowUpHash(struct {
		SHA                string `json:"sha"`
		File               string `json:"file"`
		Line               int    `json:"line"`
		Body               string `json:"body"`
		Severity           string `json:"severity"`
		IntroducedByChange bool   `json:"introduced_by_change"`
		Requirement        string `json:"requirement"`
		RequirementSource  string `json:"requirement_source"`
		FindingBasis       string `json:"finding_basis"`
		RemediationScope   string `json:"remediation_scope"`
		ScopeRationale     string `json:"scope_rationale"`
	}{
		SHA: comment.SHA, File: comment.File, Line: comment.Line, Body: comment.Body,
		Severity: comment.Severity, IntroducedByChange: introduced, Requirement: comment.Requirement,
		RequirementSource: comment.RequirementSource, FindingBasis: comment.FindingBasis,
		RemediationScope: comment.RemediationScope, ScopeRationale: comment.ScopeRationale,
	})
}
