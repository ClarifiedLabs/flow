package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrDarwinProcessUnsupported is returned when the Darwin-only provider is
// constructed or used on another operating system.
var ErrDarwinProcessUnsupported = errors.New("Darwin process provider is unsupported on this operating system")

// DarwinProcessProviderOptions configures capacity-slot-scoped local processes.
type DarwinProcessProviderOptions struct {
	StateDir     string
	Executable   string
	WorkDir      string
	WorkerArgs   []string
	TermTimeout  time.Duration
	PollInterval time.Duration
}

// DarwinProcessProvider persists enough state to inspect and delete a child
// after the orchestrator itself restarts.
type DarwinProcessProvider struct {
	stateDir     string
	executable   string
	workDir      string
	workerArgs   []string
	termTimeout  time.Duration
	pollInterval time.Duration
	mu           sync.Mutex
}

// NewDarwinProcessProvider constructs a local process provider. It fails
// clearly on non-Darwin platforms rather than silently changing semantics.
func NewDarwinProcessProvider(options DarwinProcessProviderOptions) (*DarwinProcessProvider, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrDarwinProcessUnsupported
	}
	options.StateDir = strings.TrimSpace(options.StateDir)
	if options.StateDir == "" {
		return nil, errors.New("Darwin process provider state dir is required")
	}
	if strings.TrimSpace(options.Executable) == "" {
		options.Executable = "flow-worker"
	}
	if options.TermTimeout <= 0 {
		options.TermTimeout = 5 * time.Second
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 25 * time.Millisecond
	}
	if err := validateWorkerArgs(options.WorkerArgs); err != nil {
		return nil, err
	}
	stateDir, err := canonicalProcessRoot(options.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Darwin process state dir: %w", err)
	}
	workDir := strings.TrimSpace(options.WorkDir)
	if workDir != "" {
		workDir, err = canonicalProcessRoot(workDir)
		if err != nil {
			return nil, fmt.Errorf("resolve Darwin process work dir: %w", err)
		}
	}
	return &DarwinProcessProvider{
		stateDir: stateDir, executable: options.Executable,
		workDir: workDir, workerArgs: append([]string(nil), options.WorkerArgs...),
		termTimeout: options.TermTimeout, pollInterval: options.PollInterval,
	}, nil
}

func (p *DarwinProcessProvider) darwinProcessStateRoot(AssignmentIdentity) string {
	// The provider is constructed from either the approved live profile or the
	// assignment's persisted descriptor. Always use that canonicalized root;
	// request data must not redirect an already-constructed provider.
	return p.stateDir
}

// DarwinProcessStateDir returns the stable private state directory for an assignment.
func (p *DarwinProcessProvider) DarwinProcessStateDir(identity AssignmentIdentity) string {
	jobName, _ := KubernetesResourceNames(identity)
	return filepath.Join(p.darwinProcessStateRoot(identity), jobName)
}

// DarwinProcessWorkDir returns the assignment-isolated worker directory. A
// configured work_dir is a root, not a directory shared by concurrent workers.
func (p *DarwinProcessProvider) DarwinProcessWorkDir(identity AssignmentIdentity) string {
	stateDir := p.DarwinProcessStateDir(identity)
	workRoot := p.workDir
	if workRoot == "" {
		return filepath.Join(stateDir, "work")
	}
	return filepath.Join(workRoot, filepath.Base(stateDir))
}

