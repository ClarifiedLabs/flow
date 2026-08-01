import assert from "node:assert/strict";
import { test } from "node:test";
import { actionScope, applyBusyState, gateResponsePending, handleAction, inFlight, pendingStatus } from "./actions.js";
import { startConsoleView } from "./console-view.js";
import { handleFormSubmit, formBusyKey } from "./forms.js";
import { workflowStepCanBeSkipped } from "./task-view.js";
import { renderTranscriptButton } from "./terminal.js";

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

test("console page offers shell harness and posts selected harness", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/console", search: "" },
  }, {
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

  await app.renderConsole();
  assert.match(content.innerHTML, /<option value="harness" selected>Harness<\/option>/);
  assert.match(content.innerHTML, /<option value="shell">Shell<\/option>/);

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

test("theme switcher defaults to system without a stored override", async () => {
  const harness = await themeShellHarness();
  harness.rootAttributes.set("data-theme", "dark");

  harness.app.renderShell();

  assert.deepEqual(harness.pressedThemes(), ["system"]);
  assert.equal(harness.themeButtons.light.attributes.get("aria-pressed"), "false");
  assert.equal(harness.themeButtons.dark.attributes.get("aria-pressed"), "false");
  assert.equal(harness.rootAttributes.has("data-theme"), false);
  assert.match(harness.app.innerHTML, /data-theme-option/);
});

test("shell keeps the terminal-style brand and New Task action", async () => {
  const harness = await themeShellHarness();

  harness.app.renderShell();

  assert.match(harness.app.innerHTML, /<a class="brand" href="\/ui\/board" data-link>flow<span class="brand-cursor">_<\/span><\/a>/);
  assert.match(harness.app.innerHTML, /<button class="button" data-action="new-task">New Task<\/button>/);
  assert.doesNotMatch(harness.app.innerHTML, /<aside class="sidebar">/);
});

test("theme switcher applies stored overrides and persists user choices", async () => {
  const harness = await themeShellHarness("dark");

  harness.app.renderShell();

  assert.deepEqual(harness.pressedThemes(), ["dark"]);
  assert.equal(harness.rootAttributes.get("data-theme"), "dark");

  harness.themeButtons.light.listeners.get("click")();
  assert.equal(harness.storage.get("flow.ui.theme"), "light");
  assert.equal(harness.rootAttributes.get("data-theme"), "light");
  assert.deepEqual(harness.pressedThemes(), ["light"]);

  harness.themeButtons.system.listeners.get("click")();
  assert.equal(harness.storage.has("flow.ui.theme"), false);
  assert.equal(harness.rootAttributes.has("data-theme"), false);
  assert.deepEqual(harness.pressedThemes(), ["system"]);
});

test("shell replaces the sidebar with a top-bar nav dropdown", async () => {
  const harness = await navShellHarness("/ui/board");

  const html = harness.app.innerHTML;
  assert.match(html, /<header class="topbar">/);
  assert.match(html, /<details class="nav-menu">/);
  assert.match(html, /<summary class="button secondary nav-trigger">/);
  assert.match(html, /<nav class="nav"><\/nav>/);
  assert.match(html, /nav-footer[\s\S]*theme-switcher/);
  assert.doesNotMatch(html, /<aside class="sidebar">/);
  // Before the first /v2/sidebar poll lands the trigger shows only the label.
  assert.match(html, /<span class="nav-trigger-label">board<\/span>/);
  assert.doesNotMatch(html, /nav-board-group/);
});

test("nav trigger label tracks the current route", async () => {
  const harness = await navShellHarness("/ui/board");
  const cases = [
    ["/ui/", "board"],
    ["/ui/board", "board"],
    ["/ui/tasks", "tasks"],
    ["/ui/console", "console"],
    ["/ui/done", "done"],
    ["/ui/flows", "flows"],
    ["/ui/workers", "workers"],
    ["/ui/jobs", "jobs"],
    ["/ui/tasks/new", "board"],
    ["/ui/tasks/t-0001", "board"],
    ["/ui/tasks/t-0001/epic", "board"],
    ["/ui/projects/p-alpha/tasks/t-0001", "board"],
    ["/ui/changes/ch-0001", "menu"],
    ["/ui/sessions/s-0001/terminal", "menu"],
  ];

  for (const [path, label] of cases) {
    harness.context.window.location.pathname = path;
    harness.app.updateActiveNav();
    assert.match(
      harness.trigger.innerHTML,
      new RegExp(`<span class="nav-trigger-label">${label}</span>`),
      `trigger label for ${path}`,
    );
  }
});

test("nav trigger gains board lane chips once sidebar status lands", async () => {
  const fetchCalls = [];
  const harness = await navShellHarness("/ui/board", (path) => {
    fetchCalls.push(path);
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        board: { unscheduled: 2, scheduled: 3, in_progress: 4, blocked: 1 },
      }),
    });
  });

  assert.doesNotMatch(harness.app.innerHTML, /nav-board-group/);

  await harness.app.refreshSidebarStatus();

  assert.deepEqual(fetchCalls, ["/ui/api/v2/sidebar"]);
  assert.match(harness.trigger.innerHTML, /class="nav-board-group"/);
  assert.match(harness.trigger.innerHTML, /data-board-lane="blocked" title="1 blocked task">1<\/span>/);
  // The panel keeps the eight nav destinations with the board badge markup.
  assert.equal(harness.nav.links.length, 8);
  assert.match(harness.nav.innerHTML, /class="nav-board-group"/);
});

test("active nav link keeps aria-current in the dropdown panel", async () => {
  const harness = await navShellHarness("/ui/done");

  harness.app.updateActiveNav();

  const active = harness.nav.links.filter((link) => link.attributes.get("aria-current") === "page");
  assert.deepEqual(active.map((link) => link.href), ["/ui/done"]);
});

test("selecting a nav link closes the dropdown and navigates client-side", async () => {
  const harness = await navShellHarness("/ui/board");
  harness.navMenu.open = true;
  const done = harness.nav.links.find((link) => link.href === "/ui/done");
  let prevented = false;

  done.listeners.get("click")({ preventDefault() { prevented = true; } });

  assert.equal(prevented, true);
  assert.equal(harness.navMenu.open, false);
  assert.deepEqual(harness.pushed, ["/ui/done"]);
});

test("clicking outside the nav dropdown closes it, clicking inside does not", async () => {
  const harness = await navShellHarness("/ui/board");

  harness.navMenu.open = true;
  harness.app.handleMenuDismiss({ target: { closest: () => null } });
  assert.equal(harness.navMenu.open, false);

  harness.navMenu.open = true;
  harness.app.handleMenuDismiss({
    target: { closest: (selector) => (selector === ".nav-menu" ? harness.navMenu : null) },
  });
  assert.equal(harness.navMenu.open, true);
});

test("Escape closes the nav dropdown without navigating", async () => {
  const harness = await navShellHarness("/ui/tasks/t-0001");
  harness.navMenu.open = true;

  harness.app.handleShortcut({ key: "Escape", target: { tagName: "DIV" } });

  assert.equal(harness.navMenu.open, false);
  assert.deepEqual(harness.pushed, []);
});

test("Escape still returns to the board once the dropdown is closed", async () => {
  const harness = await navShellHarness("/ui/tasks/t-0001");

  harness.app.handleShortcut({ key: "Escape", target: { tagName: "DIV" } });

  assert.deepEqual(harness.pushed, ["/ui/board"]);
});

test("opening one top-bar menu closes the other", async () => {
  const harness = await navShellHarness("/ui/board");

  harness.picker.open = true;
  harness.navMenu.open = true;
  harness.navMenu.listeners.get("toggle")();
  assert.equal(harness.picker.open, false);

  harness.picker.open = true;
  harness.picker.listeners.get("toggle")();
  assert.equal(harness.navMenu.open, false);
});

test("toggling the theme inside the panel does not close the dropdown", async () => {
  const harness = await navShellHarness("/ui/board");
  harness.navMenu.open = true;

  harness.themeButtons.dark.listeners.get("click")();

  assert.equal(harness.storage.get("flow.ui.theme"), "dark");
  assert.equal(harness.themeButtons.dark.attributes.get("aria-pressed"), "true");
  assert.equal(harness.navMenu.open, true);
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
      if (path === "/ui/api/v2/projects/p-alpha/tasks") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            tasks: [
              { id: "t-alpha-0001", title: "First task" },
              { id: "t-alpha-0002", title: "Second task" },
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
    "/ui/api/v2/projects/p-alpha/features?status=all",
    "/ui/api/v2/projects/p-alpha/tasks",
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
  // The relation picker's target-task datalist is populated from the selected
  // project's freshly loaded tasks, even though the cache started empty.
  assert.match(content.innerHTML, /<datalist id="relation-target-tasks">/);
  assert.match(content.innerHTML, /<option value="t-alpha-0001" label="First task"><\/option>/);
  assert.match(content.innerHTML, /<option value="t-alpha-0002" label="Second task"><\/option>/);
  assert.match(content.innerHTML, /<input name="queue_task" type="checkbox" checked>/);
  assert.match(content.innerHTML, /<button class="button" type="submit">Create<\/button>/);
  assert.equal(status.textContent, "");
});

test("new task route keeps the relation picker in manual-entry mode when task suggestions fail to load", async () => {
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
      if (path === "/ui/api/v2/projects/p-alpha/tasks") {
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

  // The failed task fetch is swallowed: the route still renders and the picker
  // simply has no suggestions, leaving the target input free-text.
  assert.ok(fetchCalls.some((call) => call.path === "/ui/api/v2/projects/p-alpha/tasks"));
  assert.equal(title.textContent, "New Task");
  assert.match(content.innerHTML, /<datalist id="relation-target-tasks"><\/datalist>/);
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
  });
  assert.equal(pushedPath, "/ui/tasks/t-alpha-0001");
  assert.equal(loads, 1);
});
test("task form renders the relation picker only in create mode", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];

  const createHTML = app.renderTaskForm({ title: "", priority: 0 }, { mode: "create", submitLabel: "Create" });
  assert.match(createHTML, /data-relation-picker/);
  assert.match(createHTML, /data-relation-rows/);
  assert.match(createHTML, /data-relation-add/);
  assert.match(createHTML, /<option value="parent_of" >child of<\/option>/);
  assert.match(createHTML, /<option value="blocks" >blocks<\/option>/);
  assert.match(createHTML, /<option value="related_to" >related to<\/option>/);

  const editHTML = app.renderTaskForm({ title: "T" }, { taskID: "t-alpha-0001", projectID: "p-alpha" });
  assert.doesNotMatch(editHTML, /data-relation-picker/);
  assert.doesNotMatch(editHTML, /data-relation-add/);
  assert.doesNotMatch(editHTML, /data-relation-row/);
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
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).relations, [
    { target_task_id: "t-alpha-0002", kind: "blocks" },
    { target_task_id: "t-alpha-0003", kind: "related_to" },
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
  // target_is_new_task, so the server applies it in the create transaction. There
  // is no second, post-create link call that could partially succeed after the
  // task has already been committed.
  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).relations, [
    { target_task_id: "t-alpha-0003", kind: "blocks" },
    { source_task_id: "t-alpha-0001", target_task_id: "", kind: "parent_of", target_is_new_task: true },
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
      return Promise.resolve({
        ok: false,
        status: 400,
        json: () => Promise.resolve({ error: { message: "task relation would create a cycle" } }),
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
  assert.match(statuses.at(-1), /task relation would create a cycle/);
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
    assert.deepEqual(JSON.parse(call.options.body).relations, [
      { source_task_id: "t-alpha-0001", target_task_id: "", kind: "parent_of", target_is_new_task: true },
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
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body).relations, [
    { target_task_id: "t-alpha-0002", kind: "related_to" },
  ]);

  const allBlank = createFormWithRelations([relationRow("blocks", "")]);
  await handleFormSubmit(app, { target: allBlank, preventDefault() {} });
  assert.equal("relations" in JSON.parse(fetchCalls[1].options.body), false);
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
  // one parent. The second link would fail after the create POST committed,
  // leaving a partially related task, so the submission must be rejected
  // before any request goes out.
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
// A button with just enough surface for handleAction's synchronous pending
// state: a dataset, a disabled flag, and attribute/class tracking.
class ActionButton {
  constructor(dataset = {}) {
    this.dataset = dataset;
    this.disabled = false;
    this.attributes = new Map();
    this.classes = new Set();
    this.classList = {
      add: (name) => this.classes.add(name),
      remove: (name) => this.classes.delete(name),
      contains: (name) => this.classes.has(name),
    };
  }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }
  removeAttribute(name) {
    this.attributes.delete(name);
  }
}

// A gate outcome button: an ActionButton that also knows its enclosing gate
// panel and its sibling outcome controls, so handleAction can suppress the
// whole set when one of them is clicked.
class GateButton extends ActionButton {
  constructor(dataset, panel) {
    super(dataset);
    this.panel = panel;
  }
  closest(selector) {
    if (selector === "[data-gate-panel]" || selector === "[data-gate-node-run]") return this.panel;
    return null;
  }
}

// A gate panel stub exposing the outcome buttons to suppressGateSiblings and a
// null feedback textarea to the workflowRespond handler.
function gatePanel() {
  return {
    buttons: [],
    querySelector() {
      return null;
    },
    querySelectorAll(selector) {
      return selector === "[data-workflow-respond]" ? this.buttons : [];
    },
  };
}

function statusApp() {
  const statuses = [];
  return {
    statuses,
    setStatus(message) {
      statuses.push(message);
    },
    refresh() {},
  };
}

test("manual scope review requests a typed convergence hold", async () => {
  await scriptContext();
  const app = statusApp();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({
    convergenceRequest: "t-0043",
    project: "p-alpha",
  });

  assert.equal(await handleAction(app, { target: button, preventDefault() {} }), true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0043/workflow/convergence/request");
  assert.deepEqual(JSON.parse(calls[0].options.body), {});
  assert.deepEqual(app.statuses, [
    "Starting scope review t-0043\u2026",
    "Convergence review started for t-0043",
  ]);
});

test("a convergence decision posts an explicit disposition instead of a workflow edge", async () => {
  await scriptContext();
  const app = statusApp();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({
    convergenceDecision: "t-0043",
    disposition: "accept_scope",
    evidenceFingerprint: "sha256:reviewed-evidence",
    project: "p-alpha",
  });
  button.closest = (selector) => selector === "[data-convergence-panel]"
    ? { querySelector: () => ({ value: "  reviewed scope  " }) }
    : button;

  assert.equal(await handleAction(app, { target: button, preventDefault() {} }), true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0043/workflow/convergence");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    disposition: "accept_scope",
    expected_evidence_fingerprint: "sha256:reviewed-evidence",
    note: "reviewed scope",
  });
  assert.deepEqual(app.statuses, [
    "Resolving convergence review t-0043\u2026",
    "Continuing t-0043 as-is",
  ]);
});

test("an action click marks the control busy and names the in-flight action before the request resolves", async () => {
  await scriptContext();
  const app = statusApp();
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending state is synchronous: before the network resolves the control
  // is disabled and aria-busy, and the status line names the action.
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026"]);

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Scheduled"]);
});

test("a poll re-render replacing the button re-applies the busy state instead of re-enabling it", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: first, preventDefault() {} });
  assert.equal(requests, 1);

  // The busy metadata lives outside the node: the pending label survives the
  // route render that clears the status line.
  assert.equal(pendingStatus(), "Scheduling t-0001\u2026");

  // The board's 10 s poll repaints and swaps the button node for a fresh one.
  // The repaint re-applies the in-flight state from the registry, so the
  // replacement is disabled and visibly busy — not actionable.
  const replacement = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  applyBusyState({ querySelectorAll: () => [replacement] });
  assert.equal(replacement.disabled, true);
  assert.equal(replacement.getAttribute("aria-busy"), "true");
  assert.equal(replacement.classList.contains("is-busy"), true);

  // Clicking the replacement while the first request is still in flight must
  // not issue a second request: the guard lives in the in-flight registry, not
  // on the (now discarded) node.
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;

  // Settling restores whatever control is on screen now — the repaint-marked
  // replacement — not the discarded node the click started on.
  assert.equal(replacement.disabled, false);
  assert.equal(replacement.getAttribute("aria-busy"), null);
  assert.equal(replacement.classList.contains("is-busy"), false);
  assert.equal(pendingStatus(), "");

  // Once the first request settles the action is available again.
  const second = handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("answering a gate suppresses every sibling outcome until the response settles", async () => {
  await scriptContext();
  const app = statusApp();
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const revise = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "changes_requested", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, revise, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });

  // Synchronously, before the request resolves, every outcome for the node run
  // is suppressed — not just the one that was clicked.
  for (const control of [approve, revise, reject]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-busy"), "true");
    assert.equal(control.classList.contains("is-busy"), true);
  }

  // The shared in-flight registry reports the gate response as pending, which
  // is exactly what the render path consults to re-suppress fresh buttons after
  // a poll repaint — so no sibling flashes enabled while the request is out.
  assert.equal(gateResponsePending("wnr-1"), true);

  resolveRequest();
  assert.equal(await handled, true);

  // Settling restores the live outcome controls and clears the pending flag.
  for (const control of [approve, revise, reject]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.equal(gateResponsePending("wnr-1"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "Feedback sent"]);
  assert.equal(inFlight.size, 0);
});

