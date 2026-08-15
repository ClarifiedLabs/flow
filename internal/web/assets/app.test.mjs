import assert from "node:assert/strict";
import { test } from "node:test";
import { actionScope, applyBusyState, failureMessage, gateResponsePending, handleAction, inFlight, pendingStatus, threadClaimPending } from "./actions.js";
import { startConsoleView } from "./actions/console.js";
import { handleFormSubmit, formBusyKey } from "./forms.js";
import { workflowStepCanBeSkipped } from "./task-view.js";
import { renderTranscriptButton } from "./terminal.js";
import { ActionButton, InlineDOMElement, RepaintingInlineDOMElement, SmokeDetails, SmokeElement, SmokeLink, SmokeNav, applyContext, browserSmokeHarness, consoleDocument, consoleImports, deferred, findInlineTerminal, flushAsync, inlineDocument, mountableContent, normalize, scriptContext } from "./test-helpers.mjs";

const DIFF_MODE_STORAGE_KEY = "flow.ui.diffMode";

test("terminal buttons in table rows expand a full-width inline terminal row", async () => {
  const context = await scriptContext({}, { document: inlineDocument() });
  const row = new InlineDOMElement("tr");
  row.cells = [new InlineDOMElement("td"), new InlineDOMElement("td"), new InlineDOMElement("td")];
  const terminalButton = new InlineDOMElement("button");
  terminalButton.closest = (selector) => (selector === "tr" ? row : null);
  const app = new context.FlowApp();
  app.querySelector = () => new InlineDOMElement();

  const mount = context.inlineTerminalMount(terminalButton, app);
  const terminalRow = row.nextElementSibling;

  assert.equal(terminalRow.className, "inline-terminal-row");
  assert.equal(terminalRow.dataset.inlineTerminalRow, "true");
  assert.equal(terminalRow.children[0].colSpan, 3);
  assert.equal(terminalRow.children[0].children[0], mount);
  assert.equal(context.inlineTerminalMount(terminalButton, app), mount);
});

test("inline terminal renders a Hide button next to the pop-out button", async () => {
  const context = await scriptContext();

  const html = context.renderInlineTerminal(
    "session",
    "s-0001",
    `<iframe class="terminal-frame"></iframe>`,
    "/v2/sessions/s-0001/terminal-login?token=abc",
  );

  assert.match(html, /data-terminal-popout="\/v2\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(html, /data-terminal-hide/);
  assert.match(html, />Hide</);
  const hideIndex = html.indexOf("data-terminal-hide");
  const popOutIndex = html.indexOf("data-terminal-popout");
  assert.ok(popOutIndex >= 0 && hideIndex > popOutIndex, "Hide button follows the pop-out button");
});

test("terminal modal renders a Hide button next to the pop-out button", async () => {
  const context = await scriptContext();

  const html = context.renderTerminalDialog(
    "session",
    "s-0001",
    `<iframe class="terminal-frame"></iframe>`,
    "/v2/sessions/s-0001/terminal-login?token=abc",
  );

  assert.match(html, /data-terminal-popout="\/v2\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(html, /data-terminal-close/);
  assert.match(html, />Hide</);
  assert.doesNotMatch(html, />Close</);
  const hideIndex = html.indexOf("data-terminal-close");
  const popOutIndex = html.indexOf("data-terminal-popout");
  assert.ok(popOutIndex >= 0 && hideIndex > popOutIndex, "Hide button follows the pop-out button");
});

test("inline terminal Hide button removes the terminal mount", async () => {
  const context = await scriptContext({}, { document: inlineDocument() });
  const mount = new InlineDOMElement("div");
  mount.dataset.inlineTerminal = "true";
  const removed = [];
  mount.remove = () => removed.push(mount);
  const hideButton = new InlineDOMElement("button");
  hideButton.closest = (selector) => (selector === '[data-inline-terminal="true"]' ? mount : null);

  assert.equal(context.hideInlineTerminal(hideButton), true);
  assert.deepEqual(removed, [mount]);
});

test("terminal route embeds owner-authenticated login path", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/sessions/s-0001/terminal" },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v2/sessions/s-0001/terminal-login?token=abc123" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(title.textContent, "Terminal");
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects");
  assert.equal(fetchCalls[1].path, "/ui/api/v2/sessions/s-0001/terminal-token");
  assert.equal(fetchCalls[1].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.match(content.innerHTML, /class="detail terminal-detail"/);
  assert.match(content.innerHTML, /class="terminal-frame"/);
  assert.match(content.innerHTML, /src="\/v2\/sessions\/s-0001\/terminal-login\?token=abc123"/);
  assert.match(content.innerHTML, /data-terminal-popout="\/v2\/sessions\/s-0001\/terminal-login\?token=abc123"/);
  assert.match(content.innerHTML, /Drag to select \(auto-copies\) · Shift\+drag for manual selection/);
});

test("the change route refuses a diff that names a newer head than the metadata", async () => {
  const context = await scriptContext({
    location: { pathname: "/ui/changes/ch-0001" },
    setTimeout() {},
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ projects: [] }) });
      }
      if (path === "/ui/api/v2/changes/ch-0001") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change: { id: "ch-0001", head_sha: "111111111111" },
            task: { id: "t-0001" },
            threads: [],
            review_state: "in_review",
          }),
        });
      }
      if (path === "/ui/api/v2/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "222222222222",
            total_files: 1,
            files: [{ path: "b.go" }],
          }),
        });
      }
      throw new Error(`unexpected fetch ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = new InlineDOMElement("div");
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(
    status.textContent,
    "The change advanced while it was loading",
    "a head that moved between the two GETs fails the load with a retryable error",
  );
  assert.equal(content.children.length, 0, "no change panel is mounted for an unverified pair");
});

test("the change route mounts the change once the diff names the metadata head", async () => {
  const context = await scriptContext({
    location: { pathname: "/ui/changes/ch-0001" },
    setTimeout() {},
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ projects: [] }) });
      }
      if (path === "/ui/api/v2/changes/ch-0001") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change: { id: "ch-0001", head_sha: "111111111111" },
            task: { id: "t-0001" },
            threads: [],
            review_state: "in_review",
          }),
        });
      }
      if (path === "/ui/api/v2/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "111111111111",
            total_files: 1,
            files: [{ path: "a.go" }],
          }),
        });
      }
      throw new Error(`unexpected fetch ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = new InlineDOMElement("div");
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(title.textContent, "Change");
  assert.equal(status.textContent, "");
  assert.equal(content.children.length, 1, "a verified pair mounts the change panel");
  assert.equal(content.children[0].data.change.head_sha, "111111111111");
  assert.equal(content.children[0].data.diff.head_sha, "111111111111", "the mounted diff is the one the metadata head verifies");
});

