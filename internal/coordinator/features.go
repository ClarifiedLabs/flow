package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrFeatureNotFound         = errors.New("feature not found")
	ErrFeatureTitleTaken       = errors.New("a feature with this title already exists")
	ErrFeatureClosed           = errors.New("feature is landed or archived")
	ErrFeatureRebaseRunning    = errors.New("feature rebase already running")
	ErrFeatureCreationConflict = errors.New("feature creation intent has different input")
	ErrNoOpenRebase            = errors.New("no running rebase for feature")
	// ErrFeatureRebaseForbidden rejects a restricted rebase (task-bound
	// console) whose sole-open-task invariant no longer holds: the feature's
	// non-done tasks must be exactly the allowed set. It is raised under the
	// database write lock before any rebase or relation row exists, so a task
	// added concurrently by another principal rejects the rebase atomically.
	ErrFeatureRebaseForbidden = errors.New("feature rebase forbidden: the feature's only non-done task must be the bound task")
)

type FeatureStatus string

const (
	FeatureOpen     FeatureStatus = "open"
	FeatureLanded   FeatureStatus = "landed"
	FeatureArchived FeatureStatus = "archived"
)

// RebaseState is the lifecycle of one feature_rebases row. Exactly one row per
// feature may be running at a time (enforced by a partial unique index).
type RebaseState string

const (
	RebaseRunning   RebaseState = "running"
	RebaseFinalized RebaseState = "finalized"
	RebaseStale     RebaseState = "stale"
	RebaseFailed    RebaseState = "failed"
	RebaseCancelled RebaseState = "cancelled"
)

// RebaseStartKind names the outcome of a RebaseOnMain request.
type RebaseStartKind string

const (
	RebaseAlreadyUpToDate RebaseStartKind = "already_up_to_date"
	RebaseRebased         RebaseStartKind = "rebased"
	RebaseTaskCreated     RebaseStartKind = "rebase_task_created"
)

// Feature groups a set of tasks behind one long-lived feature branch. Tasks
// assigned to a feature branch off and merge back into Branch instead of the
// project base branch.
type Feature struct {
	ID                   string        `json:"id"`
	Title                string        `json:"title"`
	Body                 string        `json:"body"`
	Branch               string        `json:"branch"`
	Status               FeatureStatus `json:"status"`
	IntegrationFeatureID *string       `json:"integration_feature_id,omitempty"`
	CreatedFromSHA       string        `json:"created_from_sha"`
	CreatedBy            Actor         `json:"created_by"`
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
	LandedAt             *time.Time    `json:"landed_at,omitempty"`
	LandSHA              string        `json:"land_sha,omitempty"`
	LandTargetFeatureID  *string       `json:"land_target_feature_id,omitempty"`
	LandTargetBranch     string        `json:"land_target_branch,omitempty"`
	LandTargetSHA        string        `json:"land_target_sha,omitempty"`
}

// FeatureRebase is the durable record of one rebase of a feature branch onto
// the project base: the crash/recovery record and the finalize node's
// compare-and-swap expectation. TaskID is empty for clean instant rebases,
// which never create a system rebase task.
type FeatureRebase struct {
	ID        string `json:"id"`
	FeatureID string `json:"feature_id"`
	TaskID    string `json:"task_id,omitempty"`
	// RestrictBlockedTo records a task-bound console's blocker confinement:
	// when non-empty, the rebase task may link only the comma-joined task IDs
	// as blockers. It is persisted so the schedule-time gate (EnsureRebaseBlock)
	// consults it while the row runs, not just during the initial sweep.
	RestrictBlockedTo string      `json:"restrict_blocked_to,omitempty"`
	OldTipSHA         string      `json:"old_tip_sha"`
	TargetBase        string      `json:"target_base"`
	TargetBaseSHA     string      `json:"target_base_sha"`
	TargetFeatureID   *string     `json:"target_feature_id,omitempty"`
	NewTipSHA         string      `json:"new_tip_sha,omitempty"`
	State             RebaseState `json:"state"`
	CreatedAt         time.Time   `json:"created_at"`
	CompletedAt       *time.Time  `json:"completed_at,omitempty"`
}

// FeatureBranchState is the live divergence of a feature branch against the
// project base, computed on read from the local exchange.
type FeatureBranchState struct {
	BaseTipSHA    string `json:"base_tip_sha"`
	FeatureTipSHA string `json:"feature_tip_sha"`
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
}

// RebaseStartResult reports what a RebaseOnMain request did.
type RebaseStartResult struct {
	Kind         RebaseStartKind `json:"kind"`
	Feature      Feature         `json:"feature"`
	NewTipSHA    string          `json:"new_tip_sha,omitempty"`
	RebaseTaskID string          `json:"rebase_task_id,omitempty"`
}

type CreateFeatureInput struct {
	Title             string
	Body              string
	ParentItemID      string
	WorkItemRelations []CreateWorkItemRelationInput
	OperationKey      string
	CreatedBy         Actor
}

type EditFeatureInput struct {
	Title *string
	Body  *string
}

// RebaseOnMainTestPhase identifies a point inside RebaseOnMain at which the
// test hook runs. Test-only.
//
// The two phases bracket the reviewed guard-to-inner-read window: a feature
// task added by another principal either commits before the locked scope
// decision (RebaseOnMainBeforeScopeCheck) and rejects the rebase with
// ErrFeatureRebaseForbidden, or it commits after the decision but before the
// conflicted path's non-done-task snapshot (RebaseOnMainAfterReservation), so
// it enters the initial relation sweep and only the relation-time restriction
// filter keeps it unlinked.
type RebaseOnMainTestPhase int

const (
	// RebaseOnMainBeforeScopeCheck runs after the Git preflight and before
	// the locked scope decision, when no database transaction is open. A
	// feature task created here commits before the decision, so the rebase is
	// rejected with ErrFeatureRebaseForbidden (the raced 403 path).
	RebaseOnMainBeforeScopeCheck RebaseOnMainTestPhase = iota

	// RebaseOnMainAfterReservation runs after the running rebase row is
	// persisted (its transaction committed) and before the rebase is executed.
	// A feature task created here commits before the conflicted path's
	// non-done-task snapshot, so it enters the initial relation sweep.
	RebaseOnMainAfterReservation
)

type FeatureService struct {
	db      *sql.DB
	tasks   *TaskService
	items   *WorkItemService
	project Project
	now     func() time.Time

	// eventLog receives post-commit feature lifecycle events; nil disables
	// emission (wired through the project bundle).
	eventLog *EventLogService

	// Runs schedules the system rebase task a conflicted rebase creates. It is
	// wired through the project bundle; a nil Runs leaves the task unscheduled
	// (tests schedule it explicitly).
	Runs *WorkflowRunService

	// RebaseOnMainTestHook, when set, runs at the named phase inside
	// RebaseOnMain so tests can inject a feature-task creation by another
	// principal at exactly the reviewed interleavings. Test-only; nil in
	// production.
	RebaseOnMainTestHook func(phase RebaseOnMainTestPhase)

	// CreateAfterIntentCommitTestHook runs after the durable intent transaction
	// commits and before its ref is created. Tests use it to force retry races and
	// pre-existing same-tip refs. Test-only; nil in production.
	CreateAfterIntentCommitTestHook func()
}

func NewFeatureService(database *sql.DB, tasks *TaskService, project Project) *FeatureService {
	if tasks == nil {
		tasks = NewTaskService(database, project.ID)
	}
	return &FeatureService{
		db: database, tasks: tasks, items: NewWorkItemService(database, project.ID),
		project: project, now: sqlitex.UTCNow,
	}
}

