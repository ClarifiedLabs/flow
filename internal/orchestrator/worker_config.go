package orchestrator

import (
	"errors"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"gopkg.in/yaml.v3"
)

// generatedWorkerConfig is the capacity-slot-scoped one-shot worker config the
// provider writes. The worker's capacity bucket is derived from its assignment
// server-side; the config never advertises capacity or accepted buckets.
type generatedWorkerConfig struct {
	WorkerID       string            `yaml:"worker_id"`
	CoordinatorURL string            `yaml:"coordinator_url"`
	Token          string            `yaml:"token"`
	WorkDir        string            `yaml:"work_dir"`
	Labels         map[string]string `yaml:"labels,omitempty"`
	Taints         []scheduler.Taint `yaml:"taints,omitempty"`
}

func generatedWorkerYAML(request LaunchRequest, workDir string) ([]byte, error) {
	workerID := request.Assignment.Assignment.WorkerID
	labels := request.Assignment.Assignment.ProfileLabels
	taints := request.Assignment.Assignment.ProfileTaints
	if request.Slot != nil {
		workerID = request.Slot.WorkerID
		labels = request.Slot.ProfileLabels
		taints = request.Slot.ProfileTaints
	}
	if strings.TrimSpace(request.CoordinatorURL) == "" || strings.TrimSpace(request.WorkerToken) == "" || strings.TrimSpace(workDir) == "" {
		return nil, errors.New("coordinator URL, direct worker token, and worker work dir are required")
	}
	config := generatedWorkerConfig{
		WorkerID: workerID, CoordinatorURL: request.CoordinatorURL,
		Token: request.WorkerToken, WorkDir: workDir,
		Labels: labels, Taints: taints,
	}
	return yaml.Marshal(config)
}

func validateWorkerArgs(args []string) error {
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "run" || trimmed == "--config" || trimmed == "-c" || strings.HasPrefix(trimmed, "--config=") || strings.HasPrefix(trimmed, "-c=") || trimmed == "--one-shot" {
			return errors.New("worker args must not override run, --config, or --one-shot")
		}
	}
	return nil
}
