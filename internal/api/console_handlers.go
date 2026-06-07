package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
)

func (s *projectServer) handleConsole(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
		writeError(w, http.StatusForbidden, "forbidden", "console requires owner or console token")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetConsole(w, r)
	case http.MethodPost:
		s.handleStartConsole(w, r)
	case http.MethodDelete:
		s.handleReleaseConsole(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	}
}

func (s *projectServer) handleGetConsole(w http.ResponseWriter, r *http.Request) {
	if err := s.projectSweepExpiredLeases(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "console_reconcile_failed", err.Error())
		return
	}
	state, err := s.sessions.CurrentConsole(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, "console_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.consoleResponse(state))
}

func (s *projectServer) handleStartConsole(w http.ResponseWriter, r *http.Request) {
	var request consoleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	harness := flowharness.NormalizeName(request.Harness)
	if harness == "" {
		harness = flowharness.DefaultConsoleName()
	}
	if err := flowharness.ValidateConsoleName(harness); err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	result, err := s.sessions.EnsureConsoleJob(r.Context(), coordinator.EnsureConsoleJobInput{
		Base:    s.project.BaseBranch,
		Harness: harness,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	state, err := s.sessions.CurrentConsole(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	if state.Job == nil {
		state.Job = &result.Job
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, s.consoleResponse(state))
}

func (s *projectServer) handleReleaseConsole(w http.ResponseWriter, r *http.Request) {
	state, err := s.sessions.ReleaseConsole(r.Context())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "console_not_found", "console not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "release_console_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.consoleResponse(state))
}

func (s *projectServer) handleIssueConsole(w http.ResponseWriter, r *http.Request, issueID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	if _, err := s.issues.GetIssue(r.Context(), issueID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "issue_not_found", "issue not found")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "get_issue_failed", err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetIssueConsole(w, r, issueID)
	case http.MethodPost:
		s.handleStartIssueConsole(w, r, issueID)
	case http.MethodDelete:
		s.handleReleaseIssueConsole(w, r, issueID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	}
}

func (s *projectServer) handleGetIssueConsole(w http.ResponseWriter, r *http.Request, issueID string) {
	if err := s.projectSweepExpiredLeases(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "console_reconcile_failed", err.Error())
		return
	}
	state, err := s.sessions.CurrentIssueConsole(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "console_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.consoleResponse(state))
}

func (s *projectServer) handleStartIssueConsole(w http.ResponseWriter, r *http.Request, issueID string) {
	var request consoleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	harness := flowharness.NormalizeName(request.Harness)
	if harness == "" {
		harness = flowharness.DefaultConsoleName()
	}
	if err := flowharness.ValidateConsoleName(harness); err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	result, err := s.sessions.EnsureIssueConsoleJob(r.Context(), coordinator.EnsureIssueConsoleJobInput{
		IssueID: issueID,
		Harness: harness,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	state, err := s.sessions.CurrentIssueConsole(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "start_console_failed", err.Error())
		return
	}
	if state.Job == nil {
		state.Job = &result.Job
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, s.consoleResponse(state))
}

func (s *projectServer) handleReleaseIssueConsole(w http.ResponseWriter, r *http.Request, issueID string) {
	state, err := s.sessions.ReleaseIssueConsole(r.Context(), issueID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "console_not_found", "console not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "release_console_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.consoleResponse(state))
}

func (s *projectServer) consoleResponse(state coordinator.ConsoleState) consoleResponse {
	response := consoleResponse{
		Active:            state.Active,
		ProjectID:         s.project.ID,
		ProjectName:       s.project.Name,
		Job:               state.Job,
		Session:           state.Session,
		Terminal:          state.Terminal,
		TerminalAvailable: state.TerminalAvailable,
	}
	return response
}
