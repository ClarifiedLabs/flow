package main

import (
	"bytes"
	"context"
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

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
	"github.com/ClarifiedLabs/flow/internal/harness"
	flowlog "github.com/ClarifiedLabs/flow/internal/logging"
	flowprompt "github.com/ClarifiedLabs/flow/internal/prompt"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/version"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
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
	slog.Debug("flow command start", "command", flowlog.CommandName(args))

	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "--version", "version":
		fmt.Fprintf(stdout, "flow %s\n", version.Current())
		return 0
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "issue":
		return runIssue(args[1:], stdout, stderr)
	case "board":
		return runBoard(args[1:], stdout, stderr)
	case "checks":
		return runChecks(args[1:], stdout, stderr)
	case "transitions":
		return runTransitions(args[1:], stdout, stderr)
	case "review":
		return runReview(args[1:], stdout, stderr)
	case "workers":
		return runWorkers(args[1:], stdout, stderr)
	case "jobs":
		return runJobs(args[1:], stdout, stderr)
	case "attach":
		return runAttach(args[1:], stdout, stderr)
	case "session":
		return runSession(args[1:], stdout, stderr)
	case "hook":
		return runHook(args[1:], stdout, stderr)
	case "fetch-prompt":
		return runFetchPrompt(args[1:], stdout, stderr)
	case "comment":
		return runComment(args[1:], stdout, stderr)
	case "thread":
		return runThread(args[1:], stdout, stderr)
	case "handoff":
		return runHandoff(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "ask":
		return runAsk(args[1:], stdout, stderr)
	case "ready":
		return runReady(args[1:], stdout, stderr)
	case "merge":
		return runMerge(args[1:], stdout, stderr)
	case "ui":
		return runUI(args[1:], stdout, stderr)
	case "reconcile":
		return runReconcile(args[1:], stdout, stderr)
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
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var dbPath string
	var clientConfigPath string
	flags.StringVar(&dbPath, "db", "", "coordinator global SQLite database path to initialize")
	flags.StringVar(&clientConfigPath, "config", "", "client config JSON path")

	if err := flags.Parse(args); err != nil {
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
	fmt.Fprintf(stdout, "protocol: %s\n", clientCfg.ProtocolVersion)
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
	apiFlags := addAPIFlags(flags)
	flags.StringVar(&repoPath, "repo", ".", "git worktree to register as a Flow project")
	flags.StringVar(&name, "name", "", "project name (default: repo directory name)")
	flags.StringVar(&baseBranch, "base", "", "base branch to seed and protect (default: current branch)")
	flags.StringVar(&exchangeName, "exchange-name", "", "git remote name for the Flow exchange (default flow)")
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

	seed, err := flowgit.SeedExchangeFromWorktree(context.Background(), flowgit.SeedOptions{
		RepoPath:     repoRoot,
		BaseBranch:   project.BaseBranch,
		ExchangeName: project.ExchangeName,
		ExchangeURL:  project.ExchangeURL,
		Token:        clientCfg.Token,
	})
	if err != nil {
		fmt.Fprintf(stderr, "seed exchange remote: %v\n", err)
		return 1
	}
	if seed.Warning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", seed.Warning)
	}
	credentialStored, credentialCommand, err := approveGitCredential(repoRoot, project.ExchangeURL, clientCfg.Token)
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
	fmt.Fprintf(stdout, "exchange_remote: %s -> %s\n", project.ExchangeName, project.ExchangeURL)
	fmt.Fprintf(stdout, "client_config: %s\n", configPath)
	fmt.Fprintln(stdout, "next:")
	if credentialCommand != "" {
		fmt.Fprintln(stdout, "  # optional: configure a git credential helper, then store the Flow Git credential")
		fmt.Fprintf(stdout, "  %s\n", credentialCommand)
	}
	fmt.Fprintln(stdout, "  flow issue create --title \"...\"   # project auto-detected from this repo")
	fmt.Fprintln(stdout, "  flow board")
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

	cfg, err := config.LocalClientConfig(clientCfg.DataDir, clientCfg.ServerURL, clientCfg.Token, clientCfg.ProtocolVersion)
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

func runIssue(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printIssueUsage(stderr)
		return 2
	}

	switch args[0] {
	case "create":
		return runIssueCreate(args[1:], stdout, stderr)
	case "attach":
		return runIssueAttach(args[1:], stdout, stderr)
	case "list":
		return runIssueList(args[1:], stdout, stderr)
	case "show":
		return runIssueShow(args[1:], stdout, stderr)
	case "edit":
		return runIssueEdit(args[1:], stdout, stderr)
	case "schedule":
		return runIssueSchedule(args[1:], stdout, stderr)
	case "state":
		return runIssueState(args[1:], stdout, stderr)
	case "reset":
		return runIssueReset(args[1:], stdout, stderr)
	case "close":
		return runIssueClose(args[1:], stdout, stderr)
	case "triage":
		return runIssueTriage(args[1:], stdout, stderr)
	case "link":
		return runIssueLink(args[1:], stdout, stderr)
	case "unlink":
		return runIssueUnlink(args[1:], stdout, stderr)
	case "plan":
		return runIssuePlan(args[1:], stdout, stderr)
	case "reply":
		return runIssueReply(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown issue command: %s\n\n", args[0])
		printIssueUsage(stderr)
		return 2
	}
}

func runIssueCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	var acceptanceCriteria string
	var priority int
	var requiresHumanReview optionalBoolFlag
	var autoMerge optionalBoolFlag
	var planMode bool
	var agentHarness string
	var attachmentFiles stringSliceFlag
	var codexArgs stringSliceFlag
	var claudeArgs stringSliceFlag
	var harnessArgs stringSliceFlag
	flags.StringVar(&title, "title", "", "issue title")
	flags.StringVar(&body, "body", "", "issue body")
	flags.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "acceptance criteria")
	flags.IntVar(&priority, "priority", 0, "issue priority")
	flags.Var(&requiresHumanReview, "requires-human-review", "require human review before merge")
	flags.Var(&autoMerge, "auto-merge", "auto merge when review becomes approved")
	flags.BoolVar(&planMode, "plan-mode", false, "ask the agent to plan and wait for approval before making changes")
	flags.StringVar(&agentHarness, "agent-harness", "", "agent harness: codex, claude, or harness")
	flags.Var(&attachmentFiles, "file", "file to attach to the initial issue prompt (repeatable)")
	flags.Var(&codexArgs, "codex-arg", "additional argv token for generated Codex harness commands (repeatable)")
	flags.Var(&claudeArgs, "claude-arg", "additional argv token for generated Claude harness commands (repeatable)")
	flags.Var(&harnessArgs, "harness-arg", "additional argv token for generated Harness commands (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	agentHarness = harness.NormalizeName(agentHarness)
	if err := harness.ValidateAgentName(agentHarness); err != nil {
		fmt.Fprintf(stderr, "invalid agent harness: %v\n", err)
		return 2
	}
	additionalArgs, err := harness.NormalizeArgs(harness.Args{
		Codex:   codexArgs.Values,
		Claude:  claudeArgs.Values,
		Harness: harnessArgs.Values,
	})
	if err != nil {
		fmt.Fprintf(stderr, "invalid harness args: %v\n", err)
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	input := flowclient.CreateIssueInput{
		Title:              title,
		Body:               body,
		AcceptanceCriteria: acceptanceCriteria,
		Priority:           priority,
		PlanMode:           planMode,
		AgentHarness:       agentHarness,
		HarnessArgs:        additionalArgs,
	}
	if requiresHumanReview.Provided {
		input.RequiresHumanReview = &requiresHumanReview.Value
	}
	if autoMerge.Provided {
		input.AutoMerge = &autoMerge.Value
	}
	issue, err := client.CreateIssue(input)
	if err != nil {
		fmt.Fprintf(stderr, "create issue: %v\n", err)
		return 1
	}
	printIssueLine(stdout, issue)
	for _, filePath := range attachmentFiles.Values {
		attachment, err := uploadIssueAttachmentFile(client, issue.ID, filePath, coordinator.IssueAttachmentStageInitial)
		if err != nil {
			fmt.Fprintf(stderr, "attach file to %s: %v\n", issue.ID, err)
			return 1
		}
		printIssueAttachmentLine(stdout, attachment)
	}

	return 0
}

func runIssueAttach(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue attach", flag.ContinueOnError)
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
		fmt.Fprintln(stderr, "issue id is required")
		return 2
	}
	if strings.TrimSpace(filePath) == "" {
		fmt.Fprintln(stderr, "--file is required")
		return 2
	}
	attachmentStage, err := issueAttachmentStageFromCLI(stage)
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
	client, issueRef := scopeClientForRef(client, flags.Arg(0))
	attachment, err := uploadIssueAttachmentFile(client, issueRef, filePath, attachmentStage)
	if err != nil {
		fmt.Fprintf(stderr, "attach file: %v\n", err)
		return 1
	}

	printIssueAttachmentLine(stdout, attachment)
	return 0
}

func runIssueList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var scheduleState string
	var triageState string
	var tag string
	flags.StringVar(&scheduleState, "schedule-state", "", "comma-separated schedule states")
	flags.StringVar(&triageState, "triage-state", "", "comma-separated triage states")
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
	issues, err := client.ListIssues(flowclient.IssueFilter{
		ScheduleStates: parseScheduleStates(scheduleState),
		TriageStates:   parseTriageStates(triageState),
		TagSlugs:       parseCSV(tag),
	})
	if err != nil {
		fmt.Fprintf(stderr, "list issues: %v\n", err)
		return 1
	}

	for _, issue := range issues {
		printIssueLine(stdout, issue)
	}
	return 0
}

