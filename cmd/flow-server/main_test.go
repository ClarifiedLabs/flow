package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestLogLevelFlagEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level=debug", "--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow-server command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestLoadOrCreateOwnerTokenIsStable(t *testing.T) {
	dataDir := t.TempDir()

	first, err := loadOrCreateOwnerToken(dataDir)
	if err != nil {
		t.Fatalf("create owner token: %v", err)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("owner token is empty")
	}

	tokenPath := filepath.Join(dataDir, "owner.token")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", info.Mode().Perm())
	}

	second, err := loadOrCreateOwnerToken(dataDir)
	if err != nil {
		t.Fatalf("load owner token: %v", err)
	}
	if second != first {
		t.Fatalf("second token = %q, want first token", second)
	}
}

func TestLoadOrCreateHookTokenIsStable(t *testing.T) {
	dataDir := t.TempDir()

	first, err := loadOrCreateHookToken(dataDir)
	if err != nil {
		t.Fatalf("create hook token: %v", err)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("hook token is empty")
	}

	tokenPath := filepath.Join(dataDir, "hook.token")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", info.Mode().Perm())
	}

	second, err := loadOrCreateHookToken(dataDir)
	if err != nil {
		t.Fatalf("load hook token: %v", err)
	}
	if second != first {
		t.Fatalf("second token = %q, want first token", second)
	}
}

func TestLoadOrCreateTokenRejectsUnsafeExistingPermissions(t *testing.T) {
	dataDir := t.TempDir()
	tokenPath := filepath.Join(dataDir, "owner.token")
	if err := os.WriteFile(tokenPath, []byte("owner-token\n"), 0o644); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatalf("chmod owner token: %v", err)
	}

	if _, err := loadOrCreateOwnerToken(dataDir); err == nil {
		t.Fatal("loadOrCreateOwnerToken accepted group/other-readable token file")
	}
}

func TestWriteServeClientConfigPublishesLocalDiscovery(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "owner.token"), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}

	configPath, err := writeServeClientConfig(config.CoordinatorConfig{
		DataDir:         dataDir,
		ListenAddr:      "127.0.0.1:9000",
		ProtocolVersion: "2",
		AuthorEntrypoint: map[string]any{
			"argv": []string{"flow"},
		},
	}, "owner-token", "", "")
	if err != nil {
		t.Fatalf("write serve client config: %v", err)
	}
	wantPath := filepath.Join(configHome, "flow", "config.yaml")
	if configPath != wantPath {
		t.Fatalf("configPath = %q, want %q", configPath, wantPath)
	}

	cfg, err := config.LoadClient("")
	if err != nil {
		t.Fatalf("load client config: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:9000" || cfg.ProtocolVersion != "2" || cfg.DataDir != dataDir {
		t.Fatalf("client config = %+v", cfg)
	}
	if cfg.Token != "owner-token" || cfg.TokenFile != filepath.Join(dataDir, "owner.token") {
		t.Fatalf("client token fields = token:%q token_file:%q", cfg.Token, cfg.TokenFile)
	}
}

func TestWriteServeClientConfigSupportsExplicitPath(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "owner.token"), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}

	explicitPath := filepath.Join(t.TempDir(), "isolated", "flow-client.yaml")
	configPath, err := writeServeClientConfig(config.CoordinatorConfig{
		DataDir:         dataDir,
		ListenAddr:      "127.0.0.1:9001",
		ProtocolVersion: "2",
	}, "owner-token", "", explicitPath)
	if err != nil {
		t.Fatalf("write serve client config: %v", err)
	}
	if configPath != explicitPath {
		t.Fatalf("configPath = %q, want %q", configPath, explicitPath)
	}
	if _, err := os.Stat(filepath.Join(configHome, "flow", "config.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default client config stat err = %v, want not exist", err)
	}

	cfg, err := config.LoadClient(explicitPath)
	if err != nil {
		t.Fatalf("load explicit client config: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:9001" || cfg.DataDir != dataDir {
		t.Fatalf("client config = %+v", cfg)
	}
}

func TestWriteServeClientConfigReferencesExplicitOwnerTokenFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dataDir := t.TempDir()
	ownerTokenPath := filepath.Join(t.TempDir(), "owner.token")
	if err := os.WriteFile(ownerTokenPath, []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}

	if _, err := writeServeClientConfig(config.CoordinatorConfig{
		DataDir:         dataDir,
		ListenAddr:      "127.0.0.1:9003",
		ProtocolVersion: "2",
	}, "owner-token", ownerTokenPath, ""); err != nil {
		t.Fatalf("write serve client config: %v", err)
	}

	cfg, err := config.LoadClient("")
	if err != nil {
		t.Fatalf("load client config: %v", err)
	}
	if cfg.Token != "owner-token" || cfg.TokenFile != ownerTokenPath {
		t.Fatalf("client token fields = token:%q token_file:%q", cfg.Token, cfg.TokenFile)
	}
}

