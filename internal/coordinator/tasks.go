package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type ScheduleState string

const (
	ScheduleBacklog ScheduleState = "backlog"
	ScheduleUpNext  ScheduleState = "up_next"
	ScheduleClosed  ScheduleState = "closed"
)

type TaskState string

const (
	TaskStateTriage   TaskState = "triage"
	TaskStateBacklog  TaskState = "backlog"
	TaskStateUpNext   TaskState = "up_next"
	TaskStateClosed   TaskState = "closed"
	TaskStateRejected TaskState = "rejected"
)

type TriageState string

const (
	TriagePending  TriageState = "triage"
	TriageAccepted TriageState = "accepted"
	TriageRejected TriageState = "rejected"
)

type LifecycleState string

const (
	LifecycleScheduled  LifecycleState = "scheduled"
	LifecycleInProgress LifecycleState = "in_progress"
	LifecycleDone       LifecycleState = "done"
)

// AllLifecycleStates enumerates every LifecycleState constant in declaration
// order. It is the server's exhaustive lifecycle vocabulary: the task-relations
// parity test (internal/web/work_item_relations_parity_test.go) iterates it to prove
// the client's verdict covers every state the server serializes, so a newly
// added non-done state fails the build until the client allowlist catches up.
// TestAllLifecycleStatesExhaustive (internal/coordinator) parses every
// LifecycleState constant in this package with go/parser — no matter which
// file or declaration form, including derived values like
// `LifecycleScheduled + "_paused"`, whose inferred type is LifecycleState,
// and constants typed or converted through a type alias of LifecycleState —
// and fails if a constant is added without being listed here, so it is the
// drift guard that keeps this enumeration and the LifecycleState const block
// in lockstep.
var AllLifecycleStates = [...]LifecycleState{
	LifecycleScheduled,
	LifecycleInProgress,
	LifecycleDone,
}

type InProgressSubstate string

const (
	InProgressWorking InProgressSubstate = "working"
	InProgressBlocked InProgressSubstate = "blocked"
)

type Actor string

const (
	ActorHuman  Actor = "human"
	ActorAgent  Actor = "agent"
	ActorSystem Actor = "system"
)

type RelationKind string

const (
	RelationParentOf  RelationKind = "parent_of"
	RelationBlocks    RelationKind = "blocks"
	RelationRelatedTo RelationKind = "related_to"
)

type Task struct {
	ID                  string
	Title               string
	Body                string
	Priority            int
	ScheduleState       ScheduleState   `json:"-"`
	TriageState         TriageState     `json:"-"`
	RequiresHumanReview bool            `json:"requires_human_review"`
	AutoMerge           bool            `json:"-"`
	FlowID              string          `json:"flow_id,omitempty"`
	FeatureID           *string         `json:"feature_id,omitempty"`
	State               *LifecycleState `json:"state"`
	DoneResolution      *DoneResolution `json:"done_resolution,omitempty"`
	DoneMessage         string          `json:"done_message,omitempty"`
	DoneEvidence        []Evidence      `json:"done_evidence,omitempty"`
	DoneAt              *time.Time      `json:"done_at,omitempty"`
	CreatedBy           Actor
	CreatedBySessionID  *string
	SourceTaskID        *string
	SourceChangeID      *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ClosedAt            *time.Time `json:"-"`
}

type Tag struct {
	ID          int64
	Slug        string
	Name        string
	Color       string
	Description string
	CreatedBy   Actor
	CreatedAt   time.Time
}

type TaskRelation struct {
	SourceTaskID string
	TargetTaskID string
	Kind         RelationKind
	// SourceTitle and TargetTitle denormalize the related tasks' titles so UI
	// read models can render relation lists without an extra fetch per task.
	SourceTitle string
	TargetTitle string
	// SourceState and TargetState denormalize the related tasks' lifecycle
	// state ("" when unscheduled) so read models can tell whether a blocks
	// relation still bites — a blocker only counts until it reaches done —
	// without a second query per relation.
	SourceState LifecycleState
	TargetState LifecycleState
	// SourcePriority and SourceUpdatedAt denormalize the blocker (source)
	// task's priority and recency so read models can rank a task's live
	// blockers — most important first — without a second query per relation.
	SourcePriority  int
	SourceUpdatedAt time.Time
	CreatedBy       Actor
	CreatedAt       time.Time
}

type CreateTaskInput struct {
	Title               string
	Body                string
	Priority            int
	ScheduleState       ScheduleState
	TriageState         TriageState
	RequiresHumanReview *bool
	AutoMerge           *bool
	FlowID              string
	ParentItemID        string
	// FeatureID is a compatibility spelling for ParentItemID when the parent is
	// a feature. feature_id itself is a derived, read-only cache.
	FeatureID          *string
	CreatedBy          Actor
	CreatedBySessionID *string
	SourceTaskID       *string
	SourceChangeID     *string
}

type CreateTaskWithDetailsInput struct {
	Task              CreateTaskInput
	Tags              []CreateTagInput
	Relations         []CreateTaskRelationInput
	WorkItemRelations []CreateWorkItemRelationInput
}

type CreateTaskRelationInput struct {
	SourceTaskID string
	TargetTaskID string
	Kind         RelationKind
	CreatedBy    Actor
	// BlankTargetIsNewTask opts a relation into the shorthand where an empty
	// TargetTaskID resolves to the task being created. It is set by callers that
	// have authorized that form: session tokens relating their bound source task
	// to the new task, and create-task requests that set target_is_new_task so an
	// existing task (named as the source) can be linked parent_of/blocks the new
	// task. Owner/form requests that leave it false have a blank target rejected.
	BlankTargetIsNewTask bool
}

type EditTaskInput struct {
	Title               *string
	Body                *string
	Priority            *int
	RequiresHumanReview *bool
	AutoMerge           *bool
	FlowID              *string
	// FeatureID moves the task to a feature: non-nil requests a change, an
	// inner nil pointer clears the assignment, and a non-nil inner pointer
	// assigns that feature. The change is rejected once the task is in
	// progress or has a change row — moving the base mid-flight would strand
	// the change.
	FeatureID **string
}

type TaskFilter struct {
	LifecycleStates []string
	TagSlugs        []string
	// Search matches a case-insensitive substring against the task's title or
	// body; empty means no text filtering. The input is always bound as a
	// parameter, never interpolated into SQL.
	Search string
}

type CreateTagInput struct {
	Slug        string
	Name        string
	Color       string
	Description string
	CreatedBy   Actor
}

type Board struct {
	Unscheduled    []Task
	Scheduled      []Task
	InProgress     []Task
	Backlog        []Task `json:"-"`
	UpNext         []Task `json:"-"`
	NeedsAttention []Task `json:"-"`
}

// LaneState is the fine-grained derived sub-state of an open task. It is the
// outcome of the board precedence cascade before coarsening into one of the
// four board lanes, and is surfaced as a pill on task cards.
type LaneState string

const (
	LaneStateReadyToMerge     LaneState = "ready_to_merge"
	LaneStateChangesRequested LaneState = "changes_requested"
	LaneStateInProgress       LaneState = "in_progress"
	LaneStateInReview         LaneState = "in_review"
	LaneStateTriage           LaneState = "triage"
	LaneStateUpNext           LaneState = "up_next"
	LaneStateBacklog          LaneState = "backlog"
	LaneStateUnscheduled      LaneState = "unscheduled"
	LaneStateScheduled        LaneState = "scheduled"
	LaneStateWorking          LaneState = "working"
	// LaneStateAwaitingWorker marks an in-progress task whose active workflow
	// node has enqueued an author job that no worker has claimed yet. It is
	// derived from the live job state, not stored: the board says "Working"
	// while the job sits queued, so this surfaces the real stall.
	LaneStateAwaitingWorker LaneState = "awaiting_worker"
	LaneStateBlocked        LaneState = "blocked"
	// LaneStateHeld is an operator holding the run. It outranks blocked: who
	// owns the task is more useful than what the task was waiting on.
	LaneStateHeld LaneState = "held"
)

type WaitReason string

const (
	WaitReasonPhaseApproval WaitReason = "phase_approval"
	WaitReasonQuestion      WaitReason = "question"
	WaitReasonManualMerge   WaitReason = "manual_merge"
	WaitReasonHumanReview   WaitReason = "human_review"
	WaitReasonBlocked       WaitReason = "blocked"
	WaitReasonReviewCycles  WaitReason = "review_cycle_limit"
	WaitReasonCrashLoop     WaitReason = "crash_loop"
)

