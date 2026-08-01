package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// ProjectBundle holds one project's database and the services constructed on
// it. Every project-scoped request resolves to a bundle; the bundle's
// services never see another project's data.
type ProjectBundle struct {
	Project           coordinator.Project
	Store             *flowdb.Store
	Tasks             *coordinator.TaskService
	Features          *coordinator.FeatureService
	AgentDefs         *coordinator.AgentDefService
	Flows             *coordinator.FlowService
	WorkflowRuns      *coordinator.WorkflowRunService
	WorkflowArtifacts *coordinator.WorkflowArtifactService
	WorkflowExecutor  *coordinator.WorkflowExecutor
	Checks            *coordinator.CheckService
	Threads           *coordinator.ThreadService
	Sessions          *coordinator.SessionService
	Transcripts       *coordinator.TranscriptStore
	Attachments       *coordinator.TaskAttachmentStore
	Status            *coordinator.StatusService
	Reconciler        *coordinator.ReconcileService
	CheckConfigs      *coordinator.CheckConfigService
	Merges            *coordinator.MergeService
	GitEvents         *coordinator.GitEventService
	Idempotency       *coordinator.IdempotencyService
	GitEventConsumer  *coordinator.GitEventConsumer
	Queue             *worker.Service
	HistoryCaptures   *coordinator.HistoryCaptureService
}

type RegistryOptions struct {
	// DataDir is the coordinator data directory; project databases live at
	// <DataDir>/projects/<id>/flow.db.
	DataDir string
	// Global is the coordinator-wide database (projects registry, workers,
	// tokens, web sessions).
	Global *flowdb.Store
	// HistoryBlobStore is shared by every project history service. When nil the
	// registry creates a private local store under <DataDir>/history/blobs. A
	// supplied store remains caller-owned; the registry never closes it.
	HistoryBlobStore blob.Store
	// HistoryCaptureServiceOptions applies the resolved coordinator limits to
	// every per-project capture service opened by this registry.
	HistoryCaptureServiceOptions coordinator.HistoryCaptureServiceOptions

	AuthorEntrypoint           map[string]any
	AuthorEntrypointConfigured bool
	HarnessArgs                []string
	// DefaultAgent is the configured fallback agent selection (coordinator
	// config default_agent); the zero value resolves to the built-in default
	// harness.
	DefaultAgent flowharness.AgentSelection

	// ReviewAuthorCycleLimit bounds automated review/acceptance -> author fix
	// loops before a human must grant more cycles.
	ReviewAuthorCycleLimit int
	ReviewScopeFileLimit   int
	ReviewScopeLineLimit   int

	// CommitIdentity sets the git author/committer identity the coordinator
	// uses for the commits it creates (the squash-merge commit). The zero value
	// uses the git package's default identity.
	CommitIdentity flowgit.CommitIdentity
}

