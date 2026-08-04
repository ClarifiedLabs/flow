package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func TestProvisionerAssignmentsAuthorization(t *testing.T) {
	fixture := newTestFixture(t)
	mintTestCredential(t, fixture.Registry, "orchestrator-token", coordinator.TokenScopeProvisioner, "test-provider")
	mintTestCredential(t, fixture.Registry, "provisioner-worker-token", coordinator.TokenScopeWorker, "w-provisioner-auth")

	request := httptest.NewRequest(http.MethodGet, provisionerAssignmentsPath, nil)
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	response := httptest.NewRecorder()
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", response.Code)
	}

	doJSONRequestAs(t, fixture.Server, "provisioner-worker-token", http.MethodGet, provisionerAssignmentsPath, nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "orchestrator-token", http.MethodGet, provisionerAssignmentsPath, nil, http.StatusOK, &contract.ProvisionerAssignmentsResponse{})
	doJSONRequestAs(t, fixture.Server, "orchestrator-token", http.MethodGet, provisionerAssignmentsPath+"?provider_id=test-provider", nil, http.StatusOK, &contract.ProvisionerAssignmentsResponse{})
	doJSONRequestAs(t, fixture.Server, "orchestrator-token", http.MethodGet, provisionerAssignmentsPath+"?provider_id=foreign-provider", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "orchestrator-token", http.MethodPost, provisionerAssignmentsPath+"/reserve", contract.ReserveProvisionerAssignmentRequest{ProviderID: "foreign-provider"}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, provisionerAssignmentsPath, nil, http.StatusOK, &contract.ProvisionerAssignmentsResponse{})
}

func TestProvisionerAssignmentListUsesTokenProviderBindingsWithoutQueryFilter(t *testing.T) {
	fixture := newTestFixture(t)
	mintTestCredential(t, fixture.Registry, "orchestrator-token", coordinator.TokenScopeProvisioner, "retired-provider")
	ctx := context.Background()
	for i, providerID := range []string{"retired-provider", "foreign-provider"} {
		job, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Payload: map[string]any{"blocking": true}})
		if err != nil {
			t.Fatal(err)
		}
		record, _, reserved, err := fixture.Registry.ReserveProvisionerAssignment(ctx, reserveProvisionerAssignmentInput{
			ProviderID: providerID, ProviderRequestID: providerID + "-request", ProfileName: "removed-profile", ProviderType: "kubernetes",
			MaxConcurrency: 1, StartupTimeout: time.Minute,
		})
		if err != nil || !reserved {
			t.Fatalf("reserve %s = %+v, %v, %v", providerID, record, reserved, err)
		}
		if record.Assignment.JobID != job.ID {
			t.Fatalf("reserve %d job = %s, want %s", i, record.Assignment.JobID, job.ID)
		}
	}

	var listed contract.ProvisionerAssignmentsResponse
	doJSONRequestAs(t, fixture.Server, "orchestrator-token", http.MethodGet, provisionerAssignmentsPath+"?open_only=true", nil, http.StatusOK, &listed)
	if len(listed.Assignments) != 1 || listed.Assignments[0].Assignment.ProviderID != "retired-provider" {
		t.Fatalf("authorized assignment list = %+v", listed.Assignments)
	}
	var ownerListed contract.ProvisionerAssignmentsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, provisionerAssignmentsPath+"?open_only=true", nil, http.StatusOK, &ownerListed)
	if len(ownerListed.Assignments) != 2 {
		t.Fatalf("owner assignment list length = %d, want 2", len(ownerListed.Assignments))
	}
}

