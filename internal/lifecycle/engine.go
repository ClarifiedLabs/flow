package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// Sentinel errors surfaced by Step. Handlers map these to HTTP status codes.
var (
	// ErrInvalidTransition means the event is not legal in the issue's current
	// phase (no matching row in the transition table).
	ErrInvalidTransition = errors.New("lifecycle: no transition for event in current phase")
	// ErrVersionConflict means the workflow_state row changed concurrently
	// between snapshot and commit; the caller may retry.
	ErrVersionConflict = errors.New("lifecycle: workflow_state version conflict")
	// ErrCascadeLimit means a transition's follow-on events exceeded the bound,
	// indicating a buggy guard/action loop.
	ErrCascadeLimit = errors.New("lifecycle: cascade depth limit exceeded")
)

const maxCascadeDepth = 8

type nonFatalFollowUpError struct {
	kind EventKind
	err  error
}

func (e *nonFatalFollowUpError) Error() string {
	return e.err.Error()
}

func (e *nonFatalFollowUpError) Unwrap() error {
	return e.err
}

// Engine owns the issue lifecycle FSM. It is the single entry point through
// which lifecycle-changing events are applied: it loads a snapshot, looks up the
// transition for (phase, event), evaluates the guard, runs the action (whose
// side effects go through Effects), records the new phase, and appends an
// append-only transition-log row — all so the workflow is explicit, durable, and
// auditable.
type Engine struct {
	db        *sql.DB
	eff       Effects
	now       func() time.Time
	deadlines DeadlineConfig
}

// DeadlineConfig bounds the otherwise-unbounded waits the engine cannot
// observe directly: a hung-but-heartbeating planning/authoring session and a
// check job that never reports. The zero value disables every deadline so
// existing deployments and tests see no new behavior; the server applies
// conservative defaults at config load.
type DeadlineConfig struct {
	// CheckPending bounds a pending check with no report. After this window the
	// check is reported blocked ("timed out"), riding the normal fix cascade.
	CheckPending time.Duration
	// AuthoringStall bounds authoring with no agent activity. After this window
	// the engine reports a non-required blocked "phase-deadline" check and a
	// blocker status entry so a human notices the wedged session.
	AuthoringStall time.Duration
}

// deadlineFor returns the configured dwell window for a phase, or 0 (disabled).
func (d DeadlineConfig) deadlineFor(phase coordinator.Phase) time.Duration {
	switch phase {
	case coordinator.PhasePlanning:
		return d.AuthoringStall
	case coordinator.PhaseAuthoring:
		return d.AuthoringStall
	default:
		return 0
	}
}

// NewEngine builds an Engine over the given database (for its own workflow_state
// / transitions / timers tables) and Effects (for all domain side effects).
func NewEngine(db *sql.DB, eff Effects) *Engine {
	return &Engine{
		db:  db,
		eff: eff,
		now: sqlitex.UTCNow,
	}
}

// SetDeadlines configures the deadline windows. It is called once at wiring
// time before the engine handles traffic; the zero value leaves all deadlines
// disabled.
func (e *Engine) SetDeadlines(d DeadlineConfig) {
	e.deadlines = d
}

// snapshot is the pre-action view of an issue the guards and actions read.
type snapshot struct {
	issueID   string
	issue     coordinator.Issue
	change    coordinator.Change
	hasChange bool
	phase     coordinator.Phase
	version   int64
}

