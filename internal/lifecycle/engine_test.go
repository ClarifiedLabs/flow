package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// fakeEffects is a small stateful model of the coordinator used to exercise the
// engine's FSM logic without the full service stack. Reads return current field
// values; mutating effects update them so cascades behave realistically.
type fakeEffects struct {
	calls []string

	issue       coordinator.Issue
	reviewState coordinator.ReviewState
	readyChange coordinator.Change
	hasReady    bool
	activeState coordinator.SessionRuntimeState
	hasActive   bool
	session     coordinator.Session
	change      coordinator.Change
	thread      coordinator.ReviewThread
	reported    []coordinator.ReportCheckInput

	ensureAuthorErr           error
	enqueueAcceptance         []string
	acceptanceFlipsToInReview bool
	acceptancePending         bool
	merged                    bool

	updatedSessionStates []coordinator.SessionRuntimeState
	closedIssues         []string

	// Deadline support.
	checks            map[string]coordinator.Check
	scheduledNames    []string
	recoverPending    []coordinator.PendingCheckTimeout
	lastAgentActivity *time.Time
	hasActiveSession  bool
	statusWrites      []coordinator.WriteStatusInput

	// getIssueHook, if set, runs at the start of every GetIssue call. Tests use
	// it to inject concurrent workflow_state churn between snapshot load and
	// apply (GetIssue is the first Effects read of every step/derive).
	getIssueHook func()

	failOn map[string]error
}

func newFake(issue coordinator.Issue) *fakeEffects {
	return &fakeEffects{issue: issue, failOn: map[string]error{}}
}

func (f *fakeEffects) record(name string)      { f.calls = append(f.calls, name) }
func (f *fakeEffects) fail(name string) error  { return f.failOn[name] }
func (f *fakeEffects) called(name string) bool { return countCalls(f.calls, name) > 0 }

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeEffects) GetIssue(ctx context.Context, id string) (coordinator.Issue, error) {
	if f.getIssueHook != nil {
		f.getIssueHook()
	}
	f.record("GetIssue")
	return f.issue, f.fail("GetIssue")
}

func (f *fakeEffects) HasMergedChange(ctx context.Context, issueID string) (bool, error) {
	f.record("HasMergedChange")
	return f.merged, f.fail("HasMergedChange")
}

func (f *fakeEffects) ScheduleIssue(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Issue, error) {
	f.record("ScheduleIssue")
	if err := f.fail("ScheduleIssue"); err != nil {
		return coordinator.Issue{}, err
	}
	f.issue.ScheduleState = state
	return f.issue, nil
}

func (f *fakeEffects) SetIssueState(ctx context.Context, id string, state coordinator.IssueState) (coordinator.Issue, error) {
	f.record("SetIssueState")
	if err := f.fail("SetIssueState"); err != nil {
		return coordinator.Issue{}, err
	}
	switch state {
	case coordinator.IssueStateTriage:
		f.issue.ScheduleState = coordinator.ScheduleBacklog
		f.issue.TriageState = coordinator.TriagePending
		f.issue.ClosedAt = nil
	case coordinator.IssueStateBacklog:
		f.issue.ScheduleState = coordinator.ScheduleBacklog
		f.issue.TriageState = coordinator.TriageAccepted
		f.issue.ClosedAt = nil
	case coordinator.IssueStateUpNext:
		f.issue.ScheduleState = coordinator.ScheduleUpNext
		f.issue.TriageState = coordinator.TriageAccepted
		f.issue.ClosedAt = nil
	case coordinator.IssueStateClosed:
		f.closedIssues = append(f.closedIssues, id)
		f.issue.ScheduleState = coordinator.ScheduleClosed
	case coordinator.IssueStateRejected:
		f.closedIssues = append(f.closedIssues, id)
		f.issue.ScheduleState = coordinator.ScheduleClosed
		f.issue.TriageState = coordinator.TriageRejected
	}
	return f.issue, nil
}

func (f *fakeEffects) RetryCrashedAuthorJob(ctx context.Context, issueID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error) {
	f.record("RetryCrashedAuthorJob")
	if err := f.fail("RetryCrashedAuthorJob"); err != nil {
		return coordinator.RetryCrashedAuthorJobResult{}, err
	}
	return coordinator.RetryCrashedAuthorJobResult{Issue: f.issue}, nil
}

func (f *fakeEffects) AcceptTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	f.record("AcceptTriage")
	if err := f.fail("AcceptTriage"); err != nil {
		return coordinator.Issue{}, err
	}
	f.issue.TriageState = coordinator.TriageAccepted
	return f.issue, nil
}

func (f *fakeEffects) RejectTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	f.record("RejectTriage")
	if err := f.fail("RejectTriage"); err != nil {
		return coordinator.Issue{}, err
	}
	f.issue.TriageState = coordinator.TriageRejected
	f.issue.ScheduleState = coordinator.ScheduleClosed
	return f.issue, nil
}

func (f *fakeEffects) CloseIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	f.record("CloseIssue")
	if err := f.fail("CloseIssue"); err != nil {
		return coordinator.Issue{}, err
	}
	f.closedIssues = append(f.closedIssues, issueID)
	f.issue.ScheduleState = coordinator.ScheduleClosed
	return f.issue, nil
}

func (f *fakeEffects) GetSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	f.record("GetSession")
	return f.session, f.fail("GetSession")
}

