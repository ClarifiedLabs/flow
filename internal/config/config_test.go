package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"gopkg.in/yaml.v3"
)

func TestLoadCoordinatorOverlaysDataDir(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "data")
	configPath := filepath.Join(tempDir, "coordinator.json")

	payload, err := json.Marshal(CoordinatorConfig{
		DataDir:         dataDir,
		ListenAddr:      "127.0.0.1:9000",
		ProtocolVersion: "2",
		AuthorEntrypoint: map[string]any{
			"argv":  []string{"claude", "--continue"},
			"cwd":   "agents",
			"env":   map[string]string{"CUSTOM": "true"},
			"shell": false,
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, payload, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}

	if cfg.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, dataDir)
	}
	if want := filepath.Join(dataDir, "global.db"); cfg.GlobalDatabasePath() != want {
		t.Fatalf("GlobalDatabasePath = %q, want %q", cfg.GlobalDatabasePath(), want)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.ProtocolVersion != "2" {
		t.Fatalf("ProtocolVersion = %q", cfg.ProtocolVersion)
	}
	if !cfg.AuthorEntrypointConfigured {
		t.Fatal("AuthorEntrypointConfigured = false, want true for file override")
	}
	argv, ok := cfg.AuthorEntrypoint["argv"].([]any)
	if !ok || len(argv) != 2 || argv[0] != "claude" || argv[1] != "--continue" {
		t.Fatalf("AuthorEntrypoint argv = %#v", cfg.AuthorEntrypoint["argv"])
	}
}

func TestLoadCoordinatorRejectsInvalidAuthorEntrypoint(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
author_entrypoint:
  argv: []
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted empty author_entrypoint argv")
	}
}

func TestLoadCoordinatorParsesHarnessArgs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
harness_args:
  codex: ["--model", "gpt-5"]
  claude: ["--model", "sonnet"]
  harness: ["--profile", "review"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	if got := cfg.HarnessArgs.Codex; len(got) != 2 || got[0] != "--model" || got[1] != "gpt-5" {
		t.Fatalf("codex harness args = %#v", got)
	}
	if got := cfg.HarnessArgs.For(flowharness.Harness); len(got) != 2 || got[0] != "--profile" || got[1] != "review" {
		t.Fatalf("harness args = %#v", got)
	}
}

func TestLoadCoordinatorRejectsManagedHarnessArgs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
harness_args:
  codex: ["-c", "hooks.Stop=[]"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted Flow-managed codex hook config")
	}
}

func TestResolveDeadlinesAppliesDefaults(t *testing.T) {
	// Empty/omitted values take the coordinator defaults.
	resolved, err := DeadlineConfig{}.ResolveDeadlines()
	if err != nil {
		t.Fatalf("resolve defaults: %v", err)
	}
	if resolved.CheckPending != defaultCheckPendingDeadline {
		t.Fatalf("CheckPending = %s, want %s", resolved.CheckPending, defaultCheckPendingDeadline)
	}
	if resolved.AuthoringStall != defaultAuthoringStallDeadline {
		t.Fatalf("AuthoringStall = %s, want %s", resolved.AuthoringStall, defaultAuthoringStallDeadline)
	}

	// Explicit values override; "0" disables.
	resolved, err = DeadlineConfig{CheckPending: "5m", AuthoringStall: "0"}.ResolveDeadlines()
	if err != nil {
		t.Fatalf("resolve explicit: %v", err)
	}
	if resolved.CheckPending.Minutes() != 5 || resolved.AuthoringStall != 0 {
		t.Fatalf("resolved = %+v, want 5m/0", resolved)
	}
}

func TestResolveDeadlinesRejectsBadDuration(t *testing.T) {
	if _, err := (DeadlineConfig{CheckPending: "soon"}).ResolveDeadlines(); err == nil {
		t.Fatal("ResolveDeadlines accepted an unparseable duration")
	}
	if _, err := (DeadlineConfig{AuthoringStall: "-2h"}).ResolveDeadlines(); err == nil {
		t.Fatal("ResolveDeadlines accepted a negative duration")
	}
}

func TestLoadCoordinatorParsesDeadlines(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
deadlines:
  check_pending: "15m"
  authoring_stall: "1h"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	resolved, err := cfg.Deadlines.ResolveDeadlines()
	if err != nil {
		t.Fatalf("resolve deadlines: %v", err)
	}
	if resolved.CheckPending.Minutes() != 15 {
		t.Fatalf("CheckPending = %s, want 15m", resolved.CheckPending)
	}
	if resolved.AuthoringStall.Hours() != 1 {
		t.Fatalf("AuthoringStall = %s, want 1h", resolved.AuthoringStall)
	}
}

