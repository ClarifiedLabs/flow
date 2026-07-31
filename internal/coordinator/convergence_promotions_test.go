package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// newPromotionTestEnv wires a real-git project fixture with the services the
// convergence promotion path needs, mirroring the registry wiring.
func newPromotionTestEnv(t *testing.T) (*featureTestEnv, *WorkflowExecutor, *SessionService) {
	t.Helper()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	credentials := NewCredentialService(globalStore.DB())
	workers := flowworker.NewService(env.fixture.store.DB())
	sessions := NewSessionServiceWithOptions(env.fixture.store.DB(), env.tasks, workers, SessionServiceOptions{
		Credentials: credentials,
		Project:     env.fixture.project,
	})
	artifacts := NewWorkflowArtifactService(env.fixture.store.DB(), env.tasks)
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: env.fixture.store.DB(), Runs: env.runs, Artifacts: artifacts,
		Tasks: env.tasks, Features: env.features, Sessions: sessions,
		Queue: workers, Project: env.fixture.project,
	})
	return env, executor, sessions
}

// holdRealConvergenceReview drives a hold-fixture task to a system convergence
// hold backed by a real commit on the fixture exchange, returning the task and
// the active evidence fingerprint.
func holdRealConvergenceReview(t *testing.T, env *featureTestEnv, executor *WorkflowExecutor) (Task, ConvergenceEvidence) {
	t.Helper()
	ctx := context.Background()
	task, run, nodeRun := newHoldFlow(t, env.flows, env.tasks, env.runs)

	branch := "task/" + task.ID
	if err := runReconcileGit(env.fixture.repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("create source branch: %v", err)
	}
	writeReconcileFile(t, env.fixture.repoPath, "oversized.txt", "a change far beyond the scope budget\n")
	if err := runReconcileGit(env.fixture.repoPath, nil, "add", "oversized.txt"); err != nil {
		t.Fatalf("stage source change: %v", err)
	}
	if err := runReconcileGit(env.fixture.repoPath, nil, "commit", "-m", "oversized source change"); err != nil {
		t.Fatalf("commit source change: %v", err)
	}
	if err := runReconcileGit(env.fixture.repoPath, nil, "push", env.fixture.project.ExchangePath, "HEAD:refs/heads/"+branch); err != nil {
		t.Fatalf("push source branch: %v", err)
	}
	headSHA, err := reconcileGitOutput(env.fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve source head: %v", err)
	}

	changeID := "ch-promote-" + task.ID
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, 'main', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID, run.ID, branch, headSHA); err != nil {
		t.Fatalf("insert source change: %v", err)
	}
	change, err := executor.sessions.GetChange(ctx, changeID)
	if err != nil {
		t.Fatalf("load source change: %v", err)
	}
	evidence, err := executor.convergenceEvidenceForChange(ctx, run, nodeRun, change)
	if err != nil {
		t.Fatalf("capture convergence evidence: %v", err)
	}
	installConvergenceProjection(t, env.runs, evidence)
	if _, _, err := env.runs.HoldForConvergence(ctx, evidence); err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}
	active, err := env.runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || active == nil {
		t.Fatalf("load active convergence evidence: evidence=%+v err=%v", active, err)
	}
	return task, *active
}

func promoteInput(taskID string, evidence ConvergenceEvidence) ResolveConvergenceReviewInput {
	return ResolveConvergenceReviewInput{
		TaskID: taskID, Disposition: ConvergencePromote, Actor: ActorHuman,
		Note:                        "superseded by a feature planning workflow",
		ExpectedEvidenceFingerprint: evidence.Fingerprint,
	}
}

