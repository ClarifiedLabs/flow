package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// WorkflowExecutor is the trusted node-handler registry and dispatcher. Flow
// definitions select only these coordinator-owned handlers; definitions never
// contain executable code, expressions, commands, or webhooks.
type WorkflowExecutor struct {
	db           *sql.DB
	runs         *WorkflowRunService
	artifacts    *WorkflowArtifactService
	tasks        *TaskService
	checks       *CheckService
	checkConfigs *CheckConfigService
	sessions     *SessionService
	merges       workflowChangeMerger
	queue        *flowworker.Service
	project      Project
	harnessArgs  flowharness.Args
	handlers     map[NodeKind]workflowNodeHandler
}

type workflowChangeMerger interface {
	MergeChange(context.Context, string) (MergeResult, error)
}

type workflowNodeHandler func(context.Context, WorkflowRun, WorkflowNodeRun, FlowNodeSnapshot) (bool, error)

type workflowExecutionError struct {
	failure WorkflowExecutionFailure
	err     error
}

func (e *workflowExecutionError) Error() string {
	return e.err.Error()
}

func (e *workflowExecutionError) Unwrap() error {
	return e.err
}

type WorkflowExecutorOptions struct {
	Database     *sql.DB
	Runs         *WorkflowRunService
	Artifacts    *WorkflowArtifactService
	Tasks        *TaskService
	Checks       *CheckService
	CheckConfigs *CheckConfigService
	Sessions     *SessionService
	Merges       *MergeService
	Queue        *flowworker.Service
	Project      Project
	HarnessArgs  flowharness.Args
}

func NewWorkflowExecutor(opts WorkflowExecutorOptions) *WorkflowExecutor {
	executor := &WorkflowExecutor{
		db: opts.Database, runs: opts.Runs, artifacts: opts.Artifacts, tasks: opts.Tasks,
		checks: opts.Checks, checkConfigs: opts.CheckConfigs, sessions: opts.Sessions,
		merges: opts.Merges, queue: opts.Queue, project: opts.Project, harnessArgs: opts.HarnessArgs,
	}
	executor.handlers = map[NodeKind]workflowNodeHandler{
		NodeAgent:              executor.handleAgent,
		NodeAutomatedChecks:    executor.handleAutomatedChecks,
		NodeChangeReview:       executor.handleChangeReview,
		NodeVerifyChange:       executor.handleVerifyChange,
		NodeMaterializeTaskSet: executor.handleMaterializeTaskSet,
		NodeMergeChange:        executor.handleMergeChange,
	}
	return executor
}

