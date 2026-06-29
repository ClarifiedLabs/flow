package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	"github.com/ClarifiedLabs/flow/internal/terminal"
)

const (
	defaultBaseBranch               = "main"
	pollInterval                    = 100 * time.Millisecond
	watchdogPollInterval            = 2 * time.Second
	defaultWatchdogSilenceThreshold = 5 * time.Minute
	sessionStateReportTimeout       = 2 * time.Second
	sessionStateReportRetryInterval = 30 * time.Second
	jobTerminalRegistrationGrace    = 100 * time.Millisecond
	persistentReconcilePollInterval = 1 * time.Second
	persistentReconcileTimeout      = 2 * time.Second
	maxAckAttempts                  = 3
	ackRetryBackoff                 = 150 * time.Millisecond
	agentReadyMinStartup            = 2 * time.Second
	agentReadyStableChecks          = 3
	agentReadyConfirmChecks         = 2
	terminalStartupPollInterval     = 25 * time.Millisecond
	terminalStartupTimeout          = 2 * time.Second
	terminalProcessStopTimeout      = 2 * time.Second
	tmuxHistoryLimit                = 100000
	claudeHookSettingsFile          = "claude-flow-hooks-settings.json"
	harnessHooksFile                = "harness-flow-hooks.json"
	envClaudeHookSettings           = "FLOW_CLAUDE_HOOK_SETTINGS"
	envHarnessHooks                 = "FLOW_HARNESS_HOOKS"
	defaultUTF8Locale               = "C.UTF-8"
	// clientHookFlowBinary is the command the installed client hooks shell out
	// to. The worker exports `flow` on PATH, so the hook resolves it at runtime
	// rather than baking an absolute path into the worktree.
	clientHookFlowBinary = "flow"
)

var utf8LocaleEnvKeys = []string{"LANG", "LC_ALL", "LC_CTYPE"}

// exitFileAfterSessionExitTimeout is how long the worker waits for the
// entrypoint wrapper's exit-code file after the tmux pane dies. The file is
// written before the wrapper exits, so the wait only pays off when the
// filesystem lags the pane teardown — generous because a loaded machine
// (e.g. the full test suite) can stretch that window well past a second.
//
// It is a var so the forged-exit regression test can shrink the otherwise
// intentional production grace period.
var exitFileAfterSessionExitTimeout = 10 * time.Second

var errPersistentSessionCoordinatorTerminal = errors.New("persistent session is terminal in coordinator")

// errAgentSessionEnded reports that the tmux session disappeared while waiting
// for the agent to be ready for the initial prompt — i.e. the agent process
// exited during startup. The caller skips the paste and lets the normal
// session-exit handling report the early exit rather than masking it.
var errAgentSessionEnded = errors.New("agent session ended before initial prompt")

// gitCloneFetchTimeout bounds the worker's git subprocesses so a hung exchange
// repo cannot pin a job indefinitely. Clones of large repos are legitimately
// slow, so this budget is larger than internal/git's. A var so tests can shrink it.
var gitCloneFetchTimeout = 10 * time.Minute

type Entrypoint struct {
	Argv    []string          `json:"argv"`
	CWD     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Shell   bool              `json:"shell"`
	Harness string            `json:"harness,omitempty"`
}

