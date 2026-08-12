package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *projectServer) handleCreateTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request createTaskRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	input, err := createTaskInputForPrincipal(request, principal)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	// A session-created child task inherits its source task's feature unless
	// the caller explicitly assigned one.
	if input.Task.FeatureID == nil && principal.SourceTaskID != nil {
		if source, err := s.tasks.GetTask(r.Context(), *principal.SourceTaskID); err == nil && source.FeatureID != nil {
			input.Task.FeatureID = source.FeatureID
		}
	}
	task, err := s.tasks.CreateTaskWithDetails(r.Context(), input)
	if err != nil {
		writeTaskFeatureError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

// writeTaskFeatureError maps feature-aware task create/edit failures to
// statuses before falling back to the generic 400.
func writeTaskFeatureError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coordinator.ErrFeatureNotFound):
		writeError(w, http.StatusNotFound, "feature_not_found", err.Error())
	case errors.Is(err, coordinator.ErrFeatureClosed):
		writeError(w, http.StatusConflict, "feature_closed", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "invalid_task", err.Error())
	}
}

func (s *projectServer) handleListTasks(w http.ResponseWriter, r *http.Request) {
	filter, err := taskFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	ready, err := readyFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	var tasks []coordinator.Task
	if ready {
		tasks, err = s.tasks.ReadyTasks(r.Context(), filter)
	} else {
		tasks, err = s.tasks.ListTasks(r.Context(), filter)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_tasks_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, tasksResponse{Tasks: tasks})
}

// handleSearchTasks serves GET /v2/projects/{project}/search?q=...&limit=N.
// It returns FTS-ranked task hits when the binary is built with FTS5,
// substring matches otherwise (TaskService.SearchTasks owns the fallback).
func (s *projectServer) handleSearchTasks(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "q is required")
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	tasks, err := s.tasks.SearchTasks(r.Context(), query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tasksResponse{Tasks: tasks})
}

