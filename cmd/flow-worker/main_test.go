package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/testenv"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
)

func TestMain(m *testing.M) {
	cleanup := testenv.Isolate()
	denyDir, err := os.MkdirTemp("", "flow-worker-deny-agents-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create deny agent dir: %v\n", err)
		os.Exit(2)
	}
	writeDenyAgentExecutable(denyDir, "codex", "login status")
	writeDenyAgentExecutable(denyDir, "claude", "auth status")
	writeDenyAgentExecutable(denyDir, "harness", "--check-model-proxy")
	_ = os.Setenv("PATH", denyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = os.Setenv("HARNESS_MODEL_PROXY_URL", "http://127.0.0.1:1")
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func writeDenyAgentExecutable(dir string, name string, checkArgs string) {
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = " + workerTestShellQuote(checkArgs) + " ]; then\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo \"unexpected test invocation of real agent shim: " + name + " $*\" >&2\n" +
		"exit 127\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "write deny agent %s: %v\n", name, err)
		os.Exit(2)
	}
}

func TestIsLeaseNotRenewableMatchesWrappedRenewFailure(t *testing.T) {
	err := fmt.Errorf("renew lease: %w", &flowclient.HTTPStatusError{
		StatusCode: http.StatusBadRequest,
		Code:       "renew_lease_failed",
		Message:    "lease is not renewable",
	})
	if !isLeaseNotRenewable(err) {
		t.Fatalf("isLeaseNotRenewable(%v) = false, want true", err)
	}
	other := &flowclient.HTTPStatusError{
		StatusCode: http.StatusBadRequest,
		Code:       "renew_lease_failed",
		Message:    "different failure",
	}
	if isLeaseNotRenewable(other) {
		t.Fatalf("isLeaseNotRenewable(%v) = true, want false", other)
	}
}

func TestIsStaleSourceJobHeadReportMatchesWrappedForbidden(t *testing.T) {
	err := fmt.Errorf("report check: %w", &flowclient.HTTPStatusError{
		StatusCode: http.StatusForbidden,
		Code:       "forbidden",
		Message:    "source job head does not match current change head",
	})
	if !isStaleSourceJobHeadReport(err) {
		t.Fatalf("isStaleSourceJobHeadReport(%v) = false, want true", err)
	}
	other := &flowclient.HTTPStatusError{
		StatusCode: http.StatusForbidden,
		Code:       "forbidden",
		Message:    "source job does not belong to the reported check",
	}
	if isStaleSourceJobHeadReport(other) {
		t.Fatalf("isStaleSourceJobHeadReport(%v) = true, want false", other)
	}
}

func TestAdvisoryVerdictFindingsStayInCheckDetailsWithoutThreadActions(t *testing.T) {
	job := flowworker.Job{Payload: map[string]any{"blocking": false}}
	if checkJobBlocksApproval(job) {
		t.Fatal("advisory job was treated as blocking")
	}
	if !checkJobBlocksApproval(flowworker.Job{Payload: map[string]any{}}) {
		t.Fatal("legacy job without blocking marker must default to blocking")
	}
	report := workerexec.VerdictReport{
		Verdict: "blocked",
		Reason:  "Potential timing leak.",
		Comments: []workerexec.ReviewCommentReport{{
			SHA: "abc123", File: "auth.go", Line: 42, Body: "Use a constant-time comparison.",
		}},
		Threads: []workerexec.ThreadDecisionReport{{
			ID: "th-1", Decision: "reopen", Body: "Recheck this path.",
		}},
	}
	details := advisoryVerdictDetails(report.Reason, report)
	for _, want := range []string{"Advisory (non-blocking)", "auth.go:42", "constant-time comparison", "thread th-1", "recommends reopen"} {
		if !strings.Contains(details, want) {
			t.Fatalf("advisory details %q missing %q", details, want)
		}
	}
	var stdout bytes.Buffer
	applyVerdictActions(nil, coordinator.CheckKindReviewer, false, flowworker.Lease{}, workerexec.RunResult{}, report, &stdout)
	if !strings.Contains(stdout.String(), "retained 2 advisory finding(s)") || !strings.Contains(stdout.String(), "no review threads changed") {
		t.Fatalf("advisory action output = %q", stdout.String())
	}
}

func TestLogLevelFlagEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "debug", "--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow-worker command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestWorkerJoinsWhenConfigOmitsToken(t *testing.T) {
	fixture := newWorkerTestFixture(t)
	server, err := api.NewServer(api.ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
		omitToken:      true,
	})
	t.Setenv("FLOW_WORKER_JOIN_TOKEN", "join-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--register-only", "--heartbeat-ttl=1s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q, stdout = %q", exitCode, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "registered: w-local") || !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if value := os.Getenv("FLOW_WORKER_JOIN_TOKEN"); value != "" {
		t.Fatalf("FLOW_WORKER_JOIN_TOKEN remained set as %q", value)
	}
}

func TestReadySessionHelperProcess(t *testing.T) {
	if os.Getenv("WORKER_READY_HELPER") != "1" {
		return
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL: os.Getenv("FLOW_COORDINATOR_URL"),
		Token:     os.Getenv("FLOW_SESSION_TOKEN"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ready helper client: %v\n", err)
		os.Exit(2)
	}
	if _, err := client.ReadySession(os.Getenv("FLOW_SESSION_ID")); err != nil {
		fmt.Fprintf(os.Stderr, "ready session: %v\n", err)
		os.Exit(2)
	}
	if path := strings.TrimSpace(os.Getenv("READY_HELPER_DONE_FILE")); path != "" {
		if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write ready marker: %v\n", err)
			os.Exit(2)
		}
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

func TestWorkerRetriesTransientCoordinatorHeartbeatBeforeClaim(t *testing.T) {
	t.Parallel()
	fixture := newWorkerTestFixture(t)
	server := fixture.Server
	var heartbeatAttempts atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/workers/heartbeat" && heartbeatAttempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"restart","message":"coordinator restarting"}}`))
			return
		}
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker transient error: heartbeat worker: restart: coordinator restarting", "claimed: none"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if heartbeatAttempts.Load() < 2 {
		t.Fatalf("heartbeat attempts = %d, want retry after transient failure", heartbeatAttempts.Load())
	}
}

func TestWorkerLeaseHeartbeatRecoversAfterTransientCoordinatorRenewalFailure(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	scriptPath := writeWorkerScript(t, `#!/bin/sh
sleep 3
printf renew-ok > "$1"
`)
	outPath := filepath.Join(t.TempDir(), "renew.out")
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       9,
		Payload: map[string]any{
			"entrypoint": map[string]any{
				"argv":  []string{scriptPath, outPath},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	server := fixture.Server
	var renewAttempts atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/workers/renew" && renewAttempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"restart","message":"coordinator restarting"}}`))
			return
		}
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "4s", "--heartbeat-ttl", "2s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q\nstdout = %s", exitCode, stderr.String(), stdout.String())
	}
	output := stdout.String()
	for _, want := range []string{"renew transient error: restart: coordinator restarting", "renewed:", "released: " + job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if renewAttempts.Load() < 2 {
		t.Fatalf("renew attempts = %d, want retry after transient failure", renewAttempts.Load())
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read worker output: %v", err)
	}
	if string(contents) != "renew-ok" {
		t.Fatalf("worker output file = %q", string(contents))
	}
	released, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if released.State != flowworker.JobFinished {
		t.Fatalf("job state = %q, want finished", released.State)
	}
}