test("a failed gate response restores every suppressed sibling outcome", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(reject.disabled, true, "the sibling is suppressed while the response is pending");

  rejectRequest();
  assert.equal(await handled, true);

  // Failure restores every outcome control, leaving the gate fully actionable.
  assert.equal(approve.disabled, false);
  assert.equal(reject.disabled, false);
  assert.equal(reject.getAttribute("aria-busy"), null);
  assert.equal(reject.classList.contains("is-busy"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
});

test("a repaint mid-flight leaves no live outcome disabled after a failed settlement", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(reject.disabled, true, "the sibling is suppressed while the response is pending");

  // A poll repaints while the response is still pending: the render path
  // re-emits every outcome disabled (the shared key is still in flight) and
  // swaps them in for the now-detached originals the click captured.
  const liveApprove = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const liveReject = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  for (const control of [liveApprove, liveReject]) {
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-busy");
  }
  panel.buttons = [liveApprove, liveReject];
  globalThis.document = {
    querySelectorAll: (selector) => (selector === '[data-workflow-respond="wnr-1"]' ? panel.buttons : []),
  };

  rejectRequest();
  assert.equal(await handled, true);

  // Failure clears the shared key but issues no refresh, so nothing else
  // repaints the panel. The live replacement outcomes must be restored
  // directly — not left disabled/aria-busy/is-busy until a later poll.
  for (const control of [liveApprove, liveReject]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.equal(gateResponsePending("wnr-1"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
});

test("approving one human check does not block a different check on the same task", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const checkA = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });
  const checkB = new ActionButton({ humanReviewApprove: "t-0001", checkName: "docs", project: "p-alpha" });

  const approveA = handleAction(app, { target: checkA, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(checkA.disabled, true);

  // A different check on the same task has its own busy identity, so approving
  // it issues its own request instead of being suppressed by the in-flight one.
  const approveB = handleAction(app, { target: checkB, preventDefault() {} });
  assert.equal(requests, 2, "a distinct check is not blocked by the in-flight one");
  assert.equal(checkB.disabled, true);
  assert.equal(checkA.disabled, true, "the first check stays busy while its request is in flight");

  resolvers[0]();
  resolvers[1]();
  await approveA;
  await approveB;
  assert.equal(checkA.disabled, false);
  assert.equal(checkB.disabled, false);
  assert.equal(inFlight.size, 0);
});

test("a duplicate approval of the same human check stays suppressed while its request is in flight", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });

  const handled = handleAction(app, { target: first, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.disabled, true);
  assert.equal(first.getAttribute("aria-busy"), "true");
  assert.equal(first.classList.contains("is-busy"), true);

  // A poll repaint swaps the button for a fresh enabled node carrying the same
  // task and check; clicking it must not issue a second request.
  const replacement = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });
  assert.equal(replacement.disabled, false);
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "the same check's duplicate approval is suppressed");

  resolveRequest();
  await handled;

  // Once the request settles the check is available again.
  const second = handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("a failed action keeps the error on the status line and restores the control", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () =>
    Promise.resolve({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: "workflow is locked" } }),
    });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "workflow is locked"]);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on failure");
});

test("a console start marks the control busy, blocks a duplicate across a repaint, and confirms the start", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  app.querySelector = (selector) => (selector === "[data-console-harness]" ? { value: "shell" } : null);
  const requests = [];
  let resolveRequest;
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  // The Console view's project-level Start button: data-start-console is
  // empty (no task), the console target lives in data-project/data-task.
  const button = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending state is synchronous: the control is disabled and aria-busy
  // and the status line names the action before the POST resolves. The
  // harness is the one picked in the view's select.
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Starting console\u2026"]);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, "/ui/api/v2/projects/p-alpha/console");
  assert.deepEqual(JSON.parse(requests[0].options.body), { harness: "shell" });

  // A console repaint mid-flight swaps the button node: the registry
  // re-applies the busy state to the replacement and the repeat click is
  // swallowed without a second request.
  const replacement = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });
  applyBusyState({ querySelectorAll: () => [replacement] });
  assert.equal(replacement.disabled, true);
  assert.equal(replacement.getAttribute("aria-busy"), "true");
  assert.equal(replacement.classList.contains("is-busy"), true);
  assert.equal(await handleAction(app, { target: replacement, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "no duplicate console start while the first is in flight");

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(loads, 1, "the console view reloads after the start");
  assert.deepEqual(app.statuses, ["Starting console\u2026", "Console starting"]);
  assert.equal(replacement.disabled, false);
  assert.equal(replacement.getAttribute("aria-busy"), null);
  assert.equal(replacement.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on success");
});

test("a console release marks the control busy, suppresses a duplicate, and confirms the release", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  const requests = [];
  let resolveRequest;
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const button = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Releasing console t-0001\u2026"]);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/console");
  assert.equal(requests[0].options.method, "DELETE");

  // A fresh node for the same console target is rejected by the registry
  // even before any busy marking: no second DELETE goes out.
  const repeat = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });
  assert.equal(await handleAction(app, { target: repeat, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "no duplicate console release while the first is in flight");

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(loads, 1, "the console view reloads after the release");
  assert.deepEqual(app.statuses, ["Releasing console t-0001\u2026", "Console released"]);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on success");
});

test("a failed console start keeps the error on the status line and restores the control", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  globalThis.fetch = () =>
    Promise.resolve({ ok: false, status: 409, json: () => Promise.resolve({ error: { message: "console is locked" } }) });
  const button = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const handled = handleAction(app, { target: button, preventDefault() {} });
  assert.equal(button.disabled, true);
  assert.deepEqual(app.statuses, ["Starting console t-0001\u2026"]);

  assert.equal(await handled, true);
  assert.deepEqual(app.statuses, ["Starting console t-0001\u2026", "console is locked"]);
  assert.equal(loads, 0, "a rejected start never reaches the reload");
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on failure");
});

test("starting one console does not suppress a different console target", async () => {
  await scriptContext();
  const app = statusApp();
  app.load = async () => {};
  app.querySelector = () => null;
  const requests = [];
  const resolvers = new Map();
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolvers.set(path, () => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const taskConsole = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const taskStart = handleAction(app, { target: taskConsole, preventDefault() {} });
  assert.equal(requests.length, 1);
  assert.equal(taskConsole.disabled, true);

  // The same console target stays blocked while its start is in flight...
  const repeat = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });
  assert.equal(await handleAction(app, { target: repeat, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "the same console target stays suppressed");

  // ...but the project console is a different target: its start proceeds.
  const projectConsole = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });
  const projectStart = handleAction(app, { target: projectConsole, preventDefault() {} });
  assert.equal(requests.length, 2, "a distinct console target is not blocked");
  assert.equal(requests[1].path, "/ui/api/v2/projects/p-alpha/console");
  assert.equal(projectConsole.disabled, true);

  resolvers.get("/ui/api/v2/projects/p-alpha/tasks/t-0001/console")();
  assert.equal(await taskStart, true);
  resolvers.get("/ui/api/v2/projects/p-alpha/console")();
  assert.equal(await projectStart, true);
  // The task console settles first, but the project console is still in
  // flight, so settlement keeps its pending label on the line and reveals the
  // confirmation only when the final start settles.
  assert.deepEqual(app.statuses, [
    "Starting console t-0001\u2026",
    "Starting console\u2026",
    "Starting console\u2026",
    "Console starting",
  ]);
  assert.equal(taskConsole.disabled, false);
  assert.equal(projectConsole.disabled, false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains once both settle");
});

// A promise can reject with a value that is not an Error (a bare reject(null),
// an abort, or fetch middleware). Formatting that failure must stay total: if
// reading error.message threw before settleStatus ran, the in-flight key would
// leak and a repainted control would stay disabled forever.
test("a non-Error action rejection still drains the registry and shows a final failure", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () => Promise.reject(null);
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Request failed"]);
  assert.equal(button.disabled, false, "the control is restored");
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a non-Error rejection");

  // A repainted replacement is accepted again, proving the key did not leak.
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
  };
  const replacement = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "the replacement control is not rejected by a leaked key");
  assert.equal(inFlight.size, 0);
});

test("a non-Error form rejection still drains the registry and shows a final failure", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () => Promise.reject(null);
  const submitter = new ActionButton();
  const form = {
    tagName: "FORM",
    dataset: { project: "p-alpha", taskForm: "t-0001", taskFormMode: "edit" },
    elements: {
      priority: { value: "0" },
      title: { value: "Renamed" },
      body: { value: "" },
      flow_id: { value: "" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitter : null;
    },
  };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Request failed"]);
  assert.equal(submitter.disabled, false, "the submit control is restored");
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a non-Error rejection");
});

// Two distinct mutations may be in flight at once; the shared status line must
// keep naming a still-pending mutation after an earlier one settles, and only
// reveal a result once nothing is pending. These cover out-of-order success
// and failure settlement.
function controllableFetch() {
  const pending = [];
  globalThis.fetch = () =>
    new Promise((resolve) => {
      pending.push((ok = true, body = {}) =>
        resolve({ ok, status: ok ? 200 : 409, json: () => Promise.resolve(body) }),
      );
    });
  return pending;
}

test("settling one action keeps a still-pending sibling's label on the status line", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Scheduling t-0002\u2026"]);
  assert.equal(inFlight.size, 2);

  // The second mutation settles first: its "Scheduled" must not clobber the
  // first mutation's still-pending label.
  pending[1](true);
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0001\u2026");
  assert.equal(inFlight.size, 1);

  // When the final mutation settles, its own result stays visible.
  pending[0](true);
  await first;
  assert.deepEqual(app.statuses, [
    "Scheduling t-0001\u2026",
    "Scheduling t-0002\u2026",
    "Scheduling t-0001\u2026",
    "Scheduled",
  ]);
  assert.equal(inFlight.size, 0);
});

test("an early failure does not hide a still-pending sibling's label", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });

  // The first mutation fails while the second is still running: the failure is
  // suppressed in favour of the second mutation's pending label.
  pending[0](false, { error: { message: "workflow is locked" } });
  await first;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0002\u2026");
  assert.equal(inFlight.size, 1);

  // The final mutation's success then stays visible.
  pending[1](true);
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduled");
  assert.equal(inFlight.size, 0);
});

test("the final mutation's failure stays visible after a sibling already settled", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });

  // The first mutation succeeds while the second is pending: its result is
  // suppressed and the second's pending label stays.
  pending[0](true);
  await first;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0002\u2026");

  // The final mutation fails: with nothing left pending, the failure shows.
  pending[1](false, { error: { message: "boom" } });
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "boom");
  assert.equal(inFlight.size, 0);
});

