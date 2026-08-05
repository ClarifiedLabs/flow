package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// reconcileCapacity performs recovery, capability gating, binding, demand
// calculation, and provisioning in that order for one-shot capacity slots.
func (r *Reconciler) reconcileCapacity(ctx context.Context, coordinator CapacityCoordinator) error {
	slots, err := coordinator.ListCapacitySlots(ctx, r.providerIDs)
	if err != nil {
		return fmt.Errorf("list durable capacity slots: %w", err)
	}
	assignments, err := r.coordinator.ListAssignments(ctx, r.providerIDs)
	if err != nil {
		return fmt.Errorf("list durable assignments for capacity slots: %w", err)
	}
	r.updateActiveMetrics(assignments)
	assignmentByID := make(map[string]contract.ProvisionerAssignment, len(assignments))
	for _, value := range assignments {
		assignmentByID[value.Assignment.ID] = value
	}

	var cycleErrors []error
	providerBlocked := make(map[string]bool)
	for _, profile := range r.profiles {
		if err := r.providers[profile.Provider].Health(ctx, profile); err != nil {
			key := profileKey(profile.ProviderID, profile.ProfileName)
			providerBlocked[key] = true
			cycleErrors = append(cycleErrors, fmt.Errorf("check provider for profile %s/%s: %w", profile.ProviderID, profile.ProfileName, err))
		}
	}

	// Close bound slots only after their project-local assignment is closed.
	for i := range slots {
		slot := slots[i]
		if slot.State != worker.CapacitySlotBound || slot.AssignmentID == nil {
			continue
		}
		assignment, found := assignmentByID[*slot.AssignmentID]
		if !found || assignment.Assignment.State != worker.AssignmentClosed {
			continue
		}
		closed, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "assignment_closed"})
		if closeErr != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("close completed slot %s: %w", slot.ID, closeErr))
			continue
		}
		slots[i] = closed
	}
	// Collapse any failed bulk generation to one durable retrying probe per
	// profile before provider recovery.
	unreadyProbeSeen := make(map[string]bool)
	capabilityDegraded := make(map[string]bool)
	for i := range slots {
		slot := slots[i]
		if slot.State != worker.CapacitySlotUnready {
			continue
		}
		key := profileKey(slot.ProviderID, slot.ProfileName)
		capabilityDegraded[key] = true
		if !unreadyProbeSeen[key] {
			unreadyProbeSeen[key] = true
			continue
		}
		closed, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "surplus_capability_probe", ProviderError: slot.CapabilityError})
		if closeErr != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("close surplus capability probe %s: %w", slot.ID, closeErr))
			continue
		}
		slots[i] = closed
	}

	for i := range slots {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot := slots[i]
		profile, provider, approved, providerErr := r.capacityRecoveryProvider(slot)
		if providerErr != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("recover capacity slot %s: %w", slot.ID, providerErr))
			continue
		}
		key := profileKey(slot.ProviderID, slot.ProfileName)
		if slot.State == worker.CapacitySlotClosed {
			if slot.CleanedAt != nil {
				continue
			}
			if slot.CredentialsRevokedAt == nil {
				if _, err := coordinator.RevokeCapacitySlot(ctx, slot.ID); err != nil {
					cycleErrors = append(cycleErrors, fmt.Errorf("revoke slot %s: %w", slot.ID, err))
					continue
				}
			}
			if err := provider.Delete(ctx, IdentityOfSlot(slot)); err != nil {
				cycleErrors = append(cycleErrors, fmt.Errorf("delete slot %s: %w", slot.ID, err))
				continue
			}
			if _, err := coordinator.CleanCapacitySlot(ctx, slot.ID); err != nil {
				cycleErrors = append(cycleErrors, fmt.Errorf("clean slot %s: %w", slot.ID, err))
				continue
			}
			if slot.AssignmentID != nil {
				if _, err := r.coordinator.Revoke(ctx, *slot.AssignmentID); err != nil {
					cycleErrors = append(cycleErrors, fmt.Errorf("revoke assignment %s after slot cleanup: %w", *slot.AssignmentID, err))
					continue
				}
				if _, err := r.coordinator.Cleaned(ctx, *slot.AssignmentID); err != nil {
					cycleErrors = append(cycleErrors, fmt.Errorf("clean assignment %s after slot cleanup: %w", *slot.AssignmentID, err))
				}
			}
			continue
		}
		if providerBlocked[key] {
			continue
		}
		if slot.State == worker.CapacitySlotBound {
			continue
		}
		if !approved {
			closed, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "profile_descriptor_unavailable"})
			if closeErr != nil {
				cycleErrors = append(cycleErrors, fmt.Errorf("close unapproved capacity slot %s: %w", slot.ID, closeErr))
			} else {
				slots[i] = closed
			}
			continue
		}
		if slot.State == worker.CapacitySlotProvisioning && !r.now().Before(slot.StartupDeadline) {
			closed, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "startup_timeout"})
			if closeErr != nil {
				cycleErrors = append(cycleErrors, closeErr)
			} else {
				slots[i] = closed
			}
			continue
		}
		status, inspectErr := provider.Inspect(ctx, IdentityOfSlot(slot))
		if inspectErr != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("inspect slot %s: %w", slot.ID, inspectErr))
			continue
		}
		if slot.State == worker.CapacitySlotUnready {
			// A failed capability promise opens the profile circuit. Keep this
			// durable slot as the sole retrying probe and surface readiness 503.
			cycleErrors = append(cycleErrors, fmt.Errorf("profile %s/%s capability probe failed: %s", slot.ProviderID, slot.ProfileName, slot.CapabilityError))
			if slot.NextRetryAt != nil && r.now().Before(*slot.NextRetryAt) {
				continue
			}
			if status != ProviderNotFound {
				if err := provider.Delete(ctx, IdentityOfSlot(slot)); err != nil {
					continue
				}
				next := r.now().Add(r.retryDelay(slot.RetryCount))
				_, _ = coordinator.RecordCapacitySlotAttempt(ctx, slot.ID, contract.RecordProvisionerCapacitySlotAttemptRequest{ProviderError: slot.CapabilityError, NextRetryAt: &next})
				continue
			}
			if !approved {
				continue
			}
			if _, launchErr := r.launchCapacitySlot(ctx, coordinator, provider, profile, slot); launchErr != nil {
				cycleErrors = append(cycleErrors, launchErr)
			}
			continue
		}
		switch status {
		case ProviderPending, ProviderRunning:
			// Registration moves provisioning -> ready. Ready workers may bind
			// while their one-shot process remains blocked in the claim loop.
			if slot.State == worker.CapacitySlotReady {
				response, bindErr := coordinator.BindCapacitySlot(ctx, slot.ID, contract.BindProvisionerCapacitySlotRequest{CapabilityTTLSeconds: max(1, int(r.pollInterval*3/time.Second))})
				if bindErr != nil {
					cycleErrors = append(cycleErrors, fmt.Errorf("bind ready slot %s: %w", slot.ID, bindErr))
				} else {
					slots[i] = response.Slot
				}
			}
		case ProviderNotFound, ProviderIncomplete:
			if slot.NextRetryAt != nil && r.now().Before(*slot.NextRetryAt) {
				continue
			}
			if status == ProviderIncomplete {
				_ = provider.Delete(ctx, IdentityOfSlot(slot))
			}
			if _, launchErr := r.launchCapacitySlot(ctx, coordinator, provider, profile, slot); launchErr != nil {
				cycleErrors = append(cycleErrors, launchErr)
			}
		case ProviderSucceeded, ProviderFailed:
			closed, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "worker_exited_before_claim", ProviderError: string(status)})
			if closeErr != nil {
				cycleErrors = append(cycleErrors, closeErr)
			} else {
				slots[i] = closed
			}
		default:
			cycleErrors = append(cycleErrors, fmt.Errorf("slot %s returned unknown provider status %q", slot.ID, status))
		}
	}

	// Re-list after recovery/binding so target arithmetic includes every state
	// transition committed above.
	slots, err = coordinator.ListCapacitySlots(ctx, r.providerIDs)
	if err != nil {
		return errors.Join(append(cycleErrors, err)...)
	}
	for _, profile := range r.profiles {
		key := profileKey(profile.ProviderID, profile.ProfileName)
		demand, demandErr := coordinator.CapacityDemand(ctx, profile.capacityDemandRequest())
		if demandErr != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("load capacity demand for %s/%s: %w", profile.ProviderID, profile.ProfileName, demandErr))
			continue
		}
		profileSlots := slotsForProfile(slots, profile)
		open := 0
		healthy := false
		for _, slot := range profileSlots {
			if slot.State != worker.CapacitySlotClosed {
				open++
			}
			if slot.State == worker.CapacitySlotReady || slot.State == worker.CapacitySlotBound {
				healthy = true
			}
		}
		if providerBlocked[key] || capabilityDegraded[key] {
			continue
		}
		// Unknown profiles launch exactly one probe. Once a runtime report has
		// proved the profile, the remaining desired slots may launch in parallel.
		target := demand.DesiredInstances
		if !healthy && target > 1 {
			target = 1
		}
		for open < target {
			requestID, idErr := r.newRequestID()
			if idErr != nil {
				cycleErrors = append(cycleErrors, idErr)
				break
			}
			response, createErr := coordinator.CreateCapacitySlot(ctx, profile.capacitySlotRequest(requestID))
			if createErr != nil {
				cycleErrors = append(cycleErrors, fmt.Errorf("create capacity slot for %s/%s: %w", profile.ProviderID, profile.ProfileName, createErr))
				break
			}
			if !response.Created || response.Slot == nil {
				break
			}
			open++
			if _, launchErr := r.launchCapacitySlotResponse(ctx, coordinator, r.providers[profile.Provider], profile, *response.Slot, response.WorkerToken); launchErr != nil {
				cycleErrors = append(cycleErrors, launchErr)
				break
			}
		}
		// Drain only surplus unbound slots; a bound one-shot process remains
		// owned by its assignment until terminal cleanup.
		if open > demand.DesiredInstances {
			unbound := make([]worker.CapacitySlot, 0)
			for _, slot := range profileSlots {
				if slot.State != worker.CapacitySlotClosed && slot.State != worker.CapacitySlotBound {
					unbound = append(unbound, slot)
				}
			}
			sort.Slice(unbound, func(i, j int) bool { return unbound[i].CreatedAt.After(unbound[j].CreatedAt) })
			for _, slot := range unbound {
				if open <= demand.DesiredInstances {
					break
				}
				if _, closeErr := coordinator.CloseCapacitySlot(ctx, slot.ID, contract.CloseProvisionerCapacitySlotRequest{Reason: "surplus_capacity"}); closeErr == nil {
					open--
				} else {
					cycleErrors = append(cycleErrors, closeErr)
				}
			}
		}
	}
	return errors.Join(cycleErrors...)
}

