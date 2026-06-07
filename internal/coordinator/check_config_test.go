package coordinator

import (
	"context"
	"path/filepath"
	"strings"
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

func TestCheckConfigSchedulesCritiqueAndAcceptanceJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := true
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Configured checks issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	change, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, issue, ensured, "add check config", []checkConfigFile{
		{".flow/checks/ci.yaml", `
name: unit
kind: ci
required: true
entrypoint:
  argv: ["go", "test", "./..."]
requires: ["docker"]
`},
		{".flow/checks/reviewer.yaml", `
name: reviewer
kind: reviewer
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`},
		{".flow/checks/verifier.yaml", `
name: verifier
kind: verifier
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`},
	})

	scheduled, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{Issue: issue, Change: change})
	if err != nil {
		t.Fatalf("schedule review round: %v", err)
	}
	if scheduled.ChecksCreated != 4 || scheduled.JobsEnqueued != 2 {
		t.Fatalf("schedule result = %+v, want four checks and two critique jobs", scheduled)
	}
	assertCheckPending(t, checks, issue.ID, "unit", CheckKindCI)
	assertCheckPending(t, checks, issue.ID, "reviewer", CheckKindReviewer)
	assertCheckPending(t, checks, issue.ID, "verifier", CheckKindVerifier)
	assertCheckPending(t, checks, issue.ID, "human-review", CheckKindHuman)
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})

	required := true
	for _, input := range []ReportCheckInput{
		{IssueID: issue.ID, Name: "unit", Kind: CheckKindCI, Required: &required, Verdict: CheckSatisfied},
		{IssueID: issue.ID, Name: "reviewer", Kind: CheckKindReviewer, Required: &required, Verdict: CheckSatisfied},
		{IssueID: issue.ID, Name: "human-review", Kind: CheckKindHuman, Required: &required, Verdict: CheckSatisfied},
	} {
		if _, err := checks.ReportCheck(ctx, input); err != nil {
			t.Fatalf("satisfy %s: %v", input.Name, err)
		}
	}
	enqueued, err := checkConfig.EnqueueAcceptanceIfReady(ctx, issue.ID, change)
	if err != nil {
		t.Fatalf("enqueue acceptance: %v", err)
	}
	if len(enqueued) != 1 {
		t.Fatalf("acceptance enqueued = %v, want one verifier job", enqueued)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 1,
	})
}

func TestCheckConfigDefaultsReviewerAndVerifierWhenMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Default agent checks issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	writeReconcileFile(t, repoPath, "app.go", "package app\n\nconst Value = 1\n")
	if err := runReconcileGit(repoPath, nil, "add", "app.go"); err != nil {
		t.Fatalf("git add app: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "add app"); err != nil {
		t.Fatalf("commit app: %v", err)
	}
	head, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, head)
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}

	scheduled, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{Issue: issue, Change: change})
	if err != nil {
		t.Fatalf("schedule review round: %v", err)
	}
	if scheduled.ChecksCreated != 2 || scheduled.JobsEnqueued != 1 {
		t.Fatalf("schedule result = %+v, want default reviewer/verifier checks and reviewer job", scheduled)
	}
	assertCheckPending(t, checks, issue.ID, "reviewer", CheckKindReviewer)
	assertCheckPending(t, checks, issue.ID, "verifier", CheckKindVerifier)
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})
	assertLiveCheckJobEntrypointContains(t, workers, issue.ID, flowworker.RoleReviewer, "reviewer", "flow fetch-prompt")
	assertLiveCheckJobEntrypointContains(t, workers, issue.ID, flowworker.RoleReviewer, "reviewer", "codex exec")

	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy reviewer: %v", err)
	}
	enqueued, err := checkConfig.EnqueueAcceptanceIfReady(ctx, issue.ID, change)
	if err != nil {
		t.Fatalf("enqueue acceptance: %v", err)
	}
	if len(enqueued) != 1 {
		t.Fatalf("acceptance enqueued = %v, want one default verifier job", enqueued)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 1,
	})
	assertLiveCheckJobEntrypointContains(t, workers, issue.ID, flowworker.RoleVerifier, "verifier", "flow fetch-prompt")
	assertLiveCheckJobEntrypointContains(t, workers, issue.ID, flowworker.RoleVerifier, "verifier", "codex exec")

	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "verifier",
		Kind:     CheckKindVerifier,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy verifier: %v", err)
	}
	state, err := checks.ReviewState(ctx, issue.ID)
	if err != nil {
		t.Fatalf("review state: %v", err)
	}
	if state != ReviewApproved {
		t.Fatalf("review state = %q, want approved", state)
	}
}

func TestCheckConfigRecoverPendingCheckJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Recover pending checks issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	change, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, issue, ensured, "add unit check", []checkConfigFile{
		{".flow/checks/unit.yaml", `
name: unit
kind: ci
entrypoint:
  argv: ["go", "test", "./..."]
`},
	})
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		change.ID,
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}

	scheduled, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{Issue: issue, Change: change})
	if err != nil {
		t.Fatalf("schedule review round: %v", err)
	}
	if scheduled.ChecksCreated != 3 || scheduled.JobsEnqueued != 2 {
		t.Fatalf("schedule result = %+v, want unit/default reviewer/default verifier checks", scheduled)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE issue_id = ?
	AND role IN (?, ?)`,
		string(flowworker.JobCrashed),
		issue.ID,
		string(flowworker.RoleCI),
		string(flowworker.RoleReviewer),
	); err != nil {
		t.Fatalf("crash critique jobs: %v", err)
	}

	recovered, _, err := checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover pending check jobs: %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recovered = %d, want unit and reviewer jobs", recovered)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})

	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "unit",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy unit: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy reviewer: %v", err)
	}
	reviewerBefore, err := checks.GetCheck(ctx, issue.ID, "reviewer")
	if err != nil {
		t.Fatalf("get reviewer before recovery: %v", err)
	}
	recovered, _, err = checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover acceptance check job: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want verifier job", recovered)
	}
	reviewerAfter, err := checks.GetCheck(ctx, issue.ID, "reviewer")
	if err != nil {
		t.Fatalf("get reviewer after recovery: %v", err)
	}
	if reviewerAfter.Verdict != CheckSatisfied || !reviewerAfter.UpdatedAt.Equal(reviewerBefore.UpdatedAt) {
		t.Fatalf("reviewer changed during recovery: before=%+v after=%+v", reviewerBefore, reviewerAfter)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleVerifier: 1,
	})

	recovered, _, err = checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover idempotently: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("second recovery enqueued %d jobs, want 0", recovered)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleVerifier: 1,
	})
}

func TestCheckConfigRecoverIsolatesPoisonedCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checkConfig := svc.checkConfig
	requiresHuman := false

	// Healthy candidate: a real issue branch pushed to the exchange with a unit
	// check config, so its recovery succeeds.
	healthyIssue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Healthy recovery issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create healthy issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, healthyIssue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule healthy issue: %v", err)
	}
	healthyEnsured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: healthyIssue.ID})
	if err != nil {
		t.Fatalf("ensure healthy author job: %v", err)
	}
	healthyChange, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, healthyIssue, healthyEnsured, "add unit check", []checkConfigFile{
		{".flow/checks/unit.yaml", `
name: unit
kind: ci
entrypoint:
  argv: ["go", "test", "./..."]
`},
	})
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"2026-01-02T00:00:00Z",
		healthyChange.ID,
	); err != nil {
		t.Fatalf("mark healthy change ready: %v", err)
	}

	// Poisoned candidate: an accepted, ready, unmerged change whose head_sha does
	// not exist in the exchange repo, so LoadSuiteForChange fails. Its updated_at
	// sorts it first, so before fault isolation it starved the healthy candidate.
	poisonIssue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Poisoned recovery issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create poison issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, poisonIssue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule poison issue: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`,
		"ch-poison",
		poisonIssue.ID,
		"issue/"+poisonIssue.ID,
		"0000000000000000000000000000000000000000",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert poisoned change: %v", err)
	}

	recovered, _, err := checkConfig.RecoverPendingCheckJobs(ctx)
	if err == nil {
		t.Fatal("recover returned nil error, want joined poisoned-candidate error")
	}
	if !strings.Contains(err.Error(), "ch-poison") {
		t.Fatalf("error = %v, want it to reference poisoned change ch-poison", err)
	}
	// Healthy candidate enqueues unit (CI) and the default reviewer job despite
	// the poisoned candidate failing.
	if recovered != 2 {
		t.Fatalf("recovered = %d, want 2 healthy jobs despite poisoned candidate", recovered)
	}
	assertLiveCheckJobs(t, workers, healthyIssue.ID, map[flowworker.JobRole]int{
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})
}

