package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
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
	ScheduleState       ScheduleState
	TriageState         TriageState
	RequiresHumanReview bool
	AutoMerge           bool
	PlanMode            bool             `json:"plan_mode"`
	PlanBody            string           `json:"plan_body,omitempty"`
	PlanStatusLogID     *int64           `json:"plan_status_log_id,omitempty"`
	PlanSessionID       string           `json:"plan_session_id,omitempty"`
	PlanSubmittedAt     *time.Time       `json:"plan_submitted_at,omitempty"`
	PlanApprovedAt      *time.Time       `json:"plan_approved_at,omitempty"`
	AgentHarness        string           `json:"agent_harness"`
	HarnessArgs         flowharness.Args `json:"harness_args"`
	CreatedBy           Actor
	CreatedBySessionID  *string
	SourceIssueID       *string
	SourceChangeID      *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ClosedAt            *time.Time
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
	PlanMode            bool
	AgentHarness        string
	HarnessArgs         flowharness.Args
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

type RecordIssuePlanInput struct {
	IssueID     string
	Body        string
	StatusLogID int64
	SessionID   string
	SubmittedAt time.Time
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
	PlanMode            *bool
	AgentHarness        *string
	HarnessArgs         *flowharness.ArgsPatch
}

type IssueFilter struct {
	ScheduleStates []ScheduleState
	TriageStates   []TriageState
	TagSlugs       []string
}

type CreateTagInput struct {
	Slug        string
	Name        string
	Color       string
	Description string
	CreatedBy   Actor
}

type Board struct {
	Backlog        []Issue
	UpNext         []Issue
	InProgress     []Issue
	NeedsAttention []Issue
}

// LaneState is the fine-grained derived sub-state of an open issue. It is the
// outcome of the board precedence cascade before coarsening into one of the
// four board lanes, and is surfaced as a pill on issue cards.
type LaneState string

const (
	LaneStateReadyToMerge     LaneState = "ready_to_merge"
	LaneStateChangesRequested LaneState = "changes_requested"
	LaneStatePlanning         LaneState = "planning"
	LaneStateInProgress       LaneState = "in_progress"
	LaneStateInReview         LaneState = "in_review"
	LaneStateTriage           LaneState = "triage"
	LaneStateUpNext           LaneState = "up_next"
	LaneStateBacklog          LaneState = "backlog"
)

type WaitReason string

