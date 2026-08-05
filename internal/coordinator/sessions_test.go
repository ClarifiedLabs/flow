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

func TestEnsureChangeUsesTaskAlignedIncrementingIDs(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Friendly change IDs"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	first, err := fixture.sessions.ensureChange(ctx, task.ID, "task/"+task.ID+"/run-1", "main")
	if err != nil {
		t.Fatalf("ensure first change: %v", err)
	}
	if first.ID != "ch-test-0001" {
		t.Fatalf("first change ID = %q, want ch-test-0001", first.ID)
	}

	replayed, err := fixture.sessions.ensureChange(ctx, task.ID, first.Branch, "main")
	if err != nil {
		t.Fatalf("replay first change: %v", err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("replayed change ID = %q, want %q", replayed.ID, first.ID)
	}

	for sequence, want := range []string{"ch-test-0001-2", "ch-test-0001-3"} {
		change, err := fixture.sessions.ensureChange(ctx, task.ID, fmt.Sprintf("task/%s/run-%d", task.ID, sequence+2), "main")
		if err != nil {
			t.Fatalf("ensure change %d: %v", sequence+2, err)
		}
		if change.ID != want {
			t.Fatalf("change %d ID = %q, want %q", sequence+2, change.ID, want)
		}
	}
}

func TestInsertWithTaskChangeIDRecoversConcurrentLogicalChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Concurrent friendly change ID"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	branch := "task/" + task.ID + "/run-1"
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	type allocation struct {
		id      string
		created bool
		err     error
	}
	allocations := make(chan allocation, 2)
	for range 2 {
		go func() {
			firstAttempt := true
			id, created, err := insertWithTaskChangeID(ctx, fixture.store.DB(), task.ID, branch, "", func(id string) error {
				if firstAttempt {
					firstAttempt = false
					ready <- struct{}{}
					<-release
				}
				_, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, created_at, updated_at)
VALUES (?, ?, ?, 'main', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, id, task.ID, branch)
				return err
			})
			allocations <- allocation{id: id, created: created, err: err}
		}()
	}
	<-ready
	<-ready
	close(release)

	createdCount := 0
	for range 2 {
		allocation := <-allocations
		if allocation.err != nil {
			t.Fatalf("allocate concurrent change: %v", allocation.err)
		}
		if allocation.id != "ch-test-0001" {
			t.Errorf("concurrent change ID = %q, want ch-test-0001", allocation.id)
		}
		if allocation.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created allocations = %d, want 1", createdCount)
	}
}

func TestConsoleSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers, credentials := fixture.sessions, fixture.workers, fixture.credentials

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Harness})
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
	if payloadString(ensured.Job.Payload, "console_harness") != flowharness.Harness || payloadString(ensured.Job.Payload, "session_purpose") != "console" {
		t.Fatalf("console payload = %+v", ensured.Job.Payload)
	}
	if got := ensured.Job.Selector; len(got) != 0 {
		t.Fatalf("console selector = %#v, want no scheduling requirements", ensured.Job.Selector)
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
	if !ok || !strings.Contains(command, `harness --session "$FLOW_HARNESS_SESSION" --hooks "$FLOW_HARNESS_HOOKS"`) {
		t.Fatalf("console command = %#v", entrypoint["argv"])
	}
	for _, unexpected := range []string{"flow fetch-prompt", `"$prompt"`, "flow-console"} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("console command includes prompt setup %q:\n%s", unexpected, command)
		}
	}
	replayed, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Harness})
	if err != nil {
		t.Fatalf("replay console job: %v", err)
	}
	if !replayed.Existing || replayed.Job.ID != ensured.Job.ID {
		t.Fatalf("replayed console = %+v, want existing %s", replayed, ensured.Job.ID)
	}

	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:             "w-local",
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
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
		Harness:  flowharness.Harness,
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

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Harness})
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

