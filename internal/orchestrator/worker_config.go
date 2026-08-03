package orchestrator

import (
	"errors"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/worker"
	"gopkg.in/yaml.v3"
)

type generatedWorkerConfig struct {
	WorkerID       string                  `yaml:"worker_id"`
	CoordinatorURL string                  `yaml:"coordinator_url"`
	Token          string                  `yaml:"token"`
	WorkDir        string                  `yaml:"work_dir"`
	Labels         map[string]string       `yaml:"labels,omitempty"`
	Taints         []scheduler.Taint       `yaml:"taints,omitempty"`
	Accepts        []worker.CapacityBucket `yaml:"accepts"`
}

func generatedWorkerYAML(request LaunchRequest, workDir string) ([]byte, error) {
	a := request.Assignment.Assignment
	if strings.TrimSpace(request.CoordinatorURL) == "" || strings.TrimSpace(request.WorkerToken) == "" || strings.TrimSpace(workDir) == "" {
		return nil, errors.New("coordinator URL, direct worker token, and worker work dir are required")
	}
	config := generatedWorkerConfig{
		WorkerID: a.WorkerID, CoordinatorURL: request.CoordinatorURL,
		Token: request.WorkerToken, WorkDir: workDir,
		Labels: a.ProfileLabels, Taints: a.ProfileTaints,
		Accepts: []worker.CapacityBucket{a.CapacityBucket},
	}
	return yaml.Marshal(config)
}

func validateWorkerArgs(args []string) error {
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "--config" || trimmed == "-c" || strings.HasPrefix(trimmed, "--config=") || strings.HasPrefix(trimmed, "-c=") || trimmed == "--one-shot" {
			return errors.New("worker args must not override --config or --one-shot")
		}
	}
	return nil
}
