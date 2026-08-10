package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

func TestLoadOrchestratorDefaultsPermitPreResolveValidation(t *testing.T) {
	cfg, err := LoadOrchestrator("")
	if err != nil {
		t.Fatalf("LoadOrchestrator defaults: %v", err)
	}
	if cfg.CoordinatorURL != "http://127.0.0.1:8421" || cfg.PollInterval != "5s" || cfg.RetryBase != "1s" || cfg.RetryMax != "1m" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.Profiles != nil && len(cfg.Profiles) != 0 {
		t.Fatalf("default profiles = %+v", cfg.Profiles)
	}
	if _, err := cfg.Resolve(); err == nil || !strings.Contains(err.Error(), "profiles are required") {
		t.Fatalf("Resolve() error = %v, want profiles required", err)
	}
}

func TestLoadOrchestratorYAMLProfilesAndResolve(t *testing.T) {
	path := writeOrchestratorConfig(t, "orchestrator.yaml", `coordinator_url: " http://flow-server:8421 "
token: " secret "
poll_interval: 10s
retry_base: 2s
retry_max: 30s
profiles:
  - name: " linux-large "
    provider: KUBERNETES
    max_concurrency: 7
    idle_capacity: 2
    allowed_roles: [AUTHOR, reviewer]
    accepts: [PERSISTENT_AGENT, ephemeral]
    labels:
      OS: LINUX
      Pool: Agents
    taints:
      - key: " GPU "
        value: " TRUE "
        effect: "NoSchedule"
    harness_models: []
    required_selector:
      SIZE: LARGE
    startup_timeout: 3m
    kubernetes:
      namespace: " workers "
      image: " registry.example/flow-worker:v2 "
      service_account: " worker-sa "
      work_dir: /workspace/worker
      worker_args: [" --no-metrics ", "--metrics-listen=:9000"]
      image_pull_policy: always
      harness_model_proxy_secret_name: " flow-harness-model-proxy "
      harness_config_file: " /etc/flow/harness/config.json "
      work_volume:
        type: generic_ephemeral
        mount_path: /workspace
        size: 20Gi
      resources:
        requests: {cpu: 500m, memory: 1Gi}
        limits: {ephemeral-storage: 5Gi}
      node_selector:
        kubernetes.io/os: linux
metrics:
  enabled: false
  listen: ":8422"
`)

	cfg, err := LoadOrchestrator(path)
	if err != nil {
		t.Fatalf("LoadOrchestrator YAML: %v", err)
	}
	if cfg.CoordinatorURL != "http://flow-server:8421" || cfg.Token != "secret" || len(cfg.Profiles) != 1 {
		t.Fatalf("loaded config = %+v", cfg)
	}
	profile := cfg.Profiles[0]
	if profile.Name != "linux-large" || profile.Provider != "kubernetes" || profile.ProviderID != "kubernetes" {
		t.Fatalf("profile identity = %+v", profile)
	}
	if profile.IdleCapacity != 2 {
		t.Fatalf("idle capacity = %d, want 2", profile.IdleCapacity)
	}
	if !reflect.DeepEqual(profile.AllowedRoles, []string{"author", "reviewer"}) || !reflect.DeepEqual(profile.Accepts, []string{"persistent_agent", "ephemeral"}) {
		t.Fatalf("profile eligibility = roles=%v accepts=%v", profile.AllowedRoles, profile.Accepts)
	}
	if !reflect.DeepEqual(profile.Labels, map[string]string{"os": "linux", "pool": "agents"}) || !reflect.DeepEqual(profile.RequiredSelector, map[string]string{"size": "large"}) {
		t.Fatalf("normalized labels = labels=%v selector=%v", profile.Labels, profile.RequiredSelector)
	}
	wantTaint := scheduler.Taint{Key: "gpu", Value: "true", Effect: scheduler.EffectNoSchedule}
	if !reflect.DeepEqual(profile.Taints, []scheduler.Taint{wantTaint}) {
		t.Fatalf("normalized taints = %+v", profile.Taints)
	}
	if profile.Kubernetes == nil || profile.Kubernetes.Namespace != "workers" || profile.Kubernetes.Image != "registry.example/flow-worker:v2" || profile.Kubernetes.ImagePullPolicy != "Always" || profile.Kubernetes.HarnessModelProxySecretName != "flow-harness-model-proxy" {
		t.Fatalf("Kubernetes = %+v", profile.Kubernetes)
	}
	if profile.Kubernetes.HarnessConfigFile != "/etc/flow/harness/config.json" {
		t.Fatalf("Kubernetes.HarnessConfigFile = %q, want trimmed path", profile.Kubernetes.HarnessConfigFile)
	}
	if !reflect.DeepEqual(profile.Kubernetes.WorkerArgs, []string{"--no-metrics", "--metrics-listen=:9000"}) {
		t.Fatalf("worker args = %#v", profile.Kubernetes.WorkerArgs)
	}
	if profile.Kubernetes.WorkVolume == nil || profile.Kubernetes.WorkVolume.MountPath != "/workspace" || profile.Kubernetes.WorkVolume.Size != "20Gi" ||
		!reflect.DeepEqual(profile.Kubernetes.WorkVolume.AccessModes, []string{"ReadWriteOnce"}) ||
		profile.Kubernetes.Resources.Requests["cpu"] != "500m" || profile.Kubernetes.Resources.Limits["ephemeral-storage"] != "5Gi" ||
		profile.Kubernetes.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("Kubernetes workload settings = %+v", profile.Kubernetes)
	}
	if cfg.Metrics.Enabled == nil || *cfg.Metrics.Enabled || cfg.Metrics.Listen != ":8422" {
		t.Fatalf("metrics = %+v", cfg.Metrics)
	}

	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.PollInterval != 10*time.Second || resolved.RetryBase != 2*time.Second || resolved.RetryMax != 30*time.Second {
		t.Fatalf("resolved durations = %v/%v/%v", resolved.PollInterval, resolved.RetryBase, resolved.RetryMax)
	}
	got := resolved.Profiles[0]
	if got.StartupTimeout != 3*time.Minute || !reflect.DeepEqual(got.Accepts, []scheduler.CapacityBucket{scheduler.CapacityPersistentAgent, scheduler.CapacityEphemeral}) {
		t.Fatalf("resolved profile = %+v", got)
	}
	if got.Kubernetes == nil || got.Darwin != nil || got.Kubernetes.ServiceAccount != "worker-sa" || got.Kubernetes.WorkDir != "/workspace/worker" || got.Kubernetes.HarnessModelProxySecretName != "flow-harness-model-proxy" {
		t.Fatalf("resolved providers = kubernetes=%+v darwin=%+v", got.Kubernetes, got.Darwin)
	}
	if got.Kubernetes.HarnessConfigFile != "/etc/flow/harness/config.json" {
		t.Fatalf("resolved HarnessConfigFile = %q, want /etc/flow/harness/config.json", got.Kubernetes.HarnessConfigFile)
	}
}

