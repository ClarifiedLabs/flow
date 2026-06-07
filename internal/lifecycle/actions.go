package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// actionFunc is a transition's reducer. It performs the transition's side
// effects through Effects, may populate the StepResult, and may return bounded
// follow-on events that the engine applies in order within the same Step.
type actionFunc func(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error)

// shouldScheduleReadyReview is ported verbatim from the ready handler
// (server.go): a review round is (re)scheduled when the head advanced or the
// change has not yet been marked ready.
func shouldScheduleReadyReview(change coordinator.Change, headSHA string) bool {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return false
	}
	return strings.TrimSpace(change.HeadSHA) != headSHA || change.ReadyAt == nil
}

// isCritiqueCheckKind is ported verbatim from server.go: which check kinds, when
// satisfied, can advance an issue toward acceptance.
func isCritiqueCheckKind(kind coordinator.CheckKind) bool {
	switch kind {
	case coordinator.CheckKindCI, coordinator.CheckKindReviewer, coordinator.CheckKindHuman:
		return true
	default:
		return false
	}
}

// actReadyAuthorSession mirrors the ready handler cascade (server.go) exactly,
// preserving the load-bearing ordering: capture the pre-ready change, run the
// preflight suite validation, ready the session, advance the head + reset
// automated checks, then schedule a review round on the pre-ready snapshot.
func actReadyAuthorSession(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	sessionID := ev.SessionID
	headSHA := strings.TrimSpace(ev.Payload.HeadSHA)
	sessionBeforeReady, err := e.eff.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	issue, err := e.eff.GetIssue(ctx, sessionBeforeReady.IssueID)
	if err != nil {
		return nil, err
	}
	if issue.PlanMode && issue.PlanApprovedAt == nil {
		if strings.TrimSpace(issue.PlanBody) == "" {
			return nil, errors.New("planning session requires a recorded plan before ready")
		}
		return nil, errors.New("human plan approval required before ready")
	}

	var preReadyChange coordinator.Change
	var havePreReadyChange bool
	if headSHA != "" {
		change, err := e.eff.GetChange(ctx, sessionBeforeReady.ChangeID)
		if err != nil {
			return nil, err
		}
		preReadyChange = change
		havePreReadyChange = true
		if shouldScheduleReadyReview(change, headSHA) {
			change.HeadSHA = headSHA
			if _, err := e.eff.LoadSuiteForChange(ctx, change); err != nil {
				return nil, err
			}
		}
	}

	session, err := e.eff.ReadyAuthorSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if headSHA != "" {
		change := preReadyChange
		if !havePreReadyChange {
			loaded, err := e.eff.GetChange(ctx, session.ChangeID)
			if err != nil {
				return nil, err
			}
			change = loaded
		}
		if strings.TrimSpace(change.HeadSHA) != headSHA {
			updated, err := e.eff.UpdateChangeHead(ctx, session.ChangeID, headSHA)
			if err != nil {
				return nil, err
			}
			if _, err := e.eff.ResetAutomatedChecksForNewRevision(ctx, session.IssueID); err != nil {
				return nil, err
			}
			change = updated
		}
		if shouldScheduleReadyReview(preReadyChange, headSHA) {
			issue, err := e.eff.GetIssue(ctx, session.IssueID)
			if err != nil {
				return nil, err
			}
			previousHeadSHA := strings.TrimSpace(preReadyChange.HeadSHA)
			if previousHeadSHA == headSHA {
				previousHeadSHA = ""
			}
			round, err := e.eff.ScheduleReviewRound(ctx, coordinator.ScheduleReviewRoundInput{
				Issue:           issue,
				Change:          change,
				PreviousHeadSHA: previousHeadSHA,
			})
			if err != nil {
				return nil, err
			}
			if err := e.scheduleCheckTimeouts(ctx, session.IssueID, change.HeadSHA, round.EnqueuedCheckNames); err != nil {
				return nil, err
			}
		}
	}

	res.Session = &session
	return nil, nil
}

// actSessionStateChanged records a working/waiting flip through the engine so
// the session state and derived planning/authoring phase stay synchronized.
func actSessionStateChanged(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	session, err := e.eff.UpdateSessionState(ctx, ev.SessionID, ev.Payload.SessionState)
	if err != nil {
		return nil, err
	}
	res.Session = &session
	return nil, nil
}

