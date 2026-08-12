package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/cliout"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
	"github.com/ClarifiedLabs/flow/internal/harness"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	flowmcp "github.com/ClarifiedLabs/flow/internal/mcp"
	flowprompt "github.com/ClarifiedLabs/flow/internal/prompt"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/version"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type globalOptions struct {
	configPath string
	configSet  bool
	jsonOut    bool
	agentOut   bool
}

func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	options := globalOptions{}
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--config":
			if len(args) == 1 || args[1] == "--" || strings.HasPrefix(args[1], "-") {
				return globalOptions{}, nil, errors.New("--config requires a value")
			}
			options.configPath = args[1]
			options.configSet = true
			args = args[2:]
		case strings.HasPrefix(arg, "--config="):
			options.configPath = strings.TrimPrefix(arg, "--config=")
			options.configSet = true
			args = args[1:]
		case arg == "--json" || arg == "--agent":
			if err := setOutputFlag(&options, arg, "true"); err != nil {
				return globalOptions{}, nil, err
			}
			args = args[1:]
		case strings.HasPrefix(arg, "--json=") || strings.HasPrefix(arg, "--agent="):
			name, value, _ := strings.Cut(arg, "=")
			if err := setOutputFlag(&options, name, value); err != nil {
				return globalOptions{}, nil, err
			}
			args = args[1:]
		default:
			return options, args, nil
		}
	}
	return options, args, nil
}

func setOutputFlag(options *globalOptions, name string, value string) error {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s requires a boolean value: %w", name, err)
	}
	if name == "--json" {
		options.jsonOut = parsed
	} else {
		options.agentOut = parsed
	}
	return nil
}

func (options globalOptions) withConfig(args []string) []string {
	var prefix []string
	if options.configSet {
		prefix = append(prefix, "--config", options.configPath)
	}
	if options.agentOut {
		prefix = append(prefix, "--agent")
	} else if options.jsonOut {
		prefix = append(prefix, "--json")
	}
	if len(prefix) == 0 {
		return args
	}
	return append(prefix, args...)
}

func run(args []string, stdout, stderr io.Writer) int {
	configuredArgs, restoreLogging, err := flowlog.Configure(args, stderr, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configure logging: %v\n", err)
		return 2
	}
	defer restoreLogging()

	options, args, err := parseGlobalOptions(configuredArgs)
	if err != nil {
		fmt.Fprintf(stderr, "parse global options: %v\n", err)
		return 2
	}
	slog.Debug("flow command start", "command", flowlog.CommandName(args))

	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "--version", "version":
		return runVersion(options.withConfig(nil), stdout, stderr)
	case "init":
		return runInit(options.withConfig(args[1:]), stdout, stderr)
	case "doctor":
		return runDoctorWithGlobalOptions(args[1:], options, stdout, stderr)
	case "task":
		return runTaskWithGlobalOptions(args[1:], options, stdout, stderr)
	case "feature":
		return runFeatureWithGlobalOptions(args[1:], options, stdout, stderr)
	case "epic":
		return runEpicWithGlobalOptions(args[1:], options, stdout, stderr)
	case "work-item":
		return runWorkItemWithGlobalOptions(args[1:], options, stdout, stderr)
	case "board":
		return runBoard(options.withConfig(args[1:]), stdout, stderr)
	case "ready":
		return runReadyTasks(options.withConfig(args[1:]), stdout, stderr)
	case "next":
		return runNextTask(options.withConfig(args[1:]), stdout, stderr)
	case "wait":
		return runWait(options.withConfig(args[1:]), stdout, stderr)
	case "events":
		return runEvents(options.withConfig(args[1:]), stdout, stderr)
	case "quickstart", "agent-instructions":
		return runQuickstart(options.withConfig(args[1:]), stdout, stderr)
	case "search":
		return runSearch(options.withConfig(args[1:]), stdout, stderr)
	case "audit":
		return runAuditWithGlobalOptions(args[1:], options, stdout, stderr)
	case "mcp":
		return runMCPWithGlobalOptions(args[1:], options, stdout, stderr)
	case "checks":
		return runChecks(options.withConfig(args[1:]), stdout, stderr)
	case "transitions":
		return runTransitions(options.withConfig(args[1:]), stdout, stderr)
	case "workers":
		return runWorkers(options.withConfig(args[1:]), stdout, stderr)
	case "jobs":
		return runJobs(options.withConfig(args[1:]), stdout, stderr)
	case "history":
		return runHistoryWithGlobalOptions(args[1:], options, stdout, stderr)
	case "attach":
		return runAttach(options.withConfig(args[1:]), stdout, stderr)
	case "session":
		return runSessionWithGlobalOptions(args[1:], options, stdout, stderr)
	case "hook":
		return runHookWithGlobalOptions(args[1:], options, stdout, stderr)
	case "fetch-prompt":
		return runFetchPrompt(options.withConfig(args[1:]), stdout, stderr)
	case "comment":
		return runComment(options.withConfig(args[1:]), stdout, stderr)
	case "thread":
		return runThreadWithGlobalOptions(args[1:], options, stdout, stderr)
	case "status":
		return runStatus(options.withConfig(args[1:]), stdout, stderr)
	case "ask":
		return runAsk(options.withConfig(args[1:]), stdout, stderr)
	case "complete":
		return runCompleteWithGlobalOptions(args[1:], options, stdout, stderr)
	case "submit":
		return runSubmit(options.withConfig(args[1:]), stdout, stderr)
	case "flows":
		return runFlowsWithGlobalOptions(args[1:], options, stdout, stderr)
	case "agent-defs":
		return runAgentDefsWithGlobalOptions(args[1:], options, stdout, stderr)
	case "ui":
		return runUI(options.withConfig(args[1:]), stdout, stderr)
	case "reconcile":
		return runReconcile(options.withConfig(args[1:]), stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	return runDoctorWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runDoctorWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) != 0 && args[0] == "work-items" {
		parsed, code := parseAPICommand(options.withConfig(args[1:]), stderr, "doctor work-items", 0, "doctor work-items does not accept positional arguments")
		if code != 0 {
			return code
		}
		report, err := parsed.client.DoctorWorkItems()
		if err != nil {
			fmt.Fprintf(stderr, "check work items: %v\n", err)
			return 1
		}
		if report.Healthy {
			fmt.Fprintln(stdout, "work-items: ok")
			return 0
		}
		for _, issue := range report.Issues {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", issue.Code, issue.ItemID, issue.Message)
		}
		return 1
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var dbPath string
	var clientConfigPath string
	flags.StringVar(&dbPath, "db", "", "coordinator global SQLite database path to initialize")
	flags.StringVar(&clientConfigPath, "config", "", "client config JSON path")

	if err := flags.Parse(options.withConfig(args)); err != nil {
		return 2
	}

	clientCfg, err := config.LoadClient(clientConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "load client config: %v\n", err)
		return 1
	}

	if strings.TrimSpace(dbPath) == "" {
		dataDir, err := config.ResolveClientDataDir(clientCfg)
		if err != nil {
			fmt.Fprintf(stderr, "resolve data dir: %v\n", err)
			return 1
		}
		dbPath = filepath.Join(dataDir, "global.db")
	}

	store, err := db.OpenGlobal(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "initialize database: %v\n", err)
		return 1
	}
	defer store.Close()

	migrations, err := store.AppliedMigrations(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "read migrations: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "flow doctor")
	fmt.Fprintf(stdout, "version: %s\n", version.Current())
	fmt.Fprintf(stdout, "server: %s\n", clientCfg.ServerURL)
	fmt.Fprintf(stdout, "protocol: %s\n", contract.ProtocolVersion)
	fmt.Fprintf(stdout, "database: %s\n", store.Path())
	fmt.Fprintln(stdout, "sqlite: ok")
	fmt.Fprintf(stdout, "migrations: %s\n", strings.Join(migrations, ", "))

	return 0
}

func runInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var repoPath string
	var name string
	var baseBranch string
	var exchangeName string
	var withAgents bool
	apiFlags := addAPIFlags(flags)
	flags.StringVar(&repoPath, "repo", ".", "git worktree to register as a Flow project")
	flags.StringVar(&name, "name", "", "project name (default: repo directory name)")
	flags.StringVar(&baseBranch, "base", "", "base branch to seed and protect (default: current branch)")
	flags.StringVar(&exchangeName, "exchange-name", "", "git remote name for the Flow exchange (default flow)")
	flags.BoolVar(&withAgents, "with-agents", false, "write the flow agent block into AGENTS.md/CLAUDE.md")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "init does not accept positional arguments")
		return 2
	}

	repoRoot, err := resolveInitRepoRoot(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "resolve repository: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "repo: %s\n", repoRoot)

	client, clientCfg, err := newInitClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "connect to flow-server: %v\n", err)
		return 1
	}

	if strings.TrimSpace(baseBranch) == "" {
		baseBranch, err = currentBranch(repoRoot)
		if err != nil {
			fmt.Fprintf(stderr, "detect base branch: %v\n", err)
			return 1
		}
	}

	project, created, err := client.CreateProject(flowclient.CreateProjectInput{
		Name:         strings.TrimSpace(name),
		RepoPath:     repoRoot,
		BaseBranch:   strings.TrimSpace(baseBranch),
		ExchangeName: strings.TrimSpace(exchangeName),
	})
	if err != nil {
		fmt.Fprintf(stderr, "register project: %v\n", err)
		return 1
	}
	exchangeURL, err := flowgit.ExchangeHTTPURL(clientCfg.ServerURL, project.ID)
	if err != nil {
		fmt.Fprintf(stderr, "resolve exchange remote: %v\n", err)
		return 1
	}

	seed, err := flowgit.SeedExchangeFromWorktree(context.Background(), flowgit.SeedOptions{
		RepoPath:     repoRoot,
		BaseBranch:   project.BaseBranch,
		ExchangeName: project.ExchangeName,
		ExchangeURL:  exchangeURL,
		Token:        clientCfg.Token,
	})
	if err != nil {
		fmt.Fprintf(stderr, "seed exchange remote: %v\n", err)
		return 1
	}
	if seed.Warning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", seed.Warning)
	}
	credentialStored, credentialCommand, err := approveGitCredential(repoRoot, exchangeURL, clientCfg.Token)
	if err != nil {
		fmt.Fprintf(stderr, "warning: git credential storage skipped: %v\n", err)
	} else if credentialStored {
		fmt.Fprintln(stdout, "git_credential: stored")
	}

	if err := writeInitClientConfig(clientCfg); err != nil {
		fmt.Fprintf(stderr, "write client config: %v\n", err)
		return 1
	}
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "resolve client config path: %v\n", err)
		return 1
	}

	if created {
		fmt.Fprintln(stdout, "flow project created")
	} else {
		fmt.Fprintln(stdout, "flow project already registered")
	}
	fmt.Fprintf(stdout, "project_id: %s\n", project.ID)
	fmt.Fprintf(stdout, "name: %s\n", project.Name)
	fmt.Fprintf(stdout, "base_branch: %s\n", project.BaseBranch)
	fmt.Fprintf(stdout, "exchange_remote: %s -> %s\n", project.ExchangeName, exchangeURL)
	fmt.Fprintf(stdout, "client_config: %s\n", configPath)
	fmt.Fprintln(stdout, "next:")
	if credentialCommand != "" {
		fmt.Fprintln(stdout, "  # optional: configure a git credential helper, then store the Flow Git credential")
		fmt.Fprintf(stdout, "  %s\n", credentialCommand)
	}
	fmt.Fprintln(stdout, "  flow task create --title \"...\"   # project auto-detected from this repo")
	fmt.Fprintln(stdout, "  flow board")
	if withAgents {
		installAgentsBlocks(repoRoot, stdout)
	}
	return 0
}

func approveGitCredential(repoRoot string, exchangeURL string, token string) (bool, string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return false, "", nil
	}
	parsed, err := url.Parse(strings.TrimSpace(exchangeURL))
	if err != nil {
		return false, "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false, "", nil
	}
	credential := gitCredentialInput(parsed, token)
	command := gitCredentialApproveCommand(parsed)

	if err := gitConfig(repoRoot, "credential.useHttpPath", "true"); err != nil {
		return false, command, err
	}
	configured, err := gitCredentialHelperConfigured(repoRoot)
	if err != nil {
		return false, command, err
	}
	if !configured {
		return false, command, nil
	}

	cmd := exec.Command("git", "credential", "approve")
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(credential)
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, command, fmt.Errorf("git credential approve: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return true, "", nil
}

func gitConfig(repoRoot string, key string, value string) error {
	cmd := exec.Command("git", "config", key, value)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config %s: %s: %w", key, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func gitCredentialHelperConfigured(repoRoot string) (bool, error) {
	cmd := exec.Command("git", "config", "--get-all", "credential.helper")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			return true, nil
		}
	}
	return false, nil
}

func gitCredentialInput(parsed *url.URL, token string) string {
	return "protocol=" + parsed.Scheme + "\n" +
		"host=" + parsed.Host + "\n" +
		"path=" + strings.TrimPrefix(parsed.Path, "/") + "\n" +
		"username=flow\n" +
		"password=" + token + "\n\n"
}

func gitCredentialApproveCommand(parsed *url.URL) string {
	return "printf 'protocol=" + shellEscapeForPrintf(parsed.Scheme) +
		"\\nhost=" + shellEscapeForPrintf(parsed.Host) +
		"\\npath=" + shellEscapeForPrintf(strings.TrimPrefix(parsed.Path, "/")) +
		"\\nusername=flow\\npassword=%s\\n\\n' \"$FLOW_OWNER_TOKEN\" | git credential approve"
}

func shellEscapeForPrintf(value string) string {
	return strings.ReplaceAll(value, "'", "'\"'\"'")
}

// newInitClient builds an owner-authenticated client for project
// registration. The owner token comes from explicit flags, Flow token
// environment, the client config, or — for a same-machine coordinator — the
// owner.token file in the data dir.
func newInitClient(values *apiFlagValues) (*flowclient.Client, config.ClientConfig, error) {
	cfg, err := resolvedAPIConfig(values)
	if err != nil {
		return nil, config.ClientConfig{}, err
	}

	if strings.TrimSpace(cfg.Token) == "" {
		dataDir, dataDirErr := config.ResolveClientDataDir(cfg)
		if dataDirErr != nil {
			return nil, config.ClientConfig{}, dataDirErr
		}
		return nil, config.ClientConfig{}, fmt.Errorf("no owner token: pass --token, set FLOW_OWNER_TOKEN, or start flow-server serve on this machine first (looked for %s)", config.OwnerTokenPath(dataDir))
	}

	client, err := flowclient.New(cfg)
	if err != nil {
		return nil, config.ClientConfig{}, err
	}

	return client, cfg, nil
}

// writeInitClientConfig records the coordinator URL and owner credential in
// $XDG_CONFIG_HOME/flow/config.yaml so later commands need no flags. A local
// owner.token file is referenced rather than copied.
func writeInitClientConfig(clientCfg config.ClientConfig) error {
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		return err
	}

	cfg, err := config.LocalClientConfig(clientCfg.DataDir, clientCfg.ServerURL, clientCfg.Token)
	if err != nil {
		return err
	}

	return config.WriteClientConfig(configPath, cfg)
}

func currentBranch(repoRoot string) (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("detect current branch: %s", strings.TrimSpace(string(output)))
	}
	branch := strings.TrimSpace(string(output))
	if branch == "" {
		return "", errors.New("detect current branch: detached HEAD; pass --base")
	}

	return branch, nil
}

