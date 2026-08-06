package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const ConvergenceEvidenceSchemaVersion = 1

// ConvergenceEvidence is the bounded, versioned snapshot behind a system
// convergence hold. Git SHAs and DiffDigest identify the complete immutable
// source change while ChangedFiles remains safe to retain in SQLite and render.
type ConvergenceEvidence struct {
	SchemaVersion       int               `json:"schema_version"`
	Fingerprint         string            `json:"fingerprint"`
	WorkflowRunID       string            `json:"workflow_run_id"`
	NodeRunID           string            `json:"node_run_id"`
	ChangeID            string            `json:"change_id"`
	TaskID              string            `json:"task_id"`
	SourceBranch        string            `json:"source_branch"`
	SourceHeadSHA       string            `json:"source_head_sha"`
	TargetBaseBranch    string            `json:"target_base_branch"`
	TargetBaseTipSHA    string            `json:"target_base_tip_sha"`
	MergeBaseSHA        string            `json:"merge_base_sha"`
	Files               int               `json:"files"`
	Additions           int               `json:"additions"`
	Deletions           int               `json:"deletions"`
	ChangedFiles        []ConvergenceFile `json:"changed_files,omitempty"`
	ChangedFilesOmitted int               `json:"changed_files_omitted,omitempty"`
	DiffDigest          string            `json:"diff_digest"`
	ReviewCyclesUsed    int               `json:"review_cycles_used"`
	ReviewCycleBudget   int               `json:"review_cycle_budget"`
	MaxFiles            int               `json:"max_files"`
	MaxLines            int               `json:"max_lines"`
	CapturedAt          time.Time         `json:"captured_at"`
}

type ConvergenceDisposition string

const (
	ConvergenceAcceptScope  ConvergenceDisposition = "accept_scope"
	ConvergenceRepairBranch ConvergenceDisposition = "repair_branch"
	ConvergenceReturnAuthor ConvergenceDisposition = "return_to_author"
	ConvergencePromote      ConvergenceDisposition = "promote"
	ConvergenceCancel       ConvergenceDisposition = "cancel"
)

var ErrConvergencePromotionRequired = errors.New("convergence promotion must be prepared by the promotion service")

type ResolveConvergenceReviewInput struct {
	TaskID                      string
	Disposition                 ConvergenceDisposition
	Actor                       Actor
	Note                        string
	ExpectedEvidenceFingerprint string
}

type ConvergenceReviewResult struct {
	Disposition  ConvergenceDisposition `json:"disposition"`
	Evidence     ConvergenceEvidence    `json:"evidence"`
	Run          WorkflowRun            `json:"run"`
	Task         *Task                  `json:"task,omitempty"`
	Feature      *Feature               `json:"feature,omitempty"`
	PlanningTask *Task                  `json:"planning_task,omitempty"`
	PlanningRun  *WorkflowRun           `json:"planning_run,omitempty"`
	Ruling       *OwnerRuling           `json:"ruling,omitempty"`
	Delivery     *OwnerRulingDelivery   `json:"delivery,omitempty"`
}

type convergenceResolutionPayload struct {
	Disposition         ConvergenceDisposition `json:"disposition"`
	EvidenceFingerprint string                 `json:"evidence_fingerprint"`
	Actor               string                 `json:"actor"`
	Note                string                 `json:"note,omitempty"`
	FeatureID           string                 `json:"feature_id,omitempty"`
	PlanningTaskID      string                 `json:"planning_task_id,omitempty"`
}

