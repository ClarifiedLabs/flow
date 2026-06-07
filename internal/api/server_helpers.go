package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func createIssueInputForPrincipal(request createIssueRequest, principal coordinator.Principal) (coordinator.CreateIssueWithDetailsInput, error) {
	actor := coordinator.ActorHuman
	createdBySessionID := request.CreatedBySessionID
	sourceIssueID := request.SourceIssueID
	scheduleState := coordinator.ScheduleState(request.ScheduleState)
	triageState := coordinator.TriageState(request.TriageState)

	if principal.Scope == coordinator.TokenScopeSession {
		if request.ScheduleState != "" && scheduleState != coordinator.ScheduleBacklog {
			return coordinator.CreateIssueWithDetailsInput{}, errors.New("session tokens can only create backlog issues")
		}
		if request.TriageState != "" && triageState != coordinator.TriagePending {
			return coordinator.CreateIssueWithDetailsInput{}, errors.New("session tokens can only create triage issues")
		}
		if principal.SourceIssueID == nil && sourceIssueID != nil {
			return coordinator.CreateIssueWithDetailsInput{}, errors.New("session token is not bound to a source issue")
		}
		if principal.SourceIssueID != nil && sourceIssueID != nil && strings.TrimSpace(*sourceIssueID) != *principal.SourceIssueID {
			return coordinator.CreateIssueWithDetailsInput{}, errors.New("session token cannot create issues for a different source issue")
		}

		actor = coordinator.ActorAgent
		createdBySessionID = &principal.Subject
		sourceIssueID = principal.SourceIssueID
		scheduleState = coordinator.ScheduleBacklog
		triageState = coordinator.TriagePending
	} else if principal.Scope == coordinator.TokenScopeConsole {
		actor = coordinator.ActorAgent
		createdBySessionID = &principal.Subject
	}

	input := coordinator.CreateIssueWithDetailsInput{
		Issue: coordinator.CreateIssueInput{
			Title:               request.Title,
			Body:                request.Body,
			AcceptanceCriteria:  request.AcceptanceCriteria,
			Priority:            request.Priority,
			ScheduleState:       scheduleState,
			TriageState:         triageState,
			RequiresHumanReview: request.RequiresHumanReview,
			AutoMerge:           request.AutoMerge,
			PlanMode:            request.PlanMode,
			AgentHarness:        request.AgentHarness,
			HarnessArgs:         request.HarnessArgs,
			CreatedBy:           actor,
			CreatedBySessionID:  createdBySessionID,
			SourceIssueID:       sourceIssueID,
			SourceChangeID:      request.SourceChangeID,
		},
		Tags:      tagInputs(request.Tags, actor),
		Relations: relationInputs(request.Relations, actor),
	}
	if principal.Scope == coordinator.TokenScopeSession {
		if err := constrainSessionRelations(input.Relations, principal.SourceIssueID); err != nil {
			return coordinator.CreateIssueWithDetailsInput{}, err
		}
	}

	return input, nil
}

func constrainSessionRelations(relations []coordinator.CreateIssueRelationInput, sourceIssueID *string) error {
	if sourceIssueID == nil && len(relations) > 0 {
		return errors.New("session token is not bound to a source issue")
	}
	for _, relation := range relations {
		source := strings.TrimSpace(relation.SourceIssueID)
		target := strings.TrimSpace(relation.TargetIssueID)
		if source != "" && target != "" {
			return errors.New("session-created issue relations must involve the newly created issue")
		}
		if sourceIssueID == nil {
			continue
		}
		ownedIssueID := *sourceIssueID
		switch {
		case source == "" && target == ownedIssueID:
		case target == "" && source == ownedIssueID:
		default:
			return errors.New("session-created issue relations must relate to the session source issue")
		}
	}

	return nil
}

func tagInputs(tags []tagRequest, actor coordinator.Actor) []coordinator.CreateTagInput {
	inputs := make([]coordinator.CreateTagInput, 0, len(tags))
	for _, tag := range tags {
		inputs = append(inputs, coordinator.CreateTagInput{
			Slug:        tag.Slug,
			Name:        tag.Name,
			Color:       tag.Color,
			Description: tag.Description,
			CreatedBy:   actor,
		})
	}

	return inputs
}

