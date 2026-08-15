package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// featureTestEnv bundles a real-git project fixture with the coordinator
// services feature work flows through, wired the way the registry wires them.
type featureTestEnv struct {
	fixture  projectFixture
	tasks    *TaskService
	flows    *FlowService
	runs     *WorkflowRunService
	features *FeatureService
}

func newFeatureTestEnv(t *testing.T) *featureTestEnv {
	t.Helper()
	ctx := context.Background()
	fixture := newProjectFixture(t)

	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}
	defs := NewInheritedAgentDefService(fixture.store.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(fixture.store.DB(), defs)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed default flows: %v", err)
	}

	tasks := NewTaskService(fixture.store.DB(), testProjectID)
	runs := NewWorkflowRunService(fixture.store.DB(), flows, tasks)
	features := NewFeatureService(fixture.store.DB(), tasks, fixture.project)
	features.Runs = runs
	runs.Features = features

	return &featureTestEnv{fixture: fixture, tasks: tasks, flows: flows, runs: runs, features: features}
}

func (env *featureTestEnv) exchangePath() string {
	return env.fixture.project.ExchangePath
}

// advanceMain commits a file on main in the fixture repo and pushes it to the
// exchange, returning the new main tip.
func (env *featureTestEnv) advanceMain(t *testing.T, file, contents, message string) string {
	t.Helper()
	repoPath := env.fixture.repoPath
	writeReconcileFile(t, repoPath, file, contents)
	if err := runReconcileGit(repoPath, nil, "add", file); err != nil {
		t.Fatalf("stage %s: %v", file, err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", message); err != nil {
		t.Fatalf("commit %s: %v", message, err)
	}
	if err := runReconcileGit(repoPath, nil, "push", env.exchangePath(), "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("push main: %v", err)
	}
	sha, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve main tip: %v", err)
	}
	return sha
}

// cloneExchange clones the fixture exchange for direct branch work.
func (env *featureTestEnv) cloneExchange(t *testing.T) string {
	t.Helper()
	clonePath := filepath.Join(t.TempDir(), "clone")
	if err := runReconcileGit("", nil, "clone", env.exchangePath(), clonePath); err != nil {
		t.Fatalf("clone exchange: %v", err)
	}
	if err := runReconcileGit(clonePath, nil, "config", "user.name", "Flow Test"); err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if err := runReconcileGit(clonePath, nil, "config", "user.email", "flow-test@example.com"); err != nil {
		t.Fatalf("config user.email: %v", err)
	}
	return clonePath
}

// commitOnBranch commits a file on top of origin/<branch> in dir and pushes it
// back to the branch, returning the new head.
func commitOnBranch(t *testing.T, dir, branch, file, contents, message string) string {
	t.Helper()
	if err := runReconcileGit(dir, nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		t.Fatalf("checkout %s: %v", branch, err)
	}
	writeReconcileFile(t, dir, file, contents)
	if err := runReconcileGit(dir, nil, "add", file); err != nil {
		t.Fatalf("stage %s: %v", file, err)
	}
	if err := runReconcileGit(dir, nil, "commit", "-m", message); err != nil {
		t.Fatalf("commit %s: %v", message, err)
	}
	if err := runReconcileGit(dir, nil, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		t.Fatalf("push %s: %v", branch, err)
	}
	sha, err := reconcileGitOutput(dir, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve %s head: %v", branch, err)
	}
	return sha
}

func (env *featureTestEnv) branchTip(t *testing.T, branch string) string {
	t.Helper()
	sha, err := reconcileGitOutput("", nil, "--git-dir", env.exchangePath(), "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("resolve %s tip: %v", branch, err)
	}
	return sha
}

func stringPtrPtr(value *string) **string {
	return &value
}

func TestFeatureServiceCreateSeedsBranchFromBaseTip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	baseTip := env.branchTip(t, "main")

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "Payments", Body: "payment work"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	if feature.ID != "f-test-0001" {
		t.Fatalf("feature id = %q, want f-test-0001", feature.ID)
	}
	if feature.Title != "Payments" || feature.Body != "payment work" {
		t.Fatalf("feature = %+v", feature)
	}
	if feature.Branch != "feature/f-test-0001" || feature.Status != FeatureOpen {
		t.Fatalf("feature = %+v", feature)
	}

	if tip := env.branchTip(t, "feature/f-test-0001"); tip != baseTip {
		t.Fatalf("feature branch tip = %s, want base tip %s", tip, baseTip)
	}

	second, err := env.features.Create(ctx, CreateFeatureInput{Title: "Search"})
	if err != nil {
		t.Fatalf("create second feature: %v", err)
	}
	if second.ID != "f-test-0002" {
		t.Fatalf("second feature id = %q, want f-test-0002", second.ID)
	}

	byRef, err := env.features.Resolve(ctx, "Payments")
	if err != nil {
		t.Fatalf("resolve by title: %v", err)
	}
	if byRef.ID != feature.ID {
		t.Fatalf("resolve by title id = %q, want %q", byRef.ID, feature.ID)
	}
	if _, err := env.features.Get(ctx, "f-test-9999"); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("missing feature error = %v, want ErrFeatureNotFound", err)
	}
	if _, err := env.features.Create(ctx, CreateFeatureInput{Title: "  "}); err == nil {
		t.Fatal("blank feature title was accepted")
	}
	// Titles are unique per project, case-insensitively.
	if _, err := env.features.Create(ctx, CreateFeatureInput{Title: "payments"}); !errors.Is(err, ErrFeatureTitleTaken) {
		t.Fatalf("duplicate title error = %v, want ErrFeatureTitleTaken", err)
	}

	title := "Payments v2"
	body := "updated"
	edited, err := env.features.Edit(ctx, feature.ID, EditFeatureInput{Title: &title, Body: &body})
	if err != nil {
		t.Fatalf("edit feature: %v", err)
	}
	if edited.Title != "Payments v2" || edited.Body != "updated" {
		t.Fatalf("edited feature = %+v", edited)
	}
}

func TestFeatureServiceListFiltersByStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	first, err := env.features.Create(ctx, CreateFeatureInput{Title: "alpha"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := env.features.Create(ctx, CreateFeatureInput{Title: "beta"})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE features SET status = 'archived' WHERE id = ?`, second.ID); err != nil {
		t.Fatalf("archive second: %v", err)
	}

	open, err := env.features.List(ctx, FeatureOpen)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 || open[0].ID != first.ID {
		t.Fatalf("open features = %+v, want only first", open)
	}
	archived, err := env.features.List(ctx, FeatureArchived)
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(archived) != 1 || archived[0].ID != second.ID {
		t.Fatalf("archived features = %+v, want only second", archived)
	}
	all, err := env.features.List(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 || all[0].Title != "alpha" || all[1].Title != "beta" {
		t.Fatalf("all features = %+v, want title-ordered pair", all)
	}
	if _, err := env.features.List(ctx, "bogus"); err == nil {
		t.Fatal("invalid status filter was accepted")
	}
}

func TestTaskFeatureAssignmentAndEditGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "billing"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	other, err := env.features.Create(ctx, CreateFeatureInput{Title: "search"})
	if err != nil {
		t.Fatalf("create other feature: %v", err)
	}

	// Creation-time assignment requires an existing, open feature.
	task, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "scoped work", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create feature task: %v", err)
	}
	if task.FeatureID == nil || *task.FeatureID != feature.ID {
		t.Fatalf("task feature = %v, want %s", task.FeatureID, feature.ID)
	}
	unknown := "f-test-9999"
	if _, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bad", FeatureID: &unknown}); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("create with unknown feature error = %v, want ErrFeatureNotFound", err)
	}

	// Edit to another open feature works while the task is untouched, and a
	// review-policy edit in the same request survives the relationship reload.
	required := true
	reloaded, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{
		FeatureID:           stringPtrPtr(&other.ID),
		RequiresHumanReview: &required,
	})
	if err != nil {
		t.Fatalf("edit feature and review policy: %v", err)
	}
	if reloaded.FeatureID == nil || *reloaded.FeatureID != other.ID {
		t.Fatalf("edited feature = %v, want %s", reloaded.FeatureID, other.ID)
	}
	if !reloaded.RequiresHumanReview {
		t.Fatal("combined feature edit discarded requires_human_review")
	}
	// A no-op assignment always succeeds.
	if _, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{FeatureID: stringPtrPtr(&other.ID)}); err != nil {
		t.Fatalf("no-op feature edit: %v", err)
	}
	// Clearing works too.
	cleared, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{FeatureID: stringPtrPtr(nil)})
	if err != nil {
		t.Fatalf("clear feature: %v", err)
	}
	if cleared.FeatureID != nil {
		t.Fatalf("cleared feature = %v, want nil", cleared.FeatureID)
	}

	// Scheduled (but not in progress) tasks may still move.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE tasks SET lifecycle_state = 'scheduled' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark scheduled: %v", err)
	}
	if _, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{FeatureID: stringPtrPtr(&feature.ID)}); err != nil {
		t.Fatalf("scheduled task feature edit: %v", err)
	}
	// A scheduled task's frozen review policy cannot change, and rejection must
	// roll back the feature move in the same transaction.
	required = false
	if _, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{
		FeatureID:           stringPtrPtr(&other.ID),
		RequiresHumanReview: &required,
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("scheduled combined edit error = %v, want ErrWorkflowConflict", err)
	}
	unchanged, err := env.tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("reload rejected combined edit: %v", err)
	}
	if unchanged.FeatureID == nil || *unchanged.FeatureID != feature.ID || !unchanged.RequiresHumanReview {
		t.Fatalf("task after rejected combined edit = feature %v review %t, want %s/true", unchanged.FeatureID, unchanged.RequiresHumanReview, feature.ID)
	}

	// In-progress tasks may not move.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark in progress: %v", err)
	}
	if _, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{FeatureID: stringPtrPtr(&other.ID)}); err == nil ||
		!strings.Contains(err.Error(), "in progress") {
		t.Fatalf("in-progress feature edit error = %v, want in-progress rejection", err)
	}

	// A task with a change row may not move even after the run resets.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE tasks SET lifecycle_state = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("reset lifecycle: %v", err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, created_at, updated_at)
VALUES ('ch-guard', ?, 'task/t-test-0001/run-1', 'main', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, task.ID); err != nil {
		t.Fatalf("insert change: %v", err)
	}
	if _, err := env.tasks.EditTask(ctx, task.ID, EditTaskInput{FeatureID: stringPtrPtr(&other.ID)}); err == nil ||
		!strings.Contains(err.Error(), "has a change") {
		t.Fatalf("changed task feature edit error = %v, want change-row rejection", err)
	}

	// A landed feature rejects new assignments.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE features SET status = 'landed' WHERE id = ?`, other.ID); err != nil {
		t.Fatalf("land other feature: %v", err)
	}
	fresh, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "fresh"})
	if err != nil {
		t.Fatalf("create fresh task: %v", err)
	}
	if _, err := env.tasks.EditTask(ctx, fresh.ID, EditTaskInput{FeatureID: stringPtrPtr(&other.ID)}); !errors.Is(err, ErrFeatureClosed) {
		t.Fatalf("landed feature assignment error = %v, want ErrFeatureClosed", err)
	}
}

func TestExecutorChangeBaseForTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	executor := &WorkflowExecutor{db: env.fixture.store.DB(), project: env.fixture.project}

	plain, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "plain"})
	if err != nil {
		t.Fatalf("create plain task: %v", err)
	}
	base, err := executor.changeBaseForTask(ctx, plain.ID)
	if err != nil {
		t.Fatalf("plain base: %v", err)
	}
	if base != "main" {
		t.Fatalf("plain task base = %q, want main", base)
	}

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "checkout"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	scoped, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "scoped", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create scoped task: %v", err)
	}
	base, err = executor.changeBaseForTask(ctx, scoped.ID)
	if err != nil {
		t.Fatalf("scoped base: %v", err)
	}
	if base != "feature/f-test-0001" {
		t.Fatalf("scoped task base = %q, want feature/f-test-0001", base)
	}

	// A landed feature no longer routes work to its frozen branch.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`UPDATE features SET status = 'landed' WHERE id = ?`, feature.ID); err != nil {
		t.Fatalf("land feature: %v", err)
	}
	base, err = executor.changeBaseForTask(ctx, scoped.ID)
	if err != nil {
		t.Fatalf("landed base: %v", err)
	}
	if base != "main" {
		t.Fatalf("landed feature task base = %q, want main", base)
	}
}

// setupConflictedFeature creates a feature with one commit, then advances main
// with a conflicting change to the same file, returning the feature.
func setupConflictedFeature(t *testing.T, env *featureTestEnv) Feature {
	t.Helper()
	ctx := context.Background()
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "payments"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	commitOnBranch(t, clone, feature.Branch, "conflict.txt", "feature version\n", "feature work")
	env.advanceMain(t, "conflict.txt", "main version\n", "conflicting main work")
	return feature
}