func runTask(args []string, stdout, stderr io.Writer) int {
	return runTaskWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runTaskWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTaskUsage(stderr)
		return 2
	}

	switch args[0] {
	case "create":
		return runTaskCreate(options.withConfig(args[1:]), stdout, stderr)
	case "attach":
		return runTaskAttach(options.withConfig(args[1:]), stdout, stderr)
	case "list":
		return runTaskList(options.withConfig(args[1:]), stdout, stderr)
	case "show":
		return runTaskShow(options.withConfig(args[1:]), stdout, stderr)
	case "edit":
		return runTaskEdit(options.withConfig(args[1:]), stdout, stderr)
	case "schedule":
		return runTaskSchedule(options.withConfig(args[1:]), stdout, stderr)
	case "reset":
		return runTaskReset(options.withConfig(args[1:]), stdout, stderr)
	case "done":
		return runTaskDone(options.withConfig(args[1:]), stdout, stderr)
	case "reopen":
		return runTaskReopen(options.withConfig(args[1:]), stdout, stderr)
	case "workflow":
		return runTaskWorkflow(options.withConfig(args[1:]), stdout, stderr)
	case "respond":
		return runTaskRespond(options.withConfig(args[1:]), stdout, stderr)
	case "budget":
		return runTaskBudget(options.withConfig(args[1:]), stdout, stderr)
	case "retry":
		return runTaskRetry(options.withConfig(args[1:]), stdout, stderr)
	case "link":
		return runTaskLink(options.withConfig(args[1:]), stdout, stderr)
	case "unlink":
		return runTaskUnlink(options.withConfig(args[1:]), stdout, stderr)
	case "relations":
		return runTaskRelations(options.withConfig(args[1:]), stdout, stderr)
	case "reply":
		return runTaskReply(options.withConfig(args[1:]), stdout, stderr)
	case "guide":
		return runTaskGuide(options.withConfig(args[1:]), stdout, stderr)
	case "decide-review":
		return runTaskDecideReview(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown task command: %s\n\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
}

// generateIdempotencyKey returns a random request replay key with a
// human-readable prefix identifying the operation that created it.
func generateIdempotencyKey(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func runTaskGuide(args []string, stdout, stderr io.Writer) int {
	args = ownerCommandTrailingFlags(args, 2)
	flags := flag.NewFlagSet("task guide", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var supersedesID string
	var idempotencyKey string
	flags.StringVar(&supersedesID, "supersedes", "", "active ruling id this ruling replaces")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "request replay key (generated when omitted)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 2 || strings.TrimSpace(flags.Arg(1)) == "" {
		fmt.Fprintln(stderr, "usage: flow task guide [flags] TASK_ID MESSAGE [--supersedes RULING_ID] [--idempotency-key KEY]")
		return 2
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		generated, err := generateIdempotencyKey("guide-")
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		idempotencyKey = generated
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	result, err := client.RecordOwnerRuling(taskRef, flags.Arg(1), supersedesID, idempotencyKey)
	if err != nil {
		fmt.Fprintf(stderr, "record owner ruling: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\tqueued %d/%d\n", result.Ruling.RulingID, result.Delivery.QueuedSessions, result.Delivery.TargetedSessions)
	return 0
}

func runTaskDecideReview(args []string, stdout, stderr io.Writer) int {
	args = ownerCommandTrailingFlags(args, 3)
	flags := flag.NewFlagSet("task decide-review", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var guidance string
	var idempotencyKey string
	flags.StringVar(&guidance, "guidance", "", "optional owner guidance appended to the ruling")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "request replay key (generated when omitted)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 3 {
		fmt.Fprintln(stderr, "usage: flow task decide-review [flags] TASK_ID WAIT_ID fix_in_task|out_of_scope|defer_follow_up")
		return 2
	}
	choice := coordinator.ReviewScopeDecisionChoice(strings.TrimSpace(flags.Arg(2)))
	if choice != coordinator.ReviewScopeFixInTask && choice != coordinator.ReviewScopeOutOfScope && choice != coordinator.ReviewScopeDeferFollowUp {
		fmt.Fprintf(stderr, "invalid review decision choice %q\n", choice)
		return 2
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		generated, err := generateIdempotencyKey("review-decision-")
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		idempotencyKey = generated
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	result, err := client.ResolveReviewScopeDecision(taskRef, flags.Arg(1), choice, guidance, idempotencyKey)
	if err != nil {
		fmt.Fprintf(stderr, "resolve review scope decision: %v\n", err)
		return 1
	}
	rulingID := ""
	if result.Ruling != nil {
		rulingID = result.Ruling.RulingID
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", result.Result, result.Run.State, rulingID)
	return 0
}

// ownerCommandTrailingFlags accepts the documented noun-first form (for
// example `task guide TASK MESSAGE --supersedes ID`) while retaining the
// standard flag-first form used by the rest of the CLI. The positional values
// are opaque and never re-tokenized.
func ownerCommandTrailingFlags(args []string, positionalCount int) []string {
	if positionalCount < 1 || len(args) <= positionalCount || strings.HasPrefix(args[0], "-") {
		return args
	}
	for index := 0; index < positionalCount; index++ {
		if strings.HasPrefix(args[index], "-") {
			return args
		}
	}
	result := append([]string(nil), args[positionalCount:]...)
	return append(result, args[:positionalCount]...)
}

func runTaskCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	var priority int
	var attachmentFiles stringSliceFlag
	flags.StringVar(&title, "title", "", "task title")
	flags.StringVar(&body, "body", "", "task body")
	flags.IntVar(&priority, "priority", 0, "task priority")
	var flowRef string
	var featureRef string
	var parentItemID string
	var idempotencyKey string
	flags.StringVar(&flowRef, "flow", "", "workflow (id or name) used when the task is scheduled")
	flags.StringVar(&featureRef, "feature", "", "feature (id or title) the task is assigned to")
	flags.StringVar(&parentItemID, "parent", "", "organizational parent epic or feature id")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "request replay key (generated when omitted)")
	flags.Var(&attachmentFiles, "file", "file to attach to the initial task prompt (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "task create", "create client", err)
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		generated, err := generateIdempotencyKey("create-")
		if err != nil {
			return machineError(stdout, stderr, mode, "task create", "generate idempotency key", err)
		}
		idempotencyKey = generated
	}
	input := flowclient.CreateTaskInput{
		Title: title, Body: body, Priority: priority, ParentItemID: parentItemID,
		IdempotencyKey: idempotencyKey,
	}
	if strings.TrimSpace(flowRef) != "" {
		flowID, err := resolveFlowRef(client, flowRef)
		if err != nil {
			return machineError(stdout, stderr, mode, "task create", "resolve flow", err)
		}
		input.FlowID = flowID
	}
	if strings.TrimSpace(featureRef) != "" {
		featureID, err := resolveFeatureRef(client, featureRef)
		if err != nil {
			return machineError(stdout, stderr, mode, "task create", "resolve feature", err)
		}
		input.FeatureID = featureID
	}
	task, reused, err := client.CreateTask(input)
	if err != nil {
		return machineError(stdout, stderr, mode, "task create", "create task", err)
	}
	slog.Debug("task create response", "task", task.ID, "reused", reused)
	attachments := make([]coordinator.TaskAttachment, 0, len(attachmentFiles.Values))
	if !mode.Machine() {
		printTaskLine(stdout, task)
	}
	for _, filePath := range attachmentFiles.Values {
		attachment, err := uploadTaskAttachmentFile(client, task.ID, filePath, coordinator.TaskAttachmentStageInitial)
		if err != nil {
			return machineError(stdout, stderr, mode, "task create", "attach file to "+task.ID, err)
		}
		attachments = append(attachments, attachment)
		if !mode.Machine() {
			printTaskAttachmentLine(stdout, attachment)
		}
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "task create", map[string]any{
			"task":        task,
			"reused":      reused,
			"attachments": attachments,
		})
	}

	return 0
}

func runTaskAttach(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task attach", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var filePath string
	var stage string
	flags.StringVar(&filePath, "file", "", "file to attach")
	flags.StringVar(&stage, "stage", "", "attachment stage: initial, author, reviewer, or verifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "task id is required")
		return 2
	}
	if strings.TrimSpace(filePath) == "" {
		fmt.Fprintln(stderr, "--file is required")
		return 2
	}
	attachmentStage, err := taskAttachmentStageFromCLI(stage)
	if err != nil {
		fmt.Fprintf(stderr, "invalid attachment stage: %v\n", err)
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	attachment, err := uploadTaskAttachmentFile(client, taskRef, filePath, attachmentStage)
	if err != nil {
		fmt.Fprintf(stderr, "attach file: %v\n", err)
		return 1
	}

	printTaskAttachmentLine(stdout, attachment)
	return 0
}

func runTaskList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var lifecycleStates string
	var tag string
	flags.StringVar(&lifecycleStates, "state", "", "comma-separated unscheduled,scheduled,in_progress,done states")
	flags.StringVar(&tag, "tag", "", "comma-separated tag slugs")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	tasks, err := client.ListTasks(flowclient.TaskFilter{
		LifecycleStates: parseCSV(lifecycleStates),
		TagSlugs:        parseCSV(tag),
	})
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "task list", "list tasks", err)
	}
	if apiFlags.outputMode().Machine() {
		if tasks == nil {
			tasks = []coordinator.Task{}
		}
		return cliout.WriteData(stdout, apiFlags.outputMode(), "task list", map[string]any{"tasks": tasks})
	}

	for _, task := range tasks {
		printTaskLine(stdout, task)
	}
	return 0
}

func runTaskShow(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 1 {
		return machineUsage(stdout, stderr, mode, "task show", "task id is required")
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "task show", "create client", err)
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	task, err := client.GetTask(taskRef)
	if err != nil {
		return machineError(stdout, stderr, mode, "task show", "show task", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "task show", map[string]any{"task": task})
	}

	printTaskDetail(stdout, task)
	return 0
}

func runTaskReply(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task reply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var message string
	var statusLogID int64
	flags.StringVar(&message, "message", "", "reply message")
	flags.Int64Var(&statusLogID, "status-log-id", 0, "status log entry this reply answers")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: flow task reply [flags] TASK_ID [MESSAGE]")
		return 2
	}
	if strings.TrimSpace(message) == "" && flags.NArg() > 1 {
		message = strings.TrimSpace(strings.Join(flags.Args()[1:], " "))
	}
	if strings.TrimSpace(message) == "" {
		fmt.Fprintln(stderr, "reply message is required")
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	var statusLogIDPtr *int64
	if statusLogID > 0 {
		statusLogIDPtr = &statusLogID
	}
	messageResult, queued, err := client.ReplyToTask(taskRef, flowclient.ReplyToTaskInput{
		Message:     message,
		StatusLogID: statusLogIDPtr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "reply task: %v\n", err)
		return 1
	}
	if queued {
		fmt.Fprintf(stdout, "%s\t%s\tqueued\n", messageResult.ID, messageResult.SessionID)
	} else {
		fmt.Fprintln(stdout, "recorded")
	}
	return 0
}

func runTaskEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	var priority string
	var flowRef string
	var featureRef string
	flags.StringVar(&title, "title", "", "new task title")
	flags.StringVar(&body, "body", "", "new task body")
	flags.StringVar(&priority, "priority", "", "new task priority")
	flags.StringVar(&flowRef, "flow", "", "workflow (id or name) used by the next run")
	flags.StringVar(&featureRef, "feature", "", "feature (id or title) to assign; pass an empty value to clear the assignment")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 1 {
		return machineUsage(stdout, stderr, mode, "task edit", "task id is required")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "task edit", "create client", err)
	}

	input := flowclient.EditTaskInput{}
	if title != "" {
		input.Title = &title
	}
	if body != "" {
		input.Body = &body
	}
	if priority != "" {
		parsedPriority, err := strconv.Atoi(priority)
		if err != nil {
			fmt.Fprintf(stderr, "invalid priority: %v\n", err)
			return 2
		}
		input.Priority = &parsedPriority
	}
	if strings.TrimSpace(flowRef) != "" {
		flowID, err := resolveFlowRef(client, flowRef)
		if err != nil {
			fmt.Fprintf(stderr, "resolve flow: %v\n", err)
			return 1
		}
		input.FlowID = &flowID
	}
	// Feature assignment is tri-state: --feature absent leaves it alone, an
	// empty value clears it, anything else assigns that feature.
	featureAssigned := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "feature" {
			featureAssigned = true
		}
	})
	if featureAssigned {
		featureID := strings.TrimSpace(featureRef)
		if featureID != "" {
			resolved, err := resolveFeatureRef(client, featureID)
			if err != nil {
				fmt.Fprintf(stderr, "resolve feature: %v\n", err)
				return 1
			}
			featureID = resolved
		}
		input.FeatureID = &featureID
	}

	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	task, err := client.EditTask(taskRef, input)
	if err != nil {
		return machineError(stdout, stderr, mode, "task edit", "edit task", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "task edit", map[string]any{"task": task})
	}

	printTaskLine(stdout, task)
	return 0
}

func runFeature(args []string, stdout, stderr io.Writer) int {
	return runFeatureWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runFeatureWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printFeatureUsage(stderr)
		return 2
	}

	switch args[0] {
	case "create":
		return runFeatureCreate(options.withConfig(args[1:]), stdout, stderr)
	case "list":
		return runFeatureList(options.withConfig(args[1:]), stdout, stderr)
	case "show":
		return runFeatureShow(options.withConfig(args[1:]), stdout, stderr)
	case "edit":
		return runFeatureEdit(options.withConfig(args[1:]), stdout, stderr)
	case "rebase":
		return runFeatureRebase(options.withConfig(args[1:]), stdout, stderr)
	case "land":
		return runFeatureLand(options.withConfig(args[1:]), stdout, stderr)
	case "archive":
		return runFeatureArchive(options.withConfig(args[1:]), stdout, stderr)
	case "start":
		return runFeatureStart(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown feature command: %s\n\n", args[0])
		printFeatureUsage(stderr)
		return 2
	}
}

func runFeatureCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("feature create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	var parentItemID string
	flags.StringVar(&title, "title", "", "feature title")
	flags.StringVar(&body, "body", "", "feature body")
	flags.StringVar(&parentItemID, "parent", "", "organizational parent epic or feature id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if strings.TrimSpace(title) == "" {
		return machineUsage(stdout, stderr, mode, "feature create", "--title is required")
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "feature create", "create client", err)
	}
	feature, err := client.CreateFeature(flowclient.CreateFeatureInput{Title: title, Body: body, ParentItemID: parentItemID})
	if err != nil {
		return machineError(stdout, stderr, mode, "feature create", "create feature", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "feature create", map[string]any{"feature": feature.Feature})
	}
	printFeatureLine(stdout, feature.Feature)
	return 0
}

func runFeatureList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("feature list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var status string
	flags.StringVar(&status, "status", "", "lifecycle filter: open, landed, archived, all")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	features, err := client.ListFeatures(status)
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "feature list", "list features", err)
	}
	if apiFlags.outputMode().Machine() {
		if features == nil {
			features = []contract.FeatureResponse{}
		}
		return cliout.WriteData(stdout, apiFlags.outputMode(), "feature list", map[string]any{"features": features})
	}

	for _, feature := range features {
		printFeatureLine(stdout, feature.Feature)
	}
	return 0
}

func runFeatureShow(args []string, stdout, stderr io.Writer) int {
	parsed, featureRef, code := parseScopedTaskAPICommand(args, stderr, "feature show", 1, "usage: flow feature show [flags] FEATURE_ID")
	if code != 0 {
		return code
	}
	feature, err := parsed.client.GetFeature(featureRef)
	if err != nil {
		return machineError(stdout, stderr, parsed.mode, "feature show", "show feature", err)
	}
	if parsed.mode.Machine() {
		return cliout.WriteData(stdout, parsed.mode, "feature show", feature)
	}
	printFeatureDetail(stdout, feature)
	return 0
}

func runFeatureEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("feature edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	flags.StringVar(&title, "title", "", "new feature title")
	flags.StringVar(&body, "body", "", "new feature body")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow feature edit [flags] FEATURE_ID")
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}

	input := flowclient.UpdateFeatureInput{}
	if title != "" {
		input.Title = &title
	}
	if body != "" {
		input.Body = &body
	}

	client, featureRef := scopeClientForRef(client, flags.Arg(0))
	feature, err := client.UpdateFeature(featureRef, input)
	if err != nil {
		fmt.Fprintf(stderr, "edit feature: %v\n", err)
		return 1
	}
	printFeatureLine(stdout, feature.Feature)
	return 0
}

func runFeatureRebase(args []string, stdout, stderr io.Writer) int {
	parsed, featureRef, code := parseScopedTaskAPICommand(args, stderr, "feature rebase", 1, "usage: flow feature rebase [flags] FEATURE_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.RebaseFeature(featureRef)
	if err != nil {
		fmt.Fprintf(stderr, "rebase feature: %v\n", err)
		return 1
	}

	switch result.Result.Kind {
	case coordinator.RebaseAlreadyUpToDate:
		fmt.Fprintf(stdout, "%s\tup to date\n", featureRef)
	case coordinator.RebaseRebased:
		fmt.Fprintf(stdout, "%s\trebased\t%s\n", featureRef, result.Result.NewTipSHA)
	case coordinator.RebaseTaskCreated:
		fmt.Fprintf(stdout, "%s\trebase task\t%s\n", featureRef, result.Result.RebaseTaskID)
	}
	return 0
}

func runFeatureLand(args []string, stdout, stderr io.Writer) int {
	parsed, featureRef, code := parseScopedTaskAPICommand(args, stderr, "feature land", 1, "usage: flow feature land [flags] FEATURE_ID")
	if code != 0 {
		return code
	}
	feature, err := parsed.client.LandFeature(featureRef)
	if err != nil {
		fmt.Fprintf(stderr, "land feature: %v\n", err)
		return 1
	}
	printFeatureLine(stdout, feature.Feature)
	return 0
}

func runFeatureArchive(args []string, stdout, stderr io.Writer) int {
	parsed, featureRef, code := parseScopedTaskAPICommand(args, stderr, "feature archive", 1, "usage: flow feature archive [flags] FEATURE_ID")
	if code != 0 {
		return code
	}
	feature, err := parsed.client.ArchiveFeature(featureRef)
	if err != nil {
		fmt.Fprintf(stderr, "archive feature: %v\n", err)
		return 1
	}
	printFeatureLine(stdout, feature.Feature)
	return 0
}

func runFeatureStart(args []string, stdout, stderr io.Writer) int {
	parsed, featureRef, code := parseScopedTaskAPICommand(args, stderr, "feature start", 1, "usage: flow feature start [flags] FEATURE_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.StartFeature(featureRef)
	if err != nil {
		fmt.Fprintf(stderr, "start feature: %v\n", err)
		return 1
	}
	printContainerStart(stdout, result)
	return 0
}

func runEpic(args []string, stdout, stderr io.Writer) int {
	return runEpicWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runEpicWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printEpicUsage(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return runEpicCreate(options.withConfig(args[1:]), stdout, stderr)
	case "list":
		return runEpicList(options.withConfig(args[1:]), stdout, stderr)
	case "show":
		return runEpicShow(options.withConfig(args[1:]), stdout, stderr)
	case "edit":
		return runEpicEdit(options.withConfig(args[1:]), stdout, stderr)
	case "start", "complete", "reopen", "archive":
		return runEpicAction(args[0], options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown epic command: %s\n\n", args[0])
		printEpicUsage(stderr)
		return 2
	}
}

func runEpicCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("epic create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var title, body, policy, parent string
	var priority int
	flags.StringVar(&title, "title", "", "epic title")
	flags.StringVar(&body, "body", "", "epic body")
	flags.IntVar(&priority, "priority", 0, "epic priority")
	flags.StringVar(&policy, "completion-policy", string(coordinator.EpicAllChildren), "all_children or manual")
	flags.StringVar(&parent, "parent", "", "organizational parent epic or feature id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if strings.TrimSpace(title) == "" {
		return machineUsage(stdout, stderr, mode, "epic create", "--title is required")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "epic create", "create client", err)
	}
	response, err := client.CreateEpic(contract.CreateEpicRequest{
		Title: title, Body: body, Priority: priority, CompletionPolicy: policy, ParentItemID: parent,
	})
	if err != nil {
		return machineError(stdout, stderr, mode, "epic create", "create epic", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "epic create", map[string]any{"epic": response.Epic})
	}
	printEpicLine(stdout, response.Epic)
	return 0
}

func runEpicList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("epic list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var status string
	flags.StringVar(&status, "status", "", "open, completed, archived, or all")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	epics, err := client.ListEpics(status)
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "epic list", "list epics", err)
	}
	if apiFlags.outputMode().Machine() {
		if epics == nil {
			epics = []contract.EpicResponse{}
		}
		return cliout.WriteData(stdout, apiFlags.outputMode(), "epic list", map[string]any{"epics": epics})
	}
	for _, epic := range epics {
		printEpicLine(stdout, epic.Epic)
	}
	return 0
}

