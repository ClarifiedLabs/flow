package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestReportCheckMapsCIExitCodeToVerdict(t *testing.T) {
	ctx := context.Background()
	_, issues, checks := newCheckService(t)
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Check target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	exitZero := 0
	passing, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "fake-ci",
		ExitCode: &exitZero,
		Reporter: "worker:w-local",
	})
	if err != nil {
		t.Fatalf("report passing check: %v", err)
	}
	if passing.Kind != CheckKindCI || passing.Verdict != CheckSatisfied || passing.ExitCode == nil || *passing.ExitCode != 0 {
		t.Fatalf("passing check = %+v", passing)
	}

	exitFailure := 2
	blocked, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "fake-ci",
		ExitCode: &exitFailure,
		Details:  "exit status 2",
		Reporter: "worker:w-local",
	})
	if err != nil {
		t.Fatalf("report blocked check: %v", err)
	}
	if blocked.ID != passing.ID {
		t.Fatalf("rerun check ID = %d, want %d", blocked.ID, passing.ID)
	}
	if blocked.Verdict != CheckBlocked || blocked.ExitCode == nil || *blocked.ExitCode != 2 {
		t.Fatalf("blocked check = %+v", blocked)
	}

	listed, err := checks.ListChecks(ctx, issue.ID)
	if err != nil {
		t.Fatalf("list checks: %v", err)
	}
	if len(listed) != 1 || listed[0].Verdict != CheckBlocked {
		t.Fatalf("listed checks = %+v", listed)
	}
}

func TestRequiredChecksDeriveReviewStateAndBoardLane(t *testing.T) {
	ctx := context.Background()
	_, issues, checks := newCheckService(t)
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Review target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}

	state, err := checks.ReviewState(ctx, issue.ID)
	if err != nil {
		t.Fatalf("initial review state: %v", err)
	}
	if state != ReviewInReview {
		t.Fatalf("initial review state = %q, want in_review", state)
	}

	exitFailure := 1
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "fake-ci",
		ExitCode: &exitFailure,
	}); err != nil {
		t.Fatalf("report blocked check: %v", err)
	}
	state, err = checks.ReviewState(ctx, issue.ID)
	if err != nil {
		t.Fatalf("blocked review state: %v", err)
	}
	if state != ReviewChangesRequested {
		t.Fatalf("blocked review state = %q, want changes_requested", state)
	}
	board, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board: %v", err)
	}
	assertIssueIDs(t, board.Board.InProgress, []string{issue.ID})
	assertIssueIDs(t, board.Board.UpNext, []string{})
	if got := board.LaneStates[issue.ID]; got != LaneStateChangesRequested {
		t.Fatalf("lane state = %q, want changes_requested", got)
	}

	exitZero := 0
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "fake-ci",
		ExitCode: &exitZero,
	}); err != nil {
		t.Fatalf("report satisfied check: %v", err)
	}
	state, err = checks.ReviewState(ctx, issue.ID)
	if err != nil {
		t.Fatalf("approved review state: %v", err)
	}
	if state != ReviewApproved {
		t.Fatalf("approved review state = %q, want approved", state)
	}
	board, err = issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board after satisfied: %v", err)
	}
	assertIssueIDs(t, board.Board.InProgress, []string{})
	assertIssueIDs(t, board.Board.UpNext, []string{issue.ID})
	if got := board.LaneStates[issue.ID]; got != LaneStateUpNext {
		t.Fatalf("lane state = %q, want up_next", got)
	}
}

