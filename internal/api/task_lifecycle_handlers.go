package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
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
	task, err := s.tasks.CreateTaskWithDetails(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) ensureAuthorJobForCreatedTask(r *http.Request, task coordinator.Task, principal coordinator.Principal) error {
	if task.ScheduleState != coordinator.ScheduleUpNext {
		return nil
	}
	if s.engine != nil {
		_, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
			Kind:   lifecycle.EventEnsureWorkPhaseJob,
			TaskID: task.ID,
		}))
		return err
	}
	if s.sessions != nil {
		_, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{TaskID: task.ID})
		if errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
			return nil
		}
		return err
	}
	return errors.New("lifecycle engine is not configured")
}

func (s *projectServer) handleListTasks(w http.ResponseWriter, r *http.Request) {
	filter, err := taskFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	tasks, err := s.tasks.ListTasks(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_tasks_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, tasksResponse{Tasks: tasks})
}

func (s *projectServer) handleTaskPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/tasks/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}

	taskID := parts[0]
	if err := checkBoundTaskScope(principal, taskID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "task read requires owner, session, worker, or console token")
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

	if len(parts) >= 2 && parts[1] == "workflow" {
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
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
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "relation updates require owner or console token")
			return
		}
		s.handleTaskRelations(w, r, principal, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "prompt-context" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "prompt context requires owner, session, or worker token")
			return
		}
		s.handlePromptContext(w, r, taskID)
		return
	}

	if len(parts) == 2 && parts[1] == "transitions" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession) {
			writeError(w, http.StatusForbidden, "forbidden", "transition history requires owner or session token")
			return
		}
		s.handleListTransitions(w, r, taskID)
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

// promptContextResponse carries the per-phase prompt material fetch-prompt
// assembles into an agent session's prompt: the current work phase's
// role instructions (from the frozen agent-def snapshot), gate feedback from a
// human's request-changes, and the handoffs of the phases already completed.
type promptContextResponse struct {
	RoleInstructions string                    `json:"role_instructions,omitempty"`
	PhaseName        string                    `json:"phase_name,omitempty"`
	WorkspaceMode    coordinator.WorkspaceMode `json:"workspace_mode,omitempty"`
	ArtifactKind     coordinator.ArtifactKind  `json:"artifact_kind,omitempty"`
	PhaseIndex       int                       `json:"phase_index"`
	FinalPhase       bool                      `json:"final_phase"`
	GateFeedback     string                    `json:"gate_feedback,omitempty"`
	PriorHandoffs    []promptPhaseHandoff      `json:"prior_handoffs,omitempty"`
}

type promptPhaseHandoff struct {
	PhaseName string `json:"phase_name"`
	Content   string `json:"content"`
}

