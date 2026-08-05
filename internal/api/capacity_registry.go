package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

type createCapacitySlotInput struct {
	Slot         worker.CreateCapacitySlotInput
	MaxInstances int
}

func (r *Registry) CreateProvisionerCapacitySlot(ctx context.Context, input createCapacitySlotInput) (worker.CapacitySlot, string, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if input.Slot.ProviderOptions == nil {
		input.Slot.ProviderOptions = map[string]string{}
	}
	if input.MaxInstances <= 0 {
		return worker.CapacitySlot{}, "", false, errors.New("max_instances must be positive")
	}
	existing, err := r.capacitySlots.FindByRequest(ctx, input.Slot.ProviderID, input.Slot.ProviderRequestID)
	if err == nil {
		if existing.State == worker.CapacitySlotClosed {
			return worker.CapacitySlot{}, "", false, fmt.Errorf("%w: provider request belongs to a closed capacity slot", worker.ErrCapacitySlotConflict)
		}
		if existing.ProfileName != strings.TrimSpace(input.Slot.ProfileName) || existing.ProviderType != strings.TrimSpace(input.Slot.ProviderType) || !reflect.DeepEqual(existing.ProviderOptions, input.Slot.ProviderOptions) {
			return worker.CapacitySlot{}, "", false, fmt.Errorf("%w: provider request belongs to another profile or descriptor", worker.ErrCapacitySlotConflict)
		}
		token, err := r.credentials.ReplaceSubjectToken(ctx, coordinator.CredentialInput{Scope: coordinator.TokenScopeWorker, Subject: existing.WorkerID})
		return existing, token, err == nil, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return worker.CapacitySlot{}, "", false, err
	}
	open, err := r.capacitySlots.List(ctx, worker.CapacitySlotFilter{ProviderID: input.Slot.ProviderID, ProfileName: input.Slot.ProfileName, OpenOnly: true})
	if err != nil {
		return worker.CapacitySlot{}, "", false, err
	}
	if len(open) >= input.MaxInstances {
		return worker.CapacitySlot{}, "", false, nil
	}
	slot, err := r.capacitySlots.Create(ctx, input.Slot)
	if err != nil {
		return worker.CapacitySlot{}, "", false, err
	}
	token, err := r.credentials.CreateToken(ctx, coordinator.CredentialInput{Scope: coordinator.TokenScopeWorker, Subject: slot.WorkerID})
	if err != nil {
		_, _ = r.capacitySlots.Close(ctx, slot.ID, "credential_issue_failed", err.Error())
		return worker.CapacitySlot{}, "", false, fmt.Errorf("issue capacity slot worker credential: %w", err)
	}
	return slot, token, true, nil
}

func (r *Registry) ListProvisionerCapacitySlots(ctx context.Context, filter worker.CapacitySlotFilter) ([]worker.CapacitySlot, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	return r.capacitySlots.List(ctx, filter)
}

func (r *Registry) ProvisionerCapacityDemand(ctx context.Context, providerID, profileName string, maxConcurrency, idleCapacity int, candidate worker.AssignmentCandidateFilter) (active, queued, desired int, err error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(profileName) == "" {
		return 0, 0, 0, errors.New("provider_id and profile_name are required")
	}
	if maxConcurrency <= 0 || idleCapacity < 0 {
		return 0, 0, 0, errors.New("max_concurrency must be positive and idle_capacity non-negative")
	}
	for _, bundle := range r.All() {
		count, countErr := bundle.Queue.CountOpenAssignments(ctx, providerID, profileName)
		if countErr != nil {
			return 0, 0, 0, countErr
		}
		active += count
		count, countErr = bundle.Queue.CountAssignmentCandidates(ctx, candidate)
		if countErr != nil {
			return 0, 0, 0, countErr
		}
		queued += count
	}
	desiredActive := active + queued
	if desiredActive > maxConcurrency {
		desiredActive = maxConcurrency
	}
	return active, queued, desiredActive + idleCapacity, nil
}

