package coordinator

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

const defaultAuthorBase = "main"

// maxAutomaticCrashAttempts bounds how many crashed author attempts the
// coordinator tolerates before requiring human intervention. It counts crashed
// attempts, so the threshold of 2 permits exactly one automatic restart.
const maxAutomaticCrashAttempts = 2

// crashRestartLimitMessageFormat is the fmt.Sprintf template for the crash-loop
// blocker status message, and crashRestartLimitMessageLike is the matching SQL
// LIKE pattern. They are kept byte-identical (the LIKE wildcard replaces the
// %d count) so recordCrashRestartLimit and crashLoopStatusExists never drift.
const (
	crashRestartLimitMessageFormat = "Author job crashed %d times; human intervention required."
	crashRestartLimitMessageLike   = "Author job crashed % times; human intervention required."
)

var ErrAuthorJobSuppressed = errors.New("author job suppressed")

type SessionRuntimeState string

const (
	SessionStarting  SessionRuntimeState = "starting"
	SessionWorking   SessionRuntimeState = "working"
	SessionWaiting   SessionRuntimeState = "waiting"
	SessionFinished  SessionRuntimeState = "finished"
	SessionCrashed   SessionRuntimeState = "crashed"
	SessionAbandoned SessionRuntimeState = "abandoned"
)

type SessionSignalKind string

const (
	SessionSignalWorking  SessionSignalKind = "working"
	SessionSignalWaiting  SessionSignalKind = "waiting"
	SessionSignalActivity SessionSignalKind = "activity"
)

const (
	SessionEventSourceNativeHook = "native_hook"
	SessionEventSourceWatchdog   = "watchdog"
)

type Change struct {
	ID        string
	TaskID    string
	Branch    string
	Base      string
	HeadSHA   string
	CreatedAt time.Time
	UpdatedAt time.Time
	ReadyAt   *time.Time
	MergedAt  *time.Time
}

type Session struct {
	ID             string
	TaskID         string
	ChangeID       string
	WorkflowRunID  string
	NodeRunID      string
	WorkspaceMode  WorkspaceMode
	JobID          string
	LeaseID        string
	WorkerID       string
	Role           flowworker.JobRole
	RuntimeState   SessionRuntimeState
	Branch         string
	Base           string
	Harness        string
	TranscriptPath string
	TokenHash      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinishedAt     *time.Time
	// LastAgentActivityAt records the last time the agent inside the session
	// demonstrably did something (status write, hook event, state change,
	// ready). It is distinct from the worker lease heartbeat, which only proves
	// the worker process is alive.
	LastAgentActivityAt *time.Time `json:"last_agent_activity_at,omitempty"`
}

type EnsureAuthorJobInput struct {
	TaskID   string
	Branch   string
	Base     string
	Priority int
	Payload  map[string]any
}

type EnsureAuthorJobResult struct {
	Job      flowworker.Job
	Change   Change
	Existing bool
}

type RetryCrashedAuthorJobResult struct {
	Task         Task
	Job          *flowworker.Job
	Change       *Change
	Existing     bool
	ResolvedRows int64
}

type StartAuthorSessionInput struct {
	JobID          string
	LeaseID        string
	WorkerID       string
	Harness        string
	TranscriptPath string
}

type StartAuthorSessionResult struct {
	Session Session
	Change  Change
	Token   string
}

type EnsureConsoleJobInput struct {
	Base       string
	Harness    string
	Entrypoint map[string]any
	Priority   int
}

type EnsureTaskConsoleJobInput struct {
	TaskID   string
	Harness  string
	Priority int
}

type EnsureConsoleJobResult struct {
	Job      flowworker.Job
	Existing bool
}

type StartConsoleSessionInput struct {
	JobID          string
	LeaseID        string
	WorkerID       string
	Harness        string
	TranscriptPath string
}

type StartConsoleSessionResult struct {
	Session Session
	Token   string
}

type MarkPersistentSessionExitedInput struct {
	SessionID string
	LeaseID   string
	ExitCode  int
}

type ConsoleState struct {
	Active            bool
	Job               *flowworker.Job
	Session           *Session
	Terminal          *SessionTerminal
	TerminalAvailable bool
}

type SessionTerminal struct {
	SessionID      string    `json:"session_id"`
	TmuxSocketPath string    `json:"tmux_socket_path,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type SessionTerminalAccess struct {
	SessionID string    `json:"session_id"`
	Token     string    `json:"token"`
	LoginPath string    `json:"login_path"`
	ExpiresAt time.Time `json:"expires_at"`
}

type JobTerminal struct {
	JobID          string    `json:"job_id"`
	LeaseID        string    `json:"lease_id"`
	TmuxSocketPath string    `json:"tmux_socket_path,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type JobTerminalAccess struct {
	JobID     string    `json:"job_id"`
	Token     string    `json:"token"`
	LoginPath string    `json:"login_path"`
	ExpiresAt time.Time `json:"expires_at"`
}

type SessionServiceOptions struct {
	DefaultAuthorEntrypoint         map[string]any
	DefaultAuthorEntrypointOverride bool
	HarnessArgs                     []string
	// DefaultAgent is the configured fallback agent selection; the zero value
	// resolves to the built-in default harness.
	DefaultAgent           flowharness.AgentSelection
	ReviewAuthorCycleLimit int

	// Credentials is the coordinator-wide credential service; session
	// tokens live in the global database and carry a project binding.
	Credentials *CredentialService
	// Project identifies the project this service's database belongs to.
	Project Project

	// HandoffSnapshots and ReviewRounds enable Mode-B targeted-review recovery:
	// a crashed author whose branch is ahead of base and that left a handoff
	// snapshot is routed to a completion-assessment review instead of a blind
	// full relaunch. Both are optional — when either is nil the recovery path is
	// skipped and crash recovery falls back to the bounded author relaunch.
	HandoffSnapshots handoffSnapshotGetter
	ReviewRounds     reviewRoundScheduler

}

// handoffSnapshotGetter reads the coordinator-owned handoff snapshot for a
// change. Satisfied by *ReconcileService.
type handoffSnapshotGetter interface {
	GetHandoffSnapshot(ctx context.Context, changeID string) (HandoffSnapshot, error)
}

// reviewRoundScheduler schedules a review round for a change. Satisfied by
// *CheckConfigService.
type reviewRoundScheduler interface {
	ScheduleReviewRound(ctx context.Context, input ScheduleReviewRoundInput) (ScheduleReviewRoundResult, error)
}

type SessionService struct {
	db                              *sql.DB
	tasks                           *TaskService
	workers                         *flowworker.Service
	credentials                     *CredentialService
	project                         Project
	defaultAuthorEntrypoint         map[string]any
	defaultAuthorEntrypointOverride bool
	harnessArgs                     []string
	defaultAgent                    flowharness.AgentSelection
	reviewCycles                    *ReviewCycleService
	handoffSnapshots                handoffSnapshotGetter
	reviewRounds                    reviewRoundScheduler
	now                             func() time.Time
}

func NewSessionService(database *sql.DB, tasks *TaskService, workers *flowworker.Service) *SessionService {
	return NewSessionServiceWithOptions(database, tasks, workers, SessionServiceOptions{})
}

func NewSessionServiceWithOptions(database *sql.DB, tasks *TaskService, workers *flowworker.Service, opts SessionServiceOptions) *SessionService {
	if tasks == nil {
		tasks = NewTaskService(database, opts.Project.ID)
	}
	if workers == nil {
		workers = flowworker.NewService(database)
	}
	harnessArgs, err := flowharness.NormalizeArgs(opts.HarnessArgs)
	if err != nil {
		panic(fmt.Sprintf("normalize harness args: %v", err))
	}
	defaultAgent, err := flowharness.ResolveAgentSelection(opts.DefaultAgent)
	if err != nil {
		panic(fmt.Sprintf("resolve default agent: %v", err))
	}
	defaultEntrypoint := map[string]any{}
	if opts.DefaultAuthorEntrypointOverride {
		defaultEntrypoint = copyPayload(opts.DefaultAuthorEntrypoint)
	}
	if opts.DefaultAuthorEntrypointOverride && len(defaultEntrypoint) == 0 {
		defaultEntrypoint = defaultAuthorEntrypoint(defaultAgent)
	}
	return &SessionService{
		db:                              database,
		tasks:                           tasks,
		workers:                         workers,
		credentials:                     opts.Credentials,
		project:                         opts.Project,
		defaultAuthorEntrypoint:         defaultEntrypoint,
		defaultAuthorEntrypointOverride: opts.DefaultAuthorEntrypointOverride,
		harnessArgs:                     harnessArgs,
		defaultAgent:                    defaultAgent,
		reviewCycles:                    NewReviewCycleService(database, opts.ReviewAuthorCycleLimit),
		handoffSnapshots:                opts.HandoffSnapshots,
		reviewRounds:                    opts.ReviewRounds,
		now:                             sqlitex.UTCNow,
	}
}

// workPhaseContext is the resolved "which agent runs this author job" answer:
// the cursor's current phase for pipeline work, the flow's fix agent once the
// pipeline completed (review fix rounds), or the task-level fallback when no
// cursor exists.
type workPhaseContext struct {
	hasCursor    bool
	phaseName    string
	phaseIndex   int
	finalPhase   bool
	gateFeedback string
	agent        AgentDefSnapshot
}

// workPhaseHarness returns the harness the job should launch: the phase
// agent's when a cursor drives the job, else the configured default agent
// harness.
func (s *SessionService) workPhaseHarness(phaseCtx workPhaseContext) string {
	if phaseCtx.hasCursor && strings.TrimSpace(phaseCtx.agent.Harness) != "" {
		return phaseCtx.agent.Harness
	}
	return s.defaultAgent.Harness
}


func (s *SessionService) EnsureAuthorJob(ctx context.Context, input EnsureAuthorJobInput) (EnsureAuthorJobResult, error) {
	if _, err := s.ReconcileCrashedAuthorSessions(ctx); err != nil {
		return EnsureAuthorJobResult{}, err
	}

	return s.ensureAuthorJob(ctx, input)
}

func (s *SessionService) RetryCrashedAuthorJob(ctx context.Context, taskID string, actor string) (RetryCrashedAuthorJobResult, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return RetryCrashedAuthorJobResult{}, errors.New("task id is required")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "system"
	}

	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}
	if task.ScheduleState == ScheduleClosed {
		return RetryCrashedAuthorJobResult{}, errors.New("cannot retry a closed task")
	}
	if task.TriageState != TriageAccepted {
		return RetryCrashedAuthorJobResult{}, errors.New("crash retry requires an accepted task")
	}
	crashLoop, err := crashLoopStatusExists(ctx, s.db, task.ID)
	if err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}
	if !crashLoop {
		return RetryCrashedAuthorJobResult{}, errors.New("task is not held for crash retry")
	}

	phase, err := s.tasks.PhaseForTask(ctx, task)
	if err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}

	var ensured *EnsureAuthorJobResult
	switch phase {
	case PhaseUpNext, PhaseWorking:
		// working with an active session means the task already resumed; the
		// ensure would only be suppressed, so skip it and just clear the hold.
		active, err := s.hasActiveAuthorSession(ctx, task.ID)
		if err != nil {
			return RetryCrashedAuthorJobResult{}, err
		}
		if !active {
			result, err := s.EnsureAuthorJob(ctx, EnsureAuthorJobInput{TaskID: task.ID})
			if err != nil {
				return RetryCrashedAuthorJobResult{}, err
			}
			ensured = &result
		}
	case PhaseCritique, PhaseAcceptance, PhaseApproved:
		// The task has already advanced beyond the crashed author attempt.
		// Clearing the stale crash hold is enough.
	default:
		return RetryCrashedAuthorJobResult{}, fmt.Errorf("cannot retry crashed author job from phase %q", phase)
	}

	resolved, err := s.resolveCrashRestartLimit(ctx, task.ID)
	if err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}
	if _, err := NewStatusService(s.db).Write(ctx, WriteStatusInput{
		TaskID:  task.ID,
		Actor:   actor,
		Message: "Crash hold cleared; retrying author job.",
		Kind:    StatusKindProgress,
	}); err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}
	task, err = s.tasks.GetTask(ctx, task.ID)
	if err != nil {
		return RetryCrashedAuthorJobResult{}, err
	}

	result := RetryCrashedAuthorJobResult{Task: task, ResolvedRows: resolved}
	if ensured != nil {
		result.Job = &ensured.Job
		result.Change = &ensured.Change
		result.Existing = ensured.Existing
	}
	return result, nil
}

