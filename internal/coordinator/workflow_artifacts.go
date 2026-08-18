package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var ErrWorkflowArtifactNotFound = errors.New("workflow artifact not found")

type WorkflowArtifact struct {
	ID              string          `json:"id"`
	WorkflowRunID   string          `json:"workflow_run_id"`
	NodeRunID       string          `json:"node_run_id"`
	SessionID       string          `json:"session_id,omitempty"`
	Kind            ArtifactKind    `json:"kind"`
	SummaryMarkdown string          `json:"summary_markdown"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	PayloadSHA256   string          `json:"payload_sha256"`
	BaseRevision    string          `json:"base_revision,omitempty"`
	ClientKey       string          `json:"client_key"`
	CreatedAt       time.Time       `json:"created_at"`
}

type CreateWorkflowArtifactInput struct {
	WorkflowRunID   string          `json:"workflow_run_id"`
	NodeRunID       string          `json:"node_run_id"`
	SessionID       string          `json:"session_id,omitempty"`
	CreatorKey      string          `json:"-"`
	Kind            ArtifactKind    `json:"kind"`
	SummaryMarkdown string          `json:"summary_markdown"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	BaseRevision    string          `json:"base_revision,omitempty"`
	ClientKey       string          `json:"client_key"`
}

type WorkflowArtifactService struct {
	db       *sql.DB
	tasks    *TaskService
	items    *WorkItemService
	epics    *EpicService
	Features *FeatureService
	now      func() time.Time
}

func NewWorkflowArtifactService(db *sql.DB, tasks *TaskService) *WorkflowArtifactService {
	projectID := ""
	if tasks != nil {
		projectID = tasks.projectID
	}
	items := NewWorkItemService(db, projectID)
	return &WorkflowArtifactService{
		db: db, tasks: tasks, items: items,
		epics: NewEpicService(db, projectID, items), now: sqlitex.UTCNow,
	}
}

func (s *WorkflowArtifactService) Create(ctx context.Context, input CreateWorkflowArtifactInput) (WorkflowArtifact, bool, error) {
	input.WorkflowRunID = strings.TrimSpace(input.WorkflowRunID)
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.CreatorKey = strings.TrimSpace(input.CreatorKey)
	input.SummaryMarkdown = strings.TrimSpace(input.SummaryMarkdown)
	input.BaseRevision = strings.TrimSpace(input.BaseRevision)
	input.ClientKey = strings.TrimSpace(input.ClientKey)
	if input.WorkflowRunID == "" || input.NodeRunID == "" {
		return WorkflowArtifact{}, false, errors.New("workflow run id and node run id are required")
	}
	if input.CreatorKey == "" {
		return WorkflowArtifact{}, false, errors.New("artifact creator key is required")
	}
	if input.ClientKey == "" {
		return WorkflowArtifact{}, false, errors.New("artifact client key is required")
	}
	if input.SummaryMarkdown == "" {
		return WorkflowArtifact{}, false, errors.New("artifact summary is required")
	}
	canonicalPayload, err := canonicalArtifactPayload(input.Kind, input.Payload)
	if err != nil {
		return WorkflowArtifact{}, false, err
	}
	var taskSetManifest *TaskSetManifest
	if input.Kind == ArtifactTaskSet {
		manifest, err := DecodeTaskSetManifest(canonicalPayload)
		if err != nil {
			return WorkflowArtifact{}, false, err
		}
		taskSetManifest = &manifest
		canonicalPayload, err = json.Marshal(manifest)
		if err != nil {
			return WorkflowArtifact{}, false, fmt.Errorf("encode normalized task-set manifest: %w", err)
		}
	}
	digest := artifactDigest(input.Kind, input.SummaryMarkdown, canonicalPayload, input.BaseRevision)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowArtifact{}, false, err
	}
	defer tx.Rollback()
	existing, found, err := scanWorkflowArtifactMaybe(tx.QueryRowContext(ctx, workflowArtifactSelect+`
WHERE creator_key = ? AND client_key = ?`, input.CreatorKey, input.ClientKey))
	if err != nil {
		return WorkflowArtifact{}, false, err
	}
	if found {
		if existing.PayloadSHA256 != digest || existing.WorkflowRunID != input.WorkflowRunID || existing.NodeRunID != input.NodeRunID {
			return WorkflowArtifact{}, false, fmt.Errorf("%w: artifact idempotency key was reused with different content", ErrWorkflowConflict)
		}
		return existing, true, nil
	}

	var runID, nodeKey, state string
	if err := tx.QueryRowContext(ctx, `
SELECT nr.workflow_run_id, nr.node_key, nr.state
FROM workflow_node_runs nr WHERE nr.id = ?`, input.NodeRunID).Scan(&runID, &nodeKey, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowArtifact{}, false, ErrWorkflowRunNotFound
		}
		return WorkflowArtifact{}, false, err
	}
	if runID != input.WorkflowRunID {
		return WorkflowArtifact{}, false, errors.New("node run does not belong to workflow run")
	}
	if state != string(WorkflowNodeRunning) && state != string(WorkflowNodeWaiting) {
		return WorkflowArtifact{}, false, fmt.Errorf("%w: node run is %s", ErrWorkflowConflict, state)
	}
	var snapshotJSON string
	if err := tx.QueryRowContext(ctx, `SELECT flow_snapshot_json FROM workflow_runs WHERE id = ?`, runID).Scan(&snapshotJSON); err != nil {
		return WorkflowArtifact{}, false, err
	}
	snapshot, err := decodeFlowSnapshot([]byte(snapshotJSON))
	if err != nil {
		return WorkflowArtifact{}, false, fmt.Errorf("decode workflow snapshot: %w", err)
	}
	node, ok := snapshot.Node(nodeKey)
	if !ok || node.Kind != NodeAgent || node.Config.Agent == nil {
		return WorkflowArtifact{}, false, errors.New("only agent nodes may create workflow artifacts")
	}
	if node.Config.Agent.Artifact != input.Kind {
		return WorkflowArtifact{}, false, fmt.Errorf("node %q requires a %s artifact", nodeKey, node.Config.Agent.Artifact)
	}
	if input.Kind == ArtifactTaskSet {
		config, found, err := taskSetMaterializerConfig(snapshot, nodeKey)
		if err != nil {
			return WorkflowArtifact{}, false, err
		}
		if found {
			if err := validateTaskSetWorkflowSelectionTx(ctx, tx, *taskSetManifest, config); err != nil {
				return WorkflowArtifact{}, false, err
			}
		}
	}
	var reviewPlan *reviewFollowUpPlanContext
	if taskSetManifest != nil && taskSetManifest.ReviewFollowUp != nil {
		reviewPlan, err = s.validateReviewFollowUpPlanTx(ctx, tx, *taskSetManifest, runID, "")
		if err != nil {
			return WorkflowArtifact{}, false, err
		}
	}
	if input.Kind == ArtifactChange {
		var payload struct {
			ChangeID string `json:"change_id"`
		}
		if err := json.Unmarshal(canonicalPayload, &payload); err != nil {
			return WorkflowArtifact{}, false, err
		}
		var owned int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM changes WHERE id = ? AND workflow_run_id = ?`, strings.TrimSpace(payload.ChangeID), runID).Scan(&owned); err != nil {
			return WorkflowArtifact{}, false, err
		}
		if owned == 0 {
			return WorkflowArtifact{}, false, errors.New("change artifact must reference a change owned by the workflow run")
		}
	}
	if input.SessionID != "" {
		var owned int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sessions
WHERE id = ? AND workflow_run_id = ? AND node_run_id = ?`, input.SessionID, runID, input.NodeRunID).Scan(&owned); err != nil {
			return WorkflowArtifact{}, false, err
		}
		if owned == 0 {
			return WorkflowArtifact{}, false, errors.New("session does not own the active node run")
		}
	}

	id, err := randomPrefixedID("wa")
	if err != nil {
		return WorkflowArtifact{}, false, err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, session_id, creator_key, kind,
	summary_markdown, payload_json, payload_sha256, base_revision, client_key, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, runID, input.NodeRunID,
		sqlitex.NullableNonEmptyString(input.SessionID), input.CreatorKey, string(input.Kind),
		input.SummaryMarkdown, nullableRawJSON(canonicalPayload), digest,
		sqlitex.NullableNonEmptyString(input.BaseRevision), input.ClientKey, sqlitex.FormatTime(now)); err != nil {
		return WorkflowArtifact{}, false, err
	}
	if reviewPlan != nil {
		nowText := sqlitex.FormatTime(now)
		if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET plan_artifact_id = ?, plan_sha256 = ?, state = 'awaiting_review', updated_at = ?
WHERE id = ? AND plan_artifact_id IS NULL`, id, digest, nowText, reviewPlan.PlanRevisionID); err != nil {
			return WorkflowArtifact{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET active_plan_artifact_id = ?, state = 'awaiting_review', updated_at = ?
WHERE id = ? AND revision = ?`, id, nowText, reviewPlan.SetID, reviewPlan.SetRevision); err != nil {
			return WorkflowArtifact{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowArtifact{}, false, err
	}
	artifact, err := s.Get(ctx, id)
	return artifact, false, err
}

