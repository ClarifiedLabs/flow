package coordinator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestDefaultAgentChecksUseSelectedHarnessAndArgs(t *testing.T) {
	suite, err := withDefaultAgentChecks(CheckSuite{}, flowharness.Claude, flowharness.Args{
		Claude: []string{"--model", "sonnet"},
		Codex:  []string{"--model", "gpt-5", "-c", "model_reasoning_effort=high"},
	})
	if err != nil {
		t.Fatalf("default agent checks: %v", err)
	}
	if len(suite.Definitions) != 2 {
		t.Fatalf("default definitions = %+v, want reviewer and verifier", suite.Definitions)
	}
	for _, definition := range suite.Definitions {
		if definition.Entrypoint == nil || len(definition.Entrypoint.Argv) != 1 {
			t.Fatalf("%s entrypoint = %+v", definition.Name, definition.Entrypoint)
		}
		command := definition.Entrypoint.Argv[0]
		for _, want := range []string{"claude --dangerously-skip-permissions --permission-mode bypassPermissions", "'--model' 'sonnet'", "--harness claude"} {
			if !strings.Contains(command, want) {
				t.Fatalf("%s default command missing %q:\n%s", definition.Name, want, command)
			}
		}
		if got := definition.Requires; len(got) != 1 || got[0] != flowharness.AgentHarnessLabel(flowharness.Claude) {
			t.Fatalf("%s requires = %#v, want claude harness label", definition.Name, got)
		}
	}
}

func TestCheckConfigInvalidEntrypointFailsClearly(t *testing.T) {
	t.Parallel()
	_, err := parseCheckDefinition(".flow/checks/bad.yaml", `
name: bad
kind: ci
`)
	if err == nil || !strings.Contains(err.Error(), "entrypoint is required") {
		t.Fatalf("parse err = %v, want missing entrypoint", err)
	}
}

func TestCheckConfigRetiresRemovedAutomatedChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	tasks := NewTaskService(store.DB(), "p-test")
	checks := NewCheckService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, flowworker.NewService(store.DB()), nil, Project{}, CheckConfigServiceOptions{})
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Removed check task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		TaskID:   task.ID,
		Name:     "removed",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckPending,
	}); err != nil {
		t.Fatalf("seed removed check: %v", err)
	}
	if err := checkConfig.retireAbsentAutomatedChecks(ctx, task.ID, CheckSuite{
		Configured: true,
		Definitions: []CheckDefinition{{
			Name: "unit",
			Kind: CheckKindCI,
		}},
	}); err != nil {
		t.Fatalf("retire absent: %v", err)
	}
	removed, err := checks.GetCheck(ctx, task.ID, "removed")
	if err != nil {
		t.Fatalf("get removed: %v", err)
	}
	if removed.Verdict != CheckSkipped || removed.Required {
		t.Fatalf("removed check = %+v, want skipped optional", removed)
	}
}

func TestLiveCheckJobExistsIsScopedToHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	tasks := NewTaskService(store.DB(), "p-test")
	workers := flowworker.NewService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), nil, workers, nil, Project{}, CheckConfigServiceOptions{})
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Head scoped job task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	changeID := "ch-head-scoped-1"
	otherChangeID := "ch-head-scoped-2"
	for _, change := range []struct {
		id     string
		branch string
	}{
		{id: changeID, branch: "task/head-scoped-1"},
		{id: otherChangeID, branch: "task/head-scoped-2"},
	} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, 'main', 'head-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			change.id, task.ID, change.branch); err != nil {
			t.Fatalf("insert change %s: %v", change.id, err)
		}
	}
	if _, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		ChangeID:       &changeID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"check_name": "unit",
			"head_sha":   "head-1",
		},
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	exists, err := checkConfig.liveCheckJobExists(ctx, task.ID, changeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup matching head: %v", err)
	}
	if !exists {
		t.Fatal("live job not found for matching head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, task.ID, changeID, flowworker.RoleCI, "unit", "head-2")
	if err != nil {
		t.Fatalf("lookup different head: %v", err)
	}
	if exists {
		t.Fatal("live job matched different head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, task.ID, otherChangeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup different change: %v", err)
	}
	if exists {
		t.Fatal("live job matched different change")
	}
}

func TestScheduleWorkflowNodeChecksConcurrentlyFansOutExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project := Project{ID: "p-test", Name: "test", BaseBranch: "main"}
	services := wireCheckConfigServices(store, project)
	task, err := services.tasks.CreateTask(ctx, CreateTaskInput{Title: "Parallel review"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	change := Change{ID: "ch-parallel", TaskID: task.ID, Branch: "task/parallel", Base: "main", HeadSHA: "head-parallel"}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		change.ID, change.TaskID, change.Branch, change.Base, change.HeadSHA); err != nil {
		t.Fatalf("insert change: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, transition_budget, created_at, started_at
) VALUES ('wr-parallel', ?, 1, '{}', 'running', 'review', 'nr-parallel', 50,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES ('nr-parallel', 'wr-parallel', 'review', 1, 1, 'running',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, task.ID); err != nil {
		t.Fatalf("insert workflow ownership: %v", err)
	}
	agents := []SnapshotReviewAgent{
		{Blocking: true, Agent: AgentDefSnapshot{Name: "code-review", Harness: "codex", Model: "gpt-5", ReasoningEffort: "high", Prompt: "Focus on correctness."}},
		{Blocking: false, Agent: AgentDefSnapshot{Name: "security-review", Harness: "claude", Model: "claude-sonnet-4-6", Prompt: "Focus on security."}},
	}

	const attempts = 24
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names, err := services.checkConfig.ScheduleWorkflowNodeChecks(ctx, task, change, WorkflowChecksReview, agents, "wr-parallel", "nr-parallel")
			if err != nil {
				errs <- err
				return
			}
			if len(names) != 2 || names[0] != "code-review.node.nr-parallel" || names[1] != "security-review.node.nr-parallel" {
				errs <- fmt.Errorf("check names = %v", names)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("schedule checks: %v", err)
	}

	jobs, err := services.workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v, want exactly two", jobs)
	}
	for _, job := range jobs {
		if job.State != flowworker.JobQueued || job.Role != flowworker.RoleReviewer || job.TaskID == nil || *job.TaskID != task.ID || job.ChangeID == nil || *job.ChangeID != change.ID {
			t.Errorf("job identity = %+v", job)
		}
		if job.WorkflowRunID == nil || *job.WorkflowRunID != "wr-parallel" || job.NodeRunID == nil || *job.NodeRunID != "nr-parallel" {
			t.Errorf("job workflow ownership = %+v", job)
		}
		name := payloadString(job.Payload, "check_name")
		if payloadString(job.Payload, "head_sha") != change.HeadSHA {
			t.Errorf("job %s head = %#v", name, job.Payload["head_sha"])
		}
		entrypoint, _ := job.Payload["entrypoint"].(map[string]any)
		command := fmt.Sprint(entrypoint["argv"])
		switch name {
		case "code-review.node.nr-parallel":
			if payloadString(job.Payload, "role_instructions") != "Focus on correctness." || !strings.Contains(command, "gpt-5") || job.Payload["blocking"] != true {
				t.Errorf("code review payload = %+v", job.Payload)
			}
		case "security-review.node.nr-parallel":
			if payloadString(job.Payload, "role_instructions") != "Focus on security." || !strings.Contains(command, "claude-sonnet-4-6") || job.Payload["blocking"] != false {
				t.Errorf("security review payload = %+v", job.Payload)
			}
		default:
			t.Errorf("unexpected check job %q", name)
		}
	}
	checks, err := services.checks.ListChecks(ctx, task.ID)
	if err != nil {
		t.Fatalf("list checks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, check := range checks {
		wantRequired := check.Name == "code-review.node.nr-parallel"
		if check.Kind != CheckKindReviewer || check.Verdict != CheckPending || check.Required != wantRequired {
			t.Errorf("check = %+v, required want %v", check, wantRequired)
		}
	}

	blocking := true
	advisory := false
	if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{TaskID: task.ID, Name: "code-review.node.nr-parallel", Kind: CheckKindReviewer, Required: &blocking, Verdict: CheckSatisfied}); err != nil {
		t.Fatalf("satisfy prior blocking check: %v", err)
	}
	if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{TaskID: task.ID, Name: "security-review.node.nr-parallel", Kind: CheckKindReviewer, Required: &advisory, Verdict: CheckBlocked, Details: "Advisory cache finding."}); err != nil {
		t.Fatalf("block prior advisory check: %v", err)
	}
	// A scheduler that observed this visit before the reports must not reset
	// either terminal result when it resumes.
	if _, err := services.checkConfig.ScheduleWorkflowNodeChecks(ctx, task, change, WorkflowChecksReview, agents, "wr-parallel", "nr-parallel"); err != nil {
		t.Fatalf("repeat first-visit schedule after reports: %v", err)
	}
	preserved, err := services.checks.GetCheck(ctx, task.ID, "security-review.node.nr-parallel")
	if err != nil || preserved.Verdict != CheckBlocked || preserved.Required || preserved.Details != "Advisory cache finding." {
		t.Fatalf("advisory result after stale schedule = %+v err=%v", preserved, err)
	}
	if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{TaskID: task.ID, Name: "stale.node.nr-parallel", Kind: CheckKindReviewer, Required: &blocking, Verdict: CheckPending}); err != nil {
		t.Fatalf("seed stale pending check: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE jobs SET state = 'finished' WHERE node_run_id = 'nr-parallel';
UPDATE workflow_node_runs SET state = 'succeeded', outcome = 'changes_requested' WHERE id = 'nr-parallel';
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES ('nr-parallel-visit-2', 'wr-parallel', 'review', 2, 1, 'running',
	'2026-01-01T00:01:00Z', '2026-01-01T00:01:00Z');
UPDATE workflow_runs SET current_node_run_id = 'nr-parallel-visit-2' WHERE id = 'wr-parallel'`); err != nil {
		t.Fatalf("enter second review visit: %v", err)
	}
	secondNames, err := services.checkConfig.ScheduleWorkflowNodeChecks(ctx, task, change, WorkflowChecksReview, agents, "wr-parallel", "nr-parallel-visit-2")
	if err != nil {
		t.Fatalf("schedule second visit: %v", err)
	}
	if len(secondNames) != 2 || secondNames[0] != "code-review.node.nr-parallel-visit-2" || secondNames[1] != "security-review.node.nr-parallel-visit-2" {
		t.Fatalf("second visit check names = %v", secondNames)
	}
	priorSatisfied, err := services.checks.GetCheck(ctx, task.ID, "code-review.node.nr-parallel")
	if err != nil || priorSatisfied.Verdict != CheckSatisfied || !priorSatisfied.Required {
		t.Fatalf("prior satisfied check = %+v err=%v", priorSatisfied, err)
	}
	priorAdvisory, err := services.checks.GetCheck(ctx, task.ID, "security-review.node.nr-parallel")
	if err != nil || priorAdvisory.Verdict != CheckBlocked || priorAdvisory.Required || priorAdvisory.Details != "Advisory cache finding." {
		t.Errorf("historical advisory check = %+v err=%v", priorAdvisory, err)
	}
	retired, err := services.checks.GetCheck(ctx, task.ID, "stale.node.nr-parallel")
	if err != nil || retired.Verdict != CheckSkipped || retired.Required || !strings.Contains(retired.Details, "retired by a later visit") {
		t.Errorf("retired blocking check = %+v err=%v", retired, err)
	}
	var liveJobs int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state IN ('queued', 'claimed', 'running')`).Scan(&liveJobs); err != nil {
		t.Fatalf("count second-visit jobs: %v", err)
	}
	if liveJobs != 2 {
		t.Fatalf("live jobs after revisit = %d, want two fresh jobs", liveJobs)
	}

	// A stale scheduling call for the prior review node must not retire checks
	// created by an adjacent verification node.
	for _, name := range secondNames {
		required := name == "code-review.node.nr-parallel-visit-2"
		if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{TaskID: task.ID, Name: name, Kind: CheckKindReviewer, Required: &required, Verdict: CheckSatisfied}); err != nil {
			t.Fatalf("complete second-visit check %s: %v", name, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE jobs SET state = 'finished' WHERE node_run_id = 'nr-parallel-visit-2';
UPDATE workflow_node_runs SET state = 'succeeded', outcome = 'approved' WHERE id = 'nr-parallel-visit-2';
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES ('nr-verify', 'wr-parallel', 'verify', 1, 1, 'running',
	'2026-01-01T00:02:00Z', '2026-01-01T00:02:00Z');
UPDATE workflow_runs SET current_node_key = 'verify', current_node_run_id = 'nr-verify' WHERE id = 'wr-parallel'`); err != nil {
		t.Fatalf("enter verification node: %v", err)
	}
	verifiers := []SnapshotReviewAgent{{Blocking: true, Agent: AgentDefSnapshot{Name: "correctness-verifier", Harness: "codex", Prompt: "Verify the fixes."}}}
	verifyNames, err := services.checkConfig.ScheduleWorkflowNodeChecks(ctx, task, change, WorkflowChecksVerify, verifiers, "wr-parallel", "nr-verify")
	if err != nil || len(verifyNames) != 1 {
		t.Fatalf("schedule verification checks = %v err=%v", verifyNames, err)
	}
	if _, err := services.checkConfig.ScheduleWorkflowNodeChecks(ctx, task, change, WorkflowChecksReview, agents, "wr-parallel", "nr-parallel-visit-2"); err != nil {
		t.Fatalf("stale prior-node schedule: %v", err)
	}
	verifier, err := services.checks.GetCheck(ctx, task.ID, verifyNames[0])
	if err != nil || verifier.Verdict != CheckPending || !verifier.Required {
		t.Fatalf("verification check after stale prior-node schedule = %+v err=%v", verifier, err)
	}
}

func TestCheckConfigValidationRejectsEscapingCWDAndReservedEnv(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"cwd": `
name: bad-cwd
kind: ci
entrypoint:
  argv: ["go", "test"]
  cwd: "../outside"
`,
		"env": `
name: bad-env
kind: ci
entrypoint:
  argv: ["go", "test"]
  env:
    flow_token: secret
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCheckDefinition(".flow/checks/"+name+".yaml", config); err == nil {
				t.Fatal("parse succeeded, want validation failure")
			}
		})
	}
}