func (s *SessionService) ensureAuthorJob(ctx context.Context, input EnsureAuthorJobInput) (EnsureAuthorJobResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.TaskID == "" {
		return EnsureAuthorJobResult{}, errors.New("task id is required")
	}

	task, err := s.tasks.GetTask(ctx, input.TaskID)
	if err != nil {
		return EnsureAuthorJobResult{}, err
	}
	if task.TriageState != TriageAccepted {
		return EnsureAuthorJobResult{}, errors.New("author jobs require an accepted task")
	}
	if task.ScheduleState != ScheduleUpNext {
		return EnsureAuthorJobResult{}, errors.New("author jobs require an up_next task")
	}
	blocked, err := s.tasks.taskIsBlocked(ctx, task.ID)
	if err != nil {
		return EnsureAuthorJobResult{}, err
	}
	if blocked {
		return EnsureAuthorJobResult{}, fmt.Errorf("%w: task has unresolved blockers", ErrAuthorJobSuppressed)
	}
	if active, err := s.hasActiveAuthorSession(ctx, task.ID); err != nil {
		return EnsureAuthorJobResult{}, err
	} else if active {
		return EnsureAuthorJobResult{}, fmt.Errorf("%w: task already has an active author session", ErrAuthorJobSuppressed)
	}

	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		branch = taskBranch(task.ID)
	}
	base := strings.TrimSpace(input.Base)
	if base == "" {
		base = defaultAuthorBase
	}
	if err := validateBranchLike("branch", branch); err != nil {
		return EnsureAuthorJobResult{}, err
	}
	if err := validateBranchLike("base", base); err != nil {
		return EnsureAuthorJobResult{}, err
	}
	phaseCtx := workPhaseContext{finalPhase: true}
	jobHarness := s.workPhaseHarness(phaseCtx)

	if existing, ok, err := s.workers.LiveAuthorJobForTask(ctx, task.ID); err != nil {
		return EnsureAuthorJobResult{}, err
	} else if ok {
		existingChangeID := stringPointerValue(existing.ChangeID)
		if existingChangeID == "" || !authorJobMatches(existing, existingChangeID, branch, base, jobHarness, phaseCtx.phaseIndex, s.defaultAgent.Harness) {
			return EnsureAuthorJobResult{}, errors.New("live author job has incompatible change or branch")
		}
		change, err := s.GetChange(ctx, existingChangeID)
		if err != nil {
			return EnsureAuthorJobResult{}, err
		}
		return EnsureAuthorJobResult{Job: existing, Change: change, Existing: true}, nil
	}

	change, err := s.ensureChange(ctx, task.ID, branch, base)
	if err != nil {
		return EnsureAuthorJobResult{}, err
	}
	if change.MergedAt != nil {
		return EnsureAuthorJobResult{}, errors.New("author jobs cannot be enqueued for a merged change")
	}
	reviewFix, err := s.shouldConsumeReviewCycle(ctx, task.ID)
	if err != nil {
		return EnsureAuthorJobResult{}, err
	}
	if reviewFix {
		budget, err := s.reviewCycles.Consume(ctx, task.ID, "system")
		if errors.Is(err, ErrReviewCycleLimitReached) {
			return EnsureAuthorJobResult{}, fmt.Errorf("%w: %w (%d/%d review-author cycles used)", ErrAuthorJobSuppressed, ErrReviewCycleLimitReached, budget.UsedCycles, budget.GrantedCycles)
		}
		if err != nil {
			return EnsureAuthorJobResult{}, err
		}
		if input.Payload == nil {
			input.Payload = map[string]any{}
		}
		input.Payload["review_cycle_number"] = budget.UsedCycles
		input.Payload["review_cycle_limit"] = budget.GrantedCycles
		if strings.TrimSpace(budget.LastInstructions) != "" {
			input.Payload["review_cycle_instructions"] = strings.TrimSpace(budget.LastInstructions)
		}
	}

	priority := input.Priority
	if priority == 0 {
		priority = task.Priority
	}
	payload := copyPayload(input.Payload)
	if _, ok := payload["entrypoint"]; !ok {
		entrypoint, injectInitialPrompt, err := s.authorEntrypointPayload(phaseCtx)
		if err != nil {
			return EnsureAuthorJobResult{}, err
		}
		payload["entrypoint"] = entrypoint
		payload["inject_initial_prompt"] = injectInitialPrompt
		payload["prompt_harness"] = jobHarness
	}
	payload["change_id"] = change.ID
	payload["branch"] = branch
	payload["base"] = base
	payload["agent_harness"] = jobHarness
	stampWorkPhasePayload(payload, phaseCtx)
	if err := stampImageAttachments(ctx, s.tasks, payload, task.ID); err != nil {
		return EnsureAuthorJobResult{}, err
	}
	stampProjectPayload(payload, s.project)

	job, err := s.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		ChangeID:       &change.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       priority,
		Requires:       authorHarnessRequirements(jobHarness),
		Payload:        payload,
	})
	if err != nil {
		if existing, ok, lookupErr := s.workers.LiveAuthorJobForTask(ctx, task.ID); lookupErr == nil && ok && authorJobMatches(existing, change.ID, branch, base, jobHarness, phaseCtx.phaseIndex, s.defaultAgent.Harness) {
			return EnsureAuthorJobResult{Job: existing, Change: change, Existing: true}, nil
		}
		return EnsureAuthorJobResult{}, err
	}

	return EnsureAuthorJobResult{Job: job, Change: change}, nil
}

// stampWorkPhasePayload records the job's work-phase coordinates: which phase
// it runs, whether that phase publishes the change, and any gate feedback to
// inject into the prompt. Jobs without a cursor are the implicit single final
// phase.
func stampWorkPhasePayload(payload map[string]any, phaseCtx workPhaseContext) {
	payload["phase_index"] = phaseCtx.phaseIndex
	payload["final_phase"] = phaseCtx.finalPhase
	if phaseCtx.hasCursor {
		payload["phase_name"] = phaseCtx.phaseName
	}
	if strings.TrimSpace(phaseCtx.gateFeedback) != "" {
		payload["gate_feedback"] = strings.TrimSpace(phaseCtx.gateFeedback)
	} else {
		delete(payload, "gate_feedback")
	}
}

func (s *SessionService) shouldConsumeReviewCycle(ctx context.Context, taskID string) (bool, error) {
	reviewState, err := reviewStateForTask(ctx, s.db, taskID)
	if err != nil {
		return false, err
	}
	if reviewState != ReviewChangesRequested {
		return false, nil
	}
	return taskHasUnmergedChange(ctx, s.db, taskID)
}

func (s *SessionService) ReviewCycleBudget(ctx context.Context, taskID string) (ReviewCycleBudget, error) {
	return s.reviewCycles.Get(ctx, taskID)
}

func (s *SessionService) ApproveReviewCycles(ctx context.Context, input ApproveReviewCyclesInput) (ReviewCycleBudget, error) {
	return s.reviewCycles.ApproveMore(ctx, input)
}

func (s *SessionService) EnsureConsoleJob(ctx context.Context, input EnsureConsoleJobInput) (EnsureConsoleJobResult, error) {
	if _, err := s.ReconcileCrashedConsoleSessions(ctx); err != nil {
		return EnsureConsoleJobResult{}, err
	}

	if existing, ok, err := s.liveConsoleJob(ctx); err != nil {
		return EnsureConsoleJobResult{}, err
	} else if ok {
		return EnsureConsoleJobResult{Job: existing, Existing: true}, nil
	}
	if state, err := s.CurrentConsole(ctx); err != nil {
		return EnsureConsoleJobResult{}, err
	} else if state.Session != nil {
		return EnsureConsoleJobResult{}, errors.New("console session is already active")
	}

	base := strings.TrimSpace(input.Base)
	if base == "" {
		base = strings.TrimSpace(s.project.BaseBranch)
	}
	if base == "" {
		base = defaultAuthorBase
	}
	if err := validateBranchLike("base", base); err != nil {
		return EnsureConsoleJobResult{}, err
	}
	harness := flowharness.NormalizeName(input.Harness)
	if harness == "" {
		harness = flowharness.DefaultConsoleName()
	}
	if err := flowharness.ValidateConsoleName(harness); err != nil {
		return EnsureConsoleJobResult{}, err
	}

	payload := map[string]any{}
	if len(input.Entrypoint) > 0 {
		payload["entrypoint"] = copyPayload(input.Entrypoint)
	} else {
		entrypoint, err := flowharness.DefaultConsoleEntrypointWithArgs(harness, s.harnessArgs)
		if err != nil {
			return EnsureConsoleJobResult{}, err
		}
		payload["entrypoint"] = entrypoint
	}
	payload["base"] = base
	payload["branch"] = base
	payload["console_harness"] = harness
	payload["session_purpose"] = "console"
	stampProjectPayload(payload, s.project)

	job, err := s.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleConsole,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       input.Priority,
		Requires:       consoleHarnessRequirements(harness),
		Payload:        payload,
	})
	if err != nil {
		if existing, ok, lookupErr := s.liveConsoleJob(ctx); lookupErr == nil && ok {
			return EnsureConsoleJobResult{Job: existing, Existing: true}, nil
		}
		return EnsureConsoleJobResult{}, err
	}

	return EnsureConsoleJobResult{Job: job}, nil
}

func (s *SessionService) EnsureTaskConsoleJob(ctx context.Context, input EnsureTaskConsoleJobInput) (EnsureConsoleJobResult, error) {
	if _, err := s.ReconcileCrashedConsoleSessions(ctx); err != nil {
		return EnsureConsoleJobResult{}, err
	}
	taskID := strings.TrimSpace(input.TaskID)
	if taskID == "" {
		return EnsureConsoleJobResult{}, errors.New("task id is required")
	}
	if existing, ok, err := s.liveTaskConsoleJob(ctx, taskID); err != nil {
		return EnsureConsoleJobResult{}, err
	} else if ok {
		return EnsureConsoleJobResult{Job: existing, Existing: true}, nil
	}
	if state, err := s.CurrentTaskConsole(ctx, taskID); err != nil {
		return EnsureConsoleJobResult{}, err
	} else if state.Session != nil {
		return EnsureConsoleJobResult{}, errors.New("task console session is already active")
	}

	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return EnsureConsoleJobResult{}, err
	}
	base := strings.TrimSpace(s.project.BaseBranch)
	if base == "" {
		base = defaultAuthorBase
	}
	change, ok, err := s.ReadyUnmergedChangeForTask(ctx, task.ID)
	if err != nil {
		return EnsureConsoleJobResult{}, err
	}
	if !ok {
		change, err = s.ensureChange(ctx, task.ID, taskBranch(task.ID), base)
		if err != nil {
			return EnsureConsoleJobResult{}, err
		}
	}
	if err := validateBranchLike("branch", change.Branch); err != nil {
		return EnsureConsoleJobResult{}, err
	}
	if err := validateBranchLike("base", change.Base); err != nil {
		return EnsureConsoleJobResult{}, err
	}
	harness := flowharness.NormalizeName(input.Harness)
	if harness == "" {
		harness = flowharness.DefaultConsoleName()
	}
	if err := flowharness.ValidateConsoleName(harness); err != nil {
		return EnsureConsoleJobResult{}, err
	}

	entrypoint, err := flowharness.DefaultConsoleEntrypointWithArgs(harness, s.harnessArgs)
	if err != nil {
		return EnsureConsoleJobResult{}, err
	}
	payload := map[string]any{
		"entrypoint":      entrypoint,
		"base":            change.Base,
		"branch":          change.Branch,
		"change_id":       change.ID,
		"console_harness": harness,
		"console_scope":   "task_recovery",
		"session_purpose": "task_console",
	}
	stampProjectPayload(payload, s.project)

	job, err := s.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		ChangeID:       &change.ID,
		Role:           flowworker.RoleConsole,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       input.Priority,
		Requires:       consoleHarnessRequirements(harness),
		Payload:        payload,
	})
	if err != nil {
		if existing, ok, lookupErr := s.liveTaskConsoleJob(ctx, task.ID); lookupErr == nil && ok {
			return EnsureConsoleJobResult{Job: existing, Existing: true}, nil
		}
		return EnsureConsoleJobResult{}, err
	}

	return EnsureConsoleJobResult{Job: job}, nil
}