// BoardResult bundles the four board lanes with the per-task overlays the UI
// and CLI need: the fine-grained sub-state and the derived blocked overlay.
// Blocked tasks are routed to needs_attention while retaining their natural
// lane state for card badges and CLI annotations.
type BoardResult struct {
	Board       Board
	LaneStates  map[string]LaneState
	WaitReasons map[string]WaitReason
	BlockedIDs  []string
}

type TaskService struct {
	db        *sql.DB
	projectID string
	now       func() time.Time
	eventLog  *EventLogService

	// editTaskAfterReadTestHook runs after EditTask's optimistic validation read
	// and before its write transaction. Tests use it to force stale-read races.
	editTaskAfterReadTestHook func()
}

// SetEventLog wires the project event log; a nil log disables emission.
func (s *TaskService) SetEventLog(log *EventLogService) {
	s.eventLog = log
}

func NewTaskService(database *sql.DB, projectID string) *TaskService {
	return &TaskService{
		db:        database,
		projectID: strings.TrimSpace(projectID),
		now:       sqlitex.UTCNow,
	}
}

func (s *TaskService) CreateTask(ctx context.Context, input CreateTaskInput) (Task, error) {
	return s.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{Task: input})
}

func (s *TaskService) CreateTaskWithDetails(ctx context.Context, input CreateTaskWithDetailsInput) (Task, error) {
	taskInput, err := normalizeCreateTaskInput(input.Task)
	if err != nil {
		return Task{}, err
	}
	for i := range input.Tags {
		if input.Tags[i].CreatedBy == "" {
			input.Tags[i].CreatedBy = taskInput.CreatedBy
		}
		if _, err := normalizeCreateTagInput(input.Tags[i]); err != nil {
			return Task{}, err
		}
	}
	items := NewWorkItemService(s.db, s.projectID)
	items.now = s.now
	for i := range input.Relations {
		if input.Relations[i].CreatedBy == "" {
			input.Relations[i].CreatedBy = taskInput.CreatedBy
		}
		if input.Relations[i].Kind == "" {
			return Task{}, errors.New("task relation kind is required")
		}
		if strings.TrimSpace(input.Relations[i].TargetTaskID) == "" && !input.Relations[i].BlankTargetIsNewTask {
			return Task{}, errors.New("task relation target_task_id is required")
		}
		if input.Relations[i].BlankTargetIsNewTask {
			// The blank-target shorthand makes the new task the relation target, so
			// the other task must be named as the source and the target left blank.
			// Enforcing it here keeps the create transaction atomic: a child-of link
			// either commits with the task or rolls the whole create back, never a
			// committed task missing its parent.
			if strings.TrimSpace(input.Relations[i].TargetTaskID) != "" {
				return Task{}, errors.New("task relation target_is_new_task requires a blank target_task_id")
			}
			if strings.TrimSpace(input.Relations[i].SourceTaskID) == "" {
				return Task{}, errors.New("task relation target_is_new_task requires source_task_id to name the other task")
			}
		}
		if err := validateRelationKind(input.Relations[i].Kind); err != nil {
			return Task{}, err
		}
		if err := validateActor(input.Relations[i].CreatedBy); err != nil {
			return Task{}, err
		}
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Task{}, fmt.Errorf("begin create task transaction: %w", err)
	}
	defer tx.Rollback()

	createRelationPlan, err := items.prepareCreateRelations(ctx, tx, WorkItemTask, taskInput.ParentItemID, input.WorkItemRelations, taskInput.CreatedBy)
	if err != nil {
		return Task{}, err
	}
	id, err := s.allocateTaskID(ctx, tx)
	if err != nil {
		return Task{}, err
	}

	parentItemID := createRelationPlan.ParentItemID
	if taskInput.FeatureID != nil {
		featureID := strings.TrimSpace(*taskInput.FeatureID)
		if err := featureOpenForAssignmentTx(ctx, tx, featureID); err != nil {
			return Task{}, err
		}
		if parentItemID == "" {
			parentItemID = featureID
		}
	}
	if parentItemID != "" {
		parentKind, err := workItemKindTx(ctx, tx, parentItemID)
		if err != nil {
			return Task{}, err
		}
		if parentKind != WorkItemEpic && parentKind != WorkItemFeature {
			return Task{}, fmt.Errorf("%w: %s items cannot contain tasks", ErrWorkItemMoveConflict, parentKind)
		}
		open, err := workItemContainerOpenTx(ctx, tx, parentItemID, parentKind)
		if err != nil {
			return Task{}, err
		}
		if !open {
			return Task{}, ErrWorkItemParentClosed
		}
		inheritedFeatureID, err := nearestFeatureFromParentTx(ctx, tx, parentItemID)
		if err != nil {
			return Task{}, err
		}
		if taskInput.FeatureID != nil && strings.TrimSpace(*taskInput.FeatureID) != inheritedFeatureID {
			return Task{}, errors.New("task feature_id must match its nearest feature ancestor")
		}
		if inheritedFeatureID != "" {
			taskInput.FeatureID = &inheritedFeatureID
		} else {
			taskInput.FeatureID = nil
		}
	}

	now := s.now().UTC()
	nowText := formatTime(now)
	if err := insertWorkItem(ctx, tx, id, WorkItemTask, nowText); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id,
	title,
	body,
	priority,
	requires_human_review,
	flow_id,
	feature_id,
	created_by,
	created_by_session_id,
	source_task_id,
	source_change_id,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		taskInput.Title,
		taskInput.Body,
		taskInput.Priority,
		*taskInput.RequiresHumanReview,
		sqlitex.NullableNonEmptyString(taskInput.FlowID),
		nullableStringValue(taskInput.FeatureID),
		string(taskInput.CreatedBy),
		nullableStringValue(taskInput.CreatedBySessionID),
		nullableStringValue(taskInput.SourceTaskID),
		nullableStringValue(taskInput.SourceChangeID),
		nowText,
		nowText,
	); err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	// Parent insertion remains before legacy task relations so their historical
	// one-parent behavior and error ordering stay unchanged.
	parentPlan := createRelationPlan
	parentPlan.ParentItemID = parentItemID
	parentPlan.Relations = nil
	if _, err := items.linkCreateRelationsTx(ctx, tx, id, parentPlan, taskInput.CreatedBy); err != nil {
		return Task{}, err
	}

	for _, tagInput := range input.Tags {
		tagInput.CreatedBy = defaultActor(tagInput.CreatedBy, taskInput.CreatedBy)
		tagID, err := upsertTagInTx(ctx, tx, tagInput, nowText)
		if err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO task_tags (task_id, tag_id, created_by, created_at)
VALUES (?, ?, ?, ?)`,
			id,
			tagID,
			string(taskInput.CreatedBy),
			nowText,
		); err != nil {
			return Task{}, fmt.Errorf("tag task: %w", err)
		}
	}

	for _, relationInput := range input.Relations {
		sourceTaskID := strings.TrimSpace(relationInput.SourceTaskID)
		if sourceTaskID == "" {
			sourceTaskID = id
		}
		targetTaskID := strings.TrimSpace(relationInput.TargetTaskID)
		if targetTaskID == "" {
			if !relationInput.BlankTargetIsNewTask {
				return Task{}, errors.New("task relation target_task_id is required")
			}
			// Authorized shorthand: the newly created task is the relation target.
			targetTaskID = id
		}
		if err := linkTasksInTx(ctx, tx, sourceTaskID, targetTaskID, relationInput.Kind, defaultActor(relationInput.CreatedBy, taskInput.CreatedBy), nowText); err != nil {
			return Task{}, err
		}
	}
	remainingPlan := createRelationPlan
	remainingPlan.ParentItemID = ""
	remainingPlan.Relations = remainingPlan.Relations[:0]
	for _, relation := range createRelationPlan.Relations {
		if relation.Kind != RelationParentOf || !relation.TargetIsNewItem {
			remainingPlan.Relations = append(remainingPlan.Relations, relation)
		}
	}
	touched, err := items.linkCreateRelationsTx(ctx, tx, id, remainingPlan, taskInput.CreatedBy)
	if err != nil {
		return Task{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, touched, now); err != nil {
		return Task{}, err
	}

	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventTaskCreated,
		Actor:   string(taskInput.CreatedBy),
		TaskID:  id,
		Payload: eventPayload(map[string]any{"title": taskInput.Title}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Task{}, fmt.Errorf("commit create task: %w", err)
	}

	return s.GetTask(ctx, id)
}

func (s *TaskService) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
	id,
	title,
	body,
	priority,
	requires_human_review,
	flow_id,
	feature_id,
	created_by,
	created_by_session_id,
	source_task_id,
	source_change_id,
	created_at,
	updated_at,
	lifecycle_state,
	done_resolution,
	done_message,
	done_evidence_json,
	done_at
FROM tasks
WHERE id = ?`, id)

	task, err := scanTask(row)
	if err != nil {
		return Task{}, err
	}

	return task, nil
}