func normalizeConvergenceEvidence(run WorkflowRun, evidence ConvergenceEvidence, now time.Time) (ConvergenceEvidence, error) {
	// Every producer stamps SchemaVersion explicitly; an absent or unknown
	// version is corrupt/unsupported data, never silently promoted.
	if evidence.SchemaVersion != ConvergenceEvidenceSchemaVersion {
		return ConvergenceEvidence{}, fmt.Errorf("unsupported convergence evidence schema version %d", evidence.SchemaVersion)
	}
	if evidence.WorkflowRunID == "" {
		evidence.WorkflowRunID = run.ID
	}
	if evidence.TaskID == "" {
		evidence.TaskID = run.TaskID
	}
	if evidence.WorkflowRunID != run.ID || evidence.TaskID != run.TaskID {
		return ConvergenceEvidence{}, errors.New("convergence evidence does not identify the active workflow")
	}
	for name, value := range map[string]string{
		"node run id":         evidence.NodeRunID,
		"change id":           evidence.ChangeID,
		"source branch":       evidence.SourceBranch,
		"source head sha":     evidence.SourceHeadSHA,
		"target base branch":  evidence.TargetBaseBranch,
		"target base tip sha": evidence.TargetBaseTipSHA,
		"merge base sha":      evidence.MergeBaseSHA,
		"diff digest":         evidence.DiffDigest,
	} {
		if strings.TrimSpace(value) == "" {
			return ConvergenceEvidence{}, fmt.Errorf("convergence evidence %s is required", name)
		}
	}
	if evidence.Files < 0 || evidence.Additions < 0 || evidence.Deletions < 0 || evidence.MaxFiles < 0 || evidence.MaxLines < 0 {
		return ConvergenceEvidence{}, errors.New("convergence evidence counts may not be negative")
	}
	evidence.ReviewCyclesUsed = run.ReviewCyclesUsed
	evidence.ReviewCycleBudget = run.ReviewCycleBudget
	evidence.CapturedAt = now.UTC()
	evidence.ChangedFiles = sortConvergenceFiles(evidence.ChangedFiles)
	evidence.ChangedFiles, evidence.ChangedFilesOmitted = convergencePayloadFiles(evidence.Files, evidence.ChangedFiles)
	evidence.Fingerprint = ""
	fingerprint, err := convergenceEvidenceFingerprint(evidence)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	evidence.Fingerprint = fingerprint
	return evidence, nil
}

func convergenceEvidenceFingerprint(evidence ConvergenceEvidence) (string, error) {
	// CapturedAt records when the durable observation was written; it is not
	// part of the observed Git/workflow identity and must not make retries hash
	// differently.
	evidence.CapturedAt = time.Time{}
	evidence.Fingerprint = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("encode convergence evidence fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

// ActiveConvergenceEvidence returns the newest unresolved typed convergence
// request. repair_branch records an audit decision but deliberately leaves the
// request active while the operator works in the held branch.
func ActiveConvergenceEvidence(transitions []WorkflowTransition) (*ConvergenceEvidence, error) {
	ordered := append([]WorkflowTransition(nil), transitions...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence > ordered[j].Sequence })
	for _, transition := range ordered {
		switch transition.EventKind {
		case "workflow_convergence_review_resolved":
			var resolved convergenceResolutionPayload
			if err := json.Unmarshal(transition.Payload, &resolved); err != nil {
				return nil, fmt.Errorf("decode convergence resolution: %w", err)
			}
			if resolved.Disposition != ConvergenceRepairBranch {
				return nil, nil
			}
		case "workflow_convergence_review_requested":
			var evidence ConvergenceEvidence
			if err := json.Unmarshal(transition.Payload, &evidence); err != nil {
				return nil, fmt.Errorf("decode convergence evidence: %w", err)
			}
			// The newest request governs the hold. An unversioned or
			// unknown-version payload is corrupt/unsupported data: fail closed
			// rather than reporting "no active evidence" or scanning backward to
			// resurrect an earlier request.
			if evidence.SchemaVersion != ConvergenceEvidenceSchemaVersion {
				return nil, fmt.Errorf("unsupported convergence evidence schema version %d", evidence.SchemaVersion)
			}
			return &evidence, nil
		}
	}
	return nil, nil
}

type convergenceTransitionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ActiveConvergenceEvidenceForTask resolves evidence from the complete
// transition history of the active held run. Task-wide activity feeds are
// intentionally bounded and may contain transitions from older runs.
func (s *WorkflowRunService) ActiveConvergenceEvidenceForTask(ctx context.Context, taskID string) (*ConvergenceEvidence, error) {
	run, active, err := s.ActiveForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !active || !run.Held() {
		return nil, nil
	}
	return activeConvergenceEvidenceTx(ctx, s.db, run.ID)
}

// RefreshConvergenceEvidence adopts a changed Git observation as a new
// immutable artifact/evidence pair. Source-head changes require an explicit
// repair_branch decision; base-only movement can be refreshed directly while
// the same convergence hold remains active.
func (s *WorkflowRunService) RefreshConvergenceEvidence(ctx context.Context, evidence ConvergenceEvidence) (ConvergenceEvidence, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, strings.TrimSpace(evidence.TaskID)))
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if !run.Held() {
		return ConvergenceEvidence{}, ErrWorkflowNotHeld
	}
	current, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if current == nil {
		return ConvergenceEvidence{}, fmt.Errorf("%w: workflow has no active convergence review", ErrWorkflowConflict)
	}
	evidence, err = normalizeConvergenceEvidence(run, evidence, s.now().UTC())
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if run.CurrentNodeRunID != evidence.NodeRunID || evidence.ChangeID != current.ChangeID {
		return ConvergenceEvidence{}, fmt.Errorf("%w: refreshed evidence does not match the active review", ErrWorkflowConflict)
	}
	if evidence.SourceHeadSHA != current.SourceHeadSHA {
		var latestResolutionJSON string
		if err := tx.QueryRowContext(ctx, `
SELECT payload_json
FROM workflow_transitions
WHERE workflow_run_id = ?
	AND event_kind = 'workflow_convergence_review_resolved'
	AND seq > COALESCE((
		SELECT MAX(seq) FROM workflow_transitions
		WHERE workflow_run_id = ? AND event_kind = 'workflow_convergence_review_requested'
	), 0)
ORDER BY seq DESC LIMIT 1`, run.ID, run.ID).Scan(&latestResolutionJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ConvergenceEvidence{}, fmt.Errorf("%w: branch evidence can only refresh after repair_branch", ErrWorkflowConflict)
			}
			return ConvergenceEvidence{}, err
		}
		var latestResolution convergenceResolutionPayload
		if err := json.Unmarshal([]byte(latestResolutionJSON), &latestResolution); err != nil {
			return ConvergenceEvidence{}, fmt.Errorf("decode convergence repair resolution: %w", err)
		}
		if latestResolution.Disposition != ConvergenceRepairBranch {
			return ConvergenceEvidence{}, fmt.Errorf("%w: branch evidence can only refresh after repair_branch", ErrWorkflowConflict)
		}
	}
	var branch, base string
	var headSHA, workflowRunID sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT branch, base, head_sha, workflow_run_id FROM changes WHERE id = ? AND task_id = ?`,
		evidence.ChangeID, run.TaskID).Scan(&branch, &base, &headSHA, &workflowRunID); err != nil {
		return ConvergenceEvidence{}, err
	}
	if branch != evidence.SourceBranch || base != evidence.TargetBaseBranch ||
		!headSHA.Valid || headSHA.String != evidence.SourceHeadSHA ||
		!workflowRunID.Valid || workflowRunID.String != run.ID {
		return ConvergenceEvidence{}, fmt.Errorf("%w: repaired evidence does not match the current change projection", ErrWorkflowConflict)
	}

	artifactPayload, err := canonicalArtifactPayload(ArtifactChange, json.RawMessage(fmt.Sprintf(
		`{"change_id":%q,"head_sha":%q}`, evidence.ChangeID, evidence.SourceHeadSHA)))
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	const summary = "Change snapshot refreshed after convergence branch repair."
	artifactPayloadDigest := artifactDigest(ArtifactChange, summary, artifactPayload, evidence.MergeBaseSHA)
	creatorKey := "system:convergence-repair:" + run.ID
	clientKey := evidence.Fingerprint
	var artifactID, existingDigest string
	err = tx.QueryRowContext(ctx, `