func runIssueShow(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "issue id is required")
		return 2
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, issueRef := scopeClientForRef(client, flags.Arg(0))
	issue, err := client.GetIssue(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "show issue: %v\n", err)
		return 1
	}

	printIssueDetail(stdout, issue)
	return 0
}

func runIssuePlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: flow issue plan approve|reject [flags] ISSUE_ID")
		return 2
	}
	switch args[0] {
	case "approve":
		return runIssuePlanApprove(args[1:], stdout, stderr)
	case "reject":
		return runIssuePlanReject(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown issue plan command: %s\n", args[0])
		return 2
	}
}

func runIssuePlanApprove(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue plan approve", 1, "usage: flow issue plan approve [flags] ISSUE_ID")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.ApprovePlan(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "approve plan: %v\n", err)
		return 1
	}
	printIssueLine(stdout, issue)
	return 0
}

func runIssuePlanReject(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue plan reject", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var comments string
	flags.StringVar(&comments, "comments", "", "rejection comments")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: flow issue plan reject [flags] ISSUE_ID [COMMENTS]")
		return 2
	}
	if strings.TrimSpace(comments) == "" && flags.NArg() > 1 {
		comments = strings.TrimSpace(strings.Join(flags.Args()[1:], " "))
	}
	if strings.TrimSpace(comments) == "" {
		fmt.Fprintln(stderr, "rejection comments are required")
		return 2
	}
	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, issueRef := scopeClientForRef(client, flags.Arg(0))
	issue, err := client.RejectPlan(issueRef, flowclient.RejectPlanInput{Comments: comments})
	if err != nil {
		fmt.Fprintf(stderr, "reject plan: %v\n", err)
		return 1
	}
	printIssueLine(stdout, issue)
	return 0
}