// taskSelectColumns is the canonical task column list, shared by every reader
// that scans rows with scanTasks. Keep the order in sync with scanTasks.
const taskSelectColumns = `
	i.id,
	i.title,
	i.body,
	i.priority,
	i.requires_human_review,
	i.flow_id,
	i.feature_id,
	i.created_by,
	i.created_by_session_id,
	i.source_task_id,
	i.source_change_id,
	i.created_at,
	i.updated_at,
	i.lifecycle_state,
	i.done_resolution,
	COALESCE(i.done_message, ''),
	COALESCE(i.done_evidence_json, ''),
	i.done_at`

func (s *TaskService) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	query := "SELECT" + taskSelectColumns + "\nFROM tasks i"

	var args []any
	var predicates []string
	if len(filter.TagSlugs) > 0 {
		query += `
JOIN task_tags it ON it.task_id = i.id
JOIN tags t ON t.id = it.tag_id`
		predicates = append(predicates, inPredicate("t.slug", len(filter.TagSlugs)))
		for _, slug := range filter.TagSlugs {
			args = append(args, slug)
		}
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		predicates = append(predicates, "(LOWER(i.title) LIKE ? OR LOWER(i.body) LIKE ?)")
		args = append(args, pattern, pattern)
	}
	if len(filter.LifecycleStates) > 0 {
		var statePredicates []string
		for _, state := range filter.LifecycleStates {
			switch strings.TrimSpace(state) {
			case "unscheduled":
				statePredicates = append(statePredicates, "i.lifecycle_state IS NULL")
			case string(LifecycleScheduled), string(LifecycleInProgress), string(LifecycleDone):
				statePredicates = append(statePredicates, "i.lifecycle_state = ?")
				args = append(args, strings.TrimSpace(state))
			}
		}
		if len(statePredicates) == 0 {
			return []Task{}, nil
		}
		predicates = append(predicates, "("+strings.Join(statePredicates, " OR ")+")")
	}
	if len(predicates) > 0 {
		query += "\nWHERE " + strings.Join(predicates, " AND ")
	}
	// CAST(substr(i.id, 3) AS INTEGER) orders legacy `t-<number>` ids
	// numerically. For canonical keyed ids (`t-<key>-NNNN`, see formatTaskID)
	// it does not parse the trailing task number: it casts the leading digits
	// of the substring after `t-` (the key itself when the key starts with a
	// digit, otherwise 0), so server list order is not numeric by task number
	// for keyed ids; the board applies its own trailing-suffix numeric sort
	// on top.
	query += "\nGROUP BY i.id\nORDER BY CAST(substr(i.id, 3) AS INTEGER)"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	return scanTasks(rows)
}

// SearchTasks ranks tasks matching query by full-text relevance (title and
// body, FTS5 bm25). The query is sanitized term-by-term into a prefix MATCH
// so user input can never inject FTS syntax. When the FTS index cannot answer
// (e.g. a phrase the tokenizer dropped to nothing), it falls back to the
// case-insensitive LIKE substring match ListTasks uses, so search never
// regresses below the pre-FTS behavior. limit <= 0 defaults to 50.
func (s *TaskService) SearchTasks(ctx context.Context, query string, limit int) ([]Task, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Task{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	match := fts5Query(query)
	if match == "" || !fts5Available {
		return s.searchTasksLike(ctx, query, limit)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT"+taskSelectColumns+`
FROM task_fts f
JOIN tasks i ON i.rowid = f.rowid
WHERE task_fts MATCH ?
ORDER BY bm25(task_fts)
LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search tasks: %w", err)
	}
	defer rows.Close()
	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		// FTS found nothing: the substring may still appear (e.g. inside a token
		// the prefix index does not start). Fall back to substring matching.
		return s.searchTasksLike(ctx, query, limit)
	}
	return tasks, nil
}

func (s *TaskService) searchTasksLike(ctx context.Context, query string, limit int) ([]Task, error) {
	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx, "SELECT"+taskSelectColumns+`
FROM tasks i
WHERE LOWER(i.title) LIKE ? OR LOWER(i.body) LIKE ?
ORDER BY i.updated_at DESC
LIMIT ?`, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("search tasks (like): %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// fts5Query builds a safe FTS5 MATCH expression from free text: each
// whitespace-separated run of word characters becomes a quoted prefix term
// ANDed with the others. Anything else (quotes, operators, punctuation runs)
// is dropped, so user input can never change the query's shape.
func fts5Query(query string) string {
	terms := strings.Fields(query)
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		word := strings.Map(func(r rune) rune {
			if r == '_' || r == '-' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || r > 127 {
				return r
			}
			return -1
		}, term)
		if word == "" {
			continue
		}
		parts = append(parts, `"`+word+`"*`)
	}
	return strings.Join(parts, " AND ")
}

// ListCompletions returns done tasks with their resolution, message, and
// evidence, newest first, for `flow audit completions`. resolution filters by
// terminal disposition (empty = any); limit <= 0 defaults to 100.
func (s *TaskService) ListCompletions(ctx context.Context, resolution string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := "SELECT" + taskSelectColumns + `
FROM tasks i
WHERE i.lifecycle_state = ?`
	args := []any{string(LifecycleDone)}
	if strings.TrimSpace(resolution) != "" {
		query += `
	AND i.done_resolution = ?`
		args = append(args, strings.TrimSpace(resolution))
	}
	query += `
ORDER BY i.done_at DESC, i.id DESC
LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list completions: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ClosedOutcome filters closed tasks by their terminal disposition. The empty
// value means "any outcome". The predicates mirror derivePhaseFromTask: a
// rejected triage wins over a merged change, which wins over abandonment.
type ClosedOutcome string

const (
	ClosedOutcomeAll       ClosedOutcome = ""
	ClosedOutcomeCompleted ClosedOutcome = "completed"
	ClosedOutcomeMerged    ClosedOutcome = "merged"
	ClosedOutcomeRejected  ClosedOutcome = "rejected"
	ClosedOutcomeAbandoned ClosedOutcome = "abandoned"
	ClosedOutcomeCancelled ClosedOutcome = "cancelled"
	ClosedOutcomeFailed    ClosedOutcome = "failed"
)

// ClosedTaskQuery bounds a page of closed tasks. It is deliberately separate
// from TaskFilter/ListTasks (the board + triage hot path) so the unbounded
// history reader can never widen those queries.
type ClosedTaskQuery struct {
	// Limit caps the page size; <= 0 falls back to defaultClosedTaskLimit.
	Limit int
	// Before/BeforeID is the keyset cursor: only rows strictly older than this
	// (done_at, id) pair are returned. Both come from a prior ClosedCursor.
	Before   *time.Time
	BeforeID string
	// Within, when set, restricts results to tasks closed at or after it.
	Within *time.Time
	// Outcome filters by terminal disposition; empty means any.
	Outcome ClosedOutcome
}

// ClosedCursor is the keyset position of the last returned row, used to fetch
// the next (older) page via ClosedTaskQuery.Before/BeforeID.
type ClosedCursor struct {
	ClosedAt time.Time
	ID       string
}

const defaultClosedTaskLimit = 50

// ListClosedTasks returns one keyset-paginated page of closed tasks ordered
// newest-closed first (done_at desc, id desc tiebreak). It never loads the
// full set: closed tasks grow unbounded, so callers must page or window. The
// returned cursor is non-nil only when more rows remain.
func (s *TaskService) ListClosedTasks(ctx context.Context, q ClosedTaskQuery) ([]Task, *ClosedCursor, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultClosedTaskLimit
	}

	predicates := []string{"i.lifecycle_state = ?", "i.done_at IS NOT NULL"}
	args := []any{string(LifecycleDone)}

	if q.Before != nil {
		before := formatTime(*q.Before)
		predicates = append(predicates, "(i.done_at < ? OR (i.done_at = ? AND i.id < ?))")
		args = append(args, before, before, q.BeforeID)
	}
	if q.Within != nil {
		predicates = append(predicates, "i.done_at >= ?")
		args = append(args, formatTime(*q.Within))
	}

	switch q.Outcome {
	case ClosedOutcomeAll:
		// No disposition predicate.
	case ClosedOutcomeCompleted, ClosedOutcomeMerged, ClosedOutcomeRejected, ClosedOutcomeAbandoned, ClosedOutcomeCancelled, ClosedOutcomeFailed:
		predicates = append(predicates, "i.done_resolution = ?")
		args = append(args, string(q.Outcome))
	default:
		return nil, nil, fmt.Errorf("invalid closed outcome %q", q.Outcome)
	}

	query := "SELECT" + taskSelectColumns + "\nFROM tasks i\nWHERE " + strings.Join(predicates, " AND ") +
		"\nORDER BY i.done_at DESC, i.id DESC\nLIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list closed tasks: %w", err)
	}
	defer rows.Close()

	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, nil, err
	}

	var next *ClosedCursor
	if len(tasks) > limit {
		tasks = tasks[:limit]
		last := tasks[limit-1]
		if last.DoneAt != nil {
			next = &ClosedCursor{ClosedAt: *last.DoneAt, ID: last.ID}
		}
	}

	return tasks, next, nil
}

// CountClosedTasks returns the total number of closed tasks, for the nav
// badge. It is a cheap indexed COUNT and intentionally ignores disposition.
func (s *TaskService) CountClosedTasks(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM tasks
WHERE lifecycle_state = ?`, string(LifecycleDone)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count closed tasks: %w", err)
	}

	return count, nil
}

