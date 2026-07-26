package coordinator

import (
	"context"
	"database/sql"
)

// Phase is the explicit, authoritative lifecycle coordinate for an task. It is
// stored in workflow_state by the lifecycle engine and is a projection of the
// existing state mechanisms (schedule/triage columns, the task_review_state
// view, change ready/merged latches, and active author sessions).
//
// blocked is deliberately NOT a phase: it remains a derived overlay computed
// live from task_relations, so a blocked task still carries its underlying
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
