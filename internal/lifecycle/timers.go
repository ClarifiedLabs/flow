package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// Tick is one iteration of the background lifecycle ticker: it drains any due
// durable timers and then runs crash recovery. It is safe to call repeatedly and
// requires no inbound API traffic, closing the durability gap where recovery
// previously fired only when a request happened to pass through a handler. A
// failing drain must not suppress recovery (or vice versa), so both halves
// always run and their errors are joined.
func (e *Engine) Tick(ctx context.Context) error {
	_, drainErr := e.DrainDueTimers(ctx)
	inboxErr := e.redeliverInbox(ctx)
	_, recErr := e.RunRecovery(ctx)
	return errors.Join(drainErr, inboxErr, recErr)
}

// RunRecovery reconciles crashed author sessions (releasing expired leases,
// marking sessions crashed, revoking their tokens, and re-enqueuing work),
// restores missing automated check jobs for pending checks, and then refreshes
// any lifecycle phases that recovery may have invalidated. Each recovery step
// runs even when an earlier one fails, so one wedged subsystem cannot starve
// the others; the errors are joined. Returns the number of recovered sessions
// plus check jobs re-enqueued.
func (e *Engine) RunRecovery(ctx context.Context) (int, error) {
	var errs error
	recovered, err := e.eff.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	checksRecovered, pendingCheckTimeouts, err := e.eff.RecoverPendingCheckJobs(ctx)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	// Arm a check timeout for any pending check the scan surfaced that lacks one,
	// closing the gap where a review round scheduled outside the engine (Mode-B
	// completion review) was never timeout-armed. Run even when the scan errored:
	// the partial pending list is still worth arming, and arming is deduped so a
	// normal round (already armed at ready-time) is a no-op.
	if err := e.armRecoveredCheckTimeouts(ctx, pendingCheckTimeouts); err != nil {
		errs = errors.Join(errs, err)
	}
	mergesRecovered, err := e.eff.RecoverPendingMerges(ctx)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	if err := e.refreshSessionPhases(ctx); err != nil {
		errs = errors.Join(errs, err)
	}
	return recovered + checksRecovered + mergesRecovered, errs
}

// refreshSessionPhases re-derives the phase for issues currently recorded in a
// session-derived phase (planning / authoring). After a crash these phases
// go stale — the session is gone but the column still claims work is in flight —
// so a refresh keeps the explicit phase (and therefore the board/timeline)
// accurate without waiting for the next inbound event.
func (e *Engine) refreshSessionPhases(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx,
		`SELECT issue_id FROM workflow_state WHERE phase IN (?, ?)`,
		string(coordinator.PhasePlanning), string(coordinator.PhaseAuthoring))
	if err != nil {
		return fmt.Errorf("select session-phase issues: %w", err)
	}
	var issueIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan session-phase issue: %w", err)
		}
		issueIDs = append(issueIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate session-phase issues: %w", err)
	}

	var errs error
	for _, issueID := range issueIDs {
		if err := e.reconcilePhase(ctx, issueID); err != nil {
			errs = errors.Join(errs, fmt.Errorf("reconcile phase for %s: %w", issueID, err))
		}
	}
	return errs
}

// reconcilePhase re-derives an issue's phase and, if it changed, records the move
// as a reconcile transition (no external effects run).
func (e *Engine) reconcilePhase(ctx context.Context, issueID string) error {
	snap, err := e.loadSnapshot(ctx, issueID)
	if err != nil {
		return err
	}
	toPhase, err := e.derivePhase(ctx, issueID)
	if err != nil {
		return err
	}
	if toPhase == snap.phase {
		return nil
	}
	// Assert the snapshot version: a conflict means a real Step transitioned this
	// issue between our load and apply. Reconcile is a best-effort background
	// refresh, and that concurrent Step already recorded an authoritative move, so
	// a conflict is a benign skip — the next tick re-derives from committed state
	// if the phase is still stale. We deliberately do not retry here (unlike
	// step()) to avoid contending with live traffic on a hot issue.
	_, err = e.applyTransition(ctx, issueID, snap, Event{Kind: EventReconcile}, "reconcile", toPhase, snap.version)
	if errors.Is(err, ErrVersionConflict) {
		return nil
	}
	return err
}

// ScheduleTimer records a durable timer that fires the given event at fireAt. The
// background ticker drains it via DrainDueTimers. Returns the timer id.
func (e *Engine) ScheduleTimer(ctx context.Context, issueID string, kind EventKind, fireAt time.Time, payload EventPayload) (string, error) {
	id, err := newTimerID()
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal timer payload: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, `
INSERT INTO timers (id, issue_id, fire_at, kind, payload_json, fired_at)
VALUES (?, ?, ?, ?, ?, NULL)`, id, issueID, formatTime(fireAt.UTC()), string(kind), string(payloadJSON)); err != nil {
		return "", fmt.Errorf("schedule timer: %w", err)
	}
	return id, nil
}

