package coordinator

import (
	"context"
	"testing"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

const modeBHeadSHA = "deadbeefcafefeed0000000000000000deadbeef"

// registerModeBWorker registers a single persistent-agent worker the crash
// recovery tests reuse to claim author jobs.
func registerModeBWorker(t *testing.T, ctx context.Context, fixture sessionFixture) {
	t.Helper()
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
}

func expireLease(t *testing.T, ctx context.Context, fixture sessionFixture, leaseID string) {
	t.Helper()
	if _, err := fixture.sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), leaseID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

// seedBranchAheadWithHandoff makes the crashed change look finished-but-
// unfinalized: it advances the head past base and records a handoff snapshot,
// the two signals that route a crash to a completion-assessment review.
func seedBranchAheadWithHandoff(t *testing.T, ctx context.Context, fixture sessionFixture, changeID string) {
	t.Helper()
	if _, err := fixture.sessions.UpdateChangeHead(ctx, changeID, modeBHeadSHA); err != nil {
		t.Fatalf("advance change head: %v", err)
	}
	if err := fixture.reconciler.UpsertHandoffSnapshot(ctx, changeID, modeBHeadSHA, "## Handoff\nwork in progress\n"); err != nil {
		t.Fatalf("write handoff snapshot: %v", err)
	}
}

func liveReviewerJobCount(t *testing.T, ctx context.Context, fixture sessionFixture, issueID string) int {
	t.Helper()
	jobs, err := fixture.workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	count := 0
	for _, job := range jobs {
		if job.Role != flowworker.RoleReviewer || job.IssueID == nil || *job.IssueID != issueID {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			count++
		}
	}
	return count
}

// TestReconcileCrashedAuthorBranchAheadWithHandoffDispatchesCompletionReview is
// the Mode-B happy path: a crashed author that is ahead of base and left a
// handoff is routed to a completion-assessment review instead of a blind full
// author relaunch.
func TestReconcileCrashedAuthorBranchAheadWithHandoffDispatchesCompletionReview(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	registerModeBWorker(t, ctx, fixture)

	crashed := startWaitingAuthorSession(t, ctx, fixture, "Mode-B finished work")
	seedBranchAheadWithHandoff(t, ctx, fixture, crashed.session.ChangeID)
	expireLease(t, ctx, fixture, crashed.leaseID)

	recovered, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("reconcile crashed author: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	// No blind author relaunch.
	if live, ok, err := fixture.workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID); err != nil {
		t.Fatalf("live author job: %v", err)
	} else if ok {
		t.Fatalf("targeted-review recovery still enqueued an author job: %+v", live)
	}

	// A completion-assessment reviewer job is in flight.
	if got := liveReviewerJobCount(t, ctx, fixture, crashed.session.IssueID); got != 1 {
		t.Fatalf("live reviewer jobs = %d, want 1", got)
	}

	// The reviewer check carries the completion-assessment marker so the reviewer
	// renders recovery guidance.
	reviewer, err := fixture.checks.GetCheck(ctx, crashed.session.IssueID, defaultReviewerCheckName)
	if err != nil {
		t.Fatalf("get reviewer check: %v", err)
	}
	if reviewer.Verdict != CheckPending || reviewer.Details != CompletionAssessmentCheckMarker {
		t.Fatalf("reviewer check = %+v, want pending completion-assessment marker", reviewer)
	}

	// The change is published so the reviewer verdict routes through the normal
	// review machinery.
	change, err := fixture.sessions.GetChange(ctx, crashed.session.ChangeID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.ReadyAt == nil {
		t.Fatalf("change was not marked ready: %+v", change)
	}

	// The crashed job is stamped so re-selection is a no-op.
	crashedJob, err := fixture.workers.GetJob(ctx, crashed.jobID)
	if err != nil {
		t.Fatalf("get crashed job: %v", err)
	}
	if !payloadBool(crashedJob.Payload, completionReviewDispatchedKey) {
		t.Fatalf("crashed job missing dispatch flag: %+v", crashedJob.Payload)
	}
}

// TestCompletionReviewSurfacesPendingCheckTimeout proves the coordinator emits
// the dispatched completion review's pending reviewer check from
// RecoverPendingCheckJobs, so the lifecycle engine's recovery can arm a check
// timeout for it. The completion review is scheduled outside the engine and is
// otherwise not timeout-armed; the engine drives RecoverPendingCheckJobs every
// recovery tick, so surfacing the pending check there closes the gap
// independently of which call dispatched the review.
func TestCompletionReviewSurfacesPendingCheckTimeout(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	registerModeBWorker(t, ctx, fixture)

	crashed := startWaitingAuthorSession(t, ctx, fixture, "Completion review timeout")
	seedBranchAheadWithHandoff(t, ctx, fixture, crashed.session.ChangeID)
	expireLease(t, ctx, fixture, crashed.leaseID)

	if _, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The reviewer job is live (just dispatched) but no timeout is armed for it;
	// the recovery scan must still surface the pending reviewer so the engine can
	// arm one — a live job that never reports needs the timeout just as much.
	_, pending, err := fixture.checkConfigs.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover pending check jobs: %v", err)
	}
	var found bool
	for _, p := range pending {
		if p.IssueID != crashed.session.IssueID || p.HeadSHA != modeBHeadSHA {
			continue
		}
		for _, name := range p.CheckNames {
			if name == defaultReviewerCheckName {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("RecoverPendingCheckJobs did not surface the pending reviewer at %s: %+v", modeBHeadSHA, pending)
	}
}

// TestReconcileCrashedAuthorRequiresBothBranchAheadAndHandoff proves the gate is
// conjunctive: a crash missing either signal keeps today's bounded relaunch.
func TestReconcileCrashedAuthorRequiresBothBranchAheadAndHandoff(t *testing.T) {
	t.Run("branch ahead but no handoff", func(t *testing.T) {
		ctx := context.Background()
		fixture := newSessionServiceFixture(t)
		registerModeBWorker(t, ctx, fixture)

		crashed := startWaitingAuthorSession(t, ctx, fixture, "Ahead without handoff")
		if _, err := fixture.sessions.UpdateChangeHead(ctx, crashed.session.ChangeID, modeBHeadSHA); err != nil {
			t.Fatalf("advance head: %v", err)
		}
		expireLease(t, ctx, fixture, crashed.leaseID)

		if _, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertRelaunchedNotReviewed(t, ctx, fixture, crashed)
	})

	t.Run("handoff but branch not ahead", func(t *testing.T) {
		ctx := context.Background()
		fixture := newSessionServiceFixture(t)
		registerModeBWorker(t, ctx, fixture)

		crashed := startWaitingAuthorSession(t, ctx, fixture, "Handoff without head")
		if err := fixture.reconciler.UpsertHandoffSnapshot(ctx, crashed.session.ChangeID, "", "## Handoff\nnotes\n"); err != nil {
			t.Fatalf("write handoff: %v", err)
		}
		expireLease(t, ctx, fixture, crashed.leaseID)

		if _, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertRelaunchedNotReviewed(t, ctx, fixture, crashed)
	})
}

func assertRelaunchedNotReviewed(t *testing.T, ctx context.Context, fixture sessionFixture, crashed crashableSession) {
	t.Helper()
	if _, ok, err := fixture.workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID); err != nil {
		t.Fatalf("live author job: %v", err)
	} else if !ok {
		t.Fatal("crash without both Mode-B signals was not relaunched as an author")
	}
	if got := liveReviewerJobCount(t, ctx, fixture, crashed.session.IssueID); got != 0 {
		t.Fatalf("unexpected reviewer jobs = %d, want 0", got)
	}
	change, err := fixture.sessions.GetChange(ctx, crashed.session.ChangeID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.ReadyAt != nil {
		t.Fatalf("change unexpectedly marked ready: %+v", change)
	}
}

// TestReconcileCompletionReviewIsIdempotent proves the anti-loop property: once a
// completion review is dispatched, the still-crashed session keeps matching the
// reconcile query (a reviewer, not an author, job is live), but re-running
// reconcile is a no-op rather than a second review or a blind relaunch.
func TestReconcileCompletionReviewIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	registerModeBWorker(t, ctx, fixture)

	crashed := startWaitingAuthorSession(t, ctx, fixture, "Idempotent recovery")
	seedBranchAheadWithHandoff(t, ctx, fixture, crashed.session.ChangeID)
	expireLease(t, ctx, fixture, crashed.leaseID)

	if recovered, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	} else if recovered != 1 {
		t.Fatalf("first recovered = %d, want 1", recovered)
	}

	for tick := 0; tick < 3; tick++ {
		recovered, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx)
		if err != nil {
			t.Fatalf("reconcile tick %d: %v", tick, err)
		}
		if recovered != 0 {
			t.Fatalf("reconcile tick %d recovered = %d, want 0 (no-op)", tick, recovered)
		}
		if _, ok, err := fixture.workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID); err != nil {
			t.Fatalf("live author job tick %d: %v", tick, err)
		} else if ok {
			t.Fatalf("re-selection tick %d wrongly relaunched an author", tick)
		}
		if got := liveReviewerJobCount(t, ctx, fixture, crashed.session.IssueID); got != 1 {
			t.Fatalf("reviewer jobs tick %d = %d, want exactly 1 (no duplicate)", tick, got)
		}
	}
}

