package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
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
	tmuxHistoryLimit                = 100000
	harnessHooksFile                = "harness-flow-hooks.json"
	envHarnessHooks                 = "FLOW_HARNESS_HOOKS"
	defaultUTF8Locale               = "C.UTF-8"
	hermeticHomeDirName             = "home"
	hermeticConfigDirName           = "config"
	hermeticDataDirName             = "data"
	hermeticCacheDirName            = "cache"
	hermeticRuntimeDirName          = "runtime"
	hermeticTempDirName             = "tmp"
	hermeticGoBuildCacheDirName     = "go-build-cache"
	hermeticGoModCacheDirName       = "go-mod-cache"
	hermeticDockerConfigDirName     = "docker"
	hermeticNPMCacheDirName         = "npm-cache"
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
	WorkspaceMode              string      `json:"workspace_mode,omitempty"`
	ArtifactKind               string      `json:"artifact_kind,omitempty"`
	RoleInstructions           string      `json:"role_instructions,omitempty"`
	PhaseName                  string      `json:"phase_name,omitempty"`
	PhaseIndex                 int         `json:"phase_index,omitempty"`
	FinalPhase                 *bool       `json:"final_phase,omitempty"`
	GateFeedback               string      `json:"gate_feedback,omitempty"`
	InjectInitialPrompt        bool        `json:"inject_initial_prompt,omitempty"`
	PromptHarness              string      `json:"prompt_harness,omitempty"`
	ReviewCycleInstructions    string      `json:"review_cycle_instructions,omitempty"`
	HumanAttentionInstructions string      `json:"human_attention_instructions,omitempty"`
	ConsoleScope               string      `json:"console_scope,omitempty"`
	CompletionProtocol         string      `json:"completion_protocol,omitempty"`
	CompletionMode             string      `json:"completion_mode,omitempty"`
	ReviewDiscovery            bool        `json:"review_discovery,omitempty"`
	ReviewAggregation          bool        `json:"review_aggregation,omitempty"`
	// ImageAttachments is the coordinator-stamped list of image attachment
	// descriptors {id, filename} the worker materializes into .flow/attachments
	// for every author job, regardless of harness. Only the harness CLI receives
	// --image flags (see injectImageFlags); other harnesses get the materialized
	// files but no flag.
	ImageAttachments []coordinator.TaskImageAttachment `json:"image_attachments,omitempty"`
	// AgentHarness / ConsoleHarness are the harness kinds the coordinator stamps
	// on author / console jobs respectively, so the worker reads the stored
	// harness instead of re-deriving it from argv. See resolveHarness.
	AgentHarness   string `json:"agent_harness,omitempty"`
	ConsoleHarness string `json:"console_harness,omitempty"`
	// ProjectID and ProjectName identify the owning project. The coordinator
	// stamps them on every payload so one worker serves all projects and derives
	// each exchange URL from its own coordinator URL.
	ProjectID     string                `json:"project_id,omitempty"`
	ProjectName   string                `json:"project_name,omitempty"`
	HistoryResume *HistoryResumePayload `json:"history_resume,omitempty"`
}

// HistoryResumePayload is coordinator-stamped restore metadata. Workers never
// accept archive locations, digests, lineage, or compatibility values from an
// owner request directly.
type HistoryResumePayload struct {
	ID                           string `json:"id"`
	SourceCaptureID              string `json:"source_capture_id"`
	NativeSessionID              string `json:"native_session_id"`
	SessionRelativeDir           string `json:"session_relative_dir,omitempty"`
	HarnessArtifactID            string `json:"harness_artifact_id"`
	HarnessSHA256                string `json:"harness_sha256"`
	WorkspaceArtifactID          string `json:"workspace_artifact_id"`
	WorkspaceSHA256              string `json:"workspace_sha256"`
	RequiredHeadCommit           string `json:"required_head_commit"`
	SourceHarnessBuild           string `json:"source_harness_build"`
	RequiredHarnessSchemaVersion int    `json:"required_harness_schema_version"`
}

func effectiveExchangeURL(payload JobPayload, cfg config.WorkerConfig) (string, error) {
	return flowgit.ExchangeHTTPURL(cfg.CoordinatorURL, payload.ProjectID)
}

type RunInput struct {
	Config          config.WorkerConfig
	Job             Job
	Lease           Lease
	ProjectID       string
	Session         *coordinator.Session
	SessionToken    string
	BeforeExecution func(ExecutionPreparation) error
}

// ExecutionPreparation identifies immutable, attempt-scoped capture sources.
// The callback runs after worktree preparation but before user code starts.
type ExecutionPreparation struct {
	Worktree          string
	TranscriptPath    string
	HarnessName       string
	NativeSessionRoot string
}

type RunResult struct {
	FinalState JobState
	ExitCode   int
	Session    string
	Worktree   string
	Payload    JobPayload
	// VerdictFilePath is where the check job's structured verdict file
	// (FLOW_VERDICT_FILE) lives. Callers read it after the entrypoint exits as
	// the required reviewer/verifier result.
	VerdictFilePath string
	// VerdictReport is populated from the exact bytes authenticated by a valid
	// completion seal. Flow-owned interactive checks use this captured report
	// instead of reopening the verdict file after the harness is stopped.
	VerdictReport *VerdictReport
	// TranscriptPath is the local file the worker piped tmux pane output to.
	// Empty when transcript capture could not be started. The caller uploads
	// its tail to the coordinator after the job completes.
	TranscriptPath     string
	HistoryPreparation ExecutionPreparation
	Err                error
}

