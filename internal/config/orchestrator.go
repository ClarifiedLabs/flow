package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

// DefaultTelemetryListen is the shared default listen address for the
// unauthenticated telemetry port (/readyz, /livez, /metrics) exposed by flow
// binaries. Kubernetes manifests should set ":8422" so probes and Prometheus
// can reach it from outside the pod.
const DefaultTelemetryListen = "127.0.0.1:8422"

const (
	defaultOrchestratorPollInterval  = "5s"
	defaultOrchestratorRetryBase     = "1s"
	defaultOrchestratorRetryMax      = "1m"
	defaultProfileStartupTimeout     = "2m"
	defaultKubernetesNamespace       = "flow"
	defaultKubernetesWorkDir         = "/var/lib/flow-worker"
	defaultKubernetesImagePullPolicy = "IfNotPresent"
	defaultDarwinBinary              = "flow-worker"
)

// OrchestratorConfig configures assignment-scoped worker provisioning.
type OrchestratorConfig struct {
	CoordinatorURL string                      `json:"coordinator_url" yaml:"coordinator_url"`
	Token          string                      `json:"token" yaml:"token"`
	PollInterval   string                      `json:"poll_interval" yaml:"poll_interval"`
	RetryBase      string                      `json:"retry_base" yaml:"retry_base"`
	RetryMax       string                      `json:"retry_max" yaml:"retry_max"`
	Profiles       []OrchestratorProfileConfig `json:"profiles" yaml:"profiles"`
	Metrics        metrics.Config              `json:"metrics" yaml:"metrics"`
}

// OrchestratorProfileConfig describes one assignment scheduling profile and
// the provider used to launch its workers.
type OrchestratorProfileConfig struct {
	Name             string                        `json:"name" yaml:"name"`
	Provider         string                        `json:"provider" yaml:"provider"`
	ProviderID       string                        `json:"provider_id" yaml:"provider_id"`
	MaxConcurrency   int                           `json:"max_concurrency" yaml:"max_concurrency"`
	AllowedRoles     []string                      `json:"allowed_roles" yaml:"allowed_roles"`
	Accepts          []string                      `json:"accepts" yaml:"accepts"`
	Labels           map[string]string             `json:"labels" yaml:"labels"`
	Taints           []scheduler.Taint             `json:"taints" yaml:"taints"`
	HarnessModels    []flowharness.Model           `json:"harness_models" yaml:"harness_models"`
	RequiredSelector map[string]string             `json:"required_selector" yaml:"required_selector"`
	StartupTimeout   string                        `json:"startup_timeout" yaml:"startup_timeout"`
	Kubernetes       *OrchestratorKubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	Darwin           *OrchestratorDarwinConfig     `json:"darwin,omitempty" yaml:"darwin,omitempty"`
}

// OrchestratorKubernetesConfig configures Kubernetes Job workers for a profile.
type OrchestratorKubernetesConfig struct {
	Namespace       string   `json:"namespace" yaml:"namespace"`
	Image           string   `json:"image" yaml:"image"`
	ServiceAccount  string   `json:"service_account" yaml:"service_account"`
	WorkDir         string   `json:"work_dir" yaml:"work_dir"`
	WorkerArgs      []string `json:"worker_args" yaml:"worker_args"`
	ImagePullPolicy string   `json:"image_pull_policy" yaml:"image_pull_policy"`
}

// OrchestratorDarwinConfig configures local Darwin process workers for a profile.
type OrchestratorDarwinConfig struct {
	Binary     string   `json:"binary" yaml:"binary"`
	StateDir   string   `json:"state_dir" yaml:"state_dir"`
	WorkDir    string   `json:"work_dir" yaml:"work_dir"`
	WorkerArgs []string `json:"worker_args" yaml:"worker_args"`
}

// ResolvedOrchestrator is the normalized, parsed orchestrator configuration.
type ResolvedOrchestrator struct {
	CoordinatorURL string
	Token          string
	PollInterval   time.Duration
	RetryBase      time.Duration
	RetryMax       time.Duration
	Profiles       []ResolvedOrchestratorProfile
	Metrics        metrics.Config
}

