package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestTaskBudgetAcceptsFlagsAfterTaskID proves the argument-order regression
// is gone: Go's stdlib flag parsing stops at the first positional argument, so
// `flow task budget TASK_ID --additional 2` used to fail with a bare usage
// line that named nothing about ordering. parseInterspersed makes both orders
// parse, so the operator's natural phrasing reaches the server.
func TestTaskBudgetAcceptsFlagsAfterTaskID(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		taskID       string
		additional   int
		instructions string
	}
	request := capturedRequest{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the budget POST carries a body to assert on; ignore any other
		// request the client makes while resolving the project scope.
		if !strings.HasSuffix(r.URL.Path, "/workflow/budget") || r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		var body struct {
			Additional   int    `json:"additional"`
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(buf.Bytes(), &body); err != nil {
			t.Errorf("decode request: %v (raw %q)", err, buf.String())
			return
		}
		mu.Lock()
		request.additional = body.Additional
		request.instructions = body.Instructions
		request.taskID = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run":{"task_id":"t-x","state":"running"}}`))
	}))
	defer server.Close()

	common := []string{"--server", server.URL, "--token", "owner-token"}
	orders := []struct {
		name string
		args []string
	}{
		{"flags before task id", append(append([]string{}, common...), "--additional", "2", "--instructions", "narrow the fix", "t-test-0001")},
		{"flags after task id", append(append([]string{"t-test-0001"}, common...), "--additional", "2", "--instructions", "narrow the fix")},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runTaskBudget(order.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(request.taskID, "/t-test-0001/") {
				t.Fatalf("requested path = %q, want the scoped task path", request.taskID)
			}
			if request.additional != 2 {
				t.Fatalf("requested additional = %d, want 2", request.additional)
			}
			if request.instructions != "narrow the fix" {
				t.Fatalf("requested instructions = %q, want the operator rationale", request.instructions)
			}
		})
	}
}

// TestParseInterspersed covers the shared helper across argument orderings,
// including a leading flag whose value is adjacent, positionals on both sides,
// and `--` terminating flag collection.
func TestParseInterspersed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want []string
		add  int
		ins  string
	}{
		{"flags before", []string{"--additional", "2", "--instructions", "x", "t-1"}, []string{"t-1"}, 2, "x"},
		{"flags after", []string{"t-1", "--additional", "2", "--instructions", "x"}, []string{"t-1"}, 2, "x"},
		{"flags both sides", []string{"--instructions", "x", "t-1", "--additional", "2"}, []string{"t-1"}, 2, "x"},
		{"two positionals between flag groups", []string{"t-1", "--additional", "2", "t-2", "--instructions", "x"}, []string{"t-1", "t-2"}, 2, "x"},
		{"no flags", []string{"t-1"}, []string{"t-1"}, 0, ""},
		{"two positionals only", []string{"t-1", "t-2"}, []string{"t-1", "t-2"}, 0, ""},
		{"dashdash ends flags", []string{"--instructions", "x", "--", "t-1", "-weird"}, []string{"t-1", "-weird"}, 0, "x"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			flags := flag.NewFlagSet("x", flag.ContinueOnError)
			flags.SetOutput(&nullWriter{})
			var additional int
			var instructions string
			flags.IntVar(&additional, "additional", 0, "")
			flags.StringVar(&instructions, "instructions", "", "")
			if err := parseInterspersed(flags, testCase.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if flags.NArg() != len(testCase.want) {
				t.Fatalf("positional count = %d (%v), want %d (%v)", flags.NArg(), flags.Args(), len(testCase.want), testCase.want)
			}
			for i, want := range testCase.want {
				if flags.Arg(i) != want {
					t.Fatalf("positional[%d] = %q, want %q", i, flags.Arg(i), want)
				}
			}
			if additional != testCase.add || instructions != testCase.ins {
				t.Fatalf("flags = additional %d instructions %q, want %d/%q", additional, instructions, testCase.add, testCase.ins)
			}
		})
	}
}

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

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