func runEpicShow(args []string, stdout, stderr io.Writer) int {
	parsed, id, code := parseScopedTaskAPICommand(args, stderr, "epic show", 1, "usage: flow epic show [flags] EPIC_ID")
	if code != 0 {
		return code
	}
	response, err := parsed.client.GetEpic(id)
	if err != nil {
		return machineError(stdout, stderr, parsed.mode, "epic show", "show epic", err)
	}
	if parsed.mode.Machine() {
		return cliout.WriteData(stdout, parsed.mode, "epic show", response)
	}
	printEpicDetail(stdout, response)
	return 0
}

func runEpicEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("epic edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var title, body, policy string
	var priority int
	flags.StringVar(&title, "title", "", "new title")
	flags.StringVar(&body, "body", "", "new body")
	flags.StringVar(&policy, "completion-policy", "", "all_children or manual")
	flags.IntVar(&priority, "priority", -1, "new priority")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 1 {
		return machineUsage(stdout, stderr, mode, "epic edit", "usage: flow epic edit [flags] EPIC_ID")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "epic edit", "create client", err)
	}
	client, id := scopeClientForRef(client, flags.Arg(0))
	input := contract.EditEpicRequest{}
	if title != "" {
		input.Title = &title
	}
	if body != "" {
		input.Body = &body
	}
	if policy != "" {
		input.CompletionPolicy = &policy
	}
	if priority >= 0 {
		input.Priority = &priority
	}
	response, err := client.UpdateEpic(id, input)
	if err != nil {
		return machineError(stdout, stderr, mode, "epic edit", "edit epic", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "epic edit", map[string]any{"epic": response.Epic})
	}
	printEpicLine(stdout, response.Epic)
	return 0
}

func runEpicAction(action string, args []string, stdout, stderr io.Writer) int {
	parsed, id, code := parseScopedTaskAPICommand(args, stderr, "epic "+action, 1, "usage: flow epic "+action+" [flags] EPIC_ID")
	if code != 0 {
		return code
	}
	if action == "start" {
		result, err := parsed.client.StartEpic(id)
		if err != nil {
			return machineError(stdout, stderr, parsed.mode, "epic start", "start epic", err)
		}
		if parsed.mode.Machine() {
			return cliout.WriteData(stdout, parsed.mode, "epic start", result)
		}
		printContainerStart(stdout, result)
		return 0
	}
	var response contract.EpicResponse
	var err error
	switch action {
	case "complete":
		response, err = parsed.client.CompleteEpic(id)
	case "reopen":
		response, err = parsed.client.ReopenEpic(id)
	case "archive":
		response, err = parsed.client.ArchiveEpic(id)
	}
	if err != nil {
		return machineError(stdout, stderr, parsed.mode, "epic "+action, action+" epic", err)
	}
	if parsed.mode.Machine() {
		return cliout.WriteData(stdout, parsed.mode, "epic "+action, map[string]any{"epic": response.Epic})
	}
	printEpicLine(stdout, response.Epic)
	return 0
}

func runWorkItem(args []string, stdout, stderr io.Writer) int {
	return runWorkItemWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runWorkItemWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printWorkItemUsage(stderr)
		return 2
	}
	switch args[0] {
	case "show", "tree", "relations":
		parsed, id, code := parseScopedTaskAPICommand(options.withConfig(args[1:]), stderr, "work-item "+args[0], 1, "usage: flow work-item "+args[0]+" [flags] ITEM_ID")
		if code != 0 {
			return code
		}
		if args[0] == "relations" {
			relations, err := parsed.client.GetWorkItemRelations(id)
			if err != nil {
				fmt.Fprintf(stderr, "list work-item relations: %v\n", err)
				return 1
			}
			printWorkItemRelations(stdout, id, relations)
			return 0
		}
		response, err := parsed.client.GetWorkItem(id, args[0] == "tree")
		if err != nil {
			fmt.Fprintf(stderr, "%s work item: %v\n", args[0], err)
			return 1
		}
		printWorkItemResponse(stdout, response, 0)
		return 0
	case "link", "unlink":
		parsed, sourceID, targetID, kind, code := parseTaskRelationCommand(options.withConfig(args[1:]), stderr, "work-item "+args[0])
		if code != 0 {
			return code
		}
		var err error
		if args[0] == "link" {
			err = parsed.client.LinkWorkItems(sourceID, kind, targetID)
		} else {
			err = parsed.client.UnlinkWorkItems(sourceID, kind, targetID)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s work items: %v\n", args[0], err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown work-item command: %s\n\n", args[0])
		printWorkItemUsage(stderr)
		return 2
	}
}

func runTaskSchedule(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task schedule", 1, "usage: flow task schedule [flags] TASK_ID")
	if code != 0 {
		return code
	}
	run, err := parsed.client.ScheduleWorkflow(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "schedule task: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", run.TaskID, run.State, run.CurrentNodeKey)
	return 0
}

func runTaskState(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task state", 2, "usage: flow task state [flags] TASK_ID triage|backlog|up_next|closed|rejected")
	if code != 0 {
		return code
	}
	task, err := parsed.client.SetTaskState(taskRef, coordinator.TaskState(parsed.flags.Arg(1)))
	if err != nil {
		fmt.Fprintf(stderr, "set task state: %v\n", err)
		return 1
	}

	printTaskLine(stdout, task)
	return 0
}

func runTaskReset(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task reset", 1, "usage: flow task reset [flags] TASK_ID")
	if code != 0 {
		return code
	}
	task, err := parsed.client.ResetTask(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "reset task: %v\n", err)
		return 1
	}

	printTaskLine(stdout, task)
	return 0
}

func runTaskDone(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task done", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var resolution string
	var note string
	var evidenceFlags stringSliceFlag
	flags.StringVar(&resolution, "resolution", string(coordinator.ResolutionCompleted), "completed|rejected|abandoned|cancelled|failed")
	flags.StringVar(&note, "note", "", "completion note")
	flags.StringVar(&note, "message", "", "substantive resolution message (alias for --note)")
	flags.Var(&evidenceFlags, "evidence", "typed completion evidence as type:value (repeatable); types: commit|test|pr|review|note")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 1 {
		return machineUsage(stdout, stderr, mode, "task done", "task id is required")
	}
	evidence, err := parseEvidenceFlags(evidenceFlags.Values)
	if err != nil {
		return machineUsage(stdout, stderr, mode, "task done", err.Error())
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "task done", "create client", err)
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	task, err := client.ForceDoneWithEvidence(taskRef, coordinator.DoneResolution(resolution), note, evidence)
	if err != nil {
		return machineError(stdout, stderr, mode, "task done", "complete task", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "task done", map[string]any{"task": task})
	}
	printTaskLine(stdout, task)
	return 0
}

// parseEvidenceFlags parses repeatable "type:value" evidence flags.
func parseEvidenceFlags(values []string) ([]coordinator.Evidence, error) {
	if len(values) == 0 {
		return nil, nil
	}
	evidence := make([]coordinator.Evidence, 0, len(values))
	for _, raw := range values {
		typeName, value, found := strings.Cut(raw, ":")
		if !found || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid --evidence %q: want type:value (e.g. commit:abc123)", raw)
		}
		evidence = append(evidence, coordinator.Evidence{Type: coordinator.EvidenceType(typeName), Value: value})
	}
	return evidence, nil
}

// runAuditWithGlobalOptions routes `flow audit <subcommand>`, threading the
// global output flags down like runTaskWithGlobalOptions does.
func runAuditWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return machineUsage(stdout, stderr, cliout.ModeHuman, "audit", "usage: flow audit completions [--resolution R] [--limit N]")
	}
	switch args[0] {
	case "completions":
		return runAuditCompletions(options.withConfig(args[1:]), stdout, stderr)
	default:
		return machineUsage(stdout, stderr, cliout.ModeHuman, "audit", "usage: flow audit completions [--resolution R] [--limit N]")
	}
}

func runAuditCompletions(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("audit completions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var resolution string
	var limit int
	flags.StringVar(&resolution, "resolution", "", "filter by resolution: completed|merged|rejected|abandoned|cancelled|failed")
	flags.IntVar(&limit, "limit", 100, "maximum completions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 0 {
		return machineUsage(stdout, stderr, mode, "audit completions", "usage: flow audit completions [--resolution R] [--limit N]")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "audit completions", "create client", err)
	}
	tasks, err := client.ListCompletions(resolution, limit)
	if err != nil {
		return machineError(stdout, stderr, mode, "audit completions", "list completions", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "audit completions", map[string]any{"completions": tasks})
	}
	for _, task := range tasks {
		printCompletionLine(stdout, task)
	}
	return 0
}

func printCompletionLine(out io.Writer, task coordinator.Task) {
	resolution := ""
	if task.DoneResolution != nil {
		resolution = string(*task.DoneResolution)
	}
	doneAt := ""
	if task.DoneAt != nil {
		doneAt = task.DoneAt.Format("2006-01-02T15:04:05Z")
	}
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\tevidence=%d\t%s\n", task.ID, resolution, task.CreatedBy, doneAt, len(task.DoneEvidence), task.Title)
	if task.DoneMessage != "" {
		fmt.Fprintf(out, "  message: %s\n", task.DoneMessage)
	}
	for _, ev := range task.DoneEvidence {
		fmt.Fprintf(out, "  evidence: %s: %s\n", ev.Type, ev.Value)
	}
}

// runMCPWithGlobalOptions routes `flow mcp <subcommand>`.
func runMCPWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		return machineUsage(stdout, stderr, cliout.ModeHuman, "mcp", "usage: flow mcp serve [--project P]")
	}
	return runMCPServe(options.withConfig(args[1:]), stdout, stderr)
}

// runMCPServe runs the MCP read server over stdio against the resolved
// flow-server. Nothing but MCP protocol goes to stdout; logs go to stderr.
func runMCPServe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return machineUsage(stdout, stderr, apiFlags.outputMode(), "mcp serve", "usage: flow mcp serve [--project P]")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "mcp serve", "create client", err)
	}
	server := flowmcp.NewServer(client)
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		fmt.Fprintf(stderr, "mcp serve: %v\n", err)
		return 1
	}
	return 0
}

func runTaskReopen(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task reopen", 1, "usage: flow task reopen [flags] TASK_ID")
	if code != 0 {
		return code
	}
	task, err := parsed.client.ReopenTask(taskRef)
	if err != nil {
		return machineError(stdout, stderr, parsed.mode, "task reopen", "reopen task", err)
	}
	if parsed.mode.Machine() {
		return cliout.WriteData(stdout, parsed.mode, "task reopen", map[string]any{"task": task})
	}
	printTaskLine(stdout, task)
	return 0
}

func runTaskWorkflow(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task workflow", 1, "usage: flow task workflow [flags] TASK_ID")
	if code != 0 {
		return code
	}
	detail, err := parsed.client.GetWorkflow(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "get workflow: %v\n", err)
		return 1
	}
	encoded, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "encode workflow: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

func runTaskRespond(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task respond", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var nodeRunID string
	var reviewWaitID string
	var outcome string
	var feedback string
	flags.StringVar(&nodeRunID, "node-run", "", "waiting node run id")
	flags.StringVar(&reviewWaitID, "review-wait", "", "exact open human-gate wait id")
	flags.StringVar(&outcome, "outcome", "", "human-gate outcome")
	flags.StringVar(&feedback, "feedback", "", "feedback for the next node")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || strings.TrimSpace(nodeRunID) == "" || strings.TrimSpace(reviewWaitID) == "" || strings.TrimSpace(outcome) == "" {
		fmt.Fprintln(stderr, "usage: flow task respond [flags] TASK_ID --node-run NODE_RUN_ID --review-wait REVIEW_WAIT_ID --outcome OUTCOME [--feedback TEXT]")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	result, err := client.RespondWorkflow(taskRef, nodeRunID, reviewWaitID, outcome, feedback)
	if err != nil {
		fmt.Fprintf(stderr, "respond to workflow: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", result.Run.TaskID, result.Run.State, result.Run.CurrentNodeKey)
	return 0
}

func runTaskBudget(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task budget", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var additional int
	flags.IntVar(&additional, "additional", 0, "additional transitions or review-author cycles for the active budget wait")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || additional < 1 {
		fmt.Fprintln(stderr, "usage: flow task budget [flags] TASK_ID --additional N")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	run, err := client.ExtendWorkflowBudget(taskRef, additional)
	if err != nil {
		fmt.Fprintf(stderr, "extend workflow budget: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\ttransitions %d/%d\treview cycles %d/%d\n",
		run.TaskID,
		run.TransitionsUsed,
		run.TransitionBudget,
		run.ReviewCyclesUsed,
		run.ReviewCycleBudget,
	)
	return 0
}

func runTaskRetry(args []string, stdout, stderr io.Writer) int {
	flags, apiFlags := newAPIFlagSet("task retry", stderr)
	var refreshAgentRuntime bool
	flags.BoolVar(&refreshAgentRuntime, "refresh-agent-runtime", false, "refresh the current node's harness, model, and reasoning effort before retrying")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow task retry [flags] TASK_ID")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	run, err := client.RetryWorkflow(taskRef, refreshAgentRuntime)
	if err != nil {
		fmt.Fprintf(stderr, "retry workflow: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", run.TaskID, run.State, run.CurrentNodeKey)
	return 0
}

func runTaskClose(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task close", 1, "task id is required")
	if code != 0 {
		return code
	}
	task, err := parsed.client.CloseTask(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "close task: %v\n", err)
		return 1
	}

	printTaskLine(stdout, task)
	return 0
}

func runTaskTriage(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "task triage", 2, "usage: flow task triage [flags] TASK_ID accepted|rejected")
	if code != 0 {
		return code
	}
	task, err := parsed.client.TriageTask(taskRef, coordinator.TriageState(parsed.flags.Arg(1)))
	if err != nil {
		fmt.Fprintf(stderr, "triage task: %v\n", err)
		return 1
	}

	printTaskLine(stdout, task)
	return 0
}

func runTaskLink(args []string, stdout, stderr io.Writer) int {
	parsed, sourceRef, targetRef, kind, code := parseTaskRelationCommand(args, stderr, "task link")
	if code != 0 {
		return code
	}
	if err := parsed.client.LinkTasks(sourceRef, kind, targetRef); err != nil {
		fmt.Fprintf(stderr, "link tasks: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", sourceRef, kind, targetRef)
	return 0
}

func runTaskUnlink(args []string, stdout, stderr io.Writer) int {
	parsed, sourceRef, targetRef, kind, code := parseTaskRelationCommand(args, stderr, "task unlink")
	if code != 0 {
		return code
	}
	if err := parsed.client.UnlinkTasks(sourceRef, kind, targetRef); err != nil {
		fmt.Fprintf(stderr, "unlink tasks: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", sourceRef, kind, targetRef)
	return 0
}

func runTaskRelations(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("task relations", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow task relations [flags] TASK_ID")
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, taskRef := scopeClientForRef(client, flags.Arg(0))
	relations, err := client.GetTaskRelations(taskRef)
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "task relations", "list relations", err)
	}
	if apiFlags.outputMode().Machine() {
		if relations == nil {
			relations = []coordinator.TaskRelation{}
		}
		return cliout.WriteData(stdout, apiFlags.outputMode(), "task relations", map[string]any{"relations": relations})
	}

	printTaskRelations(stdout, taskRef, relations)
	return 0
}

// runSearch implements `flow search`: full-text (or substring, when the
// server lacks FTS5) task search. Human output reuses the task line format.
func runSearch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var limit int
	flags.IntVar(&limit, "limit", 0, "maximum hits (server default 50)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 1 {
		return machineUsage(stdout, stderr, mode, "search", "usage: flow search QUERY [--limit N]")
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "search", "create client", err)
	}
	tasks, err := client.SearchTasks(flags.Arg(0), limit)
	if err != nil {
		return machineError(stdout, stderr, mode, "search", "search tasks", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "search", map[string]any{"tasks": tasks})
	}
	for _, task := range tasks {
		printTaskLine(stdout, task)
	}
	return 0
}

// runVersion implements `flow version`: the binary version plus the machine
// contracts it speaks. Machine modes expose agent_format and protocol so an
// agent can confirm compatibility before parsing further output.
func runVersion(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	info := version.Current()
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "version", map[string]any{
			"version":      info.Version,
			"commit":       info.Commit,
			"date":         info.Date,
			"agent_format": cliout.ContractVersion,
			"protocol":     contract.ProtocolVersion,
		})
	}
	fmt.Fprintf(stdout, "flow %s\n", info)
	return 0
}

// runEvents implements `flow events`: one page of the project event log, or
// a live tail with --follow (server-sent events). Machine output is the page
// plus a resumable next_since cursor; --follow emits one event JSON per line.
func runEvents(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var since int64
	var limit int
	var follow bool
	var tail bool
	var kindFilter string
	var taskFilter string
	var actorFilter string
	flags.Int64Var(&since, "since", 0, "only events with seq greater than N")
	flags.IntVar(&limit, "limit", 100, "maximum events per fetch")
	flags.BoolVar(&follow, "follow", false, "stream new events as they happen")
	flags.BoolVar(&tail, "tail", false, "alias for --follow")
	flags.StringVar(&kindFilter, "kind", "", "only events of this kind (e.g. task.created)")
	flags.StringVar(&taskFilter, "task", "", "only events for this task id")
	flags.StringVar(&actorFilter, "actor", "", "only events by this actor")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 0 {
		return machineUsage(stdout, stderr, mode, "events", "usage: flow events [--since N] [--limit N] [--kind K] [--task ID] [--actor A] [--follow]")
	}
	filter := coordinator.EventFilter{Kind: kindFilter, TaskID: taskFilter, Actor: actorFilter}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "events", "create client", err)
	}
	if follow || tail {
		err := client.StreamEvents(context.Background(), since, func(event coordinator.Event) error {
			if mode.Machine() {
				encoded, err := json.Marshal(event)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(stdout, string(encoded))
				return err
			}
			printEventLines(stdout, []coordinator.Event{event})
			return nil
		})
		if err != nil {
			return machineError(stdout, stderr, mode, "events", "stream events", err)
		}
		return 0
	}
	events, next, err := client.ListEvents(since, limit, filter)
	if err != nil {
		return machineError(stdout, stderr, mode, "events", "list events", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "events", map[string]any{"events": events, "next_since": next})
	}
	printEventLines(stdout, events)
	return 0
}

func printEventLines(stdout io.Writer, events []coordinator.Event) {
	for _, event := range events {
		payload := "{}"
		if len(event.Payload) > 0 {
			if encoded, err := json.Marshal(event.Payload); err == nil {
				payload = string(encoded)
			}
		}
		task := event.TaskID
		if task == "" {
			task = "-"
		}
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", event.Seq, event.Kind, task, payload)
	}
}

