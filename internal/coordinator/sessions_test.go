package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestEnsureAuthorJobCreatesChangeAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	store, sessions, issues, workers := fixture.store, fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:    "Author issue",
		Priority: 5,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}

	result, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if result.Existing {
		t.Fatal("first author job was marked existing")
	}
	if result.Change.IssueID != issue.ID || result.Change.Branch != "issue/"+issue.ID || result.Change.Base != defaultAuthorBase {
		t.Fatalf("change = %+v", result.Change)
	}
	if result.Job.Role != flowworker.RoleAuthor || result.Job.CapacityBucket != flowworker.BucketPersistentAgent {
		t.Fatalf("job = %+v", result.Job)
	}
	if result.Job.ChangeID == nil || *result.Job.ChangeID != result.Change.ID {
		t.Fatalf("job ChangeID = %v, want %s", result.Job.ChangeID, result.Change.ID)
	}
	if result.Job.Priority != 5 {
		t.Fatalf("job priority = %d, want 5", result.Job.Priority)
	}
	if payloadString(result.Job.Payload, "branch") != result.Change.Branch || payloadString(result.Job.Payload, "base") != result.Change.Base {
		t.Fatalf("job payload = %+v", result.Job.Payload)
	}
	entrypoint, ok := result.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", result.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("entrypoint argv = %#v", entrypoint["argv"])
	}
	command, ok := argv[0].(string)
	if !ok || !strings.Contains(command, `-c "projects.$PWD.trust_level=trusted"`) {
		t.Fatalf("default command does not trust the job worktree: %#v", entrypoint["argv"])
	}
	if !strings.Contains(command, "--dangerously-bypass-hook-trust") || !strings.Contains(command, "flow hook codex ingest") {
		t.Fatalf("default command does not configure Codex native hooks: %#v", entrypoint["argv"])
	}
	for _, want := range []string{"flow fetch-prompt --harness codex", `"$prompt"`} {
		if !strings.Contains(command, want) {
			t.Fatalf("default codex command missing %q:\n%s", want, command)
		}
	}
	if got := result.Job.Payload["inject_initial_prompt"]; got != false {
		t.Fatalf("inject_initial_prompt = %#v, want false", got)
	}
	if got := payloadString(result.Job.Payload, "prompt_harness"); got != flowharness.Codex {
		t.Fatalf("prompt_harness = %q, want codex", got)
	}
	board, err := issues.Board(ctx)
	if err != nil {
		t.Fatalf("board after queued author job: %v", err)
	}
	assertIssueIDs(t, board.UpNext, []string{issue.ID})
	assertIssueIDs(t, board.InProgress, []string{})

	replayed, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("replay author job: %v", err)
	}
	if !replayed.Existing || replayed.Job.ID != result.Job.ID || replayed.Change.ID != result.Change.ID {
		t.Fatalf("replayed result = %+v, want existing job/change", replayed)
	}
	jobs, err := workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want exactly one", jobs)
	}
	changes, err := countRows(store.DB(), "changes")
	if err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if changes != 1 {
		t.Fatalf("changes = %d, want 1", changes)
	}

	if _, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{
		IssueID: issue.ID,
		Branch:  "issue/" + issue.ID + "-other",
	}); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("incompatible duplicate err = %v, want incompatible error", err)
	}
}

func TestEnsureAuthorJobUsesIssueAgentHarness(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues := fixture.sessions, fixture.issues

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:        "Claude issue",
		AgentHarness: flowharness.Claude,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}

	result, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if got := payloadString(result.Job.Payload, "agent_harness"); got != flowharness.Claude {
		t.Fatalf("job agent_harness = %q, want claude", got)
	}
	if got := result.Job.Selector[flowharness.AgentHarnessLabel(flowharness.Claude)]; got != "true" {
		t.Fatalf("job selector = %#v, want claude harness requirement", result.Job.Selector)
	}
	entrypoint, ok := result.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", result.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("entrypoint argv = %#v", entrypoint["argv"])
	}
	command, ok := argv[0].(string)
	if !ok || !strings.Contains(command, `claude --dangerously-skip-permissions --permission-mode bypassPermissions`) || !strings.Contains(command, "flow hook claude start") {
		t.Fatalf("claude command = %#v", entrypoint["argv"])
	}
	for _, want := range []string{"flow fetch-prompt --harness claude", `"$prompt"`} {
		if !strings.Contains(command, want) {
			t.Fatalf("claude author command missing %q:\n%s", want, command)
		}
	}
	if got := result.Job.Payload["inject_initial_prompt"]; got != false {
		t.Fatalf("inject_initial_prompt = %#v, want false", got)
	}
	if got := payloadString(result.Job.Payload, "prompt_harness"); got != flowharness.Claude {
		t.Fatalf("prompt_harness = %q, want claude", got)
	}
}