func (s *SessionService) ReconcileCrashedAuthorSessions(ctx context.Context) (int, error) {
	if _, err := s.workers.SweepExpiredLeases(ctx); err != nil {
		return 0, err
	}

	now := s.now().UTC()
	rows, err := s.db.QueryContext(ctx, `
SELECT s.id
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
WHERE s.role = ?
	AND (
		(
			s.runtime_state IN (?, ?, ?)
			AND (
				j.state IN (?, ?, ?, ?)
				OR l.released_at IS NOT NULL
				OR l.expires_at <= ?
			)
		)
		OR (
			s.workflow_run_id IS NULL
			AND s.runtime_state = ?
			AND NOT EXISTS (
				SELECT 1
				FROM jobs live
				WHERE live.task_id = s.task_id
					AND live.role = ?
					AND live.state IN (?, ?, ?)
			)
		)
	)
ORDER BY s.updated_at`,
		string(flowworker.RoleAuthor),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobFinished),
		string(flowworker.JobFailed),
		string(flowworker.JobCrashed),
		string(flowworker.JobCanceled),
		formatTime(now),
		string(SessionCrashed),
		string(flowworker.RoleAuthor),
		string(flowworker.JobQueued),
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
	)
	if err != nil {
		return 0, fmt.Errorf("select crashed author sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan crashed author session: %w", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close crashed author sessions rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate crashed author sessions: %w", err)
	}
	if len(sessionIDs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin crashed session transaction: %w", err)
	}
	defer tx.Rollback()

	nowText := formatTime(now)
	var joinedErr error
	// Isolate per-session faults: a poisoned session must not abort the tx for
	// the surviving ones. The crash UPDATEs are idempotent (guarded by
	// runtime_state IN (...)), so any session whose update we skip this tick is
	// safely retried next tick.
	markedSessionIDs := make([]string, 0, len(sessionIDs))
	var revokeTokenHashes []string
	for _, sessionID := range sessionIDs {
		session, err := scanSession(tx.QueryRowContext(ctx, sessionSelectSQL+`
WHERE id = ?`, sessionID))
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("load crashed author session %s: %w", sessionID, err))
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?
	AND runtime_state IN (?, ?, ?)`,
			string(SessionCrashed),
			nowText,
			nowText,
			session.ID,
			string(SessionStarting),
			string(SessionWorking),
			string(SessionWaiting),
		); err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("mark author session %s crashed: %w", sessionID, err))
			continue
		}
		revokeTokenHashes = append(revokeTokenHashes, session.TokenHash)
		markedSessionIDs = append(markedSessionIDs, sessionID)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit crashed session transaction: %w", err)
	}
	// Session tokens live in the coordinator's global database, so they are
	// revoked after the project transaction commits. A revocation failure
	// leaves a token that still authenticates, but every mutating session
	// operation also validates the session's runtime state.
	for _, tokenHash := range revokeTokenHashes {
		if err := s.revokeSessionTokenHash(ctx, tokenHash); err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
	}

	var recovered int
	for _, sessionID := range markedSessionIDs {
		session, err := s.GetSession(ctx, sessionID)
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("get crashed author session %s: %w", sessionID, err))
			continue
		}
		// Workflow retries are owned by the workflow executor and require an
		// explicit operator action after an execution failure. Reconcile the
		// dead session so it no longer blocks the one-active-author constraint,
		// but do not route it through the legacy automatic author-job recovery.
		if session.WorkflowRunID != "" {
			continue
		}
		job, err := s.workers.GetJob(ctx, session.JobID)
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("get job for crashed author session %s: %w", sessionID, err))
			continue
		}
		enqueued, err := s.enqueueCrashedAuthorSession(ctx, session, job)
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("re-enqueue crashed author session %s: %w", sessionID, err))
			continue
		}
		if enqueued {
			recovered++
		}
	}

	return recovered, joinedErr
}

func (s *SessionService) ReconcileCrashedConsoleSessions(ctx context.Context) (int, error) {
	if _, err := s.workers.SweepExpiredLeases(ctx); err != nil {
		return 0, err
	}

	now := s.now().UTC()
	rows, err := s.db.QueryContext(ctx, `
SELECT s.id
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
WHERE s.role = ?
	AND s.runtime_state IN (?, ?, ?)
	AND (
		j.state IN (?, ?, ?, ?)
		OR l.released_at IS NOT NULL
		OR l.expires_at <= ?
	)
ORDER BY s.updated_at`,
		string(flowworker.RoleConsole),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobFinished),
		string(flowworker.JobFailed),
		string(flowworker.JobCrashed),
		string(flowworker.JobCanceled),
		formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("select crashed console sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan crashed console session: %w", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close crashed console sessions rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate crashed console sessions: %w", err)
	}
	if len(sessionIDs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin crashed console session transaction: %w", err)
	}
	defer tx.Rollback()

	nowText := formatTime(now)
	var joinedErr error
	var revoked []string
	var marked int
	for _, sessionID := range sessionIDs {
		session, err := scanSession(tx.QueryRowContext(ctx, sessionSelectSQL+`
WHERE id = ?`, sessionID))
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("load crashed console session %s: %w", sessionID, err))
			continue
		}
		result, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?
	AND runtime_state IN (?, ?, ?)`,
			string(SessionCrashed),
			nowText,
			nowText,
			session.ID,
			string(SessionStarting),
			string(SessionWorking),
			string(SessionWaiting),
		)
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("mark console session %s crashed: %w", sessionID, err))
			continue
		}
		rows, err := result.RowsAffected()
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("read console crash rows affected %s: %w", sessionID, err))
			continue
		}
		if rows > 0 {
			marked++
			revoked = append(revoked, session.TokenHash)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit crashed console session transaction: %w", err)
	}
	for _, tokenHash := range revoked {
		if err := s.revokeSessionTokenHash(ctx, tokenHash); err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
	}

	return marked, joinedErr
}

func (s *SessionService) enqueueCrashedAuthorSession(ctx context.Context, session Session, job flowworker.Job) (bool, error) {
	task, err := s.tasks.GetTask(ctx, session.TaskID)
	if err != nil {
		return false, err
	}
	if task.TriageState != TriageAccepted || task.ScheduleState != ScheduleUpNext {
		return false, nil
	}
	blocked, err := s.tasks.taskIsBlocked(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	change, err := s.GetChange(ctx, session.ChangeID)
	if err != nil {
		return false, err
	}
	if change.MergedAt != nil {
		return false, nil
	}
	crashedHarness := payloadString(job.Payload, "agent_harness")
	if crashedHarness == "" {
		crashedHarness = s.defaultAgent.Harness
	}
	crashedPhaseIndex := payloadPhaseIndex(job.Payload)
	if existing, ok, err := s.workers.LiveAuthorJobForTask(ctx, task.ID); err != nil {
		return false, err
	} else if ok {
		if authorJobMatches(existing, change.ID, session.Branch, session.Base, crashedHarness, crashedPhaseIndex, s.defaultAgent.Harness) {
			return false, nil
		}
		return false, errors.New("live author job has incompatible change or branch")
	}
	// Mode-B recovery. A crashed author session keeps matching the reconcile
	// query until a live author job exists for the task; the targeted-review
	// path enqueues a reviewer job, not an author job, so the same crashed
	// session is re-selected every tick. The dispatch flag, stamped onto the
	// crashed job, makes that re-selection a no-op instead of a blind relaunch.
	if completionReviewDispatched(job) {
		return false, nil
	}
	if dispatched, err := s.maybeDispatchCompletionReview(ctx, task, change, job); err != nil {
		return false, err
	} else if dispatched {
		return true, nil
	}
	exhausted, attempts, err := s.authorCrashRestartLimitReached(ctx, task.ID, change.ID, crashedPhaseIndex)
	if err != nil {
		return false, err
	}
	if exhausted {
		if err := s.recordCrashRestartLimit(ctx, task.ID, attempts); err != nil {
			return false, err
		}
		return false, nil
	}

	payload := copyPayload(job.Payload)
	reviewFix, err := s.shouldConsumeReviewCycle(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if reviewFix {
		budget, err := s.reviewCycles.Consume(ctx, task.ID, "system")
		if errors.Is(err, ErrReviewCycleLimitReached) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		payload["review_cycle_number"] = budget.UsedCycles
		payload["review_cycle_limit"] = budget.GrantedCycles
		if strings.TrimSpace(budget.LastInstructions) != "" {
			payload["review_cycle_instructions"] = strings.TrimSpace(budget.LastInstructions)
		}
	}
	// The crashed job's payload carries its entrypoint; the relaunch re-runs
	// the same author attempt with the same agent.
	if _, ok := payload["entrypoint"]; !ok {
		phaseCtx := workPhaseContext{finalPhase: true}
		entrypoint, injectInitialPrompt, err := s.authorEntrypointPayload(phaseCtx)
		if err != nil {
			return false, err
		}
		payload["entrypoint"] = entrypoint
		payload["inject_initial_prompt"] = injectInitialPrompt
		payload["prompt_harness"] = s.workPhaseHarness(phaseCtx)
		payload["agent_harness"] = s.workPhaseHarness(phaseCtx)
		stampWorkPhasePayload(payload, phaseCtx)
	}
	payload["change_id"] = change.ID
	payload["branch"] = session.Branch
	payload["base"] = session.Base
	if err := stampImageAttachments(ctx, s.tasks, payload, task.ID); err != nil {
		return false, err
	}

	_, err = s.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		ChangeID:       &change.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       job.Priority,
		Requires:       authorHarnessRequirements(crashedHarness),
		Payload:        payload,
	})
	if err != nil {
		return false, fmt.Errorf("re-enqueue crashed author job: %w", err)
	}

	return true, nil
}

// completionReviewDispatchedKey marks a crashed author job that has already been
// routed to a Mode-B completion-assessment review, so re-selection of the same
// crashed session is a no-op rather than a blind author relaunch.
const completionReviewDispatchedKey = "completion_review_dispatched"

// completionReviewDispatched reports whether this crashed author job already had
// a completion-assessment review dispatched for it.
func completionReviewDispatched(job flowworker.Job) bool {
	return payloadBool(job.Payload, completionReviewDispatchedKey)
}

// changeIsAheadOfBase reports whether the change has commits beyond its base.
// The coordinator never stores a base SHA, so a non-empty projected head is the
// available proxy: an task branch is cut from base and only acquires a head SHA
// once the git reconcile projects real commits onto it (a fresh branch with no
// commits has an empty head). The same non-empty-head signal already gates the
// ordinary ready-review scheduling, so this stays consistent with it.
func changeIsAheadOfBase(change Change) bool {
	return strings.TrimSpace(change.HeadSHA) != ""
}

