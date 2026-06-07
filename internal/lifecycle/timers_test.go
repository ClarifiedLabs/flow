package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestDrainDueTimersFiresAndLatches(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	if _, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(-time.Minute), EventPayload{}); err != nil {
		t.Fatalf("schedule timer: %v", err)
	}

	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("timer did not dispatch EnsureAuthorJob: %v", fake.calls)
	}

	// dispatched_at confirm: a second drain is a no-op even though the event
	// itself is not inherently idempotent.
	fired2, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain2: %v", err)
	}
	if fired2 != 0 {
		t.Fatalf("second drain fired = %d, want 0", fired2)
	}
}

func TestDrainDueTimersRedeliversAfterCrashBeforeConfirm(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	timerID, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(-time.Minute), EventPayload{})
	if err != nil {
		t.Fatalf("schedule timer: %v", err)
	}

	// Simulate a crash after the timer's Step committed but before the confirm
	// update: the transition (keyed "timer:<id>") exists, dispatched_at is NULL.
	if _, err := eng.Step(ctx, Event{Kind: EventEnsureAuthorJob, IssueID: issueID, IdempotencyKey: "timer:" + timerID}); err != nil {
		t.Fatalf("pre-commit step: %v", err)
	}
	if got := countCalls(fake.calls, "EnsureAuthorJob"); got != 1 {
		t.Fatalf("EnsureAuthorJob calls = %d, want 1 before redelivery", got)
	}

	// Redelivery must dedupe via the idempotency key (no second effect) and
	// confirm the timer.
	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if got := countCalls(fake.calls, "EnsureAuthorJob"); got != 1 {
		t.Fatalf("EnsureAuthorJob calls = %d, want 1 after redelivery (replay must be a no-op)", got)
	}
	var dispatched *string
	if err := store.DB().QueryRow(`SELECT dispatched_at FROM timers WHERE id = ?`, timerID).Scan(&dispatched); err != nil {
		t.Fatalf("read timer: %v", err)
	}
	if dispatched == nil {
		t.Fatalf("timer not confirmed after redelivery")
	}
}

func TestDrainDueTimersIsolatesPoisonTimer(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	// A poison timer (corrupt payload) due before the good one.
	if _, err := store.DB().Exec(`
INSERT INTO timers (id, issue_id, fire_at, kind, payload_json, fired_at)
VALUES ('tm-poison', ?, '2020-01-01T00:00:00Z', 'ensure_author_job', '{', NULL)`, issueID); err != nil {
		t.Fatalf("seed poison timer: %v", err)
	}
	goodID, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(-time.Minute), EventPayload{})
	if err != nil {
		t.Fatalf("schedule timer: %v", err)
	}

	fired, err := eng.DrainDueTimers(ctx)
	if err == nil {
		t.Fatalf("drain succeeded, want joined error from the poison timer")
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (the good timer must still dispatch)", fired)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("good timer did not dispatch despite poison sibling")
	}

	var attempts int
	var lastError string
	var dispatched *string
	if err := store.DB().QueryRow(`SELECT attempts, last_error, dispatched_at FROM timers WHERE id = 'tm-poison'`).Scan(&attempts, &lastError, &dispatched); err != nil {
		t.Fatalf("read poison timer: %v", err)
	}
	// A poison payload can never be made to parse, so the timer is parked
	// (confirmed with its error preserved) instead of retried every tick.
	if attempts != 1 || lastError == "" || dispatched == nil {
		t.Fatalf("poison timer attempts=%d lastError=%q dispatched=%v, want 1/non-empty/parked", attempts, lastError, dispatched)
	}
	if err := store.DB().QueryRow(`SELECT dispatched_at FROM timers WHERE id = ?`, goodID).Scan(&dispatched); err != nil {
		t.Fatalf("read good timer: %v", err)
	}
	if dispatched == nil {
		t.Fatalf("good timer not confirmed")
	}

	// The parked poison timer no longer surfaces on subsequent drains.
	fired, err = eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("second drain still errors on parked poison timer: %v", err)
	}
	if fired != 0 {
		t.Fatalf("second drain fired = %d, want 0", fired)
	}
}