// actReportCheck mirrors the report-check handler cascade (server.go): record the
// check, enqueue acceptance inline when a critique check is satisfied (which must
// run before the auto-merge decision reads ReviewState), then emit the guarded
// fix and auto-merge follow-on edges.
func actReportCheck(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	check, err := e.eff.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:     snap.issueID,
		Name:        ev.Payload.Name,
		Kind:        ev.Payload.CheckKind,
		Required:    ev.Payload.Required,
		Verdict:     ev.Payload.Verdict,
		ExitCode:    ev.Payload.ExitCode,
		Details:     ev.Payload.Details,
		SourceJobID: ev.Payload.SourceJobID,
		Reporter:    ev.Payload.Reporter,
	})
	if err != nil {
		return nil, err
	}
	res.Check = &check

	if check.Verdict == coordinator.CheckSatisfied && isCritiqueCheckKind(check.Kind) {
		change, ok, err := e.eff.ReadyUnmergedChangeForIssue(ctx, snap.issueID)
		if err != nil {
			return nil, err
		}
		if ok {
			enqueuedNames, err := e.eff.EnqueueAcceptanceIfReady(ctx, snap.issueID, change)
			if err != nil {
				return nil, err
			}
			if err := e.scheduleCheckTimeouts(ctx, snap.issueID, change.HeadSHA, enqueuedNames); err != nil {
				return nil, err
			}
		}
	}

	reviewState, err := e.eff.ReviewState(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	res.ReviewState = reviewState

	var followups []Event
	if check.Required && check.Verdict == coordinator.CheckBlocked {
		followups = append(followups, Event{Kind: EventEnsureFixAuthorJob, IssueID: snap.issueID})
	}
	if reviewState == coordinator.ReviewApproved {
		followups = append(followups, Event{Kind: EventAutoMerge, IssueID: snap.issueID})
	}
	return followups, nil
}

// scheduleCheckTimeouts arms one durable EventCheckTimeout per newly enqueued
// check name at now+CheckPending, so a check job that never reports cannot park
// the issue forever. It is a no-op when the deadline is disabled. Each timer
// carries the head SHA it was armed for: checks are keyed (issue, name) and a
// new revision resets the same row back to pending, so a stale timer from an
// older head must NOT fire against the restarted check. The guard compares the
// payload head to the issue's current ready-change head and declines (confirms)
// when they differ; the new head's own timer governs the restarted check.
func (e *Engine) scheduleCheckTimeouts(ctx context.Context, issueID, headSHA string, checkNames []string) error {
	if e.deadlines.CheckPending <= 0 {
		return nil
	}
	headSHA = strings.TrimSpace(headSHA)
	fireAt := e.now().Add(e.deadlines.CheckPending)
	for _, name := range checkNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if _, err := e.ScheduleTimer(ctx, issueID, EventCheckTimeout, fireAt, EventPayload{Name: name, HeadSHA: headSHA}); err != nil {
			return fmt.Errorf("schedule check timeout for %q: %w", name, err)
		}
	}
	return nil
}

// actEnsureAuthorJob enqueues an author job, tolerating ErrAuthorJobSuppressed —
// the benign signal that an existing session/job already covers the work. Used
// by both the blocked-check fix edge and the schedule up-next edge.
func actEnsureAuthorJob(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	input := coordinator.EnsureAuthorJobInput{IssueID: snap.issueID}
	if snap.hasChange {
		input.Branch = snap.change.Branch
		input.Base = snap.change.Base
	}
	if _, err := e.eff.EnsureAuthorJob(ctx, input); err != nil &&
		!errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
		return nil, err
	}
	return nil, nil
}

// Auto-merge retry policy: a transient (non-conflict) merge failure schedules
// a durable retry timer with doubling backoff; the attempt count rides in the
// event payload. After the final attempt a blocked auto-merge check surfaces
// the exhaustion to a human.
const (
	maxAutoMergeAttempts    = 5
	autoMergeRetryBaseDelay = 30 * time.Second
)

