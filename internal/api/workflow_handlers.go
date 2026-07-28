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

type workflowSkipRequest struct {
	NodeRunID string `json:"node_run_id"`
}

type workflowDoneRequest struct {
	Resolution coordinator.DoneResolution `json:"resolution"`
	Note       string                     `json:"note,omitempty"`
}

type workflowCompleteRequest struct {
	NodeRunID  string `json:"node_run_id"`
	ArtifactID string `json:"artifact_id"`
}

// workflowSubmitReviewRequest parks the active agent node on a human review
// of its artifact (interactive gate). The agent stays in its session and
// polls the review GET below until a respond resolves the wait.
type workflowSubmitReviewRequest struct {
	NodeRunID  string `json:"node_run_id"`
	ArtifactID string `json:"artifact_id"`
}

type workflowSubmitReviewResponse struct {
	Wait coordinator.WorkflowWait `json:"wait"`
}

// workflowCommentRequest records plan feedback without resolving the gate.
// When the reviewed agent session is still alive the comment is also queued
// into it, so discussing the plan never requires a verdict.
type workflowCommentRequest struct {
	Message string `json:"message"`
}

type workflowCommentResponse struct {
	Status coordinator.StatusLogEntry `json:"status"`
	Queued bool                       `json:"queued"`
}

