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

type IssueState string

const (
	IssueStateTriage   IssueState = "triage"
	IssueStateBacklog  IssueState = "backlog"
	IssueStateUpNext   IssueState = "up_next"
	IssueStateClosed   IssueState = "closed"
	IssueStateRejected IssueState = "rejected"
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

type Issue struct {
	ID                  string
	Title               string
	Body                string
	AcceptanceCriteria  string
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
	SourceIssueID       *string
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

type IssueRelation struct {
	SourceIssueID string
	TargetIssueID string
	Kind          RelationKind
	CreatedBy     Actor
	CreatedAt     time.Time
}

type CreateIssueInput struct {
	Title               string
	Body                string
	AcceptanceCriteria  string
	Priority            int
	ScheduleState       ScheduleState
	TriageState         TriageState
	RequiresHumanReview *bool
	AutoMerge           *bool
	FlowID              string
	CreatedBy           Actor
	CreatedBySessionID  *string
	SourceIssueID       *string
	SourceChangeID      *string
}

type CreateIssueWithDetailsInput struct {
	Issue     CreateIssueInput
	Tags      []CreateTagInput
	Relations []CreateIssueRelationInput
}

type CreateIssueRelationInput struct {
	SourceIssueID string
	TargetIssueID string
	Kind          RelationKind
	CreatedBy     Actor
}

type EditIssueInput struct {
	Title               *string
	Body                *string
	AcceptanceCriteria  *string
	Priority            *int
	RequiresHumanReview *bool
	AutoMerge           *bool
	FlowID              *string
}

type IssueFilter struct {
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
	Unscheduled    []Issue
	Scheduled      []Issue
	InProgress     []Issue
	Backlog        []Issue `json:"-"`
	UpNext         []Issue `json:"-"`
	NeedsAttention []Issue `json:"-"`
}

// LaneState is the fine-grained derived sub-state of an open issue. It is the
// outcome of the board precedence cascade before coarsening into one of the
// four board lanes, and is surfaced as a pill on issue cards.
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

// BoardResult bundles the four board lanes with the per-issue overlays the UI
// and CLI need: the fine-grained sub-state and the derived blocked overlay.
// Blocked issues are routed to needs_attention while retaining their natural
// lane state for card badges and CLI annotations.
type BoardResult struct {
	Board       Board
	LaneStates  map[string]LaneState
	WaitReasons map[string]WaitReason
	BlockedIDs  []string
}

type IssueService struct {
	db  *sql.DB
	now func() time.Time
}

func NewIssueService(database *sql.DB) *IssueService {
	return &IssueService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *IssueService) CreateIssue(ctx context.Context, input CreateIssueInput) (Issue, error) {
	return s.CreateIssueWithDetails(ctx, CreateIssueWithDetailsInput{Issue: input})
}

func (s *IssueService) CreateIssueWithDetails(ctx context.Context, input CreateIssueWithDetailsInput) (Issue, error) {
	issueInput, err := normalizeCreateIssueInput(input.Issue)
	if err != nil {
		return Issue{}, err
	}
	for i := range input.Tags {
		if input.Tags[i].CreatedBy == "" {
			input.Tags[i].CreatedBy = issueInput.CreatedBy
		}
		if _, err := normalizeCreateTagInput(input.Tags[i]); err != nil {
			return Issue{}, err
		}
	}
	for i := range input.Relations {
		if input.Relations[i].CreatedBy == "" {
			input.Relations[i].CreatedBy = issueInput.CreatedBy
		}
		if input.Relations[i].Kind == "" {
			return Issue{}, errors.New("issue relation kind is required")
		}
		if err := validateRelationKind(input.Relations[i].Kind); err != nil {
			return Issue{}, err
		}
		if err := validateActor(input.Relations[i].CreatedBy); err != nil {
			return Issue{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, fmt.Errorf("begin create issue transaction: %w", err)
	}
	defer tx.Rollback()

	id, err := allocateIssueID(ctx, tx)
	if err != nil {
		return Issue{}, err
	}

	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO issues (
	id,
	title,
	body,
	acceptance_criteria,
	priority,
	flow_id,
	created_by,
	created_by_session_id,
	source_issue_id,
	source_change_id,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		issueInput.Title,
		issueInput.Body,
		issueInput.AcceptanceCriteria,
		issueInput.Priority,
		sqlitex.NullableNonEmptyString(issueInput.FlowID),
		string(issueInput.CreatedBy),
		nullableStringValue(issueInput.CreatedBySessionID),
		nullableStringValue(issueInput.SourceIssueID),
		nullableStringValue(issueInput.SourceChangeID),
		nowText,
		nowText,
	); err != nil {
		return Issue{}, fmt.Errorf("insert issue: %w", err)
	}

	for _, tagInput := range input.Tags {
		tagInput.CreatedBy = defaultActor(tagInput.CreatedBy, issueInput.CreatedBy)
		tagID, err := upsertTagInTx(ctx, tx, tagInput, nowText)
		if err != nil {
			return Issue{}, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO issue_tags (issue_id, tag_id, created_by, created_at)
VALUES (?, ?, ?, ?)`,
			id,
			tagID,
			string(issueInput.CreatedBy),
			nowText,
		); err != nil {
			return Issue{}, fmt.Errorf("tag issue: %w", err)
		}
	}

	for _, relationInput := range input.Relations {
		sourceIssueID := strings.TrimSpace(relationInput.SourceIssueID)
		if sourceIssueID == "" {
			sourceIssueID = id
		}
		targetIssueID := strings.TrimSpace(relationInput.TargetIssueID)
		if targetIssueID == "" {
			targetIssueID = id
		}
		if err := linkIssuesInTx(ctx, tx, sourceIssueID, targetIssueID, relationInput.Kind, defaultActor(relationInput.CreatedBy, issueInput.CreatedBy), nowText); err != nil {
			return Issue{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Issue{}, fmt.Errorf("commit create issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) GetIssue(ctx context.Context, id string) (Issue, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
	id,
	title,
	body,
	acceptance_criteria,
	priority,
	flow_id,
	created_by,
	created_by_session_id,
	source_issue_id,
	source_change_id,
	created_at,
	updated_at,
	lifecycle_state,
	done_resolution,
	done_at
FROM issues
WHERE id = ?`, id)

	issue, err := scanIssue(row)
	if err != nil {
		return Issue{}, err
	}

	return issue, nil
}

// issueSelectColumns is the canonical issue column list, shared by every reader
// that scans rows with scanIssues. Keep the order in sync with scanIssues.
const issueSelectColumns = `
	i.id,
	i.title,
	i.body,
	i.acceptance_criteria,
	i.priority,
	i.flow_id,
	i.created_by,
	i.created_by_session_id,
	i.source_issue_id,
	i.source_change_id,
	i.created_at,
	i.updated_at,
	i.lifecycle_state,
	i.done_resolution,
	i.done_at`

func (s *IssueService) ListIssues(ctx context.Context, filter IssueFilter) ([]Issue, error) {
	query := "SELECT" + issueSelectColumns + "\nFROM issues i"

	var args []any
	var predicates []string
	if len(filter.TagSlugs) > 0 {
		query += `
JOIN issue_tags it ON it.issue_id = i.id
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
			return []Issue{}, nil
		}
		predicates = append(predicates, "("+strings.Join(statePredicates, " OR ")+")")
	}
	if len(predicates) > 0 {
		query += "\nWHERE " + strings.Join(predicates, " AND ")
	}
	query += "\nGROUP BY i.id\nORDER BY CAST(substr(i.id, 3) AS INTEGER)"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()

	return scanIssues(rows)
}

// ClosedOutcome filters closed issues by their terminal disposition. The empty
// value means "any outcome". The predicates mirror derivePhaseFromIssue: a
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

// ClosedIssueQuery bounds a page of closed issues. It is deliberately separate
// from IssueFilter/ListIssues (the board + triage hot path) so the unbounded
// history reader can never widen those queries.
type ClosedIssueQuery struct {
	// Limit caps the page size; <= 0 falls back to defaultClosedIssueLimit.
	Limit int
	// Before/BeforeID is the keyset cursor: only rows strictly older than this
	// (done_at, id) pair are returned. Both come from a prior ClosedCursor.
	Before   *time.Time
	BeforeID string
	// Within, when set, restricts results to issues closed at or after it.
	Within *time.Time
	// Outcome filters by terminal disposition; empty means any.
	Outcome ClosedOutcome
}

// ClosedCursor is the keyset position of the last returned row, used to fetch
// the next (older) page via ClosedIssueQuery.Before/BeforeID.
type ClosedCursor struct {
	ClosedAt time.Time
	ID       string
}

const defaultClosedIssueLimit = 50

// ListClosedIssues returns one keyset-paginated page of closed issues ordered
// newest-closed first (done_at desc, id desc tiebreak). It never loads the
// full set: closed issues grow unbounded, so callers must page or window. The
// returned cursor is non-nil only when more rows remain.
func (s *IssueService) ListClosedIssues(ctx context.Context, q ClosedIssueQuery) ([]Issue, *ClosedCursor, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultClosedIssueLimit
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

	query := "SELECT" + issueSelectColumns + "\nFROM issues i\nWHERE " + strings.Join(predicates, " AND ") +
		"\nORDER BY i.done_at DESC, i.id DESC\nLIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list closed issues: %w", err)
	}
	defer rows.Close()

	issues, err := scanIssues(rows)
	if err != nil {
		return nil, nil, err
	}

	var next *ClosedCursor
	if len(issues) > limit {
		issues = issues[:limit]
		last := issues[limit-1]
		if last.DoneAt != nil {
			next = &ClosedCursor{ClosedAt: *last.DoneAt, ID: last.ID}
		}
	}

	return issues, next, nil
}

// CountClosedIssues returns the total number of closed issues, for the nav
// badge. It is a cheap indexed COUNT and intentionally ignores disposition.
func (s *IssueService) CountClosedIssues(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM issues
WHERE lifecycle_state = ?`, string(LifecycleDone)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count closed issues: %w", err)
	}

	return count, nil
}

func (s *IssueService) EditIssue(ctx context.Context, id string, input EditIssueInput) (Issue, error) {
	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return Issue{}, err
	}

	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			return Issue{}, errors.New("issue title is required")
		}
		current.Title = title
	}
	if input.Body != nil {
		current.Body = *input.Body
	}
	if input.AcceptanceCriteria != nil {
		current.AcceptanceCriteria = *input.AcceptanceCriteria
	}
	if input.Priority != nil {
		if *input.Priority < 0 {
			return Issue{}, errors.New("issue priority must be non-negative")
		}
		current.Priority = *input.Priority
	}
	if input.FlowID != nil {
		current.FlowID = strings.TrimSpace(*input.FlowID)
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET
	title = ?,
	body = ?,
	acceptance_criteria = ?,
	priority = ?,
	flow_id = ?,
	updated_at = ?
WHERE id = ?`,
		current.Title,
		current.Body,
		current.AcceptanceCriteria,
		current.Priority,
		sqlitex.NullableNonEmptyString(current.FlowID),
		formatTime(s.now().UTC()),
		id,
	); err != nil {
		return Issue{}, fmt.Errorf("edit issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) ScheduleIssue(ctx context.Context, id string, state ScheduleState) (Issue, error) {
	return Issue{}, errors.New("legacy schedule states were removed; schedule the issue through its workflow")
}

func (s *IssueService) CloseIssue(ctx context.Context, id string) (Issue, error) {
	return Issue{}, errors.New("legacy close was removed; complete the issue through its workflow")
}

func (s *IssueService) SetIssueState(ctx context.Context, id string, state IssueState) (Issue, error) {
	return Issue{}, errors.New("legacy issue states were removed; use workflow lifecycle operations")
}

func (s *IssueService) AcceptTriage(ctx context.Context, id string) (Issue, error) {
	return Issue{}, errors.New("triage was removed from the issue lifecycle")
}

func (s *IssueService) RejectTriage(ctx context.Context, id string) (Issue, error) {
	return Issue{}, errors.New("triage was removed from the issue lifecycle")
}

func (s *IssueService) CreateTag(ctx context.Context, input CreateTagInput) (Tag, error) {
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

func (s *IssueService) GetTag(ctx context.Context, id int64) (Tag, error) {
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

func (s *IssueService) TagsForIssue(ctx context.Context, issueID string) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t.slug, t.name, t.color, t.description, t.created_by, t.created_at
FROM tags t
JOIN issue_tags it ON it.tag_id = t.id
WHERE it.issue_id = ?
ORDER BY t.slug`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue tags: %w", err)
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
		return nil, fmt.Errorf("iterate issue tags: %w", err)
	}

	return tags, nil
}

func (s *IssueService) TagIssue(ctx context.Context, issueID string, tagID int64, actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO issue_tags (issue_id, tag_id, created_by, created_at)
VALUES (?, ?, ?, ?)`,
		issueID,
		tagID,
		string(actor),
		formatTime(s.now().UTC()),
	); err != nil {
		return fmt.Errorf("tag issue: %w", err)
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

func (s *IssueService) UntagIssue(ctx context.Context, issueID string, tagID int64) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM issue_tags
WHERE issue_id = ? AND tag_id = ?`, issueID, tagID); err != nil {
		return fmt.Errorf("untag issue: %w", err)
	}

	return nil
}

func (s *IssueService) LinkIssues(ctx context.Context, sourceIssueID, targetIssueID string, kind RelationKind, actor Actor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin link issue transaction: %w", err)
	}
	defer tx.Rollback()

	if err := linkIssuesInTx(ctx, tx, sourceIssueID, targetIssueID, kind, actor, formatTime(s.now().UTC())); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit link issues: %w", err)
	}

	return nil
}

