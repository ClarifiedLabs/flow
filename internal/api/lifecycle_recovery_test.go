package api

import (
	"context"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// TestLifecycleRecoveryWithoutInboundTraffic is the regression for the durability
// gap the engine closes: a crashed author session (expired lease) must be
// reconciled and its work re-enqueued by the background ticker's recovery pass
// WITHOUT any inbound API request driving it. Previously recovery only fired
// lazily when traffic happened to pass through a handler.
func TestLifecycleRecoveryWithoutInboundTraffic(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	started := startAuthorSessionForStatusTestWithWorker(t, fixture, "Recovery issue", "w-recovery")

	// Expire the session's lease so it qualifies as crashed.
	if _, err := fixture.DB.Exec(`UPDATE leases SET expires_at = ? WHERE id = ?`,
		"2020-01-01T00:00:00Z", started.Session.LeaseID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	engine := fixture.Bundle.Engine
	if engine == nil {
		t.Fatal("bundle has no lifecycle engine")
	}

	// Recovery runs directly (as the ticker does) — no HTTP request is made.
	recovered, err := engine.RunRecovery(ctx)
	if err != nil {
		t.Fatalf("run recovery: %v", err)
	}
	if recovered == 0 {
		t.Fatalf("expected at least one crashed session to be recovered")
	}

	session, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.RuntimeState != coordinator.SessionCrashed {
		t.Fatalf("session runtime_state = %q, want crashed", session.RuntimeState)
	}

	live := liveAuthorJobsForIssue(t, fixture, started.Session.IssueID)
	if len(live) == 0 {
		t.Fatalf("expected a re-enqueued author job after recovery, got none")
	}
}

func TestLifecycleRecoveryRetriesCrashedVerifierCheckJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	started := startAuthorSessionForStatusTestWithWorker(t, fixture, "Verifier recovery issue", "w-verifier-author")
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{RequiresHumanReview: &requiresHuman}); err != nil {
		t.Fatalf("disable human review: %v", err)
	}
	exchangePath, headSHA := createMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	doJSONRequestAs(t, fixture.Server, started.Token, "POST", "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, 200, nil)
	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)

	verifierJobs := liveAPICheckJobsForIssue(t, fixture, started.Session.IssueID, flowworker.RoleVerifier, "verifier")
	if len(verifierJobs) != 1 {
		t.Fatalf("live verifier jobs = %+v, want one before crash", verifierJobs)
	}
	crashedJobID := verifierJobs[0].ID
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-verifier-recovery",
		Labels:                  map[string]string{"agent.harness.codex": "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register verifier worker: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, "w-verifier-recovery", crashedJobID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark verifier running: %v", err)
	}
	if _, err := fixture.DB.Exec(`UPDATE leases SET expires_at = ? WHERE id = ?`,
		"2020-01-01T00:00:00Z", claimed.Lease.ID); err != nil {
		t.Fatalf("expire verifier lease: %v", err)
	}

	engine := fixture.Bundle.Engine
	if engine == nil {
		t.Fatal("bundle has no lifecycle engine")
	}
	recovered, err := engine.RunRecovery(ctx)
	if err != nil {
		t.Fatalf("run recovery: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want one replacement verifier job", recovered)
	}
	crashedJob, err := fixture.Workers.GetJob(ctx, crashedJobID)
	if err != nil {
		t.Fatalf("get crashed verifier job: %v", err)
	}
	if crashedJob.State != flowworker.JobCrashed {
		t.Fatalf("expired verifier job state = %q, want crashed", crashedJob.State)
	}
	assertAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer, coordinator.CheckSatisfied)
	assertAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier, coordinator.CheckPending)

	replacements := liveAPICheckJobsForIssue(t, fixture, started.Session.IssueID, flowworker.RoleVerifier, "verifier")
	if len(replacements) != 1 || replacements[0].ID == crashedJobID {
		t.Fatalf("replacement verifier jobs = %+v, old job %s", replacements, crashedJobID)
	}
	recovered, err = engine.RunRecovery(ctx)
	if err != nil {
		t.Fatalf("run recovery again: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("second recovery enqueued %d jobs, want 0", recovered)
	}
	replacements = liveAPICheckJobsForIssue(t, fixture, started.Session.IssueID, flowworker.RoleVerifier, "verifier")
	if len(replacements) != 1 {
		t.Fatalf("live verifier jobs after second recovery = %+v, want one", replacements)
	}
}

func liveAPICheckJobsForIssue(t *testing.T, fixture testFixture, issueID string, role flowworker.JobRole, checkName string) []flowworker.Job {
	t.Helper()

	jobs, err := fixture.Workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var live []flowworker.Job
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID || job.Role != role {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			if payloadString(job.Payload, "check_name") == checkName {
				live = append(live, job)
			}
		}
	}
	return live
}
