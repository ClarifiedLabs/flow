package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	flowmetrics "github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrWorkflowRunNotFound = errors.New("workflow run not found")
	ErrNoActiveWorkflowRun = errors.New("task has no active workflow run")
	ErrWorkflowConflict    = errors.New("workflow state conflict")

	errSnapshotRevisionChanged = errors.New("flow snapshot dependencies changed while scheduling")
)

type WorkflowRunState string

const (
	WorkflowRunScheduled WorkflowRunState = "scheduled"
	WorkflowRunRunning   WorkflowRunState = "running"
	WorkflowRunWaiting   WorkflowRunState = "waiting"
	WorkflowRunCompleted WorkflowRunState = "completed"
	WorkflowRunCancelled WorkflowRunState = "cancelled"
)

type WorkflowNodeRunState string

const (
	WorkflowNodeQueued    WorkflowNodeRunState = "queued"
	WorkflowNodeRunning   WorkflowNodeRunState = "running"
	WorkflowNodeWaiting   WorkflowNodeRunState = "waiting"
	WorkflowNodeSucceeded WorkflowNodeRunState = "succeeded"
	WorkflowNodeFailed    WorkflowNodeRunState = "failed"
	WorkflowNodeCancelled WorkflowNodeRunState = "cancelled"
)

type WorkflowWaitKind string

const (
	WorkflowWaitHumanGate            WorkflowWaitKind = "human_gate"
	WorkflowWaitAgentRequest         WorkflowWaitKind = "agent_request"
	WorkflowWaitOperatorIntervention WorkflowWaitKind = "operator_intervention"
	WorkflowWaitReviewScopeDecision  WorkflowWaitKind = "review_scope_decision"
)

type WorkflowWaitReason string

const (
	WorkflowWaitReasonExecutionFailed           WorkflowWaitReason = "execution_failed"
	WorkflowWaitReasonReviewCycleLimit          WorkflowWaitReason = "review_cycle_limit"
	WorkflowWaitReasonTransitionBudgetExhausted WorkflowWaitReason = "transition_budget_exhausted"
)

type WorkflowRun struct {
	ID                string           `json:"id"`
	TaskID            string           `json:"task_id"`
	RunSequence       int              `json:"run_sequence"`
	FlowID            string           `json:"flow_id,omitempty"`
	Snapshot          FlowSnapshot     `json:"snapshot"`
	State             WorkflowRunState `json:"state"`
	CurrentNodeKey    string           `json:"current_node_key,omitempty"`
	CurrentNodeRunID  string           `json:"current_node_run_id,omitempty"`
	CurrentArtifactID string           `json:"current_artifact_id,omitempty"`
	TransitionBudget  int              `json:"transition_budget"`
	TransitionsUsed   int              `json:"transitions_used"`
	ReviewCycleBudget int              `json:"review_cycle_budget"`
	ReviewCyclesUsed  int              `json:"review_cycles_used"`
	Version           int64            `json:"version"`
	CreatedAt         time.Time        `json:"created_at"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	CompletedAt       *time.Time       `json:"completed_at,omitempty"`
	CancelledAt       *time.Time       `json:"cancelled_at,omitempty"`
	CompletionSource  string           `json:"completion_source,omitempty"`
	HeldAt            *time.Time       `json:"held_at,omitempty"`
	HeldBy            string           `json:"held_by,omitempty"`
}

// Held reports whether an operator has taken the run: the executor will not
// advance it to the next node until the hold is released.
func (r WorkflowRun) Held() bool { return r.HeldAt != nil }

type WorkflowNodeRun struct {
	ID               string               `json:"id"`
	WorkflowRunID    string               `json:"workflow_run_id"`
	NodeKey          string               `json:"node_key"`
	Visit            int                  `json:"visit"`
	Attempt          int                  `json:"attempt"`
	State            WorkflowNodeRunState `json:"state"`
	InputArtifactID  string               `json:"input_artifact_id,omitempty"`
	OutputArtifactID string               `json:"output_artifact_id,omitempty"`
	Outcome          string               `json:"outcome,omitempty"`
	Error            string               `json:"error,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	StartedAt        *time.Time           `json:"started_at,omitempty"`
	CompletedAt      *time.Time           `json:"completed_at,omitempty"`

	// Resolved against the run's frozen snapshot and the jobs table when the
	// node run is read as part of a run detail. Not columns.
	Name string               `json:"name,omitempty"`
	Kind NodeKind             `json:"kind,omitempty"`
	Jobs []WorkflowNodeRunJob `json:"jobs,omitempty"`
}

// WorkflowNodeRunJob is one unit of work fanned out under a node run — the
// review agents on a change_review node, the check runner on a ci node. The run
// spine renders one row per job under the node it belongs to.
type WorkflowNodeRunJob struct {
	ID       string       `json:"id"`
	Role     string       `json:"role"`
	State    string       `json:"state"`
	Name     string       `json:"name,omitempty"`
	Verdict  CheckVerdict `json:"verdict,omitempty"`
	Details  string       `json:"details,omitempty"`
	WorkerID string       `json:"worker_id,omitempty"`
}

// WorkflowEdgeCount is how many times this run traversed one graph edge. The
// graph draws taken edges differently from possible-but-not-taken ones and
// annotates repeats with a visit count.
type WorkflowEdgeCount struct {
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
	Count   int    `json:"count"`
}

type WorkflowWait struct {
	ID            string             `json:"id"`
	WorkflowRunID string             `json:"workflow_run_id"`
	NodeRunID     string             `json:"node_run_id,omitempty"`
	Kind          WorkflowWaitKind   `json:"kind"`
	Reason        WorkflowWaitReason `json:"reason,omitempty"`
	Details       json.RawMessage    `json:"details,omitempty"`
	Message       string             `json:"message"`
	CreatedBy     Actor              `json:"created_by"`
	CreatedAt     time.Time          `json:"created_at"`
}

type WorkflowExecutionFailure struct {
	Operation string   `json:"operation"`
	NodeKind  NodeKind `json:"node_kind,omitempty"`
	Attempt   int      `json:"attempt,omitempty"`
	CheckID   int64    `json:"check_id,omitempty"`
	JobID     string   `json:"job_id,omitempty"`
	Message   string   `json:"message"`
}

type WorkflowAgentRuntimeSettings struct {
	Harness         string `json:"harness"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type WorkflowAgentRuntimeRefresh struct {
	AgentID string                       `json:"agent_id"`
	Old     WorkflowAgentRuntimeSettings `json:"old"`
	New     WorkflowAgentRuntimeSettings `json:"new"`
}

type WorkflowTransition struct {
	Sequence      int64           `json:"sequence"`
	TaskID        string          `json:"task_id"`
	WorkflowRunID string          `json:"workflow_run_id,omitempty"`
	FromTaskState string          `json:"from_task_state,omitempty"`
	ToTaskState   string          `json:"to_task_state,omitempty"`
	FromNodeKey   string          `json:"from_node_key,omitempty"`
	ToNodeKey     string          `json:"to_node_key,omitempty"`
	Outcome       string          `json:"outcome,omitempty"`
	EventKind     string          `json:"event_kind"`
	Payload       json.RawMessage `json:"payload"`
	Actor         string          `json:"actor,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type WorkflowRunDetail struct {
	Run                 WorkflowRun          `json:"run"`
	NodeRuns            []WorkflowNodeRun    `json:"node_runs"`
	OpenWait            *WorkflowWait        `json:"open_wait,omitempty"`
	Substate            InProgressSubstate   `json:"substate,omitempty"`
	Transitions         []WorkflowTransition `json:"transitions"`
	TransitionCounts    []WorkflowEdgeCount  `json:"transition_counts,omitempty"`
	ConvergenceEvidence *ConvergenceEvidence `json:"convergence_evidence,omitempty"`
	ActiveRulings       []OwnerRuling        `json:"active_rulings,omitempty"`
}

type WorkflowRunService struct {
	db                     *sql.DB
	flows                  *FlowService
	tasks                  *TaskService
	reviewAuthorCycleLimit int
	now                    func() time.Time
	metrics                *flowmetrics.Workflow

	// reviewLocksMu guards reviewLocks. Interactive review decisions on one
	// task are serialised end to end: a response's wait-id validation, its
	// marker transaction, and its terminal completions must not interleave
	// with a concurrent decision that resolves the validated wait and reopens
	// a fresh round on the same node run.
	reviewLocksMu sync.Mutex
	reviewLocks   map[string]*reviewLockEntry

	// eventLog receives post-commit lifecycle events (done/reopen/reset);
	// nil disables emission (wired through the project bundle).
	eventLog *EventLogService

	// reviewLockGate is a test seam: when set, RespondReview invokes it just
	// before taking the per-task review lock. Race tests use it to hold a
	// stale response at the pre-lock point while a concurrent decision
	// resolves the round and reopens a fresh wait.
	reviewLockGate func()

	// reviewLockAcquireGate is a test seam: when set, reviewLock invokes it
	// after the task's lock entry is looked up (and, with the reference-counted
	// cleanup, referenced) but before blocking on the entry mutex. The
	// lock-cleanup race test uses it to hold a queued acquirer at the exact
	// pre-block point while the holder's cleanup decides whether to drop the
	// entry.
	reviewLockAcquireGate func()

	// reviewLockCleanupGate is a test seam: when set, the reviewLock release
	// func invokes it after unlocking the entry mutex and before dropping the
	// entry reference. The lock-cleanup race test uses it to hold the cleanup
	// while a queued acquirer and a third acquirer arrive.
	reviewLockCleanupGate func()

	// reviewTerminalGate is a test seam: when set, RespondReview invokes it
	// after atomically advancing the reviewed agent to its derived gate but
	// before consuming that gate in the same transaction. Race tests use it to
	// prove a concurrent decision cannot observe the intermediate gate.
	reviewTerminalGate func()

	// Features gates task scheduling on a running feature rebase. It is wired
	// through the project bundle and stays nil in tests that construct the
	// service directly; scheduling then skips the rebase-block link.
	Features *FeatureService

	// scheduleSnapshotResolvedTestHook is a test seam invoked after snapshot
	// resolution and before the scheduling transaction. Concurrency tests use it
	// to update the selected flow in the former stale-snapshot window.
	scheduleSnapshotResolvedTestHook func(FlowSnapshot)

	// scheduleBeforeProjectLockTestHook is invoked immediately before scheduling
	// takes the project writer lock. Lock-order tests hold that lock and verify the
	// inherited catalog remains writable until scheduling acquires project first.
	scheduleBeforeProjectLockTestHook func()
}

func NewWorkflowRunService(db *sql.DB, flows *FlowService, tasks *TaskService) *WorkflowRunService {
	return NewWorkflowRunServiceWithOptions(db, flows, tasks, WorkflowRunServiceOptions{})
}

type WorkflowRunServiceOptions struct {
	ReviewAuthorCycleLimit int
	Metrics                *flowmetrics.Workflow
}

func NewWorkflowRunServiceWithOptions(db *sql.DB, flows *FlowService, tasks *TaskService, opts WorkflowRunServiceOptions) *WorkflowRunService {
	limit := opts.ReviewAuthorCycleLimit
	if limit <= 0 {
		limit = DefaultReviewAuthorCycleLimit
	}
	return &WorkflowRunService{
		db: db, flows: flows, tasks: tasks,
		reviewAuthorCycleLimit: limit,
		now:                    sqlitex.UTCNow,
		metrics:                opts.Metrics,
	}
}

// Schedule freezes the selected flow and creates a new run. The task remains
// Scheduled until its first node actually starts; unresolved effective
// work-item blockers suppress node creation but not scheduling.
func (s *WorkflowRunService) Schedule(ctx context.Context, taskID string) (WorkflowRun, error) {
	return s.ScheduleAs(ctx, taskID, ActorHuman)
}

func (s *WorkflowRunService) ScheduleAs(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return WorkflowRun{}, errors.New("task id is required")
	}
	if actor == "" {
		actor = ActorSystem
	}
	kind, err := workItemKindTx(ctx, s.db, taskID)
	if err != nil {
		if errors.Is(err, ErrWorkItemNotFound) {
			return WorkflowRun{}, sql.ErrNoRows
		}
		return WorkflowRun{}, err
	}
	if kind != WorkItemTask {
		return WorkflowRun{}, fmt.Errorf("%w: %s %s", ErrWorkItemNotSchedulable, kind, taskID)
	}
	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if task.State != nil {
		return WorkflowRun{}, fmt.Errorf("%w: task is already %s", ErrWorkflowConflict, *task.State)
	}
	// Gate before creating the run: a task whose feature is mid-rebase gets a
	// rebase_task blocks link first, so the dependency gate in
	// EnsureCurrentNode can never observe the new run ungated. The
	// rebase-start sweep covers the inverse race (rebase starts after this
	// schedule), and the rebase task itself is never self-blocked.
	if s.Features != nil {
		if err := s.Features.EnsureRebaseBlock(ctx, taskID); err != nil {
			return WorkflowRun{}, fmt.Errorf("gate task on running feature rebase: %w", err)
		}
	}
	for {
		snapshot, err := s.flows.ResolveSnapshot(ctx, task.FlowID)
		if err != nil {
			return WorkflowRun{}, err
		}
		snapshot, err = snapshotForTaskHumanReview(snapshot, task.RequiresHumanReview)
		if err != nil {
			return WorkflowRun{}, err
		}
		if len(snapshot.Nodes) == 0 || strings.TrimSpace(snapshot.StartNode) == "" {
			return WorkflowRun{}, errors.New("selected flow is not a graph workflow")
		}
		if s.scheduleSnapshotResolvedTestHook != nil {
			s.scheduleSnapshotResolvedTestHook(snapshot)
		}

		id, err := s.persistScheduledRun(ctx, task, snapshot, actor)
		if errors.Is(err, errSnapshotRevisionChanged) {
			continue
		}
		if err != nil {
			return WorkflowRun{}, err
		}
		return s.Get(ctx, id)
	}
}

