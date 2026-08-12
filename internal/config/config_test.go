package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"gopkg.in/yaml.v3"
)

func TestLoadCoordinatorOverlaysDataDir(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "data")
	configPath := filepath.Join(tempDir, "coordinator.json")

	payload, err := json.Marshal(CoordinatorConfig{
		DataDir:    dataDir,
		ListenAddr: "127.0.0.1:9000",
		AuthorEntrypoint: map[string]any{
			"argv":  []string{"harness", "--continue"},
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
	if !cfg.AuthorEntrypointConfigured {
		t.Fatal("AuthorEntrypointConfigured = false, want true for file override")
	}
	argv, ok := cfg.AuthorEntrypoint["argv"].([]any)
	if !ok || len(argv) != 2 || argv[0] != "harness" || argv[1] != "--continue" {
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

func TestLoadCoordinatorRejectsRemovedExchangeBaseURL(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
exchange_base_url: http://flow-server:8421
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted removed exchange_base_url")
	}
}

func TestLoadCoordinatorParsesHarnessArgs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
harness_args: ["--profile", "review"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	if got := cfg.HarnessArgs; len(got) != 2 || got[0] != "--profile" || got[1] != "review" {
		t.Fatalf("harness args = %#v", got)
	}
}

func TestLoadCoordinatorRejectsManagedHarnessArgs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
harness_args: ["--hooks", "/tmp/evil-hooks.json"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted Flow-managed harness hooks arg")
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

func TestResolveCoordinatorWorkersAppliesReconnectGrace(t *testing.T) {
	resolved, err := (CoordinatorWorkers{}).Resolve()
	if err != nil {
		t.Fatalf("resolve defaults: %v", err)
	}
	if resolved.ReconnectGrace != 2*time.Minute {
		t.Fatalf("ReconnectGrace = %s, want 2m", resolved.ReconnectGrace)
	}

	resolved, err = (CoordinatorWorkers{ReconnectGrace: "5m"}).Resolve()
	if err != nil {
		t.Fatalf("resolve custom grace: %v", err)
	}
	if resolved.ReconnectGrace != 5*time.Minute {
		t.Fatalf("ReconnectGrace = %s, want 5m", resolved.ReconnectGrace)
	}

	resolved, err = (CoordinatorWorkers{ReconnectGrace: "0"}).Resolve()
	if err != nil {
		t.Fatalf("resolve disabled grace: %v", err)
	}
	if resolved.ReconnectGrace != 0 {
		t.Fatalf("ReconnectGrace = %s, want disabled", resolved.ReconnectGrace)
	}
}

func TestResolveCoordinatorWorkersRejectsBadReconnectGrace(t *testing.T) {
	for _, value := range []string{"soon", "-1s"} {
		if _, err := (CoordinatorWorkers{ReconnectGrace: value}).Resolve(); err == nil {
			t.Fatalf("Resolve accepted reconnect grace %q", value)
		}
	}
}

func TestResolveLimitsAppliesReviewConvergencePolicy(t *testing.T) {
	resolved, err := (LimitConfig{}).ResolveLimits()
	if err != nil {
		t.Fatalf("resolve defaults: %v", err)
	}
	if resolved.ReviewAuthorCycles != 2 || resolved.ReviewScopeFiles != 10 || resolved.ReviewScopeLines != 500 {
		t.Fatalf("resolved defaults = %+v, want 2 cycles, 10 files, and 500 lines", resolved)
	}

	resolved, err = (LimitConfig{
		ReviewAuthorCycles: 4,
		ReviewScopeFiles:   12,
		ReviewScopeLines:   1500,
	}).ResolveLimits()
	if err != nil {
		t.Fatalf("resolve explicit limits: %v", err)
	}
	if resolved.ReviewAuthorCycles != 4 || resolved.ReviewScopeFiles != 12 || resolved.ReviewScopeLines != 1500 {
		t.Fatalf("resolved explicit limits = %+v", resolved)
	}
}

func TestResolveLimitsRejectsNegativeValues(t *testing.T) {
	tests := []LimitConfig{
		{ReviewAuthorCycles: -1},
		{ReviewScopeFiles: -1},
		{ReviewScopeLines: -1},
	}
	for _, limits := range tests {
		if _, err := limits.ResolveLimits(); err == nil {
			t.Fatalf("ResolveLimits accepted %+v", limits)
		}
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

func TestLoadCoordinatorParsesWorkerReconnectGrace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
workers:
  reconnect_grace: "90s"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	resolved, err := cfg.Workers.Resolve()
	if err != nil {
		t.Fatalf("resolve worker policy: %v", err)
	}
	if resolved.ReconnectGrace != 90*time.Second {
		t.Fatalf("ReconnectGrace = %s, want 90s", resolved.ReconnectGrace)
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

func TestLoadCoordinatorParsesDefaultAgent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
default_agent:
  harness: " Harness "
  model: anthropic:claude-sonnet-4-6
  reasoning_effort: high
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	if cfg.AuthorEntrypointConfigured {
		t.Fatal("AuthorEntrypointConfigured = true, want false for default_agent-only config")
	}
	resolved, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		t.Fatalf("resolve default agent: %v", err)
	}
	want := flowharness.AgentSelection{Harness: flowharness.Harness, Model: "anthropic:claude-sonnet-4-6", ReasoningEffort: "high"}
	if resolved != want {
		t.Fatalf("resolved default agent = %+v, want %+v", resolved, want)
	}
	// The default author entrypoint is rebuilt from the configured selection.
	if cfg.AuthorEntrypoint["harness"] != flowharness.Harness {
		t.Fatalf("author entrypoint harness = %#v, want harness", cfg.AuthorEntrypoint["harness"])
	}
	argv, ok := cfg.AuthorEntrypoint["argv"].([]string)
	if !ok || len(argv) != 1 {
		t.Fatalf("author entrypoint argv = %#v", cfg.AuthorEntrypoint["argv"])
	}
	for _, token := range []string{`harness --session "$FLOW_HARNESS_SESSION"`, `--hooks "$FLOW_HARNESS_HOOKS"`, "'--model' 'anthropic:claude-sonnet-4-6'", "'--reasoning' 'high'"} {
		if !strings.Contains(argv[0], token) {
			t.Fatalf("author entrypoint command missing %q:\n%s", token, argv[0])
		}
	}
}

func TestLoadCoordinatorDefaultAgentModelWithoutHarnessAppliesToHarness(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
default_agent:
  model: openai:gpt-5
  reasoning_effort: high
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	resolved, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		t.Fatalf("resolve default agent: %v", err)
	}
	if resolved.Harness != flowharness.Harness {
		t.Fatalf("resolved harness = %q, want harness", resolved.Harness)
	}
	if cfg.AuthorEntrypoint["harness"] != flowharness.Harness {
		t.Fatalf("author entrypoint harness = %#v, want harness", cfg.AuthorEntrypoint["harness"])
	}
	argv, ok := cfg.AuthorEntrypoint["argv"].([]string)
	if !ok || len(argv) != 1 {
		t.Fatalf("author entrypoint argv = %#v", cfg.AuthorEntrypoint["argv"])
	}
	for _, token := range []string{"'--model' 'openai:gpt-5'", "'--reasoning' 'high'"} {
		if !strings.Contains(argv[0], token) {
			t.Fatalf("author entrypoint command missing %q:\n%s", token, argv[0])
		}
	}
}

func TestLoadCoordinatorRejectsInvalidDefaultAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
	}{
		{name: "unknown harness", block: "harness: bogus"},
		{name: "non-agent harness", block: "harness: shell"},
		{name: "whitespace model", block: "model: \"gpt 5\""},
		{name: "whitespace effort", block: "reasoning_effort: \"very high\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
			if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
default_agent:
  `+tc.block+`
`), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadCoordinator(configPath)
			if err == nil || !strings.Contains(err.Error(), "coordinator default_agent") {
				t.Fatalf("load error = %v, want coordinator default_agent rejection", err)
			}
		})
	}
}

func TestLoadCoordinatorAuthorEntrypointWinsOverDefaultAgent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
author_entrypoint:
  argv: ["harness --continue"]
  shell: true
  harness: harness
default_agent:
  model: openai:gpt-5
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	if !cfg.AuthorEntrypointConfigured {
		t.Fatal("AuthorEntrypointConfigured = false, want true for file override")
	}
	argv, ok := cfg.AuthorEntrypoint["argv"].([]any)
	if !ok || len(argv) != 1 || argv[0] != "harness --continue" {
		t.Fatalf("author entrypoint argv = %#v, want the explicit file entrypoint", cfg.AuthorEntrypoint["argv"])
	}
	// The selection still resolves for the other consumers (agent-def seeding,
	// default checks); only the author entrypoint keeps the explicit override.
	resolved, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		t.Fatalf("resolve default agent: %v", err)
	}
	if resolved.Harness != flowharness.Harness || resolved.Model != "openai:gpt-5" {
		t.Fatalf("resolved default agent = %+v, want harness/openai:gpt-5", resolved)
	}
}

func TestLoadCoordinatorZeroConfigKeepsHarnessDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	resolved, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		t.Fatalf("resolve default agent: %v", err)
	}
	if resolved != flowharness.DefaultAgentSelection() {
		t.Fatalf("resolved default agent = %+v, want the built-in default", resolved)
	}
	if cfg.AuthorEntrypoint["harness"] != flowharness.Harness {
		t.Fatalf("author entrypoint harness = %#v, want harness", cfg.AuthorEntrypoint["harness"])
	}
}

func TestDefaultAuthorEntrypointUsesHarnessHooks(t *testing.T) {
	entrypoint := DefaultAuthorEntrypoint()
	argv, ok := entrypoint["argv"].([]string)
	if !ok || len(argv) != 1 {
		t.Fatalf("argv = %#v", entrypoint["argv"])
	}
	if entrypoint["shell"] != true {
		t.Fatalf("shell = %#v, want true", entrypoint["shell"])
	}
	if !strings.Contains(argv[0], "flow fetch-prompt --harness harness") {
		t.Fatalf("default command does not fetch a prompt: %q", argv[0])
	}
	if !strings.Contains(argv[0], `harness --session "$FLOW_HARNESS_SESSION" --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`) {
		t.Fatalf("default command does not configure harness native hooks: %q", argv[0])
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
	expectedOS := runtime.GOOS
	if expectedOS == "darwin" {
		expectedOS = "macos"
	}
	if cfg.Labels["os"] != expectedOS || cfg.Labels["arch"] != runtime.GOARCH {
		t.Fatalf("platform labels = %#v, want os=%s arch=%s", cfg.Labels, expectedOS, runtime.GOARCH)
	}
	if cfg.CoordinatorURL == "" {
		t.Fatal("CoordinatorURL is empty")
	}
	if cfg.WorkDir == "" {
		t.Fatal("WorkDir is empty")
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
  agent.harness.harness: "true"
cleanup:
  interval: 2m
  orphan_grace: 30m
  min_free_bytes: 2GiB
  resume_free_bytes: 4GiB
  min_free_percent: 8
  resume_free_percent: 12
git:
  principal: worker:w-local
  commit_name: Flow Bot
  commit_email: flow-bot@example.com
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
	if cfg.Labels["local"] != "true" || cfg.Labels["agent.harness.harness"] != "true" {
		t.Fatalf("Labels = %#v", cfg.Labels)
	}
	cleanup, err := cfg.Cleanup.Resolve()
	if err != nil {
		t.Fatalf("resolve cleanup: %v", err)
	}
	if cleanup.Interval != 2*time.Minute || cleanup.OrphanGrace != 30*time.Minute {
		t.Fatalf("cleanup durations = %+v", cleanup)
	}
	if cleanup.MinFreeBytes != 2<<30 || cleanup.ResumeFreeBytes != 4<<30 {
		t.Fatalf("cleanup byte thresholds = %+v", cleanup)
	}
	if cleanup.MinFreePercent != 8 || cleanup.ResumeFreePercent != 12 {
		t.Fatalf("cleanup percentage thresholds = %+v", cleanup)
	}
	if cfg.Git.Principal != "worker:w-local" {
		t.Fatalf("Git.Principal = %q", cfg.Git.Principal)
	}
	if cfg.Git.CommitName != "Flow Bot" || cfg.Git.CommitEmail != "flow-bot@example.com" {
		t.Fatalf("Git commit identity = %+v", cfg.Git)
	}
	if strings.Contains(cfg.WorkDir, "~") {
		t.Fatalf("WorkDir was not expanded: %q", cfg.WorkDir)
	}
}

func TestLoadWorkerRejectsConflictingDetectedPlatformLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, []byte(`coordinator_url: http://127.0.0.1:8421
work_dir: /tmp/worker
labels:
  arch: definitely-not-this-architecture
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}
	if _, err := LoadWorker(path); err == nil || !strings.Contains(err.Error(), "conflicts with detected platform") {
		t.Fatalf("LoadWorker platform conflict error = %v", err)
	}
}

func TestLoadWorkerHarnessConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write worker config: %v", err)
		}
	}

	write("coordinator_url: http://127.0.0.1:8421\nwork_dir: " + filepath.Join(dir, "work") + "\nharness_config_file:  /etc/flow/harness/config.json  \n")
	cfg, err := LoadWorker(path)
	if err != nil {
		t.Fatalf("load worker config: %v", err)
	}
	if cfg.HarnessConfigFile != "/etc/flow/harness/config.json" {
		t.Fatalf("HarnessConfigFile = %q, want trimmed absolute path", cfg.HarnessConfigFile)
	}

	write("coordinator_url: http://127.0.0.1:8421\nwork_dir: " + filepath.Join(dir, "work") + "\n")
	cfg, err = LoadWorker(path)
	if err != nil {
		t.Fatalf("load worker config: %v", err)
	}
	if cfg.HarnessConfigFile != "" {
		t.Fatalf("HarnessConfigFile = %q, want empty", cfg.HarnessConfigFile)
	}

	for _, value := range []string{"relative/harness.json", "/etc/flow/../harness.json", "etc/harness.json"} {
		write("coordinator_url: http://127.0.0.1:8421\nwork_dir: " + filepath.Join(dir, "work") + "\nharness_config_file: " + value + "\n")
		if _, err := LoadWorker(path); err == nil || !strings.Contains(err.Error(), "harness_config_file must be an absolute, clean path") {
			t.Fatalf("LoadWorker(harness_config_file %q) error = %v, want absolute/clean rejection", value, err)
		}
	}
}

