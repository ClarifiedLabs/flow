package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"gopkg.in/yaml.v3"
)

const DefaultProtocolVersion = "1"

type CoordinatorConfig struct {
	DataDir                    string           `json:"data_dir" yaml:"data_dir"`
	ListenAddr                 string           `json:"listen_addr" yaml:"listen_addr"`
	ExchangeBaseURL            string           `json:"exchange_base_url" yaml:"exchange_base_url"`
	ProtocolVersion            string           `json:"protocol_version" yaml:"protocol_version"`
	AuthorEntrypoint           map[string]any   `json:"author_entrypoint" yaml:"author_entrypoint"`
	AuthorEntrypointConfigured bool             `json:"-" yaml:"-"`
	Deadlines                  DeadlineConfig   `json:"deadlines" yaml:"deadlines"`
	Limits                     LimitConfig      `json:"limits" yaml:"limits"`
	HarnessArgs                flowharness.Args `json:"harness_args" yaml:"harness_args"`
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
	// ReviewAuthorCycles limits how many times an issue may be automatically
	// sent from review/acceptance back to authoring before a human grants more.
	// The default is 5.
	ReviewAuthorCycles int `json:"review_author_cycles" yaml:"review_author_cycles"`
}

type ResolvedLimits struct {
	ReviewAuthorCycles int
}

const (
	defaultCheckPendingDeadline   = 30 * time.Minute
	defaultAuthoringStallDeadline = 2 * time.Hour
	defaultReviewAuthorCycles     = 5
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
	return ResolvedLimits{ReviewAuthorCycles: reviewAuthorCycles}, nil
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
	TokenFile       string `json:"token_file,omitempty" yaml:"token_file,omitempty"`
	DataDir         string `json:"data_dir,omitempty" yaml:"data_dir,omitempty"`
	ProtocolVersion string `json:"protocol_version" yaml:"protocol_version"`
}

type WorkerConfig struct {
	WorkerID        string               `json:"worker_id" yaml:"worker_id"`
	CoordinatorURL  string               `json:"coordinator_url" yaml:"coordinator_url"`
	Token           string               `json:"token" yaml:"token"`
	WorkDir         string               `json:"work_dir" yaml:"work_dir"`
	ProtocolVersion string               `json:"protocol_version" yaml:"protocol_version"`
	Labels          map[string]string    `json:"labels" yaml:"labels"`
	Taints          []scheduler.Taint    `json:"taints" yaml:"taints"`
	Capacity        WorkerCapacity       `json:"capacity" yaml:"capacity"`
	Terminal        WorkerTerminalConfig `json:"terminal" yaml:"terminal"`
	Tmux            WorkerTmuxConfig     `json:"tmux" yaml:"tmux"`
	Git             WorkerGitConfig      `json:"git" yaml:"git"`
}

type WorkerCapacity struct {
	PersistentAgent int `json:"persistent_agent" yaml:"persistent_agent"`
	Ephemeral       int `json:"ephemeral" yaml:"ephemeral"`
}

type WorkerGitConfig struct {
	ExchangeURL string `json:"exchange_url" yaml:"exchange_url"`
	Principal   string `json:"principal" yaml:"principal"`
}

type WorkerTerminalConfig struct {
	BindAddress   string `json:"bind_address" yaml:"bind_address"`
	PublicBaseURL string `json:"public_base_url" yaml:"public_base_url"`
	TTYDPath      string `json:"ttyd_path" yaml:"ttyd_path"`
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
		DataDir:          dataDir,
		ListenAddr:       "127.0.0.1:8421",
		ProtocolVersion:  DefaultProtocolVersion,
		AuthorEntrypoint: DefaultAuthorEntrypoint(),
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
	if fileCfg.ExchangeBaseURL != "" {
		cfg.ExchangeBaseURL = fileCfg.ExchangeBaseURL
	}
	if fileCfg.ProtocolVersion != "" {
		cfg.ProtocolVersion = fileCfg.ProtocolVersion
	}
	if fileCfg.AuthorEntrypoint != nil {
		cfg.AuthorEntrypoint = copyAnyMap(fileCfg.AuthorEntrypoint)
		cfg.AuthorEntrypointConfigured = true
	}
	cfg.Deadlines = fileCfg.Deadlines
	cfg.Limits = fileCfg.Limits
	cfg.HarnessArgs = fileCfg.HarnessArgs

	return normalizeCoordinator(cfg)
}

func DefaultAuthorEntrypoint() map[string]any {
	entrypoint, err := flowharness.DefaultAuthorEntrypoint(flowharness.DefaultAgentName())
	if err != nil {
		panic(err)
	}
	return entrypoint
}