func TestLoadCoordinatorRejectsBadDeadline(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
deadlines:
  check_pending: "nope"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted an unparseable deadline duration")
	}
}

func TestDefaultAuthorEntrypointWrapsCodexHooks(t *testing.T) {
	entrypoint := DefaultAuthorEntrypoint()
	argv, ok := entrypoint["argv"].([]string)
	if !ok || len(argv) != 1 {
		t.Fatalf("argv = %#v", entrypoint["argv"])
	}
	if entrypoint["shell"] != true {
		t.Fatalf("shell = %#v, want true", entrypoint["shell"])
	}
	if !strings.Contains(argv[0], "flow hook codex start") || !strings.Contains(argv[0], "flow hook codex stop") {
		t.Fatalf("default command does not wrap hooks: %q", argv[0])
	}
	if !strings.Contains(argv[0], `-c "projects.$PWD.trust_level=trusted"`) {
		t.Fatalf("default command does not trust the job worktree: %q", argv[0])
	}
	if !strings.Contains(argv[0], "--dangerously-bypass-hook-trust") || !strings.Contains(argv[0], "flow hook codex ingest") {
		t.Fatalf("default command does not configure Codex native hooks: %q", argv[0])
	}
	if !strings.Contains(argv[0], "flow fetch-prompt") || !strings.Contains(argv[0], `"$prompt"`) {
		t.Fatalf("default command does not start codex with a prompt: %q", argv[0])
	}
}

func TestLoadClientRejectsUnknownFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(configPath, []byte(`{"server_url":"http://127.0.0.1:1","extra":true}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadClient(configPath); err == nil {
		t.Fatal("LoadClient accepted an unknown field")
	}
}

func TestLoadWorkerDefaultsLabels(t *testing.T) {
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load worker defaults: %v", err)
	}

	if cfg.Labels == nil {
		t.Fatal("Labels is nil")
	}
	if cfg.CoordinatorURL == "" {
		t.Fatal("CoordinatorURL is empty")
	}
	if cfg.WorkDir == "" {
		t.Fatal("WorkDir is empty")
	}
	if cfg.ProtocolVersion != DefaultProtocolVersion {
		t.Fatalf("ProtocolVersion = %q, want %q", cfg.ProtocolVersion, DefaultProtocolVersion)
	}
}

func TestLoadWorkerYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.local.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
token: worker-secret
work_dir: ~/flow/workers/local
labels:
  local: "true"
  agent.harness.codex: "true"
capacity:
  persistent_agent: 1
  ephemeral: 2
terminal:
  bind_address: 100.64.1.2
  public_base_url: http://100.64.1.2
  ttyd_path: /tmp/flow-test-ttyd
tmux:
  socket_path: /tmp/flow-test-tmux.sock
git:
  exchange_url: file:///tmp/flow.git
  principal: worker:w-local
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	cfg, err := LoadWorker(configPath)
	if err != nil {
		t.Fatalf("load worker config: %v", err)
	}

	if cfg.WorkerID != "w-local" {
		t.Fatalf("WorkerID = %q", cfg.WorkerID)
	}
	if cfg.Token != "worker-secret" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.ProtocolVersion != DefaultProtocolVersion {
		t.Fatalf("ProtocolVersion = %q, want %q", cfg.ProtocolVersion, DefaultProtocolVersion)
	}
	if cfg.Labels["local"] != "true" || cfg.Labels["agent.harness.codex"] != "true" {
		t.Fatalf("Labels = %#v", cfg.Labels)
	}
	if cfg.Capacity.PersistentAgent != 1 || cfg.Capacity.Ephemeral != 2 {
		t.Fatalf("Capacity = %+v", cfg.Capacity)
	}
	if cfg.Terminal.BindAddress != "100.64.1.2" ||
		cfg.Terminal.PublicBaseURL != "http://100.64.1.2" ||
		cfg.Terminal.TTYDPath != "/tmp/flow-test-ttyd" {
		t.Fatalf("Terminal = %+v", cfg.Terminal)
	}
	if cfg.Tmux.SocketPath != "/tmp/flow-test-tmux.sock" {
		t.Fatalf("Tmux = %+v", cfg.Tmux)
	}
	if cfg.Git.ExchangeURL != "file:///tmp/flow.git" {
		t.Fatalf("Git.ExchangeURL = %q", cfg.Git.ExchangeURL)
	}
	if cfg.Git.Principal != "worker:w-local" {
		t.Fatalf("Git.Principal = %q", cfg.Git.Principal)
	}
	if strings.Contains(cfg.WorkDir, "~") {
		t.Fatalf("WorkDir was not expanded: %q", cfg.WorkDir)
	}
}

func TestLoadWorkerYAMLRejectsUnknownFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
work_dir: /tmp/worker
extra: true
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	if _, err := LoadWorker(configPath); err == nil {
		t.Fatal("LoadWorker accepted an unknown YAML field")
	}
}

func TestApplyWorkerEnvOverrides(t *testing.T) {
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load default worker: %v", err)
	}
	getenv := func(key string) string {
		values := map[string]string{
			"FLOW_WORKER_ID":                        "w-env",
			"FLOW_WORKER_COORDINATOR_URL":           "http://flow-server:8421",
			"FLOW_WORKER_TOKEN":                     "worker-env-token",
			"FLOW_WORKER_WORK_DIR":                  "~/flow-worker",
			"FLOW_WORKER_CAPACITY_PERSISTENT_AGENT": "3",
			"FLOW_WORKER_CAPACITY_EPHEMERAL":        "4",
			"FLOW_WORKER_TERMINAL_BIND_ADDRESS":     "0.0.0.0",
			"FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL":  "http://worker.local",
			"FLOW_WORKER_TERMINAL_TTYD_PATH":        "/tmp/ttyd",
			"FLOW_WORKER_TMUX_SOCKET_PATH":          "/tmp/tmux.sock",
			"FLOW_WORKER_GIT_EXCHANGE_URL":          "https://example.test/exchange.git",
			"FLOW_WORKER_GIT_PRINCIPAL":             "worker:w-env",
			"FLOW_WORKER_PROTOCOL_VERSION":          "2",
		}
		return values[key]
	}

	cfg, err = ApplyWorkerEnvOverrides(cfg, getenv)
	if err != nil {
		t.Fatalf("apply worker env overrides: %v", err)
	}
	if cfg.WorkerID != "w-env" || cfg.CoordinatorURL != "http://flow-server:8421" || cfg.Token != "worker-env-token" {
		t.Fatalf("identity/token fields = %+v", cfg)
	}
	if strings.Contains(cfg.WorkDir, "~") {
		t.Fatalf("WorkDir was not expanded: %q", cfg.WorkDir)
	}
	if cfg.Capacity.PersistentAgent != 3 || cfg.Capacity.Ephemeral != 4 {
		t.Fatalf("Capacity = %+v", cfg.Capacity)
	}
	if cfg.Terminal.BindAddress != "0.0.0.0" || cfg.Terminal.PublicBaseURL != "http://worker.local" || cfg.Terminal.TTYDPath != "/tmp/ttyd" {
		t.Fatalf("Terminal = %+v", cfg.Terminal)
	}
	if cfg.Tmux.SocketPath != "/tmp/tmux.sock" {
		t.Fatalf("Tmux = %+v", cfg.Tmux)
	}
	if cfg.Git.ExchangeURL != "https://example.test/exchange.git" || cfg.Git.Principal != "worker:w-env" {
		t.Fatalf("Git = %+v", cfg.Git)
	}
	if cfg.ProtocolVersion != "2" {
		t.Fatalf("ProtocolVersion = %q, want 2", cfg.ProtocolVersion)
	}
}

func TestApplyWorkerEnvOverridesRejectsInvalidCapacity(t *testing.T) {
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load default worker: %v", err)
	}

	_, err = ApplyWorkerEnvOverrides(cfg, func(key string) string {
		if key == "FLOW_WORKER_CAPACITY_EPHEMERAL" {
			return "many"
		}
		return ""
	})
	if err == nil {
		t.Fatal("ApplyWorkerEnvOverrides accepted invalid capacity")
	}
}

func TestDefaultClientConfigPathUsesXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if want := filepath.Join(configHome, "flow", "config.yaml"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestDefaultDataDirUsesFlowDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "custom-flow")
	t.Setenv("FLOW_DATA_DIR", dataDir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("default data dir: %v", err)
	}
	if got != dataDir {
		t.Fatalf("DefaultDataDir = %q, want %q", got, dataDir)
	}
}

func TestLoadClientAutoDiscoversDefaultConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	// Without a config file, the defaults apply.
	cfg, err := LoadClient("")
	if err != nil {
		t.Fatalf("load client without config file: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:8421" || cfg.Token != "" {
		t.Fatalf("defaults = %+v", cfg)
	}

	if err := os.MkdirAll(filepath.Join(configHome, "flow"), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	dataDir := filepath.Join(t.TempDir(), "flow-data")
	contents := "server_url: http://127.0.0.1:9999\ntoken: secret\ndata_dir: " + filepath.ToSlash(dataDir) + "\n"
	if err := os.WriteFile(filepath.Join(configHome, "flow", "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err = LoadClient("")
	if err != nil {
		t.Fatalf("load client with discovered config: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:9999" {
		t.Fatalf("ServerURL = %q, want discovered value", cfg.ServerURL)
	}
	if cfg.Token != "secret" {
		t.Fatalf("Token = %q, want secret", cfg.Token)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, dataDir)
	}
}

func TestLoadClientResolvesTokenFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	tokenPath := filepath.Join(t.TempDir(), "owner.token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "flow"), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	contents := "server_url: http://127.0.0.1:8421\ntoken_file: " + tokenPath + "\n"
	if err := os.WriteFile(filepath.Join(configHome, "flow", "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadClient("")
	if err != nil {
		t.Fatalf("load client: %v", err)
	}
	if cfg.Token != "file-token" {
		t.Fatalf("Token = %q, want file-token", cfg.Token)
	}

	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	if _, err := LoadClient(""); err == nil {
		t.Fatal("group/other-readable token file was accepted")
	}
}

func TestResolveOwnerTokenFallbackUsesConfiguredDataDir(t *testing.T) {
	t.Setenv("FLOW_DATA_DIR", filepath.Join(t.TempDir(), "default-flow"))
	dataDir := t.TempDir()
	tokenPath := OwnerTokenPath(dataDir)
	if err := os.WriteFile(tokenPath, []byte("owner-from-data-dir\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}

	token, path, ok, err := ResolveOwnerTokenFallback(ClientConfig{DataDir: dataDir})
	if err != nil {
		t.Fatalf("resolve owner token: %v", err)
	}
	if !ok || token != "owner-from-data-dir" || path != tokenPath {
		t.Fatalf("fallback = token:%q path:%q ok:%t, want configured token", token, path, ok)
	}
}

func TestLocalClientConfigReferencesOwnerTokenFile(t *testing.T) {
	dataDir := t.TempDir()
	tokenPath := OwnerTokenPath(dataDir)
	if err := os.WriteFile(tokenPath, []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}

	cfg, err := LocalClientConfig(dataDir, "http://127.0.0.1:9000", "owner-token", "2")
	if err != nil {
		t.Fatalf("local client config: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:9000" || cfg.ProtocolVersion != "2" || cfg.DataDir != dataDir {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Token != "" || cfg.TokenFile != tokenPath {
		t.Fatalf("token fields = token:%q token_file:%q, want token_file", cfg.Token, cfg.TokenFile)
	}
}

func TestWriteClientConfigRoundTrips(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	if err := WriteClientConfig(path, ClientConfig{
		ServerURL: "http://127.0.0.1:8421",
		TokenFile: "/tmp/owner.token",
		DataDir:   "/tmp/flow",
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config mode = %v, want private", info.Mode().Perm())
	}

	var raw struct {
		ServerURL string `yaml:"server_url"`
		TokenFile string `yaml:"token_file"`
		DataDir   string `yaml:"data_dir"`
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := yaml.Unmarshal(contents, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if raw.ServerURL != "http://127.0.0.1:8421" || raw.TokenFile != "/tmp/owner.token" || raw.DataDir != "/tmp/flow" {
		t.Fatalf("written config = %+v", raw)
	}
}

func TestResolveWorkerConfigPathUsesClientDataDir(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("FLOW_DATA_DIR", filepath.Join(t.TempDir(), "default-flow"))
	dataDir := t.TempDir()
	configPath, err := DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := WriteClientConfig(configPath, ClientConfig{
		ServerURL: "http://127.0.0.1:8421",
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	resolved, err := ResolveWorkerConfigPath("")
	if err != nil {
		t.Fatalf("resolve worker config path: %v", err)
	}
	if want := filepath.Join(dataDir, "worker.yaml"); resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}