func (s *WorkflowArtifactService) Get(ctx context.Context, id string) (WorkflowArtifact, error) {
	artifact, found, err := scanWorkflowArtifactMaybe(s.db.QueryRowContext(ctx, workflowArtifactSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if err != nil {
		return WorkflowArtifact{}, err
	}
	if !found {
		return WorkflowArtifact{}, ErrWorkflowArtifactNotFound
	}
	return artifact, nil
}

func (s *WorkflowArtifactService) ListForRun(ctx context.Context, runID string) ([]WorkflowArtifact, error) {
	rows, err := s.db.QueryContext(ctx, workflowArtifactSelect+` WHERE workflow_run_id = ? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WorkflowArtifact
	for rows.Next() {
		artifact, _, err := scanWorkflowArtifactMaybe(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func canonicalArtifactPayload(kind ArtifactKind, raw json.RawMessage) ([]byte, error) {
	switch kind {
	case ArtifactHandoff:
		if len(raw) == 0 {
			return nil, nil
		}
	case ArtifactChange, ArtifactTaskSet:
		if len(raw) == 0 {
			return nil, fmt.Errorf("%s artifact payload is required", kind)
		}
	default:
		return nil, fmt.Errorf("invalid artifact kind %q", kind)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode artifact payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("artifact payload must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("decode artifact payload: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode artifact payload: %w", err)
	}
	if kind == ArtifactChange {
		var payload struct {
			ChangeID string `json:"change_id"`
			HeadSHA  string `json:"head_sha"`
		}
		if err := json.Unmarshal(canonical, &payload); err != nil || strings.TrimSpace(payload.ChangeID) == "" || strings.TrimSpace(payload.HeadSHA) == "" {
			return nil, errors.New("change artifact payload requires change_id and head_sha")
		}
	}
	if kind == ArtifactTaskSet {
		if _, err := DecodeTaskSetManifest(canonical); err != nil {
			return nil, err
		}
	}
	return canonical, nil
}

func artifactDigest(kind ArtifactKind, summary string, payload []byte, baseRevision string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(summary))
	h.Write([]byte{0})
	h.Write(payload)
	h.Write([]byte{0})
	h.Write([]byte(baseRevision))
	return hex.EncodeToString(h.Sum(nil))
}

func nullableRawJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

const workflowArtifactSelect = `
SELECT id, workflow_run_id, node_run_id, session_id, kind, summary_markdown,
	payload_json, payload_sha256, base_revision, client_key, created_at
FROM workflow_artifacts`

func scanWorkflowArtifactMaybe(scanner taskScanner) (WorkflowArtifact, bool, error) {
	var artifact WorkflowArtifact
	var sessionID, payload, baseRevision sql.NullString
	var kind, createdAt string
	if err := scanner.Scan(&artifact.ID, &artifact.WorkflowRunID, &artifact.NodeRunID, &sessionID,
		&kind, &artifact.SummaryMarkdown, &payload, &artifact.PayloadSHA256, &baseRevision,
		&artifact.ClientKey, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowArtifact{}, false, nil
		}
		return WorkflowArtifact{}, false, err
	}
	artifact.SessionID = sessionID.String
	artifact.Kind = ArtifactKind(kind)
	artifact.BaseRevision = baseRevision.String
	if payload.Valid {
		artifact.Payload = json.RawMessage(payload.String)
	}
	parsed, err := sqlitex.ParseTime(createdAt)
	if err != nil {
		return WorkflowArtifact{}, false, err
	}
	artifact.CreatedAt = parsed
	return artifact, true, nil
}

const reviewFollowUpRationaleMaxBytes = 4096

type ReviewFollowUpDisposition string

const (
	ReviewFollowUpDispositionCreateTask        ReviewFollowUpDisposition = "create_task"
	ReviewFollowUpDispositionUseExistingTask   ReviewFollowUpDisposition = "use_existing_task"
	ReviewFollowUpDispositionMergeWithProposal ReviewFollowUpDisposition = "merge_with_proposal"
	ReviewFollowUpDispositionCoveredBySource   ReviewFollowUpDisposition = "covered_by_source"
	ReviewFollowUpDispositionDiscardDuplicate  ReviewFollowUpDisposition = "discard_duplicate"
)

type TaskSetManifest struct {
	SchemaVersion  int                    `json:"schema_version"`
	Items          []TaskSetItem          `json:"items"`
	Dependencies   []TaskSetDependency    `json:"dependencies,omitempty"`
	ReviewFollowUp *TaskSetReviewFollowUp `json:"review_follow_up,omitempty"`
}

type TaskSetItem struct {
	Key              string               `json:"key"`
	Kind             WorkItemKind         `json:"kind"`
	ExistingTaskID   string               `json:"existing_task_id,omitempty"`
	ParentKey        string               `json:"parent_key,omitempty"`
	Title            string               `json:"title,omitempty"`
	Body             string               `json:"body,omitempty"`
	Priority         int                  `json:"priority,omitempty"`
	TagSlugs         []string             `json:"tag_slugs,omitempty"`
	FlowID           string               `json:"flow_id,omitempty"`
	CompletionPolicy EpicCompletionPolicy `json:"completion_policy,omitempty"`
}

type TaskSetReviewFollowUp struct {
	SetID       string                            `json:"set_id"`
	SetRevision int                               `json:"set_revision"`
	Assignments []TaskSetReviewFollowUpAssignment `json:"assignments"`
}

type TaskSetReviewFollowUpAssignment struct {
	ProposalID          string                    `json:"proposal_id"`
	Disposition         ReviewFollowUpDisposition `json:"disposition"`
	ItemKey             string                    `json:"item_key,omitempty"`
	TargetTaskID        string                    `json:"target_task_id,omitempty"`
	CanonicalProposalID string                    `json:"canonical_proposal_id,omitempty"`
	Rationale           string                    `json:"rationale"`
}

type TaskSetDependency struct {
	Blocker string `json:"blocker"`
	Blocked string `json:"blocked"`
}

type MaterializeTaskSetResult struct {
	RootEpicID string            `json:"root_epic_id"`
	ItemIDs    map[string]string `json:"item_ids"`
	TaskIDs    map[string]string `json:"task_ids,omitempty"`
}

func (s *WorkflowArtifactService) MaterializeTaskSet(ctx context.Context, artifactID string, config MaterializeTaskSetNodeConfig) (MaterializeTaskSetResult, bool, error) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return MaterializeTaskSetResult{}, false, errors.New("artifact id is required")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	defer tx.Rollback()
	artifact, found, err := scanWorkflowArtifactMaybe(tx.QueryRowContext(ctx, workflowArtifactSelect+` WHERE id = ?`, artifactID))
	if err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	if !found {
		return MaterializeTaskSetResult{}, false, ErrWorkflowArtifactNotFound
	}
	if artifact.Kind != ArtifactTaskSet {
		return MaterializeTaskSetResult{}, false, errors.New("materialization requires an task_set artifact")
	}
	manifest, err := DecodeTaskSetManifest(artifact.Payload)
	if err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	config = normalizedTaskSetMaterializerConfig(config)
	if err := validateTaskSetWorkflowSelectionTx(ctx, tx, manifest, config); err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	result := newMaterializeTaskSetResult()
	replayed := false
	var materializationState, resultJSON string
	err = tx.QueryRowContext(ctx, `
SELECT state, result_json FROM workflow_materializations WHERE artifact_id = ?`, artifactID).Scan(&materializationState, &resultJSON)
	if err == nil {
		replayed = true
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
		normalizeMaterializeTaskSetResult(&result)
		if materializationState == "completed" {
			return result, true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MaterializeTaskSetResult{}, false, err
	}

	reviewPlan, err := s.validateReviewFollowUpPlanTx(ctx, tx, manifest, artifact.WorkflowRunID, artifact.ID)
	if err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	var sourceTaskID, sourceChangeID string
	if reviewPlan != nil {
		sourceTaskID = reviewPlan.SourceTaskID
		sourceChangeID = reviewPlan.SourceChangeID
	} else if err := tx.QueryRowContext(ctx, `SELECT task_id FROM workflow_runs WHERE id = ?`, artifact.WorkflowRunID).Scan(&sourceTaskID); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	var sourceTitle string
	var sourceFeatureID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT title, feature_id FROM tasks WHERE id = ?`, sourceTaskID).Scan(&sourceTitle, &sourceFeatureID); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	for _, item := range manifest.Items {
		if item.Kind == WorkItemFeature && item.ExistingTaskID == "" && s.Features == nil {
			return MaterializeTaskSetResult{}, replayed, errors.New("feature service is required to materialize feature items")
		}
		if item.ExistingTaskID == "" {
			continue
		}
		if existing := result.ItemIDs[item.Key]; existing != "" && existing != item.ExistingTaskID {
			return MaterializeTaskSetResult{}, replayed, fmt.Errorf("materialized item %q changed id from %s to %s", item.Key, existing, item.ExistingTaskID)
		}
		result.ItemIDs[item.Key] = item.ExistingTaskID
		result.TaskIDs[item.Key] = item.ExistingTaskID
	}
	if replayed {
		if err := storeMaterializationResultTx(ctx, tx, artifactID, result, "prepared", ""); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
	} else {
		nowText := sqlitex.FormatTime(s.now().UTC())
		resultJSON, _ := json.Marshal(result)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_materializations (
	artifact_id, workflow_run_id, state, result_json, created_at, updated_at
) VALUES (?, ?, 'prepared', ?, ?, ?)`, artifact.ID, artifact.WorkflowRunID, string(resultJSON), nowText, nowText); err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
	}
	if reviewPlan != nil {
		nowText := sqlitex.FormatTime(s.now().UTC())
		if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions SET state = 'materializing', updated_at = ? WHERE id = ?`, nowText, reviewPlan.PlanRevisionID); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_sets SET state = 'materializing', updated_at = ?
WHERE id = ? AND revision = ?`, nowText, reviewPlan.SetID, reviewPlan.SetRevision); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MaterializeTaskSetResult{}, false, err
	}

	if result.RootEpicID == "" {
		tx, err = sqlitex.BeginImmediate(ctx, s.db)
		if err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
		defer tx.Rollback()
		if err := loadMaterializationResultTx(ctx, tx, artifactID, &result); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
		if result.RootEpicID == "" {
			id, err := s.epics.allocateID(ctx, tx)
			if err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
			now := s.now().UTC()
			nowText := sqlitex.FormatTime(now)
			if err := insertWorkItem(ctx, tx, id, WorkItemEpic, nowText); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO epics (
	id, title, body, priority, status, completion_policy, completed_automatically,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, 0, 'open', 'all_children', 0, 'system', ?, ?)`,
				id, "Plan: "+sourceTitle, artifact.SummaryMarkdown, nowText, nowText); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
			if sourceFeatureID.Valid {
				if err := s.items.linkTx(ctx, tx, sourceFeatureID.String, id, RelationParentOf, ActorSystem); err != nil {
					return MaterializeTaskSetResult{}, replayed, err
				}
			}
			if err := s.items.linkTx(ctx, tx, sourceTaskID, id, RelationRelatedTo, ActorSystem); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
			result.RootEpicID = id
			if err := reconcileEpicAncestorsTx(ctx, tx, []string{id}, now); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
			if err := storeMaterializationResultTx(ctx, tx, artifactID, result, "prepared", ""); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
	}

	remaining := len(manifest.Items) - len(result.ItemIDs)
	for remaining > 0 {
		progress := false
		for _, item := range manifest.Items {
			if result.ItemIDs[item.Key] != "" {
				continue
			}
			parentID := result.RootEpicID
			if item.ParentKey != "" {
				parentID = result.ItemIDs[item.ParentKey]
				if parentID == "" {
					continue
				}
			}
			var id string
			if item.Kind == WorkItemFeature {
				if s.Features == nil {
					return MaterializeTaskSetResult{}, replayed, errors.New("feature service is required to materialize feature items")
				}
				feature, err := s.Features.Create(ctx, CreateFeatureInput{
					Title: item.Title, Body: item.Body, ParentItemID: parentID,
					OperationKey: "materialization:" + artifactID + ":" + item.Key, CreatedBy: ActorSystem,
				})
				if err != nil {
					_ = s.storeMaterializationError(ctx, artifactID, err)
					return MaterializeTaskSetResult{}, replayed, err
				}
				id = feature.ID
				if err := s.persistMaterializedItem(ctx, artifactID, item, id, &result); err != nil {
					return MaterializeTaskSetResult{}, replayed, err
				}
			} else {
				id, err = s.materializeDatabaseItem(ctx, artifact, config, item, parentID, sourceTaskID, sourceChangeID, artifactID, &result)
				if err != nil {
					_ = s.storeMaterializationError(ctx, artifactID, err)
					return MaterializeTaskSetResult{}, replayed, err
				}
			}
			result.ItemIDs[item.Key] = id
			if item.Kind == WorkItemTask {
				result.TaskIDs[item.Key] = id
			}
			remaining--
			progress = true
		}
		if !progress {
			return MaterializeTaskSetResult{}, replayed, errors.New("task-set hierarchy could not be materialized")
		}
	}

	tx, err = sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	defer tx.Rollback()
	if err := loadMaterializationResultTx(ctx, tx, artifactID, &result); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	if reviewPlan != nil {
		if _, err := s.validateReviewFollowUpPlanTx(ctx, tx, manifest, artifact.WorkflowRunID, artifact.ID); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
	}
	for _, dependency := range manifest.Dependencies {
		sourceID, targetID := result.ItemIDs[dependency.Blocker], result.ItemIDs[dependency.Blocked]
		var exists int
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks')`, sourceID, targetID).Scan(&exists); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
		if exists == 0 {
			if err := s.items.linkTx(ctx, tx, sourceID, targetID, RelationBlocks, ActorSystem); err != nil {
				return MaterializeTaskSetResult{}, replayed, err
			}
		}
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, append([]string{result.RootEpicID}, mapValues(result.ItemIDs)...), s.now().UTC()); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	if reviewPlan != nil {
		if err := s.persistReviewFollowUpDispositionsTx(ctx, tx, manifest, *reviewPlan, artifact, result); err != nil {
			return MaterializeTaskSetResult{}, replayed, err
		}
	}
	if err := storeMaterializationResultTx(ctx, tx, artifactID, result, "completed", ""); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MaterializeTaskSetResult{}, replayed, err
	}
	return result, replayed, nil
}

