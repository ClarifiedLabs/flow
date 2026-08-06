package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestOwnerRulingAPIRequiresIdempotencyAndReplaysExactResponse(t *testing.T) {
	fixture := newTestFixture(t)
	task, err := fixture.Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Guide the active workflow"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	path := "/v2/projects/" + fixture.Project.ID + "/tasks/" + task.ID + "/workflow/rulings"
	request := workflowOwnerRulingRequest{Body: "Keep this implementation local to the storage package."}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, request, http.StatusBadRequest, nil)
	var first coordinator.RecordOwnerRulingResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, request, http.StatusCreated, &first,
		idempotencyHeader, "guide-api-1")
	var replay coordinator.RecordOwnerRulingResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, request, http.StatusCreated, &replay,
		idempotencyHeader, "guide-api-1")
	if first.Ruling.RulingID == "" || replay.Ruling.RulingID != first.Ruling.RulingID || replay.Ruling.Body != first.Ruling.Body {
		t.Fatalf("first=%+v replay=%+v", first, replay)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path,
		workflowOwnerRulingRequest{Body: "Different guidance."}, http.StatusConflict, nil,
		idempotencyHeader, "guide-api-1")
}
