package lifecycle

import (
	"context"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func twoPhaseCursor(issueID string, gate coordinator.FlowGate) coordinator.FlowCursor {
	return coordinator.FlowCursor{
		IssueID: issueID,
		Snapshot: coordinator.FlowSnapshot{
			FlowID:   "fl-test",
			FlowName: "spec-then-implement",
			Phases: []coordinator.FlowPhaseSnapshot{
				{Name: "spec", Gate: gate, Agent: coordinator.AgentDefSnapshot{Name: "planner", Harness: "codex"}},
				{Name: "implement", Gate: coordinator.FlowGateAuto, Agent: coordinator.AgentDefSnapshot{Name: "author", Harness: "claude"}},
			},
		},
		PhaseIndex: 0,
		PhaseState: coordinator.FlowPhasePending,
	}
}

// TestWorkPhaseAutoAdvance is the regression for the composable-flow core: an
// intermediate auto phase's flow ready stores the phase handoff, advances the
// cursor, finishes the session WITHOUT publishing the change, and ensures the
// next phase's job — leaving the issue in the working phase.
func TestWorkPhaseAutoAdvance(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateAuto)
	fake.changeHandoff = coordinator.HandoffSnapshot{ChangeID: "c1", Present: true, Content: "# Spec", HeadSHA: ""}
	fake.hasChangeHandoff = true

	res, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1"})
	if err != nil {
		t.Fatalf("step: %v", err)
	}

	if fake.cursor.PhaseIndex != 1 || fake.cursor.PhaseState != coordinator.FlowPhasePending {
		t.Fatalf("cursor = %d/%s, want 1/pending", fake.cursor.PhaseIndex, fake.cursor.PhaseState)
	}
	if handoff := fake.phaseHandoffs[0]; handoff.Content != "# Spec" || handoff.PhaseName != "spec" {
		t.Fatalf("phase handoff = %+v, want stored spec handoff", fake.phaseHandoffs[0])
	}
	assertOrder(t, fake.calls, "StorePhaseHandoff", "AdvanceFlowCursor", "FinishWorkPhaseSession", "EnsureAuthorJob")
	if fake.called("ReadyAuthorSession") || fake.called("ReadyChange") {
		t.Fatalf("intermediate phase published the change: calls %v", fake.calls)
	}
	if fake.called("ScheduleReviewRound") {
		t.Fatalf("intermediate phase scheduled a review round: calls %v", fake.calls)
	}
	if res.ToPhase != coordinator.PhaseWorking {
		t.Fatalf("ToPhase = %q, want working", res.ToPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseWorking {
		t.Fatalf("phase = %q, want working", currentPhase(t, store, issueID))
	}
}

// TestWorkPhaseHumanGatePauses: a human-gated phase's flow ready parks the
// cursor awaiting approval without enqueueing the next phase.
func TestWorkPhaseHumanGatePauses(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateHuman)
	fake.changeHandoff = coordinator.HandoffSnapshot{ChangeID: "c1", Present: true, Content: "# Plan"}
	fake.hasChangeHandoff = true

	if _, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1"}); err != nil {
		t.Fatalf("step: %v", err)
	}

	if fake.cursor.PhaseIndex != 0 || fake.cursor.PhaseState != coordinator.FlowPhaseAwaitingApproval {
		t.Fatalf("cursor = %d/%s, want 0/awaiting_approval", fake.cursor.PhaseIndex, fake.cursor.PhaseState)
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("gate pause must not enqueue the next phase: calls %v", fake.calls)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseWorking {
		t.Fatalf("phase = %q, want working while awaiting approval", currentPhase(t, store, issueID))
	}
}

// TestGateApproveAdvances: approving a paused intermediate phase advances the
// cursor and ensures the next phase's job.
func TestGateApproveAdvances(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateHuman)
	fake.cursor.PhaseState = coordinator.FlowPhaseAwaitingApproval

	res, err := eng.Step(ctx, Event{Kind: EventWorkPhaseApproved, IssueID: issueID})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.cursor.PhaseIndex != 1 || fake.cursor.PhaseState != coordinator.FlowPhasePending {
		t.Fatalf("cursor = %d/%s, want 1/pending", fake.cursor.PhaseIndex, fake.cursor.PhaseState)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("approve must ensure the next phase's job: calls %v", fake.calls)
	}
	if res.ToPhase != coordinator.PhaseWorking {
		t.Fatalf("ToPhase = %q, want working", res.ToPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseWorking {
		t.Fatalf("phase = %q, want working", currentPhase(t, store, issueID))
	}
}

// TestGateReworkRerunsSamePhase: request-changes returns the paused phase to
// pending with the feedback stored, and re-ensures its job.
func TestGateReworkRerunsSamePhase(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateHuman)
	fake.cursor.PhaseState = coordinator.FlowPhaseAwaitingApproval

	if _, err := eng.Step(ctx, Event{Kind: EventWorkPhaseRework, IssueID: issueID, Payload: EventPayload{GateFeedback: "cover the edge cases"}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.cursor.PhaseIndex != 0 || fake.cursor.PhaseState != coordinator.FlowPhasePending {
		t.Fatalf("cursor = %d/%s, want 0/pending", fake.cursor.PhaseIndex, fake.cursor.PhaseState)
	}
	if fake.cursor.GateFeedback != "cover the edge cases" {
		t.Fatalf("gate feedback = %q, want stored", fake.cursor.GateFeedback)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("rework must re-ensure the phase's job: calls %v", fake.calls)
	}
}

// TestGateDecisionWithoutPauseIsBenignNoOp: an approve landing after the
// cursor already moved on (double click, stale UI) declines at the guard.
func TestGateDecisionWithoutPauseIsBenignNoOp(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateHuman)
	fake.cursor.PhaseIndex = 1
	fake.cursor.PhaseState = coordinator.FlowPhasePending

	res, err := eng.Step(ctx, Event{Kind: EventWorkPhaseApproved, IssueID: issueID})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Transitioned {
		t.Fatalf("stale approve transitioned: %+v", res)
	}
	if fake.cursor.PhaseIndex != 1 || fake.called("EnsureAuthorJob") {
		t.Fatalf("stale approve mutated state: cursor=%d calls=%v", fake.cursor.PhaseIndex, fake.calls)
	}
}

// TestFinalPhaseReadyRunsReadyTail: the last (auto) phase's flow ready runs
// the existing publish tail and completes the cursor.
func TestFinalPhaseReadyRunsReadyTail(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "old"}
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewInReview
	fake.hasCursor = true
	cursor := twoPhaseCursor(issueID, coordinator.FlowGateAuto)
	cursor.PhaseIndex = 1 // on "implement", the final phase
	fake.cursor = cursor
	fake.changeHandoff = coordinator.HandoffSnapshot{ChangeID: "c1", Present: true, Content: "# Done", HeadSHA: "new"}
	fake.hasChangeHandoff = true

	if _, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1", Payload: EventPayload{HeadSHA: "new"}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	assertOrder(t, fake.calls, "ReadyAuthorSession", "UpdateChangeHead", "ResetAutomatedChecksForNewRevision", "ScheduleReviewRound")
	if fake.cursor.PhaseState != coordinator.FlowPhaseCompleted {
		t.Fatalf("cursor state = %q, want completed", fake.cursor.PhaseState)
	}
	if handoff := fake.phaseHandoffs[1]; handoff.PhaseName != "implement" {
		t.Fatalf("final phase handoff = %+v, want stored implement handoff", fake.phaseHandoffs[1])
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseCritique {
		t.Fatalf("phase = %q, want critique", currentPhase(t, store, issueID))
	}
}

// TestFinalPhaseHumanGateApprovePublishes: a human-gated FINAL phase pauses at
// ready; approval publishes the change from the stored handoff head and
// schedules the first review round.
func TestFinalPhaseHumanGateApprovePublishes(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "old"}
	fake.hasCursor = true
	fake.cursor = coordinator.FlowCursor{
		IssueID: issueID,
		Snapshot: coordinator.FlowSnapshot{
			FlowID:   "fl-gated",
			FlowName: "gated-implement",
			Phases: []coordinator.FlowPhaseSnapshot{
				{Name: "implement", Gate: coordinator.FlowGateHuman, Agent: coordinator.AgentDefSnapshot{Name: "author", Harness: "codex"}},
			},
		},
		PhaseIndex: 0,
		PhaseState: coordinator.FlowPhasePending,
	}
	fake.changeHandoff = coordinator.HandoffSnapshot{ChangeID: "c1", Present: true, Content: "# Done", HeadSHA: "new"}
	fake.hasChangeHandoff = true

	if _, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1", Payload: EventPayload{HeadSHA: "new"}}); err != nil {
		t.Fatalf("ready step: %v", err)
	}
	if fake.cursor.PhaseState != coordinator.FlowPhaseAwaitingApproval {
		t.Fatalf("cursor state after ready = %q, want awaiting_approval", fake.cursor.PhaseState)
	}
	if fake.change.ReadyAt != nil {
		t.Fatalf("gated final ready published the change early: %+v", fake.change)
	}

	fake.reviewState = coordinator.ReviewInReview
	if _, err := eng.Step(ctx, Event{Kind: EventWorkPhaseApproved, IssueID: issueID}); err != nil {
		t.Fatalf("approve step: %v", err)
	}
	assertOrder(t, fake.calls, "LatestChangeForIssue", "ReadyChange", "UpdateChangeHead", "ResetAutomatedChecksForNewRevision", "ScheduleReviewRound")
	if fake.change.ReadyAt == nil || fake.change.HeadSHA != "new" {
		t.Fatalf("change after approve = %+v, want ready at handoff head", fake.change)
	}
	if fake.cursor.PhaseState != coordinator.FlowPhaseCompleted {
		t.Fatalf("cursor state after approve = %q, want completed", fake.cursor.PhaseState)
	}
	if currentPhase(t, store, issueID) == coordinator.PhaseWorking {
		t.Fatalf("phase stayed working after final approve")
	}
}