func relationInputs(relations []relationRequest, actor coordinator.Actor) []coordinator.CreateIssueRelationInput {
	inputs := make([]coordinator.CreateIssueRelationInput, 0, len(relations))
	for _, relation := range relations {
		inputs = append(inputs, coordinator.CreateIssueRelationInput{
			SourceIssueID: relation.SourceIssueID,
			TargetIssueID: relation.TargetIssueID,
			Kind:          coordinator.RelationKind(relation.Kind),
			CreatedBy:     actor,
		})
	}

	return inputs
}

func scopeAllowed(principal coordinator.Principal, allowed ...coordinator.TokenScope) bool {
	for _, scope := range allowed {
		if principal.Scope == scope {
			return true
		}
	}

	return false
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}

	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
	return false
}

func requireScope(w http.ResponseWriter, principal coordinator.Principal, message string, allowed ...coordinator.TokenScope) bool {
	if scopeAllowed(principal, allowed...) {
		return true
	}

	writeError(w, http.StatusForbidden, "forbidden", message)
	return false
}

func checkSessionScope(principal coordinator.Principal, sessionID string) error {
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession, coordinator.TokenScopeConsole:
		if strings.TrimSpace(principal.Subject) != strings.TrimSpace(sessionID) {
			return errors.New("session credential cannot operate on a different session")
		}
		return nil
	default:
		return errors.New("session operation requires owner, session, or console token")
	}
}

func checkSessionTokenScope(principal coordinator.Principal, sessionID string) error {
	if principal.Scope != coordinator.TokenScopeSession && principal.Scope != coordinator.TokenScopeConsole {
		return errors.New("terminal registration requires a session or console token")
	}
	if strings.TrimSpace(principal.Subject) != strings.TrimSpace(sessionID) {
		return errors.New("session credential cannot operate on a different session")
	}

	return nil
}

func checkConsoleIssueScope(principal coordinator.Principal, issueID string) error {
	if principal.Scope != coordinator.TokenScopeConsole || principal.SourceIssueID == nil {
		return nil
	}
	if strings.TrimSpace(*principal.SourceIssueID) != strings.TrimSpace(issueID) {
		return errors.New("issue console token cannot operate on a different issue")
	}
	return nil
}

var errWorkerLeaseForbidden = errors.New("lease belongs to a different worker")

func workerIDForPrincipal(requested string, principal coordinator.Principal) (string, error) {
	subject := strings.TrimSpace(principal.Subject)
	if subject == "" {
		return "", errors.New("worker token subject is required")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return subject, nil
	}
	if requested != subject {
		return "", errors.New("worker token subject does not match worker id")
	}

	return requested, nil
}

func nonNegativeSeconds(seconds int, field string) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("%s cannot be negative", field)
	}

	return time.Duration(seconds) * time.Second, nil
}

func positiveSecondsOrDefault(seconds int, defaultSeconds int, field string) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("%s cannot be negative", field)
	}
	if seconds == 0 {
		seconds = defaultSeconds
	}

	return time.Duration(seconds) * time.Second, nil
}

func claimWaitDuration(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, errors.New("wait_seconds cannot be negative")
	}
	if seconds > maxClaimWaitSeconds {
		return 0, fmt.Errorf("wait_seconds cannot exceed %d", maxClaimWaitSeconds)
	}

	return time.Duration(seconds) * time.Second, nil
}

func writeLeaseAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "lease_not_found", "lease not found")
	case errors.Is(err, errWorkerLeaseForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "lease belongs to a different worker")
	default:
		writeError(w, http.StatusInternalServerError, "get_lease_failed", err.Error())
	}
}

func trimmedStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}

	return &trimmed
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}

	return *value
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

