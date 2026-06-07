package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

// This file makes the lifecycle engine's determinism ENFORCED rather than
// asserted by convention. It generates batches of external events and checks
// three properties against real workflow_state / transitions / event_inbox
// SQLite tables:
//
//   Property 1 (order-insensitive convergence): permuting a batch of events and
//     running each permutation against a FRESH store converges to the same final
//     phase and the same check-verdict multiset — for the subset of batches that
//     are genuinely order-independent (see exactPhaseConvergent below for the
//     honest scoping). For ALL batches we additionally assert torn-state freedom:
//     the persisted phase equals derivePhase and the transition log is consistent.
//
//   Property 2 (replay is a no-op): re-Stepping every transition-logged event with
//     its recorded idempotency key appends zero new transition rows and leaves the
//     final phase unchanged.
//
//   Property 3 (crash-redelivery converges): un-confirming k inbox rows and ticking
//     past the grace window leaves the final phase unchanged, appends no duplicate
//     transition rows, and confirms every row.
//
// Harness choice: a REAL flowdb store (so workflow_state, transitions, timers and
// event_inbox — the durability machinery under test — are exercised for real)
// driven by a reactive in-memory domain model (modelEffects). The production
// liveEffects needs seven coordinator services plus a real git repo for merges,
// which is the heavy api integration harness; the lifecycle package's own fake is
// the honest, light seam the engine was designed against. The stock fakeEffects is
// too static for interleaving (fixed reviewState / hasReady), so modelEffects
// derives every read from accumulated mutations — making commutative events
// genuinely reconverge instead of trivially agreeing.

// ---------------------------------------------------------------------------
// modelEffects: a reactive domain model whose reads are a deterministic function
// of the mutations applied so far. Order-independent mutations (e.g. recording
// two different checks) commute, which is what gives the convergence properties
// teeth: if the engine's apply order leaked into the final state, permutations
// would diverge.
// ---------------------------------------------------------------------------

type modelEffects struct {
	issueID  string
	changeID string

	triage         coordinator.TriageState
	schedule       coordinator.ScheduleState
	closed         bool
	merged         bool
	planApprovedAt *time.Time

	// ready is true once the change has been readied (a ready event with a head).
	ready   bool
	headSHA string

	// sessionState is the active author session's runtime state; hasSession is
	// false until a session event arrives.
	sessionState coordinator.SessionRuntimeState
	hasSession   bool
	lastActivity *time.Time

	// checks maps name -> verdict (and kind/required), the source of truth for the
	// derived review state. A map makes distinct check reports commute.
	checks map[string]coordinator.Check

	// threads maps id -> state.
	threads map[string]coordinator.ReviewThreadState

	// reported is the append-only audit of every ReportCheck the engine drove,
	// used to compare the check-verdict multiset across permutations.
	reported []coordinator.ReportCheckInput
}

func newModel(issueID, changeID string) *modelEffects {
	return &modelEffects{
		issueID:  issueID,
		changeID: changeID,
		triage:   coordinator.TriageAccepted,
		schedule: coordinator.ScheduleUpNext,
		checks:   map[string]coordinator.Check{},
		threads:  map[string]coordinator.ReviewThreadState{},
	}
}

func (m *modelEffects) issue() coordinator.Issue {
	return coordinator.Issue{
		ID:             m.issueID,
		TriageState:    m.triage,
		ScheduleState:  m.schedule,
		AutoMerge:      false, // auto-merge needs a real git repo; out of scope here
		PlanApprovedAt: m.planApprovedAt,
	}
}

func (m *modelEffects) change() coordinator.Change {
	return coordinator.Change{ID: m.changeID, IssueID: m.issueID, HeadSHA: m.headSHA}
}

// derivedReviewState mirrors the coordinator's verdict aggregation closely enough
// for the FSM's reads: a required blocked check => changes_requested; otherwise if
// every required check is satisfied (and at least one exists) => approved; else
// in_review. This is a pure function of the verdict multiset, so it is invariant
// to the order checks were reported in.
func (m *modelEffects) derivedReviewState() coordinator.ReviewState {
	if m.merged {
		return coordinator.ReviewMerged
	}
	anyRequired := false
	allRequiredSatisfied := true
	for _, c := range m.checks {
		if !c.Required {
			continue
		}
		anyRequired = true
		switch c.Verdict {
		case coordinator.CheckBlocked:
			return coordinator.ReviewChangesRequested
		case coordinator.CheckSatisfied:
		default:
			allRequiredSatisfied = false
		}
	}
	if anyRequired && allRequiredSatisfied {
		return coordinator.ReviewApproved
	}
	return coordinator.ReviewInReview
}

// acceptancePending mirrors the real gate: all required critique-kind checks
// satisfied AND at least one verifier-kind check still pending. Pure over the
// verdict multiset.
func (m *modelEffects) acceptancePending() bool {
	verifierPending := false
	for _, c := range m.checks {
		if c.Kind == coordinator.CheckKindVerifier && c.Verdict == coordinator.CheckPending {
			verifierPending = true
		}
	}
	if !verifierPending {
		return false
	}
	for _, c := range m.checks {
		if !c.Required {
			continue
		}
		if isCritiqueCheckKind(c.Kind) && c.Verdict != coordinator.CheckSatisfied {
			return false
		}
	}
	return true
}

func (m *modelEffects) hasReadyChange() bool { return m.ready && !m.merged }

// --- Effects implementation ------------------------------------------------

