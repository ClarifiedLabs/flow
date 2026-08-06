package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

type scopeDecisionFixture struct {
	runs      *WorkflowRunService
	run       WorkflowRun
	node      WorkflowNodeRun
	task      Task
	change    Change
	checkName string
	job       flowworker.Job
	leaseID   string
	report    checkverdict.VerdictReport
}

func newScopeDecisionFixture(t *testing.T) scopeDecisionFixture {
	t.Helper()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agents := NewAgentDefService(flows.db)
	author, err := agents.Create(ctx, AgentDefInput{Name: "scope author", Harness: "harness", Prompt: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := agents.Create(ctx, AgentDefInput{Name: "scope reviewer", Harness: "harness", Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name: "scope decision flow", StartNode: "implement", TransitionBudget: 20,
		Nodes: []FlowNodeInput{
			{Key: "review", Name: "Review", Kind: NodeChangeReview, Config: FlowNodeConfig{ChangeReview: &ChangeReviewNodeConfig{
				Agents: []ReviewAgentConfig{{AgentDefID: reviewer.ID}}, AggregatorAgentDefID: reviewer.ID,
			}}},
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{
				AgentDefID: author.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange,
			}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{
			{From: "review", Outcome: "changes_requested", To: "implement"},
			{From: "review", Outcome: "approved", To: "done"},
			{From: "implement", Outcome: "completed", To: "review"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Decide inferred scope", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorNode, _, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.MarkNodeRunning(ctx, authorNode.ID); err != nil {
		t.Fatal(err)
	}
	advanced, err := runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: authorNode.ID, Outcome: "completed", Actor: ActorSystem, OperatorSatisfied: true,
	})
	if err != nil || advanced.Next == nil {
		t.Fatalf("advance to review = %+v, err=%v", advanced, err)
	}
	node := *advanced.Next
	if _, err := runs.MarkNodeRunning(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change := Change{ID: "ch-scope", TaskID: task.ID, Branch: "task/scope", Base: "main", HeadSHA: "head-one", CreatedAt: now, UpdatedAt: now}
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, change.ID, task.ID, run.ID, change.Branch, change.Base, change.HeadSHA, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	payload, err := canonicalArtifactPayload(ArtifactChange, json.RawMessage(`{"change_id":"ch-scope","head_sha":"head-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	const artifactID = "wa-scope"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO workflow_artifacts
	(id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown, payload_json, payload_sha256, base_revision, client_key, created_at)
VALUES (?, ?, ?, 'test:scope', 'change', 'scope change', ?, ?, 'base-one', 'scope-one', ?)`,
		artifactID, run.ID, node.ID, string(payload), artifactDigest(ArtifactChange, "scope change", payload, "base-one"), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.db.ExecContext(ctx, `UPDATE workflow_node_runs SET input_artifact_id = ? WHERE id = ?`, artifactID, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.db.ExecContext(ctx, `UPDATE workflow_runs SET current_artifact_id = ? WHERE id = ?`, artifactID, run.ID); err != nil {
		t.Fatal(err)
	}
	discoveryName := "reviewer.node." + node.ID
	checkName := ReviewAggregationCheckName + ".node." + node.ID
	for _, check := range []struct {
		name, verdict, details string
		required               int
	}{
		{name: discoveryName, verdict: string(CheckSatisfied), details: `{"verdict":"satisfied"}`, required: 1},
		{name: checkName, verdict: string(CheckPending), details: ReviewAggregationDetailsPrefix, required: 1},
	} {
		if _, err := runs.db.ExecContext(ctx, `
INSERT INTO checks (task_id, name, kind, required, verdict, details, created_at, updated_at)
VALUES (?, ?, 'reviewer', ?, ?, ?, ?, ?)`, task.ID, check.name, check.required, check.verdict, check.details, formatTime(now), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	queue := flowworker.NewService(runs.db)
	job, err := queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: &change.ID, WorkflowRunID: &run.ID, NodeRunID: &node.ID,
		Role: flowworker.RoleReviewer, CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{"review_aggregation": true, "check_name": checkName, "head_sha": change.HeadSHA, "blocking": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.db.ExecContext(ctx, `UPDATE jobs SET state = 'running' WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	leaseID := "l-scope"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-scope', 'ephemeral', ?, ?)`, leaseID, job.ID, formatTime(now), formatTime(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.db.ExecContext(ctx, `UPDATE checks SET source_job_id = ? WHERE task_id = ? AND name = ?`, job.ID, task.ID, checkName); err != nil {
		t.Fatal(err)
	}
	introduced := true
	report := checkverdict.VerdictReport{
		Verdict: "blocked", Reason: "scope decision",
		Comments: []checkverdict.ReviewCommentReport{{
			SHA: change.HeadSHA, File: "main.go", Line: 3, Body: "change every caller", Severity: "high",
			IntroducedByChange: &introduced, Requirement: "inferred consistency", RequirementSource: "inferred",
			FindingBasis: "scope_inference", RemediationScope: "cross_cutting", ScopeRationale: "all callers share the contract",
		}},
		DecisionRequest: &checkverdict.ReviewDecisionRequest{
			Key: "caller.contract", Question: "Should every caller change in this task?",
			Rationale: "The remediation crosses package boundaries.", CommentIndexes: []int{0},
		},
	}
	return scopeDecisionFixture{runs: runs, run: run, node: node, task: task, change: change, checkName: checkName, job: job, leaseID: leaseID, report: report}
}

func (f scopeDecisionFixture) open(t *testing.T) WorkflowWait {
	t.Helper()
	result, err := f.runs.RequestReviewScopeDecision(context.Background(), RequestReviewScopeDecisionInput{
		TaskID: f.task.ID, CheckName: f.checkName, LeaseID: f.leaseID, SourceJobID: f.job.ID, WorkerID: "w-scope", Report: f.report,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Wait
}

func TestReviewScopeDecisionPausesAndRestartsAggregationOnly(t *testing.T) {
	f := newScopeDecisionFixture(t)
	wait := f.open(t)
	if wait.Kind != WorkflowWaitReviewScopeDecision {
		t.Fatalf("wait = %+v", wait)
	}
	details, err := ParseReviewScopeDecisionWaitDetails(wait.Details)
	if err != nil || details.DecisionKey != "caller.contract" || details.SourceHeadSHA != f.change.HeadSHA {
		t.Fatalf("details = %+v, err=%v", details, err)
	}
	result, err := f.runs.ResolveReviewScopeDecision(context.Background(), ResolveReviewScopeDecisionInput{
		TaskID: f.task.ID, WaitID: wait.ID, Choice: ReviewScopeFixInTask,
		Guidance: "Keep the compatibility surface small.", Actor: ActorHuman, IdempotencyKey: "resolve-scope-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "resolved" || result.Ruling == nil || result.Ruling.Decision == nil || result.Ruling.Decision.Choice != string(ReviewScopeFixInTask) {
		t.Fatalf("resolution = %+v", result)
	}
	if result.Run.State != WorkflowRunRunning {
		t.Fatalf("run state = %s", result.Run.State)
	}
	var verdict string
	var sourceJobID *string
	if err := f.runs.db.QueryRow(`SELECT verdict, source_job_id FROM checks WHERE task_id = ? AND name = ?`, f.task.ID, f.checkName).Scan(&verdict, &sourceJobID); err != nil {
		t.Fatal(err)
	}
	if verdict != string(CheckPending) || sourceJobID != nil {
		t.Fatalf("aggregation check verdict=%q source=%v", verdict, sourceJobID)
	}
	node, ok, err := f.runs.GetNodeRun(context.Background(), f.node.ID)
	if err != nil || !ok || node.Attempt != 2 || node.State != WorkflowNodeQueued {
		t.Fatalf("node = %+v, ok=%v, err=%v", node, ok, err)
	}
}

func TestReviewScopeDecisionStaleHeadRestartsFullDiscoveryWithoutRuling(t *testing.T) {
	f := newScopeDecisionFixture(t)
	wait := f.open(t)
	if _, err := f.runs.db.Exec(`UPDATE changes SET head_sha = 'head-two' WHERE id = ?`, f.change.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := f.runs.ReconcileReviewScopeDecisionHeads(context.Background())
	if err != nil || len(restarted) != 1 || restarted[0] != f.run.ID {
		t.Fatalf("restarted = %v, err=%v", restarted, err)
	}
	detail, err := f.runs.Detail(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.OpenWait != nil || len(detail.ActiveRulings) != 0 {
		t.Fatalf("detail after stale restart = %+v", detail)
	}
	var pending int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE task_id = ? AND name LIKE ? AND verdict = 'pending' AND source_job_id IS NULL`,
		f.task.ID, "%.node."+f.node.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("pending reset checks = %d, want 2", pending)
	}
	var staleEvents int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM workflow_transitions WHERE workflow_run_id = ? AND event_kind = 'workflow_review_scope_decision_stale'`, f.run.ID).Scan(&staleEvents); err != nil {
		t.Fatal(err)
	}
	if staleEvents != 1 {
		t.Fatalf("stale events = %d", staleEvents)
	}
	if _, err := f.runs.ResolveReviewScopeDecision(context.Background(), ResolveReviewScopeDecisionInput{
		TaskID: f.task.ID, WaitID: wait.ID, Choice: ReviewScopeFixInTask, Actor: ActorHuman, IdempotencyKey: "too-late",
	}); !errors.Is(err, ErrWorkflowConflict) && !errors.Is(err, sql.ErrNoRows) {
		// The run is no longer waiting; either conflict/no-row is acceptable at
		// this low-level boundary, and both map to a non-success API response.
		t.Fatalf("late resolution error = %v", err)
	}
}

func TestConvergenceReturnToAuthorRecordsRulingAndTakesSendBackEdge(t *testing.T) {
	f := newScopeDecisionFixture(t)
	ctx := context.Background()
	evidence := ConvergenceEvidence{
		SchemaVersion: ConvergenceEvidenceSchemaVersion,
		WorkflowRunID: f.run.ID, NodeRunID: f.node.ID, ChangeID: f.change.ID, TaskID: f.task.ID,
		SourceBranch: f.change.Branch, SourceHeadSHA: f.change.HeadSHA,
		TargetBaseBranch: f.change.Base, TargetBaseTipSHA: "base-tip", MergeBaseSHA: "merge-base",
		DiffDigest: "sha256:change", Files: 2, Additions: 20, Deletions: 3, MaxFiles: 10, MaxLines: 500,
	}
	if _, held, err := f.runs.HoldForConvergence(ctx, evidence); err != nil || !held {
		t.Fatalf("hold = %v, err=%v", held, err)
	}
	active, err := f.runs.ActiveConvergenceEvidenceForTask(ctx, f.task.ID)
	if err != nil || active == nil {
		t.Fatalf("active evidence = %+v, err=%v", active, err)
	}
	result, err := f.runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: f.task.ID, Disposition: ConvergenceReturnAuthor,
		Note: "Keep the cross-package fix in this task and preserve the public contract.", Actor: ActorHuman,
		ExpectedEvidenceFingerprint: active.Fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ruling == nil || result.Ruling.Source != OwnerRulingSourceConvergenceReturn || result.Run.CurrentNodeKey != "implement" {
		t.Fatalf("return result = %+v", result)
	}
	if result.Run.Held() || result.Run.ReviewCyclesUsed != 1 {
		t.Fatalf("returned run = %+v", result.Run)
	}
	detail, err := f.runs.Detail(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ActiveRulings) != 1 || detail.ActiveRulings[0].RulingID != result.Ruling.RulingID {
		t.Fatalf("active rulings = %+v", detail.ActiveRulings)
	}
}