type JobPayload struct {
	Entrypoint                 *Entrypoint `json:"entrypoint"`
	Branch                     string      `json:"branch"`
	Base                       string      `json:"base"`
	ChangeID                   string      `json:"change_id"`
	HeadSHA                    string      `json:"head_sha"`
	CheckName                  string      `json:"check_name"`
	SessionID                  string      `json:"session_id"`
	SessionPurpose             string      `json:"session_purpose"`
	InjectInitialPrompt        bool        `json:"inject_initial_prompt,omitempty"`
	PromptHarness              string      `json:"prompt_harness,omitempty"`
	ReviewCycleInstructions    string      `json:"review_cycle_instructions,omitempty"`
	HumanAttentionInstructions string      `json:"human_attention_instructions,omitempty"`
	ConsoleScope               string      `json:"console_scope,omitempty"`
	// ImageAttachments is the coordinator-stamped list of image attachment
	// descriptors {id, filename} the worker materializes into .flow/attachments
	// for every author job, regardless of harness. Only the harness CLI receives
	// --image flags (see injectImageFlags); other harnesses get the materialized
	// files but no flag.
	ImageAttachments []coordinator.IssueImageAttachment `json:"image_attachments,omitempty"`
	// AgentHarness / ConsoleHarness are the harness kinds the coordinator stamps
	// on author / console jobs respectively, so the worker reads the stored
	// harness instead of re-deriving it from argv. See resolveHarness.
	AgentHarness   string `json:"agent_harness,omitempty"`
	ConsoleHarness string `json:"console_harness,omitempty"`
	// ExchangeURL, ProjectID, and ProjectName identify the owning project;
	// the coordinator stamps them on every payload so one worker serves all
	// projects, cloning each job's exchange.
	ExchangeURL string `json:"exchange_url,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

// effectiveExchangeURL prefers the payload's per-project exchange and falls
// back to the worker config's pinned URL (single-exchange setups).
func effectiveExchangeURL(payload JobPayload, cfg config.WorkerConfig) string {
	if url := strings.TrimSpace(payload.ExchangeURL); url != "" {
		return url
	}

	return strings.TrimSpace(cfg.Git.ExchangeURL)
}

type RunInput struct {
	Config       config.WorkerConfig
	Job          Job
	Lease        Lease
	Session      *coordinator.Session
	SessionToken string
}

type RunResult struct {
	FinalState JobState
	ExitCode   int
	Session    string
	Worktree   string
	Payload    JobPayload
	// VerdictFilePath is where the check job's structured verdict file
	// (FLOW_VERDICT_FILE) lives. Callers read it after the entrypoint exits to
	// prefer a written verdict over the exit-code mapping.
	VerdictFilePath string
	// TranscriptPath is the local file the worker piped tmux pane output to.
	// Empty when transcript capture could not be started. The caller uploads
	// its tail to the coordinator after the job completes.
	TranscriptPath string
	Err            error
}

func RunJob(ctx context.Context, input RunInput) RunResult {
	slog.Debug("worker job start", "job_id", input.Job.ID, "role", input.Job.Role, "bucket", input.Job.CapacityBucket)
	payload, err := DecodePayload(input.Job.Payload)
	if err != nil {
		return failedResult(input, payload, fmt.Errorf("decode job payload: %w", err))
	}
	if payload.Entrypoint == nil {
		return failedResult(input, payload, errors.New("job payload entrypoint is required"))
	}
	if err := validateEntrypoint(*payload.Entrypoint); err != nil {
		return failedResult(input, payload, err)
	}
	if effectiveExchangeURL(payload, input.Config) == "" {
		return failedResult(input, payload, errors.New("job payload exchange_url (or worker config git.exchange_url) is required to run jobs"))
	}

	worktree, err := prepareWorktree(ctx, input.Config, input.Job, payload, sessionIDForRun(input, payload), input.SessionToken)
	if err != nil {
		return failedResult(input, payload, err)
	}
	slog.Debug("worker worktree prepared", "job_id", input.Job.ID, "worktree", worktree, "branch", payload.Branch, "base", payload.Base)

	if err := materializeImageAttachments(ctx, input, payload, worktree); err != nil {
		slog.Warn("worker image attachment materialization failed", "job_id", input.Job.ID, "error", err)
		// Materialization is best-effort: a download failure must never fail the
		// job. The entrypoint keeps its original argv, and injectImageFlags is a
		// no-op for any image that was not written.
	}

	sessionName := sessionNameForJob(input.Job.ID)
	jobDirectory := jobDir(input.Config.WorkDir, input.Job.ID)
	exitFile := filepath.Join(jobDirectory, "exit-code")
	_ = os.Remove(exitFile)
	workerExitFile, err := privateExitFilePath(jobDirectory)
	if err != nil {
		return failedResult(input, payload, err)
	}
	transcriptFile := filepath.Join(jobDirectory, "transcript.log")
	_ = os.Remove(transcriptFile)

	entrypoint := *payload.Entrypoint
	hookConfigValue, hookConfigEnvVar, err := prepareHookConfig(tmuxInput{
		Config:       input.Config,
		Job:          input.Job,
		Session:      input.Session,
		SessionToken: input.SessionToken,
		Payload:      payload,
		Entrypoint:   entrypoint,
	})
	if err != nil {
		return failedResult(input, payload, err)
	}

	tmuxConfig, err := tmuxConfigForJob(input.Config, input.Job.ID)
	if err != nil {
		return failedResult(input, payload, err)
	}

	exitCode, err := runEntrypointInTmux(ctx, tmuxInput{
		SessionName:      sessionName,
		Worktree:         worktree,
		ExitFile:         exitFile,
		WorkerExitFile:   workerExitFile,
		TranscriptFile:   transcriptFile,
		Config:           tmuxConfig,
		Job:              input.Job,
		Lease:            input.Lease,
		Session:          input.Session,
		SessionToken:     input.SessionToken,
		Payload:          payload,
		Entrypoint:       entrypoint,
		HookConfigValue:  hookConfigValue,
		HookConfigEnvVar: hookConfigEnvVar,
	})
	result := RunResult{
		FinalState:      stateForExit(exitCode, err),
		ExitCode:        exitCode,
		Session:         sessionName,
		Worktree:        worktree,
		Payload:         payload,
		VerdictFilePath: verdictFilePath(input.Config.WorkDir, input.Job.ID),
		TranscriptPath:  transcriptFile,
		Err:             err,
	}
	slog.Debug("worker job finish", "job_id", input.Job.ID, "session", sessionName, "exit_code", exitCode, "final_state", result.FinalState, "error", err)
	return result
}

func DecodePayload(payload map[string]any) (JobPayload, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return JobPayload{}, err
	}

	var decoded JobPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return JobPayload{}, err
	}

	return decoded, nil
}

func failedResult(input RunInput, payload JobPayload, err error) RunResult {
	return RunResult{
		FinalState:      JobFailed,
		ExitCode:        -1,
		Session:         sessionNameForJob(input.Job.ID),
		Worktree:        filepath.Join(jobDir(input.Config.WorkDir, input.Job.ID), "repo"),
		Payload:         payload,
		VerdictFilePath: verdictFilePath(input.Config.WorkDir, input.Job.ID),
		Err:             err,
	}
}

func validateEntrypoint(entrypoint Entrypoint) error {
	if len(entrypoint.Argv) == 0 {
		return errors.New("entrypoint argv is required")
	}
	for _, arg := range entrypoint.Argv {
		if strings.TrimSpace(arg) == "" {
			return errors.New("entrypoint argv entries must not be empty")
		}
	}
	for key := range entrypoint.Env {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(key)), "FLOW_") {
			return fmt.Errorf("entrypoint env cannot override reserved FLOW_* variable %q", key)
		}
		if !validEnvKey(key) {
			return fmt.Errorf("entrypoint env key %q is invalid", key)
		}
	}
	if entrypoint.Shell && len(entrypoint.Argv) != 1 {
		return errors.New("shell entrypoints require exactly one argv command string")
	}
	if filepath.IsAbs(entrypoint.CWD) {
		return errors.New("entrypoint cwd must be relative")
	}
	if strings.Contains(entrypoint.CWD, "..") {
		cleaned := filepath.Clean(entrypoint.CWD)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return errors.New("entrypoint cwd must stay inside the job worktree")
		}
	}

	return nil
}

func prepareWorktree(ctx context.Context, cfg config.WorkerConfig, job Job, payload JobPayload, sessionID string, sessionToken string) (string, error) {
	jobDirectory := jobDir(cfg.WorkDir, job.ID)
	repoDir := filepath.Join(jobDirectory, "repo")
	slog.Debug("worker prepare worktree", "job_id", job.ID, "repo_dir", repoDir)
	if err := os.MkdirAll(jobDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create job directory: %w", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(repoDir); err == nil {
			return "", fmt.Errorf("job worktree exists but is not a git repository: %s", repoDir)
		}
		slog.Debug("worker clone exchange remote", "job_id", job.ID, "repo_dir", repoDir)
		if err := git(ctx, "", cfg, "clone", effectiveExchangeURL(payload, cfg), repoDir); err != nil {
			return "", fmt.Errorf("clone exchange remote: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("stat job worktree: %w", err)
	}

	if err := git(ctx, repoDir, cfg, "fetch", "origin", "--prune"); err != nil {
		return "", fmt.Errorf("fetch exchange remote: %w", err)
	}
	slog.Debug("worker fetched exchange remote", "job_id", job.ID, "repo_dir", repoDir)

	base := strings.TrimSpace(payload.Base)
	if base == "" {
		base = defaultBaseBranch
	}
	branch := strings.TrimSpace(payload.Branch)
	if branch == "" {
		branch = base
	}
	if err := validateBranchName("base", base); err != nil {
		return "", err
	}
	if err := validateBranchName("branch", branch); err != nil {
		return "", err
	}

	source := "origin/" + base
	branchExists := remoteRefExists(ctx, repoDir, cfg, branch)
	if branchExists {
		source = "origin/" + branch
	}
	slog.Debug("worker checkout branch", "job_id", job.ID, "branch", branch, "source", source)
	if err := git(ctx, repoDir, cfg, "checkout", "-B", branch, source); err != nil {
		return "", fmt.Errorf("checkout %s from %s: %w", branch, source, err)
	}
	if headSHA := strings.TrimSpace(payload.HeadSHA); looksLikeCommitSHA(headSHA) {
		slog.Debug("worker checkout requested head", "job_id", job.ID, "branch", branch, "head_sha", headSHA)
		if err := git(ctx, repoDir, cfg, "checkout", "-B", branch, headSHA); err != nil {
			return "", fmt.Errorf("checkout %s at %s: %w", branch, headSHA, err)
		}
	}
	if !branchExists && branch != base {
		slog.Debug("worker push new branch", "job_id", job.ID, "branch", branch)
		if err := git(ctx, repoDir, cfg, "push", "-u", "origin", branch+":"+branch); err != nil {
			return "", fmt.Errorf("push new branch %s: %w", branch, err)
		}
	}

	installClientHooks(repoDir, cfg, job, payload, sessionID, sessionToken)
	excludeFlowArtifactsFromWorktree(repoDir)

	return repoDir, nil
}

// flowWorktreeExcludePatterns are the per-worktree gitignore patterns the
// worker writes to the worktree's .git/info/exclude so Flow session artifacts
// that must live inside the worktree cannot be staged and committed by
// accident by a blanket `git add -A` / `git add .`. info/exclude is local to
// the clone and never appears in the committed diff, mirroring Flow's pattern
// of keeping artifacts out of the change (see verdictFilePath).
//
// The patterns are scoped narrowly: only the materialized-image directory
// (.flow/attachments) is excluded, NOT the whole .flow/ tree. .flow/ is a
// shared Flow namespace that also holds paths authors are expected to commit
// — .flow/checks/*.yaml check definitions (read from the issue branch HEAD by
// checkConfigPrefix in internal/coordinator/check_config.go) and .flow/session
// (a real committed path whose presence on the base branch is guarded in
// internal/git/hooks.go). Excluding all of .flow/ would silently drop those
// from a `git add -A` commit, defeating the check-config workflow.
var flowWorktreeExcludePatterns = []string{
	".flow/attachments/",
}

// excludeFlowArtifactsFromWorktree appends the Flow session artifact patterns to
// the worktree's .git/info/exclude (creating it if absent), skipping any pattern
// already present so repeated prep is idempotent. It is best-effort: a failure
// is logged and never fails worktree prep, since the only consequence is that
// an agent could accidentally stage artifacts that are not part of the change.
func excludeFlowArtifactsFromWorktree(repoDir string) {
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("worker read worktree exclude file", "repo_dir", repoDir, "error", err)
		return
	}
	existingLines := strings.Split(string(existing), "\n")
	present := make(map[string]bool, len(existingLines))
	for _, line := range existingLines {
		present[strings.TrimSpace(line)] = true
	}
	var additions []string
	for _, pattern := range flowWorktreeExcludePatterns {
		if !present[pattern] {
			additions = append(additions, pattern)
		}
	}
	if len(additions) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o700); err != nil {
		slog.Warn("worker create worktree exclude dir", "repo_dir", repoDir, "error", err)
		return
	}
	content := strings.Join(existingLines, "\n")
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "# Flow session artifacts: never committed with the change.\n"
	content += strings.Join(additions, "\n") + "\n"
	if err := os.WriteFile(excludePath, []byte(content), 0o600); err != nil {
		slog.Warn("worker write worktree exclude file", "repo_dir", repoDir, "error", err)
	}
}

// installClientHooks installs Flow-managed client-side git hooks into the job
// worktree so capture/steering fires on the agent's natural commit/push. It is
// best-effort: hooks only steer and never gate the "done" judgment, so a
// failure here must never fail worktree prep.
func installClientHooks(repoDir string, cfg config.WorkerConfig, job Job, payload JobPayload, sessionID string, sessionToken string) {
	if !shouldInstallClientHooks(job.Role, sessionID, sessionToken) {
		return
	}
	harnessKind := resolveHarness(tmuxInput{Payload: payload, Entrypoint: payloadEntrypoint(payload)})
	if err := flowgit.InstallClientHooks(repoDir, flowgit.ClientHookInstallOptions{
		HookCommand: flowgit.HookCommand{Path: clientHookFlowBinary},
		HarnessKind: harnessKind,
	}); err != nil {
		slog.Warn("install client git hooks", "job_id", job.ID, "error", err)
	}
}

// shouldInstallClientHooks gates client hooks to author/console jobs backed by a
// live session token, mirroring the native-hook gating. Check jobs
// (reviewer/verifier/ci) never get them: their git activity is machine-driven
// and the capture/steer layer targets the interactive agent.
func shouldInstallClientHooks(role JobRole, sessionID string, sessionToken string) bool {
	return (role == RoleAuthor || role == RoleConsole) &&
		strings.TrimSpace(sessionID) != "" &&
		strings.TrimSpace(sessionToken) != ""
}

func sessionIDForRun(input RunInput, payload JobPayload) string {
	if input.Session != nil && strings.TrimSpace(input.Session.ID) != "" {
		return input.Session.ID
	}
	return payload.SessionID
}

func payloadEntrypoint(payload JobPayload) Entrypoint {
	if payload.Entrypoint != nil {
		return *payload.Entrypoint
	}
	return Entrypoint{}
}

func validateBranchName(kind string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s branch is required", kind)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s branch must not start with '-'", kind)
	}
	if strings.Contains(value, "..") || strings.Contains(value, " ") {
		return fmt.Errorf("%s branch %q is not supported", kind, value)
	}

	return nil
}

func remoteRefExists(ctx context.Context, repoDir string, cfg config.WorkerConfig, branch string) bool {
	err := git(ctx, repoDir, cfg, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	return err == nil
}

func RequireTerminalAttach(cfg config.WorkerConfig) error {
	if _, err := exec.LookPath(ttydCommand(cfg)); err != nil {
		return errors.New("ttyd is required for worker terminal attach")
	}
	if _, _, ok, err := terminalEndpointBase(cfg); err != nil {
		return err
	} else if !ok {
		return errors.New("worker terminal public_base_url is required when coordinator_url is not loopback")
	}

	return nil
}

func git(ctx context.Context, dir string, cfg config.WorkerConfig, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gitCloneFetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	env := append(os.Environ(), "FLOW_GIT_PRINCIPAL="+gitPrincipal(cfg))
	env = append(env, gitHTTPAuthEnv(cfg.Token)...)
	cmd.Env = env
	started := time.Now()
	slog.Debug("worker git command start", "dir", dir, "args", redactedGitArgs(args))
	output, err := cmd.CombinedOutput()
	if err != nil {
		// A timeout kill arrives as a SIGKILL; surface the context error so
		// callers see a deadline rather than a masked signal.
		if ctxErr := ctx.Err(); ctxErr != nil {
			slog.Debug("worker git command timed out", "dir", dir, "args", redactedGitArgs(args), "duration", time.Since(started), "error", ctxErr)
			return fmt.Errorf("git %s timed out: %w", strings.Join(args, " "), ctxErr)
		}
		slog.Debug("worker git command failed", "dir", dir, "args", redactedGitArgs(args), "duration", time.Since(started), "error", err)
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	slog.Debug("worker git command finish", "dir", dir, "args", redactedGitArgs(args), "duration", time.Since(started))

	return nil
}

func redactedGitArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	if len(redacted) >= 3 && redacted[0] == "clone" {
		redacted[1] = "<remote>"
	}
	return redacted
}

func gitPrincipal(cfg config.WorkerConfig) string {
	if strings.TrimSpace(cfg.Git.Principal) != "" {
		return strings.TrimSpace(cfg.Git.Principal)
	}
	if strings.TrimSpace(cfg.WorkerID) != "" {
		return "worker:" + strings.TrimSpace(cfg.WorkerID)
	}

	return "worker"
}

func gitHTTPAuthEnv(token string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}

	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + token,
	}
}

func ttydCommand(cfg config.WorkerConfig) string {
	if path := strings.TrimSpace(cfg.Terminal.TTYDPath); path != "" {
		return path
	}

	return "ttyd"
}

func tmuxCommandContext(ctx context.Context, cfg config.WorkerConfig, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", tmuxCommandArgs(cfg, args...)...)
	cmd.Env = tmuxClientEnv(os.Environ())
	return cmd
}

func tmuxCommand(cfg config.WorkerConfig, args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", tmuxCommandArgs(cfg, args...)...)
	cmd.Env = tmuxClientEnv(os.Environ())
	return cmd
}

func tmuxCommandArgs(cfg config.WorkerConfig, args ...string) []string {
	socketPath := strings.TrimSpace(cfg.Tmux.SocketPath)
	if socketPath == "" {
		return append([]string(nil), args...)
	}

	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "-S", socketPath)
	commandArgs = append(commandArgs, args...)
	return commandArgs
}

func tmuxClientEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "TMUX=") || strings.HasPrefix(value, "TMUX_PANE=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return withDefaultUTF8Locale(filtered)
}

func withDefaultUTF8Locale(env []string) []string {
	result := append([]string(nil), env...)
	present := map[string]bool{}
	for i, value := range result {
		key, rawValue, ok := strings.Cut(value, "=")
		if !ok || !isUTF8LocaleKey(key) {
			continue
		}
		present[key] = true
		if !isUTF8Locale(rawValue) {
			result[i] = key + "=" + defaultUTF8Locale
		}
	}
	for _, key := range utf8LocaleEnvKeys {
		if !present[key] {
			result = append(result, key+"="+defaultUTF8Locale)
		}
	}
	return result
}

func ensureDefaultUTF8Locale(env map[string]string) {
	for _, key := range utf8LocaleEnvKeys {
		if !isUTF8Locale(env[key]) {
			env[key] = defaultUTF8Locale
		}
	}
}

func isUTF8LocaleKey(key string) bool {
	for _, candidate := range utf8LocaleEnvKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func isUTF8Locale(value string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
	return strings.Contains(normalized, "UTF8")
}

func tmuxConfigForJob(cfg config.WorkerConfig, jobID string) (config.WorkerConfig, error) {
	socketPath, err := tmuxSocketPathForJob(cfg, jobID)
	if err != nil {
		return config.WorkerConfig{}, err
	}
	cfg.Tmux.SocketPath = socketPath
	return cfg, nil
}

func tmuxSocketPathForJob(cfg config.WorkerConfig, jobID string) (string, error) {
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return "", errors.New("worker work_dir is required for job tmux socket")
	}
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("job id is required for job tmux socket")
	}
	root := filepath.Join(os.TempDir(), "flow-job-tmux")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create job tmux socket directory: %w", err)
	}
	key := strings.TrimSpace(cfg.WorkDir) + "\x00" + strings.TrimSpace(jobID)
	sum := sha256.Sum256([]byte(key))
	name := "job-" + hex.EncodeToString(sum[:])[:24] + ".sock"
	return filepath.Join(root, name), nil
}

// agentTmuxTmpDirForJob is the TMUX_TMPDIR exported to the entrypoint pane. Any
// tmux client the agent runs without an explicit socket lands on an empty
// per-job server here instead of the job's private server or the operator's
// default server. The hash keeps the derived default socket
// (<dir>/tmux-<uid>/default) under the unix socket path limit.
func agentTmuxTmpDirForJob(cfg config.WorkerConfig, jobID string) (string, error) {
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return "", errors.New("worker work_dir is required for agent tmux tmpdir")
	}
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("job id is required for agent tmux tmpdir")
	}
	root := filepath.Join(os.TempDir(), "flow-job-tmux")
	key := strings.TrimSpace(cfg.WorkDir) + "\x00" + strings.TrimSpace(jobID)
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(root, "a-"+hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create agent tmux tmpdir: %w", err)
	}
	return dir, nil
}

type tmuxInput struct {
	SessionName      string
	Worktree         string
	ExitFile         string
	WorkerExitFile   string
	TranscriptFile   string
	HookConfigValue  string
	HookConfigEnvVar string
	Config           config.WorkerConfig
	Job              Job
	Lease            Lease
	Session          *coordinator.Session
	SessionToken     string
	Payload          JobPayload
	Entrypoint       Entrypoint
}

func runEntrypointInTmux(ctx context.Context, input tmuxInput) (int, error) {
	cwd, err := resolveEntrypointCWD(input.Worktree, input.Entrypoint.CWD)
	if err != nil {
		return -1, err
	}
	if strings.TrimSpace(input.WorkerExitFile) == "" {
		return -1, errors.New("worker exit file path is required")
	}
	agentTmuxTmpDir, err := agentTmuxTmpDirForJob(input.Config, input.Job.ID)
	if err != nil {
		return -1, err
	}
	jobDirectory := jobDir(input.Config.WorkDir, input.Job.ID)
	wrapper, err := writeWrapper(jobDirectory, input.Entrypoint, input.WorkerExitFile, agentTmuxTmpDir, workerPathEnv(input.Entrypoint))
	if err != nil {
		return -1, err
	}
	startGate, err := privateStartGateFilePath(jobDirectory)
	if err != nil {
		return -1, err
	}
	defer os.Remove(startGate)
	bootstrap, err := writeBootstrap(jobDirectory, wrapper, startGate)
	if err != nil {
		return -1, err
	}

	resetTmuxForJob(input.Config, input.SessionName)
	command := append([]string{"new-session", "-d", "-s", input.SessionName, "-c", cwd}, tmuxEnv(input)...)
	command = append(command, "--", shellQuote(bootstrap))

	slog.Debug("worker tmux session start", "job_id", input.Job.ID, "session", input.SessionName, "cwd", cwd, "harness", resolveHarness(input))
	if output, err := tmuxCommandContext(ctx, input.Config, command...).CombinedOutput(); err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return -1, fmt.Errorf("start tmux session: %s: %w", details, err)
		}
		return -1, fmt.Errorf("start tmux session: %w", err)
	}
	defer cleanupTmuxForJob(input.Config, input.SessionName)
	defer cleanupAgentTmuxServer(input.Config, agentTmuxTmpDir)
	if err := configureTmuxForJob(ctx, input.Config, input.SessionName); err != nil {
		return -1, err
	}

	if os.Getenv("FLOW_DISABLE_TRANSCRIPT_CAPTURE") == "" {
		startTranscriptCapture(ctx, input)
	}

	reporter := newSessionStateReporter(input)
	reconciler := newPersistentSessionReconciler(input)
	if reporter != nil || canRegisterJobTerminal(input) {
		terminalProcess, err := startTmuxTerminal(ctx, input.Config, input.SessionName)
		if err != nil {
			slog.Debug("worker tmux terminal start failed", "job_id", input.Job.ID, "session", input.SessionName, "error", err)
		}
		if terminalProcess != nil {
			registered := false
			if reporter != nil {
				registered = reporter.registerTerminal(terminalProcess.targetURL, input.Config.Tmux.SocketPath) || registered
			}
			if canRegisterJobTerminal(input) {
				registered = waitForJobTerminalRegistration(input, terminalProcess.targetURL) || registered
			}
			slog.Debug("worker tmux terminal registration", "job_id", input.Job.ID, "session", input.SessionName, "registered", registered)
			if registered {
				defer terminalProcess.stop()
			} else {
				terminalProcess.stop()
			}
		}
	}
	if err := releaseEntrypointStartGate(startGate); err != nil {
		killTmuxSession(input.Config, input.SessionName)
		return -1, err
	}
	if input.Payload.InjectInitialPrompt {
		if err := injectInitialPrompt(ctx, input); err != nil {
			killTmuxSession(input.Config, input.SessionName)
			return -1, err
		}
	}
	messenger := newSessionMessagePoller(input)
	if err := waitForTmux(ctx, input.Config, input.SessionName, input.WorkerExitFile, reporter, reconciler, messenger); err != nil {
		if errors.Is(err, errPersistentSessionCoordinatorTerminal) {
			slog.Debug("worker tmux session stopped because coordinator session is terminal", "job_id", input.Job.ID, "session", input.SessionName)
			return 0, nil
		}
		return -1, err
	}
	exitCode, err := readExitCode(input.WorkerExitFile)
	if err != nil {
		return -1, err
	}

	return exitCode, nil
}

func privateExitFilePath(directory string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".worker-exit-code-*")
	if err != nil {
		return "", fmt.Errorf("create private worker exit file: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close private worker exit file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove private worker exit file placeholder: %w", err)
	}

	return path, nil
}

func privateStartGateFilePath(directory string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".entrypoint-start-*")
	if err != nil {
		return "", fmt.Errorf("create private entrypoint start gate: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close private entrypoint start gate: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove private entrypoint start gate placeholder: %w", err)
	}

	return path, nil
}

func writeWrapper(directory string, entrypoint Entrypoint, workerExitFile string, agentTmuxTmpDir string, pathEnv string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker directory: %w", err)
	}
	if strings.TrimSpace(agentTmuxTmpDir) == "" {
		return "", errors.New("agent tmux tmpdir is required")
	}
	path := filepath.Join(directory, "run-entrypoint.sh")
	var command strings.Builder
	if entrypoint.Shell {
		command.WriteString("/bin/sh -c ")
		command.WriteString(shellQuote(entrypoint.Argv[0]))
	} else {
		for i, arg := range entrypoint.Argv {
			if i > 0 {
				command.WriteByte(' ')
			}
			command.WriteString(shellQuote(arg))
		}
	}
	// The pane inherits TMUX pointing at the job's private tmux server. Anything
	// the entrypoint runs — including stale checkouts of this repository's own
	// test suite — could reach that server through it and kill the session that
	// hosts the job. Strip the inherited identity and point socketless tmux
	// clients at an empty per-job TMUX_TMPDIR so they cannot reach the job's
	// server or the operator's default server.
	script := `#!/bin/sh
unset TMUX TMUX_PANE
TMUX_TMPDIR=` + shellQuote(agentTmuxTmpDir) + `
export TMUX_TMPDIR
PATH=` + shellQuote(pathEnv) + `
export PATH
` + command.String() + `
code=$?
worker_exit_file=` + shellQuote(workerExitFile) + `
tmp="${worker_exit_file}.$$"
printf '%s\n' "$code" > "$tmp"
mv "$tmp" "$worker_exit_file"
exit "$code"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write entrypoint wrapper: %w", err)
	}

	return path, nil
}

