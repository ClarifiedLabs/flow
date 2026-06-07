package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// putFakeGitOnPath installs a fake `git` script ahead of the real one on PATH.
func putFakeGitOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunGitTimesOutOnHang(t *testing.T) {
	putFakeGitOnPath(t, "#!/bin/sh\nexec sleep 60\n")

	prev := gitOpTimeout
	gitOpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitOpTimeout = prev })

	start := time.Now()
	_, err := runGit(context.Background(), "", "", nil, "status")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected prompt return, took %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestGitExitCodeSurfacesDeadlineAsNegativeOne(t *testing.T) {
	putFakeGitOnPath(t, "#!/bin/sh\nexec sleep 60\n")

	prev := gitOpTimeout
	gitOpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitOpTimeout = prev })

	code, err := gitExitCode(context.Background(), "", "", nil, "status")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code != -1 {
		t.Fatalf("expected exit code -1 for deadline kill, got %d", code)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

// TestGitRunTransferOutlivesRoutineTimeout pins the seed-push regression: a
// full-history transfer must get the larger transfer budget, not the routine
// 120s one that would kill large-repo onboarding mid-push.
func TestGitRunTransferOutlivesRoutineTimeout(t *testing.T) {
	putFakeGitOnPath(t, "#!/bin/sh\nsleep 0.5\nexit 0\n")

	prevOp := gitOpTimeout
	gitOpTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitOpTimeout = prevOp })

	if err := gitRunTransfer(context.Background(), "", nil, "push", "origin", "main"); err != nil {
		t.Fatalf("transfer-budget push failed under a shrunken routine timeout: %v", err)
	}

	prevTransfer := gitTransferTimeout
	gitTransferTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitTransferTimeout = prevTransfer })

	if err := gitRunTransfer(context.Background(), "", nil, "push", "origin", "main"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected transfer deadline error, got %v", err)
	}
}

func TestRunGitSucceedsUnderDefaultTimeout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed")
	}

	result, err := runGit(context.Background(), "", "", nil, "version")
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if result.stdout == "" {
		t.Fatal("expected git version output")
	}
}
