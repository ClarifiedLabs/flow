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

type EpicStatus string

const (
	EpicOpen      EpicStatus = "open"
	EpicCompleted EpicStatus = "completed"
	EpicArchived  EpicStatus = "archived"
)

type EpicCompletionPolicy string

const (
	EpicAllChildren EpicCompletionPolicy = "all_children"
	EpicManual      EpicCompletionPolicy = "manual"
)

var (
	ErrEpicNotFound       = errors.New("epic not found")
	ErrEpicClosed         = errors.New("epic is completed or archived")
	ErrEpicNotCompletable = errors.New("epic is not eligible for completion")
)

type Epic struct {
	ID                     string               `json:"id"`
	Title                  string               `json:"title"`
	Body                   string               `json:"body"`
	Priority               int                  `json:"priority"`
	Status                 EpicStatus           `json:"status"`
	CompletionPolicy       EpicCompletionPolicy `json:"completion_policy"`
	CompletedAutomatically bool                 `json:"completed_automatically"`
	CreatedBy              Actor                `json:"created_by"`
	CreatedAt              time.Time            `json:"created_at"`
	UpdatedAt              time.Time            `json:"updated_at"`
	CompletedAt            *time.Time           `json:"completed_at,omitempty"`
	ArchivedAt             *time.Time           `json:"archived_at,omitempty"`
}

type CreateEpicInput struct {
	Title             string
	Body              string
	Priority          int
	CompletionPolicy  EpicCompletionPolicy
	ParentItemID      string
	WorkItemRelations []CreateWorkItemRelationInput
	CreatedBy         Actor
}

type EditEpicInput struct {
	Title            *string
	Body             *string
	Priority         *int
	CompletionPolicy *EpicCompletionPolicy
}

type EpicService struct {
	db        *sql.DB
	projectID string
	items     *WorkItemService
	now       func() time.Time
	eventLog  *EventLogService
}

// SetEventLog wires the project event log; a nil log disables emission.
func (s *EpicService) SetEventLog(log *EventLogService) {
	s.eventLog = log
}

func NewEpicService(database *sql.DB, projectID string, items *WorkItemService) *EpicService {
	if items == nil {
		items = NewWorkItemService(database, projectID)
	}
	return &EpicService{db: database, projectID: strings.TrimSpace(projectID), items: items, now: sqlitex.UTCNow}
}

