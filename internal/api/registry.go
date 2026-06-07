package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// ProjectBundle holds one project's database and the services constructed on
// it. Every project-scoped request resolves to a bundle; the bundle's
// services never see another project's data.
type ProjectBundle struct {
	Project          coordinator.Project
	Store            *flowdb.Store
	Issues           *coordinator.IssueService
	Checks           *coordinator.CheckService
	Threads          *coordinator.ThreadService
	Sessions         *coordinator.SessionService
	Transcripts      *coordinator.TranscriptStore
	Attachments      *coordinator.IssueAttachmentStore
	Status           *coordinator.StatusService
	Reconciler       *coordinator.ReconcileService
	CheckConfigs     *coordinator.CheckConfigService
	Merges           *coordinator.MergeService
	Transitions      *coordinator.TransitionService
	GitEvents        *coordinator.GitEventService
	Idempotency      *coordinator.IdempotencyService
	GitEventConsumer *coordinator.GitEventConsumer
	Queue            *worker.Service
	Engine           *lifecycle.Engine
}

type RegistryOptions struct {
	// DataDir is the coordinator data directory; project databases live at
	// <DataDir>/projects/<id>/flow.db.
	DataDir string
	// Global is the coordinator-wide database (projects registry, workers,
	// tokens, web sessions).
	Global *flowdb.Store

	// ExchangeBaseURL is the public base URL used for per-project HTTP Git
	// exchange remotes. When empty, project registration keeps same-machine
	// file:// exchange URLs for tests and local direct use.
	ExchangeBaseURL string

	AuthorEntrypoint           map[string]any
	AuthorEntrypointConfigured bool
	HarnessArgs                flowharness.Args

	// Deadlines bounds hung planning/authoring sessions and never-reporting
	// checks. The zero value disables every deadline.
	Deadlines lifecycle.DeadlineConfig

	// ReviewAuthorCycleLimit bounds automated review/acceptance -> author fix
	// loops before a human must grant more cycles.
	ReviewAuthorCycleLimit int
}

// Registry owns the coordinator's global services and the set of open
// project bundles. Projects registered while the server runs join the
// registry live via OpenProject.
type Registry struct {
	dataDir                    string
	global                     *flowdb.Store
	projects                   *coordinator.ProjectService
	credentials                *coordinator.CredentialService
	directory                  *worker.Directory
	webSessions                *coordinator.WebSessionService
	idempotency                *coordinator.IdempotencyService
	exchangeBaseURL            string
	authorEntrypoint           map[string]any
	authorEntrypointConfigured bool
	harnessArgs                flowharness.Args
	deadlines                  lifecycle.DeadlineConfig
	reviewAuthorCycleLimit     int

	mu      sync.RWMutex
	bundles map[string]*ProjectBundle

	// claimMu serializes job claims: worker capacity is enforced against
	// lease counts aggregated across project databases, and no transaction
	// spans them.
	claimMu sync.Mutex
}

func NewRegistry(opts RegistryOptions) (*Registry, error) {
	if opts.Global == nil {
		return nil, errors.New("global database store is required")
	}
	harnessArgs, err := flowharness.NormalizeArgs(opts.HarnessArgs)
	if err != nil {
		return nil, fmt.Errorf("harness args: %w", err)
	}

	return &Registry{
		dataDir:                    opts.DataDir,
		global:                     opts.Global,
		projects:                   coordinator.NewProjectService(opts.Global.DB()),
		credentials:                coordinator.NewCredentialService(opts.Global.DB()),
		directory:                  worker.NewDirectory(opts.Global.DB()),
		webSessions:                coordinator.NewWebSessionService(opts.Global.DB()),
		idempotency:                coordinator.NewIdempotencyService(opts.Global.DB()),
		exchangeBaseURL:            strings.TrimRight(strings.TrimSpace(opts.ExchangeBaseURL), "/"),
		authorEntrypoint:           opts.AuthorEntrypoint,
		authorEntrypointConfigured: opts.AuthorEntrypointConfigured,
		harnessArgs:                harnessArgs,
		deadlines:                  opts.Deadlines,
		reviewAuthorCycleLimit:     opts.ReviewAuthorCycleLimit,
		bundles:                    map[string]*ProjectBundle{},
	}, nil
}

