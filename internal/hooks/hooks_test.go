package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	"github.com/ClarifiedLabs/flow/internal/metrics"
)

func TestMatchAnyExactAndGlob(t *testing.T) {
	cases := []struct {
		patterns []string
		kind     string
		want     bool
	}{
		{[]string{"task.done"}, "task.done", true},
		{[]string{"task.done"}, "task.created", false},
		{[]string{"task.*"}, "task.done", true},
		{[]string{"task.*"}, "task.created", true},
		{[]string{"task.*"}, "task", false},
		{[]string{"task.*"}, "tasks.done", false},
		{[]string{"task.*"}, "epic.completed", false},
		{[]string{"epic.completed", "task.*"}, "epic.completed", true},
		{[]string{"epic.completed", "task.*"}, "task.reset", true},
		{[]string{"epic.completed", "task.*"}, "git.push", false},
		{nil, "task.done", false},
	}
	for _, tc := range cases {
		if got := matchAny(tc.patterns, tc.kind); got != tc.want {
			t.Errorf("matchAny(%v, %q) = %v, want %v", tc.patterns, tc.kind, got, tc.want)
		}
	}
}

func TestBuildEnvelopeCapsPayloadAndEnvelope(t *testing.T) {
	event := coordinator.Event{
		Seq:     7,
		Kind:    "task.done",
		TaskID:  "t-1",
		Actor:   "human",
		Payload: json.RawMessage(`{"note":"` + strings.Repeat("x", 100<<10) + `"}`),
	}
	encoded := buildEnvelope("p-test", event)
	// The payload cap truncates raw bytes, so the payload may no longer parse;
	// the first maxPayloadBytes must be carried verbatim.
	if !bytes.Contains(encoded, event.Payload[:maxPayloadBytes]) {
		t.Fatalf("envelope must carry the first %d payload bytes verbatim", maxPayloadBytes)
	}
	if !bytes.Contains(encoded, []byte(`"project_id":"p-test"`)) {
		t.Fatalf("envelope missing header fields: %.200s", encoded)
	}
	if len(encoded) > maxEnvelopeBytes {
		t.Fatalf("envelope bytes = %d, want <= %d", len(encoded), maxEnvelopeBytes)
	}

	// An oversized non-payload field trips the whole-envelope hard cap.
	giant := event
	giant.Payload = json.RawMessage(`{}`)
	giant.TaskID = strings.Repeat("t", maxEnvelopeBytes*2)
	capped := buildEnvelope("p-test", giant)
	if len(capped) != maxEnvelopeBytes {
		t.Fatalf("envelope bytes = %d, want hard cap %d", len(capped), maxEnvelopeBytes)
	}

	// A missing payload defaults to an empty object.
	minimal := buildEnvelope("p-test", coordinator.Event{Seq: 1, Kind: "git.push"})
	var minimalDecoded envelope
	if err := json.Unmarshal(minimal, &minimalDecoded); err != nil {
		t.Fatalf("minimal envelope: %v", err)
	}
	if string(minimalDecoded.Payload) != `{}` {
		t.Fatalf("default payload = %s, want {}", minimalDecoded.Payload)
	}
	if minimalDecoded.ProjectID != "p-test" || minimalDecoded.Seq != 1 {
		t.Fatalf("envelope = %+v", minimalDecoded)
	}
}

func counterValue(t *testing.T, registry *metrics.Registry, name string) float64 {
	t.Helper()
	var buf bytes.Buffer
	registry.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name)), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return value
		}
	}
	return 0
}

func writeStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func waitForNonEmptyFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s was not written within 30s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHookRunTimeoutKillsAndCountsFailure(t *testing.T) {
	registry := metrics.New()
	d := New(nil, nil, registry)
	d.RunTimeout = 50 * time.Millisecond

	start := time.Now()
	d.execute(context.Background(), run{
		hook:  config.HookConfig{Command: []string{writeStub(t, "sleep 30")}},
		event: coordinator.Event{Seq: 1, Kind: "task.done"},
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hook was not killed promptly: %v", elapsed)
	}
	if got := counterValue(t, registry, "flow_hook_run_failures_total"); got != 1 {
		t.Fatalf("failures = %v, want 1", got)
	}
}

func TestEnqueueDropsWhenQueueFull(t *testing.T) {
	registry := metrics.New()
	d := New(nil, nil, registry)
	d.QueueSize = 2
	queue := make(chan run, d.QueueSize)

	hook := config.HookConfig{Command: []string{"/bin/true"}}
	event := coordinator.Event{Seq: 1, Kind: "task.done"}
	for i := 0; i < 4; i++ {
		d.enqueue(queue, hook, "p-test", event)
	}
	if got := counterValue(t, registry, "flow_hook_runs_dropped_total"); got != 2 {
		t.Fatalf("dropped = %v, want 2", got)
	}
	if len(queue) != 2 {
		t.Fatalf("queued = %d, want 2", len(queue))
	}
}

func TestNilRegistryIsTolerated(t *testing.T) {
	d := New(nil, nil, nil)
	queue := make(chan run) // unbuffered: every enqueue drops
	d.enqueue(queue, config.HookConfig{Command: []string{"/bin/true"}}, "p-test", coordinator.Event{Kind: "task.done"})
	d.execute(context.Background(), run{
		hook:  config.HookConfig{Command: []string{"/nonexistent/hook"}},
		event: coordinator.Event{Seq: 1, Kind: "task.done"},
	})
}

func TestDispatcherFiresOnAppendedEvent(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := coordinator.NewEventLogService(store.DB())

	// Committed before the dispatcher starts: must never fire.
	if _, err := log.Append(ctx, coordinator.Event{Kind: coordinator.EventTaskCreated, TaskID: "t-old"}); err != nil {
		t.Fatalf("append pre-start event: %v", err)
	}

	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.json")
	envPath := filepath.Join(dir, "env.txt")
	stub := writeStub(t, "cat > \"$1\"\nprintf '%s\\n' \"$FLOW_EVENT_KIND\" \"$FLOW_EVENT_SEQ\" \"$FLOW_PROJECT_ID\" \"$FLOW_TASK_ID\" > \"$2\"\n")

	registry := metrics.New()
	d := New([]config.HookConfig{{
		Events:  []string{"task.done"},
		Command: []string{stub, stdinPath, envPath},
	}}, func() []ProjectSource {
		return []ProjectSource{{ProjectID: "p-test", EventLog: log}}
	}, registry)
	d.PollInterval = 10 * time.Millisecond

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(runCtx)
	}()

	// Append the events under test only after the dispatcher has seeded the
	// project's cursor at the pre-start event (seq 1), so they are never
	// skipped by a slow first poll.
	select {
	case <-d.Seeded("p-test"):
	case <-time.After(30 * time.Second):
		t.Fatalf("dispatcher did not seed the project cursor within 30s")
	}

	appended, err := log.Append(ctx, coordinator.Event{
		Kind:    coordinator.EventTaskDone,
		TaskID:  "t-9",
		Actor:   "human",
		Payload: json.RawMessage(`{"resolution":"completed"}`),
	})
	if err != nil {
		t.Fatalf("append matching event: %v", err)
	}
	// A non-matching kind must not fire.
	if _, err := log.Append(ctx, coordinator.Event{Kind: coordinator.EventTaskCreated, TaskID: "t-10"}); err != nil {
		t.Fatalf("append non-matching event: %v", err)
	}

	waitForNonEmptyFile(t, stdinPath)
	// Stop polling while the hook may still be writing envPath. Run must drain
	// the in-flight hook before returning, so both outputs are complete below.
	cancel()
	<-done

	data, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read recorded stdin: %v", err)
	}
	var got envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode recorded envelope: %v", err)
	}
	if got.Seq != appended.Seq || got.Kind != "task.done" || got.ProjectID != "p-test" || got.TaskID != "t-9" || got.Actor != "human" {
		t.Fatalf("envelope = %+v, want seq %d task.done p-test t-9 human", got, appended.Seq)
	}
	if string(got.Payload) != `{"resolution":"completed"}` {
		t.Fatalf("payload = %s", got.Payload)
	}

	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read recorded env: %v", err)
	}
	wantEnv := "task.done\n" + strconv.FormatInt(appended.Seq, 10) + "\np-test\nt-9\n"
	if string(envData) != wantEnv {
		t.Fatalf("env = %q, want %q", envData, wantEnv)
	}

	if got := counterValue(t, registry, "flow_hook_run_failures_total"); got != 0 {
		t.Fatalf("failures = %v, want 0", got)
	}
}

func TestDispatcherDrainsInFlightHookOnShutdown(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := coordinator.NewEventLogService(store.DB())

	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	releasePath := filepath.Join(dir, "release")
	completedPath := filepath.Join(dir, "completed")
	stub := writeStub(t, "cat >/dev/null\nprintf started > \"$1\"\nwhile [ ! -e \"$2\" ]; do sleep 0.01; done\nprintf completed > \"$3\"\n")

	registry := metrics.New()
	d := New([]config.HookConfig{{
		Events:  []string{"task.done"},
		Command: []string{stub, startedPath, releasePath, completedPath},
	}}, func() []ProjectSource {
		return []ProjectSource{{ProjectID: "p-test", EventLog: log}}
	}, registry)
	d.PollInterval = 10 * time.Millisecond

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("dispatcher did not stop during cleanup")
		}
	})

	select {
	case <-d.Seeded("p-test"):
	case <-time.After(30 * time.Second):
		t.Fatalf("dispatcher did not seed the project cursor within 30s")
	}
	if _, err := log.Append(ctx, coordinator.Event{Kind: coordinator.EventTaskDone, TaskID: "t-1"}); err != nil {
		t.Fatalf("append matching event: %v", err)
	}
	waitForNonEmptyFile(t, startedPath)

	// The hook is blocked until releasePath appears. Canceling Run must stop
	// polling without killing this in-flight command.
	cancel()
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release hook: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("dispatcher did not drain the in-flight hook within 30s")
	}

	completed, err := os.ReadFile(completedPath)
	if err != nil {
		t.Fatalf("read hook completion: %v", err)
	}
	if string(completed) != "completed" {
		t.Fatalf("hook completion = %q, want completed", completed)
	}
	if got := counterValue(t, registry, "flow_hook_run_failures_total"); got != 0 {
		t.Fatalf("failures = %v, want 0", got)
	}
}
