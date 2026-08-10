package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func TestGeneratedWorkerConfigUsesCanonicalAcceptanceAndLoads(t *testing.T) {
	request := LaunchRequest{
		Assignment:     testAssignment(worker.AssignmentPending),
		CoordinatorURL: "https://coordinator.example",
		WorkerToken:    "direct-worker-token",
	}
	data, err := generatedWorkerYAML(request, "/work", "")
	if err != nil {
		t.Fatal(err)
	}
	// The generated config never advertises capacity or accepted buckets: the
	// worker's bucket is derived from its assignment server-side.
	for _, banned := range []string{"capacity:", "accepts:"} {
		if strings.Contains(string(data), banned) {
			t.Fatalf("generated worker config contains legacy %q:\n%s", banned, data)
		}
	}

	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadWorker(path); err != nil {
		t.Fatalf("LoadWorker(generated config): %v\n%s", err, data)
	}
}

func TestGeneratedWorkerConfigHarnessConfigFile(t *testing.T) {
	request := LaunchRequest{
		Assignment:     testAssignment(worker.AssignmentPending),
		CoordinatorURL: "https://coordinator.example",
		WorkerToken:    "direct-worker-token",
	}
	data, err := generatedWorkerYAML(request, "/work", "/var/run/flow/harness.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "harness_config_file: /var/run/flow/harness.json") {
		t.Fatalf("generated worker config missing harness_config_file:\n%s", data)
	}
	// The generated config must still load as a worker config.
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadWorker(path)
	if err != nil {
		t.Fatalf("LoadWorker(generated config): %v\n%s", err, data)
	}
	if loaded.HarnessConfigFile != "/var/run/flow/harness.json" {
		t.Fatalf("loaded HarnessConfigFile = %q", loaded.HarnessConfigFile)
	}

	data, err = generatedWorkerYAML(request, "/work", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "harness_config_file") {
		t.Fatalf("unconfigured generated worker config mentions harness_config_file:\n%s", data)
	}
}
