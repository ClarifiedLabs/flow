package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

type retryRuntimeTestEnv struct {
	store       *flowdb.Store
	globalStore *flowdb.Store
	globalPath  string
	globals     *AgentDefService
	defs        *AgentDefService
	tasks       *TaskService
	runs        *WorkflowRunService
	checks      *CheckService
}

func newRetryRuntimeTestEnv(t *testing.T) *retryRuntimeTestEnv {
	t.Helper()
	ctx := context.Background()
	globalPath := filepath.Join(t.TempDir(), "global.db")
	globalStore, err := flowdb.OpenGlobal(ctx, globalPath)
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	globals := NewGlobalAgentDefService(globalStore.DB())
	defs := NewInheritedAgentDefService(store.DB(), globals)
	tasks := NewTaskService(store.DB(), "p-test")
	flows := NewFlowServiceWithAgentDefs(store.DB(), defs)
	return &retryRuntimeTestEnv{
		store: store, globalStore: globalStore, globalPath: globalPath, globals: globals, defs: defs,
		tasks: tasks, runs: NewWorkflowRunService(store.DB(), flows, tasks),
		checks: NewCheckService(store.DB()),
	}
}

type waitingRetryWorkflow struct {
	task      Task
	run       WorkflowRun
	runID     string
	nodeRunID string
}

func (e *retryRuntimeTestEnv) currentAgentRuntimeRevision(t *testing.T) AgentRuntimeRevision {
	t.Helper()
	ctx := context.Background()
	local, err := agentDefCatalogRevision(ctx, e.store.DB())
	if err != nil {
		t.Fatalf("read project agent catalog revision: %v", err)
	}
	inherited, err := agentDefCatalogRevision(ctx, e.globalStore.DB())
	if err != nil {
		t.Fatalf("read inherited agent catalog revision: %v", err)
	}
	return AgentRuntimeRevision{AgentDefsRevision: local, InheritedAgentDefsRevision: inherited}
}

