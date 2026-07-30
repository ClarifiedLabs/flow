package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *projectServer) handleChangePath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/changes/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	if len(parts) == 1 {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleGetChange(w, r, parts[0])
		return
	}

	switch parts[1] {
	case "diff":
		if len(parts) != 2 || r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleGetChangeDiff(w, r, parts[0])
	case "checks":
		taskID := parts[0]
		s.handleChecksPath(w, r, principal, taskID, parts[2:])
	case "comments":
		if len(parts) != 2 || r.Method != http.MethodPost {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "thread creation requires owner, session, or worker token")
			return
		}
		s.handleCreateThread(w, r, principal, parts[0])
	case "threads":
		if len(parts) != 2 || r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "thread read requires owner, session, or worker token")
			return
		}
		s.handleListThreads(w, r, principal, parts[0])
	case "review":
		if len(parts) != 2 || !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleSubmitReview(w, r, principal, parts[0])
	case "merge":
		if len(parts) != 2 || !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleMergeChange(w, r, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *projectServer) handleGetChange(w http.ResponseWriter, r *http.Request, changeID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}

	change, err := s.sessions.GetChange(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "get_change_failed", err.Error())
		return
	}
	task, err := s.tasks.GetTask(r.Context(), change.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "get_change_task_failed", err.Error())
		return
	}

	var checks []coordinator.Check
	var reviewState coordinator.ReviewState
	var requiredChecks uiRequiredCheckSummary
	if s.checks != nil {
		checks, err = s.checks.ListChecks(r.Context(), change.TaskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_checks_failed", err.Error())
			return
		}
		reviewState, err = s.checks.ReviewState(r.Context(), change.TaskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "review_state_failed", err.Error())
			return
		}
		requiredChecks = uiRequiredCheckSummaryFromChecks(checks)
	}

	var threads []coordinator.ReviewThread
	if s.threads != nil {
		threads, err = s.threads.ListThreadsForChange(r.Context(), change.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_threads_failed", err.Error())
			return
		}
	}

	canMerge, mergeBlockedReason, err := s.changeMergeEligibility(r.Context(), task, change)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "merge_eligibility_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, changeResponse{
		Change:             change,
		ProjectID:          s.project.ID,
		ProjectName:        s.project.Name,
		Task:               task,
		Checks:             checks,
		ReviewState:        reviewState,
		RequiredChecks:     requiredChecks,
		Threads:            threads,
		CanMerge:           canMerge,
		MergeBlockedReason: mergeBlockedReason,
	})
}

func (s *projectServer) handleGetChangeDiff(w http.ResponseWriter, r *http.Request, changeID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	change, err := s.sessions.GetChange(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "get_change_failed", err.Error())
		return
	}

	response := changeDiffResponse{
		ChangeID: change.ID,
		Base:     change.Base,
		HeadSHA:  change.HeadSHA,
	}
	stats, unavailableReason, err := s.changeDiffStats(r.Context(), change, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "change_diff_failed", err.Error())
		return
	}
	if unavailableReason != "" {
		response.UnavailableReason = unavailableReason
		writeJSON(w, http.StatusOK, response)
		return
	}
	response.Available = true
	response.Files = stats.Files
	response.TotalFiles = len(stats.Files)
	response.Additions = stats.Additions
	response.Deletions = stats.Deletions
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) changeDiffStats(ctx context.Context, change coordinator.Change, includeHunks bool) (flowgit.DiffStats, string, error) {
	if strings.TrimSpace(change.HeadSHA) == "" {
		return flowgit.DiffStats{}, "change head sha is not recorded", nil
	}
	if s.merges == nil {
		return flowgit.DiffStats{}, "merge service is not configured", nil
	}
	exchangePath, err := s.merges.ExchangePathForChange(ctx, change)
	if err != nil {
		return flowgit.DiffStats{}, err.Error(), nil
	}

	// Keep a merged change's review baseline pinned to the pre-merge base tip
	// (previous_base_sha) recorded in its completed merge intent. Unmerged
	// changes compare against the current base ref; the git layer resolves the
	// merge base so later base-only commits never appear as reverse edits.
	oldRef := "refs/heads/" + change.Base
	if change.MergedAt != nil {
		if baseSHA, ok, baseErr := s.merges.MergeBaseForChange(ctx, change); baseErr != nil {
			return flowgit.DiffStats{}, "", baseErr
		} else if ok {
			oldRef = baseSHA
		}
	}

	var stats flowgit.DiffStats
	if includeHunks {
		stats, err = flowgit.ChangedFileDiff(ctx, exchangePath, oldRef, change.HeadSHA)
	} else {
		stats, err = flowgit.ChangedFileStats(ctx, exchangePath, oldRef, change.HeadSHA)
	}
	if err != nil {
		return flowgit.DiffStats{}, "", err
	}

	return stats, "", nil
}