// Step applies a single externally-originated event to the lifecycle FSM and
// returns the result. It is the entry point API handlers call.
//
// Before processing, Step journals the event into the durable event_inbox and,
// on success, confirms that row; a crash between the journal and the confirm
// leaves the row unconfirmed so the background ticker's redeliverInbox re-runs
// it (the transitions replay guard makes redelivery a no-op once the original
// cascade actually committed). This closes the durability gap where a crash
// mid-Step lost the audit row and any un-run cascade follow-ups — external events
// now get the same claim→dispatch→confirm contract the timer drain already has.
//
// Timer dispatch and inbox redelivery deliberately do NOT journal: they enter
// through stepRoot directly (they already carry their own durable bookkeeping and
// must not double-journal). Only this externally-facing entry point inserts inbox
// rows.
func (e *Engine) Step(ctx context.Context, ev Event) (StepResult, error) {
	// Resolve the issue before journaling: the inbox row's issue_id is NOT NULL,
	// and a pre-resolution failure means nothing has committed, so it is safe to
	// fail without a journal entry (the client retries).
	issueID, err := e.resolveIssueID(ctx, ev)
	if err != nil {
		return StepResult{}, err
	}
	if issueID == "" {
		return StepResult{}, fmt.Errorf("lifecycle: event %q has no issue", ev.Kind)
	}
	ev.IssueID = issueID

	// Journal the event. An inbox INSERT failure fails the Step (we cannot
	// guarantee what we cannot durably record — fail fast; the client retries).
	// insertInbox assigns and writes back an idempotency key when the event has
	// none, so the transitions replay guard can dedupe a later redelivery.
	inboxID, err := e.insertInbox(ctx, issueID, &ev)
	if err != nil {
		return StepResult{}, err
	}

	var res StepResult
	if err := e.stepRoot(ctx, ev, &res); err != nil {
		// Leave the row unconfirmed with its error recorded so redelivery retries.
		if recErr := e.recordInboxError(ctx, inboxID, err); recErr != nil {
			slog.Warn("lifecycle: record inbox error failed (non-fatal)",
				"inbox", inboxID, "error", recErr)
		}
		return StepResult{}, err
	}

	// The cascade committed. A confirm failure here must NOT fail the Step: the
	// work is durable, and redelivery will no-op via the replay guard and then
	// confirm. Mirror the post-commit deadline-arm contract: log non-fatally.
	if err := e.confirmInbox(ctx, inboxID); err != nil {
		slog.Warn("lifecycle: confirm inbox row failed (non-fatal)",
			"inbox", inboxID, "error", err)
	}
	return res, nil
}

// stepRoot runs an event's cascade from the top WITHOUT journaling it to the
// inbox. It is the shared core behind the journaling Step (external events),
// timer dispatch, and inbox redelivery — the latter two carry their own durable
// bookkeeping and must not double-journal.
func (e *Engine) stepRoot(ctx context.Context, ev Event, res *StepResult) error {
	return e.step(ctx, ev, res, 0)
}

func (e *Engine) step(ctx context.Context, ev Event, res *StepResult, depth int) error {
	if depth > maxCascadeDepth {
		return ErrCascadeLimit
	}

	issueID, err := e.resolveIssueID(ctx, ev)
	if err != nil {
		return err
	}
	if issueID == "" {
		return fmt.Errorf("lifecycle: event %q has no issue", ev.Kind)
	}

	// Optimistic-concurrency retry loop around snapshot→select→action→apply. The
	// snapshot the guard and action read is asserted against the live
	// workflow_state version inside applyTransition's write lock; if a concurrent
	// Step moved the issue in between, the apply is stale and returns
	// ErrVersionConflict, so we reload a fresh snapshot and run the whole
	// sequence again. The action therefore re-runs on a retry: this is safe
	// because actions' Effects are idempotent by convention (unique indexes on
	// the domain rows they write) and the reloaded snapshot re-evaluates guards
	// against committed state, so a retry either lands cleanly or re-declines.
	// This is the same at-least-once contract the timer drain already relies on.
	// The bound (maxApplyAttempts) keeps a pathologically contended issue from
	// spinning forever; a conflict that survives every attempt is surfaced to the
	// caller (the API maps it to 409, a timer redelivers on the next tick).
	const maxApplyAttempts = 3
	var followups []Event
	var terminal bool
	for attempt := 1; ; attempt++ {
		followups, terminal, err = e.attemptTransition(ctx, ev, issueID, res)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrVersionConflict) || attempt >= maxApplyAttempts {
			return err
		}
	}
	if terminal {
		return nil
	}

	for _, f := range followups {
		if f.IssueID == "" {
			f.IssueID = issueID
		}
		if f.Actor.Scope == "" {
			f.Actor = ev.Actor
		}
		if f.Audit.empty() {
			f.Audit = ev.Audit
		}
		if err := e.step(ctx, f, res, depth+1); err != nil {
			var followUpErr *nonFatalFollowUpError
			if errors.As(err, &followUpErr) && followUpErr.kind == f.Kind {
				if recordErr := e.recordFollowUpFailure(ctx, issueID, f, followUpErr.err); recordErr != nil {
					return fmt.Errorf("record %s follow-up failure after %w: %v", f.Kind, followUpErr.err, recordErr)
				}
				res.FollowUpFailures = append(res.FollowUpFailures, FollowUpFailure{
					EventKind: f.Kind,
					Details:   strings.TrimSpace(followUpErr.err.Error()),
				})
				continue
			}
			return err
		}
	}

	return nil
}

