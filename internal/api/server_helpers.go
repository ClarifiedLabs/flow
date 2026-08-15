package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func createTaskInputForPrincipal(request createTaskRequest, principal coordinator.Principal) (coordinator.CreateTaskWithDetailsInput, error) {
	actor := coordinator.ActorHuman
	createdBySessionID := request.CreatedBySessionID
	sourceTaskID := request.SourceTaskID
	scheduleState := coordinator.ScheduleBacklog
	triageState := coordinator.TriageAccepted

	if principal.Scope == coordinator.TokenScopeSession {
		if principal.SourceTaskID == nil && sourceTaskID != nil {
			return coordinator.CreateTaskWithDetailsInput{}, errors.New("session token is not bound to a source task")
		}
		if principal.SourceTaskID != nil && sourceTaskID != nil && strings.TrimSpace(*sourceTaskID) != *principal.SourceTaskID {
			return coordinator.CreateTaskWithDetailsInput{}, errors.New("session token cannot create tasks for a different source task")
		}

		actor = coordinator.ActorAgent
		createdBySessionID = &principal.Subject
		sourceTaskID = principal.SourceTaskID
		scheduleState = coordinator.ScheduleBacklog
		triageState = coordinator.TriageAccepted
	} else if principal.Scope == coordinator.TokenScopeConsole {
		// The console is human-operated: attribute to the human while keeping
		// the console session id for traceability.
		actor = coordinator.ActorHuman
		createdBySessionID = &principal.Subject
	}

	input := coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{
			Title:               request.Title,
			Body:                request.Body,
			Priority:            request.Priority,
			ScheduleState:       scheduleState,
			RequiresHumanReview: request.RequiresHumanReview,
			TriageState:         triageState,
			FlowID:              request.FlowID,
			ParentItemID:        request.ParentItemID,
			FeatureID:           request.FeatureID,
			CreatedBy:           actor,
			CreatedBySessionID:  createdBySessionID,
			SourceTaskID:        sourceTaskID,
			SourceChangeID:      request.SourceChangeID,
		},
		Tags:              tagInputs(request.Tags, actor),
		Relations:         relationInputs(request.Relations, actor),
		WorkItemRelations: createWorkItemRelationInputs(request.WorkItemRelations, actor),
	}
	if principal.Scope == coordinator.TokenScopeSession {
		if err := constrainSessionRelations(input.Relations, principal.SourceTaskID); err != nil {
			return coordinator.CreateTaskWithDetailsInput{}, err
		}
		if err := constrainCreateWorkItemRelations(input.WorkItemRelations, principal.SourceTaskID, true); err != nil {
			return coordinator.CreateTaskWithDetailsInput{}, err
		}
	} else if principal.Scope == coordinator.TokenScopeConsole {
		if err := constrainConsoleRelations(input.Relations, principal.SourceTaskID); err != nil {
			return coordinator.CreateTaskWithDetailsInput{}, err
		}
		if err := constrainCreateWorkItemRelations(input.WorkItemRelations, principal.SourceTaskID, false); err != nil {
			return coordinator.CreateTaskWithDetailsInput{}, err
		}
	}

	return input, nil
}

// constrainConsoleRelations confines create-time relations for task-bound
// console credentials (task-recovery consoles started from a task's console
// endpoint). Every named relation endpoint must be the console's bound task;
// a blank endpoint resolves to the newly created task. Unbound project
// consoles have no bound task and keep their project-wide relation behavior.
func constrainConsoleRelations(relations []coordinator.CreateTaskRelationInput, boundTaskID *string) error {
	if boundTaskID == nil {
		return nil
	}
	bound := strings.TrimSpace(*boundTaskID)
	for i := range relations {
		source := strings.TrimSpace(relations[i].SourceTaskID)
		target := strings.TrimSpace(relations[i].TargetTaskID)
		if source != "" && source != bound {
			return errors.New("console-created task relations must relate to the console's bound task")
		}
		if target != "" && target != bound {
			return errors.New("console-created task relations must relate to the console's bound task")
		}
	}

	return nil
}

func constrainSessionRelations(relations []coordinator.CreateTaskRelationInput, sourceTaskID *string) error {
	if sourceTaskID == nil && len(relations) > 0 {
		return errors.New("session token is not bound to a source task")
	}
	for i := range relations {
		source := strings.TrimSpace(relations[i].SourceTaskID)
		target := strings.TrimSpace(relations[i].TargetTaskID)
		if source != "" && target != "" {
			return errors.New("session-created task relations must involve the newly created task")
		}
		if sourceTaskID == nil {
			continue
		}
		ownedTaskID := *sourceTaskID
		switch {
		case source == "" && target == ownedTaskID:
		case target == "" && source == ownedTaskID:
			// Authorized shorthand: the new task is the relation target, so let
			// the coordinator resolve the blank target to the allocated task ID.
			relations[i].BlankTargetIsNewTask = true
		default:
			return errors.New("session-created task relations must relate to the session source task")
		}
	}

	return nil
}