// Registry owns the coordinator's global services and the set of open
// project bundles. Projects registered while the server runs join the
// registry live via OpenProject.
type Registry struct {
	dataDir                    string
	global                     *flowdb.Store
	projects                   *coordinator.ProjectService
	globalAgentDefs            *coordinator.AgentDefService
	credentials                *coordinator.CredentialService
	directory                  *worker.Directory
	webSessions                *coordinator.WebSessionService
	idempotency                *coordinator.IdempotencyService
	authorEntrypoint           map[string]any
	authorEntrypointConfigured bool
	harnessArgs                []string
	defaultAgent               flowharness.AgentSelection
	reviewAuthorCycleLimit     int
	reviewScopeFileLimit       int
	reviewScopeLineLimit       int
	commitIdentity             flowgit.CommitIdentity
	historyBlobs               blob.Store
	historyBlobsOwned          bool
	historyCaptureOptions      coordinator.HistoryCaptureServiceOptions

	mu       sync.RWMutex
	bundles  map[string]*ProjectBundle
	createMu sync.Mutex

	// catalogMu serializes global agent-definition mutations with the complete
	// project open sequence. It stays held from restoring canonical definitions
	// through seeding project flows, and across global deletion reference checks.
	catalogMu sync.Mutex

	// These hooks coordinate catalog boundaries in concurrency tests.
	// Production registries leave them nil.
	beforeProjectFlowSeed      func()
	beforeGlobalAgentDefDelete func()
	catalogMutationLockBlocked func()

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
	defaultAgent, err := flowharness.ResolveAgentSelection(opts.DefaultAgent)
	if err != nil {
		return nil, fmt.Errorf("default agent: %w", err)
	}
	historyBlobs := opts.HistoryBlobStore
	historyBlobsOwned := false
	if historyBlobs == nil {
		historyBlobs, err = blob.NewLocal(filepath.Join(opts.DataDir, "history", "blobs"), blob.LocalOptions{})
		if err != nil {
			return nil, fmt.Errorf("open registry history blob store: %w", err)
		}
		historyBlobsOwned = true
	}

	registry := &Registry{
		dataDir:                    opts.DataDir,
		global:                     opts.Global,
		projects:                   coordinator.NewProjectService(opts.Global.DB()),
		globalAgentDefs:            coordinator.NewGlobalAgentDefServiceWithOptions(opts.Global.DB(), coordinator.AgentDefServiceOptions{DefaultAgent: defaultAgent}),
		credentials:                coordinator.NewCredentialService(opts.Global.DB()),
		directory:                  worker.NewDirectory(opts.Global.DB()),
		webSessions:                coordinator.NewWebSessionService(opts.Global.DB()),
		idempotency:                coordinator.NewIdempotencyService(opts.Global.DB()),
		authorEntrypoint:           opts.AuthorEntrypoint,
		authorEntrypointConfigured: opts.AuthorEntrypointConfigured,
		harnessArgs:                harnessArgs,
		defaultAgent:               defaultAgent,
		reviewAuthorCycleLimit:     opts.ReviewAuthorCycleLimit,
		reviewScopeFileLimit:       opts.ReviewScopeFileLimit,
		reviewScopeLineLimit:       opts.ReviewScopeLineLimit,
		commitIdentity:             opts.CommitIdentity,
		historyBlobs:               historyBlobs,
		historyBlobsOwned:          historyBlobsOwned,
		historyCaptureOptions:      opts.HistoryCaptureServiceOptions,
		bundles:                    map[string]*ProjectBundle{},
	}
	if err := registry.globalAgentDefs.SeedDefaults(context.Background()); err != nil {
		if historyBlobsOwned {
			if closer, ok := historyBlobs.(blob.Closer); ok {
				_ = closer.Close()
			}
		}
		return nil, fmt.Errorf("seed global agent definitions: %w", err)
	}
	return registry, nil
}

// AgentDefCatalog exposes the coordinator-global agent definitions while
// keeping mutations synchronized with project opening and flow seeding.
type AgentDefCatalog interface {
	List(context.Context) ([]coordinator.AgentDef, error)
	Get(context.Context, string) (coordinator.AgentDef, error)
	GetByName(context.Context, string) (coordinator.AgentDef, error)
	Create(context.Context, coordinator.AgentDefInput) (coordinator.AgentDef, error)
	Update(context.Context, string, coordinator.AgentDefInput) (coordinator.AgentDef, error)
	Delete(context.Context, string) error
}

type globalAgentDefCatalog struct {
	registry *Registry
}

func (c globalAgentDefCatalog) List(ctx context.Context) ([]coordinator.AgentDef, error) {
	return c.registry.globalAgentDefs.List(ctx)
}

func (c globalAgentDefCatalog) Get(ctx context.Context, id string) (coordinator.AgentDef, error) {
	return c.registry.globalAgentDefs.Get(ctx, id)
}

func (c globalAgentDefCatalog) GetByName(ctx context.Context, name string) (coordinator.AgentDef, error) {
	return c.registry.globalAgentDefs.GetByName(ctx, name)
}

func (c globalAgentDefCatalog) Create(ctx context.Context, input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	c.registry.lockCatalogMutation()
	defer c.registry.catalogMu.Unlock()
	return c.registry.globalAgentDefs.Create(ctx, input)
}

func (c globalAgentDefCatalog) Update(ctx context.Context, id string, input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	c.registry.lockCatalogMutation()
	defer c.registry.catalogMu.Unlock()
	return c.registry.globalAgentDefs.Update(ctx, id, input)
}

func (c globalAgentDefCatalog) Delete(ctx context.Context, id string) error {
	return c.registry.DeleteGlobalAgentDef(ctx, id)
}

