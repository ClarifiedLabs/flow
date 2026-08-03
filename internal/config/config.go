package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"gopkg.in/yaml.v3"
)

type CoordinatorConfig struct {
	DataDir                    string                   `json:"data_dir" yaml:"data_dir"`
	ListenAddr                 string                   `json:"listen_addr" yaml:"listen_addr"`
	AuthorEntrypoint           map[string]any           `json:"author_entrypoint" yaml:"author_entrypoint"`
	AuthorEntrypointConfigured bool                     `json:"-" yaml:"-"`
	DefaultAgent               DefaultAgentConfig       `json:"default_agent" yaml:"default_agent"`
	Deadlines                  DeadlineConfig           `json:"deadlines" yaml:"deadlines"`
	Limits                     LimitConfig              `json:"limits" yaml:"limits"`
	Workers                    CoordinatorWorkers       `json:"workers" yaml:"workers"`
	HarnessArgs                []string                 `json:"harness_args" yaml:"harness_args"`
	Git                        CoordinatorGitConfig     `json:"git" yaml:"git"`
	Metrics                    metrics.Config           `json:"metrics" yaml:"metrics"`
	History                    CoordinatorHistoryConfig `json:"history" yaml:"history"`
}

// DefaultAgentConfig configures the coordinator's fallback agent: the harness
// (and optional model/reasoning effort) used for jobs without an explicit
// agent definition, and for the default author entrypoint when
// author_entrypoint is unset. The zero value keeps the built-in default: the
// harness CLI with its own default model.
type DefaultAgentConfig struct {
	Harness         string `json:"harness" yaml:"harness"`
	Model           string `json:"model" yaml:"model"`
	ReasoningEffort string `json:"reasoning_effort" yaml:"reasoning_effort"`
}

// CoordinatorWorkers configures coordinator-side worker lifecycle policy.
type CoordinatorWorkers struct {
	// ReconnectGrace reserves active leases while workers reconnect after a
	// coordinator restart (default 2m). "0" disables restart protection.
	ReconnectGrace string `json:"reconnect_grace" yaml:"reconnect_grace"`
}

// ResolvedCoordinatorWorkers is the parsed, default-applied worker lifecycle
// policy used by the coordinator runtime.
type ResolvedCoordinatorWorkers struct {
	ReconnectGrace time.Duration
}

// CoordinatorGitConfig configures the git commit identity the coordinator uses
// for the commits it creates (the squash-merge commit landing a change on the
// base branch). When unset, the built-in default identity is used.
type CoordinatorGitConfig struct {
	CommitName  string `json:"commit_name" yaml:"commit_name"`
	CommitEmail string `json:"commit_email" yaml:"commit_email"`
}

// DeadlineConfig bounds otherwise-unbounded waits in the lifecycle. Each value
// is a Go duration string (e.g. "30m", "2h"); an empty string means "use the
// default", and "0" explicitly disables that deadline. ResolveDeadlines applies
// the defaults and parses the durations.
type DeadlineConfig struct {
	// CheckPending bounds a pending check with no report (default 30m).
	CheckPending string `json:"check_pending" yaml:"check_pending"`
	// AuthoringStall bounds planning/authoring with no agent activity
	// (default 2h).
	AuthoringStall string `json:"authoring_stall" yaml:"authoring_stall"`
}

// ResolvedDeadlines is the parsed, default-applied form of DeadlineConfig.
type ResolvedDeadlines struct {
	CheckPending   time.Duration
	AuthoringStall time.Duration
}

// LimitConfig configures bounded automation loops.
type LimitConfig struct {
	// ReviewAuthorCycles limits how many times a task may be automatically
	// sent from review/acceptance back to authoring before a human grants more.
	// The default is 2 so repeated send-backs become an explicit human
	// convergence decision before they turn into an unbounded redesign.
	ReviewAuthorCycles int `json:"review_author_cycles" yaml:"review_author_cycles"`
	// ReviewScopeFiles and ReviewScopeLines pause an oversized change before
	// automated review so a human can split or explicitly accept the scope.
	// Defaults are 10 files and 500 added/deleted lines.
	ReviewScopeFiles int `json:"review_scope_files" yaml:"review_scope_files"`
	ReviewScopeLines int `json:"review_scope_lines" yaml:"review_scope_lines"`
}

type ResolvedLimits struct {
	ReviewAuthorCycles int
	ReviewScopeFiles   int
	ReviewScopeLines   int
}

const (
	defaultCheckPendingDeadline   = 30 * time.Minute
	defaultAuthoringStallDeadline = 2 * time.Hour
	defaultReviewAuthorCycles     = 2
	defaultReviewScopeFiles       = 10
	defaultReviewScopeLines       = 500
	defaultWorkerReconnectGrace   = 2 * time.Minute
)

// ResolveDeadlines parses the configured duration strings, applying the
// coordinator defaults where a value is unset. An empty string takes the
// default; "0" (or any zero duration) disables that deadline.
func (c DeadlineConfig) ResolveDeadlines() (ResolvedDeadlines, error) {
	checkPending, err := resolveDeadline(c.CheckPending, defaultCheckPendingDeadline, "check_pending")
	if err != nil {
		return ResolvedDeadlines{}, err
	}
	authoringStall, err := resolveDeadline(c.AuthoringStall, defaultAuthoringStallDeadline, "authoring_stall")
	if err != nil {
		return ResolvedDeadlines{}, err
	}
	return ResolvedDeadlines{
		CheckPending:   checkPending,
		AuthoringStall: authoringStall,
	}, nil
}