// Tick advances every active workflow until it reaches asynchronous work, a
// human wait, a dependency wait, or Done. Repeated calls are idempotent.
func (e *WorkflowExecutor) Tick(ctx context.Context) error {
	// held_at excludes runs an operator has taken over; they resume only when
	// the hold is released.
	rows, err := e.db.QueryContext(ctx, `
SELECT id FROM workflow_runs
WHERE state IN ('scheduled', 'running') AND held_at IS NULL
ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var errs error
	for _, id := range ids {
		if err := e.Advance(ctx, id); err != nil {
			errs = errors.Join(errs, fmt.Errorf("advance workflow %s: %w", id, err))
		}
	}
	return errs
}

func (e *WorkflowExecutor) Advance(ctx context.Context, runID string) error {
	for step := 0; step < MaxFlowTransitionBudget+MaxFlowNodes; step++ {
		run, err := e.runs.Get(ctx, runID)
		if err != nil {
			return err
		}
		if run.State == WorkflowRunWaiting || run.State == WorkflowRunCompleted || run.State == WorkflowRunCancelled {
			return nil
		}
		// An operator holds the run. Advance is also called synchronously by
		// every mutating workflow endpoint, so this guard — not just Tick's
		// query — is what actually keeps a held run still.
		if run.Held() {
			return nil
		}
		nodeRun, _, err := e.runs.EnsureCurrentNode(ctx, run.ID)
		if err != nil {
			return e.pauseExecutionError(ctx, run, WorkflowNodeRun{}, NodeKind(""), "prepare workflow node", err)
		}
		if nodeRun.ID == "" {
			return nil
		}
		node, ok := run.Snapshot.Node(nodeRun.NodeKey)
		if !ok {
			return e.pauseExecutionError(
				ctx,
				run,
				nodeRun,
				NodeKind(""),
				"load workflow node",
				fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey),
			)
		}
		if node.Kind == NodeHumanGate || node.Kind == NodeTerminal {
			return nil
		}
		handler := e.handlers[node.Kind]
		if handler == nil {
			return e.pauseExecutionError(
				ctx,
				run,
				nodeRun,
				node.Kind,
				"dispatch workflow node",
				fmt.Errorf("no trusted handler registered for node kind %q", node.Kind),
			)
		}
		completed, err := handler(ctx, run, nodeRun, node)
		if err != nil {
			return e.pauseExecutionError(ctx, run, nodeRun, node.Kind, "execute "+string(node.Kind)+" node", err)
		}
		if !completed {
			return nil
		}
	}
	run, err := e.runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	return e.pauseExecutionError(
		ctx,
		run,
		WorkflowNodeRun{ID: run.CurrentNodeRunID, NodeKey: run.CurrentNodeKey},
		NodeKind(""),
		"advance workflow",
		errors.New("workflow executor local cascade limit exceeded"),
	)
}

func (e *WorkflowExecutor) pauseExecutionError(
	ctx context.Context,
	run WorkflowRun,
	nodeRun WorkflowNodeRun,
	nodeKind NodeKind,
	operation string,
	executionErr error,
) error {
	if executionErr == nil {
		return nil
	}
	if errors.Is(executionErr, context.Canceled) || errors.Is(executionErr, context.DeadlineExceeded) {
		return executionErr
	}
	if errors.Is(executionErr, ErrWorkflowConflict) {
		return nil
	}
	failure := WorkflowExecutionFailure{
		Operation: operation,
		NodeKind:  nodeKind,
		Attempt:   nodeRun.Attempt,
		Message:   executionErr.Error(),
	}
	var typed *workflowExecutionError
	if errors.As(executionErr, &typed) {
		failure = typed.failure
		if strings.TrimSpace(failure.Operation) == "" {
			failure.Operation = operation
		}
		if failure.NodeKind == "" {
			failure.NodeKind = nodeKind
		}
		if failure.Attempt == 0 {
			failure.Attempt = nodeRun.Attempt
		}
		if strings.TrimSpace(failure.Message) == "" {
			failure.Message = executionErr.Error()
		}
	}
	if err := e.runs.PauseForExecutionError(ctx, run.ID, nodeRun.ID, failure); err != nil {
		if errors.Is(err, ErrWorkflowConflict) {
			return nil
		}
		return errors.Join(executionErr, fmt.Errorf("park workflow after execution failure: %w", err))
	}
	return nil
}

func (e *WorkflowExecutor) handleAgent(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, node FlowNodeSnapshot) (bool, error) {
	if node.Config.Agent == nil {
		return false, errors.New("agent node is missing configuration")
	}
	var live int
	if err := e.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs WHERE node_run_id = ? AND state IN ('queued', 'claimed', 'running')`, nodeRun.ID).Scan(&live); err != nil {
		return false, err
	}
	if live > 0 {
		return false, nil
	}
	var terminalJobID, terminalState string
	err := e.db.QueryRowContext(ctx, `
SELECT id, state
FROM jobs
WHERE node_run_id = ?
	AND role = ?
	AND state IN (?, ?, ?, ?)
	AND CAST(COALESCE(json_extract(payload_json, '$.node_attempt'), 0) AS INTEGER) = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`,
		nodeRun.ID,
		string(flowworker.RoleAuthor),
		string(flowworker.JobFinished),
		string(flowworker.JobFailed),
		string(flowworker.JobCrashed),
		string(flowworker.JobCanceled),
		nodeRun.Attempt,
	).Scan(&terminalJobID, &terminalState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		cause := fmt.Errorf("author job %s ended in state %s before completing the workflow node", terminalJobID, terminalState)
		return false, &workflowExecutionError{
			failure: WorkflowExecutionFailure{
				Operation: "run author agent",
				NodeKind:  NodeAgent,
				Attempt:   nodeRun.Attempt,
				JobID:     terminalJobID,
				Message:   cause.Error(),
			},
			err: cause,
		}
	}

	taskID := run.TaskID
	var changeID *string
	branch := ""
	base := strings.TrimSpace(e.project.BaseBranch)
	if base == "" {
		base = "main"
	}
	if node.Config.Agent.Workspace == WorkspaceChange {
		change, err := e.changeForAgentNode(ctx, run, nodeRun, base)
		if err != nil {
			return false, err
		}
		changeID = &change.ID
		branch = change.Branch
	}
	modelArgs, err := node.Config.Agent.Agent.ModelSelectionArgs()
	if err != nil {
		return false, err
	}
	harness := flowharness.NormalizeName(node.Config.Agent.Agent.Harness)
	entrypoint, err := flowharness.DefaultAuthorEntrypointWithArgs(harness,
		e.harnessArgs.Add(flowharness.ArgsFor(harness, modelArgs)))
	if err != nil {
		return false, err
	}
	payload := map[string]any{
		"entrypoint": entrypoint, "workflow_run_id": run.ID, "node_run_id": nodeRun.ID,
		"node_attempt":   nodeRun.Attempt,
		"workspace_mode": node.Config.Agent.Workspace, "artifact_kind": node.Config.Agent.Artifact,
		"agent": node.Config.Agent.Agent, "role_instructions": node.Config.Agent.Agent.Prompt,
		"branch": branch, "base": base, "project_id": e.project.ID, "project_name": e.project.Name,
	}
	_, _, err = e.queue.EnqueueJobWithDispatchKey(ctx,
		fmt.Sprintf("workflow-agent:%s:%d", nodeRun.ID, nodeRun.Attempt),
		flowworker.EnqueueJobInput{
			TaskID: &taskID, ChangeID: changeID, WorkflowRunID: &run.ID, NodeRunID: &nodeRun.ID,
			Role: flowworker.RoleAuthor, CapacityBucket: flowworker.BucketPersistentAgent,
			Priority: 0, Requires: []string{flowharness.AgentHarnessLabel(harness)}, Payload: payload,
		})
	return false, err
}

