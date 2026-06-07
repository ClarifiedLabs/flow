package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShouldInstallClientHooks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		role      JobRole
		sessionID string
		token     string
		want      bool
	}{
		{"author with live session", RoleAuthor, "s-1", "tok", true},
		{"console with live session", RoleConsole, "s-1", "tok", true},
		{"author missing token", RoleAuthor, "s-1", "", false},
		{"author missing session", RoleAuthor, "", "tok", false},
		{"reviewer never", RoleReviewer, "s-1", "tok", false},
		{"verifier never", RoleVerifier, "s-1", "tok", false},
		{"check job never", RoleCI, "s-1", "tok", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldInstallClientHooks(tc.role, tc.sessionID, tc.token); got != tc.want {
				t.Fatalf("shouldInstallClientHooks(%s, %q, %q) = %v, want %v", tc.role, tc.sessionID, tc.token, got, tc.want)
			}
		})
	}
}

func TestPrepareWorktreeInstallsClientHooksForAuthorSession(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	cfg := workerConfig(t.TempDir(), exchangeURL)
	payload, err := DecodePayload(map[string]any{
		"base":          "main",
		"branch":        "main",
		"agent_harness": "claude",
		"entrypoint":    map[string]any{"argv": []string{"true"}, "cwd": "."},
	})
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	job := Job{ID: "j-author", Role: RoleAuthor}

	worktree, err := prepareWorktree(ctx, cfg, job, payload, "s-1", "session-token")
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}

	for hook, sub := range map[string]string{"pre-push": "prepush", "commit-msg": "commit-msg"} {
		path := filepath.Join(worktree, ".git", "hooks", hook)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("expected %s installed: %v", hook, err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Fatalf("%s mode = %v, want 0755", hook, perm)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", hook, err)
		}
		if want := "hook 'claude' '" + sub + "'"; !strings.Contains(string(data), want) {
			t.Fatalf("%s missing %q:\n%s", hook, want, string(data))
		}
	}
}

func TestPrepareWorktreeExcludesFlowArtifacts(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	cfg := workerConfig(t.TempDir(), exchangeURL)
	payload, err := DecodePayload(map[string]any{
		"base":       "main",
		"branch":     "main",
		"entrypoint": map[string]any{"argv": []string{"true"}, "cwd": "."},
	})
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	job := Job{ID: "j-exclude", Role: RoleAuthor}

	worktree, err := prepareWorktree(ctx, cfg, job, payload, "s-1", "session-token")
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}

	// The info/exclude file is written and contains the narrow .flow/attachments/
	// pattern. Crucially it must NOT contain the broad .flow/ pattern, which would
	// also exclude .flow/checks and .flow/session (committable Flow artifacts).
	excludePath := filepath.Join(worktree, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	if !strings.Contains(string(data), ".flow/attachments/") {
		t.Fatalf("exclude file missing .flow/attachments/ pattern:\n%s", string(data))
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == ".flow/" || strings.HasPrefix(line, ".flow/ ") {
			t.Fatalf("exclude file must not contain a broad .flow/ pattern (would exclude .flow/checks and .flow/session):\n%s", string(data))
		}
	}

	// A materialized image written under .flow/attachments/ stays untracked by git.
	if err := os.MkdirAll(filepath.Join(worktree, ".flow", "attachments"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	imagePath := filepath.Join(worktree, ".flow", "attachments", "att-0001-shot.png")
	if err := os.WriteFile(imagePath, []byte("png-bytes"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	// A check definition and a session file under .flow/ — both real committed
	// Flow paths — must remain stageable by `git add -A`.
	if err := os.MkdirAll(filepath.Join(worktree, ".flow", "checks"), 0o700); err != nil {
		t.Fatalf("mkdir checks: %v", err)
	}
	checkPath := filepath.Join(worktree, ".flow", "checks", "unit.yaml")
	if err := os.WriteFile(checkPath, []byte("name: unit\n"), 0o600); err != nil {
		t.Fatalf("write check: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".flow", "session"), 0o700); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	sessionPath := filepath.Join(worktree, ".flow", "session", "state.json")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	gitOutput(t, worktree, "add", "-A")
	staged := gitOutput(t, worktree, "diff", "--cached", "--name-only")
	normalized := map[string]bool{}
	for _, name := range strings.Split(staged, "\n") {
		if n := strings.TrimSpace(name); n != "" {
			normalized[n] = true
		}
	}
	// `git diff --cached --name-only` reports paths relative to the worktree root,
	// so compare against the worktree-relative form of each path. The binary image
	// must NOT be staged; the check definition and session file MUST be staged so
	// the check-config workflow survives a blanket `git add -A`.
	for absPath, mustNotStage := range map[string]bool{
		imagePath:   true,
		checkPath:   false,
		sessionPath: false,
	} {
		relPath, err := filepath.Rel(worktree, absPath)
		if err != nil {
			t.Fatalf("rel %s: %v", absPath, err)
		}
		relPath = filepath.ToSlash(relPath)
		isStaged := normalized[relPath]
		if mustNotStage && isStaged {
			t.Fatalf("path %s must NOT be staged, but it appears in:\n%s", relPath, staged)
		}
		if !mustNotStage && !isStaged {
			t.Fatalf("committable Flow artifact %s must be staged by `git add -A`, but it is missing from:\n%s", relPath, staged)
		}
	}
}

func TestExcludeFlowArtifactsFromWorktreeIsIdempotent(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	excludePath := filepath.Join(repoDir, ".git", "info")
	if err := os.MkdirAll(excludePath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// First write adds the pattern.
	excludeFlowArtifactsFromWorktree(repoDir)
	first, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if got := strings.Count(string(first), ".flow/attachments/"); got != 1 {
		t.Fatalf("expected one .flow/attachments/ pattern after first write, got %d:\n%s", got, string(first))
	}

	// Second write must not duplicate the pattern.
	excludeFlowArtifactsFromWorktree(repoDir)
	second, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if got := strings.Count(string(second), ".flow/attachments/"); got != 1 {
		t.Fatalf("expected one .flow/attachments/ pattern after second write, got %d:\n%s", got, string(second))
	}
	if string(first) != string(second) {
		t.Fatalf("exclude file changed on repeated write:\nfirst: %s\nsecond: %s", string(first), string(second))
	}
}

func TestPrepareWorktreeSkipsClientHooksForCheckJob(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	ctx := context.Background()
	exchangeURL := createExchangeRemote(t)
	cfg := workerConfig(t.TempDir(), exchangeURL)
	payload, err := DecodePayload(map[string]any{
		"base":       "main",
		"branch":     "main",
		"entrypoint": map[string]any{"argv": []string{"true"}, "cwd": "."},
	})
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	job := Job{ID: "j-ci", Role: RoleCI}

	worktree, err := prepareWorktree(ctx, cfg, job, payload, "", "")
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(worktree, ".git", "hooks", "pre-push")); !os.IsNotExist(err) {
		t.Fatalf("check job unexpectedly got client hooks (stat err = %v)", err)
	}
}
