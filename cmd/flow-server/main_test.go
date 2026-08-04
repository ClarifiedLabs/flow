package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/metrics"
)

func TestHistoryBlobFactoryConfigOpensDefaultLocalStore(t *testing.T) {
	history, err := (config.CoordinatorHistoryConfig{}).Resolve(t.TempDir())
	if err != nil {
		t.Fatalf("resolve default history config: %v", err)
	}

	store, err := blob.Open(context.Background(), historyBlobFactoryConfig(history.Blob))
	if err != nil {
		t.Fatalf("open default local history store: %v", err)
	}
	if closer, ok := store.(blob.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
	if _, ok := store.(*blob.Local); !ok {
		t.Fatalf("store type = %T, want *blob.Local", store)
	}
}

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
		DataDir:    dataDir,
		ListenAddr: "127.0.0.1:9000",
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
	if cfg.ServerURL != "http://127.0.0.1:9000" || cfg.DataDir != dataDir {
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
		DataDir:    dataDir,
		ListenAddr: "127.0.0.1:9001",
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
		DataDir:    dataDir,
		ListenAddr: "127.0.0.1:9003",
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
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:9002",
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

func TestServeRequiresProviderBindingForOrchestratorToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServe([]string{"--orchestrator-token", "orchestrator-token"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--orchestrator-provider-ids") {
		t.Fatalf("stderr = %q, want provider binding error", stderr.String())
	}
}

func TestNormalizeProviderIDSubject(t *testing.T) {
	got, err := normalizeProviderIDSubject(" darwin-host ,in-cluster,darwin-host ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "darwin-host,in-cluster" {
		t.Fatalf("normalizeProviderIDSubject() = %q", got)
	}
}

func TestInstrumentAPIHandlerPreservesWebSocketUpgrade(t *testing.T) {
	registry := metrics.New()
	counters := telemetryCounters{
		requests:  registry.Counter("test_requests", "requests"),
		enqueued:  registry.Counter("test_enqueued", "enqueued"),
		completed: registry.Counter("test_completed", "completed"),
	}

	accepted := make(chan error, 1)
	handler := instrumentAPIHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			accepted <- err
			return
		}
		accepted <- nil
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}), counters)

	// A real TCP server is required: httptest.NewRecorder is not hijackable
	// and would fail even with the fix.
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial through instrumented handler: %v", err)
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	if err := <-accepted; err != nil {
		t.Fatalf("server-side accept through instrumented handler: %v", err)
	}
}

// newServeTestRegistry builds a registry backed by a global database and a
// single project (with its real bare exchange), mirroring how `flow-server
// serve` wires the coordinator. The single project lets unscoped task routes
// resolve implicitly, exactly as a fresh single-project deployment behaves.
func newServeTestRegistry(t *testing.T) (*api.Registry, coordinator.Project) {
	t.Helper()
	return newServeTestRegistryWithHistoryStore(t, nil)
}