func TestRebaseOnMainCleanThenUpToDate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "payments"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	// A feature commit so the rebase replays real work (non-conflicting).
	clone := env.cloneExchange(t)
	commitOnBranch(t, clone, feature.Branch, "feature.txt", "feature work\n", "feature commit")
	oldTip := env.branchTip(t, feature.Branch)
	env.advanceMain(t, "main-only.txt", "main work\n", "main commit")

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if result.Kind != RebaseRebased {
		t.Fatalf("rebase kind = %q, want %q", result.Kind, RebaseRebased)
	}
	if result.NewTipSHA == "" || result.NewTipSHA == oldTip {
		t.Fatalf("new tip = %q, want a moved head", result.NewTipSHA)
	}
	if tip := env.branchTip(t, feature.Branch); tip != result.NewTipSHA {
		t.Fatalf("feature ref tip = %s, want %s", tip, result.NewTipSHA)
	}
	// The rebased branch contains main and still carries the feature commit.
	_, behind, err := flowgit.RefDivergence(ctx, env.exchangePath(), "main", feature.Branch)
	if err != nil {
		t.Fatalf("divergence: %v", err)
	}
	if behind != 0 {
		t.Fatalf("behind = %d, want 0", behind)
	}
	content, err := reconcileGitOutput("", nil, "--git-dir", env.exchangePath(), "show", result.NewTipSHA+":feature.txt")
	if err != nil || strings.TrimSpace(content) != "feature work" {
		t.Fatalf("feature.txt at new head = %q, err = %v", content, err)
	}

	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	if len(rebases) != 1 || rebases[0].State != RebaseFinalized || rebases[0].TaskID != "" ||
		rebases[0].NewTipSHA != result.NewTipSHA || rebases[0].OldTipSHA != oldTip {
		t.Fatalf("rebases = %+v, want one finalized clean row", rebases)
	}

	again, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil {
		t.Fatalf("second rebase: %v", err)
	}
	if again.Kind != RebaseAlreadyUpToDate {
		t.Fatalf("second rebase kind = %q, want %q", again.Kind, RebaseAlreadyUpToDate)
	}
}

func TestRebaseOnMainConflictCreatesBlockingTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	taskA, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "feature task A", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create task A: %v", err)
	}
	taskB, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "feature task B", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create task B: %v", err)
	}
	other, err := env.features.Create(ctx, CreateFeatureInput{Title: "other"})
	if err != nil {
		t.Fatalf("create other feature: %v", err)
	}
	taskC, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "other feature task", FeatureID: &other.ID})
	if err != nil {
		t.Fatalf("create task C: %v", err)
	}

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if result.Kind != RebaseTaskCreated || result.RebaseTaskID == "" {
		t.Fatalf("rebase result = %+v, want rebase_task_created", result)
	}
	rebaseTask, err := env.tasks.GetTask(ctx, result.RebaseTaskID)
	if err != nil {
		t.Fatalf("load rebase task: %v", err)
	}
	if rebaseTask.CreatedBy != ActorSystem || rebaseTask.FeatureID == nil || *rebaseTask.FeatureID != feature.ID {
		t.Fatalf("rebase task = %+v", rebaseTask)
	}
	if rebaseTask.State == nil || *rebaseTask.State != LifecycleScheduled {
		t.Fatalf("rebase task state = %v, want scheduled", rebaseTask.State)
	}
	if !strings.Contains(rebaseTask.Body, "conflict.txt") {
		t.Fatalf("rebase task body missing conflict context: %q", rebaseTask.Body)
	}

	running, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("running rebase = %+v, %v, %v", running, found, err)
	}
	if running.TaskID != rebaseTask.ID || running.State != RebaseRunning {
		t.Fatalf("running rebase = %+v", running)
	}

	// The rebase task blocks the feature's other non-done tasks — and neither
	// itself nor other features' tasks.
	assertBlockedBy := func(taskID, blockerID string, want bool) {
		t.Helper()
		var count int
		if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, blockerID, taskID).Scan(&count); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		if (count == 1) != want {
			t.Fatalf("%s blocks %s = %v, want %v", blockerID, taskID, count == 1, want)
		}
	}
	assertBlockedBy(taskA.ID, rebaseTask.ID, true)
	assertBlockedBy(taskB.ID, rebaseTask.ID, true)
	assertBlockedBy(taskC.ID, rebaseTask.ID, false)
	assertBlockedBy(rebaseTask.ID, rebaseTask.ID, false)

	// A second rebase refuses while one runs.
	if _, err := env.features.RebaseOnMain(ctx, feature); !errors.Is(err, ErrFeatureRebaseRunning) {
		t.Fatalf("second rebase error = %v, want ErrFeatureRebaseRunning", err)
	}

	// Scheduling a feature task mid-rebase holds it at the dependency gate:
	// no node is created, so no author job is enqueued.
	if _, err := env.runs.ScheduleAs(ctx, taskA.ID, ActorHuman); err != nil {
		t.Fatalf("schedule task A: %v", err)
	}
	runA, _, err := env.runs.ActiveForTask(ctx, taskA.ID)
	if err != nil {
		t.Fatalf("active run for task A: %v", err)
	}
	nodeRun, _, err := env.runs.EnsureCurrentNode(ctx, runA.ID)
	if err != nil {
		t.Fatalf("ensure current node: %v", err)
	}
	if nodeRun.ID != "" {
		t.Fatalf("blocked task created node run %s, want none", nodeRun.ID)
	}

	// The rebase task itself is never self-blocked: its run advances.
	rebaseRun, _, err := env.runs.ActiveForTask(ctx, rebaseTask.ID)
	if err != nil {
		t.Fatalf("active run for rebase task: %v", err)
	}
	rebaseNode, _, err := env.runs.EnsureCurrentNode(ctx, rebaseRun.ID)
	if err != nil {
		t.Fatalf("ensure rebase node: %v", err)
	}
	if rebaseNode.ID == "" {
		t.Fatal("rebase task is blocked, want its first node created")
	}

	// Force-done closes the rebase row and releases the held task.
	if _, err := env.runs.ForceDone(ctx, rebaseTask.ID, ResolutionCancelled, "operator abort", ActorHuman); err != nil {
		t.Fatalf("force done rebase task: %v", err)
	}
	if _, found, err := env.features.RunningRebase(ctx, feature.ID); err != nil || found {
		t.Fatalf("running rebase after force-done = %v, %v", found, err)
	}
	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	if len(rebases) != 1 || rebases[0].State != RebaseCancelled {
		t.Fatalf("rebases = %+v, want one cancelled row", rebases)
	}
	nodeRun, _, err = env.runs.EnsureCurrentNode(ctx, runA.ID)
	if err != nil {
		t.Fatalf("ensure current node after release: %v", err)
	}
	if nodeRun.ID == "" {
		t.Fatal("released task still blocked after rebase task done")
	}
}