func DefaultClient() ClientConfig {
	return ClientConfig{
		ServerURL:       "http://127.0.0.1:8421",
		ProtocolVersion: DefaultProtocolVersion,
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
	if fileCfg.ProtocolVersion != "" {
		cfg.ProtocolVersion = fileCfg.ProtocolVersion
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
func LocalClientConfig(dataDir string, serverURL string, ownerToken string, protocolVersion string) (ClientConfig, error) {
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
	if strings.TrimSpace(protocolVersion) != "" {
		cfg.ProtocolVersion = strings.TrimSpace(protocolVersion)
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
		CoordinatorURL:  "http://127.0.0.1:8421",
		WorkDir:         filepath.Join(dataDir, "workers"),
		ProtocolVersion: DefaultProtocolVersion,
		Labels:          map[string]string{},
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

	var fileCfg WorkerConfig
	if err := loadConfig(path, &fileCfg); err != nil {
		return WorkerConfig{}, err
	}
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
	if fileCfg.ProtocolVersion != "" {
		cfg.ProtocolVersion = fileCfg.ProtocolVersion
	}
	if fileCfg.Labels != nil {
		cfg.Labels = fileCfg.Labels
	}
	if fileCfg.Taints != nil {
		cfg.Taints = fileCfg.Taints
	}
	if fileCfg.Capacity.PersistentAgent != 0 {
		cfg.Capacity.PersistentAgent = fileCfg.Capacity.PersistentAgent
	}
	if fileCfg.Capacity.Ephemeral != 0 {
		cfg.Capacity.Ephemeral = fileCfg.Capacity.Ephemeral
	}
	if fileCfg.Git.ExchangeURL != "" {
		cfg.Git.ExchangeURL = fileCfg.Git.ExchangeURL
	}
	if fileCfg.Git.Principal != "" {
		cfg.Git.Principal = fileCfg.Git.Principal
	}
	if fileCfg.Terminal.BindAddress != "" {
		cfg.Terminal.BindAddress = fileCfg.Terminal.BindAddress
	}
	if fileCfg.Terminal.PublicBaseURL != "" {
		cfg.Terminal.PublicBaseURL = fileCfg.Terminal.PublicBaseURL
	}
	if fileCfg.Terminal.TTYDPath != "" {
		cfg.Terminal.TTYDPath = fileCfg.Terminal.TTYDPath
	}
	if fileCfg.Tmux.SocketPath != "" {
		cfg.Tmux.SocketPath = fileCfg.Tmux.SocketPath
	}

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
	if value := strings.TrimSpace(getenv("FLOW_WORKER_PROTOCOL_VERSION")); value != "" {
		cfg.ProtocolVersion = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_CAPACITY_PERSISTENT_AGENT")); value != "" {
		capacity, err := strconv.Atoi(value)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("FLOW_WORKER_CAPACITY_PERSISTENT_AGENT must be an integer: %w", err)
		}
		cfg.Capacity.PersistentAgent = capacity
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_CAPACITY_EPHEMERAL")); value != "" {
		capacity, err := strconv.Atoi(value)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("FLOW_WORKER_CAPACITY_EPHEMERAL must be an integer: %w", err)
		}
		cfg.Capacity.Ephemeral = capacity
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TERMINAL_BIND_ADDRESS")); value != "" {
		cfg.Terminal.BindAddress = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL")); value != "" {
		cfg.Terminal.PublicBaseURL = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TERMINAL_TTYD_PATH")); value != "" {
		cfg.Terminal.TTYDPath = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_TMUX_SOCKET_PATH")); value != "" {
		cfg.Tmux.SocketPath = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_GIT_EXCHANGE_URL")); value != "" {
		cfg.Git.ExchangeURL = value
	}
	if value := strings.TrimSpace(getenv("FLOW_WORKER_GIT_PRINCIPAL")); value != "" {
		cfg.Git.Principal = value
	}

	return normalizeWorker(cfg)
}

func normalizeCoordinator(cfg CoordinatorConfig) (CoordinatorConfig, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return CoordinatorConfig{}, errors.New("coordinator data_dir is required")
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return CoordinatorConfig{}, errors.New("coordinator listen_addr is required")
	}
	if strings.TrimSpace(cfg.ProtocolVersion) == "" {
		return CoordinatorConfig{}, errors.New("coordinator protocol_version is required")
	}
	if strings.TrimSpace(cfg.ExchangeBaseURL) == "" {
		cfg.ExchangeBaseURL = CoordinatorURLForListenAddr(cfg.ListenAddr)
	}
	if cfg.AuthorEntrypoint == nil {
		cfg.AuthorEntrypoint = DefaultAuthorEntrypoint()
	}
	if err := validateAuthorEntrypoint(cfg.AuthorEntrypoint); err != nil {
		return CoordinatorConfig{}, err
	}
	if _, err := cfg.Deadlines.ResolveDeadlines(); err != nil {
		return CoordinatorConfig{}, err
	}
	harnessArgs, err := flowharness.NormalizeArgs(cfg.HarnessArgs)
	if err != nil {
		return CoordinatorConfig{}, fmt.Errorf("coordinator harness_args: %w", err)
	}

	cfg.DataDir = cleanRequiredPath(cfg.DataDir)
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	cfg.ExchangeBaseURL = strings.TrimRight(strings.TrimSpace(cfg.ExchangeBaseURL), "/")
	cfg.HarnessArgs = harnessArgs
	return cfg, nil
}

func normalizeWorker(cfg WorkerConfig) (WorkerConfig, error) {
	if strings.TrimSpace(cfg.CoordinatorURL) == "" {
		return WorkerConfig{}, errors.New("worker coordinator_url is required")
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return WorkerConfig{}, errors.New("worker work_dir is required")
	}
	if strings.TrimSpace(cfg.ProtocolVersion) == "" {
		return WorkerConfig{}, errors.New("worker protocol_version is required")
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
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

	cfg.WorkDir = cleanRequiredPath(cfg.WorkDir)
	cfg.Terminal.BindAddress = strings.TrimSpace(cfg.Terminal.BindAddress)
	cfg.Terminal.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.Terminal.PublicBaseURL), "/")
	cfg.Terminal.TTYDPath = cleanOptionalPath(cfg.Terminal.TTYDPath)
	cfg.Tmux.SocketPath = cleanOptionalPath(cfg.Tmux.SocketPath)
	return cfg, nil
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
