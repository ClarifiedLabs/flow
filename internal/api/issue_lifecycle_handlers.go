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

func (s *projectServer) handleCreateIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request createIssueRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	input, err := createIssueInputForPrincipal(request, principal)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	issue, err := s.issues.CreateIssueWithDetails(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_issue", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) ensureAuthorJobForCreatedIssue(r *http.Request, issue coordinator.Issue, principal coordinator.Principal) error {
	if issue.ScheduleState != coordinator.ScheduleUpNext {
		return nil
	}
	if s.engine != nil {
		_, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
			Kind:    lifecycle.EventEnsureWorkPhaseJob,
			IssueID: issue.ID,
		}))
		return err
	}
	if s.sessions != nil {
		_, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
		if errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
			return nil
		}
		return err
	}
	return errors.New("lifecycle engine is not configured")
}

func (s *projectServer) handleListIssues(w http.ResponseWriter, r *http.Request) {
	filter, err := issueFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	issues, err := s.issues.ListIssues(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_issues_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issuesResponse{Issues: issues})
}

func (s *projectServer) handleIssuePath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/issues/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "issue not found")
		return
	}

	issueID := parts[0]
	if err := checkBoundIssueScope(principal, issueID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "issue read requires owner, session, worker, or console token")
				return
			}
			s.handleGetIssue(w, r, principal, issueID)
		case http.MethodPatch:
			if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
				return
			}
			s.handleEditIssue(w, r, issueID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	if len(parts) >= 2 && parts[1] == "checks" {
		s.handleChecksPath(w, r, principal, issueID, parts[2:])
		return
	}

	if len(parts) >= 2 && parts[1] == "attachments" {
		s.handleIssueAttachmentsPath(w, r, principal, issueID, parts[2:])
		return
	}

	if len(parts) >= 2 && parts[1] == "workflow" {
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "workflow access requires an owner, session, or console token")
			return
		}
		s.handleWorkflowPath(w, r, principal, issueID, parts[2:])
		return
	}

	if len(parts) == 3 && parts[1] == "attention" && parts[2] == "reply" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleAttentionReply(w, r, principal, issueID)
		return
	}

	if len(parts) == 2 && parts[1] == "console" {
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleIssueConsole(w, r, issueID)
		return
	}

	if len(parts) == 2 && parts[1] == "relations" {
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "relation updates require owner or console token")
			return
		}
		s.handleIssueRelations(w, r, principal, issueID)
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
		s.handlePromptContext(w, r, issueID)
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
		s.handleListTransitions(w, r, issueID)
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
		s.handleScheduleWorkflow(w, r, principal, issueID)
	case "reset":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleResetWorkflow(w, r, principal, issueID)
	case "done":
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleForceDoneWorkflow(w, r, principal, issueID)
	case "reopen":
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleReopenWorkflow(w, r, principal, issueID)
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

func (s *projectServer) handlePromptContext(w http.ResponseWriter, r *http.Request, issueID string) {
	response := promptContextResponse{FinalPhase: true}
	if s.workflowRuns != nil {
		run, active, err := s.workflowRuns.ActiveForIssue(r.Context(), issueID)
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
	cursor, ok, err := s.cursors.GetCursor(r.Context(), issueID)
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
		handoffs, err := s.cursors.PhaseHandoffs(r.Context(), issueID)
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

	handoffs, err := s.cursors.PhaseHandoffs(r.Context(), issueID)
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

func (s *projectServer) handleGetIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	issue, err := s.issues.GetIssue(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusNotFound, "issue_not_found", err.Error())
		return
	}

	response := issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name}
	if s.status != nil {
		statusLog, err := s.status.ListForIssue(r.Context(), issueID, 20)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status_log_failed", err.Error())
			return
		}
		response.StatusLog = statusLog
	}
	if scopeAllowed(principal, coordinator.TokenScopeOwner) {
		detail, err := s.buildUIIssueDetail(r.Context(), issue)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "issue_detail_failed", err.Error())
			return
		}
		response.Detail = detail
	}
	writeJSON(w, http.StatusOK, response)
}