func TestEnsureAuthorJobUsesHarnessInitialPromptFlag(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues := fixture.sessions, fixture.issues

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:        "Harness issue",
		AgentHarness: flowharness.Harness,
		HarnessArgs:  flowharness.Args{Harness: []string{"--model", "fast"}},
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}

	result, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	command := entrypointCommandForTest(t, result.Job.Payload)
	for _, want := range []string{
		"flow fetch-prompt --harness harness",
		`harness --hooks "$FLOW_HARNESS_HOOKS" '--model' 'fast' -i "$prompt"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("harness author command missing %q:\n%s", want, command)
		}
	}
	if got := result.Job.Payload["inject_initial_prompt"]; got != false {
		t.Fatalf("inject_initial_prompt = %#v, want false for harness -i", got)
	}
	if got := payloadString(result.Job.Payload, "prompt_harness"); got != flowharness.Harness {
		t.Fatalf("prompt_harness = %q, want harness", got)
	}
}

func TestEnsureAuthorAndConsoleJobsAppendHarnessArgs(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := NewSessionServiceWithOptions(fixture.store.DB(), fixture.issues, fixture.workers, SessionServiceOptions{
		HarnessArgs: flowharness.Args{
			Codex:  []string{"--model", "gpt-5"},
			Claude: []string{"--model", "sonnet"},
		},
		Credentials: fixture.credentials,
		Project:     fixture.project,
	})

	issue, err := fixture.issues.CreateIssue(ctx, CreateIssueInput{
		Title:       "Args issue",
		HarnessArgs: flowharness.Args{Codex: []string{"--sandbox", "workspace-write"}},
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	author, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	authorCommand := entrypointCommandForTest(t, author.Job.Payload)
	for _, want := range []string{"'--model' 'gpt-5'", "'--sandbox' 'workspace-write'"} {
		if !strings.Contains(authorCommand, want) {
			t.Fatalf("author command missing %q:\n%s", want, authorCommand)
		}
	}
	for _, want := range []string{"flow fetch-prompt --harness codex", `"$prompt"`} {
		if !strings.Contains(authorCommand, want) {
			t.Fatalf("author command missing %q:\n%s", want, authorCommand)
		}
	}
	if got := author.Job.Payload["inject_initial_prompt"]; got != false {
		t.Fatalf("author inject_initial_prompt = %#v, want false", got)
	}

	console, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Claude})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	consoleCommand := entrypointCommandForTest(t, console.Job.Payload)
	if !strings.Contains(consoleCommand, "'--model' 'sonnet'") {
		t.Fatalf("console command did not include claude defaults:\n%s", consoleCommand)
	}
	if strings.Contains(consoleCommand, "fetch-prompt") || strings.Contains(consoleCommand, `"$prompt"`) {
		t.Fatalf("console command includes prompt handling:\n%s", consoleCommand)
	}
}

func TestListSessionsForIssueOrdersByRecentActivity(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	store, sessions, issues, workers := fixture.store, fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Session ordering"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	first, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure first author job: %v", err)
	}
	firstClaim, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{WorkerID: "w-local", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("claim first job: %v", err)
	}
	if !ok || firstClaim.Job.ID != first.Job.ID {
		t.Fatalf("first claim = %+v ok=%t", firstClaim.Job, ok)
	}
	if _, err := workers.MarkJobRunning(ctx, firstClaim.Lease.ID); err != nil {
		t.Fatalf("mark first running: %v", err)
	}
	firstSession, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    firstClaim.Job.ID,
		LeaseID:  firstClaim.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start first session: %v", err)
	}
	if _, err := sessions.ReadyAuthorSession(ctx, firstSession.Session.ID); err != nil {
		t.Fatalf("ready first session: %v", err)
	}

	second, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure second author job: %v", err)
	}
	secondClaim, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{WorkerID: "w-local", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("claim second job: %v", err)
	}
	if !ok || secondClaim.Job.ID != second.Job.ID {
		t.Fatalf("second claim = %+v ok=%t", secondClaim.Job, ok)
	}
	if _, err := workers.MarkJobRunning(ctx, secondClaim.Lease.ID); err != nil {
		t.Fatalf("mark second running: %v", err)
	}
	secondSession, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    secondClaim.Job.ID,
		LeaseID:  secondClaim.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start second session: %v", err)
	}

	oldTime := formatTime(time.Now().UTC().Add(-2 * time.Hour))
	newTime := formatTime(time.Now().UTC())
	if _, err := store.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, newTime, firstSession.Session.ID); err != nil {
		t.Fatalf("touch first session: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, oldTime, secondSession.Session.ID); err != nil {
		t.Fatalf("age second session: %v", err)
	}

	listed, err := sessions.ListSessionsForIssue(ctx, issue.ID, 2)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != firstSession.Session.ID || listed[1].ID != secondSession.Session.ID {
		t.Fatalf("listed sessions = %+v", listed)
	}
}

func TestEnsureAuthorJobUsesConfiguredDefaultEntrypoint(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, t.TempDir()+"/flow.db")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionServiceWithOptions(store.DB(), issues, workers, SessionServiceOptions{
		DefaultAuthorEntrypoint: map[string]any{
			"argv":  []string{"custom-author", "--resume"},
			"cwd":   "agents",
			"env":   map[string]string{"CUSTOM": "true"},
			"shell": false,
		},
		DefaultAuthorEntrypointOverride: true,
	})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Configured author issue"})
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
	entrypoint, ok := ensured.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", ensured.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 2 || argv[0] != "custom-author" || argv[1] != "--resume" {
		t.Fatalf("entrypoint argv = %#v", entrypoint["argv"])
	}
	if entrypoint["cwd"] != "agents" {
		t.Fatalf("entrypoint cwd = %#v", entrypoint["cwd"])
	}
}

func TestEnsureAuthorJobRejectsBlockedOrUnacceptedIssues(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	backlog, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Backlog"})
	if err != nil {
		t.Fatalf("create backlog: %v", err)
	}
	if _, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: backlog.ID}); err == nil {
		t.Fatal("author job was enqueued for backlog issue")
	}

	sessionID := "s-agent"
	triage, err := issues.CreateIssue(ctx, CreateIssueInput{
		Title:              "Triage",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &sessionID,
	})
	if err != nil {
		t.Fatalf("create triage: %v", err)
	}
	if _, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: triage.ID}); err == nil {
		t.Fatal("author job was enqueued for triage issue")
	}

	blocker, blocked := createTwoIssues(t, issues, "Blocker", "Blocked")
	if _, err := issues.ScheduleIssue(ctx, blocked.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule blocked issue: %v", err)
	}
	if err := issues.LinkIssues(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: blocked.ID}); err == nil || !strings.Contains(err.Error(), "unresolved blockers") {
		t.Fatalf("blocked issue err = %v, want unresolved blockers error", err)
	}

	jobs, err := workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none", jobs)
	}
}

func TestStartAuthorSessionCreatesTokenAndDrivesBoardLanes(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers
	credentials := fixture.credentials

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Session issue"})
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
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	started, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
		Harness:  "codex",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	if started.Token == "" || started.Session.ID == "" || started.Session.RuntimeState != SessionStarting {
		t.Fatalf("started session = %+v token=%q", started.Session, started.Token)
	}
	principal, err := credentials.Authenticate(ctx, started.Token)
	if err != nil {
		t.Fatalf("authenticate session token: %v", err)
	}
	if principal.Scope != TokenScopeSession || principal.Subject != started.Session.ID || principal.SourceIssueID == nil || *principal.SourceIssueID != issue.ID {
		t.Fatalf("principal = %+v", principal)
	}

	board, err := issues.Board(ctx)
	if err != nil {
		t.Fatalf("board with starting session: %v", err)
	}
	assertIssueIDs(t, board.InProgress, []string{issue.ID})

	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}
	waiting, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board with waiting session: %v", err)
	}
	assertIssueIDs(t, waiting.Board.NeedsAttention, []string{issue.ID})
	assertIssueIDs(t, waiting.Board.InProgress, []string{})
	if got := waiting.LaneStates[issue.ID]; got != LaneStateInProgress {
		t.Fatalf("waiting lane state = %q, want in_progress", got)
	}
	if got := waiting.WaitReasons[issue.ID]; got != WaitReasonQuestion {
		t.Fatalf("waiting wait reason = %q, want question", got)
	}
	active, ok, err := sessions.ActiveAuthorSessionForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("active author session: %v", err)
	}
	if !ok || active.ID != started.Session.ID || active.RuntimeState != SessionWaiting {
		t.Fatalf("active author session = %+v ok=%t", active, ok)
	}

	readySession, err := sessions.ReadyAuthorSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("ready session: %v", err)
	}
	if readySession.RuntimeState != SessionFinished || readySession.FinishedAt == nil {
		t.Fatalf("ready session = %+v", readySession)
	}
	readyChange, err := sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	if readyChange.ReadyAt == nil {
		t.Fatal("ready change ReadyAt is nil")
	}
	releasedJob, err := workers.GetJob(ctx, claimed.Job.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if releasedJob.State != flowworker.JobFinished {
		t.Fatalf("released job state = %q, want finished", releasedJob.State)
	}
	releasedLease, err := workers.GetLease(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("get released lease: %v", err)
	}
	if releasedLease.ReleasedAt == nil {
		t.Fatal("released lease ReleasedAt is nil")
	}
	if _, err := credentials.Authenticate(ctx, started.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate revoked token err = %v, want ErrInvalidCredential", err)
	}
	finished, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after finished session: %v", err)
	}
	assertIssueIDs(t, finished.Board.InProgress, []string{issue.ID})
	assertIssueIDs(t, finished.Board.NeedsAttention, []string{})
	if got := finished.LaneStates[issue.ID]; got != LaneStateInReview {
		t.Fatalf("finished lane state = %q, want in_review", got)
	}
	active, ok, err = sessions.ActiveAuthorSessionForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("active author session after ready: %v", err)
	}
	if ok {
		t.Fatalf("active author session after ready = %+v, want none", active)
	}
	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revive finished session err = %v, want sql.ErrNoRows", err)
	}
	stillFinished, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get still-finished session: %v", err)
	}
	if stillFinished.RuntimeState != SessionFinished {
		t.Fatalf("session state after revive attempt = %q, want finished", stillFinished.RuntimeState)
	}
}

func TestPauseAuthorSessionAbandonsSessionCancelsJobAndAllowsResume(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	started := startAuthorSessionForFixture(t, fixture, "Paused session issue")

	paused, err := fixture.sessions.PauseAuthorSession(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("pause author session: %v", err)
	}
	if paused.RuntimeState != SessionAbandoned || paused.FinishedAt == nil {
		t.Fatalf("paused session = %+v, want abandoned with finished_at", paused)
	}
	active, ok, err := fixture.sessions.ActiveAuthorSessionForIssue(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("active author session after pause: %v", err)
	}
	if ok {
		t.Fatalf("active author session after pause = %+v, want none", active)
	}
	job, err := fixture.workers.GetJob(ctx, started.Session.JobID)
	if err != nil {
		t.Fatalf("get paused job: %v", err)
	}
	if job.State != flowworker.JobCanceled {
		t.Fatalf("paused job state = %q, want canceled", job.State)
	}
	lease, err := fixture.workers.GetLease(ctx, started.Session.LeaseID)
	if err != nil {
		t.Fatalf("get paused lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("paused lease ReleasedAt is nil")
	}
	if _, err := fixture.credentials.Authenticate(ctx, started.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate paused token err = %v, want ErrInvalidCredential", err)
	}
	board, err := fixture.issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after pause: %v", err)
	}
	assertIssueIDs(t, board.Board.UpNext, []string{started.Session.IssueID})
	if got := board.LaneStates[started.Session.IssueID]; got != LaneStateUpNext {
		t.Fatalf("paused lane state = %q, want up_next", got)
	}

	resumed, err := fixture.sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: started.Session.IssueID})
	if err != nil {
		t.Fatalf("resume author job: %v", err)
	}
	if resumed.Job.ID == started.Session.JobID {
		t.Fatalf("resume job reused paused job id %s", resumed.Job.ID)
	}
	if resumed.Change.ID != started.Change.ID {
		t.Fatalf("resume change = %s, want %s", resumed.Change.ID, started.Change.ID)
	}
	if payloadString(resumed.Job.Payload, "branch") != started.Change.Branch || payloadString(resumed.Job.Payload, "base") != started.Change.Base {
		t.Fatalf("resume payload = %+v, want branch=%s base=%s", resumed.Job.Payload, started.Change.Branch, started.Change.Base)
	}
	if _, err := fixture.sessions.PauseAuthorSession(ctx, started.Session.IssueID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second pause err = %v, want sql.ErrNoRows", err)
	}
}

func TestReconcileCrashedAuthorSessionReenqueuesSameChangeBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers
	credentials := fixture.credentials

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Crash resume issue"})
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
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), claimed.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	reconciled, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("reconcile crashed author sessions: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", reconciled)
	}
	crashed, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed session: %v", err)
	}
	if crashed.RuntimeState != SessionCrashed || crashed.FinishedAt == nil {
		t.Fatalf("crashed session = %+v", crashed)
	}
	if _, err := credentials.Authenticate(ctx, started.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate crashed token err = %v, want ErrInvalidCredential", err)
	}
	oldJob, err := workers.GetJob(ctx, claimed.Job.ID)
	if err != nil {
		t.Fatalf("get old job: %v", err)
	}
	if oldJob.State != flowworker.JobCrashed {
		t.Fatalf("old job state = %q, want crashed", oldJob.State)
	}
	resumeJob, ok, err := workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job: %v", err)
	}
	if !ok {
		t.Fatal("crashed author session was not re-enqueued")
	}
	if resumeJob.ID == claimed.Job.ID {
		t.Fatal("resume job reused crashed job id")
	}
	if resumeJob.ChangeID == nil || *resumeJob.ChangeID != started.Change.ID {
		t.Fatalf("resume job ChangeID = %v, want %s", resumeJob.ChangeID, started.Change.ID)
	}
	if payloadString(resumeJob.Payload, "branch") != started.Change.Branch || payloadString(resumeJob.Payload, "base") != started.Change.Base {
		t.Fatalf("resume job payload = %+v, want branch=%s base=%s", resumeJob.Payload, started.Change.Branch, started.Change.Base)
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobCanceled), resumeJob.ID); err != nil {
		t.Fatalf("cancel first resume job: %v", err)
	}
	retried, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("retry crashed author session reconcile: %v", err)
	}
	if retried != 1 {
		t.Fatalf("retried = %d, want 1", retried)
	}
	retryJob, ok, err := workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job after retry: %v", err)
	}
	if !ok {
		t.Fatal("crashed author session retry did not enqueue a live job")
	}
	if retryJob.ID == resumeJob.ID || retryJob.ID == claimed.Job.ID {
		t.Fatalf("retry job id = %s, want a new resume job", retryJob.ID)
	}
	if retryJob.ChangeID == nil || *retryJob.ChangeID != started.Change.ID {
		t.Fatalf("retry job ChangeID = %v, want %s", retryJob.ChangeID, started.Change.ID)
	}
	resumeJob = retryJob
	if replayed, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID}); err != nil {
		t.Fatalf("ensure after crash: %v", err)
	} else if !replayed.Existing || replayed.Job.ID != resumeJob.ID {
		t.Fatalf("ensure after crash = %+v, want existing resume job %s", replayed, resumeJob.ID)
	}
	board, err := issues.Board(ctx)
	if err != nil {
		t.Fatalf("board after crash reconcile: %v", err)
	}
	assertIssueIDs(t, board.UpNext, []string{issue.ID})
	assertIssueIDs(t, board.InProgress, []string{})
	assertIssueIDs(t, board.NeedsAttention, []string{})
}

func TestRetryCrashedAuthorJobClearsCrashHoldAndPreservesChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Retry crash hold"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	first, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobCanceled), first.Job.ID); err != nil {
		t.Fatalf("cancel first job: %v", err)
	}
	if _, err := sessions.db.ExecContext(ctx, `
INSERT INTO status_log (issue_id, actor, message, kind, created_at)
VALUES (?, ?, ?, ?, ?)`,
		issue.ID,
		"system",
		fmt.Sprintf(crashRestartLimitMessageFormat, maxAutomaticCrashAttempts),
		StatusKindBlocker,
		formatTime(time.Now().UTC()),
	); err != nil {
		t.Fatalf("insert crash blocker: %v", err)
	}
	held, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board before retry: %v", err)
	}
	if got := held.WaitReasons[issue.ID]; got != WaitReasonCrashLoop {
		t.Fatalf("wait reason before retry = %q, want %q", got, WaitReasonCrashLoop)
	}

	retried, err := sessions.RetryCrashedAuthorJob(ctx, issue.ID, "owner:test")
	if err != nil {
		t.Fatalf("retry crashed author job: %v", err)
	}
	if retried.ResolvedRows != 1 {
		t.Fatalf("resolved rows = %d, want 1", retried.ResolvedRows)
	}
	if retried.Job == nil || retried.Job.ID == first.Job.ID {
		t.Fatalf("retry job = %+v, want new job", retried.Job)
	}
	if retried.Change == nil || retried.Change.ID != first.Change.ID {
		t.Fatalf("retry change = %+v, want %s", retried.Change, first.Change.ID)
	}
	live, ok, err := workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job after retry: %v", err)
	}
	if !ok || live.ID != retried.Job.ID {
		t.Fatalf("live job = %+v ok=%t, want retry job %s", live, ok, retried.Job.ID)
	}
	board, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after retry: %v", err)
	}
	if got := board.WaitReasons[issue.ID]; got != "" {
		t.Fatalf("wait reason after retry = %q, want empty", got)
	}
	assertIssueIDs(t, board.Board.UpNext, []string{issue.ID})
	entries, err := NewStatusService(sessions.db).ListForIssue(ctx, issue.ID, 2)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	if len(entries) == 0 || entries[0].Kind != StatusKindProgress || entries[0].Message != "Crash hold cleared; retrying author job." {
		t.Fatalf("latest status = %+v, want crash retry progress note", entries)
	}
}

func TestReconcileCrashedAuthorSessionStopsAfterTwoCrashes(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	crashed := startWaitingAuthorSession(t, ctx, fixture, "Repeated crash issue")
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), crashed.leaseID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	recovered, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("first crash reconcile: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("first recovered = %d, want 1", recovered)
	}
	resumeJob, ok, err := workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID)
	if err != nil {
		t.Fatalf("live author job after first crash: %v", err)
	}
	if !ok {
		t.Fatal("first crash did not enqueue a replacement")
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobCrashed), resumeJob.ID); err != nil {
		t.Fatalf("mark replacement crashed: %v", err)
	}

	recovered, err = sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("second crash reconcile: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("second recovered = %d, want 0", recovered)
	}
	if live, ok, err := workers.LiveAuthorJobForIssue(ctx, crashed.session.IssueID); err != nil {
		t.Fatalf("live author job after crash limit: %v", err)
	} else if ok {
		t.Fatalf("crash-limited issue still has live author job: %+v", live)
	}
	board, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after crash limit: %v", err)
	}
	if got := board.WaitReasons[crashed.session.IssueID]; got != WaitReasonCrashLoop {
		t.Fatalf("wait reason = %q, want %q", got, WaitReasonCrashLoop)
	}
	assertIssueIDs(t, board.Board.NeedsAttention, []string{crashed.session.IssueID})
	entries, err := NewStatusService(sessions.db).ListForIssue(ctx, crashed.session.IssueID, 5)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	if len(entries) == 0 || entries[0].Kind != StatusKindBlocker || !strings.Contains(entries[0].Message, "human intervention required") {
		t.Fatalf("status entries = %+v, want crash-loop blocker", entries)
	}
}

func TestReconcileCrashedAuthorSessionsIsolatesPoisonedSession(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers := fixture.sessions, fixture.workers

	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 2,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	poisoned := startWaitingAuthorSession(t, ctx, fixture, "Poisoned crash issue")
	healthy := startWaitingAuthorSession(t, ctx, fixture, "Healthy crash issue")

	// Expire both leases only after both sessions are waiting, so the reconcile
	// sweep inside the second EnsureAuthorJob does not crash the first session
	// early. Both now look crashed to ReconcileCrashedAuthorSessions.
	for _, leaseID := range []string{poisoned.leaseID, healthy.leaseID} {
		if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), leaseID); err != nil {
			t.Fatalf("expire lease %s: %v", leaseID, err)
		}
	}

	// Corrupt the poisoned session's updated_at so scanSession fails when the
	// reconcile tx reloads it, while the pre-tx id scan still returns it.
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE sessions
SET updated_at = ?
WHERE id = ?`, "not-a-timestamp", poisoned.session.ID); err != nil {
		t.Fatalf("corrupt poisoned session updated_at: %v", err)
	}

	recovered, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err == nil {
		t.Fatal("reconcile returned nil error, want joined poisoned-session error")
	}
	if !strings.Contains(err.Error(), poisoned.session.ID) {
		t.Fatalf("error = %v, want it to reference poisoned session %s", err, poisoned.session.ID)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1 healthy session despite poisoned session", recovered)
	}

	// The healthy session is still marked crashed and re-enqueued.
	healthyCrashed, err := sessions.GetSession(ctx, healthy.session.ID)
	if err != nil {
		t.Fatalf("get healthy session: %v", err)
	}
	if healthyCrashed.RuntimeState != SessionCrashed || healthyCrashed.FinishedAt == nil {
		t.Fatalf("healthy session = %+v, want crashed with finished_at", healthyCrashed)
	}
	resumeJob, ok, err := workers.LiveAuthorJobForIssue(ctx, healthy.session.IssueID)
	if err != nil {
		t.Fatalf("live author job for healthy issue: %v", err)
	}
	if !ok {
		t.Fatal("healthy crashed author session was not re-enqueued")
	}
	if resumeJob.ID == healthy.jobID {
		t.Fatal("healthy resume job reused crashed job id")
	}
	if resumeJob.ChangeID == nil || *resumeJob.ChangeID != healthy.session.ChangeID {
		t.Fatalf("resume job ChangeID = %v, want %s", resumeJob.ChangeID, healthy.session.ChangeID)
	}
}

