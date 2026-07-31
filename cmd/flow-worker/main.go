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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/ClarifiedLabs/flow/internal/worker/terminalbridge"
)

const transientWorkerRetryDelay = time.Second

// jobError marks a failure scoped to a single claimed job. The coordinator
// recovers such jobs through lease expiry and job state, so a service-mode
// worker logs them and keeps serving instead of exiting and abandoning its
// other in-flight jobs.
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
		return runWorker(nil, stdout, stderr)
	}

	switch args[0] {
	case "--version", "version":
		fmt.Fprintf(stdout, "flow-worker %s\n", version.Current())
		return 0
	case "-c", "--config", "run":
		if args[0] == "run" {
			return runWorker(args[1:], stdout, stderr)
		}
		return runWorker(args, stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		if strings.HasPrefix(args[0], "-") {
			return runWorker(args, stdout, stderr)
		}
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
	fmt.Fprintf(stdout, "capacity_persistent_agent: %d\n", cfg.Capacity.PersistentAgent)
	fmt.Fprintf(stdout, "capacity_ephemeral: %d\n", cfg.Capacity.Ephemeral)
	cleanup, _ := cfg.Cleanup.Resolve()
	fmt.Fprintf(stdout, "cleanup_interval: %s\n", cleanup.Interval)
	fmt.Fprintf(stdout, "cleanup_orphan_grace: %s\n", cleanup.OrphanGrace)
	fmt.Fprintf(stdout, "cleanup_min_free_bytes: %d\n", cleanup.MinFreeBytes)
	fmt.Fprintf(stdout, "cleanup_resume_free_bytes: %d\n", cleanup.ResumeFreeBytes)
	fmt.Fprintf(stdout, "cleanup_min_free_percent: %g\n", cleanup.MinFreePercent)
	fmt.Fprintf(stdout, "cleanup_resume_free_percent: %g\n", cleanup.ResumeFreePercent)
	return 0
}

func runWorker(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	var registerOnly bool
	var once bool
	var ephemeral bool
	var noMetrics bool
	var metricsListen string
	var claimWait time.Duration
	var leaseDuration time.Duration
	var heartbeatTTL time.Duration
	var gitCommitName string
	var gitCommitEmail string
	flags.StringVar(&configPath, "c", "", "worker config path")
	flags.StringVar(&configPath, "config", "", "worker config path")
	flags.BoolVar(&registerOnly, "register-only", false, "register and heartbeat without claiming jobs")
	flags.BoolVar(&once, "once", false, "run at most one claim attempt")
	flags.BoolVar(&ephemeral, "ephemeral", false, "keep long-polling until one job is claimed, run it, then exit")
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
	if once && ephemeral {
		fmt.Fprintln(stderr, "--once and --ephemeral cannot be used together")
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
	if strings.TrimSpace(cfg.WorkerID) == "" {
		fmt.Fprintln(stderr, "worker config worker_id is required")
		return 1
	}
	if strings.TrimSpace(cfg.Token) == "" {
		token, err := joinWorker(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "join worker: %v\n", err)
			return 1
		}
		cfg.Token = token
	}
	os.Unsetenv("FLOW_WORKER_JOIN_TOKEN")
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
	jobMetrics = &workerJobMetrics{
		claimed:   telemetryRegistry.Counter("flow_jobs_claimed_total", "Jobs claimed by this worker."),
		completed: telemetryRegistry.Counter("flow_jobs_completed_total", "Jobs released by this worker by final state."),
	}
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
	telemetry, err := metrics.StartEndpoint(context.Background(), slog.Default(), metrics.Mux(telemetryRegistry, ready), telemetrySettings)
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
	controlCtx, controlCancel := context.WithCancel(context.Background())
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
		"capacity_persistent_agent", cfg.Capacity.PersistentAgent,
		"capacity_ephemeral", cfg.Capacity.Ephemeral,
	)

	client, err := newWorkerClient(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	maintenance := newWorkerMaintenance(cfg, cleanupPolicy, client, maintenanceMetrics, &diskReady)
	registered, err := registerWorkerWithRetry(client, cfg, heartbeatTTL, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "register worker: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "registered: %s\n", registered.ID)
	if registerOnly {
		registeredReady.Store(true)
		heartbeat, err := client.HeartbeatWorker(flowclient.HeartbeatWorkerInput{
			WorkerID:     cfg.WorkerID,
			HeartbeatTTL: heartbeatTTL,
		})
		if err != nil {
			fmt.Fprintf(stderr, "heartbeat worker: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "heartbeat: %s\n", heartbeat.ID)
		fmt.Fprintln(stdout, "claim: disabled")
		return 0
	}

	maintenance.Maintain(true, stderr)
	registeredReady.Store(true)
	maintenanceCtx, maintenanceCancel := context.WithCancel(context.Background())
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
	}
	runOutput := &lockedWriter{writer: stdout}
	var runErr error
	if ephemeral {
		runErr = runWorkerEphemeral(client, cfg, timings, maintenance, runOutput)
	} else if slots := workerSlotCount(cfg); slots == 1 {
		runErr = runWorkerLoop(client, cfg, timings, maintenance, once, runOutput)
	} else {
		runErr = runWorkerSlots(cfg, timings, maintenance, slots, once, runOutput)
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "%v\n", runErr)
		return 1
	}
	return 0
}

// workerJobMetrics are the per-worker job counters. The package-level handle
// is nil-safe so claim paths can count without threading state through every
// helper, and tests that never start the telemetry endpoint stay unaffected.
type workerJobMetrics struct {
	claimed   *metrics.Counter
	completed *metrics.Counter
}

var jobMetrics *workerJobMetrics

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

// runWorkerEphemeral keeps long-polling until a job is claimed, runs exactly
// that one job, then returns. A job-scoped failure has already been reported
// to the coordinator (release, check verdict, or discarded-on-lease-loss), so
// it is not a process failure: the worker exits 0 either way and the kubelet
// restarts the container for the next assignment.
func runWorkerEphemeral(client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, stdout io.Writer) error {
	for {
		slog.Debug("flow-worker ephemeral claim attempt", "worker_id", cfg.WorkerID)
		claimed, err := runWorkerOnce(client, cfg, timings, maintenance, stdout)
		if err != nil {
			if flowclient.IsRetryableError(err) {
				slog.Debug("flow-worker transient worker error", "worker_id", cfg.WorkerID, "error", err)
				fmt.Fprintf(stdout, "worker transient error: %v; retrying in %s\n", err, transientWorkerRetryDelay)
				time.Sleep(transientWorkerRetryDelay)
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
			time.Sleep(time.Second)
		}
	}
}

type workerTimings struct {
	ClaimWait     time.Duration
	LeaseDuration time.Duration
	HeartbeatTTL  time.Duration
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

func workerSlotCount(cfg config.WorkerConfig) int {
	slots := cfg.Capacity.PersistentAgent + cfg.Capacity.Ephemeral
	if slots < 1 {
		return 1
	}

	return slots
}

func runWorkerSlots(cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, slots int, once bool, stdout io.Writer) error {
	if once {
		errs := make(chan error, slots)
		var wg sync.WaitGroup
		for range slots {
			wg.Add(1)
			go func() {
				defer wg.Done()
				client, err := newWorkerClient(cfg)
				if err != nil {
					errs <- fmt.Errorf("create client: %w", err)
					return
				}
				errs <- runWorkerLoop(client, cfg, timings, maintenance, true, stdout)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}

	errs := make(chan error, slots)
	for range slots {
		go func() {
			client, err := newWorkerClient(cfg)
			if err != nil {
				errs <- fmt.Errorf("create client: %w", err)
				return
			}
			if err := runWorkerLoop(client, cfg, timings, maintenance, false, stdout); err != nil {
				errs <- err
			}
		}()
	}
	return <-errs
}

func runWorkerLoop(client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, once bool, stdout io.Writer) error {
	for {
		slog.Debug("flow-worker claim loop iteration", "worker_id", cfg.WorkerID, "once", once)
		claimed, err := runWorkerOnce(client, cfg, timings, maintenance, stdout)
		if err != nil {
			if flowclient.IsRetryableError(err) {
				slog.Debug("flow-worker transient worker error", "worker_id", cfg.WorkerID, "error", err)
				fmt.Fprintf(stdout, "worker transient error: %v; retrying in %s\n", err, transientWorkerRetryDelay)
				time.Sleep(transientWorkerRetryDelay)
				continue
			}
			var jobErr *jobError
			if !once && errors.As(err, &jobErr) {
				slog.Debug("flow-worker job error", "worker_id", cfg.WorkerID, "error", err)
				fmt.Fprintf(stdout, "job error: %v; continuing\n", err)
				continue
			}
			return err
		}
		if once {
			return nil
		}
		if !claimed && timings.ClaimWait <= 0 {
			time.Sleep(time.Second)
		}
	}
}

func runWorkerOnce(client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, maintenance *workerMaintenance, stdout io.Writer) (bool, error) {
	slog.Debug("flow-worker heartbeat worker", "worker_id", cfg.WorkerID, "heartbeat_ttl", timings.HeartbeatTTL)
	heartbeat, err := client.HeartbeatWorker(flowclient.HeartbeatWorkerInput{
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

	slog.Debug("flow-worker claim job", "worker_id", cfg.WorkerID, "claim_wait", timings.ClaimWait, "lease_duration", timings.LeaseDuration)
	claim, err := client.ClaimJob(flowclient.ClaimJobInput{
		WorkerID:      cfg.WorkerID,
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent, flowworker.BucketEphemeral},
		LeaseDuration: timings.LeaseDuration,
		Wait:          timings.ClaimWait,
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
	jobMetrics.jobClaimed()
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
	jobCtx, cancelJob := context.WithCancelCause(context.Background())
	defer cancelJob(nil)
	leaseHeartbeat := startLeaseHeartbeat(client, cfg, *claim.Lease, timings, stdout, cancelJob)
	result := workerexec.RunJob(jobCtx, workerexec.RunInput{
		Config:       cfg,
		Job:          running.Job,
		Lease:        *claim.Lease,
		ProjectID:    claim.ProjectID,
		Session:      running.Session,
		SessionToken: running.SessionToken,
	})
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
		if isLeaseNotRenewable(heartbeatErr) &&
			persistentSession &&
			persistentSessionFinalized(context.Background(), client, running.Job, *claim.Lease, running.Session) {
			if running.Job.Role == flowworker.RoleConsole {
				if err := retryTransientOperation("acknowledge stopped console process exit", stdout, func() error {
					return reportPersistentSessionProcessExit(context.Background(), client, running.Session, *claim.Lease, result.ExitCode)
				}); err != nil {
					return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %w", err))
				}
				fmt.Fprintf(stdout, "console process exit acknowledged: %s\n", running.Session.ID)
			}
			cleanupFinalized = true
			slog.Debug("flow-worker persistent session finalized while lease heartbeat was active", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID)
			fmt.Fprintf(stdout, "persistent session finalized: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
			return true, nil
		}
		slog.Debug("flow-worker discarded job result after authoritative lease loss", "job_id", claim.Job.ID, "lease_id", claim.Lease.ID, "error", heartbeatErr)
		fmt.Fprintf(stdout, "lease lost: %s; discarded local result\n", claim.Lease.ID)
		return true, jobFailure(fmt.Errorf("lease heartbeat: %w", heartbeatErr))
	}
	var reportedCheckVerdict coordinator.CheckVerdict
	checkErr := retryTransientOperation("report check", stdout, func() error {
		var err error
		reportedCheckVerdict, err = reportCheckIfNeeded(client, *claim.Job, *claim.Lease, result, stdout)
		return err
	})
	staleCheckResult := isStaleSourceJobHeadReport(checkErr)
	if staleCheckResult {
		fmt.Fprintf(stdout, "check: %s stale source head; result discarded\n", strings.TrimSpace(result.Payload.CheckName))
	}

	// Persist the tmux transcript before the lease is released (worker jobs
	// authenticate the upload with the still-live lease). Upload failures are
	// logged and never fail the job.
	uploadTranscript(client, cfg, *claim.Job, *claim.Lease, running.Session, running.SessionToken, result, stdout)

	finalState := finalStateForCheckReport(result, reportedCheckVerdict, checkErr, staleCheckResult)

	if persistentSession {
		// A console session normally releases its lease through /v2/console.
		// When an owner has already stopped the console, that route is unavailable
		// because the session token is revoked; in that case the worker must send
		// the process-exit acknowledgement that clears the coordinator's fence.
		if running.Job.Role == flowworker.RoleConsole {
			releaseErr := retryTransientOperation("release console", stdout, func() error {
				return releaseConsoleSession(cfg, running.SessionToken)
			})
			heartbeatErr := leaseHeartbeat.Stop()
			if releaseErr != nil {
				if isInvalidBearerToken(releaseErr) && persistentSessionFinalized(context.Background(), client, running.Job, *claim.Lease, running.Session) {
					exitErr := retryTransientOperation("acknowledge stopped console process exit", stdout, func() error {
						return reportPersistentSessionProcessExit(context.Background(), client, running.Session, *claim.Lease, result.ExitCode)
					})
					if exitErr != nil {
						if heartbeatErr != nil {
							return true, jobFailure(fmt.Errorf("lease heartbeat: %v; acknowledge stopped console process exit: %w", heartbeatErr, exitErr))
						}
						return true, jobFailure(fmt.Errorf("acknowledge stopped console process exit: %w", exitErr))
					}
					fmt.Fprintf(stdout, "console process exit acknowledged: %s\n", running.Session.ID)
				} else {
					if heartbeatErr != nil {
						return true, jobFailure(fmt.Errorf("lease heartbeat: %v; release console: %w", heartbeatErr, releaseErr))
					}
					return true, jobFailure(fmt.Errorf("release console: %w", releaseErr))
				}
			} else {
				slog.Debug("flow-worker console session released", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "error", result.Err)
				fmt.Fprintf(stdout, "released: %s state=%s\n", running.Job.ID, flowworker.JobFinished)
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

		alreadyFinalized := persistentSessionFinalized(context.Background(), client, running.Job, *claim.Lease, running.Session)
		var processExitErr error
		if !alreadyFinalized {
			processExitErr = retryTransientOperation("report persistent session process exit", stdout, func() error {
				return reportPersistentSessionProcessExit(context.Background(), client, running.Session, *claim.Lease, result.ExitCode)
			})
		}
		heartbeatErr := leaseHeartbeat.Stop()
		if alreadyFinalized {
			slog.Debug("flow-worker persistent session finalized by coordinator", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "role", running.Job.Role)
			fmt.Fprintf(stdout, "persistent session finalized: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
		} else if processExitErr == nil {
			slog.Debug("flow-worker persistent session process exited", "job_id", running.Job.ID, "session_id", running.Session.ID, "lease_id", claim.Lease.ID, "role", running.Job.Role)
			fmt.Fprintf(stdout, "persistent session exited: %s lease=%s\n", running.Session.ID, claim.Lease.ID)
		}
		if processExitErr != nil {
			return true, jobFailure(fmt.Errorf("report persistent session process exit: %w", processExitErr))
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
	releaseErr := retryTransientOperation("release lease", stdout, func() error {
		var err error
		released, err = client.ReleaseLease(flowclient.ReleaseLeaseInput{
			LeaseID:    claim.Lease.ID,
			FinalState: finalState,
		})
		return err
	})
	heartbeatErr := leaseHeartbeat.Stop()
	if releaseErr != nil {
		if heartbeatErr != nil {
			return true, jobFailure(fmt.Errorf("lease heartbeat: %v; release lease: %w", heartbeatErr, releaseErr))
		}
		return true, jobFailure(fmt.Errorf("release lease: %w", releaseErr))
	}
	slog.Debug("flow-worker lease released", "job_id", released.ID, "state", released.State)
	fmt.Fprintf(stdout, "released: %s state=%s\n", released.ID, released.State)
	cleanupFinalized = true
	jobMetrics.jobCompleted(string(released.State))
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

func releaseConsoleSession(cfg config.WorkerConfig, sessionToken string) error {
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
	return client.ReleaseConsole(context.Background())
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

func registerWorkerWithRetry(client *flowclient.Client, cfg config.WorkerConfig, heartbeatTTL time.Duration, stderr io.Writer) (flowworker.Worker, error) {
	labels, harnessAvailability := registrationLabelsWithAvailability(cfg.Labels)
	logAgentHarnessAvailability(harnessAvailability)
	harnessModels := registrationHarnessModels(labels)
	for {
		slog.Debug("flow-worker register worker", "worker_id", cfg.WorkerID, "heartbeat_ttl", heartbeatTTL)
		registered, err := client.RegisterWorker(flowclient.RegisterWorkerInput{
			ID:                      cfg.WorkerID,
			Labels:                  labels,
			Taints:                  cfg.Taints,
			HarnessModels:           harnessModels,
			CapacityPersistentAgent: cfg.Capacity.PersistentAgent,
			CapacityEphemeral:       cfg.Capacity.Ephemeral,
			HeartbeatTTL:            heartbeatTTL,
		})
		if err == nil {
			return registered, nil
		}
		if !flowclient.IsRetryableError(err) {
			return flowworker.Worker{}, err
		}
		slog.Debug("flow-worker register worker transient error", "worker_id", cfg.WorkerID, "error", err)
		fmt.Fprintf(stderr, "register worker transient error: %v; retrying in %s\n", err, transientWorkerRetryDelay)
		time.Sleep(transientWorkerRetryDelay)
	}
}

func joinWorker(cfg config.WorkerConfig) (string, error) {
	joinToken := strings.TrimSpace(os.Getenv("FLOW_WORKER_JOIN_TOKEN"))
	if joinToken == "" {
		return "", errors.New("worker config token is required or FLOW_WORKER_JOIN_TOKEN must be set")
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL: cfg.CoordinatorURL,
		Token:     joinToken,
	})
	if err != nil {
		return "", err
	}
	joined, err := client.JoinWorker(flowclient.JoinWorkerInput{WorkerID: cfg.WorkerID})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(joined.WorkerID) != strings.TrimSpace(cfg.WorkerID) {
		return "", fmt.Errorf("joined worker_id %q, want %q", joined.WorkerID, cfg.WorkerID)
	}
	if strings.TrimSpace(joined.Token) == "" {
		return "", errors.New("join response did not include a worker token")
	}
	return strings.TrimSpace(joined.Token), nil
}

func registrationLabels(configured map[string]string) map[string]string {
	labels, _ := registrationLabelsWithAvailability(configured)
	return labels
}

func registrationLabelsWithAvailability(configured map[string]string) (map[string]string, []flowharness.Availability) {
	labels := make(map[string]string, len(configured)+4)
	for key, value := range configured {
		if strings.TrimSpace(strings.ToLower(key)) == "agent" {
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
	for {
		err := fn()
		if err == nil || !flowclient.IsRetryableError(err) {
			return err
		}
		slog.Debug("flow-worker transient operation error", "action", action, "error", err)
		fmt.Fprintf(stdout, "%s transient error: %v; retrying in %s\n", action, err, transientWorkerRetryDelay)
		time.Sleep(transientWorkerRetryDelay)
	}
}

func reportCheckIfNeeded(client *flowclient.Client, job flowworker.Job, lease flowworker.Lease, result workerexec.RunResult, stdout io.Writer) (coordinator.CheckVerdict, error) {
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

	blocking := checkJobBlocksApproval(job)
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
	if haveVerdict {
		var err error
		followUpResults, followUpFailures, err = applyVerdictActions(
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

	check, err := client.ReportCheck(*job.TaskID, checkName, flowclient.ReportCheckInput{
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
			err := retryTransientOperation("apply review follow-up", stdout, func() error {
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
				err := retryTransientOperation("file verdict comment", stdout, func() error {
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
			err := retryTransientOperation("apply verdict thread decision", stdout, func() error {
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

func checkJobBlocksApproval(job flowworker.Job) bool {
	blocking, ok := job.Payload["blocking"].(bool)
	return !ok || blocking
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
func uploadTranscript(client *flowclient.Client, cfg config.WorkerConfig, job flowworker.Job, lease flowworker.Lease, session *coordinator.Session, sessionToken string, result workerexec.RunResult, stdout io.Writer) {
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

	ctx := context.Background()
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
  flow-worker [--once]
  flow-worker --register-only
  flow-worker run [--once]
  flow-worker -c PATH [--once]
  flow-worker config [-c PATH]
  flow-worker --version

Global flags:
  --log-level LEVEL   structured log level: debug, info, warn, error, or off (overrides LOG_LEVEL)
`)
}
