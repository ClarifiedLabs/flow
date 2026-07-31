package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// TestConvergencePromoteDispositionSucceedsAndIsIdempotent is the regression
// test for the web UI "promote to feature" action: POSTing the promote
// disposition to the convergence endpoint must return 200 (not 503), create a
// clean-base feature planning workflow, and replay safely on a duplicate click.
func TestConvergencePromoteDispositionSucceedsAndIsIdempotent(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := fixture.Project.ExchangePath
	makeExchangeHooksInert(t, exchangePath)
	mainSHA := seedAPIMain(t, exchangePath)
	ctx := context.Background()

	// Set up a task whose run is parked at its current node with a real change.
	defaultFlowID, err := fixture.Bundle.Flows.DefaultFlowID(ctx)
	if err != nil {
		t.Fatalf("default flow: %v", err)
	}
	task, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{
		Title: "Oversized task", FlowID: defaultFlowID,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule run: %v", err)
	}
	nodeRun, _, err := fixture.Bundle.WorkflowRuns.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("enter first node: %v", err)
	}

	// Push a real source branch commit to the exchange.
	branch := "task/" + task.ID
	clonePath := filepath.Join(t.TempDir(), "clone")
	runAPIGit(t, "", "clone", exchangePath, clonePath)
	runAPIGit(t, clonePath, "config", "user.name", "Flow Test")
	runAPIGit(t, clonePath, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, clonePath, "checkout", "-b", branch, "origin/main")
	writeAPIFile(t, clonePath, "oversized.txt", "a change far beyond the scope budget\n")
	runAPIGit(t, clonePath, "add", "oversized.txt")
	runAPIGit(t, clonePath, "commit", "-m", "oversized source change")
	runAPIGit(t, clonePath, "push", "origin", "HEAD:refs/heads/"+branch)
	headSHA := apiGitOutput(t, clonePath, "rev-parse", "HEAD")

	// Project the change/artifact pair the convergence path revalidates, the
	// same projection a git event consumer would have recorded.
	const changeID = "ch-api-promote-0001"
	artifactID := "wa-convergence-" + run.ID
	payload, err := json.Marshal(map[string]string{"change_id": changeID, "head_sha": headSHA})
	if err != nil {
		t.Fatalf("marshal artifact payload: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, 'main', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		changeID, task.ID, run.ID, branch, headSHA); err != nil {
		t.Fatalf("insert change projection: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-convergence', 'change', 'Convergence fixture', ?, 'digest', 'convergence-fixture', ?)`,
		artifactID, run.ID, nodeRun.ID, string(payload), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert artifact projection: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE workflow_runs SET current_artifact_id = ? WHERE id = ?;
UPDATE workflow_node_runs SET input_artifact_id = ? WHERE id = ?`,
		artifactID, run.ID, artifactID, nodeRun.ID); err != nil {
		t.Fatalf("project current artifact: %v", err)
	}

	// Drive the typed convergence hold through the production executor path so
	// the fingerprint the UI would send is the one the disposition revalidates.
	if _, err := fixture.Bundle.WorkflowExecutor.RequestConvergenceReview(ctx, task.ID, coordinator.ActorSystem); err != nil {
		t.Fatalf("request convergence review: %v", err)
	}
	active, err := fixture.Bundle.WorkflowRuns.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || active == nil {
		t.Fatalf("load active convergence evidence: evidence=%+v err=%v", active, err)
	}

	path := "/v2/projects/" + fixture.Project.ID + "/tasks/" + task.ID + "/workflow/convergence"
	requestBody := workflowConvergenceRequest{
		Disposition:                 coordinator.ConvergencePromote,
		Note:                        "superseded by a feature planning workflow",
		ExpectedEvidenceFingerprint: active.Fingerprint,
	}

	var promoted coordinator.ConvergenceReviewResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, requestBody, http.StatusOK, &promoted)
	if promoted.Disposition != coordinator.ConvergencePromote || promoted.Feature == nil ||
		promoted.PlanningTask == nil || promoted.PlanningRun == nil {
		t.Fatalf("promote response = %+v, want promote with feature/planning task/planning run", promoted)
	}

	// The feature branch is seeded at the reviewed base tip, not the source head.
	featureBranch := "feature/" + promoted.Feature.ID
	if promoted.Feature.Branch != featureBranch {
		t.Fatalf("feature branch = %q, want %q", promoted.Feature.Branch, featureBranch)
	}
	if tip := apiRefTip(t, exchangePath, featureBranch); tip != mainSHA {
		t.Fatalf("feature ref tip = %s, want base tip %s", tip, mainSHA)
	}
	if promoted.PlanningTask.FeatureID == nil || *promoted.PlanningTask.FeatureID != promoted.Feature.ID {
		t.Fatalf("planning task feature = %v, want %s", promoted.PlanningTask.FeatureID, promoted.Feature.ID)
	}
	var planningFlowName string
	if err := fixture.DB.QueryRowContext(ctx, `
SELECT f.name FROM flows f JOIN tasks t ON t.flow_id = f.id WHERE t.id = ?`, promoted.PlanningTask.ID).Scan(&planningFlowName); err != nil {
		t.Fatalf("read planning flow: %v", err)
	}
	if planningFlowName != coordinator.PlanningFlowName {
		t.Fatalf("planning task flow = %q, want %q", planningFlowName, coordinator.PlanningFlowName)
	}
	if promoted.Run.State != coordinator.WorkflowRunCompleted {
		t.Fatalf("source run state = %q, want completed", promoted.Run.State)
	}
	if promoted.Task.State == nil || *promoted.Task.State != coordinator.LifecycleDone {
		t.Fatalf("source task state = %v, want done", promoted.Task.State)
	}
	// The planning workflow was picked up by the handler, not left waiting for a
	// tick. Its first node is the planning agent, which enqueues an author job;
	// the planning task must be scheduled with the workflow advancing.
	if promoted.PlanningRun.State != coordinator.WorkflowRunScheduled && promoted.PlanningRun.State != coordinator.WorkflowRunRunning {
		t.Fatalf("planning run state = %q, want scheduled or running", promoted.PlanningRun.State)
	}
	var planningJobs int
	if err := fixture.DB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs WHERE node_run_id IN (
	SELECT id FROM workflow_node_runs WHERE workflow_run_id = ?
) AND state IN ('queued', 'claimed', 'running')`, promoted.PlanningRun.ID).Scan(&planningJobs); err != nil {
		t.Fatalf("count planning jobs: %v", err)
	}
	if planningJobs != 1 {
		t.Fatalf("planning jobs = %d, want 1 enqueued by the handler advance", planningJobs)
	}

	// A duplicate click replays to the same feature and planning task.
	var replayed coordinator.ConvergenceReviewResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, requestBody, http.StatusOK, &replayed)
	if replayed.Feature == nil || replayed.Feature.ID != promoted.Feature.ID {
		t.Fatalf("replayed feature = %+v, want %s", replayed.Feature, promoted.Feature.ID)
	}
	if replayed.PlanningTask == nil || replayed.PlanningTask.ID != promoted.PlanningTask.ID {
		t.Fatalf("replayed planning task = %+v, want %s", replayed.PlanningTask, promoted.PlanningTask.ID)
	}
	if replayed.PlanningRun == nil || replayed.PlanningRun.ID != promoted.PlanningRun.ID {
		t.Fatalf("replayed planning run = %+v, want %s", replayed.PlanningRun, promoted.PlanningRun.ID)
	}
	var features, promotions int
	if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM features`).Scan(&features); err != nil {
		t.Fatalf("count features: %v", err)
	}
	if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM convergence_promotions`).Scan(&promotions); err != nil {
		t.Fatalf("count promotions: %v", err)
	}
	if features != 1 || promotions != 1 {
		t.Fatalf("duplicate promote duplicated state: features=%d promotions=%d", features, promotions)
	}

	// A stale fingerprint still fails closed without creating anything.
	stale := requestBody
	stale.ExpectedEvidenceFingerprint = "sha256:stale"
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, stale, http.StatusConflict, nil)
	if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM features`).Scan(&features); err != nil {
		t.Fatalf("count features after stale request: %v", err)
	}
	if features != 1 {
		t.Fatalf("stale promote created features: %d", features)
	}
}