// Create allocates the feature id, seeds its branch from the base-branch tip,
// and stores the row. The branch is seeded with a coordinator-local update-ref
// before any other writer can know the ref name.
func (s *FeatureService) Create(ctx context.Context, input CreateFeatureInput) (Feature, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return Feature{}, errors.New("feature title is required")
	}
	body := strings.TrimSpace(input.Body)
	actor := defaultActor(input.CreatedBy, ActorHuman)
	if err := validateActor(actor); err != nil {
		return Feature{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return Feature{}, errors.New("project exchange remote is required for feature branches")
	}
	normalizedPlan, relationPayload, err := normalizeCreateRelations(input.ParentItemID, input.WorkItemRelations, actor)
	if err != nil {
		return Feature{}, err
	}
	parentItemID := normalizedPlan.ParentItemID
	operationKey := strings.TrimSpace(input.OperationKey)
	if operationKey == "" {
		var pendingPayload string
		err = s.db.QueryRowContext(ctx, `
SELECT operation_key, relation_payload_json FROM feature_creation_intents
WHERE state != 'completed'
	AND lower(trim(title)) = lower(trim(?))
	AND body = ?
	AND COALESCE(parent_item_id, '') = ?
ORDER BY created_at LIMIT 1`, title, body, parentItemID).Scan(&operationKey, &pendingPayload)
		if err == nil && pendingPayload != relationPayload {
			return Feature{}, ErrFeatureCreationConflict
		}
		if errors.Is(err, sql.ErrNoRows) {
			operationKey, err = randomPrefixedID("fci")
		}
		if err != nil {
			return Feature{}, err
		}
	}

	var priorID, priorState, priorTitle, priorBody, priorParent, priorPayload, priorBranch, priorTargetSHA string
	err = s.db.QueryRowContext(ctx, `
SELECT id, state, title, body, COALESCE(parent_item_id, ''), relation_payload_json, branch, target_sha
FROM feature_creation_intents WHERE operation_key = ?`, operationKey).Scan(
		&priorID, &priorState, &priorTitle, &priorBody, &priorParent, &priorPayload, &priorBranch, &priorTargetSHA,
	)
	if err == nil {
		if priorTitle != title || priorBody != body || priorParent != parentItemID || priorPayload != relationPayload {
			return Feature{}, ErrFeatureCreationConflict
		}
		if priorState == "completed" {
			return s.Get(ctx, priorID)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Feature{}, err
	}

	createRelationPlan, err := s.items.prepareCreateRelations(ctx, s.db, WorkItemFeature, input.ParentItemID, input.WorkItemRelations, actor)
	if err != nil {
		if priorState == "ref_created" {
			return Feature{}, s.reconcileFailedCreationIntent(ctx, exchangePath, priorID, priorBranch, priorTargetSHA, err)
		}
		return Feature{}, err
	}
	integrationFeatureID, err := nearestFeatureFromParentTx(ctx, s.db, parentItemID)
	if err != nil {
		return Feature{}, err
	}
	if parentItemID != "" {
		parentKind, err := workItemKindTx(ctx, s.db, parentItemID)
		if err != nil {
			return Feature{}, err
		}
		if parentKind != WorkItemEpic && parentKind != WorkItemFeature {
			return Feature{}, fmt.Errorf("%w: %s items cannot contain features", ErrWorkItemMoveConflict, parentKind)
		}
		open, err := workItemContainerOpenTx(ctx, s.db, parentItemID, parentKind)
		if err != nil {
			return Feature{}, err
		}
		if !open {
			return Feature{}, ErrWorkItemParentClosed
		}
	}
	targetBranch := s.baseBranch()
	if integrationFeatureID != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT branch FROM features WHERE id = ? AND status = 'open'`, integrationFeatureID).Scan(&targetBranch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Feature{}, ErrFeatureClosed
			}
			return Feature{}, err
		}
	}
	targetTip, ok, err := flowgit.BranchTip(ctx, exchangePath, targetBranch)
	if err != nil {
		return Feature{}, fmt.Errorf("resolve integration target tip: %w", err)
	}
	if !ok {
		return Feature{}, fmt.Errorf("integration target branch %q not found in exchange remote", targetBranch)
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var id, branch, intentState, storedTitle, storedBody, storedParent, storedIntegration, storedTargetBranch, storedTargetSHA, storedRelationPayload, storedCreatedBy string
	err = tx.QueryRowContext(ctx, `
SELECT id, branch, state, title, body, COALESCE(parent_item_id, ''),
	COALESCE(integration_feature_id, ''), target_branch, target_sha, relation_payload_json, created_by
FROM feature_creation_intents WHERE operation_key = ?`, operationKey).Scan(
		&id, &branch, &intentState, &storedTitle, &storedBody, &storedParent,
		&storedIntegration, &storedTargetBranch, &storedTargetSHA, &storedRelationPayload, &storedCreatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var titleExists int
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM features WHERE title_norm = lower(trim(?)))
	OR EXISTS(SELECT 1 FROM feature_creation_intents WHERE lower(trim(title)) = lower(trim(?)))`, title, title).Scan(&titleExists); err != nil {
			return Feature{}, err
		}
		if titleExists != 0 {
			return Feature{}, ErrFeatureTitleTaken
		}
		id, err = s.allocateFeatureID(ctx, tx)
		if err != nil {
			return Feature{}, err
		}
		branch = "feature/" + id
		intentState = "prepared"
		if _, err := tx.ExecContext(ctx, `
INSERT INTO feature_creation_intents (
	id, operation_key, parent_item_id, integration_feature_id, title, body,
	branch, target_branch, target_sha, relation_payload_json, created_by, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'prepared', ?, ?)`,
			id, operationKey, sqlitex.NullableNonEmptyString(parentItemID), sqlitex.NullableNonEmptyString(integrationFeatureID),
			title, body, branch, targetBranch, targetTip, relationPayload, string(actor), formatTime(now), formatTime(now)); err != nil {
			if strings.Contains(err.Error(), "title") {
				return Feature{}, ErrFeatureTitleTaken
			}
			return Feature{}, fmt.Errorf("prepare feature creation: %w", err)
		}
		storedTitle, storedBody, storedParent = title, body, parentItemID
		storedIntegration, storedTargetBranch, storedTargetSHA = integrationFeatureID, targetBranch, targetTip
		storedRelationPayload, storedCreatedBy = relationPayload, string(actor)
	} else if err != nil {
		return Feature{}, err
	} else if storedTitle != title || storedBody != body || storedParent != parentItemID || storedRelationPayload != relationPayload {
		return Feature{}, ErrFeatureCreationConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return Feature{}, fmt.Errorf("commit feature creation intent: %w", err)
	}
	if s.CreateAfterIntentCommitTestHook != nil {
		s.CreateAfterIntentCommitTestHook()
	}
	if intentState == "completed" {
		return s.Get(ctx, id)
	}

	// Serialize ref creation with retries for this database. Git's zero-old-value
	// compare-and-swap distinguishes a ref created by this intent from an
	// ambiguous pre-existing same-tip ref; ownership and state are committed while
	// the SQLite write lock still excludes another retry.
	refTx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer refTx.Rollback()
	var refState string
	if err := refTx.QueryRowContext(ctx, `SELECT state FROM feature_creation_intents WHERE id = ?`, id).Scan(&refState); err != nil {
		return Feature{}, err
	}
	if refState == "completed" {
		refTx.Rollback()
		return s.Get(ctx, id)
	}
	if refState != "prepared" && refState != "ref_created" {
		return Feature{}, fmt.Errorf("invalid feature creation intent state %q", refState)
	}
	refCreatedByIntent, err := flowgit.CreateOrVerifyRefOwned(ctx, exchangePath, "refs/heads/"+branch, storedTargetSHA)
	if err != nil {
		if _, updateErr := refTx.ExecContext(ctx, `UPDATE feature_creation_intents SET last_error = ?, updated_at = ? WHERE id = ? AND state != 'completed'`, err.Error(), formatTime(s.now().UTC()), id); updateErr != nil {
			return Feature{}, fmt.Errorf("seed feature branch: %w (record error: %v)", err, updateErr)
		}
		if commitErr := refTx.Commit(ctx); commitErr != nil {
			return Feature{}, fmt.Errorf("seed feature branch: %w (commit error record: %v)", err, commitErr)
		}
		return Feature{}, fmt.Errorf("seed feature branch: %w", err)
	}
	result, err := refTx.ExecContext(ctx, `
UPDATE feature_creation_intents
SET state = 'ref_created',
	ref_created_by_intent = CASE WHEN ? THEN TRUE ELSE ref_created_by_intent END,
	last_error = '', updated_at = ?
WHERE id = ? AND state IN ('prepared', 'ref_created')`, refCreatedByIntent, formatTime(s.now().UTC()), id)
	if err != nil {
		return Feature{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Feature{}, err
	}
	if updated == 0 {
		return Feature{}, errors.New("feature creation intent disappeared or completed while creating its ref")
	}
	if err := refTx.Commit(ctx); err != nil {
		return Feature{}, fmt.Errorf("commit feature ref ownership: %w", err)
	}

	tx, err = sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer tx.Rollback()
	var finalState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM feature_creation_intents WHERE id = ?`, id).Scan(&finalState); err != nil {
		return Feature{}, err
	}
	if finalState == "completed" {
		tx.Rollback()
		return s.Get(ctx, id)
	}
	if finalState != "ref_created" {
		return Feature{}, fmt.Errorf("invalid feature creation intent state %q before finalization", finalState)
	}
	storedRelations, err := decodeCreateRelationPayload(storedRelationPayload)
	if err != nil {
		return Feature{}, err
	}
	storedDeclaredParent := storedParent
	for _, relation := range storedRelations {
		if relation.Kind == RelationParentOf && relation.TargetIsNewItem {
			storedDeclaredParent = ""
			break
		}
	}
	createRelationPlan, err = s.items.prepareCreateRelations(ctx, tx, WorkItemFeature, storedDeclaredParent, storedRelations, Actor(storedCreatedBy))
	if err != nil {
		tx.Rollback()
		return Feature{}, s.reconcileFailedCreationIntent(ctx, exchangePath, id, branch, storedTargetSHA, err)
	}
	if createRelationPlan.ParentItemID != storedParent {
		tx.Rollback()
		return Feature{}, s.reconcileFailedCreationIntent(ctx, exchangePath, id, branch, storedTargetSHA, ErrFeatureCreationConflict)
	}
	var featureExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM features WHERE id = ?)`, id).Scan(&featureExists); err != nil {
		return Feature{}, err
	}
	if featureExists == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_items (id, kind, created_at) VALUES (?, 'feature', ?)`, id, formatTime(now)); err != nil {
			return Feature{}, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO features (
	id, title, body, branch, status, integration_feature_id, created_from_sha,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?, ?)`, id, storedTitle, storedBody, branch,
			sqlitex.NullableNonEmptyString(storedIntegration), storedTargetSHA, storedCreatedBy, formatTime(now), formatTime(now)); err != nil {
			if strings.Contains(err.Error(), "features.title_norm") {
				return Feature{}, ErrFeatureTitleTaken
			}
			return Feature{}, fmt.Errorf("insert feature: %w", err)
		}
	}
	parentPlan := createRelationPlan
	parentPlan.ParentItemID = storedParent
	if featureExists != 0 && storedParent != "" {
		var relationExists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'parent_of')`, storedParent, id).Scan(&relationExists); err != nil {
			return Feature{}, err
		}
		if relationExists != 0 {
			parentPlan.ParentItemID = ""
		}
	}
	touched, err := s.items.linkCreateRelationsTx(ctx, tx, id, parentPlan, Actor(storedCreatedBy))
	if err != nil {
		return Feature{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, touched, s.now().UTC()); err != nil {
		return Feature{}, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE feature_creation_intents SET state = 'completed', last_error = '', updated_at = ? WHERE id = ? AND state = 'ref_created'`, formatTime(s.now().UTC()), id)
	if err != nil {
		return Feature{}, err
	}
	if updated, err = result.RowsAffected(); err != nil {
		return Feature{}, err
	}
	if updated != 1 {
		return Feature{}, errors.New("feature creation intent changed while finalizing")
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventFeatureCreated,
		Actor:   storedCreatedBy,
		Payload: eventPayload(map[string]any{"feature_id": id, "title": input.Title}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Feature{}, err
	}
	return s.Get(ctx, id)
}