// ResolvedOrchestratorProfile is a normalized assignment profile.
type ResolvedOrchestratorProfile struct {
	Name             string
	Provider         string
	ProviderID       string
	MaxConcurrency   int
	AllowedRoles     []string
	Accepts          []scheduler.CapacityBucket
	Labels           map[string]string
	Taints           []scheduler.Taint
	HarnessModels    []flowharness.Model
	RequiredSelector map[string]string
	StartupTimeout   time.Duration
	Kubernetes       *ResolvedOrchestratorKubernetesConfig
	Darwin           *ResolvedOrchestratorDarwinConfig
}

// ResolvedOrchestratorKubernetesConfig is a default-applied Kubernetes profile.
type ResolvedOrchestratorKubernetesConfig struct {
	Namespace       string
	Image           string
	ServiceAccount  string
	WorkDir         string
	WorkerArgs      []string
	ImagePullPolicy string
}

// ResolvedOrchestratorDarwinConfig is a default-applied Darwin profile.
type ResolvedOrchestratorDarwinConfig struct {
	Binary     string
	StateDir   string
	WorkDir    string
	WorkerArgs []string
}

// DefaultOrchestrator returns defaults that do not include an assignment
// profile. This permits flag help and token diagnostics without requiring a
// deployment-specific provider configuration; Resolve requires profiles.
func DefaultOrchestrator() OrchestratorConfig {
	return OrchestratorConfig{
		CoordinatorURL: "http://127.0.0.1:8421",
		PollInterval:   defaultOrchestratorPollInterval,
		RetryBase:      defaultOrchestratorRetryBase,
		RetryMax:       defaultOrchestratorRetryMax,
	}
}

// LoadOrchestrator loads path over the scalar defaults. Profiles are always
// taken as one complete array and are never overlaid profile-by-profile.
func LoadOrchestrator(path string) (OrchestratorConfig, error) {
	cfg := DefaultOrchestrator()
	if strings.TrimSpace(path) == "" {
		return normalizeOrchestrator(cfg)
	}

	var fileCfg OrchestratorConfig
	if err := loadConfig(path, &fileCfg); err != nil {
		return OrchestratorConfig{}, err
	}
	if fileCfg.CoordinatorURL != "" {
		cfg.CoordinatorURL = fileCfg.CoordinatorURL
	}
	if fileCfg.Token != "" {
		cfg.Token = fileCfg.Token
	}
	if fileCfg.PollInterval != "" {
		cfg.PollInterval = fileCfg.PollInterval
	}
	if fileCfg.RetryBase != "" {
		cfg.RetryBase = fileCfg.RetryBase
	}
	if fileCfg.RetryMax != "" {
		cfg.RetryMax = fileCfg.RetryMax
	}
	if fileCfg.Profiles != nil {
		cfg.Profiles = append([]OrchestratorProfileConfig(nil), fileCfg.Profiles...)
	}
	cfg.Metrics = fileCfg.Metrics
	return normalizeOrchestrator(cfg)
}

// ApplyOrchestratorEnvOverrides applies the supported typed scalar overrides.
// Provider profiles are intentionally file-only so an environment layer cannot
// partially replace a profile.
func ApplyOrchestratorEnvOverrides(cfg OrchestratorConfig, getenv func(string) string) (OrchestratorConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, entry := range []struct {
		env  string
		dest *string
	}{
		{"FLOW_ORCHESTRATOR_COORDINATOR_URL", &cfg.CoordinatorURL},
		{"FLOW_ORCHESTRATOR_TOKEN", &cfg.Token},
		{"FLOW_ORCHESTRATOR_POLL_INTERVAL", &cfg.PollInterval},
		{"FLOW_ORCHESTRATOR_RETRY_BASE", &cfg.RetryBase},
		{"FLOW_ORCHESTRATOR_RETRY_MAX", &cfg.RetryMax},
	} {
		if value := strings.TrimSpace(getenv(entry.env)); value != "" {
			*entry.dest = value
		}
	}
	return normalizeOrchestrator(cfg)
}