func newServeTestRegistryWithHistoryStore(t *testing.T, historyStore blob.Store) (*api.Registry, coordinator.Project) {
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

	registry, err := api.NewRegistry(api.RegistryOptions{DataDir: dataDir, Global: global, HistoryBlobStore: historyStore})
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

type reconcileDespitePublicationFailureStore struct {
	blob.Store
	reconcileCalls int
}

func (s *reconcileDespitePublicationFailureStore) Publish(context.Context, blob.Temporary, blob.Key) (blob.Object, error) {
	return blob.Object{}, errors.New("simulated publication outage")
}

func (s *reconcileDespitePublicationFailureStore) Reconcile(ctx context.Context, request blob.ReconcileRequest) (blob.ReconcileResult, error) {
	s.reconcileCalls++
	return s.Store.Reconcile(ctx, request)
}

func TestHistoryReconciliationContinuesBackendCleanupAfterPublicationFailure(t *testing.T) {
	ctx := context.Background()
	local, err := blob.NewLocal(filepath.Join(t.TempDir(), "blobs"), blob.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	store := &reconcileDespitePublicationFailureStore{Store: local}
	registry, project := newServeTestRegistryWithHistoryStore(t, store)
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatal("project bundle not open")
	}
	reserved, err := bundle.HistoryCaptures.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: project.ID, JobID: "pending-publication", LeaseID: "lease-pending-publication",
		LeaseAttempt: 1, WorkerID: "worker", Role: "author", ExpectedHarness: true,
		HarnessName: "harness", HarnessVersion: "0.4.5", HarnessSchemaVersion: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := bundle.HistoryCaptures.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	pending, err := upload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.HistoryCaptures.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "harness/final/pending", Kind: coordinator.HistoryArtifactHarnessRoot, Phase: coordinator.HistoryArtifactFinal,
		ArchiveID: "pending", MediaType: "application/octet-stream", LogicalSize: 7, EntryCount: 1,
	}, pending); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	abandonedUpload, err := local.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := abandonedUpload.Write([]byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	abandoned, err := abandonedUpload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metricSet := metrics.RegisterHistoryStorage(metrics.New(), "local")
	policy := config.ResolvedHistoryReconciliation{Interval: time.Minute, TemporaryGrace: time.Hour, OrphanGrace: time.Hour, BatchSize: 100}
	result, reconcileErr := reconcileHistoryStorage(ctx, registry, store, policy, metricSet, time.Now().UTC().Add(2*time.Hour))
	if reconcileErr == nil {
		t.Fatal("publication failure was not reported")
	}
	if store.reconcileCalls != 1 {
		t.Fatalf("backend reconciliation calls = %d, want 1", store.reconcileCalls)
	}
	if len(result.RemovedTemporaryIDs) != 1 || result.RemovedTemporaryIDs[0] != abandoned.ID {
		t.Fatalf("backend cleanup result = %+v", result)
	}
	if _, err := local.Resume(ctx, pending.ID); err != nil {
		t.Fatalf("pending publication temporary was not protected: %v", err)
	}
}

func TestHistoryReconciliationRemovesOnlyUnreferencedTemporariesAndReportsPublishedOrphans(t *testing.T) {
	ctx := context.Background()
	registry, project := newServeTestRegistry(t)
	store := registry.HistoryBlobStore()
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatal("project bundle not open")
	}
	reserved, err := bundle.HistoryCaptures.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: project.ID, JobID: "active-history-upload", LeaseID: "lease-active-history-upload",
		LeaseAttempt: 1, WorkerID: "worker", Role: "author", ExpectedHarness: true,
		HarnessName: "harness", HarnessVersion: "0.4.5", HarnessSchemaVersion: 5,
	})
	if err != nil {
		t.Fatalf("reserve history capture: %v", err)
	}
	active, err := bundle.HistoryCaptures.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
	if err != nil {
		t.Fatalf("begin active history upload: %v", err)
	}
	if _, err := active.Write([]byte("active")); err != nil {
		t.Fatalf("write active history upload: %v", err)
	}
	activeTemporary, err := active.Complete(ctx)
	if err != nil {
		t.Fatalf("complete active history upload: %v", err)
	}
	referencedUpload, err := bundle.HistoryCaptures.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := referencedUpload.Write([]byte("referenced")); err != nil {
		t.Fatal(err)
	}
	referencedTemporary, err := referencedUpload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.HistoryCaptures.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "harness/final/referenced", Kind: coordinator.HistoryArtifactHarnessRoot, Phase: coordinator.HistoryArtifactFinal,
		ArchiveID: "referenced", MediaType: "application/octet-stream", LogicalSize: 10, EntryCount: 1,
	}, referencedTemporary); err != nil {
		t.Fatalf("publish referenced history artifact: %v", err)
	}

	abandoned, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := abandoned.Write([]byte("temporary")); err != nil {
		t.Fatal(err)
	}
	abandonedTemporary, err := abandoned.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	published, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := published.Write([]byte("orphan")); err != nil {
		t.Fatal(err)
	}
	publishedTemporary, err := published.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, err := blob.NewKey("history/reconciliation-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, publishedTemporary, key); err != nil {
		t.Fatal(err)
	}

	registryMetrics := metrics.New()
	historyMetrics := metrics.RegisterHistoryStorage(registryMetrics, "local")
	policy := config.ResolvedHistoryReconciliation{
		Interval: time.Minute, TemporaryGrace: time.Hour, OrphanGrace: time.Hour, BatchSize: 100,
	}
	result, err := reconcileHistoryStorage(ctx, registry, store, policy, historyMetrics, time.Now().UTC().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("reconcile history storage: %v", err)
	}
	if len(result.RemovedTemporaryIDs) != 1 || result.RemovedTemporaryIDs[0] != abandonedTemporary.ID || len(result.Orphans) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if _, err := store.Head(ctx, key); err != nil {
		t.Fatalf("reported orphan was deleted: %v", err)
	}
	if _, err := store.Resume(ctx, abandonedTemporary.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("abandoned temporary still exists: %v", err)
	}
	if _, err := store.Resume(ctx, activeTemporary.ID); err != nil {
		t.Fatalf("active-intent temporary was removed: %v", err)
	}
	var exposition bytes.Buffer
	registryMetrics.Render(&exposition)
	if !strings.Contains(exposition.String(), "flow_history_reconciliation_orphans 1") {
		t.Fatalf("metrics do not report orphan:\n%s", exposition.String())
	}
}