func resolveDeadline(value string, fallback time.Duration, key string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("coordinator deadlines.%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("coordinator deadlines.%s must not be negative", key)
	}
	return d, nil
}

func (c LimitConfig) ResolveLimits() (ResolvedLimits, error) {
	reviewAuthorCycles := c.ReviewAuthorCycles
	if reviewAuthorCycles == 0 {
		reviewAuthorCycles = defaultReviewAuthorCycles
	}
	if reviewAuthorCycles < 0 {
		return ResolvedLimits{}, errors.New("coordinator limits.review_author_cycles must not be negative")
	}
	reviewScopeFiles := c.ReviewScopeFiles
	if reviewScopeFiles == 0 {
		reviewScopeFiles = defaultReviewScopeFiles
	}
	if reviewScopeFiles < 0 {
		return ResolvedLimits{}, errors.New("coordinator limits.review_scope_files must not be negative")
	}
	reviewScopeLines := c.ReviewScopeLines
	if reviewScopeLines == 0 {
		reviewScopeLines = defaultReviewScopeLines
	}
	if reviewScopeLines < 0 {
		return ResolvedLimits{}, errors.New("coordinator limits.review_scope_lines must not be negative")
	}
	return ResolvedLimits{
		ReviewAuthorCycles: reviewAuthorCycles,
		ReviewScopeFiles:   reviewScopeFiles,
		ReviewScopeLines:   reviewScopeLines,
	}, nil
}

// Resolve parses coordinator-side worker lifecycle durations, applying their
// defaults. A zero reconnect grace explicitly disables restart protection.
func (c CoordinatorWorkers) Resolve() (ResolvedCoordinatorWorkers, error) {
	value := strings.TrimSpace(c.ReconnectGrace)
	if value == "" {
		return ResolvedCoordinatorWorkers{ReconnectGrace: defaultWorkerReconnectGrace}, nil
	}
	grace, err := time.ParseDuration(value)
	if err != nil {
		return ResolvedCoordinatorWorkers{}, fmt.Errorf("coordinator workers.reconnect_grace: %w", err)
	}
	if grace < 0 {
		return ResolvedCoordinatorWorkers{}, errors.New("coordinator workers.reconnect_grace must not be negative")
	}
	return ResolvedCoordinatorWorkers{ReconnectGrace: grace}, nil
}

// GlobalDatabasePath is the coordinator-wide database under the data dir; it
// holds the projects registry, workers, tokens, and web sessions. Per-project
// databases live at <data_dir>/projects/<id>/flow.db.
func (c CoordinatorConfig) GlobalDatabasePath() string {
	return filepath.Join(c.DataDir, "global.db")
}

type ClientConfig struct {
	ServerURL string `json:"server_url" yaml:"server_url"`
	Token     string `json:"token" yaml:"token"`
	// TokenFile points at a mode-0600 file holding the bearer token; it is
	// resolved into Token at load time. flow init prefers it over an inline
	// token so the secret lives in exactly one place.
	TokenFile string `json:"token_file,omitempty" yaml:"token_file,omitempty"`
	DataDir   string `json:"data_dir,omitempty" yaml:"data_dir,omitempty"`
}

type WorkerConfig struct {
	WorkerID       string                     `json:"worker_id" yaml:"worker_id"`
	CoordinatorURL string                     `json:"coordinator_url" yaml:"coordinator_url"`
	Token          string                     `json:"token" yaml:"token"`
	WorkDir        string                     `json:"work_dir" yaml:"work_dir"`
	Labels         map[string]string          `json:"labels" yaml:"labels"`
	Taints         []scheduler.Taint          `json:"taints" yaml:"taints"`
	Accepts        []scheduler.CapacityBucket `json:"accepts" yaml:"accepts"`
	Capacity       WorkerCapacity             `json:"capacity" yaml:"capacity"`
	Cleanup        WorkerCleanup              `json:"cleanup" yaml:"cleanup"`
	Tmux           WorkerTmuxConfig           `json:"tmux" yaml:"tmux"`
	Git            WorkerGitConfig            `json:"git" yaml:"git"`
	Metrics        metrics.Config             `json:"metrics" yaml:"metrics"`
	History        WorkerHistoryConfig        `json:"history" yaml:"history"`

	acceptsConfigured  bool `json:"-" yaml:"-"`
	capacityConfigured bool `json:"-" yaml:"-"`
}

type WorkerCapacity struct {
	PersistentAgent int `json:"persistent_agent" yaml:"persistent_agent"`
	Ephemeral       int `json:"ephemeral" yaml:"ephemeral"`
}