test("a cancelled confirm clears the pending label and issues no request", async () => {
  await scriptContext({ confirm: () => false });
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowReset: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending label is written synchronously on click, before the confirm.
  assert.deepEqual(app.statuses, ["Resetting t-0001\u2026"]);
  assert.equal(button.disabled, true);

  assert.equal(await handled, true);
  // Backing out of the confirm clears the pending label the click created and
  // restores the control — no request went out.
  assert.deepEqual(app.statuses, ["Resetting t-0001\u2026", ""]);
  assert.equal(requests, 0, "a cancelled confirm issues no request");
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0);
});

test("a cancelled prompt clears the pending label and issues no request", async () => {
  await scriptContext({ prompt: () => null });
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowDone: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });
  assert.deepEqual(app.statuses, ["Closing out t-0001\u2026"]);

  assert.equal(await handled, true);
  assert.deepEqual(app.statuses, ["Closing out t-0001\u2026", ""]);
  assert.equal(requests, 0, "a cancelled prompt issues no request");
  assert.equal(button.disabled, false);
  assert.equal(inFlight.size, 0);
});

test("a form submission marks its submit control busy and guards against a duplicate submit", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  const handled = handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(requests, 1);
  assert.equal(submitButton.disabled, true);
  assert.equal(submitButton.getAttribute("aria-busy"), "true");
  assert.deepEqual(app.statuses, ["Saving task\u2026"]);

  // A second submit while the first is in flight must not issue another request.
  await handleFormSubmit(app, { target: form, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;
  assert.equal(submitButton.disabled, false);
  assert.equal(submitButton.getAttribute("aria-busy"), null);
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Task updated"]);
});

test("a poll re-render replacing the form re-applies the busy state to the replacement submitter", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const makeForm = (submitter) => ({
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitter : null;
    },
  });

  const firstSubmit = new ActionButton({});
  const handled = handleFormSubmit(app, { target: makeForm(firstSubmit), preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(firstSubmit.disabled, true);
  assert.deepEqual(app.statuses, ["Saving task\u2026"]);

  // The poll swaps the form for a fresh one. The repaint re-applies the
  // in-flight state from the registry, so the replacement's submit control is
  // disabled and visibly busy — not apparently actionable but inert.
  const replacementSubmit = new ActionButton({});
  const replacementForm = makeForm(replacementSubmit);
  applyBusyState({ querySelectorAll: (selector) => (selector === "form" ? [replacementForm] : []) });
  assert.equal(replacementSubmit.disabled, true);
  assert.equal(replacementSubmit.getAttribute("aria-busy"), "true");
  assert.equal(replacementSubmit.classList.contains("is-busy"), true);

  // Submitting the replacement while the first request is in flight must not
  // issue a second request: the guard lives in the registry, not on the node.
  await handleFormSubmit(app, { target: replacementForm, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;

  // Settling restores whatever control is on screen now — the repaint-marked
  // replacement — not the discarded original form's submitter.
  assert.equal(replacementSubmit.disabled, false);
  assert.equal(replacementSubmit.getAttribute("aria-busy"), null);
  assert.equal(replacementSubmit.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0);

  // Once the first submission settles the form is submittable again.
  const second = handleFormSubmit(app, { target: replacementForm, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("a validation-cancelled form submit replaces the pending label with the validation error", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "   " },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The pending label is replaced by the validation error, which stays visible
  // rather than being cleared.
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Task title is required"]);
  assert.equal(requests, 0, "a validation-cancelled submit issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(submitButton.getAttribute("aria-busy"), null);
  assert.equal(inFlight.size, 0);
});

test("a backed-out form submit clears the pending label it created", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { threadReplyForm: "th-0001" },
    elements: {
      body: { value: "   " },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The handler backed out with nothing to show, so the pending label is cleared.
  assert.deepEqual(app.statuses, ["Posting reply\u2026", ""]);
  assert.equal(requests, 0, "a backed-out submit issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(inFlight.size, 0);
});

test("an empty relation target keeps its validation failure visible when it is the final submission", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { relationAddForm: "t-0001", project: "p-alpha" },
    elements: {
      kind: { value: "blocked_by" },
      target_task_id: { value: "   " },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The validation failure is the final mutation's outcome, so it must remain
  // on the status line rather than being cleared by settlement.
  assert.deepEqual(app.statuses, ["Adding relation\u2026", "Target task ID is required"]);
  assert.equal(requests, 0, "an empty relation target issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(inFlight.size, 0);
});

// An attachment form carries enough surface for handleFormSubmit's pending
// state plus the attachmentForm handler: a dataset, a submit control, a file
// input, a stage select, and a reset() the handler calls on success.
function attachmentFormFixture(dataset) {
  const submitButton = new ActionButton({});
  return {
    submitButton,
    form: {
      tagName: "FORM",
      dataset,
      elements: {
        file: { files: [new Blob(["attachment body"], { type: "text/plain" })] },
        stage: { value: "initial" },
      },
      reportValidity() {
        return true;
      },
      querySelector(selector) {
        return selector === '[type="submit"]' ? submitButton : null;
      },
      reset() {},
    },
  };
}

test("attachment forms for distinct task targets submit concurrently", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const first = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });
  const second = attachmentFormFixture({ attachmentForm: "", task: "t-0002", project: "p-alpha" });

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // The second uploader targets a different task, so its busy identity is
  // distinct and it is not suppressed by the first form's in-flight upload.
  const secondHandled = handleFormSubmit(app, { target: second.form, preventDefault() {} });
  assert.equal(requests, 2, "a distinct task target is not blocked by the in-flight upload");
  assert.equal(second.submitButton.disabled, true);
  assert.equal(first.submitButton.disabled, true, "the first form stays busy while its upload is in flight");

  resolvers[0]();
  resolvers[1]();
  await firstHandled;
  await secondHandled;
  assert.equal(inFlight.size, 0);
});

test("a poll re-render replacing an attachment form cannot re-enable a duplicate submission", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });

  const handled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);

  // The task view repaints on a 10 s poll and swaps the form node for a fresh
  // one carrying the same target identity; its submit control starts enabled.
  const replacement = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });
  assert.equal(replacement.submitButton.disabled, false);

  // Submitting the replacement while the first upload is in flight must not
  // issue a second request: the guard lives in the in-flight registry keyed by
  // target identity, not on the (now discarded) node.
  await handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;
  assert.equal(inFlight.size, 0);

  // Once the first upload settles, the same target can submit again.
  const second = handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("formBusyKey gives concurrent task forms distinct, stable busy identities", () => {
  // A boolean data-attachment-form carries an empty primary value, so two
  // uploaders on different tasks must not collapse onto one key.
  assert.notEqual(
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0002", project: "p-alpha" } }),
  );
  // The same target is stable across a repaint that rebuilds the dataset.
  assert.equal(
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
  );
  // Edit forms for two tasks, and a create form (empty data-task-form) versus
  // an edit form, each get their own identity so concurrent forms do not
  // suppress one another.
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" } }),
    formBusyKey("taskForm", { dataset: { taskForm: "t-0002", taskFormMode: "edit", project: "p-alpha" } }),
  );
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" } }),
  );
  // A create form rendered with several projects carries no data-project; its
  // mutation target is the selected project, so two creates for different
  // projects must not collapse onto `form:taskForm::`.
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-beta" } } }),
  );
  // The selected-project identity is stable across a repaint that rebuilds the
  // form node with the same selection.
  assert.equal(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
  );
});

test("a create task form and an edit task form submit concurrently", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) }));
    });
  };
  const createSubmit = new ActionButton({});
  const createForm = {
    tagName: "FORM",
    // A create form rendered with several projects carries no data-project; the
    // mutation target is the selected project in the project <select>.
    dataset: { taskForm: "", taskFormMode: "create" },
    elements: {
      project: { value: "p-alpha" },
      priority: { value: "1" },
      flow_id: { value: "fl-coding" },
      title: { value: "New task" },
      body: { value: "Body" },
      attachments: { files: [] },
      queue_task: { checked: false },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? createSubmit : null;
    },
  };
  const editSubmit = new ActionButton({});
  const editForm = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? editSubmit : null;
    },
  };

  const createHandled = handleFormSubmit(app, { target: createForm, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(createSubmit.disabled, true);

  // The edit form targets an existing task, so its busy identity differs from
  // the create form's and it is not suppressed by the in-flight create.
  const editHandled = handleFormSubmit(app, { target: editForm, preventDefault() {} });
  assert.equal(requests, 2, "a concurrent edit form is not blocked by the in-flight create");
  assert.equal(editSubmit.disabled, true);

  resolvers[0]();
  resolvers[1]();
  await createHandled;
  await editHandled;
  assert.equal(inFlight.size, 0);
});

test("multi-project create forms for different projects submit concurrently", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) }));
    });
  };
  // The real multi-project create form shape from renderTaskFormView: no
  // data-project, with the mutation target in the project <select>. Two such
  // forms selected for different projects must not share `form:taskForm::`.
  function createFormFixture(projectID) {
    const submitButton = new ActionButton({});
    return {
      submitButton,
      form: {
        tagName: "FORM",
        dataset: { taskForm: "", taskFormMode: "create" },
        elements: {
          project: { value: projectID },
          priority: { value: "1" },
          flow_id: { value: "fl-coding" },
          title: { value: "New task" },
          body: { value: "Body" },
          attachments: { files: [] },
          queue_task: { checked: false },
        },
        reportValidity() {
          return true;
        },
        querySelector(selector) {
          return selector === '[type="submit"]' ? submitButton : null;
        },
      },
    };
  }
  const first = createFormFixture("p-alpha");
  const second = createFormFixture("p-beta");

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // The second create targets a different project, so its busy identity is
  // distinct and it is not suppressed by the first form's in-flight create.
  const secondHandled = handleFormSubmit(app, { target: second.form, preventDefault() {} });
  assert.equal(requests, 2, "a distinct project create is not blocked by the in-flight create");
  assert.equal(second.submitButton.disabled, true);
  assert.equal(first.submitButton.disabled, true, "the first form stays busy while its create is in flight");

  resolvers[0]();
  resolvers[1]();
  await firstHandled;
  await secondHandled;
  assert.equal(inFlight.size, 0);
});

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

test("jobs view shows project column, filters by project, and sorts by updated", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      assert.equal(path, "/ui/api/v2/jobs");
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          jobs: [
            // Intentionally out of updated order across two projects to prove
            // the view re-sorts globally rather than trusting server order.
            { id: "j-old", state: "finished", role: "ci", updated_at: "2026-06-01T00:00:00Z" },
            { id: "j-mid", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
            { id: "j-new", state: "failed", role: "reviewer", updated_at: "2026-06-09T00:00:00Z" },
          ],
          diagnostics: {
            "j-old": { project_name: "beta" },
            "j-mid": { project_name: "alpha" },
            "j-new": { project_name: "beta" },
          },
        }),
      });
    },
  });

  const content = { innerHTML: "" };
  const app = new context.FlowApp();
  app.setTitle = () => {};
  app.bindTaskActions = () => {};
  app.isActiveLoad = () => true;
  app.querySelector = () => content;
  // Stub the per-view control listeners so change handlers do not blow up;
  // the table body is rendered into content.innerHTML up front.
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    return null;
  };

  await context.renderJobsView(app);

  const html = content.innerHTML;
  // Project column renders the per-job project name.
  assert.match(html, /<th>Project<\/th>/);
  assert.match(html, /alpha/);
  assert.match(html, /beta/);
  // Default sort is updated desc, so j-new (Jun 9) precedes j-mid (Jun 5)
  // which precedes j-old (Jun 1).
  const newIdx = html.indexOf("j-new");
  const midIdx = html.indexOf("j-mid");
  const oldIdx = html.indexOf("j-old");
  assert.ok(newIdx > -1 && midIdx > -1 && oldIdx > -1, "all job rows rendered");
  assert.ok(newIdx < midIdx, "j-new before j-mid");
  assert.ok(midIdx < oldIdx, "j-mid before j-old");
  // Filter and sort controls are present with the default selection.
  assert.match(html, /data-jobs-filter/);
  assert.match(html, /data-jobs-sort-field/);
  assert.match(html, /data-jobs-sort-order/);
  assert.match(html, /<option value="updated" selected>Updated<\/option>/);
  assert.match(html, /<option value="desc" selected>Newest first<\/option>/);
  // State colors render via row tint classes.
  assert.match(html, /class="row-ok"/);
  assert.match(html, /class="row-run"/);
  assert.match(html, /class="row-danger"/);
});

test("jobs view filter selects only the chosen project", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          jobs: [
            { id: "j-a", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
            { id: "j-b", state: "running", role: "author", updated_at: "2026-06-09T00:00:00Z" },
          ],
          diagnostics: {
            "j-a": { project_name: "alpha" },
            "j-b": { project_name: "beta" },
          },
        }),
      });
    },
  });

  const content = { innerHTML: "" };
  const app = new context.FlowApp();
  app.setTitle = () => {};
  app.bindTaskActions = () => {};
  app.isActiveLoad = () => true;
  app.querySelector = (selector) => (selector === ".content" ? content : null);

  // Pretend the user picked the "beta" project filter before this render.
  app.jobsView = { filter: "beta", sort: { field: "updated", order: "desc" } };
  await context.renderJobsView(app);

  const html = content.innerHTML;
  assert.match(html, /j-b/);
  assert.doesNotMatch(html, /j-a/);
  // The beta option is the selected one.
  assert.match(html, /<option value="beta" selected>beta<\/option>/);
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

