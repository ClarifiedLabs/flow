package coordinator

import (
	"context"
	"database/sql"
	"strings"
)

// Phase is the explicit, authoritative lifecycle coordinate for an task. It is
// stored in workflow_state by the lifecycle engine and is a projection of the
// existing state mechanisms (schedule/triage columns, the task_review_state
// view, change ready/merged latches, and active author sessions).
//
// blocked is deliberately NOT a phase: it remains a derived overlay computed
// live from work_item_relations, so a blocked task still carries its underlying
// phase (backlog/up_next/critique/...) and the block is layered on at read time.
type Phase string

const (
	PhaseBacklog Phase = "backlog"
	PhaseTriage  Phase = "triage"
	PhaseUpNext  Phase = "up_next"
	// PhaseWorking is the container phase for the task's whole work pipeline:
	// the position within the pipeline (which flow phase, running vs paused at
	// a human gate) lives on the task's flow cursor, not in workflow_state.
	PhaseWorking        Phase = "working"
	PhaseCritique       Phase = "critique"
	PhaseAcceptance     Phase = "acceptance"
	PhaseApproved       Phase = "approved"
	PhaseMergedClosed   Phase = "merged_closed"
	PhaseRejectedClosed Phase = "rejected_closed"
	PhaseAbandoned      Phase = "abandoned"
)

// AllPhases enumerates every Phase constant in declaration order. It is the
// server's exhaustive phase vocabulary; the parity test and lifecycle control
// enumerate it to prove the UI covers every phase the server derives.
var AllPhases = [...]Phase{
	PhaseBacklog,
	PhaseTriage,
	PhaseUpNext,
	PhaseWorking,
	PhaseCritique,
	PhaseAcceptance,
	PhaseApproved,
	PhaseMergedClosed,
	PhaseRejectedClosed,
	PhaseAbandoned,
}

// AllLifecycleTransitionTargets is the allowlist for POST
// /v2/tasks/{id}/lifecycle/transition. It is the union of Phase, LifecycleState,
// the client-side unscheduled, DoneResolution, and the operational escape hatches
// (reopen/retry/skip/hold/resume). The handler normalizes to lowercase before
// checking, and returns 400 invalid_lifecycle_target with this list on mismatch.
var AllLifecycleTransitionTargets = []string{
	// Phases
	string(PhaseBacklog), string(PhaseTriage), string(PhaseUpNext), string(PhaseWorking),
	string(PhaseCritique), string(PhaseAcceptance), string(PhaseApproved),
	string(PhaseMergedClosed), string(PhaseRejectedClosed), string(PhaseAbandoned),
	// LifecycleStates + client derived unscheduled
	string(LifecycleScheduled), string(LifecycleInProgress), string(LifecycleDone), "unscheduled",
	// Done resolutions (also accepted as bare targets and as done:<resolution>)
	string(ResolutionCompleted), string(ResolutionMerged), string(ResolutionRejected),
	string(ResolutionAbandoned), string(ResolutionCancelled), string(ResolutionFailed),
	// Operational
	"reopen", "retry", "skip", "hold", "pause", "resume", "release", "reset", "schedule", "done",
}

// IsValidLifecycleTarget reports whether target is in the lifecycle transition
// vocabulary. It normalizes to lowercase and accepts both bare resolutions
// ("completed") and the done:<resolution> form ("done:completed").
func IsValidLifecycleTarget(target string) bool {
	normalized := strings.ToLower(strings.TrimSpace(target))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, ":") {
		parts := strings.SplitN(normalized, ":", 2)
		if strings.TrimSpace(parts[0]) == "done" && strings.TrimSpace(parts[1]) != "" {
			return validateDoneResolution(DoneResolution(strings.TrimSpace(parts[1]))) == nil
		}
		return false
	}
	// Bare done resolutions are also valid.
	if validateDoneResolution(DoneResolution(normalized)) == nil {
		return true
	}
	for _, candidate := range AllLifecycleTransitionTargets {
		if candidate == normalized {
			return true
		}
	}
	return false
}

// IsValidDoneResolution reports whether resolution is a known DoneResolution.
func IsValidDoneResolution(resolution DoneResolution) bool {
	return validateDoneResolution(resolution) == nil
}

// PhaseForTask derives the lifecycle phase for an already-loaded task,
// reusing the same disposition logic as the board projection. For a closed
// task this resolves to merged_closed / rejected_closed / abandoned.
func (s *TaskService) PhaseForTask(ctx context.Context, task Task) (Phase, error) {
	return derivePhaseFromTask(ctx, s.db, task)
}

func derivePhaseFromTask(ctx context.Context, db *sql.DB, task Task) (Phase, error) {
	if task.ScheduleState == ScheduleClosed {
		if task.TriageState == TriageRejected {
			return PhaseRejectedClosed, nil
		}
		merged, err := taskHasMergedChange(ctx, db, task.ID)
		if err != nil {
			return "", err
		}
		if merged {
			return PhaseMergedClosed, nil
		}
		return PhaseAbandoned, nil
	}

	reviewState, err := reviewStateForTask(ctx, db, task.ID)
	if err != nil {
		return "", err
	}
	hasChange, err := taskHasUnmergedChange(ctx, db, task.ID)
	if err != nil {
		return "", err
	}
	if hasChange && reviewState == ReviewApproved {
		return PhaseApproved, nil
	}
	if reviewState == ReviewChangesRequested {
		return PhaseCritique, nil
	}
	if _, ok, err := activeSessionStateForTask(ctx, db, task.ID); err != nil {
		return "", err
	} else if ok {
		return PhaseWorking, nil
	}
	if task.TriageState == TriagePending {
		return PhaseTriage, nil
	}
	if task.TriageState != TriageAccepted {
		return PhaseTriage, nil
	}
	if hasChange && reviewState == ReviewInReview {
		// Acceptance is the slice of in-review where every required critique
		// check is satisfied but a verifier check is still pending; otherwise the
		// change is still in critique. This must match the lifecycle engine's
		// derivePhase, which reads the same predicate via Effects.AcceptancePending.
		pending, err := acceptancePendingForTask(ctx, db, task.ID)
		if err != nil {
			return "", err
		}
		if pending {
			return PhaseAcceptance, nil
		}
		return PhaseCritique, nil
	}

	switch task.ScheduleState {
	case ScheduleUpNext:
		return PhaseUpNext, nil
	default:
		return PhaseBacklog, nil
	}
}

// HasMergedChange reports whether the task has any merged change. It is the
// exported reader the lifecycle engine uses to distinguish an abandoned closed
// task (no merge) from a merged_closed one.
func (s *TaskService) HasMergedChange(ctx context.Context, taskID string) (bool, error) {
	return taskHasMergedChange(ctx, s.db, taskID)
}

func taskHasMergedChange(ctx context.Context, db *sql.DB, taskID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM changes
WHERE task_id = ?
	AND merged_at IS NOT NULL`, taskID).Scan(&count); err != nil {
		return false, err
	}

	return count > 0, nil
}
