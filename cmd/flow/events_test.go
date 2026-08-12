package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunEventsListsAndPages(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_OWNER_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"task", "create", "--title", "Evented"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"events"}, &stdout, &stderr); code != 0 {
		t.Fatalf("events exitCode = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "task.created") || !strings.Contains(stdout.String(), "Evented") {
		t.Fatalf("human events output = %q, want a task.created line mentioning the title", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"events", "--agent"}, &stdout, &stderr); code != 0 {
		t.Fatalf("events --agent exitCode = %d, stderr = %q", code, stderr.String())
	}
	var envelope struct {
		ContractVersion int  `json:"contract_version"`
		Command         string `json:"command"`
		OK              bool   `json:"ok"`
		Data            struct {
			Events []struct {
				Seq  int64  `json:"seq"`
				Kind string `json:"kind"`
			} `json:"events"`
			NextSince int64 `json:"next_since"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode agent envelope: %v; output = %q", err, stdout.String())
	}
	if envelope.Command != "events" || !envelope.OK {
		t.Fatalf("envelope command = %q ok = %v, want events/true", envelope.Command, envelope.OK)
	}
	if len(envelope.Data.Events) == 0 {
		t.Fatalf("agent events = [], want the task.created event")
	}
	if envelope.Data.Events[0].Kind != "task.created" {
		t.Fatalf("first event kind = %q, want task.created", envelope.Data.Events[0].Kind)
	}
	if envelope.Data.NextSince != envelope.Data.Events[len(envelope.Data.Events)-1].Seq {
		t.Fatalf("next_since = %d, want last seq %d", envelope.Data.NextSince, envelope.Data.Events[len(envelope.Data.Events)-1].Seq)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"events", "--json", "--since", "1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("events --since exitCode = %d, stderr = %q", code, stderr.String())
	}
	var page struct {
		Events    []json.RawMessage `json:"events"`
		NextSince int64             `json:"next_since"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &page); err != nil {
		t.Fatalf("decode json page: %v; output = %q", err, stdout.String())
	}
	if len(page.Events) != 0 {
		t.Fatalf("since=1 events = %d, want 0 (only one event exists)", len(page.Events))
	}
	if page.NextSince != 1 {
		t.Fatalf("since=1 next_since = %d, want 1 (empty page echoes the cursor)", page.NextSince)
	}
}

func TestRunEventsRejectsPositionalArgs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"events", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("events with positional arg exitCode = %d, want 2", code)
	}
}