// TestReconcileCompletionReviewFiresOnlyForUnfinalizedChange proves the targeted
// review fires at most once: a crash whose change is already ready (e.g. a later
// fix-round author) falls through to the normal bounded relaunch instead of
// dispatching another review.
func TestReconcileCompletionReviewFiresOnlyForUnfinalizedChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	registerModeBWorker(t, ctx, fixture)

	crashed := startWaitingAuthorSession(t, ctx, fixture, "Already-published change")
	seedBranchAheadWithHandoff(t, ctx, fixture, crashed.session.ChangeID)
	// Publish the change up front: the author already finalized a prior revision,
	// so this crash is a fix-round crash, not a never-finalized one.
	if _, err := fixture.sessions.db.ExecContext(ctx, `
UPDATE changes SET ready_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), crashed.session.ChangeID); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}
	expireLease(t, ctx, fixture, crashed.leaseID)

	if recovered, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	} else if recovered != 1 {
		t.Fatalf("recovered = %d, want 1 (bounded relaunch)", recovered)
	}

	if _, ok, err := fixture.workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID); err != nil {
		t.Fatalf("live author job: %v", err)
	} else if !ok {
		t.Fatal("already-ready change did not relaunch an author")
	}
	if got := liveReviewerJobCount(t, ctx, fixture, crashed.session.IssueID); got != 0 {
		t.Fatalf("already-ready change wrongly dispatched a review: reviewer jobs = %d", got)
	}
	crashedJob, err := fixture.workers.GetJob(ctx, crashed.jobID)
	if err != nil {
		t.Fatalf("get crashed job: %v", err)
	}
	if payloadBool(crashedJob.Payload, completionReviewDispatchedKey) {
		t.Fatal("already-ready change wrongly stamped the dispatch flag")
	}
}

// TestCompletionReviewCrashExcludedFromRelaunchBudget proves a crash routed to a
// completion review does not consume the automatic relaunch budget, so a real
// fix-round author still gets its bounded relaunch (maxAutomaticCrashAttempts).
func TestCompletionReviewCrashExcludedFromRelaunchBudget(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)

	issue, err := fixture.issues.CreateIssue(ctx, CreateIssueInput{Title: "Crash budget accounting"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	change, err := fixture.sessions.ensureChange(ctx, issue.ID, issueBranch(issue.ID), "main")
	if err != nil {
		t.Fatalf("ensure change: %v", err)
	}

	enqueueCrashedAuthorJob := func(dispatched bool) {
		payload := map[string]any{
			"change_id":       change.ID,
			"branch":          change.Branch,
			"base":            change.Base,
			"session_purpose": string(AuthorSessionPurposeAuthoring),
		}
		if dispatched {
			payload[completionReviewDispatchedKey] = true
		}
		job, err := fixture.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			IssueID:        &issue.ID,
			ChangeID:       &change.ID,
			Role:           flowworker.RoleAuthor,
			CapacityBucket: flowworker.BucketPersistentAgent,
			Payload:        payload,
		})
		if err != nil {
			t.Fatalf("enqueue crashed author job: %v", err)
		}
		if _, err := fixture.sessions.db.ExecContext(ctx, `
UPDATE jobs SET state = ? WHERE id = ?`, string(flowworker.JobCrashed), job.ID); err != nil {
			t.Fatalf("mark job crashed: %v", err)
		}
	}

	// One completion-review crash plus one real author crash: the flagged crash
	// is excluded, so only the single real attempt counts — not yet exhausted.
	enqueueCrashedAuthorJob(true)
	enqueueCrashedAuthorJob(false)
	exhausted, attempts, err := fixture.sessions.authorCrashRestartLimitReached(ctx, issue.ID, change.ID, AuthorSessionPurposeAuthoring)
	if err != nil {
		t.Fatalf("crash limit: %v", err)
	}
	if exhausted || attempts != 1 {
		t.Fatalf("exhausted=%t attempts=%d, want false/1 (flagged crash excluded)", exhausted, attempts)
	}

	// A second real author crash reaches the limit; the flagged crash still does
	// not count, so the bound is exactly maxAutomaticCrashAttempts real attempts.
	enqueueCrashedAuthorJob(false)
	exhausted, attempts, err = fixture.sessions.authorCrashRestartLimitReached(ctx, issue.ID, change.ID, AuthorSessionPurposeAuthoring)
	if err != nil {
		t.Fatalf("crash limit second: %v", err)
	}
	if !exhausted || attempts != maxAutomaticCrashAttempts {
		t.Fatalf("exhausted=%t attempts=%d, want true/%d", exhausted, attempts, maxAutomaticCrashAttempts)
	}
}

// TestReconcileCompletionReviewSkippedWithoutRecoveryDeps proves graceful
// degradation: a session service constructed without the recovery dependencies
// keeps the bounded author relaunch even when the Mode-B signals are present.
func TestReconcileCompletionReviewSkippedWithoutRecoveryDeps(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	registerModeBWorker(t, ctx, fixture)

	// Rebuild the session service without the recovery dependencies, sharing the
	// same database so the fixture's helpers still observe its writes.
	fixture.sessions = NewSessionServiceWithOptions(fixture.store.DB(), fixture.issues, fixture.workers, SessionServiceOptions{
		Credentials: fixture.credentials,
		Project:     fixture.project,
	})

	crashed := startWaitingAuthorSession(t, ctx, fixture, "No recovery deps")
	seedBranchAheadWithHandoff(t, ctx, fixture, crashed.session.ChangeID)
	expireLease(t, ctx, fixture, crashed.leaseID)

	if _, err := fixture.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertRelaunchedNotReviewed(t, ctx, fixture, crashed)
}