func runIssueReply(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue reply", flag.ContinueOnError)
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
		fmt.Fprintln(stderr, "usage: flow issue reply [flags] ISSUE_ID [MESSAGE]")
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
	client, issueRef := scopeClientForRef(client, flags.Arg(0))
	var statusLogIDPtr *int64
	if statusLogID > 0 {
		statusLogIDPtr = &statusLogID
	}
	messageResult, queued, err := client.ReplyToIssue(issueRef, flowclient.ReplyToIssueInput{
		Message:     message,
		StatusLogID: statusLogIDPtr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "reply issue: %v\n", err)
		return 1
	}
	if queued {
		fmt.Fprintf(stdout, "%s\t%s\tqueued\n", messageResult.ID, messageResult.SessionID)
	} else {
		fmt.Fprintln(stdout, "recorded")
	}
	return 0
}

func runIssueEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("issue edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)

	var title string
	var body string
	var acceptanceCriteria string
	var priority string
	var requiresHumanReview optionalBoolFlag
	var autoMerge optionalBoolFlag
	var planMode optionalBoolFlag
	var agentHarness optionalStringFlag
	var codexArgs stringSliceFlag
	var claudeArgs stringSliceFlag
	var harnessArgs stringSliceFlag
	var clearCodexArgs bool
	var clearClaudeArgs bool
	var clearHarnessArgs bool
	flags.StringVar(&title, "title", "", "new issue title")
	flags.StringVar(&body, "body", "", "new issue body")
	flags.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "new acceptance criteria")
	flags.StringVar(&priority, "priority", "", "new issue priority")
	flags.Var(&requiresHumanReview, "requires-human-review", "require human review before merge")
	flags.Var(&autoMerge, "auto-merge", "auto merge when review becomes approved")
	flags.Var(&planMode, "plan-mode", "ask the agent to plan and wait for approval before making changes")
	flags.Var(&agentHarness, "agent-harness", "agent harness: codex, claude, or harness")
	flags.Var(&codexArgs, "codex-arg", "set issue-level argv token for generated Codex harness commands (repeatable)")
	flags.Var(&claudeArgs, "claude-arg", "set issue-level argv token for generated Claude harness commands (repeatable)")
	flags.Var(&harnessArgs, "harness-arg", "set issue-level argv token for generated Harness commands (repeatable)")
	flags.BoolVar(&clearCodexArgs, "clear-codex-args", false, "clear issue-level Codex harness args")
	flags.BoolVar(&clearClaudeArgs, "clear-claude-args", false, "clear issue-level Claude harness args")
	flags.BoolVar(&clearHarnessArgs, "clear-harness-args", false, "clear issue-level Harness args")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "issue id is required")
		return 2
	}

	input := flowclient.EditIssueInput{}
	if title != "" {
		input.Title = &title
	}
	if body != "" {
		input.Body = &body
	}
	if acceptanceCriteria != "" {
		input.AcceptanceCriteria = &acceptanceCriteria
	}
	if priority != "" {
		parsedPriority, err := strconv.Atoi(priority)
		if err != nil {
			fmt.Fprintf(stderr, "invalid priority: %v\n", err)
			return 2
		}
		input.Priority = &parsedPriority
	}
	if requiresHumanReview.Provided {
		input.RequiresHumanReview = &requiresHumanReview.Value
	}
	if autoMerge.Provided {
		input.AutoMerge = &autoMerge.Value
	}
	if planMode.Provided {
		input.PlanMode = &planMode.Value
	}
	if agentHarness.Provided {
		value := harness.NormalizeName(agentHarness.Value)
		if err := harness.ValidateAgentName(value); err != nil {
			fmt.Fprintf(stderr, "invalid agent harness: %v\n", err)
			return 2
		}
		input.AgentHarness = &value
	}
	harnessArgsPatch, err := harnessArgsPatchFromFlags(codexArgs, clearCodexArgs, claudeArgs, clearClaudeArgs, harnessArgs, clearHarnessArgs)
	if err != nil {
		fmt.Fprintf(stderr, "invalid harness args: %v\n", err)
		return 2
	}
	if harnessArgsPatch != nil {
		input.HarnessArgs = harnessArgsPatch
	}

	applySessionEnvironment(apiFlags, nil)
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	client, issueRef := scopeClientForRef(client, flags.Arg(0))
	issue, err := client.EditIssue(issueRef, input)
	if err != nil {
		fmt.Fprintf(stderr, "edit issue: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueSchedule(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue schedule", 2, "usage: flow issue schedule [flags] ISSUE_ID backlog|up_next")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.ScheduleIssue(issueRef, coordinator.ScheduleState(parsed.flags.Arg(1)))
	if err != nil {
		fmt.Fprintf(stderr, "schedule issue: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueState(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue state", 2, "usage: flow issue state [flags] ISSUE_ID triage|backlog|up_next|closed|rejected")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.SetIssueState(issueRef, coordinator.IssueState(parsed.flags.Arg(1)))
	if err != nil {
		fmt.Fprintf(stderr, "set issue state: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueReset(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue reset", 1, "usage: flow issue reset [flags] ISSUE_ID")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.ResetIssue(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "reset issue: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueClose(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue close", 1, "issue id is required")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.CloseIssue(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "close issue: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueTriage(args []string, stdout, stderr io.Writer) int {
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "issue triage", 2, "usage: flow issue triage [flags] ISSUE_ID accepted|rejected")
	if code != 0 {
		return code
	}
	issue, err := parsed.client.TriageIssue(issueRef, coordinator.TriageState(parsed.flags.Arg(1)))
	if err != nil {
		fmt.Fprintf(stderr, "triage issue: %v\n", err)
		return 1
	}

	printIssueLine(stdout, issue)
	return 0
}

func runIssueLink(args []string, stdout, stderr io.Writer) int {
	parsed, sourceRef, targetRef, kind, code := parseIssueRelationCommand(args, stderr, "issue link")
	if code != 0 {
		return code
	}
	if err := parsed.client.LinkIssues(sourceRef, kind, targetRef); err != nil {
		fmt.Fprintf(stderr, "link issues: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", sourceRef, kind, targetRef)
	return 0
}

func runIssueUnlink(args []string, stdout, stderr io.Writer) int {
	parsed, sourceRef, targetRef, kind, code := parseIssueRelationCommand(args, stderr, "issue unlink")
	if code != 0 {
		return code
	}
	if err := parsed.client.UnlinkIssues(sourceRef, kind, targetRef); err != nil {
		fmt.Fprintf(stderr, "unlink issues: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", sourceRef, kind, targetRef)
	return 0
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
		fmt.Fprintf(stderr, "load board: %v\n", err)
		return 1
	}

	printBoard(stdout, board)
	return 0
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
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "checks", 1, "usage: flow checks [flags] ISSUE_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.ListChecks(issueRef)
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
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "transitions", 1, "usage: flow transitions [flags] ISSUE_ID")
	if code != 0 {
		return code
	}
	transitions, err := parsed.client.ListTransitions(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "list transitions: %v\n", err)
		return 1
	}

	for _, entry := range transitions {
		printTransitionLine(stdout, entry)
	}
	return 0
}

func printTransitionLine(out io.Writer, entry coordinator.TransitionLogEntry) {
	from := entry.FromPhase
	if from == "" {
		from = "-"
	}
	actor := entry.Actor
	if actor == "" {
		actor = "-"
	}
	fmt.Fprintf(out, "%d\t%s\t%s -> %s\t%s\tactor=%s\t%s\n",
		entry.Seq,
		entry.EventKind,
		from,
		entry.ToPhase,
		entry.GuardResult,
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
	parsed, issueRef, code := parseScopedIssueAPICommand(args, stderr, "review run", 1, "usage: flow review run [flags] ISSUE_ID")
	if code != 0 {
		return code
	}
	result, err := parsed.client.RunReview(issueRef)
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
	if apiFlags.protocolVersion == "" {
		apiFlags.protocolVersion = os.Getenv("FLOW_PROTOCOL_VERSION")
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
	if len(args) == 0 {
		printSessionUsage(stderr)
		return 2
	}

	switch args[0] {
	case "event":
		return runSessionEvent(args[1:], stdout, stderr)
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
	if len(args) == 0 {
		printHookUsage(stderr)
		return 2
	}

	switch args[0] {
	case harness.Codex, harness.Claude, harness.Harness:
		if len(args) > 1 {
			switch args[1] {
			case "ingest":
				return runHookIngest(args[0], args[2:], stdout, stderr)
			case "prepush":
				return runHookPrepush(args[0], args[2:], stdout, stderr)
			case "commit-msg":
				return runHookCommitMsg(args[0], args[2:], stdout, stderr)
			}
		}
		return runHookEvent(args[0], args[1:], stdout, stderr)
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
	flags.StringVar(&harness, "harness", "", "prompt harness: codex, claude, harness, or agents")
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
	if err := enrichPromptIssueContext(&input, apiFlags); err != nil {
		fmt.Fprintf(stderr, "fetch prompt: warning: %v; continuing without issue context\n", err)
	}
	rendered, err := flowprompt.Build(input)
	if err != nil {
		fmt.Fprintf(stderr, "fetch prompt: %v\n", err)
		return 2
	}

	fmt.Fprintln(stdout, rendered)
	return 0
}

func enrichPromptIssueContext(input *flowprompt.Input, apiFlags *apiFlagValues) error {
	issueID := strings.TrimSpace(input.IssueID)
	if issueID == "" || !promptIssueFetchConfigured(apiFlags) {
		return nil
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		return fmt.Errorf("create client for issue context: %w", err)
	}
	issue, statusLog, err := client.GetIssueWithStatus(issueID)
	if err != nil {
		return fmt.Errorf("fetch issue context: %w", err)
	}

	input.IssueTitle = issue.Title
	input.IssueBody = issue.Body
	input.IssueAcceptanceCriteria = issue.AcceptanceCriteria
	input.PlanMode = issue.PlanMode && issue.PlanApprovedAt == nil
	if issue.PlanApprovedAt != nil {
		input.ApprovedPlan = issue.PlanBody
	}
	input.HumanAttentionContext = humanAttentionPromptContext(statusLog)
	// Inject the previous session's handoff (the coordinator is the sole store
	// now that .handoff.md is no longer committed) so the next author fix round
	// and verifier have the prior context they used to read from the worktree.
	// Best-effort: prior handoff is supplementary context, so a fetch failure
	// must never strip the rest of the prompt — it only skips this section.
	if changeID := strings.TrimSpace(input.ChangeID); changeID != "" {
		leaseID := strings.TrimSpace(os.Getenv("FLOW_LEASE_ID"))
		if _, content, found, err := client.GetHandoff(changeID, leaseID); err != nil {
			slog.Debug("skip prior handoff injection", "change_id", changeID, "error", err)
		} else if found {
			input.PriorHandoff = content
		}
	}
	if promptInputRole(*input) == flowprompt.RoleAuthor {
		if err := enrichPromptAuthorReviewContext(input, client); err != nil {
			return err
		}
	}
	if promptInputRole(*input) == flowprompt.RoleReviewer {
		enrichPromptCompletionAssessment(input, client)
	}
	return nil
}

// enrichPromptCompletionAssessment marks a reviewer prompt as a Mode-B recovery
// review when its own check carries the completion-assessment marker (stamped by
// the coordinator when a crashed author with a saved handoff was routed to a
// targeted review rather than a blind relaunch). It is best-effort: the marker
// is supplementary guidance, so a lookup failure must never strip the prompt.
func enrichPromptCompletionAssessment(input *flowprompt.Input, client *flowclient.Client) {
	issueID := strings.TrimSpace(input.IssueID)
	checkName := strings.TrimSpace(input.CheckName)
	if issueID == "" || checkName == "" {
		return
	}
	result, err := client.GetCheck(issueID, checkName)
	if err != nil {
		slog.Debug("skip completion-assessment detection", "issue_id", issueID, "check", checkName, "error", err)
		return
	}
	if strings.TrimSpace(result.Check.Details) == coordinator.CompletionAssessmentCheckMarker {
		input.CompletionAssessment = true
	}
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
	issueID := strings.TrimSpace(input.IssueID)
	if issueID == "" {
		return nil
	}

	checks, err := client.ListChecks(issueID)
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

func promptIssueFetchConfigured(apiFlags *apiFlagValues) bool {
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
	if len(args) == 0 {
		printThreadUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runThreadList(args[1:], stdout, stderr)
	case "reply":
		return runThreadReply(args[1:], stdout, stderr)
	case "claim":
		return runThreadClaim(args[1:], stdout, stderr)
	case "certify":
		return runThreadVerify("certify", args[1:], stdout, stderr)
	case "reopen":
		return runThreadVerify("reopen", args[1:], stdout, stderr)
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
	input.IssueID = os.Getenv("FLOW_ISSUE_ID")
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
	message := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if message == "" {
		fmt.Fprintln(stderr, "status message is required")
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
	status, err := client.WriteSessionStatus(sessionID, message, kind)
	if err != nil {
		fmt.Fprintf(stderr, "write status: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%d\t%s\t%s\n", status.ID, status.IssueID, status.Message)
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

	planningSession := strings.TrimSpace(os.Getenv("FLOW_SESSION_PURPOSE")) == "planning"

	// Read and validate the handoff up front, before touching the remote, so a
	// malformed handoff fails fast and the agent can fix it and re-run.
	handoffBody := ""
	if !planningSession {
		body, err := readReadyHandoff(handoffFile)
		if err != nil {
			fmt.Fprintf(stderr, "read handoff: %v\n", err)
			return 1
		}
		if err := handoff.Validate(body); err != nil {
			if !allowMissingHandoff {
				fmt.Fprintf(stderr, "handoff validation: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "warning: handoff validation skipped: %v\n", err)
		}
		handoffBody = body
	}

	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	headSHA := ""
	if !planningSession {
		headSHA, err = currentGitSHA()
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
		// overwrites the same snapshot, so this is idempotent too.
		if changeID := strings.TrimSpace(os.Getenv("FLOW_CHANGE_ID")); changeID != "" && strings.TrimSpace(handoffBody) != "" {
			if _, err := client.PutHandoff(changeID, flowclient.PutHandoffInput{
				Content: handoffBody,
				HeadSHA: headSHA,
			}); err != nil {
				fmt.Fprintf(stderr, "submit handoff: %v\n", err)
				return 1
			}
		}
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

func runMerge(args []string, stdout, stderr io.Writer) int {
	parsed, code := parseAPICommand(args, stderr, "merge", 1, "usage: flow merge [flags] ISSUE_ID|CHANGE_ID")
	if code != 0 {
		return code
	}
	client, target := scopeClientForRef(parsed.client, strings.TrimSpace(parsed.flags.Arg(0)))
	var result coordinator.MergeResult
	var err error
	if strings.HasPrefix(target, "ch-") {
		result, err = client.MergeChange(target)
	} else {
		result, err = client.MergeIssue(target)
	}
	if err != nil {
		fmt.Fprintf(stderr, "merge: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", result.Issue.ID, result.Change.ID, result.MergeSHA, result.HeadSHA)
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
  flow [--log-level LEVEL] COMMAND
  flow init [--repo PATH] [--name NAME] [--base BRANCH]
  flow doctor [--db PATH] [--config PATH]
  flow issue create --title TITLE [--agent-harness codex|claude|harness] [--file PATH]
  flow issue attach ISSUE_ID --file PATH [--stage initial|author|reviewer|verifier]
  flow issue list
  flow issue show [--project PROJECT] ISSUE_ID|PROJECT/ISSUE_ID
  flow issue plan approve|reject ISSUE_ID
  flow issue reply ISSUE_ID MESSAGE
  flow issue state ISSUE_ID triage|backlog|up_next|closed|rejected
  flow board
  flow checks ISSUE_ID
  flow transitions ISSUE_ID
  flow review run ISSUE_ID
  flow workers
  flow jobs
  flow ui
  flow attach [--job] SESSION_ID|JOB_ID
  flow session event working|waiting
  flow hook codex|claude|harness EVENT|ingest
  flow fetch-prompt [--role author|reviewer|verifier] [--harness codex|claude|harness|agents]
  flow comment SHA:FILE:LINE BODY
  flow thread reply|claim|certify|reopen
  flow handoff write [flags]
  flow status MESSAGE
  flow ask QUESTION
  flow ready [--handoff-file PATH] [--allow-missing-handoff]   (handoff piped on stdin)
  flow merge ISSUE_ID|CHANGE_ID
  flow reconcile
  flow --version

Global flags:
  --log-level LEVEL   structured log level: debug, info, warn, error, or off (overrides LOG_LEVEL)

API override flags on owner commands:
  --server URL        coordinator server URL
  --token TOKEN       bearer token
  --project PROJECT   project id or name
`)
}

func printIssueUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow issue create --title TITLE [--agent-harness codex|claude|harness] [--file PATH]
  flow issue attach [flags] ISSUE_ID
  flow issue list
  flow issue show [flags] ISSUE_ID
  flow issue edit [--agent-harness codex|claude|harness] [flags] ISSUE_ID
  flow issue plan approve [flags] ISSUE_ID
  flow issue plan reject [flags] ISSUE_ID [COMMENTS]
  flow issue reply [flags] ISSUE_ID [MESSAGE]
  flow issue schedule [flags] ISSUE_ID backlog|up_next
  flow issue state [flags] ISSUE_ID triage|backlog|up_next|closed|rejected
  flow issue reset [flags] ISSUE_ID
  flow issue close [flags] ISSUE_ID
  flow issue triage [flags] ISSUE_ID accepted|rejected
  flow issue link [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
  flow issue unlink [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID
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
  flow review run [flags] ISSUE_ID
`)
}

func printSessionUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow session event [flags] working|waiting
`)
}

func printHookUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  flow hook codex [flags] start|stop|working|waiting
  flow hook codex ingest [flags]
  flow hook claude [flags] start|stop|notification|working|waiting
  flow hook claude ingest [flags]
  flow hook harness [flags] start|stop|working|waiting
  flow hook harness ingest [flags]

Client-side git hooks (installed into the agent worktree; never block git):
  flow hook codex|claude|harness prepush       (capture push + steer threads)
  flow hook codex|claude|harness commit-msg MSGFILE   (record Resolves: trailers)
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
	configPath      string
	serverURL       string
	token           string
	protocolVersion string
	project         string
}

type parsedAPICommand struct {
	flags  *flag.FlagSet
	client *flowclient.Client
}

type optionalBoolFlag struct {
	Value    bool
	Provided bool
}

func (f *optionalBoolFlag) Set(value string) error {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	f.Value = parsed
	f.Provided = true
	return nil
}

func (f *optionalBoolFlag) String() string {
	if f == nil || !f.Provided {
		return ""
	}
	return strconv.FormatBool(f.Value)
}

func (f *optionalBoolFlag) IsBoolFlag() bool {
	return true
}

type optionalStringFlag struct {
	Value    string
	Provided bool
}

func (f *optionalStringFlag) Set(value string) error {
	f.Value = strings.TrimSpace(value)
	f.Provided = true
	return nil
}

func (f *optionalStringFlag) String() string {
	if f == nil || !f.Provided {
		return ""
	}
	return f.Value
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

func harnessArgsPatchFromFlags(codexArgs stringSliceFlag, clearCodexArgs bool, claudeArgs stringSliceFlag, clearClaudeArgs bool, harnessArgFlag stringSliceFlag, clearHarnessArgs bool) (*harness.ArgsPatch, error) {
	var patch harness.ArgsPatch
	var provided bool
	if clearCodexArgs && len(codexArgs.Values) > 0 {
		return nil, errors.New("--codex-arg cannot be combined with --clear-codex-args")
	}
	if clearClaudeArgs && len(claudeArgs.Values) > 0 {
		return nil, errors.New("--claude-arg cannot be combined with --clear-claude-args")
	}
	if clearHarnessArgs && len(harnessArgFlag.Values) > 0 {
		return nil, errors.New("--harness-arg cannot be combined with --clear-harness-args")
	}
	if clearCodexArgs {
		values := []string{}
		patch.Codex = &values
		provided = true
	} else if len(codexArgs.Values) > 0 {
		values := append([]string(nil), codexArgs.Values...)
		patch.Codex = &values
		provided = true
	}
	if clearClaudeArgs {
		values := []string{}
		patch.Claude = &values
		provided = true
	} else if len(claudeArgs.Values) > 0 {
		values := append([]string(nil), claudeArgs.Values...)
		patch.Claude = &values
		provided = true
	}
	if clearHarnessArgs {
		values := []string{}
		patch.Harness = &values
		provided = true
	} else if len(harnessArgFlag.Values) > 0 {
		values := append([]string(nil), harnessArgFlag.Values...)
		patch.Harness = &values
		provided = true
	}
	if !provided {
		return nil, nil
	}
	normalized, err := harness.NormalizeArgsPatch(patch)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func addAPIFlags(flags *flag.FlagSet) *apiFlagValues {
	values := &apiFlagValues{}
	flags.StringVar(&values.configPath, "config", "", "client config JSON path")
	flags.StringVar(&values.serverURL, "server", "", "coordinator server URL")
	flags.StringVar(&values.token, "token", "", "owner bearer token")
	flags.StringVar(&values.project, "project", "", "project id or name (default: auto-detect from the current repo)")
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

	return parsedAPICommand{flags: flags, client: client}, 0
}

func parseScopedIssueAPICommand(args []string, stderr io.Writer, name string, positionalCount int, positionalError string) (parsedAPICommand, string, int) {
	parsed, code := parseAPICommand(args, stderr, name, positionalCount, positionalError)
	if code != 0 {
		return parsedAPICommand{}, "", code
	}
	client, issueRef := scopeClientForRef(parsed.client, parsed.flags.Arg(0))
	parsed.client = client
	return parsed, issueRef, 0
}

func parseIssueRelationCommand(args []string, stderr io.Writer, name string) (parsedAPICommand, string, string, coordinator.RelationKind, int) {
	parsed, code := parseAPICommand(args, stderr, name, 3, "usage: flow "+name+" [flags] SOURCE_ID blocks|parent_of|related_to TARGET_ID")
	if code != 0 {
		return parsedAPICommand{}, "", "", "", code
	}

	sourceProject, sourceID := splitQualifiedRef(parsed.flags.Arg(0))
	targetProject, targetID := splitQualifiedRef(parsed.flags.Arg(2))
	if sourceProject != "" && targetProject != "" && sourceProject != targetProject {
		fmt.Fprintln(stderr, "source and target issues must be in the same project")
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
	if values.protocolVersion != "" {
		cfg.ProtocolVersion = values.protocolVersion
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
	if values.protocolVersion == "" {
		values.protocolVersion = os.Getenv("FLOW_PROTOCOL_VERSION")
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

// splitQualifiedRef peels an optional "project/" qualifier off an issue or
// change ref: "myproj/i-0001" addresses i-0001 in project myproj.
func splitQualifiedRef(ref string) (string, string) {
	projectRef, id, found := strings.Cut(ref, "/")
	if !found {
		return "", ref
	}
	if strings.HasPrefix(id, "i-") || strings.HasPrefix(id, "ch-") {
		return projectRef, id
	}

	return "", ref
}

// scopeClientForRef rescopes the client when a positional ref carries a
// project qualifier.
func scopeClientForRef(client *flowclient.Client, ref string) (*flowclient.Client, string) {
	projectRef, id := splitQualifiedRef(ref)
	if projectRef == "" {
		return client, ref
	}

	return client.WithProject(projectRef), id
}

func uploadIssueAttachmentFile(client *flowclient.Client, issueID string, filePath string, stage coordinator.IssueAttachmentStage) (coordinator.IssueAttachment, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return coordinator.IssueAttachment{}, errors.New("attachment file path is required")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return coordinator.IssueAttachment{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return coordinator.IssueAttachment{}, err
	}
	if info.IsDir() {
		return coordinator.IssueAttachment{}, fmt.Errorf("%s is a directory", filePath)
	}
	contentType, err := detectAttachmentContentType(file, filePath)
	if err != nil {
		return coordinator.IssueAttachment{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return coordinator.IssueAttachment{}, err
	}

	return client.UploadIssueAttachment(issueID, flowclient.UploadIssueAttachmentInput{
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

func issueAttachmentStageFromCLI(stage string) (coordinator.IssueAttachmentStage, error) {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = strings.TrimSpace(os.Getenv("FLOW_ROLE"))
	}
	if stage == "" {
		stage = strings.TrimSpace(os.Getenv("FLOW_WORKER_ROLE"))
	}
	switch coordinator.IssueAttachmentStage(stage) {
	case coordinator.IssueAttachmentStageInitial:
		return coordinator.IssueAttachmentStageInitial, nil
	case coordinator.IssueAttachmentStageAuthor:
		return coordinator.IssueAttachmentStageAuthor, nil
	case coordinator.IssueAttachmentStageReviewer:
		return coordinator.IssueAttachmentStageReviewer, nil
	case coordinator.IssueAttachmentStageVerifier:
		return coordinator.IssueAttachmentStageVerifier, nil
	case "":
		return coordinator.IssueAttachmentStageInitial, nil
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

func printIssueLine(out io.Writer, issue coordinator.Issue) {
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", issue.ID, issue.ScheduleState, issue.TriageState, issue.Title)
}

func printIssueAttachmentLine(out io.Writer, attachment coordinator.IssueAttachment) {
	fmt.Fprintf(out, "%s\t%s\t%s\t%d\n", attachment.ID, attachment.Stage, attachment.Filename, attachment.SizeBytes)
}

func printIssueDetail(out io.Writer, issue coordinator.Issue) {
	printIssueLine(out, issue)
	if issue.AgentHarness != "" {
		fmt.Fprintf(out, "\nagent_harness: %s\n", issue.AgentHarness)
	}
	if !issue.HarnessArgs.Empty() {
		fmt.Fprintln(out, "harness_args:")
		printHarnessArgList(out, "codex", issue.HarnessArgs.Codex)
		printHarnessArgList(out, "claude", issue.HarnessArgs.Claude)
		printHarnessArgList(out, "harness", issue.HarnessArgs.Harness)
	}
	if issue.PlanMode {
		fmt.Fprintln(out, "plan_mode: true")
	}
	if issue.PlanBody != "" {
		fmt.Fprintf(out, "plan:\n%s\n", issue.PlanBody)
		if issue.PlanApprovedAt != nil {
			fmt.Fprintf(out, "plan_approved_at: %s\n", issue.PlanApprovedAt.Format(time.RFC3339Nano))
		}
	}
	if issue.Body != "" {
		fmt.Fprintf(out, "\n%s\n", issue.Body)
	}
	if issue.AcceptanceCriteria != "" {
		fmt.Fprintf(out, "\nacceptance_criteria:\n%s\n", issue.AcceptanceCriteria)
	}
}

func printHarnessArgList(out io.Writer, name string, args []string) {
	if len(args) == 0 {
		return
	}
	fmt.Fprintf(out, "  %s:\n", name)
	for _, arg := range args {
		fmt.Fprintf(out, "    - %q\n", arg)
	}
}

func printBoard(out io.Writer, result coordinator.BoardResult) {
	blocked := make(map[string]bool, len(result.BlockedIDs))
	for _, id := range result.BlockedIDs {
		blocked[id] = true
	}
	printBoardLane(out, "backlog", result.Board.Backlog, result.LaneStates, result.WaitReasons, blocked)
	printBoardLane(out, "up_next", result.Board.UpNext, result.LaneStates, result.WaitReasons, blocked)
	printBoardLane(out, "in_progress", result.Board.InProgress, result.LaneStates, result.WaitReasons, blocked)
	printBoardLane(out, "needs_attention", result.Board.NeedsAttention, result.LaneStates, result.WaitReasons, blocked)
}

func printBoardLane(out io.Writer, name string, issues []coordinator.Issue, states map[string]coordinator.LaneState, waitReasons map[string]coordinator.WaitReason, blocked map[string]bool) {
	fmt.Fprintf(out, "%s:\n", name)
	for _, issue := range issues {
		annotations := ""
		if state, ok := states[issue.ID]; ok && string(state) != string(issue.ScheduleState) && string(state) != string(issue.TriageState) {
			annotations += "\t[" + strings.ReplaceAll(string(state), "_", " ") + "]"
		}
		if reason := waitReasons[issue.ID]; reason != "" {
			annotations += "\t[" + strings.ReplaceAll(string(reason), "_", " ") + "]"
		}
		if blocked[issue.ID] {
			annotations += "\t[blocked]"
		}
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s%s\n", issue.ID, issue.ScheduleState, issue.TriageState, issue.Title, annotations)
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
	fmt.Fprintf(out, "%s\t%s\tpersistent_agent=%d\tephemeral=%d\tlabels=%s\n",
		worker.ID,
		worker.Status,
		worker.CapacityPersistentAgent,
		worker.CapacityEphemeral,
		formatLabels(worker.Labels),
	)
}

func printJobLine(out io.Writer, job flowworker.Job) {
	issueID := ""
	if job.IssueID != nil {
		issueID = *job.IssueID
	}
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\tissue=%s\tpriority=%d\n",
		job.ID,
		job.State,
		job.Role,
		job.CapacityBucket,
		issueID,
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
