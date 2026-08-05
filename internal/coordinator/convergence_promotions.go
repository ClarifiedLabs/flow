package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

const (
	convergencePromotionPrepared     = "prepared"
	convergencePromotionMaterialized = "materialized"
	convergencePromotionCompleted    = "completed"
)

type convergencePromotion struct {
	SourceTaskID          string
	WorkflowRunID         string
	EvidenceFingerprint   string
	Evidence              ConvergenceEvidence
	FeatureID             string
	PlanningTaskID        string
	PlanningWorkflowRunID string
	FeatureTitle          string
	FeatureBody           string
	PlanningFlowID        string
	PlanningTitle         string
	PlanningBody          string
	Actor                 Actor
	Note                  string
	State                 string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	CompletedAt           *time.Time
}

type convergencePromotionPreparedPayload struct {
	EvidenceFingerprint string `json:"evidence_fingerprint"`
	FeatureID           string `json:"feature_id"`
	PlanningTaskID      string `json:"planning_task_id"`
	Actor               string `json:"actor"`
	Note                string `json:"note,omitempty"`
}

const convergencePromotionSelect = `
SELECT source_task_id, workflow_run_id, evidence_fingerprint, evidence_json,
	feature_id, planning_task_id, COALESCE(planning_workflow_run_id, ''),
	feature_title, feature_body, planning_flow_id, planning_title, planning_body,
	actor, note, state, created_at, updated_at, completed_at
FROM convergence_promotions`

