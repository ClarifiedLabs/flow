package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

type fakeCoordinator struct {
	mu              sync.Mutex
	assignments     []contract.ProvisionerAssignment
	listErr         error
	listProviderIDs []string
	reserveFn       func(contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error)
	reserves        []contract.ReserveProvisionerAssignmentRequest
	attempts        []attemptCall
	abandons        []string
	revoked         []string
	cleaned         []string
}

type attemptCall struct {
	id      string
	request contract.RecordProvisionerAssignmentAttemptRequest
}

func (f *fakeCoordinator) ListAssignments(_ context.Context, providerIDs []string) ([]contract.ProvisionerAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listProviderIDs != nil {
		providerIDs = f.listProviderIDs
	}
	allowed := make(map[string]struct{}, len(providerIDs))
	for _, providerID := range providerIDs {
		allowed[providerID] = struct{}{}
	}
	var assignments []contract.ProvisionerAssignment
	for _, assignment := range f.assignments {
		if _, ok := allowed[assignment.Assignment.ProviderID]; ok {
			assignments = append(assignments, assignment)
		}
	}
	return assignments, f.listErr
}

func (f *fakeCoordinator) Reserve(_ context.Context, request contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserves = append(f.reserves, request)
	if f.reserveFn != nil {
		return f.reserveFn(request)
	}
	return contract.ReserveProvisionerAssignmentResponse{}, nil
}

func (f *fakeCoordinator) RecordAttempt(_ context.Context, id string, request contract.RecordProvisionerAssignmentAttemptRequest) (contract.ProvisionerAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attemptCall{id: id, request: request})
	for i := range f.assignments {
		if f.assignments[i].Assignment.ID == id {
			f.assignments[i].Assignment.RetryCount++
			f.assignments[i].Assignment.NextRetryAt = request.NextRetryAt
			return f.assignments[i], nil
		}
	}
	return contract.ProvisionerAssignment{}, errors.New("assignment missing")
}

func (f *fakeCoordinator) Abandon(_ context.Context, id string, _ contract.AbandonProvisionerAssignmentRequest) (contract.ProvisionerAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandons = append(f.abandons, id)
	for i := range f.assignments {
		if f.assignments[i].Assignment.ID == id {
			f.assignments[i].Assignment.State = worker.AssignmentClosed
			return f.assignments[i], nil
		}
	}
	return contract.ProvisionerAssignment{}, errors.New("assignment missing")
}

func (f *fakeCoordinator) Revoke(_ context.Context, id string) (contract.ProvisionerAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	for i := range f.assignments {
		if f.assignments[i].Assignment.ID == id {
			now := time.Now()
			f.assignments[i].Assignment.CredentialsRevokedAt = &now
			return f.assignments[i], nil
		}
	}
	return contract.ProvisionerAssignment{}, errors.New("assignment missing")
}

func (f *fakeCoordinator) Cleaned(_ context.Context, id string) (contract.ProvisionerAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = append(f.cleaned, id)
	for i := range f.assignments {
		if f.assignments[i].Assignment.ID == id {
			now := time.Now()
			f.assignments[i].Assignment.CleanedAt = &now
			return f.assignments[i], nil
		}
	}
	return contract.ProvisionerAssignment{}, errors.New("assignment missing")
}

type fakeProvider struct {
	mu           sync.Mutex
	status       ProviderStatus
	healthErr    error
	healthChecks int
	inspectErr   error
	launchFn     func(LaunchRequest) error
	deleteFn     func(AssignmentIdentity) error
	launches     []LaunchRequest
	inspects     []AssignmentIdentity
	deletes      []AssignmentIdentity
	deleteErr    error
}

func (f *fakeProvider) Health(_ context.Context, _ Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthChecks++
	return f.healthErr
}

func (f *fakeProvider) Launch(_ context.Context, request LaunchRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, request)
	if f.launchFn != nil {
		return f.launchFn(request)
	}
	f.status = ProviderRunning
	return nil
}

func (f *fakeProvider) Inspect(_ context.Context, identity AssignmentIdentity) (ProviderStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects = append(f.inspects, identity)
	return f.status, f.inspectErr
}