func TestWorkerCapacityStartsConcurrentClaimLoops(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	scriptPath := writeWorkerScript(t, `#!/bin/sh
sleep 3
`)
	firstJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       10,
		Payload: map[string]any{
			"entrypoint": map[string]any{
				"argv":  []string{scriptPath},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue first job: %v", err)
	}
	secondJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       9,
		Payload: map[string]any{
			"entrypoint": map[string]any{
				"argv":  []string{scriptPath},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue second job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  2,
		toolYAML:       toolYAML,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	}()

	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		first, err := fixture.Queue.GetJob(ctx, firstJob.ID)
		if err != nil {
			t.Fatalf("get first job: %v", err)
		}
		second, err := fixture.Queue.GetJob(ctx, secondJob.ID)
		if err != nil {
			t.Fatalf("get second job: %v", err)
		}
		if first.State == flowworker.JobRunning && second.State == flowworker.JobRunning {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("jobs did not run concurrently: first=%s second=%s", first.State, second.State)
		case <-ticker.C:
		}
	}

	select {
	case exitCode := <-done:
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("worker did not finish; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	first, err := fixture.Queue.GetJob(ctx, firstJob.ID)
	if err != nil {
		t.Fatalf("get finished first job: %v", err)
	}
	second, err := fixture.Queue.GetJob(ctx, secondJob.ID)
	if err != nil {
		t.Fatalf("get finished second job: %v", err)
	}
	if first.State != flowworker.JobFinished || second.State != flowworker.JobFinished {
		t.Fatalf("job states = %s, %s; want both finished", first.State, second.State)
	}
}

func TestWorkerConsoleCleanExitReleasesSession(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	outPath := filepath.Join(t.TempDir(), "console.out")
	scriptPath := writeWorkerScript(t, `#!/bin/sh
printf console-exit > "$1"
`)
	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "codex",
		Entrypoint: map[string]any{
			"argv":  []string{scriptPath, outPath},
			"shell": false,
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		codexHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"claimed: " + ensured.Job.ID, "running: " + ensured.Job.ID + " state=running", "released: " + ensured.Job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "persistent session active:") {
		t.Fatalf("console session stayed active after clean exit:\n%s", output)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read console output: %v", err)
	}
	if string(contents) != "console-exit" {
		t.Fatalf("console output file = %q", string(contents))
	}

	finished, err := fixture.Queue.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get console job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("console job state = %q, want finished", finished.State)
	}
	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	if session.RuntimeState != coordinator.SessionFinished || session.FinishedAt == nil {
		t.Fatalf("console session = %+v, want finished", session)
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}

func TestWorkerConsoleNonZeroExitReleasesSession(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	outPath := filepath.Join(t.TempDir(), "console-failed.out")
	scriptPath := writeWorkerScript(t, `#!/bin/sh
printf console-failed > "$1"
exit 42
`)
	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "codex",
		Entrypoint: map[string]any{
			"argv":  []string{scriptPath, outPath},
			"shell": false,
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		codexHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"claimed: " + ensured.Job.ID, "exit=42", "released: " + ensured.Job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "persistent session active:") {
		t.Fatalf("console session stayed active after non-zero exit:\n%s", output)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read console output: %v", err)
	}
	if string(contents) != "console-failed" {
		t.Fatalf("console output file = %q", string(contents))
	}

	finished, err := fixture.Queue.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get console job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("console job state = %q, want finished", finished.State)
	}
	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	if session.RuntimeState != coordinator.SessionFinished || session.FinishedAt == nil {
		t.Fatalf("console session = %+v, want finished", session)
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}

func TestWorkerRegisterOnlyDoesNotClaimJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`+toolYAML+`
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--register-only", "--once"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("worker output = %q", stdout.String())
	}

	stillQueued, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stillQueued.State != flowworker.JobQueued {
		t.Fatalf("job state = %q, want queued", stillQueued.State)
	}
}

func TestWorkerUsesDiscoveredWorkerConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	dataDir := t.TempDir()
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	workerConfigPath := config.DefaultWorkerConfigPath(dataDir)
	if err := os.WriteFile(workerConfigPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--register-only"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("worker output = %q", stdout.String())
	}

	stillQueued, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stillQueued.State != flowworker.JobQueued {
		t.Fatalf("job state = %q, want queued", stillQueued.State)
	}
}

func TestWorkerConfigUsesDiscoveredWorkerConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dataDir := t.TempDir()
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: "http://127.0.0.1:8421",
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	workerConfigPath := config.DefaultWorkerConfigPath(dataDir)
	if err := os.WriteFile(workerConfigPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
token: worker-token
work_dir: /tmp/worker
labels:
  local: "true"
capacity:
  persistent_agent: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"config"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker_id: w-local", "protocol: 3", "labels: 1", "capacity_persistent_agent: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("config output missing %q:\n%s", want, output)
		}
	}
}

