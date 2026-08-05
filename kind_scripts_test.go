package flow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKindSmokeDisablesInteractiveCredentialHelpers(t *testing.T) {
	script, err := os.ReadFile("scripts/kind/smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, `git -C "${SMOKE_REPO}" config credential.helper ''`) {
		t.Fatal("kind smoke must disable workstation credential helpers before flow init")
	}
	if !strings.Contains(text, `git -C "${SMOKE_REPO}" remote remove flow`) {
		t.Fatal("kind smoke must replace the generated repository's stale exchange remote")
	}
	if !strings.Contains(text, "PROJECT_ID=p-flow-kind-smoke") {
		t.Fatal("kind smoke must use the normalized coordinator project id")
	}
	if !strings.Contains(text, `export KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"`) {
		t.Fatal("kind smoke must preserve kubeconfig before isolating HOME")
	}
}

func TestKubernetesReferenceManifestDoesNotPublishCredentials(t *testing.T) {
	server, err := os.ReadFile("k8s/server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if text := string(server); strings.Contains(text, "kind: Secret") || strings.Contains(text, "CHANGE_ME") {
		t.Fatal("server manifest must not contain a live or placeholder credential Secret")
	}

	orchestrator, err := os.ReadFile("k8s/orchestrator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The profile itself is mounted from a Secret, but it must contain only a
	// reference to the out-of-band model proxy Secret, never its credential keys.
	if text := string(orchestrator); strings.Contains(text, "CHANGE_ME") ||
		strings.Contains(text, "HARNESS_MODEL_PROXY_URL") || strings.Contains(text, "HARNESS_MODEL_PROXY_API_KEY") {
		t.Fatal("orchestrator manifest must not publish model proxy credentials")
	}
}

func TestKindCreatesHarnessModelProxySecretFromLocalEnvironment(t *testing.T) {
	up, err := os.ReadFile("scripts/kind/up.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "apply_harness_model_proxy_secret") {
		t.Fatal("kind up must create the local Harness model proxy Secret")
	}
	common, err := os.ReadFile("scripts/kind/common.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(common)
	for _, want := range []string{
		"kube create secret generic flow-harness-model-proxy --namespace flow",
		"--from-file=HARNESS_MODEL_PROXY_URL=\"${MODEL_PROXY_URL_FILE}\"",
		"--from-file=HARNESS_MODEL_PROXY_API_KEY=\"${MODEL_PROXY_API_KEY_FILE}\"",
		"HARNESS_MODEL_PROXY_URL and HARNESS_MODEL_PROXY_API_KEY must be set",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("kind proxy Secret setup missing %q", want)
		}
	}
	if strings.Contains(text, "--from-literal=HARNESS_MODEL_PROXY") {
		t.Fatal("kind proxy Secret setup must not expose credentials as command-line literals")
	}
}

func TestKindHarnessModelProxySecretRequiresAndStoresLocalEnv(t *testing.T) {
	stateDir := t.TempDir()
	const apply = `
source "$1"
STATE_DIR="$2"
TOKEN_DIR="${STATE_DIR}/tokens"
MODEL_PROXY_URL_FILE="${TOKEN_DIR}/harness-model-proxy-url"
MODEL_PROXY_API_KEY_FILE="${TOKEN_DIR}/harness-model-proxy-api-key"
ensure_state_dirs
kube() { cat >/dev/null; }
apply_harness_model_proxy_secret
`
	cmd := exec.Command("bash", "-c", apply, "bash", "scripts/kind/common.sh", stateDir)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HARNESS_MODEL_PROXY_URL=http://proxy.test:8080",
		"HARNESS_MODEL_PROXY_API_KEY=test-api-key",
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply local model proxy Secret: %v\n%s", err, output)
	}
	for path, want := range map[string]string{
		filepath.Join(stateDir, "tokens", "harness-model-proxy-url"):     "http://proxy.test:8080",
		filepath.Join(stateDir, "tokens", "harness-model-proxy-api-key"): "test-api-key",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read local proxy credential file %s: %v", path, err)
		}
		if got := string(contents); got != want {
			t.Errorf("local proxy credential %s = %q, want %q", path, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat local proxy credential file %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("local proxy credential mode %s = %04o, want 0600", path, got)
		}
	}

	cmd = exec.Command("bash", "-c", `source "$1"; apply_harness_model_proxy_secret`, "bash", "scripts/kind/common.sh")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "HARNESS_MODEL_PROXY_URL and HARNESS_MODEL_PROXY_API_KEY must be set") {
		t.Fatalf("missing local model proxy variables: err=%v output=%s", err, output)
	}
}

func TestKubernetesOrchestratorCanVerifyOwnedSecrets(t *testing.T) {
	manifest, err := os.ReadFile("k8s/orchestrator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if !strings.Contains(text, `resources: ["secrets"]`) || !strings.Contains(text, `verbs: ["create", "get", "update", "delete"]`) {
		t.Fatal("orchestrator RBAC must permit owned Secret inspection and credential replacement")
	}
	if !strings.Contains(text, `resources: ["jobs"]`) || !strings.Contains(text, `verbs: ["create", "get", "list", "delete"]`) {
		t.Fatal("orchestrator RBAC must permit provider health listing and owned Job lifecycle operations")
	}
	if !strings.Contains(text, `harness_model_proxy_secret_name: flow-harness-model-proxy`) {
		t.Fatal("orchestrator profile must reference the out-of-band Harness model proxy Secret")
	}
}

func TestKindScriptsHonorAndValidateAPIHostPort(t *testing.T) {
	command := `source scripts/kind/common.sh; validate_cluster_name; printf '%s' "$API_HOST_PORT"`
	cmd := exec.Command("bash", "-c", command)
	cmd.Env = append(os.Environ(), "FLOW_KIND_API_HOST_PORT=18421")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("valid host-port override failed: %v\n%s", err, output)
	}
	if got := string(output); got != "18421" {
		t.Fatalf("API_HOST_PORT = %q, want 18421", got)
	}

	cmd = exec.Command("bash", "-c", command)
	cmd.Env = append(os.Environ(), "FLOW_KIND_API_HOST_PORT=invalid")
	output, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid host-port override unexpectedly succeeded: %s", output)
	}
	if !strings.Contains(string(output), "FLOW_KIND_API_HOST_PORT must be an integer") {
		t.Fatalf("invalid host-port error = %q", output)
	}
}