func TestLoadOrchestratorJSONPreservesCompleteProfileArray(t *testing.T) {
	path := writeOrchestratorConfig(t, "orchestrator.json", `{
  "profiles": [
    {
      "name": "linux",
      "provider": "kubernetes",
      "provider_id": "cluster-a",
      "max_concurrency": 2,
      "kubernetes": {
        "image": "flow-worker:v1",
        "worker_args": ["--no-metrics"],
        "work_dir": "/home/flow/work",
        "work_volume": {"type": "empty_dir", "mount_path": "/home/flow", "size_limit": "10Gi"},
        "resources": {"requests": {"ephemeral-storage": "1Gi"}},
        "node_selector": {"kubernetes.io/os": "linux"}
      }
    },
    {
      "name": "local",
      "provider": "darwin",
      "provider_id": "mac-a",
      "max_concurrency": 1,
      "allowed_roles": ["console"],
      "accepts": ["persistent_agent"],
      "darwin": {
        "binary": "/opt/flow-worker",
        "state_dir": "/tmp/flow-state",
        "work_dir": "/tmp/flow-work",
        "worker_args": ["--no-metrics"]
      }
    }
  ]
}`)
	cfg, err := LoadOrchestrator(path)
	if err != nil {
		t.Fatalf("LoadOrchestrator JSON: %v", err)
	}
	if len(cfg.Profiles) != 2 || cfg.Profiles[0].Name != "linux" || cfg.Profiles[1].Name != "local" {
		t.Fatalf("profiles = %+v", cfg.Profiles)
	}
	if cfg.Profiles[0].Kubernetes == nil || cfg.Profiles[0].Darwin != nil || cfg.Profiles[1].Darwin == nil || cfg.Profiles[1].Kubernetes != nil {
		t.Fatalf("profile provider blocks were overlaid: %+v", cfg.Profiles)
	}
	linux := cfg.Profiles[0].Kubernetes
	if linux.WorkVolume == nil || linux.WorkVolume.Type != "empty_dir" || linux.WorkVolume.MountPath != "/home/flow" || linux.WorkVolume.SizeLimit != "10Gi" ||
		linux.Resources.Requests["ephemeral-storage"] != "1Gi" || linux.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("JSON Kubernetes workload settings = %+v", linux)
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Profiles[1].Darwin.Binary != "/opt/flow-worker" || resolved.Profiles[1].Darwin.StateDir != "/tmp/flow-state" || resolved.Profiles[1].Darwin.WorkDir != "/tmp/flow-work" {
		t.Fatalf("resolved Darwin options = %+v", resolved.Profiles[1].Darwin)
	}
}

