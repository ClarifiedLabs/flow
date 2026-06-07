package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type GitEventSource string

const (
	GitEventSourceAPI   GitEventSource = "api"
	GitEventSourceSpool GitEventSource = "spool"
)

type GitEvent struct {
	ID         int64
	OldSHA     string
	NewSHA     string
	Ref        string
	Actor      string
	ObservedAt time.Time
	ReceivedAt time.Time
	Source     GitEventSource
}

type RecordGitEventResult struct {
	Event    GitEvent
	Inserted bool
}

type GitEventService struct {
	db  *sql.DB
	now func() time.Time
}

func NewGitEventService(database *sql.DB) *GitEventService {
	return &GitEventService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *GitEventService) Record(ctx context.Context, event GitEvent, source GitEventSource) (RecordGitEventResult, error) {
	event, err := normalizeGitEvent(event, source, s.now().UTC())
	if err != nil {
		return RecordGitEventResult{}, err
	}
	eventHash := gitEventHash(event)
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO git_events (
	event_hash,
	old_sha,
	new_sha,
	ref,
	actor,
	observed_at,
	received_at,
	source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventHash,
		event.OldSHA,
		event.NewSHA,
		event.Ref,
		event.Actor,
		formatTime(event.ObservedAt.UTC()),
		formatTime(event.ReceivedAt.UTC()),
		string(event.Source),
	)
	if err != nil {
		return RecordGitEventResult{}, fmt.Errorf("record git event: %w", err)
	}

	inserted := false
	if rowsAffected, err := result.RowsAffected(); err == nil {
		inserted = rowsAffected > 0
	}

	stored, err := s.GetByHash(ctx, eventHash)
	if err != nil {
		return RecordGitEventResult{}, err
	}

	return RecordGitEventResult{Event: stored, Inserted: inserted}, nil
}

func (s *GitEventService) DrainSpooled(ctx context.Context, exchangeRepoPath string) (int, error) {
	events, err := flowgit.ReadSpooledEvents(exchangeRepoPath)
	if err != nil {
		return 0, err
	}

	inserted := 0
	for _, event := range events {
		result, err := s.Record(ctx, GitEvent{
			OldSHA:     event.OldSHA,
			NewSHA:     event.NewSHA,
			Ref:        event.Ref,
			Actor:      event.Actor,
			ObservedAt: event.ObservedAt,
		}, GitEventSourceSpool)
		if err != nil {
			return inserted, err
		}
		if result.Inserted {
			inserted++
		}
	}

	return inserted, nil
}

func (s *GitEventService) GetByHash(ctx context.Context, eventHash string) (GitEvent, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, old_sha, new_sha, ref, actor, observed_at, received_at, source
FROM git_events
WHERE event_hash = ?`, eventHash)

	return scanGitEvent(row)
}

func (s *GitEventService) List(ctx context.Context) ([]GitEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, old_sha, new_sha, ref, actor, observed_at, received_at, source
FROM git_events
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list git events: %w", err)
	}
	defer rows.Close()

	var events []GitEvent
	for rows.Next() {
		event, err := scanGitEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate git events: %w", err)
	}

	return events, nil
}

func normalizeGitEvent(event GitEvent, source GitEventSource, now time.Time) (GitEvent, error) {
	event.OldSHA = strings.TrimSpace(event.OldSHA)
	event.NewSHA = strings.TrimSpace(event.NewSHA)
	event.Ref = strings.TrimSpace(event.Ref)
	event.Actor = strings.TrimSpace(event.Actor)
	if event.OldSHA == "" || event.NewSHA == "" || event.Ref == "" {
		return GitEvent{}, errors.New("git event old_sha, new_sha, and ref are required")
	}
	if event.Actor == "" {
		event.Actor = "unknown"
	}
	switch source {
	case GitEventSourceAPI, GitEventSourceSpool:
		event.Source = source
	default:
		return GitEvent{}, fmt.Errorf("invalid git event source: %s", source)
	}
	if event.ObservedAt.IsZero() {
		event.ObservedAt = now
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = now
	}

	return event, nil
}

func gitEventHash(event GitEvent) string {
	hasher := sha256.New()
	for _, value := range []string{event.OldSHA, event.NewSHA, event.Ref, event.Actor} {
		hasher.Write([]byte(value))
		hasher.Write([]byte{0})
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

func scanGitEvent(scanner issueScanner) (GitEvent, error) {
	var event GitEvent
	var observedAt string
	var receivedAt string
	var source string
	if err := scanner.Scan(
		&event.ID,
		&event.OldSHA,
		&event.NewSHA,
		&event.Ref,
		&event.Actor,
		&observedAt,
		&receivedAt,
		&source,
	); err != nil {
		return GitEvent{}, fmt.Errorf("scan git event: %w", err)
	}

	parsedObservedAt, err := parseTime(observedAt)
	if err != nil {
		return GitEvent{}, err
	}
	parsedReceivedAt, err := parseTime(receivedAt)
	if err != nil {
		return GitEvent{}, err
	}
	event.ObservedAt = parsedObservedAt
	event.ReceivedAt = parsedReceivedAt
	event.Source = GitEventSource(source)
	return event, nil
}