func sessionHarnessForJob(job worker.Job) string {
	if job.Role == worker.RoleConsole {
		consoleHarness := flowharness.NormalizeName(payloadString(job.Payload, "console_harness"))
		if err := flowharness.ValidateConsoleName(consoleHarness); err == nil && consoleHarness != "" {
			return consoleHarness
		}
		return flowharness.DefaultConsoleName()
	}

	agentHarness := flowharness.NormalizeName(payloadString(job.Payload, "agent_harness"))
	if _, ok := flowharness.Lookup(agentHarness); ok {
		return agentHarness
	}
	return flowharness.DefaultAgentName()
}

type createIssueRequest struct {
	Title               string            `json:"title"`
	Body                string            `json:"body"`
	AcceptanceCriteria  string            `json:"acceptance_criteria"`
	Priority            int               `json:"priority"`
	ScheduleState       string            `json:"schedule_state"`
	TriageState         string            `json:"triage_state"`
	RequiresHumanReview *bool             `json:"requires_human_review"`
	AutoMerge           *bool             `json:"auto_merge"`
	PlanMode            bool              `json:"plan_mode"`
	AgentHarness        string            `json:"agent_harness"`
	HarnessArgs         flowharness.Args  `json:"harness_args"`
	CreatedBySessionID  *string           `json:"created_by_session_id"`
	SourceIssueID       *string           `json:"source_issue_id"`
	SourceChangeID      *string           `json:"source_change_id"`
	Tags                []tagRequest      `json:"tags"`
	Relations           []relationRequest `json:"relations"`
}

type tagRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type relationRequest = contract.IssueRelationRequest

type editIssueRequest struct {
	Title               *string                `json:"title"`
	Body                *string                `json:"body"`
	AcceptanceCriteria  *string                `json:"acceptance_criteria"`
	Priority            *int                   `json:"priority"`
	RequiresHumanReview *bool                  `json:"requires_human_review"`
	AutoMerge           *bool                  `json:"auto_merge"`
	PlanMode            *bool                  `json:"plan_mode"`
	AgentHarness        *string                `json:"agent_harness"`
	HarnessArgs         *flowharness.ArgsPatch `json:"harness_args"`
}

type scheduleIssueRequest = contract.ScheduleIssueRequest
type issueStateRequest = contract.IssueStateRequest
type triageIssueRequest = contract.TriageIssueRequest
type registerWorkerRequest = contract.RegisterWorkerRequest
type joinWorkerRequest = contract.JoinWorkerRequest
type joinWorkerResponse = contract.JoinWorkerResponse
type heartbeatWorkerRequest = contract.HeartbeatWorkerRequest
type claimJobRequest = contract.ClaimJobRequest
type renewLeaseRequest = contract.RenewLeaseRequest
type workerJobStatusRequest = contract.WorkerJobStatusRequest
type markJobRunningRequest = contract.MarkJobRunningRequest
type releaseLeaseRequest = contract.ReleaseLeaseRequest
type enqueueJobRequest = contract.EnqueueJobRequest
type consoleRequest = contract.ConsoleRequest
type approveReviewCyclesRequest = contract.ApproveReviewCyclesRequest
type reportCheckRequest = contract.ReportCheckRequest
type sessionEventRequest = contract.SessionEventRequest
type sessionSignalRequest = contract.SessionSignalRequest
type readySessionRequest = contract.ReadySessionRequest
type sessionStatusRequest = contract.SessionStatusRequest
type sessionProcessExitRequest = contract.SessionProcessExitRequest
type sessionMessageDeliveredRequest = contract.SessionMessageDeliveredRequest
type planRejectRequest = contract.PlanRejectRequest
type attentionReplyRequest = contract.AttentionReplyRequest
type sessionTerminalRequest = contract.SessionTerminalRequest
type jobTerminalRequest = contract.JobTerminalRequest
type createThreadRequest = contract.CreateThreadRequest
type putHandoffRequest = contract.PutHandoffRequest
type threadCommentRequest = contract.ThreadCommentRequest
type threadClaimRequest = contract.ThreadClaimRequest

type gitEventsRequest struct {
	OldSHA     string          `json:"old_sha"`
	NewSHA     string          `json:"new_sha"`
	Ref        string          `json:"ref"`
	Actor      string          `json:"actor"`
	ObservedAt time.Time       `json:"observed_at"`
	Events     []gitEventInput `json:"events"`
}