func (p *DarwinProcessProvider) Health(ctx context.Context, profile Profile) error {
	if err := darwinSupported(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	executable := strings.TrimSpace(profile.ProviderOptions["executable"])
	if executable == "" {
		executable = p.executable
	}
	if _, err := executablePath(executable); err != nil {
		return fmt.Errorf("resolve assignment worker executable: %w", err)
	}
	for _, root := range []string{p.stateDir, p.workDir} {
		if root == "" {
			continue
		}
		if err := ensurePrivateProcessRoot(root); err != nil {
			return fmt.Errorf("secure Darwin provider directory %s: %w", root, err)
		}
	}
	return nil
}

func (p *DarwinProcessProvider) Launch(ctx context.Context, request LaunchRequest) error {
	if err := darwinSupported(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateProcessRoot(p.darwinProcessStateRoot(request.Identity)); err != nil {
		return Permanent(fmt.Errorf("secure assignment process root: %w", err))
	}
	dir := p.DarwinProcessStateDir(request.Identity)
	if err := requireProcessStateDirectory(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Permanent(err)
	}
	statusPath := filepath.Join(dir, "status.json")
	pidPath := filepath.Join(dir, "worker.pid")
	identityPath := filepath.Join(dir, "process.json")
	configPath := filepath.Join(dir, "worker.yaml")
	status, hasStatus := readProcessStatus(statusPath)
	if hasStatus {
		if _, err := requireProcessIdentity(identityPath, request.Identity); err != nil {
			return Permanent(fmt.Errorf("refuse assignment state without matching ownership: %w", err))
		}
	}
	if hasStatus && (status.State == ProviderSucceeded || status.State == ProviderFailed) {
		return nil
	}
	if pid, err := readPID(pidPath); err == nil {
		alive, owned := assignmentProcessIdentity(pid, identityPath, request.Identity)
		if alive && owned {
			return nil
		}
		if alive {
			return Permanent(fmt.Errorf("recorded pid %d is not the assignment worker", pid))
		}
		return Permanent(fmt.Errorf("recorded worker process %d exited without durable status", pid))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Permanent(fmt.Errorf("read worker pid: %w", err))
	} else if hasStatus && status.State == ProviderPending {
		// The child cannot exec until its PID is durable. Adopt a launcher left
		// by a crash between spawn and PID persistence; if none exists, the
		// marker is stale and this call may safely retry the launch.
		if _, found, err := recoverMarkedProcess(identityPath, pidPath, configPath, request.Identity); err != nil {
			return err
		} else if found {
			return nil
		}
		if err := os.Remove(statusPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale assignment launch marker: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create assignment process dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure assignment process dir: %w", err)
	}
	workDir := p.DarwinProcessWorkDir(request.Identity)
	if p.workDir != "" {
		if err := ensurePrivateProcessRoot(p.workDir); err != nil {
			return Permanent(fmt.Errorf("secure assignment work root: %w", err))
		}
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create worker work dir: %w", err)
	}
	config, err := generatedWorkerYAML(request, workDir)
	if err != nil {
		return Permanent(fmt.Errorf("generate worker config: %w", err))
	}
	if err := writePrivateFile(configPath, config); err != nil {
		return fmt.Errorf("write private worker config: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open worker log: %w", err)
	}
	if err := logFile.Chmod(0o600); err != nil {
		logFile.Close()
		return fmt.Errorf("secure worker log: %w", err)
	}
	executable := strings.TrimSpace(request.Profile.ProviderOptions["executable"])
	if executable == "" {
		executable = p.executable
	}
	workerArgs := append([]string(nil), p.workerArgs...)
	if raw := strings.TrimSpace(request.Profile.ProviderOptions["worker_args"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &workerArgs); err != nil {
			logFile.Close()
			return Permanent(fmt.Errorf("provider option worker_args must be a JSON string array: %w", err))
		}
	}
	if err := validateWorkerArgs(workerArgs); err != nil {
		logFile.Close()
		return Permanent(err)
	}
	args := []string{"run", "--one-shot", "--config", configPath, "--no-metrics"}
	args = append(args, workerArgs...)
	executable, err = executablePath(executable)
	if err != nil {
		logFile.Close()
		return Permanent(fmt.Errorf("resolve assignment worker executable: %w", err))
	}
	processIdentity := processIdentity{
		AssignmentID: request.Identity.AssignmentID, IdentityHash: identityHash(request.Identity),
		ConfigPath: configPath, Executable: executable,
	}
	if err := writeProcessIdentity(identityPath, processIdentity); err != nil {
		logFile.Close()
		return fmt.Errorf("persist assignment process identity: %w", err)
	}
	if err := writeProcessStatus(statusPath, processStatus{State: ProviderPending}); err != nil {
		logFile.Close()
		return fmt.Errorf("persist assignment launch marker: %w", err)
	}
	launcherArgs := []string{"-c", darwinWorkerLaunchScript, "flow-orchestrator-launch", pidPath, executable}
	launcherArgs = append(launcherArgs, args...)
	cmd := exec.Command("/bin/sh", launcherArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = workerEnvironment(request.WorkerToken)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(statusPath)
		logFile.Close()
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return Permanent(fmt.Errorf("start assignment worker: %w", err))
		}
		return fmt.Errorf("start assignment worker: %w", err)
	}
	processIdentity.PID = cmd.Process.Pid
	processIdentity.StartedAt, err = processStartIdentity(cmd.Process.Pid)
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = os.Remove(statusPath)
		logFile.Close()
		return fmt.Errorf("inspect assignment launcher identity: %w", err)
	}
	if err := writeProcessIdentity(identityPath, processIdentity); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = os.Remove(statusPath)
		logFile.Close()
		return fmt.Errorf("persist assignment process identity: %w", err)
	}
	if err := writePrivateFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n")); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = os.Remove(statusPath)
		logFile.Close()
		return fmt.Errorf("persist assignment worker pid: %w", err)
	}
	go p.wait(cmd, logFile, dir, request.Identity)
	return nil
}

