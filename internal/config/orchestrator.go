package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"k8s.io/apimachinery/pkg/api/resource"
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
	IdleCapacity     int                           `json:"idle_capacity" yaml:"idle_capacity"`
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
	Namespace                   string                            `json:"namespace" yaml:"namespace"`
	Image                       string                            `json:"image" yaml:"image"`
	ServiceAccount              string                            `json:"service_account" yaml:"service_account"`
	WorkDir                     string                            `json:"work_dir" yaml:"work_dir"`
	WorkerArgs                  []string                          `json:"worker_args" yaml:"worker_args"`
	ImagePullPolicy             string                            `json:"image_pull_policy" yaml:"image_pull_policy"`
	HarnessModelProxySecretName string                            `json:"harness_model_proxy_secret_name" yaml:"harness_model_proxy_secret_name"`
	WorkVolume                  *OrchestratorWorkVolumeConfig     `json:"work_volume,omitempty" yaml:"work_volume,omitempty"`
	Resources                   *OrchestratorResourceRequirements `json:"resources,omitempty" yaml:"resources,omitempty"`
	NodeSelector                map[string]string                 `json:"node_selector,omitempty" yaml:"node_selector,omitempty"`
}

// OrchestratorWorkVolumeConfig describes provider-neutral scratch storage.
// EmptyDir uses SizeLimit; GenericEphemeral uses Size as its PVC storage request.
type OrchestratorWorkVolumeConfig struct {
	Type             string   `json:"type" yaml:"type"`
	MountPath        string   `json:"mount_path,omitempty" yaml:"mount_path,omitempty"`
	SizeLimit        string   `json:"size_limit,omitempty" yaml:"size_limit,omitempty"`
	Size             string   `json:"size,omitempty" yaml:"size,omitempty"`
	StorageClassName string   `json:"storage_class_name,omitempty" yaml:"storage_class_name,omitempty"`
	AccessModes      []string `json:"access_modes,omitempty" yaml:"access_modes,omitempty"`
}

// OrchestratorResourceRequirements is a provider-neutral resource-list pair.
type OrchestratorResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty" yaml:"limits,omitempty"`
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
	IdleCapacity     int
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
	Namespace                   string
	Image                       string
	ServiceAccount              string
	WorkDir                     string
	WorkerArgs                  []string
	ImagePullPolicy             string
	HarnessModelProxySecretName string
	WorkVolume                  *OrchestratorWorkVolumeConfig
	Resources                   *OrchestratorResourceRequirements
	NodeSelector                map[string]string
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
			MaxConcurrency: profile.MaxConcurrency, IdleCapacity: profile.IdleCapacity, AllowedRoles: append([]string(nil), profile.AllowedRoles...),
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
				HarnessModelProxySecretName: profile.Kubernetes.HarnessModelProxySecretName,
				WorkVolume:                  cloneWorkVolume(profile.Kubernetes.WorkVolume), Resources: cloneResourceRequirements(profile.Kubernetes.Resources),
				NodeSelector: cloneStringMap(profile.Kubernetes.NodeSelector),
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
	if profile.IdleCapacity < 0 {
		return OrchestratorProfileConfig{}, errors.New("idle_capacity must be non-negative")
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

// ResolveOrchestratorKubernetesConfig validates and default-applies a
// Kubernetes provider block. It is also used to reject corrupted persisted
// provider options before reconstructing a provider.
func ResolveOrchestratorKubernetesConfig(cfg OrchestratorKubernetesConfig) (ResolvedOrchestratorKubernetesConfig, error) {
	normalized, err := normalizeKubernetesConfig(cfg)
	if err != nil {
		return ResolvedOrchestratorKubernetesConfig{}, err
	}
	return ResolvedOrchestratorKubernetesConfig{
		Namespace: normalized.Namespace, Image: normalized.Image, ServiceAccount: normalized.ServiceAccount,
		WorkDir: normalized.WorkDir, WorkerArgs: append([]string(nil), normalized.WorkerArgs...),
		ImagePullPolicy: normalized.ImagePullPolicy, HarnessModelProxySecretName: normalized.HarnessModelProxySecretName,
		WorkVolume: cloneWorkVolume(normalized.WorkVolume), Resources: cloneResourceRequirements(normalized.Resources),
		NodeSelector: cloneStringMap(normalized.NodeSelector),
	}, nil
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
	}
	if err := validateKubernetesWorkPath("work_dir", cfg.WorkDir); err != nil {
		return OrchestratorKubernetesConfig{}, err
	}
	cfg.WorkerArgs = normalizeWorkerArgs(cfg.WorkerArgs)
	cfg.HarnessModelProxySecretName = strings.TrimSpace(cfg.HarnessModelProxySecretName)
	if cfg.HarnessModelProxySecretName != "" && !validKubernetesSecretName(cfg.HarnessModelProxySecretName) {
		return OrchestratorKubernetesConfig{}, errors.New("harness_model_proxy_secret_name must be a DNS-1123 subdomain")
	}
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
	var err error
	cfg.WorkVolume, err = normalizeWorkVolume(cfg.WorkVolume, cfg.WorkDir)
	if err != nil {
		return OrchestratorKubernetesConfig{}, fmt.Errorf("work_volume: %w", err)
	}
	cfg.Resources, err = normalizeResourceRequirements(cfg.Resources)
	if err != nil {
		return OrchestratorKubernetesConfig{}, fmt.Errorf("resources: %w", err)
	}
	cfg.NodeSelector, err = normalizeKubernetesLabels(cfg.NodeSelector)
	if err != nil {
		return OrchestratorKubernetesConfig{}, fmt.Errorf("node_selector: %w", err)
	}
	return cfg, nil
}

