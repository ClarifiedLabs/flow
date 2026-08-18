package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowmetrics "github.com/ClarifiedLabs/flow/internal/metrics"
)

func TestDecodeTaskSetManifestReviewFollowUpValidation(t *testing.T) {
	t.Parallel()
	valid := `{
		"schema_version":1,
		"items":[{"key":"existing","existing_task_id":"t-test-0002"}],
		"review_follow_up":{"set_id":" rfus-1 ","set_revision":1,"assignments":[
			{"proposal_id":"rfp-1","disposition":"use_existing_task","target_task_id":"t-test-0002","rationale":" already tracked "}
		]}
	}`
	manifest, err := DecodeTaskSetManifest([]byte(valid))
	if err != nil {
		t.Fatalf("decode existing-only organizer manifest: %v", err)
	}
	if manifest.Items[0].Kind != WorkItemTask || manifest.ReviewFollowUp.SetID != "rfus-1" ||
		manifest.ReviewFollowUp.Assignments[0].Rationale != "already tracked" {
		t.Fatalf("normalized manifest = %+v", manifest)
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate assignment", `{"schema_version":1,"items":[{"key":"new","kind":"task","title":"New","body":"Body"}],"review_follow_up":{"set_id":"s","set_revision":1,"assignments":[{"proposal_id":"p","disposition":"create_task","item_key":"new","rationale":"one"},{"proposal_id":"p","disposition":"covered_by_source","rationale":"two"}]}}`, "duplicate review_follow_up assignment"},
		{"bad shape", `{"schema_version":1,"items":[{"key":"new","kind":"task","title":"New","body":"Body"}],"review_follow_up":{"set_id":"s","set_revision":1,"assignments":[{"proposal_id":"p","disposition":"create_task","item_key":"new","target_task_id":"t-test-0002","rationale":"bad"}]}}`, "invalid create_task assignment shape"},
		{"unknown item", `{"schema_version":1,"items":[{"key":"new","kind":"task","title":"New","body":"Body"}],"review_follow_up":{"set_id":"s","set_revision":1,"assignments":[{"proposal_id":"p","disposition":"create_task","item_key":"missing","rationale":"bad"}]}}`, "unknown item_key"},
		{"bad canonical", `{"schema_version":1,"items":[{"key":"new","kind":"task","title":"New","body":"Body"}],"review_follow_up":{"set_id":"s","set_revision":1,"assignments":[{"proposal_id":"p","disposition":"merge_with_proposal","canonical_proposal_id":"missing","rationale":"bad"}]}}`, "invalid canonical_proposal_id"},
		{"rationale bound", `{"schema_version":1,"items":[{"key":"new","kind":"task","title":"New","body":"Body"}],"review_follow_up":{"set_id":"s","set_revision":1,"assignments":[{"proposal_id":"p","disposition":"covered_by_source","rationale":"` + strings.Repeat("x", reviewFollowUpRationaleMaxBytes+1) + `"}]}}`, "rationale exceeds"},
		{"duplicate dependency", `{"schema_version":1,"items":[{"key":"a","existing_task_id":"t-test-0002"},{"key":"b","existing_task_id":"t-test-0003"}],"dependencies":[{"blocker":"a","blocked":"b"},{"blocker":"a","blocked":"b"}]}`, "duplicate dependency"},
		{"dependency cycle", `{"schema_version":1,"items":[{"key":"a","existing_task_id":"t-test-0002"},{"key":"b","existing_task_id":"t-test-0003"}],"dependencies":[{"blocker":"a","blocked":"b"},{"blocker":"b","blocked":"a"}]}`, "must be acyclic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeTaskSetManifest([]byte(test.raw)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeTaskSetManifest() error = %v, want %q", err, test.want)
			}
		})
	}
}

type organizerArtifactFixture struct {
	ctx                         context.Context
	store                       *flowdb.Store
	tasks                       *TaskService
	artifacts                   *WorkflowArtifactService
	source, organizer, existing Task
	runID, nodeRunID            string
	setID, planID               string
	proposalIDs                 []string
	config                      MaterializeTaskSetNodeConfig
	metrics                     *flowmetrics.Registry
}