func (p *DarwinProcessProvider) Inspect(ctx context.Context, identity AssignmentIdentity) (ProviderStatus, error) {
	if err := darwinSupported(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := p.DarwinProcessStateDir(identity)
	if err := requireProcessStateDirectory(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProviderNotFound, nil
		}
		return "", Permanent(err)
	}
	statusPath := filepath.Join(dir, "status.json")
	pidPath := filepath.Join(dir, "worker.pid")
	identityPath := filepath.Join(dir, "process.json")
	configPath := filepath.Join(dir, "worker.yaml")
	if _, err := requireProcessIdentity(identityPath, identity); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, statusErr := os.Lstat(statusPath); errors.Is(statusErr, os.ErrNotExist) {
				if _, pidErr := os.Lstat(pidPath); errors.Is(pidErr, os.ErrNotExist) {
					return ProviderNotFound, nil
				}
			}
		}
		return "", Permanent(fmt.Errorf("refuse assignment state without matching ownership: %w", err))
	}
	if status, ok := readProcessStatus(statusPath); ok && (status.State == ProviderSucceeded || status.State == ProviderFailed) {
		return status.State, nil
	}
	pid, err := readPID(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		if status, ok := readProcessStatus(statusPath); ok && status.State == ProviderPending {
			if _, found, recoverErr := recoverMarkedProcess(identityPath, pidPath, configPath, identity); recoverErr != nil {
				return "", recoverErr
			} else if found {
				return ProviderRunning, nil
			}
			if removeErr := os.Remove(statusPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return "", fmt.Errorf("remove stale assignment launch marker: %w", removeErr)
			}
		}
		return ProviderNotFound, nil
	}
	if err != nil {
		return "", Permanent(fmt.Errorf("read worker pid: %w", err))
	}
	alive, owned := assignmentProcessIdentity(pid, identityPath, identity)
	if alive && owned {
		return ProviderRunning, nil
	}
	detail := "worker exited without status"
	if alive {
		detail = "recorded pid no longer belongs to assignment worker"
	}
	_ = writeProcessStatus(statusPath, processStatus{State: ProviderFailed, Error: detail})
	return ProviderFailed, nil
}

func (p *DarwinProcessProvider) Delete(ctx context.Context, identity AssignmentIdentity) error {
	if err := darwinSupported(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	dir := p.DarwinProcessStateDir(identity)
	if err := requireProcessStateDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return Permanent(err)
	}
	identityPath := filepath.Join(dir, "process.json")
	if _, err := requireProcessIdentity(identityPath, identity); err != nil {
		return Permanent(fmt.Errorf("refuse to delete assignment state without matching ownership: %w", err))
	}
	status, hasStatus := readProcessStatus(filepath.Join(dir, "status.json"))
	terminal := hasStatus && (status.State == ProviderSucceeded || status.State == ProviderFailed)
	pidPath := filepath.Join(dir, "worker.pid")
	configPath := filepath.Join(dir, "worker.yaml")
	pid, err := readPID(pidPath)
	if errors.Is(err, os.ErrNotExist) && terminal {
		return p.removeProcessResources(identity)
	}
	if errors.Is(err, os.ErrNotExist) {
		if _, identityErr := os.Stat(identityPath); errors.Is(identityErr, os.ErrNotExist) {
			return p.removeProcessResources(identity)
		} else if identityErr != nil {
			return fmt.Errorf("inspect assignment process identity: %w", identityErr)
		}
		var found bool
		pid, found, err = recoverMarkedProcess(identityPath, pidPath, configPath, identity)
		if err != nil {
			return err
		}
		if !found {
			return p.removeProcessResources(identity)
		}
	}
	if err != nil {
		return Permanent(fmt.Errorf("read worker pid for deletion: %w", err))
	}
	alive, owned := assignmentProcessIdentity(pid, identityPath, identity)
	if alive && (owned || trustedProcessGroupIdentity(pid, identityPath, identity)) {
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("terminate worker process group: %w", err)
		}
		exited, err := p.waitForProcessGroupExit(ctx, pid)
		if err != nil {
			return err
		}
		if !exited {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill worker process group: %w", err)
			}
			exited, err = p.waitForProcessGroupExit(ctx, pid)
			if err != nil {
				return err
			}
			if !exited {
				return fmt.Errorf("worker process group %d remains alive after SIGKILL", pid)
			}
		}
	}
	return p.removeProcessResources(identity)
}

