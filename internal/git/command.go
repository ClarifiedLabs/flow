package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// gitOpTimeout bounds every git subprocess so a hung exchange repo cannot pin
// the merge cascade or the worker indefinitely. A var so tests can shrink it.
var gitOpTimeout = 120 * time.Second

// gitTransferTimeout is the larger budget for full-history transfers (the
// project-init seed push), which legitimately exceed gitOpTimeout on large
// repositories with a remote exchange.
var gitTransferTimeout = 10 * time.Minute

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func gitOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	result, err := runGit(ctx, dir, "", env, args...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(result.stdout), nil
}

func gitBareOutput(ctx context.Context, gitDir string, env []string, args ...string) (string, error) {
	result, err := runGit(ctx, "", gitDir, env, args...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(result.stdout), nil
}

func gitRun(ctx context.Context, dir string, env []string, args ...string) error {
	_, err := runGit(ctx, dir, "", env, args...)
	return err
}

// gitRunTransfer is gitRun with the full-history transfer budget.
func gitRunTransfer(ctx context.Context, dir string, env []string, args ...string) error {
	_, err := runGitTimeout(ctx, gitTransferTimeout, dir, "", env, args...)
	return err
}

func gitBareRun(ctx context.Context, gitDir string, env []string, args ...string) error {
	_, err := runGit(ctx, "", gitDir, env, args...)
	return err
}

func gitExitCode(ctx context.Context, dir string, gitDir string, env []string, args ...string) (int, error) {
	result, err := runGit(ctx, dir, gitDir, env, args...)
	if err == nil {
		return 0, nil
	}

	var exitErr *gitCommandError
	if errors.As(err, &exitErr) {
		return result.exitCode, nil
	}

	return -1, err
}

// WithLockedRefs verifies and locks the expected bare-repository refs for the
// duration of fn. Git's own reference transaction protocol closes the gap
// between a live-ref read and a related durable database decision: competing
// pushes cannot update any locked ref until fn returns and the no-op reference
// transaction commits.
func WithLockedRefs(ctx context.Context, gitDir string, expected map[string]string, fn func(context.Context) error) error {
	gitDir = strings.TrimSpace(gitDir)
	if gitDir == "" {
		return errors.New("git directory is required")
	}
	if len(expected) == 0 {
		return errors.New("at least one expected ref is required")
	}
	if fn == nil {
		return errors.New("locked ref callback is required")
	}

	refs := make([]string, 0, len(expected))
	objects := make(map[string]string, len(expected))
	for ref, sha := range expected {
		ref = strings.TrimSpace(ref)
		sha = strings.TrimSpace(sha)
		if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, " \t\r\n\x00") {
			return fmt.Errorf("invalid ref %q", ref)
		}
		if sha == "" || strings.ContainsAny(sha, " \t\r\n\x00") || strings.HasPrefix(sha, "-") {
			return fmt.Errorf("invalid expected object for %s", ref)
		}
		if _, exists := objects[ref]; exists {
			return fmt.Errorf("duplicate expected ref %s", ref)
		}
		objects[ref] = sha
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	lockCtx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	gitArgs := []string{"--git-dir", gitDir, "update-ref", "--stdin"}
	// Do not bind the lock-holder process directly to lockCtx. Once prepare
	// succeeds, the process must retain its ref locks until the callback has
	// observed cancellation and returned; otherwise its durable writes could
	// outlive the lock fence.
	cmd := exec.Command("git", gitArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open git reference transaction input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git reference transaction output: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git reference transaction: %w", err)
	}
	var processState sync.Mutex
	prepared := false
	commandDone := make(chan struct{})
	defer close(commandDone)
	go func() {
		select {
		case <-lockCtx.Done():
			processState.Lock()
			if !prepared {
				_ = cmd.Process.Kill()
			}
			processState.Unlock()
		case <-commandDone:
		}
	}()
	reader := bufio.NewReader(stdout)
	wait := func() error {
		if err := stdin.Close(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		if err := cmd.Wait(); err != nil {
			if lockCtx.Err() != nil {
				return fmt.Errorf("git %s timed out: %w", strings.Join(gitArgs, " "), lockCtx.Err())
			}
			exitCode := -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			return &gitCommandError{args: gitArgs, stderr: stderr.String(), exitCode: exitCode, err: err}
		}
		return nil
	}
	fail := func(cause error) error {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return cause
	}
	write := func(command string) error {
		if _, err := io.WriteString(stdin, command+"\n"); err != nil {
			return fmt.Errorf("write git reference transaction: %w", err)
		}
		return nil
	}
	readOK := func(want string) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read git reference transaction response: %w", err)
		}
		if got := strings.TrimSpace(line); got != want {
			return fmt.Errorf("git reference transaction response = %q, want %q", got, want)
		}
		return nil
	}

	if err := write("start"); err != nil {
		return fail(err)
	}
	for _, ref := range refs {
		if err := write("verify " + ref + " " + objects[ref]); err != nil {
			return fail(err)
		}
	}
	if err := write("prepare"); err != nil {
		return fail(err)
	}
	if err := readOK("start: ok"); err != nil {
		return fail(err)
	}
	if err := readOK("prepare: ok"); err != nil {
		// Wait for the process and its stderr-copy goroutine before reading the
		// buffer; preparing the formatted error first races with exec's writer.
		cause := fail(err)
		return fmt.Errorf("lock expected git refs: %w: %s", cause, strings.TrimSpace(stderr.String()))
	}
	processState.Lock()
	if err := lockCtx.Err(); err != nil {
		processState.Unlock()
		return fail(err)
	}
	prepared = true
	processState.Unlock()

	callbackErr := fn(lockCtx)
	terminal := "commit"
	if callbackErr != nil {
		terminal = "abort"
	}
	if err := write(terminal); err != nil {
		if callbackErr != nil {
			return fail(callbackErr)
		}
		return fail(err)
	}
	if err := readOK(terminal + ": ok"); err != nil {
		if callbackErr != nil {
			return fail(callbackErr)
		}
		return fail(err)
	}
	if err := wait(); err != nil && callbackErr == nil {
		return err
	}
	return callbackErr
}

