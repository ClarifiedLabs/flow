package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func newClientForTest(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(config.ClientConfig{
		ServerURL:       server.URL,
		Token:           "test-token",
		ProtocolVersion: config.DefaultProtocolVersion,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestListIssueAttachments(t *testing.T) {
	t.Parallel()
	want := []coordinator.IssueAttachment{
		{ID: "att-0001", IssueID: "i-0001", Filename: "shot.png", ContentType: "image/png"},
		{ID: "att-0002", IssueID: "i-0001", Filename: "notes.txt", ContentType: "text/plain"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/issues/i-0001/attachments", func(w http.ResponseWriter, r *http.Request) {
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

	got, err := client.ListIssueAttachments(context.Background(), "i-0001")
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

func TestJoinWorker(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workers/join", func(w http.ResponseWriter, r *http.Request) {
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

func TestListIssueAttachmentsScopedToProject(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/proj-1/issues/i-0001/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"attachments": []coordinator.IssueAttachment{
			{ID: "att-0001", IssueID: "i-0001", Filename: "shot.png"},
		}})
	})
	client := newClientForTest(t, mux).WithProject("proj-1")

	got, err := client.ListIssueAttachments(context.Background(), "i-0001")
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(got) != 1 || got[0].ID != "att-0001" {
		t.Fatalf("attachments = %+v, want att-0001", got)
	}
}

func TestListIssueAttachmentsSurfacesErrorStatus(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/issues/i-missing/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "issue_not_found", "message": "issue not found"},
		})
	})
	client := newClientForTest(t, mux)

	_, err := client.ListIssueAttachments(context.Background(), "i-missing")
	if err == nil {
		t.Fatal("expected error for missing issue")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound || statusErr.Code != "issue_not_found" {
		t.Fatalf("err = %v, want HTTPStatusError 404 issue_not_found", err)
	}
}

func TestDownloadIssueAttachment(t *testing.T) {
	t.Parallel()
	want := []byte("png-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/issues/i-0001/attachments/att-0001", func(w http.ResponseWriter, r *http.Request) {
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
	if err := client.DownloadIssueAttachment(context.Background(), "i-0001", "att-0001", &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("downloaded bytes = %q, want %q", buf.String(), string(want))
	}
}

func TestDownloadIssueAttachmentSurfacesErrorStatus(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/issues/i-0001/attachments/att-missing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "attachment_not_found", "message": "attachment not found"},
		})
	})
	client := newClientForTest(t, mux)

	var buf bytes.Buffer
	err := client.DownloadIssueAttachment(context.Background(), "i-0001", "att-missing", &buf)
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