func (r *Reconciler) launchCapacitySlot(ctx context.Context, coordinator CapacityCoordinator, provider Provider, profile Profile, slot worker.CapacitySlot) (bool, error) {
	response, err := coordinator.CreateCapacitySlot(ctx, profile.capacitySlotRequest(slot.ProviderRequestID))
	if err != nil {
		return true, fmt.Errorf("refresh capacity slot %s token: %w", slot.ID, err)
	}
	if !response.Created || response.Slot == nil || response.Slot.ID != slot.ID || strings.TrimSpace(response.WorkerToken) == "" {
		return true, fmt.Errorf("refresh capacity slot %s returned inconsistent identity or credential", slot.ID)
	}
	return r.launchCapacitySlotResponse(ctx, coordinator, provider, profile, *response.Slot, response.WorkerToken)
}

func (r *Reconciler) launchCapacitySlotResponse(ctx context.Context, coordinator CapacityCoordinator, provider Provider, profile Profile, slot worker.CapacitySlot, token string) (bool, error) {
	if !profileMatchesSlot(profile, slot) {
		return false, fmt.Errorf("capacity slot %s does not match configured profile", slot.ID)
	}
	err := provider.Launch(ctx, LaunchRequest{Identity: IdentityOfSlot(slot), Slot: &slot, Profile: profile, CoordinatorURL: r.coordinatorURL, WorkerToken: token})
	if err != nil {
		next := r.now().Add(r.retryDelay(slot.RetryCount))
		_, recordErr := coordinator.RecordCapacitySlotAttempt(ctx, slot.ID, contract.RecordProvisionerCapacitySlotAttemptRequest{ProviderError: err.Error(), NextRetryAt: &next})
		return true, errors.Join(fmt.Errorf("launch capacity slot %s: %w", slot.ID, err), recordErr)
	}
	_, err = coordinator.RecordCapacitySlotAttempt(ctx, slot.ID, contract.RecordProvisionerCapacitySlotAttemptRequest{})
	return true, err
}