func TestReconcileCrashedWorkflowAuthorSessionAllowsReplacement(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers, credentials := fixture.sessions, fixture.workers, fixture.credentials

	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Retry crashed workflow author"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	const (
		workflowRunID = "wr-crashed-author"
		nodeRunID     = "wnr-crashed-author"
	)
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', 'author', ?, 10, ?, ?)`,
		workflowRunID, task.ID, testWorkflowSnapshotJSON(t, "author"), nodeRunID, formatTime(time.Now().UTC()), formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES (?, ?, 'author', 1, 1, 'running', ?, ?)`,
		nodeRunID, workflowRunID, formatTime(time.Now().UTC()), formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("insert workflow node run: %v", err)
	}

	// Workers are one-shot: worker_assignments.worker_id is globally unique, so
	// each claim uses a fresh worker id.
	registerWorker := func(workerID string) {
		t.Helper()
		if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
			ID:             workerID,
			CapacityBucket: flowworker.BucketPersistentAgent,
		}); err != nil {
			t.Fatalf("register worker %s: %v", workerID, err)
		}
	}
	enqueueWorkflowAuthor := func(attempt int) flowworker.Job {
		t.Helper()
		runID := workflowRunID
		currentNodeRunID := nodeRunID
		job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			TaskID:         &task.ID,
			WorkflowRunID:  &runID,
			NodeRunID:      &currentNodeRunID,
			Role:           flowworker.RoleAuthor,
			CapacityBucket: flowworker.BucketPersistentAgent,
			Payload: map[string]any{
				"base":           "main",
				"workspace_mode": string(WorkspaceBase),
				"node_attempt":   attempt, "agent_harness": "harness", "phase_index": 0, "final_phase": true},
		})
		if err != nil {
			t.Fatalf("enqueue workflow author attempt %d: %v", attempt, err)
		}
		return job
	}
	startWorkflowAuthor := func(job flowworker.Job, workerID string) StartAuthorSessionResult {
		t.Helper()
		registerWorker(workerID)
		claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
			WorkerID:      workerID,
			LeaseDuration: time.Minute,
		})
		if err != nil {
			t.Fatalf("claim workflow author %s: %v", job.ID, err)
		}
		if !ok || claimed.Job.ID != job.ID {
			t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
		}
		if _, err := workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
			t.Fatalf("mark workflow author %s running: %v", job.ID, err)
		}
		started, err := sessions.StartAuthorSession(ctx, StartAuthorSessionInput{
			JobID:    job.ID,
			LeaseID:  claimed.Lease.ID,
			WorkerID: workerID,
			Harness:  flowharness.Harness,
		})
		if err != nil {
			t.Fatalf("start workflow author %s: %v", job.ID, err)
		}
		return started
	}

	firstJob := enqueueWorkflowAuthor(1)
	first := startWorkflowAuthor(firstJob, "w-local")
	if _, err := sessions.UpdateSessionState(ctx, first.Session.ID, SessionWorking); err != nil {
		t.Fatalf("mark first workflow session working: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `
UPDATE leases SET expires_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC().Add(-time.Minute)), first.Session.LeaseID); err != nil {
		t.Fatalf("expire first workflow lease: %v", err)
	}

	recovered, err := sessions.ReconcileCrashedAuthorSessions(ctx)
	if err != nil {
		t.Fatalf("reconcile crashed workflow author: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered jobs = %d, want workflow retry to remain operator-owned", recovered)
	}
	crashed, err := sessions.GetSession(ctx, first.Session.ID)
	if err != nil {
		t.Fatalf("get crashed workflow session: %v", err)
	}
	if crashed.RuntimeState != SessionCrashed || crashed.FinishedAt == nil {
		t.Fatalf("crashed workflow session = %+v", crashed)
	}
	if _, err := credentials.Authenticate(ctx, first.Token); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate crashed workflow token err = %v, want ErrInvalidCredential", err)
	}
	jobs, err := workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs after workflow reconciliation: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != firstJob.ID || jobs[0].State != flowworker.JobCrashed {
		t.Fatalf("jobs after workflow reconciliation = %+v, want only crashed job %s", jobs, firstJob.ID)
	}

	replacementJob := enqueueWorkflowAuthor(2)
	replacement := startWorkflowAuthor(replacementJob, "w-local-2")
	if replacement.Session.ID == first.Session.ID ||
		replacement.Session.WorkflowRunID != workflowRunID ||
		replacement.Session.NodeRunID != nodeRunID {
		t.Fatalf("replacement workflow session = %+v, first = %s", replacement.Session, first.Session.ID)
	}
}

func TestReconcileCrashedConsoleSessionDoesNotReenqueue(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions, workers, credentials := fixture.sessions, fixture.workers, fixture.credentials

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Harness})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:             "w-local",
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
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

