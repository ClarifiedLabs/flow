package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOwnerRulingsProjectReplaceAndReplay(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agents := NewAgentDefService(flows.db)
	author, err := agents.Create(ctx, AgentDefInput{Name: "ruling author", Harness: "harness", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name: "ruling flow", StartNode: "work", TransitionBudget: 10,
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: author.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Apply owner guidance", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Schedule(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	first, err := runs.RecordOwnerRuling(ctx, RecordOwnerRulingInput{
		TaskID: task.ID, Body: "Use the narrow API surface.", Source: OwnerRulingSourceOwner,
		Actor: ActorHuman, IdempotencyKey: "guide-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := runs.RecordOwnerRuling(ctx, RecordOwnerRulingInput{
		TaskID: task.ID, Body: "Use the narrow API surface.", Source: OwnerRulingSourceOwner,
		Actor: ActorHuman, IdempotencyKey: "guide-1",
	})
	if err != nil || replay.Ruling.RulingID != first.Ruling.RulingID {
		t.Fatalf("replay = %+v, err=%v", replay, err)
	}
	if _, err := runs.RecordOwnerRuling(ctx, RecordOwnerRulingInput{
		TaskID: task.ID, Body: "Different", Source: OwnerRulingSourceOwner,
		Actor: ActorHuman, IdempotencyKey: "guide-1",
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	replacement, err := runs.RecordOwnerRuling(ctx, RecordOwnerRulingInput{
		TaskID: task.ID, Body: "Use the typed API surface.", Source: OwnerRulingSourceOwner,
		SupersedesID: first.Ruling.RulingID, Actor: ActorHuman, IdempotencyKey: "guide-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, active, err := runs.ActiveForTask(ctx, task.ID)
	if err != nil || !active {
		t.Fatalf("active run = %+v, active=%v, err=%v", run, active, err)
	}
	detail, err := runs.Detail(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ActiveRulings) != 1 || detail.ActiveRulings[0].RulingID != replacement.Ruling.RulingID ||
		detail.ActiveRulings[0].SupersedesID != first.Ruling.RulingID {
		t.Fatalf("active rulings = %+v", detail.ActiveRulings)
	}
}

func TestProjectOwnerRulingsFailsClosedOnCorruptTransition(t *testing.T) {
	payload, err := json.Marshal(ownerRulingPayload{
		SchemaVersion: 99, RulingID: "rule-bad", Body: "bad", Source: OwnerRulingSourceOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProjectOwnerRulings([]WorkflowTransition{{
		Sequence: 1, TaskID: "t-1", WorkflowRunID: "wr-1", EventKind: OwnerRulingEventKind,
		Payload: payload, Actor: string(ActorHuman), CreatedAt: time.Now().UTC(),
	}})
	if err == nil {
		t.Fatal("corrupt ruling transition unexpectedly projected")
	}
}

func TestOwnerRulingDeliveryTargetsLiveSameRunAgentsOnce(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agents := NewAgentDefService(flows.db)
	author, err := agents.Create(ctx, AgentDefInput{Name: "delivery author", Harness: "harness", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name: "delivery flow", StartNode: "work", TransitionBudget: 10,
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: author.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	createRun := func(title string) (Task, WorkflowRun) {
		t.Helper()
		task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: title, FlowID: flow.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runs.Schedule(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		run, active, err := runs.ActiveForTask(ctx, task.ID)
		if err != nil || !active {
			t.Fatalf("active run for %s = %+v active=%v err=%v", task.ID, run, active, err)
		}
		return task, run
	}
	task, run := createRun("Deliver owner guidance")
	oldTask, oldRun := createRun("Different workflow run")
	now := time.Now().UTC()
	insertSession := func(id, taskID, runID, nodeRunID, role, state string) {
		t.Helper()
		jobID, leaseID := "j-"+id, "l-"+id
		var projectedNodeRunID any
		if nodeRunID != "" {
			projectedNodeRunID = nodeRunID
		}
		if _, err := runs.db.ExecContext(ctx, `
INSERT INTO jobs (id, task_id, workflow_run_id, node_run_id, role, state, capacity_bucket, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'running', 'persistent_agent', ?, ?)`, jobID, taskID, runID, projectedNodeRunID, role,
			formatTime(now), formatTime(now)); err != nil {
			t.Fatalf("insert job %s: %v", jobID, err)
		}
		if _, err := runs.db.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, 'w-delivery', 'persistent_agent', ?, ?)`, leaseID, jobID, formatTime(now), formatTime(now.Add(time.Hour))); err != nil {
			t.Fatalf("insert lease %s: %v", leaseID, err)
		}
		if _, err := runs.db.ExecContext(ctx, `
INSERT INTO sessions (id, task_id, workflow_run_id, node_run_id, job_id, lease_id, worker_id, role,
	workspace_mode, runtime_state, branch, base, harness, token_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'w-delivery', ?, 'change', ?, ?, 'main', 'harness', ?, ?, ?)`,
			id, taskID, runID, projectedNodeRunID, jobID, leaseID, role, state, "task/"+taskID, "token-"+id,
			formatTime(now), formatTime(now)); err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}
	}
	insertSession("s-author", task.ID, run.ID, run.CurrentNodeRunID, "author", "working")
	insertSession("s-reviewer", task.ID, run.ID, run.CurrentNodeRunID, "reviewer", "waiting")
	insertSession("s-verifier", task.ID, run.ID, run.CurrentNodeRunID, "verifier", "starting")
	insertSession("s-console", task.ID, run.ID, run.CurrentNodeRunID, "console", "working")
	insertSession("s-finished", task.ID, run.ID, run.CurrentNodeRunID, "reviewer", "finished")
	insertSession("s-old-run", oldTask.ID, oldRun.ID, oldRun.CurrentNodeRunID, "reviewer", "working")

	recorded, err := runs.RecordOwnerRuling(ctx, RecordOwnerRulingInput{
		TaskID: task.ID, Body: "Keep the change local.", Actor: ActorHuman, IdempotencyKey: "delivery-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Delivery.TargetedSessions != 3 || recorded.Delivery.QueuedSessions != 3 || len(recorded.Delivery.Warnings) != 0 {
		t.Fatalf("delivery = %+v, want three queued live agents", recorded.Delivery)
	}
	duplicate := runs.deliverOwnerRuling(ctx, recorded.Ruling)
	if duplicate.TargetedSessions != 3 || duplicate.QueuedSessions != 0 || duplicate.DuplicateSessions != 3 {
		t.Fatalf("duplicate delivery = %+v", duplicate)
	}
	var delivered, excluded int
	var deliveredBody string
	if err := runs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE source_id = ?`, recorded.Ruling.RulingID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if err := runs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages
WHERE source_id = ? AND session_id IN ('s-console','s-finished','s-old-run')`, recorded.Ruling.RulingID).Scan(&excluded); err != nil {
		t.Fatal(err)
	}
	if err := runs.db.QueryRowContext(ctx, `SELECT body FROM session_messages WHERE source_id = ? LIMIT 1`, recorded.Ruling.RulingID).Scan(&deliveredBody); err != nil {
		t.Fatal(err)
	}
	if delivered != 3 || excluded != 0 {
		t.Fatalf("delivered=%d excluded=%d, want 3 and 0", delivered, excluded)
	}
	if !strings.Contains(deliveredBody, recorded.Ruling.RulingID) || !strings.Contains(deliveredBody, recorded.Ruling.Body) {
		t.Fatalf("delivered body = %q", deliveredBody)
	}
}