func TestResetAutomatedChecksForNewRevisionLeavesHumanChecksBlocked(t *testing.T) {
	ctx := context.Background()
	_, issues, checks := newCheckService(t)
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Reset target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "ci",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("report ci: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckBlocked,
	}); err != nil {
		t.Fatalf("report reviewer: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "human",
		Kind:     CheckKindHuman,
		Required: &required,
		Verdict:  CheckBlocked,
	}); err != nil {
		t.Fatalf("report human: %v", err)
	}

	reset, err := checks.ResetAutomatedChecksForNewRevision(ctx, issue.ID)
	if err != nil {
		t.Fatalf("reset checks: %v", err)
	}
	if reset != 2 {
		t.Fatalf("reset = %d, want two automated checks", reset)
	}
	ci, err := checks.GetCheck(ctx, issue.ID, "ci")
	if err != nil {
		t.Fatalf("get ci: %v", err)
	}
	if ci.Verdict != CheckPending || ci.ExitCode != nil || ci.SourceJobID != nil {
		t.Fatalf("ci after reset = %+v", ci)
	}
	reviewer, err := checks.GetCheck(ctx, issue.ID, "reviewer")
	if err != nil {
		t.Fatalf("get reviewer: %v", err)
	}
	if reviewer.Verdict != CheckPending || reviewer.ExitCode != nil || reviewer.SourceJobID != nil {
		t.Fatalf("reviewer after reset = %+v", reviewer)
	}
	human, err := checks.GetCheck(ctx, issue.ID, "human")
	if err != nil {
		t.Fatalf("get human: %v", err)
	}
	if human.Verdict != CheckBlocked {
		t.Fatalf("human verdict = %q, want blocked", human.Verdict)
	}
}

// seedReadyChange creates an issue with an author session/change and marks the
// change ready so review threads can hang off it, returning the issue and
// change for the review-thread cross-check tests.
func seedReadyChange(t *testing.T, store *flowdb.Store, issues *IssueService) (Issue, Change) {
	t.Helper()
	ctx := context.Background()
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Cross-check target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	head_sha = ?,
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"deadbeef",
		"2026-01-01T00:00:00Z",
		ensured.Change.ID,
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}

	return issue, ensured.Change
}

func openReviewThread(t *testing.T, store *flowdb.Store, change Change) {
	t.Helper()
	threads := NewThreadService(store.DB())
	if _, err := threads.CreateThread(context.Background(), CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: "deadbeef",
		FilePath:        "main.go",
		Line:            1,
		Body:            "needs a fix",
		Actor:           "reviewer:r-local",
	}); err != nil {
		t.Fatalf("create review thread: %v", err)
	}
}

func TestReportReviewerSatisfiedWithOpenThreadsOverriddenToBlocked(t *testing.T) {
	ctx := context.Background()
	store, issues, checks := newCheckService(t)
	issue, change := seedReadyChange(t, store, issues)
	openReviewThread(t, store, change)

	reviewer, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Verdict:  CheckSatisfied,
		Details:  "looks good",
		Reporter: "worker:r-local",
	})
	if err != nil {
		t.Fatalf("report reviewer check: %v", err)
	}
	if reviewer.Verdict != CheckBlocked {
		t.Fatalf("verdict = %q, want blocked (overridden by open threads)", reviewer.Verdict)
	}
	if !strings.Contains(reviewer.Details, "open review threads") {
		t.Fatalf("details = %q, want it to mention open review threads", reviewer.Details)
	}
	if !strings.Contains(reviewer.Details, "looks good") {
		t.Fatalf("details = %q, want it to retain the original reason", reviewer.Details)
	}
}

func TestReportReviewerSatisfiedWithNoOpenThreadsStaysSatisfied(t *testing.T) {
	ctx := context.Background()
	store, issues, checks := newCheckService(t)
	issue, _ := seedReadyChange(t, store, issues)

	reviewer, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Verdict:  CheckSatisfied,
		Reporter: "worker:r-local",
	})
	if err != nil {
		t.Fatalf("report reviewer check: %v", err)
	}
	if reviewer.Verdict != CheckSatisfied {
		t.Fatalf("verdict = %q, want satisfied (no open threads)", reviewer.Verdict)
	}
}

