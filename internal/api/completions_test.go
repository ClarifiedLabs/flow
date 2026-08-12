package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestTaskDoneCarriesEvidenceAndCompletionsAudit(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	task, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Audited task", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Complete the task via the API with a message and evidence.
	doneBody := map[string]any{
		"resolution": "completed",
		"note":       "shipped with tests",
		"evidence": []map[string]string{
			{"type": "commit", "value": "deadbeef"},
			{"type": "test", "value": "go test ./..."},
		},
	}
	doneResp := httptest.NewRecorder()
	fixture.Server.ServeHTTP(doneResp, authorizedRequest(http.MethodPost, "/v2/projects/"+fixture.Project.ID+"/tasks/"+task.ID+"/done", doneBody))
	if doneResp.Code != http.StatusOK {
		t.Fatalf("done status = %d, body = %s", doneResp.Code, doneResp.Body.String())
	}
	var doneTaskResp struct {
		Task coordinator.Task `json:"task"`
	}
	if err := json.NewDecoder(doneResp.Body).Decode(&doneTaskResp); err != nil {
		t.Fatalf("decode done response: %v", err)
	}
	if doneTaskResp.Task.DoneMessage != "shipped with tests" || len(doneTaskResp.Task.DoneEvidence) != 2 {
		t.Fatalf("done task message=%q evidence=%+v", doneTaskResp.Task.DoneMessage, doneTaskResp.Task.DoneEvidence)
	}

	// The completions audit endpoint returns it with resolution + evidence.
	auditResp := httptest.NewRecorder()
	fixture.Server.ServeHTTP(auditResp, authorizedRequest(http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/completions", nil))
	if auditResp.Code != http.StatusOK {
		t.Fatalf("completions status = %d", auditResp.Code)
	}
	var auditBody tasksResponse
	if err := json.NewDecoder(auditResp.Body).Decode(&auditBody); err != nil {
		t.Fatalf("decode completions: %v", err)
	}
	if len(auditBody.Tasks) != 1 || auditBody.Tasks[0].ID != task.ID {
		t.Fatalf("completions = %+v", auditBody.Tasks)
	}
	if auditBody.Tasks[0].DoneMessage != "shipped with tests" || len(auditBody.Tasks[0].DoneEvidence) != 2 {
		t.Fatalf("completion message=%q evidence=%+v", auditBody.Tasks[0].DoneMessage, auditBody.Tasks[0].DoneEvidence)
	}

	// resolution filter narrows.
	filterResp := httptest.NewRecorder()
	fixture.Server.ServeHTTP(filterResp, authorizedRequest(http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/completions?resolution=rejected", nil))
	var filterBody tasksResponse
	if err := json.NewDecoder(filterResp.Body).Decode(&filterBody); err != nil {
		t.Fatalf("decode filter: %v", err)
	}
	if len(filterBody.Tasks) != 0 {
		t.Fatalf("rejected filter = %+v, want empty", filterBody.Tasks)
	}

	// invalid resolution is a 400.
	badResp := httptest.NewRecorder()
	fixture.Server.ServeHTTP(badResp, authorizedRequest(http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/completions?resolution=nope", nil))
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("invalid resolution status = %d, want 400", badResp.Code)
	}
}