// Resolve applies defaults, validates all profiles, and parses durations.
func (c OrchestratorConfig) Resolve() (ResolvedOrchestrator, error) {
	cfg, err := normalizeOrchestrator(c)
	if err != nil {
		return ResolvedOrchestrator{}, err
	}
	if len(cfg.Profiles) == 0 {
		return ResolvedOrchestrator{}, errors.New("orchestrator profiles are required")
	}

	resolved := ResolvedOrchestrator{
		CoordinatorURL: cfg.CoordinatorURL,
		Token:          cfg.Token,
		PollInterval:   mustParseDuration(cfg.PollInterval),
		RetryBase:      mustParseDuration(cfg.RetryBase),
		RetryMax:       mustParseDuration(cfg.RetryMax),
		Profiles:       make([]ResolvedOrchestratorProfile, 0, len(cfg.Profiles)),
		Metrics:        cfg.Metrics,
	}
	for _, profile := range cfg.Profiles {
		item := ResolvedOrchestratorProfile{
			Name: profile.Name, Provider: profile.Provider, ProviderID: profile.ProviderID,
			MaxConcurrency: profile.MaxConcurrency, AllowedRoles: append([]string(nil), profile.AllowedRoles...),
			Labels: cloneStringMap(profile.Labels), Taints: append([]scheduler.Taint(nil), profile.Taints...),
			HarnessModels: flowharness.CloneModels(profile.HarnessModels), RequiredSelector: cloneStringMap(profile.RequiredSelector),
			StartupTimeout: mustParseDuration(profile.StartupTimeout),
		}
		for _, bucket := range profile.Accepts {
			item.Accepts = append(item.Accepts, scheduler.CapacityBucket(bucket))
		}
		if profile.Kubernetes != nil {
			item.Kubernetes = &ResolvedOrchestratorKubernetesConfig{
				Namespace: profile.Kubernetes.Namespace, Image: profile.Kubernetes.Image,
				ServiceAccount: profile.Kubernetes.ServiceAccount, WorkDir: profile.Kubernetes.WorkDir,
				WorkerArgs: append([]string(nil), profile.Kubernetes.WorkerArgs...), ImagePullPolicy: profile.Kubernetes.ImagePullPolicy,
			}
		}
		if profile.Darwin != nil {
			item.Darwin = &ResolvedOrchestratorDarwinConfig{
				Binary: profile.Darwin.Binary, StateDir: profile.Darwin.StateDir,
				WorkDir: profile.Darwin.WorkDir, WorkerArgs: append([]string(nil), profile.Darwin.WorkerArgs...),
			}
		}
		resolved.Profiles = append(resolved.Profiles, item)
	}
	return resolved, nil
}

func normalizeOrchestrator(cfg OrchestratorConfig) (OrchestratorConfig, error) {
	cfg.CoordinatorURL = strings.TrimSpace(cfg.CoordinatorURL)
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.CoordinatorURL == "" {
		return OrchestratorConfig{}, errors.New("orchestrator coordinator_url is required")
	}
	if strings.TrimSpace(cfg.PollInterval) == "" {
		cfg.PollInterval = defaultOrchestratorPollInterval
	} else {
		cfg.PollInterval = strings.TrimSpace(cfg.PollInterval)
	}
	if strings.TrimSpace(cfg.RetryBase) == "" {
		cfg.RetryBase = defaultOrchestratorRetryBase
	} else {
		cfg.RetryBase = strings.TrimSpace(cfg.RetryBase)
	}
	if strings.TrimSpace(cfg.RetryMax) == "" {
		cfg.RetryMax = defaultOrchestratorRetryMax
	} else {
		cfg.RetryMax = strings.TrimSpace(cfg.RetryMax)
	}
	pollInterval, err := positiveDuration(cfg.PollInterval, "orchestrator poll_interval")
	if err != nil {
		return OrchestratorConfig{}, err
	}
	retryBase, err := positiveDuration(cfg.RetryBase, "orchestrator retry_base")
	if err != nil {
		return OrchestratorConfig{}, err
	}
	retryMax, err := positiveDuration(cfg.RetryMax, "orchestrator retry_max")
	if err != nil {
		return OrchestratorConfig{}, err
	}
	if retryMax < retryBase {
		return OrchestratorConfig{}, errors.New("orchestrator retry_max must be greater than or equal to retry_base")
	}
	_ = pollInterval

	seenNames := make(map[string]struct{}, len(cfg.Profiles))
	profiles := make([]OrchestratorProfileConfig, 0, len(cfg.Profiles))
	for i, raw := range cfg.Profiles {
		profile, err := normalizeOrchestratorProfile(raw)
		if err != nil {
			return OrchestratorConfig{}, fmt.Errorf("orchestrator profile %d: %w", i+1, err)
		}
		if _, exists := seenNames[profile.Name]; exists {
			return OrchestratorConfig{}, fmt.Errorf("orchestrator profile name %q is duplicated", profile.Name)
		}
		seenNames[profile.Name] = struct{}{}
		profiles = append(profiles, profile)
	}
	cfg.Profiles = profiles
	return cfg, nil
}

