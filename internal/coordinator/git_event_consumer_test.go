package coordinator

import (
	"context"
	"testing"
)

// pushOutOfBandCommit advances the fixture's issue branch on the exchange
// without any session-ready signal, returning the new head SHA.
func pushOutOfBandCommit(t *testing.T, fixture mergeServiceFixture, file string, content string) string {
	t.Helper()
	branch := "issue/" + fixture.issue.ID
	writeReconcileFile(t, fixture.repoPath, file, content)
	if err := runReconcileGit(fixture.repoPath, nil, "add", file); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "out-of-band "+file); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	head, err := reconcileGitOutput(fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.exchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push out-of-band commit: %v", err)
	}
	return head
}

func TestGitEventConsumerWatermarkSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	events := NewGitEventService(fixture.db)

	oldHead := fixture.change.HeadSHA
	newHead := pushOutOfBandCommit(t, fixture, "extra.go", "package app\n\nconst Extra = 2\n")
	if _, err := events.Record(ctx, GitEvent{
		OldSHA: oldHead,
		NewSHA: newHead,
		Ref:    "refs/heads/issue/" + fixture.issue.ID,
	}, GitEventSourceAPI); err != nil {
		t.Fatalf("record git event: %v", err)
	}

	// First consumer instance drains the recorded events.
	first := NewGitEventConsumer(fixture.db, fixture.project)
	ran, err := first.ConsumeNew(ctx)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if !ran {
		t.Fatal("first consumer did not run despite a recorded git event")
	}

	// A fresh consumer over the same db (simulating a server restart) must read
	// the persisted watermark and NOT re-run reconcile when no new events
	// arrived since the previous instance consumed them.
	second := NewGitEventConsumer(fixture.db, fixture.project)
	ran, err = second.ConsumeNew(ctx)
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	if ran {
		t.Fatal("recreated consumer re-ran reconcile despite no new git events (watermark not persisted)")
	}
}

func TestGitEventConsumerSyncsHeadAndResetsChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	consumer := NewGitEventConsumer(fixture.db, fixture.project)
	events := NewGitEventService(fixture.db)

	// First pass consumes the events recorded during fixture setup (if any)
	// so the assertions below isolate the out-of-band push.
	if _, err := consumer.ConsumeNew(ctx); err != nil {
		t.Fatalf("initial consume: %v", err)
	}

	required := true
	if _, err := fixture.checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  fixture.issue.ID,
		Name:     "unit",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy check: %v", err)
	}

	oldHead := fixture.change.HeadSHA
	newHead := pushOutOfBandCommit(t, fixture, "extra.go", "package app\n\nconst Extra = 2\n")
	if _, err := events.Record(ctx, GitEvent{
		OldSHA: oldHead,
		NewSHA: newHead,
		Ref:    "refs/heads/issue/" + fixture.issue.ID,
	}, GitEventSourceAPI); err != nil {
		t.Fatalf("record git event: %v", err)
	}

	ran, err := consumer.ConsumeNew(ctx)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !ran {
		t.Fatal("consumer did not run despite a new git event")
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.HeadSHA != newHead {
		t.Fatalf("change head = %s, want resynced tip %s", change.HeadSHA, newHead)
	}
	var verdict string
	if err := fixture.db.QueryRow(
		`SELECT verdict FROM checks WHERE issue_id = ? AND name = 'unit'`, fixture.issue.ID).Scan(&verdict); err != nil {
		t.Fatalf("read check verdict: %v", err)
	}
	if verdict != string(CheckPending) {
		t.Fatalf("check verdict = %q, want pending after out-of-band push", verdict)
	}

	// The consumer is event-gated: a push that records no git event does not
	// trigger a pass, and an already-consumed watermark stays consumed.
	staleHead := pushOutOfBandCommit(t, fixture, "more.go", "package app\n\nconst More = 3\n")
	ran, err = consumer.ConsumeNew(ctx)
	if err != nil {
		t.Fatalf("gated consume: %v", err)
	}
	if ran {
		t.Fatal("consumer ran with no new git events")
	}
	change, err = fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.HeadSHA != newHead {
		t.Fatalf("change head = %s, want %s (no event recorded for %s yet)", change.HeadSHA, newHead, staleHead)
	}
}