func constrainCreateWorkItemRelations(relations []coordinator.CreateWorkItemRelationInput, boundTaskID *string, requireBound bool) error {
	if boundTaskID == nil {
		if requireBound && len(relations) != 0 {
			return errors.New("session token is not bound to a source task")
		}
		return nil
	}
	bound := strings.TrimSpace(*boundTaskID)
	for _, relation := range relations {
		if !relation.SourceIsNewItem && strings.TrimSpace(relation.SourceItemID) != bound {
			return errors.New("create-time work item relations must relate to the credential's bound task")
		}
		if !relation.TargetIsNewItem && strings.TrimSpace(relation.TargetItemID) != bound {
			return errors.New("create-time work item relations must relate to the credential's bound task")
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

func relationInputs(relations []relationRequest, actor coordinator.Actor) []coordinator.CreateTaskRelationInput {
	inputs := make([]coordinator.CreateTaskRelationInput, 0, len(relations))
	for _, relation := range relations {
		inputs = append(inputs, coordinator.CreateTaskRelationInput{
			SourceTaskID: relation.SourceTaskID,
			TargetTaskID: relation.TargetTaskID,
			Kind:         coordinator.RelationKind(relation.Kind),
			CreatedBy:    actor,
			// An explicit target_is_new_task opt-in resolves the blank target to
			// the task being created, letting the new task be the relation target
			// (e.g. an existing parent linked parent_of the new task) inside the
			// create transaction.
			BlankTargetIsNewTask: relation.TargetIsNewTask,
		})
	}

	return inputs
}

func createWorkItemRelationInputs(relations []contract.CreateWorkItemRelationRequest, actor coordinator.Actor) []coordinator.CreateWorkItemRelationInput {
	inputs := make([]coordinator.CreateWorkItemRelationInput, 0, len(relations))
	for _, relation := range relations {
		inputs = append(inputs, coordinator.CreateWorkItemRelationInput{
			SourceItemID: relation.SourceItemID, TargetItemID: relation.TargetItemID,
			SourceIsNewItem: relation.SourceIsNewItem, TargetIsNewItem: relation.TargetIsNewItem,
			Kind: coordinator.RelationKind(relation.Kind), CreatedBy: actor,
		})
	}
	return inputs
}

func scopeAllowed(principal coordinator.Principal, allowed ...coordinator.TokenScope) bool {
	for _, scope := range allowed {
		if principal.Scope == scope {
			return true
		}
		// A console session runs with owner-equivalent authority: any gate that
		// admits the owner also admits a console principal.
		if scope == coordinator.TokenScopeOwner && principal.Scope == coordinator.TokenScopeConsole {
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

var errProjectReadForbidden = errors.New("task-facing project read is not permitted")

// checkProjectReadAccess authorizes the deliberately narrow task-facing read
// surface. SourceTaskID remains a write boundary; reads instead use the
// project association. A worker proves that association with a currently live
// lease in this project's queue, rather than a client-supplied lease id.
func (s *projectServer) checkProjectReadAccess(ctx context.Context, principal coordinator.Principal) error {
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession, coordinator.TokenScopeConsole:
		if principal.ProjectID != nil && strings.TrimSpace(*principal.ProjectID) != "" &&
			strings.TrimSpace(*principal.ProjectID) == strings.TrimSpace(s.project.ID) {
			return nil
		}
	case coordinator.TokenScopeWorker:
		if s.workers == nil {
			return errors.New("worker service is not configured")
		}
		live, err := s.workers.HasLiveLeaseForWorker(ctx, principal.Subject)
		if err != nil {
			return fmt.Errorf("check worker project lease: %w", err)
		}
		if live {
			return nil
		}
	}

	return errProjectReadForbidden
}

func (s *projectServer) requireProjectReadAccess(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) bool {
	if err := s.checkProjectReadAccess(r.Context(), principal); err != nil {
		if errors.Is(err, errProjectReadForbidden) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeInternalError(w, r, "project_read_authorization_failed", err)
		}
		return false
	}
	return true
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

func checkBoundTaskScope(principal coordinator.Principal, taskID string) error {
	if (principal.Scope != coordinator.TokenScopeSession && principal.Scope != coordinator.TokenScopeConsole) || principal.SourceTaskID == nil {
		return nil
	}
	if strings.TrimSpace(*principal.SourceTaskID) != strings.TrimSpace(taskID) {
		return errors.New("session credential cannot operate on a different task")
	}
	return nil
}

// checkBoundTaskRelationScope confines direct relation mutations (link/unlink)
// for task-bound console credentials: the resolved relation must involve the
// console's bound task as source or target. Unbound project consoles are
// unaffected.
func checkBoundTaskRelationScope(principal coordinator.Principal, sourceTaskID, targetTaskID string) error {
	if principal.Scope != coordinator.TokenScopeConsole || principal.SourceTaskID == nil {
		return nil
	}
	bound := strings.TrimSpace(*principal.SourceTaskID)
	if strings.TrimSpace(sourceTaskID) != bound && strings.TrimSpace(targetTaskID) != bound {
		return errors.New("console credential cannot mutate a relation that does not involve its bound task")
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

// sessionHarnessForJob reads the harness stamped into the job payload.
// Producers must stamp a complete payload; an absent, empty, or wrong-typed
// value is corrupt job data, not a fallback to the configured default.
func sessionHarnessForJob(job worker.Job) (string, error) {
	if job.Role == worker.RoleConsole {
		consoleHarness, ok := job.Payload["console_harness"].(string)
		if !ok || strings.TrimSpace(consoleHarness) == "" {
			return "", fmt.Errorf("console job %s payload is missing console_harness", job.ID)
		}
		consoleHarness = flowharness.NormalizeName(consoleHarness)
		if err := flowharness.ValidateConsoleName(consoleHarness); err != nil {
			return "", fmt.Errorf("console job %s payload: %w", job.ID, err)
		}
		return consoleHarness, nil
	}

	agentHarness, ok := job.Payload["agent_harness"].(string)
	if !ok || strings.TrimSpace(agentHarness) == "" {
		return "", fmt.Errorf("job %s payload is missing agent_harness", job.ID)
	}
	agentHarness = flowharness.NormalizeName(agentHarness)
	if _, ok := flowharness.Lookup(agentHarness); !ok {
		return "", fmt.Errorf("job %s payload: unsupported agent harness %q", job.ID, agentHarness)
	}
	return agentHarness, nil
}

type createTaskRequest struct {
	Title               string                                   `json:"title"`
	Body                string                                   `json:"body"`
	Priority            int                                      `json:"priority"`
	RequiresHumanReview *bool                                    `json:"requires_human_review"`
	FlowID              string                                   `json:"flow_id"`
	ParentItemID        string                                   `json:"parent_item_id"`
	FeatureID           *string                                  `json:"feature_id"`
	ScheduleState       string                                   `json:"-"`
	TriageState         string                                   `json:"-"`
	CreatedBySessionID  *string                                  `json:"created_by_session_id"`
	SourceTaskID        *string                                  `json:"source_task_id"`
	SourceChangeID      *string                                  `json:"source_change_id"`
	Tags                []tagRequest                             `json:"tags"`
	Relations           []relationRequest                        `json:"relations"`
	WorkItemRelations   []contract.CreateWorkItemRelationRequest `json:"work_item_relations,omitempty"`
}

type tagRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type relationRequest = contract.TaskRelationRequest

type editTaskRequest struct {
	Title               *string `json:"title"`
	Body                *string `json:"body"`
	Priority            *int    `json:"priority"`
	RequiresHumanReview *bool   `json:"requires_human_review"`
	FlowID              *string `json:"flow_id"`
	FeatureID           *string `json:"feature_id"`
}

type scheduleTaskRequest = contract.ScheduleTaskRequest
type taskStateRequest = contract.TaskStateRequest
type triageTaskRequest = contract.TriageTaskRequest
type registerWorkerRequest = contract.RegisterWorkerRequest
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

type taskResponse struct {
	Task        coordinator.Task             `json:"task"`
	ProjectID   string                       `json:"project_id,omitempty"`
	ProjectName string                       `json:"project_name,omitempty"`
	StatusLog   []coordinator.StatusLogEntry `json:"status_log,omitempty"`
	Detail      *uiTaskDetail                `json:"task_detail,omitempty"`
	// Blocked is the derived needs-attention flag: unresolved effective
	// blockers for unscheduled tasks, open waits or a system hold for
	// in-progress ones.
	Blocked bool `json:"blocked"`
}

// lifecycleTransitionResponse is the unified task lifecycle endpoint response.
// It mirrors taskResponse so app.refresh() still works, and optionally carries
// the active run produced or mutated by the transition.
type lifecycleTransitionResponseAlias struct {
	Task        coordinator.Task         `json:"task"`
	ProjectID   string                   `json:"project_id,omitempty"`
	ProjectName string                   `json:"project_name,omitempty"`
	Run         *coordinator.WorkflowRun `json:"run,omitempty"`
}

type tasksResponse = contract.TasksResponse
type taskAttachmentResponse = contract.TaskAttachmentResponse
type taskAttachmentsResponse = contract.TaskAttachmentsResponse

type sessionMessagesResponse = contract.SessionMessagesResponse
type sessionMessageResponse = contract.SessionMessageResponse

type boardResponse struct {
	contract.BoardResponse
	TaskCards  map[string]uiTaskCard     `json:"task_cards,omitempty"`
	Containers []uiBoardContainerSummary `json:"containers,omitempty"`
	// Features is retained for older board clients. It is populated from the same
	// canonical work-item summaries as Containers rather than the legacy feature
	// cache, so both projections agree after hierarchy mutations.
	Features []featureBoardEntry `json:"features,omitempty"`
}

type uiBoardContainerSummary struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	TaskCount int    `json:"task_count"`
}

type featureBoardEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// doneResponse is the per-project read model for terminal (closed) tasks. It
// mirrors the board's owner-scoped card gating but carries the derived outcome
// phase per task plus a keyset cursor for paging older history.
type doneResponse struct {
	Tasks        []coordinator.Task           `json:"tasks"`
	Outcomes     map[string]coordinator.Phase `json:"outcomes"`
	TaskCards    map[string]uiDoneCard        `json:"task_cards,omitempty"`
	NextBefore   string                       `json:"next_before,omitempty"`
	NextBeforeID string                       `json:"next_before_id,omitempty"`
}

// uiDoneCard is a lean card for a closed task: just the merged change (if any)
// and tags. It deliberately omits the session/job/diff/check fan-out that
// buildUITaskCards performs for active tasks.
type uiDoneCard struct {
	TaskID string            `json:"task_id"`
	Change *uiChangeSummary  `json:"change,omitempty"`
	Tags   []coordinator.Tag `json:"tags,omitempty"`
}

type changeResponse struct {
	Change             coordinator.Change         `json:"change"`
	ProjectID          string                     `json:"project_id,omitempty"`
	ProjectName        string                     `json:"project_name,omitempty"`
	Task               coordinator.Task           `json:"task"`
	OpenWait           *coordinator.WorkflowWait  `json:"open_wait,omitempty"`
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

type uiTaskCard struct {
	TaskID                string                         `json:"task_id"`
	Container             *uiBoardContainerSummary       `json:"container,omitempty"`
	Tags                  []coordinator.Tag              `json:"tags,omitempty"`
	Relations             uiRelationSummary              `json:"relations"`
	CurrentStep           *uiWorkflowStepSummary         `json:"current_step,omitempty"`
	ActiveSession         *uiSessionSummary              `json:"active_session,omitempty"`
	TerminalAvailable     bool                           `json:"terminal_available,omitempty"`
	TerminalJobID         string                         `json:"terminal_job_id,omitempty"`
	Change                *uiChangeSummary               `json:"change,omitempty"`
	DiffStats             *uiDiffStatSummary             `json:"diff_stats,omitempty"`
	DiffUnavailableReason string                         `json:"diff_unavailable_reason,omitempty"`
	Handoff               *uiHandoffSummary              `json:"handoff,omitempty"`
	RequiredChecks        uiRequiredCheckSummary         `json:"required_checks"`
	ReviewCycleBudget     *coordinator.ReviewCycleBudget `json:"review_cycle_budget,omitempty"`
	LatestStatus          *coordinator.StatusLogEntry    `json:"latest_status,omitempty"`
	Blockers              uiBlockerSummary               `json:"blockers"`
	CrashRetryAvailable   bool                           `json:"crash_retry_available,omitempty"`
	// DwellSince is when the task entered the state it is in now. The board
	// renders the elapsed time as its load-bearing number: a task sitting
	// somewhere too long is the thing worth noticing.
	DwellSince *time.Time `json:"dwell_since,omitempty"`
	// LastAgentActivityAt is the most recent agent activity across the task's
	// sessions, feeding the board's "last active" sort.
	LastAgentActivityAt *time.Time `json:"last_agent_activity_at,omitempty"`
	// StepIndex/StepCount place the current node in the frozen graph ("3/6").
	// Ordinals, not progress: branching graphs visit nodes out of slice order.
	StepIndex int `json:"step_index,omitempty"`
	StepCount int `json:"step_count,omitempty"`
	// Wait is the open durable wait, so the board can state the real question
	// or the verbatim error instead of a generic "blocked".
	Wait   *uiWorkflowWait `json:"wait,omitempty"`
	Held   bool            `json:"held,omitempty"`
	HeldBy string          `json:"held_by,omitempty"`
}

type uiWorkflowStepSummary struct {
	Key  string               `json:"key"`
	Name string               `json:"name"`
	Kind coordinator.NodeKind `json:"kind,omitempty"`
}

type uiWorkflowWait struct {
	Kind      coordinator.WorkflowWaitKind   `json:"kind"`
	Reason    coordinator.WorkflowWaitReason `json:"reason,omitempty"`
	Message   string                         `json:"message,omitempty"`
	NodeRunID string                         `json:"node_run_id,omitempty"`
	CreatedAt time.Time                      `json:"created_at"`
}

type uiTaskDetail struct {
	Tags                []coordinator.Tag              `json:"tags,omitempty"`
	Relations           []coordinator.TaskRelation     `json:"relations,omitempty"`
	ActiveSession       *uiSessionSummary              `json:"active_session,omitempty"`
	Paused              bool                           `json:"paused,omitempty"`
	TerminalAvailable   bool                           `json:"terminal_available,omitempty"`
	TerminalJobID       string                         `json:"terminal_job_id,omitempty"`
	Sessions            []uiSessionSummary             `json:"sessions,omitempty"`
	Changes             []uiChangeSummary              `json:"changes,omitempty"`
	ReadyChange         *uiChangeSummary               `json:"ready_change,omitempty"`
	ReviewState         coordinator.ReviewState        `json:"review_state,omitempty"`
	RequiredChecks      uiRequiredCheckSummary         `json:"required_checks"`
	ReviewCycleBudget   *coordinator.ReviewCycleBudget `json:"review_cycle_budget,omitempty"`
	WaitReason          coordinator.WaitReason         `json:"wait_reason,omitempty"`
	CrashRetryAvailable bool                           `json:"crash_retry_available,omitempty"`
	TaskConsole         *consoleResponse               `json:"task_console,omitempty"`
	Checks              []coordinator.Check            `json:"checks,omitempty"`
	// Transitions is the task's workflow transition log, newest first. It is
	// what the Activity tab renders.
	Transitions         []coordinator.WorkflowTransition `json:"transitions,omitempty"`
	ConvergenceEvidence *coordinator.ConvergenceEvidence `json:"convergence_evidence,omitempty"`
	Attachments         []coordinator.TaskAttachment     `json:"attachments,omitempty"`
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
	Errored            int  `json:"errored"`
}

type uiBlockerSummary struct {
	Count int                    `json:"count"`
	Tasks []uiBlockerTaskSummary `json:"tasks,omitempty"`
	// Omitted counts live blockers left out of Tasks because the display is
	// bounded; the card renders it as "+N more" so nothing hides silently.
	Omitted int `json:"omitted,omitempty"`
}

type uiBlockerTaskSummary struct {
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

func taskFilterFromQuery(r *http.Request) (coordinator.TaskFilter, error) {
	var filter coordinator.TaskFilter
	for _, state := range r.URL.Query()["state"] {
		if state == "" {
			continue
		}
		switch state {
		case "unscheduled", string(coordinator.LifecycleScheduled), string(coordinator.LifecycleInProgress), string(coordinator.LifecycleDone):
			filter.LifecycleStates = append(filter.LifecycleStates, state)
		default:
			return coordinator.TaskFilter{}, fmt.Errorf("invalid state %q", state)
		}
	}
	filter.TagSlugs = r.URL.Query()["tag"]
	filter.Search = r.URL.Query().Get("q")

	return filter, nil
}

// readyFilterFromQuery reports whether the request asks for the ready read
// model (unscheduled tasks with no unresolved effective blockers).
func readyFilterFromQuery(r *http.Request) (bool, error) {
	switch value := strings.TrimSpace(r.URL.Query().Get("ready")); value {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("invalid ready %q", value)
	}
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
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

// writeInternalError logs the full internal error server-side and writes a
// generic client-facing 5xx response, so raw store/SQLite error detail never
// crosses the trust boundary. The error code is preserved so clients can still
// classify the failure.
func writeInternalError(w http.ResponseWriter, r *http.Request, code string, err error) {
	slog.ErrorContext(r.Context(), "internal error handling request", "code", code, "error", err)
	writeError(w, http.StatusInternalServerError, code, "internal server error")
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