type crashableSession struct {
	session Session
	jobID   string
	leaseID string
}

// startWaitingAuthorSession creates an issue and drives it through to a waiting
// author session, returning the session plus its original job and lease ids. The
// caller expires the lease afterwards to make ReconcileCrashedAuthorSessions
// treat it as crashed.
func startWaitingAuthorSession(t *testing.T, ctx context.Context, fixture sessionFixture, title string) crashableSession {
	t.Helper()
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: title})
	if err != nil {
		t.Fatalf("create issue %q: %v", title, err)
	}
	if _, err := issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue %q: %v", title, err)
	}
	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job %q: %v", title, err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job %q: %v", title, err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim %q = %+v ok=%t, want %s", title, claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running %q: %v", title, err)
	}
	started, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session %q: %v", title, err)
	}
	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); err != nil {
		t.Fatalf("mark waiting %q: %v", title, err)
	}

	return crashableSession{session: started.Session, jobID: claimed.Job.ID, leaseID: claimed.Lease.ID}
}

func TestStartAuthorSessionRejectsExpiredLease(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Expired session issue"})
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
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), claimed.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	if _, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	}); err == nil {
		t.Fatal("expired lease started an author session")
	}
	sweptJob, err := workers.GetJob(ctx, claimed.Job.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if sweptJob.State != flowworker.JobCrashed {
		t.Fatalf("swept job state = %q, want crashed", sweptJob.State)
	}
	sessionsCount, err := countRows(sessions.db, "sessions")
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionsCount != 0 {
		t.Fatalf("sessions count = %d, want 0", sessionsCount)
	}
}

