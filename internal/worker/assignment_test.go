package worker

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

func TestAssignmentReservationEligibilityAndExclusion(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	ineligible, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role: RoleCI, CapacityBucket: BucketEphemeral, Priority: 30,
		RunsOn:  map[string]string{"os": "darwin", "pool": "trusted"},
		Payload: map[string]any{"blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue ineligible: %v", err)
	}
	reservedJob, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role: RoleCI, CapacityBucket: BucketEphemeral, Priority: 20,
		RunsOn:  map[string]string{"os": "linux", "pool": "trusted"},
		Payload: map[string]any{"blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue reserved: %v", err)
	}
	genericJob, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role: RoleCI, CapacityBucket: BucketEphemeral, Priority: 10,
		RunsOn:  map[string]string{"os": "linux", "pool": "trusted"},
		Payload: map[string]any{"blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue generic: %v", err)
	}

	filter := AssignmentCandidateFilter{
		ProfileLabels: map[string]string{"os": "linux", "pool": "trusted"},
		AllowedRoles:  []JobRole{RoleCI}, AllowedBuckets: []CapacityBucket{BucketEphemeral},
		RequiredSelector: map[string]string{"pool": "trusted"},
	}
	candidate, ok, err := service.FindAssignmentCandidate(ctx, filter)
	if err != nil {
		t.Fatalf("find assignment candidate: %v", err)
	}
	if !ok || candidate.ID != reservedJob.ID {
		t.Fatalf("candidate = %s/%v, want %s/true (higher-priority %s is ineligible)", candidate.ID, ok, reservedJob.ID, ineligible.ID)
	}
	assignment, err := service.ReserveAssignment(ctx, ReserveAssignmentInput{
		JobID: reservedJob.ID, ProviderID: "kubernetes", ProfileName: "linux-ci", ProviderType: "kubernetes",
		ProviderRequestID: "request-1", ProfileLabels: filter.ProfileLabels,
		AllowedRoles: filter.AllowedRoles, AllowedBuckets: filter.AllowedBuckets,
		RequiredSelector: filter.RequiredSelector, StartupDeadline: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve assignment: %v", err)
	}
	if assignment.State != AssignmentPending || assignment.Role != RoleCI || assignment.CapacityBucket != BucketEphemeral {
		t.Fatalf("reserved assignment = %+v", assignment)
	}
	jobAfterReserve, err := service.GetJob(ctx, reservedJob.ID)
	if err != nil {
		t.Fatalf("get reserved job: %v", err)
	}
	if jobAfterReserve.State != JobQueued || !jobAfterReserve.CreatedAt.Equal(reservedJob.CreatedAt) || !jobAfterReserve.UpdatedAt.Equal(reservedJob.UpdatedAt) {
		t.Fatalf("reservation changed queued job: before=%+v after=%+v", reservedJob, jobAfterReserve)
	}

	repeated, err := service.ReserveAssignment(ctx, ReserveAssignmentInput{
		JobID: reservedJob.ID, ProviderID: "kubernetes", ProfileName: "linux-ci", ProviderType: "kubernetes",
		ProviderRequestID: "request-1", ProfileLabels: filter.ProfileLabels,
		AllowedRoles: filter.AllowedRoles, AllowedBuckets: filter.AllowedBuckets,
		RequiredSelector: filter.RequiredSelector, StartupDeadline: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("repeat reservation: %v", err)
	}
	if repeated.ID != assignment.ID || repeated.WorkerID != assignment.WorkerID {
		t.Fatalf("repeat = %+v, want original %+v", repeated, assignment)
	}

	if _, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID:             "w-next",
		Labels:         map[string]string{"os": "linux", "pool": "trusted"},
		CapacityBucket: BucketEphemeral,
	}); err != nil {
		t.Fatalf("register next worker: %v", err)
	}
	claimed, ok, err := claimNext(ctx, directory, service, ClaimInput{WorkerID: "w-next", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("assignment-backed claim: %v", err)
	}
	if !ok || claimed.Job.ID != genericJob.ID {
		t.Fatalf("assignment-backed claim = %s/%v, want unassigned %s", claimed.Job.ID, ok, genericJob.ID)
	}
	stillQueued, _ := service.GetJob(ctx, reservedJob.ID)
	if stillQueued.State != JobQueued {
		t.Fatalf("reserved job state = %s, want queued", stillQueued.State)
	}
}

