package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	flowtoken "github.com/ClarifiedLabs/flow/internal/token"
	"github.com/ClarifiedLabs/flow/internal/version"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// lifecycleTickInterval is how often the durable background ticker drains timers
// and runs crash recovery, independent of inbound API traffic.
const lifecycleTickInterval = 5 * time.Second

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
	slog.Debug("flow-server command start", "command", flowlog.CommandName(args))

	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "--version", "version":
		fmt.Fprintf(stdout, "flow-server %s\n", version.Current())
		return 0
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "git-hook":
		return runGitHook(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runServe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	var addr string
	var dataDir string
	var ownerToken string
	var ownerTokenFile string
	var hookToken string
	var hookTokenFile string
	var workerJoinToken string
	var workerJoinTokenFile string
	var orchestratorToken string
	var orchestratorTokenFile string
	var noMetrics bool
	var metricsListen string
	var clientConfigPathFlag string
	var noWriteClientConfig bool
	var gitCommitName string
	var gitCommitEmail string
	flags.StringVar(&configPath, "config", "", "coordinator config JSON path")
	flags.StringVar(&addr, "addr", "", "listen address")
	flags.StringVar(&dataDir, "data-dir", "", "Flow data directory")
	flags.StringVar(&ownerToken, "owner-token", "", "owner bearer token")
	flags.StringVar(&ownerTokenFile, "owner-token-file", "", "mode-0600 file containing the owner bearer token")
	flags.StringVar(&hookToken, "hook-token", "", "hook bearer token")
	flags.StringVar(&hookTokenFile, "hook-token-file", "", "mode-0600 file containing the hook bearer token")
	flags.StringVar(&workerJoinToken, "worker-join-token", "", "worker join bearer token")
	flags.StringVar(&workerJoinTokenFile, "worker-join-token-file", "", "mode-0600 file containing the worker join bearer token")
	flags.StringVar(&orchestratorToken, "orchestrator-token", "", "orchestrator bearer token (authorizes only GET /v2/queue/stats; not generated when unset)")
	flags.StringVar(&orchestratorTokenFile, "orchestrator-token-file", "", "mode-0600 file containing the orchestrator bearer token")
	flags.BoolVar(&noMetrics, "no-metrics", false, "disable the telemetry endpoint (/readyz, /livez, /metrics)")
	flags.StringVar(&metricsListen, "metrics-listen", "", "telemetry endpoint listen address")
	flags.StringVar(&clientConfigPathFlag, "client-config", "", "client config path to write for local CLI discovery")
	flags.BoolVar(&noWriteClientConfig, "no-write-client-config", false, "do not write a local client config")
	flags.StringVar(&gitCommitName, "git-commit-name", "", "git user.name for coordinator-created commits")
	flags.StringVar(&gitCommitEmail, "git-commit-email", "", "git user.email for coordinator-created commits")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if noWriteClientConfig && strings.TrimSpace(clientConfigPathFlag) != "" {
		fmt.Fprintln(stderr, "--client-config and --no-write-client-config cannot be used together")
		return 2
	}

	cfg, err := config.LoadCoordinator(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load coordinator config: %v\n", err)
		return 1
	}
	cfg, err = config.ApplyCoordinatorEnvOverrides(cfg, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "apply coordinator env overrides: %v\n", err)
		return 1
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if addr != "" {
		cfg.ListenAddr = addr
	}
	if strings.TrimSpace(gitCommitName) != "" {
		cfg.Git.CommitName = strings.TrimSpace(gitCommitName)
	}
	if strings.TrimSpace(gitCommitEmail) != "" {
		cfg.Git.CommitEmail = strings.TrimSpace(gitCommitEmail)
	}
	if err := config.ValidateCoordinatorCommitIdentity(cfg.Git); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	ownerToken = strings.TrimSpace(ownerToken)
	ownerTokenFile = strings.TrimSpace(ownerTokenFile)
	if ownerTokenFile != "" {
		if ownerToken != "" {
			fmt.Fprintln(stderr, "--owner-token and --owner-token-file cannot be used together")
			return 2
		}
		ownerToken, ownerTokenFile, err = readServeTokenFile(ownerTokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "read owner token: %v\n", err)
			return 1
		}
	}
	hookToken = strings.TrimSpace(hookToken)
	hookTokenFile = strings.TrimSpace(hookTokenFile)
	if hookTokenFile != "" {
		if hookToken != "" {
			fmt.Fprintln(stderr, "--hook-token and --hook-token-file cannot be used together")
			return 2
		}
		hookToken, hookTokenFile, err = readServeTokenFile(hookTokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "read hook token: %v\n", err)
			return 1
		}
	}
	if strings.TrimSpace(workerJoinToken) == "" {
		workerJoinToken = strings.TrimSpace(os.Getenv("FLOW_WORKER_JOIN_TOKEN"))
	}
	if strings.TrimSpace(workerJoinTokenFile) != "" {
		if strings.TrimSpace(workerJoinToken) != "" {
			fmt.Fprintln(stderr, "--worker-join-token and --worker-join-token-file cannot be used together")
			return 2
		}
		workerJoinToken, err = config.ReadTokenFile(workerJoinTokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "read worker join token: %v\n", err)
			return 1
		}
	}
	if strings.TrimSpace(orchestratorToken) == "" {
		orchestratorToken = strings.TrimSpace(os.Getenv("FLOW_ORCHESTRATOR_TOKEN"))
	}
	if strings.TrimSpace(orchestratorTokenFile) != "" {
		if strings.TrimSpace(orchestratorToken) != "" {
			fmt.Fprintln(stderr, "--orchestrator-token and --orchestrator-token-file cannot be used together")
			return 2
		}
		orchestratorToken, err = config.ReadTokenFile(orchestratorTokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "read orchestrator token: %v\n", err)
			return 1
		}
	}
	historyPolicy, err := cfg.History.Resolve(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "resolve history configuration: %v\n", err)
		return 1
	}
	historyStore, err := blob.Open(context.Background(), historyBlobFactoryConfig(historyPolicy.Blob))
	if err != nil {
		fmt.Fprintf(stderr, "open history blob store: %v\n", err)
		return 1
	}
	if closer, ok := historyStore.(blob.Closer); ok {
		defer closer.Close()
	}
	slog.Debug("flow-server serve configuration loaded", "addr", cfg.ListenAddr, "database", cfg.GlobalDatabasePath(), "data_dir", cfg.DataDir, "history_backend", historyPolicy.Blob.Backend)

	ownerTokenFileDisplay := "inline"
	if ownerToken == "" {
		ownerToken, err = loadOrCreateOwnerToken(cfg.DataDir)
		if err != nil {
			fmt.Fprintf(stderr, "load owner token: %v\n", err)
			return 1
		}
		ownerTokenFileDisplay = tokenPath(cfg.DataDir, "owner.token")
	} else if ownerTokenFile != "" {
		ownerTokenFileDisplay = ownerTokenFile
	}
	hookTokenFileDisplay := "inline"
	if hookToken == "" {
		hookToken, err = loadOrCreateHookToken(cfg.DataDir)
		if err != nil {
			fmt.Fprintf(stderr, "load hook token: %v\n", err)
			return 1
		}
		hookTokenFileDisplay = tokenPath(cfg.DataDir, "hook.token")
	} else if hookTokenFile != "" {
		hookTokenFileDisplay = hookTokenFile
	}
	clientConfigPath, err := prepareServeClientConfig(cfg, ownerToken, ownerTokenFile, clientConfigPathFlag, noWriteClientConfig)
	if err != nil {
		fmt.Fprintf(stderr, "write client config: %v\n", err)
		return 1
	}

	globalStore, err := flowdb.OpenGlobal(context.Background(), cfg.GlobalDatabasePath())
	if err != nil {
		fmt.Fprintf(stderr, "open global database: %v\n", err)
		return 1
	}
	defer globalStore.Close()

	if _, err := cfg.Deadlines.ResolveDeadlines(); err != nil {
		fmt.Fprintf(stderr, "resolve deadlines: %v\n", err)
		return 1
	}
	limits, err := cfg.Limits.ResolveLimits()
	if err != nil {
		fmt.Fprintf(stderr, "resolve limits: %v\n", err)
		return 1
	}
	workerPolicy, err := cfg.Workers.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "resolve worker policy: %v\n", err)
		return 1
	}
	defaultAgent, err := cfg.ResolvedDefaultAgent()
	if err != nil {
		fmt.Fprintf(stderr, "resolve default agent: %v\n", err)
		return 1
	}

	registry, err := api.NewRegistry(api.RegistryOptions{
		DataDir:          cfg.DataDir,
		Global:           globalStore,
		HistoryBlobStore: historyStore,
		HistoryCaptureServiceOptions: coordinator.HistoryCaptureServiceOptions{
			MaxUploadBytes:                      historyPolicy.Archive.MaxStoredBytes,
			MaxTranscriptSegmentBytes:           historyPolicy.Transcript.SegmentBytes,
			MaxOutstandingUploadsPerCapture:     historyPolicy.Archive.MaxOutstandingUploads,
			MaxOutstandingUploadBytesPerCapture: historyPolicy.Archive.MaxOutstandingBytes,
			MaxArchiveEntries:                   historyPolicy.Archive.MaxEntries,
			MaxArchiveLogicalBytes:              historyPolicy.Archive.MaxLogicalBytes,
		},
		AuthorEntrypoint:           cfg.AuthorEntrypoint,
		AuthorEntrypointConfigured: cfg.AuthorEntrypointConfigured,
		HarnessArgs:                cfg.HarnessArgs,
		DefaultAgent:               defaultAgent,
		ReviewAuthorCycleLimit:     limits.ReviewAuthorCycles,
		ReviewScopeFileLimit:       limits.ReviewScopeFiles,
		ReviewScopeLineLimit:       limits.ReviewScopeLines,
		CommitIdentity: flowgit.CommitIdentity{
			Name:  cfg.Git.CommitName,
			Email: cfg.Git.CommitEmail,
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "create project registry: %v\n", err)
		return 1
	}
	defer registry.Close()

	credentials := registry.Credentials()
	if err := credentials.ReplaceSubjectCredential(context.Background(), coordinator.CredentialInput{
		Token: ownerToken,
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		fmt.Fprintf(stderr, "store owner token: %v\n", err)
		return 1
	}
	if err := credentials.ReplaceSubjectCredential(context.Background(), coordinator.CredentialInput{
		Token: hookToken,
		Scope: coordinator.TokenScopeHook,
	}); err != nil {
		fmt.Fprintf(stderr, "store hook token: %v\n", err)
		return 1
	}
	if orchestratorToken != "" {
		if err := credentials.ReplaceSubjectCredential(context.Background(), coordinator.CredentialInput{
			Token: orchestratorToken,
			Scope: coordinator.TokenScopeOrchestrator,
		}); err != nil {
			fmt.Fprintf(stderr, "store orchestrator token: %v\n", err)
			return 1
		}
	}

	if err := registry.OpenAll(context.Background()); err != nil {
		fmt.Fprintf(stderr, "open projects: %v\n", err)
		return 1
	}

	server, err := api.NewServer(api.ServerOptions{
		Registry:        registry,
		OwnerToken:      ownerToken,
		HookToken:       hookToken,
		WorkerJoinToken: workerJoinToken,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create api server: %v\n", err)
		return 1
	}

	if workerPolicy.ReconnectGrace > 0 {
		reconnectDeadline := time.Now().UTC().Add(workerPolicy.ReconnectGrace)
		protected, err := registry.ExtendActiveLeaseDeadlines(context.Background(), reconnectDeadline)
		if err != nil {
			fmt.Fprintf(stderr, "protect active worker leases: %v\n", err)
			return 1
		}
		slog.Info("protected active worker leases for coordinator restart",
			"leases", protected,
			"reconnect_deadline", reconnectDeadline,
			"reconnect_grace", workerPolicy.ReconnectGrace,
		)
	}

	fmt.Fprintf(stdout, "flow-server listening on %s\n", cfg.ListenAddr)
	fmt.Fprintf(stdout, "database: %s\n", cfg.GlobalDatabasePath())
	fmt.Fprintf(stdout, "projects: %d\n", len(registry.All()))
	fmt.Fprintf(stdout, "owner_token_file: %s\n", ownerTokenFileDisplay)
	fmt.Fprintf(stdout, "hook_token_file: %s\n", hookTokenFileDisplay)
	fmt.Fprintf(stdout, "client_config_file: %s\n", clientConfigPath)
	fmt.Fprintf(stdout, "history_blob_backend: %s\n", historyPolicy.Blob.Backend)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Telemetry: a single unauthenticated port serving /readyz (global DB
	// ping), /livez, and /metrics. It must only be exposed inside the cluster.
	telemetryRegistry := metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "flow_server_build_info",
		Help:    "flow-server build information.",
		Version: version.Current().String(),
	})
	counters := telemetryCounters{
		requests:  telemetryRegistry.Counter("flow_http_requests_total", "HTTP API requests by route and response status."),
		enqueued:  telemetryRegistry.Counter("flow_jobs_enqueued_total", "Jobs successfully enqueued via POST /v2/jobs."),
		completed: telemetryRegistry.Counter("flow_jobs_completed_total", "Jobs released to the finished state."),
	}
	queueDepth := telemetryRegistry.Gauge("flow_queue_depth", "Jobs by state across every project database.")
	historyMetrics := metrics.RegisterHistoryStorage(telemetryRegistry, historyPolicy.Blob.Backend)
	updateQueueDepthGauge(ctx, registry.All(), queueDepth)
	telemetrySettings := metrics.Resolve(cfg.Metrics, config.DefaultTelemetryListen, metrics.Overrides{
		Disable:    noMetrics,
		DisableSet: noMetrics,
		Listen:     metricsListen,
		ListenSet:  strings.TrimSpace(metricsListen) != "",
	})
	telemetry, err := metrics.StartEndpoint(ctx, slog.Default(), metrics.Mux(telemetryRegistry, func() bool {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
		defer pingCancel()
		return globalStore.DB().PingContext(pingCtx) == nil
	}), telemetrySettings)
	if err != nil {
		fmt.Fprintf(stderr, "start telemetry endpoint: %v\n", err)
		return 1
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), metrics.DefaultShutdownTimeout)
		defer shutdownCancel()
		_ = telemetry.Shutdown(shutdownCtx)
	}()

	// Durable background ticker: drains due timers and runs crash recovery on an
	// interval, so recovery fires regardless of inbound API traffic. It is
	// stopped (and awaited) before the deferred close runs. Projects opened
	// while serving join the loop on the next tick.
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		runLifecycleTicker(ctx, registry, stderr, queueDepth)
	}()
	historyReconcileDone := make(chan struct{})
	go func() {
		defer close(historyReconcileDone)
		runHistoryReconciliation(ctx, registry, historyStore, historyPolicy.Reconciliation, historyMetrics)
	}()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: instrumentAPIHandler(server, counters)}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	serveErr := srv.ListenAndServe()
	cancel()
	// ListenAndServe returns ErrServerClosed as soon as Shutdown closes the
	// listener — before in-flight requests drain. Wait for Shutdown to finish
	// (connections drained) and the ticker to stop BEFORE the deferred
	// closes run, so no handler touches a closed database.
	<-shutdownDone
	<-tickerDone
	<-historyReconcileDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "serve: %v\n", serveErr)
		return 1
	}

	return 0
}