func newOrganizerArtifactFixture(t *testing.T) organizerArtifactFixture {
	t.Helper()
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	tasks.SetEventLog(NewEventLogService(store.DB()))
	metricRegistry := flowmetrics.New()
	tasks.SetWorkflowMetrics(flowmetrics.RegisterWorkflow(metricRegistry))
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	flows := NewFlowServiceWithAgentDefs(store.DB(), NewInheritedAgentDefService(store.DB(), globals))
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	planning, err := flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := flows.ResolveSnapshot(ctx, planning.ID)
	if err != nil {
		t.Fatal(err)
	}
	config, found, err := taskSetMaterializerConfig(snapshot, "write-plan")
	if err != nil || !found {
		t.Fatalf("planning materializer config = %+v, %t, %v", config, found, err)
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	source, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Reviewed source", Body: "Original reviewed work."})
	if err != nil {
		t.Fatal(err)
	}
	organizer, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Organize follow-ups", Body: "Consolidate proposals.", FlowID: planning.ID})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Existing open follow-up"})
	if err != nil {
		t.Fatal(err)
	}
	const stamp = "2026-01-01T00:00:00.000Z"
	const sourceRunID = "wr-review-source"
	const sourceNodeID = "wnr-review-source"
	const runID = "wr-organizer"
	const nodeRunID = "wnr-organizer"
	for _, row := range []struct{ run, task, node string }{{sourceRunID, source.ID, sourceNodeID}, {runID, organizer.ID, nodeRunID}} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_runs (id, task_id, run_sequence, flow_id, flow_snapshot_json, state,
 current_node_key, current_node_run_id, transition_budget, created_at, started_at)
VALUES (?, ?, 1, ?, ?, 'running', 'write-plan', ?, ?, ?, ?)`, row.run, row.task, planning.ID, string(snapshotJSON), row.node, snapshot.TransitionBudget, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_node_runs (id, workflow_run_id, node_key, visit, attempt, state, created_at, started_at)
VALUES (?, ?, 'write-plan', 1, 1, 'running', ?, ?)`, row.node, row.run, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	const changeID = "ch-review-source"
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, 'flow/review-source', 'main', 'abc123', ?, ?)`, changeID, source.ID, sourceRunID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO jobs (id, task_id, change_id, workflow_run_id, node_run_id, role, state, capacity_bucket, created_at, updated_at)
VALUES ('j-review-source', ?, ?, ?, ?, 'reviewer', 'finished', 'ephemeral', ?, ?)`, source.ID, changeID, sourceRunID, sourceNodeID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	const setID = "rfus-organizer"
	const planID = "rfupr-organizer"
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO review_follow_up_sets (id, source_task_id, source_change_id, workflow_run_id, revision, state, organizer_task_id, created_at, updated_at)
VALUES (?, ?, ?, ?, 1, 'organizing', ?, ?, ?)`, setID, source.ID, changeID, sourceRunID, organizer.ID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO review_follow_up_batches (id, set_id, source_task_id, source_change_id, workflow_run_id, node_run_id,
 check_name, source_job_id, reviewed_head_sha, report_sha256, report_json, state, created_at, updated_at)
VALUES ('rfub-organizer', ?, ?, ?, ?, ?, 'review.aggregate', 'j-review-source', 'abc123', ?, '{}', 'accepted', ?, ?)`,
		setID, source.ID, changeID, sourceRunID, sourceNodeID, strings.Repeat("a", 64), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	proposalIDs := []string{"rfp-create", "rfp-existing"}
	for index, proposalID := range proposalIDs {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO review_follow_up_proposals (id, batch_id, comment_index, finding_hash, sha, file_path, line, body,
 severity, introduced_by_change, requirement, requirement_source, finding_basis, remediation_scope,
 scope_rationale, follow_up, suggested_action, state, created_at, updated_at)
VALUES (?, 'rfub-organizer', ?, ?, 'abc123', 'follow_up.go', ?, 'finding', 'medium', 0, 'requirement',
 'explicit', 'explicit_requirement', 'local', 'scope', 'follow up', 'create_task', 'active', ?, ?)`,
			proposalID, index, strings.Repeat(string(rune('b'+index)), 64), index+1, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO review_follow_up_plan_revisions (id, set_id, set_revision, organizer_task_id, organizer_workflow_run_id, state, created_at, updated_at)
VALUES (?, ?, 1, ?, ?, 'organizing', ?, ?)`, planID, setID, organizer.ID, runID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return organizerArtifactFixture{ctx: ctx, store: store, tasks: tasks, artifacts: NewWorkflowArtifactService(store.DB(), tasks),
		source: source, organizer: organizer, existing: existing, runID: runID, nodeRunID: nodeRunID,
		setID: setID, planID: planID, proposalIDs: proposalIDs, config: config, metrics: metricRegistry}
}

