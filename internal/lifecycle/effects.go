package lifecycle

import (
	"context"
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
	// Issue setters / reads.
	GetIssue(ctx context.Context, id string) (coordinator.Issue, error)
	HasMergedChange(ctx context.Context, issueID string) (bool, error)
	ScheduleIssue(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Issue, error)
	SetIssueState(ctx context.Context, id string, state coordinator.IssueState) (coordinator.Issue, error)
	AcceptTriage(ctx context.Context, id string) (coordinator.Issue, error)
	RejectTriage(ctx context.Context, id string) (coordinator.Issue, error)
	CloseIssue(ctx context.Context, issueID string) (coordinator.Issue, error)

	// Author / ready cascade.
	GetSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	GetChange(ctx context.Context, changeID string) (coordinator.Change, error)
	ReadyAuthorSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	ReadyPlanningSession(ctx context.Context, sessionID string) (coordinator.Session, error)
	MarkPlanApproved(ctx context.Context, issueID string) (coordinator.Issue, error)
	UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error)
	UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error)
	ResetAutomatedChecksForNewRevision(ctx context.Context, issueID string) (int, error)
	LoadSuiteForChange(ctx context.Context, change coordinator.Change) (coordinator.CheckSuite, error)
	ScheduleReviewRound(ctx context.Context, input coordinator.ScheduleReviewRoundInput) (coordinator.ScheduleReviewRoundResult, error)

	// Checks.
	ReportCheck(ctx context.Context, input coordinator.ReportCheckInput) (coordinator.Check, error)
	GetCheck(ctx context.Context, issueID, name string) (coordinator.Check, error)
	ReviewState(ctx context.Context, issueID string) (coordinator.ReviewState, error)
	HasReadyUnmergedChange(ctx context.Context, issueID string) (bool, error)
	ReadyUnmergedChangeForIssue(ctx context.Context, issueID string) (coordinator.Change, bool, error)
	ActiveAuthorSessionState(ctx context.Context, issueID string) (coordinator.SessionRuntimeState, bool, error)
	// EnqueueAcceptanceIfReady enqueues acceptance-phase check jobs once the
	// critique gate is met and returns the names of the checks it enqueued.
	EnqueueAcceptanceIfReady(ctx context.Context, issueID string, change coordinator.Change) ([]string, error)
	AcceptancePending(ctx context.Context, issueID string) (bool, error)

	// Author jobs. The raw coordinator error (including ErrAuthorJobSuppressed)
	// is returned; the caller decides whether suppression is benign.
	EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error)
	// ResetIssue discards the issue's authoring artifacts (jobs, sessions,
	// changes, checks, exchange branches) so the next author job starts over
	// from the base branch.
	ResetIssue(ctx context.Context, issueID string) (coordinator.Issue, error)
	RetryCrashedAuthorJob(ctx context.Context, issueID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error)

	// Merge.
	MergeIssue(ctx context.Context, issueID string) (coordinator.MergeResult, error)
	MergeChange(ctx context.Context, changeID string) (coordinator.MergeResult, error)

	// Review threads.
	GetThread(ctx context.Context, threadID string) (coordinator.ReviewThread, error)
	ClaimThread(ctx context.Context, input coordinator.ClaimThreadInput) (coordinator.ReviewThread, error)
	CertifyThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error)
	ReopenThread(ctx context.Context, input coordinator.VerifyThreadInput) (coordinator.ReviewThread, error)
	AddComment(ctx context.Context, input coordinator.AddThreadCommentInput) (coordinator.ReviewThread, error)

	// Deadlines.
	// LastAgentActivity returns the active author session's last agent-activity
	// timestamp for the issue; ok is false when no active author session exists.
	LastAgentActivity(ctx context.Context, issueID string) (*time.Time, bool, error)
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
	issues       *coordinator.IssueService
	checks       *coordinator.CheckService
	checkConfigs *coordinator.CheckConfigService
	sessions     *coordinator.SessionService
	merges       *coordinator.MergeService
	threads      *coordinator.ThreadService
	status       *coordinator.StatusService
}

// NewEffects builds the production Effects from the existing coordinator services.
func NewEffects(
	issues *coordinator.IssueService,
	checks *coordinator.CheckService,
	checkConfigs *coordinator.CheckConfigService,
	sessions *coordinator.SessionService,
	merges *coordinator.MergeService,
	threads *coordinator.ThreadService,
	status *coordinator.StatusService,
) Effects {
	return &liveEffects{
		issues:       issues,
		checks:       checks,
		checkConfigs: checkConfigs,
		sessions:     sessions,
		merges:       merges,
		threads:      threads,
		status:       status,
	}
}