func historyBlobFactoryConfig(config config.ResolvedHistoryBlob) blob.FactoryConfig {
	encryption := blob.S3EncryptionAES256
	if config.S3.Encryption == configpkgHistorySSEKMS() {
		encryption = blob.S3EncryptionKMS
	}
	return blob.FactoryConfig{
		Backend: config.Backend, LocalPath: config.LocalPath, MaxRangeBytes: config.MaxRangeBytes,
		S3: blob.FactoryS3Config{
			Region: config.S3.Region, Bucket: config.S3.Bucket, Prefix: config.S3.Prefix,
			EndpointURL: config.S3.Endpoint, PathStyle: config.S3.PathStyle, AllowHTTP: config.S3.AllowHTTP,
			Encryption: encryption, KMSKeyID: config.S3.KMSKeyID, BucketKey: config.S3.BucketKey,
		},
	}
}

// Kept as a tiny helper so the similarly named function parameter above cannot
// shadow the imported config package constant.
func configpkgHistorySSEKMS() string { return config.HistorySSEKMS }

func runHistoryReconciliation(ctx context.Context, registry *api.Registry, store blob.Store, policy config.ResolvedHistoryReconciliation, metricSet metrics.HistoryStorage) {
	reconcile := func() {
		if _, err := reconcileHistoryStorage(ctx, registry, store, policy, metricSet, time.Now().UTC()); err != nil && ctx.Err() == nil {
			metricSet.ObserveFailure()
			slog.Warn("history blob reconciliation failed", "error", err)
		}
	}
	reconcile()
	ticker := time.NewTicker(policy.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func reconcileHistoryStorage(ctx context.Context, registry *api.Registry, store blob.Store, policy config.ResolvedHistoryReconciliation, metricSet metrics.HistoryStorage, now time.Time) (blob.ReconcileResult, error) {
	// Each project receives an independent allowance. Recover durable pending
	// publications before taking the relational protection snapshot used for
	// destructive temporary cleanup. Pending publication does blocking object
	// verification, so keep its per-project pass smaller than the backend scan.
	pendingLimit := policy.BatchSize
	if pendingLimit > 500 {
		pendingLimit = 500
	}
	_, pendingErr := registry.ReconcilePendingHistoryArtifacts(ctx, pendingLimit)
	request, complete, err := registry.HistoryBlobMetadata(ctx, policy.BatchSize)
	if err != nil {
		return blob.ReconcileResult{}, errors.Join(pendingErr, err)
	}
	if !complete {
		// A partial reference snapshot is never safe for deletion. Keep this pass
		// bounded and wait for explicit retention to reduce the protected set.
		result := blob.ReconcileResult{Truncated: true}
		metricSet.ObserveSuccess(0, 0, 0, true, now)
		return result, pendingErr
	}
	request.Before = now.Add(-policy.TemporaryGrace)
	request.Limit = policy.BatchSize
	result, err := store.Reconcile(ctx, request)
	if err != nil {
		return blob.ReconcileResult{}, errors.Join(pendingErr, err)
	}
	agedOrphans := 0
	orphanCutoff := now.Add(-policy.OrphanGrace)
	for _, orphan := range result.Orphans {
		if orphan.Modified.Before(orphanCutoff) {
			agedOrphans++
		}
	}
	metricSet.ObserveSuccess(len(result.RemovedTemporaryIDs), len(result.AbortedMultipartIDs), agedOrphans, result.Truncated, now)
	if agedOrphans > 0 {
		// Keys and storage locations are deliberately omitted. Published objects are
		// report-only and require a future explicitly authorized retention workflow.
		slog.Warn("history reconciliation reported unreferenced published blobs", "count", agedOrphans)
	}
	return result, pendingErr
}

// lifecycleTickConcurrency bounds how many projects tick in parallel per tick so
// a large registry cannot exhaust connections or goroutines.
const lifecycleTickConcurrency = 8

func runLifecycleTicker(ctx context.Context, registry *api.Registry, stderr io.Writer, queueDepth *metrics.Gauge) {
	ticker := time.NewTicker(lifecycleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Debug("lifecycle ticker tick")
			tickProjects(ctx, registry.All(), stderr)
			updateQueueDepthGauge(ctx, registry.All(), queueDepth)
		}
	}
}

// updateQueueDepthGauge refreshes the flow_queue_depth gauge from every open
// project database. A project whose jobs cannot be listed is skipped rather
// than failing the whole refresh: partial depth is better than a stale zero.
func updateQueueDepthGauge(ctx context.Context, bundles []*api.ProjectBundle, gauge *metrics.Gauge) {
	var queued, claimed, running float64
	for _, bundle := range bundles {
		jobs, err := bundle.Queue.ListJobs(ctx)
		if err != nil {
			slog.Debug("queue depth refresh failed", "project", bundle.Project.ID, "error", err)
			continue
		}
		for _, job := range jobs {
			switch job.State {
			case worker.JobQueued:
				queued++
			case worker.JobClaimed:
				claimed++
			case worker.JobRunning:
				running++
			}
		}
	}
	gauge.Set(queued, map[string]string{"state": "queued"})
	gauge.Set(claimed, map[string]string{"state": "claimed"})
	gauge.Set(running, map[string]string{"state": "running"})
}

// telemetryCounters holds the flow-server API counters.
type telemetryCounters struct {
	requests  *metrics.Counter
	enqueued  *metrics.Counter
	completed *metrics.Counter
}

// instrumentAPIHandler wraps the API server to count requests by route and
// status, successful enqueues, and jobs released to the finished state.
func instrumentAPIHandler(handler http.Handler, counters telemetryCounters) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captureRelease := r.Method == http.MethodPost && r.URL.Path == "/v2/workers/release"
		recorder := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
		if captureRelease {
			recorder.body = &bytes.Buffer{}
		}
		handler.ServeHTTP(recorder, r)
		counters.requests.Inc(map[string]string{
			"route":  r.Method + " " + r.URL.Path,
			"status": strconv.Itoa(recorder.status),
		})
		if r.Method == http.MethodPost && r.URL.Path == "/v2/jobs" && recorder.status/100 == 2 {
			counters.enqueued.Inc(nil)
		}
		if captureRelease && recorder.status/100 == 2 {
			var released struct {
				Job struct {
					State string `json:"state"`
				} `json:"job"`
			}
			if json.Unmarshal(recorder.body.Bytes(), &released) == nil && released.Job.State == string(worker.JobFinished) {
				counters.completed.Inc(nil)
			}
		}
	})
}

