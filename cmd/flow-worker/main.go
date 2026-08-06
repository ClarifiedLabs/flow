package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/version"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
	"github.com/ClarifiedLabs/flow/internal/worker/historycapture"
	"github.com/ClarifiedLabs/flow/internal/worker/terminalbridge"
)

const transientWorkerRetryDelay = time.Second

// jobError marks a failure scoped to a single claimed job. The worker has
// already reported the outcome to the coordinator (release, check verdict, or
// discarded-on-lease-loss), so a job-scoped failure exits 0 instead of
// failing the process.
type jobError struct{ err error }

func (e *jobError) Error() string { return e.err.Error() }
func (e *jobError) Unwrap() error { return e.err }

func jobFailure(err error) error {
	if err == nil {
		return nil
	}
	return &jobError{err: err}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	configuredArgs, restoreLogging, err := flowlog.Configure(args, stderr, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configure logging: %v\n", err)
		return 2
	}
	defer restoreLogging()
	args = configuredArgs
	slog.Debug("flow-worker command start", "command", flowlog.CommandName(args))

	if len(args) == 0 {
		fmt.Fprintln(stderr, "flow-worker requires the managed `run --one-shot` command")
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "--version", "version":
		fmt.Fprintf(stdout, "flow-worker %s\n", version.Current())
		return 0
	case "run":
		return runWorker(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("config", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	flags.StringVar(&configPath, "c", "", "worker config path")
	flags.StringVar(&configPath, "config", "", "worker config path")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := loadWorkerConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load worker config: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "worker_id: %s\n", cfg.WorkerID)
	fmt.Fprintf(stdout, "coordinator_url: %s\n", cfg.CoordinatorURL)
	fmt.Fprintf(stdout, "work_dir: %s\n", cfg.WorkDir)
	fmt.Fprintf(stdout, "protocol: %s\n", contract.ProtocolVersion)
	fmt.Fprintf(stdout, "labels: %d\n", len(cfg.Labels))
	cleanup, _ := cfg.Cleanup.Resolve()
	history, _ := cfg.History.Resolve(cfg.WorkDir)
	fmt.Fprintf(stdout, "cleanup_interval: %s\n", cleanup.Interval)
	fmt.Fprintf(stdout, "cleanup_orphan_grace: %s\n", cleanup.OrphanGrace)
	fmt.Fprintf(stdout, "cleanup_min_free_bytes: %d\n", cleanup.MinFreeBytes)
	fmt.Fprintf(stdout, "cleanup_resume_free_bytes: %d\n", cleanup.ResumeFreeBytes)
	fmt.Fprintf(stdout, "cleanup_min_free_percent: %g\n", cleanup.MinFreePercent)
	fmt.Fprintf(stdout, "cleanup_resume_free_percent: %g\n", cleanup.ResumeFreePercent)
	fmt.Fprintf(stdout, "history_transcript_segment_bytes: %d\n", history.Transcript.SegmentBytes)
	fmt.Fprintf(stdout, "history_transcript_flush_interval: %s\n", history.Transcript.FlushInterval)
	fmt.Fprintf(stdout, "history_archive_max_stored_bytes: %d\n", history.Archive.MaxStoredBytes)
	fmt.Fprintf(stdout, "history_outbox_path: %s\n", history.OutboxPath)
	fmt.Fprintf(stdout, "history_outbox_replay_interval: %s\n", history.ReplayInterval)
	fmt.Fprintf(stdout, "history_mandatory_final_capture: %t\n", history.MandatoryFinal)
	fmt.Fprintf(stdout, "history_stop_transcript_wakeup: %t\n", history.StopWakeup)
	fmt.Fprintf(stdout, "history_live_checkpoints: %t\n", history.LiveCheckpoints.Enabled)
	return 0
}

func runWorker(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	var oneShot bool
	var noMetrics bool
	var metricsListen string
	var claimWait time.Duration
	var leaseDuration time.Duration
	var heartbeatTTL time.Duration
	var gitCommitName string
	var gitCommitEmail string
	flags.StringVar(&configPath, "c", "", "worker config path")
	flags.StringVar(&configPath, "config", "", "worker config path")
	flags.BoolVar(&oneShot, "one-shot", false, "wait for and run the capacity slot's single bound job, then exit (required)")
	flags.BoolVar(&noMetrics, "no-metrics", false, "disable the telemetry endpoint (/readyz, /livez, /metrics)")
	flags.StringVar(&metricsListen, "metrics-listen", "", "telemetry endpoint listen address")
	flags.DurationVar(&claimWait, "claim-wait", 30*time.Second, "claim long-poll duration")
	flags.DurationVar(&leaseDuration, "lease", 60*time.Second, "lease duration")
	flags.DurationVar(&heartbeatTTL, "heartbeat-ttl", 60*time.Second, "worker heartbeat TTL")
	flags.StringVar(&gitCommitName, "git-commit-name", "", "git user.name for agent commits in the job worktree")
	flags.StringVar(&gitCommitEmail, "git-commit-email", "", "git user.email for agent commits in the job worktree")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "flow-worker does not accept positional arguments")
		return 2
	}
	if !oneShot {
		fmt.Fprintln(stderr, "flow-worker run requires --one-shot: capacity workers bind at most once")
		return 2
	}

	cfg, _, err := loadWorkerConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load worker config: %v\n", err)
		return 1
	}
	if strings.TrimSpace(gitCommitName) != "" {
		cfg.Git.CommitName = strings.TrimSpace(gitCommitName)
	}
	if strings.TrimSpace(gitCommitEmail) != "" {
		cfg.Git.CommitEmail = strings.TrimSpace(gitCommitEmail)
	}
	if err := config.ValidateWorkerCommitIdentity(cfg.Git); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	cleanupPolicy, err := cfg.Cleanup.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "resolve worker cleanup policy: %v\n", err)
		return 1
	}
	historyPolicy, err := cfg.History.Resolve(cfg.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "resolve worker history policy: %v\n", err)
		return 1
	}
	if strings.TrimSpace(cfg.WorkerID) == "" {
		fmt.Fprintln(stderr, "worker config worker_id is required")
		return 1
	}
	if strings.TrimSpace(cfg.Token) == "" {
		fmt.Fprintln(stderr, "worker config token is required: workers authenticate with their capacity-slot credential")
		return 1
	}
	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "jobs"), 0o700); err != nil {
		fmt.Fprintf(stderr, "create worker jobs directory: %v\n", err)
		return 1
	}

	// Telemetry: one unauthenticated port serving /readyz (set once the worker
	// has registered with the coordinator and is not under disk pressure),
	// /livez, and /metrics.
	var registeredReady atomic.Bool
	var diskReady atomic.Bool
	diskReady.Store(true)
	telemetryRegistry := metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "flow_worker_build_info",
		Help:    "flow-worker build information.",
		Version: version.Current().String(),
	})
	jobMetrics.Store(&workerJobMetrics{
		claimed:   telemetryRegistry.Counter("flow_jobs_claimed_total", "Jobs claimed by this worker."),
		completed: telemetryRegistry.Counter("flow_jobs_completed_total", "Jobs released by this worker by final state."),
	})
	maintenanceMetrics := newWorkerMaintenanceMetrics(telemetryRegistry)
	telemetrySettings := metrics.Resolve(cfg.Metrics, config.DefaultTelemetryListen, metrics.Overrides{
		Disable:    noMetrics,
		DisableSet: noMetrics,
		Listen:     metricsListen,
		ListenSet:  strings.TrimSpace(metricsListen) != "",
	})
	ready := func() bool {
		return registeredReady.Load() && diskReady.Load()
	}
	executionCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	telemetry, err := metrics.StartEndpoint(executionCtx, slog.Default(), metrics.Mux(telemetryRegistry, ready), telemetrySettings)
	if err != nil {
		fmt.Fprintf(stderr, "start telemetry endpoint: %v\n", err)
		return 1
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), metrics.DefaultShutdownTimeout)
		defer shutdownCancel()
		_ = telemetry.Shutdown(shutdownCtx)
	}()

	jobRegistry := executionJobRegistry{}
	controlClient := terminalbridge.NewControlClient(cfg.CoordinatorURL, cfg.WorkerID, cfg.Token, jobRegistry)
	controlCtx, controlCancel := context.WithCancel(executionCtx)
	defer controlCancel()
	go func() {
		if err := controlClient.Run(controlCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("worker control client exited", "error", err)
		}
	}()

	slog.Debug("flow-worker worker configuration loaded",
		"worker_id", cfg.WorkerID,
		"coordinator_url", cfg.CoordinatorURL,
		"work_dir", cfg.WorkDir,
		"history_transcript_segment_bytes", historyPolicy.Transcript.SegmentBytes,
		"history_transcript_flush_interval", historyPolicy.Transcript.FlushInterval,
		"history_archive_max_stored_bytes", historyPolicy.Archive.MaxStoredBytes,
		"history_outbox_replay_interval", historyPolicy.ReplayInterval,
		"history_mandatory_final_capture", historyPolicy.MandatoryFinal,
		"history_stop_transcript_wakeup", historyPolicy.StopWakeup,
		"history_live_checkpoints", historyPolicy.LiveCheckpoints.Enabled,
	)

	client, err := newWorkerClient(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	maintenance := newWorkerMaintenance(cfg, cleanupPolicy, client, maintenanceMetrics, &diskReady)
	registered, err := registerWorkerWithRetry(executionCtx, client, cfg, heartbeatTTL, stderr)
	if err != nil {
		if executionCtx.Err() != nil {
			return 0
		}
		fmt.Fprintf(stderr, "register worker: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "registered: %s\n", registered.ID)

	historyManager, err := newHistoryCaptureManager(client, cfg, historyPolicy)
	if err != nil {
		fmt.Fprintf(stderr, "open worker history outbox: %v\n", err)
		return 1
	}
	maintenance.protectedJobs = historyManager.ProtectedJobIDs
	if err := historyManager.Replay(executionCtx); err != nil && executionCtx.Err() == nil {
		slog.Warn("initial worker history outbox replay failed; claims paused", "error", err)
	}
	historyCtx, historyCancel := context.WithCancel(executionCtx)
	historyDone := make(chan struct{})
	go func() {
		defer close(historyDone)
		historyManager.Run(historyCtx)
	}()
	defer func() {
		historyCancel()
		<-historyDone
	}()

	maintenance.Maintain(true, stderr)
	registeredReady.Store(true)
	maintenanceCtx, maintenanceCancel := context.WithCancel(executionCtx)
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		maintenance.Run(maintenanceCtx)
	}()
	defer func() {
		maintenanceCancel()
		<-maintenanceDone
	}()

	timings := workerTimings{
		ClaimWait:     claimWait,
		LeaseDuration: leaseDuration,
		HeartbeatTTL:  heartbeatTTL,
		History:       historyManager,
	}
	runOutput := &lockedWriter{writer: stdout}
	runErr := runWorkerOneShot(executionCtx, client, cfg, timings, maintenance, runOutput)
	if runErr != nil {
		if executionCtx.Err() != nil {
			return 0
		}
		fmt.Fprintf(stderr, "%v\n", runErr)
		return 1
	}
	return 0
}

// workerJobMetrics are the per-worker job counters. The package-level atomic
// handle is nil-safe so claim paths can count without threading state through
// every helper, and parallel entrypoint tests cannot race with claim loops.
type workerJobMetrics struct {
	claimed   *metrics.Counter
	completed *metrics.Counter
}

var jobMetrics atomic.Pointer[workerJobMetrics]

func (m *workerJobMetrics) jobClaimed() {
	if m == nil {
		return
	}
	m.claimed.Inc(nil)
}

func (m *workerJobMetrics) jobCompleted(result string) {
	if m == nil {
		return
	}
	m.completed.Inc(map[string]string{"result": result})
}

// runWorkerOneShot keeps long-polling until a job is claimed, runs exactly
// that one job, then returns. A job-scoped failure has already been reported
// to the coordinator (release, check verdict, or discarded-on-lease-loss), so
// it is not a process failure: the worker exits 0 either way.
func runWorkerOneShot(ctx context.Context, client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, stdout io.Writer) error {
	for {
		slog.Debug("flow-worker one-shot claim attempt", "worker_id", cfg.WorkerID)
		claimed, err := runWorkerOnce(ctx, client, cfg, timings, maintenance, stdout)
		if err != nil {
			if flowclient.IsRetryableError(err) {
				slog.Debug("flow-worker transient worker error", "worker_id", cfg.WorkerID, "error", err)
				fmt.Fprintf(stdout, "worker transient error: %v; retrying in %s\n", err, transientWorkerRetryDelay)
				if err := waitWorkerContext(ctx, transientWorkerRetryDelay); err != nil {
					return err
				}
				continue
			}
			var jobErr *jobError
			if errors.As(err, &jobErr) {
				fmt.Fprintf(stdout, "job error: %v; exiting\n", err)
				return nil
			}
			return err
		}
		if claimed {
			return nil
		}
		if timings.ClaimWait <= 0 {
			if err := waitWorkerContext(ctx, time.Second); err != nil {
				return err
			}
		}
	}
}

type workerTimings struct {
	ClaimWait     time.Duration
	LeaseDuration time.Duration
	HeartbeatTTL  time.Duration
	History       *historyCaptureManager
}

type executionJobRegistry struct{}

func (executionJobRegistry) Register(jobID string)   { workerexec.RegisterActiveJob(jobID) }
func (executionJobRegistry) Unregister(jobID string) { workerexec.UnregisterActiveJob(jobID) }
func (executionJobRegistry) Has(jobID string) bool   { return workerexec.IsActiveJob(jobID) }

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func waitWorkerContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func runWorkerOnce(ctx context.Context, client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, stdout io.Writer) (bool, error) {
	slog.Debug("flow-worker heartbeat worker", "worker_id", cfg.WorkerID, "heartbeat_ttl", timings.HeartbeatTTL)
	heartbeat, err := client.HeartbeatWorkerContext(ctx, flowclient.HeartbeatWorkerInput{
		WorkerID:     cfg.WorkerID,
		HeartbeatTTL: timings.HeartbeatTTL,
	})
	if err != nil {
		return false, fmt.Errorf("heartbeat worker: %w", err)
	}
	fmt.Fprintf(stdout, "heartbeat: %s\n", heartbeat.ID)
	if !maintenance.Maintain(false, nil) {
		slog.Debug("flow-worker claim paused by disk pressure", "worker_id", cfg.WorkerID)
		return false, nil
	}
	if timings.History != nil && !timings.History.CanClaim() {
		slog.Debug("flow-worker claim paused by pending history outbox", "worker_id", cfg.WorkerID)
		return false, nil
	}

	slog.Debug("flow-worker claim job", "worker_id", cfg.WorkerID, "claim_wait", timings.ClaimWait, "lease_duration", timings.LeaseDuration)
	labels, _ := registrationLabelsWithAvailability(cfg.Labels)
	harnessModels := registrationHarnessModels(labels)
	claim, err := client.ClaimJobContext(ctx, flowclient.ClaimJobInput{
		WorkerID:             cfg.WorkerID,
		LeaseDuration:        timings.LeaseDuration,
		Wait:                 timings.ClaimWait,
		CapabilitiesReported: true,
		Labels:               labels,
		Taints:               cfg.Taints,
		HarnessModels:        harnessModels,
		HeartbeatTTL:         timings.HeartbeatTTL,
	})
	if err != nil {
		return false, fmt.Errorf("claim job: %w", err)
	}
	if !claim.Claimed {
		slog.Debug("flow-worker no job claimed", "worker_id", cfg.WorkerID)
		fmt.Fprintln(stdout, "claimed: none")
		return false, nil
	}
	if claim.Job == nil || claim.Lease == nil {
		return false, fmt.Errorf("claim job: malformed response")
	}
	if strings.TrimSpace(claim.ProjectID) == "" {
		return false, fmt.Errorf("claim job: project id is required")
	}
	slog.Debug("flow-worker job claimed",
		"worker_id", cfg.WorkerID,
		"job_id", claim.Job.ID,
		"lease_id", claim.Lease.ID,
		"role", claim.Job.Role,
		"bucket", claim.Job.CapacityBucket,
	)
	fmt.Fprintf(stdout, "claimed: %s lease=%s\n", claim.Job.ID, claim.Lease.ID)
	jobMetrics.Load().jobClaimed()
	running, err := client.MarkJobRunning(claim.Lease.ID)
	if err != nil {
		return true, jobFailure(fmt.Errorf("mark job running: %w", err))
	}
	slog.Debug("flow-worker job running", "job_id", running.Job.ID, "state", running.Job.State)
	fmt.Fprintf(stdout, "running: %s state=%s\n", running.Job.ID, running.Job.State)

	persistentSession := running.Session != nil
	if running.Job.Payload == nil {
		running.Job.Payload = map[string]any{}
	}
	running.Job.Payload["project_id"] = claim.ProjectID
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(nil)
	leaseHeartbeat := startLeaseHeartbeat(client, cfg, *claim.Lease, timings, stdout, cancelJob)
	sensitiveValues := make([][]byte, 0, 3)
	for _, value := range []string{cfg.Token, running.SessionToken} {
		if value = strings.TrimSpace(value); value != "" {
			sensitiveValues = append(sensitiveValues, []byte(value))
		}
	}
	if payload, decodeErr := workerexec.DecodePayload(running.Job.Payload); decodeErr == nil && payload.Entrypoint != nil {
		if value := strings.TrimSpace(payload.Entrypoint.Env["HARNESS_MODEL_PROXY_API_KEY"]); value != "" {
			sensitiveValues = append(sensitiveValues, []byte(value))
		}
	}
	var captureID string
	var beforeExecution func(workerexec.ExecutionPreparation) error
	if timings.History != nil {
		beforeExecution = func(preparation workerexec.ExecutionPreparation) error {
			var err error
			captureID, err = timings.History.Reserve(jobCtx, running.Job, *claim.Lease, running.Session, preparation, sensitiveValues)
			return err
		}
	}
	result := workerexec.RunJob(jobCtx, workerexec.RunInput{
		Config:          cfg,
		Job:             running.Job,
		Lease:           *claim.Lease,
		ProjectID:       claim.ProjectID,
		Session:         running.Session,
		SessionToken:    running.SessionToken,
		BeforeExecution: beforeExecution,
	})
	if timings.History != nil {
		defer func() { timings.History.deactivate(captureID) }()
	}
	captureInterrupted := historyCaptureWasInterrupted(ctx.Err(), jobCtx.Err(), leaseHeartbeat.Err())
	var reportedCheckVerdict coordinator.CheckVerdict
	var checkErr error
	var staleCheckResult bool
	finalState := result.FinalState
	if !captureInterrupted {
		// Persist the tmux transcript before check reporting can advance lifecycle
		// state. Worker job uploads authenticate with the still-live lease; failures
		// remain best effort because the mandatory capture stores the same source.
		uploadTranscript(jobCtx, client, cfg, *claim.Job, *claim.Lease, running.Session, running.SessionToken, result, stdout)
		captureInterrupted = historyCaptureWasInterrupted(ctx.Err(), jobCtx.Err(), leaseHeartbeat.Err())
	}
	if !captureInterrupted {
		checkCtx, checkCancel := context.WithTimeout(jobCtx, historyShutdownBudget)
		checkErr = retryTransientOperationContext(checkCtx, "report check", stdout, func() error {
			var err error
			reportedCheckVerdict, err = reportCheckIfNeeded(checkCtx, client, *claim.Job, *claim.Lease, result, stdout)
			return err
		})
		checkCancel()
		staleCheckResult = isStaleSourceJobHeadReport(checkErr)
		if staleCheckResult {
			fmt.Fprintf(stdout, "check: %s stale source head; result discarded\n", strings.TrimSpace(result.Payload.CheckName))
		}
		captureInterrupted = historyCaptureWasInterrupted(ctx.Err(), jobCtx.Err(), leaseHeartbeat.Err())
		if !captureInterrupted {
			finalState = finalStateForCheckReport(result, reportedCheckVerdict, checkErr, staleCheckResult)
		}
	}
	terminal := historyTerminalAction(running.Job, running.Session, running.SessionToken, result.ExitCode, finalState)
	if captureInterrupted {
		terminal = nil
	}
	if timings.History != nil {
		// Final capture uses its upload grant rather than the expiring job lease.
		// Detach it so lease loss or host cancellation records a crashed attempt
		// instead of aborting the evidence needed for recovery.
		captureCtx, captureCancel := context.WithTimeout(context.Background(), historyShutdownBudget)
		if captureID == "" {
			preparation := result.HistoryPreparation
			if result.Worktree != "" {
				preparation.Worktree = result.Worktree
			}
			if result.TranscriptPath != "" {
				preparation.TranscriptPath = result.TranscriptPath
			}
			captureID, err = timings.History.Reserve(captureCtx, running.Job, *claim.Lease, running.Session, preparation, sensitiveValues)
		}
		if err == nil {
			err = timings.History.Finalize(captureCtx, captureID, result, captureInterrupted, terminal, sensitiveValues, stdout)
		}
		captureCancel()
		if err != nil {
			heartbeatErr := leaseHeartbeat.Stop()
			if historyCaptureWasInterrupted(ctx.Err(), jobCtx.Err(), heartbeatErr) {
				discardCtx, discardCancel := context.WithTimeout(context.Background(), historyShutdownBudget)
				discardErr := timings.History.discardTerminal(discardCtx, captureID)
				discardCancel()
				if discardErr != nil {
					return true, fmt.Errorf("publish mandatory final history capture: %w; discard stale terminal action: %v", err, discardErr)
				}
			}
			// Publication is worker durability, not an already-reported job failure.
			// A one-shot pod must restart and replay rather than exit successfully.
			return true, fmt.Errorf("publish mandatory final history capture: %w", err)
		}
		// The detached capture may finish after host cancellation or authoritative
		// lease loss. Check again before any live lifecycle operation can consume
		// the terminal checkpoint; replay skips this capture while it is active.
		if historyCaptureWasInterrupted(ctx.Err(), jobCtx.Err(), leaseHeartbeat.Err()) {
			discardCtx, discardCancel := context.WithTimeout(context.Background(), historyShutdownBudget)
			discardErr := timings.History.discardTerminal(discardCtx, captureID)
			discardCancel()
			if discardErr != nil {
				return true, fmt.Errorf("discard stale terminal action after final history capture: %w", discardErr)
			}
		}
		fmt.Fprintf(stdout, "history: %s state=complete\n", captureID)
	}
	if ctx.Err() != nil {
		// Host or pod shutdown is not a user-code result. Stop renewing and leave
		// the lease live so coordinator crash-expiry remains authoritative. Remove
		// any terminal action captured just before the cancellation became visible.
		_ = leaseHeartbeat.Stop()
		discardCtx, discardCancel := context.WithTimeout(context.Background(), historyShutdownBudget)
		discardErr := timings.History.discardTerminal(discardCtx, captureID)
		discardCancel()
		if discardErr != nil {
			return true, fmt.Errorf("host interrupted: %v; discard stale history terminal action: %w", context.Cause(ctx), discardErr)
		}
		return true, context.Cause(ctx)
	}
	cleanupFinalized := false
	defer func() {
		if cleanupFinalized {
			maintenance.CleanupFinalized(running.Job.ID)
		}
	}()
	fmt.Fprintf(stdout, "ran: %s session=%s exit=%d\n", claim.Job.ID, result.Session, result.ExitCode)
	slog.Debug("flow-worker job completed", "job_id", claim.Job.ID, "session", result.Session, "exit_code", result.ExitCode, "final_state", result.FinalState, "error", result.Err)
	if heartbeatErr := leaseHeartbeat.Err(); heartbeatErr != nil {
		_ = leaseHeartbeat.Stop()
		if isLeaseNotRenewable(heartbeatErr) && persistentSession {
			alreadyFinalized, finalizeErr := acknowledgeStoppedPersistentSession(client, running.Job, *claim.Lease, running.Session, result.ExitCode, stdout)
			if finalizeErr != nil {
				discarded, discardErr := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr)
				if discardErr != nil {
					return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %v; discard stale history terminal action: %w", finalizeErr, discardErr))
				}
				if !discarded {
					timings.History.deactivate(captureID)
				}
				return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %w", finalizeErr))
			}
			if alreadyFinalized {
				if running.Job.Role == flowworker.RoleConsole {
					fmt.Fprintf(stdout, "console process exit acknowledged: %s\n", running.Session.ID)
				}
				if err := timings.History.acknowledgeTerminal(context.Background(), captureID); err != nil {
					timings.History.deactivate(captureID)
					return true, jobFailure(fmt.Errorf("acknowledge history terminal lifecycle: %w", err))
				}
				cleanupFinalized = true
				slog.Debug("flow-worker persistent session finalized while lease heartbeat was active", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID)
				fmt.Fprintf(stdout, "persistent session finalized: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
				return true, nil
			}
		}
		if _, err := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr); err != nil {
			return true, jobFailure(fmt.Errorf("lease heartbeat: %v; discard stale history terminal action: %w", heartbeatErr, err))
		}
		slog.Debug("flow-worker discarded job result after authoritative lease loss", "job_id", claim.Job.ID, "lease_id", claim.Lease.ID, "error", heartbeatErr)
		fmt.Fprintf(stdout, "lease lost: %s; discarded local result\n", claim.Lease.ID)
		return true, jobFailure(fmt.Errorf("lease heartbeat: %w", heartbeatErr))
	}
	if persistentSession {
		// A console session normally releases its lease through /v2/console.
		// When an owner has already stopped the console, that route is unavailable
		// because the session token is revoked; in that case the worker must send
		// the process-exit acknowledgement that clears the coordinator's fence.
		if running.Job.Role == flowworker.RoleConsole {
			releaseCtx, releaseCancel := context.WithTimeout(jobCtx, historyShutdownBudget)
			releaseErr := retryTransientOperationContext(releaseCtx, "release console", stdout, func() error {
				return releaseConsoleSession(releaseCtx, cfg, running.SessionToken)
			})
			releaseCancel()
			heartbeatErr := leaseHeartbeat.Stop()
			if releaseErr != nil {
				alreadyFinalized := false
				var finalizeErr error
				if isInvalidBearerToken(releaseErr) {
					alreadyFinalized, finalizeErr = acknowledgeStoppedPersistentSession(client, running.Job, *claim.Lease, running.Session, result.ExitCode, stdout)
				}
				if finalizeErr != nil {
					discarded, discardErr := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr)
					if discardErr != nil {
						return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %v; discard stale history terminal action: %w", finalizeErr, discardErr))
					}
					if !discarded {
						timings.History.deactivate(captureID)
					}
					if heartbeatErr != nil {
						return true, jobFailure(fmt.Errorf("lease heartbeat: %v; acknowledge stopped console process exit: %w", heartbeatErr, finalizeErr))
					}
					return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %w", finalizeErr))
				}
				if alreadyFinalized {
					fmt.Fprintf(stdout, "console process exit acknowledged: %s\n", running.Session.ID)
				} else {
					discarded, discardErr := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr)
					if discardErr != nil {
						return true, jobFailure(fmt.Errorf("release console: %v; discard stale history terminal action: %w", releaseErr, discardErr))
					}
					if !discarded {
						timings.History.deactivate(captureID)
					}
					if heartbeatErr != nil {
						return true, jobFailure(fmt.Errorf("lease heartbeat: %v; release console: %w", heartbeatErr, releaseErr))
					}
					return true, jobFailure(fmt.Errorf("release console: %w", releaseErr))
				}
			} else {
				slog.Debug("flow-worker console session released", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "error", result.Err)
				fmt.Fprintf(stdout, "released: %s state=%s\n", running.Job.ID, flowworker.JobFinished)
			}
			if err := timings.History.acknowledgeTerminal(context.Background(), captureID); err != nil {
				timings.History.deactivate(captureID)
				return true, jobFailure(fmt.Errorf("acknowledge history terminal lifecycle: %w", err))
			}
			cleanupFinalized = true
			if heartbeatErr != nil && !isLeaseNotRenewable(heartbeatErr) {
				return true, jobFailure(fmt.Errorf("lease heartbeat: %w", heartbeatErr))
			}
			if checkErr != nil && !staleCheckResult {
				return true, jobFailure(checkErr)
			}
			if result.Err != nil && !staleCheckResult {
				return true, jobFailure(fmt.Errorf("run job: %w", result.Err))
			}
			return true, nil
		}

		processExitCtx, processExitCancel := context.WithTimeout(jobCtx, historyShutdownBudget)
		alreadyFinalized := persistentSessionFinalized(processExitCtx, client, running.Job, *claim.Lease, running.Session)
		var processExitErr error
		if !alreadyFinalized {
			processExitErr = retryTransientOperationContext(processExitCtx, "report persistent session process exit", stdout, func() error {
				return reportPersistentSessionProcessExit(processExitCtx, client, running.Session, *claim.Lease, result.ExitCode)
			})
		}
		processExitCancel()
		heartbeatErr := leaseHeartbeat.Stop()
		if alreadyFinalized {
			slog.Debug("flow-worker persistent session finalized by coordinator", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "role", running.Job.Role)
			fmt.Fprintf(stdout, "persistent session finalized: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
		} else if processExitErr == nil {
			slog.Debug("flow-worker persistent session process exited", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "role", running.Job.Role)
			fmt.Fprintf(stdout, "persistent session exited: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
		}
		if processExitErr != nil {
			discarded, discardErr := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr)
			if discardErr != nil {
				return true, jobFailure(fmt.Errorf("report persistent session process exit: %v; discard stale history terminal action: %w", processExitErr, discardErr))
			}
			if !discarded {
				timings.History.deactivate(captureID)
			}
			return true, jobFailure(fmt.Errorf("report persistent session process exit: %w", processExitErr))
		}
		if err := timings.History.acknowledgeTerminal(context.Background(), captureID); err != nil {
			timings.History.deactivate(captureID)
			return true, jobFailure(fmt.Errorf("acknowledge history terminal lifecycle: %w", err))
		}
		cleanupFinalized = true
		if heartbeatErr != nil && !isLeaseNotRenewable(heartbeatErr) {
			return true, jobFailure(fmt.Errorf("lease heartbeat: %w", heartbeatErr))
		}
		if checkErr != nil && !staleCheckResult {
			return true, jobFailure(checkErr)
		}
		if result.Err != nil && !staleCheckResult {
			return true, jobFailure(fmt.Errorf("run job: %w", result.Err))
		}
		return true, nil
	}

	var released flowworker.Job
	releaseCtx, releaseCancel := context.WithTimeout(jobCtx, historyShutdownBudget)
	releaseErr := retryTransientOperationContext(releaseCtx, "release lease", stdout, func() error {
		var err error
		released, err = client.ReleaseLeaseContext(releaseCtx, flowclient.ReleaseLeaseInput{
			LeaseID:    claim.Lease.ID,
			FinalState: finalState,
		})
		return err
	})
	releaseCancel()
	heartbeatErr := leaseHeartbeat.Stop()
	if releaseErr != nil {
		discarded, discardErr := discardHistoryTerminalAfterInterruption(timings.History, captureID, ctx, jobCtx, heartbeatErr)
		if discardErr != nil {
			return true, jobFailure(fmt.Errorf("release lease: %v; discard stale history terminal action: %w", releaseErr, discardErr))
		}
		if !discarded {
			timings.History.deactivate(captureID)
		}
		if heartbeatErr != nil {
			return true, jobFailure(fmt.Errorf("lease heartbeat: %v; release lease: %w", heartbeatErr, releaseErr))
		}
		return true, jobFailure(fmt.Errorf("release lease: %w", releaseErr))
	}
	slog.Debug("flow-worker lease released", "job_id", released.ID, "state", released.State)
	fmt.Fprintf(stdout, "released: %s state=%s\n", released.ID, released.State)
	if err := timings.History.acknowledgeTerminal(context.Background(), captureID); err != nil {
		timings.History.deactivate(captureID)
		return true, jobFailure(fmt.Errorf("acknowledge history terminal lifecycle: %w", err))
	}
	cleanupFinalized = true
	jobMetrics.Load().jobCompleted(string(released.State))
	if checkErr != nil && !staleCheckResult {
		return true, jobFailure(checkErr)
	}
	if result.Err != nil &&
		!staleCheckResult &&
		reportedCheckVerdict != coordinator.CheckSatisfied &&
		reportedCheckVerdict != coordinator.CheckBlocked {
		return true, jobFailure(fmt.Errorf("run job: %w", result.Err))
	}
	return true, nil
}