func (s *projectServer) handlePromptContext(w http.ResponseWriter, r *http.Request, taskID string) {
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
			node, ok := run.Snapshot.Node(run.CurrentNodeKey)
			if ok {
				response.PhaseName = node.Name
				checkName := strings.TrimSpace(r.URL.Query().Get("check"))
				baseCheckName := strings.SplitN(checkName, ".node.", 2)[0]
				switch {
				case checkName == "" && node.Config.Agent != nil:
					response.RoleInstructions = node.Config.Agent.Agent.Prompt
					response.WorkspaceMode = node.Config.Agent.Workspace
					response.ArtifactKind = node.Config.Agent.Artifact
				case checkName != "" && node.Config.ChangeReview != nil:
					for _, agent := range node.Config.ChangeReview.Agents {
						if strings.TrimSpace(agent.Agent.Name) == baseCheckName {
							response.RoleInstructions = agent.Agent.Prompt
							break
						}
					}
				case checkName != "" && node.Config.VerifyChange != nil:
					for _, agent := range node.Config.VerifyChange.Agents {
						if strings.TrimSpace(agent.Agent.Name) == baseCheckName {
							response.RoleInstructions = agent.Agent.Prompt
							break
						}
					}
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
	if s.cursors == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}
	cursor, ok, err := s.cursors.GetCursor(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, response)
		return
	}

	completed := cursor.PhaseState == coordinator.FlowPhaseCompleted
	response.PhaseIndex = cursor.PhaseIndex
	response.FinalPhase = cursor.OnFinalPhase() || completed
	response.GateFeedback = cursor.GateFeedback
	if checkName := strings.TrimSpace(r.URL.Query().Get("check")); checkName != "" {
		// A review check job's prompt: the flow review agent running under this
		// check name (checks are named after their agent defs). Unmatched names
		// (repo-defined checks, deduped collisions) fall back to the embedded
		// role skill client-side.
		response = promptContextResponse{FinalPhase: true}
		for _, reviewAgent := range cursor.Snapshot.ReviewAgents {
			if strings.TrimSpace(reviewAgent.Agent.Name) == checkName {
				response.RoleInstructions = reviewAgent.Agent.Prompt
				break
			}
		}
		handoffs, err := s.cursors.PhaseHandoffs(r.Context(), taskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
			return
		}
		for _, handoff := range handoffs {
			if strings.TrimSpace(handoff.Content) == "" {
				continue
			}
			response.PriorHandoffs = append(response.PriorHandoffs, promptPhaseHandoff{
				PhaseName: handoff.PhaseName,
				Content:   handoff.Content,
			})
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if completed {
		// The pipeline already readied its change: this is a review fix round,
		// driven by the flow's fix agent.
		if agent, err := cursor.Snapshot.FixAgentOrLastPhase(); err == nil {
			response.PhaseName = "fix"
			response.RoleInstructions = agent.Prompt
		}
	} else if phase, ok := cursor.CurrentPhase(); ok {
		response.PhaseName = phase.Name
		response.RoleInstructions = phase.Agent.Prompt
	}

	handoffs, err := s.cursors.PhaseHandoffs(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prompt_context_failed", err.Error())
		return
	}
	for _, handoff := range handoffs {
		if !completed && handoff.PhaseIndex >= cursor.PhaseIndex {
			continue
		}
		if strings.TrimSpace(handoff.Content) == "" {
			continue
		}
		response.PriorHandoffs = append(response.PriorHandoffs, promptPhaseHandoff{
			PhaseName: handoff.PhaseName,
			Content:   handoff.Content,
		})
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleGetTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	task, err := s.tasks.GetTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task_not_found", err.Error())
		return
	}

	response := taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name}
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

// handleApproveWorkPhase applies a human's approval of a gate-paused work
// phase through the engine: the cursor advances to the next phase (or the
// final phase's change is published into review).
func (s *projectServer) handleApproveWorkPhase(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:   lifecycle.EventWorkPhaseApproved,
		TaskID: taskID,
	}))
	if err != nil {
		writeEngineError(w, err, "approve_phase_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "approve_phase_failed", err.Error())
		return
	}
	response := taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name}
	response.Flow = s.taskFlowStatus(r.Context(), taskID)
	writeJSON(w, http.StatusOK, response)
}

// handleReworkWorkPhase applies a human's request-changes on a gate-paused
// work phase: the same phase re-runs with the feedback injected into its
// prompt.
func (s *projectServer) handleReworkWorkPhase(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request phaseRequestChangesRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	feedback := strings.TrimSpace(request.Feedback)
	if feedback == "" {
		writeError(w, http.StatusBadRequest, "request_changes_failed", "feedback is required")
		return
	}
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventWorkPhaseRework,
		TaskID:  taskID,
		Payload: lifecycle.EventPayload{GateFeedback: feedback},
	}))
	if err != nil {
		writeEngineError(w, err, "request_changes_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request_changes_failed", err.Error())
		return
	}
	response := taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name}
	response.Flow = s.taskFlowStatus(r.Context(), taskID)
	writeJSON(w, http.StatusOK, response)
}

type phaseRequestChangesRequest struct {
	Feedback string `json:"feedback"`
}

