// tasks-view.js unit tests. The view's logic lives in exported functions that
// take plain app/view stand-ins, so these run without the DOM shim: only a
// cookie-less document and a fetch stub are needed for the bulk fan-out.

import test from "node:test";
import assert from "node:assert/strict";

const {
  TASKS_STATE_FILTERS,
  tasksQueryView,
  renderTasksControlsView,
  renderTaskRowView,
  bulkFlowOptionsView,
  taskBulkPathView,
  applyTasksBulkAction,
} = await import("./tasks-view.js");

function fakeApp(overrides = {}) {
  return {
    tasksState: "all",
    tasksProject: "",
    tasksQuery: "",
    tasksSelected: new Set(),
    tasksList: [],
    tasksProjectBadge: false,
    projects: [],
    selectedProjectIDs: () => [],
    setStatus() {},
    async load() {},
    ...overrides,
  };
}

test("tasksQueryView composes project, state and search params", () => {
  const app = fakeApp({ selectedProjectIDs: () => ["p-a", "p-b"] });
  assert.equal(tasksQueryView(app, "all"), "?project=p-a&project=p-b");
  assert.equal(tasksQueryView(app, "unscheduled"), "?project=p-a&project=p-b&state=unscheduled");
  assert.equal(tasksQueryView(app, "in_progress", { q: "flaky" }), "?project=p-a&project=p-b&state=in_progress&q=flaky");

  // No topbar selection and an "all" chip mean the unfiltered aggregate list.
  assert.equal(tasksQueryView(fakeApp(), "all"), "");
  assert.equal(tasksQueryView(fakeApp(), "all", { q: "   " }), "");

  // The in-view project filter narrows the fetch instead of unioning with the
  // topbar selection (repeatable project params would list its tasks twice).
  const narrowed = fakeApp({ tasksProject: "p-b", selectedProjectIDs: () => ["p-a", "p-b"] });
  assert.equal(tasksQueryView(narrowed, "done"), "?project=p-b&state=done");
});

test("renderTasksControlsView paints chips, the project dropdown and the search box", () => {
  const app = fakeApp({
    tasksState: "scheduled",
    tasksProject: "p-1",
    tasksQuery: "flaky",
    projects: [
      { id: "p-1", name: "flow" },
      { id: "p-2", name: "site" },
    ],
  });
  const html = renderTasksControlsView(app);
  assert.equal(TASKS_STATE_FILTERS.length, 5);
  for (const [key] of TASKS_STATE_FILTERS) {
    assert.match(html, new RegExp(`data-tasks-state="${key}"`));
  }
  assert.match(html, /class="chip active" data-tasks-state="scheduled" aria-pressed="true"/);
  assert.match(html, /<option value="">All projects<\/option>/);
  assert.match(html, /<option value="p-1" selected>flow<\/option>/);
  assert.match(html, /<option value="p-2">site<\/option>/);
  assert.match(html, /data-tasks-search[^>]*value="flaky"/);
});