func linkIssuesInTx(ctx context.Context, tx *sql.Tx, sourceIssueID, targetIssueID string, kind RelationKind, actor Actor, nowText string) error {
	sourceIssueID = strings.TrimSpace(sourceIssueID)
	targetIssueID = strings.TrimSpace(targetIssueID)
	if sourceIssueID == "" || targetIssueID == "" {
		return errors.New("issue relation source_issue_id and target_issue_id are required")
	}
	if sourceIssueID == targetIssueID {
		return errors.New("issue relation cannot target itself")
	}
	if err := validateRelationKind(kind); err != nil {
		return err
	}
	if err := validateActor(actor); err != nil {
		return err
	}

	if kind == RelationParentOf {
		hasParent, err := issueHasParent(ctx, tx, targetIssueID)
		if err != nil {
			return err
		}
		if hasParent {
			return errors.New("issue already has a parent")
		}
	}
	if kind == RelationParentOf || kind == RelationBlocks {
		cycle, err := relationPathExists(ctx, tx, kind, targetIssueID, sourceIssueID)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("%s relation would create a cycle", kind)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO issue_relations (source_issue_id, target_issue_id, kind, created_by, created_at)
VALUES (?, ?, ?, ?, ?)`,
		sourceIssueID,
		targetIssueID,
		string(kind),
		string(actor),
		nowText,
	); err != nil {
		return fmt.Errorf("link issues: %w", err)
	}

	return nil
}

func (s *IssueService) UnlinkIssues(ctx context.Context, sourceIssueID, targetIssueID string, kind RelationKind) error {
	if err := validateRelationKind(kind); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
DELETE FROM issue_relations
WHERE source_issue_id = ? AND target_issue_id = ? AND kind = ?`,
		sourceIssueID,
		targetIssueID,
		string(kind),
	); err != nil {
		return fmt.Errorf("unlink issues: %w", err)
	}

	return nil
}