// taskFlowStatus assembles the task's flow position for API/UI consumers:
// which flow, the ordered phases, the cursor position and gate state, and —
// when paused at a gate — the pending handoff awaiting review. Nil when the
// task has no cursor.
func (s *projectServer) taskFlowStatus(ctx context.Context, taskID string) *taskFlowStatus {
	if s.cursors == nil {
		return nil
	}
	cursor, ok, err := s.cursors.GetCursor(ctx, taskID)
	if err != nil || !ok {
		return nil
	}
	status := &taskFlowStatus{
		FlowID:       cursor.Snapshot.FlowID,
		FlowName:     cursor.Snapshot.FlowName,
		PhaseIndex:   cursor.PhaseIndex,
		PhaseCount:   len(cursor.Snapshot.Phases),
		PhaseState:   string(cursor.PhaseState),
		GateFeedback: cursor.GateFeedback,
	}
	if phase, ok := cursor.CurrentPhase(); ok {
		status.PhaseName = phase.Name
		status.Gate = string(phase.Gate)
	}
	for _, phase := range cursor.Snapshot.Phases {
		status.Phases = append(status.Phases, taskFlowPhase{
			Name:            phase.Name,
			Gate:            string(phase.Gate),
			AgentName:       phase.Agent.Name,
			AgentHarness:    phase.Agent.Harness,
			Model:           phase.Agent.Model,
			ReasoningEffort: phase.Agent.ReasoningEffort,
		})
	}
	if cursor.PhaseState == coordinator.FlowPhaseAwaitingApproval {
		if handoff, ok, err := s.cursors.PhaseHandoff(ctx, taskID, cursor.PhaseIndex); err == nil && ok {
			status.PendingHandoff = handoff.Content
		}
	}
	return status
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

func (s *projectServer) handleApproveReviewCycles(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	var request approveReviewCyclesRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if _, err := s.tasks.GetTask(r.Context(), taskID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "get_task_failed", err.Error())
		return
	}

	_, err := s.sessions.ApproveReviewCycles(r.Context(), coordinator.ApproveReviewCyclesInput{
		TaskID:       taskID,
		Cycles:       request.Cycles,
		Instructions: request.Instructions,
		Actor:        principal.Actor(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "approve_review_cycles_failed", err.Error())
		return
	}

	var failures []lifecycle.FollowUpFailure
	if s.engine != nil {
		result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
			Kind:   lifecycle.EventEnsureWorkPhaseJob,
			TaskID: taskID,
		}))
		if err != nil && !errors.Is(err, lifecycle.ErrInvalidTransition) {
			writeEngineError(w, err, "approve_review_cycles_failed")
			return
		}
		failures = result.FollowUpFailures
	}

	budget, err := s.sessions.ReviewCycleBudget(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_review_cycles_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reviewCycleBudgetResponse{Budget: budget, FollowUpFailures: failures})
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
	if s.transitions != nil {
		transitions, err := s.transitions.ListForTask(ctx, task.ID, 50)
		if err != nil {
			return nil, fmt.Errorf("load transitions: %w", err)
		}
		detail.Transitions = transitions
		timeline, err := s.transitions.ListForTaskWithPayload(ctx, task.ID, 50)
		if err != nil {
			return nil, fmt.Errorf("load timeline transitions: %w", err)
		}
		detail.TimelineTransitions = timeline
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

	task, err := s.tasks.EditTask(r.Context(), taskID, coordinator.EditTaskInput{
		Title:              request.Title,
		Body:               request.Body,
		AcceptanceCriteria: request.AcceptanceCriteria,
		Priority:           request.Priority,
		FlowID:             request.FlowID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "edit_task_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleTaskRelations(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
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

	switch r.Method {
	case http.MethodPost:
		actor := coordinator.ActorHuman
		if principal.Scope == coordinator.TokenScopeConsole {
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

// requireEngine guards lifecycle handlers against a server constructed without
// the engine's dependencies, returning false (and a 503) instead of panicking.
func (s *projectServer) requireEngine(w http.ResponseWriter) bool {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "lifecycle_unavailable", "lifecycle engine is not configured")
		return false
	}
	return true
}

// writeEngineError maps a lifecycle engine error to an HTTP response, preserving
// the per-handler default code for ordinary failures while surfacing the FSM's
// own sentinels with appropriate statuses.
func writeEngineError(w http.ResponseWriter, err error, defaultCode string) {
	switch {
	case errors.Is(err, lifecycle.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, lifecycle.ErrVersionConflict):
		writeError(w, http.StatusConflict, "version_conflict", err.Error())
	case errors.Is(err, lifecycle.ErrCascadeLimit):
		writeError(w, http.StatusInternalServerError, "cascade_limit", err.Error())
	default:
		writeError(w, http.StatusBadRequest, defaultCode, err.Error())
	}
}

func (s *projectServer) handleScheduleTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request scheduleTaskRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventScheduleTask,
		TaskID:  taskID,
		Payload: lifecycle.EventPayload{Schedule: coordinator.ScheduleState(request.State)},
	}))
	if err != nil {
		writeEngineError(w, err, "schedule_task_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "schedule_task_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleSetTaskState(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request taskStateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventSetTaskState,
		TaskID:  taskID,
		Payload: lifecycle.EventPayload{TaskState: coordinator.TaskState(request.State)},
	}))
	if err != nil {
		writeEngineError(w, err, "set_task_state_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "set_task_state_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleResetTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:   lifecycle.EventResetTask,
		TaskID: taskID,
	}))
	if err != nil {
		writeEngineError(w, err, "reset_task_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reset_task_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

// taskForResult returns the task carried by a StepResult, falling back to a
// fresh load when the transition did not surface one (e.g. an idempotent replay).
func (s *projectServer) taskForResult(ctx context.Context, result lifecycle.StepResult, taskID string) (coordinator.Task, error) {
	if result.Task != nil {
		return *result.Task, nil
	}
	return s.tasks.GetTask(ctx, taskID)
}

func (s *projectServer) handleCloseTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventCloseTask, TaskID: taskID}))
	if err != nil {
		writeEngineError(w, err, "close_task_failed")
		return
	}
	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "close_task_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handlePauseTask(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_unavailable", "session service is not configured")
		return
	}
	if _, err := s.sessions.PauseAuthorSession(r.Context(), taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusConflict, "pause_task_failed", "task has no active author session")
			return
		}
		writeError(w, http.StatusBadRequest, "pause_task_failed", err.Error())
		return
	}
	task, err := s.tasks.GetTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pause_task_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleResumeTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventEnsureWorkPhaseJob, TaskID: taskID}))
	if err != nil {
		writeEngineError(w, err, "resume_task_failed")
		return
	}
	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "resume_task_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleRetryCrashedAuthorJob(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventRetryCrashedAuthorJob, TaskID: taskID}))
	if err != nil {
		writeEngineError(w, err, "retry_crashed_author_job_failed")
		return
	}
	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "retry_crashed_author_job_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleMergeTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if !s.requireEngine(w) {
		return
	}
	if s.merges == nil {
		writeError(w, http.StatusInternalServerError, "merges_unavailable", "merge service is not configured")
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventMergeRequested, TaskID: taskID}))
	if err != nil {
		writeEngineError(w, err, "merge_failed")
		return
	}
	if result.Merge == nil {
		writeError(w, http.StatusBadRequest, "merge_failed", "merge produced no result")
		return
	}

	writeJSON(w, http.StatusOK, mergeResponse{Merge: *result.Merge})
}