func writeBootstrap(directory string, wrapper string, startGate string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker directory: %w", err)
	}
	if strings.TrimSpace(wrapper) == "" {
		return "", errors.New("entrypoint wrapper path is required")
	}
	if strings.TrimSpace(startGate) == "" {
		return "", errors.New("entrypoint start gate path is required")
	}
	path := filepath.Join(directory, "start-entrypoint.sh")
	script := `#!/bin/sh
entrypoint_start_gate=` + shellQuote(startGate) + `
while [ ! -e "$entrypoint_start_gate" ]; do
  sleep 0.05
done
exec ` + shellQuote(wrapper) + `
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write entrypoint bootstrap: %w", err)
	}

	return path, nil
}

func releaseEntrypointStartGate(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("release entrypoint start gate: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close entrypoint start gate: %w", err)
	}
	return nil
}

// configureTmuxForJob tunes the job's tmux session for a web terminal. Mouse is
// kept on so wheel scrolling works and the pane stays clean of the \e[M
// escape garbage that turning mouse off reintroduces (see 4ce8891). Mouse on
// means tmux owns plain-drag text selection, which drops on mouse-up in a
// browser, so set-clipboard is enabled and terminal-features is extended with a
// `*:clipboard` entry so tmux advertises the clipboard capability and emits an
// OSC 52 clipboard sequence to the outer terminal even when its terminfo does
// not. OSC 52 is only honored by terminals/clients that implement it; the ttyd
// build shipped here has no OSC 52 handler, so it does not forward or auto-copy
// those sequences (a plain drag does not copy in this deployment). The reliable
// copy path that works on every transport is Shift+drag (bypasses tmux
// selection for a native browser selection) followed by Ctrl/Cmd+C; the web UI
// surfaces that as a hint.
func configureTmuxForJob(ctx context.Context, cfg config.WorkerConfig, sessionName string) error {
	session := strings.TrimSpace(sessionName)
	options := [][]string{
		{"set-option", "-g", "mouse", "on"},
		{"set-option", "-g", "history-limit", strconv.Itoa(tmuxHistoryLimit)},
		{"set-option", "-g", "set-clipboard", "on"},
		{"set-option", "-g", "-a", "terminal-features", "*:clipboard"},
		{"set-option", "-t", session, "mouse", "on"},
		{"set-option", "-t", session, "history-limit", strconv.Itoa(tmuxHistoryLimit)},
		{"set-option", "-t", session, "set-clipboard", "on"},
		{"set-option", "-t", session, "-a", "terminal-features", "*:clipboard"},
	}
	for _, args := range options {
		if output, err := tmuxCommandContext(ctx, cfg, args...).CombinedOutput(); err != nil {
			details := strings.TrimSpace(string(output))
			if details != "" {
				return fmt.Errorf("configure tmux %s: %s: %w", strings.Join(args, " "), details, err)
			}
			return fmt.Errorf("configure tmux %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

func workerPathEnv(entrypoint Entrypoint) string {
	if entrypoint.Env != nil {
		if path, ok := entrypoint.Env["PATH"]; ok {
			return path
		}
	}
	return os.Getenv("PATH")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func tmuxEnv(input tmuxInput) []string {
	values := workerEnv(input)
	args := make([]string, 0, len(values)*2)
	for key, value := range values {
		args = append(args, "-e", key+"="+value)
	}

	return args
}

func workerEnv(input tmuxInput) map[string]string {
	env := map[string]string{}
	for key, value := range input.Entrypoint.Env {
		if validEnvKey(key) {
			env[key] = value
		}
	}
	if _, ok := env["PATH"]; !ok {
		if path := os.Getenv("PATH"); strings.TrimSpace(path) != "" {
			env["PATH"] = path
		}
	}
	for _, key := range []string{
		"BASH_ENV",
		"CARGO_HOME",
		"CODEX_HOME",
		"DOCKER_HOST",
		"JAVA_HOME",
		"LANG",
		"LC_ALL",
		"LC_CTYPE",
		"NVM_DIR",
		"RUSTUP_HOME",
		"XDG_RUNTIME_DIR",
	} {
		if _, ok := env[key]; !ok {
			if value := strings.TrimSpace(os.Getenv(key)); value != "" {
				env[key] = value
			}
		}
	}
	ensureDefaultUTF8Locale(env)
	scrubWorkerDeploymentEnv(env)
	reserved := map[string]string{
		"FLOW_COORDINATOR_URL":  input.Config.CoordinatorURL,
		"FLOW_PROTOCOL_VERSION": input.Config.ProtocolVersion,
		"FLOW_JOB_ID":           input.Job.ID,
		"FLOW_LEASE_ID":         input.Lease.ID,
		"FLOW_ROLE":             string(input.Job.Role),
		"FLOW_WORKER_ROLE":      string(input.Job.Role),
		"FLOW_WORKER_HARNESS":   resolveHarness(input),
		"FLOW_WORKER_EXIT_FILE": input.ExitFile,
		"FLOW_VERDICT_FILE":     verdictFilePath(input.Config.WorkDir, input.Job.ID),
	}
	if strings.TrimSpace(input.TranscriptFile) != "" {
		reserved["FLOW_TRANSCRIPT_FILE"] = strings.TrimSpace(input.TranscriptFile)
	}
	if input.Job.IssueID != nil {
		reserved["FLOW_ISSUE_ID"] = *input.Job.IssueID
	}
	if input.Payload.Branch != "" {
		reserved["FLOW_BRANCH"] = input.Payload.Branch
	}
	if input.Payload.Base != "" {
		reserved["FLOW_BASE"] = input.Payload.Base
	}
	if input.Payload.CheckName != "" {
		reserved["FLOW_CHECK_NAME"] = input.Payload.CheckName
	}
	if input.Payload.ChangeID != "" {
		reserved["FLOW_CHANGE_ID"] = input.Payload.ChangeID
	}
	if input.Payload.SessionID != "" {
		reserved["FLOW_SESSION_ID"] = input.Payload.SessionID
	}
	if input.Payload.SessionPurpose != "" {
		reserved["FLOW_SESSION_PURPOSE"] = input.Payload.SessionPurpose
	}
	if strings.TrimSpace(input.Payload.ReviewCycleInstructions) != "" {
		reserved["FLOW_REVIEW_CYCLE_INSTRUCTIONS"] = strings.TrimSpace(input.Payload.ReviewCycleInstructions)
	}
	if strings.TrimSpace(input.Payload.HumanAttentionInstructions) != "" {
		reserved["FLOW_HUMAN_ATTENTION_INSTRUCTIONS"] = strings.TrimSpace(input.Payload.HumanAttentionInstructions)
	}
	if strings.TrimSpace(input.Payload.ConsoleScope) != "" {
		reserved["FLOW_CONSOLE_SCOPE"] = strings.TrimSpace(input.Payload.ConsoleScope)
	}
	if input.Session != nil {
		reserved["FLOW_SESSION_ID"] = input.Session.ID
	}
	if strings.TrimSpace(input.SessionToken) != "" {
		reserved["FLOW_SESSION_TOKEN"] = strings.TrimSpace(input.SessionToken)
	}
	if value := strings.TrimSpace(input.HookConfigValue); value != "" {
		if envVar := strings.TrimSpace(input.HookConfigEnvVar); envVar != "" {
			reserved[envVar] = value
		}
	}
	if input.Payload.ProjectID != "" {
		reserved["FLOW_PROJECT_ID"] = input.Payload.ProjectID
	}
	if input.Payload.ProjectName != "" {
		reserved["FLOW_PROJECT_NAME"] = input.Payload.ProjectName
	}
	if jobUsesWorkerAPI(input.Job.Role) && strings.TrimSpace(input.Config.Token) != "" {
		reserved["FLOW_WORKER_TOKEN"] = strings.TrimSpace(input.Config.Token)
	}
	for key, value := range workerGitAuthEnv(input) {
		reserved[key] = value
	}
	for key, value := range reserved {
		env[key] = value
	}

	return env
}

func scrubWorkerDeploymentEnv(env map[string]string) {
	for _, key := range []string{
		"FLOW_WORKER_CAPACITY_EPHEMERAL",
		"FLOW_WORKER_CAPACITY_PERSISTENT_AGENT",
		"FLOW_WORKER_COORDINATOR_URL",
		"FLOW_WORKER_DOCKERD",
		"FLOW_WORKER_DOCKERD_ARGS",
		"FLOW_WORKER_DOCKERD_LOG",
		"FLOW_WORKER_GIT_EXCHANGE_URL",
		"FLOW_WORKER_GIT_PRINCIPAL",
		"FLOW_WORKER_GIT_URL_REWRITE_FROM",
		"FLOW_WORKER_GIT_URL_REWRITE_TO",
		"FLOW_WORKER_ID",
		"FLOW_WORKER_INTERNAL_BASE_URL",
		"FLOW_WORKER_JOIN_TOKEN",
		"FLOW_WORKER_PROTOCOL_VERSION",
		"FLOW_WORKER_TERMINAL_BIND_ADDRESS",
		"FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL",
		"FLOW_WORKER_TERMINAL_TTYD_PATH",
		"FLOW_WORKER_TMUX_SOCKET_PATH",
		"FLOW_WORKER_TOKEN",
		"FLOW_WORKER_WORK_DIR",
	} {
		env[key] = ""
	}
}

func workerGitAuthEnv(input tmuxInput) map[string]string {
	if !strings.HasPrefix(strings.TrimSpace(input.Payload.ExchangeURL), "http://") &&
		!strings.HasPrefix(strings.TrimSpace(input.Payload.ExchangeURL), "https://") {
		return nil
	}
	token := strings.TrimSpace(input.SessionToken)
	if token == "" {
		token = strings.TrimSpace(input.Config.Token)
	}
	if token == "" {
		return nil
	}

	return map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Bearer " + token,
	}
}

// resolveHarness reports the harness kind for a job. It prefers the explicitly
// stored harness (the entrypoint's stamped harness, then the coordinator's
// agent_harness / console_harness payload fields) and only falls back to the
// argv heuristic for unmanaged entrypoints that carry no stored harness.
func resolveHarness(input tmuxInput) string {
	if harness := flowharness.NormalizeName(input.Entrypoint.Harness); harness != "" {
		return harness
	}
	if harness := flowharness.NormalizeName(input.Payload.AgentHarness); harness != "" {
		return harness
	}
	if harness := flowharness.NormalizeName(input.Payload.ConsoleHarness); harness != "" {
		return harness
	}
	return flowharness.DetectEntrypointHarness(input.Entrypoint.Argv)
}

// promptConventionHarness resolves which harness convention the initial prompt
// should follow. The explicit prompt_harness payload (which may legitimately be
// "agents") takes precedence over the stored agent harness so the deliberate
// prompt-vs-agent distinction is preserved.
func promptConventionHarness(input tmuxInput) string {
	if harness := strings.TrimSpace(input.Payload.PromptHarness); harness != "" {
		return harness
	}
	return resolveHarness(input)
}

func jobUsesWorkerAPI(role JobRole) bool {
	return role == RoleReviewer || role == RoleVerifier
}

// prepareHookConfig renders and installs the native-hook config for the input's
// harness, returning the value to export and the env var that carries it. Codex
// is installed as a managed `codex --profile` file under $CODEX_HOME (the env
// value is the profile name); claude and harness are written as per-job settings
// files (the env value is the file path). It returns empty strings for jobs that
// do not warrant managed hooks. This replaced the per-harness
// prepareClaudeHookSettings / prepareHarnessHooks pair.
func prepareHookConfig(input tmuxInput) (value string, envVar string, err error) {
	if !shouldPrepareHookConfig(input) {
		return "", "", nil
	}
	switch resolveHarness(input) {
	case flowharness.Codex:
		return prepareCodexHookConfig(input)
	default:
		return prepareHookConfigFile(input)
	}
}

// prepareHookConfigFile writes a per-job native-hook settings file (claude,
// harness) from the table-driven renderer and returns its path plus the env var
// that points the harness at it.
func prepareHookConfigFile(input tmuxInput) (string, string, error) {
	definition, ok := flowharness.Lookup(resolveHarness(input))
	if !ok || definition.HookEnvVar == "" {
		return "", "", nil
	}
	data, err := flowharness.RenderHookConfig(definition)
	if err != nil {
		return "", "", fmt.Errorf("render %s hook config: %w", definition.Name, err)
	}
	path := filepath.Join(jobDir(input.Config.WorkDir, input.Job.ID), hookConfigFileName(definition.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", fmt.Errorf("create %s hook config directory: %w", definition.Name, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", "", fmt.Errorf("write %s hook config: %w", definition.Name, err)
	}
	return path, definition.HookEnvVar, nil
}

// prepareCodexHookConfig writes codex's managed hook profile to
// $CODEX_HOME/<profile>.config.toml (atomically, 0600) and returns the profile
// name plus FLOW_CODEX_HOOK_PROFILE so the codex command builders pass it to
// `codex --profile`. The profile content is deterministic, so concurrent jobs on
// one worker safely converge on the same file.
func prepareCodexHookConfig(input tmuxInput) (string, string, error) {
	definition, ok := flowharness.Lookup(flowharness.Codex)
	if !ok || definition.HookEnvVar == "" {
		return "", "", nil
	}
	data, err := flowharness.RenderHookConfig(definition)
	if err != nil {
		return "", "", fmt.Errorf("render codex hook profile: %w", err)
	}
	home := codexHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", "", fmt.Errorf("create codex home: %w", err)
	}
	path := filepath.Join(home, flowharness.CodexHookProfileName+".config.toml")
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return "", "", fmt.Errorf("write codex hook profile: %w", err)
	}
	return flowharness.CodexHookProfileName, definition.HookEnvVar, nil
}

// codexHome resolves codex's config home the way codex does: $CODEX_HOME, or
// ~/.codex when unset.
func codexHome() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex")
	}
	return ".codex"
}

// writeFileAtomic writes data to path via a temp file in the same directory and a
// rename, so a concurrent reader (or competing writer of identical bytes) never
// observes a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// shouldPrepareHookConfig gates managed-hook rendering to interactive author and
// console jobs that carry the session identity native hooks report against.
func shouldPrepareHookConfig(input tmuxInput) bool {
	return (input.Job.Role == RoleAuthor || input.Job.Role == RoleConsole) &&
		strings.TrimSpace(tmuxInputSessionID(input)) != "" &&
		strings.TrimSpace(input.SessionToken) != ""
}

func hookConfigFileName(harnessName string) string {
	switch harnessName {
	case flowharness.Claude:
		return claudeHookSettingsFile
	case flowharness.Harness:
		return harnessHooksFile
	default:
		return harnessName + "-flow-hooks"
	}
}

func tmuxInputSessionID(input tmuxInput) string {
	if input.Session != nil && strings.TrimSpace(input.Session.ID) != "" {
		return input.Session.ID
	}
	return input.Payload.SessionID
}

func resolveEntrypointCWD(worktree string, cwd string) (string, error) {
	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path: %w", err)
	}
	target := resolvedWorktree
	if strings.TrimSpace(cwd) != "" {
		target = filepath.Join(resolvedWorktree, filepath.Clean(cwd))
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve entrypoint cwd: %w", err)
	}
	relative, err := filepath.Rel(resolvedWorktree, resolvedTarget)
	if err != nil {
		return "", fmt.Errorf("check entrypoint cwd: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("entrypoint cwd must stay inside the job worktree")
	}

	return resolvedTarget, nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var commitSHAPattern = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func validEnvKey(key string) bool {
	return envKeyPattern.MatchString(key)
}

func looksLikeCommitSHA(value string) bool {
	return commitSHAPattern.MatchString(strings.TrimSpace(value))
}

func waitForTmux(ctx context.Context, cfg config.WorkerConfig, sessionName string, exitFile string, reporter *sessionStateReporter, reconciler *persistentSessionReconciler, messenger *sessionMessagePoller) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	watchdog := newTmuxWatchdogWithConfig(cfg, sessionName, defaultWatchdogSilenceThreshold)
	nextWatchdogAt := time.Time{}
	nextReconcileAt := time.Time{}
	nextMessageAt := time.Time{}
	for {
		if entrypointExitRecorded(exitFile) {
			_ = tmuxCommandContext(ctx, cfg, "kill-session", "-t", sessionName).Run()
			return nil
		}
		now := time.Now().UTC()
		if reconciler != nil && (nextReconcileAt.IsZero() || !now.Before(nextReconcileAt)) {
			if reconciler.coordinatorTerminal(ctx) {
				killTmuxSession(cfg, sessionName)
				return errPersistentSessionCoordinatorTerminal
			}
			nextReconcileAt = now.Add(persistentReconcilePollInterval)
		}
		if messenger != nil && (nextMessageAt.IsZero() || !now.Before(nextMessageAt)) {
			messenger.deliver(ctx, cfg, sessionName)
			nextMessageAt = now.Add(persistentReconcilePollInterval)
		}
		if !tmuxSessionExists(ctx, cfg, sessionName) {
			return waitForEntrypointExitFile(ctx, exitFile, exitFileAfterSessionExitTimeout)
		}
		if tmuxPaneDead(ctx, cfg, sessionName) {
			if err := waitForEntrypointExitFile(ctx, exitFile, exitFileAfterSessionExitTimeout); err != nil {
				return err
			}
			killTmuxSession(cfg, sessionName)
			return nil
		}
		if nextWatchdogAt.IsZero() || !now.Before(nextWatchdogAt) {
			observeTmuxSession(ctx, watchdog, reporter, now)
			nextWatchdogAt = now.Add(watchdogPollInterval)
		}
		select {
		case <-ctx.Done():
			killTmuxSession(cfg, sessionName)
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForEntrypointExitFile(ctx context.Context, path string, timeout time.Duration) error {
	if entrypointExitRecorded(path) {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("entrypoint exit file path is required")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("entrypoint exit file %q was not recorded before tmux session exited", path)
		case <-ticker.C:
			if entrypointExitRecorded(path) {
				return nil
			}
		}
	}
}

func entrypointExitRecorded(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func killTmuxSession(cfg config.WorkerConfig, sessionName string) {
	if strings.TrimSpace(sessionName) == "" {
		return
	}
	_ = tmuxCommand(cfg, "kill-session", "-t", sessionName).Run()
}

func resetTmuxForJob(cfg config.WorkerConfig, sessionName string) {
	if strings.TrimSpace(cfg.Tmux.SocketPath) != "" {
		cleanupTmuxServer(cfg)
		return
	}
	killTmuxSession(cfg, sessionName)
}

func cleanupTmuxForJob(cfg config.WorkerConfig, sessionName string) {
	if strings.TrimSpace(cfg.Tmux.SocketPath) != "" {
		cleanupTmuxServer(cfg)
		return
	}
	killTmuxSession(cfg, sessionName)
}

func cleanupTmuxServer(cfg config.WorkerConfig) {
	socketPath := strings.TrimSpace(cfg.Tmux.SocketPath)
	if socketPath == "" {
		return
	}
	_ = tmuxCommand(cfg, "kill-server").Run()
	_ = os.Remove(socketPath)
}

// cleanupAgentTmuxServer kills any server the entrypoint started on the
// per-job TMUX_TMPDIR default socket so agent-spawned tmux servers do not
// outlive the job.
func cleanupAgentTmuxServer(cfg config.WorkerConfig, agentTmuxTmpDir string) {
	dir := strings.TrimSpace(agentTmuxTmpDir)
	if dir == "" {
		return
	}
	socket := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if _, err := os.Stat(socket); err == nil {
		agentCfg := cfg
		agentCfg.Tmux.SocketPath = socket
		cleanupTmuxServer(agentCfg)
	}
	_ = os.RemoveAll(dir)
}

// startTranscriptCapture wires `tmux pipe-pane` to append the session's pane
// output to a worker-owned log file. The session name and the file path are
// worker-generated (derived from the job id and the worker work dir), never
// from job payload data; the path is still shell-quoted because pipe-pane runs
// its argument through /bin/sh. A failure here is non-fatal: transcript capture
// is best-effort and must not abort the job.
func startTranscriptCapture(ctx context.Context, input tmuxInput) {
	path := strings.TrimSpace(input.TranscriptFile)
	if path == "" {
		return
	}
	// -o appends to the file across pane redraws; cat streams stdin verbatim.
	pipeCommand := "cat >> " + shellQuote(path)
	if output, err := tmuxCommandContext(ctx, input.Config, "pipe-pane", "-o", "-t", input.SessionName, pipeCommand).CombinedOutput(); err != nil {
		slog.Debug("worker tmux pipe-pane failed",
			"job_id", input.Job.ID,
			"session", input.SessionName,
			"error", err,
			"details", strings.TrimSpace(string(output)),
		)
	}
}

func tmuxSessionExists(ctx context.Context, cfg config.WorkerConfig, sessionName string) bool {
	err := tmuxCommandContext(ctx, cfg, "has-session", "-t", sessionName).Run()
	return err == nil
}

func tmuxPaneDead(ctx context.Context, cfg config.WorkerConfig, sessionName string) bool {
	output, err := tmuxCommandContext(ctx, cfg, "display-message", "-p", "-t", sessionName, "#{pane_dead}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "1"
}

type sessionStateClient interface {
	ReportSessionSignal(ctx context.Context, sessionID string, input flowclient.SessionSignalInput) (coordinator.Session, error)
	RegisterSessionTerminal(ctx context.Context, sessionID string, targetURL string, tmuxSocketPath string) (coordinator.SessionTerminal, error)
}

type sessionMessageClient interface {
	ListPendingSessionMessages(ctx context.Context, input flowclient.ListPendingSessionMessagesInput) ([]coordinator.SessionMessage, error)
	MarkSessionMessageDelivered(ctx context.Context, input flowclient.MarkSessionMessageDeliveredInput) (coordinator.SessionMessage, error)
}

type sessionStateReporter struct {
	client        sessionStateClient
	sessionID     string
	last          coordinator.SessionRuntimeState
	lastAttempt   coordinator.SessionRuntimeState
	lastAttemptAt time.Time
}

func newSessionStateReporter(input tmuxInput) *sessionStateReporter {
	if input.Session == nil || strings.TrimSpace(input.Session.ID) == "" || strings.TrimSpace(input.SessionToken) == "" {
		return nil
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       input.Config.CoordinatorURL,
		Token:           strings.TrimSpace(input.SessionToken),
		ProtocolVersion: input.Config.ProtocolVersion,
	})
	if err != nil {
		return nil
	}

	return &sessionStateReporter{
		client:    client,
		sessionID: input.Session.ID,
		last:      input.Session.RuntimeState,
	}
}

func (r *sessionStateReporter) report(state coordinator.SessionRuntimeState) {
	if r == nil || r.client == nil || strings.TrimSpace(r.sessionID) == "" || state == "" {
		return
	}
	now := time.Now().UTC()
	if state == r.lastAttempt && now.Sub(r.lastAttemptAt) < sessionStateReportRetryInterval {
		return
	}
	r.lastAttempt = state
	r.lastAttemptAt = now
	ctx, cancel := context.WithTimeout(context.Background(), sessionStateReportTimeout)
	defer cancel()
	if session, err := r.client.ReportSessionSignal(ctx, r.sessionID, flowclient.SessionSignalInput{
		Signal: coordinator.SessionSignalKind(state),
		Source: coordinator.SessionEventSourceWatchdog,
	}); err == nil {
		r.last = session.RuntimeState
		if session.RuntimeState != "" && session.RuntimeState != state {
			r.lastAttempt = session.RuntimeState
			r.lastAttemptAt = now
		}
	}
}

func (r *sessionStateReporter) registerTerminal(targetURL string, tmuxSocketPath string) bool {
	if r == nil || r.client == nil || strings.TrimSpace(r.sessionID) == "" || strings.TrimSpace(targetURL) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStateReportTimeout)
	defer cancel()
	_, err := r.client.RegisterSessionTerminal(ctx, r.sessionID, targetURL, tmuxSocketPath)
	return err == nil
}

func canRegisterJobTerminal(input tmuxInput) bool {
	return strings.TrimSpace(input.Config.Token) != "" &&
		strings.TrimSpace(input.Job.ID) != "" &&
		strings.TrimSpace(input.Lease.ID) != ""
}

func waitForJobTerminalRegistration(input tmuxInput, targetURL string) bool {
	result := make(chan bool, 1)
	go func() {
		result <- registerJobTerminal(input, targetURL)
	}()
	select {
	case registered := <-result:
		return registered
	case <-time.After(jobTerminalRegistrationGrace):
		return true
	}
}

func registerJobTerminal(input tmuxInput, targetURL string) bool {
	if !canRegisterJobTerminal(input) || strings.TrimSpace(targetURL) == "" {
		return false
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       input.Config.CoordinatorURL,
		Token:           strings.TrimSpace(input.Config.Token),
		ProtocolVersion: input.Config.ProtocolVersion,
	})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStateReportTimeout)
	defer cancel()
	_, err = client.RegisterJobTerminal(ctx, input.Job.ID, input.Lease.ID, targetURL, input.Config.Tmux.SocketPath)
	return err == nil
}

func injectInitialPrompt(ctx context.Context, input tmuxInput) error {
	if input.Session == nil {
		return nil
	}
	prompt, err := renderInitialPrompt(ctx, input)
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("initial prompt is empty")
	}
	harness := promptConventionHarness(input)
	// Wait for the agent TUI to come up before pasting. Pasting straight after
	// new-session races the agent's startup: the Enter that submits the prompt
	// would instead answer the trust prompt, eating the prompt. The watchdog
	// dismisses any visible trust prompt itself so readiness can be reached.
	if err := waitForAgentReady(ctx, input.Config, input.SessionName, harness); err != nil {
		if errors.Is(err, errAgentSessionEnded) {
			// The agent process exited before it was ready for the prompt. There
			// is nothing to paste into; let the normal session-exit handling
			// report the exit instead of masking it as an injection failure.
			return nil
		}
		return err
	}
	return tmuxPasteAndEnter(ctx, input.Config, input.SessionName, prompt)
}

// initialPromptReadyTimeout bounds how long injectInitialPrompt waits for the
// agent TUI to be ready before pasting the initial prompt. It is a var so the
// readiness-gate regression tests can shrink the otherwise generous startup
// budget (cold agents can take several seconds to draw their input box).
var initialPromptReadyTimeout = 30 * time.Second

// waitForAgentReady blocks until the agent TUI is ready to receive the initial
// prompt or the readiness budget expires. It carries its own short-lived
// watchdog so it can dismiss a trust prompt itself (rather than threading a
// messenger through waitForTmux) and never pastes into a trust prompt. On
// timeout it returns an error so the caller kills the session and fails loudly.
func waitForAgentReady(ctx context.Context, cfg config.WorkerConfig, sessionName string, harness string) error {
	watchdog := newTmuxWatchdogWithConfig(cfg, sessionName, defaultWatchdogSilenceThreshold)
	start := time.Now()
	deadline := start.Add(initialPromptReadyTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	const unsetPane = "\x00unset"
	lastPane := unsetPane
	stableChecks := 0
	readyChecks := 0
	// Tracked for timeout telemetry: the most recent pane and whether a trust
	// prompt was up on the last observation. If readiness times out while a trust
	// prompt is still visible, the prompt copy has likely drifted past the
	// matcher; we log the pane head/tail so that drift is diagnosable instead of
	// silently hanging until the budget expires.
	lastObservedPane := ""
	trustPromptUp := false
	for {
		// If the agent process exited before it was ready, the tmux session is
		// gone. Report that distinctly so the caller skips the paste and lets the
		// normal session-exit handling deal with the early exit instead of
		// spinning here until the readiness budget expires.
		if !tmuxSessionExists(ctx, cfg, sessionName) {
			return errAgentSessionEnded
		}
		if pane, err := tmuxPaneContents(ctx, cfg, sessionName); err == nil {
			lastObservedPane = pane
			foreground, _ := tmuxForegroundProcess(ctx, cfg, sessionName)
			// Dismiss any visible trust prompt so the Enter that submits the
			// prompt is never swallowed by it.
			_ = watchdog.approveTrustPrompt(ctx, pane, foreground)
			trustPromptUp = anyTrustPromptVisible(pane)
			switch {
			case trustPromptUp:
				// Trust prompt still up: not ready, and reset both windows so we
				// never count ticks during which input would be swallowed.
				lastPane = pane
				stableChecks = 0
				readyChecks = 0
			case agentReady(pane, foreground, harness):
				// Fast path: the foreground is a recognized agent binary and the
				// input box has drawn. Require a couple of consecutive ready
				// observations so a trust prompt that draws a beat after the agent
				// process appears is still caught before we paste.
				readyChecks++
				if readyChecks >= agentReadyConfirmChecks {
					return nil
				}
			default:
				readyChecks = 0
				// Fallback for agents whose process name tmux does not report as
				// the harness binary (e.g. a shell-wrapped agent): treat the agent
				// as ready once the pane has stopped changing for a short window,
				// after a startup grace long enough for a trust prompt to appear.
				if pane == lastPane {
					stableChecks++
				} else {
					lastPane = pane
					stableChecks = 0
				}
				if stableChecks >= agentReadyStableChecks && !time.Now().Before(start.Add(agentReadyMinStartup)) {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			if trustPromptUp {
				head, tail := panePreviewLines(lastObservedPane, 4)
				slog.Warn("agent readiness timed out with a trust prompt still visible; the harness TUI trust-prompt copy may have drifted past the matcher",
					"session", sessionName, "harness", harness,
					"pane_head", head, "pane_tail", tail)
			}
			return fmt.Errorf("agent for session %q was not ready for the initial prompt within %s", sessionName, initialPromptReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// agentReady reports whether the agent TUI is ready to receive the initial
// prompt: no trust prompt is visible, the foreground process is the agent
// binary (not the bootstrapping shell), and the pane has drawn something. It is
// pure so the readiness gate can be tested without a live tmux server.
func agentReady(pane string, foreground string, harness string) bool {
	if strings.TrimSpace(pane) == "" {
		return false
	}
	if anyTrustPromptVisible(pane) {
		return false
	}
	return foregroundIsAgent(foreground, harness)
}

// foregroundIsAgent reports whether the pane's foreground process is the agent
// binary for the harness (or the node wrapper claude/codex commonly run under),
// distinguishing a ready agent from the bootstrapping wrapper shell.
func foregroundIsAgent(foreground string, harness string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(foreground)))
	if name == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case flowharness.Codex:
		return name == flowharness.Codex || name == "node"
	case flowharness.Claude:
		return name == flowharness.Claude || name == "node"
	case flowharness.Harness:
		return name == flowharness.Harness
	default:
		return name == strings.ToLower(strings.TrimSpace(harness))
	}
}

func renderInitialPrompt(ctx context.Context, input tmuxInput) (string, error) {
	harness := promptConventionHarness(input)
	cwd, err := resolveEntrypointCWD(input.Worktree, input.Entrypoint.CWD)
	if err != nil {
		return "", err
	}
	promptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(promptCtx, resolveFlowBinary(input.Entrypoint), "fetch-prompt", "--harness", harness)
	cmd.Dir = cwd
	env := os.Environ()
	for key, value := range workerEnv(input) {
		env = append(env, key+"="+value)
	}
	cmd.Env = tmuxClientEnv(env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return "", fmt.Errorf("fetch initial prompt: %s: %w", details, err)
		}
		return "", fmt.Errorf("fetch initial prompt: %w", err)
	}

	return strings.TrimRight(string(output), "\r\n"), nil
}

// resolveFlowBinary resolves an absolute path to the `flow` CLI for the initial
// prompt fetch. exec.CommandContext resolves a bare "flow" against the worker's
// own PATH, not the entrypoint's, so the prompt fetch could pick up a different
// flow than the agent sees. Prefer a `flow` on the entrypoint PATH, fall back to
// a sibling of the running worker binary, then the literal "flow" (which keeps
// the previous PATH-relative behavior when nothing better is found).
func resolveFlowBinary(entrypoint Entrypoint) string {
	pathEnv := workerPathEnv(entrypoint)
	for _, dir := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "flow")
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "flow")
		if isExecutableFile(sibling) {
			return sibling
		}
	}

	return "flow"
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func tmuxPasteAndEnter(ctx context.Context, cfg config.WorkerConfig, sessionName string, text string) error {
	load := tmuxCommandContext(ctx, cfg, "load-buffer", "-")
	load.Stdin = strings.NewReader(text)
	if output, err := load.CombinedOutput(); err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("load tmux buffer: %s: %w", details, err)
		}
		return fmt.Errorf("load tmux buffer: %w", err)
	}
	if output, err := tmuxCommandContext(ctx, cfg, "paste-buffer", "-p", "-t", sessionName).CombinedOutput(); err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("paste tmux buffer: %s: %w", details, err)
		}
		return fmt.Errorf("paste tmux buffer: %w", err)
	}
	return tmuxSendKeys(ctx, cfg, sessionName, "Enter")
}

type sessionMessagePoller struct {
	client    sessionMessageClient
	sessionID string
	leaseID   string
	// delivered tracks messages already pasted into the pane this process so an
	// ack-only retry (paste succeeded, ack failed) never re-pastes.
	delivered map[string]bool
	paste     func(ctx context.Context, cfg config.WorkerConfig, sessionName string, text string) error
}

func newSessionMessagePoller(input tmuxInput) *sessionMessagePoller {
	if input.Session == nil ||
		strings.TrimSpace(input.Session.ID) == "" ||
		strings.TrimSpace(input.Lease.ID) == "" ||
		strings.TrimSpace(input.Config.Token) == "" {
		return nil
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       input.Config.CoordinatorURL,
		Token:           strings.TrimSpace(input.Config.Token),
		ProtocolVersion: input.Config.ProtocolVersion,
	})
	if err != nil {
		return nil
	}

	return &sessionMessagePoller{
		client:    client,
		sessionID: input.Session.ID,
		leaseID:   input.Lease.ID,
		delivered: make(map[string]bool),
		paste:     tmuxPasteAndEnter,
	}
}

func (p *sessionMessagePoller) deliver(ctx context.Context, cfg config.WorkerConfig, sessionName string) {
	if p == nil || p.client == nil {
		return
	}
	pollCtx, cancel := context.WithTimeout(ctx, sessionStateReportTimeout)
	defer cancel()
	messages, err := p.client.ListPendingSessionMessages(pollCtx, flowclient.ListPendingSessionMessagesInput{
		SessionID: p.sessionID,
		LeaseID:   p.leaseID,
	})
	if err != nil {
		return
	}
	for _, message := range messages {
		if p.delivered[message.ID] {
			// Already pasted this process; the message is only still pending
			// because the ack failed. Retry the ack only — never re-paste.
			p.ackDelivered(ctx, message.ID)
			continue
		}
		if err := p.paste(ctx, cfg, sessionName, formatSessionMessageForAgent(message)); err != nil {
			slog.Warn("worker session message paste failed",
				"session_id", p.sessionID,
				"message_id", message.ID,
				"error", err,
			)
			// Leave the message unmarked so it is retried next tick, and keep
			// delivering the rest of the batch.
			continue
		}
		// Record the paste before acking so a failed ack retries ack-only.
		p.delivered[message.ID] = true
		p.ackDelivered(ctx, message.ID)
	}
}

// ackDelivered marks a pasted message delivered with bounded retry/backoff. An
// already-delivered response (sql.ErrNoRows, or the equivalent client 400) is
// treated as success because the worker-local delivered map already guarantees
// no re-paste; the goal is only to clear the server's pending row.
func (p *sessionMessagePoller) ackDelivered(ctx context.Context, messageID string) {
	for attempt := 0; attempt < maxAckAttempts; attempt++ {
		ackCtx, cancel := context.WithTimeout(ctx, sessionStateReportTimeout)
		_, err := p.client.MarkSessionMessageDelivered(ackCtx, flowclient.MarkSessionMessageDeliveredInput{
			SessionID: p.sessionID,
			MessageID: messageID,
			LeaseID:   p.leaseID,
		})
		cancel()
		if err == nil || alreadyDelivered(err) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ackRetryBackoff):
		}
	}
}

// alreadyDelivered reports whether the ack failed because the message was no
// longer pending (already delivered / non-pending). The coordinator returns
// sql.ErrNoRows for that case, which the HTTP client surfaces as a 400.
func alreadyDelivered(err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	var statusErr *flowclient.HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusBadRequest
	}
	return false
}

func formatSessionMessageForAgent(message coordinator.SessionMessage) string {
	body := strings.TrimSpace(message.Body)
	if body == "" {
		body = "(empty human response)"
	}
	return "Human response:\n\n" + body
}

type persistentSessionStatusClient interface {
	WorkerJobStatus(ctx context.Context, input flowclient.WorkerJobStatusInput) (flowclient.WorkerJobStatusResult, error)
}

type persistentSessionReconciler struct {
	client    persistentSessionStatusClient
	jobID     string
	leaseID   string
	sessionID string
	now       func() time.Time
}

func newPersistentSessionReconciler(input tmuxInput) *persistentSessionReconciler {
	if input.Session == nil ||
		strings.TrimSpace(input.Session.ID) == "" ||
		strings.TrimSpace(input.Lease.ID) == "" ||
		strings.TrimSpace(input.Config.Token) == "" {
		return nil
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       input.Config.CoordinatorURL,
		Token:           strings.TrimSpace(input.Config.Token),
		ProtocolVersion: input.Config.ProtocolVersion,
	})
	if err != nil {
		return nil
	}

	return &persistentSessionReconciler{
		client:    client,
		jobID:     input.Job.ID,
		leaseID:   input.Lease.ID,
		sessionID: input.Session.ID,
		now:       sqlitex.UTCNow,
	}
}

func (r *persistentSessionReconciler) coordinatorTerminal(ctx context.Context) bool {
	if r == nil || r.client == nil || strings.TrimSpace(r.leaseID) == "" {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, persistentReconcileTimeout)
	defer cancel()
	status, err := r.client.WorkerJobStatus(checkCtx, flowclient.WorkerJobStatusInput{LeaseID: r.leaseID})
	if err != nil {
		return false
	}
	if status.Job.ID != r.jobID || status.Lease.ID != r.leaseID {
		return false
	}
	if status.Lease.ReleasedAt != nil || !status.Lease.ExpiresAt.After(r.now().UTC()) {
		return true
	}
	if IsTerminalJobState(status.Job.State) {
		return true
	}
	if status.Session != nil && status.Session.ID == r.sessionID && terminalSessionState(status.Session.RuntimeState) {
		return true
	}

	return false
}

func terminalSessionState(state coordinator.SessionRuntimeState) bool {
	switch state {
	case coordinator.SessionFinished, coordinator.SessionCrashed, coordinator.SessionAbandoned:
		return true
	default:
		return false
	}
}

type tmuxWatchdog struct {
	sessionName      string
	tmuxConfig       config.WorkerConfig
	silenceThreshold time.Duration
	lastPane         string
	lastActivityAt   time.Time
	initialized      bool
	// trustPromptApproved is the one-shot latch per scraped harness: once Flow has
	// dismissed a harness's trust prompt it never sends the submit key again, so
	// the keypress can never be swallowed by (or pasted into) a later prompt.
	trustPromptApproved map[string]bool
	sendKeys            func(context.Context, string, ...string) error
}

func newTmuxWatchdogWithConfig(cfg config.WorkerConfig, sessionName string, silenceThreshold time.Duration) *tmuxWatchdog {
	return &tmuxWatchdog{
		sessionName:         sessionName,
		tmuxConfig:          cfg,
		silenceThreshold:    silenceThreshold,
		trustPromptApproved: map[string]bool{},
		sendKeys: func(ctx context.Context, sessionName string, keys ...string) error {
			return tmuxSendKeys(ctx, cfg, sessionName, keys...)
		},
	}
}

func observeTmuxSession(ctx context.Context, watchdog *tmuxWatchdog, reporter *sessionStateReporter, now time.Time) {
	if watchdog == nil {
		return
	}
	pane, err := tmuxPaneContents(ctx, watchdog.tmuxConfig, watchdog.sessionName)
	if err != nil {
		return
	}
	foreground, _ := tmuxForegroundProcess(ctx, watchdog.tmuxConfig, watchdog.sessionName)
	_ = watchdog.approveTrustPrompt(ctx, pane, foreground)
	switch watchdog.observe(now, pane, foreground) {
	case terminal.WatchdogWorking:
		reporter.report(coordinator.SessionWorking)
	case terminal.WatchdogWaiting:
		reporter.report(coordinator.SessionWaiting)
	}
}

// approveTrustPrompt dismisses any visible directory/workspace-trust prompt by
// sending the harness's submit key once. It is driven entirely by the table data
// in harness.Definition: the marker match (TrustPromptVisible) and the
// foreground gate (TrustPromptForegroundAllowed) keep it from typing into an
// unrelated foreground, and the per-harness one-shot latch ensures the submit
// key is sent at most once per harness. It returns true when it dismissed a
// prompt this call.
func (w *tmuxWatchdog) approveTrustPrompt(ctx context.Context, paneContents string, foregroundProcess string) bool {
	if w == nil {
		return false
	}
	for _, def := range flowharness.TrustPromptDefinitions() {
		if w.trustPromptApproved[def.Name] {
			continue
		}
		if !def.TrustPromptForegroundAllowed(foregroundProcess) || !def.TrustPromptVisible(paneContents) {
			continue
		}
		submitKey := def.TrustPromptSubmitKey
		if submitKey == "" {
			submitKey = "Enter"
		}
		if err := w.sendKeys(ctx, w.sessionName, submitKey); err != nil {
			return false
		}
		if w.trustPromptApproved == nil {
			w.trustPromptApproved = map[string]bool{}
		}
		w.trustPromptApproved[def.Name] = true
		return true
	}
	return false
}

// anyTrustPromptVisible reports whether the pane shows the trust prompt of any
// scraped harness, using the same data-driven matcher as the approver. Readiness
// counters reset while this is true so the initial prompt is never pasted into a
// trust dialog.
func anyTrustPromptVisible(paneContents string) bool {
	for _, def := range flowharness.TrustPromptDefinitions() {
		if def.TrustPromptVisible(paneContents) {
			return true
		}
	}
	return false
}

func (w *tmuxWatchdog) observe(now time.Time, paneContents string, foregroundProcess string) terminal.WatchdogDecision {
	if w == nil {
		return terminal.WatchdogNoChange
	}
	if !w.initialized {
		w.lastPane = paneContents
		w.lastActivityAt = now
		w.initialized = true
		return terminal.WatchdogNoChange
	}
	if paneContents != w.lastPane {
		w.lastPane = paneContents
		w.lastActivityAt = now
		return terminal.WatchdogWorking
	}

	busy := foregroundLooksBusy(foregroundProcess)
	decision := terminal.ClassifyWatchdog(terminal.WatchdogObservation{
		TmuxSession:       w.sessionName,
		SilentFor:         now.Sub(w.lastActivityAt),
		SilenceThreshold:  w.silenceThreshold,
		ForegroundProcess: foregroundProcess,
		BusyChildProcess:  busy,
	})
	if decision == terminal.WatchdogWorking {
		w.lastActivityAt = now
	}
	return decision
}

// panePreviewLines returns up to n leading and n trailing non-empty lines of a
// captured pane, joined with a visible separator, for trust-prompt timeout
// telemetry. It drops tmux's blank padding rows so the log shows the real
// prompt copy (head and tail) without dumping the whole pane.
func panePreviewLines(pane string, n int) (head, tail string) {
	lines := make([]string, 0)
	for _, line := range strings.Split(pane, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return "", ""
	}
	headLines := lines
	if len(headLines) > n {
		headLines = headLines[:n]
	}
	tailLines := lines
	if len(tailLines) > n {
		tailLines = tailLines[len(tailLines)-n:]
	}
	const sep = " ⏎ "
	return strings.Join(headLines, sep), strings.Join(tailLines, sep)
}

func tmuxPaneContents(ctx context.Context, cfg config.WorkerConfig, sessionName string) (string, error) {
	output, err := tmuxCommandContext(ctx, cfg, "capture-pane", "-p", "-t", sessionName).Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func tmuxForegroundProcess(ctx context.Context, cfg config.WorkerConfig, sessionName string) (string, error) {
	output, err := tmuxCommandContext(ctx, cfg, "display-message", "-p", "-t", sessionName, "#{pane_current_command}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func tmuxSendKeys(ctx context.Context, cfg config.WorkerConfig, sessionName string, keys ...string) error {
	args := append([]string{"send-keys", "-t", sessionName}, keys...)
	return tmuxCommandContext(ctx, cfg, args...).Run()
}

func foregroundLooksBusy(command string) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}
	name := strings.ToLower(filepath.Base(trimmed))
	switch name {
	case "bash", "claude", "codex", "fish", "harness", "node", "sh", "tmux", "zsh":
		return false
	default:
		return true
	}
}

type tmuxTerminalProcess struct {
	targetURL string
	cmd       *exec.Cmd
}

func startTmuxTerminal(ctx context.Context, cfg config.WorkerConfig, sessionName string) (*tmuxTerminalProcess, error) {
	if err := RequireTerminalAttach(cfg); err != nil {
		return nil, err
	}
	bindAddress, publicBaseURL, ok, err := terminalEndpointBase(cfg)
	if err != nil || !ok {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindAddress, "0"))
	if err != nil {
		return nil, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("terminal listener did not allocate a TCP address")
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		return nil, err
	}

	command := ttydServeCommand(cfg, sessionName, bindAddress, port)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = tmuxClientEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	process := &tmuxTerminalProcess{
		targetURL: terminalURLWithPort(publicBaseURL, port),
		cmd:       cmd,
	}
	if err := waitForTerminalListener(ctx, bindAddress, port); err != nil {
		process.stop()
		return nil, err
	}

	return process, nil
}

func ttydServeCommand(cfg config.WorkerConfig, sessionName string, bindAddress string, port int) []string {
	command := terminal.TTYDServeCommand(sessionName, bindAddress, port)
	command[0] = ttydCommand(cfg)
	socketPath := strings.TrimSpace(cfg.Tmux.SocketPath)
	if socketPath == "" {
		return command
	}
	for i, arg := range command {
		if arg != "tmux" {
			continue
		}
		withSocket := make([]string, 0, len(command)+2)
		withSocket = append(withSocket, command[:i+1]...)
		withSocket = append(withSocket, "-S", socketPath)
		withSocket = append(withSocket, command[i+1:]...)
		return withSocket
	}

	return command
}

func terminalEndpointBase(cfg config.WorkerConfig) (string, string, bool, error) {
	publicBaseURL := strings.TrimRight(strings.TrimSpace(cfg.Terminal.PublicBaseURL), "/")
	if publicBaseURL != "" {
		parsed, err := url.Parse(publicBaseURL)
		if err != nil {
			return "", "", false, err
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", "", false, errors.New("worker terminal public_base_url must use http or https")
		}
		if parsed.User != nil || strings.TrimSpace(parsed.Host) == "" || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", false, errors.New("worker terminal public_base_url must be scheme and host without port, user, query, or fragment")
		}
		bindAddress := strings.TrimSpace(cfg.Terminal.BindAddress)
		if bindAddress == "" {
			bindAddress = parsed.Hostname()
		}
		targetURL := terminalURLWithPort(publicBaseURL, 1)
		if _, err := terminal.NormalizeProxyTargetURL(targetURL); err != nil {
			return "", "", false, err
		}
		if err := validateTerminalBindAddress(bindAddress, parsed.Hostname()); err != nil {
			return "", "", false, err
		}
		return bindAddress, publicBaseURL, true, nil
	}

	parsedCoordinator, err := url.Parse(strings.TrimSpace(cfg.CoordinatorURL))
	if err != nil {
		return "", "", false, err
	}
	host := parsedCoordinator.Hostname()
	if strings.EqualFold(host, "localhost") {
		return "127.0.0.1", "http://127.0.0.1", true, nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return "127.0.0.1", "http://127.0.0.1", true, nil
	}

	return "", "", false, nil
}

func validateTerminalBindAddress(bindAddress string, publicHost string) error {
	bindAddress = strings.TrimSpace(bindAddress)
	if bindAddress == "" {
		return errors.New("worker terminal bind_address is required")
	}
	ip := net.ParseIP(bindAddress)
	if ip == nil {
		return errors.New("worker terminal bind_address must be an IP address")
	}
	if ip.IsUnspecified() {
		publicIP := net.ParseIP(strings.TrimSpace(publicHost))
		if publicIP == nil || !(publicIP.IsLoopback() || publicIP.IsPrivate() || isTailnetIP(publicIP)) {
			return errors.New("worker terminal wildcard bind requires a private or tailnet public_base_url host")
		}
	}

	return nil
}

func isTailnetIP(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}

	return ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

func terminalURLWithPort(publicBaseURL string, port int) string {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"))
	if err != nil {
		return publicBaseURL
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), strconv.Itoa(port))

	return parsed.String()
}

func waitForTerminalListener(ctx context.Context, bindAddress string, port int) error {
	address := terminalReadinessAddress(bindAddress, port)
	waitCtx, cancel := context.WithTimeout(ctx, terminalStartupTimeout)
	defer cancel()

	ticker := time.NewTicker(terminalStartupPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", address, terminalStartupPollInterval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("terminal proxy did not start listening on %s within %s: %w", address, terminalStartupTimeout, lastErr)
		case <-ticker.C:
		}
	}
}

func terminalReadinessAddress(bindAddress string, port int) string {
	host := strings.TrimSpace(bindAddress)
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}

	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (p *tmuxTerminalProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = p.cmd.Process.Kill()
	}
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(terminalProcessStopTimeout):
	}
}

func readExitCode(path string) (int, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return -1, fmt.Errorf("read entrypoint exit code: %w", err)
	}
	trimmed := strings.TrimSpace(string(value))
	switch trimmed {
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	default:
		var code int
		if _, err := fmt.Sscanf(trimmed, "%d", &code); err != nil {
			return -1, fmt.Errorf("parse entrypoint exit code %q: %w", trimmed, err)
		}
		return code, nil
	}
}

func stateForExit(exitCode int, err error) JobState {
	if err != nil {
		return JobFailed
	}
	if exitCode == 0 {
		return JobFinished
	}

	return JobFailed
}

func jobDir(workDir string, jobID string) string {
	return filepath.Join(workDir, "jobs", jobID)
}

// verdictFilePath is the path a check job writes its structured verdict to,
// exported to the entrypoint as FLOW_VERDICT_FILE. It lives in the job work
// directory (not the worktree) so it survives worktree teardown and cannot be
// committed by accident.
func verdictFilePath(workDir string, jobID string) string {
	return filepath.Join(jobDir(workDir, jobID), VerdictFileName)
}

func sessionNameForJob(jobID string) string {
	return terminal.TmuxSessionNameForJob(jobID)
}
