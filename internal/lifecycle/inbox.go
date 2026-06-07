package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// inboxGrace is the minimum age an unconfirmed inbox row must reach before
// redelivery considers it. The window keeps redeliverInbox from racing a Step
// that is still in flight (its row is inserted before its cascade commits and
// confirms): only rows demonstrably older than an in-progress request — i.e.
// crashed Steps — are re-run.
const inboxGrace = 30 * time.Second

// maxInboxAttempts caps how many times a single inbox row is redelivered before
// it is parked (confirmed with its error retained). It mirrors the poison-timer
// pattern: a row that can never succeed (e.g. corrupt event_json) must not spam
// the log forever.
const maxInboxAttempts = 10

// storedEvent is the JSON-serialisable mirror of Event written into
// event_inbox.event_json. Event itself carries no JSON tags (its fields are read
// directly by the engine), so a dedicated mirror keeps the on-disk shape stable
// and explicit. Every field a transition reads — kind, the routing ids, the
// actor principal, audit provenance, and the payload — round-trips through it.
type storedEvent struct {
	Kind           EventKind             `json:"kind"`
	IssueID        string                `json:"issue_id,omitempty"`
	ChangeID       string                `json:"change_id,omitempty"`
	ThreadID       string                `json:"thread_id,omitempty"`
	SessionID      string                `json:"session_id,omitempty"`
	Actor          coordinator.Principal `json:"actor"`
	Audit          EventAudit            `json:"audit,omitempty"`
	IdempotencyKey string                `json:"idempotency_key,omitempty"`
	Payload        EventPayload          `json:"payload"`
}

func toStoredEvent(ev Event) storedEvent {
	return storedEvent(ev)
}

func (s storedEvent) toEvent() Event {
	return Event(s)
}

// insertInbox journals an external event into event_inbox before its cascade
// runs, returning the new row id. When the event carries no idempotency key, one
// is assigned ("inbox:<id>") and written back onto *ev so the cascade's
// transition row — and therefore the replay guard — uses the same key, making a
// later redelivery a dedup no-op. The caller must have resolved a non-empty
// issueID (the column is NOT NULL).
func (e *Engine) insertInbox(ctx context.Context, issueID string, ev *Event) (string, error) {
	id, err := newInboxID()
	if err != nil {
		return "", err
	}
	if ev.IdempotencyKey == "" {
		ev.IdempotencyKey = "inbox:" + id
	}
	raw, err := json.Marshal(toStoredEvent(*ev))
	if err != nil {
		return "", fmt.Errorf("marshal inbox event: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, `
INSERT INTO event_inbox (id, issue_id, event_json, idempotency_key, created_at, attempts, last_error, confirmed_at)
VALUES (?, ?, ?, ?, ?, 0, '', NULL)`,
		id, issueID, string(raw), ev.IdempotencyKey, formatTime(e.now())); err != nil {
		return "", fmt.Errorf("insert inbox row: %w", err)
	}
	return id, nil
}

// confirmInbox stamps confirmed_at and clears last_error after a cascade
// committed. A confirm of an already-confirmed row is a harmless no-op.
func (e *Engine) confirmInbox(ctx context.Context, id string) error {
	if _, err := e.db.ExecContext(ctx,
		`UPDATE event_inbox SET confirmed_at = ?, last_error = '' WHERE id = ?`,
		formatTime(e.now()), id); err != nil {
		return fmt.Errorf("confirm inbox row %s: %w", id, err)
	}
	return nil
}

// recordInboxError records a failed delivery: it bumps attempts and stores the
// error, leaving the row unconfirmed so redelivery retries it.
func (e *Engine) recordInboxError(ctx context.Context, id string, cause error) error {
	if _, err := e.db.ExecContext(ctx,
		`UPDATE event_inbox SET attempts = attempts + 1, last_error = ? WHERE id = ?`,
		truncateError(cause), id); err != nil {
		return fmt.Errorf("record inbox error %s: %w", id, err)
	}
	return nil
}

// redeliverInbox re-runs every unconfirmed inbox row older than the grace window
// (crashed Steps whose journal survived but whose confirm did not). Each row is
// re-Stepped through stepRoot — NOT Step — so it is not re-journaled; redelivery
// is safe because effects are idempotent by convention and the transitions replay
// guard skips an event whose transition already committed (the same at-least-once
// contract DrainDueTimers documents). A row whose redelivery still fails has its
// attempts bumped and is retried next tick, until maxInboxAttempts, after which it
// is parked — confirmed with last_error retained — exactly like a poison timer so
// an unparseable event cannot spin forever. One failing row is fault-isolated:
// its error is recorded and joined while the rest of the batch still drains.
func (e *Engine) redeliverInbox(ctx context.Context) error {
	cutoff := formatTime(e.now().Add(-inboxGrace))
	rows, err := e.db.QueryContext(ctx, `
SELECT id, event_json, attempts
FROM event_inbox
WHERE confirmed_at IS NULL AND created_at <= ?
ORDER BY created_at`, cutoff)
	if err != nil {
		return fmt.Errorf("select pending inbox rows: %w", err)
	}
	type pendingRow struct {
		id        string
		eventJSON string
		attempts  int
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.id, &p.eventJSON, &p.attempts); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending inbox row: %w", err)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending inbox rows: %w", err)
	}

	var errs error
	for _, p := range pending {
		err := e.redeliverOne(ctx, p.eventJSON)
		if err == nil {
			if confErr := e.confirmInbox(ctx, p.id); confErr != nil {
				errs = errors.Join(errs, confErr)
			}
			continue
		}

		errs = errors.Join(errs, fmt.Errorf("redeliver inbox %s: %w", p.id, err))
		// A row at or past the attempt cap (this failed delivery is its
		// attempts+1-th) can never make progress, so park it: confirm with the
		// error preserved, mirroring the poison-timer pattern.
		if p.attempts+1 >= maxInboxAttempts {
			if _, parkErr := e.db.ExecContext(ctx,
				`UPDATE event_inbox SET attempts = attempts + 1, last_error = ?, confirmed_at = ? WHERE id = ?`,
				truncateError(err), formatTime(e.now()), p.id); parkErr != nil {
				errs = errors.Join(errs, fmt.Errorf("park inbox %s: %w", p.id, parkErr))
			}
			continue
		}
		if recErr := e.recordInboxError(ctx, p.id, err); recErr != nil {
			errs = errors.Join(errs, recErr)
		}
	}
	return errs
}

// redeliverOne unmarshals a stored event and re-runs it through stepRoot. A
// stale event whose phase no longer has a candidate edge is a benign no-op (the
// guard-declined / invalid-transition case), so it is treated as delivered and
// the row confirms instead of retrying forever.
func (e *Engine) redeliverOne(ctx context.Context, eventJSON string) error {
	var stored storedEvent
	if err := json.Unmarshal([]byte(eventJSON), &stored); err != nil {
		return fmt.Errorf("unmarshal inbox event: %w", err)
	}
	var res StepResult
	err := e.stepRoot(ctx, stored.toEvent(), &res)
	if errors.Is(err, ErrInvalidTransition) {
		return nil
	}
	return err
}

func newInboxID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate inbox id: %w", err)
	}
	return "in-" + hex.EncodeToString(buf[:]), nil
}