func (s *FeatureService) reconcileFailedCreationIntent(ctx context.Context, exchangePath, id, branch, expectedSHA string, cause error) error {
	// Keep the intent write-locked while inspecting and deleting an owned ref.
	// Ref creation and ownership persistence use the same database write lock, so
	// a concurrent retry cannot create or claim the ref while cleanup runs.
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return fmt.Errorf("%w (lock failed feature intent: %v)", cause, err)
	}
	defer tx.Rollback()
	var state string
	var refCreatedByIntent bool
	if err := tx.QueryRowContext(ctx, `SELECT state, ref_created_by_intent FROM feature_creation_intents WHERE id = ?`, id).Scan(&state, &refCreatedByIntent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return cause
		}
		return fmt.Errorf("%w (inspect failed feature intent: %v)", cause, err)
	}
	if state == "completed" {
		return fmt.Errorf("%w (feature creation completed concurrently; preserving ref and intent)", cause)
	}
	if refCreatedByIntent {
		currentTip, exists, err := flowgit.BranchTip(ctx, exchangePath, branch)
		if err != nil {
			return fmt.Errorf("%w (inspect orphan feature ref: %v)", cause, err)
		}
		if exists {
			if currentTip != expectedSHA {
				preserveErr := fmt.Errorf("feature branch %q has unexpected tip %s; preserving ref and intent", branch, currentTip)
				if _, err := tx.ExecContext(ctx, `UPDATE feature_creation_intents SET last_error = ?, updated_at = ? WHERE id = ?`, preserveErr.Error(), formatTime(s.now().UTC()), id); err != nil {
					return fmt.Errorf("%w (%v; record preservation error: %v)", cause, preserveErr, err)
				}
				if err := tx.Commit(ctx); err != nil {
					return fmt.Errorf("%w (%v; commit preservation error: %v)", cause, preserveErr, err)
				}
				return fmt.Errorf("%w (%v)", cause, preserveErr)
			}
			if err := flowgit.DeleteRefIfMatches(ctx, exchangePath, "refs/heads/"+branch, expectedSHA); err != nil {
				return fmt.Errorf("%w (delete orphan feature ref: %v)", cause, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM feature_creation_intents WHERE id = ? AND state != 'completed'`, id); err != nil {
		return fmt.Errorf("%w (delete failed feature intent: %v)", cause, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w (commit failed feature cleanup: %v)", cause, err)
	}
	return cause
}

func (s *FeatureService) Get(ctx context.Context, id string) (Feature, error) {
	return s.getOne(ctx, featureSelect+`
FROM features
WHERE id = ?`, strings.TrimSpace(id))
}

// Resolve loads a feature by id, falling back to an exact title match so CLI
// and web callers can use either.
func (s *FeatureService) Resolve(ctx context.Context, ref string) (Feature, error) {
	feature, err := s.Get(ctx, ref)
	if err == nil {
		return feature, nil
	}
	if !errors.Is(err, ErrFeatureNotFound) {
		return Feature{}, err
	}
	return s.getOne(ctx, featureSelect+`
FROM features
WHERE title = ?`, strings.TrimSpace(ref))
}

func (s *FeatureService) getOne(ctx context.Context, query string, arg any) (Feature, error) {
	feature, err := scanFeature(s.db.QueryRowContext(ctx, query, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Feature{}, ErrFeatureNotFound
	}
	if err != nil {
		return Feature{}, err
	}
	return feature, nil
}

const featureSelect = `
SELECT
	id,
	title,
	body,
	branch,
	status,
	integration_feature_id,
	created_from_sha,
	created_by,
	created_at,
	updated_at,
	landed_at,
	land_sha,
	land_target_feature_id,
	land_target_branch,
	land_target_sha`

// List returns features ordered by title. A non-empty status filters in SQL.
func (s *FeatureService) List(ctx context.Context, status FeatureStatus) ([]Feature, error) {
	query := featureSelect + `
FROM features`
	var args []any
	switch status {
	case "":
		// No filter.
	case FeatureOpen, FeatureLanded, FeatureArchived:
		query += `
WHERE status = ?`
		args = append(args, string(status))
	default:
		return nil, fmt.Errorf("invalid feature status: %s", status)
	}
	query += `
ORDER BY title`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	defer rows.Close()

	var features []Feature
	for rows.Next() {
		feature, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		features = append(features, feature)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate features: %w", err)
	}

	return features, nil
}

// Edit updates mutable feature metadata. Status transitions go through the
// dedicated Land/Archive methods; the branch name is immutable because task
// branches and changes reference it.
func (s *FeatureService) Edit(ctx context.Context, id string, input EditFeatureInput) (Feature, error) {
	feature, err := s.Get(ctx, id)
	if err != nil {
		return Feature{}, err
	}
	if input.Title == nil && input.Body == nil {
		return feature, nil
	}
	title := feature.Title
	if input.Title != nil {
		title = strings.TrimSpace(*input.Title)
		if title == "" {
			return Feature{}, errors.New("feature title is required")
		}
	}
	body := feature.Body
	if input.Body != nil {
		body = *input.Body
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE features
SET title = ?, body = ?, updated_at = ?
WHERE id = ?`, title, body, formatTime(s.now().UTC()), feature.ID); err != nil {
		if strings.Contains(err.Error(), "features.title_norm") {
			return Feature{}, ErrFeatureTitleTaken
		}
		return Feature{}, fmt.Errorf("update feature: %w", err)
	}
	return s.Get(ctx, feature.ID)
}

// ErrFeatureActive reports a land/archive request against a feature that
// still has active work: non-done tasks or a running rebase. Offenders lists
// the blocking task ids so the caller can name them.
type ErrFeatureActive struct {
	Offenders []string
}

func (e *ErrFeatureActive) Error() string {
	return fmt.Sprintf("feature has active work: %s", strings.Join(e.Offenders, ", "))
}

// Land squash-merges the feature branch into the project base and stamps the
// feature landed. Landing requires an idle feature: every assigned task done
// and no rebase running. A squash that stages nothing is the heal path: the
// feature's content is already in the base (an empty feature, or a crashed
// earlier land whose push landed), so the feature is stamped landed at the
// current base tip. Replays return the landed feature unchanged.
func (s *FeatureService) Land(ctx context.Context, featureRef string, actor Actor) (Feature, error) {
	actor = defaultActor(actor, ActorHuman)
	if err := validateActor(actor); err != nil {
		return Feature{}, err
	}
	feature, err := s.Resolve(ctx, featureRef)
	if err != nil {
		return Feature{}, err
	}
	if feature.Status == FeatureLanded {
		return feature, nil
	}
	if feature.Status != FeatureOpen {
		return Feature{}, ErrFeatureClosed
	}
	if err := requireNoEffectiveBlockersTx(ctx, s.db, feature.ID); err != nil {
		return Feature{}, err
	}
	if err := s.requireIdle(ctx, feature.ID); err != nil {
		return Feature{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return Feature{}, errors.New("project exchange remote is required for feature branches")
	}
	target, err := s.integrationTarget(ctx, feature)
	if err != nil {
		return Feature{}, err
	}
	tip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return Feature{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return Feature{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}

	result, err := flowgit.SquashMergeToBase(ctx, flowgit.SquashMergeInput{
		ExchangeRepoPath: exchangePath,
		BaseBranch:       target.Branch,
		Branch:           feature.Branch,
		ExpectedHeadSHA:  tip,
		ExpectedBaseSHA:  target.TipSHA,
		Message:          fmt.Sprintf("%s: %s", feature.ID, feature.Title),
	})
	landSHA := ""
	landTargetSHA := target.TipSHA
	switch {
	case err == nil:
		landSHA = result.MergeSHA
		landTargetSHA = result.PreviousBaseSHA
	case errors.Is(err, flowgit.ErrNoMergeChanges):
		baseTip, found, tipErr := flowgit.BranchTip(ctx, exchangePath, target.Branch)
		if tipErr != nil {
			return Feature{}, fmt.Errorf("resolve base branch tip: %w", tipErr)
		}
		if !found {
			return Feature{}, fmt.Errorf("integration target branch %q not found in exchange remote", target.Branch)
		}
		landSHA = baseTip
		landTargetSHA = baseTip
	default:
		var conflict *flowgit.MergeConflictError
		if errors.As(err, &conflict) {
			return Feature{}, fmt.Errorf("feature branch conflicts with %s; rebase it first: %w", target.Branch, err)
		}
		return Feature{}, fmt.Errorf("land feature branch: %w", err)
	}

	nowTime := s.now().UTC()
	now := formatTime(nowTime)
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
UPDATE features
SET status = ?, landed_at = ?, land_sha = ?, land_target_feature_id = ?,
	land_target_branch = ?, land_target_sha = ?, updated_at = ?
WHERE id = ? AND status = 'open'`,
		string(FeatureLanded), now, landSHA, sqlitex.NullableNonEmptyString(target.FeatureID),
		target.Branch, landTargetSHA, now, feature.ID); err != nil {
		return Feature{}, fmt.Errorf("stamp feature landed: %w", err)
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{feature.ID}, nowTime); err != nil {
		return Feature{}, err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventFeatureLanded,
		Actor:   string(actor),
		Payload: eventPayload(map[string]any{"feature_id": feature.ID, "land_sha": landSHA, "target_branch": target.Branch}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Feature{}, err
	}
	return s.Get(ctx, feature.ID)
}

// Archive closes a feature without landing it. Archive is the only delete:
// the branch is retained in the exchange for audit. It requires the same idle
// precondition as Land and replays as a no-op.
func (s *FeatureService) Archive(ctx context.Context, featureRef string) (Feature, error) {
	feature, err := s.Resolve(ctx, featureRef)
	if err != nil {
		return Feature{}, err
	}
	if feature.Status == FeatureArchived {
		return feature, nil
	}
	if err := requireNoEffectiveBlockersTx(ctx, s.db, feature.ID); err != nil {
		return Feature{}, err
	}
	if err := s.requireIdle(ctx, feature.ID); err != nil {
		return Feature{}, err
	}
	now := s.now().UTC()
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
UPDATE features SET status = ?, updated_at = ? WHERE id = ? AND status != ?`,
		string(FeatureArchived), formatTime(now), feature.ID, string(FeatureArchived)); err != nil {
		return Feature{}, fmt.Errorf("archive feature: %w", err)
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{feature.ID}, now); err != nil {
		return Feature{}, err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventFeatureArchived,
		Payload: eventPayload(map[string]any{"feature_id": feature.ID, "title": feature.Title}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Feature{}, err
	}
	return s.Get(ctx, feature.ID)
}

// SetEventLog wires the project event log; a nil log disables emission.
func (s *FeatureService) SetEventLog(log *EventLogService) {
	s.eventLog = log
}

// Tasks returns the feature's assigned tasks in creation order.
func (s *FeatureService) Tasks(ctx context.Context, featureID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+taskSelectColumns+`
FROM tasks i
WHERE i.feature_id = ?
ORDER BY i.created_at, i.id`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, fmt.Errorf("list feature tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature tasks: %w", err)
	}

	return tasks, nil
}

// TreeTasks returns the flattened, deduplicated task membership of a feature:
// explicit organizational descendants plus tasks cached to this feature or a
// nested descendant feature. It is the canonical container read; Tasks stays
// as the direct-membership primitive used by rebase confinement.
func (s *FeatureService) TreeTasks(ctx context.Context, featureID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE descendants(id) AS (
	VALUES (?)
	UNION
	SELECT r.target_item_id FROM descendants
	JOIN work_item_relations r ON r.source_item_id = descendants.id AND r.kind = 'parent_of'
), feature_descendants(id) AS (
	SELECT d.id FROM descendants d JOIN work_items wi ON wi.id = d.id AND wi.kind = 'feature'
)
SELECT id FROM (
	SELECT t.id FROM tasks t JOIN descendants d ON d.id = t.id
	UNION
	SELECT t.id FROM tasks t JOIN feature_descendants f ON f.id = t.feature_id
)
ORDER BY id`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	taskRows, err := s.db.QueryContext(ctx, "SELECT"+taskSelectColumns+`
FROM tasks i WHERE `+inPredicate("i.id", len(ids))+` ORDER BY i.created_at, i.id`, args...)
	if err != nil {
		return nil, err
	}
	return scanRows(taskRows, scanTask)
}

// requireIdle enforces the land/archive precondition: no running rebase and
// no non-done tasks. A rebase task is itself a non-done feature task, so it
// appears among the offenders naturally; a taskless running row (the clean
// path's crash window) is named symbolically.
func (s *FeatureService) requireIdle(ctx context.Context, featureID string) error {
	offenders, err := s.nonDoneFeatureTaskIDs(ctx, featureID)
	if err != nil {
		return err
	}
	if running, found, err := s.RunningRebase(ctx, featureID); err != nil {
		return err
	} else if found && running.TaskID == "" {
		offenders = append([]string{"rebase in progress"}, offenders...)
	}
	openChildren, err := s.openIntegrationDescendants(ctx, featureID)
	if err != nil {
		return err
	}
	offenders = append(offenders, openChildren...)
	if len(offenders) > 0 {
		return &ErrFeatureActive{Offenders: offenders}
	}
	return nil
}

// BranchState computes the feature branch's live divergence from the project
// base: ahead/behind commit counts plus both tips, read from the local
// exchange so the feature page can show "N ahead / M behind" on every load.
func (s *FeatureService) BranchState(ctx context.Context, featureID string) (FeatureBranchState, error) {
	feature, err := s.Get(ctx, featureID)
	if err != nil {
		return FeatureBranchState{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return FeatureBranchState{}, errors.New("project exchange remote is required for feature branches")
	}
	target, err := s.integrationTarget(ctx, feature)
	if err != nil {
		return FeatureBranchState{}, err
	}
	featureTip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return FeatureBranchState{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return FeatureBranchState{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	ahead, behind, err := flowgit.RefDivergence(ctx, exchangePath, target.Branch, feature.Branch)
	if err != nil {
		return FeatureBranchState{}, err
	}

	return FeatureBranchState{
		BaseTipSHA:    target.TipSHA,
		FeatureTipSHA: featureTip,
		Ahead:         ahead,
		Behind:        behind,
	}, nil
}

// RunningRebase returns the feature's open feature_rebases row, if any. Both
// the schedule-time block linking and the finalize node resolve through here.
func (s *FeatureService) RunningRebase(ctx context.Context, featureID string) (FeatureRebase, bool, error) {
	rebase, err := scanFeatureRebase(s.db.QueryRowContext(ctx, `
SELECT
	id,
	feature_id,
	COALESCE(task_id, ''),
	old_tip_sha,
	target_base,
	target_base_sha,
	target_feature_id,
	new_tip_sha,
	state,
	created_at,
	completed_at,
	restrict_blocked_to
FROM feature_rebases
WHERE feature_id = ? AND state = 'running'`, strings.TrimSpace(featureID)))
	if errors.Is(err, sql.ErrNoRows) {
		return FeatureRebase{}, false, nil
	}
	if err != nil {
		return FeatureRebase{}, false, fmt.Errorf("load running rebase: %w", err)
	}
	return rebase, true, nil
}

// ListRebases returns the feature's rebase history, newest first.
func (s *FeatureService) ListRebases(ctx context.Context, featureID string) ([]FeatureRebase, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	feature_id,
	COALESCE(task_id, ''),
	old_tip_sha,
	target_base,
	target_base_sha,
	target_feature_id,
	new_tip_sha,
	state,
	created_at,
	completed_at,
	restrict_blocked_to
FROM feature_rebases
WHERE feature_id = ?
ORDER BY created_at DESC, id DESC`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, fmt.Errorf("list feature rebases: %w", err)
	}
	defer rows.Close()

	var rebases []FeatureRebase
	for rows.Next() {
		rebase, err := scanFeatureRebase(rows)
		if err != nil {
			return nil, err
		}
		rebases = append(rebases, rebase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature rebases: %w", err)
	}

	return rebases, nil
}

// RebaseOnMain rebases the feature branch onto the project base tip. A branch
// that is not behind the base is already up to date. A clean rebase is
// published immediately and the row stamped finalized. A conflicted rebase
// keeps the running row, creates a system rebase task on the feature-rebase
// flow, links rebase_task blocks every other non-done feature task, and
// schedules it — the trusted finalize node publishes the result.
//
// The feature must already be resolved: callers that only hold a ref look it
// up once and pass the value, so the console rebase path never resolves the
// same ref twice.
//
// restrictBlockedTo confines the conflicted path's blocker links to the named
// tasks: when non-empty, only those tasks (if still non-done) receive a
// rebase_task blocks relation. Task-bound console credentials pass exactly
// their bound task. The restriction is persisted on the running feature_rebases
// row and consulted by the schedule-time gate (EnsureRebaseBlock) for tasks
// created or reopened while the rebase runs, so the blocker set is confined
// for the row's whole lifetime rather than by a racy pre-read: a feature task
// created concurrently after any API-side scope check can never be linked.
//
// For a restricted rebase the sole-open-task invariant is additionally
// re-checked under the database write lock after the Git preflight and before
// any rebase or relation row exists (checkRebaseScopeLocked), and that
// transaction stays open only across the scope check and the row insert, so
// the confinement decision is atomic with the relation-inserting rebase: a
// feature task added by another principal either lands before the check (the
// rebase is rejected with ErrFeatureRebaseForbidden) or after it (the task
// never receives a rebase_task blocks relation, because the blocker set is
// confined at relation-creation time and the schedule-time gate consults the
// persisted restriction).
func (s *FeatureService) RebaseOnMain(ctx context.Context, feature Feature, restrictBlockedTo ...string) (RebaseStartResult, error) {
	if feature.Status != FeatureOpen {
		return RebaseStartResult{}, ErrFeatureClosed
	}
	if err := requireNoEffectiveBlockersTx(ctx, s.db, feature.ID); err != nil {
		return RebaseStartResult{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return RebaseStartResult{}, errors.New("project exchange remote is required for feature branches")
	}
	if err := s.requireNoOpenIntegrationDescendants(ctx, feature.ID); err != nil {
		return RebaseStartResult{}, err
	}

	if running, found, err := s.RunningRebase(ctx, feature.ID); err != nil {
		return RebaseStartResult{}, err
	} else if found {
		// Crash between the clean path's push and its stamp: the ref already
		// moved, so heal the row to finalized instead of refusing forever.
		tip, ok, tipErr := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
		if tipErr == nil && ok && tip != running.OldTipSHA {
			if err := s.stampRebaseDone(ctx, running.ID, RebaseFinalized, tip); err != nil {
				return RebaseStartResult{}, err
			}
			return RebaseStartResult{Kind: RebaseRebased, Feature: feature, NewTipSHA: tip}, nil
		}
		return RebaseStartResult{}, ErrFeatureRebaseRunning
	}

	// Preflight runs before the write reservation: resolving the base and
	// feature tips and measuring divergence only touches the exchange remote,
	// so holding the per-project database's single connection through them
	// would stall every read and write for the project, including an
	// already-up-to-date rebase that writes nothing.
	target, err := s.integrationTarget(ctx, feature)
	if err != nil {
		return RebaseStartResult{}, err
	}
	base, baseTip := target.Branch, target.TipSHA
	featureTip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return RebaseStartResult{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return RebaseStartResult{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	_, behind, err := flowgit.RefDivergence(ctx, exchangePath, base, feature.Branch)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if behind == 0 {
		return RebaseStartResult{Kind: RebaseAlreadyUpToDate, Feature: feature, NewTipSHA: featureTip}, nil
	}

	if s.RebaseOnMainTestHook != nil {
		s.RebaseOnMainTestHook(RebaseOnMainBeforeScopeCheck)
	}

	// A restricted rebase (task-bound console) re-checks its scope under the
	// database write lock, after the preflight and before any rebase row or
	// relation exists, so the decision is atomic with concurrent task creation
	// by other principals. The transaction stays open only across the scope
	// check and the row insert: insertRebaseRow reuses it, so the decision and
	// the persisted confinement commit atomically — there is no window in which
	// a task added by another principal is neither rejected nor covered by a
	// persisted restriction. The transaction is rolled back if any later step
	// fails. This only affects the restricted path: owner and unbound
	// project-console rebases take the nil-transaction path.
	scopeTx, err := s.checkRebaseScopeLocked(ctx, feature.ID, restrictBlockedTo)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if scopeTx != nil {
		defer scopeTx.Rollback()
	}

	// The durable record comes first: if the process crashes mid-rebase the
	// next request reconciles the row against the exchange ref. For a restricted
	// rebase the row is inserted inside the write transaction opened by
	// checkRebaseScopeLocked, so the confinement decision and the persisted
	// restriction commit atomically.
	rebaseID, err := s.insertRebaseRow(ctx, scopeTx, feature.ID, "", featureTip, base, baseTip, target.FeatureID, restrictBlockedToKey(restrictBlockedTo))
	if err != nil {
		return RebaseStartResult{}, err
	}

	if s.RebaseOnMainTestHook != nil {
		s.RebaseOnMainTestHook(RebaseOnMainAfterReservation)
	}

	result, err := flowgit.RebaseOnto(ctx, flowgit.RebaseOntoInput{
		ExchangePath:    exchangePath,
		Branch:          feature.Branch,
		Onto:            base,
		ExpectedOldSHA:  featureTip,
		ExpectedOntoSHA: baseTip,
	})
	if err == nil {
		if err := s.stampRebaseDone(ctx, rebaseID, RebaseFinalized, result.HeadSHA); err != nil {
			return RebaseStartResult{}, err
		}
		return RebaseStartResult{Kind: RebaseRebased, Feature: feature, NewTipSHA: result.HeadSHA}, nil
	}
	var conflict *flowgit.RebaseConflictError
	if !errors.As(err, &conflict) {
		// Operational failure: nothing was pushed, so the running row would
		// block future rebases forever. Fail it and surface the error.
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, fmt.Errorf("rebase feature branch: %w", err)
	}

	flowID, err := s.flowIDByName(ctx)
	if err != nil {
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, err
	}
	body := fmt.Sprintf(
		"Rebase the feature branch `%s` onto `%s` (base tip `%s`).\n\n"+
			"The automatic rebase stopped on conflicts. Resolve them preserving the feature's content exactly, "+
			"then complete this task's change; the rebase is published to the feature branch after checks, verification, and human approval.\n\n"+
			"git reported:\n```\n%s\n```",
		feature.Branch, base, baseTip, strings.TrimSpace(conflict.Output))
	task, err := s.tasks.CreateTask(ctx, CreateTaskInput{
		Title:     fmt.Sprintf("Rebase %s onto %s", feature.Title, base),
		Body:      body,
		FeatureID: &feature.ID,
		FlowID:    flowID,
		CreatedBy: ActorSystem,
	})
	if err != nil {
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, fmt.Errorf("create rebase task: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE feature_rebases SET task_id = ? WHERE id = ?`, task.ID, rebaseID); err != nil {
		return RebaseStartResult{}, fmt.Errorf("attach rebase task: %w", err)
	}

	blocked, err := s.nonDoneFeatureTaskIDs(ctx, feature.ID)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if err := s.tasks.BlockOnRebase(ctx, task.ID, restrictBlockedTasks(blocked, restrictBlockedTo)); err != nil {
		return RebaseStartResult{}, fmt.Errorf("block feature tasks on rebase: %w", err)
	}

	if s.Runs != nil {
		if _, err := s.Runs.ScheduleAs(ctx, task.ID, ActorSystem); err != nil {
			return RebaseStartResult{}, fmt.Errorf("schedule rebase task: %w", err)
		}
	}

	return RebaseStartResult{Kind: RebaseTaskCreated, Feature: feature, RebaseTaskID: task.ID}, nil
}

func requireNoEffectiveBlockersTx(ctx context.Context, q workItemRelationQuerier, itemID string) error {
	count, err := effectiveUnresolvedBlockerCountTx(ctx, q, itemID)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: %s has %d unresolved blocker(s)", ErrWorkItemBlocked, itemID, count)
	}
	return nil
}

// flowIDByName resolves the seeded feature-rebase flow.
func (s *FeatureService) flowIDByName(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM flows WHERE name = ?`, FeatureRebaseFlowName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s flow is not seeded for this project", FeatureRebaseFlowName)
	}
	if err != nil {
		return "", fmt.Errorf("look up %s flow: %w", FeatureRebaseFlowName, err)
	}
	return id, nil
}

// FinalizeRebase publishes the rebased head recorded on the rebase task's
// change to the feature ref, guarded by a compare-and-swap on the tip the
// running feature_rebases row recorded. It returns the finalize node's
// outcome: "finalized" when the ref now points at the change head, "stale"
// when the feature branch moved mid-rebase and the rebase must redo itself
// (the flow loops back to the rebase agent with a fresh running row).
func (s *FeatureService) FinalizeRebase(ctx context.Context, featureID, taskID string, change Change) (string, error) {
	feature, err := s.Get(ctx, featureID)
	if err != nil {
		return "", err
	}
	rebase, found, err := s.RunningRebase(ctx, feature.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoOpenRebase
	}
	if rebase.TaskID != strings.TrimSpace(taskID) {
		return "", fmt.Errorf("running rebase belongs to task %s, not %s", rebase.TaskID, taskID)
	}
	head := strings.TrimSpace(change.HeadSHA)
	if head == "" {
		return "", errors.New("rebase task change has no head sha")
	}
	if head == rebase.OldTipSHA {
		return "", errors.New("rebase task change head matches the pre-rebase tip")
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return "", errors.New("project exchange remote is required for feature branches")
	}

	tip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	if tip == head {
		// Heal: the push landed but the stamp crashed (merge-intent-style
		// replay). The ref already points at the new head, so finalizing is a
		// no-op beyond the row.
		if err := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); err != nil {
			return "", err
		}
		return "finalized", nil
	}
	if tip != rebase.OldTipSHA {
		if err := s.markRebaseStale(ctx, rebase, tip); err != nil {
			return "", err
		}
		return "stale", nil
	}

	ref := "refs/heads/" + feature.Branch
	if err := flowgit.PushRefCompareAndSwap(ctx, exchangePath, ref, head, rebase.OldTipSHA); err != nil {
		// A lost lease means a concurrent update (a task merged mid-rebase).
		// Re-probe and take the stale path when the tip really moved.
		current, ok, probeErr := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
		if probeErr == nil && ok && current != rebase.OldTipSHA && current != head {
			if staleErr := s.markRebaseStale(ctx, rebase, current); staleErr != nil {
				return "", staleErr
			}
			return "stale", nil
		}
		if probeErr == nil && ok && current == head {
			if stampErr := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); stampErr != nil {
				return "", stampErr
			}
			return "finalized", nil
		}
		return "", fmt.Errorf("push rebased head to feature branch: %w", err)
	}

	if err := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); err != nil {
		return "", err
	}
	return "finalized", nil
}

// markRebaseStale records that the feature branch moved out from under a
// running rebase and opens the redo row against the current tip, keeping the
// finalize node's compare-and-swap expectation truthful for the next visit.
func (s *FeatureService) markRebaseStale(ctx context.Context, rebase FeatureRebase, currentTip string) error {
	baseTip, ok, err := flowgit.BranchTip(ctx, strings.TrimSpace(s.project.ExchangePath), rebase.TargetBase)
	if err != nil {
		return fmt.Errorf("resolve base branch tip: %w", err)
	}
	if !ok {
		return fmt.Errorf("base branch %q not found in exchange remote", rebase.TargetBase)
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `
UPDATE feature_rebases SET state = ?, completed_at = ? WHERE id = ? AND state = 'running'`,
		string(RebaseStale), now, rebase.ID); err != nil {
		return fmt.Errorf("mark rebase stale: %w", err)
	}
	id, err := s.allocateRebaseID(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feature_rebases (
	id, feature_id, task_id, old_tip_sha, target_base, target_base_sha,
	target_feature_id, new_tip_sha, state, created_at, restrict_blocked_to
) VALUES (?, ?, ?, ?, ?, ?, ?, '', 'running', ?, ?)`,
		id, rebase.FeatureID, rebase.TaskID, currentTip, rebase.TargetBase, baseTip,
		nullableStringValue(rebase.TargetFeatureID), now, rebase.RestrictBlockedTo); err != nil {
		return fmt.Errorf("open redo rebase row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit stale rebase: %w", err)
	}

	return nil
}

// EnsureRebaseBlock links a running rebase's task as a blocker of taskID when
// taskID belongs to the rebased feature. WorkflowRunService calls it before
// scheduling a task so tasks created or reopened mid-rebase are held at the
// dependency gate like the rest of the feature. A restriction persisted on the
// running row (a task-bound console's confinement) applies here too: when the
// row names allowed blocker targets, taskID is linked only if it is one of
// them, so a sibling of the bound task is never linked by the schedule path.
func (s *FeatureService) EnsureRebaseBlock(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	var rebaseTaskID, restriction string
	err := s.db.QueryRowContext(ctx, `
SELECT fr.task_id, fr.restrict_blocked_to
FROM feature_rebases fr
JOIN tasks t ON t.feature_id = fr.feature_id
WHERE t.id = ? AND fr.state = 'running' AND fr.task_id IS NOT NULL AND fr.task_id != ?`,
		taskID, taskID).Scan(&rebaseTaskID, &restriction)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load running rebase for task: %w", err)
	}
	if !restrictionAllows(restriction, taskID) {
		// The running rebase is confined: a task-bound console started it and
		// may link only its bound task. This task is out of scope and must not
		// receive a rebase_task blocks relation whose endpoints exclude the
		// bound task.
		return nil
	}

	return s.tasks.BlockOnRebase(ctx, rebaseTaskID, []string{taskID})
}

// restrictBlockedTasks filters ids down to the allowed membership set,
// preserving order. An empty allowed set leaves ids untouched, keeping the
// default rebase behavior of holding every non-done feature task.
func restrictBlockedTasks(ids, allowed []string) []string {
	if len(allowed) == 0 {
		return ids
	}
	set := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		set[strings.TrimSpace(id)] = struct{}{}
	}
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := set[strings.TrimSpace(id)]; ok {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

// restrictBlockedToKey serializes a blocker restriction for persistence on the
// feature_rebases row as a comma-joined list of task ids. An empty key means
// the rebase gates the whole feature (owner and unbound project-console
// rebases).
func restrictBlockedToKey(allowed []string) string {
	seen := make(map[string]struct{}, len(allowed))
	var ids []string
	for _, id := range allowed {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

// restrictionAllows reports whether a persisted rebase restriction permits
// linking taskID as a blocker target. An empty restriction (no confinement)
// allows every task.
func restrictionAllows(restriction, taskID string) bool {
	restriction = strings.TrimSpace(restriction)
	if restriction == "" {
		return true
	}
	for _, id := range strings.Split(restriction, ",") {
		if strings.TrimSpace(id) == taskID {
			return true
		}
	}
	return false
}

// nonDoneFeatureTasksQuery lists a feature's tasks that a running rebase must
// hold: everything not done. Done blockers are resolved by definition, so they
// neither need nor get a link. checkRebaseScopeLocked runs the same query
// inside its write-locked transaction so the decision and the relation-time
// filter use the same notion of "non-done".
const nonDoneFeatureTasksQuery = `
SELECT id FROM tasks WHERE feature_id = ? AND (lifecycle_state IS NULL OR lifecycle_state != 'done')`

// nonDoneFeatureTaskIDs lists the feature's tasks that a running rebase must
// hold: everything not done. Done blockers are resolved by definition, so
// they neither need nor get a link.
func (s *FeatureService) nonDoneFeatureTaskIDs(ctx context.Context, featureID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, nonDoneFeatureTasksQuery, featureID)
	if err != nil {
		return nil, fmt.Errorf("list feature tasks: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature tasks: %w", err)
	}

	return ids, nil
}

// checkRebaseScopeLocked enforces the sole-open-task invariant for a restricted
// rebase (task-bound console) under the database write lock: the feature's
// non-done tasks must be exactly the allowed set. An empty allowed set is
// unrestricted and returns a nil transaction. The caller runs the Git preflight
// before acquiring the lock, and the immediate transaction is held only across
// this scope check and the row insert: the caller passes the transaction to
// insertRebaseRow, so the confinement decision and the durable restricted row
// commit atomically — there is no pre-persistence window in which a task added
// by another principal is neither rejected nor covered by a persisted
// restriction.
func (s *FeatureService) checkRebaseScopeLocked(ctx context.Context, featureID string, allowed []string) (*sqlitex.Tx, error) {
	if len(allowed) == 0 {
		return nil, nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		allowedSet[strings.TrimSpace(id)] = struct{}{}
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, nonDoneFeatureTasksQuery, featureID)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("list feature tasks: %w", err)
	}
	defer rows.Close()

	found := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			tx.Rollback()
			return nil, err
		}
		found[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("iterate feature tasks: %w", err)
	}

	if len(found) != len(allowedSet) {
		tx.Rollback()
		return nil, ErrFeatureRebaseForbidden
	}
	for id := range found {
		if _, ok := allowedSet[id]; !ok {
			tx.Rollback()
			return nil, ErrFeatureRebaseForbidden
		}
	}

	return tx, nil
}

// insertRebaseRow persists the running rebase row inside tx. tx is the open
// write transaction returned by checkRebaseScopeLocked for a restricted rebase
// (nil for an unrestricted one), so a restricted rebase's confinement decision
// and its persisted restrict_blocked_to row commit atomically; a concurrent
// task creation either lost the write lock to this transaction or lands after
// the restricted row is visible and is refused by the schedule-time gate.
func (s *FeatureService) insertRebaseRow(ctx context.Context, tx *sqlitex.Tx, featureID, taskID, oldTip, targetBase, targetBaseSHA, targetFeatureID, restrictBlockedTo string) (string, error) {
	if tx == nil {
		var err error
		tx, err = sqlitex.BeginImmediate(ctx, s.db)
		if err != nil {
			return "", err
		}
	}
	defer tx.Rollback()

	// Re-check for a running rebase inside the write reservation: the caller's
	// RunningRebase pre-read happens before the Git preflight, so a concurrent
	// rebase request can have persisted its own row while the remote was being
	// probed. Refuse cleanly instead of tripping the one-running-row partial
	// unique index (which would surface as a 500).
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_rebases WHERE feature_id = ? AND state = 'running'`, featureID).Scan(&running); err != nil {
		return "", fmt.Errorf("check running rebase: %w", err)
	}
	if running != 0 {
		return "", ErrFeatureRebaseRunning
	}

	id, err := s.allocateRebaseID(ctx, tx)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feature_rebases (
	id, feature_id, task_id, old_tip_sha, target_base, target_base_sha,
	target_feature_id, new_tip_sha, state, created_at, restrict_blocked_to
) VALUES (?, ?, ?, ?, ?, ?, ?, '', 'running', ?, ?)`,
		id, featureID, sqlitex.NullableNonEmptyString(taskID), oldTip, targetBase, targetBaseSHA,
		sqlitex.NullableNonEmptyString(targetFeatureID), formatTime(s.now().UTC()), restrictBlockedTo); err != nil {
		return "", fmt.Errorf("insert rebase row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit rebase row: %w", err)
	}

	return id, nil
}

// stampRebaseDone closes a rebase row. newTipSHA is recorded for finalized
// rows and left empty otherwise.
func (s *FeatureService) stampRebaseDone(ctx context.Context, rebaseID string, state RebaseState, newTipSHA string) error {
	now := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE feature_rebases
SET state = ?, new_tip_sha = ?, completed_at = ?
WHERE id = ? AND state = 'running'`,
		string(state), newTipSHA, now, rebaseID); err != nil {
		return fmt.Errorf("stamp rebase %s: %w", state, err)
	}

	return nil
}

func (s *FeatureService) baseBranch() string {
	base := strings.TrimSpace(s.project.BaseBranch)
	if base == "" {
		base = "main"
	}
	return base
}

type featureIntegrationTarget struct {
	FeatureID string
	Branch    string
	TipSHA    string
}

func (s *FeatureService) integrationTarget(ctx context.Context, feature Feature) (featureIntegrationTarget, error) {
	target := featureIntegrationTarget{Branch: s.baseBranch()}
	if feature.IntegrationFeatureID != nil {
		target.FeatureID = strings.TrimSpace(*feature.IntegrationFeatureID)
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT branch, status FROM features WHERE id = ?`, target.FeatureID).Scan(&target.Branch, &status); err != nil {
			return featureIntegrationTarget{}, err
		}
		if status == string(FeatureArchived) {
			return featureIntegrationTarget{}, fmt.Errorf("integration target feature %s is archived", target.FeatureID)
		}
	}
	tip, found, err := flowgit.BranchTip(ctx, strings.TrimSpace(s.project.ExchangePath), target.Branch)
	if err != nil {
		return featureIntegrationTarget{}, fmt.Errorf("resolve integration target tip: %w", err)
	}
	if !found {
		return featureIntegrationTarget{}, fmt.Errorf("integration target branch %q not found in exchange remote", target.Branch)
	}
	target.TipSHA = tip
	return target, nil
}

func (s *FeatureService) openIntegrationDescendants(ctx context.Context, featureID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE descendants(id) AS (
	SELECT id FROM features WHERE integration_feature_id = ?
	UNION ALL
	SELECT f.id FROM features f JOIN descendants d ON f.integration_feature_id = d.id
)
SELECT d.id FROM descendants d JOIN features f ON f.id = d.id
WHERE f.status = 'open' ORDER BY d.id`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, "child feature "+id)
	}
	return ids, rows.Err()
}

func (s *FeatureService) requireNoOpenIntegrationDescendants(ctx context.Context, featureID string) error {
	children, err := s.openIntegrationDescendants(ctx, featureID)
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return &ErrFeatureActive{Offenders: children}
	}
	return nil
}

// allocateFeatureID mirrors allocateTaskID: per-project, gapless under the
// write transaction, formatted f-<project-key>-NNNN.
func (s *FeatureService) allocateFeatureID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	return s.allocateID(ctx, tx, "feature", "f")
}

// allocateRebaseID formats rb-<project-key>-NNNN from its own allocator row.
func (s *FeatureService) allocateRebaseID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	return s.allocateID(ctx, tx, "feature_rebase", "rb")
}

func (s *FeatureService) allocateID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, allocator, prefix string) (string, error) {
	var nextNumber int64
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = ?
RETURNING next_number - 1`, allocator).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate %s id: %w", allocator, err)
	}

	key, err := projectKeyFromID(s.project.ID)
	if err != nil {
		return "", fmt.Errorf("format %s id: %w", allocator, err)
	}
	return fmt.Sprintf("%s-%s-%04d", prefix, key, nextNumber), nil
}