func historyTerminalAction(job flowworker.Job, session *coordinator.Session, sessionToken string, exitCode int, finalState flowworker.JobState) *historycapture.TerminalAction {
	if session == nil {
		return &historycapture.TerminalAction{Kind: historycapture.TerminalReleaseLease, FinalState: string(finalState)}
	}
	if job.Role == flowworker.RoleConsole {
		return &historycapture.TerminalAction{
			Kind: historycapture.TerminalConsoleExit, SessionID: session.ID,
			SessionToken: sessionToken, ExitCode: exitCode,
		}
	}
	return &historycapture.TerminalAction{Kind: historycapture.TerminalSessionExit, SessionID: session.ID, ExitCode: exitCode}
}

func finalStateForCheckReport(result workerexec.RunResult, verdict coordinator.CheckVerdict, reportErr error, stale bool) flowworker.JobState {
	switch {
	case stale:
		return flowworker.JobCanceled
	case reportErr != nil:
		return flowworker.JobFailed
	case verdict == coordinator.CheckSatisfied || verdict == coordinator.CheckBlocked:
		return flowworker.JobFinished
	default:
		return result.FinalState
	}
}

func releaseConsoleSession(ctx context.Context, cfg config.WorkerConfig, sessionToken string) error {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return errors.New("console session token is required")
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL: cfg.CoordinatorURL,
		Token:     sessionToken,
	})
	if err != nil {
		return fmt.Errorf("create console client: %w", err)
	}
	return client.ReleaseConsole(ctx)
}