// WorkerCleanup configures lifecycle cleanup and disk-pressure admission for a
// worker work directory. Duration and byte-size values are strings so configs
// remain readable ("5m", "10GiB") while validation stays centralized.
type WorkerCleanup struct {
	Interval          string  `json:"interval" yaml:"interval"`
	OrphanGrace       string  `json:"orphan_grace" yaml:"orphan_grace"`
	MinFreeBytes      string  `json:"min_free_bytes" yaml:"min_free_bytes"`
	ResumeFreeBytes   string  `json:"resume_free_bytes" yaml:"resume_free_bytes"`
	MinFreePercent    float64 `json:"min_free_percent" yaml:"min_free_percent"`
	ResumeFreePercent float64 `json:"resume_free_percent" yaml:"resume_free_percent"`
}

// ResolvedWorkerCleanup is the parsed, default-applied worker cleanup policy.
type ResolvedWorkerCleanup struct {
	Interval          time.Duration
	OrphanGrace       time.Duration
	MinFreeBytes      uint64
	ResumeFreeBytes   uint64
	MinFreePercent    float64
	ResumeFreePercent float64
}

const (
	defaultWorkerCleanupInterval    = 5 * time.Minute
	defaultWorkerCleanupOrphanGrace = time.Hour
	defaultWorkerMinFreePercent     = 10
	defaultWorkerResumeFreePercent  = 15
)

// Resolve parses worker cleanup settings. Empty byte thresholds disable the
// absolute-byte check; percentage thresholds are always enabled by default.
func (c WorkerCleanup) Resolve() (ResolvedWorkerCleanup, error) {
	interval, err := resolveWorkerCleanupDuration(c.Interval, defaultWorkerCleanupInterval, "interval", false)
	if err != nil {
		return ResolvedWorkerCleanup{}, err
	}
	orphanGrace, err := resolveWorkerCleanupDuration(c.OrphanGrace, defaultWorkerCleanupOrphanGrace, "orphan_grace", true)
	if err != nil {
		return ResolvedWorkerCleanup{}, err
	}
	minFreeBytes, err := parseWorkerByteSize(c.MinFreeBytes, "min_free_bytes")
	if err != nil {
		return ResolvedWorkerCleanup{}, err
	}
	resumeFreeBytes, err := parseWorkerByteSize(c.ResumeFreeBytes, "resume_free_bytes")
	if err != nil {
		return ResolvedWorkerCleanup{}, err
	}
	if resumeFreeBytes < minFreeBytes {
		return ResolvedWorkerCleanup{}, errors.New("worker cleanup.resume_free_bytes must be >= min_free_bytes")
	}

	minFreePercent := c.MinFreePercent
	if minFreePercent == 0 {
		minFreePercent = defaultWorkerMinFreePercent
	}
	resumeFreePercent := c.ResumeFreePercent
	if resumeFreePercent == 0 {
		resumeFreePercent = defaultWorkerResumeFreePercent
	}
	if math.IsNaN(minFreePercent) || math.IsInf(minFreePercent, 0) || minFreePercent < 0 || minFreePercent > 100 {
		return ResolvedWorkerCleanup{}, errors.New("worker cleanup.min_free_percent must be between 0 and 100")
	}
	if math.IsNaN(resumeFreePercent) || math.IsInf(resumeFreePercent, 0) || resumeFreePercent < minFreePercent || resumeFreePercent > 100 {
		return ResolvedWorkerCleanup{}, errors.New("worker cleanup.resume_free_percent must be between min_free_percent and 100")
	}

	return ResolvedWorkerCleanup{
		Interval:          interval,
		OrphanGrace:       orphanGrace,
		MinFreeBytes:      minFreeBytes,
		ResumeFreeBytes:   resumeFreeBytes,
		MinFreePercent:    minFreePercent,
		ResumeFreePercent: resumeFreePercent,
	}, nil
}

func resolveWorkerCleanupDuration(value string, fallback time.Duration, key string, allowZero bool) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("worker cleanup.%s: %w", key, err)
	}
	if duration < 0 || (!allowZero && duration == 0) {
		if allowZero {
			return 0, fmt.Errorf("worker cleanup.%s must not be negative", key)
		}
		return 0, fmt.Errorf("worker cleanup.%s must be greater than zero", key)
	}
	return duration, nil
}

func parseWorkerByteSize(value string, key string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	lower := strings.ToLower(value)
	multiplier := uint64(1)
	number := lower
	for _, unit := range []struct {
		suffix     string
		multiplier uint64
	}{
		{"tib", 1 << 40},
		{"gib", 1 << 30},
		{"mib", 1 << 20},
		{"kib", 1 << 10},
		{"tb", 1_000_000_000_000},
		{"gb", 1_000_000_000},
		{"mb", 1_000_000},
		{"kb", 1_000},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, unit.suffix) {
			number = strings.TrimSpace(strings.TrimSuffix(lower, unit.suffix))
			multiplier = unit.multiplier
			break
		}
	}
	amount, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("worker cleanup.%s must be a byte size such as 10GiB: %w", key, err)
	}
	if amount > ^uint64(0)/multiplier {
		return 0, fmt.Errorf("worker cleanup.%s is too large", key)
	}
	return amount * multiplier, nil
}

type WorkerGitConfig struct {
	Principal   string `json:"principal" yaml:"principal"`
	CommitName  string `json:"commit_name" yaml:"commit_name"`
	CommitEmail string `json:"commit_email" yaml:"commit_email"`
}

