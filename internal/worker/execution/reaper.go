package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/terminal"
)

// flowSessionPrefix is the prefix shared by every tmux session name produced by
// terminal.TmuxSessionNameForJob. Only sessions carrying this prefix are ever
// considered for reaping; anything else belongs to another tool and is left
// untouched.
const flowSessionPrefix = "flow-"

// tmuxReapCommandTimeout bounds each individual tmux invocation so a hung tmux
// server cannot stall worker startup.
const tmuxReapCommandTimeout = 5 * time.Second

type reapOptions struct {
	tmux    config.WorkerTmuxConfig
	workDir string
}

// ReapOption customizes ReapOrphanedSessions.
type ReapOption func(*reapOptions)

// WithWorkerConfig reaps both legacy shared-socket sessions and current
// per-job tmux servers for a worker.
func WithWorkerConfig(cfg config.WorkerConfig) ReapOption {
	return func(options *reapOptions) {
		options.tmux = cfg.Tmux
		options.workDir = cfg.WorkDir
	}
}

// ReapOrphanedSessions kills leaked "flow-" tmux sessions left behind by a
// previously SIGKILLed worker whose deferred cleanup never ran. It is meant to
// be called once at worker startup, before the claim/work loop begins: at that
// point there are no in-flight local claims, so there is no claim race by
// construction.
//
// A live "flow-" session is killed when it maps to a job whose state is
// terminal, or when it maps to no known job at all. Sessions for queued,
// claimed, or running jobs are never touched, and non-"flow-" sessions are
// never touched. It returns the number of sessions killed and a joined error
// accumulating any per-session kill failures; a single failure never aborts the
// loop.
//
// jobs is the full job list fetched from the coordinator (client.ListJobs). The
// candidate map is built FORWARD: each job ID is mapped to its session name via
// terminal.TmuxSessionNameForJob. The name sanitization is lossy, so a session
// name is never reversed back to a job ID.
//
// Scope caveats, accepted by design: the job list is global (not scoped to
// this worker), so with multiple worker processes sharing one host and tmux
// server, a worker may reap a session whose owning process is still alive but
// whose job the coordinator already declared crashed (lease expired) — that
// job has been written off and re-enqueued, so killing its zombie session is
// the desired cleanup; the owning worker notices the lost lease on its next
// heartbeat regardless. Similarly, "maps to no known job" assumes one Flow
// coordinator per host user: the reap-jobs list spans every project of that
// coordinator, so multi-project deployments are fine, but a second
// coordinator's sessions would look unknown here. Both are accepted until
// multi-worker-per-host becomes a first-class topology.
func ReapOrphanedSessions(ctx context.Context, jobs []Job, opts ...ReapOption) (int, error) {
	options := reapOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	killed := 0
	var errs []error
	if strings.TrimSpace(options.workDir) != "" {
		count, err := reapPerJobTmuxServers(ctx, options.workDir, jobs)
		killed += count
		if err != nil {
			errs = append(errs, err)
		}
	}

	count, err := reapSharedTmuxSessions(ctx, options.tmux, jobs)
	killed += count
	if err != nil {
		errs = append(errs, err)
	}

	return killed, errors.Join(errs...)
}

func reapSharedTmuxSessions(ctx context.Context, tmux config.WorkerTmuxConfig, jobs []Job) (int, error) {
	tmuxConfig := config.WorkerConfig{Tmux: tmux}

	jobBySession := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		jobBySession[terminal.TmuxSessionNameForJob(job.ID)] = job
	}

	sessions, err := listFlowTmuxSessions(ctx, tmuxConfig)
	if err != nil {
		return 0, err
	}

	killed := 0
	var errs []error
	for _, session := range sessions {
		job, known := jobBySession[session]
		if known && !IsTerminalJobState(job.State) {
			// Live job (queued/claimed/running): leave its session alone.
			continue
		}
		// Either the job is terminal, or no job owns this session anymore.
		if err := killFlowTmuxSession(ctx, tmuxConfig, session); err != nil {
			errs = append(errs, err)
			continue
		}
		killed++
	}

	return killed, errors.Join(errs...)
}

