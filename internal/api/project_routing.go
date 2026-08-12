package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// scopedBundles returns the bundles a principal may see: project-bound tokens
// are pinned to their own project, everything else sees all open projects.
func (s *Server) scopedBundles(principal coordinator.Principal) []*ProjectBundle {
	if principal.IsProjectBound() {
		if bundle, ok := s.registry.Bundle(*principal.ProjectID); ok {
			return []*ProjectBundle{bundle}
		}
		return nil
	}

	return s.registry.All()
}

// resolveProjectBundle resolves an explicit project reference (id or name)
// for a principal, enforcing project-bound token confinement.
func (s *Server) resolveProjectBundle(ctx context.Context, principal coordinator.Principal, projectRef string) (*ProjectBundle, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		return nil, errors.New("project is required")
	}
	if principal.IsProjectBound() && *principal.ProjectID != projectRef {
		return nil, errProjectForbidden
	}
	if bundle, ok := s.registry.Bundle(projectRef); ok {
		return bundle, nil
	}

	// Fall back to a name lookup for human-friendly references.
	project, err := s.registry.Projects().GetByName(ctx, projectRef)
	if err != nil {
		return nil, errProjectNotFound
	}
	if principal.IsProjectBound() && *principal.ProjectID != project.ID {
		return nil, errProjectForbidden
	}
	if bundle, ok := s.registry.Bundle(project.ID); ok {
		return bundle, nil
	}

	return nil, errProjectNotFound
}

var (
	errProjectNotFound  = errors.New("project not found")
	errProjectForbidden = errors.New("project is not accessible with this token")
	errTaskNotFound     = errors.New("task not found")
)

func writeProjectResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errProjectForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, errProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "invalid_project", err.Error())
	}
}

// Bundle-by-id resolution: change, thread, session, job, and lease ids are
// random and unique across projects, so unscoped routes resolve their owning
// project by probing each bundle's database. Session principals only probe
// their own project.

// collectJobsAndLeases gathers every job and lease across the given bundles. On
// any store error it writes the response and returns ok=false.
func collectJobsAndLeases(w http.ResponseWriter, r *http.Request, bundles []*ProjectBundle) ([]worker.Job, []worker.Lease, bool) {
	var jobs []worker.Job
	var leases []worker.Lease
	for _, bundle := range bundles {
		bundleJobs, err := bundle.Queue.ListJobs(r.Context())
		if err != nil {
			writeInternalError(w, r, "list_jobs_failed", err)
			return nil, nil, false
		}
		jobs = append(jobs, bundleJobs...)
		bundleLeases, err := bundle.Queue.ListLeases(r.Context())
		if err != nil {
			writeInternalError(w, r, "list_leases_failed", err)
			return nil, nil, false
		}
		leases = append(leases, bundleLeases...)
	}

	return jobs, leases, true
}