type WorkerTmuxConfig struct {
	SocketPath string `json:"socket_path" yaml:"socket_path"`
}

func DefaultDataDir() (string, error) {
	if flowDataDir := strings.TrimSpace(os.Getenv("FLOW_DATA_DIR")); flowDataDir != "" {
		return cleanRequiredPath(flowDataDir), nil
	}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		return filepath.Join(expandHome(dataHome), "flow"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".local", "share", "flow"), nil
}

func DefaultCoordinator() (CoordinatorConfig, error) {
	dataDir, err := DefaultDataDir()
	if err != nil {
		return CoordinatorConfig{}, err
	}

	return CoordinatorConfig{
		DataDir:    dataDir,
		ListenAddr: "127.0.0.1:8421",
	}, nil
}

func CoordinatorURLForListenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}

	return "http://" + addr
}

func LoadCoordinator(path string) (CoordinatorConfig, error) {
	defaults, err := DefaultCoordinator()
	if err != nil {
		return CoordinatorConfig{}, err
	}
	if strings.TrimSpace(path) == "" {
		return normalizeCoordinator(defaults)
	}

	var fileCfg CoordinatorConfig
	if err := loadConfig(path, &fileCfg); err != nil {
		return CoordinatorConfig{}, err
	}

	cfg := defaults
	if fileCfg.DataDir != "" {
		cfg.DataDir = fileCfg.DataDir
	}
	if fileCfg.ListenAddr != "" {
		cfg.ListenAddr = fileCfg.ListenAddr
	}
	if fileCfg.AuthorEntrypoint != nil {
		cfg.AuthorEntrypoint = copyAnyMap(fileCfg.AuthorEntrypoint)
		cfg.AuthorEntrypointConfigured = true
	}
	cfg.Deadlines = fileCfg.Deadlines
	cfg.Limits = fileCfg.Limits
	cfg.Workers = fileCfg.Workers
	cfg.HarnessArgs = fileCfg.HarnessArgs
	cfg.DefaultAgent = fileCfg.DefaultAgent
	cfg.Metrics = fileCfg.Metrics
	cfg.History = fileCfg.History
	if strings.TrimSpace(fileCfg.Git.CommitName) != "" {
		cfg.Git.CommitName = strings.TrimSpace(fileCfg.Git.CommitName)
	}
	if strings.TrimSpace(fileCfg.Git.CommitEmail) != "" {
		cfg.Git.CommitEmail = strings.TrimSpace(fileCfg.Git.CommitEmail)
	}

	return normalizeCoordinator(cfg)
}

func DefaultAuthorEntrypoint() map[string]any {
	return DefaultAuthorEntrypointForAgent(flowharness.DefaultAgentSelection())
}

// ResolvedDefaultAgent normalizes and validates the configured default agent
// selection, falling back to the built-in default harness when unset.
func (c CoordinatorConfig) ResolvedDefaultAgent() (flowharness.AgentSelection, error) {
	return flowharness.ResolveAgentSelection(flowharness.AgentSelection{
		Harness:         c.DefaultAgent.Harness,
		Model:           c.DefaultAgent.Model,
		ReasoningEffort: c.DefaultAgent.ReasoningEffort,
	})
}

// DefaultAuthorEntrypointForAgent builds the default author entrypoint for a
// resolved agent selection: the selection's harness with its model/effort
// tokens applied. The selection must already be resolved (see
// ResolvedDefaultAgent); invalid values panic, mirroring
// DefaultAuthorEntrypoint.
func DefaultAuthorEntrypointForAgent(sel flowharness.AgentSelection) map[string]any {
	modelTokens, err := sel.ModelArgs()
	if err != nil {
		panic(err)
	}
	entrypoint, err := flowharness.DefaultAuthorEntrypointWithArgs(sel.Harness, modelTokens)
	if err != nil {
		panic(err)
	}
	return entrypoint
}

func DefaultClient() ClientConfig {
	return ClientConfig{
		ServerURL: "http://127.0.0.1:8421",
	}
}

// LoadClient loads the client config from path, or, when path is empty, from
// the default location under $XDG_CONFIG_HOME/flow (missing default config
// just yields the built-in defaults).
func LoadClient(path string) (ClientConfig, error) {
	cfg := DefaultClient()
	if strings.TrimSpace(path) == "" {
		defaultPath, err := DefaultClientConfigPath()
		if err != nil {
			return ClientConfig{}, err
		}
		if _, err := os.Stat(defaultPath); errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		} else if err != nil {
			return ClientConfig{}, fmt.Errorf("stat client config: %w", err)
		}
		path = defaultPath
	}

	var fileCfg ClientConfig
	if err := loadConfig(path, &fileCfg); err != nil {
		return ClientConfig{}, err
	}
	if fileCfg.ServerURL != "" {
		cfg.ServerURL = fileCfg.ServerURL
	}
	if fileCfg.Token != "" {
		cfg.Token = fileCfg.Token
	}
	if fileCfg.TokenFile != "" {
		cfg.TokenFile = expandHome(fileCfg.TokenFile)
	}
	if fileCfg.DataDir != "" {
		cfg.DataDir = cleanRequiredPath(fileCfg.DataDir)
	}
	if cfg.Token == "" && cfg.TokenFile != "" {
		token, err := ReadTokenFile(cfg.TokenFile)
		if err != nil {
			return ClientConfig{}, err
		}
		cfg.Token = token
	}

	return cfg, nil
}

