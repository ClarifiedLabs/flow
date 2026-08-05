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
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
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
		FlowID: "fl-review-barrier", FlowName: "review barrier", StartNode: "author", TransitionBudget: 50,
		Nodes: []FlowNodeSnapshot{
			{Key: "author", Name: "Author", Kind: NodeAgent, Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
				Agent:     AgentDefSnapshot{ID: "ad-review-author", Name: "author", Harness: "harness", Prompt: "Implement the change."},
				Workspace: WorkspaceChange, Artifact: ArtifactChange,
			}}},
			{Key: "review", Name: "Review", Kind: NodeChangeReview, Config: FlowNodeSnapshotConfig{ChangeReview: &ChangeReviewNodeSnapshotConfig{
				Agents: agents,
				Aggregator: AgentDefSnapshot{
					ID: "ad-review-aggregator", Name: "review-aggregator", Harness: "harness", Model: "openai:gpt-5-mini", Prompt: "Synthesize review reports.",
				},
			}}},
			{Key: "approved", Name: "Approved", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "changes", Name: "Changes requested", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionFailed}}},
		},
		Edges: []FlowEdge{
			{From: "author", Outcome: "completed", To: "review"},
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
		{Blocking: true, Agent: AgentDefSnapshot{ID: "ad-code-review", Name: "code-review", Harness: "harness", Prompt: "Review correctness."}},
		{Blocking: secondBlocking, Agent: AgentDefSnapshot{ID: "ad-security-review", Name: "security-review", Harness: "harness", Prompt: "Review security."}},
	}
}

