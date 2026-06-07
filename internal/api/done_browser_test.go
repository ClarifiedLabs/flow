package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestWebUIDoneViewSurfacesClosedWork drives the real UI: the Done page lists
// closed issues with their merged change, the outcome filter narrows the list,
// the density toggle switches to compact rows, the nav badge shows the closed
// count, and the board grows a Done column.
func TestWebUIDoneViewSurfacesClosedWork(t *testing.T) {
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found for browser smoke test")
	}

	fixture := newTestFixture(t)
	ctx := context.Background()
	merged, rejected, abandoned := seedClosedIssues(t, fixture)

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	bootstrap, err := fixture.WebSessions.CreateBootstrap(ctx, time.Minute)
	if err != nil {
		t.Fatalf("create web bootstrap: %v", err)
	}

	browserCtx, cancel := newBrowserTestContext(t, browserPath)
	defer cancel()

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "Board")

	// Done page lists every terminal issue with its merged change link.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/done", merged.ID)
	assertActiveNav(t, browserCtx, "/ui/done")
	assertPageContains(t, browserCtx, rejected.ID)
	assertPageContains(t, browserCtx, abandoned.ID)
	assertPageContains(t, browserCtx, "c-0001")

	// The nav badge reflects the total closed count (polled from /v1/sidebar).
	if err := chromedp.Run(browserCtx, waitForDoneBadge("3")); err != nil {
		t.Fatalf("done nav badge did not reach 3: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}

	// Filtering by Merged narrows to the merged issue and drops the others.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-done-outcome="merged"]`, chromedp.ByQuery),
		waitForTextGone(rejected.ID),
	); err != nil {
		t.Fatalf("filter Done by merged: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	assertPageContains(t, browserCtx, merged.ID)
	assertPageNotContains(t, browserCtx, abandoned.ID)

	// Back to all, then switch to compact density (renders .done-row rows).
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-done-outcome="all"]`, chromedp.ByQuery),
		waitForText(rejected.ID),
		chromedp.Click(`button[data-done-density="compact"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.done-row`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("toggle Done density to compact: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}

	// The board grows a Done column listing the same terminal issues.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/board", merged.ID)
	var hasDoneLane bool
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.querySelector('.lane[data-lane="done"]') !== null`, &hasDoneLane),
	); err != nil {
		t.Fatalf("evaluate board Done lane presence: %v", err)
	}
	if !hasDoneLane {
		t.Fatalf("board is missing the Done column\nbody:\n%s", browserBody(t, browserCtx))
	}
	assertPageContains(t, browserCtx, abandoned.ID)
}

// waitForTextGone polls until the page no longer contains text.
func waitForTextGone(text string) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`text => document.body && !document.body.innerText.toLowerCase().includes(text.toLowerCase())`, &matched,
		chromedp.WithPollingArgs(text),
		chromedp.WithPollingTimeout(browserTextPollingTimeout),
	)
}

// waitForDoneBadge polls until the Done nav entry shows the given count.
func waitForDoneBadge(count string) chromedp.Action {
	var matched bool
	return chromedp.PollFunction(`count => { const el = document.querySelector('.nav a[href="/ui/done"] .nav-status'); return !!el && el.textContent.trim() === count; }`, &matched,
		chromedp.WithPollingArgs(count),
		chromedp.WithPollingTimeout(browserTextPollingTimeout),
	)
}