// statusCapturingWriter records the response status, and optionally buffers
// the body so the instrumenting wrapper can inspect small JSON responses.
type statusCapturingWriter struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (w *statusCapturingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCapturingWriter) Write(p []byte) (int, error) {
	if w.body != nil {
		w.body.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Hijack delegates connection hijacking to the wrapped ResponseWriter so
// WebSocket upgrades (coder/websocket Accept) keep working through the
// instrumentation wrapper.
func (w *statusCapturingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("wrapped ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}

// Unwrap exposes the wrapped ResponseWriter for http.ResponseController and
// middleware-aware libraries (coder/websocket's hijacker lookup) that walk
// the wrapper chain.
func (w *statusCapturingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// tickProjects ticks every project's engine and git-event consumer concurrently,
// bounded at lifecycleTickConcurrency so one wedged project no longer delays the
// others' timer drains and crash recovery. Each project's errors are logged
// independently, exactly as the sequential loop did.
func tickProjects(ctx context.Context, bundles []*api.ProjectBundle, stderr io.Writer) {
	sem := make(chan struct{}, lifecycleTickConcurrency)
	var wg sync.WaitGroup
	for _, bundle := range bundles {
		wg.Add(1)
		go func(bundle *api.ProjectBundle) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tickProject(ctx, bundle, stderr)
		}(bundle)
	}
	wg.Wait()
}

func tickProject(ctx context.Context, bundle *api.ProjectBundle, stderr io.Writer) {
	if err := bundle.WorkflowExecutor.Tick(ctx); err != nil && ctx.Err() == nil {
		slog.Debug("lifecycle ticker failed", "project", bundle.Project.ID, "error", err)
		fmt.Fprintf(stderr, "lifecycle ticker (%s): %v\n", bundle.Project.ID, err)
	}
	if _, err := bundle.Sessions.ReconcileCrashedConsoleSessions(ctx); err != nil && ctx.Err() == nil {
		slog.Debug("console recovery failed", "project", bundle.Project.ID, "error", err)
		fmt.Fprintf(stderr, "console recovery (%s): %v\n", bundle.Project.ID, err)
	}
	if _, err := bundle.GitEventConsumer.ConsumeNew(ctx); err != nil && ctx.Err() == nil {
		slog.Debug("git event consumer failed", "project", bundle.Project.ID, "error", err)
		fmt.Fprintf(stderr, "git event consumer (%s): %v\n", bundle.Project.ID, err)
	}
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("config", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	flags.StringVar(&configPath, "config", "", "coordinator config JSON path")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadCoordinator(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load coordinator config: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "data_dir: %s\n", cfg.DataDir)
	fmt.Fprintf(stdout, "database_path: %s\n", cfg.GlobalDatabasePath())
	fmt.Fprintf(stdout, "listen_addr: %s\n", cfg.ListenAddr)
	history, _ := cfg.History.Resolve(cfg.DataDir)
	fmt.Fprintf(stdout, "history_blob_backend: %s\n", history.Blob.Backend)
	fmt.Fprintf(stdout, "history_reconciliation_interval: %s\n", history.Reconciliation.Interval)
	fmt.Fprintf(stdout, "protocol: %s\n", contract.ProtocolVersion)
	return 0
}

func runGitHook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "missing git hook name")
		return 2
	}

	hookName := args[0]
	flags := flag.NewFlagSet("git-hook "+hookName, flag.ContinueOnError)
	flags.SetOutput(stderr)

	var exchangeRepoPath string
	var baseBranch string
	flags.StringVar(&exchangeRepoPath, "repo", "", "bare exchange repository path")
	flags.StringVar(&baseBranch, "base", "main", "protected base branch")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if exchangeRepoPath == "" {
		fmt.Fprintln(stderr, "--repo is required")
		return 2
	}

	opts := flowgit.HookOptions{
		ExchangeRepoPath: exchangeRepoPath,
		BaseBranch:       baseBranch,
		Stdin:            os.Stdin,
		Stdout:           stdout,
		Stderr:           stderr,
		AllowedRef:       os.Getenv("FLOW_GIT_ALLOWED_REF"),
	}

	var err error
	switch hookName {
	case "pre-receive":
		err = flowgit.HandlePreReceive(context.Background(), opts)
	case "post-receive":
		err = flowgit.HandlePostReceive(context.Background(), opts)
	default:
		fmt.Fprintf(stderr, "unsupported git hook: %s\n", hookName)
		return 2
	}
	if err != nil {
		return 1
	}

	return 0
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow-server [--log-level LEVEL] COMMAND
  flow-server serve [--data-dir PATH] [--addr HOST:PORT] [--worker-join-token TOKEN | --worker-join-token-file PATH] [--owner-token TOKEN | --owner-token-file PATH] [--hook-token TOKEN | --hook-token-file PATH] [--client-config PATH | --no-write-client-config]
  flow-server config [--config PATH]
  flow-server git-hook pre-receive --repo PATH --base BRANCH
  flow-server git-hook post-receive --repo PATH --base BRANCH
  flow-server --version

Global flags:
  --log-level LEVEL   structured log level: debug, info, warn, error, or off (overrides LOG_LEVEL)

Projects are registered with flow init, which calls the running coordinator.
`)
}

func loadOrCreateOwnerToken(dataDir string) (string, error) {
	return loadOrCreateToken(dataDir, "owner.token", "owner")
}

func loadOrCreateHookToken(dataDir string) (string, error) {
	return loadOrCreateToken(dataDir, "hook.token", "hook")
}

func loadOrCreateToken(dataDir string, fileName string, label string) (string, error) {
	path := tokenPath(dataDir, fileName)
	contents, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", statErr
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s token file %s must not be readable by group or others", label, path)
		}
		token := strings.TrimSpace(string(contents))
		if token == "" {
			return "", fmt.Errorf("%s token file %s is empty", label, path)
		}
		return token, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}

	token, err := flowtoken.Generate()
	if err != nil {
		return "", fmt.Errorf("generate %s token: %w", label, err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}

	return token, nil
}

func tokenPath(dataDir string, fileName string) string {
	return filepath.Join(dataDir, fileName)
}

func readServeTokenFile(path string) (string, string, error) {
	token, err := config.ReadTokenFile(path)
	if err != nil {
		return "", "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve token file: %w", err)
	}

	return token, absolute, nil
}

func prepareServeClientConfig(cfg config.CoordinatorConfig, ownerToken string, ownerTokenFile string, configPath string, skipWrite bool) (string, error) {
	if skipWrite {
		return "skipped", nil
	}

	return writeServeClientConfig(cfg, ownerToken, ownerTokenFile, configPath)
}

func writeServeClientConfig(cfg config.CoordinatorConfig, ownerToken string, ownerTokenFile string, configPath string) (string, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		defaultPath, err := config.DefaultClientConfigPath()
		if err != nil {
			return "", err
		}
		configPath = defaultPath
	}
	clientCfg, err := config.LocalClientConfig(cfg.DataDir, config.CoordinatorURLForListenAddr(cfg.ListenAddr), ownerToken)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ownerTokenFile) != "" {
		clientCfg.Token = ""
		clientCfg.TokenFile = strings.TrimSpace(ownerTokenFile)
	}
	if err := config.WriteClientConfig(configPath, clientCfg); err != nil {
		return "", err
	}

	return configPath, nil
}