type featureScanner interface {
	Scan(dest ...any) error
}

func scanFeature(row featureScanner) (Feature, error) {
	var feature Feature
	var status, createdBy string
	var createdAt, updatedAt string
	var integrationFeatureID, landedAt, landTargetFeatureID sql.NullString
	err := row.Scan(
		&feature.ID,
		&feature.Title,
		&feature.Body,
		&feature.Branch,
		&status,
		&integrationFeatureID,
		&feature.CreatedFromSHA,
		&createdBy,
		&createdAt,
		&updatedAt,
		&landedAt,
		&feature.LandSHA,
		&landTargetFeatureID,
		&feature.LandTargetBranch,
		&feature.LandTargetSHA,
	)
	if err != nil {
		return Feature{}, err
	}
	feature.Status = FeatureStatus(status)
	feature.IntegrationFeatureID = nullableStringPointer(integrationFeatureID)
	feature.LandTargetFeatureID = nullableStringPointer(landTargetFeatureID)
	feature.CreatedBy = Actor(createdBy)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Feature{}, fmt.Errorf("parse feature created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Feature{}, fmt.Errorf("parse feature updated_at: %w", err)
	}
	feature.CreatedAt = parsedCreatedAt
	feature.UpdatedAt = parsedUpdatedAt
	if landedAt.Valid {
		parsed, err := parseTime(landedAt.String)
		if err != nil {
			return Feature{}, fmt.Errorf("parse feature landed_at: %w", err)
		}
		feature.LandedAt = &parsed
	}
	return feature, nil
}

func scanFeatureRebase(row featureScanner) (FeatureRebase, error) {
	var rebase FeatureRebase
	var state string
	var createdAt string
	var targetFeatureID, completedAt sql.NullString
	err := row.Scan(
		&rebase.ID,
		&rebase.FeatureID,
		&rebase.TaskID,
		&rebase.OldTipSHA,
		&rebase.TargetBase,
		&rebase.TargetBaseSHA,
		&targetFeatureID,
		&rebase.NewTipSHA,
		&state,
		&createdAt,
		&completedAt,
		&rebase.RestrictBlockedTo,
	)
	if err != nil {
		return FeatureRebase{}, err
	}
	rebase.State = RebaseState(state)
	rebase.TargetFeatureID = nullableStringPointer(targetFeatureID)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return FeatureRebase{}, fmt.Errorf("parse rebase created_at: %w", err)
	}
	rebase.CreatedAt = parsedCreatedAt
	if completedAt.Valid {
		parsed, err := parseTime(completedAt.String)
		if err != nil {
			return FeatureRebase{}, fmt.Errorf("parse rebase completed_at: %w", err)
		}
		rebase.CompletedAt = &parsed
	}
	return rebase, nil
}
