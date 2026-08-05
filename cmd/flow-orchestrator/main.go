// flow-orchestrator reconciles durable coordinator assignments into exactly one
// one-shot worker resource per Flow job.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/ClarifiedLabs/flow/internal/worker"
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
	profiles, providers, err := buildProfilesAndProviders(resolved)
	if err != nil {
		fmt.Fprintf(stderr, "configure assignment providers: %v\n", err)
		return 1
	}
	coordinatorClient := orchestrator.NewCoordinatorClient(resolved.CoordinatorURL, resolved.Token, nil)

	telemetryRegistry := metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "flow_orchestrator_build_info",
		Help:    "flow-orchestrator build information.",
		Version: version.Current().String(),
	})
	reconciler, err := orchestrator.NewReconciler(orchestrator.ReconcilerOptions{
		Coordinator: coordinatorClient, CoordinatorURL: resolved.CoordinatorURL,
		Profiles: profiles, Providers: providers, ProviderFactory: providerFromProfile,
		PollInterval: resolved.PollInterval, RetryBase: resolved.RetryBase, RetryMax: resolved.RetryMax,
		Metrics: orchestrator.NewMetrics(telemetryRegistry),
	})
	if err != nil {
		fmt.Fprintf(stderr, "create assignment reconciler: %v\n", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	telemetrySettings := metrics.Resolve(resolved.Metrics, config.DefaultTelemetryListen, metrics.Overrides{
		Disable:    noMetrics,
		DisableSet: noMetrics,
		Listen:     metricsListen,
		ListenSet:  strings.TrimSpace(metricsListen) != "",
	})
	telemetry, err := metrics.StartEndpoint(ctx, slog.Default(), metrics.Mux(telemetryRegistry, reconciler.Ready), telemetrySettings)
	if err != nil {
		fmt.Fprintf(stderr, "start telemetry endpoint: %v\n", err)
		return 1
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), metrics.DefaultShutdownTimeout)
		defer shutdownCancel()
		_ = telemetry.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(stdout, "orchestrator: reconciling %d assignment profile(s) from %s every %s\n",
		len(profiles), resolved.CoordinatorURL, resolved.PollInterval)
	if err := reconciler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "run assignment reconciler: %v\n", err)
		return 1
	}
	return 0
}

func buildProfilesAndProviders(cfg config.ResolvedOrchestrator) ([]orchestrator.Profile, map[string]orchestrator.Provider, error) {
	profiles := make([]orchestrator.Profile, 0, len(cfg.Profiles))
	providers := make(map[string]orchestrator.Provider, len(cfg.Profiles))
	for _, configured := range cfg.Profiles {
		providerKey := configured.ProviderID + "/" + configured.Name
		profile := orchestrator.Profile{
			ProviderID: configured.ProviderID, ProfileName: configured.Name,
			MaxConcurrency: configured.MaxConcurrency, Labels: configured.Labels,
			Taints: configured.Taints, HarnessModels: configured.HarnessModels,
			RequiredSelector: configured.RequiredSelector, StartupTimeout: configured.StartupTimeout,
			Provider: providerKey, ProviderType: configured.Provider, ProviderOptions: make(map[string]string),
		}
		for _, role := range configured.AllowedRoles {
			profile.AllowedRoles = append(profile.AllowedRoles, worker.JobRole(role))
		}
		for _, bucket := range configured.Accepts {
			profile.AllowedBuckets = append(profile.AllowedBuckets, worker.CapacityBucket(bucket))
		}

		switch configured.Provider {
		case "kubernetes":
			if configured.Kubernetes == nil {
				return nil, nil, fmt.Errorf("profile %s: kubernetes configuration is missing", configured.Name)
			}
			options := configured.Kubernetes
			profile.ProviderOptions = map[string]string{
				"namespace": options.Namespace, "image": options.Image, "service_account": options.ServiceAccount,
				"work_dir": options.WorkDir, "worker_args": encodeStringSlice(options.WorkerArgs),
				"image_pull_policy":               options.ImagePullPolicy,
				"harness_model_proxy_secret_name": options.HarnessModelProxySecretName,
			}
		case "darwin":
			if configured.Darwin == nil {
				return nil, nil, fmt.Errorf("profile %s: darwin configuration is missing", configured.Name)
			}
			options := configured.Darwin
			profile.ProviderOptions = map[string]string{
				"state_dir": options.StateDir, "executable": options.Binary, "work_dir": options.WorkDir,
				"worker_args": encodeStringSlice(options.WorkerArgs),
			}
		default:
			return nil, nil, fmt.Errorf("unsupported provider %q", configured.Provider)
		}
		provider, err := providerFromProfile(profile)
		if err != nil {
			return nil, nil, fmt.Errorf("profile %s (%s): %w", configured.Name, configured.Provider, err)
		}
		providers[providerKey] = provider
		profiles = append(profiles, profile)
	}
	return profiles, providers, nil
}

func providerFromProfile(profile orchestrator.Profile) (orchestrator.Provider, error) {
	var workerArgs []string
	if raw := strings.TrimSpace(profile.ProviderOptions["worker_args"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &workerArgs); err != nil {
			return nil, fmt.Errorf("decode persisted worker args: %w", err)
		}
	}
	switch profile.ProviderType {
	case "kubernetes":
		return orchestrator.NewInClusterKubernetesProvider(orchestrator.KubernetesProviderOptions{
			Namespace: profile.ProviderOptions["namespace"], Image: profile.ProviderOptions["image"],
			ServiceAccount: profile.ProviderOptions["service_account"], WorkDir: profile.ProviderOptions["work_dir"],
			WorkerArgs: workerArgs, ImagePullPolicy: profile.ProviderOptions["image_pull_policy"],
		})
	case "darwin":
		return orchestrator.NewDarwinProcessProvider(orchestrator.DarwinProcessProviderOptions{
			StateDir: profile.ProviderOptions["state_dir"], Executable: profile.ProviderOptions["executable"],
			WorkDir: profile.ProviderOptions["work_dir"], WorkerArgs: workerArgs,
		})
	default:
		return nil, fmt.Errorf("unsupported persisted provider type %q", profile.ProviderType)
	}
}

func encodeStringSlice(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `flow-orchestrator reconciles durable coordinator assignments into exactly one
one-shot worker resource per Flow job.

Usage:
  flow-orchestrator [flags]
  flow-orchestrator version

Flags:
  -c, --config <path>     orchestrator config path (JSON or YAML)
      --no-metrics        disable the telemetry endpoint
      --metrics-listen    telemetry endpoint listen address (default 127.0.0.1:8422)

Provider profiles are file-only. Environment:
  FLOW_ORCHESTRATOR_TOKEN            orchestrator-scoped bearer token (required)
  FLOW_ORCHESTRATOR_COORDINATOR_URL  coordinator base URL
  FLOW_ORCHESTRATOR_POLL_INTERVAL    reconciliation interval
  FLOW_ORCHESTRATOR_RETRY_BASE       initial provider retry delay
  FLOW_ORCHESTRATOR_RETRY_MAX        maximum provider retry delay
  KUBERNETES_SERVICE_HOST/PORT       Kubernetes API address (in-cluster)`)
}
