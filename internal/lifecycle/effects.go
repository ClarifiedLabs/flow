package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// ErrMergeUnavailable is returned by the merge effects when no merge service is
// configured (a minimal server). It mirrors the original handlers'
// "merges_unavailable" degradation rather than panicking on a nil service.
var ErrMergeUnavailable = errors.New("lifecycle: merge service unavailable")

// Effects is the seam between the FSM and the outside world. Every side effect a
// transition action performs goes through this interface so the engine stays a
// deterministic reducer over (event, snapshot) and can be exercised with a fake
// in tests. Each method wraps an existing coordinator service call verbatim and
// owns its own transaction; the engine never holds a transaction across an
// effect.
type Effects interface {
	// Task setters / reads.
	GetTask(ctx context.Context, id string) (coordinator.Task, error)
	HasMergedChange(ctx context.Context, taskID string) (bool, error)
	ScheduleTask(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Task, error)
	SetTaskState(ctx context.Context, id string, state coordinator.TaskState) (coordinator.Task, error)
	AcceptTriage(ctx context.Context, id string) (coordinator.Task, error)
	RejectTriage(ctx context.Context, id string) (coordinator.Task, error)
	CloseTask(ctx context.Context, taskID string) (coordinator.Task, error)

	// Author / ready cascade.
	GetSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	GetChange(ctx context.Context, changeID string) (coordinator.Change, error)
	ReadyAuthorSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	// FinishWorkPhaseSession finishes an intermediate work phase's session
	// without publishing the change.
	FinishWorkPhaseSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	// ReadyChange publishes a change directly (final-phase gate approval,
	// where the phase's session already finished at the pause).
	ReadyChange(ctx context.Context, changeID string) (coordinator.Change, error)
	LatestChangeForTask(ctx context.Context, taskID string) (coordinator.Change, bool, error)
	UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error)
	UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error)
	ResetAutomatedChecksForNewRevision(ctx context.Context, taskID string) (int, error)
	LoadSuiteForChange(ctx context.Context, change coordinator.Change) (coordinator.CheckSuite, error)
	ScheduleReviewRound(ctx context.Context, input coordinator.ScheduleReviewRoundInput) (coordinator.ScheduleReviewRoundResult, error)

	// Flow cursor: the task's frozen position within its flow. Mutations are
	// CAS on the phase index so at-least-once redelivery stays idempotent.
	FlowCursor(ctx context.Context, taskID string) (coordinator.FlowCursor, bool, error)
	// EnsureFlowCursor freezes a cursor from the task's flow on first use; ok
	// is false when no flow could be resolved (implicit single-phase behavior).
	EnsureFlowCursor(ctx context.Context, taskID string) (coordinator.FlowCursor, bool, error)
	AdvanceFlowCursor(ctx context.Context, taskID string, fromIndex int) (bool, error)
	PauseFlowCursor(ctx context.Context, taskID string, atIndex int) (bool, error)
	ResumeFlowCursor(ctx context.Context, taskID string, atIndex int, feedback string) (bool, error)
	CompleteFlowCursor(ctx context.Context, taskID string, atIndex int) (bool, error)
	StorePhaseHandoff(ctx context.Context, input coordinator.StorePhaseHandoffInput) error
	PhaseHandoff(ctx context.Context, taskID string, phaseIndex int) (coordinator.PhaseHandoff, bool, error)
	// ChangeHandoff reads the change-scoped handoff snapshot the agent
	// submitted at flow ready (the source copied into the per-phase store).
	ChangeHandoff(ctx context.Context, changeID string) (coordinator.HandoffSnapshot, bool, error)

	// Checks.
	ReportCheck(ctx context.Context, input coordinator.ReportCheckInput) (coordinator.Check, error)
	GetCheck(ctx context.Context, taskID, name string) (coordinator.Check, error)
	ReviewState(ctx context.Context, taskID string) (coordinator.ReviewState, error)
	HasReadyUnmergedChange(ctx context.Context, taskID string) (bool, error)
	ReadyUnmergedChangeForTask(ctx context.Context, taskID string) (coordinator.Change, bool, error)
	ActiveAuthorSessionState(ctx context.Context, taskID string) (coordinator.SessionRuntimeState, bool, error)
	// EnqueueAcceptanceIfReady enqueues acceptance-phase check jobs once the
	// critique gate is met and returns the names of the checks it enqueued.
	EnqueueAcceptanceIfReady(ctx context.Context, taskID string, change coordinator.Change) ([]string, error)
	AcceptancePending(ctx context.Context, taskID string) (bool, error)

	// Author jobs. The raw coordinator error (including ErrAuthorJobSuppressed)
	// is returned; the caller decides whether suppression is benign.
	EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error)
	// ResetTask discards the task's authoring artifacts (jobs, sessions,
	// changes, checks, exchange branches) so the next author job starts over
	// from the base branch.
	ResetTask(ctx context.Context, taskID string) (coordinator.Task, error)
	RetryCrashedAuthorJob(ctx context.Context, taskID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error)

	// Merge.
	MergeTask(ctx context.Context, taskID string) (coordinator.MergeResult, error)
	MergeChange(ctx context.Context, changeID string) (coordinator.MergeResult, error)

	// Review threads.
	GetThread(ctx context.Context, threadID string) (coordinator.ReviewThread, error)
	ClaimThread(ctx context.Context, input coordinator.ClaimThreadInput) (coordinator.ReviewThread, error)
	CertifyThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error)
	ReopenThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error)
	AddComment(ctx context.Context, input coordinator.AddThreadCommentInput) (coordinator.ReviewThread, error)

	// Deadlines.
	// LastAgentActivity returns the active author session's last agent-activity
	// timestamp for the task; ok is false when no active author session exists.
	LastAgentActivity(ctx context.Context, taskID string) (*time.Time, bool, error)
	// WriteStatus records a status-log entry (used by the deadline actions to
	// surface a blocker/question to a human).
	WriteStatus(ctx context.Context, input coordinator.WriteStatusInput) error

	// Crash recovery (timer-driven, Phase 4).
	ReconcileCrashedAuthorSessions(ctx context.Context) (int, error)
	// RecoverPendingCheckJobs re-enqueues missing automated check jobs and
	// returns the pending checks expecting a job report so the engine can arm a
	// check timeout for any review round scheduled outside it (Mode-B completion
	// review).
	RecoverPendingCheckJobs(ctx context.Context) (int, []coordinator.PendingCheckTimeout, error)
	RecoverPendingMerges(ctx context.Context) (int, error)
}