// handleApproveWorkPhase applies a human's approval of a gate-paused work
// phase through the engine: the cursor advances to the next phase (or the
// final phase's change is published into review).
func (s *projectServer) handleApproveWorkPhase(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventWorkPhaseApproved,
		IssueID: issueID,
	}))
	if err != nil {
		writeEngineError(w, err, "approve_phase_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "approve_phase_failed", err.Error())
		return
	}
	response := issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name}
	response.Flow = s.issueFlowStatus(r.Context(), issueID)
	writeJSON(w, http.StatusOK, response)
}

// handleReworkWorkPhase applies a human's request-changes on a gate-paused
// work phase: the same phase re-runs with the feedback injected into its
// prompt.
func (s *projectServer) handleReworkWorkPhase(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
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
		IssueID: issueID,
		Payload: lifecycle.EventPayload{GateFeedback: feedback},
	}))
	if err != nil {
		writeEngineError(w, err, "request_changes_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request_changes_failed", err.Error())
		return
	}
	response := issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name}
	response.Flow = s.issueFlowStatus(r.Context(), issueID)
	writeJSON(w, http.StatusOK, response)
}

type phaseRequestChangesRequest struct {
	Feedback string `json:"feedback"`
}

// issueFlowStatus assembles the issue's flow position for API/UI consumers:
// which flow, the ordered phases, the cursor position and gate state, and —
// when paused at a gate — the pending handoff awaiting review. Nil when the
// issue has no cursor.
func (s *projectServer) issueFlowStatus(ctx context.Context, issueID string) *issueFlowStatus {
	if s.cursors == nil {
		return nil
	}
	cursor, ok, err := s.cursors.GetCursor(ctx, issueID)
	if err != nil || !ok {
		return nil
	}
	status := &issueFlowStatus{
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
		status.Phases = append(status.Phases, issueFlowPhase{
			Name:            phase.Name,
			Gate:            string(phase.Gate),
			AgentName:       phase.Agent.Name,
			AgentHarness:    phase.Agent.Harness,
			Model:           phase.Agent.Model,
			ReasoningEffort: phase.Agent.ReasoningEffort,
		})
	}
	if cursor.PhaseState == coordinator.FlowPhaseAwaitingApproval {
		if handoff, ok, err := s.cursors.PhaseHandoff(ctx, issueID, cursor.PhaseIndex); err == nil && ok {
			status.PendingHandoff = handoff.Content
		}
	}
	return status
}

func (s *projectServer) handleAttentionReply(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
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
	if _, err := s.issues.GetIssue(r.Context(), issueID); err != nil {
		writeError(w, http.StatusNotFound, "issue_not_found", err.Error())
		return
	}
	// Validate any client-supplied status_log_id up front, before writing
	// anything: it must reference an existing status entry on this issue.
	// Rejecting here eliminates the orphaned-status-row window that a later FK
	// failure would otherwise leave behind, and prevents cross-linking the reply
	// to another issue's status entry.
	if request.StatusLogID != nil {
		entry, err := s.status.Get(r.Context(), *request.StatusLogID)
		if err != nil || entry.IssueID != issueID {
			writeError(w, http.StatusBadRequest, "invalid_status_log_id", "status_log_id does not belong to this issue")
			return
		}
	}
	status, err := s.status.Write(r.Context(), coordinator.WriteStatusInput{
		IssueID: issueID,
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
	message, queued, err := s.sessions.ReplyToIssue(r.Context(), coordinator.ReplyToIssueInput{
		IssueID:     issueID,
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
		resumed, err = s.workflowRuns.ResumeAgentRequest(r.Context(), issueID, body, coordinator.ActorHuman)
		if err != nil {
			writeWorkflowError(w, err, "attention_reply_resume_failed")
			return
		}
	}
	if !queued && !resumed {
		if err := s.ensureAuthorJobWithHumanInstructions(r, principal, issueID, body); err != nil {
			writeError(w, http.StatusBadRequest, "attention_reply_queue_failed", err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, sessionMessageResponse{Message: message, Queued: queued})
}

func (s *projectServer) ensureAuthorJobWithHumanInstructions(r *http.Request, principal coordinator.Principal, issueID string, instructions string) error {
	payload := map[string]any{"human_attention_instructions": strings.TrimSpace(instructions)}
	if s.sessions != nil {
		_, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{IssueID: issueID, Payload: payload})
		if errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
			return nil
		}
		return err
	}
	return errors.New("lifecycle engine is not configured")
}

func (s *projectServer) handleApproveReviewCycles(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	var request approveReviewCyclesRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if _, err := s.issues.GetIssue(r.Context(), issueID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "issue_not_found", "issue not found")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "get_issue_failed", err.Error())
		return
	}

	_, err := s.sessions.ApproveReviewCycles(r.Context(), coordinator.ApproveReviewCyclesInput{
		IssueID:      issueID,
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
			Kind:    lifecycle.EventEnsureWorkPhaseJob,
			IssueID: issueID,
		}))
		if err != nil && !errors.Is(err, lifecycle.ErrInvalidTransition) {
			writeEngineError(w, err, "approve_review_cycles_failed")
			return
		}
		failures = result.FollowUpFailures
	}

	budget, err := s.sessions.ReviewCycleBudget(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_review_cycles_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reviewCycleBudgetResponse{Budget: budget, FollowUpFailures: failures})
}