// A settle-burst harness: a real FlowApp with a recording setTimeout and a
// stub load() that counts invocations and mimics the real load's contract —
// the generation moves synchronously as the load starts and the returned
// context carries the path the load started on — so refresh() and the burst
// ticks observe the same load-generation movement as with the real load. The
// status line is stubbed because handleAction writes to it.
async function settleBurstHarness(pathname = "/ui/board") {
  const timers = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  });
  context.window.location.pathname = pathname;
  const app = new context.FlowApp();
  app.pollingActive = true;
  const status = { textContent: "" };
  app.querySelector = (selector) => (selector === ".status" ? status : null);
  let loads = 0;
  app.load = async () => {
    loads += 1;
    const loadContext = {
      generation: (app.loadGeneration || 0) + 1,
      path: context.window.location.pathname,
    };
    app.loadGeneration = loadContext.generation;
    return loadContext;
  };
  return {
    app,
    context,
    status,
    timers,
    loads: () => loads,
    // actionRefresh runs a refresh the way an action handler does: the
    // dispatcher hands the handler an action-scoped app whose refresh carries
    // the ACTION_SETTLE provenance token — which is how refresh() tells an
    // action-triggered refresh (arm the settle burst) from an ordinary one
    // (stay one load).
    async actionRefresh() {
      await actionScope(app).refresh();
    },
    // fire runs a pending timer callback the way the browser would — Poller's
    // wrapper invokes the async burst tick without awaiting it — then flushes
    // the microtask queue so the tick settles.
    async fire(index) {
      timers[index].callback();
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

test("a successful action arms a bounded settle burst of follow-up reloads", async () => {
  const harness = await settleBurstHarness();
  globalThis.fetch = (path) => {
    assert.equal(path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/schedule");
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay, secondDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  assert.equal(harness.status.textContent, "Scheduled");
  assert.equal(harness.loads(), 1, "the action triggers the immediate refresh");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.loads(), 2, "the first burst tick reloads the route");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");
  // Delays are absolute offsets from the action's refresh; the one-shot
  // Poller re-arms per tick, so the second arm waits out only the delta.
  assert.equal(harness.timers[1].delay, secondDelay - firstDelay);

  await harness.fire(1);
  assert.equal(harness.loads(), 3, "the second burst tick reloads the route");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("navigating away before the burst fires cancels the pending reloads", async () => {
  const harness = await settleBurstHarness("/ui/board");
  await harness.actionRefresh();
  assert.equal(harness.loads(), 1);
  assert.equal(harness.timers.length, 1);

  // Opening another route starts a newer load: the generation bumps and the
  // pathname changes, so the pending burst tick's isActiveLoad guard is stale
  // before the timer fires.
  harness.context.window.location.pathname = "/ui/jobs";
  await harness.app.load();
  assert.equal(harness.loads(), 2);

  await harness.fire(0);
  assert.equal(harness.loads(), 2, "the stale burst tick never reloads the new route");
  assert.equal(harness.timers.length, 1, "the burst ends instead of arming another tick");
});

test("a failed action does not schedule a settle burst", async () => {
  const harness = await settleBurstHarness();
  globalThis.fetch = () => Promise.resolve({
    ok: false,
    json: () => Promise.resolve({ error: { message: "boom" } }),
  });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  assert.equal(harness.status.textContent, "boom");
  assert.equal(harness.loads(), 0, "a failed action never reaches the refresh");
  assert.equal(harness.timers.length, 0, "no settle burst follows a failed action");
});

test("a refresh with no action in flight does not arm the settle burst", async () => {
  const harness = await settleBurstHarness();

  // The board's Done filter change is the ordinary case: it only re-reads
  // the current route and carries no settle provenance, so it must stay the
  // single load it always was.
  await harness.app.refresh();

  assert.equal(harness.loads(), 1, "an ordinary refresh stays a single load");
  assert.equal(harness.timers.length, 0, "no burst timers without the action's provenance");
});

test("an unrelated refresh during a pending action arms no burst, even if the action fails", async () => {
  const harness = await settleBurstHarness();
  const post = deferred();
  globalThis.fetch = () => post.promise;
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const action = handleAction(harness.app, { target: button, preventDefault() {} });

  // The board's Done filter change while the action's POST is still pending:
  // overlapping an in-flight action is not provenance — this refresh carries
  // no token, so it stays a single load whether or not the action succeeds.
  assert.equal(inFlight.size, 1, "the action is still in flight");
  await harness.app.refresh();
  assert.equal(harness.loads(), 1, "the unrelated refresh runs its single load");
  assert.equal(harness.timers.length, 0, "no burst timers without the action's provenance");

  post.resolve({ ok: false, json: () => Promise.resolve({ error: { message: "boom" } }) });
  await action;
  assert.equal(harness.status.textContent, "boom");
  assert.equal(harness.loads(), 1, "the failed action never reaches a refresh");
  assert.equal(harness.timers.length, 0, "the failed action arms no burst either");
});

test("navigating away during the action's immediate refresh cancels the settle burst", async () => {
  const harness = await settleBurstHarness("/ui/board");
  // Hold the action's immediate load in flight so the navigation lands while
  // the refresh is still awaiting it; the load's generation and path were
  // already captured, exactly like the real load.
  const gate = deferred();
  const baseLoad = harness.app.load;
  let holdLoad = true;
  harness.app.load = async () => {
    const loadContext = await baseLoad();
    if (holdLoad) {
      holdLoad = false;
      await gate.promise;
    }
    return loadContext;
  };

  const refresh = harness.actionRefresh();
  // Opening another route starts a newer load: the generation bumps and the
  // pathname changes while the action's refresh is still awaiting its own —
  // now stale — load.
  harness.context.window.location.pathname = "/ui/jobs";
  await harness.app.load();
  gate.resolve();
  await refresh;

  assert.equal(harness.loads(), 2);
  assert.equal(harness.timers.length, 0, "a superseded refresh arms no burst on the new route");
});

test("a burst tick that finds a load in flight skips its reload instead of overlapping", async () => {
  const harness = await settleBurstHarness();
  await harness.actionRefresh();
  assert.equal(harness.loads(), 1);

  harness.app.loadsInFlight = 1;
  await harness.fire(0);
  assert.equal(harness.loads(), 1, "the tick skips rather than overlap the running load");
  assert.equal(harness.timers.length, 2, "the burst still arms its next tick");

  harness.app.loadsInFlight = 0;
  await harness.fire(1);
  assert.equal(harness.loads(), 2, "the next tick reloads once no load is in flight");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

// A console-action harness: a real FlowApp (real load, real settle-burst
// machinery) parked on /ui/console with the project and harness registries
// preloaded, a recording setTimeout, and a fetch stub that answers the
// start/release mutations and serves every console state reload as an
// inactive console. /ui/console has no regular poll (pollConfigForPath) and
// an inactive console schedules no console poll, so every timer recorded
// after the action belongs to the settle burst. The Console view's
// startConsole/releaseConsole handlers reload with app.load() instead of
// refresh(), so these tests pin the provenance stamping of handler-owned
// loads: a successful Console Start or Release must arm the burst exactly
// like a refresh()-based action, while the GET reload a burst tick performs
// (fromPoll, untokened) must not re-arm it.
async function consoleActionHarness() {
  const timers = [];
  const fetches = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "", dataset: {} };
  const context = await scriptContext({
    location: { pathname: "/ui/console", search: "" },
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    URLSearchParams,
    fetch(path, options) {
      fetches.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ active: false, project_id: "p-alpha" }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.projects = [{ id: "p-alpha", name: "Alpha" }];
  app.harnesses = { agents: [], consoles: [{ name: "harness", display_name: "Harness" }] };
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === "[data-console-harness]") return { value: "harness" };
    return null;
  };
  app.querySelectorAll = () => [];
  return {
    app,
    context,
    status,
    timers,
    fetches,
    // consoleGets counts the console state reloads (the GETs), ignoring the
    // start/release mutations themselves.
    consoleGets: () => fetches.filter((call) => call.options.method === "GET").length,
    async fire(index) {
      timers[index].callback();
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

test("a successful console start performs its reload and arms the settle burst", async () => {
  const harness = await consoleActionHarness();
  // The Console view's project-level Start button: data-start-console is
  // empty (no task), the console target lives in data-project/data-task.
  const button = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay, secondDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  const post = harness.fetches.find((call) => call.options.method === "POST");
  assert.equal(post.path, "/ui/api/v2/projects/p-alpha/console");
  assert.deepEqual(JSON.parse(post.options.body), { harness: "harness" });
  assert.equal(harness.status.textContent, "Console starting");
  assert.equal(harness.consoleGets(), 1, "the start performs its immediate reload");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.consoleGets(), 2, "the first burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");
  assert.equal(harness.timers[1].delay, secondDelay - firstDelay);

  await harness.fire(1);
  assert.equal(harness.consoleGets(), 3, "the second burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("a successful console release performs its reload and arms the settle burst", async () => {
  const harness = await consoleActionHarness();
  const button = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  const mutation = harness.fetches.find((call) => call.options.method === "DELETE");
  assert.equal(mutation.path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/console");
  assert.equal(harness.status.textContent, "Console released");
  assert.equal(harness.consoleGets(), 1, "the release performs its immediate reload");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.consoleGets(), 2, "the first burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");

  await harness.fire(1);
  assert.equal(harness.consoleGets(), 3, "the second burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("load tracks in-flight invocations and never arms a settle burst itself", async () => {
  const timers = [];
  const jobsResponse = deferred();
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "", dataset: {} };
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
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

test("board sidebar status separates blocked tasks in compact lifecycle groups", async () => {
  const context = await scriptContext();
  const html = context.renderNavStatus("/ui/board", {
    board: { unscheduled: 2, scheduled: 3, in_progress: 4, blocked: 1 },
  });

  assert.equal((html.match(/class="nav-board-group"/g) || []).length, 2);
  assert.match(html, /data-board-group="queued"/);
  assert.match(html, /data-board-group="active"/);
  // The board badge no longer renders the unscheduled lane even though the
  // sidebar payload still carries it (the Tasks view badge uses it).
  assert.doesNotMatch(html, /data-board-lane="unscheduled"/);
  assert.match(html, /data-board-lane="scheduled" title="3 scheduled tasks">3<\/span>/);
  assert.match(html, /data-board-lane="in_progress" title="4 in progress tasks">4<\/span>/);
  assert.match(html, /data-board-lane="blocked" title="1 blocked task">1<\/span>/);
  assert.match(html, /aria-label="3 scheduled tasks, 4 in progress tasks, 1 blocked task"/);
});

test("tasks sidebar status badge shows the unscheduled count", async () => {
  const context = await scriptContext();
  const html = context.renderNavStatus("/ui/tasks", {
    board: { unscheduled: 2, scheduled: 3, in_progress: 4, blocked: 1 },
  });

  assert.match(html, /title="2 unscheduled tasks">2<\/span>/);
});

test("jobs sidebar status describes each number", async () => {
  const context = await scriptContext();
  const html = context.renderNavStatus("/ui/jobs", {
    jobs: { active: 1, queued: 2 },
  });

  assert.match(html, /data-job-status="active" title="1 active job">1<\/span>/);
  assert.match(html, /data-job-status="queued" title="2 queued jobs">2<\/span>/);
  assert.match(html, /aria-label="1 active job, 2 queued jobs"/);
});

test("sidebar status refresh renders live nav badges and polls", async () => {
  const timers = [];
  const fetchCalls = [];
  const nav = new SmokeNav();
  const refresh = new SmokeElement();
  const newTask = new SmokeElement();

  class SidebarHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === ".nav") return nav;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector === '[data-action="new-task"]') return newTask;
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      if (selector === "[data-theme-option]") return [];
      return [];
    }
  }

  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    HTMLElement: SidebarHTMLElement,
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          done: 8,
          workers: { in_use: 2, capacity: 5 },
          jobs: { active: 6, queued: 7 },
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.sidebarStatusPollingActive = true;
  app.renderShell();

  await app.refreshSidebarStatus();

  assert.deepEqual(fetchCalls, ["/ui/api/v2/sidebar"]);
  assert.match(nav.innerHTML, /title="8 done items">8<\/span>/);
  assert.match(nav.innerHTML, /title="2 in use of 5 worker slots">2\/5<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="active" title="6 active jobs">6<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="queued" title="7 queued jobs">7<\/span>/);
  assert.equal(timers[0].delay, 10000);
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
  const content = { innerHTML: "" };
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
  assert.equal(content.innerHTML, "");
  assert.equal(timers.length, 0);

  newJobs.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [] }),
  });
  await newLoad;
  assert.match(content.innerHTML, /No jobs/);
  assert.equal(timers[0].delay, 30000);
});

async function taskSaveHarness(options = {}) {
  let submitHandler;
  const mode = options.mode || "edit";
  const projectID = options.projectID ?? (mode === "create" ? "p-alpha" : "");
  const form = {
    dataset: { taskForm: mode === "create" ? "" : "t-alpha-0001", taskFormMode: mode },
    elements: {
      title: { value: options.title ?? "Updated task" },
      body: { value: "New body\n\n## Requirements\n- New criteria" },
      priority: { value: options.priority ?? "4" },
      requires_human_review: { checked: false },
      auto_merge: { checked: true },
      flow_id: { value: options.flowID ?? "" },
    },
    reportValidity() {
      return options.valid !== false;
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
    querySelector() {
      return null;
    },
  };
  if (mode === "create") {
    form.elements.project = { value: projectID };
    form.elements.attachments = { files: options.files || [] };
    form.elements.queue_task = { checked: options.queueTask !== false };
  } else if (projectID) {
    form.dataset.project = projectID;
  }
  const status = { textContent: "" };
  const fetchCalls = [];
  const storage = new Map();
  let pushedPath = "";
  let loads = 0;
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/" },
      addEventListener() {},
      localStorage: {
        getItem(key) {
          return storage.has(key) ? storage.get(key) : null;
        },
        setItem(key, value) {
          storage.set(key, String(value));
        },
        removeItem(key) {
          storage.delete(key);
        },
      },
      open() {
        throw new Error("window.open should not be used for task save");
      },
    },
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      return Promise.resolve({
        ok: options.fetchOK !== false,
        json: () => Promise.resolve(options.fetchOK === false
          ? { error: { message: options.errorMessage || "request failed" } }
          : { task: options.responseTask || { id: "t-alpha-0001" } }),
      });
    },
    FormData: class {
      constructor() {
        this.fields = [];
      }
      set(name, value, filename) {
        this.fields.push({ name, value, filename });
      }
    },
    console,
  };
  context.history.pushState = (_state, _title, path) => {
    pushedPath = path;
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  if (options.harnesses) {
    flowApp.harnesses = options.harnesses;
  }
  flowApp.querySelectorAll = (selector) => (selector === "[data-task-form]" ? [form] : []);
  flowApp.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  flowApp.bindTaskActions(async () => {
    refreshed = true;
  });
  flowApp.load = async () => {
    loads += 1;
  };

  return {
    fetchCalls,
    status,
    storage,
    refreshed: () => refreshed,
    pushedPath: () => pushedPath,
    loads: () => loads,
    submit: () => submitHandler({ preventDefault() {} }),
  };
}

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

async function triageEditHarness(options = {}) {
  let clickHandler;
  const button = {
    dataset: { taskEdit: "t-alpha-0001", taskTitle: "Old title" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({
    prompt(message, initial) {
      assert.equal(message, "Title");
      assert.equal(initial, "Old title");
      return options.promptValue;
    },
  }, {
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      return Promise.resolve({
        ok: options.fetchOK !== false,
        json: () => Promise.resolve(options.fetchOK === false
          ? { error: { message: options.errorMessage || "request failed" } }
          : { task: { id: "t-alpha-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-task-edit]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  return {
    fetchCalls,
    status,
    refreshed: () => refreshed,
    click: () => clickHandler(),
  };
}

async function browserSmokeHarness(path, responses) {
  const [pathname, search = ""] = String(path).split("?", 2);
  const title = new SmokeElement();
  const status = new SmokeElement();
  const content = new SmokeElement();
  const refresh = new SmokeElement();
  const nav = new SmokeNav();
  const statusbar = new SmokeElement();
  const sbLabel = new SmokeElement();
  const sbMeta = new SmokeElement();
  statusbar.querySelector = (selector) => (selector === ".sb-label" ? sbLabel : null);
  const diffContainers = new Map();
  const fetchCalls = [];

  class SmokeHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === "h1") return title;
      if (selector === ".status") return status;
      if (selector === ".content") return content;
      if (selector === ".nav") return nav;
      if (selector === ".statusbar") return statusbar;
      if (selector === ".sb-meta") return sbMeta;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector.startsWith("[data-change-diff=")) {
        const id = selector.match(/"([^"]+)"/)?.[1] || selector;
        if (!diffContainers.has(id)) diffContainers.set(id, new SmokeElement());
        return diffContainers.get(id);
      }
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      return [];
    }
  }

  const context = await scriptContext({
    location: { pathname, search: search ? `?${search}` : "" },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  }, {
    HTMLElement: SmokeHTMLElement,
    fetch(requestPath) {
      fetchCalls.push(requestPath);
      if (requestPath === "/ui/api/v2/projects" && !(requestPath in responses)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      if (requestPath === "/ui/api/v2/harnesses" && !(requestPath in responses)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ agents: [], consoles: [] }),
        });
      }
      if (!(requestPath in responses)) {
        return Promise.resolve({
          ok: false,
          json: () => Promise.resolve({ error: { message: `missing smoke response for ${requestPath}` } }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(responses[requestPath]),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderShell();

  return {
    app,
    title,
    status,
    content,
    statusbar,
    sbLabel,
    sbMeta,
    fetchCalls,
    activeNavHref() {
      return nav.links.find((link) => link.attributes.get("aria-current") === "page")?.href || "";
    },
    diffContainer(id) {
      return diffContainers.get(id) || new SmokeElement();
    },
  };
}

class SmokeElement {
  constructor() {
    this.innerHTML = "";
    this.textContent = "";
    this.dataset = {};
    this.attributes = new Map();
    this.listeners = new Map();
  }

  addEventListener(event, handler) {
    this.listeners.set(event, handler);
  }

  setAttribute(name, value) {
    this.attributes.set(name, value);
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  querySelector() {
    return null;
  }

  querySelectorAll() {
    return [];
  }
}

class SmokeNav extends SmokeElement {
  constructor() {
    super();
    this.links = [];
  }

  set innerHTML(html) {
    this._innerHTML = html;
    this.links = [...String(html).matchAll(/href="([^"]+)"/g)].map((match) => new SmokeLink(match[1]));
  }

  get innerHTML() {
    return this._innerHTML || "";
  }

  querySelectorAll(selector) {
    return selector === "a" ? this.links : [];
  }
}

class SmokeLink extends SmokeElement {
  constructor(href) {
    super();
    this.href = href;
  }

  getAttribute(name) {
    return name === "href" ? this.href : "";
  }
}

function inlineDocument() {
  return {
    cookie: "flow_ui_csrf=csrf-token",
    addEventListener() {},
    createElement(tagName) {
      return new InlineDOMElement(tagName);
    },
  };
}

class InlineDOMElement extends SmokeElement {
  constructor(tagName = "div") {
    super();
    this.tagName = String(tagName).toUpperCase();
    this.className = "";
    this.children = [];
    this.parentElement = null;
    this.previousElementSibling = null;
    this.nextElementSibling = null;
    this.cells = [];
    this.colSpan = 0;
  }

  appendChild(child) {
    child.parentElement = this;
    this.children.push(child);
    return child;
  }

  remove() {
    if (this.previousElementSibling) this.previousElementSibling.nextElementSibling = this.nextElementSibling;
    if (this.nextElementSibling) this.nextElementSibling.previousElementSibling = this.previousElementSibling;
    if (this.parentElement?.children) {
      const index = this.parentElement.children.indexOf(this);
      if (index >= 0) this.parentElement.children.splice(index, 1);
    }
    this.parentElement = null;
    this.previousElementSibling = null;
    this.nextElementSibling = null;
  }

  after(element) {
    element.parentElement = this.parentElement;
    element.previousElementSibling = this;
    element.nextElementSibling = this.nextElementSibling;
    this.nextElementSibling = element;
  }

  querySelector(selector) {
    if (selector === "[data-inline-terminal]") return findInlineTerminal(this);
    return null;
  }

  querySelectorAll(selector) {
    if (selector === "td, th") return this.cells;
    return [];
  }
}

class RepaintingInlineDOMElement extends InlineDOMElement {
  set innerHTML(html) {
    this._innerHTML = String(html);
    if (!this.children) return;
    for (const child of this.children) child.parentElement = null;
    this.children = [];
  }

  get innerHTML() {
    return this._innerHTML || "";
  }
}

function findInlineTerminal(element) {
  if (element.dataset?.inlineTerminal === "true") return element;
  for (const child of element.children || []) {
    const match = findInlineTerminal(child);
    if (match) return match;
  }
  return null;
}

async function themeShellHarness(storedTheme = "") {
  const storage = new Map();
  if (storedTheme) storage.set("flow.ui.theme", storedTheme);
  const rootAttributes = new Map();
  const themeButtons = {};
  for (const option of ["system", "light", "dark"]) {
    const button = new SmokeElement();
    button.dataset.themeOption = option;
    themeButtons[option] = button;
  }
  const refresh = new SmokeElement();
  const newTask = new SmokeElement();
  const nav = new SmokeNav();

  class ThemeHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === ".nav") return nav;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector === '[data-action="new-task"]') return newTask;
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      if (selector === "[data-theme-option]") return Object.values(themeButtons);
      return [];
    }
  }

  const context = await scriptContext({
    localStorage: {
      getItem(key) {
        return storage.has(key) ? storage.get(key) : null;
      },
      setItem(key, value) {
        storage.set(key, String(value));
      },
      removeItem(key) {
        storage.delete(key);
      },
    },
  }, {
    HTMLElement: ThemeHTMLElement,
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      documentElement: {
        setAttribute(name, value) {
          rootAttributes.set(name, value);
        },
        removeAttribute(name) {
          rootAttributes.delete(name);
        },
      },
    },
  });

  return {
    app: new context.FlowApp(),
    rootAttributes,
    storage,
    themeButtons,
    pressedThemes() {
      return Object.entries(themeButtons)
        .filter(([, button]) => button.attributes.get("aria-pressed") === "true")
        .map(([option]) => option);
    },
  };
}

class SmokeDetails extends SmokeElement {
  constructor() {
    super();
    this.open = false;
  }
}

// navShellHarness gives the FlowApp stable top-bar mocks (nav dropdown,
// trigger, project picker, theme buttons) so shell/nav dropdown behavior is
// assertable without a real DOM.
async function navShellHarness(pathname = "/ui/board", fetchImpl) {
  const storage = new Map();
  const rootAttributes = new Map();
  const themeButtons = {};
  for (const option of ["system", "light", "dark"]) {
    const button = new SmokeElement();
    button.dataset.themeOption = option;
    themeButtons[option] = button;
  }
  const title = new SmokeElement();
  const status = new SmokeElement();
  const content = new SmokeElement();
  const refresh = new SmokeElement();
  const newTask = new SmokeElement();
  const nav = new SmokeNav();
  const navMenu = new SmokeDetails();
  const trigger = new SmokeElement();
  const picker = new SmokeDetails();
  const pushed = [];

  class NavHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === "h1") return title;
      if (selector === ".status") return status;
      if (selector === ".content") return content;
      if (selector === ".nav") return nav;
      if (selector === ".nav-menu") return navMenu;
      if (selector === ".nav-trigger") return trigger;
      if (selector === ".project-picker") return picker;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector === '[data-action="new-task"]') return newTask;
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      if (selector === "[data-theme-option]") return Object.values(themeButtons);
      return [];
    }
  }

  const context = await scriptContext({
    location: { pathname, search: "" },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
    localStorage: {
      getItem(key) {
        return storage.has(key) ? storage.get(key) : null;
      },
      setItem(key, value) {
        storage.set(key, String(value));
      },
      removeItem(key) {
        storage.delete(key);
      },
    },
  }, {
    HTMLElement: NavHTMLElement,
    history: {
      pushState(_state, _title, path) {
        pushed.push(path);
      },
    },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      documentElement: {
        setAttribute(name, value) {
          rootAttributes.set(name, value);
        },
        removeAttribute(name) {
          rootAttributes.delete(name);
        },
      },
    },
    fetch: fetchImpl || (() => Promise.resolve({
      ok: true,
      json: () => Promise.resolve({}),
    })),
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.sidebarStatusPollingActive = true;
  app.renderShell();

  return { app, context, nav, navMenu, trigger, picker, pushed, storage, themeButtons };
}

let appLoadCount = 0;
function loadAppModule() {
  // Import a fresh entry-module instance per call (cache-busting query) so
  // `class FlowApp extends HTMLElement` re-binds to THIS test's globalThis
  // .HTMLElement — tests like themeShellHarness inject a custom HTMLElement
  // subclass to give the FlowApp instance querySelector/querySelectorAll. The
  // old vm sandbox re-evaluated the source per test; this reproduces that.
  // Pure submodules imported by app.js use unqueried specifiers, so they load
  // once and stay shared.
  appLoadCount += 1;
  return import(`./app.js?test=${appLoadCount}`);
}

// Native-ESM replacement for the old vm sandbox. app.js reads `fetch` as a bare
// global and everything else through `window`/`document`/`customElements`/
// `history`/`HTMLElement`, so install the per-test stubs as real globals, then
// dynamic-import a fresh entry module and copy its exports onto `context` so
// existing `context.X` call-sites keep working. The entry's load-time side
// effects (customElements.define no-op stub, document listeners) re-run per
// import against the current stubs; node:test runs top-level tests sequentially,
// so the per-test global assignment below is race-free.
const CORE_GLOBAL_KEYS = new Set([
  "HTMLElement", "customElements", "document", "history", "window", "fetch",
]);

async function applyContext(context) {
  // Reset the core stubs to the provided value or a safe default on every call,
  // so nothing leaks between sequential tests.
  globalThis.HTMLElement = context.HTMLElement ?? class {};
  globalThis.customElements = context.customElements ?? { define() {} };
  globalThis.document = context.document ?? { cookie: "", addEventListener() {} };
  globalThis.history = context.history ?? { pushState() {} };
  globalThis.window = context.window ?? {};
  globalThis.fetch = context.fetch ?? (() => {
    throw new Error("fetch should not be used");
  });
  // Expose any extra stubs the test supplies (e.g. FormData) as bare globals,
  // matching the old vm sandbox where the whole context object was the global
  // scope. app.js reads these (new FormData(), etc.) off the global.
  for (const [key, value] of Object.entries(context)) {
    if (!CORE_GLOBAL_KEYS.has(key)) globalThis[key] = value;
  }
  Object.assign(context, await loadAppModule());
  return context;
}

async function scriptContext(windowOverrides = {}, contextOverrides = {}) {
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/" },
      addEventListener() {},
      setTimeout() {
        throw new Error("setTimeout should not be used");
      },
      clearTimeout() {},
      open() {
        throw new Error("window.open should not be used");
      },
      ...windowOverrides,
    },
    fetch() {
      throw new Error("fetch should not be used");
    },
    console,
    ...contextOverrides,
  };
  return applyContext(context);
}

