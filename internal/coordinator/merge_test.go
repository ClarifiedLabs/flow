package coordinator

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestMergeIssueSquashesApprovedChangeAndExcludesHandoffFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
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
	board, err := fixture.issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board before merge: %v", err)
	}
	assertIssueIDs(t, board.Board.NeedsAttention, []string{fixture.issue.ID})
	if got := board.LaneStates[fixture.issue.ID]; got != LaneStateReadyToMerge {
		t.Fatalf("lane state = %q, want ready_to_merge", got)
	}

	merge, err := fixture.merges.MergeIssue(ctx, fixture.issue.ID)
	if err != nil {
		t.Fatalf("merge issue: %v", err)
	}
	if merge.Issue.ScheduleState != ScheduleClosed || merge.Change.MergedAt == nil || merge.MergeSHA == "" {
		t.Fatalf("merge result = %+v", merge)
	}
	app, present, err := flowgit.ReadTextFileAtRef(ctx, fixture.exchangePath, "refs/heads/main", "app.go")
	if err != nil {
		t.Fatalf("read merged app: %v", err)
	}
	if !present || app != "package app\n\nconst Value = 1\n" {
		t.Fatalf("merged app present=%t content=%q", present, app)
	}
	if _, present, err := flowgit.ReadTextFileAtRef(ctx, fixture.exchangePath, "refs/heads/main", ".flow/session/state.json"); err != nil {
		t.Fatalf("read session state: %v", err)
	} else if present {
		t.Fatal(".flow/session file was included in base merge")
	}
	baseHead, err := reconcileGitOutput("", nil, "--git-dir", fixture.exchangePath, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("read base head: %v", err)
	}
	if baseHead != merge.MergeSHA {
		t.Fatalf("base head = %s, want merge sha %s", baseHead, merge.MergeSHA)
	}
}

