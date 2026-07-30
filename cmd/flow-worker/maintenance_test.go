package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

type stubMaintenanceClient struct {
	jobs []flowworker.Job
	err  error
}

func (c stubMaintenanceClient) ListWorkerReapJobs() ([]flowworker.Job, error) {
	return c.jobs, c.err
}

func TestWorkerMaintenancePausesAtLowWatermarkAndUsesResumeHysteresis(t *testing.T) {
	now := time.Now().UTC()
	spaces := []workerDiskSpace{
		{TotalBytes: 100, AvailableBytes: 9},
		{TotalBytes: 100, AvailableBytes: 9},
		{TotalBytes: 100, AvailableBytes: 14},
		{TotalBytes: 100, AvailableBytes: 15},
	}
	probeCall := 0
	sweepCall := 0
	var diskReady atomic.Bool
	diskReady.Store(true)
	manager := &workerMaintenance{
		cfg: config.WorkerConfig{WorkDir: t.TempDir()},
		policy: config.ResolvedWorkerCleanup{
			Interval:          time.Hour,
			OrphanGrace:       time.Hour,
			MinFreePercent:    10,
			ResumeFreePercent: 15,
		},
		diskReady: &diskReady,
		lastSweep: now,
		now:       func() time.Time { return now },
		probe: func(string) (workerDiskSpace, error) {
			space := spaces[probeCall]
			probeCall++
			return space, nil
		},
		sweep: func(io.Writer) {
			sweepCall++
		},
	}

	if manager.Maintain(false, nil) {
		t.Fatal("Maintain allowed a claim below the low watermark")
	}
	if sweepCall != 1 {
		t.Fatalf("low-watermark sweeps = %d, want 1 immediate sweep", sweepCall)
	}
	if diskReady.Load() {
		t.Fatal("disk readiness stayed true under pressure")
	}
	if manager.Maintain(false, nil) {
		t.Fatal("Maintain resumed before the high watermark")
	}
	if !manager.Maintain(false, nil) {
		t.Fatal("Maintain did not resume at the high watermark")
	}
	if !diskReady.Load() {
		t.Fatal("disk readiness stayed false after recovery")
	}
}

func TestWorkerMaintenanceRunsOnIntervalWhileNoClaimLoopAdvances(t *testing.T) {
	swept := make(chan struct{}, 1)
	manager := &workerMaintenance{
		cfg: config.WorkerConfig{WorkDir: t.TempDir()},
		policy: config.ResolvedWorkerCleanup{
			Interval:          10 * time.Millisecond,
			OrphanGrace:       time.Hour,
			MinFreePercent:    10,
			ResumeFreePercent: 15,
		},
		now: time.Now,
		probe: func(string) (workerDiskSpace, error) {
			return workerDiskSpace{TotalBytes: 100, AvailableBytes: 90}, nil
		},
		sweep: func(io.Writer) {
			select {
			case swept <- struct{}{}:
			default:
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.Run(ctx)
	}()

	select {
	case <-swept:
	case <-time.After(time.Second):
		t.Fatal("maintenance interval elapsed without a sweep")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance loop did not stop after cancellation")
	}
}

func TestWorkerMaintenanceDoesNotDeleteWhenCoordinatorListingFails(t *testing.T) {
	workDir := t.TempDir()
	jobPath := filepath.Join(workDir, "jobs", "j-unknown")
	if err := os.MkdirAll(jobPath, 0o700); err != nil {
		t.Fatalf("create unknown job workspace: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(jobPath, old, old); err != nil {
		t.Fatalf("age unknown job workspace: %v", err)
	}
	var diskReady atomic.Bool
	diskReady.Store(true)
	manager := &workerMaintenance{
		cfg:       config.WorkerConfig{WorkDir: workDir},
		policy:    config.ResolvedWorkerCleanup{Interval: time.Minute, OrphanGrace: time.Hour, MinFreePercent: 1, ResumeFreePercent: 2},
		client:    stubMaintenanceClient{err: errors.New("coordinator unavailable")},
		diskReady: &diskReady,
		now:       time.Now,
		probe: func(string) (workerDiskSpace, error) {
			return workerDiskSpace{TotalBytes: 100, AvailableBytes: 90}, nil
		},
	}
	manager.sweep = manager.sweepWorkerState

	if !manager.Maintain(true, nil) {
		t.Fatal("coordinator listing failure incorrectly caused disk pressure")
	}
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("workspace was deleted without coordinator state: %v", err)
	}
}

func TestWorkerDiskWatermarksCombineByteAndPercentThresholds(t *testing.T) {
	policy := config.ResolvedWorkerCleanup{
		MinFreeBytes:      20,
		ResumeFreeBytes:   40,
		MinFreePercent:    10,
		ResumeFreePercent: 15,
	}
	if !belowWorkerDiskLowWatermark(workerDiskSpace{TotalBytes: 1000, AvailableBytes: 19}, policy) {
		t.Fatal("absolute low watermark was ignored")
	}
	if !belowWorkerDiskLowWatermark(workerDiskSpace{TotalBytes: 1000, AvailableBytes: 99}, policy) {
		t.Fatal("percentage low watermark was ignored")
	}
	if aboveWorkerDiskHighWatermark(workerDiskSpace{TotalBytes: 1000, AvailableBytes: 100}, policy) {
		t.Fatal("high watermark accepted insufficient percentage")
	}
	if !aboveWorkerDiskHighWatermark(workerDiskSpace{TotalBytes: 1000, AvailableBytes: 150}, policy) {
		t.Fatal("high watermark did not accept recovered disk")
	}
}