func normalizeOrchestratorProfile(profile OrchestratorProfileConfig) (OrchestratorProfileConfig, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return OrchestratorProfileConfig{}, errors.New("name is required")
	}
	profile.Provider = strings.ToLower(strings.TrimSpace(profile.Provider))
	profile.ProviderID = strings.TrimSpace(profile.ProviderID)
	if profile.ProviderID == "" {
		profile.ProviderID = profile.Provider
	}
	if profile.MaxConcurrency <= 0 {
		return OrchestratorProfileConfig{}, errors.New("max_concurrency must be greater than zero")
	}
	if strings.TrimSpace(profile.StartupTimeout) == "" {
		profile.StartupTimeout = defaultProfileStartupTimeout
	} else {
		profile.StartupTimeout = strings.TrimSpace(profile.StartupTimeout)
	}
	if _, err := positiveDuration(profile.StartupTimeout, "startup_timeout"); err != nil {
		return OrchestratorProfileConfig{}, err
	}

	roles, err := normalizeProfileRoles(profile.AllowedRoles)
	if err != nil {
		return OrchestratorProfileConfig{}, err
	}
	profile.AllowedRoles = roles
	accepts, err := normalizeProfileBuckets(profile.Accepts)
	if err != nil {
		return OrchestratorProfileConfig{}, err
	}
	profile.Accepts = accepts

	labels, err := scheduler.NormalizeLabels(profile.Labels)
	if err != nil {
		return OrchestratorProfileConfig{}, fmt.Errorf("labels: %w", err)
	}
	profile.Labels = map[string]string(labels)
	selector, err := scheduler.NormalizeLabels(profile.RequiredSelector)
	if err != nil {
		return OrchestratorProfileConfig{}, fmt.Errorf("required_selector: %w", err)
	}
	profile.RequiredSelector = map[string]string(selector)

	profile.Taints = append([]scheduler.Taint(nil), profile.Taints...)
	for i, raw := range profile.Taints {
		taint, err := scheduler.NormalizeTaint(raw)
		if err != nil {
			return OrchestratorProfileConfig{}, fmt.Errorf("taints[%d]: %w", i, err)
		}
		profile.Taints[i] = taint
	}
	profile.HarnessModels, err = flowharness.NormalizeModels(profile.HarnessModels)
	if err != nil {
		return OrchestratorProfileConfig{}, fmt.Errorf("harness_models: %w", err)
	}

	switch profile.Provider {
	case "kubernetes":
		if profile.Kubernetes == nil {
			return OrchestratorProfileConfig{}, errors.New("provider kubernetes requires kubernetes configuration")
		}
		if profile.Darwin != nil {
			return OrchestratorProfileConfig{}, errors.New("provider kubernetes cannot use darwin configuration")
		}
		if osLabel, ok := profile.Labels["os"]; ok && osLabel != "linux" {
			return OrchestratorProfileConfig{}, fmt.Errorf("label os=%s conflicts with kubernetes provider; expected linux", osLabel)
		}
		provider, err := normalizeKubernetesConfig(*profile.Kubernetes)
		if err != nil {
			return OrchestratorProfileConfig{}, fmt.Errorf("kubernetes: %w", err)
		}
		profile.Kubernetes = &provider
	case "darwin":
		if profile.Darwin == nil {
			return OrchestratorProfileConfig{}, errors.New("provider darwin requires darwin configuration")
		}
		if profile.Kubernetes != nil {
			return OrchestratorProfileConfig{}, errors.New("provider darwin cannot use kubernetes configuration")
		}
		if osLabel, ok := profile.Labels["os"]; ok && osLabel != "macos" {
			return OrchestratorProfileConfig{}, fmt.Errorf("label os=%s conflicts with darwin provider; expected macos", osLabel)
		}
		provider, err := normalizeDarwinConfig(*profile.Darwin)
		if err != nil {
			return OrchestratorProfileConfig{}, fmt.Errorf("darwin: %w", err)
		}
		profile.Darwin = &provider
	default:
		return OrchestratorProfileConfig{}, fmt.Errorf("invalid provider %q (want kubernetes or darwin)", profile.Provider)
	}
	return profile, nil
}

