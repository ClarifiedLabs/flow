package git

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	zeroSHA          = "0000000000000000000000000000000000000000"
	spoolDirName     = "flow-spool"
	spoolFileName    = "post-receive.jsonl"
	coordinatorActor = "coordinator"
	ownerActor       = "owner"
)

var issueRefPattern = regexp.MustCompile(`^refs/heads/issue/i-[0-9]{4,}$`)

type HookInstallOptions struct {
	BaseBranch  string
	HookCommand HookCommand
}

type HookOptions struct {
	ExchangeRepoPath string
	BaseBranch       string
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	Principal        *string
}

type RefUpdate struct {
	OldSHA string
	NewSHA string
	Ref    string
}

type HookEvent struct {
	OldSHA     string    `json:"old_sha"`
	NewSHA     string    `json:"new_sha"`
	Ref        string    `json:"ref"`
	Actor      string    `json:"actor"`
	ObservedAt time.Time `json:"observed_at"`
}

func InstallHooks(exchangePath string, opts HookInstallOptions) error {
	if strings.TrimSpace(opts.BaseBranch) == "" {
		return errors.New("hook base branch is required")
	}
	if strings.TrimSpace(opts.HookCommand.Path) == "" {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable for hooks: %w", err)
		}
		opts.HookCommand.Path = executable
	}

	hooksDir := filepath.Join(exchangePath, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks directory: %w", err)
	}

	preReceive, err := hookScript(opts.HookCommand, "pre-receive", exchangePath, opts.BaseBranch)
	if err != nil {
		return err
	}
	postReceive, err := hookScript(opts.HookCommand, "post-receive", exchangePath, opts.BaseBranch)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(hooksDir, "pre-receive"), []byte(preReceive), 0o755); err != nil {
		return fmt.Errorf("write pre-receive hook: %w", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(postReceive), 0o755); err != nil {
		return fmt.Errorf("write post-receive hook: %w", err)
	}

	return nil
}

func HandlePreReceive(ctx context.Context, opts HookOptions) error {
	opts = normalizeHookOptions(opts)
	updates, err := readRefUpdates(opts.Stdin)
	if err != nil {
		return err
	}

	principal := hookPrincipal(opts)
	for _, update := range updates {
		if err := validateRefUpdate(ctx, opts.ExchangeRepoPath, opts.BaseBranch, principal, update); err != nil {
			fmt.Fprintf(opts.Stderr, "flow pre-receive rejected %s: %v\n", update.Ref, err)
			return err
		}
	}

	return nil
}

func HandlePostReceive(_ context.Context, opts HookOptions) error {
	opts = normalizeHookOptions(opts)
	updates, err := readRefUpdates(opts.Stdin)
	if err != nil {
		return err
	}

	if len(updates) == 0 {
		return nil
	}

	spoolPath := SpoolPath(opts.ExchangeRepoPath)
	if err := os.MkdirAll(filepath.Dir(spoolPath), 0o755); err != nil {
		return fmt.Errorf("create hook event spool: %w", err)
	}
	file, err := os.OpenFile(spoolPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open hook event spool: %w", err)
	}
	defer file.Close()

	actor := hookPrincipal(opts)
	if actor == "" {
		actor = "local"
	}

	encoder := json.NewEncoder(file)
	now := time.Now().UTC()
	for _, update := range updates {
		if err := encoder.Encode(HookEvent{
			OldSHA:     update.OldSHA,
			NewSHA:     update.NewSHA,
			Ref:        update.Ref,
			Actor:      actor,
			ObservedAt: now,
		}); err != nil {
			return fmt.Errorf("write hook event: %w", err)
		}
	}

	return nil
}

func hookPrincipal(opts HookOptions) string {
	if opts.Principal != nil {
		return strings.TrimSpace(*opts.Principal)
	}
	return strings.TrimSpace(os.Getenv("FLOW_GIT_PRINCIPAL"))
}

func ReadSpooledEvents(exchangeRepoPath string) ([]HookEvent, error) {
	file, err := os.Open(SpoolPath(exchangeRepoPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open hook event spool: %w", err)
	}
	defer file.Close()

	var events []HookEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event HookEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode hook event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read hook event spool: %w", err)
	}

	return events, nil
}

func SpoolPath(exchangeRepoPath string) string {
	return filepath.Join(exchangeRepoPath, spoolDirName, spoolFileName)
}