func TestRegistrationLabelsAdvertiseAvailableHarnessesAndDropGenericAgent(t *testing.T) {
	toolDir := t.TempDir()
	for _, name := range []string{"codex", "harness"} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", toolDir)

	labels := registrationLabels(map[string]string{
		"agent": "true",
		"local": "true",
	})
	if labels["agent"] != "" {
		t.Fatalf("labels = %#v, generic agent label should be dropped", labels)
	}
	for _, want := range []string{"local", "agent.harness.codex", "agent.harness.harness"} {
		if labels[want] != "true" {
			t.Fatalf("labels = %#v, missing %s=true", labels, want)
		}
	}
	if labels["agent.harness.claude"] == "true" {
		t.Fatalf("labels = %#v, did not expect claude", labels)
	}
}

func TestRegistrationLabelsReportHarnessAvailability(t *testing.T) {
	toolDir := t.TempDir()
	for _, name := range []string{"codex", "harness"} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", toolDir)

	labels, availability := registrationLabelsWithAvailability(map[string]string{"local": "true"})
	if labels["agent.harness.codex"] != "true" || labels["agent.harness.harness"] != "true" {
		t.Fatalf("labels = %#v, want codex and harness labels", labels)
	}
	statusByName := map[string]flowharness.Availability{}
	for _, status := range availability {
		statusByName[status.Name] = status
	}
	if !statusByName[flowharness.Codex].Available || !statusByName[flowharness.Harness].Available {
		t.Fatalf("availability = %#v, want codex and harness available", statusByName)
	}
	if statusByName[flowharness.Claude].Available || !strings.Contains(statusByName[flowharness.Claude].Reason, "executable not found") {
		t.Fatalf("claude availability = %#v, want missing executable", statusByName[flowharness.Claude])
	}
}

func TestLogAgentHarnessAvailabilityIncludesDetectedAndMissing(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	logAgentHarnessAvailability([]flowharness.Availability{
		{Name: flowharness.Harness, Executable: "harness", Path: "/usr/bin/harness", Available: true, Reason: "usability check passed"},
		{Name: flowharness.Claude, Executable: "claude", Available: false, Reason: "executable not found", Error: "not found"},
	})

	output := logs.String()
	for _, want := range []string{
		"msg=\"flow-worker agent harness detected\"",
		"harness=harness",
		"msg=\"flow-worker agent harness not detected\"",
		"harness=claude",
		"available=harness",
		"unavailable=claude",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("log output missing %q:\n%s", want, output)
		}
	}
}

func TestRegistrationHarnessModelsUsesHarnessCatalogWhenAvailable(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte(`#!/bin/sh
if [ "$*" = "--models --format json" ]; then
  printf '%s\n' '{"version":1,"model_count":1,"models":[{"target_id":"anthropic:claude-opus-4-8","display_name":"Claude Opus 4.8","provider_label":"Anthropic","model_label":"claude-opus-4-8","reasoning":true}]}'
  exit 0
fi
exit 1
`), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	models := registrationHarnessModels(map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"})
	if len(models) != 1 || models[0].QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("registration harness models = %#v", models)
	}
	if models[0].TargetID != "anthropic:claude-opus-4-8" || models[0].ProviderID != "anthropic" || models[0].ModelID != "claude-opus-4-8" {
		t.Fatalf("registration harness model normalized = %#v", models[0])
	}
	if models[0].Reasoning.Options[0].Type != "profile" {
		t.Fatalf("reasoning option = %#v, want profile", models[0].Reasoning.Options[0])
	}
	if values := models[0].Reasoning.Options[0].Values; len(values) != 7 || values[0] != "none" || values[6] != "max" {
		t.Fatalf("reasoning values = %#v", values)
	}
}

