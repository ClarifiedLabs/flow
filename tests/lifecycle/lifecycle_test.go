package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
)

const (
	ownerToken      = "owner-lifecycle-token"
	hookToken       = "hook-lifecycle-token"
	workerJoinToken = "worker-join-lifecycle-token"
)

var lifecycleBuild lifecycleBinaryBuild

type lifecycleBinaryBuild struct {
	once       sync.Once
	dir        string
	flow       string
	flowServer string
	flowWorker string
	err        error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if lifecycleBuild.dir != "" {
		_ = os.RemoveAll(lifecycleBuild.dir)
	}
	os.Exit(code)
}

// projectFixture is one onboarded Flow project: its worktree, the server-side
// exchange and database, and the ids the server assigned at registration.
type projectFixture struct {
	repoPath     string
	projectID    string
	projectName  string
	exchangePath string
	dbPath       string
}

func TestCLIIntegrationLifecycle(t *testing.T) {
	t.Parallel()
	lc := newLifecycle(t)

	// Project A is onboarded against the already-running server.
	projectA := lc.onboardProject(t, "alpha")
	// Project B is onboarded AFTER the server is serving requests, proving the
	// registry picks up new projects live without a restart.
	projectB := lc.onboardProject(t, "beta")
	lc.configureWorker(t)

	// Drive both projects' i-0001 through the full lifecycle. Issue ids restart
	// per project, so both are i-0001.
	lc.driveIssue(t, projectA)
	lc.driveIssue(t, projectB)

	t.Run("reconcile aggregates both projects", func(t *testing.T) {
		output := lc.flowCLIInRepo(t, projectA.repoPath, "reconcile")
		assertContains(t, output, "projects_scanned: 2")
	})

	t.Run("single worker served both projects", func(t *testing.T) {
		// The lone w-local worker ran author jobs for both projects: each
		// project's exchange advanced to a merge commit carrying its own issue
		// marker, and neither merge leaked into the other exchange.
		lc.assertMergedBase(t, projectA)
		lc.assertMergedBase(t, projectB)
		lc.assertExchangeLacks(t, projectA.exchangePath, "lifecycle complete for "+projectB.projectName)
		lc.assertExchangeLacks(t, projectB.exchangePath, "lifecycle complete for "+projectA.projectName)
	})
}

// driveIssue creates, schedules, authors, checks, and merges i-0001 in one
// project, asserting the merge landed on that project's exchange.
func (lc *lifecycle) driveIssue(t *testing.T, project projectFixture) {
	t.Helper()
	const issueID = "i-0001"

	t.Run(project.projectName+": cli creates and queues issue", func(t *testing.T) {
		output := lc.flowCLIInRepo(t, project.repoPath,
			"issue", "create",
			"--title", project.projectName+" lifecycle issue",
			"--body", "Exercise serve, init, worker, checks, and merge.",
			"--acceptance-criteria", "app.txt contains lifecycle complete",
			"--requires-human-review=false",
		)
		got := firstField(t, output)
		if got != issueID {
			t.Fatalf("issue id = %q, want %s; output:\n%s", got, issueID, output)
		}

		output = lc.flowCLIInRepo(t, project.repoPath, "issue", "schedule", issueID, "up_next")
		assertContains(t, output, issueID+"\tup_next\taccepted")

		output = lc.flowCLIInRepo(t, project.repoPath, "board")
		assertContains(t, output, "up_next:")
		assertContains(t, output, project.projectName+" lifecycle issue")
	})

	t.Run(project.projectName+": worker completes author job through tmux", func(t *testing.T) {
		output := lc.runWorkerOnce(t)
		assertContains(t, output, "registered: w-local")
		assertContains(t, output, "persistent session finalized:")

		output = lc.flowCLIInRepo(t, project.repoPath, "jobs")
		assertContains(t, output, "\tfinished\tauthor\tpersistent_agent\tissue="+issueID)
		if !strings.Contains(output, "\tqueued\tci\tephemeral\tissue="+issueID) && !strings.Contains(output, "\tfinished\tci\tephemeral\tissue="+issueID) {
			t.Fatalf("output missing queued or finished ci job for %s:\n%s", issueID, output)
		}
	})

	t.Run(project.projectName+": worker completes configured checks", func(t *testing.T) {
		lc.runWorkerUntilApproved(t, project, issueID)

		output := lc.flowCLIInRepo(t, project.repoPath, "checks", issueID)
		assertContains(t, output, "review_state: approved")
		assertContains(t, output, "unit\tci\tsatisfied\trequired=true\texit_code=0")
		assertContains(t, output, "reviewer\treviewer\tsatisfied\trequired=true\texit_code=0")
		assertContains(t, output, "verifier\tverifier\tsatisfied\trequired=true\texit_code=0")
	})

	t.Run(project.projectName+": cli merges approved change", func(t *testing.T) {
		output := lc.flowCLIInRepo(t, project.repoPath, "merge", issueID)
		fields := strings.Fields(output)
		if len(fields) != 4 || fields[0] != issueID || !strings.HasPrefix(fields[1], "ch-") {
			t.Fatalf("merge output = %q", output)
		}

		output = lc.flowCLIInRepo(t, project.repoPath, "issue", "show", issueID)
		assertContains(t, output, issueID+"\tclosed\taccepted")
		lc.assertMergedBase(t, project)
	})
}