func TestRefreshConvergenceEvidenceAdoptsRepairedBranchHead(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectFixture(t)
	tasks := NewTaskService(fixture.store.DB(), fixture.project.ID)
	flows := NewFlowService(fixture.store.DB())
	runs := NewWorkflowRunService(fixture.store.DB(), flows, tasks)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)

	branch := "task/" + task.ID
	if err := runReconcileGit(fixture.repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("create source branch: %v", err)
	}
	writeReconcileFile(t, fixture.repoPath, "repair.txt", "first oversized observation\n")
	if err := runReconcileGit(fixture.repoPath, nil, "add", "repair.txt"); err != nil {
		t.Fatalf("stage source change: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "initial source change"); err != nil {
		t.Fatalf("commit source change: %v", err)
	}
	firstHead, err := reconcileGitOutput(fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve initial source head: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "push", fixture.project.ExchangePath, "HEAD:refs/heads/"+branch); err != nil {
		t.Fatalf("push initial source branch: %v", err)
	}
	const changeID = "ch-convergence-repair"
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, 'main', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID, run.ID, branch, firstHead); err != nil {
		t.Fatalf("insert source change: %v", err)
	}

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	if _, err := NewProjectService(global.DB()).Insert(ctx, fixture.project); err != nil {
		t.Fatalf("register project: %v", err)
	}
	workers := flowworker.NewService(fixture.store.DB())
	credentials := NewCredentialService(global.DB())
	sessions := NewSessionServiceWithOptions(fixture.store.DB(), tasks, workers, SessionServiceOptions{
		Credentials: credentials,
		Project:     fixture.project,
	})
	artifacts := NewWorkflowArtifactService(fixture.store.DB(), tasks)
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: fixture.store.DB(), Runs: runs, Artifacts: artifacts, Tasks: tasks,
		Sessions: sessions, Project: fixture.project,
	})
	change, err := sessions.GetChange(ctx, changeID)
	if err != nil {
		t.Fatalf("load source change: %v", err)
	}
	firstEvidence, err := executor.convergenceEvidenceForChange(ctx, run, nodeRun, change)
	if err != nil {
		t.Fatalf("capture initial convergence evidence: %v", err)
	}
	for _, sha := range []string{firstEvidence.SourceHeadSHA, firstEvidence.TargetBaseTipSHA, firstEvidence.MergeBaseSHA} {
		ref := "refs/flow/convergence/objects/" + sha
		retained, err := reconcileGitOutput(fixture.repoPath, nil, "--git-dir", fixture.project.ExchangePath, "rev-parse", ref)
		if err != nil || retained != sha {
			t.Fatalf("retained convergence ref %s = %q err=%v, want %q", ref, retained, err, sha)
		}
	}
	if err := executor.WithConvergenceEvidenceRefsLocked(ctx, firstEvidence, func(lockedCtx context.Context) error {
		return executor.WithConvergenceEvidenceRefsLocked(lockedCtx, firstEvidence, func(context.Context) error { return nil })
	}); err != nil {
		t.Fatalf("reenter the same convergence ref lock: %v", err)
	}
	installConvergenceProjection(t, runs, firstEvidence)
	if _, _, err := runs.HoldForConvergence(ctx, firstEvidence); err != nil {
		t.Fatalf("hold initial convergence evidence: %v", err)
	}
	activeEvidence, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || activeEvidence == nil {
		t.Fatalf("load initial convergence evidence: evidence=%+v err=%v", activeEvidence, err)
	}
	firstEvidence = *activeEvidence
	if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceRepairBranch, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: firstEvidence.Fingerprint,
	}); err != nil {
		t.Fatalf("choose repair disposition: %v", err)
	}
	otherChangeID := "ch-other-convergence-change"
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'task/other-change', 'main', ?, '2026-01-01T00:00:02Z', '2026-01-01T00:00:02Z', '2026-01-01T00:00:02Z')`,
		otherChangeID, task.ID, run.ID, firstEvidence.SourceHeadSHA); err != nil {
		t.Fatalf("insert newer unrelated task change: %v", err)
	}
	wrongConsole, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: &otherChangeID, Role: flowworker.RoleConsole,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload:        map[string]any{"change_id": otherChangeID, "branch": "task/other-change", "base": "main", "console_harness": "harness"},
	})
	if err != nil {
		t.Fatalf("queue unrelated task console: %v", err)
	}
	ensureInput := EnsureTaskConsoleJobInput{
		TaskID: task.ID, Harness: "shell", ConvergenceWorkflowRunID: run.ID,
		ConvergenceChangeID: firstEvidence.ChangeID, ConvergenceEvidenceFingerprint: firstEvidence.Fingerprint,
	}
	if _, err := sessions.EnsureTaskConsoleJob(ctx, ensureInput); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("attach convergence repair to unrelated console err = %v, want workflow conflict", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'canceled' WHERE id = ?`, wrongConsole.ID); err != nil {
		t.Fatalf("cancel unrelated task console: %v", err)
	}
	staleFingerprint := strings.Repeat("f", 64)
	convergenceChangeID := changeID
	if _, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: &convergenceChangeID, RequireHeldWorkflowRunID: &run.ID,
		RequireHeldWorkflowEvidenceFingerprint: &staleFingerprint, Role: flowworker.RoleConsole,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload:        map[string]any{"console_harness": "harness"},
	}); err == nil {
		t.Fatal("atomic convergence enqueue accepted a stale evidence fingerprint")
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = 'stale-projected-head' WHERE id = ?`, changeID); err != nil {
		t.Fatalf("move convergence change head projection: %v", err)
	}
	if _, err := sessions.EnsureTaskConsoleJob(ctx, ensureInput); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("queue repair console with stale change head err = %v, want workflow conflict", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = ?`, firstEvidence.SourceHeadSHA, changeID); err != nil {
		t.Fatalf("restore convergence change head projection: %v", err)
	}
	console, err := sessions.EnsureTaskConsoleJob(ctx, ensureInput)
	if err != nil {
		t.Fatalf("queue exact repair console: %v", err)
	}
	if console.Job.ChangeID == nil || *console.Job.ChangeID != firstEvidence.ChangeID {
		t.Fatalf("repair console change = %+v, want %q", console.Job.ChangeID, firstEvidence.ChangeID)
	}
	if console.Job.WorkflowRunID == nil || *console.Job.WorkflowRunID != run.ID {
		t.Fatalf("repair console workflow = %+v, want %q", console.Job.WorkflowRunID, run.ID)
	}
	if got := strings.TrimSpace(fmt.Sprint(console.Job.Payload["convergence_evidence_fingerprint"])); got != firstEvidence.Fingerprint {
		t.Fatalf("repair console fingerprint = %q, want %q", got, firstEvidence.Fingerprint)
	}
	if got := strings.TrimSpace(fmt.Sprint(console.Job.Payload["convergence_source_head_sha"])); got != firstEvidence.SourceHeadSHA {
		t.Fatalf("repair console source head = %q, want %q", got, firstEvidence.SourceHeadSHA)
	}
	now := time.Now().UTC()
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'claimed', updated_at = ? WHERE id = ?`,
		formatTime(now), console.Job.ID); err != nil {
		t.Fatalf("claim repair console: %v", err)
	}
	const leaseID = "l-convergence-repair"
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-repair', 'persistent_agent', ?, ?)`, leaseID, console.Job.ID,
		formatTime(now), formatTime(now.Add(time.Minute))); err != nil {
		t.Fatalf("lease repair console: %v", err)
	}
	if _, err := workers.MarkJobRunning(ctx, leaseID); err != nil {
		t.Fatalf("start repair console job: %v", err)
	}
	started, err := sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID: console.Job.ID, LeaseID: leaseID, WorkerID: "w-repair", Harness: "shell",
	})
	if err != nil {
		t.Fatalf("start repair console session: %v", err)
	}

	writeReconcileFile(t, fixture.repoPath, "repair.txt", "repaired branch observation\n")
	if err := runReconcileGit(fixture.repoPath, nil, "add", "repair.txt"); err != nil {
		t.Fatalf("stage repaired source change: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "repair source change"); err != nil {
		t.Fatalf("commit repaired source change: %v", err)
	}
	repairedHead, err := reconcileGitOutput(fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repaired source head: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "push", fixture.project.ExchangePath, "HEAD:refs/heads/"+branch); err != nil {
		t.Fatalf("push repaired source branch: %v", err)
	}
	stopped, err := sessions.StopConvergenceRepairConsole(ctx, task.ID)
	if err != nil {
		t.Fatalf("stop repair console: %v", err)
	}
	if !stopped.Active || stopped.Session == nil || stopped.Session.ID != started.Session.ID {
		t.Fatalf("stopped repair console = %+v, want active process-exit fence", stopped)
	}
	if _, err := credentials.Authenticate(ctx, started.Token); err == nil {
		t.Fatal("repair console token still authenticates after stop request")
	}
	if _, err := executor.RefreshConvergenceEvidence(ctx, task.ID); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("refresh while repair process may be live err = %v, want workflow conflict", err)
	}
	exited, err := sessions.MarkPersistentSessionExited(ctx, MarkPersistentSessionExitedInput{
		SessionID: started.Session.ID, LeaseID: leaseID, ExitCode: 0,
	})
	if err != nil {
		t.Fatalf("acknowledge repair console exit: %v", err)
	}
	if exited.RuntimeState != SessionFinished {
		t.Fatalf("exited repair console = %+v, want finished", exited)
	}
	projected, err := sessions.GetChange(ctx, changeID)
	if err != nil {
		t.Fatalf("load projected repaired change: %v", err)
	}
	if projected.HeadSHA != repairedHead {
		t.Fatalf("projected repaired head = %q, want %q", projected.HeadSHA, repairedHead)
	}

	refreshed, err := executor.RefreshConvergenceEvidence(ctx, task.ID)
	if err != nil {
		t.Fatalf("refresh repaired convergence evidence: %v", err)
	}
	if refreshed.SourceHeadSHA != repairedHead || refreshed.Fingerprint == firstEvidence.Fingerprint {
		t.Fatalf("refreshed evidence = %+v, want repaired head and a new fingerprint", refreshed)
	}
	ref := "refs/flow/convergence/objects/" + repairedHead
	if retained, err := reconcileGitOutput(fixture.repoPath, nil, "--git-dir", fixture.project.ExchangePath, "rev-parse", ref); err != nil || retained != repairedHead {
		t.Fatalf("retained repaired convergence ref %s = %q err=%v, want %q", ref, retained, err, repairedHead)
	}
	refreshedRun, err := runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("load refreshed workflow: %v", err)
	}
	refreshedNode, found, err := runs.GetNodeRun(ctx, nodeRun.ID)
	if err != nil || !found {
		t.Fatalf("load refreshed node: found=%t err=%v", found, err)
	}
	if refreshedRun.CurrentArtifactID == "" || refreshedNode.InputArtifactID != refreshedRun.CurrentArtifactID {
		t.Fatalf("refreshed artifact pointers: run=%q node=%q", refreshedRun.CurrentArtifactID, refreshedNode.InputArtifactID)
	}
	artifact, err := artifacts.Get(ctx, refreshedRun.CurrentArtifactID)
	if err != nil {
		t.Fatalf("load refreshed artifact: %v", err)
	}
	artifactChangeID, artifactHead, err := changeIdentityFromArtifact(artifact)
	if err != nil || artifactChangeID != changeID || artifactHead != repairedHead {
		t.Fatalf("refreshed artifact identity = %q/%q err=%v", artifactChangeID, artifactHead, err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = ?`, firstEvidence.SourceHeadSHA, changeID); err != nil {
		t.Fatalf("move change projection after refresh: %v", err)
	}
	if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceAcceptScope, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: refreshed.Fingerprint,
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("accept after projected head moved err = %v, want workflow conflict", err)
	}
	if stillHeld, err := runs.Get(ctx, run.ID); err != nil || !stillHeld.Held() {
		t.Fatalf("run after stale disposition = %+v err=%v, want held", stillHeld, err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = ?`, repairedHead, changeID); err != nil {
		t.Fatalf("restore repaired change projection: %v", err)
	}
	resolved, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceAcceptScope, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: refreshed.Fingerprint,
	})
	if err != nil {
		t.Fatalf("accept refreshed convergence evidence: %v", err)
	}
	if resolved.Run.Held() || resolved.Evidence.Fingerprint != refreshed.Fingerprint {
		t.Fatalf("accepted convergence result = %+v, want refreshed evidence and released hold", resolved)
	}
}