// ResolveClientDataDir returns the data directory recorded in the client
// config, falling back to the local default when no config has been written yet.
func ResolveClientDataDir(cfg ClientConfig) (string, error) {
	if strings.TrimSpace(cfg.DataDir) != "" {
		return cleanRequiredPath(cfg.DataDir), nil
	}

	return DefaultDataDir()
}

// OwnerTokenPath is the conventional same-machine owner credential file under
// a Flow data directory.
func OwnerTokenPath(dataDir string) string {
	return filepath.Join(cleanRequiredPath(dataDir), "owner.token")
}

// ResolveOwnerTokenFallback reads the conventional owner.token file when the
// loaded client config did not already provide a token. It tries the client
// config's data_dir first, then the built-in default data dir.
func ResolveOwnerTokenFallback(cfg ClientConfig) (string, string, bool, error) {
	if strings.TrimSpace(cfg.Token) != "" {
		return strings.TrimSpace(cfg.Token), "", true, nil
	}

	var dirs []string
	if strings.TrimSpace(cfg.DataDir) != "" {
		dirs = append(dirs, cleanRequiredPath(cfg.DataDir))
	}
	defaultDir, err := DefaultDataDir()
	if err != nil {
		return "", "", false, err
	}
	if !containsPath(dirs, defaultDir) {
		dirs = append(dirs, defaultDir)
	}

	for _, dir := range dirs {
		path := OwnerTokenPath(dir)
		token, err := ReadTokenFile(path)
		if err == nil {
			return token, path, true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", "", false, err
	}

	return "", "", false, nil
}

// LocalClientConfig returns the client config a same-machine coordinator should
// publish for local CLIs. When the owner token matches data_dir/owner.token, the
// config references that file instead of duplicating the secret.
func LocalClientConfig(dataDir string, serverURL string, ownerToken string) (ClientConfig, error) {
	if strings.TrimSpace(dataDir) == "" {
		resolved, err := DefaultDataDir()
		if err != nil {
			return ClientConfig{}, err
		}
		dataDir = resolved
	}
	dataDir = cleanRequiredPath(dataDir)

	cfg := DefaultClient()
	cfg.DataDir = dataDir
	if strings.TrimSpace(serverURL) != "" {
		cfg.ServerURL = strings.TrimSpace(serverURL)
	}
	ownerToken = strings.TrimSpace(ownerToken)
	if ownerToken != "" {
		ownerTokenPath := OwnerTokenPath(dataDir)
		if token, err := ReadTokenFile(ownerTokenPath); err == nil && token == ownerToken {
			cfg.TokenFile = ownerTokenPath
		} else {
			cfg.Token = ownerToken
		}
	}

	return cfg, nil
}

// DefaultConfigDir is $XDG_CONFIG_HOME/flow, falling back to ~/.config/flow.
func DefaultConfigDir() (string, error) {
	if configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configHome != "" {
		return filepath.Join(configHome, "flow"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".config", "flow"), nil
}

func DefaultClientConfigPath() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "config.yaml"), nil
}

// WriteClientConfig writes the client config privately (it may carry or
// reference a bearer token).
func WriteClientConfig(path string, cfg ClientConfig) error {
	contents, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode client config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		return fmt.Errorf("write client config: %w", err)
	}

	return nil
}

// ReadTokenFile reads a bearer token from a private file, rejecting
// group/other-accessible modes.
func ReadTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat token file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s must not be readable by group or others", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}

	return token, nil
}

func DefaultWorker() (WorkerConfig, error) {
	dataDir, err := DefaultDataDir()
	if err != nil {
		return WorkerConfig{}, err
	}

	return WorkerConfig{
		CoordinatorURL: "http://127.0.0.1:8421",
		WorkDir:        filepath.Join(dataDir, "workers"),
		Labels:         map[string]string{},
	}, nil
}

// DefaultWorkerConfigPath is the conventional worker config path resolved from
// local client discovery. The config is global: one worker serves every project,
// cloning each job's exchange from the job payload.
func DefaultWorkerConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "worker.yaml")
}

// ResolveWorkerConfigPath returns the worker config path implied by local
// client discovery when no explicit -c/--config path was supplied.
func ResolveWorkerConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return cleanRequiredPath(path), nil
	}

	clientCfg, err := LoadClient("")
	if err != nil {
		return "", err
	}
	dataDir, err := ResolveClientDataDir(clientCfg)
	if err != nil {
		return "", err
	}

	return DefaultWorkerConfigPath(dataDir), nil
}