func (f *fakeProvider) Delete(_ context.Context, identity AssignmentIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, identity)
	if f.deleteFn != nil {
		if err := f.deleteFn(identity); err != nil {
			return err
		}
	}
	if f.deleteErr == nil {
		f.status = ProviderNotFound
	}
	return f.deleteErr
}

func testAssignment(state worker.AssignmentState) contract.ProvisionerAssignment {
	return contract.ProvisionerAssignment{
		Project: contract.Project{ID: "project-1"},
		Assignment: worker.Assignment{
			ID: "assignment_1", WorkerID: "worker_1", JobID: "job-1",
			ProviderID: "local", ProfileName: "default", ProviderRequestID: "request-stable",
			ProviderType: "fake", ProviderOptions: map[string]string{"immutable": "persisted"}, State: state, CapacityBucket: worker.BucketEphemeral,
			ProfileLabels:   map[string]string{"pool": "test"},
			StartupDeadline: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}
}

func testProfile() Profile {
	return Profile{
		ProviderID: "local", ProfileName: "default", Provider: "fake", ProviderType: "fake",
		ProviderOptions: map[string]string{"immutable": "persisted"}, Labels: map[string]string{"pool": "test"},
		MaxConcurrency: 1, StartupTimeout: time.Minute,
	}
}

func testReconciler(t *testing.T, coordinator Coordinator, provider Provider, now func() time.Time) *Reconciler {
	t.Helper()
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": provider},
		PollInterval: time.Hour, RetryBase: 2 * time.Second, RetryMax: time.Minute, Now: now,
		NewRequestID: func() (string, error) { return "new-request", nil },
	})
	if err != nil {
		t.Fatalf("NewReconciler() error = %v", err)
	}
	return r
}

func TestReconcilerRestartRecoveryRepeatsStoredReservation(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	coordinator.reserveFn = func(request contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
		if request.ProviderRequestID != "request-stable" {
			t.Fatalf("provider request id = %q, want durable id", request.ProviderRequestID)
		}
		copy := assignment
		return contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "fresh-direct-token"}, nil
	}
	provider := &fakeProvider{status: ProviderNotFound}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.launches) != 1 || provider.launches[0].WorkerToken != "fresh-direct-token" {
		t.Fatalf("launches = %+v", provider.launches)
	}
	if len(coordinator.attempts) != 1 || coordinator.attempts[0].request.NextRetryAt != nil {
		t.Fatalf("attempts = %+v", coordinator.attempts)
	}
}

func TestReconcilerReserveResponseLossRecoveredIdempotently(t *testing.T) {
	coordinator := &fakeCoordinator{}
	provider := &fakeProvider{status: ProviderNotFound}
	assignment := testAssignment(worker.AssignmentPending)
	assignment.Assignment.ProviderRequestID = "new-request"
	calls := 0
	coordinator.reserveFn = func(request contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
		calls++
		if calls == 1 {
			coordinator.assignments = []contract.ProvisionerAssignment{assignment}
			return contract.ReserveProvisionerAssignmentResponse{}, errors.New("response lost")
		}
		copy := assignment
		return contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "replacement-token"}, nil
	}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("first Reconcile() succeeded after response loss")
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	if len(coordinator.reserves) != 2 || coordinator.reserves[1].ProviderRequestID != "new-request" {
		t.Fatalf("reserve requests = %+v", coordinator.reserves)
	}
	if len(provider.launches) != 1 {
		t.Fatalf("launch count = %d, want 1", len(provider.launches))
	}
}

func TestReconcilerLaunchResponseLossBacksOffAndRelaunchesIdempotently(t *testing.T) {
	now := time.Now()
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	coordinator.reserveFn = func(contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
		copy := coordinator.assignments[0]
		return contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "fresh-token"}, nil
	}
	provider := &fakeProvider{status: ProviderNotFound}
	launchCalls := 0
	provider.launchFn = func(LaunchRequest) error {
		launchCalls++
		if launchCalls == 1 {
			return errors.New("create response lost")
		}
		provider.status = ProviderRunning
		return nil
	}
	r := testReconciler(t, coordinator, provider, func() time.Time { return now })

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("first Reconcile() succeeded, want launch error")
	}
	if got := coordinator.attempts[0].request.NextRetryAt.Sub(now); got != 2*time.Second {
		t.Fatalf("first backoff = %v, want 2s", got)
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("backoff Reconcile() error = %v", err)
	}
	if launchCalls != 1 {
		t.Fatalf("launches during backoff = %d, want 1", launchCalls)
	}
	now = now.Add(3 * time.Second)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	if launchCalls != 2 {
		t.Fatalf("launch calls = %d, want idempotent second call", launchCalls)
	}
}