func (r gitEventsRequest) eventItems() []gitEventInput {
	if len(r.Events) > 0 {
		return r.Events
	}
	if strings.TrimSpace(r.OldSHA) == "" && strings.TrimSpace(r.NewSHA) == "" && strings.TrimSpace(r.Ref) == "" {
		return nil
	}

	return []gitEventInput{{
		OldSHA:     r.OldSHA,
		NewSHA:     r.NewSHA,
		Ref:        r.Ref,
		Actor:      r.Actor,
		ObservedAt: r.ObservedAt,
	}}
}

type gitEventInput struct {
	OldSHA     string    `json:"old_sha"`
	NewSHA     string    `json:"new_sha"`
	Ref        string    `json:"ref"`
	Actor      string    `json:"actor"`
	ObservedAt time.Time `json:"observed_at"`
}

type drainGitEventsRequest struct {
	ExchangeRepoPath string `json:"exchange_repo_path"`
}

type issueResponse struct {
	Issue       coordinator.Issue            `json:"issue"`
	ProjectID   string                       `json:"project_id,omitempty"`
	ProjectName string                       `json:"project_name,omitempty"`
	StatusLog   []coordinator.StatusLogEntry `json:"status_log,omitempty"`
	Detail      *uiIssueDetail               `json:"issue_detail,omitempty"`
}

type issuesResponse = contract.IssuesResponse
type issueAttachmentResponse = contract.IssueAttachmentResponse
type issueAttachmentsResponse = contract.IssueAttachmentsResponse

type sessionMessagesResponse = contract.SessionMessagesResponse
type sessionMessageResponse = contract.SessionMessageResponse

type boardResponse struct {
	contract.BoardResponse
	IssueCards map[string]uiIssueCard `json:"issue_cards,omitempty"`
}

// doneResponse is the per-project read model for terminal (closed) issues. It
// mirrors the board's owner-scoped card gating but carries the derived outcome
// phase per issue plus a keyset cursor for paging older history.
type doneResponse struct {
	Issues       []coordinator.Issue          `json:"issues"`
	Outcomes     map[string]coordinator.Phase `json:"outcomes"`
	IssueCards   map[string]uiDoneCard        `json:"issue_cards,omitempty"`
	NextBefore   string                       `json:"next_before,omitempty"`
	NextBeforeID string                       `json:"next_before_id,omitempty"`
}

// uiDoneCard is a lean card for a closed issue: just the merged change (if any)
// and tags. It deliberately omits the session/job/diff/check fan-out that
// buildUIIssueCards performs for active issues.
type uiDoneCard struct {
	IssueID string            `json:"issue_id"`
	Change  *uiChangeSummary  `json:"change,omitempty"`
	Tags    []coordinator.Tag `json:"tags,omitempty"`
}

type changeResponse struct {
	Change             coordinator.Change         `json:"change"`
	ProjectID          string                     `json:"project_id,omitempty"`
	ProjectName        string                     `json:"project_name,omitempty"`
	Issue              coordinator.Issue          `json:"issue"`
	Checks             []coordinator.Check        `json:"checks,omitempty"`
	ReviewState        coordinator.ReviewState    `json:"review_state,omitempty"`
	RequiredChecks     uiRequiredCheckSummary     `json:"required_checks"`
	Threads            []coordinator.ReviewThread `json:"threads,omitempty"`
	CanMerge           bool                       `json:"can_merge"`
	MergeBlockedReason string                     `json:"merge_blocked_reason,omitempty"`
}

type changeDiffResponse struct {
	ChangeID          string                 `json:"change_id"`
	Base              string                 `json:"base"`
	HeadSHA           string                 `json:"head_sha,omitempty"`
	Available         bool                   `json:"available"`
	UnavailableReason string                 `json:"unavailable_reason,omitempty"`
	TotalFiles        int                    `json:"total_files"`
	Additions         int                    `json:"additions"`
	Deletions         int                    `json:"deletions"`
	Files             []flowgit.DiffFileStat `json:"files,omitempty"`
}

