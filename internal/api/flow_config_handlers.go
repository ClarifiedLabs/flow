package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// Flow configuration endpoints: agent definitions and flows are project-owned
// rows edited live from the web UI / CLI (harness+model availability is a
// server/worker concern, so this configuration deliberately does NOT live in
// the repository the way .flow/checks/*.yaml CI checks do).

type agentDefResponse struct {
	AgentDef coordinator.AgentDef `json:"agent_def"`
}

type agentDefsResponse struct {
	AgentDefs []coordinator.AgentDef `json:"agent_defs"`
}

type flowResponse struct {
	Flow coordinator.Flow `json:"flow"`
}

type flowsResponse struct {
	Flows         []coordinator.Flow `json:"flows"`
	DefaultFlowID string             `json:"default_flow_id,omitempty"`
}

func (s *projectServer) handleAgentDefsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireScope(w, principal, "agent definitions require an owner or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
		return
	}
	if s.agentDefs == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_defs_unavailable", "agent definition service is not configured")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v2/agent-defs")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			defs, err := s.agentDefs.List(r.Context())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "list_agent_defs_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, agentDefsResponse{AgentDefs: defs})
		case http.MethodPost:
			var input coordinator.AgentDefInput
			if err := decodeJSON(r, &input); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
				return
			}
			def, err := s.agentDefs.Create(r.Context(), input)
			if err != nil {
				writeAgentDefError(w, err, "create_agent_def_failed")
				return
			}
			writeJSON(w, http.StatusCreated, agentDefResponse{AgentDef: def})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	id := rest
	switch r.Method {
	case http.MethodGet:
		def, err := s.agentDefs.Get(r.Context(), id)
		if err != nil {
			writeAgentDefError(w, err, "get_agent_def_failed")
			return
		}
		writeJSON(w, http.StatusOK, agentDefResponse{AgentDef: def})
	case http.MethodPatch:
		var input coordinator.AgentDefInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		def, err := s.agentDefs.Update(r.Context(), id, input)
		if err != nil {
			writeAgentDefError(w, err, "update_agent_def_failed")
			return
		}
		writeJSON(w, http.StatusOK, agentDefResponse{AgentDef: def})
	case http.MethodDelete:
		if err := s.agentDefs.Delete(r.Context(), id); err != nil {
			writeAgentDefError(w, err, "delete_agent_def_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	}
}

func (s *projectServer) handleFlowsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireScope(w, principal, "flows require an owner or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
		return
	}
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, "flows_unavailable", "flow service is not configured")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v2/flows")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			flows, err := s.flows.List(r.Context())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "list_flows_failed", err.Error())
				return
			}
			defaultID, err := s.flows.DefaultFlowID(r.Context())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "list_flows_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, flowsResponse{Flows: flows, DefaultFlowID: defaultID})
		case http.MethodPost:
			var input coordinator.FlowInput
			if err := decodeJSON(r, &input); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
				return
			}
			flow, err := s.flows.Create(r.Context(), input)
			if err != nil {
				writeFlowError(w, err, "create_flow_failed")
				return
			}
			writeJSON(w, http.StatusCreated, flowResponse{Flow: flow})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	id, action, _ := strings.Cut(rest, "/")
	if action == "default" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if err := s.flows.SetDefaultFlow(r.Context(), id); err != nil {
			writeFlowError(w, err, "set_default_flow_failed")
			return
		}
		flow, err := s.flows.Get(r.Context(), id)
		if err != nil {
			writeFlowError(w, err, "set_default_flow_failed")
			return
		}
		writeJSON(w, http.StatusOK, flowResponse{Flow: flow})
		return
	}
	if action != "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		flow, err := s.flows.Get(r.Context(), id)
		if err != nil {
			writeFlowError(w, err, "get_flow_failed")
			return
		}
		writeJSON(w, http.StatusOK, flowResponse{Flow: flow})
	case http.MethodPatch:
		var input coordinator.FlowInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		flow, err := s.flows.Update(r.Context(), id, input)
		if err != nil {
			writeFlowError(w, err, "update_flow_failed")
			return
		}
		writeJSON(w, http.StatusOK, flowResponse{Flow: flow})
	case http.MethodDelete:
		if err := s.flows.Delete(r.Context(), id); err != nil {
			writeFlowError(w, err, "delete_flow_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	}
}

func writeAgentDefError(w http.ResponseWriter, err error, code string) {
	switch {
	case errors.Is(err, coordinator.ErrAgentDefNotFound):
		writeError(w, http.StatusNotFound, "agent_def_not_found", err.Error())
	case errors.Is(err, coordinator.ErrAgentDefNameTaken), errors.Is(err, coordinator.ErrAgentDefInUse):
		writeError(w, http.StatusConflict, code, err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}

func writeFlowError(w http.ResponseWriter, err error, code string) {
	switch {
	case errors.Is(err, coordinator.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "flow_not_found", err.Error())
	case errors.Is(err, coordinator.ErrFlowNameTaken), errors.Is(err, coordinator.ErrFlowIsDefault), errors.Is(err, coordinator.ErrFlowInUse):
		writeError(w, http.StatusConflict, code, err.Error())
	case errors.Is(err, coordinator.ErrAgentDefNotFound):
		writeError(w, http.StatusBadRequest, code, err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}
