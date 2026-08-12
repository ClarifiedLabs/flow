package coordinator

import (
	"encoding/json"
	"testing"
)

// Reconciler-driven epic transitions (auto-complete when the last child
// finishes, auto-reopen when a child reopens) are emitted into the event log
// from inside reconcileEpicAncestorsTx itself, because no single service owns
// that mutation.
func TestReconcilerEmitsAutomaticEpicTransitions(t *testing.T) {
	ctx, log, tasks, runs := openEventLogTestServices(t)
	epics := NewEpicService(log.db, "p-test", nil)

	epic, err := epics.Create(ctx, CreateEpicInput{Title: "Auto epic", CompletionPolicy: EpicAllChildren})
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Only child", CreatedBy: ActorHuman, ParentItemID: epic.ID},
	})
	if err != nil {
		t.Fatalf("create child task: %v", err)
	}

	if _, err := runs.ForceDone(ctx, task.ID, ResolutionCompleted, "done", ActorHuman); err != nil {
		t.Fatalf("force done: %v", err)
	}

	events, err := log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var autoComplete *Event
	for i := range events {
		if events[i].Kind == EventEpicCompleted {
			autoComplete = &events[i]
		}
	}
	if autoComplete == nil {
		t.Fatalf("no epic.completed event after last child done: %+v", eventKinds(t, log, ctx))
	}
	if autoComplete.Actor != string(ActorSystem) {
		t.Fatalf("auto-complete actor = %q, want system", autoComplete.Actor)
	}
	var payload struct {
		EpicID    string `json:"epic_id"`
		Automatic bool   `json:"automatic"`
	}
	if err := json.Unmarshal(autoComplete.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.EpicID != epic.ID || !payload.Automatic {
		t.Fatalf("auto-complete payload = %+v, want epic %s automatic", payload, epic.ID)
	}

	// Reopening the child reopens the auto-completed epic, also emitted.
	if _, err := runs.Reopen(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	events, err = log.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list events after reopen: %v", err)
	}
	var autoReopen *Event
	for i := range events {
		if events[i].Kind == EventEpicReopened {
			autoReopen = &events[i]
		}
	}
	if autoReopen == nil {
		t.Fatalf("no epic.reopened event after child reopen: %+v", eventKinds(t, log, ctx))
	}
	if err := json.Unmarshal(autoReopen.Payload, &payload); err != nil {
		t.Fatalf("decode reopen payload: %v", err)
	}
	if payload.EpicID != epic.ID || !payload.Automatic || autoReopen.Actor != string(ActorSystem) {
		t.Fatalf("auto-reopen event = %+v payload %+v", autoReopen, payload)
	}
}