func (s *IssueService) RelationsForIssue(ctx context.Context, issueID string) ([]IssueRelation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT source_issue_id, target_issue_id, kind, created_by, created_at
FROM issue_relations
WHERE source_issue_id = ? OR target_issue_id = ?
ORDER BY created_at, source_issue_id, target_issue_id, kind`, issueID, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue relations: %w", err)
	}
	defer rows.Close()

	var relations []IssueRelation
	for rows.Next() {
		relation, err := scanIssueRelation(rows)
		if err != nil {
			return nil, err
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue relations: %w", err)
	}

	return relations, nil
}

func (s *IssueService) Board(ctx context.Context) (Board, error) {
	result, err := s.BoardResult(ctx)
	if err != nil {
		return Board{}, err
	}
	return result.Board, nil
}

func (s *IssueService) BoardResult(ctx context.Context) (BoardResult, error) {
	issues, err := s.ListIssues(ctx, IssueFilter{})
	if err != nil {
		return BoardResult{}, err
	}

	result := BoardResult{LaneStates: map[string]LaneState{}, WaitReasons: map[string]WaitReason{}}
	for _, issue := range issues {
		if issue.State == nil {
			result.Board.Unscheduled = append(result.Board.Unscheduled, issue)
			result.LaneStates[issue.ID] = LaneStateUnscheduled
			continue
		}
		switch *issue.State {
		case LifecycleScheduled:
			result.Board.Scheduled = append(result.Board.Scheduled, issue)
			result.LaneStates[issue.ID] = LaneStateScheduled
		case LifecycleInProgress:
			result.Board.InProgress = append(result.Board.InProgress, issue)
			var openWaits int
			if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_waits w
JOIN workflow_runs r ON r.id = w.workflow_run_id
WHERE r.issue_id = ? AND w.state = 'open'`, issue.ID).Scan(&openWaits); err != nil {
				return BoardResult{}, err
			}
			if openWaits > 0 {
				result.LaneStates[issue.ID] = LaneStateBlocked
				result.BlockedIDs = append(result.BlockedIDs, issue.ID)
				result.WaitReasons[issue.ID] = WaitReasonBlocked
			} else {
				result.LaneStates[issue.ID] = LaneStateWorking
			}
		case LifecycleDone:
			// Done is served by the paginated Done reader.
		}
	}

	return result, nil
}