test("console page offers shell harness and posts selected harness", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = mountableContent();
  const { renderConsoleRoute, FlowConsole } = await consoleImports();
  const context = await scriptContext({
    location: { pathname: "/ui/console", search: "" },
  }, {
    document: consoleDocument(FlowConsole),
    URLSearchParams,
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/harnesses") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            agents: [{ name: "harness", display_name: "Harness" }],
            consoles: [
              { name: "harness", display_name: "Harness" },
              { name: "shell", display_name: "Shell" },
            ],
          }),
        });
      }
      if (path === "/ui/api/v2/projects/p-alpha/console" && options.method === "POST") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ active: true }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          active: false,
          project_id: "p-alpha",
          project_name: "Alpha",
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "Alpha" }];
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { value: "shell", textContent: "" };
  };
  app.querySelectorAll = () => [];

  await renderConsoleRoute(app);
  const mounted = content.firstElementChild;
  mounted.paint();
  assert.match(mounted.innerHTML, /<option value="harness" selected>Harness<\/option>/);
  assert.match(mounted.innerHTML, /<option value="shell">Shell<\/option>/);

  await startConsoleView(app, "p-alpha", "shell");
  const post = fetchCalls.find((call) => call.path === "/ui/api/v2/projects/p-alpha/console" && call.options.method === "POST");
  assert.equal(post.options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(JSON.parse(post.options.body).harness, "shell");
  assert.equal(loads, 1);
  // The status line is the click dispatcher's now: ACTIONS.startConsole owns
  // the pending and confirmation messages (covered by the tests below).
});

test("terminal pop out opens a popup-style window", async () => {
  const opened = [];
  const context = await scriptContext({
    innerWidth: 1600,
    innerHeight: 1000,
    screen: { availWidth: 1600, availHeight: 1000 },
    open(url, target, features) {
      opened.push({ url, target, features });
      return {};
    },
  });

  context.openTerminalWindow("/v2/sessions/s-0001/terminal-login?token=abc123");

  assert.deepEqual(opened, [{
    url: "/v2/sessions/s-0001/terminal-login?token=abc123",
    target: "_blank",
    features: "popup=yes,noopener,noreferrer,width=1400,height=880,left=100,top=60,resizable=yes,scrollbars=yes",
  }]);
});

test("terminal route is recognized without polling", async () => {
  const context = await scriptContext();

  assert.equal(context.terminalSessionIDForPath("/ui/sessions/s-0001/terminal"), "s-0001");
  assert.equal(context.terminalSessionIDForPath("/ui/sessions/bad%ZZ/terminal"), "");
  assert.equal(context.pollConfigForPath("/ui/sessions/s-0001/terminal"), null);
});

async function tasksRouteImports() {
  const { renderTasksRoute } = await import("./tasks-route.js");
  const { FlowTasks } = await import("./elements/tasks.js");
  return { renderTasksRoute, FlowTasks };
}

function tasksRouteContent() {
  return {
    innerHTML: "",
    dataset: {},
    firstElementChild: null,
    appendChild(child) {
      this.firstElementChild = child;
    },
  };
}

function tasksRouteDocument(FlowTasks) {
  return {
    cookie: "flow_ui_csrf=csrf-token",
    addEventListener() {},
    createElement(tag) {
      const element = new FlowTasks();
      element.tagName = String(tag).toUpperCase();
      // The element is never connected here, so there is no <flow-app> to
      // delegate to: app-facing services resolve to empty.
      element.closest = () => null;
      return element;
    },
  };
}

test("tasks route ?state=done deep link pre-filters over the stored selection", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = tasksRouteContent();
  const { renderTasksRoute, FlowTasks } = await tasksRouteImports();
  const context = await scriptContext({
    location: { pathname: "/ui/tasks", search: "?state=done" },
    localStorage: {
      getItem(key) {
        if (key === "flow.ui.tasksState") return JSON.stringify(["scheduled"]);
        return null;
      },
      setItem() {},
      removeItem() {},
    },
  }, {
    document: tasksRouteDocument(FlowTasks),
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ tasks: [] }) });
    },
  });
  const app = new context.FlowApp();
  app.projects = [];
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return null;
  };
  app.querySelectorAll = () => [];

  const ok = await renderTasksRoute(app);

  assert.equal(ok, true);
  assert.deepEqual([...content.firstElementChild.tasksState], ["done"], "deep link wins over the stored scheduled filter");
  assert.deepEqual(fetchCalls, ["/ui/api/v2/tasks?state=done"]);
});

test("tasks route ignores an invalid ?state= deep link and keeps the stored filter", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = tasksRouteContent();
  const { renderTasksRoute, FlowTasks } = await tasksRouteImports();
  const context = await scriptContext({
    location: { pathname: "/ui/tasks", search: "?state=bogus&state=also-bogus" },
    localStorage: {
      getItem(key) {
        if (key === "flow.ui.tasksState") return JSON.stringify(["in_progress"]);
        return null;
      },
      setItem() {},
      removeItem() {},
    },
  }, {
    document: tasksRouteDocument(FlowTasks),
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ tasks: [] }) });
    },
  });
  const app = new context.FlowApp();
  app.projects = [];
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return null;
  };
  app.querySelectorAll = () => [];

  const ok = await renderTasksRoute(app);

  assert.equal(ok, true);
  assert.deepEqual([...content.firstElementChild.tasksState], ["in_progress"], "unknown state values fall back to the stored filter");
  assert.deepEqual(fetchCalls, ["/ui/api/v2/tasks?state=in_progress"]);
});

test("tasks ?state= deep link re-seeds after an in-app navigation from a prior visit", async () => {
  const fetchCalls = [];
  const content = tasksRouteContent();
  const { renderTasksRoute, FlowTasks } = await tasksRouteImports();
  const context = await scriptContext({
    location: { pathname: "/ui/tasks", search: "?state=done" },
    localStorage: {
      getItem(key) {
        if (key === "flow.ui.tasksState") return JSON.stringify(["scheduled"]);
        return null;
      },
      setItem() {},
      removeItem() {},
    },
  }, {
    document: tasksRouteDocument(FlowTasks),
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ tasks: [] }) });
    },
  });
  const app = new context.FlowApp();
  app.projects = [];
  app.querySelector = (selector) => {
    if (selector === "h1") return { textContent: "" };
    if (selector === ".status") return { textContent: "" };
    if (selector === ".content") return content;
    if (selector === ".statusbar") return null;
    return null;
  };
  app.querySelectorAll = () => [];

  // First visit: the retained scheduled filter applies (no state params).
  context.window.location.search = "";
  const first = await renderTasksRoute(app);
  assert.equal(first, true);
  const element = content.firstElementChild;
  assert.deepEqual([...element.tasksState], ["scheduled"], "no deep link keeps the persisted filter");

  // The throughput-strip data-link navigation reuses the same FlowApp and
  // reloads the route with ?state=done: mount() reuses the element and the
  // deep link must replace the retained filter instead of being ignored.
  context.window.location.search = "?state=done";
  const second = await renderTasksRoute(app);
  assert.equal(second, true);
  assert.equal(content.firstElementChild, element, "the same-route reload reuses the element");
  assert.deepEqual([...element.tasksState], ["done"], "in-app navigation to the deep link re-seeds the filter");
  assert.deepEqual(fetchCalls, ["/ui/api/v2/tasks?state=scheduled", "/ui/api/v2/tasks?state=done"]);
});

// Legacy Harness budget/toggle reasoning flags are no longer valid with
// harness v0.0.19. The form treats them as managed stale selection args so a
// later save does not keep emitting them.
test("new task action navigates to blank task form without posting", async () => {
  const harness = await createTaskHarness();

  await harness.create();

  assert.equal(harness.fetchCalls.length, 0);
  assert.equal(harness.pushedPath(), "/ui/tasks/new");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.status.textContent, "");
});