// DrainDueTimers fires every timer whose fire_at has passed and whose event has
// not yet committed. Delivery is at-least-once: a timer is claimed (fired_at
// stamped, attempts bumped) before dispatch, but only confirmed (dispatched_at
// set) after its event commits through Step, so a crash or error between claim
// and confirm redelivers on the next drain. Redelivery cannot double-run the
// event because every dispatch carries the deterministic idempotency key
// "timer:<id>", which the engine's transition-log replay guard dedupes. Timers
// are fault-isolated: one failing timer is recorded (attempts/last_error) and
// retried next tick while the rest of the batch still drains. Returns the
// number whose events committed.
func (e *Engine) DrainDueTimers(ctx context.Context) (int, error) {
	now := formatTime(e.now())
	rows, err := e.db.QueryContext(ctx, `
SELECT id, issue_id, kind, payload_json
FROM timers
WHERE dispatched_at IS NULL AND fire_at <= ?
ORDER BY fire_at`, now)
	if err != nil {
		return 0, fmt.Errorf("select due timers: %w", err)
	}
	type dueTimer struct {
		id, issueID, kind, payload string
	}
	var due []dueTimer
	for rows.Next() {
		var t dueTimer
		if err := rows.Scan(&t.id, &t.issueID, &t.kind, &t.payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due timer: %w", err)
		}
		due = append(due, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate due timers: %w", err)
	}

	fired := 0
	var errs error
	for _, t := range due {
		// Claim: stamp the attempt. This is a soft lease, not the completion
		// marker — a claimed timer whose dispatch never confirms is redelivered.
		res, err := e.db.ExecContext(ctx,
			`UPDATE timers SET attempts = attempts + 1, fired_at = ? WHERE id = ? AND dispatched_at IS NULL`, now, t.id)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("claim timer %s: %w", t.id, err))
			continue
		}
		affected, err := res.RowsAffected()
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("claim timer %s: %w", t.id, err))
			continue
		}
		if affected == 0 {
			continue
		}

		if err := e.dispatchTimer(ctx, t.id, t.issueID, EventKind(t.kind), t.payload); err != nil {
			errs = errors.Join(errs, fmt.Errorf("dispatch timer %s (%s): %w", t.id, t.kind, err))
			// A poison timer (unretryable failure) is parked — confirmed with
			// its error preserved — because redelivery can never succeed and
			// would otherwise retry and spam the log every tick forever.
			var unretryable *unretryableTimerError
			park := errors.As(err, &unretryable)
			query := `UPDATE timers SET last_error = ? WHERE id = ?`
			args := []any{truncateError(err), t.id}
			if park {
				query = `UPDATE timers SET last_error = ?, dispatched_at = ? WHERE id = ?`
				args = []any{truncateError(err), formatTime(e.now()), t.id}
			}
			if _, recordErr := e.db.ExecContext(ctx, query, args...); recordErr != nil {
				errs = errors.Join(errs, fmt.Errorf("record timer %s error: %w", t.id, recordErr))
			}
			continue
		}
		if _, err := e.db.ExecContext(ctx,
			`UPDATE timers SET dispatched_at = ?, last_error = '' WHERE id = ?`, formatTime(e.now()), t.id); err != nil {
			errs = errors.Join(errs, fmt.Errorf("confirm timer %s: %w", t.id, err))
			continue
		}
		fired++
	}
	return fired, errs
}

// dispatchTimer applies one claimed timer's event through Step. A stale timer —
// its issue has since moved to a phase where the event has no candidate edge —
// is treated as successfully dispatched so it confirms instead of retrying
// forever (the guard-declined case is already a benign no-op inside Step).
func (e *Engine) dispatchTimer(ctx context.Context, timerID, issueID string, kind EventKind, payloadJSON string) error {
	var payload EventPayload
	if payloadJSON != "" {
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return &unretryableTimerError{err: fmt.Errorf("unmarshal timer payload: %w", err)}
		}
	}
	ev := Event{
		Kind:           kind,
		IssueID:        issueID,
		Payload:        payload,
		IdempotencyKey: "timer:" + timerID,
	}
	// Route through stepRoot, not Step: a timer carries its own durable
	// claim/confirm bookkeeping in the timers table, so journaling it again to the
	// event_inbox would double-journal the same delivery.
	var res StepResult
	err := e.stepRoot(ctx, ev, &res)
	if errors.Is(err, ErrInvalidTransition) {
		return nil
	}
	// A non-fatal action failure (e.g. a transient auto-merge error that
	// already scheduled its own retry timer) is recorded in the transition log
	// and treated as delivered — redelivering this timer would only duplicate
	// the action's follow-up scheduling.
	var followUpErr *nonFatalFollowUpError
	if errors.As(err, &followUpErr) && followUpErr.kind == kind {
		if recordErr := e.recordFollowUpFailure(ctx, issueID, ev, followUpErr.err); recordErr != nil {
			return fmt.Errorf("record %s timer failure after %w: %v", kind, followUpErr.err, recordErr)
		}
		return nil
	}
	return err
}