// laneStateForPhase projects the lifecycle phase into the board's visible
// fine-grained state. Closed phases are omitted from the board. Critique keeps
// the existing user-facing distinction between a change under review and one
// that explicitly has requested changes.
func (s *IssueService) laneStateForPhase(ctx context.Context, issueID string, phase Phase) (LaneState, bool, error) {
	state, ok := laneStateForPhase(phase)
	if !ok || phase != PhaseCritique {
		return state, ok, nil
	}
	reviewState, err := s.reviewState(ctx, issueID)
	if err != nil {
		return "", false, err
	}
	if reviewState == ReviewChangesRequested {
		return LaneStateChangesRequested, true, nil
	}
	return state, true, nil
}

func laneStateForPhase(phase Phase) (LaneState, bool) {
	switch phase {
	case PhaseBacklog:
		return LaneStateBacklog, true
	case PhaseTriage:
		return LaneStateTriage, true
	case PhaseUpNext:
		return LaneStateUpNext, true
	case PhaseWorking:
		return LaneStateInProgress, true
	case PhaseCritique, PhaseAcceptance:
		return LaneStateInReview, true
	case PhaseApproved:
		return LaneStateReadyToMerge, true
	case PhaseMergedClosed, PhaseRejectedClosed, PhaseAbandoned:
		return "", false
	default:
		return LaneStateBacklog, true
	}
}