func (s *projectServer) buildUIIssueDetail(ctx context.Context, issue coordinator.Issue) (*uiIssueDetail, error) {
	tags, err := s.issues.TagsForIssue(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("load issue tags: %w", err)
	}
	relations, err := s.issues.RelationsForIssue(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("load issue relations: %w", err)
	}
	detail := &uiIssueDetail{
		Tags:      tags,
		Relations: relations,
	}
	board, err := s.issues.BoardResult(ctx)
	if err != nil {
		return nil, fmt.Errorf("load issue wait reason: %w", err)
	}
	detail.WaitReason = board.WaitReasons[issue.ID]
	terminalJobs, err := s.uiTerminalJobsByIssue(ctx, []coordinator.Issue{issue})
	if err != nil {
		return nil, err
	}
	if jobID, ok := terminalJobs[issue.ID]; ok {
		detail.TerminalJobID = jobID
		detail.TerminalAvailable = true
	}
	if s.sessions != nil {
		active, ok, err := s.sessions.ActiveAuthorSessionForIssue(ctx, issue.ID)
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
		sessions, err := s.sessions.ListSessionsForIssue(ctx, issue.ID, 10)
		if err != nil {
			return nil, fmt.Errorf("load issue sessions: %w", err)
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
				if _, live, err := s.workers.LiveAuthorJobForIssue(ctx, issue.ID); err != nil {
					return nil, fmt.Errorf("load live author job: %w", err)
				} else if live {
					paused = false
				}
			}
			detail.Paused = paused
		}
		changes, err := s.sessions.ListChangesForIssue(ctx, issue.ID, 10)
		if err != nil {
			return nil, fmt.Errorf("load issue changes: %w", err)
		}
		for _, change := range changes {
			summary := uiChangeSummaryFromChange(change)
			detail.Changes = append(detail.Changes, *summary)
		}
		readyChange, ok, err := s.sessions.ReadyUnmergedChangeForIssue(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load ready change: %w", err)
		}
		if ok {
			detail.ReadyChange = uiChangeSummaryFromChange(readyChange)
		}
		consoleState, err := s.sessions.CurrentIssueConsole(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load issue console: %w", err)
		}
		consoleResponse := s.consoleResponse(consoleState)
		detail.IssueConsole = &consoleResponse
	}
	if s.checks != nil {
		checks, err := s.checks.ListChecks(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load checks: %w", err)
		}
		detail.Checks = checks
		detail.RequiredChecks = uiRequiredCheckSummaryFromChecks(checks)
		reviewState, err := s.checks.ReviewState(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load review state: %w", err)
		}
		detail.ReviewState = reviewState
	}
	if s.transitions != nil {
		transitions, err := s.transitions.ListForIssue(ctx, issue.ID, 50)
		if err != nil {
			return nil, fmt.Errorf("load transitions: %w", err)
		}
		detail.Transitions = transitions
		timeline, err := s.transitions.ListForIssueWithPayload(ctx, issue.ID, 50)
		if err != nil {
			return nil, fmt.Errorf("load timeline transitions: %w", err)
		}
		detail.TimelineTransitions = timeline
	}
	attachments, err := s.issues.ListIssueAttachments(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("load issue attachments: %w", err)
	}
	detail.Attachments = attachments

	return detail, nil
}