func (e *WorkflowExecutor) changeForAgentNode(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, base string) (Change, error) {
	if nodeRun.InputArtifactID != "" {
		artifact, err := e.artifacts.Get(ctx, nodeRun.InputArtifactID)
		if err == nil && artifact.Kind == ArtifactChange {
			changeID, err := changeIDFromArtifact(artifact)
			if err != nil {
				return Change{}, err
			}
			return e.sessions.GetChange(ctx, changeID)
		}
		if err != nil && !errors.Is(err, ErrWorkflowArtifactNotFound) {
			return Change{}, err
		}
	}
	branch := fmt.Sprintf("task/%s/run-%d", run.TaskID, run.RunSequence)
	now := sqlitex.FormatTime(sqlitex.UTCNow())
	id, _, err := insertWithTaskChangeID(ctx, e.db, run.TaskID, branch, run.ID, func(id string) error {
		_, err := e.db.ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, run.TaskID, run.ID, branch, base, now, now)
		return err
	})
	if err != nil {
		return Change{}, fmt.Errorf("insert workflow change: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, `
UPDATE changes
SET workflow_run_id = ?, updated_at = ?
WHERE id = ? AND workflow_run_id IS NULL`, run.ID, now, id); err != nil {
		return Change{}, fmt.Errorf("associate workflow change: %w", err)
	}
	var associatedRunID sql.NullString
	if err := e.db.QueryRowContext(ctx, `SELECT workflow_run_id FROM changes WHERE id = ?`, id).Scan(&associatedRunID); err != nil {
		return Change{}, fmt.Errorf("load workflow change association: %w", err)
	}
	if !associatedRunID.Valid || associatedRunID.String != run.ID {
		return Change{}, fmt.Errorf("change %s belongs to workflow run %s", id, associatedRunID.String)
	}
	return e.sessions.GetChange(ctx, id)
}

func (e *WorkflowExecutor) handleAutomatedChecks(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, _ FlowNodeSnapshot) (bool, error) {
	return e.handleChecks(ctx, run, nodeRun, WorkflowChecksAutomated, nil, "passed", "failed")
}

func (e *WorkflowExecutor) handleChangeReview(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, node FlowNodeSnapshot) (bool, error) {
	if node.Config.ChangeReview == nil {
		return false, errors.New("change review node is missing configuration")
	}
	return e.handleChecks(ctx, run, nodeRun, WorkflowChecksReview, node.Config.ChangeReview.Agents, "approved", "changes_requested")
}

func (e *WorkflowExecutor) handleVerifyChange(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, node FlowNodeSnapshot) (bool, error) {
	if node.Config.VerifyChange == nil {
		return false, errors.New("verify node is missing configuration")
	}
	return e.handleChecks(ctx, run, nodeRun, WorkflowChecksVerify, node.Config.VerifyChange.Agents, "passed", "changes_requested")
}

func (e *WorkflowExecutor) handleChecks(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, mode WorkflowCheckMode, agents []SnapshotReviewAgent, successOutcome, failureOutcome string) (bool, error) {
	if nodeRun.State == WorkflowNodeQueued {
		if _, err := e.runs.MarkNodeRunning(ctx, nodeRun.ID); err != nil {
			return false, err
		}
	}
	artifact, err := e.artifacts.Get(ctx, nodeRun.InputArtifactID)
	if err != nil {
		return false, fmt.Errorf("load change artifact: %w", err)
	}
	changeID, err := changeIDFromArtifact(artifact)
	if err != nil {
		return false, err
	}
	change, err := e.sessions.GetChange(ctx, changeID)
	if err != nil {
		return false, err
	}
	task, err := e.tasks.GetTask(ctx, run.TaskID)
	if err != nil {
		return false, err
	}
	names, err := e.checkConfigs.ScheduleWorkflowNodeChecks(ctx, task, change, mode, agents, run.ID, nodeRun.ID)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		_, err := e.runs.CompleteNode(ctx, CompleteWorkflowNodeInput{NodeRunID: nodeRun.ID, Outcome: successOutcome, Actor: ActorSystem})
		return err == nil, err
	}
	checks, err := e.checks.ListChecks(ctx, run.TaskID)
	if err != nil {
		return false, err
	}
	byName := map[string]Check{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	allFinished := true
	failed := false
	for _, name := range names {
		check, ok := byName[name]
		if !ok || check.Verdict == CheckPending {
			allFinished = false
			continue
		}
		if check.Required && check.Verdict == CheckErrored {
			jobID := ""
			if check.SourceJobID != nil {
				jobID = *check.SourceJobID
			}
			cause := fmt.Errorf("required check %s failed to produce a result: %s", check.Name, strings.TrimSpace(check.Details))
			return false, &workflowExecutionError{
				failure: WorkflowExecutionFailure{
					Operation: "run required " + string(check.Kind) + " check",
					NodeKind:  nodeKindForCheckMode(mode),
					Attempt:   nodeRun.Attempt,
					CheckID:   check.ID,
					JobID:     jobID,
					Message:   cause.Error(),
				},
				err: cause,
			}
		}
		if check.Required && check.Verdict != CheckSatisfied {
			failed = true
		}
	}
	if !allFinished {
		return false, nil
	}
	outcome := successOutcome
	if failed {
		outcome = failureOutcome
	}
	_, err = e.runs.CompleteNode(ctx, CompleteWorkflowNodeInput{NodeRunID: nodeRun.ID, Outcome: outcome, Actor: ActorSystem})
	return err == nil, err
}

func nodeKindForCheckMode(mode WorkflowCheckMode) NodeKind {
	switch mode {
	case WorkflowChecksAutomated:
		return NodeAutomatedChecks
	case WorkflowChecksReview:
		return NodeChangeReview
	case WorkflowChecksVerify:
		return NodeVerifyChange
	default:
		return ""
	}
}

func (e *WorkflowExecutor) handleMaterializeTaskSet(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, node FlowNodeSnapshot) (bool, error) {
	if node.Config.MaterializeTaskSet == nil {
		return false, errors.New("materialize node is missing configuration")
	}
	if nodeRun.State == WorkflowNodeQueued {
		if _, err := e.runs.MarkNodeRunning(ctx, nodeRun.ID); err != nil {
			return false, err
		}
	}
	result, replayed, err := e.artifacts.MaterializeTaskSet(ctx, nodeRun.InputArtifactID, *node.Config.MaterializeTaskSet)
	if err != nil {
		return false, err
	}
	_, err = e.runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID, Outcome: "completed", Actor: ActorSystem,
		Payload: map[string]any{"task_ids": result.TaskIDs, "replayed": replayed},
	})
	return err == nil, err
}

func (e *WorkflowExecutor) handleMergeChange(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, _ FlowNodeSnapshot) (bool, error) {
	if nodeRun.State == WorkflowNodeQueued {
		if _, err := e.runs.MarkNodeRunning(ctx, nodeRun.ID); err != nil {
			return false, err
		}
	}
	artifact, err := e.artifacts.Get(ctx, nodeRun.InputArtifactID)
	if err != nil {
		return false, err
	}
	changeID, err := changeIDFromArtifact(artifact)
	if err != nil {
		return false, err
	}
	outcome := "merged"
	change, err := e.sessions.GetChange(ctx, changeID)
	if err != nil {
		return false, err
	}
	if change.MergedAt == nil {
		_, err = e.merges.MergeChange(ctx, changeID)
	}
	if err != nil {
		// A merge may have committed before the process lost its response. The
		// durable change latch is authoritative when this node is retried.
		if refreshed, loadErr := e.sessions.GetChange(ctx, changeID); loadErr == nil && refreshed.MergedAt != nil {
			err = nil
		}
	}
	if err != nil {
		var conflict *flowgit.MergeConflictError
		if errors.As(err, &conflict) ||
			errors.Is(err, flowgit.ErrHeadMismatch) ||
			errors.Is(err, flowgit.ErrNoMergeChanges) ||
			errors.Is(err, errRecordMergedConflict) {
			if reportErr := e.reportWorkflowMergeConflict(ctx, run.TaskID, change, err, conflict); reportErr != nil {
				return false, reportErr
			}
			outcome = "conflict"
		} else {
			return false, err
		}
	}
	_, err = e.runs.CompleteNode(ctx, CompleteWorkflowNodeInput{NodeRunID: nodeRun.ID, Outcome: outcome, Actor: ActorSystem})
	return err == nil, err
}

func (e *WorkflowExecutor) reportWorkflowMergeConflict(
	ctx context.Context,
	taskID string,
	change Change,
	mergeErr error,
	conflict *flowgit.MergeConflictError,
) error {
	required := true
	exitCode := 1
	details := ""
	if conflict != nil {
		details = strings.TrimSpace(conflict.Output)
	}
	if details == "" {
		details = strings.TrimSpace(mergeErr.Error())
	}
	if details == "" {
		details = flowgit.ErrMergeConflict.Error()
	}
	guidance := fmt.Sprintf(
		"Integrate origin/%s into %s, resolve the merge conflicts, commit the result, then run flow complete.",
		change.Base,
		change.Branch,
	)
	_, err := e.checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   taskID,
		Name:     AutoMergeCheckName,
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckBlocked,
		ExitCode: &exitCode,
		Details:  AutoMergeConflictDetailsPrefix + " " + guidance + "\n" + details,
		Reporter: "coordinator",
	})
	if err != nil {
		return fmt.Errorf("report workflow merge conflict: %w", err)
	}
	return nil
}

func changeIDFromArtifact(artifact WorkflowArtifact) (string, error) {
	if artifact.Kind != ArtifactChange {
		return "", errors.New("node requires a change artifact")
	}
	var payload struct {
		ChangeID string `json:"change_id"`
	}
	if err := json.Unmarshal(artifact.Payload, &payload); err != nil {
		return "", err
	}
	payload.ChangeID = strings.TrimSpace(payload.ChangeID)
	if payload.ChangeID == "" {
		return "", errors.New("change artifact has no change_id")
	}
	return payload.ChangeID, nil
}
