package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func (s *projectServer) handleGitEvents(w http.ResponseWriter, r *http.Request) {
	if s.gitEvents == nil {
		writeError(w, http.StatusInternalServerError, "git_events_unavailable", "git event service is not configured")
		return
	}

	var request gitEventsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	events := request.eventItems()
	if len(events) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_git_event", "at least one git event is required")
		return
	}

	response := gitEventsResponse{Events: make([]coordinator.GitEvent, 0, len(events))}
	for _, event := range events {
		result, err := s.gitEvents.Record(r.Context(), coordinator.GitEvent{
			OldSHA:     event.OldSHA,
			NewSHA:     event.NewSHA,
			Ref:        event.Ref,
			Actor:      event.Actor,
			ObservedAt: event.ObservedAt,
		}, coordinator.GitEventSourceAPI)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_git_event", err.Error())
			return
		}
		response.Events = append(response.Events, result.Event)
		response.Recorded++
		if result.Inserted {
			response.Inserted++
		}
	}

	writeJSON(w, http.StatusAccepted, response)
}

// handleDrainGitEventsByExchange resolves the owning project from the
// request's exchange_repo_path and drains that project's hook spool.
func (s *Server) handleDrainGitEventsByExchange(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body_failed", err.Error())
		return
	}
	_ = r.Body.Close()

	var request drainGitEventsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	exchangePath := strings.TrimSpace(request.ExchangeRepoPath)
	if exchangePath == "" {
		writeError(w, http.StatusBadRequest, "invalid_exchange_repo_path", "exchange_repo_path is required")
		return
	}
	project, err := s.registry.Projects().GetByExchangePath(r.Context(), exchangePath)
	if err != nil {
		writeError(w, http.StatusNotFound, "project_not_found", "no project owns this exchange path")
		return
	}
	bundle, ok := s.registry.Bundle(project.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "project_not_found", "project is not open")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	s.forBundle(bundle).handleDrainGitEvents(w, r)
}

func (s *projectServer) handleDrainGitEvents(w http.ResponseWriter, r *http.Request) {
	if s.gitEvents == nil {
		writeError(w, http.StatusInternalServerError, "git_events_unavailable", "git event service is not configured")
		return
	}

	var request drainGitEventsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	request.ExchangeRepoPath = strings.TrimSpace(request.ExchangeRepoPath)
	if request.ExchangeRepoPath == "" {
		writeError(w, http.StatusBadRequest, "invalid_exchange_repo_path", "exchange_repo_path is required")
		return
	}

	drained, err := s.gitEvents.DrainSpooled(r.Context(), request.ExchangeRepoPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "drain_git_events_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, drainGitEventsResponse{Drained: drained})
}
