package execution

import (
	"context"
	"testing"
)

func job(id string, state JobState) Job {
	return Job{ID: id, State: state}
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
