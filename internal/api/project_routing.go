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
			writeError(w, http.StatusInternalServerError, "list_jobs_failed", err.Error())
			return nil, nil, false
		}
		jobs = append(jobs, bundleJobs...)
		bundleLeases, err := bundle.Queue.ListLeases(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_leases_failed", err.Error())
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

// bundleForChangeIssue resolves the owning bundle for the
// /v1/changes/{issueID}/checks subroute, whose leading path segment is an
// issue id (checks are keyed by issue) rather than a change id.
func (s *Server) bundleForChangeIssue(ctx context.Context, principal coordinator.Principal, issueID string) (*projectServer, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		if _, err := bundle.Issues.GetIssue(ctx, issueID); err == nil {
			return s.forBundle(bundle), true
		}
	}

	return nil, false
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

// changesSubpathIsChecks reports whether a /v1/changes/{id}/... path targets
// the checks subroute, whose leading segment is an issue id.
func changesSubpathIsChecks(path string) bool {
	rest := strings.TrimPrefix(path, "/v1/changes/")
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

	return nil, errors.New("multiple projects are registered; use /v1/projects/{project}/issues")
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
	rest := strings.TrimPrefix(r.URL.Path, "/v1/projects/")
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
	case sub == "issues":
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "issue read requires owner, session, or console token")
				return
			}
			ps.handleListIssues(w, requestWithPath(r, "/v1/issues"))
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
				writeError(w, http.StatusForbidden, "forbidden", "issue creation requires owner, session, or console token")
				return
			}
			ps.handleCreateIssue(w, requestWithPath(r, "/v1/issues"), principal)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
	case strings.HasPrefix(sub, "issues/"):
		ps.handleIssuePath(w, requestWithPath(r, "/v1/"+sub), principal)
	case sub == "board":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "board read requires owner, session, or console token", coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
			return
		}
		ps.handleBoard(w, r, principal)
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
		ExchangeURL:  project.ExchangeURL,
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

// uiIssueWithProject decorates an issue with its owning project for
// aggregate responses; issue ids alone are ambiguous across projects.
type uiIssueWithProject struct {
	coordinator.Issue
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
}

type aggregateIssuesResponse struct {
	Issues []uiIssueWithProject `json:"issues"`
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
	Workers  uiSidebarWorkerSummary   `json:"workers"`
	Jobs     uiSidebarJobStateSummary `json:"jobs"`
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

func (s *Server) handleListIssuesAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	filter, err := issueFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	response := aggregateIssuesResponse{Issues: []uiIssueWithProject{}}
	for _, bundle := range bundles {
		issues, err := bundle.Issues.ListIssues(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_issues_failed", err.Error())
			return
		}
		for _, issue := range issues {
			response.Issues = append(response.Issues, uiIssueWithProject{
				Issue:       issue,
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
			writeError(w, http.StatusInternalServerError, "board_failed", err.Error())
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

// maxClosedIssueLimit caps a single /v1/done page so the unbounded history can
// never be fetched in one request.
const maxClosedIssueLimit = 200

func (s *Server) handleDoneAggregate(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	bundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	query, err := closedIssueQueryFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}

	response := aggregateDoneResponse{Done: []projectDoneResponse{}}
	for _, bundle := range bundles {
		ps := s.forBundle(bundle)
		done, err := ps.doneResponseForProject(r.Context(), principal, query)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "done_failed", err.Error())
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

// closedIssueQueryFromRequest parses the /v1/done query string into a bounded
// ClosedIssueQuery (limit, keyset cursor, time window, outcome filter).
func closedIssueQueryFromRequest(r *http.Request) (coordinator.ClosedIssueQuery, error) {
	values := r.URL.Query()
	query := coordinator.ClosedIssueQuery{}

	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return query, fmt.Errorf("invalid limit %q", raw)
		}
		if limit > maxClosedIssueLimit {
			limit = maxClosedIssueLimit
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
	case coordinator.ClosedOutcomeAll, coordinator.ClosedOutcomeMerged, coordinator.ClosedOutcomeRejected, coordinator.ClosedOutcomeAbandoned:
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

	issueBundles, ok := s.projectFilterBundles(w, r, principal)
	if !ok {
		return
	}

	response := sidebarResponse{}
	for _, bundle := range issueBundles {
		result, err := bundle.Issues.BoardResult(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "sidebar_failed", err.Error())
			return
		}
		addSidebarBoardCounts(&response, result)

		closed, err := bundle.Issues.CountClosedIssues(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "sidebar_failed", err.Error())
			return
		}
		response.Done += closed
	}

	workers, err := s.registry.Directory().ListWorkers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_workers_failed", err.Error())
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
	for _, registeredWorker := range workers {
		summary.Capacity += registeredWorker.CapacityPersistentAgent + registeredWorker.CapacityEphemeral
	}
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