var (
	activeJobsMu sync.RWMutex
	activeJobs   = map[string]struct{}{}
)

// RegisterActiveJob marks jobID as active on this worker. The worker control
// channel uses this set to decide whether to accept terminal-open requests.
func RegisterActiveJob(jobID string) {
	activeJobsMu.Lock()
	defer activeJobsMu.Unlock()
	activeJobs[jobID] = struct{}{}
}

// UnregisterActiveJob removes jobID from this worker's active set.
func UnregisterActiveJob(jobID string) {
	activeJobsMu.Lock()
	defer activeJobsMu.Unlock()
	delete(activeJobs, jobID)
}

// IsActiveJob reports whether jobID is currently active on this worker.
func IsActiveJob(jobID string) bool {
	activeJobsMu.RLock()
	defer activeJobsMu.RUnlock()
	_, ok := activeJobs[jobID]
	return ok
}

func RunJob(ctx context.Context, input RunInput) RunResult {
	slog.Debug("worker job start", "job_id", input.Job.ID, "role", input.Job.Role, "bucket", input.Job.CapacityBucket)
	RegisterActiveJob(input.Job.ID)
	defer UnregisterActiveJob(input.Job.ID)

	payload, err := DecodePayload(input.Job.Payload)
	if err != nil {
		return failedResult(input, payload, fmt.Errorf("decode job payload: %w", err))
	}
	if projectID := strings.TrimSpace(input.ProjectID); projectID != "" {
		if payload.ProjectID != "" && payload.ProjectID != projectID {
			return failedResult(input, payload, fmt.Errorf("job payload project id %q does not match claimed project %q", payload.ProjectID, projectID))
		}
		payload.ProjectID = projectID
	}
	if payload.Entrypoint == nil {
		return failedResult(input, payload, errors.New("job payload entrypoint is required"))
	}
	if err := validateEntrypoint(*payload.Entrypoint); err != nil {
		return failedResult(input, payload, err)
	}
	if payload.HistoryResume != nil {
		if err := validateHistoryResumePayload(*payload.HistoryResume); err != nil {
			return failedResult(input, payload, err)
		}
		if strings.TrimSpace(payload.HeadSHA) != payload.HistoryResume.RequiredHeadCommit {
			return failedResult(input, payload, errors.New("history resume job head does not match its restore precondition"))
		}
	}
	if _, err := effectiveExchangeURL(payload, input.Config); err != nil {
		return failedResult(input, payload, fmt.Errorf("derive project exchange url: %w", err))
	}

	entrypoint := *payload.Entrypoint
	harnessName := ""
	if usesManagedNativeHarnessSession(tmuxInput{Payload: payload, Entrypoint: entrypoint}) {
		harnessName = flowharness.Harness
	}
	attemptDirectory := historyAttemptDir(input.Config.WorkDir, input.Job.ID, input.Lease.ID)
	nativeSessionRoot := ""
	if harnessName != "" {
		nativeSessionRoot = filepath.Join(attemptDirectory, "harness-session")
	}
	historyPreparation := ExecutionPreparation{
		Worktree:          HistoryWorktreePath(input.Config.WorkDir, input.Job.ID),
		TranscriptPath:    filepath.Join(attemptDirectory, "transcript.log"),
		HarnessName:       harnessName,
		NativeSessionRoot: nativeSessionRoot,
	}
	if payload.HistoryResume != nil && harnessName == "" {
		return failedResult(input, payload, errors.New("history resume requires the managed native Harness entrypoint"))
	}
	if input.BeforeExecution != nil {
		if err := input.BeforeExecution(historyPreparation); err != nil {
			return failedResult(input, payload, fmt.Errorf("prepare history capture: %w", err))
		}
	}
	if err := os.MkdirAll(attemptDirectory, 0o700); err != nil {
		return failedResult(input, payload, fmt.Errorf("create history attempt directory: %w", err))
	}
	var resumeArchives historyResumeArchives
	if payload.HistoryResume != nil {
		resumeArchives, err = prepareHistoryResumeArchives(ctx, input, *payload.HistoryResume, attemptDirectory)
		if err != nil {
			return failedResult(input, payload, fmt.Errorf("preflight history resume: %w", err))
		}
		defer resumeArchives.cleanup()
	}

	worktree, err := prepareWorktree(ctx, input.Config, input.Job, payload, sessionIDForRun(input, payload), input.SessionToken)
	if err != nil {
		return failedResult(input, payload, err)
	}
	slog.Debug("worker worktree prepared", "job_id", input.Job.ID, "worktree", worktree, "branch", payload.Branch, "base", payload.Base)
	if payload.HistoryResume != nil {
		if err := restoreHistoryResume(ctx, *payload.HistoryResume, resumeArchives, worktree, nativeSessionRoot); err != nil {
			return failedResult(input, payload, fmt.Errorf("restore history resume: %w", err))
		}
	}

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
	transcriptFile := historyPreparation.TranscriptPath
	if err := os.WriteFile(transcriptFile, nil, 0o600); err != nil {
		return failedResult(input, payload, fmt.Errorf("initialize history transcript: %w", err))
	}

	historyPreparation.Worktree = worktree
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

	completionCapture := &checkCompletionCapture{}
	exitCode, err := runEntrypointInTmux(ctx, tmuxInput{
		SessionName:       sessionName,
		Worktree:          worktree,
		ExitFile:          exitFile,
		WorkerExitFile:    workerExitFile,
		TranscriptFile:    transcriptFile,
		Config:            tmuxConfig,
		Job:               input.Job,
		Lease:             input.Lease,
		Session:           input.Session,
		SessionToken:      input.SessionToken,
		Payload:           payload,
		Entrypoint:        entrypoint,
		HookConfigValue:   hookConfigValue,
		HookConfigEnvVar:  hookConfigEnvVar,
		CompletionCapture: completionCapture,
	})
	result := RunResult{
		FinalState:         stateForExit(exitCode, err),
		ExitCode:           exitCode,
		Session:            sessionName,
		Worktree:           worktree,
		Payload:            payload,
		VerdictFilePath:    verdictFilePath(input.Config.WorkDir, input.Job.ID),
		VerdictReport:      completionCapture.report,
		TranscriptPath:     transcriptFile,
		HistoryPreparation: historyPreparation,
		Err:                err,
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
	preparation := ExecutionPreparation{
		Worktree:       HistoryWorktreePath(input.Config.WorkDir, input.Job.ID),
		TranscriptPath: filepath.Join(historyAttemptDir(input.Config.WorkDir, input.Job.ID, input.Lease.ID), "transcript.log"),
	}
	if payload.Entrypoint != nil && usesManagedNativeHarnessSession(tmuxInput{Payload: payload, Entrypoint: *payload.Entrypoint}) {
		preparation.HarnessName = flowharness.Harness
		preparation.NativeSessionRoot = filepath.Join(historyAttemptDir(input.Config.WorkDir, input.Job.ID, input.Lease.ID), "harness-session")
	}
	return RunResult{
		FinalState:         JobFailed,
		ExitCode:           -1,
		Session:            sessionNameForJob(input.Job.ID),
		Worktree:           preparation.Worktree,
		Payload:            payload,
		TranscriptPath:     preparation.TranscriptPath,
		HistoryPreparation: preparation,
		VerdictFilePath:    verdictFilePath(input.Config.WorkDir, input.Job.ID),
		Err:                err,
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
	exchangeURL, err := effectiveExchangeURL(payload, cfg)
	if err != nil {
		return "", fmt.Errorf("derive project exchange url: %w", err)
	}
	slog.Debug("worker prepare worktree", "job_id", job.ID, "repo_dir", repoDir)
	if err := os.MkdirAll(jobDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create job directory: %w", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(repoDir); err == nil {
			return "", fmt.Errorf("job worktree exists but is not a git repository: %s", repoDir)
		}
		slog.Debug("worker clone exchange remote", "job_id", job.ID, "repo_dir", repoDir)
		if err := git(ctx, "", cfg, "clone", exchangeURL, repoDir); err != nil {
			return "", fmt.Errorf("clone exchange remote: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("stat job worktree: %w", err)
	}

	if err := git(ctx, repoDir, cfg, "remote", "set-url", "origin", exchangeURL); err != nil {
		return "", fmt.Errorf("set exchange origin: %w", err)
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

	baseRef := "refs/remotes/origin/" + base
	if err := git(ctx, repoDir, cfg, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		return "", fmt.Errorf("remote-tracking base %q is missing or does not resolve to a commit after fetch", baseRef)
	}

	source := baseRef
	branchExists := remoteRefExists(ctx, repoDir, cfg, branch)
	if branchExists {
		source = "refs/remotes/origin/" + branch
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
// The patterns are scoped narrowly to generated attachments and reserved
// session state, NOT the whole .flow/ tree. Authors must still be able to commit
// .flow/checks/*.yaml definitions (read from the task branch HEAD by
// checkConfigPrefix in internal/coordinator/check_config.go).
var flowWorktreeExcludePatterns = []string{
	".flow/attachments/",
	".flow/session/",
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

func tmuxCommandContext(ctx context.Context, cfg config.WorkerConfig, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", tmuxCommandArgs(cfg, args...)...)
	cmd.Env = terminal.TmuxClientEnv(os.Environ())
	return cmd
}

func tmuxCommand(cfg config.WorkerConfig, args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", tmuxCommandArgs(cfg, args...)...)
	cmd.Env = terminal.TmuxClientEnv(os.Environ())
	return cmd
}

func tmuxCommandArgs(cfg config.WorkerConfig, args ...string) []string {
	socketPath := strings.TrimSpace(cfg.TmuxSocketPath)
	if socketPath == "" {
		return append([]string(nil), args...)
	}

	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "-S", socketPath)
	commandArgs = append(commandArgs, args...)
	return commandArgs
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
	cfg.TmuxSocketPath = socketPath
	return cfg, nil
}

// tmuxRuntimeRoot returns the deterministic, short runtime directory shared by
// this user's per-job tmux servers. Keeping it directly below /tmp prevents a
// long worker work_dir or inherited TMPDIR from overflowing sockaddr_un.sun_path.
// Reusing a valid directory is intentional: deterministic socket paths let the
// worker reap servers left behind after a crash.
func tmuxRuntimeRoot() (string, error) {
	root := filepath.Join("/tmp", fmt.Sprintf("flow-job-tmux-%d", os.Getuid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create tmux runtime directory: %w", err)
	}

	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect tmux runtime directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("tmux runtime path %q is not a directory", root)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("inspect tmux runtime directory owner: unsupported file info for %q", root)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("tmux runtime directory %q is owned by uid %d, want %d", root, stat.Uid, os.Geteuid())
	}
	if permissions := info.Mode().Perm(); permissions != 0o700 {
		return "", fmt.Errorf("tmux runtime directory %q has permissions %04o, want 0700", root, permissions)
	}
	return root, nil
}

func tmuxSocketPathForJob(cfg config.WorkerConfig, jobID string) (string, error) {
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return "", errors.New("worker work_dir is required for job tmux socket")
	}
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("job id is required for job tmux socket")
	}
	root, err := tmuxRuntimeRoot()
	if err != nil {
		return "", err
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
	root, err := tmuxRuntimeRoot()
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(cfg.WorkDir) + "\x00" + strings.TrimSpace(jobID)
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(root, "a-"+hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create agent tmux tmpdir: %w", err)
	}
	return dir, nil
}

type tmuxInput struct {
	SessionName       string
	Worktree          string
	ExitFile          string
	WorkerExitFile    string
	TranscriptFile    string
	HookConfigValue   string
	HookConfigEnvVar  string
	Config            config.WorkerConfig
	Job               Job
	Lease             Lease
	Session           *coordinator.Session
	SessionToken      string
	Payload           JobPayload
	Entrypoint        Entrypoint
	CompletionCapture *checkCompletionCapture
}

type checkCompletionCapture struct {
	report *VerdictReport
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
	if err := ensureHermeticJobEnvironment(input.Config.WorkDir, input.Job.ID); err != nil {
		return -1, err
	}
	if err := installJobHarnessConfig(input.Config, input.Job.ID); err != nil {
		return -1, err
	}
	entrypointEnv := workerEnv(input)
	entrypointEnv["TMUX_TMPDIR"] = agentTmuxTmpDir
	wrapper, err := writeWrapper(jobDirectory, input.Entrypoint, input.WorkerExitFile, entrypointEnv)
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
	command := []string{"new-session", "-d", "-s", input.SessionName, "-c", cwd, "--", envCommand(entrypointEnv, []string{bootstrap})}

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
		registered := false
		if reporter != nil {
			registered = reporter.registerTerminal(input.Config.TmuxSocketPath) || registered
		}
		if canRegisterJobTerminal(input) {
			registered = waitForJobTerminalRegistration(input) || registered
		}
		slog.Debug("worker tmux terminal registration", "job_id", input.Job.ID, "session", input.SessionName, "registered", registered)
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
	completion, err := newCheckCompletionWatcher(input)
	if err != nil {
		return -1, err
	}
	if err := waitForTmux(ctx, input.Config, input.SessionName, input.WorkerExitFile, reporter, reconciler, messenger, completion); err != nil {
		if errors.Is(err, errPersistentSessionCoordinatorTerminal) {
			slog.Debug("worker tmux session stopped because coordinator session is terminal", "job_id", input.Job.ID, "session", input.SessionName)
			return 0, nil
		}
		return -1, err
	}
	if input.CompletionCapture != nil && input.CompletionCapture.report != nil {
		return 0, nil
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

func writeWrapper(directory string, entrypoint Entrypoint, workerExitFile string, entrypointEnv map[string]string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker directory: %w", err)
	}
	path := filepath.Join(directory, "run-entrypoint.sh")
	var argv []string
	if entrypoint.Shell {
		argv = []string{"/bin/sh", "-c", entrypoint.Argv[0]}
	} else {
		argv = append([]string(nil), entrypoint.Argv...)
	}
	command := envCommand(entrypointEnv, argv)
	// The pane inherits tmux's server environment, but the configured entrypoint
	// runs through env -i with Flow's computed job environment. That keeps host
	// home/config/cache variables out of the job while preserving the wrapper's
	// exit-code reporting after the command returns.
	script := `#!/bin/sh
` + command + `
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
// OSC 52 clipboard sequence to the outer terminal. The flow-server terminal
// page forwards those OSC 52 sequences to the browser clipboard
// (internal/web/assets/terminal-clipboard.js); as a result a plain drag-select
// in the web UI copies locally while tmux keeps owning the mouse. Shift+drag
// (bypasses tmux selection for a native browser selection) followed by
// Ctrl/Cmd+C remains the transport-independent fallback — for non-web attach
// such as the CLI `tmux attach` — and the web UI surfaces that as a hint.
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

func envCommand(env map[string]string, argv []string) string {
	var command strings.Builder
	command.WriteString("/usr/bin/env -i")
	for _, assignment := range envAssignments(env) {
		command.WriteByte(' ')
		command.WriteString(shellQuote(assignment))
	}
	for _, arg := range argv {
		command.WriteByte(' ')
		command.WriteString(shellQuote(arg))
	}
	return command.String()
}

func workerEnv(input tmuxInput) map[string]string {
	env := entrypointEnvWithDeploymentDefaults(input)
	scrubWorkerDeploymentEnv(env)
	reserved := map[string]string{
		"FLOW_COORDINATOR_URL":  input.Config.CoordinatorURL,
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
	if usesManagedNativeHarnessSession(input) {
		nativeSessionPath := filepath.Join(historyAttemptDir(input.Config.WorkDir, input.Job.ID, input.Lease.ID), "harness-session")
		if input.Payload.HistoryResume != nil && input.Payload.HistoryResume.SessionRelativeDir != "" {
			nativeSessionPath = filepath.Join(nativeSessionPath, filepath.FromSlash(input.Payload.HistoryResume.SessionRelativeDir))
		}
		reserved["FLOW_HARNESS_SESSION"] = nativeSessionPath
	}
	if input.Job.TaskID != nil {
		reserved["FLOW_TASK_ID"] = *input.Job.TaskID
	}
	if input.Job.WorkflowRunID != nil {
		reserved["FLOW_WORKFLOW_RUN_ID"] = *input.Job.WorkflowRunID
	}
	if input.Job.NodeRunID != nil {
		reserved["FLOW_NODE_RUN_ID"] = *input.Job.NodeRunID
	}
	if input.Payload.WorkspaceMode != "" {
		reserved["FLOW_WORKSPACE_MODE"] = input.Payload.WorkspaceMode
	}
	if input.Payload.ArtifactKind != "" {
		reserved["FLOW_ARTIFACT_KIND"] = input.Payload.ArtifactKind
	}
	if input.Payload.RoleInstructions != "" {
		reserved["FLOW_ROLE_INSTRUCTIONS"] = input.Payload.RoleInstructions
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
	if context, ok := checkCompletionContext(input); ok {
		reserved["FLOW_COMPLETION_PROTOCOL"] = checkverdict.CompletionProtocol
		reserved["FLOW_CHECK_MODE"] = string(context.Mode)
		reserved["FLOW_COMPLETION_FILE"] = completionFilePath(input.Config.WorkDir, input.Job.ID)
	}
	if input.Job.ChangeID != nil {
		reserved["FLOW_CHANGE_ID"] = *input.Job.ChangeID
	}
	if input.Payload.SessionID != "" {
		reserved["FLOW_SESSION_ID"] = input.Payload.SessionID
	}
	if input.Payload.SessionPurpose != "" {
		reserved["FLOW_SESSION_PURPOSE"] = input.Payload.SessionPurpose
	}
	if strings.TrimSpace(input.Payload.PhaseName) != "" {
		reserved["FLOW_PHASE_NAME"] = strings.TrimSpace(input.Payload.PhaseName)
	}
	if input.Payload.FinalPhase != nil {
		reserved["FLOW_PHASE_INDEX"] = strconv.Itoa(input.Payload.PhaseIndex)
		reserved["FLOW_PHASE_FINAL"] = strconv.FormatBool(*input.Payload.FinalPhase)
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

	// Inject the configured git commit identity (if any) as GIT_AUTHOR_*/
	// GIT_COMMITTER_* so the harness and the shells it spawns commit under it.
	// These are not FLOW_WORKER_* keys, so scrubWorkerDeploymentEnv leaves them
	// in place.
	identity := flowgit.CommitIdentity{
		Name:  input.Config.Git.CommitName,
		Email: input.Config.Git.CommitEmail,
	}
	if identity.Configured() {
		for _, assignment := range identity.Env() {
			key, value, _ := strings.Cut(assignment, "=")
			env[key] = value
		}
	}

	return env
}

var agentDeploymentEnvKeys = map[string][]string{
	flowharness.Harness: {
		"HARNESS_MODEL_PROXY_URL",
		"HARNESS_MODEL_PROXY_API_KEY",
	},
}

// forwardAgentDeploymentEnv carries only the selected harness's worker
// authentication configuration across the hermetic env -i boundary. CI and
// non-matching harnesses remain isolated from agent credentials, and an
// entrypoint's explicit env (including an intentional empty value) wins over
// deployment defaults.
func forwardAgentDeploymentEnv(env map[string]string, input tmuxInput) {
	if !jobUsesAgentCredentials(input.Job.Role) {
		return
	}
	for _, key := range agentDeploymentEnvKeys[resolveHarness(input)] {
		if _, explicit := input.Entrypoint.Env[key]; explicit {
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
}

func jobUsesAgentCredentials(role JobRole) bool {
	switch role {
	case RoleAuthor, RoleReviewer, RoleVerifier, RoleConsole:
		return true
	default:
		return false
	}
}

func entrypointEnvWithHermeticDefaults(input tmuxInput) map[string]string {
	env := hermeticJobEnv(input.Config.WorkDir, input.Job.ID)
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
	ensureDefaultUTF8Locale(env)
	return env
}

func entrypointEnvWithDeploymentDefaults(input tmuxInput) map[string]string {
	env := entrypointEnvWithHermeticDefaults(input)
	forwardAgentDeploymentEnv(env, input)
	return env
}

func hermeticJobEnv(workDir string, jobID string) map[string]string {
	root := jobDir(workDir, jobID)
	tempDir := filepath.Join(root, hermeticTempDirName)
	return map[string]string{
		"HOME":             filepath.Join(root, hermeticHomeDirName),
		"XDG_CONFIG_HOME":  filepath.Join(root, hermeticConfigDirName),
		"XDG_DATA_HOME":    filepath.Join(root, hermeticDataDirName),
		"XDG_CACHE_HOME":   filepath.Join(root, hermeticCacheDirName),
		"XDG_RUNTIME_DIR":  filepath.Join(root, hermeticRuntimeDirName),
		"TMPDIR":           tempDir,
		"TMP":              tempDir,
		"TEMP":             tempDir,
		"GOCACHE":          filepath.Join(root, hermeticGoBuildCacheDirName),
		"GOMODCACHE":       filepath.Join(root, hermeticGoModCacheDirName),
		"DOCKER_CONFIG":    filepath.Join(root, hermeticDockerConfigDirName),
		"NPM_CONFIG_CACHE": filepath.Join(root, hermeticNPMCacheDirName),
	}
}

func ensureHermeticJobEnvironment(workDir string, jobID string) error {
	seen := map[string]bool{}
	for _, path := range hermeticJobEnv(workDir, jobID) {
		if strings.TrimSpace(path) == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create hermetic job environment directory %s: %w", path, err)
		}
	}
	return nil
}

// installJobHarnessConfig copies the worker-configured Harness JSON config
// into the job's hermetic HOME as Harness's global config
// ($HOME/.config/harness/config.json). Harness merges flags > env > project
// .harness/config.json > this global file > built-in defaults, so the file
// supplies deployment-wide defaults only: a repo's project config still wins
// per key. The source may carry credentials; it is copied, never logged.
func installJobHarnessConfig(cfg config.WorkerConfig, jobID string) error {
	source := strings.TrimSpace(cfg.HarnessConfigFile)
	if source == "" {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read worker harness_config_file %s: %w", source, err)
	}
	target := filepath.Join(hermeticJobEnv(cfg.WorkDir, jobID)["HOME"], ".config", "harness", "config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create job Harness config directory: %w", err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("write job Harness config %s: %w", target, err)
	}
	return nil
}

func envAssignments(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		if validEnvKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	assignments := make([]string, 0, len(keys))
	for _, key := range keys {
		assignments = append(assignments, key+"="+env[key])
	}
	return assignments
}

func scrubWorkerDeploymentEnv(env map[string]string) {
	for _, key := range []string{
		"FLOW_WORKER_CAPACITY_EPHEMERAL",
		"FLOW_WORKER_CAPACITY_PERSISTENT_AGENT",
		"FLOW_WORKER_COORDINATOR_URL",
		"FLOW_WORKER_DOCKERD",
		"FLOW_WORKER_DOCKERD_ARGS",
		"FLOW_WORKER_DOCKERD_LOG",
		"FLOW_WORKER_GIT_PRINCIPAL",
		"FLOW_WORKER_ID",
		"FLOW_WORKER_JOIN_TOKEN",
		"FLOW_WORKER_TMUX_SOCKET_PATH",
		"FLOW_WORKER_TOKEN",
		"FLOW_WORKER_WORK_DIR",
	} {
		env[key] = ""
	}
}

func workerGitAuthEnv(input tmuxInput) map[string]string {
	if strings.TrimSpace(input.Payload.WorkspaceMode) == "base" {
		return nil
	}
	exchangeURL, err := effectiveExchangeURL(input.Payload, input.Config)
	if err != nil || (!strings.HasPrefix(exchangeURL, "http://") && !strings.HasPrefix(exchangeURL, "https://")) {
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
func usesManagedNativeHarnessSession(input tmuxInput) bool {
	if resolveHarness(input) != flowharness.Harness || !input.Entrypoint.Shell || len(input.Entrypoint.Argv) != 1 {
		return false
	}
	return strings.Contains(input.Entrypoint.Argv[0], `harness --session "$FLOW_HARNESS_SESSION"`)
}

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
// harness as a per-job settings file (the env value is the file path). It
// returns empty strings for jobs that do not warrant managed hooks.
func prepareHookConfig(input tmuxInput) (value string, envVar string, err error) {
	if !shouldPrepareHookConfig(input) {
		return "", "", nil
	}
	return prepareHookConfigFile(input)
}

// prepareHookConfigFile writes a per-job native-hook settings file from the
// table-driven renderer and returns its path plus the env var that points the
// harness at it.
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

// shouldPrepareHookConfig gates managed-hook rendering to interactive author and
// console jobs that carry the session identity native hooks report against.
func shouldPrepareHookConfig(input tmuxInput) bool {
	return (input.Job.Role == RoleAuthor || input.Job.Role == RoleConsole) &&
		strings.TrimSpace(tmuxInputSessionID(input)) != "" &&
		strings.TrimSpace(input.SessionToken) != ""
}

func hookConfigFileName(harnessName string) string {
	switch harnessName {
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

type checkCompletionWatcher struct {
	sealPath    string
	verdictPath string
	context     checkverdict.Context
	capture     *checkCompletionCapture
	lastError   string
}

func newCheckCompletionWatcher(input tmuxInput) (*checkCompletionWatcher, error) {
	context, ok := checkCompletionContext(input)
	if !ok {
		if strings.TrimSpace(input.Payload.CompletionProtocol) == checkverdict.CompletionProtocol {
			return nil, fmt.Errorf("check completion protocol is not valid for job role %q", input.Job.Role)
		}
		return nil, nil
	}
	if context.JobID == "" || context.CheckName == "" {
		return nil, errors.New("check completion job id and check name are required")
	}
	if strings.TrimSpace(input.Payload.CompletionMode) != string(context.Mode) {
		return nil, fmt.Errorf(
			"check completion mode %q does not match authoritative mode %q",
			input.Payload.CompletionMode,
			context.Mode,
		)
	}
	return &checkCompletionWatcher{
		sealPath:    completionFilePath(input.Config.WorkDir, input.Job.ID),
		verdictPath: verdictFilePath(input.Config.WorkDir, input.Job.ID),
		context:     context,
		capture:     input.CompletionCapture,
	}, nil
}

func checkCompletionContext(input tmuxInput) (checkverdict.Context, bool) {
	if strings.TrimSpace(input.Payload.CompletionProtocol) != checkverdict.CompletionProtocol {
		return checkverdict.Context{}, false
	}
	var mode checkverdict.Mode
	switch input.Job.Role {
	case RoleVerifier:
		mode = checkverdict.ModeVerify
	case RoleReviewer:
		switch {
		case input.Payload.ReviewAggregation:
			mode = checkverdict.ModeReviewAggregation
		case input.Payload.ReviewDiscovery:
			mode = checkverdict.ModeReviewDiscovery
		default:
			mode = checkverdict.ModeReview
		}
	default:
		return checkverdict.Context{}, false
	}
	return checkverdict.Context{
		JobID:     strings.TrimSpace(input.Job.ID),
		CheckName: strings.TrimSpace(input.Payload.CheckName),
		Mode:      mode,
	}, true
}

func (w *checkCompletionWatcher) poll() (bool, error) {
	validated, present, err := checkverdict.VerifySeal(w.sealPath, w.verdictPath, w.context)
	if err != nil || !present {
		return false, err
	}
	if w.capture != nil {
		report := validated.Report
		w.capture.report = &report
	}
	return true, nil
}

func (w *checkCompletionWatcher) logInvalid(err error) {
	if err == nil || err.Error() == w.lastError {
		return
	}
	w.lastError = err.Error()
	slog.Warn("worker ignored invalid check completion seal", "job_id", w.context.JobID, "check_name", w.context.CheckName, "error", err)
}

func waitForTmux(ctx context.Context, cfg config.WorkerConfig, sessionName string, exitFile string, reporter *sessionStateReporter, reconciler *persistentSessionReconciler, messenger *sessionMessagePoller, completion *checkCompletionWatcher) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	watchdog := newTmuxWatchdogWithConfig(cfg, sessionName, defaultWatchdogSilenceThreshold)
	nextWatchdogAt := time.Time{}
	nextReconcileAt := time.Time{}
	nextMessageAt := time.Time{}
	for {
		if completion != nil {
			sealed, err := completion.poll()
			if err != nil {
				completion.logInvalid(err)
			} else if sealed {
				killTmuxSession(cfg, sessionName)
				return nil
			}
		}
		if entrypointExitRecorded(exitFile) {
			_ = tmuxCommandContext(ctx, cfg, "kill-session", "-t", sessionName).Run()
			if completion != nil {
				return errors.New("Flow-owned agent check exited before running flow complete")
			}
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
			if ctx.Err() != nil {
				// The probe was cut short by the caller's context, so the
				// session state is unknown. Report the context error like the
				// select below instead of falsely declaring the session exited.
				killTmuxSession(cfg, sessionName)
				return ctx.Err()
			}
			if completion != nil {
				return errors.New("Flow-owned agent check exited before running flow complete")
			}
			return waitForEntrypointExitFile(ctx, exitFile, exitFileAfterSessionExitTimeout)
		}
		if tmuxPaneDead(ctx, cfg, sessionName) {
			if completion != nil {
				killTmuxSession(cfg, sessionName)
				return errors.New("Flow-owned agent check exited before running flow complete")
			}
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
	if strings.TrimSpace(cfg.TmuxSocketPath) != "" {
		cleanupTmuxServer(cfg)
		return
	}
	killTmuxSession(cfg, sessionName)
}

func cleanupTmuxForJob(cfg config.WorkerConfig, sessionName string) {
	if strings.TrimSpace(cfg.TmuxSocketPath) != "" {
		cleanupTmuxServer(cfg)
		return
	}
	killTmuxSession(cfg, sessionName)
}

func cleanupTmuxServer(cfg config.WorkerConfig) {
	socketPath := strings.TrimSpace(cfg.TmuxSocketPath)
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
		agentCfg.TmuxSocketPath = socket
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
	if err == nil {
		return true
	}
	// tmux's has-session exits 1 when the session (or its server) is gone.
	// Any other probe failure says nothing about the session — most importantly
	// a probe aborted because the caller's context expired or was canceled,
	// which kills the tmux client before it can answer. Presume the session is
	// still alive rather than killing a live check on an inconclusive probe:
	// waitForTmux reports the context error itself on the deadline path, and a
	// genuinely dead session is confirmed by the next probe (exit 1).
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	return true
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
	RegisterSessionTerminal(ctx context.Context, sessionID string, tmuxSocketPath string) (coordinator.SessionTerminal, error)
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
		ServerURL: input.Config.CoordinatorURL,
		Token:     strings.TrimSpace(input.SessionToken),
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

func (r *sessionStateReporter) registerTerminal(tmuxSocketPath string) bool {
	if r == nil || r.client == nil || strings.TrimSpace(r.sessionID) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStateReportTimeout)
	defer cancel()
	_, err := r.client.RegisterSessionTerminal(ctx, r.sessionID, tmuxSocketPath)
	return err == nil
}

func canRegisterJobTerminal(input tmuxInput) bool {
	return strings.TrimSpace(input.Config.Token) != "" &&
		strings.TrimSpace(input.Job.ID) != "" &&
		strings.TrimSpace(input.Lease.ID) != ""
}

func waitForJobTerminalRegistration(input tmuxInput) bool {
	result := make(chan bool, 1)
	go func() {
		result <- registerJobTerminal(input)
	}()
	select {
	case registered := <-result:
		return registered
	case <-time.After(jobTerminalRegistrationGrace):
		return true
	}
}

func registerJobTerminal(input tmuxInput) bool {
	if !canRegisterJobTerminal(input) {
		return false
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL: input.Config.CoordinatorURL,
		Token:     strings.TrimSpace(input.Config.Token),
	})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStateReportTimeout)
	defer cancel()
	_, err = client.RegisterJobTerminal(ctx, input.Job.ID, input.Lease.ID, input.Config.TmuxSocketPath)
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
// prompt or the readiness budget expires. On timeout it returns an error so the
// caller kills the session and fails loudly.
func waitForAgentReady(ctx context.Context, cfg config.WorkerConfig, sessionName string, harness string) error {
	start := time.Now()
	deadline := start.Add(initialPromptReadyTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	const unsetPane = "\x00unset"
	lastPane := unsetPane
	stableChecks := 0
	readyChecks := 0
	for {
		// If the agent process exited before it was ready, the tmux session is
		// gone. Report that distinctly so the caller skips the paste and lets the
		// normal session-exit handling deal with the early exit instead of
		// spinning here until the readiness budget expires.
		if !tmuxSessionExists(ctx, cfg, sessionName) {
			return errAgentSessionEnded
		}
		if pane, err := tmuxPaneContents(ctx, cfg, sessionName); err == nil {
			foreground, _ := tmuxForegroundProcess(ctx, cfg, sessionName)
			if agentReady(pane, foreground, harness) {
				// Fast path: the foreground is a recognized agent binary and the
				// input box has drawn. Require a couple of consecutive ready
				// observations so a slow-drawing TUI is still caught before we paste.
				readyChecks++
				if readyChecks >= agentReadyConfirmChecks {
					return nil
				}
			} else {
				readyChecks = 0
				// Fallback for agents whose process name tmux does not report as
				// the harness binary (e.g. a shell-wrapped agent): treat the agent
				// as ready once the pane has stopped changing for a short window,
				// after a startup grace period.
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
// prompt: the foreground process is the agent binary (not the bootstrapping
// shell) and the pane has drawn something. It is pure so the readiness gate can
// be tested without a live tmux server.
func agentReady(pane string, foreground string, harness string) bool {
	if strings.TrimSpace(pane) == "" {
		return false
	}
	return foregroundIsAgent(foreground, harness)
}

// foregroundIsAgent reports whether the pane's foreground process is the agent
// binary for the harness, distinguishing a ready agent from the bootstrapping
// wrapper shell.
func foregroundIsAgent(foreground string, harness string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(foreground)))
	if name == "" {
		return false
	}
	return name == strings.ToLower(strings.TrimSpace(harness))
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
	cmd.Env = terminal.TmuxClientEnv(envAssignments(workerEnv(input)))
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
		ServerURL: input.Config.CoordinatorURL,
		Token:     strings.TrimSpace(input.Config.Token),
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
		ServerURL: input.Config.CoordinatorURL,
		Token:     strings.TrimSpace(input.Config.Token),
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
}

func newTmuxWatchdogWithConfig(cfg config.WorkerConfig, sessionName string, silenceThreshold time.Duration) *tmuxWatchdog {
	return &tmuxWatchdog{
		sessionName:      sessionName,
		tmuxConfig:       cfg,
		silenceThreshold: silenceThreshold,
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
	switch watchdog.observe(now, pane, foreground) {
	case terminal.WatchdogWorking:
		reporter.report(coordinator.SessionWorking)
	case terminal.WatchdogWaiting:
		reporter.report(coordinator.SessionWaiting)
	}
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
	case "bash", "fish", "harness", "node", "sh", "tmux", "zsh":
		return false
	default:
		return true
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

// HistoryAttemptDir is the Flow-owned, lease-scoped root for local capture
// sources. Hashing the lease ID prevents path traversal and keeps retries apart.
func HistoryAttemptDir(workDir, jobID, leaseID string) string {
	digest := sha256.Sum256([]byte(leaseID))
	return filepath.Join(jobDir(workDir, jobID), "history", hex.EncodeToString(digest[:16]))
}

func historyAttemptDir(workDir, jobID, leaseID string) string {
	return HistoryAttemptDir(workDir, jobID, leaseID)
}

// HistoryWorktreePath returns the Flow-owned worktree for one job.
func HistoryWorktreePath(workDir, jobID string) string {
	return filepath.Join(jobDir(workDir, jobID), "repo")
}

// QuiesceJobHistorySources terminates Flow-owned tmux servers left by a crashed
// worker before startup recovery snapshots their files.
func QuiesceJobHistorySources(cfg config.WorkerConfig, jobID string) {
	resetTmuxForJob(cfg, sessionNameForJob(jobID))
	if dir, err := agentTmuxTmpDirForJob(cfg, jobID); err == nil {
		cleanupAgentTmuxServer(cfg, dir)
	}
}

// verdictFilePath is the path a check job writes its structured verdict to,
// exported to the entrypoint as FLOW_VERDICT_FILE. It lives in the job work
// directory (not the worktree) so it survives worktree teardown and cannot be
// committed by accident.
func verdictFilePath(workDir string, jobID string) string {
	return filepath.Join(jobDir(workDir, jobID), VerdictFileName)
}

func completionFilePath(workDir string, jobID string) string {
	return filepath.Join(jobDir(workDir, jobID), checkverdict.CompletionFileName)
}

func sessionNameForJob(jobID string) string {
	return terminal.TmuxSessionNameForJob(jobID)
}