func (p *DarwinProcessProvider) waitForProcessGroupExit(ctx context.Context, pid int) (bool, error) {
	deadline := time.NewTimer(p.termTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for processGroupAlive(pid) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
	return true, nil
}

func (p *DarwinProcessProvider) removeProcessResources(identity AssignmentIdentity) error {
	stateDir := p.DarwinProcessStateDir(identity)
	if err := requireProcessStateDirectory(stateDir); err != nil {
		return Permanent(err)
	}
	if _, err := requireProcessIdentity(filepath.Join(stateDir, "process.json"), identity); err != nil {
		return Permanent(fmt.Errorf("refuse to remove assignment state without matching ownership: %w", err))
	}
	if status, ok := readProcessStatus(filepath.Join(stateDir, "status.json")); ok && status.State == ProviderFailed {
		// Preserve failure logs, work products, and the non-secret ownership marker
		// for idempotent cleanup retries, while deleting credential and PID state.
		for _, name := range []string{"worker.yaml", "worker.yaml.tmp", "worker.pid", "worker.pid.tmp", "process.json.tmp"} {
			if err := os.Remove(filepath.Join(stateDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove failed assignment private state %s: %w", name, err)
			}
		}
		return nil
	}
	workDir := p.DarwinProcessWorkDir(identity)
	var workRoot *os.Root
	var workName string
	if workDir != stateDir && !pathWithin(workDir, stateDir) {
		var err error
		workRoot, workName, err = openProcessWorkRoot(p.workDir, workDir)
		if err != nil {
			return Permanent(fmt.Errorf("refuse assignment work cleanup: %w", err))
		}
		if workRoot != nil {
			defer workRoot.Close()
		}
	}
	if err := removeProcessDir(stateDir); err != nil {
		return err
	}
	// The default work directory is nested below stateDir and is already gone.
	// A configured work root is pinned before state mutation, so substitution of
	// that root cannot redirect recursive deletion.
	if workRoot != nil {
		if err := workRoot.RemoveAll(workName); err != nil {
			return fmt.Errorf("remove assignment worker directory: %w", err)
		}
	}
	return nil
}

func openProcessWorkRoot(rootPath, workDir string) (*os.Root, string, error) {
	rootInfo, err := os.Lstat(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := requireOwnedProcessDirectory(rootPath, rootInfo, true); err != nil {
		return nil, "", err
	}
	if err := requireCanonicalProcessPath(rootPath); err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(rootPath, workDir)
	if err != nil || relative == "." || filepath.Base(relative) != relative {
		return nil, "", fmt.Errorf("assignment work directory %s is not a direct child of configured root %s", workDir, rootPath)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", err
	}
	pinnedInfo, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, "", err
	}
	if !os.SameFile(rootInfo, pinnedInfo) {
		root.Close()
		return nil, "", errors.New("configured assignment work root changed during ownership validation")
	}
	assignmentInfo, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return root, relative, nil
	}
	if err != nil {
		root.Close()
		return nil, "", err
	}
	if err := requireOwnedProcessDirectory(workDir, assignmentInfo, true); err != nil {
		root.Close()
		return nil, "", err
	}
	return root, relative, nil
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (p *DarwinProcessProvider) wait(cmd *exec.Cmd, logFile *os.File, dir string, identity AssignmentIdentity) {
	err := cmd.Wait()
	_ = logFile.Close()
	status := processStatus{State: ProviderSucceeded}
	if err != nil {
		status.State = ProviderFailed
		status.Error = err.Error()
	}
	if cmd.ProcessState != nil {
		status.ExitCode = cmd.ProcessState.ExitCode()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if requireProcessStateDirectory(dir) != nil {
		return
	}
	persisted, identityErr := requireProcessIdentity(filepath.Join(dir, "process.json"), identity)
	if identityErr == nil && persisted.PID == cmd.Process.Pid {
		_ = writeProcessStatus(filepath.Join(dir, "status.json"), status)
	}
}

type processStatus struct {
	State    ProviderStatus `json:"state"`
	ExitCode int            `json:"exit_code,omitempty"`
	Error    string         `json:"error,omitempty"`
}

func readProcessStatus(path string) (processStatus, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return processStatus{}, false
	}
	var status processStatus
	if json.Unmarshal(data, &status) != nil {
		return processStatus{}, false
	}
	return status, true
}

func writeProcessStatus(path string, status processStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'))
}

func writePrivateFile(path string, data []byte) error {
	// A random, exclusively created temporary file cannot be redirected through
	// a pre-planted <name>.tmp symlink. Rename then atomically replaces the final
	// directory entry rather than following a symlink at the destination.
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, errors.New("invalid worker pid file")
	}
	return pid, nil
}

const darwinWorkerLaunchScript = `set -eu
pid_path=$1
shift
while [ ! -s "$pid_path" ]; do
	sleep 0.01
done
exec "$@"
`

type processIdentity struct {
	PID          int    `json:"pid,omitempty"`
	AssignmentID string `json:"assignment_id"`
	IdentityHash string `json:"identity_hash"`
	ConfigPath   string `json:"config_path"`
	Executable   string `json:"executable"`
	StartedAt    string `json:"started_at,omitempty"`
}

func executablePath(value string) (string, error) {
	path, err := exec.LookPath(value)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func writeProcessIdentity(path string, identity processIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'))
}

func readProcessIdentity(path string) (processIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return processIdentity{}, err
	}
	var identity processIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return processIdentity{}, err
	}
	if identity.AssignmentID == "" || identity.IdentityHash == "" || identity.ConfigPath == "" || identity.Executable == "" {
		return processIdentity{}, errors.New("incomplete assignment process identity")
	}
	return identity, nil
}

func canonicalProcessRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("configured root %s is a symlink", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func ensurePrivateProcessRoot(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if err := requireOwnedProcessDirectory(path, info, false); err != nil {
		return err
	}
	if err := requireCanonicalProcessPath(path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	return requireOwnedProcessDirectory(path, info, true)
}

func requireCanonicalProcessPath(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(path) {
		return fmt.Errorf("assignment process path %s traverses a symlink", path)
	}
	return nil
}

func requireProcessStateDirectory(path string) error {
	root := filepath.Dir(path)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if err := requireOwnedProcessDirectory(root, rootInfo, true); err != nil {
		return err
	}
	if err := requireCanonicalProcessPath(root); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := requireOwnedProcessDirectory(path, info, true); err != nil {
		return err
	}
	return requireCanonicalProcessPath(path)
}

func requireOwnedProcessDirectory(path string, info os.FileInfo, private bool) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("assignment process path %s is not an owned directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("assignment process directory %s is not owned by the orchestrator user", path)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("assignment process directory %s must not grant group or other access", path)
	}
	return nil
}

func requireProcessIdentity(path string, identity AssignmentIdentity) (processIdentity, error) {
	persisted, err := readProcessIdentity(path)
	if err != nil {
		return processIdentity{}, err
	}
	expectedConfig := filepath.Join(filepath.Dir(path), "worker.yaml")
	if persisted.AssignmentID != identity.AssignmentID || persisted.IdentityHash != identityHash(identity) || persisted.ConfigPath != expectedConfig {
		return processIdentity{}, errors.New("assignment process identity does not match the requested assignment")
	}
	return persisted, nil
}

func processIdentitySnapshot(pid int) (startedAt, command string, err error) {
	output, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "pgid=", "-o", "lstart=", "-o", "command=").Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 7 {
		return "", "", errors.New("incomplete process identity from ps")
	}
	pgid, err := strconv.Atoi(fields[0])
	if err != nil || pgid != pid {
		return "", "", errors.New("assignment process is not its process-group leader")
	}
	return strings.Join(fields[1:6], " "), strings.Join(fields[6:], " "), nil
}