func (s *IssueService) waitReason(ctx context.Context, issue Issue, state LaneState) (WaitReason, error) {
	// A flow paused at a human gate has no active session; the cursor is the
	// signal that a phase handoff awaits approval.
	if _, cursorState, ok, err := cursorStateForIssue(ctx, s.db, issue.ID); err != nil {
		return "", err
	} else if ok && cursorState == FlowPhaseAwaitingApproval {
		return WaitReasonPhaseApproval, nil
	}
	if sessionState, ok, err := activeSessionStateForIssue(ctx, s.db, issue.ID); err != nil {
		return "", err
	} else if ok && sessionState == SessionWaiting {
		return WaitReasonQuestion, nil
	}
	if state == LaneStateReadyToMerge && !issue.AutoMerge {
		return WaitReasonManualMerge, nil
	}
	if state == LaneStateInReview {
		pending, err := pendingHumanReview(ctx, s.db, issue.ID)
		if err != nil {
			return "", err
		}
		if pending {
			return WaitReasonHumanReview, nil
		}
	}
	crashLoop, err := crashLoopStatusExists(ctx, s.db, issue.ID)
	if err != nil {
		return "", err
	}
	if crashLoop {
		return WaitReasonCrashLoop, nil
	}
	exhausted, err := reviewCycleBudgetExhausted(ctx, s.db, issue.ID)
	if err != nil {
		return "", err
	}
	if exhausted {
		return WaitReasonReviewCycles, nil
	}
	return "", nil
}

