package api

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	"github.com/chromedp/chromedp"
)

const (
	browserTestTimeout        = 2 * time.Minute
	browserStartupTimeout     = time.Minute
	browserTextPollingTimeout = 20 * time.Second
)

func TestWebUIBrowserSmokeRoutesAndDeepLinks(t *testing.T) {
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found for browser smoke test")
	}

	fixture := newTestFixture(t)
	ctx := context.Background()
	backlog, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title: "Browser backlog issue",
		Body:  "## Overview\n\n- first point\n- second point",
	})
	if err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	started, exchangePath := readyApprovedMergeChange(t, fixture, "Browser change detail")
	humanReviewStarted := startAuthorSessionForStatusTestWithWorker(t, fixture, "Browser human review", "w-browser-human")
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	humanReviewHead := pushBrowserSmokeBranch(t, exchangePath, humanReviewStarted.Change.Branch)
	doJSONRequestAs(t, fixture.Server, humanReviewStarted.Token, "POST", "/v1/sessions/"+humanReviewStarted.Session.ID+"/ready", readySessionRequest{
		HeadSHA: humanReviewHead,
	}, 200, nil)
	humanCheck, err := fixture.Checks.GetCheck(ctx, humanReviewStarted.Session.IssueID, "human-review")
	if err != nil {
		t.Fatalf("get human review check: %v", err)
	}
	if humanCheck.Verdict != coordinator.CheckPending {
		t.Fatalf("human review check = %+v, want pending", humanCheck)
	}
	if _, err := fixture.Threads.CreateThread(ctx, coordinator.CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: change.HeadSHA,
		FilePath:        "app.go",
		Line:            1,
		Context:         "const Value = 1",
		Body:            "Browser review note",
		Actor:           "reviewer",
	}); err != nil {
		t.Fatalf("create review thread: %v", err)
	}
	triage, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title:              "Browser triage issue",
		CreatedBy:          coordinator.ActorAgent,
		CreatedBySessionID: &started.Session.ID,
	})
	if err != nil {
		t.Fatalf("create triage issue: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	bootstrap, err := fixture.WebSessions.CreateBootstrap(ctx, time.Minute)
	if err != nil {
		t.Fatalf("create web bootstrap: %v", err)
	}

	browserCtx, cancel := newBrowserTestContext(t, browserPath)
	defer cancel()

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "Browser backlog issue")
	assertActiveNav(t, browserCtx, "/ui/board")

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/board", backlog.ID)
	assertPageContains(t, browserCtx, "Browser triage issue")
	assertActiveNav(t, browserCtx, "/ui/board")

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/triage", triage.ID)
	assertPageContains(t, browserCtx, "Browser triage issue")
	assertPageNotContains(t, browserCtx, "Browser backlog issue")
	assertActiveNav(t, browserCtx, "/ui/triage")
	reloadAndWaitForText(t, browserCtx, "Browser triage issue")

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/console", "Start Console")
	assertPageContains(t, browserCtx, "Harness")
	assertActiveNav(t, browserCtx, "/ui/console")
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-start-console]`, chromedp.ByQuery),
		waitForText("Release Console"),
	); err != nil {
		t.Fatalf("start console through browser UI: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	consoleState, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console after browser start: %v", err)
	}
	if !consoleState.Active || consoleState.Job == nil {
		t.Fatalf("console state after browser start = %+v", consoleState)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-release-console]`, chromedp.ByQuery),
		waitForText("Start Console"),
	); err != nil {
		t.Fatalf("release console through browser UI: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	consoleState, err = fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console after browser release: %v", err)
	}
	if consoleState.Active {
		t.Fatalf("console state after browser release = %+v, want inactive", consoleState)
	}

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/projects/"+fixture.Project.ID+"/issues/"+started.Session.IssueID, "Browser change detail")
	assertPageContains(t, browserCtx, "Ready Change")
	var hasLifecycleChart bool
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.querySelector(".lifecycle-chart svg") !== null`, &hasLifecycleChart)); err != nil {
		t.Fatalf("evaluate lifecycle chart presence: %v", err)
	}
	if !hasLifecycleChart {
		t.Fatalf("expected lifecycle chart svg on issue detail\nbody:\n%s", browserBody(t, browserCtx))
	}
	reloadAndWaitForText(t, browserCtx, "Browser change detail")

	// The backlog issue's markdown body must render as real HTML (a .md block
	// with the heading/list it contains), not as an escaped plain-text blob.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/projects/"+fixture.Project.ID+"/issues/"+backlog.ID, "Overview")
	var hasMarkdownBody bool
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.querySelector(".md h2") !== null && document.querySelector(".md li") !== null`, &hasMarkdownBody)); err != nil {
		t.Fatalf("evaluate markdown body presence: %v", err)
	}
	if !hasMarkdownBody {
		t.Fatalf("expected rendered markdown (.md h2/.md li) on issue detail\nbody:\n%s", browserBody(t, browserCtx))
	}

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/changes/"+change.ID, "Browser review note")
	waitForPageText(t, browserCtx, "files 1")
	reloadAndWaitForText(t, browserCtx, "Browser review note")

	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/changes/"+humanReviewStarted.Change.ID, "human-review")
	waitForPageText(t, browserCtx, "files 1")
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-human-review-approve="`+humanReviewStarted.Session.IssueID+`"]`, chromedp.ByQuery),
		waitForText("approved via web UI"),
	); err != nil {
		t.Fatalf("approve human review through browser UI: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	humanCheck, err = fixture.Checks.GetCheck(ctx, humanReviewStarted.Session.IssueID, "human-review")
	if err != nil {
		t.Fatalf("get approved human review check: %v", err)
	}
	if humanCheck.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("human review check after browser approval = %+v, want satisfied", humanCheck)
	}
}

func TestWebUIConsoleAutoReleaseRefreshesOpenPage(t *testing.T) {
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found for browser smoke test")
	}

	fixture := newTestFixture(t)
	ctx := context.Background()
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	bootstrap, err := fixture.WebSessions.CreateBootstrap(ctx, time.Minute)
	if err != nil {
		t.Fatalf("create web bootstrap: %v", err)
	}

	browserCtx, cancel := newBrowserTestContext(t, browserPath)
	defer cancel()

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "No issues")
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/console", "Start Console")
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-start-console]`, chromedp.ByQuery),
		waitForText("Release Console"),
	); err != nil {
		t.Fatalf("start console through browser UI: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	activateBrowserConsoleSession(t, fixture)
	if err := chromedp.Run(browserCtx, chromedp.WaitVisible(`iframe.terminal-frame`, chromedp.ByQuery)); err != nil {
		t.Fatalf("wait for console terminal iframe: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	if released, err := fixture.Sessions.ReleaseConsole(ctx); err != nil {
		t.Fatalf("auto-release console session: %v", err)
	} else if released.Active {
		t.Fatalf("auto-release console state = %+v, want inactive", released)
	}
	if err := chromedp.Run(browserCtx, waitForText("Start Console")); err != nil {
		t.Fatalf("console page did not refresh after auto-release: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	assertPageNotContains(t, browserCtx, "Release Console")
	consoleState, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console after auto-release: %v", err)
	}
	if consoleState.Active {
		t.Fatalf("console state after auto-release = %+v, want inactive", consoleState)
	}
}

func activateBrowserConsoleSession(t *testing.T, fixture testFixture) coordinator.StartConsoleSessionResult {
	t.Helper()

	ctx := context.Background()
	workerID := "w-browser-console"
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{"agent.harness.codex": "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register console worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim console job: %v", err)
	}
	if !ok || claimed.Job.Role != flowworker.RoleConsole {
		t.Fatalf("claim console job = %+v ok=%t, want console", claimed.Job, ok)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark console job running: %v", err)
	}
	started, err := fixture.Sessions.StartConsoleSession(ctx, coordinator.StartConsoleSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: workerID,
	})
	if err != nil {
		t.Fatalf("start console session: %v", err)
	}
	if _, err := fixture.Sessions.RegisterTerminalTarget(ctx, started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register console terminal target: %v", err)
	}

	return started
}

func pushBrowserSmokeBranch(t *testing.T, exchangePath string, branch string) string {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	runAPIGit(t, "", "clone", exchangePath, repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	runAPIGit(t, repoPath, "checkout", "-b", branch, "origin/main")
	writeAPIFile(t, repoPath, "human.go", "package app\n\nconst HumanReview = true\n")
	runAPIGit(t, repoPath, "add", "human.go")
	runAPIGit(t, repoPath, "commit", "-m", "add human review target")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, branch+":"+branch)

	return headSHA
}

func newBrowserTestContext(t *testing.T, browserPath string) (context.Context, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), browserTestTimeout)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.UserDataDir(t.TempDir()),
		chromedp.WindowSize(1280, 900),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WSURLReadTimeout(browserStartupTimeout),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	return browserCtx, func() {
		browserCancel()
		allocCancel()
		cancel()
	}
}

