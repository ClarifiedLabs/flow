package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/metrics"
)

// DefaultTelemetryListen is the shared default listen address for the
// unauthenticated telemetry port (/readyz, /livez, /metrics) exposed by flow
// binaries. Kubernetes manifests should set ":8422" so probes and Prometheus
// can reach it from outside the pod.
const DefaultTelemetryListen = "127.0.0.1:8422"

// OrchestratorConfig configures the flow-orchestrator binary: it polls the
// coordinator's queue depth and scales the worker Kubernetes Deployment,
// maintaining a standby pool of ephemeral workers.
type OrchestratorConfig struct {
	// CoordinatorURL is the base URL of the flow coordinator.
	CoordinatorURL string `json:"coordinator_url" yaml:"coordinator_url"`
	// Token is the orchestrator-scoped bearer token (authorizes only
	// GET /v2/queue/stats). Prefer FLOW_ORCHESTRATOR_TOKEN in deployments.
	Token string `json:"token" yaml:"token"`
	// Namespace is the Kubernetes namespace of the worker Deployment.
	Namespace string `json:"namespace" yaml:"namespace"`
	// Deployment is the name of the worker Kubernetes Deployment.
	Deployment string `json:"deployment" yaml:"deployment"`
	// MinReplicas is the floor for the standby worker pool (default 0).
	MinReplicas int `json:"min_replicas" yaml:"min_replicas"`
	// MaxReplicas is the ceiling for worker scale-out (default 10).
	MaxReplicas int `json:"max_replicas" yaml:"max_replicas"`
	// DesiredReplicas is the standby pool size: workers up and ready for a
	// job assignment even when the queue is empty (default 1).
	DesiredReplicas int `json:"desired_replicas" yaml:"desired_replicas"`
	// PollInterval is the Go duration between scaling cycles (default "5s").
	PollInterval string `json:"poll_interval" yaml:"poll_interval"`
	// ScaleDownIdle is how long the queue must stay empty before scaling
	// down toward the standby pool (default "2m"). "0" disables the delay.
	ScaleDownIdle string `json:"scaledown_idle" yaml:"scaledown_idle"`
	// Metrics configures the telemetry endpoint.
	Metrics metrics.Config `json:"metrics" yaml:"metrics"`
}

// ResolvedOrchestrator is the parsed, default-applied orchestrator policy.
type ResolvedOrchestrator struct {
	PollInterval  time.Duration
	ScaleDownIdle time.Duration
}

// DefaultOrchestrator returns the built-in orchestrator defaults, targeting
// the flow-worker Deployment in the flow namespace (matching the reference
// Kubernetes manifests).
func DefaultOrchestrator() OrchestratorConfig {
	return OrchestratorConfig{
		CoordinatorURL:  "http://127.0.0.1:8421",
		Namespace:       "flow",
		Deployment:      "flow-worker",
		MaxReplicas:     10,
		DesiredReplicas: 1,
		PollInterval:    "5s",
		ScaleDownIdle:   "2m",
	}
}

// LoadOrchestrator loads the orchestrator config from path, applying built-in
// defaults for unset values.
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
	if fileCfg.Namespace != "" {
		cfg.Namespace = fileCfg.Namespace
	}
	if fileCfg.Deployment != "" {
		cfg.Deployment = fileCfg.Deployment
	}
	if fileCfg.MinReplicas != 0 {
		cfg.MinReplicas = fileCfg.MinReplicas
	}
	if fileCfg.MaxReplicas != 0 {
		cfg.MaxReplicas = fileCfg.MaxReplicas
	}
	if fileCfg.DesiredReplicas != 0 {
		cfg.DesiredReplicas = fileCfg.DesiredReplicas
	}
	if fileCfg.PollInterval != "" {
		cfg.PollInterval = fileCfg.PollInterval
	}
	if fileCfg.ScaleDownIdle != "" {
		cfg.ScaleDownIdle = fileCfg.ScaleDownIdle
	}
	cfg.Metrics = fileCfg.Metrics

	return normalizeOrchestrator(cfg)
}

