package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const (
	provisionerCapacitySlotsPath  = "/v2/provisioner/capacity-slots"
	provisionerCapacityDemandPath = "/v2/provisioner/capacity-demand"
)

func (s *Server) handleProvisionerCapacityPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if principal.Scope != coordinator.TokenScopeOwner && principal.Scope != coordinator.TokenScopeProvisioner {
		writeError(w, http.StatusForbidden, "forbidden", "owner or provider-bound orchestrator token is required")
		return
	}
	if r.URL.Path == provisionerCapacityDemandPath {
		if requireMethod(w, r, http.MethodPost) {
			s.handleProvisionerCapacityDemand(w, r, principal)
		}
		return
	}
	if r.URL.Path == provisionerCapacitySlotsPath {
		switch r.Method {
		case http.MethodGet:
			s.handleListProvisionerCapacitySlots(w, r, principal)
		case http.MethodPost:
			s.handleCreateProvisionerCapacitySlot(w, r, principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, provisionerCapacitySlotsPath+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	slot, err := s.registry.CapacitySlots().Get(r.Context(), parts[0])
	if err != nil {
		writeCapacitySlotError(w, "capacity_slot_not_found", err)
		return
	}
	if !requireProvisionerProvider(w, principal, slot.ProviderID) {
		return
	}
	switch parts[1] {
	case "bind":
		s.handleBindProvisionerCapacitySlot(w, r, slot.ID)
	case "attempt":
		s.handleRecordProvisionerCapacitySlotAttempt(w, r, slot.ID)
	case "close":
		s.handleCloseProvisionerCapacitySlot(w, r, slot.ID)
	case "revoked":
		s.handleRevokeProvisionerCapacitySlot(w, r, slot.ID)
	case "cleaned":
		s.handleCleanProvisionerCapacitySlot(w, r, slot.ID)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *Server) handleCreateProvisionerCapacitySlot(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.CreateProvisionerCapacitySlotRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !requireProvisionerProvider(w, principal, request.ProviderID) {
		return
	}
	if request.StartupTimeoutSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_startup_timeout", "startup_timeout_seconds must be positive")
		return
	}
	slot, token, created, err := s.registry.CreateProvisionerCapacitySlot(r.Context(), createCapacitySlotInput{
		MaxInstances: request.MaxInstances,
		Slot: worker.CreateCapacitySlotInput{
			ProviderID: request.ProviderID, ProviderRequestID: request.ProviderRequestID,
			ProfileName: request.ProfileName, ProviderType: request.ProviderType,
			ProviderOptions: request.ProviderOptions, ProfileLabels: request.Labels,
			ProfileTaints: request.Taints, ProfileHarnessModels: request.HarnessModels,
			AllowedRoles: request.AllowedRoles, AllowedBuckets: request.AllowedBuckets,
			RequiredSelector: request.RequiredSelector,
			StartupDeadline:  time.Now().UTC().Add(time.Duration(request.StartupTimeoutSeconds) * time.Second),
		},
	})
	if err != nil {
		writeCapacitySlotError(w, "create_capacity_slot_failed", err)
		return
	}
	response := contract.CreateProvisionerCapacitySlotResponse{Created: created}
	if created {
		response.Slot, response.WorkerToken = &slot, token
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleListProvisionerCapacitySlots(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	query := r.URL.Query()
	providerID := strings.TrimSpace(query.Get("provider_id"))
	filter := worker.CapacitySlotFilter{ProviderID: providerID, ProfileName: query.Get("profile_name"), WorkerID: query.Get("worker_id")}
	if providerID != "" {
		if !requireProvisionerProvider(w, principal, providerID) {
			return
		}
	} else if principal.Scope == coordinator.TokenScopeProvisioner {
		filter.ProviderIDs = authorizedProvisionerProviderIDs(principal)
	}
	var err error
	if filter.OpenOnly, err = queryBool(query.Get("open_only")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if filter.NeedsCleanup, err = queryBool(query.Get("needs_cleanup")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	for _, raw := range query["state"] {
		for _, value := range strings.Split(raw, ",") {
			if value = strings.TrimSpace(value); value != "" {
				filter.States = append(filter.States, worker.CapacitySlotState(value))
			}
		}
	}
	slots, err := s.registry.ListProvisionerCapacitySlots(r.Context(), filter)
	if err != nil {
		writeCapacitySlotError(w, "list_capacity_slots_failed", err)
		return
	}
	if slots == nil {
		slots = []worker.CapacitySlot{}
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacitySlotsResponse{Slots: slots})
}

func (s *Server) handleProvisionerCapacityDemand(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.ProvisionerCapacityDemandRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !requireProvisionerProvider(w, principal, request.ProviderID) {
		return
	}
	active, queued, desired, err := s.registry.ProvisionerCapacityDemand(r.Context(), request.ProviderID, request.ProfileName, request.MaxConcurrency, request.IdleCapacity, worker.AssignmentCandidateFilter{
		ProfileLabels: request.Labels, ProfileTaints: request.Taints,
		ProfileHarnessModels: request.HarnessModels, AllowedRoles: request.AllowedRoles,
		AllowedBuckets: request.AllowedBuckets, RequiredSelector: request.RequiredSelector,
	})
	if err != nil {
		writeCapacitySlotError(w, "capacity_demand_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacityDemandResponse{ActiveAssignments: active, EligibleQueuedJobs: queued, DesiredInstances: desired})
}

func (s *Server) handleBindProvisionerCapacitySlot(w http.ResponseWriter, r *http.Request, slotID string) {
	var request contract.BindProvisionerCapacitySlotRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if request.CapabilityTTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_capability_ttl", "capability_ttl_seconds must be non-negative")
		return
	}
	slot, record, bound, err := s.registry.BindProvisionerCapacitySlot(r.Context(), slotID, time.Duration(request.CapabilityTTLSeconds)*time.Second)
	if err != nil {
		writeCapacitySlotError(w, "bind_capacity_slot_failed", err)
		return
	}
	response := contract.BindProvisionerCapacitySlotResponse{Bound: bound, Slot: slot}
	if bound {
		value := provisionerAssignmentContract(record)
		response.Assignment = &value
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRecordProvisionerCapacitySlotAttempt(w http.ResponseWriter, r *http.Request, slotID string) {
	var request contract.RecordProvisionerCapacitySlotAttemptRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	slot, err := s.registry.RecordProvisionerCapacitySlotAttempt(r.Context(), slotID, request.ProviderError, request.NextRetryAt)
	if err != nil {
		writeCapacitySlotError(w, "record_capacity_slot_attempt_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacitySlotResponse{Slot: slot})
}

func (s *Server) handleCloseProvisionerCapacitySlot(w http.ResponseWriter, r *http.Request, slotID string) {
	var request contract.CloseProvisionerCapacitySlotRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		request.Reason = "orchestrator_closed"
	}
	slot, err := s.registry.CloseProvisionerCapacitySlot(r.Context(), slotID, request.Reason, request.ProviderError)
	if err != nil {
		writeCapacitySlotError(w, "close_capacity_slot_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacitySlotResponse{Slot: slot})
}

func (s *Server) handleRevokeProvisionerCapacitySlot(w http.ResponseWriter, r *http.Request, slotID string) {
	slot, err := s.registry.RevokeProvisionerCapacitySlotCredentials(r.Context(), slotID)
	if err != nil {
		writeCapacitySlotError(w, "revoke_capacity_slot_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacitySlotResponse{Slot: slot})
}

func (s *Server) handleCleanProvisionerCapacitySlot(w http.ResponseWriter, r *http.Request, slotID string) {
	slot, err := s.registry.CleanProvisionerCapacitySlot(r.Context(), slotID)
	if err != nil {
		writeCapacitySlotError(w, "clean_capacity_slot_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerCapacitySlotResponse{Slot: slot})
}

func writeCapacitySlotError(w http.ResponseWriter, code string, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "capacity_slot_not_found", "capacity slot not found")
	case errors.Is(err, worker.ErrCapacitySlotConflict), errors.Is(err, worker.ErrAssignmentConflict):
		writeError(w, http.StatusConflict, code, err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}