func runBoard(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("board", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	board, err := client.Board()
	if err != nil {
		return machineError(stdout, stderr, apiFlags.outputMode(), "board", "load board", err)
	}
	if apiFlags.outputMode().Machine() {
		return cliout.WriteData(stdout, apiFlags.outputMode(), "board", map[string]any{"board": board})
	}

	printBoard(stdout, board)
	return 0
}

func runReadyTasks(args []string, stdout, stderr io.Writer) int {
	tasks, mode, code := fetchReadyTasks(args, stdout, stderr, "ready")
	if code != 0 {
		return code
	}
	if mode.Machine() {
		if tasks == nil {
			tasks = []coordinator.Task{}
		}
		return cliout.WriteData(stdout, mode, "ready", map[string]any{"tasks": tasks})
	}
	for _, task := range tasks {
		printTaskLine(stdout, task)
	}
	return 0
}

func runNextTask(args []string, stdout, stderr io.Writer) int {
	tasks, mode, code := fetchReadyTasks(args, stdout, stderr, "next")
	if code != 0 {
		return code
	}
	if mode.Machine() {
		var next *coordinator.Task
		if len(tasks) > 0 {
			next = &tasks[0]
		}
		return cliout.WriteData(stdout, mode, "next", map[string]any{"task": next})
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "no ready tasks")
		return 0
	}
	printTaskLine(stdout, tasks[0])
	return 0
}

// fetchReadyTasks lists the tasks an agent could start right now: unscheduled
// with no unresolved blockers, ordered by priority then creation time.
func fetchReadyTasks(args []string, stdout, stderr io.Writer, command string) ([]coordinator.Task, cliout.Mode, int) {
	flags := flag.NewFlagSet("ready", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var tag string
	flags.StringVar(&tag, "tag", "", "comma-separated tag slugs")
	if err := flags.Parse(args); err != nil {
		return nil, cliout.ModeHuman, 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() != 0 {
		return nil, mode, machineUsage(stdout, stderr, mode, command, "usage: flow ready|next [--tag TAG]")
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return nil, mode, machineError(stdout, stderr, mode, command, "create client", err)
	}
	tasks, err := client.ListTasks(flowclient.TaskFilter{Ready: true, TagSlugs: parseCSV(tag)})
	if err != nil {
		return nil, mode, machineError(stdout, stderr, mode, command, "list ready tasks", err)
	}
	return tasks, mode, 0
}

// runWait polls tasks until they reach the requested condition. Exit codes:
// 0 condition met, 1 error, 2 usage, 3 timeout. Durations must carry units
// (time.ParseDuration), and --until matches current states: a task that reset
// to unscheduled never satisfies "done".
func runWait(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var until string
	var timeoutText string
	var intervalText string
	var anyMode bool
	var allMode bool
	flags.StringVar(&until, "until", "done", "condition to wait for: done|blocked|scheduled|in_progress")
	flags.StringVar(&timeoutText, "timeout", "30s", "overall timeout (Go duration with units, e.g. 30s, 5m)")
	flags.StringVar(&intervalText, "poll-interval", "2s", "poll interval (Go duration with units)")
	flags.BoolVar(&anyMode, "any", false, "succeed when any task matches")
	flags.BoolVar(&allMode, "all", false, "succeed when every task matches (default)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	if flags.NArg() == 0 {
		return machineUsage(stdout, stderr, mode, "wait", "usage: flow wait [flags] TASK_ID...")
	}
	if anyMode && allMode {
		return machineUsage(stdout, stderr, mode, "wait", "--any and --all conflict")
	}
	switch until {
	case "done", "blocked", "scheduled", "in_progress":
	default:
		return machineUsage(stdout, stderr, mode, "wait", fmt.Sprintf("invalid --until %q: want done|blocked|scheduled|in_progress", until))
	}
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 {
		return machineUsage(stdout, stderr, mode, "wait", fmt.Sprintf("invalid --timeout %q: use a Go duration with units (e.g. 30s, 5m)", timeoutText))
	}
	interval, err := time.ParseDuration(intervalText)
	if err != nil || interval <= 0 {
		return machineUsage(stdout, stderr, mode, "wait", fmt.Sprintf("invalid --poll-interval %q: use a Go duration with units (e.g. 2s, 500ms)", intervalText))
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "wait", "create client", err)
	}
	type waitTarget struct {
		client *flowclient.Client
		id     string
	}
	targets := make([]waitTarget, 0, flags.NArg())
	for _, arg := range flags.Args() {
		scoped, ref := scopeClientForRef(client, arg)
		targets = append(targets, waitTarget{client: scoped, id: ref})
	}

	matches := func(view flowclient.TaskView) bool {
		switch until {
		case "blocked":
			return view.Blocked
		case "done":
			return view.Task.State != nil && *view.Task.State == coordinator.LifecycleDone
		case "scheduled":
			return view.Task.State != nil && *view.Task.State == coordinator.LifecycleScheduled
		case "in_progress":
			return view.Task.State != nil && *view.Task.State == coordinator.LifecycleInProgress
		}
		return false
	}

	deadline := time.Now().Add(timeout)
	for {
		matched := make([]coordinator.Task, 0, len(targets))
		for _, target := range targets {
			view, err := target.client.GetTaskView(target.id)
			if err != nil {
				return machineError(stdout, stderr, mode, "wait", "get task "+target.id, err)
			}
			if matches(view) {
				matched = append(matched, view.Task)
			}
		}
		if (anyMode && len(matched) > 0) || (!anyMode && len(matched) == len(targets)) {
			if mode.Machine() {
				return cliout.WriteData(stdout, mode, "wait", map[string]any{"tasks": matched, "until": until})
			}
			for _, task := range matched {
				printTaskLine(stdout, task)
			}
			return 0
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			message := fmt.Sprintf("timed out after %s waiting for %s (%d/%d tasks matched)", timeout, until, len(matched), len(targets))
			if mode.Machine() {
				return cliout.WriteFailure(stdout, mode, "wait", "wait_timeout", message, 3)
			}
			fmt.Fprintf(stderr, "wait: %s\n", message)
			return 3
		}
		if interval < remaining {
			time.Sleep(interval)
		} else {
			time.Sleep(remaining)
		}
	}
}

func runUI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ui does not accept positional arguments")
		return 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	bootstrap, err := client.CreateWebBootstrap()
	if err != nil {
		fmt.Fprintf(stderr, "create ui login URL: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\n", client.URLForPath(bootstrap.LoginPath))
	return 0
}

func runChecks(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "checks", 1, "usage: flow checks [flags] TASK_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.ListChecks(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "list checks: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "review_state: %s\n", result.ReviewState)
	for _, check := range result.Checks {
		printCheckLine(stdout, check)
	}
	return 0
}

func runTransitions(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "transitions", 1, "usage: flow transitions [flags] TASK_ID")
	if code != 0 {
		return code
	}
	transitions, err := parsed.client.ListTransitions(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "list transitions: %v\n", err)
		return 1
	}

	for _, entry := range transitions {
		printTransitionLine(stdout, entry)
	}
	return 0
}

func printTransitionLine(out io.Writer, entry coordinator.WorkflowTransition) {
	from := entry.FromTaskState
	if from == "" {
		from = "-"
	}
	to := entry.ToTaskState
	if entry.FromNodeKey != "" || entry.ToNodeKey != "" {
		from = entry.FromNodeKey
		to = entry.ToNodeKey
	}
	actor := entry.Actor
	if actor == "" {
		actor = "-"
	}
	fmt.Fprintf(out, "%d\t%s\t%s -> %s\toutcome=%s\tactor=%s\t%s\n",
		entry.Sequence,
		entry.EventKind,
		from,
		to,
		entry.Outcome,
		actor,
		entry.CreatedAt.Format(time.RFC3339),
	)
}

func runReview(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printReviewUsage(stderr)
		return 2
	}

	switch args[0] {
	case "run":
		return runReviewRun(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown review command: %s\n\n", args[0])
		printReviewUsage(stderr)
		return 2
	}
}