test("new task route renders project-scoped blank form with the selected project's flows", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/tasks/new" },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            projects: [
              { id: "p-alpha", name: "alpha" },
              { id: "p-beta", name: "beta" },
            ],
          }),
        });
      }
      if (path === "/ui/api/v2/projects/p-alpha/flows") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            flows: [
              { id: "fl-coding", name: "coding" },
              { id: "fl-planning", name: "planning" },
            ],
            default_flow_id: "fl-coding",
          }),
        });
      }
      if (path === "/ui/api/v2/projects/p-alpha/features?status=all") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ features: [] }),
        });
      }
      if (path === "/ui/api/v2/projects/p-alpha/work-items") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            items: [
              { id: "t-alpha-0001", kind: "task", title: "First task" },
              { id: "e-alpha-0001", kind: "epic", title: "First epic" },
              { id: "f-alpha-0001", kind: "feature", title: "First feature" },
            ],
          }),
        });
      }
      throw new Error(`new task route unexpectedly fetched ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderProjectPicker = () => {};
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.deepEqual(fetchCalls.map((call) => call.path), [
    "/ui/api/v2/projects",
    "/ui/api/v2/projects/p-alpha/flows",
    "/ui/api/v2/projects/p-alpha/work-items",
  ]);
  assert.equal(title.textContent, "New Task");
  assert.match(content.innerHTML, /data-task-form-mode="create"/);
  assert.match(content.innerHTML, /<span>Project<\/span>/);
  assert.match(content.innerHTML, /<option value="p-alpha" selected>alpha<\/option>/);
  assert.match(content.innerHTML, /<option value="p-beta" >beta<\/option>/);
  assert.match(content.innerHTML, /<input name="title" value="" required>/);
  assert.match(content.innerHTML, /<textarea name="body" rows="8"><\/textarea>/);
  assert.equal(content.innerHTML.match(/<textarea\b/g)?.length, 1);
  assert.match(content.innerHTML, /<span>Flow<\/span>/);
  assert.match(content.innerHTML, /<select name="flow_id" data-flow-select>/);
  assert.match(content.innerHTML, /<option value="fl-coding" selected>coding<\/option>/);
  assert.match(content.innerHTML, /<option value="fl-planning" >planning<\/option>/);
  assert.doesNotMatch(content.innerHTML, /\(default\)|Project default/);
  // The relation picker is populated from all project work-item summaries.
  assert.match(content.innerHTML, /<datalist id="relation-target-work-items">/);
  assert.match(content.innerHTML, /<option value="t-alpha-0001" label="task · First task"><\/option>/);
  assert.match(content.innerHTML, /<option value="e-alpha-0001" label="epic · First epic"><\/option>/);
  assert.match(content.innerHTML, /<option value="f-alpha-0001" label="feature · First feature"><\/option>/);
  // Human review is opt-in and appears beside the existing queue control.
  assert.match(content.innerHTML, /<div class="form-actions task-form-actions">\s*<label class="check">\s*<input name="requires_human_review" type="checkbox">\s*<span>Require human review<\/span>\s*<\/label>\s*<label class="check">\s*<input name="queue_task" type="checkbox" checked>\s*<span>Queue after creation<\/span>\s*<\/label>\s*<button class="button" type="submit">Create<\/button>/);
  assert.equal(status.textContent, "");
});

test("new task route keeps the relation picker in manual-entry mode when work-item suggestions fail to load", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/tasks/new" },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [{ id: "p-alpha", name: "alpha" }] }),
        });
      }
      if (path === "/ui/api/v2/projects/p-alpha/flows") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ flows: [], default_flow_id: "" }) });
      }
      if (path === "/ui/api/v2/projects/p-alpha/work-items") {
        return Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({ error: { message: "boom" } }) });
      }
      throw new Error(`new task route unexpectedly fetched ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderProjectPicker = () => {};
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  // The failed work-item fetch is swallowed: the route still renders and the
  // picker simply has no suggestions, leaving the target input free-text.
  assert.ok(fetchCalls.some((call) => call.path === "/ui/api/v2/projects/p-alpha/work-items"));
  assert.equal(title.textContent, "New Task");
  assert.match(content.innerHTML, /<datalist id="relation-target-work-items"><\/datalist>/);
  assert.match(content.innerHTML, /data-relation-target/);
  assert.equal(status.textContent, "");
});

test("new task form shows the project field even with one project", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];

  const html = app.renderTaskForm({ title: "", priority: 0 }, { mode: "create", submitLabel: "Create" });

  assert.match(html, /<span>Project<\/span>/);
  assert.match(html, /<select name="project" required>/);
  assert.match(html, /<option value="p-alpha" selected>alpha<\/option>/);
  assert.ok(html.indexOf('class="task-field-project"') < html.indexOf('class="task-field-priority"'));
  assert.ok(html.indexOf('class="task-field-priority"') < html.indexOf('class="task-field-flow"'));
  assert.ok(html.indexOf('class="task-field-flow"') < html.indexOf('class="task-field-title wide"'));
});

test("new task form submission posts to the selected project collection", async () => {
  const fetchCalls = [];
  let pushedPath = "";
  let loads = 0;
  await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPath = path;
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }),
      });
    },
  });
  const form = {
    tagName: "FORM",
    dataset: {
      project: "p-alpha",
      taskForm: "",
      taskFormMode: "create",
    },
    elements: {
      project: { value: "p-alpha" },
      priority: { value: "2" },
      flow_id: { value: "fl-coding" },
      title: { value: "First task" },
      body: { value: "Task details" },
      requires_human_review: { checked: true },
      attachments: { files: [] },
      queue_task: { checked: false },
    },
    reportValidity() {
      return true;
    },
  };
  const app = {
    setStatus() {},
    async load() {
      loads += 1;
    },
    async refresh() {
      throw new Error("create submission should not refresh the edit route");
    },
  };

  const handled = await handleFormSubmit(app, {
    target: form,
    preventDefault() {},
  });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    title: "First task",
    body: "Task details",
    priority: 2,
    flow_id: "fl-coding",
    requires_human_review: true,
  });
  assert.equal(pushedPath, "/ui/tasks/t-alpha-0001");
  assert.equal(loads, 1);
});
test("task form renders the relation picker only in create mode", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];

  const createHTML = app.renderTaskForm({ title: "", priority: 0 }, { mode: "create", submitLabel: "Create" });
  assert.match(createHTML, /<input name="requires_human_review" type="checkbox">/);
  assert.match(createHTML, /data-relation-picker/);
  assert.match(createHTML, /data-relation-rows/);
  assert.match(createHTML, /data-relation-add/);
  assert.match(createHTML, /<option value="parent_of" >child of<\/option>/);
  assert.match(createHTML, /name="parent_item_id"/);
  assert.match(createHTML, /<option value="blocks" >blocks<\/option>/);
  assert.match(createHTML, /<option value="related_to" selected>related to<\/option>/);
  // Containment keeps its canonical picker; an explicit child-of relation row
  // is also available and duplicate declarations are rejected on submit.
  assert.match(createHTML, /data-relation-kind[^>]*>[\s\S]*?<option value="related_to" selected>/);

  const editHTML = app.renderTaskForm({ title: "T", requires_human_review: true }, { taskID: "t-alpha-0001", projectID: "p-alpha" });
  assert.match(editHTML, /<input name="requires_human_review" type="checkbox" checked>/);
  assert.doesNotMatch(editHTML, /data-relation-picker/);
  assert.doesNotMatch(editHTML, /data-relation-add/);
  assert.doesNotMatch(editHTML, /data-relation-row/);
  // Edit mode retains the review policy but has no queue checkbox.
  assert.doesNotMatch(editHTML, /queue_task/);
  assert.match(editHTML, /<div class="form-actions task-form-actions">\s*<label class="check">\s*<input name="requires_human_review" type="checkbox" checked>\s*<span>Require human review<\/span>\s*<\/label>\s*<button class="button" type="submit">Save<\/button>/);
});

