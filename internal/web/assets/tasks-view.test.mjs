// elements/tasks-view.js unit tests. The view's logic lives in exported functions that
// take plain app/view stand-ins, so these run without the DOM shim: only a
// cookie-less document and a fetch stub are needed for the bulk fan-out.

import test from "node:test";
import assert from "node:assert/strict";

const {
  TASKS_STATE_FILTERS,
  tasksQueryView,
  tasksRootFromSearch,
  tasksRootHref,
  filteredTasksView,
  taskContainerGroupsView,
  pruneTasksSelectionView,
  renderTasksControlsView,
  renderTasksListView,
  renderTaskRowView,
  bulkFlowOptionsView,
  taskBulkPathView,
  applyTasksBulkAction,
  toggleTasksState,
} = await import("./elements/tasks-view.js");
const { buildWorkItemIndex } = await import("./work-item-model.js");

function fakeApp(overrides = {}) {
  return {
    tasksState: new Set(["unscheduled", "scheduled", "in_progress", "done"]),
    tasksProject: "",
    tasksQuery: "",
    tasksRoot: "",
    tasksLayout: "flat",
    tasksSelected: new Set(),
    tasksList: [],
    tasksWorkIndexes: new Map(),
    tasksProjectBadge: false,
    projects: [],
    selectedProjectIDs: () => [],
    setStatus() {},
    async load() {},
    ...overrides,
  };
}

test("tasksQueryView composes project, repeatable state and search params", () => {
  const app = fakeApp({ selectedProjectIDs: () => ["p-a", "p-b"] });
  const all = new Set(["unscheduled", "scheduled", "in_progress", "done"]);
  assert.equal(
    tasksQueryView(app, all),
    "?project=p-a&project=p-b&state=unscheduled&state=scheduled&state=in_progress&state=done",
  );
  assert.equal(tasksQueryView(app, new Set(["unscheduled"])), "?project=p-a&project=p-b&state=unscheduled");
  assert.equal(tasksQueryView(app, new Set(["scheduled", "done"])), "?project=p-a&project=p-b&state=scheduled&state=done");
  assert.equal(tasksQueryView(app, new Set(["in_progress"]), { q: "flaky" }), "?project=p-a&project=p-b&state=in_progress&q=flaky");

  // No topbar selection and every state selected mean the aggregate list.
  assert.equal(tasksQueryView(fakeApp(), all), "?state=unscheduled&state=scheduled&state=in_progress&state=done");
  assert.equal(tasksQueryView(fakeApp(), all, { q: "   " }), "?state=unscheduled&state=scheduled&state=in_progress&state=done");

  // An empty selection adds no state params (the view itself skips the fetch
  // in that case, because the server treats absent states as no filter).
  assert.equal(tasksQueryView(app, new Set()), "?project=p-a&project=p-b");

  // The in-view project filter narrows the fetch instead of unioning with the
  // topbar selection (repeatable project params would list its tasks twice).
  const narrowed = fakeApp({ tasksProject: "p-b", selectedProjectIDs: () => ["p-a", "p-b"] });
  assert.equal(tasksQueryView(narrowed, new Set(["done"])), "?project=p-b&state=done");
});

function hierarchyApp(overrides = {}) {
  const items = [
    { id: "e-1", kind: "epic", title: "Launch initiative" },
    { id: "f-1", kind: "feature", title: "Checkout", parent_item_id: "e-1" },
    { id: "t-nested", kind: "task", title: "Nested", parent_item_id: "f-1" },
    { id: "t-standalone", kind: "task", title: "Standalone" },
    { id: "t-orphan", kind: "task", title: "Orphan", parent_item_id: "missing" },
  ];
  const task = (ID, Title) => ({ ID, Title, State: "scheduled", project_id: "p-1" });
  return fakeApp({
    projects: [{ id: "p-1", name: "Flow" }],
    tasksList: [
      task("t-nested", "Nested"),
      task("t-standalone", "Standalone"),
      task("t-orphan", "Orphan"),
      task("t-missing", "Missing summary"),
    ],
    tasksWorkIndexes: new Map([["p-1", buildWorkItemIndex({ items })]]),
    ...overrides,
  });
}