// actAutoMerge merges an approved auto-merge issue and re-reads the review state
// so the result reflects the post-merge ("merged") projection.
func actAutoMerge(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	merge, err := e.eff.MergeIssue(ctx, snap.issueID)
	if err != nil {
		var conflict *flowgit.MergeConflictError
		if !errors.As(err, &conflict) {
			if retryErr := scheduleAutoMergeRetry(ctx, e, ev, snap.issueID, err); retryErr != nil {
				return nil, retryErr
			}
			return nil, &nonFatalFollowUpError{kind: EventAutoMerge, err: err}
		}
		check, err := reportAutoMergeConflict(ctx, e, snap.issueID, err, conflict)
		if err != nil {
			return nil, err
		}
		if res.Check == nil {
			res.Check = &check
		}
		reviewState, err := e.eff.ReviewState(ctx, snap.issueID)
		if err != nil {
			return nil, err
		}
		res.ReviewState = reviewState
		return []Event{{Kind: EventEnsureFixAuthorJob, IssueID: snap.issueID}}, nil
	}
	res.Merge = &merge
	reviewState, err := e.eff.ReviewState(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	res.ReviewState = reviewState
	return nil, nil
}

// scheduleAutoMergeRetry arranges the next durable attempt after a transient
// auto-merge failure, or — when attempts are exhausted — reports a blocked
// auto-merge check so a human sees the issue parked in approved. The retry
// timer re-fires EventAutoMerge through guardAutoMergeReady, so a retry that
// lands after the issue merged or lost approval is a benign no-op.
func scheduleAutoMergeRetry(ctx context.Context, e *Engine, ev Event, issueID string, cause error) error {
	// At most one pending retry chain per issue: scheduling commits before the
	// dispatching event's dedup transition does, so a crash-redelivery would
	// otherwise re-run this effect and fork a second chain, defeating the
	// attempt bound. The dispatching timer itself (still unconfirmed while
	// this action runs) is excluded.
	if pending, err := e.hasPendingTimer(ctx, issueID, EventAutoMerge,
		strings.TrimPrefix(ev.IdempotencyKey, "timer:")); err != nil {
		return err
	} else if pending {
		return nil
	}
	next := ev.Payload.AutoMergeAttempt + 1
	if next >= maxAutoMergeAttempts {
		required := true
		exitCode := 1
		details := strings.TrimSpace(cause.Error())
		if details == "" {
			details = "unknown error"
		}
		if _, err := e.eff.ReportCheck(ctx, coordinator.ReportCheckInput{
			IssueID:  issueID,
			Name:     coordinator.AutoMergeCheckName,
			Kind:     coordinator.CheckKindCI,
			Required: &required,
			Verdict:  coordinator.CheckBlocked,
			ExitCode: &exitCode,
			Details:  fmt.Sprintf("%s %d attempts; last: %s", coordinator.AutoMergeTransientDetailsPrefix, next, details),
			Reporter: "coordinator",
		}); err != nil {
			return fmt.Errorf("report auto-merge retry exhaustion: %w", err)
		}
		return nil
	}
	delay := autoMergeRetryBaseDelay << (next - 1)
	if _, err := e.ScheduleTimer(ctx, issueID, EventAutoMerge, e.now().Add(delay), EventPayload{AutoMergeAttempt: next}); err != nil {
		return fmt.Errorf("schedule auto-merge retry: %w", err)
	}
	return nil
}

func reportAutoMergeConflict(ctx context.Context, e *Engine, issueID string, mergeErr error, conflict *flowgit.MergeConflictError) (coordinator.Check, error) {
	required := true
	exitCode := 1
	details := strings.TrimSpace(conflict.Output)
	if details == "" {
		details = strings.TrimSpace(mergeErr.Error())
	}
	if details == "" {
		details = flowgit.ErrMergeConflict.Error()
	}
	return e.eff.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  issueID,
		Name:     coordinator.AutoMergeCheckName,
		Kind:     coordinator.CheckKindCI,
		Required: &required,
		Verdict:  coordinator.CheckBlocked,
		ExitCode: &exitCode,
		Details:  coordinator.AutoMergeConflictDetailsPrefix + " " + details,
		Reporter: "coordinator",
	})
}