func TestReadyIssueWithUnresolvedBlockerMovesToNeedsAttentionWithBlockedOverlay(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, issues, workers := fixture.sessions, fixture.issues, fixture.workers

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Ready blocked issue"})
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
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	blocker, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Discovered blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := issues.LinkIssues(ctx, blocker.ID, issue.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}

	working, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board with working session: %v", err)
	}
	assertIssueIDs(t, working.Board.InProgress, []string{})
	assertIssueIDs(t, working.Board.NeedsAttention, []string{issue.ID})
	if got := working.LaneStates[issue.ID]; got != LaneStateInProgress {
		t.Fatalf("working lane state = %q, want in_progress", got)
	}
	assertBlockedIDs(t, working.BlockedIDs, []string{issue.ID})

	if _, err := sessions.ReadyAuthorSession(ctx, started.Session.ID); err != nil {
		t.Fatalf("ready session: %v", err)
	}

	ready, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after ready: %v", err)
	}
	assertIssueIDs(t, ready.Board.InProgress, []string{})
	assertIssueIDs(t, ready.Board.NeedsAttention, []string{issue.ID})
	if got := ready.LaneStates[issue.ID]; got != LaneStateInReview {
		t.Fatalf("ready lane state = %q, want in_review", got)
	}
	assertBlockedIDs(t, ready.BlockedIDs, []string{issue.ID})
}