test("renderTasksControlsView paints chips, the project dropdown and the search box", () => {
  const app = fakeApp({
    tasksState: new Set(["scheduled", "done"]),
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
  assert.match(html, /class="chip active" data-tasks-state="done" aria-pressed="true"/);
  assert.doesNotMatch(html, /data-tasks-state="all"[^>]*aria-pressed="true"/);
  assert.doesNotMatch(html, /data-tasks-state="unscheduled"[^>]*aria-pressed="true"/);
  assert.doesNotMatch(html, /data-tasks-state="in_progress"[^>]*aria-pressed="true"/);
  assert.match(html, /<option value="">All projects<\/option>/);
  assert.match(html, /<option value="p-1" selected>flow<\/option>/);
  assert.match(html, /<option value="p-2">site<\/option>/);
  assert.match(html, /data-tasks-search[^>]*value="flaky"/);
});

test("container controls expose a persisted layout toggle and top-level root filter", () => {
  const app = hierarchyApp({ tasksLayout: "container", tasksRoot: "e-1" });
  const html = renderTasksControlsView(app);
  assert.match(html, /data-tasks-layout="flat" aria-pressed="false"/);
  assert.match(html, /class="chip active" data-tasks-layout="container" aria-pressed="true"/);
  assert.match(html, /data-tasks-root aria-label="Filter by top-level container"/);
  assert.match(html, /<option value="e-1" selected>Launch initiative · e-1<\/option>/);
  assert.match(html, /<option value="standalone">Standalone<\/option>/);
  assert.match(html, /<option value="unknown">Unknown<\/option>/);
  assert.doesNotMatch(html, /<option value="f-1"/, "the filter uses top-level rather than nearest containers");
});

test("root deep links preserve other UI params and never enter the aggregate tasks query", () => {
  assert.equal(tasksRootFromSearch("?project=p-1&root=%20e-1%20&state=done"), "e-1");
  assert.equal(
    tasksRootHref("?project=p-1&state=done&state=scheduled&q=flaky", "e / 1"),
    "/ui/tasks?project=p-1&state=done&state=scheduled&q=flaky&root=e+%2F+1",
  );
  assert.equal(tasksRootHref("?project=p-1&state=done&root=e-1", ""), "/ui/tasks?project=p-1&state=done");
  const app = fakeApp({ tasksProject: "p-1", tasksRoot: "e-1" });
  assert.equal(tasksQueryView(app, new Set(["done"]), { q: "flaky" }), "?project=p-1&state=done&q=flaky");
});

test("root filtering classifies nested, standalone, orphaned and missing-summary tasks", () => {
  const app = hierarchyApp();
  assert.deepEqual(taskContainerGroupsView(app).map((group) => [group.id, group.tasks.map((task) => task.ID)]), [
    ["e-1", ["t-nested"]],
    ["standalone", ["t-standalone"]],
    ["unknown", ["t-orphan", "t-missing"]],
  ]);

  app.tasksRoot = "e-1";
  assert.deepEqual(filteredTasksView(app).map((task) => task.ID), ["t-nested"]);
  app.tasksRoot = "unknown";
  assert.deepEqual(filteredTasksView(app).map((task) => task.ID), ["t-orphan", "t-missing"]);
});

test("renderTasksListView groups by top-level container with Standalone and Unknown sections", () => {
  const list = { innerHTML: "" };
  const app = hierarchyApp({ tasksLayout: "container" });
  app.querySelector = (selector) => (selector === ".tasks-list" ? list : null);
  renderTasksListView(app);
  assert.match(list.innerHTML, /data-tasks-group="e-1"/);
  assert.match(list.innerHTML, /data-tasks-group="standalone"/);
  assert.match(list.innerHTML, /data-tasks-group="unknown"/);
  assert.doesNotMatch(list.innerHTML, /data-tasks-group="f-1"/);
  assert.equal((list.innerHTML.match(/class="tasks-group-list" role="list"/g) || []).length, 3);
  assert.equal((list.innerHTML.match(/class="tasks-row" role="listitem"/g) || []).length, 4);
  assert.equal((list.innerHTML.match(/data-task-row=/g) || []).length, 4);
});

test("flat rendering keeps aggregate order and adds only the top-level ancestor breadcrumb", () => {
  const list = { innerHTML: "" };
  const app = hierarchyApp({ tasksLayout: "flat" });
  app.querySelector = (selector) => (selector === ".tasks-list" ? list : null);
  renderTasksListView(app);
  assert.doesNotMatch(list.innerHTML, /class="tasks-group"/);
  assert.match(list.innerHTML, /Select all 4 visible<\/label>\s*<div class="tasks-flat-list" role="list">/);
  assert.equal((list.innerHTML.match(/class="tasks-row" role="listitem"/g) || []).length, 4);
  assert.ok(list.innerHTML.indexOf('data-task-row="t-nested"') < list.innerHTML.indexOf('data-task-row="t-standalone"'));
  assert.match(list.innerHTML, /class="tasks-row-breadcrumb"[^>]*>.*Launch initiative.*<\/nav>/s);
  assert.doesNotMatch(list.innerHTML, />Checkout<\/a><span aria-hidden="true">\/<\/span><\/nav>/, "the nearest feature is not used as the top-level crumb");
  assert.equal((list.innerHTML.match(/class="tasks-row-breadcrumb"/g) || []).length, 1, "standalone and unknown tasks have no fake ancestor");
});

test("selection pruning follows the currently visible root scope", () => {
  const app = hierarchyApp({ tasksRoot: "unknown", tasksSelected: new Set(["t-nested", "t-orphan", "t-missing"]) });
  pruneTasksSelectionView(app);
  assert.deepEqual([...app.tasksSelected], ["t-orphan", "t-missing"]);
});

test("renderTasksListView hints at the state chips when none are selected", () => {
  const list = { innerHTML: "" };
  const app = fakeApp({ tasksState: new Set(), tasksList: [] });
  app.querySelector = (selector) => (selector === ".tasks-list" ? list : null);
  renderTasksListView(app);
  assert.match(list.innerHTML, /No states selected/);
  assert.match(list.innerHTML, /pick All or a state chip/);
});

test("renderTasksListView keeps the plain empty rendering for a selection that matches nothing", () => {
  const list = { innerHTML: "" };
  const app = fakeApp({ tasksState: new Set(["done"]), tasksList: [] });
  app.querySelector = (selector) => (selector === ".tasks-list" ? list : null);
  renderTasksListView(app);
  assert.equal(list.innerHTML, `<div class="empty">No tasks</div>`);
  assert.doesNotMatch(list.innerHTML, /No states selected/);
});

test("renderTasksListView paints rows for a non-empty list", () => {
  const list = { innerHTML: "" };
  const app = fakeApp({ tasksState: new Set(["done"]), tasksList: [{ ID: "t-1", Title: "Shipped", State: "done" }] });
  app.querySelector = (selector) => (selector === ".tasks-list" ? list : null);
  renderTasksListView(app);
  assert.match(list.innerHTML, /data-task-row="t-1"/);
  assert.doesNotMatch(list.innerHTML, /class="empty"/);
});

test("renderTasksControlsView lights All when every state is selected", () => {
  const app = fakeApp({ tasksState: new Set(["unscheduled", "scheduled", "in_progress", "done"]) });
  const html = renderTasksControlsView(app);
  assert.match(html, /class="chip active" data-tasks-state="all" aria-pressed="true"/);
  assert.equal((html.match(/data-tasks-state="[^"]+"[^>]*aria-pressed="true"/g) || []).length, 5, "All plus all four state chips are pressed");
});

test("toggleTasksState flips one state chip and All selects or clears every state", () => {
  const all = new Set(["unscheduled", "scheduled", "in_progress", "done"]);
  assert.deepEqual(toggleTasksState(all, "done"), new Set(["unscheduled", "scheduled", "in_progress"]));
  assert.deepEqual(toggleTasksState(new Set(["unscheduled"]), "scheduled"), new Set(["unscheduled", "scheduled"]));
  assert.deepEqual(toggleTasksState(new Set(), "unscheduled"), new Set(["unscheduled"]));
  assert.deepEqual(toggleTasksState(new Set(), "all"), all);
  assert.deepEqual(toggleTasksState(all, "all"), new Set());
  assert.deepEqual(toggleTasksState(new Set(["done"]), "all"), all);
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

test("applyTasksBulkAction survives hostile rejection proxies and still reports the bulk status", async () => {
  globalThis.document = { cookie: "" };
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    if (url.includes("t-throws")) {
      // A rejected Proxy whose message getter throws: the unguarded
      // `${result.reason?.message || result.reason}` read would make the
      // template literal itself throw inside the forEach, aborting the
      // status report before setStatus ran. failureMessage stays total.
      return Promise.reject(new Proxy(new Error("boom"), {
        get(target, prop) {
          if (prop === "message") throw new Error("message trap");
          return Reflect.get(target, prop);
        },
      }));
    }
    if (url.includes("t-noproto")) {
      // A rejected Proxy whose prototype lookup throws: even the instanceof
      // check inside failureMessage is guarded.
      return Promise.reject(new Proxy({}, {
        getPrototypeOf() {
          throw new Error("prototype trap");
        },
      }));
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const statuses = [];
  const app = fakeApp({
    tasksSelected: new Set(["t-ok", "t-throws", "t-noproto"]),
    tasksList: [
      { ID: "t-ok", project_id: "p-1" },
      { ID: "t-throws", project_id: "p-1" },
      { ID: "t-noproto", project_id: "p-1" },
    ],
    setStatus: (message) => statuses.push(message),
  });

  await applyTasksBulkAction(app, "priority", { querySelector: () => ({ value: "3" }) });

  assert.equal(calls.length, 3);
  assert.deepEqual(
    [...app.tasksSelected].sort(),
    ["t-noproto", "t-throws"],
    "both hostile-rejected tasks stay selected",
  );
  assert.match(statuses.at(-1), /priority: 1 updated, 2 failed/);
  assert.match(statuses.at(-1), /t-throws: Request failed/);
  assert.match(statuses.at(-1), /t-noproto: Request failed/);
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
