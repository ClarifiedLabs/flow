package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

func TestWorkerRegisterHeartbeatAndClaimLifecycle(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	registered, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"gpu": "true"},
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       1,
		HeartbeatTTL:            time.Minute,
	})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if registered.LastHeartbeatAt == nil || registered.ExpiresAt == nil {
		t.Fatalf("registered heartbeat fields = %+v", registered)
	}

	heartbeat, err := directory.HeartbeatWorker(ctx, "w-local", 2*time.Minute)
	if err != nil {
		t.Fatalf("heartbeat worker: %v", err)
	}
	if heartbeat.ExpiresAt == nil {
		t.Fatal("heartbeat ExpiresAt is nil")
	}

	job, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
		Priority:       10,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if job.State != JobQueued {
		t.Fatalf("job.State = %q, want queued", job.State)
	}

	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim next job: %v", err)
	}
	if !ok {
		t.Fatal("claim returned ok=false")
	}
	if claimed.Job.ID != job.ID || claimed.Job.State != JobClaimed {
		t.Fatalf("claimed job = %+v, want %s claimed", claimed.Job, job.ID)
	}
	if claimed.Lease.JobID != job.ID || claimed.Lease.WorkerID != "w-local" {
		t.Fatalf("lease mismatch: %+v", claimed.Lease)
	}
	leases, err := service.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(leases) != 1 || leases[0].ID != claimed.Lease.ID {
		t.Fatalf("leases = %+v, want %s", leases, claimed.Lease.ID)
	}

	running, err := service.MarkJobRunning(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if running.State != JobRunning {
		t.Fatalf("running.State = %q, want running", running.State)
	}

	renewed, err := service.RenewLease(ctx, claimed.Lease.ID, 3*time.Minute)
	if err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	if renewed.RenewalCount != 1 {
		t.Fatalf("RenewalCount = %d, want 1", renewed.RenewalCount)
	}

	finished, err := service.ReleaseLease(ctx, claimed.Lease.ID, JobFinished)
	if err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if finished.State != JobFinished {
		t.Fatalf("finished.State = %q, want finished", finished.State)
	}

	released, err := service.GetLease(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("get released lease: %v", err)
	}
	if released.ReleasedAt == nil {
		t.Fatal("ReleasedAt is nil")
	}
}

func TestRegisterWorkerPersistsHarnessModels(t *testing.T) {
	ctx := context.Background()
	_, directory, _ := newWorkerService(t)
	minBudget := 1024

	registered, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:     "w-harness",
		Labels: map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		HarnessModels: []flowharness.Model{{
			ProviderID:  "anthropic",
			ModelID:     "claude-opus-4-8",
			QualifiedID: "anthropic:claude-opus-4-8",
			Harness:     flowharness.Harness,
			Reasoning: flowharness.ReasoningInfo{
				Supported: true,
				Options: []flowharness.ReasoningOption{{
					Type: "budget_tokens",
					Min:  &minBudget,
				}},
			},
		}},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if len(registered.HarnessModels) != 1 || registered.HarnessModels[0].QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("registered harness models = %#v", registered.HarnessModels)
	}
	if registered.HarnessModels[0].Harness != flowharness.Harness {
		t.Fatalf("registered harness model harness = %q, want %q", registered.HarnessModels[0].Harness, flowharness.Harness)
	}
	if registered.HarnessModels[0].Reasoning.Options[0].Min == nil || *registered.HarnessModels[0].Reasoning.Options[0].Min != minBudget {
		t.Fatalf("registered reasoning option = %#v", registered.HarnessModels[0].Reasoning.Options[0])
	}

	updated, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-harness",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	})
	if err != nil {
		t.Fatalf("update worker without models: %v", err)
	}
	if len(updated.HarnessModels) != 0 {
		t.Fatalf("updated harness models = %#v, want cleared", updated.HarnessModels)
	}
}

func TestConcurrentClaimIsAtomic(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	const attempts = 20
	var wg sync.WaitGroup
	results := make(chan ClaimedJob, attempts)
	errs := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
				WorkerID:      "w-local",
				Buckets:       []CapacityBucket{BucketPersistentAgent},
				LeaseDuration: time.Minute,
			})
			if err != nil {
				errs <- err
				return
			}
			if ok {
				results <- claimed
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("claim error: %v", err)
	}

	var claims []ClaimedJob
	for claim := range results {
		claims = append(claims, claim)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want exactly one claim", claims)
	}
	if claims[0].Job.ID != job.ID {
		t.Fatalf("claimed job = %s, want %s", claims[0].Job.ID, job.ID)
	}
}

func TestLiveAuthorJobUniqueness(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	first, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue first author job: %v", err)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err == nil {
		t.Fatal("duplicate live author job was accepted")
	}

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim first author job: %v", err)
	}
	if !ok || claimed.Job.ID != first.ID {
		t.Fatalf("claim = %+v ok=%v, want %s", claimed, ok, first.ID)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err == nil {
		t.Fatal("duplicate claimed author job was accepted")
	}
	if _, err := service.ReleaseLease(ctx, claimed.Lease.ID, JobFailed); err != nil {
		t.Fatalf("release first author job: %v", err)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue author after terminal state: %v", err)
	}
}

