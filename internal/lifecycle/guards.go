package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// guardFunc decides whether a transition applies to an event in a given
// snapshot. It returns the decision, a short human-readable reason recorded as
// guard_result in the transition log, and any error.
type guardFunc func(ctx context.Context, e *Engine, ev Event, snap *snapshot) (bool, string, error)

// guardCanFix ports ensureFixAuthorJobForBlockedCheck's preconditions
// (server.go): a fix author job is only enqueued for a blocked required check
// when the issue is accepted, up next, and has a ready unmerged change.
func guardCanFix(ctx context.Context, e *Engine, ev Event, snap *snapshot) (bool, string, error) {
	if snap.issue.TriageState != coordinator.TriageAccepted || snap.issue.ScheduleState != coordinator.ScheduleUpNext {
		return false, "not accepted/up_next", nil
	}
	ready, err := e.eff.HasReadyUnmergedChange(ctx, snap.issueID)
	if err != nil {
		return false, "", err
	}
	if !ready {
		return false, "no ready change", nil
	}
	return true, "blocked->fix", nil
}

// guardPhaseDeadlineRelevant passes iff the issue is still in the phase the
// timer was armed for. The decision of reschedule-vs-escalate (for fresh vs
// stale agent activity) lives in the action so it is logged either way; the
// guard only filters out a timer that fired after the issue already moved on,
// which is a benign no-op (the timer confirms).
func guardPhaseDeadlineRelevant(ctx context.Context, e *Engine, ev Event, snap *snapshot) (bool, string, error) {
	if snap.phase != ev.Payload.DeadlinePhase {
		return false, "phase moved on", nil
	}
	return true, "phase-deadline", nil
}

// guardCheckTimeoutPending passes iff the named check still exists for the
// issue, is unresolved (verdict pending), AND the timer was armed for the
// current ready-change head. The head check is load-bearing: checks are keyed
// (issue, name) and a new revision UPDATEs the same row back to pending, so a
// stale timer from an older head would otherwise fire against the just-restarted
// check and falsely report it timed out. A timer whose head no longer matches
// (or whose change is gone) is declined, so it confirms while the new head's own
// timer governs the restarted check. A check that already reported — satisfied,
// blocked, or skipped — also makes the timeout moot.
func guardCheckTimeoutPending(ctx context.Context, e *Engine, ev Event, snap *snapshot) (bool, string, error) {
	payloadHead := strings.TrimSpace(ev.Payload.HeadSHA)
	if payloadHead != "" {
		if !snap.hasChange || strings.TrimSpace(snap.change.HeadSHA) != payloadHead {
			return false, "head moved on", nil
		}
	}
	check, err := e.eff.GetCheck(ctx, snap.issueID, ev.Payload.Name)
	if err != nil {
		// A missing check (e.g. retired by a new revision) makes the timeout
		// moot; decline so the timer confirms rather than retrying forever.
		if errors.Is(err, sql.ErrNoRows) {
			return false, "check gone", nil
		}
		return false, "", err
	}
	if check.Verdict != coordinator.CheckPending {
		return false, "check reported", nil
	}
	return true, "check-timeout", nil
}

// guardAutoMergeReady ports autoMergeApprovedIssue's preconditions (server.go):
// auto-merge fires only when the review is approved and the issue opts into it.
func guardAutoMergeReady(ctx context.Context, e *Engine, ev Event, snap *snapshot) (bool, string, error) {
	reviewState, err := e.eff.ReviewState(ctx, snap.issueID)
	if err != nil {
		return false, "", err
	}
	if reviewState != coordinator.ReviewApproved {
		return false, "not approved", nil
	}
	if !snap.issue.AutoMerge {
		return false, "auto-merge off", nil
	}
	return true, "approved->merge", nil
}