func TestOrchestratorProviderDefaults(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cfg := DefaultOrchestrator()
	cfg.Profiles = []OrchestratorProfileConfig{
		{Name: "linux", Provider: "kubernetes", MaxConcurrency: 1, Kubernetes: &OrchestratorKubernetesConfig{Image: "worker:v1"}},
		{Name: "mac", Provider: "darwin", MaxConcurrency: 2, Darwin: &OrchestratorDarwinConfig{}},
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve defaults: %v", err)
	}
	linux := resolved.Profiles[0]
	if linux.ProviderID != "kubernetes" || linux.StartupTimeout != 2*time.Minute || !reflect.DeepEqual(linux.Accepts, []scheduler.CapacityBucket{scheduler.CapacityEphemeral}) {
		t.Fatalf("profile defaults = %+v", linux)
	}
	if linux.Kubernetes.Namespace != "flow" || linux.Kubernetes.WorkDir != "/var/lib/flow-worker" || linux.Kubernetes.ImagePullPolicy != "IfNotPresent" {
		t.Fatalf("Kubernetes defaults = %+v", linux.Kubernetes)
	}
	mac := resolved.Profiles[1]
	wantState := filepath.Join(dataHome, "flow", "orchestrator")
	if mac.Darwin.Binary != "flow-worker" || mac.Darwin.StateDir != wantState || mac.Darwin.WorkDir != filepath.Join(wantState, "work") {
		t.Fatalf("Darwin defaults = %+v, want state %q", mac.Darwin, wantState)
	}
}

func TestApplyOrchestratorEnvOverridesOnlySupportedFields(t *testing.T) {
	cfg := validOrchestratorConfig()
	requested := map[string]bool{}
	cfg, err := ApplyOrchestratorEnvOverrides(cfg, func(key string) string {
		requested[key] = true
		return map[string]string{
			"FLOW_ORCHESTRATOR_COORDINATOR_URL": " http://coordinator:9999 ",
			"FLOW_ORCHESTRATOR_TOKEN":           " env-token ",
			"FLOW_ORCHESTRATOR_POLL_INTERVAL":   "7s",
			"FLOW_ORCHESTRATOR_RETRY_BASE":      "3s",
			"FLOW_ORCHESTRATOR_RETRY_MAX":       "45s",
			"FLOW_ORCHESTRATOR_NAMESPACE":       "must-be-ignored",
			"FLOW_ORCHESTRATOR_DEPLOYMENT":      "must-be-ignored",
			"FLOW_ORCHESTRATOR_MAX_REPLICAS":    "99",
			"FLOW_ORCHESTRATOR_SCALEDOWN_IDLE":  "1h",
		}[key]
	})
	if err != nil {
		t.Fatalf("ApplyOrchestratorEnvOverrides: %v", err)
	}
	if cfg.CoordinatorURL != "http://coordinator:9999" || cfg.Token != "env-token" || cfg.PollInterval != "7s" || cfg.RetryBase != "3s" || cfg.RetryMax != "45s" {
		t.Fatalf("env config = %+v", cfg)
	}
	for _, old := range []string{"FLOW_ORCHESTRATOR_NAMESPACE", "FLOW_ORCHESTRATOR_DEPLOYMENT", "FLOW_ORCHESTRATOR_MIN_REPLICAS", "FLOW_ORCHESTRATOR_MAX_REPLICAS", "FLOW_ORCHESTRATOR_DESIRED_REPLICAS", "FLOW_ORCHESTRATOR_SCALEDOWN_IDLE"} {
		if requested[old] {
			t.Fatalf("legacy env %s was read", old)
		}
	}
	if _, err := ApplyOrchestratorEnvOverrides(validOrchestratorConfig(), func(key string) string {
		if key == "FLOW_ORCHESTRATOR_RETRY_BASE" {
			return "not-a-duration"
		}
		return ""
	}); err == nil {
		t.Fatal("invalid typed duration override accepted")
	}
}

