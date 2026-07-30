package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
)

type workerMaintenanceClient interface {
	ListWorkerReapJobs() ([]flowworker.Job, error)
}

type workerDiskSpace struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

func (s workerDiskSpace) availablePercent() float64 {
	if s.TotalBytes == 0 {
		return 0
	}
	return float64(s.AvailableBytes) * 100 / float64(s.TotalBytes)
}

type workerMaintenanceMetrics struct {
	freeBytes       *metrics.Gauge
	totalBytes      *metrics.Gauge
	jobDirectories  *metrics.Gauge
	diskPressure    *metrics.Gauge
	cleanedJobs     *metrics.Counter
	reclaimedBytes  *metrics.Counter
	cleanupFailures *metrics.Counter
}

func newWorkerMaintenanceMetrics(registry *metrics.Registry) *workerMaintenanceMetrics {
	if registry == nil {
		return nil
	}
	return &workerMaintenanceMetrics{
		freeBytes:       registry.Gauge("flow_worker_workdir_free_bytes", "Bytes available on the filesystem containing the worker work directory."),
		totalBytes:      registry.Gauge("flow_worker_workdir_total_bytes", "Total bytes on the filesystem containing the worker work directory."),
		jobDirectories:  registry.Gauge("flow_worker_job_directories", "Job workspace entries currently retained by this worker."),
		diskPressure:    registry.Gauge("flow_worker_disk_pressure", "Whether this worker has paused claims because work-directory disk space is low."),
		cleanedJobs:     registry.Counter("flow_worker_cleanup_jobs_total", "Job workspaces removed by lifecycle or reconciliation cleanup."),
		reclaimedBytes:  registry.Counter("flow_worker_cleanup_reclaimed_bytes_total", "Increase in filesystem-available bytes observed across job workspace cleanup."),
		cleanupFailures: registry.Counter("flow_worker_cleanup_errors_total", "Worker maintenance errors by operation."),
	}
}

type workerMaintenance struct {
	cfg       config.WorkerConfig
	policy    config.ResolvedWorkerCleanup
	client    workerMaintenanceClient
	metrics   *workerMaintenanceMetrics
	diskReady *atomic.Bool

	mu        sync.Mutex
	pressure  atomic.Bool
	lastSweep time.Time
	now       func() time.Time
	probe     func(string) (workerDiskSpace, error)
	sweep     func(io.Writer)
}

func newWorkerMaintenance(
	cfg config.WorkerConfig,
	policy config.ResolvedWorkerCleanup,
	client *flowclient.Client,
	maintenanceMetrics *workerMaintenanceMetrics,
	diskReady *atomic.Bool,
) *workerMaintenance {
	manager := &workerMaintenance{
		cfg:       cfg,
		policy:    policy,
		client:    client,
		metrics:   maintenanceMetrics,
		diskReady: diskReady,
		now:       time.Now,
		probe:     probeWorkerDiskSpace,
	}
	manager.sweep = manager.sweepWorkerState
	return manager
}

// Run performs maintenance on the configured interval even while every claim
// slot is occupied by a long-running job.
func (m *workerMaintenance) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(m.policy.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Maintain(false, nil)
		}
	}
}

// Maintain performs due reconciliation, refreshes disk metrics, and returns
// whether another job may be claimed. force is used once at startup.
func (m *workerMaintenance) Maintain(force bool, report io.Writer) bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now().UTC()
	swept := false
	if force || m.lastSweep.IsZero() || now.Sub(m.lastSweep) >= m.policy.Interval {
		m.sweep(report)
		m.lastSweep = now
		swept = true
	}

	space, err := m.sampleDiskLocked()
	if err == nil && !swept && !m.pressure.Load() && belowWorkerDiskLowWatermark(space, m.policy) {
		// A newly observed low watermark gets one immediate cleanup pass even if
		// the regular interval has not elapsed.
		m.sweep(nil)
		m.lastSweep = now
		space, err = m.sampleDiskLocked()
	}
	m.updatePressureLocked(space, err)
	return !m.pressure.Load()
}

// CleanupFinalized removes a job workspace after coordinator finalization. It
// is deliberately best-effort: cleanup must never rewrite the job outcome.
func (m *workerMaintenance) CleanupFinalized(jobID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	before, beforeErr := m.probe(m.cfg.WorkDir)
	removed, err := workerexec.CleanupFinalizedJob(m.cfg.WorkDir, jobID)
	if err != nil {
		m.recordError("finalize")
		slog.Warn("flow-worker finalized workspace cleanup failed", "job_id", jobID, "error", err)
		return
	}
	if removed {
		if m.metrics != nil {
			m.metrics.cleanedJobs.Inc(map[string]string{"reason": "finalized"})
		}
		slog.Debug("flow-worker removed finalized job workspace", "job_id", jobID)
	}
	if count, err := workerexec.CountJobWorkspaces(m.cfg.WorkDir); err == nil {
		m.setJobDirectoryMetric(count)
	} else {
		m.recordError("count")
	}
	after, probeErr := m.sampleDiskLocked()
	if probeErr == nil && beforeErr == nil {
		if removed && after.AvailableBytes > before.AvailableBytes && m.metrics != nil {
			m.metrics.reclaimedBytes.Add(float64(after.AvailableBytes-before.AvailableBytes), map[string]string{"reason": "finalized"})
		}
	}
	m.updatePressureLocked(after, probeErr)
}

