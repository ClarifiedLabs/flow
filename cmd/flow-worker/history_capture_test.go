package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
	"github.com/ClarifiedLabs/flow/internal/worker/historycapture"
)

func TestHistoryCaptureWasInterruptedIncludesLeaseFailure(t *testing.T) {
	if historyCaptureWasInterrupted(nil, nil, nil) {
		t.Fatal("clean execution was classified as interrupted")
	}
	if !historyCaptureWasInterrupted(nil, nil, errors.New("lease expired")) {
		t.Fatal("lease heartbeat failure was not classified as interrupted")
	}
}

func TestInterruptedTerminalFailuresDiscardEveryLifecycleAction(t *testing.T) {
	tests := []struct {
		name   string
		action *historycapture.TerminalAction
	}{
		{name: "lease", action: &historycapture.TerminalAction{Kind: historycapture.TerminalReleaseLease, FinalState: string(flowworker.JobFinished)}},
		{name: "session", action: &historycapture.TerminalAction{Kind: historycapture.TerminalSessionExit, SessionID: "session-1", ExitCode: 1}},
		{name: "console", action: &historycapture.TerminalAction{Kind: historycapture.TerminalConsoleExit, SessionID: "console-1", SessionToken: "console-secret", ExitCode: 1}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			outbox, err := historycapture.New(historycapture.Options{
				Dir: t.TempDir(), SegmentBytes: 4, ArchiveLimits: historyarchive.DefaultLimits(),
				MaxOutstandingBytes: 64 << 20, MaxOutstandingEntries: 10, SensitiveDataKey: []byte("worker-token"),
			})
			if err != nil {
				t.Fatal(err)
			}
			capture := contract.HistoryCapture{
				ID: fmt.Sprintf("capture-%d", index), ProjectID: "project-1", JobID: "job-1", LeaseID: "lease-1", LeaseAttempt: 1,
				WorkerID: "worker-1", Role: "author", ExpectedTranscript: true, State: "reserved", ExecutionVerdict: "pending", Version: 1,
			}
			sources := historycapture.SourcePaths{Worktree: t.TempDir(), Transcript: filepath.Join(t.TempDir(), "transcript.log")}
			reservation := contract.ReserveHistoryCaptureResponse{Capture: capture, UploadGrant: "private-upload-grant", Created: true}
			if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
				t.Fatal(err)
			}
			exitCode := 1
			if _, err := outbox.RecordFinal(ctx, capture.ID, historycapture.Final{Verdict: "failed", ExitCode: &exitCode, Terminal: test.action}); err != nil {
				t.Fatal(err)
			}
			capture.State, capture.ExecutionVerdict, capture.Version = "complete", "failed", 2
			reservation.Capture = capture
			if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
				t.Fatal(err)
			}
			manager := &historyCaptureManager{outbox: outbox, active: map[string]struct{}{capture.ID: {}}}
			jobCtx, cancelJob := context.WithCancel(ctx)
			cancelJob()
			discarded, err := discardHistoryTerminalAfterInterruption(manager, capture.ID, ctx, jobCtx, nil)
			if err != nil || !discarded {
				t.Fatalf("discard after interruption = %v, %v; want true, nil", discarded, err)
			}
			pending, err := outbox.ListPending(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending terminal after interrupted failure = %+v, err=%v", pending, err)
			}
		})
	}
}

func TestHistoryCaptureFinalizeSensitiveValuesIncludeRecoveryCredentials(t *testing.T) {
	manager := &historyCaptureManager{recoverySensitiveValues: [][]byte{[]byte("worker-token"), []byte("model-proxy-key")}}
	values := manager.captureSensitiveValues([][]byte{[]byte("session-token")})
	got := make([]string, len(values))
	for index := range values {
		got[index] = string(values[index])
	}
	want := []string{"worker-token", "model-proxy-key", "session-token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capture sensitive values = %q, want %q", got, want)
	}
}

func TestFinalHistoryPublicationStopsAtContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	var attempts atomic.Int32
	err := retryFinalHistoryPublication(ctx, io.Discard, func() error {
		attempts.Add(1)
		return &flowclient.HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Code: "temporarily_unavailable", Message: "retry"}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryFinalHistoryPublication() error = %v, want deadline exceeded", err)
	}
	if attempts.Load() == 0 {
		t.Fatal("publication was not attempted")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("publication ignored deadline for %s", elapsed)
	}
}

func TestHistoryCaptureFinalizeLeavesTerminalLifecycleToLiveJobPath(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, `{"error":{"code":"unexpected","message":"detached finalization attempted lifecycle I/O"}}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	outbox, err := historycapture.New(historycapture.Options{
		Dir: t.TempDir(), SegmentBytes: 4, ArchiveLimits: historyarchive.DefaultLimits(),
		MaxOutstandingBytes: 64 << 20, MaxOutstandingEntries: 10, SensitiveDataKey: []byte("worker-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	capture := contract.HistoryCapture{
		ID: "capture-detached", ProjectID: "project-1", JobID: "job-1", LeaseID: "lease-1", LeaseAttempt: 1,
		WorkerID: "worker-1", Role: "author", ExpectedTranscript: true, State: "reserved", ExecutionVerdict: "pending", Version: 1,
	}
	sources := historycapture.SourcePaths{Worktree: t.TempDir(), Transcript: filepath.Join(t.TempDir(), "transcript.log")}
	reservation := contract.ReserveHistoryCaptureResponse{Capture: capture, UploadGrant: "private-upload-grant", Created: true}
	if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := outbox.RecordFinal(ctx, capture.ID, historycapture.Final{
		Verdict: "succeeded", ExitCode: &exitCode,
		Terminal: &historycapture.TerminalAction{Kind: historycapture.TerminalReleaseLease, FinalState: string(flowworker.JobFinished)},
	}); err != nil {
		t.Fatal(err)
	}
	capture.State, capture.ExecutionVerdict, capture.Version = "complete", "succeeded", 2
	reservation.Capture = capture
	if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
	if err != nil {
		t.Fatal(err)
	}
	manager := &historyCaptureManager{outbox: outbox, client: client, active: map[string]struct{}{capture.ID: {}}}
	if err := manager.Finalize(ctx, capture.ID, workerexec.RunResult{}, false, nil, nil, io.Discard); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("detached finalization made %d lifecycle requests, want zero", requests.Load())
	}
	pending, err := outbox.ListPending(ctx)
	if err != nil || len(pending) != 1 || pending[0].Terminal == nil {
		t.Fatalf("terminal checkpoint after detached finalization = %+v, err=%v", pending, err)
	}
}

func TestHistoryCaptureTerminalLifecycleReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	var processExitCalls, consoleReleaseCalls, statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/sessions/session-1/process-exit":
			if processExitCalls.Add(1) == 1 {
				http.Error(w, `{"code":"temporarily_unavailable","message":"injected failure"}`, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"session":{"id":"session-1","job_id":"job-1","lease_id":"lease-1","runtime_state":"finished"}}`))
		case "/v2/console":
			consoleReleaseCalls.Add(1)
			http.Error(w, `{"code":"invalid_bearer_token","message":"owner stopped console"}`, http.StatusUnauthorized)
		case "/v2/workers/status":
			statusCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"canceled"},"lease":{"id":"lease-1","released_at":"2026-08-04T00:00:00Z"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	outboxDir := filepath.Join(t.TempDir(), "outbox")
	newOutbox := func() *historycapture.Outbox {
		outbox, err := historycapture.New(historycapture.Options{
			Dir: outboxDir, SegmentBytes: 4, ArchiveLimits: historyarchive.DefaultLimits(),
			MaxOutstandingBytes: 64 << 20, MaxOutstandingEntries: 10, SensitiveDataKey: []byte("worker-token"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return outbox
	}
	outbox := newOutbox()
	capture := contract.HistoryCapture{
		ID: "capture-1", ProjectID: "project-1", JobID: "job-1", LeaseID: "lease-1", LeaseAttempt: 1,
		WorkerID: "worker-1", Role: "author", ExpectedTranscript: true, State: "reserved", ExecutionVerdict: "pending", Version: 1,
	}
	sources := historycapture.SourcePaths{Worktree: t.TempDir(), Transcript: filepath.Join(t.TempDir(), "transcript.log")}
	reservation := contract.ReserveHistoryCaptureResponse{Capture: capture, UploadGrant: "private-upload-grant", Created: true}
	if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := outbox.RecordFinal(ctx, capture.ID, historycapture.Final{
		Verdict: "succeeded", ExitCode: &exitCode,
		Terminal: &historycapture.TerminalAction{Kind: historycapture.TerminalConsoleExit, SessionID: "session-1", SessionToken: "revoked-session-token", ExitCode: exitCode},
	}); err != nil {
		t.Fatal(err)
	}
	capture.State, capture.ExecutionVerdict, capture.Version = "complete", "succeeded", 2
	reservation.Capture = capture
	if _, err := outbox.RecordReservation(ctx, historycapture.Reservation{Response: reservation, Sources: sources}); err != nil {
		t.Fatal(err)
	}

	newManager := func(outbox *historycapture.Outbox) *historyCaptureManager {
		client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
		if err != nil {
			t.Fatal(err)
		}
		return &historyCaptureManager{cfg: config.WorkerConfig{CoordinatorURL: server.URL}, client: client, outbox: outbox, active: map[string]struct{}{}}
	}
	if err := newManager(outbox).Replay(ctx); err == nil {
		t.Fatal("failed terminal lifecycle replay unexpectedly succeeded")
	}
	pending, err := outbox.ListPending(ctx)
	if err != nil || len(pending) != 1 || pending[0].Terminal == nil {
		t.Fatalf("pending terminal action after failure = %+v, err=%v", pending, err)
	}

	restarted := newOutbox()
	if err := newManager(restarted).Replay(ctx); err != nil {
		t.Fatalf("terminal lifecycle replay after restart: %v", err)
	}
	pending, err = restarted.ListPending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending terminal action after reconciliation = %+v, err=%v", pending, err)
	}
	before := processExitCalls.Load()
	if err := newManager(newOutbox()).Replay(ctx); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if processExitCalls.Load() != before || before != 2 || consoleReleaseCalls.Load() != 1 || statusCalls.Load() != 0 {
		t.Fatalf("network calls process-exit=%d console-release=%d status=%d", processExitCalls.Load(), consoleReleaseCalls.Load(), statusCalls.Load())
	}
}

func TestHistoryCaptureTerminalActionsRecoverAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	var releaseFinal, sessionFinal, consoleFinal atomic.Bool
	var releaseCalls, sessionCalls, consoleCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/workers/status":
			var request struct {
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			jobID, state, releasedAt := strings.TrimSuffix(request.LeaseID, "-lease")+"-job", "running", "null"
			final := request.LeaseID == "release-lease" && releaseFinal.Load() || request.LeaseID == "session-lease" && sessionFinal.Load()
			if final {
				state, releasedAt = "finished", `"2026-08-04T00:00:00Z"`
			}
			fmt.Fprintf(w, `{"job":{"id":%q,"state":%q},"lease":{"id":%q,"released_at":%s}}`, jobID, state, request.LeaseID, releasedAt)
		case r.URL.Path == "/v2/workers/release":
			releaseCalls.Add(1)
			releaseFinal.Store(true)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"temporarily_unavailable","message":"response lost"}}`))
		case r.URL.Path == "/v2/sessions/session-1/process-exit":
			sessionCalls.Add(1)
			sessionFinal.Store(true)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"temporarily_unavailable","message":"response lost"}}`))
		case r.URL.Path == "/v2/sessions/console-1/process-exit":
			consoleCalls.Add(1)
			if consoleFinal.Load() {
				_, _ = w.Write([]byte(`{"session":{"id":"console-1","job_id":"console-job","lease_id":"console-lease","runtime_state":"finished"}}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"session_process_exit_failed","message":"console sessions are released through console release"}}`))
		case r.URL.Path == "/v2/console" && r.Method == http.MethodDelete:
			consoleCalls.Add(1)
			consoleFinal.Store(true)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"temporarily_unavailable","message":"response lost"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
	if err != nil {
		t.Fatal(err)
	}
	manager := &historyCaptureManager{cfg: config.WorkerConfig{CoordinatorURL: server.URL}, client: client}

	releaseEntry := historycapture.Entry{
		Capture:  contract.HistoryCapture{JobID: "release-job", LeaseID: "release-lease"},
		Terminal: &historycapture.TerminalAction{Kind: historycapture.TerminalReleaseLease, FinalState: string(flowworker.JobFinished)},
	}
	if err := manager.reconcileTerminalAction(ctx, releaseEntry, releaseEntry.Terminal); err != nil {
		t.Fatalf("release response-loss reconciliation: %v", err)
	}
	if releaseCalls.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releaseCalls.Load())
	}

	sessionEntry := historycapture.Entry{
		Capture:  contract.HistoryCapture{JobID: "session-job", LeaseID: "session-lease"},
		Terminal: &historycapture.TerminalAction{Kind: historycapture.TerminalSessionExit, SessionID: "session-1", ExitCode: 1},
	}
	if err := manager.reconcileTerminalAction(ctx, sessionEntry, sessionEntry.Terminal); err != nil {
		t.Fatalf("session response-loss reconciliation: %v", err)
	}
	if sessionCalls.Load() != 1 {
		t.Fatalf("session process-exit calls = %d, want 1", sessionCalls.Load())
	}

	consoleEntry := historycapture.Entry{
		Capture:  contract.HistoryCapture{JobID: "console-job", LeaseID: "console-lease"},
		Terminal: &historycapture.TerminalAction{Kind: historycapture.TerminalConsoleExit, SessionID: "console-1", SessionToken: "console-token"},
	}
	if err := manager.reconcileTerminalAction(ctx, consoleEntry, consoleEntry.Terminal); err == nil {
		t.Fatal("lost console-release response was acknowledged before replay")
	}
	if err := manager.reconcileTerminalAction(ctx, consoleEntry, consoleEntry.Terminal); err != nil {
		t.Fatalf("console response-loss replay: %v", err)
	}
	if consoleCalls.Load() != 3 {
		t.Fatalf("console lifecycle calls = %d, want process-exit, release, process-exit", consoleCalls.Load())
	}
}

