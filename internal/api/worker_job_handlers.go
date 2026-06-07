package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *Server) handleJoinWorker(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if strings.TrimSpace(s.workerJoinToken) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, authScheme) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, authScheme))
	if token == "" || !tokenMatches(token, s.workerJoinToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
		return
	}

	var request joinWorkerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	workerID := strings.TrimSpace(request.WorkerID)
	if workerID == "" {
		writeError(w, http.StatusBadRequest, "join_worker_failed", "worker_id is required")
		return
	}
	if s.credentials == nil {
		writeError(w, http.StatusInternalServerError, "join_worker_failed", "credentials are unavailable")
		return
	}
	workerToken, err := s.credentials.ReplaceSubjectToken(r.Context(), coordinator.CredentialInput{
		Scope:   coordinator.TokenScopeWorker,
		Subject: workerID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "join_worker_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, joinWorkerResponse{
		WorkerID: workerID,
		Token:    workerToken,
	})
}

func (s *Server) handleWorkerPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if r.URL.Path == "/v1/workers/reap-jobs" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "worker token is required")
			return
		}
		s.handleWorkerReapJobs(w, r, principal)
		return
	}

	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !scopeAllowed(principal, coordinator.TokenScopeWorker) {
		writeError(w, http.StatusForbidden, "forbidden", "worker token is required")
		return
	}

	switch r.URL.Path {
	case "/v1/workers/register":
		s.handleRegisterWorker(w, r, principal)
	case "/v1/workers/heartbeat":
		s.handleHeartbeatWorker(w, r, principal)
	case "/v1/workers/claim":
		s.handleClaimWorkerJob(w, r, principal)
	case "/v1/workers/running":
		s.handleMarkJobRunning(w, r, principal)
	case "/v1/workers/renew":
		s.handleRenewLease(w, r, principal)
	case "/v1/workers/status":
		s.handleWorkerJobStatus(w, r, principal)
	case "/v1/workers/release":
		s.handleReleaseLease(w, r, principal)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *Server) handleWorkersDiagnostics(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
		return
	}

	workers, err := s.registry.Directory().ListWorkers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_workers_failed", err.Error())
		return
	}
	jobs, leases, ok := collectJobsAndLeases(w, r, s.registry.All())
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, workersResponse{
		Workers:     workers,
		Diagnostics: uiWorkerDiagnosticsFromLeases(workers, leases, time.Now().UTC()),
		Queue:       uiQueueSummaryFromJobs(jobs),
	})
}

func (s *Server) handleRegisterWorker(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request registerWorkerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	workerID, err := workerIDForPrincipal(request.ID, principal)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	heartbeatTTL, err := nonNegativeSeconds(request.HeartbeatTTLSeconds, "heartbeat_ttl_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_heartbeat_ttl", err.Error())
		return
	}

	registered, err := s.registry.Directory().RegisterWorker(r.Context(), worker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  request.Labels,
		Taints:                  request.Taints,
		HarnessModels:           request.HarnessModels,
		CapacityPersistentAgent: request.CapacityPersistentAgent,
		CapacityEphemeral:       request.CapacityEphemeral,
		HeartbeatTTL:            heartbeatTTL,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "register_worker_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, workerResponse{Worker: registered})
}

func (s *Server) handleWorkerReapJobs(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if _, err := workerIDForPrincipal("", principal); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}

	// The reaper compares local tmux sessions against every job the
	// coordinator knows, so the list spans all projects.
	response := []reapJob{}
	for _, bundle := range s.registry.All() {
		jobs, err := bundle.Queue.ListJobs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_jobs_failed", err.Error())
			return
		}
		for _, job := range jobs {
			response = append(response, reapJob{
				ID:    job.ID,
				State: job.State,
			})
		}
	}
	writeJSON(w, http.StatusOK, reapJobsResponse{Jobs: response})
}