func TestReconcilerExponentialBackoff(t *testing.T) {
	now := time.Now()
	assignment := testAssignment(worker.AssignmentPending)
	assignment.Assignment.RetryCount = 3
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	coordinator.reserveFn = func(contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
		copy := assignment
		return contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "token"}, nil
	}
	provider := &fakeProvider{status: ProviderNotFound, launchFn: func(LaunchRequest) error { return errors.New("temporary") }}
	r := testReconciler(t, coordinator, provider, func() time.Time { return now })

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded, want launch error")
	}
	if got := coordinator.attempts[0].request.NextRetryAt.Sub(now); got != 16*time.Second {
		t.Fatalf("backoff = %v, want 16s", got)
	}
}

func TestReconcilerClaimedAssignmentDoesNotInspectProvider(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClaimed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{inspectErr: errors.New("provider unavailable")}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.inspects) != 0 || len(coordinator.reserves) != 0 {
		t.Fatalf("claimed assignment inspects=%d reserves=%d, want 0/0", len(provider.inspects), len(coordinator.reserves))
	}
}

func TestReconcilerIgnoresForeignProviderAssignments(t *testing.T) {
	foreign := testAssignment(worker.AssignmentPending)
	foreign.Assignment.ProviderID = "another-cluster"
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{foreign}}
	provider := &fakeProvider{inspectErr: errors.New("foreign assignment must not be inspected")}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.inspects) != 0 {
		t.Fatalf("foreign assignment inspect calls = %d, want 0", len(provider.inspects))
	}
	if len(coordinator.reserves) != 1 || coordinator.reserves[0].ProviderID != "local" {
		t.Fatalf("reserve calls = %+v, want local provider only", coordinator.reserves)
	}
}

func TestReconcilerRecoveryErrorBlocksNewReservations(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{status: ProviderRunning, inspectErr: errors.New("provider unavailable")}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite recovery error")
	}
	if len(coordinator.reserves) != 0 {
		t.Fatalf("reserve calls after recovery error = %d, want 0", len(coordinator.reserves))
	}
}

func TestReconcilerRecoversRemovedProfileFromPersistedProviderDescriptor(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	recovered := &fakeProvider{status: ProviderRunning}
	current := testProfile()
	current.ProfileName = "replacement"
	current.Provider = "replacement"
	var factoryProfile Profile
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{current}, Providers: map[string]Provider{"replacement": &fakeProvider{}},
		ProviderFactory: func(profile Profile) (Provider, error) {
			factoryProfile = profile
			return recovered, nil
		},
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if factoryProfile.ProviderType != "fake" || factoryProfile.ProviderOptions["immutable"] != "persisted" || len(recovered.inspects) != 1 {
		t.Fatalf("factory profile = %+v, inspects = %d", factoryProfile, len(recovered.inspects))
	}
}

func TestReconcilerRecoversAuthorizedProviderIDAfterItsFinalProfileIsRemoved(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{
		assignments: []contract.ProvisionerAssignment{assignment},
		// Emulate the coordinator authorizing both the removed provider ID and the
		// currently configured ID through the provisioner token subject.
		listProviderIDs: []string{"local", "replacement-provider"},
	}
	recovered := &fakeProvider{status: ProviderRunning}
	current := testProfile()
	current.ProviderID = "replacement-provider"
	current.ProfileName = "replacement"
	current.Provider = "replacement"
	var recoveredProfile Profile
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{current}, Providers: map[string]Provider{"replacement": &fakeProvider{}},
		ProviderFactory: func(profile Profile) (Provider, error) {
			recoveredProfile = profile
			return recovered, nil
		},
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if recoveredProfile.ProviderID != "local" || recoveredProfile.ProfileName != "default" || len(recovered.inspects) != 1 {
		t.Fatalf("recovered profile = %+v, inspects = %d", recoveredProfile, len(recovered.inspects))
	}
}