// liveEffects is the production Effects implementation: thin pass-throughs to the
// coordinator services already wired into the API server.
type liveEffects struct {
	tasks        *coordinator.TaskService
	checks       *coordinator.CheckService
	checkConfigs *coordinator.CheckConfigService
	sessions     *coordinator.SessionService
	merges       *coordinator.MergeService
	threads      *coordinator.ThreadService
	status       *coordinator.StatusService
	cursors      *coordinator.FlowCursorService
	reconciler   *coordinator.ReconcileService
}

// NewEffects builds the production Effects from the existing coordinator services.
func NewEffects(
	tasks *coordinator.TaskService,
	checks *coordinator.CheckService,
	checkConfigs *coordinator.CheckConfigService,
	sessions *coordinator.SessionService,
	merges *coordinator.MergeService,
	threads *coordinator.ThreadService,
	status *coordinator.StatusService,
	cursors *coordinator.FlowCursorService,
	reconciler *coordinator.ReconcileService,
) Effects {
	return &liveEffects{
		tasks:        tasks,
		checks:       checks,
		checkConfigs: checkConfigs,
		sessions:     sessions,
		merges:       merges,
		threads:      threads,
		status:       status,
		cursors:      cursors,
		reconciler:   reconciler,
	}
}

func (e *liveEffects) GetTask(ctx context.Context, id string) (coordinator.Task, error) {
	return e.tasks.GetTask(ctx, id)
}

func (e *liveEffects) HasMergedChange(ctx context.Context, taskID string) (bool, error) {
	return e.tasks.HasMergedChange(ctx, taskID)
}

func (e *liveEffects) ScheduleTask(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Task, error) {
	return e.tasks.ScheduleTask(ctx, id, state)
}

func (e *liveEffects) SetTaskState(ctx context.Context, id string, state coordinator.TaskState) (coordinator.Task, error) {
	return e.tasks.SetTaskState(ctx, id, state)
}

func (e *liveEffects) AcceptTriage(ctx context.Context, id string) (coordinator.Task, error) {
	return e.tasks.AcceptTriage(ctx, id)
}

func (e *liveEffects) RejectTriage(ctx context.Context, id string) (coordinator.Task, error) {
	return e.tasks.RejectTriage(ctx, id)
}

func (e *liveEffects) CloseTask(ctx context.Context, taskID string) (coordinator.Task, error) {
	return e.tasks.CloseTask(ctx, taskID)
}

func (e *liveEffects) GetSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	return e.sessions.GetSession(ctx, sessionID)
}

func (e *liveEffects) GetChange(ctx context.Context, changeID string) (coordinator.Change, error) {
	return e.sessions.GetChange(ctx, changeID)
}

