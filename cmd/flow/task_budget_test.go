package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTaskBudgetRequiresInstructionsFlag proves `flow task budget` refuses to
// run without the operator rationale: extending a review budget invisibly is
// exactly the failure mode the flag exists to prevent.
func TestTaskBudgetRequiresInstructionsFlag(t *testing.T) {
	t.Parallel()

	var captured struct {
		body string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		captured.body = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run":{"task_id":"t-x","state":"running"}}`))
	}))
	defer server.Close()

	args := []string{"--server", server.URL, "--token", "owner-token", "--additional", "2"}
	var stderr bytes.Buffer
	if code := runTaskBudget(append([]string{"t-test-0001"}, args...), &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("exit code without --instructions = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--instructions") {
		t.Fatalf("stderr = %q, want a usage line naming --instructions", stderr.String())
	}
	if captured.body != "" {
		t.Fatalf("request body = %q, want no request without instructions", captured.body)
	}
}
