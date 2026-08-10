package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/orchestrator"
)

func TestRunHelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--help) exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow-orchestrator") {
		t.Fatalf("help output missing usage:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow-orchestrator") {
		t.Fatalf("version output:\n%s", stdout.String())
	}
}

func TestProviderOptionsSerializeDeterministicallyAndReconstruct(t *testing.T) {
	encoded, err := encodeProviderOption(map[string]string{"z": "last", "a": "first"})
	if err != nil || encoded != `{"a":"first","z":"last"}` {
		t.Fatalf("encodeProviderOption = %q, %v", encoded, err)
	}
	volume := &config.OrchestratorWorkVolumeConfig{
		Type: "generic_ephemeral", MountPath: "/home/flow", Size: "5Gi", AccessModes: []string{"ReadWriteOnce"},
	}
	encoded, err = encodeProviderOption(volume)
	if err != nil || encoded != `{"type":"generic_ephemeral","mount_path":"/home/flow","size":"5Gi","access_modes":["ReadWriteOnce"]}` {
		t.Fatalf("encoded work volume = %q, %v", encoded, err)
	}
	profile := orchestrator.Profile{ProviderType: "kubernetes", ProviderOptions: map[string]string{
		"namespace": "flow", "image": "worker:v1", "service_account": "flow-worker", "work_dir": "/home/flow/work",
		"image_pull_policy": "IfNotPresent", "harness_model_proxy_secret_name": "flow-harness-model-proxy",
		"harness_config_file": "/etc/flow/harness/config.json",
		"work_volume":         encoded,
		"resources":           `{"requests":{"cpu":"500m","memory":"1Gi"},"limits":{"ephemeral-storage":"5Gi"}}`,
		"node_selector":       `{"kubernetes.io/os":"linux"}`,
	}}
	got, err := kubernetesProviderOptionsFromProfile(profile, []string{"--no-metrics"})
	if err != nil {
		t.Fatalf("reconstruct Kubernetes options: %v", err)
	}
	if got.WorkVolume == nil || got.WorkVolume.MountPath != "/home/flow" || got.WorkVolume.Size != "5Gi" ||
		got.Resources.Requests["cpu"] != "500m" || got.Resources.Limits["ephemeral-storage"] != "5Gi" ||
		got.NodeSelector["kubernetes.io/os"] != "linux" || got.HarnessModelProxySecretName != "flow-harness-model-proxy" {
		t.Fatalf("reconstructed options = %+v", got)
	}
	if got.HarnessConfigFile != "/etc/flow/harness/config.json" {
		t.Fatalf("reconstructed HarnessConfigFile = %q", got.HarnessConfigFile)
	}
}

func TestPersistedProviderOptionsFailClosed(t *testing.T) {
	base := map[string]string{"namespace": "flow", "image": "worker:v1", "work_dir": "/work"}
	for _, missing := range []string{"namespace", "image", "work_dir"} {
		options := make(map[string]string, len(base)-1)
		for key, value := range base {
			if key != missing {
				options[key] = value
			}
		}
		profile := orchestrator.Profile{ProviderType: "kubernetes", ProviderOptions: options}
		if _, err := kubernetesProviderOptionsFromProfile(profile, nil); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing persisted %s error = %v", missing, err)
		}
	}
	for _, raw := range []string{"", "null", `{broken`, `{"type":"empty_dir","unknown":true}`, `{"type":"empty_dir"} {}`} {
		options := make(map[string]string, len(base)+1)
		for key, value := range base {
			options[key] = value
		}
		options["work_volume"] = raw
		profile := orchestrator.Profile{ProviderType: "kubernetes", ProviderOptions: options}
		if _, err := kubernetesProviderOptionsFromProfile(profile, nil); err == nil {
			t.Fatalf("malformed persisted work_volume %q was accepted", raw)
		}
	}

	profile := orchestrator.Profile{ProviderType: "kubernetes", ProviderOptions: map[string]string{
		"namespace": "flow", "image": "worker:v1", "work_dir": "/work",
		"work_volume": `{"type":"generic_ephemeral","size":"invalid"}`,
	}}
	if _, err := kubernetesProviderOptionsFromProfile(profile, nil); err == nil || !strings.Contains(err.Error(), "validate persisted kubernetes options") {
		t.Fatalf("semantically invalid persisted option error = %v", err)
	}
}

func TestValidateHarnessConfigFile(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(valid, []byte(`{"default_agent":{"model":"test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateHarnessConfigFile(valid); err != nil {
		t.Fatalf("validateHarnessConfigFile(valid) = %v", err)
	}
	if err := validateHarnessConfigFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("validateHarnessConfigFile accepted a missing file")
	}
	for name, content := range map[string]string{
		"broken.json":    `{"default_agent":`,
		"nonobject.json": `[1,2]`,
		"null.json":      `null`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateHarnessConfigFile(path); err == nil {
			t.Fatalf("validateHarnessConfigFile(%s) accepted %q", name, content)
		}
	}
}

func TestBuildProfilesValidatesHarnessConfigFileAtStartup(t *testing.T) {
	resolved := config.ResolvedOrchestrator{
		Profiles: []config.ResolvedOrchestratorProfile{{
			Name: "linux", Provider: "kubernetes",
			Kubernetes: &config.ResolvedOrchestratorKubernetesConfig{
				Namespace: "flow", Image: "worker:v1", WorkDir: "/work",
				HarnessConfigFile: filepath.Join(t.TempDir(), "missing.json"),
			},
		}},
	}
	_, _, err := buildProfilesAndProviders(resolved)
	if err == nil || !strings.Contains(err.Error(), "profile linux") || !strings.Contains(err.Error(), "harness_config_file") {
		t.Fatalf("buildProfilesAndProviders() error = %v, want profile-scoped harness_config_file failure", err)
	}
}

func TestRunRequiresToken(t *testing.T) {
	// testenv clears the environment, so FLOW_ORCHESTRATOR_TOKEN is unset and
	// the default config carries no token.
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run() without token exit = %d, want 1; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "orchestrator token is required") {
		t.Fatalf("stderr missing token error: %s", stderr.String())
	}
}
