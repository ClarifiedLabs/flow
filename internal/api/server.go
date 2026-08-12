package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
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

	OwnerToken string
	HookToken  string
}

type Server struct {
	registry        *Registry
	credentials     *coordinator.CredentialService
	webSessions     *coordinator.WebSessionService
	workerTerminals *coordinator.WorkerTerminalService
	gitWriteGates   sync.Map
	ownerToken      string
	hookToken       string
}

func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Registry == nil {
		return nil, errors.New("project registry is required")
	}
	return &Server{
		registry:        opts.Registry,
		credentials:     opts.Registry.Credentials(),
		webSessions:     opts.Registry.WebSessions(),
		workerTerminals: coordinator.NewWorkerTerminalService(),
		ownerToken:      strings.TrimSpace(opts.OwnerToken),
		hookToken:       strings.TrimSpace(opts.HookToken),
	}, nil
}

// Registry exposes the project registry so the daemon can run per-project
// background work (lifecycle ticks, git event consumption, lease sweeps).
func (s *Server) Registry() *Registry {
	return s.registry
}

// gitWriteGate serializes terminal session decisions and convergence review
// with HTTP receive-pack requests for one project. Writers hold a read lock for
// their entire request; terminal/final decisions take the exclusive lock, drain
// admitted writes, then revalidate state before committing.
func (s *Server) gitWriteGate(projectID string) *sync.RWMutex {
	value, _ := s.gitWriteGates.LoadOrStore(strings.TrimSpace(projectID), &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

func (s *projectServer) drainGitWrites() func() {
	if s == nil || s.Server == nil {
		return func() {}
	}
	gate := s.gitWriteGate(s.project.ID)
	gate.Lock()
	return gate.Unlock
}

// projectServer is the per-request view of the server scoped to one project.
// It carries the project bundle's services under the same field names the
// handlers always used; everything coordinator-global is reached through the
// embedded Server.
type projectServer struct {
	*Server
	project           coordinator.Project
	workItems         *coordinator.WorkItemService
	epics             *coordinator.EpicService
	containers        *coordinator.ContainerService
	tasks             *coordinator.TaskService
	features          *coordinator.FeatureService
	checks            *coordinator.CheckService
	threads           *coordinator.ThreadService
	sessions          *coordinator.SessionService
	transcripts       *coordinator.TranscriptStore
	attachments       *coordinator.TaskAttachmentStore
	status            *coordinator.StatusService
	reconciler        *coordinator.ReconcileService
	agentDefs         *coordinator.AgentDefService
	flows             *coordinator.FlowService
	workflowRuns      *coordinator.WorkflowRunService
	workflowArtifacts *coordinator.WorkflowArtifactService
	workflowExecutor  *coordinator.WorkflowExecutor
	checkConfigs      *coordinator.CheckConfigService
	merges            *coordinator.MergeService
	gitEvents         *coordinator.GitEventService
	eventLog          *coordinator.EventLogService
	history           *coordinator.HistoryCaptureService
	workers           *worker.Service
}

func (s *Server) forBundle(bundle *ProjectBundle) *projectServer {
	if bundle.Sessions != nil {
		projectID := bundle.Project.ID
		bundle.Sessions.SetGitWriteProjectionFence(func(operation func() error) error {
			gate := s.gitWriteGate(projectID)
			gate.Lock()
			defer gate.Unlock()
			return operation()
		})
	}
	return &projectServer{
		Server:            s,
		project:           bundle.Project,
		workItems:         bundle.WorkItems,
		epics:             bundle.Epics,
		containers:        bundle.Containers,
		tasks:             bundle.Tasks,
		features:          bundle.Features,
		checks:            bundle.Checks,
		threads:           bundle.Threads,
		sessions:          bundle.Sessions,
		transcripts:       bundle.Transcripts,
		attachments:       bundle.Attachments,
		status:            bundle.Status,
		reconciler:        bundle.Reconciler,
		agentDefs:         bundle.AgentDefs,
		flows:             bundle.Flows,
		workflowRuns:      bundle.WorkflowRuns,
		workflowArtifacts: bundle.WorkflowArtifacts,
		workflowExecutor:  bundle.WorkflowExecutor,
		checkConfigs:      bundle.CheckConfigs,
		merges:            bundle.Merges,
		gitEvents:         bundle.GitEvents,
		eventLog:          bundle.EventLog,
		history:           bundle.HistoryCaptures,
		workers:           bundle.Queue,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Debug("flow api server request", "method", r.Method, "path", r.URL.Path)
	w.Header().Set(protocolHeader, contract.ProtocolVersion)
	if r.URL.Path == "/v2/health" {
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

	principal, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	if err := s.authorizeCapacityWorkerRequest(r.Context(), r, principal); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}

	if s.shouldUseIdempotency(r, principal) {
		s.serveIdempotent(w, r, principal)
		return
	}

	s.dispatch(w, r, principal)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if r.URL.Path == "/v2/ui/bootstrap" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleWebBootstrap(w, r)
		return
	}

	if r.URL.Path == "/v2/projects" {
		s.handleProjectsCollection(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/global/agent-defs" || strings.HasPrefix(r.URL.Path, "/v2/global/agent-defs/") {
		s.handleGlobalAgentDefsPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/harnesses" {
		s.handleHarnesses(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/projects/") {
		s.handleProjectScopedPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/console" {
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleConsole(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/git/events/drain" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleDrainGitEventsByExchange(w, r)
		return
	}

	if r.URL.Path == "/v2/sidebar" {
		s.handleSidebar(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/history/captures" || strings.HasPrefix(r.URL.Path, "/v2/history/captures/") {
		if principal.Scope == coordinator.TokenScopeWorker {
			s.handleWorkerHistoryPath(w, r, principal)
			return
		}
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleHistoryPath(w, requestWithPath(r, r.URL.Path), principal)
		return
	}

	if r.URL.Path == "/v2/workers" {
		s.handleWorkersDiagnostics(w, r, principal)
		return
	}

	if r.URL.Path == provisionerAssignmentsPath || strings.HasPrefix(r.URL.Path, provisionerAssignmentsPath+"/") {
		s.handleProvisionerAssignmentsPath(w, r, principal)
		return
	}

	if r.URL.Path == provisionerCapacityDemandPath || r.URL.Path == provisionerCapacitySlotsPath || strings.HasPrefix(r.URL.Path, provisionerCapacitySlotsPath+"/") {
		s.handleProvisionerCapacityPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/reconcile" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleReconcile(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/workers/") {
		s.dispatchWorkerPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/jobs" || strings.HasPrefix(r.URL.Path, "/v2/jobs/") {
		s.handleJobsPath(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/sessions/") {
		sessionID := pathResourceID(r.URL.Path, "/v2/sessions/")
		ps, ok := s.bundleForSession(r.Context(), principal, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		ps.handleSessionPath(w, r, principal)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/changes/") {
		resourceID := pathResourceID(r.URL.Path, "/v2/changes/")
		// The /v2/changes/{id}/checks subroute addresses checks by task id;
		// every other changes subroute addresses by change id.
		if changesSubpathIsChecks(r.URL.Path) {
			ps, ok := s.bundleForChangeTask(r.Context(), principal, resourceID)
			if !ok {
				writeError(w, http.StatusNotFound, "task_not_found", "task not found")
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

	if strings.HasPrefix(r.URL.Path, "/v2/threads/") {
		threadID := pathResourceID(r.URL.Path, "/v2/threads/")
		ps, ok := s.bundleForThread(r.Context(), principal, threadID)
		if !ok {
			writeError(w, http.StatusNotFound, "thread_not_found", "thread not found")
			return
		}
		ps.handleThreadPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/agent-defs" || strings.HasPrefix(r.URL.Path, "/v2/agent-defs/") {
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleAgentDefsPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/flows" || strings.HasPrefix(r.URL.Path, "/v2/flows/") {
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleFlowsPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/tasks" {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "task read requires owner, session, or console token")
				return
			}
			s.handleListTasksAggregate(w, r, principal)
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "task creation requires owner, session, or console token")
				return
			}
			ps, err := s.implicitProjectServer(principal)
			if err != nil {
				writeProjectResolveError(w, err)
				return
			}
			ps.handleCreateTask(w, r, principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/tasks/") {
		taskID := pathResourceID(r.URL.Path, "/v2/tasks/")
		ps, err := s.projectServerForTask(r.Context(), principal, taskID)
		if err != nil {
			if errors.Is(err, errProjectForbidden) {
				writeProjectResolveError(w, err)
			} else {
				writeError(w, http.StatusNotFound, "task_not_found", "task not found")
			}
			return
		}
		ps.handleTaskPath(w, r, principal)
		return
	}

	if r.URL.Path == "/v2/board" {
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

	if r.URL.Path == "/v2/search" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "search requires owner, session, or console token")
			return
		}
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		ps.handleSearchTasks(w, r)
		return
	}

	if r.URL.Path == "/v2/events" || r.URL.Path == "/v2/events/stream" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "event read requires owner, session, or console token")
			return
		}
		ps, err := s.implicitProjectServer(principal)
		if err != nil {
			writeProjectResolveError(w, err)
			return
		}
		if r.URL.Path == "/v2/events/stream" {
			ps.handleEventStream(w, r)
			return
		}
		ps.handleEvents(w, r)
		return
	}

	if r.URL.Path == "/v2/done" {
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

	if r.URL.Path == "/v2/stats/completions" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			writeError(w, http.StatusForbidden, "forbidden", "completion stats read requires owner, session, or console token")
			return
		}
		s.handleCompletionStatsAggregate(w, r, principal)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}