func normalizeProfileRoles(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	roles := make([]string, 0, len(values))
	for _, raw := range values {
		role := strings.ToLower(strings.TrimSpace(raw))
		switch role {
		case "author", "reviewer", "verifier", "ci", "console":
		default:
			return nil, fmt.Errorf("invalid allowed role %q", raw)
		}
		if _, exists := seen[role]; exists {
			return nil, fmt.Errorf("allowed_roles contains duplicate role %s", role)
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	return roles, nil
}

func normalizeProfileBuckets(values []string) ([]string, error) {
	if len(values) == 0 {
		values = []string{string(scheduler.CapacityEphemeral)}
	}
	seen := make(map[scheduler.CapacityBucket]struct{}, len(values))
	buckets := make([]string, 0, len(values))
	for _, raw := range values {
		bucket, err := scheduler.ParseCapacityBucket(raw)
		if err != nil {
			return nil, fmt.Errorf("accepts: %w", err)
		}
		if _, exists := seen[bucket]; exists {
			return nil, fmt.Errorf("accepts contains duplicate bucket %s", bucket)
		}
		seen[bucket] = struct{}{}
		buckets = append(buckets, string(bucket))
	}
	return buckets, nil
}

func normalizeKubernetesConfig(cfg OrchestratorKubernetesConfig) (OrchestratorKubernetesConfig, error) {
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	if cfg.Namespace == "" {
		cfg.Namespace = defaultKubernetesNamespace
	}
	cfg.Image = strings.TrimSpace(cfg.Image)
	if cfg.Image == "" {
		return OrchestratorKubernetesConfig{}, errors.New("image is required")
	}
	cfg.ServiceAccount = strings.TrimSpace(cfg.ServiceAccount)
	cfg.WorkDir = strings.TrimSpace(cfg.WorkDir)
	if cfg.WorkDir == "" {
		cfg.WorkDir = defaultKubernetesWorkDir
	} else {
		cfg.WorkDir = cleanRequiredPath(cfg.WorkDir)
	}
	cfg.WorkerArgs = normalizeWorkerArgs(cfg.WorkerArgs)
	policy := strings.ToLower(strings.TrimSpace(cfg.ImagePullPolicy))
	switch policy {
	case "":
		cfg.ImagePullPolicy = defaultKubernetesImagePullPolicy
	case "always":
		cfg.ImagePullPolicy = "Always"
	case "ifnotpresent":
		cfg.ImagePullPolicy = "IfNotPresent"
	case "never":
		cfg.ImagePullPolicy = "Never"
	default:
		return OrchestratorKubernetesConfig{}, fmt.Errorf("invalid image_pull_policy %q", cfg.ImagePullPolicy)
	}
	return cfg, nil
}

func normalizeDarwinConfig(cfg OrchestratorDarwinConfig) (OrchestratorDarwinConfig, error) {
	cfg.Binary = strings.TrimSpace(cfg.Binary)
	if cfg.Binary == "" {
		cfg.Binary = defaultDarwinBinary
	}
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	if cfg.StateDir == "" {
		dataDir, err := DefaultDataDir()
		if err != nil {
			return OrchestratorDarwinConfig{}, err
		}
		cfg.StateDir = filepath.Join(dataDir, "orchestrator")
	} else {
		cfg.StateDir = cleanRequiredPath(cfg.StateDir)
	}
	cfg.WorkDir = strings.TrimSpace(cfg.WorkDir)
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.StateDir, "work")
	} else {
		cfg.WorkDir = cleanRequiredPath(cfg.WorkDir)
	}
	cfg.WorkerArgs = normalizeWorkerArgs(cfg.WorkerArgs)
	return cfg, nil
}

func normalizeWorkerArgs(values []string) []string {
	if values == nil {
		return nil
	}
	normalized := make([]string, len(values))
	for i, value := range values {
		normalized[i] = strings.TrimSpace(value)
	}
	return normalized
}

func positiveDuration(value, field string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return duration, nil
}

func mustParseDuration(value string) time.Duration {
	duration, _ := time.ParseDuration(value)
	return duration
}

func cloneStringMap(value map[string]string) map[string]string {
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}
