package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// updateGoldens regenerates the golden contract fixtures under
// internal/api/testdata/contract and the generated route doc at
// docs/reference/http-api.md:
//
//	go test ./internal/api -run 'Contract|Route' -update
var updateGoldens = flag.Bool("update", false, "regenerate golden contract fixtures and the generated route doc")

// contractGoldenDir holds the checked-in golden responses, one file per
// pinned endpoint.
const contractGoldenDir = "testdata/contract"

// TestContractGoldenResponses pins the response shape of the stable
// agent-facing read endpoints. Any change to a response shape fails this test
// until the goldens are regenerated with -update and reviewed.
func TestContractGoldenResponses(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	// Seed one open task and one completed task so every pinned endpoint has
	// a non-empty response (board/lists show the open task, completions shows
	// the done one).
	openTask, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Golden open task", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create open task: %v", err)
	}
	doneTask, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Golden done task", CreatedBy: coordinator.ActorHuman},
	})
	if err != nil {
		t.Fatalf("create done task: %v", err)
	}
	doneBody := map[string]any{"resolution": "completed", "note": "golden completion"}
	doneResponse := httptest.NewRecorder()
	fixture.Server.ServeHTTP(doneResponse, authorizedRequest(http.MethodPost, "/v2/projects/"+fixture.Project.ID+"/tasks/"+doneTask.ID+"/done", doneBody))
	if doneResponse.Code != http.StatusOK {
		t.Fatalf("done status = %d: %s", doneResponse.Code, doneResponse.Body.String())
	}

	// scrubReplacements maps volatile entity ids to placeholders. It applies
	// to map keys too (task_cards, lane_states, and wait_reasons are keyed by
	// task id).
	scrubReplacements := map[string]string{
		fixture.Project.ID: "<project-id>",
		openTask.ID:        "<open-task-id>",
		doneTask.ID:        "<done-task-id>",
	}

	projectPath := "/v2/projects/" + fixture.Project.ID
	cases := []struct {
		name string
		path string
	}{
		{"project", projectPath},
		{"events", projectPath + "/events"},
		{"tasks", projectPath + "/tasks"},
		{"search", projectPath + "/search?q=Golden"},
		{"completions", projectPath + "/completions"},
		{"board", projectPath + "/board"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			fixture.Server.ServeHTTP(response, authorizedRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200: %s", tc.path, response.Code, response.Body.String())
			}

			canonical, err := canonicalizeContractJSON(response.Body.Bytes(), scrubReplacements)
			if err != nil {
				t.Fatalf("canonicalize %s response: %v", tc.path, err)
			}

			goldenPath := filepath.Join(contractGoldenDir, tc.name+".golden.json")
			if *updateGoldens {
				if err := os.MkdirAll(contractGoldenDir, 0o755); err != nil {
					t.Fatalf("create golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, canonical, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to create it)", goldenPath, err)
			}
			if !bytes.Equal(want, canonical) {
				t.Errorf("GET %s response drifted from %s; review and regenerate with `go test ./internal/api -run Contract -update`\n%s",
					tc.path, goldenPath, goldenDiff(string(want), string(canonical)))
			}
		})
	}
}

// canonicalizeContractJSON decodes a response body, scrubs volatile fields,
// and re-marshals with stable formatting (json.MarshalIndent sorts map keys),
// so goldens are deterministic across runs.
func canonicalizeContractJSON(body []byte, replacements map[string]string) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	scrubbed := scrubContractValue(decoded, replacements)
	canonical, err := json.MarshalIndent(scrubbed, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return append(canonical, '\n'), nil
}

// scrubContractValue walks decoded JSON and replaces volatile content with
// placeholders:
//   - strings listed in replacements (entity ids; also applied to map keys,
//     which carry task ids in board maps);
//   - RFC3339/RFC3339Nano timestamps, replaced with "<ts>";
//   - seq/next_since event-cursor numbers, replaced with "<seq>".
func scrubContractValue(value any, replacements map[string]string) any {
	switch typed := value.(type) {
	case map[string]any:
		scrubbed := make(map[string]any, len(typed))
		for key, item := range typed {
			scrubbedKey := key
			if replacement, ok := replacements[key]; ok {
				scrubbedKey = replacement
			}
			lowerKey := strings.ToLower(key)
			if lowerKey == "seq" || lowerKey == "next_since" {
				scrubbed[scrubbedKey] = "<seq>"
				continue
			}
			// Event ids are random (evt_<rand>); scrub them so goldens are stable.
			if lowerKey == "id" {
				if str, isStr := item.(string); isStr && strings.HasPrefix(str, "evt_") {
					scrubbed[scrubbedKey] = "<event-id>"
					continue
				}
			}
			scrubbed[scrubbedKey] = scrubContractValue(item, replacements)
		}
		return scrubbed
	case []any:
		scrubbed := make([]any, len(typed))
		for i, item := range typed {
			scrubbed[i] = scrubContractValue(item, replacements)
		}
		return scrubbed
	case string:
		if replacement, ok := replacements[typed]; ok {
			return replacement
		}
		if isContractTimestamp(typed) {
			return "<ts>"
		}
		return typed
	default:
		return typed
	}
}

// isContractTimestamp reports whether s is an RFC3339 timestamp as emitted by
// time.Time JSON marshaling.
func isContractTimestamp(s string) bool {
	if len(s) < len(time.RFC3339) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// goldenDiff renders a line-aligned -want/+got diff for small golden files.
func goldenDiff(want string, got string) string {
	wantLines := strings.Split(strings.TrimRight(want, "\n"), "\n")
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	var out strings.Builder
	out.WriteString("diff (-want +got):\n")
	lines := max(len(wantLines), len(gotLines))
	for i := 0; i < lines; i++ {
		var wantLine, gotLine string
		haveWant, haveGot := i < len(wantLines), i < len(gotLines)
		if haveWant {
			wantLine = wantLines[i]
		}
		if haveGot {
			gotLine = gotLines[i]
		}
		switch {
		case haveWant && haveGot && wantLine == gotLine:
			fmt.Fprintf(&out, "  %s\n", wantLine)
		case haveWant && haveGot:
			fmt.Fprintf(&out, "- %s\n+ %s\n", wantLine, gotLine)
		case haveWant:
			fmt.Fprintf(&out, "- %s\n", wantLine)
		default:
			fmt.Fprintf(&out, "+ %s\n", gotLine)
		}
	}
	return out.String()
}