func TestHistoryReconciliationContinuesCleanupPastRetainedArtifactBatch(t *testing.T) {
	ctx := context.Background()
	registry, project := newServeTestRegistry(t)
	store := registry.HistoryBlobStore()
	bundle, ok := registry.Bundle(project.ID)
	if !ok {
		t.Fatal("project bundle not open")
	}
	reserved, err := bundle.HistoryCaptures.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: project.ID, JobID: "retained-history", LeaseID: "lease-retained-history",
		LeaseAttempt: 1, WorkerID: "worker", Role: "author", ExpectedHarness: true,
		HarnessName: "harness", HarnessVersion: "0.4.5", HarnessSchemaVersion: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		content := []byte{byte('a' + index)}
		upload, err := bundle.HistoryCaptures.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := upload.Write(content); err != nil {
			t.Fatal(err)
		}
		temporary, err := upload.Complete(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bundle.HistoryCaptures.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, coordinator.PublishHistoryArtifactInput{
			LogicalKey: fmt.Sprintf("harness/final/retained-%d", index), Kind: coordinator.HistoryArtifactHarnessRoot,
			Phase: coordinator.HistoryArtifactFinal, ArchiveID: fmt.Sprintf("retained-%d", index),
			MediaType: "application/octet-stream", LogicalSize: 1, EntryCount: 1,
		}, temporary); err != nil {
			t.Fatal(err)
		}
	}
	abandonedUpload, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := abandonedUpload.Write([]byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	abandoned, err := abandonedUpload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metricSet := metrics.RegisterHistoryStorage(metrics.New(), "local")
	policy := config.ResolvedHistoryReconciliation{
		Interval: time.Minute, TemporaryGrace: time.Hour, OrphanGrace: time.Hour, BatchSize: 1,
	}
	result, err := reconcileHistoryStorage(ctx, registry, store, policy, metricSet, time.Now().UTC().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedTemporaryIDs) != 1 || result.RemovedTemporaryIDs[0] != abandoned.ID {
		t.Fatalf("cleanup past retained artifact batch = %+v", result)
	}
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
			Ref:    "refs/heads/task/seed",
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
	task, err := bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{
		Title: "Serve wiring task",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	server, err := api.NewServer(api.ServerOptions{
		Registry:   registry,
		OwnerToken: "owner-token",
		HookToken:  "hook-token",
	})
	if err != nil {
		t.Fatalf("new serve api: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v2/workers", nil)
	request.Header.Set("Authorization", "Bearer owner-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"workers"`) {
		t.Fatalf("body = %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v2/tasks/"+task.ID+"/checks", nil)
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