func TestCapacityBucketsAreIndependent(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	persistent, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue persistent job: %v", err)
	}
	ephemeral, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleCI,
		CapacityBucket: BucketEphemeral,
		Priority:       100,
	})
	if err != nil {
		t.Fatalf("enqueue ephemeral job: %v", err)
	}

	claimedPersistent, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim persistent: %v", err)
	}
	if !ok || claimedPersistent.Job.ID != persistent.ID {
		t.Fatalf("persistent claim = %+v ok=%v, want %s", claimedPersistent, ok, persistent.ID)
	}

	claimedEphemeral, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim ephemeral: %v", err)
	}
	if !ok || claimedEphemeral.Job.ID != ephemeral.ID {
		t.Fatalf("ephemeral claim = %+v ok=%v, want %s", claimedEphemeral, ok, ephemeral.ID)
	}
}

func TestWorkerCapacityPreventsOverclaimingSameBucket(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	firstIssue := createIssue(t, store)
	secondIssue := createIssue(t, store)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	firstJob, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &firstIssue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
		Priority:       10,
	})
	if err != nil {
		t.Fatalf("enqueue first job: %v", err)
	}
	secondJob, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &secondIssue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
		Priority:       1,
	})
	if err != nil {
		t.Fatalf("enqueue second job: %v", err)
	}

	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim first job: %v", err)
	}
	if !ok || claimed.Job.ID != firstJob.ID {
		t.Fatalf("claim = %+v ok=%v, want %s", claimed, ok, firstJob.ID)
	}

	_, ok, err = claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim while full: %v", err)
	}
	if ok {
		t.Fatal("worker claimed second persistent job while capacity was full")
	}

	if _, err := service.ReleaseLease(ctx, claimed.Lease.ID, JobFinished); err != nil {
		t.Fatalf("release first job: %v", err)
	}
	claimed, ok, err = claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim second job after release: %v", err)
	}
	if !ok || claimed.Job.ID != secondJob.ID {
		t.Fatalf("second claim = %+v ok=%v, want %s", claimed, ok, secondJob.ID)
	}
}

func TestClaimSkipsJobsWithUnmatchedSelectors(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"gpu": "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	blocked, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleReviewer,
		CapacityBucket: BucketPersistentAgent,
		Priority:       100,
		Requires:       []string{"gpu", "docker"},
	})
	if err != nil {
		t.Fatalf("enqueue unmatched selector job: %v", err)
	}
	matched, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleReviewer,
		CapacityBucket: BucketPersistentAgent,
		Priority:       1,
		Requires:       []string{"gpu"},
	})
	if err != nil {
		t.Fatalf("enqueue matched selector job: %v", err)
	}

	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim selector-matched job: %v", err)
	}
	if !ok || claimed.Job.ID != matched.ID {
		t.Fatalf("claim = %+v ok=%v, want %s", claimed, ok, matched.ID)
	}

	stillQueued, err := service.GetJob(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("get unmatched selector job: %v", err)
	}
	if stillQueued.State != JobQueued {
		t.Fatalf("unmatched selector job state = %q, want queued", stillQueued.State)
	}
	if stillQueued.Selector["docker"] != "true" {
		t.Fatalf("unmatched selector requirements = %+v, want docker=true", stillQueued.Selector)
	}
}

func TestClaimSkipsTaintedWorkerWithoutExactToleration(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"gpu": "true"},
		Taints:                  []scheduler.Taint{{Key: "lifetime", Value: "persistent", Effect: scheduler.EffectNoSchedule}},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register tainted worker: %v", err)
	}
	blocked, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleReviewer,
		CapacityBucket: BucketPersistentAgent,
		Priority:       100,
		Requires:       []string{"gpu"},
	})
	if err != nil {
		t.Fatalf("enqueue untolerated job: %v", err)
	}
	matched, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleReviewer,
		CapacityBucket: BucketPersistentAgent,
		Priority:       1,
		Requires:       []string{"gpu"},
		Tolerations:    []scheduler.Toleration{{Key: "lifetime", Value: "persistent", Effect: scheduler.EffectNoSchedule}},
	})
	if err != nil {
		t.Fatalf("enqueue tolerated job: %v", err)
	}

	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim tolerated job: %v", err)
	}
	if !ok || claimed.Job.ID != matched.ID {
		t.Fatalf("claim = %+v ok=%v, want %s", claimed, ok, matched.ID)
	}

	stillQueued, err := service.GetJob(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("get untolerated job: %v", err)
	}
	if stillQueued.State != JobQueued {
		t.Fatalf("untolerated job state = %q, want queued", stillQueued.State)
	}
}

