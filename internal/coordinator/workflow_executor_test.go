package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

type reviewBarrierFixture struct {
	db       *flowdb.Store
	task     Task
	runs     *WorkflowRunService
	checks   *CheckService
	workers  *flowworker.Service
	executor *WorkflowExecutor
	runID    string
	nodeID   string
}

func newReviewBarrierFixture(t *testing.T, agents []SnapshotReviewAgent) *reviewBarrierFixture {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open workflow executor database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Review barrier"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	snapshot := FlowSnapshot{
		FlowName: "review barrier", StartNode: "review", TransitionBudget: 50,
		Nodes: []FlowNodeSnapshot{
			{Key: "review", Name: "Review", Kind: NodeChangeReview, Config: FlowNodeSnapshotConfig{ChangeReview: &ChangeReviewNodeSnapshotConfig{Agents: agents}}},
			{Key: "approved", Name: "Approved", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "changes", Name: "Changes requested", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionFailed}}},
		},
		Edges: []FlowEdge{
			{From: "review", Outcome: "approved", To: "approved"},
			{From: "review", Outcome: "changes_requested", To: "changes"},
		},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	const runID = "wr-review-barrier"
	const authorNodeID = "nr-author-complete"
	const reviewNodeID = "nr-review-current"
	const changeID = "ch-review-barrier"
	const artifactID = "wa-review-barrier"
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, current_artifact_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', 'review', ?, ?, 50,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		runID, task.ID, string(snapshotJSON), reviewNodeID, artifactID); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, output_artifact_id,
	outcome, created_at, started_at, completed_at
) VALUES (?, ?, 'author', 1, 1, 'succeeded', ?, 'completed',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		authorNodeID, runID, artifactID); err != nil {
		t.Fatalf("insert prior node: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id, created_at
) VALUES (?, ?, 'review', 1, 1, 'queued', ?, '2026-01-01T00:00:01Z')`,
		reviewNodeID, runID, artifactID); err != nil {
		t.Fatalf("insert review node: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (
	id, task_id, workflow_run_id, branch, base, head_sha, ready_at, created_at, updated_at
) VALUES (?, ?, ?, 'task/review-barrier', 'main', 'head-review-barrier',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID, runID); err != nil {
		t.Fatalf("insert change: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-author', 'change', 'Implemented', ?, 'digest', 'artifact-1',
	'2026-01-01T00:00:00Z')`,
		artifactID, runID, authorNodeID, fmt.Sprintf(`{"change_id":%q,"head_sha":"head-review-barrier"}`, changeID)); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	flows := NewFlowService(store.DB())
	runs := NewWorkflowRunService(store.DB(), flows, tasks)
	workers := flowworker.NewService(store.DB())
	checks := NewCheckService(store.DB())
	sessions := NewSessionService(store.DB(), tasks, workers)
	artifacts := NewWorkflowArtifactService(store.DB(), tasks)
	project := Project{ID: "p-test", Name: "test", BaseBranch: "main"}
	checkConfigs := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, nil, project, CheckConfigServiceOptions{})
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: store.DB(), Runs: runs, Artifacts: artifacts, Tasks: tasks,
		Checks: checks, CheckConfigs: checkConfigs, Sessions: sessions,
		Queue: workers, Project: project,
	})
	return &reviewBarrierFixture{
		db: store, task: task, runs: runs, checks: checks, workers: workers,
		executor: executor, runID: runID, nodeID: reviewNodeID,
	}
}

func barrierAgents(secondBlocking bool) []SnapshotReviewAgent {
	return []SnapshotReviewAgent{
		{Blocking: true, Agent: AgentDefSnapshot{Name: "code-review", Harness: "codex", Prompt: "Review correctness."}},
		{Blocking: secondBlocking, Agent: AgentDefSnapshot{Name: "security-review", Harness: "claude", Prompt: "Review security."}},
	}
}

