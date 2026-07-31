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
	db                   *sql.DB
	runs                 *WorkflowRunService
	artifacts            *WorkflowArtifactService
	tasks                *TaskService
	features             *FeatureService
	checks               *CheckService
	checkConfigs         *CheckConfigService
	sessions             *SessionService
	merges               workflowChangeMerger
	queue                *flowworker.Service
	project              Project
	harnessArgs          []string
	reviewScopeFileLimit int
	reviewScopeLineLimit int
	handlers             map[NodeKind]workflowNodeHandler
}

type workflowChangeMerger interface {
	MergeChange(context.Context, string) (MergeResult, error)
}

type workflowNodeHandler func(context.Context, WorkflowRun, WorkflowNodeRun, FlowNodeSnapshot) (bool, error)

type convergenceRefLockContextKey struct{}

type convergenceRefLockState struct {
	exchangePath  string
	sourceRef     string
	sourceHeadSHA string
	targetRef     string
	targetTipSHA  string
}

type workflowExecutionError struct {
	failure WorkflowExecutionFailure
	err     error
}

const (
	DefaultReviewScopeFileLimit = 10
	DefaultReviewScopeLineLimit = 500
)

func (e *workflowExecutionError) Error() string {
	return e.err.Error()
}

func (e *workflowExecutionError) Unwrap() error {
	return e.err
}

type WorkflowExecutorOptions struct {
	Database  *sql.DB
	Runs      *WorkflowRunService
	Artifacts *WorkflowArtifactService
	Tasks     *TaskService
	// Features resolves the task's feature for the finalize_rebase handler.
	// Nil disables finalize_rebase nodes (tests construct executors directly).
	Features             *FeatureService
	Checks               *CheckService
	CheckConfigs         *CheckConfigService
	Sessions             *SessionService
	Merges               *MergeService
	Queue                *flowworker.Service
	Project              Project
	HarnessArgs          []string
	ReviewScopeFileLimit int
	ReviewScopeLineLimit int
}