func (r *Registry) lockCatalogMutation() {
	if r.catalogMu.TryLock() {
		return
	}
	if r.catalogMutationLockBlocked != nil {
		r.catalogMutationLockBlocked()
	}
	r.catalogMu.Lock()
}

func (r *Registry) Projects() *coordinator.ProjectService              { return r.projects }
func (r *Registry) GlobalAgentDefs() AgentDefCatalog                   { return globalAgentDefCatalog{registry: r} }
func (r *Registry) Credentials() *coordinator.CredentialService        { return r.credentials }
func (r *Registry) Directory() *worker.Directory                       { return r.directory }
func (r *Registry) WebSessions() *coordinator.WebSessionService        { return r.webSessions }
func (r *Registry) GlobalIdempotency() *coordinator.IdempotencyService { return r.idempotency }
func (r *Registry) HarnessArgs() []string                              { return r.harnessArgs }
func (r *Registry) DefaultAgent() flowharness.AgentSelection           { return r.defaultAgent }
func (r *Registry) HistoryBlobStore() blob.Store                       { return r.historyBlobs }
func (r *Registry) OwnsHistoryBlobStore() bool                         { return r.historyBlobsOwned }

// OpenAll opens a bundle for every project in the global registry.
func (r *Registry) OpenAll(ctx context.Context) error {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	return r.openAllLocked(ctx)
}

// openAllLocked opens every registered project while catalogMu is held.
func (r *Registry) openAllLocked(ctx context.Context) error {
	projects, err := r.projects.List(ctx)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if _, err := r.openProjectLocked(ctx, project); err != nil {
			return fmt.Errorf("open project %s: %w", project.ID, err)
		}
	}

	return nil
}

// OpenProject opens the project's database, constructs its service bundle,
// and registers it. Opening an already-open project returns the existing
// bundle. Global catalog mutations wait until default flow seeding completes.
func (r *Registry) OpenProject(ctx context.Context, project coordinator.Project) (*ProjectBundle, error) {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	return r.openProjectLocked(ctx, project)
}