func (s *WorkflowRunService) persistScheduledRun(ctx context.Context, task Task, snapshot FlowSnapshot, actor Actor) (string, error) {
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode flow snapshot: %w", err)
	}
	now := s.now().UTC()

	// Project mutations that resolve inherited definitions already lock project
	// before global. Scheduling follows the same order to avoid a cross-database
	// connection-pool deadlock while holding both revisions stable.
	if s.scheduleBeforeProjectLockTestHook != nil {
		s.scheduleBeforeProjectLockTestHook()
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	inheritedCatalogLock, current, err := s.flows.lockInheritedAgentDefsRevision(ctx, snapshot.InheritedAgentDefsRevision)
	if err != nil {
		return "", fmt.Errorf("lock inherited agent definitions for scheduling: %w", err)
	}
	if !current {
		return "", errSnapshotRevisionChanged
	}
	if inheritedCatalogLock != nil {
		defer inheritedCatalogLock.Rollback()
	}
	if err := claimTaskForScheduling(ctx, tx, task, snapshot, now); err != nil {
		return "", err
	}
	var sequence int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(run_sequence), 0) + 1 FROM workflow_runs WHERE task_id = ?`, task.ID).Scan(&sequence); err != nil {
		return "", err
	}
	id, err := randomPrefixedID("wr")
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_id, flow_snapshot_json, state,
	current_node_key, transition_budget, review_cycle_budget, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, task.ID, sequence,
		sqlitex.NullableNonEmptyString(snapshot.FlowID), string(snapshotJSON), string(WorkflowRunScheduled),
		snapshot.StartNode, snapshot.TransitionBudget, s.reviewAuthorCycleLimit, sqlitex.FormatTime(now)); err != nil {
		return "", fmt.Errorf("insert workflow run: %w", err)
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: task.ID, WorkflowRunID: id, ToTaskState: string(LifecycleScheduled),
		ToNodeKey: snapshot.StartNode, EventKind: "task_scheduled", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// claimTaskForScheduling atomically freezes the editable task settings and the
// project-owned revisions used to build the workflow snapshot. Once this write
// succeeds, the transaction's writer lock prevents the selected flow or local
// agent catalog from changing before the run is persisted. The inherited catalog
// lock is held by persistScheduledRun across this transaction.
type workflowSchedulingTx interface {
	agentDefQueryer
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func claimTaskForScheduling(ctx context.Context, tx workflowSchedulingTx, task Task, snapshot FlowSnapshot, now time.Time) error {
	if snapshot.FlowRevision < 1 || snapshot.AgentDefsRevision < 1 {
		return errors.New("resolved flow snapshot has incomplete revisions")
	}
	if task.FlowID != "" && task.FlowID != snapshot.FlowID {
		return fmt.Errorf("%w: task workflow settings changed while scheduling", ErrWorkflowConflict)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE tasks
SET lifecycle_state = ?, done_resolution = NULL, done_at = NULL, updated_at = ?
WHERE id = ?
  AND lifecycle_state IS NULL
  AND requires_human_review = ?
  AND COALESCE(flow_id, '') = ?
  AND EXISTS (SELECT 1 FROM flows WHERE id = ? AND revision = ?)
  AND EXISTS (
	SELECT 1 FROM app_metadata
	WHERE key = ? AND CAST(value AS INTEGER) = ?
  )
  AND (? <> '' OR EXISTS (
	SELECT 1 FROM app_metadata WHERE key = ? AND value = ?
  ))`,
		string(LifecycleScheduled), sqlitex.FormatTime(now), task.ID,
		task.RequiresHumanReview, task.FlowID, snapshot.FlowID, snapshot.FlowRevision,
		agentDefsRevisionMetadataKey, snapshot.AgentDefsRevision,
		task.FlowID, defaultFlowMetadataKey, snapshot.FlowID)
	if err != nil {
		return fmt.Errorf("claim task for scheduling: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count scheduled task claim: %w", err)
	}
	if updated == 1 {
		return nil
	}

	var state sql.NullString
	var requiresHumanReview bool
	var flowID string
	if err := tx.QueryRowContext(ctx, `
SELECT lifecycle_state, requires_human_review, COALESCE(flow_id, '')
FROM tasks WHERE id = ?`, task.ID).Scan(&state, &requiresHumanReview, &flowID); err != nil {
		return err
	}
	if state.Valid {
		return fmt.Errorf("%w: task is already %s", ErrWorkflowConflict, state.String)
	}
	if requiresHumanReview != task.RequiresHumanReview || flowID != task.FlowID {
		return fmt.Errorf("%w: task workflow settings changed while scheduling", ErrWorkflowConflict)
	}

	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM flows WHERE id = ?`, snapshot.FlowID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errSnapshotRevisionChanged
		}
		return err
	}
	if revision != snapshot.FlowRevision {
		return errSnapshotRevisionChanged
	}
	localAgentDefsRevision, err := agentDefCatalogRevision(ctx, tx)
	if err != nil {
		return err
	}
	if localAgentDefsRevision != snapshot.AgentDefsRevision {
		return errSnapshotRevisionChanged
	}
	if task.FlowID == "" {
		defaultID, err := defaultFlowIDWith(ctx, tx)
		if err != nil {
			return err
		}
		if defaultID != snapshot.FlowID {
			return errSnapshotRevisionChanged
		}
	}
	return fmt.Errorf("%w: task could not be claimed for scheduling", ErrWorkflowConflict)
}

func (s *WorkflowRunService) Get(ctx context.Context, runID string) (WorkflowRun, error) {
	return scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, runID))
}

func (s *WorkflowRunService) requireTaskWorkItem(ctx context.Context, itemID string) error {
	kind, err := workItemKindTx(ctx, s.db, itemID)
	if err != nil {
		if errors.Is(err, ErrWorkItemNotFound) {
			return sql.ErrNoRows
		}
		return err
	}
	if kind != WorkItemTask {
		return fmt.Errorf("%w: %s %s", ErrWorkItemNotSchedulable, kind, itemID)
	}
	return nil
}

func (s *WorkflowRunService) ActiveForTask(ctx context.Context, taskID string) (WorkflowRun, bool, error) {
	run, err := scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRun{}, false, nil
	}
	return run, err == nil, err
}

func (s *WorkflowRunService) ListForTask(ctx context.Context, taskID string) ([]WorkflowRun, error) {
	rows, err := s.db.QueryContext(ctx, workflowRunSelect+` WHERE task_id = ? ORDER BY run_sequence DESC`, taskID)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, scanWorkflowRun)
}

func (s *WorkflowRunService) ListTransitionsForTask(ctx context.Context, taskID string, limit int) ([]WorkflowTransition, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, task_id, workflow_run_id, from_task_state, to_task_state,
	from_node_key, to_node_key, outcome, event_kind, payload_json, actor, created_at
FROM workflow_transitions WHERE task_id = ? ORDER BY seq DESC LIMIT ?`, strings.TrimSpace(taskID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []WorkflowTransition
	for rows.Next() {
		var entry WorkflowTransition
		var runID sql.NullString
		var payload, createdAt string
		if err := rows.Scan(&entry.Sequence, &entry.TaskID, &runID, &entry.FromTaskState, &entry.ToTaskState,
			&entry.FromNodeKey, &entry.ToNodeKey, &entry.Outcome, &entry.EventKind, &payload, &entry.Actor, &createdAt); err != nil {
			return nil, err
		}
		entry.WorkflowRunID = runID.String
		entry.Payload = json.RawMessage(payload)
		entry.CreatedAt, err = sqlitex.ParseTime(createdAt)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *WorkflowRunService) Detail(ctx context.Context, runID string) (WorkflowRunDetail, error) {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	nodeRows, err := s.db.QueryContext(ctx, workflowNodeRunSelect+` WHERE workflow_run_id = ? ORDER BY created_at, id`, run.ID)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	nodes, err := scanRows(nodeRows, scanWorkflowNodeRun)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	// The run spine renders a node name once and its kind as a tag, so resolve
	// both here rather than making every reader re-join the frozen snapshot.
	for index := range nodes {
		if node, ok := run.Snapshot.Node(nodes[index].NodeKey); ok {
			nodes[index].Kind = node.Kind
			nodes[index].Name = strings.TrimSpace(node.Name)
		}
		if nodes[index].Name == "" {
			nodes[index].Name = workflowNodeKeyLabel(nodes[index].NodeKey)
		}
	}
	if err := s.attachNodeRunJobs(ctx, run, nodes); err != nil {
		return WorkflowRunDetail{}, err
	}
	detail := WorkflowRunDetail{Run: run, NodeRuns: nodes}
	if detail.TransitionCounts, err = s.transitionCounts(ctx, run.ID); err != nil {
		return WorkflowRunDetail{}, err
	}
	if wait, ok, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID)); err != nil {
		return WorkflowRunDetail{}, err
	} else if ok {
		if wait.Kind == WorkflowWaitHumanGate {
			if _, err := ParseReviewWaitDetails(wait.Details); err != nil {
				return WorkflowRunDetail{}, fmt.Errorf("decode human-gate wait %q: %w", wait.ID, err)
			}
		}
		if wait.Kind == WorkflowWaitReviewScopeDecision {
			if _, err := ParseReviewScopeDecisionWaitDetails(wait.Details); err != nil {
				return WorkflowRunDetail{}, fmt.Errorf("decode review-scope decision wait %q: %w", wait.ID, err)
			}
		}
		detail.OpenWait = &wait
		detail.Substate = InProgressBlocked
	} else if run.State == WorkflowRunRunning {
		detail.Substate = InProgressWorking
	}
	transitionRows, err := s.db.QueryContext(ctx, `
SELECT seq, task_id, workflow_run_id, from_task_state, to_task_state,
	from_node_key, to_node_key, outcome, event_kind, payload_json, actor, created_at
FROM workflow_transitions WHERE workflow_run_id = ? ORDER BY seq`, run.ID)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	defer transitionRows.Close()
	for transitionRows.Next() {
		var transition WorkflowTransition
		var workflowRunID sql.NullString
		var payload, createdAt string
		if err := transitionRows.Scan(&transition.Sequence, &transition.TaskID, &workflowRunID,
			&transition.FromTaskState, &transition.ToTaskState, &transition.FromNodeKey,
			&transition.ToNodeKey, &transition.Outcome, &transition.EventKind, &payload,
			&transition.Actor, &createdAt); err != nil {
			return WorkflowRunDetail{}, err
		}
		transition.WorkflowRunID = workflowRunID.String
		transition.Payload = json.RawMessage(payload)
		transition.CreatedAt, err = sqlitex.ParseTime(createdAt)
		if err != nil {
			return WorkflowRunDetail{}, err
		}
		detail.Transitions = append(detail.Transitions, transition)
	}
	if err := transitionRows.Err(); err != nil {
		return WorkflowRunDetail{}, err
	}
	detail.ActiveRulings, err = ProjectOwnerRulings(detail.Transitions)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	detail.ConvergenceEvidence, err = ActiveConvergenceEvidence(detail.Transitions)
	if err != nil {
		return WorkflowRunDetail{}, err
	}
	return detail, nil
}

