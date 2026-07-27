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

// A hold is an operator taking the run: the executor stops advancing it to the
// next node, but in-flight worker jobs keep running so an operator can attach a
// terminal and work in the same tree the agent was using.
//
// Holds are deliberately not modelled as a workflow_waits row. The waits table
// carries a single-open-row constraint (idx_workflow_waits_one_open), so a hold
// could not coexist with the human gate it is most useful to pause at, and
// resolveOpenWaitTx — which every node completion calls — would clear it as a
// side effect.

// ReleaseEdge names how a held run is handed back to the executor.
type ReleaseEdge string

const (
	// ReleaseResume drops the hold and leaves the run exactly where it was.
	ReleaseResume ReleaseEdge = "resume"
	// ReleaseSubmit completes the current node with an operator-supplied
	// artifact and takes the node's success edge.
	ReleaseSubmit ReleaseEdge = "submit"
	// ReleaseSatisfy completes the current node with no new artifact, taking
	// the success edge on the strength of the operator's say-so.
	ReleaseSatisfy ReleaseEdge = "satisfy"
	// ReleaseMerge abandons the remaining nodes and jumps to the terminal.
	ReleaseMerge ReleaseEdge = "merge"
)

// ErrWorkflowNotHeld reports a release against a run no operator holds.
var ErrWorkflowNotHeld = errors.New("workflow run is not held")

// HoldForConvergence pauses an oversized change once per workflow run before
// automated reviewers are dispatched. Releasing the hold with Resume records
// the operator's decision and the durable transition marker prevents the same
// run from being stopped again on every review visit.
func (s *WorkflowRunService) HoldForConvergence(
	ctx context.Context,
	taskID string,
	files int,
	additions int,
	deletions int,
	maxFiles int,
	maxLines int,
) (WorkflowRun, bool, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, strings.TrimSpace(taskID)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, false, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, false, err
	}
	var prior int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'workflow_convergence_review_requested'`,
		run.ID,
	).Scan(&prior); err != nil {
		return WorkflowRun{}, false, err
	}
	if prior > 0 || run.Held() {
		return run, false, nil
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = ?, held_by = ?, version = version + 1
WHERE id = ? AND held_at IS NULL`,
		sqlitex.FormatTime(now), string(ActorSystem), run.ID,
	); err != nil {
		return WorkflowRun{}, false, err
	}
	payload, err := json.Marshal(map[string]any{
		"files": files, "additions": additions, "deletions": deletions,
		"max_files": maxFiles, "max_lines": maxLines,
	})
	if err != nil {
		return WorkflowRun{}, false, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, EventKind: "workflow_convergence_review_requested",
		PayloadJSON: string(payload), Actor: string(ActorSystem), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, false, err
	}
	message := fmt.Sprintf(
		"Convergence review required before automated review: this change touches %d files and %d lines (automatic threshold: %d files or %d lines). Decide whether to split or re-scope the task, then release the hold with Resume.",
		files,
		additions+deletions,
		maxFiles,
		maxLines,
	)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO status_log (task_id, actor, message, kind, created_at)
VALUES (?, ?, ?, ?, ?)`,
		run.TaskID,
		string(ActorSystem),
		message,
		StatusKindPlan,
		sqlitex.FormatTime(now),
	); err != nil {
		return WorkflowRun{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRun{}, false, err
	}
	held, err := s.Get(ctx, run.ID)
	return held, true, err
}

// ReleaseWorkflowInput carries a hand-back. ArtifactID is required by
// ReleaseSubmit and ignored otherwise.
type ReleaseWorkflowInput struct {
	TaskID     string
	Edge       ReleaseEdge
	ArtifactID string
	Actor      Actor
}

// Hold stops the executor from advancing the task's active run. Holding an
// already-held run is a no-op so a double click cannot stack transitions.
func (s *WorkflowRunService) Hold(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
	if actor == "" {
		actor = ActorHuman
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, strings.TrimSpace(taskID)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, err
	}
	if run.Held() {
		return run, nil
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = ?, held_by = ?, version = version + 1 WHERE id = ?`,
		sqlitex.FormatTime(now), string(actor), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, EventKind: "workflow_hold_requested",
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