func (r *Registry) Projects() *coordinator.ProjectService              { return r.projects }
func (r *Registry) Credentials() *coordinator.CredentialService        { return r.credentials }
func (r *Registry) Directory() *worker.Directory                       { return r.directory }
func (r *Registry) WebSessions() *coordinator.WebSessionService        { return r.webSessions }
func (r *Registry) GlobalIdempotency() *coordinator.IdempotencyService { return r.idempotency }
func (r *Registry) HarnessArgs() flowharness.Args                      { return r.harnessArgs }

// OpenAll opens a bundle for every project in the global registry.
func (r *Registry) OpenAll(ctx context.Context) error {
	projects, err := r.projects.List(ctx)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if _, err := r.OpenProject(ctx, project); err != nil {
			return fmt.Errorf("open project %s: %w", project.ID, err)
		}
	}

	return nil
}

// OpenProject opens the project's database, constructs its service bundle,
// and registers it. Opening an already-open project returns the existing
// bundle.
func (r *Registry) OpenProject(ctx context.Context, project coordinator.Project) (*ProjectBundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if bundle, ok := r.bundles[project.ID]; ok {
		return bundle, nil
	}

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(r.dataDir, project.ID))
	if err != nil {
		return nil, err
	}

	db := store.DB()
	issues := coordinator.NewIssueService(db)
	checks := coordinator.NewCheckService(db)
	threads := coordinator.NewThreadService(db)
	queue := worker.NewService(db)
	// checkConfigs and reconciler are constructed before sessions so the session
	// service can route a crashed author with a saved handoff to a completion-
	// assessment review (Mode-B recovery) instead of a blind full relaunch.
	checkConfigs := coordinator.NewCheckConfigServiceWithOptions(db, checks, queue, threads, project, coordinator.CheckConfigServiceOptions{
		HarnessArgs: r.harnessArgs,
	})
	reconciler := coordinator.NewReconcileService(db)
	sessions := coordinator.NewSessionServiceWithOptions(db, issues, queue, coordinator.SessionServiceOptions{
		DefaultAuthorEntrypoint:         r.authorEntrypoint,
		DefaultAuthorEntrypointOverride: r.authorEntrypointConfigured,
		HarnessArgs:                     r.harnessArgs,
		Credentials:                     r.credentials,
		Project:                         project,
		ReviewAuthorCycleLimit:          r.reviewAuthorCycleLimit,
		HandoffSnapshots:                reconciler,
		ReviewRounds:                    checkConfigs,
	})
	merges := coordinator.NewMergeService(db, issues, sessions, project)
	status := coordinator.NewStatusService(db)

	engine := lifecycle.NewEngine(db, lifecycle.NewEffects(issues, checks, checkConfigs, sessions, merges, threads, status))
	engine.SetDeadlines(r.deadlines)

	bundle := &ProjectBundle{
		Project:          project,
		Store:            store,
		Issues:           issues,
		Checks:           checks,
		Threads:          threads,
		Sessions:         sessions,
		Transcripts:      coordinator.NewTranscriptStore(filepath.Join(flowgit.ProjectDir(r.dataDir, project.ID), "transcripts")),
		Attachments:      coordinator.NewIssueAttachmentStore(filepath.Join(flowgit.ProjectDir(r.dataDir, project.ID), "attachments")),
		Status:           status,
		Reconciler:       reconciler,
		CheckConfigs:     checkConfigs,
		Merges:           merges,
		Transitions:      coordinator.NewTransitionService(db),
		GitEvents:        coordinator.NewGitEventService(db),
		Idempotency:      coordinator.NewIdempotencyService(db),
		GitEventConsumer: coordinator.NewGitEventConsumer(db, project),
		Queue:            queue,
		Engine:           engine,
	}
	r.bundles[project.ID] = bundle

	return bundle, nil
}