func (s *Server) bundleForSession(ctx context.Context, principal coordinator.Principal, sessionID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Sessions.GetSession(ctx, sessionID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
}

func (s *Server) bundleForChange(ctx context.Context, principal coordinator.Principal, changeID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Sessions.GetChange(ctx, changeID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
}

// bundleForChangeTask resolves the owning bundle for the
// /v2/changes/{taskID}/checks subroute, whose leading path segment is an
// task id (checks are keyed by task) rather than a change id.
func (s *Server) bundleForChangeTask(ctx context.Context, principal coordinator.Principal, taskID string) (*projectServer, bool) {
	projectID, ok := coordinator.ProjectIDFromTaskID(taskID)
	if !ok {
		return nil, false
	}
	bundle, ok := s.registry.Bundle(projectID)
	if !ok || (principal.IsProjectBound() && *principal.ProjectID != projectID) {
		return nil, false
	}
	if _, err := bundle.Tasks.GetTask(ctx, taskID); err != nil {
		return nil, false
	}

	return s.forBundle(bundle), true
}

// projectServerForTask resolves a globally descriptive task ID to its owning
// project while preserving project-bound token confinement.
func (s *Server) projectServerForTask(ctx context.Context, principal coordinator.Principal, taskID string) (*projectServer, error) {
	projectID, ok := coordinator.ProjectIDFromTaskID(taskID)
	if !ok {
		return nil, errTaskNotFound
	}
	bundle, err := s.resolveProjectBundle(ctx, principal, projectID)
	if err != nil {
		if errors.Is(err, errProjectForbidden) {
			return nil, err
		}
		return nil, errTaskNotFound
	}
	if _, err := bundle.Tasks.GetTask(ctx, taskID); err != nil {
		return nil, errTaskNotFound
	}

	return s.forBundle(bundle), nil
}

func (s *Server) bundleForThread(ctx context.Context, principal coordinator.Principal, threadID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Threads.GetThread(ctx, threadID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
}

func (s *Server) bundleForJob(ctx context.Context, principal coordinator.Principal, jobID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Queue.GetJob(ctx, jobID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
}

func (s *Server) bundleForLease(ctx context.Context, principal coordinator.Principal, leaseID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Queue.GetLease(ctx, leaseID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
}

// changesSubpathIsChecks reports whether a /v2/changes/{id}/... path targets
// the checks subroute, whose leading segment is an task id.
func changesSubpathIsChecks(path string) bool {
	rest := strings.TrimPrefix(path, "/v2/changes/")
	_, sub, _ := strings.Cut(rest, "/")
	sub, _, _ = strings.Cut(sub, "/")
	return sub == "checks"
}

// pathResourceID extracts the first path segment after a prefix.
func pathResourceID(path string, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}

	return strings.TrimSpace(rest)
}

// implicitProjectServer resolves the project for unscoped project routes: a
// project-bound token is bound to its project, and a coordinator with exactly
// one open project needs no qualifier.
func (s *Server) implicitProjectServer(principal coordinator.Principal) (*projectServer, error) {
	if principal.IsProjectBound() {
		if bundle, ok := s.registry.Bundle(*principal.ProjectID); ok {
			return s.forBundle(bundle), nil
		}
		return nil, errProjectNotFound
	}
	bundles := s.registry.All()
	if len(bundles) == 1 {
		return s.forBundle(bundles[0]), nil
	}
	if len(bundles) == 0 {
		return nil, errProjectNotFound
	}

	return nil, errors.New("multiple projects are registered; use /v2/projects/{project}/tasks")
}

func (s *Server) handleProjectsCollection(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	switch r.Method {
	case http.MethodGet:
		if !requireScope(w, principal, "project read requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		s.handleListProjects(w, r, principal)
	case http.MethodPost:
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleCreateProject(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	}
}

func (s *Server) handleProjectScopedPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	rest := strings.TrimPrefix(r.URL.Path, "/v2/projects/")
	projectRef, sub, _ := strings.Cut(rest, "/")
	if strings.TrimSpace(projectRef) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	bundle, err := s.resolveProjectBundle(r.Context(), principal, projectRef)
	if err != nil {
		writeProjectResolveError(w, err)
		return
	}
	ps := s.forBundle(bundle)

	switch {
	case sub == "":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "project read requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		writeJSON(w, http.StatusOK, projectResponse{Project: uiProjectFromRegistry(bundle.Project)})
	case sub == "console":
		ps.handleConsole(w, r, principal)
	case sub == "tasks":
		switch r.Method {
		case http.MethodGet:
			if !ps.requireProjectReadAccess(w, r, principal) {
				return
			}
			ps.handleListTasks(w, requestWithPath(r, "/v2/tasks"))
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "task creation requires owner, session, or console token")
				return
			}
			ps.handleCreateTask(w, requestWithPath(r, "/v2/tasks"), principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
	case strings.HasPrefix(sub, "tasks/"):
		ps.handleTaskPath(w, requestWithPath(r, "/v2/"+sub), principal)
	case sub == "agent-defs" || strings.HasPrefix(sub, "agent-defs/"):
		ps.handleAgentDefsPath(w, requestWithPath(r, "/v2/"+sub), principal)
	case sub == "features" || strings.HasPrefix(sub, "features/"):
		ps.handleFeaturesPath(w, requestWithPath(r, "/v2/"+sub), principal)
		return
	case sub == "epics" || strings.HasPrefix(sub, "epics/"):
		ps.handleEpicsPath(w, requestWithPath(r, "/v2/"+sub), principal)
		return
	case sub == "work-items" || strings.HasPrefix(sub, "work-items/"):
		ps.handleWorkItemsPath(w, requestWithPath(r, "/v2/"+sub), principal)
		return
	case sub == "flows" || strings.HasPrefix(sub, "flows/"):
		ps.handleFlowsPath(w, requestWithPath(r, "/v2/"+sub), principal)
	case sub == "history/captures" || strings.HasPrefix(sub, "history/captures/"):
		ps.handleHistoryPath(w, requestWithPath(r, "/v2/"+sub), principal)
	case sub == "board":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "board read requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		ps.handleBoard(w, r, principal)
	case sub == "search":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "search requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		ps.handleSearchTasks(w, r)
	case sub == "events" || sub == "events/stream":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "event read requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		if sub == "events/stream" {
			ps.handleEventStream(w, r)
			return
		}
		ps.handleEvents(w, r)
	case sub == "git/events":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "hook token is required", coordinator.TokenScopeHook) {
			return
		}
		ps.handleGitEvents(w, r)
	case sub == "git/events/drain":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		ps.handleDrainGitEvents(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

type uiProject = contract.Project
type projectResponse = contract.ProjectResponse
type projectsResponse = contract.ProjectsResponse
type createProjectRequest = contract.CreateProjectRequest

func uiProjectFromRegistry(project coordinator.Project) uiProject {
	return uiProject{
		ID:           project.ID,
		Name:         project.Name,
		RepoPath:     project.RepoPath,
		BaseBranch:   project.BaseBranch,
		ExchangeName: project.ExchangeName,
		CreatedAt:    project.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	repoPath := strings.TrimSpace(r.URL.Query().Get("repo_path"))

	response := projectsResponse{Projects: []uiProject{}}
	for _, bundle := range s.scopedBundles(principal) {
		if repoPath != "" && bundle.Project.RepoPath != repoPath {
			continue
		}
		response.Projects = append(response.Projects, uiProjectFromRegistry(bundle.Project))
	}

	writeJSON(w, http.StatusOK, response)
}

// handleCreateProject registers a project. The server only touches its own
// data directory (project dir, database, bare exchange, hooks); the client
// owns every worktree-side step, so a future remote coordinator works the
// same way.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var request createProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// Re-registering the same repo path returns the existing project.
	repoPath := strings.TrimSpace(request.RepoPath)
	if repoPath != "" {
		if existing, err := s.registry.Projects().GetByRepoPath(r.Context(), repoPath); err == nil {
			writeJSON(w, http.StatusOK, projectResponse{Project: uiProjectFromRegistry(existing), Created: false})
			return
		}
	}

	project, err := s.registry.CreateProject(r.Context(), coordinator.Project{
		Name:         strings.TrimSpace(request.Name),
		RepoPath:     repoPath,
		BaseBranch:   strings.TrimSpace(request.BaseBranch),
		ExchangeName: strings.TrimSpace(request.ExchangeName),
	})
	if err != nil {
		if errors.Is(err, coordinator.ErrProjectRepoPathExists) {
			if existing, lookupErr := s.registry.Projects().GetByRepoPath(r.Context(), repoPath); lookupErr == nil {
				writeJSON(w, http.StatusOK, projectResponse{Project: uiProjectFromRegistry(existing), Created: false})
				return
			}
		}
		writeError(w, http.StatusBadRequest, "create_project_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, projectResponse{Project: uiProjectFromRegistry(project), Created: true})
}

// uiTaskWithProject decorates a task with project display metadata for
// aggregate responses.
type uiTaskWithProject struct {
	coordinator.Task
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
}

type aggregateTasksResponse struct {
	Tasks []uiTaskWithProject `json:"tasks"`
}

type projectBoardResponse struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	boardResponse
}

type aggregateBoardResponse struct {
	Boards []projectBoardResponse `json:"boards"`
}

type projectDoneResponse struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	doneResponse
}

type aggregateDoneResponse struct {
	Done []projectDoneResponse `json:"done"`
}

type sidebarResponse struct {
	Triage   int                      `json:"triage"`
	Feedback int                      `json:"feedback"`
	Merge    int                      `json:"merge"`
	Done     int                      `json:"done"`
	Board    uiSidebarBoardSummary    `json:"board"`
	Workers  uiSidebarWorkerSummary   `json:"workers"`
	Jobs     uiSidebarJobStateSummary `json:"jobs"`
}

type uiSidebarBoardSummary struct {
	Unscheduled int `json:"unscheduled"`
	Scheduled   int `json:"scheduled"`
	InProgress  int `json:"in_progress"`
	Blocked     int `json:"blocked"`
}

type uiSidebarWorkerSummary struct {
	InUse    int `json:"in_use"`
	Capacity int `json:"capacity"`
}

type uiSidebarJobStateSummary struct {
	Active int `json:"active"`
	Queued int `json:"queued"`
}

// projectFilterBundles applies the repeatable ?project= query (id or name)
// to the principal-visible bundles.
func (s *Server) projectFilterBundles(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) ([]*ProjectBundle, bool) {
	refs := r.URL.Query()["project"]
	bundles := s.scopedBundles(principal)
	if len(refs) == 0 {
		return bundles, true
	}

	selected := make([]*ProjectBundle, 0, len(refs))
	for _, ref := range refs {
		bundle, err := s.resolveProjectBundle(r.Context(), principal, ref)
		if err != nil {
			writeProjectResolveError(w, err)
			return nil, false
		}
		allowed := false
		for _, visible := range bundles {
			if visible.Project.ID == bundle.Project.ID {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "forbidden", "project is not accessible with this token")
			return nil, false
		}
		selected = append(selected, bundle)
	}

	return selected, true
}

func (s *Server) handleListTasksAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

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

	response := aggregateTasksResponse{Tasks: []uiTaskWithProject{}}
	for _, bundle := range bundles {
		var tasks []coordinator.Task
		if ready {
			tasks, err = bundle.Tasks.ReadyTasks(r.Context(), filter)
		} else {
			tasks, err = bundle.Tasks.ListTasks(r.Context(), filter)
		}
		if err != nil {
			writeInternalError(w, r, "list_tasks_failed", err)
			return
		}
		for _, task := range tasks {
			response.Tasks = append(response.Tasks, uiTaskWithProject{
				Task:        task,
				ProjectID:   bundle.Project.ID,
				ProjectName: bundle.Project.Name,
			})
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleBoardAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	response := aggregateBoardResponse{Boards: []projectBoardResponse{}}
	for _, bundle := range bundles {
		ps := s.forBundle(bundle)
		board, err := ps.boardResponseForProject(r.Context(), principal)
		if err != nil {
			writeInternalError(w, r, "board_failed", err)
			return
		}
		response.Boards = append(response.Boards, projectBoardResponse{
			ProjectID:     bundle.Project.ID,
			ProjectName:   bundle.Project.Name,
			boardResponse: board,
		})
	}

	writeJSON(w, http.StatusOK, response)
}

// maxClosedTaskLimit caps a single /v2/done page so the unbounded history can
// never be fetched in one request.
const maxClosedTaskLimit = 200

func (s *Server) handleDoneAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	query, err := closedTaskQueryFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}

	response := aggregateDoneResponse{Done: []projectDoneResponse{}}
	for _, bundle := range bundles {
		ps := s.forBundle(bundle)
		done, err := ps.doneResponseForProject(r.Context(), principal, query)
		if err != nil {
			writeInternalError(w, r, "done_failed", err)
			return
		}
		response.Done = append(response.Done, projectDoneResponse{
			ProjectID:    bundle.Project.ID,
			ProjectName:  bundle.Project.Name,
			doneResponse: done,
		})
	}

	writeJSON(w, http.StatusOK, response)
}

// completionStatWindow pairs a cumulative throughput window with its exact
// response label for /v2/stats/completions, in ascending order.
type completionStatWindow struct {
	duration time.Duration
	label    string
}

var completionStatWindows = []completionStatWindow{
	{15 * time.Minute, "15m"},
	{30 * time.Minute, "30m"},
	{time.Hour, "1h"},
	{2 * time.Hour, "2h"},
	{4 * time.Hour, "4h"},
	{6 * time.Hour, "6h"},
	{12 * time.Hour, "12h"},
	{24 * time.Hour, "24h"},
}

// successfulDoneOutcomes are the terminal resolutions that count as a
// successful completion for the throughput stats; they mirror the
// DONE_OUTCOMES vocabulary in internal/coordinator/flow_graph.go and
// internal/web/assets/config.js.
var successfulDoneOutcomes = []coordinator.DoneResolution{
	coordinator.ResolutionCompleted,
	coordinator.ResolutionMerged,
}

// completionStatBucket is one cumulative window of successful completions.
type completionStatBucket struct {
	Window string `json:"window"`
	Count  int    `json:"count"`
}

// completionStatsResponse is the /v2/stats/completions payload: ascending
// cumulative buckets plus a per-outcome breakdown for the same windows.
type completionStatsResponse struct {
	Buckets   []completionStatBucket    `json:"buckets"`
	ByOutcome map[string]map[string]int `json:"by_outcome"`
}

// handleCompletionStatsAggregate sums successful completions across the
// visible projects. Like /v2/done it fans out per project bundle and adds the
// counts, but it returns only aggregate counts (no card detail), so any read
// scope may see it.
func (s *Server) handleCompletionStatsAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	response := completionStatsResponse{
		Buckets:   make([]completionStatBucket, len(completionStatWindows)),
		ByOutcome: make(map[string]map[string]int, len(completionStatWindows)),
	}
	for i, window := range completionStatWindows {
		response.Buckets[i] = completionStatBucket{Window: window.label}
		outcomeCounts := make(map[string]int, len(successfulDoneOutcomes))
		for _, outcome := range successfulDoneOutcomes {
			outcomeCounts[string(outcome)] = 0
		}
		response.ByOutcome[window.label] = outcomeCounts
	}

	for _, bundle := range bundles {
		ps := s.forBundle(bundle)
		counts, err := ps.completionStatsForProject(r.Context())
		if err != nil {
			writeInternalError(w, r, "completion_stats_failed", err)
			return
		}
		if len(counts) != len(completionStatWindows) {
			writeError(w, http.StatusInternalServerError, "completion_stats_failed", "unexpected window count")
			return
		}
		for i, window := range completionStatWindows {
			response.Buckets[i].Count += counts[i].Count
			for _, outcome := range successfulDoneOutcomes {
				response.ByOutcome[window.label][string(outcome)] += counts[i].ByOutcome[outcome]
			}
		}
	}

	writeJSON(w, http.StatusOK, response)
}

// closedTaskQueryFromRequest parses the /v2/done query string into a bounded
// ClosedTaskQuery (limit, keyset cursor, time window, outcome filter).
func closedTaskQueryFromRequest(r *http.Request) (coordinator.ClosedTaskQuery, error) {
	values := r.URL.Query()
	query := coordinator.ClosedTaskQuery{}

	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return query, fmt.Errorf("invalid limit %q", raw)
		}
		if limit > maxClosedTaskLimit {
			limit = maxClosedTaskLimit
		}
		query.Limit = limit
	}

	if raw := strings.TrimSpace(values.Get("before")); raw != "" {
		before, err := sqlitex.ParseTime(raw)
		if err != nil {
			return query, fmt.Errorf("invalid before cursor %q", raw)
		}
		query.Before = &before
		query.BeforeID = strings.TrimSpace(values.Get("before_id"))
	}

	if raw := strings.TrimSpace(values.Get("within")); raw != "" {
		window, err := parseWithinWindow(raw)
		if err != nil {
			return query, err
		}
		cutoff := time.Now().UTC().Add(-window)
		query.Within = &cutoff
	}

	switch outcome := coordinator.ClosedOutcome(strings.TrimSpace(values.Get("outcome"))); outcome {
	case coordinator.ClosedOutcomeAll, coordinator.ClosedOutcomeCompleted, coordinator.ClosedOutcomeMerged,
		coordinator.ClosedOutcomeRejected, coordinator.ClosedOutcomeAbandoned,
		coordinator.ClosedOutcomeCancelled, coordinator.ClosedOutcomeFailed:
		query.Outcome = outcome
	default:
		return query, fmt.Errorf("invalid outcome %q", outcome)
	}

	return query, nil
}

// parseWithinWindow accepts a Go duration (e.g. "24h") plus a day suffix
// ("7d", "30d") that time.ParseDuration cannot handle.
func parseWithinWindow(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid within window %q", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window <= 0 {
		return 0, fmt.Errorf("invalid within window %q", raw)
	}
	return window, nil
}

func (s *Server) handleSidebar(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
		return
	}

	taskBundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	response := sidebarResponse{}
	for _, bundle := range taskBundles {
		result, err := bundle.Tasks.BoardResult(r.Context())
		if err != nil {
			writeInternalError(w, r, "sidebar_failed", err)
			return
		}
		addSidebarBoardCounts(&response, result)

		closed, err := bundle.Tasks.CountClosedTasks(r.Context())
		if err != nil {
			writeInternalError(w, r, "sidebar_failed", err)
			return
		}
		response.Done += closed
	}

	workers, err := s.registry.Directory().ListWorkers(r.Context())
	if err != nil {
		writeInternalError(w, r, "list_workers_failed", err)
		return
	}

	jobs, leases, ok := collectJobsAndLeases(w, r, s.scopedBundles(principal))
	if !ok {
		return
	}

	now := time.Now().UTC()
	response.Workers = uiSidebarWorkerSummaryFromLeases(workers, leases, now)
	response.Jobs = uiSidebarJobStateSummaryFromJobs(jobs, leases, now)
	writeJSON(w, http.StatusOK, response)
}