func TestMergePushFailureDoesNotMarkChangeMerged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
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
	if err := os.WriteFile(filepath.Join(fixture.exchangePath, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("replace pre-receive hook: %v", err)
	}

	if _, err := fixture.merges.MergeIssue(ctx, fixture.issue.ID); err == nil {
		t.Fatal("merge succeeded despite rejecting pre-receive hook")
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatalf("change merged_at = %v, want nil", change.MergedAt)
	}
	issue, err := fixture.issues.GetIssue(ctx, fixture.issue.ID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if issue.ScheduleState == ScheduleClosed {
		t.Fatalf("issue was closed despite failed push: %+v", issue)
	}
	if _, present, err := flowgit.ReadTextFileAtRef(ctx, fixture.exchangePath, "refs/heads/main", "app.go"); err != nil {
		t.Fatalf("read base app: %v", err)
	} else if present {
		t.Fatal("base branch advanced despite failed push")
	}

	// The ambiguous failure leaves the intent open for the recovery pass,
	// which detects the base never moved and discards it.
	if n := openMergeIntentCount(t, fixture.db); n != 1 {
		t.Fatalf("open merge intents after failed push = %d, want 1", n)
	}
	recovered, err := fixture.merges.RecoverPendingMerges(ctx)
	if err != nil {
		t.Fatalf("recover pending merges: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	if n := openMergeIntentCount(t, fixture.db); n != 0 {
		t.Fatalf("open merge intents after recovery = %d, want 0", n)
	}
}

func TestMergeChangeRejectsNonCurrentReadyChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
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
	now := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	if _, err := fixture.db.ExecContext(ctx, `
INSERT INTO changes (
	id,
	issue_id,
	branch,
	base,
	head_sha,
	ready_at,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"ch-current",
		fixture.issue.ID,
		"issue/"+fixture.issue.ID+"-current",
		"main",
		fixture.change.HeadSHA,
		now,
		now,
		now,
	); err != nil {
		t.Fatalf("insert newer ready change: %v", err)
	}

	if _, err := fixture.merges.MergeChange(ctx, fixture.change.ID); err == nil {
		t.Fatal("older ready change merged; want rejection")
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get older change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatalf("older change merged_at = %v, want nil", change.MergedAt)
	}
}

func TestRecordMergedRequiresUnchangedHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	originalHead := fixture.change.HeadSHA
	if _, err := fixture.sessions.UpdateChangeHead(ctx, fixture.change.ID, "changed-head"); err != nil {
		t.Fatalf("change head: %v", err)
	}

	if _, _, err := fixture.merges.recordMerged(ctx, fixture.issue.ID, fixture.change.ID, originalHead); err == nil {
		t.Fatal("record merged succeeded with stale head")
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatalf("change merged_at = %v, want nil", change.MergedAt)
	}
}

// strandMerge simulates the crash window between the exchange push and
// recordMerged: it drives a real merge, then reverts the DB record and
// re-opens the merge intent, leaving the base branch advanced on the exchange
// with the change still unmerged.
func strandMerge(t *testing.T, fixture mergeServiceFixture) MergeResult {
	t.Helper()
	ctx := context.Background()
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
	merge, err := fixture.merges.MergeIssue(ctx, fixture.issue.ID)
	if err != nil {
		t.Fatalf("merge issue: %v", err)
	}
	if _, err := fixture.db.ExecContext(ctx,
		`UPDATE changes SET merged_at = NULL WHERE id = ?`, fixture.change.ID); err != nil {
		t.Fatalf("revert merged_at: %v", err)
	}
	if _, err := fixture.db.ExecContext(ctx,
		`UPDATE issues SET schedule_state = 'up_next', closed_at = NULL WHERE id = ?`, fixture.issue.ID); err != nil {
		t.Fatalf("reopen issue: %v", err)
	}
	if _, err := fixture.db.ExecContext(ctx, `
INSERT INTO merge_intents
	(id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha, created_at)
VALUES ('mi-strand', ?, ?, 'main', ?, ?, ?, ?)`,
		fixture.issue.ID, fixture.change.ID, fixture.exchangePath,
		fixture.change.HeadSHA, merge.PreviousBaseSHA,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert stranded intent: %v", err)
	}
	return merge
}

func openMergeIntentCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM merge_intents WHERE completed_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count open merge intents: %v", err)
	}
	return n
}

func TestRecoverPendingMergeCompletesAfterPushButBeforeRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	strandMerge(t, fixture)

	recovered, err := fixture.merges.RecoverPendingMerges(ctx)
	if err != nil {
		t.Fatalf("recover pending merges: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt == nil {
		t.Fatal("stranded merge not completed: merged_at still nil")
	}
	issue, err := fixture.issues.GetIssue(ctx, fixture.issue.ID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if issue.ScheduleState != ScheduleClosed {
		t.Fatalf("issue schedule state = %q, want closed", issue.ScheduleState)
	}
	if n := openMergeIntentCount(t, fixture.db); n != 0 {
		t.Fatalf("open merge intents = %d, want 0", n)
	}
}

func TestMergeIssueHealsStrandedMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	stranded := strandMerge(t, fixture)

	// A re-driven merge hits ErrNoMergeChanges (the content is already in the
	// base) and must complete the record instead of looping forever.
	merge, err := fixture.merges.MergeIssue(ctx, fixture.issue.ID)
	if err != nil {
		t.Fatalf("re-driven merge did not heal: %v", err)
	}
	if merge.Change.MergedAt == nil || merge.Issue.ScheduleState != ScheduleClosed {
		t.Fatalf("healed merge result = %+v", merge)
	}
	if merge.PreviousBaseSHA != stranded.PreviousBaseSHA {
		t.Fatalf("healed previous base sha = %s, want the intent's %s", merge.PreviousBaseSHA, stranded.PreviousBaseSHA)
	}
	if n := openMergeIntentCount(t, fixture.db); n != 0 {
		t.Fatalf("open merge intents = %d, want 0", n)
	}
}

func TestRecoverPendingMergeDiscardsUnpushedIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	baseTip, ok, err := flowgit.BranchTip(ctx, fixture.exchangePath, "main")
	if err != nil || !ok {
		t.Fatalf("read base tip: ok=%t err=%v", ok, err)
	}
	// An intent whose recorded base tip still matches the exchange: the crash
	// happened before the push landed.
	if _, err := fixture.db.ExecContext(ctx, `
INSERT INTO merge_intents
	(id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha, created_at)
VALUES ('mi-unpushed', ?, ?, 'main', ?, ?, ?, ?)`,
		fixture.issue.ID, fixture.change.ID, fixture.exchangePath,
		fixture.change.HeadSHA, baseTip,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert unpushed intent: %v", err)
	}

	recovered, err := fixture.merges.RecoverPendingMerges(ctx)
	if err != nil {
		t.Fatalf("recover pending merges: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatalf("unpushed intent marked merged: %v", change.MergedAt)
	}
	if n := openMergeIntentCount(t, fixture.db); n != 0 {
		t.Fatalf("open merge intents = %d, want 0 (stale intent must be deleted)", n)
	}
}

func TestRecoverPendingMergeRejectsNonEquivalentContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMergeServiceFixture(t)
	// The base "advanced" relative to the intent, but the branch still squashes
	// to real changes — whatever moved the base, it was not this merge, so the
	// intent must be discarded without flipping merged_at.
	if _, err := fixture.db.ExecContext(ctx, `
INSERT INTO merge_intents
	(id, issue_id, change_id, base_branch, exchange_path, head_sha, previous_base_sha, created_at)
VALUES ('mi-foreign', ?, ?, 'main', ?, ?, 'not-the-current-base-tip', ?)`,
		fixture.issue.ID, fixture.change.ID, fixture.exchangePath,
		fixture.change.HeadSHA,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert foreign intent: %v", err)
	}

	recovered, err := fixture.merges.RecoverPendingMerges(ctx)
	if err != nil {
		t.Fatalf("recover pending merges: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	change, err := fixture.sessions.GetChange(ctx, fixture.change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatal("non-equivalent content was marked merged")
	}
	if n := openMergeIntentCount(t, fixture.db); n != 0 {
		t.Fatalf("open merge intents = %d, want 0", n)
	}
}

type mergeServiceFixture struct {
	repoPath     string
	exchangePath string
	exchangeURL  string
	project      Project
	issue        Issue
	change       Change
	db           *sql.DB
	issues       *IssueService
	checks       *CheckService
	sessions     *SessionService
	merges       *MergeService
}

func newMergeServiceFixture(t *testing.T) mergeServiceFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	store := fixture.store

	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	checks := NewCheckService(store.DB())
	merges := NewMergeService(store.DB(), issues, sessions, fixture.project)
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Merge target",
		RequiresHumanReview: &requiresHuman,
	})
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

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	writeReconcileFile(t, repoPath, "app.go", "package app\n\nconst Value = 1\n")
	writeReconcileFile(t, repoPath, ".flow/session/state.json", "{}\n")
	if err := runReconcileGit(repoPath, nil, "add", "app.go", ".flow/session/state.json"); err != nil {
		t.Fatalf("git add issue files: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "add app change"); err != nil {
		t.Fatalf("commit issue branch: %v", err)
	}
	head, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read issue head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", fixture.project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, head)
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = ?, updated_at = ?
WHERE id = ?`, now, now, change.ID); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}
	change, err = sessions.GetChange(ctx, change.ID)
	if err != nil {
		t.Fatalf("reload change: %v", err)
	}

	return mergeServiceFixture{
		repoPath:     repoPath,
		exchangePath: fixture.project.ExchangePath,
		exchangeURL:  fixture.project.ExchangeURL,
		project:      fixture.project,
		issue:        issue,
		change:       change,
		db:           store.DB(),
		issues:       issues,
		checks:       checks,
		sessions:     sessions,
		merges:       merges,
	}
}
