package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Client-side git hooks fire on the agent's NATURAL git actions inside the job
// worktree (commit, push), so a deterministic capture + steering layer runs
// without the agent having to remember anything. They are deliberately
// best-effort and NON-BLOCKING: a hook must never reject a commit or push, so
// every generated script swallows failures and exits 0.
//
// Honest scope: these hooks only fire when the agent runs git. They
// capture/steer/validate; they are NOT a substitute for the "done" judgment.
// A push is not task completion -- `flow ready` remains the authoritative
// finalize. The hooks complement it.

// clientHookSubcommands maps each git hook filename Flow installs to the
// `flow hook <kind> <subcommand>` it dispatches to.
var clientHookSubcommands = map[string]string{
	"pre-push":   "prepush",
	"commit-msg": "commit-msg",
}

// ClientHookInstallOptions configures the client-side hooks Flow installs into
// the agent's job worktree.
type ClientHookInstallOptions struct {
	// HookCommand is the flow invocation the generated hooks shell out to.
	// When Path is empty it defaults to the current executable. Env, when set,
	// is exported before the command; it is intentionally left empty by the
	// worker so the session token is read from the inherited environment rather
	// than written into a script on disk.
	HookCommand HookCommand
	// HarnessKind is the harness label routed through as `flow hook <kind> ...`.
	HarnessKind string
}

// InstallClientHooks writes Flow-managed client-side git hooks into the job
// worktree's hooks directory. repoDir is the worktree root (its .git is a full
// clone). Existing flow hooks are overwritten so re-prepared worktrees stay
// current.
func InstallClientHooks(repoDir string, opts ClientHookInstallOptions) error {
	if strings.TrimSpace(opts.HarnessKind) == "" {
		return errors.New("client hook harness kind is required")
	}
	if strings.TrimSpace(opts.HookCommand.Path) == "" {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable for client hooks: %w", err)
		}
		opts.HookCommand.Path = executable
	}

	hooksDir, err := clientHooksDir(repoDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create client hooks directory: %w", err)
	}

	for hookName, subcommand := range clientHookSubcommands {
		script, err := clientHookScript(opts.HookCommand, opts.HarnessKind, subcommand)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(hooksDir, hookName), []byte(script), 0o755); err != nil {
			return fmt.Errorf("write %s hook: %w", hookName, err)
		}
	}

	return nil
}

// clientHooksDir resolves the hooks directory git consults for the worktree.
// For a normal clone .git is a directory; for a linked worktree it is a gitfile
// pointing at the real git dir.
func clientHooksDir(repoDir string) (string, error) {
	gitPath := filepath.Join(repoDir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("stat worktree git dir: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(gitPath, "hooks"), nil
	}

	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", fmt.Errorf("read worktree gitfile: %w", err)
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if gitDir == "" {
		return "", errors.New("worktree gitfile does not point at a git dir")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoDir, gitDir)
	}
	return filepath.Join(gitDir, "hooks"), nil
}

// clientHookScript renders a POSIX hook that shells out to flow and ALWAYS
// exits 0. It deliberately avoids `exec` so the `|| true` guard runs even when
// flow is missing or errors -- the hook can capture/steer but never block the
// agent's git action. git's positional args ($1.. for the message file path,
// remote name/url) and stdin (ref updates) are forwarded verbatim via "$@".
func clientHookScript(command HookCommand, harnessKind string, subcommand string) (string, error) {
	if strings.TrimSpace(command.Path) == "" {
		return "", errors.New("hook command path is required")
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Flow-managed client hook. Fires on the agent's natural git actions to\n")
	b.WriteString("# capture push context and steer review-thread hygiene. Best-effort and\n")
	b.WriteString("# NON-BLOCKING: it never rejects a commit or push, so it always exits 0.\n")
	b.WriteString("# A push is not task completion -- `flow ready` remains the finalize.\n")
	for _, key := range sortedEnvKeys(command.Env) {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(command.Env[key]))
		b.WriteString("\n")
	}
	b.WriteString(shellQuote(command.Path))
	for _, arg := range command.Args {
		b.WriteString(" ")
		b.WriteString(shellQuote(arg))
	}
	b.WriteString(" hook ")
	b.WriteString(shellQuote(harnessKind))
	b.WriteString(" ")
	b.WriteString(shellQuote(subcommand))
	b.WriteString(` "$@" || true` + "\n")
	b.WriteString("exit 0\n")

	return b.String(), nil
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