// ApplyOrchestratorEnvOverrides applies deployment-specific orchestrator
// overrides after a file config has been loaded. It mirrors
// ApplyWorkerEnvOverrides: a typed layer rather than generic env-to-YAML
// templating so invalid values fail at startup.
func ApplyOrchestratorEnvOverrides(cfg OrchestratorConfig, getenv func(string) string) (OrchestratorConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_COORDINATOR_URL")); value != "" {
		cfg.CoordinatorURL = value
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_TOKEN")); value != "" {
		cfg.Token = value
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_NAMESPACE")); value != "" {
		cfg.Namespace = value
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_DEPLOYMENT")); value != "" {
		cfg.Deployment = value
	}
	for _, entry := range []struct {
		env  string
		dest *int
	}{
		{"FLOW_ORCHESTRATOR_MIN_REPLICAS", &cfg.MinReplicas},
		{"FLOW_ORCHESTRATOR_MAX_REPLICAS", &cfg.MaxReplicas},
		{"FLOW_ORCHESTRATOR_DESIRED_REPLICAS", &cfg.DesiredReplicas},
	} {
		if value := strings.TrimSpace(getenv(entry.env)); value != "" {
			replicas, err := strconv.Atoi(value)
			if err != nil {
				return OrchestratorConfig{}, fmt.Errorf("%s must be an integer: %w", entry.env, err)
			}
			*entry.dest = replicas
		}
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_POLL_INTERVAL")); value != "" {
		cfg.PollInterval = value
	}
	if value := strings.TrimSpace(getenv("FLOW_ORCHESTRATOR_SCALEDOWN_IDLE")); value != "" {
		cfg.ScaleDownIdle = value
	}

	return normalizeOrchestrator(cfg)
}

// Resolve parses the duration fields of the config into a
// ResolvedOrchestrator.
func (c OrchestratorConfig) Resolve() (ResolvedOrchestrator, error) {
	pollInterval, err := time.ParseDuration(c.PollInterval)
	if err != nil {
		return ResolvedOrchestrator{}, fmt.Errorf("orchestrator poll_interval: %w", err)
	}
	if pollInterval <= 0 {
		return ResolvedOrchestrator{}, errors.New("orchestrator poll_interval must be greater than zero")
	}
	scaleDownIdle, err := time.ParseDuration(c.ScaleDownIdle)
	if err != nil {
		return ResolvedOrchestrator{}, fmt.Errorf("orchestrator scaledown_idle: %w", err)
	}
	if scaleDownIdle < 0 {
		return ResolvedOrchestrator{}, errors.New("orchestrator scaledown_idle cannot be negative")
	}

	return ResolvedOrchestrator{PollInterval: pollInterval, ScaleDownIdle: scaleDownIdle}, nil
}

func normalizeOrchestrator(cfg OrchestratorConfig) (OrchestratorConfig, error) {
	if strings.TrimSpace(cfg.CoordinatorURL) == "" {
		return OrchestratorConfig{}, errors.New("orchestrator coordinator_url is required")
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
		return OrchestratorConfig{}, errors.New("orchestrator namespace is required")
	}
	if strings.TrimSpace(cfg.Deployment) == "" {
		return OrchestratorConfig{}, errors.New("orchestrator deployment is required")
	}
	if cfg.MinReplicas < 0 {
		return OrchestratorConfig{}, errors.New("orchestrator min_replicas cannot be negative")
	}
	if cfg.MaxReplicas < cfg.MinReplicas {
		return OrchestratorConfig{}, errors.New("orchestrator max_replicas must be >= min_replicas")
	}
	if cfg.DesiredReplicas < cfg.MinReplicas {
		return OrchestratorConfig{}, errors.New("orchestrator desired_replicas must be >= min_replicas")
	}
	if cfg.DesiredReplicas > cfg.MaxReplicas {
		return OrchestratorConfig{}, errors.New("orchestrator desired_replicas must be <= max_replicas")
	}
	if _, err := cfg.Resolve(); err != nil {
		return OrchestratorConfig{}, err
	}

	cfg.CoordinatorURL = strings.TrimSpace(cfg.CoordinatorURL)
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	cfg.Deployment = strings.TrimSpace(cfg.Deployment)
	return cfg, nil
}