func (s *projectServer) handleEditIssue(w http.ResponseWriter, r *http.Request, issueID string) {
	var request editIssueRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	issue, err := s.issues.EditIssue(r.Context(), issueID, coordinator.EditIssueInput{
		Title:              request.Title,
		Body:               request.Body,
		AcceptanceCriteria: request.AcceptanceCriteria,
		Priority:           request.Priority,
		FlowID:             request.FlowID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "edit_issue_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleIssueRelations(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}

	var request relationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	sourceIssueID := strings.TrimSpace(request.SourceIssueID)
	if sourceIssueID == "" {
		sourceIssueID = issueID
	}
	targetIssueID := strings.TrimSpace(request.TargetIssueID)
	kind := coordinator.RelationKind(request.Kind)

	switch r.Method {
	case http.MethodPost:
		actor := coordinator.ActorHuman
		if principal.Scope == coordinator.TokenScopeConsole {
			actor = coordinator.ActorAgent
		}
		if err := s.issues.LinkIssues(r.Context(), sourceIssueID, targetIssueID, kind, actor); err != nil {
			writeError(w, http.StatusBadRequest, "link_issues_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.issues.UnlinkIssues(r.Context(), sourceIssueID, targetIssueID, kind); err != nil {
			writeError(w, http.StatusBadRequest, "unlink_issues_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *projectServer) handleListTransitions(w http.ResponseWriter, r *http.Request, issueID string) {
	if s.workflowRuns == nil {
		writeError(w, http.StatusInternalServerError, "transitions_unavailable", "transition service is not configured")
		return
	}
	entries, err := s.workflowRuns.ListTransitionsForIssue(r.Context(), issueID, 100)
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

func (s *projectServer) handleScheduleIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	var request scheduleIssueRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventScheduleIssue,
		IssueID: issueID,
		Payload: lifecycle.EventPayload{Schedule: coordinator.ScheduleState(request.State)},
	}))
	if err != nil {
		writeEngineError(w, err, "schedule_issue_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "schedule_issue_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleSetIssueState(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	var request issueStateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventSetIssueState,
		IssueID: issueID,
		Payload: lifecycle.EventPayload{IssueState: coordinator.IssueState(request.State)},
	}))
	if err != nil {
		writeEngineError(w, err, "set_issue_state_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "set_issue_state_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleResetIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
		Kind:    lifecycle.EventResetIssue,
		IssueID: issueID,
	}))
	if err != nil {
		writeEngineError(w, err, "reset_issue_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reset_issue_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

// issueForResult returns the issue carried by a StepResult, falling back to a
// fresh load when the transition did not surface one (e.g. an idempotent replay).
func (s *projectServer) issueForResult(ctx context.Context, result lifecycle.StepResult, issueID string) (coordinator.Issue, error) {
	if result.Issue != nil {
		return *result.Issue, nil
	}
	return s.issues.GetIssue(ctx, issueID)
}

func (s *projectServer) handleCloseIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventCloseIssue, IssueID: issueID}))
	if err != nil {
		writeEngineError(w, err, "close_issue_failed")
		return
	}
	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "close_issue_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handlePauseIssue(w http.ResponseWriter, r *http.Request, issueID string) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_unavailable", "session service is not configured")
		return
	}
	if _, err := s.sessions.PauseAuthorSession(r.Context(), issueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusConflict, "pause_issue_failed", "issue has no active author session")
			return
		}
		writeError(w, http.StatusBadRequest, "pause_issue_failed", err.Error())
		return
	}
	issue, err := s.issues.GetIssue(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pause_issue_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleResumeIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventEnsureWorkPhaseJob, IssueID: issueID}))
	if err != nil {
		writeEngineError(w, err, "resume_issue_failed")
		return
	}
	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "resume_issue_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleRetryCrashedAuthorJob(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventRetryCrashedAuthorJob, IssueID: issueID}))
	if err != nil {
		writeEngineError(w, err, "retry_crashed_author_job_failed")
		return
	}
	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "retry_crashed_author_job_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleMergeIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if !s.requireEngine(w) {
		return
	}
	if s.merges == nil {
		writeError(w, http.StatusInternalServerError, "merges_unavailable", "merge service is not configured")
		return
	}
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventMergeRequested, IssueID: issueID}))
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