func writeCheckConfig(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()
	writeReconcileFile(t, repoPath, relativePath, strings.TrimSpace(contents)+"\n")
}

// checkConfigServices bundles the services wired together for the
// check-config tests so call sites can pull out only what they need.
type checkConfigServices struct {
	tasks       *TaskService
	workers     *flowworker.Service
	sessions    *SessionService
	checks      *CheckService
	threads     *ThreadService
	checkConfig *CheckConfigService
}

// wireCheckConfigServices constructs the standard service graph used across the
// check-config tests, sharing the same DB handle and project.
func wireCheckConfigServices(store *flowdb.Store, project Project) checkConfigServices {
	tasks := NewTaskService(store.DB(), "p-test")
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), tasks, workers)
	checks := NewCheckService(store.DB())
	threads := NewThreadService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, threads, project, CheckConfigServiceOptions{})
	return checkConfigServices{
		tasks:       tasks,
		workers:     workers,
		sessions:    sessions,
		checks:      checks,
		threads:     threads,
		checkConfig: checkConfig,
	}
}

// checkConfigFile is an ordered (path, content) pair seeded into .flow/checks.
type checkConfigFile struct {
	path    string
	content string
}

// seedReadyChangeWithConfig creates the task branch, writes the given check
// config files, commits and pushes them to the exchange, and advances the
// change head to the pushed commit. It returns the updated change and the
// pushed head SHA. commitMessage is the message used for the seed commit.
func seedReadyChangeWithConfig(t *testing.T, ctx context.Context, repoPath string, project Project, sessions *SessionService, task Task, ensured EnsureAuthorJobResult, commitMessage string, configs []checkConfigFile) (Change, string) {
	t.Helper()
	branch := "task/" + task.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout task branch: %v", err)
	}
	for _, config := range configs {
		writeCheckConfig(t, repoPath, config.path, config.content)
	}
	if err := runReconcileGit(repoPath, nil, "add", ".flow/checks"); err != nil {
		t.Fatalf("git add checks: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", commitMessage); err != nil {
		t.Fatalf("commit checks: %v", err)
	}
	head, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("push task branch: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, head)
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}
	return change, head
}