func TestRegistryExpiresPendingAssignmentsForGenericClaims(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha")
	ctx := context.Background()
	job, err := bundles[0].Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatal(err)
	}
	record, _, reserved, err := server.registry.ReserveProvisionerAssignment(ctx, reserveProvisionerAssignmentInput{
		ProviderID: "test-provider", ProviderRequestID: "expiry-request", ProfileName: "test-profile", ProviderType: "kubernetes",
		ProviderOptions: map[string]string{}, MaxConcurrency: 1, StartupTimeout: time.Minute,
		Candidate: worker.AssignmentCandidateFilter{AllowedRoles: []worker.JobRole{worker.RoleCI}, AllowedBuckets: []worker.CapacityBucket{worker.BucketEphemeral}},
	})
	if err != nil || !reserved {
		t.Fatalf("reserve assignment = %+v, %t, %v", record, reserved, err)
	}
	if _, err := bundles[0].Store.DB().ExecContext(ctx, `UPDATE worker_assignments SET startup_deadline = ? WHERE id = ?`, "2000-01-01T00:00:00Z", record.Assignment.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := server.registry.ExpirePendingProvisionerAssignments(ctx)
	if err != nil || expired != 1 {
		t.Fatalf("expire pending assignments = %d, %v", expired, err)
	}
	assignment, err := bundles[0].Queue.GetAssignment(ctx, record.Assignment.ID)
	if err != nil || assignment.State != worker.AssignmentClosed {
		t.Fatalf("expired assignment = %+v, %v", assignment, err)
	}
	candidate, found, err := bundles[0].Queue.FindAssignmentCandidate(ctx, worker.AssignmentCandidateFilter{})
	if err != nil || !found || candidate.ID != job.ID {
		t.Fatalf("generic candidate after expiry = %+v, %t, %v", candidate, found, err)
	}
}

func TestProvisionerReserveIsIdempotentAndEnforcesGlobalProfileConcurrency(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	mintTestCredential(t, server.registry, "orchestrator-token", coordinator.TokenScopeProvisioner, "test-provider")
	ctx := context.Background()
	for _, bundle := range bundles {
		if _, err := bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Payload: map[string]any{"blocking": true}}); err != nil {
			t.Fatalf("enqueue in %s: %v", bundle.Project.ID, err)
		}
	}

	request := reserveTestProvisionerRequest("provider-request-1", 1)
	var first contract.ReserveProvisionerAssignmentResponse
	doJSONRequestAs(t, server, "orchestrator-token", http.MethodPost, provisionerAssignmentsPath+"/reserve", request, http.StatusOK, &first)
	if !first.Reserved || first.Assignment == nil || first.WorkerToken == "" {
		t.Fatalf("first reserve = %+v", first)
	}
	if first.Assignment.Project.ID == "" || first.Assignment.Project.Name == "" {
		t.Fatalf("first reserve lacks project identity: %+v", first.Assignment.Project)
	}
	mintTestCredential(t, server.registry, "foreign-orchestrator-token", coordinator.TokenScopeProvisioner, "foreign-provider")
	doJSONRequestAs(t, server, "foreign-orchestrator-token", http.MethodPost, provisionerAssignmentsPath+"/"+first.Assignment.Assignment.ID+"/abandon", contract.AbandonProvisionerAssignmentRequest{ProviderError: "must not be authorized"}, http.StatusForbidden, nil)

	var repeated contract.ReserveProvisionerAssignmentResponse
	doJSONRequestAs(t, server, "orchestrator-token", http.MethodPost, provisionerAssignmentsPath+"/reserve", request, http.StatusOK, &repeated)
	if !repeated.Reserved || repeated.Assignment == nil || repeated.Assignment.Assignment.ID != first.Assignment.Assignment.ID {
		t.Fatalf("repeated reserve = %+v, want assignment %s", repeated, first.Assignment.Assignment.ID)
	}
	if repeated.WorkerToken == "" || repeated.WorkerToken == first.WorkerToken {
		t.Fatalf("repeated reserve token was not freshly issued")
	}
	if _, err := server.registry.credentials.Authenticate(ctx, first.WorkerToken); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("superseded assignment credential remains valid: %v", err)
	}
	if principal, err := server.registry.credentials.Authenticate(ctx, repeated.WorkerToken); err != nil || principal.Subject != first.Assignment.Assignment.WorkerID {
		t.Fatalf("replacement assignment credential = %+v, %v", principal, err)
	}

	var blocked contract.ReserveProvisionerAssignmentResponse
	doJSONRequestAs(t, server, "orchestrator-token", http.MethodPost, provisionerAssignmentsPath+"/reserve", reserveTestProvisionerRequest("provider-request-2", 1), http.StatusOK, &blocked)
	if blocked.Reserved || blocked.Assignment != nil || blocked.WorkerToken != "" {
		t.Fatalf("reserve above global max concurrency = %+v", blocked)
	}

	var listed contract.ProvisionerAssignmentsResponse
	doJSONRequestAs(t, server, "orchestrator-token", http.MethodGet, provisionerAssignmentsPath+"?open_only=true", nil, http.StatusOK, &listed)
	if len(listed.Assignments) != 1 || listed.Assignments[0].Assignment.ID != first.Assignment.Assignment.ID || listed.Assignments[0].Project.ID != first.Assignment.Project.ID {
		t.Fatalf("listed assignments = %+v", listed.Assignments)
	}
}