func (f *fakeEffects) GetChange(ctx context.Context, changeID string) (coordinator.Change, error) {
	f.record("GetChange")
	return f.change, f.fail("GetChange")
}

func (f *fakeEffects) ReadyAuthorSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	f.record("ReadyAuthorSession")
	return f.session, f.fail("ReadyAuthorSession")
}

func (f *fakeEffects) ReadyPlanningSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	f.record("ReadyPlanningSession")
	return f.session, f.fail("ReadyPlanningSession")
}

func (f *fakeEffects) MarkPlanApproved(ctx context.Context, issueID string) (coordinator.Issue, error) {
	f.record("MarkPlanApproved")
	if err := f.fail("MarkPlanApproved"); err != nil {
		return coordinator.Issue{}, err
	}
	now := time.Now().UTC()
	f.issue.PlanApprovedAt = &now
	return f.issue, nil
}

func (f *fakeEffects) UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	f.record("UpdateSessionState")
	if err := f.fail("UpdateSessionState"); err != nil {
		return coordinator.Session{}, err
	}
	f.updatedSessionStates = append(f.updatedSessionStates, state)
	f.activeState = state
	f.hasActive = true
	f.session.RuntimeState = state
	return f.session, nil
}

func (f *fakeEffects) UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error) {
	f.record("UpdateChangeHead")
	if err := f.fail("UpdateChangeHead"); err != nil {
		return coordinator.Change{}, err
	}
	f.change.HeadSHA = headSHA
	return f.change, nil
}

func (f *fakeEffects) ResetAutomatedChecksForNewRevision(ctx context.Context, issueID string) (int, error) {
	f.record("ResetAutomatedChecksForNewRevision")
	return 0, f.fail("ResetAutomatedChecksForNewRevision")
}

func (f *fakeEffects) LoadSuiteForChange(ctx context.Context, change coordinator.Change) (coordinator.CheckSuite, error) {
	f.record("LoadSuiteForChange")
	return coordinator.CheckSuite{}, f.fail("LoadSuiteForChange")
}

func (f *fakeEffects) ScheduleReviewRound(ctx context.Context, input coordinator.ScheduleReviewRoundInput) (coordinator.ScheduleReviewRoundResult, error) {
	f.record("ScheduleReviewRound")
	if err := f.fail("ScheduleReviewRound"); err != nil {
		return coordinator.ScheduleReviewRoundResult{}, err
	}
	return coordinator.ScheduleReviewRoundResult{EnqueuedCheckNames: f.scheduledNames}, nil
}

func (f *fakeEffects) ReportCheck(ctx context.Context, input coordinator.ReportCheckInput) (coordinator.Check, error) {
	f.record("ReportCheck")
	if err := f.fail("ReportCheck"); err != nil {
		return coordinator.Check{}, err
	}
	f.reported = append(f.reported, input)
	required := false
	if input.Required != nil {
		required = *input.Required
	}
	if required && input.Verdict == coordinator.CheckBlocked {
		f.reviewState = coordinator.ReviewChangesRequested
	}
	check := coordinator.Check{
		IssueID:  input.IssueID,
		Name:     input.Name,
		Kind:     input.Kind,
		Required: required,
		Verdict:  input.Verdict,
	}
	if f.checks == nil {
		f.checks = map[string]coordinator.Check{}
	}
	f.checks[input.Name] = check
	return check, nil
}

func (f *fakeEffects) GetCheck(ctx context.Context, issueID, name string) (coordinator.Check, error) {
	f.record("GetCheck")
	if err := f.fail("GetCheck"); err != nil {
		return coordinator.Check{}, err
	}
	if check, ok := f.checks[name]; ok {
		return check, nil
	}
	return coordinator.Check{}, sql.ErrNoRows
}

func (f *fakeEffects) ReviewState(ctx context.Context, issueID string) (coordinator.ReviewState, error) {
	f.record("ReviewState")
	return f.reviewState, f.fail("ReviewState")
}

func (f *fakeEffects) HasReadyUnmergedChange(ctx context.Context, issueID string) (bool, error) {
	f.record("HasReadyUnmergedChange")
	return f.hasReady, f.fail("HasReadyUnmergedChange")
}

func (f *fakeEffects) ReadyUnmergedChangeForIssue(ctx context.Context, issueID string) (coordinator.Change, bool, error) {
	f.record("ReadyUnmergedChangeForIssue")
	return f.readyChange, f.hasReady, f.fail("ReadyUnmergedChangeForIssue")
}

func (f *fakeEffects) ActiveAuthorSessionState(ctx context.Context, issueID string) (coordinator.SessionRuntimeState, bool, error) {
	f.record("ActiveAuthorSessionState")
	return f.activeState, f.hasActive, f.fail("ActiveAuthorSessionState")
}

func (f *fakeEffects) EnqueueAcceptanceIfReady(ctx context.Context, issueID string, change coordinator.Change) ([]string, error) {
	f.record("EnqueueAcceptanceIfReady")
	if err := f.fail("EnqueueAcceptanceIfReady"); err != nil {
		return nil, err
	}
	if f.acceptanceFlipsToInReview {
		f.reviewState = coordinator.ReviewInReview
	}
	return f.enqueueAcceptance, nil
}