// TestRebaseOnMainRestrictBlockedTo locks the task-bound console rebase
// confinement at relation-creation time: when RebaseOnMain is told to restrict
// its blocker links to one task, a conflicted rebase links exactly that task
// even when the feature holds other non-done tasks. This closes the TOCTOU
// race of an API-side pre-read: a feature task created concurrently with the
// rebase can never receive a rebase_task blocks relation.
func TestRebaseOnMainRestrictBlockedTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	countRows := func(table string) int {
		t.Helper()
		var n int
		if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		return n
	}

	// A feature holding another non-done task rejects the restricted rebase at
	// decision time, before any rebase or relation row exists.
	featureWithExtra := setupConflictedFeature(t, env)
	boundTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &featureWithExtra.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}
	if _, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "extra task", FeatureID: &featureWithExtra.ID}); err != nil {
		t.Fatalf("create extra task: %v", err)
	}
	if _, err := env.features.RebaseOnMain(ctx, featureWithExtra, boundTask.ID); !errors.Is(err, ErrFeatureRebaseForbidden) {
		t.Fatalf("restricted rebase error = %v, want ErrFeatureRebaseForbidden", err)
	}
	if got := countRows("feature_rebases"); got != 0 {
		t.Fatalf("forbidden rebase created %d rebase rows, want 0", got)
	}
	if got := countRows("work_item_relations WHERE kind = 'blocks'"); got != 0 {
		t.Fatalf("forbidden rebase created %d blocker rows, want 0", got)
	}

	// A feature whose only non-done task is the bound task still rebases, and
	// the conflicted rebase links exactly the bound task.
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "payments sole"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	commitOnBranch(t, clone, feature.Branch, "conflict.txt", "sole feature version\n", "sole feature work")
	env.advanceMain(t, "conflict.txt", "sole main version\n", "sole conflicting main work")

	boundTask, err = env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}
	result, err := env.features.RebaseOnMain(ctx, feature, boundTask.ID)
	if err != nil {
		t.Fatalf("restricted rebase: %v", err)
	}
	if result.Kind != RebaseTaskCreated || result.RebaseTaskID == "" {
		t.Fatalf("rebase result = %+v, want rebase_task_created", result)
	}
	if got := countRows("feature_rebases"); got != 1 {
		t.Fatalf("allowed rebase created %d rebase rows, want 1", got)
	}
	// The task-bound confinement is persisted on the rebase row so the
	// scheduling gate can enforce it for the rebase's whole lifetime.
	running, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("running rebase = %+v, %v, %v", running, found, err)
	}
	if running.RestrictBlockedTo != boundTask.ID {
		t.Fatalf("running rebase restriction = %q, want %q", running.RestrictBlockedTo, boundTask.ID)
	}
	if got := countRows("work_item_relations WHERE kind = 'blocks'"); got != 1 {
		t.Fatalf("allowed rebase created %d blocker rows, want exactly 1", got)
	}
	rebaseTask, err := env.tasks.GetTask(ctx, result.RebaseTaskID)
	if err != nil {
		t.Fatalf("load rebase task: %v", err)
	}
	assertBlocked := func(taskID string, want bool) {
		t.Helper()
		var count int
		if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, rebaseTask.ID, taskID).Scan(&count); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		if (count == 1) != want {
			t.Fatalf("rebase task blocks %s = %v, want %v", taskID, count == 1, want)
		}
	}
	assertBlocked(boundTask.ID, true)
}

// TestRebaseOnMainConcurrentTaskAddStaysUnlinked exercises the review's
// guard-to-inner-read window deterministically: a feature task created by
// another principal after the restricted rebase's atomic scope decision (and
// after its running row is persisted) but before the conflicted path's
// non-done-task snapshot. The task commits in time to enter the initial
// relation sweep — without the relation-time restriction filter it would be
// linked — and the filter confines the sweep to the bound task, so no relation
// exists whose endpoints are both unrelated to the bound task.
func TestRebaseOnMainConcurrentTaskAddStaysUnlinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	boundTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}

	var concurrentTaskID string
	env.features.RebaseOnMainTestHook = func(phase RebaseOnMainTestPhase) {
		if phase != RebaseOnMainAfterReservation {
			return
		}
		concurrent, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "concurrent task", FeatureID: &feature.ID})
		if err != nil {
			t.Errorf("create concurrent task: %v", err)
			return
		}
		concurrentTaskID = concurrent.ID
	}
	t.Cleanup(func() { env.features.RebaseOnMainTestHook = nil })

	result, err := env.features.RebaseOnMain(ctx, feature, boundTask.ID)
	if err != nil {
		t.Fatalf("restricted rebase: %v", err)
	}
	if result.Kind != RebaseTaskCreated || result.RebaseTaskID == "" {
		t.Fatalf("rebase result = %+v, want rebase_task_created", result)
	}
	if concurrentTaskID == "" {
		t.Fatal("rebase hook did not run")
	}

	var relations int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE kind = 'blocks' AND source_item_id = ?`, result.RebaseTaskID).Scan(&relations); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if relations != 1 {
		t.Fatalf("rebase task created %d block relations, want exactly 1", relations)
	}
	var blockedBound, blockedConcurrent int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, result.RebaseTaskID, boundTask.ID).Scan(&blockedBound); err != nil {
		t.Fatalf("count bound relations: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, result.RebaseTaskID, concurrentTaskID).Scan(&blockedConcurrent); err != nil {
		t.Fatalf("count concurrent relations: %v", err)
	}
	if blockedBound != 1 {
		t.Fatalf("rebase task blocks bound task = %d, want 1", blockedBound)
	}
	if blockedConcurrent != 0 {
		t.Fatalf("rebase task blocks concurrently added task = %d, want 0", blockedConcurrent)
	}
}

// TestRebaseOnMainTaskAddBeforeDecisionRejectsRebase covers the raced 403
// path: a feature task added by another principal after the API-side scope
// pre-read but before the locked scope decision commits in time to be seen by
// the decision, so the restricted rebase is rejected with
// ErrFeatureRebaseForbidden before any rebase or relation row exists.
func TestRebaseOnMainTaskAddBeforeDecisionRejectsRebase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	boundTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}

	var concurrentTaskID string
	env.features.RebaseOnMainTestHook = func(phase RebaseOnMainTestPhase) {
		if phase != RebaseOnMainBeforeScopeCheck {
			return
		}
		concurrent, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "concurrent task", FeatureID: &feature.ID})
		if err != nil {
			t.Errorf("create concurrent task: %v", err)
			return
		}
		concurrentTaskID = concurrent.ID
	}
	t.Cleanup(func() { env.features.RebaseOnMainTestHook = nil })

	result, err := env.features.RebaseOnMain(ctx, feature, boundTask.ID)
	if !errors.Is(err, ErrFeatureRebaseForbidden) {
		t.Fatalf("restricted rebase error = %v, want ErrFeatureRebaseForbidden", err)
	}
	if result.Kind != RebaseStartKind("") {
		t.Fatalf("rebase result = %+v, want empty result on forbidden", result)
	}
	if concurrentTaskID == "" {
		t.Fatal("rebase hook did not run")
	}
	var rebaseRows, relationRows int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_rebases`).Scan(&rebaseRows); err != nil {
		t.Fatalf("count rebase rows: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_item_relations WHERE kind = 'blocks'`).Scan(&relationRows); err != nil {
		t.Fatalf("count relation rows: %v", err)
	}
	if rebaseRows != 0 {
		t.Fatalf("forbidden rebase created %d rebase rows, want 0", rebaseRows)
	}
	if relationRows != 0 {
		t.Fatalf("forbidden rebase created %d blocker rows, want 0", relationRows)
	}
}