func (s *EpicService) Create(ctx context.Context, input CreateEpicInput) (Epic, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return Epic{}, errors.New("epic title is required")
	}
	if input.Priority < 0 {
		return Epic{}, errors.New("epic priority cannot be negative")
	}
	policy := input.CompletionPolicy
	if policy == "" {
		policy = EpicAllChildren
	}
	if policy != EpicAllChildren && policy != EpicManual {
		return Epic{}, errors.New("invalid epic completion policy")
	}
	actor := defaultActor(input.CreatedBy, ActorHuman)
	if err := validateActor(actor); err != nil {
		return Epic{}, err
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Epic{}, err
	}
	defer tx.Rollback()
	createRelationPlan, err := s.items.prepareCreateRelations(ctx, tx, WorkItemEpic, input.ParentItemID, input.WorkItemRelations, actor)
	if err != nil {
		return Epic{}, err
	}
	id, err := s.allocateID(ctx, tx)
	if err != nil {
		return Epic{}, err
	}
	now := s.now().UTC()
	nowText := formatTime(now)
	if err := insertWorkItem(ctx, tx, id, WorkItemEpic, nowText); err != nil {
		return Epic{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO epics (
	id, title, body, priority, status, completion_policy,
	completed_automatically, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, 'open', ?, 0, ?, ?, ?)`,
		id, title, input.Body, input.Priority, string(policy), string(actor), nowText, nowText); err != nil {
		return Epic{}, fmt.Errorf("insert epic: %w", err)
	}
	touched, err := s.items.linkCreateRelationsTx(ctx, tx, id, createRelationPlan, actor)
	if err != nil {
		return Epic{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, touched, now); err != nil {
		return Epic{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Epic{}, err
	}
	return s.Get(ctx, id)
}

func (s *EpicService) Get(ctx context.Context, id string) (Epic, error) {
	epic, err := scanEpic(s.db.QueryRowContext(ctx, epicSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Epic{}, ErrEpicNotFound
	}
	return epic, err
}

func (s *EpicService) Resolve(ctx context.Context, ref string) (Epic, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Epic{}, errors.New("epic reference is required")
	}
	epic, err := s.Get(ctx, ref)
	if err == nil || !errors.Is(err, ErrEpicNotFound) {
		return epic, err
	}
	rows, err := s.db.QueryContext(ctx, epicSelect+` WHERE lower(title) = lower(?) ORDER BY id`, ref)
	if err != nil {
		return Epic{}, err
	}
	defer rows.Close()
	var matches []Epic
	for rows.Next() {
		epic, err := scanEpic(rows)
		if err != nil {
			return Epic{}, err
		}
		matches = append(matches, epic)
	}
	if len(matches) == 0 {
		return Epic{}, ErrEpicNotFound
	}
	if len(matches) > 1 {
		return Epic{}, fmt.Errorf("epic title %q is ambiguous; use an id", ref)
	}
	return matches[0], nil
}

func (s *EpicService) List(ctx context.Context, status EpicStatus) ([]Epic, error) {
	switch status {
	case "", EpicOpen, EpicCompleted, EpicArchived:
	default:
		return nil, fmt.Errorf("invalid epic status: %s", status)
	}
	query := epicSelect
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, string(status))
	}
	query += ` ORDER BY status, priority DESC, updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var epics []Epic
	for rows.Next() {
		epic, err := scanEpic(rows)
		if err != nil {
			return nil, err
		}
		epics = append(epics, epic)
	}
	return epics, rows.Err()
}

func (s *EpicService) Edit(ctx context.Context, id string, input EditEpicInput) (Epic, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Epic{}, err
	}
	defer tx.Rollback()
	current, err := scanEpic(tx.QueryRowContext(ctx, epicSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Epic{}, ErrEpicNotFound
	}
	if err != nil {
		return Epic{}, err
	}
	if current.Status == EpicArchived {
		return Epic{}, ErrEpicClosed
	}
	if input.Title != nil {
		current.Title = strings.TrimSpace(*input.Title)
		if current.Title == "" {
			return Epic{}, errors.New("epic title is required")
		}
	}
	if input.Body != nil {
		current.Body = *input.Body
	}
	if input.Priority != nil {
		if *input.Priority < 0 {
			return Epic{}, errors.New("epic priority cannot be negative")
		}
		current.Priority = *input.Priority
	}
	if input.CompletionPolicy != nil {
		if *input.CompletionPolicy != EpicAllChildren && *input.CompletionPolicy != EpicManual {
			return Epic{}, errors.New("invalid epic completion policy")
		}
		current.CompletionPolicy = *input.CompletionPolicy
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE epics SET title = ?, body = ?, priority = ?, completion_policy = ?, updated_at = ?
WHERE id = ?`, current.Title, current.Body, current.Priority, string(current.CompletionPolicy), formatTime(s.now().UTC()), current.ID); err != nil {
		return Epic{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{current.ID}, s.now().UTC()); err != nil {
		return Epic{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Epic{}, err
	}
	return s.Get(ctx, current.ID)
}

func (s *EpicService) Complete(ctx context.Context, id string, actor Actor) (Epic, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Epic{}, err
	}
	defer tx.Rollback()
	epic, err := scanEpic(tx.QueryRowContext(ctx, epicSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Epic{}, ErrEpicNotFound
	}
	if err != nil {
		return Epic{}, err
	}
	if epic.Status == EpicCompleted {
		return epic, nil
	}
	if epic.Status == EpicArchived {
		return Epic{}, ErrEpicClosed
	}
	eligible, childCount, err := epicEligibleTx(ctx, tx, epic.ID)
	if err != nil {
		return Epic{}, err
	}
	if !eligible || (epic.CompletionPolicy == EpicAllChildren && childCount == 0) {
		return Epic{}, ErrEpicNotCompletable
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE epics SET status = 'completed', completed_automatically = 0,
	completed_at = ?, archived_at = NULL, updated_at = ? WHERE id = ?`, formatTime(now), formatTime(now), epic.ID); err != nil {
		return Epic{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{epic.ID}, now); err != nil {
		return Epic{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Epic{}, err
	}
	appendEventLog(ctx, s.eventLog, Event{
		Kind:    EventEpicCompleted,
		Actor:   string(actor),
		Payload: eventPayload(map[string]any{"epic_id": epic.ID, "title": epic.Title}),
	})
	return s.Get(ctx, epic.ID)
}

func (s *EpicService) Reopen(ctx context.Context, id string, actor Actor) (Epic, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Epic{}, err
	}
	defer tx.Rollback()
	epic, err := scanEpic(tx.QueryRowContext(ctx, epicSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Epic{}, ErrEpicNotFound
	}
	if err != nil {
		return Epic{}, err
	}
	if epic.Status == EpicArchived {
		return Epic{}, ErrEpicClosed
	}
	if epic.Status == EpicOpen {
		return epic, nil
	}
	blockedByAncestor, err := hasManuallyCompletedAncestorTx(ctx, tx, epic.ID)
	if err != nil {
		return Epic{}, err
	}
	if blockedByAncestor {
		return Epic{}, ErrWorkItemParentClosed
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE epics SET status = 'open', completed_automatically = 0,
	completed_at = NULL, updated_at = ? WHERE id = ?`, formatTime(now), epic.ID); err != nil {
		return Epic{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{epic.ID}, now); err != nil {
		return Epic{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Epic{}, err
	}
	appendEventLog(ctx, s.eventLog, Event{
		Kind:    EventEpicReopened,
		Actor:   string(actor),
		Payload: eventPayload(map[string]any{"epic_id": epic.ID, "title": epic.Title}),
	})
	return s.Get(ctx, epic.ID)
}

func (s *EpicService) Archive(ctx context.Context, id string, actor Actor) (Epic, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Epic{}, err
	}
	defer tx.Rollback()
	epic, err := scanEpic(tx.QueryRowContext(ctx, epicSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Epic{}, ErrEpicNotFound
	}
	if err != nil {
		return Epic{}, err
	}
	if epic.Status == EpicArchived {
		return epic, nil
	}
	eligible, _, err := epicEligibleTx(ctx, tx, epic.ID)
	if err != nil {
		return Epic{}, err
	}
	if !eligible {
		return Epic{}, ErrEpicNotCompletable
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE epics SET status = 'archived', completed_automatically = 0,
	archived_at = ?, updated_at = ? WHERE id = ?`, formatTime(now), formatTime(now), epic.ID); err != nil {
		return Epic{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{epic.ID}, now); err != nil {
		return Epic{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Epic{}, err
	}
	appendEventLog(ctx, s.eventLog, Event{
		Kind:    EventEpicArchived,
		Actor:   string(actor),
		Payload: eventPayload(map[string]any{"epic_id": epic.ID, "title": epic.Title}),
	})
	return s.Get(ctx, epic.ID)
}

func (s *EpicService) Reconcile(ctx context.Context, itemIDs ...string) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reconcileEpicAncestorsTx(ctx, tx, itemIDs, s.now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *EpicService) allocateID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var number int64
	if err := q.QueryRowContext(ctx, `
UPDATE id_allocators SET next_number = next_number + 1
WHERE name = 'epic' RETURNING next_number - 1`).Scan(&number); err != nil {
		return "", fmt.Errorf("allocate epic id: %w", err)
	}
	key, err := projectKeyFromID(s.projectID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("e-%s-%04d", key, number), nil
}

const epicSelect = `SELECT
	id, title, body, priority, status, completion_policy, completed_automatically,
	created_by, created_at, updated_at, completed_at, archived_at
FROM epics`

type epicScanner interface{ Scan(...any) error }

func scanEpic(row epicScanner) (Epic, error) {
	var epic Epic
	var status, policy, actor, createdAt, updatedAt string
	var automatic int
	var completedAt, archivedAt sql.NullString
	if err := row.Scan(
		&epic.ID, &epic.Title, &epic.Body, &epic.Priority, &status, &policy, &automatic,
		&actor, &createdAt, &updatedAt, &completedAt, &archivedAt,
	); err != nil {
		return Epic{}, err
	}
	var err error
	epic.Status = EpicStatus(status)
	epic.CompletionPolicy = EpicCompletionPolicy(policy)
	epic.CompletedAutomatically = automatic != 0
	epic.CreatedBy = Actor(actor)
	epic.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Epic{}, err
	}
	epic.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Epic{}, err
	}
	if completedAt.Valid {
		value, err := parseTime(completedAt.String)
		if err != nil {
			return Epic{}, err
		}
		epic.CompletedAt = &value
	}
	if archivedAt.Valid {
		value, err := parseTime(archivedAt.String)
		if err != nil {
			return Epic{}, err
		}
		epic.ArchivedAt = &value
	}
	return epic, nil
}

func reconcileEpicAncestorsTx(ctx context.Context, q workItemRelationQuerier, itemIDs []string, now time.Time) error {
	ids := uniqueTrimmed(itemIDs)
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	rows, err := q.QueryContext(ctx, `
WITH RECURSIVE touched(id, depth) AS (
	SELECT wi.id, 0 FROM work_items wi WHERE `+inPredicate("wi.id", len(ids))+`
	UNION
	SELECT r.target_item_id, touched.depth
	FROM touched
	JOIN work_item_relations r ON r.source_item_id = touched.id AND r.kind = 'blocks'
	UNION
	SELECT r.target_item_id, touched.depth + 1
	FROM touched
	JOIN work_item_relations r ON r.source_item_id = touched.id AND r.kind = 'parent_of'
), candidates(id, depth) AS (
	SELECT id, depth FROM touched
	UNION
	SELECT r.source_item_id, candidates.depth + 1
	FROM candidates
	JOIN work_item_relations r ON r.target_item_id = candidates.id AND r.kind = 'parent_of'
)
SELECT candidates.id, MAX(candidates.depth)
FROM candidates JOIN work_items wi ON wi.id = candidates.id AND wi.kind = 'epic'
GROUP BY candidates.id ORDER BY MAX(candidates.depth)`, args...)
	if err != nil {
		return err
	}
	var epicIDs []string
	for rows.Next() {
		var id string
		var depth int
		if err := rows.Scan(&id, &depth); err != nil {
			rows.Close()
			return err
		}
		epicIDs = append(epicIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// Child epics are selected before their ancestors. Repeat until stable so a
	// state transition at one level is visible at every higher level.
	for pass := 0; pass <= len(epicIDs); pass++ {
		changed := false
		for _, epicID := range epicIDs {
			var status, policy string
			var automatic int
			if err := q.QueryRowContext(ctx, `
SELECT status, completion_policy, completed_automatically FROM epics WHERE id = ?`, epicID).Scan(&status, &policy, &automatic); err != nil {
				return err
			}
			if status == string(EpicArchived) || policy == string(EpicManual) {
				continue
			}
			eligible, childCount, err := epicEligibleTx(ctx, q, epicID)
			if err != nil {
				return err
			}
			shouldComplete := eligible && childCount > 0
			switch {
			case status == string(EpicOpen) && shouldComplete:
				if _, err := q.ExecContext(ctx, `
UPDATE epics SET status = 'completed', completed_automatically = 1,
	completed_at = ?, updated_at = ? WHERE id = ?`, formatTime(now), formatTime(now), epicID); err != nil {
					return err
				}
				changed = true
			case status == string(EpicCompleted) && automatic != 0 && !shouldComplete:
				if _, err := q.ExecContext(ctx, `
UPDATE epics SET status = 'open', completed_automatically = 0,
	completed_at = NULL, updated_at = ? WHERE id = ?`, formatTime(now), epicID); err != nil {
					return err
				}
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return nil
}

func epicEligibleTx(ctx context.Context, q workItemRelationQuerier, epicID string) (bool, int, error) {
	rows, err := q.QueryContext(ctx, `
SELECT r.target_item_id
FROM work_item_relations r
WHERE r.source_item_id = ? AND r.kind = 'parent_of'`, epicID)
	if err != nil {
		return false, 0, err
	}
	var children []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, 0, err
		}
		children = append(children, id)
	}
	if err := rows.Close(); err != nil {
		return false, 0, err
	}
	for _, childID := range children {
		terminal, err := workItemTerminalTx(ctx, q, childID)
		if err != nil {
			return false, len(children), err
		}
		if !terminal {
			return false, len(children), nil
		}
	}
	unresolved, err := effectiveUnresolvedBlockerCountTx(ctx, q, epicID)
	if err != nil {
		return false, len(children), err
	}
	return unresolved == 0, len(children), nil
}

func workItemTerminalTx(ctx context.Context, q workItemRelationQuerier, itemID string) (bool, error) {
	var terminal int
	err := q.QueryRowContext(ctx, `
SELECT CASE wi.kind
	WHEN 'task' THEN COALESCE((SELECT lifecycle_state = 'done' FROM tasks WHERE id = wi.id), 0)
	WHEN 'epic' THEN COALESCE((SELECT status IN ('completed', 'archived') FROM epics WHERE id = wi.id), 0)
	WHEN 'feature' THEN COALESCE((SELECT status IN ('landed', 'archived') FROM features WHERE id = wi.id), 0)
END
FROM work_items wi WHERE wi.id = ?`, itemID).Scan(&terminal)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrWorkItemNotFound
	}
	return terminal != 0, err
}

func effectiveUnresolvedBlockerCountTx(ctx context.Context, q workItemRelationQuerier, itemID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
WITH RECURSIVE ancestors(id) AS (
	VALUES (?)
	UNION ALL
	SELECT r.source_item_id FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT DISTINCT r.source_item_id
FROM ancestors
JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'blocks'`, itemID)
	if err != nil {
		return 0, err
	}
	var blockerIDs []string
	for rows.Next() {
		var blockerID string
		if err := rows.Scan(&blockerID); err != nil {
			rows.Close()
			return 0, err
		}
		blockerIDs = append(blockerIDs, blockerID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	count := 0
	for _, blockerID := range blockerIDs {
		terminal, err := workItemTerminalTx(ctx, q, blockerID)
		if err != nil {
			return 0, err
		}
		if !terminal {
			count++
		}
	}
	return count, nil
}

func hasManuallyCompletedAncestorTx(ctx context.Context, q workItemRelationQuerier, itemID string) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(id) AS (
	SELECT source_item_id FROM work_item_relations
	WHERE target_item_id = ? AND kind = 'parent_of'
	UNION ALL
	SELECT r.source_item_id FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT EXISTS(
	SELECT 1 FROM ancestors JOIN epics e ON e.id = ancestors.id
	WHERE e.status = 'completed' AND e.completed_automatically = 0
)`, itemID).Scan(&found)
	return found != 0, err
}