// Release hands a held run back to the executor along the named edge.
func (s *WorkflowRunService) Release(ctx context.Context, input ReleaseWorkflowInput) (CompleteWorkflowNodeResult, error) {
	if input.Actor == "" {
		input.Actor = ActorHuman
	}
	taskID := strings.TrimSpace(input.TaskID)

	run, nodeRun, err := s.clearHold(ctx, taskID, input.Actor, input.Edge)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if input.Edge == ReleaseResume {
		return CompleteWorkflowNodeResult{Run: run}, nil
	}
	if input.Edge == ReleaseMerge {
		return s.jumpToTerminal(ctx, run, input.Actor)
	}
	if nodeRun == nil {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: run has no active node to hand back", ErrWorkflowConflict)
	}

	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
	}
	outcome, ok := workflowSuccessOutcome(node)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node kind %q has no success edge to take", ErrWorkflowConflict, node.Kind)
	}

	completion := CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID,
		Outcome:   outcome,
		Actor:     input.Actor,
		Payload:   map[string]any{"released_edge": string(input.Edge)},
	}
	switch input.Edge {
	case ReleaseSubmit:
		completion.ArtifactID = strings.TrimSpace(input.ArtifactID)
		if completion.ArtifactID == "" {
			return CompleteWorkflowNodeResult{}, errors.New("submit requires an artifact")
		}
	case ReleaseSatisfy:
		// No artifact: the operator asserts the node is done, so waive the
		// agent-node artifact contract and carry the run's current artifact.
		completion.OperatorSatisfied = true
	default:
		return CompleteWorkflowNodeResult{}, fmt.Errorf("unknown release edge %q", input.Edge)
	}
	return s.CompleteNode(ctx, completion)
}