func assertCheckPending(t *testing.T, checks *CheckService, taskID string, name string, kind CheckKind) {
	t.Helper()
	check, err := checks.GetCheck(context.Background(), taskID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	if check.Kind != kind || check.Verdict != CheckPending || !check.Required {
		t.Fatalf("check %s = %+v", name, check)
	}
}

func assertLiveCheckJobs(t *testing.T, workers *flowworker.Service, taskID string, want map[flowworker.JobRole]int) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	counts := map[flowworker.JobRole]int{}
	for _, job := range jobs {
		if job.TaskID == nil || *job.TaskID != taskID {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			counts[job.Role]++
		}
	}
	for role, expected := range want {
		if counts[role] != expected {
			t.Fatalf("live %s jobs = %d, want %d; all counts=%+v", role, counts[role], expected, counts)
		}
	}
}

func assertLiveCheckJobEntrypointContains(t *testing.T, workers *flowworker.Service, taskID string, role flowworker.JobRole, checkName string, snippet string) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, job := range jobs {
		if job.TaskID == nil || *job.TaskID != taskID || job.Role != role {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			if payloadString(job.Payload, "check_name") != checkName {
				continue
			}
			entrypoint, ok := job.Payload["entrypoint"].(map[string]any)
			if !ok {
				t.Fatalf("%s job entrypoint = %#v", checkName, job.Payload["entrypoint"])
			}
			if !argvContains(entrypoint["argv"], snippet) {
				t.Fatalf("%s job argv = %#v, want snippet %q", checkName, entrypoint["argv"], snippet)
			}
			return
		}
	}
	t.Fatalf("live %s job %q not found", role, checkName)
}

func argvContains(value any, snippet string) bool {
	switch argv := value.(type) {
	case []any:
		for _, arg := range argv {
			if text, ok := arg.(string); ok && strings.Contains(text, snippet) {
				return true
			}
		}
	case []string:
		for _, arg := range argv {
			if strings.Contains(arg, snippet) {
				return true
			}
		}
	}
	return false
}