func TestRegistrationHarnessModelsFallsBackWhenCatalogFails(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte("#!/bin/sh\nexit 12\n"), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	models := registrationHarnessModels(map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"})
	if len(models) != 0 {
		t.Fatalf("registration harness models = %#v, want none", models)
	}
}

func TestWorkerStartupReaperUsesWorkerToken(t *testing.T) {
	putFakeTTYDOnPath(t)
	putFakeEmptyTmuxOnPath(t)
	fixture := newWorkerTestFixture(t)
	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "owner token is required") {
		t.Fatalf("startup reaper used owner-only job listing: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reaped orphaned tmux sessions: 0") {
		t.Fatalf("stderr = %q, want startup reaper summary", stderr.String())
	}
}

func TestWorkerRefusesToStartWithoutTTYD(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
token: worker-token
work_dir: /tmp/worker
capacity:
  persistent_agent: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "terminal attach preflight: ttyd is required") {
		t.Fatalf("stderr = %q, want ttyd preflight error", stderr.String())
	}
}

// TestRunWorkerLoopContinuesAfterJobError is the regression for one failed job
// taking down the whole worker process (and abandoning its sibling jobs): in
// service mode a job-scoped failure must be logged and survived, leaving the
// loop free to claim and finish the next job.
func TestRunWorkerLoopContinuesAfterJobError(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
		HeartbeatTTL:      time.Minute,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	failingJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       2,
		Payload: map[string]any{
			"base":   "missing-base",
			"branch": "missing-base",
			"entrypoint": map[string]any{
				"argv":  []string{"true"},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue failing job: %v", err)
	}

	nextOut := filepath.Join(t.TempDir(), "next.out")
	nextScript := writeWorkerScript(t, `#!/bin/sh
printf next-ok > "$1"
`)
	nextJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       1,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  []string{nextScript, nextOut},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue next job: %v", err)
	}

	coordinatorServer := httptest.NewServer(fixture.Server)
	t.Cleanup(coordinatorServer.Close)
	target, err := url.Parse(coordinatorServer.URL)
	if err != nil {
		t.Fatalf("parse coordinator url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// The gate ends the otherwise endless service loop: once closed, the next
	// pre-claim heartbeat fails non-retryably and runWorkerLoop returns.
	var gateClosed atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gateClosed.Load() {
			http.Error(w, "test gate closed", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)

	cfg := config.WorkerConfig{
		WorkerID:        "w-local",
		CoordinatorURL:  gate.URL,
		Token:           "worker-token",
		ProtocolVersion: config.DefaultProtocolVersion,
		WorkDir:         t.TempDir(),
		Terminal: config.WorkerTerminalConfig{
			TTYDPath: fakeTTYDPath(t),
		},
		Tmux: config.WorkerTmuxConfig{
			SocketPath: isolatedWorkerTmuxSocket(t),
		},
		Git: config.WorkerGitConfig{
			Principal: "worker:w-local",
		},
	}
	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	timings := workerTimings{
		ClaimWait:     0,
		LeaseDuration: 30 * time.Second,
		HeartbeatTTL:  30 * time.Second,
	}

	var stdout bytes.Buffer
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runWorkerLoop(client, cfg, timings, false, &stdout)
	}()

	waitForWorkerJobState(t, fixture, failingJob.ID, flowworker.JobFailed, 30*time.Second)
	waitForWorkerJobState(t, fixture, nextJob.ID, flowworker.JobFinished, 30*time.Second)
	waitForWorkerFile(t, nextOut, 30*time.Second)
	gateClosed.Store(true)

	var loopErr error
	select {
	case loopErr = <-loopDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("worker loop did not stop after gate closed; stdout:\n%s", stdout.String())
	}
	if loopErr == nil {
		t.Fatal("worker loop returned nil, want gate error")
	}
	var jobErr *jobError
	if errors.As(loopErr, &jobErr) {
		t.Fatalf("worker loop exited on job-scoped error %v; should have continued", loopErr)
	}
	output := stdout.String()
	if !strings.Contains(output, "job error:") || !strings.Contains(output, "continuing") {
		t.Fatalf("stdout missing job error continuation:\n%s", output)
	}
	finishedNext, err := fixture.Queue.GetJob(ctx, nextJob.ID)
	if err != nil {
		t.Fatalf("get next job: %v", err)
	}
	if finishedNext.State != flowworker.JobFinished {
		t.Fatalf("next job state = %q, want finished", finishedNext.State)
	}
}

func waitForWorkerJobState(t *testing.T, fixture workerTestFixture, jobID string, want flowworker.JobState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := fixture.Queue.GetJob(context.Background(), jobID)
		if err == nil && job.State == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, err := fixture.Queue.GetJob(context.Background(), jobID)
	t.Fatalf("job %s did not reach state %q within %s (state=%v err=%v)", jobID, want, timeout, job.State, err)
}

func workerTestRenderShellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-c" {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, workerTestShellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func waitForWorkerSessionState(t *testing.T, fixture workerTestFixture, taskID string, want coordinator.SessionRuntimeState, timeout time.Duration) coordinator.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last coordinator.Session
	for time.Now().Before(deadline) {
		sessions, err := fixture.Sessions.ListSessionsForTask(context.Background(), taskID, 1)
		if err == nil && len(sessions) > 0 {
			last = sessions[0]
			if sessions[0].RuntimeState == want {
				return sessions[0]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session for task %s did not reach state %q within %s (last=%+v)", taskID, want, timeout, last)
	return coordinator.Session{}
}

func workerTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeWorkerScript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func waitForWorkerFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("file %s was not created within %s", path, timeout)
		case <-ticker.C:
		}
	}
}

func isolatedWorkerTmuxSocket(t *testing.T) string {
	t.Helper()
	tmuxTmp, err := os.MkdirTemp("/tmp", "flow-worker-tmux-")
	if err != nil {
		t.Fatalf("create tmux dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmuxTmp)
	})
	return filepath.Join(tmuxTmp, "tmux.sock")
}

func workerTmuxSessionExists(socketPath string, sessionName string) bool {
	args := []string{"has-session", "-t", sessionName}
	if strings.TrimSpace(socketPath) != "" {
		args = append([]string{"-S", socketPath}, args...)
	}
	return exec.Command("tmux", args...).Run() == nil
}

func fakeTTYDPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ttyd")
	script := `#!/bin/sh
bind="127.0.0.1"
port=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -i)
      shift
      bind="$1"
      ;;
    -p)
      shift
      port="$1"
      ;;
  esac
  shift
done
if [ -z "$port" ]; then
  echo "missing -p" >&2
  exit 2
fi
FLOW_FAKE_TTYD_HELPER=1 FLOW_FAKE_TTYD_BIND="$bind" FLOW_FAKE_TTYD_PORT="$port" exec ` + testShellQuote(os.Args[0]) + ` -test.run=^TestFakeTTYDHelperProcess$
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ttyd: %v", err)
	}
	return path
}

func TestFakeTTYDHelperProcess(t *testing.T) {
	if os.Getenv("FLOW_FAKE_TTYD_HELPER") != "1" {
		return
	}
	bind := strings.TrimSpace(os.Getenv("FLOW_FAKE_TTYD_BIND"))
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := strings.TrimSpace(os.Getenv("FLOW_FAKE_TTYD_PORT"))
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		t.Fatalf("listen fake ttyd: %v", err)
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func testShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func putFakeTTYDOnPath(t *testing.T) {
	t.Helper()
	path := fakeTTYDPath(t)
	dir := filepath.Dir(path)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// workerConfigOptions describes the per-test variations of the worker
// worker.yaml fixture assembled by writeWorkerConfig. The constant fields
// (worker_id, token, dynamic work_dir, protocol_version) are shared across
// every call site; only the fields below differ.
type workerConfigOptions struct {
	coordinatorURL    string // coordinator_url value (e.g. httpServer.URL)
	protocolVersion   string // protocol_version value; defaults to the current protocol
	codexHarnessLabel bool   // include labels: { agent.harness.codex: "true" }
	capacityBucket    string // capacity bucket key (e.g. "ephemeral")
	capacityCount     int    // capacity bucket count
	toolYAML          string // tool/terminal/tmux config fragment
	principal         bool   // include git.principal: worker:w-local
	omitToken         bool   // leave token empty so the worker joins at startup
}

// writeWorkerConfig writes a worker worker.yaml into dir from opts and returns
// the config path. It assembles the shared constant fragments plus the varying
// capacity bucket, labels, and git fields used across the worker tests.
func writeWorkerConfig(t *testing.T, dir string, opts workerConfigOptions) string {
	t.Helper()
	protocolVersion := opts.protocolVersion
	if protocolVersion == "" {
		protocolVersion = config.DefaultProtocolVersion
	}
	var b strings.Builder
	b.WriteString("worker_id: w-local\n")
	b.WriteString("coordinator_url: " + opts.coordinatorURL + "\n")
	if !opts.omitToken {
		b.WriteString("token: worker-token\n")
	}
	b.WriteString("work_dir: " + filepath.ToSlash(t.TempDir()) + "\n")
	b.WriteString("protocol_version: " + protocolVersion + "\n")
	if opts.codexHarnessLabel {
		b.WriteString("labels:\n  agent.harness.codex: \"true\"\n")
	}
	b.WriteString("capacity:\n")
	b.WriteString(fmt.Sprintf("  %s: %d\n", opts.capacityBucket, opts.capacityCount))
	b.WriteString(opts.toolYAML)
	if opts.principal {
		b.WriteString("git:\n")
		b.WriteString("  principal: worker:w-local\n")
	}
	configPath := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(configPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}
	return configPath
}

func workerToolConfigYAML(t *testing.T) (string, string) {
	t.Helper()
	socketPath := isolatedWorkerTmuxSocket(t)
	return `terminal:
  ttyd_path: ` + filepath.ToSlash(fakeTTYDPath(t)) + `
tmux:
  socket_path: ` + filepath.ToSlash(socketPath) + `
`, socketPath
}

func putFakeEmptyTmuxOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
case "$1" in
list-sessions)
  exit 0
  ;;
kill-session)
  exit 0
  ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func requireWorkerTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func TestWorkerConfigLoadsYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
work_dir: /tmp/worker
labels:
  local: "true"
capacity:
  persistent_agent: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"config", "-c", configPath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker_id: w-local", "protocol: 3", "labels: 1", "capacity_persistent_agent: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("config output missing %q:\n%s", want, output)
		}
	}
}

// TestWorkerConsoleRunErrorReleasesSessionAndSurfacesError is a regression test
// for the console-crash masking + lease leak: when a console session's worker
// step fails (RunResult.Err != nil), the console branch must still release the
// lease through /v2/console and surface the real error, never falling through to
// the generic persistent-session process-exit path (which rejects the console
// role, leaking the lease and masking the error). The reserved FLOW_* env key
// makes RunJob fail in validateEntrypoint deterministically before any tmux work.
func TestWorkerConsoleRunErrorReleasesSessionAndSurfacesError(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "codex",
		Entrypoint: map[string]any{
			"argv":  []string{"/bin/sh", "-c", "true"},
			"shell": false,
			// A reserved FLOW_* override is rejected by validateEntrypoint, so
			// RunJob returns RunResult.Err != nil before reaching the agent.
			"env": map[string]string{"FLOW_INJECTED": "1"},
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		codexHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	// The job fails, so the worker exits non-zero and reports the error on stderr.
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1; stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "released: "+ensured.Job.ID+" state=finished") {
		t.Fatalf("worker output missing console release:\n%s", output)
	}
	// The console branch must surface the real run error, never the masked
	// process-exit error from the generic persistent-session path.
	if strings.Contains(output, "report persistent session process exit") ||
		strings.Contains(stderr.String(), "report persistent session process exit") {
		t.Fatalf("console run error masked by process-exit path:\nstdout=%s\nstderr=%s", output, stderr.String())
	}
	if !strings.Contains(stderr.String(), "run job:") {
		t.Fatalf("worker stderr missing real run error: %q", stderr.String())
	}

	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil; lease leaked after console run error")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}
