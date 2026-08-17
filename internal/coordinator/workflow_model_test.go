package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func newWorkflowModelServices(t *testing.T) (*FlowService, *TaskService, *WorkflowRunService) {
	t.Helper()
	store, err := flowdb.Open(context.Background(), filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open workflow model database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	flows := NewFlowService(store.DB())
	tasks := NewTaskService(store.DB(), "p-test")
	return flows, tasks, NewWorkflowRunService(store.DB(), flows, tasks)
}

func TestForceDoneCleansTaskScopedConsoleWithoutWorkflowRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks, runs := newWorkflowModelServices(t)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Unscheduled task with console"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	workers := flowworker.NewService(runs.db)
	job, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, Role: flowworker.RoleConsole, CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{"console_harness": "harness"},
	})
	if err != nil {
		t.Fatalf("enqueue task console: %v", err)
	}
	now := time.Now().UTC()
	if _, err := runs.db.ExecContext(ctx, `UPDATE jobs SET state = 'claimed', updated_at = ? WHERE id = ?`, formatTime(now), job.ID); err != nil {
		t.Fatalf("claim task console: %v", err)
	}
	const leaseID = "l-force-done-task-console"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-force-done', 'persistent_agent', ?, ?)`, leaseID, job.ID,
		formatTime(now), formatTime(now.Add(time.Minute))); err != nil {
		t.Fatalf("lease task console: %v", err)
	}
	if _, err := workers.MarkJobRunning(ctx, leaseID); err != nil {
		t.Fatalf("start task console: %v", err)
	}
	const sessionID = "s-force-done-task-console"
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO sessions (
	id, task_id, job_id, lease_id, worker_id, role, workspace_mode, runtime_state,
	branch, base, harness, token_hash, created_at, updated_at
) VALUES (?, ?, ?, ?, 'w-force-done', 'console', 'change', 'working',
	'task/force-done', 'main', 'shell', 'force-done-token-hash', ?, ?)`,
		sessionID, task.ID, job.ID, leaseID, formatTime(now), formatTime(now)); err != nil {
		t.Fatalf("insert task console session: %v", err)
	}

	if _, err := runs.ForceDone(ctx, task.ID, ResolutionCancelled, "operator cancellation", ActorHuman); err != nil {
		t.Fatalf("force done task: %v", err)
	}
	canceledJob, err := workers.GetJob(ctx, job.ID)
	if err != nil || canceledJob.State != flowworker.JobCanceled {
		t.Fatalf("task console job after force done = %+v err=%v, want canceled", canceledJob, err)
	}
	releasedLease, err := workers.GetLease(ctx, leaseID)
	if err != nil || releasedLease.ReleasedAt == nil {
		t.Fatalf("task console lease after force done = %+v err=%v, want released", releasedLease, err)
	}
	var runtimeState string
	if err := runs.db.QueryRowContext(ctx, `SELECT runtime_state FROM sessions WHERE id = ?`, sessionID).Scan(&runtimeState); err != nil {
		t.Fatalf("load task console session: %v", err)
	}
	if runtimeState != string(SessionAbandoned) {
		t.Fatalf("task console session state after force done = %q, want abandoned", runtimeState)
	}
}

func TestWorkflowModelHumanGateCanUseCustomOutcomeAndComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)

	flow, err := flows.Create(ctx, FlowInput{
		Name:             "release decision",
		StartNode:        "decision",
		TransitionBudget: 5,
		Nodes: []FlowNodeInput{
			{Key: "decision", Name: "Release decision", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"ship_it", "stop"}}}},
			{Key: "shipped", Name: "Shipped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "stopped", Name: "Stopped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCancelled}}},
		},
		Edges: []FlowEdgeInput{
			{From: "decision", Outcome: "ship_it", To: "shipped"},
			{From: "decision", Outcome: "stop", To: "stopped"},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Release", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.State != nil {
		t.Fatalf("new task state = %v, want Unscheduled/null", task.State)
	}

	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	if run.State != WorkflowRunScheduled {
		t.Fatalf("scheduled run state = %q", run.State)
	}
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("enter human gate: %v", err)
	}
	if !created || nodeRun.State != WorkflowNodeWaiting {
		t.Fatalf("human gate node = %+v, created=%t", nodeRun, created)
	}
	state, wait, err := runs.Substate(ctx, task.ID)
	if err != nil {
		t.Fatalf("derive substate: %v", err)
	}
	if state != InProgressBlocked || wait == nil || wait.NodeRunID != nodeRun.ID {
		t.Fatalf("substate = %q wait=%+v, want Blocked on active node", state, wait)
	}

	if _, err := runs.Respond(ctx, task.ID, nodeRun.ID, "", "ship_it", "release approved", ActorHuman); !errors.Is(err, ErrReviewWaitIDRequired) {
		t.Fatalf("respond without review wait ID err = %v, want %v", err, ErrReviewWaitIDRequired)
	}

	result, err := runs.Respond(ctx, task.ID, nodeRun.ID, wait.ID, "ship_it", "release approved", ActorHuman)
	if err != nil {
		t.Fatalf("respond to human gate: %v", err)
	}
	if !result.Done || result.Run.State != WorkflowRunCompleted || result.Run.TransitionsUsed != 1 {
		t.Fatalf("completion result = %+v", result)
	}
	completed, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("load completed task: %v", err)
	}
	if completed.State == nil || *completed.State != LifecycleDone || completed.DoneResolution == nil || *completed.DoneResolution != ResolutionCompleted {
		t.Fatalf("completed task = %+v", completed)
	}

	reopened, err := runs.Reopen(ctx, task.ID, ActorHuman)
	if err != nil {
		t.Fatalf("reopen task: %v", err)
	}
	if reopened.State != nil || reopened.DoneResolution != nil || reopened.DoneAt != nil {
		t.Fatalf("reopened task = %+v, want Unscheduled without resolution", reopened)
	}
}

func TestClaimTaskForSchedulingRejectsStaleHumanReviewChoice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks, runs := newWorkflowModelServices(t)
	stale, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Review setting changes during scheduling"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	required := true
	if _, err := tasks.EditTask(ctx, stale.ID, EditTaskInput{RequiresHumanReview: &required}); err != nil {
		t.Fatalf("edit task review setting: %v", err)
	}

	tx, err := runs.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin scheduling transaction: %v", err)
	}
	defer tx.Rollback()
	if err := claimTaskForScheduling(ctx, tx, stale, FlowSnapshot{FlowID: "fl-stale", FlowRevision: 1, AgentDefsRevision: 1}, time.Now().UTC()); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("claim with stale review setting error = %v, want workflow conflict", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback stale claim: %v", err)
	}

	current, err := tasks.GetTask(ctx, stale.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if !current.RequiresHumanReview || current.State != nil {
		t.Fatalf("task after stale claim = requires review %t state %v, want true and unscheduled", current.RequiresHumanReview, current.State)
	}
}

func TestWorkflowScheduleRetriesWhenFlowRevisionChanges(t *testing.T) {
	flowInput := func(name string, withMandatoryGate bool) FlowInput {
		input := FlowInput{
			Name:      name,
			StartNode: "done",
			Nodes: []FlowNodeInput{
				{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			},
		}
		if withMandatoryGate {
			input.StartNode = "approval"
			input.Nodes = append([]FlowNodeInput{
				{Key: "approval", Name: "Mandatory approval", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved"}}}},
			}, input.Nodes...)
			input.Edges = []FlowEdgeInput{{From: "approval", Outcome: "approved", To: "done"}}
		}
		return input
	}

	for _, tc := range []struct {
		name           string
		initialHasGate bool
		updatedHasGate bool
	}{
		{name: "adds mandatory gate", initialHasGate: false, updatedHasGate: true},
		{name: "removes mandatory gate", initialHasGate: true, updatedHasGate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			flows, tasks, runs := newWorkflowModelServices(t)
			flow, err := flows.Create(ctx, flowInput("revision race", tc.initialHasGate))
			if err != nil {
				t.Fatalf("create flow: %v", err)
			}
			task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Schedule current flow", FlowID: flow.ID})
			if err != nil {
				t.Fatalf("create task: %v", err)
			}

			resolved := make(chan FlowSnapshot, 1)
			resumeScheduling := make(chan struct{})
			var resolutionCount atomic.Int32
			runs.scheduleSnapshotResolvedTestHook = func(snapshot FlowSnapshot) {
				if resolutionCount.Add(1) == 1 {
					resolved <- snapshot
					<-resumeScheduling
				}
			}
			type scheduleResult struct {
				run WorkflowRun
				err error
			}
			scheduled := make(chan scheduleResult, 1)
			go func() {
				run, scheduleErr := runs.Schedule(ctx, task.ID)
				scheduled <- scheduleResult{run: run, err: scheduleErr}
			}()

			var stale FlowSnapshot
			select {
			case stale = <-resolved:
			case result := <-scheduled:
				t.Fatalf("scheduling finished before revision update: run=%+v err=%v", result.run, result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for initial snapshot resolution")
			}
			updated, updateErr := flows.Update(ctx, flow.ID, flowInput(flow.Name, tc.updatedHasGate))
			close(resumeScheduling)
			if updateErr != nil {
				t.Fatalf("update flow during scheduling: %v", updateErr)
			}

			var result scheduleResult
			select {
			case result = <-scheduled:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for scheduling retry")
			}
			if result.err != nil {
				t.Fatalf("schedule after flow update: %v", result.err)
			}
			if stale.FlowRevision != flow.Revision {
				t.Fatalf("initial snapshot revision = %d, want %d", stale.FlowRevision, flow.Revision)
			}
			if updated.Revision <= stale.FlowRevision {
				t.Fatalf("updated revision = %d, want greater than stale revision %d", updated.Revision, stale.FlowRevision)
			}
			if resolutionCount.Load() < 2 {
				t.Fatalf("snapshot resolutions = %d, want retry after revision changed", resolutionCount.Load())
			}
			if result.run.Snapshot.FlowRevision != updated.Revision {
				t.Fatalf("committed snapshot revision = %d, want current revision %d", result.run.Snapshot.FlowRevision, updated.Revision)
			}
			_, committedHasGate := result.run.Snapshot.Node("approval")
			if committedHasGate != tc.updatedHasGate {
				t.Fatalf("committed snapshot has mandatory gate = %t, want %t", committedHasGate, tc.updatedHasGate)
			}
			var runCount int
			if err := runs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, task.ID).Scan(&runCount); err != nil {
				t.Fatalf("count committed runs: %v", err)
			}
			if runCount != 1 {
				t.Fatalf("committed runs = %d, want exactly one current snapshot", runCount)
			}
		})
	}
}

func TestWorkflowScheduleRetriesWhenAgentDefinitionChanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		catalog  string
		override bool
	}{
		{name: "local definition update", catalog: "local"},
		{name: "project override creation", catalog: "inherited", override: true},
		{name: "inherited definition update", catalog: "inherited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			projectPath := filepath.Join(root, "project.db")
			projectStore, err := flowdb.Open(ctx, projectPath)
			if err != nil {
				t.Fatalf("open project database: %v", err)
			}
			t.Cleanup(func() { _ = projectStore.Close() })

			var globals *AgentDefService
			var projectDefs *AgentDefService
			var agent AgentDef
			if tc.catalog == "inherited" {
				globalStore, openErr := flowdb.OpenGlobal(ctx, filepath.Join(root, "global.db"))
				if openErr != nil {
					t.Fatalf("open global database: %v", openErr)
				}
				t.Cleanup(func() { _ = globalStore.Close() })
				globals = NewGlobalAgentDefService(globalStore.DB())
				agent, err = globals.Create(ctx, AgentDefInput{Name: "schedule-author", Harness: "harness", Model: "old-model", Prompt: "old prompt"})
				projectDefs = NewInheritedAgentDefService(projectStore.DB(), globals)
			} else {
				projectDefs = NewAgentDefService(projectStore.DB())
				agent, err = projectDefs.Create(ctx, AgentDefInput{Name: "schedule-author", Harness: "harness", Model: "old-model", Prompt: "old prompt"})
			}
			if err != nil {
				t.Fatalf("create agent definition: %v", err)
			}

			flows := NewFlowServiceWithAgentDefs(projectStore.DB(), projectDefs)
			tasks := NewTaskService(projectStore.DB(), "p-test")
			runs := NewWorkflowRunService(projectStore.DB(), flows, tasks)
			flow, err := flows.Create(ctx, FlowInput{
				Name: "agent revision race", StartNode: "implement",
				Nodes: []FlowNodeInput{
					{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
					{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
				},
				Edges: []FlowEdgeInput{{From: "implement", Outcome: "completed", To: "done"}},
			})
			if err != nil {
				t.Fatalf("create flow: %v", err)
			}
			task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Schedule current agent", FlowID: flow.ID})
			if err != nil {
				t.Fatalf("create task: %v", err)
			}

			resolved := make(chan FlowSnapshot, 1)
			resumeScheduling := make(chan struct{})
			var resolutionCount atomic.Int32
			runs.scheduleSnapshotResolvedTestHook = func(snapshot FlowSnapshot) {
				if resolutionCount.Add(1) == 1 {
					resolved <- snapshot
					<-resumeScheduling
				}
			}
			type scheduleResult struct {
				run WorkflowRun
				err error
			}
			scheduled := make(chan scheduleResult, 1)
			go func() {
				run, scheduleErr := runs.Schedule(ctx, task.ID)
				scheduled <- scheduleResult{run: run, err: scheduleErr}
			}()

			var stale FlowSnapshot
			select {
			case stale = <-resolved:
			case result := <-scheduled:
				t.Fatalf("scheduling finished before agent update: run=%+v err=%v", result.run, result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for initial snapshot resolution")
			}

			updatedInput := AgentDefInput{Name: agent.Name, Harness: "harness", Model: "new-model", Prompt: "new prompt"}
			if tc.catalog == "local" || tc.override {
				_, err = projectDefs.Update(ctx, agent.ID, updatedInput)
			} else {
				_, err = globals.Update(ctx, agent.ID, updatedInput)
			}
			close(resumeScheduling)
			if err != nil {
				t.Fatalf("update agent definition during scheduling: %v", err)
			}

			var result scheduleResult
			select {
			case result = <-scheduled:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for scheduling retry")
			}
			if result.err != nil {
				t.Fatalf("schedule after agent update: %v", result.err)
			}
			if resolutionCount.Load() < 2 {
				t.Fatalf("snapshot resolutions = %d, want retry after agent definition changed", resolutionCount.Load())
			}
			if result.run.Snapshot.AgentDefsRevision < stale.AgentDefsRevision || result.run.Snapshot.InheritedAgentDefsRevision < stale.InheritedAgentDefsRevision {
				t.Fatalf("committed catalog revisions = local %d inherited %d, stale = local %d inherited %d",
					result.run.Snapshot.AgentDefsRevision, result.run.Snapshot.InheritedAgentDefsRevision,
					stale.AgentDefsRevision, stale.InheritedAgentDefsRevision)
			}
			if result.run.Snapshot.AgentDefsRevision == stale.AgentDefsRevision && result.run.Snapshot.InheritedAgentDefsRevision == stale.InheritedAgentDefsRevision {
				t.Fatal("committed snapshot retained both stale agent catalog revisions")
			}
			node, ok := result.run.Snapshot.Node("implement")
			if !ok || node.Config.Agent == nil {
				t.Fatalf("committed implement node = %+v, ok=%t", node, ok)
			}
			if node.Config.Agent.Agent.Model != "new-model" || node.Config.Agent.Agent.Prompt != "new prompt" {
				t.Fatalf("committed agent = %+v, want updated model and prompt", node.Config.Agent.Agent)
			}
			var runCount int
			if err := runs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE task_id = ?`, task.ID).Scan(&runCount); err != nil {
				t.Fatalf("count committed runs: %v", err)
			}
			if runCount != 1 {
				t.Fatalf("committed runs = %d, want exactly one current snapshot", runCount)
			}
		})
	}
}

