package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinProcessProviderLaunchRestartInspectAndDelete(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	helper := filepath.Join(root, "flow-worker-helper")
	script := `#!/bin/sh
set -eu
dir=$(dirname "$2")
printf '%s\n' "$@" > "$dir/observed-args"
env > "$dir/observed-env"
trap 'exit 0' TERM INT
while :; do sleep 1; done
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOW_WORKER_TOKEN", "private-direct-token")
	t.Setenv("FLOW_WORKER_ID", "wrong-inherited-worker")
	t.Setenv("FLOW_WORKER_COORDINATOR_URL", "https://credential-thief.example")
	t.Setenv("FLOW_WORKER_WORK_DIR", "/wrong-inherited-work-dir")
	t.Setenv("FLOW_WORKER_ACCEPTS", "persistent-agent")
	t.Setenv("FLOW_WORKER_CAPACITY_EPHEMERAL", "99")
	t.Setenv("FLOW_ORCHESTRATOR_TOKEN", "private-orchestrator-token")
	t.Setenv("FLOW_OWNER_TOKEN", "private-owner-token")
	options := DarwinProcessProviderOptions{
		StateDir: filepath.Join(root, "state"), WorkDir: filepath.Join(root, "work"), Executable: helper,
		TermTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	}
	provider, err := NewDarwinProcessProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("pending")
	assignment.Assignment.ProviderOptions["state_dir"] = options.StateDir
	assignment.Assignment.ProviderOptions["work_dir"] = options.WorkDir
	identity := IdentityOf(assignment)
	request := LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: testProfile(),
		CoordinatorURL: "https://coordinator.example", WorkerToken: "private-direct-token",
	}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	dir := provider.DarwinProcessStateDir(identity)
	waitForFile(t, filepath.Join(dir, "observed-args"))
	pidBefore, err := os.ReadFile(filepath.Join(dir, "worker.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatalf("idempotent Launch() error = %v", err)
	}
	pidAfter, _ := os.ReadFile(filepath.Join(dir, "worker.pid"))
	if string(pidBefore) != string(pidAfter) {
		t.Fatalf("repeat launch replaced process: %q -> %q", pidBefore, pidAfter)
	}
	persistedIdentity, err := readProcessIdentity(filepath.Join(dir, "process.json"))
	if err != nil {
		t.Fatal(err)
	}
	if persistedIdentity.AssignmentID != identity.AssignmentID || persistedIdentity.IdentityHash != identityHash(identity) || persistedIdentity.PID <= 1 || persistedIdentity.StartedAt == "" || persistedIdentity.Executable == "" {
		t.Fatalf("incomplete durable process identity: %+v", persistedIdentity)
	}
	tamperedPath := filepath.Join(root, "tampered-process.json")
	persistedIdentity.StartedAt = "Thu Jan 1 00:00:00 1970"
	if err := writeProcessIdentity(tamperedPath, persistedIdentity); err != nil {
		t.Fatal(err)
	}
	if alive, owned := assignmentProcessIdentity(persistedIdentity.PID, tamperedPath, identity); !alive || owned {
		t.Fatalf("tampered process identity accepted: alive=%t owned=%t", alive, owned)
	}
	configPath := filepath.Join(dir, "worker.yaml")
	workDir := provider.DarwinProcessWorkDir(identity)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "token: private-direct-token") || !strings.Contains(string(config), "work_dir: "+workDir) {
		t.Fatalf("worker config does not hold direct token and isolated work dir:\n%s", config)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("worker config mode = %o, want 600", info.Mode().Perm())
	}
	args, _ := os.ReadFile(filepath.Join(dir, "observed-args"))
	wantArgs := "--config\n" + configPath + "\n--one-shot\n--no-metrics\n"
	if string(args) != wantArgs || strings.Contains(string(args), "private-direct-token") {
		t.Fatalf("child args = %q, want %q", args, wantArgs)
	}
	environment, _ := os.ReadFile(filepath.Join(dir, "observed-env"))
	for _, secret := range []string{"private-direct-token", "private-orchestrator-token", "private-owner-token", "FLOW_WORKER_", "wrong-inherited-worker", "credential-thief.example", "wrong-inherited-work-dir", "FLOW_ORCHESTRATOR_TOKEN", "FLOW_OWNER_TOKEN"} {
		if strings.Contains(string(environment), secret) {
			t.Fatalf("coordinator credential %q leaked to child environment:\n%s", secret, environment)
		}
	}

	// A newly constructed provider recovers the process exclusively from stable files.
	restarted, err := NewDarwinProcessProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	status, err := restarted.Inspect(context.Background(), identity)
	if err != nil || status != ProviderRunning {
		t.Fatalf("restart Inspect() = %q, %v", status, err)
	}
	if err := restarted.Delete(context.Background(), identity); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir still exists after deletion: %v", err)
	}
	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work dir still exists after deletion: %v", err)
	}
	// Allow the original provider's waiter to finish; it must not recreate state.
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("waiter recreated deleted state dir: %v", err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestDarwinProcessProviderRetriesStaleLaunchMarker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	helper := filepath.Join(root, "flow-worker-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: root, Executable: helper})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("pending")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	dir := provider.DarwinProcessStateDir(identity)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := executablePath(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeProcessIdentity(filepath.Join(dir, "process.json"), processIdentity{
		AssignmentID: identity.AssignmentID, IdentityHash: identityHash(identity),
		ConfigPath: filepath.Join(dir, "worker.yaml"), Executable: executable,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessStatus(filepath.Join(dir, "status.json"), processStatus{State: ProviderPending}); err != nil {
		t.Fatal(err)
	}
	status, err := provider.Inspect(context.Background(), identity)
	if err != nil || status != ProviderNotFound {
		t.Fatalf("Inspect() = %q, %v, want stale marker to be retryable", status, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale marker still exists: %v", err)
	}
	request := LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: testProfile(),
		CoordinatorURL: "https://coordinator.example", WorkerToken: "private-direct-token",
	}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatalf("Launch() after stale marker = %v", err)
	}
	waitForFile(t, filepath.Join(dir, "worker.pid"))
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinProcessProviderDeletesTerminalDescendantGroup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	helper := filepath.Join(root, "flow-worker-helper")
	script := `#!/bin/sh
set -eu
sleep 300 &
printf '%s\n' "$!" > "$(dirname "$2")/child.pid"
exit 0
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{
		StateDir: root, Executable: helper, TermTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("pending")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	request := LaunchRequest{Identity: identity, Assignment: assignment, Profile: testProfile(), CoordinatorURL: "https://coordinator.example", WorkerToken: "token"}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	dir := provider.DarwinProcessStateDir(identity)
	childPath := filepath.Join(dir, "child.pid")
	waitForFile(t, childPath)
	childPID, err := readPID(childPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, ok := readProcessStatus(filepath.Join(dir, "status.json")); ok && (status.State == ProviderSucceeded || status.State == ProviderFailed) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := writeProcessStatus(filepath.Join(dir, "status.json"), processStatus{State: ProviderPending}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{
		StateDir: root, Executable: helper, TermTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Delete(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if errors.Is(syscall.Kill(childPID, syscall.Signal(0)), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal assignment descendant %d survived cleanup", childPID)
}

func TestDarwinProcessProviderWaitsForSIGKILLCleanup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	helper := filepath.Join(root, "flow-worker-helper")
	script := `#!/bin/sh
set -eu
trap '' TERM INT
while :; do sleep 1; done
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{
		StateDir: root, Executable: helper, TermTimeout: 25 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("pending")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	request := LaunchRequest{Identity: identity, Assignment: assignment, Profile: testProfile(), CoordinatorURL: "https://coordinator.example", WorkerToken: "token"}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(provider.DarwinProcessStateDir(identity), "worker.pid")
	waitForFile(t, pidPath)
	pid, err := readPID(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if processGroupAlive(pid) {
		t.Fatalf("Delete() returned while SIGKILLed process group %d remained alive", pid)
	}
}

func TestDarwinProcessProviderRetainsFailedDiagnosticsWithoutCredential(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	stateRoot := filepath.Join(root, "state")
	workRoot := filepath.Join(root, "work")
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: stateRoot, WorkDir: workRoot})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	assignment.Assignment.ProviderOptions["state_dir"] = stateRoot
	assignment.Assignment.ProviderOptions["work_dir"] = workRoot
	identity := IdentityOf(assignment)
	stateDir := provider.DarwinProcessStateDir(identity)
	workDir := provider.DarwinProcessWorkDir(identity)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessStatus(filepath.Join(stateDir, "status.json"), processStatus{State: ProviderFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessIdentity(filepath.Join(stateDir, "process.json"), processIdentity{
		AssignmentID: identity.AssignmentID, IdentityHash: identityHash(identity),
		ConfigPath: filepath.Join(stateDir, "worker.yaml"), Executable: "/bin/sh",
	}); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(stateDir, "worker.yaml"):     "token: private-direct-token\n",
		filepath.Join(stateDir, "worker.yaml.tmp"): "token: private-direct-token\n",
		filepath.Join(stateDir, "worker.log"):      "failure details\n",
		filepath.Join(workDir, "result.txt"):       "diagnostic work product\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatalf("repeated Delete() after private-state removal: %v", err)
	}
	for _, path := range []string{filepath.Join(stateDir, "status.json"), filepath.Join(stateDir, "worker.log"), filepath.Join(workDir, "result.txt")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained diagnostic %s: %v", path, err)
		}
	}
	for _, name := range []string{"worker.yaml", "worker.yaml.tmp", "worker.pid"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private state %s was retained: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "process.json")); err != nil {
		t.Fatalf("ownership marker was not retained for cleanup retries: %v", err)
	}
}

func TestDarwinProcessProviderRejectsSubstitutedWorkRootBeforeStateMutation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	stateRoot := filepath.Join(root, "state")
	workRoot := filepath.Join(root, "work")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: stateRoot, WorkDir: workRoot})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	assignment.Assignment.ProviderOptions["state_dir"] = stateRoot
	assignment.Assignment.ProviderOptions["work_dir"] = workRoot
	identity := IdentityOf(assignment)
	stateDir := provider.DarwinProcessStateDir(identity)
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessIdentity(filepath.Join(stateDir, "process.json"), processIdentity{
		AssignmentID: identity.AssignmentID, IdentityHash: identityHash(identity),
		ConfigPath: filepath.Join(stateDir, "worker.yaml"), Executable: "/bin/sh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessStatus(filepath.Join(stateDir, "status.json"), processStatus{State: ProviderSucceeded}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "attacker-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	targetAssignment := filepath.Join(target, filepath.Base(provider.DarwinProcessWorkDir(identity)))
	if err := os.Mkdir(targetAssignment, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(targetAssignment, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, workRoot); err != nil {
		t.Fatal(err)
	}

	if err := provider.Delete(context.Background(), identity); err == nil || !IsPermanent(err) {
		t.Fatalf("Delete() with substituted work root = %v, want permanent ownership error", err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("substituted work-root target was mutated: %q, %v", content, err)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("state was mutated before work-root validation: %v", err)
	}
}

func TestDarwinProcessProviderRejectsUnownedAndSymlinkedState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	stateDir := provider.DarwinProcessStateDir(identity)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessStatus(filepath.Join(stateDir, "status.json"), processStatus{State: ProviderFailed, Error: "forged"}); err != nil {
		t.Fatal(err)
	}

	if status, err := provider.Inspect(context.Background(), identity); err == nil || status != "" || !IsPermanent(err) {
		t.Fatalf("Inspect() unowned terminal marker = %q, %v, want permanent ownership error", status, err)
	}
	if err := provider.Delete(context.Background(), identity); err == nil || !IsPermanent(err) {
		t.Fatalf("Delete() unowned terminal marker = %v, want permanent ownership error", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "status.json")); err != nil {
		t.Fatalf("unowned state was mutated: %v", err)
	}

	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "attacker-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Inspect(context.Background(), identity); err == nil || status != "" || !IsPermanent(err) {
		t.Fatalf("Inspect() symlinked state = %q, %v, want permanent ownership error", status, err)
	}
	if err := provider.Delete(context.Background(), identity); err == nil || !IsPermanent(err) {
		t.Fatalf("Delete() symlinked state = %v, want permanent ownership error", err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("symlink target was mutated: %q, %v", content, err)
	}
}

func TestWritePrivateFileDoesNotFollowPredictableTemporarySymlink(t *testing.T) {
	root := privateTempDir(t)
	path := filepath.Join(root, "status.json")
	target := filepath.Join(root, "sentinel")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("status")); err != nil {
		t.Fatalf("writePrivateFile() error = %v", err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "keep" {
		t.Fatalf("temporary symlink target was mutated: %q, %v", content, err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "status" {
		t.Fatalf("private file = %q, %v", content, err)
	}
}

func TestDarwinProcessProviderRejectsSubstitutedStateRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	root := filepath.Join(parent, "state")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	if err := os.Mkdir(provider.DarwinProcessStateDir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Inspect(context.Background(), identity); err == nil || status != "" || !IsPermanent(err) {
		t.Fatalf("Inspect() through substituted state root = %q, %v, want permanent ownership error", status, err)
	}
	if err := provider.Delete(context.Background(), identity); err == nil || !IsPermanent(err) {
		t.Fatalf("Delete() through substituted state root = %v, want permanent ownership error", err)
	}
}

func TestDarwinProcessProviderRejectsSharedStateRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	identity := IdentityOf(assignment)
	if err := os.Mkdir(provider.DarwinProcessStateDir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Inspect(context.Background(), identity); err == nil || status != "" || !IsPermanent(err) {
		t.Fatalf("Inspect() with shared state root = %q, %v, want permanent ownership error", status, err)
	}
}

func TestDarwinProcessProviderRejectsMismatchedIdentityMarker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin process provider test")
	}
	root := privateTempDir(t)
	provider, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	assignment := testAssignment("closed")
	assignment.Assignment.ProviderOptions["state_dir"] = root
	identity := IdentityOf(assignment)
	stateDir := provider.DarwinProcessStateDir(identity)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessIdentity(filepath.Join(stateDir, "process.json"), processIdentity{
		AssignmentID: identity.AssignmentID, IdentityHash: "another-assignment",
		ConfigPath: filepath.Join(stateDir, "worker.yaml"), Executable: "/bin/sh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessStatus(filepath.Join(stateDir, "status.json"), processStatus{State: ProviderPending}); err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Inspect(context.Background(), identity); err == nil || status != "" || !IsPermanent(err) {
		t.Fatalf("Inspect() mismatched marker = %q, %v, want permanent ownership error", status, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "status.json")); err != nil {
		t.Fatalf("mismatched state was mutated: %v", err)
	}
}

func TestDarwinProcessProviderUnsupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-Darwin behavior")
	}
	_, err := NewDarwinProcessProvider(DarwinProcessProviderOptions{StateDir: t.TempDir()})
	if !errors.Is(err, ErrDarwinProcessUnsupported) {
		t.Fatalf("constructor error = %v", err)
	}
}