func NewWorkflowExecutor(opts WorkflowExecutorOptions) *WorkflowExecutor {
	fileLimit := opts.ReviewScopeFileLimit
	if fileLimit <= 0 {
		fileLimit = DefaultReviewScopeFileLimit
	}
	lineLimit := opts.ReviewScopeLineLimit
	if lineLimit <= 0 {
		lineLimit = DefaultReviewScopeLineLimit
	}
	executor := &WorkflowExecutor{
		db: opts.Database, runs: opts.Runs, artifacts: opts.Artifacts, tasks: opts.Tasks,
		features: opts.Features,
		checks:   opts.Checks, checkConfigs: opts.CheckConfigs, sessions: opts.Sessions,
		merges: opts.Merges, queue: opts.Queue, project: opts.Project, harnessArgs: opts.HarnessArgs,
		reviewScopeFileLimit: fileLimit, reviewScopeLineLimit: lineLimit,
	}
	executor.handlers = map[NodeKind]workflowNodeHandler{
		NodeAgent:              executor.handleAgent,
		NodeAutomatedChecks:    executor.handleAutomatedChecks,
		NodeChangeReview:       executor.handleChangeReview,
		NodeVerifyChange:       executor.handleVerifyChange,
		NodeMaterializeTaskSet: executor.handleMaterializeTaskSet,
		NodeMergeChange:        executor.handleMergeChange,
		NodeFinalizeRebase:     executor.handleFinalizeRebase,
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
	base, err := e.changeBaseForTask(ctx, taskID)
	if err != nil {
		return false, err
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
		append(append([]string{}, e.harnessArgs...), modelArgs...))
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
	if node.Config.Agent.Workspace == WorkspaceChange && run.ReviewCyclesUsed > 0 {
		payload["review_cycle_number"] = run.ReviewCyclesUsed
		payload["review_cycle_limit"] = run.ReviewCycleBudget
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

// changeBaseForTask resolves the base branch a task's changes branch from
// and merge into: the feature branch when the task belongs to an open
// feature, the project base branch otherwise. A landed or archived feature no
// longer routes work to its frozen branch; a dangling feature_id (defensive —
// the FK normally keeps this impossible) falls back the same way.
func (e *WorkflowExecutor) changeBaseForTask(ctx context.Context, taskID string) (string, error) {
	base := strings.TrimSpace(e.project.BaseBranch)
	if base == "" {
		base = "main"
	}
	var featureBranch, featureStatus string
	err := e.db.QueryRowContext(ctx, `
SELECT f.branch, f.status
FROM features f
JOIN tasks t ON t.feature_id = f.id
WHERE t.id = ?`, strings.TrimSpace(taskID)).Scan(&featureBranch, &featureStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return base, nil
	}
	if err != nil {
		return "", fmt.Errorf("load task feature: %w", err)
	}
	if FeatureStatus(featureStatus) != FeatureOpen {
		return base, nil
	}
	if strings.TrimSpace(featureBranch) == "" {
		return base, nil
	}

	return featureBranch, nil
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
	held, err := e.holdOversizedChangeForConvergence(ctx, run, nodeRun)
	if err != nil || held {
		return false, err
	}
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
	agents := node.Config.ChangeReview.Agents
	names, err := e.checkConfigs.ScheduleWorkflowNodeChecks(
		ctx,
		task,
		change,
		WorkflowChecksReview,
		agents,
		run.ID,
		nodeRun.ID,
	)
	if err != nil {
		return false, err
	}
	checks, err := e.checks.ListChecks(ctx, run.TaskID)
	if err != nil {
		return false, err
	}
	byName := map[string]Check{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	for index, name := range names {
		check, ok := byName[name]
		if !ok || check.Verdict == CheckPending {
			return false, nil
		}
		if index < len(agents) && agents[index].Blocking && check.Verdict == CheckErrored {
			return false, workflowRequiredCheckError(check, WorkflowChecksReview, nodeRun.Attempt)
		}
	}

	aggregationName, err := e.checkConfigs.ScheduleWorkflowReviewAggregation(
		ctx,
		task,
		change,
		agents,
		node.Config.ChangeReview.Aggregator,
		names,
		run.ID,
		nodeRun.ID,
	)
	if err != nil {
		return false, err
	}
	aggregation, err := e.checks.GetCheck(ctx, run.TaskID, aggregationName)
	if err != nil {
		return false, err
	}
	if aggregation.Verdict == CheckPending {
		return false, nil
	}
	if aggregation.Verdict == CheckErrored {
		return false, workflowRequiredCheckError(aggregation, WorkflowChecksReview, nodeRun.Attempt)
	}
	outcome := "approved"
	if aggregation.Required && aggregation.Verdict != CheckSatisfied {
		outcome = "changes_requested"
	}
	_, err = e.runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID,
		Outcome:   outcome,
		Actor:     ActorSystem,
	})
	return err == nil, err
}

func (e *WorkflowExecutor) holdOversizedChangeForConvergence(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun) (bool, error) {
	exchangePath := strings.TrimSpace(e.project.ExchangePath)
	if exchangePath == "" {
		return false, nil
	}
	artifact, err := e.artifacts.Get(ctx, nodeRun.InputArtifactID)
	if err != nil {
		return false, fmt.Errorf("load review artifact for convergence check: %w", err)
	}
	changeID, pinnedHeadSHA, err := changeIdentityFromArtifact(artifact)
	if err != nil {
		return false, err
	}
	change, err := e.sessions.GetChange(ctx, changeID)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(change.HeadSHA) == "" {
		return false, nil
	}
	if pinnedHeadSHA == "" {
		return false, errors.New("change artifact has no pinned head_sha")
	}
	if pinnedHeadSHA != strings.TrimSpace(change.HeadSHA) {
		return false, fmt.Errorf("%w: change head moved from pinned %s to %s", ErrWorkflowConflict, pinnedHeadSHA, change.HeadSHA)
	}
	evidence, err := e.convergenceEvidenceForChange(ctx, run, nodeRun, change)
	if err != nil {
		return false, err
	}
	if !reviewScopeExceeded(evidence.Files, evidence.Additions+evidence.Deletions, e.reviewScopeFileLimit, e.reviewScopeLineLimit) {
		return false, nil
	}
	var held bool
	err = e.WithConvergenceEvidenceRefsLocked(ctx, evidence, func(lockedCtx context.Context) error {
		_, held, err = e.runs.HoldForConvergence(lockedCtx, evidence)
		return err
	})
	return held, err
}

// RequestConvergenceReview captures the active change's immutable Git evidence
// and starts the typed convergence flow without applying the automatic size
// threshold. A current, pinned change artifact is required so later owner
// dispositions can revalidate exactly what was reviewed.
func (e *WorkflowExecutor) RequestConvergenceReview(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
	run, active, err := e.runs.ActiveForTask(ctx, taskID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if !active {
		return WorkflowRun{}, ErrNoActiveWorkflowRun
	}
	if run.CurrentNodeRunID == "" {
		return WorkflowRun{}, fmt.Errorf("%w: workflow has no active node to hold", ErrWorkflowConflict)
	}
	if run.CurrentArtifactID == "" {
		return WorkflowRun{}, fmt.Errorf("%w: workflow has no current change artifact", ErrWorkflowConflict)
	}
	nodeRun, found, err := e.runs.GetNodeRun(ctx, run.CurrentNodeRunID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if !found || nodeRun.WorkflowRunID != run.ID {
		return WorkflowRun{}, fmt.Errorf("%w: workflow current node changed", ErrWorkflowConflict)
	}
	artifact, err := e.artifacts.Get(ctx, run.CurrentArtifactID)
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("load current artifact for convergence review: %w", err)
	}
	if artifact.WorkflowRunID != run.ID {
		return WorkflowRun{}, fmt.Errorf("%w: current artifact belongs to another workflow", ErrWorkflowConflict)
	}
	changeID, pinnedHeadSHA, err := changeIdentityFromArtifact(artifact)
	if err != nil {
		return WorkflowRun{}, err
	}
	if pinnedHeadSHA == "" {
		return WorkflowRun{}, errors.New("change artifact has no pinned head_sha")
	}
	change, err := e.sessions.GetChange(ctx, changeID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if change.TaskID != run.TaskID {
		return WorkflowRun{}, fmt.Errorf("%w: current change belongs to another task", ErrWorkflowConflict)
	}
	if pinnedHeadSHA != strings.TrimSpace(change.HeadSHA) {
		return WorkflowRun{}, fmt.Errorf("%w: change head moved from pinned %s to %s", ErrWorkflowConflict, pinnedHeadSHA, change.HeadSHA)
	}
	evidence, err := e.convergenceEvidenceForChange(ctx, run, nodeRun, change)
	if err != nil {
		return WorkflowRun{}, err
	}
	var held WorkflowRun
	err = e.WithConvergenceEvidenceRefsLocked(ctx, evidence, func(lockedCtx context.Context) error {
		var requestErr error
		held, _, requestErr = e.runs.RequestConvergenceReview(lockedCtx, evidence, actor)
		return requestErr
	})
	return held, err
}

// WithConvergenceEvidenceRefsLocked verifies and locks the exact source and
// target refs represented by evidence while fn records a final disposition.
// The lock is a no-op Git reference transaction, so competing pushes cannot
// invalidate the decision between the live-ref refresh and the SQLite commit.
func (e *WorkflowExecutor) WithConvergenceEvidenceRefsLocked(ctx context.Context, evidence ConvergenceEvidence, fn func(context.Context) error) error {
	if strings.TrimSpace(e.project.ExchangePath) == "" {
		return errors.New("project exchange path is not configured")
	}
	lockState := convergenceRefLockState{
		exchangePath:  e.project.ExchangePath,
		sourceRef:     "refs/heads/" + evidence.SourceBranch,
		sourceHeadSHA: evidence.SourceHeadSHA,
		targetRef:     "refs/heads/" + evidence.TargetBaseBranch,
		targetTipSHA:  evidence.TargetBaseTipSHA,
	}
	if active, ok := ctx.Value(convergenceRefLockContextKey{}).(convergenceRefLockState); ok && active == lockState {
		return fn(ctx)
	}
	return flowgit.WithLockedRefs(ctx, e.project.ExchangePath, map[string]string{
		lockState.sourceRef: lockState.sourceHeadSHA,
		lockState.targetRef: lockState.targetTipSHA,
	}, func(lockedCtx context.Context) error {
		return fn(context.WithValue(lockedCtx, convergenceRefLockContextKey{}, lockState))
	})
}

// RefreshConvergenceEvidence revalidates the exact Git observation before a
// final disposition. If repair_branch changed the source head, it adopts a new
// immutable artifact/evidence pair before the owner resolves the hold.
func (e *WorkflowExecutor) RefreshConvergenceEvidence(ctx context.Context, taskID string) (ConvergenceEvidence, error) {
	run, active, err := e.runs.ActiveForTask(ctx, taskID)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if !active || !run.Held() {
		return ConvergenceEvidence{}, ErrWorkflowNotHeld
	}
	current, err := e.runs.ActiveConvergenceEvidenceForTask(ctx, taskID)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if current == nil {
		return ConvergenceEvidence{}, fmt.Errorf("%w: workflow has no active convergence review", ErrWorkflowConflict)
	}
	nodeRun, found, err := e.runs.GetNodeRun(ctx, run.CurrentNodeRunID)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if !found {
		return ConvergenceEvidence{}, ErrWorkflowRunNotFound
	}
	change, err := e.sessions.GetChange(ctx, current.ChangeID)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	refreshed, err := e.convergenceEvidenceForChange(ctx, run, nodeRun, change)
	if err != nil {
		return ConvergenceEvidence{}, err
	}
	if refreshed.SourceHeadSHA == current.SourceHeadSHA &&
		refreshed.TargetBaseTipSHA == current.TargetBaseTipSHA &&
		refreshed.MergeBaseSHA == current.MergeBaseSHA &&
		refreshed.DiffDigest == current.DiffDigest {
		return *current, nil
	}
	return e.runs.RefreshConvergenceEvidence(ctx, refreshed)
}

func (e *WorkflowExecutor) convergenceEvidenceForChange(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, change Change) (ConvergenceEvidence, error) {
	exchangePath := strings.TrimSpace(e.project.ExchangePath)
	if exchangePath == "" {
		return ConvergenceEvidence{}, errors.New("project exchange path is required for convergence evidence")
	}
	sourceHeadSHA := strings.TrimSpace(change.HeadSHA)
	if sourceHeadSHA == "" {
		return ConvergenceEvidence{}, errors.New("review source change has no head")
	}
	sourceExists, err := flowgit.CommitExists(ctx, exchangePath, sourceHeadSHA)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("verify review source head: %w", err)
	}
	if !sourceExists {
		return ConvergenceEvidence{}, fmt.Errorf("review source head %s does not exist in the exchange", sourceHeadSHA)
	}
	sourceTipSHA, exists, err := flowgit.BranchTip(ctx, exchangePath, change.Branch)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("resolve review source branch: %w", err)
	}
	if !exists {
		return ConvergenceEvidence{}, fmt.Errorf("review source branch %s does not exist", change.Branch)
	}
	if sourceTipSHA != sourceHeadSHA {
		return ConvergenceEvidence{}, fmt.Errorf("%w: review source branch %s moved from projected %s to %s", ErrWorkflowConflict, change.Branch, sourceHeadSHA, sourceTipSHA)
	}
	baseTipSHA, exists, err := flowgit.BranchTip(ctx, exchangePath, change.Base)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("resolve review target base: %w", err)
	}
	if !exists {
		return ConvergenceEvidence{}, fmt.Errorf("review target base branch %s does not exist", change.Base)
	}
	mergeBaseSHA, err := flowgit.MergeBase(ctx, exchangePath, baseTipSHA, sourceHeadSHA)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("resolve review merge base: %w", err)
	}
	for _, sha := range []string{sourceHeadSHA, baseTipSHA, mergeBaseSHA} {
		ref := "refs/flow/convergence/objects/" + sha
		if err := flowgit.CreateOrVerifyRef(ctx, exchangePath, ref, sha); err != nil {
			return ConvergenceEvidence{}, fmt.Errorf("retain convergence commit %s: %w", sha, err)
		}
	}
	diffDigest, err := flowgit.CanonicalDiffDigest(ctx, exchangePath, mergeBaseSHA, sourceHeadSHA)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("fingerprint review change: %w", err)
	}
	stats, err := flowgit.ChangedFileStatsBounded(ctx, exchangePath, mergeBaseSHA, sourceHeadSHA, convergencePayloadFileLimit)
	if err != nil {
		return ConvergenceEvidence{}, fmt.Errorf("measure review scope: %w", err)
	}
	changedFiles := make([]ConvergenceFile, 0, len(stats.Files))
	for _, file := range stats.Files {
		changedFiles = append(changedFiles, ConvergenceFile{
			Path: file.Path, Additions: file.Additions, Deletions: file.Deletions, Binary: file.Binary,
		})
	}
	return ConvergenceEvidence{
		WorkflowRunID: run.ID, NodeRunID: nodeRun.ID, ChangeID: change.ID, TaskID: run.TaskID,
		SourceBranch: change.Branch, SourceHeadSHA: sourceHeadSHA,
		TargetBaseBranch: change.Base, TargetBaseTipSHA: baseTipSHA, MergeBaseSHA: mergeBaseSHA,
		Files: stats.FileCount, Additions: stats.Additions, Deletions: stats.Deletions,
		ChangedFiles: changedFiles, DiffDigest: diffDigest,
		ReviewCyclesUsed: run.ReviewCyclesUsed, ReviewCycleBudget: run.ReviewCycleBudget,
		MaxFiles: e.reviewScopeFileLimit, MaxLines: e.reviewScopeLineLimit,
	}, nil
}