func addSidebarBoardCounts(response *sidebarResponse, result coordinator.BoardResult) {
	blocked := len(result.BlockedIDs)
	response.Board.Unscheduled += len(result.Board.Unscheduled)
	response.Board.Scheduled += len(result.Board.Scheduled)
	response.Board.InProgress += len(result.Board.InProgress) - blocked
	response.Board.Blocked += blocked

	for id, state := range result.LaneStates {
		switch state {
		case coordinator.LaneStateTriage:
			response.Triage++
		case coordinator.LaneStateReadyToMerge:
			response.Merge++
		}
		if result.WaitReasons[id] != "" {
			response.Feedback++
		}
	}
}

func uiSidebarWorkerSummaryFromLeases(workers []worker.Worker, leases []worker.Lease, now time.Time) uiSidebarWorkerSummary {
	summary := uiSidebarWorkerSummary{}
	// Every worker is a one-shot process holding one assignment-derived bucket,
	// so registered workers equal concurrent job slots.
	summary.Capacity = len(workers)
	diagnostics := uiWorkerDiagnosticsFromLeases(workers, leases, now)
	for _, diagnostic := range diagnostics {
		summary.InUse += diagnostic.LiveJobs
	}

	return summary
}

func uiSidebarJobStateSummaryFromJobs(jobs []worker.Job, leases []worker.Lease, now time.Time) uiSidebarJobStateSummary {
	summary := uiSidebarJobStateSummary{}
	for _, job := range jobs {
		if job.State == worker.JobQueued {
			summary.Queued++
		}
	}

	activeJobIDs := map[string]bool{}
	for _, lease := range leases {
		if uiLeaseIsLive(lease, now) {
			activeJobIDs[lease.JobID] = true
		}
	}
	summary.Active = len(activeJobIDs)

	return summary
}