func runReviewRun(args []string, stdout, stderr io.Writer) int {
	parsed, taskRef, code := parseScopedTaskAPICommand(args, stderr, "review run", 1, "usage: flow review run [flags] TASK_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.RunReview(taskRef)
	if err != nil {
		fmt.Fprintf(stderr, "run review: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "change: %s\n", result.Change.ID)
	fmt.Fprintf(stdout, "checks_created: %d\n", result.Scheduled.ChecksCreated)
	fmt.Fprintf(stdout, "jobs_enqueued: %d\n", result.Scheduled.JobsEnqueued)
	fmt.Fprintf(stdout, "review_state: %s\n", result.ReviewState)
	for _, check := range result.Checks {
		printCheckLine(stdout, check)
	}
	return 0
}

func runWorkers(args []string, stdout, stderr io.Writer) int {
	parsed, code := parseAPICommand(args, stderr, "workers", 0, "workers does not accept positional arguments")
	if code != 0 {
		return code
	}
	workers, err := parsed.client.ListWorkers()
	if err != nil {
		fmt.Fprintf(stderr, "list workers: %v\n", err)
		return 1
	}

	for _, worker := range workers {
		printWorkerLine(stdout, worker)
	}
	return 0
}

func runJobs(args []string, stdout, stderr io.Writer) int {
	parsed, code := parseAPICommand(args, stderr, "jobs", 0, "jobs does not accept positional arguments")
	if code != 0 {
		return code
	}
	jobs, err := parsed.client.ListJobs()
	if err != nil {
		fmt.Fprintf(stderr, "list jobs: %v\n", err)
		return 1
	}

	for _, job := range jobs {
		printJobLine(stdout, job)
	}
	return 0
}

func runAttach(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("attach", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var printCommand bool
	var web bool
	var job bool
	flags.BoolVar(&printCommand, "print-command", false, "print the tmux attach command or terminal URL instead of executing it")
	flags.BoolVar(&web, "web", false, "print the coordinator terminal proxy URL")
	flags.BoolVar(&job, "job", false, "attach to a live worker job instead of an author session")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow attach [flags] [--job] SESSION_ID|JOB_ID")
		return 2
	}
	if job && web {
		fmt.Fprintln(stderr, "flow attach --web is only available for author sessions")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	var info terminal.AttachInfo
	if job {
		info, err = client.JobAttach(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "attach job: %v\n", err)
			return 1
		}
	} else {
		info, err = client.SessionAttach(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "attach session: %v\n", err)
			return 1
		}
	}
	if web {
		access, err := client.CreateSessionTerminalAccess(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "create terminal access URL: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", client.URLForPath(access.LoginPath))
		return 0
	}
	commandArgs := info.Command
	if printCommand {
		fmt.Fprintf(stdout, "%s\n", strings.Join(commandArgs, " "))
		return 0
	}
	if len(commandArgs) == 0 {
		fmt.Fprintln(stderr, "attach session: empty attach command")
		return 1
	}

	command := exec.Command(commandArgs[0], commandArgs[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		fmt.Fprintf(stderr, "run attach command: %v\n", err)
		return 1
	}

	return 0
}

func runSession(args []string, stdout, stderr io.Writer) int {
	return runSessionWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runSessionWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printSessionUsage(stderr)
		return 2
	}

	switch args[0] {
	case "event":
		return runSessionEvent(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown session command: %s\n\n", args[0])
		printSessionUsage(stderr)
		return 2
	}
}

func runSessionEvent(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("session event", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var sessionID string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow session event [flags] working|waiting")
		return 2
	}
	state := coordinator.SessionRuntimeState(strings.TrimSpace(flags.Arg(0)))
	switch state {
	case coordinator.SessionWorking, coordinator.SessionWaiting:
	default:
		fmt.Fprintln(stderr, "session event state must be working or waiting")
		return 2
	}

	applySessionEnvironment(apiFlags, &sessionID)
	if sessionID == "" {
		fmt.Fprintln(stderr, "session id is required")
		return 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	session, err := client.UpdateSessionState(sessionID, state)
	if err != nil {
		fmt.Fprintf(stderr, "update session event: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\t%s\t%s\n", session.ID, session.RuntimeState, session.ChangeID)
	return 0
}

func runHook(args []string, stdout, stderr io.Writer) int {
	return runHookWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runHookWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHookUsage(stderr)
		return 2
	}

	switch args[0] {
	case harness.Harness:
		if len(args) > 1 {
			switch args[1] {
			case "ingest":
				return runHookIngest(args[0], options.withConfig(args[2:]), stdout, stderr)
			case "prepush":
				return runHookPrepush(args[0], options.withConfig(args[2:]), stdout, stderr)
			case "commit-msg":
				return runHookCommitMsg(args[0], options.withConfig(args[2:]), stdout, stderr)
			}
		}
		return runHookEvent(args[0], options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown hook tool: %s\n\n", args[0])
		printHookUsage(stderr)
		return 2
	}
}

func runHookEvent(tool string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hook "+tool, flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var sessionID string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: flow hook %s [flags] EVENT\n", tool)
		return 2
	}
	state, err := harness.StateForHook(tool, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "hook event: %v\n", err)
		return 2
	}

	applySessionEnvironment(apiFlags, &sessionID)
	if sessionID == "" {
		fmt.Fprintln(stderr, "session id is required")
		return 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	session, err := client.UpdateSessionState(sessionID, coordinator.SessionRuntimeState(state))
	if err != nil {
		fmt.Fprintf(stderr, "update hook event: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\t%s\t%s\t%s:%s\n", session.ID, session.RuntimeState, session.ChangeID, tool, strings.TrimSpace(flags.Arg(0)))
	return 0
}

func runHookIngest(tool string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hook "+tool+" ingest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var sessionID string
	var strict bool
	var explicitEvent string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	flags.BoolVar(&strict, "strict", false, "fail on parse, environment, or coordinator errors")
	flags.StringVar(&explicitEvent, "event", "", "hook event fallback")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: flow hook %s ingest [--strict]\n", tool)
		return 2
	}

	payload, readErr := io.ReadAll(os.Stdin)
	fmt.Fprintln(stdout, "{}")
	if readErr != nil {
		if strict {
			fmt.Fprintf(stderr, "read hook payload: %v\n", readErr)
			return 1
		}
		return 0
	}
	signal, err := harness.ParseNativeHook(harness.NativeHookInput{
		Harness:       tool,
		RawJSON:       payload,
		ExplicitEvent: explicitEvent,
	})
	if err != nil {
		if strict {
			fmt.Fprintf(stderr, "parse hook payload: %v\n", err)
			return 2
		}
		return 0
	}

	applySessionEnvironment(apiFlags, &sessionID)
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(apiFlags.token) == "" {
		if strict {
			fmt.Fprintln(stderr, "FLOW_SESSION_ID and FLOW_SESSION_TOKEN are required")
			return 2
		}
		return 0
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		if strict {
			fmt.Fprintf(stderr, "create client: %v\n", err)
			return 1
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.ReportSessionSignal(ctx, sessionID, flowclient.SessionSignalInput{
		Signal:        coordinator.SessionSignalKind(signal.Signal),
		Source:        coordinator.SessionEventSourceNativeHook,
		Harness:       signal.Harness,
		HookEventName: signal.HookEventName,
		Details:       signal.Details,
	})
	if err != nil {
		if strict {
			fmt.Fprintf(stderr, "report hook signal: %v\n", err)
			return 1
		}
		return 0
	}

	return 0
}

// runHookPrepush backs the client-side pre-push hook. It captures the agent's
// push context to the coordinator and surfaces unresolved review threads as
// terminal steering. It is deliberately non-blocking: like the native-hook
// ingest path, it NEVER returns nonzero, so a flow or coordinator error can
// never reject the agent's push. A push is not "done"; `flow ready` remains the
// authoritative finalize and this only complements it.
func runHookPrepush(tool string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hook "+tool+" prepush", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apiFlags := addAPIFlags(flags)
	var sessionID string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	// git passes "<remote> <url>" positionals; ignore parse errors so a hook
	// invocation shape we don't recognize still exits cleanly.
	_ = flags.Parse(args)

	// Drain the ref-update lines git writes to stdin so the push pipe never
	// blocks, even though HEAD is read from git directly below.
	_, _ = io.ReadAll(os.Stdin)

	applySessionEnvironment(apiFlags, &sessionID)
	captureAndSteerPrepush(tool, apiFlags, sessionID, stderr)
	return 0
}

func captureAndSteerPrepush(tool string, apiFlags *apiFlagValues, sessionID string, stderr io.Writer) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.TrimSpace(apiFlags.token) == "" {
		// Not inside a flow author/console session; nothing to capture or steer.
		return
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// CAPTURE: record the push as agent activity, enriched with the commit
	// context the server-side post-receive can't see (it only sees SHAs).
	if sha, shaErr := currentGitSHA(); shaErr == nil {
		details := "pre-push HEAD " + sha
		if subject := firstLine(currentGitMessageOrEmpty()); subject != "" {
			details += ": " + subject
		}
		_, _ = client.ReportSessionSignal(ctx, sessionID, flowclient.SessionSignalInput{
			Signal:        coordinator.SessionSignalActivity,
			Source:        coordinator.SessionEventSourceNativeHook,
			Harness:       tool,
			HookEventName: "pre-push",
			Details:       details,
		})
	}

	// STEER: surface unresolved review threads so the agent addresses or claims
	// them before `flow ready`. Absence of a change (or any error) is fine.
	changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID"))
	if changeID == "" {
		return
	}
	threads, err := client.ListThreads(changeID, strings.TrimSpace(os.Getenv("FLOW_LEASE_ID")))
	if err != nil {
		return
	}
	if unresolved := countUnresolvedThreads(threads); unresolved > 0 {
		fmt.Fprintf(stderr, "flow: %d unresolved review thread(s) on this change — address or claim them before `flow ready`.\n", unresolved)
	}
}

// runHookCommitMsg backs the client-side commit-msg hook. It records reliable
// `Resolves:` trailers for review threads the author has already claimed but not
// yet tied to a commit, so claimResolvedTrailers (run by `flow ready`) has them.
// It is conservative: it only appends trailers, never blocks a commit, and
// always exits 0.
func runHookCommitMsg(tool string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hook "+tool+" commit-msg", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apiFlags := addAPIFlags(flags)
	var sessionID string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	_ = flags.Parse(args)

	msgPath := strings.TrimSpace(flags.Arg(0))
	if msgPath == "" {
		return 0
	}
	applySessionEnvironment(apiFlags, &sessionID)
	injectResolvesTrailers(apiFlags, sessionID, msgPath, stderr)
	return 0
}

func injectResolvesTrailers(apiFlags *apiFlagValues, sessionID string, msgPath string, stderr io.Writer) {
	changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID"))
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(apiFlags.token) == "" || changeID == "" {
		// Not a flow author session: leave the commit message untouched so
		// normal, non-flow git operations behave exactly as they would unhooked.
		return
	}
	raw, err := os.ReadFile(msgPath)
	if err != nil {
		return
	}
	message := string(raw)

	existing, err := flowgit.ResolveThreadIDsFromMessage(context.Background(), message)
	if err != nil {
		return
	}
	referenced := map[string]bool{}
	for _, id := range existing {
		referenced[id] = true
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		return
	}
	threads, err := client.ListThreads(changeID, strings.TrimSpace(os.Getenv("FLOW_LEASE_ID")))
	if err != nil {
		return
	}

	var toAdd []string
	for _, thread := range threads {
		// Only threads the author has explicitly claimed and not yet tied to a
		// commit are unambiguous to record on this commit. Threads already
		// carrying a claim commit, or referenced in the message, are skipped.
		if thread.State != coordinator.ThreadClaimed {
			continue
		}
		if thread.ClaimCommitSHA != nil && strings.TrimSpace(*thread.ClaimCommitSHA) != "" {
			continue
		}
		if referenced[thread.ID] {
			continue
		}
		referenced[thread.ID] = true
		toAdd = append(toAdd, thread.ID)
	}
	if len(toAdd) == 0 {
		return
	}

	updated := appendResolvesTrailers(message, toAdd)
	if updated == message {
		return
	}
	if err := os.WriteFile(msgPath, []byte(updated), 0o644); err != nil {
		return
	}
	fmt.Fprintf(stderr, "flow: recorded %d Resolves: trailer(s) for claimed review thread(s).\n", len(toAdd))
}

// appendResolvesTrailers uses git interpret-trailers (the same tool that parses
// them) to place the trailers in the message's trailer block. On any error it
// returns the message unchanged so a commit is never blocked or corrupted.
func appendResolvesTrailers(message string, ids []string) string {
	gitArgs := []string{"interpret-trailers"}
	for _, id := range ids {
		gitArgs = append(gitArgs, "--trailer", "Resolves: "+id)
	}
	cmd := exec.Command("git", gitArgs...)
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		return message
	}
	return string(out)
}

func countUnresolvedThreads(threads []coordinator.ReviewThread) int {
	count := 0
	for _, thread := range threads {
		if thread.State == coordinator.ThreadOpen || thread.State == coordinator.ThreadReopened {
			count++
		}
	}
	return count
}

func currentGitMessageOrEmpty() string {
	message, err := currentGitMessage()
	if err != nil {
		return ""
	}
	return message
}

func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		return strings.TrimSpace(trimmed[:idx])
	}
	return trimmed
}

func runFetchPrompt(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fetch-prompt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var role string
	var harness string
	flags.StringVar(&role, "role", "", "worker role: author, reviewer, or verifier")
	flags.StringVar(&harness, "harness", "", "prompt harness: harness or agents")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "fetch-prompt does not accept positional arguments")
		return 2
	}

	input := flowprompt.InputFromEnvironment(os.Getenv)
	if strings.TrimSpace(role) != "" {
		input.Role = role
	}
	harness = promptHarness(harness, os.Getenv)
	if err := harnesspkgValidatePrompt(harness); err != nil {
		fmt.Fprintf(stderr, "fetch prompt: %v\n", err)
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	if err := enrichPromptTaskContext(&input, apiFlags); err != nil {
		fmt.Fprintf(stderr, "fetch prompt: %v\n", err)
		return 1
	}
	rendered, err := flowprompt.Build(input)
	if err != nil {
		fmt.Fprintf(stderr, "fetch prompt: %v\n", err)
		return 2
	}

	fmt.Fprintln(stdout, rendered)
	return 0
}

func enrichPromptTaskContext(input *flowprompt.Input, apiFlags *apiFlagValues) error {
	taskID := strings.TrimSpace(input.TaskID)
	if taskID == "" || !promptTaskFetchConfigured(apiFlags) {
		return nil
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		return fmt.Errorf("create client for task context: %w", err)
	}
	task, statusLog, err := client.GetTaskWithStatus(taskID)
	if err != nil {
		return fmt.Errorf("fetch task context: %w", err)
	}

	input.TaskTitle = task.Title
	input.TaskBody = task.Body
	input.HumanAttentionContext = humanAttentionPromptContext(statusLog)
	// Resolve the flow prompt context: for authors, the current graph node's
	// role instructions (from the frozen agent-def snapshot), human gate feedback,
	// and the completed preceding-node handoffs; for reviewer/verifier check
	// jobs, the review agent def running under this check name. Task-bound
	// context is mandatory so no role can execute without current owner rulings.
	checkName := ""
	if role := promptInputRole(*input); role == flowprompt.RoleReviewer || role == flowprompt.RoleVerifier {
		checkName = strings.TrimSpace(input.CheckName)
	}
	var priorNodeHandoffs string
	if promptContext, err := client.GetPromptContext(taskID, checkName); err != nil {
		return fmt.Errorf("fetch workflow prompt context: %w", err)
	} else {
		input.RoleInstructionsOverride = promptContext.RoleInstructions
		input.OwnerRulings = make([]flowprompt.OwnerRuling, 0, len(promptContext.OwnerRulings))
		for _, ruling := range promptContext.OwnerRulings {
			input.OwnerRulings = append(input.OwnerRulings, flowprompt.OwnerRuling{
				ID: ruling.RulingID, Body: ruling.Body, Source: string(ruling.Source), SupersedesID: ruling.SupersedesID,
			})
		}
		if promptInputRole(*input) == flowprompt.RoleAuthor {
			input.PhaseName = promptContext.PhaseName
			input.GateFeedback = promptContext.GateFeedback
			if promptContext.WorkspaceMode != "" {
				input.WorkspaceMode = string(promptContext.WorkspaceMode)
			}
			if promptContext.ArtifactKind != "" {
				input.ArtifactKind = string(promptContext.ArtifactKind)
			}
			input.TaskSetWorkflow = taskSetWorkflowPromptContract(promptContext.TaskSetWorkflow)
		}
		priorNodeHandoffs = renderPriorNodeHandoffs(promptContext.PriorHandoffs)
	}
	input.PriorHandoff = priorNodeHandoffs
	if promptInputRole(*input) == flowprompt.RoleAuthor {
		if err := enrichPromptAuthorReviewContext(input, client); err != nil {
			return err
		}
	}
	if promptInputRole(*input) == flowprompt.RoleReviewer {
		enrichPromptReviewerCheckContext(input, client)
	}
	return nil
}

func taskSetWorkflowPromptContract(contract *coordinator.TaskSetWorkflowContract) *flowprompt.TaskSetWorkflowContract {
	if contract == nil {
		return nil
	}
	result := &flowprompt.TaskSetWorkflowContract{
		DefaultChildFlowID:     contract.DefaultChildFlowID,
		AllowChildFlowOverride: contract.AllowChildFlowOverride,
		MaxItems:               contract.MaxItems,
		AvailableFlows:         make([]flowprompt.TaskSetFlowOption, 0, len(contract.AvailableFlows)),
	}
	for _, flow := range contract.AvailableFlows {
		result.AvailableFlows = append(result.AvailableFlows, flowprompt.TaskSetFlowOption{
			ID: flow.ID, Name: flow.Name, Description: flow.Description,
		})
	}
	return result
}

// renderPriorNodeHandoffs formats completed graph-node handoffs as labeled
// sections for prompt injection.
func renderPriorNodeHandoffs(handoffs []flowclient.PromptPhaseHandoff) string {
	var sections []string
	for _, handoff := range handoffs {
		content := strings.TrimSpace(handoff.Content)
		if content == "" {
			continue
		}
		label := strings.TrimSpace(handoff.PhaseName)
		if label == "" {
			label = "previous node"
		}
		sections = append(sections, fmt.Sprintf("### Handoff from %s node\n\n%s", label, content))
	}
	return strings.Join(sections, "\n\n")
}

// combinePriorHandoffs merges preceding-node handoffs with the change-scoped
// handoff snapshot (the most recent session's handoff, which fix rounds
// overwrite). The change handoff is skipped when a node section already carries
// the identical content.
func combinePriorHandoffs(nodeHandoffs string, changeHandoff string) string {
	changeHandoff = strings.TrimSpace(changeHandoff)
	nodeHandoffs = strings.TrimSpace(nodeHandoffs)
	if nodeHandoffs == "" {
		return changeHandoff
	}
	if changeHandoff == "" || strings.Contains(nodeHandoffs, changeHandoff) {
		return nodeHandoffs
	}
	return nodeHandoffs + "\n\n### Handoff from the previous session\n\n" + changeHandoff
}

// enrichPromptReviewerCheckContext resolves coordinator-stamped reviewer modes
// from the pending check's details. These markers are prompt inputs only; the
// worker replaces Details with the actual verdict report.
func enrichPromptReviewerCheckContext(input *flowprompt.Input, client *flowclient.Client) {
	taskID := strings.TrimSpace(input.TaskID)
	checkName := strings.TrimSpace(input.CheckName)
	if taskID == "" || checkName == "" {
		return
	}
	result, err := client.GetCheck(taskID, checkName)
	if err != nil {
		slog.Debug("skip completion-assessment detection", "task_id", taskID, "check", checkName, "error", err)
		return
	}
	details := strings.TrimSpace(result.Check.Details)
	if details == coordinator.CompletionAssessmentCheckMarker {
		input.CompletionAssessment = true
	}
	if details == coordinator.ReviewDiscoveryDetailsMarker {
		input.ReviewDiscovery = true
	}
	if strings.HasPrefix(details, strings.TrimSpace(coordinator.ReviewAggregationDetailsPrefix)) {
		input.ReviewAggregationContext = strings.TrimSpace(strings.TrimPrefix(
			details,
			strings.TrimSpace(coordinator.ReviewAggregationDetailsPrefix),
		))
		if statistics, statErr := reviewDiffStatistics(input.Base); statErr != nil {
			slog.Debug("skip review diff statistics", "task_id", taskID, "check", checkName, "error", statErr)
		} else {
			input.ReviewDiffStatistics = statistics
		}
	}
}

func reviewDiffStatistics(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "main"
	}
	output, err := exec.Command("git", "diff", "--numstat", "--find-renames", "origin/"+base+"...HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git diff --numstat: %w", err)
	}
	files, additions, deletions, binaries, err := parseGitNumstat(output)
	if err != nil {
		return "", err
	}
	statistics := fmt.Sprintf("Current full diff against origin/%s: %d files, +%d/-%d", base, files, additions, deletions)
	if binaries > 0 {
		statistics += fmt.Sprintf(", %d binary", binaries)
		if binaries == 1 {
			statistics += " file"
		} else {
			statistics += " files"
		}
	}
	statistics += ". Treat size as evidence for judgment, not automatic proof that the change is wrong."
	return statistics, nil
}

func parseGitNumstat(output []byte) (files, additions, deletions, binaries int, err error) {
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return 0, 0, 0, 0, fmt.Errorf("invalid git numstat line %q", line)
		}
		files++
		if fields[0] == "-" || fields[1] == "-" {
			binaries++
			continue
		}
		added, addErr := strconv.Atoi(fields[0])
		deleted, deleteErr := strconv.Atoi(fields[1])
		if addErr != nil || deleteErr != nil {
			return 0, 0, 0, 0, fmt.Errorf("invalid git numstat counts %q", line)
		}
		additions += added
		deletions += deleted
	}
	return files, additions, deletions, binaries, nil
}

