package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestReadyTasksMatchesScheduleGateBlockers pins the parity between the ready
// read model and the schedule-time dependency gate: a task is ready exactly
// when it is unscheduled and effectiveUnresolvedBlockerCountTx reports zero.
func TestReadyTasksMatchesScheduleGateBlockers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	items := NewWorkItemService(store.DB(), "p-test")
	epics := NewEpicService(store.DB(), "p-test", items)

	free, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "free", Priority: 1})
	if err != nil {
		t.Fatalf("create free task: %v", err)
	}
	blocker, blocked := createTwoTasks(t, tasks, "blocker", "blocked")
	if err := tasks.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}

	// An epic blocker counts too: the gate's effective semantics cover every
	// work-item kind, not just tasks.
	gateEpic, err := epics.Create(ctx, CreateEpicInput{Title: "gate epic", CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	epicBlocked, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "epic blocked"})
	if err != nil {
		t.Fatalf("create epic-blocked task: %v", err)
	}
	if err := items.Link(ctx, gateEpic.ID, epicBlocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link epic blocker: %v", err)
	}

	// A blocker on an ancestor blocks the descendant. Tasks cannot contain
	// tasks, so the ancestor is an epic here.
	parentEpic, err := epics.Create(ctx, CreateEpicInput{Title: "blocked parent epic", CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create parent epic: %v", err)
	}
	childOfBlocked, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "child of blocked", ParentItemID: parentEpic.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := items.Link(ctx, blocker.ID, parentEpic.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link parent blocker: %v", err)
	}

	assertReadyParity := func() {
		t.Helper()
		ready, err := tasks.ReadyTasks(ctx, TaskFilter{})
		if err != nil {
			t.Fatalf("ready tasks: %v", err)
		}
		readyIDs := make(map[string]bool, len(ready))
		for _, task := range ready {
			readyIDs[task.ID] = true
		}
		all, err := tasks.ListTasks(ctx, TaskFilter{})
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		for _, task := range all {
			gate, err := effectiveUnresolvedBlockerCountTx(ctx, store.DB(), task.ID)
			if err != nil {
				t.Fatalf("gate count for %s: %v", task.ID, err)
			}
			want := task.State == nil && gate == 0
			if readyIDs[task.ID] != want {
				t.Fatalf("ready membership for %s = %t, want %t (state=%v gate=%d)", task.ID, readyIDs[task.ID], want, task.State, gate)
			}
		}
	}

	assertReadyParity()

	// Initial set: the blocker itself is ready (nothing blocks it), plus the
	// free task; priority puts the free task first.
	ready, err := tasks.ReadyTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatalf("ready tasks: %v", err)
	}
	if len(ready) != 2 || ready[0].ID != free.ID || ready[1].ID != blocker.ID {
		t.Fatalf("ready = %+v, want [%s %s]", readyTaskIDs(ready), free.ID, blocker.ID)
	}

	// Finishing the blocker unblocks its dependents; the open epic still gates.
	stamp := formatTime(time.Now().UTC())
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(ResolutionCompleted), stamp, stamp, blocker.ID); err != nil {
		t.Fatalf("finish blocker: %v", err)
	}
	assertReadyParity()
	ready, err = tasks.ReadyTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatalf("ready tasks after done: %v", err)
	}
	readyIDs := make(map[string]bool, len(ready))
	for _, task := range ready {
		readyIDs[task.ID] = true
	}
	if !readyIDs[blocked.ID] || !readyIDs[childOfBlocked.ID] {
		t.Fatalf("ready = %+v, want the blocker dependents unblocked", readyTaskIDs(ready))
	}
	if readyIDs[epicBlocked.ID] {
		t.Fatalf("ready = %+v, want %s still gated by the open epic", readyTaskIDs(ready), epicBlocked.ID)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE epics SET status = 'completed', completed_at = ? WHERE id = ?`, stamp, gateEpic.ID); err != nil {
		t.Fatalf("complete epic: %v", err)
	}
	assertReadyParity()
}

func TestReadyTasksOrdersByPriorityThenCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	low, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "low"})
	if err != nil {
		t.Fatalf("create low: %v", err)
	}
	high, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "high", Priority: 9})
	if err != nil {
		t.Fatalf("create high: %v", err)
	}

	ready, err := tasks.ReadyTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatalf("ready tasks: %v", err)
	}
	if len(ready) != 2 || ready[0].ID != high.ID || ready[1].ID != low.ID {
		t.Fatalf("ready = %+v, want [%s %s] (priority desc)", readyTaskIDs(ready), high.ID, low.ID)
	}
}

func TestTaskBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	blocker, blocked := createTwoTasks(t, tasks, "blocker", "blocked")
	if err := tasks.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}

	if got, err := tasks.TaskBlocked(ctx, blocked.ID); err != nil || !got {
		t.Fatalf("blocked task TaskBlocked = %t, %v; want true", got, err)
	}
	if got, err := tasks.TaskBlocked(ctx, blocker.ID); err != nil || got {
		t.Fatalf("blocker TaskBlocked = %t, %v; want false", got, err)
	}

	// Scheduled tasks report unblocked even with open blockers: the gate
	// suppresses node creation, but the board lane stays "scheduled".
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ? WHERE id = ?`, string(LifecycleScheduled), blocked.ID); err != nil {
		t.Fatalf("mark task scheduled: %v", err)
	}
	if got, err := tasks.TaskBlocked(ctx, blocked.ID); err != nil || got {
		t.Fatalf("scheduled task TaskBlocked = %t, %v; want false", got, err)
	}

	// In-progress with an open human wait is the board's blocked lane.
	free, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "waiting task"})
	if err != nil {
		t.Fatalf("create waiting task: %v", err)
	}
	markTaskInProgress(t, store.DB(), free.ID)
	seedActiveWorkflowRun(t, store.DB(), free.ID, "wr-wait", "author", "nr-wait")
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO workflow_waits (
	id, workflow_run_id, node_run_id, kind, state, created_by, created_at
) VALUES ('ww-blocked-1', 'wr-wait', 'nr-wait', 'human_gate', 'open', 'system', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert workflow wait: %v", err)
	}
	if got, err := tasks.TaskBlocked(ctx, free.ID); err != nil || !got {
		t.Fatalf("waiting task TaskBlocked = %t, %v; want true", got, err)
	}

	// Done tasks are never blocked.
	stamp := formatTime(time.Now().UTC())
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tasks SET lifecycle_state = ?, done_resolution = ?, done_at = ?, updated_at = ? WHERE id = ?`,
		string(LifecycleDone), string(ResolutionCompleted), stamp, stamp, blocker.ID); err != nil {
		t.Fatalf("finish blocker: %v", err)
	}
	if got, err := tasks.TaskBlocked(ctx, blocker.ID); err != nil || got {
		t.Fatalf("done task TaskBlocked = %t, %v; want false", got, err)
	}
	if got, err := tasks.TaskBlocked(ctx, blocked.ID); err != nil || got {
		t.Fatalf("unblocked task TaskBlocked = %t, %v; want false", got, err)
	}
}

func readyTaskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}