// maybeDispatchCompletionReview implements Mode-B targeted-review recovery. When
// a crashed author session's branch is ahead of base and a handoff snapshot is
// present — the signal that the author may actually be finished — it publishes
// the change (marks it ready) and enqueues a completion-assessment review
// instead of a blind full relaunch, returning true. The reviewer's verdict then
// routes through the existing review→fix cycle: satisfied proceeds to
// verification, blocked enqueues a bounded author fix round. It fires at most
// once per change: marking the change ready closes the not-yet-ready gate, so a
// later fix-round author that crashes falls through to the normal bounded
// relaunch. Returns false (with no side effects) whenever the recovery
// preconditions are not met, so the caller keeps today's behavior.
func (s *SessionService) maybeDispatchCompletionReview(ctx context.Context, task Task, change Change, job flowworker.Job) (bool, error) {
	if s.reviewRounds == nil || s.handoffSnapshots == nil {
		return false, nil
	}
	// Only the final work phase produces a reviewable change; intermediate
	// phases (spec, plan, ...) cannot be completion-assessed.
	if !payloadBool(job.Payload, "final_phase") {
		return false, nil
	}
	// Fire only for a change the author never finalized. Once marked ready below,
	// this gate closes so a subsequent fix-round crash relaunches normally.
	if change.ReadyAt != nil {
		return false, nil
	}
	if !changeIsAheadOfBase(change) {
		return false, nil
	}
	snapshot, err := s.handoffSnapshots.GetHandoffSnapshot(ctx, change.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !snapshot.Present {
		return false, nil
	}

	// Schedule the review first: it is the only failure-prone, git-touching step
	// and it is idempotent (live-job dedup + pending-check upsert), so a retry
	// after a transient failure is safe. Publishing the change + stamping the
	// dispatch flag only happens once the review is in flight.
	if _, err := s.reviewRounds.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Task:                 task,
		Change:               change,
		CompletionAssessment: true,
	}); err != nil {
		return false, fmt.Errorf("schedule completion-assessment review: %w", err)
	}
	if err := s.publishCompletionReview(ctx, change.ID, job.ID, job.Payload); err != nil {
		return false, err
	}

	return true, nil
}

// publishCompletionReview atomically marks the change ready — so the completion-
// assessment reviewer's verdict routes through the existing machinery — and
// stamps the crashed job with the dispatch flag so re-selection of the crashed
// session becomes a no-op.
func (s *SessionService) publishCompletionReview(ctx context.Context, changeID string, jobID string, jobPayload map[string]any) error {
	payload := copyPayload(jobPayload)
	payload[completionReviewDispatchedKey] = true
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal completion-review payload: %w", err)
	}

	nowText := formatTime(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin completion-review transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
		nowText,
		nowText,
		changeID,
	); err != nil {
		return fmt.Errorf("publish completion-review change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET payload_json = ?,
	updated_at = ?
WHERE id = ?`,
		string(raw),
		nowText,
		jobID,
	); err != nil {
		return fmt.Errorf("stamp completion-review job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completion-review transaction: %w", err)
	}

	return nil
}

func (s *SessionService) authorCrashRestartLimitReached(ctx context.Context, taskID string, changeID string, phaseIndex int) (bool, int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM jobs
WHERE task_id = ?
	AND change_id = ?
	AND role = ?
	AND state = ?`,
		taskID,
		changeID,
		string(flowworker.RoleAuthor),
		string(flowworker.JobCrashed),
	)
	if err != nil {
		return false, 0, fmt.Errorf("count crashed author attempts: %w", err)
	}
	defer rows.Close()

	attempts := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, 0, fmt.Errorf("scan crashed author payload: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			// A single corrupt payload must not abort the whole reconcile tick;
			// skip it and keep counting the well-formed crashed attempts.
			slog.Warn("skip malformed crashed author payload", "task_id", taskID, "change_id", changeID, "error", err)
			continue
		}
		// A crash that was routed to a completion-assessment review (Mode-B
		// recovery) was never relaunched, so it must not consume the automatic
		// relaunch budget; only real relaunched-and-crashed-again attempts count.
		if payloadBool(payload, completionReviewDispatchedKey) {
			continue
		}
		// Restart budgets are per work phase: a crash in phase 2 must not
		// inherit phase 1's spent attempts.
		if payloadPhaseIndex(payload) == phaseIndex {
			attempts++
		}
	}
	if err := rows.Err(); err != nil {
		return false, 0, fmt.Errorf("iterate crashed author payloads: %w", err)
	}

	return attempts >= maxAutomaticCrashAttempts, attempts, nil
}

func (s *SessionService) recordCrashRestartLimit(ctx context.Context, taskID string, attempts int) error {
	nowText := formatTime(s.now().UTC())
	message := fmt.Sprintf(crashRestartLimitMessageFormat, attempts)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO status_log (task_id, actor, message, kind, created_at)
SELECT ?, ?, ?, ?, ?
WHERE NOT EXISTS (
	SELECT 1
	FROM status_log
	WHERE task_id = ?
		AND kind = ?
		AND message LIKE ?
		AND resolved_at IS NULL
)`,
		taskID,
		"system",
		message,
		StatusKindBlocker,
		nowText,
		taskID,
		StatusKindBlocker,
		crashRestartLimitMessageLike,
	); err != nil {
		return fmt.Errorf("record crash restart limit: %w", err)
	}

	return nil
}

func (s *SessionService) resolveCrashRestartLimit(ctx context.Context, taskID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE status_log
SET resolved_at = ?
WHERE task_id = ?
	AND kind = ?
	AND message LIKE ?
	AND resolved_at IS NULL`,
		formatTime(s.now().UTC()),
		taskID,
		StatusKindBlocker,
		crashRestartLimitMessageLike,
	)
	if err != nil {
		return 0, fmt.Errorf("resolve crash restart limit: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read resolved crash restart rows: %w", err)
	}
	return rows, nil
}

func authorHarnessRequirements(harness string) []string {
	return []string{flowharness.AgentHarnessLabel(harness)}
}

func consoleHarnessRequirements(harness string) []string {
	if flowharness.NormalizeName(harness) == flowharness.Shell {
		return nil
	}
	return []string{flowharness.AgentHarnessLabel(harness)}
}

// authorEntrypointPayload builds the agent launch entrypoint for an author
// job: the phase agent's harness with its model/effort selection appended to
// the coordinator harness args, or the configured default agent when no cursor
// drives the job.
func (s *SessionService) authorEntrypointPayload(phaseCtx workPhaseContext) (map[string]any, bool, error) {
	if s.defaultAuthorEntrypointOverride {
		return copyPayload(s.defaultAuthorEntrypoint), true, nil
	}
	args := append([]string{}, s.harnessArgs...)
	if phaseCtx.hasCursor {
		tokens, err := phaseCtx.agent.ModelSelectionArgs()
		if err != nil {
			return nil, false, err
		}
		args = append(args, tokens...)
	} else {
		// The default agent's model tokens precede the manual harness args so a
		// --model in harness_args wins (last-token-wins) over the configured
		// default model.
		tokens, err := s.defaultAgent.ModelArgs()
		if err != nil {
			return nil, false, err
		}
		args = append(tokens, args...)
	}
	entrypoint, err := flowharness.DefaultAuthorEntrypointWithArgs(s.workPhaseHarness(phaseCtx), args)
	return entrypoint, false, err
}

func (s *SessionService) StartAuthorSession(ctx context.Context, input StartAuthorSessionInput) (StartAuthorSessionResult, error) {
	jobID := strings.TrimSpace(input.JobID)
	leaseID := strings.TrimSpace(input.LeaseID)
	workerID := strings.TrimSpace(input.WorkerID)
	if jobID == "" || leaseID == "" || workerID == "" {
		return StartAuthorSessionResult{}, errors.New("job_id, lease_id, and worker_id are required")
	}
	if _, err := s.workers.SweepExpiredLeases(ctx); err != nil {
		return StartAuthorSessionResult{}, err
	}

	job, err := s.workers.GetJob(ctx, jobID)
	if err != nil {
		return StartAuthorSessionResult{}, err
	}
	if job.Role != flowworker.RoleAuthor || job.TaskID == nil {
		return StartAuthorSessionResult{}, errors.New("author session requires an author job with task id")
	}
	if job.State != flowworker.JobRunning {
		return StartAuthorSessionResult{}, errors.New("author session requires a running author job")
	}
	lease, err := s.workers.GetLease(ctx, leaseID)
	if err != nil {
		return StartAuthorSessionResult{}, err
	}
	if lease.JobID != job.ID || lease.WorkerID != workerID || lease.ReleasedAt != nil {
		return StartAuthorSessionResult{}, errors.New("author session requires a live lease for the job and worker")
	}
	if !s.now().UTC().Before(lease.ExpiresAt) {
		return StartAuthorSessionResult{}, errors.New("author session requires an unexpired lease")
	}

	changeID := stringPointerValue(job.ChangeID)
	workspaceMode := WorkspaceMode(payloadString(job.Payload, "workspace_mode"))
	if workspaceMode == "" {
		workspaceMode = WorkspaceChange
	}
	branch := payloadString(job.Payload, "branch")
	base := payloadString(job.Payload, "base")
	if base == "" {
		return StartAuthorSessionResult{}, errors.New("author job payload requires base")
	}
	if branch == "" && workspaceMode == WorkspaceBase {
		branch = base
	}
	var change Change
	switch workspaceMode {
	case WorkspaceChange:
		if changeID == "" || branch == "" {
			return StartAuthorSessionResult{}, errors.New("change-workspace author job requires change_id and branch")
		}
		change, err = s.GetChange(ctx, changeID)
		if err != nil {
			return StartAuthorSessionResult{}, err
		}
		if change.TaskID != *job.TaskID || change.Branch != branch || change.Base != base {
			return StartAuthorSessionResult{}, errors.New("author job payload does not match change")
		}
	case WorkspaceBase:
		if changeID != "" || branch != base {
			return StartAuthorSessionResult{}, errors.New("base-workspace author job cannot carry a change or non-base branch")
		}
	default:
		return StartAuthorSessionResult{}, fmt.Errorf("author job has invalid workspace_mode %q", workspaceMode)
	}

	sessionID, err := randomPrefixedID("s")
	if err != nil {
		return StartAuthorSessionResult{}, err
	}
	token, err := randomCredentialToken()
	if err != nil {
		return StartAuthorSessionResult{}, err
	}
	tokenHash := HashToken(token)
	now := s.now().UTC()

	// The session token lives in the coordinator's global database with a
	// binding to this project, so it cannot join the project transaction.
	// Mint it first; if the session insert fails the token is revoked again
	// below (a dangling token would otherwise authenticate until expiry).
	if s.credentials == nil {
		return StartAuthorSessionResult{}, errors.New("session service requires a credential service")
	}
	projectID := s.project.ID
	if err := s.credentials.EnsureToken(ctx, CredentialInput{
		Token:        token,
		Scope:        TokenScopeSession,
		Subject:      sessionID,
		ProjectID:    &projectID,
		SourceTaskID: job.TaskID,
	}); err != nil {
		return StartAuthorSessionResult{}, fmt.Errorf("store session token: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if revokeErr := s.credentials.RevokeTokenHash(context.WithoutCancel(ctx), tokenHash); revokeErr != nil {
				slog.Warn("revoke orphaned session token", "session_id", sessionID, "error", revokeErr)
			}
		}
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartAuthorSessionResult{}, fmt.Errorf("begin start session transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions (
	id,
	task_id,
	change_id,
	workflow_run_id,
	node_run_id,
	job_id,
	lease_id,
	worker_id,
	role,
	workspace_mode,
	runtime_state,
	branch,
	base,
	harness,
	transcript_path,
	last_agent_activity_at,
	token_hash,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		*job.TaskID,
		nullableStringValue(job.ChangeID),
		nullableStringValue(job.WorkflowRunID),
		nullableStringValue(job.NodeRunID),
		job.ID,
		lease.ID,
		workerID,
		string(flowworker.RoleAuthor),
		string(workspaceMode),
		string(SessionStarting),
		branch,
		base,
		strings.TrimSpace(input.Harness),
		strings.TrimSpace(input.TranscriptPath),
		// A new session has never demonstrated agent activity; it stays NULL
		// until the first TouchAgentActivity, so GetSession round-trips nil.
		nil,
		tokenHash,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return StartAuthorSessionResult{}, fmt.Errorf("insert session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StartAuthorSessionResult{}, fmt.Errorf("commit start session transaction: %w", err)
	}
	committed = true

	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return StartAuthorSessionResult{}, err
	}
	return StartAuthorSessionResult{Session: session, Change: change, Token: token}, nil
}

