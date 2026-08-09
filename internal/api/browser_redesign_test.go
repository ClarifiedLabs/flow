package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/chromedp/chromedp"
)

// These are the behaviours the custom-element conversion exists to deliver,
// and none of them can be checked from a string assertion: they are all about
// what survives a repaint, and what happens where you clicked.
func TestWebUIRedesignSurvivesRepaintsAndKeepsDisclosureLocal(t *testing.T) {
	t.Parallel()
	browserPath, ok := findBrowserExecutable()
	if !ok {
		t.Skip("no Chromium or Chrome executable found for browser smoke test")
	}

	fixture := newTestFixture(t)
	ctx := context.Background()
	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	flow := newBoardFixtureFlow(t, fixture, "browser redesign")
	task, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{
		Title: "Retry budget for failed check nodes", FlowID: flow.ID, CreatedBy: coordinator.ActorHuman,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID); err != nil {
		t.Fatalf("schedule task: %v", err)
	}

	bootstrap, err := fixture.WebSessions.CreateBootstrap(ctx, time.Minute)
	if err != nil {
		t.Fatalf("create web bootstrap: %v", err)
	}

	browserCtx, cancel := newBrowserTestContext(t, browserPath)
	defer cancel()

	navigateAndWaitForText(t, browserCtx, httpServer.URL+webLoginPath(bootstrap.Token), "Retry budget")

	// The board renders as elements, not as one innerHTML blob.
	var cards int
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.querySelectorAll("flow-task-card").length`, &cards),
	); err != nil {
		t.Fatalf("count task cards: %v", err)
	}
	if cards == 0 {
		t.Fatalf("no flow-task-card elements rendered\nbody:\n%s", browserBody(t, browserCtx))
	}

	// `v` toggles the dense view and the choice persists across a reload,
	// because which view suits you is not a property of one visit.
	if err := chromedp.Run(browserCtx,
		chromedp.KeyEvent("v"),
		chromedp.WaitVisible(`flow-board-table`, chromedp.ByQuery),
		chromedp.Reload(),
		chromedp.WaitVisible(`flow-board-table`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("toggle board view with v and persist it: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}

	// Task detail: the tab you chose must outlive a poll. Setting fresh data
	// on the element is exactly what the ten-second poll does.
	navigateAndWaitForText(t, browserCtx, httpServer.URL+"/ui/tasks/"+task.ID, "Overview")
	var activeAfterRepaint string
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`flow-tab-strip [data-tab="activity"]`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			const detail = document.querySelector("flow-task-detail");
			detail.data = detail.data;
			return document.querySelector("flow-tab-strip").active;
		})()`, &activeAfterRepaint),
	); err != nil {
		t.Fatalf("switch tab and repaint: %v\nbody:\n%s", err, browserBody(t, browserCtx))
	}
	if activeAfterRepaint != "activity" {
		t.Fatalf("active tab after repaint = %q, want activity — a poll must not reset the reader's tab", activeAfterRepaint)
	}
}