func (s *WorkflowArtifactService) persistReviewFollowUpDispositionsTx(
	ctx context.Context,
	tx workItemRelationQuerier,
	manifest TaskSetManifest,
	plan reviewFollowUpPlanContext,
	artifact WorkflowArtifact,
	result MaterializeTaskSetResult,
) error {
	review := manifest.ReviewFollowUp
	assignments := make(map[string]TaskSetReviewFollowUpAssignment, len(review.Assignments))
	for _, assignment := range review.Assignments {
		assignments[assignment.ProposalID] = assignment
	}
	resolveTarget := func(assignment TaskSetReviewFollowUpAssignment) string {
		switch assignment.Disposition {
		case ReviewFollowUpDispositionCreateTask:
			return result.ItemIDs[assignment.ItemKey]
		case ReviewFollowUpDispositionUseExistingTask:
			return assignment.TargetTaskID
		case ReviewFollowUpDispositionCoveredBySource:
			return plan.SourceTaskID
		case ReviewFollowUpDispositionMergeWithProposal, ReviewFollowUpDispositionDiscardDuplicate:
			canonical := assignments[assignment.CanonicalProposalID]
			if canonical.Disposition == ReviewFollowUpDispositionCreateTask {
				return result.ItemIDs[canonical.ItemKey]
			}
			return canonical.TargetTaskID
		default:
			return ""
		}
	}
	nowText := sqlitex.FormatTime(s.now().UTC())
	for _, assignment := range review.Assignments {
		targetTaskID := resolveTarget(assignment)
		if targetTaskID == "" {
			return fmt.Errorf("proposal %q has no resolved disposition target", assignment.ProposalID)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_dispositions (
	proposal_id, plan_revision_id, disposition, item_key, target_task_id,
	canonical_proposal_id, rationale, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, assignment.ProposalID, plan.PlanRevisionID,
			string(assignment.Disposition), sqlitex.NullableNonEmptyString(assignment.ItemKey), targetTaskID,
			sqlitex.NullableNonEmptyString(assignment.CanonicalProposalID), assignment.Rationale, nowText, nowText); err != nil {
			return fmt.Errorf("persist proposal %q disposition: %w", assignment.ProposalID, err)
		}
		updated, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_proposals SET state = 'dispositioned', updated_at = ?
WHERE id = ? AND state = 'active'`, nowText, assignment.ProposalID)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil || count != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("proposal %q is no longer active", assignment.ProposalID)
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `
UPDATE review_follow_up_plan_revisions
SET state = 'materialized', materialization_result_json = ?, materialization_error = '', updated_at = ?
WHERE id = ? AND plan_artifact_id = ?`, string(resultJSON), nowText, plan.PlanRevisionID, artifact.ID)
	if err != nil {
		return err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errors.New("review_follow_up plan revision changed during materialization")
	}
	updated, err = tx.ExecContext(ctx, `
UPDATE review_follow_up_sets
SET state = 'materialized', active_plan_artifact_id = ?, last_error = '', updated_at = ?
WHERE id = ? AND revision = ?`, artifact.ID, nowText, plan.SetID, plan.SetRevision)
	if err != nil {
		return err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errors.New("review_follow_up set revision changed during materialization")
	}
	return nil
}

func newMaterializeTaskSetResult() MaterializeTaskSetResult {
	return MaterializeTaskSetResult{ItemIDs: map[string]string{}, TaskIDs: map[string]string{}}
}

func normalizeMaterializeTaskSetResult(result *MaterializeTaskSetResult) {
	if result.ItemIDs == nil {
		result.ItemIDs = map[string]string{}
	}
	if result.TaskIDs == nil {
		result.TaskIDs = map[string]string{}
	}
	for key, id := range result.TaskIDs {
		if result.ItemIDs[key] == "" {
			result.ItemIDs[key] = id
		}
	}
}

func loadMaterializationResultTx(ctx context.Context, q workItemRelationQuerier, artifactID string, result *MaterializeTaskSetResult) error {
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT result_json FROM workflow_materializations WHERE artifact_id = ?`, artifactID).Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), result); err != nil {
		return err
	}
	normalizeMaterializeTaskSetResult(result)
	return nil
}