func TestAssignedWorkerRegistrationAndClaimAreExactAndExcludeGenericWorkers(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	mintTestCredential(t, fixture.Registry, "generic-worker-token", coordinator.TokenScopeWorker, "w-generic")
	assignedJob, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Priority: 100, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue assigned job: %v", err)
	}
	genericJob, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Priority: 1, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue generic job: %v", err)
	}

	request := reserveTestProvisionerRequest("exact-claim-request", 2)
	request.Labels = map[string]string{"pool": "managed"}
	var reserved contract.ReserveProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/reserve", request, http.StatusOK, &reserved)
	if reserved.Assignment == nil || reserved.Assignment.Assignment.JobID != assignedJob.ID {
		t.Fatalf("reserved assignment = %+v, want job %s", reserved, assignedJob.ID)
	}

	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/register", contract.RegisterWorkerRequest{
		Labels: map[string]string{"wrong": "profile"},
	}, http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/register", contract.RegisterWorkerRequest{
		Labels: map[string]string{"pool": "managed", "discovered": "true"},
	}, http.StatusOK, nil)
	// Workers without an open assignment cannot register or claim: there is no
	// static worker admission and no general queue fallback.
	doJSONRequestAs(t, fixture.Server, "generic-worker-token", http.MethodPost, "/v2/workers/register", contract.RegisterWorkerRequest{}, http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, "generic-worker-token", http.MethodPost, "/v2/workers/claim", contract.ClaimJobRequest{LeaseDurationSeconds: 60}, http.StatusConflict, nil)
	generic, err := fixture.Bundle.Queue.GetJob(ctx, genericJob.ID)
	if err != nil || generic.State != worker.JobQueued {
		t.Fatalf("generic job = %+v, err=%v; unassigned workers must not claim it", generic, err)
	}

	var assignedClaim claimJobResponse
	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/claim", contract.ClaimJobRequest{LeaseDurationSeconds: 60}, http.StatusOK, &assignedClaim)
	if !assignedClaim.Claimed || assignedClaim.Job == nil || assignedClaim.Job.ID != assignedJob.ID {
		t.Fatalf("assigned claim = %+v, want exact job %s", assignedClaim, assignedJob.ID)
	}
	assignment, err := fixture.Bundle.Queue.GetAssignment(ctx, reserved.Assignment.Assignment.ID)
	if err != nil || assignment.State != worker.AssignmentClaimed || assignment.ClaimedLeaseID == nil || *assignment.ClaimedLeaseID != assignedClaim.Lease.ID {
		t.Fatalf("claimed assignment = %+v, err=%v", assignment, err)
	}
}

