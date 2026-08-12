package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// testEnvelope mirrors cliout.Envelope for decoding; keeping a local copy
// pins the wire shape the tests assert (contract v1).
type testEnvelope struct {
	ContractVersion int             `json:"contract_version"`
	Command         string          `json:"command"`
	OK              bool            `json:"ok"`
	Data            json.RawMessage `json:"data"`
	Error           *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, raw string) testEnvelope {
	t.Helper()
	var envelope testEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("output is not an envelope: %v\n%s", err, raw)
	}
	return envelope
}

func runAgentCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestParseGlobalOptionsOutputFlags(t *testing.T) {
	options, rest, err := parseGlobalOptions([]string{"--agent", "board"})
	if err != nil {
		t.Fatalf("parse --agent: %v", err)
	}
	if !options.agentOut || options.jsonOut {
		t.Fatalf("--agent options = %+v", options)
	}
	if len(rest) != 1 || rest[0] != "board" {
		t.Fatalf("rest = %v", rest)
	}

	options, rest, err = parseGlobalOptions([]string{"--json", "--config", "/tmp/c.json", "ready"})
	if err != nil {
		t.Fatalf("parse --json: %v", err)
	}
	if !options.jsonOut || options.agentOut || !options.configSet {
		t.Fatalf("--json options = %+v", options)
	}
	if len(rest) != 1 || rest[0] != "ready" {
		t.Fatalf("rest = %v", rest)
	}

	options, _, err = parseGlobalOptions([]string{"--agent=false", "board"})
	if err != nil {
		t.Fatalf("parse --agent=false: %v", err)
	}
	if options.agentOut {
		t.Fatalf("--agent=false should clear agentOut: %+v", options)
	}

	if _, _, err = parseGlobalOptions([]string{"--json=maybe", "board"}); err == nil {
		t.Fatalf("--json=maybe should fail")
	}

	withFlags := globalOptions{agentOut: true}.withConfig([]string{"--title", "x"})
	if len(withFlags) != 3 || withFlags[0] != "--agent" {
		t.Fatalf("withConfig injection = %v", withFlags)
	}
}

func TestAgentModeTaskCreateEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	code, stdout, stderr := runAgentCommand(t,
		"--agent", "task", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "Agent task",
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.ContractVersion != 1 {
		t.Fatalf("contract_version = %d, want 1", envelope.ContractVersion)
	}
	if envelope.Command != "task create" || !envelope.OK || envelope.Error != nil {
		t.Fatalf("envelope = %+v", envelope)
	}
	var data struct {
		Task struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"task"`
		Reused      bool             `json:"reused"`
		Attachments []map[string]any `json:"attachments"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatalf("decode data: %v\n%s", err, envelope.Data)
	}
	if data.Task.ID == "" || data.Task.Title != "Agent task" {
		t.Fatalf("data.task = %+v", data.Task)
	}
	if data.Reused {
		t.Fatalf("reused = true, want false")
	}
	if data.Attachments == nil {
		t.Fatalf("attachments must be [] not null")
	}
}

func TestJSONModeBareDataWithoutEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	code, stdout, stderr := runAgentCommand(t,
		"task", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "JSON task",
		"--json",
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "contract_version") {
		t.Fatalf("--json output must not be enveloped: %q", stdout)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(stdout), &data); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if _, ok := data["task"]; !ok {
		t.Fatalf("bare data has no task key: %v", data)
	}
}

func TestAgentModeErrorEnvelopeUsesServerCode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	code, stdout, _ := runAgentCommand(t,
		"--agent", "task", "show",
		"--server", serverURL,
		"--token", "owner-token",
		"t-demo-9999",
	)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Error.Code != "task_not_found" {
		t.Fatalf("error.code = %q, want task_not_found", envelope.Error.Code)
	}
	if envelope.Error.Message == "" {
		t.Fatalf("error.message must not be empty")
	}
}

func TestAgentModeUsageError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	code, stdout, _ := runAgentCommand(t,
		"--agent", "task", "done",
		"--server", serverURL,
		"--token", "owner-token",
	)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "usage_error" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestAgentModeReadyNextAndWaitTimeout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	code, stdout, stderr := runAgentCommand(t,
		"--agent", "task", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "Ready candidate",
	)
	if code != 0 {
		t.Fatalf("create exit = %d, stderr = %q", code, stderr)
	}
	var created testEnvelope
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	var createData struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(created.Data, &createData); err != nil {
		t.Fatalf("decode create data: %v", err)
	}
	taskID := createData.Task.ID

	code, stdout, stderr = runAgentCommand(t, "--agent", "ready", "--server", serverURL, "--token", "owner-token")
	if code != 0 {
		t.Fatalf("ready exit = %d, stderr = %q", code, stderr)
	}
	ready := decodeEnvelope(t, stdout)
	if !ready.OK || !strings.Contains(string(ready.Data), taskID) {
		t.Fatalf("ready envelope = %+v", ready)
	}

	code, stdout, _ = runAgentCommand(t, "--agent", "next", "--server", serverURL, "--token", "owner-token")
	if code != 0 {
		t.Fatalf("next exit = %d", code)
	}
	next := decodeEnvelope(t, stdout)
	if !next.OK || !strings.Contains(string(next.Data), `"`+taskID+`"`) {
		t.Fatalf("next envelope = %+v", next)
	}

	// wait on an unscheduled task with a short timeout must exit 3 with a
	// wait_timeout envelope, preserving flow wait's domain exit code.
	code, stdout, _ = runAgentCommand(t,
		"--agent", "wait",
		"--server", serverURL,
		"--token", "owner-token",
		"--until", "done",
		"--timeout", "100ms",
		"--poll-interval", "20ms",
		taskID,
	)
	if code != 3 {
		t.Fatalf("wait exit = %d, want 3", code)
	}
	timedOut := decodeEnvelope(t, stdout)
	if timedOut.OK || timedOut.Error == nil || timedOut.Error.Code != "wait_timeout" {
		t.Fatalf("timeout envelope = %+v", timedOut)
	}
}

func TestAgentModeGlobalAndLocalPlacementAgree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	globalCode, globalOut, _ := runAgentCommand(t, "--agent", "board", "--server", serverURL, "--token", "owner-token")
	localCode, localOut, _ := runAgentCommand(t, "board", "--agent", "--server", serverURL, "--token", "owner-token")
	if globalCode != 0 || localCode != 0 {
		t.Fatalf("codes = global:%d local:%d", globalCode, localCode)
	}
	global := decodeEnvelope(t, globalOut)
	local := decodeEnvelope(t, localOut)
	if global.Command != "board" || local.Command != "board" {
		t.Fatalf("commands = global:%q local:%q", global.Command, local.Command)
	}
	if string(global.Data) != string(local.Data) {
		t.Fatalf("data differs:\nglobal: %s\nlocal:  %s", global.Data, local.Data)
	}
}
