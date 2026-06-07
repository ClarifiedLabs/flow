package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// flowTestFixture bundles a single-project registry, its API server, and the
// per-project services the CLI tests drive directly. A single project lets the
// CLI's unscoped issue/board routes resolve implicitly, matching a fresh
// single-project deployment.
type flowTestFixture struct {
	Registry    *api.Registry
	Server      *api.Server
	Project     coordinator.Project
	Credentials *coordinator.CredentialService
	Directory   *flowworker.Directory
	Issues      *coordinator.IssueService
	Checks      *coordinator.CheckService
	Threads     *coordinator.ThreadService
	Sessions    *coordinator.SessionService
	Status      *coordinator.StatusService
	Reconciler  *coordinator.ReconcileService
	Queue       *flowworker.Service
}

// newFlowTestFixture builds the registry, seeds the owner/worker tokens, and
// opens a single project (with its real bare exchange).
func newFlowTestFixture(t *testing.T) flowTestFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed")
	}

	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() {
		_ = global.Close()
	})

	registry, err := api.NewRegistry(api.RegistryOptions{DataDir: dataDir, Global: global})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() {
		_ = registry.Close()
	})

	credentials := registry.Credentials()
	if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   "worker-token",
		Scope:   coordinator.TokenScopeWorker,
		Subject: "w-local",
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}

	project, err := registry.CreateProject(ctx, coordinator.Project{Name: "demo", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatalf("project bundle not open")
	}

	server, err := api.NewServer(api.ServerOptions{
		Registry:        registry,
		OwnerToken:      "owner-token",
		ProtocolVersion: "1",
	})
	if err != nil {
		t.Fatalf("new api server: %v", err)
	}

	return flowTestFixture{
		Registry:    registry,
		Server:      server,
		Project:     project,
		Credentials: credentials,
		Directory:   registry.Directory(),
		Issues:      bundle.Issues,
		Checks:      bundle.Checks,
		Threads:     bundle.Threads,
		Sessions:    bundle.Sessions,
		Status:      bundle.Status,
		Reconciler:  bundle.Reconciler,
		Queue:       bundle.Queue,
	}
}

// claimNext claims the next eligible job for the worker through the registry,
// the same path the coordinator's claim endpoint uses.
func (f flowTestFixture) claimNext(ctx context.Context, input flowworker.ClaimInput) (flowworker.ProjectClaim, bool, error) {
	return f.Registry.Claim(ctx, input)
}

// newFlowTestFixtureWithProtocol constructs the server with an explicit
// protocol version for tests that exercise the protocol header.
func newFlowTestFixtureWithProtocol(t *testing.T, protocolVersion string) flowTestFixture {
	t.Helper()
	f := newFlowTestFixture(t)
	server, err := api.NewServer(api.ServerOptions{
		Registry:        f.Registry,
		OwnerToken:      "owner-token",
		ProtocolVersion: protocolVersion,
	})
	if err != nil {
		t.Fatalf("new api server: %v", err)
	}
	f.Server = server
	return f
}

func repointFlowTestFixtureExchange(t *testing.T, fixture flowTestFixture, exchangePath string) {
	t.Helper()

	bundle, ok := fixture.Registry.Bundle(fixture.Project.ID)
	if !ok {
		t.Fatalf("project bundle not open")
	}
	project := bundle.Project
	project.ExchangeURL = exchangePath
	project.ExchangePath = exchangePath
	bundle.Project = project

	db := bundle.Store.DB()
	bundle.Merges = coordinator.NewMergeService(db, bundle.Issues, bundle.Sessions, project)
	bundle.CheckConfigs = coordinator.NewCheckConfigServiceWithOptions(db, bundle.Checks, bundle.Queue, bundle.Threads, project, coordinator.CheckConfigServiceOptions{})
	bundle.GitEventConsumer = coordinator.NewGitEventConsumer(db, project)
	bundle.Engine = lifecycle.NewEngine(db, lifecycle.NewEffects(
		bundle.Issues,
		bundle.Checks,
		bundle.CheckConfigs,
		bundle.Sessions,
		bundle.Merges,
		bundle.Threads,
		bundle.Status,
	))
}
