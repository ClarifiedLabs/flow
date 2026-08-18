package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type reviewFollowUpOrganizerSet struct {
	ID             string
	SourceTaskID   string
	SourceChangeID string
	WorkflowRunID  string
	Revision       int
}

type reviewFollowUpOrganizerContext struct {
	SetID             string                            `json:"set_id"`
	SetRevision       int                               `json:"set_revision"`
	SourceTaskID      string                            `json:"source_task_id"`
	SourceChangeID    string                            `json:"source_change_id"`
	SourceWorkflowRun string                            `json:"source_workflow_run_id"`
	Proposals         []reviewFollowUpOrganizerProposal `json:"active_proposals"`
	PriorDispositions []reviewFollowUpPriorDisposition  `json:"prior_dispositions,omitempty"`
	CandidateTasks    []reviewFollowUpCandidateTask     `json:"candidate_open_tasks,omitempty"`
	OwnerRulings      []OwnerRuling                     `json:"active_owner_rulings,omitempty"`
}

type reviewFollowUpOrganizerProposal struct {
	ID                    string `json:"proposal_id"`
	BatchID               string `json:"batch_id"`
	VisitOrder            int    `json:"review_visit_order"`
	SourceJobID           string `json:"source_job_id"`
	CheckName             string `json:"check_name"`
	ReviewedHeadSHA       string `json:"reviewed_head_sha"`
	CommentIndex          int    `json:"comment_index"`
	FindingHash           string `json:"finding_hash"`
	SHA                   string `json:"sha"`
	File                  string `json:"file"`
	Line                  int    `json:"line"`
	Body                  string `json:"body"`
	Severity              string `json:"severity"`
	IntroducedByChange    bool   `json:"introduced_by_change"`
	Requirement           string `json:"requirement"`
	RequirementSource     string `json:"requirement_source"`
	FindingBasis          string `json:"finding_basis"`
	RemediationScope      string `json:"remediation_scope"`
	ScopeRationale        string `json:"scope_rationale"`
	FollowUp              string `json:"follow_up,omitempty"`
	SuggestedAction       string `json:"suggested_action"`
	SuggestedTitle        string `json:"suggested_title,omitempty"`
	SuggestedBody         string `json:"suggested_body,omitempty"`
	SuggestedExistingTask string `json:"suggested_task_id,omitempty"`
}

type reviewFollowUpPriorDisposition struct {
	ProposalID          string `json:"proposal_id"`
	SetRevision         int    `json:"set_revision"`
	Disposition         string `json:"disposition"`
	ItemKey             string `json:"item_key,omitempty"`
	TargetTaskID        string `json:"target_task_id,omitempty"`
	CanonicalProposalID string `json:"canonical_proposal_id,omitempty"`
	Rationale           string `json:"rationale"`
}

type reviewFollowUpCandidateTask struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	State     string `json:"state"`
	CreatedBy string `json:"created_by"`
	FeatureID string `json:"feature_id,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Blockers  string `json:"blockers,omitempty"`
}

// MarkReviewFollowUpOrganizerPending is called only after the approved review
// node transition has committed. It never creates work synchronously.
func (s *TaskService) MarkReviewFollowUpOrganizerPending(ctx context.Context, taskID, changeID, workflowRunID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET state = 'organizer_pending', last_error = '', updated_at = ?
WHERE source_task_id = ? AND source_change_id = ? AND workflow_run_id = ? AND state = 'open'`,
		formatTime(s.now().UTC()), strings.TrimSpace(taskID), strings.TrimSpace(changeID), strings.TrimSpace(workflowRunID))
	return err
}

