package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/db"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

var flowRuntimeEnvKeys = []string{
	"FLOW_WORKER_ROLE",
	"FLOW_ROLE",
	"FLOW_ISSUE_ID",
	"FLOW_CHANGE_ID",
	"FLOW_BRANCH",
	"FLOW_BASE",
	"FLOW_CHECK_NAME",
	"FLOW_WORKER_HARNESS",
	"FLOW_SESSION_PURPOSE",
	"FLOW_COORDINATOR_URL",
	"FLOW_SESSION_TOKEN",
	"FLOW_WORKER_TOKEN",
	"FLOW_OWNER_TOKEN",
	"FLOW_PROJECT_ID",
	"FLOW_PROJECT_NAME",
	"FLOW_SESSION_ID",
	"FLOW_REVIEW_CYCLE_INSTRUCTIONS",
	"FLOW_CONSOLE_SCOPE",
	"FLOW_PROTOCOL_VERSION",
	"FLOW_DATA_DIR",
	"FLOW_TRANSCRIPT_FILE",
}

func TestMain(m *testing.M) {
	for _, key := range flowRuntimeEnvKeys {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

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

func clearFetchPromptEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range flowRuntimeEnvKeys {
		t.Setenv(key, "")
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
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_ISSUE_ID", "i-0001")
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
		"Issue: i-0001",
		"Change: ch-1",
		"Check: reviewer",
		"Use flow comment",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptIncludesIssueDetailsFromAPI(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	issue, err := client.CreateIssue(flowclient.CreateIssueInput{
		Title:              "Prompt details issue",
		Body:               "Build the prompt with complete issue context.",
		AcceptanceCriteria: "The agent can start work without calling issue show.",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", issue.ID)
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
		"Issue: " + issue.ID,
		"Issue Title: Prompt details issue",
		"Issue Body:\nBuild the prompt with complete issue context.",
		"Acceptance Criteria:\nThe agent can start work without calling issue show.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
}

func TestFetchPromptContinuesWhenIssueContextFetchFails(t *testing.T) {
	clearFetchPromptEnvironment(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	t.Setenv("FLOW_WORKER_ROLE", "reviewer")
	t.Setenv("FLOW_ISSUE_ID", "i-0001")
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
		"Issue: i-0001",
		"Check: reviewer",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fetch-prompt output missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(stderr.String(), "continuing without issue context") {
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
	clearFetchPromptEnvironment(t)
	t.Setenv("FLOW_WORKER_ROLE", "author")
	t.Setenv("FLOW_ISSUE_ID", "i-0002")
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
		"Issue: i-0002",
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
	clearFetchPromptEnvironment(t)
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
	clearFetchPromptEnvironment(t)
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
	clearFetchPromptEnvironment(t)
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
		clearFetchPromptEnvironment(t)
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

func TestIssueCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{
		"issue", "create",
		"--server", serverURL,
		"--token", "owner-token",
		"--title", "CLI issue",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tunscheduled\t\tCLI issue") {
		t.Fatalf("create output = %q", stdout.String())
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: serverURL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	created, err := client.GetIssue("i-0001")
	if err != nil {
		t.Fatalf("get created issue: %v", err)
	}
	if created.State != nil {
		t.Fatalf("created state = %v, want unscheduled", created.State)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{
		"issue", "edit",
		"--server", serverURL,
		"--token", "owner-token",
		"--priority=4",
		"i-0001",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue edit flags exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	edited, err := client.GetIssue("i-0001")
	if err != nil {
		t.Fatalf("get edited issue: %v", err)
	}
	if edited.Priority != 4 {
		t.Fatalf("edited priority = %d, want 4", edited.Priority)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "schedule", "--server", serverURL, "--token", "owner-token", "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue schedule exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tscheduled\timplement") {
		t.Fatalf("schedule output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "show", "--server", serverURL, "--token", "owner-token", "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue show exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tscheduled\t\tCLI issue") {
		t.Fatalf("show output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"board", "--server", serverURL, "--token", "owner-token"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("board exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "scheduled:\n  i-0001\tscheduled\tCLI issue") {
		t.Fatalf("board output = %q", stdout.String())
	}
}

func TestIssueCreateUsesDiscoveredClientConfigOwnerToken(t *testing.T) {
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
	exitCode := run([]string{"issue", "create", "--title", "Discovered CLI issue"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tunscheduled\t\tDiscovered CLI issue") {
		t.Fatalf("create output = %q", stdout.String())
	}
}

func TestIssueRelationCommandsUseAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newFlowTestFixture(t)
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	source, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Source issue"})
	if err != nil {
		t.Fatalf("create source issue: %v", err)
	}
	target, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Target issue"})
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "link", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue link exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("link stdout = %q", stdout.String())
	}
	relations, err := fixture.Issues.RelationsForIssue(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after link: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceIssueID != source.ID || relations[0].Kind != coordinator.RelationBlocks {
		t.Fatalf("relations after link = %+v", relations)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"issue", "unlink", "--server", httpServer.URL, "--token", "owner-token", source.ID, "blocks", target.ID}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue unlink exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != source.ID+"\tblocks\t"+target.ID {
		t.Fatalf("unlink stdout = %q", stdout.String())
	}
	relations, err = fixture.Issues.RelationsForIssue(ctx, target.ID)
	if err != nil {
		t.Fatalf("relations after unlink: %v", err)
	}
	if len(relations) != 0 {
		t.Fatalf("relations after unlink = %+v, want none", relations)
	}
}

func TestIssueCreateUploadsInitialAttachment(t *testing.T) {
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
		case r.Method == http.MethodPost && r.URL.Path == "/v2/issues":
			sawCreate = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issue":{"ID":"i-0001","Title":"With file"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/issues/i-0001/attachments":
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
			_, _ = w.Write([]byte(`{"attachment":{"id":"att-0001","issue_id":"i-0001","stage":"initial","filename":"initial.txt","content_type":"text/plain; charset=utf-8","size_bytes":18}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "create", "--server", server.URL, "--token", "owner-token", "--title", "With file", "--file", filePath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue create exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !sawCreate || !sawAttachment {
		t.Fatalf("sawCreate=%t sawAttachment=%t", sawCreate, sawAttachment)
	}
	if !strings.Contains(stdout.String(), "att-0001\tinitial\tinitial.txt\t18") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestIssueAttachUsesInferredRoleAndLease(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("FLOW_ROLE", "reviewer")
	t.Setenv("FLOW_LEASE_ID", "l-0001")
	filePath := filepath.Join(t.TempDir(), "review.png")
	if err := os.WriteFile(filePath, []byte("png"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/issues/i-0001/attachments" {
			t.Fatalf("request = %s %s, want POST /v2/issues/i-0001/attachments", r.Method, r.URL.Path)
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
		_, _ = w.Write([]byte(`{"attachment":{"id":"att-0002","issue_id":"i-0001","stage":"reviewer","filename":"review.png","content_type":"image/png","size_bytes":3}}`))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "attach", "--server", server.URL, "--token", "owner-token", "--file", filePath, "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("issue attach exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "att-0002\treviewer\treview.png\t3") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestIssueCreateDiscoveryIgnoresAmbientFlowSessionEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssueCreateUsesDiscoveredClientConfigOwnerToken$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"FLOW_COORDINATOR_URL=http://127.0.0.1:1",
		"FLOW_SESSION_TOKEN=leaked-session-token",
		"FLOW_SESSION_ID=s-live",
		"FLOW_ISSUE_ID=i-live",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child test used ambient Flow session environment: %v\n%s", err, string(output))
	}
}

func TestPrintBoardAnnotatesSubStateAndBlocked(t *testing.T) {
	scheduled := coordinator.LifecycleScheduled
	inProgress := coordinator.LifecycleInProgress
	result := coordinator.BoardResult{
		Board: coordinator.Board{
			Unscheduled: []coordinator.Issue{
				{ID: "i-0001", Title: "Unplanned"},
			},
			Scheduled: []coordinator.Issue{
				{ID: "i-0002", State: &scheduled, Title: "Queued"},
			},
			InProgress: []coordinator.Issue{
				{ID: "i-0003", State: &inProgress, Title: "Working"},
				{ID: "i-0004", State: &inProgress, Title: "Needs input"},
			},
		},
		LaneStates: map[string]coordinator.LaneState{
			"i-0001": coordinator.LaneStateUnscheduled,
			"i-0002": coordinator.LaneStateScheduled,
			"i-0003": coordinator.LaneStateWorking,
			"i-0004": coordinator.LaneStateBlocked,
		},
		WaitReasons: map[string]coordinator.WaitReason{"i-0004": coordinator.WaitReasonQuestion},
	}

	var out bytes.Buffer
	printBoard(&out, result)

	want := "unscheduled:\n" +
		"  i-0001\tunscheduled\tUnplanned\n" +
		"scheduled:\n" +
		"  i-0002\tscheduled\tQueued\n" +
		"in_progress:\n" +
		"  i-0003\tin_progress\tWorking\t[working]\n" +
		"  i-0004\tin_progress\tNeeds input\t[blocked]\t[question]\n"
	if out.String() != want {
		t.Fatalf("board output = %q, want %q", out.String(), want)
	}
}

func TestIssueCommandRejectsUnauthorizedToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	serverURL := newFlowAPIServer(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "list", "--server", serverURL, "--token", "wrong"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("issue list exitCode = %d, want 1", exitCode)
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

func TestIssueShowUsesSessionEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Run outside any git repo so the CLI leaves issue routes unscoped instead
	// of looking up a project for the cwd.
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/issues/i-0001" {
			t.Fatalf("request = %s %s, want GET /v2/issues/i-0001", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue":{"ID":"i-0001","Title":"Session issue","state":"in_progress"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)
	t.Setenv("FLOW_SESSION_TOKEN", "session-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"issue", "show", "i-0001"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "i-0001\tin_progress\t\tSession issue") {
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
	issues := fixture.Issues
	checks := fixture.Checks
	issue, err := issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Diagnostics issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
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
		IssueID:        &issue.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       7,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	exitFailure := 1
	if _, err := checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  issue.ID,
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
	if !strings.Contains(stdout.String(), job.ID+"\tqueued\tauthor\tpersistent_agent\tissue="+issue.ID+"\tpriority=7") {
		t.Fatalf("jobs output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run([]string{"checks", "--server", httpServer.URL, "--token", "owner-token", issue.ID}, &stdout, &stderr)
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
		{"i-0001", "", "i-0001"},
		{"myproj/i-0001", "myproj", "i-0001"},
		{"myproj/ch-abc123", "myproj", "ch-abc123"},
		{"p-1234/i-0042", "p-1234", "i-0042"},
		{"issue/i-0001", "issue", "i-0001"},
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
	exchangePath := filepath.Join(t.TempDir(), "exchange.git")
	runFlowTestGit(t, "", "init", "--bare", exchangePath)
	exchangeURL := (&url.URL{Scheme: "file", Path: exchangePath}).String()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/projects" {
			t.Fatalf("request = %s %s, want POST /v2/projects", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":"p-discovered","name":"repo","repo_path":"","base_branch":"main","exchange_name":"flow","exchange_url":` + strconv.Quote(exchangeURL) + `},"created":true}`))
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