func navigateAndWaitForText(t *testing.T, ctx context.Context, url string, text string) {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		waitForText(text),
	); err != nil {
		t.Fatalf("navigate to %s and wait for %q: %v", url, text, err)
	}
}

func reloadAndWaitForText(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Reload(),
		waitForText(text),
	); err != nil {
		t.Fatalf("reload and wait for %q: %v", text, err)
	}
}

func waitForPageText(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	if err := chromedp.Run(ctx, waitForText(text)); err != nil {
		t.Fatalf("wait for %q: %v", text, err)
	}
}

func assertPageContains(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	body := browserBody(t, ctx)
	if !strings.Contains(strings.ToLower(body), strings.ToLower(text)) {
		t.Fatalf("page body missing %q:\n%s", text, body)
	}
}

func assertPageNotContains(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	body := browserBody(t, ctx)
	if strings.Contains(strings.ToLower(body), strings.ToLower(text)) {
		t.Fatalf("page body unexpectedly contained %q:\n%s", text, body)
	}
}

func browserBody(t *testing.T, ctx context.Context) string {
	t.Helper()
	var body string
	if err := chromedp.Run(ctx, chromedp.Text("body", &body, chromedp.ByQuery)); err != nil {
		t.Fatalf("read page body: %v", err)
	}
	return body
}

func assertActiveNav(t *testing.T, ctx context.Context, want string) {
	t.Helper()
	var active string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector(".nav a[aria-current='page']")?.getAttribute("href") || ""`, &active)); err != nil {
		t.Fatalf("read active nav: %v", err)
	}
	if active != want {
		t.Fatalf("active nav = %q, want %q", active, want)
	}
}

// Text matching is case-insensitive because the UI styles state labels with
// CSS text-transform, which innerText reflects.
func waitForText(text string) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`text => document.body && document.body.innerText.toLowerCase().includes(text.toLowerCase())`, &matched,
		chromedp.WithPollingArgs(text),
		chromedp.WithPollingTimeout(browserTextPollingTimeout),
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
