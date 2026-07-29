package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestTaskSetMaterializerConfigRejectsConflictingConsumers(t *testing.T) {
	snapshot := FlowSnapshot{
		Nodes: []FlowNodeSnapshot{
			{Key: "plan", Kind: NodeAgent, Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{Artifact: ArtifactTaskSet}}},
			{Key: "gate", Kind: NodeHumanGate, Config: FlowNodeSnapshotConfig{HumanGate: &HumanGateNodeConfig{}}},
			{Key: "materialize-a", Kind: NodeMaterializeTaskSet, Config: FlowNodeSnapshotConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: "fl-coding", AllowChildFlowOverride: true, MaxItems: 25}}},
			{Key: "materialize-b", Kind: NodeMaterializeTaskSet, Config: FlowNodeSnapshotConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: "fl-planning", AllowChildFlowOverride: true, MaxItems: 25}}},
		},
		Edges: []FlowEdge{
			{From: "plan", To: "gate"},
			{From: "gate", To: "plan"},
			{From: "gate", To: "materialize-a"},
			{From: "gate", To: "materialize-b"},
		},
	}
	if _, _, err := taskSetMaterializerConfig(snapshot, "plan"); err == nil || !strings.Contains(err.Error(), "conflicting policies") {
		t.Fatalf("taskSetMaterializerConfig() error = %v, want conflicting policies", err)
	}
}