// ClosedTaskWindowCount is one cumulative time window of successful task
// completions: Count is the total and ByOutcome splits it by resolution.
type ClosedTaskWindowCount struct {
	Window    time.Duration
	Count     int
	ByOutcome map[DoneResolution]int
}

// CountClosedTasksByWindow counts tasks closed with one of the given outcomes
// within each window. Windows are cumulative: a task closed at now-10m counts
// toward every window of 15m or larger, and a task closed exactly at a window
// edge counts in that window. It is a single grouped query over done_at /
// done_resolution bounded by the largest window, so the unbounded closed-task
// history is never loaded. Windows must be positive and outcomes must be known
// resolutions.
func (s *TaskService) CountClosedTasksByWindow(ctx context.Context, windows []time.Duration, outcomes []DoneResolution) ([]ClosedTaskWindowCount, error) {
	if len(windows) == 0 {
		return nil, errors.New("count closed tasks by window: at least one window is required")
	}
	if len(outcomes) == 0 {
		return nil, errors.New("count closed tasks by window: at least one outcome is required")
	}

	maxWindow := time.Duration(0)
	for _, window := range windows {
		if window <= 0 {
			return nil, fmt.Errorf("count closed tasks by window: invalid window %s: must be positive", window)
		}
		if window > maxWindow {
			maxWindow = window
		}
	}

	resolutions := make([]any, 0, len(outcomes))
	seen := make(map[DoneResolution]bool, len(outcomes))
	for _, outcome := range outcomes {
		if err := ValidateDoneResolution(outcome); err != nil {
			return nil, fmt.Errorf("count closed tasks by window: %w", err)
		}
		if seen[outcome] {
			continue
		}
		seen[outcome] = true
		resolutions = append(resolutions, string(outcome))
	}

	// Capture one timestamp for the SQL lower bound and every in-memory bucket
	// cutoff so a single request has one stable definition of now. Using two
	// snapshots would let a completion at exactly the first now-maxWindow edge
	// pass the SQL filter yet fall before a later bucket cutoff.
	now := s.now().UTC()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(resolutions)), ",")
	args := []any{string(LifecycleDone), formatTime(now.Add(-maxWindow))}
	args = append(args, resolutions...)
	rows, err := s.db.QueryContext(ctx, `
SELECT done_at, done_resolution, COUNT(*)
FROM tasks
WHERE lifecycle_state = ? AND done_at IS NOT NULL AND done_at >= ?
	AND done_resolution IN (`+placeholders+`)
GROUP BY done_at, done_resolution`, args...)
	if err != nil {
		return nil, fmt.Errorf("count closed tasks by window: %w", err)
	}
	defer rows.Close()

	type group struct {
		closedAt time.Time
		outcome  DoneResolution
		count    int
	}
	var groups []group
	for rows.Next() {
		var closedAt string
		var outcome string
		var count int
		if err := rows.Scan(&closedAt, &outcome, &count); err != nil {
			return nil, fmt.Errorf("count closed tasks by window: %w", err)
		}
		at, err := sqlitex.ParseTime(closedAt)
		if err != nil {
			return nil, fmt.Errorf("count closed tasks by window: parse done_at %q: %w", closedAt, err)
		}
		groups = append(groups, group{closedAt: at, outcome: DoneResolution(outcome), count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count closed tasks by window: %w", err)
	}

	counts := make([]ClosedTaskWindowCount, len(windows))
	for i, window := range windows {
		cutoff := now.Add(-window)
		bucket := ClosedTaskWindowCount{Window: window, ByOutcome: make(map[DoneResolution]int, len(seen))}
		for _, g := range groups {
			if g.closedAt.Before(cutoff) {
				continue
			}
			bucket.Count += g.count
			bucket.ByOutcome[g.outcome] += g.count
		}
		counts[i] = bucket
	}
	return counts, nil
}

func (s *TaskService) EditTask(ctx context.Context, id string, input EditTaskInput) (Task, error) {
	current, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}

	if input.Title != nil && strings.TrimSpace(*input.Title) == "" {
		return Task{}, errors.New("task title is required")
	}
	if input.Priority != nil && *input.Priority < 0 {
		return Task{}, errors.New("task priority must be non-negative")
	}
	parentID := ""
	if input.FeatureID != nil {
		featureID, err := s.guardFeatureChange(ctx, current, *input.FeatureID)
		if err != nil {
			return Task{}, err
		}
		if featureID != nil {
			parentID = *featureID
		}
	}
	if hook := s.editTaskAfterReadTestHook; hook != nil {
		hook()
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Task{}, fmt.Errorf("begin edit task transaction: %w", err)
	}
	defer tx.Rollback()
	locked, err := taskInTx(ctx, tx, id)
	if err != nil {
		return Task{}, err
	}
	if input.RequiresHumanReview != nil && locked.State != nil && locked.RequiresHumanReview != *input.RequiresHumanReview {
		return Task{}, fmt.Errorf("%w: human review policy is frozen after scheduling", ErrWorkflowConflict)
	}

	// Base every edit on the row protected by this write transaction. The
	// optimistic read above may be stale after a concurrent edit or schedule.
	current = locked
	if input.FeatureID != nil {
		items := NewWorkItemService(s.db, s.projectID)
		items.now = s.now
		if err := items.moveTx(ctx, tx, current.ID, parentID, ActorHuman); err != nil {
			return Task{}, err
		}
		current, err = taskInTx(ctx, tx, id)
		if err != nil {
			return Task{}, err
		}
	}
	if input.Title != nil {
		current.Title = strings.TrimSpace(*input.Title)
	}
	if input.Body != nil {
		current.Body = *input.Body
	}
	if input.Priority != nil {
		current.Priority = *input.Priority
	}
	if input.RequiresHumanReview != nil {
		current.RequiresHumanReview = *input.RequiresHumanReview
	}
	if input.FlowID != nil {
		current.FlowID = strings.TrimSpace(*input.FlowID)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks
SET
	title = ?,
	body = ?,
	priority = ?,
	requires_human_review = ?,
	flow_id = ?,
	updated_at = ?
WHERE id = ?`,
		current.Title,
		current.Body,
		current.Priority,
		current.RequiresHumanReview,
		sqlitex.NullableNonEmptyString(current.FlowID),
		formatTime(s.now().UTC()),
		id,
	); err != nil {
		return Task{}, fmt.Errorf("edit task: %w", err)
	}

	// emit the edit event inside the write transaction
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventTaskEdited,
		TaskID:  id,
		Payload: eventPayload(map[string]any{"title": current.Title}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Task{}, fmt.Errorf("commit edit task: %w", err)
	}

	edited, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return edited, nil
}

func (s *TaskService) ScheduleTask(ctx context.Context, id string, state ScheduleState) (Task, error) {
	return Task{}, errors.New("legacy schedule states were removed; schedule the task through its workflow")
}

func (s *TaskService) CloseTask(ctx context.Context, id string) (Task, error) {
	return Task{}, errors.New("legacy close was removed; complete the task through its workflow")
}

func (s *TaskService) SetTaskState(ctx context.Context, id string, state TaskState) (Task, error) {
	return Task{}, errors.New("legacy task states were removed; use workflow lifecycle operations")
}

func (s *TaskService) AcceptTriage(ctx context.Context, id string) (Task, error) {
	return Task{}, errors.New("triage was removed from the task lifecycle")
}

func (s *TaskService) RejectTriage(ctx context.Context, id string) (Task, error) {
	return Task{}, errors.New("triage was removed from the task lifecycle")
}

func (s *TaskService) CreateTag(ctx context.Context, input CreateTagInput) (Tag, error) {
	input, err := normalizeCreateTagInput(input)
	if err != nil {
		return Tag{}, err
	}

	nowText := formatTime(s.now().UTC())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO tags (slug, name, color, description, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		input.Slug,
		input.Name,
		input.Color,
		input.Description,
		string(input.CreatedBy),
		nowText,
	)
	if err != nil {
		return Tag{}, fmt.Errorf("create tag: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Tag{}, fmt.Errorf("read tag id: %w", err)
	}

	return s.GetTag(ctx, id)
}

func (s *TaskService) GetTag(ctx context.Context, id int64) (Tag, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, slug, name, color, description, created_by, created_at
FROM tags
WHERE id = ?`, id)

	tag, err := scanTag(row)
	if err != nil {
		return Tag{}, err
	}

	return tag, nil
}