func reapPerJobTmuxServers(ctx context.Context, workDir string, jobs []Job) (int, error) {
	jobByID := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		jobByID[job.ID] = job
	}

	candidateIDs := make(map[string]struct{}, len(jobByID))
	for jobID := range jobByID {
		candidateIDs[jobID] = struct{}{}
	}
	jobRoot := filepath.Join(workDir, "jobs")
	entries, err := os.ReadDir(jobRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("scan job tmux directories: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			candidateIDs[entry.Name()] = struct{}{}
		}
	}

	killed := 0
	var errs []error
	workerCfg := config.WorkerConfig{WorkDir: workDir}
	for jobID := range candidateIDs {
		job, known := jobByID[jobID]
		if known && !IsTerminalJobState(job.State) {
			continue
		}
		jobCfg, err := tmuxConfigForJob(workerCfg, jobID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sessions, err := listFlowTmuxSessions(ctx, jobCfg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(sessions) == 0 {
			_ = os.Remove(strings.TrimSpace(jobCfg.Tmux.SocketPath))
			continue
		}
		if err := killTmuxServer(ctx, jobCfg); err != nil {
			errs = append(errs, err)
			continue
		}
		killed += len(sessions)
	}

	return killed, errors.Join(errs...)
}

// reaperTerminalJobState mirrors the unexported worker.isTerminalState so the
// reaper agrees with the coordinator on which job states are terminal.
// listFlowTmuxSessions returns the names of live tmux sessions that start with
// the "flow-" prefix. A missing tmux server (no sessions, exit status 1) is
// treated as an empty list rather than an error, mirroring the tolerant pattern
// in tmuxSessionExists.
func listFlowTmuxSessions(ctx context.Context, cfg config.WorkerConfig) ([]string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, tmuxReapCommandTimeout)
	defer cancel()

	output, err := tmuxCommandContext(cmdCtx, cfg, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		if cmdCtx.Err() != nil {
			return nil, fmt.Errorf("list tmux sessions: %w", cmdCtx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// tmux exits non-zero (typically status 1, "no server running" /
			// "no sessions") when there are no sessions to list. Treat that as
			// an empty list rather than a hard error.
			return nil, nil
		}
		return nil, fmt.Errorf("list tmux sessions: %w", err)
	}

	var sessions []string
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, flowSessionPrefix) {
			sessions = append(sessions, name)
		}
	}

	return sessions, nil
}

// killFlowTmuxSession kills a single tmux session, giving the invocation its own
// short timeout so a hung tmux server cannot stall the reap loop.
func killFlowTmuxSession(ctx context.Context, cfg config.WorkerConfig, sessionName string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, tmuxReapCommandTimeout)
	defer cancel()

	if output, err := tmuxCommandContext(cmdCtx, cfg, "kill-session", "-t", sessionName).CombinedOutput(); err != nil {
		if cmdCtx.Err() != nil {
			return fmt.Errorf("kill orphaned tmux session %q: %w", sessionName, cmdCtx.Err())
		}
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("kill orphaned tmux session %q: %s: %w", sessionName, details, err)
		}
		return fmt.Errorf("kill orphaned tmux session %q: %w", sessionName, err)
	}

	return nil
}

func killTmuxServer(ctx context.Context, cfg config.WorkerConfig) error {
	cmdCtx, cancel := context.WithTimeout(ctx, tmuxReapCommandTimeout)
	defer cancel()

	if output, err := tmuxCommandContext(cmdCtx, cfg, "kill-server").CombinedOutput(); err != nil {
		if cmdCtx.Err() != nil {
			return fmt.Errorf("kill tmux server %q: %w", cfg.Tmux.SocketPath, cmdCtx.Err())
		}
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("kill tmux server %q: %s: %w", cfg.Tmux.SocketPath, details, err)
		}
		return fmt.Errorf("kill tmux server %q: %w", cfg.Tmux.SocketPath, err)
	}
	_ = os.Remove(strings.TrimSpace(cfg.Tmux.SocketPath))

	return nil
}
