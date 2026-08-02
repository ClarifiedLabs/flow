package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestTaskFindingsRegistryAggregatesThreadsAcrossChanges(t *testing.T) {
	_, threads, tasks, task := newFindingsRegistryFixture(t)
	ctx := context.Background()

	// Every resolution bucket, spread over both the older and the newer change
	// so the registry must look across all of the task's changes:
	//   t1 certified (claimed fixed, then verified),
	//   t2 claimed not_warranted,
	//   t3 open,
	//   t4 claimed fixed,
	//   t5 claimed superseded,
	//   t6 claimed fixed then reopened.
	const (
		oldChange = "ch-reg-old"
		newChange = "ch-reg-new"
	)
	oldThreads := []ReviewThread{}
	for _, input := range []CreateThreadInput{
		{ChangeID: oldChange, AnchorCommitSHA: "1111111111111111111111111111111111111111", FilePath: "a.go", Line: 1, Body: "t1 opening finding", Actor: "reviewer"},
		{ChangeID: oldChange, AnchorCommitSHA: "1111111111111111111111111111111111111111", FilePath: "a.go", Line: 2, Body: "t2 opening finding", Actor: "reviewer"},
		{ChangeID: oldChange, AnchorCommitSHA: "1111111111111111111111111111111111111111", FilePath: "a.go", Line: 6, Body: "t6 opening finding", Actor: "reviewer"},
	} {
		thread, err := threads.CreateThread(ctx, input)
		if err != nil {
			t.Fatalf("create thread on %s: %v", input.ChangeID, err)
		}
		oldThreads = append(oldThreads, thread)
	}
	newThreads := []ReviewThread{}
	for _, input := range []CreateThreadInput{
		{ChangeID: newChange, AnchorCommitSHA: "2222222222222222222222222222222222222222", FilePath: "b.go", Line: 1, Body: "t3 opening finding", Actor: "reviewer"},
		{ChangeID: newChange, AnchorCommitSHA: "2222222222222222222222222222222222222222", FilePath: "b.go", Line: 2, Body: "t4 opening finding", Actor: "reviewer"},
		{ChangeID: newChange, AnchorCommitSHA: "2222222222222222222222222222222222222222", FilePath: "b.go", Line: 3, Body: "t5 opening finding", Actor: "reviewer"},
	} {
		thread, err := threads.CreateThread(ctx, input)
		if err != nil {
			t.Fatalf("create thread on %s: %v", input.ChangeID, err)
		}
		newThreads = append(newThreads, thread)
	}

	claim := func(thread ReviewThread, kind ReviewClaimKind, body string, by string) {
		t.Helper()
		if _, err := threads.ClaimThread(ctx, ClaimThreadInput{
			ThreadID: thread.ID, Kind: kind, Body: body, Actor: by,
			ClaimCommitSHA: "3333333333333333333333333333333333333333",
		}); err != nil {
			t.Fatalf("claim thread %s as %s: %v", thread.ID, kind, err)
		}
	}
	claim(oldThreads[0], ClaimFixed, "", "author")
	if _, err := threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: oldThreads[0].ID, Body: "verified", Actor: "verifier"}); err != nil {
		t.Fatalf("certify thread: %v", err)
	}
	claim(oldThreads[1], ClaimNotWarranted, "not a bug", "author")
	claim(oldThreads[2], ClaimFixed, "", "author")
	if _, err := threads.ReopenThread(ctx, VerifyThreadInput{ThreadID: oldThreads[2].ID, Body: "still broken", Actor: "verifier"}); err != nil {
		t.Fatalf("reopen thread: %v", err)
	}
	claim(newThreads[1], ClaimFixed, "", "author")
	claim(newThreads[2], ClaimSuperseded, "covered elsewhere", "author")

	// Two deferred follow-ups: one create_task and one use_existing_task.
	existing, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Existing follow-up target"})
	if err != nil {
		t.Fatalf("create existing follow-up task: %v", err)
	}
	deferred, err := tasks.ApplyReviewFollowUp(ctx, ApplyReviewFollowUpInput{
		SourceTaskID:   task.ID,
		SourceChangeID: oldChange,
		CheckName:      "lint-review",
		Finding: ReviewFollowUpFinding{
			SHA: "1111111111111111111111111111111111111111", File: "a.go", Line: 10,
			Body: "defer this cleanup", Severity: "medium", Requirement: "req",
		},
		TaskAction: ReviewFollowUpTaskAction{Action: ReviewFollowUpCreateTask, Title: "Deferred cleanup task", Body: "Clean up the deferred resource."},
	})
	if err != nil {
		t.Fatalf("apply create_task follow-up: %v", err)
	}
	if _, err := tasks.ApplyReviewFollowUp(ctx, ApplyReviewFollowUpInput{
		SourceTaskID:   task.ID,
		SourceChangeID: oldChange,
		CheckName:      "security-review",
		Finding: ReviewFollowUpFinding{
			SHA: "1111111111111111111111111111111111111111", File: "b.go", Line: 20,
			Body: "audit this path", Severity: "low", Requirement: "req",
		},
		TaskAction: ReviewFollowUpTaskAction{Action: ReviewFollowUpUseExistingTask, TaskID: existing.ID},
	}); err != nil {
		t.Fatalf("apply use_existing_task follow-up: %v", err)
	}

	registry, err := threads.TaskFindingsRegistry(ctx, task.ID)
	if err != nil {
		t.Fatalf("load findings registry: %v", err)
	}
	if registry.TaskID != task.ID {
		t.Fatalf("registry task id = %q, want %q", registry.TaskID, task.ID)
	}
	if len(registry.Findings) != 6 {
		t.Fatalf("registry findings = %d, want 6: %+v", len(registry.Findings), registry.Findings)
	}

	byID := make(map[string]TaskReviewFinding, len(registry.Findings))
	changesSeen := map[string]bool{}
	for _, finding := range registry.Findings {
		byID[finding.ID] = finding
		changesSeen[finding.ChangeID] = true
	}
	// Findings from the older, non-latest change must not be lost.
	if !changesSeen[oldChange] || !changesSeen[newChange] {
		t.Fatalf("registry changes = %v, want both %s and %s", changesSeen, oldChange, newChange)
	}

	certified := byID[oldThreads[0].ID]
	if certified.State != ThreadCertified || certified.ClaimKind == nil || *certified.ClaimKind != ClaimFixed {
		t.Fatalf("certified finding = %+v, want state certified with fixed claim", certified)
	}
	if certified.ClaimedBy == nil || *certified.ClaimedBy != "author" {
		t.Fatalf("certified finding claimed_by = %v, want author", certified.ClaimedBy)
	}
	if certified.CertifiedBy == nil || *certified.CertifiedBy != "verifier" {
		t.Fatalf("certified finding certified_by = %v, want verifier", certified.CertifiedBy)
	}
	if certified.CertifiedAt == nil || certified.ClaimedAt == nil {
		t.Fatalf("certified finding timestamps = claim %v certify %v, want both set", certified.ClaimedAt, certified.CertifiedAt)
	}
	if certified.Finding != "t1 opening finding" {
		t.Fatalf("certified finding body = %q, want the opening comment", certified.Finding)
	}
	if certified.ChangeID != oldChange || certified.FilePath != "a.go" || certified.Line != 1 {
		t.Fatalf("certified finding anchor = %s %s:%d, want %s a.go:1", certified.ChangeID, certified.FilePath, certified.Line, oldChange)
	}

	notWarranted := byID[oldThreads[1].ID]
	if notWarranted.State != ThreadClaimed || notWarranted.ClaimKind == nil || *notWarranted.ClaimKind != ClaimNotWarranted {
		t.Fatalf("not_warranted finding = %+v, want claimed with not_warranted claim", notWarranted)
	}
	fixed := byID[newThreads[1].ID]
	if fixed.State != ThreadClaimed || fixed.ClaimKind == nil || *fixed.ClaimKind != ClaimFixed {
		t.Fatalf("fixed finding = %+v, want claimed with fixed claim", fixed)
	}
	if fixed.ClaimCommitSHA == nil || *fixed.ClaimCommitSHA != "3333333333333333333333333333333333333333" {
		t.Fatalf("fixed finding claim_commit_sha = %v, want the claim sha", fixed.ClaimCommitSHA)
	}
	superseded := byID[newThreads[2].ID]
	if superseded.State != ThreadClaimed || superseded.ClaimKind == nil || *superseded.ClaimKind != ClaimSuperseded {
		t.Fatalf("superseded finding = %+v, want claimed with superseded claim", superseded)
	}
	open := byID[newThreads[0].ID]
	if open.State != ThreadOpen || open.ClaimKind != nil || open.ClaimedBy != nil || open.CertifiedBy != nil {
		t.Fatalf("open finding = %+v, want open with no claim fields", open)
	}
	if open.Finding != "t3 opening finding" {
		t.Fatalf("open finding body = %q, want the opening comment", open.Finding)
	}
	reopened := byID[oldThreads[2].ID]
	if reopened.State != ThreadReopened || reopened.ReopenedBy == nil || *reopened.ReopenedBy != "verifier" {
		t.Fatalf("reopened finding = %+v, want reopened by verifier", reopened)
	}
	if reopened.ReopenedAt == nil {
		t.Fatalf("reopened finding reopened_at is nil")
	}

	if len(registry.FollowUps) != 2 {
		t.Fatalf("registry follow-ups = %d, want 2: %+v", len(registry.FollowUps), registry.FollowUps)
	}
	byAction := map[string]TaskReviewFollowUp{}
	for _, followUp := range registry.FollowUps {
		byAction[followUp.Action] = followUp
	}
	created := byAction[ReviewFollowUpCreateTask]
	if created.TargetTaskID != deferred.Task.ID || created.TargetTaskTitle != "Deferred cleanup task" {
		t.Fatalf("create_task follow-up = %+v, want target %s titled %q", created, deferred.Task.ID, "Deferred cleanup task")
	}
	if created.CheckName != "lint-review" || created.FindingHash == "" || created.CreatedAt.IsZero() {
		t.Fatalf("create_task follow-up provenance = %+v, want check lint-review with hash and timestamp", created)
	}
	if created.RelatedAt == nil {
		t.Fatalf("create_task follow-up related_at = nil, want the backing relation timestamp")
	}
	used := byAction[ReviewFollowUpUseExistingTask]
	if used.TargetTaskID != existing.ID || used.TargetTaskTitle != "Existing follow-up target" {
		t.Fatalf("use_existing_task follow-up = %+v, want target %s titled %q", used, existing.ID, "Existing follow-up target")
	}
	if used.CheckName != "security-review" {
		t.Fatalf("use_existing_task follow-up check = %q, want security-review", used.CheckName)
	}

	wantSummary := TaskFindingsSummary{
		ResolvedFixed:        1,
		ResolvedNotWarranted: 1,
		ResolvedSuperseded:   1,
		Certified:            1,
		Unresolved:           2,
		DeferredToTask:       2,
	}
	if registry.Summary != wantSummary {
		t.Fatalf("registry summary = %+v, want %+v", registry.Summary, wantSummary)
	}
}