func TestBrowserE2ELifecycle(t *testing.T) {
	t.Parallel()
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found; set FLOW_BROWSER_BIN to run browser lifecycle e2e")
	}

	lc := newLifecycle(t)
	projectA := lc.onboardProject(t, "alpha")
	projectB := lc.onboardProject(t, "beta")
	lc.configureWorker(t)

	// Seed one driven issue per project so each project's board has a card
	// (the per-project badge only renders on cards, and the board only shows a
	// project once it has issues).
	const title = "Browser lifecycle issue"
	lc.flowCLIInRepo(t, projectA.repoPath, "issue", "create", "--title", title, "--requires-human-review=false")
	lc.flowCLIInRepo(t, projectA.repoPath, "issue", "schedule", "i-0001", "up_next")
	lc.flowCLIInRepo(t, projectB.repoPath, "issue", "create", "--title", title, "--requires-human-review=false")
	lc.flowCLIInRepo(t, projectB.repoPath, "issue", "schedule", "i-0001", "up_next")

	browserCtx, cancel := newBrowserContext(t, browserPath)
	defer cancel()

	loginURL := strings.TrimSpace(lc.flowCLIInRepo(t, projectA.repoPath, "ui"))
	// Logging in establishes the session cookie; land on the board afterward.
	navigateAndWaitForText(t, browserCtx, loginURL, "Board")

	// The aggregate board shows every project. With two projects the per-card
	// project badge renders, so both project names appear on their cards.
	navigateAndWaitForText(t, browserCtx, lc.serverURL+"/ui/board", title)
	waitForPageText(t, browserCtx, projectA.projectName)
	waitForPageText(t, browserCtx, projectB.projectName)

	// The project picker exists because there are two projects (it is hidden
	// when <= 1). Assert the control rendered.
	assertSelectorVisible(t, browserCtx, "details.project-picker")

	// Deep links into the per-project issue route work.
	navigateAndWaitForText(t, browserCtx, lc.serverURL+"/ui/projects/"+projectA.projectID+"/issues/i-0001", title)
	navigateAndWaitForText(t, browserCtx, lc.serverURL+"/ui/projects/"+projectB.projectID+"/issues/i-0001", title)

	// Drive project A's issue to merge through the worker, then merge it via
	// the browser merge view (project B is unaffected).
	lc.runWorkerOnce(t)
	lc.runWorkerUntilApproved(t, projectA, "i-0001")

	// The aggregate merge view lists every project's ready_to_merge issue, and
	// issue ids restart per project, so both projects show a data-merge="i-0001"
	// button. Disambiguate by project: merge project A only and assert project
	// B's button is untouched.
	mergeButtonA := fmt.Sprintf(`button[data-merge="i-0001"][data-project=%q]`, projectA.projectID)
	mergeButtonB := fmt.Sprintf(`button[data-merge="i-0001"][data-project=%q]`, projectB.projectID)
	navigateAndWaitForText(t, browserCtx, lc.serverURL+"/ui/merge", title)
	waitForPageText(t, browserCtx, "approved")
	assertSelectorVisible(t, browserCtx, mergeButtonB)
	if err := chromedp.Run(browserCtx,
		chromedp.Click(mergeButtonA, chromedp.ByQuery),
		waitForNoSelector(mergeButtonA),
	); err != nil {
		t.Fatalf("merge issue through browser UI: %v", err)
	}
	// Project B's issue was not merged: its merge button still renders.
	assertSelectorVisible(t, browserCtx, mergeButtonB)

	navigateAndWaitForText(t, browserCtx, lc.serverURL+"/ui/projects/"+projectA.projectID+"/issues/i-0001", "closed")
	lc.assertMergedBase(t, projectA)

	// NOTE: the project-picker filter (clicking checkboxes to filter the board)
	// is not exercised — the minimal chromedp driver here cannot reliably toggle
	// the <details> menu items. Both projects' cards rendering plus the deep
	// links are asserted instead.
}