func TestDrainDueTimersConfirmsStaleTimerWithNoCandidateEdge(t *testing.T) {
	eng, _, store, issueID := newEngineTest(t)
	ctx := context.Background()

	// An event kind with no transition row at all: the issue has moved on and
	// the timer is stale. It must confirm (not retry forever) without erroring.
	timerID, err := eng.ScheduleTimer(ctx, issueID, EventKind("bogus_kind"), eng.now().Add(-time.Minute), EventPayload{})
	if err != nil {
		t.Fatalf("schedule timer: %v", err)
	}
	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	var dispatched *string
	if err := store.DB().QueryRow(`SELECT dispatched_at FROM timers WHERE id = ?`, timerID).Scan(&dispatched); err != nil {
		t.Fatalf("read timer: %v", err)
	}
	if dispatched == nil {
		t.Fatalf("stale timer not confirmed")
	}
}

func TestTickRunsRecoveryDespiteDrainError(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	if _, err := store.DB().Exec(`
INSERT INTO timers (id, issue_id, fire_at, kind, payload_json, fired_at)
VALUES ('tm-poison', ?, '2020-01-01T00:00:00Z', 'ensure_author_job', '{', NULL)`, issueID); err != nil {
		t.Fatalf("seed poison timer: %v", err)
	}

	if err := eng.Tick(ctx); err == nil {
		t.Fatalf("tick succeeded, want the poison timer's error surfaced")
	}
	if !fake.called("ReconcileCrashedAuthorSessions") || !fake.called("RecoverPendingCheckJobs") {
		t.Fatalf("recovery skipped because the drain errored: %v", fake.calls)
	}
}

// rewindPendingTimers makes every undispatched timer immediately due.
func rewindPendingTimers(t *testing.T, store *flowdb.Store) {
	t.Helper()
	if _, err := store.DB().Exec(`UPDATE timers SET fire_at = '2020-01-01T00:00:00Z' WHERE dispatched_at IS NULL`); err != nil {
		t.Fatalf("rewind timers: %v", err)
	}
}

func pendingTimerCount(t *testing.T, store *flowdb.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM timers WHERE dispatched_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count pending timers: %v", err)
	}
	return n
}

// approveForAutoMerge puts the fake into the state guardAutoMergeReady accepts.
func approveForAutoMerge(fake *fakeEffects) {
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.issue.AutoMerge = true
	fake.reviewState = coordinator.ReviewApproved
	fake.hasReady = true
}

func TestAutoMergeTransientFailureSchedulesRetryTimer(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	approveForAutoMerge(fake)
	fake.failOn["MergeIssue"] = errors.New("exchange unavailable")

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "unit", CheckKind: coordinator.CheckKindCI, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if len(res.FollowUpFailures) != 1 || res.FollowUpFailures[0].EventKind != EventAutoMerge {
		t.Fatalf("follow-up failures = %+v, want one auto_merge entry", res.FollowUpFailures)
	}

	var kind, payload string
	if err := store.DB().QueryRow(
		`SELECT kind, payload_json FROM timers WHERE dispatched_at IS NULL`).Scan(&kind, &payload); err != nil {
		t.Fatalf("expected one pending retry timer: %v", err)
	}
	if kind != string(EventAutoMerge) || !strings.Contains(payload, `"auto_merge_attempt":1`) {
		t.Fatalf("retry timer kind=%q payload=%q, want auto_merge attempt 1", kind, payload)
	}
}

func TestAutoMergeRetryTimerRemerges(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	approveForAutoMerge(fake)
	fake.failOn["MergeIssue"] = errors.New("exchange unavailable")

	if _, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "unit", CheckKind: coordinator.CheckKindCI, Verdict: coordinator.CheckSatisfied,
	}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	delete(fake.failOn, "MergeIssue")
	rewindPendingTimers(t, store)

	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if got := countCalls(fake.calls, "MergeIssue"); got != 2 {
		t.Fatalf("MergeIssue calls = %d, want 2 (original + retry)", got)
	}
	if !fake.merged {
		t.Fatal("retry did not merge the issue")
	}
	if n := pendingTimerCount(t, store); n != 0 {
		t.Fatalf("pending timers = %d, want 0 after successful retry", n)
	}
}