func TestCheckConfigRecoveryCreatesMissingChecksBeforeVerifierGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Missing check recovery issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	change, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, issue, ensured, "add verifier check", []checkConfigFile{
		{".flow/checks/verifier.yaml", `
name: verifier
kind: verifier
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`},
	})
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		change.ID,
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}

	recovered, _, err := checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover missing checks: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want default reviewer job only", recovered)
	}
	assertCheckPending(t, checks, issue.ID, "reviewer", CheckKindReviewer)
	assertCheckPending(t, checks, issue.ID, "verifier", CheckKindVerifier)
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})

	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy reviewer: %v", err)
	}
	recovered, _, err = checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover verifier after critique: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want verifier job after critique", recovered)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleVerifier: 1,
	})
}

func TestCheckConfigRecoveryRefreshesPendingCheckDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Refresh pending check issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	change, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, issue, ensured, "add reused check", []checkConfigFile{
		{".flow/checks/reused.yaml", `
name: reused
kind: ci
required: false
entrypoint:
  argv: ["go", "test", "./..."]
`},
	})
	if _, err := store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	updated_at = ?
WHERE id = ?`,
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		change.ID,
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}

	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reused",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckPending,
		Reporter: "coordinator",
	}); err != nil {
		t.Fatalf("seed stale check: %v", err)
	}

	recovered, _, err := checkConfig.RecoverPendingCheckJobs(ctx)
	if err != nil {
		t.Fatalf("recover pending check jobs: %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recovered = %d, want reused CI and default reviewer jobs", recovered)
	}
	refreshed, err := checks.GetCheck(ctx, issue.ID, "reused")
	if err != nil {
		t.Fatalf("get refreshed check: %v", err)
	}
	if refreshed.Kind != CheckKindCI || refreshed.Required || refreshed.Verdict != CheckPending {
		t.Fatalf("refreshed check = %+v, want optional pending CI", refreshed)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})
}

func TestCheckConfigVerifierOnlySuiteRunsDefaultReviewerFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions := svc.issues, svc.workers, svc.sessions
	checks, checkConfig := svc.checks, svc.checkConfig
	requiresHuman := false
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Verifier only issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	change, _ := seedReadyChangeWithConfig(t, ctx, repoPath, project, sessions, issue, ensured, "add verifier check", []checkConfigFile{
		{".flow/checks/verifier.yaml", `
