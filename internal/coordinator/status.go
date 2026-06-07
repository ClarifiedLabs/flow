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

// StatusKind classifies a status_log entry. The empty value defaults to
// StatusKindNote when written.
const (
	StatusKindNote     = "note"
	StatusKindProgress = "progress"
	StatusKindPlan     = "plan"
	StatusKindBlocker  = "blocker"
	StatusKindQuestion = "question"
)

func validStatusKind(kind string) bool {
	switch kind {
	case StatusKindNote, StatusKindProgress, StatusKindPlan, StatusKindBlocker, StatusKindQuestion:
		return true
	default:
		return false
	}
}

type StatusLogEntry struct {
	ID        int64     `json:"id"`
	IssueID   string    `json:"issue_id"`
	ChangeID  string    `json:"change_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Actor     string    `json:"actor"`
	Message   string    `json:"message"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

type WriteStatusInput struct {
	IssueID   string
	ChangeID  string
	SessionID string
	Actor     string
	Message   string
	Kind      string
}

type StatusService struct {
	db  *sql.DB
	now func() time.Time
}

func NewStatusService(database *sql.DB) *StatusService {
	return &StatusService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *StatusService) Write(ctx context.Context, input WriteStatusInput) (StatusLogEntry, error) {
	input.IssueID = strings.TrimSpace(input.IssueID)
	input.ChangeID = strings.TrimSpace(input.ChangeID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.Actor = strings.TrimSpace(input.Actor)
	input.Message = strings.TrimSpace(input.Message)
	if input.IssueID == "" {
		return StatusLogEntry{}, errors.New("issue id is required")
	}
	if input.Message == "" {
		return StatusLogEntry{}, errors.New("status message is required")
	}
	if input.Actor == "" {
		input.Actor = "unknown"
	}
	input.Kind = strings.TrimSpace(input.Kind)
	if input.Kind == "" {
		input.Kind = StatusKindNote
	}
	if !validStatusKind(input.Kind) {
		return StatusLogEntry{}, fmt.Errorf("invalid status kind %q", input.Kind)
	}

	nowText := formatTime(s.now().UTC())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO status_log (
	issue_id,
	change_id,
	session_id,
	actor,
	message,
	kind,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		input.IssueID,
		sqlitex.NullableNonEmptyString(input.ChangeID),
		sqlitex.NullableNonEmptyString(input.SessionID),
		input.Actor,
		input.Message,
		input.Kind,
		nowText,
	)
	if err != nil {
		return StatusLogEntry{}, fmt.Errorf("write status: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return StatusLogEntry{}, fmt.Errorf("read status id: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *StatusService) WriteSessionStatus(ctx context.Context, sessionID string, message string, actor string, kind string) (StatusLogEntry, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return StatusLogEntry{}, errors.New("session id is required")
	}

	var issueID string
	var changeID string
	if err := s.db.QueryRowContext(ctx, `
SELECT issue_id, change_id
FROM sessions
WHERE id = ?`, sessionID).Scan(&issueID, &changeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StatusLogEntry{}, errors.New("session not found")
		}
		return StatusLogEntry{}, fmt.Errorf("load session for status: %w", err)
	}

	return s.Write(ctx, WriteStatusInput{
		IssueID:   issueID,
		ChangeID:  changeID,
		SessionID: sessionID,
		Actor:     actor,
		Message:   message,
		Kind:      kind,
	})
}

func (s *StatusService) Get(ctx context.Context, id int64) (StatusLogEntry, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, issue_id, COALESCE(change_id, ''), COALESCE(session_id, ''), actor, message, kind, created_at
FROM status_log
WHERE id = ?`, id)

	return scanStatusLogEntry(row)
}

func (s *StatusService) ListForIssue(ctx context.Context, issueID string, limit int) ([]StatusLogEntry, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, errors.New("issue id is required")
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id, issue_id, COALESCE(change_id, ''), COALESCE(session_id, ''), actor, message, kind, created_at
FROM status_log
WHERE issue_id = ?
ORDER BY created_at DESC, id DESC
LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, fmt.Errorf("list issue status: %w", err)
	}
	return scanRows(rows, scanStatusLogEntry)
}

func (s *StatusService) SessionHasStatusKind(ctx context.Context, sessionID string, kind string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, errors.New("session id is required")
	}
	kind = strings.TrimSpace(kind)
	if !validStatusKind(kind) {
		return false, fmt.Errorf("invalid status kind %q", kind)
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1
	FROM status_log
	WHERE session_id = ?
		AND kind = ?
)`,
		sessionID,
		kind,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check session status kind: %w", err)
	}

	return exists == 1, nil
}

func (s *StatusService) SessionHasStatusMessage(ctx context.Context, sessionID string, kind string, message string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, errors.New("session id is required")
	}
	kind = strings.TrimSpace(kind)
	if !validStatusKind(kind) {
		return false, fmt.Errorf("invalid status kind %q", kind)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return false, errors.New("status message is required")
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1
	FROM status_log
	WHERE session_id = ?
		AND kind = ?
		AND message = ?
)`,
		sessionID,
		kind,
		message,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check session status message: %w", err)
	}

	return exists == 1, nil
}

// ListRecentByKind returns the most recent status entries whose kind is in the
// provided set, across all issues. Invalid kinds are ignored; an empty kind set
// yields no rows. It is the read primitive for surfacing blocker/question
// entries (e.g. a feedback view) and is consumed by downstream feedback work.
func (s *StatusService) ListRecentByKind(ctx context.Context, kinds []string, limit int) ([]StatusLogEntry, error) {
	if limit <= 0 {
		limit = 20
	}

	placeholders := make([]string, 0, len(kinds))
	args := make([]any, 0, len(kinds)+1)
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if !validStatusKind(kind) {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, kind)
	}
	if len(placeholders) == 0 {
		return nil, nil
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
SELECT id, issue_id, COALESCE(change_id, ''), COALESCE(session_id, ''), actor, message, kind, created_at
FROM status_log
WHERE kind IN (%s)
ORDER BY created_at DESC, id DESC
LIMIT ?`, strings.Join(placeholders, ", "))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list status by kind: %w", err)
	}
	return scanRows(rows, scanStatusLogEntry)
}

func scanStatusLogEntry(scanner issueScanner) (StatusLogEntry, error) {
	var entry StatusLogEntry
	var createdAt string
	if err := scanner.Scan(
		&entry.ID,
		&entry.IssueID,
		&entry.ChangeID,
		&entry.SessionID,
		&entry.Actor,
		&entry.Message,
		&entry.Kind,
		&createdAt,
	); err != nil {
		return StatusLogEntry{}, fmt.Errorf("scan status log entry: %w", err)
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return StatusLogEntry{}, err
	}
	entry.CreatedAt = parsedCreatedAt

	return entry, nil
}
