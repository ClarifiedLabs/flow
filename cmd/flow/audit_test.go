package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskDoneWithEvidenceAndAudit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_OWNER_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"task", "create", "--title", "Evidence task"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"task", "done", "--message", "shipped it", "--evidence", "commit:abc123", "--evidence", "test:go test ./...", "t-demo-0001"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task done exitCode = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001") {
		t.Fatalf("task done output = %q", stdout.String())
	}

	// audit completions human output shows the evidence + message.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"audit", "completions"}, &stdout, &stderr); code != 0 {
		t.Fatalf("audit completions exitCode = %d, stderr = %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"t-demo-0001", "completed", "evidence=2", "shipped it", "commit: abc123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}

	// audit completions --agent envelope.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--agent", "audit", "completions", "--resolution", "completed"}, &stdout, &stderr); code != 0 {
		t.Fatalf("audit --agent exitCode = %d, stderr = %q", code, stderr.String())
	}
	var envelope struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Data    struct {
			Completions []struct {
				ID           string `json:"id"`
				DoneMessage  string `json:"done_message"`
				DoneEvidence []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"done_evidence"`
			} `json:"completions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode audit envelope: %v; output = %q", err, stdout.String())
	}
	if envelope.Command != "audit completions" || !envelope.OK || len(envelope.Data.Completions) != 1 {
		t.Fatalf("envelope = %+v", envelope)
	}
	c := envelope.Data.Completions[0]
	if c.DoneMessage != "shipped it" || len(c.DoneEvidence) != 2 || c.DoneEvidence[0].Type != "commit" {
		t.Fatalf("completion = %+v", c)
	}
}

func TestTaskDoneRejectsBadEvidence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// malformed evidence (no colon) is a usage error before any network call.
	if code := run([]string{"task", "done", "--evidence", "nocolon", "t-demo-0001"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad evidence exitCode = %d, want 2", code)
	}
}
