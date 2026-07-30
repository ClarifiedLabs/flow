package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrchestratorDefaults(t *testing.T) {
	cfg, err := LoadOrchestrator("")
	if err != nil {
		t.Fatalf("load orchestrator defaults: %v", err)
	}

	if cfg.CoordinatorURL != "http://127.0.0.1:8421" {
		t.Fatalf("CoordinatorURL = %q", cfg.CoordinatorURL)
	}
	if cfg.Namespace != "flow" {
		t.Fatalf("Namespace = %q", cfg.Namespace)
	}
	if cfg.Deployment != "flow-worker" {
		t.Fatalf("Deployment = %q", cfg.Deployment)
	}
	if cfg.MinReplicas != 0 || cfg.MaxReplicas != 10 || cfg.DesiredReplicas != 1 {
		t.Fatalf("replicas = %d/%d/%d, want 0/10/1", cfg.MinReplicas, cfg.MaxReplicas, cfg.DesiredReplicas)
	}

	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("resolve defaults: %v", err)
	}
	if resolved.PollInterval != 5*time.Second {
		t.Fatalf("PollInterval = %v", resolved.PollInterval)
	}
	if resolved.ScaleDownIdle != 2*time.Minute {
		t.Fatalf("ScaleDownIdle = %v", resolved.ScaleDownIdle)
	}
}

func TestLoadOrchestratorYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "orchestrator.yaml")
	if err := os.WriteFile(configPath, []byte(`coordinator_url: http://flow-server:8421
namespace: prod
deployment: flow-worker
min_replicas: 1
max_replicas: 20
desired_replicas: 2
poll_interval: 10s
scaledown_idle: 5m
metrics:
  enabled: false
  listen: ":8422"
`), 0o600); err != nil {
		t.Fatalf("write orchestrator config: %v", err)
	}

	cfg, err := LoadOrchestrator(configPath)
	if err != nil {
		t.Fatalf("load orchestrator config: %v", err)
	}

	if cfg.CoordinatorURL != "http://flow-server:8421" {
		t.Fatalf("CoordinatorURL = %q", cfg.CoordinatorURL)
	}
	if cfg.Namespace != "prod" || cfg.Deployment != "flow-worker" {
		t.Fatalf("target = %s/%s", cfg.Namespace, cfg.Deployment)
	}
	if cfg.MinReplicas != 1 || cfg.MaxReplicas != 20 || cfg.DesiredReplicas != 2 {
		t.Fatalf("replicas = %d/%d/%d, want 1/20/2", cfg.MinReplicas, cfg.MaxReplicas, cfg.DesiredReplicas)
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.PollInterval != 10*time.Second || resolved.ScaleDownIdle != 5*time.Minute {
		t.Fatalf("durations = %v/%v", resolved.PollInterval, resolved.ScaleDownIdle)
	}
	if cfg.Metrics.Enabled == nil || *cfg.Metrics.Enabled {
		t.Fatalf("Metrics.Enabled = %v", cfg.Metrics.Enabled)
	}
	if cfg.Metrics.Listen != ":8422" {
		t.Fatalf("Metrics.Listen = %q", cfg.Metrics.Listen)
	}
}

func TestLoadOrchestratorRejectsInvalidReplicas(t *testing.T) {
	cfg := DefaultOrchestrator()
	if _, err := normalizeOrchestrator(cfg); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}

	bad := DefaultOrchestrator()
	bad.MaxReplicas = 0
	bad.DesiredReplicas = 0
	bad.MinReplicas = 1
	if _, err := normalizeOrchestrator(bad); err == nil {
		t.Fatal("min > max accepted")
	}

	bad = DefaultOrchestrator()
	bad.DesiredReplicas = 11
	if _, err := normalizeOrchestrator(bad); err == nil {
		t.Fatal("desired > max accepted")
	}

	bad = DefaultOrchestrator()
	bad.PollInterval = "0s"
	if _, err := normalizeOrchestrator(bad); err == nil {
		t.Fatal("zero poll_interval accepted")
	}

	bad = DefaultOrchestrator()
	bad.Namespace = ""
	if _, err := normalizeOrchestrator(bad); err == nil {
		t.Fatal("empty namespace accepted")
	}
}

func TestApplyOrchestratorEnvOverrides(t *testing.T) {
	cfg := DefaultOrchestrator()
	cfg, err := ApplyOrchestratorEnvOverrides(cfg, func(key string) string {
		switch key {
		case "FLOW_ORCHESTRATOR_TOKEN":
			return " orch-secret "
		case "FLOW_ORCHESTRATOR_NAMESPACE":
			return "staging"
		case "FLOW_ORCHESTRATOR_MAX_REPLICAS":
			return "30"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("apply env overrides: %v", err)
	}
	if cfg.Token != "orch-secret" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.Namespace != "staging" {
		t.Fatalf("Namespace = %q", cfg.Namespace)
	}
	if cfg.MaxReplicas != 30 {
		t.Fatalf("MaxReplicas = %d", cfg.MaxReplicas)
	}

	if _, err := ApplyOrchestratorEnvOverrides(DefaultOrchestrator(), func(key string) string {
		if key == "FLOW_ORCHESTRATOR_MIN_REPLICAS" {
			return "not-an-int"
		}
		return ""
	}); err == nil {
		t.Fatal("invalid integer env override accepted")
	}
}
