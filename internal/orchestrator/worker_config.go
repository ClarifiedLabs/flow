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
	// HarnessConfigFile is the worker-visible path of a Harness JSON config the
	// worker installs as each job's global Harness config. Empty preserves the
	// exact prior config shape.
	HarnessConfigFile string `yaml:"harness_config_file,omitempty"`
}

func generatedWorkerYAML(request LaunchRequest, workDir string, harnessConfigFile string) ([]byte, error) {
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
		Labels: labels, Taints: taints, HarnessConfigFile: strings.TrimSpace(harnessConfigFile),
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