func (r *Registry) BindProvisionerCapacitySlot(ctx context.Context, slotID string, capabilityTTL time.Duration) (worker.CapacitySlot, provisionerAssignmentRecord, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if capabilityTTL <= 0 {
		capabilityTTL = 2 * time.Minute
	}
	slot, err := r.capacitySlots.Get(ctx, slotID)
	if err != nil {
		return worker.CapacitySlot{}, provisionerAssignmentRecord{}, false, err
	}
	if existing, found, findErr := r.findAssignmentByWorkerLocked(ctx, slot.WorkerID); findErr != nil {
		return worker.CapacitySlot{}, provisionerAssignmentRecord{}, false, findErr
	} else if found {
		if existing.Assignment.CapacitySlotID != slot.ID {
			return worker.CapacitySlot{}, provisionerAssignmentRecord{}, false, fmt.Errorf("%w: worker is bound to another capacity slot", worker.ErrCapacitySlotConflict)
		}
		if slot.State != worker.CapacitySlotBound {
			slot, err = r.capacitySlots.RepairBinding(ctx, slot.ID, existing.Project.ID, existing.Assignment.ID)
			if err != nil {
				return worker.CapacitySlot{}, provisionerAssignmentRecord{}, false, err
			}
		}
		return slot, existing, true, nil
	}
	if slot.State == worker.CapacitySlotBound {
		return slot, provisionerAssignmentRecord{}, false, fmt.Errorf("%w: bound capacity slot has no project assignment", worker.ErrCapacitySlotConflict)
	}
	if slot.State != worker.CapacitySlotReady {
		return slot, provisionerAssignmentRecord{}, false, nil
	}
	actual, err := r.directory.GetWorker(ctx, slot.WorkerID)
	if err != nil {
		return slot, provisionerAssignmentRecord{}, false, nil
	}
	now := time.Now().UTC()
	if actual.ExpiresAt != nil && !actual.ExpiresAt.After(now) {
		return slot, provisionerAssignmentRecord{}, false, nil
	}
	if actual.LastHeartbeatAt == nil || slot.CapabilityCheckedAt == nil || now.Sub(*slot.CapabilityCheckedAt) > capabilityTTL {
		return slot, provisionerAssignmentRecord{}, false, nil
	}
	if err := worker.WorkerSatisfiesSlot(slot, actual); err != nil {
		slot, _ = r.capacitySlots.RecordCapabilities(ctx, slot.ID, err)
		return slot, provisionerAssignmentRecord{}, false, nil
	}

	filter := worker.AssignmentCandidateFilter{
		ProfileLabels: slot.ProfileLabels, ProfileTaints: slot.ProfileTaints,
		ProfileHarnessModels: slot.ProfileHarnessModels, AllowedRoles: slot.AllowedRoles,
		AllowedBuckets: slot.AllowedBuckets, RequiredSelector: slot.RequiredSelector,
	}
	var selected *provisionerAssignmentRecord
	var selectedJob worker.Job
	for _, bundle := range r.All() {
		job, ok, findErr := bundle.Queue.FindAssignmentCandidateForWorker(ctx, filter, &actual)
		if findErr != nil {
			return slot, provisionerAssignmentRecord{}, false, findErr
		}
		if !ok {
			continue
		}
		record := provisionerAssignmentRecord{Project: bundle.Project, bundle: bundle}
		if selected == nil || job.Priority > selectedJob.Priority ||
			(job.Priority == selectedJob.Priority && (job.CreatedAt.Before(selectedJob.CreatedAt) ||
				(job.CreatedAt.Equal(selectedJob.CreatedAt) && bundle.Project.ID < selected.Project.ID))) {
			selected, selectedJob = &record, job
		}
	}
	if selected == nil {
		return slot, provisionerAssignmentRecord{}, false, nil
	}
	assignment, err := selected.bundle.Queue.ReserveAssignment(ctx, worker.ReserveAssignmentInput{
		CapacitySlotID: slot.ID, WorkerID: slot.WorkerID, JobID: selectedJob.ID,
		ProviderID: slot.ProviderID, ProfileName: slot.ProfileName,
		ProviderRequestID: slot.ProviderRequestID, ProviderType: slot.ProviderType,
		ProviderOptions: slot.ProviderOptions, ProfileLabels: slot.ProfileLabels,
		ProfileTaints: slot.ProfileTaints, ProfileHarnessModels: slot.ProfileHarnessModels,
		AllowedRoles: slot.AllowedRoles, AllowedBuckets: slot.AllowedBuckets,
		RequiredSelector: slot.RequiredSelector, StartupDeadline: now.Add(max(capabilityTTL, 2*time.Minute)),
	})
	if err != nil {
		return slot, provisionerAssignmentRecord{}, false, err
	}
	selected.Assignment = assignment
	slot, err = r.capacitySlots.Bind(ctx, slot.ID, selected.Project.ID, assignment.ID)
	if err != nil {
		// The assignment is already durable. A later bind call repairs this exact
		// worker-id association before considering any new queue candidate.
		return slot, *selected, false, err
	}
	return slot, *selected, true, nil
}