func TestExpiredLeaseIsSwept(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}
	if _, err := service.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	swept, err := service.SweepExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("sweep expired leases: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	sweptJob, err := service.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if sweptJob.State != JobCrashed {
		t.Fatalf("swept job state = %q, want crashed", sweptJob.State)
	}
	sweptLease, err := service.GetLease(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("get swept lease: %v", err)
	}
	if sweptLease.ReleasedAt == nil {
		t.Fatal("swept lease ReleasedAt is nil")
	}
}

func TestExpiredLeaseCannotBeRenewed(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	now := time.Date(2026, 6, 7, 13, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := service.RenewLease(ctx, claimed.Lease.ID, time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("renew expired lease err = %v, want sql.ErrNoRows", err)
	}
}

func TestExpiredLeaseCannotBeMarkedRunning(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := service.MarkJobRunning(ctx, claimed.Lease.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("mark expired lease running err = %v, want sql.ErrNoRows", err)
	}
}

func TestLiveLeaseUniquenessIsEnforcedAtDatabaseLayer(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	issue := createIssue(t, store)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 2,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}

	now := time.Now().UTC()
	_, err = store.DB().ExecContext(ctx, `
INSERT INTO leases (
	id,
	job_id,
	worker_id,
	capacity_bucket,
	leased_at,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		"l-duplicate",
		claimed.Job.ID,
		"w-local",
		string(BucketPersistentAgent),
		formatTime(now),
		formatTime(now.Add(time.Minute)),
	)
	if err == nil {
		t.Fatal("database accepted a second live lease for one job")
	}
}

func TestInvalidWorkerInputsAreRejected(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{}); err == nil {
		t.Fatal("empty worker id was accepted")
	}
	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err == nil {
		t.Fatal("author job without issue was accepted")
	}
	if _, _, err := claimNext(ctx, directory, service, ClaimInput{
		WorkerID:      "missing-worker",
		LeaseDuration: time.Minute,
	}); err == nil || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing worker claim err = %v, want sql.ErrNoRows", err)
	}
}

func TestEnqueueJobRejectsChangeIssueMismatch(t *testing.T) {
	ctx := context.Background()
	store, _, service := newWorkerService(t)
	firstIssue := createIssue(t, store)
	secondIssue := createIssue(t, store)
	changeID := insertChange(t, store, firstIssue.ID)

	if _, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &secondIssue.ID,
		ChangeID:       &changeID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	}); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("enqueue mismatched change err = %v, want mismatch error", err)
	}
	matched, err := service.EnqueueJob(ctx, EnqueueJobInput{
		IssueID:        &firstIssue.ID,
		ChangeID:       &changeID,
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue matched change: %v", err)
	}
	if matched.ChangeID == nil || *matched.ChangeID != changeID {
		t.Fatalf("matched ChangeID = %v, want %s", matched.ChangeID, changeID)
	}
}

func newWorkerService(t *testing.T) (*flowdb.Store, *Directory, *Service) {
	t.Helper()

	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	global, err := flowdb.OpenGlobal(context.Background(), filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() {
		_ = global.Close()
	})

	return store, NewDirectory(global.DB()), NewService(store.DB())
}

// claimNext adapts the single-project tests to the cross-project claim entry
// point with one queue.
func claimNext(ctx context.Context, directory *Directory, service *Service, input ClaimInput) (ClaimedJob, bool, error) {
	claim, ok, err := ClaimAcrossProjects(ctx, directory, []ProjectQueue{{ProjectID: "p-test", Queue: service}}, input)
	return ClaimedJob{Job: claim.Job, Lease: claim.Lease}, ok, err
}

type testIssue struct {
	ID string
}

func createIssue(t *testing.T, store *flowdb.Store) testIssue {
	t.Helper()

	ctx := context.Background()
	var nextNumber int64
	if err := store.DB().QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'issue'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		t.Fatalf("allocate issue id: %v", err)
	}
	id := fmt.Sprintf("i-%04d", nextNumber)
	now := formatTime(time.Now().UTC())
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO issues (
	id,
	title,
	body,
	acceptance_criteria,
	priority,
	schedule_state,
	triage_state,
	requires_human_review,
	auto_merge,
	created_by,
	created_at,
	updated_at
) VALUES (?, 'Worker issue', '', '', 0, 'backlog', 'accepted', 0, 0, 'human', ?, ?)`,
		id,
		now,
		now,
	); err != nil {
		t.Fatalf("insert issue: %v", err)
	}

	return testIssue{ID: id}
}

func insertChange(t *testing.T, store *flowdb.Store, issueID string) string {
	t.Helper()

	id := "ch-" + strings.TrimPrefix(issueID, "i-")
	now := formatTime(time.Now().UTC())
	if _, err := store.DB().ExecContext(context.Background(), `
INSERT INTO changes (
	id,
	issue_id,
	branch,
	base,
	head_sha,
	created_at,
	updated_at
) VALUES (?, ?, ?, 'main', '', ?, ?)`,
		id,
		issueID,
		"issue/"+issueID,
		now,
		now,
	); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	return id
}