func reportPersistentSessionProcessExit(ctx context.Context, client *flowclient.Client, session *coordinator.Session, lease flowworker.Lease, exitCode int) error {
	if client == nil || session == nil {
		return nil
	}
	_, err := client.ReportSessionProcessExit(ctx, flowclient.ReportSessionProcessExitInput{
		SessionID: session.ID,
		LeaseID:   lease.ID,
		ExitCode:  exitCode,
	})
	return err
}

func registerWorkerWithRetry(ctx context.Context, client *flowclient.Client, cfg config.WorkerConfig, heartbeatTTL time.Duration, stderr io.Writer) (flowworker.Worker, error) {
	labels, harnessAvailability := registrationLabelsWithAvailability(cfg.Labels)
	logAgentHarnessAvailability(harnessAvailability)
	harnessModels := registrationHarnessModels(labels)
	for {
		slog.Debug("flow-worker register worker", "worker_id", cfg.WorkerID, "heartbeat_ttl", heartbeatTTL)
		registered, err := client.RegisterWorkerContext(ctx, flowclient.RegisterWorkerInput{
			ID:            cfg.WorkerID,
			Labels:        labels,
			Taints:        cfg.Taints,
			HarnessModels: harnessModels,
			HeartbeatTTL:  heartbeatTTL,
		})
		if err == nil {
			return registered, nil
		}
		if !flowclient.IsRetryableError(err) {
			return flowworker.Worker{}, err
		}
		slog.Debug("flow-worker register worker transient error", "worker_id", cfg.WorkerID, "error", err)
		fmt.Fprintf(stderr, "register worker transient error: %v; retrying in %s\n", err, transientWorkerRetryDelay)
		if err := waitWorkerContext(ctx, transientWorkerRetryDelay); err != nil {
			return flowworker.Worker{}, err
		}
	}
}