func TestTaskFindingsRegistryEmptyForTaskWithoutFindings(t *testing.T) {
	_, threads, _, task := newFindingsRegistryFixture(t)
	ctx := context.Background()

	registry, err := threads.TaskFindingsRegistry(ctx, task.ID)
	if err != nil {
		t.Fatalf("load empty findings registry: %v", err)
	}
	if len(registry.Findings) != 0 || len(registry.FollowUps) != 0 {
		t.Fatalf("empty registry = %+v, want no findings and no follow-ups", registry)
	}
	if registry.Summary != (TaskFindingsSummary{}) {
		t.Fatalf("empty registry summary = %+v, want all zeros", registry.Summary)
	}
}

func TestTaskFindingsRegistryUnknownTaskIsNoRows(t *testing.T) {
	_, threads, _, _ := newFindingsRegistryFixture(t)
	ctx := context.Background()

	_, err := threads.TaskFindingsRegistry(ctx, "t-does-not-exist")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown task registry error = %v, want sql.ErrNoRows", err)
	}
}

func newFindingsRegistryFixture(t *testing.T) (*flowdb.Store, *ThreadService, *TaskService, Task) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Findings registry target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	insertChangeForTest(t, store.DB(), task.ID, "ch-reg-old", "task/reg-old", false)
	insertChangeForTest(t, store.DB(), task.ID, "ch-reg-new", "task/reg-new", false)

	return store, NewThreadService(store.DB()), tasks, task
}