// openProjectLocked opens one project while catalogMu is held.
func (r *Registry) openProjectLocked(ctx context.Context, project coordinator.Project) (_ *ProjectBundle, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if bundle, ok := r.bundles[project.ID]; ok {
		return bundle, nil
	}

	// Global defaults may have been renamed or deleted through the owner API
	// since registry startup. Restore every canonical role before opening a
	// fresh project so its built-in flows can always inherit those definitions.
	if err := r.globalAgentDefs.SeedDefaults(ctx); err != nil {
		return nil, fmt.Errorf("ensure global agent definitions for project %s: %w", project.ID, err)
	}

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(r.dataDir, project.ID))
	if err != nil {
		return nil, err
	}
	keepStore := false
	defer func() {
		if keepStore {
			return
		}
		if closeErr := store.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close project %s database after open failure: %w", project.ID, closeErr))
		}
	}()

	db := store.DB()
	tasks := coordinator.NewTaskService(db, project.ID)
	agentDefs := coordinator.NewInheritedAgentDefService(db, r.globalAgentDefs)
	flows := coordinator.NewFlowServiceWithAgentDefs(db, agentDefs)
	if r.beforeProjectFlowSeed != nil {
		r.beforeProjectFlowSeed()
	}
	if err := flows.SeedDefaults(ctx); err != nil {
		return nil, fmt.Errorf("seed default flows for project %s: %w", project.ID, err)
	}
	checks := coordinator.NewCheckService(db)
	threads := coordinator.NewThreadService(db)
	queue := worker.NewService(db)
	// checkConfigs and reconciler are constructed before sessions so the session
	// service can route a crashed author with a saved handoff to a completion-
	// assessment review (Mode-B recovery) instead of a blind full relaunch.
	checkConfigs := coordinator.NewCheckConfigServiceWithOptions(db, checks, queue, threads, project, coordinator.CheckConfigServiceOptions{
		HarnessArgs:  r.harnessArgs,
		DefaultAgent: r.defaultAgent,
	})
	reconciler := coordinator.NewReconcileService(db)
	sessions := coordinator.NewSessionServiceWithOptions(db, tasks, queue, coordinator.SessionServiceOptions{
		DefaultAuthorEntrypoint:         r.authorEntrypoint,
		DefaultAuthorEntrypointOverride: r.authorEntrypointConfigured,
		HarnessArgs:                     r.harnessArgs,
		DefaultAgent:                    r.defaultAgent,
		Credentials:                     r.credentials,
		Project:                         project,
		ReviewAuthorCycleLimit:          r.reviewAuthorCycleLimit,
		HandoffSnapshots:                reconciler,
		ReviewRounds:                    checkConfigs,
	})
	merges := coordinator.NewMergeService(db, tasks, sessions, project)
	merges.CommitIdentity = r.commitIdentity
	workflowRuns := coordinator.NewWorkflowRunServiceWithOptions(db, flows, tasks, coordinator.WorkflowRunServiceOptions{
		ReviewAuthorCycleLimit: r.reviewAuthorCycleLimit,
	})
	features := coordinator.NewFeatureService(db, tasks, project)
	features.Runs = workflowRuns
	workflowRuns.Features = features
	workflowArtifacts := coordinator.NewWorkflowArtifactService(db, tasks)
	workflowExecutor := coordinator.NewWorkflowExecutor(coordinator.WorkflowExecutorOptions{
		Database: db, Runs: workflowRuns, Artifacts: workflowArtifacts, Tasks: tasks,
		Features: features,
		Checks:   checks, CheckConfigs: checkConfigs, Sessions: sessions, Merges: merges,
		Queue: queue, Project: project, HarnessArgs: r.harnessArgs,
		ReviewScopeFileLimit: r.reviewScopeFileLimit,
		ReviewScopeLineLimit: r.reviewScopeLineLimit,
	})
	status := coordinator.NewStatusService(db)

	bundle := &ProjectBundle{
		Project:           project,
		Store:             store,
		Tasks:             tasks,
		Features:          features,
		AgentDefs:         agentDefs,
		Flows:             flows,
		WorkflowRuns:      workflowRuns,
		WorkflowArtifacts: workflowArtifacts,
		WorkflowExecutor:  workflowExecutor,
		Checks:            checks,
		Threads:           threads,
		Sessions:          sessions,
		Transcripts:       coordinator.NewTranscriptStore(filepath.Join(flowgit.ProjectDir(r.dataDir, project.ID), "transcripts")),
		Attachments:       coordinator.NewTaskAttachmentStore(filepath.Join(flowgit.ProjectDir(r.dataDir, project.ID), "attachments")),
		Status:            status,
		Reconciler:        reconciler,
		CheckConfigs:      checkConfigs,
		Merges:            merges,
		GitEvents:         coordinator.NewGitEventService(db),
		Idempotency:       coordinator.NewIdempotencyService(db),
		GitEventConsumer:  coordinator.NewGitEventConsumer(db, project),
		Queue:             queue,
		HistoryCaptures:   coordinator.NewHistoryCaptureServiceWithOptions(db, r.historyBlobs, r.historyCaptureOptions),
	}
	r.bundles[project.ID] = bundle
	keepStore = true

	return bundle, nil
}