func pendingHumanReview(ctx context.Context, db *sql.DB, issueID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM checks
WHERE issue_id = ?
	AND kind = ?
	AND required = 1
	AND verdict = ?`,
		issueID,
		string(CheckKindHuman),
		string(CheckPending),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check pending human review: %w", err)
	}

	return count > 0, nil
}

func crashLoopStatusExists(ctx context.Context, db *sql.DB, issueID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM status_log
WHERE issue_id = ?
	AND kind = ?
	AND message LIKE ?
	AND resolved_at IS NULL`,
		issueID,
		StatusKindBlocker,
		crashRestartLimitMessageLike,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check crash loop status: %w", err)
	}

	return count > 0, nil
}

func (s *IssueService) CrashRetryAvailable(ctx context.Context, issueID string) (bool, error) {
	return crashLoopStatusExists(ctx, s.db, issueID)
}

// laneForState coarsens a fine-grained sub-state into one of the four board
// lanes, grouped by who acts next: backlog (undecided or unscheduled),
// up_next (waiting for an agent), in_progress (automation working),
// needs_attention (waiting on a human). Unresolved blockers are applied by
// BoardResult as a needs_attention override after this coarsening.
func laneForState(state LaneState) string {
	switch state {
	case LaneStateTriage, LaneStateBacklog:
		return "backlog"
	case LaneStateUpNext:
		return "up_next"
	case LaneStateInProgress, LaneStateInReview, LaneStateChangesRequested:
		return "in_progress"
	case LaneStateReadyToMerge:
		return "needs_attention"
	}
	return "backlog"
}

func (s *IssueService) reviewState(ctx context.Context, issueID string) (ReviewState, error) {
	return reviewStateForIssue(ctx, s.db, issueID)
}