func (e *liveEffects) ReadyAuthorSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	return e.sessions.ReadyAuthorSession(ctx, sessionID)
}

func (e *liveEffects) FinishWorkPhaseSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	return e.sessions.FinishWorkPhaseSession(ctx, sessionID)
}

func (e *liveEffects) ReadyChange(ctx context.Context, changeID string) (coordinator.Change, error) {
	return e.sessions.ReadyChange(ctx, changeID)
}

func (e *liveEffects) LatestChangeForTask(ctx context.Context, taskID string) (coordinator.Change, bool, error) {
	return e.sessions.LatestChangeForTask(ctx, taskID)
}

func (e *liveEffects) FlowCursor(ctx context.Context, taskID string) (coordinator.FlowCursor, bool, error) {
	if e.cursors == nil {
		return coordinator.FlowCursor{}, false, nil
	}
	return e.cursors.GetCursor(ctx, taskID)
}

func (e *liveEffects) EnsureFlowCursor(ctx context.Context, taskID string) (coordinator.FlowCursor, bool, error) {
	if e.cursors == nil {
		return coordinator.FlowCursor{}, false, nil
	}
	return e.cursors.EnsureCursor(ctx, taskID)
}

func (e *liveEffects) AdvanceFlowCursor(ctx context.Context, taskID string, fromIndex int) (bool, error) {
	if e.cursors == nil {
		return false, nil
	}
	return e.cursors.AdvanceCursor(ctx, taskID, fromIndex)
}

func (e *liveEffects) PauseFlowCursor(ctx context.Context, taskID string, atIndex int) (bool, error) {
	if e.cursors == nil {
		return false, nil
	}
	return e.cursors.PauseCursor(ctx, taskID, atIndex)
}

func (e *liveEffects) ResumeFlowCursor(ctx context.Context, taskID string, atIndex int, feedback string) (bool, error) {
	if e.cursors == nil {
		return false, nil
	}
	return e.cursors.ResumeCursor(ctx, taskID, atIndex, feedback)
}

func (e *liveEffects) CompleteFlowCursor(ctx context.Context, taskID string, atIndex int) (bool, error) {
	if e.cursors == nil {
		return false, nil
	}
	return e.cursors.CompleteCursor(ctx, taskID, atIndex)
}

func (e *liveEffects) StorePhaseHandoff(ctx context.Context, input coordinator.StorePhaseHandoffInput) error {
	if e.cursors == nil {
		return nil
	}
	return e.cursors.StorePhaseHandoff(ctx, input)
}

func (e *liveEffects) PhaseHandoff(ctx context.Context, taskID string, phaseIndex int) (coordinator.PhaseHandoff, bool, error) {
	if e.cursors == nil {
		return coordinator.PhaseHandoff{}, false, nil
	}
	return e.cursors.PhaseHandoff(ctx, taskID, phaseIndex)
}

func (e *liveEffects) ChangeHandoff(ctx context.Context, changeID string) (coordinator.HandoffSnapshot, bool, error) {
	if e.reconciler == nil {
		return coordinator.HandoffSnapshot{}, false, nil
	}
	snapshot, err := e.reconciler.GetHandoffSnapshot(ctx, changeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coordinator.HandoffSnapshot{}, false, nil
		}
		return coordinator.HandoffSnapshot{}, false, err
	}
	return snapshot, snapshot.Present, nil
}

func (e *liveEffects) UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	return e.sessions.UpdateSessionState(ctx, sessionID, state)
}

func (e *liveEffects) UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error) {
	return e.sessions.UpdateChangeHead(ctx, changeID, headSHA)
}

func (e *liveEffects) ResetAutomatedChecksForNewRevision(ctx context.Context, taskID string) (int, error) {
	return e.checks.ResetAutomatedChecksForNewRevision(ctx, taskID)
}

func (e *liveEffects) LoadSuiteForChange(ctx context.Context, change coordinator.Change) (coordinator.CheckSuite, error) {
	if e.checkConfigs == nil {
		return coordinator.CheckSuite{}, nil
	}
	return e.checkConfigs.LoadSuiteForChange(ctx, change)
}

func (e *liveEffects) ScheduleReviewRound(ctx context.Context, input coordinator.ScheduleReviewRoundInput) (coordinator.ScheduleReviewRoundResult, error) {
	if e.checkConfigs == nil {
		return coordinator.ScheduleReviewRoundResult{}, nil
	}
	return e.checkConfigs.ScheduleReviewRound(ctx, input)
}

func (e *liveEffects) ReportCheck(ctx context.Context, input coordinator.ReportCheckInput) (coordinator.Check, error) {
	return e.checks.ReportCheck(ctx, input)
}

