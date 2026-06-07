package contract

import (
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const (
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
	ExchangeURL  string `json:"exchange_url"`
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

type ScheduleIssueRequest struct {
	State string `json:"state"`
}

type IssueStateRequest struct {
	State string `json:"state"`
}

type TriageIssueRequest struct {
	State string `json:"state"`
}

type IssueRelationRequest struct {
	SourceIssueID string `json:"source_issue_id,omitempty"`
	TargetIssueID string `json:"target_issue_id"`
	Kind          string `json:"kind"`
}

type RegisterWorkerRequest struct {
	ID                      string              `json:"id"`
	Labels                  map[string]string   `json:"labels"`
	Taints                  []scheduler.Taint   `json:"taints"`
	HarnessModels           []flowharness.Model `json:"harness_models,omitempty"`
	CapacityPersistentAgent int                 `json:"capacity_persistent_agent"`
	CapacityEphemeral       int                 `json:"capacity_ephemeral"`
	HeartbeatTTLSeconds     int                 `json:"heartbeat_ttl_seconds"`
}

type JoinWorkerRequest struct {
	WorkerID string `json:"worker_id"`
}

type JoinWorkerResponse struct {
	WorkerID string `json:"worker_id"`
	Token    string `json:"token"`
}

type HeartbeatWorkerRequest struct {
	WorkerID            string `json:"worker_id"`
	HeartbeatTTLSeconds int    `json:"heartbeat_ttl_seconds"`
}

type ClaimJobRequest struct {
	WorkerID             string                  `json:"worker_id"`
	Buckets              []worker.CapacityBucket `json:"buckets"`
	LeaseDurationSeconds int                     `json:"lease_duration_seconds"`
	WaitSeconds          int                     `json:"wait_seconds"`
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
	IssueID        *string                `json:"issue_id"`
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

type PlanRejectRequest struct {
	Comments string `json:"comments"`
}

type AttentionReplyRequest struct {
	Message     string `json:"message"`
	StatusLogID *int64 `json:"status_log_id,omitempty"`
}

type SessionTerminalRequest struct {
	TargetURL      string `json:"target_url"`
	TmuxSocketPath string `json:"tmux_socket_path,omitempty"`
}

type JobTerminalRequest struct {
	LeaseID        string `json:"lease_id"`
	TargetURL      string `json:"target_url"`
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

type IssuesResponse struct {
	Issues []coordinator.Issue `json:"issues"`
}

type IssueResponse struct {
	Issue     coordinator.Issue            `json:"issue"`
	StatusLog []coordinator.StatusLogEntry `json:"status_log,omitempty"`
}

type IssueAttachmentResponse struct {
	Attachment coordinator.IssueAttachment `json:"attachment"`
}

type IssueAttachmentsResponse struct {
	Attachments []coordinator.IssueAttachment `json:"attachments"`
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

type CheckFollowUpFailure = lifecycle.FollowUpFailure

type CheckResponse struct {
	Check            coordinator.Check       `json:"check"`
	ReviewState      coordinator.ReviewState `json:"review_state"`
	FollowUpFailures []CheckFollowUpFailure  `json:"follow_up_failures,omitempty"`
}

type ChecksResponse struct {
	Checks      []coordinator.Check     `json:"checks"`
	ReviewState coordinator.ReviewState `json:"review_state"`
}

type TransitionsResponse struct {
	Transitions []coordinator.TransitionLogEntry `json:"transitions"`
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
	Code    string `json:"code"`
	Message string `json:"message"`
}