// CreateProject performs the coordinator-side half of project registration:
// allocate an id, create the project directory with its bare exchange and
// hooks, insert the registry row, and open the bundle. The caller (the CLI)
// owns all worktree-side work.
func (r *Registry) CreateProject(ctx context.Context, input coordinator.Project) (coordinator.Project, error) {
	r.createMu.Lock()
	defer r.createMu.Unlock()
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()

	name := strings.TrimSpace(input.Name)
	if name == "" && strings.TrimSpace(input.RepoPath) != "" {
		name = filepath.Base(strings.TrimSpace(input.RepoPath))
	}
	if name == "" {
		return coordinator.Project{}, errors.New("project name or repo path is required")
	}
	if repoPath := strings.TrimSpace(input.RepoPath); repoPath != "" {
		if _, lookupErr := r.projects.GetByRepoPath(ctx, repoPath); lookupErr == nil {
			return coordinator.Project{}, coordinator.ErrProjectRepoPathExists
		} else if !errors.Is(lookupErr, coordinator.ErrProjectNotFound) {
			return coordinator.Project{}, lookupErr
		}
	}

	id, err := coordinator.ProjectIDFromName(name)
	if err != nil {
		return coordinator.Project{}, err
	}
	if existing, lookupErr := r.projects.Get(ctx, id); lookupErr == nil {
		return coordinator.Project{}, fmt.Errorf("%w: project name %q normalizes to %q, already used by %q; choose a distinct --name", coordinator.ErrProjectIDExists, name, id, existing.Name)
	} else if !errors.Is(lookupErr, coordinator.ErrProjectNotFound) {
		return coordinator.Project{}, lookupErr
	}

	// Check and restore the catalog before creating durable Git or registry
	// state. catalogMu remains held through openProjectLocked and flow seeding;
	// direct OpenProject calls take the same lock before repeating this check.
	if err := r.globalAgentDefs.SeedDefaults(ctx); err != nil {
		return coordinator.Project{}, fmt.Errorf("ensure global agent definitions: %w", err)
	}

	created, err := flowgit.CreateServerProject(ctx, flowgit.ServerProjectOptions{
		DataDir:    r.dataDir,
		ProjectID:  id,
		BaseBranch: input.BaseBranch,
	})
	if err != nil {
		return coordinator.Project{}, err
	}

	project, err := r.projects.Insert(ctx, coordinator.Project{
		ID:           id,
		Name:         name,
		RepoPath:     input.RepoPath,
		BaseBranch:   input.BaseBranch,
		ExchangeName: input.ExchangeName,
		ExchangePath: created.ExchangePath,
	})
	if err != nil {
		return coordinator.Project{}, err
	}

	if _, err := r.openProjectLocked(ctx, project); err != nil {
		return coordinator.Project{}, err
	}

	return project, nil
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

// ExtendActiveLeaseDeadlines reserves every open project's active leases
// through deadline. The coordinator calls this once during startup, before
// serving requests or starting recovery, so workers that survived a server
// restart can renew their existing leases instead of racing the expiry sweep.
func (r *Registry) ExtendActiveLeaseDeadlines(ctx context.Context, deadline time.Time) (int, error) {
	var total int
	var joined error
	for _, bundle := range r.All() {
		extended, err := bundle.Queue.ExtendActiveLeaseDeadlines(ctx, deadline)
		total += extended
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("extend active leases for project %s: %w", bundle.Project.ID, err))
		}
	}
	return total, joined
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

// DeleteGlobalAgentDef removes a global definition only when no project flow
// directly references its id. Project-local overrides do not erase that stored
// reference, so deleting the global row would otherwise make the flow
// impossible to resolve if its override were later removed.
func (r *Registry) DeleteGlobalAgentDef(ctx context.Context, id string) error {
	r.lockCatalogMutation()
	defer r.catalogMu.Unlock()

	id = strings.TrimSpace(id)
	if _, err := r.globalAgentDefs.Get(ctx, id); err != nil {
		return err
	}
	// OpenAll is normally completed at server startup, but keeping this method
	// self-contained prevents a registered, not-yet-open project from escaping
	// the cross-project reference check in tests and embedded uses. Holding
	// catalogMu prevents a new project from appearing between this check and the
	// delete.
	if err := r.openAllLocked(ctx); err != nil {
		return fmt.Errorf("open projects before inspecting agent references: %w", err)
	}
	for _, bundle := range r.All() {
		inUse, err := bundle.AgentDefs.IsReferenced(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect project %s agent references: %w", bundle.Project.ID, err)
		}
		if inUse {
			return coordinator.ErrAgentDefInUse
		}
	}
	if r.beforeGlobalAgentDefDelete != nil {
		r.beforeGlobalAgentDefDelete()
	}
	return r.globalAgentDefs.Delete(ctx, id)
}

type HistoryArtifactReconcileResult struct {
	Projects  int
	Examined  int
	Committed int
	Pending   int
	Failed    int
}

// ReconcilePendingHistoryArtifacts retries a separately bounded amount of
// coordinator-authorized publication work in every project. A broken project is
// reported after the remaining projects receive their own allowance.
func (r *Registry) ReconcilePendingHistoryArtifacts(ctx context.Context, limitPerProject int) (HistoryArtifactReconcileResult, error) {
	if limitPerProject <= 0 {
		return HistoryArtifactReconcileResult{}, errors.New("history reconcile limit per project must be positive")
	}
	result := HistoryArtifactReconcileResult{}
	var reconcileErrors []error
	for _, bundle := range r.All() {
		result.Projects++
		projectResult, err := bundle.HistoryCaptures.ReconcilePendingArtifacts(ctx, "", limitPerProject)
		result.Examined += projectResult.Examined
		result.Committed += projectResult.Committed
		result.Pending += projectResult.Pending
		result.Failed += len(projectResult.Failures)
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile project %s pending history artifacts: %w", bundle.Project.ID, err))
		}
	}
	return result, errors.Join(reconcileErrors...)
}