func (s *SessionService) StartConsoleSession(ctx context.Context, input StartConsoleSessionInput) (StartConsoleSessionResult, error) {
	jobID := strings.TrimSpace(input.JobID)
	leaseID := strings.TrimSpace(input.LeaseID)
	workerID := strings.TrimSpace(input.WorkerID)
	if jobID == "" || leaseID == "" || workerID == "" {
		return StartConsoleSessionResult{}, errors.New("job_id, lease_id, and worker_id are required")
	}
	if _, err := s.workers.SweepExpiredLeases(ctx); err != nil {
		return StartConsoleSessionResult{}, err
	}

	job, err := s.workers.GetJob(ctx, jobID)
	if err != nil {
		return StartConsoleSessionResult{}, err
	}
	if job.Role != flowworker.RoleConsole {
		return StartConsoleSessionResult{}, errors.New("console session requires a console job")
	}
	if job.State != flowworker.JobRunning {
		return StartConsoleSessionResult{}, errors.New("console session requires a running console job")
	}
	lease, err := s.workers.GetLease(ctx, leaseID)
	if err != nil {
		return StartConsoleSessionResult{}, err
	}
	if lease.JobID != job.ID || lease.WorkerID != workerID || lease.ReleasedAt != nil {
		return StartConsoleSessionResult{}, errors.New("console session requires a live lease for the job and worker")
	}
	if !s.now().UTC().Before(lease.ExpiresAt) {
		return StartConsoleSessionResult{}, errors.New("console session requires an unexpired lease")
	}

	base := payloadString(job.Payload, "base")
	branch := payloadString(job.Payload, "branch")
	if base == "" {
		base = strings.TrimSpace(s.project.BaseBranch)
	}
	if base == "" {
		base = defaultAuthorBase
	}
	if branch == "" {
		branch = base
	}
	if job.TaskID == nil && branch != base {
		return StartConsoleSessionResult{}, errors.New("console job branch must match base")
	}
	changeID := stringPointerValue(job.ChangeID)
	if job.TaskID != nil {
		if changeID == "" {
			return StartConsoleSessionResult{}, errors.New("task console job requires change id")
		}
		change, err := s.GetChange(ctx, changeID)
		if err != nil {
			return StartConsoleSessionResult{}, err
		}
		if change.TaskID != *job.TaskID || change.Branch != branch || change.Base != base {
			return StartConsoleSessionResult{}, errors.New("task console job payload does not match change")
		}
	}

	sessionID, err := randomPrefixedID("s")
	if err != nil {
		return StartConsoleSessionResult{}, err
	}
	token, err := randomCredentialToken()
	if err != nil {
		return StartConsoleSessionResult{}, err
	}
	tokenHash := HashToken(token)
	now := s.now().UTC()

	if s.credentials == nil {
		return StartConsoleSessionResult{}, errors.New("session service requires a credential service")
	}
	projectID := s.project.ID
	if err := s.credentials.EnsureToken(ctx, CredentialInput{
		Token:        token,
		Scope:        TokenScopeConsole,
		Subject:      sessionID,
		ProjectID:    &projectID,
		SourceTaskID: job.TaskID,
	}); err != nil {
		return StartConsoleSessionResult{}, fmt.Errorf("store console token: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if revokeErr := s.credentials.RevokeTokenHash(context.WithoutCancel(ctx), tokenHash); revokeErr != nil {
				slog.Warn("revoke orphaned console token", "session_id", sessionID, "error", revokeErr)
			}
		}
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartConsoleSessionResult{}, fmt.Errorf("begin start console session transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions (
	id,
	task_id,
	change_id,
	workflow_run_id,
	node_run_id,
	job_id,
	lease_id,
	worker_id,
	role,
	workspace_mode,
	runtime_state,
	branch,
	base,
	harness,
	transcript_path,
	last_agent_activity_at,
	token_hash,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		nullableStringValue(job.TaskID),
		nullableStringValue(job.ChangeID),
		nullableStringValue(job.WorkflowRunID),
		nullableStringValue(job.NodeRunID),
		job.ID,
		lease.ID,
		workerID,
		string(flowworker.RoleConsole),
		string(WorkspaceChange),
		string(SessionStarting),
		branch,
		base,
		strings.TrimSpace(input.Harness),
		strings.TrimSpace(input.TranscriptPath),
		nil,
		tokenHash,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return StartConsoleSessionResult{}, fmt.Errorf("insert console session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StartConsoleSessionResult{}, fmt.Errorf("commit start console session transaction: %w", err)
	}
	committed = true

	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return StartConsoleSessionResult{}, err
	}
	return StartConsoleSessionResult{Session: session, Token: token}, nil
}