func TestEnsureAuthorJobUsesConfiguredDefaultAgent(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := NewSessionServiceWithOptions(fixture.store.DB(), fixture.tasks, fixture.workers, SessionServiceOptions{
		Credentials: fixture.credentials,
		Project:     fixture.project,
		DefaultAgent: flowharness.AgentSelection{
			Harness:         flowharness.Harness,
			Model:           "anthropic:claude-sonnet-4-6",
			ReasoningEffort: "high",
		},
		HarnessArgs: []string{"--model", "openai:gpt-5"},
	})
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Configured default agent"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE tasks SET lifecycle_state = 'scheduled' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("schedule task: %v", err)
	}

	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{TaskID: task.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	payload := ensured.Job.Payload
	if got := payloadString(payload, "agent_harness"); got != flowharness.Harness {
		t.Fatalf("agent_harness = %q, want harness", got)
	}
	if got := payloadString(payload, "prompt_harness"); got != flowharness.Harness {
		t.Fatalf("prompt_harness = %q, want harness", got)
	}
	if got := ensured.Job.Selector; len(got) != 0 {
		t.Fatalf("selector = %#v, want no scheduling requirements", ensured.Job.Selector)
	}
	entrypoint, ok := payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 {
		t.Fatalf("entrypoint argv = %#v", entrypoint["argv"])
	}
	command, _ := argv[0].(string)
	if !strings.Contains(command, `harness --session "$FLOW_HARNESS_SESSION" --hooks "$FLOW_HARNESS_HOOKS"`) {
		t.Fatalf("entrypoint command does not launch harness:\n%s", command)
	}
	// The configured default model/effort tokens precede the manual
	// harness_args so the manual --model wins (last-token-wins).
	defaultIdx := strings.Index(command, "'--model' 'anthropic:claude-sonnet-4-6' '--reasoning' 'high'")
	manualIdx := strings.Index(command, "'--model' 'openai:gpt-5'")
	if defaultIdx < 0 || manualIdx < 0 {
		t.Fatalf("entrypoint command missing default or manual model tokens:\n%s", command)
	}
	if defaultIdx > manualIdx {
		t.Fatalf("default model tokens must precede harness_args:\n%s", command)
	}
}