// TestRebaseBlockRestrictionHoldsAtScheduleTime locks the task-bound console
// confinement into the running rebase row's whole lifetime, not just the
// initial conflicted sweep: the restriction is persisted on feature_rebases and
// the schedule-time gate (WorkflowRunService.ScheduleAs → EnsureRebaseBlock)
// consults it. A sibling created after the rebase started and then scheduled
// never receives a rebase_task blocks relation, while the bound task keeps its
// link (duplicate-tolerant) when it is scheduled mid-rebase.
func TestRebaseBlockRestrictionHoldsAtScheduleTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	boundTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}
	result, err := env.features.RebaseOnMain(ctx, feature, boundTask.ID)
	if err != nil {
		t.Fatalf("restricted rebase: %v", err)
	}
	if result.Kind != RebaseTaskCreated || result.RebaseTaskID == "" {
		t.Fatalf("rebase result = %+v, want rebase_task_created", result)
	}

	// The confinement is persisted on the running row so the schedule-time
	// gate can consult it without a racy pre-read.
	var persisted string
	if err := env.fixture.store.DB().QueryRowContext(ctx,
		`SELECT restrict_blocked_to FROM feature_rebases WHERE state = 'running' AND feature_id = ?`,
		feature.ID).Scan(&persisted); err != nil {
		t.Fatalf("read persisted restriction: %v", err)
	}
	if persisted != boundTask.ID {
		t.Fatalf("persisted restriction = %q, want %q", persisted, boundTask.ID)
	}

	countBlockedBy := func(rebaseTaskID, taskID string) int {
		t.Helper()
		var count int
		if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, rebaseTaskID, taskID).Scan(&count); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return count
	}

	// A sibling created mid-rebase and then scheduled (the lifecycle path the
	// initial sweep cannot see) is not linked: ScheduleAs consults the running
	// row's restriction before BlockOnRebase runs.
	late, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "late sibling", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create late sibling: %v", err)
	}
	if _, err := env.runs.ScheduleAs(ctx, late.ID, ActorHuman); err != nil {
		t.Fatalf("schedule late sibling: %v", err)
	}
	if got := countBlockedBy(result.RebaseTaskID, late.ID); got != 0 {
		t.Fatalf("rebase task blocks late sibling %d times, want 0", got)
	}

	// The bound task's own mid-rebase schedule stays allowed and keeps exactly
	// one link (BlockOnRebase tolerates the duplicate).
	if _, err := env.runs.ScheduleAs(ctx, boundTask.ID, ActorHuman); err != nil {
		t.Fatalf("schedule bound task: %v", err)
	}
	if got := countBlockedBy(result.RebaseTaskID, boundTask.ID); got != 1 {
		t.Fatalf("rebase task blocks bound task %d times, want exactly 1", got)
	}
}

// TestRestrictionAllows locks the persisted-restriction predicate: an empty
// restriction allows every task (owner and unbound project-console rebases),
// and a comma-joined list allows exactly its members.
func TestRestrictionAllows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		restriction string
		taskID      string
		want        bool
	}{
		{"empty allows any task", "", "t-any", true},
		{"empty allows the bound task", "", "t-bound", true},
		{"list allows its member", "t-a,t-b", "t-b", true},
		{"list rejects a non-member", "t-a,t-b", "t-c", false},
		{"list tolerates surrounding whitespace", " t-a , t-b ", "t-a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restrictionAllows(tc.restriction, tc.taskID); got != tc.want {
				t.Fatalf("restrictionAllows(%q, %q) = %v, want %v", tc.restriction, tc.taskID, got, tc.want)
			}
		})
	}
}

// produceRebasedHead simulates the rebase agent: it rebases the feature
// branch onto main in a clone (favoring the feature side on conflicts),
// pushes the result to the rebase task's branch, and returns the new head.
func produceRebasedHead(t *testing.T, env *featureTestEnv, feature Feature, taskBranch string) string {
	t.Helper()
	clone := env.cloneExchange(t)
	if err := runReconcileGit(clone, nil, "checkout", "-B", taskBranch, "origin/"+feature.Branch); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	if err := runReconcileGit(clone, nil, "-c", "core.editor=true", "rebase", "-X", "theirs", "origin/main"); err != nil {
		t.Fatalf("agent rebase: %v", err)
	}
	if err := runReconcileGit(clone, nil, "push", "origin", "+HEAD:refs/heads/"+taskBranch); err != nil {
		t.Fatalf("push task branch: %v", err)
	}
	sha, err := reconcileGitOutput(clone, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve rebased head: %v", err)
	}
	return sha
}

func TestFinalizeRebasePublishesAndStales(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v, want rebase task", result, err)
	}
	taskBranch := "task/" + result.RebaseTaskID + "/run-1"
	head := produceRebasedHead(t, env, feature, taskBranch)

	// Finalizing publishes the rebased head to the feature ref and stamps the
	// row finalized.
	change := Change{ID: "ch-rebase", TaskID: result.RebaseTaskID, Branch: taskBranch, Base: feature.Branch, HeadSHA: head}
	outcome, err := env.features.FinalizeRebase(ctx, feature.ID, result.RebaseTaskID, change)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if outcome != "finalized" {
		t.Fatalf("finalize outcome = %q, want finalized", outcome)
	}
	if tip := env.branchTip(t, feature.Branch); tip != head {
		t.Fatalf("feature tip = %s, want rebased head %s", tip, head)
	}
	if _, found, err := env.features.RunningRebase(ctx, feature.ID); err != nil || found {
		t.Fatalf("running rebase after finalize = %v, %v", found, err)
	}
	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	if len(rebases) != 1 || rebases[0].State != RebaseFinalized || rebases[0].NewTipSHA != head {
		t.Fatalf("rebases = %+v, want finalized row at new head", rebases)
	}

	// Replay: the ref already at the new head heals to finalized without a
	// running row complaint — it errors because no row is open anymore.
	if _, err := env.features.FinalizeRebase(ctx, feature.ID, result.RebaseTaskID, change); !errors.Is(err, ErrNoOpenRebase) {
		t.Fatalf("finalize without open rebase error = %v, want ErrNoOpenRebase", err)
	}
}

func TestFinalizeRebaseStaleWhenBranchMoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v, want rebase task", result, err)
	}
	taskBranch := "task/" + result.RebaseTaskID + "/run-1"
	head := produceRebasedHead(t, env, feature, taskBranch)

	// A task merges into the feature branch mid-rebase: the tip moves.
	clone := env.cloneExchange(t)
	movedTip := commitOnBranch(t, clone, feature.Branch, "merged-task.txt", "merged work\n", "task merge")

	change := Change{ID: "ch-rebase", TaskID: result.RebaseTaskID, Branch: taskBranch, Base: feature.Branch, HeadSHA: head}
	outcome, err := env.features.FinalizeRebase(ctx, feature.ID, result.RebaseTaskID, change)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if outcome != "stale" {
		t.Fatalf("finalize outcome = %q, want stale", outcome)
	}
	if tip := env.branchTip(t, feature.Branch); tip != movedTip {
		t.Fatalf("feature tip = %s, want untouched moved tip %s", tip, movedTip)
	}

	// The redo row is open against the moved tip for the same task.
	running, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("redo row = %+v, %v, %v", running, found, err)
	}
	if running.OldTipSHA != movedTip || running.TaskID != result.RebaseTaskID {
		t.Fatalf("redo row = %+v", running)
	}
	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	if len(rebases) != 2 {
		t.Fatalf("rebases = %d rows, want stale + running", len(rebases))
	}

	// The redo rebases onto the moved tip; finalizing then publishes.
	head2 := produceRebasedHead(t, env, feature, taskBranch)
	change.HeadSHA = head2
	outcome, err = env.features.FinalizeRebase(ctx, feature.ID, result.RebaseTaskID, change)
	if err != nil {
		t.Fatalf("redo finalize: %v", err)
	}
	if outcome != "finalized" {
		t.Fatalf("redo outcome = %q, want finalized", outcome)
	}
	if tip := env.branchTip(t, feature.Branch); tip != head2 {
		t.Fatalf("feature tip = %s, want redo head %s", tip, head2)
	}
}

// TestRestrictedRebaseRedoKeepsConfinement locks the stale-redo path for a
// task-bound rebase: when the feature branch moves mid-rebase and finalizing
// opens a redo row, the restriction is carried onto the redo row, so the
// schedule-time gate keeps linking nothing new for other tasks and keeps the
// bound task's link across the redo.
func TestRestrictedRebaseRedoKeepsConfinement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	boundTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}

	result, err := env.features.RebaseOnMain(ctx, feature, boundTask.ID)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v, want rebase task", result, err)
	}
	// A task added mid-rebase (the concurrent-add window) stays outside the
	// confinement: the gate links nothing new for it, across the redo.
	otherTask, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "other task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	taskBranch := "task/" + result.RebaseTaskID + "/run-1"
	head := produceRebasedHead(t, env, feature, taskBranch)

	// A task merges into the feature branch mid-rebase: the tip moves, so
	// finalizing opens a redo row against the moved tip.
	clone := env.cloneExchange(t)
	movedTip := commitOnBranch(t, clone, feature.Branch, "merged-task.txt", "merged work\n", "task merge")

	change := Change{ID: "ch-rebase", TaskID: result.RebaseTaskID, Branch: taskBranch, Base: feature.Branch, HeadSHA: head}
	outcome, err := env.features.FinalizeRebase(ctx, feature.ID, result.RebaseTaskID, change)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if outcome != "stale" {
		t.Fatalf("finalize outcome = %q, want stale", outcome)
	}
	if tip := env.branchTip(t, feature.Branch); tip != movedTip {
		t.Fatalf("feature tip = %s, want untouched moved tip %s", tip, movedTip)
	}

	// The redo row carries the restriction forward.
	running, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("redo row = %+v, %v, %v", running, found, err)
	}
	if running.RestrictBlockedTo != boundTask.ID {
		t.Fatalf("redo row restriction = %q, want %q", running.RestrictBlockedTo, boundTask.ID)
	}
	countBlockedBy := func(rebaseTaskID, taskID string) int {
		t.Helper()
		var count int
		if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, rebaseTaskID, taskID).Scan(&count); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return count
	}
	// The gate links nothing new for the mid-rebase sibling (no error: the
	// shipped design silently omits out-of-scope links) and keeps the bound
	// task's link.
	if err := env.features.EnsureRebaseBlock(ctx, otherTask.ID); err != nil {
		t.Fatalf("ensure other-task block: %v", err)
	}
	if got := countBlockedBy(result.RebaseTaskID, otherTask.ID); got != 0 {
		t.Fatalf("rebase task blocks other task %d times, want 0", got)
	}
	if err := env.features.EnsureRebaseBlock(ctx, boundTask.ID); err != nil {
		t.Fatalf("ensure bound-task block: %v", err)
	}
	if got := countBlockedBy(result.RebaseTaskID, boundTask.ID); got != 1 {
		t.Fatalf("rebase task blocks bound task %d times, want exactly 1", got)
	}
}

func TestEnsureRebaseBlockToleratesDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v", result, err)
	}
	task, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "late task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create late task: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := env.features.EnsureRebaseBlock(ctx, task.ID); err != nil {
			t.Fatalf("ensure rebase block %d: %v", i, err)
		}
	}
	var count int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`,
		result.RebaseTaskID, task.ID).Scan(&count); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if count != 1 {
		t.Fatalf("block relations = %d, want exactly 1", count)
	}
	// The rebase task never blocks itself.
	if err := env.features.EnsureRebaseBlock(ctx, result.RebaseTaskID); err != nil {
		t.Fatalf("ensure self block: %v", err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`,
		result.RebaseTaskID, result.RebaseTaskID).Scan(&count); err != nil {
		t.Fatalf("count self relations: %v", err)
	}
	if count != 0 {
		t.Fatalf("self block relations = %d, want 0", count)
	}
}