// ReconcileReviewFollowUpOrganizers creates and schedules organizer planning
// tasks after source review approval. Each plan revision and task are durable;
// failures move only the follow-up set to attention.
func (e *WorkflowExecutor) ReconcileReviewFollowUpOrganizers(ctx context.Context) error {
	nowText := formatTime(e.tasks.now().UTC())
	// Heal a crash between the approved node transition and the post-transition
	// pending marker.
	if _, err := e.db.ExecContext(ctx, `
UPDATE review_follow_up_sets AS s
SET state = 'organizer_pending', last_error = '', updated_at = ?
WHERE s.state = 'open' AND EXISTS (
    SELECT 1 FROM review_follow_up_batches b
    JOIN workflow_node_runs nr ON nr.id = b.node_run_id
    WHERE b.set_id = s.id AND nr.state = 'succeeded' AND nr.outcome = 'approved'
)`, nowText); err != nil {
		return err
	}
	rows, err := e.db.QueryContext(ctx, `
SELECT id, source_task_id, source_change_id, workflow_run_id, revision
FROM review_follow_up_sets
WHERE state = 'organizer_pending'
ORDER BY updated_at, id`)
	if err != nil {
		return err
	}
	var sets []reviewFollowUpOrganizerSet
	for rows.Next() {
		var set reviewFollowUpOrganizerSet
		if err := rows.Scan(&set.ID, &set.SourceTaskID, &set.SourceChangeID, &set.WorkflowRunID, &set.Revision); err != nil {
			rows.Close()
			return err
		}
		sets = append(sets, set)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var errs error
	for _, set := range sets {
		if err := e.reconcileReviewFollowUpOrganizer(ctx, set); err != nil {
			errs = errors.Join(errs, fmt.Errorf("reconcile follow-up set %s revision %d: %w", set.ID, set.Revision, err))
		}
	}
	return errs
}

func (e *WorkflowExecutor) reconcileReviewFollowUpOrganizer(ctx context.Context, set reviewFollowUpOrganizerSet) error {
	organizerFlow, err := e.runs.flows.GetByName(ctx, ReviewFollowUpOrganizerFlowName)
	if err != nil {
		return e.markReviewFollowUpOrganizerAttention(ctx, set.ID, set.Revision, "load organizer flow: "+err.Error())
	}
	planID, organizerTaskID, err := e.reserveReviewFollowUpPlanRevision(ctx, set)
	if err != nil {
		return err
	}
	if organizerTaskID == "" {
		body, err := e.reviewFollowUpOrganizerTaskBody(ctx, set)
		if err != nil {
			return e.markReviewFollowUpOrganizerAttention(ctx, set.ID, set.Revision, "build organizer context: "+err.Error())
		}
		title := fmt.Sprintf("Organize review follow-ups for %s (revision %d)", set.SourceTaskID, set.Revision)
		// Recover a task created immediately before a coordinator crash but not yet
		// attached to the reserved plan row.
		err = e.db.QueryRowContext(ctx, `
SELECT id FROM tasks
WHERE source_task_id = ? AND source_change_id = ? AND flow_id = ? AND title = ?
  AND created_by = 'system'
ORDER BY created_at DESC LIMIT 1`, set.SourceTaskID, set.SourceChangeID, organizerFlow.ID, title).Scan(&organizerTaskID)
		if errors.Is(err, sql.ErrNoRows) {
			sourceTaskID, sourceChangeID := set.SourceTaskID, set.SourceChangeID
			task, createErr := e.tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
				Task: CreateTaskInput{
					Title: title, Body: body, FlowID: organizerFlow.ID, CreatedBy: ActorSystem,
					SourceTaskID: &sourceTaskID, SourceChangeID: &sourceChangeID,
				},
				Relations: []CreateTaskRelationInput{{
					SourceTaskID: set.SourceTaskID, Kind: RelationRelatedTo, CreatedBy: ActorSystem, BlankTargetIsNewTask: true,
				}},
			})
			if createErr != nil {
				return e.markReviewFollowUpOrganizerAttention(ctx, set.ID, set.Revision, "create organizer task: "+createErr.Error())
			}
			organizerTaskID = task.ID
		} else if err != nil {
			return err
		}
		if _, err := e.db.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET organizer_task_id = ?, updated_at = ?
WHERE id = ? AND organizer_task_id IS NULL`, organizerTaskID, formatTime(e.tasks.now().UTC()), planID); err != nil {
			return err
		}
	}

	var currentRevision int
	var currentState string
	if err := e.db.QueryRowContext(ctx, `SELECT revision, state FROM review_follow_up_sets WHERE id = ?`, set.ID).
		Scan(&currentRevision, &currentState); err != nil {
		return err
	}
	if currentRevision != set.Revision || currentState != string(ReviewFollowUpSetOrganizerPending) {
		_, _ = e.db.ExecContext(ctx, `UPDATE review_follow_up_plan_revisions SET state = 'stale', updated_at = ? WHERE id = ?`, formatTime(e.tasks.now().UTC()), planID)
		return nil
	}

	run, active, err := e.runs.ActiveForTask(ctx, organizerTaskID)
	if err != nil {
		return e.markReviewFollowUpOrganizerAttention(ctx, set.ID, set.Revision, "load organizer workflow: "+err.Error())
	}
	if !active {
		run, err = e.runs.ScheduleAs(ctx, organizerTaskID, ActorSystem)
		if err != nil {
			return e.markReviewFollowUpOrganizerAttention(ctx, set.ID, set.Revision, "schedule organizer workflow: "+err.Error())
		}
	}
	tx, err := sqlitex.BeginImmediate(ctx, e.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowText := formatTime(e.tasks.now().UTC())
	result, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET organizer_task_id = ?, state = 'organizing', last_error = '', updated_at = ?
WHERE id = ? AND revision = ? AND state = 'organizer_pending'`, organizerTaskID, nowText, set.ID, set.Revision)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		_, _ = tx.ExecContext(ctx, `UPDATE review_follow_up_plan_revisions SET state = 'stale', updated_at = ? WHERE id = ?`, nowText, planID)
		return tx.Commit(ctx)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET organizer_task_id = ?, organizer_workflow_run_id = ?, state = 'organizing', updated_at = ?
