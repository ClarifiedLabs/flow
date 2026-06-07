package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const (
	protocolHeader           = contract.ProtocolHeader
	idempotencyHeader        = contract.IdempotencyHeader
	authScheme               = contract.AuthScheme
	defaultOwnerSubject      = "owner"
	defaultHookSubject       = "hook"
	defaultLeaseSeconds      = 60
	maxClaimWaitSeconds      = 30
	defaultTerminalAccessTTL = 10 * time.Minute
	terminalAccessCookie     = "flow_terminal_access"
	defaultWebBootstrapTTL   = 10 * time.Minute
	defaultWebSessionTTL     = 12 * time.Hour
	webSessionCookie         = "flow_ui_session"
	webCSRFCookie            = "flow_ui_csrf"
	webCSRFHeader            = "X-Flow-CSRF"
	webAPIPrefix             = "/ui/api"
	terminalSandboxCSP       = "sandbox allow-scripts allow-same-origin allow-forms allow-downloads allow-modals"
)

type ServerOptions struct {
	// Registry owns the global database services and the open project
	// bundles. It is required.
	Registry *Registry

	OwnerToken      string
	HookToken       string
	WorkerJoinToken string
	ProtocolVersion string
}

type Server struct {
	registry        *Registry
	credentials     *coordinator.CredentialService
	webSessions     *coordinator.WebSessionService
	ownerToken      string
	hookToken       string
	workerJoinToken string
	protocolVersion string
}

func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Registry == nil {
		return nil, errors.New("project registry is required")
	}
	if strings.TrimSpace(opts.ProtocolVersion) == "" {
		opts.ProtocolVersion = config.DefaultProtocolVersion
	}

	return &Server{
		registry:        opts.Registry,
		credentials:     opts.Registry.Credentials(),
		webSessions:     opts.Registry.WebSessions(),
		ownerToken:      strings.TrimSpace(opts.OwnerToken),
		hookToken:       strings.TrimSpace(opts.HookToken),
		workerJoinToken: strings.TrimSpace(opts.WorkerJoinToken),
		protocolVersion: opts.ProtocolVersion,
	}, nil
}

// Registry exposes the project registry so the daemon can run per-project
// background work (lifecycle ticks, git event consumption, lease sweeps).
func (s *Server) Registry() *Registry {
	return s.registry
}

// projectServer is the per-request view of the server scoped to one project.
// It carries the project bundle's services under the same field names the
// handlers always used; everything coordinator-global is reached through the
// embedded Server.
type projectServer struct {
	*Server
	project      coordinator.Project
	issues       *coordinator.IssueService
	checks       *coordinator.CheckService
	threads      *coordinator.ThreadService
	sessions     *coordinator.SessionService
	transcripts  *coordinator.TranscriptStore
	attachments  *coordinator.IssueAttachmentStore
	status       *coordinator.StatusService
	reconciler   *coordinator.ReconcileService
	checkConfigs *coordinator.CheckConfigService
	merges       *coordinator.MergeService
	transitions  *coordinator.TransitionService
	gitEvents    *coordinator.GitEventService
	workers      *worker.Service
	engine       *lifecycle.Engine
}

func (s *Server) forBundle(bundle *ProjectBundle) *projectServer {
	return &projectServer{
		Server:       s,
		project:      bundle.Project,
		issues:       bundle.Issues,
		checks:       bundle.Checks,
		threads:      bundle.Threads,
		sessions:     bundle.Sessions,
		transcripts:  bundle.Transcripts,
		attachments:  bundle.Attachments,
		status:       bundle.Status,
		reconciler:   bundle.Reconciler,
		checkConfigs: bundle.CheckConfigs,
		merges:       bundle.Merges,
		transitions:  bundle.Transitions,
		gitEvents:    bundle.GitEvents,
		workers:      bundle.Queue,
		engine:       bundle.Engine,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Debug("flow api server request", "method", r.Method, "path", r.URL.Path)
	w.Header().Set(protocolHeader, s.protocolVersion)
	if r.URL.Path == "/v1/health" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if s.serveGitHTTPRequest(w, r) {
		return
	}

	if err := s.checkProtocol(r); err != nil {
		writeError(w, http.StatusBadRequest, "protocol_mismatch", err.Error())
		return
	}

	if s.serveWebAPIRequest(w, r) {
		return
	}

	if s.serveWebRequest(w, r) {
		return
	}

	if s.serveTerminalBrowserRequest(w, r) {
		return
	}

	if r.URL.Path == "/v1/workers/join" {
		s.handleJoinWorker(w, r)
		return
	}

	principal, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}

	if s.shouldUseIdempotency(r, principal) {
		s.serveIdempotent(w, r, principal)
		return
	}

	s.dispatch(w, r, principal)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if r.URL.Path == "/v1/ui/bootstrap" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleWebBootstrap(w, r)
		return
	}

	if r.URL.Path == "/v1/projects" {
		s.handleProjectsCollection(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/harnesses" {
		s.handleHarnesses(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/projects/") {
		s.handleProjectScopedPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/console" {
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleConsole(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/git/events/drain" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleDrainGitEventsByExchange(w, r)
		return
	}

	if r.URL.Path == "/v1/sidebar" {
		s.handleSidebar(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/workers" {
		s.handleWorkersDiagnostics(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/reconcile" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleReconcile(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/workers/") {
		s.handleWorkerPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/jobs" || strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
		s.handleJobsPath(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		sessionID := pathResourceID(r.URL.Path, "/v1/sessions/")
		ps, ok := s.bundleForSession(r.Context(), principal, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		ps.handleSessionPath(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/changes/") {
		resourceID := pathResourceID(r.URL.Path, "/v1/changes/")
		// The /v1/changes/{id}/checks subroute addresses checks by issue id;
		// every other changes subroute addresses by change id.
		if changesSubpathIsChecks(r.URL.Path) {
			ps, ok := s.bundleForChangeIssue(r.Context(), principal, resourceID)
			if !ok {
				writeError(w, http.StatusNotFound, "issue_not_found", "issue not found")
				return
			}
			ps.handleChangePath(w, r, principal)
			return
		}
		ps, ok := s.bundleForChange(r.Context(), principal, resourceID)
		if !ok {
			writeError(w, http.StatusNotFound, "change_not_found", "change not found")
			return
		}
		ps.handleChangePath(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/threads/") {
		threadID := pathResourceID(r.URL.Path, "/v1/threads/")
		ps, ok := s.bundleForThread(r.Context(), principal, threadID)
		if !ok {
			writeError(w, http.StatusNotFound, "thread_not_found", "thread not found")
			return
		}
		ps.handleThreadPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/issues" {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "issue read requires owner, session, or console token")
				return
			}
			s.handleListIssuesAggregate(w, r, principal)
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "issue creation requires owner, session, or console token")
				return
			}
			ps, err := s.implicitProjectServer(principal)
			if err != nil {
				writeProjectResolveError(w, err)
				return
			}
			ps.handleCreateIssue(w, r, principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/issues/") {
		// Issue ids are only unique within a project; unscoped issue routes
		// work for principals with an implicit project (session tokens, or a
		// coordinator with exactly one project). Everything else must use
		// /v1/projects/{project}/issues/...
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleIssuePath(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/board" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "board read requires owner, session, or console token")
			return
		}
		s.handleBoardAggregate(w, r, principal)
		return
	}

	if r.URL.Path == "/v1/done" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "done read requires owner, session, or console token")
			return
		}
		s.handleDoneAggregate(w, r, principal)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}