func TestAutoMergeRetryExhaustionReportsBlockedCheck(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	approveForAutoMerge(fake)
	fake.failOn["MergeIssue"] = errors.New("exchange unavailable")

	if _, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "unit", CheckKind: coordinator.CheckKindCI, Verdict: coordinator.CheckSatisfied,
	}}); err != nil {
		t.Fatalf("step: %v", err)
	}

	// Drain each scheduled retry until the attempt budget runs out.
	for i := 0; i < maxAutoMergeAttempts-1; i++ {
		rewindPendingTimers(t, store)
		if _, err := eng.DrainDueTimers(ctx); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}

	if got := countCalls(fake.calls, "MergeIssue"); got != maxAutoMergeAttempts {
		t.Fatalf("MergeIssue calls = %d, want %d", got, maxAutoMergeAttempts)
	}
	if n := pendingTimerCount(t, store); n != 0 {
		t.Fatalf("pending timers = %d, want 0 after exhaustion", n)
	}
	var exhaustion *coordinator.ReportCheckInput
	for i := range fake.reported {
		if fake.reported[i].Name == coordinator.AutoMergeCheckName &&
			strings.HasPrefix(fake.reported[i].Details, coordinator.AutoMergeTransientDetailsPrefix) {
			exhaustion = &fake.reported[i]
		}
	}
	if exhaustion == nil {
		t.Fatalf("no transient-exhaustion auto-merge check reported: %+v", fake.reported)
	}
	if exhaustion.Verdict != coordinator.CheckBlocked || exhaustion.Required == nil || !*exhaustion.Required {
		t.Fatalf("exhaustion check = %+v, want required+blocked", exhaustion)
	}
}

func TestAutoMergeRetryNotDuplicatedOnCrashRedelivery(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	approveForAutoMerge(fake)
	fake.failOn["MergeIssue"] = errors.New("exchange unavailable")

	// A retry timer fires, fails transiently, and schedules its successor.
	timerID, err := eng.ScheduleTimer(ctx, issueID, EventAutoMerge, eng.now().Add(-time.Minute), EventPayload{AutoMergeAttempt: 1})
	if err != nil {
		t.Fatalf("schedule timer: %v", err)
	}
	if _, err := eng.DrainDueTimers(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := pendingTimerCount(t, store); n != 1 {
		t.Fatalf("pending timers after failed dispatch = %d, want 1 (the successor)", n)
	}

	// Simulate a crash between the successor's INSERT and the dispatching
	// timer's dedup transition + confirm: the successor survived, the rest
	// was lost.
	if _, err := store.DB().Exec(`DELETE FROM transitions WHERE idempotency_key = ?`, "timer:"+timerID); err != nil {
		t.Fatalf("erase dedup transition: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE timers SET dispatched_at = NULL WHERE id = ?`, timerID); err != nil {
		t.Fatalf("unconfirm timer: %v", err)
	}

	// Redelivery re-runs the action, but must NOT fork a second retry chain.
	if _, err := eng.DrainDueTimers(ctx); err != nil {
		t.Fatalf("redelivery drain: %v", err)
	}
	if n := pendingTimerCount(t, store); n != 1 {
		t.Fatalf("pending timers after crash redelivery = %d, want 1 (no duplicate chain)", n)
	}
}

func TestAutoMergeRetryAfterMergeIsBenignNoop(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	approveForAutoMerge(fake)
	fake.failOn["MergeIssue"] = errors.New("exchange unavailable")

	if _, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "unit", CheckKind: coordinator.CheckKindCI, Verdict: coordinator.CheckSatisfied,
	}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	// The issue merges through some other path before the retry fires.
	fake.reviewState = coordinator.ReviewMerged
	fake.merged = true
	rewindPendingTimers(t, store)

	before := countCalls(fake.calls, "MergeIssue")
	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (stale retry must confirm)", fired)
	}
	if got := countCalls(fake.calls, "MergeIssue"); got != before {
		t.Fatalf("MergeIssue calls grew from %d to %d on a merged issue", before, got)
	}
	if n := pendingTimerCount(t, store); n != 0 {
		t.Fatalf("pending timers = %d, want 0", n)
	}
}

func TestRunRecoveryContinuesPastFailingStep(t *testing.T) {
	eng, fake, _, _ := newEngineTest(t)
	ctx := context.Background()
	fake.failOn["ReconcileCrashedAuthorSessions"] = errors.New("boom")

	if _, err := eng.RunRecovery(ctx); err == nil {
		t.Fatalf("recovery succeeded, want joined error")
	}
	if !fake.called("RecoverPendingCheckJobs") {
		t.Fatalf("check-job recovery skipped after session recovery failed: %v", fake.calls)
	}
}

func TestDrainDueTimersSkipsFutureTimers(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted

	if _, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(time.Hour), EventPayload{}); err != nil {
		t.Fatalf("schedule timer: %v", err)
	}
	fired, err := eng.DrainDueTimers(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (timer is in the future)", fired)
	}
}

