package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

func runGit(ctx context.Context, dir string, gitDir string, env []string, args ...string) (commandResult, error) {
	return runGitTimeout(ctx, gitOpTimeout, dir, gitDir, env, args...)
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