func (f *reviewBarrierFixture) report(t *testing.T, name string, verdict CheckVerdict) {
	t.Helper()
	ctx := context.Background()
	check, err := f.checks.GetCheck(ctx, f.task.ID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	required := check.Required
	if _, err := f.checks.ReportCheck(ctx, ReportCheckInput{
		TaskID: f.task.ID, Name: name, Kind: check.Kind, Required: &required,
		Verdict: verdict, Details: string(verdict), Reporter: "test",
	}); err != nil {
		t.Fatalf("report check %s: %v", name, err)
	}
}

func (f *reviewBarrierFixture) assertReviewOutcome(t *testing.T, want string) {
	t.Helper()
	node, found, err := f.runs.GetNodeRun(context.Background(), f.nodeID)
	if err != nil || !found {
		t.Fatalf("load review node: found=%v err=%v", found, err)
	}
	if node.State != WorkflowNodeSucceeded || node.Outcome != want {
		t.Fatalf("review node = %+v, want succeeded/%s", node, want)
	}
	var transitions int
	if err := f.db.DB().QueryRow(`
SELECT COUNT(*) FROM workflow_transitions
WHERE workflow_run_id = ? AND from_node_key = 'review' AND event_kind = 'node_completed'`, f.runID).Scan(&transitions); err != nil {
		t.Fatalf("count review transitions: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("review completion transitions = %d, want 1", transitions)
	}
}

type workflowMergeConflictStub struct {
	err error
}

func (s workflowMergeConflictStub) MergeChange(context.Context, string) (MergeResult, error) {
	return MergeResult{}, s.err
}

func TestWorkflowExecutorMergeConflictReportsBlockedCheckForImplementor(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open workflow executor database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tasks := NewTaskService(store.DB(), "p-test")
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Resolve merge conflict"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	snapshot := FlowSnapshot{
		FlowName: "merge conflict", StartNode: "merge", TransitionBudget: 10,
		Nodes: []FlowNodeSnapshot{
			{Key: "merge", Name: "Merge", Kind: NodeMergeChange, Config: FlowNodeSnapshotConfig{MergeChange: &MergeChangeNodeConfig{}}},
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
				Workspace: WorkspaceChange,
				Artifact:  ArtifactChange,
				Agent:     AgentDefSnapshot{Name: "author", Harness: "codex", Prompt: "Implement the task."},
			}}},
		},
		Edges: []FlowEdge{{From: "merge", Outcome: "conflict", To: "implement"}},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal workflow snapshot: %v", err)
	}

	const (
		runID       = "wr-merge-conflict"
		sourceNode  = "nr-author-complete"
		mergeNode   = "nr-merge-conflict"
		artifactID  = "wa-merge-conflict"
		changeID    = "ch-merge-conflict"
		branch      = "task/t-test-0001/run-1"
		conflictLog = "Auto-merging internal/coordinator/workflow_executor.go\n" +
			"CONFLICT (content): Merge conflict in internal/coordinator/workflow_executor.go\n" +
			"Automatic merge failed; fix conflicts and then commit the result."
	)
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, current_artifact_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', 'merge', ?, ?, 10,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		runID, task.ID, string(snapshotJSON), mergeNode, artifactID); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, output_artifact_id,
	outcome, created_at, started_at, completed_at
) VALUES (?, ?, 'author', 1, 1, 'succeeded', ?, 'completed',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		sourceNode, runID, artifactID); err != nil {
		t.Fatalf("insert source node: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id, created_at
) VALUES (?, ?, 'merge', 1, 1, 'queued', ?, '2026-01-01T00:00:01Z')`,
		mergeNode, runID, artifactID); err != nil {
		t.Fatalf("insert merge node: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (
	id, task_id, workflow_run_id, branch, base, head_sha, ready_at, created_at, updated_at
) VALUES (?, ?, ?, ?, 'main', 'head-merge-conflict',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID, runID, branch); err != nil {
		t.Fatalf("insert change: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-author', 'change', 'Implemented', ?, 'digest', 'artifact-1',
	'2026-01-01T00:00:00Z')`,
		artifactID, runID, sourceNode,
		fmt.Sprintf(`{"change_id":%q,"head_sha":"head-merge-conflict"}`, changeID)); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	flows := NewFlowService(store.DB())
	runs := NewWorkflowRunService(store.DB(), flows, tasks)
	checks := NewCheckService(store.DB())
	sessions := NewSessionService(store.DB(), tasks, nil)
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: store.DB(), Runs: runs, Artifacts: NewWorkflowArtifactService(store.DB(), tasks),
		Tasks: tasks, Checks: checks, Sessions: sessions,
		Project: Project{ID: "p-test", Name: "test", BaseBranch: "main"},
	})
	executor.merges = workflowMergeConflictStub{
		err: fmt.Errorf("squash merge task branch: %w", &flowgit.MergeConflictError{Output: conflictLog}),
	}

	run, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	nodeRun, found, err := runs.GetNodeRun(ctx, mergeNode)
	if err != nil || !found {
		t.Fatalf("load merge node: found=%v err=%v", found, err)
	}
	node, ok := run.Snapshot.Node("merge")
	if !ok {
		t.Fatal("merge node missing from snapshot")
	}
	completed, err := executor.handleMergeChange(ctx, run, nodeRun, node)
	if err != nil {
		t.Fatalf("handle merge conflict: %v", err)
	}
	if !completed {
		t.Fatal("merge conflict did not complete the merge node")
	}

	completedNode, found, err := runs.GetNodeRun(ctx, mergeNode)
	if err != nil || !found {
		t.Fatalf("reload merge node: found=%v err=%v", found, err)
	}
	if completedNode.State != WorkflowNodeSucceeded || completedNode.Outcome != "conflict" {
		t.Fatalf("merge node = %+v, want succeeded/conflict", completedNode)
	}
	advanced, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("reload workflow run: %v", err)
	}
	if advanced.CurrentNodeKey != "implement" {
		t.Fatalf("workflow current node = %q, want implement", advanced.CurrentNodeKey)
	}

	check, err := checks.GetCheck(ctx, task.ID, AutoMergeCheckName)
	if err != nil {
		t.Fatalf("load auto-merge check: %v", err)
	}
	if !check.Required || check.Kind != CheckKindCI || check.Verdict != CheckBlocked ||
		check.ExitCode == nil || *check.ExitCode != 1 || check.Reporter != "coordinator" {
		t.Fatalf("auto-merge check = %+v, want required blocked coordinator CI failure", check)
	}
	for _, want := range []string{
		AutoMergeConflictDetailsPrefix,
		"Integrate origin/main into " + branch,
		"CONFLICT (content): Merge conflict in internal/coordinator/workflow_executor.go",
	} {
		if !strings.Contains(check.Details, want) {
			t.Fatalf("auto-merge check details missing %q:\n%s", want, check.Details)
		}
	}
	state, err := checks.ReviewState(ctx, task.ID)
	if err != nil {
		t.Fatalf("load review state: %v", err)
	}
	if state != ReviewChangesRequested {
		t.Fatalf("review state = %q, want %q so the next author receives fix context", state, ReviewChangesRequested)
	}
}