func (s *TaskService) TagsForTask(ctx context.Context, taskID string) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t.slug, t.name, t.color, t.description, t.created_by, t.created_at
FROM tags t
JOIN task_tags it ON it.tag_id = t.id
WHERE it.task_id = ?
ORDER BY t.slug`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task tags: %w", err)
	}

	return tags, nil
}

func (s *TaskService) TagTask(ctx context.Context, taskID string, tagID int64, actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO task_tags (task_id, tag_id, created_by, created_at)
VALUES (?, ?, ?, ?)`,
		taskID,
		tagID,
		string(actor),
		formatTime(s.now().UTC()),
	); err != nil {
		return fmt.Errorf("tag task: %w", err)
	}

	return nil
}

func upsertTagInTx(ctx context.Context, tx workItemRelationQuerier, input CreateTagInput, nowText string) (int64, error) {
	input, err := normalizeCreateTagInput(input)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO tags (slug, name, color, description, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(slug) DO NOTHING`,
		input.Slug,
		input.Name,
		input.Color,
		input.Description,
		string(input.CreatedBy),
		nowText,
	); err != nil {
		return 0, fmt.Errorf("create tag: %w", err)
	}

	var tagID int64
	if err := tx.QueryRowContext(ctx, `
SELECT id
FROM tags
WHERE slug = ?`, input.Slug).Scan(&tagID); err != nil {
		return 0, fmt.Errorf("load tag: %w", err)
	}

	return tagID, nil
}

func (s *TaskService) UntagTask(ctx context.Context, taskID string, tagID int64) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM task_tags
WHERE task_id = ? AND tag_id = ?`, taskID, tagID); err != nil {
		return fmt.Errorf("untag task: %w", err)
	}

	return nil
}

// BlockOnRebase inserts (rebaseTask blocks task) relations for every id,
// tolerating duplicates and skipping the rebase task itself. A running
// feature rebase holds the feature's other tasks at the workflow dependency
// gate through these ordinary blocks relations; done blockers resolve by
// definition, so release needs no cleanup. A relation that would cycle is
// skipped rather than failing the caller's schedule path.
func (s *TaskService) BlockOnRebase(ctx context.Context, rebaseTaskID string, taskIDs []string) error {
	rebaseTaskID = strings.TrimSpace(rebaseTaskID)
	if rebaseTaskID == "" {
		return errors.New("rebase task id is required")
	}
	nowText := formatTime(s.now().UTC())
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" || taskID == rebaseTaskID {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		cycle, err := workItemDependencyPathExists(ctx, tx, taskID, rebaseTaskID)
		if err != nil {
			tx.Rollback()
			return err
		}
		if cycle {
			tx.Rollback()
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO work_item_relations (source_item_id, target_item_id, kind, created_by, created_at)
VALUES (?, ?, ?, ?, ?)`,
			rebaseTaskID, taskID, string(RelationBlocks), string(ActorSystem), nowText); err != nil {
			tx.Rollback()
			return fmt.Errorf("block task on rebase: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit rebase block: %w", err)
		}
	}

	return nil
}

func (s *TaskService) LinkTasks(ctx context.Context, sourceTaskID, targetTaskID string, kind RelationKind, actor Actor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin link task transaction: %w", err)
	}
	defer tx.Rollback()

	if err := linkTasksInTx(ctx, tx, sourceTaskID, targetTaskID, kind, actor, formatTime(s.now().UTC())); err != nil {
		return err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{sourceTaskID, targetTaskID}, s.now().UTC()); err != nil {
		return err
	}

	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventRelationLinked,
		Actor:   string(actor),
		TaskID:  targetTaskID,
		Payload: eventPayload(map[string]any{"source": sourceTaskID, "target": targetTaskID, "relation": string(kind)}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit link tasks: %w", err)
	}

	return nil
}

func linkTasksInTx(ctx context.Context, tx workItemRelationQuerier, sourceTaskID, targetTaskID string, kind RelationKind, actor Actor, nowText string) error {
	sourceTaskID = strings.TrimSpace(sourceTaskID)
	targetTaskID = strings.TrimSpace(targetTaskID)
	if sourceTaskID == "" || targetTaskID == "" {
		return errors.New("task relation source_task_id and target_task_id are required")
	}
	if sourceTaskID == targetTaskID {
		return errors.New("task relation cannot target itself")
	}
	if err := validateRelationKind(kind); err != nil {
		return err
	}
	if err := validateActor(actor); err != nil {
		return err
	}
	if err := taskExistsInTx(ctx, tx, sourceTaskID); err != nil {
		if kind == RelationParentOf && errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("the selected parent cannot be used: task %s does not exist or is not accessible", sourceTaskID)
		}
		return fmt.Errorf("task relation references a task that does not exist: %s", sourceTaskID)
	}
	if err := taskExistsInTx(ctx, tx, targetTaskID); err != nil {
		return fmt.Errorf("task relation references a task that does not exist: %s", targetTaskID)
	}

	if kind == RelationParentOf {
		hasParent, err := taskHasParent(ctx, tx, targetTaskID)
		if err != nil {
			return err
		}
		if hasParent {
			return errors.New("task already has a parent")
		}
	}
	if kind == RelationParentOf || kind == RelationBlocks {
		fromID, toID := sourceTaskID, targetTaskID
		if kind == RelationParentOf {
			fromID, toID = targetTaskID, sourceTaskID
		}
		cycle, err := workItemDependencyPathExists(ctx, tx, toID, fromID)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("%s relation would create a dependency cycle", kind)
		}
	}
	if kind == RelationRelatedTo && targetTaskID < sourceTaskID {
		sourceTaskID, targetTaskID = targetTaskID, sourceTaskID
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO work_item_relations (source_item_id, target_item_id, kind, created_by, created_at)
VALUES (?, ?, ?, ?, ?)`,
		sourceTaskID,
		targetTaskID,
		string(kind),
		string(actor),
		nowText,
	); err != nil {
		if isForeignKeyViolation(err) {
			return relationFKError(ctx, tx, sourceTaskID, targetTaskID, kind, err)
		}
		return fmt.Errorf("link tasks: %w", err)
	}

	return nil
}

