package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

type fakeCapacityCoordinator struct {
	*fakeCoordinator
	slots   []worker.CapacitySlot
	demand  contract.ProvisionerCapacityDemandResponse
	created int
}

func (f *fakeCapacityCoordinator) ListCapacitySlots(context.Context, []string) ([]worker.CapacitySlot, error) {
	return append([]worker.CapacitySlot(nil), f.slots...), nil
}

func (f *fakeCapacityCoordinator) CreateCapacitySlot(_ context.Context, request contract.CreateProvisionerCapacitySlotRequest) (contract.CreateProvisionerCapacitySlotResponse, error) {
	for i := range f.slots {
		if f.slots[i].ProviderID == request.ProviderID && f.slots[i].ProviderRequestID == request.ProviderRequestID {
			value := f.slots[i]
			return contract.CreateProvisionerCapacitySlotResponse{Created: true, Slot: &value, WorkerToken: "refreshed-token"}, nil
		}
	}
	f.created++
	now := time.Now().UTC()
	slot := worker.CapacitySlot{
		ID: fmt.Sprintf("slot-%d", f.created), WorkerID: fmt.Sprintf("worker-%d", f.created),
		ProviderID: request.ProviderID, ProfileName: request.ProfileName,
		ProviderRequestID: request.ProviderRequestID, ProviderType: request.ProviderType,
		ProviderOptions: request.ProviderOptions, State: worker.CapacitySlotProvisioning,
		ProfileLabels: request.Labels, ProfileTaints: request.Taints,
		ProfileHarnessModels: request.HarnessModels, AllowedRoles: request.AllowedRoles,
		AllowedBuckets: request.AllowedBuckets, RequiredSelector: request.RequiredSelector,
		StartupDeadline: now.Add(time.Duration(request.StartupTimeoutSeconds) * time.Second),
		CreatedAt:       now, UpdatedAt: now,
	}
	f.slots = append(f.slots, slot)
	return contract.CreateProvisionerCapacitySlotResponse{Created: true, Slot: &slot, WorkerToken: "worker-token"}, nil
}

func (f *fakeCapacityCoordinator) CapacityDemand(context.Context, contract.ProvisionerCapacityDemandRequest) (contract.ProvisionerCapacityDemandResponse, error) {
	return f.demand, nil
}

func (f *fakeCapacityCoordinator) BindCapacitySlot(_ context.Context, id string, _ contract.BindProvisionerCapacitySlotRequest) (contract.BindProvisionerCapacitySlotResponse, error) {
	for i := range f.slots {
		if f.slots[i].ID != id {
			continue
		}
		assignmentID, projectID := "assignment-"+id, "project-1"
		f.slots[i].State, f.slots[i].AssignmentID, f.slots[i].ProjectID = worker.CapacitySlotBound, &assignmentID, &projectID
		f.slots[i].UpdatedAt = time.Now().UTC()
		assignment := contract.ProvisionerAssignment{Project: contract.Project{ID: projectID}, Assignment: worker.Assignment{ID: assignmentID, CapacitySlotID: id, WorkerID: f.slots[i].WorkerID}}
		return contract.BindProvisionerCapacitySlotResponse{Bound: true, Slot: f.slots[i], Assignment: &assignment}, nil
	}
	return contract.BindProvisionerCapacitySlotResponse{}, errors.New("slot missing")
}

func (f *fakeCapacityCoordinator) RecordCapacitySlotAttempt(_ context.Context, id string, request contract.RecordProvisionerCapacitySlotAttemptRequest) (worker.CapacitySlot, error) {
	for i := range f.slots {
		if f.slots[i].ID == id {
			f.slots[i].RetryCount++
			f.slots[i].NextRetryAt = request.NextRetryAt
			return f.slots[i], nil
		}
	}
	return worker.CapacitySlot{}, errors.New("slot missing")
}

func (f *fakeCapacityCoordinator) CloseCapacitySlot(_ context.Context, id string, _ contract.CloseProvisionerCapacitySlotRequest) (worker.CapacitySlot, error) {
	for i := range f.slots {
		if f.slots[i].ID == id {
			now := time.Now().UTC()
			f.slots[i].State = worker.CapacitySlotClosed
			f.slots[i].ClosedAt = &now
			return f.slots[i], nil
		}
	}
	return worker.CapacitySlot{}, errors.New("slot missing")
}
func (f *fakeCapacityCoordinator) RevokeCapacitySlot(context.Context, string) (worker.CapacitySlot, error) {
	return worker.CapacitySlot{}, nil
}
func (f *fakeCapacityCoordinator) CleanCapacitySlot(context.Context, string) (worker.CapacitySlot, error) {
	return worker.CapacitySlot{}, nil
}

