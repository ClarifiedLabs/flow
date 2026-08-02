package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// Features are project-child task groups with their own exchange branches.
// Reads are open to every project scope; mutations (create/edit/rebase/land/
// archive) rewrite shared refs or schedule work, so they require an owner or
// console token.

func (s *projectServer) handleFeaturesPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if s.features == nil {
		writeError(w, http.StatusServiceUnavailable, "features_unavailable", "feature service is not configured")
		return
	}
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/features"), "/")
	var parts []string
	if sub != "" {
		parts = strings.Split(sub, "/")
	}

	requireRead := func() bool {
		return requireScope(w, principal, "owner, console, or session token is required",
			coordinator.TokenScopeOwner, coordinator.TokenScopeConsole, coordinator.TokenScopeSession)
	}
	requireWrite := func() bool {
		return requireScope(w, principal, "owner or console token is required",
			coordinator.TokenScopeOwner, coordinator.TokenScopeConsole)
	}

	switch {
	case len(parts) == 0:
		switch r.Method {
		case http.MethodGet:
			if requireRead() {
				s.handleListFeatures(w, r)
			}
		case http.MethodPost:
			if requireWrite() {
				s.handleCreateFeature(w, r, principal)
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			if requireRead() {
				s.handleGetFeature(w, r, parts[0])
			}
		case http.MethodPatch:
			if requireWrite() {
				s.handleEditFeature(w, r, parts[0])
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	case len(parts) == 2 && r.Method == http.MethodPost:
		if !requireWrite() {
			return
		}
		switch parts[1] {
		case "rebase":
			s.handleRebaseFeature(w, r, parts[0], principal)
		case "land":
			s.handleLandFeature(w, r, parts[0], principal)
		case "archive":
			s.handleArchiveFeature(w, r, parts[0])
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown features route")
		}
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown features route")
	}
}

func (s *projectServer) handleListFeatures(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := coordinator.FeatureStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "all" {
		status = ""
	} else if status == "" {
		status = coordinator.FeatureOpen
	}
	features, err := s.features.List(ctx, status)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_status", err.Error())
		return
	}

	response := contract.FeaturesResponse{Features: make([]contract.FeatureResponse, 0, len(features))}
	for _, feature := range features {
		payload, err := s.featureSummary(ctx, feature)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load_feature_failed", err.Error())
			return
		}
		response.Features = append(response.Features, payload)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleCreateFeature(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.CreateFeatureRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	actor := coordinator.ActorHuman
	if principal.Scope == coordinator.TokenScopeSession {
		actor = coordinator.ActorAgent
	}
	feature, err := s.features.Create(r.Context(), coordinator.CreateFeatureInput{
		Title: request.Title, Body: request.Body, CreatedBy: actor,
	})
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	payload, err := s.featureSummary(r.Context(), feature)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_feature_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, payload)
}

func (s *projectServer) handleGetFeature(w http.ResponseWriter, r *http.Request, ref string) {
	ctx := r.Context()
	payload, err := s.featureDetail(ctx, ref)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleEditFeature(w http.ResponseWriter, r *http.Request, ref string) {
	ctx := r.Context()
	feature, err := s.features.Resolve(ctx, ref)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	var request contract.UpdateFeatureRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	edited, err := s.features.Edit(ctx, feature.ID, coordinator.EditFeatureInput{Title: request.Title, Body: request.Body})
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	payload, err := s.featureSummary(ctx, edited)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_feature_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleRebaseFeature(w http.ResponseWriter, r *http.Request, ref string, principal coordinator.Principal) {
	ctx := r.Context()
	feature, err := s.features.Resolve(ctx, ref)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	restrictBlockedTo, ok := s.checkFeatureRebaseScope(w, r, principal, feature)
	if !ok {
		return
	}
	result, err := s.features.RebaseOnMain(ctx, feature, restrictBlockedTo...)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.RebaseFeatureResponse{Result: result})
}

// checkFeatureRebaseScope confines feature rebases for task-bound console
// credentials to features that contain the console's bound task as open work,
// and returns the only task such a credential's conflicted rebase may link as
// a blocker: the bound task itself. The restriction is applied at
// relation-creation time inside RebaseOnMain, so a feature task created
// concurrently after this read can never receive a rebase_task blocks link —
// the relation set is confined by construction rather than by a racy pre-read.
// Unbound project consoles and owner credentials keep project-wide rebase
// access. The caller resolves the feature ref once and passes the value here,
// so the console rebase path performs a single feature lookup.
func (s *projectServer) checkFeatureRebaseScope(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, feature coordinator.Feature) (restrictBlockedTo []string, ok bool) {
	if principal.Scope != coordinator.TokenScopeConsole || principal.SourceTaskID == nil {
		return nil, true
	}
	ctx := r.Context()
	tasks, err := s.features.Tasks(ctx, feature.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_feature_tasks_failed", err.Error())
		return nil, false
	}
	bound := strings.TrimSpace(*principal.SourceTaskID)
	boundOpen := false
	for _, task := range tasks {
		if task.ID == bound && (task.State == nil || *task.State != coordinator.LifecycleDone) {
			boundOpen = true
			break
		}
	}
	if !boundOpen {
		writeError(w, http.StatusForbidden, "forbidden", "console credential cannot rebase a feature that does not contain its bound task as open work")
		return nil, false
	}
	return []string{bound}, true
}

func (s *projectServer) handleLandFeature(w http.ResponseWriter, r *http.Request, ref string, principal coordinator.Principal) {
	actor := coordinator.ActorHuman
	if principal.Scope == coordinator.TokenScopeSession {
		actor = coordinator.ActorAgent
	}
	feature, err := s.features.Land(r.Context(), ref, actor)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	payload, err := s.featureSummary(r.Context(), feature)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_feature_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleArchiveFeature(w http.ResponseWriter, r *http.Request, ref string) {
	feature, err := s.features.Archive(r.Context(), ref)
	if err != nil {
		writeFeatureError(w, err)
		return
	}
	payload, err := s.featureSummary(r.Context(), feature)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_feature_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// featureSummary is the list-view payload: row + counts + live divergence.
func (s *projectServer) featureSummary(ctx context.Context, feature coordinator.Feature) (contract.FeatureResponse, error) {
	payload := contract.FeatureResponse{Feature: feature}
	tasks, err := s.features.Tasks(ctx, feature.ID)
	if err != nil {
		return contract.FeatureResponse{}, err
	}
	payload.Counts = featureTaskCounts(tasks)
	if state, err := s.features.BranchState(ctx, feature.ID); err == nil {
		payload.BranchState = &state
	}
	if running, found, err := s.features.RunningRebase(ctx, feature.ID); err == nil && found {
		payload.RunningRebase = &running
	}
	return payload, nil
}

// featureDetail adds the assigned tasks and the rebase history.
func (s *projectServer) featureDetail(ctx context.Context, ref string) (contract.FeatureResponse, error) {
	feature, err := s.features.Resolve(ctx, ref)
	if err != nil {
		return contract.FeatureResponse{}, err
	}
	payload, err := s.featureSummary(ctx, feature)
	if err != nil {
		return contract.FeatureResponse{}, err
	}
	tasks, err := s.features.Tasks(ctx, feature.ID)
	if err != nil {
		return contract.FeatureResponse{}, err
	}
	payload.Tasks = tasks
	rebases, err := s.features.ListRebases(ctx, feature.ID)
	if err != nil {
		return contract.FeatureResponse{}, err
	}
	payload.Rebases = rebases
	return payload, nil
}

func featureTaskCounts(tasks []coordinator.Task) contract.FeatureTaskCounts {
	var counts contract.FeatureTaskCounts
	for _, task := range tasks {
		switch {
		case task.State == nil:
			counts.Open++
		case *task.State == coordinator.LifecycleDone:
			counts.Done++
		case *task.State == coordinator.LifecycleInProgress:
			counts.InProgress++
		default:
			counts.Scheduled++
		}
	}
	return counts
}

func writeFeatureError(w http.ResponseWriter, err error) {
	var active *coordinator.ErrFeatureActive
	switch {
	case errors.Is(err, coordinator.ErrFeatureNotFound):
		writeError(w, http.StatusNotFound, "feature_not_found", err.Error())
	case errors.Is(err, coordinator.ErrFeatureTitleTaken):
		writeError(w, http.StatusConflict, "feature_title_taken", err.Error())
	case errors.Is(err, coordinator.ErrFeatureClosed):
		writeError(w, http.StatusConflict, "feature_closed", err.Error())
	case errors.Is(err, coordinator.ErrFeatureRebaseRunning):
		writeError(w, http.StatusConflict, "rebase_running", err.Error())
	case errors.As(err, &active):
		writeError(w, http.StatusConflict, "feature_has_active_tasks", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "feature_error", err.Error())
	}
}
