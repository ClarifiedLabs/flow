package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestResetIssueDiscardsAuthoringArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectFixture(t)
	db := fixture.store.DB()

	issues := NewIssueService(db)
	workers := flowworker.NewService(db)
	checks := NewCheckService(db)
	sessions := NewSessionServiceWithOptions(db, issues, workers, SessionServiceOptions{
		Project: fixture.project,
	})

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Reset issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	// Publish the issue branch the way an author would, then leave a stale
	// blocked check behind: the artifacts a reset must discard.
	branchRef := "refs/heads/" + ensured.Change.Branch
	if err := runReconcileGit(fixture.repoPath, nil, "push", fixture.project.ExchangeURL, "main:"+branchRef); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}
	required := true
	exitCode := 1
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "review",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckBlocked,
		ExitCode: &exitCode,
		Reporter: "test",
	}); err != nil {
		t.Fatalf("report stale check: %v", err)
	}

	reset, err := sessions.ResetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("reset issue: %v", err)
	}
	if reset.ID != issue.ID {
		t.Fatalf("reset issue ID = %q, want %q", reset.ID, issue.ID)
	}

	job, err := workers.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get author job: %v", err)
	}
	if job.State != flowworker.JobCanceled {
		t.Fatalf("author job state = %q, want canceled", job.State)
	}
	changes, err := sessions.changesForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("list changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes after reset = %+v, want none", changes)
	}
	if err := runReconcileGit(fixture.project.ExchangePath, nil, "rev-parse", "--verify", "--quiet", branchRef); err == nil {
		t.Fatalf("exchange branch %s still exists after reset", branchRef)
	}
	var checkCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks WHERE issue_id = ?`, issue.ID).Scan(&checkCount); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("checks after reset = %d, want 0", checkCount)
	}

	// A fresh author attempt starts over: same branch name, no recorded head.
	fresh, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure fresh author job: %v", err)
	}
	if fresh.Existing {
		t.Fatal("fresh author job reused canceled job")
	}
	if fresh.Change.ID == ensured.Change.ID {
		t.Fatalf("fresh change reused old change %s", ensured.Change.ID)
	}
	if fresh.Change.Branch != ensured.Change.Branch {
		t.Fatalf("fresh change branch = %q, want %q", fresh.Change.Branch, ensured.Change.Branch)
	}
	if fresh.Change.HeadSHA != "" {
		t.Fatalf("fresh change head = %q, want empty", fresh.Change.HeadSHA)
	}
}

func TestResetIssueRejectsMergedChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues := fixture.sessions, fixture.issues

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Merged issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE changes SET merged_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), ensured.Change.ID); err != nil {
		t.Fatalf("mark change merged: %v", err)
	}

	if _, err := sessions.ResetIssue(ctx, issue.ID); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Fatalf("reset of merged issue: err = %v, want merged-change rejection", err)
	}
}

func TestResetIssueRejectsClosedIssues(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues := fixture.sessions, fixture.issues

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Closed issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.CloseIssue(ctx, issue.ID); err != nil {
		t.Fatalf("close issue: %v", err)
	}

	if _, err := sessions.ResetIssue(ctx, issue.ID); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("reset of closed issue: err = %v, want closed rejection", err)
	}
}