// CreateProject performs the coordinator-side half of project registration:
// allocate an id, create the project directory with its bare exchange and
// hooks, insert the registry row, and open the bundle. The caller (the CLI)
// owns all worktree-side work.
func (r *Registry) CreateProject(ctx context.Context, input coordinator.Project) (coordinator.Project, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" && strings.TrimSpace(input.RepoPath) != "" {
		name = filepath.Base(strings.TrimSpace(input.RepoPath))
	}
	if name == "" {
		return coordinator.Project{}, errors.New("project name or repo path is required")
	}

	id, err := coordinator.NewProjectID()
	if err != nil {
		return coordinator.Project{}, err
	}

	created, err := flowgit.CreateServerProject(ctx, flowgit.ServerProjectOptions{
		DataDir:    r.dataDir,
		ProjectID:  id,
		BaseBranch: input.BaseBranch,
	})
	if err != nil {
		return coordinator.Project{}, err
	}

	exchangeURL := created.ExchangeURL
	if r.exchangeBaseURL != "" {
		exchangeURL, err = exchangeHTTPURL(r.exchangeBaseURL, id)
		if err != nil {
			return coordinator.Project{}, err
		}
	}

	project, err := r.projects.Insert(ctx, coordinator.Project{
		ID:           id,
		Name:         name,
		RepoPath:     input.RepoPath,
		BaseBranch:   input.BaseBranch,
		ExchangeName: input.ExchangeName,
		ExchangeURL:  exchangeURL,
		ExchangePath: created.ExchangePath,
	})
	if err != nil {
		return coordinator.Project{}, err
	}

	if _, err := r.OpenProject(ctx, project); err != nil {
		return coordinator.Project{}, err
	}

	return project, nil
}

func exchangeHTTPURL(baseURL string, projectID string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", errors.New("exchange base url is required")
	}
	if strings.TrimSpace(projectID) == "" {
		return "", errors.New("project id is required")
	}
	joined, err := url.JoinPath(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "git", "projects", projectID, "exchange.git")
	if err != nil {
		return "", fmt.Errorf("build exchange url: %w", err)
	}

	return joined, nil
}

// Bundle returns the open bundle for a project id.
func (r *Registry) Bundle(projectID string) (*ProjectBundle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bundle, ok := r.bundles[projectID]
	return bundle, ok
}

// All returns the open bundles ordered by project name for stable
// aggregation output.
func (r *Registry) All() []*ProjectBundle {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bundles := make([]*ProjectBundle, 0, len(r.bundles))
	for _, bundle := range r.bundles {
		bundles = append(bundles, bundle)
	}
	sort.Slice(bundles, func(i, j int) bool {
		if bundles[i].Project.Name != bundles[j].Project.Name {
			return bundles[i].Project.Name < bundles[j].Project.Name
		}
		return bundles[i].Project.ID < bundles[j].Project.ID
	})

	return bundles
}

// Claim claims the next eligible job for a worker across all projects,
// serialized by the registry's claim mutex.
func (r *Registry) Claim(ctx context.Context, input worker.ClaimInput) (worker.ProjectClaim, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()

	queues := make([]worker.ProjectQueue, 0)
	for _, bundle := range r.All() {
		queues = append(queues, worker.ProjectQueue{ProjectID: bundle.Project.ID, Queue: bundle.Queue})
	}

	return worker.ClaimAcrossProjects(ctx, r.directory, queues, input)
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var joined error
	for id, bundle := range r.bundles {
		if err := bundle.Store.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close project %s database: %w", id, err))
		}
		delete(r.bundles, id)
	}

	return joined
}