const (
	WaitReasonPlanApproval WaitReason = "plan_approval"
	WaitReasonQuestion     WaitReason = "question"
	WaitReasonManualMerge  WaitReason = "manual_merge"
	WaitReasonHumanReview  WaitReason = "human_review"
	WaitReasonBlocked      WaitReason = "blocked"
	WaitReasonReviewCycles WaitReason = "review_cycle_limit"
	WaitReasonCrashLoop    WaitReason = "crash_loop"
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
	harnessArgsJSON, err := marshalHarnessArgs(issueInput.HarnessArgs)
	if err != nil {
		return Issue{}, err
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
	schedule_state,
	triage_state,
	requires_human_review,
	auto_merge,
	plan_mode,
	agent_harness,
	harness_args_json,
	created_by,
	created_by_session_id,
	source_issue_id,
	source_change_id,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		issueInput.Title,
		issueInput.Body,
		issueInput.AcceptanceCriteria,
		issueInput.Priority,
		string(issueInput.ScheduleState),
		string(issueInput.TriageState),
		boolInt(*issueInput.RequiresHumanReview),
		boolInt(*issueInput.AutoMerge),
		boolInt(issueInput.PlanMode),
		issueInput.AgentHarness,
		harnessArgsJSON,
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
	schedule_state,
	triage_state,
	requires_human_review,
	auto_merge,
	plan_mode,
	plan_body,
	plan_status_log_id,
	plan_session_id,
	plan_submitted_at,
	plan_approved_at,
	agent_harness,
	harness_args_json,
	created_by,
	created_by_session_id,
	source_issue_id,
	source_change_id,
	created_at,
	updated_at,
	closed_at
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
	i.schedule_state,
	i.triage_state,
	i.requires_human_review,
	i.auto_merge,
	i.plan_mode,
	i.plan_body,
	i.plan_status_log_id,
	i.plan_session_id,
	i.plan_submitted_at,
	i.plan_approved_at,
	i.agent_harness,
	i.harness_args_json,
	i.created_by,
	i.created_by_session_id,
	i.source_issue_id,
	i.source_change_id,
	i.created_at,
	i.updated_at,
	i.closed_at`

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
	if len(filter.ScheduleStates) > 0 {
		predicates = append(predicates, inPredicate("i.schedule_state", len(filter.ScheduleStates)))
		for _, state := range filter.ScheduleStates {
			args = append(args, string(state))
		}
	}
	if len(filter.TriageStates) > 0 {
		predicates = append(predicates, inPredicate("i.triage_state", len(filter.TriageStates)))
		for _, state := range filter.TriageStates {
			args = append(args, string(state))
		}
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
	ClosedOutcomeMerged    ClosedOutcome = "merged"
	ClosedOutcomeRejected  ClosedOutcome = "rejected"
	ClosedOutcomeAbandoned ClosedOutcome = "abandoned"
)

// ClosedIssueQuery bounds a page of closed issues. It is deliberately separate
// from IssueFilter/ListIssues (the board + triage hot path) so the unbounded
// history reader can never widen those queries.
type ClosedIssueQuery struct {
	// Limit caps the page size; <= 0 falls back to defaultClosedIssueLimit.
	Limit int
	// Before/BeforeID is the keyset cursor: only rows strictly older than this
	// (closed_at, id) pair are returned. Both come from a prior ClosedCursor.
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
// newest-closed first (closed_at desc, id desc tiebreak). It never loads the
// full set: closed issues grow unbounded, so callers must page or window. The
// returned cursor is non-nil only when more rows remain.
func (s *IssueService) ListClosedIssues(ctx context.Context, q ClosedIssueQuery) ([]Issue, *ClosedCursor, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultClosedIssueLimit
	}

	predicates := []string{"i.schedule_state = ?", "i.closed_at IS NOT NULL"}
	args := []any{string(ScheduleClosed)}

	if q.Before != nil {
		before := formatTime(*q.Before)
		predicates = append(predicates, "(i.closed_at < ? OR (i.closed_at = ? AND i.id < ?))")
		args = append(args, before, before, q.BeforeID)
	}
	if q.Within != nil {
		predicates = append(predicates, "i.closed_at >= ?")
		args = append(args, formatTime(*q.Within))
	}

	switch q.Outcome {
	case ClosedOutcomeAll:
		// No disposition predicate.
	case ClosedOutcomeRejected:
		predicates = append(predicates, "i.triage_state = ?")
		args = append(args, string(TriageRejected))
	case ClosedOutcomeMerged:
		predicates = append(predicates, "i.triage_state != ? AND EXISTS (SELECT 1 FROM changes c WHERE c.issue_id = i.id AND c.merged_at IS NOT NULL)")
		args = append(args, string(TriageRejected))
	case ClosedOutcomeAbandoned:
		predicates = append(predicates, "i.triage_state != ? AND NOT EXISTS (SELECT 1 FROM changes c WHERE c.issue_id = i.id AND c.merged_at IS NOT NULL)")
		args = append(args, string(TriageRejected))
	default:
		return nil, nil, fmt.Errorf("invalid closed outcome %q", q.Outcome)
	}

	query := "SELECT" + issueSelectColumns + "\nFROM issues i\nWHERE " + strings.Join(predicates, " AND ") +
		"\nORDER BY i.closed_at DESC, i.id DESC\nLIMIT ?"
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
		if last.ClosedAt != nil {
			next = &ClosedCursor{ClosedAt: *last.ClosedAt, ID: last.ID}
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
WHERE schedule_state = ?`, string(ScheduleClosed)).Scan(&count); err != nil {
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
	if input.RequiresHumanReview != nil {
		current.RequiresHumanReview = *input.RequiresHumanReview
	}
	if input.AutoMerge != nil {
		current.AutoMerge = *input.AutoMerge
	}
	if input.PlanMode != nil {
		if *input.PlanMode != current.PlanMode {
			current.PlanBody = ""
			current.PlanStatusLogID = nil
			current.PlanSessionID = ""
			current.PlanSubmittedAt = nil
			current.PlanApprovedAt = nil
		}
		current.PlanMode = *input.PlanMode
	}
	if input.AgentHarness != nil {
		agentHarness, err := normalizeAgentHarness(*input.AgentHarness)
		if err != nil {
			return Issue{}, err
		}
		current.AgentHarness = agentHarness
	}
	if input.HarnessArgs != nil {
		patch, err := flowharness.NormalizeArgsPatch(*input.HarnessArgs)
		if err != nil {
			return Issue{}, err
		}
		current.HarnessArgs = current.HarnessArgs.ApplyPatch(patch)
	}
	harnessArgsJSON, err := marshalHarnessArgs(current.HarnessArgs)
	if err != nil {
		return Issue{}, err
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET
	title = ?,
	body = ?,
	acceptance_criteria = ?,
	priority = ?,
	requires_human_review = ?,
	auto_merge = ?,
	plan_mode = ?,
	plan_body = ?,
	plan_status_log_id = ?,
	plan_session_id = ?,
	plan_submitted_at = ?,
	plan_approved_at = ?,
	agent_harness = ?,
	harness_args_json = ?,
	updated_at = ?
WHERE id = ?`,
		current.Title,
		current.Body,
		current.AcceptanceCriteria,
		current.Priority,
		boolInt(current.RequiresHumanReview),
		boolInt(current.AutoMerge),
		boolInt(current.PlanMode),
		current.PlanBody,
		nullableInt64Value(current.PlanStatusLogID),
		sqlitex.NullableNonEmptyString(current.PlanSessionID),
		nullableTimeValue(current.PlanSubmittedAt),
		nullableTimeValue(current.PlanApprovedAt),
		current.AgentHarness,
		harnessArgsJSON,
		formatTime(s.now().UTC()),
		id,
	); err != nil {
		return Issue{}, fmt.Errorf("edit issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) MarkPlanApproved(ctx context.Context, id string) (Issue, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Issue{}, errors.New("issue id is required")
	}

	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET
	plan_approved_at = COALESCE(plan_approved_at, ?),
	updated_at = CASE WHEN plan_approved_at IS NULL THEN ? ELSE updated_at END
WHERE id = ?`,
		nowText,
		nowText,
		id,
	); err != nil {
		return Issue{}, fmt.Errorf("mark plan approved: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) ClearPendingPlan(ctx context.Context, id string) (Issue, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Issue{}, errors.New("issue id is required")
	}

	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET
	plan_body = '',
	plan_status_log_id = NULL,
	plan_session_id = NULL,
	plan_submitted_at = NULL,
	plan_approved_at = NULL,
	updated_at = ?
WHERE id = ?`,
		nowText,
		id,
	); err != nil {
		return Issue{}, fmt.Errorf("clear pending plan: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) RecordPlan(ctx context.Context, input RecordIssuePlanInput) (Issue, error) {
	input.IssueID = strings.TrimSpace(input.IssueID)
	input.Body = strings.TrimSpace(input.Body)
	input.SessionID = strings.TrimSpace(input.SessionID)
	if input.IssueID == "" {
		return Issue{}, errors.New("issue id is required")
	}
	if input.Body == "" {
		return Issue{}, errors.New("plan body is required")
	}
	if input.StatusLogID <= 0 {
		return Issue{}, errors.New("plan status log id is required")
	}
	submittedAt := input.SubmittedAt.UTC()
	if submittedAt.IsZero() {
		submittedAt = s.now().UTC()
	}
	nowText := formatTime(s.now().UTC())
	statusID := input.StatusLogID
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET
	plan_body = ?,
	plan_status_log_id = ?,
	plan_session_id = ?,
	plan_submitted_at = ?,
	plan_approved_at = NULL,
	updated_at = ?
WHERE id = ?`,
		input.Body,
		statusID,
		sqlitex.NullableNonEmptyString(input.SessionID),
		formatTime(submittedAt),
		nowText,
		input.IssueID,
	); err != nil {
		return Issue{}, fmt.Errorf("record issue plan: %w", err)
	}

	return s.GetIssue(ctx, input.IssueID)
}

func (s *IssueService) ScheduleIssue(ctx context.Context, id string, state ScheduleState) (Issue, error) {
	if state != ScheduleBacklog && state != ScheduleUpNext {
		return Issue{}, errors.New("schedule state must be backlog or up_next")
	}

	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	if current.TriageState != TriageAccepted {
		return Issue{}, errors.New("only accepted issues can be scheduled")
	}
	if current.ScheduleState == ScheduleClosed {
		return Issue{}, errors.New("closed issues cannot be scheduled")
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET schedule_state = ?, updated_at = ?
WHERE id = ?`, string(state), formatTime(s.now().UTC()), id); err != nil {
		return Issue{}, fmt.Errorf("schedule issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) CloseIssue(ctx context.Context, id string) (Issue, error) {
	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET schedule_state = ?, updated_at = ?, closed_at = COALESCE(closed_at, ?)
WHERE id = ?`, string(ScheduleClosed), nowText, nowText, id); err != nil {
		return Issue{}, fmt.Errorf("close issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

// issueStateUpdate describes how SetIssueState rewrites an issue's
// schedule/triage/closed_at columns for a target state. An empty triage means
// "keep the issue's current triage"; clearClosed wipes closed_at to NULL while
// the closed states preserve any existing close timestamp.
type issueStateUpdate struct {
	schedule    ScheduleState
	triage      TriageState
	clearClosed bool
}

var issueStateUpdates = map[IssueState]issueStateUpdate{
	IssueStateTriage:   {schedule: ScheduleBacklog, triage: TriagePending, clearClosed: true},
	IssueStateBacklog:  {schedule: ScheduleBacklog, triage: TriageAccepted, clearClosed: true},
	IssueStateUpNext:   {schedule: ScheduleUpNext, triage: TriageAccepted, clearClosed: true},
	IssueStateClosed:   {schedule: ScheduleClosed},
	IssueStateRejected: {schedule: ScheduleClosed, triage: TriageRejected},
}

func (s *IssueService) SetIssueState(ctx context.Context, id string, state IssueState) (Issue, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Issue{}, errors.New("issue id is required")
	}
	state = IssueState(strings.TrimSpace(string(state)))
	if err := validateIssueState(state); err != nil {
		return Issue{}, err
	}

	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	if state != IssueStateClosed {
		merged, err := s.HasMergedChange(ctx, id)
		if err != nil {
			return Issue{}, err
		}
		if merged {
			return Issue{}, errors.New("merged issues cannot be moved to a manual state other than closed")
		}
	}

	update := issueStateUpdates[state]
	triage := update.triage
	if triage == "" {
		triage = current.TriageState
	}
	nowText := formatTime(s.now().UTC())
	// closed_at is NULL for open states; closed states preserve any existing
	// close timestamp (COALESCE(closed_at, now), computed here from current).
	var closedAt any
	if !update.clearClosed {
		closedAt = nowText
		if current.ClosedAt != nil {
			closedAt = formatTime(*current.ClosedAt)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET schedule_state = ?, triage_state = ?, updated_at = ?, closed_at = ?
WHERE id = ?`, string(update.schedule), string(triage), nowText, closedAt, id); err != nil {
		return Issue{}, fmt.Errorf("set issue state: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) AcceptTriage(ctx context.Context, id string) (Issue, error) {
	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	if current.ScheduleState == ScheduleClosed {
		return Issue{}, errors.New("closed issues cannot be accepted from triage")
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET triage_state = ?, updated_at = ?
WHERE id = ?`, string(TriageAccepted), formatTime(s.now().UTC()), id); err != nil {
		return Issue{}, fmt.Errorf("accept triage issue: %w", err)
	}

	return s.GetIssue(ctx, id)
}

func (s *IssueService) RejectTriage(ctx context.Context, id string) (Issue, error) {
	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE issues
SET triage_state = ?, schedule_state = ?, updated_at = ?, closed_at = COALESCE(closed_at, ?)
WHERE id = ?`, string(TriageRejected), string(ScheduleClosed), nowText, nowText, id); err != nil {
		return Issue{}, fmt.Errorf("reject triage issue: %w", err)
	}

	return s.GetIssue(ctx, id)
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
		phase, err := derivePhaseFromIssue(ctx, s.db, issue)
		if err != nil {
			return BoardResult{}, err
		}
		state, ok, err := s.laneStateForPhase(ctx, issue.ID, phase)
		if err != nil {
			return BoardResult{}, err
		}
		if !ok {
			continue
		}
		result.LaneStates[issue.ID] = state
		if reason, err := s.waitReason(ctx, issue, state); err != nil {
			return BoardResult{}, err
		} else if reason != "" {
			result.WaitReasons[issue.ID] = reason
		}

		blocked, err := s.issueIsBlocked(ctx, issue.ID)
		if err != nil {
			return BoardResult{}, err
		}
		if blocked {
			result.BlockedIDs = append(result.BlockedIDs, issue.ID)
			result.WaitReasons[issue.ID] = WaitReasonBlocked
		}

		lane := laneForState(state)
		if blocked || result.WaitReasons[issue.ID] != "" {
			lane = "needs_attention"
		}

		switch lane {
		case "backlog":
			result.Board.Backlog = append(result.Board.Backlog, issue)
		case "up_next":
			result.Board.UpNext = append(result.Board.UpNext, issue)
		case "in_progress":
			result.Board.InProgress = append(result.Board.InProgress, issue)
		case "needs_attention":
			result.Board.NeedsAttention = append(result.Board.NeedsAttention, issue)
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
	case PhasePlanning:
		return LaneStatePlanning, true
	case PhaseAuthoring:
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
	if sessionState, ok, err := activeSessionStateForIssue(ctx, s.db, issue.ID); err != nil {
		return "", err
	} else if ok && sessionState == SessionWaiting {
		if issue.PlanMode && issue.PlanApprovedAt == nil && strings.TrimSpace(issue.PlanBody) != "" {
			return WaitReasonPlanApproval, nil
		}
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
	case LaneStatePlanning, LaneStateInProgress, LaneStateInReview, LaneStateChangesRequested:
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
	AND blocker.schedule_state != ?`,
		string(RelationBlocks),
		issueID,
		string(ScheduleClosed),
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
	blocker.schedule_state,
	blocker.triage_state,
	blocker.requires_human_review,
	blocker.auto_merge,
	blocker.plan_mode,
	blocker.plan_body,
	blocker.plan_status_log_id,
	blocker.plan_session_id,
	blocker.plan_submitted_at,
	blocker.plan_approved_at,
	blocker.agent_harness,
	blocker.harness_args_json,
	blocker.created_by,
	blocker.created_by_session_id,
	blocker.source_issue_id,
	blocker.source_change_id,
	blocker.created_at,
	blocker.updated_at,
	blocker.closed_at
FROM issue_relations r
JOIN issues blocker ON blocker.id = r.source_issue_id
WHERE r.kind = ?
	AND r.target_issue_id = ?
	AND blocker.schedule_state != ?
ORDER BY blocker.priority DESC, blocker.updated_at DESC, blocker.id`,
		string(RelationBlocks),
		issueID,
		string(ScheduleClosed),
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
	agentHarness, err := normalizeAgentHarness(input.AgentHarness)
	if err != nil {
		return CreateIssueInput{}, err
	}
	input.AgentHarness = agentHarness
	harnessArgs, err := flowharness.NormalizeArgs(input.HarnessArgs)
	if err != nil {
		return CreateIssueInput{}, err
	}
	input.HarnessArgs = harnessArgs

	return input, nil
}

func normalizeAgentHarness(value string) (string, error) {
	agentHarness := flowharness.NormalizeName(value)
	if agentHarness == "" {
		agentHarness = flowharness.DefaultAgentName()
	}
	if err := flowharness.ValidateAgentName(agentHarness); err != nil {
		return "", err
	}
	return agentHarness, nil
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

func marshalHarnessArgs(args flowharness.Args) (string, error) {
	normalized, err := flowharness.NormalizeArgs(args)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode harness args: %w", err)
	}
	return string(encoded), nil
}

func unmarshalHarnessArgs(raw string) (flowharness.Args, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var args flowharness.Args
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return flowharness.Args{}, fmt.Errorf("decode harness args: %w", err)
	}
	normalized, err := flowharness.NormalizeArgs(args)
	if err != nil {
		return flowharness.Args{}, err
	}
	return normalized, nil
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
	var scheduleState string
	var triageState string
	var requiresHumanReview int
	var autoMerge int
	var planMode int
	var planStatusLogID sql.NullInt64
	var planSessionID sql.NullString
	var planSubmittedAt sql.NullString
	var planApprovedAt sql.NullString
	var agentHarness string
	var harnessArgsJSON string
	var createdBy string
	var createdBySessionID sql.NullString
	var sourceIssueID sql.NullString
	var sourceChangeID sql.NullString
	var createdAt string
	var updatedAt string
	var closedAt sql.NullString

	if err := scanner.Scan(
		&issue.ID,
		&issue.Title,
		&issue.Body,
		&issue.AcceptanceCriteria,
		&issue.Priority,
		&scheduleState,
		&triageState,
		&requiresHumanReview,
		&autoMerge,
		&planMode,
		&issue.PlanBody,
		&planStatusLogID,
		&planSessionID,
		&planSubmittedAt,
		&planApprovedAt,
		&agentHarness,
		&harnessArgsJSON,
		&createdBy,
		&createdBySessionID,
		&sourceIssueID,
		&sourceChangeID,
		&createdAt,
		&updatedAt,
		&closedAt,
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

	issue.ScheduleState = ScheduleState(scheduleState)
	issue.TriageState = TriageState(triageState)
	issue.RequiresHumanReview = requiresHumanReview == 1
	issue.AutoMerge = autoMerge == 1
	issue.PlanMode = planMode == 1
	if planStatusLogID.Valid {
		value := planStatusLogID.Int64
		issue.PlanStatusLogID = &value
	}
	if planSessionID.Valid {
		issue.PlanSessionID = planSessionID.String
	}
	if planSubmittedAt.Valid {
		parsedPlanSubmittedAt, err := parseTime(planSubmittedAt.String)
		if err != nil {
			return Issue{}, err
		}
		issue.PlanSubmittedAt = &parsedPlanSubmittedAt
	}
	if planApprovedAt.Valid {
		parsedPlanApprovedAt, err := parseTime(planApprovedAt.String)
		if err != nil {
			return Issue{}, err
		}
		issue.PlanApprovedAt = &parsedPlanApprovedAt
	}
	issue.AgentHarness = agentHarness
	harnessArgs, err := unmarshalHarnessArgs(harnessArgsJSON)
	if err != nil {
		return Issue{}, err
	}
	issue.HarnessArgs = harnessArgs
	issue.CreatedBy = Actor(createdBy)
	issue.CreatedBySessionID = nullableStringPointer(createdBySessionID)
	issue.SourceIssueID = nullableStringPointer(sourceIssueID)
	issue.SourceChangeID = nullableStringPointer(sourceChangeID)
	issue.CreatedAt = parsedCreatedAt
	issue.UpdatedAt = parsedUpdatedAt
	if closedAt.Valid {
		parsedClosedAt, err := parseTime(closedAt.String)
		if err != nil {
			return Issue{}, err
		}
		issue.ClosedAt = &parsedClosedAt
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
