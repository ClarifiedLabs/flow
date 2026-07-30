// flow-orchestrator polls the flow coordinator's queue depth and scales the
// worker Kubernetes Deployment, maintaining a standby pool of ephemeral
// workers ready for job assignments.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ClarifiedLabs/flow/internal/config"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/orchestrator"
	"github.com/ClarifiedLabs/flow/internal/version"
)

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
	slog.Debug("flow-orchestrator command start", "command", flowlog.CommandName(args))

	if len(args) > 0 {
		switch args[0] {
		case "--version", "version":
			fmt.Fprintf(stdout, "flow-orchestrator %s\n", version.Current())
			return 0
		case "-h", "--help", "help":
			printUsage(stdout)
			return 0
		}
	}

	return runOrchestrator(args, stdout, stderr)
}

func runOrchestrator(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	var noMetrics bool
	var metricsListen string
	flags.StringVar(&configPath, "c", "", "orchestrator config path")
	flags.StringVar(&configPath, "config", "", "orchestrator config path")
	flags.BoolVar(&noMetrics, "no-metrics", false, "disable the telemetry endpoint (/readyz, /livez, /metrics)")
	flags.StringVar(&metricsListen, "metrics-listen", "", "telemetry endpoint listen address")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "flow-orchestrator does not accept positional arguments")
		return 2
	}

	cfg, err := config.LoadOrchestrator(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load orchestrator config: %v\n", err)
		return 1
	}
	cfg, err = config.ApplyOrchestratorEnvOverrides(cfg, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "apply orchestrator env overrides: %v\n", err)
		return 1
	}
	if strings.TrimSpace(cfg.Token) == "" {
		fmt.Fprintln(stderr, "orchestrator token is required (config token or FLOW_ORCHESTRATOR_TOKEN)")
		return 1
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "resolve orchestrator config: %v\n", err)
		return 1
	}

	telemetryRegistry := metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "flow_orchestrator_build_info",
		Help:    "flow-orchestrator build information.",
		Version: version.Current().String(),
	})
	controller := orchestrator.NewController(orchestrator.Options{
		Stats:           orchestrator.NewCoordinatorPoller(cfg.CoordinatorURL, cfg.Token, nil),
		MinReplicas:     cfg.MinReplicas,
		MaxReplicas:     cfg.MaxReplicas,
		DesiredReplicas: cfg.DesiredReplicas,
		PollInterval:    resolved.PollInterval,
		ScaleDownIdle:   resolved.ScaleDownIdle,
		Metrics: orchestrator.Metrics{
			QueueDepth:            telemetryRegistry.Gauge("flow_queue_depth", "Jobs queued across every project database."),
			WorkerReplicasDesired: telemetryRegistry.Gauge("flow_worker_replicas_desired", "Spec replicas of the worker Deployment."),
			ScaleOperations:       telemetryRegistry.Counter("flow_orchestrator_scale_operations_total", "Deployment scale operations by direction."),
			PollErrors:            telemetryRegistry.Counter("flow_orchestrator_poll_errors_total", "Skipped cycles by failing source (coordinator or k8s)."),
		},
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	telemetrySettings := metrics.Resolve(cfg.Metrics, config.DefaultTelemetryListen, metrics.Overrides{
		Disable:    noMetrics,
		DisableSet: noMetrics,
		Listen:     metricsListen,
		ListenSet:  strings.TrimSpace(metricsListen) != "",
	})
	telemetry, err := metrics.StartEndpoint(ctx, slog.Default(), metrics.Mux(telemetryRegistry, controller.Ready), telemetrySettings)
	if err != nil {
		fmt.Fprintf(stderr, "start telemetry endpoint: %v\n", err)
		return 1
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), metrics.DefaultShutdownTimeout)
		defer shutdownCancel()
		_ = telemetry.Shutdown(shutdownCtx)
	}()

	scaler, err := orchestrator.NewInClusterK8sScaler(cfg.Namespace, cfg.Deployment)
	if err != nil {
		fmt.Fprintf(stderr, "configure kubernetes client: %v\n", err)
		return 1
	}
	controller.SetScaler(scaler)

	fmt.Fprintf(stdout, "orchestrator: scaling deployment %s/%s (min=%d desired=%d max=%d) from %s every %s\n",
		cfg.Namespace, cfg.Deployment, cfg.MinReplicas, cfg.DesiredReplicas, cfg.MaxReplicas, cfg.CoordinatorURL, resolved.PollInterval)
	return runController(ctx, controller)
}

func runController(ctx context.Context, controller *orchestrator.Controller) int {
	if err := controller.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "orchestrator: %v\n", err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `flow-orchestrator scales the worker Kubernetes Deployment from the
coordinator's queue depth, maintaining a standby pool of ephemeral workers.

Usage:
  flow-orchestrator [flags]
  flow-orchestrator version

Flags:
  -c, --config <path>     orchestrator config path (JSON or YAML)
      --no-metrics        disable the telemetry endpoint
      --metrics-listen    telemetry endpoint listen address (default 127.0.0.1:8422)

Environment:
  FLOW_ORCHESTRATOR_TOKEN            orchestrator-scoped bearer token (required)
  FLOW_ORCHESTRATOR_COORDINATOR_URL  coordinator base URL
  FLOW_ORCHESTRATOR_NAMESPACE        worker Deployment namespace
  FLOW_ORCHESTRATOR_DEPLOYMENT       worker Deployment name
  FLOW_ORCHESTRATOR_MIN_REPLICAS     standby pool floor (default 0)
  FLOW_ORCHESTRATOR_MAX_REPLICAS     scale-out ceiling (default 10)
  FLOW_ORCHESTRATOR_DESIRED_REPLICAS standby pool size (default 1)
  KUBERNETES_SERVICE_HOST/PORT       Kubernetes API address (in-cluster)`)
}
