package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create stdin temp file: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("write stdin temp file: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek stdin temp file: %v", err)
	}
	original := os.Stdin
	os.Stdin = file
	defer func() {
		os.Stdin = original
		_ = file.Close()
	}()
	fn()
}

func TestLogLevelFlagEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "debug", "--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestLogLevelEnvironmentEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestInvalidLogLevelFails(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "verbose", "--version"}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("exitCode = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "invalid log level") {
		t.Fatalf("stderr missing invalid log level error: %q", stderr.String())
	}
}

func TestFlowsListSummarizesGraphReviewAgents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/flows" {
			t.Fatalf("request = %s %s, want GET /v2/flows", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"flows":[{"id":"fl-quality","name":"quality","default":true,"start_node":"implement","nodes":[{"id":"fn-1","key":"implement","name":"Implement","kind":"agent","position":0,"config":{"agent":{"agent_def_id":"ad-author","workspace":"change","artifact":"change"}}},{"id":"fn-2","key":"review","name":"Review","kind":"change_review","position":1,"config":{"change_review":{"agents":[{"agent_def_id":"ad-review-default"},{"agent_def_id":"ad-review-advisory","required":false}]}}},{"id":"fn-3","key":"verify","name":"Verify","kind":"verify_change","position":2,"config":{"verify_change":{"agents":[{"agent_def_id":"ad-verify-blocking","required":true},{"agent_def_id":"ad-verify-advisory-1","required":false},{"agent_def_id":"ad-verify-advisory-2","required":false}]}}},{"id":"fn-4","key":"done","name":"Done","kind":"terminal","position":3,"config":{"terminal":{"resolution":"completed"}}}]}]}`)
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"flows", "list", "--server", server.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("flows list exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	want := "fl-quality\tquality*\tstart=implement\tnodes=4\treviewers=1 blocking/1 advisory\tverifiers=1 blocking/2 advisory\n"
	if stdout.String() != want {
		t.Fatalf("flows list output = %q, want %q", stdout.String(), want)
	}
}

func TestAgentDefsListGlobalUsesGlobalCatalog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/global/agent-defs" {
			t.Fatalf("request = %s %s, want GET /v2/global/agent-defs", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agent_defs":[{"id":"ad-global","name":"shared-reviewer","harness":"codex","model":"gpt-5","reasoning_effort":"high"}]}`)
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"agent-defs", "list", "--global", "--server", server.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("agent-defs list exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	want := "ad-global\tshared-reviewer\tcodex\tgpt-5 effort=high\tglobal\n"
	if stdout.String() != want {
		t.Fatalf("agent-defs list output = %q, want %q", stdout.String(), want)
	}
}

func TestAgentDefsGlobalAndProjectScopesConflict(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runAgentDefsList([]string{"--global", "--project", "demo"}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("agent-defs list exitCode = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--global and --project cannot be used together") {
		t.Fatalf("stderr = %q, want conflicting scope error", stderr.String())
	}
}

func TestDoctorInitializesDatabase(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "global.db")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"doctor", "--db", dbPath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}

	output := stdout.String()
	// flow doctor opens the coordinator-wide (global) database, whose schema
	// is applied by the single global migration.
	for _, want := range []string{"flow doctor", "sqlite: ok", "0001_global_init"} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, output)
		}
	}

	store, err := db.OpenGlobal(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer store.Close()

	migrations, err := store.AppliedMigrations(context.Background())
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertMigrationsInclude(t, migrations, "0001_global_init")
}

