package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// makeExchangeHooksInert replaces the fixture exchange's hook scripts. The
// fixture registry points hooks at the test binary; feature operations push to
// the exchange, which would re-exec the whole test suite as a hook.
func makeExchangeHooksInert(t *testing.T, exchangePath string) {
	t.Helper()
	for _, name := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(exchangePath, "hooks", name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write inert %s hook: %v", name, err)
		}
	}
}

// seedAPIMain pushes an initial main commit into the fixture exchange.
func seedAPIMain(t *testing.T, exchangePath string) string {
	t.Helper()
	repoPath := filepath.Join(t.TempDir(), "repo")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, repoPath, "push", exchangePath, "HEAD:refs/heads/main")
	return apiGitOutput(t, repoPath, "rev-parse", "HEAD")
}

// advanceAPIMain clones the exchange, commits a file, and pushes main forward.
func advanceAPIMain(t *testing.T, exchangePath, file, contents string) string {
	t.Helper()
	clonePath := filepath.Join(t.TempDir(), "clone")
	runAPIGit(t, "", "clone", exchangePath, clonePath)
	runAPIGit(t, clonePath, "config", "user.name", "Flow Test")
	runAPIGit(t, clonePath, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, clonePath, "checkout", "-B", "main", "origin/main")
	writeAPIFile(t, clonePath, file, contents)
	runAPIGit(t, clonePath, "add", file)
	runAPIGit(t, clonePath, "commit", "-m", "advance main")
	runAPIGit(t, clonePath, "push", "origin", "HEAD:refs/heads/main")
	return apiGitOutput(t, clonePath, "rev-parse", "HEAD")
}

func apiRefTip(t *testing.T, exchangePath, branch string) string {
	t.Helper()
	return apiGitOutput(t, "", "--git-dir", exchangePath, "rev-parse", "refs/heads/"+branch)
}