func registrationLabels(configured map[string]string) map[string]string {
	labels, _ := registrationLabelsWithAvailability(configured)
	return labels
}

func registrationLabelsWithAvailability(configured map[string]string) (map[string]string, []flowharness.Availability) {
	labels := make(map[string]string, len(configured)+4)
	for key, value := range configured {
		normalizedKey := strings.TrimSpace(strings.ToLower(key))
		if normalizedKey == "agent" || strings.HasPrefix(normalizedKey, "agent.harness.") {
			continue
		}
		labels[key] = value
	}
	availability := make([]flowharness.Availability, 0, len(flowharness.AgentNames()))
	for _, name := range flowharness.AgentNames() {
		definition, ok := flowharness.Lookup(name)
		if !ok {
			continue
		}
		status := definition.Availability()
		availability = append(availability, status)
		if status.Available {
			labels[flowharness.AgentHarnessLabel(definition.Name)] = "true"
		}
	}
	if _, configured := labels["docker"]; !configured && dockerAvailable() {
		labels["docker"] = "true"
	}
	return labels, availability
}

func logAgentHarnessAvailability(availability []flowharness.Availability) {
	available := make([]string, 0, len(availability))
	unavailable := make([]string, 0, len(availability))
	for _, status := range availability {
		attrs := []any{
			"harness", status.Name,
			"executable", status.Executable,
			"path", status.Path,
			"reason", status.Reason,
		}
		if status.Error != "" {
			attrs = append(attrs, "error", status.Error)
		}
		if status.Available {
			available = append(available, status.Name)
			slog.Info("flow-worker agent harness detected", attrs...)
			continue
		}
		unavailable = append(unavailable, status.Name)
		slog.Info("flow-worker agent harness not detected", attrs...)
	}
	slog.Info("flow-worker agent harness detection summary",
		"available", strings.Join(available, ","),
		"unavailable", strings.Join(unavailable, ","),
	)
}