// unretryableTimerError marks a timer dispatch failure that no redelivery can
// fix (e.g. a payload that does not parse); the drain parks such timers.
type unretryableTimerError struct {
	err error
}

func (e *unretryableTimerError) Error() string {
	return e.err.Error()
}

func (e *unretryableTimerError) Unwrap() error {
	return e.err
}

// truncateError renders an error for the timers.last_error column, bounded so a
// pathological message cannot bloat the row.
func truncateError(err error) string {
	msg := strings.TrimSpace(err.Error())
	const max = 1024
	if len(msg) > max {
		msg = msg[:max]
	}
	return msg
}

// schedulePhaseDeadline arms a durable EventPhaseDeadline timer for the target
// phase when that phase has a configured nonzero dwell window. It dedupes: a
// phase that already has an undispatched phase_deadline timer is left alone so
// repeated active-agent phase churn cannot spawn a
// storm of timers. The dispatching timer (still unconfirmed while its action
// reschedules) is excluded so a reschedule is not mistaken for a duplicate.
func (e *Engine) schedulePhaseDeadline(ctx context.Context, issueID string, phase coordinator.Phase) error {
	window := e.deadlines.deadlineFor(phase)
	if window <= 0 {
		return nil
	}
	pending, err := e.hasPendingTimer(ctx, issueID, EventPhaseDeadline, "")
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	_, err = e.ScheduleTimer(ctx, issueID, EventPhaseDeadline, e.now().Add(window), EventPayload{
		DeadlinePhase: phase,
	})
	return err
}

// armRecoveredCheckTimeouts arms a check timeout for each pending automated
// check the recovery scan surfaced that does not already have one. It closes the
// gap where a review round scheduled OUTSIDE the engine — a Mode-B completion-
// assessment review dispatched directly by the coordinator's crash reconcile,
// which cannot reach the engine's scheduleCheckTimeouts — left its reviewer check
// un-timeout-armed, so a reviewer that never reports could park the change
// indefinitely. Arming is deduped per (issue, name, head): a normal round
// already armed its timeout at ready-time, so the same surfaced check is a
// no-op, and the SAME EventCheckTimeout the normal path arms is used. It is a
// no-op when the deadline is disabled. Errors are joined so one bad check cannot
// suppress arming the rest.
func (e *Engine) armRecoveredCheckTimeouts(ctx context.Context, pending []coordinator.PendingCheckTimeout) error {
	if e.deadlines.CheckPending <= 0 {
		return nil
	}
	var errs error
	for _, p := range pending {
		issueID := strings.TrimSpace(p.IssueID)
		if issueID == "" {
			continue
		}
		headSHA := strings.TrimSpace(p.HeadSHA)
		var missing []string
		for _, name := range p.CheckNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			armed, err := e.hasPendingCheckTimeout(ctx, issueID, name, headSHA)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if !armed {
				missing = append(missing, name)
			}
		}
		if err := e.scheduleCheckTimeouts(ctx, issueID, headSHA, missing); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// hasPendingCheckTimeout reports whether an undispatched EventCheckTimeout is
// already armed for this (issue, check name, head). Check timeouts carry the
// name and head in their payload (a single issue can have one per check), so the
// match parses the payload rather than keying on kind alone.
func (e *Engine) hasPendingCheckTimeout(ctx context.Context, issueID, name, headSHA string) (bool, error) {
	rows, err := e.db.QueryContext(ctx, `
SELECT payload_json FROM timers
WHERE issue_id = ? AND kind = ? AND dispatched_at IS NULL`,
		issueID, string(EventCheckTimeout))
	if err != nil {
		return false, fmt.Errorf("query pending check timeouts: %w", err)
	}
	defer rows.Close()

	name = strings.TrimSpace(name)
	headSHA = strings.TrimSpace(headSHA)
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return false, fmt.Errorf("scan pending check timeout: %w", err)
		}
		var payload EventPayload
		if payloadJSON != "" {
			if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
				return false, fmt.Errorf("decode pending check timeout payload: %w", err)
			}
		}
		if strings.TrimSpace(payload.Name) == name && strings.TrimSpace(payload.HeadSHA) == headSHA {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate pending check timeouts: %w", err)
	}
	return false, nil
}

// hasPendingTimer reports whether an undispatched timer of the given kind is
// already scheduled for the issue, ignoring excludeID (the timer currently
// being dispatched is unconfirmed while its action runs, and must not count
// as its own successor).
func (e *Engine) hasPendingTimer(ctx context.Context, issueID string, kind EventKind, excludeID string) (bool, error) {
	var count int
	if err := e.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM timers
WHERE issue_id = ? AND kind = ? AND dispatched_at IS NULL AND id != ?`,
		issueID, string(kind), excludeID).Scan(&count); err != nil {
		return false, fmt.Errorf("check pending timers: %w", err)
	}
	return count > 0, nil
}

func newTimerID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate timer id: %w", err)
	}
	return "tm-" + hex.EncodeToString(buf[:]), nil
}
