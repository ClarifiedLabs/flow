package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func openEventLogTestDB(t *testing.T) (*EventLogService, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewEventLogService(store.DB()), ctx
}

func TestEventLogAppendAssignsCursorAndDefaults(t *testing.T) {
	log, ctx := openEventLogTestDB(t)

	first, err := log.Append(ctx, Event{Kind: EventTaskCreated, TaskID: "t-1", Actor: "human"})
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if first.Seq != 1 || first.ID == "" || first.OccurredAt.IsZero() {
		t.Fatalf("first = %+v", first)
	}
	if string(first.Payload) != `{}` {
		t.Fatalf("default payload = %s, want {}", first.Payload)
	}

	second, err := log.Append(ctx, Event{Kind: EventTaskDone, TaskID: "t-1", Payload: json.RawMessage(`{"resolution":"completed"}`)})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if second.Seq != 2 {
		t.Fatalf("second seq = %d, want 2", second.Seq)
	}
	if second.ID == first.ID {
		t.Fatalf("event ids must be unique")
	}

	if _, err := log.Append(ctx, Event{}); err == nil {
		t.Fatalf("empty kind must fail")
	}
	if _, err := log.Append(ctx, Event{Kind: "x", Payload: json.RawMessage(`{no`)}); err == nil {
		t.Fatalf("invalid payload JSON must fail")
	}
}

func TestEventLogListPagesWithSinceAndLimit(t *testing.T) {
	log, ctx := openEventLogTestDB(t)

	kinds := []string{EventTaskCreated, EventTaskEdited, EventTaskDone, EventGitPush}
	for _, kind := range kinds {
		if _, err := log.Append(ctx, Event{Kind: kind}); err != nil {
			t.Fatalf("append %s: %v", kind, err)
		}
	}

	all, err := log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 || all[0].Kind != EventTaskCreated || all[3].Kind != EventGitPush {
		t.Fatalf("all = %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seqs not increasing: %+v", all)
		}
	}

	page, err := log.List(ctx, all[1].Seq, 1)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 1 || page[0].Kind != EventTaskDone {
		t.Fatalf("page = %+v, want exactly task.done", page)
	}

	none, err := log.List(ctx, all[3].Seq, 0)
	if err != nil {
		t.Fatalf("list tail: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("tail = %+v, want empty", none)
	}
}

func openEventLogTestServices(t *testing.T) (context.Context, *EventLogService, *TaskService, *WorkflowRunService) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	db := store.DB()
	log := NewEventLogService(db)
	tasks := NewTaskService(db, "p-test")
	tasks.SetEventLog(log)
	runs := NewWorkflowRunServiceWithOptions(db, NewFlowService(db), tasks, WorkflowRunServiceOptions{})
	runs.SetEventLog(log)
	return ctx, log, tasks, runs
}

func eventKinds(t *testing.T, log *EventLogService, ctx context.Context) []string {
	t.Helper()
	events, err := log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestTaskServiceEmitsLifecycleEvents(t *testing.T) {
	ctx, log, tasks, _ := openEventLogTestServices(t)

	created, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "First", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Second", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := tasks.EditTask(ctx, second.ID, EditTaskInput{Priority: intPtr(3)}); err != nil {
		t.Fatalf("edit second: %v", err)
	}
	if err := tasks.LinkTasks(ctx, second.ID, created.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := tasks.UnlinkTasks(ctx, second.ID, created.ID, RelationBlocks); err != nil {
		t.Fatalf("unlink: %v", err)
	}

	want := []string{EventTaskCreated, EventTaskCreated, EventTaskEdited, EventRelationLinked, EventRelationUnlinked}
	got := eventKinds(t, log, ctx)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}

	events, err := log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if events[0].Actor != string(ActorHuman) || events[0].TaskID != created.ID {
		t.Fatalf("create event = %+v", events[0])
	}
	var linkPayload struct {
		Source   string `json:"source"`
		Target   string `json:"target"`
		Relation string `json:"relation"`
	}
	if err := json.Unmarshal(events[3].Payload, &linkPayload); err != nil {
		t.Fatalf("link payload: %v", err)
	}
	if linkPayload.Source != second.ID || linkPayload.Target != created.ID || linkPayload.Relation != string(RelationBlocks) {
		t.Fatalf("link payload = %+v", linkPayload)
	}
}

func TestWorkflowRunServiceEmitsDoneAndReopenEvents(t *testing.T) {
	ctx, log, tasks, runs := openEventLogTestServices(t)

	task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Completable", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := runs.ForceDone(ctx, task.ID, ResolutionCompleted, "ship it", ActorHuman); err != nil {
		t.Fatalf("force done: %v", err)
	}
	if _, err := runs.Reopen(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	want := []string{EventTaskCreated, EventTaskDone, EventTaskReopened}
	got := eventKinds(t, log, ctx)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}

	events, err := log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var donePayload struct {
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(events[1].Payload, &donePayload); err != nil {
		t.Fatalf("done payload: %v", err)
	}
	if donePayload.Resolution != string(ResolutionCompleted) || donePayload.Note != "ship it" {
		t.Fatalf("done payload = %+v", donePayload)
	}
}

func TestEventLogAppendFailureDoesNotFailMutation(t *testing.T) {
	ctx, _, tasks, _ := openEventLogTestServices(t)
	// Emission is best-effort: disabling the log must never fail the mutation.
	tasks.SetEventLog(nil)
	if _, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "No log", CreatedBy: ActorHuman},
	}); err != nil {
		t.Fatalf("create with nil log: %v", err)
	}
}

func intPtr(v int) *int { return &v }
