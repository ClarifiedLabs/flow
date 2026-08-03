package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const provisionerAssignmentsPath = "/v2/provisioner/assignments"

func (s *Server) handleProvisionerAssignmentsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if principal.Scope != coordinator.TokenScopeOwner && principal.Scope != coordinator.TokenScopeProvisioner {
		writeError(w, http.StatusForbidden, "forbidden", "owner or provider-bound orchestrator token is required")
		return
	}
	if r.URL.Path == provisionerAssignmentsPath {
		switch r.Method {
		case http.MethodGet:
			s.handleListProvisionerAssignments(w, r, principal)
		case http.MethodPost:
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if r.URL.Path == provisionerAssignmentsPath+"/reserve" {
		if requireMethod(w, r, http.MethodPost) {
			s.handleReserveProvisionerAssignment(w, r, principal)
		}
		return
	}

	remainder := strings.TrimPrefix(r.URL.Path, provisionerAssignmentsPath+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.authorizeProvisionerAssignment(w, r, principal, parts[0]) {
		return
	}
	switch parts[1] {
	case "abandon":
		s.handleAbandonProvisionerAssignment(w, r, parts[0])
	case "attempt":
		s.handleRecordProvisionerAssignmentAttempt(w, r, parts[0])
	case "revoked":
		s.handleRevokeProvisionerAssignment(w, r, parts[0])
	case "cleaned":
		s.handleCleanProvisionerAssignment(w, r, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *Server) handleReserveProvisionerAssignment(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.ReserveProvisionerAssignmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !requireProvisionerProvider(w, principal, request.ProviderID) {
		return
	}
	waitDuration, err := claimWaitDuration(request.WaitSeconds)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_wait", err.Error())
		return
	}
	if request.StartupTimeoutSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_startup_timeout", "startup_timeout_seconds must be positive")
		return
	}

	input := reserveProvisionerAssignmentInput{
		ProviderID: request.ProviderID, ProviderRequestID: request.ProviderRequestID,
		ProfileName: request.ProfileName, ProviderType: request.ProviderType,
		ProviderOptions: request.ProviderOptions, MaxConcurrency: request.MaxConcurrency,
		StartupTimeout: time.Duration(request.StartupTimeoutSeconds) * time.Second,
		Candidate: worker.AssignmentCandidateFilter{
			ProfileLabels: request.Labels, ProfileTaints: request.Taints,
			ProfileHarnessModels: request.HarnessModels, AllowedRoles: request.AllowedRoles,
			AllowedBuckets: request.AllowedBuckets, RequiredSelector: request.RequiredSelector,
		},
	}
	deadline := time.Now().UTC().Add(waitDuration)
	for {
		record, token, reserved, err := s.registry.ReserveProvisionerAssignment(r.Context(), input)
		if err != nil {
			writeProvisionerAssignmentError(w, "reserve_assignment_failed", err)
			return
		}
		if reserved {
			response := provisionerAssignmentContract(record)
			writeJSON(w, http.StatusOK, contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &response, WorkerToken: token})
			return
		}
		if waitDuration <= 0 || time.Until(deadline) <= 0 {
			writeJSON(w, http.StatusOK, contract.ReserveProvisionerAssignmentResponse{Reserved: false})
			return
		}
		remaining := time.Until(deadline)
		sleep := 250 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) handleListProvisionerAssignments(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	query := r.URL.Query()
	providerID := strings.TrimSpace(query.Get("provider_id"))
	var providerIDs []string
	if providerID != "" {
		if !requireProvisionerProvider(w, principal, providerID) {
			return
		}
	} else if principal.Scope == coordinator.TokenScopeProvisioner {
		providerIDs = authorizedProvisionerProviderIDs(principal)
		if len(providerIDs) == 0 {
			writeError(w, http.StatusForbidden, "forbidden", "provider-bound orchestrator token has no authorized providers")
			return
		}
	}
	filter := worker.AssignmentFilter{
		ProviderID: providerID, ProfileName: query.Get("profile_name"),
		WorkerID: query.Get("worker_id"), JobID: query.Get("job_id"),
	}
	for _, raw := range query["state"] {
		for _, state := range strings.Split(raw, ",") {
			if state = strings.TrimSpace(state); state != "" {
				filter.States = append(filter.States, worker.AssignmentState(state))
			}
		}
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
	records, err := s.registry.ListProvisionerAssignments(r.Context(), listProvisionerAssignmentsInput{
		ProjectID: query.Get("project_id"), ProviderRequestID: query.Get("provider_request_id"),
		ProviderIDs: providerIDs, Filter: filter,
	})
	if err != nil {
		writeProvisionerAssignmentError(w, "list_assignments_failed", err)
		return
	}
	response := contract.ProvisionerAssignmentsResponse{Assignments: make([]contract.ProvisionerAssignment, 0, len(records))}
	for _, record := range records {
		response.Assignments = append(response.Assignments, provisionerAssignmentContract(record))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAbandonProvisionerAssignment(w http.ResponseWriter, r *http.Request, assignmentID string) {
	var request contract.AbandonProvisionerAssignmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	record, err := s.registry.AbandonProvisionerAssignment(r.Context(), assignmentID, request.ProviderError)
	if err != nil {
		writeProvisionerAssignmentError(w, "abandon_assignment_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: provisionerAssignmentContract(record)})
}

func (s *Server) handleRecordProvisionerAssignmentAttempt(w http.ResponseWriter, r *http.Request, assignmentID string) {
	var request contract.RecordProvisionerAssignmentAttemptRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	record, err := s.registry.RecordProvisionerAssignmentAttempt(r.Context(), assignmentID, request.ProviderError, request.NextRetryAt)
	if err != nil {
		writeProvisionerAssignmentError(w, "record_assignment_attempt_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: provisionerAssignmentContract(record)})
}

func (s *Server) handleRevokeProvisionerAssignment(w http.ResponseWriter, r *http.Request, assignmentID string) {
	record, err := s.registry.RevokeProvisionerAssignmentCredentials(r.Context(), assignmentID)
	if err != nil {
		writeProvisionerAssignmentError(w, "revoke_assignment_credentials_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: provisionerAssignmentContract(record)})
}

func (s *Server) handleCleanProvisionerAssignment(w http.ResponseWriter, r *http.Request, assignmentID string) {
	record, err := s.registry.CleanProvisionerAssignment(r.Context(), assignmentID)
	if err != nil {
		writeProvisionerAssignmentError(w, "clean_assignment_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: provisionerAssignmentContract(record)})
}

func (s *Server) authorizeProvisionerAssignment(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, assignmentID string) bool {
	if principal.Scope == coordinator.TokenScopeOwner {
		return true
	}
	record, err := s.registry.GetProvisionerAssignment(r.Context(), assignmentID)
	if err != nil {
		writeProvisionerAssignmentError(w, "authorize_assignment_failed", err)
		return false
	}
	return requireProvisionerProvider(w, principal, record.Assignment.ProviderID)
}

func authorizedProvisionerProviderIDs(principal coordinator.Principal) []string {
	if principal.Scope != coordinator.TokenScopeProvisioner {
		return nil
	}
	seen := make(map[string]struct{})
	var providerIDs []string
	for _, raw := range strings.Split(principal.Subject, ",") {
		providerID := strings.TrimSpace(raw)
		if providerID == "" {
			continue
		}
		if _, ok := seen[providerID]; ok {
			continue
		}
		seen[providerID] = struct{}{}
		providerIDs = append(providerIDs, providerID)
	}
	return providerIDs
}

func requireProvisionerProvider(w http.ResponseWriter, principal coordinator.Principal, providerID string) bool {
	if principal.Scope == coordinator.TokenScopeOwner {
		return true
	}
	providerID = strings.TrimSpace(providerID)
	if principal.Scope != coordinator.TokenScopeProvisioner || providerID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "provider-bound orchestrator token does not authorize this provider")
		return false
	}
	for _, allowed := range strings.Split(principal.Subject, ",") {
		if strings.TrimSpace(allowed) == providerID {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "forbidden", "provider-bound orchestrator token does not authorize this provider")
	return false
}

func provisionerAssignmentContract(record provisionerAssignmentRecord) contract.ProvisionerAssignment {
	return contract.ProvisionerAssignment{Project: uiProjectFromRegistry(record.Project), Assignment: record.Assignment}
}

func queryBool(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

func writeProvisionerAssignmentError(w http.ResponseWriter, code string, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "assignment_not_found", "assignment not found")
	case errors.Is(err, worker.ErrAssignmentConflict):
		writeError(w, http.StatusConflict, code, err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}
