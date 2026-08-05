package contract

import (
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const (
	ProtocolVersion   = "7"
	ProtocolHeader    = "Flow-Protocol-Version"
	IdempotencyHeader = "Idempotency-Key"
	AuthScheme        = "Bearer "
)

type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RepoPath     string `json:"repo_path,omitempty"`
	BaseBranch   string `json:"base_branch"`
	ExchangeName string `json:"exchange_name"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type CreateProjectRequest struct {
	Name         string `json:"name"`
	RepoPath     string `json:"repo_path"`
	BaseBranch   string `json:"base_branch"`
	ExchangeName string `json:"exchange_name"`
}

type ProjectResponse struct {
	Project Project `json:"project"`
	Created bool    `json:"created,omitempty"`
}

type ProjectsResponse struct {
	Projects []Project `json:"projects"`
}

type HarnessOption struct {
	Name        string              `json:"name"`
	DisplayName string              `json:"display_name"`
	DefaultArgs []string            `json:"default_args,omitempty"`
	Models      []flowharness.Model `json:"models,omitempty"`
}

type HarnessesResponse struct {
	Agents   []HarnessOption `json:"agents"`
	Consoles []HarnessOption `json:"consoles"`
}

type ScheduleTaskRequest struct {
	State string `json:"state"`
}

type TaskStateRequest struct {
	State string `json:"state"`
}

type TriageTaskRequest struct {
	State string `json:"state"`
}

type TaskRelationRequest struct {
	SourceTaskID string `json:"source_task_id,omitempty"`
	TargetTaskID string `json:"target_task_id"`
	Kind         string `json:"kind"`
	// TargetIsNewTask is an explicit opt-in, honored only on the create-task
	// path, that resolves a blank TargetTaskID to the task being created. It
	// lets a caller make the new task the relation *target* — e.g. an existing
	// parent linked parent_of the new task — inside the single create
	// transaction, so a child-of link cannot partially succeed. The link and
	// unlink endpoints ignore it.
	TargetIsNewTask bool `json:"target_is_new_task,omitempty"`
}

type CreateEpicRequest struct {
	Title             string                          `json:"title"`
	Body              string                          `json:"body,omitempty"`
	Priority          int                             `json:"priority,omitempty"`
	CompletionPolicy  string                          `json:"completion_policy,omitempty"`
	ParentItemID      string                          `json:"parent_item_id,omitempty"`
	WorkItemRelations []CreateWorkItemRelationRequest `json:"work_item_relations,omitempty"`
}

type EditEpicRequest struct {
	Title            *string `json:"title,omitempty"`
	Body             *string `json:"body,omitempty"`
	Priority         *int    `json:"priority,omitempty"`
	CompletionPolicy *string `json:"completion_policy,omitempty"`
}

type WorkItemRelationRequest struct {
	SourceItemID string `json:"source_item_id"`
	TargetItemID string `json:"target_item_id"`
	Kind         string `json:"kind"`
}

// CreateWorkItemRelationRequest relates an existing item to the item being
// created. Exactly one endpoint must be marked new, and that endpoint ID blank.
type CreateWorkItemRelationRequest struct {
	SourceItemID    string `json:"source_item_id,omitempty"`
	TargetItemID    string `json:"target_item_id,omitempty"`
	SourceIsNewItem bool   `json:"source_is_new_item,omitempty"`
	TargetIsNewItem bool   `json:"target_is_new_item,omitempty"`
	Kind            string `json:"kind"`
}

type MoveWorkItemRequest struct {
	ParentItemID string `json:"parent_item_id"`
}

type MoveWorkItemsRequest struct {
	ItemIDs      []string `json:"item_ids"`
	ParentItemID string   `json:"parent_item_id"`
}

type MoveWorkItemsResponse struct {
	Items []coordinator.WorkItemSummary `json:"items"`
}

type WorkItemResponse struct {
	Item      coordinator.WorkItemSummary    `json:"item"`
	Relations []coordinator.WorkItemRelation `json:"relations,omitempty"`
	Blockers  []coordinator.WorkItemBlocker  `json:"blockers,omitempty"`
	Children  []WorkItemResponse             `json:"children,omitempty"`
}

type WorkItemsResponse struct {
	Items []coordinator.WorkItemSummary `json:"items"`
}

// ActionReadiness gives clients a stable machine reason and useful operator
// copy whenever an action is not currently available.
type ActionReadiness struct {
	Allowed    bool   `json:"allowed"`
	ReasonCode string `json:"reason_code,omitempty"`
	DenialText string `json:"denial_text,omitempty"`
}

type WorkItemActionReadiness struct {
	Start    *ActionReadiness `json:"start,omitempty"`
	Complete *ActionReadiness `json:"complete,omitempty"`
	Rebase   *ActionReadiness `json:"rebase,omitempty"`
	Land     *ActionReadiness `json:"land,omitempty"`
}

type WorkItemRollup struct {
	DirectChildren  coordinator.WorkItemDirectChildren  `json:"direct_children"`
	DescendantTasks coordinator.WorkItemDescendantTasks `json:"descendant_tasks"`
}

// FeatureOverview is live feature state. GitAvailable is always explicit so
// zero ahead/behind values are never mistaken for a successful Git read.
type FeatureOverview struct {
	Branch               string                     `json:"branch"`
	IntegrationTarget    string                     `json:"integration_target"`
	Ahead                int                        `json:"ahead"`
	Behind               int                        `json:"behind"`
	GitAvailable         bool                       `json:"git_available"`
	GitUnavailableReason string                     `json:"git_unavailable_reason,omitempty"`
	RunningRebase        *coordinator.FeatureRebase `json:"running_rebase,omitempty"`
}

type WorkItemOverviewEntry struct {
	Item                coordinator.WorkItemSummary `json:"item"`
	Rollup              WorkItemRollup              `json:"rollup"`
	AttentionCount      int                         `json:"attention_count"`
	CriticalPathTaskIDs []string                    `json:"critical_path_task_ids,omitempty"`
	Feature             *FeatureOverview            `json:"feature,omitempty"`
	Actions             WorkItemActionReadiness     `json:"actions"`
}

type WorkItemOverviewResponse struct {
	Items []WorkItemOverviewEntry `json:"items"`
}

type WorkItemContextResponse struct {
	Item           coordinator.WorkItemSummary    `json:"item"`
	Ancestors      []coordinator.WorkItemSummary  `json:"ancestors"`
	Children       []coordinator.WorkItemSummary  `json:"children"`
	Blockers       []coordinator.WorkItemBlocker  `json:"blockers"`
	Relations      []coordinator.WorkItemRelation `json:"relations"`
	Rollup         WorkItemRollup                 `json:"rollup"`
	AttentionCount int                            `json:"attention_count"`
	Feature        *FeatureOverview               `json:"feature,omitempty"`
	Actions        WorkItemActionReadiness        `json:"actions"`
}

type EpicResponse struct {
	Epic     coordinator.Epic              `json:"epic"`
	Item     coordinator.WorkItemSummary   `json:"item"`
	Children []coordinator.WorkItemSummary `json:"children,omitempty"`
	Blockers []coordinator.WorkItemBlocker `json:"blockers,omitempty"`
}

type EpicsResponse struct {
	Epics []EpicResponse `json:"epics"`
}

type ReviewFollowUpFinding struct {
	SHA                string `json:"sha"`
	File               string `json:"file"`
	Line               int    `json:"line"`
	Body               string `json:"body"`
	Severity           string `json:"severity"`
	IntroducedByChange bool   `json:"introduced_by_change"`
	Requirement        string `json:"requirement"`
	DuplicateOf        string `json:"duplicate_of,omitempty"`
}

type ReviewFollowUpTaskAction struct {
	Action string `json:"action"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type ApplyReviewFollowUpRequest struct {
	LeaseID    string                   `json:"lease_id"`
	Finding    ReviewFollowUpFinding    `json:"finding"`
	TaskAction ReviewFollowUpTaskAction `json:"task_action"`
}

type ApplyReviewFollowUpResponse struct {
	Task        coordinator.Task `json:"task"`
	Disposition string           `json:"disposition"`
}

type RegisterWorkerRequest struct {
	ID                  string              `json:"id"`
	Labels              map[string]string   `json:"labels"`
	Taints              []scheduler.Taint   `json:"taints"`
	HarnessModels       []flowharness.Model `json:"harness_models,omitempty"`
	HeartbeatTTLSeconds int                 `json:"heartbeat_ttl_seconds"`
}

type HeartbeatWorkerRequest struct {
	WorkerID            string `json:"worker_id"`
	HeartbeatTTLSeconds int    `json:"heartbeat_ttl_seconds"`
}

type ClaimJobRequest struct {
	WorkerID             string `json:"worker_id"`
	LeaseDurationSeconds int    `json:"lease_duration_seconds"`
	WaitSeconds          int    `json:"wait_seconds"`
}

// ReserveProvisionerAssignmentRequest describes the immutable virtual-worker
// profile used to select and reserve one exact queued job. MaxConcurrency is
// enforced for ProviderID/ProfileName across every open project database.
type ReserveProvisionerAssignmentRequest struct {
	ProviderID            string                  `json:"provider_id"`
	ProviderRequestID     string                  `json:"provider_request_id"`
	ProfileName           string                  `json:"profile_name"`
	ProviderType          string                  `json:"provider_type"`
	ProviderOptions       map[string]string       `json:"provider_options,omitempty"`
	MaxConcurrency        int                     `json:"max_concurrency"`
	AllowedRoles          []worker.JobRole        `json:"allowed_roles,omitempty"`
	AllowedBuckets        []worker.CapacityBucket `json:"allowed_buckets,omitempty"`
	Labels                map[string]string       `json:"labels,omitempty"`
	Taints                []scheduler.Taint       `json:"taints,omitempty"`
	HarnessModels         []flowharness.Model     `json:"harness_models,omitempty"`
	RequiredSelector      map[string]string       `json:"required_selector,omitempty"`
	StartupTimeoutSeconds int                     `json:"startup_timeout_seconds"`
	WaitSeconds           int                     `json:"wait_seconds,omitempty"`
}

// ProvisionerAssignment includes project identity because assignment rows are
// project-local. Assignment carries the complete durable lifecycle snapshot.
type ProvisionerAssignment struct {
	Project    Project           `json:"project"`
	Assignment worker.Assignment `json:"assignment"`
}

type ReserveProvisionerAssignmentResponse struct {
	Reserved    bool                   `json:"reserved"`
	Assignment  *ProvisionerAssignment `json:"assignment,omitempty"`
	WorkerToken string                 `json:"worker_token,omitempty"`
}

type ProvisionerAssignmentsResponse struct {
	Assignments []ProvisionerAssignment `json:"assignments"`
}

type AbandonProvisionerAssignmentRequest struct {
	ProviderError string `json:"provider_error"`
}

type RecordProvisionerAssignmentAttemptRequest struct {
	ProviderError string     `json:"provider_error,omitempty"`
	NextRetryAt   *time.Time `json:"next_retry_at,omitempty"`
}

type ProvisionerAssignmentResponse struct {
	Assignment ProvisionerAssignment `json:"assignment"`
}

type RenewLeaseRequest struct {
	LeaseID              string `json:"lease_id"`
	LeaseDurationSeconds int    `json:"lease_duration_seconds"`
}

type WorkerJobStatusRequest struct {
	LeaseID string `json:"lease_id"`
}

type MarkJobRunningRequest struct {
	LeaseID string `json:"lease_id"`
}

type ReleaseLeaseRequest struct {
	LeaseID    string `json:"lease_id"`
	FinalState string `json:"final_state"`
}

type EnqueueJobRequest struct {
	TaskID         *string                `json:"task_id"`
	ChangeID       *string                `json:"change_id"`
	Role           string                 `json:"role"`
	CapacityBucket string                 `json:"capacity_bucket"`
	Priority       int                    `json:"priority"`
	RunsOn         map[string]string      `json:"runs_on"`
	Requires       []string               `json:"requires"`
	Size           string                 `json:"size"`
	Tolerations    []scheduler.Toleration `json:"tolerations"`
	Payload        map[string]any         `json:"payload"`
}

type ConsoleRequest struct {
	Harness string `json:"harness"`
}

type ApproveReviewCyclesRequest struct {
	Cycles       int    `json:"cycles,omitempty"`
	Instructions string `json:"instructions"`
}

type ReportCheckRequest struct {
	Kind        string  `json:"kind"`
	Required    *bool   `json:"required"`
	Verdict     string  `json:"verdict"`
	ExitCode    *int    `json:"exit_code"`
	Details     string  `json:"details"`
	SourceJobID *string `json:"source_job_id"`
	LeaseID     *string `json:"lease_id"`
	Reporter    string  `json:"reporter"`
}

type SessionEventRequest struct {
	State  string `json:"state"`
	Source string `json:"source,omitempty"`
}

type SessionSignalRequest struct {
	Signal        string `json:"signal"`
	Source        string `json:"source,omitempty"`
	Harness       string `json:"harness,omitempty"`
	HookEventName string `json:"hook_event_name,omitempty"`
	Details       string `json:"details,omitempty"`
}

type ReadySessionRequest struct {
	HeadSHA string `json:"head_sha"`
}

type SessionStatusRequest struct {
	Message string `json:"message"`
	Kind    string `json:"kind"`
}

type SessionProcessExitRequest struct {
	LeaseID  string `json:"lease_id"`
	ExitCode int    `json:"exit_code"`
}

type SessionMessagesRequest struct {
	LeaseID string `json:"lease_id"`
	Limit   int    `json:"limit,omitempty"`
}

type SessionMessageDeliveredRequest struct {
	LeaseID string `json:"lease_id"`
}

type AttentionReplyRequest struct {
	Message     string `json:"message"`
	StatusLogID *int64 `json:"status_log_id,omitempty"`
}

type SessionTerminalRequest struct {
	TmuxSocketPath string `json:"tmux_socket_path,omitempty"`
}

type JobTerminalRequest struct {
	LeaseID        string `json:"lease_id"`
	TmuxSocketPath string `json:"tmux_socket_path,omitempty"`
}

type CreateThreadRequest struct {
	AnchorCommitSHA string `json:"anchor_commit_sha"`
	FilePath        string `json:"file_path"`
	Line            int    `json:"line"`
	Context         string `json:"context"`
	Body            string `json:"body"`
	LeaseID         string `json:"lease_id"`
}

type PutHandoffRequest struct {
	Content string `json:"content"`
	HeadSHA string `json:"head_sha"`
}

type ThreadCommentRequest struct {
	Body    string `json:"body"`
	LeaseID string `json:"lease_id"`
}

type ThreadClaimRequest struct {
	Kind           string `json:"kind"`
	Body           string `json:"body"`
	ClaimCommitSHA string `json:"claim_commit_sha"`
	LeaseID        string `json:"lease_id"`
}

type TasksResponse struct {
	Tasks []coordinator.Task `json:"tasks"`
}

// CreateFeatureRequest creates a feature: a project-child task group with its
// own long-lived branch in the exchange.
type CreateFeatureRequest struct {
	Title             string                          `json:"title"`
	Body              string                          `json:"body"`
	ParentItemID      string                          `json:"parent_item_id,omitempty"`
	WorkItemRelations []CreateWorkItemRelationRequest `json:"work_item_relations,omitempty"`
}

// UpdateFeatureRequest edits feature metadata; nil fields are left unchanged.
type UpdateFeatureRequest struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
}