func TestReportCICheckWithOpenThreadsNotOverridden(t *testing.T) {
	ctx := context.Background()
	store, issues, checks := newCheckService(t)
	issue, change := seedReadyChange(t, store, issues)
	openReviewThread(t, store, change)

	exitZero := 0
	ci, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "ci",
		Kind:     CheckKindCI,
		ExitCode: &exitZero,
		Reporter: "worker:w-local",
	})
	if err != nil {
		t.Fatalf("report ci check: %v", err)
	}
	if ci.Verdict != CheckSatisfied {
		t.Fatalf("verdict = %q, want satisfied (cross-check is reviewer-only)", ci.Verdict)
	}
}

func TestReportCheckRejectsSourceJobForDifferentIssue(t *testing.T) {
	ctx := context.Background()
	store, issues, checks := newCheckService(t)
	target, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Check target"})
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}
	other, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Other issue"})
	if err != nil {
		t.Fatalf("create other issue: %v", err)
	}
	workers := flowworker.NewService(store.DB())
	job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &other.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue source job: %v", err)
	}

	sourceJobID := job.ID
	_, err = checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:     target.ID,
		Name:        "fake-ci",
		SourceJobID: &sourceJobID,
	})
	if err == nil || !strings.Contains(err.Error(), "source job does not belong") {
		t.Fatalf("ReportCheck err = %v, want source job mismatch", err)
	}
}

func TestAcceptancePendingGate(t *testing.T) {
	ctx := context.Background()
	store, issues, checks := newCheckService(t)
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, nil, nil, Project{}, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Acceptance gate"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	required := true

	report := func(name string, kind CheckKind, verdict CheckVerdict) {
		t.Helper()
		if _, err := checks.ReportCheck(ctx, ReportCheckInput{
			IssueID:  issue.ID,
			Name:     name,
			Kind:     kind,
			Required: &required,
			Verdict:  verdict,
		}); err != nil {
			t.Fatalf("report %s: %v", name, err)
		}
	}

	assertGate := func(label string, wantAcceptance bool) {
		t.Helper()
		got, err := checkConfig.AcceptancePending(ctx, issue.ID)
		if err != nil {
			t.Fatalf("%s: acceptance pending: %v", label, err)
		}
		if got != wantAcceptance {
			t.Fatalf("%s: AcceptancePending = %v, want %v", label, got, wantAcceptance)
		}
	}

	// Critique not yet satisfied and verifier pending: not acceptance.
	report("unit", CheckKindCI, CheckPending)
	report("verifier", CheckKindVerifier, CheckPending)
	assertGate("critique pending", false)

	// Critique satisfied, verifier still pending: acceptance.
	report("unit", CheckKindCI, CheckSatisfied)
	assertGate("critique satisfied, verifier pending", true)

	pending, err := checks.VerifierPending(ctx, issue.ID)
	if err != nil {
		t.Fatalf("verifier pending: %v", err)
	}
	if !pending {
		t.Fatal("VerifierPending = false, want true with a pending verifier")
	}

	// Verifier satisfied closes the gate: no longer acceptance.
	report("verifier", CheckKindVerifier, CheckSatisfied)
	assertGate("verifier satisfied", false)

	pending, err = checks.VerifierPending(ctx, issue.ID)
	if err != nil {
		t.Fatalf("verifier pending after satisfy: %v", err)
	}
	if pending {
		t.Fatal("VerifierPending = true, want false once the verifier is satisfied")
	}

	// A non-required pending verifier neither counts as pending nor reopens
	// the acceptance gate.
	notRequired := false
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "verifier-optional",
		Kind:     CheckKindVerifier,
		Required: &notRequired,
		Verdict:  CheckPending,
	}); err != nil {
		t.Fatalf("report verifier-optional: %v", err)
	}
	assertGate("non-required verifier pending", false)

	pending, err = checks.VerifierPending(ctx, issue.ID)
	if err != nil {
		t.Fatalf("verifier pending with optional verifier: %v", err)
	}
	if pending {
		t.Fatal("VerifierPending = true, want false for a non-required verifier")
	}
}

func newCheckService(t *testing.T) (*flowdb.Store, *IssueService, *CheckService) {
	t.Helper()

	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store, NewIssueService(store.DB()), NewCheckService(store.DB())
}