func TestRefreshSessionPhasesRederivesStalePhase(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	// Seed a stale authoring phase (as if a now-crashed session left it behind).
	if _, err := store.DB().Exec(`INSERT INTO workflow_state (issue_id, phase, version, updated_at) VALUES (?, 'authoring', 3, ?)`,
		issueID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed workflow_state: %v", err)
	}
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasActive = false // no live session => derive leaves authoring

	if err := eng.refreshSessionPhases(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := currentPhase(t, store, issueID); got != coordinator.PhaseUpNext {
		t.Fatalf("phase = %q, want up_next after refresh", got)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM transitions WHERE issue_id = ? AND event_kind = 'reconcile'`, issueID).Scan(&n); err != nil {
		t.Fatalf("count reconcile transitions: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconcile transitions = %d, want 1", n)
	}
}

func TestTickDrainsTimersAndRecovers(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	if _, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(-time.Minute), EventPayload{}); err != nil {
		t.Fatalf("schedule timer: %v", err)
	}
	if err := eng.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("tick did not drain the timer")
	}
	if !fake.called("ReconcileCrashedAuthorSessions") {
		t.Fatalf("tick did not run crash recovery")
	}
	if !fake.called("RecoverPendingCheckJobs") {
		t.Fatalf("tick did not recover pending check jobs")
	}
	if !fake.called("RecoverPendingMerges") {
		t.Fatalf("tick did not recover pending merges")
	}
}

// --- Phase-deadline and check-timeout timers (Task 9) ----------------------

const (
	testAuthoringStall = 2 * time.Hour
	testCheckPending   = 30 * time.Minute
)

// pendingTimerKindCount counts undispatched timers of a given kind.
func pendingTimerKindCount(t *testing.T, store *flowdb.Store, kind EventKind) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM timers WHERE kind = ? AND dispatched_at IS NULL`, string(kind)).Scan(&n); err != nil {
		t.Fatalf("count pending %s timers: %v", kind, err)
	}
	return n
}

// enterAuthoring drives the issue into the authoring phase through the engine
// (a working author session), so the post-commit phase-deadline hook runs.
func enterAuthoring(t *testing.T, eng *Engine, fake *fakeEffects, issueID string) {
	t.Helper()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	res, err := eng.Step(context.Background(), Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWorking}})
	if err != nil {
		t.Fatalf("enter authoring: %v", err)
	}
	if res.ToPhase != coordinator.PhaseAuthoring {
		t.Fatalf("ToPhase = %q, want authoring", res.ToPhase)
	}
}

// TestEnteringAuthoringSchedulesPhaseDeadline covers numbered case 1: a
// phase-changing transition into authoring arms exactly one phase_deadline
// timer, and re-entering the same phase does not duplicate it.
func TestEnteringAuthoringSchedulesPhaseDeadline(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: testAuthoringStall})

	enterAuthoring(t, eng, fake, issueID)
	if n := pendingTimerKindCount(t, store, EventPhaseDeadline); n != 1 {
		t.Fatalf("phase_deadline timers = %d, want 1 after entering authoring", n)
	}

	// A redundant working report keeps the phase authoring (no phase change);
	// even a phase flip back to authoring must not spawn a second timer while
	// one is still pending.
	if _, err := eng.Step(context.Background(), Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWaiting}}); err != nil {
		t.Fatalf("flip to waiting: %v", err)
	}
	if _, err := eng.Step(context.Background(), Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWorking}}); err != nil {
		t.Fatalf("flip back to working: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventPhaseDeadline); n != 1 {
		t.Fatalf("phase_deadline timers = %d, want 1 (no duplicate while pending)", n)
	}
}

// TestPhaseDeadlineStaleActivityEscalates covers numbered case 2: firing the
// phase deadline with stale agent activity reports the non-required blocked
// phase-deadline check and writes a blocker status entry.
func TestPhaseDeadlineStaleActivityEscalates(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: testAuthoringStall})
	enterAuthoring(t, eng, fake, issueID)

	// The active session's last activity is older than the stall window.
	fake.hasActiveSession = true
	stale := eng.now().Add(-3 * time.Hour)
	fake.lastAgentActivity = &stale

	rewindPendingTimers(t, store)
	if _, err := eng.DrainDueTimers(ctx()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	var escalation *coordinator.ReportCheckInput
	for i := range fake.reported {
		if fake.reported[i].Name == phaseDeadlineCheckName {
			escalation = &fake.reported[i]
		}
	}
	if escalation == nil {
		t.Fatalf("phase-deadline check not reported: %+v", fake.reported)
	}
	if escalation.Verdict != coordinator.CheckBlocked || escalation.Required == nil || *escalation.Required {
		t.Fatalf("phase-deadline check = %+v, want non-required blocked", escalation)
	}
	if len(fake.statusWrites) != 1 || fake.statusWrites[0].Kind != coordinator.StatusKindBlocker {
		t.Fatalf("status writes = %+v, want one blocker", fake.statusWrites)
	}
}

