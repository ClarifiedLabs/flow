package cliout

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
)

func TestWriteDataJSONModeIsBare(t *testing.T) {
	var buf bytes.Buffer
	if code := WriteData(&buf, ModeJSON, "task list", map[string]int{"count": 2}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := buf.String()
	if !strings.Contains(got, `"count":2`) {
		t.Fatalf("output = %q, want bare data", got)
	}
	if strings.Contains(got, "contract_version") {
		t.Fatalf("json mode must not wrap in envelope: %q", got)
	}
}

func TestWriteDataAgentModeWrapsEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if code := WriteData(&buf, ModeAgent, "task create", map[string]string{"id": "T-1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := buf.String()
	for _, want := range []string{`"contract_version":1`, `"command":"task create"`, `"ok":true`, `"id":"T-1"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, missing %s", got, want)
		}
	}
}

func TestWriteErrorAgentModeCarriesServerCode(t *testing.T) {
	var buf bytes.Buffer
	err := &flowclient.HTTPStatusError{StatusCode: http.StatusConflict, Code: "idempotency_key_conflict", Message: "key reused"}
	if code := WriteError(&buf, ModeAgent, "task create", err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := buf.String()
	for _, want := range []string{`"ok":false`, `"code":"idempotency_key_conflict"`, `"message":"key reused"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, missing %s", got, want)
		}
	}
}

func TestWriteErrorJSONModeIsBareErrorObject(t *testing.T) {
	var buf bytes.Buffer
	if code := WriteError(&buf, ModeJSON, "board", errors.New("boom")); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := buf.String()
	if !strings.Contains(got, `"error":{"code":"command_failed","message":"boom"}`) {
		t.Fatalf("output = %q, want bare error object", got)
	}
}

func TestWriteUsageErrorExitsTwo(t *testing.T) {
	var buf bytes.Buffer
	if code := WriteUsageError(&buf, ModeAgent, "task done", "task id is required"); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(buf.String(), `"code":"usage_error"`) {
		t.Fatalf("output = %q, want usage_error code", buf.String())
	}
}

func TestErrorCodeClassification(t *testing.T) {
	if got := ErrorCode(fmt.Errorf("wrap: %w", &flowclient.HTTPStatusError{StatusCode: 503})); got != "http_503" {
		t.Fatalf("status without code = %q, want http_503", got)
	}
	if got := ErrorCode(errors.New("nope")); got != "command_failed" {
		t.Fatalf("generic error = %q, want command_failed", got)
	}
}
