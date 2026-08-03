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
	data, err := generatedWorkerYAML(request, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "capacity:") {
		t.Fatalf("generated worker config contains legacy capacity:\n%s", data)
	}

	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWorker(path)
	if err != nil {
		t.Fatalf("LoadWorker(generated config): %v\n%s", err, data)
	}
	if len(cfg.Accepts) != 1 || string(cfg.Accepts[0]) != string(worker.BucketEphemeral) || cfg.Capacity.Ephemeral != 1 || cfg.Capacity.PersistentAgent != 0 {
		t.Fatalf("generated acceptance = accepts %v capacity %+v", cfg.Accepts, cfg.Capacity)
	}
}