// clearHold drops the hold and returns the run plus its active node run, if any.
func (s *WorkflowRunService) clearHold(ctx context.Context, taskID string, actor Actor, edge ReleaseEdge) (WorkflowRun, *WorkflowNodeRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, nil, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, nil, err
	}
	if !run.Held() {
		return WorkflowRun{}, nil, ErrWorkflowNotHeld
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`, run.ID); err != nil {
		return WorkflowRun{}, nil, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, Outcome: string(edge),
		EventKind: "workflow_hold_released", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, nil, err
	}

	var nodeRun *WorkflowNodeRun
	if strings.TrimSpace(run.CurrentNodeRunID) != "" {
		active, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, nil, err
		}
		if err == nil {
			nodeRun = &active
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, nil, err
	}
	run.HeldAt = nil
	run.HeldBy = ""
	return run, nodeRun, nil
}

// jumpToTerminal abandons the remaining nodes and closes the run at its
// terminal node, which is how "skip to merge" ends a hand-back.
func (s *WorkflowRunService) jumpToTerminal(ctx context.Context, run WorkflowRun, actor Actor) (CompleteWorkflowNodeResult, error) {
	terminalKey, ok := run.Snapshot.TerminalNodeKey()
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: flow has no terminal node", ErrWorkflowConflict)
	}
	terminalNode, ok := run.Snapshot.Node(terminalKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("terminal node %q not found", terminalKey)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()

	now := s.now().UTC()
	fromNodeKey := run.CurrentNodeKey

	// Retire whatever the run was in the middle of before closing it out.
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, completed_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
		string(WorkflowNodeCancelled), sqlitex.FormatTime(now), run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'claimed', 'running')`,
		sqlitex.FormatTime(now), run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}

	terminalRun, err := createNodeRunTx(ctx, tx, run, terminalKey, 1, run.CurrentArtifactID, now)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, started_at = ?, completed_at = ? WHERE id = ?`,
		string(WorkflowNodeSucceeded), sqlitex.FormatTime(now), sqlitex.FormatTime(now), terminalRun.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: fromNodeKey, ToNodeKey: terminalKey,
		Outcome: string(ReleaseMerge), EventKind: "node_jumped", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := s.completeTerminalTx(ctx, tx, &run, terminalNode, "operator", now); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	completed, err := s.Get(ctx, run.ID)
	return CompleteWorkflowNodeResult{Run: completed, Done: true}, err
}

// workflowSuccessOutcome names the edge a node takes when it succeeds. Human
// gates declare their own vocabulary, so the first configured outcome is the
// affirmative one by convention.
func workflowSuccessOutcome(node FlowNodeSnapshot) (string, bool) {
	switch node.Kind {
	case NodeAgent, NodeMaterializeTaskSet:
		return "completed", true
	case NodeAutomatedChecks, NodeVerifyChange:
		return "passed", true
	case NodeChangeReview:
		return "approved", true
	case NodeMergeChange:
		return "merged", true
	case NodeHumanGate:
		if node.Config.HumanGate != nil && len(node.Config.HumanGate.Outcomes) > 0 {
			return node.Config.HumanGate.Outcomes[0], true
		}
	}
	return "", false
}

// WorkflowCardState is the per-task workflow summary the board renders: how
// long the task has sat where it is, how far through the flow it is, what it is
// waiting on, and whether an operator holds it.
type WorkflowCardState struct {
	RunID      string        `json:"run_id,omitempty"`
	StepIndex  int           `json:"step_index"`
	StepCount  int           `json:"step_count"`
	StepKey    string        `json:"step_key,omitempty"`
	DwellSince time.Time     `json:"dwell_since"`
	Wait       *WorkflowWait `json:"wait,omitempty"`
	Held       bool          `json:"held,omitempty"`
	HeldBy     string        `json:"held_by,omitempty"`
}

// CardState summarises the task's active run for the board. It reports false
// when the task has no active run (unscheduled, or already done).
func (s *WorkflowRunService) CardState(ctx context.Context, taskID string) (WorkflowCardState, bool, error) {
	run, ok, err := s.ActiveForTask(ctx, strings.TrimSpace(taskID))
	if err != nil || !ok {
		return WorkflowCardState{}, false, err
	}

	state := WorkflowCardState{
		RunID:     run.ID,
		StepCount: len(run.Snapshot.Nodes),
		StepKey:   run.CurrentNodeKey,
		Held:      run.Held(),
		HeldBy:    run.HeldBy,
	}
	if index, ok := run.Snapshot.NodeIndex(run.CurrentNodeKey); ok {
		state.StepIndex = index + 1
	}

	wait, waiting, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
	if err != nil {
		return WorkflowCardState{}, false, err
	}
	if waiting {
		state.Wait = &wait
	}

	state.DwellSince, err = s.dwellSince(ctx, run, state.Wait)
	if err != nil {
		return WorkflowCardState{}, false, err
	}
	return state, true, nil
}

// dwellSince answers "how long has this task been where it is". An open wait is
// the most specific answer; otherwise it is when the run entered its current
// node, falling back through the node run's own clock to the run's.
func (s *WorkflowRunService) dwellSince(ctx context.Context, run WorkflowRun, wait *WorkflowWait) (time.Time, error) {
	if wait != nil {
		return wait.CreatedAt, nil
	}
	if run.Held() {
		return *run.HeldAt, nil
	}

	var enteredAt sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT created_at FROM workflow_transitions
WHERE workflow_run_id = ? AND to_node_key = ?
ORDER BY seq DESC LIMIT 1`, run.ID, run.CurrentNodeKey).Scan(&enteredAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	if enteredAt.Valid {
		return sqlitex.ParseTime(enteredAt.String)
	}

	if strings.TrimSpace(run.CurrentNodeRunID) != "" {
		nodeRun, err := scanWorkflowNodeRun(s.db.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, err
		}
		if err == nil {
			if nodeRun.StartedAt != nil {
				return *nodeRun.StartedAt, nil
			}
			return nodeRun.CreatedAt, nil
		}
	}
	if run.StartedAt != nil {
		return *run.StartedAt, nil
	}
	return run.CreatedAt, nil
}