func LoadWorker(path string) (WorkerConfig, error) {
	cfg, err := DefaultWorker()
	if err != nil {
		return WorkerConfig{}, err
	}
	if strings.TrimSpace(path) == "" {
		return normalizeWorker(cfg)
	}

	acceptsConfigured, capacityConfigured, err := workerAcceptanceFieldPresence(path)
	if err != nil {
		return WorkerConfig{}, err
	}
	if acceptsConfigured && capacityConfigured {
		return WorkerConfig{}, errors.New("worker accepts and capacity cannot both be configured")
	}

	var fileCfg WorkerConfig
	if err := loadConfig(path, &fileCfg); err != nil {
		return WorkerConfig{}, err
	}
	cfg.acceptsConfigured = acceptsConfigured
	cfg.capacityConfigured = capacityConfigured
	if fileCfg.WorkerID != "" {
		cfg.WorkerID = fileCfg.WorkerID
	}
	if fileCfg.CoordinatorURL != "" {
		cfg.CoordinatorURL = fileCfg.CoordinatorURL
	}
	if fileCfg.Token != "" {
		cfg.Token = fileCfg.Token
	}
	if fileCfg.WorkDir != "" {
		cfg.WorkDir = fileCfg.WorkDir
	}
	if fileCfg.Labels != nil {
		cfg.Labels = fileCfg.Labels
	}
	if fileCfg.Taints != nil {
		cfg.Taints = fileCfg.Taints
	}
	if acceptsConfigured {
		cfg.Accepts = append([]scheduler.CapacityBucket(nil), fileCfg.Accepts...)
	}
	if capacityConfigured {
		cfg.Capacity = fileCfg.Capacity
	}
	cfg.Cleanup = fileCfg.Cleanup
	if fileCfg.Git.Principal != "" {
		cfg.Git.Principal = fileCfg.Git.Principal
	}
	if strings.TrimSpace(fileCfg.Git.CommitName) != "" {
		cfg.Git.CommitName = strings.TrimSpace(fileCfg.Git.CommitName)
	}
	if strings.TrimSpace(fileCfg.Git.CommitEmail) != "" {
		cfg.Git.CommitEmail = strings.TrimSpace(fileCfg.Git.CommitEmail)
	}
	if fileCfg.Tmux.SocketPath != "" {
		cfg.Tmux.SocketPath = fileCfg.Tmux.SocketPath
	}
	cfg.Metrics = fileCfg.Metrics
	cfg.History = fileCfg.History

	return normalizeWorker(cfg)
}

// ApplyWorkerEnvOverrides applies deployment-specific worker overrides after a
// file config has been loaded. It is intentionally typed rather than a generic
// environment-to-YAML templating layer so invalid values fail at startup.
func ApplyWorkerEnvOverrides(cfg WorkerConfig, getenv func(string) string) (WorkerConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_ID")); value != "" {
		cfg.WorkerID = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_COORDINATOR_URL")); value != "" {
		cfg.CoordinatorURL = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TOKEN")); value != "" {
		cfg.Token = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_WORK_DIR")); value != "" {
		cfg.WorkDir = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_ACCEPTS")); value != "" {
		if cfg.capacityConfigured {
			return WorkerConfig{}, errors.New("FLOW_WORKER_ACCEPTS conflicts with worker capacity")
		}
		cfg.acceptsConfigured = true
		cfg.Accepts = cfg.Accepts[:0]
		for _, bucket := range strings.Split(value, ",") {
			cfg.Accepts = append(cfg.Accepts, scheduler.CapacityBucket(strings.TrimSpace(bucket)))
		}
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_CAPACITY_PERSISTENT_AGENT")); value != "" {
		capacity, err := strconv.Atoi(value)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("FLOW_WORKER_CAPACITY_PERSISTENT_AGENT must be an integer: %w", err)
		}
		if cfg.acceptsConfigured {
			return WorkerConfig{}, errors.New("FLOW_WORKER_CAPACITY_PERSISTENT_AGENT conflicts with worker accepts")
		}
		cfg.capacityConfigured = true
		cfg.Capacity.PersistentAgent = capacity
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_CAPACITY_EPHEMERAL")); value != "" {
		capacity, err := strconv.Atoi(value)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("FLOW_WORKER_CAPACITY_EPHEMERAL must be an integer: %w", err)
		}
		if cfg.acceptsConfigured {
			return WorkerConfig{}, errors.New("FLOW_WORKER_CAPACITY_EPHEMERAL conflicts with worker accepts")
		}
		cfg.capacityConfigured = true
		cfg.Capacity.Ephemeral = capacity
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TMUX_SOCKET_PATH")); value != "" {
		cfg.Tmux.SocketPath = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_GIT_PRINCIPAL")); value != "" {
		cfg.Git.Principal = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_GIT_COMMIT_NAME")); value != "" {
		cfg.Git.CommitName = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_GIT_COMMIT_EMAIL")); value != "" {
		cfg.Git.CommitEmail = value
	}

	return normalizeWorker(cfg)
}

// ApplyCoordinatorEnvOverrides applies deployment-specific coordinator
// overrides after a file config has been loaded. It mirrors
// ApplyWorkerEnvOverrides: a typed layer rather than generic env-to-YAML
// templating so invalid values fail at startup.
func ApplyCoordinatorEnvOverrides(cfg CoordinatorConfig, getenv func(string) string) (CoordinatorConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if value := strings.TrimSpace(getenv("FLOW_GIT_COMMIT_NAME")); value != "" {
		cfg.Git.CommitName = value
	}
	if value := strings.TrimSpace(getenv("FLOW_GIT_COMMIT_EMAIL")); value != "" {
		cfg.Git.CommitEmail = value
	}

	return normalizeCoordinator(cfg)
}