func (s *projectServer) changeMergeEligibility(ctx context.Context, task coordinator.Task, change coordinator.Change) (bool, string, error) {
	if s.merges == nil {
		return false, "merge service is not configured", nil
	}

	return s.merges.ChangeMergeEligibility(ctx, task, change)
}

func (s *projectServer) handleThreadPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if s.threads == nil {
		writeError(w, http.StatusInternalServerError, "threads_unavailable", "thread service is not configured")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/threads/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
		writeError(w, http.StatusForbidden, "forbidden", "thread operation requires owner, session, or worker token")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	switch parts[1] {
	case "comments":
		s.handleReplyThread(w, r, principal, parts[0])
	case "claims":
		s.handleClaimThread(w, r, principal, parts[0])
	case "certify":
		s.handleCertifyThread(w, r, principal, parts[0])
	case "reopen":
		s.handleReopenThread(w, r, principal, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *projectServer) handleCreateThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if s.threads == nil {
		writeError(w, http.StatusInternalServerError, "threads_unavailable", "thread service is not configured")
		return
	}
	var request createThreadRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	taskID, err := s.threads.ChangeTaskID(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}
	if err := s.checkThreadChangeAccess(r, principal, taskID, changeID, request.LeaseID, true, worker.RoleReviewer, worker.RoleVerifier); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	thread, err := s.threads.CreateThread(r.Context(), coordinator.CreateThreadInput{
		ChangeID:        changeID,
		AnchorCommitSHA: request.AnchorCommitSHA,
		FilePath:        request.FilePath,
		Line:            request.Line,
		Context:         request.Context,
		Body:            request.Body,
		Actor:           principal.Actor(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "create_thread_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, threadResponse{Thread: thread})
}

func (s *projectServer) handleListThreads(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if s.threads == nil {
		writeError(w, http.StatusInternalServerError, "threads_unavailable", "thread service is not configured")
		return
	}
	taskID, err := s.threads.ChangeTaskID(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}
	if err := s.checkThreadChangeAccess(r, principal, taskID, changeID, r.URL.Query().Get("lease_id"), true, worker.RoleReviewer, worker.RoleVerifier); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	threads, err := s.threads.ListThreadsForChange(r.Context(), changeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "list_threads_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, threadsResponse{Threads: threads})
}

// threadMutation names which review-thread verb an endpoint is performing.
// Thread mutations are workflow inputs, not state-machine transitions: active
// workflow check nodes observe the resulting thread/check state when the
// executor next advances.
type threadMutation string

const (
	threadMutationComment threadMutation = "comment"
	threadMutationClaim   threadMutation = "claim"
	threadMutationCertify threadMutation = "certify"
	threadMutationReopen  threadMutation = "reopen"
)

type threadMutationInput struct {
	Kind           threadMutation
	Body           string
	ClaimKind      coordinator.ReviewClaimKind
	ClaimCommitSHA string
}

// applyThreadMutation runs the shared thread-mutation flow — load, authorize,
// dispatch — against the review thread service.
func (s *projectServer) applyThreadMutation(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string, leaseID string, allowSession bool, roles []worker.JobRole, mutation threadMutationInput, failCode string, notFoundMsg string) {
	thread, err := s.threads.GetThread(r.Context(), threadID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "thread_not_found", "thread not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_thread_failed", err.Error())
		return
	}
	if err := s.checkThreadChangeAccess(r, principal, thread.TaskID, thread.ChangeID, leaseID, allowSession, roles...); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	actor := principal.Actor()
	var updated coordinator.ReviewThread
	switch mutation.Kind {
	case threadMutationComment:
		updated, err = s.threads.AddComment(r.Context(), coordinator.AddThreadCommentInput{
			ThreadID: threadID, Body: mutation.Body, Actor: actor,
		})
	case threadMutationClaim:
		updated, err = s.threads.ClaimThread(r.Context(), coordinator.ClaimThreadInput{
			ThreadID: threadID, Kind: mutation.ClaimKind, Body: mutation.Body,
			ClaimCommitSHA: mutation.ClaimCommitSHA, Actor: actor,
		})
	case threadMutationCertify:
		updated, err = s.threads.CertifyThread(r.Context(), coordinator.VerifyThreadInput{
			ThreadID: threadID, Body: mutation.Body, Actor: actor,
		})
	case threadMutationReopen:
		updated, err = s.threads.ReopenThread(r.Context(), coordinator.VerifyThreadInput{
			ThreadID: threadID, Body: mutation.Body, Actor: actor,
		})
	default:
		err = errors.New("unsupported review thread mutation")
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "thread_not_found", notFoundMsg)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, failCode, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, threadResponse{Thread: updated})
}

func (s *projectServer) handleReplyThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadCommentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.applyThreadMutation(w, r, principal, threadID, request.LeaseID, true,
		[]worker.JobRole{worker.RoleReviewer, worker.RoleVerifier},
		threadMutationInput{Kind: threadMutationComment, Body: request.Body},
		"reply_thread_failed", "thread not found")
}

func (s *projectServer) handleClaimThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.applyThreadMutation(w, r, principal, threadID, request.LeaseID, true, nil,
		threadMutationInput{
			Kind:           threadMutationClaim,
			Body:           request.Body,
			ClaimKind:      coordinator.ReviewClaimKind(strings.TrimSpace(request.Kind)),
			ClaimCommitSHA: request.ClaimCommitSHA,
		},
		"claim_thread_failed", "thread not found or not claimable")
}

func (s *projectServer) handleCertifyThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadCommentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.applyThreadMutation(w, r, principal, threadID, request.LeaseID, false,
		[]worker.JobRole{worker.RoleVerifier},
		threadMutationInput{Kind: threadMutationCertify, Body: request.Body},
		"certify_thread_failed", "thread not found or not certifiable")
}