func (e *retryRuntimeTestEnv) pauseWorkflowForRetry(t *testing.T, snapshot FlowSnapshot, currentNodeKey string) waitingRetryWorkflow {
	t.Helper()
	ctx := context.Background()
	task, err := e.tasks.CreateTask(ctx, CreateTaskInput{Title: "Refresh agent runtime"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = 'in_progress' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}

	currentNode, ok := snapshot.Node(currentNodeKey)
	if !ok {
		t.Fatalf("current snapshot node %q not found", currentNodeKey)
	}
	const runID = "wr-runtime-refresh"
	const nodeRunID = "nr-runtime-refresh"
	const priorNodeRunID = "nr-completed-progress"
	const artifactID = "wa-runtime-refresh"
	artifactValue := any(nil)
	if currentNode.Kind == NodeChangeReview || currentNode.Kind == NodeVerifyChange {
		artifactValue = artifactID
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal workflow snapshot: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	current_node_run_id, current_artifact_id, transition_budget, version, created_at, started_at
) VALUES (?, ?, 1, ?, 'running', ?, ?, ?, ?, 7,
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		runID, task.ID, string(snapshotJSON), currentNodeKey, nodeRunID, artifactValue, snapshot.TransitionBudget); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, output_artifact_id,
	outcome, created_at, started_at, completed_at
) VALUES (?, ?, 'completed-progress', 1, 1, 'succeeded', ?, 'completed',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		priorNodeRunID, runID, artifactValue); err != nil {
		t.Fatalf("insert completed node run: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, input_artifact_id,
	created_at, started_at
) VALUES (?, ?, ?, 1, 2, 'running', ?,
	'2026-01-01T00:00:01Z', '2026-01-01T00:00:01Z')`,
		nodeRunID, runID, currentNodeKey, artifactValue); err != nil {
		t.Fatalf("insert current node run: %v", err)
	}
	if artifactValue != nil {
		const changeID = "ch-runtime-refresh"
		if _, err := e.store.DB().ExecContext(ctx, `
INSERT INTO changes (
	id, task_id, workflow_run_id, branch, base, head_sha, ready_at, created_at, updated_at
) VALUES (?, ?, ?, 'task/runtime-refresh', 'main', 'head-runtime-refresh',
	'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			changeID, task.ID, runID); err != nil {
			t.Fatalf("insert change: %v", err)
		}
		if _, err := e.store.DB().ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-author', 'change', 'Implemented', ?, 'digest', 'artifact-1',
	'2026-01-01T00:00:00Z')`,
			artifactID, runID, priorNodeRunID,
			fmt.Sprintf(`{"change_id":%q,"head_sha":"head-runtime-refresh"}`, changeID)); err != nil {
			t.Fatalf("insert workflow artifact: %v", err)
		}
	}
	if err := e.runs.PauseForExecutionError(ctx, runID, nodeRunID, WorkflowExecutionFailure{
		Operation: "launch agent", NodeKind: currentNode.Kind, Attempt: 2, Message: "agent process failed",
	}); err != nil {
		t.Fatalf("pause workflow for execution failure: %v", err)
	}
	run, err := e.runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("load paused workflow: %v", err)
	}
	return waitingRetryWorkflow{task: task, run: run, runID: runID, nodeRunID: nodeRunID}
}

func retryTestSnapshot(current FlowNodeSnapshot) FlowSnapshot {
	edges := []FlowEdge{{From: "completed-progress", Outcome: "completed", To: current.Key}}
	switch current.Kind {
	case NodeAgent:
		edges = append(edges, FlowEdge{From: current.Key, Outcome: "completed", To: "done"})
	case NodeChangeReview:
		edges = append(edges,
			FlowEdge{From: current.Key, Outcome: "approved", To: "done"},
			FlowEdge{From: current.Key, Outcome: "changes_requested", To: "completed-progress"},
		)
	case NodeVerifyChange:
		edges = append(edges,
			FlowEdge{From: current.Key, Outcome: "passed", To: "done"},
			FlowEdge{From: current.Key, Outcome: "changes_requested", To: "completed-progress"},
		)
	default:
		panic("retry test snapshot has unsupported current node kind " + string(current.Kind))
	}
	return FlowSnapshot{
		FlowID: "fl-runtime-refresh", FlowRevision: 3, AgentDefsRevision: 7, InheritedAgentDefsRevision: 11,
		FlowName: "Frozen workflow", StartNode: "completed-progress", TransitionBudget: 17,
		Nodes: []FlowNodeSnapshot{
			{
				Key: "completed-progress", Name: "Completed progress", Kind: NodeAgent,
				Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
					Agent:     AgentDefSnapshot{ID: "ad-completed", Name: "completed", Harness: "harness", Model: "frozen-completed", Prompt: "Completed frozen prompt."},
					Workspace: WorkspaceChange, Artifact: ArtifactChange,
				}},
			},
			current,
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeSnapshotConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: edges,
	}
}

func TestRetryExecutionRefreshesAuthorRuntimeFromProjectOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRetryRuntimeTestEnv(t)
	globalAuthor, err := env.globals.Create(ctx, AgentDefInput{
		Name: "shared-author", Harness: "harness", Model: "global-model", ReasoningEffort: "medium", Prompt: "Live global prompt.",
	})
	if err != nil {
		t.Fatalf("create global author: %v", err)
	}
	frozenAgent := AgentDefSnapshot{
		ID: globalAuthor.ID, Name: "Frozen author name", Harness: "harness", Model: "frozen-model",
		ReasoningEffort: "low", Prompt: "Frozen author prompt.",
	}
	snapshot := retryTestSnapshot(FlowNodeSnapshot{
		Key: "author", Name: "Author frozen name", Kind: NodeAgent,
		Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
			Agent: frozenAgent, Workspace: WorkspaceChange, Artifact: ArtifactChange,
		}},
	})
	waiting := env.pauseWorkflowForRetry(t, snapshot, "author")
	projectOverride, err := env.defs.Create(ctx, AgentDefInput{
		Name: globalAuthor.Name, Harness: "harness", Model: "project-model", ReasoningEffort: "high", Prompt: "New project prompt must not leak.",
	})
	if err != nil {
		t.Fatalf("create project override: %v", err)
	}

	expectedSnapshot := snapshot
	expectedRevision := env.currentAgentRuntimeRevision(t)
	expectedSnapshot.Nodes[1].Config.Agent.Agent.Harness = projectOverride.Harness
	expectedSnapshot.Nodes[1].Config.Agent.Agent.Model = projectOverride.Model
	expectedSnapshot.Nodes[1].Config.Agent.Agent.ReasoningEffort = projectOverride.ReasoningEffort
	expectedSnapshot.Nodes[1].Config.Agent.Agent.RuntimeRevision = &expectedRevision
	retried, err := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true)
	if err != nil {
		t.Fatalf("retry with runtime refresh: %v", err)
	}
	if retried.State != WorkflowRunRunning || retried.Version != waiting.run.Version+1 {
		t.Fatalf("retried workflow = state %q version %d, want running version %d", retried.State, retried.Version, waiting.run.Version+1)
	}
	if !reflect.DeepEqual(retried.Snapshot, expectedSnapshot) {
		t.Fatalf("refreshed snapshot = %#v, want %#v", retried.Snapshot, expectedSnapshot)
	}
	refreshed := retried.Snapshot.Nodes[1].Config.Agent.Agent
	if refreshed.ID != globalAuthor.ID || refreshed.ID == projectOverride.ID {
		t.Fatalf("refreshed snapshot id = %q, want stable global id %q (override id %q)", refreshed.ID, globalAuthor.ID, projectOverride.ID)
	}
	if refreshed.Name != frozenAgent.Name || refreshed.Prompt != frozenAgent.Prompt {
		t.Fatalf("refreshed frozen fields = name %q prompt %q", refreshed.Name, refreshed.Prompt)
	}

	payload := retryTransitionPayload(t, env.runs, waiting.runID)
	if len(payload.RefreshedAgents) != 1 {
		t.Fatalf("refreshed transition agents = %+v, want one", payload.RefreshedAgents)
	}
	wantRefresh := WorkflowAgentRuntimeRefresh{
		AgentID: globalAuthor.ID,
		Old:     workflowAgentRuntimeSettings(frozenAgent),
		New: WorkflowAgentRuntimeSettings{
			Harness: projectOverride.Harness, Model: projectOverride.Model, ReasoningEffort: projectOverride.ReasoningEffort,
		},
		RuntimeRevision: &expectedRevision,
	}
	if !reflect.DeepEqual(payload.RefreshedAgents[0], wantRefresh) {
		t.Fatalf("runtime refresh transition = %+v, want %+v", payload.RefreshedAgents[0], wantRefresh)
	}
}

func TestRetryExecutionAuditRetainsRuntimeRevisionAcrossRefreshes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRetryRuntimeTestEnv(t)
	author, err := env.globals.Create(ctx, AgentDefInput{
		Name: "revisioned-author", Harness: "harness", Model: "first-live", ReasoningEffort: "medium", Prompt: "Live prompt.",
	})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	snapshot := retryTestSnapshot(FlowNodeSnapshot{
		Key: "author", Name: "Author", Kind: NodeAgent,
		Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
			Agent: AgentDefSnapshot{
				ID: author.ID, Name: "Frozen author", Harness: "harness", Model: "frozen-model", ReasoningEffort: "low", Prompt: "Frozen prompt.",
			},
			Workspace: WorkspaceChange, Artifact: ArtifactChange,
		}},
	})
	waiting := env.pauseWorkflowForRetry(t, snapshot, "author")
	firstRevision := env.currentAgentRuntimeRevision(t)
	if _, err := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true); err != nil {
		t.Fatalf("first runtime refresh: %v", err)
	}

	if _, err := env.globals.Update(ctx, author.ID, AgentDefInput{
		Name: author.Name, Harness: "harness", Model: "second-live", ReasoningEffort: "high", Prompt: "Changed live prompt.",
	}); err != nil {
		t.Fatalf("update author before second refresh: %v", err)
	}
	secondRevision := env.currentAgentRuntimeRevision(t)
	if secondRevision.InheritedAgentDefsRevision <= firstRevision.InheritedAgentDefsRevision {
		t.Fatalf("second inherited revision = %d, want newer than first %d",
			secondRevision.InheritedAgentDefsRevision, firstRevision.InheritedAgentDefsRevision)
	}
	if err := env.runs.PauseForExecutionError(ctx, waiting.runID, waiting.nodeRunID, WorkflowExecutionFailure{
		Operation: "launch agent", NodeKind: NodeAgent, Message: "agent process failed again",
	}); err != nil {
		t.Fatalf("pause workflow for second refresh: %v", err)
	}
	retried, err := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true)
	if err != nil {
		t.Fatalf("second runtime refresh: %v", err)
	}

	payloads := retryTransitionPayloads(t, env.runs, waiting.runID)
	if len(payloads) != 2 {
		t.Fatalf("retry audit payload count = %d, want 2", len(payloads))
	}
	wantRevisions := []AgentRuntimeRevision{firstRevision, secondRevision}
	wantModels := []string{"first-live", "second-live"}
	for i, payload := range payloads {
		if len(payload.RefreshedAgents) != 1 {
			t.Fatalf("retry audit payload %d refreshes = %+v, want one", i, payload.RefreshedAgents)
		}
		refresh := payload.RefreshedAgents[0]
		if !reflect.DeepEqual(refresh.RuntimeRevision, &wantRevisions[i]) || refresh.New.Model != wantModels[i] {
			t.Fatalf("retry audit payload %d = %+v, want model %q revision %+v", i, refresh, wantModels[i], wantRevisions[i])
		}
	}
	refreshed := retried.Snapshot.Nodes[1].Config.Agent.Agent
	if refreshed.RuntimeRevision == nil || !reflect.DeepEqual(*refreshed.RuntimeRevision, secondRevision) || refreshed.Model != "second-live" {
		t.Fatalf("latest persisted runtime = %+v, want second model and revision %+v", refreshed, secondRevision)
	}
}

func TestRetryExecutionRefreshesReviewAndVerificationAgentsAndOnlyErroredChecks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind NodeKind
	}{
		{name: "review", kind: NodeChangeReview},
		{name: "verification", kind: NodeVerifyChange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			env := newRetryRuntimeTestEnv(t)
			first, err := env.globals.Create(ctx, AgentDefInput{Name: tc.name + "-first", Harness: "harness", Model: "first-live-old", ReasoningEffort: "medium", Prompt: "Live first prompt."})
			if err != nil {
				t.Fatalf("create first agent: %v", err)
			}
			second, err := env.globals.Create(ctx, AgentDefInput{Name: tc.name + "-second", Harness: "harness", Model: "second-live-old", ReasoningEffort: "low", Prompt: "Live second prompt."})
			if err != nil {
				t.Fatalf("create second agent: %v", err)
			}
			var aggregator AgentDef
			if tc.kind == NodeChangeReview {
				aggregator, err = env.globals.Create(ctx, AgentDefInput{Name: "review-aggregator", Harness: "harness", Model: "aggregator-live-old", ReasoningEffort: "medium", Prompt: "Live aggregator prompt."})
				if err != nil {
					t.Fatalf("create aggregator agent: %v", err)
				}
			}
			frozenAgents := []SnapshotReviewAgent{
				{Blocking: true, Agent: AgentDefSnapshot{ID: first.ID, Name: "Frozen first", Harness: "harness", Model: "first-frozen", ReasoningEffort: "low", Prompt: "Frozen first prompt."}},
				{Blocking: false, Agent: AgentDefSnapshot{ID: second.ID, Name: "Frozen second", Harness: "harness", Model: "second-frozen", ReasoningEffort: "medium", Prompt: "Frozen second prompt."}},
			}
			current := FlowNodeSnapshot{Key: tc.name, Name: "Frozen " + tc.name, Kind: tc.kind}
			if tc.kind == NodeChangeReview {
				current.Config.ChangeReview = &ChangeReviewNodeSnapshotConfig{
					Agents: frozenAgents,
					Aggregator: AgentDefSnapshot{
						ID: aggregator.ID, Name: "Frozen aggregator", Harness: "harness", Model: "aggregator-frozen", ReasoningEffort: "low", Prompt: "Frozen aggregator prompt.",
					},
				}
			} else {
				current.Config.VerifyChange = &VerifyChangeNodeSnapshotConfig{Agents: frozenAgents}
			}
			snapshot := retryTestSnapshot(current)
			waiting := env.pauseWorkflowForRetry(t, snapshot, tc.name)
			required := true
			checkKind := CheckKindReviewer
			if tc.kind == NodeVerifyChange {
				checkKind = CheckKindVerifier
			}
			satisfied, err := env.checks.ReportCheck(ctx, ReportCheckInput{
				TaskID: waiting.task.ID, Name: "first.node." + waiting.nodeRunID, Kind: checkKind,
				Required: &required, Verdict: CheckSatisfied, Details: "keep satisfied details", Reporter: "test",
			})
			if err != nil {
				t.Fatalf("report satisfied check: %v", err)
			}
			if _, err := env.checks.ReportCheck(ctx, ReportCheckInput{
				TaskID: waiting.task.ID, Name: "second.node." + waiting.nodeRunID, Kind: checkKind,
				Required: &required, Verdict: CheckErrored, Details: "clear errored details", Reporter: "test",
			}); err != nil {
				t.Fatalf("report errored check: %v", err)
			}
			firstLive, err := env.globals.Update(ctx, first.ID, AgentDefInput{
				Name: first.Name, Harness: "harness", Model: "first-new", ReasoningEffort: "high", Prompt: "Changed first prompt.",
			})
			if err != nil {
				t.Fatalf("update first agent: %v", err)
			}
			secondLive, err := env.globals.Update(ctx, second.ID, AgentDefInput{
				Name: second.Name, Harness: "harness", Model: "second-new", ReasoningEffort: "high", Prompt: "Changed second prompt.",
			})
			if err != nil {
				t.Fatalf("update second agent: %v", err)
			}
			var aggregatorLive AgentDef
			if tc.kind == NodeChangeReview {
				aggregatorLive, err = env.globals.Update(ctx, aggregator.ID, AgentDefInput{
					Name: aggregator.Name, Harness: "harness", Model: "aggregator-new", ReasoningEffort: "high", Prompt: "Changed aggregator prompt.",
				})
				if err != nil {
					t.Fatalf("update aggregator agent: %v", err)
				}
			}

			expectedSnapshot := snapshot
			var expectedAgents []SnapshotReviewAgent
			if tc.kind == NodeChangeReview {
				expectedAgents = expectedSnapshot.Nodes[1].Config.ChangeReview.Agents
			} else {
				expectedAgents = expectedSnapshot.Nodes[1].Config.VerifyChange.Agents
			}
			expectedRevision := env.currentAgentRuntimeRevision(t)
			expectedAgents[0].Agent.Harness = firstLive.Harness
			expectedAgents[0].Agent.Model = firstLive.Model
			expectedAgents[0].Agent.ReasoningEffort = firstLive.ReasoningEffort
			expectedAgents[0].Agent.RuntimeRevision = &expectedRevision
			expectedAgents[1].Agent.Harness = secondLive.Harness
			expectedAgents[1].Agent.Model = secondLive.Model
			expectedAgents[1].Agent.ReasoningEffort = secondLive.ReasoningEffort
			expectedAgents[1].Agent.RuntimeRevision = &expectedRevision
			if tc.kind == NodeChangeReview {
				expectedAggregator := &expectedSnapshot.Nodes[1].Config.ChangeReview.Aggregator
				expectedAggregator.Harness = aggregatorLive.Harness
				expectedAggregator.Model = aggregatorLive.Model
				expectedAggregator.ReasoningEffort = aggregatorLive.ReasoningEffort
				expectedAggregator.RuntimeRevision = &expectedRevision
			}

			retried, err := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true)
			if err != nil {
				t.Fatalf("retry %s with runtime refresh: %v", tc.name, err)
			}
			if !reflect.DeepEqual(retried.Snapshot, expectedSnapshot) {
				t.Fatalf("%s snapshot = %#v, want %#v", tc.name, retried.Snapshot, expectedSnapshot)
			}
			kept, err := env.checks.GetCheck(ctx, waiting.task.ID, satisfied.Name)
			if err != nil {
				t.Fatalf("load satisfied check: %v", err)
			}
			if kept.Verdict != CheckSatisfied || kept.Details != satisfied.Details || kept.Reporter != satisfied.Reporter {
				t.Fatalf("satisfied check changed = %+v, want %+v", kept, satisfied)
			}
			reset, err := env.checks.GetCheck(ctx, waiting.task.ID, "second.node."+waiting.nodeRunID)
			if err != nil {
				t.Fatalf("load reset check: %v", err)
			}
			if reset.Verdict != CheckPending || reset.Details != "" || reset.Reporter != "coordinator" {
				t.Fatalf("errored check after retry = %+v, want pending reset", reset)
			}
			var progressState string
			if err := env.store.DB().QueryRowContext(ctx, `SELECT state FROM workflow_node_runs WHERE id = 'nr-completed-progress'`).Scan(&progressState); err != nil {
				t.Fatalf("load completed progress: %v", err)
			}
			if progressState != string(WorkflowNodeSucceeded) {
				t.Fatalf("completed progress state = %q, want succeeded", progressState)
			}
			payload := retryTransitionPayload(t, env.runs, waiting.runID)
			wantRefreshed := 2
			if tc.kind == NodeChangeReview {
				wantRefreshed = 3
			}
			if payload.ChecksReset != 1 || len(payload.RefreshedAgents) != wantRefreshed {
				t.Fatalf("retry transition payload = %+v, want one reset and %d refreshed agents", payload, wantRefreshed)
			}
		})
	}
}

func TestRetryExecutionRefreshUsesCoherentInheritedCatalogSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRetryRuntimeTestEnv(t)
	first, err := env.globals.Create(ctx, AgentDefInput{
		Name: "coherent-first", Harness: "harness", Model: "first-before", ReasoningEffort: "medium", Prompt: "Live first prompt.",
	})
	if err != nil {
		t.Fatalf("create first agent: %v", err)
	}
	second, err := env.globals.Create(ctx, AgentDefInput{
		Name: "coherent-second", Harness: "harness", Model: "second-before", ReasoningEffort: "medium", Prompt: "Live second prompt.",
	})
	if err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	frozenAgents := []SnapshotReviewAgent{
		{Blocking: true, Agent: AgentDefSnapshot{ID: first.ID, Name: "Frozen first", Harness: "harness", Model: "first-frozen", ReasoningEffort: "low", Prompt: "Frozen first prompt."}},
		{Blocking: true, Agent: AgentDefSnapshot{ID: second.ID, Name: "Frozen second", Harness: "harness", Model: "second-frozen", ReasoningEffort: "low", Prompt: "Frozen second prompt."}},
	}
	snapshot := retryTestSnapshot(FlowNodeSnapshot{
		Key: "verify", Name: "Verify", Kind: NodeVerifyChange,
		Config: FlowNodeSnapshotConfig{VerifyChange: &VerifyChangeNodeSnapshotConfig{Agents: frozenAgents}},
	})
	waiting := env.pauseWorkflowForRetry(t, snapshot, "verify")
	pinnedRevision := env.currentAgentRuntimeRevision(t)
	concurrentDB, err := sql.Open("sqlite3", env.globalPath)
	if err != nil {
		t.Fatalf("open independent inherited catalog connection: %v", err)
	}
	concurrentDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = concurrentDB.Close() })
	if _, err := concurrentDB.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		t.Fatalf("configure independent inherited catalog connection: %v", err)
	}
	concurrentGlobals := NewGlobalAgentDefService(concurrentDB)

	firstResolved := make(chan struct{})
	resumeRefresh := make(chan struct{}, 1)
	defer func() {
		select {
		case resumeRefresh <- struct{}{}:
		default:
		}
	}()
	env.runs.refreshAgentRuntimeResolvedTestHook = func(index int) {
		if index == 0 {
			close(firstResolved)
			<-resumeRefresh
		}
	}
	type retryResult struct {
		run WorkflowRun
		err error
	}
	retried := make(chan retryResult, 1)
	go func() {
		run, retryErr := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true)
		retried <- retryResult{run: run, err: retryErr}
	}()
	select {
	case <-firstResolved:
	case result := <-retried:
		t.Fatalf("retry finished before inherited update: run=%+v err=%v", result.run, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first agent resolution")
	}

	updated := make(chan error, 1)
	go func() {
		_, updateErr := concurrentGlobals.Update(ctx, second.ID, AgentDefInput{
			Name: second.Name, Harness: "harness", Model: "second-after", ReasoningEffort: "high", Prompt: "Changed live second prompt.",
		})
		updated <- updateErr
	}()
	select {
	case updateErr := <-updated:
		if updateErr != nil {
			t.Fatalf("commit inherited agent update between resolutions: %v", updateErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out committing inherited update between agent resolutions")
	}
	resumeRefresh <- struct{}{}

	var result retryResult
	select {
	case result = <-retried:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for retry after inherited update")
	}
	if result.err != nil {
		t.Fatalf("retry after inherited update: %v", result.err)
	}
	if result.run.Snapshot.AgentDefsRevision != snapshot.AgentDefsRevision ||
		result.run.Snapshot.InheritedAgentDefsRevision != snapshot.InheritedAgentDefsRevision {
		t.Fatalf("original snapshot revisions changed = local %d inherited %d, want local %d inherited %d",
			result.run.Snapshot.AgentDefsRevision, result.run.Snapshot.InheritedAgentDefsRevision,
			snapshot.AgentDefsRevision, snapshot.InheritedAgentDefsRevision)
	}
	verify, ok := result.run.Snapshot.Node("verify")
	if !ok || verify.Config.VerifyChange == nil || len(verify.Config.VerifyChange.Agents) != 2 {
		t.Fatalf("persisted verification node = %+v, ok=%t", verify, ok)
	}
	gotAgents := verify.Config.VerifyChange.Agents
	if gotAgents[0].Agent.Model != "first-before" || gotAgents[1].Agent.Model != "second-before" {
		t.Fatalf("persisted runtime models = %q/%q, want coherent pre-update models first-before/second-before",
			gotAgents[0].Agent.Model, gotAgents[1].Agent.Model)
	}
	for i, wantFrozen := range frozenAgents {
		if !reflect.DeepEqual(gotAgents[i].Agent.RuntimeRevision, &pinnedRevision) {
			t.Fatalf("agent %d runtime revision = %+v, want pinned %+v", i, gotAgents[i].Agent.RuntimeRevision, pinnedRevision)
		}
		if gotAgents[i].Agent.ID != wantFrozen.Agent.ID || gotAgents[i].Agent.Name != wantFrozen.Agent.Name || gotAgents[i].Agent.Prompt != wantFrozen.Agent.Prompt {
			t.Fatalf("agent %d non-runtime fields changed = %+v, want id/name/prompt from %+v", i, gotAgents[i].Agent, wantFrozen.Agent)
		}
	}
	currentRevision := env.currentAgentRuntimeRevision(t)
	if currentRevision.InheritedAgentDefsRevision <= pinnedRevision.InheritedAgentDefsRevision {
		t.Fatalf("inherited revision after update = %d, want newer than pinned %d",
			currentRevision.InheritedAgentDefsRevision, pinnedRevision.InheritedAgentDefsRevision)
	}
	payload := retryTransitionPayload(t, env.runs, waiting.runID)
	if len(payload.RefreshedAgents) != 2 || payload.RefreshedAgents[0].New.Model != "first-before" || payload.RefreshedAgents[1].New.Model != "second-before" {
		t.Fatalf("retry audit payload = %+v, want two coherent pre-update refreshes", payload)
	}
	for i, refresh := range payload.RefreshedAgents {
		if !reflect.DeepEqual(refresh.RuntimeRevision, &pinnedRevision) {
			t.Fatalf("audit refresh %d revision = %+v, want pinned %+v", i, refresh.RuntimeRevision, pinnedRevision)
		}
	}
}

func TestRetryExecutionRuntimeRefreshFailureIsAtomic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		invalidAgent AgentDefSnapshot
		wantError    string
	}{
		// A missing frozen ID is rejected while decoding the persisted snapshot;
		// refresh only needs to prove its own lookup failure is atomic.
		{name: "unresolvable snapshot id", invalidAgent: AgentDefSnapshot{ID: "ad-does-not-exist", Name: "Missing definition", Harness: "harness", Prompt: "Frozen."}, wantError: "agent definition not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			env := newRetryRuntimeTestEnv(t)
			valid, err := env.globals.Create(ctx, AgentDefInput{Name: "valid-reviewer", Harness: "harness", Model: "new-valid", ReasoningEffort: "high", Prompt: "Live."})
			if err != nil {
				t.Fatalf("create valid agent: %v", err)
			}
			current := FlowNodeSnapshot{
				Key: "review", Name: "Review", Kind: NodeChangeReview,
				Config: FlowNodeSnapshotConfig{ChangeReview: &ChangeReviewNodeSnapshotConfig{
					Agents: []SnapshotReviewAgent{
						{Blocking: true, Agent: AgentDefSnapshot{ID: valid.ID, Name: "Valid frozen", Harness: "harness", Model: "old-valid", Prompt: "Frozen valid."}},
						{Blocking: true, Agent: tc.invalidAgent},
					},
					Aggregator: AgentDefSnapshot{ID: valid.ID, Name: "Valid aggregator", Harness: "harness", Model: "old-valid", Prompt: "Frozen aggregator."},
				}},
			}
			waiting := env.pauseWorkflowForRetry(t, retryTestSnapshot(current), "review")
			required := true
			check, err := env.checks.ReportCheck(ctx, ReportCheckInput{
				TaskID: waiting.task.ID, Name: "invalid.node." + waiting.nodeRunID, Kind: CheckKindReviewer,
				Required: &required, Verdict: CheckErrored, Details: "must survive rollback", Reporter: "test",
			})
			if err != nil {
				t.Fatalf("report errored check: %v", err)
			}
			before, err := env.runs.Detail(ctx, waiting.runID)
			if err != nil {
				t.Fatalf("load workflow before retry: %v", err)
			}

			_, err = env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, true)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("retry error = %v, want containing %q", err, tc.wantError)
			}
			if tc.invalidAgent.ID != "" && !errors.Is(err, ErrAgentDefNotFound) {
				t.Fatalf("retry error = %v, want ErrAgentDefNotFound", err)
			}
			after, err := env.runs.Detail(ctx, waiting.runID)
			if err != nil {
				t.Fatalf("load workflow after failed retry: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("workflow mutated after failed refresh:\nbefore=%#v\nafter=%#v", before, after)
			}
			afterCheck, err := env.checks.GetCheck(ctx, waiting.task.ID, check.Name)
			if err != nil {
				t.Fatalf("load check after failed retry: %v", err)
			}
			if !reflect.DeepEqual(afterCheck, check) {
				t.Fatalf("check mutated after failed refresh: before=%+v after=%+v", check, afterCheck)
			}
		})
	}
}

func TestRetryExecutionWithoutRefreshKeepsFrozenRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRetryRuntimeTestEnv(t)
	author, err := env.globals.Create(ctx, AgentDefInput{Name: "ordinary-author", Harness: "harness", Model: "live-old", Prompt: "Live old."})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	snapshot := retryTestSnapshot(FlowNodeSnapshot{
		Key: "author", Name: "Author", Kind: NodeAgent,
		Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{
			Agent:     AgentDefSnapshot{ID: author.ID, Name: "Frozen author", Harness: "harness", Model: "frozen-model", ReasoningEffort: "low", Prompt: "Frozen prompt."},
			Workspace: WorkspaceChange, Artifact: ArtifactChange,
		}},
	})
	waiting := env.pauseWorkflowForRetry(t, snapshot, "author")
	if _, err := env.globals.Update(ctx, author.ID, AgentDefInput{
		Name: author.Name, Harness: "harness", Model: "live-new", ReasoningEffort: "high", Prompt: "Live new.",
	}); err != nil {
		t.Fatalf("update live author: %v", err)
	}

	retried, err := env.runs.RetryExecution(ctx, waiting.task.ID, ActorHuman, false)
	if err != nil {
		t.Fatalf("ordinary retry: %v", err)
	}
	if !reflect.DeepEqual(retried.Snapshot, snapshot) {
		t.Fatalf("ordinary retry snapshot = %#v, want unchanged %#v", retried.Snapshot, snapshot)
	}
	payload := retryTransitionPayload(t, env.runs, waiting.runID)
	if payload.RefreshedAgents != nil {
		t.Fatalf("ordinary retry recorded refreshed agents: %+v", payload.RefreshedAgents)
	}
}

type decodedRetryTransitionPayload struct {
	Attempt         int                           `json:"attempt"`
	ChecksReset     int64                         `json:"checks_reset"`
	RefreshedAgents []WorkflowAgentRuntimeRefresh `json:"refreshed_agents"`
}

func retryTransitionPayload(t *testing.T, runs *WorkflowRunService, runID string) decodedRetryTransitionPayload {
	t.Helper()
	payloads := retryTransitionPayloads(t, runs, runID)
	if len(payloads) == 0 {
		t.Fatal("node_retry_requested transition not found")
	}
	return payloads[len(payloads)-1]
}

func retryTransitionPayloads(t *testing.T, runs *WorkflowRunService, runID string) []decodedRetryTransitionPayload {
	t.Helper()
	detail, err := runs.Detail(context.Background(), runID)
	if err != nil {
		t.Fatalf("load workflow detail: %v", err)
	}
	var payloads []decodedRetryTransitionPayload
	for i := range detail.Transitions {
		if detail.Transitions[i].EventKind != "node_retry_requested" {
			continue
		}
		var payload decodedRetryTransitionPayload
		if err := json.Unmarshal(detail.Transitions[i].Payload, &payload); err != nil {
			t.Fatalf("decode retry transition payload: %v", err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}
