package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// putTranscriptAs issues a raw-body PUT with the given bearer token and returns
// the recorder.
func putTranscriptAs(t *testing.T, server *Server, token string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(protocolHeader, "1")
	request.Header.Set("Content-Type", "text/plain")
	server.ServeHTTP(response, request)
	return response
}

func getAs(t *testing.T, server *Server, token string, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(protocolHeader, "1")
	server.ServeHTTP(response, request)
	return response
}

func TestSessionTranscriptUploadAndDownload(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Transcript session")
	sessionID := started.Session.ID

	content := "author session pane output\nsecond line\n"
	upload := putTranscriptAs(t, fixture.Server, started.Token, "/v1/sessions/"+sessionID+"/transcript", content)
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
	download := getAs(t, fixture.Server, "owner-token", "/v1/sessions/"+sessionID+"/transcript")
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

func TestSessionTranscriptUploadRejectsOtherSession(t *testing.T) {
	fixture := newTestFixture(t)
	owner := startAuthorSessionForStatusTestWithWorker(t, fixture, "Owner session", "w-owner")
	other := startAuthorSessionForStatusTestWithWorker(t, fixture, "Other session", "w-other")

	// other's session token may not upload to owner's session.
	response := putTranscriptAs(t, fixture.Server, other.Token, "/v1/sessions/"+owner.Session.ID+"/transcript", "nope")
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-session upload status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptDownloadRequiresOwner(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Owner-only download")
	sessionID := started.Session.ID

	// Seed a transcript so a 200 is otherwise possible.
	if up := putTranscriptAs(t, fixture.Server, started.Token, "/v1/sessions/"+sessionID+"/transcript", "data"); up.Code != http.StatusNoContent {
		t.Fatalf("seed upload status = %d; body: %s", up.Code, up.Body.String())
	}

	// The owning session token may not GET its transcript.
	response := getAs(t, fixture.Server, started.Token, "/v1/sessions/"+sessionID+"/transcript")
	if response.Code != http.StatusForbidden {
		t.Fatalf("session-token download status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptDownloadMissing(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "No transcript yet")

	response := getAs(t, fixture.Server, "owner-token", "/v1/sessions/"+started.Session.ID+"/transcript")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing transcript status = %d, want 404; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptUploadRequiresLiveLease(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	issue, change := seedIssueWithChange(t, fixture, "Job transcript issue")
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-rev", "w-rev", issue.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	content := "reviewer job pane output\n"
	path := "/v1/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
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

	// Owner download round-trips.
	download := getAs(t, fixture.Server, "owner-token", "/v1/jobs/"+jobID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("job download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if download.Body.String() != content {
		t.Fatalf("job download body = %q, want %q", download.Body.String(), content)
	}
}

func TestJobTranscriptUploadRejectsMissingLease(t *testing.T) {
	fixture := newTestFixture(t)
	issue, change := seedIssueWithChange(t, fixture, "Job transcript no-lease")
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-rev2", "w-rev2", issue.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	// No lease_id query param -> rejected.
	response := putTranscriptAs(t, fixture.Server, "worker-token-rev2", "/v1/jobs/"+jobID+"/transcript", "x")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing-lease upload status = %d, want 400; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptUploadRejectsReleasedLease(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, change := seedIssueWithChange(t, fixture, "Job transcript released-lease")
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-rev3", "w-rev3", issue.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	if _, err := fixture.Workers.ReleaseLease(ctx, claimed.Lease.ID, flowworker.JobFinished); err != nil {
		t.Fatalf("release lease: %v", err)
	}

	path := "/v1/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	response := putTranscriptAs(t, fixture.Server, "worker-token-rev3", path, "x")
	if response.Code != http.StatusForbidden {
		t.Fatalf("released-lease upload status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestJobTranscriptDownloadServesTextPlain(t *testing.T) {
	fixture := newTestFixture(t)
	issue, change := seedIssueWithChange(t, fixture, "Job transcript content type")
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-ct", "w-ct", issue.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	path := "/v1/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	if up := putTranscriptAs(t, fixture.Server, "worker-token-ct", path, "reviewer pane output\n"); up.Code != http.StatusNoContent {
		t.Fatalf("seed job upload status = %d; body: %s", up.Code, up.Body.String())
	}

	download := getAs(t, fixture.Server, "owner-token", "/v1/jobs/"+jobID+"/transcript")
	if download.Code != http.StatusOK {
		t.Fatalf("job download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if ct := download.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("job download content-type = %q, want text/plain", ct)
	}
}

func TestJobTranscriptDownloadRequiresOwner(t *testing.T) {
	fixture := newTestFixture(t)
	issue, change := seedIssueWithChange(t, fixture, "Job transcript owner-only download")
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-own", "w-own", issue.ID, change.ID, "", "reviewer-check", flowworker.RoleReviewer, flowworker.BucketEphemeral)
	jobID := claimed.Job.ID

	path := "/v1/jobs/" + jobID + "/transcript?lease_id=" + claimed.Lease.ID
	if up := putTranscriptAs(t, fixture.Server, "worker-token-own", path, "data"); up.Code != http.StatusNoContent {
		t.Fatalf("seed job upload status = %d; body: %s", up.Code, up.Body.String())
	}

	// A worker token (even holding the lease) may not download the transcript.
	response := getAs(t, fixture.Server, "worker-token-own", "/v1/jobs/"+jobID+"/transcript")
	if response.Code != http.StatusForbidden {
		t.Fatalf("worker-token download status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
}

func TestSessionTranscriptUploadTruncatesToLast10MB(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Big transcript")
	sessionID := started.Session.ID

	// Upload just over the cap; only the trailing bytes are retained.
	head := strings.Repeat("H", 4096)
	tail := strings.Repeat("T", 64)
	// Pad so the total exceeds 10MB while keeping the tail recognizable.
	pad := strings.Repeat("P", (10<<20)-len(tail)+1)
	full := head + pad + tail

	up := putTranscriptAs(t, fixture.Server, started.Token, "/v1/sessions/"+sessionID+"/transcript", full)
	if up.Code != http.StatusNoContent {
		t.Fatalf("big upload status = %d; body: %s", up.Code, up.Body.String())
	}

	download := getAs(t, fixture.Server, "owner-token", "/v1/sessions/"+sessionID+"/transcript")
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

// seedIssueWithChange creates an issue and a ready change for it so a check job
// can be enqueued and addressed by change id.
func seedIssueWithChange(t *testing.T, fixture testFixture, title string) (coordinator.Issue, coordinator.Change) {
	t.Helper()
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: title})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	return issue, ensured.Change
}