func reviewScopeExceeded(files, lines, maxFiles, maxLines int) bool {
	return files > maxFiles || lines > maxLines
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
			return false, workflowRequiredCheckError(check, mode, nodeRun.Attempt)
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

func workflowRequiredCheckError(check Check, mode WorkflowCheckMode, attempt int) error {
	jobID := ""
	if check.SourceJobID != nil {
		jobID = *check.SourceJobID
	}
	cause := fmt.Errorf("required check %s failed to produce a result: %s", check.Name, strings.TrimSpace(check.Details))
	return &workflowExecutionError{
		failure: WorkflowExecutionFailure{
			Operation: "run required " + string(check.Kind) + " check",
			NodeKind:  nodeKindForCheckMode(mode),
			Attempt:   attempt,
			CheckID:   check.ID,
			JobID:     jobID,
			Message:   cause.Error(),
		},
		err: cause,
	}
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
	// Author completion normally retires the prior conflict check. Keep merge
	// execution self-healing for workflow runs that reached this node before
	// that behavior was deployed or whose completion response was interrupted.
	if _, err := e.checks.RetireAutoMergeConflictCheckForNewRevision(ctx, run.TaskID); err != nil {
		return false, fmt.Errorf("retire prior workflow merge conflict: %w", err)
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

// handleFinalizeRebase publishes the rebase task's change head to the task's
// feature ref. The compare-and-swap guard in FeatureService.FinalizeRebase
// decides the outcome: "finalized" publishes, "stale" loops back to the
// rebase agent because the feature branch moved mid-rebase.
func (e *WorkflowExecutor) handleFinalizeRebase(ctx context.Context, run WorkflowRun, nodeRun WorkflowNodeRun, _ FlowNodeSnapshot) (bool, error) {
	if e.features == nil {
		return false, errors.New("feature service is not configured")
	}
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
	if task.FeatureID == nil {
		return false, errors.New("finalize_rebase task is not assigned to a feature")
	}
	outcome, err := e.features.FinalizeRebase(ctx, *task.FeatureID, run.TaskID, change)
	if err != nil {
		return false, err
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
	changeID, _, err := changeIdentityFromArtifact(artifact)
	return changeID, err
}

func changeIdentityFromArtifact(artifact WorkflowArtifact) (string, string, error) {
	if artifact.Kind != ArtifactChange {
		return "", "", errors.New("node requires a change artifact")
	}
	var payload struct {
		ChangeID string `json:"change_id"`
		HeadSHA  string `json:"head_sha"`
	}
	if err := json.Unmarshal(artifact.Payload, &payload); err != nil {
		return "", "", err
	}
	payload.ChangeID = strings.TrimSpace(payload.ChangeID)
	payload.HeadSHA = strings.TrimSpace(payload.HeadSHA)
	if payload.ChangeID == "" {
		return "", "", errors.New("change artifact has no change_id")
	}
	return payload.ChangeID, payload.HeadSHA, nil
}