type lifecycle struct {
	root         string
	dataDir      string
	configDir    string
	dbPath       string
	workerConfig string
	binDir       string
	flow         string
	flowServer   string
	flowWorker   string
	serverURL    string
	serverAddr   string
	serverConfig string
	serverLog    string
	authorScript string
	tmuxTmp      string
	baseEnv      []string
	projectSeq   int
}

func newLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	requireTool(t, "git")
	requireTool(t, "tmux")

	root := t.TempDir()
	lc := &lifecycle{
		root:      root,
		dataDir:   filepath.Join(root, "flow-data"),
		configDir: filepath.Join(root, "xdg-config"),
		binDir:    filepath.Join(root, "bin"),
		serverLog: filepath.Join(root, "flow-server.log"),
	}
	lc.baseEnv = []string{
		"XDG_DATA_HOME=" + filepath.Join(root, "xdg-data"),
		"XDG_CONFIG_HOME=" + lc.configDir,
	}
	tmuxTmp, err := os.MkdirTemp("/tmp", "flow-tmux-")
	if err != nil {
		t.Fatalf("create tmux dir: %v", err)
	}
	lc.tmuxTmp = tmuxTmp
	t.Cleanup(func() {
		_ = os.RemoveAll(tmuxTmp)
	})

	if err := os.MkdirAll(lc.binDir, 0o755); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	lc.flow = filepath.Join(lc.binDir, "flow")
	lc.flowServer = filepath.Join(lc.binDir, "flow-server")
	lc.flowWorker = filepath.Join(lc.binDir, "flow-worker")
	lc.flow, lc.flowServer, lc.flowWorker = lifecycleBinaries(t)
	fakeTTYD := filepath.Join(lc.binDir, "ttyd")
	writeFile(t, fakeTTYD, "#!/bin/sh\nsleep 1\n")
	if err := os.Chmod(fakeTTYD, 0o700); err != nil {
		t.Fatalf("chmod fake ttyd: %v", err)
	}

	lc.authorScript = lc.writeAuthorScript(t)
	lc.writeServerConfig(t)
	lc.startServer(t)
	return lc
}

func lifecycleBinaries(t *testing.T) (string, string, string) {
	t.Helper()
	lifecycleBuild.once.Do(func() {
		source := sourceRoot(t)
		dir, err := os.MkdirTemp("", "flow-lifecycle-bin-")
		if err != nil {
			lifecycleBuild.err = fmt.Errorf("create lifecycle binary dir: %w", err)
			return
		}
		lifecycleBuild.dir = dir
		lifecycleBuild.flow = filepath.Join(dir, "flow")
		lifecycleBuild.flowServer = filepath.Join(dir, "flow-server")
		lifecycleBuild.flowWorker = filepath.Join(dir, "flow-worker")
		for _, build := range []struct {
			output string
			pkg    string
		}{
			{lifecycleBuild.flow, "./cmd/flow"},
			{lifecycleBuild.flowServer, "./cmd/flow-server"},
			{lifecycleBuild.flowWorker, "./cmd/flow-worker"},
		} {
			if err := buildBinary(source, build.output, build.pkg); err != nil {
				lifecycleBuild.err = err
				return
			}
		}
	})
	if lifecycleBuild.err != nil {
		t.Fatal(lifecycleBuild.err)
	}
	return lifecycleBuild.flow, lifecycleBuild.flowServer, lifecycleBuild.flowWorker
}

