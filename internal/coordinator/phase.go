package coordinator

import (
	"context"
	"database/sql"
)

// Phase is the explicit, authoritative lifecycle coordinate for an issue. It is
// stored in workflow_state by the lifecycle engine and is a projection of the
// existing state mechanisms (schedule/triage columns, the issue_review_state
// view, change ready/merged latches, and active author sessions).
//
// blocked is deliberately NOT a phase: it remains a derived overlay computed
// live from issue_relations, so a blocked issue still carries its underlying
// phase (backlog/up_next/critique/...) and the block is layered on at read time.
type Phase string

const (
	PhaseBacklog Phase = "backlog"
	PhaseTriage  Phase = "triage"
	PhaseUpNext  Phase = "up_next"
	// PhaseWorking is the container phase for the issue's whole work pipeline:
	// the position within the pipeline (which flow phase, running vs paused at
	// a human gate) lives on the issue's flow cursor, not in workflow_state.
	PhaseWorking        Phase = "working"
	PhaseCritique       Phase = "critique"
	PhaseAcceptance     Phase = "acceptance"
	PhaseApproved       Phase = "approved"
	PhaseMergedClosed   Phase = "merged_closed"
	PhaseRejectedClosed Phase = "rejected_closed"
	PhaseAbandoned      Phase = "abandoned"
)

// PhaseForIssue derives the lifecycle phase for an already-loaded issue,
// reusing the same disposition logic as the board projection. For a closed
// issue this resolves to merged_closed / rejected_closed / abandoned.
func (s *IssueService) PhaseForIssue(ctx context.Context, issue Issue) (Phase, error) {
	return derivePhaseFromIssue(ctx, s.db, issue)
}

func derivePhaseFromIssue(ctx context.Context, db *sql.DB, issue Issue) (Phase, error) {
	if issue.ScheduleState == ScheduleClosed {
		if issue.TriageState == TriageRejected {
			return PhaseRejectedClosed, nil
		}
		merged, err := issueHasMergedChange(ctx, db, issue.ID)
		if err != nil {
			return "", err
		}
		if merged {
			return PhaseMergedClosed, nil
		}
		return PhaseAbandoned, nil
	}

	reviewState, err := reviewStateForIssue(ctx, db, issue.ID)
	if err != nil {
		return "", err
	}
	hasChange, err := issueHasUnmergedChange(ctx, db, issue.ID)
	if err != nil {
		return "", err
	}
	if hasChange && reviewState == ReviewApproved {
		return PhaseApproved, nil
	}
	if reviewState == ReviewChangesRequested {
		return PhaseCritique, nil
	}
	if _, ok, err := activeSessionStateForIssue(ctx, db, issue.ID); err != nil {
		return "", err
	} else if ok {
		return PhaseWorking, nil
	}
	// No active session, but the flow cursor can still pin the issue in
	// working: paused at a human gate, or mid-pipeline between phase jobs.
	// This must match the lifecycle engine's derivePhase.
	if index, state, ok, err := cursorStateForIssue(ctx, db, issue.ID); err != nil {
		return "", err
	} else if ok && cursorIndicatesWorking(index, state) {
		return PhaseWorking, nil
	}
	if issue.TriageState == TriagePending {
		return PhaseTriage, nil
	}
	if issue.TriageState != TriageAccepted {
		return PhaseTriage, nil
	}
	if hasChange && reviewState == ReviewInReview {
		// Acceptance is the slice of in-review where every required critique
		// check is satisfied but a verifier check is still pending; otherwise the
		// change is still in critique. This must match the lifecycle engine's
		// derivePhase, which reads the same predicate via Effects.AcceptancePending.
		pending, err := acceptancePendingForIssue(ctx, db, issue.ID)
		if err != nil {
			return "", err
		}
		if pending {
			return PhaseAcceptance, nil
		}
		return PhaseCritique, nil
	}

	switch issue.ScheduleState {
	case ScheduleUpNext:
		return PhaseUpNext, nil
	default:
		return PhaseBacklog, nil
	}
}

// HasMergedChange reports whether the issue has any merged change. It is the
// exported reader the lifecycle engine uses to distinguish an abandoned closed
// issue (no merge) from a merged_closed one.
func (s *IssueService) HasMergedChange(ctx context.Context, issueID string) (bool, error) {
	return issueHasMergedChange(ctx, s.db, issueID)
}

func issueHasMergedChange(ctx context.Context, db *sql.DB, issueID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM changes
WHERE issue_id = ?
	AND merged_at IS NOT NULL`, issueID).Scan(&count); err != nil {
		return false, err
	}

	return count > 0, nil
}