func TestCapacityReconcileUsesOneProbeBeforeFillingIdleAndDemandTarget(t *testing.T) {
	coordinator := &fakeCapacityCoordinator{fakeCoordinator: &fakeCoordinator{}, demand: contract.ProvisionerCapacityDemandResponse{DesiredInstances: 3}}
	provider := &fakeProvider{status: ProviderRunning}
	reconciler := testReconciler(t, coordinator, provider, time.Now)
	requestSequence := 0
	reconciler.newRequestID = func() (string, error) { requestSequence++; return fmt.Sprintf("request-%d", requestSequence), nil }
	reconciler.profiles[0].IdleCapacity = 1
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if coordinator.created != 1 || len(provider.launches) != 1 {
		t.Fatalf("unknown profile created=%d launches=%d, want one probe", coordinator.created, len(provider.launches))
	}
	now := time.Now().UTC()
	coordinator.slots[0].State = worker.CapacitySlotReady
	coordinator.slots[0].CapabilityCheckedAt = &now
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if coordinator.created != 3 || len(provider.launches) != 3 {
		t.Fatalf("healthy profile created=%d launches=%d, want target 3", coordinator.created, len(provider.launches))
	}
}

func TestCapacityReconcileUnreadyProfileOpensCircuitWithoutBulkProvisioning(t *testing.T) {
	now := time.Now().UTC()
	profile := testProfile()
	slot := worker.CapacitySlot{
		ID: "slot-probe", WorkerID: "worker-probe", ProviderID: profile.ProviderID,
		ProfileName: profile.ProfileName, ProviderRequestID: "probe", ProviderType: profile.ProviderType,
		ProviderOptions: profile.ProviderOptions, ProfileLabels: profile.Labels,
		State: worker.CapacitySlotUnready, CapabilityError: "Harness unavailable",
		StartupDeadline: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	coordinator := &fakeCapacityCoordinator{fakeCoordinator: &fakeCoordinator{}, slots: []worker.CapacitySlot{slot}, demand: contract.ProvisionerCapacityDemandResponse{DesiredInstances: 4}}
	provider := &fakeProvider{status: ProviderRunning}
	reconciler := testReconciler(t, coordinator, provider, time.Now)
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("unready capability probe did not fail readiness cycle")
	}
	if coordinator.created != 0 || len(provider.launches) != 0 {
		t.Fatalf("degraded profile bulk-provisioned: created=%d launches=%d", coordinator.created, len(provider.launches))
	}
}

func TestCapacityReconcileUnreadyProfileDoesNotBlockAnotherProfile(t *testing.T) {
	now := time.Now().UTC()
	degraded := testProfile()
	healthy := degraded
	healthy.ProfileName = "healthy-profile"
	slot := worker.CapacitySlot{
		ID: "slot-probe", WorkerID: "worker-probe", ProviderID: degraded.ProviderID,
		ProfileName: degraded.ProfileName, ProviderRequestID: "probe", ProviderType: degraded.ProviderType,
		ProviderOptions: degraded.ProviderOptions, ProfileLabels: degraded.Labels,
		State: worker.CapacitySlotUnready, CapabilityError: "Harness unavailable",
		StartupDeadline: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	coordinator := &fakeCapacityCoordinator{
		fakeCoordinator: &fakeCoordinator{}, slots: []worker.CapacitySlot{slot},
		demand: contract.ProvisionerCapacityDemandResponse{DesiredInstances: 2},
	}
	provider := &fakeProvider{status: ProviderRunning}
	reconciler := testReconciler(t, coordinator, provider, time.Now)
	reconciler.profiles = append(reconciler.profiles, healthy)
	reconciler.profileByID[profileKey(healthy.ProviderID, healthy.ProfileName)] = healthy
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("unready capability probe did not fail readiness cycle")
	}
	if coordinator.created != 1 || len(provider.launches) != 1 {
		t.Fatalf("healthy profile created=%d launches=%d, want one initial probe", coordinator.created, len(provider.launches))
	}
	if launched := provider.launches[0].Slot; launched == nil || launched.ProfileName != healthy.ProfileName {
		t.Fatalf("launched slot = %+v, want healthy profile", launched)
	}
}

func TestCapacityReconcileClosesUnboundSlotForRemovedProfile(t *testing.T) {
	now := time.Now().UTC()
	configured := testProfile()
	removed := worker.CapacitySlot{
		ID: "slot-removed", WorkerID: "worker-removed", ProviderID: configured.ProviderID,
		ProfileName: "removed-profile", ProviderRequestID: "removed-request", ProviderType: configured.ProviderType,
		ProviderOptions: configured.ProviderOptions, State: worker.CapacitySlotProvisioning,
		StartupDeadline: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	coordinator := &fakeCapacityCoordinator{fakeCoordinator: &fakeCoordinator{}, slots: []worker.CapacitySlot{removed}}
	provider := &fakeProvider{status: ProviderNotFound}
	reconciler := testReconciler(t, coordinator, provider, time.Now)
	reconciler.providerFactory = func(Profile) (Provider, error) { return provider, nil }
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if coordinator.slots[0].State != worker.CapacitySlotClosed {
		t.Fatalf("removed profile slot state = %s, want closed", coordinator.slots[0].State)
	}
	if len(provider.launches) != 0 {
		t.Fatalf("removed profile launched %d resources", len(provider.launches))
	}
}
