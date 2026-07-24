package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrWorkflowRunNotFound = errors.New("workflow run not found")
	ErrNoActiveWorkflowRun = errors.New("task has no active workflow run")
	ErrWorkflowConflict    = errors.New("workflow state conflict")
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
)

type WorkflowWaitReason string

const (
	WorkflowWaitReasonExecutionFailed           WorkflowWaitReason = "execution_failed"
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
	Version           int64            `json:"version"`
	CreatedAt         time.Time        `json:"created_at"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	CompletedAt       *time.Time       `json:"completed_at,omitempty"`
	CancelledAt       *time.Time       `json:"cancelled_at,omitempty"`
	CompletionSource  string           `json:"completion_source,omitempty"`
}

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
	Run         WorkflowRun          `json:"run"`
	NodeRuns    []WorkflowNodeRun    `json:"node_runs"`
	OpenWait    *WorkflowWait        `json:"open_wait,omitempty"`
	Substate    InProgressSubstate   `json:"substate,omitempty"`
	Transitions []WorkflowTransition `json:"transitions"`
}

type WorkflowRunService struct {
	db    *sql.DB
	flows *FlowService
	tasks *TaskService
	now   func() time.Time
}

func NewWorkflowRunService(db *sql.DB, flows *FlowService, tasks *TaskService) *WorkflowRunService {
	return &WorkflowRunService{db: db, flows: flows, tasks: tasks, now: sqlitex.UTCNow}
}

// Schedule freezes the selected flow and creates a new run. The task remains
// Scheduled until its first node actually starts; unresolved task blockers
// suppress node creation but not scheduling.
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
	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if task.State != nil {
		return WorkflowRun{}, fmt.Errorf("%w: task is already %s", ErrWorkflowConflict, *task.State)
	}
	snapshot, err := s.flows.ResolveSnapshot(ctx, task.FlowID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if len(snapshot.Nodes) == 0 || strings.TrimSpace(snapshot.StartNode) == "" {
		return WorkflowRun{}, errors.New("selected flow is not a graph workflow")
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("encode flow snapshot: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	var existingState sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM tasks WHERE id = ?`, taskID).Scan(&existingState); err != nil {
		return WorkflowRun{}, err
	}
	if existingState.Valid {
		return WorkflowRun{}, fmt.Errorf("%w: task is already %s", ErrWorkflowConflict, existingState.String)
	}
	var sequence int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(run_sequence), 0) + 1 FROM workflow_runs WHERE task_id = ?`, taskID).Scan(&sequence); err != nil {
		return WorkflowRun{}, err
	}
	id, err := randomPrefixedID("wr")
	if err != nil {
		return WorkflowRun{}, err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_id, flow_snapshot_json, state,
	current_node_key, transition_budget, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, taskID, sequence,
		sqlitex.NullableNonEmptyString(snapshot.FlowID), string(snapshotJSON), string(WorkflowRunScheduled),
		snapshot.StartNode, snapshot.TransitionBudget, sqlitex.FormatTime(now)); err != nil {
		return WorkflowRun{}, fmt.Errorf("insert workflow run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = NULL, done_at = NULL, updated_at = ?
WHERE id = ?`, string(LifecycleScheduled), sqlitex.FormatTime(now), taskID); err != nil {
		return WorkflowRun{}, fmt.Errorf("mark task scheduled: %w", err)
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, WorkflowRunID: id, ToTaskState: string(LifecycleScheduled),
		ToNodeKey: snapshot.StartNode, EventKind: "task_scheduled", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, id)
}

