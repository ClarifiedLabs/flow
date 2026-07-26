package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	RequiresHumanReview bool            `json:"-"`
	AutoMerge           bool            `json:"-"`
	FlowID              string          `json:"flow_id,omitempty"`
	State               *LifecycleState `json:"state"`
	DoneResolution      *DoneResolution `json:"done_resolution,omitempty"`
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
	CreatedBy    Actor
	CreatedAt    time.Time
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
	CreatedBy           Actor
	CreatedBySessionID  *string
	SourceTaskID        *string
	SourceChangeID      *string
}

type CreateTaskWithDetailsInput struct {
	Task      CreateTaskInput
	Tags      []CreateTagInput
	Relations []CreateTaskRelationInput
}

type CreateTaskRelationInput struct {
	SourceTaskID string
	TargetTaskID string
	Kind         RelationKind
	CreatedBy    Actor
}

type EditTaskInput struct {
	Title               *string
	Body                *string
	Priority            *int
	RequiresHumanReview *bool
	AutoMerge           *bool
	FlowID              *string
}

type TaskFilter struct {
	LifecycleStates []string
	TagSlugs        []string
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
	LaneStateBlocked          LaneState = "blocked"
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
	for i := range input.Relations {
		if input.Relations[i].CreatedBy == "" {
			input.Relations[i].CreatedBy = taskInput.CreatedBy
		}
		if input.Relations[i].Kind == "" {
			return Task{}, errors.New("task relation kind is required")
		}
		if err := validateRelationKind(input.Relations[i].Kind); err != nil {
			return Task{}, err
		}
		if err := validateActor(input.Relations[i].CreatedBy); err != nil {
			return Task{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("begin create task transaction: %w", err)
	}
	defer tx.Rollback()

	id, err := s.allocateTaskID(ctx, tx)
	if err != nil {
		return Task{}, err
	}

	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id,
	title,
	body,
	priority,
	flow_id,
	created_by,
	created_by_session_id,
	source_task_id,
	source_change_id,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		taskInput.Title,
		taskInput.Body,
		taskInput.Priority,
		sqlitex.NullableNonEmptyString(taskInput.FlowID),
		string(taskInput.CreatedBy),
		nullableStringValue(taskInput.CreatedBySessionID),
		nullableStringValue(taskInput.SourceTaskID),
		nullableStringValue(taskInput.SourceChangeID),
		nowText,
		nowText,
	); err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
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
			targetTaskID = id
		}
		if err := linkTasksInTx(ctx, tx, sourceTaskID, targetTaskID, relationInput.Kind, defaultActor(relationInput.CreatedBy, taskInput.CreatedBy), nowText); err != nil {
			return Task{}, err
		}
	}

	if err := tx.Commit(); err != nil {
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
	flow_id,
	created_by,
	created_by_session_id,
	source_task_id,
	source_change_id,
	created_at,
	updated_at,
	lifecycle_state,
	done_resolution,
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
	i.flow_id,
	i.created_by,
	i.created_by_session_id,
	i.source_task_id,
	i.source_change_id,
	i.created_at,
	i.updated_at,
	i.lifecycle_state,
	i.done_resolution,
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
	query += "\nGROUP BY i.id\nORDER BY CAST(substr(i.id, 3) AS INTEGER)"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
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

func (s *TaskService) EditTask(ctx context.Context, id string, input EditTaskInput) (Task, error) {
	current, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}

	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			return Task{}, errors.New("task title is required")
		}
		current.Title = title
	}
	if input.Body != nil {
		current.Body = *input.Body
	}
	if input.Priority != nil {
		if *input.Priority < 0 {
			return Task{}, errors.New("task priority must be non-negative")
		}
		current.Priority = *input.Priority
	}
	if input.FlowID != nil {
		current.FlowID = strings.TrimSpace(*input.FlowID)
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE tasks
SET
	title = ?,
	body = ?,
	priority = ?,
	flow_id = ?,
	updated_at = ?
WHERE id = ?`,
		current.Title,
		current.Body,
		current.Priority,
		sqlitex.NullableNonEmptyString(current.FlowID),
		formatTime(s.now().UTC()),
		id,
	); err != nil {
		return Task{}, fmt.Errorf("edit task: %w", err)
	}

	return s.GetTask(ctx, id)
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

func upsertTagInTx(ctx context.Context, tx *sql.Tx, input CreateTagInput, nowText string) (int64, error) {
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

func (s *TaskService) LinkTasks(ctx context.Context, sourceTaskID, targetTaskID string, kind RelationKind, actor Actor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin link task transaction: %w", err)
	}
	defer tx.Rollback()

	if err := linkTasksInTx(ctx, tx, sourceTaskID, targetTaskID, kind, actor, formatTime(s.now().UTC())); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit link tasks: %w", err)
	}

	return nil
}

func linkTasksInTx(ctx context.Context, tx *sql.Tx, sourceTaskID, targetTaskID string, kind RelationKind, actor Actor, nowText string) error {
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
		cycle, err := relationPathExists(ctx, tx, kind, targetTaskID, sourceTaskID)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("%s relation would create a cycle", kind)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO task_relations (source_task_id, target_task_id, kind, created_by, created_at)
VALUES (?, ?, ?, ?, ?)`,
		sourceTaskID,
		targetTaskID,
		string(kind),
		string(actor),
		nowText,
	); err != nil {
		return fmt.Errorf("link tasks: %w", err)
	}

	return nil
}

