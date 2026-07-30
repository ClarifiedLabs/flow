package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--help) exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow-orchestrator") {
		t.Fatalf("help output missing usage:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow-orchestrator") {
		t.Fatalf("version output:\n%s", stdout.String())
	}
}

func TestRunRequiresToken(t *testing.T) {
	// testenv clears the environment, so FLOW_ORCHESTRATOR_TOKEN is unset and
	// the default config carries no token.
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run() without token exit = %d, want 1; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "orchestrator token is required") {
		t.Fatalf("stderr missing token error: %s", stderr.String())
	}
}