func runGit(ctx context.Context, dir string, gitDir string, env []string, args ...string) (commandResult, error) {
	return runGitTimeout(ctx, gitOpTimeout, dir, gitDir, env, args...)
}

// writeGitOutput streams stdout to dst instead of retaining command output in
// memory. It is used for potentially unbounded Git object data such as patches.
func writeGitOutput(ctx context.Context, dir string, gitDir string, env []string, dst io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()

	gitArgs := gitCommandArgs(dir, gitDir, args)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return gitProcessError(ctx, gitArgs, stderr.String(), err)
	}
	return nil
}

// scanGitOutput streams line-oriented command output through consume instead
// of retaining it in commandResult. Git path output is bounded to one line at a
// time while aggregate numstat output may be arbitrarily large.
func scanGitOutput(ctx context.Context, dir string, gitDir string, env []string, consume func(string) error, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()

	gitArgs := gitCommandArgs(dir, gitDir, args)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git output: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git %s: %w", strings.Join(gitArgs, " "), err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := consume(scanner.Text()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("scan git %s output: %w", strings.Join(gitArgs, " "), err)
	}
	if err := cmd.Wait(); err != nil {
		return gitProcessError(ctx, gitArgs, stderr.String(), err)
	}
	return nil
}

func gitCommandArgs(dir string, gitDir string, args []string) []string {
	gitArgs := make([]string, 0, len(args)+3)
	if gitDir != "" {
		gitArgs = append(gitArgs, "--git-dir", gitDir)
	}
	if dir != "" {
		gitArgs = append(gitArgs, "-C", dir)
	}
	return append(gitArgs, args...)
}

func gitProcessError(ctx context.Context, gitArgs []string, stderr string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git %s timed out: %w", strings.Join(gitArgs, " "), ctxErr)
	}
	exitCode := -1
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	return &gitCommandError{args: gitArgs, stderr: stderr, exitCode: exitCode, err: err}
}

func runGitTimeout(ctx context.Context, timeout time.Duration, dir string, gitDir string, env []string, args ...string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	gitArgs := make([]string, 0, len(args)+3)
	if gitDir != "" {
		gitArgs = append(gitArgs, "--git-dir", gitDir)
	}
	if dir != "" {
		gitArgs = append(gitArgs, "-C", dir)
	}
	gitArgs = append(gitArgs, args...)

	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := commandResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
	}
	if err == nil {
		return result, nil
	}

	// A timeout kill arrives as a SIGKILL ExitError; surface the context error
	// directly so callers see a deadline rather than a masked exit code.
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.exitCode = -1
		return result, fmt.Errorf("git %s timed out: %w", strings.Join(gitArgs, " "), ctxErr)
	}

	exitCode := -1
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	result.exitCode = exitCode

	return result, &gitCommandError{
		args:     gitArgs,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		exitCode: exitCode,
		err:      err,
	}
}

type gitCommandError struct {
	args     []string
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (e *gitCommandError) Error() string {
	output := strings.TrimSpace(e.stderr)
	if output == "" {
		output = strings.TrimSpace(e.stdout)
	}
	if output == "" {
		output = e.err.Error()
	}

	return fmt.Sprintf("git %s failed: %s", strings.Join(e.args, " "), output)
}

func (e *gitCommandError) Unwrap() error {
	return e.err
}