// attemptTransition runs one snapshot→select→action→apply attempt for an event
// and reports the follow-up events the action queued. terminal is true when the
// step is already complete and the caller must not run the follow-up cascade:
// either the event was an idempotent replay (its transition already exists) or
// no guard accepted it (a benign no-op). A returned ErrVersionConflict means the
// snapshot was superseded under the write lock; step() reloads and retries.
func (e *Engine) attemptTransition(ctx context.Context, ev Event, issueID string, res *StepResult) (followups []Event, terminal bool, err error) {
	snap, err := e.loadSnapshot(ctx, issueID)
	if err != nil {
		return nil, false, err
	}

	// Replay guard: a repeated idempotency key for this issue is a no-op, and
	// must skip the action so effects are not re-run.
	if ev.IdempotencyKey != "" {
		seen, err := e.transitionExists(ctx, issueID, ev.IdempotencyKey)
		if err != nil {
			return nil, false, err
		}
		if seen {
			if res.IssueID == "" {
				res.IssueID = issueID
				res.FromPhase = snap.phase
				res.ToPhase = snap.phase
			}
			return nil, true, nil
		}
	}

	transition, guardResult, ok, err := e.selectTransition(ctx, ev, snap)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		// The event has no candidate rows at all for this phase: a real misuse.
		if !hasCandidate(snap.phase, ev.Kind) {
			return nil, false, fmt.Errorf("%w: phase=%s event=%s", ErrInvalidTransition, snap.phase, ev.Kind)
		}
		// Candidates exist but every guard declined: a benign no-op.
		if res.IssueID == "" {
			res.IssueID = issueID
			res.FromPhase = snap.phase
			res.ToPhase = snap.phase
		}
		return nil, true, nil
	}

	if transition.Action != nil {
		followups, err = transition.Action(ctx, e, ev, snap, res)
		if err != nil {
			return nil, false, err
		}
	}

	toPhase := transition.To
	if toPhase == "" {
		toPhase, err = e.derivePhase(ctx, issueID)
		if err != nil {
			return nil, false, err
		}
	}

	applied, err := e.applyTransition(ctx, issueID, snap, ev, guardResult, toPhase, snap.version)
	if err != nil {
		// On a version conflict the phase/transition accounting below has not run
		// yet (we return before touching res here); any domain-result fields the
		// action set in res are simply re-set when step() re-runs the action on
		// the reloaded snapshot, so the retry converges to consistent output.
		return nil, false, err
	}

	if res.IssueID == "" {
		res.IssueID = issueID
	}
	if !res.Transitioned {
		res.FromPhase = snap.phase
	}
	res.ToPhase = toPhase
	if applied {
		res.Transitioned = true
	}

	// Phase-deadline scheduling: a committed transition that CHANGED the phase
	// arms (or rearms) the dwell-window timer for the target phase. This runs
	// AFTER the commit and OUTSIDE its transaction; a timer lost to a crash here
	// is acceptable because the next phase-changing transition into the same
	// phase re-enters this logic and rearms it. The dominant entry into the
	// deadline phases (planning / authoring) is a session-state event, which
	// always flows through step(), so the arm is not missed in practice.
	//
	// Because the transition is already committed, an arming failure must NOT
	// fail the Step (that would turn a committed transition into an HTTP error
	// the caller would retry against already-applied state). Per the lost-timer
	// contract above it is non-fatal: log and continue, leaving the rearm to the
	// next phase-changing transition or reconcile refresh.
	if applied && toPhase != snap.phase {
		if err := e.schedulePhaseDeadline(ctx, issueID, toPhase); err != nil {
			slog.Warn("lifecycle: arm phase deadline failed (non-fatal)",
				"issue", issueID, "phase", toPhase, "error", err)
		}
	}

	return followups, false, nil
}

