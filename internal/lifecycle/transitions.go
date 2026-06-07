package lifecycle

import (
	"context"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// Transition is one edge of the issue lifecycle FSM. The table below IS the
// canonical, reviewable workflow specification the system used to express only
// implicitly across HTTP handlers.
//
// From == "" matches any phase. To == "" means the post-action phase is derived
// from the resulting issue state (engine.derivePhase) rather than fixed by the
// edge. Guard == nil always passes. Action performs the edge's side effects
// (through Effects) and may emit bounded follow-on events.
type Transition struct {
	From   coordinator.Phase
	On     EventKind
	Guard  guardFunc
	Action actionFunc
	To     coordinator.Phase
	Desc   string
}

// transitionTable is the single source of truth for the lifecycle workflow.
// Rows are evaluated in order; for a given (phase, event) the first row whose
// guard passes wins.
func transitionTable() []Transition {
	return []Transition{
		// --- Author / ready ---------------------------------------------------
		// Readying an author session marks the change ready, advances the head,
		// resets automated checks, and schedules the first/next review round. The
		// resulting phase is derived (typically critique once the change is ready).
		{On: EventSessionReady, Action: actReadyAuthorSession, Desc: "ready"},
		// A working/waiting flip is routed through the engine so the recorded
		// session state and derived planning/authoring phase stay in sync.
		{On: EventSessionStateChanged, Action: actSessionStateChanged, Desc: "session-state"},

		// --- Check report / critique -> fix / acceptance / approved -> merge ---
		// A reported check is the engine of the review loop. The action records the
		// check, enqueues acceptance inline for satisfied critiques, then fans out
		// to the guarded fix and auto-merge edges.
		{On: EventCheckReported, Action: actReportCheck, Desc: "report-check"},
		// Internal follow-on: a blocked required check enqueues a fix author job.
		{On: EventEnsureFixAuthorJob, Guard: guardCanFix, Action: actEnsureAuthorJob, Desc: "ensure-fix"},
		// Internal follow-on: an approved auto-merge issue is merged.
		{On: EventAutoMerge, Guard: guardAutoMergeReady, Action: actAutoMerge, Desc: "auto-merge"},

		// --- Schedule ---------------------------------------------------------
		// Scheduling sets backlog/up_next; moving up next enqueues an author job.
		{On: EventScheduleIssue, Action: actScheduleIssue, Desc: "schedule"},
		// Manual state changes are owner-operated repairs/overrides, including
		// reopening a mistakenly closed issue.
		{On: EventSetIssueState, Action: actSetIssueState, Desc: "set-issue-state"},
		// Closing an issue is routed through the engine so the move to a closed
		// phase (derived: abandoned/merged_closed/rejected_closed) is logged.
		{On: EventCloseIssue, Action: actCloseIssue, Desc: "close"},
		// Resetting discards authoring artifacts (jobs, sessions, changes,
		// branches) and re-enqueues a fresh author job for up_next issues.
		{On: EventResetIssue, Action: actResetIssue, Desc: "reset"},
		// Retrying a crash-held author job clears the crash hold and preserves
		// the current change/branch.
		{On: EventRetryCrashedAuthorJob, Action: actRetryCrashedAuthorJob, Desc: "retry-crashed-author"},
		// Internal follow-on from scheduling up next.
		{On: EventEnsureAuthorJob, Action: actEnsureAuthorJob, Desc: "ensure-author"},

		// --- Triage -----------------------------------------------------------
		// Accepting derives back to a live phase; rejecting derives to rejected_closed.
		{On: EventTriageIssue, Action: actTriage, Desc: "triage"},

		// --- Merge ------------------------------------------------------------
		{On: EventMergeRequested, Action: actMergeIssue, Desc: "merge"},
		{On: EventMergeChange, Action: actMergeChange, Desc: "merge-change"},

		// --- Deadlines (durable timers; see DeadlineConfig) -------------------
		// A phase-dwell timer escalates a stalled planning/authoring session; the
		// action decides reschedule vs escalate.
		{On: EventPhaseDeadline, Guard: guardPhaseDeadlineRelevant, Action: actPhaseDeadline, Desc: "phase-deadline"},
		// A check-timeout fires when a pending check never reported; it emits a
		// blocked EventCheckReported so the normal report-check cascade runs.
		{On: EventCheckTimeout, Guard: guardCheckTimeoutPending, Action: actCheckTimeout, Desc: "check-timeout"},

		// --- Review threads (sub-state machine; issue phase unchanged) ---------
		{On: EventThreadClaimed, Action: actClaimThread, Desc: "thread-claim"},
		{On: EventThreadCertify, Action: actCertifyThread, Desc: "thread-certify"},
		{On: EventThreadReopen, Action: actReopenThread, Desc: "thread-reopen"},
		{On: EventThreadComment, Action: actCommentThread, Desc: "thread-comment"},
	}
}

// matches reports whether a transition row applies to the given phase/event.
func (t Transition) matches(phase coordinator.Phase, kind EventKind) bool {
	if t.On != kind {
		return false
	}
	return t.From == "" || t.From == phase
}

// hasCandidate reports whether any transition row could handle the event in the
// given phase (regardless of guards). Distinguishes "illegal event" from "legal
// event whose guard declined".
func hasCandidate(phase coordinator.Phase, kind EventKind) bool {
	for _, t := range transitionTable() {
		if t.matches(phase, kind) {
			return true
		}
	}
	return false
}

// selectTransition finds the first matching transition whose guard passes,
// returning it plus the guard_result string for the audit log.
func (e *Engine) selectTransition(ctx context.Context, ev Event, snap *snapshot) (Transition, string, bool, error) {
	for _, t := range transitionTable() {
		if !t.matches(snap.phase, ev.Kind) {
			continue
		}
		if t.Guard == nil {
			return t, t.Desc, true, nil
		}
		ok, reason, err := t.Guard(ctx, e, ev, snap)
		if err != nil {
			return Transition{}, "", false, err
		}
		if ok {
			if reason == "" {
				reason = t.Desc
			}
			return t, reason, true, nil
		}
	}
	return Transition{}, "", false, nil
}
