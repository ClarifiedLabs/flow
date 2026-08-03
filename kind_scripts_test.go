package flow_test

import (
	"os"
	"os/exec"
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
	manifest, err := os.ReadFile("k8s/server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if strings.Contains(text, "kind: Secret") || strings.Contains(text, "CHANGE_ME") {
		t.Fatal("normal Kubernetes apply path must not contain a live or placeholder credential Secret")
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