func TestPrepareServeClientConfigCanSkipWriting(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := prepareServeClientConfig(config.CoordinatorConfig{
		DataDir:         t.TempDir(),
		ListenAddr:      "127.0.0.1:9002",
		ProtocolVersion: "2",
	}, "owner-token", "", "", true)
	if err != nil {
		t.Fatalf("prepare skipped client config: %v", err)
	}
	if path != "skipped" {
		t.Fatalf("path = %q, want skipped", path)
	}
	if _, err := os.Stat(filepath.Join(configHome, "flow", "config.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default client config stat err = %v, want not exist", err)
	}
}

func TestServeRejectsConflictingClientConfigFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServe([]string{"--client-config", filepath.Join(t.TempDir(), "client.yaml"), "--no-write-client-config"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--client-config and --no-write-client-config cannot be used together") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServeRejectsConflictingOwnerTokenFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	tokenPath := filepath.Join(t.TempDir(), "owner.token")
	code := runServe([]string{"--owner-token", "owner-token", "--owner-token-file", tokenPath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--owner-token and --owner-token-file cannot be used together") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// newServeTestRegistry builds a registry backed by a global database and a
// single project (with its real bare exchange), mirroring how `flow-server
// serve` wires the coordinator. The single project lets unscoped issue routes
// resolve implicitly, exactly as a fresh single-project deployment behaves.
func newServeTestRegistry(t *testing.T) (*api.Registry, coordinator.Project) {
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

	project, err := registry.CreateProject(ctx, coordinator.Project{Name: "demo", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	return registry, project
}

func TestTickProjectsTicksEveryProject(t *testing.T) {
	ctx := context.Background()
	registry, _ := newServeTestRegistry(t)

	// More projects than the concurrency bound so the semaphore path is
	// exercised and we confirm every project's tick runs to completion rather
	// than just the first few.
	const projectCount = lifecycleTickConcurrency + 3
	for i := 0; i < projectCount-1; i++ { // newServeTestRegistry already made one
		if _, err := registry.CreateProject(ctx, coordinator.Project{
			Name:       "demo-" + string(rune('a'+i)),
			BaseBranch: "main",
		}); err != nil {
			t.Fatalf("create project %d: %v", i, err)
		}
	}

	bundles := registry.All()
	if len(bundles) != projectCount {
		t.Fatalf("registry has %d projects, want %d", len(bundles), projectCount)
	}

	// Record a git event in every project so each consumer has real work; a
	// successful concurrent pass runs reconcile and persists the watermark.
	for _, bundle := range bundles {
		if _, err := bundle.GitEvents.Record(ctx, coordinator.GitEvent{
			Ref:    "refs/heads/issue/seed",
			OldSHA: "0000000000000000000000000000000000000000",
			NewSHA: "1111111111111111111111111111111111111111",
		}, coordinator.GitEventSourceAPI); err != nil {
			t.Fatalf("record git event for %s: %v", bundle.Project.ID, err)
		}
	}

	var stderr bytes.Buffer
	tickProjects(ctx, bundles, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("tickProjects logged errors: %q", stderr.String())
	}

	// Every project's consumer ran a clean pass that persisted its watermark —
	// proof all goroutines completed, not just the ones under the bound.
	for _, bundle := range bundles {
		var lastSeen int64
		if err := bundle.Store.DB().QueryRowContext(ctx,
			`SELECT last_seen_id FROM consumer_watermarks WHERE name = 'git_events'`).Scan(&lastSeen); err != nil {
			t.Fatalf("read watermark for %s: %v", bundle.Project.ID, err)
		}
		if lastSeen == 0 {
			t.Fatalf("project %s watermark did not advance; tick did not run its consumer", bundle.Project.ID)
		}
	}
}

func TestServeAPIWiresWorkerDiagnostics(t *testing.T) {
	ctx := context.Background()
	registry, project := newServeTestRegistry(t)

	if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatalf("project bundle not open")
	}
	issue, err := bundle.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title: "Serve wiring issue",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	server, err := api.NewServer(api.ServerOptions{
		Registry:        registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		ProtocolVersion: "1",
	})
	if err != nil {
		t.Fatalf("new serve api: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	request.Header.Set("Authorization", "Bearer owner-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"workers"`) {
		t.Fatalf("body = %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/issues/"+issue.ID+"/checks", nil)
	request.Header.Set("Authorization", "Bearer owner-token")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("checks status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"checks"`) {
		t.Fatalf("checks body = %s", response.Body.String())
	}
}
