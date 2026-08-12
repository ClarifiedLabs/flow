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

func TestEventsEndpointValidatesQuery(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	path := "/v2/projects/" + fixture.Project.ID + "/events"

	for _, query := range []string{"?since=nope", "?since=-1", "?limit=0", "?limit=-3", "?limit=lots"} {
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