func (m *modelEffects) GetIssue(ctx context.Context, id string) (coordinator.Issue, error) {
	return m.issue(), nil
}

func (m *modelEffects) HasMergedChange(ctx context.Context, issueID string) (bool, error) {
	return m.merged, nil
}

func (m *modelEffects) ResetIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	return m.issue(), nil
}

func (m *modelEffects) RetryCrashedAuthorJob(ctx context.Context, issueID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error) {
	return coordinator.RetryCrashedAuthorJobResult{Issue: m.issue()}, nil
}

func (m *modelEffects) ScheduleIssue(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Issue, error) {
	m.schedule = state
	if state == coordinator.ScheduleClosed {
		m.closed = true
	}
	return m.issue(), nil
}

func (m *modelEffects) SetIssueState(ctx context.Context, id string, state coordinator.IssueState) (coordinator.Issue, error) {
	switch state {
	case coordinator.IssueStateTriage:
		m.schedule = coordinator.ScheduleBacklog
		m.triage = coordinator.TriagePending
		m.closed = false
	case coordinator.IssueStateBacklog:
		m.schedule = coordinator.ScheduleBacklog
		m.triage = coordinator.TriageAccepted
		m.closed = false
	case coordinator.IssueStateUpNext:
		m.schedule = coordinator.ScheduleUpNext
		m.triage = coordinator.TriageAccepted
		m.closed = false
	case coordinator.IssueStateClosed:
		m.schedule = coordinator.ScheduleClosed
		m.closed = true
	case coordinator.IssueStateRejected:
		m.triage = coordinator.TriageRejected
		m.schedule = coordinator.ScheduleClosed
		m.closed = true
	}
	return m.issue(), nil
}

func (m *modelEffects) AcceptTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	m.triage = coordinator.TriageAccepted
	return m.issue(), nil
}

func (m *modelEffects) RejectTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	m.triage = coordinator.TriageRejected
	m.schedule = coordinator.ScheduleClosed
	m.closed = true
	return m.issue(), nil
}

func (m *modelEffects) CloseIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	m.schedule = coordinator.ScheduleClosed
	m.closed = true
	return m.issue(), nil
}

func (m *modelEffects) GetSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	return coordinator.Session{ID: sessionID, IssueID: m.issueID, ChangeID: m.changeID, RuntimeState: m.sessionState}, nil
}

func (m *modelEffects) GetChange(ctx context.Context, changeID string) (coordinator.Change, error) {
	return m.change(), nil
}

func (m *modelEffects) ReadyAuthorSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	// Readying a session closes out the active author session (it is no longer
	// in flight), so the issue is no longer in a session-derived phase.
	m.hasSession = false
	return coordinator.Session{ID: sessionID, IssueID: m.issueID, ChangeID: m.changeID}, nil
}

func (m *modelEffects) ReadyPlanningSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	m.hasSession = false
	return coordinator.Session{ID: sessionID, IssueID: m.issueID, ChangeID: m.changeID}, nil
}

func (m *modelEffects) MarkPlanApproved(ctx context.Context, issueID string) (coordinator.Issue, error) {
	now := time.Now().UTC()
	m.planApprovedAt = &now
	return m.issue(), nil
}

func (m *modelEffects) UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	m.sessionState = state
	m.hasSession = true
	return coordinator.Session{ID: sessionID, IssueID: m.issueID, ChangeID: m.changeID, RuntimeState: state}, nil
}

func (m *modelEffects) UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error) {
	m.headSHA = headSHA
	return m.change(), nil
}

func (m *modelEffects) ResetAutomatedChecksForNewRevision(ctx context.Context, issueID string) (int, error) {
	// A new revision retires automated (non-human) checks back to pending.
	n := 0
	for name, c := range m.checks {
		if c.Kind == coordinator.CheckKindHuman {
			continue
		}
		if c.Verdict != coordinator.CheckPending {
			c.Verdict = coordinator.CheckPending
			m.checks[name] = c
			n++
		}
	}
	return n, nil
}

func (m *modelEffects) LoadSuiteForChange(ctx context.Context, change coordinator.Change) (coordinator.CheckSuite, error) {
	return coordinator.CheckSuite{}, nil
}

func (m *modelEffects) ScheduleReviewRound(ctx context.Context, input coordinator.ScheduleReviewRoundInput) (coordinator.ScheduleReviewRoundResult, error) {
	// Readying marks the change ready for review.
	m.ready = true
	return coordinator.ScheduleReviewRoundResult{}, nil
}

func (m *modelEffects) ReportCheck(ctx context.Context, input coordinator.ReportCheckInput) (coordinator.Check, error) {
	required := false
	if input.Required != nil {
		required = *input.Required
	}
	check := coordinator.Check{
		IssueID:  input.IssueID,
		Name:     input.Name,
		Kind:     input.Kind,
		Required: required,
		Verdict:  input.Verdict,
		Details:  input.Details,
	}
	m.checks[input.Name] = check
	m.reported = append(m.reported, input)
	return check, nil
}

func (m *modelEffects) GetCheck(ctx context.Context, issueID, name string) (coordinator.Check, error) {
	if c, ok := m.checks[name]; ok {
		return c, nil
	}
	return coordinator.Check{}, errors.New("no rows")
}

func (m *modelEffects) ReviewState(ctx context.Context, issueID string) (coordinator.ReviewState, error) {
	return m.derivedReviewState(), nil
}

func (m *modelEffects) HasReadyUnmergedChange(ctx context.Context, issueID string) (bool, error) {
	return m.hasReadyChange(), nil
}

