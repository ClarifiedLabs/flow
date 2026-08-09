package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestReportCheckMapsCIExitCodeToVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks, checks := newCheckService(t)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Check target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exitZero := 0
	passing, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
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
		TaskID:   task.ID,
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

	listed, err := checks.ListChecks(ctx, task.ID)
	if err != nil {
		t.Fatalf("list checks: %v", err)
	}
	if len(listed) != 1 || listed[0].Verdict != CheckBlocked {
		t.Fatalf("listed checks = %+v", listed)
	}
}

func TestReviewerCheckRequiresExplicitResultAndKeepsErrorsInReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks, checks := newCheckService(t)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Reviewer execution failure"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	required := true
	exitFailure := 1
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "security-review",
		Kind:     CheckKindReviewer,
		Required: &required,
		ExitCode: &exitFailure,
	}); err == nil || !strings.Contains(err.Error(), "explicit structured verdict") {
		t.Fatalf("implicit reviewer verdict error = %v", err)
	}
	check, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "security-review",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckErrored,
		ExitCode: &exitFailure,
		Details:  "agent transcript invalid",
	})
	if err != nil {
		t.Fatalf("report reviewer execution error: %v", err)
	}
	if check.Verdict != CheckErrored {
		t.Fatalf("reviewer verdict = %s, want errored", check.Verdict)
	}
	state, err := checks.ReviewState(ctx, task.ID)
	if err != nil {
		t.Fatalf("load review state: %v", err)
	}
	if state != ReviewInReview {
		t.Fatalf("review state = %s, want in_review", state)
	}
}

func TestResetAutomatedChecksForNewRevisionLeavesHumanChecksBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks, checks := newCheckService(t)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Reset target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "ci",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("report ci: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckBlocked,
	}); err != nil {
		t.Fatalf("report reviewer: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "human",
		Kind:     CheckKindHuman,
		Required: &required,
		Verdict:  CheckBlocked,
	}); err != nil {
		t.Fatalf("report human: %v", err)
	}

	reset, err := checks.ResetAutomatedChecksForNewRevision(ctx, task.ID)
	if err != nil {
		t.Fatalf("reset checks: %v", err)
	}
	if reset != 2 {
		t.Fatalf("reset = %d, want two automated checks", reset)
	}
	ci, err := checks.GetCheck(ctx, task.ID, "ci")
	if err != nil {
		t.Fatalf("get ci: %v", err)
	}
	if ci.Verdict != CheckPending || ci.ExitCode != nil || ci.SourceJobID != nil {
		t.Fatalf("ci after reset = %+v", ci)
	}
	reviewer, err := checks.GetCheck(ctx, task.ID, "reviewer")
	if err != nil {
		t.Fatalf("get reviewer: %v", err)
	}
	if reviewer.Verdict != CheckPending || reviewer.ExitCode != nil || reviewer.SourceJobID != nil {
		t.Fatalf("reviewer after reset = %+v", reviewer)
	}
	human, err := checks.GetCheck(ctx, task.ID, "human")
	if err != nil {
		t.Fatalf("get human: %v", err)
	}
	if human.Verdict != CheckBlocked {
		t.Fatalf("human verdict = %q, want blocked", human.Verdict)
	}
}

// seedReadyChange creates a ready change directly so review-thread checks do
// not depend on a particular workflow graph or agent-node setup.
func seedReadyChange(t *testing.T, store *flowdb.Store, tasks *TaskService) (Task, Change) {
	t.Helper()
	ctx := context.Background()
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Cross-check target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	insertChangeForTest(t, store.DB(), task.ID, "ch-cross-check", "task/cross-check", false)
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	head_sha = ?,
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"deadbeef",
		"2026-01-01T00:00:00Z",
		"ch-cross-check",
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}
	change, err := NewSessionService(store.DB(), tasks, nil).GetChange(ctx, "ch-cross-check")
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	return task, change
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
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	task, change := seedReadyChange(t, store, tasks)
	openReviewThread(t, store, change)

	reviewer, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
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
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	task, _ := seedReadyChange(t, store, tasks)

	reviewer, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
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
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	task, change := seedReadyChange(t, store, tasks)
	openReviewThread(t, store, change)

	exitZero := 0
	ci, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
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

func TestReportCheckRejectsSourceJobForDifferentTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	target, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Check target"})
	if err != nil {
		t.Fatalf("create target task: %v", err)
	}
	other, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Other task"})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	workers := flowworker.NewService(store.DB())
	job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &other.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload:        map[string]any{"blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue source job: %v", err)
	}

	sourceJobID := job.ID
	_, err = checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:      target.ID,
		Name:        "fake-ci",
		SourceJobID: &sourceJobID,
	})
	if err == nil || !strings.Contains(err.Error(), "source job does not belong") {
		t.Fatalf("ReportCheck err = %v, want source job mismatch", err)
	}
}

func TestWorkerCheckReportRejectsLeaseReleasedBeforeAtomicWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	task, change := seedReadyChange(t, store, tasks)
	workers := flowworker.NewService(store.DB())
	job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: &change.ID, Role: flowworker.RoleCI, CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{"check_name": "unit", "change_id": change.ID, "head_sha": change.HeadSHA, "blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue check job: %v", err)
	}
	leaseID := "l-check"
	now := time.Now().UTC()
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'claimed', updated_at = ? WHERE id = ?`, formatTime(now), job.ID); err != nil {
		t.Fatalf("claim check job: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-check', 'ephemeral', ?, ?)`, leaseID, job.ID, formatTime(now), formatTime(now.Add(time.Minute))); err != nil {
		t.Fatalf("create check lease: %v", err)
	}
	if _, err := workers.MarkJobRunning(ctx, leaseID); err != nil {
		t.Fatalf("mark check job running: %v", err)
	}

	required := true
	sourceJobID := job.ID
	input := ReportCheckInput{
		TaskID: task.ID, Name: "unit", Kind: CheckKindCI, Required: &required,
		Verdict: CheckSatisfied, SourceJobID: &sourceJobID, Reporter: "w-check",
		WorkerID: "w-check", WorkerLeaseID: leaseID,
	}
	if _, err := checks.ReportCheck(ctx, input); err != nil {
		t.Fatalf("report check with live lease: %v", err)
	}
	if _, err := workers.ReleaseLease(ctx, leaseID, flowworker.JobCanceled); err != nil {
		t.Fatalf("release check lease: %v", err)
	}

	input.Verdict = CheckBlocked
	input.Details = "late result"
	if _, err := checks.ReportCheck(ctx, input); !errors.Is(err, ErrCheckReportLeaseInvalid) {
		t.Fatalf("late worker report error = %v, want lease invalid", err)
	}
	check, err := checks.GetCheck(ctx, task.ID, input.Name)
	if err != nil || check.Verdict != CheckSatisfied || check.Details == "late result" {
		t.Fatalf("check after late worker report = %+v err=%v, want original satisfied result", check, err)
	}
}

func TestAcceptancePendingGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks, checks := newCheckService(t)
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, nil, nil, Project{}, CheckConfigServiceOptions{})
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Acceptance gate"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	required := true

	report := func(name string, kind CheckKind, verdict CheckVerdict) {
		t.Helper()
		if _, err := checks.ReportCheck(ctx, ReportCheckInput{
			TaskID:   task.ID,
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
		got, err := checkConfig.AcceptancePending(ctx, task.ID)
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

	pending, err := checks.VerifierPending(ctx, task.ID)
	if err != nil {
		t.Fatalf("verifier pending: %v", err)
	}
	if !pending {
		t.Fatal("VerifierPending = false, want true with a pending verifier")
	}

	// Verifier satisfied closes the gate: no longer acceptance.
	report("verifier", CheckKindVerifier, CheckSatisfied)
	assertGate("verifier satisfied", false)

	pending, err = checks.VerifierPending(ctx, task.ID)
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
		TaskID:   task.ID,
		Name:     "verifier-optional",
		Kind:     CheckKindVerifier,
		Required: &notRequired,
		Verdict:  CheckPending,
	}); err != nil {
		t.Fatalf("report verifier-optional: %v", err)
	}
	assertGate("non-required verifier pending", false)

	pending, err = checks.VerifierPending(ctx, task.ID)
	if err != nil {
		t.Fatalf("verifier pending with optional verifier: %v", err)
	}
	if pending {
		t.Fatal("VerifierPending = true, want false for a non-required verifier")
	}
}

func newCheckService(t *testing.T) (*flowdb.Store, *TaskService, *CheckService) {
	t.Helper()

	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store, NewTaskService(store.DB(), "p-test"), NewCheckService(store.DB())
}