func (s *projectServer) handleReopenThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadCommentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.applyThreadMutation(w, r, principal, threadID, request.LeaseID, false,
		[]worker.JobRole{worker.RoleVerifier},
		threadMutationInput{Kind: threadMutationReopen, Body: request.Body},
		"reopen_thread_failed", "thread not found or not reopenable")
}

func (s *projectServer) handleChecksPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string, parts []string) {
	if s.checks == nil {
		writeError(w, http.StatusInternalServerError, "checks_unavailable", "check service is not configured")
		return
	}

	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "check read requires owner, session, or worker token")
			return
		}
		s.handleListChecks(w, r, taskID)
	case len(parts) == 1 && r.Method == http.MethodGet:
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "check read requires owner, session, or worker token")
			return
		}
		s.handleGetCheck(w, r, taskID, parts[0])
	case len(parts) == 1 && r.Method == http.MethodPost:
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "check reporting requires owner, session, or worker token")
			return
		}
		s.handleReportCheck(w, r, principal, taskID, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *projectServer) handleListChecks(w http.ResponseWriter, r *http.Request, taskID string) {
	checks, err := s.checks.ListChecks(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "list_checks_failed", err.Error())
		return
	}
	reviewState, err := s.checks.ReviewState(r.Context(), taskID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "review_state_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, checksResponse{Checks: checks, ReviewState: reviewState})
}