func TestSeedDefaultsSeedsPerName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	flows, err := env.flows.List(ctx)
	if err != nil {
		t.Fatalf("list flows: %v", err)
	}
	byName := map[string]Flow{}
	for _, flow := range flows {
		byName[flow.Name] = flow
	}
	for _, name := range []string{CodingFlowName, PlanningFlowName, FeatureRebaseFlowName} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("seeded flows missing %q: %v", name, flows)
		}
	}
	defaultID, err := env.flows.DefaultFlowID(ctx)
	if err != nil {
		t.Fatalf("default flow: %v", err)
	}
	if defaultID != byName[CodingFlowName].ID {
		t.Fatalf("default flow = %q, want coding %q", defaultID, byName[CodingFlowName].ID)
	}

	// The feature-rebase flow graph matches the trusted shape.
	rebaseFlow, err := env.flows.Get(ctx, byName[FeatureRebaseFlowName].ID)
	if err != nil {
		t.Fatalf("get rebase flow: %v", err)
	}
	kinds := map[string]NodeKind{}
	for _, node := range rebaseFlow.Nodes {
		kinds[node.Key] = node.Kind
	}
	for key, kind := range map[string]NodeKind{
		"rebase": NodeAgent, "checks": NodeAutomatedChecks, "verify": NodeVerifyChange,
		"human-review": NodeHumanGate, "finalize": NodeFinalizeRebase,
		"done": NodeTerminal, "abandoned": NodeTerminal,
	} {
		if kinds[key] != kind {
			t.Fatalf("rebase flow node %q kind = %q, want %q", key, kinds[key], kind)
		}
	}

	// Re-seeding is idempotent: no duplicates, default untouched.
	if err := env.flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	flows, err = env.flows.List(ctx)
	if err != nil {
		t.Fatalf("list flows after reseed: %v", err)
	}
	if len(flows) != 3 {
		t.Fatalf("flows after reseed = %d, want 3", len(flows))
	}

	// A missing built-in is restored without touching the others.
	if _, err := env.fixture.store.DB().ExecContext(ctx,
		`DELETE FROM flows WHERE name = ?`, FeatureRebaseFlowName); err != nil {
		t.Fatalf("delete rebase flow: %v", err)
	}
	if err := env.flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("reseed after delete: %v", err)
	}
	restoredID, err := env.flows.flowIDByName(ctx, FeatureRebaseFlowName)
	if err != nil || restoredID == "" {
		t.Fatalf("restored rebase flow id = %q, err = %v", restoredID, err)
	}
	if defaultAfter, _ := env.flows.DefaultFlowID(ctx); defaultAfter != defaultID {
		t.Fatalf("default flow changed to %q after reseed", defaultAfter)
	}
}