// validateCommitIdentity enforces that a git commit identity is either fully
// set or fully empty: git refuses to commit with a name but no email (or vice
// versa), so a half-set identity is a startup error.
func validateCommitIdentity(name string, email string, scope string) error {
	hasName := strings.TrimSpace(name) != ""
	hasEmail := strings.TrimSpace(email) != ""
	if hasName != hasEmail {
		return fmt.Errorf("%s git.commit_name and git.commit_email must both be set or both be empty", scope)
	}
	return nil
}

// ValidateCoordinatorCommitIdentity validates the coordinator's git commit
// identity (both fields set or both empty). Callers that mutate the identity
// after loading (e.g. CLI flags) re-run this before serving.
func ValidateCoordinatorCommitIdentity(git CoordinatorGitConfig) error {
	return validateCommitIdentity(git.CommitName, git.CommitEmail, "coordinator")
}

// ValidateWorkerCommitIdentity validates the worker's git commit identity
// (both fields set or both empty). Callers that mutate the identity after
// loading (e.g. CLI flags) re-run this before serving.
func ValidateWorkerCommitIdentity(git WorkerGitConfig) error {
	return validateCommitIdentity(git.CommitName, git.CommitEmail, "worker")
}

func normalizeCoordinator(cfg CoordinatorConfig) (CoordinatorConfig, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return CoordinatorConfig{}, errors.New("coordinator data_dir is required")
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return CoordinatorConfig{}, errors.New("coordinator listen_addr is required")
	}
	defaultAgent, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		return CoordinatorConfig{}, fmt.Errorf("coordinator default_agent: %w", err)
	}
	cfg.DefaultAgent = DefaultAgentConfig{
		Harness:         defaultAgent.Harness,
		Model:           defaultAgent.Model,
		ReasoningEffort: defaultAgent.ReasoningEffort,
	}
	if cfg.AuthorEntrypoint == nil {
		cfg.AuthorEntrypoint = DefaultAuthorEntrypointForAgent(defaultAgent)
	}
	if err := validateAuthorEntrypoint(cfg.AuthorEntrypoint); err != nil {
		return CoordinatorConfig{}, err
	}
	if _, err := cfg.Deadlines.ResolveDeadlines(); err != nil {
		return CoordinatorConfig{}, err
	}
	if _, err := cfg.Workers.Resolve(); err != nil {
		return CoordinatorConfig{}, err
	}
	harnessArgs, err := flowharness.NormalizeArgs(cfg.HarnessArgs)
	if err != nil {
		return CoordinatorConfig{}, fmt.Errorf("coordinator harness_args: %w", err)
	}
	if err := validateCommitIdentity(cfg.Git.CommitName, cfg.Git.CommitEmail, "coordinator"); err != nil {
		return CoordinatorConfig{}, err
	}
	if _, err := cfg.History.Resolve(cfg.DataDir); err != nil {
		return CoordinatorConfig{}, err
	}

	cfg.DataDir = cleanRequiredPath(cfg.DataDir)
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	cfg.HarnessArgs = harnessArgs
	return cfg, nil
}