func TestFeatureAPIEndpoints(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := fixture.Project.ExchangePath
	makeExchangeHooksInert(t, exchangePath)
	mainSHA := seedAPIMain(t, exchangePath)
	featuresPath := "/v2/projects/" + fixture.Project.ID + "/features"

	// Mutations require owner or console scope.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, featuresPath,
		contract.CreateFeatureRequest{Title: "payments"}, http.StatusForbidden, nil)

	var created contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath,
		contract.CreateFeatureRequest{Title: "payments", Body: "payment work"}, http.StatusCreated, &created)
	feature := created.Feature
	if feature.ID != "f-api-0001" || feature.Branch != "feature/f-api-0001" || feature.Status != coordinator.FeatureOpen {
		t.Fatalf("created feature = %+v", feature)
	}
	if created.Counts != (contract.FeatureTaskCounts{}) {
		t.Fatalf("created counts = %+v, want zero", created.Counts)
	}
	if created.BranchState == nil || created.BranchState.FeatureTipSHA != mainSHA || created.BranchState.Behind != 0 {
		t.Fatalf("created branch state = %+v, want seeded at %s", created.BranchState, mainSHA)
	}
	if tip := apiRefTip(t, exchangePath, "feature/f-api-0001"); tip != mainSHA {
		t.Fatalf("feature ref tip = %s, want %s", tip, mainSHA)
	}

	// Duplicate titles conflict.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath,
		contract.CreateFeatureRequest{Title: "payments"}, http.StatusConflict, nil)

	// List (default = open).
	var listed contract.FeaturesResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, featuresPath, nil, http.StatusOK, &listed)
	if len(listed.Features) != 1 || listed.Features[0].Feature.ID != feature.ID {
		t.Fatalf("listed features = %+v", listed.Features)
	}

	// Assign a task at creation.
	var taskResp taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		map[string]any{"title": "scoped work", "feature_id": feature.ID}, http.StatusCreated, &taskResp)
	if taskResp.Task.FeatureID == nil || *taskResp.Task.FeatureID != feature.ID {
		t.Fatalf("created task feature = %v", taskResp.Task.FeatureID)
	}
	taskID := taskResp.Task.ID

	// Unknown features are 404 on task create.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		map[string]any{"title": "bad", "feature_id": "f-api-9999"}, http.StatusNotFound, nil)

	// Detail: counts + tasks.
	var detail contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, featuresPath+"/"+feature.ID, nil, http.StatusOK, &detail)
	if detail.Counts.Open != 1 || len(detail.Tasks) != 1 || detail.Tasks[0].ID != taskID {
		t.Fatalf("feature detail = counts %+v tasks %+v", detail.Counts, detail.Tasks)
	}

	// Task edit: reassign and clear via the tri-state field.
	var edited taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPatch, "/v2/tasks/"+taskID,
		map[string]any{"feature_id": ""}, http.StatusOK, &edited)
	if edited.Task.FeatureID != nil {
		t.Fatalf("cleared task feature = %v, want nil", edited.Task.FeatureID)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPatch, "/v2/tasks/"+taskID,
		map[string]any{"feature_id": feature.ID}, http.StatusOK, &edited)
	if edited.Task.FeatureID == nil || *edited.Task.FeatureID != feature.ID {
		t.Fatalf("reassigned task feature = %v", edited.Task.FeatureID)
	}

	// Metadata edit, resolvable by title too.
	var renamed contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPatch, featuresPath+"/payments",
		contract.UpdateFeatureRequest{Title: stringPtr("payments v2")}, http.StatusOK, &renamed)
	if renamed.Feature.Title != "payments v2" {
		t.Fatalf("renamed feature = %+v", renamed.Feature)
	}

	// Rebase: up to date at first, then a clean instant rebase after main moves.
	var rebase contract.RebaseFeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath+"/"+feature.ID+"/rebase",
		nil, http.StatusOK, &rebase)
	if rebase.Result.Kind != coordinator.RebaseAlreadyUpToDate {
		t.Fatalf("rebase kind = %q, want already_up_to_date", rebase.Result.Kind)
	}
	advanceAPIMain(t, exchangePath, "main.txt", "main work")
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath+"/"+feature.ID+"/rebase",
		nil, http.StatusOK, &rebase)
	if rebase.Result.Kind != coordinator.RebaseRebased || rebase.Result.NewTipSHA == "" {
		t.Fatalf("rebase result = %+v, want rebased", rebase.Result)
	}
	if tip := apiRefTip(t, exchangePath, feature.Branch); tip != rebase.Result.NewTipSHA {
		t.Fatalf("feature tip = %s, want %s", tip, rebase.Result.NewTipSHA)
	}

	// Archive refuses while the task is open.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath+"/"+feature.ID+"/archive",
		nil, http.StatusConflict, nil)

	// Board exposes the open feature for card chips.
	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	foundBoardEntry := false
	for _, entry := range board.Features {
		if entry.ID == feature.ID && entry.Title == "payments v2" {
			foundBoardEntry = true
		}
	}
	if !foundBoardEntry {
		t.Fatalf("board features = %+v, want the open feature listed", board.Features)
	}

	// Finish the task, then land.
	if _, err := fixture.DB.Exec(
		`UPDATE tasks SET lifecycle_state = 'done', done_resolution = 'merged', done_at = '2026-01-01T00:00:00Z' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("mark task done: %v", err)
	}
	var landed contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath+"/"+feature.ID+"/land",
		nil, http.StatusOK, &landed)
	if landed.Feature.Status != coordinator.FeatureLanded || landed.Feature.LandSHA == "" || landed.Feature.LandedAt == nil {
		t.Fatalf("landed feature = %+v", landed.Feature)
	}

	// Landed features vanish from the default list, show under ?status=landed.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, featuresPath, nil, http.StatusOK, &listed)
	if len(listed.Features) != 0 {
		t.Fatalf("default list after land = %+v, want empty", listed.Features)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, featuresPath+"?status=landed", nil, http.StatusOK, &listed)
	if len(listed.Features) != 1 || listed.Features[0].Feature.ID != feature.ID {
		t.Fatalf("landed list = %+v", listed.Features)
	}

	// Archive a fresh empty feature end to end.
	var second contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath,
		contract.CreateFeatureRequest{Title: "abandoned"}, http.StatusCreated, &second)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, featuresPath+"/"+second.Feature.ID+"/archive",
		nil, http.StatusOK, &second)
	if second.Feature.Status != coordinator.FeatureArchived {
		t.Fatalf("archived feature = %+v", second.Feature)
	}

	// Unknown features are 404.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, featuresPath+"/f-api-9999", nil, http.StatusNotFound, nil)
}

func TestSessionCreatedTaskInheritsFeature(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := fixture.Project.ExchangePath
	makeExchangeHooksInert(t, exchangePath)
	seedAPIMain(t, exchangePath)
	ctx := context.Background()

	feature, err := fixture.Bundle.Features.Create(ctx, coordinator.CreateFeatureInput{Title: "scoped"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	parent, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "parent", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Mint a session token bound to the parent task, then create a child task
	// with it: the child inherits the parent's feature.
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "feature-session-token",
		Scope:        coordinator.TokenScopeSession,
		Subject:      "s-feature-test",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &parent.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}
	var child taskResponse
	doJSONRequestAs(t, fixture.Server, "feature-session-token", http.MethodPost, "/v2/tasks",
		map[string]any{"title": "child"}, http.StatusCreated, &child)
	if child.Task.FeatureID == nil || *child.Task.FeatureID != feature.ID {
		t.Fatalf("session child feature = %v, want %s", child.Task.FeatureID, feature.ID)
	}

	// An explicit feature_id still wins.
	other, err := fixture.Bundle.Features.Create(ctx, coordinator.CreateFeatureInput{Title: "other"})
	if err != nil {
		t.Fatalf("create other feature: %v", err)
	}
	var explicit taskResponse
	doJSONRequestAs(t, fixture.Server, "feature-session-token", http.MethodPost, "/v2/tasks",
		map[string]any{"title": "explicit", "feature_id": other.ID}, http.StatusCreated, &explicit)
	if explicit.Task.FeatureID == nil || *explicit.Task.FeatureID != other.ID {
		t.Fatalf("explicit child feature = %v, want %s", explicit.Task.FeatureID, other.ID)
	}
	if !strings.HasPrefix(child.Task.ID, "t-") {
		t.Fatalf("child task id = %q", child.Task.ID)
	}
}

// TestTaskBoundConsoleRebaseScope locks the rebase route into task-bound
// console confinement: a console credential bound to a task may only rebase a
// feature that holds the bound task as open work, and a conflicted rebase may
// link only the bound task as a blocker. The restriction is persisted on the
// running feature_rebases row and applied at relation-creation time inside
// RebaseOnMain and by the schedule-time gate. Two concurrent-add interleavings
// are exercised through the HTTP API as a competing principal: a task added
// after the locked scope decision but before the conflicted path's
// non-done-task snapshot enters the initial relation sweep and stays unlinked
// (relation-time confinement), and a task added before the locked decision
// rejects the rebase with 403 and no rows (the raced 403 path). Unbound
// project consoles keep project-wide rebase access.
func TestTaskBoundConsoleRebaseScope(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := fixture.Project.ExchangePath
	makeExchangeHooksInert(t, exchangePath)
	seedAPIMain(t, exchangePath)
	ctx := context.Background()

	feature, err := fixture.Bundle.Features.Create(ctx, coordinator.CreateFeatureInput{Title: "bound feature"})
	if err != nil {
		t.Fatalf("create bound feature: %v", err)
	}
	other, err := fixture.Bundle.Features.Create(ctx, coordinator.CreateFeatureInput{Title: "unrelated feature"})
	if err != nil {
		t.Fatalf("create unrelated feature: %v", err)
	}
	boundTask, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "bound task", FeatureID: &feature.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}
	rebasesPath := "/v2/projects/" + fixture.Project.ID + "/features"

	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "task-bound-console-token",
		Scope:        coordinator.TokenScopeConsole,
		Subject:      "s-bound-console",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &boundTask.ID,
	}); err != nil {
		t.Fatalf("store task-bound console token: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "project-console-token",
		Scope:        coordinator.TokenScopeConsole,
		Subject:      "s-project-console",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: nil,
	}); err != nil {
		t.Fatalf("store project console token: %v", err)
	}

	// A task-bound console cannot rebase a feature unrelated to its bound task,
	// and the rejected request creates no rebase or relation rows.
	doJSONRequestAs(t, fixture.Server, "task-bound-console-token", http.MethodPost,
		rebasesPath+"/"+other.ID+"/rebase", nil, http.StatusForbidden, nil)
	var rebaseRows int
	if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_rebases`).Scan(&rebaseRows); err != nil {
		t.Fatalf("count rebase rows: %v", err)
	}
	if rebaseRows != 0 {
		t.Fatalf("forbidden rebase created %d rebase rows, want 0", rebaseRows)
	}
	var relationRows int
	if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_item_relations WHERE kind = 'blocks'`).Scan(&relationRows); err != nil {
		t.Fatalf("count relation rows: %v", err)
	}
	if relationRows != 0 {
		t.Fatalf("forbidden rebase created %d blocker rows, want 0", relationRows)
	}

	// Rebasing the feature that holds the bound task stays allowed.
	var allowed contract.RebaseFeatureResponse
	doJSONRequestAs(t, fixture.Server, "task-bound-console-token", http.MethodPost,
		rebasesPath+"/"+feature.ID+"/rebase", nil, http.StatusOK, &allowed)
	if allowed.Result.Kind != coordinator.RebaseAlreadyUpToDate {
		t.Fatalf("bound-console rebase kind = %q, want %q", allowed.Result.Kind, coordinator.RebaseAlreadyUpToDate)
	}

	// An unbound project console keeps project-wide rebase access.
	var unbound contract.RebaseFeatureResponse
	doJSONRequestAs(t, fixture.Server, "project-console-token", http.MethodPost,
		rebasesPath+"/"+other.ID+"/rebase", nil, http.StatusOK, &unbound)
	if unbound.Result.Kind != coordinator.RebaseAlreadyUpToDate {
		t.Fatalf("project-console rebase kind = %q, want %q", unbound.Result.Kind, coordinator.RebaseAlreadyUpToDate)
	}

	// Force a conflicted rebase on the bound feature: the feature branch gains
	// a commit that conflicts with main.
	clonePath := filepath.Join(t.TempDir(), "feature-clone")
	runAPIGit(t, "", "clone", exchangePath, clonePath)
	runAPIGit(t, clonePath, "config", "user.name", "Flow Test")
	runAPIGit(t, clonePath, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, clonePath, "checkout", "-B", feature.Branch, "origin/"+feature.Branch)
	writeAPIFile(t, clonePath, "conflict.txt", "feature version")
	runAPIGit(t, clonePath, "add", "conflict.txt")
	runAPIGit(t, clonePath, "commit", "-m", "feature work")
	runAPIGit(t, clonePath, "push", "origin", "HEAD:refs/heads/"+feature.Branch)
	advanceAPIMain(t, exchangePath, "conflict.txt", "main version")

	countRows := func(table string) int {
		t.Helper()
		var n int
		if err := fixture.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		return n
	}
	// Scenario A — the guard-to-inner-read race, made deterministic: a task
	// created by another principal after the locked scope decision (and after
	// the running row is persisted) but before the conflicted path's
	// non-done-task snapshot commits in time to enter the initial relation
	// sweep; only the relation-time restriction filter keeps it unlinked. The
	// create and schedule go through the HTTP API as the owner, so the
	// competing principal is real.
	createConcurrent := func(featureID string) string {
		t.Helper()
		var created taskResponse
		recorder := httptest.NewRecorder()
		request := authorizedRequest(http.MethodPost, "/v2/tasks",
			createTaskRequest{Title: "concurrent task", FeatureID: &featureID})
		request.Header.Set("Authorization", "Bearer owner-token")
		fixture.Server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Errorf("create concurrent task via API: status = %d, body: %s", recorder.Code, recorder.Body.String())
			return ""
		}
		if err := json.NewDecoder(recorder.Body).Decode(&created); err != nil {
			t.Errorf("decode concurrent task response: %v", err)
			return ""
		}
		return created.Task.ID
	}

	var concurrentTaskID string
	fixture.Bundle.Features.RebaseOnMainTestHook = func(phase coordinator.RebaseOnMainTestPhase) {
		if phase != coordinator.RebaseOnMainAfterReservation {
			return
		}
		concurrentTaskID = createConcurrent(feature.ID)
		if concurrentTaskID == "" {
			return
		}
		// The submitted concurrent-add window ends with the task being scheduled
		// while the rebase is running. WorkflowRunService.ScheduleAs gates the
		// schedule on EnsureRebaseBlock, which consults the running row's
		// restriction: the schedule is allowed but no block relation is inserted
		// for the out-of-scope task.
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost,
			"/v2/tasks/"+concurrentTaskID+"/schedule", nil, http.StatusOK, nil)
	}
	t.Cleanup(func() { fixture.Bundle.Features.RebaseOnMainTestHook = nil })

	var conflicted contract.RebaseFeatureResponse
	doJSONRequestAs(t, fixture.Server, "task-bound-console-token", http.MethodPost,
		rebasesPath+"/"+feature.ID+"/rebase", nil, http.StatusOK, &conflicted)
	if conflicted.Result.Kind != coordinator.RebaseTaskCreated || conflicted.Result.RebaseTaskID == "" {
		t.Fatalf("bound-console conflicted rebase = %+v, want %q", conflicted.Result, coordinator.RebaseTaskCreated)
	}
	if concurrentTaskID == "" {
		t.Fatal("rebase confinement hook did not run")
	}
	if got := countRows("work_item_relations WHERE kind = 'blocks'"); got != 1 {
		t.Fatalf("conflicted rebase created %d blocker rows, want exactly 1", got)
	}

	// The only relation the conflicted rebase may insert involves the bound
	// task as its target; the concurrently added task must never be linked, so
	// no created relation has both endpoints unrelated to the bound task.
	var blockedBound, blockedConcurrent int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`,
		conflicted.Result.RebaseTaskID, boundTask.ID).Scan(&blockedBound); err != nil {
		t.Fatalf("count bound-task block: %v", err)
	}
	if blockedBound != 1 {
		t.Fatalf("rebase task blocks bound task %d times, want 1", blockedBound)
	}
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`,
		conflicted.Result.RebaseTaskID, concurrentTaskID).Scan(&blockedConcurrent); err != nil {
		t.Fatalf("count concurrent-task block: %v", err)
	}
	if blockedConcurrent != 0 {
		t.Fatalf("rebase task blocks concurrently added task %d times, want 0", blockedConcurrent)
	}

	// The in-window schedule succeeded (the gate omits the link rather than
	// erroring), and the task-bound confinement is persisted on the running
	// rebase row so the gate keeps omitting out-of-scope links for the rebase's
	// whole lifetime.
	var concurrentRuns int
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, concurrentTaskID).Scan(&concurrentRuns); err != nil {
		t.Fatalf("count concurrent-task runs: %v", err)
	}
	if concurrentRuns != 1 {
		t.Fatalf("concurrent task scheduled %d workflow runs, want 1", concurrentRuns)
	}
	var restrictedTo string
	if err := fixture.DB.QueryRowContext(ctx,
		`SELECT restrict_blocked_to FROM feature_rebases WHERE task_id = ?`, conflicted.Result.RebaseTaskID).Scan(&restrictedTo); err != nil {
		t.Fatalf("read rebase restriction: %v", err)
	}
	if restrictedTo != boundTask.ID {
		t.Fatalf("running rebase restriction = %q, want bound task %q", restrictedTo, boundTask.ID)
	}

	// The feature now holds another open task created before the request: the
	// rebase is rejected with 403 and creates no rebase or relation rows.
	if _, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "extra task", FeatureID: &feature.ID}); err != nil {
		t.Fatalf("create extra task: %v", err)
	}
	rebasesBefore := countRows("feature_rebases")
	relationsBefore := countRows("work_item_relations WHERE kind = 'blocks'")
	doJSONRequestAs(t, fixture.Server, "task-bound-console-token", http.MethodPost,
		rebasesPath+"/"+feature.ID+"/rebase", nil, http.StatusForbidden, nil)
	if got := countRows("feature_rebases"); got != rebasesBefore {
		t.Fatalf("forbidden rebase created %d rebase rows, want 0", got-rebasesBefore)
	}
	if got := countRows("work_item_relations WHERE kind = 'blocks'"); got != relationsBefore {
		t.Fatalf("forbidden rebase created %d blocker rows, want 0", got-relationsBefore)
	}

	// Scenario B — the raced 403 path: another principal adds a task after the
	// API-side scope pre-read but before the locked scope decision, and the add
	// commits before the decision. The rebase is rejected with 403 before any
	// rebase or relation row exists.
	raced, err := fixture.Bundle.Features.Create(ctx, coordinator.CreateFeatureInput{Title: "raced feature"})
	if err != nil {
		t.Fatalf("create raced feature: %v", err)
	}
	racedBound, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "raced bound task", FeatureID: &raced.ID})
	if err != nil {
		t.Fatalf("create raced bound task: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "task-bound-raced-token",
		Scope:        coordinator.TokenScopeConsole,
		Subject:      "s-raced-console",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &racedBound.ID,
	}); err != nil {
		t.Fatalf("store raced console token: %v", err)
	}
	// Force a conflicted rebase on the raced feature so the request passes the
	// Git preflight and reaches the locked scope decision.
	racedClone := filepath.Join(t.TempDir(), "raced-clone")
	runAPIGit(t, "", "clone", exchangePath, racedClone)
	runAPIGit(t, racedClone, "config", "user.name", "Flow Test")
	runAPIGit(t, racedClone, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, racedClone, "checkout", "-B", raced.Branch, "origin/"+raced.Branch)
	writeAPIFile(t, racedClone, "conflict.txt", "raced feature version")
	runAPIGit(t, racedClone, "add", "conflict.txt")
	runAPIGit(t, racedClone, "commit", "-m", "raced feature work")
	runAPIGit(t, racedClone, "push", "origin", "HEAD:refs/heads/"+raced.Branch)
	advanceAPIMain(t, exchangePath, "conflict.txt", "raced main version")

	var racedHookRan bool
	fixture.Bundle.Features.RebaseOnMainTestHook = func(phase coordinator.RebaseOnMainTestPhase) {
		if phase != coordinator.RebaseOnMainBeforeScopeCheck {
			return
		}
		racedHookRan = true
		createConcurrent(raced.ID)
	}
	t.Cleanup(func() { fixture.Bundle.Features.RebaseOnMainTestHook = nil })

	racedRebasesBefore := countRows("feature_rebases")
	racedRelationsBefore := countRows("work_item_relations WHERE kind = 'blocks'")
	doJSONRequestAs(t, fixture.Server, "task-bound-raced-token", http.MethodPost,
		rebasesPath+"/"+raced.ID+"/rebase", nil, http.StatusForbidden, nil)
	if !racedHookRan {
		t.Fatal("before-scope-check hook did not run")
	}
	if got := countRows("feature_rebases"); got != racedRebasesBefore {
		t.Fatalf("raced rebase created %d rebase rows, want 0", got-racedRebasesBefore)
	}
	if got := countRows("work_item_relations WHERE kind = 'blocks'"); got != racedRelationsBefore {
		t.Fatalf("raced rebase created %d blocker rows, want 0", got-racedRelationsBefore)
	}
}

func stringPtr(value string) *string { return &value }
