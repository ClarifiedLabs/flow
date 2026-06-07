package lifecycle

import "github.com/ClarifiedLabs/flow/internal/coordinator"

// EventKind enumerates every input the lifecycle FSM reacts to. External events
// map one-to-one to an API handler (or the timer ticker); internal events are
// the bounded follow-ons a transition may emit so a multi-step cascade becomes
// an explicit, ordered, logged sequence of edges.
type EventKind string

const (
	// External events (one per coordinator input).
	EventSessionReady   EventKind = "session_ready"
	EventCheckReported  EventKind = "check_reported"
	EventScheduleIssue  EventKind = "schedule_issue"
	EventSetIssueState  EventKind = "set_issue_state"
	EventTriageIssue    EventKind = "triage_issue"
	EventMergeRequested EventKind = "merge_requested"
	EventMergeChange    EventKind = "merge_change"
	EventThreadClaimed  EventKind = "thread_claimed"
	EventThreadCertify  EventKind = "thread_certify"
	EventThreadReopen   EventKind = "thread_reopen"
	EventThreadComment  EventKind = "thread_comment"

	EventSessionStateChanged   EventKind = "session_state_changed"
	EventCloseIssue            EventKind = "close_issue"
	EventResetIssue            EventKind = "reset_issue"
	EventRetryCrashedAuthorJob EventKind = "retry_crashed_author_job"

	// Deadline timers (durable, scheduled by the engine itself).
	EventPhaseDeadline EventKind = "phase_deadline"
	EventCheckTimeout  EventKind = "check_timeout"

	// Internal follow-on events emitted by actions during a Step cascade.
	EventEnsureFixAuthorJob EventKind = "ensure_fix_author_job"
	EventEnqueueAcceptance  EventKind = "enqueue_acceptance"
	EventAutoMerge          EventKind = "auto_merge"
	EventEnsureAuthorJob    EventKind = "ensure_author_job"

	// EventReconcile records a ticker-driven phase refresh in the transition log
	// (e.g. after crash recovery moved an issue out of an authoring phase). It is
	// applied directly, not dispatched through the transition table.
	EventReconcile EventKind = "reconcile"
)

// Event is the typed input to Engine.Step. The engine resolves IssueID for
// events keyed by change/thread/session before loading the snapshot.
type Event struct {
	Kind           EventKind
	IssueID        string
	ChangeID       string
	ThreadID       string
	SessionID      string
	Actor          coordinator.Principal
	Audit          EventAudit
	IdempotencyKey string
	Payload        EventPayload
}

// EventAudit carries request provenance for lifecycle-changing inputs. It is
// persisted in the inbox event JSON and embedded in transition payload_json so
// post-incident debugging does not depend on transient process logs.
type EventAudit struct {
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	Principal    string `json:"principal,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	ProjectName  string `json:"project_name,omitempty"`
	IssueID      string `json:"issue_id,omitempty"`
	ChangeID     string `json:"change_id,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	WebSessionID string `json:"web_session_id,omitempty"`
}

func (a EventAudit) empty() bool {
	return a == EventAudit{}
}

// EventPayload is the (sparse) union of fields carried by the various events.
// Only the fields relevant to Event.Kind are populated.
type EventPayload struct {
	// SessionReady
	HeadSHA string `json:"head_sha,omitempty"`

	// SessionStateChanged
	SessionState coordinator.SessionRuntimeState `json:"session_state,omitempty"`

	// PhaseDeadline: the phase whose dwell window elapsed. The guard's phase
	// check alone decides relevance, so no entered-version is carried.
	DeadlinePhase coordinator.Phase `json:"deadline_phase,omitempty"`

	// CheckReported
	Name        string                   `json:"name,omitempty"`
	CheckKind   coordinator.CheckKind    `json:"check_kind,omitempty"`
	Required    *bool                    `json:"required,omitempty"`
	Verdict     coordinator.CheckVerdict `json:"verdict,omitempty"`
	ExitCode    *int                     `json:"exit_code,omitempty"`
	Details     string                   `json:"details,omitempty"`
	SourceJobID *string                  `json:"source_job_id,omitempty"`
	Reporter    string                   `json:"reporter,omitempty"`

	// ScheduleIssue / SetIssueState / TriageIssue
	Schedule   coordinator.ScheduleState `json:"schedule,omitempty"`
	IssueState coordinator.IssueState    `json:"issue_state,omitempty"`
	Triage     coordinator.TriageState   `json:"triage,omitempty"`

	// Thread events
	ThreadKind     coordinator.ReviewClaimKind `json:"thread_kind,omitempty"`
	Body           string                      `json:"body,omitempty"`
	ClaimCommitSHA string                      `json:"claim_commit_sha,omitempty"`

	// AutoMerge retry bookkeeping: how many merge attempts this event
	// represents (0 for the original check-triggered attempt).
	AutoMergeAttempt int `json:"auto_merge_attempt,omitempty"`
}

// StepResult is what Engine.Step returns; handlers surface the populated fields
// in their HTTP responses. Only the fields a given event produces are set.
type StepResult struct {
	IssueID      string
	FromPhase    coordinator.Phase
	ToPhase      coordinator.Phase
	Transitioned bool

	Issue            *coordinator.Issue
	Session          *coordinator.Session
	Check            *coordinator.Check
	ReviewState      coordinator.ReviewState
	Thread           *coordinator.ReviewThread
	Merge            *coordinator.MergeResult
	FollowUpFailures []FollowUpFailure
}

type FollowUpFailure struct {
	EventKind EventKind `json:"event_kind"`
	Details   string    `json:"details"`
}