type mergeResponse = contract.MergeResponse

type uiIssueCard struct {
	IssueID               string                         `json:"issue_id"`
	Tags                  []coordinator.Tag              `json:"tags,omitempty"`
	Relations             uiRelationSummary              `json:"relations"`
	ActiveSession         *uiSessionSummary              `json:"active_session,omitempty"`
	TerminalAvailable     bool                           `json:"terminal_available,omitempty"`
	TerminalJobID         string                         `json:"terminal_job_id,omitempty"`
	Change                *uiChangeSummary               `json:"change,omitempty"`
	DiffStats             *uiDiffStatSummary             `json:"diff_stats,omitempty"`
	DiffUnavailableReason string                         `json:"diff_unavailable_reason,omitempty"`
	Handoff               *uiHandoffSummary              `json:"handoff,omitempty"`
	ReviewState           coordinator.ReviewState        `json:"review_state,omitempty"`
	RequiredChecks        uiRequiredCheckSummary         `json:"required_checks"`
	ReviewCycleBudget     *coordinator.ReviewCycleBudget `json:"review_cycle_budget,omitempty"`
	LatestStatus          *coordinator.StatusLogEntry    `json:"latest_status,omitempty"`
	Blockers              uiBlockerSummary               `json:"blockers"`
	BlockingReason        string                         `json:"blocking_reason,omitempty"`
	PrimaryAction         string                         `json:"primary_action,omitempty"`
	CrashRetryAvailable   bool                           `json:"crash_retry_available,omitempty"`
}

type uiIssueDetail struct {
	Tags                []coordinator.Tag                `json:"tags,omitempty"`
	Relations           []coordinator.IssueRelation      `json:"relations,omitempty"`
	ActiveSession       *uiSessionSummary                `json:"active_session,omitempty"`
	Paused              bool                             `json:"paused,omitempty"`
	TerminalAvailable   bool                             `json:"terminal_available,omitempty"`
	TerminalJobID       string                           `json:"terminal_job_id,omitempty"`
	Sessions            []uiSessionSummary               `json:"sessions,omitempty"`
	Changes             []uiChangeSummary                `json:"changes,omitempty"`
	ReadyChange         *uiChangeSummary                 `json:"ready_change,omitempty"`
	ReviewState         coordinator.ReviewState          `json:"review_state,omitempty"`
	RequiredChecks      uiRequiredCheckSummary           `json:"required_checks"`
	ReviewCycleBudget   *coordinator.ReviewCycleBudget   `json:"review_cycle_budget,omitempty"`
	WaitReason          coordinator.WaitReason           `json:"wait_reason,omitempty"`
	CrashRetryAvailable bool                             `json:"crash_retry_available,omitempty"`
	IssueConsole        *consoleResponse                 `json:"issue_console,omitempty"`
	Checks              []coordinator.Check              `json:"checks,omitempty"`
	Transitions         []coordinator.TransitionLogEntry `json:"transitions,omitempty"`
	// TimelineTransitions is the enriched view of Transitions used by the web
	// UI's unified timeline: session_ready / session_state_changed rows carry
	// the decoded session_id, session_state, head_sha, and change_id so a
	// timeline entry can render the exact session's terminal/transcript
	// controls. It is populated in parallel with Transitions (which stays the
	// raw log for the timeline endpoint and CLI) and is empty when no
	// transitions exist.
	TimelineTransitions []coordinator.SessionTimelineEntry `json:"timeline_transitions,omitempty"`
	LifecycleGraph      *coordinator.LifecycleGraphSummary `json:"lifecycle_graph,omitempty"`
	Attachments         []coordinator.IssueAttachment      `json:"attachments,omitempty"`
}