// actScheduleIssue sets the schedule state and, when moving to up_next, emits an
// ensure-author-job edge.
func actScheduleIssue(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	issue, err := e.eff.ScheduleIssue(ctx, snap.issueID, ev.Payload.Schedule)
	if err != nil {
		return nil, err
	}
	res.Issue = &issue

	var followups []Event
	if ev.Payload.Schedule == coordinator.ScheduleUpNext {
		followups = append(followups, Event{Kind: EventEnsureAuthorJob, IssueID: snap.issueID})
	}
	return followups, nil
}

func actSetIssueState(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	issue, err := e.eff.SetIssueState(ctx, snap.issueID, ev.Payload.IssueState)
	if err != nil {
		return nil, err
	}
	res.Issue = &issue

	var followups []Event
	if issue.ScheduleState == coordinator.ScheduleUpNext && issue.TriageState == coordinator.TriageAccepted {
		followups = append(followups, Event{Kind: EventEnsureAuthorJob, IssueID: snap.issueID})
	}
	return followups, nil
}

// actResetIssue discards the issue's authoring artifacts and, when the issue is
// still scheduled up next, emits an ensure-author-job edge so a fresh attempt
// starts from the base branch.
func actResetIssue(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	issue, err := e.eff.ResetIssue(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	res.Issue = &issue

	var followups []Event
	if issue.ScheduleState == coordinator.ScheduleUpNext {
		followups = append(followups, Event{Kind: EventEnsureAuthorJob, IssueID: snap.issueID})
	}
	return followups, nil
}

func actRetryCrashedAuthorJob(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	result, err := e.eff.RetryCrashedAuthorJob(ctx, snap.issueID, ev.Actor.Actor())
	if err != nil {
		return nil, err
	}
	res.Issue = &result.Issue
	return nil, nil
}

// actCloseIssue closes the issue through the engine; the resulting closed phase
// (abandoned/merged_closed/rejected_closed) is derived from the issue state.
func actCloseIssue(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	issue, err := e.eff.CloseIssue(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	res.Issue = &issue
	return nil, nil
}

// actTriage accepts or rejects an issue. Acceptance derives back to a live phase;
// rejection derives to rejected_closed.
func actTriage(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	var issue coordinator.Issue
	var err error
	switch ev.Payload.Triage {
	case coordinator.TriageAccepted:
		issue, err = e.eff.AcceptTriage(ctx, snap.issueID)
	case coordinator.TriageRejected:
		issue, err = e.eff.RejectTriage(ctx, snap.issueID)
	default:
		return nil, fmt.Errorf("lifecycle: invalid triage state %q", ev.Payload.Triage)
	}
	if err != nil {
		return nil, err
	}
	res.Issue = &issue
	return nil, nil
}

func actMergeIssue(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	merge, err := e.eff.MergeIssue(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	res.Merge = &merge
	return nil, nil
}

func actMergeChange(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	merge, err := e.eff.MergeChange(ctx, ev.ChangeID)
	if err != nil {
		return nil, err
	}
	res.Merge = &merge
	return nil, nil
}

func actClaimThread(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	thread, err := e.eff.ClaimThread(ctx, coordinator.ClaimThreadInput{
		ThreadID:       ev.ThreadID,
		Kind:           ev.Payload.ThreadKind,
		Body:           ev.Payload.Body,
		Actor:          ev.Actor.Actor(),
		ClaimCommitSHA: ev.Payload.ClaimCommitSHA,
	})
	if err != nil {
		return nil, err
	}
	res.Thread = &thread
	return nil, nil
}

func actCertifyThread(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	thread, err := e.eff.CertifyThread(ctx, coordinator.VerifyThreadInput{
		ThreadID: ev.ThreadID,
		Body:     ev.Payload.Body,
		Actor:    ev.Actor.Actor(),
	})
	if err != nil {
		return nil, err
	}
	res.Thread = &thread
	return nil, nil
}

func actReopenThread(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	thread, err := e.eff.ReopenThread(ctx, coordinator.VerifyThreadInput{
		ThreadID: ev.ThreadID,
		Body:     ev.Payload.Body,
		Actor:    ev.Actor.Actor(),
	})
	if err != nil {
		return nil, err
	}
	res.Thread = &thread
	return nil, nil
}

func actCommentThread(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	thread, err := e.eff.AddComment(ctx, coordinator.AddThreadCommentInput{
		ThreadID: ev.ThreadID,
		Body:     ev.Payload.Body,
		Actor:    ev.Actor.Actor(),
	})
	if err != nil {
		return nil, err
	}
	res.Thread = &thread
	return nil, nil
}

// actPhaseDeadline fires when a phase's dwell window elapses (the guard has
// already confirmed the issue is still in that phase). For planning/authoring
// it decides reschedule-vs-escalate from agent activity. The decision is
// recorded in the transition log either way.
func actPhaseDeadline(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	switch ev.Payload.DeadlinePhase {
	case coordinator.PhasePlanning:
		return e.handleAuthoringDeadline(ctx, ev, snap)
	case coordinator.PhaseAuthoring:
		return e.handleAuthoringDeadline(ctx, ev, snap)
	default:
		// A deadline for a phase we no longer escalate on (or a disabled one):
		// nothing to do; the timer confirms.
		return nil, nil
	}
}

// handleAuthoringDeadline reschedules the deadline when the agent was active
// within the window, or escalates a stalled authoring session otherwise. "No
// active session / no activity timestamp" is treated as stale: the guard
// already proved the issue is still in authoring, and the window already
// elapsed since it was entered, so a session that produced no signal in that
// time is wedged.
func (e *Engine) handleAuthoringDeadline(ctx context.Context, ev Event, snap *snapshot) ([]Event, error) {
	window := e.deadlines.AuthoringStall
	lastActivity, ok, err := e.eff.LastAgentActivity(ctx, snap.issueID)
	if err != nil {
		return nil, err
	}
	if ok && lastActivity != nil {
		if lastActivity.Add(window).After(e.now()) {
			// Fresh activity: rearm for the moment the window next lapses from
			// the last signal, rather than escalating a session that is working.
			if _, err := e.ScheduleTimer(ctx, snap.issueID, EventPhaseDeadline, lastActivity.Add(window), EventPayload{
				DeadlinePhase: coordinator.PhaseAuthoring,
			}); err != nil {
				return nil, fmt.Errorf("reschedule authoring deadline: %w", err)
			}
			return nil, nil
		}
	}

	// Stale: surface the stall as a non-required blocked check plus a blocker
	// status entry so a human notices without the issue being forced backward.
	notRequired := false
	phase := strings.TrimSpace(string(ev.Payload.DeadlinePhase))
	if phase == "" {
		phase = string(coordinator.PhaseAuthoring)
	}
	details := fmt.Sprintf("%s stalled: no agent activity for %s", phase, window)
	if _, err := e.eff.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  snap.issueID,
		Name:     phaseDeadlineCheckName,
		Kind:     coordinator.CheckKindCI,
		Required: &notRequired,
		Verdict:  coordinator.CheckBlocked,
		Details:  details,
		Reporter: "coordinator",
	}); err != nil {
		return nil, err
	}
	if err := e.eff.WriteStatus(ctx, coordinator.WriteStatusInput{
		IssueID: snap.issueID,
		Actor:   "coordinator",
		Kind:    coordinator.StatusKindBlocker,
		Message: details,
	}); err != nil {
		return nil, err
	}
	return nil, nil
}

// phaseDeadlineCheckName is the non-required check the engine reports when an
// authoring session stalls past its deadline.
const phaseDeadlineCheckName = "phase-deadline"

// actCheckTimeout escalates a still-pending check whose timeout elapsed (the
// guard has already confirmed it is pending). Rather than calling ReportCheck
// directly, it emits a follow-up EventCheckReported so the full existing
// report-check cascade (fix-job follow-up, idempotency, acceptance/auto-merge)
// runs through the normal edge. Requiredness is preserved by reading the
// existing check and passing its Required back, so the timeout never silently
// flips a required check to optional or vice versa.
func actCheckTimeout(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	existing, err := e.eff.GetCheck(ctx, snap.issueID, ev.Payload.Name)
	if err != nil {
		return nil, err
	}
	required := existing.Required
	return []Event{{
		Kind:    EventCheckReported,
		IssueID: snap.issueID,
		Payload: EventPayload{
			Name:      ev.Payload.Name,
			CheckKind: existing.Kind,
			Required:  &required,
			Verdict:   coordinator.CheckBlocked,
			Details:   fmt.Sprintf("timed out after %s", e.deadlines.CheckPending),
			Reporter:  "coordinator",
		},
	}}, nil
}