// writeServerConfig writes the coordinator config. The serve command derives
// the database from data_dir (<data_dir>/global.db); the worker config is owned
// by the worker side of the test harness.
func (lc *lifecycle) writeServerConfig(t *testing.T) {
	t.Helper()
	lc.serverAddr = freeLocalAddress(t)
	lc.serverURL = "http://" + lc.serverAddr

	coordinatorCfg := config.CoordinatorConfig{
		DataDir:         lc.dataDir,
		ListenAddr:      lc.serverAddr,
		ProtocolVersion: config.DefaultProtocolVersion,
		AuthorEntrypoint: map[string]any{
			"argv":  []string{lc.authorScript},
			"cwd":   ".",
			"env":   map[string]string{},
			"shell": false,
		},
	}
	contents, err := json.MarshalIndent(coordinatorCfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal coordinator config: %v", err)
	}
	lc.serverConfig = filepath.Join(lc.root, "coordinator.json")
	if err := os.WriteFile(lc.serverConfig, append(contents, '\n'), 0o600); err != nil {
		t.Fatalf("write coordinator config: %v", err)
	}
	lc.dbPath = coordinatorCfg.GlobalDatabasePath()
}

// onboardProject creates a worktree and registers it as a Flow project against
// the running server.
func (lc *lifecycle) onboardProject(t *testing.T, name string) projectFixture {
	t.Helper()
	lc.projectSeq++
	repoPath := filepath.Join(lc.root, "repos", name)
	lc.createRepo(t, repoPath)

	runGit(t, repoPath, "add", "README.md", "app.txt", ".flow/checks")
	runGit(t, repoPath, "commit", "-m", "test: seed lifecycle fixture")

	registerOut := runCommand(t, commandInput{
		Env: lc.baseEnv,
		Dir: repoPath,
		Argv: []string{
			lc.flow, "init", "--repo", repoPath,
			"--name", name, "--base", "main",
			"--server", lc.serverURL, "--token", ownerToken,
		},
	})
	projectID := parseKeyLine(t, registerOut, "project_id")
	if got := parseKeyLine(t, registerOut, "name"); got != name {
		t.Fatalf("registered project name = %q, want %q", got, name)
	}
	if got := parseKeyLine(t, registerOut, "base_branch"); got != "main" {
		t.Fatalf("registered base_branch = %q, want main", got)
	}
	exchangeRemote := parseKeyLine(t, registerOut, "exchange_remote")
	assertHTTPExchangeRemote(t, exchangeRemote, lc.serverURL, projectID)
	exchangePath := filepath.Join(lc.dataDir, "projects", projectID, "exchange.git")

	// Re-running init is idempotent.
	rerunOut := runCommand(t, commandInput{
		Dir:  repoPath,
		Argv: []string{lc.flow, "init", "--repo", repoPath, "--server", lc.serverURL, "--token", ownerToken},
		Env:  lc.baseEnv,
	})
	assertContains(t, rerunOut, "flow project already registered")

	return projectFixture{
		repoPath:     repoPath,
		projectID:    projectID,
		projectName:  name,
		exchangePath: exchangePath,
		dbPath:       filepath.Join(lc.dataDir, "projects", projectID, "flow.db"),
	}
}

func assertHTTPExchangeRemote(t *testing.T, remoteLine string, serverURL string, projectID string) {
	t.Helper()
	parts := strings.SplitN(remoteLine, "->", 2)
	if len(parts) != 2 {
		t.Fatalf("exchange_remote line missing arrow: %q", remoteLine)
	}
	exchangeURL := strings.TrimSpace(parts[1])
	parsed, err := url.Parse(exchangeURL)
	if err != nil {
		t.Fatalf("parse exchange URL %q: %v", exchangeURL, err)
	}
	server, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", serverURL, err)
	}
	if parsed.Scheme != server.Scheme || parsed.Host != server.Host {
		t.Fatalf("exchange URL = %q, want server %s://%s", exchangeURL, server.Scheme, server.Host)
	}
	wantPath := "/git/projects/" + projectID + "/exchange.git"
	if parsed.Path != wantPath {
		t.Fatalf("exchange URL path = %q, want %q", parsed.Path, wantPath)
	}
}

