package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// putTranscriptAs tasks a raw-body PUT with the given bearer token and returns
// the recorder.
func putTranscriptAs(t *testing.T, server *Server, token string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "text/plain")
	server.ServeHTTP(response, request)
	return response
}

func getAs(t *testing.T, server *Server, token string, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	server.ServeHTTP(response, request)
	return response
}

func TestSessionTranscriptUploadAndDownload(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Transcript session")
	sessionID := started.Session.ID

	content := "author session pane output\nsecond line\n"
	upload := putTranscriptAs(t, fixture.Server, started.Token, "/v2/sessions/"+sessionID+"/transcript", content)
	if upload.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204; body: %s", upload.Code, upload.Body.String())
	}

	// The coordinator records the on-disk path on the session.
	session, err := fixture.Sessions.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.TranscriptPath == "" {
		t.Fatalf("session transcript path was not recorded")
	}

	// Owner can download as text/plain.
	download := getAs(t, fixture.Server, "owner-token", "/v2/sessions/"+sessionID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if ct := download.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("download content-type = %q, want text/plain", ct)
	}
	if download.Body.String() != content {
		t.Fatalf("download body = %q, want %q", download.Body.String(), content)
	}
}

func TestSessionTranscriptDownloadRendersTerminalOutputAndPreservesRawStorage(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Rendered transcript")
	sessionID := started.Session.ID

	raw := "working 10%\rworking 100%\x1b[K\n\x1b[32mcomplete\x1b[0m\n"
	upload := putTranscriptAs(t, fixture.Server, started.Token, "/v2/sessions/"+sessionID+"/transcript", raw)
	if upload.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204; body: %s", upload.Code, upload.Body.String())
	}

	download := getAs(t, fixture.Server, "owner-token", "/v2/sessions/"+sessionID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	const want = "working 100%\ncomplete\n"
	if download.Body.String() != want {
		t.Fatalf("download body = %q, want rendered %q", download.Body.String(), want)
	}

	stored, err := fixture.Bundle.Transcripts.Open(sessionID)
	if err != nil {
		t.Fatalf("open raw transcript: %v", err)
	}
	defer stored.Close()
	storedBytes, err := io.ReadAll(stored)
	if err != nil {
		t.Fatalf("read raw transcript: %v", err)
	}
	if string(storedBytes) != raw {
		t.Fatalf("stored transcript = %q, want raw bytes %q", storedBytes, raw)
	}
}

