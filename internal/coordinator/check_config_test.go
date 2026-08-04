package coordinator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestDefaultAgentChecksUseSelectedHarnessAndArgs(t *testing.T) {
	suite, err := withDefaultAgentChecks(CheckSuite{}, flowharness.AgentSelection{Harness: flowharness.Harness}, []string{
		"--model", "anthropic:claude-sonnet-4-6",
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
		for _, want := range []string{"flow fetch-prompt --harness harness", "harness --session \"$FLOW_HARNESS_SESSION\" '--model' 'anthropic:claude-sonnet-4-6' -i \"$prompt\""} {
			if !strings.Contains(command, want) {
				t.Fatalf("%s default command missing %q:\n%s", definition.Name, want, command)
			}
		}
		if !definition.flowAgent {
			t.Fatalf("%s is not marked as a Flow-owned agent check", definition.Name)
		}
		if got := definition.Requires; len(got) != 1 || got[0] != flowharness.AgentHarnessLabel(flowharness.Harness) {
			t.Fatalf("%s requires = %#v, want harness harness label", definition.Name, got)
		}
	}
}

func TestDefaultAgentChecksUseConfiguredDefaultModel(t *testing.T) {
	suite, err := withDefaultAgentChecks(CheckSuite{}, flowharness.AgentSelection{
		Harness:         flowharness.Harness,
		Model:           "anthropic:claude-sonnet-4-6",
		ReasoningEffort: "high",
	}, []string{
		"--model", "openai:gpt-5",
	})
	if err != nil {
		t.Fatalf("default agent checks: %v", err)
	}
	if len(suite.Definitions) != 2 {
		t.Fatalf("default definitions = %+v, want reviewer and verifier", suite.Definitions)
	}
	for _, definition := range suite.Definitions {
		command := definition.Entrypoint.Argv[0]
		// The configured default model/effort tokens precede the manual
		// harness_args so the manual --model wins (last-token-wins).
		defaultIdx := strings.Index(command, "'--model' 'anthropic:claude-sonnet-4-6' '--reasoning' 'high'")
		manualIdx := strings.Index(command, "'--model' 'openai:gpt-5'")
		if defaultIdx < 0 || manualIdx < 0 {
			t.Fatalf("%s default command missing default or manual model tokens:\n%s", definition.Name, command)
		}
		if defaultIdx > manualIdx {
			t.Fatalf("%s default model tokens must precede harness_args:\n%s", definition.Name, command)
		}
		if got := definition.Requires; len(got) != 1 || got[0] != flowharness.AgentHarnessLabel(flowharness.Harness) {
			t.Fatalf("%s requires = %#v, want harness harness label", definition.Name, got)
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

func TestValidateReviewDiscoveryAgentsReservesAggregationCheckName(t *testing.T) {
	agents := []SnapshotReviewAgent{{Agent: AgentDefSnapshot{Name: ReviewAggregationCheckName}}}
	if err := validateReviewDiscoveryAgents(agents); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("validate review discovery agents error = %v, want reserved-name rejection", err)
	}
	if err := validateReviewDiscoveryAgents([]SnapshotReviewAgent{{Agent: AgentDefSnapshot{Name: "review-aggregator"}}}); err != nil {
		t.Fatalf("dedicated aggregator-style discovery name rejected: %v", err)
	}
}

func TestScheduleWorkflowNodeChecksConcurrentlyFansOutAndAggregatesExactlyOnce(t *testing.T) {
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
	openCandidate, err := services.tasks.CreateTask(ctx, CreateTaskInput{
		Title: "Bound the shared cache",
		Body:  "Add a size limit and eviction coverage.",
	})
	if err != nil {
		t.Fatalf("create open task candidate: %v", err)
	}
	doneCandidate, err := services.tasks.CreateTask(ctx, CreateTaskInput{Title: "Completed cache cleanup"})
	if err != nil {
		t.Fatalf("create completed task candidate: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks
SET lifecycle_state = 'done', done_resolution = 'completed', done_at = '2026-01-01T00:00:00Z'
WHERE id = ?`, doneCandidate.ID); err != nil {
		t.Fatalf("complete task candidate: %v", err)
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
		{Blocking: true, Agent: AgentDefSnapshot{Name: "code-review", Harness: "harness", Model: "openai:gpt-5", ReasoningEffort: "high", Prompt: "Focus on correctness."}},
		{Blocking: false, Agent: AgentDefSnapshot{Name: "security-review", Harness: "harness", Model: "anthropic:claude-sonnet-4-6", Prompt: "Focus on security."}},
	}
	aggregator := AgentDefSnapshot{Name: "review-aggregator", Harness: "harness", Model: "openai:gpt-5-mini", ReasoningEffort: "medium", Prompt: "Synthesize the reports."}

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
		t.Fatalf("jobs = %+v, want two parallel reviewers", jobs)
	}
	for _, job := range jobs {
		checkName := payloadString(job.Payload, "check_name")
		if job.State != flowworker.JobQueued || job.Role != flowworker.RoleReviewer || job.TaskID == nil || *job.TaskID != task.ID || job.ChangeID == nil || *job.ChangeID != change.ID {
			t.Errorf("job identity = %+v", job)
		}
		if job.WorkflowRunID == nil || *job.WorkflowRunID != "wr-parallel" || job.NodeRunID == nil || *job.NodeRunID != "nr-parallel" {
			t.Errorf("job workflow ownership = %+v", job)
		}
		name := checkName
		if payloadString(job.Payload, "head_sha") != change.HeadSHA {
			t.Errorf("job %s head = %#v", name, job.Payload["head_sha"])
		}
		entrypoint, _ := job.Payload["entrypoint"].(map[string]any)
		command := fmt.Sprint(entrypoint["argv"])
		switch name {
		case "code-review.node.nr-parallel":
			if payloadString(job.Payload, "role_instructions") != "Focus on correctness." || !strings.Contains(command, "openai:gpt-5") || !strings.Contains(command, `-i "$prompt"`) || job.Payload["blocking"] != true || job.Payload["review_discovery"] != true || payloadString(job.Payload, "completion_protocol") != checkverdict.CompletionProtocol || payloadString(job.Payload, "completion_mode") != string(checkverdict.ModeReviewDiscovery) {
				t.Errorf("code review payload = %+v", job.Payload)
			}
		case "security-review.node.nr-parallel":
			if payloadString(job.Payload, "role_instructions") != "Focus on security." || !strings.Contains(command, "anthropic:claude-sonnet-4-6") || !strings.Contains(command, `-i "$prompt"`) || job.Payload["blocking"] != false || job.Payload["review_discovery"] != true || payloadString(job.Payload, "completion_protocol") != checkverdict.CompletionProtocol || payloadString(job.Payload, "completion_mode") != string(checkverdict.ModeReviewDiscovery) {
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
		if check.Kind != CheckKindReviewer || check.Verdict != CheckPending || check.Required != wantRequired ||
			check.Details != ReviewDiscoveryDetailsMarker {
			t.Errorf("check = %+v, want pending discovery required %v", check, wantRequired)
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
	aggregationName, err := services.checkConfig.ScheduleWorkflowReviewAggregation(
		ctx,
		task,
		change,
		agents,
		aggregator,
		[]string{"code-review.node.nr-parallel", "security-review.node.nr-parallel"},
		"wr-parallel",
		"nr-parallel",
	)
	if err != nil {
		t.Fatalf("schedule aggregation: %v", err)
	}
	if aggregationName != "review-aggregation.node.nr-parallel" {
		t.Fatalf("aggregation name = %q", aggregationName)
	}
	aggregation, err := services.checks.GetCheck(ctx, task.ID, aggregationName)
	if err != nil || !aggregation.Required || aggregation.Verdict != CheckPending ||
		!strings.Contains(aggregation.Details, "Advisory cache finding.") ||
		!strings.Contains(aggregation.Details, "blocking source") ||
		!strings.Contains(aggregation.Details, "## Open Task Candidates") ||
		!strings.Contains(aggregation.Details, openCandidate.ID+" — Bound the shared cache") ||
		strings.Contains(aggregation.Details, doneCandidate.ID) ||
		strings.Contains(aggregation.Details, task.ID+" — Parallel review") {
		t.Fatalf("aggregation check = %+v err=%v", aggregation, err)
	}
	codeSource, err := services.checks.GetCheck(ctx, task.ID, "code-review.node.nr-parallel")
	if err != nil || codeSource.Required {
		t.Fatalf("aggregated code source = %+v err=%v, want advisory", codeSource, err)
	}
	jobs, err = services.workers.ListJobs(ctx)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("jobs after aggregation = %+v err=%v", jobs, err)
	}
	var aggregateJob flowworker.Job
	for _, job := range jobs {
		if payloadString(job.Payload, "check_name") == aggregationName {
			aggregateJob = job
		}
	}
	entrypoint, _ := aggregateJob.Payload["entrypoint"].(map[string]any)
	if aggregateJob.ID == "" || aggregateJob.Payload["blocking"] != true ||
		aggregateJob.Payload["review_aggregation"] != true ||
		aggregateJob.Payload["review_discovery"] != nil ||
		payloadString(aggregateJob.Payload, "completion_protocol") != checkverdict.CompletionProtocol ||
		payloadString(aggregateJob.Payload, "completion_mode") != string(checkverdict.ModeReviewAggregation) ||
		payloadString(aggregateJob.Payload, "role_instructions") != "Synthesize the reports." ||
		!strings.Contains(fmt.Sprint(entrypoint["argv"]), "gpt-5-mini") {
		t.Fatalf("aggregation job = %+v, want dedicated blocking aggregator runtime", aggregateJob)
	}
	if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{
		TaskID: task.ID, Name: aggregationName, Kind: CheckKindReviewer,
		Required: &blocking, Verdict: CheckSatisfied,
	}); err != nil {
		t.Fatalf("complete aggregation: %v", err)
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
	if err != nil || priorSatisfied.Verdict != CheckSatisfied || priorSatisfied.Required {
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
		t.Fatalf("live jobs after revisit = %d, want two fresh parallel reviewers", liveJobs)
	}

	// A stale scheduling call for the prior review node must not retire checks
	// created by an adjacent verification node.
	for _, name := range secondNames {
		required := name == "code-review.node.nr-parallel-visit-2"
		if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{TaskID: task.ID, Name: name, Kind: CheckKindReviewer, Required: &required, Verdict: CheckSatisfied}); err != nil {
			t.Fatalf("complete second-visit check %s: %v", name, err)
		}
	}
	secondAggregation, err := services.checkConfig.ScheduleWorkflowReviewAggregation(
		ctx,
		task,
		change,
		agents,
		aggregator,
		secondNames,
		"wr-parallel",
		"nr-parallel-visit-2",
	)
	if err != nil {
		t.Fatalf("schedule second aggregation: %v", err)
	}
	if _, err := services.checks.ReportCheck(ctx, ReportCheckInput{
		TaskID: task.ID, Name: secondAggregation, Kind: CheckKindReviewer,
		Required: &blocking, Verdict: CheckSatisfied,
	}); err != nil {
		t.Fatalf("complete second aggregation: %v", err)
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
	verifiers := []SnapshotReviewAgent{{Blocking: true, Agent: AgentDefSnapshot{Name: "correctness-verifier", Harness: "harness", Prompt: "Verify the fixes."}}}
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