func float64Ptr(v float64) *float64 { return &v }

func TestWorkerCleanupDefaultsAndValidation(t *testing.T) {
	resolved, err := (WorkerCleanup{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve defaults: %v", err)
	}
	if resolved.Interval != 5*time.Minute || resolved.OrphanGrace != time.Hour {
		t.Fatalf("default durations = %+v", resolved)
	}
	if resolved.MinFreeBytes != 0 || resolved.ResumeFreeBytes != 0 {
		t.Fatalf("default byte thresholds = %+v, want disabled", resolved)
	}
	if resolved.MinFreePercent != 10 || resolved.ResumeFreePercent != 15 {
		t.Fatalf("default percentage thresholds = %+v", resolved)
	}

	zero := 0.0
	disabled, err := (WorkerCleanup{MinFreePercent: &zero, ResumeFreePercent: &zero}).Resolve()
	if err != nil {
		t.Fatalf("Resolve explicit zero percentages: %v", err)
	}
	if disabled.MinFreePercent != 0 || disabled.ResumeFreePercent != 0 {
		t.Fatalf("explicit zero percentages resolved to %+v, want the percentage check disabled", disabled)
	}

	for name, cleanup := range map[string]WorkerCleanup{
		"bad interval": {
			Interval: "later",
		},
		"zero interval": {
			Interval: "0",
		},
		"bad bytes": {
			MinFreeBytes: "lots",
		},
		"resume bytes below minimum": {
			MinFreeBytes:    "2GiB",
			ResumeFreeBytes: "1GiB",
		},
		"resume percent below minimum": {
			MinFreePercent:    float64Ptr(20),
			ResumeFreePercent: float64Ptr(15),
		},
		"non-finite percent": {
			MinFreePercent: float64Ptr(math.NaN()),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cleanup.Resolve(); err == nil {
				t.Fatalf("Resolve(%+v) succeeded, want validation error", cleanup)
			}
		})
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

func TestLoadWorkerYAMLRejectsRemovedExchangeURL(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://flow-server:8421
work_dir: /tmp/worker
git:
  exchange_url: http://127.0.0.1:8421/git/projects/p-flow/exchange.git
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	if _, err := LoadWorker(configPath); err == nil {
		t.Fatal("LoadWorker accepted removed git.exchange_url")
	}
}

func TestApplyWorkerEnvOverrides(t *testing.T) {
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load default worker: %v", err)
	}
	getenv := func(key string) string {
		values := map[string]string{
			"FLOW_WORKER_ID":               "w-env",
			"FLOW_WORKER_COORDINATOR_URL":  "http://flow-server:8421",
			"FLOW_WORKER_TOKEN":            "worker-env-token",
			"FLOW_WORKER_WORK_DIR":         "~/flow-worker",
			"FLOW_WORKER_GIT_PRINCIPAL":    "worker:w-env",
			"FLOW_WORKER_GIT_COMMIT_NAME":  "Flow Bot",
			"FLOW_WORKER_GIT_COMMIT_EMAIL": "flow-bot@example.com",
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
	if cfg.Git.Principal != "worker:w-env" {
		t.Fatalf("Git = %+v", cfg.Git)
	}
	if cfg.Git.CommitName != "Flow Bot" || cfg.Git.CommitEmail != "flow-bot@example.com" {
		t.Fatalf("Git commit identity = %+v", cfg.Git)
	}
}

func TestLoadCoordinatorGitCommitIdentity(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
git:
  commit_name: Flow Bot
  commit_email: flow-bot@example.com
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadCoordinator(configPath)
	if err != nil {
		t.Fatalf("load coordinator: %v", err)
	}
	if cfg.Git.CommitName != "Flow Bot" || cfg.Git.CommitEmail != "flow-bot@example.com" {
		t.Fatalf("Git commit identity = %+v", cfg.Git)
	}
}

func TestApplyCoordinatorEnvOverrides(t *testing.T) {
	cfg, err := LoadCoordinator("")
	if err != nil {
		t.Fatalf("load default coordinator: %v", err)
	}

	cfg, err = ApplyCoordinatorEnvOverrides(cfg, func(key string) string {
		values := map[string]string{
			"FLOW_GIT_COMMIT_NAME":  "Flow Bot",
			"FLOW_GIT_COMMIT_EMAIL": "flow-bot@example.com",
		}
		return values[key]
	})
	if err != nil {
		t.Fatalf("apply coordinator env overrides: %v", err)
	}
	if cfg.Git.CommitName != "Flow Bot" || cfg.Git.CommitEmail != "flow-bot@example.com" {
		t.Fatalf("Git commit identity = %+v", cfg.Git)
	}
}

func TestCoordinatorRejectsHalfSetCommitIdentity(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(configPath, []byte(`data_dir: /tmp/flow
listen_addr: 127.0.0.1:8421
git:
  commit_name: Flow Bot
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadCoordinator(configPath); err == nil {
		t.Fatal("LoadCoordinator accepted a name without an email")
	}
}

func TestWorkerRejectsHalfSetCommitIdentity(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
work_dir: /tmp/worker
git:
  commit_email: flow-bot@example.com
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadWorker(configPath); err == nil {
		t.Fatal("LoadWorker accepted an email without a name")
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

	cfg, err := LocalClientConfig(dataDir, "http://127.0.0.1:9000", "owner-token")
	if err != nil {
		t.Fatalf("local client config: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:9000" || cfg.DataDir != dataDir {
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