func (lc *lifecycle) createRepo(t *testing.T, repoPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoPath, ".flow", "checks"), 0o755); err != nil {
		t.Fatalf("create repo dirs: %v", err)
	}
	writeFile(t, filepath.Join(repoPath, "README.md"), "# Lifecycle Fixture\n")
	writeFile(t, filepath.Join(repoPath, "app.txt"), "initial\n")
	writeFile(t, filepath.Join(repoPath, ".flow", "checks", "unit.yaml"), `name: unit
kind: ci
phase: critique
entrypoint:
  argv:
    - /bin/sh
    - -c
    - |
      test -f app.txt
      grep -q "lifecycle complete" app.txt
  cwd: .
  shell: false
`)
	writeFile(t, filepath.Join(repoPath, ".flow", "checks", "reviewer.yaml"), `name: reviewer
kind: reviewer
phase: critique
entrypoint:
  argv:
    - /bin/sh
    - -c
    - exit 0
  cwd: .
  shell: false
requires: ["agent.harness.codex"]
`)
	writeFile(t, filepath.Join(repoPath, ".flow", "checks", "verifier.yaml"), `name: verifier
kind: verifier
phase: acceptance
entrypoint:
  argv:
    - /bin/sh
    - -c
    - exit 0
  cwd: .
  shell: false
requires: ["agent.harness.codex"]
`)

	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "config", "user.name", "Flow Lifecycle Test")
	runGit(t, repoPath, "config", "user.email", "flow-lifecycle@example.test")
}

func (lc *lifecycle) configureWorker(t *testing.T) {
	t.Helper()
	lc.workerConfig = filepath.Join(lc.root, "worker.yaml")
	workerCfg := config.WorkerConfig{
		WorkerID:        "w-local",
		CoordinatorURL:  lc.serverURL,
		WorkDir:         filepath.Join(lc.root, "worker-work"),
		ProtocolVersion: config.DefaultProtocolVersion,
		Labels: map[string]string{
			"agent.harness.codex": "true",
			"local":               "true",
		},
		Capacity: config.WorkerCapacity{
			PersistentAgent: 5,
			Ephemeral:       5,
		},
		Git: config.WorkerGitConfig{
			Principal: "worker:w-local",
		},
	}
	workerContents, err := yaml.Marshal(workerCfg)
	if err != nil {
		t.Fatalf("marshal worker config: %v", err)
	}
	if err := os.WriteFile(lc.workerConfig, workerContents, 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}
}

func (lc *lifecycle) writeAuthorScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(lc.root, "scripted-author.sh")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
FLOW_BIN=%s

"$FLOW_BIN" hook codex start >/dev/null 2>&1 || true
"$FLOW_BIN" status "scripted author started"
printf 'lifecycle complete for %%s\n' "$FLOW_PROJECT_NAME" > app.txt
mkdir -p .flow/session
printf '{"issue":"%%s","change":"%%s"}\n' "$FLOW_ISSUE_ID" "$FLOW_CHANGE_ID" > .flow/session/state.json
git config user.name "Flow Lifecycle Test"
git config user.email "flow-lifecycle@example.test"
git add app.txt .flow/session/state.json
git commit -m "test: implement lifecycle fixture"
"$FLOW_BIN" ready <<'HANDOFF'
# Flow Handoff

## Current Goal
Complete the lifecycle fixture.

## Completed Work
Updated app.txt and committed the change.

## Remaining Work
Run configured checks and merge.

## Tests Run and Results
Configured unit check will validate app.txt.

## Failed Approaches
None.

## Important Files and Commands
app.txt