func TestSessionTranscriptUploadRejectsOtherSession(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	owner := startAuthorSessionForStatusTestWithWorker(t, fixture, "Owner session", "w-owner")
	other := startAuthorSessionForStatusTestWithWorker(t, fixture, "Other session", "w-other")

	// other's session token may not upload to owner's session.
	response := putTranscriptAs(t, fixture.Server, other.Token, "/v2/sessions/"+owner.Session.ID+"/transcript", "nope")
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-session upload status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptDownloadRequiresOwner(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Owner-only download")
	sessionID := started.Session.ID

	// Seed a transcript so a 200 is otherwise possible.
	if up := putTranscriptAs(t, fixture.Server, started.Token, "/v2/sessions/"+sessionID+"/transcript", "data"); up.Code != http.StatusNoContent {
		t.Fatalf("seed upload status = %d; body: %s", up.Code, up.Body.String())
	}

	// The owning session token may not GET its transcript.
	response := getAs(t, fixture.Server, started.Token, "/v2/sessions/"+sessionID+"/transcript")
	if response.Code != http.StatusForbidden {
		t.Fatalf("session-token download status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptDownloadMissing(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "No transcript yet")

	response := getAs(t, fixture.Server, "owner-token", "/v2/sessions/"+started.Session.ID+"/transcript")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing transcript status = %d, want 404; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptUploadRequiresLiveLease(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	task, change := seedTaskWithChange(t, fixture, "Job transcript task")
	claimed := startLiveCheckJobForTask(t, fixture, "worker-token-rev", "w-rev", task.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	content := "checking 1%\rchecking 100%\x1b[K\n\x1b[36mreview passed\x1b[0m\n"
	path := "/v2/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	upload := putTranscriptAs(t, fixture.Server, "worker-token-rev", path, content)
	if upload.Code != http.StatusNoContent {
		t.Fatalf("job upload status = %d, want 204; body: %s", upload.Code, upload.Body.String())
	}

	job, err := fixture.Workers.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TranscriptPath == "" {
		t.Fatalf("job transcript path was not recorded")
	}

	// Owner download receives terminal-rendered plain text.
	download := getAs(t, fixture.Server, "owner-token", "/v2/jobs/"+jobID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("job download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	const want = "checking 100%\nreview passed\n"
	if download.Body.String() != want {
		t.Fatalf("job download body = %q, want rendered %q", download.Body.String(), want)
	}
}

func TestJobTranscriptUploadRejectsMissingLease(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	task, change := seedTaskWithChange(t, fixture, "Job transcript no-lease")
	claimed := startLiveCheckJobForTask(t, fixture, "worker-token-rev2", "w-rev2", task.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	// No lease_id query param -> rejected.
	response := putTranscriptAs(t, fixture.Server, "worker-token-rev2", "/v2/jobs/"+jobID+"/transcript", "x")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing-lease upload status = %d, want 400; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptUploadRejectsReleasedLease(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, change := seedTaskWithChange(t, fixture, "Job transcript released-lease")
	claimed := startLiveCheckJobForTask(t, fixture, "worker-token-rev3", "w-rev3", task.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	if _, err := fixture.Workers.ReleaseLease(ctx, claimed.Lease.ID, flowworker.JobFinished); err != nil {
		t.Fatalf("release lease: %v", err)
	}

	path := "/v2/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	response := putTranscriptAs(t, fixture.Server, "worker-token-rev3", path, "x")
	if response.Code != http.StatusForbidden {
		t.Fatalf("released-lease upload status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptDownloadServesTextPlain(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	task, change := seedTaskWithChange(t, fixture, "Job transcript content type")
	claimed := startLiveCheckJobForTask(t, fixture, "worker-token-ct", "w-ct", task.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	path := "/v2/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	if up := putTranscriptAs(t, fixture.Server, "worker-token-ct", path, "reviewer pane output\n"); up.Code != http.StatusNoContent {
		t.Fatalf("seed job upload status = %d; body: %s", up.Code, up.Body.String())
	}

	download := getAs(t, fixture.Server, "owner-token", "/v2/jobs/"+jobID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("job download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if ct := download.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("job download content-type = %q, want text/plain", ct)
	}
}

func TestJobTranscriptDownloadRequiresOwner(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	task, change := seedTaskWithChange(t, fixture, "Job transcript owner-only download")
	claimed := startLiveCheckJobForTask(t, fixture, "worker-token-own", "w-own", task.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	path := "/v2/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	if up := putTranscriptAs(t, fixture.Server, "worker-token-own", path, "data"); up.Code != http.StatusNoContent {
		t.Fatalf("seed job upload status = %d; body: %s", up.Code, up.Body.String())
	}

	// A worker token (even holding the lease) may not download the transcript.
	response := getAs(t, fixture.Server, "worker-token-own", "/v2/jobs/"+jobID+"/transcript")
	if response.Code != http.StatusForbidden {
		t.Fatalf("worker-token download status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptUploadTruncatesToLast10MB(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Big transcript")
	sessionID := started.Session.ID

	// Upload just over the cap; only the trailing bytes are retained.
	head := strings.Repeat("H", 4096)
	tail := strings.Repeat("T", 64)
	// Pad so the total exceeds 10MB while keeping the tail recognizable.
	pad := strings.Repeat("P", (10<<20)-len(tail)+1)
	full := head + pad + tail

	up := putTranscriptAs(t, fixture.Server, started.Token, "/v2/sessions/"+sessionID+"/transcript", full)
	if up.Code != http.StatusNoContent {
		t.Fatalf("big upload status = %d; body: %s", up.Code, up.Body.String())
	}

	download := getAs(t, fixture.Server, "owner-token", "/v2/sessions/"+sessionID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d", download.Code)
	}
	got, err := io.ReadAll(download.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if len(got) != 10<<20 {
		t.Fatalf("stored transcript size = %d, want %d", len(got), 10<<20)
	}
	if !strings.HasSuffix(string(got), tail) {
		t.Fatalf("stored transcript did not retain the trailing bytes")
	}
	if strings.HasPrefix(string(got), head) {
		t.Fatalf("stored transcript retained the dropped head")
	}
}

// seedTaskWithChange creates a ready change directly so transcript endpoint
// tests stay independent of any particular workflow graph.
func seedTaskWithChange(t *testing.T, fixture testFixture, title string) (coordinator.Task, coordinator.Change) {
	t.Helper()
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: title})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	const changeID = "ch-transcript"
	const stamp = "2026-01-01T00:00:00Z"
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`, changeID, task.ID, "task/transcript", "deadbeef", stamp, stamp, stamp); err != nil {
		t.Fatalf("insert transcript change: %v", err)
	}
	change, err := fixture.Sessions.GetChange(ctx, changeID)
	if err != nil {
		t.Fatalf("load transcript change: %v", err)
	}
	return task, change
}