name: verifier
kind: verifier
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`},
	})

	scheduled, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{Issue: issue, Change: change})
	if err != nil {
		t.Fatalf("schedule verifier-only round: %v", err)
	}
	if scheduled.ChecksCreated != 2 || scheduled.JobsEnqueued != 1 {
		t.Fatalf("schedule result = %+v, want verifier plus default reviewer checks and reviewer job", scheduled)
	}
	assertCheckPending(t, checks, issue.ID, "reviewer", CheckKindReviewer)
	assertCheckPending(t, checks, issue.ID, "verifier", CheckKindVerifier)
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})

	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckSatisfied,
	}); err != nil {
		t.Fatalf("satisfy default reviewer: %v", err)
	}
	enqueued, err := checkConfig.EnqueueAcceptanceIfReady(ctx, issue.ID, change)
	if err != nil {
		t.Fatalf("enqueue acceptance: %v", err)
	}
	if len(enqueued) != 1 {
		t.Fatalf("acceptance enqueued = %v, want one configured verifier job", enqueued)
	}
	assertLiveCheckJobs(t, workers, issue.ID, map[flowworker.JobRole]int{
		flowworker.RoleAuthor:   1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 1,
	})
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

func TestScheduleReviewRoundCompletionAssessmentMarksReviewerCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	issues := NewIssueService(store.DB())
	checks := NewCheckService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	// No exchange path: LoadSuiteForChange yields an empty suite and
	// withDefaultAgentChecks supplies the default reviewer + verifier, so the
	// round runs without a real git repo.
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, nil, Project{}, CheckConfigServiceOptions{})

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Completion assessment issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, "deadbeefcafefeed0000000000000000deadbeef")
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}

	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Issue:                issue,
		Change:               change,
		CompletionAssessment: true,
	}); err != nil {
		t.Fatalf("schedule completion-assessment round: %v", err)
	}

	reviewer, err := checks.GetCheck(ctx, issue.ID, defaultReviewerCheckName)
	if err != nil {
		t.Fatalf("get reviewer check: %v", err)
	}
	if reviewer.Verdict != CheckPending || reviewer.Details != CompletionAssessmentCheckMarker {
		t.Fatalf("reviewer check = %+v, want pending with completion-assessment marker", reviewer)
	}
	// The verifier (acceptance) check must not carry the reviewer-only marker.
	verifier, err := checks.GetCheck(ctx, issue.ID, defaultVerifierCheckName)
	if err != nil {
		t.Fatalf("get verifier check: %v", err)
	}
	if verifier.Details == CompletionAssessmentCheckMarker {
		t.Fatalf("verifier check unexpectedly carries the completion-assessment marker: %+v", verifier)
	}

	// An ordinary round leaves the reviewer check details empty.
	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Issue:  issue,
		Change: change,
	}); err != nil {
		t.Fatalf("schedule ordinary round: %v", err)
	}
	reviewer, err = checks.GetCheck(ctx, issue.ID, defaultReviewerCheckName)
	if err != nil {
		t.Fatalf("get reviewer check after ordinary round: %v", err)
	}
	if reviewer.Details != "" {
		t.Fatalf("ordinary round left reviewer details = %q, want empty", reviewer.Details)
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
	issues := NewIssueService(store.DB())
	checks := NewCheckService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, flowworker.NewService(store.DB()), nil, Project{}, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Removed check issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "removed",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckPending,
	}); err != nil {
		t.Fatalf("seed removed check: %v", err)
	}
	if err := checkConfig.retireAbsentAutomatedChecks(ctx, issue.ID, CheckSuite{
		Configured: true,
		Definitions: []CheckDefinition{{
			Name: "unit",
			Kind: CheckKindCI,
		}},
	}); err != nil {
		t.Fatalf("retire absent: %v", err)
	}
	removed, err := checks.GetCheck(ctx, issue.ID, "removed")
	if err != nil {
		t.Fatalf("get removed: %v", err)
	}
	if removed.Verdict != CheckSkipped || removed.Required {
		t.Fatalf("removed check = %+v, want skipped optional", removed)
	}
}

func TestCheckConfigRetiresChecksWhenAllConfigFilesRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, workers, sessions, checks := svc.issues, svc.workers, svc.sessions, svc.checks
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, nil, project, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Removed all config issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	writeCheckConfig(t, repoPath, ".flow/checks/unit.yaml", `
name: unit
kind: ci
entrypoint:
  argv: ["go", "test", "./..."]