SELECT id, payload_sha256 FROM workflow_artifacts WHERE creator_key = ? AND client_key = ?`,
		creatorKey, clientKey).Scan(&artifactID, &existingDigest)
	if errors.Is(err, sql.ErrNoRows) {
		artifactID, err = randomPrefixedID("wa")
		if err != nil {
			return ConvergenceEvidence{}, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, base_revision, client_key, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifactID, run.ID, evidence.NodeRunID,
			creatorKey, string(ArtifactChange), summary, string(artifactPayload), artifactPayloadDigest,
			evidence.MergeBaseSHA, clientKey, sqlitex.FormatTime(evidence.CapturedAt)); err != nil {
			return ConvergenceEvidence{}, err
		}
	} else if err != nil {
		return ConvergenceEvidence{}, err
	} else if existingDigest != artifactPayloadDigest {
		return ConvergenceEvidence{}, fmt.Errorf("%w: convergence artifact fingerprint was reused", ErrWorkflowConflict)
	}

	if result, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET input_artifact_id = ?
WHERE id = ? AND workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
		artifactID, evidence.NodeRunID, run.ID); err != nil {
		return ConvergenceEvidence{}, err
	} else if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ConvergenceEvidence{}, fmt.Errorf("%w: active review node changed during evidence refresh", ErrWorkflowConflict)
	}
	if result, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET current_artifact_id = ?, version = version + 1
WHERE id = ? AND current_node_run_id = ? AND held_at IS NOT NULL`, artifactID, run.ID, evidence.NodeRunID); err != nil {
		return ConvergenceEvidence{}, err
	} else if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ConvergenceEvidence{}, fmt.Errorf("%w: convergence hold changed during evidence refresh", ErrWorkflowConflict)
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, EventKind: "workflow_convergence_review_requested",
		PayloadJSON: string(payload), Actor: string(ActorSystem), CreatedAt: evidence.CapturedAt,
	}); err != nil {
		return ConvergenceEvidence{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConvergenceEvidence{}, err
	}
	return evidence, nil
}