func (e *liveEffects) GetCheck(ctx context.Context, taskID, name string) (coordinator.Check, error) {
	return e.checks.GetCheck(ctx, taskID, name)
}

func (e *liveEffects) ReviewState(ctx context.Context, taskID string) (coordinator.ReviewState, error) {
	return e.checks.ReviewState(ctx, taskID)
}

func (e *liveEffects) HasReadyUnmergedChange(ctx context.Context, taskID string) (bool, error) {
	return e.sessions.HasReadyUnmergedChange(ctx, taskID)
}

func (e *liveEffects) ReadyUnmergedChangeForTask(ctx context.Context, taskID string) (coordinator.Change, bool, error) {
	return e.sessions.ReadyUnmergedChangeForTask(ctx, taskID)
}

func (e *liveEffects) ActiveAuthorSessionState(ctx context.Context, taskID string) (coordinator.SessionRuntimeState, bool, error) {
	return e.sessions.ActiveAuthorSessionState(ctx, taskID)
}

func (e *liveEffects) EnqueueAcceptanceIfReady(ctx context.Context, taskID string, change coordinator.Change) ([]string, error) {
	if e.checkConfigs == nil {
		return nil, nil
	}
	return e.checkConfigs.EnqueueAcceptanceIfReady(ctx, taskID, change)
}

func (e *liveEffects) AcceptancePending(ctx context.Context, taskID string) (bool, error) {
	if e.checkConfigs == nil {
		return false, nil
	}
	return e.checkConfigs.AcceptancePending(ctx, taskID)
}

func (e *liveEffects) EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error) {
	return e.sessions.EnsureAuthorJob(ctx, input)
}

func (e *liveEffects) ResetTask(ctx context.Context, taskID string) (coordinator.Task, error) {
	return e.sessions.ResetTask(ctx, taskID)
}

func (e *liveEffects) RetryCrashedAuthorJob(ctx context.Context, taskID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error) {
	return e.sessions.RetryCrashedAuthorJob(ctx, taskID, actor)
}

func (e *liveEffects) MergeTask(ctx context.Context, taskID string) (coordinator.MergeResult, error) {
	if e.merges == nil {
		return coordinator.MergeResult{}, ErrMergeUnavailable
	}
	return e.merges.MergeTask(ctx, taskID)
}

func (e *liveEffects) MergeChange(ctx context.Context, changeID string) (coordinator.MergeResult, error) {
	if e.merges == nil {
		return coordinator.MergeResult{}, ErrMergeUnavailable
	}
	return e.merges.MergeChange(ctx, changeID)
}

func (e *liveEffects) GetThread(ctx context.Context, threadID string) (coordinator.ReviewThread, error) {
	return e.threads.GetThread(ctx, threadID)
}

func (e *liveEffects) ClaimThread(ctx context.Context, input coordinator.ClaimThreadInput) (coordinator.ReviewThread, error) {
	return e.threads.ClaimThread(ctx, input)
}

func (e *liveEffects) CertifyThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	return e.threads.CertifyThread(ctx, input)
}

func (e *liveEffects) ReopenThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error) {
	return e.threads.ReopenThread(ctx, input)
}

func (e *liveEffects) AddComment(ctx context.Context, input coordinator.AddThreadCommentInput) (coordinator.ReviewThread, error) {
	return e.threads.AddComment(ctx, input)
}

func (e *liveEffects) LastAgentActivity(ctx context.Context, taskID string) (*time.Time, bool, error) {
	session, ok, err := e.sessions.ActiveAuthorSessionForTask(ctx, taskID)
	if err != nil || !ok {
		return nil, false, err
	}
	return session.LastAgentActivityAt, true, nil
}

func (e *liveEffects) WriteStatus(ctx context.Context, input coordinator.WriteStatusInput) error {
	if e.status == nil {
		return nil
	}
	_, err := e.status.Write(ctx, input)
	return err
}

func (e *liveEffects) ReconcileCrashedAuthorSessions(ctx context.Context) (int, error) {
	return e.sessions.ReconcileCrashedAuthorSessions(ctx)
}

func (e *liveEffects) RecoverPendingCheckJobs(ctx context.Context) (int, []coordinator.PendingCheckTimeout, error) {
	if e.checkConfigs == nil {
		return 0, nil, nil
	}
	return e.checkConfigs.RecoverPendingCheckJobs(ctx)
}

func (e *liveEffects) RecoverPendingMerges(ctx context.Context) (int, error) {
	if e.merges == nil {
		return 0, nil
	}
	return e.merges.RecoverPendingMerges(ctx)
}