func TestHistoryCaptureManagerRejectsSourcesOutsideAttemptRoots(t *testing.T) {
	workDir := t.TempDir()
	manager := &historyCaptureManager{cfg: config.WorkerConfig{WorkDir: workDir}}
	capture := contract.HistoryCapture{ID: "capture", JobID: "job", LeaseID: "lease", ExpectedTranscript: true, ExpectedHarness: true}
	attemptRoot := workerexec.HistoryAttemptDir(workDir, capture.JobID, capture.LeaseID)
	valid := historycapture.SourcePaths{
		Worktree:          workerexec.HistoryWorktreePath(workDir, capture.JobID),
		Transcript:        filepath.Join(attemptRoot, "transcript.log"),
		NativeSessionRoot: filepath.Join(attemptRoot, "harness-session"),
	}
	if err := manager.validateSources(capture, valid); err != nil {
		t.Fatalf("valid sources rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*historycapture.SourcePaths)
	}{
		{name: "worktree", mutate: func(s *historycapture.SourcePaths) { s.Worktree = t.TempDir() }},
		{name: "transcript", mutate: func(s *historycapture.SourcePaths) { s.Transcript = filepath.Join(t.TempDir(), "transcript.log") }},
		{name: "native session", mutate: func(s *historycapture.SourcePaths) { s.NativeSessionRoot = t.TempDir() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sources := valid
			tt.mutate(&sources)
			if err := manager.validateSources(capture, sources); err == nil {
				t.Fatal("source outside Flow-owned roots was accepted")
			}
		})
	}
}