func (f *fakeEffects) LastAgentActivity(ctx context.Context, issueID string) (*time.Time, bool, error) {
	f.record("LastAgentActivity")
	if err := f.fail("LastAgentActivity"); err != nil {
		return nil, false, err
	}
	return f.lastAgentActivity, f.hasActiveSession, nil
}

func (f *fakeEffects) WriteStatus(ctx context.Context, input coordinator.WriteStatusInput) error {
	f.record("WriteStatus")
	if err := f.fail("WriteStatus"); err != nil {
		return err
	}
	f.statusWrites = append(f.statusWrites, input)
	return nil
}

func (f *fakeEffects) AcceptancePending(ctx context.Context, issueID string) (bool, error) {
	f.record("AcceptancePending")
	return f.acceptancePending, f.fail("AcceptancePending")
}

func (f *fakeEffects) EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error) {
	f.record("EnsureAuthorJob")
	return coordinator.EnsureAuthorJobResult{}, f.ensureAuthorErr
}

func (f *fakeEffects) ResetIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	f.record("ResetIssue")
	if err := f.fail("ResetIssue"); err != nil {
		return coordinator.Issue{}, err
	}
	return f.issue, nil
}

func (f *fakeEffects) MergeIssue(ctx context.Context, issueID string) (coordinator.MergeResult, error) {
	f.record("MergeIssue")
	if err := f.fail("MergeIssue"); err != nil {
		return coordinator.MergeResult{}, err
	}
	f.issue.ScheduleState = coordinator.ScheduleClosed
	f.reviewState = coordinator.ReviewMerged
	f.merged = true
	return coordinator.MergeResult{Issue: f.issue}, nil
}

func (f *fakeEffects) MergeChange(ctx context.Context, changeID string) (coordinator.MergeResult, error) {
	f.record("MergeChange")
	if err := f.fail("MergeChange"); err != nil {
		return coordinator.MergeResult{}, err
	}
	f.issue.ScheduleState = coordinator.ScheduleClosed
	f.reviewState = coordinator.ReviewMerged
	f.merged = true
	return coordinator.MergeResult{Issue: f.issue}, nil
}

func (f *fakeEffects) GetThread(ctx context.Context, threadID string) (coordinator.ReviewThread, error) {
	f.record("GetThread")
	return f.thread, f.fail("GetThread")
}

func (f *fakeEffects) ClaimThread(ctx context.Context, input coordinator.ClaimThreadInput) (coordinator.ReviewThread, error) {
	f.record("ClaimThread")
	if err := f.fail("ClaimThread"); err != nil {
		return coordinator.ReviewThread{}, err
	}
	f.thread.State = coordinator.ThreadClaimed
	return f.thread, nil
}

func (f *fakeEffects) CertifyThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	f.record("CertifyThread")
	if err := f.fail("CertifyThread"); err != nil {
		return coordinator.ReviewThread{}, err
	}
	f.thread.State = coordinator.ThreadCertified
	return f.thread, nil
}

func (f *fakeEffects) ReopenThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	f.record("ReopenThread")
	if err := f.fail("ReopenThread"); err != nil {
		return coordinator.ReviewThread{}, err
	}
	f.thread.State = coordinator.ThreadReopened
	return f.thread, nil
}

func (f *fakeEffects) AddComment(ctx context.Context, input coordinator.AddThreadCommentInput) (coordinator.ReviewThread, error) {
	f.record("AddComment")
	return f.thread, f.fail("AddComment")
}

func (f *fakeEffects) ReconcileCrashedAuthorSessions(ctx context.Context) (int, error) {
	f.record("ReconcileCrashedAuthorSessions")
	return 0, f.fail("ReconcileCrashedAuthorSessions")
}

func (f *fakeEffects) RecoverPendingCheckJobs(ctx context.Context) (int, []coordinator.PendingCheckTimeout, error) {
	f.record("RecoverPendingCheckJobs")
	return 0, f.recoverPending, f.fail("RecoverPendingCheckJobs")
}

func (f *fakeEffects) RecoverPendingMerges(ctx context.Context) (int, error) {
	f.record("RecoverPendingMerges")
	return 0, f.fail("RecoverPendingMerges")
}

// newEngineTest opens an in-memory DB with the migration applied and seeds one
// real issue row as the foreign-key anchor for workflow_state/transitions. The
// returned fake controls all domain behaviour; its issue carries the seeded ID.
func newEngineTest(t *testing.T) (*Engine, *fakeEffects, *flowdb.Store, string) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	issue, err := coordinator.NewIssueService(store.DB()).CreateIssue(ctx, coordinator.CreateIssueInput{Title: "test"})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	fake := newFake(issue)
	return NewEngine(store.DB(), fake), fake, store, issue.ID
}

// assertOrder checks that the named calls appear as an ordered subsequence.
func assertOrder(t *testing.T, calls []string, want ...string) {
	t.Helper()
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("calls %v do not contain ordered subsequence %v (matched %d)", calls, want, i)
	}
}

func transitionCount(t *testing.T, store *flowdb.Store, issueID string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM transitions WHERE issue_id = ?`, issueID).Scan(&n); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return n
}

func currentPhase(t *testing.T, store *flowdb.Store, issueID string) coordinator.Phase {
	t.Helper()
	var phase string
	if err := store.DB().QueryRow(`SELECT phase FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	return coordinator.Phase(phase)
}