// HistoryBlobMetadata returns the complete relational protection set used by
// blob reconciliation. Active upload intents protect completed temporaries that
// have not yet been assigned a logical artifact. Pending publications protect
// both their temporary upload and destination key; every committed artifact
// remains referenced, including superseded checkpoints, until a future
// explicitly authorized retention transaction removes that reference.
func (r *Registry) HistoryBlobMetadata(ctx context.Context, limitPerProject int) (blob.ReconcileRequest, bool, error) {
	if limitPerProject <= 0 {
		return blob.ReconcileRequest{}, false, errors.New("history metadata limit per project must be positive")
	}
	request := blob.ReconcileRequest{
		LiveTemporaryIDs: map[string]struct{}{},
		ReferencedKeys:   map[blob.Key]struct{}{},
		PendingKeys:      map[blob.Key]struct{}{},
	}
	complete := true
	for _, bundle := range r.All() {
		rows, err := bundle.Store.DB().QueryContext(ctx, `
SELECT temporary_upload_id, blob_key, publication_state
FROM history_artifacts ORDER BY id LIMIT ?`, limitPerProject+1)
		if err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("read project %s history blob metadata: %w", bundle.Project.ID, err)
		}
		artifactRows := 0
		for rows.Next() {
			artifactRows++
			var temporaryID, keyText, state string
			if err := rows.Scan(&temporaryID, &keyText, &state); err != nil {
				rows.Close()
				return blob.ReconcileRequest{}, false, fmt.Errorf("scan project %s history blob metadata: %w", bundle.Project.ID, err)
			}
			if artifactRows > limitPerProject {
				complete = false
				continue
			}
			key, err := blob.ParseKey(keyText)
			if err != nil {
				rows.Close()
				return blob.ReconcileRequest{}, false, fmt.Errorf("project %s has invalid history blob key: %w", bundle.Project.ID, err)
			}
			switch coordinator.HistoryPublicationState(state) {
			case coordinator.HistoryPublicationPending:
				request.PendingKeys[key] = struct{}{}
				if temporaryID != "" {
					request.LiveTemporaryIDs[temporaryID] = struct{}{}
				}
			case coordinator.HistoryPublicationCommitted:
				request.ReferencedKeys[key] = struct{}{}
			default:
				rows.Close()
				return blob.ReconcileRequest{}, false, fmt.Errorf("project %s has invalid history publication state %q", bundle.Project.ID, state)
			}
		}
		if err := rows.Close(); err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("close project %s history metadata rows: %w", bundle.Project.ID, err)
		}
		if err := rows.Err(); err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("read project %s history blob metadata: %w", bundle.Project.ID, err)
		}

		intentRows, err := bundle.Store.DB().QueryContext(ctx, `
SELECT temporary_upload_id
FROM history_upload_intents
WHERE state = 'active'
ORDER BY temporary_upload_id LIMIT ?`, limitPerProject+1)
		if err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("read project %s active history upload intents: %w", bundle.Project.ID, err)
		}
		intentCount := 0
		for intentRows.Next() {
			intentCount++
			var temporaryID string
			if err := intentRows.Scan(&temporaryID); err != nil {
				intentRows.Close()
				return blob.ReconcileRequest{}, false, fmt.Errorf("scan project %s active history upload intent: %w", bundle.Project.ID, err)
			}
			if intentCount > limitPerProject {
				complete = false
				continue
			}
			request.LiveTemporaryIDs[temporaryID] = struct{}{}
		}
		if err := intentRows.Close(); err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("close project %s active history upload intents: %w", bundle.Project.ID, err)
		}
		if err := intentRows.Err(); err != nil {
			return blob.ReconcileRequest{}, false, fmt.Errorf("read project %s active history upload intents: %w", bundle.Project.ID, err)
		}
	}
	return request, complete, nil
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
	if r.historyBlobsOwned {
		if closer, ok := r.historyBlobs.(blob.Closer); ok {
			if err := closer.Close(); err != nil {
				joined = errors.Join(joined, fmt.Errorf("close owned history blob store: %w", err))
			}
		}
	}

	return joined
}
