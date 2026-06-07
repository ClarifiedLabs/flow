package api

import (
	"context"
	"database/sql"
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
	if err := s.ensureAuthorJobForCreatedIssue(r, issue, principal); err != nil {
		writeError(w, http.StatusBadRequest, "create_issue_queue_failed", err.Error())
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
			Kind:    lifecycle.EventEnsureAuthorJob,
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
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/issues/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "issue not found")
		return
	}

	issueID := parts[0]
	if err := checkConsoleIssueScope(principal, issueID); err != nil {
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

	if len(parts) == 3 && parts[1] == "plan" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		switch parts[2] {
		case "approve":
			s.handleApprovePlan(w, r, principal, issueID)
		case "reject":
			s.handleRejectPlan(w, r, principal, issueID)
		default:
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
		}
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

	if len(parts) == 3 && parts[1] == "review" && parts[2] == "run" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleRunReview(w, r, issueID)
		return
	}

	if len(parts) == 3 && parts[1] == "review-cycles" && parts[2] == "approve" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleApproveReviewCycles(w, r, principal, issueID)
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
		s.handleScheduleIssue(w, r, principal, issueID)
	case "state":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleSetIssueState(w, r, principal, issueID)
	case "close":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleCloseIssue(w, r, principal, issueID)
	case "pause":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handlePauseIssue(w, r, issueID)
	case "resume":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleResumeIssue(w, r, principal, issueID)
	case "retry":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleRetryCrashedAuthorJob(w, r, principal, issueID)
	case "triage":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleTriageIssue(w, r, principal, issueID)
	case "merge":
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleMergeIssue(w, r, principal, issueID)
	case "reset":
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleResetIssue(w, r, principal, issueID)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
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

func (s *projectServer) handleApprovePlan(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	issue, err := s.issues.GetIssue(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusNotFound, "issue_not_found", err.Error())
		return
	}
	if !issue.PlanMode {
		writeError(w, http.StatusBadRequest, "approve_plan_failed", "issue is not in plan mode")
		return
	}
	if strings.TrimSpace(issue.PlanBody) == "" || issue.PlanApprovedAt != nil {
		writeError(w, http.StatusBadRequest, "approve_plan_failed", "issue does not have a pending plan")
		return
	}
	if s.sessions != nil && strings.TrimSpace(issue.PlanSessionID) != "" {
		session, err := s.sessions.GetSession(r.Context(), issue.PlanSessionID)
		if err == nil && (session.RuntimeState == coordinator.SessionStarting || session.RuntimeState == coordinator.SessionWorking || session.RuntimeState == coordinator.SessionWaiting) {
			if _, err := s.sessions.ReadyPlanningSession(r.Context(), issue.PlanSessionID); err != nil {
				writeError(w, http.StatusBadRequest, "approve_plan_failed", err.Error())
				return
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "approve_plan_failed", err.Error())
			return
		}
	}
	approved, err := s.issues.MarkPlanApproved(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "approve_plan_failed", err.Error())
		return
	}
	if s.engine != nil {
		if _, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{
			Kind:    lifecycle.EventEnsureAuthorJob,
			IssueID: issue.ID,
		})); err != nil {
			writeError(w, http.StatusBadRequest, "approve_plan_queue_failed", err.Error())
			return
		}
	} else if s.sessions != nil {
		if _, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{IssueID: issue.ID, Purpose: coordinator.AuthorSessionPurposeAuthoring}); err != nil && !errors.Is(err, coordinator.ErrAuthorJobSuppressed) {
			writeError(w, http.StatusBadRequest, "approve_plan_queue_failed", err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: approved, ProjectID: s.project.ID, ProjectName: s.project.Name})
}

func (s *projectServer) handleRejectPlan(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if s.status == nil {
		writeError(w, http.StatusServiceUnavailable, "status_unavailable", "status service is not configured")
		return
	}
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_unavailable", "session service is not configured")
		return
	}
	var request planRejectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	comments := strings.TrimSpace(request.Comments)
	if comments == "" {
		writeError(w, http.StatusBadRequest, "reject_plan_failed", "comments are required")
		return
	}
	issue, err := s.issues.GetIssue(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusNotFound, "issue_not_found", err.Error())
		return
	}
	if !issue.PlanMode || strings.TrimSpace(issue.PlanBody) == "" || issue.PlanApprovedAt != nil {
		writeError(w, http.StatusBadRequest, "reject_plan_failed", "issue does not have a pending plan")
		return
	}
	status, err := s.status.Write(r.Context(), coordinator.WriteStatusInput{
		IssueID: issue.ID,
		Actor:   principal.Actor(),
		Kind:    coordinator.StatusKindQuestion,
		Message: "Plan rejected:\n\n" + comments + "\n\nRejected plan:\n\n" + strings.TrimSpace(issue.PlanBody),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "reject_plan_failed", err.Error())
		return
	}
	replyBody := "The plan was rejected. Revise the plan using these comments, then record a new plan with `flow status --kind plan`.\n\n" + comments
	_, queued, err := s.sessions.ReplyToIssue(r.Context(), coordinator.ReplyToIssueInput{
		IssueID:     issue.ID,
		StatusLogID: &status.ID,
		Actor:       principal.Actor(),
		Body:        replyBody,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "reject_plan_reply_failed", err.Error())
		return
	}
	cleared, err := s.issues.ClearPendingPlan(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reject_plan_failed", err.Error())
		return
	}
	if !queued {
		if err := s.ensureAuthorJobWithHumanInstructions(r, principal, issue.ID, replyBody); err != nil {
			writeError(w, http.StatusBadRequest, "reject_plan_queue_failed", err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, issueResponse{Issue: cleared, ProjectID: s.project.ID, ProjectName: s.project.Name})
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
	if !queued {
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
			Kind:    lifecycle.EventEnsureAuthorJob,
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
	crashRetry, err := s.issues.CrashRetryAvailable(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("load crash retry availability: %w", err)
	}
	detail.CrashRetryAvailable = crashRetry
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
		budget, err := s.sessions.ReviewCycleBudget(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load review cycle budget: %w", err)
		}
		detail.ReviewCycleBudget = &budget
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
		graph, err := s.transitions.GraphSummaryForIssue(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load lifecycle graph: %w", err)
		}
		detail.LifecycleGraph = &graph
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
		Title:               request.Title,
		Body:                request.Body,
		AcceptanceCriteria:  request.AcceptanceCriteria,
		Priority:            request.Priority,
		RequiresHumanReview: request.RequiresHumanReview,
		AutoMerge:           request.AutoMerge,
		PlanMode:            request.PlanMode,
		AgentHarness:        request.AgentHarness,
		HarnessArgs:         request.HarnessArgs,
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
	if s.transitions == nil {
		writeError(w, http.StatusInternalServerError, "transitions_unavailable", "transition service is not configured")
		return
	}
	entries, err := s.transitions.ListForIssue(r.Context(), issueID, 100)
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
	result, err := s.engine.Step(r.Context(), s.lifecycleEvent(r, principal, lifecycle.Event{Kind: lifecycle.EventEnsureAuthorJob, IssueID: issueID}))
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
		phase, err := s.issues.PhaseForIssue(ctx, issue)
		if err != nil {
			return doneResponse{}, fmt.Errorf("derive phase for %s: %w", issue.ID, err)
		}
		response.Outcomes[issue.ID] = phase
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
		board.Backlog,
		board.UpNext,
		board.InProgress,
		board.NeedsAttention,
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