func (m *modelEffects) ReadyUnmergedChangeForIssue(ctx context.Context, issueID string) (coordinator.Change, bool, error) {
	if !m.hasReadyChange() {
		return coordinator.Change{}, false, nil
	}
	return m.change(), true, nil
}

func (m *modelEffects) ActiveAuthorSessionState(ctx context.Context, issueID string) (coordinator.SessionRuntimeState, bool, error) {
	if !m.hasSession {
		return "", false, nil
	}
	return m.sessionState, true, nil
}

func (m *modelEffects) EnqueueAcceptanceIfReady(ctx context.Context, issueID string, change coordinator.Change) ([]string, error) {
	return nil, nil
}

func (m *modelEffects) AcceptancePending(ctx context.Context, issueID string) (bool, error) {
	return m.acceptancePending(), nil
}

func (m *modelEffects) EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error) {
	return coordinator.EnsureAuthorJobResult{}, nil
}

func (m *modelEffects) MergeIssue(ctx context.Context, issueID string) (coordinator.MergeResult, error) {
	m.merged = true
	m.schedule = coordinator.ScheduleClosed
	m.closed = true
	return coordinator.MergeResult{Issue: m.issue()}, nil
}

func (m *modelEffects) MergeChange(ctx context.Context, changeID string) (coordinator.MergeResult, error) {
	m.merged = true
	m.schedule = coordinator.ScheduleClosed
	m.closed = true
	return coordinator.MergeResult{Issue: m.issue()}, nil
}

func (m *modelEffects) GetThread(ctx context.Context, threadID string) (coordinator.ReviewThread, error) {
	state, ok := m.threads[threadID]
	if !ok {
		state = coordinator.ThreadOpen
	}
	return coordinator.ReviewThread{ID: threadID, IssueID: m.issueID, State: state}, nil
}

func (m *modelEffects) ClaimThread(ctx context.Context, input coordinator.ClaimThreadInput) (coordinator.ReviewThread, error) {
	m.threads[input.ThreadID] = coordinator.ThreadClaimed
	return coordinator.ReviewThread{ID: input.ThreadID, IssueID: m.issueID, State: coordinator.ThreadClaimed}, nil
}

func (m *modelEffects) CertifyThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	m.threads[input.ThreadID] = coordinator.ThreadCertified
	return coordinator.ReviewThread{ID: input.ThreadID, IssueID: m.issueID, State: coordinator.ThreadCertified}, nil
}

func (m *modelEffects) ReopenThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	m.threads[input.ThreadID] = coordinator.ThreadReopened
	return coordinator.ReviewThread{ID: input.ThreadID, IssueID: m.issueID, State: coordinator.ThreadReopened}, nil
}

func (m *modelEffects) AddComment(ctx context.Context, input coordinator.AddThreadCommentInput) (coordinator.ReviewThread, error) {
	state, ok := m.threads[input.ThreadID]
	if !ok {
		state = coordinator.ThreadOpen
	}
	return coordinator.ReviewThread{ID: input.ThreadID, IssueID: m.issueID, State: state}, nil
}

func (m *modelEffects) LastAgentActivity(ctx context.Context, issueID string) (*time.Time, bool, error) {
	if !m.hasSession {
		return nil, false, nil
	}
	return m.lastActivity, true, nil
}

func (m *modelEffects) WriteStatus(ctx context.Context, input coordinator.WriteStatusInput) error {
	return nil
}

func (m *modelEffects) ReconcileCrashedAuthorSessions(ctx context.Context) (int, error) {
	return 0, nil
}
func (m *modelEffects) RecoverPendingCheckJobs(ctx context.Context) (int, []coordinator.PendingCheckTimeout, error) {
	return 0, nil, nil
}
func (m *modelEffects) RecoverPendingMerges(ctx context.Context) (int, error) { return 0, nil }

// ---------------------------------------------------------------------------
// Scenario generator: a catalog of external event constructors over one seeded
// issue+change fixture. Each scenario is a batch of 5-10 events drawn with a
// seeded rand.
// ---------------------------------------------------------------------------

const (
	scenarioIssueID  = "iss-prop"
	scenarioChangeID = "chg-prop"
	scenarioSession  = "ses-prop"
	scenarioHead     = "headcafe"
)

// eventGen is one catalog entry: a constructor plus the classification metadata
// the convergence property needs to decide, HONESTLY, whether a batch is
// expected to converge on the exact same final phase under any permutation.
//
// The metadata captures the three legitimate sources of order-dependence the FSM
// has (verified empirically against the engine, NOT papered over):
//
//  1. terminalSink — close/merge are absorbing: close-then-ready ends abandoned,
//     ready-then-close also ends abandoned BUT the intermediate cascade differs,
//     and merge-then-anything vs anything-then-merge can land in different review
//     projections. Their relative order to other mutations changes the result.
//
//  2. sessionPhase — a working/waiting flip puts the issue in a session-derived
//     phase (planning/authoring), while EventSessionReady CLOSES the active
//     author session. So a batch mixing `ready` with any session-state event is
//     order-dependent: the session-derived phase survives iff a session event
//     lands after the last ready.
//
//  3. checkName/checkVerdict — checks are keyed by name (last write wins), so a
//     batch reporting the SAME check name with two DIFFERENT verdicts is
//     order-dependent: whichever lands last decides that check, and therefore the
//     review state and phase.
//
//  4. ready-vs-automated-check — EventSessionReady advances the head and RESETS
//     every automated (non-human) check back to pending (a new revision retires
//     stale verdicts). So a check reported BEFORE ready is wiped while one reported
//     AFTER ready survives: a batch mixing ready with any automated check report is
//     order-dependent. (autoCheck flags those reports.)
//
// A batch free of all four hazards is exact-phase convergent; otherwise only
// torn-state freedom is asserted (see exactPhaseConvergent).
type eventGen struct {
	name string
	make func() Event

	terminalSink bool   // close / merge
	sessionPhase bool   // session_working / session_waiting (NOT ready)
	ready        bool   // EventSessionReady — closes the session AND resets checks
	checkName    string // non-empty for check reports; "" otherwise
	checkVerdict string // the verdict the report writes (for the conflict check)
	autoCheck    bool   // an automated (non-human) check report, reset by ready
}

