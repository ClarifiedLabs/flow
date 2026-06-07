package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallClientHooksWritesExecutableScripts(t *testing.T) {
	t.Parallel()
	repoDir := newClonedRepoDir(t)

	if err := InstallClientHooks(repoDir, ClientHookInstallOptions{
		HookCommand: HookCommand{Path: "/usr/local/bin/flow"},
		HarnessKind: "claude",
	}); err != nil {
		t.Fatalf("install client hooks: %v", err)
	}

	for hookName, sub := range clientHookSubcommands {
		path := filepath.Join(repoDir, ".git", "hooks", hookName)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", hookName, err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Fatalf("%s mode = %v, want 0755", hookName, perm)
		}
		script := readFile(t, path)
		for _, want := range []string{
			"#!/bin/sh",
			"'/usr/local/bin/flow' hook 'claude' '" + sub + "'",
			`"$@"`,
			"|| true",
			"exit 0",
		} {
			if !strings.Contains(script, want) {
				t.Fatalf("%s script missing %q:\n%s", hookName, want, script)
			}
		}
		// A blocking hook (exec or a bare command) could reject the agent's git
		// action. The script must shell out without exec so `|| true` runs.
		if strings.Contains(script, "exec ") {
			t.Fatalf("%s script uses exec, which would defeat the non-blocking guard:\n%s", hookName, script)
		}
	}
}

func TestInstallClientHooksDefaultsToCurrentExecutable(t *testing.T) {
	t.Parallel()
	repoDir := newClonedRepoDir(t)

	if err := InstallClientHooks(repoDir, ClientHookInstallOptions{HarnessKind: "codex"}); err != nil {
		t.Fatalf("install client hooks: %v", err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve executable: %v", err)
	}
	script := readFile(t, filepath.Join(repoDir, ".git", "hooks", "pre-push"))
	if !strings.Contains(script, shellQuote(executable)) {
		t.Fatalf("pre-push script missing default executable %q:\n%s", executable, script)
	}
}

func TestInstallClientHooksRequiresHarnessKind(t *testing.T) {
	t.Parallel()
	repoDir := newClonedRepoDir(t)

	if err := InstallClientHooks(repoDir, ClientHookInstallOptions{
		HookCommand: HookCommand{Path: "flow"},
	}); err == nil {
		t.Fatal("install without harness kind was accepted")
	}
}

// The session token reaches the hook through the inherited process environment,
// never written into a script on disk. Baking it would leak it into .git/hooks.
func TestInstallClientHooksDoesNotEmbedEnvByDefault(t *testing.T) {
	t.Parallel()
	repoDir := newClonedRepoDir(t)

	if err := InstallClientHooks(repoDir, ClientHookInstallOptions{
		HookCommand: HookCommand{Path: "flow"},
		HarnessKind: "claude",
	}); err != nil {
		t.Fatalf("install client hooks: %v", err)
	}
	script := readFile(t, filepath.Join(repoDir, ".git", "hooks", "commit-msg"))
	if strings.Contains(script, "export ") {
		t.Fatalf("commit-msg script unexpectedly exports environment:\n%s", script)
	}
}

func TestInstallClientHooksResolvesGitFileWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realGitDir := filepath.Join(root, "actual-git")
	if err := os.MkdirAll(realGitDir, 0o755); err != nil {
		t.Fatalf("create real git dir: %v", err)
	}
	repoDir := filepath.Join(root, "worktree")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write gitfile: %v", err)
	}

	if err := InstallClientHooks(repoDir, ClientHookInstallOptions{
		HookCommand: HookCommand{Path: "flow"},
		HarnessKind: "claude",
	}); err != nil {
		t.Fatalf("install client hooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realGitDir, "hooks", "pre-push")); err != nil {
		t.Fatalf("hook not installed into gitfile-resolved hooks dir: %v", err)
	}
}

func newClonedRepoDir(t *testing.T) string {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	return repoDir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
