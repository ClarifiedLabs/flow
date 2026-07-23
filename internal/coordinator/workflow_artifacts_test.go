package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

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
	flows := NewFlowServiceWithAgentDefs(store.DB(), NewInheritedAgentDefService(store.DB(), globals))
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
		Tasks: []TaskSetItem{{
			Key:      "generated-task",
			Title:    "Generated task",
			Body:     generatedBody,
			Priority: 7,
			TagSlugs: []string{"generated"},
			FlowID:   coding.ID,
		}},
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
		SummaryMarkdown: "Generated one implementation task.",
		Payload:         payload,
		ClientKey:       "task-set-1",
	})
	if err != nil {
		t.Fatalf("create task-set artifact: %v", err)
	}
	if replayed {
		t.Fatal("first artifact create was reported as a replay")
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
}