func TestOrchestratorHarnessModelsAreNormalized(t *testing.T) {
	cfg := validOrchestratorConfig()
	cfg.Profiles[0].HarnessModels = []flowharness.Model{{
		ProviderID: " provider ", ModelID: " model ", QualifiedID: " provider:model ", Harness: " HARNESS ",
	}}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	models := resolved.Profiles[0].HarnessModels
	if len(models) != 1 || models[0].ProviderID != "provider" || models[0].ModelID != "model" || models[0].Harness != "harness" {
		t.Fatalf("models = %+v", models)
	}
}

func TestOrchestratorValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*OrchestratorConfig)
		want string
	}{
		{"coordinator required", func(c *OrchestratorConfig) { c.CoordinatorURL = " " }, "coordinator_url is required"},
		{"poll invalid", func(c *OrchestratorConfig) { c.PollInterval = "soon" }, "poll_interval"},
		{"poll positive", func(c *OrchestratorConfig) { c.PollInterval = "0" }, "greater than zero"},
		{"retry base positive", func(c *OrchestratorConfig) { c.RetryBase = "-1s" }, "retry_base must be greater"},
		{"retry ordering", func(c *OrchestratorConfig) { c.RetryBase, c.RetryMax = "2m", "1m" }, "retry_max must be greater"},
		{"name required", func(c *OrchestratorConfig) { c.Profiles[0].Name = "" }, "name is required"},
		{"duplicate name", func(c *OrchestratorConfig) { c.Profiles = append(c.Profiles, c.Profiles[0]) }, "duplicated"},
		{"provider invalid", func(c *OrchestratorConfig) { c.Profiles[0].Provider = "docker" }, "invalid provider"},
		{"concurrency positive", func(c *OrchestratorConfig) { c.Profiles[0].MaxConcurrency = 0 }, "max_concurrency"},
		{"idle capacity non-negative", func(c *OrchestratorConfig) { c.Profiles[0].IdleCapacity = -1 }, "idle_capacity"},
		{"startup positive", func(c *OrchestratorConfig) { c.Profiles[0].StartupTimeout = "0s" }, "startup_timeout must be greater"},
		{"role invalid", func(c *OrchestratorConfig) { c.Profiles[0].AllowedRoles = []string{"owner"} }, "invalid allowed role"},
		{"role duplicate normalized", func(c *OrchestratorConfig) { c.Profiles[0].AllowedRoles = []string{"author", " AUTHOR "} }, "duplicate role"},
		{"bucket invalid", func(c *OrchestratorConfig) { c.Profiles[0].Accepts = []string{"batch"} }, "invalid capacity bucket"},
		{"bucket duplicate normalized", func(c *OrchestratorConfig) { c.Profiles[0].Accepts = []string{"ephemeral", " EPHEMERAL "} }, "duplicate bucket"},
		{"labels invalid", func(c *OrchestratorConfig) { c.Profiles[0].Labels = map[string]string{"bad key": "x"} }, "invalid label key"},
		{"selector invalid", func(c *OrchestratorConfig) { c.Profiles[0].RequiredSelector = map[string]string{"ok": "bad value"} }, "required_selector"},
		{"taint invalid", func(c *OrchestratorConfig) {
			c.Profiles[0].Taints = []scheduler.Taint{{Key: "gpu", Value: "true", Effect: "PreferNoSchedule"}}
		}, "unsupported taint effect"},
		{"harness model invalid", func(c *OrchestratorConfig) { c.Profiles[0].HarnessModels = []flowharness.Model{{ModelID: "m"}} }, "provider_id is required"},
		{"kubernetes config required", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes = nil }, "requires kubernetes configuration"},
		{"kubernetes mismatch", func(c *OrchestratorConfig) { c.Profiles[0].Darwin = &OrchestratorDarwinConfig{} }, "cannot use darwin"},
		{"kubernetes image required", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.Image = "" }, "image is required"},
		{"work dir absolute", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.WorkDir = "relative/work" }, "absolute, clean"},
		{"work dir clean", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.WorkDir = "/work/../other" }, "absolute, clean"},
		{"work dir root", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.WorkDir = "/" }, "must not be /"},
		{"work dir config overlap", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.WorkDir = "/var/run" }, "must not overlap"},
		{"work volume type", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "pvc"}
		}, "invalid type"},
		{"generic volume size required", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral"}
		}, "size is required"},
		{"empty dir fields", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", AccessModes: []string{"ReadWriteOnce"}}
		}, "cannot set"},
		{"empty dir size field", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", Size: "1Gi"}
		}, "cannot set size"},
		{"empty dir quantity", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", SizeLimit: "-1Gi"}
		}, "size_limit"},
		{"generic size limit", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral", Size: "1Gi", SizeLimit: "2Gi"}
		}, "cannot set size_limit"},
		{"mount path absolute", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", MountPath: "work"}
		}, "absolute, clean"},
		{"mount path clean", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", MountPath: "/home/flow/../work"}
		}, "absolute, clean"},
		{"mount path root", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", MountPath: "/"}
		}, "must not be /"},
		{"mount path config overlap", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", MountPath: "/var/run"}
		}, "must not overlap"},
		{"work dir outside mount", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "empty_dir", MountPath: "/home/flow"}
			c.Profiles[0].Kubernetes.WorkDir = "/work"
		}, "must be within"},
		{"volume access mode", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral", Size: "1Gi", AccessModes: []string{"ReadSometimes"}}
		}, "access mode"},
		{"read-only work volume", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral", Size: "1Gi", AccessModes: []string{"ReadOnlyMany"}}
		}, "access mode"},
		{"ReadWriteOncePod combined", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral", Size: "1Gi", AccessModes: []string{"ReadWriteOncePod", "ReadWriteOnce"}}
		}, "only access mode"},
		{"storage class", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{Type: "generic_ephemeral", Size: "1Gi", StorageClassName: "NOT_VALID"}
		}, "storage_class_name"},
		{"resource quantity", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{Requests: map[string]string{"cpu": "lots"}}
		}, "quantity"},
		{"resource empty quantity", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{Requests: map[string]string{"memory": ""}}
		}, "quantity"},
		{"resource name", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{Limits: map[string]string{"example.com/gpu": "1"}}
		}, "unsupported resource name"},
		{"cpu request exceeds limit", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{Requests: map[string]string{"cpu": "1001m"}, Limits: map[string]string{"cpu": "1"}}
		}, "must not exceed"},
		{"memory request exceeds limit", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{Requests: map[string]string{"memory": "2Gi"}, Limits: map[string]string{"memory": "1024Mi"}}
		}, "must not exceed"},
		{"node selector key", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.NodeSelector = map[string]string{"bad prefix!/node": "linux"}
		}, "label key"},
		{"node selector value", func(c *OrchestratorConfig) {
			c.Profiles[0].Kubernetes.NodeSelector = map[string]string{"kubernetes.io/os": "bad value"}
		}, "label value"},
		{"pull policy invalid", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.ImagePullPolicy = "Sometimes" }, "invalid image_pull_policy"},
		{"model proxy Secret name invalid", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.HarnessModelProxySecretName = "not valid" }, "harness_model_proxy_secret_name"},
		{"harness config file relative", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.HarnessConfigFile = "harness/config.json" }, "harness_config_file must be an absolute, clean path"},
		{"harness config file unclean", func(c *OrchestratorConfig) { c.Profiles[0].Kubernetes.HarnessConfigFile = "/etc/flow/../config.json" }, "harness_config_file must be an absolute, clean path"},
		{"kubernetes os conflict", func(c *OrchestratorConfig) { c.Profiles[0].Labels = map[string]string{"os": "macos"} }, "conflicts with kubernetes"},
		{"darwin config required", func(c *OrchestratorConfig) { c.Profiles[0].Provider = "darwin"; c.Profiles[0].Kubernetes = nil }, "requires darwin configuration"},
		{"darwin mismatch", func(c *OrchestratorConfig) {
			c.Profiles[0].Provider = "darwin"
			c.Profiles[0].Darwin = &OrchestratorDarwinConfig{}
		}, "cannot use kubernetes"},
		{"darwin os conflict", func(c *OrchestratorConfig) {
			c.Profiles[0].Provider = "darwin"
			c.Profiles[0].Kubernetes = nil
			c.Profiles[0].Darwin = &OrchestratorDarwinConfig{}
			c.Profiles[0].Labels = map[string]string{"os": "linux"}
		}, "conflicts with darwin"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validOrchestratorConfig()
			test.edit(&cfg)
			_, err := cfg.Resolve()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestKubernetesQuantityValidation(t *testing.T) {
	for _, value := range []string{"0", "-0", "-0.0", "+1", ".5", "500m", "1Ki", "5e-1", "1E3", "1e100", "1n", "1E"} {
		if !validKubernetesQuantity(value, false) {
			t.Errorf("validKubernetesQuantity(%q, false) = false", value)
		}
	}
	for _, value := range []string{"", "-1", "1e", "1MiB", "1MM", "NaN", "1e1001", "1e2147483648"} {
		if validKubernetesQuantity(value, false) {
			t.Errorf("validKubernetesQuantity(%q, false) = true", value)
		}
	}
	for _, value := range []string{"0", "-0", "0.0", "0m"} {
		if validKubernetesQuantity(value, true) {
			t.Errorf("validKubernetesQuantity(%q, true) = true", value)
		}
	}
}

func TestOrchestratorKubernetesResourceQuantityComparison(t *testing.T) {
	cfg := validOrchestratorConfig()
	cfg.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{
		Requests: map[string]string{"cpu": "5e-1", "memory": "1024Mi"},
		Limits:   map[string]string{"cpu": "500m", "memory": "1Gi"},
	}
	if _, err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve() rejected equivalent quantities: %v", err)
	}
}