func TestReviewAuthorCycleLimitRequiresApprovalBeforeMoreFixJobs(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := NewSessionServiceWithOptions(fixture.store.DB(), fixture.issues, fixture.workers, SessionServiceOptions{
		Credentials:            fixture.credentials,
		Project:                fixture.project,
		ReviewAuthorCycleLimit: 1,
	})

	issue, err := fixture.issues.CreateIssue(ctx, CreateIssueInput{Title: "Looping review issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	initial, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure initial author job: %v", err)
	}
	nowText := formatTime(time.Now().UTC())
	if _, err := fixture.store.DB().ExecContext(ctx, `
UPDATE changes
SET ready_at = COALESCE(ready_at, ?),
	head_sha = ?,
	updated_at = ?
WHERE id = ?`,
		nowText,
		"head-1",
		nowText,
		initial.Change.ID,
	); err != nil {
		t.Fatalf("mark change ready: %v", err)
	}
	if canceled, err := fixture.workers.CancelLiveJobsForIssue(ctx, issue.ID, flowworker.RoleAuthor); err != nil {
		t.Fatalf("cancel initial author job: %v", err)
	} else if len(canceled) != 1 {
		t.Fatalf("canceled initial author jobs = %v, want one", canceled)
	}

	required := true
	checks := NewCheckService(fixture.store.DB())
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "reviewer",
		Kind:     CheckKindReviewer,
		Required: &required,
		Verdict:  CheckBlocked,
		Details:  "Needs another pass.",
		Reporter: "reviewer:r-local",
	}); err != nil {
		t.Fatalf("report blocked reviewer check: %v", err)
	}

	firstFix, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure first fix job: %v", err)
	}
	if got := payloadString(firstFix.Job.Payload, "review_cycle_number"); got != "1" {
		t.Fatalf("first fix review_cycle_number = %q, want 1", got)
	}
	if got := payloadString(firstFix.Job.Payload, "review_cycle_limit"); got != "1" {
		t.Fatalf("first fix review_cycle_limit = %q, want 1", got)
	}
	if canceled, err := fixture.workers.CancelLiveJobsForIssue(ctx, issue.ID, flowworker.RoleAuthor); err != nil {
		t.Fatalf("cancel first fix job: %v", err)
	} else if len(canceled) != 1 || canceled[0] != firstFix.Job.ID {
		t.Fatalf("canceled first fix jobs = %v, want %s", canceled, firstFix.Job.ID)
	}

	_, err = sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err == nil || !errors.Is(err, ErrAuthorJobSuppressed) || !errors.Is(err, ErrReviewCycleLimitReached) {
		t.Fatalf("second fix err = %v, want author suppression wrapping review cycle limit", err)
	}
	budget, err := sessions.ReviewCycleBudget(ctx, issue.ID)
	if err != nil {
		t.Fatalf("load exhausted budget: %v", err)
	}
	if !budget.Exhausted || budget.UsedCycles != 1 || budget.GrantedCycles != 1 || budget.RemainingCycles != 0 {
		t.Fatalf("exhausted budget = %+v, want exhausted 1/1", budget)
	}
	board, err := fixture.issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after exhausted budget: %v", err)
	}
	if got := board.WaitReasons[issue.ID]; got != WaitReasonReviewCycles {
		t.Fatalf("wait reason = %q, want %q", got, WaitReasonReviewCycles)
	}
	assertIssueIDs(t, board.Board.NeedsAttention, []string{issue.ID})

	instructions := "Inspect the previous reviewer notes and summarize why the fix loop repeated before changing code."
	approved, err := sessions.ApproveReviewCycles(ctx, ApproveReviewCyclesInput{
		IssueID:      issue.ID,
		Instructions: instructions,
		Actor:        "owner",
	})
	if err != nil {
		t.Fatalf("approve more review cycles: %v", err)
	}
	if approved.Exhausted || approved.GrantedCycles != 2 || approved.UsedCycles != 1 || approved.RemainingCycles != 1 || approved.LastInstructions != instructions {
		t.Fatalf("approved budget = %+v, want one more cycle with instructions", approved)
	}

	approvedFix, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure approved fix job: %v", err)
	}
	if got := payloadString(approvedFix.Job.Payload, "review_cycle_number"); got != "2" {
		t.Fatalf("approved fix review_cycle_number = %q, want 2", got)
	}
	if got := payloadString(approvedFix.Job.Payload, "review_cycle_limit"); got != "2" {
		t.Fatalf("approved fix review_cycle_limit = %q, want 2", got)
	}
	if got := payloadString(approvedFix.Job.Payload, "review_cycle_instructions"); got != instructions {
		t.Fatalf("approved fix instructions = %q, want %q", got, instructions)
	}
}

func TestConsoleSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers, credentials := fixture.sessions, fixture.workers, fixture.credentials

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Claude})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	if ensured.Existing {
		t.Fatal("first console job was marked existing")
	}
	if ensured.Job.Role != flowworker.RoleConsole || ensured.Job.IssueID != nil || ensured.Job.ChangeID != nil {
		t.Fatalf("console job = %+v", ensured.Job)
	}
	if payloadString(ensured.Job.Payload, "branch") != fixture.project.BaseBranch || payloadString(ensured.Job.Payload, "base") != fixture.project.BaseBranch {
		t.Fatalf("console payload branch/base = %+v", ensured.Job.Payload)
	}
	if payloadString(ensured.Job.Payload, "console_harness") != flowharness.Claude || payloadString(ensured.Job.Payload, "session_purpose") != "console" {
		t.Fatalf("console payload = %+v", ensured.Job.Payload)
	}
	if got := ensured.Job.Selector[flowharness.AgentHarnessLabel(flowharness.Claude)]; got != "true" {
		t.Fatalf("console selector = %#v, want claude harness requirement", ensured.Job.Selector)
	}
	entrypoint, ok := ensured.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("console entrypoint payload = %#v", ensured.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("console entrypoint argv = %#v", entrypoint["argv"])
	}
	command, ok := argv[0].(string)
	if !ok || !strings.Contains(command, `claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions`) {
		t.Fatalf("console command = %#v", entrypoint["argv"])
	}
	for _, unexpected := range []string{"flow fetch-prompt", `"$prompt"`, "flow-console"} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("console command includes prompt setup %q:\n%s", unexpected, command)
		}
	}
	replayed, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Claude})
	if err != nil {
		t.Fatalf("replay console job: %v", err)
	}
	if !replayed.Existing || replayed.Job.ID != ensured.Job.ID {
		t.Fatalf("replayed console = %+v, want existing %s", replayed, ensured.Job.ID)
	}

	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Claude): "true"},
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
		t.Fatalf("claim console job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
		Harness:  flowharness.Claude,
	})
	if err != nil {
		t.Fatalf("start console session: %v", err)
	}
	if started.Session.Role != flowworker.RoleConsole || started.Session.IssueID != "" || started.Session.ChangeID != "" {
		t.Fatalf("started console session = %+v", started.Session)
	}
	principal, err := credentials.Authenticate(ctx, started.Token)
	if err != nil {
		t.Fatalf("authenticate console token: %v", err)
	}
	if principal.Scope != TokenScopeConsole || principal.Subject != started.Session.ID || principal.ProjectID == nil || *principal.ProjectID != fixture.project.ID || principal.SourceIssueID != nil {
		t.Fatalf("console principal = %+v", principal)
	}
	waiting, err := sessions.UpdateConsoleSessionState(ctx, started.Session.ID, SessionWaiting)
	if err != nil {
		t.Fatalf("mark console waiting: %v", err)
	}
	if waiting.RuntimeState != SessionWaiting {
		t.Fatalf("waiting console state = %q", waiting.RuntimeState)
	}
	current, err := sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if !current.Active || current.Job == nil || current.Job.ID != ensured.Job.ID || current.Session == nil || current.Session.ID != started.Session.ID {
		t.Fatalf("current console = %+v", current)
	}

	released, err := sessions.ReleaseConsole(ctx)
	if err != nil {
		t.Fatalf("release console: %v", err)
	}
	if released.Active {
		t.Fatalf("released console state = %+v, want inactive", released)
	}
	finished, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get finished console: %v", err)
	}
	if finished.RuntimeState != SessionFinished || finished.FinishedAt == nil {
		t.Fatalf("finished console = %+v", finished)
	}
	releasedJob, err := workers.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get released console job: %v", err)
	}
	if releasedJob.State != flowworker.JobFinished {
		t.Fatalf("released console job state = %q, want finished", releasedJob.State)
	}
	releasedLease, err := workers.GetLease(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("get released console lease: %v", err)
	}
	if releasedLease.ReleasedAt == nil {
		t.Fatal("released console lease ReleasedAt is nil")
	}
	if _, err := credentials.Authenticate(ctx, started.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate revoked console token err = %v, want ErrInvalidCredential", err)
	}
	again, err := sessions.ReleaseConsole(ctx)
	if err != nil {
		t.Fatalf("idempotent release console: %v", err)
	}
	if again.Active {
		t.Fatalf("idempotent release state = %+v, want inactive", again)
	}
}

func TestEnsureConsoleJobSupportsShellHarness(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)

	ensured, err := fixture.sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Shell})
	if err != nil {
		t.Fatalf("ensure shell console job: %v", err)
	}
	if payloadString(ensured.Job.Payload, "console_harness") != flowharness.Shell {
		t.Fatalf("console_harness = %q, want %q", payloadString(ensured.Job.Payload, "console_harness"), flowharness.Shell)
	}
	if payloadString(ensured.Job.Payload, "agent_harness") != "" {
		t.Fatalf("agent_harness = %q, want empty", payloadString(ensured.Job.Payload, "agent_harness"))
	}
	if len(ensured.Job.Selector) != 0 {
		t.Fatalf("shell console selector = %#v, want no harness requirement", ensured.Job.Selector)
	}
	entrypoint, ok := ensured.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("console entrypoint payload = %#v", ensured.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("console entrypoint argv = %#v", entrypoint["argv"])
	}
	command, ok := argv[0].(string)
	if !ok || command != `exec "${SHELL:-/bin/sh}"` {
		t.Fatalf("console command = %#v, want shell", entrypoint["argv"])
	}
}

func TestReleaseConsoleCancelsQueuedJob(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers := fixture.sessions, fixture.workers

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Codex})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	released, err := sessions.ReleaseConsole(ctx)
	if err != nil {
		t.Fatalf("release queued console: %v", err)
	}
	if released.Active {
		t.Fatalf("release queued console state = %+v, want inactive", released)
	}
	job, err := workers.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get canceled console job: %v", err)
	}
	if job.State != flowworker.JobCanceled {
		t.Fatalf("queued console job state = %q, want canceled", job.State)
	}
}

func TestReconcileCrashedConsoleSessionDoesNotReenqueue(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers, credentials := fixture.sessions, fixture.workers, fixture.credentials

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Codex})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim console job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start console session: %v", err)
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), claimed.Lease.ID); err != nil {
		t.Fatalf("expire console lease: %v", err)
	}

	reconciled, err := sessions.ReconcileCrashedConsoleSessions(ctx)
	if err != nil {
		t.Fatalf("reconcile crashed console: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", reconciled)
	}
	crashed, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed console session: %v", err)
	}
	if crashed.RuntimeState != SessionCrashed || crashed.FinishedAt == nil {
		t.Fatalf("crashed console session = %+v", crashed)
	}
	if _, err := credentials.Authenticate(ctx, started.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate crashed console token err = %v, want ErrInvalidCredential", err)
	}
	if live, ok, err := sessions.liveConsoleJob(ctx); err != nil {
		t.Fatalf("live console job: %v", err)
	} else if ok {
		t.Fatalf("crashed console re-enqueued or left live job: %+v", live)
	}
}

// sessionFixture wires a project database (issues, changes, sessions, jobs,
// leases) together with the coordinator-wide global database (projects,
// workers, tokens) so author sessions can mint project-scoped session tokens.
type sessionFixture struct {
	store        *flowdb.Store
	global       *flowdb.Store
	sessions     *SessionService
	issues       *IssueService
	workers      *flowworker.Service
	directory    *flowworker.Directory
	credentials  *CredentialService
	checks       *CheckService
	checkConfigs *CheckConfigService
	reconciler   *ReconcileService
	project      Project
}