// relationFKError translates a work_item_relations foreign-key failure into a
// message a user can act on. The insert references a task id that does not
// exist in this project's database (a stale or cross-project id), so instead
// of surfacing SQLite's raw "FOREIGN KEY constraint failed" text we say which
// side is unavailable. For a parent_of relation the source is the parent: the
// new-task child-of form relies on this to tell the user the chosen parent
// cannot be used, while the transaction rollback still guarantees that the
// failed create commits no task.
func relationFKError(ctx context.Context, tx workItemRelationQuerier, sourceTaskID, targetTaskID string, kind RelationKind, fkErr error) error {
	sourceErr := taskExistsInTx(ctx, tx, sourceTaskID)
	targetErr := taskExistsInTx(ctx, tx, targetTaskID)
	switch {
	case sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows):
		return fmt.Errorf("link tasks: %w", fkErr)
	case targetErr != nil && !errors.Is(targetErr, sql.ErrNoRows):
		return fmt.Errorf("link tasks: %w", fkErr)
	case sourceErr != nil && kind == RelationParentOf:
		return fmt.Errorf("the selected parent cannot be used: task %s does not exist or is not accessible", sourceTaskID)
	case targetErr != nil && kind == RelationParentOf:
		return fmt.Errorf("the selected child cannot be used: task %s does not exist", targetTaskID)
	case sourceErr != nil:
		return fmt.Errorf("task relation references a task that does not exist: %s", sourceTaskID)
	case targetErr != nil:
		return fmt.Errorf("task relation references a task that does not exist: %s", targetTaskID)
	default:
		return fmt.Errorf("link tasks: %w", fkErr)
	}
}

func (s *TaskService) UnlinkTasks(ctx context.Context, sourceTaskID, targetTaskID string, kind RelationKind) error {
	if err := validateRelationKind(kind); err != nil {
		return err
	}
	if kind == RelationRelatedTo && targetTaskID < sourceTaskID {
		sourceTaskID, targetTaskID = targetTaskID, sourceTaskID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unlink task transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
DELETE FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = ?`,
		sourceTaskID,
		targetTaskID,
		string(kind),
	); err != nil {
		return fmt.Errorf("unlink tasks: %w", err)
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{sourceTaskID, targetTaskID}, s.now().UTC()); err != nil {
		return err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventRelationUnlinked,
		TaskID:  targetTaskID,
		Payload: eventPayload(map[string]any{"source": sourceTaskID, "target": targetTaskID, "relation": string(kind)}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unlink tasks: %w", err)
	}
	return nil
}

func (s *TaskService) RelationsForTask(ctx context.Context, taskID string) ([]TaskRelation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	r.source_item_id,
	r.target_item_id,
	r.kind,
	s.title,
	t.title,
	s.lifecycle_state,
	t.lifecycle_state,
	s.priority,
	s.updated_at,
	r.created_by,
	r.created_at
FROM work_item_relations r
JOIN tasks s ON s.id = r.source_item_id
JOIN tasks t ON t.id = r.target_item_id
WHERE r.source_item_id = ? OR r.target_item_id = ?
ORDER BY r.created_at, r.source_item_id, r.target_item_id, r.kind`, taskID, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task relations: %w", err)
	}
	defer rows.Close()

	var relations []TaskRelation
	for rows.Next() {
		relation, err := scanTaskRelation(rows)
		if err != nil {
			return nil, err
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task relations: %w", err)
	}

	return relations, nil
}

// RelationsForTasks returns title-bearing relations for a set of task IDs in a
// single query, keyed by the task each relation involves. A relation between
// two requested tasks appears under both keys. It exists so board-style read
// models can load relations for many tasks without an N+1 query.
func (s *TaskService) RelationsForTasks(ctx context.Context, taskIDs []string) (map[string][]TaskRelation, error) {
	result := make(map[string][]TaskRelation, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}

	ids := make([]string, 0, len(taskIDs))
	seen := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		ids = append(ids, trimmed)
	}
	if len(ids) == 0 {
		return result, nil
	}

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT
	r.source_item_id,
	r.target_item_id,
	r.kind,
	s.title,
	t.title,
	s.lifecycle_state,
	t.lifecycle_state,
	s.priority,
	s.updated_at,
	r.created_by,
	r.created_at
FROM work_item_relations r
JOIN tasks s ON s.id = r.source_item_id
JOIN tasks t ON t.id = r.target_item_id
WHERE `+inPredicate("r.source_item_id", len(ids))+` OR `+inPredicate("r.target_item_id", len(ids))+`
ORDER BY r.created_at, r.source_item_id, r.target_item_id, r.kind`, append(args, args...)...)
	if err != nil {
		return nil, fmt.Errorf("list task relations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		relation, err := scanTaskRelation(rows)
		if err != nil {
			return nil, err
		}
		if seen[relation.SourceTaskID] {
			result[relation.SourceTaskID] = append(result[relation.SourceTaskID], relation)
		}
		if relation.TargetTaskID != relation.SourceTaskID && seen[relation.TargetTaskID] {
			result[relation.TargetTaskID] = append(result[relation.TargetTaskID], relation)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task relations: %w", err)
	}

	return result, nil
}

func (s *TaskService) Board(ctx context.Context) (Board, error) {
	result, err := s.BoardResult(ctx)
	if err != nil {
		return Board{}, err
	}
	return result.Board, nil
}

func (s *TaskService) BoardResult(ctx context.Context) (BoardResult, error) {
	tasks, err := s.ListTasks(ctx, TaskFilter{})
	if err != nil {
		return BoardResult{}, err
	}

	result := BoardResult{LaneStates: map[string]LaneState{}, WaitReasons: map[string]WaitReason{}}
	for _, task := range tasks {
		if task.State == nil {
			result.Board.Unscheduled = append(result.Board.Unscheduled, task)
			result.LaneStates[task.ID] = LaneStateUnscheduled
			continue
		}
		switch *task.State {
		case LifecycleScheduled:
			result.Board.Scheduled = append(result.Board.Scheduled, task)
			result.LaneStates[task.ID] = LaneStateScheduled
		case LifecycleInProgress:
			result.Board.InProgress = append(result.Board.InProgress, task)
			openWaits, held, heldBySystem, err := s.openWaitAndHoldCounts(ctx, task.ID)
			if err != nil {
				return BoardResult{}, err
			}
			switch {
			case held > 0:
				result.LaneStates[task.ID] = LaneStateHeld
				// A convergence hold blocks the task on a human scope
				// decision, so it counts as blocked too: the board counters
				// and the task card both say so. A manual hold does not —
				// the operator already owns the task.
				if heldBySystem > 0 {
					result.BlockedIDs = append(result.BlockedIDs, task.ID)
					result.WaitReasons[task.ID] = WaitReasonBlocked
				}
			case openWaits > 0:
				result.LaneStates[task.ID] = LaneStateBlocked
				result.BlockedIDs = append(result.BlockedIDs, task.ID)
				result.WaitReasons[task.ID] = WaitReasonBlocked
			default:
				result.LaneStates[task.ID] = LaneStateWorking
			}
		case LifecycleDone:
			// Done is served by the paginated Done reader.
		}
	}

	if err := s.applyAwaitingWorkerStates(ctx, result.LaneStates); err != nil {
		return BoardResult{}, err
	}

	return result, nil
}

// applyAwaitingWorkerStates refines the working lane state for in-progress
// tasks whose active workflow node has a live author job no worker has claimed
// yet. It runs one batched lookup across every task currently classified
// working: each task's active workflow run is joined to its current node run
// (the one identified by workflow_runs.current_node_run_id, i.e. the current
// visit) and then to any live job on that node run. A live job in state queued
// or claimed means the task is actually awaiting a worker; a running job (or no
// live job, e.g. a synchronous merge/checks node) stays working. Selecting by
// current_node_run_id rather than the greatest attempt matters on a revisit:
// RetryExecution bumps attempt on the earlier visit's node run, so a retried
// earlier visit can carry a higher attempt than the fresh current visit; the
// current_node_run_id always points at the live visit. Held and blocked tasks
// never reach here, so those states keep precedence regardless of job state.
func (s *TaskService) applyAwaitingWorkerStates(ctx context.Context, laneStates map[string]LaneState) error {
	workingIDs := make([]string, 0, len(laneStates))
	for taskID, state := range laneStates {
		if state == LaneStateWorking {
			workingIDs = append(workingIDs, taskID)
		}
	}
	if len(workingIDs) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(workingIDs)), ",")
	args := make([]any, len(workingIDs))
	for i, id := range workingIDs {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT r.task_id, j.state
FROM workflow_runs r
JOIN workflow_node_runs n ON n.id = r.current_node_run_id
JOIN jobs j ON j.node_run_id = n.id AND j.state IN ('queued', 'claimed', 'running')
WHERE r.task_id IN (`+placeholders+`)
	AND r.state IN ('scheduled', 'running', 'waiting')`, args...)
	if err != nil {
		return fmt.Errorf("query live jobs for awaiting-worker lane states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, jobState string
		if err := rows.Scan(&taskID, &jobState); err != nil {
			return fmt.Errorf("scan live job lane state: %w", err)
		}
		if jobState == "queued" || jobState == "claimed" {
			laneStates[taskID] = LaneStateAwaitingWorker
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate live job lane states: %w", err)
	}
	return nil
}

func pendingHumanReview(ctx context.Context, db *sql.DB, taskID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM checks
WHERE task_id = ?
	AND kind = ?
	AND required = 1
	AND verdict = ?`,
		taskID,
		string(CheckKindHuman),
		string(CheckPending),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check pending human review: %w", err)
	}

	return count > 0, nil
}

func crashLoopStatusExists(ctx context.Context, db *sql.DB, taskID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM status_log
WHERE task_id = ?
	AND kind = ?
	AND message LIKE ?
	AND resolved_at IS NULL`,
		taskID,
		StatusKindBlocker,
		crashRestartLimitMessageLike,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check crash loop status: %w", err)
	}

	return count > 0, nil
}