func TestOrchestratorKubernetesWorkloadResolutionAndOmission(t *testing.T) {
	omitted, err := validOrchestratorConfig().Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := omitted.Profiles[0].Kubernetes; got.WorkVolume != nil || got.Resources != nil || got.NodeSelector != nil {
		t.Fatalf("omitted workload options changed: %+v", got)
	}

	cfg := validOrchestratorConfig()
	cfg.Profiles[0].Kubernetes.WorkDir = "/home/flow/work"
	cfg.Profiles[0].Kubernetes.WorkVolume = &OrchestratorWorkVolumeConfig{
		Type: " GENERIC_EPHEMERAL ", MountPath: " /home/flow ", Size: " 20Gi ", AccessModes: []string{" readwriteoncepod "},
	}
	cfg.Profiles[0].Kubernetes.Resources = &OrchestratorResourceRequirements{
		Requests: map[string]string{"cpu": " 500m ", "memory": "1Gi"}, Limits: map[string]string{"ephemeral-storage": "5Gi"},
	}
	cfg.Profiles[0].Kubernetes.NodeSelector = map[string]string{" kubernetes.io/os ": " linux ", "node.example.com/Class": "Build_1"}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve workload options: %v", err)
	}
	got := resolved.Profiles[0].Kubernetes
	if got.WorkVolume.Type != "generic_ephemeral" || got.WorkVolume.MountPath != "/home/flow" || got.WorkVolume.Size != "20Gi" || !reflect.DeepEqual(got.WorkVolume.AccessModes, []string{"ReadWriteOncePod"}) {
		t.Fatalf("work volume = %+v", got.WorkVolume)
	}
	if got.Resources.Requests["cpu"] != "500m" || got.Resources.Limits["ephemeral-storage"] != "5Gi" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	if !reflect.DeepEqual(got.NodeSelector, map[string]string{"kubernetes.io/os": "linux", "node.example.com/Class": "Build_1"}) {
		t.Fatalf("node selector = %+v", got.NodeSelector)
	}
	cfg.Profiles[0].Kubernetes.WorkVolume.MountPath = "/mutated"
	cfg.Profiles[0].Kubernetes.WorkVolume.AccessModes[0] = "ReadOnlyMany"
	cfg.Profiles[0].Kubernetes.Resources.Requests["cpu"] = "9"
	cfg.Profiles[0].Kubernetes.NodeSelector["kubernetes.io/os"] = "mutated"
	if got.WorkVolume.MountPath != "/home/flow" || got.WorkVolume.AccessModes[0] != "ReadWriteOncePod" || got.Resources.Requests["cpu"] != "500m" || got.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("resolved Kubernetes settings alias input: %+v", got)
	}
}

func validOrchestratorConfig() OrchestratorConfig {
	cfg := DefaultOrchestrator()
	cfg.Profiles = []OrchestratorProfileConfig{{
		Name: "linux", Provider: "kubernetes", MaxConcurrency: 1,
		Kubernetes: &OrchestratorKubernetesConfig{Image: "flow-worker:v1"},
	}}
	return cfg
}

func writeOrchestratorConfig(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write orchestrator config: %v", err)
	}
	return path
}