// EnsureCurrentNode creates the run's current node visit once its initial
// dependency gate is clear. It is safe to call repeatedly.
func (s *WorkflowRunService) EnsureCurrentNode(ctx context.Context, runID string) (WorkflowNodeRun, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNodeRun{}, false, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, runID))
	if err != nil {
		return WorkflowNodeRun{}, false, err
	}
	if run.State != WorkflowRunScheduled && run.State != WorkflowRunRunning {
		return WorkflowNodeRun{}, false, nil
	}
	if run.CurrentNodeRunID != "" {
		nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		return nodeRun, false, err
	}
	if run.State == WorkflowRunScheduled {
		blocked, err := unresolvedBlockerCountTx(ctx, tx, run.TaskID)
		if err != nil {
			return WorkflowNodeRun{}, false, err
		}
		if blocked > 0 {
			return WorkflowNodeRun{}, false, nil
		}
	}
	node, ok := run.Snapshot.Node(run.CurrentNodeKey)
	if !ok {
		return WorkflowNodeRun{}, false, fmt.Errorf("snapshot node %q not found", run.CurrentNodeKey)
	}
	if node.Kind == NodeTerminal {
		now := s.now().UTC()
		if run.State == WorkflowRunScheduled {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET lifecycle_state = ?, updated_at = ? WHERE id = ?`,
				string(LifecycleInProgress), sqlitex.FormatTime(now), run.TaskID); err != nil {
				return WorkflowNodeRun{}, false, err
			}
			if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
				TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleScheduled),
				ToTaskState: string(LifecycleInProgress), ToNodeKey: node.Key,
				EventKind: "workflow_started", Actor: string(ActorSystem), CreatedAt: now,
			}); err != nil {
				return WorkflowNodeRun{}, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET started_at = COALESCE(started_at, ?) WHERE id = ?`,
				sqlitex.FormatTime(now), run.ID); err != nil {
				return WorkflowNodeRun{}, false, err
			}
		}
		terminalRun, err := createNodeRunTx(ctx, tx, run, node.Key, 1, run.CurrentArtifactID, now)
		if err != nil {
			return WorkflowNodeRun{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, started_at = ?, completed_at = ? WHERE id = ?`,
			string(WorkflowNodeSucceeded), sqlitex.FormatTime(now), sqlitex.FormatTime(now), terminalRun.ID); err != nil {
			return WorkflowNodeRun{}, false, err
		}
		if err := s.completeTerminalTx(ctx, tx, &run, node, "workflow", now); err != nil {
			return WorkflowNodeRun{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return WorkflowNodeRun{}, false, err
		}
		return WorkflowNodeRun{}, true, nil
	}
	nodeRun, err := createNodeRunTx(ctx, tx, run, node.Key, 1, run.CurrentArtifactID, s.now().UTC())
	if err != nil {
		return WorkflowNodeRun{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET current_node_run_id = ?, version = version + 1 WHERE id = ?`, nodeRun.ID, run.ID); err != nil {
		return WorkflowNodeRun{}, false, err
	}
	if node.Kind == NodeHumanGate {
		details, err := humanGateWaitDetails(node, run.CurrentArtifactID, false)
		if err != nil {
			return WorkflowNodeRun{}, false, err
		}
		if err := enterWaitWithDetailsTx(ctx, tx, &run, &nodeRun, WorkflowWaitHumanGate, details, humanGateWaitMessage(node), ActorSystem, s.now().UTC()); err != nil {
			return WorkflowNodeRun{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNodeRun{}, false, err
	}
	return s.GetNodeRun(ctx, nodeRun.ID)
}

func (s *WorkflowRunService) MarkNodeRunning(ctx context.Context, nodeRunID string) (WorkflowNodeRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	defer tx.Rollback()
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	if nodeRun.State == WorkflowNodeRunning {
		return nodeRun, nil
	}
	if nodeRun.State != WorkflowNodeQueued {
		return WorkflowNodeRun{}, fmt.Errorf("%w: node run is %s", ErrWorkflowConflict, nodeRun.State)
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = ?, started_at = ? WHERE id = ?`,
		string(WorkflowNodeRunning), sqlitex.FormatTime(now), nodeRun.ID); err != nil {
		return WorkflowNodeRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, started_at = COALESCE(started_at, ?), version = version + 1
WHERE id = ?`, string(WorkflowRunRunning), sqlitex.FormatTime(now), nodeRun.WorkflowRunID); err != nil {
		return WorkflowNodeRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, updated_at = ?
WHERE id = (SELECT task_id FROM workflow_runs WHERE id = ?)`,
		string(LifecycleInProgress), sqlitex.FormatTime(now), nodeRun.WorkflowRunID); err != nil {
		return WorkflowNodeRun{}, err
	}
	if run.State == WorkflowRunScheduled {
		if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
			TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleScheduled),
			ToTaskState: string(LifecycleInProgress), ToNodeKey: nodeRun.NodeKey,
			EventKind: "workflow_started", Actor: string(ActorSystem), CreatedAt: now,
		}); err != nil {
			return WorkflowNodeRun{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNodeRun{}, err
	}
	updated, found, err := s.GetNodeRun(ctx, nodeRun.ID)
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	if !found {
		return WorkflowNodeRun{}, ErrWorkflowRunNotFound
	}
	return updated, nil
}

// PauseForExecutionError durably parks an active workflow without selecting an
// outcome edge. It is idempotent so concurrent executor advances cannot create
// duplicate operator waits.
func (s *WorkflowRunService) PauseForExecutionError(ctx context.Context, runID, nodeRunID string, failure WorkflowExecutionFailure) error {
	runID = strings.TrimSpace(runID)
	nodeRunID = strings.TrimSpace(nodeRunID)
	failure.Operation = strings.TrimSpace(failure.Operation)
	failure.Message = strings.TrimSpace(failure.Message)
	if runID == "" {
		return errors.New("execution failure requires a workflow run id")
	}
	if failure.Operation == "" {
		failure.Operation = "execute workflow node"
	}
	if failure.Message == "" {
		failure.Message = "workflow phase failed without an error message"
	}
	const maxFailureMessage = 8 * 1024
	if len(failure.Message) > maxFailureMessage {
		failure.Message = failure.Message[:maxFailureMessage]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, runID))
	if err != nil {
		return err
	}
	if run.State == WorkflowRunWaiting {
		wait, ok, err := openWaitTx(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if ok &&
			wait.Kind == WorkflowWaitOperatorIntervention &&
			wait.Reason == WorkflowWaitReasonExecutionFailed &&
			wait.NodeRunID == nodeRunID {
			return nil
		}
		return fmt.Errorf("%w: workflow is already waiting for another reason", ErrWorkflowConflict)
	}
	if run.State != WorkflowRunScheduled && run.State != WorkflowRunRunning {
		return fmt.Errorf("%w: workflow run is %s", ErrWorkflowConflict, run.State)
	}
	if nodeRunID != "" && run.CurrentNodeRunID != nodeRunID {
		return fmt.Errorf("%w: workflow node is no longer current", ErrWorkflowConflict)
	}

	var nodeRun WorkflowNodeRun
	if nodeRunID != "" {
		nodeRun, err = scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
		if err != nil {
			return err
		}
		if nodeRun.WorkflowRunID != run.ID {
			return fmt.Errorf("%w: node does not belong to workflow run", ErrWorkflowConflict)
		}
		if nodeRun.State != WorkflowNodeQueued && nodeRun.State != WorkflowNodeRunning && nodeRun.State != WorkflowNodeWaiting {
			return fmt.Errorf("%w: workflow node is %s", ErrWorkflowConflict, nodeRun.State)
		}
		if failure.NodeKind == "" {
			if node, ok := run.Snapshot.Node(nodeRun.NodeKey); ok {
				failure.NodeKind = node.Kind
			}
		}
		if failure.Attempt == 0 {
			failure.Attempt = nodeRun.Attempt
		}
	}

	now := s.now().UTC()
	if nodeRunID != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs
SET state = ?, error = ?, completed_at = NULL
WHERE id = ?`, string(WorkflowNodeWaiting), failure.Message, nodeRunID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs
SET state = ?, started_at = COALESCE(started_at, ?), version = version + 1
WHERE id = ?`, string(WorkflowRunWaiting), sqlitex.FormatTime(now), run.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleInProgress), sqlitex.FormatTime(now), run.TaskID); err != nil {
		return err
	}
	payload, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("encode workflow execution failure: %w", err)
	}
	message := fmt.Sprintf("%s: %s", failure.Operation, failure.Message)
	if err := insertWaitWithReasonTx(
		ctx,
		tx,
		run.ID,
		nodeRunID,
		WorkflowWaitOperatorIntervention,
		WorkflowWaitReasonExecutionFailed,
		failure,
		message,
		ActorSystem,
		now,
	); err != nil {
		return err
	}
	fromTaskState := LifecycleInProgress
	if run.State == WorkflowRunScheduled {
		fromTaskState = LifecycleScheduled
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID,
		FromTaskState: string(fromTaskState), ToTaskState: string(LifecycleInProgress),
		FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
		EventKind: "node_execution_failed", PayloadJSON: string(payload),
		Actor: string(ActorSystem), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryExecution resolves an execution-failure wait and reruns only the current
// node attempt. For check nodes, only errored checks are reset; completed
// results for the same pinned change revision remain authoritative. When
// refreshAgentRuntime is true, the current node's frozen agents keep their
// identity and prompts while adopting the latest effective runtime settings.
func (s *WorkflowRunService) RetryExecution(ctx context.Context, taskID string, actor Actor, refreshAgentRuntime bool) (WorkflowRun, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return WorkflowRun{}, errors.New("task id is required")
	}
	if actor == "" {
		actor = ActorHuman
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state = ?`, taskID, string(WorkflowRunWaiting)))
	if err != nil {
		return WorkflowRun{}, err
	}
	wait, ok, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if !ok || wait.Kind != WorkflowWaitOperatorIntervention || wait.Reason != WorkflowWaitReasonExecutionFailed {
		return WorkflowRun{}, fmt.Errorf("%w: workflow is not waiting on an execution failure", ErrWorkflowConflict)
	}

	var nodeRun WorkflowNodeRun
	if run.CurrentNodeRunID != "" {
		nodeRun, err = scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if err != nil {
			return WorkflowRun{}, err
		}
		if nodeRun.State != WorkflowNodeWaiting &&
			nodeRun.State != WorkflowNodeQueued &&
			nodeRun.State != WorkflowNodeRunning {
			return WorkflowRun{}, fmt.Errorf("%w: workflow node is %s", ErrWorkflowConflict, nodeRun.State)
		}
		node, ok := run.Snapshot.Node(nodeRun.NodeKey)
		if !ok {
			return WorkflowRun{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
		}
		if requiresPinnedChangeHead(node.Kind) {
			if err := verifyPinnedChangeHeadTx(ctx, tx, run.CurrentArtifactID); err != nil {
				return WorkflowRun{}, err
			}
		}
	}

	var refreshedAgents []WorkflowAgentRuntimeRefresh
	if refreshAgentRuntime {
		nodeKey := nodeRun.NodeKey
		if nodeKey == "" {
			nodeKey = run.CurrentNodeKey
		}
		refreshedAgents, err = s.refreshNodeAgentRuntime(ctx, tx, &run.Snapshot, nodeKey)
		if err != nil {
			return WorkflowRun{}, err
		}
	}

	now := s.now().UTC()
	resetChecks := int64(0)
	if nodeRun.ID != "" {
		result, err := tx.ExecContext(ctx, `
UPDATE checks
SET verdict = ?, exit_code = NULL, details = '', source_job_id = NULL,
	reporter = 'coordinator', updated_at = ?
WHERE task_id = ?
	AND name LIKE ?
	AND verdict = ?`,
			string(CheckPending),
			sqlitex.FormatTime(now),
			run.TaskID,
			"%.node."+nodeRun.ID,
			string(CheckErrored),
		)
		if err != nil {
			return WorkflowRun{}, err
		}
		resetChecks, err = result.RowsAffected()
		if err != nil {
			return WorkflowRun{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs
SET attempt = attempt + 1, state = ?, error = '', started_at = NULL, completed_at = NULL
WHERE id = ?`, string(WorkflowNodeQueued), nodeRun.ID); err != nil {
			return WorkflowRun{}, err
		}
		nodeRun.Attempt++
	}
	if refreshAgentRuntime {
		snapshotJSON, err := json.Marshal(run.Snapshot)
		if err != nil {
			return WorkflowRun{}, fmt.Errorf("encode refreshed workflow snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET flow_snapshot_json = ?, state = ?, version = version + 1 WHERE id = ?`,
			string(snapshotJSON), string(WorkflowRunRunning), run.ID); err != nil {
			return WorkflowRun{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`,
		string(WorkflowRunRunning), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return WorkflowRun{}, err
	}
	payloadFields := map[string]any{
		"attempt":      nodeRun.Attempt,
		"checks_reset": resetChecks,
	}
	if refreshAgentRuntime {
		payloadFields["refreshed_agents"] = refreshedAgents
	}
	payload, err := json.Marshal(payloadFields)
	if err != nil {
		return WorkflowRun{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID,
		FromTaskState: string(LifecycleInProgress), ToTaskState: string(LifecycleInProgress),
		FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
		EventKind: "node_retry_requested", PayloadJSON: string(payload),
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

func (s *WorkflowRunService) refreshNodeAgentRuntime(
	ctx context.Context,
	tx *sql.Tx,
	snapshot *FlowSnapshot,
	nodeKey string,
) ([]WorkflowAgentRuntimeRefresh, error) {
	if s.flows == nil || s.flows.agentDefs == nil {
		return nil, errors.New("workflow agent definitions are unavailable")
	}
	var node *FlowNodeSnapshot
	for i := range snapshot.Nodes {
		if snapshot.Nodes[i].Key == nodeKey {
			node = &snapshot.Nodes[i]
			break
		}
	}
	if node == nil {
		return nil, fmt.Errorf("snapshot node %q not found", nodeKey)
	}

	var agents []*AgentDefSnapshot
	switch node.Kind {
	case NodeAgent:
		if node.Config.Agent == nil {
			return nil, fmt.Errorf("snapshot agent node %q is missing configuration", nodeKey)
		}
		agents = append(agents, &node.Config.Agent.Agent)
	case NodeChangeReview:
		if node.Config.ChangeReview == nil {
			return nil, fmt.Errorf("snapshot review node %q is missing configuration", nodeKey)
		}
		for i := range node.Config.ChangeReview.Agents {
			agents = append(agents, &node.Config.ChangeReview.Agents[i].Agent)
		}
		agents = append(agents, &node.Config.ChangeReview.Aggregator)
	case NodeVerifyChange:
		if node.Config.VerifyChange == nil {
			return nil, fmt.Errorf("snapshot verification node %q is missing configuration", nodeKey)
		}
		for i := range node.Config.VerifyChange.Agents {
			agents = append(agents, &node.Config.VerifyChange.Agents[i].Agent)
		}
	}

	refreshed := make([]WorkflowAgentRuntimeRefresh, 0, len(agents))
	for _, agent := range agents {
		agentID := strings.TrimSpace(agent.ID)
		if agentID == "" {
			return nil, fmt.Errorf("snapshot agent %q in node %q has no id", agent.Name, nodeKey)
		}
		definition, err := s.flows.agentDefs.resolveWith(ctx, tx, agentID)
		if err != nil {
			return nil, fmt.Errorf("resolve snapshot agent %q in node %q: %w", agentID, nodeKey, err)
		}
		oldRuntime := workflowAgentRuntimeSettings(*agent)
		newRuntime := WorkflowAgentRuntimeSettings{
			Harness:         definition.Harness,
			Model:           definition.Model,
			ReasoningEffort: definition.ReasoningEffort,
		}
		agent.Harness = newRuntime.Harness
		agent.Model = newRuntime.Model
		agent.ReasoningEffort = newRuntime.ReasoningEffort
		refreshed = append(refreshed, WorkflowAgentRuntimeRefresh{
			AgentID: agentID,
			Old:     oldRuntime,
			New:     newRuntime,
		})
	}
	return refreshed, nil
}

func workflowAgentRuntimeSettings(agent AgentDefSnapshot) WorkflowAgentRuntimeSettings {
	return WorkflowAgentRuntimeSettings{
		Harness:         agent.Harness,
		Model:           agent.Model,
		ReasoningEffort: agent.ReasoningEffort,
	}
}

func requiresPinnedChangeHead(kind NodeKind) bool {
	switch kind {
	case NodeAutomatedChecks, NodeChangeReview, NodeVerifyChange:
		return true
	default:
		return false
	}
}

func verifyPinnedChangeHeadTx(ctx context.Context, tx workflowTx, artifactID string) error {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil
	}
	var kind string
	var payloadJSON sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT kind, payload_json FROM workflow_artifacts WHERE id = ?`, artifactID).Scan(&kind, &payloadJSON); err != nil {
		return err
	}
	if ArtifactKind(kind) != ArtifactChange {
		return nil
	}
	var payload struct {
		ChangeID string `json:"change_id"`
		HeadSHA  string `json:"head_sha"`
	}
	if err := json.Unmarshal([]byte(payloadJSON.String), &payload); err != nil {
		return fmt.Errorf("decode pinned change artifact: %w", err)
	}
	var currentHead string
	if err := tx.QueryRowContext(ctx, `SELECT head_sha FROM changes WHERE id = ?`, strings.TrimSpace(payload.ChangeID)).Scan(&currentHead); err != nil {
		return err
	}
	if strings.TrimSpace(currentHead) != strings.TrimSpace(payload.HeadSHA) {
		return fmt.Errorf(
			"%w: change head moved from %s to %s; reset or start a new workflow run",
			ErrWorkflowConflict,
			strings.TrimSpace(payload.HeadSHA),
			strings.TrimSpace(currentHead),
		)
	}
	return nil
}

func (s *WorkflowRunService) GetNodeRun(ctx context.Context, nodeRunID string) (WorkflowNodeRun, bool, error) {
	nodeRun, err := scanWorkflowNodeRun(s.db.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowNodeRun{}, false, nil
	}
	return nodeRun, err == nil, err
}

type CompleteWorkflowNodeInput struct {
	NodeRunID      string
	Outcome        string
	ArtifactID     string
	Actor          Actor
	Payload        map[string]any
	IdempotencyKey string
	SkipTaskID     string
	// OperatorSatisfied waives the agent-node artifact contract: the operator
	// has done the work by hand and is asserting the node is done. Set only by
	// the ReleaseSatisfy hand-back.
	OperatorSatisfied bool

	// humanGateWaitID is an internal capability issued only after the caller has
	// transactionally bound a human-gate response to its exact persisted wait and
	// node run. Public completion paths cannot manufacture it.
	humanGateWaitID string
}

type CompleteWorkflowNodeResult struct {
	Run      WorkflowRun      `json:"run"`
	Next     *WorkflowNodeRun `json:"next,omitempty"`
	Done     bool             `json:"done"`
	Replayed bool             `json:"replayed,omitempty"`
}

func isAutomatedReviewAuthorCycle(source, target FlowNodeSnapshot, outcome string) bool {
	if outcome != "changes_requested" {
		return false
	}
	if source.Kind != NodeChangeReview && source.Kind != NodeVerifyChange {
		return false
	}
	return target.Kind == NodeAgent &&
		target.Config.Agent != nil &&
		target.Config.Agent.Workspace == WorkspaceChange
}

func (s *WorkflowRunService) CompleteNode(ctx context.Context, input CompleteWorkflowNodeInput) (CompleteWorkflowNodeResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()
	return s.completeNodeTx(ctx, tx, input, true, tx.Commit)
}

// completeNodeTx runs the node-completion state machine against tx. When
// selfCommit is true it commits tx at the state machine's natural completion
// points and reloads the run (and next node run) before returning, exactly as
// CompleteNode always has; commit is the function that commits tx. When
// selfCommit is false it leaves tx open for the caller to commit and returns
// the in-memory projection of the writes instead of reloading — the atomic
// review submission completes its human gate this way, inside the same
// transaction that validated the inspected head and recorded the verdict, and
// commits that transaction itself.
func (s *WorkflowRunService) completeNodeTx(ctx context.Context, tx workflowTx, input CompleteWorkflowNodeInput, selfCommit bool, commit func() error) (CompleteWorkflowNodeResult, error) {
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.Outcome = strings.TrimSpace(input.Outcome)
	if input.Actor == "" {
		input.Actor = ActorSystem
	}
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, input.NodeRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	sourceNode, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
	}
	skipping := strings.TrimSpace(input.SkipTaskID) != ""
	if skipping {
		if run.TaskID != strings.TrimSpace(input.SkipTaskID) {
			return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
		}
		if input.IdempotencyKey != "skip:"+nodeRun.ID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: skip idempotency key does not match the node run", ErrWorkflowConflict)
		}
		skipOutcome, allowed := workflowSkipOutcome(sourceNode.Kind)
		if !allowed || input.Outcome != skipOutcome {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: workflow node kind %q cannot be skipped", ErrWorkflowConflict, sourceNode.Kind)
		}
	}
	if nodeRun.State == WorkflowNodeSucceeded {
		if nodeRun.Outcome != input.Outcome || (strings.TrimSpace(input.ArtifactID) != "" && strings.TrimSpace(nodeRun.OutputArtifactID) != strings.TrimSpace(input.ArtifactID)) {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: completed node replay does not match the recorded outcome and artifact", ErrWorkflowConflict)
		}
		if skipping {
			var recorded int
			if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'node_skipped' AND idempotency_key = ?`,
				run.ID, input.IdempotencyKey).Scan(&recorded); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			if recorded == 0 {
				return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: failed workflow step is no longer active", ErrWorkflowConflict)
			}
		}
		// Same-outcome replay of a decision another call already committed: the
		// snapshot scanned above was taken inside this transaction, so for a
		// retry that raced the original commit it can still show the run
		// waiting even though the committed decision completed it. End the
		// transaction and reload the committed run so Result.Run and Done
		// describe the state the caller observes (a terminal verdict must
		// report Done even though this call wrote nothing).
		if selfCommit {
			if err := commit(); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			latest, err := s.Get(ctx, run.ID)
			if err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			return CompleteWorkflowNodeResult{Run: latest, Done: latest.State == WorkflowRunCompleted, Replayed: true}, nil
		}
		// Non-self-committing callers (the atomic review-submission path) own
		// the transaction and cannot reload it here; Result.Run is the
		// transaction's snapshot of the committed decision.
		return CompleteWorkflowNodeResult{Run: run, Done: run.State == WorkflowRunCompleted, Replayed: true}, nil
	}
	if nodeRun.State != WorkflowNodeRunning && nodeRun.State != WorkflowNodeWaiting && nodeRun.State != WorkflowNodeQueued {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node run is %s", ErrWorkflowConflict, nodeRun.State)
	}
	if run.CurrentNodeRunID != nodeRun.ID {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node run is not active", ErrWorkflowConflict)
	}
	// A human-gate wait is a review round, not a generic node pause. Only the
	// internal response paths below may complete its node, and they must carry the
	// exact wait identity they validated in this same transaction. In particular,
	// an agent parked on an interactive review cannot bypass it through /complete
	// or a release hand-back. A completed-node replay returned above is harmless:
	// it writes nothing and must retain normal idempotency semantics.
	wait, waiting, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if sourceNode.Kind == NodeHumanGate || (waiting && wait.Kind == WorkflowWaitHumanGate) {
		if !waiting || wait.Kind != WorkflowWaitHumanGate ||
			strings.TrimSpace(input.humanGateWaitID) == "" || input.humanGateWaitID != wait.ID || wait.NodeRunID != nodeRun.ID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: human-gate completion requires its exact review_wait_id and node_run_id", ErrWorkflowConflict)
		}
	}
	// ReleaseSatisfy is an operator hand-back for non-human nodes. A missing wait
	// on a waiting run is corrupt persisted state and must fail closed rather than
	// let the hand-back consume an unknown review round.
	if input.OperatorSatisfied && !waiting && run.State == WorkflowRunWaiting {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: operator satisfaction cannot resolve a workflow waiting without its persisted wait", ErrWorkflowConflict)
	}
	if skipping {
		wait, waiting, err := openWaitTx(ctx, tx, run.ID)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if run.State != WorkflowRunWaiting || !waiting ||
			wait.Kind != WorkflowWaitOperatorIntervention ||
			wait.Reason != WorkflowWaitReasonExecutionFailed ||
			wait.NodeRunID != nodeRun.ID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: workflow is not waiting on this failed step", ErrWorkflowConflict)
		}
		if requiresPinnedChangeHead(sourceNode.Kind) {
			if err := verifyPinnedChangeHeadTx(ctx, tx, run.CurrentArtifactID); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
		}
	}
	target, ok := run.Snapshot.Target(nodeRun.NodeKey, input.Outcome)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("node %q has no transition for outcome %q", nodeRun.NodeKey, input.Outcome)
	}
	targetNode, ok := run.Snapshot.Node(target)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("target node %q not found", target)
	}
	now := s.now().UTC()
	reviewCycle := isAutomatedReviewAuthorCycle(sourceNode, targetNode, input.Outcome)
	if reviewCycle && run.ReviewCyclesUsed >= run.ReviewCycleBudget {
		pendingIdempotencyKey := strings.TrimSpace(input.IdempotencyKey)
		if pendingIdempotencyKey == "" {
			pendingIdempotencyKey = fmt.Sprintf("review-cycle-resume:%s:%d:%s", nodeRun.ID, nodeRun.Attempt, input.Outcome)
		}
		if run.State == WorkflowRunWaiting && nodeRun.State == WorkflowNodeWaiting {
			wait, waiting, err := openWaitTx(ctx, tx, run.ID)
			if err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			if waiting &&
				wait.Kind == WorkflowWaitOperatorIntervention &&
				wait.Reason == WorkflowWaitReasonReviewCycleLimit &&
				wait.NodeRunID == nodeRun.ID {
				return CompleteWorkflowNodeResult{Run: run, Replayed: true}, nil
			}
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ? WHERE id = ?`,
			string(WorkflowNodeWaiting), nodeRun.ID); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`,
			string(WorkflowRunWaiting), run.ID); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if err := insertWaitWithReasonTx(
			ctx,
			tx,
			run.ID,
			nodeRun.ID,
			WorkflowWaitOperatorIntervention,
			WorkflowWaitReasonReviewCycleLimit,
			map[string]any{
				"review_cycle_budget":     run.ReviewCycleBudget,
				"review_cycles_used":      run.ReviewCyclesUsed,
				"pending_outcome":         input.Outcome,
				"pending_idempotency_key": pendingIdempotencyKey,
				"pending_payload":         input.Payload,
			},
			fmt.Sprintf(
				"Review-author cycle limit reached after %d automated send-backs",
				run.ReviewCyclesUsed,
			),
			ActorSystem,
			now,
		); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		run.State = WorkflowRunWaiting
		if selfCommit {
			if err := commit(); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			waiting, err := s.Get(ctx, run.ID)
			return CompleteWorkflowNodeResult{Run: waiting}, err
		}
		return CompleteWorkflowNodeResult{Run: run}, nil
	}
	if skipping {
		retiredChecks, cancelledJobs, err := retireSkippedWorkflowNodeTx(ctx, tx, run.TaskID, run.ID, nodeRun.ID, sourceNode.Kind, now)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		payload := make(map[string]any, len(input.Payload)+3)
		for key, value := range input.Payload {
			payload[key] = value
		}
		payload["node_run_id"] = nodeRun.ID
		payload["retired_checks"] = retiredChecks
		payload["cancelled_jobs"] = cancelledJobs
		input.Payload = payload
	}
	artifactID := strings.TrimSpace(input.ArtifactID)
	if sourceNode.Kind == NodeAgent && !skipping && !input.OperatorSatisfied {
		if artifactID == "" {
			return CompleteWorkflowNodeResult{}, errors.New("agent node completion requires an artifact")
		}
		if sourceNode.Config.Agent == nil {
			return CompleteWorkflowNodeResult{}, errors.New("agent node has no configuration")
		}
		var artifactRunID, artifactNodeRunID, artifactKind string
		if err := tx.QueryRowContext(ctx, `
SELECT workflow_run_id, node_run_id, kind FROM workflow_artifacts WHERE id = ?`, artifactID).
			Scan(&artifactRunID, &artifactNodeRunID, &artifactKind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return CompleteWorkflowNodeResult{}, ErrWorkflowArtifactNotFound
			}
			return CompleteWorkflowNodeResult{}, err
		}
		if artifactRunID != run.ID || artifactNodeRunID != nodeRun.ID || ArtifactKind(artifactKind) != sourceNode.Config.Agent.Artifact {
			return CompleteWorkflowNodeResult{}, errors.New("artifact does not satisfy the active agent node contract")
		}
	} else if artifactID != "" && artifactID != run.CurrentArtifactID {
		return CompleteWorkflowNodeResult{}, errors.New("non-agent nodes cannot replace the current artifact")
	}
	if artifactID == "" {
		artifactID = run.CurrentArtifactID
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, output_artifact_id = ?, outcome = ?, completed_at = ?
WHERE id = ?`, string(WorkflowNodeSucceeded), sqlitex.NullableNonEmptyString(artifactID), input.Outcome,
		sqlitex.FormatTime(now), nodeRun.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, input.Actor, now); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	payloadJSON, err := json.Marshal(input.Payload)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	eventKind := "node_completed"
	if skipping {
		eventKind = "node_skipped"
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: target,
		Outcome: input.Outcome, EventKind: eventKind, PayloadJSON: string(payloadJSON),
		Actor: string(input.Actor), IdempotencyKey: input.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	used := run.TransitionsUsed + 1
	reviewCyclesUsed := run.ReviewCyclesUsed
	if reviewCycle {
		reviewCyclesUsed++
	}
	if targetNode.Kind == NodeTerminal {
		// The terminal edge is not exclusive to the public CompleteNode: the
		// review marker transaction also completes the gate through this state
		// machine (SubmitReview's respondToReviewGateTx), and when the gate's
		// selected outcome edges to a terminal node this branch runs inside that
		// same transaction via completeTerminalTx. That is safe because the
		// terminal completion commits atomically with the marker, the run is
		// reloaded after commit, and the executor's Advance guards
		// (waiting/completed/held/gate/terminal) prevent any double-advance.
		terminalRun, err := createNodeRunTx(ctx, tx, run, targetNode.Key, 1, artifactID, now)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, started_at = ?, completed_at = ? WHERE id = ?`,
			string(WorkflowNodeSucceeded), sqlitex.FormatTime(now), sqlitex.FormatTime(now), terminalRun.ID); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		run.CurrentArtifactID = artifactID
		run.TransitionsUsed = used
		if err := s.completeTerminalTx(ctx, tx, &run, targetNode, "workflow", now); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		run.State = WorkflowRunCompleted
		run.CurrentNodeKey = target
		run.CurrentNodeRunID = ""
		if selfCommit {
			if err := commit(); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			completed, err := s.Get(ctx, run.ID)
			return CompleteWorkflowNodeResult{Run: completed, Done: true}, err
		}
		return CompleteWorkflowNodeResult{Run: run, Done: true}, nil
	}

	if used >= run.TransitionBudget {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, current_node_key = ?, current_node_run_id = NULL,
	current_artifact_id = ?, transitions_used = ?, review_cycles_used = ?, version = version + 1
WHERE id = ?`, string(WorkflowRunWaiting), target, sqlitex.NullableNonEmptyString(artifactID),
			used, reviewCyclesUsed, run.ID); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if err := insertWaitWithReasonTx(
			ctx,
			tx,
			run.ID,
			"",
			WorkflowWaitOperatorIntervention,
			WorkflowWaitReasonTransitionBudgetExhausted,
			map[string]any{"transition_budget": run.TransitionBudget, "transitions_used": used},
			"Workflow transition budget exhausted",
			ActorSystem,
			now,
		); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		run.State = WorkflowRunWaiting
		run.CurrentNodeKey = target
		run.CurrentNodeRunID = ""
		run.CurrentArtifactID = artifactID
		run.TransitionsUsed = used
		run.ReviewCyclesUsed = reviewCyclesUsed
		if selfCommit {
			if err := commit(); err != nil {
				return CompleteWorkflowNodeResult{}, err
			}
			waiting, err := s.Get(ctx, run.ID)
			return CompleteWorkflowNodeResult{Run: waiting}, err
		}
		return CompleteWorkflowNodeResult{Run: run}, nil
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, current_node_key = ?, current_node_run_id = NULL,
	current_artifact_id = ?, transitions_used = ?, review_cycles_used = ?, version = version + 1
WHERE id = ?`, string(WorkflowRunRunning), target, sqlitex.NullableNonEmptyString(artifactID),
		used, reviewCyclesUsed, run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run.State = WorkflowRunRunning
	run.CurrentNodeKey = target
	run.CurrentNodeRunID = ""
	run.CurrentArtifactID = artifactID
	run.TransitionsUsed = used
	run.ReviewCyclesUsed = reviewCyclesUsed
	next, err := createNodeRunTx(ctx, tx, run, target, 1, artifactID, now)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET current_node_run_id = ? WHERE id = ?`, next.ID, run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if targetNode.Kind == NodeHumanGate {
		details, err := humanGateWaitDetails(targetNode, artifactID, false)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if err := enterWaitWithDetailsTx(ctx, tx, &run, &next, WorkflowWaitHumanGate, details, humanGateWaitMessage(targetNode), ActorSystem, now); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
	}
	run.CurrentNodeRunID = next.ID
	if selfCommit {
		if err := commit(); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		updated, err := s.Get(ctx, run.ID)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		nextLoaded, _, err := s.GetNodeRun(ctx, next.ID)
		return CompleteWorkflowNodeResult{Run: updated, Next: &nextLoaded}, err
	}
	return CompleteWorkflowNodeResult{Run: run, Next: &next}, nil
}

// SkipExecution resolves a specific execution-failure wait by waiving a failed
// check barrier. The expected node-run identity prevents a stale browser action
// from skipping a later failure after the workflow has advanced.
func (s *WorkflowRunService) SkipExecution(ctx context.Context, taskID, expectedNodeRunID string, actor Actor) (CompleteWorkflowNodeResult, error) {
	taskID = strings.TrimSpace(taskID)
	expectedNodeRunID = strings.TrimSpace(expectedNodeRunID)
	if taskID == "" {
		return CompleteWorkflowNodeResult{}, errors.New("task id is required")
	}
	if expectedNodeRunID == "" {
		return CompleteWorkflowNodeResult{}, errors.New("node run id is required")
	}
	if actor == "" {
		actor = ActorHuman
	}
	nodeRun, found, err := s.GetNodeRun(ctx, expectedNodeRunID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if !found {
		return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
	}
	run, err := s.Get(ctx, nodeRun.WorkflowRunID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if run.TaskID != taskID {
		return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
	}
	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
	}
	outcome, ok := workflowSkipOutcome(node.Kind)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: workflow node kind %q cannot be skipped", ErrWorkflowConflict, node.Kind)
	}
	return s.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID:      expectedNodeRunID,
		Outcome:        outcome,
		Actor:          actor,
		Payload:        map[string]any{"skipped": true},
		IdempotencyKey: "skip:" + expectedNodeRunID,
		SkipTaskID:     taskID,
	})
}

func workflowSkipOutcome(kind NodeKind) (string, bool) {
	switch kind {
	case NodeAutomatedChecks, NodeVerifyChange:
		return "passed", true
	case NodeChangeReview:
		return "approved", true
	default:
		return "", false
	}
}

func retireSkippedWorkflowNodeTx(ctx context.Context, tx workflowTx, taskID, workflowRunID, nodeRunID string, kind NodeKind, now time.Time) (int64, int64, error) {
	nowText := sqlitex.FormatTime(now)
	jobResult, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND node_run_id = ? AND state IN ('queued', 'claimed', 'running')`,
		nowText, workflowRunID, nodeRunID)
	if err != nil {
		return 0, 0, err
	}
	cancelledJobs, err := jobResult.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (
	SELECT id FROM jobs WHERE workflow_run_id = ? AND node_run_id = ?
) AND released_at IS NULL`, nowText, workflowRunID, nodeRunID); err != nil {
		return 0, 0, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE checks
SET required = 0,
	verdict = ?,
	details = CASE
		WHEN trim(details) = '' THEN ?
		ELSE details || char(10) || char(10) || ?
	END,
	updated_at = ?
WHERE task_id = ?
	AND name LIKE ?
	AND verdict != ?`,
		string(CheckSkipped), "Skipped with the workflow step by an operator.", "Skipped with the workflow step by an operator.", nowText,
		taskID, "%.node."+nodeRunID, string(CheckSatisfied))
	if err != nil {
		return 0, 0, err
	}
	retiredChecks, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	checkKind, ok := workflowSkipCheckKind(kind)
	if !ok {
		return 0, 0, fmt.Errorf("%w: workflow node kind %q cannot be skipped", ErrWorkflowConflict, kind)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO checks (
	task_id, name, kind, required, verdict, exit_code, details,
	source_job_id, reporter, created_at, updated_at
) VALUES (?, ?, ?, 1, ?, NULL, ?, NULL, 'coordinator', ?, ?)
ON CONFLICT(task_id, name) DO UPDATE SET
	kind = excluded.kind,
	required = excluded.required,
	verdict = excluded.verdict,
	exit_code = NULL,
	details = excluded.details,
	source_job_id = NULL,
	reporter = excluded.reporter,
	updated_at = excluded.updated_at`,
		taskID, "workflow-step-skipped.node."+nodeRunID, string(checkKind), string(CheckSatisfied),
		"Workflow step skipped by an operator; its required checks were waived.", nowText, nowText); err != nil {
		return 0, 0, err
	}
	return retiredChecks, cancelledJobs, nil
}

func workflowSkipCheckKind(kind NodeKind) (CheckKind, bool) {
	switch kind {
	case NodeAutomatedChecks:
		return CheckKindCI, true
	case NodeChangeReview:
		return CheckKindReviewer, true
	case NodeVerifyChange:
		return CheckKindVerifier, true
	default:
		return "", false
	}
}

// Respond applies a verdict to an ordinary human gate. expectedWaitID is
// mandatory: a node run is not a review-round identity because a revisited
// gate (and an interactive revision) can open a later wait for related work.
// The wait is re-read and compared inside this immediate transaction before
// the state machine resolves it.
func (s *WorkflowRunService) Respond(ctx context.Context, taskID, nodeRunID, expectedWaitID, outcome, feedback string, actor Actor) (CompleteWorkflowNodeResult, error) {
	taskID = strings.TrimSpace(taskID)
	nodeRunID = strings.TrimSpace(nodeRunID)
	expectedWaitID = strings.TrimSpace(expectedWaitID)
	outcome = strings.TrimSpace(outcome)
	feedback = strings.TrimSpace(feedback)
	if taskID == "" {
		return CompleteWorkflowNodeResult{}, errors.New("task id is required")
	}
	if nodeRunID == "" {
		return CompleteWorkflowNodeResult{}, errors.New("node run id is required")
	}
	if expectedWaitID == "" {
		return CompleteWorkflowNodeResult{}, ErrReviewWaitIDRequired
	}
	if outcome == "" {
		return CompleteWorkflowNodeResult{}, errors.New("review outcome is required")
	}
	if actor == "" {
		actor = ActorHuman
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()

	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if run.TaskID != taskID {
		return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
	}
	wait, waiting, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if waiting {
		if run.State != WorkflowRunWaiting || run.CurrentNodeRunID != nodeRun.ID ||
			wait.Kind != WorkflowWaitHumanGate || wait.NodeRunID != nodeRun.ID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: task is not waiting on that node", ErrWorkflowConflict)
		}
		if wait.ID != expectedWaitID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: task is not waiting on that review round", ErrWorkflowConflict)
		}
	} else {
		// A transport retry may replay the exact resolved wait, but it can never
		// bind an old verdict to a later open wait. If there is a later wait the
		// branch above rejects its different id before this resolved lookup.
		if nodeRun.State != WorkflowNodeSucceeded {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: task is not waiting on that node", ErrWorkflowConflict)
		}
		var found bool
		wait, found, err = scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE id = ?`, expectedWaitID))
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		if !found || wait.WorkflowRunID != run.ID || wait.Kind != WorkflowWaitHumanGate || wait.NodeRunID != nodeRun.ID {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: task is not waiting on that review round", ErrWorkflowConflict)
		}
	}
	details, err := ParseReviewWaitDetails(wait.Details)
	if err != nil {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("decode review wait %q: %w", wait.ID, err)
	}
	if details.Interactive {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: wait requires interactive review response", ErrWorkflowConflict)
	}
	if details.GateNodeKey != nodeRun.NodeKey {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: review wait does not belong to that human gate", ErrWorkflowConflict)
	}
	offered := false
	for _, candidate := range details.Outcomes {
		if candidate == outcome {
			offered = true
			break
		}
	}
	if !offered {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: outcome %q is not offered by this review gate", ErrWorkflowConflict, outcome)
	}

	result, err := s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
		NodeRunID:       nodeRun.ID,
		Outcome:         outcome,
		Actor:           actor,
		Payload:         map[string]any{"feedback": feedback},
		IdempotencyKey:  "human:" + wait.ID + ":" + outcome,
		humanGateWaitID: wait.ID,
	}, false, nil)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	latest, err := s.Get(ctx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	result.Run = latest
	result.Done = latest.State == WorkflowRunCompleted
	return result, nil
}

// respondToReviewGateTx applies a human review verdict to the task's active
// workflow run inside the caller's transaction. It mirrors Respond for the
// review submission path, which must complete the gate in the same
// transaction as the head validation and the verdict record so a stale review
// cannot advance a workflow that has moved past the inspected head.
//
// A verdict that lands after the task's workflow already recorded a
// human-gate decision — the run answered its gate and moved on, or completed
// — is late and contradictory, so it is refused with ErrWorkflowConflict and
// the whole submission rolls back: no inline threads and no verdict check
// survive beside the decision they contradict. A task with no run, or a run
// that has not yet reached its human gate, is left alone — the review still
// records its threads and verdict.
func (s *WorkflowRunService) respondToReviewGateTx(ctx context.Context, tx workflowTx, taskID, expectedNodeRunID, expectedWaitID, verdict, feedback string) error {
	expectedNodeRunID = strings.TrimSpace(expectedNodeRunID)
	expectedWaitID = strings.TrimSpace(expectedWaitID)
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// An open gate answers the verdict only when the caller supplies the exact
	// persisted review-round id and node-run assertion it observed. A change
	// review can outlive a changes_requested loop, which reopens a new wait on
	// the same gate node; resolving by node alone would decide that newer round.
	if err == nil && run.State == WorkflowRunWaiting && run.CurrentNodeRunID != "" {
		nodeRun, nodeErr := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if nodeErr != nil {
			return nodeErr
		}
		wait, waiting, waitErr := openWaitTx(ctx, tx, run.ID)
		if waitErr != nil {
			return waitErr
		}
		if waiting && wait.Kind == WorkflowWaitHumanGate && wait.NodeRunID == nodeRun.ID {
			if expectedWaitID == "" {
				return ErrReviewWaitIDRequired
			}
			if expectedNodeRunID == "" || expectedNodeRunID != nodeRun.ID {
				return fmt.Errorf("%w: task is not waiting on that node", ErrWorkflowConflict)
			}
			if expectedWaitID != wait.ID {
				return fmt.Errorf("%w: task is not waiting on that review round", ErrWorkflowConflict)
			}
			details, parseErr := ParseReviewWaitDetails(wait.Details)
			if parseErr != nil {
				return fmt.Errorf("decode review wait %q: %w", wait.ID, parseErr)
			}
			if details.Interactive || details.GateNodeKey != nodeRun.NodeKey {
				return fmt.Errorf("%w: review wait does not belong to the current ordinary human gate", ErrWorkflowConflict)
			}
			outcome := "changes_requested"
			if verdict == "approve" {
				outcome = "approved"
			}
			offered := false
			for _, candidate := range details.Outcomes {
				if candidate == outcome {
					offered = true
					break
				}
			}
			if !offered {
				return fmt.Errorf("%w: outcome %q is not offered by this review gate", ErrWorkflowConflict, outcome)
			}
			_, err = s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
				NodeRunID:       nodeRun.ID,
				Outcome:         outcome,
				Actor:           ActorHuman,
				Payload:         map[string]any{"feedback": strings.TrimSpace(feedback)},
				IdempotencyKey:  "human:" + wait.ID + ":" + outcome,
				humanGateWaitID: wait.ID,
			}, false, nil)
			return err
		}
	}

	// A supplied binding means the caller observed a specific review round. If
	// that round is no longer the active ordinary gate, never persist the verdict
	// as an unbound review: it could otherwise appear to belong to a later round
	// or to a run that has already moved on. Calls with no binding still record
	// normal pre-gate review findings.
	if expectedNodeRunID != "" || expectedWaitID != "" {
		return fmt.Errorf("%w: task is not waiting on that bound review round", ErrWorkflowConflict)
	}

	// No open gate to answer. If the task's latest workflow run already
	// recorded a human-gate decision and no gate is left open or reachable for
	// this verdict — the run is finished, or it passed its last gate and is
	// still active with no future or revisited gate ahead — the verdict is late
	// and contradicts the decision the run moved on with, so the submission
	// must be refused rather than persisted beside it. A verdict that still has
	// a gate to belong to (an active run on its way to a second gate, or back
	// at a revisited gate) records its threads and check as today; a workflow
	// without a human gate, or a run that never reached its gate, records no
	// decision either.
	decided, err := s.humanGateDecidedTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if decided {
		return fmt.Errorf("%w: task is no longer waiting on a human gate; a decision was already recorded", ErrWorkflowConflict)
	}
	return nil
}

// humanGateDecidedTx reports whether the task's latest workflow run already
// recorded a human-gate decision that a new verdict would contradict: a gate
// node run in that run succeeded with an outcome, and no gate is left open or
// reachable for the new verdict to belong to. A verdict arriving then is late
// and would file its inline threads and overwrite the human-review check
// beside the decision it contradicts.
//
// A finished run (completed or cancelled) holds every decision it recorded:
// any succeeded gate with an outcome makes a later verdict stale. An active
// run (still scheduled, running, or waiting) keeps its verdicts valid while
// any human gate is still reachable from where the run is — a waiting run at
// a gate answers the verdict through the open-gate path in
// respondToReviewGateTx, and a run on its way to a second human gate later in
// the flow (or back at a revisited gate after a changes_requested loop) has a
// gate ahead that the fresh review belongs to. But once an active run has
// passed its last gate — a decided gate advanced the run to a non-gate node
// with no reachable future or revisited human gate — its recorded decision is
// just as stale as a finished run's, and a late verdict must be refused the
// same way. A workflow without a human gate, or a run that never reached its
// gate, records no decision and leaves verdicts free to file their threads
// and check.
func (s *WorkflowRunService) humanGateDecidedTx(ctx context.Context, tx workflowTx, taskID string) (bool, error) {
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? ORDER BY run_sequence DESC, id DESC LIMIT 1`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var gateKeys []string
	for _, node := range run.Snapshot.Nodes {
		if node.Kind == NodeHumanGate {
			gateKeys = append(gateKeys, node.Key)
		}
	}
	if len(gateKeys) == 0 {
		return false, nil
	}
	if run.State != WorkflowRunCompleted && run.State != WorkflowRunCancelled {
		// The run is still active. Its recorded decisions are only stale once
		// the run has passed its last human gate: if a gate is reachable from
		// the run's current node, a fresh verdict still belongs to a gate that
		// has not been reached and must file its threads and check.
		if snapshotReachesHumanGate(run.Snapshot, run.CurrentNodeKey, gateKeys) {
			return false, nil
		}
	}
	placeholders := make([]string, 0, len(gateKeys))
	args := make([]any, 0, len(gateKeys)+2)
	args = append(args, run.ID)
	for _, key := range gateKeys {
		args = append(args, key)
		placeholders = append(placeholders, "?")
	}
	args = append(args, string(WorkflowNodeSucceeded))
	var decided int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM workflow_node_runs
WHERE workflow_run_id = ? AND node_key IN (`+strings.Join(placeholders, ",")+`) AND state = ? AND outcome != ''`, args...).Scan(&decided); err != nil {
		return false, err
	}
	return decided > 0, nil
}

// snapshotReachesHumanGate reports whether any of the given gate keys is
// reachable from the node at `from` along the snapshot's edges. The current
// node itself counts: a run parked at its gate still has that gate open to a
// verdict. Cycles (a changes_requested loop back to the same gate) terminate
// through the seen set.
func snapshotReachesHumanGate(snapshot FlowSnapshot, from string, gateKeys []string) bool {
	gateSet := make(map[string]struct{}, len(gateKeys))
	for _, key := range gateKeys {
		gateSet[key] = struct{}{}
	}
	edges := make(map[string][]string, len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		edges[edge.From] = append(edges[edge.From], edge.To)
	}
	seen := make(map[string]struct{})
	queue := []string{from}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := gateSet[key]; ok {
			return true
		}
		queue = append(queue, edges[key]...)
	}
	return false
}

func (s *WorkflowRunService) ExtendBudget(ctx context.Context, taskID string, additional int, actor Actor) (WorkflowRun, error) {
	if additional < 1 || additional > MaxFlowTransitionBudget {
		return WorkflowRun{}, fmt.Errorf("additional budget must be between 1 and %d", MaxFlowTransitionBudget)
	}
	if actor == "" {
		actor = ActorHuman
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state = 'waiting'`, taskID))
	if err != nil {
		return WorkflowRun{}, err
	}
	wait, ok, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if !ok || wait.Kind != WorkflowWaitOperatorIntervention {
		return WorkflowRun{}, fmt.Errorf("%w: workflow is not waiting on an automation budget", ErrWorkflowConflict)
	}
	now := s.now().UTC()
	resolvedByCompletion := false
	switch wait.Reason {
	case WorkflowWaitReasonTransitionBudgetExhausted:
		if run.TransitionBudget+additional > MaxFlowTransitionBudget {
			return WorkflowRun{}, fmt.Errorf("transition budget may not exceed %d", MaxFlowTransitionBudget)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET transition_budget = transition_budget + ?, state = ?, version = version + 1 WHERE id = ?`,
			additional, string(WorkflowRunRunning), run.ID); err != nil {
			return WorkflowRun{}, err
		}
	case WorkflowWaitReasonReviewCycleLimit:
		var pending struct {
			Outcome        string         `json:"pending_outcome"`
			IdempotencyKey string         `json:"pending_idempotency_key"`
			Payload        map[string]any `json:"pending_payload"`
		}
		if err := json.Unmarshal(wait.Details, &pending); err != nil {
			return WorkflowRun{}, fmt.Errorf("decode pending review-cycle completion: %w", err)
		}
		if strings.TrimSpace(pending.Outcome) == "" || strings.TrimSpace(pending.IdempotencyKey) == "" {
			return WorkflowRun{}, fmt.Errorf("%w: review-cycle wait has no pending completion", ErrWorkflowConflict)
		}
		base := max(run.ReviewCycleBudget, run.ReviewCyclesUsed)
		if base+additional > MaxFlowTransitionBudget {
			return WorkflowRun{}, fmt.Errorf("review cycle budget may not exceed %d", MaxFlowTransitionBudget)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET review_cycle_budget = ?, version = version + 1 WHERE id = ?`,
			base+additional, run.ID); err != nil {
			return WorkflowRun{}, err
		}
		if _, err := s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
			NodeRunID: wait.NodeRunID, Outcome: pending.Outcome, Actor: actor,
			Payload: pending.Payload, IdempotencyKey: pending.IdempotencyKey,
		}, false, nil); err != nil {
			return WorkflowRun{}, err
		}
		resolvedByCompletion = true
	default:
		return WorkflowRun{}, fmt.Errorf("%w: workflow is not waiting on an automation budget", ErrWorkflowConflict)
	}
	if !resolvedByCompletion {
		if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
			return WorkflowRun{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

func (s *WorkflowRunService) Reset(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
	if err := s.requireTaskWorkItem(ctx, taskID); err != nil {
		return WorkflowRun{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if err != nil {
		return WorkflowRun{}, err
	}
	convergenceEvidence, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if convergenceEvidence != nil {
		return WorkflowRun{}, fmt.Errorf("%w: convergence hold requires an explicit disposition", ErrWorkflowConflict)
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE (workflow_run_id = ? OR task_id = ?) AND state IN ('queued', 'claimed', 'running')`,
		sqlitex.FormatTime(now), run.ID, taskID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (SELECT id FROM jobs WHERE workflow_run_id = ? OR task_id = ?) AND released_at IS NULL`,
		sqlitex.FormatTime(now), run.ID, taskID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE (workflow_run_id = ? OR task_id = ?) AND runtime_state IN ('starting', 'working', 'waiting')`,
		sqlitex.FormatTime(now), sqlitex.FormatTime(now), run.ID, taskID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, completed_at = COALESCE(completed_at, ?)
WHERE workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
		string(WorkflowNodeCancelled), sqlitex.FormatTime(now), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, cancelled_at = ?, current_node_run_id = NULL,
	completion_source = 'reset', version = version + 1 WHERE id = ?`,
		string(WorkflowRunCancelled), sqlitex.FormatTime(now), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return WorkflowRun{}, err
	}
	if err := retireIncompleteWorkflowChecksTx(ctx, tx, taskID, run.ID, now); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = NULL, done_resolution = NULL, done_at = NULL,
	updated_at = ? WHERE id = ?`, sqlitex.FormatTime(now), taskID); err != nil {
		return WorkflowRun{}, err
	}
	fromState := LifecycleInProgress
	if run.State == WorkflowRunScheduled {
		fromState = LifecycleScheduled
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, WorkflowRunID: run.ID, FromTaskState: string(fromState),
		EventKind: "workflow_reset", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:   EventTaskReset,
		Actor:  string(actor),
		TaskID: taskID,
		RunID:  run.ID,
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

// SetEventLog wires the project event log; a nil log disables emission.
func (s *WorkflowRunService) SetEventLog(log *EventLogService) {
	s.eventLog = log
}

func (s *WorkflowRunService) ForceDone(ctx context.Context, taskID string, resolution DoneResolution, note string, actor Actor) (Task, error) {
	return s.ForceDoneDetailed(ctx, taskID, resolution, note, nil, actor)
}

// ForceDoneDetailed is ForceDone plus a substantive resolution message and
// typed completion evidence, persisted with the done stamp.
func (s *WorkflowRunService) ForceDoneDetailed(ctx context.Context, taskID string, resolution DoneResolution, note string, evidence []Evidence, actor Actor) (Task, error) {
	if err := s.requireTaskWorkItem(ctx, taskID); err != nil {
		return Task{}, err
	}
	if err := ValidateDoneResolution(resolution); err != nil {
		return Task{}, err
	}
	evidence, err := validateEvidence(evidence)
	if err != nil {
		return Task{}, err
	}
	if resolution == ResolutionMerged {
		return Task{}, errors.New("merged resolution may only be produced by a merge node")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	var currentState sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return Task{}, err
	}
	if currentState.Valid && currentState.String == string(LifecycleDone) {
		return Task{}, fmt.Errorf("%w: task is already done", ErrWorkflowConflict)
	}
	now := s.now().UTC()
	var runID sql.NullString
	_ = tx.QueryRowContext(ctx, `
SELECT id FROM workflow_runs WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID).Scan(&runID)
	if runID.Valid {
		convergenceEvidence, err := activeConvergenceEvidenceTx(ctx, tx, runID.String)
		if err != nil {
			return Task{}, err
		}
		if convergenceEvidence != nil {
			return Task{}, fmt.Errorf("%w: convergence hold requires an explicit disposition", ErrWorkflowConflict)
		}
	}
	var runIDArg any
	if runID.Valid {
		runIDArg = runID.String
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE (task_id = ? OR (? IS NOT NULL AND workflow_run_id = ?))
	AND state IN ('queued', 'claimed', 'running')`, sqlitex.FormatTime(now), taskID, runIDArg, runIDArg); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (
	SELECT id FROM jobs WHERE task_id = ? OR (? IS NOT NULL AND workflow_run_id = ?)
) AND released_at IS NULL`, sqlitex.FormatTime(now), taskID, runIDArg, runIDArg); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE (task_id = ? OR (? IS NOT NULL AND workflow_run_id = ?))
	AND runtime_state IN ('starting', 'working', 'waiting')`,
		sqlitex.FormatTime(now), sqlitex.FormatTime(now), taskID, runIDArg, runIDArg); err != nil {
		return Task{}, err
	}
	if runID.Valid {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, completed_at = COALESCE(completed_at, ?)
WHERE workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
			string(WorkflowNodeCancelled), sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, completed_at = ?, completion_source = 'owner_override',
	current_node_run_id = NULL, version = version + 1 WHERE id = ?`,
			string(WorkflowRunCompleted), sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if err := resolveOpenWaitTx(ctx, tx, runID.String, actor, now); err != nil {
			return Task{}, err
		}
		if err := retireIncompleteWorkflowChecksTx(ctx, tx, taskID, runID.String, now); err != nil {
			return Task{}, err
		}
	}
	evidenceJSON := ""
	if len(evidence) > 0 {
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return Task{}, fmt.Errorf("encode evidence: %w", err)
		}
		evidenceJSON = string(encoded)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_message = ?, done_evidence_json = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(resolution), strings.TrimSpace(note), evidenceJSON, sqlitex.FormatTime(now), sqlitex.FormatTime(now), taskID); err != nil {
		return Task{}, err
	}
	// Force-done rebase tasks never finalized: close the running rebase row so
	// the feature is not wedged and the blocks it holds resolve with the
	// blocker done.
	rebaseState := string(RebaseCancelled)
	if resolution == ResolutionFailed {
		rebaseState = string(RebaseFailed)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE feature_rebases SET state = ?, completed_at = ?
WHERE task_id = ? AND state = 'running'`, rebaseState, sqlitex.FormatTime(now), taskID); err != nil {
		return Task{}, fmt.Errorf("close rebase row for force-done task: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"note": strings.TrimSpace(note), "resolution": resolution})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, WorkflowRunID: runID.String, FromTaskState: currentState.String,
		ToTaskState: string(LifecycleDone), EventKind: "owner_done", PayloadJSON: string(payload),
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return Task{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{taskID}, now); err != nil {
		return Task{}, err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventTaskDone,
		Actor:   string(actor),
		TaskID:  taskID,
		Payload: eventPayload(map[string]any{"resolution": string(resolution), "note": strings.TrimSpace(note), "evidence_count": len(evidence)}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.tasks.GetTask(ctx, taskID)
}

func retireIncompleteWorkflowChecksTx(ctx context.Context, tx workflowTransitionExecer, taskID, runID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE checks
SET required = 0, updated_at = ?
WHERE task_id = ?
	AND required = 1
	AND verdict != ?
	AND EXISTS (
		SELECT 1
		FROM workflow_node_runs AS node
		WHERE node.workflow_run_id = ?
			AND checks.name LIKE '%.node.' || node.id
	)`,
		sqlitex.FormatTime(now),
		taskID,
		string(CheckSatisfied),
		runID,
	)
	return err
}

func (s *WorkflowRunService) Reopen(ctx context.Context, taskID string, actor Actor) (Task, error) {
	if err := s.requireTaskWorkItem(ctx, taskID); err != nil {
		return Task{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	blockedByAncestor, err := hasManuallyCompletedAncestorTx(ctx, tx, taskID)
	if err != nil {
		return Task{}, err
	}
	if blockedByAncestor {
		return Task{}, ErrWorkItemParentClosed
	}
	var previousResolution string
	if err := tx.QueryRowContext(ctx, `SELECT done_resolution FROM tasks WHERE id = ? AND lifecycle_state = ?`,
		taskID, string(LifecycleDone)).Scan(&previousResolution); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, fmt.Errorf("%w: only Done tasks can be reopened", ErrWorkflowConflict)
		}
		return Task{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = NULL, done_resolution = NULL, done_at = NULL, updated_at = ?
WHERE id = ? AND lifecycle_state = ?`, sqlitex.FormatTime(now), taskID, string(LifecycleDone))
	if err != nil {
		return Task{}, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return Task{}, fmt.Errorf("%w: only Done tasks can be reopened", ErrWorkflowConflict)
	}
	payload, _ := json.Marshal(map[string]any{"previous_resolution": previousResolution})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, FromTaskState: string(LifecycleDone), EventKind: "task_reopened",
		PayloadJSON: string(payload), Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return Task{}, err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{taskID}, now); err != nil {
		return Task{}, err
	}
	if _, err := s.eventLog.AppendTx(ctx, tx, Event{
		Kind:    EventTaskReopened,
		Actor:   string(actor),
		TaskID:  taskID,
		Payload: eventPayload(map[string]any{"previous_resolution": previousResolution}),
	}); err != nil {
		slog.Warn("event log append failed", "error", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.tasks.GetTask(ctx, taskID)
}

func (s *WorkflowRunService) OpenWait(ctx context.Context, taskID string) (WorkflowWait, bool, error) {
	run, ok, err := s.ActiveForTask(ctx, taskID)
	if err != nil || !ok {
		return WorkflowWait{}, false, err
	}
	wait, waiting, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
	if err != nil || !waiting {
		return wait, waiting, err
	}
	if wait.Kind == WorkflowWaitHumanGate {
		if _, err := ParseReviewWaitDetails(wait.Details); err != nil {
			return WorkflowWait{}, false, fmt.Errorf("decode human-gate wait %q: %w", wait.ID, err)
		}
	}
	return wait, true, nil
}

func (s *WorkflowRunService) Substate(ctx context.Context, taskID string) (InProgressSubstate, *WorkflowWait, error) {
	wait, ok, err := s.OpenWait(ctx, taskID)
	if err != nil {
		return "", nil, err
	}
	if ok {
		return InProgressBlocked, &wait, nil
	}
	return InProgressWorking, nil, nil
}

// RequestAgentInput pauses the active agent node without completing it. The
// live session may continue polling for a human reply, while the task derives
// In Progress / Blocked from the durable wait.
func (s *WorkflowRunService) RequestAgentInput(ctx context.Context, nodeRunID, message string, actor Actor) error {
	nodeRunID = strings.TrimSpace(nodeRunID)
	message = strings.TrimSpace(message)
	if nodeRunID == "" || message == "" {
		return errors.New("node run id and message are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
	if err != nil {
		return err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return err
	}
	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || node.Kind != NodeAgent || run.CurrentNodeRunID != nodeRun.ID {
		return fmt.Errorf("%w: only the active agent node can request input", ErrWorkflowConflict)
	}
	if nodeRun.State == WorkflowNodeWaiting {
		wait, found, err := openWaitTx(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if found && wait.Kind == WorkflowWaitAgentRequest {
			return nil
		}
	}
	if nodeRun.State != WorkflowNodeRunning {
		return fmt.Errorf("%w: node run is %s", ErrWorkflowConflict, nodeRun.State)
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = ? WHERE id = ?`, string(WorkflowNodeWaiting), nodeRun.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`, string(WorkflowRunWaiting), run.ID); err != nil {
		return err
	}
	if err := insertWaitTx(ctx, tx, run.ID, nodeRun.ID, WorkflowWaitAgentRequest, message, actor, now); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"message": message})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
		EventKind: "agent_input_requested", PayloadJSON: string(payload), Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ResumeAgentRequest resolves an open agent-input wait after the reply has
// been queued to the live session. It leaves the same node visit active.
func (s *WorkflowRunService) ResumeAgentRequest(ctx context.Context, taskID, feedback string, actor Actor) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state = 'waiting'`, strings.TrimSpace(taskID)))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	wait, found, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return false, err
	}
	if !found || wait.Kind != WorkflowWaitAgentRequest || run.CurrentNodeRunID == "" {
		return false, nil
	}
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
	if err != nil {
		return false, err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = ? WHERE id = ?`, string(WorkflowNodeRunning), nodeRun.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`, string(WorkflowRunRunning), run.ID); err != nil {
		return false, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return false, err
	}
	payload, _ := json.Marshal(map[string]string{"feedback": strings.TrimSpace(feedback)})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
		EventKind: "agent_input_received", PayloadJSON: string(payload), Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *WorkflowRunService) completeTerminalTx(ctx context.Context, tx workflowTx, run *WorkflowRun, node FlowNodeSnapshot, source string, now time.Time) error {
	if node.Config.Terminal == nil {
		return fmt.Errorf("terminal node %q has no terminal config", node.Key)
	}
	resolution := node.Config.Terminal.Resolution
	if err := ValidateDoneResolution(resolution); err != nil {
		return err
	}
	if resolution == ResolutionMerged {
		var merged int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM changes WHERE workflow_run_id = ? AND merged_at IS NOT NULL`, run.ID).Scan(&merged); err != nil {
			return fmt.Errorf("verify merged terminal: %w", err)
		}
		if merged == 0 {
			return errors.New("merged terminal requires a merged change for this run")
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, current_node_key = ?, current_node_run_id = NULL,
	completed_at = ?, completion_source = ?, transitions_used = ?, version = version + 1 WHERE id = ?`,
		string(WorkflowRunCompleted), node.Key, sqlitex.FormatTime(now), source, run.TransitionsUsed, run.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(resolution), sqlitex.FormatTime(now), sqlitex.FormatTime(now), run.TaskID); err != nil {
		return err
	}
	// A rebase task that reaches a terminal without finalizing leaves its
	// feature_rebases row running; close it so the feature can rebase again.
	// The finalize node stamps the row finalized before the terminal runs, so
	// this only fires for cancelled, failed, abandoned, and force-done paths.
	rebaseState := string(RebaseCancelled)
	if resolution == ResolutionFailed {
		rebaseState = string(RebaseFailed)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE feature_rebases SET state = ?, completed_at = ?
WHERE task_id = ? AND state = 'running'`, rebaseState, sqlitex.FormatTime(now), run.TaskID); err != nil {
		return fmt.Errorf("close rebase row for terminal task: %w", err)
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleDone), FromNodeKey: run.CurrentNodeKey, ToNodeKey: node.Key,
		EventKind: "workflow_completed", PayloadJSON: fmt.Sprintf(`{"resolution":%q}`, resolution),
		Actor: string(ActorSystem), CreatedAt: now,
	}); err != nil {
		return err
	}
	return reconcileEpicAncestorsTx(ctx, tx, []string{run.TaskID}, now)
}

func createNodeRunTx(ctx context.Context, tx workflowTx, run WorkflowRun, nodeKey string, attempt int, inputArtifactID string, now time.Time) (WorkflowNodeRun, error) {
	var visit int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(visit), 0) + 1 FROM workflow_node_runs
WHERE workflow_run_id = ? AND node_key = ?`, run.ID, nodeKey).Scan(&visit); err != nil {
		return WorkflowNodeRun{}, err
	}
	id, err := randomPrefixedID("wnr")
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, run.ID, nodeKey, visit, attempt,
		string(WorkflowNodeQueued), sqlitex.NullableNonEmptyString(inputArtifactID), sqlitex.FormatTime(now)); err != nil {
		return WorkflowNodeRun{}, err
	}
	return WorkflowNodeRun{ID: id, WorkflowRunID: run.ID, NodeKey: nodeKey, Visit: visit, Attempt: attempt,
		State: WorkflowNodeQueued, InputArtifactID: inputArtifactID, CreatedAt: now}, nil
}

