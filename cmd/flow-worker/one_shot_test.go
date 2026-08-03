package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// oneShotTestSetup registers an ephemeral-capable worker and returns the
// fixture, config, and timings wired to a test coordinator.
func oneShotTestSetup(t *testing.T) (workerTestFixture, config.WorkerConfig, workerTimings) {
	t.Helper()
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

	coordinatorServer := httptest.NewServer(fixture.Server)
	t.Cleanup(coordinatorServer.Close)

	cfg := config.WorkerConfig{
		WorkerID:       "w-local",
		CoordinatorURL: coordinatorServer.URL,
		Token:          "worker-token",
		WorkDir:        t.TempDir(),
		Tmux: config.WorkerTmuxConfig{
			SocketPath: isolatedWorkerTmuxSocket(t),
		},
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
			},
		},
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

func TestRunWorkerOneShotWaitsForClaim(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := oneShotTestSetup(t)

	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkerOneShot(context.Background(), client, cfg, timings, nil, &stdout)
	}()

	// Enqueue only after the worker is already polling: the one-shot worker
	// must keep long-polling instead of exiting when the queue is empty.
	time.Sleep(300 * time.Millisecond)
	out := filepath.Join(t.TempDir(), "late.out")
	script := writeWorkerScript(t, `#!/bin/sh
printf late-ok > "$1"
`)
	job := enqueueEphemeralTestJob(t, fixture, script, out)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorkerOneShot() error = %v; stdout:\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("one-shot worker did not exit after one job; stdout:\n%s", stdout.String())
	}

	finished, err := fixture.Queue.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("job state = %q, want finished", finished.State)
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
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue failing job: %v", err)
	}

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

func TestRunWorkerRejectsOnceAndOneShot(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runWorker([]string{"--once", "--one-shot"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runWorker(--once --one-shot) exit = %d, want 2; stderr: %s", code, stderr.String())
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