// workflowReleaseRequest hands a held run back to the executor. Edge names
// which way out the operator is taking: resume (leave it where it is), submit
// (an artifact they produced), satisfy (done, no artifact), merge (jump to the
// terminal). ArtifactID is required by submit and ignored otherwise.
type workflowReleaseRequest struct {
	Edge       string `json:"edge"`
	ArtifactID string `json:"artifact_id,omitempty"`
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
		var result coordinator.CompleteWorkflowNodeResult
		if wait, waiting, waitErr := s.workflowRuns.OpenWait(r.Context(), taskID); waitErr == nil && waiting &&
			wait.Kind == coordinator.WorkflowWaitHumanGate &&
			strings.TrimSpace(wait.NodeRunID) == strings.TrimSpace(request.NodeRunID) &&
			coordinator.ParseReviewWaitDetails(wait.Details).Interactive {
			review, err := s.workflowRuns.RespondReview(r.Context(), taskID, request.NodeRunID, request.Outcome, request.Feedback, coordinator.ActorHuman)
			if err != nil {
				writeWorkflowError(w, err, "respond_workflow_failed")
				return
			}
			// The reviewed session is deliberately not finished here: the agent's
			// flow submit is still polling with its session token, and finishing
			// would revoke it before the verdict arrives. The agent finalizes its
			// own session through the idempotent complete endpoint afterwards.
			result = review.Result
		} else {
			legacy, err := s.workflowRuns.Respond(r.Context(), taskID, request.NodeRunID, request.Outcome, request.Feedback, coordinator.ActorHuman)
			if err != nil {
				writeWorkflowError(w, err, "respond_workflow_failed")
				return
			}
			result = legacy
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
	case "submit-review":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
			writeError(w, http.StatusForbidden, "forbidden", "review submission requires an owner or session token")
			return
		}
		var request workflowSubmitReviewRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		input := coordinator.SubmitForReviewInput{
			NodeRunID:  request.NodeRunID,
			ArtifactID: request.ArtifactID,
			Actor:      workflowActor(principal),
		}
		if principal.Scope == coordinator.TokenScopeSession {
			input.SessionID = strings.TrimSpace(principal.Subject)
		}
		wait, err := s.workflowRuns.SubmitForReview(r.Context(), input)
		if err != nil {
			writeWorkflowError(w, err, "submit_review_failed")
			return
		}
		writeJSON(w, http.StatusCreated, workflowSubmitReviewResponse{Wait: wait})
	case "review":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
			writeError(w, http.StatusForbidden, "forbidden", "review status requires an owner or session token")
			return
		}
		nodeRunID := strings.TrimSpace(r.URL.Query().Get("node_run_id"))
		if nodeRunID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "node_run_id is required")
			return
		}
		status, err := s.workflowRuns.ReviewStatus(r.Context(), taskID, nodeRunID)
		if err != nil {
			writeWorkflowError(w, err, "review_status_failed")
			return
		}
		writeJSON(w, http.StatusOK, status)
	case "comment":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowCommentRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if strings.TrimSpace(request.Message) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "message is required")
			return
		}
		entry, err := s.status.Write(r.Context(), coordinator.WriteStatusInput{
			TaskID:  taskID,
			Actor:   principal.Actor(),
			Kind:    coordinator.StatusKindNote,
			Message: request.Message,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "comment_failed", err.Error())
			return
		}
		queued := false
		if s.sessions != nil {
			if _, ok, err := s.sessions.ReplyToTask(r.Context(), coordinator.ReplyToTaskInput{
				TaskID:      taskID,
				StatusLogID: &entry.ID,
				Actor:       principal.Actor(),
				Body:        request.Message,
			}); err != nil {
				slog.Warn("queue review comment for live session", "task_id", taskID, "error", err)
			} else {
				queued = ok
			}
		}
		writeJSON(w, http.StatusCreated, workflowCommentResponse{Status: entry, Queued: queued})
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
	case "skip":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowSkipRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		result, err := s.workflowRuns.SkipExecution(r.Context(), taskID, request.NodeRunID, coordinator.ActorHuman)
		if err != nil {
			writeWorkflowError(w, err, "skip_workflow_step_failed")
			return
		}
		if s.workflowExecutor != nil && !result.Done {
			if err := s.workflowExecutor.Advance(r.Context(), result.Run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			result.Run, err = s.workflowRuns.Get(r.Context(), result.Run.ID)
			if err != nil {
				writeWorkflowError(w, err, "load_workflow_failed")
				return
			}
			result.Done = result.Run.State == coordinator.WorkflowRunCompleted
		}
		writeJSON(w, http.StatusOK, result)
	case "hold":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		run, err := s.workflowRuns.Hold(r.Context(), taskID, workflowActor(principal))
		if err != nil {
			writeWorkflowError(w, err, "hold_workflow_failed")
			return
		}
		writeJSON(w, http.StatusOK, workflowRunResponse{Run: run})
	case "release":
		if !requireMethod(w, r, http.MethodPost) || !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		var request workflowReleaseRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		edge := coordinator.ReleaseEdge(strings.TrimSpace(request.Edge))
		if edge == "" {
			edge = coordinator.ReleaseResume
		}
		result, err := s.workflowRuns.Release(r.Context(), coordinator.ReleaseWorkflowInput{
			TaskID:     taskID,
			Edge:       edge,
			ArtifactID: request.ArtifactID,
			Actor:      workflowActor(principal),
		})
		if err != nil {
			writeWorkflowError(w, err, "release_workflow_failed")
			return
		}
		// The executor skipped this run while it was held, so pick it back up
		// rather than waiting out a tick.
		if s.workflowExecutor != nil && !result.Done {
			if err := s.workflowExecutor.Advance(r.Context(), result.Run.ID); err != nil {
				writeWorkflowError(w, err, "advance_workflow_failed")
				return
			}
			result.Run, err = s.workflowRuns.Get(r.Context(), result.Run.ID)
			if err != nil {
				writeWorkflowError(w, err, "load_workflow_failed")
				return
			}
			result.Done = result.Run.State == coordinator.WorkflowRunCompleted
		}
		writeJSON(w, http.StatusOK, result)
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
	case errors.Is(err, coordinator.ErrWorkflowNotHeld):
		writeError(w, http.StatusConflict, "workflow_not_held", err.Error())
	case errors.Is(err, coordinator.ErrFlowNotFound):
		writeError(w, http.StatusBadRequest, "flow_not_found", err.Error())
	default:
		writeError(w, http.StatusBadRequest, code, err.Error())
	}
}