// catalog returns the fixed set of external event constructors over the seeded
// issue+change fixture, each tagged with the metadata exactPhaseConvergent reads.
func catalog() []eventGen {
	required := true
	notRequired := false
	return []eventGen{
		{name: "session_working", sessionPhase: true, make: func() Event {
			return Event{Kind: EventSessionStateChanged, SessionID: scenarioSession, Payload: EventPayload{SessionState: coordinator.SessionWorking}}
		}},
		{name: "session_waiting", sessionPhase: true, make: func() Event {
			return Event{Kind: EventSessionStateChanged, SessionID: scenarioSession, Payload: EventPayload{SessionState: coordinator.SessionWaiting}}
		}},
		{name: "ready", ready: true, make: func() Event {
			return Event{Kind: EventSessionReady, SessionID: scenarioSession, Payload: EventPayload{HeadSHA: scenarioHead}}
		}},
		{name: "ci_satisfied", checkName: "ci", checkVerdict: "satisfied", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "ci", CheckKind: coordinator.CheckKindCI, Required: &required, Verdict: coordinator.CheckSatisfied}}
		}},
		{name: "ci_blocked", checkName: "ci", checkVerdict: "blocked", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "ci", CheckKind: coordinator.CheckKindCI, Required: &required, Verdict: coordinator.CheckBlocked}}
		}},
		{name: "reviewer_satisfied", checkName: "reviewer", checkVerdict: "satisfied", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "reviewer", CheckKind: coordinator.CheckKindReviewer, Required: &required, Verdict: coordinator.CheckSatisfied}}
		}},
		{name: "reviewer_blocked", checkName: "reviewer", checkVerdict: "blocked", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "reviewer", CheckKind: coordinator.CheckKindReviewer, Required: &required, Verdict: coordinator.CheckBlocked}}
		}},
		{name: "verifier_satisfied", checkName: "verifier", checkVerdict: "satisfied", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "verifier", CheckKind: coordinator.CheckKindVerifier, Required: &required, Verdict: coordinator.CheckSatisfied}}
		}},
		{name: "verifier_blocked", checkName: "verifier", checkVerdict: "blocked", autoCheck: true, make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "verifier", CheckKind: coordinator.CheckKindVerifier, Required: &required, Verdict: coordinator.CheckBlocked}}
		}},
		{name: "phase_deadline_optional", checkName: "phase-deadline", checkVerdict: "blocked", autoCheck: true, make: func() Event {
			// A non-required check report (models a deadline escalation arriving as
			// an external check). Never gates approval; commutes with other names.
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: "phase-deadline", CheckKind: coordinator.CheckKindCI, Required: &notRequired, Verdict: coordinator.CheckBlocked}}
		}},
		{name: "thread_claim", make: func() Event {
			return Event{Kind: EventThreadClaimed, ThreadID: "thr-1", Payload: EventPayload{ThreadKind: coordinator.ClaimFixed, Body: "done"}}
		}},
		{name: "thread_certify", make: func() Event {
			return Event{Kind: EventThreadCertify, ThreadID: "thr-1", Payload: EventPayload{Body: "ok"}}
		}},
		{name: "thread_reopen", make: func() Event {
			return Event{Kind: EventThreadReopen, ThreadID: "thr-1", Payload: EventPayload{Body: "nope"}}
		}},
		{name: "schedule_backlog", make: func() Event {
			return Event{Kind: EventScheduleIssue, IssueID: scenarioIssueID, Payload: EventPayload{Schedule: coordinator.ScheduleBacklog}}
		}},
		{name: "schedule_up_next", make: func() Event {
			return Event{Kind: EventScheduleIssue, IssueID: scenarioIssueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}
		}},
		{name: "close", terminalSink: true, make: func() Event {
			return Event{Kind: EventCloseIssue, IssueID: scenarioIssueID}
		}},
		{name: "merge", terminalSink: true, make: func() Event {
			return Event{Kind: EventMergeChange, ChangeID: scenarioChangeID, IssueID: scenarioIssueID}
		}},
	}
}