func validateRefUpdate(ctx context.Context, exchangeRepoPath string, baseBranch string, principal string, update RefUpdate) error {
	baseRef := "refs/heads/" + baseBranch
	switch {
	case update.Ref == baseRef:
		return validateBaseUpdate(ctx, exchangeRepoPath, principal, update)
	case issueRefPattern.MatchString(update.Ref):
		return validateIssueUpdate(ctx, exchangeRepoPath, principal, update)
	case strings.HasPrefix(update.Ref, "refs/tags/"):
		if principal != coordinatorActor {
			return errors.New("only the coordinator may update tags")
		}
		return nil
	case strings.HasPrefix(update.Ref, "refs/flow/"):
		if principal != coordinatorActor {
			return errors.New("only the coordinator may update internal flow refs")
		}
		return nil
	default:
		return errors.New("ref is outside Flow-managed namespaces")
	}
}

func validateBaseUpdate(ctx context.Context, exchangeRepoPath string, principal string, update RefUpdate) error {
	if update.NewSHA == zeroSHA {
		return errors.New("protected base branch cannot be deleted")
	}
	if update.OldSHA == zeroSHA {
		// The initial seed is pushed from the client worktree by flow init,
		// so creating the base ref is allowed for the owner (and local
		// same-machine pushes).
		if principal != coordinatorActor && principal != ownerActor && principal != "" {
			return errors.New("protected base branch creation requires owner or coordinator principal")
		}
	} else if principal != coordinatorActor && principal != ownerActor {
		return errors.New("protected base branch updates require owner or coordinator principal")
	}
	if treePathExists(ctx, exchangeRepoPath, update.NewSHA, ".flow/session") {
		return errors.New("protected base branch cannot contain .flow/session")
	}

	return nil
}

func validateIssueUpdate(ctx context.Context, exchangeRepoPath string, principal string, update RefUpdate) error {
	if update.NewSHA == zeroSHA {
		if principal == coordinatorActor {
			return nil
		}
		return errors.New("only the coordinator may delete issue branches")
	}
	if principal == coordinatorActor {
		return nil
	}
	if !issueBranchPrincipalAllowed(principal) {
		return errors.New("issue branch updates require owner, worker, coordinator, or local principal")
	}
	if update.OldSHA == zeroSHA {
		return nil
	}

	fastForward, err := isFastForward(ctx, exchangeRepoPath, update.OldSHA, update.NewSHA)
	if err != nil {
		return err
	}
	if !fastForward {
		return errors.New("non-fast-forward issue branch updates require coordinator principal")
	}

	return nil
}

func issueBranchPrincipalAllowed(principal string) bool {
	return principal == "" ||
		principal == ownerActor ||
		principal == "worker" ||
		strings.HasPrefix(principal, "worker:") ||
		strings.HasPrefix(principal, "session:")
}

func treePathExists(ctx context.Context, exchangeRepoPath string, sha string, path string) bool {
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "ls-tree", "-r", "--name-only", sha, "--", path)
	if err != nil {
		return false
	}

	return strings.TrimSpace(output) != ""
}

func isFastForward(ctx context.Context, exchangeRepoPath string, oldSHA string, newSHA string) (bool, error) {
	exitCode, err := gitExitCode(ctx, "", exchangeRepoPath, nil, "merge-base", "--is-ancestor", oldSHA, newSHA)
	if err != nil {
		return false, fmt.Errorf("check fast-forward: %w", err)
	}

	switch exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base returned exit code %d", exitCode)
	}
}

func readRefUpdates(input io.Reader) ([]RefUpdate, error) {
	scanner := bufio.NewScanner(input)
	var updates []RefUpdate
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid ref update line %q", scanner.Text())
		}
		updates = append(updates, RefUpdate{
			OldSHA: fields[0],
			NewSHA: fields[1],
			Ref:    fields[2],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ref updates: %w", err)
	}

	return updates, nil
}

func normalizeHookOptions(opts HookOptions) HookOptions {
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	return opts
}

func hookScript(command HookCommand, hookName string, exchangePath string, baseBranch string) (string, error) {
	if strings.TrimSpace(command.Path) == "" {
		return "", errors.New("hook command path is required")
	}

	var builder strings.Builder
	builder.WriteString("#!/bin/sh\n")
	for key, value := range command.Env {
		builder.WriteString("export ")
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(shellQuote(value))
		builder.WriteString("\n")
	}
	builder.WriteString("exec ")
	builder.WriteString(shellQuote(command.Path))
	for _, arg := range command.Args {
		builder.WriteString(" ")
		builder.WriteString(shellQuote(arg))
	}
	builder.WriteString(" git-hook ")
	builder.WriteString(shellQuote(hookName))
	builder.WriteString(" --repo ")
	builder.WriteString(shellQuote(exchangePath))
	builder.WriteString(" --base ")
	builder.WriteString(shellQuote(baseBranch))
	builder.WriteString("\n")

	return builder.String(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
