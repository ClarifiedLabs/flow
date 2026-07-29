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
	db    *sql.DB
	tasks *TaskService
	now   func() time.Time
}

func NewWorkflowArtifactService(db *sql.DB, tasks *TaskService) *WorkflowArtifactService {
	return &WorkflowArtifactService{db: db, tasks: tasks, now: sqlitex.UTCNow}
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
	if input.Kind == ArtifactTaskSet {
		manifest, err := DecodeTaskSetManifest(canonicalPayload)
		if err != nil {
			return WorkflowArtifact{}, false, err
		}
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
	var snapshot FlowSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
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
			manifest, err := DecodeTaskSetManifest(canonicalPayload)
			if err != nil {
				return WorkflowArtifact{}, false, err
			}
			if err := validateTaskSetWorkflowSelectionTx(ctx, tx, manifest, config); err != nil {
				return WorkflowArtifact{}, false, err
			}
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

type TaskSetManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Tasks         []TaskSetItem       `json:"tasks"`
	Dependencies  []TaskSetDependency `json:"dependencies,omitempty"`
}

type TaskSetItem struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Priority int      `json:"priority,omitempty"`
	TagSlugs []string `json:"tag_slugs,omitempty"`
	FlowID   string   `json:"flow_id,omitempty"`
}

type TaskSetDependency struct {
	Blocker string `json:"blocker"`
	Blocked string `json:"blocked"`
}

type MaterializeTaskSetResult struct {
	TaskIDs map[string]string `json:"task_ids"`
}

func (s *WorkflowArtifactService) MaterializeTaskSet(ctx context.Context, artifactID string, config MaterializeTaskSetNodeConfig) (MaterializeTaskSetResult, bool, error) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return MaterializeTaskSetResult{}, false, errors.New("artifact id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	var existingJSON string
	if err := tx.QueryRowContext(ctx, `SELECT result_json FROM workflow_materializations WHERE artifact_id = ?`, artifactID).Scan(&existingJSON); err == nil {
		var existing MaterializeTaskSetResult
		if err := json.Unmarshal([]byte(existingJSON), &existing); err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
		return existing, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MaterializeTaskSetResult{}, false, err
	}
	var sourceTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM workflow_runs WHERE id = ?`, artifact.WorkflowRunID).Scan(&sourceTaskID); err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	defaultFlowID := config.DefaultChildFlowID

	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	result := MaterializeTaskSetResult{TaskIDs: make(map[string]string, len(manifest.Tasks))}
	for _, item := range manifest.Tasks {
		flowID := strings.TrimSpace(item.FlowID)
		if flowID == "" {
			flowID = defaultFlowID
		}
		id, err := s.tasks.allocateTaskID(ctx, tx)
		if err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
		createdBy := ActorSystem
		var sessionID any
		if artifact.SessionID != "" {
			createdBy = ActorAgent
			sessionID = artifact.SessionID
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, flow_id, created_by, created_by_session_id,
	source_task_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, item.Title, item.Body,
			item.Priority, flowID, string(createdBy), sessionID, sourceTaskID, nowText, nowText); err != nil {
			return MaterializeTaskSetResult{}, false, fmt.Errorf("create generated task %q: %w", item.Key, err)
		}
		if err := linkTasksInTx(ctx, tx, sourceTaskID, id, RelationParentOf, ActorSystem, nowText); err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
		for _, slug := range item.TagSlugs {
			tagID, err := upsertTagInTx(ctx, tx, CreateTagInput{Slug: slug, Name: slug, CreatedBy: ActorSystem}, nowText)
			if err != nil {
				return MaterializeTaskSetResult{}, false, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO task_tags (task_id, tag_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
				id, tagID, string(ActorSystem), nowText); err != nil {
				return MaterializeTaskSetResult{}, false, err
			}
		}
		result.TaskIDs[item.Key] = id
	}
	for _, dependency := range manifest.Dependencies {
		if err := linkTasksInTx(ctx, tx, result.TaskIDs[dependency.Blocker], result.TaskIDs[dependency.Blocked], RelationBlocks, ActorSystem, nowText); err != nil {
			return MaterializeTaskSetResult{}, false, err
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_materializations (artifact_id, workflow_run_id, result_json, created_at)
VALUES (?, ?, ?, ?)`, artifact.ID, artifact.WorkflowRunID, string(resultJSON), nowText); err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MaterializeTaskSetResult{}, false, err
	}
	return result, false, nil
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
	if len(manifest.Tasks) == 0 {
		return TaskSetManifest{}, errors.New("task-set requires at least one task")
	}
	if len(manifest.Dependencies) > 200 {
		return TaskSetManifest{}, errors.New("task-set may contain at most 200 dependencies")
	}
	seen := map[string]bool{}
	for i := range manifest.Tasks {
		item := &manifest.Tasks[i]
		item.Key = strings.TrimSpace(item.Key)
		item.Title = strings.TrimSpace(item.Title)
		item.FlowID = strings.TrimSpace(item.FlowID)
		if !flowNodeKeyPattern.MatchString(item.Key) {
			return TaskSetManifest{}, fmt.Errorf("task %d key %q is invalid", i+1, item.Key)
		}
		if seen[item.Key] {
			return TaskSetManifest{}, fmt.Errorf("duplicate task key %q", item.Key)
		}
		seen[item.Key] = true
		if item.Title == "" || strings.TrimSpace(item.Body) == "" {
			return TaskSetManifest{}, fmt.Errorf("task %q requires title and body", item.Key)
		}
		if item.Priority < 0 {
			return TaskSetManifest{}, fmt.Errorf("task %q priority must be non-negative", item.Key)
		}
		seenTags := map[string]bool{}
		for j := range item.TagSlugs {
			item.TagSlugs[j] = strings.TrimSpace(item.TagSlugs[j])
			if item.TagSlugs[j] == "" {
				return TaskSetManifest{}, fmt.Errorf("task %q contains an empty tag slug", item.Key)
			}
			if seenTags[item.TagSlugs[j]] {
				return TaskSetManifest{}, fmt.Errorf("task %q repeats tag slug %q", item.Key, item.TagSlugs[j])
			}
			seenTags[item.TagSlugs[j]] = true
		}
	}
	dependencyEdges := map[string][]string{}
	seenDependencies := map[string]bool{}
	for i := range manifest.Dependencies {
		dependency := &manifest.Dependencies[i]
		dependency.Blocker = strings.TrimSpace(dependency.Blocker)
		dependency.Blocked = strings.TrimSpace(dependency.Blocked)
		if !seen[dependency.Blocker] || !seen[dependency.Blocked] {
			return TaskSetManifest{}, fmt.Errorf("dependency %q -> %q references an unknown task", dependency.Blocker, dependency.Blocked)
		}
		if dependency.Blocker == dependency.Blocked {
			return TaskSetManifest{}, fmt.Errorf("task %q cannot block itself", dependency.Blocker)
		}
		edgeKey := dependency.Blocker + "\x00" + dependency.Blocked
		if seenDependencies[edgeKey] {
			return TaskSetManifest{}, fmt.Errorf("duplicate dependency %q -> %q", dependency.Blocker, dependency.Blocked)
		}
		seenDependencies[edgeKey] = true
		dependencyEdges[dependency.Blocker] = append(dependencyEdges[dependency.Blocker], dependency.Blocked)
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
			return TaskSetManifest{}, errors.New("task-set dependencies must be acyclic")
		}
	}
	return manifest, nil
}
