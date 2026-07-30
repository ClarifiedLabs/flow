package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// ephemeralTestSetup registers an ephemeral-capable worker and returns the
// fixture, config, and timings wired to a test coordinator.
func ephemeralTestSetup(t *testing.T) (workerTestFixture, config.WorkerConfig, workerTimings) {
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

func TestRunWorkerEphemeralRunsOneJobAndExits(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := ephemeralTestSetup(t)

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
	if err := runWorkerEphemeral(client, cfg, timings, &stdout); err != nil {
		t.Fatalf("runWorkerEphemeral() error = %v; stdout:\n%s", err, stdout.String())
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

func TestRunWorkerEphemeralWaitsForClaim(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := ephemeralTestSetup(t)

	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkerEphemeral(client, cfg, timings, &stdout)
	}()

	// Enqueue only after the worker is already polling: the ephemeral worker
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
			t.Fatalf("runWorkerEphemeral() error = %v; stdout:\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("ephemeral worker did not exit after one job; stdout:\n%s", stdout.String())
	}

	finished, err := fixture.Queue.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("job state = %q, want finished", finished.State)
	}
}

func TestRunWorkerEphemeralExitsZeroAfterJobError(t *testing.T) {
	t.Parallel()
	fixture, cfg, timings := ephemeralTestSetup(t)

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
		done <- runWorkerEphemeral(client, cfg, timings, &stdout)
	}()

	waitForWorkerJobState(t, fixture, job.ID, flowworker.JobFailed, 30*time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorkerEphemeral() error = %v, want nil after job error; stdout:\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("ephemeral worker did not exit after job error; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "job error:") {
		t.Fatalf("stdout missing job error:\n%s", stdout.String())
	}
}

func TestRunWorkerRejectsOnceAndEphemeral(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runWorker([]string{"--once", "--ephemeral"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runWorker(--once --ephemeral) exit = %d, want 2; stderr: %s", code, stderr.String())
	}
}