// exactPhaseConvergent reports whether every permutation of a batch MUST land in
// the same final phase and verdict multiset. It returns false (only torn-state
// freedom is then asserted) when the batch hits any of the three legitimate
// order-dependence hazards documented on eventGen: a terminal sink, a ready event
// mixed with any session-state event, or a check name reported with conflicting
// verdicts. This is the honest boundary of the strong convergence claim — a batch
// outside it can legitimately reach different phases under different orders.
func exactPhaseConvergent(batch []eventGen) bool {
	hasReady := false
	hasSessionPhase := false
	hasAutoCheck := false
	verdictsByName := map[string]map[string]bool{}
	for _, g := range batch {
		if g.terminalSink {
			return false
		}
		if g.ready {
			hasReady = true
		}
		if g.sessionPhase {
			hasSessionPhase = true
		}
		if g.autoCheck {
			hasAutoCheck = true
		}
		if g.checkName != "" {
			if verdictsByName[g.checkName] == nil {
				verdictsByName[g.checkName] = map[string]bool{}
			}
			verdictsByName[g.checkName][g.checkVerdict] = true
		}
	}
	if hasReady && hasSessionPhase {
		return false // hazard 2: ready closes the session; the session-derived phase races
	}
	if hasReady && hasAutoCheck {
		return false // hazard 4: ready resets automated checks; before/after ready differs
	}
	for _, verdicts := range verdictsByName {
		if len(verdicts) > 1 {
			return false // hazard 3: same check name, conflicting verdicts (last write wins)
		}
	}
	return true
}

// generateBatch draws between 5 and 10 catalog entries (with replacement) for a
// seed. Returning the gens (not just events) lets the property classify the batch.
func generateBatch(rng *rand.Rand, cat []eventGen) []eventGen {
	n := 5 + rng.Intn(6) // 5..10
	batch := make([]eventGen, n)
	for i := range batch {
		batch[i] = cat[rng.Intn(len(cat))]
	}
	return batch
}

func batchEvents(batch []eventGen) []Event {
	evs := make([]Event, len(batch))
	for i, g := range batch {
		evs[i] = g.make()
	}
	return evs
}

