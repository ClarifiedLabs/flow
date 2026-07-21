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
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
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

	// After a squash merge the base ref advances to a commit whose tree equals
	// the branch content, so diffing the current base ref against the head is
	// empty. When the change is merged, diff against the pre-merge base tip
	// (previous_base_sha) recorded in the completed merge intent instead. Fall
	// back to the current base ref when no intent is found.
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

// handlePutHandoff records the handoff an author agent just wrote into the
// coordinator's snapshot store, so the latest handoff reaches the coordinator
// the moment it is written rather than waiting for the next git reconcile. Git
// remains the durable source of truth: a later reconcile pass still overwrites
// this snapshot from the branch ref. Invalid handoffs are recorded (valid=false)
// rather than rejected, mirroring reconcile semantics.
func (s *projectServer) handlePutHandoff(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	if s.reconciler == nil {
		writeError(w, http.StatusInternalServerError, "reconciler_unavailable", "reconcile service is not configured")
		return
	}
	var request putHandoffRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	change, err := s.sessions.GetChange(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}
	// A session token must own this change; owners may write any change's
	// handoff. Workers cannot write handoffs (no allowed worker roles passed).
	if err := s.checkThreadChangeAccess(r, principal, change.TaskID, change.ID, "", true); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err := s.reconciler.UpsertHandoffSnapshot(r.Context(), change.ID, strings.TrimSpace(request.HeadSHA), request.Content); err != nil {
		writeError(w, http.StatusBadRequest, "put_handoff_failed", err.Error())
		return
	}
	snapshot, err := s.reconciler.GetHandoffSnapshot(r.Context(), change.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_handoff_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, handoffResponse{
		ChangeID: snapshot.ChangeID,
		HeadSHA:  snapshot.HeadSHA,
		Present:  snapshot.Present,
		Valid:    snapshot.Valid,
		Summary:  snapshot.Summary,
	})
}

// handleGetHandoff returns the coordinator's current handoff snapshot for a
// change, including the full body. The session builder injects this prior
// handoff into the next author (fix round) and verifier prompt, replacing the
// committed .handoff.md the next session used to cat. A missing snapshot returns
// 404 so callers can treat "no prior handoff" as a normal, empty case.
func (s *projectServer) handleGetHandoff(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	if s.reconciler == nil {
		writeError(w, http.StatusInternalServerError, "reconciler_unavailable", "reconcile service is not configured")
		return
	}
	change, err := s.sessions.GetChange(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}
	// Owners read any change; the change's session and the reviewer/verifier
	// workers acting on it read its handoff for prompt context.
	if err := s.checkThreadChangeAccess(r, principal, change.TaskID, change.ID, r.URL.Query().Get("lease_id"), true, worker.RoleReviewer, worker.RoleVerifier); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	snapshot, err := s.reconciler.GetHandoffSnapshot(r.Context(), change.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "handoff_not_found", "handoff not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_handoff_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, handoffResponse{
		ChangeID: snapshot.ChangeID,
		HeadSHA:  snapshot.HeadSHA,
		Present:  snapshot.Present,
		Valid:    snapshot.Valid,
		Summary:  snapshot.Summary,
		Content:  snapshot.Content,
	})
}