type uiSessionSummary struct {
	ID                  string                          `json:"id"`
	ChangeID            string                          `json:"change_id"`
	WorkerID            string                          `json:"worker_id"`
	State               coordinator.SessionRuntimeState `json:"state"`
	Branch              string                          `json:"branch"`
	Base                string                          `json:"base"`
	Harness             string                          `json:"harness,omitempty"`
	TerminalAvailable   bool                            `json:"terminal_available,omitempty"`
	TranscriptAvailable bool                            `json:"transcript_available,omitempty"`
	UpdatedAt           time.Time                       `json:"updated_at"`
	LastAgentActivityAt *time.Time                      `json:"last_agent_activity_at,omitempty"`
}

type uiChangeSummary struct {
	ID        string     `json:"id"`
	Branch    string     `json:"branch"`
	Base      string     `json:"base"`
	HeadSHA   string     `json:"head_sha,omitempty"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type uiDiffStatSummary struct {
	HeadSHA    string `json:"head_sha,omitempty"`
	TotalFiles int    `json:"total_files"`
	Additions  int    `json:"additions"`
	Deletions  int    `json:"deletions"`
}

type uiHandoffSummary struct {
	HeadSHA   string    `json:"head_sha,omitempty"`
	Present   bool      `json:"present"`
	Valid     bool      `json:"valid"`
	Summary   string    `json:"summary,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type uiRequiredCheckSummary struct {
	Total              int  `json:"total"`
	Pending            int  `json:"pending"`
	PendingHumanReview bool `json:"pending_human_review,omitempty"`
	Satisfied          int  `json:"satisfied"`
	Blocked            int  `json:"blocked"`
	Skipped            int  `json:"skipped"`
}

type uiBlockerSummary struct {
	Count  int                     `json:"count"`
	Issues []uiBlockerIssueSummary `json:"issues,omitempty"`
}

type uiBlockerIssueSummary struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type uiRelationSummary struct {
	Total     int `json:"total"`
	Parents   int `json:"parents"`
	Children  int `json:"children"`
	Blocks    int `json:"blocks"`
	BlockedBy int `json:"blocked_by"`
	Related   int `json:"related"`
}

type webBootstrapResponse = contract.WebBootstrapResponse
type checkResponse = contract.CheckResponse
type checksResponse = contract.ChecksResponse
type transitionsResponse = contract.TransitionsResponse
type reviewRunResponse = contract.ReviewRunResponse
type reviewCycleBudgetResponse = contract.ReviewCycleBudgetResponse
type workerResponse = contract.WorkerResponse

type workersResponse struct {
	Workers     []worker.Worker                `json:"workers"`
	Diagnostics map[string]uiWorkerDiagnostics `json:"diagnostics,omitempty"`
	Queue       uiQueueSummary                 `json:"queue"`
}

type jobResponse struct {
	Job          worker.Job           `json:"job"`
	ProjectID    string               `json:"project_id,omitempty"`
	Change       *coordinator.Change  `json:"change,omitempty"`
	Session      *coordinator.Session `json:"session,omitempty"`
	Diagnostics  *uiJobDiagnostics    `json:"diagnostics,omitempty"`
	SessionToken string               `json:"session_token,omitempty"`
}

type jobsResponse struct {
	Jobs        []worker.Job                `json:"jobs"`
	Diagnostics map[string]uiJobDiagnostics `json:"diagnostics,omitempty"`
}

type consoleResponse = contract.ConsoleResponse

type reapJob struct {
	ID    string          `json:"id"`
	State worker.JobState `json:"state"`
}

type reapJobsResponse struct {
	Jobs []reapJob `json:"jobs"`
}

type uiWorkerDiagnostics struct {
	LiveJobs                         int `json:"live_jobs"`
	LivePersistentAgent              int `json:"live_persistent_agent"`
	LiveEphemeral                    int `json:"live_ephemeral"`
	ExpiredUnreleasedJobs            int `json:"expired_unreleased_jobs"`
	ExpiredUnreleasedPersistentAgent int `json:"expired_unreleased_persistent_agent"`
	ExpiredUnreleasedEphemeral       int `json:"expired_unreleased_ephemeral"`
}

type uiQueueSummary struct {
	Queued          int `json:"queued"`
	PersistentAgent int `json:"persistent_agent"`
	Ephemeral       int `json:"ephemeral"`
	Author          int `json:"author"`
	Reviewer        int `json:"reviewer"`
	Verifier        int `json:"verifier"`
	CI              int `json:"ci"`
	Console         int `json:"console"`
}