func (s *SessionService) UpdateSessionState(ctx context.Context, sessionID string, state SessionRuntimeState) (Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, errors.New("session id is required")
	}
	if err := validateSessionRuntimeState(state); err != nil {
		return Session{}, err
	}
	if state == SessionFinished {
		return s.ReadyAuthorSession(ctx, sessionID)
	}
	if _, err := s.ReconcileCrashedAuthorSessions(ctx); err != nil {
		return Session{}, err
	}

	now := s.now().UTC()
	var finishedAt any
	if isTerminalSessionState(state) {
		finishedAt = formatTime(now)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(?, finished_at)
WHERE id = ?
	AND runtime_state IN (?, ?, ?)`,
		string(state),
		formatTime(now),
		finishedAt,
		sessionID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	if err != nil {
		return Session{}, fmt.Errorf("update session state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("read session update rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, sql.ErrNoRows
	}

	return s.GetSession(ctx, sessionID)
}

// PauseAuthorSession abandons the task's live author session and cancels its
// running job so the worker slot is released until a human resumes the task.
func (s *SessionService) PauseAuthorSession(ctx context.Context, taskID string) (Session, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Session{}, errors.New("task id is required")
	}

	session, ok, err := s.ActiveAuthorSessionForTask(ctx, taskID)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, sql.ErrNoRows
	}

	nowText := formatTime(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin pause session transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?
	AND task_id = ?
	AND role = ?
	AND runtime_state IN (?, ?, ?)`,
		string(SessionAbandoned),
		nowText,
		nowText,
		session.ID,
		taskID,
		string(flowworker.RoleAuthor),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	if err != nil {
		return Session{}, fmt.Errorf("pause author session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("read pause session rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = COALESCE(released_at, ?)
WHERE id = ?
	AND released_at IS NULL`,
		nowText,
		session.LeaseID,
	); err != nil {
		return Session{}, fmt.Errorf("release paused session lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?, ?)`,
		string(flowworker.JobCanceled),
		nowText,
		session.JobID,
		string(flowworker.JobQueued),
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
	); err != nil {
		return Session{}, fmt.Errorf("cancel paused session job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit pause session transaction: %w", err)
	}
	if err := s.revokeSessionTokenHash(ctx, session.TokenHash); err != nil {
		slog.Warn("revoke paused session token", "session_id", session.ID, "error", err)
	}

	return s.GetSession(ctx, session.ID)
}

func (s *SessionService) UpdateConsoleSessionState(ctx context.Context, sessionID string, state SessionRuntimeState) (Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, errors.New("session id is required")
	}
	if state != SessionWorking && state != SessionWaiting {
		return Session{}, errors.New("console session state must be working or waiting")
	}
	if _, err := s.ReconcileCrashedConsoleSessions(ctx); err != nil {
		return Session{}, err
	}

	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?
WHERE id = ?
	AND role = ?
	AND runtime_state IN (?, ?, ?)`,
		string(state),
		formatTime(s.now().UTC()),
		sessionID,
		string(flowworker.RoleConsole),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	if err != nil {
		return Session{}, fmt.Errorf("update console session state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("read console session update rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, sql.ErrNoRows
	}

	return s.GetSession(ctx, sessionID)
}

// TouchAgentActivity stamps the moment an agent demonstrably did something
// (status write, hook event, state change, ready) — distinct from the worker
// lease heartbeat, which only proves the worker process is alive.
func (s *SessionService) TouchAgentActivity(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET last_agent_activity_at = ?
WHERE id = ?`,
		formatTime(s.now().UTC()),
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("touch agent activity: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read agent activity update rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// ProtectHumanWaitFromWatchdog records that the next watchdog-inferred working
// report may still be output from the status command that parked the session.
func (s *SessionService) ProtectHumanWaitFromWatchdog(ctx context.Context, sessionID string, kind string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	kind = strings.TrimSpace(kind)
	switch kind {
	case StatusKindPlan, StatusKindQuestion:
	default:
		return fmt.Errorf("human wait protection kind must be %s or %s", StatusKindPlan, StatusKindQuestion)
	}
	now := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO session_human_wait_latches (session_id, kind, created_at)
VALUES (?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
	kind = excluded.kind,
	created_at = excluded.created_at`,
		sessionID,
		kind,
		now,
	); err != nil {
		return fmt.Errorf("protect human wait from watchdog: %w", err)
	}

	return nil
}

// ConsumeHumanWaitWatchdogProtection consumes the one-shot watchdog guard for a
// session. It returns true when a pending guard was present.
func (s *SessionService) ConsumeHumanWaitWatchdogProtection(ctx context.Context, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, errors.New("session id is required")
	}
	result, err := s.db.ExecContext(ctx, `
DELETE FROM session_human_wait_latches
WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, fmt.Errorf("consume human wait watchdog protection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read human wait watchdog protection rows affected: %w", err)
	}

	return rows > 0, nil
}

// SetSessionTranscriptPath records where the coordinator stored the session's
// tmux transcript. It is keyed by session id.
func (s *SessionService) SetSessionTranscriptPath(ctx context.Context, sessionID string, path string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET transcript_path = ?,
	updated_at = ?
WHERE id = ?`,
		strings.TrimSpace(path),
		formatTime(s.now().UTC()),
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("set session transcript path: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read session transcript update rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// readySession finishes an author session, releases its lease and job, and
// revokes its token. markChangeReady additionally marks the session's change
// ready (the author-vs-planning distinction: a planning session readies without
// publishing a change).
func (s *SessionService) readySession(ctx context.Context, sessionID string, markChangeReady bool) (Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, errors.New("session id is required")
	}
	if _, err := s.ReconcileCrashedAuthorSessions(ctx); err != nil {
		return Session{}, err
	}

	now := s.now().UTC()
	nowText := formatTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin ready session transaction: %w", err)
	}
	defer tx.Rollback()

	session, err := scanSession(tx.QueryRowContext(ctx, sessionSelectSQL+`
WHERE id = ?`, sessionID))
	if err != nil {
		return Session{}, err
	}
	if session.Role != flowworker.RoleAuthor {
		return Session{}, errors.New("ready requires an author session")
	}
	if session.RuntimeState != SessionStarting && session.RuntimeState != SessionWorking && session.RuntimeState != SessionWaiting && session.RuntimeState != SessionFinished {
		return Session{}, errors.New("session is not readyable")
	}
	if session.RuntimeState != SessionFinished {
		var jobState string
		var releasedAt sql.NullString
		var expiresAtText string
		if err := tx.QueryRowContext(ctx, `
SELECT j.state, l.released_at, l.expires_at
FROM jobs j
JOIN leases l ON l.id = ?
WHERE j.id = ?`,
			session.LeaseID,
			session.JobID,
		).Scan(&jobState, &releasedAt, &expiresAtText); err != nil {
			return Session{}, fmt.Errorf("load ready lease: %w", err)
		}
		expiresAt, err := parseTime(expiresAtText)
		if err != nil {
			return Session{}, err
		}
		if jobState != string(flowworker.JobRunning) || releasedAt.Valid || !now.Before(expiresAt) {
			return Session{}, errors.New("session lease is not live")
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?`,
		string(SessionFinished),
		nowText,
		nowText,
		sessionID,
	); err != nil {
		return Session{}, fmt.Errorf("finish session: %w", err)
	}
	if markChangeReady {
		if _, err := tx.ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
			nowText,
			nowText,
			session.ChangeID,
		); err != nil {
			return Session{}, fmt.Errorf("mark change ready: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = COALESCE(released_at, ?)
WHERE id = ?`,
		nowText,
		session.LeaseID,
	); err != nil {
		return Session{}, fmt.Errorf("release session lease: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?, ?)`,
		string(flowworker.JobFinished),
		nowText,
		session.JobID,
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
		string(flowworker.JobFinished),
	)
	if err != nil {
		return Session{}, fmt.Errorf("finish session job: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("read finish job rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, errors.New("session job is not releasable")
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit ready session transaction: %w", err)
	}
	// The session token lives in the coordinator's global database; revoke it
	// after the project transaction commits.
	// Workflow completion is retryable with the session-scoped idempotency
	// identity. Keep that token valid until its normal expiry; every mutating
	// operation still verifies node ownership and active/succeeded state.
	if session.WorkflowRunID == "" {
		if err := s.revokeSessionTokenHash(ctx, session.TokenHash); err != nil {
			slog.Warn("revoke ready session token", "session_id", sessionID, "error", err)
		}
	}

	return s.GetSession(ctx, sessionID)
}

// ReadyAuthorSession finishes an author session and publishes its change.
func (s *SessionService) ReadyAuthorSession(ctx context.Context, sessionID string) (Session, error) {
	return s.readySession(ctx, sessionID, true)
}

// FinishWorkPhaseSession finishes an intermediate work phase's session without
// publishing the change: the phase produced a handoff, not a reviewable
// revision, so the change's ready latch stays closed until the final phase.
func (s *SessionService) FinishWorkPhaseSession(ctx context.Context, sessionID string) (Session, error) {
	return s.readySession(ctx, sessionID, false)
}

// ReadyChange publishes a change directly (sets its ready latch). Used when a
// human approves a final work phase that paused at a gate: the phase's session
// already finished at the pause, so there is no session to ready.
func (s *SessionService) ReadyChange(ctx context.Context, changeID string) (Change, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return Change{}, errors.New("change id is required")
	}
	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`, nowText, nowText, changeID); err != nil {
		return Change{}, fmt.Errorf("ready change: %w", err)
	}
	return s.GetChange(ctx, changeID)
}

// LatestChangeForTask returns the task's most recent change, ready or not.
func (s *SessionService) LatestChangeForTask(ctx context.Context, taskID string) (Change, bool, error) {
	changes, err := s.ListChangesForTask(ctx, taskID, 1)
	if err != nil {
		return Change{}, false, err
	}
	if len(changes) == 0 {
		return Change{}, false, nil
	}
	return changes[0], true, nil
}

func (s *SessionService) MarkPersistentSessionExited(ctx context.Context, input MarkPersistentSessionExitedInput) (Session, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	leaseID := strings.TrimSpace(input.LeaseID)
	if sessionID == "" || leaseID == "" {
		return Session{}, errors.New("session id and lease id are required")
	}
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	if session.LeaseID != leaseID {
		return Session{}, errors.New("lease does not belong to session")
	}
	if session.Role == flowworker.RoleConsole {
		return Session{}, errors.New("console sessions are released through console release")
	}
	if session.RuntimeState != SessionStarting && session.RuntimeState != SessionWorking && session.RuntimeState != SessionWaiting {
		return session, nil
	}
	// An interactive agent never exits cleanly on its own; reaching this point
	// means the session was not finalized (the expected flow workflow updates
	// were not made), so every un-finalized process exit is treated as a crash
	// regardless of exit code. ExitCode is load-bearing for diagnostics only:
	// logging it lets a clean-but-incomplete exit 0 be told apart from a hard
	// crash. Threading it into recordCrashRestartLimit is a follow-up (it crosses
	// the crash-counting boundary owned elsewhere).
	slog.Info("persistent session process exited",
		"session_id", sessionID,
		"lease_id", leaseID,
		"role", session.Role,
		"exit_code", input.ExitCode,
	)
	if _, err := s.workers.ReleaseLease(ctx, leaseID, flowworker.JobCrashed); err != nil {
		return Session{}, fmt.Errorf("release exited persistent session lease: %w", err)
	}

	nowText := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?
	AND runtime_state IN (?, ?, ?)`,
		string(SessionCrashed),
		nowText,
		nowText,
		sessionID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	); err != nil {
		return Session{}, fmt.Errorf("mark persistent session exited: %w", err)
	}
	if err := s.revokeSessionTokenHash(ctx, session.TokenHash); err != nil {
		return Session{}, err
	}

	return s.GetSession(ctx, sessionID)
}

func (s *SessionService) CurrentConsole(ctx context.Context) (ConsoleState, error) {
	var state ConsoleState
	if job, ok, err := s.liveConsoleJob(ctx); err != nil {
		return ConsoleState{}, err
	} else if ok {
		state.Job = &job
		state.Active = true
	}
	if session, ok, err := s.activeConsoleSession(ctx); err != nil {
		return ConsoleState{}, err
	} else if ok {
		state.Session = &session
		state.Active = true
		if state.Job == nil {
			if job, err := s.workers.GetJob(ctx, session.JobID); err == nil {
				state.Job = &job
			}
		}
		available, err := s.TerminalAvailable(ctx, session.ID)
		if err != nil {
			return ConsoleState{}, err
		}
		state.TerminalAvailable = available
		if available {
			terminalTarget, err := s.TerminalTarget(ctx, session.ID)
			if err != nil {
				return ConsoleState{}, err
			}
			state.Terminal = &terminalTarget
		}
	}

	return state, nil
}

func (s *SessionService) CurrentTaskConsole(ctx context.Context, taskID string) (ConsoleState, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ConsoleState{}, errors.New("task id is required")
	}
	var state ConsoleState
	if job, ok, err := s.liveTaskConsoleJob(ctx, taskID); err != nil {
		return ConsoleState{}, err
	} else if ok {
		state.Job = &job
		state.Active = true
	}
	if session, ok, err := s.activeTaskConsoleSession(ctx, taskID); err != nil {
		return ConsoleState{}, err
	} else if ok {
		state.Session = &session
		state.Active = true
		if state.Job == nil {
			if job, err := s.workers.GetJob(ctx, session.JobID); err == nil {
				state.Job = &job
			}
		}
		available, err := s.TerminalAvailable(ctx, session.ID)
		if err != nil {
			return ConsoleState{}, err
		}
		state.TerminalAvailable = available
		if available {
			terminalTarget, err := s.TerminalTarget(ctx, session.ID)
			if err != nil {
				return ConsoleState{}, err
			}
			state.Terminal = &terminalTarget
		}
	}

	return state, nil
}

func (s *SessionService) ReleaseConsole(ctx context.Context) (ConsoleState, error) {
	if _, err := s.ReconcileCrashedConsoleSessions(ctx); err != nil {
		return ConsoleState{}, err
	}
	state, err := s.CurrentConsole(ctx)
	if err != nil {
		return ConsoleState{}, err
	}
	if state.Session != nil {
		if _, err := s.finishConsoleSession(ctx, state.Session.ID); err != nil {
			return ConsoleState{}, err
		}
		return s.CurrentConsole(ctx)
	}
	if state.Job != nil {
		if err := s.cancelConsoleJob(ctx, state.Job.ID); err != nil {
			return ConsoleState{}, err
		}
		return s.CurrentConsole(ctx)
	}

	return ConsoleState{}, nil
}

func (s *SessionService) ReleaseTaskConsole(ctx context.Context, taskID string) (ConsoleState, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ConsoleState{}, errors.New("task id is required")
	}
	if _, err := s.ReconcileCrashedConsoleSessions(ctx); err != nil {
		return ConsoleState{}, err
	}
	state, err := s.CurrentTaskConsole(ctx, taskID)
	if err != nil {
		return ConsoleState{}, err
	}
	if state.Session != nil {
		if _, err := s.finishConsoleSession(ctx, state.Session.ID); err != nil {
			return ConsoleState{}, err
		}
		return s.CurrentTaskConsole(ctx, taskID)
	}
	if state.Job != nil {
		if err := s.cancelConsoleJob(ctx, state.Job.ID); err != nil {
			return ConsoleState{}, err
		}
		return s.CurrentTaskConsole(ctx, taskID)
	}

	return ConsoleState{}, nil
}