func (f organizerArtifactFixture) createArtifact(t *testing.T) WorkflowArtifact {
	t.Helper()
	payload, err := json.Marshal(TaskSetManifest{SchemaVersion: 1,
		Items: []TaskSetItem{
			{Key: "created", Kind: WorkItemTask, Title: "Consolidated fix", Body: "Implement the consolidated review follow-up."},
			{Key: "existing", Kind: WorkItemTask, ExistingTaskID: f.existing.ID},
		},
		Dependencies: []TaskSetDependency{{Blocker: "existing", Blocked: "created"}},
		ReviewFollowUp: &TaskSetReviewFollowUp{SetID: f.setID, SetRevision: 1, Assignments: []TaskSetReviewFollowUpAssignment{
			{ProposalID: f.proposalIDs[0], Disposition: ReviewFollowUpDispositionCreateTask, ItemKey: "created", Rationale: "one consolidated task"},
			{ProposalID: f.proposalIDs[1], Disposition: ReviewFollowUpDispositionUseExistingTask, TargetTaskID: f.existing.ID, Rationale: "already tracked"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, replayed, err := f.artifacts.Create(f.ctx, CreateWorkflowArtifactInput{WorkflowRunID: f.runID, NodeRunID: f.nodeRunID,
		CreatorKey: "test:organizer", Kind: ArtifactTaskSet, SummaryMarkdown: "Consolidated follow-up plan.", Payload: payload, ClientKey: "organizer-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("first artifact create replayed")
	}
	return artifact
}

func TestReviewFollowUpTaskSetMaterializesExistingReferencesDispositionsAndReplays(t *testing.T) {
	f := newOrganizerArtifactFixture(t)
	artifact := f.createArtifact(t)
	result, replayed, err := f.artifacts.MaterializeTaskSet(f.ctx, artifact.ID, f.config)
	if err != nil {
		t.Fatalf("materialize organizer plan: %v", err)
	}
	if replayed || result.TaskIDs["existing"] != f.existing.ID || result.TaskIDs["created"] == "" {
		t.Fatalf("result = %+v, replayed = %t", result, replayed)
	}
	created, err := f.tasks.GetTask(f.ctx, result.TaskIDs["created"])
	if err != nil {
		t.Fatal(err)
	}
	if created.SourceTaskID == nil || *created.SourceTaskID != f.source.ID || created.SourceChangeID == nil || *created.SourceChangeID != "ch-review-source" {
		t.Fatalf("created provenance = task:%v change:%v", created.SourceTaskID, created.SourceChangeID)
	}
	var dispositions, dispositioned, blocks int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_follow_up_dispositions WHERE plan_revision_id = ?`, f.planID).Scan(&dispositions); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_follow_up_proposals WHERE state = 'dispositioned'`).Scan(&dispositioned); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'blocks'`, f.existing.ID, created.ID).Scan(&blocks); err != nil {
		t.Fatal(err)
	}
	if dispositions != 2 || dispositioned != 2 || blocks != 1 {
		t.Fatalf("dispositions/proposals/blocks = %d/%d/%d", dispositions, dispositioned, blocks)
	}
	var taskCreatedEvents, relationEvents int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE kind = ? AND task_id = ?`, EventTaskCreated, created.ID).Scan(&taskCreatedEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE kind = ?`, EventRelationLinked).Scan(&relationEvents); err != nil {
		t.Fatal(err)
	}
	if taskCreatedEvents != 1 || relationEvents != 3 {
		t.Fatalf("materialization task.created/relation.linked events = %d/%d, want 1/3", taskCreatedEvents, relationEvents)
	}
	var planState, setState string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_follow_up_plan_revisions WHERE id = ?`, f.planID).Scan(&planState); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT state FROM review_follow_up_sets WHERE id = ?`, f.setID).Scan(&setState); err != nil {
		t.Fatal(err)
	}
	if planState != "materialized" || setState != "materialized" {
		t.Fatalf("plan/set states = %s/%s", planState, setState)
	}
	registry, err := NewThreadService(f.store.DB()).TaskFindingsRegistry(f.ctx, f.source.ID)
	if err != nil {
		t.Fatalf("load source findings registry: %v", err)
	}
	if len(registry.FollowUpSets) != 1 || registry.FollowUpSets[0].ID != f.setID ||
		registry.FollowUpSets[0].Plan == nil || registry.FollowUpSets[0].Plan.ArtifactID != artifact.ID ||
		len(registry.FollowUpSets[0].Batches) != 1 || len(registry.FollowUpSets[0].Batches[0].Proposals) != 2 {
		t.Fatalf("organized follow-up registry = %+v", registry.FollowUpSets)
	}
	proposals := registry.FollowUpSets[0].Batches[0].Proposals
	if proposals[0].Disposition == nil || proposals[0].Disposition.TargetTaskID != created.ID ||
		len(proposals[0].Disposition.TargetBlockerIDs) != 1 || proposals[0].Disposition.TargetBlockerIDs[0] != f.existing.ID ||
		proposals[1].Disposition == nil || proposals[1].Disposition.TargetTaskID != f.existing.ID {
		t.Fatalf("organized proposal dispositions = %+v", proposals)
	}
	if registry.Summary.DeferredToTask != 2 {
		t.Fatalf("organized deferred summary = %d, want 2", registry.Summary.DeferredToTask)
	}
	second, replayed, err := f.artifacts.MaterializeTaskSet(f.ctx, artifact.ID, f.config)
	if err != nil || !replayed || second.TaskIDs["created"] != created.ID {
		t.Fatalf("replay = %+v, %t, %v", second, replayed, err)
	}
	var tasks, relations int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM work_item_relations`).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	if tasks != 4 || dispositions != 2 || blocks != 1 || relations != 3 {
		t.Fatalf("post-replay tasks/dispositions/blocks/relations = %d/%d/%d/%d", tasks, dispositions, blocks, relations)
	}
	var metrics strings.Builder
	f.metrics.Render(&metrics)
	for _, want := range []string{
		`flow_review_follow_up_materializations_total{outcome="completed"} 1`,
		`flow_review_follow_up_materializations_total{outcome="replayed"} 1`,
		`flow_review_follow_up_plan_outcomes_total{outcome="created"} 1`,
		`flow_review_follow_up_plan_outcomes_total{outcome="reused"} 1`,
		`flow_review_follow_up_organizer_runs_total{outcome="completed"} 1`,
		`flow_review_follow_up_dependency_blocked_tasks_total 1`,
	} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("materialization metrics missing %q:\n%s", want, metrics.String())
		}
	}
}

func TestReviewFollowUpTaskSetRejectsStaleRevisionBeforeMaterializationReceipt(t *testing.T) {
	f := newOrganizerArtifactFixture(t)
	artifact := f.createArtifact(t)
	if _, err := f.store.DB().Exec(`UPDATE review_follow_up_sets SET revision = 2 WHERE id = ?`, f.setID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.artifacts.MaterializeTaskSet(f.ctx, artifact.ID, f.config); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("materialize stale plan error = %v", err)
	}
	var receipts, dispositions int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM workflow_materializations`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_follow_up_dispositions`).Scan(&dispositions); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 || dispositions != 0 {
		t.Fatalf("stale materialization receipts/dispositions = %d/%d", receipts, dispositions)
	}
}