func TestSeedDefaultsPreservesUserFlowsAndFreshDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)

	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}
	defs := NewInheritedAgentDefService(fixture.store.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(fixture.store.DB(), defs)

	// A pre-existing (user) flow means the project is not fresh: built-ins are
	// still seeded, but the default flow is not chosen for the project.
	if _, err := flows.Create(ctx, FlowInput{
		Name: "mine", StartNode: "gate",
		Nodes: []FlowNodeInput{
			{Key: "gate", Name: "Gate", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved"}}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "gate", Outcome: "approved", To: "done"}},
	}); err != nil {
		t.Fatalf("create user flow: %v", err)
	}
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	all, err := flows.List(ctx)
	if err != nil {
		t.Fatalf("list flows: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("flows = %d, want user + 3 built-ins", len(all))
	}
	if defaultID, err := flows.DefaultFlowID(ctx); err != nil || defaultID != "" {
		t.Fatalf("default flow = %q, %v, want unset on non-fresh seed", defaultID, err)
	}
}

func TestExecutorFinalizeRebaseNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v", result, err)
	}
	rebaseTask, err := env.tasks.GetTask(ctx, result.RebaseTaskID)
	if err != nil {
		t.Fatalf("load rebase task: %v", err)
	}
	taskBranch := "task/" + rebaseTask.ID + "/run-1"
	head := produceRebasedHead(t, env, feature, taskBranch)

	// Craft the run at the finalize node: the rebase agent has completed and
	// its change artifact feeds finalize.
	snapshot := FlowSnapshot{
		FlowID: "fl-feature-rebase", FlowName: FeatureRebaseFlowName, StartNode: "rebase", TransitionBudget: 50,
		Nodes: []FlowNodeSnapshot{
			{Key: "rebase", Name: "Rebase", Kind: NodeAgent, Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
				Agent:     AgentDefSnapshot{ID: "ad-feature-rebase", Name: "rebase agent", Harness: "harness", Prompt: "Rebase the feature."},
				Workspace: WorkspaceChange, Artifact: ArtifactChange,
			}}},
			{Key: "finalize", Name: "Finalize", Kind: NodeFinalizeRebase, Config: FlowNodeSnapshotConfig{FinalizeRebase: &FinalizeRebaseNodeConfig{}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdge{
			{From: "rebase", Outcome: "completed", To: "finalize"},
			{From: "finalize", Outcome: "finalized", To: "done"},
			{From: "finalize", Outcome: "stale", To: "rebase"},
		},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	db := env.fixture.store.DB()
	const runID = "wr-finalize-test"
	const nodeRunID = "wnr-finalize-test"
	const artifactID = "wa-finalize-test"
	// Remove the run RebaseOnMain scheduled; the crafted run below resumes the
	// flow at the finalize node.
	if _, err := db.ExecContext(ctx, `
DELETE FROM workflow_runs WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, rebaseTask.ID); err != nil {
		t.Fatalf("remove scheduled run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, rebaseTask.ID); err != nil {
		t.Fatalf("mark in progress: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, current_artifact_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', 'finalize', ?, ?, 50,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		runID, rebaseTask.ID, string(snapshotJSON), nodeRunID, artifactID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id, created_at
) VALUES (?, ?, 'finalize', 1, 1, 'queued', ?, '2026-01-01T00:00:01Z')`, nodeRunID, runID, artifactID); err != nil {
		t.Fatalf("insert node run: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"change_id": "ch-finalize-test"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO changes (
	id, task_id, workflow_run_id, branch, base, head_sha, ready_at, created_at, updated_at
) VALUES ('ch-finalize-test', ?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		rebaseTask.ID, runID, taskBranch, feature.Branch, head); err != nil {
		t.Fatalf("insert change: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'rebase', 'change', 'Rebased', ?, 'digest', 'artifact-finalize', '2026-01-01T00:00:01Z')`,
		artifactID, runID, nodeRunID, string(payload)); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	artifacts := NewWorkflowArtifactService(db, env.tasks)
	sessions := NewSessionService(db, env.tasks, nil)
	executor := NewWorkflowExecutor(WorkflowExecutorOptions{
		Database: db, Runs: env.runs, Artifacts: artifacts, Tasks: env.tasks,
		Features: env.features, Sessions: sessions, Project: env.fixture.project,
	})
	if err := executor.Advance(ctx, runID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	run, err := env.runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", run.State)
	}
	if tip := env.branchTip(t, feature.Branch); tip != head {
		t.Fatalf("feature tip = %s, want rebased head %s", tip, head)
	}
	task, err := env.tasks.GetTask(ctx, rebaseTask.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State == nil || *task.State != LifecycleDone || task.DoneResolution == nil || *task.DoneResolution != ResolutionCompleted {
		t.Fatalf("rebase task = %+v, want done/completed", task)
	}
	if _, found, err := env.features.RunningRebase(ctx, feature.ID); err != nil || found {
		t.Fatalf("running rebase after finalize = %v, %v", found, err)
	}
}

func markTaskDone(t *testing.T, env *featureTestEnv, taskID string) {
	t.Helper()
	if _, err := env.fixture.store.DB().ExecContext(context.Background(), `
UPDATE tasks SET lifecycle_state = 'done', done_resolution = 'completed',
	done_at = '2026-01-01T00:00:00Z' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("mark task done: %v", err)
	}
}

func TestLandFeatureSquashMergesAndStamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "payments"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	commitOnBranch(t, clone, feature.Branch, "feature.txt", "feature work\n", "feature commit")
	task, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "feature task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Active work bars the land, naming the offender.
	if _, err := env.features.Land(ctx, feature.ID, ActorHuman); err == nil {
		t.Fatal("land with active task was accepted")
	} else {
		var active *ErrFeatureActive
		if !errors.As(err, &active) || len(active.Offenders) != 1 || active.Offenders[0] != task.ID {
			t.Fatalf("land error = %v, want ErrFeatureActive{%s}", err, task.ID)
		}
	}
	markTaskDone(t, env, task.ID)

	landed, err := env.features.Land(ctx, feature.ID, ActorHuman)
	if err != nil {
		t.Fatalf("land: %v", err)
	}
	if landed.Status != FeatureLanded || landed.LandedAt == nil || landed.LandSHA == "" {
		t.Fatalf("landed feature = %+v", landed)
	}
	if tip := env.branchTip(t, "main"); tip != landed.LandSHA {
		t.Fatalf("main tip = %s, want land sha %s", tip, landed.LandSHA)
	}
	content, err := reconcileGitOutput("", nil, "--git-dir", env.exchangePath(), "show", landed.LandSHA+":feature.txt")
	if err != nil || strings.TrimSpace(content) != "feature work" {
		t.Fatalf("feature.txt at land sha = %q, err = %v", content, err)
	}

	// Replay returns the stamped feature without re-merging.
	replay, err := env.features.Land(ctx, feature.ID, ActorHuman)
	if err != nil {
		t.Fatalf("replay land: %v", err)
	}
	if replay.LandSHA != landed.LandSHA {
		t.Fatalf("replay land sha = %s, want %s", replay.LandSHA, landed.LandSHA)
	}
}

func TestLandEmptyFeatureHeals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "empty"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	baseTip := env.branchTip(t, "main")
	landed, err := env.features.Land(ctx, feature.ID, ActorHuman)
	if err != nil {
		t.Fatalf("land empty feature: %v", err)
	}
	if landed.Status != FeatureLanded || landed.LandSHA != baseTip {
		t.Fatalf("landed empty feature = %+v, want landed at base tip %s", landed, baseTip)
	}
	if tip := env.branchTip(t, "main"); tip != baseTip {
		t.Fatalf("main moved to %s on empty land", tip)
	}
}

func TestLandConflictedFeatureTellsOperatorToRebase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	landed, err := env.features.Land(ctx, feature.ID, ActorHuman)
	if err == nil {
		t.Fatalf("conflicted land returned %+v, want rebase-first error", landed)
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Fatalf("conflicted land error = %v, want a rebase hint", err)
	}
	reloaded, err := env.features.Get(ctx, feature.ID)
	if err != nil {
		t.Fatalf("get feature: %v", err)
	}
	if reloaded.Status != FeatureOpen {
		t.Fatalf("feature status = %q after failed land, want open", reloaded.Status)
	}
}

func TestLandRefusesRunningRebase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)

	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil || result.Kind != RebaseTaskCreated {
		t.Fatalf("rebase = %+v, %v", result, err)
	}
	if _, err := env.features.Land(ctx, feature.ID, ActorHuman); err == nil {
		t.Fatal("land during running rebase was accepted")
	} else {
		var active *ErrFeatureActive
		if !errors.As(err, &active) {
			t.Fatalf("land error = %v, want ErrFeatureActive", err)
		}
		found := false
		for _, offender := range active.Offenders {
			if offender == result.RebaseTaskID {
				found = true
			}
		}
		if !found {
			t.Fatalf("offenders = %v, want the rebase task %s named", active.Offenders, result.RebaseTaskID)
		}
	}
	if _, err := env.features.Archive(ctx, feature.ID); err == nil {
		t.Fatal("archive during running rebase was accepted")
	}
}

func TestArchiveFeature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "abandoned"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	task, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "wip", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := env.features.Archive(ctx, feature.ID); err == nil {
		t.Fatal("archive with active task was accepted")
	}
	markTaskDone(t, env, task.ID)

	archived, err := env.features.Archive(ctx, feature.ID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived.Status != FeatureArchived {
		t.Fatalf("archived feature = %+v", archived)
	}
	// Replay is a no-op.
	if _, err := env.features.Archive(ctx, feature.ID); err != nil {
		t.Fatalf("replay archive: %v", err)
	}
	// The archived feature rejects new assignments.
	fresh, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "fresh"})
	if err != nil {
		t.Fatalf("create fresh task: %v", err)
	}
	if _, err := env.tasks.EditTask(ctx, fresh.ID, EditTaskInput{FeatureID: stringPtrPtr(&feature.ID)}); !errors.Is(err, ErrFeatureClosed) {
		t.Fatalf("archived feature assignment error = %v, want ErrFeatureClosed", err)
	}
	// Default list hides nothing; the status filter finds the archived one.
	archivedList, err := env.features.List(ctx, FeatureArchived)
	if err != nil || len(archivedList) != 1 || archivedList[0].ID != feature.ID {
		t.Fatalf("archived list = %+v, %v", archivedList, err)
	}
}

func TestFeatureTasksListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)

	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "grouped"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	first, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "one", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create one: %v", err)
	}
	second, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "two", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create two: %v", err)
	}
	if _, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "unrelated"}); err != nil {
		t.Fatalf("create unrelated: %v", err)
	}

	tasks, err := env.features.Tasks(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list feature tasks: %v", err)
	}
	if len(tasks) != 2 || tasks[0].ID != first.ID || tasks[1].ID != second.ID {
		t.Fatalf("feature tasks = %+v, want the pair in order", tasks)
	}
}