func (s *projectServer) handleGetCheck(w http.ResponseWriter, r *http.Request, taskID string, name string) {
	check, err := s.checks.GetCheck(r.Context(), taskID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "check_not_found", "check not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "get_check_failed", err.Error())
		return
	}
	reviewState, err := s.checks.ReviewState(r.Context(), taskID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "review_state_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, checkResponse{Check: check, ReviewState: reviewState})
}

func (s *projectServer) handleReportCheck(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string, name string) {
	var request reportCheckRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := s.checkReportScope(r, taskID, name, request, principal); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}

	reportInput := coordinator.ReportCheckInput{
		TaskID: taskID, Name: name,
		Kind: coordinator.CheckKind(strings.TrimSpace(request.Kind)), Required: request.Required,
		Verdict: coordinator.CheckVerdict(strings.TrimSpace(request.Verdict)), ExitCode: request.ExitCode,
		Details: request.Details, SourceJobID: request.SourceJobID, Reporter: checkReporter(request, principal),
	}
	if principal.Scope == coordinator.TokenScopeWorker {
		reportInput.WorkerID = principal.Subject
		reportInput.WorkerLeaseID = stringValue(request.LeaseID)
	}
	check, err := s.checks.ReportCheck(r.Context(), reportInput)
	if errors.Is(err, coordinator.ErrCheckReportLeaseInvalid) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "report_check_failed", err.Error())
		return
	}
	var workflowRun *coordinator.WorkflowRun
	if s.workflowExecutor != nil {
		run, active, err := s.workflowRuns.ActiveForTask(r.Context(), taskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load_workflow_failed", err.Error())
			return
		}
		if active {
			if err := s.workflowExecutor.Advance(r.Context(), run.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "advance_workflow_failed", err.Error())
				return
			}
			latest, err := s.workflowRuns.Get(r.Context(), run.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "load_workflow_failed", err.Error())
				return
			}
			workflowRun = &latest
		}
	}
	reviewState, err := s.checks.ReviewState(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "review_state_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, checkResponse{
		Check:       check,
		ReviewState: reviewState,
		Workflow:    workflowRun,
	})
}

func checkReporter(request reportCheckRequest, principal coordinator.Principal) string {
	if principal.Scope == coordinator.TokenScopeOwner {
		reporter := strings.TrimSpace(request.Reporter)
		if reporter != "" {
			return reporter
		}
	}
	reporter := strings.TrimSpace(principal.Subject)
	if reporter != "" {
		return reporter
	}

	return string(principal.Scope)
}

func (s *projectServer) checkThreadChangeAccess(r *http.Request, principal coordinator.Principal, taskID string, changeID string, leaseID string, allowSession bool, workerRoles ...worker.JobRole) error {
	taskID = strings.TrimSpace(taskID)
	changeID = strings.TrimSpace(changeID)
	leaseID = strings.TrimSpace(leaseID)
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession:
		if !allowSession {
			return errors.New("session token cannot verify review threads")
		}
		if s.sessions == nil {
			return errors.New("session service is not configured")
		}
		session, err := s.sessions.GetSession(r.Context(), principal.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("session not found")
		}
		if err != nil {
			return fmt.Errorf("load session: %w", err)
		}
		if session.TaskID != taskID || session.ChangeID != changeID {
			return errors.New("session token cannot access threads for a different change")
		}
		return nil
	case coordinator.TokenScopeWorker:
		if len(workerRoles) == 0 {
			return errors.New("worker token cannot perform this thread operation")
		}
		return s.checkWorkerThreadLease(r.Context(), principal, leaseID, taskID, changeID, workerRoles)
	default:
		return errors.New("thread operation requires owner, session, or worker token")
	}
}