func (s *Server) handleHeartbeatWorker(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request heartbeatWorkerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	workerID, err := workerIDForPrincipal(request.WorkerID, principal)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	heartbeatTTL, err := nonNegativeSeconds(request.HeartbeatTTLSeconds, "heartbeat_ttl_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_heartbeat_ttl", err.Error())
		return
	}

	heartbeat, err := s.registry.Directory().HeartbeatWorker(r.Context(), workerID, heartbeatTTL)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "worker_not_found", "worker not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "heartbeat_worker_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, workerResponse{Worker: heartbeat})
}

func (s *Server) handleClaimWorkerJob(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request claimJobRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	workerID, err := workerIDForPrincipal(request.WorkerID, principal)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	leaseDuration, err := positiveSecondsOrDefault(request.LeaseDurationSeconds, defaultLeaseSeconds, "lease_duration_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_lease_duration", err.Error())
		return
	}
	waitDuration, err := claimWaitDuration(request.WaitSeconds)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_wait", err.Error())
		return
	}

	deadline := time.Now().UTC().Add(waitDuration)
	for {
		if err := s.sweepExpiredLeases(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "sweep_expired_leases_failed", err.Error())
			return
		}
		claimed, ok, err := s.registry.Claim(r.Context(), worker.ClaimInput{
			WorkerID:      workerID,
			Buckets:       request.Buckets,
			LeaseDuration: leaseDuration,
		})
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "worker_not_found", "worker not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "claim_job_failed", err.Error())
			return
		}
		if ok {
			job := claimed.Job
			lease := claimed.Lease
			writeJSON(w, http.StatusOK, claimJobResponse{
				Claimed:   true,
				ProjectID: claimed.ProjectID,
				Job:       &job,
				Lease:     &lease,
			})
			return
		}
		if waitDuration <= 0 {
			writeJSON(w, http.StatusOK, claimJobResponse{Claimed: false})
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, claimJobResponse{Claimed: false})
			return
		}
		sleep := 250 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// resolveLease validates a worker request's lease_id, finds the project bundle
// that owns the lease, and verifies the caller owns it. It writes the error
// response and returns ok=false on any failure.
func (s *Server) resolveLease(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, requestedLeaseID string) (*projectServer, string, bool) {
	leaseID := strings.TrimSpace(requestedLeaseID)
	if leaseID == "" {
		writeError(w, http.StatusBadRequest, "invalid_lease", "lease_id is required")
		return nil, "", false
	}
	ps, found := s.bundleForLease(r.Context(), principal, leaseID)
	if !found {
		writeLeaseAuthError(w, sql.ErrNoRows)
		return nil, "", false
	}
	if err := ps.checkLeaseOwner(r, principal, leaseID); err != nil {
		writeLeaseAuthError(w, err)
		return nil, "", false
	}

	return ps, leaseID, true
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request renewLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	leaseDuration, err := positiveSecondsOrDefault(request.LeaseDurationSeconds, defaultLeaseSeconds, "lease_duration_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_lease_duration", err.Error())
		return
	}
	ps, leaseID, ok := s.resolveLease(w, r, principal, request.LeaseID)
	if !ok {
		return
	}

	renewed, err := ps.workers.RenewLease(r.Context(), leaseID, leaseDuration)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "renew_lease_failed", "lease is not renewable")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "renew_lease_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, leaseResponse{Lease: renewed})
}