func TestWorkflowExecutorConcurrentChangeWorkspaceUsesOneChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Concurrent change workspace"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	const runID = "wr-concurrent-change"
	const nodeID = "nr-concurrent-change"
	snapshotJSON, err := json.Marshal(FlowSnapshot{
		FlowName: "concurrent change", StartNode: "implement", TransitionBudget: 10,
		Nodes: []FlowNodeSnapshot{{
			Key: "implement", Name: "Implement", Kind: NodeAgent,
			Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
				Agent:     AgentDefSnapshot{Name: "author", Harness: "codex", Prompt: "Implement the task."},
				Workspace: WorkspaceChange,
				Artifact:  ArtifactChange,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal workflow snapshot: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', 'implement', ?, 10,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, runID, task.ID, string(snapshotJSON), nodeID); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at
) VALUES (?, ?, 'implement', 1, 1, 'queued', '2026-01-01T00:00:00Z')`, nodeID, runID); err != nil {
		t.Fatalf("insert workflow node: %v", err)
	}

	runs := NewWorkflowRunService(fixture.store.DB(), NewFlowService(fixture.store.DB()), fixture.tasks)
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: fixture.store.DB(), Runs: runs,
		Artifacts: NewWorkflowArtifactService(fixture.store.DB(), fixture.tasks),
		Tasks:     fixture.tasks, Sessions: fixture.sessions, Queue: fixture.workers,
		Project: fixture.project,
	})
	const advances = 20
	start := make(chan struct{})
	errs := make(chan error, advances)
	var wg sync.WaitGroup
	for i := 0; i < advances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- executor.Advance(ctx, runID)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent advance: %v", err)
		}
	}

	detail, err := runs.Detail(ctx, runID)
	if err != nil {
		t.Fatalf("load workflow run: %v", err)
	}
	if detail.Run.State != WorkflowRunRunning {
		t.Fatalf("workflow detail = %+v, want running (not parked after a change conflict)", detail)
	}
	var changeID string
	var associatedRunID string
	var changeCount int
	if err := fixture.store.DB().QueryRowContext(ctx, `
SELECT MIN(id), MIN(workflow_run_id), COUNT(*)
FROM changes
WHERE task_id = ? AND branch = ?`, task.ID, "task/"+task.ID+"/run-1").Scan(&changeID, &associatedRunID, &changeCount); err != nil {
		t.Fatalf("load workflow change: %v", err)
	}
	if changeCount != 1 || changeID != "ch-test-0001" || associatedRunID != runID {
		t.Fatalf("workflow changes = count %d id %q run %q, want one ch-test-0001 for %s", changeCount, changeID, associatedRunID, runID)
	}
	var authorJobs int
	if err := fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs WHERE node_run_id = ? AND role = 'author'`, nodeID).Scan(&authorJobs); err != nil {
		t.Fatalf("count workflow author jobs: %v", err)
	}
	if authorJobs != 1 {
		t.Fatalf("workflow author jobs = %d, want 1", authorJobs)
	}
}

func TestWorkflowExecutorParallelReviewBarrier(t *testing.T) {
	t.Run("queues every job once and waits for the first blocking result", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		const advances = 20
		errs := make(chan error, advances)
		var wg sync.WaitGroup
		for i := 0; i < advances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent advance: %v", err)
		}
		jobs, err := fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		if len(jobs) != 2 || jobs[0].State != flowworker.JobQueued || jobs[1].State != flowworker.JobQueued {
			t.Fatalf("jobs before reports = %+v, want two queued", jobs)
		}

		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance after first report: %v", err)
		}
		node, found, err := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if err != nil || !found || node.State != WorkflowNodeRunning || node.Outcome != "" {
			t.Fatalf("node after first report = %+v found=%v err=%v", node, found, err)
		}

		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		errs = make(chan error, advances)
		wg = sync.WaitGroup{}
		for i := 0; i < advances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent final advance: %v", err)
		}
		fixture.assertReviewOutcome(t, "approved")
	})

	t.Run("does not fail fast when a blocking reviewer blocks", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance with one blocked: %v", err)
		}
		node, _, _ := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if node.State != WorkflowNodeRunning {
			t.Fatalf("node failed fast: %+v", node)
		}
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final advance: %v", err)
		}
		fixture.assertReviewOutcome(t, "changes_requested")
	})

	t.Run("awaits an advisory reviewer but ignores its blocked verdict", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(false))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance while advisory pending: %v", err)
		}
		node, _, _ := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if node.State != WorkflowNodeRunning {
			t.Fatalf("advisory reviewer was not awaited: %+v", node)
		}
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final advance: %v", err)
		}
		advisory, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "security-review.node."+fixture.nodeID)
		if err != nil || advisory.Required || advisory.Verdict != CheckBlocked {
			t.Fatalf("advisory result = %+v err=%v", advisory, err)
		}
		fixture.assertReviewOutcome(t, "approved")
	})

	t.Run("treats a skipped blocking reviewer as failure", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSkipped)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final advance: %v", err)
		}
		fixture.assertReviewOutcome(t, "changes_requested")
	})

	t.Run("pauses on a required reviewer execution error and retries only that check", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckErrored)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance after reviewer execution error: %v", err)
		}

		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("load paused workflow: %v", err)
		}
		if detail.Run.State != WorkflowRunWaiting ||
			detail.OpenWait == nil ||
			detail.OpenWait.Reason != WorkflowWaitReasonExecutionFailed {
			t.Fatalf("workflow detail = %+v, want execution-failure wait", detail)
		}
		if state, err := fixture.checks.ReviewState(ctx, fixture.task.ID); err != nil || state != ReviewInReview {
			t.Fatalf("review state = %s err=%v, want in_review", state, err)
		}

		retried, err := fixture.runs.RetryExecution(ctx, fixture.task.ID, ActorHuman, false)
		if err != nil {
			t.Fatalf("retry execution: %v", err)
		}
		if retried.State != WorkflowRunRunning {
			t.Fatalf("retried run = %+v, want running", retried)
		}
		node, found, err := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if err != nil || !found || node.Attempt != 2 || node.State != WorkflowNodeQueued || node.Error != "" {
			t.Fatalf("retried node = %+v found=%v err=%v", node, found, err)
		}
		satisfied, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "code-review.node."+fixture.nodeID)
		if err != nil || satisfied.Verdict != CheckSatisfied {
			t.Fatalf("satisfied check after retry = %+v err=%v", satisfied, err)
		}
		errored, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "security-review.node."+fixture.nodeID)
		if err != nil || errored.Verdict != CheckPending || errored.SourceJobID != nil {
			t.Fatalf("errored check after retry = %+v err=%v, want fresh pending", errored, err)
		}

		const retryAdvances = 10
		retryErrs := make(chan error, retryAdvances)
		var retryWG sync.WaitGroup
		for i := 0; i < retryAdvances; i++ {
			retryWG.Add(1)
			go func() {
				defer retryWG.Done()
				if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
					retryErrs <- err
				}
			}()
		}
		retryWG.Wait()
		close(retryErrs)
		for err := range retryErrs {
			t.Fatalf("concurrent advance after retry: %v", err)
		}
		jobs, err := fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		attemptTwoJobs := 0
		for _, job := range jobs {
			if attempt, ok := job.Payload["node_attempt"].(float64); ok && int(attempt) == 2 {
				attemptTwoJobs++
				if job.Payload["check_name"] != "security-review.node."+fixture.nodeID {
					t.Fatalf("retried wrong check: %+v", job.Payload)
				}
			}
		}
		if attemptTwoJobs != 1 {
			t.Fatalf("attempt-two jobs = %d, want one errored-check retry; jobs=%+v", attemptTwoJobs, jobs)
		}
	})

	t.Run("rejects retry after the pinned change head moves", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckErrored)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("pause workflow: %v", err)
		}
		if _, err := fixture.db.DB().ExecContext(ctx, `
UPDATE changes SET head_sha = 'head-moved' WHERE workflow_run_id = ?`, fixture.runID); err != nil {
			t.Fatalf("move change head: %v", err)
		}
		if _, err := fixture.runs.RetryExecution(ctx, fixture.task.ID, ActorHuman, false); !errors.Is(err, ErrWorkflowConflict) {
			t.Fatalf("retry error = %v, want workflow conflict", err)
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("load still-paused workflow: %v", err)
		}
		if detail.Run.State != WorkflowRunWaiting || detail.OpenWait == nil {
			t.Fatalf("workflow after rejected retry = %+v, want original wait", detail)
		}
		if _, err := fixture.runs.Reset(ctx, fixture.task.ID, ActorHuman); err != nil {
			t.Fatalf("reset workflow after rejected retry: %v", err)
		}
		check, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "code-review.node."+fixture.nodeID)
		if err != nil {
			t.Fatalf("load reset workflow check: %v", err)
		}
		if check.Required {
			t.Fatalf("reset workflow check = %+v, want historical non-required result", check)
		}
	})

	t.Run("reconciles a terminal check job that never reported", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		name := "code-review.node." + fixture.nodeID
		check, err := fixture.checks.GetCheck(ctx, fixture.task.ID, name)
		if err != nil {
			t.Fatalf("load scheduled check: %v", err)
		}
		if check.SourceJobID == nil {
			t.Fatalf("scheduled check = %+v, want bound source job", check)
		}
		if _, err := fixture.db.DB().ExecContext(ctx, `
UPDATE jobs SET state = ? WHERE id = ?`, string(flowworker.JobFailed), *check.SourceJobID); err != nil {
			t.Fatalf("fail source job: %v", err)
		}

		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("reconcile terminal source job: %v", err)
		}
		check, err = fixture.checks.GetCheck(ctx, fixture.task.ID, name)
		if err != nil {
			t.Fatalf("reload reconciled check: %v", err)
		}
		if check.Verdict != CheckErrored || !strings.Contains(check.Details, "without reporting a result") {
			t.Fatalf("reconciled check = %+v, want errored missing-result detail", check)
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("load paused workflow: %v", err)
		}
		if detail.Run.State != WorkflowRunWaiting ||
			detail.OpenWait == nil ||
			detail.OpenWait.Reason != WorkflowWaitReasonExecutionFailed {
			t.Fatalf("workflow after missing check result = %+v, want execution-failure wait", detail)
		}
	})
}
