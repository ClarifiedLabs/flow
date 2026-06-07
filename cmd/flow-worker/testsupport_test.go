package main

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// workerTestFixture bundles a single-project registry, its API server, and the
// per-project services the worker tests drive directly. The worker itself talks
// to the coordinator over HTTP, so a single project lets it claim, run, and
// report against the bundle without any project qualifier.
type workerTestFixture struct {
	Registry    *api.Registry
	Server      *api.Server
	Project     coordinator.Project
	Credentials *coordinator.CredentialService
	Directory   *flowworker.Directory
	Issues      *coordinator.IssueService
	Checks      *coordinator.CheckService
	Sessions    *coordinator.SessionService
	Queue       *flowworker.Service
	// DB is the project bundle's database, for tests that assert directly on
	// rows the worker writes (leases, transitions).
	DB *sql.DB
}

// newWorkerTestFixture builds the registry, seeds the worker token, and opens a
// single project (with its real bare exchange under the data dir).
func newWorkerTestFixture(t *testing.T) workerTestFixture {
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

	if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
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
	// The exchange's pre/post-receive hooks invoke os.Executable(), which under
	// `go test` is the test binary, not flow-server. Neutralize them so seed
	// pushes succeed without re-entering the test binary.
	neutralizeExchangeHooks(t, project.ExchangePath)
	// Seed the project's exchange with a base branch so the worker can clone
	// and check out jobs. Jobs enqueued through the
	// session service are stamped with this project's exchange URL; plain jobs
	// fall back to the worker config's git.exchange_url, which tests point at
	// the same exchange.
	seedWorkerProjectExchange(t, project)
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatalf("project bundle not open")
	}

	server, err := api.NewServer(api.ServerOptions{
		Registry:        registry,
		ProtocolVersion: "1",
	})
	if err != nil {
		t.Fatalf("new api server: %v", err)
	}

	return workerTestFixture{
		Registry:    registry,
		Server:      server,
		Project:     project,
		Credentials: registry.Credentials(),
		Directory:   registry.Directory(),
		Issues:      bundle.Issues,
		Checks:      bundle.Checks,
		Sessions:    bundle.Sessions,
		Queue:       bundle.Queue,
		DB:          bundle.Store.DB(),
	}
}

// claimNext claims the next eligible job for the worker through the registry,
// the same path the coordinator's claim endpoint uses.
func (f workerTestFixture) claimNext(ctx context.Context, input flowworker.ClaimInput) (flowworker.ProjectClaim, bool, error) {
	return f.Registry.Claim(ctx, input)
}

// neutralizeExchangeHooks overwrites the bare exchange's git hooks with no-op
// scripts. Under `go test` the real hooks would invoke the test binary, which
// declines the push (and would recursively re-run the test suite).
func neutralizeExchangeHooks(t *testing.T, exchangePath string) {
	t.Helper()
	hooksDir := filepath.Join(exchangePath, "hooks")
	for _, name := range []string{"pre-receive", "post-receive"} {
		if err := os.WriteFile(filepath.Join(hooksDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("neutralize %s hook: %v", name, err)
		}
	}
}

// seedWorkerProjectExchange builds a worktree with the project's base branch,
// then pushes it into the project's bare exchange so the worker can clone a
// valid repository for its jobs.
func seedWorkerProjectExchange(t *testing.T, project coordinator.Project) {
	t.Helper()

	worktree := filepath.Join(t.TempDir(), "worktree")
	gitRun(t, "", "-c", "init.defaultBranch=main", "init", worktree)
	gitRun(t, worktree, "config", "user.email", "flow@example.com")
	gitRun(t, worktree, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	gitRun(t, worktree, "add", "README.md")
	gitRun(t, worktree, "commit", "-m", "seed")
	gitRun(t, worktree, "branch", "-M", project.BaseBranch)

	if _, err := flowgit.SeedExchangeFromWorktree(context.Background(), flowgit.SeedOptions{
		RepoPath:     worktree,
		BaseBranch:   project.BaseBranch,
		ExchangeName: flowgit.DefaultExchangeName,
		ExchangeURL:  project.ExchangeURL,
	}); err != nil {
		t.Fatalf("seed project exchange: %v", err)
	}
}
