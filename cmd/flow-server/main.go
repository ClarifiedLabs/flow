package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	flowtoken "github.com/ClarifiedLabs/flow/internal/token"
	"github.com/ClarifiedLabs/flow/internal/version"
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
	var exchangeBaseURL string
	var ownerToken string
	var ownerTokenFile string
	var hookToken string
	var hookTokenFile string
	var workerJoinToken string
	var workerJoinTokenFile string
	var clientConfigPathFlag string
	var noWriteClientConfig bool
	flags.StringVar(&configPath, "config", "", "coordinator config JSON path")
	flags.StringVar(&addr, "addr", "", "listen address")
	flags.StringVar(&dataDir, "data-dir", "", "Flow data directory")
	flags.StringVar(&exchangeBaseURL, "exchange-base-url", "", "public base URL for HTTP git exchange remotes")
	flags.StringVar(&ownerToken, "owner-token", "", "owner bearer token")
	flags.StringVar(&ownerTokenFile, "owner-token-file", "", "mode-0600 file containing the owner bearer token")
	flags.StringVar(&hookToken, "hook-token", "", "hook bearer token")
	flags.StringVar(&hookTokenFile, "hook-token-file", "", "mode-0600 file containing the hook bearer token")
	flags.StringVar(&workerJoinToken, "worker-join-token", "", "worker join bearer token")
	flags.StringVar(&workerJoinTokenFile, "worker-join-token-file", "", "mode-0600 file containing the worker join bearer token")
	flags.StringVar(&clientConfigPathFlag, "client-config", "", "client config path to write for local CLI discovery")
	flags.BoolVar(&noWriteClientConfig, "no-write-client-config", false, "do not write a local client config")
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
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if addr != "" {
		cfg.ListenAddr = addr
		if exchangeBaseURL == "" {
			cfg.ExchangeBaseURL = config.CoordinatorURLForListenAddr(cfg.ListenAddr)
		}
	}
	if exchangeBaseURL != "" {
		cfg.ExchangeBaseURL = strings.TrimRight(strings.TrimSpace(exchangeBaseURL), "/")
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
	slog.Debug("flow-server serve configuration loaded", "addr", cfg.ListenAddr, "database", cfg.GlobalDatabasePath(), "data_dir", cfg.DataDir)

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

	deadlines, err := cfg.Deadlines.ResolveDeadlines()
	if err != nil {
		fmt.Fprintf(stderr, "resolve deadlines: %v\n", err)
		return 1
	}
	limits, err := cfg.Limits.ResolveLimits()
	if err != nil {
		fmt.Fprintf(stderr, "resolve limits: %v\n", err)
		return 1
	}

	registry, err := api.NewRegistry(api.RegistryOptions{
		DataDir:                    cfg.DataDir,
		Global:                     globalStore,
		ExchangeBaseURL:            cfg.ExchangeBaseURL,
		AuthorEntrypoint:           cfg.AuthorEntrypoint,
		AuthorEntrypointConfigured: cfg.AuthorEntrypointConfigured,
		HarnessArgs:                cfg.HarnessArgs,
		Deadlines: lifecycle.DeadlineConfig{
			CheckPending:   deadlines.CheckPending,
			AuthoringStall: deadlines.AuthoringStall,
		},
		ReviewAuthorCycleLimit: limits.ReviewAuthorCycles,
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

	if err := registry.OpenAll(context.Background()); err != nil {
		fmt.Fprintf(stderr, "open projects: %v\n", err)
		return 1
	}

	server, err := api.NewServer(api.ServerOptions{
		Registry:        registry,
		OwnerToken:      ownerToken,
		HookToken:       hookToken,
		WorkerJoinToken: workerJoinToken,
		ProtocolVersion: cfg.ProtocolVersion,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create api server: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "flow-server listening on %s\n", cfg.ListenAddr)
	fmt.Fprintf(stdout, "database: %s\n", cfg.GlobalDatabasePath())
	fmt.Fprintf(stdout, "projects: %d\n", len(registry.All()))
	fmt.Fprintf(stdout, "exchange_base_url: %s\n", cfg.ExchangeBaseURL)
	fmt.Fprintf(stdout, "owner_token_file: %s\n", ownerTokenFileDisplay)
	fmt.Fprintf(stdout, "hook_token_file: %s\n", hookTokenFileDisplay)
	fmt.Fprintf(stdout, "client_config_file: %s\n", clientConfigPath)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Durable background ticker: drains due timers and runs crash recovery on an
	// interval, so recovery fires regardless of inbound API traffic. It is
	// stopped (and awaited) before the deferred close runs. Projects opened
	// while serving join the loop on the next tick.
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		runLifecycleTicker(ctx, registry, stderr)
	}()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: server}
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
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "serve: %v\n", serveErr)
		return 1
	}

	return 0
}

// lifecycleTickConcurrency bounds how many projects tick in parallel per tick so
// a large registry cannot exhaust connections or goroutines.
const lifecycleTickConcurrency = 8

func runLifecycleTicker(ctx context.Context, registry *api.Registry, stderr io.Writer) {
	ticker := time.NewTicker(lifecycleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Debug("lifecycle ticker tick")
			tickProjects(ctx, registry.All(), stderr)
		}
	}
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
	if err := bundle.Engine.Tick(ctx); err != nil && ctx.Err() == nil {
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
	fmt.Fprintf(stdout, "exchange_base_url: %s\n", cfg.ExchangeBaseURL)
	fmt.Fprintf(stdout, "protocol: %s\n", cfg.ProtocolVersion)
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
  flow-server serve [--data-dir PATH] [--addr HOST:PORT] [--exchange-base-url URL] [--worker-join-token TOKEN | --worker-join-token-file PATH] [--owner-token TOKEN | --owner-token-file PATH] [--hook-token TOKEN | --hook-token-file PATH] [--client-config PATH | --no-write-client-config]
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
	clientCfg, err := config.LocalClientConfig(cfg.DataDir, config.CoordinatorURLForListenAddr(cfg.ListenAddr), ownerToken, cfg.ProtocolVersion)
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
