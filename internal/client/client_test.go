package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func newClientForTest(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(config.ClientConfig{
		ServerURL: server.URL,
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestHistoryClientListsAndPublishesWithTypedContracts(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/projects/p-history/history/captures", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("job_id") != "job-1" || r.URL.Query().Get("limit") != "25" {
			t.Fatalf("history list request = %s %s", r.Method, r.URL.String())
		}
		writeJSON(t, w, http.StatusOK, contract.HistoryCapturesResponse{
			Captures: []contract.HistoryCapture{{ID: "hc-1", JobID: "job-1"}}, SnapshotUntil: "2026-08-03T00:00:00Z",
		})
	})
	mux.HandleFunc("/v2/history/captures/hc-1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Flow-History-Upload-Grant") != "grant-1" || r.Header.Get(contract.ProtocolHeader) != contract.ProtocolVersion {
			t.Fatalf("history upload headers = %v", r.Header)
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(r.Body); err != nil || body.String() != "history bytes" {
			t.Fatalf("history upload body = %q err=%v", body.String(), err)
		}
		writeJSON(t, w, http.StatusCreated, contract.HistoryUploadResponse{TemporaryUploadID: "temporary-1", SHA256: "digest-1", StoredSize: 13})
	})
	mux.HandleFunc("/v2/history/captures/hc-1/uploads/temporary-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("Flow-History-Upload-Grant") != "grant-1" || r.Header.Get(contract.ProtocolHeader) != contract.ProtocolVersion {
			t.Fatalf("history upload abandon request = %s headers=%v", r.Method, r.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v2/history/captures/hc-1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Flow-History-Upload-Grant") != "grant-1" {
			t.Fatalf("history publication grant = %q", r.Header.Get("Flow-History-Upload-Grant"))
		}
		var input contract.PublishHistoryArtifactRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.TemporaryUploadID != "temporary-1" || input.LogicalKey != "workspace/final" {
			t.Fatalf("history publication input = %+v err=%v", input, err)
		}
		writeJSON(t, w, http.StatusCreated, contract.HistoryArtifact{ID: "artifact-1", SHA256: "digest-1", PublicationState: "committed"})
	})
	client := newClientForTest(t, mux).WithProject("p-history")

	listed, err := client.ListHistoryCaptures(context.Background(), HistoryCaptureFilter{JobIDs: []string{"job-1"}, Limit: 25})
	if err != nil || len(listed.Captures) != 1 || listed.Captures[0].ID != "hc-1" {
		t.Fatalf("list history = %+v err=%v", listed, err)
	}
	upload, err := client.UploadHistoryArtifactBytes(context.Background(), "hc-1", "grant-1", bytes.NewBufferString("history bytes"))
	if err != nil || upload.TemporaryUploadID != "temporary-1" {
		t.Fatalf("upload history = %+v err=%v", upload, err)
	}
	if err := client.AbandonHistoryArtifactUpload(context.Background(), "hc-1", "grant-1", upload.TemporaryUploadID); err != nil {
		t.Fatalf("abandon history upload: %v", err)
	}
	artifact, err := client.PublishHistoryArtifact(context.Background(), "hc-1", "grant-1", contract.PublishHistoryArtifactRequest{
		TemporaryUploadID: upload.TemporaryUploadID, LogicalKey: "workspace/final",
	})
	if err != nil || artifact.ID != "artifact-1" {
		t.Fatalf("publish history = %+v err=%v", artifact, err)
	}
}

func TestListTaskAttachments(t *testing.T) {
	t.Parallel()
	want := []coordinator.TaskAttachment{
		{ID: "att-0001", TaskID: "t-test-0001", Filename: "shot.png", ContentType: "image/png"},
		{ID: "att-0002", TaskID: "t-test-0001", Filename: "notes.txt", ContentType: "text/plain"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/tasks/t-test-0001/attachments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header = %q", got)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"attachments": want,
		})
	})
	client := newClientForTest(t, mux)

	got, err := client.ListTaskAttachments(context.Background(), "t-test-0001")
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("attachments = %+v, want %+v", got, want)
	}
	for i, attachment := range want {
		if got[i].ID != attachment.ID || got[i].Filename != attachment.Filename || got[i].ContentType != attachment.ContentType {
			t.Fatalf("attachment %d = %+v, want %+v", i, got[i], attachment)
		}
	}
}