func TestWorkflowScheduleLocksProjectBeforeInheritedCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(root, "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	projectPath := filepath.Join(root, "project.db")
	projectStore, err := flowdb.Open(ctx, projectPath)
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	globals := NewGlobalAgentDefService(globalStore.DB())
	agent, err := globals.Create(ctx, AgentDefInput{Name: "lock-order-author", Harness: "harness", Prompt: "old prompt"})
	if err != nil {
		t.Fatalf("create global agent definition: %v", err)
	}
	projectDefs := NewInheritedAgentDefService(projectStore.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(projectStore.DB(), projectDefs)
	flow, err := flows.Create(ctx, FlowInput{
		Name: "lock order flow", StartNode: "implement",
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "implement", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	tasks := NewTaskService(projectStore.DB(), "p-test")
	runs := NewWorkflowRunService(projectStore.DB(), flows, tasks)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Schedule without lock inversion", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	lockDB, err := sql.Open("sqlite3", projectPath)
	if err != nil {
		t.Fatalf("open independent project lock connection: %v", err)
	}
	t.Cleanup(func() { _ = lockDB.Close() })
	projectLock, err := sqlitex.BeginImmediate(ctx, lockDB)
	if err != nil {
		t.Fatalf("hold project writer lock: %v", err)
	}
	defer projectLock.Rollback()
	beforeProjectLock := make(chan struct{})
	runs.scheduleBeforeProjectLockTestHook = func() {
		runs.scheduleBeforeProjectLockTestHook = nil
		close(beforeProjectLock)
	}
	type scheduleResult struct {
		run WorkflowRun
		err error
	}
	scheduled := make(chan scheduleResult, 1)
	go func() {
		run, scheduleErr := runs.Schedule(ctx, task.ID)
		scheduled <- scheduleResult{run: run, err: scheduleErr}
	}()
	select {
	case <-beforeProjectLock:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for scheduling to attempt the project lock")
	}

	globalUpdated := make(chan error, 1)
	go func() {
		_, updateErr := globals.Update(ctx, agent.ID, AgentDefInput{Name: agent.Name, Harness: "harness", Prompt: "new prompt"})
		globalUpdated <- updateErr
	}()
	select {
	case updateErr := <-globalUpdated:
		if updateErr != nil {
			t.Fatalf("update inherited definition while scheduling waits for project: %v", updateErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inherited catalog was locked before scheduling acquired the project lock")
	}
	projectLock.Rollback()

	select {
	case result := <-scheduled:
		if result.err != nil {
			t.Fatalf("schedule after inherited update: %v", result.err)
		}
		node, ok := result.run.Snapshot.Node("implement")
		if !ok || node.Config.Agent == nil || node.Config.Agent.Agent.Prompt != "new prompt" {
			t.Fatalf("committed agent after retry = %+v, ok=%t; want new prompt", node.Config.Agent, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduling did not finish after releasing the project lock")
	}
}

func TestEditTaskOmittingReviewPolicyPreservesConcurrentScheduledOptIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "stale edit review race",
		StartNode: "review",
		Nodes: []FlowNodeInput{
			{Key: "review", Name: "Human review", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{
				Outcomes: []string{"approved"}, TaskOptIn: true, SkipOutcome: "approved",
			}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "review", Outcome: "approved", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create optional review flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Original title", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	readComplete := make(chan struct{})
	continueEdit := make(chan struct{})
	tasks.editTaskAfterReadTestHook = func() {
		// Disable the one-shot hook before allowing the concurrent operations to
		// proceed through their own EditTask calls.
		tasks.editTaskAfterReadTestHook = nil
		close(readComplete)
		<-continueEdit
	}
	type editResult struct {
		task Task
		err  error
	}
	result := make(chan editResult, 1)
	updatedTitle := "Updated title"
	go func() {
		edited, editErr := tasks.EditTask(ctx, task.ID, EditTaskInput{Title: &updatedTitle})
		result <- editResult{task: edited, err: editErr}
	}()
	<-readComplete

	required := true
	_, optInErr := tasks.EditTask(ctx, task.ID, EditTaskInput{RequiresHumanReview: &required})
	var run WorkflowRun
	var scheduleErr error
	if optInErr == nil {
		run, scheduleErr = runs.Schedule(ctx, task.ID)
	}
	close(continueEdit)
	staleEdit := <-result

	if optInErr != nil {
		t.Fatalf("opt in while stale edit is paused: %v", optInErr)
	}
	if scheduleErr != nil {
		t.Fatalf("schedule opted-in task: %v", scheduleErr)
	}
	if staleEdit.err != nil {
		t.Fatalf("complete stale title edit: %v", staleEdit.err)
	}
	if staleEdit.task.Title != updatedTitle || !staleEdit.task.RequiresHumanReview || staleEdit.task.State == nil || *staleEdit.task.State != LifecycleScheduled {
		t.Fatalf("task after stale edit = title %q review %t state %v, want updated/true/scheduled", staleEdit.task.Title, staleEdit.task.RequiresHumanReview, staleEdit.task.State)
	}
	if node, ok := run.Snapshot.Node("review"); !ok || node.Kind != NodeHumanGate {
		t.Fatalf("scheduled snapshot review node = %+v, found=%t", node, ok)
	}
}

func TestFlowCreateAndUpdateRejectTaskOptInHumanGateSkipCycles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, _, _ := newWorkflowModelServices(t)

	invalidFlows := []struct {
		name  string
		input func(string) FlowInput
	}{
		{
			name: "self cycle",
			input: func(name string) FlowInput {
				return FlowInput{
					Name: name, StartNode: "review",
					Nodes: []FlowNodeInput{
						{Key: "review", Name: "Review", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"skip", "finish"}, TaskOptIn: true, SkipOutcome: "skip"}}},
						{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
					},
					Edges: []FlowEdgeInput{
						{From: "review", Outcome: "skip", To: "review"},
						{From: "review", Outcome: "finish", To: "done"},
					},
				}
			},
		},
		{
			name: "multiple gate cycle",
			input: func(name string) FlowInput {
				return FlowInput{
					Name: name, StartNode: "review-a",
					Nodes: []FlowNodeInput{
						{Key: "review-a", Name: "Review A", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"skip", "finish"}, TaskOptIn: true, SkipOutcome: "skip"}}},
						{Key: "review-b", Name: "Review B", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"skip", "finish"}, TaskOptIn: true, SkipOutcome: "skip"}}},
						{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
					},
					Edges: []FlowEdgeInput{
						{From: "review-a", Outcome: "skip", To: "review-b"},
						{From: "review-a", Outcome: "finish", To: "done"},
						{From: "review-b", Outcome: "skip", To: "review-a"},
						{From: "review-b", Outcome: "finish", To: "done"},
					},
				}
			},
		},
	}

	for _, tc := range invalidFlows {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := flows.Create(ctx, tc.input("create "+tc.name)); err == nil || !strings.Contains(err.Error(), "skip outcomes form a cycle") {
				t.Fatalf("Create error = %v, want task-opt-in skip cycle rejection", err)
			}

			existing, err := flows.Create(ctx, FlowInput{
				Name: "update " + tc.name, StartNode: "done",
				Nodes: []FlowNodeInput{{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}}},
			})
			if err != nil {
				t.Fatalf("create update target: %v", err)
			}
			if _, err := flows.Update(ctx, existing.ID, tc.input(existing.Name)); err == nil || !strings.Contains(err.Error(), "skip outcomes form a cycle") {
				t.Fatalf("Update error = %v, want task-opt-in skip cycle rejection", err)
			}
		})
	}
}