test("renderTaskRowView falls back to unscheduled for a null state and escapes the title", () => {
  const app = fakeApp({ tasksSelected: new Set(["t-0001"]), tasksProjectBadge: true });
  const html = renderTaskRowView(app, {
    ID: "t-0001",
    Title: "Fix <flaky> checkout",
    Priority: 2,
    State: null,
    flow_id: "f-default",
    project_id: "p-1",
    project_name: "flow",
  });
  assert.match(html, /data-task-row="t-0001"/);
  assert.match(html, /data-phase="backlog"/);
  assert.match(html, /data-tasks-select="t-0001" aria-label="Select t-0001" checked/);
  assert.match(html, /Fix &lt;flaky&gt; checkout/);
  assert.match(html, />unscheduled<\/span>/);
  assert.match(html, /href="\/ui\/tasks\/t-0001"/);
  assert.match(html, /card-project-badge">flow<\/span>/);
  assert.match(html, /f-default · p2/);
});

test("renderTaskRowView omits project badge and meta when there is nothing to show", () => {
  const app = fakeApp();
  const html = renderTaskRowView(app, { ID: "t-0002", Title: "Plain", State: "scheduled", project_id: "p-1" });
  assert.match(html, /data-phase="up_next"/);
  assert.match(html, />scheduled<\/span>/);
  assert.doesNotMatch(html, /checked/);
  assert.doesNotMatch(html, /tasks-row-meta/);
});

test("bulkFlowOptionsView renders a disabled placeholder and never preselects a flow", () => {
  const app = fakeApp({
    tasksProject: "p-1",
    flowsByProject: new Map([["p-1", { flows: [{ id: "f-1", name: "Default" }], defaultFlowID: "f-1" }]]),
  });
  const html = bulkFlowOptionsView(app);
  assert.match(html, /<option value="" selected disabled>Choose a flow<\/option>/);
  assert.match(html, /<option value="f-1">Default<\/option>/);
  // flowSelectOptionsView would preselect the project default; a bulk action
  // must only apply a flow the user explicitly picked.
  assert.doesNotMatch(html, /<option value="f-1" selected>/);

  const allProjects = bulkFlowOptionsView(fakeApp());
  assert.match(allProjects, /Pick a project above to choose a flow/);
});

test("taskBulkPathView scopes to the row's project and falls back to the global route", () => {
  assert.equal(taskBulkPathView({ ID: "t-1", project_id: "p-1" }), "/v2/projects/p-1/tasks/t-1");
  assert.equal(taskBulkPathView({ ID: "t-1", project_id: "p-1" }, "/schedule"), "/v2/projects/p-1/tasks/t-1/schedule");
  assert.equal(taskBulkPathView({ ID: "t 1" }, "/reset"), "/v2/tasks/t%201/reset");
});

test("applyTasksBulkAction fans out, reports failures and keeps failed tasks selected", async () => {
  globalThis.document = { cookie: "" };
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    if (url.includes("t-bad")) {
      return { ok: false, status: 409, json: async () => ({ error: { message: "conflict" } }), text: async () => "" };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const statuses = [];
  const app = fakeApp({
    tasksSelected: new Set(["t-good", "t-bad"]),
    tasksList: [
      { ID: "t-good", project_id: "p-1" },
      { ID: "t-bad", project_id: "p-1" },
    ],
    setStatus: (message) => statuses.push(message),
  });

  await applyTasksBulkAction(app, "priority", { querySelector: () => ({ value: "3" }) });

  assert.deepEqual(
    calls.map((call) => [call.url, call.options.method, JSON.parse(call.options.body)]),
    [
      ["/ui/api/v2/projects/p-1/tasks/t-good", "PATCH", { priority: 3 }],
      ["/ui/api/v2/projects/p-1/tasks/t-bad", "PATCH", { priority: 3 }],
    ],
  );
  assert.deepEqual([...app.tasksSelected], ["t-bad"], "the failed task stays selected");
  assert.match(statuses.at(-1), /priority: 1 updated, 1 failed/);
  assert.match(statuses.at(-1), /t-bad: conflict/);
});

test("applyTasksBulkAction maps schedule, reset and retry onto the per-task endpoints", async () => {
  globalThis.document = { cookie: "" };
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, method: options.method });
    return { ok: true, status: 200, json: async () => ({}) };
  };
  for (const [action, suffix] of [["schedule", "/schedule"], ["reset", "/reset"], ["retry", "/workflow/retry"]]) {
    calls.length = 0;
    const app = fakeApp({ tasksSelected: new Set(["t-1"]), tasksList: [{ ID: "t-1", project_id: "p-1" }] });
    await applyTasksBulkAction(app, action, { querySelector: () => null });
    assert.deepEqual(calls, [{ url: `/ui/api/v2/projects/p-1/tasks/t-1${suffix}`, method: "POST" }], action);
  }
});

test("applyTasksBulkAction validates input before any fetch", async () => {
  globalThis.document = { cookie: "" };
  let fetched = 0;
  globalThis.fetch = async () => {
    fetched += 1;
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const app = fakeApp({ tasksSelected: new Set(["t-1"]), tasksList: [{ ID: "t-1", project_id: "p-1" }] });
  const statuses = [];
  app.setStatus = (message) => statuses.push(message);

  await applyTasksBulkAction(app, "priority", { querySelector: () => ({ value: "-1" }) });
  await applyTasksBulkAction(app, "priority", { querySelector: () => ({ value: "1.5" }) });
  await applyTasksBulkAction(app, "flow", { querySelector: () => ({ value: "" }) });

  assert.equal(fetched, 0);
  assert.match(statuses[0], /whole number/);
  assert.match(statuses[1], /whole number/);
  assert.match(statuses[2], /Choose a flow/);
});
