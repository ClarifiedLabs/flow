package execution

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupFinalizedJobRemovesWholeHermeticWorkspace(t *testing.T) {
	workDir := t.TempDir()
	jobID := "j-finalized"
	for _, path := range []string{
		filepath.Join(workDir, "jobs", jobID, "repo", "source.go"),
		filepath.Join(workDir, "jobs", jobID, hermeticGoBuildCacheDirName, "cache-entry"),
		filepath.Join(workDir, "jobs", jobID, hermeticGoModCacheDirName, "module"),
		filepath.Join(workDir, "jobs", jobID, "transcript.log"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create workspace directory: %v", err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("write workspace file: %v", err)
		}
	}

	removed, err := CleanupFinalizedJob(workDir, jobID)
	if err != nil {
		t.Fatalf("CleanupFinalizedJob: %v", err)
	}
	if !removed {
		t.Fatal("CleanupFinalizedJob removed = false, want true")
	}
	if _, err := os.Stat(filepath.Join(workDir, "jobs", jobID)); !os.IsNotExist(err) {
		t.Fatalf("finalized workspace still exists: %v", err)
	}
}

func TestCleanupFinalizedJobRefusesActiveJob(t *testing.T) {
	workDir := t.TempDir()
	jobID := "j-active"
	path := filepath.Join(workDir, "jobs", jobID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("create active workspace: %v", err)
	}
	RegisterActiveJob(jobID)
	t.Cleanup(func() { UnregisterActiveJob(jobID) })

	removed, err := CleanupFinalizedJob(workDir, jobID)
	if err == nil {
		t.Fatal("CleanupFinalizedJob accepted an active job")
	}
	if removed {
		t.Fatal("CleanupFinalizedJob removed an active job")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active workspace was removed: %v", err)
	}
}

func TestReapOrphanedJobWorkspacesUsesCoordinatorStateAndGrace(t *testing.T) {
	workDir := t.TempDir()
	now := time.Now().UTC()
	for _, jobID := range []string{"j-terminal", "j-running", "j-old-unknown", "j-fresh-unknown", "j-terminal-active"} {
		path := filepath.Join(workDir, "jobs", jobID)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create %s: %v", jobID, err)
		}
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(workDir, "jobs", "j-old-unknown"), old, old); err != nil {
		t.Fatalf("age unknown workspace: %v", err)
	}
	RegisterActiveJob("j-terminal-active")
	t.Cleanup(func() { UnregisterActiveJob("j-terminal-active") })

	result, err := ReapOrphanedJobWorkspaces(workDir, []Job{
		{ID: "j-terminal", State: JobFinished},
		{ID: "j-running", State: JobRunning},
		{ID: "j-terminal-active", State: JobFailed},
	}, time.Hour, now)
	if err != nil {
		t.Fatalf("ReapOrphanedJobWorkspaces: %v", err)
	}
	if result.Removed != 2 || result.Remaining != 3 {
		t.Fatalf("result = %+v, want removed=2 remaining=3", result)
	}
	for _, jobID := range []string{"j-terminal", "j-old-unknown"} {
		if _, err := os.Stat(filepath.Join(workDir, "jobs", jobID)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", jobID, err)
		}
	}
	for _, jobID := range []string{"j-running", "j-fresh-unknown", "j-terminal-active"} {
		if _, err := os.Stat(filepath.Join(workDir, "jobs", jobID)); err != nil {
			t.Errorf("%s was not preserved: %v", jobID, err)
		}
	}
}

func TestCleanupFinalizedJobRejectsPathTraversal(t *testing.T) {
	workDir := t.TempDir()
	outside := filepath.Join(workDir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create outside directory: %v", err)
	}
	for _, jobID := range []string{"", ".", "..", "../outside", `..\outside`} {
		if removed, err := CleanupFinalizedJob(workDir, jobID); err == nil || removed {
			t.Errorf("CleanupFinalizedJob(%q) = removed=%v err=%v, want false/error", jobID, removed, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("path traversal removed outside directory: %v", err)
	}
}

func TestCleanupFinalizedJobRejectsSymlinkedJobsRoot(t *testing.T) {
	workDir := t.TempDir()
	outside := t.TempDir()
	jobID := "j-outside"
	outsideJob := filepath.Join(outside, jobID)
	if err := os.MkdirAll(outsideJob, 0o700); err != nil {
		t.Fatalf("create outside job: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "jobs")); err != nil {
		t.Skipf("create jobs-root symlink: %v", err)
	}

	removed, err := CleanupFinalizedJob(workDir, jobID)
	if err == nil || removed {
		t.Fatalf("CleanupFinalizedJob through jobs-root symlink = removed=%v err=%v, want false/error", removed, err)
	}
	if _, err := os.Stat(outsideJob); err != nil {
		t.Fatalf("outside job was removed through symlinked root: %v", err)
	}
}
