package execution

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/terminal"
)

func TestRunJobClonesCheckoutAndRunsArgvEntrypointInTmux(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	workDir := t.TempDir()
	cfg := workerConfigWithTmux(t, workDir, exchangeURL)
	outPath := filepath.Join(t.TempDir(), "argv.out")
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	literalArg := "literal; touch " + sentinel + " 'quoted'"
	writer := writeScript(t, `#!/bin/sh
printf '%s' "$1" > "$2"
`)

	job := Job{
		ID:             "j-argv",
		Role:           RoleCI,
		CapacityBucket: BucketEphemeral,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  []string{writer, literalArg, outPath},
				"cwd":   ".",
				"env":   map[string]string{"WORKER_TEST_ENV": "ok"},
				"shell": false,
			},
		},
	}

	result := RunJob(ctx, RunInput{
		Config: cfg,
		Job:    job,
		Lease:  Lease{ID: "l-argv", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if result.FinalState != JobFinished || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want finished exit 0", result)
	}
	if want := filepath.Join(workDir, "jobs", job.ID, VerdictFileName); result.VerdictFilePath != want {
		t.Fatalf("VerdictFilePath = %q, want %q", result.VerdictFilePath, want)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read entrypoint output: %v", err)
	}
	if string(contents) != literalArg {
		t.Fatalf("entrypoint output = %q", string(contents))
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("argv entrypoint interpreted shell metacharacters")
	}
	if _, err := os.Stat(filepath.Join(result.Worktree, ".git")); err != nil {
		t.Fatalf("worktree was not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.Worktree, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("worktree unexpectedly contains repository skills; stat err = %v", err)
	}
}

func TestRunJobCapturesTmuxTranscript(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	workDir := t.TempDir()
	cfg := workerConfigWithTmux(t, workDir, exchangeURL)

	marker := "transcript-marker-12345"
	// A shell entrypoint that echoes a recognizable marker to the pane.
	job := Job{
		ID:             "j-transcript",
		Role:           RoleCI,
		CapacityBucket: BucketEphemeral,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  []string{"printf '%s\\n' " + marker},
				"cwd":   ".",
				"shell": true,
			},
		},
	}

	result := RunJob(ctx, RunInput{
		Config: cfg,
		Job:    job,
		Lease:  Lease{ID: "l-transcript", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if result.TranscriptPath == "" {
		t.Fatalf("result did not report a transcript path")
	}
	if result.TranscriptPath != filepath.Join(jobDir(workDir, job.ID), "transcript.log") {
		t.Fatalf("transcript path = %q, want job-dir transcript.log", result.TranscriptPath)
	}

	contents, err := os.ReadFile(result.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript log: %v", err)
	}
	if len(contents) == 0 {
		t.Fatalf("transcript log is empty")
	}
	if !strings.Contains(string(contents), marker) {
		t.Fatalf("transcript log = %q, want to contain marker %q", string(contents), marker)
	}
}

func TestRunJobChecksOutPayloadHeadSHA(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	worktree, remote := createSeedGitRemote(t)
	gitRun(t, worktree, "push", remote, "main:main")
	pinnedHead := gitOutput(t, worktree, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("new remote main\n"), 0o644); err != nil {
		t.Fatalf("write updated readme: %v", err)
	}
	gitRun(t, worktree, "add", "README.md")
	gitRun(t, worktree, "commit", "-m", "test: advance main")
	gitRun(t, worktree, "push", remote, "main:main")

	outPath := filepath.Join(t.TempDir(), "readme.out")
	reader := writeScript(t, `#!/bin/sh
cat README.md > "$1"
`)
	job := Job{
		ID:             "j-head-sha",
		Role:           RoleCI,
		CapacityBucket: BucketEphemeral,
		Payload: map[string]any{
			"base":     "main",
			"branch":   "main",
			"head_sha": pinnedHead,
			"entrypoint": map[string]any{
				"argv":  []string{reader, outPath},
				"shell": false,
			},
		},
	}

	result := RunJob(ctx, RunInput{
		Config: workerConfigWithTmux(t, t.TempDir(), "file://"+filepath.ToSlash(remote)),
		Job:    job,
		Lease:  Lease{ID: "l-head-sha", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if got := gitOutput(t, result.Worktree, "rev-parse", "HEAD"); got != pinnedHead {
		t.Fatalf("worktree HEAD = %s, want pinned %s", got, pinnedHead)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(contents) != "seed\n" {
		t.Fatalf("README content = %q, want pinned revision", string(contents))
	}
}

func TestRunJobRunsWhenRepositorySkillsAreMissing(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	exchangeURL := createExchangeRemoteWithoutFlowSkills(t)
	workDir := t.TempDir()

	result := RunJob(ctx, RunInput{
		Config: workerConfig(workDir, exchangeURL),
		Job: Job{
			ID:             "j-missing-skills",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"base":   "main",
				"branch": "main",
				"entrypoint": map[string]any{
					"argv":  []string{"true"},
					"shell": true,
				},
			},
		},
		Lease: Lease{ID: "l-missing-skills", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run job without repository skills: %v", result.Err)
	}
	if result.FinalState != JobFinished || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want finished exit 0", result)
	}
}

func TestRunJobPushesNewIssueBranch(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	workDir := t.TempDir()
	cfg := workerConfigWithTmux(t, workDir, exchangeURL)

	job := Job{
		ID:             "j-branch",
		Role:           RoleAuthor,
		CapacityBucket: BucketPersistentAgent,
		Payload: map[string]any{
			"base":   "main",
			"branch": "issue/i-0001",
			"entrypoint": map[string]any{
				"argv":  []string{"/bin/true"},
				"shell": false,
			},
		},
	}

	result := RunJob(ctx, RunInput{
		Config: cfg,
		Job:    job,
		Lease:  Lease{ID: "l-branch", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run author job: %v", result.Err)
	}
	remotePath := strings.TrimPrefix(exchangeURL, "file://")
	gitRun(t, remotePath, "show-ref", "--verify", "refs/heads/issue/i-0001")
}

func TestRunJobShellEntrypointRequiresExplicitShell(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	workDir := t.TempDir()
	cfg := workerConfigWithTmux(t, workDir, exchangeURL)
	outPath := filepath.Join(t.TempDir(), "shell.out")

	result := RunJob(ctx, ciRunInput(cfg, "j-shell", "l-shell", []string{"printf shell-ok > " + outPath}, true))
	if result.Err != nil {
		t.Fatalf("run shell job: %v", result.Err)
	}
	if result.FinalState != JobFinished {
		t.Fatalf("state = %q, want finished", result.FinalState)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read shell output: %v", err)
	}
	if string(contents) != "shell-ok" {
		t.Fatalf("shell output = %q", string(contents))
	}
}

func TestRunJobCleansTmuxSessionAfterEntrypointExit(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	jobID := "j-cleanup"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	keepaliveSession := "flow-test-keepalive"
	tmuxRun(t, jobCfg, "new-session", "-d", "-s", keepaliveSession, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(jobCfg)
	})
	tmuxRun(t, jobCfg, "set-window-option", "-g", "remain-on-exit", "on")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
exit 0
`)
	result := RunJob(ctx, ciRunInput(cfg, jobID, "l-cleanup", []string{entrypoint}, false))
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if result.FinalState != JobFinished || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want finished exit 0", result)
	}
	if tmuxSessionExists(context.Background(), jobCfg, result.Session) {
		t.Fatalf("tmux session %q still exists after entrypoint exit", result.Session)
	}
	if tmuxSessionExists(context.Background(), jobCfg, keepaliveSession) {
		t.Fatalf("tmux session %q still exists after job tmux cleanup", keepaliveSession)
	}
}

func TestRunJobIgnoresEarlyEntrypointExitFileWrite(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	jobID := "j-early-exit-file"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
printf '0\n' > "$FLOW_WORKER_EXIT_FILE"
sleep 1
exit 7
`)
	result := RunJob(ctx, ciRunInput(cfg, jobID, "l-early-exit-file", []string{entrypoint}, false))
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if result.FinalState != JobFailed || result.ExitCode != 7 {
		t.Fatalf("result = %+v, want failed exit 7", result)
	}
	if tmuxSessionExists(context.Background(), jobCfg, result.Session) {
		t.Fatalf("tmux session %q still exists after entrypoint exit", result.Session)
	}
}

func TestRunJobRejectsForgedExitFileWhenEntrypointKillsTmuxSession(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	jobID := "j-forged-exit-file"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	previousTimeout := exitFileAfterSessionExitTimeout
	exitFileAfterSessionExitTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		exitFileAfterSessionExitTimeout = previousTimeout
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The wrapper strips TMUX from the pane, so killing the hosting session
	// requires targeting the job socket explicitly.
	entrypoint := writeScript(t, `#!/bin/sh
printf '0\n' > "$FLOW_WORKER_EXIT_FILE"
tmux -S `+shellQuote(jobCfg.Tmux.SocketPath)+` kill-session -t "flow-$FLOW_JOB_ID"
exit 7
`)
	result := RunJob(ctx, ciRunInput(cfg, jobID, "l-forged-exit-file", []string{entrypoint}, false))
	if result.FinalState != JobFailed || result.ExitCode == 0 {
		t.Fatalf("result = %+v, want failed without forged exit 0", result)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "was not recorded before tmux session exited") {
		t.Fatalf("err = %v, want missing private worker exit file", result.Err)
	}
	if tmuxSessionExists(context.Background(), jobCfg, result.Session) {
		t.Fatalf("tmux session %q still exists after entrypoint killed it", result.Session)
	}
}

func TestRunJobIsolatesTmuxKillServerToCurrentJob(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	siblingJobID := "j-sibling"
	siblingCfg, err := tmuxConfigForJob(cfg, siblingJobID)
	if err != nil {
		t.Fatalf("sibling tmux config: %v", err)
	}
	siblingSession := sessionNameForJob(siblingJobID)
	tmuxRun(t, siblingCfg, "new-session", "-d", "-s", siblingSession, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(siblingCfg)
	})

	previousTimeout := exitFileAfterSessionExitTimeout
	exitFileAfterSessionExitTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		exitFileAfterSessionExitTimeout = previousTimeout
	})

	ownCfg, err := tmuxConfigForJob(cfg, "j-kill-server")
	if err != nil {
		t.Fatalf("own tmux config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The wrapper strips TMUX from the pane, so killing the hosting server
	// requires targeting the job socket explicitly.
	entrypoint := writeScript(t, `#!/bin/sh
tmux -S `+shellQuote(ownCfg.Tmux.SocketPath)+` kill-server
exit 7
`)
	result := RunJob(ctx, ciRunInput(cfg, "j-kill-server", "l-kill-server", []string{entrypoint}, false))
	if result.FinalState != JobFailed {
		t.Fatalf("result = %+v, want failed job after killing its own tmux server", result)
	}
	if result.Err != nil {
		if !strings.Contains(result.Err.Error(), "was not recorded before tmux session exited") {
			t.Fatalf("err = %v, want missing private worker exit file", result.Err)
		}
	} else if result.ExitCode != 7 {
		t.Fatalf("result = %+v, want failed exit 7 after killing its own tmux server", result)
	}
	if !tmuxSessionExists(context.Background(), siblingCfg, siblingSession) {
		t.Fatalf("sibling tmux session %q was killed by another job", siblingSession)
	}
}

func TestRunJobStripsTmuxClientEnvironmentFromPane(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	jobID := "j-env-isolated"
	wantTmpDir, err := agentTmuxTmpDirForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("agent tmux tmpdir: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "env.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
printf '%s\n%s\n%s\n' "${TMUX:-unset}" "${TMUX_PANE:-unset}" "${TMUX_TMPDIR:-unset}" > `+shellQuote(outPath)+`
exit 0
`)
	result := RunJob(ctx, RunInput{
		Config: cfg,
		Job: Job{
			ID:             jobID,
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"base":   "main",
				"branch": "main",
				"entrypoint": map[string]any{
					"argv":  []string{entrypoint},
					"shell": false,
				},
			},
		},
		Lease: Lease{ID: "l-env-isolated", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read env output: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	want := []string{"unset", "unset", wantTmpDir}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] || lines[2] != want[2] {
		t.Fatalf("pane tmux environment = %q, want %q", lines, want)
	}
}

func TestRunJobScrubsInheritedWorkerDeploymentEnvironment(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	t.Setenv("FLOW_WORKER_COORDINATOR_URL", "http://flow-server:8421")
	t.Setenv("FLOW_WORKER_WORK_DIR", "/home/flow/.local/share/flow/workers/local")
	t.Setenv("FLOW_WORKER_JOIN_TOKEN", "join-token")
	t.Setenv("FLOW_WORKER_TOKEN", "live-worker-token")
	t.Setenv("FLOW_WORKER_CAPACITY_EPHEMERAL", "5")

	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	outPath := filepath.Join(t.TempDir(), "worker-env.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
{
  printf 'coord=%s\n' "${FLOW_WORKER_COORDINATOR_URL:-}"
  printf 'work=%s\n' "${FLOW_WORKER_WORK_DIR:-}"
  printf 'join=%s\n' "${FLOW_WORKER_JOIN_TOKEN:-}"
  printf 'token=%s\n' "${FLOW_WORKER_TOKEN:-}"
  printf 'capacity=%s\n' "${FLOW_WORKER_CAPACITY_EPHEMERAL:-}"
  printf 'flow_coord=%s\n' "${FLOW_COORDINATOR_URL:-}"
} > `+shellQuote(outPath)+`
exit 0
`)
	result := RunJob(ctx, ciRunInput(cfg, "j-worker-env-scrub", "l-worker-env-scrub", []string{entrypoint}, false))
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read worker env output: %v", err)
	}
	got := strings.TrimSpace(string(contents))
	want := strings.Join([]string{
		"coord=",
		"work=",
		"join=",
		"token=",
		"capacity=",
		"flow_coord=" + cfg.CoordinatorURL,
	}, "\n")
	if got != want {
		t.Fatalf("worker deployment env:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRunJobSurvivesSocketlessTmuxKillsFromEntrypoint is the regression for
// author agents running stale checkouts of this repository's own test suite
// inside their pane: socketless tmux kill commands must not be able to reach
// the job's hosting server through inherited client environment.
func TestRunJobSurvivesSocketlessTmuxKillsFromEntrypoint(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
tmux kill-server >/dev/null 2>&1
tmux kill-session -t "flow-$FLOW_JOB_ID" >/dev/null 2>&1
exit 7
`)
	result := RunJob(ctx, ciRunInput(cfg, "j-socketless-kill", "l-socketless-kill", []string{entrypoint}, false))
	if result.Err != nil {
		t.Fatalf("run job: %v (session was killed through inherited tmux environment)", result.Err)
	}
	if result.FinalState != JobFailed || result.ExitCode != 7 {
		t.Fatalf("result = %+v, want failed exit 7", result)
	}
}

func TestConfigureTmuxForJobEnablesMouseScrollback(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), "file:///tmp/exchange.git")
	sessionName := "flow-test-options"
	tmuxRun(t, cfg, "new-session", "-d", "-s", sessionName, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(cfg)
	})

	if err := configureTmuxForJob(context.Background(), cfg, sessionName); err != nil {
		t.Fatalf("configureTmuxForJob: %v", err)
	}
	mouse, err := tmuxCommand(cfg, "show-options", "-t", sessionName, "-qv", "mouse").Output()
	if err != nil {
		t.Fatalf("show tmux mouse option: %v", err)
	}
	if strings.TrimSpace(string(mouse)) != "on" {
		t.Fatalf("tmux mouse = %q, want on", strings.TrimSpace(string(mouse)))
	}
	historyLimit, err := tmuxCommand(cfg, "show-options", "-t", sessionName, "-qv", "history-limit").Output()
	if err != nil {
		t.Fatalf("show tmux history-limit option: %v", err)
	}
	if strings.TrimSpace(string(historyLimit)) != "100000" {
		t.Fatalf("tmux history-limit = %q, want 100000", strings.TrimSpace(string(historyLimit)))
	}
	setClipboard, err := tmuxCommand(cfg, "show-options", "-t", sessionName, "-qv", "set-clipboard").Output()
	if err != nil {
		t.Fatalf("show tmux set-clipboard option: %v", err)
	}
	if strings.TrimSpace(string(setClipboard)) != "on" {
		t.Fatalf("tmux set-clipboard = %q, want on", strings.TrimSpace(string(setClipboard)))
	}
	// terminal-features is a list option; confirm the clipboard capability is
	// advertised so tmux emits OSC 52 to the outer terminal.
	terminalFeatures, err := tmuxCommand(cfg, "show-options", "-t", sessionName, "-v", "terminal-features").Output()
	if err != nil {
		t.Fatalf("show tmux terminal-features option: %v", err)
	}
	if !strings.Contains(string(terminalFeatures), "*:clipboard") {
		t.Fatalf("tmux terminal-features = %q, want to contain *:clipboard", strings.TrimSpace(string(terminalFeatures)))
	}
}

func TestRunJobReclaimsStaleTmuxSessionBeforeLaunch(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	jobID := "j-stale"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	staleSession := sessionNameForJob(jobID)
	tmuxRun(t, jobCfg, "new-session", "-d", "-s", staleSession, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(jobCfg)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entrypoint := writeScript(t, `#!/bin/sh
exit 0
`)
	result := RunJob(ctx, ciRunInput(cfg, jobID, "l-stale", []string{entrypoint}, false))
	if result.Err != nil {
		t.Fatalf("run job: %v", result.Err)
	}
	if result.FinalState != JobFinished || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want finished exit 0", result)
	}
}

func TestWaitForTmuxWaitsForExitFileAfterSessionVanishes(t *testing.T) {
	t.Parallel()
	exitFile := filepath.Join(t.TempDir(), "exit-code")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- waitForTmux(ctx, config.WorkerConfig{}, "flow-missing-session", exitFile, nil, nil, nil)
	}()
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(exitFile, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write exit file: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForTmux returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForTmux did not observe delayed exit file")
	}
}

func TestRunJobFailsMissingEntrypoint(t *testing.T) {
	result := RunJob(context.Background(), RunInput{
		Config: workerConfig(t.TempDir(), "file:///tmp/missing.git"),
		Job: Job{
			ID:             "j-missing",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload:        map[string]any{},
		},
		Lease: Lease{ID: "l-missing", WorkerID: "w-local"},
	})
	if result.FinalState != JobFailed {
		t.Fatalf("state = %q, want failed", result.FinalState)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "entrypoint is required") {
		t.Fatalf("err = %v, want missing entrypoint", result.Err)
	}
}

func TestRunJobRejectsReservedFlowEnvOverride(t *testing.T) {
	result := RunJob(context.Background(), RunInput{
		Config: workerConfig(t.TempDir(), "file:///tmp/missing.git"),
		Job: Job{
			ID:             "j-env",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"entrypoint": map[string]any{
					"argv": []string{"/bin/true"},
					"env": map[string]string{
						"FLOW_WORKER_EXIT_FILE": "/tmp/elsewhere",
					},
				},
			},
		},
		Lease: Lease{ID: "l-env", WorkerID: "w-local"},
	})
	if result.FinalState != JobFailed {
		t.Fatalf("state = %q, want failed", result.FinalState)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "reserved FLOW_*") {
		t.Fatalf("err = %v, want reserved env error", result.Err)
	}
}

func TestWorkerEnvIncludesSessionToken(t *testing.T) {
	t.Setenv("PATH", "/tmp/flow-test-bin:/usr/bin")
	env := workerEnv(tmuxInput{
		Worktree:       "/tmp/work/repo",
		Config:         workerConfig("/tmp/work", "file:///tmp/exchange.git"),
		TranscriptFile: "/tmp/work/jobs/j-session/transcript.log",
		Job: Job{
			ID:   "j-session",
			Role: RoleAuthor,
		},
		Lease: Lease{ID: "l-session", WorkerID: "w-local"},
		Session: &coordinator.Session{
			ID: "s-session",
		},
		SessionToken: "session-token",
		Payload: JobPayload{
			SessionID: "payload-session",
			Branch:    "issue/i-0001",
			Base:      "main",
		},
		Entrypoint: Entrypoint{},
	})
	if env["FLOW_SESSION_ID"] != "s-session" {
		t.Fatalf("FLOW_SESSION_ID = %q, want session id", env["FLOW_SESSION_ID"])
	}
	if env["FLOW_SESSION_TOKEN"] != "session-token" {
		t.Fatalf("FLOW_SESSION_TOKEN = %q, want token", env["FLOW_SESSION_TOKEN"])
	}
	if env["FLOW_WORKER_ROLE"] != "author" {
		t.Fatalf("FLOW_WORKER_ROLE = %q, want author", env["FLOW_WORKER_ROLE"])
	}
	if want := filepath.Join("/tmp/work", "jobs", "j-session", VerdictFileName); env["FLOW_VERDICT_FILE"] != want {
		t.Fatalf("FLOW_VERDICT_FILE = %q, want %q", env["FLOW_VERDICT_FILE"], want)
	}
	if env["FLOW_TRANSCRIPT_FILE"] != "/tmp/work/jobs/j-session/transcript.log" {
		t.Fatalf("FLOW_TRANSCRIPT_FILE = %q, want transcript path", env["FLOW_TRANSCRIPT_FILE"])
	}
	if env["PATH"] != "/tmp/flow-test-bin:/usr/bin" {
		t.Fatalf("PATH = %q, want worker process PATH", env["PATH"])
	}
	if env["FLOW_WORKER_HARNESS"] != "codex" {
		t.Fatalf("FLOW_WORKER_HARNESS = %q, want codex", env["FLOW_WORKER_HARNESS"])
	}

	env = workerEnv(tmuxInput{
		Worktree: "/tmp/work/repo",
		Config:   workerConfig("/tmp/work", "file:///tmp/exchange.git"),
		Job: Job{
			ID:   "j-harness",
			Role: RoleAuthor,
		},
		Lease: Lease{ID: "l-harness", WorkerID: "w-local"},
		Entrypoint: Entrypoint{
			Argv:  []string{`harness -i "$(flow fetch-prompt --harness harness)"`},
			Shell: true,
		},
	})
	if env["FLOW_WORKER_HARNESS"] != "harness" {
		t.Fatalf("FLOW_WORKER_HARNESS = %q, want harness", env["FLOW_WORKER_HARNESS"])
	}
}

func TestWorkerEnvUsesHermeticJobStateDefaults(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	t.Setenv("HOME", "/host/home")
	t.Setenv("XDG_CONFIG_HOME", "/host/config")
	t.Setenv("XDG_DATA_HOME", "/host/data")
	t.Setenv("XDG_CACHE_HOME", "/host/cache")
	t.Setenv("XDG_RUNTIME_DIR", "/host/runtime")
	t.Setenv("CODEX_HOME", "/host/codex")
	t.Setenv("GOCACHE", "/host/go-build-cache")
	t.Setenv("GOMODCACHE", "/host/go-mod-cache")
	t.Setenv("DOCKER_CONFIG", "/host/docker")
	t.Setenv("NPM_CONFIG_CACHE", "/host/npm-cache")
	t.Setenv("BASH_ENV", "/host/bash-env")
	t.Setenv("CARGO_HOME", "/host/cargo")
	t.Setenv("DOCKER_HOST", "unix:///host/docker.sock")
	t.Setenv("JAVA_HOME", "/host/java")
	t.Setenv("NVM_DIR", "/host/nvm")
	t.Setenv("RUSTUP_HOME", "/host/rustup")
	t.Setenv("PATH", "/worker/bin:/usr/bin")

	env := workerEnv(tmuxInput{
		Config:     workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:        Job{ID: "j-hermetic", Role: RoleCI},
		Lease:      Lease{ID: "l-hermetic", WorkerID: "w-local"},
		Entrypoint: Entrypoint{},
	})
	root := filepath.Join(workDir, "jobs", "j-hermetic")
	want := map[string]string{
		"HOME":             filepath.Join(root, hermeticHomeDirName),
		"XDG_CONFIG_HOME":  filepath.Join(root, hermeticConfigDirName),
		"XDG_DATA_HOME":    filepath.Join(root, hermeticDataDirName),
		"XDG_CACHE_HOME":   filepath.Join(root, hermeticCacheDirName),
		"XDG_RUNTIME_DIR":  filepath.Join(root, hermeticRuntimeDirName),
		"TMPDIR":           filepath.Join(root, hermeticTempDirName),
		"TMP":              filepath.Join(root, hermeticTempDirName),
		"TEMP":             filepath.Join(root, hermeticTempDirName),
		"CODEX_HOME":       filepath.Join(root, hermeticCodexDirName),
		"GOCACHE":          filepath.Join(root, hermeticGoBuildCacheDirName),
		"GOMODCACHE":       filepath.Join(root, hermeticGoModCacheDirName),
		"DOCKER_CONFIG":    filepath.Join(root, hermeticDockerConfigDirName),
		"NPM_CONFIG_CACHE": filepath.Join(root, hermeticNPMCacheDirName),
		"PATH":             "/worker/bin:/usr/bin",
	}
	for key, wantValue := range want {
		if env[key] != wantValue {
			t.Fatalf("%s = %q, want %q", key, env[key], wantValue)
		}
	}
	for _, key := range []string{"BASH_ENV", "CARGO_HOME", "DOCKER_HOST", "JAVA_HOME", "NVM_DIR", "RUSTUP_HOME"} {
		if _, ok := env[key]; ok {
			t.Fatalf("%s leaked into job env as %q", key, env[key])
		}
	}
	if err := ensureHermeticJobEnvironment(workDir, "j-hermetic"); err != nil {
		t.Fatalf("ensure hermetic job environment: %v", err)
	}
	for key, path := range hermeticJobEnv(workDir, "j-hermetic") {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s dir %s: %v", key, path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s path %s is not a directory", key, path)
		}
	}

	overrideHome := filepath.Join(t.TempDir(), "home")
	overrideCodex := filepath.Join(t.TempDir(), "codex")
	env = workerEnv(tmuxInput{
		Config: workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:    Job{ID: "j-explicit-env", Role: RoleCI},
		Lease:  Lease{ID: "l-explicit-env", WorkerID: "w-local"},
		Entrypoint: Entrypoint{Env: map[string]string{
			"HOME":       overrideHome,
			"CODEX_HOME": overrideCodex,
			"PATH":       "/entrypoint/bin",
		}},
	})
	if env["HOME"] != overrideHome {
		t.Fatalf("explicit HOME = %q, want %q", env["HOME"], overrideHome)
	}
	if env["CODEX_HOME"] != overrideCodex {
		t.Fatalf("explicit CODEX_HOME = %q, want %q", env["CODEX_HOME"], overrideCodex)
	}
	if env["PATH"] != "/entrypoint/bin" {
		t.Fatalf("explicit PATH = %q, want /entrypoint/bin", env["PATH"])
	}
}

func TestWorkerEnvDefaultsUTF8LocaleForAgentTerminal(t *testing.T) {
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	env := workerEnv(tmuxInput{
		Config:     workerConfig("/tmp/work", "file:///tmp/exchange.git"),
		Job:        Job{ID: "j-locale", Role: RoleAuthor},
		Lease:      Lease{ID: "l-locale", WorkerID: "w-local"},
		Entrypoint: Entrypoint{},
	})
	for _, key := range utf8LocaleEnvKeys {
		if env[key] != defaultUTF8Locale {
			t.Fatalf("%s = %q, want %q", key, env[key], defaultUTF8Locale)
		}
	}

	env = workerEnv(tmuxInput{
		Config: workerConfig("/tmp/work", "file:///tmp/exchange.git"),
		Job:    Job{ID: "j-locale-explicit", Role: RoleAuthor},
		Lease:  Lease{ID: "l-locale-explicit", WorkerID: "w-local"},
		Entrypoint: Entrypoint{Env: map[string]string{
			"LANG":     "en_US.UTF-8",
			"LC_ALL":   "en_US.UTF-8",
			"LC_CTYPE": "en_US.UTF-8",
		}},
	})
	for _, key := range utf8LocaleEnvKeys {
		if env[key] != "en_US.UTF-8" {
			t.Fatalf("%s = %q, want explicit locale", key, env[key])
		}
	}
}

func TestTmuxClientEnvStripsTmuxAndDefaultsUTF8Locale(t *testing.T) {
	env := envSliceMap(tmuxClientEnv([]string{
		"PATH=/usr/bin",
		"TMUX=/tmp/tmux.sock",
		"TMUX_PANE=%1",
		"LANG=POSIX",
		"LC_CTYPE=en_US.UTF-8",
	}))
	if _, ok := env["TMUX"]; ok {
		t.Fatalf("TMUX was not stripped: %+v", env)
	}
	if _, ok := env["TMUX_PANE"]; ok {
		t.Fatalf("TMUX_PANE was not stripped: %+v", env)
	}
	if env["LANG"] != defaultUTF8Locale {
		t.Fatalf("LANG = %q, want %q", env["LANG"], defaultUTF8Locale)
	}
	if env["LC_ALL"] != defaultUTF8Locale {
		t.Fatalf("LC_ALL = %q, want %q", env["LC_ALL"], defaultUTF8Locale)
	}
	if env["LC_CTYPE"] != "en_US.UTF-8" {
		t.Fatalf("LC_CTYPE = %q, want explicit locale", env["LC_CTYPE"])
	}
}

func envSliceMap(env []string) map[string]string {
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

func TestPrepareHookConfigWritesClaudeAuthorSettings(t *testing.T) {
	workDir := t.TempDir()
	path, envVar, err := prepareHookConfig(tmuxInput{
		Config: workerConfig(workDir, "file:///tmp/exchange.git"),
		Job: Job{
			ID:   "j-claude",
			Role: RoleAuthor,
		},
		Session:      &coordinator.Session{ID: "s-claude"},
		SessionToken: "session-token",
		Entrypoint: Entrypoint{
			Argv:  []string{`claude "$prompt"`},
			Shell: true,
		},
	})
	if err != nil {
		t.Fatalf("prepareHookConfig: %v", err)
	}
	wantPath := filepath.Join(workDir, "jobs", "j-claude", claudeHookSettingsFile)
	if path != wantPath {
		t.Fatalf("settings path = %q, want %q", path, wantPath)
	}
	if envVar != envClaudeHookSettings {
		t.Fatalf("env var = %q, want %q", envVar, envClaudeHookSettings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	def, ok := flowharness.Lookup(flowharness.Claude)
	if !ok {
		t.Fatal("lookup claude definition")
	}
	want, err := flowharness.RenderHookConfig(def)
	if err != nil {
		t.Fatalf("render claude hook config: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("settings content mismatch:\n got %s\nwant %s", data, want)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat settings: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings perm = %o, want 600", perm)
	}

	env := workerEnv(tmuxInput{
		Config:           workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:              Job{ID: "j-claude", Role: RoleAuthor},
		HookConfigValue:  path,
		HookConfigEnvVar: envVar,
		Entrypoint:       Entrypoint{Argv: []string{`claude "$prompt"`}, Shell: true},
	})
	if env[envClaudeHookSettings] != path {
		t.Fatalf("%s = %q, want %q", envClaudeHookSettings, env[envClaudeHookSettings], path)
	}
}

func TestPrepareHookConfigWritesHarnessAuthorSettings(t *testing.T) {
	workDir := t.TempDir()
	path, envVar, err := prepareHookConfig(tmuxInput{
		Config: workerConfig(workDir, "file:///tmp/exchange.git"),
		Job: Job{
			ID:   "j-harness",
			Role: RoleAuthor,
		},
		Session:      &coordinator.Session{ID: "s-harness"},
		SessionToken: "session-token",
		Entrypoint: Entrypoint{
			Argv:  []string{`harness -i "$prompt"`},
			Shell: true,
		},
	})
	if err != nil {
		t.Fatalf("prepareHookConfig: %v", err)
	}
	wantPath := filepath.Join(workDir, "jobs", "j-harness", harnessHooksFile)
	if path != wantPath {
		t.Fatalf("hooks path = %q, want %q", path, wantPath)
	}
	if envVar != envHarnessHooks {
		t.Fatalf("env var = %q, want %q", envVar, envHarnessHooks)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read harness hooks: %v", err)
	}
	def, ok := flowharness.Lookup(flowharness.Harness)
	if !ok {
		t.Fatal("lookup harness definition")
	}
	want, err := flowharness.RenderHookConfig(def)
	if err != nil {
		t.Fatalf("render harness hook config: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("hooks content mismatch:\n got %s\nwant %s", data, want)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat hooks: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("hooks perm = %o, want 600", perm)
	}

	env := workerEnv(tmuxInput{
		Config:           workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:              Job{ID: "j-harness", Role: RoleAuthor},
		HookConfigValue:  path,
		HookConfigEnvVar: envVar,
		Entrypoint:       Entrypoint{Argv: []string{`harness "$prompt"`}, Shell: true},
	})
	if env[envHarnessHooks] != path {
		t.Fatalf("%s = %q, want %q", envHarnessHooks, env[envHarnessHooks], path)
	}
}

func TestPrepareHookConfigWritesCodexProfile(t *testing.T) {
	workDir := t.TempDir()
	codexHomeDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexHomeDir)

	value, envVar, err := prepareHookConfig(tmuxInput{
		Config: workerConfig(workDir, "file:///tmp/exchange.git"),
		Job: Job{
			ID:   "j-codex",
			Role: RoleAuthor,
		},
		Session:      &coordinator.Session{ID: "s-codex"},
		SessionToken: "session-token",
		Entrypoint: Entrypoint{
			Argv:  []string{`codex "$prompt"`},
			Shell: true,
		},
	})
	if err != nil {
		t.Fatalf("prepareHookConfig: %v", err)
	}
	if value != flowharness.CodexHookProfileName {
		t.Fatalf("profile value = %q, want %q", value, flowharness.CodexHookProfileName)
	}
	if envVar != "FLOW_CODEX_HOOK_PROFILE" {
		t.Fatalf("env var = %q, want FLOW_CODEX_HOOK_PROFILE", envVar)
	}
	profileName := flowharness.CodexHookProfileName + ".config.toml"
	profilePath := filepath.Join(hermeticJobEnv(workDir, "j-codex")["CODEX_HOME"], profileName)
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read codex profile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHomeDir, profileName)); !os.IsNotExist(err) {
		t.Fatalf("host CODEX_HOME profile stat err = %v, want not exist", err)
	}
	def, ok := flowharness.Lookup(flowharness.Codex)
	if !ok {
		t.Fatal("lookup codex definition")
	}
	want, err := flowharness.RenderHookConfig(def)
	if err != nil {
		t.Fatalf("render codex hook config: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("codex profile content mismatch:\n got %s\nwant %s", data, want)
	}
	if info, err := os.Stat(profilePath); err != nil {
		t.Fatalf("stat codex profile: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("codex profile perm = %o, want 600", perm)
	}

	// The worker exports the profile NAME (not a path) under FLOW_CODEX_HOOK_PROFILE.
	env := workerEnv(tmuxInput{
		Config:           workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:              Job{ID: "j-codex", Role: RoleAuthor},
		HookConfigValue:  value,
		HookConfigEnvVar: envVar,
		Entrypoint:       Entrypoint{Argv: []string{`codex "$prompt"`}, Shell: true},
	})
	if env["FLOW_CODEX_HOOK_PROFILE"] != flowharness.CodexHookProfileName {
		t.Fatalf("FLOW_CODEX_HOOK_PROFILE = %q, want %q", env["FLOW_CODEX_HOOK_PROFILE"], flowharness.CodexHookProfileName)
	}
}

func TestPrepareHookConfigSkipsHarnessesAndJobsWithoutManagedHooks(t *testing.T) {
	workDir := t.TempDir()
	tests := []struct {
		name  string
		input tmuxInput
	}{
		{
			name: "codex reviewer role is not eligible",
			input: tmuxInput{
				Config:       workerConfig(workDir, "file:///tmp/exchange.git"),
				Job:          Job{ID: "j-codex", Role: RoleReviewer},
				Session:      &coordinator.Session{ID: "s-codex"},
				SessionToken: "session-token",
				Entrypoint:   Entrypoint{Argv: []string{`codex "$prompt"`}, Shell: true},
			},
		},
		{
			name: "claude reviewer role is not eligible",
			input: tmuxInput{
				Config:       workerConfig(workDir, "file:///tmp/exchange.git"),
				Job:          Job{ID: "j-review", Role: RoleReviewer},
				Session:      &coordinator.Session{ID: "s-review"},
				SessionToken: "session-token",
				Entrypoint:   Entrypoint{Argv: []string{`claude "$prompt"`}, Shell: true},
			},
		},
		{
			name: "harness reviewer role is not eligible",
			input: tmuxInput{
				Config:       workerConfig(workDir, "file:///tmp/exchange.git"),
				Job:          Job{ID: "j-h-review", Role: RoleReviewer},
				Session:      &coordinator.Session{ID: "s-h-review"},
				SessionToken: "session-token",
				Entrypoint:   Entrypoint{Argv: []string{`harness "$prompt"`}, Shell: true},
			},
		},
		{
			name: "claude author missing session token",
			input: tmuxInput{
				Config:     workerConfig(workDir, "file:///tmp/exchange.git"),
				Job:        Job{ID: "j-missing-token", Role: RoleAuthor},
				Session:    &coordinator.Session{ID: "s-missing-token"},
				Entrypoint: Entrypoint{Argv: []string{`claude "$prompt"`}, Shell: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, envVar, err := prepareHookConfig(tt.input)
			if err != nil {
				t.Fatalf("prepareHookConfig: %v", err)
			}
			if path != "" || envVar != "" {
				t.Fatalf("path/envVar = %q/%q, want empty", path, envVar)
			}
		})
	}
}

func TestHarnessForEntrypoint(t *testing.T) {
	// These entrypoints carry no stored harness, so resolveHarness falls back to
	// the argv heuristic — the behavior the old harnessForEntrypoint provided.
	tests := []struct {
		name       string
		entrypoint Entrypoint
		want       string
	}{
		{
			name:       "codex argv",
			entrypoint: Entrypoint{Argv: []string{"/usr/local/bin/codex", "prompt"}},
			want:       "codex",
		},
		{
			name:       "codex shell",
			entrypoint: Entrypoint{Argv: []string{`codex "$(flow fetch-prompt)"`}, Shell: true},
			want:       "codex",
		},
		{
			name:       "claude shell",
			entrypoint: Entrypoint{Argv: []string{`claude "$(flow fetch-prompt)"`}, Shell: true},
			want:       "claude",
		},
		{
			name:       "harness shell",
			entrypoint: Entrypoint{Argv: []string{`harness --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`}, Shell: true},
			want:       "harness",
		},
		{
			name:       "generic agent",
			entrypoint: Entrypoint{Argv: []string{"custom-agent"}},
			want:       "agents",
		},
		{
			name:       "empty defaults to codex",
			entrypoint: Entrypoint{},
			want:       "codex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveHarness(tmuxInput{Entrypoint: tt.entrypoint}); got != tt.want {
				t.Fatalf("resolveHarness = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkerEnvIncludesWorkerTokenForReviewerAndVerifierJobs(t *testing.T) {
	for _, role := range []JobRole{RoleReviewer, RoleVerifier} {
		t.Run(string(role), func(t *testing.T) {
			cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
			cfg.Token = "worker-token"
			env := workerEnv(tmuxInput{
				Config: cfg,
				Job: Job{
					ID:   "j-review",
					Role: role,
				},
				Lease:      Lease{ID: "l-review", WorkerID: "w-local"},
				Payload:    JobPayload{},
				Entrypoint: Entrypoint{},
			})
			if env["FLOW_WORKER_TOKEN"] != "worker-token" {
				t.Fatalf("FLOW_WORKER_TOKEN = %q, want worker token", env["FLOW_WORKER_TOKEN"])
			}
		})
	}

	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.Token = "worker-token"
	env := workerEnv(tmuxInput{
		Config: cfg,
		Job: Job{
			ID:             "j-ci",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
		},
		Lease:      Lease{ID: "l-ci", WorkerID: "w-local"},
		Payload:    JobPayload{},
		Entrypoint: Entrypoint{},
	})
	if env["FLOW_WORKER_TOKEN"] != "" {
		t.Fatalf("ci FLOW_WORKER_TOKEN = %q, want empty", env["FLOW_WORKER_TOKEN"])
	}
}

func TestWorkerEnvScrubsDeploymentConfigOverrides(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.Token = "worker-token"
	env := workerEnv(tmuxInput{
		Config: cfg,
		Job: Job{
			ID:             "j-ci",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
		},
		Lease:      Lease{ID: "l-ci", WorkerID: "w-local"},
		Payload:    JobPayload{},
		Entrypoint: Entrypoint{},
	})
	for _, key := range []string{
		"FLOW_WORKER_COORDINATOR_URL",
		"FLOW_WORKER_WORK_DIR",
		"FLOW_WORKER_JOIN_TOKEN",
		"FLOW_WORKER_CAPACITY_EPHEMERAL",
		"FLOW_WORKER_TOKEN",
	} {
		if env[key] != "" {
			t.Fatalf("%s = %q, want scrubbed empty value", key, env[key])
		}
	}
	if env["FLOW_COORDINATOR_URL"] != cfg.CoordinatorURL {
		t.Fatalf("FLOW_COORDINATOR_URL = %q, want %q", env["FLOW_COORDINATOR_URL"], cfg.CoordinatorURL)
	}
	if env["FLOW_WORKER_ROLE"] != string(RoleCI) {
		t.Fatalf("FLOW_WORKER_ROLE = %q, want %q", env["FLOW_WORKER_ROLE"], RoleCI)
	}

	env = workerEnv(tmuxInput{
		Config: cfg,
		Job: Job{
			ID:   "j-reviewer",
			Role: RoleReviewer,
		},
		Lease:      Lease{ID: "l-reviewer", WorkerID: "w-local"},
		Payload:    JobPayload{},
		Entrypoint: Entrypoint{},
	})
	if env["FLOW_WORKER_TOKEN"] != "worker-token" {
		t.Fatalf("reviewer FLOW_WORKER_TOKEN = %q, want worker token", env["FLOW_WORKER_TOKEN"])
	}
}

func TestWorkerEnvIncludesHTTPGitAuthForJobCommands(t *testing.T) {
	cfg := workerConfig("/tmp/work", "http://127.0.0.1:8421/git/projects/p-test/exchange.git")
	cfg.Token = "worker-token"
	env := workerEnv(tmuxInput{
		Config: cfg,
		Job: Job{
			ID:   "j-author",
			Role: RoleAuthor,
		},
		Lease:        Lease{ID: "l-author", WorkerID: "w-local"},
		SessionToken: "session-token",
		Payload: JobPayload{
			ExchangeURL: "http://127.0.0.1:8421/git/projects/p-test/exchange.git",
		},
		Entrypoint: Entrypoint{},
	})
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "http.extraHeader" {
		t.Fatalf("git config env = count:%q key:%q", env["GIT_CONFIG_COUNT"], env["GIT_CONFIG_KEY_0"])
	}
	if env["GIT_CONFIG_VALUE_0"] != "Authorization: Bearer session-token" {
		t.Fatalf("GIT_CONFIG_VALUE_0 = %q, want session token header", env["GIT_CONFIG_VALUE_0"])
	}
}

func TestRunJobRejectsCWDOutsideWorktreeViaSymlink(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	exchangeURL := createExchangeRemote(t)
	workDir := t.TempDir()
	cfg := workerConfigWithTmux(t, workDir, exchangeURL)
	result := RunJob(context.Background(), RunInput{
		Config: cfg,
		Job: Job{
			ID:             "j-cwd",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"base":   "main",
				"branch": "main",
				"entrypoint": map[string]any{
					"argv": []string{"/bin/true"},
					"cwd":  ".",
				},
			},
		},
		Lease: Lease{ID: "l-cwd", WorkerID: "w-local"},
	})
	if result.Err != nil {
		t.Fatalf("initial run should create worktree: %v", result.Err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(result.Worktree, "escape")); err != nil {
		t.Fatalf("create cwd escape symlink: %v", err)
	}

	result = RunJob(context.Background(), RunInput{
		Config: cfg,
		Job: Job{
			ID:             "j-cwd",
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"base":   "main",
				"branch": "main",
				"entrypoint": map[string]any{
					"argv": []string{"/bin/true"},
					"cwd":  "escape",
				},
			},
		},
		Lease: Lease{ID: "l-cwd", WorkerID: "w-local"},
	})
	if result.FinalState != JobFailed {
		t.Fatalf("state = %q, want failed", result.FinalState)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "must stay inside") {
		t.Fatalf("err = %v, want cwd containment error", result.Err)
	}
}

func TestRunJobMapsNonZeroExitToFailed(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	result := RunJob(context.Background(), ciRunInput(cfg, "j-fail", "l-fail", []string{"exit 7"}, true))
	if result.Err != nil {
		t.Fatalf("run failing job returned execution error: %v", result.Err)
	}
	if result.FinalState != JobFailed || result.ExitCode != 7 {
		t.Fatalf("result = %+v, want failed exit 7", result)
	}
}

func TestRunEntrypointInTmuxHandlesImmediateExit(t *testing.T) {
	t.Parallel()
	requireTool(t, "tmux")
	baseCfg := workerConfig(t.TempDir(), "file:///tmp/exchange.git")
	job := Job{ID: "j-immediate-exit", Role: RoleCI, CapacityBucket: BucketEphemeral}
	cfg, err := tmuxConfigForJob(baseCfg, job.ID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	jobDirectory := jobDir(cfg.WorkDir, job.ID)
	workerExitFile, err := privateExitFilePath(jobDirectory)
	if err != nil {
		t.Fatalf("private exit file: %v", err)
	}
	marker := "immediate-exit-marker"
	transcriptFile := filepath.Join(jobDirectory, "transcript.log")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exitCode, err := runEntrypointInTmux(ctx, tmuxInput{
		SessionName:    sessionNameForJob(job.ID),
		Worktree:       t.TempDir(),
		WorkerExitFile: workerExitFile,
		TranscriptFile: transcriptFile,
		Config:         cfg,
		Job:            job,
		Lease:          Lease{ID: "l-immediate-exit", WorkerID: "w-local"},
		Entrypoint: Entrypoint{
			Argv:  []string{"printf '%s\\n' " + shellQuote(marker)},
			Shell: true,
		},
	})
	if err != nil {
		t.Fatalf("run immediate-exit entrypoint: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	transcript, err := os.ReadFile(transcriptFile)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(transcript), marker) {
		t.Fatalf("transcript = %q, want marker %q", string(transcript), marker)
	}
}

func TestAnyTrustPromptVisibleRecognizesActivePrompts(t *testing.T) {
	if !anyTrustPromptVisible(codexTrustPromptPane()) {
		t.Fatal("anyTrustPromptVisible returned false for active Codex trust prompt")
	}
	if !anyTrustPromptVisible(claudeTrustPromptPane()) {
		t.Fatal("anyTrustPromptVisible returned false for active Claude trust prompt")
	}
	if anyTrustPromptVisible("> Type your message\n") {
		t.Fatal("anyTrustPromptVisible returned true for an agent input pane")
	}
}

func TestAnyTrustPromptVisibleRejectsEmbeddedPromptText(t *testing.T) {
	// The trust-prompt copy quoted inside larger pane content (an issue body) must
	// not be mistaken for a live prompt: the submit instruction is no longer the
	// last line on screen.
	for _, pane := range []string{codexTrustPromptPane(), claudeTrustPromptPane()} {
		embedded := "Flow role instructions (flow-author):\n\n# Flow Author\n\nIssue: i-0005\n\n" +
			pane + "\n\nacceptance_criteria:\nthe agent no longer gets stuck at the trust prompt\n"
		if anyTrustPromptVisible(embedded) {
			t.Fatal("anyTrustPromptVisible accepted prompt text embedded in issue content")
		}
	}
}

func TestTmuxWatchdogApprovesTrustPromptOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pane       string
		foreground string
	}{
		{"codex", codexTrustPromptPane(), "codex"},
		{"claude", claudeTrustPromptPane(), "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watchdog := newTmuxWatchdogWithConfig(config.WorkerConfig{}, "flow-j-session", time.Minute)
			var sentSession string
			var sentKeys []string
			calls := 0
			watchdog.sendKeys = func(_ context.Context, sessionName string, keys ...string) error {
				calls++
				sentSession = sessionName
				sentKeys = append([]string(nil), keys...)
				return nil
			}

			if !watchdog.approveTrustPrompt(context.Background(), tc.pane, tc.foreground) {
				t.Fatalf("approveTrustPrompt returned false for active %s trust prompt", tc.name)
			}
			if sentSession != "flow-j-session" || strings.Join(sentKeys, " ") != "Enter" {
				t.Fatalf("sent %q to %q, want Enter to flow-j-session", strings.Join(sentKeys, " "), sentSession)
			}
			if watchdog.approveTrustPrompt(context.Background(), tc.pane, tc.foreground) {
				t.Fatalf("approveTrustPrompt sent the submit key more than once for %s", tc.name)
			}
			if calls != 1 {
				t.Fatalf("sendKeys called %d times for %s, want exactly 1", calls, tc.name)
			}
		})
	}
}

func TestTmuxWatchdogDoesNotApproveTrustPromptForOtherForegroundProcess(t *testing.T) {
	watchdog := newTmuxWatchdogWithConfig(config.WorkerConfig{}, "flow-j-session", time.Minute)
	watchdog.sendKeys = func(context.Context, string, ...string) error {
		t.Fatal("sendKeys called for unrelated foreground process")
		return nil
	}

	// A non-shell, non-agent foreground (a build/tool process) must never have the
	// submit key typed into it.
	if watchdog.approveTrustPrompt(context.Background(), codexTrustPromptPane(), "go") {
		t.Fatal("approveTrustPrompt approved a Codex prompt under a non-shell/non-agent foreground")
	}
	if watchdog.approveTrustPrompt(context.Background(), claudeTrustPromptPane(), "go") {
		t.Fatal("approveTrustPrompt approved a Claude prompt under a non-shell/non-agent foreground")
	}
}

func TestTmuxWatchdogApprovesTrustPromptUnderWrappers(t *testing.T) {
	// codex/claude commonly run under a node wrapper or a bootstrapping shell;
	// tmux reports those as the foreground command name.
	for _, tc := range []struct {
		name       string
		pane       string
		foreground string
	}{
		{"codex-under-node", codexTrustPromptPane(), "node"},
		{"claude-under-shell", claudeTrustPromptPane(), "zsh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watchdog := newTmuxWatchdogWithConfig(config.WorkerConfig{}, "flow-j-session", time.Minute)
			watchdog.sendKeys = func(context.Context, string, ...string) error { return nil }
			if !watchdog.approveTrustPrompt(context.Background(), tc.pane, tc.foreground) {
				t.Fatalf("approveTrustPrompt rejected %s", tc.name)
			}
		})
	}
}

func TestTmuxWatchdogReportsWorkingWhenOutputChangesAfterWaiting(t *testing.T) {
	watchdog := newTmuxWatchdogWithConfig(config.WorkerConfig{}, "flow-j-session", time.Minute)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if decision := watchdog.observe(now, "Codex waiting for input", "codex"); decision != terminal.WatchdogNoChange {
		t.Fatalf("initial decision = %q, want no_change", decision)
	}
	if decision := watchdog.observe(now.Add(2*time.Minute), "Codex waiting for input", "codex"); decision != terminal.WatchdogWaiting {
		t.Fatalf("silent decision = %q, want waiting", decision)
	}
	if decision := watchdog.observe(now.Add(2*time.Minute+time.Second), "Codex resumed work", "codex"); decision != terminal.WatchdogWorking {
		t.Fatalf("output-change decision = %q, want working", decision)
	}
}

func TestTmuxWatchdogSuppressesWaitingForBusyForegroundProcess(t *testing.T) {
	watchdog := newTmuxWatchdogWithConfig(config.WorkerConfig{}, "flow-j-session", time.Minute)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	_ = watchdog.observe(now, "Running tests", "go")
	if decision := watchdog.observe(now.Add(2*time.Minute), "Running tests", "go"); decision != terminal.WatchdogWorking {
		t.Fatalf("busy foreground decision = %q, want working", decision)
	}
	if decision := watchdog.observe(now.Add(2*time.Minute+30*time.Second), "Running tests", "codex"); decision != terminal.WatchdogNoChange {
		t.Fatalf("post-busy decision = %q, want no_change before threshold restarts", decision)
	}
}

func TestForegroundLooksBusy(t *testing.T) {
	for _, command := range []string{"go", "/usr/bin/make", "python3", "xcodebuild", "sleep", "mvn", "/tmp/custom-script"} {
		if !foregroundLooksBusy(command) {
			t.Fatalf("foregroundLooksBusy(%q) = false, want true", command)
		}
	}
	for _, command := range []string{"codex", "claude", "zsh", "node", ""} {
		if foregroundLooksBusy(command) {
			t.Fatalf("foregroundLooksBusy(%q) = true, want false", command)
		}
	}
}

func codexTrustPromptPane() string {
	return `> You are in /Users/tester/.local/share/flow/projects/p-1/workers/local/jobs/j-1/repo

  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection. Trusting the directory allows project-local config,
  hooks, and exec policies to load.

  1. Yes, continue
  2. No, quit

  Press enter to continue
`
}

func claudeTrustPromptPane() string {
	return `Accessing workspace:

 /Users/tester/.local/share/flow/projects/p-1/workers/local/jobs/j-1/repo

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 > 1. Yes, I trust this folder
   2. No, exit

 Enter to confirm / Esc to cancel
`
}

func TestSessionStateReporterRefreshesDuplicateReportsWithThrottle(t *testing.T) {
	client := &fakeSessionStateClient{}
	reporter := &sessionStateReporter{
		client:    client,
		sessionID: "s-session",
		last:      coordinator.SessionWorking,
	}

	reporter.report(coordinator.SessionWorking)
	reporter.report(coordinator.SessionWaiting)
	reporter.report(coordinator.SessionWaiting)
	reporter.report(coordinator.SessionWorking)

	want := []coordinator.SessionRuntimeState{coordinator.SessionWorking, coordinator.SessionWaiting, coordinator.SessionWorking}
	if strings.Join(runtimeStates(client.states), ",") != strings.Join(runtimeStates(want), ",") {
		t.Fatalf("reported states = %v, want %v", client.states, want)
	}
	for _, source := range client.sources {
		if source != coordinator.SessionEventSourceWatchdog {
			t.Fatalf("reported source = %q, want %q", source, coordinator.SessionEventSourceWatchdog)
		}
	}
}

func TestSessionStateReporterReportsDuplicateAfterThrottleWindow(t *testing.T) {
	client := &fakeSessionStateClient{}
	reporter := &sessionStateReporter{
		client:        client,
		sessionID:     "s-session",
		last:          coordinator.SessionWorking,
		lastAttempt:   coordinator.SessionWorking,
		lastAttemptAt: time.Now().UTC().Add(-sessionStateReportRetryInterval - time.Second),
	}

	reporter.report(coordinator.SessionWorking)

	want := []coordinator.SessionRuntimeState{coordinator.SessionWorking}
	if strings.Join(runtimeStates(client.states), ",") != strings.Join(runtimeStates(want), ",") {
		t.Fatalf("reported states = %v, want %v", client.states, want)
	}
}

func TestSessionStateReporterDoesNotThrottleWhenServerKeepsDifferentState(t *testing.T) {
	client := &fakeSessionStateClient{
		responseStates: []coordinator.SessionRuntimeState{coordinator.SessionWaiting},
	}
	reporter := &sessionStateReporter{
		client:    client,
		sessionID: "s-session",
		last:      coordinator.SessionWaiting,
	}

	reporter.report(coordinator.SessionWorking)
	reporter.report(coordinator.SessionWorking)

	want := []coordinator.SessionRuntimeState{coordinator.SessionWorking, coordinator.SessionWorking}
	if strings.Join(runtimeStates(client.states), ",") != strings.Join(runtimeStates(want), ",") {
		t.Fatalf("reported states = %v, want %v", client.states, want)
	}
}

func TestSessionStateReporterThrottlesRetryAfterFailure(t *testing.T) {
	client := &fakeSessionStateClient{fail: true}
	reporter := &sessionStateReporter{
		client:    client,
		sessionID: "s-session",
		last:      coordinator.SessionWorking,
	}

	reporter.report(coordinator.SessionWaiting)
	reporter.report(coordinator.SessionWaiting)

	if len(client.states) != 1 {
		t.Fatalf("reported states = %v, want one failed attempt", client.states)
	}
}

func TestCanRegisterJobTerminalForNonAuthorWorkerJob(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.Token = "worker-token"
	input := tmuxInput{
		Config: cfg,
		Job: Job{
			ID:   "j-reviewer",
			Role: RoleReviewer,
		},
		Lease: Lease{
			ID: "l-reviewer",
		},
	}

	if !canRegisterJobTerminal(input) {
		t.Fatal("reviewer job with worker token should register a job terminal")
	}
	input.Config.Token = ""
	if canRegisterJobTerminal(input) {
		t.Fatal("job terminal registration should require a worker token")
	}
}

func TestTerminalEndpointBaseUsesLoopbackOnlyForLocalCoordinator(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "http://127.0.0.1:8421"

	bindAddress, publicBaseURL, ok, err := terminalEndpointBase(cfg)
	if err != nil {
		t.Fatalf("terminalEndpointBase: %v", err)
	}
	if !ok || bindAddress != "127.0.0.1" || publicBaseURL != "http://127.0.0.1" {
		t.Fatalf("endpoint = bind %q public %q ok=%t", bindAddress, publicBaseURL, ok)
	}
}

func TestTerminalEndpointBaseSkipsRemoteWithoutPublicURL(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "https://flow.example.test"

	_, _, ok, err := terminalEndpointBase(cfg)
	if err != nil {
		t.Fatalf("terminalEndpointBase: %v", err)
	}
	if ok {
		t.Fatal("remote worker without terminal.public_base_url was accepted")
	}
}

func TestTerminalEndpointBaseUsesConfiguredTailnetURL(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "https://flow.example.test"
	cfg.Terminal = config.WorkerTerminalConfig{
		BindAddress:   "100.64.1.2",
		PublicBaseURL: "http://100.64.1.2",
	}

	bindAddress, publicBaseURL, ok, err := terminalEndpointBase(cfg)
	if err != nil {
		t.Fatalf("terminalEndpointBase: %v", err)
	}
	if !ok || bindAddress != "100.64.1.2" || publicBaseURL != "http://100.64.1.2" {
		t.Fatalf("endpoint = bind %q public %q ok=%t", bindAddress, publicBaseURL, ok)
	}
	if got := terminalURLWithPort(publicBaseURL, 7681); got != "http://100.64.1.2:7681" {
		t.Fatalf("terminalURLWithPort = %q", got)
	}
}

func TestTerminalEndpointBaseRejectsPublicTargetBeforeStart(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "https://flow.example.test"
	cfg.Terminal = config.WorkerTerminalConfig{
		BindAddress:   "0.0.0.0",
		PublicBaseURL: "http://8.8.8.8",
	}

	if _, _, ok, err := terminalEndpointBase(cfg); err == nil || ok {
		t.Fatalf("terminalEndpointBase ok=%t err=%v, want public target rejection", ok, err)
	}
}

func TestTerminalEndpointBaseRejectsUnsafeWildcardBind(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "https://flow.example.test"
	cfg.Terminal = config.WorkerTerminalConfig{
		BindAddress:   "0.0.0.0",
		PublicBaseURL: "http://terminal.internal",
	}

	if _, _, ok, err := terminalEndpointBase(cfg); err == nil || ok {
		t.Fatalf("terminalEndpointBase ok=%t err=%v, want wildcard host rejection", ok, err)
	}
}

func TestRequireTerminalAttachRejectsMissingTTYD(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "http://127.0.0.1:8421"

	if err := RequireTerminalAttach(cfg); err == nil || !strings.Contains(err.Error(), "ttyd is required") {
		t.Fatalf("RequireTerminalAttach err = %v, want ttyd requirement", err)
	}
}

func TestRequireTerminalAttachRejectsRemoteWithoutPublicURL(t *testing.T) {
	putFakeToolOnPath(t, "ttyd")
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "https://flow.example.test"

	if err := RequireTerminalAttach(cfg); err == nil || !strings.Contains(err.Error(), "public_base_url is required") {
		t.Fatalf("RequireTerminalAttach err = %v, want public_base_url requirement", err)
	}
}

func TestRequireTerminalAttachAcceptsLoopbackWithTTYD(t *testing.T) {
	putFakeToolOnPath(t, "ttyd")
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "http://127.0.0.1:8421"

	if err := RequireTerminalAttach(cfg); err != nil {
		t.Fatalf("RequireTerminalAttach: %v", err)
	}
}

func TestStartTmuxTerminalRejectsTTYDWithoutListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ttyd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatalf("write fake ttyd: %v", err)
	}
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "http://127.0.0.1:8421"
	cfg.Terminal.TTYDPath = path

	process, err := startTmuxTerminal(context.Background(), cfg, "flow-terminal-test")
	if process != nil {
		process.stop()
	}
	if err == nil {
		t.Fatal("startTmuxTerminal succeeded with a ttyd process that never listened")
	}
	if !strings.Contains(err.Error(), "did not start listening") {
		t.Fatalf("startTmuxTerminal error = %v, want listener startup failure", err)
	}
}

func TestStartTmuxTerminalWaitsForTTYDListener(t *testing.T) {
	cfg := workerConfig("/tmp/work", "file:///tmp/exchange.git")
	cfg.CoordinatorURL = "http://127.0.0.1:8421"
	cfg.Terminal.TTYDPath = fakeListeningTTYDPath(t)

	process, err := startTmuxTerminal(context.Background(), cfg, "flow-terminal-test")
	if err != nil {
		t.Fatalf("startTmuxTerminal: %v", err)
	}
	defer process.stop()
	if strings.TrimSpace(process.targetURL) == "" {
		t.Fatal("startTmuxTerminal returned an empty target URL")
	}
}

func fakeListeningTTYDPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ttyd")
	script := `#!/bin/sh
bind="127.0.0.1"
port=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -i)
      shift
      bind="$1"
      ;;
    -p)
      shift
      port="$1"
      ;;
  esac
  shift
done
if [ -z "$port" ]; then
  echo "missing -p" >&2
  exit 2
fi
FLOW_FAKE_TTYD_HELPER=1 FLOW_FAKE_TTYD_BIND="$bind" FLOW_FAKE_TTYD_PORT="$port" exec ` + shellQuote(os.Args[0]) + ` -test.run=^TestFakeTTYDHelperProcess$
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ttyd: %v", err)
	}
	return path
}

func TestFakeTTYDHelperProcess(t *testing.T) {
	if os.Getenv("FLOW_FAKE_TTYD_HELPER") != "1" {
		return
	}
	bind := strings.TrimSpace(os.Getenv("FLOW_FAKE_TTYD_BIND"))
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := strings.TrimSpace(os.Getenv("FLOW_FAKE_TTYD_PORT"))
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		t.Fatalf("listen fake ttyd: %v", err)
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

type fakeSessionStateClient struct {
	states         []coordinator.SessionRuntimeState
	sources        []string
	responseStates []coordinator.SessionRuntimeState
	fail           bool
}

func (f *fakeSessionStateClient) ReportSessionSignal(ctx context.Context, sessionID string, input flowclient.SessionSignalInput) (coordinator.Session, error) {
	state := coordinator.SessionRuntimeState(input.Signal)
	f.states = append(f.states, state)
	f.sources = append(f.sources, input.Source)
	if f.fail {
		return coordinator.Session{}, context.Canceled
	}
	responseState := state
	if len(f.responseStates) > 0 {
		responseState = f.responseStates[0]
		f.responseStates = f.responseStates[1:]
	}
	return coordinator.Session{ID: sessionID, RuntimeState: responseState}, nil
}

func (f *fakeSessionStateClient) RegisterSessionTerminal(ctx context.Context, sessionID string, targetURL string, tmuxSocketPath string) (coordinator.SessionTerminal, error) {
	return coordinator.SessionTerminal{SessionID: sessionID, TargetURL: targetURL, TmuxSocketPath: tmuxSocketPath}, nil
}

func runtimeStates(states []coordinator.SessionRuntimeState) []string {
	values := make([]string, 0, len(states))
	for _, state := range states {
		values = append(values, string(state))
	}
	return values
}

// ciRunInput builds a RunInput for the common ephemeral CI job shape: a job on
// main with a plain argv/shell entrypoint and a local worker lease. Only the job
// id, lease id, argv, and shell flag vary between call sites.
func ciRunInput(cfg config.WorkerConfig, jobID, leaseID string, argv []string, shell bool) RunInput {
	return RunInput{
		Config: cfg,
		Job: Job{
			ID:             jobID,
			Role:           RoleCI,
			CapacityBucket: BucketEphemeral,
			Payload: map[string]any{
				"base":   "main",
				"branch": "main",
				"entrypoint": map[string]any{
					"argv":  argv,
					"shell": shell,
				},
			},
		},
		Lease: Lease{ID: leaseID, WorkerID: "w-local"},
	}
}

func workerConfig(workDir string, exchangeURL string) config.WorkerConfig {
	return config.WorkerConfig{
		WorkerID:        "w-local",
		CoordinatorURL:  "http://127.0.0.1:8421",
		ProtocolVersion: config.DefaultProtocolVersion,
		WorkDir:         workDir,
		Git: config.WorkerGitConfig{
			ExchangeURL: exchangeURL,
			Principal:   "worker:w-local",
		},
	}
}

func workerConfigWithTmux(t *testing.T, workDir string, exchangeURL string) config.WorkerConfig {
	t.Helper()
	cfg := workerConfig(workDir, exchangeURL)
	cfg.Tmux.SocketPath = isolatedTmuxSocket(t)
	return cfg
}

func createExchangeRemote(t *testing.T) string {
	t.Helper()
	worktree, remote := createSeedGitRemote(t)
	gitRun(t, worktree, "push", remote, "main:main")
	return "file://" + filepath.ToSlash(remote)
}

func createExchangeRemoteWithoutFlowSkills(t *testing.T) string {
	t.Helper()
	_, remote := createSeedGitRemote(t)
	return "file://" + filepath.ToSlash(remote)
}

func isolatedTmuxSocket(t *testing.T) string {
	t.Helper()
	tmuxTmp, err := os.MkdirTemp("/tmp", "flow-tmux-")
	if err != nil {
		t.Fatalf("create tmux dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmuxTmp)
	})
	return filepath.Join(tmuxTmp, "tmux.sock")
}

func tmuxRun(t *testing.T, cfg config.WorkerConfig, args ...string) {
	t.Helper()
	output, err := tmuxCommand(cfg, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
}

func createSeedGitRemote(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	remote := filepath.Join(root, "exchange.git")
	gitRun(t, root, "init", worktree)
	gitRun(t, worktree, "config", "user.email", "flow@example.com")
	gitRun(t, worktree, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	gitRun(t, worktree, "add", "README.md")
	gitRun(t, worktree, "commit", "-m", "seed")
	gitRun(t, worktree, "branch", "-M", "main")
	gitRun(t, root, "init", "--bare", remote)
	gitRun(t, worktree, "push", remote, "main:main")
	return worktree, remote
}

func writeScript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func putFakeToolOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

// fakeSessionMessageClient models fakeSessionStateClient: it records the
// arguments it receives and lets a test stage pending messages, ack failures,
// and an already-delivered ack response.
type fakeSessionMessageClient struct {
	pending []coordinator.SessionMessage
	acked   []string
	// ackErr, when set, is returned by every MarkSessionMessageDelivered call
	// (used to model a persistently failing ack).
	ackErr error
	// ackErrOnce, when set, is returned by the first ack call only; later calls
	// succeed.
	ackErrOnce error
	listErr    error
	listCalls  int
}

func (f *fakeSessionMessageClient) ListPendingSessionMessages(ctx context.Context, input flowclient.ListPendingSessionMessagesInput) ([]coordinator.SessionMessage, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]coordinator.SessionMessage, len(f.pending))
	copy(out, f.pending)
	return out, nil
}

func (f *fakeSessionMessageClient) MarkSessionMessageDelivered(ctx context.Context, input flowclient.MarkSessionMessageDeliveredInput) (coordinator.SessionMessage, error) {
	if f.ackErrOnce != nil {
		err := f.ackErrOnce
		f.ackErrOnce = nil
		return coordinator.SessionMessage{}, err
	}
	if f.ackErr != nil {
		return coordinator.SessionMessage{}, f.ackErr
	}
	f.acked = append(f.acked, input.MessageID)
	// Mark the message delivered so the next ListPendingSessionMessages omits it,
	// mirroring the coordinator clearing the pending row.
	remaining := f.pending[:0]
	for _, message := range f.pending {
		if message.ID != input.MessageID {
			remaining = append(remaining, message)
		}
	}
	f.pending = remaining
	return coordinator.SessionMessage{ID: input.MessageID}, nil
}

// recordingPaster captures every (sessionName, text) pasted and can be told to
// fail for specific message bodies.
type recordingPaster struct {
	pastes     []string
	failBodies map[string]bool
}

func (p *recordingPaster) paste(ctx context.Context, cfg config.WorkerConfig, sessionName string, text string) error {
	if p.failBodies[text] {
		return errors.New("paste boom")
	}
	p.pastes = append(p.pastes, text)
	return nil
}

func newTestSessionMessagePoller(client sessionMessageClient, paster *recordingPaster) *sessionMessagePoller {
	return &sessionMessagePoller{
		client:    client,
		sessionID: "s-session",
		leaseID:   "l-lease",
		delivered: make(map[string]bool),
		paste:     paster.paste,
	}
}

func TestSessionMessagePollerDoesNotRepasteWhenAckFails(t *testing.T) {
	client := &fakeSessionMessageClient{
		pending: []coordinator.SessionMessage{{ID: "m-1", Body: "first"}},
		ackErr:  context.Canceled,
	}
	paster := &recordingPaster{}
	poller := newTestSessionMessagePoller(client, paster)

	// The ack keeps failing, so the message stays pending across ticks. The
	// worker must paste it exactly once regardless.
	for i := 0; i < 3; i++ {
		poller.deliver(context.Background(), config.WorkerConfig{}, "session")
	}

	if len(paster.pastes) != 1 {
		t.Fatalf("paste count = %d, want 1: %v", len(paster.pastes), paster.pastes)
	}
	if want := formatSessionMessageForAgent(coordinator.SessionMessage{Body: "first"}); paster.pastes[0] != want {
		t.Fatalf("pasted text = %q, want %q", paster.pastes[0], want)
	}
}

func TestSessionMessagePollerAcksAfterSuccessfulPaste(t *testing.T) {
	client := &fakeSessionMessageClient{
		pending: []coordinator.SessionMessage{{ID: "m-1", Body: "first"}},
	}
	paster := &recordingPaster{}
	poller := newTestSessionMessagePoller(client, paster)

	poller.deliver(context.Background(), config.WorkerConfig{}, "session")

	if len(paster.pastes) != 1 {
		t.Fatalf("paste count = %d, want 1", len(paster.pastes))
	}
	if len(client.acked) != 1 || client.acked[0] != "m-1" {
		t.Fatalf("acked = %v, want [m-1]", client.acked)
	}
	if !poller.delivered["m-1"] {
		t.Fatalf("delivered[m-1] = false, want true")
	}
}

func TestSessionMessagePollerToleratesAlreadyDeliveredAck(t *testing.T) {
	// An already-delivered ack (sql.ErrNoRows) is treated as success: bounded
	// attempts, no re-paste, and the message is not retried because the server
	// no longer reports it pending after the first deliver.
	client := &fakeSessionMessageClient{
		pending: []coordinator.SessionMessage{{ID: "m-1", Body: "first"}},
		ackErr:  sql.ErrNoRows,
	}
	paster := &recordingPaster{}
	poller := newTestSessionMessagePoller(client, paster)

	poller.deliver(context.Background(), config.WorkerConfig{}, "session")

	if len(paster.pastes) != 1 {
		t.Fatalf("paste count = %d, want 1", len(paster.pastes))
	}
	if !poller.delivered["m-1"] {
		t.Fatalf("delivered[m-1] = false, want true")
	}
	// A retry tick must not re-paste even though the ack reported not-pending.
	poller.deliver(context.Background(), config.WorkerConfig{}, "session")
	if len(paster.pastes) != 1 {
		t.Fatalf("paste count after retry = %d, want 1", len(paster.pastes))
	}
}

func TestSessionMessagePollerToleratesAlreadyDeliveredHTTPAck(t *testing.T) {
	// The real client surfaces the coordinator's sql.ErrNoRows as a 400; ackDelivered
	// must treat that as success too.
	client := &fakeSessionMessageClient{
		pending: []coordinator.SessionMessage{{ID: "m-1", Body: "first"}},
		ackErr:  &flowclient.HTTPStatusError{StatusCode: http.StatusBadRequest, Code: "deliver_session_message_failed"},
	}
	paster := &recordingPaster{}
	poller := newTestSessionMessagePoller(client, paster)

	poller.deliver(context.Background(), config.WorkerConfig{}, "session")

	if len(paster.pastes) != 1 {
		t.Fatalf("paste count = %d, want 1", len(paster.pastes))
	}
	if !poller.delivered["m-1"] {
		t.Fatalf("delivered[m-1] = false, want true")
	}
}

func TestSessionMessagePollerContinuesBatchAfterPasteError(t *testing.T) {
	msg1 := coordinator.SessionMessage{ID: "m-1", Body: "first"}
	msg2 := coordinator.SessionMessage{ID: "m-2", Body: "second"}
	client := &fakeSessionMessageClient{
		pending: []coordinator.SessionMessage{msg1, msg2},
	}
	paster := &recordingPaster{
		failBodies: map[string]bool{
			formatSessionMessageForAgent(msg1): true,
		},
	}
	poller := newTestSessionMessagePoller(client, paster)

	poller.deliver(context.Background(), config.WorkerConfig{}, "session")

	// msg2 still pasted despite msg1's paste error.
	if len(paster.pastes) != 1 || paster.pastes[0] != formatSessionMessageForAgent(msg2) {
		t.Fatalf("pastes = %v, want [%q]", paster.pastes, formatSessionMessageForAgent(msg2))
	}
	// msg1 is left unmarked (not delivered, not acked) so it retries next tick;
	// msg2 was acked.
	if poller.delivered["m-1"] {
		t.Fatalf("delivered[m-1] = true, want false (left pending for retry)")
	}
	if !poller.delivered["m-2"] {
		t.Fatalf("delivered[m-2] = false, want true")
	}
	if len(client.acked) != 1 || client.acked[0] != "m-2" {
		t.Fatalf("acked = %v, want [m-2]", client.acked)
	}
}

func TestAgentReadyFalseWhileTrustPromptVisible(t *testing.T) {
	if agentReady(codexTrustPromptPane(), "codex", "codex") {
		t.Fatal("agentReady returned true while the Codex trust prompt was visible")
	}
	if agentReady(claudeTrustPromptPane(), "claude", "claude") {
		t.Fatal("agentReady returned true while the Claude trust prompt was visible")
	}
}

func TestAgentReadyFalseWhileBootstrappingShell(t *testing.T) {
	// The wrapper shell is still the foreground process and the pane has not
	// drawn the agent input box yet; the prompt must not be pasted.
	if agentReady("", "sh", "codex") {
		t.Fatal("agentReady returned true for an empty pane")
	}
	if agentReady("starting agent...", "sh", "codex") {
		t.Fatal("agentReady returned true while the bootstrapping shell was foreground")
	}
	if agentReady("starting agent...", "bash", "claude") {
		t.Fatal("agentReady returned true while the bootstrapping shell was foreground")
	}
}

func TestAgentReadyTrueOnceAgentInputBoxIsUp(t *testing.T) {
	pane := "> Type your message\n"
	if !agentReady(pane, "codex", "codex") {
		t.Fatal("agentReady returned false once the Codex input box was up")
	}
	if !agentReady(pane, "claude", "claude") {
		t.Fatal("agentReady returned false once the Claude input box was up")
	}
	if !agentReady(pane, "harness", "harness") {
		t.Fatal("agentReady returned false once the harness input box was up")
	}
	// claude/codex commonly run under a node wrapper, which tmux reports as the
	// foreground command name.
	if !agentReady(pane, "node", "claude") {
		t.Fatal("agentReady returned false for the node-wrapped Claude agent")
	}
}

func TestWaitForAgentReadyDismissesTrustPromptThenReturnsNil(t *testing.T) {
	requireTool(t, "tmux")
	requireTool(t, "node")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	t.Cleanup(func() { cleanupTmuxServer(cfg) })

	// A Node-driven fake agent draws a Claude trust prompt and blocks on stdin;
	// the gate's own watchdog must send Enter, which advances the script to clear
	// the prompt and draw the input box. agentReady then flips to true: no prompt
	// is visible and tmux reports the foreground as "node", the wrapper Claude and
	// Codex run under. Node is used because tmux reports a stable "node"
	// foreground across the dismissal (a copied agent binary will not exec on a
	// signed macOS, and a wrapper shell's reported command name is not stable).
	agentScript := filepath.Join(t.TempDir(), "agent.js")
	const agentJS = "process.stdout.write('Accessing workspace:\\n\\n');\n" +
		"process.stdout.write(' /tmp/repo\\n\\n');\n" +
		"process.stdout.write(\" Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.\\n\\n\");\n" +
		"process.stdout.write(\" Claude Code'll be able to read, edit, and execute files here.\\n\\n\");\n" +
		"process.stdout.write(' Security guide\\n\\n');\n" +
		"process.stdout.write(' > 1. Yes, I trust this folder\\n   2. No, exit\\n\\n');\n" +
		"process.stdout.write(' Enter to confirm / Esc to cancel\\n');\n" +
		"process.stdin.once('data', () => { process.stdout.write('\\x1b[2J\\x1b[H'); process.stdout.write('> ready for input\\n'); setTimeout(() => {}, 30000); });\n" +
		"process.stdin.resume();\n"
	if err := os.WriteFile(agentScript, []byte(agentJS), 0o600); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	session := "flow-test-ready-dismiss"
	tmuxRun(t, cfg, "new-session", "-d", "-s", session, "node "+shellQuote(agentScript))

	prev := initialPromptReadyTimeout
	initialPromptReadyTimeout = 10 * time.Second
	t.Cleanup(func() { initialPromptReadyTimeout = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := waitForAgentReady(ctx, cfg, session, "claude"); err != nil {
		t.Fatalf("waitForAgentReady returned error after dismissing the trust prompt: %v", err)
	}
	killTmuxSession(cfg, session)
}

func TestWaitForAgentReadyTimesOutWhenNeverReady(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	t.Cleanup(func() { cleanupTmuxServer(cfg) })

	// A bare shell that never execs an agent: the foreground stays a shell, so
	// readiness is never reached and the gate must time out.
	session := "flow-test-ready-timeout"
	tmuxRun(t, cfg, "new-session", "-d", "-s", session, "sleep 30")

	prev := initialPromptReadyTimeout
	initialPromptReadyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { initialPromptReadyTimeout = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := waitForAgentReady(ctx, cfg, session, "codex")
	if err == nil {
		t.Fatal("waitForAgentReady returned nil for a pane that never became ready")
	}
	if !strings.Contains(err.Error(), "was not ready") {
		t.Fatalf("error = %v, want a readiness-timeout error", err)
	}
	killTmuxSession(cfg, session)
}

func TestWaitForAgentReadySettlesWhenForegroundUnrecognized(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	t.Cleanup(func() { cleanupTmuxServer(cfg) })

	// A shell-wrapped agent that draws output and then quiesces: tmux never
	// reports the harness binary as the foreground (it stays a shell/sleep), and
	// no trust prompt is shown, so the gate must fall back to the settle window
	// and report ready once the pane stops changing past the startup grace.
	session := "flow-test-ready-settle"
	tmuxRun(t, cfg, "new-session", "-d", "-s", session, "sh -c 'printf \"working...\\n\"; sleep 30'")

	prev := initialPromptReadyTimeout
	initialPromptReadyTimeout = 6 * time.Second
	t.Cleanup(func() { initialPromptReadyTimeout = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := waitForAgentReady(ctx, cfg, session, "codex"); err != nil {
		t.Fatalf("waitForAgentReady returned error for a settled shell-wrapped agent: %v", err)
	}
	killTmuxSession(cfg, session)
}

func TestWaitForAgentReadyReturnsWhenSessionEndsEarly(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	t.Cleanup(func() { cleanupTmuxServer(cfg) })

	// The agent exits during startup (before the settle window), so the tmux
	// session disappears. The gate must report errAgentSessionEnded promptly so
	// injectInitialPrompt skips the paste and lets normal exit handling run,
	// rather than spinning until the readiness budget expires.
	session := "flow-test-ready-ended"
	tmuxRun(t, cfg, "new-session", "-d", "-s", session, "sh -c 'sleep 0.5'")

	prev := initialPromptReadyTimeout
	initialPromptReadyTimeout = 10 * time.Second
	t.Cleanup(func() { initialPromptReadyTimeout = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := waitForAgentReady(ctx, cfg, session, "codex")
	if !errors.Is(err, errAgentSessionEnded) {
		t.Fatalf("waitForAgentReady error = %v, want errAgentSessionEnded", err)
	}
	if elapsed := time.Since(start); elapsed >= initialPromptReadyTimeout {
		t.Fatalf("waitForAgentReady took %s, want a prompt return well under the %s budget", elapsed, initialPromptReadyTimeout)
	}
}

func TestPanePreviewLines(t *testing.T) {
	pane := "first\n\n  second  \nthird\nfourth\nfifth\n\n\n"
	head, tail := panePreviewLines(pane, 2)
	if head != "first ⏎ second" {
		t.Fatalf("head = %q, want first two non-empty lines", head)
	}
	if tail != "fourth ⏎ fifth" {
		t.Fatalf("tail = %q, want last two non-empty lines", tail)
	}

	// Fewer non-empty lines than n: head and tail both span all of them.
	head, tail = panePreviewLines("only\n\n", 4)
	if head != "only" || tail != "only" {
		t.Fatalf("single-line preview head=%q tail=%q, want both %q", head, tail, "only")
	}

	if head, tail := panePreviewLines("\n  \n\n", 3); head != "" || tail != "" {
		t.Fatalf("blank pane preview head=%q tail=%q, want empty", head, tail)
	}
}

func TestWaitForAgentReadyLogsPaneOnTrustPromptTimeout(t *testing.T) {
	requireTool(t, "tmux")
	requireTool(t, "node")
	cfg := workerConfigWithTmux(t, t.TempDir(), createExchangeRemote(t))
	t.Cleanup(func() { cleanupTmuxServer(cfg) })

	// A Node-driven fake agent draws a Claude trust prompt and never reads stdin,
	// so the watchdog's Enter does not clear it: the prompt stays up and readiness
	// times out with a trust prompt still visible. The gate must then log the pane
	// head/tail so copy drift past the matcher is diagnosable instead of silent.
	agentScript := filepath.Join(t.TempDir(), "stuck-agent.js")
	const agentJS = "process.stdout.write('Accessing workspace:\\n\\n');\n" +
		"process.stdout.write(' /tmp/repo\\n\\n');\n" +
		"process.stdout.write(\" Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.\\n\\n\");\n" +
		"process.stdout.write(\" Claude Code'll be able to read, edit, and execute files here.\\n\\n\");\n" +
		"process.stdout.write(' Security guide\\n\\n');\n" +
		"process.stdout.write(' > 1. Yes, I trust this folder\\n   2. No, exit\\n\\n');\n" +
		"process.stdout.write(' Enter to confirm / Esc to cancel\\n');\n" +
		"setTimeout(() => {}, 30000);\n"
	if err := os.WriteFile(agentScript, []byte(agentJS), 0o600); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	session := "flow-test-ready-trust-timeout"
	tmuxRun(t, cfg, "new-session", "-d", "-s", session, "node "+shellQuote(agentScript))
	t.Cleanup(func() { killTmuxSession(cfg, session) })

	prev := initialPromptReadyTimeout
	initialPromptReadyTimeout = 2 * time.Second
	t.Cleanup(func() { initialPromptReadyTimeout = prev })

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := waitForAgentReady(ctx, cfg, session, "claude")
	if err == nil {
		t.Fatal("waitForAgentReady returned nil while the trust prompt was never dismissed")
	}
	if !strings.Contains(err.Error(), "was not ready") {
		t.Fatalf("error = %v, want a readiness-timeout error", err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "trust prompt") {
		t.Fatalf("timeout log = %q, want a trust-prompt drift warning", logged)
	}
	if !strings.Contains(logged, "pane_head") || !strings.Contains(logged, "pane_tail") {
		t.Fatalf("timeout log = %q, want pane head/tail telemetry", logged)
	}
	if !strings.Contains(logged, "Accessing workspace") {
		t.Fatalf("timeout log = %q, want the captured pane copy", logged)
	}
}

func TestResolveFlowBinaryPrefersEntrypointPathFlow(t *testing.T) {
	pathDir := t.TempDir()
	flowOnPath := filepath.Join(pathDir, "flow")
	if err := os.WriteFile(flowOnPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write flow on path: %v", err)
	}
	// A sibling flow exists too, but the entrypoint-PATH flow must win.
	siblingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(siblingDir, "flow"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write sibling flow: %v", err)
	}

	entrypoint := Entrypoint{Env: map[string]string{"PATH": pathDir}}
	if got := resolveFlowBinary(entrypoint); got != flowOnPath {
		t.Fatalf("resolveFlowBinary = %q, want entrypoint-PATH flow %q", got, flowOnPath)
	}
}

func TestResolveFlowBinaryFallsBackToSiblingThenLiteral(t *testing.T) {
	// Entrypoint PATH has no flow and a non-executable file named flow must be
	// ignored so resolution falls through to the literal.
	emptyDir := t.TempDir()
	nonExec := filepath.Join(emptyDir, "flow")
	if err := os.WriteFile(nonExec, []byte("not executable\n"), 0o600); err != nil {
		t.Fatalf("write non-exec flow: %v", err)
	}
	entrypoint := Entrypoint{Env: map[string]string{"PATH": emptyDir}}

	// os.Executable's sibling is the test binary's directory, which has no flow,
	// so resolution falls all the way through to the literal "flow".
	if got := resolveFlowBinary(entrypoint); got != "flow" {
		t.Fatalf("resolveFlowBinary = %q, want literal \"flow\" when no executable flow is found", got)
	}

	// With an executable flow placed next to the running test binary, the sibling
	// fallback is chosen ahead of the literal.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(exe), "flow")
	if _, statErr := os.Stat(sibling); statErr == nil {
		t.Skip("a flow already exists next to the test binary")
	}
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Skipf("cannot write sibling flow next to test binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })
	if got := resolveFlowBinary(entrypoint); got != sibling {
		t.Fatalf("resolveFlowBinary = %q, want sibling flow %q", got, sibling)
	}
}

func TestWorkerPathEnvPrefersEntrypointPathElseProcess(t *testing.T) {
	t.Setenv("PATH", "/worker/process/bin")
	if got := workerPathEnv(Entrypoint{Env: map[string]string{"PATH": "/entrypoint/bin"}}); got != "/entrypoint/bin" {
		t.Fatalf("workerPathEnv with entrypoint PATH = %q, want /entrypoint/bin", got)
	}
	if got := workerPathEnv(Entrypoint{}); got != "/worker/process/bin" {
		t.Fatalf("workerPathEnv without entrypoint PATH = %q, want process PATH", got)
	}
}