func (s *SessionService) finishConsoleSession(ctx context.Context, sessionID string) (Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, errors.New("session id is required")
	}

	now := s.now().UTC()
	nowText := formatTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin release console session transaction: %w", err)
	}
	defer tx.Rollback()

	session, err := scanSession(tx.QueryRowContext(ctx, sessionSelectSQL+`
WHERE id = ?`, sessionID))
	if err != nil {
		return Session{}, err
	}
	if session.Role != flowworker.RoleConsole {
		return Session{}, errors.New("console release requires a console session")
	}
	if session.RuntimeState != SessionStarting && session.RuntimeState != SessionWorking && session.RuntimeState != SessionWaiting && session.RuntimeState != SessionFinished {
		return Session{}, errors.New("console session is not releasable")
	}
	if session.RuntimeState != SessionFinished {
		var jobState string
		var releasedAt sql.NullString
		var expiresAtText string
		if err := tx.QueryRowContext(ctx, `
SELECT j.state, l.released_at, l.expires_at
FROM jobs j
JOIN leases l ON l.id = ?
WHERE j.id = ?`,
			session.LeaseID,
			session.JobID,
		).Scan(&jobState, &releasedAt, &expiresAtText); err != nil {
			return Session{}, fmt.Errorf("load console release lease: %w", err)
		}
		expiresAt, err := parseTime(expiresAtText)
		if err != nil {
			return Session{}, err
		}
		if jobState != string(flowworker.JobRunning) || releasedAt.Valid || !now.Before(expiresAt) {
			return Session{}, errors.New("console session lease is not live")
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET runtime_state = ?,
	updated_at = ?,
	finished_at = COALESCE(finished_at, ?)
WHERE id = ?`,
		string(SessionFinished),
		nowText,
		nowText,
		sessionID,
	); err != nil {
		return Session{}, fmt.Errorf("finish console session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = COALESCE(released_at, ?)
WHERE id = ?`,
		nowText,
		session.LeaseID,
	); err != nil {
		return Session{}, fmt.Errorf("release console lease: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?, ?)`,
		string(flowworker.JobFinished),
		nowText,
		session.JobID,
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
		string(flowworker.JobFinished),
	)
	if err != nil {
		return Session{}, fmt.Errorf("finish console job: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("read finish console job rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, errors.New("console job is not releasable")
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit release console session transaction: %w", err)
	}
	if err := s.revokeSessionTokenHash(ctx, session.TokenHash); err != nil {
		slog.Warn("revoke console token", "session_id", sessionID, "error", err)
	}

	return s.GetSession(ctx, sessionID)
}

// revokeSessionTokenHash revokes a session token in the global credential
// store, tolerating services constructed without credentials (tests that
// never mint tokens).
func (s *SessionService) revokeSessionTokenHash(ctx context.Context, tokenHash string) error {
	if s.credentials == nil || strings.TrimSpace(tokenHash) == "" {
		return nil
	}
	if err := s.credentials.RevokeTokenHash(ctx, tokenHash); err != nil {
		return fmt.Errorf("revoke session token: %w", err)
	}

	return nil
}

// RevokeWorkflowRunSessionTokens invalidates every session credential minted
// for a cancelled run. Project state is already durable when this is called;
// callers may log a credential-store failure without rolling the run back.
func (s *SessionService) RevokeWorkflowRunSessionTokens(ctx context.Context, workflowRunID string) error {
	workflowRunID = strings.TrimSpace(workflowRunID)
	if workflowRunID == "" || s.credentials == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash FROM sessions WHERE workflow_run_id = ?`, workflowRunID)
	if err != nil {
		return fmt.Errorf("list workflow session tokens: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return fmt.Errorf("scan workflow session token: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate workflow session tokens: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, hash := range hashes {
		if err := s.revokeSessionTokenHash(ctx, hash); err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionService) GetChange(ctx context.Context, changeID string) (Change, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE id = ?`, strings.TrimSpace(changeID))

	return scanChange(row)
}

func (s *SessionService) UpdateChangeHead(ctx context.Context, changeID string, headSHA string) (Change, error) {
	changeID = strings.TrimSpace(changeID)
	headSHA = strings.TrimSpace(headSHA)
	if changeID == "" {
		return Change{}, errors.New("change id is required")
	}
	if headSHA == "" {
		return Change{}, errors.New("head sha is required")
	}

	nowText := formatTime(s.now().UTC())
	result, err := s.db.ExecContext(ctx, `
UPDATE changes
SET head_sha = ?,
	updated_at = ?
WHERE id = ?`,
		headSHA,
		nowText,
		changeID,
	)
	if err != nil {
		return Change{}, fmt.Errorf("update change head: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Change{}, fmt.Errorf("read change head rows affected: %w", err)
	}
	if rows == 0 {
		return Change{}, sql.ErrNoRows
	}

	return s.GetChange(ctx, changeID)
}

func (s *SessionService) HasReadyUnmergedChange(ctx context.Context, taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, errors.New("task id is required")
	}

	return taskHasUnmergedChange(ctx, s.db, taskID)
}

func (s *SessionService) ReadyUnmergedChangeForTask(ctx context.Context, taskID string) (Change, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Change{}, false, errors.New("task id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE task_id = ?
	AND ready_at IS NOT NULL
	AND merged_at IS NULL
ORDER BY updated_at DESC, created_at DESC
LIMIT 1`, taskID)
	change, err := scanChange(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, false, nil
	}
	if err != nil {
		return Change{}, false, err
	}

	return change, true, nil
}

func (s *SessionService) ListChangesForTask(ctx context.Context, taskID string, limit int) ([]Change, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE task_id = ?
ORDER BY updated_at DESC, created_at DESC
LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("list task changes: %w", err)
	}
	defer rows.Close()

	var changes []Change
	for rows.Next() {
		change, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task changes: %w", err)
	}

	return changes, nil
}

func (s *SessionService) GetSession(ctx context.Context, sessionID string) (Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectSQL+`
WHERE id = ?`, strings.TrimSpace(sessionID))

	return scanSession(row)
}

func (s *SessionService) LatestSessionForJob(ctx context.Context, jobID string) (Session, bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return Session{}, false, errors.New("job id is required")
	}
	row := s.db.QueryRowContext(ctx, sessionSelectSQL+`
WHERE job_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`, jobID)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}

	return session, true, nil
}

func (s *SessionService) ListSessionsForTask(ctx context.Context, taskID string, limit int) ([]Session, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, sessionSelectSQL+`
WHERE task_id = ?
ORDER BY updated_at DESC, created_at DESC, id DESC
LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("list task sessions: %w", err)
	}
	return scanRows(rows, scanSession)
}

func (s *SessionService) ActiveAuthorSessionForTask(ctx context.Context, taskID string) (Session, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Session{}, false, errors.New("task id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT
	s.id,
	s.task_id,
	s.change_id,
	s.workflow_run_id,
	s.node_run_id,
	s.job_id,
	s.lease_id,
	s.worker_id,
	s.role,
	s.workspace_mode,
	s.runtime_state,
	s.branch,
	s.base,
	s.harness,
	s.transcript_path,
	s.last_agent_activity_at,
	s.token_hash,
	s.created_at,
	s.updated_at,
	s.finished_at
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
WHERE s.task_id = ?
	AND s.role = ?
	AND s.runtime_state IN (?, ?, ?)
	AND j.state = ?
	AND l.released_at IS NULL
	AND l.expires_at > ?
ORDER BY s.updated_at DESC, s.created_at DESC
LIMIT 1`,
		taskID,
		string(flowworker.RoleAuthor),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobRunning),
		formatTime(s.now().UTC()),
	)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}

	return session, true, nil
}

func (s *SessionService) AttachInfo(ctx context.Context, sessionID string) (terminal.AttachInfo, error) {
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return terminal.AttachInfo{}, err
	}
	if session.RuntimeState != SessionStarting && session.RuntimeState != SessionWorking && session.RuntimeState != SessionWaiting {
		return terminal.AttachInfo{}, errors.New("session is not attachable")
	}
	var jobState string
	var releasedAt sql.NullString
	var expiresAtText string
	if err := s.db.QueryRowContext(ctx, `
SELECT j.state, l.released_at, l.expires_at
FROM jobs j
JOIN leases l ON l.id = ?
WHERE j.id = ?`,
		session.LeaseID,
		session.JobID,
	).Scan(&jobState, &releasedAt, &expiresAtText); err != nil {
		return terminal.AttachInfo{}, fmt.Errorf("load attach lease: %w", err)
	}
	expiresAt, err := parseTime(expiresAtText)
	if err != nil {
		return terminal.AttachInfo{}, err
	}
	if jobState != string(flowworker.JobRunning) || releasedAt.Valid || !s.now().UTC().Before(expiresAt) {
		return terminal.AttachInfo{}, errors.New("session terminal is not live")
	}

	tmuxSocketPath := ""
	var storedSocketPath string
	if err := s.db.QueryRowContext(ctx, `
SELECT tmux_socket_path
FROM session_terminals
WHERE session_id = ?`, session.ID).Scan(&storedSocketPath); err == nil {
		tmuxSocketPath = storedSocketPath
	} else if !errors.Is(err, sql.ErrNoRows) {
		return terminal.AttachInfo{}, fmt.Errorf("load attach terminal: %w", err)
	}

	return terminal.AttachInfoForSession(session.ID, session.JobID, tmuxSocketPath), nil
}

func (s *SessionService) RegisterTerminal(ctx context.Context, sessionID string, tmuxSocketPath string) (SessionTerminal, error) {
	if _, err := s.AttachInfo(ctx, sessionID); err != nil {
		return SessionTerminal{}, err
	}
	tmuxSocketPath = strings.TrimSpace(tmuxSocketPath)
	if tmuxSocketPath == "" {
		return SessionTerminal{}, errors.New("tmux socket path is required")
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO session_terminals (
	session_id,
	tmux_socket_path,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
	tmux_socket_path = excluded.tmux_socket_path,
	updated_at = excluded.updated_at`,
		strings.TrimSpace(sessionID),
		tmuxSocketPath,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return SessionTerminal{}, fmt.Errorf("register terminal: %w", err)
	}

	return s.TerminalTarget(ctx, sessionID)
}

func (s *SessionService) TerminalTarget(ctx context.Context, sessionID string) (SessionTerminal, error) {
	if _, err := s.AttachInfo(ctx, sessionID); err != nil {
		return SessionTerminal{}, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT session_id, tmux_socket_path, created_at, updated_at
FROM session_terminals
WHERE session_id = ?`, strings.TrimSpace(sessionID))

	var terminalTarget SessionTerminal
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&terminalTarget.SessionID,
		&terminalTarget.TmuxSocketPath,
		&createdAt,
		&updatedAt,
	); err != nil {
		return SessionTerminal{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return SessionTerminal{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return SessionTerminal{}, err
	}
	terminalTarget.CreatedAt = parsedCreatedAt
	terminalTarget.UpdatedAt = parsedUpdatedAt

	return terminalTarget, nil
}

func (s *SessionService) TerminalAvailable(ctx context.Context, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, errors.New("session id is required")
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
JOIN session_terminals st ON st.session_id = s.id
WHERE s.id = ?
	AND s.runtime_state IN (?, ?, ?)
	AND j.state = ?
	AND l.released_at IS NULL
	AND l.expires_at > ?`,
		sessionID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobRunning),
		formatTime(s.now().UTC()),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check terminal availability: %w", err)
	}

	return count > 0, nil
}

func (s *SessionService) CreateTerminalAccess(ctx context.Context, sessionID string, ttl time.Duration) (SessionTerminalAccess, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SessionTerminalAccess{}, errors.New("session id is required")
	}
	if ttl <= 0 {
		return SessionTerminalAccess{}, errors.New("terminal access ttl is required")
	}
	if _, err := s.TerminalTarget(ctx, sessionID); err != nil {
		return SessionTerminalAccess{}, err
	}
	token, err := randomCredentialToken()
	if err != nil {
		return SessionTerminalAccess{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(ttl)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO terminal_access_tokens (
	token_hash,
	session_id,
	expires_at,
	created_at
) VALUES (?, ?, ?, ?)`,
		HashToken(token),
		sessionID,
		formatTime(expiresAt),
		formatTime(now),
	); err != nil {
		return SessionTerminalAccess{}, fmt.Errorf("create terminal access token: %w", err)
	}

	return SessionTerminalAccess{
		SessionID: sessionID,
		Token:     token,
		LoginPath: terminalLoginPath(sessionID, token),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *SessionService) ValidateTerminalAccess(ctx context.Context, sessionID string, token string) error {
	sessionID = strings.TrimSpace(sessionID)
	token = strings.TrimSpace(token)
	if sessionID == "" || token == "" {
		return ErrInvalidCredential
	}
	if _, err := s.AttachInfo(ctx, sessionID); err != nil {
		return err
	}
	var expiresAtText string
	if err := s.db.QueryRowContext(ctx, `
SELECT expires_at
FROM terminal_access_tokens
WHERE token_hash = ?
	AND session_id = ?`,
		HashToken(token),
		sessionID,
	).Scan(&expiresAtText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredential
		}
		return fmt.Errorf("validate terminal access token: %w", err)
	}
	expiresAt, err := parseTime(expiresAtText)
	if err != nil {
		return err
	}
	if !s.now().UTC().Before(expiresAt) {
		return ErrInvalidCredential
	}

	return nil
}

func (s *SessionService) ensureChange(ctx context.Context, taskID string, branch string, base string) (Change, error) {
	now := s.now().UTC()
	id, _, err := insertWithTaskChangeID(ctx, s.db, taskID, branch, "", func(id string) error {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO changes (
	id,
	task_id,
	branch,
	base,
	head_sha,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, '', ?, ?)`,
			id,
			taskID,
			branch,
			base,
			formatTime(now),
			formatTime(now),
		)
		return err
	})
	if err != nil {
		return Change{}, fmt.Errorf("insert change: %w", err)
	}

	change, err := s.GetChange(ctx, id)
	if err != nil {
		return Change{}, err
	}
	if change.Base != base {
		return Change{}, errors.New("existing change uses a different base")
	}
	return change, nil
}