func normalizeWorkVolume(cfg *OrchestratorWorkVolumeConfig, workDir string) (*OrchestratorWorkVolumeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	result := *cfg
	result.Type = strings.ToLower(strings.TrimSpace(result.Type))
	result.MountPath = strings.TrimSpace(result.MountPath)
	result.SizeLimit = strings.TrimSpace(result.SizeLimit)
	result.Size = strings.TrimSpace(result.Size)
	result.StorageClassName = strings.TrimSpace(result.StorageClassName)
	if result.MountPath == "" {
		result.MountPath = workDir
	}
	if err := validateKubernetesWorkPath("mount_path", result.MountPath); err != nil {
		return nil, err
	}
	if workDir != result.MountPath && !strings.HasPrefix(workDir, result.MountPath+"/") {
		return nil, fmt.Errorf("work_dir %q must be within mount_path %q", workDir, result.MountPath)
	}
	switch result.Type {
	case "empty_dir":
		if result.Size != "" || result.StorageClassName != "" || len(result.AccessModes) != 0 {
			return nil, errors.New("empty_dir cannot set size, storage_class_name, or access_modes")
		}
		if result.SizeLimit != "" && !validKubernetesQuantity(result.SizeLimit, true) {
			return nil, fmt.Errorf("size_limit %q is not a positive Kubernetes resource quantity", result.SizeLimit)
		}
	case "generic_ephemeral":
		if result.SizeLimit != "" {
			return nil, errors.New("generic_ephemeral cannot set size_limit")
		}
		if result.Size == "" {
			return nil, errors.New("generic_ephemeral size is required")
		}
		if !validKubernetesQuantity(result.Size, true) {
			return nil, fmt.Errorf("size %q is not a positive Kubernetes resource quantity", result.Size)
		}
		if result.StorageClassName != "" && !validKubernetesDNS1123Subdomain(result.StorageClassName) {
			return nil, errors.New("storage_class_name must be a DNS-1123 subdomain")
		}
		modes, err := normalizeAccessModes(result.AccessModes)
		if err != nil {
			return nil, err
		}
		result.AccessModes = modes
	default:
		return nil, fmt.Errorf("invalid type %q (want empty_dir or generic_ephemeral)", result.Type)
	}
	return &result, nil
}

func validateKubernetesWorkPath(field, value string) error {
	if !path.IsAbs(value) || path.Clean(value) != value {
		return fmt.Errorf("%s must be an absolute, clean container path", field)
	}
	if value == "/" {
		return fmt.Errorf("%s must not be /", field)
	}
	const workerConfigDir = "/var/run/flow"
	if value == workerConfigDir || strings.HasPrefix(value, workerConfigDir+"/") || strings.HasPrefix(workerConfigDir, value+"/") {
		return fmt.Errorf("%s must not overlap %s", field, workerConfigDir)
	}
	return nil
}

func normalizeAccessModes(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"ReadWriteOnce"}, nil
	}
	canonical := map[string]string{
		"readwriteonce": "ReadWriteOnce", "readwritemany": "ReadWriteMany",
		"readwriteoncepod": "ReadWriteOncePod",
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		mode, ok := canonical[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return nil, fmt.Errorf("invalid access mode %q", raw)
		}
		if _, ok := seen[mode]; ok {
			return nil, fmt.Errorf("duplicate access mode %s", mode)
		}
		seen[mode] = struct{}{}
		result = append(result, mode)
	}
	if _, ok := seen["ReadWriteOncePod"]; ok && len(result) != 1 {
		return nil, errors.New("ReadWriteOncePod must be the only access mode")
	}
	return result, nil
}

