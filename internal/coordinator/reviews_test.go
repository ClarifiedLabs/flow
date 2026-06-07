package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestThreadLifecycleClaimCertifyAndReopen(t *testing.T) {
	_, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()

	thread, err := threads.CreateThread(ctx, CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            42,
		Context:         "func run()",
		Body:            "This needs a guard.",
		Actor:           "reviewer:alice",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.State != ThreadOpen || len(thread.Comments) != 1 {
		t.Fatalf("created thread = %+v", thread)
	}

	replied, err := threads.AddComment(ctx, AddThreadCommentInput{
		ThreadID: thread.ID,
		Body:     "I will fix it.",
		Actor:    "author",
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(replied.Comments) != 2 {
		t.Fatalf("comments = %+v", replied.Comments)
	}

	claimed, err := threads.ClaimThread(ctx, ClaimThreadInput{
		ThreadID:       thread.ID,
		Kind:           ClaimFixed,
		Actor:          "author",
		ClaimCommitSHA: "def456",
	})
	if err != nil {
		t.Fatalf("claim fixed: %v", err)
	}
	if claimed.State != ThreadClaimed || claimed.ClaimKind == nil || *claimed.ClaimKind != ClaimFixed || claimed.ClaimCommitSHA == nil || *claimed.ClaimCommitSHA != "def456" {
		t.Fatalf("claimed thread = %+v", claimed)
	}

	certified, err := threads.CertifyThread(ctx, VerifyThreadInput{
		ThreadID: thread.ID,
		Actor:    "verifier",
		Body:     "Confirmed.",
	})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if certified.State != ThreadCertified || certified.CertifiedBy == nil || *certified.CertifiedBy != "verifier" {
		t.Fatalf("certified thread = %+v", certified)
	}

	reopened, err := threads.ReopenThread(ctx, VerifyThreadInput{
		ThreadID: thread.ID,
		Actor:    "verifier",
		Body:     "The guard still misses nil.",
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.State != ThreadReopened || reopened.ReopenedBy == nil || *reopened.ReopenedBy != "verifier" || len(reopened.Comments) != 4 {
		t.Fatalf("reopened thread = %+v", reopened)
	}
}

func TestNotWarrantedClaimRequiresRationale(t *testing.T) {
	_, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	thread, err := threads.CreateThread(ctx, CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: "abc123",
		FilePath:        "app.go",
		Line:            1,
		Body:            "Concern.",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if _, err := threads.ClaimThread(ctx, ClaimThreadInput{
		ThreadID: thread.ID,
		Kind:     ClaimNotWarranted,
		Actor:    "author",
	}); err == nil {
		t.Fatal("not_warranted claim without rationale succeeded")
	}

	claimed, err := threads.ClaimThread(ctx, ClaimThreadInput{
		ThreadID: thread.ID,
		Kind:     ClaimNotWarranted,
		Actor:    "author",
		Body:     "The behavior is intentional.",
	})
	if err != nil {
		t.Fatalf("claim not_warranted: %v", err)
	}
	if claimed.State != ThreadClaimed || len(claimed.Comments) != 2 {
		t.Fatalf("claimed thread = %+v", claimed)
	}
}

func TestReviewContextIncludesAllThreadStates(t *testing.T) {
	_, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	open, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "a", FilePath: "a.go", Line: 1, Body: "open"})
	if err != nil {
		t.Fatalf("create open: %v", err)
	}
	claimed, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "b", FilePath: "b.go", Line: 2, Body: "claimed"})
	if err != nil {
		t.Fatalf("create claimed: %v", err)
	}
	if _, err := threads.ClaimThread(ctx, ClaimThreadInput{ThreadID: claimed.ID, Kind: ClaimFixed, Actor: "author"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	certified, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "c", FilePath: "c.go", Line: 3, Body: "certified"})
	if err != nil {
		t.Fatalf("create certified: %v", err)
	}
	if _, err := threads.ClaimThread(ctx, ClaimThreadInput{ThreadID: certified.ID, Kind: ClaimFixed, Actor: "author"}); err != nil {
		t.Fatalf("claim certified: %v", err)
	}
	if _, err := threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: certified.ID, Actor: "verifier"}); err != nil {
		t.Fatalf("certify: %v", err)
	}
	reopened, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "d", FilePath: "d.go", Line: 4, Body: "reopened"})
	if err != nil {
		t.Fatalf("create reopened: %v", err)
	}
	if _, err := threads.ClaimThread(ctx, ClaimThreadInput{ThreadID: reopened.ID, Kind: ClaimFixed, Actor: "author"}); err != nil {
		t.Fatalf("claim reopened: %v", err)
	}
	if _, err := threads.ReopenThread(ctx, VerifyThreadInput{ThreadID: reopened.ID, Actor: "verifier", Body: "still bad"}); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	context, err := threads.ReviewContextForIssue(ctx, change.IssueID)
	if err != nil {
		t.Fatalf("review context: %v", err)
	}
	if len(context.Threads) != 4 {
		t.Fatalf("context threads = %+v, want 4 including %s", context.Threads, open.ID)
	}
}

func TestCreateThreadIsIdempotent(t *testing.T) {
	store, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	input := CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            42,
		Body:            "needs a guard",
		Actor:           "reviewer:r-local",
	}

	first, err := threads.CreateThread(ctx, input)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := threads.CreateThread(ctx, input)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent create returned different ids: %s vs %s", first.ID, second.ID)
	}

	var threadCount, commentCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM review_threads WHERE change_id = ?`, change.ID).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 1 {
		t.Fatalf("thread count = %d, want 1 (no double-file)", threadCount)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM review_comments WHERE thread_id = ?`, first.ID).Scan(&commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("comment count = %d, want 1 (no duplicate comment)", commentCount)
	}

	// A different body at the same anchor is a distinct concern, not a retry.
	other := input
	other.Body = "also handle the empty slice"
	third, err := threads.CreateThread(ctx, other)
	if err != nil {
		t.Fatalf("third create: %v", err)
	}
	if third.ID == first.ID {
		t.Fatalf("distinct body collapsed onto existing thread %s", first.ID)
	}
}

func TestCertifyTwiceIsBenignNoOp(t *testing.T) {
	// The worker applies verifier decisions from the verdict file and tolerates a
	// thread_not_found on re-apply. The coordinator surfaces that as sql.ErrNoRows
	// once a thread is already in the target state, so a retried certify is a no-op.
	_, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	thread, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "a", FilePath: "a.go", Line: 1, Body: "open"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := threads.ClaimThread(ctx, ClaimThreadInput{ThreadID: thread.ID, Kind: ClaimFixed, Actor: "author"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: thread.ID, Actor: "verifier"}); err != nil {
		t.Fatalf("first certify: %v", err)
	}
	if _, err := threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: thread.ID, Actor: "verifier"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second certify err = %v, want sql.ErrNoRows (benign no-op)", err)
	}
}

func TestCertifyRequiresClaimedThread(t *testing.T) {
	_, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	thread, err := threads.CreateThread(ctx, CreateThreadInput{ChangeID: change.ID, AnchorCommitSHA: "a", FilePath: "a.go", Line: 1, Body: "open"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := threads.CertifyThread(ctx, VerifyThreadInput{ThreadID: thread.ID, Actor: "verifier"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("certify open err = %v, want sql.ErrNoRows", err)
	}
}

func newThreadServiceFixture(t *testing.T) (*flowdb.Store, *ThreadService, Change) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Review target"})
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

	return store, NewThreadService(store.DB()), ensured.Change
}