func (e *Engine) recordFollowUpFailure(ctx context.Context, issueID string, ev Event, cause error) error {
	toPhase, err := e.derivePhase(ctx, issueID)
	if err != nil {
		return err
	}
	fromPhase, version, err := e.readWorkflowState(ctx, issueID)
	if err != nil {
		return err
	}
	if version < 0 {
		fromPhase = toPhase
	}
	details := strings.TrimSpace(cause.Error())
	if details == "" {
		details = "unknown error"
	}
	// expectedVersion = -1: this is a best-effort failure log against the derived
	// phase, not a guarded transition, so there is no snapshot version to assert.
	// Skipping the check lets the failure row be recorded even if a concurrent
	// Step bumped the version after the action failed.
	_, err = e.applyTransition(ctx, issueID, &snapshot{phase: fromPhase}, ev, "failed: "+details, toPhase, -1)
	return err
}

// resolveIssueID determines the issue an event targets, following the
// session/change/thread key when the event is not keyed directly by issue.
func (e *Engine) resolveIssueID(ctx context.Context, ev Event) (string, error) {
	if ev.IssueID != "" {
		return ev.IssueID, nil
	}
	switch {
	case ev.SessionID != "":
		session, err := e.eff.GetSession(ctx, ev.SessionID)
		if err != nil {
			return "", err
		}
		return session.IssueID, nil
	case ev.ChangeID != "":
		change, err := e.eff.GetChange(ctx, ev.ChangeID)
		if err != nil {
			return "", err
		}
		return change.IssueID, nil
	case ev.ThreadID != "":
		thread, err := e.eff.GetThread(ctx, ev.ThreadID)
		if err != nil {
			return "", err
		}
		return thread.IssueID, nil
	}
	return "", nil
}

// loadSnapshot reads the issue's phase/version (lazily initialising the
// workflow_state row from the derived phase if absent) and the relevant domain
// state for guards.
func (e *Engine) loadSnapshot(ctx context.Context, issueID string) (*snapshot, error) {
	phase, version, err := e.readWorkflowState(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if version < 0 {
		derived, err := e.derivePhase(ctx, issueID)
		if err != nil {
			return nil, err
		}
		if err := e.ensureWorkflowState(ctx, issueID, derived); err != nil {
			return nil, err
		}
		phase, version, err = e.readWorkflowState(ctx, issueID)
		if err != nil {
			return nil, err
		}
	}

	issue, err := e.eff.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	change, hasChange, err := e.eff.ReadyUnmergedChangeForIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}

	return &snapshot{
		issueID:   issueID,
		issue:     issue,
		change:    change,
		hasChange: hasChange,
		phase:     phase,
		version:   version,
	}, nil
}