func normalizeResourceRequirements(cfg *OrchestratorResourceRequirements) (*OrchestratorResourceRequirements, error) {
	if cfg == nil {
		return nil, nil
	}
	result := &OrchestratorResourceRequirements{}
	var err error
	if result.Requests, err = normalizeResourceList(cfg.Requests); err != nil {
		return nil, fmt.Errorf("requests: %w", err)
	}
	if result.Limits, err = normalizeResourceList(cfg.Limits); err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	for _, name := range []string{"cpu", "memory", "ephemeral-storage"} {
		request, hasRequest := result.Requests[name]
		limit, hasLimit := result.Limits[name]
		if !hasRequest || !hasLimit {
			continue
		}
		requestQuantity, _ := parseKubernetesQuantity(request)
		limitQuantity, _ := parseKubernetesQuantity(limit)
		if requestQuantity.Cmp(*limitQuantity) > 0 {
			return nil, fmt.Errorf("requests.%s quantity %q must not exceed limits.%s quantity %q", name, request, name, limit)
		}
	}
	return result, nil
}

func normalizeResourceList(values map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, rawKey := range keys {
		key, value := strings.TrimSpace(rawKey), strings.TrimSpace(values[rawKey])
		if !supportedKubernetesResourceName(key) {
			return nil, fmt.Errorf("unsupported resource name %q (supported: cpu, memory, ephemeral-storage)", rawKey)
		}
		if !validKubernetesQuantity(value, false) {
			return nil, fmt.Errorf("%s quantity %q is not a non-negative Kubernetes resource quantity", key, value)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate resource name %q", key)
		}
		result[key] = value
	}
	return result, nil
}

func supportedKubernetesResourceName(value string) bool {
	switch value {
	case "cpu", "memory", "ephemeral-storage":
		return true
	default:
		return false
	}
}

func validKubernetesQuantity(value string, positive bool) bool {
	quantity, ok := parseKubernetesQuantity(value)
	return ok && quantity.Sign() >= 0 && (!positive || quantity.Sign() > 0)
}

func parseKubernetesQuantity(value string) (*resource.Quantity, bool) {
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		exponent := value[i+1:]
		exponent = strings.TrimPrefix(strings.TrimPrefix(exponent, "+"), "-")
		if exponent != "" && strings.IndexFunc(exponent, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
			parsed, err := strconv.Atoi(value[i+1:])
			if err != nil || parsed < -1000 || parsed > 1000 {
				return nil, false
			}
		}
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, false
	}
	return &quantity, true
}

func normalizeKubernetesLabels(values map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, rawKey := range keys {
		key, value := strings.TrimSpace(rawKey), strings.TrimSpace(values[rawKey])
		if !validKubernetesQualifiedName(key) {
			return nil, fmt.Errorf("invalid Kubernetes label key %q", rawKey)
		}
		if !validKubernetesLabelValue(value) {
			return nil, fmt.Errorf("invalid Kubernetes label value %q for %s", value, key)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate Kubernetes label key %q", key)
		}
		result[key] = value
	}
	return result, nil
}

func validKubernetesQualifiedName(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) > 2 || len(parts) == 2 && !validKubernetesDNS1123Subdomain(parts[0]) {
		return false
	}
	return validKubernetesLabelValue(parts[len(parts)-1]) && parts[len(parts)-1] != ""
}

func validKubernetesLabelValue(value string) bool {
	if len(value) > 63 {
		return false
	}
	if value == "" {
		return true
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && i < len(value)-1 && (c == '-' || c == '_' || c == '.')) {
			return false
		}
	}
	return true
}

func validKubernetesSecretName(value string) bool {
	return validKubernetesDNS1123Subdomain(value)
}

func validKubernetesDNS1123Subdomain(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || !isKubernetesDNS1123AlphaNumeric(label[0]) || !isKubernetesDNS1123AlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !isKubernetesDNS1123AlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isKubernetesDNS1123AlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
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
	if value == nil {
		return nil
	}
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func cloneWorkVolume(value *OrchestratorWorkVolumeConfig) *OrchestratorWorkVolumeConfig {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.AccessModes = append([]string(nil), value.AccessModes...)
	return &cloned
}

func cloneResourceRequirements(value *OrchestratorResourceRequirements) *OrchestratorResourceRequirements {
	if value == nil {
		return nil
	}
	return &OrchestratorResourceRequirements{Requests: cloneStringMap(value.Requests), Limits: cloneStringMap(value.Limits)}
}