func TestAssignmentExactClaimRetryAndTerminalClosure(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)
	now := time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{Role: RoleCI, CapacityBucket: BucketEphemeral, RunsOn: map[string]string{"os": "linux"}, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	assignment := reserveTestAssignment(t, service, job.ID, now, map[string]string{"os": "linux"})
	worker, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
		ID: assignment.WorkerID, Labels: map[string]string{"os": "linux"}, CapacityBucket: BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("register assigned worker: %v", err)
	}

	claimed, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{AssignmentID: assignment.ID, Worker: worker, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("claim assignment: %v", err)
	}
	if claimed.Job.ID != job.ID || claimed.Job.State != JobClaimed || claimed.Lease.WorkerID != assignment.WorkerID {
		t.Fatalf("claim = %+v", claimed)
	}
	retried, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{
		AssignmentID: assignment.ID, Worker: worker,
		Used: scheduler.Capacity{Ephemeral: 1}, LeaseDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if retried.Lease.ID != claimed.Lease.ID || !retried.Lease.ExpiresAt.Equal(claimed.Lease.ExpiresAt) {
		t.Fatalf("retry lease = %+v, want unchanged %+v", retried.Lease, claimed.Lease)
	}
	open, err := service.CountOpenAssignments(ctx, "test-provider", "test-profile")
	if err != nil || open != 1 {
		t.Fatalf("open assignments = %d, %v", open, err)
	}
	found, ok, err := service.FindAssignmentByWorker(ctx, assignment.WorkerID)
	if err != nil || !ok || found.ID != assignment.ID {
		t.Fatalf("find by worker = %+v/%v/%v", found, ok, err)
	}
	found, ok, err = service.FindAssignmentByRequest(ctx, "test-provider", "request-"+job.ID)
	if err != nil || !ok || found.ID != assignment.ID {
		t.Fatalf("find by request = %+v/%v/%v", found, ok, err)
	}

	if _, err := service.ReleaseLease(ctx, claimed.Lease.ID, JobFinished); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	closed, err := service.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatalf("get closed assignment: %v", err)
	}
	if closed.State != AssignmentClosed || closed.CloseReason == nil || *closed.CloseReason != string(JobFinished) || closed.ClaimedLeaseID == nil {
		t.Fatalf("closed assignment = %+v", closed)
	}
	if _, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{AssignmentID: assignment.ID, Worker: worker, LeaseDuration: time.Minute}); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("claim closed assignment err = %v, want conflict", err)
	}
	revoked, err := service.MarkAssignmentCredentialsRevoked(ctx, assignment.ID)
	if err != nil || revoked.CredentialsRevokedAt == nil {
		t.Fatalf("mark credentials revoked: %+v, %v", revoked, err)
	}
	cleaned, err := service.MarkAssignmentCleaned(ctx, assignment.ID)
	if err != nil {
		t.Fatalf("mark cleaned: %v", err)
	}
	cleanedAgain, err := service.MarkAssignmentCleaned(ctx, assignment.ID)
	if err != nil {
		t.Fatalf("mark cleaned again: %v", err)
	}
	if cleaned.CleanedAt == nil || cleanedAgain.CleanedAt == nil || !cleaned.CleanedAt.Equal(*cleanedAgain.CleanedAt) {
		t.Fatalf("cleaned timestamps = %v/%v", cleaned.CleanedAt, cleanedAgain.CleanedAt)
	}
}

