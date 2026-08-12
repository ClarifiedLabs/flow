package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
)

// The task-scoped threads read feeds the web UI's Threads tab: every review
// thread across the task's changes, each with its full comment timeline. The
// per-change endpoint stays the worker session-token surface; this one is the
// owner/console/web-session project read.
func TestTaskThreadsEndpoint(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Threads endpoint target"}, http.StatusCreated, &created)
	taskID := created.Task.ID

	const (
		oldChange = "ch-threads-old"
		newChange = "ch-threads-new"
		oldHead   = "1111111111111111111111111111111111111111"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	for _, change := range []struct {
		id     string
		branch string
	}{
		{oldChange, "task/threads-old"},
		{newChange, "task/threads-new"},
	} {
		if _, err := fixture.DB.Exec(`INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at) VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`,
			change.id, taskID, change.branch, oldHead, timestamp, timestamp, timestamp); err != nil {
			t.Fatalf("insert change %s: %v", change.id, err)
		}
	}

	createThread := func(changeID string, file string, line int, body string) string {
		t.Helper()
		var response threadResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/comments",
			createThreadRequest{AnchorCommitSHA: oldHead, FilePath: file, Line: line, Body: body},
			http.StatusCreated, &response)
		return response.Thread.ID
	}
	// One thread per change, and a follow-up comment on the older one so the
	// timeline has to travel with the read.
	oldID := createThread(oldChange, "a.go", 1, "old round: buffer overflow")
	newID := createThread(newChange, "b.go", 2, "new round: missing nil check")
	var commented threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/threads/"+oldID+"/comments",
		threadCommentRequest{Body: "looking into it"}, http.StatusOK, &commented)

	var listed threadsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+taskID+"/threads",
		nil, http.StatusOK, &listed)

	if len(listed.Threads) != 2 {
		t.Fatalf("task threads = %d, want 2 across both changes: %+v", len(listed.Threads), listed.Threads)
	}
	if listed.Threads[0].ID != oldID || listed.Threads[1].ID != newID {
		t.Fatalf("task thread order = [%s %s], want creation order [%s %s]",
			listed.Threads[0].ID, listed.Threads[1].ID, oldID, newID)
	}
	if listed.Threads[0].ChangeID != oldChange || listed.Threads[1].ChangeID != newChange {
		t.Fatalf("task thread changes = [%s %s], want [%s %s]",
			listed.Threads[0].ChangeID, listed.Threads[1].ChangeID, oldChange, newChange)
	}
	if len(listed.Threads[0].Comments) != 2 || listed.Threads[0].Comments[1].Body != "looking into it" {
		t.Fatalf("old thread comments = %+v, want the opening body plus the follow-up", listed.Threads[0].Comments)
	}
	if len(listed.Threads[1].Comments) != 1 {
		t.Fatalf("new thread comments = %+v, want the opening comment", listed.Threads[1].Comments)
	}

	// The project-scoped path the web UI calls serves the same read.
	var projectListed threadsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v2/projects/"+fixture.Project.ID+"/tasks/"+taskID+"/threads", nil, http.StatusOK, &projectListed)
	if len(projectListed.Threads) != 2 {
		t.Fatalf("project-scoped task threads = %d, want 2: %+v", len(projectListed.Threads), projectListed.Threads)
	}

	// Worker scope is not a task-threads reader: worker session tokens keep the
	// per-change endpoint whose change-binding they rely on.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/tasks/"+taskID+"/threads",
		nil, http.StatusForbidden, nil)

	// Unknown task id → 404 through task routing.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/t-does-not-exist/threads",
		nil, http.StatusNotFound, nil)
}

// A browser principal (web session cookie + CSRF header) reads the endpoint
// through the /ui/api prefix, the same path the Threads tab fetches.
func TestTaskThreadsEndpointWebSession(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Web-session threads target"}, http.StatusCreated, &created)

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/ui/api/v2/projects/"+fixture.Project.ID+"/tasks/"+created.Task.ID+"/threads", nil)
	request.Header.Set(webCSRFHeader, csrfCookie.Value)
	request.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("web-session task threads status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var listed contract.ThreadsResponse
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatalf("decode web-session task threads: %v", err)
	}
	if len(listed.Threads) != 0 {
		t.Fatalf("web-session task threads = %+v, want none for a threadless task", listed.Threads)
	}
}

func TestTaskThreadsEndpointEmptyTask(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Threads-free target"}, http.StatusCreated, &created)

	var listed threadsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/threads",
		nil, http.StatusOK, &listed)
	if listed.Threads == nil {
		t.Fatal("threads = null, want an empty array so the tab renders its empty state")
	}
	if len(listed.Threads) != 0 {
		t.Fatalf("threads = %+v, want none", listed.Threads)
	}
}

func TestTaskThreadsUnavailable(t *testing.T) {
	t.Parallel()
	server := &projectServer{}
	response := httptest.NewRecorder()
	server.handleTaskThreads(response, httptest.NewRequest(http.MethodGet, "/v2/tasks/t-1/threads", nil), "t-1")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "threads_unavailable") {
		t.Fatalf("body = %s, want threads_unavailable", response.Body.String())
	}
}