func activeConvergenceEvidenceTx(ctx context.Context, queryer convergenceTransitionQueryer, runID string) (*ConvergenceEvidence, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT seq, event_kind, payload_json
FROM workflow_transitions
WHERE workflow_run_id = ?
	AND event_kind IN ('workflow_convergence_review_requested', 'workflow_convergence_review_resolved')
ORDER BY seq DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var transitions []WorkflowTransition
	for rows.Next() {
		var transition WorkflowTransition
		var payload string
		if err := rows.Scan(&transition.Sequence, &transition.EventKind, &payload); err != nil {
			return nil, err
		}
		transition.Payload = json.RawMessage(payload)
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ActiveConvergenceEvidence(transitions)
}

// ResolveConvergenceReview applies decisions that are local to the source
// workflow. Promotion is intentionally rejected here: its service must first
// persist a repairable intent before performing any Git write.
func (s *WorkflowRunService) ResolveConvergenceReview(ctx context.Context, input ResolveConvergenceReviewInput) (ConvergenceReviewResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.Note = strings.TrimSpace(input.Note)
	input.ExpectedEvidenceFingerprint = strings.TrimSpace(input.ExpectedEvidenceFingerprint)
	if len(input.Note) > 4096 {
		return ConvergenceReviewResult{}, errors.New("convergence decision note exceeds 4096 bytes")
	}
	if input.Actor == "" {
		input.Actor = ActorHuman
	}
	switch input.Disposition {
	case ConvergenceAcceptScope, ConvergenceRepairBranch, ConvergenceReturnAuthor, ConvergenceCancel:
	case ConvergencePromote:
		return ConvergenceReviewResult{}, ErrConvergencePromotionRequired
	default:
		return ConvergenceReviewResult{}, fmt.Errorf("invalid convergence disposition %q", input.Disposition)
	}

	if input.ExpectedEvidenceFingerprint == "" {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: reviewed convergence evidence fingerprint is required", ErrWorkflowConflict)
	}
	if input.Disposition == ConvergenceReturnAuthor && input.Note == "" {
		return ConvergenceReviewResult{}, errors.New("return_to_author requires a decision note")
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	defer tx.Rollback()
	var pendingPromotion int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM convergence_promotions
WHERE source_task_id = ? AND state != 'completed'`, input.TaskID).Scan(&pendingPromotion); err != nil {
		return ConvergenceReviewResult{}, err
	}
	if pendingPromotion > 0 {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: convergence promotion is already in progress", ErrWorkflowConflict)
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, input.TaskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConvergenceReviewResult{}, ErrWorkflowRunNotFound
		}
		return ConvergenceReviewResult{}, err
	}
	if !run.Held() {
		return ConvergenceReviewResult{}, ErrWorkflowNotHeld
	}
	evidence, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	if evidence == nil {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: workflow has no active convergence review", ErrWorkflowConflict)
	}
	if evidence.Fingerprint != input.ExpectedEvidenceFingerprint {
		return ConvergenceReviewResult{}, fmt.Errorf("%w: convergence evidence changed before disposition", ErrWorkflowConflict)
	}
	if input.Disposition != ConvergenceRepairBranch {
		if err := validateConvergenceProjectionTx(ctx, tx, run, *evidence); err != nil {
			return ConvergenceReviewResult{}, err
		}
		var liveConsoles int
		if err := tx.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM jobs
	 WHERE task_id = ? AND role = 'console' AND state IN ('queued', 'claimed', 'running')) +
	(SELECT COUNT(*) FROM sessions
	 WHERE task_id = ? AND role = 'console'
		AND runtime_state IN ('starting', 'working', 'waiting'))`, input.TaskID, input.TaskID).Scan(&liveConsoles); err != nil {
			return ConvergenceReviewResult{}, err
		}
		if liveConsoles > 0 {
			return ConvergenceReviewResult{}, fmt.Errorf("%w: exit the repair console before choosing a final disposition", ErrWorkflowConflict)
		}
	}
	now := s.now().UTC()
	payload, err := json.Marshal(convergenceResolutionPayload{
		Disposition: input.Disposition, EvidenceFingerprint: evidence.Fingerprint,
		Actor: string(input.Actor), Note: input.Note,
	})
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, Outcome: string(input.Disposition),
		EventKind: "workflow_convergence_review_resolved", PayloadJSON: string(payload),
		Actor: string(input.Actor), CreatedAt: now,
	}); err != nil {
		return ConvergenceReviewResult{}, err
	}

	result := ConvergenceReviewResult{Disposition: input.Disposition, Evidence: *evidence, Run: run}
	switch input.Disposition {
	case ConvergenceRepairBranch:
		// The hold and typed evidence remain active until a later decision.
	case ConvergenceAcceptScope:
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`, run.ID); err != nil {
			return ConvergenceReviewResult{}, err
		}
	case ConvergenceReturnAuthor:
		sourceNode, ok := run.Snapshot.Node(run.CurrentNodeKey)
		if !ok || sourceNode.Kind != NodeChangeReview || run.CurrentNodeRunID != evidence.NodeRunID {
			return ConvergenceReviewResult{}, fmt.Errorf("%w: return_to_author requires the active change-review node", ErrWorkflowConflict)
		}
		targetKey, ok := run.Snapshot.Target(sourceNode.Key, "changes_requested")
		if !ok {
			return ConvergenceReviewResult{}, fmt.Errorf("%w: change-review node has no changes_requested edge", ErrWorkflowConflict)
		}
		targetNode, ok := run.Snapshot.Node(targetKey)
		if !ok || targetNode.Kind != NodeAgent || targetNode.Config.Agent == nil || targetNode.Config.Agent.Workspace != WorkspaceChange {
			return ConvergenceReviewResult{}, fmt.Errorf("%w: changes_requested must target a change-workspace author", ErrWorkflowConflict)
		}
		rulingID, err := randomPrefixedID("rule")
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
		rulingPayload := ownerRulingPayload{
			SchemaVersion: OwnerRulingSchemaVersion, RulingID: rulingID, Body: input.Note,
			Source: OwnerRulingSourceConvergenceReturn, NodeRunID: evidence.NodeRunID,
		}
		rulingJSON, err := json.Marshal(rulingPayload)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
		if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
			TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
			EventKind: OwnerRulingEventKind, PayloadJSON: string(rulingJSON), Actor: string(input.Actor),
			IdempotencyKey: "ruling:convergence-return:" + evidence.Fingerprint, CreatedAt: now,
		}); err != nil {
			return ConvergenceReviewResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`, run.ID); err != nil {
			return ConvergenceReviewResult{}, err
		}
		completion, err := s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
			NodeRunID: evidence.NodeRunID, Outcome: "changes_requested", Actor: input.Actor,
			Payload:        map[string]any{"convergence_return": true, "ruling_id": rulingID},
			IdempotencyKey: "convergence-return:" + evidence.Fingerprint,
		}, false, nil)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
		result.Run = completion.Run
		ruling := ownerRulingFromPayload(rulingPayload, run.ID, run.TaskID, string(input.Actor), now)
		result.Ruling = &ruling
	case ConvergenceCancel:
		task, err := forceDoneTaskTx(ctx, tx, input.TaskID, ResolutionCancelled, input.Note, input.Actor, now)
		if err != nil {
			return ConvergenceReviewResult{}, err
		}
		result.Task = &task
	}
	if err := tx.Commit(ctx); err != nil {
		return ConvergenceReviewResult{}, err
	}
	result.Run, err = s.Get(ctx, run.ID)
	if err != nil {
		return ConvergenceReviewResult{}, err
	}
	if result.Ruling != nil {
		delivery := s.deliverOwnerRuling(ctx, *result.Ruling)
		result.Delivery = &delivery
		s.observeOwnerRuling(*result.Ruling, delivery)
	}
	return result, nil
}