func normalizeWorker(cfg WorkerConfig) (WorkerConfig, error) {
	if !cfg.acceptsConfigured && !cfg.capacityConfigured {
		cfg.acceptsConfigured = cfg.Accepts != nil
		cfg.capacityConfigured = cfg.Capacity.PersistentAgent != 0 || cfg.Capacity.Ephemeral != 0
	}
	if cfg.acceptsConfigured && cfg.capacityConfigured {
		return WorkerConfig{}, errors.New("worker accepts and capacity cannot both be configured")
	}
	if strings.TrimSpace(cfg.CoordinatorURL) == "" {
		return WorkerConfig{}, errors.New("worker coordinator_url is required")
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return WorkerConfig{}, errors.New("worker work_dir is required")
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	normalizedLabels, err := scheduler.NormalizeLabels(cfg.Labels)
	if err != nil {
		return WorkerConfig{}, err
	}
	platformOS := runtime.GOOS
	if platformOS == "darwin" {
		platformOS = "macos"
	}
	for key, expected := range map[string]string{"os": platformOS, "arch": runtime.GOARCH} {
		if actual, ok := normalizedLabels[key]; ok && actual != expected {
			return WorkerConfig{}, fmt.Errorf("worker label %s=%s conflicts with detected platform %s", key, actual, expected)
		}
		normalizedLabels[key] = expected
	}
	cfg.Labels = map[string]string(normalizedLabels)
	if cfg.Taints == nil {
		cfg.Taints = []scheduler.Taint{}
	}
	for _, taint := range cfg.Taints {
		if _, err := scheduler.NormalizeTaint(taint); err != nil {
			return WorkerConfig{}, err
		}
	}
	if cfg.Capacity.PersistentAgent < 0 || cfg.Capacity.Ephemeral < 0 {
		return WorkerConfig{}, errors.New("worker capacity cannot be negative")
	}
	if err := normalizeWorkerAcceptance(&cfg); err != nil {
		return WorkerConfig{}, err
	}
	if _, err := cfg.Cleanup.Resolve(); err != nil {
		return WorkerConfig{}, err
	}
	if err := validateCommitIdentity(cfg.Git.CommitName, cfg.Git.CommitEmail, "worker"); err != nil {
		return WorkerConfig{}, err
	}
	if _, err := cfg.History.Resolve(cfg.WorkDir); err != nil {
		return WorkerConfig{}, err
	}

	cfg.WorkDir = cleanRequiredPath(cfg.WorkDir)
	cfg.Tmux.SocketPath = cleanOptionalPath(cfg.Tmux.SocketPath)
	return cfg, nil
}

func normalizeWorkerAcceptance(cfg *WorkerConfig) error {
	if cfg.acceptsConfigured {
		cfg.Capacity = WorkerCapacity{}
		seen := make(map[scheduler.CapacityBucket]struct{}, len(cfg.Accepts))
		for i, raw := range cfg.Accepts {
			bucket, err := scheduler.ParseCapacityBucket(string(raw))
			if err != nil {
				return fmt.Errorf("worker accepts: %w", err)
			}
			if _, ok := seen[bucket]; ok {
				return fmt.Errorf("worker accepts contains duplicate bucket %s", bucket)
			}
			seen[bucket] = struct{}{}
			cfg.Accepts[i] = bucket
			switch bucket {
			case scheduler.CapacityPersistentAgent:
				cfg.Capacity.PersistentAgent = 1
			case scheduler.CapacityEphemeral:
				cfg.Capacity.Ephemeral = 1
			}
		}
		return nil
	}

	legacy := cfg.Capacity
	if legacy.PersistentAgent > 1 || legacy.Ephemeral > 1 {
		slog.Warn("worker capacity magnitudes are ignored; use accepts instead",
			"capacity_persistent_agent", legacy.PersistentAgent,
			"capacity_ephemeral", legacy.Ephemeral,
		)
	}
	cfg.Accepts = cfg.Accepts[:0]
	cfg.Capacity = WorkerCapacity{}
	if legacy.PersistentAgent > 0 {
		cfg.Accepts = append(cfg.Accepts, scheduler.CapacityPersistentAgent)
		cfg.Capacity.PersistentAgent = 1
	}
	if legacy.Ephemeral > 0 {
		cfg.Accepts = append(cfg.Accepts, scheduler.CapacityEphemeral)
		cfg.Capacity.Ephemeral = 1
	}
	return nil
}

func workerAcceptanceFieldPresence(path string) (accepts bool, capacity bool, err error) {
	data, err := os.ReadFile(cleanRequiredPath(path))
	if err != nil {
		return false, false, fmt.Errorf("read config %q: %w", path, err)
	}
	if strings.EqualFold(filepath.Ext(path), ".yaml") || strings.EqualFold(filepath.Ext(path), ".yml") {
		var document yaml.Node
		if err := yaml.Unmarshal(data, &document); err != nil {
			return false, false, fmt.Errorf("decode config %q: %w", path, err)
		}
		if len(document.Content) == 0 || len(document.Content[0].Content)%2 != 0 {
			return false, false, nil
		}
		fields := document.Content[0].Content
		for i := 0; i < len(fields); i += 2 {
			switch fields[i].Value {
			case "accepts":
				accepts = true
			case "capacity":
				capacity = true
			}
		}
		return accepts, capacity, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, false, fmt.Errorf("decode config %q: %w", path, err)
	}
	_, accepts = fields["accepts"]
	_, capacity = fields["capacity"]
	return accepts, capacity, nil
}

func validateAuthorEntrypoint(entrypoint map[string]any) error {
	argvValue, ok := entrypoint["argv"]
	if !ok {
		return errors.New("coordinator author_entrypoint argv is required")
	}
	argv, err := stringList(argvValue)
	if err != nil {
		return fmt.Errorf("coordinator author_entrypoint argv: %w", err)
	}
	if len(argv) == 0 {
		return errors.New("coordinator author_entrypoint argv is required")
	}
	for _, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			return errors.New("coordinator author_entrypoint argv entries must not be empty")
		}
	}
	if shell, ok := entrypoint["shell"].(bool); ok && shell && len(argv) != 1 {
		return errors.New("coordinator author_entrypoint shell commands require exactly one argv entry")
	}

	return nil
}

func stringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case []string:
		return typed, nil
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("entries must be strings")
			}
			items = append(items, text)
		}
		return items, nil
	default:
		return nil, errors.New("must be a list of strings")
	}
}

func loadConfig(path string, target any) error {
	file, err := os.Open(cleanRequiredPath(path))
	if err != nil {
		return fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		decoder := yaml.NewDecoder(file)
		decoder.KnownFields(true)
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("decode config %q: %w", path, err)
		}
	default:
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("decode config %q: %w", path, err)
		}
	}

	return nil
}

func copyAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func containsPath(paths []string, path string) bool {
	cleaned := cleanRequiredPath(path)
	for _, existing := range paths {
		if cleanRequiredPath(existing) == cleaned {
			return true
		}
	}
	return false
}

func cleanRequiredPath(path string) string {
	return filepath.Clean(expandHome(path))
}

func cleanOptionalPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}

	return cleanRequiredPath(path)
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}

	return path
}