func TestEnsureAuthorJobExplicitEntrypointOverridesDefaultAgent(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	sessions := NewSessionServiceWithOptions(fixture.store.DB(), fixture.tasks, fixture.workers, SessionServiceOptions{
		Credentials:                     fixture.credentials,
		Project:                         fixture.project,
		DefaultAuthorEntrypoint:         map[string]any{"argv": []string{"harness --continue"}, "shell": true, "harness": "harness"},
		DefaultAuthorEntrypointOverride: true,
		DefaultAgent: flowharness.AgentSelection{
			Harness: flowharness.Harness,
			Model:   "openai:gpt-5",
		},
	})
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Explicit entrypoint override"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE tasks SET lifecycle_state = 'scheduled' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("schedule task: %v", err)
	}

	ensured, err := sessions.EnsureAuthorJob(ctx, EnsureAuthorJobInput{TaskID: task.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	entrypoint, ok := ensured.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("entrypoint payload = %#v", ensured.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 || argv[0] != "harness --continue" {
		t.Fatalf("entrypoint argv = %#v, want the explicit override", entrypoint["argv"])
	}
	if !payloadBool(ensured.Job.Payload, "inject_initial_prompt") {
		t.Fatal("inject_initial_prompt = false, want true for the explicit override")
	}
}

func TestAuthorJobMatchesRequiresStampedPayload(t *testing.T) {
	changeID := "ch-test-0001"
	stamped := map[string]any{"branch": "task/t-1", "base": "main", "agent_harness": "harness", "phase_index": 0, "final_phase": true}

	for _, tc := range []struct {
		name         string
		payload      map[string]any
		agentHarness string
		phaseIndex   int
		want         bool
	}{
		{name: "stamped payload matches", payload: stamped, agentHarness: "harness", phaseIndex: 0, want: true},
		{name: "stamped payload rejected for other harness", payload: stamped, agentHarness: "agents", phaseIndex: 0, want: false},
		{name: "stamped payload rejected for other phase", payload: stamped, agentHarness: "harness", phaseIndex: 1, want: false},
		// A corrupt payload (absent agent_harness or phase_index) never matches.
		{name: "missing agent_harness never matches", payload: map[string]any{"branch": "task/t-1", "base": "main", "phase_index": 0}, agentHarness: "", phaseIndex: 0, want: false},
		{name: "missing phase_index never matches", payload: map[string]any{"branch": "task/t-1", "base": "main", "agent_harness": "harness"}, agentHarness: "harness", phaseIndex: 0, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := flowworker.Job{ChangeID: &changeID, Payload: tc.payload}
			if got := authorJobMatches(job, changeID, "task/t-1", "main", tc.agentHarness, tc.phaseIndex); got != tc.want {
				t.Fatalf("authorJobMatches = %t, want %t", got, tc.want)
			}
		})
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
	}
	// Session tokens carry a project binding with a foreign key into the global
	// projects registry, so the project row must exist before tokens are minted.
	if _, err := NewProjectService(global.DB()).Insert(ctx, project); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	tasks := NewTaskService(store.DB(), testProjectID)
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

// claimNext adapts the single-project session tests to the assignment-created
// worker model: it reserves an assignment binding the worker to the next
// eligible queued job, then claims that exact assignment.
func (f sessionFixture) claimNext(ctx context.Context, input flowworker.ClaimInput) (flowworker.ClaimedJob, bool, error) {
	workerRow, err := f.directory.GetWorker(ctx, input.WorkerID)
	if err != nil {
		return flowworker.ClaimedJob{}, false, err
	}
	jobs, err := f.workers.ListJobs(ctx)
	if err != nil {
		return flowworker.ClaimedJob{}, false, err
	}
	for _, job := range jobs {
		if job.State != flowworker.JobQueued {
			continue
		}
		assignment, err := f.workers.ReserveAssignment(ctx, flowworker.ReserveAssignmentInput{
			JobID: job.ID, WorkerID: input.WorkerID, ProviderID: "test-provider", ProfileName: "test-profile", ProviderType: "test",
			ProviderRequestID: "test-request-" + job.ID + "-" + input.WorkerID,
			ProfileLabels:     job.Selector,
			AllowedRoles:      []flowworker.JobRole{job.Role},
			AllowedBuckets:    []flowworker.CapacityBucket{job.CapacityBucket},
			RequiredSelector:  job.Selector,
			StartupDeadline:   time.Now().UTC().Add(10 * time.Minute),
		})
		if err != nil {
			continue
		}
		claimed, err := f.workers.ClaimAssignment(ctx, flowworker.ClaimAssignmentInput{
			AssignmentID: assignment.ID, Worker: workerRow, LeaseDuration: input.LeaseDuration,
		})
		if err != nil {
			return flowworker.ClaimedJob{}, false, err
		}
		return claimed, true, nil
	}
	return flowworker.ClaimedJob{}, false, nil
}

func countRows(database *sql.DB, table string) (int, error) {
	var count int
	err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
	return count, err
}

func TestTaskSessionRevocationAndGitWriteLivenessFence(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Task console revocation"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensured, err := fixture.sessions.EnsureTaskConsoleJob(ctx, EnsureTaskConsoleJobInput{
		TaskID: task.ID, Harness: flowharness.Harness,
	})
	if err != nil {
		t.Fatalf("ensure task console: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:             "w-task-console",
		HeartbeatTTL:   time.Minute,
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID: "w-task-console", LeaseDuration: time.Minute,
	})
	if err != nil || !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim task console: claim=%+v ok=%t err=%v", claimed.Job, ok, err)
	}
	if _, err := fixture.workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark task console running: %v", err)
	}
	started, err := fixture.sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID: claimed.Job.ID, LeaseID: claimed.Lease.ID, WorkerID: "w-task-console", Harness: flowharness.Harness,
	})
	if err != nil {
		t.Fatalf("start task console: %v", err)
	}
	if allowed, err := fixture.sessions.SessionAllowsGitWrites(ctx, started.Session.ID); err != nil || !allowed {
		t.Fatalf("live session git writes: allowed=%t err=%v", allowed, err)
	}
	if err := fixture.sessions.RevokeTaskSessionTokens(ctx, task.ID); err != nil {
		t.Fatalf("revoke task session tokens: %v", err)
	}
	if _, err := fixture.credentials.Authenticate(ctx, started.Token); err == nil {
		t.Fatal("revoked task console token still authenticates")
	}
	if _, err := fixture.workers.ReleaseLease(ctx, claimed.Lease.ID, flowworker.JobCanceled); err != nil {
		t.Fatalf("cancel task console runtime: %v", err)
	}
	if allowed, err := fixture.sessions.SessionAllowsGitWrites(ctx, started.Session.ID); err != nil || allowed {
		t.Fatalf("canceled session git writes: allowed=%t err=%v", allowed, err)
	}
}