func enterWaitTx(ctx context.Context, tx workflowTx, run *WorkflowRun, nodeRun *WorkflowNodeRun, kind WorkflowWaitKind, message string, actor Actor, now time.Time) error {
	return enterWaitWithDetailsTx(ctx, tx, run, nodeRun, kind, nil, message, actor, now)
}

func enterWaitWithDetailsTx(ctx context.Context, tx workflowTx, run *WorkflowRun, nodeRun *WorkflowNodeRun, kind WorkflowWaitKind, details any, message string, actor Actor, now time.Time) error {
	if kind == WorkflowWaitHumanGate {
		encoded, err := json.Marshal(details)
		if err != nil {
			return fmt.Errorf("encode human-gate wait details: %w", err)
		}
		if _, err := ParseReviewWaitDetails(encoded); err != nil {
			return fmt.Errorf("construct human-gate wait details: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = ? WHERE id = ?`, string(WorkflowNodeWaiting), nodeRun.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, started_at = COALESCE(started_at, ?), version = version + 1 WHERE id = ?`,
		string(WorkflowRunWaiting), sqlitex.FormatTime(now), run.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleInProgress), sqlitex.FormatTime(now), run.TaskID); err != nil {
		return err
	}
	if run.State == WorkflowRunScheduled {
		if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
			TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleScheduled),
			ToTaskState: string(LifecycleInProgress), ToNodeKey: nodeRun.NodeKey,
			EventKind: "workflow_started", Actor: string(ActorSystem), CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	return insertWaitWithReasonTx(ctx, tx, run.ID, nodeRun.ID, kind, "", details, message, actor, now)
}

func humanGateWaitMessage(node FlowNodeSnapshot) string {
	if node.Config.HumanGate != nil && strings.TrimSpace(node.Config.HumanGate.Instructions) != "" {
		return strings.TrimSpace(node.Config.HumanGate.Instructions)
	}
	return node.Name
}

func insertWaitTx(ctx context.Context, tx workflowTx, runID, nodeRunID string, kind WorkflowWaitKind, message string, actor Actor, now time.Time) error {
	return insertWaitWithReasonTx(ctx, tx, runID, nodeRunID, kind, "", nil, message, actor, now)
}

func insertWaitWithReasonTx(
	ctx context.Context,
	tx workflowTx,
	runID, nodeRunID string,
	kind WorkflowWaitKind,
	reason WorkflowWaitReason,
	details any,
	message string,
	actor Actor,
	now time.Time,
) error {
	id, err := randomPrefixedID("ww")
	if err != nil {
		return err
	}
	detailsJSON := []byte("{}")
	if details != nil {
		detailsJSON, err = json.Marshal(details)
		if err != nil {
			return fmt.Errorf("encode workflow wait details: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO workflow_waits (
	id, workflow_run_id, node_run_id, kind, reason, details_json, message, state, created_by, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?, ?)`, id, runID, sqlitex.NullableNonEmptyString(nodeRunID),
		string(kind), string(reason), string(detailsJSON), strings.TrimSpace(message), string(actor), sqlitex.FormatTime(now))
	return err
}

func resolveOpenWaitTx(ctx context.Context, tx workflowTransitionExecer, runID string, actor Actor, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE workflow_waits SET state = 'resolved', resolved_by = ?, resolved_at = ?
WHERE workflow_run_id = ? AND state = 'open'`, string(actor), sqlitex.FormatTime(now), runID)
	return err
}

func openWaitTx(ctx context.Context, tx workflowTx, runID string) (WorkflowWait, bool, error) {
	return scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE workflow_run_id = ? AND state = 'open'`, runID))
}

func unresolvedBlockerCountTx(ctx context.Context, tx workflowTx, taskID string) (int, error) {
	return effectiveUnresolvedBlockerCountTx(ctx, tx, taskID)
}

type workflowTransitionInput struct {
	TaskID, WorkflowRunID, FromTaskState, ToTaskState string
	FromNodeKey, ToNodeKey, Outcome, EventKind        string
	PayloadJSON, Actor, IdempotencyKey                string
	CreatedAt                                         time.Time
}

type workflowTransitionExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// workflowTx is the transactional SQL surface the workflow state machine runs
// on. CompleteNode opens a real *sql.Tx; the review submission runs the
// human-gate completion inside its own BEGIN IMMEDIATE transaction so the
// verdict cannot be separated from the head validation that guards it.
// *sql.Tx and sqlitex.Tx both satisfy it.
type workflowTx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertWorkflowTransitionTx(ctx context.Context, tx workflowTransitionExecer, input workflowTransitionInput) error {
	payload := strings.TrimSpace(input.PayloadJSON)
	if payload == "" {
		payload = "{}"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO workflow_transitions (
	task_id, workflow_run_id, from_task_state, to_task_state, from_node_key,
	to_node_key, outcome, event_kind, payload_json, actor, idempotency_key, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.TaskID,
		sqlitex.NullableNonEmptyString(input.WorkflowRunID), input.FromTaskState, input.ToTaskState,
		input.FromNodeKey, input.ToNodeKey, input.Outcome, input.EventKind, payload,
		input.Actor, sqlitex.NullableNonEmptyString(input.IdempotencyKey), sqlitex.FormatTime(input.CreatedAt.UTC()))
	return err
}

const workflowRunSelect = `
SELECT id, task_id, run_sequence, flow_id, flow_snapshot_json, state,
	current_node_key, current_node_run_id, current_artifact_id,
	transition_budget, transitions_used, review_cycle_budget, review_cycles_used,
	version, created_at, started_at,
	completed_at, cancelled_at, completion_source, held_at, held_by
FROM workflow_runs`

func scanWorkflowRun(scanner taskScanner) (WorkflowRun, error) {
	var run WorkflowRun
	var flowID, nodeRunID, artifactID sql.NullString
	var snapshotJSON, state, createdAt string
	var startedAt, completedAt, cancelledAt, heldAt sql.NullString
	if err := scanner.Scan(&run.ID, &run.TaskID, &run.RunSequence, &flowID, &snapshotJSON, &state,
		&run.CurrentNodeKey, &nodeRunID, &artifactID, &run.TransitionBudget, &run.TransitionsUsed,
		&run.ReviewCycleBudget, &run.ReviewCyclesUsed, &run.Version, &createdAt, &startedAt,
		&completedAt, &cancelledAt, &run.CompletionSource, &heldAt, &run.HeldBy); err != nil {
		return WorkflowRun{}, err
	}
	snapshot, err := decodeFlowSnapshot([]byte(snapshotJSON))
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("decode workflow snapshot: %w", err)
	}
	run.Snapshot = snapshot
	run.State = WorkflowRunState(state)
	run.FlowID = flowID.String
	run.CurrentNodeRunID = nodeRunID.String
	run.CurrentArtifactID = artifactID.String
	if run.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
		return WorkflowRun{}, err
	}
	if run.StartedAt, err = parseNullableTime(startedAt); err != nil {
		return WorkflowRun{}, err
	}
	if run.CompletedAt, err = parseNullableTime(completedAt); err != nil {
		return WorkflowRun{}, err
	}
	if run.CancelledAt, err = parseNullableTime(cancelledAt); err != nil {
		return WorkflowRun{}, err
	}
	if run.HeldAt, err = parseNullableTime(heldAt); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

const workflowNodeRunSelect = `
SELECT id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id,
	output_artifact_id, outcome, error, created_at, started_at, completed_at
FROM workflow_node_runs`

func scanWorkflowNodeRun(scanner taskScanner) (WorkflowNodeRun, error) {
	var run WorkflowNodeRun
	var state, createdAt string
	var inputArtifact, outputArtifact, startedAt, completedAt sql.NullString
	if err := scanner.Scan(&run.ID, &run.WorkflowRunID, &run.NodeKey, &run.Visit, &run.Attempt,
		&state, &inputArtifact, &outputArtifact, &run.Outcome, &run.Error, &createdAt,
		&startedAt, &completedAt); err != nil {
		return WorkflowNodeRun{}, err
	}
	run.State = WorkflowNodeRunState(state)
	run.InputArtifactID = inputArtifact.String
	run.OutputArtifactID = outputArtifact.String
	var err error
	if run.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
		return WorkflowNodeRun{}, err
	}
	if run.StartedAt, err = parseNullableTime(startedAt); err != nil {
		return WorkflowNodeRun{}, err
	}
	if run.CompletedAt, err = parseNullableTime(completedAt); err != nil {
		return WorkflowNodeRun{}, err
	}
	return run, nil
}

const workflowWaitSelect = `
SELECT id, workflow_run_id, node_run_id, kind, reason, details_json, message, created_by, created_at
FROM workflow_waits`

func scanWorkflowWaitMaybe(scanner taskScanner) (WorkflowWait, bool, error) {
	var wait WorkflowWait
	var nodeRunID sql.NullString
	var kind, reason, detailsJSON, actor, createdAt string
	if err := scanner.Scan(
		&wait.ID,
		&wait.WorkflowRunID,
		&nodeRunID,
		&kind,
		&reason,
		&detailsJSON,
		&wait.Message,
		&actor,
		&createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowWait{}, false, nil
		}
		return WorkflowWait{}, false, err
	}
	wait.NodeRunID = nodeRunID.String
	wait.Kind = WorkflowWaitKind(kind)
	wait.Reason = WorkflowWaitReason(reason)
	wait.Details = json.RawMessage(detailsJSON)
	wait.CreatedBy = Actor(actor)
	parsed, err := sqlitex.ParseTime(createdAt)
	if err != nil {
		return WorkflowWait{}, false, err
	}
	wait.CreatedAt = parsed
	return wait, true, nil
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := sqlitex.ParseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