func TestAssignedWorkerClaimNeverFallsBackWhenExactJobIsUnavailable(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	assignedJob, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Priority: 100, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue assigned job: %v", err)
	}
	fallbackJob, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Priority: 1, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue fallback job: %v", err)
	}
	var reserved contract.ReserveProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/reserve", reserveTestProvisionerRequest("no-fallback-request", 1), http.StatusOK, &reserved)
	if reserved.Assignment == nil || reserved.Assignment.Assignment.JobID != assignedJob.ID {
		t.Fatalf("reserved = %+v, want job %s", reserved, assignedJob.ID)
	}
	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/register", contract.RegisterWorkerRequest{}, http.StatusOK, nil)
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE jobs SET state = ? WHERE id = ?`, string(worker.JobCanceled), assignedJob.ID); err != nil {
		t.Fatalf("make assigned job unavailable: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/claim", contract.ClaimJobRequest{LeaseDurationSeconds: 60}, http.StatusConflict, nil)
	fallback, err := fixture.Bundle.Queue.GetJob(ctx, fallbackJob.ID)
	if err != nil || fallback.State != worker.JobQueued {
		t.Fatalf("fallback job = %+v, err=%v; assigned worker must not claim it", fallback, err)
	}
}

func TestProvisionerAbandonPreservesQueueAndCleanupRevokesAndRemovesWorker(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	job, err := fixture.Bundle.Queue.EnqueueJob(ctx, worker.EnqueueJobInput{Role: worker.RoleCI, CapacityBucket: worker.BucketEphemeral, Priority: 10, Payload: map[string]any{"blocking": true}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	before, err := fixture.Bundle.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job before reserve: %v", err)
	}

	var reserved contract.ReserveProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/reserve", reserveTestProvisionerRequest("abandon-request", 1), http.StatusOK, &reserved)
	assignment := reserved.Assignment.Assignment
	doJSONRequestAs(t, fixture.Server, reserved.WorkerToken, http.MethodPost, "/v2/workers/register", contract.RegisterWorkerRequest{}, http.StatusOK, nil)

	nextRetry := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	var attempted contract.ProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/"+assignment.ID+"/attempt", contract.RecordProvisionerAssignmentAttemptRequest{
		ProviderError: "provider warming", NextRetryAt: &nextRetry,
	}, http.StatusOK, &attempted)
	if attempted.Assignment.Assignment.RetryCount != 1 || attempted.Assignment.Assignment.NextRetryAt == nil || attempted.Assignment.Assignment.LastAttemptAt == nil {
		t.Fatalf("attempt response = %+v", attempted.Assignment.Assignment)
	}

	var abandoned contract.ProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/"+assignment.ID+"/abandon", contract.AbandonProvisionerAssignmentRequest{ProviderError: "pod create failed"}, http.StatusOK, &abandoned)
	if abandoned.Assignment.Assignment.State != worker.AssignmentClosed || abandoned.Assignment.Assignment.CloseReason == nil || *abandoned.Assignment.Assignment.CloseReason != "provider_failed" || abandoned.Assignment.Assignment.LastProviderError == nil {
		t.Fatalf("abandoned assignment = %+v", abandoned.Assignment.Assignment)
	}
	after, err := fixture.Bundle.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job after abandon: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("abandon changed queued job:\nbefore=%+v\nafter=%+v", before, after)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, reserved.WorkerToken); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("abandoned worker token authenticate error = %v, want invalid credential", err)
	}
	if _, err := fixture.Registry.Directory().GetWorker(ctx, assignment.WorkerID); err != nil {
		t.Fatalf("registered worker missing before cleanup: %v", err)
	}

	var cleaned contract.ProvisionerAssignmentResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/"+assignment.ID+"/cleaned", nil, http.StatusOK, &cleaned)
	if cleaned.Assignment.Assignment.CleanedAt == nil {
		t.Fatalf("cleaned assignment = %+v", cleaned.Assignment.Assignment)
	}
	if _, err := fixture.Registry.Directory().GetWorker(ctx, assignment.WorkerID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("worker after cleanup error = %v, want no rows", err)
	}
	// Cleanup is idempotent and continues to fence every token for the subject.
	doJSONRequest(t, fixture.Server, http.MethodPost, provisionerAssignmentsPath+"/"+assignment.ID+"/cleaned", nil, http.StatusOK, &contract.ProvisionerAssignmentResponse{})
}

func reserveTestProvisionerRequest(requestID string, maxConcurrency int) contract.ReserveProvisionerAssignmentRequest {
	return contract.ReserveProvisionerAssignmentRequest{
		ProviderID: "test-provider", ProviderRequestID: requestID, ProfileName: "test-profile", ProviderType: "test",
		ProviderOptions: map[string]string{"immutable": "value"}, MaxConcurrency: maxConcurrency, AllowedRoles: []worker.JobRole{worker.RoleCI},
		AllowedBuckets: []worker.CapacityBucket{worker.BucketEphemeral}, StartupTimeoutSeconds: 60,
	}
}

func mintTestCredential(t *testing.T, registry *Registry, token string, scope coordinator.TokenScope, subject string) {
	t.Helper()
	if err := registry.Credentials().EnsureToken(context.Background(), coordinator.CredentialInput{Token: token, Scope: scope, Subject: subject}); err != nil {
		t.Fatalf("mint %s credential: %v", scope, err)
	}
}