func (s *IssueService) issueIsBlocked(ctx context.Context, issueID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM issue_relations r
JOIN issues blocker ON blocker.id = r.source_issue_id
	WHERE r.kind = ?
		AND r.target_issue_id = ?
		AND (blocker.lifecycle_state IS NULL OR blocker.lifecycle_state != ?)`,
		string(RelationBlocks),
		issueID,
		string(LifecycleDone),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check issue blockers: %w", err)
	}

	return count > 0, nil
}

func (s *IssueService) UnresolvedBlockers(ctx context.Context, issueID string) ([]Issue, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, errors.New("issue id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	blocker.id,
	blocker.title,
	blocker.body,
		blocker.acceptance_criteria,
		blocker.priority,
		blocker.flow_id,
	blocker.created_by,
	blocker.created_by_session_id,
	blocker.source_issue_id,
	blocker.source_change_id,
	blocker.created_at,
		blocker.updated_at,
		blocker.lifecycle_state,
	blocker.done_resolution,
	blocker.done_at
FROM issue_relations r
JOIN issues blocker ON blocker.id = r.source_issue_id
	WHERE r.kind = ?
		AND r.target_issue_id = ?
		AND (blocker.lifecycle_state IS NULL OR blocker.lifecycle_state != ?)
ORDER BY blocker.priority DESC, blocker.updated_at DESC, blocker.id`,
		string(RelationBlocks),
		issueID,
		string(LifecycleDone),
	)
	if err != nil {
		return nil, fmt.Errorf("list issue blockers: %w", err)
	}
	defer rows.Close()

	var blockers []Issue
	for rows.Next() {
		blocker, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		blockers = append(blockers, blocker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue blockers: %w", err)
	}

	return blockers, nil
}

func allocateIssueID(ctx context.Context, tx *sql.Tx) (string, error) {
	var nextNumber int64
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'issue'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate issue id: %w", err)
	}

	return formatIssueID(nextNumber), nil
}

func formatIssueID(number int64) string {
	return fmt.Sprintf("i-%04d", number)
}