func TestReconcilerDoesNotLaunchUnapprovedPersistedProviderDescriptor(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	assignment.Assignment.ProviderOptions = map[string]string{"immutable": "attacker-controlled"}
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	configured := &fakeProvider{status: ProviderNotFound}
	persisted := &fakeProvider{status: ProviderNotFound}
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": configured},
		ProviderFactory: func(Profile) (Provider, error) { return persisted, nil },
		PollInterval:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() accepted an unapproved provider descriptor")
	}
	if len(configured.launches) != 0 || len(persisted.launches) != 0 {
		t.Fatalf("unapproved provider was launched: configured=%d persisted=%d", len(configured.launches), len(persisted.launches))
	}
	if len(persisted.inspects) != 1 || len(coordinator.abandons) != 1 || len(coordinator.cleaned) != 1 {
		t.Fatalf("inspects=%d abandons=%v cleaned=%v", len(persisted.inspects), coordinator.abandons, coordinator.cleaned)
	}
}

func TestReconcilerCleansIncompleteResourceForUnapprovedDescriptor(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	assignment.Assignment.ProviderOptions = map[string]string{"immutable": "removed-configuration"}
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	persisted := &fakeProvider{status: ProviderIncomplete}
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": &fakeProvider{}},
		ProviderFactory: func(Profile) (Provider, error) { return persisted, nil },
		PollInterval:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() incomplete cleanup error = %v", err)
	}
	if len(persisted.launches) != 0 || len(persisted.deletes) != 1 || len(coordinator.abandons) != 1 || len(coordinator.cleaned) != 1 {
		t.Fatalf("launches=%d deletes=%d abandons=%v cleaned=%v", len(persisted.launches), len(persisted.deletes), coordinator.abandons, coordinator.cleaned)
	}
}

func TestReconcilerChangedDescriptorCleanupBypassesReplacementHealthFailure(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	assignment.Assignment.ProviderOptions = map[string]string{"immutable": "previous"}
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	configured := &fakeProvider{healthErr: errors.New("replacement unavailable")}
	persisted := &fakeProvider{status: ProviderIncomplete}
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": configured},
		ProviderFactory: func(Profile) (Provider, error) { return persisted, nil },
		PollInterval:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite replacement provider health failure")
	}
	if len(configured.inspects) != 0 || len(persisted.inspects) != 1 || len(persisted.deletes) != 1 {
		t.Fatalf("configured inspects=%d persisted inspects=%d deletes=%d", len(configured.inspects), len(persisted.inspects), len(persisted.deletes))
	}
	if len(coordinator.abandons) != 1 || len(coordinator.cleaned) != 1 {
		t.Fatalf("abandons=%v cleaned=%v, want stale assignment fenced and cleaned", coordinator.abandons, coordinator.cleaned)
	}
}

func TestReconcilerRefreshesCredentialBeforeDeletingIncompleteResource(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	var events []string
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	coordinator.reserveFn = func(contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
		events = append(events, "reserve")
		copy := assignment
		return contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "replacement-token"}, nil
	}
	provider := &fakeProvider{status: ProviderIncomplete}
	provider.deleteFn = func(AssignmentIdentity) error {
		events = append(events, "delete")
		return nil
	}
	provider.launchFn = func(request LaunchRequest) error {
		events = append(events, "launch")
		if request.WorkerToken != "replacement-token" {
			t.Fatalf("launch token = %q, want replacement token", request.WorkerToken)
		}
		return nil
	}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if want := []string{"reserve", "delete", "launch"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("incomplete relaunch events = %v, want %v", events, want)
	}
}

func TestReconcilerProviderFailureAbandonsPending(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{status: ProviderFailed}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletes) != 1 || len(coordinator.abandons) != 1 || coordinator.abandons[0] != assignment.Assignment.ID {
		t.Fatalf("deletes=%d abandons=%v", len(provider.deletes), coordinator.abandons)
	}
}