func (s *TaskService) CrashRetryAvailable(ctx context.Context, taskID string) (bool, error) {
	return crashLoopStatusExists(ctx, s.db, taskID)
}

func (s *TaskService) reviewState(ctx context.Context, taskID string) (ReviewState, error) {
	return reviewStateForTask(ctx, s.db, taskID)
}

func (s *TaskService) taskIsBlocked(ctx context.Context, taskID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM work_item_relations r
JOIN tasks blocker ON blocker.id = r.source_item_id
	WHERE r.kind = ?
		AND r.target_item_id = ?
		AND (blocker.lifecycle_state IS NULL OR blocker.lifecycle_state != ?)`,
		string(RelationBlocks),
		taskID,
		string(LifecycleDone),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check task blockers: %w", err)
	}

	return count > 0, nil
}

// TaskIDsWithSource returns tasks an agent session created while working the
// given task. Together with the parent_of relation the task-set materializer
// writes, this is how an epic finds its members.
func (s *TaskService) TaskIDsWithSource(ctx context.Context, sourceTaskID string) ([]string, error) {
	sourceTaskID = strings.TrimSpace(sourceTaskID)
	if sourceTaskID == "" {
		return nil, errors.New("source task id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM tasks WHERE source_task_id = ? ORDER BY id`, sourceTaskID)
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
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *TaskService) UnresolvedBlockers(ctx context.Context, taskID string) ([]Task, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	blocker.id,
	blocker.title,
	blocker.body,
	blocker.priority,
	blocker.requires_human_review,
	blocker.flow_id,
	blocker.feature_id,
	blocker.created_by,
	blocker.created_by_session_id,
	blocker.source_task_id,
	blocker.source_change_id,
	blocker.created_at,
	blocker.updated_at,
	blocker.lifecycle_state,
	blocker.done_resolution,
	COALESCE(blocker.done_message, ''),
	COALESCE(blocker.done_evidence_json, ''),
	blocker.done_at
FROM work_item_relations r
JOIN tasks blocker ON blocker.id = r.source_item_id
	WHERE r.kind = ?
		AND r.target_item_id IN (
			WITH RECURSIVE ancestors(id) AS (
				VALUES (?)
				UNION ALL
				SELECT parent.source_item_id
				FROM ancestors
				JOIN work_item_relations parent
					ON parent.target_item_id = ancestors.id AND parent.kind = 'parent_of'
			)
			SELECT id FROM ancestors
		)
		AND (blocker.lifecycle_state IS NULL OR blocker.lifecycle_state != ?)
ORDER BY blocker.priority DESC, blocker.updated_at DESC, blocker.id`,
		string(RelationBlocks),
		taskID,
		string(LifecycleDone),
	)
	if err != nil {
		return nil, fmt.Errorf("list task blockers: %w", err)
	}
	defer rows.Close()

	var blockers []Task
	for rows.Next() {
		blocker, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		blockers = append(blockers, blocker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task blockers: %w", err)
	}

	return blockers, nil
}

func (s *TaskService) allocateTaskID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var nextNumber int64
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'task'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate task id: %w", err)
	}

	return formatTaskID(s.projectID, nextNumber)
}

func formatTaskID(projectID string, number int64) (string, error) {
	key, err := projectKeyFromID(projectID)
	if err != nil {
		return "", fmt.Errorf("format task id: %w", err)
	}

	return fmt.Sprintf("t-%s-%04d", key, number), nil
}

func normalizeCreateTaskInput(input CreateTaskInput) (CreateTaskInput, error) {
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return CreateTaskInput{}, errors.New("task title is required")
	}
	if input.Priority < 0 {
		return CreateTaskInput{}, errors.New("task priority must be non-negative")
	}

	if input.CreatedBy == "" {
		input.CreatedBy = ActorHuman
	}
	if err := validateActor(input.CreatedBy); err != nil {
		return CreateTaskInput{}, err
	}
	if input.CreatedBy == ActorAgent && (input.CreatedBySessionID == nil || strings.TrimSpace(*input.CreatedBySessionID) == "") {
		return CreateTaskInput{}, errors.New("agent-created tasks require created_by_session_id")
	}

	if input.ScheduleState == "" {
		input.ScheduleState = ScheduleBacklog
	}
	if err := validateScheduleState(input.ScheduleState); err != nil {
		return CreateTaskInput{}, err
	}
	if input.ScheduleState == ScheduleClosed {
		return CreateTaskInput{}, errors.New("tasks cannot be created closed")
	}

	if input.TriageState == "" {
		if input.CreatedBy == ActorAgent {
			input.TriageState = TriagePending
		} else {
			input.TriageState = TriageAccepted
		}
	}
	if err := validateTriageState(input.TriageState); err != nil {
		return CreateTaskInput{}, err
	}
	if input.TriageState == TriageRejected {
		return CreateTaskInput{}, errors.New("tasks cannot be created rejected")
	}
	if input.ScheduleState != ScheduleBacklog && input.TriageState != TriageAccepted {
		return CreateTaskInput{}, errors.New("only accepted tasks can be scheduled")
	}

	if input.RequiresHumanReview == nil {
		defaultRequiresHumanReview := false
		input.RequiresHumanReview = &defaultRequiresHumanReview
	}
	if input.AutoMerge == nil {
		defaultAutoMerge := false
		input.AutoMerge = &defaultAutoMerge
	}

	return input, nil
}

func normalizeCreateTagInput(input CreateTagInput) (CreateTagInput, error) {
	input.Slug = strings.TrimSpace(input.Slug)
	input.Name = strings.TrimSpace(input.Name)
	input.Color = strings.TrimSpace(input.Color)
	if input.Name == "" {
		input.Name = input.Slug
	}
	if input.CreatedBy == "" {
		input.CreatedBy = ActorHuman
	}

	if err := validateTagSlug(input.Slug); err != nil {
		return CreateTagInput{}, err
	}
	if input.Name == "" {
		return CreateTagInput{}, errors.New("tag name is required")
	}
	if err := validateActor(input.CreatedBy); err != nil {
		return CreateTagInput{}, err
	}

	return input, nil
}

func validateScheduleState(state ScheduleState) error {
	switch state {
	case ScheduleBacklog, ScheduleUpNext, ScheduleClosed:
		return nil
	default:
		return fmt.Errorf("invalid schedule state: %s", state)
	}
}

func validateTriageState(state TriageState) error {
	switch state {
	case TriagePending, TriageAccepted, TriageRejected:
		return nil
	default:
		return fmt.Errorf("invalid triage state: %s", state)
	}
}

func validateActor(actor Actor) error {
	switch actor {
	case ActorHuman, ActorAgent, ActorSystem:
		return nil
	default:
		return fmt.Errorf("invalid actor: %s", actor)
	}
}

func defaultActor(value Actor, fallback Actor) Actor {
	if value == "" {
		return fallback
	}

	return value
}

func validateRelationKind(kind RelationKind) error {
	switch kind {
	case RelationParentOf, RelationBlocks, RelationRelatedTo:
		return nil
	default:
		return fmt.Errorf("invalid relation kind: %s", kind)
	}
}

func validateTagSlug(slug string) error {
	if slug == "" {
		return errors.New("tag slug is required")
	}
	for i, r := range slug {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			return fmt.Errorf("invalid tag slug: %s", slug)
		}
		if i == 0 && r == '-' {
			return fmt.Errorf("invalid tag slug: %s", slug)
		}
	}
	if strings.HasSuffix(slug, "-") {
		return fmt.Errorf("invalid tag slug: %s", slug)
	}

	return nil
}

func taskHasParent(ctx context.Context, tx workItemRelationQuerier, taskID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM work_item_relations
WHERE target_item_id = ? AND kind = ?`, taskID, string(RelationParentOf)).Scan(&count); err != nil {
		return false, fmt.Errorf("check task parent: %w", err)
	}

	return count > 0, nil
}