func processStartIdentity(pid int) (string, error) {
	startedAt, _, err := processIdentitySnapshot(pid)
	return startedAt, err
}

func recoverMarkedProcess(identityPath, pidPath, configPath string, identity AssignmentIdentity) (int, bool, error) {
	persisted, err := requireProcessIdentity(identityPath, identity)
	if err != nil {
		return 0, false, Permanent(fmt.Errorf("read assignment process identity: %w", err))
	}
	output, err := exec.Command("/bin/ps", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		return 0, false, fmt.Errorf("inspect assignment launcher: %w", err)
	}
	var matched int
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.Contains(line, configPath) || !strings.Contains(line, persisted.Executable) {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		pgid, pgidErr := strconv.Atoi(fields[1])
		if pidErr != nil || pgidErr != nil || pid <= 1 || pgid != pid {
			continue
		}
		if matched != 0 && matched != pid {
			return 0, false, Permanent(errors.New("multiple processes match the assignment launch marker"))
		}
		matched = pid
	}
	if matched == 0 {
		return 0, false, nil
	}
	persisted.PID = matched
	persisted.StartedAt, err = processStartIdentity(matched)
	if err != nil {
		return 0, false, fmt.Errorf("inspect recovered assignment launcher identity: %w", err)
	}
	if err := writeProcessIdentity(identityPath, persisted); err != nil {
		return 0, false, fmt.Errorf("persist recovered assignment process identity: %w", err)
	}
	if err := writePrivateFile(pidPath, []byte(strconv.Itoa(matched)+"\n")); err != nil {
		return 0, false, fmt.Errorf("persist recovered assignment worker pid: %w", err)
	}
	return matched, true, nil
}

func assignmentProcessIdentity(pid int, identityPath string, identity AssignmentIdentity) (alive, owned bool) {
	if !processGroupAlive(pid) {
		return false, false
	}
	persisted, err := requireProcessIdentity(identityPath, identity)
	if err != nil || persisted.PID != pid {
		return true, false
	}
	startedAt, command, err := processIdentitySnapshot(pid)
	if err != nil {
		return true, false
	}
	return true, startedAt == persisted.StartedAt && strings.Contains(command, persisted.ConfigPath) && strings.Contains(command, persisted.Executable)
}

func trustedProcessGroupIdentity(pid int, identityPath string, identity AssignmentIdentity) bool {
	persisted, err := requireProcessIdentity(identityPath, identity)
	if err != nil || persisted.PID != pid || persisted.StartedAt == "" {
		return false
	}
	// If the leader still exists, require its immutable start identity. If it has
	// exited but descendants retain the original process group, the durable
	// terminal status and identity are sufficient to terminate that orphaned group.
	startedAt, command, err := processIdentitySnapshot(pid)
	if err != nil {
		return errors.Is(syscall.Kill(pid, syscall.Signal(0)), syscall.ESRCH)
	}
	return startedAt == persisted.StartedAt && strings.Contains(command, persisted.ConfigPath) && strings.Contains(command, persisted.Executable)
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func removeProcessDir(path string) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := os.RemoveAll(path)
		if err == nil {
			return nil
		}
		// A waiter inherited from a just-restarted provider instance can finish
		// while cleanup is removing the directory and briefly recreate its
		// status temporary. Retry only that bounded directory-not-empty race.
		if !errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf("remove assignment process state: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove assignment process state: %w", err)
	}
	return nil
}

func workerEnvironment(token string) []string {
	result := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "FLOW_WORKER_") || upper == "FLOW_ORCHESTRATOR_TOKEN" ||
			upper == "FLOW_OWNER_TOKEN" || upper == "FLOW_SESSION_TOKEN" ||
			(token != "" && strings.Contains(value, token)) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func darwinSupported() error {
	if runtime.GOOS != "darwin" {
		return ErrDarwinProcessUnsupported
	}
	return nil
}