test("human attention panel hides the reply form once the agent resumes", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Working" };
  const statusLog = [{ id: 7, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "working" });
  assert.doesNotMatch(html, /data-attention-reply-form/);
  assert.doesNotMatch(html, /Needs Human Response/);
});

test("human attention panel renders the reply form while the session waits", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Waiting" };
  const statusLog = [{ id: 7, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /Needs Human Response/);
  assert.match(html, /which db\?/);
  assert.match(html, /data-attention-reply-form="t-alpha-0001"/);
  assert.match(html, /data-status-log-id="7"/);
});

test("human attention panel renders a waiting question and no longer renders plans", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Plan plus question" };
  const statusLog = [{ id: 9, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  // Plan-mode review is gone; phase gates are handled by renderPhaseGatePanel.
  assert.doesNotMatch(html, /Plan Review/);
  assert.doesNotMatch(html, /data-plan-approve/);
  assert.match(html, /Needs Human Response/);
  assert.match(html, /data-attention-reply-form="t-alpha-0001"/);
  assert.match(html, /data-status-log-id="9"/);
});

test("phaseKey does not map crash_loop", async () => {
  const context = await scriptContext();
  assert.equal(context.phaseKey("crash_loop"), "");
});

function normalize(value) {
  return JSON.parse(JSON.stringify(value));
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

// --- renderMarkdown: block rendering correctness -------------------------------

test("renderMarkdown returns empty string for empty or blank input", async () => {
  const context = await scriptContext();
  assert.equal(context.renderMarkdown(""), "");
  assert.equal(context.renderMarkdown("   \n  \n"), "");
  assert.equal(context.renderMarkdown(null), "");
  assert.equal(context.renderMarkdown(undefined), "");
});

test("renderMarkdown wraps block output in a .md container", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("hello");
  assert.match(html, /^<div class="md">/);
  assert.match(html, /<\/div>$/);
  assert.match(html, /<p>hello<\/p>/);
});

test("renderMarkdown renders ATX headings h1 through h6", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("# Title"), /<h1>Title<\/h1>/);
  assert.match(context.renderMarkdown("## Title"), /<h2>Title<\/h2>/);
  assert.match(context.renderMarkdown("###### Title"), /<h6>Title<\/h6>/);
});