func (s *SessionService) hasActiveAuthorSession(ctx context.Context, taskID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
WHERE s.task_id = ?
	AND s.role = ?
	AND s.runtime_state IN (?, ?, ?)
	AND j.state = ?
	AND l.released_at IS NULL
	AND l.expires_at > ?`,
		taskID,
		string(flowworker.RoleAuthor),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobRunning),
		formatTime(s.now().UTC()),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check active author session: %w", err)
	}

	return count > 0, nil
}

func (s *SessionService) liveConsoleJob(ctx context.Context) (flowworker.Job, bool, error) {
	jobs, err := s.workers.ListJobs(ctx)
	if err != nil {
		return flowworker.Job{}, false, err
	}
	for _, job := range jobs {
		if job.Role != flowworker.RoleConsole {
			continue
		}
		if job.TaskID != nil {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			return job, true, nil
		}
	}
	return flowworker.Job{}, false, nil
}

func (s *SessionService) liveTaskConsoleJob(ctx context.Context, taskID string) (flowworker.Job, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return flowworker.Job{}, false, errors.New("task id is required")
	}
	jobs, err := s.workers.ListJobs(ctx)
	if err != nil {
		return flowworker.Job{}, false, err
	}
	for _, job := range jobs {
		if job.Role != flowworker.RoleConsole || job.TaskID == nil || *job.TaskID != taskID {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			return job, true, nil
		}
	}
	return flowworker.Job{}, false, nil
}

func (s *SessionService) activeConsoleSession(ctx context.Context) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectSQL+`
WHERE role = ?
	AND task_id IS NULL
	AND runtime_state IN (?, ?, ?)
ORDER BY updated_at DESC, created_at DESC, id DESC
LIMIT 1`,
		string(flowworker.RoleConsole),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}

	return session, true, nil
}

func (s *SessionService) activeTaskConsoleSession(ctx context.Context, taskID string) (Session, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Session{}, false, errors.New("task id is required")
	}
	row := s.db.QueryRowContext(ctx, sessionSelectSQL+`
WHERE role = ?
	AND task_id = ?
	AND runtime_state IN (?, ?, ?)
ORDER BY updated_at DESC, created_at DESC, id DESC
LIMIT 1`,
		string(flowworker.RoleConsole),
		taskID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}

	return session, true, nil
}

func (s *SessionService) cancelConsoleJob(ctx context.Context, jobID string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("job id is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cancel console transaction: %w", err)
	}
	defer tx.Rollback()

	var role string
	var state string
	if err := tx.QueryRowContext(ctx, `
SELECT role, state
FROM jobs
WHERE id = ?`, jobID).Scan(&role, &state); err != nil {
		return err
	}
	if flowworker.JobRole(role) != flowworker.RoleConsole {
		return errors.New("job is not a console job")
	}
	switch flowworker.JobState(state) {
	case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
	default:
		return nil
	}

	now := formatTime(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = COALESCE(released_at, ?)
WHERE job_id = ?
	AND released_at IS NULL`, now, jobID); err != nil {
		return fmt.Errorf("release console job leases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?, ?)`,
		string(flowworker.JobCanceled),
		now,
		jobID,
		string(flowworker.JobQueued),
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
	); err != nil {
		return fmt.Errorf("cancel console job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cancel console transaction: %w", err)
	}

	return nil
}

// ActiveAuthorSessionState reports the runtime state of the task's live author
// session, if one exists. It is the exported entry point the lifecycle engine
// uses to derive the planning / authoring phases.
func (s *SessionService) ActiveAuthorSessionState(ctx context.Context, taskID string) (SessionRuntimeState, bool, error) {
	return activeSessionStateForTask(ctx, s.db, taskID)
}

func activeSessionStateForTask(ctx context.Context, db *sql.DB, taskID string) (SessionRuntimeState, bool, error) {
	var state string
	if err := db.QueryRowContext(ctx, `
SELECT s.runtime_state
FROM sessions s
JOIN jobs j ON j.id = s.job_id
JOIN leases l ON l.id = s.lease_id
WHERE s.task_id = ?
	AND s.role = ?
	AND s.runtime_state IN (?, ?, ?)
	AND j.state = ?
	AND l.released_at IS NULL
	AND l.expires_at > ?
ORDER BY s.updated_at DESC
LIMIT 1`,
		taskID,
		string(flowworker.RoleAuthor),
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
		string(flowworker.JobRunning),
		formatTime(time.Now().UTC()),
	).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load active session state: %w", err)
	}

	return SessionRuntimeState(state), true, nil
}

func taskHasUnmergedChange(ctx context.Context, db *sql.DB, taskID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM changes
WHERE task_id = ?
	AND ready_at IS NOT NULL
	AND merged_at IS NULL`, taskID).Scan(&count); err != nil {
		return false, fmt.Errorf("check ready unmerged change: %w", err)
	}

	return count > 0, nil
}

func scanChange(scanner taskScanner) (Change, error) {
	var change Change
	var createdAt string
	var updatedAt string
	var readyAt sql.NullString
	var mergedAt sql.NullString
	if err := scanner.Scan(
		&change.ID,
		&change.TaskID,
		&change.Branch,
		&change.Base,
		&change.HeadSHA,
		&createdAt,
		&updatedAt,
		&readyAt,
		&mergedAt,
	); err != nil {
		return Change{}, fmt.Errorf("scan change: %w", err)
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Change{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Change{}, err
	}
	change.CreatedAt = parsedCreatedAt
	change.UpdatedAt = parsedUpdatedAt
	if readyAt.Valid {
		parsedReadyAt, err := parseTime(readyAt.String)
		if err != nil {
			return Change{}, err
		}
		change.ReadyAt = &parsedReadyAt
	}
	if mergedAt.Valid {
		parsedMergedAt, err := parseTime(mergedAt.String)
		if err != nil {
			return Change{}, err
		}
		change.MergedAt = &parsedMergedAt
	}

	return change, nil
}

// sessionSelectSQL is the canonical full-column session projection scanned by
// scanSession. Callers append their own WHERE/ORDER/LIMIT clauses.
const sessionSelectSQL = `
SELECT
	id,
	task_id,
	change_id,
	workflow_run_id,
	node_run_id,
	job_id,
	lease_id,
	worker_id,
	role,
	workspace_mode,
	runtime_state,
	branch,
	base,
	harness,
	transcript_path,
	last_agent_activity_at,
	token_hash,
	created_at,
	updated_at,
	finished_at
FROM sessions`

func scanSession(scanner taskScanner) (Session, error) {
	var session Session
	var taskID sql.NullString
	var changeID sql.NullString
	var workflowRunID sql.NullString
	var nodeRunID sql.NullString
	var workspaceMode string
	var role string
	var runtimeState string
	var lastAgentActivityAt sql.NullString
	var createdAt string
	var updatedAt string
	var finishedAt sql.NullString
	if err := scanner.Scan(
		&session.ID,
		&taskID,
		&changeID,
		&workflowRunID,
		&nodeRunID,
		&session.JobID,
		&session.LeaseID,
		&session.WorkerID,
		&role,
		&workspaceMode,
		&runtimeState,
		&session.Branch,
		&session.Base,
		&session.Harness,
		&session.TranscriptPath,
		&lastAgentActivityAt,
		&session.TokenHash,
		&createdAt,
		&updatedAt,
		&finishedAt,
	); err != nil {
		return Session{}, fmt.Errorf("scan session: %w", err)
	}
	if taskID.Valid {
		session.TaskID = strings.TrimSpace(taskID.String)
	}
	if changeID.Valid {
		session.ChangeID = strings.TrimSpace(changeID.String)
	}
	session.WorkflowRunID = strings.TrimSpace(workflowRunID.String)
	session.NodeRunID = strings.TrimSpace(nodeRunID.String)
	session.WorkspaceMode = WorkspaceMode(workspaceMode)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Session{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Session{}, err
	}
	session.Role = flowworker.JobRole(role)
	session.RuntimeState = SessionRuntimeState(runtimeState)
	session.CreatedAt = parsedCreatedAt
	session.UpdatedAt = parsedUpdatedAt
	if lastAgentActivityAt.Valid {
		parsedLastAgentActivityAt, err := parseTime(lastAgentActivityAt.String)
		if err != nil {
			return Session{}, err
		}
		session.LastAgentActivityAt = &parsedLastAgentActivityAt
	}
	if finishedAt.Valid {
		parsedFinishedAt, err := parseTime(finishedAt.String)
		if err != nil {
			return Session{}, err
		}
		session.FinishedAt = &parsedFinishedAt
	}

	return session, nil
}

func authorJobMatches(job flowworker.Job, changeID string, branch string, base string, agentHarness string, phaseIndex int, defaultHarness string) bool {
	jobHarness := payloadString(job.Payload, "agent_harness")
	if jobHarness == "" {
		jobHarness = defaultHarness
	}
	agentHarness = flowharness.NormalizeName(agentHarness)
	if agentHarness == "" {
		agentHarness = defaultHarness
	}
	return stringPointerValue(job.ChangeID) == changeID &&
		payloadString(job.Payload, "branch") == branch &&
		payloadString(job.Payload, "base") == base &&
		jobHarness == agentHarness &&
		payloadPhaseIndex(job.Payload) == phaseIndex
}

// payloadPhaseIndex reads the job's work-phase index (0 when absent: the
// implicit single phase). JSON round-trips numbers as float64.
func payloadPhaseIndex(payload map[string]any) int {
	value, ok := payload["phase_index"]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}

	return strings.TrimSpace(*value)
}

func payloadString(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func payloadBool(payload map[string]any, key string) bool {
	value, ok := payload[key]
	if !ok || value == nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func copyPayload(payload map[string]any) map[string]any {
	copied := make(map[string]any, len(payload)+4)
	for key, value := range payload {
		copied[key] = value
	}

	return copied
}

// TaskImageAttachment is the attachment descriptor the coordinator stamps onto
// an author job payload so the worker can materialize the image bytes in the
// worktree. It deliberately carries only the attachment ID and filename (not the
// content type or bytes): the coordinator already filtered to image types via
// IsImageContentType, and the worker downloads the bytes from the exchange.
// Every agent harness receives this list; whether the bytes become a CLI flag
// is a worker-side, harness-specific concern.
type TaskImageAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

// stampImageAttachments loads an task's attachments, filters to image content
// types, and stamps the resulting {id, filename} descriptors onto the author
// payload under image_attachments. It is agent-agnostic: the worker
// materializes the bytes for any harness, and only the harness CLI gets
// --image flags. A nil attachment store (no task service) is tolerated by
// stamping an empty list so the payload shape is stable.
func stampImageAttachments(ctx context.Context, tasks *TaskService, payload map[string]any, taskID string) error {
	if payload == nil {
		return nil
	}
	descriptors := []TaskImageAttachment{}
	if tasks != nil {
		attachments, err := tasks.ListTaskAttachments(ctx, taskID)
		if err != nil {
			return fmt.Errorf("load task attachments: %w", err)
		}
		for _, attachment := range attachments {
			if !IsImageContentType(attachment.ContentType) {
				continue
			}
			descriptors = append(descriptors, TaskImageAttachment{
				ID:       attachment.ID,
				Filename: attachment.Filename,
			})
		}
	}
	payload["image_attachments"] = descriptors
	return nil
}

// defaultAuthorEntrypoint builds the fallback author entrypoint for a
// resolved default agent selection (the selection's harness with its
// model/effort tokens applied).
func defaultAuthorEntrypoint(sel flowharness.AgentSelection) map[string]any {
	modelTokens, err := sel.ModelArgs()
	if err != nil {
		panic(err)
	}
	entrypoint, err := flowharness.DefaultAuthorEntrypointWithArgs(sel.Harness, modelTokens)
	if err != nil {
		panic(err)
	}
	return entrypoint
}

func taskBranch(taskID string) string {
	return "task/" + strings.TrimSpace(taskID)
}

func terminalLoginPath(sessionID string, token string) string {
	query := url.Values{}
	query.Set("token", token)
	return terminal.TerminalProxyPath(sessionID) + "-login?" + query.Encode()
}

func validateBranchLike(kind string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.ContainsAny(value, " \t\n\r") {
		return fmt.Errorf("%s %q is not supported", kind, value)
	}

	return nil
}

func validateSessionRuntimeState(state SessionRuntimeState) error {
	switch state {
	case SessionStarting, SessionWorking, SessionWaiting, SessionFinished, SessionCrashed, SessionAbandoned:
		return nil
	default:
		return fmt.Errorf("invalid session runtime state: %s", state)
	}
}

func isTerminalSessionState(state SessionRuntimeState) bool {
	switch state {
	case SessionFinished, SessionCrashed, SessionAbandoned:
		return true
	default:
		return false
	}
}

func randomPrefixedID(prefix string) (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}

	return prefix + "-" + hex.EncodeToString(bytes), nil
}
