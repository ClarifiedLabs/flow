package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/handoff"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

var flowRuntimeEnvKeys = []string{
	"FLOW_WORKER_ROLE",
	"FLOW_ROLE",
	"FLOW_ISSUE_ID",
	"FLOW_CHANGE_ID",
	"FLOW_BRANCH",
	"FLOW_BASE",
	"FLOW_CHECK_NAME",
	"FLOW_WORKER_HARNESS",
	"FLOW_SESSION_PURPOSE",
	"FLOW_COORDINATOR_URL",
	"FLOW_SESSION_TOKEN",
	"FLOW_WORKER_TOKEN",
	"FLOW_OWNER_TOKEN",
	"FLOW_PROJECT_ID",
	"FLOW_PROJECT_NAME",
	"FLOW_SESSION_ID",
	"FLOW_REVIEW_CYCLE_INSTRUCTIONS",
	"FLOW_CONSOLE_SCOPE",
	"FLOW_PROTOCOL_VERSION",
	"FLOW_DATA_DIR",
	"FLOW_TRANSCRIPT_FILE",
}

func TestMain(m *testing.M) {
	for _, key := range flowRuntimeEnvKeys {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create stdin temp file: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("write stdin temp file: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek stdin temp file: %v", err)
	}
	original := os.Stdin
	os.Stdin = file
	defer func() {
		os.Stdin = original
		_ = file.Close()
	}()
	fn()
}

func TestLogLevelFlagEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "debug", "--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestLogLevelEnvironmentEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestInvalidLogLevelFails(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "verbose", "--version"}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("exitCode = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "invalid log level") {
		t.Fatalf("stderr missing invalid log level error: %q", stderr.String())
	}
}

func clearFetchPromptEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range flowRuntimeEnvKeys {
		t.Setenv(key, "")
	}
}

func TestDoctorInitializesDatabase(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "global.db")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"doctor", "--db", dbPath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}

	output := stdout.String()
	// flow doctor opens the coordinator-wide (global) database, whose schema
	// is applied by the single global migration.
	for _, want := range []string{"flow doctor", "sqlite: ok", "0001_global_init"} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, output)
		}
	}

	store, err := db.OpenGlobal(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer store.Close()

	migrations, err := store.AppliedMigrations(context.Background())
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertMigrationsInclude(t, migrations, "0001_global_init")
}