func (r *Registry) RecordProvisionerCapacitySlotAttempt(ctx context.Context, id, providerError string, nextRetryAt *time.Time) (worker.CapacitySlot, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	return r.capacitySlots.RecordAttempt(ctx, id, providerError, nextRetryAt)
}

func (r *Registry) CloseProvisionerCapacitySlot(ctx context.Context, id, reason, providerError string) (worker.CapacitySlot, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	slot, err := r.capacitySlots.Get(ctx, id)
	if err != nil {
		return worker.CapacitySlot{}, err
	}
	if slot.State == worker.CapacitySlotBound && slot.AssignmentID != nil {
		record, findErr := r.findAssignmentByIDLocked(ctx, *slot.AssignmentID)
		if findErr == nil && record.Assignment.State != worker.AssignmentClosed {
			return worker.CapacitySlot{}, fmt.Errorf("%w: bound capacity slot has an open assignment", worker.ErrCapacitySlotConflict)
		}
		if findErr != nil && !errors.Is(findErr, sql.ErrNoRows) {
			return worker.CapacitySlot{}, fmt.Errorf("load bound capacity slot assignment: %w", findErr)
		}
	}
	return r.capacitySlots.Close(ctx, id, reason, providerError)
}

func (r *Registry) RevokeProvisionerCapacitySlotCredentials(ctx context.Context, id string) (worker.CapacitySlot, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	slot, err := r.capacitySlots.Get(ctx, id)
	if err != nil {
		return worker.CapacitySlot{}, err
	}
	if slot.State != worker.CapacitySlotClosed {
		return worker.CapacitySlot{}, fmt.Errorf("%w: open capacity slot credentials cannot be revoked", worker.ErrCapacitySlotConflict)
	}
	if slot.CredentialsRevokedAt == nil {
		if err := r.credentials.RevokeSubjectCredentials(ctx, coordinator.TokenScopeWorker, slot.WorkerID); err != nil {
			return worker.CapacitySlot{}, err
		}
		slot, err = r.capacitySlots.MarkCredentialsRevoked(ctx, id)
	}
	return slot, err
}

func (r *Registry) CleanProvisionerCapacitySlot(ctx context.Context, id string) (worker.CapacitySlot, error) {
	slot, err := r.RevokeProvisionerCapacitySlotCredentials(ctx, id)
	if err != nil {
		return worker.CapacitySlot{}, err
	}
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if err := r.directory.DeleteWorker(ctx, slot.WorkerID); err != nil {
		return worker.CapacitySlot{}, err
	}
	return r.capacitySlots.MarkCleaned(ctx, id)
}
