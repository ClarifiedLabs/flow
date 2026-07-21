package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// satisfied, can advance an task toward acceptance.
func isCritiqueCheckKind(kind coordinator.CheckKind) bool {
	switch kind {
	case coordinator.CheckKindCI, coordinator.CheckKindReviewer, coordinator.CheckKindHuman:
		return true
	default:
		return false
	}
}

// actWorkPhaseComplete handles flow ready: the session's current work phase is
// done. An intermediate phase (or a human-gated final phase) stores its handoff
// and either auto-advances the cursor — enqueueing the next phase's job — or
// pauses awaiting approval. The final phase runs the ready tail: publish the
// change, advance the head, reset automated checks, and schedule the first
// review round. Tasks without a cursor (no flows configured) behave as an
// implicit single auto phase.
func actWorkPhaseComplete(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	sessionID := ev.SessionID
	headSHA := strings.TrimSpace(ev.Payload.HeadSHA)
	sessionBeforeReady, err := e.eff.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	cursor, hasCursor, err := e.eff.FlowCursor(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if hasCursor && cursor.PhaseState != coordinator.FlowPhaseCompleted {
		phase, ok := cursor.CurrentPhase()
		if !ok {
			return nil, fmt.Errorf("flow cursor for %s is out of range (phase %d of %d)",
				snap.taskID, cursor.PhaseIndex, len(cursor.Snapshot.Phases))
		}
		// A gated final phase pauses like any other gated phase; approval runs
		// the ready tail (actApproveWorkPhase).
		if !cursor.OnFinalPhase() || phase.Gate == coordinator.FlowGateHuman {
			return e.completeGatedOrIntermediatePhase(ctx, ev, snap, res, cursor, phase, sessionBeforeReady)
		}
		// Final auto phase: record the handoff for the phase timeline, then run
		// the ready tail below.
		if err := e.storeCursorPhaseHandoff(ctx, snap.taskID, cursor, sessionBeforeReady.ChangeID); err != nil {
			return nil, err
		}
		if _, err := e.eff.CompleteFlowCursor(ctx, snap.taskID, cursor.PhaseIndex); err != nil {
			return nil, err
		}
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
			if _, err := e.eff.ResetAutomatedChecksForNewRevision(ctx, session.TaskID); err != nil {
				return nil, err
			}
			change = updated
		}
		if shouldScheduleReadyReview(preReadyChange, headSHA) {
			task, err := e.eff.GetTask(ctx, session.TaskID)
			if err != nil {
				return nil, err
			}
			previousHeadSHA := strings.TrimSpace(preReadyChange.HeadSHA)
			if previousHeadSHA == headSHA {
				previousHeadSHA = ""
			}
			round, err := e.eff.ScheduleReviewRound(ctx, coordinator.ScheduleReviewRoundInput{
				Task:            task,
				Change:          change,
				PreviousHeadSHA: previousHeadSHA,
			})
			if err != nil {
				return nil, err
			}
			if err := e.scheduleCheckTimeouts(ctx, session.TaskID, change.HeadSHA, round.EnqueuedCheckNames); err != nil {
				return nil, err
			}
		}
	}

	res.Session = &session
	return nil, nil
}

// completeGatedOrIntermediatePhase finishes a work phase that does NOT publish
// the change: any non-final phase, or a human-gated final phase. Effect order
// is load-bearing for crash redelivery: the handoff upsert and the CAS cursor
// move commit before the session finishes, so a replay that finds the session
// already finished can safely no-op (the cursor has provably moved), while a
// replay that finds the session live simply re-runs the idempotent steps.
func (e *Engine) completeGatedOrIntermediatePhase(ctx context.Context, ev Event, snap *snapshot, res *StepResult, cursor coordinator.FlowCursor, phase coordinator.FlowPhaseSnapshot, sessionBeforeReady coordinator.Session) ([]Event, error) {
	if sessionBeforeReady.RuntimeState == coordinator.SessionFinished {
		// A duplicate ready for a phase whose completion already processed:
		// the cursor moved on, and re-completing would double-advance it.
		res.Session = &sessionBeforeReady
		return nil, nil
	}

	if err := e.storeCursorPhaseHandoff(ctx, snap.taskID, cursor, sessionBeforeReady.ChangeID); err != nil {
		return nil, err
	}

	advanced := false
	if phase.Gate == coordinator.FlowGateHuman {
		if _, err := e.eff.PauseFlowCursor(ctx, snap.taskID, cursor.PhaseIndex); err != nil {
			return nil, err
		}
	} else {
		moved, err := e.eff.AdvanceFlowCursor(ctx, snap.taskID, cursor.PhaseIndex)
		if err != nil {
			return nil, err
		}
		advanced = moved
	}

	session, err := e.eff.FinishWorkPhaseSession(ctx, ev.SessionID)
	if err != nil {
		return nil, err
	}
	res.Session = &session

	if advanced {
		// The FSM phase stays `working` across sub-phases, so the phase-change
		// deadline hook in attemptTransition never rearms; arm the next
		// phase's dwell window here (deduped, non-fatal like the hook).
		if err := e.schedulePhaseDeadline(ctx, snap.taskID, coordinator.PhaseWorking); err != nil {
			slog.Warn("lifecycle: arm work-phase deadline failed (non-fatal)",
				"task", snap.taskID, "error", err)
		}
		return []Event{{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID}}, nil
	}
	return nil, nil
}