func TestPromoteConvergenceReviewCreatesCleanBasePlanningWorkflow(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	sourceHead := evidence.SourceHeadSHA
	baseTip, ok, err := flowgit.BranchTip(ctx, env.exchangePath(), evidence.TargetBaseBranch)
	if err != nil || !ok {
		t.Fatalf("resolve base tip: tip=%s ok=%t err=%v", baseTip, ok, err)
	}
	if sourceHead == baseTip || evidence.TargetBaseTipSHA != baseTip {
		t.Fatalf("fixture needs a source head distinct from the base tip: head=%s base=%s evidence_base=%s", sourceHead, baseTip, evidence.TargetBaseTipSHA)
	}

	result, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence))
	if err != nil {
		t.Fatalf("promote convergence review: %v", err)
	}
	if result.Disposition != ConvergencePromote {
		t.Fatalf("disposition = %q, want promote", result.Disposition)
	}
	if result.Feature == nil || result.PlanningTask == nil || result.PlanningRun == nil {
		t.Fatalf("promotion result = %+v, want feature/planning task/planning run", result)
	}

	// The feature is seeded at the exact reviewed base tip, never the oversized
	// source head.
	featureRef := "feature/" + result.Feature.ID
	if tip, ok, err := flowgit.BranchTip(ctx, env.exchangePath(), featureRef); err != nil || !ok || tip != evidence.TargetBaseTipSHA {
		t.Fatalf("feature branch %s tip = %s ok=%t err=%v, want %s", featureRef, tip, ok, err, evidence.TargetBaseTipSHA)
	}
	if result.Feature.Branch != featureRef || result.Feature.Status != FeatureOpen {
		t.Fatalf("feature = %+v, want branch %s open", result.Feature, featureRef)
	}
	if result.Feature.Title != task.Title {
		t.Fatalf("feature title = %q, want %q", result.Feature.Title, task.Title)
	}

	// The planning task belongs to the built-in planning flow and the new
	// feature, and carries the promotion lineage.
	planningFlowID, err := env.flows.flowIDByName(ctx, PlanningFlowName)
	if err != nil || planningFlowID == "" {
		t.Fatalf("planning flow id = %q err=%v", planningFlowID, err)
	}
	planningTask := *result.PlanningTask
	if planningTask.FlowID != planningFlowID {
		t.Fatalf("planning task flow = %q, want %q", planningTask.FlowID, planningFlowID)
	}
	if planningTask.FeatureID == nil || *planningTask.FeatureID != result.Feature.ID {
		t.Fatalf("planning task feature = %v, want %s", planningTask.FeatureID, result.Feature.ID)
	}
	if planningTask.SourceTaskID == nil || *planningTask.SourceTaskID != task.ID {
		t.Fatalf("planning task source_task_id = %v, want %s", planningTask.SourceTaskID, task.ID)
	}
	if planningTask.SourceChangeID == nil || *planningTask.SourceChangeID != evidence.ChangeID {
		t.Fatalf("planning task source_change_id = %v, want %s", planningTask.SourceChangeID, evidence.ChangeID)
	}
	if planningTask.State == nil || *planningTask.State != LifecycleScheduled {
		t.Fatalf("planning task state = %v, want scheduled", planningTask.State)
	}
	if result.PlanningRun.TaskID != planningTask.ID {
		t.Fatalf("planning run task = %q, want %q", result.PlanningRun.TaskID, planningTask.ID)
	}

	// The oversized source task is superseded with convergence evidence intact.
	sourceTask, err := env.tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load source task: %v", err)
	}
	if sourceTask.State == nil || *sourceTask.State != LifecycleDone {
		t.Fatalf("source task state = %v, want done", sourceTask.State)
	}
	sourceRun, err := env.runs.Get(ctx, evidence.WorkflowRunID)
	if err != nil {
		t.Fatalf("load source run: %v", err)
	}
	if sourceRun.State != WorkflowRunCompleted {
		t.Fatalf("source run state = %q, want completed", sourceRun.State)
	}
	if evidence.TaskID != task.ID || evidence.ChangeID == "" {
		t.Fatalf("stored evidence = %+v", evidence)
	}

	// The promotion record is durable, completed, and references the planning run.
	var state, planningRunID, completedAt string
	var rows int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*), MAX(state), MAX(COALESCE(planning_workflow_run_id, '')), MAX(COALESCE(completed_at, ''))
FROM convergence_promotions WHERE source_task_id = ?`, task.ID).Scan(&rows, &state, &planningRunID, &completedAt); err != nil {
		t.Fatalf("read promotion record: %v", err)
	}
	if rows != 1 || state != convergencePromotionCompleted || planningRunID != result.PlanningRun.ID || completedAt == "" {
		t.Fatalf("promotion record rows=%d state=%q planning_run=%q completed_at=%q", rows, state, planningRunID, completedAt)
	}

	// The parent relation records the supersession.
	var relationCount int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM task_relations
WHERE source_task_id = ? AND target_task_id = ? AND kind = ?`, task.ID, planningTask.ID, string(RelationParentOf)).Scan(&relationCount); err != nil {
		t.Fatalf("read promotion relation: %v", err)
	}
	if relationCount != 1 {
		t.Fatalf("parent_of relations = %d, want 1", relationCount)
	}

	// The resolution transition carries the promotion identity.
	transitions, err := env.runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	foundResolution := false
	for _, transition := range transitions {
		if transition.EventKind != "workflow_convergence_review_resolved" {
			continue
		}
		var payload convergenceResolutionPayload
		if err := json.Unmarshal(transition.Payload, &payload); err != nil {
			t.Fatalf("decode resolution payload: %v", err)
		}
		if payload.Disposition == ConvergencePromote && payload.FeatureID == result.Feature.ID &&
			payload.PlanningTaskID == planningTask.ID && payload.EvidenceFingerprint == evidence.Fingerprint {
			foundResolution = true
		}
	}
	if !foundResolution {
		t.Fatal("promotion resolution transition is missing")
	}
}

func TestPromoteConvergenceReviewReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	first, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence))
	if err != nil {
		t.Fatalf("first promote: %v", err)
	}
	t.Logf("first promote result: feature=%+v planning_task=%+v planning_run=%+v", first.Feature, first.PlanningTask, first.PlanningRun)
	if first.PlanningRun == nil || first.PlanningRun.ID == "" {
		t.Fatalf("first promote planning run = %+v, want a run id", first.PlanningRun)
	}
	replayed, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence))
	if err != nil {
		t.Fatalf("replayed promote: %v", err)
	}
	if replayed.Feature == nil || replayed.Feature.ID != first.Feature.ID {
		t.Fatalf("replay feature = %+v, want %s", replayed.Feature, first.Feature.ID)
	}
	if replayed.PlanningTask == nil || replayed.PlanningTask.ID != first.PlanningTask.ID {
		t.Fatalf("replay planning task = %+v, want %s", replayed.PlanningTask, first.PlanningTask.ID)
	}
	if replayed.PlanningRun == nil || replayed.PlanningRun.ID != first.PlanningRun.ID {
		t.Fatalf("replay planning run = %+v, want %s", replayed.PlanningRun, first.PlanningRun.ID)
	}

	var features, tasks, promotions, runs int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM features`).Scan(&features); err != nil {
		t.Fatalf("count features: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM convergence_promotions`).Scan(&promotions); err != nil {
		t.Fatalf("count promotions: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM tasks t JOIN convergence_promotions p ON p.planning_task_id = t.id`).Scan(&tasks); err != nil {
		t.Fatalf("count planning tasks: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, first.PlanningTask.ID).Scan(&runs); err != nil {
		t.Fatalf("count planning runs: %v", err)
	}
	if features != 1 || promotions != 1 || tasks != 1 || runs != 1 {
		t.Fatalf("replay duplicated state: features=%d promotions=%d planning_tasks=%d planning_runs=%d", features, promotions, tasks, runs)
	}
}

func TestPromoteConvergenceReviewRejectsStaleEvidence(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	input := promoteInput(task.ID, evidence)
	input.ExpectedEvidenceFingerprint = "sha256:stale"
	if _, err := executor.PromoteConvergenceReview(ctx, input); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("stale promote error = %v, want workflow conflict", err)
	}
	var promotions, features int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM convergence_promotions`).Scan(&promotions); err != nil {
		t.Fatalf("count promotions: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM features`).Scan(&features); err != nil {
		t.Fatalf("count features: %v", err)
	}
	if promotions != 0 || features != 0 {
		t.Fatalf("stale promote mutated state: promotions=%d features=%d", promotions, features)
	}
	sourceTask, err := env.tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load source task: %v", err)
	}
	if sourceTask.State != nil && *sourceTask.State == LifecycleDone {
		t.Fatal("stale promote completed the source task")
	}

	// The same hold can still be promoted against the live fingerprint.
	if _, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence)); err != nil {
		t.Fatalf("promote after stale rejection: %v", err)
	}
}