func (s *projectServer) checkWorkerThreadLease(ctx context.Context, principal coordinator.Principal, leaseID string, taskID string, changeID string, allowedRoles []worker.JobRole) error {
	if s.workers == nil {
		return errors.New("worker service is not configured")
	}
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("worker thread operations require lease_id")
	}
	if err := s.sweepExpiredLeases(ctx); err != nil {
		return fmt.Errorf("sweep expired leases: %w", err)
	}
	lease, err := s.workers.GetLease(ctx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("lease not found")
	}
	if err != nil {
		return fmt.Errorf("load lease: %w", err)
	}
	if lease.WorkerID != strings.TrimSpace(principal.Subject) || lease.ReleasedAt != nil || !time.Now().UTC().Before(lease.ExpiresAt) {
		return errors.New("worker token does not own a live thread job lease")
	}
	job, err := s.workers.GetJob(ctx, lease.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("lease job not found")
	}
	if err != nil {
		return fmt.Errorf("load lease job: %w", err)
	}
	if job.State != worker.JobClaimed && job.State != worker.JobRunning {
		return errors.New("thread job is not live")
	}
	if !workerRoleAllowed(job.Role, allowedRoles) {
		return errors.New("worker job role cannot perform this thread operation")
	}
	if !jobBlocksApproval(job) {
		return errors.New("advisory and discovery check jobs cannot mutate review threads")
	}
	if job.TaskID == nil || strings.TrimSpace(*job.TaskID) != taskID {
		return errors.New("worker job does not belong to the thread task")
	}
	if job.ChangeID != nil && strings.TrimSpace(*job.ChangeID) != changeID {
		return errors.New("worker job does not belong to the thread change")
	}

	return nil
}

func jobBlockingValue(job worker.Job) (bool, bool) {
	value, present := job.Payload["blocking"]
	if !present {
		return true, false
	}
	blocking, ok := value.(bool)
	if !ok {
		return true, true
	}
	return blocking, true
}

func jobBlocksApproval(job worker.Job) bool {
	if discovery, _ := job.Payload["review_discovery"].(bool); discovery {
		return false
	}
	blocking, stamped := jobBlockingValue(job)
	return !stamped || blocking
}

func workerRoleAllowed(role worker.JobRole, allowed []worker.JobRole) bool {
	for _, allowedRole := range allowed {
		if role == allowedRole {
			return true
		}
	}

	return false
}

func (s *projectServer) checkReportScope(r *http.Request, taskID string, checkName string, request reportCheckRequest, principal coordinator.Principal) error {
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession:
		if principal.SourceTaskID == nil || strings.TrimSpace(*principal.SourceTaskID) != strings.TrimSpace(taskID) {
			return errors.New("session token cannot report checks for a different task")
		}
		if err := s.checkSessionCheckReportScope(r.Context(), taskID, checkName, request); err != nil {
			return err
		}
		return nil
	case coordinator.TokenScopeWorker:
		if s.workers == nil {
			return errors.New("worker service is not configured")
		}
		if err := s.sweepExpiredLeases(r.Context()); err != nil {
			return fmt.Errorf("sweep expired leases: %w", err)
		}
		sourceJobID := strings.TrimSpace(stringValue(request.SourceJobID))
		if sourceJobID == "" {
			return errors.New("worker check reports require source_job_id")
		}
		leaseID := strings.TrimSpace(stringValue(request.LeaseID))
		if leaseID == "" {
			return errors.New("worker check reports require lease_id")
		}
		lease, err := s.workers.GetLease(r.Context(), leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("lease not found")
		}
		if err != nil {
			return fmt.Errorf("load lease: %w", err)
		}
		if lease.WorkerID != strings.TrimSpace(principal.Subject) || lease.JobID != sourceJobID || lease.ReleasedAt != nil {
			return errors.New("worker token does not own the live check job lease")
		}
		job, err := s.workers.GetJob(r.Context(), sourceJobID)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("source job not found")
		}
		if err != nil {
			return fmt.Errorf("load source job: %w", err)
		}
		expectedKind, err := checkKindForWorkerJob(job)
		if err != nil {
			return err
		}
		requestKind := coordinator.CheckKind(strings.TrimSpace(string(request.Kind)))
		if requestKind == "" {
			requestKind = coordinator.CheckKindCI
		}
		if requestKind != expectedKind {
			return errors.New("worker check kind does not match source job role")
		}
		if job.TaskID == nil || strings.TrimSpace(*job.TaskID) != strings.TrimSpace(taskID) {
			return errors.New("source job does not belong to the check task")
		}
		jobCheckName := payloadString(job.Payload, "check_name")
		if jobCheckName == "" {
			return errors.New("source job missing check_name")
		}
		if jobCheckName != strings.TrimSpace(checkName) {
			return errors.New("source job does not belong to the reported check")
		}
		if err := s.checkSourceJobHead(r.Context(), job); err != nil {
			return err
		}
		stampedBlocking, stamped := jobBlockingValue(job)
		if request.Required == nil {
			if stamped {
				return errors.New("worker check required value does not match source job blocking mode")
			}
		} else if *request.Required != stampedBlocking {
			return errors.New("worker check required value does not match source job blocking mode")
		}
		return nil
	default:
		return errors.New("check reporting requires owner, session, or worker token")
	}
}