// permute returns a fresh permutation of evs using rng.
func permute(rng *rand.Rand, evs []Event) []Event {
	out := make([]Event, len(evs))
	copy(out, evs)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// ---------------------------------------------------------------------------
// Per-run harness: a fresh real store + a fresh model, seeded from the same
// fixture every time so two runs differ only in event order.
// ---------------------------------------------------------------------------

type propRun struct {
	eng   *Engine
	model *modelEffects
	store *flowdb.Store
}

func newPropRun(t *testing.T) *propRun {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, t.TempDir()+"/flow.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed the FK-anchor issue row (workflow_state/transitions reference issues).
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO issues (id, title, triage_state, schedule_state, created_by, created_at, updated_at)
		 VALUES (?, 'prop', 'accepted', 'up_next', 'system', ?, ?)`,
		scenarioIssueID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	model := newModel(scenarioIssueID, scenarioChangeID)
	eng := NewEngine(store.DB(), model)
	// A fixed clock keeps timer fire-at math deterministic across runs.
	eng.now = func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	return &propRun{eng: eng, model: model, store: store}
}

// applyBatch Steps each event in order, treating ErrInvalidTransition as a skip
// (mirroring the real handlers, which 4xx an illegal event without aborting the
// surrounding work). All other errors fail the test.
func (r *propRun) applyBatch(t *testing.T, seed int64, evs []Event) {
	t.Helper()
	ctx := context.Background()
	for i, ev := range evs {
		if _, err := r.eng.Step(ctx, ev); err != nil {
			if errors.Is(err, ErrInvalidTransition) {
				continue
			}
			t.Fatalf("seed=%d event %d (%s): %v", seed, i, ev.Kind, err)
		}
	}
}

func (r *propRun) tick(t *testing.T, seed int64) {
	t.Helper()
	if err := r.eng.Tick(context.Background()); err != nil {
		t.Fatalf("seed=%d tick: %v", seed, err)
	}
}

func (r *propRun) phase(t *testing.T) coordinator.Phase {
	t.Helper()
	return currentPhase(t, r.store, scenarioIssueID)
}

// derivedPhase recomputes the phase from the model via the engine, independent of
// the persisted workflow_state row. Torn-state freedom means these agree.
func (r *propRun) derivedPhase(t *testing.T) coordinator.Phase {
	t.Helper()
	p, err := r.eng.derivePhase(context.Background(), scenarioIssueID)
	if err != nil {
		t.Fatalf("derive phase: %v", err)
	}
	return p
}

// verdictMultiset returns a stable, order-independent fingerprint of the recorded
// check verdicts (name=verdict pairs, sorted), so two runs that reported the same
// checks — regardless of order — compare equal.
func (r *propRun) verdictMultiset() string {
	counts := map[string]int{}
	for _, in := range r.model.reported {
		counts[fmt.Sprintf("%s=%s", in.Name, in.Verdict)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, fmt.Sprintf("%s x%d;", k, counts[k])...)
	}
	return string(b)
}

// transitionLog returns the ordered (event_kind, from, to) tuples for the issue.
func (r *propRun) transitionLog(t *testing.T) []string {
	t.Helper()
	rows, err := r.store.DB().Query(
		`SELECT event_kind, from_phase, to_phase FROM transitions WHERE issue_id = ? ORDER BY seq`, scenarioIssueID)
	if err != nil {
		t.Fatalf("query transitions: %v", err)
	}
	defer rows.Close()
	var log []string
	for rows.Next() {
		var kind, from, to string
		if err := rows.Scan(&kind, &from, &to); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		log = append(log, kind+":"+from+"->"+to)
	}
	return log
}

// assertConsistentLog checks the transition log is not torn: every recorded
// to_phase of row i equals the from_phase of the next row that actually changed
// the phase. (Reconcile/no-op rows keep from==to, so we only require the chain be
// monotone in the recorded sense: consecutive rows share the boundary phase.)
func assertConsistentLog(t *testing.T, seed int64, log []string) {
	t.Helper()
	var prevTo string
	for i, entry := range log {
		// entry is kind:from->to
		arrow := splitTransition(entry)
		if i > 0 && arrow.from != prevTo {
			t.Fatalf("seed=%d torn transition log at row %d: %q starts from %q but previous row ended at %q\nlog=%v",
				seed, i, entry, arrow.from, prevTo, log)
		}
		prevTo = arrow.to
	}
}

type transitionArrow struct {
	kind, from, to string
}

func splitTransition(entry string) transitionArrow {
	// kind:from->to
	var a transitionArrow
	colon := -1
	for i := 0; i < len(entry); i++ {
		if entry[i] == ':' {
			colon = i
			break
		}
	}
	a.kind = entry[:colon]
	rest := entry[colon+1:]
	for i := 0; i+1 < len(rest); i++ {
		if rest[i] == '-' && rest[i+1] == '>' {
			a.from = rest[:i]
			a.to = rest[i+2:]
			break
		}
	}
	return a
}

func propSeeds(t *testing.T) int {
	if testing.Short() {
		return 10
	}
	return 50
}

// ---------------------------------------------------------------------------
// Property 1: order-insensitive convergence.
// ---------------------------------------------------------------------------

// TestProperty1OrderInsensitiveConvergence applies each generated batch in 5
// random permutations against 5 fresh stores, Ticks once, and asserts:
//
//   - For EVERY batch (universally guaranteed): torn-state freedom — the persisted
//     workflow_state phase equals derivePhase recomputed from the same model, and
//     the transition log is internally consistent (each row's from_phase equals the
//     previous row's to_phase, so the recorded history is a connected walk, never
//     torn). No permutation may leave the engine's durable phase out of sync with
//     the state it is derived from.
//
//   - For EXACT-PHASE-CONVERGENT batches (exactPhaseConvergent == true: no terminal
//     sink, no ready-vs-session race, no conflicting check verdicts): all five
//     permutations land in the SAME final phase AND the same check-verdict
//     multiset. This is the strong claim, scoped honestly to the commuting subset —
//     batches outside the subset hit a documented, engine-faithful order-dependence
//     and are checked for torn-state freedom only.
func TestProperty1OrderInsensitiveConvergence(t *testing.T) {
	t.Parallel()
	cat := catalog()
	seeds := propSeeds(t)
	const permutations = 5

	exactBatches := 0
	for s := 0; s < seeds; s++ {
		seed := int64(s)
		rng := rand.New(rand.NewSource(seed))
		batch := generateBatch(rng, cat)
		base := batchEvents(batch)
		exact := exactPhaseConvergent(batch)
		if exact {
			exactBatches++
		}

		var firstPhase coordinator.Phase
		var firstVerdicts string
		for p := 0; p < permutations; p++ {
			perm := permute(rng, base)
			run := newPropRun(t)
			run.applyBatch(t, seed, perm)
			run.tick(t, seed)

			persisted := run.phase(t)
			derived := run.derivedPhase(t)
			if persisted != derived {
				t.Fatalf("seed=%d perm=%d torn state: persisted phase %q != derived %q\nbatch=%v",
					seed, p, persisted, derived, genNames(batch))
			}
			assertConsistentLog(t, seed, run.transitionLog(t))

			if !exact {
				continue // torn-state checks only; exact phase may legitimately differ
			}
			verdicts := run.verdictMultiset()
			if p == 0 {
				firstPhase = persisted
				firstVerdicts = verdicts
				continue
			}
			if persisted != firstPhase {
				t.Fatalf("seed=%d perm=%d converged to %q but perm 0 reached %q\nbatch=%v",
					seed, p, persisted, firstPhase, genNames(batch))
			}
			if verdicts != firstVerdicts {
				t.Fatalf("seed=%d perm=%d verdict multiset %q != perm 0 %q\nbatch=%v",
					seed, p, verdicts, firstVerdicts, genNames(batch))
			}
		}
	}
	// Guard against the property silently degenerating into "torn-state only": if a
	// future catalog/predicate change made NO batch exact-phase convergent, the
	// strong claim would never run and we would not notice. Require coverage.
	if exactBatches == 0 {
		t.Fatalf("no exact-phase-convergent batches generated across %d seeds; "+
			"the strong convergence claim never ran", seeds)
	}
}

func genNames(batch []eventGen) []string {
	names := make([]string, len(batch))
	for i, g := range batch {
		names[i] = g.name
	}
	return names
}

// TestProperty1CheckReportsConvergeExactly is the focused, strong case the plan
// calls out explicitly: permutations of check-report-only batches MUST converge on
// the exact same phase and verdict multiset. To keep the claim clean, each catalog
// entry reports a DISTINCT check name with a FIXED verdict, so no batch can contain
// the conflicting-verdict hazard (same name, two verdicts) — the one genuine
// order-dependence among pure check reports. With no terminal sink and no session
// churn either, these batches are unconditionally commutative and any divergence is
// a real engine bug, not an over-claim.
func TestProperty1CheckReportsConvergeExactly(t *testing.T) {
	t.Parallel()
	required := true
	mkCheck := func(name string, kind coordinator.CheckKind, v coordinator.CheckVerdict) eventGen {
		return eventGen{name: name + "_" + string(v), checkName: name, checkVerdict: string(v), make: func() Event {
			return Event{Kind: EventCheckReported, IssueID: scenarioIssueID, Payload: EventPayload{Name: name, CheckKind: kind, Required: &required, Verdict: v}}
		}}
	}
	// Distinct names, each with a single fixed verdict. Weighted toward satisfied
	// so random batches explore BOTH terminal review phases — all-satisfied draws
	// reach approved, any blocked draw reaches critique — rather than collapsing to
	// a single phase (which would make the strong claim vacuous).
	checkOnly := []eventGen{
		mkCheck("ci", coordinator.CheckKindCI, coordinator.CheckSatisfied),
		mkCheck("reviewer", coordinator.CheckKindReviewer, coordinator.CheckSatisfied),
		mkCheck("verifier", coordinator.CheckKindVerifier, coordinator.CheckSatisfied),
		mkCheck("lint", coordinator.CheckKindCI, coordinator.CheckSatisfied),
		mkCheck("security", coordinator.CheckKindReviewer, coordinator.CheckBlocked),
	}
	seeds := propSeeds(t)
	const permutations = 5
	phasesSeen := map[coordinator.Phase]int{}
	for s := 0; s < seeds; s++ {
		seed := int64(s)
		rng := rand.New(rand.NewSource(seed + 10_000))
		batch := generateBatch(rng, checkOnly)
		// Safety net: the distinct-name construction should make this always true,
		// but assert it so a future edit that reuses a name is caught loudly.
		if !exactPhaseConvergent(batch) {
			t.Fatalf("seed=%d check-only batch unexpectedly order-dependent: %v", seed, genNames(batch))
		}

		// A ready change is the precondition for check reports to flow through the
		// review loop, so we mark the change ready in every run before the batch.
		readyEvent := Event{Kind: EventSessionReady, SessionID: scenarioSession, Payload: EventPayload{HeadSHA: scenarioHead}}
		base := append([]Event{readyEvent}, batchEvents(batch)...)

		var firstPhase coordinator.Phase
		var firstVerdicts string
		for p := 0; p < permutations; p++ {
			// Keep the ready event first (it is the precondition); permute the rest.
			rest := permute(rng, base[1:])
			perm := append([]Event{base[0]}, rest...)

			run := newPropRun(t)
			run.applyBatch(t, seed, perm)
			run.tick(t, seed)

			persisted := run.phase(t)
			if persisted != run.derivedPhase(t) {
				t.Fatalf("seed=%d perm=%d torn state", seed, p)
			}
			verdicts := run.verdictMultiset()
			if p == 0 {
				firstPhase = persisted
				firstVerdicts = verdicts
				phasesSeen[persisted]++
				continue
			}
			if persisted != firstPhase {
				t.Fatalf("seed=%d perm=%d phase %q != %q (check-only must converge exactly)\nbatch=%v",
					seed, p, persisted, firstPhase, genNames(batch))
			}
			if verdicts != firstVerdicts {
				t.Fatalf("seed=%d perm=%d verdicts %q != %q\nbatch=%v",
					seed, p, verdicts, firstVerdicts, genNames(batch))
			}
		}
	}
	// The strong claim is only meaningful if it actually distinguishes phases:
	// require the seeds to have reached BOTH a fully-satisfied phase (approved) and
	// a blocked phase (critique). If the catalog ever collapsed to one phase the
	// convergence assertion would be trivially true and we want to fail loudly.
	if phasesSeen[coordinator.PhaseApproved] == 0 || phasesSeen[coordinator.PhaseCritique] == 0 {
		t.Fatalf("check-only convergence did not exercise both approved and critique: %v", phasesSeen)
	}
}

// ---------------------------------------------------------------------------
// Property 2: replay is a no-op.
// ---------------------------------------------------------------------------

// recordedTransition mirrors a row of the transitions table sufficient to rebuild
// the event for replay.
type recordedTransition struct {
	eventKind      string
	idempotencyKey string
	payloadJSON    string
}

func (r *propRun) recordedTransitions(t *testing.T) []recordedTransition {
	t.Helper()
	rows, err := r.store.DB().Query(
		`SELECT event_kind, idempotency_key, payload_json FROM transitions WHERE issue_id = ? ORDER BY seq`, scenarioIssueID)
	if err != nil {
		t.Fatalf("query transitions: %v", err)
	}
	defer rows.Close()
	var out []recordedTransition
	for rows.Next() {
		var rec recordedTransition
		var key *string
		if err := rows.Scan(&rec.eventKind, &key, &rec.payloadJSON); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		if key != nil {
			rec.idempotencyKey = *key
		}
		out = append(out, rec)
	}
	return out
}

// TestProperty2ReplayIsNoOp generates a run, then re-Steps every transition-logged
// EXTERNAL event with its recorded idempotency key. The transitions replay guard
// must dedupe each one: zero new transition rows, unchanged final phase. (Internal
// follow-on kinds — ensure_*, auto_merge, reconcile — are not directly Step-able
// external events; replaying them would be meaningless, so we replay only the
// external kinds, which is exactly what a redelivery or a retrying client does.)
func TestProperty2ReplayIsNoOp(t *testing.T) {
	t.Parallel()
	cat := catalog()
	seeds := propSeeds(t)
	for s := 0; s < seeds; s++ {
		seed := int64(s)
		rng := rand.New(rand.NewSource(seed + 20_000))
		batch := generateBatch(rng, cat)

		run := newPropRun(t)
		run.applyBatch(t, seed, batchEvents(batch))
		run.tick(t, seed)

		beforeCount := transitionCount(t, run.store, scenarioIssueID)
		beforePhase := run.phase(t)
		recorded := run.recordedTransitions(t)

		replayed := 0
		for _, rec := range recorded {
			ev, ok := replayEvent(rec)
			if !ok {
				continue // internal follow-on kind; not externally replayable
			}
			if _, err := run.eng.Step(context.Background(), ev); err != nil {
				if errors.Is(err, ErrInvalidTransition) {
					continue
				}
				t.Fatalf("seed=%d replay %s: %v", seed, rec.eventKind, err)
			}
			replayed++
		}

		afterCount := transitionCount(t, run.store, scenarioIssueID)
		if afterCount != beforeCount {
			t.Fatalf("seed=%d replay appended transitions: %d -> %d (replayed %d events)\nbatch=%v",
				seed, beforeCount, afterCount, replayed, genNames(batch))
		}
		if after := run.phase(t); after != beforePhase {
			t.Fatalf("seed=%d replay changed phase: %q -> %q", seed, beforePhase, after)
		}
	}
}

// replayEvent rebuilds a Step-able external event from a recorded transition row.
// It returns ok=false for internal follow-on kinds (ensure_*, auto_merge,
// reconcile, and the deadline kinds, which carry no externally-meaningful key).
func replayEvent(rec recordedTransition) (Event, bool) {
	kind := EventKind(rec.eventKind)
	switch kind {
	case EventSessionReady, EventCheckReported, EventScheduleIssue, EventTriageIssue,
		EventMergeRequested, EventMergeChange, EventThreadClaimed, EventThreadCertify,
		EventThreadReopen, EventThreadComment, EventSessionStateChanged, EventCloseIssue:
	default:
		return Event{}, false
	}
	if rec.idempotencyKey == "" {
		return Event{}, false
	}
	var payload EventPayload
	if rec.payloadJSON != "" {
		_ = json.Unmarshal([]byte(rec.payloadJSON), &payload)
	}
	ev := Event{
		Kind:           kind,
		IssueID:        scenarioIssueID,
		IdempotencyKey: rec.idempotencyKey,
		Payload:        payload,
	}
	// Re-attach the routing key the original carried so resolveIssueID succeeds.
	switch kind {
	case EventSessionReady, EventSessionStateChanged:
		ev.SessionID = scenarioSession
	case EventMergeChange:
		ev.ChangeID = scenarioChangeID
	case EventThreadClaimed, EventThreadCertify, EventThreadReopen, EventThreadComment:
		ev.ThreadID = "thr-1"
	}
	return ev, true
}

// ---------------------------------------------------------------------------
// Property 3: crash-redelivery converges.
// ---------------------------------------------------------------------------

// TestProperty3CrashRedeliveryConverges runs a generated batch through the
// journaling Step, then simulates crashes by clearing confirmed_at on k random
// inbox rows and rewinding their created_at past the grace window. After Tick:
//
//   - the final phase is unchanged from the no-crash run,
//   - no duplicate transition rows were appended (redelivery dedupes via the
//     replay guard),
//   - every inbox row is confirmed.
func TestProperty3CrashRedeliveryConverges(t *testing.T) {
	t.Parallel()
	cat := catalog()
	seeds := propSeeds(t)
	for s := 0; s < seeds; s++ {
		seed := int64(s)
		rng := rand.New(rand.NewSource(seed + 30_000))
		batch := generateBatch(rng, cat)
		evs := batchEvents(batch)

		run := newPropRun(t)
		run.applyBatch(t, seed, evs)
		run.tick(t, seed)

		phaseBefore := run.phase(t)
		countBefore := transitionCount(t, run.store, scenarioIssueID)

		// Simulate k crashes: pick k confirmed inbox rows, un-confirm them, and age
		// them past the grace window so redeliverInbox picks them up.
		ids := run.confirmedInboxIDs(t)
		if len(ids) == 0 {
			continue
		}
		k := 1 + rng.Intn(len(ids))
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		stale := formatTime(run.eng.now().Add(-time.Hour))
		for _, id := range ids[:k] {
			if _, err := run.store.DB().Exec(
				`UPDATE event_inbox SET confirmed_at = NULL, created_at = ? WHERE id = ?`, stale, id); err != nil {
				t.Fatalf("seed=%d un-confirm inbox %s: %v", seed, id, err)
			}
		}

		// Redeliver. The grace window is satisfied by the rewound created_at; the
		// engine clock is fixed, so the rows are eligible.
		run.tick(t, seed)

		if after := run.phase(t); after != phaseBefore {
			t.Fatalf("seed=%d redelivery changed phase: %q -> %q (crashed %d rows)\nbatch=%v",
				seed, phaseBefore, after, k, genNames(batch))
		}
		if after := transitionCount(t, run.store, scenarioIssueID); after != countBefore {
			t.Fatalf("seed=%d redelivery appended %d duplicate transitions (crashed %d rows)\nbatch=%v",
				seed, after-countBefore, k, genNames(batch))
		}
		if n := unconfirmedInboxCount(t, run.store); n != 0 {
			t.Fatalf("seed=%d %d inbox rows still unconfirmed after redelivery", seed, n)
		}
	}
}

// confirmedInboxIDs returns the ids of every confirmed inbox row.
func (r *propRun) confirmedInboxIDs(t *testing.T) []string {
	t.Helper()
	rows, err := r.store.DB().Query(`SELECT id FROM event_inbox WHERE confirmed_at IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("query inbox ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan inbox id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}