test("renderMarkdown renders bold, italic and bold-italic", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("**bold**"), /<strong>bold<\/strong>/);
  assert.match(context.renderMarkdown("__bold__"), /<strong>bold<\/strong>/);
  assert.match(context.renderMarkdown("*italic*"), /<em>italic<\/em>/);
  assert.match(context.renderMarkdown("_italic_"), /<em>italic<\/em>/);
  assert.match(context.renderMarkdown("***both***"), /<strong><em>both<\/em><\/strong>/);
});

test("renderMarkdown renders strikethrough", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("~~gone~~"), /<del>gone<\/del>/);
});

test("renderMarkdown renders inline code without parsing its contents", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("use `**not bold**` here");
  assert.match(html, /<code>\*\*not bold\*\*<\/code>/);
  assert.doesNotMatch(html, /<strong>/);
});

test("renderMarkdown renders fenced code blocks verbatim", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("```\nline1\n**raw**\n```");
  assert.match(html, /<pre><code>line1\n\*\*raw\*\*\n<\/code><\/pre>/);
  assert.doesNotMatch(html, /<strong>/);
});

test("renderMarkdown renders indented code blocks", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("    indented = code");
  assert.match(html, /<pre><code>indented = code/);
});

test("renderMarkdown renders unordered lists", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("- a\n- b");
  assert.match(html, /<ul>\s*<li>a<\/li>\s*<li>b<\/li>\s*<\/ul>/);
});

test("renderMarkdown renders ordered lists and honors a start", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("1. a\n2. b"), /<ol>\s*<li>a<\/li>\s*<li>b<\/li>\s*<\/ol>/);
  assert.match(context.renderMarkdown("3. a\n4. b"), /<ol start="3">/);
});

test("renderMarkdown renders nested lists", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("- a\n    - nested");
  assert.match(html, /<ul>\s*<li>a\s*<ul>\s*<li>nested<\/li>\s*<\/ul>\s*<\/li>\s*<\/ul>/);
});

test("renderMarkdown renders blockquotes", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("> quoted");
  assert.match(html, /<blockquote>[\s\S]*quoted[\s\S]*<\/blockquote>/);
});

test("renderMarkdown renders horizontal rules", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("---"), /<hr\s*\/?>/);
  assert.match(context.renderMarkdown("***"), /<hr\s*\/?>/);
});

test("renderMarkdown renders links with safe rel and no target", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[flow](https://example.com)");
  assert.match(html, /<a href="https:\/\/example\.com" rel="noopener noreferrer ugc">flow<\/a>/);
  assert.doesNotMatch(html, /target=/);
});

test("renderMarkdown renders angle-bracket autolinks and bare URLs", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("<https://example.com>"), /<a href="https:\/\/example\.com"[^>]*>https:\/\/example\.com<\/a>/);
  assert.match(context.renderMarkdown("see https://example.com now"), /<a href="https:\/\/example\.com"[^>]*>https:\/\/example\.com<\/a>/);
});

test("renderMarkdown renders GFM tables", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("| A | B |\n| --- | --- |\n| 1 | 2 |");
  assert.match(html, /<table>/);
  assert.match(html, /<th>A<\/th>/);
  assert.match(html, /<td>1<\/td>/);
});

test("renderMarkdown renders images with a fixed safe attribute set", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![logo](https://example.com/a.png)");
  assert.match(html, /<img src="https:\/\/example\.com\/a\.png" alt="logo" loading="lazy"\s*\/?>/);
});

test("renderMarkdown preserves soft line breaks inside a paragraph", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("line one\nline two");
  assert.match(html, /line one<br>line two/);
});

test("renderMarkdown separates blank-line-delimited paragraphs", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("para one\n\npara two");
  assert.match(html, /<p>para one<\/p>\s*<p>para two<\/p>/);
});

// --- renderMarkdown: security / XSS -------------------------------------------

test("renderMarkdown escapes raw HTML tags instead of emitting them", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<script>alert(1)</script>");
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
});

test("renderMarkdown does not emit a live img tag from raw HTML", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<img src=x onerror=alert(1)>");
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});

test("renderMarkdown drops javascript: link schemes", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[x](javascript:alert(1))");
  assert.doesNotMatch(html, /href="javascript:/);
  assert.match(html, /x/);
});

test("renderMarkdown drops obfuscated javascript: schemes with embedded whitespace", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[x](java\tscript:alert(1))");
  assert.doesNotMatch(html, /href="java/);
});

test("renderMarkdown drops data: image sources", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![x](data:text/html;base64,PHN2Zz4=)");
  assert.doesNotMatch(html, /src="data:/);
});

test("renderMarkdown escapes content inside code spans that looks like a tag", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("`\"></code><script>`");
  assert.doesNotMatch(html, /<script>/);
});

test("renderMarkdown escapes ampersands and angle brackets in prose", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("a < b & c");
  assert.match(html, /a &lt; b &amp; c/);
});

// --- renderMarkdown: inline mode ---------------------------------------------

test("renderMarkdown inline mode renders inline markup without block elements", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("**done** with `sha`", { inline: true });
  assert.match(html, /<strong>done<\/strong>/);
  assert.match(html, /<code>sha<\/code>/);
  assert.doesNotMatch(html, /<(p|h1|h2|ul|ol|li|pre|blockquote|table|div)[ >]/);
});

test("renderMarkdown inline mode degrades a heading to plain inline text", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("# Title", { inline: true });
  assert.doesNotMatch(html, /<h1>/);
  assert.match(html, /Title/);
});

test("renderMarkdown inline mode degrades images to a link", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![logo](https://example.com/a.png)", { inline: true });
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /<a href="https:\/\/example\.com\/a\.png"/);
});

test("renderMarkdown inline mode still neutralizes XSS", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<script>alert(1)</script>", { inline: true });
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
});

test("renderMarkdown does not overflow the stack on deeply nested blockquotes", async () => {
  const context = await scriptContext();
  assert.doesNotThrow(() => context.renderMarkdown(">".repeat(8000) + " deep"));
});

test("renderMarkdown does not overflow the stack on deeply nested lists", async () => {
  const context = await scriptContext();
  let md = "";
  for (let d = 0; d < 4000; d++) md += " ".repeat(d) + "- item\n";
  assert.doesNotThrow(() => context.renderMarkdown(md));
});

// --- markdown surface integration --------------------------------------------

test("human attention panel renders the question message as markdown", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Q" };
  const statusLog = [{ id: 7, kind: "question", message: "Pick **one**:\n- a\n- b", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /<strong>one<\/strong>/);
  assert.match(html, /<li>a<\/li>/);
});

test("check renders its details as markdown", async () => {
  const context = await scriptContext();
  const html = context.renderCheck({ name: "ci", kind: "test", details: "failed: **boom**" });
  assert.match(html, /class="md"/);
  assert.match(html, /<strong>boom<\/strong>/);
});

test("handoff summary renders its summary as inline markdown", async () => {
  const context = await scriptContext();
  const html = context.renderHandoffSummary({ present: true, valid: true, summary: "shipped `v1`" });
  assert.match(html, /<code>v1<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>/);
});

// --- composable flows: task form, board badge, gate + flows editor ------------

test("task form flow select preselects the project default flow without annotating its name", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];
  app.flowsByProject = new Map([["p-alpha", {
    flows: [
      { id: "fl-basic", name: "basic" },
      { id: "fl-plan", name: "planned" },
    ],
    defaultFlowID: "fl-plan",
  }]]);

  const html = app.renderTaskForm({ title: "", priority: 0 }, { mode: "create", projectID: "p-alpha", submitLabel: "Create" });

  assert.match(html, /<select name="flow_id" data-flow-select>/);
  assert.match(html, /<option value="fl-basic" >basic<\/option>/);
  assert.match(html, /<option value="fl-plan" selected>planned<\/option>/);
  assert.doesNotMatch(html, /\(default\)/);
});

test("task form flow select preselects the task's saved flow when editing", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.flowsByProject = new Map([["p-alpha", {
    flows: [
      { id: "fl-basic", name: "basic" },
      { id: "fl-plan", name: "planned" },
    ],
    defaultFlowID: "fl-plan",
  }]]);

  const html = app.renderTaskForm({ title: "T", flow_id: "fl-basic" }, { taskID: "t-alpha-0001", projectID: "p-alpha" });

  assert.match(html, /<option value="fl-basic" selected>basic<\/option>/);
  assert.match(html, /<option value="fl-plan" >planned<\/option>/);
  assert.doesNotMatch(html, /\(default\)/);
});

test("wait reason phase_approval maps to a human label", async () => {
  const context = await scriptContext();
  assert.equal(context.waitReasonLabel("phase_approval"), "waiting for phase approval");
  assert.doesNotMatch(context.waitReasonLabel("phase_approval"), /plan/);
});