// TestPhaseDeadlineFreshActivityReschedules covers numbered case 3: firing with
// fresh agent activity reschedules a new undispatched phase_deadline timer and
// reports no check.
func TestPhaseDeadlineFreshActivityReschedules(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: testAuthoringStall})
	enterAuthoring(t, eng, fake, issueID)

	// The agent was active well within the stall window.
	fake.hasActiveSession = true
	fresh := eng.now().Add(-time.Minute)
	fake.lastAgentActivity = &fresh

	rewindPendingTimers(t, store)
	reportedBefore := len(fake.reported)
	if _, err := eng.DrainDueTimers(ctx()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(fake.reported) != reportedBefore {
		t.Fatalf("a check was reported on fresh activity: %+v", fake.reported[reportedBefore:])
	}
	if len(fake.statusWrites) != 0 {
		t.Fatalf("status written on fresh activity: %+v", fake.statusWrites)
	}
	if n := pendingTimerKindCount(t, store, EventPhaseDeadline); n != 1 {
		t.Fatalf("phase_deadline timers = %d, want 1 (rescheduled)", n)
	}
}

// TestWaitingSessionKeepsAuthoringPhase covers the wait overlay model: a
// waiting author session still belongs to the authoring workflow phase, so the
// existing authoring dwell timer remains the relevant deadline.
func TestWaitingSessionKeepsAuthoringPhase(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: testAuthoringStall})
	enterAuthoring(t, eng, fake, issueID)

	res, err := eng.Step(ctx(), Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWaiting}})
	if err != nil {
		t.Fatalf("flip to waiting: %v", err)
	}
	if res.ToPhase != coordinator.PhaseAuthoring {
		t.Fatalf("ToPhase = %q, want authoring", res.ToPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseAuthoring {
		t.Fatalf("phase = %q, want authoring", currentPhase(t, store, issueID))
	}
	if n := pendingTimerKindCount(t, store, EventPhaseDeadline); n != 1 {
		t.Fatalf("phase_deadline timers = %d, want 1", n)
	}
}

// TestReadySchedulesCheckTimeoutsAndFires covers numbered case 5: readying a
// change arms a check_timeout per scheduled check; firing with the check still
// pending reports it blocked "timed out" and triggers the fix follow-up, and
// firing after the check reported satisfied is a no-op.
func TestReadySchedulesCheckTimeoutsAndFires(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1"}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "old"}
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewInReview
	fake.scheduledNames = []string{"reviewer"}

	if _, err := eng.Step(ctx(), Event{Kind: EventSessionReady, SessionID: "s1", Payload: EventPayload{HeadSHA: "new"}}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 1 {
		t.Fatalf("check_timeout timers = %d, want 1", n)
	}

	// The timer was armed for head "new"; the issue's current ready change must
	// match for the guard to pass.
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "new"}

	// Seed the named check as still pending and make the issue eligible for a fix.
	if fake.checks == nil {
		fake.checks = map[string]coordinator.Check{}
	}
	fake.checks["reviewer"] = coordinator.Check{IssueID: issueID, Name: "reviewer", Kind: coordinator.CheckKindReviewer, Required: true, Verdict: coordinator.CheckPending}

	rewindPendingTimers(t, store)
	if _, err := eng.DrainDueTimers(ctx()); err != nil {
		t.Fatalf("drain timeout: %v", err)
	}

	var timedOut *coordinator.ReportCheckInput
	for i := range fake.reported {
		if fake.reported[i].Name == "reviewer" && fake.reported[i].Verdict == coordinator.CheckBlocked {
			timedOut = &fake.reported[i]
		}
	}
	if timedOut == nil {
		t.Fatalf("timed-out check not reported blocked: %+v", fake.reported)
	}
	if !strings.Contains(timedOut.Details, "timed out") {
		t.Fatalf("timeout details = %q, want 'timed out'", timedOut.Details)
	}
	if timedOut.Required == nil || !*timedOut.Required {
		t.Fatalf("timeout must preserve requiredness (required=true), got %+v", timedOut.Required)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("timed-out required check did not trigger the fix follow-up: %v", fake.calls)
	}
}

// TestCheckTimeoutAfterReportIsNoop covers the second half of numbered case 5:
// once the check has reported (satisfied), the timeout guard declines and the
// timer confirms without re-reporting.
func TestCheckTimeoutAfterReportIsNoop(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})
	fake.issue.TriageState = coordinator.TriageAccepted

	if fake.checks == nil {
		fake.checks = map[string]coordinator.Check{}
	}
	fake.checks["reviewer"] = coordinator.Check{IssueID: issueID, Name: "reviewer", Kind: coordinator.CheckKindReviewer, Verdict: coordinator.CheckSatisfied}

	if _, err := eng.ScheduleTimer(ctx(), issueID, EventCheckTimeout, eng.now().Add(-time.Minute), EventPayload{Name: "reviewer"}); err != nil {
		t.Fatalf("schedule timeout: %v", err)
	}
	reportedBefore := len(fake.reported)
	fired, err := eng.DrainDueTimers(ctx())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (stale timeout confirms)", fired)
	}
	if len(fake.reported) != reportedBefore {
		t.Fatalf("a satisfied check was re-reported on timeout: %+v", fake.reported[reportedBefore:])
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 0 {
		t.Fatalf("check_timeout timers = %d, want 0 (confirmed)", n)
	}
}

