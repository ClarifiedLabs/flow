package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
)

func TestGitHelperTimesOutOnHang(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	// exec so the SIGKILL from CommandContext lands on sleep itself rather than
	// leaving a grandchild holding the CombinedOutput pipe open.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prev := gitCloneFetchTimeout
	gitCloneFetchTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitCloneFetchTimeout = prev })

	start := time.Now()
	err := git(context.Background(), "", config.WorkerConfig{}, "fetch", "origin")
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

func TestGitHelperSucceedsUnderDefaultTimeout(t *testing.T) {
	requireTool(t, "git")

	if err := git(context.Background(), "", config.WorkerConfig{}, "version"); err != nil {
		t.Fatalf("git version: %v", err)
	}
}