test("flows editor markup opts into shared form styling and accessible row controls", async () => {
  const context = await scriptContext();
  const agentOptions = [{ name: "harness", display_name: "Harness", models: [] }];

  const inheritedDef = { id: "ad-global", name: "shared", harness: "harness", prompt: "Shared prompt", inherited: true };
  const inheritedReadHTML = context.renderAgentDefsSectionView([inheritedDef], agentOptions, { editingDefID: "" });
  assert.match(inheritedReadHTML, /Project Agent Definitions/);
  assert.match(inheritedReadHTML, /badge idle">inherited/);
  assert.match(inheritedReadHTML, /data-edit-def="ad-global">Override/);
  assert.match(inheritedReadHTML, /data-add-def/);
  assert.doesNotMatch(inheritedReadHTML, /data-agent-def-form/);

  const inheritedEditHTML = context.renderAgentDefsSectionView([inheritedDef], agentOptions, { editingDefID: "ad-global" });
  assert.match(inheritedEditHTML, /<form class="agent-def-table-form" data-agent-def-form data-def-id="ad-global">/);
  assert.match(inheritedEditHTML, /name="def_name" value="shared" aria-label="Name" readonly required/);
  assert.match(inheritedEditHTML, /name="def_harness" data-def-harness aria-label="Harness"/);
  assert.match(inheritedEditHTML, /name="def_model" data-def-model aria-label="Model"/);
  assert.match(inheritedEditHTML, /name="def_reasoning_effort" data-def-reasoning aria-label="Effort"/);
  assert.match(inheritedEditHTML, /type="submit">Save<\/button>[\s\S]*data-def-cancel>Cancel<\/button>/);
  assert.match(inheritedEditHTML, /data-agent-def-edit-row>[\s\S]*<\/tr>\s*<tr class="agent-def-prompt-row" data-agent-def-prompt-row>[\s\S]*Shared prompt/);
  assert.doesNotMatch(inheritedEditHTML, /data-delete-def="ad-global"/);

  const globalHTML = context.renderGlobalAgentDefsSectionView([], agentOptions, { editingGlobalDefID: "" });
  assert.match(globalHTML, /Global Agent Definitions/);
  assert.match(globalHTML, /Every project inherits/);
  assert.match(globalHTML, /<tr class="agent-def-add-row">[\s\S]*data-add-def[\s\S]*>\+<\/button>/);
  assert.doesNotMatch(globalHTML, /data-agent-def-form|No agent definitions/);

  const globalCreateHTML = context.renderGlobalAgentDefsSectionView([], agentOptions, { editingGlobalDefID: context.NEW_AGENT_DEF_STATE });
  assert.match(globalCreateHTML, /data-agent-def-form data-def-id=""/);
  assert.match(globalCreateHTML, /name="def_name" value="" aria-label="Name" required/);
  assert.match(globalCreateHTML, /data-agent-def-edit-row>[\s\S]*<\/tr>\s*<tr class="agent-def-prompt-row"/);
  assert.doesNotMatch(globalCreateHTML, /data-add-def/);

  const flowHTML = context.renderFlowEditorView({
    name: "custom",
    start_node: "plan",
    nodes: [{ key: "plan", name: "Plan", kind: "agent" }, { key: "done", name: "Done", kind: "terminal" }],
    edges: [{ from: "plan", outcome: "completed", to: "done" }],
  }, []);
  assert.match(flowHTML, /<form class="flow-editor task-form"/);
  assert.match(flowHTML, /class="flow-row-list wide" data-node-cards/);
  assert.match(flowHTML, /class="flow-row-actions wide"><button[^>]+data-add-node/);
  assert.match(flowHTML, /class="flow-row-list wide" data-edge-rows/);
  assert.match(flowHTML, /class="flow-row-actions wide"><button[^>]+data-add-edge/);
  assert.match(flowHTML, /class="workflow-chart flow-graph-preview" data-graph-preview/);
  assert.match(flowHTML, /<svg[^>]*aria-label="custom workflow definition"/);
  assert.match(flowHTML, /data-node="plan"/);
  assert.match(flowHTML, /data-edge-outcome="completed"/);

  const nodeHTML = context.renderNodeCardView({ key: "plan", name: "Plan", kind: "agent" });
  assert.match(nodeHTML, /class="flow-row flow-node-card" data-node-card/);
  assert.match(nodeHTML, /aria-label="Node key"/);
  assert.match(nodeHTML, /placeholder="Short display name \(e\.g\. Implement\)"/);
  assert.match(nodeHTML, /aria-label="Node name"/);
  assert.match(nodeHTML, /aria-label="Trusted node kind"/);
  assert.match(nodeHTML, /aria-label="Strict node configuration JSON"/);
  assert.match(nodeHTML, /class="flow-row-controls"/);
  assert.match(nodeHTML, /title="Move node up"/);
  assert.match(nodeHTML, /title="Remove node"/);

  const edgeHTML = context.renderEdgeRowView({ from: "plan", outcome: "done", to: "verify" }, ["plan", "verify"]);
  assert.match(edgeHTML, /data-edge-row/);
  assert.match(edgeHTML, /aria-label="From node"/);
  assert.match(edgeHTML, /aria-label="Target node"/);
  assert.match(edgeHTML, /title="Remove transition"/);
});

test("agent definition table actions enter inline edit and create modes", async () => {
  const context = await scriptContext();
  const editListeners = new Map();
  const addListeners = new Map();
  const editButton = {
    dataset: { editDef: "ad-review" },
    addEventListener(type, listener) { editListeners.set(type, listener); },
  };
  const addButton = {
    dataset: {},
    addEventListener(type, listener) { addListeners.set(type, listener); },
  };
  const section = {
    querySelector() {
      return null;
    },
    querySelectorAll(selector) {
      if (selector === "[data-edit-def]") return [editButton];
      if (selector === "[data-add-def]") return [addButton];
      return [];
    },
  };
  let loads = 0;
  const app = {
    querySelector(selector) {
      assert.equal(selector, "[data-agent-defs-section]");
      return section;
    },
    load() { loads += 1; },
  };
  const state = { editingDefID: "" };

  context.bindAgentDefsSectionView(app, { id: "p-alpha" }, [], [], state);
  editListeners.get("click")();
  assert.equal(state.editingDefID, "ad-review");
  addListeners.get("click")();
  assert.equal(state.editingDefID, context.NEW_AGENT_DEF_STATE);
  assert.equal(loads, 2);
});

test("parallel review editors render ordered structured rows without generic JSON", async () => {
  const context = await scriptContext();
  const agentDefs = [
    { id: "ad-code", name: "Code review", harness: "harness", model: "gpt-code" },
    { id: "ad-security", name: "Security review", harness: "harness", model: "opus" },
    { id: "ad-aggregator", name: "Review aggregator", harness: "harness", model: "gpt-mini" },
  ];
  const html = context.renderNodeCardView({
    key: "review",
    name: "Review",
    kind: "change_review",
    config: {
      change_review: {
        agents: [
          { agent_def_id: "ad-code", blocking: false },
          { agent_def_id: "ad-retired" },
        ],
        aggregator_agent_def_id: "ad-aggregator",
      },
    },
  }, agentDefs);

  assert.equal((html.match(/data-review-agent-row(?:\s|>)/g) || []).length, 2);
  assert.ok(html.indexOf('value="ad-code" selected') < html.indexOf('value="ad-retired" selected'));
  assert.match(html, /<option value="ad-code" selected>Code review — harness \/ gpt-code<\/option>/);
  assert.match(html, /<option value="ad-retired" selected>ad-retired \(unavailable\)<\/option>/);
  assert.equal((html.match(/name="review_agent_blocking" checked/g) || []).length, 1, "omitted blocking defaults to checked");
  assert.match(html, /Blocks approval/);
  assert.match(html, /data-review-agent-advisory >Advisory<\/span>/);
  assert.match(html, /Reviewers run in parallel/);
  assert.match(html, /selected final review aggregator/);
  assert.match(html, /<span>Final review aggregator<\/span>/);
  assert.match(html, /name="review_aggregator_agent_def_id" aria-label="Final review aggregator" required/);
  assert.match(html, /<option value="ad-aggregator" selected>Review aggregator — harness \/ gpt-mini<\/option>/);
  assert.match(html, /data-add-review-agent>Add agent/);
  assert.match(html, /title="Move agent up"/);
  assert.match(html, /title="Move agent down"/);
  assert.match(html, /title="Remove agent"/);
  assert.doesNotMatch(html, /name="node_config"|Strict node configuration JSON/);

  const legacy = context.renderReviewAgentRowView({ agent_def_id: "ad-security", required: false }, agentDefs);
  assert.doesNotMatch(legacy, /name="review_agent_blocking" checked/);
  assert.equal(context.reviewAgentBlockingView({ required: true }), true);

  const verifyHTML = context.renderNodeCardView({
    kind: "verify_change",
    config: { verify_change: { agents: [{ agent_def_id: "ad-security", blocking: true }] } },
  }, agentDefs);
  assert.match(verifyHTML, /data-review-config-key="verify_change"/);
  assert.match(verifyHTML, /Blocks success/);
  assert.match(verifyHTML, /Every listed agent runs and is awaited/);
  assert.doesNotMatch(verifyHTML, /review_aggregator_agent_def_id/);
  assert.doesNotMatch(verifyHTML, /name="node_config"|Strict node configuration JSON/);
});

test("parallel review controls add, remove, and reorder agent rows", async () => {
  const context = await scriptContext();
  const listeners = new Map();
  const form = {
    dataset: {},
    addEventListener(event, handler) {
      listeners.set(event, handler);
    },
    querySelector() {
      return null;
    },
    querySelectorAll() {
      return [];
    },
  };
  const section = {
    querySelector(selector) {
      return selector === "[data-flow-editor]" ? form : null;
    },
    querySelectorAll() {
      return [];
    },
  };
  const app = {
    querySelector(selector) {
      return selector === "[data-flows-section]" ? section : null;
    },
    load() {},
    setStatus() {},
  };
  context.bindFlowsSectionView(app, { id: "p-alpha" }, [], [{ id: "ad-code", name: "Code review" }], {});

  let addedHTML = "";
  const rowList = {
    insertAdjacentHTML(position, html) {
      assert.equal(position, "beforeend");
      addedHTML = html;
    },
  };
  const config = {
    querySelector(selector) {
      return selector === "[data-review-agent-rows]" ? rowList : null;
    },
  };
  let prevented = false;
  listeners.get("click")({
    target: {
      closest(selector) {
        if (selector === "[data-add-review-agent]") return this;
        if (selector === "[data-review-agent-config]") return config;
        return null;
      },
    },
    preventDefault() {
      prevented = true;
    },
  });
  assert.equal(prevented, true);
  assert.match(addedHTML, /data-review-agent-row/);
  assert.match(addedHTML, /<option value="ad-code" >Code review<\/option>/);
  assert.match(addedHTML, /name="review_agent_blocking" checked/);

  let removed = false;
  const removableRow = { remove() { removed = true; } };
  listeners.get("click")({
    target: {
      closest(selector) {
        if (selector === "[data-review-agent-remove]") return this;
        if (selector === "[data-review-agent-row]") return removableRow;
        return null;
      },
    },
    preventDefault() {},
  });
  assert.equal(removed, true);

  const first = { id: "first" };
  const second = { id: "second" };
  const parent = {
    children: [first, second],
    insertBefore(node, reference) {
      this.children.splice(this.children.indexOf(node), 1);
      this.children.splice(this.children.indexOf(reference), 0, node);
      relink();
    },
  };
  const relink = () => {
    parent.children.forEach((row, index) => {
      row.parentNode = parent;
      row.previousElementSibling = parent.children[index - 1] || null;
      row.nextElementSibling = parent.children[index + 1] || null;
    });
  };
  relink();
  listeners.get("click")({
    target: {
      closest(selector) {
        if (selector === "[data-review-agent-down]") return this;
        if (selector === "[data-review-agent-row]") return first;
        return null;
      },
    },
    preventDefault() {},
  });
  assert.deepEqual(parent.children.map((row) => row.id), ["second", "first"]);
});

test("switching to either parallel review kind initializes its structured config", async () => {
  const context = await scriptContext();
  const listeners = new Map();
  const editor = { innerHTML: "old config" };
  const card = {
    querySelector(selector) {
      return selector === "[data-node-config-editor]" ? editor : null;
    },
  };
  const form = {
    dataset: {},
    addEventListener(event, handler) {
      listeners.set(event, handler);
    },
    querySelector() {
      return null;
    },
    querySelectorAll() {
      return [];
    },
  };
  const section = {
    querySelector(selector) {
      return selector === "[data-flow-editor]" ? form : null;
    },
    querySelectorAll() {
      return [];
    },
  };
  const app = {
    querySelector(selector) {
      return selector === "[data-flows-section]" ? section : null;
    },
    load() {},
    setStatus() {},
  };
  context.bindFlowsSectionView(app, { id: "p-alpha" }, [], [{ id: "ad-code", name: "Code review" }], {});
  const kindSelect = {
    name: "node_kind",
    value: "change_review",
    closest(selector) {
      return selector === "[data-node-card]" ? card : null;
    },
  };

  listeners.get("change")({ target: kindSelect });
  assert.match(editor.innerHTML, /data-review-config-key="change_review"/);
  assert.equal((editor.innerHTML.match(/data-review-agent-row(?:\s|>)/g) || []).length, 1);
  assert.match(editor.innerHTML, /name="review_agent_def_id"[^>]*required/);
  assert.match(editor.innerHTML, /name="review_aggregator_agent_def_id"[^>]*required/);
  assert.doesNotMatch(editor.innerHTML, /name="node_config"/);

  kindSelect.value = "verify_change";
  listeners.get("change")({ target: kindSelect });
  assert.match(editor.innerHTML, /data-review-config-key="verify_change"/);
  assert.equal((editor.innerHTML.match(/data-review-agent-row(?:\s|>)/g) || []).length, 1);
  assert.match(editor.innerHTML, /Blocks success/);
  assert.doesNotMatch(editor.innerHTML, /review_aggregator_agent_def_id/);
  assert.doesNotMatch(editor.innerHTML, /name="node_config"/);
});

function fakeFieldForm(fields) {
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (match && match[1] in fields) return { value: fields[match[1]] };
      return null;
    },
    querySelectorAll() {
      return [];
    },
  };
}

function fakeFlowRow(fields) {
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (!match) return null;
      const name = match[1];
      if (!(name in fields)) return null;
      if (name === "review_agent_blocking" || name === "review_required") {
        return typeof fields[name] === "object" ? fields[name] : { checked: fields[name] };
      }
      return { value: fields[name] };
    },
    querySelectorAll(selector) {
      if (selector === "[data-review-agent-row]") return (fields.review_agents || []).map(fakeFlowRow);
      return [];
    },
  };
}

function fakeFlowEditor(spec) {
  const top = {
    flow_name: spec.flow_name,
    flow_description: spec.flow_description,
    start_node: spec.start_node ?? "",
    transition_budget: spec.transition_budget ?? "50",
  };
  const nodeCards = (spec.nodes || []).map(fakeFlowRow);
  const edgeRows = (spec.edges || []).map(fakeFlowRow);
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (match && match[1] in top) return { value: top[match[1]] };
      return null;
    },
    querySelectorAll(selector) {
      if (selector === "[data-node-card]") return nodeCards;
      if (selector === "[data-edge-row]") return edgeRows;
      return [];
    },
  };
}

test("agent def form payload stores plain harness target id and effort strings", async () => {
  const context = await scriptContext();
  const agentOptions = [{
    name: "harness",
    display_name: "Harness",
    models: [{
      provider_id: "anthropic",
      provider_name: "Anthropic",
      model_id: "claude-opus-4-8",
      qualified_id: "anthropic:claude-opus-4-8",
      target_id: "anthropic:claude-opus-4-8",
      model_name: "Claude Opus 4.8",
      reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
    }],
  }];
  const form = fakeFieldForm({
    def_name: "Reviewer",
    def_harness: "harness",
    def_model: "anthropic:claude-opus-4-8",
    def_reasoning_effort: "high",
    def_prompt: "review carefully",
  });

  const payload = context.agentDefPayloadFromFormView(form, agentOptions);

  assert.deepEqual(payload, {
    name: "Reviewer",
    harness: "harness",
    model: "anthropic:claude-opus-4-8",
    reasoning_effort: "high",
    prompt: "review carefully",
  });
});

test("agent def form payload stores the bare model id when the catalog model has no target id", async () => {
  const context = await scriptContext();
  const agentOptions = [{
    name: "harness",
    display_name: "Harness",
    models: [{ provider_id: "anthropic", model_id: "sonnet", qualified_id: "anthropic:sonnet", reasoning: false }],
  }];
  const form = fakeFieldForm({
    def_name: "Author",
    def_harness: "harness",
    def_model: "anthropic:sonnet",
    def_reasoning_effort: "",
    def_prompt: "",
  });

  const payload = context.agentDefPayloadFromFormView(form, agentOptions);

  assert.equal(payload.model, "anthropic:sonnet");
  assert.equal(payload.reasoning_effort, "");
});