`)
	if err := runReconcileGit(repoPath, nil, "add", ".flow/checks"); err != nil {
		t.Fatalf("git add unit: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "add unit check"); err != nil {
		t.Fatalf("commit unit: %v", err)
	}
	firstHead, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse first head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push first head: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, firstHead)
	if err != nil {
		t.Fatalf("update first head: %v", err)
	}
	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{Issue: issue, Change: change}); err != nil {
		t.Fatalf("schedule initial round: %v", err)
	}
	assertCheckPending(t, checks, issue.ID, "unit", CheckKindCI)

	if err := runReconcileGit(repoPath, nil, "rm", ".flow/checks/unit.yaml"); err != nil {
		t.Fatalf("git rm unit: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "remove check config"); err != nil {
		t.Fatalf("commit remove config: %v", err)
	}
	secondHead, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse second head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push second head: %v", err)
	}
	change, err = sessions.UpdateChangeHead(ctx, change.ID, secondHead)
	if err != nil {
		t.Fatalf("update second head: %v", err)
	}
	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Issue:           issue,
		Change:          change,
		PreviousHeadSHA: firstHead,
	}); err != nil {
		t.Fatalf("schedule removed config round: %v", err)
	}
	removed, err := checks.GetCheck(ctx, issue.ID, "unit")
	if err != nil {
		t.Fatalf("get unit after removal: %v", err)
	}
	if removed.Verdict != CheckSkipped || removed.Required {
		t.Fatalf("removed config check = %+v, want skipped optional", removed)
	}
}

func TestCheckConfigHumanApprovalStalesOnlyForTouchedCommentedFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	repoPath := fixture.repoPath
	project := fixture.project
	store := fixture.store

	svc := wireCheckConfigServices(store, project)
	issues, sessions := svc.issues, svc.sessions
	checks, threads, checkConfig := svc.checks, svc.threads, svc.checkConfig
	requiresHuman := true
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:               "Human stale issue",
		RequiresHumanReview: &requiresHuman,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	writeReconcileFile(t, repoPath, "reviewed.go", "package app\n")
	writeReconcileFile(t, repoPath, "other.go", "package app\n")
	if err := runReconcileGit(repoPath, nil, "add", "reviewed.go", "other.go"); err != nil {
		t.Fatalf("git add initial files: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "add review target files"); err != nil {
		t.Fatalf("commit initial files: %v", err)
	}
	firstHead, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse first head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push first head: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, firstHead)
	if err != nil {
		t.Fatalf("update first head: %v", err)
	}
	if _, err := threads.CreateThread(ctx, CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: firstHead,
		FilePath:        "reviewed.go",
		Line:            1,
		Body:            "Please keep this stable.",
		Actor:           "owner",
	}); err != nil {
		t.Fatalf("create human thread: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "human-review",
		Kind:     CheckKindHuman,
		Required: &required,
		Verdict:  CheckSatisfied,
		Reporter: "owner",
	}); err != nil {
		t.Fatalf("satisfy human review: %v", err)
	}

	writeReconcileFile(t, repoPath, "other.go", "package app\n\nconst Other = true\n")
	if err := runReconcileGit(repoPath, nil, "add", "other.go"); err != nil {
		t.Fatalf("git add other: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "change unrelated file"); err != nil {
		t.Fatalf("commit unrelated: %v", err)
	}
	secondHead, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse second head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push second head: %v", err)
	}
	change, err = sessions.UpdateChangeHead(ctx, change.ID, secondHead)
	if err != nil {
		t.Fatalf("update second head: %v", err)
	}
	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Issue:           issue,
		Change:          change,
		PreviousHeadSHA: firstHead,
	}); err != nil {
		t.Fatalf("schedule after unrelated change: %v", err)
	}
	human, err := checks.GetCheck(ctx, issue.ID, "human-review")
	if err != nil {
		t.Fatalf("get human check after unrelated change: %v", err)
	}
	if human.Verdict != CheckSatisfied {
		t.Fatalf("human review verdict after unrelated change = %q, want satisfied", human.Verdict)
	}

	writeReconcileFile(t, repoPath, "reviewed.go", "package app\n\nconst Reviewed = true\n")
	if err := runReconcileGit(repoPath, nil, "add", "reviewed.go"); err != nil {
		t.Fatalf("git add reviewed: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", "change reviewed file"); err != nil {
		t.Fatalf("commit reviewed: %v", err)
	}
	thirdHead, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse third head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push third head: %v", err)
	}
	change, err = sessions.UpdateChangeHead(ctx, change.ID, thirdHead)
	if err != nil {
		t.Fatalf("update third head: %v", err)
	}
	if _, err := checkConfig.ScheduleReviewRound(ctx, ScheduleReviewRoundInput{
		Issue:           issue,
		Change:          change,
		PreviousHeadSHA: secondHead,
	}); err != nil {
		t.Fatalf("schedule after reviewed change: %v", err)
	}
	human, err = checks.GetCheck(ctx, issue.ID, "human-review")
	if err != nil {
		t.Fatalf("get human check after reviewed change: %v", err)
	}
	if human.Verdict != CheckPending {
		t.Fatalf("human review verdict after reviewed change = %q, want pending", human.Verdict)
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
	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), nil, workers, nil, Project{}, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Head scoped job issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	changeID := "ch-head-scoped-1"
	otherChangeID := "ch-head-scoped-2"
	for _, change := range []struct {
		id     string
		branch string
	}{
		{id: changeID, branch: "issue/head-scoped-1"},
		{id: otherChangeID, branch: "issue/head-scoped-2"},
	} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, 'main', 'head-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			change.id, issue.ID, change.branch); err != nil {
			t.Fatalf("insert change %s: %v", change.id, err)
		}
	}
	if _, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
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
	exists, err := checkConfig.liveCheckJobExists(ctx, issue.ID, changeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup matching head: %v", err)
	}
	if !exists {
		t.Fatal("live job not found for matching head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, issue.ID, changeID, flowworker.RoleCI, "unit", "head-2")
	if err != nil {
		t.Fatalf("lookup different head: %v", err)
	}
	if exists {
		t.Fatal("live job matched different head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, issue.ID, otherChangeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup different change: %v", err)
	}
	if exists {
		t.Fatal("live job matched different change")
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
	issues      *IssueService
	workers     *flowworker.Service
	sessions    *SessionService
	checks      *CheckService
	threads     *ThreadService
	checkConfig *CheckConfigService
}

// wireCheckConfigServices constructs the standard service graph used across the
// check-config tests, sharing the same DB handle and project.
func wireCheckConfigServices(store *flowdb.Store, project Project) checkConfigServices {
	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	checks := NewCheckService(store.DB())
	threads := NewThreadService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, threads, project, CheckConfigServiceOptions{})
	return checkConfigServices{
		issues:      issues,
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

// seedReadyChangeWithConfig creates the issue branch, writes the given check
// config files, commits and pushes them to the exchange, and advances the
// change head to the pushed commit. It returns the updated change and the
// pushed head SHA. commitMessage is the message used for the seed commit.
func seedReadyChangeWithConfig(t *testing.T, ctx context.Context, repoPath string, project Project, sessions *SessionService, issue Issue, ensured EnsureAuthorJobResult, commitMessage string, configs []checkConfigFile) (Change, string) {
	t.Helper()
	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
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
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, head)
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}
	return change, head
}

func assertCheckPending(t *testing.T, checks *CheckService, issueID string, name string, kind CheckKind) {
	t.Helper()
	check, err := checks.GetCheck(context.Background(), issueID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	if check.Kind != kind || check.Verdict != CheckPending || !check.Required {
		t.Fatalf("check %s = %+v", name, check)
	}
}

func assertLiveCheckJobs(t *testing.T, workers *flowworker.Service, issueID string, want map[flowworker.JobRole]int) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	counts := map[flowworker.JobRole]int{}
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID {
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

func assertLiveCheckJobEntrypointContains(t *testing.T, workers *flowworker.Service, issueID string, role flowworker.JobRole, checkName string, snippet string) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID || job.Role != role {
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
