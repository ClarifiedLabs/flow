package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// oneShotTestSetup returns a fixture, assignment-authenticated config, and
// timings wired to a test coordinator. The CLI registers the worker itself.
func oneShotTestSetup(t *testing.T) (workerTestFixture, config.WorkerConfig, workerTimings) {
	t.Helper()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")

	fixture := newWorkerTestFixture(t)

	coordinatorServer := httptest.NewServer(fixture.Server)
	t.Cleanup(coordinatorServer.Close)

	cfg := config.WorkerConfig{
		WorkerID:       "w-local",
		CoordinatorURL: coordinatorServer.URL,
		Token:          "worker-token",
		WorkDir:        t.TempDir(),
		Git: config.WorkerGitConfig{
			Principal: "worker:w-local",
		},
	}
	timings := workerTimings{
		ClaimWait:     0,
		LeaseDuration: 30 * time.Second,
		HeartbeatTTL:  30 * time.Second,
	}

	return fixture, cfg, timings
}

func enqueueEphemeralTestJob(t *testing.T, fixture workerTestFixture, script string, args ...string) flowworker.Job {
	t.Helper()
	job, err := fixture.Queue.EnqueueJob(context.Background(), flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  append([]string{script}, args...),
				"shell": false,
			}, "blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return job
}

func TestRunWorkerOneShotRunsOneJobAndExits(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := oneShotTestSetup(t)

	out := filepath.Join(t.TempDir(), "done.out")
	script := writeWorkerScript(t, `#!/bin/sh
printf ephemeral-ok > "$1"
`)
	job := enqueueEphemeralTestJob(t, fixture, script, out)
	registerAssignedWorker(t, fixture, job, cfg.WorkerID, nil)

	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	var stdout bytes.Buffer
	if err := runWorkerOneShot(context.Background(), client, cfg, timings, nil, &stdout); err != nil {
		t.Fatalf("runWorkerOneShot() error = %v; stdout:\n%s", err, stdout.String())
	}

	finished, err := fixture.Queue.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("job state = %q, want finished", finished.State)
	}
	waitForWorkerFile(t, out, 30*time.Second)
	if !strings.Contains(stdout.String(), "claimed: "+job.ID) {
		t.Fatalf("stdout missing claim:\n%s", stdout.String())
	}
}

func TestRunWorkerOneShotPublishesMandatoryFinalHistoryBeforeRelease(t *testing.T) {
	fixture, cfg, timings := oneShotTestSetup(t)
	job := enqueueEphemeralTestJob(t, fixture, writeWorkerScript(t, "#!/bin/sh\nprintf history-ok\n"))
	registerAssignedWorker(t, fixture, job, cfg.WorkerID, nil)

	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	policy, err := cfg.History.Resolve(cfg.WorkDir)
	if err != nil {
		t.Fatalf("resolve history policy: %v", err)
	}
	manager, err := newHistoryCaptureManager(client, cfg, policy)
	if err != nil {
		t.Fatalf("new history capture manager: %v", err)
	}
	if err := manager.Replay(context.Background()); err != nil {
		t.Fatalf("initial history replay: %v", err)
	}
	timings.History = manager

	var stdout bytes.Buffer
	if err := runWorkerOneShot(context.Background(), client, cfg, timings, nil, &stdout); err != nil {
		t.Fatalf("runWorkerOneShot() error = %v; stdout:\n%s", err, stdout.String())
	}
	captures, err := fixture.History.List(context.Background(), coordinator.HistoryCaptureListOptions{JobIDs: []string{job.ID}})
	if err != nil {
		t.Fatalf("list history captures: %v", err)
	}
	if len(captures) != 1 || captures[0].State != coordinator.HistoryCaptureComplete || captures[0].ExecutionVerdict != coordinator.HistoryExecutionSucceeded {
		t.Fatalf("captures = %+v; stdout:\n%s", captures, stdout.String())
	}
	artifacts, err := fixture.History.ListArtifacts(context.Background(), captures[0].ID)
	if err != nil {
		t.Fatalf("list history artifacts: %v", err)
	}
	var logicalKeys []string
	for _, artifact := range artifacts {
		logicalKeys = append(logicalKeys, artifact.LogicalKey)
	}
	if strings.Join(logicalKeys, ",") != "manifest/final,transcript/000000000000,workspace/final" {
		t.Fatalf("artifact logical keys = %v", logicalKeys)
	}
	if !strings.Contains(stdout.String(), "history: "+captures[0].ID+" state=complete") {
		t.Fatalf("stdout missing complete history capture:\n%s", stdout.String())
	}
}

