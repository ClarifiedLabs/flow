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
	"strings"
	"sync"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	"github.com/ClarifiedLabs/flow/internal/version"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
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
	fmt.Fprintf(stdout, "protocol: %s\n", cfg.ProtocolVersion)
	fmt.Fprintf(stdout, "labels: %d\n", len(cfg.Labels))
	fmt.Fprintf(stdout, "capacity_persistent_agent: %d\n", cfg.Capacity.PersistentAgent)
	fmt.Fprintf(stdout, "capacity_ephemeral: %d\n", cfg.Capacity.Ephemeral)
	return 0
}

func runWorker(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	var registerOnly bool
	var once bool
	var claimWait time.Duration
	var leaseDuration time.Duration
	var heartbeatTTL time.Duration
	flags.StringVar(&configPath, "c", "", "worker config path")
	flags.StringVar(&configPath, "config", "", "worker config path")
	flags.BoolVar(&registerOnly, "register-only", false, "register and heartbeat without claiming jobs")
	flags.BoolVar(&once, "once", false, "run at most one claim attempt")
	flags.DurationVar(&claimWait, "claim-wait", 30*time.Second, "claim long-poll duration")
	flags.DurationVar(&leaseDuration, "lease", 60*time.Second, "lease duration")
	flags.DurationVar(&heartbeatTTL, "heartbeat-ttl", 60*time.Second, "worker heartbeat TTL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "flow-worker does not accept positional arguments")
		return 2
	}

	cfg, _, err := loadWorkerConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load worker config: %v\n", err)
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
	if err := workerexec.RequireTerminalAttach(cfg); err != nil {
		fmt.Fprintf(stderr, "terminal attach preflight: %v\n", err)
		return 1
	}
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
	registered, err := registerWorkerWithRetry(client, cfg, heartbeatTTL, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "register worker: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "registered: %s\n", registered.ID)
	if registerOnly {
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

	reapOrphanedTmuxSessions(client, cfg, stderr)

	timings := workerTimings{
		ClaimWait:     claimWait,
		LeaseDuration: leaseDuration,
		HeartbeatTTL:  heartbeatTTL,
	}
	runOutput := &lockedWriter{writer: stdout}
	slots := workerSlotCount(cfg)
	var runErr error
	if slots == 1 {
		runErr = runWorkerLoop(client, cfg, timings, once, runOutput)
	} else {
		runErr = runWorkerSlots(cfg, timings, slots, once, runOutput)
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "%v\n", runErr)
		return 1
	}
	return 0
}

type workerTimings struct {
	ClaimWait     time.Duration
	LeaseDuration time.Duration
	HeartbeatTTL  time.Duration
}

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

func runWorkerSlots(cfg config.WorkerConfig, timings workerTimings, slots int, once bool, stdout io.Writer) error {
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
				errs <- runWorkerLoop(client, cfg, timings, true, stdout)
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
			if err := runWorkerLoop(client, cfg, timings, false, stdout); err != nil {
				errs <- err
			}
		}()
	}
	return <-errs
}