func TestReviewScopeThreshold(t *testing.T) {
	tests := []struct {
		name         string
		files        int
		lines        int
		wantExceeded bool
	}{
		{name: "within limit", files: 10, lines: 500},
		{name: "too many files", files: 11, lines: 1, wantExceeded: true},
		{name: "too many lines", files: 1, lines: 501, wantExceeded: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := reviewScopeExceeded(test.files, test.lines, 10, 500); got != test.wantExceeded {
				t.Fatalf("reviewScopeExceeded(%d, %d) = %t, want %t", test.files, test.lines, got, test.wantExceeded)
			}
		})
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
	err     error
	onMerge func()
}

func (s workflowMergeConflictStub) MergeChange(context.Context, string) (MergeResult, error) {
	if s.onMerge != nil {
		s.onMerge()
	}
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
		FlowID: "fl-merge-conflict", FlowName: "merge conflict", StartNode: "implement", TransitionBudget: 10,
		Nodes: []FlowNodeSnapshot{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
				Workspace: WorkspaceChange,
				Artifact:  ArtifactChange,
				Agent:     AgentDefSnapshot{ID: "ad-merge-author", Name: "author", Harness: "harness", Prompt: "Implement the task."},
			}}},
			{Key: "merge", Name: "Merge", Kind: NodeMergeChange, Config: FlowNodeSnapshotConfig{MergeChange: &MergeChangeNodeConfig{}}},
			{Key: "merged", Name: "Merged", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionMerged}}},
		},
		Edges: []FlowEdge{
			{From: "implement", Outcome: "completed", To: "merge"},
			{From: "merge", Outcome: "merged", To: "merged"},
			{From: "merge", Outcome: "conflict", To: "implement"},
		},
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
	required := true
	exitCode := 1
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID: task.ID, Name: AutoMergeCheckName, Kind: CheckKindCI,
		Required: &required, Verdict: CheckBlocked, ExitCode: &exitCode,
		Details:  AutoMergeConflictDetailsPrefix + " conflict from the prior merge visit",
		Reporter: "coordinator",
	}); err != nil {
		t.Fatalf("seed prior auto-merge conflict check: %v", err)
	}
	executor.merges = workflowMergeConflictStub{
		err: fmt.Errorf("squash merge task branch: %w", &flowgit.MergeConflictError{Output: conflictLog}),
		onMerge: func() {
			check, err := checks.GetCheck(ctx, task.ID, AutoMergeCheckName)
			if err != nil {
				t.Fatalf("load retired auto-merge check: %v", err)
			}
			if check.Required || check.Verdict != CheckSkipped || check.Details != "reset after new author revision" {
				t.Fatalf("auto-merge check before merge = %+v, want retired prior conflict", check)
			}
		},
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
		FlowID: "fl-concurrent-change", FlowName: "concurrent change", StartNode: "implement", TransitionBudget: 10,
		Nodes: []FlowNodeSnapshot{
			{
				Key: "implement", Name: "Implement", Kind: NodeAgent,
				Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
					Agent:     AgentDefSnapshot{ID: "ad-concurrent-author", Name: "author", Harness: "harness", Prompt: "Implement the task."},
					Workspace: WorkspaceChange,
					Artifact:  ArtifactChange,
				}},
			},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdge{{From: "implement", Outcome: "completed", To: "done"}},
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
	jobs, err := fixture.workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list workflow jobs: %v", err)
	}
	for _, job := range jobs {
		if job.NodeRunID != nil && *job.NodeRunID == nodeID && job.Selector[flowharness.AgentHarnessLabel(flowharness.Harness)] != "true" {
			t.Fatalf("workflow author selector = %#v, want runtime harness requirement", job.Selector)
		}
	}
}