WHERE id = ?`, organizerTaskID, run.ID, nowText, planID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *WorkflowExecutor) reserveReviewFollowUpPlanRevision(ctx context.Context, set reviewFollowUpOrganizerSet) (string, string, error) {
	tx, err := sqlitex.BeginImmediate(ctx, e.db)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var planID, taskID string
	err = tx.QueryRowContext(ctx, `
SELECT id, COALESCE(organizer_task_id, '')
FROM review_follow_up_plan_revisions WHERE set_id = ? AND set_revision = ?`, set.ID, set.Revision).Scan(&planID, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		planID, err = randomPrefixedID("rfpr")
		if err != nil {
			return "", "", err
		}
		nowText := formatTime(e.tasks.now().UTC())
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_plan_revisions (id, set_id, set_revision, state, created_at, updated_at)
VALUES (?, ?, ?, 'pending', ?, ?)`, planID, set.ID, set.Revision, nowText, nowText); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return planID, taskID, nil
}

func (e *WorkflowExecutor) reviewFollowUpOrganizerTaskBody(ctx context.Context, set reviewFollowUpOrganizerSet) (string, error) {
	organizerContext := reviewFollowUpOrganizerContext{
		SetID: set.ID, SetRevision: set.Revision, SourceTaskID: set.SourceTaskID,
		SourceChangeID: set.SourceChangeID, SourceWorkflowRun: set.WorkflowRunID,
	}
	rows, err := e.db.QueryContext(ctx, `
SELECT p.id, p.batch_id,
       (SELECT COUNT(*) FROM review_follow_up_batches earlier WHERE earlier.set_id = b.set_id AND (earlier.created_at < b.created_at OR (earlier.created_at = b.created_at AND earlier.id <= b.id))),
       b.source_job_id, b.check_name, b.reviewed_head_sha, p.comment_index, p.finding_hash,
       p.sha, p.file_path, p.line, p.body, p.severity, p.introduced_by_change,
       p.requirement, p.requirement_source, p.finding_basis, p.remediation_scope,
       p.scope_rationale, p.follow_up, p.suggested_action, p.suggested_title,
       p.suggested_body, p.suggested_task_id
FROM review_follow_up_proposals p
JOIN review_follow_up_batches b ON b.id = p.batch_id
WHERE b.set_id = ? AND b.state = 'accepted' AND p.state = 'active'
ORDER BY b.created_at, b.id, p.comment_index`, set.ID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var proposal reviewFollowUpOrganizerProposal
		if err := rows.Scan(
			&proposal.ID, &proposal.BatchID, &proposal.VisitOrder, &proposal.SourceJobID,
			&proposal.CheckName, &proposal.ReviewedHeadSHA, &proposal.CommentIndex, &proposal.FindingHash,
			&proposal.SHA, &proposal.File, &proposal.Line, &proposal.Body, &proposal.Severity,
			&proposal.IntroducedByChange, &proposal.Requirement, &proposal.RequirementSource,
			&proposal.FindingBasis, &proposal.RemediationScope, &proposal.ScopeRationale,
			&proposal.FollowUp, &proposal.SuggestedAction, &proposal.SuggestedTitle,
			&proposal.SuggestedBody, &proposal.SuggestedExistingTask,
		); err != nil {
			rows.Close()
			return "", err
		}
		organizerContext.Proposals = append(organizerContext.Proposals, proposal)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}

	rows, err = e.db.QueryContext(ctx, `
SELECT d.proposal_id, pr.set_revision, d.disposition, COALESCE(d.item_key,''),
       COALESCE(d.target_task_id,''), COALESCE(d.canonical_proposal_id,''), d.rationale
FROM review_follow_up_dispositions d
JOIN review_follow_up_plan_revisions pr ON pr.id = d.plan_revision_id
WHERE pr.set_id = ? ORDER BY pr.set_revision, d.proposal_id`, set.ID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var disposition reviewFollowUpPriorDisposition
		if err := rows.Scan(&disposition.ProposalID, &disposition.SetRevision, &disposition.Disposition,
			&disposition.ItemKey, &disposition.TargetTaskID, &disposition.CanonicalProposalID, &disposition.Rationale); err != nil {
			rows.Close()
			return "", err
		}
		organizerContext.PriorDispositions = append(organizerContext.PriorDispositions, disposition)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}

	rows, err = e.db.QueryContext(ctx, `
SELECT t.id, t.title, COALESCE(t.lifecycle_state,'unscheduled'), t.created_by,
       COALESCE(t.feature_id,''),
       COALESCE((SELECT r.source_item_id FROM work_item_relations r WHERE r.kind = 'parent_of' AND r.target_item_id = t.id LIMIT 1), ''),
       COALESCE((SELECT c.branch FROM changes c WHERE c.task_id = t.id ORDER BY c.created_at DESC LIMIT 1), ''),
       COALESCE((SELECT group_concat(r.source_item_id, ',') FROM work_item_relations r WHERE r.kind = 'blocks' AND r.target_item_id = t.id), '')
FROM tasks t
WHERE t.id <> ? AND t.lifecycle_state IS NOT 'done'
ORDER BY t.updated_at DESC, t.id LIMIT 100`, set.SourceTaskID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var candidate reviewFollowUpCandidateTask
		if err := rows.Scan(&candidate.ID, &candidate.Title, &candidate.State, &candidate.CreatedBy,
			&candidate.FeatureID, &candidate.ParentID, &candidate.Branch, &candidate.Blockers); err != nil {
			rows.Close()
			return "", err
		}
		organizerContext.CandidateTasks = append(organizerContext.CandidateTasks, candidate)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	transitions, err := workflowTransitionsForProjection(ctx, e.db, set.WorkflowRunID)
	if err == nil {
		organizerContext.OwnerRulings, _ = ProjectOwnerRulings(transitions)
	}
	encoded, err := json.MarshalIndent(organizerContext, "", "  ")
	if err != nil {
		return "", err
	}
	return "Organize the accepted non-blocking review follow-ups below. The JSON is durable coordinator context for this exact set revision.\n\n```json\n" + string(encoded) + "\n```", nil
}

func (e *WorkflowExecutor) markReviewFollowUpOrganizerAttention(ctx context.Context, setID string, revision int, message string) error {
	message = truncateUTF8Bytes(strings.TrimSpace(message), 4096, "…")
	cause := errors.New(message)
	nowText := formatTime(e.tasks.now().UTC())
	_, err := e.db.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET state = 'attention', last_error = ?, updated_at = ?
WHERE id = ? AND revision = ?`, message, nowText, setID, revision)
	if err != nil {
		return errors.Join(cause, err)
	}
	_, err = e.db.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET state = 'failed', materialization_error = ?, updated_at = ?
WHERE set_id = ? AND set_revision = ?`, message, nowText, setID, revision)
	return errors.Join(cause, err)
}

func (s *TaskService) markReviewFollowUpOrganizerTaskState(ctx context.Context, organizerTaskID string, state ReviewFollowUpSetState, message string) error {
	message = truncateUTF8Bytes(strings.TrimSpace(message), 4096, "…")
	planState := "organizing"
	switch state {
	case ReviewFollowUpSetAwaitingReview:
		planState = "awaiting_review"
	case ReviewFollowUpSetAttention:
		planState = "failed"
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET state = ?, last_error = ?, updated_at = ?
WHERE organizer_task_id = ? AND state NOT IN ('materialized', 'closed')`,
		string(state), message, formatTime(s.now().UTC()), strings.TrimSpace(organizerTaskID))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET state = ?, materialization_error = ?, updated_at = ?
WHERE organizer_task_id = ? AND state NOT IN ('materialized', 'stale')`,
		planState, message, formatTime(s.now().UTC()), strings.TrimSpace(organizerTaskID))
	return err
}