func runWorkerLoop(client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, once bool, stdout io.Writer) error {
	for {
		slog.Debug("flow-worker claim loop iteration", "worker_id", cfg.WorkerID, "once", once)
		claimed, err := runWorkerOnce(client, cfg, timings, stdout)
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

func runWorkerOnce(client *flowclient.Client, cfg config.WorkerConfig, timings workerTimings, stdout io.Writer) (bool, error) {
	slog.Debug("flow-worker heartbeat worker", "worker_id", cfg.WorkerID, "heartbeat_ttl", timings.HeartbeatTTL)
	heartbeat, err := client.HeartbeatWorker(flowclient.HeartbeatWorkerInput{
		WorkerID:     cfg.WorkerID,
		HeartbeatTTL: timings.HeartbeatTTL,
	})
	if err != nil {
		return false, fmt.Errorf("heartbeat worker: %w", err)
	}
	fmt.Fprintf(stdout, "heartbeat: %s\n", heartbeat.ID)

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
	slog.Debug("flow-worker job claimed",
		"worker_id", cfg.WorkerID,
		"job_id", claim.Job.ID,
		"lease_id", claim.Lease.ID,
		"role", claim.Job.Role,
		"bucket", claim.Job.CapacityBucket,
	)
	fmt.Fprintf(stdout, "claimed: %s lease=%s\n", claim.Job.ID, claim.Lease.ID)
	running, err := client.MarkJobRunning(claim.Lease.ID)
	if err != nil {
		return true, jobFailure(fmt.Errorf("mark job running: %w", err))
	}
	slog.Debug("flow-worker job running", "job_id", running.Job.ID, "state", running.Job.State)
	fmt.Fprintf(stdout, "running: %s state=%s\n", running.Job.ID, running.Job.State)

	persistentSession := running.Session != nil
	stopHeartbeat := startLeaseHeartbeat(client, cfg, *claim.Lease, timings, stdout)
	result := workerexec.RunJob(context.Background(), workerexec.RunInput{
		Config:       cfg,
		Job:          running.Job,
		Lease:        *claim.Lease,
		Session:      running.Session,
		SessionToken: running.SessionToken,
	})
	fmt.Fprintf(stdout, "ran: %s session=%s exit=%d\n", claim.Job.ID, result.Session, result.ExitCode)
	slog.Debug("flow-worker job completed", "job_id", claim.Job.ID, "session", result.Session, "exit_code", result.ExitCode, "final_state", result.FinalState, "error", result.Err)
	checkErr := retryTransientOperation("report check", stdout, func() error {
		return reportCheckIfNeeded(client, *claim.Job, *claim.Lease, result, stdout)
	})
	staleCheckResult := isStaleSourceJobHeadReport(checkErr)
	if staleCheckResult {
		fmt.Fprintf(stdout, "check: %s stale source head; result discarded\n", strings.TrimSpace(result.Payload.CheckName))
	}

	// Persist the tmux transcript before the lease is released (worker jobs
	// authenticate the upload with the still-live lease). Upload failures are
	// logged and never fail the job.
	uploadTranscript(client, cfg, *claim.Job, *claim.Lease, running.Session, running.SessionToken, result, stdout)

	finalState := result.FinalState
	if staleCheckResult {
		finalState = flowworker.JobCanceled
	} else if checkErr != nil {
		finalState = flowworker.JobFailed
	}

	if persistentSession {
		// A console session always releases its lease through /v1/console
		// regardless of how the worker step ended. Routing a failed console run
		// through the generic process-exit path below would both leak the lease
		// (that path rejects the console role) and mask the real error with the
		// process-exit failure, so handle every console exit here and surface the
		// underlying error after releasing.
		if running.Job.Role == flowworker.RoleConsole {
			releaseErr := retryTransientOperation("release console", stdout, func() error {
				return releaseConsoleSession(cfg, running.SessionToken)
			})
			heartbeatErr := stopHeartbeat()
			if releaseErr != nil {
				if isInvalidBearerToken(releaseErr) && persistentSessionFinalized(context.Background(), client, running.Job, *claim.Lease, running.Session) {
					fmt.Fprintf(stdout, "console release skipped: session already finalized\n")
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
			processExitErr = reportPersistentSessionProcessExit(context.Background(), client, running.Session, *claim.Lease, result.ExitCode)
		}
		heartbeatErr := stopHeartbeat()
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
	heartbeatErr := stopHeartbeat()
	if releaseErr != nil {
		if heartbeatErr != nil {
			return true, jobFailure(fmt.Errorf("lease heartbeat: %v; release lease: %w", heartbeatErr, releaseErr))
		}
		return true, jobFailure(fmt.Errorf("release lease: %w", releaseErr))
	}
	slog.Debug("flow-worker lease released", "job_id", released.ID, "state", released.State)
	fmt.Fprintf(stdout, "released: %s state=%s\n", released.ID, released.State)
	if checkErr != nil && !staleCheckResult {
		return true, jobFailure(checkErr)
	}
	if result.Err != nil && !staleCheckResult {
		return true, jobFailure(fmt.Errorf("run job: %w", result.Err))
	}
	return true, nil
}

func releaseConsoleSession(cfg config.WorkerConfig, sessionToken string) error {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return errors.New("console session token is required")
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       cfg.CoordinatorURL,
		Token:           sessionToken,
		ProtocolVersion: cfg.ProtocolVersion,
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
	labels := registrationLabels(cfg.Labels)
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
		ServerURL:       cfg.CoordinatorURL,
		Token:           joinToken,
		ProtocolVersion: cfg.ProtocolVersion,
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
	labels := make(map[string]string, len(configured)+4)
	for key, value := range configured {
		if strings.TrimSpace(strings.ToLower(key)) == "agent" {
			continue
		}
		labels[key] = value
	}
	for key, value := range flowharness.AvailableAgentLabels() {
		labels[key] = value
	}
	if _, configured := labels["docker"]; !configured && dockerAvailable() {
		labels["docker"] = "true"
	}
	return labels
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

func reportCheckIfNeeded(client *flowclient.Client, job flowworker.Job, lease flowworker.Lease, result workerexec.RunResult, stdout io.Writer) error {
	checkName := strings.TrimSpace(result.Payload.CheckName)
	if checkName == "" || job.IssueID == nil {
		slog.Debug("flow-worker skipping check report", "job_id", job.ID, "reason", "missing check name or issue")
		return nil
	}
	kind, ok := checkKindForJob(job)
	if !ok {
		slog.Debug("flow-worker skipping check report", "job_id", job.ID, "role", job.Role, "bucket", job.CapacityBucket, "reason", "unsupported check kind")
		return nil
	}
	slog.Debug("flow-worker report check", "job_id", job.ID, "lease_id", lease.ID, "check_name", checkName, "kind", kind, "exit_code", result.ExitCode)
	sourceJobID := job.ID
	leaseID := lease.ID
	exitCode := result.ExitCode
	details := fmt.Sprintf("exit code %d", result.ExitCode)
	if result.Err != nil {
		details = result.Err.Error()
	}

	// Prefer a structured verdict the job wrote to FLOW_VERDICT_FILE over the
	// exit-code mapping. A missing file is the normal exit-code path; a parse
	// error is logged to job stdout (never silently swallowed) and we fall back
	// to the exit code. The exit code still rides along for the audit trail.
	var verdict coordinator.CheckVerdict
	var verdictReport workerexec.VerdictReport
	var haveVerdict bool
	if result.VerdictFilePath != "" {
		v, ok, err := workerexec.ReadVerdictFile(result.VerdictFilePath)
		switch {
		case err != nil:
			fmt.Fprintf(stdout, "check: verdict file unusable, falling back to exit code: %v\n", err)
		case ok:
			verdict = coordinator.CheckVerdict(v.Verdict)
			verdictReport = v
			haveVerdict = true
			if strings.TrimSpace(v.Reason) != "" {
				details = v.Reason
			}
		}
	}

	// The check report hits the issue route, which the coordinator can only
	// resolve implicitly when a single project is registered. Scope the client
	// to the job's project (carried on the payload) so reports — and the review
	// threads/decisions applied below — land in the right project once multiple
	// projects share one worker.
	if projectID := strings.TrimSpace(result.Payload.ProjectID); projectID != "" {
		client = client.WithProject(projectID)
	}

	// Apply the structured reviewer concerns / verifier decisions the job carried
	// in its verdict file BEFORE recording the check verdict. Filing review
	// threads first lets the coordinator's cross-check override a satisfied
	// reviewer verdict to blocked when open threads remain. The worker lease is
	// still live here, so the writes pass the change-access check.
	if haveVerdict {
		applyVerdictActions(client, kind, lease, result, verdictReport, stdout)
	}

	check, err := client.ReportCheck(*job.IssueID, checkName, flowclient.ReportCheckInput{
		Kind:        kind,
		Verdict:     verdict,
		ExitCode:    &exitCode,
		Details:     details,
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	})
	if err != nil {
		return fmt.Errorf("report check: %w", err)
	}
	fmt.Fprintf(stdout, "check: %s verdict=%s review_state=%s\n", check.Check.Name, check.Check.Verdict, check.ReviewState)
	for _, failure := range check.FollowUpFailures {
		fmt.Fprintf(stdout, "check follow-up: %s failed: %s\n", failure.EventKind, failure.Details)
	}
	return nil
}

// applyVerdictActions deterministically files a reviewer job's blocking concerns
// as review threads and applies a verifier job's certify/reopen decisions,
// carrying out mechanically what the agent declared in its verdict file. Failures
// are logged to job stdout and never fail the job: the check verdict still needs
// to be recorded, and CreateThread is idempotent (a retry is a no-op) while
// certify/reopen are state-guarded.
func applyVerdictActions(client *flowclient.Client, kind coordinator.CheckKind, lease flowworker.Lease, result workerexec.RunResult, report workerexec.VerdictReport, stdout io.Writer) {
	leaseID := lease.ID
	switch kind {
	case coordinator.CheckKindReviewer:
		if len(report.Comments) == 0 {
			return
		}
		changeID := strings.TrimSpace(result.Payload.ChangeID)
		if changeID == "" {
			fmt.Fprintf(stdout, "check: cannot file %d verdict comment(s): missing change id\n", len(report.Comments))
			return
		}
		for _, comment := range report.Comments {
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
				continue
			}
			fmt.Fprintf(stdout, "check: filed verdict comment %s:%d\n", comment.File, comment.Line)
		}
	case coordinator.CheckKindVerifier:
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
				continue
			}
			fmt.Fprintf(stdout, "check: applied verdict thread %s %s\n", decision.ID, decision.Decision)
		}
	}
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
			ServerURL:       cfg.CoordinatorURL,
			Token:           strings.TrimSpace(sessionToken),
			ProtocolVersion: cfg.ProtocolVersion,
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

func startLeaseHeartbeat(client *flowclient.Client, cfg config.WorkerConfig, lease flowworker.Lease, timings workerTimings, stdout io.Writer) func() error {
	interval := heartbeatInterval(timings.HeartbeatTTL, timings.LeaseDuration)
	slog.Debug("flow-worker start lease heartbeat", "worker_id", cfg.WorkerID, "lease_id", lease.ID, "interval", interval)
	stop := make(chan struct{})
	done := make(chan struct{})
	leaseID := lease.ID
	leaseExpiresAt := lease.ExpiresAt
	var fatalErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := client.HeartbeatWorker(flowclient.HeartbeatWorkerInput{
					WorkerID:     cfg.WorkerID,
					HeartbeatTTL: timings.HeartbeatTTL,
				}); err != nil {
					if !flowclient.IsRetryableError(err) {
						slog.Debug("flow-worker lease heartbeat fatal worker heartbeat error", "worker_id", cfg.WorkerID, "lease_id", leaseID, "error", err)
						fatalErr = fmt.Errorf("heartbeat worker: %w", err)
						return
					}
					slog.Debug("flow-worker lease heartbeat transient worker heartbeat error", "worker_id", cfg.WorkerID, "lease_id", leaseID, "error", err)
					fmt.Fprintf(stdout, "heartbeat transient error: %v; retrying\n", err)
				}
				renewed, err := client.RenewLease(flowclient.RenewLeaseInput{
					LeaseID:       leaseID,
					LeaseDuration: timings.LeaseDuration,
				})
				if err != nil {
					if !flowclient.IsRetryableError(err) {
						slog.Debug("flow-worker lease renewal fatal error", "lease_id", leaseID, "error", err)
						fatalErr = fmt.Errorf("renew lease: %w", err)
						return
					}
					if !time.Now().UTC().Before(leaseExpiresAt) {
						slog.Debug("flow-worker lease renewal exceeded deadline", "lease_id", leaseID, "expires_at", leaseExpiresAt, "error", err)
						fatalErr = fmt.Errorf("renew lease exceeded current lease deadline %s: %w", leaseExpiresAt.Format(time.RFC3339), err)
						return
					}
					slog.Debug("flow-worker lease renewal transient error", "lease_id", leaseID, "expires_at", leaseExpiresAt, "error", err)
					fmt.Fprintf(stdout, "renew transient error: %v; retrying before lease expires at %s\n", err, leaseExpiresAt.Format(time.RFC3339))
					continue
				}
				leaseExpiresAt = renewed.ExpiresAt
				slog.Debug("flow-worker lease renewed", "lease_id", leaseID, "expires_at", leaseExpiresAt)
				fmt.Fprintf(stdout, "renewed: %s\n", leaseID)
			}
		}
	}()

	return func() error {
		close(stop)
		<-done
		return fatalErr
	}
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

// reapOrphanedTmuxSessions kills tmux sessions leaked by a previously
// SIGKILLed worker whose deferred cleanup never ran. It runs once at startup
// before the claim/work loop begins; reaping must never fail boot, so any error
// is logged and execution continues.
func reapOrphanedTmuxSessions(client *flowclient.Client, cfg config.WorkerConfig, stderr io.Writer) {
	slog.Debug("flow-worker reap orphaned tmux sessions")
	jobs, err := client.ListWorkerReapJobs()
	if err != nil {
		slog.Debug("flow-worker reap orphaned tmux sessions failed to list jobs", "error", err)
		fmt.Fprintf(stderr, "reap orphaned tmux sessions: list jobs: %v\n", err)
		return
	}
	killed, err := workerexec.ReapOrphanedSessions(context.Background(), jobs, workerexec.WithWorkerConfig(cfg))
	slog.Debug("flow-worker reaped orphaned tmux sessions", "killed", killed, "error", err)
	fmt.Fprintf(stderr, "reaped orphaned tmux sessions: %d\n", killed)
	if err != nil {
		fmt.Fprintf(stderr, "reap orphaned tmux sessions: %v\n", err)
	}
}

func newWorkerClient(cfg config.WorkerConfig) (*flowclient.Client, error) {
	return flowclient.New(config.ClientConfig{
		ServerURL:       cfg.CoordinatorURL,
		Token:           cfg.Token,
		ProtocolVersion: cfg.ProtocolVersion,
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