func TestWorkflowExecutorParallelReviewAggregationBarrier(t *testing.T) {
	t.Run("fans out reviewers then queues one aggregate exactly once", func(t *testing.T) {
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
		if len(jobs) != 2 {
			t.Fatalf("jobs before reports = %+v, want two parallel reviewers", jobs)
		}
		for _, job := range jobs {
			if job.State != flowworker.JobQueued || job.Payload["review_discovery"] != true {
				t.Fatalf("parallel discovery job = %+v", job)
			}
		}

		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance after first report: %v", err)
		}
		node, found, err := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if err != nil || !found || node.State != WorkflowNodeRunning || node.Outcome != "" {
			t.Fatalf("node after first report = %+v found=%v err=%v", node, found, err)
		}
		jobs, err = fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs after first report: %v", err)
		}
		if len(jobs) != 2 {
			t.Fatalf("jobs after first report = %+v, want no aggregate yet", jobs)
		}

		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
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
			t.Fatalf("concurrent aggregation advance: %v", err)
		}
		jobs, err = fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs after discovery: %v", err)
		}
		aggregations := 0
		for _, job := range jobs {
			if payloadString(job.Payload, "check_name") == ReviewAggregationCheckName+".node."+fixture.nodeID {
				aggregations++
				entrypoint, _ := job.Payload["entrypoint"].(map[string]any)
				if job.Payload["review_discovery"] != nil || job.Payload["blocking"] != true ||
					payloadString(job.Payload, "role_instructions") != "Synthesize review reports." ||
					!strings.Contains(fmt.Sprint(entrypoint["argv"]), "gpt-5-mini") {
					t.Fatalf("aggregation payload = %+v", job.Payload)
				}
			}
		}
		if len(jobs) != 3 || aggregations != 1 {
			t.Fatalf("jobs after discovery = %+v, want one aggregate", jobs)
		}
		for _, source := range []string{
			"code-review.node." + fixture.nodeID,
			"security-review.node." + fixture.nodeID,
		} {
			check, err := fixture.checks.GetCheck(ctx, fixture.task.ID, source)
			if err != nil || check.Required {
				t.Fatalf("aggregated source %s = %+v err=%v, want advisory", source, check, err)
			}
		}
		fixture.report(t, ReviewAggregationCheckName+".node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("advance after aggregate: %v", err)
		}
		fixture.assertReviewOutcome(t, "approved")
	})

	t.Run("a blocked discovery source waits for the aggregate decision", func(t *testing.T) {
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
			t.Fatalf("schedule aggregate: %v", err)
		}
		node, _, _ = fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if node.State != WorkflowNodeRunning {
			t.Fatalf("aggregate was not awaited: %+v", node)
		}
		aggregation, err := fixture.checks.GetCheck(ctx, fixture.task.ID, ReviewAggregationCheckName+".node."+fixture.nodeID)
		if err != nil || !strings.Contains(aggregation.Details, "code-review") || !strings.Contains(aggregation.Details, "blocked") {
			t.Fatalf("aggregation context = %+v err=%v", aggregation, err)
		}
		fixture.report(t, ReviewAggregationCheckName+".node."+fixture.nodeID, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final aggregate advance: %v", err)
		}
		fixture.assertReviewOutcome(t, "changes_requested")
	})

	t.Run("an advisory discovery finding cannot block a satisfied aggregate", func(t *testing.T) {
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
			t.Fatalf("schedule aggregate: %v", err)
		}
		advisory, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "security-review.node."+fixture.nodeID)
		if err != nil || advisory.Required || advisory.Verdict != CheckBlocked {
			t.Fatalf("advisory result = %+v err=%v", advisory, err)
		}
		fixture.report(t, ReviewAggregationCheckName+".node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final aggregate advance: %v", err)
		}
		fixture.assertReviewOutcome(t, "approved")
	})

	t.Run("the aggregate decides how to handle a skipped discovery source", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSkipped)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("schedule aggregate: %v", err)
		}
		fixture.report(t, ReviewAggregationCheckName+".node."+fixture.nodeID, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("final aggregate advance: %v", err)
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

	t.Run("pauses on aggregation execution error and retries only the aggregate", func(t *testing.T) {
		fixture := newReviewBarrierFixture(t, barrierAgents(true))
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("schedule aggregation: %v", err)
		}
		aggregationName := ReviewAggregationCheckName + ".node." + fixture.nodeID
		fixture.report(t, aggregationName, CheckErrored)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("pause after aggregation error: %v", err)
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil || detail.Run.State != WorkflowRunWaiting ||
			detail.OpenWait == nil || detail.OpenWait.Reason != WorkflowWaitReasonExecutionFailed {
			t.Fatalf("workflow after aggregation error = %+v err=%v", detail, err)
		}
		if _, err := fixture.runs.RetryExecution(ctx, fixture.task.ID, ActorHuman, false); err != nil {
			t.Fatalf("retry aggregation: %v", err)
		}
		for _, sourceName := range []string{
			"code-review.node." + fixture.nodeID,
			"security-review.node." + fixture.nodeID,
		} {
			source, err := fixture.checks.GetCheck(ctx, fixture.task.ID, sourceName)
			if err != nil || source.Verdict != CheckSatisfied || source.Required {
				t.Fatalf("source after aggregation retry %s = %+v err=%v", sourceName, source, err)
			}
		}
		aggregation, err := fixture.checks.GetCheck(ctx, fixture.task.ID, aggregationName)
		if err != nil || aggregation.Verdict != CheckPending || aggregation.SourceJobID != nil {
			t.Fatalf("aggregate after retry = %+v err=%v", aggregation, err)
		}
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("enqueue aggregate retry: %v", err)
		}
		jobs, err := fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		attemptTwoJobs := 0
		for _, job := range jobs {
			if attempt, ok := job.Payload["node_attempt"].(float64); ok && int(attempt) == 2 {
				attemptTwoJobs++
				if job.Payload["check_name"] != aggregationName {
					t.Fatalf("retried wrong check: %+v", job.Payload)
				}
			}
		}
		if attemptTwoJobs != 1 {
			t.Fatalf("attempt-two jobs = %d, want one aggregation retry; jobs=%+v", attemptTwoJobs, jobs)
		}
	})

	t.Run("an all-advisory review aggregate cannot block", func(t *testing.T) {
		agents := barrierAgents(false)
		agents[0].Blocking = false
		fixture := newReviewBarrierFixture(t, agents)
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckBlocked)
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("schedule advisory aggregate: %v", err)
		}
		aggregationName := ReviewAggregationCheckName + ".node." + fixture.nodeID
		aggregation, err := fixture.checks.GetCheck(ctx, fixture.task.ID, aggregationName)
		if err != nil || aggregation.Required {
			t.Fatalf("all-advisory aggregation = %+v err=%v", aggregation, err)
		}
		fixture.report(t, aggregationName, CheckBlocked)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("complete advisory aggregate: %v", err)
		}
		fixture.assertReviewOutcome(t, "approved")
	})

	t.Run("an all-advisory aggregation execution error still pauses", func(t *testing.T) {
		agents := barrierAgents(false)
		agents[0].Blocking = false
		fixture := newReviewBarrierFixture(t, agents)
		ctx := context.Background()
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("initial advance: %v", err)
		}
		fixture.report(t, "code-review.node."+fixture.nodeID, CheckSatisfied)
		fixture.report(t, "security-review.node."+fixture.nodeID, CheckSatisfied)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("schedule advisory aggregate: %v", err)
		}
		aggregationName := ReviewAggregationCheckName + ".node." + fixture.nodeID
		aggregation, err := fixture.checks.GetCheck(ctx, fixture.task.ID, aggregationName)
		if err != nil || aggregation.Required {
			t.Fatalf("all-advisory aggregation = %+v err=%v", aggregation, err)
		}
		fixture.report(t, aggregationName, CheckErrored)
		if err := fixture.executor.Advance(ctx, fixture.runID); err != nil {
			t.Fatalf("pause after advisory aggregation error: %v", err)
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil || detail.Run.State != WorkflowRunWaiting ||
			detail.OpenWait == nil || detail.OpenWait.Reason != WorkflowWaitReasonExecutionFailed {
			t.Fatalf("workflow after advisory aggregation error = %+v err=%v", detail, err)
		}
	})

	t.Run("skips a failed review node along its successful outcome", func(t *testing.T) {
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
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id,
	output_artifact_id, outcome, created_at, started_at, completed_at
) VALUES ('nr-review-prior', ?, 'review', 2, 1, 'succeeded', ?, ?, 'approved',
	'2025-12-31T00:00:00Z', '2025-12-31T00:00:00Z', '2025-12-31T00:00:00Z')`,
			fixture.runID, "wa-review-barrier", "wa-review-barrier"); err != nil {
			t.Fatalf("insert prior review visit: %v", err)
		}
		if _, err := fixture.runs.SkipExecution(ctx, fixture.task.ID, "nr-review-prior", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
			t.Fatalf("stale skip error = %v, want workflow conflict", err)
		}
		paused, err := fixture.runs.Get(ctx, fixture.runID)
		if err != nil || paused.State != WorkflowRunWaiting || paused.CurrentNodeRunID != fixture.nodeID {
			t.Fatalf("workflow after stale skip = %+v err=%v, want original failed node", paused, err)
		}

		result, err := fixture.runs.SkipExecution(ctx, fixture.task.ID, fixture.nodeID, ActorHuman)
		if err != nil {
			t.Fatalf("skip failed review: %v", err)
		}
		if !result.Done || result.Run.State != WorkflowRunCompleted {
			t.Fatalf("skip result = %+v, want completed workflow", result)
		}
		node, found, err := fixture.runs.GetNodeRun(ctx, fixture.nodeID)
		if err != nil || !found || node.State != WorkflowNodeSucceeded || node.Outcome != "approved" {
			t.Fatalf("skipped node = %+v found=%v err=%v", node, found, err)
		}
		for _, name := range []string{"code-review.node." + fixture.nodeID, "security-review.node." + fixture.nodeID} {
			check, err := fixture.checks.GetCheck(ctx, fixture.task.ID, name)
			if err != nil || check.Required || check.Verdict != CheckSkipped || !strings.Contains(check.Details, "Skipped with the workflow step") {
				t.Fatalf("retired check %s = %+v err=%v", name, check, err)
			}
		}
		waiver, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "workflow-step-skipped.node."+fixture.nodeID)
		if err != nil || !waiver.Required || waiver.Verdict != CheckSatisfied {
			t.Fatalf("skip waiver = %+v err=%v, want required satisfied check", waiver, err)
		}
		if state, err := fixture.checks.ReviewState(ctx, fixture.task.ID); err != nil || state != ReviewApproved {
			t.Fatalf("review state after skip = %s err=%v, want approved", state, err)
		}
		jobs, err := fixture.workers.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list skipped node jobs: %v", err)
		}
		for _, job := range jobs {
			if job.NodeRunID != nil && *job.NodeRunID == fixture.nodeID && job.State != flowworker.JobCanceled {
				t.Fatalf("skipped node job = %+v, want canceled", job)
			}
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("load skipped workflow: %v", err)
		}
		if detail.OpenWait != nil {
			t.Fatalf("skip left open wait: %+v", detail.OpenWait)
		}
		foundSkip := false
		for _, transition := range detail.Transitions {
			if transition.EventKind == "node_skipped" && transition.FromNodeKey == "review" && transition.Outcome == "approved" {
				foundSkip = true
				var payload struct {
					RetiredChecks int64 `json:"retired_checks"`
					CancelledJobs int64 `json:"cancelled_jobs"`
				}
				if err := json.Unmarshal(transition.Payload, &payload); err != nil {
					t.Fatalf("decode skip transition payload: %v", err)
				}
				if payload.RetiredChecks != 2 || payload.CancelledJobs != 2 {
					t.Fatalf("skip transition payload = %+v, want two retired checks and canceled jobs", payload)
				}
				break
			}
		}
		if !foundSkip {
			t.Fatalf("workflow transitions = %+v, want node_skipped audit event", detail.Transitions)
		}
		replayed, err := fixture.runs.SkipExecution(ctx, fixture.task.ID, fixture.nodeID, ActorHuman)
		if err != nil || !replayed.Replayed || !replayed.Done {
			t.Fatalf("duplicate skip = %+v err=%v, want completed replay", replayed, err)
		}
	})

	t.Run("rejects skip after the pinned change head moves", func(t *testing.T) {
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

		if _, err := fixture.runs.SkipExecution(ctx, fixture.task.ID, fixture.nodeID, ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
			t.Fatalf("skip error = %v, want workflow conflict", err)
		}
		detail, err := fixture.runs.Detail(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("load still-paused workflow: %v", err)
		}
		if detail.Run.State != WorkflowRunWaiting || detail.OpenWait == nil || detail.OpenWait.NodeRunID != fixture.nodeID {
			t.Fatalf("workflow after rejected skip = %+v, want original wait", detail)
		}
		check, err := fixture.checks.GetCheck(ctx, fixture.task.ID, "code-review.node."+fixture.nodeID)
		if err != nil || !check.Required || check.Verdict != CheckErrored {
			t.Fatalf("failed check after rejected skip = %+v err=%v, want required errored check", check, err)
		}
		var waivers int
		if err := fixture.db.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM checks WHERE task_id = ? AND name = ?`,
			fixture.task.ID, "workflow-step-skipped.node."+fixture.nodeID).Scan(&waivers); err != nil {
			t.Fatalf("count skip waivers: %v", err)
		}
		if waivers != 0 {
			t.Fatalf("skip waivers = %d, want none", waivers)
		}
		for _, transition := range detail.Transitions {
			if transition.EventKind == "node_skipped" {
				t.Fatalf("workflow transitions = %+v, want no node_skipped event", detail.Transitions)
			}
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
		consoleJob, err := fixture.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			TaskID: &fixture.task.ID, Role: flowworker.RoleConsole, CapacityBucket: flowworker.BucketPersistentAgent,
			Payload: map[string]any{"console_harness": "harness"},
		})
		if err != nil {
			t.Fatalf("enqueue task console before reset: %v", err)
		}
		now := time.Now().UTC()
		if _, err := fixture.db.DB().ExecContext(ctx, `UPDATE jobs SET state = 'claimed', updated_at = ? WHERE id = ?`,
			formatTime(now), consoleJob.ID); err != nil {
			t.Fatalf("claim task console before reset: %v", err)
		}
		const leaseID = "l-reset-task-console"
		if _, err := fixture.db.DB().ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-reset', 'persistent_agent', ?, ?)`, leaseID, consoleJob.ID,
			formatTime(now), formatTime(now.Add(time.Minute))); err != nil {
			t.Fatalf("lease task console before reset: %v", err)
		}
		if _, err := fixture.workers.MarkJobRunning(ctx, leaseID); err != nil {
			t.Fatalf("start task console before reset: %v", err)
		}
		const sessionID = "s-reset-task-console"
		if _, err := fixture.db.DB().ExecContext(ctx, `
INSERT INTO sessions (
	id, task_id, job_id, lease_id, worker_id, role, workspace_mode, runtime_state,
	branch, base, harness, token_hash, created_at, updated_at
) VALUES (?, ?, ?, ?, 'w-reset', 'console', 'change', 'working',
	'task/reset', 'main', 'shell', 'reset-token-hash', ?, ?)`, sessionID, fixture.task.ID,
			consoleJob.ID, leaseID, formatTime(now), formatTime(now)); err != nil {
			t.Fatalf("insert task console before reset: %v", err)
		}
		if _, err := fixture.runs.Reset(ctx, fixture.task.ID, ActorHuman); err != nil {
			t.Fatalf("reset workflow after rejected retry: %v", err)
		}
		resetJob, err := fixture.workers.GetJob(ctx, consoleJob.ID)
		if err != nil || resetJob.State != flowworker.JobCanceled {
			t.Fatalf("task console job after reset = %+v err=%v, want canceled", resetJob, err)
		}
		resetLease, err := fixture.workers.GetLease(ctx, leaseID)
		if err != nil || resetLease.ReleasedAt == nil {
			t.Fatalf("task console lease after reset = %+v err=%v, want released", resetLease, err)
		}
		var runtimeState string
		if err := fixture.db.DB().QueryRowContext(ctx, `SELECT runtime_state FROM sessions WHERE id = ?`, sessionID).Scan(&runtimeState); err != nil {
			t.Fatalf("load task console session after reset: %v", err)
		}
		if runtimeState != string(SessionAbandoned) {
			t.Fatalf("task console session state after reset = %q, want abandoned", runtimeState)
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