func newSessionServiceFixture(t *testing.T) sessionFixture {
	t.Helper()
	ctx := context.Background()

	store, err := flowdb.Open(ctx, t.TempDir()+"/flow.db")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	global, err := flowdb.OpenGlobal(ctx, t.TempDir()+"/global.db")
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() {
		_ = global.Close()
	})

	project := Project{
		ID:           testProjectID,
		Name:         "test",
		RepoPath:     "/tmp/session-fixture",
		BaseBranch:   "main",
		ExchangeName: "flow",
		ExchangeURL:  "file:///tmp/session-fixture.git",
	}
	// Session tokens carry a project binding with a foreign key into the global
	// projects registry, so the project row must exist before tokens are minted.
	if _, err := NewProjectService(global.DB()).Insert(ctx, project); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	credentials := NewCredentialService(global.DB())
	directory := flowworker.NewDirectory(global.DB())
	checks := NewCheckService(store.DB())
	threads := NewThreadService(store.DB())
	checkConfigs := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, threads, project, CheckConfigServiceOptions{})
	reconciler := NewReconcileService(store.DB())
	sessions := NewSessionServiceWithOptions(store.DB(), issues, workers, SessionServiceOptions{
		Credentials:      credentials,
		Project:          project,
		HandoffSnapshots: reconciler,
		ReviewRounds:     checkConfigs,
	})
	return sessionFixture{
		store:        store,
		global:       global,
		sessions:     sessions,
		issues:       issues,
		workers:      workers,
		directory:    directory,
		credentials:  credentials,
		checks:       checks,
		checkConfigs: checkConfigs,
		reconciler:   reconciler,
		project:      project,
	}
}

func entrypointCommandForTest(t *testing.T, payload map[string]any) string {
	t.Helper()
	entrypoint, ok := payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("entrypoint argv = %#v", entrypoint["argv"])
	}
	command, ok := argv[0].(string)
	if !ok {
		t.Fatalf("entrypoint command = %#v", argv[0])
	}
	return command
}

// claimNext adapts the single-project session tests to the cross-project claim
// entry point with one queue.
func (f sessionFixture) claimNext(ctx context.Context, input flowworker.ClaimInput) (flowworker.ClaimedJob, bool, error) {
	claim, ok, err := flowworker.ClaimAcrossProjects(ctx, f.directory, []flowworker.ProjectQueue{{ProjectID: f.project.ID, Queue: f.workers}}, input)
	return flowworker.ClaimedJob{Job: claim.Job, Lease: claim.Lease}, ok, err
}

func countRows(database *sql.DB, table string) (int, error) {
	var count int
	err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
	return count, err
}

// startAuthorSessionForFixture drives the schedule->claim->run->start path and
// returns the live author session so liveness tests can exercise it directly.
func startAuthorSessionForFixture(t *testing.T, fixture sessionFixture, title string) StartAuthorSessionResult {
	t.Helper()
	ctx := context.Background()

	issue, err := fixture.issues.CreateIssue(ctx, CreateIssueInput{Title: title})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.issues.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	ensured, err := fixture.sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	claim, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{WorkerID: "w-local", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claim.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claim.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.workers.MarkJobRunning(ctx, claim.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
		JobID:    claim.Job.ID,
		LeaseID:  claim.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	return started
}

func TestTouchAgentActivityStampsColumn(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	started := startAuthorSessionForFixture(t, fixture, "Agent liveness")

	// A fresh session has never demonstrated agent activity.
	before, err := fixture.sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session before touch: %v", err)
	}
	if before.LastAgentActivityAt != nil {
		t.Fatalf("LastAgentActivityAt before touch = %v, want nil", before.LastAgentActivityAt)
	}

	if err := fixture.sessions.TouchAgentActivity(ctx, started.Session.ID); err != nil {
		t.Fatalf("touch agent activity: %v", err)
	}

	after, err := fixture.sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session after touch: %v", err)
	}
	if after.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after touch = nil, want a timestamp")
	}
	if after.LastAgentActivityAt.IsZero() {
		t.Fatalf("LastAgentActivityAt after touch = zero time, want a real timestamp")
	}
}

func TestTouchAgentActivityRequiresSessionID(t *testing.T) {
	fixture := newSessionServiceFixture(t)
	if err := fixture.sessions.TouchAgentActivity(context.Background(), "   "); err == nil {
		t.Fatalf("TouchAgentActivity with blank id err = nil, want error")
	}
}

func TestMarkSessionMessageDeliveredTransitionsSessionAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := fixture.sessions
	started := startAuthorSessionForFixture(t, fixture, "Atomic delivery")

	// Park the session waiting on a human, the state MarkSessionMessageDelivered
	// must flip back to working when the reply is delivered.
	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}

	message, err := sessions.EnqueueSessionMessage(ctx, EnqueueSessionMessageInput{
		SessionID: started.Session.ID,
		Actor:     "human",
		Body:      "please continue",
	})
	if err != nil {
		t.Fatalf("enqueue session message: %v", err)
	}

	delivered, err := sessions.MarkSessionMessageDelivered(ctx, MarkSessionMessageDeliveredInput{
		SessionID: started.Session.ID,
		MessageID: message.ID,
		LeaseID:   started.Session.LeaseID,
	})
	if err != nil {
		t.Fatalf("mark session message delivered: %v", err)
	}
	if delivered.State != SessionMessageDelivered {
		t.Fatalf("message state = %q, want %q", delivered.State, SessionMessageDelivered)
	}
	if delivered.DeliveredAt == nil {
		t.Fatal("delivered_at = nil, want a timestamp")
	}

	session, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.RuntimeState != SessionWorking {
		t.Fatalf("session runtime state = %q, want %q", session.RuntimeState, SessionWorking)
	}
}