func TestReconcilerNeverAbandonsClaimedAssignment(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClaimed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{status: ProviderFailed}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(coordinator.abandons) != 0 || len(provider.deletes) != 0 {
		t.Fatalf("claimed assignment was changed: abandons=%v deletes=%v", coordinator.abandons, provider.deletes)
	}
	if len(provider.inspects) != 0 {
		t.Fatalf("claimed assignment inspect count = %d, want 0", len(provider.inspects))
	}
}

func TestReconcilerClosedDeletesThenMarksCleaned(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClosed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{status: ProviderSucceeded}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(coordinator.revoked) != 1 || len(provider.deletes) != 1 || len(coordinator.cleaned) != 1 {
		t.Fatalf("revoked=%v deletes=%d cleaned=%v", coordinator.revoked, len(provider.deletes), coordinator.cleaned)
	}
}

func TestReconcilerCredentialRevocationIsDurableBeforeDeleteRetry(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClosed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{deleteErr: errors.New("delete unavailable")}
	r := testReconciler(t, coordinator, provider, time.Now)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("first Reconcile() succeeded despite delete error")
	}
	if len(coordinator.revoked) != 1 || len(coordinator.cleaned) != 0 {
		t.Fatalf("first cycle revoked=%v cleaned=%v", coordinator.revoked, coordinator.cleaned)
	}
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("second Reconcile() succeeded despite delete error")
	}
	if len(coordinator.revoked) != 1 {
		t.Fatalf("credential revocation calls = %d, want durable single call", len(coordinator.revoked))
	}
}

func TestReconcilerRevokesClosedCredentialBeforeProviderRecovery(t *testing.T) {
	closed := testAssignment(worker.AssignmentClosed)
	closed.Assignment.ProviderType = "removed-provider"
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{closed}}
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": &fakeProvider{}},
		ProviderFactory: func(Profile) (Provider, error) { return nil, errors.New("provider recovery unavailable") },
		PollInterval:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite provider recovery failure")
	}
	if len(coordinator.revoked) != 1 {
		t.Fatalf("credential revocation calls = %v, want closed credential fenced before provider recovery", coordinator.revoked)
	}
}

func TestReconcilerUnhealthyProfileDoesNotBlockClosedCleanupOrHealthyReservations(t *testing.T) {
	closed := testAssignment(worker.AssignmentClosed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{closed}}
	unhealthy := &fakeProvider{healthErr: errors.New("provider unavailable")}
	healthy := &fakeProvider{}
	healthyProfile := testProfile()
	healthyProfile.ProviderID = "healthy-provider"
	healthyProfile.Provider = "healthy"
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles:     []Profile{testProfile(), healthyProfile},
		Providers:    map[string]Provider{"fake": unhealthy, "healthy": healthy},
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() succeeded despite unhealthy profile")
	}
	if len(coordinator.revoked) != 1 || len(unhealthy.deletes) != 1 || len(coordinator.cleaned) != 1 {
		t.Fatalf("closed cleanup revoked=%v deletes=%d cleaned=%v", coordinator.revoked, len(unhealthy.deletes), coordinator.cleaned)
	}
	if len(coordinator.reserves) != 1 || coordinator.reserves[0].ProviderID != "healthy-provider" {
		t.Fatalf("healthy profile reserve calls = %+v", coordinator.reserves)
	}
}

func TestReconcilerReadinessAndAssignmentMetrics(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClaimed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}, listErr: errors.New("down")}
	provider := &fakeProvider{status: ProviderRunning}
	registry := metrics.New()
	r, err := NewReconciler(ReconcilerOptions{
		Coordinator: coordinator, CoordinatorURL: "https://coordinator.example",
		Profiles: []Profile{testProfile()}, Providers: map[string]Provider{"fake": provider},
		PollInterval: time.Hour, Metrics: NewMetrics(registry),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.runCycle(context.Background())
	if r.Ready() {
		t.Fatal("reconciler ready after failed list")
	}
	coordinator.listErr = nil
	r.runCycle(context.Background())
	if !r.Ready() {
		t.Fatal("reconciler not ready after complete cycle")
	}
	coordinator.listErr = errors.New("down again")
	r.runCycle(context.Background())
	if r.Ready() {
		t.Fatal("reconciler remained ready after losing coordinator visibility")
	}
	var output strings.Builder
	registry.Render(&output)
	for _, want := range []string{
		`flow_orchestrator_reconcile_errors_total{operation="list"} 2`,
		`flow_orchestrator_active_assignments{profile="default",provider="local",state="claimed"} 1`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, output.String())
		}
	}
}