func humanAttentionPromptContext(statusLog []coordinator.StatusLogEntry) string {
	var lines []string
	for _, entry := range statusLog {
		switch strings.TrimSpace(entry.Kind) {
		case coordinator.StatusKindQuestion, coordinator.StatusKindProgress, coordinator.StatusKindBlocker:
		default:
			continue
		}
		message := strings.TrimSpace(entry.Message)
		if message == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Kind)
		if actor := strings.TrimSpace(entry.Actor); actor != "" {
			prefix += " by " + actor
		}
		lines = append(lines, prefix+": "+message)
		if len(lines) >= 5 {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n\n")
}

func enrichPromptAuthorReviewContext(input *flowprompt.Input, client *flowclient.Client) error {
	taskID := strings.TrimSpace(input.TaskID)
	if taskID == "" {
		return nil
	}

	checks, err := client.ListChecks(taskID)
	if err != nil {
		return fmt.Errorf("fetch review check context: %w", err)
	}
	input.ReviewState = string(checks.ReviewState)
	for _, check := range checks.Checks {
		if !check.Required || check.Verdict != coordinator.CheckBlocked {
			continue
		}
		blocked := flowprompt.BlockedCheck{
			ID:       check.ID,
			Name:     check.Name,
			Kind:     string(check.Kind),
			Reporter: check.Reporter,
			ExitCode: check.ExitCode,
			Details:  check.Details,
		}
		if check.SourceJobID != nil {
			blocked.SourceJobID = strings.TrimSpace(*check.SourceJobID)
		}
		input.BlockedChecks = append(input.BlockedChecks, blocked)
	}

	changeID := strings.TrimSpace(input.ChangeID)
	if changeID != "" {
		threads, err := client.ListThreads(changeID, "")
		if err != nil {
			return fmt.Errorf("fetch review thread context: %w", err)
		}
		for _, thread := range threads {
			if !promptThreadIsActionable(thread.State) {
				continue
			}
			input.ReviewThreads = append(input.ReviewThreads, promptReviewThread(thread))
		}
	}
	if input.ReviewState == string(coordinator.ReviewChangesRequested) || len(input.BlockedChecks) > 0 || len(input.ReviewThreads) > 0 {
		input.FixRound = true
	}

	return nil
}

func promptInputRole(input flowprompt.Input) string {
	role := strings.ToLower(strings.TrimSpace(input.Role))
	if role == "" {
		return flowprompt.RoleAuthor
	}
	return role
}

func promptThreadIsActionable(state coordinator.ReviewThreadState) bool {
	return state == coordinator.ThreadOpen || state == coordinator.ThreadReopened
}

func promptReviewThread(thread coordinator.ReviewThread) flowprompt.ReviewThread {
	rendered := flowprompt.ReviewThread{
		ID:        thread.ID,
		State:     string(thread.State),
		FilePath:  thread.FilePath,
		Line:      thread.Line,
		Context:   thread.Context,
		CreatedBy: thread.CreatedBy,
		Comments:  make([]flowprompt.ReviewComment, 0, len(thread.Comments)),
	}
	for _, comment := range thread.Comments {
		rendered.Comments = append(rendered.Comments, flowprompt.ReviewComment{
			Actor: comment.Actor,
			Body:  comment.Body,
		})
	}
	return rendered
}

func promptTaskFetchConfigured(apiFlags *apiFlagValues) bool {
	return strings.TrimSpace(apiFlags.serverURL) != "" || strings.TrimSpace(apiFlags.configPath) != ""
}

func promptHarness(explicit string, getenv func(string) string) string {
	if value := strings.ToLower(strings.TrimSpace(explicit)); value != "" {
		return value
	}
	if value := strings.ToLower(strings.TrimSpace(getenv("FLOW_WORKER_HARNESS"))); value != "" {
		return value
	}
	return harness.DefaultPromptConventionName()
}

func harnesspkgValidatePrompt(name string) error {
	return harness.ValidatePromptConventionName(name)
}

func runComment(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("comment", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var changeID string
	var contextText string
	var leaseID string
	flags.StringVar(&changeID, "change-id", "", "change id")
	flags.StringVar(&contextText, "context", "", "anchor context")
	flags.StringVar(&leaseID, "lease-id", "", "worker lease id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: flow comment [flags] SHA:FILE:LINE BODY")
		return 2
	}
	anchor, err := parseCommentAnchor(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "parse anchor: %v\n", err)
		return 2
	}
	body := strings.TrimSpace(flags.Arg(1))
	if body == "" {
		fmt.Fprintln(stderr, "comment body is required")
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	if changeID == "" {
		changeID = os.Getenv("FLOW_CHANGE_ID")
	}
	if changeID == "" {
		fmt.Fprintln(stderr, "change id is required")
		return 2
	}
	if leaseID == "" {
		leaseID = os.Getenv("FLOW_LEASE_ID")
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	thread, err := client.CreateThread(changeID, flowclient.CreateThreadInput{
		AnchorCommitSHA: anchor.CommitSHA,
		FilePath:        anchor.FilePath,
		Line:            anchor.Line,
		Context:         contextText,
		Body:            body,
		LeaseID:         leaseID,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create comment: %v\n", err)
		return 1
	}

	printThreadLine(stdout, thread)
	return 0
}

func runThread(args []string, stdout, stderr io.Writer) int {
	return runThreadWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runThreadWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printThreadUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runThreadList(options.withConfig(args[1:]), stdout, stderr)
	case "reply":
		return runThreadReply(options.withConfig(args[1:]), stdout, stderr)
	case "claim":
		return runThreadClaim(options.withConfig(args[1:]), stdout, stderr)
	case "certify":
		return runThreadVerify("certify", options.withConfig(args[1:]), stdout, stderr)
	case "reopen":
		return runThreadVerify("reopen", options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown thread command: %s\n\n", args[0])
		printThreadUsage(stderr)
		return 2
	}
}

func runThreadList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("thread list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var leaseID string
	flags.StringVar(&leaseID, "lease-id", "", "worker lease id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: flow thread list [flags] CHANGE_ID")
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	if leaseID == "" {
		leaseID = os.Getenv("FLOW_LEASE_ID")
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	threads, err := client.ListThreads(flags.Arg(0), leaseID)
	if err != nil {
		fmt.Fprintf(stderr, "list threads: %v\n", err)
		return 1
	}
	for _, thread := range threads {
		printThreadLine(stdout, thread)
	}
	return 0
}

func runThreadReply(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("thread reply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var leaseID string
	flags.StringVar(&leaseID, "lease-id", "", "worker lease id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: flow thread reply [flags] THREAD_ID BODY")
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	if leaseID == "" {
		leaseID = os.Getenv("FLOW_LEASE_ID")
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	thread, err := client.ReplyThread(flags.Arg(0), flags.Arg(1), leaseID)
	if err != nil {
		fmt.Fprintf(stderr, "reply thread: %v\n", err)
		return 1
	}
	printThreadLine(stdout, thread)
	return 0
}

func runThreadClaim(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("thread claim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var body string
	var commitSHA string
	var leaseID string
	flags.StringVar(&body, "body", "", "claim rationale")
	flags.StringVar(&commitSHA, "commit", "", "claim commit sha")
	flags.StringVar(&leaseID, "lease-id", "", "worker lease id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: flow thread claim [flags] THREAD_ID fixed|not_warranted|superseded")
		return 2
	}
	kind := coordinator.ReviewClaimKind(strings.TrimSpace(flags.Arg(1)))
	if kind == coordinator.ClaimFixed && strings.TrimSpace(commitSHA) == "" {
		resolved, err := currentGitSHA()
		if err != nil {
			fmt.Fprintf(stderr, "resolve git HEAD: %v\n", err)
			return 1
		}
		commitSHA = resolved
	}
	applySessionEnvironment(apiFlags, nil)
	if leaseID == "" {
		leaseID = os.Getenv("FLOW_LEASE_ID")
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	thread, err := client.ClaimThread(flags.Arg(0), flowclient.ClaimThreadInput{
		Kind:           kind,
		Body:           body,
		ClaimCommitSHA: commitSHA,
		LeaseID:        leaseID,
	})
	if err != nil {
		fmt.Fprintf(stderr, "claim thread: %v\n", err)
		return 1
	}
	printThreadLine(stdout, thread)
	return 0
}

func runThreadVerify(action string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("thread "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var body string
	var leaseID string
	flags.StringVar(&body, "body", "", "verification comment")
	flags.StringVar(&leaseID, "lease-id", "", "worker lease id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: flow thread %s [flags] THREAD_ID\n", action)
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	if leaseID == "" {
		leaseID = os.Getenv("FLOW_LEASE_ID")
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	var thread coordinator.ReviewThread
	if action == "certify" {
		thread, err = client.CertifyThread(flags.Arg(0), body, leaseID)
	} else {
		thread, err = client.ReopenThread(flags.Arg(0), body, leaseID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s thread: %v\n", action, err)
		return 1
	}
	printThreadLine(stdout, thread)
	return 0
}

type commentAnchor struct {
	CommitSHA string
	FilePath  string
	Line      int
}

func parseCommentAnchor(value string) (commentAnchor, error) {
	value = strings.TrimSpace(value)
	first := strings.Index(value, ":")
	last := strings.LastIndex(value, ":")
	if first <= 0 || last <= first || last == len(value)-1 {
		return commentAnchor{}, errors.New("anchor must be SHA:FILE:LINE")
	}
	line, err := strconv.Atoi(value[last+1:])
	if err != nil || line <= 0 {
		return commentAnchor{}, errors.New("anchor line must be positive")
	}
	filePath := strings.TrimSpace(value[first+1 : last])
	if filePath == "" {
		return commentAnchor{}, errors.New("anchor file path is required")
	}

	return commentAnchor{CommitSHA: value[:first], FilePath: filePath, Line: line}, nil
}

func currentGitSHA() (string, error) {
	output, err := exec.Command("git", "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(string(output)))
	}

	sha := strings.TrimSpace(string(output))
	if sha == "" {
		return "", errors.New("git rev-parse HEAD returned empty output")
	}
	return sha, nil
}

func currentGitBranch() (string, error) {
	output, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD: %w: %s", err, strings.TrimSpace(string(output)))
	}
	branch := strings.TrimSpace(string(output))
	if branch == "" || branch == "HEAD" {
		return "", errors.New("not on a branch (detached HEAD)")
	}
	return branch, nil
}

func runHandoff(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHandoffUsage(stderr)
		return 2
	}

	switch args[0] {
	case "write":
		return runHandoffWrite(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown handoff command: %s\n\n", args[0])
		printHandoffUsage(stderr)
		return 2
	}
}

// runHandoffWrite renders a handoff and, inside a Flow session, POSTs it to the
// coordinator as an optional mid-session progress snapshot. It no longer writes
// a committed repo file: the coordinator is the sole handoff store, and the
// final handoff is submitted by `flow ready`. The rendered handoff is echoed to
// stdout so it can be captured or piped into `flow ready`.
func runHandoffWrite(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("handoff write", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var handoffFile string
	var input handoff.TemplateInput
	flags.StringVar(&handoffFile, "handoff-file", "", "read the handoff body from PATH instead of the structured flags")
	flags.StringVar(&input.CurrentGoal, "goal", "", "current goal")
	flags.StringVar(&input.CompletedWork, "completed", "", "completed work")
	flags.StringVar(&input.RemainingWork, "remaining", "", "remaining work")
	flags.StringVar(&input.TestsRun, "tests", "", "tests run and results")
	flags.StringVar(&input.FailedApproaches, "failed-approaches", "", "failed approaches")
	flags.StringVar(&input.ImportantFiles, "files", "", "important files and commands")
	flags.StringVar(&input.NextRecommendedAction, "next", "", "next recommended action")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "handoff write does not accept positional arguments")
		return 2
	}

	contents, err := handoffWriteBody(handoffFile, input)
	if err != nil {
		fmt.Fprintf(stderr, "render handoff: %v\n", err)
		return 1
	}
	if err := handoff.Validate(contents); err != nil {
		fmt.Fprintf(stderr, "render handoff: %v\n", err)
		return 1
	}

	// Inside a Flow session, sync the snapshot to the coordinator. Outside a
	// session (offline render) there is nothing to sync to. The sync is
	// best-effort: the durable handoff is submitted by flow ready, so a failed
	// progress snapshot only warns.
	changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID"))
	if os.Getenv("FLOW_SESSION_ID") != "" && changeID != "" {
		client, err := newAPIClient(apiFlags)
		if err != nil {
			fmt.Fprintf(stderr, "warning: handoff sync skipped: create client: %v\n", err)
		} else if headSHA, shaErr := currentGitSHA(); shaErr != nil {
			fmt.Fprintf(stderr, "warning: handoff sync skipped: %v\n", shaErr)
		} else if _, err := client.PutHandoff(changeID, flowclient.PutHandoffInput{
			Content: contents,
			HeadSHA: headSHA,
		}); err != nil {
			fmt.Fprintf(stderr, "warning: handoff sync failed: %v\n", err)
		}
	}

	fmt.Fprint(stdout, contents)
	if !strings.HasSuffix(contents, "\n") {
		fmt.Fprintln(stdout)
	}
	return 0
}

// handoffWriteBody returns a raw handoff body from path when supplied, otherwise
// renders one from the structured flags, stamping the session environment.
func handoffWriteBody(path string, input handoff.TemplateInput) (string, error) {
	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	input.TaskID = os.Getenv("FLOW_TASK_ID")
	input.ChangeID = os.Getenv("FLOW_CHANGE_ID")
	input.SessionID = os.Getenv("FLOW_SESSION_ID")
	input.Branch = os.Getenv("FLOW_BRANCH")
	input.Base = os.Getenv("FLOW_BASE")
	input.UpdatedAt = nowUTC()
	return handoff.RenderTemplate(input), nil
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var sessionID string
	var kind string
	flags.StringVar(&sessionID, "session-id", "", "session id")
	flags.StringVar(&kind, "kind", "note", "status kind: note, progress, plan, blocker, question")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	mode := apiFlags.outputMode()
	message := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if message == "" {
		return machineUsage(stdout, stderr, mode, "status", "status message is required")
	}

	applySessionEnvironment(apiFlags, &sessionID)
	if sessionID == "" {
		return machineUsage(stdout, stderr, mode, "status", "session id is required")
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		return machineError(stdout, stderr, mode, "status", "create client", err)
	}
	status, err := client.WriteSessionStatus(sessionID, message, kind)
	if err != nil {
		return machineError(stdout, stderr, mode, "status", "write status", err)
	}
	if mode.Machine() {
		return cliout.WriteData(stdout, mode, "status", map[string]any{"status": status})
	}

	fmt.Fprintf(stdout, "%d\t%s\t%s\n", status.ID, status.TaskID, status.Message)
	return 0
}

func runAsk(args []string, stdout, stderr io.Writer) int {
	return runStatus(append([]string{"--kind", coordinator.StatusKindQuestion}, args...), stdout, stderr)
}

// runReady is the single, idempotent author-finalize step. It collapses the old
// four-step ritual (commit, push, flow handoff write, flow ready) into one
// command the agent runs after its own git commit: it reads the handoff from
// stdin (or --handoff-file), validates it, pushes the branch to the exchange
// remote, submits the handoff to the coordinator, claims resolved trailers,
// uploads the transcript, and marks the session ready. Every mutation is a
// no-op when already applied, so a re-run is safe.
func runReady(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ready", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var sessionID string
	var handoffFile string
	var allowMissingHandoff bool
	flags.StringVar(&sessionID, "session-id", "", "session id")
	flags.StringVar(&handoffFile, "handoff-file", "", "read the handoff body from PATH instead of stdin")
	flags.BoolVar(&allowMissingHandoff, "allow-missing-handoff", false, "allow ready without a valid handoff")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ready does not accept positional arguments")
		return 2
	}

	applySessionEnvironment(apiFlags, &sessionID)
	if sessionID == "" {
		fmt.Fprintln(stderr, "session id is required")
		return 2
	}

	// flow ready completes the CURRENT work phase. The final phase publishes a
	// reviewable change and its handoff must follow the Flow Handoff template;
	// an intermediate phase's handoff is the phase's artifact (a spec, a plan)
	// shown at approval gates and injected into the next phase's prompt, so it
	// only has to be non-empty. FLOW_PHASE_FINAL is stamped by the worker; an
	// absent value means the implicit single (final) phase.
	finalPhase := strings.TrimSpace(os.Getenv("FLOW_PHASE_FINAL")) != "false"

	// Read and validate the handoff up front, before touching the remote, so a
	// malformed handoff fails fast and the agent can fix it and re-run.
	handoffBody, err := readReadyHandoff(handoffFile)
	if err != nil {
		fmt.Fprintf(stderr, "read handoff: %v\n", err)
		return 1
	}
	validationErr := func() error {
		if finalPhase {
			return handoff.Validate(handoffBody)
		}
		if strings.TrimSpace(handoffBody) == "" {
			return errors.New("phase handoff is empty; pipe the phase's output (spec, plan, ...) on stdin")
		}
		return nil
	}()
	if validationErr != nil {
		if !allowMissingHandoff {
			fmt.Fprintf(stderr, "handoff validation: %v\n", validationErr)
			return 1
		}
		fmt.Fprintf(stderr, "warning: handoff validation skipped: %v\n", validationErr)
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	headSHA, err := currentGitSHA()
	if err != nil {
		fmt.Fprintf(stderr, "resolve git HEAD: %v\n", err)
		return 1
	}
	// Publish the branch so the readied HEAD always exists on the exchange
	// remote for review and merge. Idempotent: an already-published branch
	// pushes nothing. The branch name comes from FLOW_BRANCH, falling back to
	// the checked-out branch so the push is never silently skipped.
	branch := strings.TrimSpace(os.Getenv("FLOW_BRANCH"))
	if branch == "" {
		branch, err = currentGitBranch()
		if err != nil {
			fmt.Fprintf(stderr, "resolve branch to push: %v\n", err)
			return 1
		}
	}
	if err := flowgit.PushBranch(context.Background(), "", branch); err != nil {
		fmt.Fprintf(stderr, "push branch: %v\n", err)
		return 1
	}
	// Submit the handoff to the coordinator, now the sole store. A re-run
	// overwrites the same snapshot, so this is idempotent too. The engine
	// copies it into the per-phase handoff store when the phase completes.
	if changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID")); changeID != "" && strings.TrimSpace(handoffBody) != "" {
		if _, err := client.PutHandoff(changeID, flowclient.PutHandoffInput{
			Content: handoffBody,
			HeadSHA: headSHA,
		}); err != nil {
			fmt.Fprintf(stderr, "submit handoff: %v\n", err)
			return 1
		}
	}
	if finalPhase {
		if err := claimResolvedTrailers(client); err != nil {
			fmt.Fprintf(stderr, "claim resolved threads: %v\n", err)
			return 1
		}
	}
	uploadReadyTranscriptBestEffort(client, sessionID, stderr)
	session, err := client.ReadySessionWithInput(sessionID, flowclient.ReadySessionInput{HeadSHA: headSHA})
	if err != nil {
		fmt.Fprintf(stderr, "ready session: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\t%s\t%s\n", session.ID, session.RuntimeState, session.ChangeID)
	return 0
}

// readReadyHandoff returns the handoff body from the given file, or from stdin
// when no file is supplied. Interactive authors pipe the handoff
// (`flow ready < handoff.md` or a heredoc); non-interactive callers pass
// --handoff-file PATH. When neither is provided and stdin is an interactive
// terminal, it returns an empty body rather than blocking, so handoff
// validation reports a clear error instead of hanging.
func readReadyHandoff(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if stdinIsInteractiveTerminal() {
		return "", nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read handoff from stdin: %w", err)
	}
	return string(data), nil
}

func stdinIsInteractiveTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const readyTranscriptTailBytes = 10 << 20 // 10 MiB

// uploadReadyTranscriptBestEffort persists the worker-owned tmux transcript
// before ReadySession revokes the author session token. The worker still does a
// post-run fallback upload, so failures here warn but never block readiness.
func uploadReadyTranscriptBestEffort(client *flowclient.Client, sessionID string, stderr io.Writer) {
	path := strings.TrimSpace(os.Getenv("FLOW_TRANSCRIPT_FILE"))
	if path == "" {
		return
	}
	tail, err := readFileTail(path, readyTranscriptTailBytes)
	if err != nil {
		fmt.Fprintf(stderr, "warning: transcript sync skipped: read transcript: %v\n", err)
		return
	}
	if len(tail) == 0 {
		return
	}
	if err := client.UploadSessionTranscript(context.Background(), sessionID, bytes.NewReader(tail)); err != nil {
		fmt.Fprintf(stderr, "warning: transcript sync failed: %v\n", err)
	}
}

func runCompleteWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if strings.TrimSpace(os.Getenv("FLOW_COMPLETION_PROTOCOL")) == checkverdict.CompletionProtocol {
		return runComplete(args, stdout, stderr)
	}
	return runComplete(options.withConfig(args), stdout, stderr)
}

func runComplete(args []string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(os.Getenv("FLOW_COMPLETION_PROTOCOL")) == checkverdict.CompletionProtocol {
		return runCheckComplete(args, stdout, stderr)
	}
	flags := flag.NewFlagSet("complete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var taskID string
	var nodeRunID string
	var kind string
	var summaryFile string
	var outputFile string
	var clientKey string
	flags.StringVar(&taskID, "task", "", "task id (default FLOW_TASK_ID)")
	flags.StringVar(&nodeRunID, "node-run", "", "node run id (default FLOW_NODE_RUN_ID)")
	flags.StringVar(&kind, "kind", "", "handoff|change|task_set (default FLOW_ARTIFACT_KIND)")
	flags.StringVar(&summaryFile, "summary-file", "", "Markdown summary file")
	flags.StringVar(&outputFile, "output-file", "", "JSON artifact payload file")
	flags.StringVar(&clientKey, "client-key", "", "idempotency key (default node run id)")
	var evidenceFlags stringSliceFlag
	flags.Var(&evidenceFlags, "evidence", "typed completion evidence as type:value (repeatable); types: commit|test|pr|review|note")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "complete does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(taskID) == "" {
		taskID = os.Getenv("FLOW_TASK_ID")
	}
	if strings.TrimSpace(nodeRunID) == "" {
		nodeRunID = os.Getenv("FLOW_NODE_RUN_ID")
	}
	if strings.TrimSpace(kind) == "" {
		kind = os.Getenv("FLOW_ARTIFACT_KIND")
	}
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeRunID) == "" || strings.TrimSpace(kind) == "" {
		fmt.Fprintln(stderr, "task, node run, and artifact kind are required")
		return 2
	}
	if strings.TrimSpace(summaryFile) == "" {
		fmt.Fprintln(stderr, "--summary-file is required")
		return 2
	}
	summary, err := os.ReadFile(summaryFile)
	if err != nil {
		fmt.Fprintf(stderr, "read summary: %v\n", err)
		return 1
	}
	var payload json.RawMessage
	if strings.TrimSpace(outputFile) != "" {
		payload, err = os.ReadFile(outputFile)
		if err != nil {
			fmt.Fprintf(stderr, "read output: %v\n", err)
			return 1
		}
	}
	evidence, err := parseEvidenceFlags(evidenceFlags.Values)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	if coordinator.ArtifactKind(kind) == coordinator.ArtifactChange {
		changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID"))
		if changeID == "" {
			fmt.Fprintln(stderr, "change completion requires FLOW_CHANGE_ID")
			return 2
		}
		changePayload := map[string]any{}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &changePayload); err != nil {
				fmt.Fprintf(stderr, "decode change output: %v\n", err)
				return 2
			}
			if supplied, _ := changePayload["change_id"].(string); strings.TrimSpace(supplied) != "" && strings.TrimSpace(supplied) != changeID {
				fmt.Fprintln(stderr, "change output change_id does not match FLOW_CHANGE_ID")
				return 2
			}
		}
		headSHA, err := currentGitSHA()
		if err != nil {
			fmt.Fprintf(stderr, "resolve git HEAD: %v\n", err)
			return 1
		}
		branch := strings.TrimSpace(os.Getenv("FLOW_BRANCH"))
		if branch == "" {
			branch, err = currentGitBranch()
			if err != nil {
				fmt.Fprintf(stderr, "resolve branch to push: %v\n", err)
				return 1
			}
		}
		if err := flowgit.PushBranch(context.Background(), "", branch); err != nil {
			fmt.Fprintf(stderr, "push branch: %v\n", err)
			return 1
		}
		changePayload["change_id"] = changeID
		changePayload["head_sha"] = headSHA
		// The pinned HEAD is itself completion evidence.
		evidence = append([]coordinator.Evidence{{Type: coordinator.EvidenceCommit, Value: headSHA}}, evidence...)
		payload, _ = json.Marshal(changePayload)
	}
	if len(evidence) > 0 {
		payloadMap := map[string]any{}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &payloadMap)
		}
		payloadMap["evidence"] = evidence
		payload, _ = json.Marshal(payloadMap)
	}
	if clientKey == "" {
		clientKey = nodeRunID
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	artifact, replayed, err := client.CreateWorkflowArtifact(taskID, coordinator.CreateWorkflowArtifactInput{
		NodeRunID: nodeRunID, Kind: coordinator.ArtifactKind(kind), SummaryMarkdown: string(summary),
		Payload: payload, ClientKey: clientKey,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create workflow artifact: %v\n", err)
		return 1
	}
	result, err := client.CompleteWorkflowAgentNode(taskID, nodeRunID, artifact.ID)
	if err != nil {
		fmt.Fprintf(stderr, "complete workflow node: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\treplayed=%t\n", artifact.ID, result.Run.ID, result.Run.State, replayed || result.Replayed)
	return 0
}

func runCheckComplete(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "check completion does not accept flags or positional arguments")
		return 2
	}
	jobID := strings.TrimSpace(os.Getenv("FLOW_JOB_ID"))
	checkName := strings.TrimSpace(os.Getenv("FLOW_CHECK_NAME"))
	verdictPath := strings.TrimSpace(os.Getenv("FLOW_VERDICT_FILE"))
	completionPath := strings.TrimSpace(os.Getenv("FLOW_COMPLETION_FILE"))
	modeValue := strings.TrimSpace(os.Getenv("FLOW_CHECK_MODE"))
	if jobID == "" || checkName == "" || verdictPath == "" || completionPath == "" || modeValue == "" {
		fmt.Fprintln(stderr, "check completion requires FLOW_JOB_ID, FLOW_CHECK_NAME, FLOW_CHECK_MODE, FLOW_VERDICT_FILE, and FLOW_COMPLETION_FILE")
		return 2
	}
	mode, err := checkverdict.ParseMode(modeValue)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	validated, err := checkverdict.SealVerdict(verdictPath, completionPath, checkverdict.Context{
		JobID:     jobID,
		CheckName: checkName,
		Mode:      mode,
	})
	if err != nil {
		fmt.Fprintf(stderr, "complete check: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", jobID, checkName, validated.Report.Verdict)
	return 0
}

// runSubmit hands the agent node's artifact to the human reviewer without
// ending the session: it creates the artifact, opens the interactive review
// wait on the current node, then blocks until the review resolves and prints
// the verdict. On "changes_requested" the agent revises and submits again in
// the same session; any other outcome is final and the agent should stop.
func runSubmit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("submit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var taskID string
	var nodeRunID string
	var kind string
	var summaryFile string
	var outputFile string
	var clientKey string
	var sessionID string
	flags.StringVar(&taskID, "task", "", "task id (default FLOW_TASK_ID)")
	flags.StringVar(&nodeRunID, "node-run", "", "node run id (default FLOW_NODE_RUN_ID)")
	flags.StringVar(&kind, "kind", "", "handoff|task_set (default FLOW_ARTIFACT_KIND)")
	flags.StringVar(&summaryFile, "summary-file", "", "Markdown summary file")
	flags.StringVar(&outputFile, "output-file", "", "JSON artifact payload file")
	flags.StringVar(&clientKey, "client-key", "", "idempotency key (default node run id + content hash)")
	flags.StringVar(&sessionID, "session-id", "", "session id (default FLOW_SESSION_ID)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "submit does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(taskID) == "" {
		taskID = os.Getenv("FLOW_TASK_ID")
	}
	if strings.TrimSpace(nodeRunID) == "" {
		nodeRunID = os.Getenv("FLOW_NODE_RUN_ID")
	}
	if strings.TrimSpace(kind) == "" {
		kind = os.Getenv("FLOW_ARTIFACT_KIND")
	}
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeRunID) == "" || strings.TrimSpace(kind) == "" {
		fmt.Fprintln(stderr, "task, node run, and artifact kind are required")
		return 2
	}
	if coordinator.ArtifactKind(kind) == coordinator.ArtifactChange {
		fmt.Fprintln(stderr, "change artifacts are finalized with flow complete, not flow submit")
		return 2
	}
	if strings.TrimSpace(summaryFile) == "" {
		fmt.Fprintln(stderr, "--summary-file is required")
		return 2
	}
	summary, err := os.ReadFile(summaryFile)
	if err != nil {
		fmt.Fprintf(stderr, "read summary: %v\n", err)
		return 1
	}
	var payload json.RawMessage
	if strings.TrimSpace(outputFile) != "" {
		payload, err = os.ReadFile(outputFile)
		if err != nil {
			fmt.Fprintf(stderr, "read output: %v\n", err)
			return 1
		}
	}
	if clientKey == "" {
		// Revision-friendly default: identical content replays, revised content
		// gets a fresh key, so one node run can accumulate review rounds.
		digest := sha256.Sum256(append(append([]byte(kind), summary...), payload...))
		clientKey = nodeRunID + ":" + hex.EncodeToString(digest[:])[:12]
	}
	applySessionEnvironment(apiFlags, &sessionID)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	artifact, _, err := client.CreateWorkflowArtifact(taskID, coordinator.CreateWorkflowArtifactInput{
		NodeRunID: nodeRunID, Kind: coordinator.ArtifactKind(kind), SummaryMarkdown: string(summary),
		Payload: payload, ClientKey: clientKey,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create workflow artifact: %v\n", err)
		return 1
	}
	if err := client.SubmitForReview(taskID, nodeRunID, artifact.ID); err != nil {
		fmt.Fprintf(stderr, "submit for review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "submitted %s for review; waiting for the human verdict\n", artifact.ID)
	if sessionID != "" {
		if _, err := client.UpdateSessionState(sessionID, coordinator.SessionWaiting); err != nil {
			fmt.Fprintf(stderr, "warning: mark session waiting: %v\n", err)
		}
	}
	status, code := waitForReviewVerdict(client, taskID, nodeRunID, stderr)
	if sessionID != "" {
		if _, err := client.UpdateSessionState(sessionID, coordinator.SessionWorking); err != nil {
			fmt.Fprintf(stderr, "warning: mark session working: %v\n", err)
		}
	}
	if code != 0 {
		return code
	}
	if status.Outcome != "changes_requested" {
		// The review is final: finalize this session through the idempotent
		// complete endpoint (the node already completed with this artifact, so
		// this replays) — flow complete's handler is what finishes the session.
		if _, err := client.CompleteWorkflowAgentNode(taskID, nodeRunID, artifact.ID); err != nil {
			fmt.Fprintf(stderr, "warning: finalize session: %v\n", err)
		}
	}
	switch status.Outcome {
	case "changes_requested":
		fmt.Fprintf(stdout, "REVIEW: changes requested\n\n%s\n\nRevise the plan and submit again with flow submit.\n", status.Feedback)
	case "":
		fmt.Fprintln(stdout, "REVIEW: resolved")
	default:
		fmt.Fprintf(stdout, "REVIEW: %s\n", strings.ReplaceAll(status.Outcome, "_", " "))
		if strings.TrimSpace(status.Feedback) != "" {
			fmt.Fprintf(stdout, "\n%s\n", status.Feedback)
		}
	}
	return 0
}

func waitForReviewVerdict(client *flowclient.Client, taskID, nodeRunID string, stderr io.Writer) (coordinator.ReviewStatusResult, int) {
	failures := 0
	for {
		status, err := client.GetReviewStatus(taskID, nodeRunID)
		if err != nil {
			failures++
			if failures >= 30 {
				fmt.Fprintf(stderr, "poll review status: %v\n", err)
				return coordinator.ReviewStatusResult{}, 1
			}
		} else {
			failures = 0
			switch status.State {
			case coordinator.ReviewStateResolved:
				return status, 0
			case coordinator.ReviewStateNone:
				fmt.Fprintln(stderr, "review ended without a verdict")
				return coordinator.ReviewStatusResult{}, 1
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func runMerge(args []string, stdout, stderr io.Writer) int {
	parsed, code := parseAPICommand(args, stderr, "merge", 1, "usage: flow merge [flags] TASK_ID|CHANGE_ID")
	if code != 0 {
		return code
	}
	client, target := scopeClientForRef(parsed.client, strings.TrimSpace(parsed.flags.Arg(0)))
	var result coordinator.MergeResult
	var err error
	if strings.HasPrefix(target, "ch-") {
		result, err = client.MergeChange(target)
	} else {
		result, err = client.MergeTask(target)
	}
	if err != nil {
		fmt.Fprintf(stderr, "merge: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", result.Task.ID, result.Change.ID, result.MergeSHA, result.HeadSHA)
	return 0
}

func claimResolvedTrailers(client *flowclient.Client) error {
	message, err := currentGitMessage()
	if err != nil {
		return nil
	}
	threadIDs, err := flowgit.ResolveThreadIDsFromMessage(context.Background(), message)
	if err != nil {
		return err
	}
	if len(threadIDs) == 0 {
		return nil
	}
	commitSHA, err := currentGitSHA()
	if err != nil {
		return err
	}
	for _, threadID := range threadIDs {
		if _, err := client.ClaimThread(threadID, flowclient.ClaimThreadInput{
			Kind:           coordinator.ClaimFixed,
			ClaimCommitSHA: commitSHA,
		}); err != nil && !strings.Contains(err.Error(), "thread_not_found") {
			return err
		}
	}

	return nil
}

func currentGitMessage() (string, error) {
	output, err := exec.Command("git", "log", "-1", "--format=%B").Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

func runReconcile(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "reconcile does not accept positional arguments")
		return 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	result, err := client.Reconcile()
	if err != nil {
		fmt.Fprintf(stderr, "reconcile: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "projects_scanned: %d\n", result.ProjectsScanned)
	fmt.Fprintf(stdout, "projects_skipped: %d\n", result.ProjectsSkipped)
	fmt.Fprintf(stdout, "branches_scanned: %d\n", result.BranchesScanned)
	fmt.Fprintf(stdout, "changes_created: %d\n", result.ChangesCreated)
	fmt.Fprintf(stdout, "changes_updated: %d\n", result.ChangesUpdated)
	if len(result.SkippedProjects) > 0 {
		fmt.Fprintf(stdout, "skipped_projects: %s\n", strings.Join(result.SkippedProjects, ","))
	}
	if len(result.UnknownBranches) > 0 {
		fmt.Fprintf(stdout, "unknown_branches: %s\n", strings.Join(result.UnknownBranches, ","))
	}
	return 0
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow [--log-level LEVEL] [--config PATH] COMMAND
  flow init [--repo PATH] [--name NAME] [--base BRANCH] [--with-agents]
  flow doctor [--db PATH] [--config PATH]
  flow doctor work-items [--project PROJECT] [API flags]
	  flow task create --title TITLE [--flow FLOW] [--feature FEATURE] [--parent ITEM_ID] [--file PATH]
  flow task attach TASK_ID --file PATH [--stage initial|author|reviewer|verifier]
  flow task list [--state unscheduled,scheduled,in_progress,done]
  flow task show [--project PROJECT] TASK_ID
  flow task relations [--project PROJECT] TASK_ID
  flow task reply TASK_ID MESSAGE
  flow task schedule TASK_ID
  flow task reset|reopen|retry|workflow TASK_ID
  flow task respond TASK_ID --node-run NODE_RUN_ID --review-wait REVIEW_WAIT_ID --outcome OUTCOME [--feedback TEXT]
  flow task budget TASK_ID --additional N
  flow task done TASK_ID [--resolution RESOLUTION] [--message TEXT] [--evidence type:value]
	  flow feature create --title TITLE [--body BODY] [--parent ITEM_ID]
  flow feature list [--status open|landed|archived|all]
	  flow feature show|edit|rebase|land|archive|start FEATURE_ID
	  flow epic create|list|show|edit|start|complete|reopen|archive
	  flow work-item show|tree|link|unlink|relations
  flow board
  flow ready [--tag TAG]
  flow next [--tag TAG]
  flow wait TASK_ID... [--until done|blocked|scheduled|in_progress] [--any|--all] [--timeout 30s] [--poll-interval 2s]
  flow events [--since N] [--limit N] [--kind K] [--task ID] [--actor A] [--follow|--tail]
  flow search QUERY [--limit N]
  flow audit completions [--resolution R] [--limit N]
  flow mcp serve [--project P]
  flow quickstart
  flow checks TASK_ID
  flow transitions TASK_ID
  flow workers
  flow jobs
  flow history list [--since RFC3339] [--until RFC3339] [--format table|json]
  flow history export --output DIR (--all | SELECTORS) [--allow-incomplete]
  flow history resume CAPTURE_ID [--native-session ID] [--idempotency-key KEY]
  flow ui
  flow attach [--job] SESSION_ID|JOB_ID
  flow session event working|waiting
  flow hook harness EVENT|ingest
  flow fetch-prompt [--role author|reviewer|verifier] [--harness harness|agents]
  flow comment SHA:FILE:LINE BODY
  flow thread reply|claim|certify|reopen
  flow status MESSAGE
  flow ask QUESTION
  flow complete [--summary-file PATH] [--output-file PATH] [--evidence type:value]
  flow submit --summary-file PATH [--output-file PATH]
  flow reconcile
  flow --version

Global flags:
  --log-level LEVEL   structured log level: debug, info, warn, error, or off (overrides LOG_LEVEL)
  --config PATH       client config path for owner commands (also accepted on those commands for compatibility)
  --json              print command results as bare JSON on stdout (machine contract v1)
  --agent             print versioned JSON envelopes with structured errors (implies --json)

API override flags on owner commands:
  --server URL        coordinator server URL
  --token TOKEN       bearer token
  --project PROJECT   project id or name
  --json              bare JSON result on stdout
  --agent             versioned JSON envelope on stdout (implies --json)
`)
}

func printTaskUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
	  flow task create --title TITLE [--flow FLOW] [--feature FEATURE] [--parent ITEM_ID] [--file PATH]
  flow task attach [flags] TASK_ID
  flow task list
  flow task show [flags] TASK_ID
  flow task relations [flags] TASK_ID
  flow task edit [flags] TASK_ID
	  flow task reply [flags] TASK_ID [MESSAGE]
	  flow task guide [flags] TASK_ID MESSAGE [--supersedes RULING_ID]
	  flow task decide-review [flags] TASK_ID WAIT_ID fix_in_task|out_of_scope|defer_follow_up
  flow task schedule [flags] TASK_ID
  flow task reset [flags] TASK_ID
  flow task done [flags] TASK_ID [--resolution completed|rejected|abandoned|cancelled|failed]
  flow task reopen [flags] TASK_ID
  flow task workflow [flags] TASK_ID
  flow task respond [flags] TASK_ID --node-run NODE_RUN_ID --review-wait REVIEW_WAIT_ID --outcome OUTCOME
  flow task budget [flags] TASK_ID --additional N
  flow task retry [flags] TASK_ID
  flow task link [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
  flow task unlink [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
  flow task relations [flags] TASK_ID
`)
}

func printFeatureUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow feature create --title TITLE [--body BODY] [--parent ITEM_ID]
  flow feature list [--status open|landed|archived|all]
  flow feature show [flags] FEATURE_ID
  flow feature edit [flags] FEATURE_ID
  flow feature rebase [flags] FEATURE_ID
  flow feature land [flags] FEATURE_ID
  flow feature archive [flags] FEATURE_ID
  flow feature start [flags] FEATURE_ID

A feature groups a set of tasks behind one long-lived feature branch in the
project's exchange remote. Tasks assigned to a feature merge back into the
feature branch; rebase pulls the base branch into it and land squash-merges
the feature into the base branch. Archive is the only delete; the branch is
retained for audit.
`)
}

func printEpicUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow epic create --title TITLE [--body BODY] [--priority N] [--completion-policy all_children|manual] [--parent ITEM_ID]
  flow epic list [--status open|completed|archived|all]
  flow epic show [flags] EPIC_ID
  flow epic edit [flags] EPIC_ID
  flow epic start [flags] EPIC_ID
  flow epic complete [flags] EPIC_ID
  flow epic reopen [flags] EPIC_ID
  flow epic archive [flags] EPIC_ID
`)
}

func printWorkItemUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow work-item show [flags] ITEM_ID
  flow work-item tree [flags] ITEM_ID
  flow work-item link [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
  flow work-item unlink [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
  flow work-item relations [flags] ITEM_ID
`)
}

func printHandoffUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow handoff write [flags]   (renders a handoff to stdout; inside a session
                                also POSTs it as a mid-session progress snapshot)
`)
}

func printReviewUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow review run [flags] TASK_ID
`)
}

func printSessionUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow session event [flags] working|waiting
`)
}

func printHookUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow hook harness [flags] start|stop|working|waiting
  flow hook harness ingest [flags]

Client-side git hooks (installed into the agent worktree; never block git):
  flow hook harness prepush       (capture push + steer threads)
  flow hook harness commit-msg MSGFILE   (record Resolves: trailers)
`)
}

func printThreadUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow thread list [flags] CHANGE_ID
  flow thread reply [flags] THREAD_ID BODY
  flow thread claim [flags] THREAD_ID fixed|not_warranted|superseded
  flow thread certify [flags] THREAD_ID
  flow thread reopen [flags] THREAD_ID
`)
}

type apiFlagValues struct {
	configPath string
	serverURL  string
	token      string
	project    string
	jsonOut    bool
	agentOut   bool
}

// outputMode resolves the machine-readable output contract requested via
// --json/--agent (global or command-local placement both land here).
func (values *apiFlagValues) outputMode() cliout.Mode {
	return cliout.FromFlags(values.jsonOut, values.agentOut)
}

// machineError renders err through the machine contract when active,
// otherwise prints "context: err" to stderr. Returns the exit code (1).
func machineError(stdout, stderr io.Writer, mode cliout.Mode, command, context string, err error) int {
	if mode.Machine() {
		return cliout.WriteError(stdout, mode, command, err)
	}
	fmt.Fprintf(stderr, "%s: %v\n", context, err)
	return 1
}

// machineUsage renders a usage error through the machine contract when
// active, otherwise prints the message to stderr. Returns the exit code (2).
func machineUsage(stdout, stderr io.Writer, mode cliout.Mode, command, message string) int {
	if mode.Machine() {
		return cliout.WriteUsageError(stdout, mode, command, message)
	}
	fmt.Fprintln(stderr, message)
	return 2
}

type parsedAPICommand struct {
	flags  *flag.FlagSet
	client *flowclient.Client
	mode   cliout.Mode
}

type stringSliceFlag struct {
	Values []string
}

func (f *stringSliceFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value is required")
	}
	f.Values = append(f.Values, value)
	return nil
}

func (f *stringSliceFlag) String() string {
	if f == nil || len(f.Values) == 0 {
		return ""
	}
	return strings.Join(f.Values, ",")
}

func addAPIFlags(flags *flag.FlagSet) *apiFlagValues {
	values := &apiFlagValues{}
	flags.StringVar(&values.configPath, "config", "", "client config JSON path")
	flags.StringVar(&values.serverURL, "server", "", "coordinator server URL")
	flags.StringVar(&values.token, "token", "", "owner bearer token")
	flags.StringVar(&values.project, "project", "", "project id or name (default: auto-detect from the current repo)")
	flags.BoolVar(&values.jsonOut, "json", false, "print the result as bare JSON on stdout")
	flags.BoolVar(&values.agentOut, "agent", false, "print a versioned JSON envelope (contract v1) on stdout; implies --json")
	return values
}

func newAPIFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *apiFlagValues) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags, addAPIFlags(flags)
}

func parseAPICommand(args []string, stderr io.Writer, name string, positionalCount int, positionalError string) (parsedAPICommand, int) {
	flags, apiFlags := newAPIFlagSet(name, stderr)
	if err := flags.Parse(args); err != nil {
		return parsedAPICommand{}, 2
	}
	if positionalCount >= 0 && flags.NArg() != positionalCount {
		fmt.Fprintln(stderr, positionalError)
		return parsedAPICommand{}, 2
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return parsedAPICommand{}, 1
	}

	return parsedAPICommand{flags: flags, client: client, mode: apiFlags.outputMode()}, 0
}

func parseScopedTaskAPICommand(args []string, stderr io.Writer, name string, positionalCount int, positionalError string) (parsedAPICommand, string, int) {
	parsed, code := parseAPICommand(args, stderr, name, positionalCount, positionalError)
	if code != 0 {
		return parsedAPICommand{}, "", code
	}
	client, taskRef := scopeClientForRef(parsed.client, parsed.flags.Arg(0))
	parsed.client = client
	return parsed, taskRef, 0
}

func parseTaskRelationCommand(args []string, stderr io.Writer, name string) (parsedAPICommand, string, string, coordinator.RelationKind, int) {
	parsed, code := parseAPICommand(args, stderr, name, 3, "usage: flow "+name+" [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID")
	if code != 0 {
		return parsedAPICommand{}, "", "", "", code
	}

	sourceProject, sourceID := splitQualifiedRef(parsed.flags.Arg(0))
	targetProject, targetID := splitQualifiedRef(parsed.flags.Arg(2))
	if embedded, ok := coordinator.ProjectIDFromTaskID(sourceID); ok {
		sourceProject = embedded
	}
	if embedded, ok := coordinator.ProjectIDFromTaskID(targetID); ok {
		targetProject = embedded
	}
	if sourceProject != "" && targetProject != "" && sourceProject != targetProject {
		fmt.Fprintln(stderr, "source and target tasks must be in the same project")
		return parsedAPICommand{}, "", "", "", 2
	}
	if sourceProject != "" {
		parsed.client = parsed.client.WithProject(sourceProject)
	} else if targetProject != "" {
		parsed.client = parsed.client.WithProject(targetProject)
	}

	return parsed, sourceID, targetID, coordinator.RelationKind(parsed.flags.Arg(1)), 0
}

func newAPIClient(values *apiFlagValues) (*flowclient.Client, error) {
	cfg, err := resolvedAPIConfig(values)
	if err != nil {
		return nil, err
	}

	client, err := flowclient.New(cfg)
	if err != nil {
		return nil, err
	}

	if ref := resolveProjectRef(values, client); ref != "" {
		client = client.WithProject(ref)
	}

	return client, nil
}

func resolvedAPIConfig(values *apiFlagValues) (config.ClientConfig, error) {
	applyClientEnvironment(values)

	cfg, err := config.LoadClient(values.configPath)
	if err != nil {
		return config.ClientConfig{}, err
	}
	if values.serverURL != "" {
		cfg.ServerURL = values.serverURL
	}
	if values.token != "" {
		cfg.Token = values.token
	}
	if strings.TrimSpace(cfg.Token) == "" {
		token, _, ok, err := config.ResolveOwnerTokenFallback(cfg)
		if err != nil {
			return config.ClientConfig{}, err
		}
		if ok {
			cfg.Token = token
		}
	}

	return cfg, nil
}

// applyClientEnvironment fills unset flag values from the environment. The
// token chain prefers the session and worker tokens that the worker injects
// into agent shells over a human's owner token.
func applyClientEnvironment(values *apiFlagValues) {
	if values.serverURL == "" {
		values.serverURL = os.Getenv("FLOW_COORDINATOR_URL")
	}
	if values.token == "" {
		values.token = os.Getenv("FLOW_SESSION_TOKEN")
	}
	if values.token == "" {
		values.token = os.Getenv("FLOW_WORKER_TOKEN")
	}
	if values.token == "" {
		values.token = os.Getenv("FLOW_OWNER_TOKEN")
	}
}

// resolveProjectRef picks the project context for a command: an explicit
// --project wins, then the worker-injected FLOW_PROJECT_ID (worker checkouts
// are clones whose paths are not registered, so the environment must beat the
// cwd lookup), then the project registered for the current repo root. An
// empty result leaves routes unscoped: the coordinator resolves session
// tokens to their bound project and single-project deployments implicitly.
func resolveProjectRef(values *apiFlagValues, client *flowclient.Client) string {
	if values.project != "" {
		return values.project
	}
	if env := strings.TrimSpace(os.Getenv("FLOW_PROJECT_ID")); env != "" {
		return env
	}
	root, err := resolveInitRepoRoot(".")
	if err != nil {
		return ""
	}
	project, err := client.LookupProjectByRepoPath(root)
	if err != nil || project == nil {
		return ""
	}

	return project.ID
}

// splitQualifiedRef peels an optional "project/" qualifier off a task or
// change ref. Canonical task IDs already carry their project; qualifiers are
// still useful for change references and explicit project-scoped calls.
func splitQualifiedRef(ref string) (string, string) {
	projectRef, id, found := strings.Cut(ref, "/")
	if !found {
		return "", ref
	}
	if strings.HasPrefix(id, "t-") || strings.HasPrefix(id, "ch-") || strings.HasPrefix(id, "f-") {
		return projectRef, id
	}

	return "", ref
}

// scopeClientForRef rescopes the client when a positional ref carries a
// project qualifier.
func scopeClientForRef(client *flowclient.Client, ref string) (*flowclient.Client, string) {
	projectRef, id := splitQualifiedRef(ref)
	if projectRef != "" {
		return client.WithProject(projectRef), id
	}
	if embeddedProject, ok := coordinator.ProjectIDFromWorkItemID(id); ok {
		return client.WithProject(embeddedProject), id
	}

	return client, ref
}

func uploadTaskAttachmentFile(client *flowclient.Client, taskID string, filePath string, stage coordinator.TaskAttachmentStage) (coordinator.TaskAttachment, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return coordinator.TaskAttachment{}, errors.New("attachment file path is required")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return coordinator.TaskAttachment{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return coordinator.TaskAttachment{}, err
	}
	if info.IsDir() {
		return coordinator.TaskAttachment{}, fmt.Errorf("%s is a directory", filePath)
	}
	contentType, err := detectAttachmentContentType(file, filePath)
	if err != nil {
		return coordinator.TaskAttachment{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return coordinator.TaskAttachment{}, err
	}

	return client.UploadTaskAttachment(taskID, flowclient.UploadTaskAttachmentInput{
		Stage:       stage,
		Filename:    filepath.Base(filePath),
		ContentType: contentType,
		Reader:      file,
		LeaseID:     strings.TrimSpace(os.Getenv("FLOW_LEASE_ID")),
	})
}

func detectAttachmentContentType(file *os.File, filePath string) (string, error) {
	if contentType := strings.TrimSpace(mime.TypeByExtension(filepath.Ext(filePath))); contentType != "" {
		return contentType, nil
	}
	var prefix [512]byte
	n, err := file.Read(prefix[:])
	if err != nil && err != io.EOF {
		return "", err
	}
	if n == 0 {
		return "application/octet-stream", nil
	}

	return http.DetectContentType(prefix[:n]), nil
}

func taskAttachmentStageFromCLI(stage string) (coordinator.TaskAttachmentStage, error) {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = strings.TrimSpace(os.Getenv("FLOW_ROLE"))
	}
	if stage == "" {
		stage = strings.TrimSpace(os.Getenv("FLOW_WORKER_ROLE"))
	}
	switch coordinator.TaskAttachmentStage(stage) {
	case coordinator.TaskAttachmentStageInitial:
		return coordinator.TaskAttachmentStageInitial, nil
	case coordinator.TaskAttachmentStageAuthor:
		return coordinator.TaskAttachmentStageAuthor, nil
	case coordinator.TaskAttachmentStageReviewer:
		return coordinator.TaskAttachmentStageReviewer, nil
	case coordinator.TaskAttachmentStageVerifier:
		return coordinator.TaskAttachmentStageVerifier, nil
	case "":
		return coordinator.TaskAttachmentStageInitial, nil
	default:
		return "", fmt.Errorf("must be one of initial, author, reviewer, or verifier")
	}
}

func applySessionEnvironment(apiFlags *apiFlagValues, sessionID *string) {
	applyClientEnvironment(apiFlags)
	if sessionID != nil && *sessionID == "" {
		*sessionID = os.Getenv("FLOW_SESSION_ID")
	}
}

func resolveInitRepoRoot(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return "", fmt.Errorf("verify git worktree: %s: %w", message, err)
		}
		return "", fmt.Errorf("verify git worktree: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("verify git worktree: empty repository root")
	}
	resolved, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve git worktree: %w", err)
	}
	return resolved, nil
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

func nowUTC() time.Time {
	return time.Now().UTC()
}

func printTaskLine(out io.Writer, task coordinator.Task) {
	resolution := ""
	if task.DoneResolution != nil {
		resolution = string(*task.DoneResolution)
	}
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", task.ID, taskLifecycleLabel(task), resolution, task.Title)
}

func printFeatureLine(out io.Writer, feature coordinator.Feature) {
	fmt.Fprintf(out, "%s\t%s\t%s\n", feature.ID, feature.Status, feature.Title)
}

func printFeatureDetail(out io.Writer, detail contract.FeatureResponse) {
	feature := detail.Feature
	printFeatureLine(out, feature)
	fmt.Fprintf(out, "branch\t%s\n", feature.Branch)
	if detail.BranchState != nil {
		fmt.Fprintf(out, "divergence\t%d ahead, %d behind\n", detail.BranchState.Ahead, detail.BranchState.Behind)
	}
	fmt.Fprintf(out, "tasks\t%d open, %d scheduled, %d in progress, %d done\n",
		detail.Counts.Open, detail.Counts.Scheduled, detail.Counts.InProgress, detail.Counts.Done)
	if detail.RunningRebase != nil {
		fmt.Fprintf(out, "rebase\t%s (task %s)\n", detail.RunningRebase.State, detail.RunningRebase.TaskID)
	}
	if strings.TrimSpace(feature.Body) != "" {
		fmt.Fprintf(out, "\n%s\n", feature.Body)
	}
}

func printEpicLine(out io.Writer, epic coordinator.Epic) {
	fmt.Fprintf(out, "%s\t%s\t%s\n", epic.ID, epic.Status, epic.Title)
}

func printEpicDetail(out io.Writer, detail contract.EpicResponse) {
	printEpicLine(out, detail.Epic)
	fmt.Fprintf(out, "completion policy\t%s\n", detail.Epic.CompletionPolicy)
	fmt.Fprintf(out, "children\t%d\n", len(detail.Children))
	if strings.TrimSpace(detail.Epic.Body) != "" {
		fmt.Fprintf(out, "\n%s\n", detail.Epic.Body)
	}
}

func printContainerStart(out io.Writer, result coordinator.ContainerStartResult) {
	for _, task := range result.Tasks {
		fmt.Fprintf(out, "%s\t%s", task.TaskID, task.Status)
		if task.RunID != "" {
			fmt.Fprintf(out, "\t%s", task.RunID)
		}
		if task.Error != "" {
			fmt.Fprintf(out, "\t%s", task.Error)
		}
		fmt.Fprintln(out)
	}
}

func printWorkItemResponse(out io.Writer, response contract.WorkItemResponse, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(out, "%s%s\t%s\t%s\t%s\n", indent, response.Item.ID, response.Item.Kind, response.Item.State.Status, response.Item.Title)
	for _, child := range response.Children {
		printWorkItemResponse(out, child, depth+1)
	}
}

func printWorkItemRelations(out io.Writer, itemID string, relations []coordinator.WorkItemRelation) {
	if len(relations) == 0 {
		fmt.Fprintf(out, "%s has no relations\n", itemID)
		return
	}
	for _, relation := range relations {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			relation.Source.ID, relation.Source.Kind, relation.Kind, relation.Target.ID, relation.Target.Kind)
	}
}

func taskLifecycleLabel(task coordinator.Task) string {
	if task.State == nil {
		return "unscheduled"
	}
	return string(*task.State)
}

func printTaskAttachmentLine(out io.Writer, attachment coordinator.TaskAttachment) {
	fmt.Fprintf(out, "%s\t%s\t%s\t%d\n", attachment.ID, attachment.Stage, attachment.Filename, attachment.SizeBytes)
}

func printTaskDetail(out io.Writer, task coordinator.Task) {
	printTaskLine(out, task)
	if task.Body != "" {
		fmt.Fprintf(out, "\n%s\n", task.Body)
	}
}

func printTaskRelations(out io.Writer, taskID string, relations []coordinator.TaskRelation) {
	if len(relations) == 0 {
		fmt.Fprintf(out, "%s has no relations\n", taskID)
		return
	}
	for _, relation := range relations {
		fmt.Fprintf(out, "%s\t%s\t%s\n", relation.SourceTaskID, relation.Kind, relation.TargetTaskID)
	}
}

func printBoard(out io.Writer, result coordinator.BoardResult) {
	blocked := make(map[string]bool, len(result.BlockedIDs))
	for _, id := range result.BlockedIDs {
		blocked[id] = true
	}
	printBoardLane(out, "unscheduled", result.Board.Unscheduled, result.LaneStates, result.WaitReasons, blocked)
	printBoardLane(out, "scheduled", result.Board.Scheduled, result.LaneStates, result.WaitReasons, blocked)
	printBoardLane(out, "in_progress", result.Board.InProgress, result.LaneStates, result.WaitReasons, blocked)
}

func printBoardLane(out io.Writer, name string, tasks []coordinator.Task, states map[string]coordinator.LaneState, waitReasons map[string]coordinator.WaitReason, blocked map[string]bool) {
	fmt.Fprintf(out, "%s:\n", name)
	for _, task := range tasks {
		// A semantic label is emitted at most once: the lane state, wait reason,
		// and blocked membership can all agree (a blocked in-progress task
		// carries LaneStateBlocked, WaitReasonBlocked, and a BlockedIDs entry),
		// and repeating [blocked] three times is noise, not signal.
		seen := make(map[string]bool, 3)
		annotations := ""
		appendAnnotation := func(label string) {
			if label == "" || seen[label] {
				return
			}
			seen[label] = true
			annotations += "\t[" + label + "]"
		}
		// The default working lane is not an annotation: a working in-progress
		// task shows no bracket. Only genuine substates that differ from the
		// lifecycle label (awaiting_worker, blocked, held) annotate the line.
		if state, ok := states[task.ID]; ok && state != coordinator.LaneStateWorking && string(state) != taskLifecycleLabel(task) {
			appendAnnotation(strings.ReplaceAll(string(state), "_", " "))
		}
		if reason := waitReasons[task.ID]; reason != "" {
			appendAnnotation(strings.ReplaceAll(string(reason), "_", " "))
		}
		if blocked[task.ID] {
			appendAnnotation("blocked")
		}
		fmt.Fprintf(out, "  %s\t%s\t%s%s\n", task.ID, taskLifecycleLabel(task), task.Title, annotations)
	}
}

func printCheckLine(out io.Writer, check coordinator.Check) {
	exitCode := ""
	if check.ExitCode != nil {
		exitCode = strconv.Itoa(*check.ExitCode)
	}
	fmt.Fprintf(out, "%s\t%s\t%s\trequired=%t\texit_code=%s\treporter=%s\n",
		check.Name,
		check.Kind,
		check.Verdict,
		check.Required,
		exitCode,
		check.Reporter,
	)
}

func printThreadLine(out io.Writer, thread coordinator.ReviewThread) {
	claim := ""
	if thread.ClaimKind != nil {
		claim = string(*thread.ClaimKind)
	}
	fmt.Fprintf(out, "%s\t%s\t%s:%d\tclaim=%s\tcomments=%d\n",
		thread.ID,
		thread.State,
		thread.FilePath,
		thread.Line,
		claim,
		len(thread.Comments),
	)
}

func printWorkerLine(out io.Writer, worker flowworker.Worker) {
	fmt.Fprintf(out, "%s\t%s\tbucket=%s\tlabels=%s\n",
		worker.ID,
		worker.Status,
		string(worker.CapacityBucket),
		formatLabels(worker.Labels),
	)
}

func printJobLine(out io.Writer, job flowworker.Job) {
	taskID := ""
	if job.TaskID != nil {
		taskID = *job.TaskID
	}
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\ttask=%s\tpriority=%d\n",
		job.ID,
		job.State,
		job.Role,
		job.CapacityBucket,
		taskID,
		job.Priority,
	)
}

func parseScheduleStates(value string) []coordinator.ScheduleState {
	values := parseCSV(value)
	states := make([]coordinator.ScheduleState, 0, len(values))
	for _, item := range values {
		states = append(states, coordinator.ScheduleState(item))
	}

	return states
}

func parseTriageStates(value string) []coordinator.TriageState {
	values := parseCSV(value)
	states := make([]coordinator.TriageState, 0, len(values))
	for _, item := range values {
		states = append(states, coordinator.TriageState(item))
	}

	return states
}

func parseCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			values = append(values, trimmed)
		}
	}

	return values
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}

	return strings.Join(parts, ",")
}