func (s *WorkflowRunService) Get(ctx context.Context, runID string) (WorkflowRun, error) {
	return scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, runID))
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
	detail := WorkflowRunDetail{Run: run, NodeRuns: nodes}
	if wait, ok, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID)); err != nil {
		return WorkflowRunDetail{}, err
	} else if ok {
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
	return detail, transitionRows.Err()
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
		if err := enterWaitTx(ctx, tx, &run, &nodeRun, WorkflowWaitHumanGate, humanGateWaitMessage(node), ActorSystem, s.now().UTC()); err != nil {
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
// results for the same pinned change revision remain authoritative.
func (s *WorkflowRunService) RetryExecution(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
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
		if node.Kind == NodeAutomatedChecks || node.Kind == NodeChangeReview || node.Kind == NodeVerifyChange {
			if err := verifyPinnedChangeHeadTx(ctx, tx, run.CurrentArtifactID); err != nil {
				return WorkflowRun{}, err
			}
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
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`,
		string(WorkflowRunRunning), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return WorkflowRun{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"attempt":      nodeRun.Attempt,
		"checks_reset": resetChecks,
	})
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

func verifyPinnedChangeHeadTx(ctx context.Context, tx *sql.Tx, artifactID string) error {
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
}

type CompleteWorkflowNodeResult struct {
	Run      WorkflowRun      `json:"run"`
	Next     *WorkflowNodeRun `json:"next,omitempty"`
	Done     bool             `json:"done"`
	Replayed bool             `json:"replayed,omitempty"`
}

func (s *WorkflowRunService) CompleteNode(ctx context.Context, input CompleteWorkflowNodeInput) (CompleteWorkflowNodeResult, error) {
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.Outcome = strings.TrimSpace(input.Outcome)
	if input.Actor == "" {
		input.Actor = ActorSystem
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, input.NodeRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if nodeRun.State == WorkflowNodeSucceeded {
		if nodeRun.Outcome != input.Outcome || (strings.TrimSpace(input.ArtifactID) != "" && strings.TrimSpace(nodeRun.OutputArtifactID) != strings.TrimSpace(input.ArtifactID)) {
			return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: completed node replay does not match the recorded outcome and artifact", ErrWorkflowConflict)
		}
		return CompleteWorkflowNodeResult{Run: run, Done: run.State == WorkflowRunCompleted, Replayed: true}, nil
	}
	if nodeRun.State != WorkflowNodeRunning && nodeRun.State != WorkflowNodeWaiting && nodeRun.State != WorkflowNodeQueued {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node run is %s", ErrWorkflowConflict, nodeRun.State)
	}
	if run.CurrentNodeRunID != nodeRun.ID {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node run is not active", ErrWorkflowConflict)
	}
	sourceNode, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
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
	artifactID := strings.TrimSpace(input.ArtifactID)
	if sourceNode.Kind == NodeAgent {
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
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: target,
		Outcome: input.Outcome, EventKind: "node_completed", PayloadJSON: string(payloadJSON),
		Actor: string(input.Actor), IdempotencyKey: input.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	used := run.TransitionsUsed + 1
	if targetNode.Kind == NodeTerminal {
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
		if err := tx.Commit(); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		completed, err := s.Get(ctx, run.ID)
		return CompleteWorkflowNodeResult{Run: completed, Done: true}, err
	}

	if used >= run.TransitionBudget {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, current_node_key = ?, current_node_run_id = NULL,
	current_artifact_id = ?, transitions_used = ?, version = version + 1
WHERE id = ?`, string(WorkflowRunWaiting), target, sqlitex.NullableNonEmptyString(artifactID), used, run.ID); err != nil {
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
		if err := tx.Commit(); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		waiting, err := s.Get(ctx, run.ID)
		return CompleteWorkflowNodeResult{Run: waiting}, err
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET state = ?, current_node_key = ?, current_node_run_id = NULL,
	current_artifact_id = ?, transitions_used = ?, version = version + 1
WHERE id = ?`, string(WorkflowRunRunning), target, sqlitex.NullableNonEmptyString(artifactID), used, run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run.State = WorkflowRunRunning
	run.CurrentNodeKey = target
	run.CurrentNodeRunID = ""
	run.CurrentArtifactID = artifactID
	run.TransitionsUsed = used
	next, err := createNodeRunTx(ctx, tx, run, target, 1, artifactID, now)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET current_node_run_id = ? WHERE id = ?`, next.ID, run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if targetNode.Kind == NodeHumanGate {
		if err := enterWaitTx(ctx, tx, &run, &next, WorkflowWaitHumanGate, humanGateWaitMessage(targetNode), ActorSystem, now); err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	updated, err := s.Get(ctx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	nextLoaded, _, err := s.GetNodeRun(ctx, next.ID)
	return CompleteWorkflowNodeResult{Run: updated, Next: &nextLoaded}, err
}

func (s *WorkflowRunService) Respond(ctx context.Context, taskID, nodeRunID, outcome, feedback string, actor Actor) (CompleteWorkflowNodeResult, error) {
	nodeRun, ok, err := s.GetNodeRun(ctx, strings.TrimSpace(nodeRunID))
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if !ok {
		return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
	}
	run, err := s.Get(ctx, nodeRun.WorkflowRunID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if run.TaskID != strings.TrimSpace(taskID) {
		return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
	}
	if nodeRun.State != WorkflowNodeSucceeded && (run.State != WorkflowRunWaiting || run.CurrentNodeRunID != nodeRun.ID) {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: task is not waiting on that node", ErrWorkflowConflict)
	}
	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || node.Kind != NodeHumanGate {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: active node is not a human gate", ErrWorkflowConflict)
	}
	return s.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRunID, Outcome: outcome, Actor: actor,
		Payload:        map[string]any{"feedback": strings.TrimSpace(feedback)},
		IdempotencyKey: "human:" + nodeRunID + ":" + strings.TrimSpace(outcome),
	})
}

func (s *WorkflowRunService) ExtendBudget(ctx context.Context, taskID string, additional int, actor Actor) (WorkflowRun, error) {
	if additional < 1 || additional > MaxFlowTransitionBudget {
		return WorkflowRun{}, fmt.Errorf("additional transitions must be between 1 and %d", MaxFlowTransitionBudget)
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
	if !ok ||
		wait.Kind != WorkflowWaitOperatorIntervention ||
		wait.Reason != WorkflowWaitReasonTransitionBudgetExhausted {
		return WorkflowRun{}, fmt.Errorf("%w: workflow is not waiting on its transition budget", ErrWorkflowConflict)
	}
	if run.TransitionBudget+additional > MaxFlowTransitionBudget {
		return WorkflowRun{}, fmt.Errorf("transition budget may not exceed %d", MaxFlowTransitionBudget)
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET transition_budget = transition_budget + ?, state = ?, version = version + 1 WHERE id = ?`,
		additional, string(WorkflowRunRunning), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

func (s *WorkflowRunService) Reset(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
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
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'claimed', 'running')`, sqlitex.FormatTime(now), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (SELECT id FROM jobs WHERE workflow_run_id = ?) AND released_at IS NULL`, sqlitex.FormatTime(now), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE workflow_run_id = ? AND runtime_state IN ('starting', 'working', 'waiting')`,
		sqlitex.FormatTime(now), sqlitex.FormatTime(now), run.ID); err != nil {
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
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

func (s *WorkflowRunService) ForceDone(ctx context.Context, taskID string, resolution DoneResolution, note string, actor Actor) (Task, error) {
	if err := validateDoneResolution(resolution); err != nil {
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
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'claimed', 'running')`, sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (SELECT id FROM jobs WHERE workflow_run_id = ?) AND released_at IS NULL`, sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE workflow_run_id = ? AND runtime_state IN ('starting', 'working', 'waiting')`,
			sqlitex.FormatTime(now), sqlitex.FormatTime(now), runID.String); err != nil {
			return Task{}, err
		}
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
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(resolution), sqlitex.FormatTime(now), sqlitex.FormatTime(now), taskID); err != nil {
		return Task{}, err
	}
	payload, _ := json.Marshal(map[string]any{"note": strings.TrimSpace(note), "resolution": resolution})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: taskID, WorkflowRunID: runID.String, FromTaskState: currentState.String,
		ToTaskState: string(LifecycleDone), EventKind: "owner_done", PayloadJSON: string(payload),
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.tasks.GetTask(ctx, taskID)
}

func retireIncompleteWorkflowChecksTx(ctx context.Context, tx *sql.Tx, taskID, runID string, now time.Time) error {
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
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
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
	return scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
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

func (s *WorkflowRunService) completeTerminalTx(ctx context.Context, tx *sql.Tx, run *WorkflowRun, node FlowNodeSnapshot, source string, now time.Time) error {
	if node.Config.Terminal == nil {
		return fmt.Errorf("terminal node %q has no terminal config", node.Key)
	}
	resolution := node.Config.Terminal.Resolution
	if err := validateDoneResolution(resolution); err != nil {
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
	return insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleDone), FromNodeKey: run.CurrentNodeKey, ToNodeKey: node.Key,
		EventKind: "workflow_completed", PayloadJSON: fmt.Sprintf(`{"resolution":%q}`, resolution),
		Actor: string(ActorSystem), CreatedAt: now,
	})
}

func createNodeRunTx(ctx context.Context, tx *sql.Tx, run WorkflowRun, nodeKey string, attempt int, inputArtifactID string, now time.Time) (WorkflowNodeRun, error) {
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

func enterWaitTx(ctx context.Context, tx *sql.Tx, run *WorkflowRun, nodeRun *WorkflowNodeRun, kind WorkflowWaitKind, message string, actor Actor, now time.Time) error {
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
	return insertWaitTx(ctx, tx, run.ID, nodeRun.ID, kind, message, actor, now)
}

func humanGateWaitMessage(node FlowNodeSnapshot) string {
	if node.Config.HumanGate != nil && strings.TrimSpace(node.Config.HumanGate.Instructions) != "" {
		return strings.TrimSpace(node.Config.HumanGate.Instructions)
	}
	return node.Name
}

func insertWaitTx(ctx context.Context, tx *sql.Tx, runID, nodeRunID string, kind WorkflowWaitKind, message string, actor Actor, now time.Time) error {
	return insertWaitWithReasonTx(ctx, tx, runID, nodeRunID, kind, "", nil, message, actor, now)
}

func insertWaitWithReasonTx(
	ctx context.Context,
	tx *sql.Tx,
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

func resolveOpenWaitTx(ctx context.Context, tx *sql.Tx, runID string, actor Actor, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE workflow_waits SET state = 'resolved', resolved_by = ?, resolved_at = ?
WHERE workflow_run_id = ? AND state = 'open'`, string(actor), sqlitex.FormatTime(now), runID)
	return err
}

func openWaitTx(ctx context.Context, tx *sql.Tx, runID string) (WorkflowWait, bool, error) {
	return scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE workflow_run_id = ? AND state = 'open'`, runID))
}

func unresolvedBlockerCountTx(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM task_relations r
JOIN tasks blocker ON blocker.id = r.source_task_id
WHERE r.kind = ? AND r.target_task_id = ?
	AND COALESCE(blocker.lifecycle_state, '') != ?`, string(RelationBlocks), taskID, string(LifecycleDone)).Scan(&count)
	return count, err
}

type workflowTransitionInput struct {
	TaskID, WorkflowRunID, FromTaskState, ToTaskState string
	FromNodeKey, ToNodeKey, Outcome, EventKind        string
	PayloadJSON, Actor, IdempotencyKey                string
	CreatedAt                                         time.Time
}

func insertWorkflowTransitionTx(ctx context.Context, tx *sql.Tx, input workflowTransitionInput) error {
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
	transition_budget, transitions_used, version, created_at, started_at,
	completed_at, cancelled_at, completion_source
FROM workflow_runs`

func scanWorkflowRun(scanner taskScanner) (WorkflowRun, error) {
	var run WorkflowRun
	var flowID, nodeRunID, artifactID sql.NullString
	var snapshotJSON, state, createdAt string
	var startedAt, completedAt, cancelledAt sql.NullString
	if err := scanner.Scan(&run.ID, &run.TaskID, &run.RunSequence, &flowID, &snapshotJSON, &state,
		&run.CurrentNodeKey, &nodeRunID, &artifactID, &run.TransitionBudget, &run.TransitionsUsed,
		&run.Version, &createdAt, &startedAt, &completedAt, &cancelledAt, &run.CompletionSource); err != nil {
		return WorkflowRun{}, err
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &run.Snapshot); err != nil {
		return WorkflowRun{}, fmt.Errorf("decode workflow snapshot: %w", err)
	}
	run.State = WorkflowRunState(state)
	run.FlowID = flowID.String
	run.CurrentNodeRunID = nodeRunID.String
	run.CurrentArtifactID = artifactID.String
	var err error
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
