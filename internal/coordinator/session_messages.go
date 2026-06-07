package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

type SessionMessageState string

const (
	SessionMessagePending   SessionMessageState = "pending"
	SessionMessageDelivered SessionMessageState = "delivered"
)

type SessionMessage struct {
	ID            string              `json:"id"`
	SessionID     string              `json:"session_id"`
	StatusLogID   *int64              `json:"status_log_id,omitempty"`
	Actor         string              `json:"actor"`
	Body          string              `json:"body"`
	State         SessionMessageState `json:"state"`
	CreatedAt     time.Time           `json:"created_at"`
	DeliveredAt   *time.Time          `json:"delivered_at,omitempty"`
	DeliveryError string              `json:"delivery_error,omitempty"`
}

type EnqueueSessionMessageInput struct {
	SessionID   string
	StatusLogID *int64
	Actor       string
	Body        string
}

type ListPendingSessionMessagesInput struct {
	SessionID string
	LeaseID   string
	Limit     int
}

type MarkSessionMessageDeliveredInput struct {
	SessionID string
	MessageID string
	LeaseID   string
}

type ReplyToIssueInput struct {
	IssueID     string
	StatusLogID *int64
	Actor       string
	Body        string
}

func (s *SessionService) EnqueueSessionMessage(ctx context.Context, input EnqueueSessionMessageInput) (SessionMessage, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return SessionMessage{}, errors.New("session id is required")
	}
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return SessionMessage{}, err
	}
	actor := strings.TrimSpace(input.Actor)
	if actor == "" {
		actor = "human"
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return SessionMessage{}, errors.New("message body is required")
	}
	id, err := randomPrefixedID("sm")
	if err != nil {
		return SessionMessage{}, err
	}
	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO session_messages (id, session_id, status_log_id, actor, body, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id,
		sessionID,
		nullableInt64Value(input.StatusLogID),
		actor,
		body,
		string(SessionMessagePending),
		nowText,
	); err != nil {
		return SessionMessage{}, fmt.Errorf("enqueue session message: %w", err)
	}

	return s.GetSessionMessage(ctx, id)
}

func (s *SessionService) ReplyToIssue(ctx context.Context, input ReplyToIssueInput) (SessionMessage, bool, error) {
	issueID := strings.TrimSpace(input.IssueID)
	if issueID == "" {
		return SessionMessage{}, false, errors.New("issue id is required")
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return SessionMessage{}, false, errors.New("reply message is required")
	}
	session, ok, err := s.ActiveAuthorSessionForIssue(ctx, issueID)
	if err != nil {
		return SessionMessage{}, false, err
	}
	if !ok {
		return SessionMessage{}, false, nil
	}
	message, err := s.EnqueueSessionMessage(ctx, EnqueueSessionMessageInput{
		SessionID:   session.ID,
		StatusLogID: input.StatusLogID,
		Actor:       input.Actor,
		Body:        body,
	})
	if err != nil {
		return SessionMessage{}, false, err
	}

	return message, true, nil
}

func (s *SessionService) ListPendingSessionMessages(ctx context.Context, input ListPendingSessionMessagesInput) ([]SessionMessage, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return nil, errors.New("session id is required")
	}
	if err := s.validateLiveSessionLease(ctx, sessionID, input.LeaseID); err != nil {
		return nil, err
	}
	limit := input.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, status_log_id, actor, body, state, created_at, delivered_at, delivery_error
FROM session_messages
WHERE session_id = ?
	AND state = ?
ORDER BY created_at, id
LIMIT ?`,
		sessionID,
		string(SessionMessagePending),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending session messages: %w", err)
	}

	return scanRows(rows, scanSessionMessage)
}

func (s *SessionService) MarkSessionMessageDelivered(ctx context.Context, input MarkSessionMessageDeliveredInput) (SessionMessage, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	messageID := strings.TrimSpace(input.MessageID)
	if sessionID == "" || messageID == "" {
		return SessionMessage{}, errors.New("session id and message id are required")
	}
	if err := s.validateLiveSessionLease(ctx, sessionID, input.LeaseID); err != nil {
		return SessionMessage{}, err
	}
	nowText := formatTime(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionMessage{}, fmt.Errorf("begin mark session message delivered transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE session_messages
SET state = ?,
	delivered_at = COALESCE(delivered_at, ?),
	delivery_error = ''
WHERE id = ?
	AND session_id = ?
	AND state = ?`,
		string(SessionMessageDelivered),
		nowText,
		messageID,
		sessionID,
		string(SessionMessagePending),
	)
	if err != nil {
		return SessionMessage{}, fmt.Errorf("mark session message delivered: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SessionMessage{}, fmt.Errorf("read delivered message rows affected: %w", err)
	}
	if rows == 0 {
		return SessionMessage{}, sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?
WHERE id = ?
	AND runtime_state = ?`,
		string(SessionWorking),
		nowText,
		sessionID,
		string(SessionWaiting),
	); err != nil {
		return SessionMessage{}, fmt.Errorf("mark session working after message delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionMessage{}, fmt.Errorf("commit mark session message delivered transaction: %w", err)
	}

	return s.GetSessionMessage(ctx, messageID)
}

func (s *SessionService) GetSessionMessage(ctx context.Context, id string) (SessionMessage, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, session_id, status_log_id, actor, body, state, created_at, delivered_at, delivery_error
FROM session_messages
WHERE id = ?`, strings.TrimSpace(id))

	return scanSessionMessage(row)
}

func (s *SessionService) validateLiveSessionLease(ctx context.Context, sessionID string, leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return errors.New("lease id is required")
	}
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.LeaseID != leaseID {
		return errors.New("lease does not belong to session")
	}
	if session.Role != flowworker.RoleAuthor && session.Role != flowworker.RoleReviewer && session.Role != flowworker.RoleVerifier {
		return errors.New("session messages require an agent session")
	}
	if session.RuntimeState != SessionStarting && session.RuntimeState != SessionWorking && session.RuntimeState != SessionWaiting {
		return errors.New("session is not live")
	}

	return s.validateLiveJobLease(ctx, session.JobID, leaseID)
}

func scanSessionMessage(row issueScanner) (SessionMessage, error) {
	var message SessionMessage
	var statusLogID sql.NullInt64
	var deliveredAt sql.NullString
	var createdAtText string
	var state string
	if err := row.Scan(
		&message.ID,
		&message.SessionID,
		&statusLogID,
		&message.Actor,
		&message.Body,
		&state,
		&createdAtText,
		&deliveredAt,
		&message.DeliveryError,
	); err != nil {
		return SessionMessage{}, err
	}
	message.State = SessionMessageState(state)
	if statusLogID.Valid {
		value := statusLogID.Int64
		message.StatusLogID = &value
	}
	createdAt, err := parseTime(createdAtText)
	if err != nil {
		return SessionMessage{}, err
	}
	message.CreatedAt = createdAt
	if deliveredAt.Valid {
		parsed, err := parseTime(deliveredAt.String)
		if err != nil {
			return SessionMessage{}, err
		}
		message.DeliveredAt = &parsed
	}

	return message, nil
}