func (e *liveEffects) GetIssue(ctx context.Context, id string) (coordinator.Issue, error) {
	return e.issues.GetIssue(ctx, id)
}

func (e *liveEffects) HasMergedChange(ctx context.Context, issueID string) (bool, error) {
	return e.issues.HasMergedChange(ctx, issueID)
}

func (e *liveEffects) ScheduleIssue(ctx context.Context, id string, state coordinator.ScheduleState) (coordinator.Issue, error) {
	return e.issues.ScheduleIssue(ctx, id, state)
}

func (e *liveEffects) SetIssueState(ctx context.Context, id string, state coordinator.IssueState) (coordinator.Issue, error) {
	return e.issues.SetIssueState(ctx, id, state)
}

func (e *liveEffects) AcceptTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	return e.issues.AcceptTriage(ctx, id)
}

func (e *liveEffects) RejectTriage(ctx context.Context, id string) (coordinator.Issue, error) {
	return e.issues.RejectTriage(ctx, id)
}

func (e *liveEffects) CloseIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	return e.issues.CloseIssue(ctx, issueID)
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

func (e *liveEffects) ReadyPlanningSession(ctx context.Context, sessionID string) (coordinator.Session, error) {
	return e.sessions.ReadyPlanningSession(ctx, sessionID)
}

func (e *liveEffects) MarkPlanApproved(ctx context.Context, issueID string) (coordinator.Issue, error) {
	return e.issues.MarkPlanApproved(ctx, issueID)
}

func (e *liveEffects) UpdateSessionState(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	return e.sessions.UpdateSessionState(ctx, sessionID, state)
}

func (e *liveEffects) UpdateChangeHead(ctx context.Context, changeID, headSHA string) (coordinator.Change, error) {
	return e.sessions.UpdateChangeHead(ctx, changeID, headSHA)
}

func (e *liveEffects) ResetAutomatedChecksForNewRevision(ctx context.Context, issueID string) (int, error) {
	return e.checks.ResetAutomatedChecksForNewRevision(ctx, issueID)
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

func (e *liveEffects) GetCheck(ctx context.Context, issueID, name string) (coordinator.Check, error) {
	return e.checks.GetCheck(ctx, issueID, name)
}

func (e *liveEffects) ReviewState(ctx context.Context, issueID string) (coordinator.ReviewState, error) {
	return e.checks.ReviewState(ctx, issueID)
}

func (e *liveEffects) HasReadyUnmergedChange(ctx context.Context, issueID string) (bool, error) {
	return e.sessions.HasReadyUnmergedChange(ctx, issueID)
}

func (e *liveEffects) ReadyUnmergedChangeForIssue(ctx context.Context, issueID string) (coordinator.Change, bool, error) {
	return e.sessions.ReadyUnmergedChangeForIssue(ctx, issueID)
}

func (e *liveEffects) ActiveAuthorSessionState(ctx context.Context, issueID string) (coordinator.SessionRuntimeState, bool, error) {
	return e.sessions.ActiveAuthorSessionState(ctx, issueID)
}

func (e *liveEffects) EnqueueAcceptanceIfReady(ctx context.Context, issueID string, change coordinator.Change) ([]string, error) {
	if e.checkConfigs == nil {
		return nil, nil
	}
	return e.checkConfigs.EnqueueAcceptanceIfReady(ctx, issueID, change)
}

func (e *liveEffects) AcceptancePending(ctx context.Context, issueID string) (bool, error) {
	if e.checkConfigs == nil {
		return false, nil
	}
	return e.checkConfigs.AcceptancePending(ctx, issueID)
}

func (e *liveEffects) EnsureAuthorJob(ctx context.Context, input coordinator.EnsureAuthorJobInput) (coordinator.EnsureAuthorJobResult, error) {
	return e.sessions.EnsureAuthorJob(ctx, input)
}

func (e *liveEffects) ResetIssue(ctx context.Context, issueID string) (coordinator.Issue, error) {
	return e.sessions.ResetIssue(ctx, issueID)
}

func (e *liveEffects) RetryCrashedAuthorJob(ctx context.Context, issueID string, actor string) (coordinator.RetryCrashedAuthorJobResult, error) {
	return e.sessions.RetryCrashedAuthorJob(ctx, issueID, actor)
}

func (e *liveEffects) MergeIssue(ctx context.Context, issueID string) (coordinator.MergeResult, error) {
	if e.merges == nil {
		return coordinator.MergeResult{}, ErrMergeUnavailable
	}
	return e.merges.MergeIssue(ctx, issueID)
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

func (e *liveEffects) LastAgentActivity(ctx context.Context, issueID string) (*time.Time, bool, error) {
	session, ok, err := e.sessions.ActiveAuthorSessionForIssue(ctx, issueID)
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
