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
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

const (
	browserTestTimeout        = 2 * time.Minute
	browserStartupTimeout     = time.Minute
	browserTextPollingTimeout = 20 * time.Second
)

func TestWebUIConsoleAutoReleaseRefreshesOpenPage(t *testing.T) {
	t.Parallel()
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

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "No tasks")
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

// TestWebUITopBarNavDropdown drives the top-bar navigation dropdown as the
// primary navigation path: the sidebar is gone, the trigger lives in the top
// bar, the panel opens from the trigger, and clicking a destination navigates
// there, marks it aria-current, and closes the panel.
func TestWebUITopBarNavDropdown(t *testing.T) {
	t.Parallel()
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

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "No tasks")

	// The sidebar is gone: the sticky top bar owns primary navigation through
	// the dropdown trigger.
	var sidebarGone, triggerExposed bool
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.querySelector(".sidebar") === null`, &sidebarGone),
		chromedp.Evaluate(`document.querySelector("header.topbar summary.nav-trigger") !== null`, &triggerExposed),
		chromedp.WaitVisible(`header.topbar summary.nav-trigger`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("inspect top bar shell: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	if !sidebarGone {
		t.Fatalf("old .sidebar element still rendered\nbody:\n%s", browserBody(t, browserCtx))
	}
	if !triggerExposed {
		t.Fatalf("top bar does not expose the nav trigger\nbody:\n%s", browserBody(t, browserCtx))
	}

	// The trigger opens the dropdown panel.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`header.topbar summary.nav-trigger`, chromedp.ByQuery),
		chromedp.WaitVisible(`.nav-menu .nav-panel`, chromedp.ByQuery),
		waitForNavMenuOpen(true),
	); err != nil {
		t.Fatalf("open nav dropdown: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}

	// Clicking the Done entry navigates to /ui/done, marks the link
	// aria-current, and closes the panel.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`.nav-menu .nav-panel a[href="/ui/done"]`, chromedp.ByQuery),
		waitForLocationPath("/ui/done"),
	); err != nil {
		t.Fatalf("navigate through nav dropdown: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	assertActiveNav(t, browserCtx, "/ui/done")
	if err := chromedp.Run(browserCtx, waitForNavMenuOpen(false)); err != nil {
		t.Fatalf("nav panel did not close after navigation: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
}

func activateBrowserConsoleSession(t *testing.T, fixture testFixture) coordinator.StartConsoleSessionResult {
	t.Helper()

	ctx := context.Background()
	workerID := "w-browser-console"
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{"agent.harness.harness": "true"},
	}); err != nil {
		t.Fatalf("register console worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
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
	if _, err := fixture.Sessions.RegisterTerminal(ctx, started.Session.ID, "http://127.0.0.1:7777"); err != nil {
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

func TestBrowserTestContextCancelRemovesUserDataDir(t *testing.T) {
	// Keep this test serial so its extra Chromium process does not compete with
	// the browser UI tests, which intentionally run in parallel.
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found for browser smoke test")
	}

	browserCtx, cancel := newBrowserTestContext(t, browserPath)
	var commandLine []string
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		commandLine, err = browser.GetBrowserCommandLine().Do(ctx)
		return err
	})); err != nil {
		cancel()
		t.Fatalf("read browser command line: %v", err)
	}

	var userDataDir string
	for _, arg := range commandLine {
		if dir, found := strings.CutPrefix(arg, "--user-data-dir="); found {
			userDataDir = dir
			break
		}
	}
	if userDataDir == "" {
		cancel()
		t.Fatalf("browser command line has no user-data-dir: %q", commandLine)
	}
	if _, err := os.Stat(userDataDir); err != nil {
		cancel()
		t.Fatalf("stat active browser user-data-dir: %v", err)
	}

	cancel()
	if _, err := os.Stat(userDataDir); !os.IsNotExist(err) {
		t.Fatalf("browser user-data-dir still exists after cancellation: %v", err)
	}
}

func newBrowserTestContext(t *testing.T, browserPath string) (context.Context, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), browserTestTimeout)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
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

// waitForNavMenuOpen polls until the top-bar nav dropdown reports the wanted
// open state.
func waitForNavMenuOpen(wantOpen bool) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`want => { const menu = document.querySelector(".nav-menu"); return !!menu && menu.open === want; }`, &matched,
		chromedp.WithPollingArgs(wantOpen),
		chromedp.WithPollingTimeout(browserTextPollingTimeout),
	)
}

// waitForLocationPath polls until the SPA router lands on the wanted path.
func waitForLocationPath(path string) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`path => window.location.pathname === path`, &matched,
		chromedp.WithPollingArgs(path),
		chromedp.WithPollingTimeout(browserTextPollingTimeout),
	)
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