func TestRunWorkerOneShotRejectsJobSpecificModelProxyCredentialFromHistory(t *testing.T) {
	fixture, cfg, timings := oneShotTestSetup(t)
	const credential = "job-specific-model-proxy-secret"
	job, err := fixture.Queue.EnqueueJob(context.Background(), flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  []string{writeWorkerScript(t, "#!/bin/sh\nprintf '%s' \"$HARNESS_MODEL_PROXY_API_KEY\"\n")},
				"shell": false,
				"env": map[string]string{
					"HARNESS_MODEL_PROXY_API_KEY": credential,
				},
			}, "blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	registerAssignedWorker(t, fixture, job, cfg.WorkerID, nil)
	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	policy, err := cfg.History.Resolve(cfg.WorkDir)
	if err != nil {
		t.Fatalf("resolve history policy: %v", err)
	}
	manager, err := newHistoryCaptureManager(client, cfg, policy)
	if err != nil {
		t.Fatalf("new history capture manager: %v", err)
	}
	if err := manager.Replay(context.Background()); err != nil {
		t.Fatalf("initial history replay: %v", err)
	}
	timings.History = manager

	var stdout bytes.Buffer
	err = runWorkerOneShot(context.Background(), client, cfg, timings, nil, &stdout)
	if !errors.Is(err, historyarchive.ErrSensitiveContent) {
		t.Fatalf("runWorkerOneShot() error = %v, want sensitive archive rejection; stdout:\n%s", err, stdout.String())
	}
	current, getErr := fixture.Queue.GetJob(context.Background(), job.ID)
	if getErr != nil {
		t.Fatalf("get job: %v", getErr)
	}
	if flowworker.IsTerminalJobState(current.State) {
		t.Fatalf("job state = %q after mandatory capture rejection, want active for recovery", current.State)
	}
	entries, readErr := filepath.Glob(filepath.Join(policy.OutboxPath, "*", "state.json"))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("outbox state files = %v, err=%v", entries, readErr)
	}
	state, readErr := os.ReadFile(entries[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(state, []byte(credential)) {
		t.Fatal("job-specific model proxy credential persisted in plaintext outbox state")
	}
}

func TestRunWorkerOneShotExitsZeroAfterJobError(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := oneShotTestSetup(t)

	// The entrypoint never runs because the base exchange ref is missing: a
	// job-scoped failure the worker reports to the coordinator.
	job, err := fixture.Queue.EnqueueJob(context.Background(), flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"base":   "missing-base",
			"branch": "missing-base",
			"entrypoint": map[string]any{
				"argv":  []string{"true"},
				"shell": false,
			}, "blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue failing job: %v", err)
	}
	registerAssignedWorker(t, fixture, job, cfg.WorkerID, nil)

	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkerOneShot(context.Background(), client, cfg, timings, nil, &stdout)
	}()

	waitForWorkerJobState(t, fixture, job.ID, flowworker.JobFailed, 30*time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorkerOneShot() error = %v, want nil after job error; stdout:\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("one-shot worker did not exit after job error; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "job error:") {
		t.Fatalf("stdout missing job error:\n%s", stdout.String())
	}
}

func TestRunWorkerOneShotCancelsLongPollPromptly(t *testing.T) {
	t.Parallel()
	claimStarted := make(chan struct{})
	releaseClaim := make(chan struct{})
	defer close(releaseClaim)
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/workers/heartbeat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(contract.WorkerResponse{Worker: flowworker.Worker{ID: "w-cancel"}})
		case "/v2/workers/claim":
			close(claimStarted)
			select {
			case <-r.Context().Done():
			case <-releaseClaim:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(coordinator.Close)
	client, err := flowclient.New(config.ClientConfig{ServerURL: coordinator.URL, Token: "worker-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWorkerOneShot(ctx, client, config.WorkerConfig{WorkerID: "w-cancel"}, workerTimings{
			ClaimWait: 30 * time.Second, LeaseDuration: time.Minute, HeartbeatTTL: time.Minute,
		}, nil, io.Discard)
	}()
	select {
	case <-claimStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("one-shot claim did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runWorkerOneShot cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("one-shot worker did not cancel its long poll promptly")
	}
}

func TestRunWorkerRequiresOneShot(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runWorker(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("runWorker() exit = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --one-shot") {
		t.Fatalf("runWorker() stderr = %q, want one-shot requirement", stderr.String())
	}
}

func TestRunWorkerRejectsRemovedEphemeralFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runWorker([]string{"--ephemeral"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runWorker(--ephemeral) exit = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -ephemeral") {
		t.Fatalf("runWorker(--ephemeral) stderr = %q, want normal unknown-flag error", stderr.String())
	}
}
