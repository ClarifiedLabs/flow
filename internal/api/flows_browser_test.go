package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestWebUIFlowsAndTasksViewsRender is the regression test for the blank
// /ui/flows and /ui/tasks pages: their routes mounted <flow-flows> and
// <flow-tasks> without importing the modules that call
// customElements.define(), so the browser created unresolved elements that
// painted nothing — no console error, no server log, just an empty page.
// The test drives a real browser to both views and asserts each custom
// element is registered and paints its content.
func TestWebUIFlowsAndTasksViewsRender(t *testing.T) {
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

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "Board")

	// The fixture has one project, so the Flows view skips the project
	// chooser and renders both agent-definition catalogs plus the flows table.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/flows", "Global Agent Definitions")
	assertPageContains(t, browserCtx, "Project Agent Definitions")
	assertCustomElementRegistered(t, browserCtx, "flow-flows")

	// The Tasks view renders its state-filter chips and layout toggle even
	// with no tasks to list.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/tasks", "By container")
	assertCustomElementRegistered(t, browserCtx, "flow-tasks")
}

// assertCustomElementRegistered fails unless the browser's custom-element
// registry has a constructor for the tag: an unregistered element never
// upgrades, so its view silently paints nothing.
func assertCustomElementRegistered(t *testing.T, ctx context.Context, tag string) {
	t.Helper()
	var registered bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`customElements.get("`+tag+`") !== undefined`, &registered)); err != nil {
		t.Fatalf("check custom element registration for %q: %v", tag, err)
	}
	if !registered {
		t.Fatalf("custom element %q is not registered; the module defining it was never imported\nbody:\n%s", tag, browserBody(t, ctx))
	}
}