// isProjectTaskReadPath identifies the intentionally project-readable task
// surface. Console, terminal, and other operational endpoints are omitted so
// their task-binding and owner checks remain unchanged.
func isProjectTaskReadPath(method string, parts []string) bool {
	if method != http.MethodGet || len(parts) == 0 {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	switch parts[1] {
	case "checks", "attachments", "workflow":
		return true
	case "relations", "prompt-context", "transitions", "findings":
		return len(parts) == 2
	default:
		return false
	}
}

func (s *projectServer) handleTaskPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/tasks/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}

	taskID := parts[0]
	// Task-facing GET routes are project reads. Every other route remains a
	// mutation or a private operational surface and therefore keeps the tighter
	// source-task boundary where applicable.
	if !isProjectTaskReadPath(r.Method, parts) {
		if err := checkBoundTaskScope(principal, taskID); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if !s.requireProjectReadAccess(w, r, principal) {
				return
			}
			s.handleGetTask(w, r, principal, taskID)
		case http.MethodPatch:
			if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
				return
			}
			s.handleEditTask(w, r, taskID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	if len(parts) >= 2 && parts[1] == "checks" {
		s.handleChecksPath(w, r, principal, taskID, parts[2:])
		return
	}

	if len(parts) >= 2 && parts[1] == "attachments" {
		s.handleTaskAttachmentsPath(w, r, principal, taskID, parts[2:])
		return
	}

	if len(parts) == 3 && parts[1] == "lifecycle" && parts[2] == "transition" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleLifecycleTransition(w, r, principal, taskID)
		return
	}

	if len(parts) >= 2 && parts[1] == "workflow" {
		if r.Method != http.MethodGet && !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "workflow access requires an owner, session, or console token")
			return
		}
		s.handleWorkflowPath(w, r, principal, taskID, parts[2:])
		return
	}

	if len(parts) == 3 && parts[1] == "attention" && parts[2] == "reply" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleAttentionReply(w, r, principal, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "console" {
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleTaskConsole(w, r, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "relations" {
		if r.Method == http.MethodGet {
			if !s.requireProjectReadAccess(w, r, principal) {
				return
			}
		} else if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "relation operations require owner or console token")
			return
		}
		s.handleTaskRelations(w, r, principal, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "review-follow-ups" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "review follow-up requires a worker token", coordinator.TokenScopeWorker) {
			return
		}
		s.handleApplyReviewFollowUp(w, r, principal, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "prompt-context" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !s.requireProjectReadAccess(w, r, principal) {
			return
		}
		s.handlePromptContext(w, r, principal, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "transitions" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !s.requireProjectReadAccess(w, r, principal) {
			return
		}
		s.handleListTransitions(w, r, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "findings" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !s.requireProjectReadAccess(w, r, principal) {
			return
		}
		s.handleTaskFindings(w, r, taskID)
		return
	}

	if len(parts) != 2 || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	switch parts[1] {
	case "schedule":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleScheduleWorkflow(w, r, principal, taskID)
	case "reset":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleResetWorkflow(w, r, principal, taskID)
	case "done":
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleForceDoneWorkflow(w, r, principal, taskID)
	case "reopen":
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleReopenWorkflow(w, r, principal, taskID)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

// handleTaskFindings serves the task's review findings registry: every
// review thread across the task's changes, every deferred follow-up action,
// and resolution-bucket counts. Unknown tasks were already rejected by task
// routing; the sql.ErrNoRows mapping is kept for direct-service callers.
func (s *projectServer) handleTaskFindings(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.threads == nil {
		writeError(w, http.StatusInternalServerError, "findings_unavailable", "thread service is not configured")
		return
	}
	registry, err := s.threads.TaskFindingsRegistry(r.Context(), taskID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "findings_load_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, contract.TaskFindingsResponse{
		TaskID:    registry.TaskID,
		Findings:  registry.Findings,
		FollowUps: registry.FollowUps,
		Summary:   registry.Summary,
	})
}

// promptContextResponse carries the per-phase prompt material fetch-prompt
// assembles into an agent session's prompt: the current work phase's
// role instructions (from the frozen agent-def snapshot), gate feedback from a
// human's request-changes, and the handoffs of the phases already completed.
type promptContextResponse struct {
	RoleInstructions string                               `json:"role_instructions,omitempty"`
	PhaseName        string                               `json:"phase_name,omitempty"`
	WorkspaceMode    coordinator.WorkspaceMode            `json:"workspace_mode,omitempty"`
	ArtifactKind     coordinator.ArtifactKind             `json:"artifact_kind,omitempty"`
	PhaseIndex       int                                  `json:"phase_index"`
	FinalPhase       bool                                 `json:"final_phase"`
	GateFeedback     string                               `json:"gate_feedback,omitempty"`
	PriorHandoffs    []promptPhaseHandoff                 `json:"prior_handoffs,omitempty"`
	TaskSetWorkflow  *coordinator.TaskSetWorkflowContract `json:"task_set_workflow,omitempty"`
	OwnerRulings     []coordinator.OwnerRuling            `json:"owner_rulings,omitempty"`
}

type promptPhaseHandoff struct {
	PhaseName string `json:"phase_name"`
	Content   string `json:"content"`
}

func workflowCheckRoleInstructions(node coordinator.FlowNodeSnapshot, checkName string) string {
	baseCheckName := strings.SplitN(strings.TrimSpace(checkName), ".node.", 2)[0]
	if node.Config.ChangeReview != nil {
		if baseCheckName == coordinator.ReviewAggregationCheckName {
			return node.Config.ChangeReview.Aggregator.Prompt
		}
		for _, agent := range node.Config.ChangeReview.Agents {
			if strings.TrimSpace(agent.Agent.Name) == baseCheckName {
				return agent.Agent.Prompt
			}
		}
	}
	if node.Config.VerifyChange != nil {
		for _, agent := range node.Config.VerifyChange.Agents {
			if strings.TrimSpace(agent.Agent.Name) == baseCheckName {
				return agent.Agent.Prompt
			}
		}
	}
	return ""
}

func (s *projectServer) handlePromptContext(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	response := promptContextResponse{FinalPhase: true}
	if s.workflowRuns != nil {
		run, active, err := s.workflowRuns.ActiveForTask(r.Context(), taskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
			return
		}
		if active {
			detail, err := s.workflowRuns.Detail(r.Context(), run.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
				return
			}
			response.OwnerRulings = detail.ActiveRulings
			node, ok := run.Snapshot.Node(run.CurrentNodeKey)
			if ok {
				response.PhaseName = node.Name
				checkName := strings.TrimSpace(r.URL.Query().Get("check"))
				switch {
				case checkName == "" && node.Config.Agent != nil:
					response.RoleInstructions = node.Config.Agent.Agent.Prompt
					response.WorkspaceMode = node.Config.Agent.Workspace
					response.ArtifactKind = node.Config.Agent.Artifact
					if node.Config.Agent.Artifact == coordinator.ArtifactTaskSet && s.flows != nil &&
						scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
						contract, found, err := s.flows.TaskSetWorkflowContractForNode(r.Context(), run.Snapshot, node.Key)
						if err != nil {
							writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
							return
						}
						if found {
							response.TaskSetWorkflow = &contract
						}
					}
				case checkName != "":
					response.RoleInstructions = workflowCheckRoleInstructions(node, checkName)
				}
			}
			for index := len(detail.Transitions) - 1; index >= 0; index-- {
				transition := detail.Transitions[index]
				if transition.ToNodeKey != run.CurrentNodeKey || len(transition.Payload) == 0 {
					continue
				}
				var payload struct {
					Feedback string `json:"feedback"`
				}
				if json.Unmarshal(transition.Payload, &payload) == nil && strings.TrimSpace(payload.Feedback) != "" {
					response.GateFeedback = strings.TrimSpace(payload.Feedback)
				}
				break
			}
			if s.workflowArtifacts != nil {
				artifacts, err := s.workflowArtifacts.ListForRun(r.Context(), run.ID)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
					return
				}
				nodeNames := map[string]string{}
				for _, nodeRun := range detail.NodeRuns {
					if snapshotNode, ok := run.Snapshot.Node(nodeRun.NodeKey); ok {
						nodeNames[nodeRun.ID] = snapshotNode.Name
					}
				}
				for _, artifact := range artifacts {
					if strings.TrimSpace(artifact.SummaryMarkdown) != "" {
						response.PriorHandoffs = append(response.PriorHandoffs, promptPhaseHandoff{
							PhaseName: nodeNames[artifact.NodeRunID], Content: artifact.SummaryMarkdown,
						})
					}
				}
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleGetTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	task, err := s.tasks.GetTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task_not_found", err.Error())
		return
	}
	blocked, err := s.tasks.TaskBlocked(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task_blocked_failed", err.Error())
		return
	}

	response := taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name, Blocked: blocked}
	if s.status != nil {
		statusLog, err := s.status.ListForTask(r.Context(), taskID, 20)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status_log_failed", err.Error())
			return
		}
		response.StatusLog = statusLog
	}
	if scopeAllowed(principal, coordinator.TokenScopeOwner) {
		detail, err := s.buildUITaskDetail(r.Context(), task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "task_detail_failed", err.Error())
			return
		}
		response.Detail = detail
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleAttentionReply(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if s.status == nil {
		writeError(w, http.StatusServiceUnavailable, "status_unavailable", "status service is not configured")
		return
	}
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_unavailable", "session service is not configured")
		return
	}
	var request attentionReplyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	body := strings.TrimSpace(request.Message)
	if body == "" {
		writeError(w, http.StatusBadRequest, "attention_reply_failed", "message is required")
		return
	}
	if _, err := s.tasks.GetTask(r.Context(), taskID); err != nil {
		writeError(w, http.StatusNotFound, "task_not_found", err.Error())
		return
	}
	// Validate any client-supplied status_log_id up front, before writing
	// anything: it must reference an existing status entry on this task.
	// Rejecting here eliminates the orphaned-status-row window that a later FK
	// failure would otherwise leave behind, and prevents cross-linking the reply
	// to another task's status entry.
	if request.StatusLogID != nil {
		entry, err := s.status.Get(r.Context(), *request.StatusLogID)
		if err != nil || entry.TaskID != taskID {
			writeError(w, http.StatusBadRequest, "invalid_status_log_id", "status_log_id does not belong to this task")
			return
		}
	}
	status, err := s.status.Write(r.Context(), coordinator.WriteStatusInput{
		TaskID:  taskID,
		Actor:   principal.Actor(),
		Kind:    coordinator.StatusKindProgress,
		Message: "Human response:\n\n" + body,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "attention_reply_failed", err.Error())
		return
	}
	statusID := status.ID
	if request.StatusLogID != nil {
		statusID = *request.StatusLogID
	}
	message, queued, err := s.sessions.ReplyToTask(r.Context(), coordinator.ReplyToTaskInput{
		TaskID:      taskID,
		StatusLogID: &statusID,
		Actor:       principal.Actor(),
		Body:        body,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "attention_reply_failed", err.Error())
		return
	}
	resumed := false
	if s.workflowRuns != nil {
		resumed, err = s.workflowRuns.ResumeAgentRequest(r.Context(), taskID, body, coordinator.ActorHuman)
		if err != nil {
			writeWorkflowError(w, err, "attention_reply_resume_failed")
			return
		}
	}
	if !queued && !resumed {
		if err := s.ensureAuthorJobWithHumanInstructions(r, principal, taskID, body); err != nil {
			writeError(w, http.StatusBadRequest, "attention_reply_queue_failed", err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, sessionMessageResponse{Message: message, Queued: queued})
}

func (s *projectServer) ensureAuthorJobWithHumanInstructions(r *http.Request, principal coordinator.Principal, taskID string, instructions string) error {
	payload := map[string]any{"human_attention_instructions": strings.TrimSpace(instructions)}
	if s.sessions != nil {
		_, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{TaskID: taskID, Payload: payload})
		if errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
			return nil
		}
		return err
	}
	return errors.New("lifecycle engine is not configured")
}

func (s *projectServer) buildUITaskDetail(ctx context.Context, task coordinator.Task) (*uiTaskDetail, error) {
	tags, err := s.tasks.TagsForTask(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("load task tags: %w", err)
	}
	relations, err := s.tasks.RelationsForTask(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("load task relations: %w", err)
	}
	detail := &uiTaskDetail{
		Tags:      tags,
		Relations: relations,
	}
	board, err := s.tasks.BoardResult(ctx)
	if err != nil {
		return nil, fmt.Errorf("load task wait reason: %w", err)
	}
	detail.WaitReason = board.WaitReasons[task.ID]
	terminalJobs, err := s.uiTerminalJobsByTask(ctx, []coordinator.Task{task})
	if err != nil {
		return nil, err
	}
	if jobID, ok := terminalJobs[task.ID]; ok {
		detail.TerminalJobID = jobID
		detail.TerminalAvailable = true
	}
	if s.sessions != nil {
		active, ok, err := s.sessions.ActiveAuthorSessionForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load active session: %w", err)
		}
		if ok {
			summary, err := s.uiSessionSummaryWithTerminal(ctx, active)
			if err != nil {
				return nil, fmt.Errorf("load active session terminal availability: %w", err)
			}
			detail.ActiveSession = summary
			if summary.TerminalAvailable {
				detail.TerminalAvailable = true
			}
		}
		sessions, err := s.sessions.ListSessionsForTask(ctx, task.ID, 10)
		if err != nil {
			return nil, fmt.Errorf("load task sessions: %w", err)
		}
		for _, session := range sessions {
			summary, err := s.uiSessionSummaryWithTerminal(ctx, session)
			if err != nil {
				return nil, fmt.Errorf("load session terminal availability: %w", err)
			}
			detail.Sessions = append(detail.Sessions, *summary)
		}
		if detail.ActiveSession == nil && len(detail.Sessions) > 0 && detail.Sessions[0].State == coordinator.SessionAbandoned {
			paused := true
			if s.workers != nil {
				if _, live, err := s.workers.LiveAuthorJobForTask(ctx, task.ID); err != nil {
					return nil, fmt.Errorf("load live author job: %w", err)
				} else if live {
					paused = false
				}
			}
			detail.Paused = paused
		}
		changes, err := s.sessions.ListChangesForTask(ctx, task.ID, 10)
		if err != nil {
			return nil, fmt.Errorf("load task changes: %w", err)
		}
		for _, change := range changes {
			summary := uiChangeSummaryFromChange(change)
			detail.Changes = append(detail.Changes, *summary)
		}
		readyChange, ok, err := s.sessions.ReadyUnmergedChangeForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load ready change: %w", err)
		}
		if ok {
			detail.ReadyChange = uiChangeSummaryFromChange(readyChange)
		}
		consoleState, err := s.sessions.CurrentTaskConsole(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load task console: %w", err)
		}
		consoleResponse := s.consoleResponse(consoleState)
		detail.TaskConsole = &consoleResponse
	}
	if s.checks != nil {
		checks, err := s.checks.ListChecks(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load checks: %w", err)
		}
		detail.Checks = checks
		detail.RequiredChecks = uiRequiredCheckSummaryFromChecks(checks)
		reviewState, err := s.checks.ReviewState(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load review state: %w", err)
		}
		detail.ReviewState = reviewState
	}
	if s.workflowRuns != nil {
		transitions, err := s.workflowRuns.ListTransitionsForTask(ctx, task.ID, 50)
		if err != nil {
			return nil, fmt.Errorf("load transitions: %w", err)
		}
		detail.Transitions = transitions
		detail.ConvergenceEvidence, err = s.workflowRuns.ActiveConvergenceEvidenceForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load active convergence evidence: %w", err)
		}
	}
	attachments, err := s.tasks.ListTaskAttachments(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("load task attachments: %w", err)
	}
	detail.Attachments = attachments

	return detail, nil
}