func (s *TaskService) UnlinkTasks(ctx context.Context, sourceTaskID, targetTaskID string, kind RelationKind) error {
	if err := validateRelationKind(kind); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
DELETE FROM task_relations
WHERE source_task_id = ? AND target_task_id = ? AND kind = ?`,
		sourceTaskID,
		targetTaskID,
		string(kind),
	); err != nil {
		return fmt.Errorf("unlink tasks: %w", err)
	}

	return nil
}

func (s *TaskService) RelationsForTask(ctx context.Context, taskID string) ([]TaskRelation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT source_task_id, target_task_id, kind, created_by, created_at
FROM task_relations
WHERE source_task_id = ? OR target_task_id = ?
ORDER BY created_at, source_task_id, target_task_id, kind`, taskID, taskID)
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
			var openWaits, held int
			if err := s.db.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM workflow_waits w
		JOIN workflow_runs r ON r.id = w.workflow_run_id
		WHERE r.task_id = ? AND w.state = 'open'),
	(SELECT COUNT(*) FROM workflow_runs r
		WHERE r.task_id = ? AND r.held_at IS NOT NULL
			AND r.state IN ('scheduled', 'running', 'waiting'))`,
				task.ID, task.ID).Scan(&openWaits, &held); err != nil {
				return BoardResult{}, err
			}
			switch {
			case held > 0:
				result.LaneStates[task.ID] = LaneStateHeld
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

	return result, nil
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
FROM task_relations r
JOIN tasks blocker ON blocker.id = r.source_task_id
	WHERE r.kind = ?
		AND r.target_task_id = ?
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
		blocker.flow_id,
	blocker.created_by,
	blocker.created_by_session_id,
	blocker.source_task_id,
	blocker.source_change_id,
	blocker.created_at,
		blocker.updated_at,
		blocker.lifecycle_state,
	blocker.done_resolution,
	blocker.done_at
FROM task_relations r
JOIN tasks blocker ON blocker.id = r.source_task_id
	WHERE r.kind = ?
		AND r.target_task_id = ?
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

func (s *TaskService) allocateTaskID(ctx context.Context, tx *sql.Tx) (string, error) {
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
		defaultRequiresHumanReview := true
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

func taskHasParent(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM task_relations
WHERE target_task_id = ? AND kind = ?`, taskID, string(RelationParentOf)).Scan(&count); err != nil {
		return false, fmt.Errorf("check task parent: %w", err)
	}

	return count > 0, nil
}

func relationPathExists(ctx context.Context, tx *sql.Tx, kind RelationKind, startTaskID, targetTaskID string) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `
WITH RECURSIVE reachable(task_id) AS (
	SELECT target_task_id
	FROM task_relations
	WHERE source_task_id = ? AND kind = ?

	UNION

	SELECT r.target_task_id
	FROM task_relations r
	JOIN reachable ON reachable.task_id = r.source_task_id
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
	var createdBy string
	var createdBySessionID sql.NullString
	var sourceTaskID sql.NullString
	var sourceChangeID sql.NullString
	var createdAt string
	var updatedAt string
	var lifecycleState sql.NullString
	var doneResolution sql.NullString
	var doneAt sql.NullString

	if err := scanner.Scan(
		&task.ID,
		&task.Title,
		&task.Body,
		&task.Priority,
		&flowID,
		&createdBy,
		&createdBySessionID,
		&sourceTaskID,
		&sourceChangeID,
		&createdAt,
		&updatedAt,
		&lifecycleState,
		&doneResolution,
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
	task.RequiresHumanReview = true
	if flowID.Valid {
		task.FlowID = flowID.String
	}
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
	var createdBy string
	var createdAt string

	if err := scanner.Scan(
		&relation.SourceTaskID,
		&relation.TargetTaskID,
		&kind,
		&createdBy,
		&createdAt,
	); err != nil {
		return TaskRelation{}, fmt.Errorf("scan task relation: %w", err)
	}

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