// TestCheckTimeoutStaleHeadDoesNotFireAgainstRereadyCheck is the regression for
// the stale-timer bug: checks are keyed (issue, name) and a new revision resets
// the same row back to pending, so a timer armed at an old head must NOT fire
// against the restarted check. The old-head timer is declined (confirms) and the
// new-head timer governs the restarted check.
func TestCheckTimeoutStaleHeadDoesNotFireAgainstRereadyCheck(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasReady = true

	// T1 was armed for head A; the check then restarted on head B (reset back to
	// pending) and T2 was armed for head B. The issue's current ready change is B.
	t1, err := eng.ScheduleTimer(ctx(), issueID, EventCheckTimeout, eng.now().Add(-time.Minute), EventPayload{Name: "ci", HeadSHA: "headA"})
	if err != nil {
		t.Fatalf("arm T1: %v", err)
	}
	t2, err := eng.ScheduleTimer(ctx(), issueID, EventCheckTimeout, eng.now().Add(testCheckPending), EventPayload{Name: "ci", HeadSHA: "headB"})
	if err != nil {
		t.Fatalf("arm T2: %v", err)
	}
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "headB"}
	if fake.checks == nil {
		fake.checks = map[string]coordinator.Check{}
	}
	fake.checks["ci"] = coordinator.Check{IssueID: issueID, Name: "ci", Kind: coordinator.CheckKindCI, Required: true, Verdict: coordinator.CheckPending}

	// Fire only T1 (head A). The guard must decline on the head mismatch: no
	// blocked report, no fix job, and the check stays pending.
	fired, err := eng.DrainDueTimers(ctx())
	if err != nil {
		t.Fatalf("drain T1: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (stale T1 confirms)", fired)
	}
	for _, r := range fake.reported {
		if r.Verdict == coordinator.CheckBlocked {
			t.Fatalf("stale-head timer falsely reported blocked: %+v", r)
		}
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("stale-head timer spawned a spurious fix job: %v", fake.calls)
	}
	if got := fake.checks["ci"].Verdict; got != coordinator.CheckPending {
		t.Fatalf("check verdict = %q, want still pending after stale T1", got)
	}
	var t1Dispatched *string
	if err := store.DB().QueryRow(`SELECT dispatched_at FROM timers WHERE id = ?`, t1).Scan(&t1Dispatched); err != nil {
		t.Fatalf("read T1: %v", err)
	}
	if t1Dispatched == nil {
		t.Fatalf("stale T1 was not confirmed")
	}

	// Now fire T2 (head B, the current head): the timeout legitimately applies.
	if _, err := store.DB().Exec(`UPDATE timers SET fire_at = '2020-01-01T00:00:00Z' WHERE id = ?`, t2); err != nil {
		t.Fatalf("rewind T2: %v", err)
	}
	if _, err := eng.DrainDueTimers(ctx()); err != nil {
		t.Fatalf("drain T2: %v", err)
	}
	var timedOut bool
	for _, r := range fake.reported {
		if r.Name == "ci" && r.Verdict == coordinator.CheckBlocked && strings.Contains(r.Details, "timed out") {
			timedOut = true
		}
	}
	if !timedOut {
		t.Fatalf("current-head timer did not report the check timed out: %+v", fake.reported)
	}
}