func TestAssignmentClaimRejectsWorkerAndCapabilityMismatches(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)
	now := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	for _, tc := range []struct {
		name     string
		workerID func(Assignment) string
		labels   map[string]string
		taints   []scheduler.Taint
	}{
		{name: "wrong worker", workerID: func(Assignment) string { return "w-wrong" }, labels: map[string]string{"os": "linux"}},
		{name: "profile labels", workerID: func(a Assignment) string { return a.WorkerID }, labels: map[string]string{"os": "darwin"}},
		{name: "actual taint eligibility", workerID: func(a Assignment) string { return a.WorkerID }, labels: map[string]string{"os": "linux"}, taints: []scheduler.Taint{{Key: "dedicated", Value: "other", Effect: scheduler.EffectNoSchedule}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := service.EnqueueJob(ctx, EnqueueJobInput{Role: RoleCI, CapacityBucket: BucketEphemeral, RunsOn: map[string]string{"os": "linux"}, Payload: map[string]any{"blocking": true}})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			assignment := reserveTestAssignment(t, service, job.ID, now, map[string]string{"os": "linux"})
			worker, err := directory.RegisterWorker(ctx, RegisterWorkerInput{
				ID: tc.workerID(assignment), Labels: tc.labels, Taints: tc.taints, CapacityBucket: BucketEphemeral,
			})
			if err != nil {
				t.Fatalf("register worker: %v", err)
			}
			if _, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{AssignmentID: assignment.ID, Worker: worker, LeaseDuration: time.Minute}); !errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("claim mismatch err = %v, want conflict", err)
			}
			stored, _ := service.GetAssignment(ctx, assignment.ID)
			queued, _ := service.GetJob(ctx, job.ID)
			if stored.State != AssignmentPending || queued.State != JobQueued {
				t.Fatalf("mismatch mutated assignment/job: %s/%s", stored.State, queued.State)
			}
			if _, err := service.AbandonAssignment(ctx, AbandonAssignmentInput{AssignmentID: assignment.ID, CloseReason: "test_cleanup"}); err != nil {
				t.Fatalf("abandon test assignment: %v", err)
			}
		})
	}
}