func (s *projectServer) handleTriageIssue(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	var request triageIssueRequest
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
		Kind:    lifecycle.EventTriageIssue,
		IssueID: issueID,
		Payload: lifecycle.EventPayload{Triage: state},
	}))
	if err != nil {
		writeEngineError(w, err, "triage_issue_failed")
		return
	}

	issue, err := s.issueForResult(r.Context(), result, issueID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "triage_issue_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueResponse{Issue: issue, ProjectID: s.project.ID, ProjectName: s.project.Name})
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

	result, err := s.issues.BoardResult(ctx)
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

	cards, err := s.buildUIIssueCards(ctx, boardIssues(result.Board))
	if err != nil {
		return boardResponse{}, err
	}

	response.IssueCards = cards
	return response, nil
}

// doneResponseForProject builds the terminal-issue read model for one project.
// Issues + outcomes are returned for any read scope; the lean cards (which read
// change details) are gated on owner scope exactly as the board cards are.
func (s *projectServer) doneResponseForProject(ctx context.Context, principal coordinator.Principal, query coordinator.ClosedIssueQuery) (doneResponse, error) {
	issues, next, err := s.issues.ListClosedIssues(ctx, query)
	if err != nil {
		return doneResponse{}, err
	}

	response := doneResponse{
		Issues:   issues,
		Outcomes: make(map[string]coordinator.Phase, len(issues)),
	}
	if next != nil {
		response.NextBefore = sqlitex.FormatTime(next.ClosedAt)
		response.NextBeforeID = next.ID
	}
	for _, issue := range issues {
		if issue.DoneResolution != nil {
			response.Outcomes[issue.ID] = coordinator.Phase(*issue.DoneResolution)
		}
	}

	if !scopeAllowed(principal, coordinator.TokenScopeOwner) {
		return response, nil
	}

	cards, err := s.buildUIDoneCards(ctx, issues)
	if err != nil {
		return doneResponse{}, err
	}
	response.IssueCards = cards
	return response, nil
}

// buildUIDoneCards loads the merged change (if any) and tags for each closed
// issue. Work is bounded by the caller's page size.
func (s *projectServer) buildUIDoneCards(ctx context.Context, issues []coordinator.Issue) (map[string]uiDoneCard, error) {
	if len(issues) == 0 {
		return nil, nil
	}

	cards := make(map[string]uiDoneCard, len(issues))
	for _, issue := range issues {
		card := uiDoneCard{IssueID: issue.ID}
		tags, err := s.issues.TagsForIssue(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load tags for %s: %w", issue.ID, err)
		}
		card.Tags = tags
		if s.sessions != nil {
			changes, err := s.sessions.ListChangesForIssue(ctx, issue.ID, 10)
			if err != nil {
				return nil, fmt.Errorf("load changes for %s: %w", issue.ID, err)
			}
			for _, change := range changes {
				if change.MergedAt != nil {
					card.Change = uiChangeSummaryFromChange(change)
					break
				}
			}
		}
		cards[issue.ID] = card
	}

	return cards, nil
}

func boardIssues(board coordinator.Board) []coordinator.Issue {
	seen := map[string]bool{}
	var issues []coordinator.Issue
	for _, lane := range [][]coordinator.Issue{
		board.Unscheduled,
		board.Scheduled,
		board.InProgress,
	} {
		for _, issue := range lane {
			if seen[issue.ID] {
				continue
			}
			seen[issue.ID] = true
			issues = append(issues, issue)
		}
	}

	return issues
}