function relationRow(kind, target) {
  return {
    querySelector(selector) {
      if (selector === "[data-relation-target]") return { value: target };
      if (selector === "[data-relation-kind]") return { value: kind };
      return null;
    },
  };
}

function createFormWithRelations(rows) {
  return {
    tagName: "FORM",
    dataset: { project: "p-alpha", taskForm: "", taskFormMode: "create" },
    elements: {
      project: { value: "p-alpha" },
      priority: { value: "0" },
      flow_id: { value: "" },
      title: { value: "Related task" },
      body: { value: "" },
      attachments: { files: [] },
      queue_task: { checked: false },
    },
    querySelectorAll(selector) {
      return selector === "[data-relation-row]" ? rows : [];
    },
    reportValidity() {
      return true;
    },
  };
}

test("new task form submission includes source-outward relation rows in the payload", async () => {
  const fetchCalls = [];
  await scriptContext({}, {
    history: { pushState() {} },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) });
    },
  });
  const form = createFormWithRelations([
    relationRow("blocks", "t-alpha-0002"),
    relationRow("related_to", "t-alpha-0003"),
  ]);
  const app = { setStatus() {}, async load() {}, async refresh() {} };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 1, "source-outward rows only need the create call");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).work_item_relations, [
    { target_item_id: "t-alpha-0002", source_is_new_item: true, kind: "blocks" },
    { target_item_id: "t-alpha-0003", source_is_new_item: true, kind: "related_to" },
  ]);
});

test("new task form sends a child-of row atomically in the create request", async () => {
  const fetchCalls = [];
  let pushedPath = "";
  let loads = 0;
  await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPath = path;
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, status: 201, json: () => Promise.resolve({ task: { id: "t-alpha-0002" } }) });
    },
  });
  const form = createFormWithRelations([
    relationRow("parent_of", "t-alpha-0001"),
    relationRow("blocks", "t-alpha-0003"),
  ]);
  const app = {
    setStatus() {},
    async load() {
      loads += 1;
    },
    async refresh() {
      throw new Error("create submission should not refresh the edit route");
    },
  };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  // Both rows ride in the single create request. The child-of row makes the new
  // task the relation target (chosen parent parent_of new task) via
  // target_is_new_item, so the server applies it in the create transaction. There
  // is no second, post-create link call that could partially succeed after the
  // task has already been committed.
  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).work_item_relations, [
    { source_item_id: "t-alpha-0001", target_is_new_item: true, kind: "parent_of" },
    { target_item_id: "t-alpha-0003", source_is_new_item: true, kind: "blocks" },
  ]);
  assert.equal(pushedPath, "/ui/tasks/t-alpha-0002");
  assert.equal(loads, 1);
});

test("a failed atomic child-of create leaves the form in place for a retry", async () => {
  const fetchCalls = [];
  let pushedPath = "";
  let loads = 0;
  const statuses = [];
  await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPath = path;
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      // The server maps a missing/inaccessible parent to this message instead
      // of surfacing the raw SQLite foreign-key text; the form must show the
      // translated message on the status line.
      return Promise.resolve({
        ok: false,
        status: 400,
        json: () => Promise.resolve({ error: { message: "the selected parent cannot be used: task t-alpha-9999 does not exist or is not accessible" } }),
        text: () => Promise.resolve(""),
      });
    },
  });
  const form = createFormWithRelations([relationRow("parent_of", "t-alpha-0001")]);
  const app = {
    setStatus(message) {
      statuses.push(message);
    },
    async load() {
      loads += 1;
    },
    async refresh() {
      throw new Error("failed create should not refresh");
    },
  };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  // The failed create is the only request: the server rolled the whole create
  // back, so nothing was committed and there is no orphaned task to recover.
  // The form stays put (no navigation, no reload) and the failure is surfaced
  // on the status line, so the user can resubmit. The failure-then-success
  // retry itself is covered by the next test.
  assert.equal(fetchCalls.length, 1);
  assert.equal(pushedPath, "");
  assert.equal(loads, 0);
  assert.match(statuses.at(-1), /the selected parent cannot be used/);
  assert.doesNotMatch(statuses.at(-1), /FOREIGN KEY/);
});


test("retrying a failed atomic child-of create recovers with one task, one relation, and one navigation", async () => {
  const fetchCalls = [];
  const pushedPaths = [];
  let loads = 0;
  const statuses = [];
  let submissions = 0;
  await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPaths.push(path);
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      // First submission fails (e.g. the chosen parent cannot be linked); the
      // server rolls the whole create back. The retry succeeds.
      submissions += 1;
      if (submissions === 1) {
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: "task relation would create a cycle" } }),
          text: () => Promise.resolve(""),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ task: { id: "t-alpha-0002" } }),
      });
    },
  });
  const form = createFormWithRelations([relationRow("parent_of", "t-alpha-0001")]);
  const app = {
    setStatus(message) {
      statuses.push(message);
    },
    async load() {
      loads += 1;
    },
    async refresh() {
      throw new Error("create submission should not refresh the edit route");
    },
  };

  // First submission fails; the form stays in place (no navigation, no reload).
  await handleFormSubmit(app, { target: form, preventDefault() {} });
  assert.equal(pushedPaths.length, 0);
  assert.equal(loads, 0);
  assert.match(statuses.at(-1), /task relation would create a cycle/);

  // The user resubmits the same form. Because the failed create committed
  // nothing, the retry creates exactly one task — not a duplicate — carrying
  // the child-of relation, and navigates to it once.
  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 2);
  for (const call of fetchCalls) {
    assert.equal(call.path, "/ui/api/v2/projects/p-alpha/tasks");
    assert.deepEqual(JSON.parse(call.options.body).work_item_relations, [
      { source_item_id: "t-alpha-0001", target_is_new_item: true, kind: "parent_of" },
    ]);
  }
  // One successful create → one navigation and one load, to the single created
  // task with its relation applied.
  assert.deepEqual(pushedPaths, ["/ui/tasks/t-alpha-0002"]);
  assert.equal(loads, 1);
  assert.equal(statuses.at(-1), "Task created");
});