func TestMarkSessionMessageDeliveredIsIdempotentAndRejectsNonPending(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := fixture.sessions
	started := startAuthorSessionForFixture(t, fixture, "Idempotent delivery")

	if _, err := sessions.UpdateSessionState(ctx, started.Session.ID, SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}

	message, err := sessions.EnqueueSessionMessage(ctx, EnqueueSessionMessageInput{
		SessionID: started.Session.ID,
		Actor:     "human",
		Body:      "deliver once",
	})
	if err != nil {
		t.Fatalf("enqueue session message: %v", err)
	}

	if _, err := sessions.MarkSessionMessageDelivered(ctx, MarkSessionMessageDeliveredInput{
		SessionID: started.Session.ID,
		MessageID: message.ID,
		LeaseID:   started.Session.LeaseID,
	}); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// A second delivery finds no pending row and must report sql.ErrNoRows.
	if _, err := sessions.MarkSessionMessageDelivered(ctx, MarkSessionMessageDeliveredInput{
		SessionID: started.Session.ID,
		MessageID: message.ID,
		LeaseID:   started.Session.LeaseID,
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delivery err = %v, want sql.ErrNoRows", err)
	}

	// The already-delivered message stays delivered and the session stays working.
	stored, err := sessions.GetSessionMessage(ctx, message.ID)
	if err != nil {
		t.Fatalf("get session message: %v", err)
	}
	if stored.State != SessionMessageDelivered {
		t.Fatalf("message state = %q, want %q", stored.State, SessionMessageDelivered)
	}
	session, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.RuntimeState != SessionWorking {
		t.Fatalf("session runtime state = %q, want %q", session.RuntimeState, SessionWorking)
	}
}

func TestReconcileCrashedAuthorSessionsSkipsMalformedPayload(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers := fixture.sessions, fixture.workers

	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 2,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// Claim the healthy issue's session first, before the poisoned issue has any
	// queued replacement jobs, so its claim is unambiguous.
	healthy := startWaitingAuthorSession(t, ctx, fixture, "Healthy reconcile issue")

	// Drive the poisoned issue through one real crash so it accumulates a crashed
	// author attempt whose payload_json we can corrupt, while leaving the session's
	// own job parseable for GetJob.
	poisoned := startWaitingAuthorSession(t, ctx, fixture, "Malformed payload issue")
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), poisoned.leaseID); err != nil {
		t.Fatalf("expire poisoned lease: %v", err)
	}
	if _, err := sessions.ReconcileCrashedAuthorSessions(ctx); err != nil {
		t.Fatalf("first poisoned reconcile: %v", err)
	}
	replacement, ok, err := workers.LiveAuthorJobForIssue(ctx, poisoned.session.IssueID)
	if err != nil {
		t.Fatalf("live author job after first poisoned crash: %v", err)
	}
	if !ok {
		t.Fatal("first poisoned crash did not enqueue a replacement")
	}
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobCrashed), replacement.ID); err != nil {
		t.Fatalf("mark poisoned replacement crashed: %v", err)
	}
	// Corrupt the replacement crashed job's payload_json (not the session's own
	// job) so authorCrashRestartLimitReached's json.Unmarshal fails. The reconcile
	// tick must skip the bad payload (slog.Warn + continue), not abort the sweep.
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE jobs
SET payload_json = ?
WHERE id = ?`, "{not valid json", replacement.ID); err != nil {
		t.Fatalf("corrupt replacement payload_json: %v", err)
	}

	// Crash the healthy issue too; it must still recover in the same tick that
	// skips the poisoned issue's malformed payload.
	if _, err := sessions.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), healthy.leaseID); err != nil {
		t.Fatalf("expire healthy lease: %v", err)
	}

	recovered, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("reconcile aborted on malformed payload: %v", err)
	}
	if recovered < 1 {
		t.Fatalf("recovered = %d, want at least the healthy session", recovered)
	}

	// The healthy crashed session is re-enqueued despite the poisoned payload.
	healthyCrashed, err := sessions.GetSession(ctx, healthy.session.ID)
	if err != nil {
		t.Fatalf("get healthy session: %v", err)
	}
	if healthyCrashed.RuntimeState != SessionCrashed {
		t.Fatalf("healthy session runtime state = %q, want %q", healthyCrashed.RuntimeState, SessionCrashed)
	}
	if _, ok, err := workers.LiveAuthorJobForIssue(ctx, healthy.session.IssueID); err != nil {
		t.Fatalf("live author job for healthy issue: %v", err)
	} else if !ok {
		t.Fatal("healthy crashed author session was not re-enqueued")
	}
}

func TestMarkPersistentSessionExitedCrashesAndIsRestartEligible(t *testing.T) {
	// An interactive agent never exits cleanly on its own; reaching
	// MarkPersistentSessionExited means the session was not finalized, so every
	// un-finalized exit is a crash regardless of exit code. Exit 0 and a non-zero
	// exit must both crash the session, release the lease as JobCrashed, revoke
	// the token, and stay restart-eligible.
	for _, exitCode := range []int{0, 137} {
		t.Run(fmt.Sprintf("exit_%d", exitCode), func(t *testing.T) {
			ctx := context.Background()
			fixture := newSessionServiceFixture(t)
			sessions, workers := fixture.sessions, fixture.workers

			if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
				ID:                      "w-local",
				Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
				CapacityPersistentAgent: 1,
			}); err != nil {
				t.Fatalf("register worker: %v", err)
			}

			crashable := startWaitingAuthorSession(t, ctx, fixture, fmt.Sprintf("Exit %d issue", exitCode))

			exited, err := sessions.MarkPersistentSessionExited(ctx, MarkPersistentSessionExitedInput{
				SessionID: crashable.session.ID,
				LeaseID:   crashable.leaseID,
				ExitCode:  exitCode,
			})
			if err != nil {
				t.Fatalf("mark persistent session exited: %v", err)
			}
			if exited.RuntimeState != SessionCrashed || exited.FinishedAt == nil {
				t.Fatalf("session = %+v, want crashed with finished_at", exited)
			}

			lease, err := workers.GetLease(ctx, crashable.leaseID)
			if err != nil {
				t.Fatalf("get lease: %v", err)
			}
			if lease.ReleasedAt == nil {
				t.Fatal("lease ReleasedAt is nil, want released")
			}
			job, err := workers.GetJob(ctx, crashable.jobID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			if job.State != flowworker.JobCrashed {
				t.Fatalf("job state = %q, want %q", job.State, flowworker.JobCrashed)
			}
			// The session token lives in the coordinator-wide global database;
			// MarkPersistentSessionExited revokes it by hash so it no longer
			// authenticates.
			var revokedAt sql.NullString
			if err := fixture.global.DB().QueryRowContext(ctx, `
SELECT revoked_at FROM tokens WHERE token_hash = ?`, exited.TokenHash).Scan(&revokedAt); err != nil {
				t.Fatalf("read session token revocation: %v", err)
			}
			if !revokedAt.Valid {
				t.Fatal("session token revoked_at is NULL, want revoked after exit")
			}

			// The first un-finalized exit stays under the automatic restart limit,
			// so a reconcile tick re-enqueues a fresh author job for the issue.
			recovered, err := sessions.ReconcileCrashedAuthorSessions(ctx)
			if err != nil {
				t.Fatalf("reconcile crashed author sessions: %v", err)
			}
			if recovered != 1 {
				t.Fatalf("recovered = %d, want 1 restart-eligible session", recovered)
			}
			resume, ok, err := workers.LiveAuthorJobForIssue(ctx, crashable.session.IssueID)
			if err != nil {
				t.Fatalf("live author job after exit: %v", err)
			}
			if !ok {
				t.Fatal("exited author session was not re-enqueued")
			}
			if resume.ID == crashable.jobID {
				t.Fatal("resume job reused crashed job id")
			}
		})
	}
}

func TestMarkPersistentSessionExitedRejectsConsoleRole(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers := fixture.sessions, fixture.workers

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Codex})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
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
		t.Fatalf("claim console job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start console session: %v", err)
	}

	_, err = sessions.MarkPersistentSessionExited(ctx, MarkPersistentSessionExitedInput{
		SessionID: started.Session.ID,
		LeaseID:   claimed.Lease.ID,
		ExitCode:  0,
	})
	if err == nil || !strings.Contains(err.Error(), "console sessions are released through console release") {
		t.Fatalf("MarkPersistentSessionExited console err = %v, want console release rejection", err)
	}
	// The console session and its lease must be untouched by the rejected call.
	session, err := sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get console session: %v", err)
	}
	if session.RuntimeState == SessionCrashed {
		t.Fatalf("console session = %+v, want unchanged (not crashed)", session)
	}
	lease, err := workers.GetLease(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt != nil {
		t.Fatal("console lease released by rejected process-exit call")
	}
}