type convergenceMutationTx interface {
	workflowTransitionExecer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateConvergenceProjectionTx(ctx context.Context, tx convergenceMutationTx, run WorkflowRun, evidence ConvergenceEvidence) error {
	var branch, base string
	var headSHA, workflowRunID sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT branch, base, head_sha, workflow_run_id
FROM changes
WHERE id = ? AND task_id = ?`, evidence.ChangeID, run.TaskID).Scan(&branch, &base, &headSHA, &workflowRunID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: convergence change projection is missing", ErrWorkflowConflict)
		}
		return err
	}
	if branch != evidence.SourceBranch || base != evidence.TargetBaseBranch ||
		!headSHA.Valid || headSHA.String != evidence.SourceHeadSHA ||
		!workflowRunID.Valid || workflowRunID.String != run.ID {
		return fmt.Errorf("%w: convergence change projection changed before disposition", ErrWorkflowConflict)
	}
	if run.CurrentArtifactID == "" {
		return fmt.Errorf("%w: convergence artifact projection is missing", ErrWorkflowConflict)
	}
	var artifact WorkflowArtifact
	var kind, payload string
	if err := tx.QueryRowContext(ctx, `
SELECT workflow_run_id, node_run_id, kind, payload_json
FROM workflow_artifacts
WHERE id = ?`, run.CurrentArtifactID).Scan(&artifact.WorkflowRunID, &artifact.NodeRunID, &kind, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: convergence artifact projection is missing", ErrWorkflowConflict)
		}
		return err
	}
	artifact.ID = run.CurrentArtifactID
	artifact.Kind = ArtifactKind(kind)
	artifact.Payload = json.RawMessage(payload)
	artifactChangeID, artifactHeadSHA, err := changeIdentityFromArtifact(artifact)
	if err != nil || artifact.WorkflowRunID != run.ID || artifactChangeID != evidence.ChangeID || artifactHeadSHA != evidence.SourceHeadSHA {
		return fmt.Errorf("%w: convergence artifact projection changed before disposition", ErrWorkflowConflict)
	}
	return nil
}

// forceDoneTaskTx mirrors the owner force-done transaction so convergence
// cancellation can add its disposition audit row atomically. P0 promotion also
// composes this helper when approved decomposition supersedes the source task.
func forceDoneTaskTx(ctx context.Context, tx convergenceMutationTx, taskID string, resolution DoneResolution, note string, actor Actor, now time.Time) (Task, error) {
	var currentState sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return Task{}, err
	}
	if currentState.Valid && currentState.String == string(LifecycleDone) {
		return Task{}, fmt.Errorf("%w: task is already done", ErrWorkflowConflict)
	}
	var runID sql.NullString
	_ = tx.QueryRowContext(ctx, `
SELECT id FROM workflow_runs WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID).Scan(&runID)
	if runID.Valid {
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE (workflow_run_id = ? OR task_id = ?) AND state IN ('queued', 'claimed', 'running')`,
			sqlitex.FormatTime(now), runID.String, taskID); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (SELECT id FROM jobs WHERE workflow_run_id = ? OR task_id = ?) AND released_at IS NULL`,
			sqlitex.FormatTime(now), runID.String, taskID); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE (workflow_run_id = ? OR task_id = ?) AND runtime_state IN ('starting', 'working', 'waiting')`,
			sqlitex.FormatTime(now), sqlitex.FormatTime(now), runID.String, taskID); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, completed_at = COALESCE(completed_at, ?)
WHERE workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
			string(WorkflowNodeCancelled), sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, completed_at = ?, completion_source = 'owner_override',
		current_node_run_id = NULL, held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`,
			string(WorkflowRunCompleted), sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if err := resolveOpenWaitTx(ctx, tx, runID.String, actor, now); err != nil {
			return Task{}, err
		}
		if err := retireIncompleteWorkflowChecksTx(ctx, tx, taskID, runID.String, now); err != nil {
			return Task{}, err
		}
	} else {
		if err := resolveOpenWaitTx(ctx, tx, "", actor, now); err != nil {
			return Task{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(resolution), sqlitex.FormatTime(now), sqlitex.FormatTime(now), taskID); err != nil {
		return Task{}, err
	}
	rebaseState := string(RebaseCancelled)
	if resolution == ResolutionFailed {
		rebaseState = string(RebaseFailed)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE feature_rebases SET state = ?, completed_at = ?
WHERE task_id = ? AND state = 'running'`, rebaseState, sqlitex.FormatTime(now), taskID); err != nil {
		return Task{}, fmt.Errorf("close rebase row for convergence-cancelled task: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"note": strings.TrimSpace(note), "resolution": resolution})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, WorkflowRunID: runID.String, FromTaskState: currentState.String,
		ToTaskState: string(LifecycleDone), EventKind: "owner_done", PayloadJSON: string(payload),
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return Task{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{taskID}, now); err != nil {
		return Task{}, err
	}
	return scanTask(tx.QueryRowContext(ctx, "SELECT"+taskSelectColumns+"\nFROM tasks i WHERE i.id = ?", taskID))
}
