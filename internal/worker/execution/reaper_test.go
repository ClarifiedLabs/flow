package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/terminal"
)

// fakeTmux installs a fake `tmux` binary on PATH that records its invocations
// to a log file. list-sessions emits the supplied session names (one per line);
// kill-session succeeds for every target unless its name is in failKills, in
// which case it exits non-zero. When sessions is nil the fake mimics "no tmux
// server running" by exiting 1 with no output.
type fakeTmux struct {
	logPath string
}

func newFakeTmux(t *testing.T, sessions []string, failKills ...string) *fakeTmux {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")

	listing := strings.Join(sessions, "\n")
	noServer := "0"
	if sessions == nil {
		noServer = "1"
	}
	failSet := "|" + strings.Join(failKills, "|") + "|"

	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + shellQuote(logPath) + `
if [ "$1" = "-S" ]; then
  shift 2
fi
case "$1" in
list-sessions)
  if [ "` + noServer + `" = "1" ]; then
    exit 1
  fi
  printf '%s' ` + shellQuote(listing) + `
  if [ -n ` + shellQuote(listing) + ` ]; then
    printf '\n'
  fi
  exit 0
  ;;
kill-session)
  target="$3"
  case "` + failSet + `" in
  *"|$target|"*)
    echo "cannot kill $target" 1>&2
    exit 1
    ;;
  esac
  exit 0
  ;;
esac
exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return &fakeTmux{logPath: logPath}
}

// killedSessions returns the session names targeted by kill-session invocations.
func (f *fakeTmux) killedSessions(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read tmux log: %v", err)
	}
	var killed []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "-S" {
			fields = fields[2:]
		}
		if len(fields) >= 3 && fields[0] == "kill-session" && fields[1] == "-t" {
			killed = append(killed, fields[2])
		}
	}
	return killed
}

func (f *fakeTmux) invocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	return string(data)
}

func job(id string, state JobState) Job {
	return Job{ID: id, State: state}
}

func TestReapOrphanedSessionsKillsTerminalJobSession(t *testing.T) {
	session := terminal.TmuxSessionNameForJob("job-terminal")
	tmux := newFakeTmux(t, []string{session})

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job("job-terminal", JobFinished),
	})
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 1 {
		t.Fatalf("killed = %d, want 1", killed)
	}
	if got := tmux.killedSessions(t); len(got) != 1 || got[0] != session {
		t.Fatalf("killed sessions = %v, want [%s]", got, session)
	}
}

func TestReapOrphanedSessionsLeavesRunningJobSession(t *testing.T) {
	session := terminal.TmuxSessionNameForJob("job-running")
	tmux := newFakeTmux(t, []string{session})

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job("job-running", JobRunning),
	})
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 0 {
		t.Fatalf("killed = %d, want 0", killed)
	}
	if got := tmux.killedSessions(t); len(got) != 0 {
		t.Fatalf("killed sessions = %v, want none", got)
	}
}

func TestReapOrphanedSessionsKillsSessionWithNoKnownJob(t *testing.T) {
	orphan := terminal.TmuxSessionNameForJob("job-gone")
	tmux := newFakeTmux(t, []string{orphan})

	killed, err := ReapOrphanedSessions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 1 {
		t.Fatalf("killed = %d, want 1", killed)
	}
	if got := tmux.killedSessions(t); len(got) != 1 || got[0] != orphan {
		t.Fatalf("killed sessions = %v, want [%s]", got, orphan)
	}
}

func TestReapOrphanedSessionsIgnoresNonFlowSessions(t *testing.T) {
	tmux := newFakeTmux(t, []string{"some-other-session", "tmux-default"})

	killed, err := ReapOrphanedSessions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 0 {
		t.Fatalf("killed = %d, want 0", killed)
	}
	if got := tmux.killedSessions(t); len(got) != 0 {
		t.Fatalf("killed sessions = %v, want none", got)
	}
}

func TestReapOrphanedSessionsContinuesAfterKillFailure(t *testing.T) {
	failing := terminal.TmuxSessionNameForJob("job-fail")
	surviving := terminal.TmuxSessionNameForJob("job-ok")
	tmux := newFakeTmux(t, []string{failing, surviving}, failing)

	killed, err := ReapOrphanedSessions(context.Background(), nil)
	if err == nil {
		t.Fatalf("ReapOrphanedSessions: expected joined error, got nil")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Fatalf("error %q does not mention failing session %q", err.Error(), failing)
	}
	if killed != 1 {
		t.Fatalf("killed = %d, want 1", killed)
	}
	// Both sessions must have a kill attempted even though the first failed.
	got := tmux.killedSessions(t)
	if !containsString(got, failing) || !containsString(got, surviving) {
		t.Fatalf("kill attempts = %v, want both %q and %q", got, failing, surviving)
	}
}

func TestReapOrphanedSessionsTreatsMissingServerAsEmpty(t *testing.T) {
	tmux := newFakeTmux(t, nil)

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job("job-terminal", JobFinished),
	})
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 0 {
		t.Fatalf("killed = %d, want 0", killed)
	}
	if got := tmux.killedSessions(t); len(got) != 0 {
		t.Fatalf("killed sessions = %v, want none", got)
	}
}

func TestReapOrphanedSessionsUsesConfiguredTmuxSocket(t *testing.T) {
	session := terminal.TmuxSessionNameForJob("job-terminal")
	socketPath := "/tmp/flow-test-tmux.sock"
	tmux := newFakeTmux(t, []string{session})

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job("job-terminal", JobFinished),
	}, WithWorkerConfig(config.WorkerConfig{Tmux: config.WorkerTmuxConfig{SocketPath: socketPath}}))
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 1 {
		t.Fatalf("killed = %d, want 1", killed)
	}
	log := tmux.invocations(t)
	if !strings.Contains(log, "-S "+socketPath+" list-sessions") ||
		!strings.Contains(log, "-S "+socketPath+" kill-session -t "+session) {
		t.Fatalf("tmux invocations did not use configured socket %q:\n%s", socketPath, log)
	}
}

func TestReapOrphanedSessionsKillsTerminalPerJobTmuxServer(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), "file:///tmp/exchange.git")
	jobID := "job-terminal"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	session := sessionNameForJob(jobID)
	tmuxRun(t, jobCfg, "new-session", "-d", "-s", session, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(jobCfg)
	})

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job(jobID, JobFinished),
	}, WithWorkerConfig(cfg))
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 1 {
		t.Fatalf("killed = %d, want 1", killed)
	}
	if tmuxSessionExists(context.Background(), jobCfg, session) {
		t.Fatalf("per-job tmux session %q still exists after reap", session)
	}
}

func TestReapOrphanedSessionsLeavesRunningPerJobTmuxServer(t *testing.T) {
	requireTool(t, "tmux")
	cfg := workerConfigWithTmux(t, t.TempDir(), "file:///tmp/exchange.git")
	jobID := "job-running"
	jobCfg, err := tmuxConfigForJob(cfg, jobID)
	if err != nil {
		t.Fatalf("job tmux config: %v", err)
	}
	session := sessionNameForJob(jobID)
	tmuxRun(t, jobCfg, "new-session", "-d", "-s", session, "sleep 60")
	t.Cleanup(func() {
		cleanupTmuxServer(jobCfg)
	})

	killed, err := ReapOrphanedSessions(context.Background(), []Job{
		job(jobID, JobRunning),
	}, WithWorkerConfig(cfg))
	if err != nil {
		t.Fatalf("ReapOrphanedSessions: %v", err)
	}
	if killed != 0 {
		t.Fatalf("killed = %d, want 0", killed)
	}
	if !tmuxSessionExists(context.Background(), jobCfg, session) {
		t.Fatalf("running per-job tmux session %q was reaped", session)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
