package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunSearch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_OWNER_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"task", "create", "--title", "Searchable needle"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"search", "needle"}, &stdout, &stderr); code != 0 {
		t.Fatalf("search exitCode = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Searchable needle") {
		t.Fatalf("search output = %q, want the created task", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--agent", "search", "needle"}, &stdout, &stderr); code != 0 {
		t.Fatalf("search --agent exitCode = %d, stderr = %q", code, stderr.String())
	}
	var envelope struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Data    struct {
			Tasks []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode search envelope: %v; output = %q", err, stdout.String())
	}
	if envelope.Command != "search" || !envelope.OK || len(envelope.Data.Tasks) != 1 {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Data.Tasks[0].Title != "Searchable needle" {
		t.Fatalf("hit = %+v", envelope.Data.Tasks[0])
	}

	// usage: no query
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"search"}, &stdout, &stderr); code != 2 {
		t.Fatalf("search with no query exitCode = %d, want 2", code)
	}
}