// TestPhaseDeadlineArmFailureDoesNotFailCommittedStep is the regression for the
// non-fatal post-commit arming contract: a transition has already committed when
// the deadline timer is armed, so an arming failure must be logged and swallowed
// rather than turning the committed Step into an error.
func TestPhaseDeadlineArmFailureDoesNotFailCommittedStep(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{AuthoringStall: testAuthoringStall})
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}

	// Make the post-commit arm fail by removing the timers table: the arm's
	// hasPendingTimer/ScheduleTimer queries error, but applyTransition (which
	// touches only workflow_state/transitions) has already committed.
	if _, err := store.DB().Exec(`DROP TABLE timers`); err != nil {
		t.Fatalf("drop timers: %v", err)
	}

	res, err := eng.Step(ctx(), Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWorking}})
	if err != nil {
		t.Fatalf("Step must succeed despite a failed deadline arm, got: %v", err)
	}
	if res.ToPhase != coordinator.PhaseAuthoring {
		t.Fatalf("ToPhase = %q, want authoring", res.ToPhase)
	}
	// The transition committed even though arming failed.
	if got := currentPhase(t, store, issueID); got != coordinator.PhaseAuthoring {
		t.Fatalf("committed phase = %q, want authoring", got)
	}
	if n := transitionCount(t, store, issueID); n != 1 {
		t.Fatalf("transitions = %d, want 1 (committed)", n)
	}
}

// TestZeroDeadlineConfigSchedulesNothing covers numbered case 6: the zero-value
// DeadlineConfig arms no timers, so existing behavior is unchanged.
func TestZeroDeadlineConfigSchedulesNothing(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	// No SetDeadlines call: zero value, everything disabled.
	enterAuthoring(t, eng, fake, issueID)
	if n := pendingTimerKindCount(t, store, EventPhaseDeadline); n != 0 {
		t.Fatalf("phase_deadline timers = %d, want 0 with deadlines disabled", n)
	}

	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1"}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "old"}
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewInReview
	fake.scheduledNames = []string{"reviewer"}
	if _, err := eng.Step(ctx(), Event{Kind: EventSessionReady, SessionID: "s1", Payload: EventPayload{HeadSHA: "new"}}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 0 {
		t.Fatalf("check_timeout timers = %d, want 0 with deadlines disabled", n)
	}
}

// --- Recovery-armed check timeouts (Mode-B completion review) --------------

// pendingCheckTimeoutPayload returns the single undispatched check_timeout
// timer's payload, failing if there is not exactly one.
func pendingCheckTimeoutPayload(t *testing.T, store *flowdb.Store) string {
	t.Helper()
	var payload string
	if err := store.DB().QueryRow(
		`SELECT payload_json FROM timers WHERE kind = ? AND dispatched_at IS NULL`,
		string(EventCheckTimeout)).Scan(&payload); err != nil {
		t.Fatalf("read armed check_timeout timer: %v", err)
	}
	return payload
}

// TestRecoveryArmsCheckTimeoutForCompletionReview is the regression for the
// Mode-B gap: a completion-assessment review is scheduled by the coordinator's
// crash reconcile OUTSIDE the engine, so actReadyAuthorSession never armed its
// reviewer check timeout. The engine's recovery must arm the SAME
// EventCheckTimeout a normal round arms, using the pending checks the recovery
// scan surfaces, so the change cannot park forever when the reviewer never
// reports.
func TestRecoveryArmsCheckTimeoutForCompletionReview(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})
	fake.recoverPending = []coordinator.PendingCheckTimeout{
		{IssueID: issueID, HeadSHA: "headC", CheckNames: []string{"reviewer"}},
	}

	if _, err := eng.RunRecovery(ctx()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 1 {
		t.Fatalf("check_timeout timers = %d, want 1 armed by recovery", n)
	}
	// The armed timer carries the reviewer name + head so its guard keys to the
	// completion review's head exactly like a normal round's timer.
	payload := pendingCheckTimeoutPayload(t, store)
	if !strings.Contains(payload, `"reviewer"`) || !strings.Contains(payload, `"headC"`) {
		t.Fatalf("armed timer payload = %q, want reviewer@headC", payload)
	}
}