test("new task form drops relation rows with an empty target and omits the key when none remain", async () => {
  const fetchCalls = [];
  await scriptContext({}, {
    history: { pushState() {} },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, status: 201, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) });
    },
  });
  const app = { setStatus() {}, async load() {}, async refresh() {} };

  // A blank source-outward row is dropped, so only the filled row is sent.
  const mixed = createFormWithRelations([
    relationRow("blocks", "   "),
    relationRow("related_to", "t-alpha-0002"),
  ]);
  await handleFormSubmit(app, { target: mixed, preventDefault() {} });
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).work_item_relations, [
    { target_item_id: "t-alpha-0002", source_is_new_item: true, kind: "related_to" },
  ]);

  const allBlank = createFormWithRelations([relationRow("blocks", "")]);
  await handleFormSubmit(app, { target: allBlank, preventDefault() {} });
  assert.equal("work_item_relations" in JSON.parse(fetchCalls[1].options.body), false);
});

test("new task form rejects duplicate relation rows before submitting", async () => {
  const fetchCalls = [];
  let status = "";
  await scriptContext({}, {
    history: { pushState() {} },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) });
    },
  });
  const form = createFormWithRelations([
    relationRow("blocks", "t-alpha-0002"),
    relationRow("blocks", "t-alpha-0002"),
  ]);
  const app = { setStatus(message) { status = message; }, async load() {}, async refresh() {} };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 0);
  assert.match(status, /Duplicate relation/);
});

test("new task form rejects more than one child-of row before any request", async () => {
  const fetchCalls = [];
  let status = "";
  await scriptContext({}, {
    history: { pushState() {} },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) });
    },
  });
  // Two distinct child-of targets are not duplicates, but a task can have only
  // one parent, so the submission must be rejected before any request goes out.
  const form = createFormWithRelations([
    relationRow("parent_of", "t-alpha-0008"),
    relationRow("parent_of", "t-alpha-0009"),
    relationRow("blocks", "t-alpha-0002"),
  ]);
  const app = { setStatus(message) { status = message; }, async load() {}, async refresh() {} };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 0, "no create POST for an invalid child-of set");
  assert.match(status, /one parent/);
});

test("new task form accepts multiple default-kind rows with distinct targets", async () => {
  const fetchCalls = [];
  await scriptContext({}, {
    history: { pushState() {} },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({ ok: true, status: 201, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) });
    },
  });
  // The picker's default kind is source-outward (related_to), so several rows
  // left on their default with distinct targets are plain create-payload
  // relations — they must not trip the one-parent child-of validation.
  const form = createFormWithRelations([
    relationRow("related_to", "t-alpha-0002"),
    relationRow("related_to", "t-alpha-0003"),
  ]);
  const app = { setStatus() {}, async load() {}, async refresh() {} };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.equal(fetchCalls.length, 1);
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).work_item_relations, [
    { target_item_id: "t-alpha-0002", source_is_new_item: true, kind: "related_to" },
    { target_item_id: "t-alpha-0003", source_is_new_item: true, kind: "related_to" },
  ]);
});
// A button with just enough surface for handleAction's synchronous pending
// state: a dataset, a disabled flag, and attribute/class tracking.
// A gate outcome button: an ActionButton that also knows its enclosing gate
// panel and its sibling outcome controls, so handleAction can suppress the
// whole set when one of them is clicked.
test("workflow skip eligibility excludes author and side-effecting steps", () => {
  assert.equal(workflowStepCanBeSkipped("automated_checks"), true);
  assert.equal(workflowStepCanBeSkipped("change_review"), true);
  assert.equal(workflowStepCanBeSkipped("verify_change"), true);
  assert.equal(workflowStepCanBeSkipped("agent"), false);
  assert.equal(workflowStepCanBeSkipped("materialize_task_set"), false);
  assert.equal(workflowStepCanBeSkipped("merge"), false);
  assert.equal(workflowStepCanBeSkipped("human_gate"), false);
  assert.equal(workflowStepCanBeSkipped("terminal"), false);
});

test("readDiffMode round-trips split and falls back to unified for invalid values", async () => {
  const storage = new Map();
  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = await scriptContext({ localStorage });

  assert.equal(context.readDiffMode(), "unified");
  context.writeDiffMode("split");
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.equal(context.readDiffMode(), "split");

  context.writeDiffMode("bogus");
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.equal(context.readDiffMode(), "split");

  storage.set(DIFF_MODE_STORAGE_KEY, "not-a-mode");
  assert.equal(context.readDiffMode(), "unified");
});

test("human review check renders approval action only while unsatisfied", async () => {
  const context = await scriptContext();
  const pendingHTML = context.renderCheck({
    task_id: "t-alpha-0001",
    name: "human-review",
    kind: "human",
    required: true,
    verdict: "pending",
  });
  const satisfiedHTML = context.renderCheck({
    task_id: "t-alpha-0001",
    name: "human-review",
    kind: "human",
    required: true,
    verdict: "satisfied",
  });
  const ciHTML = context.renderCheck({
    task_id: "t-alpha-0001",
    name: "unit",
    kind: "ci",
    required: true,
    verdict: "pending",
  });

  assert.match(pendingHTML, /data-human-review-approve="t-alpha-0001"/);
  assert.match(pendingHTML, /data-check-name="human-review"/);
  assert.match(pendingHTML, />Approve<\/button>/);
  assert.doesNotMatch(satisfiedHTML, /data-human-review-approve/);
  assert.doesNotMatch(ciHTML, /data-human-review-approve/);
});

test("attachment previews are limited to safe raster image types", async () => {
  const context = await scriptContext();

  assert.equal(context.isImageContentType("image/png"), true);
  assert.equal(context.isImageContentType("image/jpeg; charset=binary"), true);
  assert.equal(context.isImageContentType("text/html"), false);
  assert.equal(context.isImageContentType("image/svg+xml"), false);
});

test("taskHref builds a globally resolvable task detail link", async () => {
  const context = await scriptContext();

  assert.equal(context.taskHref("p-alpha", "t-alpha-0001"), "/ui/tasks/t-alpha-0001");
  assert.equal(context.taskHref("", "t-alpha-0001"), "/ui/tasks/t-alpha-0001");
});

test("generic workflow chart counts exact outcome edges and highlights the active node", async () => {
  const context = await scriptContext();
  const graph = {
    start_node: "implement",
    nodes: [
      { key: "implement", name: "Implement <safe>", kind: "agent" },
      { key: "review", name: "Review", kind: "change_review" },
      { key: "done", name: "Done", kind: "terminal" },
    ],
    edges: [
      { from: "implement", outcome: "completed", to: "review" },
      { from: "review", outcome: "changes_requested", to: "implement" },
      { from: "review", outcome: "approved", to: "done" },
    ],
  };
  const transitions = [
    { from_node_key: "implement", outcome: "completed", to_node_key: "review", event_kind: "node_completed" },
    { from_node_key: "review", outcome: "changes_requested", to_node_key: "implement", event_kind: "node_completed" },
    { from_node_key: "implement", outcome: "completed", to_node_key: "review", event_kind: "node_completed" },
    { from_node_key: "review", outcome: "approved", to_node_key: "done", event_kind: "node_completed" },
    // The lifecycle completion row traverses the same terminal edge but has no
    // outcome and must not inflate the edge count.
    { from_node_key: "review", to_node_key: "done", event_kind: "workflow_completed" },
  ];
  const counts = context.workflowTransitionCounts(transitions);
  assert.equal(counts.get(context.workflowEdgeKey("implement", "completed", "review")), 2);
  assert.equal(counts.get(context.workflowEdgeKey("review", "changes_requested", "implement")), 1);
  assert.equal(counts.get(context.workflowEdgeKey("review", "approved", "done")), 1);

  const html = context.renderWorkflowGraph(graph, { activeNode: "review", transitionCounts: counts, ariaLabel: "Task workflow" });
  assert.match(html, /<svg[^>]*aria-label="Task workflow"/);
  assert.match(html, /class="workflow-node is-current" data-node="review"/);
  assert.match(html, /class="workflow-current-halo"/);
  assert.match(html, /completed ×2/);
  assert.match(html, /changes_requested ×1/);
  assert.match(html, /approved ×1/);
  assert.match(html, /class="workflow-edge is-taken"/);
  assert.match(html, /Implement &lt;safe&gt;/);
  assert.doesNotMatch(html, /Implement <safe>/);
});