func TestFetchPromptUsesWorkerRoleEnvironment(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_ISSUE_ID", "i-0001")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_CHECK_NAME", "reviewer")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Flow role instructions (flow-reviewer):",
		"# Flow Reviewer",
		"Issue: i-0001",
		"Change: ch-1",
		"Check: reviewer",
		"Use flow comment",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptIncludesIssueDetailsFromAPI(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	issue, err := client.CreateIssue(flowclient.CreateIssueInput{
		Title:              "Prompt details issue",
		Body:               "Build the prompt with complete issue context.",
		AcceptanceCriteria: "The agent can start work without calling issue show.",
		PlanMode:           true,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_COORDINATOR_URL", serverURL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Issue: " + issue.ID,
		"Issue Title: Prompt details issue",
		"Issue Body:\nBuild the prompt with complete issue context.",
		"Acceptance Criteria:\nThe agent can start work without calling issue show.",
		"Plan Mode:",
		"flow status --kind plan",
		"Do not make code changes",
		"Flow will finish this planning session",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptOmitsPlanInstructionsAfterPlanApproval(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fixture := newFlowTestFixture(t)
	issue, err := fixture.Issues.CreateIssue(context.Background(), coordinator.CreateIssueInput{
		Title:    "Approved plan issue",
		Body:     "Implement the already-approved plan.",
		PlanMode: true,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.MarkPlanApproved(context.Background(), issue.ID); err != nil {
		t.Fatalf("mark plan approved: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Issue: " + issue.ID,
		"Issue Title: Approved plan issue",
		"Issue Body:\nImplement the already-approved plan.",
		"Implement the requested change",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{
		"Plan Mode:",
		"flow status --kind plan",
		"Before making any changes, create a plan",
	} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("fetch-prompt output included %q after plan approval:\n%s", unwanted, output)
		}
	}
}

func TestFetchPromptContinuesWhenIssueContextFetchFails(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_ISSUE_ID", "i-0001")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_CHECK_NAME", "reviewer")
	t.Setenv("FLOW_COORDINATOR_URL", serverURL)
	t.Setenv("FLOW_WORKER_TOKEN", "not-a-valid-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Flow role instructions (flow-reviewer):",
		"# Flow Reviewer",
		"Issue: i-0001",
		"Check: reviewer",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(stderr.String(), "continuing without issue context") {
		t.Fatalf("fetch-prompt stderr missing enrichment warning: %q", stderr.String())
	}
}

func TestFetchPromptIncludesAuthorFixRoundContextFromAPI(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	issues := fixture.Issues
	checks := fixture.Checks
	sessions := fixture.Sessions
	threads := fixture.Threads
	issue, err := issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title: "Terminal link recovery",
		Body:  "Original request that should not hide the fix context.",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	required := true
	exitCode := 1
	sourceJobID := ensured.Job.ID
	blocked, err := checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:     issue.ID,
		Name:        "merge-conflict",
		Kind:        coordinator.CheckKindCI,
		Required:    &required,
		Verdict:     coordinator.CheckBlocked,
		ExitCode:    &exitCode,
		Details:     "branch conflicts with base main\nconflicting file: internal/worker/worker.go",
		SourceJobID: &sourceJobID,
		Reporter:    "flow-merge",
	})
	if err != nil {
		t.Fatalf("report blocked check: %v", err)
	}
	thread, err := threads.CreateThread(ctx, coordinator.CreateThreadInput{
		ChangeID:        ensured.Change.ID,
		AnchorCommitSHA: "head-1",
		FilePath:        "internal/worker/worker.go",
		Line:            128,
		Context:         "merge conflict markers remain",
		Body:            "Resolve the conflict before ready.",
		Actor:           "reviewer",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := threads.ClaimThread(ctx, coordinator.ClaimThreadInput{
		ThreadID: thread.ID,
		Kind:     coordinator.ClaimFixed,
		Actor:    "author",
	}); err != nil {
		t.Fatalf("claim thread: %v", err)
	}
	reopened, err := threads.ReopenThread(ctx, coordinator.VerifyThreadInput{
		ThreadID: thread.ID,
		Body:     "Still conflicts after rebase.",
		Actor:    "verifier",
	})
	if err != nil {
		t.Fatalf("reopen thread: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_CHANGE_ID", ensured.Change.ID)
	t.Setenv("FLOW_BRANCH", ensured.Change.Branch)
	t.Setenv("FLOW_BASE", ensured.Change.Base)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCodeResult := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCodeResult != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCodeResult, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Round: fix/rework",
		"Review State: changes_requested",
		"Blocked Required Checks:",
		"- merge-conflict (Check ID: " + strconv.FormatInt(blocked.ID, 10),
		"Reporter: flow-merge",
		"Source Job: " + sourceJobID,
		"Details: branch conflicts with base main",
		"conflicting file: internal/worker/worker.go",
		"Open/Reopened Review Threads:",
		"- " + reopened.ID + " at internal/worker/worker.go:128 (State: reopened; Created By: reviewer)",
		"Latest Comment by verifier: Still conflicts after rebase.",
		"Issue Body:\nOriginal request that should not hide the fix context.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptInjectsPriorHandoffFromCoordinator(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title: "Resume work issue",
		Body:  "Continue from the prior session.",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	// Seed the prior handoff in the coordinator (the sole store now that the
	// committed .handoff.md is gone).
	priorHandoff := handoff.RenderTemplate(handoff.TemplateInput{
		IssueID:               issue.ID,
		ChangeID:              ensured.Change.ID,
		CurrentGoal:           "Resume the migration work.",
		CompletedWork:         "Phase 1 done.",
		RemainingWork:         "Phase 2.",
		TestsRun:              "go test ./...",
		FailedApproaches:      "None.",
		ImportantFiles:        "cmd/flow/main.go",
		NextRecommendedAction: "Start phase 2.",
	})
	if err := fixture.Reconciler.UpsertHandoffSnapshot(ctx, ensured.Change.ID, "deadbeef", priorHandoff); err != nil {
		t.Fatalf("seed handoff snapshot: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_CHANGE_ID", ensured.Change.ID)
	t.Setenv("FLOW_BRANCH", ensured.Change.Branch)
	t.Setenv("FLOW_BASE", ensured.Change.Base)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Prior Handoff (from the previous session") {
		t.Fatalf("fetch-prompt missing prior handoff section:\n%s", output)
	}
	if !strings.Contains(output, "Resume the migration work.") {
		t.Fatalf("fetch-prompt missing prior handoff body:\n%s", output)
	}
}

func TestFetchPromptReviewerRendersCompletionAssessmentFromMarker(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)

	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title: "Crashed-but-finished work",
		Body:  "Assess whether the crashed author actually finished.",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}

	// The coordinator stamps the completion-assessment marker onto the reviewer
	// check when routing a crashed author to a targeted review.
	required := true
	if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     coordinator.CheckKindReviewer,
		Required: &required,
		Verdict:  coordinator.CheckPending,
		Details:  coordinator.CompletionAssessmentCheckMarker,
		Reporter: "coordinator",
	}); err != nil {
		t.Fatalf("seed completion-assessment reviewer check: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_CHANGE_ID", ensured.Change.ID)
	t.Setenv("FLOW_CHECK_NAME", "reviewer")
	t.Setenv("FLOW_BRANCH", ensured.Change.Branch)
	t.Setenv("FLOW_BASE", ensured.Change.Base)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"Completion Assessment:",
		"ended without finalizing",
		"whether the task is actually complete",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("reviewer prompt missing completion-assessment guidance %q:\n%s", want, output)
		}
	}
}

func TestInitDoesNotSeedRepositorySkills(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runFlowTestGit(t, repoPath, "config", "user.email", "flow@example.com")
	runFlowTestGit(t, repoPath, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runFlowTestGit(t, repoPath, "add", "README.md")
	runFlowTestGit(t, repoPath, "commit", "-m", "seed")
	subdir := filepath.Join(repoPath, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init", "--repo", subdir}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("init exitCode = %d, want 1; stdout = %q stderr = %q", exitCode, stdout.String(), stderr.String())
	}

	resolvedRepoPath, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	if !strings.Contains(stdout.String(), "repo: "+resolvedRepoPath) {
		t.Fatalf("init output missing repo path:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "connect to flow-server") {
		t.Fatalf("init stderr missing connection failure:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote repository skills; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".codex", "skills")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote harness skills; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, ".flow")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote into subdir; stat err = %v", err)
	}
}

func TestFetchPromptUsesEmbeddedAuthorInstructions(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", "i-0002")
	t.Setenv("FLOW_WORKER_HARNESS", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	for _, want := range []string{
		"Flow role instructions (flow-author):",
		"# Flow Author",
		"Issue: i-0002",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "Use $flow-author") {
		t.Fatalf("fetch-prompt output still references an external skill:\n%s", stdout.String())
	}
}

func TestFetchPromptUsesEmbeddedVerifierInstructions(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "verifier")
	t.Setenv("FLOW_WORKER_HARNESS", "claude")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	for _, want := range []string{
		"Flow role instructions (flow-verifier):",
		"# Flow Verifier",
		"flow thread certify",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFetchPromptAcceptsHarnessConvention(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_WORKER_HARNESS", "harness")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Flow role instructions (flow-author):") {
		t.Fatalf("fetch-prompt output missing author instructions:\n%s", stdout.String())
	}
}

func TestFetchPromptHarnessFlagOverridesEnvironment(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_WORKER_HARNESS", "invalid-harness")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Flow role instructions (flow-reviewer):") {
		t.Fatalf("fetch-prompt output missing reviewer instructions:\n%s", stdout.String())
	}
}

func TestFetchPromptRejectsUnsupportedRole(t *testing.T) {
	for _, role := range []string{"ci", "console"} {
		clearFetchPromptEnvironment(t)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run([]string{"fetch-prompt", "--role", role}, &stdout, &stderr)
		if exitCode != 2 {
			t.Fatalf("fetch-prompt --role %s exitCode = %d, want 2; stdout=%q stderr=%q", role, exitCode, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "unsupported Flow worker role") {
			t.Fatalf("fetch-prompt --role %s stderr = %q", role, stderr.String())
		}
	}
}

func TestIssueCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{
		"issue", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "CLI issue",
		"--requires-human-review=false",
		"--auto-merge=true",
		"--agent-harness", "claude",
		"--claude-arg=--model",
		"--claude-arg=sonnet",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tbacklog\taccepted\tCLI issue") {
		t.Fatalf("create output = %q", stdout.String())
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	created, err := client.GetIssue("i-0001")
	if err != nil {
		t.Fatalf("get created issue: %v", err)
	}
	if created.RequiresHumanReview || !created.AutoMerge || created.AgentHarness != "claude" {
		t.Fatalf("created flags = human:%t auto:%t harness:%s, want false/true/claude", created.RequiresHumanReview, created.AutoMerge, created.AgentHarness)
	}
	if got := created.HarnessArgs.Claude; len(got) != 2 || got[0] != "--model" || got[1] != "sonnet" {
		t.Fatalf("created claude harness args = %#v", got)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{
		"issue", "edit",
		"--server", serverURL,
		"--token", "owner-token",
		"--requires-human-review=true",
		"--auto-merge=false",
		"--agent-harness", "codex",
		"--codex-arg=--model",
		"--codex-arg=gpt-5",
		"--clear-claude-args",
		"i-0001",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue edit flags exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	edited, err := client.GetIssue("i-0001")
	if err != nil {
		t.Fatalf("get edited issue: %v", err)
	}
	if !edited.RequiresHumanReview || edited.AutoMerge || edited.AgentHarness != "codex" {
		t.Fatalf("edited flags = human:%t auto:%t harness:%s, want true/false/codex", edited.RequiresHumanReview, edited.AutoMerge, edited.AgentHarness)
	}
	if got := edited.HarnessArgs.Codex; len(got) != 2 || got[0] != "--model" || got[1] != "gpt-5" {
		t.Fatalf("edited codex harness args = %#v", got)
	}
	if len(edited.HarnessArgs.Claude) != 0 {
		t.Fatalf("edited claude harness args = %#v, want cleared", edited.HarnessArgs.Claude)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{
		"issue", "edit",
		"--server", serverURL,
		"--token", "owner-token",
		"--agent-harness", "harness",
		"i-0001",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue edit harness exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	edited, err = client.GetIssue("i-0001")
	if err != nil {
		t.Fatalf("get harness edited issue: %v", err)
	}
	if edited.AgentHarness != "harness" {
		t.Fatalf("edited harness = %q, want harness", edited.AgentHarness)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "schedule", "--server", serverURL, "--token", "owner-token", "i-0001", "up_next"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue schedule exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tup_next\taccepted\tCLI issue") {
		t.Fatalf("schedule output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "show", "--server", serverURL, "--token", "owner-token", "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue show exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tup_next\taccepted\tCLI issue") {
		t.Fatalf("show output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"board", "--server", serverURL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("board exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "up_next:\n  i-0001\tup_next\taccepted\tCLI issue") {
		t.Fatalf("board output = %q", stdout.String())
	}
}

func TestIssueCreateUsesDiscoveredClientConfigOwnerToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	dataDir := t.TempDir()
	if err := os.WriteFile(config.OwnerTokenPath(dataDir), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "create", "--title", "Discovered CLI issue"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tbacklog\taccepted\tDiscovered CLI issue") {
		t.Fatalf("create output = %q", stdout.String())
	}
}

func TestIssueRelationCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	source, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Source issue"})
	if err != nil {
		t.Fatalf("create source issue: %v", err)
	}
	target, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Target issue"})
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "link", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue link exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("link stdout = %q", stdout.String())
	}
	relations, err := fixture.Issues.RelationsForIssue(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after link: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceIssueID != source.ID || relations[0].Kind != coordinator.RelationBlocks {
		t.Fatalf("relations after link = %+v", relations)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "unlink", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue unlink exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("unlink stdout = %q", stdout.String())
	}
	relations, err = fixture.Issues.RelationsForIssue(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after unlink: %v", err)
	}
	if len(relations) != 0 {
		t.Fatalf("relations after unlink = %+v, want none", relations)
	}
}

func TestIssueCreateUploadsInitialAttachment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	filePath := filepath.Join(t.TempDir(), "initial.txt")
	if err := os.WriteFile(filePath, []byte("initial attachment"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	var sawCreate bool
	var sawAttachment bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/issues":
			sawCreate = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issue":{"ID":"i-0001","Title":"With file","ScheduleState":"backlog","TriageState":"accepted"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/issues/i-0001/attachments":
			sawAttachment = true
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if got := r.FormValue("stage"); got != "initial" {
				t.Fatalf("stage = %q, want initial", got)
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("form file: %v", err)
			}
			defer file.Close()
			if header.Filename != "initial.txt" {
				t.Fatalf("filename = %q", header.Filename)
			}
			content, err := io.ReadAll(file)
			if err != nil {
				t.Fatalf("read uploaded file: %v", err)
			}
			if string(content) != "initial attachment" {
				t.Fatalf("uploaded content = %q", string(content))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"attachment":{"id":"att-0001","issue_id":"i-0001","stage":"initial","filename":"initial.txt","content_type":"text/plain; charset=utf-8","size_bytes":18}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "create", "--server", server.URL, "--token", "owner-token", "--title", "With file", "--file", filePath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !sawCreate || !sawAttachment {
		t.Fatalf("sawCreate=%t sawAttachment=%t", sawCreate, sawAttachment)
	}
	if !strings.Contains(stdout.String(), "att-0001\tinitial\tinitial.txt\t18") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestIssueAttachUsesInferredRoleAndLease(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("FLOW_ROLE", "reviewer")
	t.Setenv("FLOW_LEASE_ID", "l-0001")
	filePath := filepath.Join(t.TempDir(), "review.png")
	if err := os.WriteFile(filePath, []byte("png"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/issues/i-0001/attachments" {
			t.Fatalf("request = %s %s, want POST /v1/issues/i-0001/attachments", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("lease_id"); got != "l-0001" {
			t.Fatalf("lease_id = %q, want l-0001", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if got := r.FormValue("stage"); got != "reviewer" {
			t.Fatalf("stage = %q, want reviewer", got)
		}
		_, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if header.Filename != "review.png" || header.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("file header = %+v", header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"attachment":{"id":"att-0002","issue_id":"i-0001","stage":"reviewer","filename":"review.png","content_type":"image/png","size_bytes":3}}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "attach", "--server", server.URL, "--token", "owner-token", "--file", filePath, "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue attach exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "att-0002\treviewer\treview.png\t3") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestIssueCreateDiscoveryIgnoresAmbientFlowSessionEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssueCreateUsesDiscoveredClientConfigOwnerToken$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"FLOW_COORDINATOR_URL=http://127.0.0.1:1",
		"FLOW_SESSION_TOKEN=leaked-session-token",
		"FLOW_SESSION_ID=s-live",
		"FLOW_ISSUE_ID=i-live",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child test used ambient Flow session environment: %v\n%s", err, string(output))
	}
}

func TestPrintBoardAnnotatesSubStateAndBlocked(t *testing.T) {
	result := coordinator.BoardResult{
		Board: coordinator.Board{
			Backlog: []coordinator.Issue{
				{ID: "i-0001", ScheduleState: coordinator.ScheduleBacklog, TriageState: coordinator.TriagePending, Title: "Untriaged"},
			},
			InProgress: []coordinator.Issue{
				{ID: "i-0003", ScheduleState: coordinator.ScheduleUpNext, TriageState: coordinator.TriageAccepted, Title: "Reviewing"},
			},
			NeedsAttention: []coordinator.Issue{
				{ID: "i-0002", ScheduleState: coordinator.ScheduleUpNext, TriageState: coordinator.TriageAccepted, Title: "Blocked next"},
				{ID: "i-0004", ScheduleState: coordinator.ScheduleUpNext, TriageState: coordinator.TriageAccepted, Title: "Mergeable"},
			},
		},
		LaneStates: map[string]coordinator.LaneState{
			"i-0001": coordinator.LaneStateTriage,
			"i-0002": coordinator.LaneStateUpNext,
			"i-0003": coordinator.LaneStateInReview,
			"i-0004": coordinator.LaneStateReadyToMerge,
		},
		BlockedIDs: []string{"i-0002"},
	}

	var out bytes.Buffer
	printBoard(&out, result)

	want := "backlog:\n" +
		"  i-0001\tbacklog\ttriage\tUntriaged\n" +
		"up_next:\n" +
		"in_progress:\n" +
		"  i-0003\tup_next\taccepted\tReviewing\t[in review]\n" +
		"needs_attention:\n" +
		"  i-0002\tup_next\taccepted\tBlocked next\t[blocked]\n" +
		"  i-0004\tup_next\taccepted\tMergeable\t[ready to merge]\n"
	if out.String() != want {
		t.Fatalf("board output = %q, want %q", out.String(), want)
	}
}

func TestIssueCommandRejectsUnauthorizedToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "list", "--server", serverURL, "--token", "wrong"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("issue list exitCode = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "unauthorized") {
		t.Fatalf("stderr = %q, want unauthorized error", stderr.String())
	}
}

func TestSessionEnvironmentPrefersSessionTokenThenWorkerToken(t *testing.T) {
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:8421")
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")
	t.Setenv("FLOW_WORKER_TOKEN", "worker-token")
	t.Setenv("FLOW_SESSION_ID", "s-env")

	values := &apiFlagValues{}
	var sessionID string
	applySessionEnvironment(values, &sessionID)
	if values.serverURL != "http://127.0.0.1:8421" || values.protocolVersion != "2" {
		t.Fatalf("api flags = %+v", values)
	}
	if values.token != "session-token" {
		t.Fatalf("token = %q, want session token", values.token)
	}
	if sessionID != "s-env" {
		t.Fatalf("sessionID = %q, want env session", sessionID)
	}

	t.Setenv("FLOW_SESSION_TOKEN", "")
	values = &apiFlagValues{}
	applySessionEnvironment(values, nil)
	if values.token != "worker-token" {
		t.Fatalf("token = %q, want worker token fallback", values.token)
	}
}

func TestIssueShowUsesSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Run outside any git repo so the CLI leaves issue routes unscoped instead
	// of looking up a project for the cwd.
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/issues/i-0001" {
			t.Fatalf("request = %s %s, want GET /v1/issues/i-0001", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue":{"ID":"i-0001","Title":"Session issue","ScheduleState":"up_next","TriageState":"accepted"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "show", "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tup_next\taccepted\tSession issue") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestMergeCommandUsesAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name     string
		target   string
		wantPath string
	}{
		{name: "issue", target: "i-0001", wantPath: "/v1/issues/i-0001/merge"},
		{name: "change", target: "ch-0001", wantPath: "/v1/changes/ch-0001/merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tc.wantPath {
					t.Fatalf("request = %s %s, want POST %s", r.Method, r.URL.Path, tc.wantPath)
				}
				if r.Header.Get("Authorization") != "Bearer owner-token" {
					t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"merge":{"issue":{"id":"i-0001","title":"Merge target","schedule_state":"closed"},"change":{"id":"ch-0001","issue_id":"i-0001","branch":"issue/i-0001","base":"main","head_sha":"head-sha"},"head_sha":"head-sha","merge_sha":"merge-sha"}}`))
			}))
			t.Cleanup(server.Close)

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := run([]string{"merge", "--server", server.URL, "--token", "owner-token", tc.target}, &stdout, &stderr)
			if exitCode != 0 {
				t.Fatalf("merge exitCode = %d, stderr = %q", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), "i-0001\tch-0001\tmerge-sha\thead-sha") {
				t.Fatalf("merge output = %q", stdout.String())
			}
		})
	}
}

func TestUICommandPrintsBrowserLoginURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ui/bootstrap" {
			t.Fatalf("request = %s %s, want POST /v1/ui/bootstrap", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login_path":"/ui/login?token=abc123","expires_at":"2026-06-07T12:10:00Z"}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"ui", "--server", server.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("ui exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != server.URL+"/ui/login?token=abc123" {
		t.Fatalf("ui output = %q", stdout.String())
	}
}

func TestWorkerAndJobDiagnosticsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	issues := fixture.Issues
	checks := fixture.Checks
	issue, err := issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Diagnostics issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"agent.harness.codex": "true", "local": "true"},
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       2,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       7,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	exitFailure := 1
	if _, err := checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "fake-ci",
		ExitCode: &exitFailure,
		Reporter: "worker:w-local",
	}); err != nil {
		t.Fatalf("report check: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"workers", "--server", httpServer.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("workers exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "w-local\tregistered\tpersistent_agent=1\tephemeral=2\tlabels=agent.harness.codex=true,local=true") {
		t.Fatalf("workers output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"jobs", "--server", httpServer.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("jobs exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), job.ID+"\tqueued\tauthor\tpersistent_agent\tissue="+issue.ID+"\tpriority=7") {
		t.Fatalf("jobs output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"checks", "--server", httpServer.URL, "--token", "owner-token", issue.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("checks exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	checksOutput := stdout.String()
	if !strings.Contains(checksOutput, "review_state: changes_requested") || !strings.Contains(checksOutput, "fake-ci\tci\tblocked\trequired=true\texit_code=1\treporter=worker:w-local") {
		t.Fatalf("checks output = %q", checksOutput)
	}
}

func TestReviewRunUsesAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost || r.URL.Path != "/v1/issues/i-0002/review/run" {
			t.Fatalf("request = %s %s, want POST /v1/issues/i-0002/review/run", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"change":{"ID":"ch-1","IssueID":"i-0002","Branch":"issue/i-0002","Base":"main","HeadSHA":"abc"},
			"scheduled":{"checks_created":2,"jobs_enqueued":1},
			"review_state":"in_review",
			"checks":[{"id":1,"issue_id":"i-0002","name":"reviewer","kind":"reviewer","required":true,"verdict":"pending","reporter":"coordinator"}]
		}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"review", "run", "--server", server.URL, "--token", "owner-token", "i-0002"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("review run exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !sawRequest {
		t.Fatal("server did not receive review run request")
	}
	output := stdout.String()
	for _, want := range []string{
		"change: ch-1",
		"checks_created: 2",
		"jobs_enqueued: 1",
		"review_state: in_review",
		"reviewer\treviewer\tpending",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("review run output missing %q:\n%s", want, output)
		}
	}
}

func TestIssueStateUsesAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var requestBody string
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost || r.URL.Path != "/v1/issues/i-0002/state" {
			t.Fatalf("request = %s %s, want POST /v1/issues/i-0002/state", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue":{"ID":"i-0002","Title":"CLI state","ScheduleState":"backlog","TriageState":"accepted"}}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "state", "--server", server.URL, "--token", "owner-token", "i-0002", "backlog"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue state exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !sawRequest {
		t.Fatal("server did not receive issue state request")
	}
	if requestBody != `{"state":"backlog"}`+"\n" {
		t.Fatalf("request body = %q", requestBody)
	}
	if !strings.Contains(stdout.String(), "i-0002\tbacklog\taccepted\tCLI state") {
		t.Fatalf("issue state output = %q", stdout.String())
	}
}

func TestHandoffWriteRendersTemplateToStdout(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	// No session environment: handoff write is a pure offline render that emits
	// the handoff to stdout and writes no repo file.
	t.Setenv("FLOW_ISSUE_ID", "i-0001")
	t.Setenv("FLOW_CHANGE_ID", "")
	t.Setenv("FLOW_SESSION_ID", "")
	t.Setenv("FLOW_BRANCH", "issue/i-0001")
	t.Setenv("FLOW_BASE", "main")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{
		"handoff",
		"write",
		"--goal", "Finish Phase 7.",
		"--completed", "Added handoff command.",
		"--remaining", "Run tests.",
		"--tests", "Not yet.",
		"--failed-approaches", "None.",
		"--files", "cmd/flow/main.go",
		"--next", "Run go test.",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("handoff write exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if err := handoff.Validate(stdout.String()); err != nil {
		t.Fatalf("validate rendered handoff: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Finish Phase 7.") {
		t.Fatalf("handoff stdout missing goal:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, ".handoff.md")); !os.IsNotExist(err) {
		t.Fatalf("handoff write created a repo file, want none (stat err = %v)", err)
	}
}

func TestHandoffWriteEagerlySyncsToCoordinator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixtureWithProtocol(t, "1")
	started := startCLIAuthorSession(t, fixture, "Eager handoff write issue")

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "1")
	t.Setenv("FLOW_SESSION_ID", started.Session.ID)
	t.Setenv("FLOW_SESSION_TOKEN", started.Token)
	t.Setenv("FLOW_ISSUE_ID", started.Session.IssueID)
	t.Setenv("FLOW_CHANGE_ID", started.Change.ID)
	t.Setenv("FLOW_BRANCH", started.Change.Branch)
	t.Setenv("FLOW_BASE", started.Change.Base)

	workDir := t.TempDir()
	t.Chdir(workDir)
	initFlowTestGitRepo(t, workDir)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{
		"handoff", "write",
		"--goal", "Sync eagerly on write.",
		"--completed", "Wrote the command.",
		"--remaining", "Run the tests.",
		"--tests", "go test passed.",
		"--failed-approaches", "None.",
		"--files", "cmd/flow/main.go",
		"--next", "Mark ready.",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("handoff write exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("unexpected sync warning on stderr: %q", stderr.String())
	}

	got, err := fixture.Reconciler.GetHandoffSnapshot(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get handoff snapshot: %v", err)
	}
	if !got.Present || !got.Valid || got.Summary != "Sync eagerly on write." {
		t.Fatalf("eager handoff snapshot = %+v", got)
	}
}

func TestHookIngestDefaultModeSwallowsCoordinatorFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusInternalServerError)
	}))
	server.Close()
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	withStdin(t, `{"hook_event_name":"Stop"}`, func() {
		exitCode := run([]string{"hook", "codex", "ingest"}, &stdout, &stderr)
		if exitCode != 0 {
			t.Fatalf("hook ingest exitCode = %d, stderr = %q", exitCode, stderr.String())
		}
	})
	if stdout.String() != "{}\n" {
		t.Fatalf("hook ingest stdout = %q, want protocol-safe JSON", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("hook ingest stderr = %q, want empty", stderr.String())
	}
}

func TestHookIngestStrictModeRequiresSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:1")
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var exitCode int
	withStdin(t, `{"hook_event_name":"Stop"}`, func() {
		exitCode = run([]string{"hook", "claude", "ingest", "--strict"}, &stdout, &stderr)
	})
	if exitCode == 0 {
		t.Fatalf("hook ingest strict exitCode = 0, want nonzero")
	}
	if stdout.String() != "{}\n" {
		t.Fatalf("hook ingest strict stdout = %q, want protocol-safe JSON", stdout.String())
	}
	if !strings.Contains(stderr.String(), "FLOW_SESSION_ID and FLOW_SESSION_TOKEN are required") {
		t.Fatalf("hook ingest strict stderr = %q", stderr.String())
	}
}

// startCLIAuthorSession drives the registry through schedule → claim → running →
// start so CLI tests get a live author session with a session token and change.
func startCLIAuthorSession(t *testing.T, fixture flowTestFixture, title string) coordinator.StartAuthorSessionResult {
	t.Helper()
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: title})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"agent.harness.codex": "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil || !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim job: ok=%t err=%v job=%+v", ok, err, claimed.Job)
	}
	if _, err := fixture.Queue.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return started
}

func TestReadyCommandUploadsTranscriptBeforeRevokingSessionToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixtureWithProtocol(t, "1")
	repointFlowTestFixtureExchange(t, fixture, "")
	started := startCLIAuthorSession(t, fixture, "Ready transcript issue")

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "1")
	t.Setenv("FLOW_SESSION_ID", started.Session.ID)
	t.Setenv("FLOW_SESSION_TOKEN", started.Token)
	t.Setenv("FLOW_ISSUE_ID", started.Session.IssueID)
	t.Setenv("FLOW_CHANGE_ID", started.Change.ID)
	t.Setenv("FLOW_BRANCH", started.Change.Branch)
	t.Setenv("FLOW_BASE", started.Change.Base)

	workDir := t.TempDir()
	t.Chdir(workDir)
	initReadyWorktree(t, workDir, started.Change.Branch)
	handoffContents := handoff.RenderTemplate(handoff.TemplateInput{
		IssueID:               started.Session.IssueID,
		ChangeID:              started.Change.ID,
		SessionID:             started.Session.ID,
		Branch:                started.Change.Branch,
		Base:                  started.Change.Base,
		CurrentGoal:           "Persist ready-time transcripts.",
		CompletedWork:         "Uploaded transcript before ready.",
		RemainingWork:         "Review the change.",
		TestsRun:              "go test pending.",
		FailedApproaches:      "None.",
		ImportantFiles:        "cmd/flow/main.go",
		NextRecommendedAction: "Review.",
	})

	transcriptPath := filepath.Join(t.TempDir(), "transcript.log")
	transcript := "author pane line before ready\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	t.Setenv("FLOW_TRANSCRIPT_FILE", transcriptPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var exitCode int
	withStdin(t, handoffContents, func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 0 {
		t.Fatalf("ready exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "transcript sync") {
		t.Fatalf("unexpected transcript sync warning: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\tfinished\t"+started.Change.ID) {
		t.Fatalf("ready output = %q", stdout.String())
	}

	ownerClient, err := flowclient.New(config.ClientConfig{
		ServerURL:       httpServer.URL,
		Token:           "owner-token",
		ProtocolVersion: "1",
	})
	if err != nil {
		t.Fatalf("create owner client: %v", err)
	}
	gotTranscript, err := ownerClient.SessionTranscript(started.Session.ID)
	if err != nil {
		t.Fatalf("download transcript after ready: %v", err)
	}
	if gotTranscript != transcript {
		t.Fatalf("transcript = %q, want %q", gotTranscript, transcript)
	}
	session, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.TranscriptPath == "" {
		t.Fatalf("session transcript path was not recorded")
	}
	if _, err := fixture.Credentials.Authenticate(ctx, started.Token); err == nil {
		t.Fatalf("session token still authenticates after ready")
	}
}

func TestReadyCommandUsesSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixtureWithProtocol(t, "2")
	repointFlowTestFixtureExchange(t, fixture, "")
	issues := fixture.Issues
	sessions := fixture.Sessions
	issue, err := issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Ready CLI issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"agent.harness.codex": "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.Queue.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")
	t.Setenv("FLOW_SESSION_ID", started.Session.ID)
	t.Setenv("FLOW_SESSION_TOKEN", started.Token)
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
	t.Setenv("FLOW_CHANGE_ID", started.Change.ID)
	t.Setenv("FLOW_BRANCH", started.Change.Branch)
	t.Setenv("FLOW_BASE", started.Change.Base)

	workDir := t.TempDir()
	t.Chdir(workDir)
	initReadyWorktree(t, workDir, started.Change.Branch)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var exitCode int
	withStdin(t, "", func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 1 {
		t.Fatalf("ready without handoff exitCode = %d, want 1; stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "handoff validation") {
		t.Fatalf("missing handoff stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"session", "event", "waiting"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("session event exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\twaiting\t"+started.Change.ID) {
		t.Fatalf("session event output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"hook", "codex", "resume"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("hook event exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\tworking\t"+started.Change.ID+"\tcodex:resume") {
		t.Fatalf("hook event output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"attach", "--server", httpServer.URL, "--token", "owner-token", "--print-command", started.Session.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("attach exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tmux attach-session -t flow-"+claimed.Job.ID) {
		t.Fatalf("attach output = %q", stdout.String())
	}
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-review-cli",
		Labels:                  map[string]string{"agent.harness.codex": "true", "worker_id": "w-review-cli"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register review worker: %v", err)
	}
	reviewerJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleReviewer,
		CapacityBucket: flowworker.BucketPersistentAgent,
		RunsOn:         map[string]string{"worker_id": "w-review-cli"},
	})
	if err != nil {
		t.Fatalf("enqueue reviewer job: %v", err)
	}
	reviewerClaim, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-review-cli",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim reviewer job: %v", err)
	}
	if !ok || reviewerClaim.Job.ID != reviewerJob.ID {
		t.Fatalf("reviewer claim = %+v ok=%t, want %s", reviewerClaim.Job, ok, reviewerJob.ID)
	}
	if _, err := fixture.Queue.MarkJobRunning(ctx, reviewerClaim.Lease.ID); err != nil {
		t.Fatalf("mark reviewer running: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"attach", "--server", httpServer.URL, "--token", "owner-token", "--job", "--print-command", reviewerJob.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("attach job exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tmux attach-session -t flow-"+reviewerJob.ID) {
		t.Fatalf("attach job output = %q", stdout.String())
	}
	if _, err := sessions.RegisterTerminalTarget(ctx, started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"attach", "--server", httpServer.URL, "--token", "owner-token", "--web", "--print-command", started.Session.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("web attach exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	webURL := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(webURL, httpServer.URL+"/v1/sessions/"+started.Session.ID+"/terminal-login?token=") {
		t.Fatalf("web attach output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"status", "Running focused tests"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("status exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Running focused tests") {
		t.Fatalf("status output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"comment", "abc123:internal/app.go:12", "Please handle nil."}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("comment exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	threadID := strings.Fields(stdout.String())[0]
	if !strings.HasPrefix(threadID, "th-") || !strings.Contains(stdout.String(), "open") {
		t.Fatalf("comment output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"thread", "reply", threadID, "I will address it."}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("thread reply exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "comments=2") {
		t.Fatalf("thread reply output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"thread", "claim", "--body", "Intentional behavior.", threadID, "not_warranted"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("thread claim exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claimed") || !strings.Contains(stdout.String(), "claim=not_warranted") {
		t.Fatalf("thread claim output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{
		"handoff",
		"write",
		"--goal", "Ready CLI issue.",
		"--completed", "Implemented work.",
		"--remaining", "Review.",
		"--tests", "go test pending.",
		"--failed-approaches", "None.",
		"--files", "cmd/flow/main.go",
		"--next", "Call flow ready.",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("handoff write exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	handoffBody := stdout.String()

	stdout.Reset()
	stderr.Reset()
	withStdin(t, handoffBody, func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 0 {
		t.Fatalf("ready exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\tfinished\t"+started.Change.ID) {
		t.Fatalf("ready output = %q", stdout.String())
	}
	released, err := fixture.Queue.GetJob(ctx, claimed.Job.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if released.State != flowworker.JobFinished {
		t.Fatalf("released job state = %q, want finished", released.State)
	}
}

// setupReadySession starts a live CLI author session behind an httptest server
// and exports the session environment, so flow ready tests can finalize. The
// coordinator's exchange is cleared; finalize pushes go to the worktree's own
// origin (initReadyWorktree) and the handoff lands in the coordinator DB.
func setupReadySession(t *testing.T, title string) (flowTestFixture, coordinator.StartAuthorSessionResult) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fixture := newFlowTestFixtureWithProtocol(t, "1")
	repointFlowTestFixtureExchange(t, fixture, "")
	started := startCLIAuthorSession(t, fixture, title)

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "1")
	t.Setenv("FLOW_SESSION_ID", started.Session.ID)
	t.Setenv("FLOW_SESSION_TOKEN", started.Token)
	t.Setenv("FLOW_ISSUE_ID", started.Session.IssueID)
	t.Setenv("FLOW_CHANGE_ID", started.Change.ID)
	t.Setenv("FLOW_BRANCH", started.Change.Branch)
	t.Setenv("FLOW_BASE", started.Change.Base)
	return fixture, started
}

func readyTestHandoff(started coordinator.StartAuthorSessionResult, goal string) string {
	return handoff.RenderTemplate(handoff.TemplateInput{
		IssueID:               started.Session.IssueID,
		ChangeID:              started.Change.ID,
		SessionID:             started.Session.ID,
		Branch:                started.Change.Branch,
		Base:                  started.Change.Base,
		CurrentGoal:           goal,
		CompletedWork:         "Implemented the change.",
		RemainingWork:         "Review the change.",
		TestsRun:              "go test ./...",
		FailedApproaches:      "None.",
		ImportantFiles:        "cmd/flow/main.go",
		NextRecommendedAction: "Review.",
	})
}

func TestReadyPublishesHeadAndSubmitsHandoff(t *testing.T) {
	ctx := context.Background()
	fixture, started := setupReadySession(t, "Ready publishes head")

	workDir := t.TempDir()
	t.Chdir(workDir)
	exchangeDir := initReadyWorktree(t, workDir, started.Change.Branch)
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runFlowTestGit(t, workDir, "add", "feature.txt")
	runFlowTestGit(t, workDir, "commit", "-m", "feat: add feature")
	headSHA := flowTestGitOutput(t, workDir, "rev-parse", "HEAD")

	var stdout, stderr bytes.Buffer
	var exitCode int
	withStdin(t, readyTestHandoff(started, "Publish the readied HEAD."), func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 0 {
		t.Fatalf("ready exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\tfinished\t"+started.Change.ID) {
		t.Fatalf("ready output = %q", stdout.String())
	}

	// Regression: a readied HEAD always exists on the exchange remote.
	exchangeHead := flowTestGitOutput(t, exchangeDir, "rev-parse", "refs/heads/"+started.Change.Branch)
	if exchangeHead != headSHA {
		t.Fatalf("exchange branch head = %s, want readied HEAD %s", exchangeHead, headSHA)
	}
	// The handoff reached the coordinator (the sole store).
	snapshot, err := fixture.Reconciler.GetHandoffSnapshot(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get handoff snapshot: %v", err)
	}
	if !snapshot.Present || !snapshot.Valid || snapshot.HeadSHA != headSHA {
		t.Fatalf("handoff snapshot = %+v, want present/valid at %s", snapshot, headSHA)
	}
}

func TestReadyIsIdempotentWhenBranchAlreadyPushed(t *testing.T) {
	_, started := setupReadySession(t, "Ready idempotent push")

	workDir := t.TempDir()
	t.Chdir(workDir)
	exchangeDir := initReadyWorktree(t, workDir, started.Change.Branch)
	runFlowTestGit(t, workDir, "commit", "--allow-empty", "-m", "chore: work")
	headSHA := flowTestGitOutput(t, workDir, "rev-parse", "HEAD")
	// Pre-publish the branch so flow ready's push is an up-to-date no-op.
	runFlowTestGit(t, workDir, "push", "origin", "HEAD:refs/heads/"+started.Change.Branch)

	var stdout, stderr bytes.Buffer
	var exitCode int
	withStdin(t, readyTestHandoff(started, "Re-run is safe."), func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 0 {
		t.Fatalf("ready (already published) exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	exchangeHead := flowTestGitOutput(t, exchangeDir, "rev-parse", "refs/heads/"+started.Change.Branch)
	if exchangeHead != headSHA {
		t.Fatalf("exchange head = %s, want unchanged %s", exchangeHead, headSHA)
	}
}

func TestReadyReadsHandoffFromFile(t *testing.T) {
	_, started := setupReadySession(t, "Ready from file")

	workDir := t.TempDir()
	t.Chdir(workDir)
	initReadyWorktree(t, workDir, started.Change.Branch)
	runFlowTestGit(t, workDir, "commit", "--allow-empty", "-m", "chore: work")

	handoffPath := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(handoffPath, []byte(readyTestHandoff(started, "Read handoff from a file.")), 0o644); err != nil {
		t.Fatalf("write handoff file: %v", err)
	}

	// No stdin pipe: --handoff-file supplies the body for non-interactive callers.
	var stdout, stderr bytes.Buffer
	exitCode := run([]string{"ready", "--handoff-file", handoffPath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("ready --handoff-file exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), started.Session.ID+"\tfinished\t"+started.Change.ID) {
		t.Fatalf("ready output = %q", stdout.String())
	}
}

func TestReadyDerivesBranchFromCheckoutWhenEnvMissing(t *testing.T) {
	_, started := setupReadySession(t, "Ready derives branch")
	// FLOW_BRANCH unset: the push must still target the checked-out branch so
	// the readied HEAD always lands on the exchange.
	t.Setenv("FLOW_BRANCH", "")

	workDir := t.TempDir()
	t.Chdir(workDir)
	exchangeDir := initReadyWorktree(t, workDir, started.Change.Branch)
	runFlowTestGit(t, workDir, "commit", "--allow-empty", "-m", "chore: work")
	headSHA := flowTestGitOutput(t, workDir, "rev-parse", "HEAD")

	var stdout, stderr bytes.Buffer
	var exitCode int
	withStdin(t, readyTestHandoff(started, "Derive the branch."), func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 0 {
		t.Fatalf("ready (branch from checkout) exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	exchangeHead := flowTestGitOutput(t, exchangeDir, "rev-parse", "refs/heads/"+started.Change.Branch)
	if exchangeHead != headSHA {
		t.Fatalf("exchange head = %s, want readied HEAD %s", exchangeHead, headSHA)
	}
}

func TestReadyRejectsInvalidHandoffFromStdin(t *testing.T) {
	_, started := setupReadySession(t, "Ready invalid handoff")

	workDir := t.TempDir()
	t.Chdir(workDir)
	exchangeDir := initReadyWorktree(t, workDir, started.Change.Branch)
	runFlowTestGit(t, workDir, "commit", "--allow-empty", "-m", "chore: work")

	var stdout, stderr bytes.Buffer
	var exitCode int
	withStdin(t, "not a real handoff\n", func() {
		exitCode = run([]string{"ready"}, &stdout, &stderr)
	})
	if exitCode != 1 {
		t.Fatalf("ready invalid handoff exitCode = %d, want 1; stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "handoff validation") {
		t.Fatalf("invalid handoff stderr = %q", stderr.String())
	}
	// Validation fails before any remote mutation: nothing was pushed.
	if err := exec.Command("git", "--git-dir", exchangeDir, "rev-parse", "--verify", "refs/heads/"+started.Change.Branch).Run(); err == nil {
		t.Fatal("invalid handoff still pushed the branch to the exchange")
	}
}

func newFlowAPIServer(t *testing.T) string {
	t.Helper()

	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func requireFlowTestTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func runFlowTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func flowTestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func initFlowTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", dir)
	runFlowTestGit(t, dir, "config", "user.name", "Flow Test")
	runFlowTestGit(t, dir, "config", "user.email", "flow@example.invalid")
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("test repo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runFlowTestGit(t, dir, "add", "README.md")
	runFlowTestGit(t, dir, "commit", "-m", "test: initial")
}

// initReadyWorktree prepares dir as the author's worktree on branch with an
// origin remote pointing at a fresh bare exchange, so `flow ready` can push the
// branch. It returns the exchange path for asserting the readied HEAD landed
// there.
func initReadyWorktree(t *testing.T, dir string, branch string) string {
	t.Helper()
	exchangeDir := filepath.Join(t.TempDir(), "exchange.git")
	runFlowTestGit(t, "", "init", "--bare", exchangeDir)
	initFlowTestGitRepo(t, dir)
	runFlowTestGit(t, dir, "remote", "add", "origin", exchangeDir)
	runFlowTestGit(t, dir, "push", "origin", "main:main")
	runFlowTestGit(t, dir, "checkout", "-b", branch)
	return exchangeDir
}

func assertMigrationsInclude(t *testing.T, got []string, want ...string) {
	t.Helper()

	seen := map[string]bool{}
	for _, migration := range got {
		seen[migration] = true
	}
	for _, migration := range want {
		if !seen[migration] {
			t.Fatalf("migrations = %v, missing %s", got, migration)
		}
	}
}

func TestSplitQualifiedRef(t *testing.T) {
	cases := []struct {
		ref         string
		wantProject string
		wantID      string
	}{
		{"i-0001", "", "i-0001"},
		{"myproj/i-0001", "myproj", "i-0001"},
		{"myproj/ch-abc123", "myproj", "ch-abc123"},
		{"p-1234/i-0042", "p-1234", "i-0042"},
		{"issue/i-0001", "issue", "i-0001"},
		{"refs/heads/main", "", "refs/heads/main"},
		{"ch-abc123", "", "ch-abc123"},
	}
	for _, tc := range cases {
		project, id := splitQualifiedRef(tc.ref)
		if project != tc.wantProject || id != tc.wantID {
			t.Errorf("splitQualifiedRef(%q) = (%q, %q), want (%q, %q)", tc.ref, project, id, tc.wantProject, tc.wantID)
		}
	}
}

func TestInitDoesNotSeedSkillsInRepoWithoutCommits(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init", "--repo", repoPath}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("init exitCode = %d, want 1; stdout = %q stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "connect to flow-server") {
		t.Fatalf("init on a fresh repo should fail before writing skills, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("init wrote repository skills in a fresh repo; stat err = %v", err)
	}
}

func TestInitRegistersProjectWithDiscoveredClientConfig(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	exchangePath := filepath.Join(t.TempDir(), "exchange.git")
	runFlowTestGit(t, "", "init", "--bare", exchangePath)
	exchangeURL := (&url.URL{Scheme: "file", Path: exchangePath}).String()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects" {
			t.Fatalf("request = %s %s, want POST /v1/projects", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":"p-discovered","name":"repo","repo_path":"","base_branch":"main","exchange_name":"flow","exchange_url":` + strconv.Quote(exchangeURL) + `},"created":true}`))
	}))
	t.Cleanup(httpServer.Close)
	dataDir := t.TempDir()
	if err := os.WriteFile(config.OwnerTokenPath(dataDir), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runFlowTestGit(t, repoPath, "config", "user.email", "flow@example.com")
	runFlowTestGit(t, repoPath, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runFlowTestGit(t, repoPath, "add", "README.md")
	runFlowTestGit(t, repoPath, "commit", "-m", "seed")
	t.Chdir(repoPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("register init exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow project created") || !strings.Contains(stdout.String(), "client_config: "+configPath) {
		t.Fatalf("register init output = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("init wrote repository skills; stat err = %v", err)
	}
}

func TestApproveGitCredentialUsesConfiguredHelper(t *testing.T) {
	requireFlowTestTool(t, "git")
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "init", repoPath)

	capturePath := filepath.Join(t.TempDir(), "credential.txt")
	helperPath := filepath.Join(t.TempDir(), "credential-helper.sh")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\ncat > '"+strings.ReplaceAll(capturePath, "'", "'\"'\"'")+"'\n"), 0o755); err != nil {
		t.Fatalf("write credential helper: %v", err)
	}
	runFlowTestGit(t, repoPath, "config", "credential.helper", helperPath)

	stored, command, err := approveGitCredential(repoPath, "http://127.0.0.1:8421/git/projects/p-test/exchange.git", "owner-token")
	if err != nil {
		t.Fatalf("approve credential: %v", err)
	}
	if !stored || command != "" {
		t.Fatalf("stored=%t command=%q, want stored credential", stored, command)
	}
	contents, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured credential: %v", err)
	}
	for _, want := range []string{
		"protocol=http",
		"host=127.0.0.1:8421",
		"path=git/projects/p-test/exchange.git",
		"username=flow",
		"password=owner-token",
	} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("captured credential missing %q:\n%s", want, string(contents))
		}
	}
	usePath := gitHTTPTestConfig(t, repoPath, "credential.useHttpPath")
	if usePath != "true" {
		t.Fatalf("credential.useHttpPath = %q, want true", usePath)
	}
}

func gitHTTPTestConfig(t *testing.T, repoPath string, key string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", repoPath, "config", "--get", key).CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get %s: %s: %v", key, strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output))
}