func TestStepSessionReadyOrdersTheCascade(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1"}
	fake.change = coordinator.Change{ID: "c1", IssueID: issueID, HeadSHA: "old"} // ReadyAt nil => schedule review
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewInReview

	res, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1", Payload: EventPayload{HeadSHA: "new"}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Session == nil {
		t.Fatalf("expected session in result")
	}
	// Load-bearing ordering: preflight suite validation before ready; head update
	// + check reset before scheduling the review round.
	assertOrder(t, fake.calls,
		"GetSession", "GetChange", "LoadSuiteForChange", "ReadyAuthorSession",
		"UpdateChangeHead", "ResetAutomatedChecksForNewRevision", "ScheduleReviewRound")
	if transitionCount(t, store, issueID) != 1 {
		t.Fatalf("want 1 transition, got %d", transitionCount(t, store, issueID))
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseCritique {
		t.Fatalf("phase = %q, want critique", currentPhase(t, store, issueID))
	}
}

func TestStepSessionReadyWithoutHeadSkipsReviewBranch(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1"}

	if _, err := eng.Step(ctx, Event{Kind: EventSessionReady, SessionID: "s1"}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.called("ReadyAuthorSession") {
		t.Fatalf("expected ReadyAuthorSession")
	}
	if fake.called("UpdateChangeHead") || fake.called("ScheduleReviewRound") {
		t.Fatalf("did not expect head/review branch without headSHA: %v", fake.calls)
	}
}

func TestStepBlockedCheckEnqueuesFix(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewChangesRequested
	required := true

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "ci", CheckKind: coordinator.CheckKindCI, Required: &required, Verdict: coordinator.CheckBlocked,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Check == nil || res.Check.Verdict != coordinator.CheckBlocked {
		t.Fatalf("expected blocked check in result")
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("expected fix author job to be ensured: %v", fake.calls)
	}
	if fake.called("MergeIssue") {
		t.Fatalf("did not expect merge on blocked check")
	}
	if transitionCount(t, store, issueID) != 2 { // report-check + ensure-fix
		t.Fatalf("want 2 transitions, got %d", transitionCount(t, store, issueID))
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseCritique {
		t.Fatalf("phase = %q, want critique", currentPhase(t, store, issueID))
	}
}

func TestStepBlockedCheckSkipsFixWhenNotUpNext(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog // guardCanFix declines
	fake.hasReady = true
	fake.reviewState = coordinator.ReviewChangesRequested
	required := true

	if _, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "ci", CheckKind: coordinator.CheckKindCI, Required: &required, Verdict: coordinator.CheckBlocked,
	}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("did not expect author job when not up_next")
	}
	if transitionCount(t, store, issueID) != 1 { // ensure-fix guard declined => no log
		t.Fatalf("want 1 transition, got %d", transitionCount(t, store, issueID))
	}
}

func TestStepSatisfiedApprovedAutoMerges(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.AutoMerge = true
	fake.hasReady = true
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.reviewState = coordinator.ReviewApproved // satisfied check completes the suite

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "reviewer", CheckKind: coordinator.CheckKindReviewer, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	assertOrder(t, fake.calls, "ReportCheck", "EnqueueAcceptanceIfReady", "MergeIssue")
	if res.Merge == nil {
		t.Fatalf("expected merge result")
	}
	if res.ReviewState != coordinator.ReviewMerged {
		t.Fatalf("review state = %q, want merged", res.ReviewState)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseMergedClosed {
		t.Fatalf("phase = %q, want merged_closed", currentPhase(t, store, issueID))
	}
}

func TestStepAutoMergeConflictReportsBlockedCheckAndEnqueuesFix(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext
	fake.issue.AutoMerge = true
	fake.hasReady = true
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID, Branch: "issue/custom", Base: "main"}
	fake.reviewState = coordinator.ReviewApproved
	fake.failOn["MergeIssue"] = &flowgit.MergeConflictError{Output: "CONFLICT (content): Merge conflict in app.go"}

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "verifier", CheckKind: coordinator.CheckKindVerifier, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Check == nil || res.Check.Name != "verifier" {
		t.Fatalf("reported check result = %+v, want verifier", res.Check)
	}
	if res.Merge != nil {
		t.Fatalf("merge result = %+v, want nil after conflict", res.Merge)
	}
	if res.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("review state = %q, want changes_requested", res.ReviewState)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("expected fix author job after conflict: %v", fake.calls)
	}
	if transitionCount(t, store, issueID) != 3 { // report-check + auto-merge recovery + ensure-fix
		t.Fatalf("want 3 transitions, got %d", transitionCount(t, store, issueID))
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseCritique {
		t.Fatalf("phase = %q, want critique", currentPhase(t, store, issueID))
	}

	var recovered *coordinator.ReportCheckInput
	for i := range fake.reported {
		if fake.reported[i].Name == coordinator.AutoMergeCheckName {
			recovered = &fake.reported[i]
		}
	}
	if recovered == nil {
		t.Fatalf("auto-merge conflict check was not reported: %+v", fake.reported)
	}
	if recovered.Verdict != coordinator.CheckBlocked || recovered.Details == "" || recovered.Required == nil || !*recovered.Required {
		t.Fatalf("auto-merge recovery check = %+v", *recovered)
	}
	assertOrder(t, fake.calls, "MergeIssue", "ReportCheck", "EnsureAuthorJob")
}