test("generic workflow definition chart renders edge outcomes without run counts", async () => {
  const context = await scriptContext();
  const html = context.renderWorkflowGraph({
    start_node: "plan",
    nodes: [{ key: "plan", name: "Plan", kind: "agent" }, { key: "done", name: "Done", kind: "terminal" }],
    edges: [{ from: "plan", outcome: "completed", to: "done" }],
  });
  assert.match(html, /class="workflow-node is-start" data-node="plan"/);
  assert.match(html, /data-edge-outcome="completed"/);
  assert.match(html, />completed<\/text>/);
  assert.doesNotMatch(html, /×0/);
});

test("diagnostics rows render queue, lease, tmux, session, and taints", async () => {
  const context = await scriptContext();
  const queueHTML = context.renderQueueSummary({
    queued: 2,
    persistent_agent: 1,
    ephemeral: 1,
    author: 1,
    ci: 1,
  });
  assert.match(queueHTML, /queued 2/);
  assert.match(queueHTML, /persistent 1/);

  const workerHTML = context.renderWorkerRow({
    id: "w-local",
    status: "registered",
    capacity_persistent_agent: 2,
    capacity_ephemeral: 1,
    labels: { "agent.harness.harness": "true" },
    taints: [{ key: "gpu", value: "false", effect: "NoSchedule" }],
    last_seen_at: "2026-06-07T12:00:00Z",
  }, {
    live_jobs: 1,
    live_persistent_agent: 1,
    live_ephemeral: 0,
    expired_unreleased_jobs: 1,
    expired_unreleased_persistent_agent: 1,
  });
  assert.match(workerHTML, /1 jobs/);
  assert.match(workerHTML, /expired 1/);
  assert.match(workerHTML, /held 1\/0/);
  assert.match(workerHTML, /agent\.harness\.harness=true/);
  assert.match(workerHTML, /gpu=false:NoSchedule/);

  const jobHTML = context.renderJobRow({
    id: "j-0001",
    state: "running",
    role: "ci",
    capacity_bucket: "ephemeral",
    task_id: "t-alpha-0001",
    change_id: "ch-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    project_id: "p-alpha",
    project_name: "alpha",
    lease: { id: "l-0001", worker_id: "w-local" },
    live_lease: true,
    lease_status: "live",
    tmux_session: "flow-j-0001",
    session: { id: "s-0001", state: "working", terminal_available: true, transcript_available: true },
    change: { id: "ch-0001" },
  });
  assert.match(jobHTML, /alpha/);
  assert.match(jobHTML, /class="row-run"/);
  assert.match(jobHTML, /l-0001/);
  assert.match(jobHTML, /live/);
  assert.match(jobHTML, /flow-j-0001/);
  assert.match(jobHTML, /working/);
  assert.match(jobHTML, /data-terminal="s-0001"/);
  assert.doesNotMatch(jobHTML, /data-job-attach|>Attach<\/button>/);
  assert.match(jobHTML, /data-session-transcript="s-0001"/);
  assert.match(jobHTML, /\/ui\/tasks\/t-alpha-0001/);
  assert.match(jobHTML, /\/ui\/changes\/ch-0001/);

  const jobTranscriptHTML = context.renderJobRow({
    id: "j-0004",
    state: "finished",
    role: "reviewer",
    capacity_bucket: "ephemeral",
    task_id: "t-alpha-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0004", worker_id: "w-local" },
    transcript_available: true,
  });
  assert.match(jobTranscriptHTML, /data-job-transcript="j-0004"/);

  const reviewerJobHTML = context.renderJobRow({
    id: "j-0003",
    state: "running",
    role: "reviewer",
    capacity_bucket: "persistent_agent",
    task_id: "t-alpha-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0003", worker_id: "w-local" },
    live_lease: true,
    lease_status: "live",
    tmux_session: "flow-j-0003",
    terminal_available: true,
  });
  assert.match(reviewerJobHTML, /data-job-terminal="j-0003"/);
  assert.doesNotMatch(reviewerJobHTML, /data-job-attach|>Attach<\/button>/);

  const expiredJobHTML = context.renderJobRow({
    id: "j-0002",
    state: "claimed",
    role: "ci",
    capacity_bucket: "persistent_agent",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0002", worker_id: "w-local" },
    live_lease: false,
    lease_status: "expired",
  });
  assert.match(expiredJobHTML, /l-0002/);
  assert.match(expiredJobHTML, /expired/);
});

test("workflow activity labels describe common active step names", async () => {
  const context = await scriptContext();

  assert.equal(context.workflowActivityLabel("Implement", "agent"), "Implementing");
  assert.equal(context.workflowActivityLabel("Plan", "agent"), "Planning");
  assert.equal(context.workflowActivityLabel("Write task plan", "agent"), "Writing task plan");
  assert.equal(context.workflowActivityLabel("Automated checks", "automated_checks"), "Running automated checks");
  assert.equal(context.workflowActivityLabel("Code and security review", "change_review"), "Reviewing code and security");
  assert.equal(context.workflowActivityLabel("Requirements verification", "verify_change"), "Verifying requirements");
  assert.equal(context.workflowActivityLabel("Change merge", "merge_change"), "Merging change");
  assert.equal(context.workflowActivityLabel("Sync dependencies", "agent"), "Syncing dependencies");
  assert.equal(context.workflowActivityLabel("Implementation", "agent"), "Working on implementation");
  assert.equal(context.workflowActivityLabel("Security", "agent"), "Working on security");
  assert.equal(context.workflowActivityLabel("Security review", ""), "Working on security review");
});

test("statusbar reflects poll state and interval", async () => {
  const timers = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  });
  const app = new context.FlowApp();
  const label = { textContent: "" };
  const meta = { textContent: "" };
  const bar = {
    dataset: {},
    querySelector: (selector) => (selector === ".sb-label" ? label : null),
  };
  app.querySelector = (selector) => {
    if (selector === ".statusbar") return bar;
    if (selector === ".sb-meta") return meta;
    return null;
  };

  app.setPollState("live", "live");
  assert.equal(bar.dataset.state, "live");
  assert.equal(label.textContent, "live");

  app.setPollState("error", "retry 3");
  assert.equal(bar.dataset.state, "error");
  assert.equal(label.textContent, "retry 3");

  app.pollFailures = 0;
  app.schedulePolling("/ui/jobs");
  assert.equal(meta.textContent, "poll 30s");

  app.schedulePolling("/ui/projects/p-alpha/tasks/t-alpha-0001");
  assert.equal(meta.textContent, "");
});