func TestLateTaskConsoleExitDoesNotRestoreTerminalTaskChange(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)
	task, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Late task console exit"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensured, err := fixture.sessions.EnsureTaskConsoleJob(ctx, EnsureTaskConsoleJobInput{
		TaskID: task.ID, Harness: flowharness.Harness,
	})
	if err != nil {
		t.Fatalf("ensure task console: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:             "w-late-console",
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID: "w-late-console", LeaseDuration: time.Minute,
	})
	if err != nil || !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim task console: claim=%+v ok=%t err=%v", claimed.Job, ok, err)
	}
	if _, err := fixture.workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark task console running: %v", err)
	}
	started, err := fixture.sessions.StartConsoleSession(ctx, StartConsoleSessionInput{
		JobID: claimed.Job.ID, LeaseID: claimed.Lease.ID, WorkerID: "w-late-console", Harness: flowharness.Harness,
	})
	if err != nil {
		t.Fatalf("start task console: %v", err)
	}

	exchangePath := t.TempDir() + "/exchange.git"
	workPath := t.TempDir() + "/work"
	if _, err := reconcileGitOutput("", nil, "init", "--bare", exchangePath); err != nil {
		t.Fatalf("init exchange: %v", err)
	}
	if _, err := reconcileGitOutput("", nil, "init", "-b", "main", workPath); err != nil {
		t.Fatalf("init worktree: %v", err)
	}
	if _, err := reconcileGitOutput(workPath, nil, "-c", "user.name=Flow Test", "-c", "user.email=flow@example.test", "commit", "--allow-empty", "-m", "repair"); err != nil {
		t.Fatalf("commit repair tip: %v", err)
	}
	repairTip, err := reconcileGitOutput(workPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repair tip: %v", err)
	}
	repairTip = strings.TrimSpace(repairTip)
	if _, err := reconcileGitOutput(workPath, nil, "push", exchangePath, "HEAD:refs/heads/"+started.Session.Branch); err != nil {
		t.Fatalf("create repaired branch at %s: %v", repairTip, err)
	}
	fixture.sessions.project.ExchangePath = exchangePath

	const preservedHead = "preserved-terminal-head"
	if _, err := fixture.store.DB().ExecContext(ctx, `
UPDATE changes SET head_sha = ? WHERE id = ?;
UPDATE sessions SET runtime_state = 'abandoned' WHERE id = ?`, preservedHead, started.Session.ChangeID, started.Session.ID); err != nil {
		t.Fatalf("simulate force-done cleanup: %v", err)
	}
	exited, err := fixture.sessions.markConsoleSessionExited(ctx, started.Session)
	if err != nil {
		t.Fatalf("acknowledge late console exit: %v", err)
	}
	if exited.RuntimeState != SessionAbandoned {
		t.Fatalf("late exit session state = %q, want abandoned", exited.RuntimeState)
	}
	var head string
	if err := fixture.store.DB().QueryRowContext(ctx, `SELECT head_sha FROM changes WHERE id = ?`, started.Session.ChangeID).Scan(&head); err != nil {
		t.Fatalf("load preserved change projection: %v", err)
	}
	if head != preservedHead {
		t.Fatalf("late exit restored change head %q, want %q", head, preservedHead)
	}
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

	ensured, err := sessions.EnsureConsoleJob(ctx, EnsureConsoleJobInput{Harness: flowharness.Harness})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}
	if _, err := fixture.directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:             "w-local",
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.claimNext(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
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

// TestLatestSessionForTasks pins the batched latest-session lookup's semantics:
// each task maps to the session that ListSessionsForTask(ctx, taskID, 1) would
// return (updated_at, then created_at, then id, all descending), and tasks
// without sessions are absent from the result.
func TestLatestSessionForTasks(t *testing.T) {
	ctx := context.Background()
	fixture := newSessionServiceFixture(t)

	withSessions, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Latest session with sessions"})
	if err != nil {
		t.Fatalf("create task with sessions: %v", err)
	}
	single, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Latest session single"})
	if err != nil {
		t.Fatalf("create single-session task: %v", err)
	}
	empty, err := fixture.tasks.CreateTask(ctx, CreateTaskInput{Title: "Latest session empty"})
	if err != nil {
		t.Fatalf("create empty task: %v", err)
	}

	insertSession := func(t *testing.T, id string, taskID string, createdAt, updatedAt time.Time, activity *time.Time) {
		t.Helper()
		now := formatTime(time.Now().UTC())
		jobID := "j-" + id
		leaseID := "l-" + id
		if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO jobs (id, task_id, role, state, capacity_bucket, created_at, updated_at)
VALUES (?, ?, 'author', 'finished', 'persistent_agent', ?, ?)`,
			jobID, taskID, now, now); err != nil {
			t.Fatalf("insert job for session %s: %v", id, err)
		}
		if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-test', 'persistent_agent', ?, ?)`,
			leaseID, jobID, now, now); err != nil {
			t.Fatalf("insert lease for session %s: %v", id, err)
		}
		var activityText any
		if activity != nil {
			activityText = formatTime(*activity)
		}
		if _, err := fixture.store.DB().ExecContext(ctx, `
INSERT INTO sessions (
	id, task_id, job_id, lease_id, worker_id, role, workspace_mode, runtime_state,
	branch, base, harness, token_hash, created_at, updated_at, last_agent_activity_at
) VALUES (?, ?, ?, ?, 'w-test', 'author', 'change', 'finished',
	'task/latest', 'main', 'harness', ?, ?, ?, ?)`,
			id, taskID, jobID, leaseID, "tok-"+id,
			formatTime(createdAt), formatTime(updatedAt), activityText); err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}
	}

	ten := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	eleven := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	noon := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	one := time.Date(2026, 1, 2, 13, 0, 0, 0, time.UTC)

	// s-1 loses on updated_at; s-2 loses on created_at (same updated_at as
	// s-3); s-3/s-4/s-5 tie on updated_at and created_at, so id DESC decides,
	// and s-5 carries the newest activity of the tied trio.
	insertSession(t, "s-1", withSessions.ID, ten, eleven, &ten)
	insertSession(t, "s-2", withSessions.ID, ten, noon, &eleven)
	insertSession(t, "s-3", withSessions.ID, eleven, noon, &noon)
	insertSession(t, "s-4", withSessions.ID, eleven, noon, &one)
	insertSession(t, "s-5", withSessions.ID, eleven, noon, pointerTime(one.Add(time.Minute)))
	insertSession(t, "s-single", single.ID, one, one, &one)

	latest, err := fixture.sessions.LatestSessionForTasks(ctx, []string{withSessions.ID, single.ID, empty.ID})
	if err != nil {
		t.Fatalf("latest sessions for tasks: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest sessions = %+v, want exactly the two tasks with sessions", latest)
	}
	if got := latest[withSessions.ID]; got.ID != "s-5" || got.LastAgentActivityAt == nil || !got.LastAgentActivityAt.Equal(one.Add(time.Minute)) {
		t.Fatalf("latest session for %s = %+v, want s-5 with activity %v", withSessions.ID, got, one.Add(time.Minute))
	}
	if got := latest[single.ID]; got.ID != "s-single" || got.LastAgentActivityAt == nil || !got.LastAgentActivityAt.Equal(one) {
		t.Fatalf("latest session for %s = %+v, want s-single with activity %v", single.ID, got, one)
	}
	if _, ok := latest[empty.ID]; ok {
		t.Fatalf("latest sessions contains %s, want absent (no sessions)", empty.ID)
	}

	// The batched lookup must agree with the per-task reader it replaces.
	for taskID, want := range map[string]string{withSessions.ID: "s-5", single.ID: "s-single"} {
		got, err := fixture.sessions.ListSessionsForTask(ctx, taskID, 1)
		if err != nil {
			t.Fatalf("list sessions for %s: %v", taskID, err)
		}
		if len(got) != 1 || got[0].ID != want {
			t.Fatalf("ListSessionsForTask(%s) = %+v, want [%s]", taskID, got, want)
		}
	}
}

func pointerTime(value time.Time) *time.Time {
	return &value
}