// TestDuplicateReadyAfterPhaseCompletionIsNoOp: a second flow ready from an
// already-finished session must not advance the cursor again.
func TestDuplicateReadyAfterPhaseCompletionIsNoOp(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionFinished}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.hasCursor = true
	cursor := twoPhaseCursor(issueID, coordinator.FlowGateAuto)
	cursor.PhaseIndex = 1 // already advanced by the first ready
	fake.cursor = cursor

	if _, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1"}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.cursor.PhaseIndex != 1 {
		t.Fatalf("duplicate ready advanced the cursor to %d", fake.cursor.PhaseIndex)
	}
	if fake.called("AdvanceFlowCursor") || fake.called("StorePhaseHandoff") {
		t.Fatalf("duplicate ready re-ran completion effects: calls %v", fake.calls)
	}
}

// TestWorkPhaseDeadlineIgnoresGatePause: the dwell-window escalation must not
// fire while the flow is deliberately paused at a human gate.
func TestWorkPhaseDeadlineIgnoresGatePause(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: 1})
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasCursor = true
	fake.cursor = twoPhaseCursor(issueID, coordinator.FlowGateHuman)
	fake.cursor.PhaseState = coordinator.FlowPhaseAwaitingApproval

	if _, err := eng.Step(ctx, Event{Kind: EventPhaseDeadline, IssueID: issueID, Payload: EventPayload{DeadlinePhase: coordinator.PhaseWorking}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("ReportCheck") || len(fake.statusWrites) != 0 {
		t.Fatalf("gate pause escalated as a stall: calls %v statuses %v", fake.calls, fake.statusWrites)
	}
}

// TestEnsureWorkPhaseJobFreezesCursorOnFirstUse: scheduling an issue up next
// freezes the flow cursor (snapshot-at-schedule) before enqueueing the job.
func TestEnsureWorkPhaseJobFreezesCursorOnFirstUse(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	template := twoPhaseCursor(issueID, coordinator.FlowGateAuto)
	fake.cursorTemplate = &template

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.hasCursor {
		t.Fatalf("schedule did not freeze the flow cursor")
	}
	assertOrder(t, fake.calls, "ScheduleIssue", "EnsureFlowCursor", "EnsureAuthorJob")
}