func dockerAvailable() bool {
	executable, err := exec.LookPath("docker")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, executable, "info").Run() == nil
}

func registrationHarnessModels(labels map[string]string) []flowharness.Model {
	var models []flowharness.Model
	for _, definition := range flowharness.AgentDefinitionsFromLabels(labels) {
		defModels, err := definition.AvailableModels()
		if err != nil {
			slog.Debug("flow-worker harness model catalog unavailable", "harness", definition.Name, "error", err)
			continue
		}
		models = append(models, defModels...)
	}
	return models
}

func retryTransientOperation(action string, stdout io.Writer, fn func() error) error {
	return retryTransientOperationContext(context.Background(), action, stdout, fn)
}

func retryTransientOperationContext(ctx context.Context, action string, stdout io.Writer, fn func() error) error {
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := fn()
		if err == nil || !flowclient.IsRetryableError(err) {
			return err
		}
		slog.Debug("flow-worker transient operation error", "action", action, "error", err)
		fmt.Fprintf(stdout, "%s transient error: %v; retrying in %s\n", action, err, transientWorkerRetryDelay)
		timer := time.NewTimer(transientWorkerRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func reportCheckIfNeeded(ctx context.Context, client *flowclient.Client, job flowworker.Job, lease flowworker.Lease, result workerexec.RunResult, stdout io.Writer) (coordinator.CheckVerdict, error) {
	checkName := strings.TrimSpace(result.Payload.CheckName)
	if checkName == "" || job.TaskID == nil {
		slog.Debug("flow-worker skipping check report", "job_id", job.ID, "reason", "missing check name or task")
		return "", nil
	}
	kind, ok := checkKindForJob(job)
	if !ok {
		slog.Debug("flow-worker skipping check report", "job_id", job.ID, "role", job.Role, "bucket", job.CapacityBucket, "reason", "unsupported check kind")
		return "", nil
	}
	slog.Debug("flow-worker report check", "job_id", job.ID, "lease_id", lease.ID, "check_name", checkName, "kind", kind, "exit_code", result.ExitCode)
	sourceJobID := job.ID
	leaseID := lease.ID
	exitCode := result.ExitCode
	details := fmt.Sprintf("exit code %d", result.ExitCode)
	if result.Err != nil {
		details = result.Err.Error()
	}

	// Reviewer and verifier outcomes must come from a valid structured verdict.
	// Their process exit is diagnostic only: a missing/invalid verdict is an
	// execution error, never an implicit request for changes. CI keeps its
	// command-exit result semantics.
	var verdict coordinator.CheckVerdict
	var verdictReport workerexec.VerdictReport
	var haveVerdict bool
	var verdictFileErr error
	if result.VerdictReport != nil {
		verdict = coordinator.CheckVerdict(result.VerdictReport.Verdict)
		verdictReport = *result.VerdictReport
		haveVerdict = true
		if strings.TrimSpace(verdictReport.Reason) != "" {
			details = verdictReport.Reason
		}
	} else if result.Payload.CompletionProtocol == checkverdict.CompletionProtocol {
		verdictFileErr = errors.New("flow complete did not seal the structured verdict")
	} else if result.VerdictFilePath != "" {
		v, ok, err := workerexec.ReadVerdictFile(result.VerdictFilePath)
		switch {
		case err != nil:
			verdictFileErr = err
			fmt.Fprintf(stdout, "check: verdict file unusable: %v\n", err)
		case ok:
			verdict = coordinator.CheckVerdict(v.Verdict)
			verdictReport = v
			haveVerdict = true
			if strings.TrimSpace(v.Reason) != "" {
				details = v.Reason
			}
		}
	}
	structuredVerdictRequired := kind == coordinator.CheckKindReviewer || kind == coordinator.CheckKindVerifier
	if structuredVerdictRequired && !haveVerdict {
		verdict = coordinator.CheckErrored
		if verdictFileErr != nil {
			details = "structured verdict invalid: " + verdictFileErr.Error()
		} else {
			details = "structured verdict missing"
		}
	}
	if !haveVerdict && (result.Err != nil || result.ExitCode == 126 || result.ExitCode == 127 || result.ExitCode >= 128) {
		verdict = coordinator.CheckErrored
		switch {
		case result.Err != nil:
			details = "check execution failed: " + result.Err.Error()
		case result.ExitCode == 126:
			details = "check execution failed: command is not executable"
		case result.ExitCode == 127:
			details = "check execution failed: command was not found"
		default:
			details = fmt.Sprintf("check execution failed: process terminated by signal (exit code %d)", result.ExitCode)
		}
	}

	// The check report hits the task route, which the coordinator can only
	// resolve implicitly when a single project is registered. Scope the client
	// to the job's project (carried on the payload) so reports — and the review
	// threads/decisions applied below — land in the right project once multiple
	// projects share one worker.
	if projectID := strings.TrimSpace(result.Payload.ProjectID); projectID != "" {
		client = client.WithProject(projectID)
	}

	decisionRequestRejected := false
	if haveVerdict && kind == coordinator.CheckKindReviewer && reviewAggregationJob(job) && verdictReport.DecisionRequest != nil {
		var opened coordinator.RequestReviewScopeDecisionResult
		err := retryTransientOperationContext(ctx, "request review scope decision", stdout, func() error {
			var requestErr error
			opened, requestErr = client.RequestReviewScopeDecision(ctx, *job.TaskID, checkName, lease.ID, job.ID, verdictReport)
			return requestErr
		})
		if err == nil {
			fmt.Fprintf(stdout, "check: review scope decision requested wait=%s key=%s\n", opened.Wait.ID, verdictReport.DecisionRequest.Key)
			return coordinator.CheckPending, nil
		}
		decisionRequestRejected = true
		verdict = coordinator.CheckErrored
		details = "review scope decision request rejected: " + err.Error()
	}

	blocking, err := checkJobBlockingValue(job)
	if err != nil {
		return coordinator.CheckErrored, fmt.Errorf("check job payload: %w", err)
	}
	reviewDiscovery := reviewDiscoveryJob(job)
	if haveVerdict && kind == coordinator.CheckKindReviewer && blocking && !reviewDiscovery {
		blockingFindings := blockingReviewFindings(verdictReport)
		if verdict == coordinator.CheckBlocked && len(verdictReport.Comments) > 0 && blockingFindings == 0 {
			verdict = coordinator.CheckSatisfied
			details = "No task-caused high-severity blocker. " + strings.TrimSpace(details)
		}
	}

	// Apply the structured reviewer concerns / verifier decisions the job carried
	// in its verdict file BEFORE recording the check verdict. Advisory jobs retain
	// those findings in check details instead: they must not create or reopen a
	// thread that independently blocks approval. Aggregation jobs may still apply
	// non-blocking follow-up tasks. The worker lease is live here, so every
	// coordinator mutation can be bound to the exact source job.
	var followUpResults map[int]reviewFollowUpResult
	var followUpFailures []string
	if haveVerdict && !decisionRequestRejected {
		var err error
		followUpResults, followUpFailures, err = applyVerdictActions(
			ctx,
			client,
			kind,
			blocking && !reviewDiscovery,
			reviewAggregationJob(job),
			*job.TaskID,
			lease,
			result,
			verdictReport,
			stdout,
		)
		if err != nil {
			verdict = coordinator.CheckErrored
			details = "structured verdict actions failed: " + err.Error()
		}
	}
	if haveVerdict && verdict != coordinator.CheckErrored {
		if !blocking || reviewDiscovery {
			details = advisoryVerdictDetails(details, verdictReport, followUpResults)
		} else if kind == coordinator.CheckKindReviewer {
			details = classifiedReviewDetails(details, verdictReport, followUpResults)
		}
	}
	details = appendFollowUpActionFailures(details, followUpFailures)

	check, err := client.ReportCheckContext(ctx, *job.TaskID, checkName, flowclient.ReportCheckInput{
		Kind:        kind,
		Required:    &blocking,
		Verdict:     verdict,
		ExitCode:    &exitCode,
		Details:     details,
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	})
	if err != nil {
		return "", fmt.Errorf("report check: %w", err)
	}
	fmt.Fprintf(stdout, "check: %s verdict=%s review_state=%s\n", check.Check.Name, check.Check.Verdict, check.ReviewState)
	for _, failure := range check.FollowUpFailures {
		fmt.Fprintf(stdout, "check follow-up: %s failed: %s\n", failure.EventKind, failure.Details)
	}
	if check.Check.Verdict == coordinator.CheckErrored {
		return check.Check.Verdict, fmt.Errorf("check execution failed: %s", details)
	}
	return check.Check.Verdict, nil
}

// applyVerdictActions deterministically files a reviewer job's blocking
// concerns, applies final-aggregation follow-up tasks, and carries out verifier
// decisions. Load-bearing failures — filing blocking concerns as threads and
// applying verifier decisions — make the check errored so declared blocking
// work is never silently lost. Follow-up task actions are non-blocking
// bookkeeping: their failures are returned separately so the caller can record
// them in the check details without erroring the check or halting the
// workflow. Every operation is idempotent or state-guarded, making retry safe.
type reviewFollowUpResult struct {
	TaskID      string
	Disposition string
}

func applyVerdictActions(
	ctx context.Context,
	client *flowclient.Client,
	kind coordinator.CheckKind,
	blocking bool,
	reviewAggregation bool,
	sourceTaskID string,
	lease flowworker.Lease,
	result workerexec.RunResult,
	report workerexec.VerdictReport,
	stdout io.Writer,
) (map[int]reviewFollowUpResult, []string, error) {
	leaseID := lease.ID
	var actionErr error
	var followUpFailures []string
	followUpResults := map[int]reviewFollowUpResult{}
	switch kind {
	case coordinator.CheckKindReviewer:
		for index, comment := range report.Comments {
			if comment.TaskAction == nil {
				continue
			}
			if !reviewAggregation {
				failure := fmt.Sprintf("%s:%d: task_action ignored: only a review aggregation job may apply task_action", comment.File, comment.Line)
				fmt.Fprintf(stdout, "check: apply review follow-up %s\n", failure)
				followUpFailures = append(followUpFailures, failure)
				continue
			}
			introduced := comment.IntroducedByChange != nil && *comment.IntroducedByChange
			var applied flowclient.ApplyReviewFollowUpResult
			err := retryTransientOperationContext(ctx, "apply review follow-up", stdout, func() error {
				var err error
				applied, err = client.ApplyReviewFollowUp(sourceTaskID, flowclient.ApplyReviewFollowUpInput{
					LeaseID: leaseID,
					Finding: flowclient.ReviewFollowUpFinding{
						SHA:                comment.SHA,
						File:               comment.File,
						Line:               comment.Line,
						Body:               comment.Body,
						Severity:           comment.Severity,
						IntroducedByChange: introduced,
						Requirement:        comment.Requirement,
						DuplicateOf:        comment.DuplicateOf,
					},
					TaskAction: flowclient.ReviewFollowUpTaskAction{
						Action: comment.TaskAction.Action,
						Title:  comment.TaskAction.Title,
						Body:   comment.TaskAction.Body,
						TaskID: comment.TaskAction.TaskID,
					},
				})
				return err
			})
			if err != nil {
				fmt.Fprintf(stdout, "check: apply review follow-up %s:%d failed: %v\n", comment.File, comment.Line, err)
				followUpFailures = append(followUpFailures, fmt.Sprintf("%s:%d (%s): %v", comment.File, comment.Line, comment.TaskAction.Action, err))
				continue
			}
			followUpResults[index] = reviewFollowUpResult{
				TaskID:      applied.Task.ID,
				Disposition: applied.Disposition,
			}
			fmt.Fprintf(stdout, "check: review follow-up %s task %s\n", applied.Disposition, applied.Task.ID)
		}

		if blocking {
			blockingFindings := blockingReviewFindings(report)
			changeID := strings.TrimSpace(result.Payload.ChangeID)
			if blockingFindings > 0 && changeID == "" {
				fmt.Fprintf(stdout, "check: cannot file %d blocking verdict comment(s): missing change id\n", blockingFindings)
				actionErr = errors.Join(actionErr, errors.New("cannot file verdict comments: missing change id"))
			}
			for _, comment := range report.Comments {
				if !comment.BlocksApproval() || changeID == "" {
					continue
				}
				err := retryTransientOperationContext(ctx, "file verdict comment", stdout, func() error {
					_, err := client.CreateThread(changeID, flowclient.CreateThreadInput{
						AnchorCommitSHA: comment.SHA,
						FilePath:        comment.File,
						Line:            comment.Line,
						Body:            comment.Body,
						LeaseID:         leaseID,
					})
					return err
				})
				if err != nil {
					fmt.Fprintf(stdout, "check: file verdict comment %s:%s:%d failed: %v\n", comment.SHA, comment.File, comment.Line, err)
					actionErr = errors.Join(actionErr, err)
					continue
				}
				fmt.Fprintf(stdout, "check: filed verdict comment %s:%d\n", comment.File, comment.Line)
			}
		}
	case coordinator.CheckKindVerifier:
		if !blocking {
			break
		}
		for _, decision := range report.Threads {
			err := retryTransientOperationContext(ctx, "apply verdict thread decision", stdout, func() error {
				if decision.Decision == "reopen" {
					_, err := client.ReopenThread(decision.ID, decision.Body, leaseID)
					return err
				}
				_, err := client.CertifyThread(decision.ID, decision.Body, leaseID)
				return err
			})
			if err != nil {
				// certify/reopen are state-guarded: re-applying a decision that
				// already took effect (or whose thread is gone) returns
				// thread_not_found. Treat that as a benign no-op, mirroring
				// claimResolvedTrailers in cmd/flow.
				if strings.Contains(err.Error(), "thread_not_found") {
					fmt.Fprintf(stdout, "check: verdict thread %s %s already applied\n", decision.ID, decision.Decision)
					continue
				}
				fmt.Fprintf(stdout, "check: verdict thread %s %s failed: %v\n", decision.ID, decision.Decision, err)
				actionErr = errors.Join(actionErr, err)
				continue
			}
			fmt.Fprintf(stdout, "check: applied verdict thread %s %s\n", decision.ID, decision.Decision)
		}
	}
	if !blocking {
		findings := len(report.Comments) + len(report.Threads)
		if findings > 0 {
			fmt.Fprintf(stdout, "check: retained %d advisory finding(s) in check details; no review threads changed\n", findings)
		}
	}
	return followUpResults, followUpFailures, actionErr
}

// appendFollowUpActionFailures records non-blocking follow-up task action
// failures in the check details. The coordinator rejected (or never received)
// these actions, so the findings stay visible to humans instead of silently
// vanishing — but unlike blocking work they must not error the check.
func appendFollowUpActionFailures(details string, failures []string) string {
	if len(failures) == 0 {
		return details
	}
	lines := []string{strings.TrimSpace(details), "", "Follow-up task actions failed (non-blocking):"}
	for _, failure := range failures {
		lines = append(lines, "- "+failure)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// checkJobBlockingValue reads the stamped blocking value. Every review/check
// job carries an explicit Boolean; an absent or wrong-typed value marks the
// job corrupt instead of defaulting to blocking.
func checkJobBlockingValue(job flowworker.Job) (bool, error) {
	blocking, ok := job.Payload["blocking"].(bool)
	if !ok {
		return false, fmt.Errorf("job %s payload is missing boolean blocking", job.ID)
	}
	return blocking, nil
}

func reviewDiscoveryJob(job flowworker.Job) bool {
	discovery, _ := job.Payload["review_discovery"].(bool)
	return discovery
}

func reviewAggregationJob(job flowworker.Job) bool {
	aggregation, _ := job.Payload["review_aggregation"].(bool)
	return aggregation
}

func advisoryVerdictDetails(details string, report workerexec.VerdictReport, taskResults map[int]reviewFollowUpResult) string {
	var lines []string
	if trimmed := strings.TrimSpace(details); trimmed != "" {
		lines = append(lines, "Advisory (non-blocking): "+trimmed)
	} else {
		lines = append(lines, "Advisory (non-blocking) finding")
	}
	for index, comment := range report.Comments {
		lines = append(lines, formatReviewFinding(comment, taskResults[index]))
	}
	for _, decision := range report.Threads {
		finding := fmt.Sprintf("- thread %s: recommends %s", decision.ID, decision.Decision)
		if body := strings.TrimSpace(decision.Body); body != "" {
			finding += ": " + body
		}
		lines = append(lines, finding)
	}
	return strings.Join(lines, "\n")
}

func blockingReviewFindings(report workerexec.VerdictReport) int {
	count := 0
	for _, comment := range report.Comments {
		if comment.BlocksApproval() {
			count++
		}
	}
	return count
}

func classifiedReviewDetails(details string, report workerexec.VerdictReport, taskResults map[int]reviewFollowUpResult) string {
	lines := []string{strings.TrimSpace(details)}
	var followUps []string
	for index, comment := range report.Comments {
		if !comment.BlocksApproval() {
			followUps = append(followUps, formatReviewFinding(comment, taskResults[index]))
		}
	}
	if len(followUps) == 0 {
		return strings.TrimSpace(details)
	}
	lines = append(lines, "", "Non-blocking follow-ups:")
	lines = append(lines, followUps...)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func formatReviewFinding(comment workerexec.ReviewCommentReport, taskResult reviewFollowUpResult) string {
	classification := comment.Severity
	if comment.IntroducedByChange != nil && !*comment.IntroducedByChange {
		classification += ", pre-existing"
	}
	if comment.DuplicateOf != "" {
		classification += ", duplicate of " + comment.DuplicateOf
	}
	finding := fmt.Sprintf(
		"- %s:%d [%s; invariant: %s] %s",
		comment.File,
		comment.Line,
		classification,
		strings.TrimSpace(comment.Requirement),
		strings.TrimSpace(comment.Body),
	)
	if comment.FollowUp != "" {
		finding += " Follow-up: " + strings.TrimSpace(comment.FollowUp)
	}
	if taskResult.TaskID != "" {
		finding += fmt.Sprintf(
			" Follow-up task: [%s](/ui/tasks/%s) (%s).",
			taskResult.TaskID,
			taskResult.TaskID,
			taskResult.Disposition,
		)
	}
	return finding
}

// transcriptTailBytes is the maximum number of bytes the worker uploads from
// the end of the transcript log. The coordinator caps storage at the same size.
const transcriptTailBytes = 10 << 20 // 10 MiB

// uploadTranscript best-effort persists the job's tmux transcript to the
// coordinator. Persistent sessions PUT to their session route with the session
// token; check jobs PUT to their job route with the worker token and the
// still-live lease. Any failure is logged to job stdout and never fails the
// job.
func uploadTranscript(ctx context.Context, client *flowclient.Client, cfg config.WorkerConfig, job flowworker.Job, lease flowworker.Lease, session *coordinator.Session, sessionToken string, result workerexec.RunResult, stdout io.Writer) {
	path := strings.TrimSpace(result.TranscriptPath)
	if path == "" {
		return
	}
	tail, err := readFileTail(path, transcriptTailBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Debug("flow-worker no transcript to upload", "job_id", job.ID, "path", path)
			return
		}
		fmt.Fprintf(stdout, "transcript: read failed: %v\n", err)
		return
	}
	if len(tail) == 0 {
		slog.Debug("flow-worker empty transcript; nothing to upload", "job_id", job.ID, "path", path)
		return
	}

	if session != nil && strings.TrimSpace(sessionToken) != "" {
		sessionClient, err := flowclient.New(config.ClientConfig{
			ServerURL: cfg.CoordinatorURL,
			Token:     strings.TrimSpace(sessionToken),
		})
		if err != nil {
			fmt.Fprintf(stdout, "transcript: client init failed: %v\n", err)
			return
		}
		if err := sessionClient.UploadSessionTranscript(ctx, session.ID, bytes.NewReader(tail)); err != nil {
			if isInvalidBearerToken(err) && persistentSessionFinalized(ctx, client, job, lease, session) {
				fmt.Fprintf(stdout, "transcript: session upload skipped: session already finalized\n")
				return
			}
			fmt.Fprintf(stdout, "transcript: session upload failed: %v\n", err)
			return
		}
		fmt.Fprintf(stdout, "transcript: uploaded session=%s bytes=%d\n", session.ID, len(tail))
		return
	}

	// Check jobs upload against their job route with the worker token. The
	// coordinator resolves the owning project from the job id (bundleForJob),
	// so the job route needs no project prefix.
	if err := client.UploadJobTranscript(ctx, job.ID, lease.ID, bytes.NewReader(tail)); err != nil {
		fmt.Fprintf(stdout, "transcript: job upload failed: %v\n", err)
		return
	}
	fmt.Fprintf(stdout, "transcript: uploaded job=%s bytes=%d\n", job.ID, len(tail))
}

func isInvalidBearerToken(err error) bool {
	var statusErr *flowclient.HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == http.StatusUnauthorized &&
		statusErr.Code == "unauthorized" &&
		strings.Contains(statusErr.Message, "invalid bearer token")
}

func acknowledgeStoppedPersistentSession(client *flowclient.Client, job flowworker.Job, lease flowworker.Lease, session *coordinator.Session, exitCode int, stdout io.Writer) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), historyShutdownBudget)
	defer cancel()
	if !persistentSessionFinalized(ctx, client, job, lease, session) {
		return false, nil
	}
	if job.Role != flowworker.RoleConsole {
		return true, nil
	}
	if err := retryTransientOperationContext(ctx, "acknowledge stopped console process exit", stdout, func() error {
		return reportPersistentSessionProcessExit(ctx, client, session, lease, exitCode)
	}); err != nil {
		return true, err
	}
	return true, nil
}

func persistentSessionFinalized(ctx context.Context, client *flowclient.Client, job flowworker.Job, lease flowworker.Lease, session *coordinator.Session) bool {
	if client == nil || session == nil || strings.TrimSpace(lease.ID) == "" {
		return false
	}
	status, err := client.WorkerJobStatus(ctx, flowclient.WorkerJobStatusInput{LeaseID: lease.ID})
	if err != nil {
		return false
	}
	if status.Job.ID != job.ID || status.Lease.ID != lease.ID {
		return false
	}
	if status.Session != nil && status.Session.ID == session.ID && terminalSessionState(status.Session.RuntimeState) {
		return true
	}
	return status.Lease.ReleasedAt != nil || terminalJobState(status.Job.State)
}

func terminalSessionState(state coordinator.SessionRuntimeState) bool {
	switch state {
	case coordinator.SessionFinished, coordinator.SessionCrashed, coordinator.SessionAbandoned:
		return true
	default:
		return false
	}
}

func terminalJobState(state flowworker.JobState) bool {
	switch state {
	case flowworker.JobFinished, flowworker.JobFailed, flowworker.JobCrashed, flowworker.JobCanceled:
		return true
	default:
		return false
	}
}

// readFileTail returns at most the last max bytes of the file at path.
func readFileTail(path string, max int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size > int64(max) {
		if _, err := file.Seek(size-int64(max), io.SeekStart); err != nil {
			return nil, err
		}
	}

	return io.ReadAll(file)
}

func checkKindForJob(job flowworker.Job) (coordinator.CheckKind, bool) {
	switch job.Role {
	case flowworker.RoleCI:
		if job.CapacityBucket != flowworker.BucketEphemeral {
			return "", false
		}
		return coordinator.CheckKindCI, true
	case flowworker.RoleReviewer:
		return coordinator.CheckKindReviewer, true
	case flowworker.RoleVerifier:
		return coordinator.CheckKindVerifier, true
	default:
		return "", false
	}
}

type leaseHeartbeat struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	cancel   context.CancelCauseFunc

	mu  sync.Mutex
	err error
}