func (s *Server) handleWorkerJobStatus(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request workerJobStatusRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ps, leaseID, ok := s.resolveLease(w, r, principal, request.LeaseID)
	if !ok {
		return
	}

	lease, err := ps.workers.GetLease(r.Context(), leaseID)
	if err != nil {
		writeLeaseAuthError(w, err)
		return
	}
	job, err := ps.workers.GetJob(r.Context(), lease.JobID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "worker_status_failed", err.Error())
		return
	}

	response := workerJobStatusResponse{
		ProjectID: ps.project.ID,
		Job:       job,
		Lease:     lease,
	}
	session, ok, err := ps.sessions.LatestSessionForJob(r.Context(), job.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "worker_status_failed", err.Error())
		return
	}
	if ok {
		response.Session = &session
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleMarkJobRunning(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request markJobRunningRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ps, leaseID, ok := s.resolveLease(w, r, principal, request.LeaseID)
	if !ok {
		return
	}

	job, err := ps.workers.MarkJobRunning(r.Context(), leaseID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "mark_running_failed", err.Error())
		return
	}

	response := jobResponse{Job: job, ProjectID: ps.project.ID}
	if job.Role == worker.RoleAuthor {
		sessionResult, err := ps.sessions.StartAuthorSession(r.Context(), coordinator.StartAuthorSessionInput{
			JobID:    job.ID,
			LeaseID:  leaseID,
			WorkerID: strings.TrimSpace(principal.Subject),
			Harness:  sessionHarnessForJob(job),
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "start_session_failed", err.Error())
			return
		}
		response.Session = &sessionResult.Session
		response.Change = &sessionResult.Change
		response.SessionToken = sessionResult.Token
	} else if job.Role == worker.RoleConsole {
		sessionResult, err := ps.sessions.StartConsoleSession(r.Context(), coordinator.StartConsoleSessionInput{
			JobID:    job.ID,
			LeaseID:  leaseID,
			WorkerID: strings.TrimSpace(principal.Subject),
			Harness:  sessionHarnessForJob(job),
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "start_session_failed", err.Error())
			return
		}
		response.Session = &sessionResult.Session
		response.SessionToken = sessionResult.Token
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request releaseLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ps, leaseID, ok := s.resolveLease(w, r, principal, request.LeaseID)
	if !ok {
		return
	}
	lease, err := ps.workers.GetLease(r.Context(), leaseID)
	if err != nil {
		writeLeaseAuthError(w, err)
		return
	}
	job, err := ps.workers.GetJob(r.Context(), lease.JobID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "release_lease_failed", err.Error())
		return
	}
	if job.Role == worker.RoleAuthor {
		writeError(w, http.StatusBadRequest, "release_lease_failed", "author session leases are released by flow ready")
		return
	}
	if job.Role == worker.RoleConsole {
		writeError(w, http.StatusBadRequest, "release_lease_failed", "console leases are released via the console endpoint")
		return
	}

	job, err = ps.workers.ReleaseLease(r.Context(), leaseID, worker.JobState(strings.TrimSpace(request.FinalState)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "release_lease_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, jobResponse{Job: job})
}

func (s *Server) handleJobsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if r.URL.Path == "/v1/jobs" {
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleListJobsAggregate(w, r, principal)
		case http.MethodPost:
			s.handleEnqueueJobGlobal(w, r, principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	jobID := parts[0]
	ps, found := s.bundleForJob(r.Context(), principal, jobID)
	if !found {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}

	if len(parts) >= 2 && parts[1] == "terminal" {
		switch r.Method {
		case http.MethodGet:
			if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
				return
			}
			ps.handleJobTerminalProxy(w, r, jobID, parts[2:])
		case http.MethodPost:
			if len(parts) != 2 {
				writeError(w, http.StatusNotFound, "not_found", "resource not found")
				return
			}
			if !scopeAllowed(principal, coordinator.TokenScopeWorker) {
				writeError(w, http.StatusForbidden, "forbidden", "worker token is required")
				return
			}
			ps.handleJobTerminalRegister(w, r, principal, jobID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "terminal-token" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		ps.handleJobTerminalAccess(w, r, jobID)
		return
	}
	if len(parts) == 2 && parts[1] == "transcript" {
		switch r.Method {
		case http.MethodPut:
			// A worker uploading a job transcript proves a live lease on that
			// job, mirroring the worker check-report scope; owner tokens may
			// upload directly.
			if !scopeAllowed(principal, coordinator.TokenScopeWorker, coordinator.TokenScopeOwner) {
				writeError(w, http.StatusForbidden, "forbidden", "worker or owner token is required")
				return
			}
			ps.handleJobTranscriptUpload(w, r, principal, jobID)
		case http.MethodGet:
			if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
				return
			}
			ps.handleJobTranscriptDownload(w, r, jobID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
		return
	}
	if len(parts) == 2 && parts[1] == "attach" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		ps.handleJobAttach(w, r, jobID)
		return
	}

	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ps.handleGetJob(w, r, jobID)
}

func (s *Server) handleListJobsAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	response := jobsResponse{Jobs: []worker.Job{}, Diagnostics: map[string]uiJobDiagnostics{}}
	for _, bundle := range bundles {
		ps := s.forBundle(bundle)
		jobs, err := ps.workers.ListJobs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_jobs_failed", err.Error())
			return
		}
		diagnostics, err := ps.buildUIJobDiagnostics(r.Context(), jobs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "job_diagnostics_failed", err.Error())
			return
		}
		response.Jobs = append(response.Jobs, jobs...)
		for id, diagnostic := range diagnostics {
			response.Diagnostics[id] = diagnostic
		}
	}

	// Each project bundle returns its jobs ordered by created_at, so a job from
	// a later-registered project ends up after an earlier project's jobs no
	// matter how recently it was updated. Sort the combined list globally by
	// updated_at (newest first) with id as a stable tiebreaker so the jobs page
	// reflects recency rather than registration order.
	sortJobsByUpdatedDesc(response.Jobs)

	writeJSON(w, http.StatusOK, response)
}

// sortJobsByUpdatedDesc orders jobs by updated_at descending (most recently
// updated first) with id descending as a stable tiebreaker. It sorts in place.
func sortJobsByUpdatedDesc(jobs []worker.Job) {
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].UpdatedAt.Equal(jobs[j].UpdatedAt) {
			return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt)
		}
		return jobs[i].ID > jobs[j].ID
	})
}