// TestRecoveryDoesNotDoubleArmCheckTimeout proves the arming is deduped per
// (issue, name, head): a normal round already armed its timeout at ready-time,
// so the recovery scan surfacing the same pending check must NOT add a second
// timer — while a genuinely new head IS a distinct timeout and is armed.
func TestRecoveryDoesNotDoubleArmCheckTimeout(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})

	// A normal round already armed the reviewer timeout for headC.
	if _, err := eng.ScheduleTimer(ctx(), issueID, EventCheckTimeout, eng.now().Add(testCheckPending), EventPayload{Name: "reviewer", HeadSHA: "headC"}); err != nil {
		t.Fatalf("pre-arm: %v", err)
	}
	fake.recoverPending = []coordinator.PendingCheckTimeout{
		{IssueID: issueID, HeadSHA: "headC", CheckNames: []string{"reviewer"}},
	}
	if _, err := eng.RunRecovery(ctx()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 1 {
		t.Fatalf("check_timeout timers = %d, want 1 (no double-arm for an already-armed head)", n)
	}

	// A new head is a different check timeout (the older row governs the older
	// head), so recovery arms it.
	fake.recoverPending = []coordinator.PendingCheckTimeout{
		{IssueID: issueID, HeadSHA: "headD", CheckNames: []string{"reviewer"}},
	}
	if _, err := eng.RunRecovery(ctx()); err != nil {
		t.Fatalf("recovery headD: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 2 {
		t.Fatalf("check_timeout timers = %d, want 2 (new head armed)", n)
	}
}

// TestRecoveredCheckTimeoutFiresAndRecovers proves the recovery-armed completion
// review timeout actually fires and recovers: a reviewer that never reports is
// reported blocked ("timed out") and triggers the fix follow-up, exactly like a
// normal review round's timeout.
func TestRecoveredCheckTimeoutFiresAndRecovers(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	eng.SetDeadlines(DeadlineConfig{CheckPending: testCheckPending})
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewInReview
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "headC"}
	if fake.checks == nil {
		fake.checks = map[string]coordinator.Check{}
	}
	fake.checks["reviewer"] = coordinator.Check{IssueID: issueID, Name: "reviewer", Kind: coordinator.CheckKindReviewer, Required: true, Verdict: coordinator.CheckPending}
	fake.recoverPending = []coordinator.PendingCheckTimeout{
		{IssueID: issueID, HeadSHA: "headC", CheckNames: []string{"reviewer"}},
	}

	// Recovery arms the timeout the coordinator-scheduled round left missing.
	if _, err := eng.RunRecovery(ctx()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// The reviewer never reports: the armed timeout comes due and fires.
	rewindPendingTimers(t, store)
	if _, err := eng.DrainDueTimers(ctx()); err != nil {
		t.Fatalf("drain timeout: %v", err)
	}

	var timedOut *coordinator.ReportCheckInput
	for i := range fake.reported {
		if fake.reported[i].Name == "reviewer" && fake.reported[i].Verdict == coordinator.CheckBlocked {
			timedOut = &fake.reported[i]
		}
	}
	if timedOut == nil {
		t.Fatalf("recovered timeout did not report the reviewer blocked: %+v", fake.reported)
	}
	if !strings.Contains(timedOut.Details, "timed out") {
		t.Fatalf("timeout details = %q, want 'timed out'", timedOut.Details)
	}
	if timedOut.Required == nil || !*timedOut.Required {
		t.Fatalf("timeout must preserve requiredness (required=true), got %+v", timedOut.Required)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("recovered timeout did not trigger the fix follow-up: %v", fake.calls)
	}
}

// TestRecoveryArmsNothingWhenDeadlineDisabled covers the disabled deadline: the
// zero-value CheckPending keeps the recovery arming a no-op, so deployments with
// check timeouts off see no new timers.
func TestRecoveryArmsNothingWhenDeadlineDisabled(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	// No SetDeadlines: CheckPending == 0.
	fake.recoverPending = []coordinator.PendingCheckTimeout{
		{IssueID: issueID, HeadSHA: "headC", CheckNames: []string{"reviewer"}},
	}
	if _, err := eng.RunRecovery(ctx()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if n := pendingTimerKindCount(t, store, EventCheckTimeout); n != 0 {
		t.Fatalf("check_timeout timers = %d, want 0 with deadlines disabled", n)
	}
}

// ctx is a tiny helper so the deadline tests read cleanly.
func ctx() context.Context { return context.Background() }