test("flow editor payload keeps node and edge rows in document order", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Custom",
    flow_description: "two nodes",
    start_node: "plan",
    transition_budget: "50",
    nodes: [
      { node_key: "plan", node_name: "Plan", node_kind: "agent", node_config: '{"agent_def_id":"ad-plan"}' },
      { node_key: "verify", node_name: "Verify", node_kind: "automated_checks", node_config: "{}" },
    ],
    edges: [
      { edge_from: "plan", edge_outcome: "done", edge_to: "verify" },
      { edge_from: "verify", edge_outcome: "pass", edge_to: "plan" },
    ],
  });

  const payload = context.flowPayloadFromEditorView(form);

  assert.deepEqual(payload, {
    name: "Custom",
    description: "two nodes",
    start_node: "plan",
    transition_budget: 50,
    nodes: [
      { key: "plan", name: "Plan", kind: "agent", config: { agent_def_id: "ad-plan" } },
      { key: "verify", name: "Verify", kind: "automated_checks", config: {} },
    ],
    edges: [
      { from: "plan", outcome: "done", to: "verify" },
      { from: "verify", outcome: "pass", to: "plan" },
    ],
  });
});

test("flow editor payload reads each node and edge row as authored", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Sparse",
    flow_description: "",
    start_node: "",
    transition_budget: "50",
    nodes: [
      { node_key: "plan", node_name: "Plan", node_kind: "agent", node_config: "{}" },
      { node_key: "", node_name: "", node_kind: "agent", node_config: "{}" },
    ],
    edges: [
      { edge_from: "plan", edge_outcome: "done", edge_to: "" },
    ],
  });

  const payload = context.flowPayloadFromEditorView(form);

  // Rows are submitted as authored; the editor does not drop blank rows.
  assert.deepEqual(payload.nodes, [
    { key: "plan", name: "Plan", kind: "agent", config: {} },
    { key: "", name: "", kind: "agent", config: {} },
  ]);
  assert.deepEqual(payload.edges, [{ from: "plan", outcome: "done", to: "" }]);
});

test("parallel review payload preserves agent order and emits canonical blocking only", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Parallel review",
    flow_description: "",
    start_node: "review",
    nodes: [
      {
        node_key: "review",
        node_name: "Review",
        node_kind: "change_review",
        review_agents: [
          { review_agent_def_id: "ad-code", review_agent_blocking: true, review_required: false },
          { review_agent_def_id: "ad-security", review_agent_blocking: false, review_required: true },
        ],
        review_aggregator_agent_def_id: "ad-aggregator",
      },
      {
        node_key: "verify",
        node_name: "Verify",
        node_kind: "verify_change",
        review_agents: [
          { review_agent_def_id: "ad-verifier", review_agent_blocking: true },
        ],
      },
    ],
    edges: [],
  });

  const payload = context.flowPayloadFromEditorView(form);

  assert.deepEqual(payload.nodes, [
    {
      key: "review",
      name: "Review",
      kind: "change_review",
      config: {
        change_review: {
          agents: [
            { agent_def_id: "ad-code", blocking: true },
            { agent_def_id: "ad-security", blocking: false },
          ],
          aggregator_agent_def_id: "ad-aggregator",
        },
      },
    },
    {
      key: "verify",
      name: "Verify",
      kind: "verify_change",
      config: {
        verify_change: {
          agents: [{ agent_def_id: "ad-verifier", blocking: true }],
        },
      },
    },
  ]);
  assert.doesNotMatch(JSON.stringify(payload), /"required"/);
});

test("parallel review blocking checkbox toggles an agent to advisory", async () => {
  const context = await scriptContext();
  const blocking = { checked: true };
  const form = fakeFlowEditor({
    flow_name: "Advisory review",
    start_node: "review",
    nodes: [{
      node_key: "review",
      node_name: "Review",
      node_kind: "change_review",
      review_agents: [{ review_agent_def_id: "ad-security", review_agent_blocking: blocking }],
      review_aggregator_agent_def_id: "ad-aggregator",
    }],
  });

  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, true);
  blocking.checked = false;
  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, false);
});

test("cloneFlowView builds a create payload that copies the graph under a new name", async () => {
  const context = await scriptContext();

  const payload = context.cloneFlowView({
    id: "fl-1",
    name: "coding",
    description: "Ship it",
    start_node: "implement",
    transition_budget: 75,
    builtin: true,
    default: true,
    nodes: [
      { id: "fn-1", key: "implement", name: "Implement", kind: "agent", position: 0, config: { agent: { agent_def_id: "ad-author", workspace: "change", artifact: "change" } } },
      { id: "fn-2", key: "review", name: "Review", kind: "change_review", position: 1, config: { change_review: { agents: [{ agent_def_id: "ad-reviewer", blocking: true }], aggregator_agent_def_id: "ad-aggregator" } } },
    ],
    edges: [{ from: "implement", outcome: "done", to: "review" }],
  });

  assert.equal(payload.name, "coding (copy)");
  // The initial copy name is reused when it is available, and an incremented
  // deterministic suffix is chosen when the project already has the copy.
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "coding (copy)"]).name, "coding (copy 2)");
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "coding (copy)", "coding (copy 2)"]).name, "coding (copy 3)");
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "other"]).name, "coding (copy)");
  assert.equal(payload.description, "Ship it");
  assert.equal(payload.start_node, "implement");
  assert.equal(payload.transition_budget, 75);
  // Server-assigned ids/positions and the builtin/default flags are dropped so
  // the copy is a fresh, editable flow.
  assert.doesNotMatch(JSON.stringify(payload), /"id"|"position"|builtin|default/);
  assert.deepEqual(payload.nodes, [
    { key: "implement", name: "Implement", kind: "agent", config: { agent: { agent_def_id: "ad-author", workspace: "change", artifact: "change" } } },
    { key: "review", name: "Review", kind: "change_review", config: { change_review: { agents: [{ agent_def_id: "ad-reviewer", blocking: true }], aggregator_agent_def_id: "ad-aggregator" } } },
  ]);
  assert.deepEqual(payload.edges, [{ from: "implement", outcome: "done", to: "review" }]);
});

test("clone flow button posts a copied create payload and opens the new flow editor", async () => {
  const context = await scriptContext();
  const listeners = new Map();
  const cloneButton = {
    dataset: { cloneFlow: "fl-1" },
    addEventListener(event, handler) {
      listeners.set(event, handler);
    },
  };
  const section = {
    querySelector() {
      return null; // no inline editor form open
    },
    querySelectorAll(selector) {
      return selector === "[data-clone-flow]" ? [cloneButton] : [];
    },
  };
  let reloaded = 0;
  const statuses = [];
  const app = {
    querySelector(selector) {
      return selector === "[data-flows-section]" ? section : null;
    },
    load() {
      reloaded += 1;
    },
    setStatus(message) {
      statuses.push(message);
    },
  };

  const fetchCalls = [];
  globalThis.fetch = (path, options) => {
    fetchCalls.push({ path, options });
    return Promise.resolve({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ flow: { id: "fl-new" } }),
    });
  };

  const state = { editingFlowID: "" };
  const flows = [{
    id: "fl-1",
    name: "coding",
    start_node: "implement",
    nodes: [{ id: "fn-1", key: "implement", name: "Implement", kind: "agent", position: 0, config: { agent: { agent_def_id: "ad-author" } } }],
    edges: [{ from: "implement", outcome: "done", to: "review" }],
  }];
  context.bindFlowsSectionView(app, { id: "p-alpha" }, flows, [], state);

  assert.ok(listeners.has("click"), "clone button binds a click handler");
  await listeners.get("click")();

  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/flows");
  assert.equal(fetchCalls[0].options.method, "POST");
  const body = JSON.parse(fetchCalls[0].options.body);
  assert.equal(body.name, "coding (copy)");
  assert.deepEqual(body.nodes, [{ key: "implement", name: "Implement", kind: "agent", config: { agent: { agent_def_id: "ad-author" } } }]);
  assert.deepEqual(body.edges, [{ from: "implement", outcome: "done", to: "review" }]);
  // The created flow's id is unwrapped from the {flow} envelope and opened for editing.
  assert.equal(state.editingFlowID, "fl-new");
  assert.equal(reloaded, 1);
  assert.deepEqual(statuses, ["flow cloned; rename and edit your copy"]);
});

test("clone flow button posts an incremented copy name when the copy already exists", async () => {
  const context = await scriptContext();
  const listeners = new Map();
  const cloneButton = {
    dataset: { cloneFlow: "fl-1" },
    addEventListener(event, handler) {
      listeners.set(event, handler);
    },
  };
  const section = {
    querySelector() {
      return null; // no inline editor form open
    },
    querySelectorAll(selector) {
      return selector === "[data-clone-flow]" ? [cloneButton] : [];
    },
  };
  let reloaded = 0;
  const app = {
    querySelector(selector) {
      return selector === "[data-flows-section]" ? section : null;
    },
    load() {
      reloaded += 1;
    },
    setStatus() {},
  };

  const fetchCalls = [];
  globalThis.fetch = (path, options) => {
    fetchCalls.push({ path, options });
    return Promise.resolve({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ flow: { id: "fl-new" } }),
    });
  };

  const state = { editingFlowID: "" };
  // The project already contains the first clone, so the repeated clone must
  // submit the next available suffix instead of colliding on the name.
  const flows = [
    { id: "fl-1", name: "coding", nodes: [], edges: [] },
    { id: "fl-2", name: "coding (copy)", nodes: [], edges: [] },
  ];
  context.bindFlowsSectionView(app, { id: "p-alpha" }, flows, [], state);

  await listeners.get("click")();

  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/flows");
  assert.equal(fetchCalls[0].options.method, "POST");
  const body = JSON.parse(fetchCalls[0].options.body);
  assert.equal(body.name, "coding (copy 2)");
  // The created flow still opens in the inline editor.
  assert.equal(state.editingFlowID, "fl-new");
  assert.equal(reloaded, 1);
});

test("flows view renders agent definitions and flow tables for the active project", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }] },
    "/ui/api/v2/harnesses": { agents: [{ name: "harness", display_name: "Harness" }], consoles: [] },
    "/ui/api/v2/global/agent-defs": { agent_defs: [{ id: "ad-global", name: "organization-reviewer", harness: "harness" }] },
    "/ui/api/v2/projects/p-alpha/agent-defs": {
      agent_defs: [{ id: "ad-1", name: "author", harness: "harness", model: "anthropic:opus", reasoning_effort: "high", builtin: true }],
    },
    "/ui/api/v2/projects/p-alpha/flows": {
      flows: [{
        id: "fl-1",
        name: "default flow",
        default: true,
        start_node: "plan",
        nodes: [{ key: "plan", name: "Plan", kind: "agent" }, { key: "implement", name: "Implement", kind: "agent" }],
        edges: [{ from: "plan", outcome: "done", to: "implement" }],
      }],
      default_flow_id: "fl-1",
    },
  });

  await harness.app.load();

  const html = harness.content.innerHTML;
  assert.equal(harness.title.textContent, "Flows");
  assert.match(html, /Global Agent Definitions/);
  assert.match(html, /organization-reviewer/);
  assert.match(html, /Project Agent Definitions/);
  assert.match(html, /author/);
  assert.match(html, /builtin/);
  assert.equal((html.match(/data-add-def/g) || []).length, 2);
  assert.doesNotMatch(html, /data-agent-def-form/);
  assert.match(html, /default flow/);
  assert.match(html, /start: plan · plan\.done → implement/);
  assert.match(html, /class="flows-table"/);
  assert.match(html, /<th class="flow-name-column">Name<\/th><th class="flow-graph-column">Graph<\/th>/);
  assert.match(html, /<td class="flow-name-column">default flow/);
  assert.match(html, /<td class="flow-graph-column"><div class="workflow-chart compact">/);
  assert.match(html, /class="workflow-chart compact"/);
  assert.match(html, /<svg[^>]*aria-label="default flow workflow definition"/);
  assert.match(html, /data-node="implement"/);
  assert.match(html, /data-flow-editor/);
  // Every flow row offers a Clone action to seed a custom copy.
  assert.match(html, /data-clone-flow="fl-1">Clone</);
  // Keeps the project's flow cache warm for the task form.
  assert.deepEqual(harness.app.flowsByProject.get("p-alpha").defaultFlowID, "fl-1");
});

test("flows view offers a project chooser when several projects are active", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
  });
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  assert.match(harness.content.innerHTML, /Select Project/);
  assert.equal((harness.content.innerHTML.match(/class="project-choice"/g) || []).length, 2);
  assert.match(harness.content.innerHTML, /\/ui\/flows\?project=p-alpha/);
  assert.match(harness.content.innerHTML, /\/ui\/flows\?project=p-beta/);
  assert.doesNotMatch(harness.content.innerHTML, /<span>p-alpha<\/span>/);
  assert.doesNotMatch(harness.content.innerHTML, /<span>p-beta<\/span>/);
});

test("flows route refreshes a stale project registry before choosing a project", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
  });
  harness.app.projects = [{ id: "p-alpha", name: "alpha" }];
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  assert.match(harness.content.innerHTML, /Select Project/);
  assert.equal((harness.content.innerHTML.match(/class="project-choice"/g) || []).length, 2);
  assert.deepEqual(harness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/harnesses",
  ]);
});

test("flows view renders the active project name as a project switcher", async () => {
  const harness = await browserSmokeHarness("/ui/flows?project=p-beta", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
    "/ui/api/v2/global/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-beta/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-beta/flows": { flows: [], default_flow_id: "" },
  });
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  const html = harness.content.innerHTML;
  assert.match(html, /class="project-switcher"/);
  assert.match(html, /<summary aria-label="Switch project">beta<\/summary>/);
  assert.match(html, /\/ui\/flows\?project=p-alpha/);
  assert.match(html, /\/ui\/flows\?project=p-beta/);
  assert.match(html, /aria-current="page"/);
  assert.deepEqual(harness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/harnesses",
    "/ui/api/v2/global/agent-defs",
    "/ui/api/v2/projects/p-beta/agent-defs",
    "/ui/api/v2/projects/p-beta/flows",
  ]);
});