// handleEnqueueJobGlobal routes a manual job enqueue to its project: the
// request must name the project unless the coordinator has exactly one.
func (s *Server) handleEnqueueJobGlobal(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	projectRef := strings.TrimSpace(r.URL.Query().Get("project"))
	var ps *projectServer
	if projectRef != "" {
		bundle, err := s.resolveProjectBundle(r.Context(), principal, projectRef)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps = s.forBundle(bundle)
	} else {
		resolved, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps = resolved
	}

	ps.handleEnqueueJob(w, r)
}

func (s *projectServer) handleEnqueueJob(w http.ResponseWriter, r *http.Request) {
	var request enqueueJobRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	issueID := trimmedStringPointer(request.IssueID)
	changeID := trimmedStringPointer(request.ChangeID)
	role := worker.JobRole(strings.TrimSpace(request.Role))
	if role == worker.RoleAuthor && s.sessions != nil {
		if issueID == nil {
			writeError(w, http.StatusBadRequest, "enqueue_job_failed", "author jobs require issue id")
			return
		}
		result, err := s.sessions.EnsureAuthorJob(r.Context(), coordinator.EnsureAuthorJobInput{
			IssueID:  *issueID,
			Branch:   payloadString(request.Payload, "branch"),
			Base:     payloadString(request.Payload, "base"),
			Priority: request.Priority,
			Payload:  request.Payload,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "enqueue_job_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, jobResponse{Job: result.Job, Change: &result.Change})
		return
	}
	payload, err := s.jobPayloadWithReviewContext(r.Context(), role, issueID, request.Payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "enqueue_job_failed", err.Error())
		return
	}
	job, err := s.workers.EnqueueJob(r.Context(), worker.EnqueueJobInput{
		IssueID:        issueID,
		ChangeID:       changeID,
		Role:           role,
		CapacityBucket: worker.CapacityBucket(strings.TrimSpace(request.CapacityBucket)),
		Priority:       request.Priority,
		RunsOn:         request.RunsOn,
		Requires:       request.Requires,
		Size:           request.Size,
		Tolerations:    request.Tolerations,
		Payload:        payload,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "enqueue_job_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, jobResponse{Job: job})
}

func (s *projectServer) jobPayloadWithReviewContext(ctx context.Context, role worker.JobRole, issueID *string, payload map[string]any) (map[string]any, error) {
	if s.threads == nil || issueID == nil || (role != worker.RoleReviewer && role != worker.RoleVerifier) {
		return payload, nil
	}

	context, err := s.threads.ReviewContextForIssue(ctx, *issueID)
	if err != nil {
		return nil, fmt.Errorf("load review context: %w", err)
	}

	withContext := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		withContext[key] = value
	}
	withContext["review_context"] = context
	return withContext, nil
}

func (s *projectServer) handleGetJob(w http.ResponseWriter, r *http.Request, jobID string) {
	job, err := s.workers.GetJob(r.Context(), jobID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_job_failed", err.Error())
		return
	}
	diagnostics, err := s.buildUIJobDiagnostics(r.Context(), []worker.Job{job})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_diagnostics_failed", err.Error())
		return
	}

	response := jobResponse{Job: job}
	if diagnostic, ok := diagnostics[job.ID]; ok {
		response.Diagnostics = &diagnostic
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleJobAttach(w http.ResponseWriter, r *http.Request, jobID string) {
	job, err := s.workers.GetJob(r.Context(), jobID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_job_failed", err.Error())
		return
	}
	if job.State != worker.JobRunning {
		writeError(w, http.StatusBadRequest, "attach_job_failed", "job terminal is not live")
		return
	}

	leases, err := s.workers.ListLeases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_leases_failed", err.Error())
		return
	}
	live := false
	now := time.Now().UTC()
	for _, lease := range leases {
		if lease.JobID != job.ID {
			continue
		}
		live = uiLeaseIsLive(lease, now)
		break
	}
	if !live {
		writeError(w, http.StatusBadRequest, "attach_job_failed", "job terminal is not live")
		return
	}

	tmuxSocketPath := ""
	if registered, err := s.sessions.JobTerminalTarget(r.Context(), job.ID); err == nil {
		tmuxSocketPath = registered.TmuxSocketPath
	}

	writeJSON(w, http.StatusOK, attachResponse{Attach: terminal.AttachInfoForJob(job.ID, tmuxSocketPath)})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var result coordinator.ReconcileResult
	for _, bundle := range s.registry.All() {
		projectResult, err := bundle.Reconciler.Reconcile(r.Context(), bundle.Project)
		if err != nil {
			writeError(w, http.StatusBadRequest, "reconcile_failed", err.Error())
			return
		}
		result.Merge(projectResult)
	}

	writeJSON(w, http.StatusOK, reconcileResponse{Result: result})
}

func (s *projectServer) checkLeaseOwner(r *http.Request, principal coordinator.Principal, leaseID string) error {
	if err := s.sweepExpiredLeases(r.Context()); err != nil {
		return err
	}
	lease, err := s.workers.GetLease(r.Context(), leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if lease.WorkerID != strings.TrimSpace(principal.Subject) {
		return errWorkerLeaseForbidden
	}

	return nil
}

// sweepExpiredLeases sweeps every project's leases and crashed author
// sessions; claim and diagnostics paths call it before reading queue state.
func (s *Server) sweepExpiredLeases(ctx context.Context) error {
	for _, bundle := range s.registry.All() {
		if _, err := bundle.Sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
			return fmt.Errorf("sweep project %s: %w", bundle.Project.ID, err)
		}
		if _, err := bundle.Sessions.ReconcileCrashedConsoleSessions(ctx); err != nil {
			return fmt.Errorf("sweep project %s console: %w", bundle.Project.ID, err)
		}
	}

	return nil
}

func (s *projectServer) projectSweepExpiredLeases(ctx context.Context) error {
	if _, err := s.sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		return err
	}
	_, err := s.sessions.ReconcileCrashedConsoleSessions(ctx)
	return err
}
