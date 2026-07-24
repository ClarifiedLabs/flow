package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

type workflowRunResponse struct {
	Run coordinator.WorkflowRun `json:"run"`
}

type workflowDetailResponse struct {
	Detail    coordinator.WorkflowRunDetail  `json:"detail"`
	Artifacts []coordinator.WorkflowArtifact `json:"artifacts"`
}

type workflowRespondRequest struct {
	NodeRunID string `json:"node_run_id"`
	Outcome   string `json:"outcome"`
	Feedback  string `json:"feedback,omitempty"`
}

type workflowBudgetRequest struct {
	Additional int `json:"additional"`
}

type workflowRetryRequest struct {
	RefreshAgentRuntime bool `json:"refresh_agent_runtime"`
}

type workflowDoneRequest struct {
	Resolution coordinator.DoneResolution `json:"resolution"`
	Note       string                     `json:"note,omitempty"`
}

type workflowCompleteRequest struct {
	NodeRunID  string `json:"node_run_id"`
	ArtifactID string `json:"artifact_id"`
}

type workflowArtifactResponse struct {
	Artifact coordinator.WorkflowArtifact `json:"artifact"`
	Replayed bool                         `json:"replayed"`
}

func (s *projectServer) handleScheduleWorkflow(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	run, err := s.workflowRuns.ScheduleAs(r.Context(), taskID, workflowActor(principal))
	if err != nil {
		writeWorkflowError(w, err, "schedule_workflow_failed")
		return
	}
	if s.workflowExecutor != nil {
		if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
			writeWorkflowError(w, err, "start_workflow_failed")
			return
		}
		run, err = s.workflowRuns.Get(r.Context(), run.ID)
		if err != nil {
			writeWorkflowError(w, err, "load_workflow_failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
}

func (s *projectServer) handleResetWorkflow(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	run, err := s.workflowRuns.Reset(r.Context(), taskID, workflowActor(principal))
	if err != nil {
		writeWorkflowError(w, err, "reset_workflow_failed")
		return
	}
	if err := s.sessions.RevokeWorkflowRunSessionTokens(r.Context(), run.ID); err != nil {
		slog.Warn("revoke reset workflow session tokens", "workflow_run_id", run.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
}

func (s *projectServer) handleForceDoneWorkflow(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request workflowDoneRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	activeRun, hadActiveRun, lookupErr := s.workflowRuns.ActiveForTask(r.Context(), taskID)
	if lookupErr != nil {
		writeWorkflowError(w, lookupErr, "complete_workflow_failed")
		return
	}
	task, err := s.workflowRuns.ForceDone(r.Context(), taskID, request.Resolution, request.Note, workflowActor(principal))
	if err != nil {
		writeWorkflowError(w, err, "complete_workflow_failed")
		return
	}
	if hadActiveRun {
		if err := s.sessions.RevokeWorkflowRunSessionTokens(r.Context(), activeRun.ID); err != nil {
			slog.Warn("revoke completed workflow session tokens", "workflow_run_id", activeRun.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleReopenWorkflow(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	task, err := s.workflowRuns.Reopen(r.Context(), taskID, workflowActor(principal))
	if err != nil {
		writeWorkflowError(w, err, "reopen_workflow_failed")
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleWorkflowPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string, parts []string) {
	if s.workflowRuns == nil || s.workflowArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "workflows_unavailable", "workflow services are not configured")
		return
	}
	if len(parts) == 0 {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		runs, err := s.workflowRuns.ListForTask(r.Context(), taskID)
		if err != nil {
			writeWorkflowError(w, err, "list_workflows_failed")
			return
		}
		if len(runs) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"runs": []coordinator.WorkflowRun{}})
			return
		}
		detail, err := s.workflowRuns.Detail(r.Context(), runs[0].ID)
		if err != nil {
			writeWorkflowError(w, err, "load_workflow_failed")
			return
		}
		artifacts, err := s.workflowArtifacts.ListForRun(r.Context(), runs[0].ID)
		if err != nil {
			writeWorkflowError(w, err, "load_workflow_failed")
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailResponse{Detail: detail, Artifacts: artifacts})
		return
	}

	switch parts[0] {
	case "artifacts":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
			writeError(w, http.StatusForbidden, "forbidden", "artifact submission requires an owner or session token")
			return
		}
		var input coordinator.CreateWorkflowArtifactInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		nodeRun, ok, err := s.workflowRuns.GetNodeRun(r.Context(), input.NodeRunID)
		if err != nil || !ok {
			if err == nil {
				err = coordinator.ErrWorkflowRunNotFound
			}
			writeWorkflowError(w, err, "create_artifact_failed")
			return
		}
		run, err := s.workflowRuns.Get(r.Context(), nodeRun.WorkflowRunID)
		if err != nil || run.TaskID != taskID {
			if err == nil {
				err = coordinator.ErrWorkflowRunNotFound
			}
			writeWorkflowError(w, err, "create_artifact_failed")
			return
		}
		input.WorkflowRunID = run.ID
		input.CreatorKey = principal.IdempotencyPrincipalKey()
		if principal.Scope == coordinator.TokenScopeSession {
			input.SessionID = strings.TrimSpace(principal.Subject)
		} else {
			input.SessionID = ""
		}
		artifact, replayed, err := s.workflowArtifacts.Create(r.Context(), input)
		if err != nil {
			writeWorkflowError(w, err, "create_artifact_failed")
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, workflowArtifactResponse{Artifact: artifact, Replayed: replayed})
	case "complete":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
			writeError(w, http.StatusForbidden, "forbidden", "agent node completion requires an owner or session token")
			return
		}
		var request workflowCompleteRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		nodeRun, ok, err := s.workflowRuns.GetNodeRun(r.Context(), request.NodeRunID)
		if err != nil || !ok {
			if err == nil {
				err = coordinator.ErrWorkflowRunNotFound
			}
			writeWorkflowError(w, err, "complete_node_failed")
			return
		}
		run, err := s.workflowRuns.Get(r.Context(), nodeRun.WorkflowRunID)
		if err != nil || run.TaskID != taskID {
			if err == nil {
				err = coordinator.ErrWorkflowRunNotFound
			}
			writeWorkflowError(w, err, "complete_node_failed")
			return
		}
		node, ok := run.Snapshot.Node(nodeRun.NodeKey)
		if !ok || node.Kind != coordinator.NodeAgent || (nodeRun.State != coordinator.WorkflowNodeSucceeded && run.CurrentNodeRunID != strings.TrimSpace(request.NodeRunID)) {
			writeError(w, http.StatusConflict, "workflow_conflict", "only the active agent node may be completed through this endpoint")
			return
		}
		artifact, err := s.workflowArtifacts.Get(r.Context(), request.ArtifactID)
		if err != nil {
			writeWorkflowError(w, err, "complete_node_failed")
			return
		}
		if principal.Scope == coordinator.TokenScopeSession && artifact.SessionID != strings.TrimSpace(principal.Subject) {
			writeError(w, http.StatusForbidden, "forbidden", "session credential does not own the workflow artifact")
			return
		}
		if artifact.Kind == coordinator.ArtifactChange {
			var payload struct {
				ChangeID string `json:"change_id"`
				HeadSHA  string `json:"head_sha"`
			}
			if err := json.Unmarshal(artifact.Payload, &payload); err != nil || strings.TrimSpace(payload.ChangeID) == "" || strings.TrimSpace(payload.HeadSHA) == "" {
				writeError(w, http.StatusBadRequest, "invalid_change_artifact", "change artifact requires change_id and head_sha")
				return
			}
			if _, err := s.sessions.UpdateChangeHead(r.Context(), payload.ChangeID, payload.HeadSHA); err != nil {
				writeWorkflowError(w, err, "update_change_failed")
				return
			}
		}
		result, err := s.workflowRuns.CompleteNode(r.Context(), coordinator.CompleteWorkflowNodeInput{
			NodeRunID: request.NodeRunID, Outcome: "completed", ArtifactID: request.ArtifactID,
			Actor: workflowActor(principal), IdempotencyKey: r.Header.Get(idempotencyHeader),
		})
		if err != nil {
			writeWorkflowError(w, err, "complete_node_failed")
			return
		}
		if principal.Scope == coordinator.TokenScopeSession {
			if artifact.Kind == coordinator.ArtifactChange {
				_, err = s.sessions.ReadyAuthorSession(r.Context(), principal.Subject)
			} else {
				_, err = s.sessions.FinishWorkPhaseSession(r.Context(), principal.Subject)
			}
			if err != nil {
				writeWorkflowError(w, err, "finish_agent_session_failed")
				return
			}
		}
		if s.workflowExecutor != nil && !result.Done {
			if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			if latest, loadErr := s.workflowRuns.Get(r.Context(), run.ID); loadErr == nil {
				result.Run = latest
				result.Done = latest.State == coordinator.WorkflowRunCompleted
			}
		}
		writeJSON(w, http.StatusOK, result)
	case "respond":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowRespondRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		result, err := s.workflowRuns.Respond(r.Context(), taskID, request.NodeRunID, request.Outcome, request.Feedback, coordinator.ActorHuman)
		if err != nil {
			writeWorkflowError(w, err, "respond_workflow_failed")
			return
		}
		if s.workflowExecutor != nil && !result.Done {
			if err := s.workflowExecutor.Advance(r.Context(), result.Run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			if latest, loadErr := s.workflowRuns.Get(r.Context(), result.Run.ID); loadErr == nil {
				result.Run = latest
				result.Done = latest.State == coordinator.WorkflowRunCompleted
			}
		}
		writeJSON(w, http.StatusOK, result)
	case "budget":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowBudgetRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		run, err := s.workflowRuns.ExtendBudget(r.Context(), taskID, request.Additional, coordinator.ActorHuman)
		if err != nil {
			writeWorkflowError(w, err, "extend_workflow_budget_failed")
			return
		}
		if s.workflowExecutor != nil {
			if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			run, err = s.workflowRuns.Get(r.Context(), run.ID)
			if err != nil {
				writeWorkflowError(w, err, "load_workflow_failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
	case "retry":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowRetryRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		run, err := s.workflowRuns.RetryExecution(r.Context(), taskID, coordinator.ActorHuman, request.RefreshAgentRuntime)
		if err != nil {
			writeWorkflowError(w, err, "retry_workflow_failed")
			return
		}
		if s.workflowExecutor != nil {
			if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			run, err = s.workflowRuns.Get(r.Context(), run.ID)
			if err != nil {
				writeWorkflowError(w, err, "load_workflow_failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
	case "advance":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		run, ok, err := s.workflowRuns.ActiveForTask(r.Context(), taskID)
		if err != nil || !ok {
			if err == nil {
				err = coordinator.ErrNoActiveWorkflowRun
			}
			writeWorkflowError(w, err, "advance_workflow_failed")
			return
		}
		if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
			writeWorkflowError(w, err, "advance_workflow_failed")
			return
		}
		run, _ = s.workflowRuns.Get(r.Context(), run.ID)
		writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func workflowActor(principal coordinator.Principal) coordinator.Actor {
	if principal.Scope == coordinator.TokenScopeSession {
		return coordinator.ActorAgent
	}
	if principal.Scope == coordinator.TokenScopeOwner || principal.Scope == coordinator.TokenScopeConsole {
		return coordinator.ActorHuman
	}
	return coordinator.ActorSystem
}

func writeWorkflowError(w http.ResponseWriter, err error, code string) {
	switch {
	case errors.Is(err, coordinator.ErrWorkflowRunNotFound), errors.Is(err, coordinator.ErrNoActiveWorkflowRun), errors.Is(err, coordinator.ErrWorkflowArtifactNotFound):
		writeError(w, http.StatusNotFound, code, err.Error())
	case errors.Is(err, coordinator.ErrWorkflowConflict):
		writeError(w, http.StatusConflict, "workflow_conflict", err.Error())
	case errors.Is(err, coordinator.ErrFlowNotFound):
		writeError(w, http.StatusBadRequest, "flow_not_found", err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}
