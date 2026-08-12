package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newCompletionServices(t *testing.T) (context.Context, *TaskService, *WorkflowRunService) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	flows := NewFlowService(store.DB())
	tasks := NewTaskService(store.DB(), "p-test")
	runs := NewWorkflowRunService(store.DB(), flows, tasks)
	return ctx, tasks, runs
}

func TestForceDoneDetailedPersistsMessageAndEvidence(t *testing.T) {
	ctx, tasks, runs := newCompletionServices(t)

	task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Evidence task", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	evidence := []Evidence{
		{Type: EvidenceCommit, Value: "abc123"},
		{Type: EvidenceTest, Value: "go test ./... green"},
	}
	done, err := runs.ForceDoneDetailed(ctx, task.ID, ResolutionCompleted, "shipped the fix", evidence, ActorHuman)
	if err != nil {
		t.Fatalf("force done: %v", err)
	}
	if done.DoneMessage != "shipped the fix" {
		t.Fatalf("done message = %q", done.DoneMessage)
	}
	if len(done.DoneEvidence) != 2 || done.DoneEvidence[0].Type != EvidenceCommit || done.DoneEvidence[1].Value != "go test ./... green" {
		t.Fatalf("done evidence = %+v", done.DoneEvidence)
	}

	// Read back via GetTask to prove persistence, not just the returned struct.
	got, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DoneMessage != "shipped the fix" || len(got.DoneEvidence) != 2 {
		t.Fatalf("readback message=%q evidence=%+v", got.DoneMessage, got.DoneEvidence)
	}
}

func TestForceDoneDetailedValidatesEvidence(t *testing.T) {
	ctx, tasks, runs := newCompletionServices(t)

	task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Bad evidence", CreatedBy: ActorHuman},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := runs.ForceDoneDetailed(ctx, task.ID, ResolutionCompleted, "x", []Evidence{{Type: "bogus", Value: "v"}}, ActorHuman); err == nil {
		t.Fatalf("invalid evidence type must fail")
	}
	if _, err := runs.ForceDoneDetailed(ctx, task.ID, ResolutionCompleted, "x", []Evidence{{Type: EvidenceCommit, Value: "  "}}, ActorHuman); err == nil {
		t.Fatalf("empty evidence value must fail")
	}
	// The task must still be open after the rejected close.
	got, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != nil && *got.State == LifecycleDone {
		t.Fatalf("task was closed despite invalid evidence")
	}
}

func TestListCompletionsFiltersAndOrders(t *testing.T) {
	ctx, tasks, runs := newCompletionServices(t)

	mk := func(title string, resolution DoneResolution, message string) Task {
		task, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
			Task: CreateTaskInput{Title: title, CreatedBy: ActorHuman},
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		if _, err := runs.ForceDoneDetailed(ctx, task.ID, resolution, message, nil, ActorHuman); err != nil {
			t.Fatalf("done %q: %v", title, err)
		}
		return task
	}
	mk("Completed one", ResolutionCompleted, "done well")
	mk("Rejected one", ResolutionRejected, "not needed")
	// an open task must not appear
	if _, err := tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "Still open", CreatedBy: ActorHuman},
	}); err != nil {
		t.Fatalf("create open: %v", err)
	}

	all, err := tasks.ListCompletions(ctx, "", 0)
	if err != nil {
		t.Fatalf("list completions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all completions = %d, want 2", len(all))
	}

	completed, err := tasks.ListCompletions(ctx, string(ResolutionCompleted), 0)
	if err != nil {
		t.Fatalf("filter completed: %v", err)
	}
	if len(completed) != 1 || completed[0].Title != "Completed one" {
		t.Fatalf("completed filter = %+v", completed)
	}
	if completed[0].DoneMessage != "done well" {
		t.Fatalf("completion message = %q", completed[0].DoneMessage)
	}
}