// storeCursorPhaseHandoff copies the change-scoped handoff snapshot the agent
// submitted at flow ready into the per-phase handoff store under the cursor's
// current index. Missing snapshots are tolerated: not every phase writes one.
func (e *Engine) storeCursorPhaseHandoff(ctx context.Context, taskID string, cursor coordinator.FlowCursor, changeID string) error {
	phase, ok := cursor.CurrentPhase()
	if !ok {
		return nil
	}
	snapshot, ok, err := e.eff.ChangeHandoff(ctx, changeID)
	if err != nil || !ok {
		return err
	}
	return e.eff.StorePhaseHandoff(ctx, coordinator.StorePhaseHandoffInput{
		TaskID:     taskID,
		PhaseIndex: cursor.PhaseIndex,
		PhaseName:  phase.Name,
		Content:    snapshot.Content,
		HeadSHA:    snapshot.HeadSHA,
	})
}

// actApproveWorkPhase applies a human's gate approval. For an intermediate
// phase the cursor advances and the next phase's job is ensured. For the final
// phase — whose session already finished at the pause — it runs the ready
// tail directly: publish the change, advance the head to the stored handoff's
// SHA, reset automated checks, and schedule the first review round.
func actApproveWorkPhase(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	cursor, ok, err := e.eff.FlowCursor(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("task has no flow cursor to approve")
	}

	if !cursor.OnFinalPhase() {
		advanced, err := e.eff.AdvanceFlowCursor(ctx, snap.taskID, cursor.PhaseIndex)
		if err != nil {
			return nil, err
		}
		if advanced {
			if err := e.schedulePhaseDeadline(ctx, snap.taskID, coordinator.PhaseWorking); err != nil {
				slog.Warn("lifecycle: arm work-phase deadline failed (non-fatal)",
					"task", snap.taskID, "error", err)
			}
		}
		return []Event{{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID}}, nil
	}

	change, hasChange, err := e.eff.LatestChangeForTask(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if !hasChange {
		return nil, errors.New("final work phase has no change to publish")
	}
	handoff, _, err := e.eff.PhaseHandoff(ctx, snap.taskID, cursor.PhaseIndex)
	if err != nil {
		return nil, err
	}
	headSHA := strings.TrimSpace(handoff.HeadSHA)

	preReadyChange := change
	change, err = e.eff.ReadyChange(ctx, change.ID)
	if err != nil {
		return nil, err
	}
	if headSHA != "" {
		if strings.TrimSpace(change.HeadSHA) != headSHA {
			updated, err := e.eff.UpdateChangeHead(ctx, change.ID, headSHA)
			if err != nil {
				return nil, err
			}
			if _, err := e.eff.ResetAutomatedChecksForNewRevision(ctx, snap.taskID); err != nil {
				return nil, err
			}
			change = updated
		}
		if shouldScheduleReadyReview(preReadyChange, headSHA) {
			task, err := e.eff.GetTask(ctx, snap.taskID)
			if err != nil {
				return nil, err
			}
			previousHeadSHA := strings.TrimSpace(preReadyChange.HeadSHA)
			if previousHeadSHA == headSHA {
				previousHeadSHA = ""
			}
			round, err := e.eff.ScheduleReviewRound(ctx, coordinator.ScheduleReviewRoundInput{
				Task:            task,
				Change:          change,
				PreviousHeadSHA: previousHeadSHA,
			})
			if err != nil {
				return nil, err
			}
			if err := e.scheduleCheckTimeouts(ctx, snap.taskID, change.HeadSHA, round.EnqueuedCheckNames); err != nil {
				return nil, err
			}
		}
	}

	if _, err := e.eff.CompleteFlowCursor(ctx, snap.taskID, cursor.PhaseIndex); err != nil {
		return nil, err
	}
	return nil, nil
}

// actReworkWorkPhase applies a human's request-changes: the gate-paused phase
// returns to pending with the feedback stored on the cursor (injected into the
// re-run's prompt), and its job is re-ensured.
func actReworkWorkPhase(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	cursor, ok, err := e.eff.FlowCursor(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("task has no flow cursor to rework")
	}
	resumed, err := e.eff.ResumeFlowCursor(ctx, snap.taskID, cursor.PhaseIndex, ev.Payload.GateFeedback)
	if err != nil {
		return nil, err
	}
	if resumed {
		if err := e.schedulePhaseDeadline(ctx, snap.taskID, coordinator.PhaseWorking); err != nil {
			slog.Warn("lifecycle: arm work-phase deadline failed (non-fatal)",
				"task", snap.taskID, "error", err)
		}
	}
	return []Event{{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID}}, nil
}

// actEnsureWorkPhaseJob enqueues the author job for the task's current work
// phase, freezing the flow cursor from the task's flow on first use. A cursor
// paused at a gate or already through the pipeline enqueues nothing.
func actEnsureWorkPhaseJob(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	cursor, hasCursor, err := e.eff.EnsureFlowCursor(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if hasCursor {
		switch cursor.PhaseState {
		case coordinator.FlowPhaseAwaitingApproval, coordinator.FlowPhaseCompleted:
			return nil, nil
		}
	}
	return actEnsureAuthorJob(ctx, e, ev, snap, res)
}

// actSessionStateChanged records a working/waiting flip through the engine so
// the session state and derived phase stay synchronized.
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
		TaskID:      snap.taskID,
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
		change, ok, err := e.eff.ReadyUnmergedChangeForTask(ctx, snap.taskID)
		if err != nil {
			return nil, err
		}
		if ok {
			enqueuedNames, err := e.eff.EnqueueAcceptanceIfReady(ctx, snap.taskID, change)
			if err != nil {
				return nil, err
			}
			if err := e.scheduleCheckTimeouts(ctx, snap.taskID, change.HeadSHA, enqueuedNames); err != nil {
				return nil, err
			}
		}
	}

	reviewState, err := e.eff.ReviewState(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	res.ReviewState = reviewState

	var followups []Event
	if check.Required && check.Verdict == coordinator.CheckBlocked {
		followups = append(followups, Event{Kind: EventEnsureFixAuthorJob, TaskID: snap.taskID})
	}
	if reviewState == coordinator.ReviewApproved {
		followups = append(followups, Event{Kind: EventAutoMerge, TaskID: snap.taskID})
	}
	return followups, nil
}

// scheduleCheckTimeouts arms one durable EventCheckTimeout per newly enqueued
// check name at now+CheckPending, so a check job that never reports cannot park
// the task forever. It is a no-op when the deadline is disabled. Each timer
// carries the head SHA it was armed for: checks are keyed (task, name) and a
// new revision resets the same row back to pending, so a stale timer from an
// older head must NOT fire against the restarted check. The guard compares the
// payload head to the task's current ready-change head and declines (confirms)
// when they differ; the new head's own timer governs the restarted check.
func (e *Engine) scheduleCheckTimeouts(ctx context.Context, taskID, headSHA string, checkNames []string) error {
	if e.deadlines.CheckPending <= 0 {
		return nil
	}
	headSHA = strings.TrimSpace(headSHA)
	fireAt := e.now().Add(e.deadlines.CheckPending)
	for _, name := range checkNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if _, err := e.ScheduleTimer(ctx, taskID, EventCheckTimeout, fireAt, EventPayload{Name: name, HeadSHA: headSHA}); err != nil {
			return fmt.Errorf("schedule check timeout for %q: %w", name, err)
		}
	}
	return nil
}