func (s *projectServer) handleEditTask(w http.ResponseWriter, r *http.Request, taskID string) {
	var request editTaskRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	input := coordinator.EditTaskInput{
		Title:    request.Title,
		Body:     request.Body,
		Priority: request.Priority,
		FlowID:   request.FlowID,
	}
	if request.FeatureID != nil {
		// Tri-state: absent leaves the assignment alone, an empty string clears
		// it, anything else assigns that feature.
		value := strings.TrimSpace(*request.FeatureID)
		var clear *string
		if value == "" {
			input.FeatureID = &clear
		} else {
			assign := &value
			input.FeatureID = &assign
		}
	}
	task, err := s.tasks.EditTask(r.Context(), taskID, input)
	if err != nil {
		writeTaskFeatureError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleTaskRelations(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}

	if r.Method == http.MethodGet {
		relations, err := s.tasks.RelationsForTask(r.Context(), taskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_relations_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, contract.TaskRelationsResponse{Relations: relations})
		return
	}

	var request relationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	sourceTaskID := strings.TrimSpace(request.SourceTaskID)
	if sourceTaskID == "" {
		sourceTaskID = taskID
	}
	targetTaskID := strings.TrimSpace(request.TargetTaskID)
	kind := coordinator.RelationKind(request.Kind)

	// A task-bound console may only create or delete relations that involve its
	// bound task; the request body must not smuggle in unrelated endpoints.
	if err := checkBoundTaskRelationScope(principal, sourceTaskID, targetTaskID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}

	switch r.Method {
	case http.MethodPost:
		actor := coordinator.ActorHuman
		if principal.Scope == coordinator.TokenScopeSession {
			actor = coordinator.ActorAgent
		}
		if err := s.tasks.LinkTasks(r.Context(), sourceTaskID, targetTaskID, kind, actor); err != nil {
			writeError(w, http.StatusBadRequest, "link_tasks_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.tasks.UnlinkTasks(r.Context(), sourceTaskID, targetTaskID, kind); err != nil {
			writeError(w, http.StatusBadRequest, "unlink_tasks_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *projectServer) handleApplyReviewFollowUp(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request contract.ApplyReviewFollowUpRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	job, err := s.reviewAggregationJobForLease(r.Context(), principal, request.LeaseID, taskID)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if job.ChangeID == nil || strings.TrimSpace(*job.ChangeID) == "" {
		writeError(w, http.StatusForbidden, "forbidden", "review aggregation job is not bound to a change")
		return
	}
	result, err := s.tasks.ApplyReviewFollowUp(r.Context(), coordinator.ApplyReviewFollowUpInput{
		SourceTaskID:   taskID,
		SourceChangeID: strings.TrimSpace(*job.ChangeID),
		CheckName:      payloadString(job.Payload, "check_name"),
		Finding: coordinator.ReviewFollowUpFinding{
			SHA:                request.Finding.SHA,
			File:               request.Finding.File,
			Line:               request.Finding.Line,
			Body:               request.Finding.Body,
			Severity:           request.Finding.Severity,
			IntroducedByChange: request.Finding.IntroducedByChange,
			Requirement:        request.Finding.Requirement,
			DuplicateOf:        request.Finding.DuplicateOf,
		},
		TaskAction: coordinator.ReviewFollowUpTaskAction{
			Action: request.TaskAction.Action,
			Title:  request.TaskAction.Title,
			Body:   request.TaskAction.Body,
			TaskID: request.TaskAction.TaskID,
		},
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "review_follow_up_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, contract.ApplyReviewFollowUpResponse{
		Task:        result.Task,
		Disposition: result.Disposition,
	})
}

func (s *projectServer) reviewAggregationJobForLease(
	ctx context.Context,
	principal coordinator.Principal,
	leaseID string,
	taskID string,
) (flowworker.Job, error) {
	if s.workers == nil {
		return flowworker.Job{}, errors.New("worker service is not configured")
	}
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return flowworker.Job{}, errors.New("review follow-up requires lease_id")
	}
	if err := s.sweepExpiredLeases(ctx); err != nil {
		return flowworker.Job{}, fmt.Errorf("sweep expired leases: %w", err)
	}
	lease, err := s.workers.GetLease(ctx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return flowworker.Job{}, errors.New("lease not found")
	}
	if err != nil {
		return flowworker.Job{}, fmt.Errorf("load lease: %w", err)
	}
	if lease.WorkerID != strings.TrimSpace(principal.Subject) ||
		lease.ReleasedAt != nil ||
		!time.Now().UTC().Before(lease.ExpiresAt) {
		return flowworker.Job{}, errors.New("worker token does not own a live review aggregation lease")
	}
	job, err := s.workers.GetJob(ctx, lease.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return flowworker.Job{}, errors.New("review aggregation job not found")
	}
	if err != nil {
		return flowworker.Job{}, fmt.Errorf("load review aggregation job: %w", err)
	}
	if job.State != flowworker.JobClaimed && job.State != flowworker.JobRunning {
		return flowworker.Job{}, errors.New("review aggregation job is not live")
	}
	if job.Role != flowworker.RoleReviewer {
		return flowworker.Job{}, errors.New("only reviewer jobs may apply review follow-ups")
	}
	aggregation, _ := job.Payload["review_aggregation"].(bool)
	checkName := payloadString(job.Payload, "check_name")
	if !aggregation || !strings.HasPrefix(checkName, coordinator.ReviewAggregationCheckName+".node.") {
		return flowworker.Job{}, errors.New("only the final review aggregation job may apply review follow-ups")
	}
	if job.TaskID == nil || strings.TrimSpace(*job.TaskID) != strings.TrimSpace(taskID) {
		return flowworker.Job{}, errors.New("review aggregation job belongs to a different task")
	}
	if err := s.checkSourceJobHead(ctx, job); err != nil {
		return flowworker.Job{}, err
	}
	return job, nil
}

func (s *projectServer) handleListTransitions(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.workflowRuns == nil {
		writeError(w, http.StatusInternalServerError, "transitions_unavailable", "transition service is not configured")
		return
	}
	entries, err := s.workflowRuns.ListTransitionsForTask(r.Context(), taskID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_transitions_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, transitionsResponse{Transitions: entries})
}

func (s *projectServer) handleBoard(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	response, err := s.boardResponseForProject(r.Context(), principal)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "board_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) boardResponseForProject(ctx context.Context, principal coordinator.Principal) (boardResponse, error) {
	if err := s.projectSweepExpiredLeases(ctx); err != nil {
		return boardResponse{}, err
	}

	result, err := s.tasks.BoardResult(ctx)
	if err != nil {
		return boardResponse{}, err
	}

	response := boardResponse{
		BoardResponse: contract.BoardResponse{
			Board:       result.Board,
			LaneStates:  result.LaneStates,
			WaitReasons: result.WaitReasons,
			BlockedIDs:  result.BlockedIDs,
		},
	}

	tasks := boardTasks(result.Board)
	containersByTask := map[string]uiBoardContainerSummary{}
	if s.workItems != nil {
		items, err := s.workItems.List(ctx)
		if err != nil {
			return boardResponse{}, err
		}
		containersByTask, response.Containers = boardContainerSummaries(items, tasks)
		for _, item := range items {
			if item.Kind == coordinator.WorkItemFeature && !item.State.Terminal {
				response.Features = append(response.Features, featureBoardEntry{ID: item.ID, Title: item.Title})
			}
		}
	}

	if !scopeAllowed(principal, coordinator.TokenScopeOwner) {
		return response, nil
	}

	cards, err := s.buildUITaskCards(ctx, tasks)
	if err != nil {
		return boardResponse{}, err
	}

	for taskID, container := range containersByTask {
		card, ok := cards[taskID]
		if !ok {
			continue
		}
		containerCopy := container
		card.Container = &containerCopy
		cards[taskID] = card
	}
	response.TaskCards = cards
	return response, nil
}

const standaloneBoardContainerID = "standalone"

// boardContainerSummaries projects each board task through the canonical
// parent_item_id chain to its true top-level container. Task IDs are de-duplicated
// before counting, so a board lane overlap or malformed hierarchy cannot inflate
// a group's count.
func boardContainerSummaries(items []coordinator.WorkItemSummary, tasks []coordinator.Task) (map[string]uiBoardContainerSummary, []uiBoardContainerSummary) {
	byID := make(map[string]coordinator.WorkItemSummary, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}

	keysByTask := make(map[string]string, len(tasks))
	groups := make(map[string]uiBoardContainerSummary)
	order := make([]string, 0)
	seenTasks := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if seenTasks[task.ID] {
			continue
		}
		seenTasks[task.ID] = true

		container := uiBoardContainerSummary{ID: standaloneBoardContainerID, Kind: "standalone", Title: "Standalone"}
		currentID := task.ID
		seenItems := map[string]bool{}
		for currentID != "" && !seenItems[currentID] {
			seenItems[currentID] = true
			item, ok := byID[currentID]
			if !ok {
				break
			}
			if item.Kind != coordinator.WorkItemTask {
				container = uiBoardContainerSummary{ID: item.ID, Kind: string(item.Kind), Title: item.Title}
			}
			currentID = item.ParentItemID
		}

		key := container.Kind + "\x00" + container.ID
		group, exists := groups[key]
		if !exists {
			group = container
			order = append(order, key)
		}
		group.TaskCount++
		groups[key] = group
		keysByTask[task.ID] = key
	}

	byTask := make(map[string]uiBoardContainerSummary, len(keysByTask))
	for taskID, key := range keysByTask {
		byTask[taskID] = groups[key]
	}
	result := make([]uiBoardContainerSummary, 0, len(order))
	for _, key := range order {
		result = append(result, groups[key])
	}
	return byTask, result
}

// doneResponseForProject builds the terminal-task read model for one project.
// Tasks + outcomes are returned for any read scope; the lean cards (which read
// change details) are gated on owner scope exactly as the board cards are.
func (s *projectServer) doneResponseForProject(ctx context.Context, principal coordinator.Principal, query coordinator.ClosedTaskQuery) (doneResponse, error) {
	tasks, next, err := s.tasks.ListClosedTasks(ctx, query)
	if err != nil {
		return doneResponse{}, err
	}

	response := doneResponse{
		Tasks:    tasks,
		Outcomes: make(map[string]coordinator.Phase, len(tasks)),
	}
	if next != nil {
		response.NextBefore = sqlitex.FormatTime(next.ClosedAt)
		response.NextBeforeID = next.ID
	}
	for _, task := range tasks {
		if task.DoneResolution != nil {
			response.Outcomes[task.ID] = coordinator.Phase(*task.DoneResolution)
		}
	}

	if !scopeAllowed(principal, coordinator.TokenScopeOwner) {
		return response, nil
	}

	cards, err := s.buildUIDoneCards(ctx, tasks)
	if err != nil {
		return doneResponse{}, err
	}
	response.TaskCards = cards
	return response, nil
}

// completionStatsForProject computes the per-window successful-completion
// counts for one project using the standard /v2/stats/completions windows and
// outcomes. It shares the grouped query with the aggregate handler, so both
// paths see identical numbers.
func (s *projectServer) completionStatsForProject(ctx context.Context) ([]coordinator.ClosedTaskWindowCount, error) {
	windows := make([]time.Duration, len(completionStatWindows))
	for i, window := range completionStatWindows {
		windows[i] = window.duration
	}
	return s.tasks.CountClosedTasksByWindow(ctx, windows, successfulDoneOutcomes)
}

// buildUIDoneCards loads the merged change (if any) and tags for each closed
// task. Work is bounded by the caller's page size.
func (s *projectServer) buildUIDoneCards(ctx context.Context, tasks []coordinator.Task) (map[string]uiDoneCard, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	cards := make(map[string]uiDoneCard, len(tasks))
	for _, task := range tasks {
		card := uiDoneCard{TaskID: task.ID}
		tags, err := s.tasks.TagsForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load tags for %s: %w", task.ID, err)
		}
		card.Tags = tags
		if s.sessions != nil {
			changes, err := s.sessions.ListChangesForTask(ctx, task.ID, 10)
			if err != nil {
				return nil, fmt.Errorf("load changes for %s: %w", task.ID, err)
			}
			for _, change := range changes {
				if change.MergedAt != nil {
					card.Change = uiChangeSummaryFromChange(change)
					break
				}
			}
		}
		cards[task.ID] = card
	}

	return cards, nil
}

func boardTasks(board coordinator.Board) []coordinator.Task {
	seen := map[string]bool{}
	var tasks []coordinator.Task
	for _, lane := range [][]coordinator.Task{
		board.Unscheduled,
		board.Scheduled,
		board.InProgress,
	} {
		for _, task := range lane {
			if seen[task.ID] {
				continue
			}
			seen[task.ID] = true
			tasks = append(tasks, task)
		}
	}

	return tasks
}