func (s *projectServer) handleMergeChange(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if !s.requireEngine(w) {
		return
	}
	if s.merges == nil {
		writeError(w, http.StatusInternalServerError, "merges_unavailable", "merge service is not configured")
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventMergeChange, ChangeID: changeID}))
	if err != nil {
		writeEngineError(w, err, "merge_failed")
		return
	}
	if result.Merge == nil {
		writeError(w, http.StatusBadRequest, "merge_failed", "merge produced no result")
		return
	}

	writeJSON(w, http.StatusOK, mergeResponse{Merge: *result.Merge})
}

func (s *projectServer) handleTriageTask(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	var request triageTaskRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	state := coordinator.TriageState(request.State)
	if state != coordinator.TriageAccepted && state != coordinator.TriageRejected {
		writeError(w, http.StatusBadRequest, "invalid_triage_state", "triage state must be accepted or rejected")
		return
	}

	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventTriageTask,
		TaskID:  taskID,
		Payload: lifecycle.EventPayload{Triage: state},
	}))
	if err != nil {
		writeEngineError(w, err, "triage_task_failed")
		return
	}

	task, err := s.taskForResult(r.Context(), result, taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "triage_task_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Task: task, ProjectID: s.project.ID, ProjectName: s.project.Name})
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

	if !scopeAllowed(principal, coordinator.TokenScopeOwner) {
		return response, nil
	}

	cards, err := s.buildUITaskCards(ctx, boardTasks(result.Board))
	if err != nil {
		return boardResponse{}, err
	}

	response.TaskCards = cards
	return response, nil
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