func TestFetchPromptUsesWorkerRoleEnvironment(t *testing.T) {
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_TASK_ID", "t-demo-0001")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_CHECK_NAME", "reviewer")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Flow role instructions (flow-reviewer):",
		"# Flow Reviewer",
		"Task: t-demo-0001",
		"Change: ch-1",
		"Check: reviewer",
		"Use flow comment",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptIncludesTaskDetailsFromAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	task, err := client.CreateTask(flowclient.CreateTaskInput{
		Title: "Prompt details task",
		Body:  "Build the prompt with complete task context.\n\nThe agent must be able to start work without calling task show.",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_TASK_ID", task.ID)
	t.Setenv("FLOW_COORDINATOR_URL", serverURL)
	t.Setenv("FLOW_SESSION_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Task: " + task.ID,
		"Task Title: Prompt details task",
		"Task Body:\nBuild the prompt with complete task context.\n\nThe agent must be able to start work without calling task show.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
	if got := strings.Count(output, "The agent must be able to start work without calling task show."); got != 1 {
		t.Fatalf("task requirement appears %d times, want once:\n%s", got, output)
	}
}

func TestFetchPromptContinuesWhenTaskContextFetchFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_TASK_ID", "t-demo-0001")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_CHECK_NAME", "reviewer")
	t.Setenv("FLOW_COORDINATOR_URL", serverURL)
	t.Setenv("FLOW_WORKER_TOKEN", "not-a-valid-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"Flow role instructions (flow-reviewer):",
		"# Flow Reviewer",
		"Task: t-demo-0001",
		"Check: reviewer",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(stderr.String(), "continuing without task context") {
		t.Fatalf("fetch-prompt stderr missing enrichment warning: %q", stderr.String())
	}
}

func TestInitDoesNotSeedRepositorySkills(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runFlowTestGit(t, repoPath, "config", "user.email", "flow@example.com")
	runFlowTestGit(t, repoPath, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runFlowTestGit(t, repoPath, "add", "README.md")
	runFlowTestGit(t, repoPath, "commit", "-m", "seed")
	subdir := filepath.Join(repoPath, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init", "--repo", subdir}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("init exitCode = %d, want 1; stdout = %q stderr = %q", exitCode, stdout.String(), stderr.String())
	}

	resolvedRepoPath, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	if !strings.Contains(stdout.String(), "repo: "+resolvedRepoPath) {
		t.Fatalf("init output missing repo path:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "connect to flow-server") {
		t.Fatalf("init stderr missing connection failure:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote repository skills; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".codex", "skills")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote harness skills; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, ".flow")); !os.IsNotExist(err) {
		t.Fatalf("flow init wrote into subdir; stat err = %v", err)
	}
}

func TestFetchPromptUsesEmbeddedAuthorInstructions(t *testing.T) {
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_TASK_ID", "t-demo-0002")
	t.Setenv("FLOW_WORKER_HARNESS", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}

	for _, want := range []string{
		"Flow role instructions (flow-author):",
		"# Flow Author",
		"Task: t-demo-0002",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "Use $flow-author") {
		t.Fatalf("fetch-prompt output still references an external skill:\n%s", stdout.String())
	}
}

func TestFetchPromptUsesEmbeddedVerifierInstructions(t *testing.T) {
	t.Setenv("FLOW_WORKER_ROLE", "verifier")
	t.Setenv("FLOW_WORKER_HARNESS", "claude")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	for _, want := range []string{
		"Flow role instructions (flow-verifier):",
		"# Flow Verifier",
		"flow thread certify",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFetchPromptAcceptsHarnessConvention(t *testing.T) {
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_WORKER_HARNESS", "harness")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Flow role instructions (flow-author):") {
		t.Fatalf("fetch-prompt output missing author instructions:\n%s", stdout.String())
	}
}

func TestFetchPromptHarnessFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_WORKER_HARNESS", "invalid-harness")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"fetch-prompt", "--harness", "codex"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("fetch-prompt exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Flow role instructions (flow-reviewer):") {
		t.Fatalf("fetch-prompt output missing reviewer instructions:\n%s", stdout.String())
	}
}

func TestFetchPromptRejectsUnsupportedRole(t *testing.T) {
	for _, role := range []string{"ci", "console"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run([]string{"fetch-prompt", "--role", role}, &stdout, &stderr)
		if exitCode != 2 {
			t.Fatalf("fetch-prompt --role %s exitCode = %d, want 2; stdout=%q stderr=%q", role, exitCode, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "unsupported Flow worker role") {
			t.Fatalf("fetch-prompt --role %s stderr = %q", role, stderr.String())
		}
	}
}

func TestTaskCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{
		"task", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "CLI task",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tunscheduled\t\tCLI task") {
		t.Fatalf("create output = %q", stdout.String())
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	created, err := client.GetTask("t-demo-0001")
	if err != nil {
		t.Fatalf("get created task: %v", err)
	}
	if created.State != nil {
		t.Fatalf("created state = %v, want unscheduled", created.State)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{
		"task", "edit",
		"--server", serverURL,
		"--token", "owner-token",
		"--priority=4",
		"t-demo-0001",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task edit flags exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	edited, err := client.GetTask("t-demo-0001")
	if err != nil {
		t.Fatalf("get edited task: %v", err)
	}
	if edited.Priority != 4 {
		t.Fatalf("edited priority = %d, want 4", edited.Priority)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"task", "schedule", "--server", serverURL, "--token", "owner-token", "t-demo-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task schedule exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tscheduled\timplement") {
		t.Fatalf("schedule output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"task", "show", "--server", serverURL, "--token", "owner-token", "t-demo-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task show exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tscheduled\t\tCLI task") {
		t.Fatalf("show output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"board", "--server", serverURL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("board exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "scheduled:\n  t-demo-0001\tscheduled\tCLI task") {
		t.Fatalf("board output = %q", stdout.String())
	}
}

func TestTaskCreateUsesDiscoveredClientConfigOwnerToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	dataDir := t.TempDir()
	if err := os.WriteFile(config.OwnerTokenPath(dataDir), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "create", "--title", "Discovered CLI task"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tunscheduled\t\tDiscovered CLI task") {
		t.Fatalf("create output = %q", stdout.String())
	}
}

func TestTaskRelationCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	source, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Source task"})
	if err != nil {
		t.Fatalf("create source task: %v", err)
	}
	target, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Target task"})
	if err != nil {
		t.Fatalf("create target task: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "link", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task link exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("link stdout = %q", stdout.String())
	}
	relations, err := fixture.Tasks.RelationsForTask(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after link: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceTaskID != source.ID || relations[0].Kind != coordinator.RelationBlocks {
		t.Fatalf("relations after link = %+v", relations)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"task", "unlink", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task unlink exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("unlink stdout = %q", stdout.String())
	}
	relations, err = fixture.Tasks.RelationsForTask(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after unlink: %v", err)
	}
	if len(relations) != 0 {
		t.Fatalf("relations after unlink = %+v, want none", relations)
	}
}

func TestTaskCreateUploadsInitialAttachment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	filePath := filepath.Join(t.TempDir(), "initial.txt")
	if err := os.WriteFile(filePath, []byte("initial attachment"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	var sawCreate bool
	var sawAttachment bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks":
			sawCreate = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"task":{"ID":"t-demo-0001","Title":"With file"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks/t-demo-0001/attachments":
			sawAttachment = true
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if got := r.FormValue("stage"); got != "initial" {
				t.Fatalf("stage = %q, want initial", got)
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("form file: %v", err)
			}
			defer file.Close()
			if header.Filename != "initial.txt" {
				t.Fatalf("filename = %q", header.Filename)
			}
			content, err := io.ReadAll(file)
			if err != nil {
				t.Fatalf("read uploaded file: %v", err)
			}
			if string(content) != "initial attachment" {
				t.Fatalf("uploaded content = %q", string(content))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"attachment":{"id":"att-0001","task_id":"t-demo-0001","stage":"initial","filename":"initial.txt","content_type":"text/plain; charset=utf-8","size_bytes":18}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "create", "--server", server.URL, "--token", "owner-token", "--title", "With file", "--file", filePath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !sawCreate || !sawAttachment {
		t.Fatalf("sawCreate=%t sawAttachment=%t", sawCreate, sawAttachment)
	}
	if !strings.Contains(stdout.String(), "att-0001\tinitial\tinitial.txt\t18") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestTaskAttachUsesInferredRoleAndLease(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("FLOW_ROLE", "reviewer")
	t.Setenv("FLOW_LEASE_ID", "l-0001")
	filePath := filepath.Join(t.TempDir(), "review.png")
	if err := os.WriteFile(filePath, []byte("png"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/projects/p-demo/tasks/t-demo-0001/attachments" {
			t.Fatalf("request = %s %s, want POST /v2/projects/p-demo/tasks/t-demo-0001/attachments", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("lease_id"); got != "l-0001" {
			t.Fatalf("lease_id = %q, want l-0001", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if got := r.FormValue("stage"); got != "reviewer" {
			t.Fatalf("stage = %q, want reviewer", got)
		}
		_, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if header.Filename != "review.png" || header.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("file header = %+v", header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"attachment":{"id":"att-0002","task_id":"t-demo-0001","stage":"reviewer","filename":"review.png","content_type":"image/png","size_bytes":3}}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "attach", "--server", server.URL, "--token", "owner-token", "--file", filePath, "t-demo-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task attach exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "att-0002\treviewer\treview.png\t3") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestTaskCreatePrefersAmbientFlowEnvironmentOverClientConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	// A discoverable client config points at an unreachable server with an
	// unknown token; the worker-injected ambient environment must win.
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: "http://127.0.0.1:1",
		Token:     "config-token",
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	t.Setenv("FLOW_COORDINATOR_URL", httpServer.URL)
	t.Setenv("FLOW_OWNER_TOKEN", "owner-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "create", "--title", "Ambient CLI task"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("task create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tunscheduled\t\tAmbient CLI task") {
		t.Fatalf("create output = %q", stdout.String())
	}
}

func TestApplyClientEnvironmentPrefersSessionToken(t *testing.T) {
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")
	t.Setenv("FLOW_WORKER_TOKEN", "worker-token")
	t.Setenv("FLOW_OWNER_TOKEN", "owner-token")

	values := apiFlagValues{}
	applyClientEnvironment(&values)
	if values.token != "session-token" {
		t.Fatalf("token = %q, want the session token to beat worker and owner tokens", values.token)
	}
}

func TestPrintBoardAnnotatesSubStateAndBlocked(t *testing.T) {
	scheduled := coordinator.LifecycleScheduled
	inProgress := coordinator.LifecycleInProgress
	result := coordinator.BoardResult{
		Board: coordinator.Board{
			Unscheduled: []coordinator.Task{
				{ID: "t-demo-0001", Title: "Unplanned"},
			},
			Scheduled: []coordinator.Task{
				{ID: "t-demo-0002", State: &scheduled, Title: "Queued"},
			},
			InProgress: []coordinator.Task{
				{ID: "t-demo-0003", State: &inProgress, Title: "Working"},
				{ID: "t-demo-0004", State: &inProgress, Title: "Needs input"},
			},
		},
		LaneStates: map[string]coordinator.LaneState{
			"t-demo-0001": coordinator.LaneStateUnscheduled,
			"t-demo-0002": coordinator.LaneStateScheduled,
			"t-demo-0003": coordinator.LaneStateWorking,
			"t-demo-0004": coordinator.LaneStateBlocked,
		},
		WaitReasons: map[string]coordinator.WaitReason{"t-demo-0004": coordinator.WaitReasonQuestion},
	}

	var out bytes.Buffer
	printBoard(&out, result)

	want := "unscheduled:\n" +
		"  t-demo-0001\tunscheduled\tUnplanned\n" +
		"scheduled:\n" +
		"  t-demo-0002\tscheduled\tQueued\n" +
		"in_progress:\n" +
		"  t-demo-0003\tin_progress\tWorking\t[working]\n" +
		"  t-demo-0004\tin_progress\tNeeds input\t[blocked]\t[question]\n"
	if out.String() != want {
		t.Fatalf("board output = %q, want %q", out.String(), want)
	}
}

func TestTaskCommandRejectsUnauthorizedToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "list", "--server", serverURL, "--token", "wrong"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("task list exitCode = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "unauthorized") {
		t.Fatalf("stderr = %q, want unauthorized error", stderr.String())
	}
}

func TestSessionEnvironmentPrefersSessionTokenThenWorkerToken(t *testing.T) {
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:8421")
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")
	t.Setenv("FLOW_WORKER_TOKEN", "worker-token")
	t.Setenv("FLOW_SESSION_ID", "s-env")

	values := &apiFlagValues{}
	var sessionID string
	applySessionEnvironment(values, &sessionID)
	if values.serverURL != "http://127.0.0.1:8421" || values.protocolVersion != "2" {
		t.Fatalf("api flags = %+v", values)
	}
	if values.token != "session-token" {
		t.Fatalf("token = %q, want session token", values.token)
	}
	if sessionID != "s-env" {
		t.Fatalf("sessionID = %q, want env session", sessionID)
	}

	t.Setenv("FLOW_SESSION_TOKEN", "")
	values = &apiFlagValues{}
	applySessionEnvironment(values, nil)
	if values.token != "worker-token" {
		t.Fatalf("token = %q, want worker token fallback", values.token)
	}
}

func TestTaskShowUsesSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Run outside any git repo so the CLI leaves task routes unscoped instead
	// of looking up a project for the cwd.
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/projects/p-demo/tasks/t-demo-0001" {
			t.Fatalf("request = %s %s, want GET /v2/projects/p-demo/tasks/t-demo-0001", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"ID":"t-demo-0001","Title":"Session task","state":"in_progress"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"task", "show", "t-demo-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t-demo-0001\tin_progress\t\tSession task") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUICommandPrintsBrowserLoginURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/ui/bootstrap" {
			t.Fatalf("request = %s %s, want POST /v2/ui/bootstrap", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login_path":"/ui/login?token=abc123","expires_at":"2026-06-07T12:10:00Z"}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"ui", "--server", server.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("ui exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != server.URL+"/ui/login?token=abc123" {
		t.Fatalf("ui output = %q", stdout.String())
	}
}

func TestWorkerAndJobDiagnosticsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	tasks := fixture.Tasks
	checks := fixture.Checks
	task, err := tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Diagnostics task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"agent.harness.codex": "true", "local": "true"},
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       2,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       7,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	exitFailure := 1
	if _, err := checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID:   task.ID,
		Name:     "fake-ci",
		ExitCode: &exitFailure,
		Reporter: "worker:w-local",
	}); err != nil {
		t.Fatalf("report check: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"workers", "--server", httpServer.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("workers exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "w-local\tregistered\tpersistent_agent=1\tephemeral=2\tlabels=agent.harness.codex=true,local=true") {
		t.Fatalf("workers output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"jobs", "--server", httpServer.URL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("jobs exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), job.ID+"\tqueued\tauthor\tpersistent_agent\ttask="+task.ID+"\tpriority=7") {
		t.Fatalf("jobs output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"checks", "--server", httpServer.URL, "--token", "owner-token", task.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("checks exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	checksOutput := stdout.String()
	if !strings.Contains(checksOutput, "review_state: changes_requested") || !strings.Contains(checksOutput, "fake-ci\tci\tblocked\trequired=true\texit_code=1\treporter=worker:w-local") {
		t.Fatalf("checks output = %q", checksOutput)
	}
}

func TestHookIngestDefaultModeSwallowsCoordinatorFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusInternalServerError)
	}))
	server.Close()
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	withStdin(t, `{"hook_event_name":"Stop"}`, func() {
		exitCode := run([]string{"hook", "codex", "ingest"}, &stdout, &stderr)
		if exitCode != 0 {
			t.Fatalf("hook ingest exitCode = %d, stderr = %q", exitCode, stderr.String())
		}
	})
	if stdout.String() != "{}\n" {
		t.Fatalf("hook ingest stdout = %q, want protocol-safe JSON", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("hook ingest stderr = %q, want empty", stderr.String())
	}
}

func TestHookIngestStrictModeRequiresSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:1")
	t.Setenv("FLOW_PROTOCOL_VERSION", "2")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var exitCode int
	withStdin(t, `{"hook_event_name":"Stop"}`, func() {
		exitCode = run([]string{"hook", "claude", "ingest", "--strict"}, &stdout, &stderr)
	})
	if exitCode == 0 {
		t.Fatalf("hook ingest strict exitCode = 0, want nonzero")
	}
	if stdout.String() != "{}\n" {
		t.Fatalf("hook ingest strict stdout = %q, want protocol-safe JSON", stdout.String())
	}
	if !strings.Contains(stderr.String(), "FLOW_SESSION_ID and FLOW_SESSION_TOKEN are required") {
		t.Fatalf("hook ingest strict stderr = %q", stderr.String())
	}
}

// startCLIAuthorSession drives the registry through schedule → claim → running →
// start so CLI tests get a live author session with a session token and change.
func newFlowAPIServer(t *testing.T) string {
	t.Helper()

	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func requireFlowTestTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func runFlowTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func flowTestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func assertMigrationsInclude(t *testing.T, got []string, want ...string) {
	t.Helper()

	seen := map[string]bool{}
	for _, migration := range got {
		seen[migration] = true
	}
	for _, migration := range want {
		if !seen[migration] {
			t.Fatalf("migrations = %v, missing %s", got, migration)
		}
	}
}

func TestSplitQualifiedRef(t *testing.T) {
	cases := []struct {
		ref         string
		wantProject string
		wantID      string
	}{
		{"t-demo-0001", "", "t-demo-0001"},
		{"myproj/t-demo-0001", "myproj", "t-demo-0001"},
		{"myproj/ch-abc123", "myproj", "ch-abc123"},
		{"p-demo/t-demo-0042", "p-demo", "t-demo-0042"},
		{"task/t-demo-0001", "task", "t-demo-0001"},
		{"refs/heads/main", "", "refs/heads/main"},
		{"ch-abc123", "", "ch-abc123"},
	}
	for _, tc := range cases {
		project, id := splitQualifiedRef(tc.ref)
		if project != tc.wantProject || id != tc.wantID {
			t.Errorf("splitQualifiedRef(%q) = (%q, %q), want (%q, %q)", tc.ref, project, id, tc.wantProject, tc.wantID)
		}
	}
}

func TestInitDoesNotSeedSkillsInRepoWithoutCommits(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init", "--repo", repoPath}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("init exitCode = %d, want 1; stdout = %q stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "connect to flow-server") {
		t.Fatalf("init on a fresh repo should fail before writing skills, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("init wrote repository skills in a fresh repo; stat err = %v", err)
	}
}

func TestInitRegistersProjectWithDiscoveredClientConfig(t *testing.T) {
	requireFlowTestTool(t, "git")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.Server.ServeHTTP(w, r)
		if r.Method == http.MethodPost && r.URL.Path == "/v2/projects" {
			for _, bundle := range fixture.Registry.All() {
				neutralizeFlowExchangeHooks(t, bundle.Project.ExchangePath)
			}
		}
	}))
	t.Cleanup(httpServer.Close)
	dataDir := t.TempDir()
	if err := os.WriteFile(config.OwnerTokenPath(dataDir), []byte("owner-token\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	repoPath := t.TempDir()
	runFlowTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runFlowTestGit(t, repoPath, "config", "user.email", "flow@example.com")
	runFlowTestGit(t, repoPath, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runFlowTestGit(t, repoPath, "add", "README.md")
	runFlowTestGit(t, repoPath, "commit", "-m", "seed")
	t.Chdir(repoPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"init"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("register init exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flow project created") || !strings.Contains(stdout.String(), "client_config: "+configPath) {
		t.Fatalf("register init output = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".flow", "skills")); !os.IsNotExist(err) {
		t.Fatalf("init wrote repository skills; stat err = %v", err)
	}
}

func neutralizeFlowExchangeHooks(t *testing.T, exchangePath string) {
	t.Helper()
	for _, name := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(exchangePath, "hooks", name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("neutralize exchange hook %s: %v", name, err)
		}
	}
}

func TestApproveGitCredentialUsesConfiguredHelper(t *testing.T) {
	requireFlowTestTool(t, "git")
	repoPath := t.TempDir()
	runFlowTestGit(t, "", "init", repoPath)

	capturePath := filepath.Join(t.TempDir(), "credential.txt")
	helperPath := filepath.Join(t.TempDir(), "credential-helper.sh")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\ncat > '"+strings.ReplaceAll(capturePath, "'", "'\"'\"'")+"'\n"), 0o755); err != nil {
		t.Fatalf("write credential helper: %v", err)
	}
	runFlowTestGit(t, repoPath, "config", "credential.helper", helperPath)

	stored, command, err := approveGitCredential(repoPath, "http://127.0.0.1:8421/git/projects/p-test/exchange.git", "owner-token")
	if err != nil {
		t.Fatalf("approve credential: %v", err)
	}
	if !stored || command != "" {
		t.Fatalf("stored=%t command=%q, want stored credential", stored, command)
	}
	contents, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured credential: %v", err)
	}
	for _, want := range []string{
		"protocol=http",
		"host=127.0.0.1:8421",
		"path=git/projects/p-test/exchange.git",
		"username=flow",
		"password=owner-token",
	} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("captured credential missing %q:\n%s", want, string(contents))
		}
	}
	usePath := gitHTTPTestConfig(t, repoPath, "credential.useHttpPath")
	if usePath != "true" {
		t.Fatalf("credential.useHttpPath = %q, want true", usePath)
	}
}

func gitHTTPTestConfig(t *testing.T, repoPath string, key string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", repoPath, "config", "--get", key).CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get %s: %s: %v", key, strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output))
}