// stepThreadEvent runs the shared thread-mutation flow against the review
// thread service. Thread mutations are workflow inputs, not lifecycle-engine
// transitions; active workflow check nodes observe the resulting thread/check
// state when the executor advances.
func (s *projectServer) stepThreadEvent(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string, leaseID string, allowSession bool, roles []worker.JobRole, event lifecycle.Event, failCode string, notFoundMsg string) {
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
	switch event.Kind {
	case lifecycle.EventThreadComment:
		updated, err = s.threads.AddComment(r.Context(), coordinator.AddThreadCommentInput{
			ThreadID: threadID, Body: event.Payload.Body, Actor: actor,
		})
	case lifecycle.EventThreadClaimed:
		updated, err = s.threads.ClaimThread(r.Context(), coordinator.ClaimThreadInput{
			ThreadID: threadID, Kind: event.Payload.ThreadKind, Body: event.Payload.Body,
			ClaimCommitSHA: event.Payload.ClaimCommitSHA, Actor: actor,
		})
	case lifecycle.EventThreadCertify:
		updated, err = s.threads.CertifyThread(r.Context(), coordinator.VerifyThreadInput{
			ThreadID: threadID, Body: event.Payload.Body, Actor: actor,
		})
	case lifecycle.EventThreadReopen:
		updated, err = s.threads.ReopenThread(r.Context(), coordinator.VerifyThreadInput{
			ThreadID: threadID, Body: event.Payload.Body, Actor: actor,
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
	s.stepThreadEvent(w, r, principal, threadID, request.LeaseID, true,
		[]worker.JobRole{worker.RoleReviewer, worker.RoleVerifier},
		lifecycle.Event{
			Kind:     lifecycle.EventThreadComment,
			ThreadID: threadID,
			Payload:  lifecycle.EventPayload{Body: request.Body},
		},
		"reply_thread_failed", "thread not found")
}

func (s *projectServer) handleClaimThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.stepThreadEvent(w, r, principal, threadID, request.LeaseID, true, nil,
		lifecycle.Event{
			Kind:     lifecycle.EventThreadClaimed,
			ThreadID: threadID,
			Payload: lifecycle.EventPayload{
				ThreadKind:     coordinator.ReviewClaimKind(strings.TrimSpace(request.Kind)),
				Body:           request.Body,
				ClaimCommitSHA: request.ClaimCommitSHA,
			},
		},
		"claim_thread_failed", "thread not found or not claimable")
}

func (s *projectServer) handleCertifyThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadCommentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.stepThreadEvent(w, r, principal, threadID, request.LeaseID, false,
		[]worker.JobRole{worker.RoleVerifier},
		lifecycle.Event{
			Kind:     lifecycle.EventThreadCertify,
			ThreadID: threadID,
			Payload:  lifecycle.EventPayload{Body: request.Body},
		},
		"certify_thread_failed", "thread not found or not certifiable")
}

func (s *projectServer) handleReopenThread(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, threadID string) {
	var request threadCommentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.stepThreadEvent(w, r, principal, threadID, request.LeaseID, false,
		[]worker.JobRole{worker.RoleVerifier},
		lifecycle.Event{
			Kind:     lifecycle.EventThreadReopen,
			ThreadID: threadID,
			Payload:  lifecycle.EventPayload{Body: request.Body},
		},
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

func (s *projectServer) handleRunReview(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.checkConfigs == nil || s.sessions == nil || s.checks == nil {
		writeError(w, http.StatusInternalServerError, "review_unavailable", "review services are not configured")
		return
	}
	task, err := s.tasks.GetTask(r.Context(), taskID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "get_task_failed", err.Error())
		return
	}
	change, ok, err := s.sessions.ReadyUnmergedChangeForTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_ready_change_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "review_run_failed", "task has no ready unmerged change")
		return
	}
	scheduled, err := s.checkConfigs.ScheduleReviewRound(r.Context(), coordinator.ScheduleReviewRoundInput{
		Task:   task,
		Change: change,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "schedule_review_failed", err.Error())
		return
	}
	checks, err := s.checks.ListChecks(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "list_checks_failed", err.Error())
		return
	}
	reviewState, err := s.checks.ReviewState(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "review_state_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, reviewRunResponse{
		Change:      change,
		Scheduled:   scheduled,
		Checks:      checks,
		ReviewState: reviewState,
	})
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

	check, err := s.checks.ReportCheck(r.Context(), coordinator.ReportCheckInput{
		TaskID: taskID, Name: name,
		Kind: coordinator.CheckKind(strings.TrimSpace(request.Kind)), Required: request.Required,
		Verdict: coordinator.CheckVerdict(strings.TrimSpace(request.Verdict)), ExitCode: request.ExitCode,
		Details: request.Details, SourceJobID: request.SourceJobID, Reporter: checkReporter(request, principal),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "report_check_failed", err.Error())
		return
	}
	if s.workflowExecutor != nil {
		if err := s.workflowExecutor.Tick(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "advance_workflow_failed", err.Error())
			return
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

func (s *projectServer) lifecycleEvent(r *http.Request, principal coordinator.Principal, ev lifecycle.Event) lifecycle.Event {
	if ev.Actor.Scope == "" {
		ev.Actor = principal
	}
	ev.Audit = s.lifecycleAudit(r, principal, ev)
	return ev
}

func (s *projectServer) lifecycleAudit(r *http.Request, principal coordinator.Principal, ev lifecycle.Event) lifecycle.EventAudit {
	return lifecycle.EventAudit{
		Method:       r.Method,
		Path:         r.URL.Path,
		Principal:    principal.Actor(),
		ProjectID:    s.project.ID,
		ProjectName:  s.project.Name,
		TaskID:       strings.TrimSpace(ev.TaskID),
		ChangeID:     strings.TrimSpace(ev.ChangeID),
		ThreadID:     strings.TrimSpace(ev.ThreadID),
		SessionID:    strings.TrimSpace(ev.SessionID),
		UserAgent:    strings.TrimSpace(r.UserAgent()),
		WebSessionID: strings.TrimSpace(principal.WebSessionID),
	}
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
	if job.TaskID == nil || strings.TrimSpace(*job.TaskID) != taskID {
		return errors.New("worker job does not belong to the thread task")
	}
	if job.ChangeID != nil && strings.TrimSpace(*job.ChangeID) != changeID {
		return errors.New("worker job does not belong to the thread change")
	}

	return nil
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