func (r *Reconciler) capacityRecoveryProvider(slot worker.CapacitySlot) (Profile, Provider, bool, error) {
	profile := Profile{ProviderID: slot.ProviderID, ProfileName: slot.ProfileName, ProviderType: slot.ProviderType, ProviderOptions: slot.ProviderOptions, Labels: slot.ProfileLabels, Taints: slot.ProfileTaints, HarnessModels: slot.ProfileHarnessModels, AllowedRoles: slot.AllowedRoles, AllowedBuckets: slot.AllowedBuckets, RequiredSelector: slot.RequiredSelector}
	if configured, ok := r.profileByID[profileKey(slot.ProviderID, slot.ProfileName)]; ok && profileMatchesSlot(configured, slot) {
		return configured, r.providers[configured.Provider], true, nil
	}
	if r.providerFactory == nil {
		return Profile{}, nil, false, fmt.Errorf("profile descriptor is not configured and recovery is unavailable")
	}
	cacheKey := "slot-recovery:" + slot.ProviderType + ":" + slot.ProviderID + "/" + slot.ProfileName
	if provider := r.providers[cacheKey]; provider != nil {
		profile.Provider = cacheKey
		return profile, provider, false, nil
	}
	provider, err := r.providerFactory(profile)
	if err != nil {
		return Profile{}, nil, false, err
	}
	profile.Provider = cacheKey
	r.providers[cacheKey] = provider
	return profile, provider, false, nil
}

func profileMatchesSlot(profile Profile, slot worker.CapacitySlot) bool {
	return profile.ProviderID == slot.ProviderID && profile.ProfileName == slot.ProfileName && profile.ProviderType == slot.ProviderType && equalStringMaps(profile.ProviderOptions, slot.ProviderOptions) && equalStringMaps(profile.Labels, slot.ProfileLabels) && equalEmptySlices(profile.Taints, slot.ProfileTaints) && equalHarnessModels(profile.HarnessModels, slot.ProfileHarnessModels) && equalEmptySlices(profile.AllowedRoles, slot.AllowedRoles) && equalEmptySlices(profile.AllowedBuckets, slot.AllowedBuckets) && equalStringMaps(profile.RequiredSelector, slot.RequiredSelector)
}

func slotsForProfile(slots []worker.CapacitySlot, profile Profile) []worker.CapacitySlot {
	var result []worker.CapacitySlot
	for _, slot := range slots {
		if slot.ProviderID == profile.ProviderID && slot.ProfileName == profile.ProfileName {
			result = append(result, slot)
		}
	}
	return result
}