func TestPendingAssignmentExpiryAndCancellationPreserveQueueSemantics(t *testing.T) {
	ctx := context.Background()
	store, directory, service := newWorkerService(t)
	now := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{Role: RoleCI, CapacityBucket: BucketEphemeral, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue expiry job: %v", err)
	}
	assignment := reserveTestAssignment(t, service, job.ID, now, nil)
	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	expired, err := service.ExpirePendingAssignments(ctx)
	if err != nil || expired != 1 {
		t.Fatalf("expire pending = %d, %v", expired, err)
	}
	after, _ := service.GetJob(ctx, job.ID)
	if after.State != JobQueued || !after.CreatedAt.Equal(job.CreatedAt) || !after.UpdatedAt.Equal(job.UpdatedAt) {
		t.Fatalf("expiry changed queued job: before=%+v after=%+v", job, after)
	}
	closed, _ := service.GetAssignment(ctx, assignment.ID)
	if closed.State != AssignmentClosed || closed.CloseReason == nil || *closed.CloseReason != "startup_timeout" {
		t.Fatalf("expired assignment = %+v", closed)
	}
	if _, err := service.AbandonAssignment(ctx, AbandonAssignmentInput{AssignmentID: assignment.ID}); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("abandon expired err = %v, want conflict", err)
	}

	service.now = func() time.Time { return now }
	task := createTask(t, store)
	cancelJob, err := service.EnqueueJob(ctx, EnqueueJobInput{TaskID: &task.ID, Role: RoleAuthor, CapacityBucket: BucketPersistentAgent, Payload: map[string]any{"agent_harness": "harness", "phase_index": 0, "final_phase": true}})
	if err != nil {
		t.Fatalf("enqueue cancellation job: %v", err)
	}
	cancelAssignment, err := service.ReserveAssignment(ctx, ReserveAssignmentInput{
		JobID: cancelJob.ID, ProviderID: "test-provider", ProfileName: "test-profile", ProviderType: "test",
		ProviderRequestID: "request-" + cancelJob.ID, AllowedRoles: []JobRole{RoleAuthor},
		AllowedBuckets: []CapacityBucket{BucketPersistentAgent}, StartupDeadline: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve cancellation assignment: %v", err)
	}
	if _, err := service.CancelLiveJobsForTask(ctx, task.ID, RoleAuthor); err != nil {
		t.Fatalf("cancel task jobs: %v", err)
	}
	canceled, _ := service.GetAssignment(ctx, cancelAssignment.ID)
	if canceled.State != AssignmentClosed || canceled.CloseReason == nil || *canceled.CloseReason != string(JobCanceled) {
		t.Fatalf("canceled assignment = %+v", canceled)
	}

	// Owner force-done and workflow cancellation paths update jobs directly rather
	// than calling CancelLiveJobsForTask. The schema must still close a claimed
	// assignment so the provisioner can revoke its credential and delete it.
	directJob, err := service.EnqueueJob(ctx, EnqueueJobInput{TaskID: &task.ID, Role: RoleCI, CapacityBucket: BucketEphemeral, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue direct cancellation job: %v", err)
	}
	directAssignment := reserveTestAssignment(t, service, directJob.ID, now, nil)
	registered, err := directory.RegisterWorker(ctx, RegisterWorkerInput{ID: directAssignment.WorkerID, CapacityBucket: BucketEphemeral})
	if err != nil {
		t.Fatalf("register direct assignment worker: %v", err)
	}
	if _, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{AssignmentID: directAssignment.ID, Worker: registered, LeaseDuration: time.Minute}); err != nil {
		t.Fatalf("claim direct cancellation assignment: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'canceled', updated_at = ? WHERE id = ?`, formatTime(now), directJob.ID); err != nil {
		t.Fatalf("directly cancel assigned job: %v", err)
	}
	directClosed, err := service.GetAssignment(ctx, directAssignment.ID)
	if err != nil {
		t.Fatalf("get directly canceled assignment: %v", err)
	}
	if directClosed.State != AssignmentClosed || directClosed.CloseReason == nil || *directClosed.CloseReason != string(JobCanceled) {
		t.Fatalf("directly canceled assignment = %+v", directClosed)
	}
}

func TestClaimedAssignmentClosesOnLeaseSweep(t *testing.T) {
	ctx := context.Background()
	_, directory, service := newWorkerService(t)
	now := time.Date(2026, 8, 2, 16, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{Role: RoleCI, CapacityBucket: BucketEphemeral, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	assignment := reserveTestAssignment(t, service, job.ID, now, nil)
	worker, err := directory.RegisterWorker(ctx, RegisterWorkerInput{ID: assignment.WorkerID, CapacityBucket: BucketEphemeral})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	claimed, err := service.ClaimAssignment(ctx, ClaimAssignmentInput{AssignmentID: assignment.ID, Worker: worker, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := service.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if swept, err := service.SweepExpiredLeases(ctx); err != nil || swept != 1 {
		t.Fatalf("sweep = %d, %v", swept, err)
	}
	closed, _ := service.GetAssignment(ctx, assignment.ID)
	if closed.State != AssignmentClosed || closed.CloseReason == nil || *closed.CloseReason != string(JobCrashed) {
		t.Fatalf("swept assignment = %+v", closed)
	}
}

func reserveTestAssignment(t *testing.T, service *Service, jobID string, now time.Time, labels map[string]string) Assignment {
	t.Helper()
	assignment, err := service.ReserveAssignment(context.Background(), ReserveAssignmentInput{
		JobID: jobID, ProviderID: "test-provider", ProfileName: "test-profile", ProviderType: "test",
		ProviderRequestID: "request-" + jobID, ProfileLabels: labels,
		AllowedRoles: []JobRole{RoleCI}, AllowedBuckets: []CapacityBucket{BucketEphemeral},
		StartupDeadline: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve assignment: %v", err)
	}
	return assignment
}

// ensureWorkerAssignmentsTable keeps this package independently testable while
// the project migration is landed by the owning implementation slice.
func ensureWorkerAssignmentsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS worker_assignments (
	id TEXT PRIMARY KEY,
	worker_id TEXT NOT NULL,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	provider_id TEXT NOT NULL,
	profile_name TEXT NOT NULL,
	provider_request_id TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('pending', 'claimed', 'closed')),
	role TEXT NOT NULL,
	capacity_bucket TEXT NOT NULL,
	profile_labels_json TEXT NOT NULL,
	profile_taints_json TEXT NOT NULL,
	profile_harness_models_json TEXT NOT NULL,
	allowed_roles_json TEXT NOT NULL,
	allowed_buckets_json TEXT NOT NULL,
	required_selector_json TEXT NOT NULL,
	startup_deadline TEXT NOT NULL,
	retry_count INTEGER NOT NULL DEFAULT 0,
	next_retry_at TEXT,
	last_attempt_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	closed_at TEXT,
	close_reason TEXT,
	last_provider_error TEXT,
	claimed_lease_id TEXT REFERENCES leases(id),
	cleaned_at TEXT,
	UNIQUE(provider_id, provider_request_id),
	UNIQUE(worker_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS worker_assignments_active_job_idx
	ON worker_assignments(job_id) WHERE state IN ('pending', 'claimed');`)
	if err != nil {
		t.Fatalf("create worker assignments test table: %v", err)
	}
}