func (h *leaseHeartbeat) fail(err error) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	if h.err != nil {
		h.mu.Unlock()
		return
	}
	h.err = err
	h.mu.Unlock()
	if h.cancel != nil {
		h.cancel(err)
	}
}

func (h *leaseHeartbeat) Err() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *leaseHeartbeat) Stop() error {
	if h == nil {
		return nil
	}
	h.stopOnce.Do(func() {
		close(h.stop)
	})
	<-h.done
	return h.Err()
}

func startLeaseHeartbeat(
	client *flowclient.Client,
	cfg config.WorkerConfig,
	lease flowworker.Lease,
	timings workerTimings,
	stdout io.Writer,
	cancel context.CancelCauseFunc,
) *leaseHeartbeat {
	interval := heartbeatInterval(timings.HeartbeatTTL, timings.LeaseDuration)
	slog.Debug("flow-worker start lease heartbeat", "worker_id", cfg.WorkerID, "lease_id", lease.ID, "interval", interval)
	heartbeat := &leaseHeartbeat{
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	leaseID := lease.ID
	leaseExpiresAt := lease.ExpiresAt
	go func() {
		defer close(heartbeat.done)
		delay := interval
		disconnected := false
		for {
			timer := time.NewTimer(delay)
			select {
			case <-heartbeat.stop:
				timer.Stop()
				return
			case <-timer.C:
				if _, err := client.HeartbeatWorker(flowclient.HeartbeatWorkerInput{
					WorkerID:     cfg.WorkerID,
					HeartbeatTTL: timings.HeartbeatTTL,
				}); err != nil {
					if !flowclient.IsRetryableError(err) {
						slog.Debug("flow-worker lease heartbeat fatal worker heartbeat error", "worker_id", cfg.WorkerID, "lease_id", leaseID, "error", err)
						heartbeat.fail(fmt.Errorf("heartbeat worker: %w", err))
						return
					}
					slog.Debug("flow-worker lease heartbeat transient worker heartbeat error", "worker_id", cfg.WorkerID, "lease_id", leaseID, "error", err)
				}
				renewed, err := client.RenewLease(flowclient.RenewLeaseInput{
					LeaseID:       leaseID,
					LeaseDuration: timings.LeaseDuration,
				})
				if err != nil {
					if !flowclient.IsRetryableError(err) {
						slog.Debug("flow-worker lease renewal fatal error", "lease_id", leaseID, "error", err)
						heartbeat.fail(fmt.Errorf("renew lease: %w", err))
						return
					}
					slog.Debug("flow-worker lease renewal transient error", "lease_id", leaseID, "expires_at", leaseExpiresAt, "error", err)
					if !disconnected {
						fmt.Fprintf(stdout, "renew transient error: %v; coordinator unavailable, retrying every %s\n", err, transientWorkerRetryDelay)
					}
					disconnected = true
					delay = transientWorkerRetryDelay
					continue
				}
				leaseExpiresAt = renewed.ExpiresAt
				slog.Debug("flow-worker lease renewed", "lease_id", leaseID, "expires_at", leaseExpiresAt)
				if disconnected {
					fmt.Fprintf(stdout, "coordinator reconnected: lease=%s\n", leaseID)
				}
				fmt.Fprintf(stdout, "renewed: %s\n", leaseID)
				disconnected = false
				delay = interval
			}
		}
	}()

	return heartbeat
}

func historyCaptureWasInterrupted(parentErr, jobErr, heartbeatErr error) bool {
	return parentErr != nil || jobErr != nil || heartbeatErr != nil
}

func discardHistoryTerminalAfterInterruption(manager *historyCaptureManager, captureID string, parentCtx, jobCtx context.Context, heartbeatErr error) (bool, error) {
	if !historyCaptureWasInterrupted(parentCtx.Err(), jobCtx.Err(), heartbeatErr) {
		return false, nil
	}
	discardCtx, discardCancel := context.WithTimeout(context.Background(), historyShutdownBudget)
	err := manager.discardTerminal(discardCtx, captureID)
	discardCancel()
	return true, err
}

func isLeaseNotRenewable(err error) bool {
	var statusErr *flowclient.HTTPStatusError
	return errors.As(err, &statusErr) &&
		statusErr.Code == "renew_lease_failed" &&
		strings.Contains(statusErr.Message, "lease is not renewable")
}

func isStaleSourceJobHeadReport(err error) bool {
	var statusErr *flowclient.HTTPStatusError
	return errors.As(err, &statusErr) &&
		statusErr.StatusCode == http.StatusForbidden &&
		statusErr.Code == "forbidden" &&
		strings.Contains(statusErr.Message, "source job head does not match current change head")
}

func heartbeatInterval(heartbeatTTL time.Duration, leaseDuration time.Duration) time.Duration {
	interval := 30 * time.Second
	for _, candidate := range []time.Duration{heartbeatTTL / 2, leaseDuration / 2} {
		if candidate > 0 && candidate < interval {
			interval = candidate
		}
	}
	if interval < time.Second {
		return time.Second
	}

	return interval
}

func newWorkerClient(cfg config.WorkerConfig) (*flowclient.Client, error) {
	return flowclient.New(config.ClientConfig{
		ServerURL: cfg.CoordinatorURL,
		Token:     cfg.Token,
	})
}

func loadWorkerConfig(configPath string) (config.WorkerConfig, string, error) {
	resolvedPath, err := config.ResolveWorkerConfigPath(configPath)
	if err != nil {
		return config.WorkerConfig{}, "", err
	}
	cfg, err := config.LoadWorker(resolvedPath)
	if err != nil {
		return config.WorkerConfig{}, "", err
	}
	cfg, err = config.ApplyWorkerEnvOverrides(cfg, os.Getenv)
	if err != nil {
		return config.WorkerConfig{}, "", err
	}

	return cfg, resolvedPath, nil
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow-worker [--log-level LEVEL] COMMAND
  flow-worker run --one-shot [-c PATH]
  flow-worker config [-c PATH]
  flow-worker --version

Global flags:
  --log-level LEVEL   structured log level: debug, info, warn, error, or off (overrides LOG_LEVEL)

Workers are one-shot capacity slots: they register runtime capabilities, may
wait unbound, run exactly one eventual assignment, and exit.
`)
}
