package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

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
	if ensured.Job.Role != flowworker.RoleConsole || ensured.Job.TaskID != nil || ensured.Job.ChangeID != nil {
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
	if started.Session.Role != flowworker.RoleConsole || started.Session.TaskID != "" || started.Session.ChangeID != "" {
		t.Fatalf("started console session = %+v", started.Session)
	}
	principal, err := credentials.Authenticate(ctx, started.Token)
	if err != nil {
		t.Fatalf("authenticate console token: %v", err)
	}
	if principal.Scope != TokenScopeConsole || principal.Subject != started.Session.ID || principal.ProjectID == nil || *principal.ProjectID != fixture.project.ID || principal.SourceTaskID != nil {
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

// sessionFixture wires a project database (tasks, changes, sessions, jobs,
// leases) together with the coordinator-wide global database (projects,
// workers, tokens) so author sessions can mint project-scoped session tokens.
type sessionFixture struct {
	store        *flowdb.Store
	global       *flowdb.Store
	sessions     *SessionService
	tasks        *TaskService
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

	tasks := NewTaskService(store.DB())
	workers := flowworker.NewService(store.DB())
	credentials := NewCredentialService(global.DB())
	directory := flowworker.NewDirectory(global.DB())
	checks := NewCheckService(store.DB())
	threads := NewThreadService(store.DB())
	checkConfigs := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, threads, project, CheckConfigServiceOptions{})
	reconciler := NewReconcileService(store.DB())
	sessions := NewSessionServiceWithOptions(store.DB(), tasks, workers, SessionServiceOptions{
		Credentials:      credentials,
		Project:          project,
		HandoffSnapshots: reconciler,
		ReviewRounds:     checkConfigs,
	})
	return sessionFixture{
		store:        store,
		global:       global,
		sessions:     sessions,
		tasks:        tasks,
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
func TestTouchAgentActivityRequiresSessionID(t *testing.T) {
	fixture := newSessionServiceFixture(t)
	if err := fixture.sessions.TouchAgentActivity(context.Background(), "   "); err == nil {
		t.Fatalf("TouchAgentActivity with blank id err = nil, want error")
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