test("worker and job state badges map states to status classes", async () => {
  const context = await scriptContext();

  assert.equal(context.renderStateBadge("ready"), `<span class="badge ok">ready</span>`);
  assert.equal(context.renderStateBadge("succeeded"), `<span class="badge ok">succeeded</span>`);
  assert.equal(context.renderStateBadge("failed"), `<span class="badge danger">failed</span>`);
  assert.equal(context.renderStateBadge("expired"), `<span class="badge danger">expired</span>`);
  assert.equal(context.renderStateBadge("running"), `<span class="badge run">running</span>`);
  assert.equal(context.renderStateBadge("claimed"), `<span class="badge idle">claimed</span>`);
  assert.equal(context.renderStateBadge("finished"), `<span class="badge ok">finished</span>`);
  assert.equal(context.renderStateBadge("crashed"), `<span class="badge danger">crashed</span>`);
  assert.equal(context.renderStateBadge("canceled"), `<span class="badge warn">canceled</span>`);
  assert.equal(context.renderStateBadge(""), "");

  assert.equal(context.jobStateClass("finished"), "ok");
  assert.equal(context.jobStateClass("failed"), "danger");
  assert.equal(context.jobStateClass("crashed"), "danger");
  assert.equal(context.jobStateClass("canceled"), "warn");
  assert.equal(context.jobStateClass("running"), "run");
  assert.equal(context.jobStateClass("claimed"), "idle");
  assert.equal(context.jobStateClass("queued"), "idle");

  assert.match(
    context.renderJobRow({ id: "j-0001", state: "finished", role: "author" }),
    /class="row-ok"/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "failed", role: "author" }),
    /class="row-danger"/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "canceled", role: "author" }),
    /class="row-warn"/,
  );

  assert.match(
    context.renderWorkerRow({ id: "w-local", status: "registered" }),
    /<td><span class="badge idle">registered<\/span><\/td>/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "running", role: "author" }),
    /<td><span class="badge run">running<\/span><\/td>/,
  );
});


test("check verdict badges map verdicts to status classes with pending fallback", async () => {
  const context = await scriptContext();

  assert.equal(context.renderVerdictBadge("satisfied"), `<span class="badge ok">satisfied</span>`);
  assert.equal(context.renderVerdictBadge("blocked"), `<span class="badge danger">blocked</span>`);
  assert.equal(context.renderVerdictBadge("errored"), `<span class="badge danger">errored</span>`);
  assert.equal(context.renderVerdictBadge("failed"), `<span class="badge danger">failed</span>`);
  assert.equal(context.renderVerdictBadge("rejected"), `<span class="badge danger">rejected</span>`);
  assert.equal(context.renderVerdictBadge("needs_rerun"), `<span class="badge idle">needs rerun</span>`);
  assert.equal(context.renderVerdictBadge(""), `<span class="badge idle">pending</span>`);
});

test("non-polling routes report static instead of live", async () => {
  const harness = await browserSmokeHarness("/ui/tasks/new", {});

  await harness.app.load();

  assert.equal(harness.statusbar.dataset.state, "idle");
  assert.equal(harness.sbLabel.textContent, "static");
  assert.equal(harness.sbMeta.textContent, "");
  assert.deepEqual(harness.fetchCalls, ["/ui/api/v2/projects"]);
});

test("load failures surface error then retry state in the statusbar", async () => {
  const harness = await browserSmokeHarness("/ui/board", {});

  await harness.app.load();
  assert.match(harness.status.textContent, /missing smoke response/);
  assert.equal(harness.statusbar.dataset.state, "error");
  assert.equal(harness.sbLabel.textContent, "error");

  await harness.app.load({ fromPoll: true });
  assert.equal(harness.statusbar.dataset.state, "error");
  assert.equal(harness.sbLabel.textContent, "retry 2");

  harness.fetchCalls.length = 0;
});

test("polling policy matches board, diagnostics, and change routes", async () => {
  const context = await scriptContext();

  assert.deepEqual(normalize(context.pollConfigForPath("/ui/")), {
    interval: 10000,
    maxInterval: 10000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/board")), {
    interval: 10000,
    maxInterval: 10000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/changes/ch-0001")), {
    interval: 15000,
    maxInterval: 15000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/jobs")), {
    interval: 30000,
    maxInterval: 120000,
    backoff: true,
  });
  assert.equal(context.pollConfigForPath("/ui/projects/p-alpha/tasks/t-alpha-0001"), null);
});

test("diagnostics polling backs off and clears prior timer", async () => {
  const timers = [];
  const cleared = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout(id) {
      cleared.push(id);
    },
  });
  const app = new context.FlowApp();

  app.pollFailures = 0;
  app.schedulePolling("/ui/jobs");
  assert.equal(timers[0].delay, 30000);

  app.clearPolling();
  assert.deepEqual(cleared, [1]);
  assert.equal(app.mainPoll.timer, 0);

  app.pollFailures = 3;
  app.schedulePolling("/ui/jobs");
  assert.equal(timers[1].delay, 120000);

  app.pollFailures = 5;
  app.schedulePolling("/ui/workers");
  assert.equal(timers[2].delay, 120000);
  assert.deepEqual(cleared, [1, 2]);
});

test("pollDelay applies capped exponential backoff", async () => {
  const { pollDelay } = await scriptContext();
  assert.equal(pollDelay(30000, 0, 120000), 30000); // no failures -> base interval
  assert.equal(pollDelay(30000, 1, 120000), 60000); // one failure -> doubled
  assert.equal(pollDelay(30000, 2, 120000), 120000); // would be 120000, at the cap
  assert.equal(pollDelay(30000, 3, 120000), 120000); // capped, not 240000
  assert.equal(pollDelay(10000, 0, 120000), 10000); // backoff disabled -> base
});
test("load tracks in-flight invocations and never arms a settle burst itself", async () => {
  const timers = [];
  const jobsResponse = deferred();
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = {
    innerHTML: "",
    dataset: {},
    firstElementChild: null,
    appendChild(child) {
      this.firstElementChild = child;
    },
  };
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      createElement: (tag) => new SmokeElement(tag),
    },
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ projects: [] }) });
      }
      assert.equal(path, "/ui/api/v2/jobs");
      return jobsResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    return null;
  };

  assert.equal(app.loadsInFlight || 0, 0);
  const loadPromise = app.load();
  assert.equal(app.loadsInFlight, 1, "the load is tracked while it is in flight");

  jobsResponse.resolve({ ok: true, json: () => Promise.resolve({ jobs: [] }) });
  await loadPromise;

  assert.equal(app.loadsInFlight, 0, "the counter settles once the load completes");
  assert.equal(title.textContent, "Jobs");
  // A manual/navigation load re-arms only the regular poll; the settle burst
  // belongs to action-triggered refreshes alone.
  assert.equal(timers.length, 1);
  assert.equal(timers[0].delay, context.DIAGNOSTICS_POLL_MS);
});