func (e *Engine) transitionExists(ctx context.Context, issueID, idempotencyKey string) (bool, error) {
	var count int
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transitions WHERE issue_id = ? AND idempotency_key = ?`,
		issueID, idempotencyKey).Scan(&count); err != nil {
		return false, fmt.Errorf("check transition idempotency: %w", err)
	}
	return count > 0, nil
}

func (e *Engine) readWorkflowState(ctx context.Context, issueID string) (coordinator.Phase, int64, error) {
	var phase string
	var version int64
	err := e.db.QueryRowContext(ctx,
		`SELECT phase, version FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&phase, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", -1, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("load workflow_state: %w", err)
	}
	return coordinator.Phase(phase), version, nil
}

func (e *Engine) ensureWorkflowState(ctx context.Context, issueID string, phase coordinator.Phase) error {
	_, err := e.db.ExecContext(ctx, `
INSERT INTO workflow_state (issue_id, phase, version, updated_at)
VALUES (?, ?, 0, ?)
ON CONFLICT(issue_id) DO NOTHING`, issueID, string(phase), formatTime(e.now()))
	if err != nil {
		return fmt.Errorf("init workflow_state: %w", err)
	}
	return nil
}

// derivePhase computes an issue's phase via Effects reads, mirroring
// coordinator.DerivePhase exactly (including the closed-issue disambiguation:
// rejected -> rejected_closed, has a merged change -> merged_closed, otherwise
// -> abandoned). It runs both for live issues (after most transitions) and for a
// closed issue that an event lazily initialises or the reconcile ticker
// refreshes, so the closed branch must be faithful.
func (e *Engine) derivePhase(ctx context.Context, issueID string) (coordinator.Phase, error) {
	issue, err := e.eff.GetIssue(ctx, issueID)
	if err != nil {
		return "", err
	}
	if issue.ScheduleState == coordinator.ScheduleClosed {
		if issue.TriageState == coordinator.TriageRejected {
			return coordinator.PhaseRejectedClosed, nil
		}
		merged, err := e.eff.HasMergedChange(ctx, issueID)
		if err != nil {
			return "", err
		}
		if merged {
			return coordinator.PhaseMergedClosed, nil
		}
		return coordinator.PhaseAbandoned, nil
	}

	reviewState, err := e.eff.ReviewState(ctx, issueID)
	if err != nil {
		return "", err
	}
	hasChange, err := e.eff.HasReadyUnmergedChange(ctx, issueID)
	if err != nil {
		return "", err
	}
	if hasChange && reviewState == coordinator.ReviewApproved {
		return coordinator.PhaseApproved, nil
	}
	if reviewState == coordinator.ReviewChangesRequested {
		return coordinator.PhaseCritique, nil
	}
	if _, ok, err := e.eff.ActiveAuthorSessionState(ctx, issueID); err != nil {
		return "", err
	} else if ok {
		if issue.PlanMode && issue.PlanApprovedAt == nil {
			return coordinator.PhasePlanning, nil
		}
		return coordinator.PhaseAuthoring, nil
	}
	if issue.TriageState == coordinator.TriagePending {
		return coordinator.PhaseTriage, nil
	}
	if issue.TriageState != coordinator.TriageAccepted {
		return coordinator.PhaseTriage, nil
	}
	if hasChange && reviewState == coordinator.ReviewInReview {
		// Acceptance is the slice of in-review where every required critique
		// check is satisfied but a verifier check is still pending; otherwise the
		// change is still in critique.
		pending, err := e.eff.AcceptancePending(ctx, issueID)
		if err != nil {
			return "", err
		}
		if pending {
			return coordinator.PhaseAcceptance, nil
		}
		return coordinator.PhaseCritique, nil
	}
	if issue.ScheduleState == coordinator.ScheduleUpNext {
		if issue.PlanMode && issue.PlanApprovedAt == nil {
			return coordinator.PhasePlanning, nil
		}
		return coordinator.PhaseUpNext, nil
	}
	return coordinator.PhaseBacklog, nil
}