// actEnsureAuthorJob enqueues an author job, tolerating ErrAuthorJobSuppressed —
// the benign signal that an existing session/job already covers the work. Used
// by both the blocked-check fix edge and the schedule up-next edge.
func actEnsureAuthorJob(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	input := coordinator.EnsureAuthorJobInput{TaskID: snap.taskID}
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

// actAutoMerge merges an approved auto-merge task and re-reads the review state
// so the result reflects the post-merge ("merged") projection.
func actAutoMerge(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	merge, err := e.eff.MergeTask(ctx, snap.taskID)
	if err != nil {
		var conflict *flowgit.MergeConflictError
		if !errors.As(err, &conflict) {
			if retryErr := scheduleAutoMergeRetry(ctx, e, ev, snap.taskID, err); retryErr != nil {
				return nil, retryErr
			}
			return nil, &nonFatalFollowUpError{kind: EventAutoMerge, err: err}
		}
		check, err := reportAutoMergeConflict(ctx, e, snap.taskID, err, conflict)
		if err != nil {
			return nil, err
		}
		if res.Check == nil {
			res.Check = &check
		}
		reviewState, err := e.eff.ReviewState(ctx, snap.taskID)
		if err != nil {
			return nil, err
		}
		res.ReviewState = reviewState
		return []Event{{Kind: EventEnsureFixAuthorJob, TaskID: snap.taskID}}, nil
	}
	res.Merge = &merge
	reviewState, err := e.eff.ReviewState(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	res.ReviewState = reviewState
	return nil, nil
}

// scheduleAutoMergeRetry arranges the next durable attempt after a transient
// auto-merge failure, or — when attempts are exhausted — reports a blocked
// auto-merge check so a human sees the task parked in approved. The retry
// timer re-fires EventAutoMerge through guardAutoMergeReady, so a retry that
// lands after the task merged or lost approval is a benign no-op.
func scheduleAutoMergeRetry(ctx context.Context, e *Engine, ev Event, taskID string, cause error) error {
	// At most one pending retry chain per task: scheduling commits before the
	// dispatching event's dedup transition does, so a crash-redelivery would
	// otherwise re-run this effect and fork a second chain, defeating the
	// attempt bound. The dispatching timer itself (still unconfirmed while
	// this action runs) is excluded.
	if pending, err := e.hasPendingTimer(ctx, taskID, EventAutoMerge,
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
			TaskID:   taskID,
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
	if _, err := e.ScheduleTimer(ctx, taskID, EventAutoMerge, e.now().Add(delay), EventPayload{AutoMergeAttempt: next}); err != nil {
		return fmt.Errorf("schedule auto-merge retry: %w", err)
	}
	return nil
}

func reportAutoMergeConflict(ctx context.Context, e *Engine, taskID string, mergeErr error, conflict *flowgit.MergeConflictError) (coordinator.Check, error) {
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
		TaskID:   taskID,
		Name:     coordinator.AutoMergeCheckName,
		Kind:     coordinator.CheckKindCI,
		Required: &required,
		Verdict:  coordinator.CheckBlocked,
		ExitCode: &exitCode,
		Details:  coordinator.AutoMergeConflictDetailsPrefix + " " + details,
		Reporter: "coordinator",
	})
}

// actScheduleTask sets the schedule state and, when moving to up_next, emits an
// ensure-author-job edge.
func actScheduleTask(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	task, err := e.eff.ScheduleTask(ctx, snap.taskID, ev.Payload.Schedule)
	if err != nil {
		return nil, err
	}
	res.Task = &task

	var followups []Event
	if ev.Payload.Schedule == coordinator.ScheduleUpNext {
		followups = append(followups, Event{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID})
	}
	return followups, nil
}

func actSetTaskState(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	task, err := e.eff.SetTaskState(ctx, snap.taskID, ev.Payload.TaskState)
	if err != nil {
		return nil, err
	}
	res.Task = &task

	var followups []Event
	if task.ScheduleState == coordinator.ScheduleUpNext && task.TriageState == coordinator.TriageAccepted {
		followups = append(followups, Event{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID})
	}
	return followups, nil
}

// actResetTask discards the task's authoring artifacts and, when the task is
// still scheduled up next, emits an ensure-author-job edge so a fresh attempt
// starts from the base branch.
func actResetTask(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	task, err := e.eff.ResetTask(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	res.Task = &task

	var followups []Event
	if task.ScheduleState == coordinator.ScheduleUpNext {
		followups = append(followups, Event{Kind: EventEnsureWorkPhaseJob, TaskID: snap.taskID})
	}
	return followups, nil
}

func actRetryCrashedAuthorJob(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	result, err := e.eff.RetryCrashedAuthorJob(ctx, snap.taskID, ev.Actor.Actor())
	if err != nil {
		return nil, err
	}
	res.Task = &result.Task
	return nil, nil
}

// actCloseTask closes the task through the engine; the resulting closed phase
// (abandoned/merged_closed/rejected_closed) is derived from the task state.
func actCloseTask(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	task, err := e.eff.CloseTask(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	res.Task = &task
	return nil, nil
}

// actTriage accepts or rejects an task. Acceptance derives back to a live phase;
// rejection derives to rejected_closed.
func actTriage(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	var task coordinator.Task
	var err error
	switch ev.Payload.Triage {
	case coordinator.TriageAccepted:
		task, err = e.eff.AcceptTriage(ctx, snap.taskID)
	case coordinator.TriageRejected:
		task, err = e.eff.RejectTriage(ctx, snap.taskID)
	default:
		return nil, fmt.Errorf("lifecycle: invalid triage state %q", ev.Payload.Triage)
	}
	if err != nil {
		return nil, err
	}
	res.Task = &task
	return nil, nil
}

func actMergeTask(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	merge, err := e.eff.MergeTask(ctx, snap.taskID)
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
// already confirmed the task is still in that phase). For the working phase
// it decides reschedule-vs-escalate from agent activity. The decision is
// recorded in the transition log either way.
func actPhaseDeadline(ctx context.Context, e *Engine, ev Event, snap *snapshot, res *StepResult) ([]Event, error) {
	switch ev.Payload.DeadlinePhase {
	case coordinator.PhaseWorking:
		return e.handleWorkPhaseDeadline(ctx, ev, snap)
	default:
		// A deadline for a phase we no longer escalate on (or a disabled one):
		// nothing to do; the timer confirms.
		return nil, nil
	}
}

// handleWorkPhaseDeadline reschedules the deadline when the agent was active
// within the window, or escalates a stalled work-phase session otherwise. "No
// active session / no activity timestamp" is treated as stale: the guard
// already proved the task is still in working, and the window already
// elapsed since it was entered, so a session that produced no signal in that
// time is wedged. A flow paused at a human gate is deliberately waiting, not
// stalled — the timer confirms without escalating, and the gate approval
// rearms the window for the next phase.
func (e *Engine) handleWorkPhaseDeadline(ctx context.Context, ev Event, snap *snapshot) ([]Event, error) {
	if cursor, ok, err := e.eff.FlowCursor(ctx, snap.taskID); err != nil {
		return nil, err
	} else if ok && cursor.PhaseState == coordinator.FlowPhaseAwaitingApproval {
		return nil, nil
	}

	window := e.deadlines.AuthoringStall
	lastActivity, ok, err := e.eff.LastAgentActivity(ctx, snap.taskID)
	if err != nil {
		return nil, err
	}
	if ok && lastActivity != nil {
		if lastActivity.Add(window).After(e.now()) {
			// Fresh activity: rearm for the moment the window next lapses from
			// the last signal, rather than escalating a session that is working.
			if _, err := e.ScheduleTimer(ctx, snap.taskID, EventPhaseDeadline, lastActivity.Add(window), EventPayload{
				DeadlinePhase: coordinator.PhaseWorking,
			}); err != nil {
				return nil, fmt.Errorf("reschedule work-phase deadline: %w", err)
			}
			return nil, nil
		}
	}

	// Stale: surface the stall as a non-required blocked check plus a blocker
	// status entry so a human notices without the task being forced backward.
	notRequired := false
	phase := strings.TrimSpace(string(ev.Payload.DeadlinePhase))
	if phase == "" {
		phase = string(coordinator.PhaseWorking)
	}
	details := fmt.Sprintf("%s stalled: no agent activity for %s", phase, window)
	if _, err := e.eff.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID:   snap.taskID,
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
		TaskID:  snap.taskID,
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
	existing, err := e.eff.GetCheck(ctx, snap.taskID, ev.Payload.Name)
	if err != nil {
		return nil, err
	}
	required := existing.Required
	return []Event{{
		Kind:   EventCheckReported,
		TaskID: snap.taskID,
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