func TestResumeConvergencePromotionsRepairsInterruptedIntent(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	// Simulate a crash after the durable intent committed but after the feature
	// ref was already seeded: state stays prepared and the ref exists. The
	// prepared intent also persists the feature and planning task rows so the
	// foreign keys resolve on restart.
	input := promoteInput(task.ID, evidence)
	if _, err := executor.prepareConvergencePromotion(ctx, input, evidence); err != nil {
		t.Fatalf("prepare promotion intent: %v", err)
	}
	var promotion convergencePromotion
	var found bool
	var err error
	if promotion, found, err = executor.loadConvergencePromotion(ctx, task.ID); err != nil || !found {
		t.Fatalf("load prepared promotion: found=%t err=%v", found, err)
	}
	if promotion.State != convergencePromotionPrepared {
		t.Fatalf("promotion state = %q, want prepared", promotion.State)
	}
	featureRef := "refs/heads/feature/" + promotion.FeatureID
	if err := flowgit.CreateOrVerifyRef(ctx, env.exchangePath(), featureRef, evidence.TargetBaseTipSHA); err != nil {
		t.Fatalf("seed feature ref before crash: %v", err)
	}
	var featureRows int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM features`).Scan(&featureRows); err != nil {
		t.Fatalf("count features: %v", err)
	}
	if featureRows != 1 {
		t.Fatalf("feature rows = %d, want 1 from the prepared intent", featureRows)
	}

	if err := executor.ResumeConvergencePromotions(ctx); err != nil {
		t.Fatalf("resume promotions: %v", err)
	}
	if promotion, found, err = executor.loadConvergencePromotion(ctx, task.ID); err != nil || !found {
		t.Fatalf("reload promotion after resume: found=%t err=%v", found, err)
	}
	if promotion.State != convergencePromotionCompleted || promotion.PlanningWorkflowRunID == "" {
		t.Fatalf("promotion after resume = %+v, want completed with planning run", promotion)
	}
	planningRun, err := env.runs.Get(ctx, promotion.PlanningWorkflowRunID)
	if err != nil {
		t.Fatalf("load planning run: %v", err)
	}
	if planningRun.TaskID != promotion.PlanningTaskID {
		t.Fatalf("planning run task = %q, want %q", planningRun.TaskID, promotion.PlanningTaskID)
	}

	// A crash after materialization but before scheduling completes: replay must
	// adopt the existing planning run instead of scheduling a duplicate.
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
UPDATE convergence_promotions
SET state = 'materialized', planning_workflow_run_id = NULL, completed_at = NULL
WHERE source_task_id = ?`, task.ID); err != nil {
		t.Fatalf("rewind promotion to materialized: %v", err)
	}
	if err := executor.ResumeConvergencePromotions(ctx); err != nil {
		t.Fatalf("resume materialized promotion: %v", err)
	}
	if promotion, found, err = executor.loadConvergencePromotion(ctx, task.ID); err != nil || !found {
		t.Fatalf("reload promotion after second resume: found=%t err=%v", found, err)
	}
	if promotion.State != convergencePromotionCompleted || promotion.PlanningWorkflowRunID != planningRun.ID {
		t.Fatalf("promotion after materialized replay = %+v, want completed with run %s", promotion, planningRun.ID)
	}
	var planningRuns int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, promotion.PlanningTaskID).Scan(&planningRuns); err != nil {
		t.Fatalf("count planning runs: %v", err)
	}
	if planningRuns != 1 {
		t.Fatalf("planning runs = %d, want 1", planningRuns)
	}
}

func TestPromoteConvergenceReviewRequiresActiveHeldRun(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	// A hold resolved without promotion (accept_scope) releases the run; a
	// later promote must fail without touching state.
	if _, err := env.runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceAcceptScope, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: evidence.Fingerprint,
	}); err != nil {
		t.Fatalf("accept convergence scope: %v", err)
	}
	if _, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence)); !errors.Is(err, ErrWorkflowNotHeld) && !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("promote released run error = %v, want not-held or conflict", err)
	}
	var promotions int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM convergence_promotions`).Scan(&promotions); err != nil {
		t.Fatalf("count promotions: %v", err)
	}
	if promotions != 0 {
		t.Fatalf("promote on released run created %d promotion records", promotions)
	}
}

func TestPromoteConvergenceReviewCopiesTagsAndBody(t *testing.T) {
	ctx := context.Background()
	env, executor, _ := newPromotionTestEnv(t)
	task, evidence := holdRealConvergenceReview(t, env, executor)

	tag, err := env.tasks.CreateTag(ctx, CreateTagInput{Slug: "priority", Name: "priority"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := env.tasks.TagTask(ctx, task.ID, tag.ID, ActorSystem); err != nil {
		t.Fatalf("tag source task: %v", err)
	}

	if _, err := env.fixture.store.DB().ExecContext(ctx, `
UPDATE tasks SET body = 'the oversized source body' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("set source task body: %v", err)
	}

	result, err := executor.PromoteConvergenceReview(ctx, promoteInput(task.ID, evidence))
	if err != nil {
		t.Fatalf("promote convergence review: %v", err)
	}
	if result.PlanningTask.Body != "the oversized source body" {
		t.Fatalf("planning task body = %q, want the source body copied", result.PlanningTask.Body)
	}
	tags, err := env.tasks.TagsForTask(ctx, result.PlanningTask.ID)
	if err != nil {
		t.Fatalf("list planning task tags: %v", err)
	}
	found := false
	for _, taskTag := range tags {
		if taskTag.ID == tag.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("planning task tags = %+v, want copied tag %d", tags, tag.ID)
	}
}