// applyTransition advances the workflow_state phase and appends the
// transition-log row. The current phase/version is read inside the BEGIN
// IMMEDIATE write lock so the read-modify-write is atomic: concurrent Steps on
// the same issue serialize on the write lock, each bumping the version
// monotonically.
//
// expectedVersion enforces optimistic concurrency. When it is >= 0 the caller is
// asserting that the guard/action ran against that exact workflow_state version;
// if the live version under the lock differs, a concurrent writer has moved the
// issue since the snapshot was taken and this apply is stale, so it is rejected
// with ErrVersionConflict (no row is written) and the caller is expected to
// reload and retry. Pass -1 to skip the comparison for callers that have no real
// snapshot to assert (recordFollowUpFailure, which deliberately fabricates a
// minimal snapshot just to log a failure against the derived phase).
//
// Returns whether a row was written (false on a duplicate idempotency key,
// treated as a successful replay no-op, or on a version conflict alongside the
// error).
func (e *Engine) applyTransition(ctx context.Context, issueID string, snap *snapshot, ev Event, guardResult string, toPhase coordinator.Phase, expectedVersion int64) (bool, error) {
	tx, err := beginImmediate(ctx, e.db)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Replay guard: a repeated idempotency key for this issue is a no-op.
	if ev.IdempotencyKey != "" {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM transitions WHERE issue_id = ? AND idempotency_key = ?`,
			issueID, ev.IdempotencyKey).Scan(&count); err != nil {
			return false, fmt.Errorf("check transition idempotency: %w", err)
		}
		if count > 0 {
			return false, nil
		}
	}

	// Read the live phase/version under the write lock; the workflow_state row
	// is guaranteed to exist (loadSnapshot lazily initialises it).
	fromPhase := string(snap.phase)
	var liveVersion int64
	haveRow := true
	if err := tx.QueryRowContext(ctx,
		`SELECT phase, version FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&fromPhase, &liveVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			haveRow = false
		} else {
			return false, fmt.Errorf("load workflow_state for transition: %w", err)
		}
	}

	// Optimistic concurrency check: if the caller asserted a specific version and
	// the live version under the lock no longer matches, a concurrent Step moved
	// the issue since the snapshot was taken. The guard/action this apply belongs
	// to ran against stale state, so reject it; step() reloads and retries.
	if expectedVersion >= 0 && haveRow && liveVersion != expectedVersion {
		return false, ErrVersionConflict
	}

	now := formatTime(e.now())
	if haveRow {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_state
SET phase = ?, version = ?, updated_at = ?
WHERE issue_id = ?`, string(toPhase), liveVersion+1, now, issueID); err != nil {
			return false, fmt.Errorf("update workflow_state: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_state (issue_id, phase, version, updated_at)
VALUES (?, ?, 0, ?)`, issueID, string(toPhase), now); err != nil {
		return false, fmt.Errorf("insert workflow_state: %w", err)
	}

	payload, err := transitionPayloadJSON(ev)
	if err != nil {
		return false, fmt.Errorf("marshal event payload: %w", err)
	}
	var idempotencyKey any
	if ev.IdempotencyKey != "" {
		idempotencyKey = ev.IdempotencyKey
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO transitions
	(issue_id, from_phase, event_kind, payload_json, guard_result, to_phase, actor, idempotency_key, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issueID, fromPhase, string(ev.Kind), string(payload), guardResult,
		string(toPhase), ev.Actor.Actor(), idempotencyKey, now); err != nil {
		return false, fmt.Errorf("append transition: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func transitionPayloadJSON(ev Event) ([]byte, error) {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return nil, err
	}
	if ev.Audit.empty() {
		return payload, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	audit, err := json.Marshal(ev.Audit)
	if err != nil {
		return nil, err
	}
	fields["audit"] = audit
	return json.Marshal(fields)
}
