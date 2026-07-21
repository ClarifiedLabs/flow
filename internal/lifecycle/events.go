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
	EventScheduleTask   EventKind = "schedule_task"
	EventSetTaskState   EventKind = "set_task_state"
	EventTriageTask     EventKind = "triage_task"
	EventMergeRequested EventKind = "merge_requested"
	EventMergeChange    EventKind = "merge_change"
	EventThreadClaimed  EventKind = "thread_claimed"
	EventThreadCertify  EventKind = "thread_certify"
	EventThreadReopen   EventKind = "thread_reopen"
	EventThreadComment  EventKind = "thread_comment"

	EventSessionStateChanged   EventKind = "session_state_changed"
	EventCloseTask             EventKind = "close_task"
	EventResetTask             EventKind = "reset_task"
	EventRetryCrashedAuthorJob EventKind = "retry_crashed_author_job"

	// Work-phase gate events: a human approves a gate-paused phase's handoff,
	// or sends the phase back to rework with feedback.
	EventWorkPhaseApproved EventKind = "work_phase_approved"
	EventWorkPhaseRework   EventKind = "work_phase_rework"

	// Deadline timers (durable, scheduled by the engine itself).
	EventPhaseDeadline EventKind = "phase_deadline"
	EventCheckTimeout  EventKind = "check_timeout"

	// Internal follow-on events emitted by actions during a Step cascade.
	EventEnsureFixAuthorJob EventKind = "ensure_fix_author_job"
	EventEnqueueAcceptance  EventKind = "enqueue_acceptance"
	EventAutoMerge          EventKind = "auto_merge"
	// EventEnsureWorkPhaseJob enqueues the author job for the task's current
	// work phase (freezing the flow cursor on first use).
	EventEnsureWorkPhaseJob EventKind = "ensure_work_phase_job"

	// EventReconcile records a ticker-driven phase refresh in the transition log
	// (e.g. after crash recovery moved an task out of an authoring phase). It is
	// applied directly, not dispatched through the transition table.
	EventReconcile EventKind = "reconcile"
)

// Event is the typed input to Engine.Step. The engine resolves TaskID for
// events keyed by change/thread/session before loading the snapshot.
type Event struct {
	Kind           EventKind
	TaskID         string
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
	TaskID       string `json:"task_id,omitempty"`
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

	// ScheduleTask / SetTaskState / TriageTask
	Schedule  coordinator.ScheduleState `json:"schedule,omitempty"`
	TaskState coordinator.TaskState     `json:"task_state,omitempty"`
	Triage    coordinator.TriageState   `json:"triage,omitempty"`

	// Thread events
	ThreadKind     coordinator.ReviewClaimKind `json:"thread_kind,omitempty"`
	Body           string                      `json:"body,omitempty"`
	ClaimCommitSHA string                      `json:"claim_commit_sha,omitempty"`

	// AutoMerge retry bookkeeping: how many merge attempts this event
	// represents (0 for the original check-triggered attempt).
	AutoMergeAttempt int `json:"auto_merge_attempt,omitempty"`

	// WorkPhaseRework: the human's request-changes feedback, injected into the
	// re-run phase's prompt.
	GateFeedback string `json:"gate_feedback,omitempty"`
}

// StepResult is what Engine.Step returns; handlers surface the populated fields
// in their HTTP responses. Only the fields a given event produces are set.
type StepResult struct {
	TaskID       string
	FromPhase    coordinator.Phase
	ToPhase      coordinator.Phase
	Transitioned bool

	Task             *coordinator.Task
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