test("a navigation load cancels an armed settle-burst timeout through the real load", async () => {
  const timers = [];
  const cleared = [];
  const jobsResponse = deferred();
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = {
    innerHTML: "",
    dataset: {},
    firstElementChild: null,
    appendChild(child) {
      this.firstElementChild = child;
    },
  };
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout(id) {
      cleared.push(id);
    },
  }, {
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      createElement: (tag) => new SmokeElement(tag),
    },
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ projects: [] }) });
      }
      assert.equal(path, "/ui/api/v2/jobs");
      return jobsResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    return null;
  };

  // Arm a settle burst the way a successful action's refresh does.
  app.loadGeneration = 1;
  app.settle.schedule({ generation: 1, path: "/ui/jobs" });
  assert.equal(timers.length, 1, "the burst arms its first tick");
  assert.equal(app.settle.poll.timer, 1, "the burst owns the pending timer");

  // A navigation load — the same load() the nav click, popstate, and shortcut
  // handlers call — must cancel that pending timeout, not leave it live until
  // it fires with only a stale-guard making the callback a no-op.
  const loadPromise = app.load();
  jobsResponse.resolve({ ok: true, json: () => Promise.resolve({ jobs: [] }) });
  await loadPromise;
  assert.deepEqual(cleared, [1], "the navigation load cancels the pending settle-burst timeout");
  assert.equal(app.settle.poll.timer, 0, "no settle timer is left armed after the navigation");

  // A callback already queued in the browser when the navigation landed stays
  // inert: it neither reloads nor re-arms.
  timers[0].callback();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(app.loadsInFlight, 0, "the cancelled tick reloads nothing");
  assert.equal(timers.length, 2, "only the route's regular poll timer remains");
});


test("stale poll load does not repaint task route or rearm board polling", async () => {
  const timers = [];
  const status = { textContent: "" };
  const title = { textContent: "" };
  const content = { innerHTML: "task edit form" };
  const boardResponse = deferred();
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    fetch(path) {
      assert.equal(path, "/ui/api/v2/board");
      return boardResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === ".status") return status;
    if (selector === "h1") return title;
    return { textContent: "" };
  };

  const loadPromise = app.load({ fromPoll: true });
  context.window.location.pathname = "/ui/projects/p-alpha/tasks/t-alpha-0001";
  boardResponse.resolve({
    ok: true,
    json: () => Promise.resolve({ board: { backlog: [{ id: "t-alpha-0002", title: "Board task" }] } }),
  });
  await loadPromise;

  assert.equal(content.innerHTML, "task edit form");
  assert.equal(title.textContent, "");
  assert.equal(timers.length, 0);
  assert.equal(status.textContent, "");
});

test("board route fetches the completion stats, not the removed /v2/done lane", async () => {
  const timers = [];
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = new InlineDOMElement("div");
  const context = await scriptContext({
    location: { pathname: "/ui/board" },
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      if (path === "/ui/api/v2/board") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ boards: [] }),
        });
      }
      if (path === "/ui/api/v2/stats/completions") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            buckets: [{ window: "15m", count: 3 }, { window: "24h", count: 40 }],
          }),
        });
      }
      throw new Error(`board route unexpectedly fetched ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  // The Done lane is gone, so the board route must not fire the second
  // /v2/done request that used to feed it; the completion stats feed the
  // throughput strip instead, and their payload reaches the board element.
  assert.deepEqual(fetchCalls.map((call) => call.path), [
    "/ui/api/v2/projects",
    "/ui/api/v2/board",
    "/ui/api/v2/stats/completions",
  ]);
  assert.deepEqual(content.children[0].data.stats, {
    buckets: [{ window: "15m", count: 3 }, { window: "24h", count: 40 }],
  });
  assert.equal(status.textContent, "0 tasks · nothing waiting on you");
  assert.equal(timers.length, 1);
});

test("board route tolerates a completion stats failure", async () => {
  const timers = [];
  const status = { textContent: "" };
  const content = new InlineDOMElement("div");
  const context = await scriptContext({
    location: { pathname: "/ui/board" },
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      if (path === "/ui/api/v2/board") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ boards: [] }),
        });
      }
      if (path === "/ui/api/v2/stats/completions") {
        return Promise.reject(new Error("stats down"));
      }
      throw new Error(`board route unexpectedly fetched ${path}`);
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelector = (selector) => {
    if (selector === "h1") return { textContent: "" };
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  // A stats failure degrades to null, like the old done fetch: the board
  // still mounts and the strip stays silent.
  assert.equal(content.children[0].data.stats, null);
  assert.equal(status.textContent, "0 tasks · nothing waiting on you");
  assert.equal(timers.length, 1);
});

test("disconnect during pending load prevents polling rearm", async () => {
  const timers = [];
  const jobsResponse = deferred();
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    fetch(path) {
      assert.equal(path, "/ui/api/v2/jobs");
      return jobsResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = () => ({ textContent: "", innerHTML: "" });

  const loadPromise = app.load({ fromPoll: true });
  app.disconnectedCallback();
  jobsResponse.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [] }),
  });
  await loadPromise;

  assert.equal(timers.length, 0);
});

test("connected callback preserves monotonic load generation", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  let loadCalled = false;
  app.loadGeneration = 7;
  app.renderShell = () => {};
  app.load = () => {
    loadCalled = true;
  };

  app.connectedCallback();

  assert.equal(loadCalled, true);
  assert.equal(app.loadGeneration, 7);
});

test("pre-disconnect load stays stale after reconnect-style load", async () => {
  const timers = [];
  // Mount-compatible content stub: the jobs route mounts <flow-diagnostics>,
  // so the stub records the element mount() appends.
  const content = {
    innerHTML: "",
    dataset: {},
    firstElementChild: null,
    appendChild(child) {
      this.firstElementChild = child;
    },
  };
  const oldJobs = deferred();
  const newJobs = deferred();
  const responses = [oldJobs.promise, newJobs.promise];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      createElement: (tag) => new SmokeElement(tag),
    },
    fetch(path) {
      assert.equal(path, "/ui/api/v2/jobs");
      const response = responses.shift();
      if (!response) throw new Error("unexpected fetch");
      return response;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    return { textContent: "", innerHTML: "" };
  };

  const oldLoad = app.load({ fromPoll: true });
  app.disconnectedCallback();
  app.pollingActive = true;
  const newLoad = app.load();
  oldJobs.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [{ id: "old-job", state: "running" }] }),
  });
  await oldLoad;
  assert.equal(content.firstElementChild, null, "the stale load mounted nothing");
  assert.equal(timers.length, 0);

  newJobs.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [] }),
  });
  await newLoad;
  assert.equal(content.firstElementChild.data.kind, "jobs");
  assert.deepEqual(content.firstElementChild.data.jobs, [], "the fresh load mounted the empty payload");
  assert.equal(timers[0].delay, 30000);
});

async function createTaskHarness() {
  const status = { textContent: "" };
  const fetchCalls = [];
  let pushedPath = "";
  let loads = 0;
  const context = await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPath = path;
      },
    },
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      throw new Error("new task action should not fetch before submission");
    },
  });
  const app = new context.FlowApp();
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  app.load = async () => {
    loads += 1;
  };

  return {
    fetchCalls,
    status,
    create: () => app.createTask(),
    pushedPath: () => pushedPath,
    loads: () => loads,
  };
}