func storeMaterializationResultTx(ctx context.Context, q workItemRelationQuerier, artifactID string, result MaterializeTaskSetResult, state, lastError string) error {
	normalizeMaterializeTaskSetResult(&result)
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
UPDATE workflow_materializations
SET state = ?, result_json = ?, last_error = ?, updated_at = ?
WHERE artifact_id = ?`, state, string(raw), lastError, sqlitex.FormatTime(sqlitex.UTCNow()), artifactID)
	return err
}

func (s *WorkflowArtifactService) storeMaterializationError(ctx context.Context, artifactID string, cause error) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE workflow_materializations SET last_error = ?, updated_at = ? WHERE artifact_id = ?`,
		cause.Error(), sqlitex.FormatTime(s.now().UTC()), artifactID)
	return err
}

func (s *WorkflowArtifactService) persistMaterializedItem(ctx context.Context, artifactID string, item TaskSetItem, id string, result *MaterializeTaskSetResult) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := loadMaterializationResultTx(ctx, tx, artifactID, result); err != nil {
		return err
	}
	if existing := result.ItemIDs[item.Key]; existing != "" && existing != id {
		return fmt.Errorf("materialized item %q changed id from %s to %s", item.Key, existing, id)
	}
	result.ItemIDs[item.Key] = id
	if item.Kind == WorkItemTask {
		result.TaskIDs[item.Key] = id
	}
	if err := storeMaterializationResultTx(ctx, tx, artifactID, *result, "prepared", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *WorkflowArtifactService) materializeDatabaseItem(
	ctx context.Context,
	artifact WorkflowArtifact,
	config MaterializeTaskSetNodeConfig,
	item TaskSetItem,
	parentID string,
	sourceTaskID string,
	sourceChangeID string,
	artifactID string,
	result *MaterializeTaskSetResult,
) (string, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := loadMaterializationResultTx(ctx, tx, artifactID, result); err != nil {
		return "", err
	}
	if id := result.ItemIDs[item.Key]; id != "" {
		return id, tx.Commit(ctx)
	}
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	createdBy := ActorSystem
	var sessionID any
	if artifact.SessionID != "" {
		createdBy = ActorAgent
		sessionID = artifact.SessionID
	}
	var id string
	switch item.Kind {
	case WorkItemTask:
		id, err = s.tasks.allocateTaskID(ctx, tx)
		if err != nil {
			return "", err
		}
		flowID := strings.TrimSpace(item.FlowID)
		if flowID == "" {
			flowID = config.DefaultChildFlowID
		}
		featureID, err := nearestFeatureFromParentTx(ctx, tx, parentID)
		if err != nil {
			return "", err
		}
		if err := insertWorkItem(ctx, tx, id, WorkItemTask, nowText); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, flow_id, feature_id, created_by, created_by_session_id,
	source_task_id, source_change_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, item.Title, item.Body, item.Priority,
			flowID, sqlitex.NullableNonEmptyString(featureID), string(createdBy), sessionID, sourceTaskID,
			sqlitex.NullableNonEmptyString(sourceChangeID), nowText, nowText); err != nil {
			return "", fmt.Errorf("create generated task %q: %w", item.Key, err)
		}
		for _, slug := range item.TagSlugs {
			tagID, err := upsertTagInTx(ctx, tx, CreateTagInput{Slug: slug, Name: slug, CreatedBy: ActorSystem}, nowText)
			if err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO task_tags (task_id, tag_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
				id, tagID, string(ActorSystem), nowText); err != nil {
				return "", err
			}
		}
	case WorkItemEpic:
		id, err = s.epics.allocateID(ctx, tx)
		if err != nil {
			return "", err
		}
		if err := insertWorkItem(ctx, tx, id, WorkItemEpic, nowText); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO epics (
	id, title, body, priority, status, completion_policy, completed_automatically,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, 'open', ?, 0, ?, ?, ?)`, id, item.Title, item.Body, item.Priority,
			string(item.CompletionPolicy), string(createdBy), nowText, nowText); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%s item requires external materialization", item.Kind)
	}
	if err := s.items.linkTx(ctx, tx, parentID, id, RelationParentOf, ActorSystem); err != nil {
		return "", err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{id}, now); err != nil {
		return "", err
	}
	result.ItemIDs[item.Key] = id
	if item.Kind == WorkItemTask {
		result.TaskIDs[item.Key] = id
	}
	if err := storeMaterializationResultTx(ctx, tx, artifactID, *result, "prepared", ""); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func DecodeTaskSetManifest(raw []byte) (TaskSetManifest, error) {
	var manifest TaskSetManifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return TaskSetManifest{}, fmt.Errorf("decode task-set manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return TaskSetManifest{}, errors.New("task-set manifest must contain exactly one JSON value")
		}
		return TaskSetManifest{}, fmt.Errorf("decode task-set manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return TaskSetManifest{}, errors.New("task-set schema_version must be 1")
	}
	if len(manifest.Items) == 0 {
		return TaskSetManifest{}, errors.New("task-set requires at least one item")
	}
	if len(manifest.Dependencies) > 200 {
		return TaskSetManifest{}, errors.New("task-set may contain at most 200 dependencies")
	}

	seen := map[string]bool{}
	kinds := make(map[string]WorkItemKind, len(manifest.Items))
	items := make(map[string]TaskSetItem, len(manifest.Items))
	for i := range manifest.Items {
		item := &manifest.Items[i]
		item.Key = strings.TrimSpace(item.Key)
		item.ExistingTaskID = strings.TrimSpace(item.ExistingTaskID)
		item.ParentKey = strings.TrimSpace(item.ParentKey)
		item.Title = strings.TrimSpace(item.Title)
		item.FlowID = strings.TrimSpace(item.FlowID)
		if !flowNodeKeyPattern.MatchString(item.Key) {
			return TaskSetManifest{}, fmt.Errorf("item %d key %q is invalid", i+1, item.Key)
		}
		if seen[item.Key] {
			return TaskSetManifest{}, fmt.Errorf("duplicate item key %q", item.Key)
		}
		seen[item.Key] = true

		if item.ExistingTaskID != "" {
			if item.Kind == "" {
				item.Kind = WorkItemTask
			}
			if item.Kind != WorkItemTask {
				return TaskSetManifest{}, fmt.Errorf("existing item %q must have task kind", item.Key)
			}
			if item.ParentKey != "" || item.Title != "" || strings.TrimSpace(item.Body) != "" || item.Priority != 0 ||
				len(item.TagSlugs) != 0 || item.FlowID != "" || item.CompletionPolicy != "" {
				return TaskSetManifest{}, fmt.Errorf("existing task item %q may only specify key, kind, and existing_task_id", item.Key)
			}
			kinds[item.Key] = item.Kind
			items[item.Key] = *item
			continue
		}

		if item.Kind != WorkItemTask && item.Kind != WorkItemEpic && item.Kind != WorkItemFeature {
			return TaskSetManifest{}, fmt.Errorf("item %q has invalid kind %q", item.Key, item.Kind)
		}
		kinds[item.Key] = item.Kind
		if item.Title == "" || strings.TrimSpace(item.Body) == "" {
			return TaskSetManifest{}, fmt.Errorf("item %q requires title and body", item.Key)
		}
		if item.Priority < 0 {
			return TaskSetManifest{}, fmt.Errorf("item %q priority must be non-negative", item.Key)
		}
		if item.Kind != WorkItemTask && (item.FlowID != "" || len(item.TagSlugs) != 0) {
			return TaskSetManifest{}, fmt.Errorf("%s %q cannot set task flow or tags", item.Kind, item.Key)
		}
		if item.Kind == WorkItemFeature && item.Priority != 0 {
			return TaskSetManifest{}, fmt.Errorf("feature %q cannot set priority", item.Key)
		}
		if item.Kind == WorkItemEpic {
			if item.CompletionPolicy == "" {
				item.CompletionPolicy = EpicAllChildren
			}
			if item.CompletionPolicy != EpicAllChildren && item.CompletionPolicy != EpicManual {
				return TaskSetManifest{}, fmt.Errorf("epic %q has invalid completion policy", item.Key)
			}
		} else if item.CompletionPolicy != "" {
			return TaskSetManifest{}, fmt.Errorf("%s %q cannot set completion policy", item.Kind, item.Key)
		}
		seenTags := map[string]bool{}
		for j := range item.TagSlugs {
			item.TagSlugs[j] = strings.TrimSpace(item.TagSlugs[j])
			if item.TagSlugs[j] == "" {
				return TaskSetManifest{}, fmt.Errorf("item %q contains an empty tag slug", item.Key)
			}
			if seenTags[item.TagSlugs[j]] {
				return TaskSetManifest{}, fmt.Errorf("item %q repeats tag slug %q", item.Key, item.TagSlugs[j])
			}
			seenTags[item.TagSlugs[j]] = true
		}
		items[item.Key] = *item
	}

	dependencyEdges := map[string][]string{}
	for _, item := range manifest.Items {
		if item.ParentKey == "" {
			continue
		}
		parentKind, ok := kinds[item.ParentKey]
		if !ok {
			return TaskSetManifest{}, fmt.Errorf("item %q parent_key references unknown item %q", item.Key, item.ParentKey)
		}
		if parentKind != WorkItemEpic && parentKind != WorkItemFeature {
			return TaskSetManifest{}, fmt.Errorf("item %q parent %q cannot contain children", item.Key, item.ParentKey)
		}
		dependencyEdges[item.Key] = append(dependencyEdges[item.Key], item.ParentKey)
	}
	seenDependencies := map[string]bool{}
	for i := range manifest.Dependencies {
		dependency := &manifest.Dependencies[i]
		dependency.Blocker = strings.TrimSpace(dependency.Blocker)
		dependency.Blocked = strings.TrimSpace(dependency.Blocked)
		if !seen[dependency.Blocker] || !seen[dependency.Blocked] {
			return TaskSetManifest{}, fmt.Errorf("dependency %q -> %q references an unknown item", dependency.Blocker, dependency.Blocked)
		}
		if dependency.Blocker == dependency.Blocked {
			return TaskSetManifest{}, fmt.Errorf("item %q cannot block itself", dependency.Blocker)
		}
		edgeKey := dependency.Blocker + "\x00" + dependency.Blocked
		if seenDependencies[edgeKey] {
			return TaskSetManifest{}, fmt.Errorf("duplicate dependency %q -> %q", dependency.Blocker, dependency.Blocked)
		}
		seenDependencies[edgeKey] = true
		dependencyEdges[dependency.Blocker] = append(dependencyEdges[dependency.Blocker], dependency.Blocked)
	}
	if manifest.ReviewFollowUp != nil {
		if err := normalizeTaskSetReviewFollowUp(manifest.ReviewFollowUp, items); err != nil {
			return TaskSetManifest{}, err
		}
	}

	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(key string) bool {
		if visiting[key] {
			return true
		}
		if visited[key] {
			return false
		}
		visiting[key] = true
		for _, next := range dependencyEdges[key] {
			if visit(next) {
				return true
			}
		}
		visiting[key] = false
		visited[key] = true
		return false
	}
	for key := range seen {
		if visit(key) {
			return TaskSetManifest{}, errors.New("task-set containment and dependencies must be acyclic")
		}
	}
	return manifest, nil
}

func normalizeTaskSetReviewFollowUp(review *TaskSetReviewFollowUp, items map[string]TaskSetItem) error {
	review.SetID = strings.TrimSpace(review.SetID)
	if review.SetID == "" || review.SetRevision <= 0 {
		return errors.New("review_follow_up requires set_id and a positive set_revision")
	}
	if len(review.Assignments) == 0 {
		return errors.New("review_follow_up requires at least one assignment")
	}
	assignments := make(map[string]TaskSetReviewFollowUpAssignment, len(review.Assignments))
	usedItemKeys := map[string]bool{}
	for i := range review.Assignments {
		assignment := &review.Assignments[i]
		assignment.ProposalID = strings.TrimSpace(assignment.ProposalID)
		assignment.ItemKey = strings.TrimSpace(assignment.ItemKey)
		assignment.TargetTaskID = strings.TrimSpace(assignment.TargetTaskID)
		assignment.CanonicalProposalID = strings.TrimSpace(assignment.CanonicalProposalID)
		assignment.Rationale = strings.TrimSpace(assignment.Rationale)
		if assignment.ProposalID == "" {
			return fmt.Errorf("review_follow_up assignment %d requires proposal_id", i+1)
		}
		if _, duplicate := assignments[assignment.ProposalID]; duplicate {
			return fmt.Errorf("duplicate review_follow_up assignment for proposal %q", assignment.ProposalID)
		}
		if assignment.Rationale == "" {
			return fmt.Errorf("proposal %q assignment requires rationale", assignment.ProposalID)
		}
		if len(assignment.Rationale) > reviewFollowUpRationaleMaxBytes {
			return fmt.Errorf("proposal %q rationale exceeds %d bytes", assignment.ProposalID, reviewFollowUpRationaleMaxBytes)
		}
		switch assignment.Disposition {
		case ReviewFollowUpDispositionCreateTask:
			item, ok := items[assignment.ItemKey]
			if !ok {
				return fmt.Errorf("proposal %q references unknown item_key %q", assignment.ProposalID, assignment.ItemKey)
			}
			if assignment.ItemKey == "" || item.Kind != WorkItemTask || item.ExistingTaskID != "" ||
				assignment.TargetTaskID != "" || assignment.CanonicalProposalID != "" {
				return fmt.Errorf("proposal %q has invalid create_task assignment shape", assignment.ProposalID)
			}
			if usedItemKeys[assignment.ItemKey] {
				return fmt.Errorf("multiple create_task assignments reference item_key %q", assignment.ItemKey)
			}
			usedItemKeys[assignment.ItemKey] = true
		case ReviewFollowUpDispositionUseExistingTask:
			if assignment.TargetTaskID == "" || assignment.ItemKey != "" || assignment.CanonicalProposalID != "" {
				return fmt.Errorf("proposal %q has invalid use_existing_task assignment shape", assignment.ProposalID)
			}
		case ReviewFollowUpDispositionMergeWithProposal, ReviewFollowUpDispositionDiscardDuplicate:
			if assignment.CanonicalProposalID == "" || assignment.CanonicalProposalID == assignment.ProposalID ||
				assignment.ItemKey != "" || assignment.TargetTaskID != "" {
				return fmt.Errorf("proposal %q has invalid %s assignment shape", assignment.ProposalID, assignment.Disposition)
			}
		case ReviewFollowUpDispositionCoveredBySource:
			if assignment.ItemKey != "" || assignment.TargetTaskID != "" || assignment.CanonicalProposalID != "" {
				return fmt.Errorf("proposal %q has invalid covered_by_source assignment shape", assignment.ProposalID)
			}
		default:
			return fmt.Errorf("proposal %q has invalid disposition %q", assignment.ProposalID, assignment.Disposition)
		}
		assignments[assignment.ProposalID] = *assignment
	}
	for _, assignment := range review.Assignments {
		if assignment.CanonicalProposalID == "" {
			continue
		}
		canonical, ok := assignments[assignment.CanonicalProposalID]
		if !ok || (canonical.Disposition != ReviewFollowUpDispositionCreateTask && canonical.Disposition != ReviewFollowUpDispositionUseExistingTask) {
			return fmt.Errorf("proposal %q has invalid canonical_proposal_id %q", assignment.ProposalID, assignment.CanonicalProposalID)
		}
	}
	return nil
}

type reviewFollowUpPlanContext struct {
	PlanRevisionID string
	SetID          string
	SetRevision    int
	SourceTaskID   string
	SourceChangeID string
}

func (s *WorkflowArtifactService) validateReviewFollowUpPlanTx(
	ctx context.Context,
	q workItemRelationQuerier,
	manifest TaskSetManifest,
	workflowRunID string,
	artifactID string,
) (*reviewFollowUpPlanContext, error) {
	review := manifest.ReviewFollowUp
	if review == nil {
		return nil, nil
	}
	var plan reviewFollowUpPlanContext
	var currentRevision int
	var organizerTaskID, organizerWorkflowRunID, planArtifactID, runTaskID string
	if err := q.QueryRowContext(ctx, `
SELECT pr.id, pr.set_id, pr.set_revision, s.revision, s.source_task_id, s.source_change_id,
       COALESCE(pr.organizer_task_id, ''), COALESCE(pr.organizer_workflow_run_id, ''),
       COALESCE(pr.plan_artifact_id, ''), COALESCE(wr.task_id, '')
FROM review_follow_up_plan_revisions pr
JOIN review_follow_up_sets s ON s.id = pr.set_id
LEFT JOIN workflow_runs wr ON wr.id = pr.organizer_workflow_run_id
WHERE pr.set_id = ? AND pr.set_revision = ?`, review.SetID, review.SetRevision).Scan(
		&plan.PlanRevisionID, &plan.SetID, &plan.SetRevision, &currentRevision,
		&plan.SourceTaskID, &plan.SourceChangeID, &organizerTaskID, &organizerWorkflowRunID,
		&planArtifactID, &runTaskID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("review_follow_up plan revision is not bound")
		}
		return nil, err
	}
	if currentRevision != review.SetRevision {
		return nil, fmt.Errorf("review_follow_up set revision is stale: current revision is %d", currentRevision)
	}
	if organizerTaskID == "" || organizerWorkflowRunID != workflowRunID || runTaskID != organizerTaskID {
		return nil, errors.New("workflow is not the organizer task/workflow bound to review_follow_up plan revision")
	}
	if artifactID == "" {
		if planArtifactID != "" {
			return nil, errors.New("review_follow_up plan revision already has an artifact")
		}
	} else if planArtifactID != artifactID {
		return nil, errors.New("task-set artifact is not bound to review_follow_up plan revision")
	}

	active := map[string]bool{}
	rows, err := q.QueryContext(ctx, `
SELECT p.id
FROM review_follow_up_proposals p
JOIN review_follow_up_batches b ON b.id = p.batch_id
WHERE b.set_id = ? AND p.state = 'active'`, review.SetID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var proposalID string
		if err := rows.Scan(&proposalID); err != nil {
			rows.Close()
			return nil, err
		}
		active[proposalID] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	assigned := make(map[string]bool, len(review.Assignments))
	for _, assignment := range review.Assignments {
		if !active[assignment.ProposalID] {
			return nil, fmt.Errorf("review_follow_up assignment references unknown active proposal %q", assignment.ProposalID)
		}
		assigned[assignment.ProposalID] = true
	}
	for proposalID := range active {
		if !assigned[proposalID] {
			return nil, fmt.Errorf("active proposal %q is missing an assignment", proposalID)
		}
	}

	resolved := make(map[string]string, len(manifest.Items))
	for _, item := range manifest.Items {
		if item.ExistingTaskID == "" {
			continue
		}
		if err := s.requireOpenProjectTaskTx(ctx, q, item.ExistingTaskID); err != nil {
			return nil, fmt.Errorf("existing item %q: %w", item.Key, err)
		}
		resolved[item.Key] = item.ExistingTaskID
	}
	for _, assignment := range review.Assignments {
		if assignment.Disposition == ReviewFollowUpDispositionUseExistingTask {
			if err := s.requireOpenProjectTaskTx(ctx, q, assignment.TargetTaskID); err != nil {
				return nil, fmt.Errorf("proposal %q target: %w", assignment.ProposalID, err)
			}
			if assignment.TargetTaskID == plan.SourceTaskID {
				return nil, fmt.Errorf("proposal %q cannot use the reviewed source task", assignment.ProposalID)
			}
		}
	}
	for _, dependency := range manifest.Dependencies {
		blockerID, blockerKnown := resolved[dependency.Blocker]
		blockedID, blockedKnown := resolved[dependency.Blocked]
		if blockedKnown && blockedID == plan.SourceTaskID {
			return nil, errors.New("dependency target cannot resolve to the reviewed source task")
		}
		if blockerKnown && blockedKnown {
			if blockerID == blockedID {
				return nil, fmt.Errorf("dependency %q -> %q resolves to a self dependency", dependency.Blocker, dependency.Blocked)
			}
			cycle, err := workItemDependencyPathExists(ctx, q, blockedID, blockerID)
			if err != nil {
				return nil, err
			}
			if cycle {
				return nil, fmt.Errorf("dependency %q -> %q would create a cycle", dependency.Blocker, dependency.Blocked)
			}
		}
	}
	return &plan, nil
}

func (s *WorkflowArtifactService) requireOpenProjectTaskTx(ctx context.Context, q workItemRelationQuerier, taskID string) error {
	projectID, ok := ProjectIDFromTaskID(taskID)
	if !ok || s.tasks == nil || s.tasks.projectID == "" || projectID != s.tasks.projectID {
		return fmt.Errorf("task %q is not project-local", taskID)
	}
	var state sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT lifecycle_state FROM tasks WHERE id = ?`, taskID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("task %q does not exist", taskID)
		}
		return err
	}
	if state.Valid && state.String == string(LifecycleDone) {
		return fmt.Errorf("task %q must be open", taskID)
	}
	return nil
}
