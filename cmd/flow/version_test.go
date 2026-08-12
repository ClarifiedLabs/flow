package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunVersionHumanAndMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exitCode = %d, stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "flow ") {
		t.Fatalf("human version output = %q, want prefix \"flow \"", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--agent", "version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version --agent exitCode = %d, stderr = %q", code, stderr.String())
	}
	var envelope struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Data    struct {
			Version     string `json:"version"`
			AgentFormat int    `json:"agent_format"`
			Protocol    string `json:"protocol"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode version envelope: %v; output = %q", err, stdout.String())
	}
	if envelope.Command != "version" || !envelope.OK {
		t.Fatalf("envelope = %+v, want version/ok", envelope)
	}
	if envelope.Data.AgentFormat != 1 {
		t.Fatalf("agent_format = %d, want 1", envelope.Data.AgentFormat)
	}
	if envelope.Data.Protocol == "" {
		t.Fatalf("protocol is empty, want the API protocol version")
	}
	if envelope.Data.Version == "" {
		t.Fatalf("version is empty")
	}
}