func (m *workerMaintenance) sweepWorkerState(report io.Writer) {
	jobs, err := m.client.ListWorkerReapJobs()
	if err != nil {
		m.recordError("list_jobs")
		slog.Debug("flow-worker maintenance failed to list jobs", "error", err)
		if report != nil {
			fmt.Fprintf(report, "reap orphaned workspaces: list jobs: %v\n", err)
		}
		return
	}

	killed, reapErr := workerexec.ReapOrphanedSessions(context.Background(), jobs, workerexec.WithWorkerConfig(m.cfg))
	slog.Debug("flow-worker reaped orphaned tmux sessions", "killed", killed, "error", reapErr)
	if report != nil {
		fmt.Fprintf(report, "reaped orphaned tmux sessions: %d\n", killed)
	}
	if reapErr != nil {
		m.recordError("reap_tmux")
		if report != nil {
			fmt.Fprintf(report, "reap orphaned tmux sessions: %v\n", reapErr)
		}
		// Do not remove a workspace whose tmux server may still be using its
		// socket or files. The next maintenance cycle will retry.
		return
	}

	result, cleanupErr := workerexec.ReapOrphanedJobWorkspaces(
		m.cfg.WorkDir,
		jobs,
		m.policy.OrphanGrace,
		m.now().UTC(),
	)
	m.setJobDirectoryMetric(result.Remaining)
	if result.Removed > 0 && m.metrics != nil {
		m.metrics.cleanedJobs.Add(float64(result.Removed), map[string]string{"reason": "reconciled"})
	}
	slog.Debug(
		"flow-worker reconciled job workspaces",
		"removed", result.Removed,
		"remaining", result.Remaining,
		"error", cleanupErr,
	)
	if report != nil {
		fmt.Fprintf(report, "reaped orphaned job workspaces: %d\n", result.Removed)
	}
	if cleanupErr != nil {
		m.recordError("reap_workspaces")
		if report != nil {
			fmt.Fprintf(report, "reap orphaned job workspaces: %v\n", cleanupErr)
		}
	}
}

func (m *workerMaintenance) sampleDiskLocked() (workerDiskSpace, error) {
	space, err := m.probe(m.cfg.WorkDir)
	if err != nil {
		m.recordError("statfs")
		slog.Warn("flow-worker work directory disk probe failed", "work_dir", m.cfg.WorkDir, "error", err)
		return workerDiskSpace{}, err
	}
	if m.metrics != nil {
		m.metrics.freeBytes.Set(float64(space.AvailableBytes), nil)
		m.metrics.totalBytes.Set(float64(space.TotalBytes), nil)
	}
	return space, nil
}

func (m *workerMaintenance) updatePressureLocked(space workerDiskSpace, probeErr error) {
	previous := m.pressure.Load()
	next := true
	if probeErr == nil {
		if previous {
			next = !aboveWorkerDiskHighWatermark(space, m.policy)
		} else {
			next = belowWorkerDiskLowWatermark(space, m.policy)
		}
	}
	m.pressure.Store(next)
	if m.diskReady != nil {
		m.diskReady.Store(!next)
	}
	if m.metrics != nil {
		if next {
			m.metrics.diskPressure.Set(1, nil)
		} else {
			m.metrics.diskPressure.Set(0, nil)
		}
	}
	if previous != next {
		slog.Warn(
			"flow-worker disk pressure changed",
			"active", next,
			"available_bytes", space.AvailableBytes,
			"available_percent", space.availablePercent(),
		)
	}
}

func (m *workerMaintenance) setJobDirectoryMetric(count int) {
	if m.metrics != nil {
		m.metrics.jobDirectories.Set(float64(count), nil)
	}
}

func (m *workerMaintenance) recordError(operation string) {
	if m.metrics != nil {
		m.metrics.cleanupFailures.Inc(map[string]string{"operation": operation})
	}
}

func belowWorkerDiskLowWatermark(space workerDiskSpace, policy config.ResolvedWorkerCleanup) bool {
	bytesLow := policy.MinFreeBytes > 0 && space.AvailableBytes < policy.MinFreeBytes
	percentLow := space.availablePercent() < policy.MinFreePercent
	return bytesLow || percentLow
}

func aboveWorkerDiskHighWatermark(space workerDiskSpace, policy config.ResolvedWorkerCleanup) bool {
	bytesRecovered := policy.ResumeFreeBytes == 0 || space.AvailableBytes >= policy.ResumeFreeBytes
	percentRecovered := space.availablePercent() >= policy.ResumeFreePercent
	return bytesRecovered && percentRecovered
}

func probeWorkerDiskSpace(path string) (workerDiskSpace, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return workerDiskSpace{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	if stat.Bsize <= 0 {
		return workerDiskSpace{}, fmt.Errorf("statfs %s returned invalid block size %d", path, stat.Bsize)
	}
	blockSize := uint64(stat.Bsize)
	return workerDiskSpace{
		TotalBytes:     stat.Blocks * blockSize,
		AvailableBytes: stat.Bavail * blockSize,
	}, nil
}