func TestProvisionerAssignmentClient(t *testing.T) {
	t.Parallel()
	assignment := contract.ProvisionerAssignment{
		Project:    contract.Project{ID: "p-client", Name: "Client Project"},
		Assignment: flowworker.Assignment{ID: "a/client", WorkerID: "w-client", State: flowworker.AssignmentPending},
	}
	nextRetry := time.Date(2026, 8, 2, 18, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/provisioner/assignments/reserve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(contract.ProtocolHeader) != contract.ProtocolVersion {
			t.Fatalf("reserve request method/header = %s/%q", r.Method, r.Header.Get(contract.ProtocolHeader))
		}
		var request contract.ReserveProvisionerAssignmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode reserve: %v", err)
		}
		if request.ProviderRequestID != "request-client" || request.StartupTimeoutSeconds != 45 || request.WaitSeconds != 2 {
			t.Fatalf("reserve request = %+v", request)
		}
		writeJSON(t, w, http.StatusOK, contract.ReserveProvisionerAssignmentResponse{Reserved: true, Assignment: &assignment, WorkerToken: "direct-worker-token"})
	})
	mux.HandleFunc("/v2/provisioner/assignments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("provider_id") != "kubernetes" || r.URL.Query().Get("state") != "pending" || r.URL.Query().Get("open_only") != "true" {
			t.Fatalf("list request = %s %s", r.Method, r.URL.String())
		}
		writeJSON(t, w, http.StatusOK, contract.ProvisionerAssignmentsResponse{Assignments: []contract.ProvisionerAssignment{assignment}})
	})
	mux.HandleFunc("/v2/provisioner/assignments/a%2Fclient/attempt", func(w http.ResponseWriter, r *http.Request) {
		var request contract.RecordProvisionerAssignmentAttemptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode attempt: %v", err)
		}
		if request.ProviderError != "warming" || request.NextRetryAt == nil || !request.NextRetryAt.Equal(nextRetry) {
			t.Fatalf("attempt request = %+v", request)
		}
		writeJSON(t, w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: assignment})
	})
	mux.HandleFunc("/v2/provisioner/assignments/a%2Fclient/abandon", func(w http.ResponseWriter, r *http.Request) {
		var request contract.AbandonProvisionerAssignmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ProviderError != "launch failed" {
			t.Fatalf("abandon request = %+v, err=%v", request, err)
		}
		writeJSON(t, w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: assignment})
	})
	mux.HandleFunc("/v2/provisioner/assignments/a%2Fclient/cleaned", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, contract.ProvisionerAssignmentResponse{Assignment: assignment})
	})
	client := newClientForTest(t, mux)

	reserved, err := client.ReserveProvisionerAssignment(context.Background(), ReserveProvisionerAssignmentInput{
		ProviderID: "kubernetes", ProviderRequestID: "request-client", ProfileName: "linux",
		MaxConcurrency: 3, StartupTimeout: 45 * time.Second, Wait: 2 * time.Second,
	})
	if err != nil || !reserved.Reserved || reserved.Assignment == nil || reserved.WorkerToken != "direct-worker-token" {
		t.Fatalf("reserve result = %+v, err=%v", reserved, err)
	}
	listed, err := client.ListProvisionerAssignments(context.Background(), ProvisionerAssignmentFilter{
		ProviderID: "kubernetes", States: []flowworker.AssignmentState{flowworker.AssignmentPending}, OpenOnly: true,
	})
	if err != nil || len(listed) != 1 || listed[0].Assignment.ID != assignment.Assignment.ID {
		t.Fatalf("list result = %+v, err=%v", listed, err)
	}
	if _, err := client.RecordProvisionerAssignmentAttempt(context.Background(), assignment.Assignment.ID, RecordProvisionerAssignmentAttemptInput{ProviderError: "warming", NextRetryAt: &nextRetry}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if _, err := client.AbandonProvisionerAssignment(context.Background(), assignment.Assignment.ID, "launch failed"); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if _, err := client.MarkProvisionerAssignmentCleaned(context.Background(), assignment.Assignment.ID); err != nil {
		t.Fatalf("mark cleaned: %v", err)
	}
}

func TestJoinWorker(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/workers/join", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header = %q", got)
		}
		var request joinWorkerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.WorkerID != "w-local" {
			t.Fatalf("worker_id = %q, want w-local", request.WorkerID)
		}
		writeJSON(t, w, http.StatusOK, joinWorkerResponse{
			WorkerID: "w-local",
			Token:    "worker-token",
		})
	})
	client := newClientForTest(t, mux)

	joined, err := client.JoinWorker(JoinWorkerInput{WorkerID: "w-local"})
	if err != nil {
		t.Fatalf("join worker: %v", err)
	}
	if joined.WorkerID != "w-local" || joined.Token != "worker-token" {
		t.Fatalf("joined = %+v", joined)
	}
}

func TestListTaskAttachmentsScopedToProject(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/projects/p-test/tasks/t-test-0001/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"attachments": []coordinator.TaskAttachment{
			{ID: "att-0001", TaskID: "t-test-0001", Filename: "shot.png"},
		}})
	})
	client := newClientForTest(t, mux).WithProject("p-test")

	got, err := client.ListTaskAttachments(context.Background(), "t-test-0001")
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(got) != 1 || got[0].ID != "att-0001" {
		t.Fatalf("attachments = %+v, want att-0001", got)
	}
}

func TestListTaskAttachmentsSurfacesErrorStatus(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/tasks/t-test-9999/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "task_not_found", "message": "task not found"},
		})
	})
	client := newClientForTest(t, mux)

	_, err := client.ListTaskAttachments(context.Background(), "t-test-9999")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound || statusErr.Code != "task_not_found" {
		t.Fatalf("err = %v, want HTTPStatusError 404 task_not_found", err)
	}
}

func TestDownloadTaskAttachment(t *testing.T) {
	t.Parallel()
	want := []byte("png-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/tasks/t-test-0001/attachments/att-0001", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(want)
	})
	client := newClientForTest(t, mux)

	var buf bytes.Buffer
	if err := client.DownloadTaskAttachment(context.Background(), "t-test-0001", "att-0001", &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("downloaded bytes = %q, want %q", buf.String(), string(want))
	}
}

func TestDownloadTaskAttachmentSurfacesErrorStatus(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/tasks/t-test-0001/attachments/att-missing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "attachment_not_found", "message": "attachment not found"},
		})
	})
	client := newClientForTest(t, mux)

	var buf bytes.Buffer
	err := client.DownloadTaskAttachment(context.Background(), "t-test-0001", "att-missing", &buf)
	if err == nil {
		t.Fatal("expected error for missing attachment")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound || statusErr.Code != "attachment_not_found" {
		t.Fatalf("err = %v, want HTTPStatusError 404 attachment_not_found", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("buffer = %q, want empty on error", buf.String())
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