// FeatureTaskCounts summarizes the feature's assigned tasks by lifecycle.
type FeatureTaskCounts struct {
	Open       int `json:"open"`
	Scheduled  int `json:"scheduled"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
}

// FeatureResponse is the feature payload: the row, task counts, the live
// branch divergence, and — on the detail read — the assigned tasks and the
// rebase history.
type FeatureResponse struct {
	Feature       coordinator.Feature             `json:"feature"`
	Item          *coordinator.WorkItemSummary    `json:"item,omitempty"`
	Parent        *coordinator.WorkItemSummary    `json:"parent,omitempty"`
	Children      []coordinator.WorkItemSummary   `json:"children,omitempty"`
	Blockers      []coordinator.WorkItemBlocker   `json:"blockers,omitempty"`
	Counts        FeatureTaskCounts               `json:"counts"`
	BranchState   *coordinator.FeatureBranchState `json:"branch_state,omitempty"`
	RunningRebase *coordinator.FeatureRebase      `json:"running_rebase,omitempty"`
	Tasks         []coordinator.Task              `json:"tasks,omitempty"`
	Rebases       []coordinator.FeatureRebase     `json:"rebases,omitempty"`
}

type FeaturesResponse struct {
	Features []FeatureResponse `json:"features"`
}

// RebaseFeatureResponse reports what a rebase request did: rebased instantly,
// already up to date, or a rebase task was created for a conflicted rebase.
type RebaseFeatureResponse struct {
	Result coordinator.RebaseStartResult `json:"result"`
}

type TaskResponse struct {
	Task      coordinator.Task             `json:"task"`
	StatusLog []coordinator.StatusLogEntry `json:"status_log,omitempty"`
}

type TaskAttachmentResponse struct {
	Attachment coordinator.TaskAttachment `json:"attachment"`
}

type TaskAttachmentsResponse struct {
	Attachments []coordinator.TaskAttachment `json:"attachments"`
}

type TaskRelationsResponse struct {
	Relations []coordinator.TaskRelation `json:"relations"`
}

type BoardResponse struct {
	Board       coordinator.Board                 `json:"board"`
	LaneStates  map[string]coordinator.LaneState  `json:"lane_states,omitempty"`
	WaitReasons map[string]coordinator.WaitReason `json:"wait_reasons,omitempty"`
	BlockedIDs  []string                          `json:"blocked_ids,omitempty"`
}

type ProjectBoardResponse struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	BoardResponse
}

type AggregateBoardResponse struct {
	Boards []ProjectBoardResponse `json:"boards"`
}

type MergeResponse struct {
	Merge coordinator.MergeResult `json:"merge"`
}

type WebBootstrapResponse struct {
	LoginPath string    `json:"login_path"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CheckFollowUpFailure reports work that a satisfied check triggered and that
// then failed — the check itself stands, but something downstream of it did
// not. Surfaced to the worker so a follow-up failure is not silent.
type CheckFollowUpFailure struct {
	EventKind string `json:"event_kind"`
	Details   string `json:"details"`
}

type CheckResponse struct {
	Check            coordinator.Check        `json:"check"`
	ReviewState      coordinator.ReviewState  `json:"review_state"`
	FollowUpFailures []CheckFollowUpFailure   `json:"follow_up_failures,omitempty"`
	Workflow         *coordinator.WorkflowRun `json:"workflow,omitempty"`
}

type ChecksResponse struct {
	Checks      []coordinator.Check     `json:"checks"`
	ReviewState coordinator.ReviewState `json:"review_state"`
}

type TransitionsResponse struct {
	Transitions []coordinator.WorkflowTransition `json:"transitions"`
}

type ReviewRunResponse struct {
	Change      coordinator.Change                    `json:"change"`
	Scheduled   coordinator.ScheduleReviewRoundResult `json:"scheduled"`
	Checks      []coordinator.Check                   `json:"checks"`
	ReviewState coordinator.ReviewState               `json:"review_state"`
}

type ReviewCycleBudgetResponse struct {
	Budget           coordinator.ReviewCycleBudget `json:"budget"`
	FollowUpFailures []CheckFollowUpFailure        `json:"follow_up_failures,omitempty"`
}

type WorkerResponse struct {
	Worker worker.Worker `json:"worker"`
}

type WorkersResponse struct {
	Workers []worker.Worker `json:"workers"`
}

type JobResponse struct {
	Job          worker.Job           `json:"job"`
	Change       *coordinator.Change  `json:"change,omitempty"`
	Session      *coordinator.Session `json:"session,omitempty"`
	SessionToken string               `json:"session_token,omitempty"`
}

type JobsResponse struct {
	Jobs []worker.Job `json:"jobs"`
}

type ConsoleResponse struct {
	Active            bool                         `json:"active"`
	ProjectID         string                       `json:"project_id,omitempty"`
	ProjectName       string                       `json:"project_name,omitempty"`
	Job               *worker.Job                  `json:"job,omitempty"`
	Session           *coordinator.Session         `json:"session,omitempty"`
	Terminal          *coordinator.SessionTerminal `json:"terminal,omitempty"`
	TerminalAvailable bool                         `json:"terminal_available,omitempty"`
}

type WorkerJobStatusResponse struct {
	Job     worker.Job           `json:"job"`
	Lease   worker.Lease         `json:"lease"`
	Session *coordinator.Session `json:"session,omitempty"`
}

type SessionResponse struct {
	Session coordinator.Session `json:"session"`
}

type AttachResponse struct {
	Attach terminal.AttachInfo `json:"attach"`
}

type SessionTerminalResponse struct {
	Terminal coordinator.SessionTerminal `json:"terminal"`
}

type SessionTerminalAccessResponse struct {
	Access coordinator.SessionTerminalAccess `json:"access"`
}

type JobTerminalResponse struct {
	Terminal coordinator.JobTerminal `json:"terminal"`
}

type ThreadResponse struct {
	Thread coordinator.ReviewThread `json:"thread"`
}

type HandoffResponse struct {
	ChangeID string `json:"change_id"`
	HeadSHA  string `json:"head_sha"`
	Present  bool   `json:"present"`
	Valid    bool   `json:"valid"`
	Summary  string `json:"summary"`
	Content  string `json:"content,omitempty"`
}

type ThreadsResponse struct {
	Threads []coordinator.ReviewThread `json:"threads"`
}

// TaskFindingsResponse is the /v2/tasks/{id}/findings read model: every review
// thread across the task's changes, every deferred follow-up action, and
// resolution-bucket counts over both.
type TaskFindingsResponse struct {
	TaskID    string                           `json:"task_id"`
	Findings  []coordinator.TaskReviewFinding  `json:"findings"`
	FollowUps []coordinator.TaskReviewFollowUp `json:"follow_ups"`
	Summary   coordinator.TaskFindingsSummary  `json:"summary"`
}

type StatusResponse struct {
	Status coordinator.StatusLogEntry `json:"status"`
}

type SessionMessagesResponse struct {
	Messages []coordinator.SessionMessage `json:"messages"`
}

type SessionMessageResponse struct {
	Message coordinator.SessionMessage `json:"message"`
	Queued  bool                       `json:"queued"`
}

type ReconcileResponse struct {
	Result coordinator.ReconcileResult `json:"result"`
}

type LeaseResponse struct {
	Lease worker.Lease `json:"lease"`
}

type ClaimJobResponse struct {
	Claimed   bool          `json:"claimed"`
	ProjectID string        `json:"project_id,omitempty"`
	Job       *worker.Job   `json:"job,omitempty"`
	Lease     *worker.Lease `json:"lease,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string                          `json:"code"`
	Message string                          `json:"message"`
	Issues  []coordinator.WorkItemMoveIssue `json:"issues,omitempty"`
}