## Next Recommended Action
Run the configured CI check.
HANDOFF
"$FLOW_BIN" hook codex stop >/dev/null 2>&1 || true
`, shellQuote(lc.flow))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write author script: %v", err)
	}
	return path
}

func (lc *lifecycle) startServer(t *testing.T) {
	t.Helper()
	logFile, err := os.Create(lc.serverLog)
	if err != nil {
		t.Fatalf("create server log: %v", err)
	}
	cmd := exec.Command(lc.flowServer, "serve", "--config", lc.serverConfig, "--owner-token", ownerToken, "--hook-token", hookToken, "--worker-join-token", workerJoinToken)
	cmd.Env = append(sanitizedEnviron(), lc.baseEnv...)
	var startupOut bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &startupOut)
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start flow-server: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		_ = logFile.Close()
	})
	lc.waitForServer(t)

	if got := lc.parseStartupKey(t, &startupOut, "database"); got != lc.dbPath {
		t.Fatalf("server database = %q, want %q", got, lc.dbPath)
	}
	// owner.token / hook.token files are written even though we passed explicit
	// tokens via flags; confirm the lines are present.
	lc.parseStartupKey(t, &startupOut, "owner_token_file")
	lc.parseStartupKey(t, &startupOut, "hook_token_file")
}

// parseStartupKey reads a "key: value" line from the server's captured stdout,
// polling briefly because startup output may still be flushing.
func (lc *lifecycle) parseStartupKey(t *testing.T, buf *bytes.Buffer, key string) string {
	t.Helper()
	prefix := key + ":"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server startup output missing %s line:\n%s", key, buf.String())
	return ""
}

func (lc *lifecycle) waitForServer(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, lc.serverURL+"/v1/board", nil)
		if err != nil {
			t.Fatalf("create health request: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+ownerToken)
		request.Header.Set("Flow-Protocol-Version", config.DefaultProtocolVersion)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", response.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	logContents, _ := os.ReadFile(lc.serverLog)
	t.Fatalf("flow-server did not become ready: %v\nserver log:\n%s", lastErr, string(logContents))
}

// flowCLIInRepo runs the flow CLI from a project's worktree. Project context is
// auto-detected from the cwd; --server/--token are supplied for every command
// so the test does not depend on the written client config.
func (lc *lifecycle) flowCLIInRepo(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	argv := []string{lc.flow}
	switch {
	case len(args) >= 2 && (args[0] == "issue" || args[0] == "thread" || args[0] == "session"):
		argv = append(argv, args[0], args[1], "--server", lc.serverURL, "--token", ownerToken)
		argv = append(argv, args[2:]...)
	case len(args) >= 1:
		argv = append(argv, args[0], "--server", lc.serverURL, "--token", ownerToken)
		argv = append(argv, args[1:]...)
	default:
		t.Fatal("flowCLIInRepo requires a command")
	}
	return runCommand(t, commandInput{Dir: repoPath, Argv: argv, Env: lc.baseEnv})
}

func (lc *lifecycle) runWorkerOnce(t *testing.T) string {
	t.Helper()
	return runCommand(t, commandInput{
		Dir: lc.root,
		Env: append(append([]string(nil), lc.baseEnv...),
			"PATH="+lc.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"TMUX_TMPDIR="+lc.tmuxTmp,
			"FLOW_WORKER_JOIN_TOKEN="+workerJoinToken,
		),
		Argv: []string{
			lc.flowWorker,
			"-c", lc.workerConfig,
			"--once",
			"--claim-wait=2s",
			"--lease=30s",
			"--heartbeat-ttl=30s",
		},
		Timeout: 90 * time.Second,
	})
}

func (lc *lifecycle) runWorkerUntilApproved(t *testing.T, project projectFixture, issueID string) string {
	t.Helper()
	var combined strings.Builder
	for attempt := 0; attempt < 4; attempt++ {
		output := lc.runWorkerOnce(t)
		combined.WriteString(output)
		checks := lc.flowCLIInRepo(t, project.repoPath, "checks", issueID)
		if strings.Contains(checks, "review_state: approved") {
			return combined.String()
		}
		if strings.Contains(output, "claimed: none") && !strings.Contains(output, "released:") && !strings.Contains(output, "persistent session") {
			break
		}
	}
	return combined.String()
}

func (lc *lifecycle) assertMergedBase(t *testing.T, project projectFixture) {
	t.Helper()
	app := runCommand(t, commandInput{
		Argv: []string{"git", "--git-dir", project.exchangePath, "show", "main:app.txt"},
	})
	assertContains(t, app, "lifecycle complete for "+project.projectName)

	assertGitShowMissing(t, project.exchangePath, "main:.flow/session/state.json")
	assertGitShowMissing(t, project.exchangePath, "main:.handoff.md")
}

// assertExchangeLacks confirms a marker from another project's merge never
// landed in this project's exchange base.
func (lc *lifecycle) assertExchangeLacks(t *testing.T, exchangePath string, marker string) {
	t.Helper()
	app := runCommand(t, commandInput{
		Argv: []string{"git", "--git-dir", exchangePath, "show", "main:app.txt"},
	})
	if strings.Contains(app, marker) {
		t.Fatalf("exchange %s leaked marker %q:\n%s", exchangePath, marker, app)
	}
}

type commandInput struct {
	Dir     string
	Env     []string
	Argv    []string
	Timeout time.Duration
}

// sanitizedEnviron is os.Environ without inherited Flow or tmux session
// identity. When the suite itself runs inside Flow, leaked FLOW_* values can
// scope child CLI calls to the host project instead of the fixture server.
// When it runs inside a tmux pane, a leaked TMUX would let spawned tmux clients
// reach the hosting server and kill the very session running the tests.
func sanitizedEnviron() []string {
	environ := os.Environ()
	filtered := make([]string, 0, len(environ))
	for _, value := range environ {
		if strings.HasPrefix(value, "FLOW_") || strings.HasPrefix(value, "TMUX=") || strings.HasPrefix(value, "TMUX_PANE=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func runCommand(t *testing.T, input commandInput) string {
	t.Helper()
	timeout := input.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, input.Argv[0], input.Argv[1:]...)
	cmd.Dir = input.Dir
	cmd.Env = append(sanitizedEnviron(), input.Env...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += stderr.String()
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("command timed out after %s: %s\n%s", timeout, strings.Join(input.Argv, " "), output)
	}
	if err != nil {
		t.Fatalf("command failed: %s\nerror: %v\noutput:\n%s", strings.Join(input.Argv, " "), err, output)
	}
	return output
}

func buildBinary(dir string, output string, pkg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", output, pkg)
	cmd.Dir = dir
	outputBytes, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("build %s timed out: %w\n%s", pkg, ctx.Err(), string(outputBytes))
	}
	if err != nil {
		return fmt.Errorf("build %s: %w\n%s", pkg, err, string(outputBytes))
	}
	return nil
}

func sourceRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{"git", "-C", dir}, args...)
	return runCommand(t, commandInput{Argv: argv})
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func firstField(t *testing.T, output string) string {
	t.Helper()
	fields := strings.Fields(output)
	if len(fields) == 0 {
		t.Fatalf("output has no fields:\n%s", output)
	}
	return fields[0]
}

func parseKeyLine(t *testing.T, output string, key string) string {
	t.Helper()
	prefix := key + ":"
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("output missing %s line:\n%s", key, output)
	return ""
}

func assertContains(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output missing %q:\n%s", want, got)
	}
}

func assertGitShowMissing(t *testing.T, exchangePath string, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir", exchangePath, "show", ref)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git show %s unexpectedly succeeded:\n%s", ref, string(output))
	}
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is required for lifecycle tests", name)
	}
}

func freeLocalAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate local address: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func newBrowserContext(t *testing.T, browserPath string) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.UserDataDir(t.TempDir()),
		chromedp.WindowSize(1280, 900),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	return browserCtx, func() {
		browserCancel()
		allocCancel()
		cancel()
	}
}

func navigateAndWaitForText(t *testing.T, ctx context.Context, targetURL string, text string) {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(targetURL),
		waitForText(text),
	); err != nil {
		t.Fatalf("navigate to %s and wait for %q: %v", targetURL, text, err)
	}
}

func waitForPageText(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	if err := chromedp.Run(ctx, waitForText(text)); err != nil {
		t.Fatalf("wait for %q: %v", text, err)
	}
}

func assertSelectorVisible(t *testing.T, ctx context.Context, selector string) {
	t.Helper()
	var present bool
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`selector => !!document.querySelector(selector)`, &present,
		chromedp.WithPollingArgs(selector),
		chromedp.WithPollingTimeout(15*time.Second),
	)); err != nil {
		t.Fatalf("wait for selector %q: %v", selector, err)
	}
}

// Text matching is case-insensitive because the UI styles state labels with
// CSS text-transform, which innerText reflects.
func waitForText(text string) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`text => document.body && document.body.innerText.toLowerCase().includes(text.toLowerCase())`, &matched,
		chromedp.WithPollingArgs(text),
		chromedp.WithPollingTimeout(15*time.Second),
	)
}

func waitForNoSelector(selector string) chromedp.Action {
	var gone bool
	return chromedp.PollFunction(`selector => !document.querySelector(selector)`, &gone,
		chromedp.WithPollingArgs(selector),
		chromedp.WithPollingTimeout(15*time.Second),
	)
}

func findBrowserExecutable() (string, bool) {
	if path := strings.TrimSpace(os.Getenv("FLOW_BROWSER_BIN")); path != "" {
		if executableExists(path) {
			return path, true
		}
		return "", false
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		path, err := exec.LookPath(name)
		if err == nil && executableExists(path) {
			return path, true
		}
	}
	for _, path := range []string{
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
	} {
		if executableExists(path) {
			return path, true
		}
	}
	return "", false
}

func executableExists(path string) bool {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