type uiJobDiagnostics struct {
	ProjectID           string            `json:"project_id,omitempty"`
	ProjectName         string            `json:"project_name,omitempty"`
	Lease               *worker.Lease     `json:"lease,omitempty"`
	LiveLease           bool              `json:"live_lease"`
	LeaseStatus         string            `json:"lease_status,omitempty"`
	TmuxSession         string            `json:"tmux_session,omitempty"`
	TerminalAvailable   bool              `json:"terminal_available,omitempty"`
	TranscriptAvailable bool              `json:"transcript_available,omitempty"`
	Session             *uiSessionSummary `json:"session,omitempty"`
	Change              *uiChangeSummary  `json:"change,omitempty"`
}

type sessionResponse = contract.SessionResponse
type attachResponse = contract.AttachResponse
type sessionTerminalResponse = contract.SessionTerminalResponse
type sessionTerminalAccessResponse = contract.SessionTerminalAccessResponse
type jobTerminalResponse = contract.JobTerminalResponse

type jobTerminalAccessResponse struct {
	Access coordinator.JobTerminalAccess `json:"access"`
}

type threadResponse = contract.ThreadResponse
type handoffResponse = contract.HandoffResponse
type threadsResponse = contract.ThreadsResponse
type statusResponse = contract.StatusResponse
type reconcileResponse = contract.ReconcileResponse
type leaseResponse = contract.LeaseResponse

type workerJobStatusResponse struct {
	ProjectID string               `json:"project_id,omitempty"`
	Job       worker.Job           `json:"job"`
	Lease     worker.Lease         `json:"lease"`
	Session   *coordinator.Session `json:"session,omitempty"`
}

type claimJobResponse = contract.ClaimJobResponse

type gitEventsResponse struct {
	Events   []coordinator.GitEvent `json:"events"`
	Recorded int                    `json:"recorded"`
	Inserted int                    `json:"inserted"`
}

type drainGitEventsResponse struct {
	Drained int `json:"drained"`
}

type errorResponse = contract.ErrorResponse

func issueFilterFromQuery(r *http.Request) (coordinator.IssueFilter, error) {
	var filter coordinator.IssueFilter
	for _, state := range r.URL.Query()["schedule_state"] {
		if state == "" {
			continue
		}
		scheduleState := coordinator.ScheduleState(state)
		switch scheduleState {
		case coordinator.ScheduleBacklog, coordinator.ScheduleUpNext, coordinator.ScheduleClosed:
			filter.ScheduleStates = append(filter.ScheduleStates, scheduleState)
		default:
			return coordinator.IssueFilter{}, fmt.Errorf("invalid schedule_state %q", state)
		}
	}
	for _, state := range r.URL.Query()["triage_state"] {
		if state == "" {
			continue
		}
		triageState := coordinator.TriageState(state)
		switch triageState {
		case coordinator.TriagePending, coordinator.TriageAccepted, coordinator.TriageRejected:
			filter.TriageStates = append(filter.TriageStates, triageState)
		default:
			return coordinator.IssueFilter{}, fmt.Errorf("invalid triage_state %q", state)
		}
	}
	filter.TagSlugs = r.URL.Query()["tag"]

	return filter, nil
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}

	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, errorResponse{
		Error: contract.ErrorBody{
			Code:    code,
			Message: message,
		},
	})
}

type responseCapture struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
}

func newResponseCapture() *responseCapture {
	return &responseCapture{
		header:     http.Header{},
		statusCode: http.StatusOK,
	}
}

func (c *responseCapture) Header() http.Header {
	return c.header
}

func (c *responseCapture) WriteHeader(statusCode int) {
	c.statusCode = statusCode
}

func (c *responseCapture) Write(data []byte) (int, error) {
	return c.body.Write(data)
}

func (c *responseCapture) flush(w http.ResponseWriter) {
	for key, values := range c.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(c.statusCode)
	_, _ = w.Write(c.body.Bytes())
}