func TestDecodeTaskSetManifestRequiresBody(t *testing.T) {
	const body = "\n## Scope\n\nImplement the body-only task contract.\n\n- Preserve Markdown.\n"
	manifest, err := DecodeTaskSetManifest([]byte(`{
		"schema_version": 1,
		"tasks": [{
			"key": "body-contract",
			"title": "Body contract",
			"body": "\n## Scope\n\nImplement the body-only task contract.\n\n- Preserve Markdown.\n"
		}]
	}`))
	if err != nil {
		t.Fatalf("decode valid manifest: %v", err)
	}
	if len(manifest.Tasks) != 1 || manifest.Tasks[0].Body != body {
		t.Fatalf("decoded tasks = %+v, want preserved body %q", manifest.Tasks, body)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{
			name: "missing",
			raw:  `{"schema_version":1,"tasks":[{"key":"missing-body","title":"Missing body"}]}`,
		},
		{
			name: "whitespace",
			raw:  `{"schema_version":1,"tasks":[{"key":"blank-body","title":"Blank body","body":" \n\t "}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeTaskSetManifest([]byte(test.raw)); err == nil || !strings.Contains(err.Error(), "requires title and body") {
				t.Fatalf("DecodeTaskSetManifest() error = %v, want title/body requirement", err)
			}
		})
	}
}

func TestValidateTaskSetWorkflowSelectionAllowsExplicitDefaultWithoutOverrides(t *testing.T) {
	ctx := context.Background()
	store, _ := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}
	flows := NewFlowServiceWithAgentDefs(store.DB(), NewInheritedAgentDefService(store.DB(), globals))
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed flows: %v", err)
	}
	coding, _ := flows.GetByName(ctx, "coding")
	planning, _ := flows.GetByName(ctx, "planning")

	manifest := TaskSetManifest{Tasks: []TaskSetItem{{Key: "explicit-default", FlowID: coding.ID}}}
	config := MaterializeTaskSetNodeConfig{DefaultChildFlowID: coding.ID, AllowChildFlowOverride: false, MaxItems: 25}
	if err := validateTaskSetWorkflowSelectionTx(ctx, store.DB(), manifest, config); err != nil {
		t.Fatalf("validate explicit default: %v", err)
	}
	manifest.Tasks[0] = TaskSetItem{Key: "planning-override", FlowID: planning.ID}
	if err := validateTaskSetWorkflowSelectionTx(ctx, store.DB(), manifest, config); err == nil || !strings.Contains(err.Error(), "may not override default child flow") {
		t.Fatalf("validate disabled planning override error = %v, want override rejection", err)
	}
	config.DefaultChildFlowID = planning.ID
	manifest.Tasks[0] = TaskSetItem{Key: "planning-default"}
	if err := validateTaskSetWorkflowSelectionTx(ctx, store.DB(), manifest, config); err != nil {
		t.Fatalf("validate planning default: %v", err)
	}
}

func TestMaterializeTaskSetStoresBodyAndTaskMetadata(t *testing.T) {
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}
	defs := NewInheritedAgentDefService(store.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(store.DB(), defs)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed flows: %v", err)
	}
	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("get coding flow: %v", err)
	}
	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("get planning flow: %v", err)
	}
	planner, err := defs.GetByName(ctx, "task-planner")
	if err != nil {
		t.Fatalf("get task planner: %v", err)
	}
	specialized, err := flows.Create(ctx, FlowInput{
		Name: "specialized", StartNode: "work",
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Specialized work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: planner.ID, Workspace: WorkspaceBase, Artifact: ArtifactHandoff}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create specialized flow: %v", err)
	}
	planningSnapshot, err := flows.ResolveSnapshot(ctx, planning.ID)
	if err != nil {
		t.Fatalf("resolve planning snapshot: %v", err)
	}
	snapshotJSON, err := json.Marshal(planningSnapshot)
	if err != nil {
		t.Fatalf("marshal planning snapshot: %v", err)
	}

	source, err := tasks.CreateTask(ctx, CreateTaskInput{
		Title:  "Plan implementation tasks",
		Body:   "Produce a body-only task set.",
		FlowID: planning.ID,
	})
	if err != nil {
		t.Fatalf("create source task: %v", err)
	}
	const runID = "wr-task-set-test"
	const nodeRunID = "wnr-task-set-test"
	const createdAt = "2026-01-01T00:00:00.000Z"
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_id, flow_snapshot_json, state,
	current_node_key, current_node_run_id, transition_budget, created_at, started_at
) VALUES (?, ?, 1, ?, ?, ?, 'write-plan', ?, ?, ?, ?)`,
		runID, source.ID, planning.ID, string(snapshotJSON), string(WorkflowRunRunning),
		nodeRunID, planning.TransitionBudget, createdAt, createdAt); err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (
	id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at
) VALUES (?, ?, 'write-plan', 1, 1, ?, ?, ?)`,
		nodeRunID, runID, string(WorkflowNodeRunning), createdAt, createdAt); err != nil {
		t.Fatalf("insert workflow node run: %v", err)
	}

	const generatedBody = "\n## Scope and requirements\n\nImplement the generated task.\n\n- Keep this Markdown unchanged.\n"
	payload, err := json.Marshal(TaskSetManifest{
		SchemaVersion: 1,
		Tasks: []TaskSetItem{
			{
				Key:      "generated-task",
				Title:    "Generated task",
				Body:     generatedBody,
				Priority: 7,
				TagSlugs: []string{"generated"},
			},
			{
				Key:    "narrower-plan",
				Title:  "Plan the unresolved architecture",
				Body:   "Decide the unresolved architecture and produce a narrower reviewed task graph.",
				FlowID: planning.ID,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal task-set payload: %v", err)
	}
	artifacts := NewWorkflowArtifactService(store.DB(), tasks)
	artifact, replayed, err := artifacts.Create(ctx, CreateWorkflowArtifactInput{
		WorkflowRunID:   runID,
		NodeRunID:       nodeRunID,
		CreatorKey:      "test:task-set",
		Kind:            ArtifactTaskSet,
		SummaryMarkdown: "Generated one implementation task and one narrower planning task.",
		Payload:         payload,
		ClientKey:       "task-set-1",
	})
	if err != nil {
		t.Fatalf("create task-set artifact: %v", err)
	}
	if replayed {
		t.Fatal("first artifact create was reported as a replay")
	}

	invalidPayload, err := json.Marshal(TaskSetManifest{
		SchemaVersion: 1,
		Tasks:         []TaskSetItem{{Key: "unknown-flow", Title: "Unknown flow", Body: "This must fail before review.", FlowID: "fl-unknown"}},
	})
	if err != nil {
		t.Fatalf("marshal invalid task-set payload: %v", err)
	}
	if _, _, err := artifacts.Create(ctx, CreateWorkflowArtifactInput{
		WorkflowRunID: runID, NodeRunID: nodeRunID, CreatorKey: "test:task-set", Kind: ArtifactTaskSet,
		SummaryMarkdown: "Invalid workflow selection.", Payload: invalidPayload, ClientKey: "task-set-invalid",
	}); err == nil || !strings.Contains(err.Error(), `task "unknown-flow" flow: flow "fl-unknown" does not exist`) {
		t.Fatalf("create invalid task-set artifact error = %v, want pre-review unknown-flow rejection", err)
	}
	stored, err := artifacts.ListForRun(ctx, runID)
	if err != nil {
		t.Fatalf("list artifacts after invalid submission: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != artifact.ID {
		t.Fatalf("artifacts after invalid submission = %+v, want only valid artifact %s", stored, artifact.ID)
	}

	// Go's JSON decoder accepts case-variant field names. The stored payload must
	// be normalized so deletion guards inspect the same selected workflow that
	// validation saw.
	specializedPayload := []byte(fmt.Sprintf(`{
		"schema_version": 1,
		"TASKS": [{
			"key": "specialized",
			"title": "Specialized work",
			"body": "Run the specialized workflow.",
			"FLOW_ID": %q
		}]
	}`, specialized.ID))
	if _, _, err := artifacts.Create(ctx, CreateWorkflowArtifactInput{
		WorkflowRunID: runID, NodeRunID: nodeRunID, CreatorKey: "test:task-set", Kind: ArtifactTaskSet,
		SummaryMarkdown: "Specialized follow-on work.", Payload: specializedPayload, ClientKey: "task-set-specialized",
	}); err != nil {
		t.Fatalf("create specialized task-set artifact: %v", err)
	}
	if err := flows.Delete(ctx, specialized.ID); !errors.Is(err, ErrFlowInUse) {
		t.Fatalf("delete artifact-selected flow error = %v, want ErrFlowInUse", err)
	}

	concurrent, err := flows.Create(ctx, FlowInput{
		Name: "concurrent-specialized", StartNode: "work",
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Concurrent specialized work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: planner.ID, Workspace: WorkspaceBase, Artifact: ArtifactHandoff}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create concurrent specialized flow: %v", err)
	}
	concurrentPayload, err := json.Marshal(TaskSetManifest{
		SchemaVersion: 1,
		Tasks:         []TaskSetItem{{Key: "concurrent", Title: "Concurrent work", Body: "Run concurrently selected work.", FlowID: concurrent.ID}},
	})
	if err != nil {
		t.Fatalf("marshal concurrent task set: %v", err)
	}
	start := make(chan struct{})
	var createErr, deleteErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, _, createErr = artifacts.Create(ctx, CreateWorkflowArtifactInput{
			WorkflowRunID: runID, NodeRunID: nodeRunID, CreatorKey: "test:task-set", Kind: ArtifactTaskSet,
			SummaryMarkdown: "Concurrently selected work.", Payload: concurrentPayload, ClientKey: "task-set-concurrent",
		})
	}()
	go func() {
		defer wait.Done()
		<-start
		deleteErr = flows.Delete(ctx, concurrent.ID)
	}()
	close(start)
	wait.Wait()
	if (createErr == nil) == (deleteErr == nil) {
		t.Fatalf("concurrent artifact create error = %v, flow delete error = %v; want exactly one success", createErr, deleteErr)
	}
	if createErr == nil && !errors.Is(deleteErr, ErrFlowInUse) {
		t.Fatalf("delete after concurrent artifact commit error = %v, want ErrFlowInUse", deleteErr)
	}
	if deleteErr == nil && !strings.Contains(createErr.Error(), "does not exist") {
		t.Fatalf("artifact after concurrent delete error = %v, want missing flow", createErr)
	}

	result, replayed, err := artifacts.MaterializeTaskSet(ctx, artifact.ID, MaterializeTaskSetNodeConfig{
		DefaultChildFlowID:     coding.ID,
		AllowChildFlowOverride: true,
		MaxItems:               25,
	})
	if err != nil {
		t.Fatalf("materialize task set: %v", err)
	}
	if replayed {
		t.Fatal("first materialization was reported as a replay")
	}
	generatedID := result.TaskIDs["generated-task"]
	if generatedID == "" {
		t.Fatalf("materialization result = %+v, missing generated task", result)
	}
	generated, err := tasks.GetTask(ctx, generatedID)
	if err != nil {
		t.Fatalf("get generated task: %v", err)
	}
	if generated.Title != "Generated task" || generated.Body != generatedBody || generated.Priority != 7 {
		t.Fatalf("generated task text/priority = %+v", generated)
	}
	if generated.FlowID != coding.ID || generated.CreatedBy != ActorSystem {
		t.Fatalf("generated task flow/audit = %+v", generated)
	}
	if generated.SourceTaskID == nil || *generated.SourceTaskID != source.ID {
		t.Fatalf("generated source task = %v, want %s", generated.SourceTaskID, source.ID)
	}
	if generated.State != nil {
		t.Fatalf("generated implementation task state = %v, want unscheduled", *generated.State)
	}

	plannedID := result.TaskIDs["narrower-plan"]
	planned, err := tasks.GetTask(ctx, plannedID)
	if err != nil {
		t.Fatalf("get nested planning task: %v", err)
	}
	if planned.FlowID != planning.ID || planned.State != nil {
		t.Fatalf("nested planning task workflow/state = %+v, want flow %s and unscheduled", planned, planning.ID)
	}
	if planned.SourceTaskID == nil || *planned.SourceTaskID != source.ID {
		t.Fatalf("nested planning source task = %v, want %s", planned.SourceTaskID, source.ID)
	}

	relations, err := tasks.RelationsForTask(ctx, generated.ID)
	if err != nil {
		t.Fatalf("list generated task relations: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceTaskID != source.ID || relations[0].TargetTaskID != generated.ID || relations[0].Kind != RelationParentOf {
		t.Fatalf("generated task relations = %+v", relations)
	}
	tags, err := tasks.TagsForTask(ctx, generated.ID)
	if err != nil {
		t.Fatalf("list generated task tags: %v", err)
	}
	if len(tags) != 1 || tags[0].Slug != "generated" {
		t.Fatalf("generated task tags = %+v", tags)
	}

	if _, replayed, err := artifacts.MaterializeTaskSet(ctx, artifact.ID, MaterializeTaskSetNodeConfig{
		DefaultChildFlowID: coding.ID, MaxItems: 25,
	}); err == nil || replayed || !strings.Contains(err.Error(), `task "narrower-plan" may not override`) {
		t.Fatalf("replay with conflicting policy = replayed:%t err:%v, want policy rejection", replayed, err)
	}
}