func relationPathExists(ctx context.Context, tx *sql.Tx, kind RelationKind, startTaskID, targetTaskID string) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `
WITH RECURSIVE reachable(task_id) AS (
	SELECT target_item_id
	FROM work_item_relations
	WHERE source_item_id = ? AND kind = ?

	UNION

	SELECT r.target_item_id
	FROM work_item_relations r
	JOIN reachable ON reachable.task_id = r.source_item_id
	WHERE r.kind = ?
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE task_id = ?)`,
		startTaskID,
		string(kind),
		string(kind),
		targetTaskID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check relation cycle: %w", err)
	}

	return exists == 1, nil
}

type taskScanner interface {
	Scan(dest ...any) error
}

// scanRows scans every row through scan, appending the results and closing rows
// when done. It collapses the repeated for-rows.Next/append/rows.Err boilerplate
// shared by the coordinator readers.
func scanRows[T any](rows *sql.Rows, scan func(taskScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func scanTask(scanner taskScanner) (Task, error) {
	var task Task
	var flowID sql.NullString
	var featureID sql.NullString
	var createdBy string
	var createdBySessionID sql.NullString
	var sourceTaskID sql.NullString
	var sourceChangeID sql.NullString
	var createdAt string
	var updatedAt string
	var lifecycleState sql.NullString
	var doneResolution sql.NullString
	var doneMessage sql.NullString
	var doneEvidenceJSON sql.NullString
	var doneAt sql.NullString

	if err := scanner.Scan(
		&task.ID,
		&task.Title,
		&task.Body,
		&task.Priority,
		&task.RequiresHumanReview,
		&flowID,
		&featureID,
		&createdBy,
		&createdBySessionID,
		&sourceTaskID,
		&sourceChangeID,
		&createdAt,
		&updatedAt,
		&lifecycleState,
		&doneResolution,
		&doneMessage,
		&doneEvidenceJSON,
		&doneAt,
	); err != nil {
		return Task{}, fmt.Errorf("scan task: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Task{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Task{}, err
	}

	// These fields exist only so dormant pre-v2 coordinator helpers still
	// compile. They are derived from the authoritative lifecycle and are never
	// persisted or exposed by the version-2 model.
	task.ScheduleState = ScheduleBacklog
	task.TriageState = TriageAccepted
	if flowID.Valid {
		task.FlowID = flowID.String
	}
	task.FeatureID = nullableStringPointer(featureID)
	task.CreatedBy = Actor(createdBy)
	task.CreatedBySessionID = nullableStringPointer(createdBySessionID)
	task.SourceTaskID = nullableStringPointer(sourceTaskID)
	task.SourceChangeID = nullableStringPointer(sourceChangeID)
	task.CreatedAt = parsedCreatedAt
	task.UpdatedAt = parsedUpdatedAt
	if lifecycleState.Valid {
		state := LifecycleState(lifecycleState.String)
		task.State = &state
		switch state {
		case LifecycleScheduled, LifecycleInProgress:
			task.ScheduleState = ScheduleUpNext
		case LifecycleDone:
			task.ScheduleState = ScheduleClosed
		}
	}
	if doneResolution.Valid {
		resolution := DoneResolution(doneResolution.String)
		task.DoneResolution = &resolution
	}
	task.DoneMessage = doneMessage.String
	if doneEvidenceJSON.Valid && doneEvidenceJSON.String != "" {
		var evidence []Evidence
		if err := json.Unmarshal([]byte(doneEvidenceJSON.String), &evidence); err == nil {
			task.DoneEvidence = evidence
		}
	}
	if doneAt.Valid {
		parsedDoneAt, err := parseTime(doneAt.String)
		if err != nil {
			return Task{}, err
		}
		task.DoneAt = &parsedDoneAt
		task.ClosedAt = &parsedDoneAt
	}

	return task, nil
}

// featureOpenForAssignmentTx confirms the referenced feature exists and is
// open. Tasks can only join an open feature: a landed or archived feature's
// branch is frozen.
func featureOpenForAssignmentTx(ctx context.Context, tx workItemRelationQuerier, featureID string) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM features WHERE id = ?`, strings.TrimSpace(featureID)).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFeatureNotFound
	}
	if err != nil {
		return fmt.Errorf("load feature: %w", err)
	}
	if FeatureStatus(status) != FeatureOpen {
		return ErrFeatureClosed
	}

	return nil
}

// guardFeatureChange applies the feature-assignment edit rules: a no-op
// assignment always succeeds, but changing a task's feature is rejected once
// the task is in progress or has a change row — moving the base mid-flight
// would strand the change. Returns the feature id to persist.
func (s *TaskService) guardFeatureChange(ctx context.Context, current Task, requested *string) (*string, error) {
	if requested == nil {
		if current.FeatureID == nil {
			return current.FeatureID, nil
		}
	} else {
		trimmed := strings.TrimSpace(*requested)
		if current.FeatureID != nil && *current.FeatureID == trimmed {
			return current.FeatureID, nil
		}
		if trimmed == "" {
			return nil, ErrFeatureNotFound
		}
	}

	if current.State != nil && *current.State == LifecycleInProgress {
		return nil, errors.New("cannot change feature while the task is in progress")
	}
	var changeCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM changes WHERE task_id = ?`, current.ID).Scan(&changeCount); err != nil {
		return nil, fmt.Errorf("count task changes: %w", err)
	}
	if changeCount > 0 {
		return nil, errors.New("cannot change feature once the task has a change")
	}

	if requested == nil {
		return nil, nil
	}
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM features WHERE id = ?`, strings.TrimSpace(*requested)).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFeatureNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load feature: %w", err)
	}
	if FeatureStatus(status) != FeatureOpen {
		return nil, ErrFeatureClosed
	}
	trimmed := strings.TrimSpace(*requested)

	return &trimmed, nil
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}

	return tasks, nil
}

func scanTag(scanner taskScanner) (Tag, error) {
	var tag Tag
	var createdBy string
	var createdAt string

	if err := scanner.Scan(
		&tag.ID,
		&tag.Slug,
		&tag.Name,
		&tag.Color,
		&tag.Description,
		&createdBy,
		&createdAt,
	); err != nil {
		return Tag{}, fmt.Errorf("scan tag: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Tag{}, err
	}

	tag.CreatedBy = Actor(createdBy)
	tag.CreatedAt = parsedCreatedAt
	return tag, nil
}

func scanTaskRelation(scanner taskScanner) (TaskRelation, error) {
	var relation TaskRelation
	var kind string
	var sourceState sql.NullString
	var targetState sql.NullString
	var sourceUpdatedAt string
	var createdBy string
	var createdAt string

	if err := scanner.Scan(
		&relation.SourceTaskID,
		&relation.TargetTaskID,
		&kind,
		&relation.SourceTitle,
		&relation.TargetTitle,
		&sourceState,
		&targetState,
		&relation.SourcePriority,
		&sourceUpdatedAt,
		&createdBy,
		&createdAt,
	); err != nil {
		return TaskRelation{}, fmt.Errorf("scan task relation: %w", err)
	}
	relation.SourceState = LifecycleState(sourceState.String)
	relation.TargetState = LifecycleState(targetState.String)
	parsedSourceUpdatedAt, err := parseTime(sourceUpdatedAt)
	if err != nil {
		return TaskRelation{}, err
	}
	relation.SourceUpdatedAt = parsedSourceUpdatedAt

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return TaskRelation{}, err
	}

	relation.Kind = RelationKind(kind)
	relation.CreatedBy = Actor(createdBy)
	relation.CreatedAt = parsedCreatedAt
	return relation, nil
}

func inPredicate(column string, count int) string {
	placeholders := make([]string, count)
	for i := range placeholders {
		placeholders[i] = "?"
	}

	return column + " IN (" + strings.Join(placeholders, ", ") + ")"
}

var (
	nullableStringValue   = sqlitex.NullableString
	nullableStringPointer = sqlitex.NullableStringPointer
)

func nullableInt64Value(value *int64) any {
	if value == nil {
		return nil
	}

	return *value
}

func boolInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

var (
	formatTime = sqlitex.FormatTime
	parseTime  = sqlitex.ParseTime
)