func TestStepAutoMergeFailureIsRecordedAsFollowUpFailure(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.AutoMerge = true
	fake.hasReady = true
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.reviewState = coordinator.ReviewApproved
	fake.failOn["MergeIssue"] = errors.New("merge conflict")

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "verifier", CheckKind: coordinator.CheckKindVerifier, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Check == nil || res.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("check result = %+v, want satisfied", res.Check)
	}
	if res.Merge != nil {
		t.Fatalf("merge result = %+v, want nil after failed follow-up", res.Merge)
	}
	if len(res.FollowUpFailures) != 1 || res.FollowUpFailures[0].EventKind != EventAutoMerge || res.FollowUpFailures[0].Details != "merge conflict" {
		t.Fatalf("follow-up failures = %+v, want auto_merge merge conflict", res.FollowUpFailures)
	}
	if transitionCount(t, store, issueID) != 2 {
		t.Fatalf("want check_reported and failed auto_merge transitions, got %d", transitionCount(t, store, issueID))
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseApproved {
		t.Fatalf("phase = %q, want approved", currentPhase(t, store, issueID))
	}
	var guardResult string
	if err := store.DB().QueryRow(`
SELECT guard_result
FROM transitions
WHERE issue_id = ?
	AND event_kind = 'auto_merge'`, issueID).Scan(&guardResult); err != nil {
		t.Fatalf("read auto_merge transition: %v", err)
	}
	if guardResult != "failed: merge conflict" {
		t.Fatalf("auto_merge guard_result = %q, want failed details", guardResult)
	}
}

// TestStepAcceptanceBlocksAutoMerge is the regression for the load-bearing
// ordering: enqueuing acceptance (which creates a pending required check and
// flips the review back to in_review) must happen BEFORE the auto-merge decision.
func TestStepAcceptanceBlocksAutoMerge(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.AutoMerge = true
	fake.hasReady = true
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.reviewState = coordinator.ReviewApproved
	fake.acceptanceFlipsToInReview = true // enqueuing acceptance reopens the review

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "reviewer", CheckKind: coordinator.CheckKindReviewer, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("MergeIssue") {
		t.Fatalf("auto-merge must not fire while acceptance is pending")
	}
	if res.ReviewState != coordinator.ReviewInReview {
		t.Fatalf("review state = %q, want in_review", res.ReviewState)
	}
}

