package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestEventsEndpointListsAppendedEvents(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	task, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Evented task", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	path := "/v2/projects/" + fixture.Project.ID + "/events"
	response := httptest.NewRecorder()
	fixture.Server.ServeHTTP(response, authorizedRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body eventsResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode events response: %v", err)
	}
	if len(body.Events) == 0 {
		t.Fatalf("events = [], want the task.created event")
	}
	var found *coordinator.Event
	for i := range body.Events {
		if body.Events[i].Kind == coordinator.EventTaskCreated && body.Events[i].TaskID == task.ID {
			found = &body.Events[i]
		}
	}
	if found == nil {
		t.Fatalf("no task.created event for %s in %+v", task.ID, body.Events)
	}
	var payload struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("decode task.created payload: %v", err)
	}
	if payload.Title != "Evented task" {
		t.Fatalf("payload title = %q, want %q", payload.Title, "Evented task")
	}
	if want := body.Events[len(body.Events)-1].Seq; body.NextSince != want {
		t.Fatalf("next_since = %d, want last event seq %d", body.NextSince, want)
	}

	paged := httptest.NewRecorder()
	fixture.Server.ServeHTTP(paged, authorizedRequest(http.MethodGet, path+"?since=1", nil))
	if paged.Code != http.StatusOK {
		t.Fatalf("paged status = %d, want 200", paged.Code)
	}
	var pageBody eventsResponse
	if err := json.NewDecoder(paged.Body).Decode(&pageBody); err != nil {
		t.Fatalf("decode paged response: %v", err)
	}
	for _, event := range pageBody.Events {
		if event.Seq <= 1 {
			t.Fatalf("since=1 returned seq %d: %+v", event.Seq, event)
		}
	}
}

func TestEventsEndpointFiltersAndResetField(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	first, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Alpha", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Beta", CreatedBy: coordinator.ActorHuman},
	}); err != nil {
		t.Fatalf("create beta: %v", err)
	}

	path := "/v2/projects/" + fixture.Project.ID + "/events"

	// reset_required is present and false in the poll response.
	base := httptest.NewRecorder()
	fixture.Server.ServeHTTP(base, authorizedRequest(http.MethodGet, path, nil))
	var baseBody struct {
		ResetRequired *bool `json:"reset_required"`
	}
	if err := json.NewDecoder(base.Body).Decode(&baseBody); err != nil {
		t.Fatalf("decode base response: %v", err)
	}
	if baseBody.ResetRequired == nil || *baseBody.ResetRequired {
		t.Fatalf("reset_required = %v, want present and false", baseBody.ResetRequired)
	}

	// kind filter returns only matching kinds.
	byKind := httptest.NewRecorder()
	fixture.Server.ServeHTTP(byKind, authorizedRequest(http.MethodGet, path+"?kind="+coordinator.EventTaskCreated, nil))
	var kindBody eventsResponse
	if err := json.NewDecoder(byKind.Body).Decode(&kindBody); err != nil {
		t.Fatalf("decode kind filter: %v", err)
	}
	if len(kindBody.Events) != 2 {
		t.Fatalf("kind filter returned %d events, want 2 task.created", len(kindBody.Events))
	}
	for _, event := range kindBody.Events {
		if event.Kind != coordinator.EventTaskCreated {
			t.Fatalf("kind filter leaked %s", event.Kind)
		}
	}

	// task filter narrows to one task.
	byTask := httptest.NewRecorder()
	fixture.Server.ServeHTTP(byTask, authorizedRequest(http.MethodGet, path+"?task="+first.ID, nil))
	var taskBody eventsResponse
	if err := json.NewDecoder(byTask.Body).Decode(&taskBody); err != nil {
		t.Fatalf("decode task filter: %v", err)
	}
	if len(taskBody.Events) == 0 {
		t.Fatalf("task filter returned no events for %s", first.ID)
	}
	for _, event := range taskBody.Events {
		if event.TaskID != first.ID {
			t.Fatalf("task filter leaked task %s", event.TaskID)
		}
	}

	// a kind that has no events returns an empty page that echoes the cursor.
	none := httptest.NewRecorder()
	fixture.Server.ServeHTTP(none, authorizedRequest(http.MethodGet, path+"?kind="+coordinator.EventGitPush, nil))
	var noneBody eventsResponse
	if err := json.NewDecoder(none.Body).Decode(&noneBody); err != nil {
		t.Fatalf("decode empty filter: %v", err)
	}
	if len(noneBody.Events) != 0 || noneBody.NextSince != 0 {
		t.Fatalf("unmatched kind = %+v, want empty page with cursor 0", noneBody)
	}
}

func TestEventsEndpointValidatesQuery(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	path := "/v2/projects/" + fixture.Project.ID + "/events"

	for _, query := range []string{"?since=nope", "?since=-1", "?limit=0", "?limit=-3", "?limit=lots", "?kind=", "?task=", "?actor="} {
		response := httptest.NewRecorder()
		fixture.Server.ServeHTTP(response, authorizedRequest(http.MethodGet, path+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", query, response.Code)
		}
		var body errorResponse
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s response: %v", query, err)
		}
		if body.Error.Code != "invalid_query" {
			t.Fatalf("%s error code = %q, want invalid_query", query, body.Error.Code)
		}
	}
}

func TestEventsEndpointScopeEnforcement(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	path := "/v2/projects/" + fixture.Project.ID + "/events"

	hookRequest := httptest.NewRequest(http.MethodGet, path, nil)
	hookRequest.Header.Set("Authorization", "Bearer hook-token")
	hookRequest.Header.Set(protocolHeader, contract.ProtocolVersion)
	hookResponse := httptest.NewRecorder()
	fixture.Server.ServeHTTP(hookResponse, hookRequest)
	if hookResponse.Code != http.StatusForbidden {
		t.Fatalf("hook token status = %d, want 403", hookResponse.Code)
	}

	anonResponse := httptest.NewRecorder()
	fixture.Server.ServeHTTP(anonResponse, httptest.NewRequest(http.MethodGet, path, nil))
	if anonResponse.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", anonResponse.Code)
	}
}

// sseRecorder is a race-safe streaming ResponseWriter: httptest.ResponseRecorder
// cannot be read while the handler is still writing.
type sseRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	code   int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: make(http.Header)}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = code
	}
}

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *sseRecorder) Flush() {}

func (r *sseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *sseRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}

func TestEventsStreamDeliversAppendedEvents(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	task, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Streamed task", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	recorder := newSSERecorder()
	request := authorizedRequest(http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/events/stream", nil).WithContext(streamCtx)
	done := make(chan struct{})
	go func() {
		fixture.Server.ServeHTTP(recorder, request)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(recorder.String(), "data: ") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("stream produced no event frame within 5s; body so far: %q", recorder.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler did not return after context cancel")
	}

	if status := recorder.status(); status != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", status)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
	body := recorder.String()
	if !strings.Contains(body, "id: 1\ndata: ") {
		t.Fatalf("first frame missing id/data pair: %q", body)
	}
	if !strings.Contains(body, string(coordinator.EventTaskCreated)) || !strings.Contains(body, task.ID) {
		t.Fatalf("stream missing task.created frame for %s: %q", task.ID, body)
	}
}