func (s *projectServer) checkSessionCheckReportScope(ctx context.Context, taskID string, checkName string, request reportCheckRequest) error {
	requestKind := coordinator.CheckKind(strings.TrimSpace(request.Kind))
	if requestKind == "" {
		requestKind = coordinator.CheckKindCI
	}
	if requestKind != coordinator.CheckKindCI {
		return errors.New("session tokens can only report optional ci checks")
	}
	if request.Required == nil || *request.Required {
		return errors.New("session tokens cannot report required checks")
	}
	if s.checks == nil {
		return errors.New("check service is not configured")
	}
	existing, err := s.checks.GetCheck(ctx, taskID, checkName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load existing check: %w", err)
	}
	if existing.Required {
		return errors.New("session tokens cannot update required checks")
	}
	if existing.Kind != coordinator.CheckKindCI {
		return errors.New("session tokens can only update optional ci checks")
	}

	return nil
}

func checkKindForWorkerJob(job worker.Job) (coordinator.CheckKind, error) {
	switch job.Role {
	case worker.RoleCI:
		if job.CapacityBucket != worker.BucketEphemeral {
			return "", errors.New("ci check reports require an ephemeral source job")
		}
		return coordinator.CheckKindCI, nil
	case worker.RoleReviewer:
		return coordinator.CheckKindReviewer, nil
	case worker.RoleVerifier:
		return coordinator.CheckKindVerifier, nil
	default:
		return "", errors.New("worker check reports require a ci, reviewer, or verifier source job")
	}
}

func (s *projectServer) checkSourceJobHead(ctx context.Context, job worker.Job) error {
	if s.sessions == nil {
		return errors.New("session service is not configured")
	}
	var changeID string
	if job.ChangeID != nil {
		changeID = strings.TrimSpace(*job.ChangeID)
	}
	if changeID == "" {
		return errors.New("source job missing change_id")
	}
	payloadChangeID := payloadString(job.Payload, "change_id")
	if payloadChangeID == "" {
		return errors.New("source job missing change_id")
	}
	if payloadChangeID != changeID {
		return errors.New("source job change_id does not match payload")
	}
	headSHA := payloadString(job.Payload, "head_sha")
	if headSHA == "" {
		return errors.New("source job missing head_sha")
	}
	change, err := s.sessions.GetChange(ctx, changeID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("source job change not found")
	}
	if err != nil {
		return fmt.Errorf("load source job change: %w", err)
	}
	if strings.TrimSpace(change.HeadSHA) == "" {
		return errors.New("source job change head is not recorded")
	}
	if headSHA != strings.TrimSpace(change.HeadSHA) {
		return errors.New("source job head does not match current change head")
	}

	return nil
}