func normalizeCreateIssueInput(input CreateIssueInput) (CreateIssueInput, error) {
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return CreateIssueInput{}, errors.New("issue title is required")
	}
	if input.Priority < 0 {
		return CreateIssueInput{}, errors.New("issue priority must be non-negative")
	}

	if input.CreatedBy == "" {
		input.CreatedBy = ActorHuman
	}
	if err := validateActor(input.CreatedBy); err != nil {
		return CreateIssueInput{}, err
	}
	if input.CreatedBy == ActorAgent && (input.CreatedBySessionID == nil || strings.TrimSpace(*input.CreatedBySessionID) == "") {
		return CreateIssueInput{}, errors.New("agent-created issues require created_by_session_id")
	}

	if input.ScheduleState == "" {
		input.ScheduleState = ScheduleBacklog
	}
	if err := validateScheduleState(input.ScheduleState); err != nil {
		return CreateIssueInput{}, err
	}
	if input.ScheduleState == ScheduleClosed {
		return CreateIssueInput{}, errors.New("issues cannot be created closed")
	}

	if input.TriageState == "" {
		if input.CreatedBy == ActorAgent {
			input.TriageState = TriagePending
		} else {
			input.TriageState = TriageAccepted
		}
	}
	if err := validateTriageState(input.TriageState); err != nil {
		return CreateIssueInput{}, err
	}
	if input.TriageState == TriageRejected {
		return CreateIssueInput{}, errors.New("issues cannot be created rejected")
	}
	if input.ScheduleState != ScheduleBacklog && input.TriageState != TriageAccepted {
		return CreateIssueInput{}, errors.New("only accepted issues can be scheduled")
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

func validateIssueState(state IssueState) error {
	switch state {
	case IssueStateTriage, IssueStateBacklog, IssueStateUpNext, IssueStateClosed, IssueStateRejected:
		return nil
	default:
		return fmt.Errorf("invalid issue state: %s", state)
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

func issueHasParent(ctx context.Context, tx *sql.Tx, issueID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM issue_relations
WHERE target_issue_id = ? AND kind = ?`, issueID, string(RelationParentOf)).Scan(&count); err != nil {
		return false, fmt.Errorf("check issue parent: %w", err)
	}

	return count > 0, nil
}

func relationPathExists(ctx context.Context, tx *sql.Tx, kind RelationKind, startIssueID, targetIssueID string) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `
WITH RECURSIVE reachable(issue_id) AS (
	SELECT target_issue_id
	FROM issue_relations
	WHERE source_issue_id = ? AND kind = ?

	UNION

	SELECT r.target_issue_id
	FROM issue_relations r
	JOIN reachable ON reachable.issue_id = r.source_issue_id
	WHERE r.kind = ?
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE issue_id = ?)`,
		startIssueID,
		string(kind),
		string(kind),
		targetIssueID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check relation cycle: %w", err)
	}

	return exists == 1, nil
}

type issueScanner interface {
	Scan(dest ...any) error
}

// scanRows scans every row through scan, appending the results and closing rows
// when done. It collapses the repeated for-rows.Next/append/rows.Err boilerplate
// shared by the coordinator readers.
func scanRows[T any](rows *sql.Rows, scan func(issueScanner) (T, error)) ([]T, error) {
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

func scanIssue(scanner issueScanner) (Issue, error) {
	var issue Issue
	var flowID sql.NullString
	var createdBy string
	var createdBySessionID sql.NullString
	var sourceIssueID sql.NullString
	var sourceChangeID sql.NullString
	var createdAt string
	var updatedAt string
	var lifecycleState sql.NullString
	var doneResolution sql.NullString
	var doneAt sql.NullString

	if err := scanner.Scan(
		&issue.ID,
		&issue.Title,
		&issue.Body,
		&issue.AcceptanceCriteria,
		&issue.Priority,
		&flowID,
		&createdBy,
		&createdBySessionID,
		&sourceIssueID,
		&sourceChangeID,
		&createdAt,
		&updatedAt,
		&lifecycleState,
		&doneResolution,
		&doneAt,
	); err != nil {
		return Issue{}, fmt.Errorf("scan issue: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Issue{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Issue{}, err
	}

	// These fields exist only so dormant pre-v2 coordinator helpers still
	// compile. They are derived from the authoritative lifecycle and are never
	// persisted or exposed by the version-2 model.
	issue.ScheduleState = ScheduleBacklog
	issue.TriageState = TriageAccepted
	issue.RequiresHumanReview = true
	if flowID.Valid {
		issue.FlowID = flowID.String
	}
	issue.CreatedBy = Actor(createdBy)
	issue.CreatedBySessionID = nullableStringPointer(createdBySessionID)
	issue.SourceIssueID = nullableStringPointer(sourceIssueID)
	issue.SourceChangeID = nullableStringPointer(sourceChangeID)
	issue.CreatedAt = parsedCreatedAt
	issue.UpdatedAt = parsedUpdatedAt
	if lifecycleState.Valid {
		state := LifecycleState(lifecycleState.String)
		issue.State = &state
		switch state {
		case LifecycleScheduled, LifecycleInProgress:
			issue.ScheduleState = ScheduleUpNext
		case LifecycleDone:
			issue.ScheduleState = ScheduleClosed
		}
	}
	if doneResolution.Valid {
		resolution := DoneResolution(doneResolution.String)
		issue.DoneResolution = &resolution
	}
	if doneAt.Valid {
		parsedDoneAt, err := parseTime(doneAt.String)
		if err != nil {
			return Issue{}, err
		}
		issue.DoneAt = &parsedDoneAt
		issue.ClosedAt = &parsedDoneAt
	}

	return issue, nil
}

func scanIssues(rows *sql.Rows) ([]Issue, error) {
	var issues []Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issues: %w", err)
	}

	return issues, nil
}

func scanTag(scanner issueScanner) (Tag, error) {
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

func scanIssueRelation(scanner issueScanner) (IssueRelation, error) {
	var relation IssueRelation
	var kind string
	var createdBy string
	var createdAt string

	if err := scanner.Scan(
		&relation.SourceIssueID,
		&relation.TargetIssueID,
		&kind,
		&createdBy,
		&createdAt,
	); err != nil {
		return IssueRelation{}, fmt.Errorf("scan issue relation: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return IssueRelation{}, err
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
