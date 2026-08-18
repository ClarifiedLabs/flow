package coordinator

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// Unified event log kinds. These strings are part of the machine contract
// (CLI --agent output and the /v2 events API); new kinds may be added, but
// existing kinds and payload fields must not change without a contract bump.
const (
	EventTaskCreated      = "task.created"
	EventTaskEdited       = "task.edited"
	EventTaskDone         = "task.done"
	EventTaskReopened     = "task.reopened"
	EventTaskReset        = "task.reset"
	EventRelationLinked   = "relation.linked"
	EventRelationUnlinked = "relation.unlinked"
	EventEpicCompleted    = "epic.completed"
	EventEpicReopened     = "epic.reopened"
	EventEpicArchived     = "epic.archived"
	EventFeatureCreated   = "feature.created"
	EventFeatureLanded    = "feature.landed"
	EventFeatureArchived  = "feature.archived"
	EventSessionStatus    = "session.status"
	EventGitPush          = "git.push"
)

// Event is one row of the project event log. Seq is the durable, monotonically
// increasing cursor consumers page with; the reference columns (task_id,
// session_id, run_id, change_id) index the event, and Payload carries the
// kind-specific detail.
type Event struct {
	Seq        int64           `json:"seq"`
	ID         string          `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Kind       string          `json:"kind"`
	Actor      string          `json:"actor,omitempty"`
	TaskID     string          `json:"task_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	ChangeID   string          `json:"change_id,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// EventLogService appends to and pages through the project event log.
type EventLogService struct {
	db  *sql.DB
	now func() time.Time
}

func NewEventLogService(database *sql.DB) *EventLogService {
	return &EventLogService{db: database, now: sqlitex.UTCNow}
}

// Append writes the event, assigning its id, occurred_at, and seq. Payload
// defaults to an empty JSON object.
func (s *EventLogService) Append(ctx context.Context, event Event) (Event, error) {
	event.Kind = strings.TrimSpace(event.Kind)
	if event.Kind == "" {
		return Event{}, errors.New("event kind is required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.now().UTC()
	}
	if event.ID == "" {
		event.ID = newEventID()
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return Event{}, fmt.Errorf("event payload is not valid JSON")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO event_log (id, occurred_at, kind, actor, task_id, session_id, run_id, change_id, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		formatTime(event.OccurredAt.UTC()),
		event.Kind,
		event.Actor,
		event.TaskID,
		event.SessionID,
		event.RunID,
		event.ChangeID,
		string(payload),
	)
	if err != nil {
		return Event{}, fmt.Errorf("append event: %w", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("read event seq: %w", err)
	}
	event.Seq = seq
	event.Payload = payload
	return event, nil
}

// EventLogMaxLimit bounds one page; ListEvents clamps larger requests.
const EventLogMaxLimit = 500

// EventFilter narrows a List query. Empty fields match everything.
type EventFilter struct {
	Kind   string
	TaskID string
	Actor  string
}

// List returns events with seq > since in seq order, at most limit (default
// 100, clamped to EventLogMaxLimit).
func (s *EventLogService) List(ctx context.Context, since int64, limit int) ([]Event, error) {
	return s.ListFiltered(ctx, since, limit, EventFilter{})
}

// ListFiltered is List with kind/task/actor filters applied in SQL.
func (s *EventLogService) ListFiltered(ctx context.Context, since int64, limit int, filter EventFilter) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > EventLogMaxLimit {
		limit = EventLogMaxLimit
	}
	query := `
SELECT seq, id, occurred_at, kind, actor, task_id, session_id, run_id, change_id, payload
FROM event_log
WHERE seq > ?`
	args := []any{since}
	if filter.Kind != "" {
		query += `
AND kind = ?`
		args = append(args, filter.Kind)
	}
	if filter.TaskID != "" {
		query += `
AND task_id = ?`
		args = append(args, filter.TaskID)
	}
	if filter.Actor != "" {
		query += `
AND actor = ?`
		args = append(args, filter.Actor)
	}
	query += `
ORDER BY seq
LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var event Event
		var occurredAt string
		var payload string
		if err := rows.Scan(&event.Seq, &event.ID, &occurredAt, &event.Kind, &event.Actor,
			&event.TaskID, &event.SessionID, &event.RunID, &event.ChangeID, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		parsed, err := parseTime(occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse event occurred_at: %w", err)
		}
		event.OccurredAt = parsed
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

// LatestSeq returns the highest committed seq, or 0 when the log is empty.
// Post-commit hook dispatch uses it to place its cursor at the tail on boot
// so a restart never refires events that committed while it was down.
func (s *EventLogService) LatestSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM event_log`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("read latest event seq: %w", err)
	}
	return seq, nil
}

// appendEventLog is the nil-safe, non-fatal appender for mutation paths that
// do not hold the write transaction at the emission site (fire-and-forget
// producers such as the git-event consumer). Transactional mutations should
// prefer stageEvent so the append lands before the next mutation can commit,
// preserving event order. Emission must never fail the mutation that
// triggered it, so failures are logged and swallowed.
func appendEventLog(ctx context.Context, log *EventLogService, event Event) {
	if log == nil {
		return
	}
	if _, err := log.Append(ctx, event); err != nil {
		slog.Warn("event log append failed", "kind", event.Kind, "task_id", event.TaskID, "error", err)
	}
}

// eventPayload marshals a small payload map; marshal failures degrade to an
// empty object because payloads are best-effort detail, not state.
func eventPayload(fields map[string]any) json.RawMessage {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func newEventID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return "evt_" + hex.EncodeToString(random[:])
}

// insertEventLogTx writes an event inside a caller-held transaction through
// the shared relation querier, for sites (the epic reconciler) that mutate
// derived state without an EventLogService in hand. Like AppendTx: kind is
// required, payload defaults to {}, occurred_at comes from the caller's
// mutation timestamp so the event shares the mutation's clock.
func insertEventLogTx(ctx context.Context, q workItemRelationQuerier, now time.Time, event Event) error {
	if event.Kind == "" {
		return errors.New("event kind is required")
	}
	if event.ID == "" {
		event.ID = newEventID()
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("event payload is not valid JSON")
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO event_log (id, occurred_at, kind, actor, task_id, session_id, run_id, change_id, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, formatTime(now.UTC()), event.Kind, event.Actor, event.TaskID, event.SessionID, event.RunID, event.ChangeID, string(payload))
	return err
}

func relationEventTaskIDTx(ctx context.Context, q workItemRelationQuerier, sourceID, targetID string) string {
	var taskID string
	err := q.QueryRowContext(ctx, `
SELECT id FROM work_items
WHERE kind = 'task' AND id IN (?, ?)
ORDER BY CASE WHEN id = ? THEN 0 ELSE 1 END
LIMIT 1`, sourceID, targetID, targetID).Scan(&taskID)
	if err != nil {
		return ""
	}
	return taskID
}

// eventLogExecer is the transactional SQL surface AppendTx writes through;
// *sql.Tx and sqlitex.Tx both satisfy it.
type eventLogExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// AppendTx inserts the event inside the caller's transaction: the append
// commits atomically with the mutation that produced it. Invariants (kind,
// defaults, payload validity) match Append. The returned event carries its id
// and occurred_at; seq is assigned on commit, so callers that need it must
// re-list. A nil service is a no-op (emission disabled).
func (s *EventLogService) AppendTx(ctx context.Context, tx eventLogExecer, event Event) (Event, error) {
	if s == nil {
		return Event{}, nil
	}
	event.Kind = strings.TrimSpace(event.Kind)
	if event.Kind == "" {
		return Event{}, errors.New("event kind is required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.now().UTC()
	}
	if event.ID == "" {
		event.ID = newEventID()
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return Event{}, fmt.Errorf("event payload is not valid JSON")
	}
	event.Payload = payload
	if _, err := tx.ExecContext(ctx, `
INSERT INTO event_log (id, occurred_at, kind, actor, task_id, session_id, run_id, change_id, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, formatTime(event.OccurredAt.UTC()), event.Kind, event.Actor, event.TaskID, event.SessionID, event.RunID, event.ChangeID, string(payload)); err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	return event, nil
}