func TestStepApprovedNoAutoMergeWhenDisabled(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.AutoMerge = false
	fake.hasReady = true
	fake.readyChange = coordinator.Change{ID: "c1", IssueID: issueID}
	fake.reviewState = coordinator.ReviewApproved

	res, err := eng.Step(ctx, Event{Kind: EventCheckReported, IssueID: issueID, Payload: EventPayload{
		Name: "reviewer", CheckKind: coordinator.CheckKindReviewer, Verdict: coordinator.CheckSatisfied,
	}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("MergeIssue") {
		t.Fatalf("did not expect merge when auto-merge disabled")
	}
	if res.ReviewState != coordinator.ReviewApproved {
		t.Fatalf("review state = %q, want approved", res.ReviewState)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseApproved {
		t.Fatalf("phase = %q, want approved", currentPhase(t, store, issueID))
	}
}

func TestStepScheduleUpNextEnsuresAuthorJob(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.called("ScheduleIssue") || !fake.called("EnsureAuthorJob") {
		t.Fatalf("expected schedule + ensure author: %v", fake.calls)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseUpNext {
		t.Fatalf("phase = %q, want up_next", currentPhase(t, store, issueID))
	}
}

func TestStepScheduleBacklogSkipsAuthorJob(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleBacklog}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("did not expect author job when scheduling to backlog")
	}
}

func TestStepSetIssueStateUpNextReopensAndEnsuresAuthorJob(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleClosed

	if _, err := eng.Step(ctx, Event{Kind: EventSetIssueState, IssueID: issueID, Payload: EventPayload{IssueState: coordinator.IssueStateUpNext}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.called("SetIssueState") || !fake.called("EnsureAuthorJob") {
		t.Fatalf("expected set issue state + ensure author: %v", fake.calls)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseUpNext {
		t.Fatalf("phase = %q, want up_next", currentPhase(t, store, issueID))
	}
}

func TestStepSetIssueStateBacklogSkipsAuthorJob(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleClosed

	if _, err := eng.Step(ctx, Event{Kind: EventSetIssueState, IssueID: issueID, Payload: EventPayload{IssueState: coordinator.IssueStateBacklog}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("did not expect author job when setting state to backlog")
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseBacklog {
		t.Fatalf("phase = %q, want backlog", currentPhase(t, store, issueID))
	}
}

// TestStepTriageAcceptedDoesNotEnqueue is the regression for the gotcha that
// accepting triage must NOT enqueue an author job.
func TestStepTriageAcceptedDoesNotEnqueue(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriagePending
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	if _, err := eng.Step(ctx, Event{Kind: EventTriageIssue, IssueID: issueID, Payload: EventPayload{Triage: coordinator.TriageAccepted}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.called("AcceptTriage") {
		t.Fatalf("expected AcceptTriage")
	}
	if fake.called("EnsureAuthorJob") {
		t.Fatalf("accepting triage must not enqueue an author job")
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseBacklog {
		t.Fatalf("phase = %q, want backlog", currentPhase(t, store, issueID))
	}
}

func TestStepTriageRejectedClosesIssue(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriagePending

	if _, err := eng.Step(ctx, Event{Kind: EventTriageIssue, IssueID: issueID, Payload: EventPayload{Triage: coordinator.TriageRejected}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fake.called("RejectTriage") {
		t.Fatalf("expected RejectTriage")
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseRejectedClosed {
		t.Fatalf("phase = %q, want rejected_closed", currentPhase(t, store, issueID))
	}
}

func TestStepEnsureAuthorJobSuppressionTolerated(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.ensureAuthorErr = coordinator.ErrAuthorJobSuppressed

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("suppressed author job must be tolerated, got: %v", err)
	}
}

func TestStepEnsureAuthorJobHardErrorPropagates(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.ensureAuthorErr = errors.New("boom")

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err == nil {
		t.Fatalf("expected hard author-job error to propagate")
	}
}

func TestStepIdempotentReplayIsNoOp(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	ev := Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "k1", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}

	if _, err := eng.Step(ctx, ev); err != nil {
		t.Fatalf("first step: %v", err)
	}
	afterFirst := transitionCount(t, store, issueID)
	if _, err := eng.Step(ctx, ev); err != nil {
		t.Fatalf("replay step: %v", err)
	}
	if got := countCalls(fake.calls, "ScheduleIssue"); got != 1 {
		t.Fatalf("ScheduleIssue called %d times, want 1 (replay must skip the action)", got)
	}
	if got := transitionCount(t, store, issueID); got != afterFirst {
		t.Fatalf("replay must not append transitions: had %d, now %d", afterFirst, got)
	}
}

func TestStepThreadClaim(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.thread = coordinator.ReviewThread{ID: "t1", IssueID: issueID, State: coordinator.ThreadOpen}

	res, err := eng.Step(ctx, Event{Kind: EventThreadClaimed, ThreadID: "t1", Payload: EventPayload{ThreadKind: coordinator.ClaimFixed, Body: "done"}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Thread == nil || res.Thread.State != coordinator.ThreadClaimed {
		t.Fatalf("expected claimed thread in result")
	}
	if transitionCount(t, store, issueID) != 1 {
		t.Fatalf("want 1 transition, got %d", transitionCount(t, store, issueID))
	}
}

// TestSessionStateChangedTransitionsPhase is the regression for routing a
// working<->waiting flip through the engine: the action records the new session
// state via Effects while the derived workflow phase stays authoring. The
// human wait is modeled as a board/status overlay, not a durable phase.
func TestSessionStateChangedTransitionsPhase(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.activeState = coordinator.SessionWorking
	fake.hasActive = true

	res, err := eng.Step(ctx, Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWaiting}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.ToPhase != coordinator.PhaseAuthoring {
		t.Fatalf("ToPhase = %q, want authoring", res.ToPhase)
	}
	if res.Session == nil || res.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("result session = %+v, want waiting", res.Session)
	}
	if len(fake.updatedSessionStates) != 1 || fake.updatedSessionStates[0] != coordinator.SessionWaiting {
		t.Fatalf("UpdateSessionState calls = %+v, want one waiting", fake.updatedSessionStates)
	}
	if transitionCount(t, store, issueID) != 1 {
		t.Fatalf("want 1 transition, got %d", transitionCount(t, store, issueID))
	}
	var kind, toPhase string
	if err := store.DB().QueryRow(`
SELECT event_kind, to_phase
FROM transitions
WHERE issue_id = ?`, issueID).Scan(&kind, &toPhase); err != nil {
		t.Fatalf("read transition: %v", err)
	}
	if kind != string(EventSessionStateChanged) || toPhase != string(coordinator.PhaseAuthoring) {
		t.Fatalf("transition kind/to_phase = %q/%q, want session_state_changed/authoring", kind, toPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseAuthoring {
		t.Fatalf("phase = %q, want authoring", currentPhase(t, store, issueID))
	}
}

func TestPlanModeSessionStateChangedTransitionsToPlanning(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.PlanMode = true
	fake.session = coordinator.Session{ID: "s1", IssueID: issueID, ChangeID: "c1", RuntimeState: coordinator.SessionWorking}
	fake.activeState = coordinator.SessionWorking
	fake.hasActive = true

	res, err := eng.Step(ctx, Event{Kind: EventSessionStateChanged, SessionID: "s1", Payload: EventPayload{SessionState: coordinator.SessionWaiting}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.ToPhase != coordinator.PhasePlanning {
		t.Fatalf("ToPhase = %q, want planning", res.ToPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhasePlanning {
		t.Fatalf("phase = %q, want planning", currentPhase(t, store, issueID))
	}
}

// TestCloseIssueRecordsTransition is the regression for routing issue close
// through the engine: the action closes the issue via Effects and the derived
// phase lands on abandoned, with a close_issue transition row.
func TestCloseIssueRecordsTransition(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	res, err := eng.Step(ctx, Event{Kind: EventCloseIssue, IssueID: issueID})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.ToPhase != coordinator.PhaseAbandoned {
		t.Fatalf("ToPhase = %q, want abandoned", res.ToPhase)
	}
	if res.Issue == nil || res.Issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("result issue = %+v, want closed", res.Issue)
	}
	if len(fake.closedIssues) != 1 || fake.closedIssues[0] != issueID {
		t.Fatalf("CloseIssue calls = %+v, want one for %s", fake.closedIssues, issueID)
	}
	if transitionCount(t, store, issueID) != 1 {
		t.Fatalf("want 1 transition, got %d", transitionCount(t, store, issueID))
	}
	var kind, toPhase string
	if err := store.DB().QueryRow(`
SELECT event_kind, to_phase
FROM transitions
WHERE issue_id = ?`, issueID).Scan(&kind, &toPhase); err != nil {
		t.Fatalf("read transition: %v", err)
	}
	if kind != string(EventCloseIssue) || toPhase != string(coordinator.PhaseAbandoned) {
		t.Fatalf("transition kind/to_phase = %q/%q, want close_issue/abandoned", kind, toPhase)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseAbandoned {
		t.Fatalf("phase = %q, want abandoned", currentPhase(t, store, issueID))
	}
}

func TestStepInvalidTransition(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted

	_, err := eng.Step(ctx, Event{Kind: EventKind("bogus"), IssueID: issueID})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

// TestApplyTransitionVersionConflict is the regression for optimistic
// concurrency: applyTransition compares the caller's expectedVersion against the
// version it reads live under the write lock and rejects a stale apply with
// ErrVersionConflict, so a guard/action that ran against superseded state cannot
// silently overwrite a concurrent writer's phase.
func TestApplyTransitionVersionConflict(t *testing.T) {
	eng, _, store, issueID := newEngineTest(t)
	ctx := context.Background()
	if _, err := store.DB().Exec(`INSERT INTO workflow_state (issue_id, phase, version, updated_at) VALUES (?, 'backlog', 5, ?)`,
		issueID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed workflow_state: %v", err)
	}

	// Stale expectedVersion (0 != live 5): rejected, nothing written.
	stale := &snapshot{issueID: issueID, phase: coordinator.PhaseBacklog, version: 0}
	applied, err := eng.applyTransition(ctx, issueID, stale, Event{Kind: EventScheduleIssue}, "x", coordinator.PhaseUpNext, stale.version)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale apply err = %v, want ErrVersionConflict", err)
	}
	if applied {
		t.Fatalf("stale apply must not write")
	}
	var phase string
	var version int64
	if err := store.DB().QueryRow(`SELECT phase, version FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&phase, &version); err != nil {
		t.Fatalf("read workflow_state: %v", err)
	}
	if phase != string(coordinator.PhaseBacklog) || version != 5 {
		t.Fatalf("after rejected apply phase/version = %s/%d, want backlog/5", phase, version)
	}

	// Matching expectedVersion applies cleanly and bumps the version.
	fresh := &snapshot{issueID: issueID, phase: coordinator.PhaseBacklog, version: 5}
	applied, err = eng.applyTransition(ctx, issueID, fresh, Event{Kind: EventScheduleIssue}, "x", coordinator.PhaseUpNext, fresh.version)
	if err != nil {
		t.Fatalf("matching apply: %v", err)
	}
	if !applied {
		t.Fatalf("matching apply must write")
	}
	if err := store.DB().QueryRow(`SELECT phase, version FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&phase, &version); err != nil {
		t.Fatalf("read workflow_state: %v", err)
	}
	if phase != string(coordinator.PhaseUpNext) || version != 6 {
		t.Fatalf("after matching apply phase/version = %s/%d, want up_next/6", phase, version)
	}
}

// TestApplyTransitionSkipsCheckForNegativeVersion proves expectedVersion < 0
// disables the comparison — the contract recordFollowUpFailure relies on, since
// it has no real snapshot to expect.
func TestApplyTransitionSkipsCheckForNegativeVersion(t *testing.T) {
	eng, _, store, issueID := newEngineTest(t)
	ctx := context.Background()
	if _, err := store.DB().Exec(`INSERT INTO workflow_state (issue_id, phase, version, updated_at) VALUES (?, 'backlog', 5, ?)`,
		issueID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed workflow_state: %v", err)
	}
	snap := &snapshot{issueID: issueID, phase: coordinator.PhaseBacklog, version: 0}
	applied, err := eng.applyTransition(ctx, issueID, snap, Event{Kind: EventScheduleIssue}, "x", coordinator.PhaseUpNext, -1)
	if err != nil {
		t.Fatalf("apply with expectedVersion=-1: %v", err)
	}
	if !applied {
		t.Fatalf("expected transition to apply")
	}
	var version int64
	if err := store.DB().QueryRow(`SELECT version FROM workflow_state WHERE issue_id = ?`, issueID).Scan(&version); err != nil {
		t.Fatalf("read workflow_state: %v", err)
	}
	if version != 6 {
		t.Fatalf("version = %d, want 6 (bumped from live regardless of stale snapshot)", version)
	}
}

// TestStepRetriesOnVersionConflict is the regression for the retry loop:
// version churn injected between snapshot load and apply (a concurrent writer
// bumps workflow_state.version) forces applyTransition to report
// ErrVersionConflict on the first attempt; step must reload the snapshot and
// retry, converging to a successful apply rather than surfacing the conflict.
func TestStepRetriesOnVersionConflict(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	// Initialise the workflow_state row so the snapshot loads a concrete version,
	// then arm a one-shot hook that bumps the version out from under the in-flight
	// step the first time GetIssue runs inside the action/derive path. GetIssue is
	// the first Effects read of every step, so counting its calls lets us inject
	// churn after the snapshot is loaded but before applyTransition commits.
	if _, err := eng.loadSnapshot(ctx, issueID); err != nil {
		t.Fatalf("prime snapshot: %v", err)
	}
	churned := false
	fake.getIssueHook = func() {
		if churned {
			return
		}
		churned = true
		if _, err := store.DB().Exec(
			`UPDATE workflow_state SET version = version + 1 WHERE issue_id = ?`, issueID); err != nil {
			t.Fatalf("inject version churn: %v", err)
		}
	}

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("step under version churn: %v", err)
	}
	if !churned {
		t.Fatalf("hook never fired; churn was not injected")
	}
	// The retry re-ran the action, so ScheduleIssue was invoked twice (once per
	// attempt). Despite the double run, only the second attempt's apply committed:
	// exactly one schedule-kind transition row exists, proving the retry did not
	// double-commit the move. (The up_next schedule also cascades an ensure-author
	// follow-up, so the total transition count is 2 — we assert on the schedule
	// row specifically.)
	if got := countCalls(fake.calls, "ScheduleIssue"); got != 2 {
		t.Fatalf("ScheduleIssue called %d times, want 2 (one per attempt)", got)
	}
	if got := scheduleTransitionCount(t, store, issueID); got != 1 {
		t.Fatalf("want 1 schedule transition after retry, got %d", got)
	}
	if currentPhase(t, store, issueID) != coordinator.PhaseUpNext {
		t.Fatalf("phase = %q, want up_next", currentPhase(t, store, issueID))
	}
}

func scheduleTransitionCount(t *testing.T, store *flowdb.Store, issueID string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM transitions WHERE issue_id = ? AND event_kind = 'schedule_issue'`, issueID).Scan(&n); err != nil {
		t.Fatalf("count schedule transitions: %v", err)
	}
	return n
}

// TestStepSurfacesPersistentVersionConflict proves the retry loop is bounded: a
// hook that bumps the version on every GetIssue keeps every attempt stale, so
// after exhausting its attempts step surfaces ErrVersionConflict instead of
// looping forever.
func TestStepSurfacesPersistentVersionConflict(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	if _, err := eng.loadSnapshot(ctx, issueID); err != nil {
		t.Fatalf("prime snapshot: %v", err)
	}
	// Bump the version on every GetIssue so no attempt's snapshot can ever match
	// the live version at apply time.
	fake.getIssueHook = func() {
		if _, err := store.DB().Exec(
			`UPDATE workflow_state SET version = version + 1 WHERE issue_id = ?`, issueID); err != nil {
			t.Fatalf("inject version churn: %v", err)
		}
	}

	_, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("persistent churn err = %v, want ErrVersionConflict", err)
	}
}

// TestDerivePhaseClosedDisambiguation is the regression for the bug where the
// engine mislabeled abandoned issues (closed, not rejected, no merged change) as
// merged_closed, diverging from coordinator.DerivePhase.
func TestDerivePhaseClosedDisambiguation(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.ScheduleState = coordinator.ScheduleClosed

	fake.issue.TriageState = coordinator.TriageAccepted
	fake.merged = false
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseAbandoned {
		t.Fatalf("closed+accepted+no-merge: got %q (err %v), want abandoned", p, err)
	}

	fake.merged = true
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseMergedClosed {
		t.Fatalf("closed+merged: got %q (err %v), want merged_closed", p, err)
	}

	fake.merged = false
	fake.issue.TriageState = coordinator.TriageRejected
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseRejectedClosed {
		t.Fatalf("closed+rejected: got %q (err %v), want rejected_closed", p, err)
	}
}

func TestDerivePhaseAcceptance(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.hasReady = true

	// In review with a ready change, all required critique checks satisfied and
	// a verifier check still pending: the issue is in the acceptance phase.
	fake.reviewState = coordinator.ReviewInReview
	fake.acceptancePending = true
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseAcceptance {
		t.Fatalf("in-review+acceptance-pending: got %q (err %v), want acceptance", p, err)
	}

	// Same in-review state but no pending verifier (acceptance not yet gated):
	// the issue stays in critique.
	fake.acceptancePending = false
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseCritique {
		t.Fatalf("in-review+no-acceptance: got %q (err %v), want critique", p, err)
	}

	// Verifier satisfied flips the overall review state to approved, which
	// short-circuits before the in-review branch.
	fake.reviewState = coordinator.ReviewApproved
	fake.acceptancePending = false
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseApproved {
		t.Fatalf("verifier satisfied: got %q (err %v), want approved", p, err)
	}

	// Verifier blocked flips the overall review state to changes_requested,
	// which routes back to critique.
	fake.reviewState = coordinator.ReviewChangesRequested
	if p, err := eng.derivePhase(ctx, issueID); err != nil || p != coordinator.PhaseCritique {
		t.Fatalf("verifier blocked: got %q (err %v), want critique", p, err)
	}
}

func TestStepCascadeLimit(t *testing.T) {
	eng, fake, _, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	var res StepResult
	err := eng.step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID}, &res, maxCascadeDepth+1)
	if !errors.Is(err, ErrCascadeLimit) {
		t.Fatalf("want ErrCascadeLimit, got %v", err)
	}
}