func TestWorkflowScheduleAcceptsAcyclicTaskOptInHumanGateChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name: "optional human review chain", StartNode: "review-a",
		Nodes: []FlowNodeInput{
			{Key: "review-a", Name: "Review A", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved"}, TaskOptIn: true, SkipOutcome: "approved"}}},
			{Key: "review-b", Name: "Review B", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved"}, TaskOptIn: true, SkipOutcome: "approved"}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{
			{From: "review-a", Outcome: "approved", To: "review-b"},
			{From: "review-b", Outcome: "approved", To: "done"},
		},
	})
	if err != nil {
		t.Fatalf("create acyclic optional-review flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Ship through optional reviews", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create default opt-out task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule default opt-out task: %v", err)
	}
	if run.Snapshot.StartNode != "done" || len(run.Snapshot.Nodes) != 1 || run.Snapshot.Nodes[0].Key != "done" {
		t.Fatalf("projected snapshot = start %q nodes %+v, want both optional gates bypassed", run.Snapshot.StartNode, run.Snapshot.Nodes)
	}
}

func TestWorkflowScheduleIncludesTaskOptInHumanReviewOnlyWhenRequired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "optional human review",
		StartNode: "review",
		Nodes: []FlowNodeInput{
			{Key: "review", Name: "Human review", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{
				Outcomes: []string{"approved", "rejected"}, TaskOptIn: true, SkipOutcome: "approved",
			}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionRejected}}},
		},
		Edges: []FlowEdgeInput{
			{From: "review", Outcome: "approved", To: "done"},
			{From: "review", Outcome: "rejected", To: "rejected"},
		},
	})
	if err != nil {
		t.Fatalf("create optional-review flow: %v", err)
	}

	withoutReview, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Ship automatically", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create default task: %v", err)
	}
	withoutRun, err := runs.Schedule(ctx, withoutReview.ID)
	if err != nil {
		t.Fatalf("schedule default task: %v", err)
	}
	if withoutRun.Snapshot.StartNode != "done" || len(withoutRun.Snapshot.Nodes) != 1 {
		t.Fatalf("default snapshot = start %q nodes %+v, want optional gate and rejected branch removed", withoutRun.Snapshot.StartNode, withoutRun.Snapshot.Nodes)
	}

	required := true
	withReview, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Wait for a person", FlowID: flow.ID, RequiresHumanReview: &required})
	if err != nil {
		t.Fatalf("create opted-in task: %v", err)
	}
	withRun, err := runs.Schedule(ctx, withReview.ID)
	if err != nil {
		t.Fatalf("schedule opted-in task: %v", err)
	}
	if withRun.Snapshot.StartNode != "review" || len(withRun.Snapshot.Nodes) != 3 {
		t.Fatalf("opted-in snapshot = start %q nodes %+v, want human gate preserved", withRun.Snapshot.StartNode, withRun.Snapshot.Nodes)
	}
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, withRun.ID)
	if err != nil {
		t.Fatalf("enter opted-in human gate: %v", err)
	}
	if !created || nodeRun.State != WorkflowNodeWaiting {
		t.Fatalf("opted-in gate node = %+v created=%t, want waiting", nodeRun, created)
	}
}