func TestReconcilerReadinessDegradesWhenProviderIsBlindWithClaimedWork(t *testing.T) {
	assignment := testAssignment(worker.AssignmentClaimed)
	coordinator := &fakeCoordinator{assignments: []contract.ProvisionerAssignment{assignment}}
	provider := &fakeProvider{status: ProviderRunning}
	r := testReconciler(t, coordinator, provider, time.Now)

	r.runCycle(context.Background())
	if !r.Ready() {
		t.Fatal("reconciler not ready after provider health check")
	}
	if provider.healthChecks != 1 || len(provider.inspects) != 0 {
		t.Fatalf("healthy cycle checks=%d assignment inspections=%d", provider.healthChecks, len(provider.inspects))
	}
	provider.healthErr = errors.New("provider unavailable")
	r.runCycle(context.Background())
	if r.Ready() {
		t.Fatal("reconciler remained ready after losing provider visibility")
	}
	if provider.healthChecks != 2 || len(provider.inspects) != 0 {
		t.Fatalf("blind cycle checks=%d assignment inspections=%d", provider.healthChecks, len(provider.inspects))
	}
}

func TestCoordinatorClientTypedEndpointsHeadersAndErrors(t *testing.T) {
	assignment := testAssignment(worker.AssignmentPending)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != contract.AuthScheme+"orchestrator-token" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		if r.Header.Get(contract.ProtocolHeader) != contract.ProtocolVersion {
			http.Error(w, "missing protocol", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case assignmentsPath:
			query := r.URL.Query()
			if query.Get("provider_id") != "" || (query.Get("open_only") == "") == (query.Get("needs_cleanup") == "") {
				http.Error(w, "missing lifecycle filter", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(contract.ProvisionerAssignmentsResponse{Assignments: []contract.ProvisionerAssignment{assignment}})
		case assignmentsPath + "/reserve":
			var request contract.ReserveProvisionerAssignmentRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			copy := assignment
			copy.Assignment.ProviderRequestID = request.ProviderRequestID
			_ = json.NewEncoder(w).Encode(contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &copy, WorkerToken: "direct"})
		case assignmentsPath + "/assignment_1/attempt", assignmentsPath + "/assignment_1/abandon", assignmentsPath + "/assignment_1/revoked", assignmentsPath + "/assignment_1/cleaned":
			_ = json.NewEncoder(w).Encode(contract.ProvisionerAssignmentResponse{Assignment: assignment})
		case "/error":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(contract.ErrorResponse{Error: contract.ErrorBody{Code: "forbidden", Message: "denied"}})
		}
	}))
	t.Cleanup(server.Close)
	client := NewCoordinatorClient(server.URL, "orchestrator-token", server.Client())
	ctx := context.Background()
	assignments, err := client.ListAssignments(ctx, []string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 {
		t.Fatalf("ListAssignments() returned %d duplicate assignments", len(assignments))
	}
	if _, err := client.Reserve(ctx, testProfile().reserveRequest("request")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RecordAttempt(ctx, "assignment_1", contract.RecordProvisionerAssignmentAttemptRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Abandon(ctx, "assignment_1", contract.AbandonProvisionerAssignmentRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Revoke(ctx, "assignment_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Cleaned(ctx, "assignment_1"); err != nil {
		t.Fatal(err)
	}
	wantListPaths := []string{
		"GET " + assignmentsPath + "?open_only=true",
		"GET " + assignmentsPath + "?needs_cleanup=true",
	}
	if len(paths) != 7 || !reflect.DeepEqual(paths[:2], wantListPaths) {
		t.Fatalf("paths = %v", paths)
	}

	var target contract.ProvisionerAssignmentsResponse
	err = client.do(ctx, http.MethodGet, "/error", nil, &target)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.Message != "denied" {
		t.Fatalf("error = %#v", err)
	}
}