// PromoteConvergenceReview turns an approved oversized implementation into a
// clean-base feature planning workflow. The durable intent is committed before
// the feature ref is created; every later step is idempotent and Tick resumes
// interrupted promotions after a coordinator restart. The evidence refresh and
// the source/base ref lock are composed here, exactly like the other final
// dispositions, so the promotion can never adopt a different Git state than the
// one the owner reviewed.
func (e *WorkflowExecutor) PromoteConvergenceReview(ctx context.Context, input ResolveConvergenceReviewInput) (ConvergenceReviewResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.Note = strings.TrimSpace(input.Note)
	input.ExpectedEvidenceFingerprint = strings.TrimSpace(input.ExpectedEvidenceFingerprint)
	input.Disposition = ConvergencePromote
	if input.TaskID == "" {
		return ConvergenceReviewResult{}, errors.New("task id is required")
	}
	if input.ExpectedEvidenceFingerprint == "" {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: reviewed convergence evidence fingerprint is required", ErrWorkflowConflict)
	}
	if len(input.Note) > 4096 {
		return ConvergenceReviewResult{}, errors.New("convergence decision note exceeds 4096 bytes")
	}
	if input.Actor == "" {
		input.Actor = ActorHuman
	}
	if err := validateActor(input.Actor); err != nil {
		return ConvergenceReviewResult{}, err
	}
	if e.features == nil || e.tasks == nil || e.runs == nil || strings.TrimSpace(e.project.ExchangePath) == "" {
		return ConvergenceReviewResult{}, ErrConvergencePromotionRequired
	}

	promotion, found, err := e.loadConvergencePromotion(ctx, input.TaskID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	if found {
		if promotion.EvidenceFingerprint != input.ExpectedEvidenceFingerprint {
			return ConvergenceReviewResult{}, fmt.Errorf("%w: convergence promotion was prepared for different evidence", ErrWorkflowConflict)
		}
		return e.resumeConvergencePromotion(ctx, promotion)
	}

	evidence, err := e.RefreshConvergenceEvidence(ctx, input.TaskID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	if evidence.Fingerprint != input.ExpectedEvidenceFingerprint {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: convergence evidence changed before disposition", ErrWorkflowConflict)
	}

	var result ConvergenceReviewResult
	err = e.WithConvergenceEvidenceRefsLocked(ctx, evidence, func(lockedCtx context.Context) error {
		promotion, err = e.prepareConvergencePromotion(lockedCtx, input, evidence)
		if err != nil {
			return err
		}
		result, err = e.resumeConvergencePromotion(lockedCtx, promotion)
		return err
	})
	return result, err
}

// ResumeConvergencePromotions repairs promotions interrupted between their
// durable intent, Git ref creation, database materialization, and scheduling.
func (e *WorkflowExecutor) ResumeConvergencePromotions(ctx context.Context) error {
	if e.features == nil || e.tasks == nil || e.runs == nil || strings.TrimSpace(e.project.ExchangePath) == "" {
		return nil
	}
	rows, err := e.db.QueryContext(ctx, `
SELECT source_task_id
FROM convergence_promotions
WHERE state != 'completed'
ORDER BY created_at, source_task_id`)
	if err != nil {
		return err
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var errs error
	for _, taskID := range taskIDs {
		promotion, found, loadErr := e.loadConvergencePromotion(ctx, taskID)
		if loadErr != nil {
			errs = errors.Join(errs, fmt.Errorf("load convergence promotion for %s: %w", taskID, loadErr))
			continue
		}
		if !found {
			continue
		}
		if _, resumeErr := e.resumeConvergencePromotion(ctx, promotion); resumeErr != nil {
			errs = errors.Join(errs, fmt.Errorf("resume convergence promotion for %s: %w", taskID, resumeErr))
		}
	}
	return errs
}

// prepareConvergencePromotion persists the durable promotion intent and the
// feature/planning-task rows in one transaction, before any Git write. The
// intent pins the exact reviewed evidence, so a replay can never re-decide
// against a different base; the pre-created feature and planning task rows make
// the promotion foreign keys resolve and are reused by every retry.
func (e *WorkflowExecutor) prepareConvergencePromotion(ctx context.Context, input ResolveConvergenceReviewInput, refreshed ConvergenceEvidence) (convergencePromotion, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return convergencePromotion{}, err
	}
	defer tx.Rollback()

	if existing, found, err := loadConvergencePromotionTx(ctx, tx, input.TaskID); err != nil {
		return convergencePromotion{}, err
	} else if found {
		if existing.EvidenceFingerprint != input.ExpectedEvidenceFingerprint {
			return convergencePromotion{}, fmt.Errorf("%w: convergence promotion was prepared for different evidence", ErrWorkflowConflict)
		}
		return existing, nil
	}

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, input.TaskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return convergencePromotion{}, ErrWorkflowRunNotFound
		}
		return convergencePromotion{}, err
	}
	if !run.Held() {
		return convergencePromotion{}, ErrWorkflowNotHeld
	}
	evidence, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if evidence == nil || evidence.Fingerprint != input.ExpectedEvidenceFingerprint || evidence.Fingerprint != refreshed.Fingerprint {
		return convergencePromotion{}, fmt.Errorf("%w: convergence evidence changed before promotion", ErrWorkflowConflict)
	}
	if err := validateConvergenceProjectionTx(ctx, tx, run, *evidence); err != nil {
		return convergencePromotion{}, err
	}
	var liveConsoles int
	if err := tx.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM jobs
	 WHERE task_id = ? AND role = 'console' AND state IN ('queued', 'claimed', 'running')) +
	(SELECT COUNT(*) FROM sessions
	 WHERE task_id = ? AND role = 'console'
	 AND runtime_state IN ('starting', 'working', 'waiting'))`, input.TaskID, input.TaskID).Scan(&liveConsoles); err != nil {
		return convergencePromotion{}, err
	}
	if liveConsoles > 0 {
		return convergencePromotion{}, fmt.Errorf("%w: exit the repair console before choosing a final disposition", ErrWorkflowConflict)
	}

	source, err := scanTask(tx.QueryRowContext(ctx, "SELECT"+taskSelectColumns+"\nFROM tasks i WHERE i.id = ?", input.TaskID))
	if errors.Is(err, sql.ErrNoRows) {
		return convergencePromotion{}, errors.New("source task not found")
	}
	if err != nil {
		return convergencePromotion{}, err
	}
	// Feature and planning ids are allocated and persisted in the same
	// transaction as the durable intent, so a failed or retried request can
	// never mint different ids for the same task.
	var planningFlowID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM flows WHERE name = ?`, PlanningFlowName).Scan(&planningFlowID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return convergencePromotion{}, ErrFlowNotFound
		}
		return convergencePromotion{}, err
	}

	// Verify the task's open run matches the reviewed evidence and is held by
	// the operator before allocating promotion ids.
	if err := verifyPromotionHeldRunTx(ctx, tx, input.TaskID, run.ID, input.Actor); err != nil {
		return convergencePromotion{}, err
	}

	featureID, err := e.features.allocateFeatureID(ctx, tx)
	if err != nil {
		return convergencePromotion{}, err
	}
	planningTaskID, err := e.tasks.allocateTaskID(ctx, tx)
	if err != nil {
		return convergencePromotion{}, err
	}

	featureTitle := source.Title
	// The planning body carries the promotion lineage as readable context: the
	// planner is told which source task grew past convergence and how to read its
	// history before proposing the follow-on graph.
	planningBody := promotedPlanningBody(input.TaskID, source.Body)
	var titleTaken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM features WHERE title_norm = lower(trim(?))`, featureTitle).Scan(&titleTaken); err != nil {
		return convergencePromotion{}, err
	}
	if titleTaken > 0 {
		featureTitle = fmt.Sprintf("%s (%s)", source.Title, source.ID)
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return convergencePromotion{}, err
	}
	now := e.runs.now().UTC()
	nowText := formatTime(now)
	if err := insertWorkItem(ctx, tx, featureID, WorkItemFeature, nowText); err != nil {
		return convergencePromotion{}, err
	}
	if err := insertWorkItem(ctx, tx, planningTaskID, WorkItemTask, nowText); err != nil {
		return convergencePromotion{}, err
	}
	// The feature and planning task rows are inserted with the durable intent so
	// the promotion's foreign keys resolve immediately; the planner task is
	// created in the open state and the feature as open. Neither is visible to
	// the board until the materialized state completes the promotion, and the
	// exact same rows are reused by every retry.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO features (
	id, title, body, branch, status, integration_feature_id, created_from_sha,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?, ?)`, featureID, featureTitle, source.Body,
		"feature/"+featureID, nullableStringValue(source.FeatureID), evidence.TargetBaseTipSHA,
		string(input.Actor), nowText, nowText); err != nil {
		return convergencePromotion{}, fmt.Errorf("insert promoted feature row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, flow_id, feature_id, created_by,
	source_task_id, source_change_id, created_at, updated_at
)
SELECT ?, ?, ?, priority, ?, ?, ?, id, ?, ?, ?
FROM tasks WHERE id = ?`, planningTaskID, source.Title, planningBody,
		planningFlowID, featureID, string(input.Actor), evidence.ChangeID,
		nowText, nowText, input.TaskID); err != nil {
		return convergencePromotion{}, fmt.Errorf("insert promoted planning task row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO task_tags (task_id, tag_id, created_by, created_at)
SELECT ?, tag_id, ?, ? FROM task_tags WHERE task_id = ?`, planningTaskID,
		string(input.Actor), nowText, input.TaskID); err != nil {
		return convergencePromotion{}, fmt.Errorf("copy promotion task tags: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO convergence_promotions (
	source_task_id, workflow_run_id, evidence_fingerprint, evidence_json,
	feature_id, planning_task_id, feature_title, feature_body,
	planning_flow_id, planning_title, planning_body, actor, note, state,
	created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'prepared', ?, ?)`,
		input.TaskID, run.ID, evidence.Fingerprint, string(evidenceJSON),
		featureID, planningTaskID, featureTitle, source.Body,
		planningFlowID, source.Title, planningBody, string(input.Actor), input.Note,
		nowText, nowText); err != nil {
		return convergencePromotion{}, fmt.Errorf("prepare convergence promotion: %w", err)
	}
	preparedPayload, err := json.Marshal(convergencePromotionPreparedPayload{
		EvidenceFingerprint: evidence.Fingerprint,
		FeatureID:           featureID, PlanningTaskID: planningTaskID,
		Actor: string(input.Actor), Note: input.Note,
	})
	if err != nil {
		return convergencePromotion{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, Outcome: string(ConvergencePromote),
		EventKind: "workflow_convergence_promotion_prepared", PayloadJSON: string(preparedPayload),
		Actor: string(input.Actor), CreatedAt: now,
	}); err != nil {
		return convergencePromotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return convergencePromotion{}, err
	}
	promotion, found, err := e.loadConvergencePromotion(ctx, input.TaskID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if !found {
		return convergencePromotion{}, errors.New("prepared convergence promotion is missing")
	}
	return promotion, nil
}

// promotedPlanningBody prefixes the copied source-task body with the lineage a
// convergence-promoted planner needs: which source task's implementation grew
// past convergence, why it is being decomposed, and how to read that history
// before proposing the follow-on graph. The source body follows verbatim so no
// original requirement is lost.
func promotedPlanningBody(sourceTaskID, sourceBody string) string {
	showCmd := "flow task show " + sourceTaskID
	workflowCmd := "flow task workflow " + sourceTaskID
	relationsCmd := "flow task relations " + sourceTaskID
	preamble := fmt.Sprintf("## Promotion lineage\n\n"+
		"This task was promoted from a convergence review of task **%s**. That task's implementation grew large enough to require decomposition into a clean-base follow-on plan.\n\n"+
		"Before proposing tasks, inspect the source task's history to understand why it grew so large and address those causes in the new plan. Run:\n\n"+
		"    %s\n\n"+
		"This returns the source task's title, body, and status log (review rounds, blocked checks, retries, and owner notes). For its workflow transitions, review artifacts, and relations, run `%s` and `%s`.",
		sourceTaskID, showCmd, workflowCmd, relationsCmd)
	if strings.TrimSpace(sourceBody) == "" {
		return preamble
	}
	return preamble + "\n\n---\n\n" + sourceBody
}

func (e *WorkflowExecutor) resumeConvergencePromotion(ctx context.Context, promotion convergencePromotion) (ConvergenceReviewResult, error) {
	if promotion.State == convergencePromotionCompleted {
		return e.convergencePromotionResult(ctx, promotion)
	}
	// The planning run is scheduled before the feature ref and the materialized
	// commit: a crash between the ref creation and the materialized commit
	// replays onto the same run instead of scheduling a duplicate.
	if err := e.ensurePromotionPlanningRun(ctx, promotion); err != nil {
		return ConvergenceReviewResult{}, err
	}
	featureRef := "refs/heads/feature/" + promotion.FeatureID
	if err := flowgit.CreateOrVerifyRef(ctx, e.project.ExchangePath, featureRef, promotion.Evidence.TargetBaseTipSHA); err != nil {
		return ConvergenceReviewResult{}, fmt.Errorf("seed promoted feature branch: %w", err)
	}
	if promotion.State == convergencePromotionPrepared {
		var err error
		promotion, err = e.materializeConvergencePromotion(ctx, promotion)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
	}
	if promotion.State == convergencePromotionMaterialized {
		var err error
		promotion, err = e.scheduleConvergencePromotion(ctx, promotion)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
	}
	return e.convergencePromotionResult(ctx, promotion)
}

func (e *WorkflowExecutor) materializeConvergencePromotion(ctx context.Context, promotion convergencePromotion) (convergencePromotion, error) {
	tip, exists, err := flowgit.BranchTip(ctx, e.project.ExchangePath, "feature/"+promotion.FeatureID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if !exists || tip != promotion.Evidence.TargetBaseTipSHA {
		return convergencePromotion{}, fmt.Errorf("%w: promoted feature branch does not match the reviewed base", ErrWorkflowConflict)
	}
	// The planning run must exist before the durable intent advances to
	// materialized: the materialized state commits the resolved audit row, the
	// done source task, and the feature rows, so the only later work is wiring
	// the already-scheduled planning run into the promotion record.
	if err := e.ensurePromotionPlanningRun(ctx, promotion); err != nil {
		return convergencePromotion{}, err
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return convergencePromotion{}, err
	}
	defer tx.Rollback()
	current, found, err := loadConvergencePromotionTx(ctx, tx, promotion.SourceTaskID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if !found {
		return convergencePromotion{}, errors.New("convergence promotion intent is missing")
	}
	if current.State != convergencePromotionPrepared {
		return current, nil
	}

	now := e.runs.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_items (id, kind, created_at) VALUES (?, 'feature', ?), (?, 'task', ?)`,
		current.FeatureID, nowText, current.PlanningTaskID, nowText); err != nil {
		return convergencePromotion{}, fmt.Errorf("restore promoted work-item identities: %w", err)
	}
	// The feature and planning task rows were created with the durable intent;
	// these inserts are no-ops on replay (unique constraints), which is what
	// makes the materialized step idempotent.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO features (
	id, title, body, branch, status, integration_feature_id, created_from_sha,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, 'open', (SELECT feature_id FROM tasks WHERE id = ?), ?, ?, ?, ?)`,
		current.FeatureID, current.FeatureTitle,
		current.FeatureBody, "feature/"+current.FeatureID, current.SourceTaskID,
		current.Evidence.TargetBaseTipSHA, string(current.Actor), nowText, nowText); err != nil {
		if strings.Contains(err.Error(), "features.title_norm") {
			return convergencePromotion{}, ErrFeatureTitleTaken
		}
		if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return convergencePromotion{}, fmt.Errorf("materialize promoted feature: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, flow_id, feature_id, created_by,
	source_task_id, source_change_id, created_at, updated_at
)
SELECT ?, ?, ?, priority, ?, ?, ?, id, ?, ?, ?
FROM tasks WHERE id = ?`,
		current.PlanningTaskID, current.PlanningTitle, current.PlanningBody,
		current.PlanningFlowID, current.FeatureID, string(current.Actor), current.Evidence.ChangeID,
		nowText, nowText, current.SourceTaskID); err != nil {
		if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return convergencePromotion{}, fmt.Errorf("materialize promoted planning task: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO task_tags (task_id, tag_id, created_by, created_at)
SELECT ?, tag_id, ?, ? FROM task_tags WHERE task_id = ?`, current.PlanningTaskID,
		string(current.Actor), nowText, current.SourceTaskID); err != nil {
		return convergencePromotion{}, fmt.Errorf("copy promotion task tags: %w", err)
	}
	items := NewWorkItemService(e.db, e.project.ID)
	items.now = e.runs.now
	var sourceFeatureID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT feature_id FROM tasks WHERE id = ?`, current.SourceTaskID).Scan(&sourceFeatureID); err != nil {
		return convergencePromotion{}, err
	}
	if sourceFeatureID.Valid {
		if err := items.linkTx(ctx, tx, sourceFeatureID.String, current.FeatureID, RelationParentOf, current.Actor); err != nil {
			return convergencePromotion{}, fmt.Errorf("link promoted nested feature: %w", err)
		}
	}
	if err := items.linkTx(ctx, tx, current.FeatureID, current.PlanningTaskID, RelationParentOf, current.Actor); err != nil {
		return convergencePromotion{}, fmt.Errorf("link promoted planning task: %w", err)
	}
	if err := items.linkTx(ctx, tx, current.SourceTaskID, current.FeatureID, RelationRelatedTo, current.Actor); err != nil {
		return convergencePromotion{}, fmt.Errorf("relate promotion source task: %w", err)
	}
	resolutionPayload, err := json.Marshal(convergenceResolutionPayload{
		Disposition: ConvergencePromote, EvidenceFingerprint: current.EvidenceFingerprint,
		Actor: string(current.Actor), Note: current.Note,
		FeatureID: current.FeatureID, PlanningTaskID: current.PlanningTaskID,
	})
	if err != nil {
		return convergencePromotion{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: current.SourceTaskID, WorkflowRunID: current.WorkflowRunID,
		Outcome: string(ConvergencePromote), EventKind: "workflow_convergence_review_resolved",
		PayloadJSON: string(resolutionPayload), Actor: string(current.Actor), CreatedAt: now,
	}); err != nil {
		return convergencePromotion{}, err
	}
	if _, err := forceDoneTaskTx(ctx, tx, current.SourceTaskID, ResolutionCancelled, current.Note, current.Actor, now); err != nil {
		return convergencePromotion{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{current.FeatureID, current.PlanningTaskID, current.SourceTaskID}, now); err != nil {
		return convergencePromotion{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE convergence_promotions
SET state = 'materialized', updated_at = ?
WHERE source_task_id = ? AND state = 'prepared'`, nowText, current.SourceTaskID); err != nil {
		return convergencePromotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return convergencePromotion{}, err
	}
	updated, found, err := e.loadConvergencePromotion(ctx, current.SourceTaskID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if !found {
		return convergencePromotion{}, errors.New("materialized convergence promotion is missing")
	}
	return updated, nil
}

// scheduleConvergencePromotion completes the promotion by wiring the existing
// planning run into the durable record. The update is a no-op on replay because
// the state guard is 'materialized' and the completed state is terminal.
func (e *WorkflowExecutor) scheduleConvergencePromotion(ctx context.Context, promotion convergencePromotion) (convergencePromotion, error) {
	runs, err := e.runs.ListForTask(ctx, promotion.PlanningTaskID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if len(runs) == 0 {
		return convergencePromotion{}, errors.New("promoted planning task has no workflow run")
	}
	planningRun := runs[0]
	now := e.runs.now().UTC()
	if _, err := e.db.ExecContext(ctx, `
UPDATE convergence_promotions
SET state = 'completed', planning_workflow_run_id = ?, updated_at = ?, completed_at = ?
WHERE source_task_id = ? AND state = 'materialized'`, planningRun.ID, formatTime(now), formatTime(now), promotion.SourceTaskID); err != nil {
		return convergencePromotion{}, err
	}
	updated, found, err := e.loadConvergencePromotion(ctx, promotion.SourceTaskID)
	if err != nil {
		return convergencePromotion{}, err
	}
	if !found {
		return convergencePromotion{}, errors.New("completed convergence promotion is missing")
	}
	return updated, nil
}

// ensurePromotionPlanningRun schedules the planning workflow exactly once.
// Idempotent replays after a crash between the feature ref creation and the
// materialized commit reuse the existing run instead of scheduling a duplicate.
func (e *WorkflowExecutor) ensurePromotionPlanningRun(ctx context.Context, promotion convergencePromotion) error {
	planningTask, err := e.tasks.GetTask(ctx, promotion.PlanningTaskID)
	if err != nil {
		return err
	}
	if planningTask.State == nil {
		if _, err := e.runs.ScheduleAs(ctx, promotion.PlanningTaskID, ActorSystem); err != nil && !errors.Is(err, ErrWorkflowConflict) {
			return fmt.Errorf("schedule promoted planning workflow: %w", err)
		}
	}
	runs, err := e.runs.ListForTask(ctx, promotion.PlanningTaskID)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return errors.New("promoted planning task has no workflow run")
	}
	return nil
}

func (e *WorkflowExecutor) convergencePromotionResult(ctx context.Context, promotion convergencePromotion) (ConvergenceReviewResult, error) {
	feature, err := e.features.Get(ctx, promotion.FeatureID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	planningTask, err := e.tasks.GetTask(ctx, promotion.PlanningTaskID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	var sourceTask Task
	if promotion.SourceTaskID != "" {
		sourceTask, err = e.tasks.GetTask(ctx, promotion.SourceTaskID)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
	}
	sourceRun, err := e.runs.Get(ctx, promotion.WorkflowRunID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	result := ConvergenceReviewResult{
		Disposition: ConvergencePromote, Evidence: promotion.Evidence, Run: sourceRun,
		Task: &sourceTask, Feature: &feature, PlanningTask: &planningTask,
	}
	if promotion.PlanningWorkflowRunID != "" {
		planningRun, err := e.runs.Get(ctx, promotion.PlanningWorkflowRunID)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
		result.PlanningRun = &planningRun
	}
	return result, nil
}

// verifyPromotionHeldRunTx confirms the operator's held run is still the task's
// open run. The disposition lock is held by the caller, so the held run cannot
// change between the evidence refresh and the durable intent. Convergence holds
// are system-enforced (held_by is always system even when a human requested
// the review), so the human owner may promote any held convergence run.
func verifyPromotionHeldRunTx(ctx context.Context, tx *sql.Tx, taskID, runID string, actor Actor) error {
	var heldBy string
	err := tx.QueryRowContext(ctx, `
SELECT held_by FROM workflow_runs
WHERE id = ? AND task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, runID, taskID).Scan(&heldBy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: convergence workflow run is no longer active", ErrWorkflowConflict)
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(heldBy) == "" {
		return fmt.Errorf("%w: convergence workflow run is not held", ErrWorkflowNotHeld)
	}
	if actor != ActorSystem && heldBy != string(ActorSystem) {
		return fmt.Errorf("%w: convergence workflow is held by %s, not %s", ErrWorkflowConflict, heldBy, actor)
	}
	return nil
}

func (e *WorkflowExecutor) loadConvergencePromotion(ctx context.Context, taskID string) (convergencePromotion, bool, error) {
	return scanConvergencePromotion(e.db.QueryRowContext(ctx, convergencePromotionSelect+`
WHERE source_task_id = ?`, strings.TrimSpace(taskID)))
}

func loadConvergencePromotionTx(ctx context.Context, tx *sql.Tx, taskID string) (convergencePromotion, bool, error) {
	return scanConvergencePromotion(tx.QueryRowContext(ctx, convergencePromotionSelect+`
WHERE source_task_id = ?`, strings.TrimSpace(taskID)))
}

type convergencePromotionScanner interface {
	Scan(dest ...any) error
}

func scanConvergencePromotion(row convergencePromotionScanner) (convergencePromotion, bool, error) {
	var promotion convergencePromotion
	var evidenceJSON, actor, createdAt, updatedAt string
	var completedAt sql.NullString
	err := row.Scan(
		&promotion.SourceTaskID, &promotion.WorkflowRunID, &promotion.EvidenceFingerprint, &evidenceJSON,
		&promotion.FeatureID, &promotion.PlanningTaskID, &promotion.PlanningWorkflowRunID,
		&promotion.FeatureTitle, &promotion.FeatureBody, &promotion.PlanningFlowID,
		&promotion.PlanningTitle, &promotion.PlanningBody, &actor, &promotion.Note,
		&promotion.State, &createdAt, &updatedAt, &completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return convergencePromotion{}, false, nil
	}
	if err != nil {
		return convergencePromotion{}, false, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &promotion.Evidence); err != nil {
		return convergencePromotion{}, false, fmt.Errorf("decode convergence promotion evidence: %w", err)
	}
	if promotion.Evidence.SchemaVersion != ConvergenceEvidenceSchemaVersion {
		return convergencePromotion{}, false, fmt.Errorf("unsupported convergence evidence schema version %d", promotion.Evidence.SchemaVersion)
	}
	promotion.Actor = Actor(actor)
	promotion.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return convergencePromotion{}, false, err
	}
	promotion.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return convergencePromotion{}, false, err
	}
	if completedAt.Valid {
		parsed, parseErr := parseTime(completedAt.String)
		if parseErr != nil {
			return convergencePromotion{}, false, parseErr
		}
		promotion.CompletedAt = &parsed
	}
	return promotion, true, nil
}