func TestWorkflowModelSchedulingWaitsForTaskDependencies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "finish",
		StartNode: "done",
		Nodes: []FlowNodeInput{
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	blocker, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Blocker", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Blocked", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	if err := tasks.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	run, err := runs.Schedule(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("schedule blocked task: %v", err)
	}
	if node, created, err := runs.EnsureCurrentNode(ctx, run.ID); err != nil || created || node.ID != "" {
		t.Fatalf("dependency-gated node = %+v created=%t err=%v", node, created, err)
	}
	if _, err := runs.ForceDone(ctx, blocker.ID, ResolutionCompleted, "dependency satisfied", ActorHuman); err != nil {
		t.Fatalf("complete blocker: %v", err)
	}
	if _, completed, err := runs.EnsureCurrentNode(ctx, run.ID); err != nil || !completed {
		t.Fatalf("resume after dependency: completed=%t err=%v", completed, err)
	}
}

func TestWorkflowModelAgentInputWaitResumesSameNodeVisit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agent, err := NewAgentDefService(flows.db).Create(ctx, AgentDefInput{
		Name: "implementation agent", Harness: "harness", Prompt: "Implement the task.",
	})
	if err != nil {
		t.Fatalf("create agent definition: %v", err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "agent with questions",
		StartNode: "implement",
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agent.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "implement", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Needs a decision", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	nodeRun, created, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil || !created {
		t.Fatalf("create agent node: created=%t err=%v", created, err)
	}
	if _, err := runs.MarkNodeRunning(ctx, nodeRun.ID); err != nil {
		t.Fatalf("start agent node: %v", err)
	}
	if err := runs.RequestAgentInput(ctx, nodeRun.ID, "Which API shape should I use?", ActorAgent); err != nil {
		t.Fatalf("request agent input: %v", err)
	}
	state, wait, err := runs.Substate(ctx, task.ID)
	if err != nil {
		t.Fatalf("derive blocked state: %v", err)
	}
	if state != InProgressBlocked || wait == nil || wait.Kind != WorkflowWaitAgentRequest {
		t.Fatalf("blocked state = %q wait=%+v", state, wait)
	}
	resumed, err := runs.ResumeAgentRequest(ctx, task.ID, "Use the v2 shape.", ActorHuman)
	if err != nil || !resumed {
		t.Fatalf("resume agent input: resumed=%t err=%v", resumed, err)
	}
	resumedNode, found, err := runs.GetNodeRun(ctx, nodeRun.ID)
	if err != nil || !found {
		t.Fatalf("load resumed node: found=%t err=%v", found, err)
	}
	if resumedNode.State != WorkflowNodeRunning || resumedNode.Visit != nodeRun.Visit {
		t.Fatalf("resumed node = %+v, want same running visit", resumedNode)
	}
	state, wait, err = runs.Substate(ctx, task.ID)
	if err != nil || state != InProgressWorking || wait != nil {
		t.Fatalf("working state = %q wait=%+v err=%v", state, wait, err)
	}
}
