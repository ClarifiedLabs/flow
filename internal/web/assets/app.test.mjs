import assert from "node:assert/strict";
import { test } from "node:test";

const DIFF_MODE_STORAGE_KEY = "flow.ui.diffMode";

test("terminal click from a card opens a modal-sized frame", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v2/sessions/s-0001/terminal-login?token=abc" } }),
      });
    },
  });

  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindTaskActions(async () => {});
  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 1);
  assert.equal(content.children[0].className, "terminal-modal-layer");
  assert.equal(content.children[0].dataset.terminalModalLayer, "true");
  assert.match(content.children[0].innerHTML, /class="terminal-modal"/);
  assert.match(content.children[0].innerHTML, /Session terminal/);
  assert.match(content.children[0].innerHTML, /class="terminal-frame"/);
  assert.match(content.children[0].innerHTML, /src="\/v2\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /data-terminal-popout="\/v2\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /Pop out/);
  assert.match(content.children[0].innerHTML, /Drag to select \(auto-copies\) · Shift\+drag for manual selection/);
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v2/sessions/s-0001/terminal-token");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
});

test("board polling keeps an open card terminal modal mounted", async () => {
  const main = new InlineDOMElement("main");
  main.className = "main";
  const content = new RepaintingInlineDOMElement("section");
  content.className = "content";
  main.appendChild(content);
  const terminalHost = new InlineDOMElement("article");
  terminalHost.className = "card";
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const title = { textContent: "" };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({
    location: { pathname: "/ui/board" },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/sessions/s-0001/terminal-token") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ access: { login_path: "/v2/sessions/s-0001/terminal-login?token=abc" } }),
        });
      }
      if (path === "/ui/api/v2/board") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            boards: [{
              project_id: "p-alpha",
              project_name: "alpha",
              board: {},
              lane_states: {},
              task_cards: {},
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: false,
        json: () => Promise.resolve({ error: { message: `unexpected request ${path}` } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.projects = [];
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".main") return main;
    if (selector === ".content") return content;
    if (selector === ".status") return status;
    if (selector === "h1") return title;
    return null;
  };

  await app.openInlineTerminal(terminalButton, "session", "s-0001");
  const modal = main.children.find((child) => child.dataset?.terminalModalLayer === "true");
  assert.ok(modal);
  assert.match(modal.innerHTML, /class="terminal-frame"/);
  assert.match(modal.innerHTML, /Drag to select \(auto-copies\) · Shift\+drag for manual selection/);

  await app.load({ fromPoll: true });

  assert.equal(main.children.includes(modal), true);
  assert.match(modal.innerHTML, /src="\/v2\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.innerHTML, /class="board"/);
  assert.deepEqual(fetchCalls.map((call) => call.path), [
    "/ui/api/v2/sessions/s-0001/terminal-token",
    "/ui/api/v2/board",
    "/ui/api/v2/done?limit=20",
  ]);
});

test("terminal click closes an open card terminal modal without refreshing the token", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v2/sessions/s-0001/terminal-login?token=abc" } }),
      });
    },
  });

  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindTaskActions(async () => {});
  await terminalButton.listeners.get("click")();
  assert.equal(content.children.length, 1);

  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 0);
  assert.equal(fetchCalls.length, 1);
  assert.equal(status.textContent, "");
});

test("job terminal click from a card opens a modal-sized frame", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.jobTerminal = "j-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v2/jobs/j-0001/terminal-login?token=abc" } }),
      });
    },
  });
  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-job-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindTaskActions(async () => {});
  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 1);
  assert.match(content.children[0].innerHTML, /Job terminal/);
  assert.match(content.children[0].innerHTML, /class="terminal-frame"/);
  assert.match(content.children[0].innerHTML, /src="\/v2\/jobs\/j-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /data-terminal-popout="\/v2\/jobs\/j-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /Pop out/);
  assert.match(content.children[0].innerHTML, /Drag to select \(auto-copies\) · Shift\+drag for manual selection/);
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v2/jobs/j-0001/terminal-token");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
});

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
              { name: "claude", display_name: "Claude" },
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
  assert.match(content.innerHTML, /<option value="claude" selected>Claude<\/option>/);
  assert.doesNotMatch(content.innerHTML, /<option value="codex">Codex<\/option>/);
  assert.match(content.innerHTML, /<option value="harness">Harness<\/option>/);
  assert.match(content.innerHTML, /<option value="shell">Shell<\/option>/);

  await app.startConsole("p-alpha", "shell");
  const post = fetchCalls.find((call) => call.path === "/ui/api/v2/projects/p-alpha/console" && call.options.method === "POST");
  assert.equal(post.options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(JSON.parse(post.options.body).harness, "shell");
  assert.equal(loads, 1);
  assert.equal(status.textContent, "console starting");
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

  assert.match(harness.app.innerHTML, /<p class="brand">flow<span class="brand-cursor">_<\/span><\/p>/);
  assert.match(harness.app.innerHTML, /<button class="button" data-action="new-task">New Task<\/button>/);
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

test("task save submits patch payload and refreshes", async () => {
  const harness = await taskSaveHarness({ flowID: "fl-review" });
  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v2/tasks/t-alpha-0001");
  assert.equal(harness.fetchCalls[0].options.method, "PATCH");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Updated task",
    body: "New body\n\n## Requirements\n- New criteria",
    priority: 4,
    flow_id: "fl-review",
  });
  assert.equal(harness.refreshed(), true);
  assert.equal(harness.status.textContent, "");
});

// Legacy Harness budget/toggle reasoning flags are no longer valid with
// harness v0.0.19. The form treats them as managed stale selection args so a
// later save does not keep emitting them.
test("task save does not submit invalid form or priority", async () => {
  const invalidForm = await taskSaveHarness({ valid: false });
  await invalidForm.submit();
  assert.equal(invalidForm.fetchCalls.length, 0);
  assert.equal(invalidForm.refreshed(), false);

  const invalidPriority = await taskSaveHarness({ priority: "4.5" });
  await invalidPriority.submit();
  assert.equal(invalidPriority.fetchCalls.length, 0);
  assert.equal(invalidPriority.refreshed(), false);
  assert.equal(invalidPriority.status.textContent, "Priority must be a non-negative integer");
});

test("task save surfaces patch failures", async () => {
  const harness = await taskSaveHarness({ fetchOK: false, errorMessage: "task title is required" });
  await harness.submit();

  assert.equal(harness.fetchCalls.length, 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "task title is required");
});

test("task attachment form uploads stage file and refreshes", async () => {
  let submitHandler;
  const file = { name: "review.png" };
  const form = {
    dataset: { task: "t-alpha-0001", project: "p-alpha" },
    elements: {
      stage: { value: "reviewer" },
      file: { files: [file] },
    },
    resetCalled: false,
    reportValidity() {
      return true;
    },
    reset() {
      this.resetCalled = true;
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ attachment: { id: "att-0001" } }),
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
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-attachment-form]" ? [form] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/attachments");
  assert.deepEqual(fetchCalls[0].options.body.fields, [
    { name: "stage", value: "reviewer", filename: undefined },
    { name: "file", value: file, filename: "review.png" },
  ]);
  assert.equal(form.resetCalled, true);
  assert.equal(refreshed, true);
});

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
  assert.match(content.innerHTML, /<option value="fl-coding" selected>coding \(default\)<\/option>/);
  assert.match(content.innerHTML, /<option value="fl-planning" >planning<\/option>/);
  assert.doesNotMatch(content.innerHTML, /Project default/);
  assert.match(content.innerHTML, /<input name="queue_task" type="checkbox" checked>/);
  assert.match(content.innerHTML, /<button class="button" type="submit">Create<\/button>/);
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

test("new task form submits queued create payload then navigates to created task", async () => {
  const harness = await taskSaveHarness({ mode: "create", title: "  Browser task  ", flowID: "fl-plan" });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.equal(harness.fetchCalls[0].options.method, "POST");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Browser task",
    body: "New body\n\n## Requirements\n- New criteria",
    priority: 4,
    flow_id: "fl-plan",
  });
  // Queueing schedules the created task with a second call.
  assert.equal(harness.fetchCalls[1].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/schedule");
  assert.equal(harness.fetchCalls[1].options.method, "POST");
  assert.equal(harness.pushedPath(), "/ui/tasks/t-alpha-0001");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "");
});

test("new task form uploads selected initial attachments after create", async () => {
  const file = { name: "screenshot.png" };
  const harness = await taskSaveHarness({ mode: "create", files: [file] });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.equal(harness.fetchCalls[1].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/attachments");
  const body = harness.fetchCalls[1].options.body;
  assert.deepEqual(body.fields, [
    { name: "stage", value: "initial", filename: undefined },
    { name: "file", value: file, filename: "screenshot.png" },
  ]);
  assert.equal(harness.loads(), 1);
});

test("new task form can save without queueing", async () => {
  const harness = await taskSaveHarness({ mode: "create", queueTask: false });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Updated task",
    body: "New body\n\n## Requirements\n- New criteria",
    priority: 4,
    flow_id: "",
  });
  // Saving without queueing creates the task only; no schedule call follows.
  assert.equal(harness.fetchCalls.length, 1);
  assert.equal(harness.pushedPath(), "/ui/tasks/t-alpha-0001");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "");
});

test("new task form surfaces create failures and missing created task id", async () => {
  const missingProject = await taskSaveHarness({ mode: "create", projectID: "" });
  await missingProject.submit();

  assert.equal(missingProject.fetchCalls.length, 0);
  assert.equal(missingProject.pushedPath(), "");
  assert.equal(missingProject.loads(), 0);
  assert.equal(missingProject.status.textContent, "Project is required");

  const failedCreate = await taskSaveHarness({ mode: "create", fetchOK: false, errorMessage: "task title is required" });
  await failedCreate.submit();

  assert.equal(failedCreate.fetchCalls.length, 1);
  assert.equal(failedCreate.pushedPath(), "");
  assert.equal(failedCreate.loads(), 0);
  assert.equal(failedCreate.status.textContent, "task title is required");

  const missingID = await taskSaveHarness({ mode: "create", responseTask: {} });
  await missingID.submit();

  assert.equal(missingID.fetchCalls.length, 1);
  assert.equal(missingID.pushedPath(), "");
  assert.equal(missingID.loads(), 0);
  assert.equal(missingID.status.textContent, "Created task ID unavailable");
});

test("review run action posts to task review endpoint and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { reviewRun: "t-alpha-0001", project: "p-alpha" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ review_state: "in_review" }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-review-run]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/review/run");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("triage edit action patches task title and refreshes", async () => {
  const harness = await triageEditHarness({ promptValue: "  New triage title  " });

  await harness.click();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v2/tasks/t-alpha-0001");
  assert.equal(harness.fetchCalls[0].options.method, "PATCH");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), { title: "New triage title" });
  assert.equal(harness.refreshed(), true);
  assert.equal(harness.status.textContent, "");
});

test("triage edit action ignores cancel and blank titles", async () => {
  const cancelled = await triageEditHarness({ promptValue: null });
  await cancelled.click();
  assert.equal(cancelled.fetchCalls.length, 0);
  assert.equal(cancelled.refreshed(), false);
  assert.equal(cancelled.status.textContent, "");

  const blank = await triageEditHarness({ promptValue: "   " });
  await blank.click();
  assert.equal(blank.fetchCalls.length, 0);
  assert.equal(blank.refreshed(), false);
  assert.equal(blank.status.textContent, "Task title is required");
});

test("triage edit action surfaces patch failures", async () => {
  const harness = await triageEditHarness({
    promptValue: "Updated",
    fetchOK: false,
    errorMessage: "task title is required",
  });

  await harness.click();

  assert.equal(harness.fetchCalls.length, 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "task title is required");
});

test("change detail fetches read model and renders checks and threads", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const diffContainer = { innerHTML: "" };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/changes/ch-0001" },
      addEventListener() {},
      open() {
        throw new Error("window.open should not be used for change detail");
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v2/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "1234567890abcdef",
            available: true,
            total_files: 1,
            additions: 3,
            deletions: 1,
            files: [{
              path: "app.go",
              additions: 3,
              deletions: 1,
              hunks: [{
                header: "@@ -1,2 +1,4 @@",
                lines: [
                  { kind: "context", old_line: 1, new_line: 1, text: "package app" },
                  { kind: "delete", old_line: 2, text: "const Old = 1" },
                  { kind: "meta", text: "\\ No newline at end of file" },
                  { kind: "add", new_line: 2, text: "const New = 1" },
                ],
              }],
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change: {
            id: "ch-0001",
            task_id: "t-alpha-0001",
            branch: "task/t-alpha-0001",
            base: "main",
            head_sha: "1234567890abcdef",
            updated_at: "2026-06-07T12:00:00Z",
            ready_at: "2026-06-07T12:01:00Z",
          },
          task: { id: "t-alpha-0001", title: "Ship web UI" },
          review_state: "approved",
          required_checks: { total: 1, satisfied: 1 },
          checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied", details: "ok" }],
          threads: [{
            id: "th-0001",
            state: "open",
            anchor_commit_sha: "9876543210abcdef",
            file_path: "app.go",
            line: 4,
            context: "const Value = 1",
            comments: [
              { actor: "reviewer", body: "Review note", created_at: "2026-06-07T12:02:00Z" },
              { actor: "author", body: "I will address it.", created_at: "2026-06-07T12:03:00Z" },
            ],
          }],
          can_merge: true,
        }),
      });
    },
    console,
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    if (selector === '[data-change-diff="ch-0001"]') return diffContainer;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderChange("ch-0001");

  assert.equal(fetchCalls[0].path, "/ui/api/v2/changes/ch-0001");
  assert.equal(fetchCalls[1].path, "/ui/api/v2/changes/ch-0001/diff");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(title.textContent, "Change");
  assert.match(content.innerHTML, /ch-0001/);
  assert.match(content.innerHTML, /Ship web UI/);
  // Merging is driven by the workflow engine; the change page no longer
  // renders a manual merge button.
  assert.doesNotMatch(content.innerHTML, /data-merge-change=/);
  assert.match(diffContainer.innerHTML, /files 1/);
  assert.match(diffContainer.innerHTML, /app.go/);
  assert.match(diffContainer.innerHTML, /<pre class="diff-unified" style="--diff-unified-width: 27ch;">/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-hunk-header">@@ -1,2 \+1,4 @@/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-del">-const Old = 1/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-meta">\\ No newline at end of file/);
  assert.doesNotMatch(diffContainer.innerHTML, /diff-unified-line diff-meta">\\\\ No newline at end of file/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-add">\+const New = 1/);
  assert.doesNotMatch(diffContainer.innerHTML, /\s+1\s+2 \+const New = 1/);
  assert.match(content.innerHTML, /unit/);
  assert.match(content.innerHTML, /Review note/);
  assert.match(content.innerHTML, /1234567890ab/);
  assert.match(content.innerHTML, /9876543210ab/);
  assert.match(content.innerHTML, /outdated anchor/);
  assert.match(content.innerHTML, /const Value = 1/);
  assert.match(content.innerHTML, /reviewer/);
  assert.match(content.innerHTML, /data-thread-claim="th-0001"/);
  assert.match(content.innerHTML, /data-thread-reply="th-0001"/);
  assert.match(content.innerHTML, /data-claim-kind="fixed"/);
  assert.match(content.innerHTML, /data-claim-commit="1234567890abcdef"/);
  assert.match(content.innerHTML, /I will address it\./);
  // Diff mode toggle renders and defaults to unified without refetching.
  assert.match(diffContainer.innerHTML, /data-diff-mode-toggle/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="unified"/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="split"/);
  assert.match(diffContainer.innerHTML, /aria-pressed="true"[^>]*>Unified/);
  assert.match(diffContainer.innerHTML, /aria-pressed="false"[^>]*>Split/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="unified">/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-add">\+const New = 1/);
  assert.equal(fetchCalls.filter((call) => call.path === "/ui/api/v2/changes/ch-0001/diff").length, 1);
});

test("change detail diff uses the same width for all files and hunks", async () => {
  const longLine = "const LongerValue = \"wide\";";
  const diff = {
    change_id: "ch-0001",
    head_sha: "1234567890abcdef",
    available: true,
    total_files: 2,
    additions: 1,
    deletions: 0,
    files: [
      {
        path: "app.go",
        additions: 0,
        deletions: 0,
        hunks: [
          {
            header: "@@ -1 +1 @@",
            lines: [{ kind: "context", old_line: 1, new_line: 1, text: "short" }],
          },
          {
            header: "@@ -20 +20 @@",
            lines: [{ kind: "context", old_line: 20, new_line: 20, text: "also short" }],
          },
        ],
      },
      {
        path: "wide.go",
        additions: 1,
        deletions: 0,
        hunks: [{
          header: "@@ -1 +1 @@",
          lines: [{ kind: "add", new_line: 1, text: longLine }],
        }],
      },
    ],
  };
  const context = await scriptContext();

  const unifiedHTML = context.renderDiffSummary(diff, "unified");
  const unifiedWidths = [...unifiedHTML.matchAll(/--diff-unified-width: (\d+)ch/g)].map((match) => match[1]);
  assert.deepEqual(unifiedWidths, [String(longLine.length + 1), String(longLine.length + 1), String(longLine.length + 1)]);

  const splitHTML = context.renderDiffSummary(diff, "split");
  const splitWidths = [...splitHTML.matchAll(/--diff-split-width: (\d+)ch/g)].map((match) => match[1]);
  assert.deepEqual(splitWidths, [String((longLine.length + 4) * 2), String((longLine.length + 4) * 2), String((longLine.length + 4) * 2)]);
});

test("change detail diff renders side-by-side split mode", async () => {
  const diff = {
    change_id: "ch-0001",
    head_sha: "1234567890abcdef",
    available: true,
    total_files: 1,
    additions: 3,
    deletions: 1,
    files: [{
      path: "app.go",
      additions: 3,
      deletions: 1,
      hunks: [{
        header: "@@ -1,2 +1,4 @@",
        lines: [
          { kind: "context", old_line: 1, new_line: 1, text: "package app" },
          { kind: "delete", old_line: 2, text: "const Old = 1" },
          { kind: "add", new_line: 2, text: "const New = 1" },
          { kind: "add", new_line: 3, text: "const Extra = 2" },
          { kind: "context", old_line: 4, new_line: 4, text: "var Value = 0" },
        ],
      }],
    }],
  };
  const context = await scriptContext();
  const html = context.renderDiffSummary(diff, "split");

  assert.match(html, /data-diff-mode="split"/);
  assert.match(html, /<table class="diff-split" style="--diff-split-width: 38ch;">/);
  assert.match(html, /<th class="diff-col-old">Old<\/th>/);
  assert.match(html, /<th class="diff-col-new">New<\/th>/);
  assert.match(html, /diff-row-pair/);
  assert.match(html, /diff-text diff-del">const Old = 1<\/span><\/td><td class="diff-col-new"/);
  assert.match(html, /diff-text diff-add">const New = 1<\/span>/);
  assert.match(html, /diff-row-add[^"]*"[^>]*><td class="diff-col-old empty"/);
  assert.match(html, /diff-text diff-add">const Extra = 2<\/span>/);
  assert.match(html, /diff-row-context/);
  assert.match(html, /package app/);
  assert.match(html, /var Value = 0/);
  assert.doesNotMatch(html, /<pre class="thread-context"/);
});

test("change detail diff toggle re-renders cached diff in the new mode without refetching", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const storage = new Map();
  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const splitButton = {
    listener: null,
    getAttribute(name) { return name === "data-diff-mode" ? "split" : null; },
    closest() { return diffContainer; },
    addEventListener(event, listener) { if (event === "click") this.listener = listener; },
  };
  const diffContainer = {
    innerHTML: "",
    attributes: new Map(),
    setAttribute(name, value) { this.attributes.set(name, value); },
    getAttribute(name) { return this.attributes.get(name) || null; },
    closest() { return this; },
    querySelectorAll(selector) { return selector === "[data-diff-mode-toggle] button" ? [splitButton] : []; },
  };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: { cookie: "flow_ui_csrf=csrf-token", addEventListener() {} },
    history: { pushState() {} },
    window: { location: { pathname: "/ui/changes/ch-0001" }, addEventListener() {}, open() {}, localStorage },
    fetch(path) {
      fetchCalls.push(path);
      if (path === "/ui/api/v2/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "1234567890abcdef",
            available: true,
            total_files: 1,
            additions: 3,
            deletions: 1,
            files: [{
              path: "app.go",
              additions: 3,
              deletions: 1,
              hunks: [{
                header: "@@ -1,2 +1,4 @@",
                lines: [
                  { kind: "context", old_line: 1, new_line: 1, text: "package app" },
                  { kind: "delete", old_line: 2, text: "const Old = 1" },
                  { kind: "add", new_line: 2, text: "const New = 1" },
                ],
              }],
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change: { id: "ch-0001", task_id: "t-alpha-0001", branch: "task/t-alpha-0001", base: "main", head_sha: "1234567890abcdef", updated_at: "2026-06-07T12:00:00Z" },
          task: { id: "t-alpha-0001", title: "Ship web UI" },
          review_state: "approved",
          required_checks: { total: 0, satisfied: 0 },
          checks: [],
          threads: [],
          can_merge: false,
        }),
      });
    },
    console,
  };

  await applyContext(context);
  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    if (selector === '[data-change-diff="ch-0001"]') return diffContainer;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderChange("ch-0001");

  assert.match(diffContainer.innerHTML, /data-diff-mode="unified"/);
  assert.match(diffContainer.innerHTML, /<pre class="diff-unified"/);
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), undefined);
  assert.equal(fetchCalls.filter((p) => p === "/ui/api/v2/changes/ch-0001/diff").length, 1);
  assert.equal(typeof splitButton.listener, "function");

  splitButton.listener();

  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.match(diffContainer.innerHTML, /data-diff-mode="split"/);
  assert.match(diffContainer.innerHTML, /<table class="diff-split"/);
  assert.doesNotMatch(diffContainer.innerHTML, /<pre class="thread-context"/);
  assert.equal(fetchCalls.filter((p) => p === "/ui/api/v2/changes/ch-0001/diff").length, 1);
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

test("thread claim action posts claim payload and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "not_warranted",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "Intentional behavior.";
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", state: "claimed" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v2/threads/th-0001/claims");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "not_warranted",
    body: "Intentional behavior.",
    claim_commit_sha: "",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("thread fixed claim posts current head without prompting", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "fixed",
      claimCommit: "1234567890abcdef",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({
    prompt() {
      throw new Error("fixed claims should not prompt");
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", state: "claimed" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "fixed",
    body: "",
    claim_commit_sha: "1234567890abcdef",
  });
  assert.equal(refreshed, true);
});

test("thread claim action requires rationale for non-fixed claims", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "superseded",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "";
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(status.textContent, "Thread claim rationale is required");
  assert.equal(refreshed, false);
});

test("thread reply action posts comment payload and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadReply: "th-0001",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "I can reproduce this.";
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", comments: [{ body: "I can reproduce this." }] } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-reply]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v2/threads/th-0001/comments");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    body: "I can reproduce this.",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("thread reply action requires comment text", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadReply: "th-0001",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return " ";
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-reply]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(status.textContent, "Thread reply is required");
  assert.equal(refreshed, false);
});

test("phase approve action posts to task phase endpoint and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { phaseApprove: "t-alpha-0001", project: "p-alpha" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ task: { id: "t-alpha-0001" }, flow: {} }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-phase-approve]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/phase/approve");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "phase approved");
});

test("phase request-changes form posts feedback to task phase endpoint and refreshes", async () => {
  let submitHandler;
  const form = {
    dataset: { phaseRequestChanges: "t-alpha-0001", project: "p-alpha" },
    elements: {
      feedback: { value: "Please narrow the first step." },
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ task: { id: "t-alpha-0001" }, flow: {} }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-phase-request-changes]" ? [form] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/phase/request-changes");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { feedback: "Please narrow the first step." });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "changes requested");
});

test("attention reply form posts message and status log id", async () => {
  let submitHandler;
  const form = {
    dataset: { attentionReplyForm: "t-alpha-0001", statusLogId: "42", project: "p-alpha" },
    elements: {
      message: { value: "Use the smaller scope." },
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ queued: true }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-attention-reply-form]" ? [form] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/attention/reply");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    message: "Use the smaller scope.",
    status_log_id: 42,
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "reply sent");
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

test("human review approval action reports satisfied check and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      humanReviewApprove: "t-alpha-0001",
      checkName: "human-review",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          check: { name: "human-review", kind: "human", required: true, verdict: "satisfied" },
          review_state: "approved",
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-human-review-approve]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindTaskActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v2/tasks/t-alpha-0001/checks/human-review");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "human",
    required: true,
    verdict: "satisfied",
    details: "approved via web UI",
    reporter: "web-ui",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("claimed threads render reply without claim actions", async () => {
  const context = await scriptContext();
  const html = context.renderThread({
    id: "th-0001",
    state: "claimed",
    claim_kind: "fixed",
    claim_commit_sha: "1234567890abcdef",
    file_path: "app.go",
    line: 4,
    comments: [{ actor: "author", body: "Fixed.", created_at: "2026-06-07T12:00:00Z" }],
  }, "1234567890abcdef");

  assert.match(html, /claim fixed/);
  assert.match(html, /data-thread-reply="th-0001"/);
  assert.doesNotMatch(html, /data-thread-claim="th-0001"/);
});

test("review threads mark anchors that differ from the current change head", async () => {
  const context = await scriptContext();
  const staleHTML = context.renderThread({
    id: "th-0001",
    state: "open",
    anchor_commit_sha: "oldhead1234567890",
    file_path: "app.go",
    line: 4,
    context: "const Value = 1",
  }, "newhead1234567890");
  const currentHTML = context.renderThread({
    id: "th-0002",
    state: "open",
    anchor_commit_sha: "newhead1234567890",
    file_path: "app.go",
    line: 5,
    context: "const Value = 2",
  }, "newhead1234567890");

  assert.match(staleHTML, /outdated anchor/);
  assert.match(staleHTML, /oldhead12345/);
  assert.match(staleHTML, /const Value = 1/);
  assert.doesNotMatch(currentHTML, /outdated anchor/);
  assert.match(currentHTML, /newhead12345/);
});

test("change diff cache refetches only when head changes", async () => {
  let fetchCount = 0;
  const context = await scriptContext({}, {
    fetch(path) {
      fetchCount += 1;
      assert.equal(path, "/ui/api/v2/changes/ch-0001/diff");
      const headSHA = fetchCount === 1 ? "head-1" : "head-2";
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change_id: "ch-0001",
          head_sha: headSHA,
          available: true,
          total_files: 1,
          files: [{ path: "one.go", additions: 1, deletions: 0 }],
        }),
      });
    },
  });
  const containers = [{ innerHTML: "" }, { innerHTML: "" }, { innerHTML: "" }];
  let containerIndex = 0;
  const app = new context.FlowApp();
  app.querySelector = () => containers[containerIndex++];

  await app.renderChangeDiff("ch-0001", "head-1");
  await app.renderChangeDiff("ch-0001", "head-1");
  await app.renderChangeDiff("ch-0001", "head-2");

  assert.match(containers[0].innerHTML, /one.go/);
  assert.match(containers[1].innerHTML, /one.go/);
  assert.match(containers[2].innerHTML, /one.go/);
  assert.equal(fetchCount, 2);
});

test("change diff ignores payload for a different head", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      assert.equal(path, "/ui/api/v2/changes/ch-0001/diff");
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change_id: "ch-0001",
          head_sha: "new-head",
          available: true,
          files: [{ path: "stale.go", additions: 1, deletions: 0 }],
        }),
      });
    },
  });
  const container = { innerHTML: "" };
  const app = new context.FlowApp();
  app.querySelector = () => container;

  const rendered = await app.renderChangeDiff("ch-0001", "old-head");

  assert.equal(rendered, true);
  assert.match(container.innerHTML, /waiting for refresh/);
  assert.equal(app.changeDiffCache?.has("ch-0001:old-head"), false);
});

test("task detail renders owner metadata, relations, sessions, changes, and checks", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/projects/p-alpha/tasks/t-alpha-0001" },
      addEventListener() {},
      open() {
        throw new Error("window.open should not be used for task detail render");
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          project_id: "p-alpha",
          project_name: "alpha",
          task: {
            id: "t-alpha-0001",
            title: "Task detail",
            body: "Body\n\n## Requirements\n- Done",
            priority: 2,
            schedule_state: "up_next",
            triage_state: "accepted",
            created_by: "agent",
            source_task_id: "t-alpha-0000",
            source_change_id: "ch-0000",
            updated_at: "2026-06-07T12:00:00Z",
          },
          task_detail: {
            tags: [{ slug: "web-ui" }],
            relations: [{ source_task_id: "t-alpha-0002", target_task_id: "t-alpha-0001", kind: "blocks" }],
            active_session: { id: "s-0001", state: "waiting", worker_id: "w-local", branch: "task/t-alpha-0001" },
            terminal_available: true,
            sessions: [{ id: "s-0001", state: "waiting", worker_id: "w-local", branch: "task/t-alpha-0001", terminal_available: true, transcript_available: true, updated_at: "2026-06-07T12:01:00Z" }],
            changes: [{ id: "ch-0001", branch: "task/t-alpha-0001", head_sha: "abcdef1234567890", updated_at: "2026-06-07T12:02:00Z" }],
            ready_change: { id: "ch-ready", branch: "task/t-alpha-0001", head_sha: "1234567890abcdef", ready_at: "2026-06-07T12:03:00Z" },
            review_state: "changes_requested",
            required_checks: { total: 1, blocked: 1 },
            checks: [{ name: "unit", kind: "ci", required: true, verdict: "blocked", details: "failed" }],
            attachments: [{ id: "att-0001", task_id: "t-alpha-0001", stage: "reviewer", filename: "review.png", content_type: "image/png", size_bytes: 2048 }],
            transitions: [
              { seq: 2, from_phase: "up_next", event_kind: "ensure_author_job", to_phase: "authoring", actor: "owner:cli", created_at: "2026-06-07T12:04:00Z" },
            ],
            lifecycle_graph: {
              current_phase: "authoring",
              edges: [{ from_phase: "up_next", to_phase: "authoring", count: 1 }],
              reviewer_sends: 0,
              verifier_sends: 0,
            },
          },
        }),
      });
    },
    console,
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderTask("t-alpha-0001", undefined, "p-alpha");

  assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001");
  assert.match(content.innerHTML, /class="detail task-detail"/);
  assert.match(content.innerHTML, /class="task-detail-grid"/);
  assert.match(content.innerHTML, /class="task-detail-column task-detail-editor"/);
  assert.match(content.innerHTML, /class="task-detail-column task-detail-activity"/);
  assert.match(content.innerHTML, /class="task-detail-column task-detail-system"/);
  assert.match(content.innerHTML, /class="task-detail-lifecycle"><h3>Activity<\/h3><div class="lifecycle-timeline">/);
  // Unified timeline: sessions, transitions and status all render inside it.
  assert.match(content.innerHTML, /<div class="feed timeline-feed" data-timeline>/);
  assert.match(content.innerHTML, /data-timeline-glyph="session"/);
  assert.match(content.innerHTML, /data-timeline-glyph="transition"/);
  // Standalone Sessions/Status feeds are gone (folded into the timeline).
  assert.doesNotMatch(content.innerHTML, /<h3>Sessions<\/h3>/);
  assert.doesNotMatch(content.innerHTML, /<h3>Status<\/h3>/);
  // Read-only detail summary with an Edit toggle that reveals the form.
  assert.match(content.innerHTML, /data-task-read-only/);
  assert.match(content.innerHTML, /data-task-edit-toggle/);
  assert.match(content.innerHTML, /data-task-edit-form/);
  assert.match(content.innerHTML, /web-ui/);
  assert.match(content.innerHTML, /href="\/ui\/tasks\/t-alpha-0002"/);
  assert.match(content.innerHTML, /data-terminal="s-0001"/);
  assert.equal(content.innerHTML.match(/data-terminal="s-0001"/g)?.length, 2);
  assert.match(content.innerHTML, /data-session-transcript="s-0001"/);
  // Lifecycle is driven by the workflow engine: the detail exposes a Schedule
  // control rather than the legacy manual state/pause controls.
  assert.match(content.innerHTML, /data-workflow-schedule="t-alpha-0001"/);
  assert.doesNotMatch(content.innerHTML, /data-task-state-form=/);
  assert.doesNotMatch(content.innerHTML, /data-pause=/);
  assert.match(content.innerHTML, /ch-0001/);
  assert.match(content.innerHTML, /Ready Change/);
  assert.match(content.innerHTML, /ch-ready/);
  assert.match(content.innerHTML, /s-0001/);
  assert.match(content.innerHTML, /unit/);
  assert.match(content.innerHTML, /review\.png/);
  assert.match(content.innerHTML, /class="attachment-preview"/);
  assert.match(content.innerHTML, /\/ui\/api\/v2\/projects\/p-alpha\/tasks\/t-alpha-0001\/attachments\/att-0001\?download=1/);
  const taskHeadHTML = content.innerHTML.slice(content.innerHTML.indexOf('class="detail-head task-detail-head"'), content.innerHTML.indexOf('class="summary-grid"'));
  assert.equal(taskHeadHTML.match(/class="badge/g)?.length, 1);
  assert.doesNotMatch(taskHeadHTML, /changes requested|checks 0\/1/);
  assert.match(content.innerHTML, /Source Task/);
  assert.match(content.innerHTML, /Activity/);
  assert.match(content.innerHTML, /ensure_author_job|up_next/);
});

test("task detail replaces the generic working badge with the current workflow activity", async () => {
  const harness = await browserSmokeHarness("/ui/projects/p-alpha/tasks/t-alpha-0001", {
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001": {
      project_id: "p-alpha",
      task: {
        id: "t-alpha-0001",
        title: "Review the implementation",
        state: "in_progress",
        priority: 1,
        created_by: "agent",
        updated_at: "2026-06-18T12:37:00Z",
      },
      task_detail: {
        // These legacy status summaries must not compete with the workflow step.
        review_state: "changes_requested",
        required_checks: { total: 2, satisfied: 1 },
      },
    },
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow": {
      detail: {
        run: {
          id: "wr-1",
          task_id: "t-alpha-0001",
          run_sequence: 1,
          state: "running",
          current_node_key: "security-review",
          current_node_run_id: "nr-review",
          transitions_used: 3,
          transition_budget: 50,
          snapshot: {
            flow_id: "fl-default",
            flow_name: "default workflow",
            nodes: [
              { key: "implement", name: "Implement", kind: "agent", config: {} },
              { key: "security-review", name: "Security review", kind: "change_review", config: {} },
            ],
          },
        },
        node_runs: [
          { id: "nr-implement", node_key: "implement", visit: 1, attempt: 1, state: "succeeded", outcome: "draft_ready" },
          { id: "nr-review", node_key: "security-review", visit: 1, attempt: 1, state: "running" },
        ],
      },
    },
  });

  await harness.app.load();

  const html = harness.content.innerHTML;
  const taskHeadHTML = html.slice(html.indexOf('class="detail-head task-detail-head"'), html.indexOf('class="summary-grid"'));
  assert.equal(taskHeadHTML.match(/class="badge/g)?.length || 0, 0);
  assert.doesNotMatch(taskHeadHTML, />working</i);
  assert.match(taskHeadHTML, /class="task-current-step" data-current-workflow-step/);
  assert.match(taskHeadHTML, /<strong title="security-review">Reviewing security<\/strong>/);
  assert.doesNotMatch(taskHeadHTML, /task-current-step-label/);
  assert.match(taskHeadHTML, /default workflow · change review · running/);
  assert.doesNotMatch(taskHeadHTML, /changes requested|checks 1\/2/);

  // The workflow occupies its own full-width grid row, keeping run diagnostics
  // and readable history while still highlighting the active node.
  assert.match(html, /<div class="task-detail-workflow"><div class="workflow-detail">/);
  assert.ok(html.indexOf("task-detail-workflow") < html.indexOf("task-detail-editor"));
  assert.match(html, /run 1 · running · transitions 3\/50/);
  assert.match(html, /<strong>Implement<\/strong><span class="workflow-step-key">implement<\/span>/);
  assert.match(html, /outcome: draft ready/);
  assert.match(html, /class="feed-item workflow-step is-current" data-current-workflow-step-run/);
  assert.match(html, /<strong>Security review<\/strong><span class="workflow-step-key">security-review<\/span>/);
  assert.doesNotMatch(html, /default workflow · security-review · transitions/);
});

test("task detail renders a human gate wait as a full-width approval panel", async () => {
  const harness = await browserSmokeHarness("/ui/projects/p-alpha/tasks/t-alpha-0001", {
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001": {
      project_id: "p-alpha",
      task: {
        id: "t-alpha-0001",
        title: "Gated task",
        state: "in_progress",
        priority: 1,
        created_by: "agent",
        updated_at: "2026-06-18T12:37:00Z",
      },
      task_detail: {},
    },
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow": {
      detail: {
        run: {
          id: "wr-1",
          task_id: "t-alpha-0001",
          current_node_key: "approve-plan",
          snapshot: {
            flow_id: "fl-plan",
            flow_name: "planned",
            start_node: "approve-plan",
            nodes: [{
              key: "approve-plan",
              name: "Approve plan",
              kind: "human_gate",
              config: {
                human_gate: {
                  instructions: "Review the proposed implementation tasks.",
                  outcomes: ["approved", "changes_requested", "rejected"],
                },
              },
            }, {
              key: "done",
              name: "Done",
              kind: "terminal",
              config: { terminal: { resolution: "completed" } },
            }],
            edges: [{ from: "approve-plan", outcome: "approved", to: "done" }],
          },
        },
        node_runs: [],
        transitions: [],
        open_wait: {
          id: "wait-1",
          workflow_run_id: "wr-1",
          node_run_id: "nr-1",
          kind: "human_gate",
          message: "Waiting for a human decision.",
        },
      },
    },
  });

  await harness.app.load();

  const html = harness.content.innerHTML;
  assert.match(html, /class="human-attention-panel"/);
  assert.match(html, /Approve plan/);
  assert.match(html, /Review the proposed implementation tasks\./);
  const taskHeadHTML = html.slice(html.indexOf('class="detail-head task-detail-head"'), html.indexOf('class="summary-grid"'));
  assert.match(taskHeadHTML, /data-phase="blocked"><span class="dot"><\/span>blocked<\/span>/);
  assert.match(taskHeadHTML, /Current step<\/span><strong title="approve-plan">Approve plan<\/strong>/);
  assert.match(taskHeadHTML, /planned · human gate · waiting for human decision/);
  // Each gate outcome becomes a workflow respond button carrying its node run.
  assert.match(html, /data-workflow-respond="nr-1"[^>]*data-outcome="approved"/);
  assert.match(html, /data-workflow-respond="nr-1"[^>]*data-outcome="changes_requested"/);
  assert.match(html, /data-workflow-respond="nr-1"[^>]*data-outcome="rejected"/);
  assert.match(html, /data-workflow-feedback/);
  assert.match(html, /class="workflow-chart"/);
  assert.match(html, /class="workflow-node is-current is-start" data-node="approve-plan"/);
  assert.match(html, /approved ×0/);
  // The panel sits between the summary grid and the detail grid.
  assert.ok(html.indexOf("summary-grid") < html.indexOf("human-attention-panel"));
  assert.ok(html.indexOf("human-attention-panel") < html.indexOf("task-detail-grid"));
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

test("renderLifecycleChart draws the canonical graph with counts and highlights", async () => {
  const context = await scriptContext();
  const html = context.renderLifecycleChart({
    current_phase: "critique",
    edges: [
      { from_phase: "triage", to_phase: "backlog", count: 1 },
      { from_phase: "authoring", to_phase: "critique", count: 4 },
      { from_phase: "critique", to_phase: "authoring", count: 3 },
    ],
    reviewer_sends: 2,
    verifier_sends: 1,
  });
  assert.match(html, /<svg/);
  const phases = [
    "backlog", "triage", "up_next", "planning", "authoring", "critique",
    "acceptance", "approved", "merged_closed", "rejected_closed", "abandoned",
  ];
  for (const phase of phases) {
    assert.match(html, new RegExp(`data-node="${phase}"`), `missing node ${phase}`);
  }
  assert.match(html, /data-node="critique"[^>]*\bis-current\b|class="[^"]*is-current[^"]*"[^>]*data-node="critique"/);
  assert.match(html, /data-node="critique"[\s\S]*class="lifecycle-current-halo"/);
  assert.equal((html.match(/class="lifecycle-current-halo"/g) || []).length, 1);
  assert.match(html, /×4/);
  assert.match(html, /×3/);
  // triage→backlog is a canonical (accept) edge, not a dashed overlay.
  assert.match(html, /×1/);
  assert.doesNotMatch(html, /is-extra/);
  assert.match(html, /is-untaken/);
  assert.match(html, /reviewer ×2/);
  assert.match(html, /verifier ×1/);
});

test("renderLifecycleChart overlays observed edges missing from the canonical graph", async () => {
  const context = await scriptContext();
  const html = context.renderLifecycleChart({
    current_phase: "authoring",
    edges: [{ from_phase: "merged_closed", to_phase: "authoring", count: 2 }],
    reviewer_sends: 0,
    verifier_sends: 0,
  });
  assert.match(html, /is-extra/);
  assert.match(html, /×2/);
});

test("renderLifecycleChart tolerates a missing or empty graph", async () => {
  const context = await scriptContext();
  for (const graph of [null, undefined, {}]) {
    const html = context.renderLifecycleChart(graph);
    assert.match(html, /<svg/);
    assert.doesNotMatch(html, /is-current/);
    assert.doesNotMatch(html, /Sent back/);
  }
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
    labels: { "agent.harness.codex": "true" },
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
  assert.match(workerHTML, /agent\.harness\.codex=true/);
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

test("board cards render tags and relationship indicators", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard({
    id: "t-alpha-0001",
    title: "Tagged task",
    schedule_state: "backlog",
    triage_state: "triage",
    priority: 1,
    created_by: "agent",
  }, {
    tags: [{ slug: "web-ui" }],
    relations: {
      parents: 1,
      children: 2,
      blocks: 1,
      blocked_by: 1,
      related: 3,
    },
  }, "triage", false);

  assert.match(html, /web-ui/);
  assert.match(html, /parent 1/);
  assert.match(html, /child 2/);
  assert.match(html, /blocks 1/);
  assert.match(html, /blocked by 1/);
  assert.match(html, /related 3/);
});

test("board cards replace the generic working badge with the current workflow activity", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard({
    id: "t-alpha-0001",
    title: "Implement compact cards",
    state: "in_progress",
    priority: 1,
  }, {
    current_step: { key: "implement", name: "Implement", kind: "agent" },
    active_session: { id: "s-1", state: "working" },
    // These stale legacy values must not compete with the workflow step.
    review_state: "in_review",
    primary_action: "monitor",
  }, "working", false);

  assert.equal(html.match(/class="badge/g)?.length || 0, 0);
  assert.match(html, /<div class="card-current-step" title="Implementing">Implementing<\/div>/);
  assert.doesNotMatch(html, />working</i);
  assert.doesNotMatch(html, /in review/i);
  assert.doesNotMatch(html, /monitor/i);
});

test("board workflow steps handle review nodes, escaping, key fallback, and omission", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = { id: "t-alpha-0001", title: "Workflow step cases", state: "in_progress", priority: 1 };

  const review = app.renderTaskCard(task, {
    current_step: { key: "review", name: "Agent review", kind: "change_review" },
  }, "working", false);
  assert.doesNotMatch(review, /class="badge"/);
  assert.match(review, /<div class="card-current-step" title="Reviewing agent">Reviewing agent<\/div>/);
  assert.doesNotMatch(review, /Current step|>working</i);

  const longName = `Review <all> & "security" before merging this intentionally long localized change`;
  const escapedLongName = `Review &lt;all&gt; &amp; &quot;security&quot; before merging this intentionally long localized change`;
  const long = app.renderTaskCard(task, {
    current_step: { key: "long-review", name: longName, kind: "human_gate" },
  }, "blocked", false);
  assert.match(long, new RegExp(`<div class="card-current-step" title="${escapedLongName}">${escapedLongName}<\\/div>`));
  assert.doesNotMatch(long, /<span[^>]*card-current-step/);
  assert.doesNotMatch(long, /<script|<all>/);

  const fallback = app.renderTaskCard(task, {
    current_step: { key: "human-review_step", name: "" },
  }, "working", false);
  assert.match(fallback, /title="Working on human review step">Working on human review step<\/div>/);

  const missing = app.renderTaskCard(task, {}, "working", false);
  assert.match(missing, /title="Work in progress">Work in progress<\/div>/);
  assert.doesNotMatch(missing, />working</i);
});

test("board cards keep objective counts quiet and ignore obsolete state fields", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard({
    id: "t-alpha-0001",
    title: "Compact metadata",
    state: "in_progress",
    priority: 1,
  }, {
    current_step: { key: "checks", name: "Automated checks", kind: "automated_checks" },
    required_checks: { total: 2, satisfied: 1 },
    blockers: { count: 2, tasks: [] },
    relations: { blocked_by: 2 },
    latest_status: { message: "cannot continue", kind: "blocker" },
    review_state: "in_review",
    blocking_reason: "waiting for response",
    primary_action: "monitor",
    tags: [
      { slug: "checks 1/2" },
      { slug: "blocker" },
      { slug: "visible-tag" },
    ],
  }, "working", false);

  assert.equal(html.match(/checks 1\/2/g)?.length, 1);
  assert.match(html, /class="meta-quiet"[^>]*>[^<]*p1 · checks 1\/2/);
  assert.match(html, /blocked by 2/);
  assert.equal(html.match(/>blocker<\/span>/g)?.length, 1);
  assert.match(html, /visible-tag/);
  assert.doesNotMatch(html, /in review/i);
  assert.doesNotMatch(html, /waiting for response/i);
  assert.doesNotMatch(html, /monitor/i);
});

test("board cards render job terminal actions without active sessions", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard({
    id: "t-alpha-0001",
    title: "Review running",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  }, {
    terminal_available: true,
    terminal_job_id: "j-0001",
  }, "in_review", false);

  assert.match(html, /class="button secondary terminal-button icon-button"/);
  assert.match(html, /data-job-terminal="j-0001"/);
  assert.match(html, /aria-label="Open terminal"/);
  assert.match(html, /<svg class="button-icon"/);
  assert.doesNotMatch(html, /data-terminal=/);
});

test("board cards render session terminal icon actions in every running board state", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const states = ["planning", "in_progress", "in_review", "changes_requested", "ready_to_merge"];

  for (const state of states) {
    const html = app.renderTaskCard({
      id: `t-alpha-${state}`,
      title: `${state} task`,
      schedule_state: "up_next",
      triage_state: "accepted",
      priority: 1,
    }, {
      active_session: { id: `s-${state}`, state: "working", terminal_available: true },
    }, state, false);

    assert.match(html, new RegExp(`data-terminal="s-${state}"`));
    assert.match(html, /class="button secondary terminal-button icon-button"/);
    assert.match(html, /aria-label="Open terminal"/);
    assert.match(html, /<svg class="button-icon"/);
  }
});

test("ready to merge cards link to their change without a legacy review badge", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard({
    id: "t-alpha-0001",
    title: "Merge me",
    state: "in_progress",
    priority: 1,
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    change: {
      id: "ch-0001",
      branch: "task/t-alpha-0001",
      head_sha: "abcdef1234567890",
    },
    review_state: "changes_requested",
  }, "approved", false);

  assert.match(html, /<a href="\/ui\/changes\/ch-0001" data-link>ch-0001<\/a>/);
  assert.match(html, /task\/t-alpha-0001/);
  assert.doesNotMatch(html, /changes requested/);
  // Merging happens on the change page; the card only exposes workflow controls.
  assert.doesNotMatch(html, /data-merge=/);
  assert.doesNotMatch(html, /head abcdef123456/);
  assert.match(html, /data-workflow-reset="t-alpha-0001"/);
  assert.match(html, /data-workflow-done="t-alpha-0001"/);
});

test("cards carry phase identity while active work uses an activity message", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = {
    id: "t-alpha-0001",
    title: "Phase identity",
    state: "in_progress",
    priority: 1,
  };

  const inReview = app.renderTaskCard(task, {}, "critique", false);
  assert.match(inReview, /<article class="card" data-phase="critique"/);
  assert.match(inReview, /<span class="badge" data-phase="critique"><span class="dot"><\/span>critique<\/span>/);

  const working = app.renderTaskCard(task, {}, "authoring", false);
  assert.match(working, /<article class="card" data-phase="authoring"/);
  assert.match(working, />Work in progress<\/div>/);
  assert.doesNotMatch(working, /class="badge"/);

  const unscheduled = app.renderTaskCard({ ...task, state: "unscheduled" }, {}, "unscheduled", false);
  assert.match(unscheduled, /<article class="card" data-phase="backlog"/);
  assert.match(unscheduled, /<span class="badge" data-phase="backlog"><span class="dot"><\/span>unscheduled<\/span>/);
});

test("blocked cards show one blocked badge and the workflow step", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = {
    id: "t-alpha-0001",
    title: "Blocked work",
    state: "in_progress",
    priority: 1,
  };

  const blocked = app.renderTaskCard(task, {
    current_step: { key: "human-review", name: "Human change review", kind: "human_gate" },
    active_session: { id: "s-1", state: "working" },
  }, "working", true);
  assert.match(blocked, /<article class="card" data-phase="blocked"/);
  assert.match(blocked, /<span class="badge" data-phase="blocked"><span class="dot"><\/span>blocked<\/span>/);
  assert.match(blocked, /<div class="card-current-step" title="Human change review">Human change review<\/div>/);
  assert.equal(blocked.match(/class="badge/g)?.length, 1);
  assert.doesNotMatch(blocked, /data-phase="authoring"/);
  assert.doesNotMatch(blocked, /\bworking\b/i);

  const scheduledDependency = app.renderTaskCard({ ...task, state: "scheduled" }, {
    current_step: { key: "implement", name: "Implement", kind: "agent" },
    blockers: { count: 2, tasks: [] },
    relations: { blocked_by: 2 },
  }, "scheduled", false);
  assert.match(scheduledDependency, /<article class="card" data-phase="up_next"/);
  assert.match(scheduledDependency, />scheduled<\/span>/);
  assert.match(scheduledDependency, /blocked by 2/);
  assert.doesNotMatch(scheduledDependency, />blocked<\/span>/);
});

test("lifecycle transitions render phase badges around an arrow", async () => {
  const context = await scriptContext();
  const html = context.renderTransition({
    event_kind: "advance",
    from_phase: "authoring",
    to_phase: "critique",
    actor: "workflow",
    created_at: "2026-06-07T12:00:00Z",
  });

  assert.match(html, /<span class="badge" data-phase="authoring"><span class="dot"><\/span>authoring<\/span>/);
  assert.match(html, /<span class="arrow">→<\/span>/);
  assert.match(html, /<span class="badge" data-phase="critique"><span class="dot"><\/span>critique<\/span>/);

  const initial = context.renderTransition({ event_kind: "create", to_phase: "triage", created_at: "2026-06-07T12:00:00Z" });
  assert.match(initial, /<span class="badge idle">—<\/span>/);
});

test("unified timeline merges sessions, transitions, and status by time with a show-more cap", async () => {
  const context = await scriptContext();
  const sessions = [
    { id: "s-1", state: "working", worker_id: "w-local", branch: "task/t-alpha-0001", terminal_available: true, transcript_available: true, updated_at: "2026-06-07T12:05:00Z" },
  ];
  const transitions = [
    { seq: 1, event_kind: "session_ready", session_id: "s-1", head_sha: "abcdef1234567890", change_id: "ch-1", created_at: "2026-06-07T12:03:00Z" },
    { seq: 2, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:04:00Z" },
    { seq: 3, event_kind: "ensure_author_job", from_phase: "up_next", to_phase: "authoring", actor: "owner:cli", created_at: "2026-06-07T12:01:00Z" },
  ];
  const statusLog = [
    { id: 9, kind: "progress", actor: "author", message: "editing files", created_at: "2026-06-07T12:02:00Z" },
  ];

  const html = context.renderTimeline({ sessions, transitions, statusLog });

  // All three types render inside the timeline feed.
  assert.match(html, /data-timeline-glyph="session"/);
  assert.match(html, /data-timeline-glyph="transition"/);
  assert.match(html, /data-timeline-glyph="status"/);
  // Session controls render from the top-N session list...
  assert.match(html, /data-terminal="s-1"/);
  assert.match(html, /data-session-transcript="s-1"/);
  // ...and a session_ready transition row enriched with a session_id also offers
  // a transcript control for that exact session.
  const transcriptButtons = html.match(/data-session-transcript="s-1"/g) || [];
  assert.ok(transcriptButtons.length >= 2, `expected at least 2 transcript buttons, got ${transcriptButtons.length}`);
  // Times are relative with the absolute value available on hover.
  assert.match(html, /<time title="[^"]+">/);
  // No raw session id as a headline and no task id echoed.
  assert.doesNotMatch(html, /<strong>s-1<\/strong>/);

  // Entries interleave by time, newest first: s-1 (12:05), then
  // session_state_changed (12:04), session_ready (12:03), status (12:02),
  // ensure_author_job (12:01).
  const stateIdx = html.indexOf("session state");
  const readyIdx = html.indexOf("session ready");
  assert.ok(stateIdx > -1 && readyIdx > -1, "session transition rows should render");
  assert.ok(stateIdx < readyIdx, "newer session_state_changed should precede the older session_ready row");
});

test("unified timeline caps rendered rows and offers a show-more control", async () => {
  const context = await scriptContext();
  const sessions = [];
  const transitions = [];
  // Exceed TIMELINE_CAP (20) so the cap and hidden batch both engage.
  for (let i = 0; i < 25; i += 1) {
    transitions.push({
      seq: i + 1,
      event_kind: "ensure_author_job",
      from_phase: "up_next",
      to_phase: "authoring",
      actor: "owner:cli",
      created_at: `2026-06-07T12:${String(i).padStart(2, "0")}:00Z`,
    });
  }
  const html = context.renderTimeline({ sessions, transitions, statusLog: [] });
  assert.match(html, /data-timeline-show-more/);
  assert.match(html, /Show 5 more/);
  assert.match(html, /class="timeline-hidden" hidden/);
  // Only TIMELINE_CAP (20) rows render before the hidden batch; the remaining 5
  // sit inside the hidden container until "Show more" expands them.
  const [visible, hidden] = html.split("timeline-hidden");
  const visibleRows = (visible.match(/class="feed-item timeline-row"/g) || []).length;
  const hiddenRows = (hidden.match(/class="feed-item timeline-row"/g) || []).length;
  assert.equal(visibleRows, 20);
  assert.equal(hiddenRows, 5);
});

test("unified timeline renders an empty state when there is no activity", async () => {
  const context = await scriptContext();
  const html = context.renderTimeline({ sessions: [], transitions: [], statusLog: [] });
  assert.match(html, /No timeline activity yet/);
  assert.doesNotMatch(html, /data-timeline-show-more/);
});

test("unified timeline collapses consecutive session_state_changed rows for one session into a run", async () => {
  const context = await scriptContext();
  const transitions = [
    { seq: 3, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:05:00Z" },
    { seq: 2, event_kind: "session_state_changed", session_id: "s-1", session_state: "waiting", created_at: "2026-06-07T12:04:00Z" },
    { seq: 1, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:03:00Z" },
  ];
  const html = context.renderTimeline({ sessions: [], transitions, statusLog: [] });

  // The three same-session state changes collapse into a single summary row.
  assert.match(html, /data-timeline-run-toggle/);
  assert.match(html, /3 session state changes/);
  // The individual rows are hidden behind the toggle until expanded.
  assert.match(html, /class="timeline-run-rows" hidden/);
  const plainStateRows = (html.match(/<span class="badge idle session-state">session state<\/span>/g) || []).length;
  assert.ok(plainStateRows >= 3, "collapsed run should still render its child rows inside the hidden container");
});

test("unified timeline leaves a single session_state_changed row ungrouped", async () => {
  const context = await scriptContext();
  const transitions = [
    { seq: 1, event_kind: "session_state_changed", session_id: "s-1", session_state: "waiting", created_at: "2026-06-07T12:03:00Z" },
  ];
  const html = context.renderTimeline({ sessions: [], transitions, statusLog: [] });
  assert.doesNotMatch(html, /data-timeline-run-toggle/);
  assert.match(html, /session state/);
});

test("status entries render a kind badge", async () => {
  const context = await scriptContext();
  const blocker = context.renderStatus({
    actor: "author",
    message: "stuck on auth",
    kind: "blocker",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(blocker, /<span class="badge danger">blocker<\/span>/);
  assert.match(blocker, /stuck on auth/);

  const question = context.renderStatus({
    actor: "author",
    message: "which db?",
    kind: "question",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(question, /<span class="badge warn">question<\/span>/);

  const noKind = context.renderStatus({
    actor: "author",
    message: "default note",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(noKind, /<span class="badge idle">note<\/span>/);
});

test("feedback cards surface a non-note status kind badge", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = { id: "t-alpha-0001", title: "Blocked task", schedule_state: "up_next", triage_state: "accepted" };
  const blockerCard = {
    active_session: { id: "s-0001", state: "waiting" },
    latest_status: { message: "stuck on auth", kind: "blocker" },
  };
  const blockerHTML = app.renderTaskCard(task, blockerCard, "in_progress", false, 0, null, "question");
  assert.match(blockerHTML, /<span class="badge danger">blocker<\/span>/);
  assert.doesNotMatch(blockerHTML, /waiting for response/);

  const noteCard = { latest_status: { message: "running tests", kind: "note" } };
  const noteHTML = app.renderTaskCard(task, noteCard, "in_progress", false, 0, null, "question");
  assert.doesNotMatch(noteHTML, /class="badge danger"/);
  assert.match(noteHTML, /running tests/);
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
  assert.equal(context.renderVerdictBadge("failed"), `<span class="badge danger">failed</span>`);
  assert.equal(context.renderVerdictBadge("rejected"), `<span class="badge danger">rejected</span>`);
  assert.equal(context.renderVerdictBadge("needs_rerun"), `<span class="badge idle">needs rerun</span>`);
  assert.equal(context.renderVerdictBadge(""), `<span class="badge idle">pending</span>`);
});

test("board lanes carry data-lane attributes for the lane accent system", async () => {
  const harness = await browserSmokeHarness("/ui/board", {
    "/ui/api/v2/board": {
      boards: [{
        project_id: "p-alpha",
        project_name: "alpha",
        board: {
          Unscheduled: [{
            id: "t-alpha-0001",
            title: "Unscheduled task",
            state: "unscheduled",
            priority: 1,
          }],
        },
        lane_states: { "t-alpha-0001": "unscheduled" },
        task_cards: {},
      }],
    },
  });

  await harness.app.load();

  for (const lane of ["unscheduled", "scheduled", "in_progress", "done"]) {
    assert.match(harness.content.innerHTML, new RegExp(`<section class="lane" data-lane="${lane}">`));
  }
});

test("load marks content as nav or poll refresh and reports live poll state", async () => {
  const harness = await browserSmokeHarness("/ui/board", {
    "/ui/api/v2/board": { boards: [{ project_id: "p-alpha", project_name: "alpha", board: {}, lane_states: {}, task_cards: {} }] },
  });

  await harness.app.load();
  assert.equal(harness.content.dataset.refresh, "nav");
  assert.equal(harness.statusbar.dataset.state, "live");
  assert.equal(harness.sbLabel.textContent, "live");
  assert.equal(harness.sbMeta.textContent, "poll 10s");

  await harness.app.load({ fromPoll: true });
  assert.equal(harness.content.dataset.refresh, "poll");
});

test("non-polling routes report static instead of live", async () => {
  const harness = await browserSmokeHarness("/ui/tasks/new", {});

  await harness.app.load();

  assert.equal(harness.statusbar.dataset.state, "idle");
  assert.equal(harness.sbLabel.textContent, "static");
  assert.equal(harness.sbMeta.textContent, "");
  assert.deepEqual(harness.fetchCalls, ["/ui/api/v2/projects"]);
});

test("canonical task detail route resolves the embedded project", async () => {
  const harness = await browserSmokeHarness("/ui/tasks/t-alpha-0001", {
    "/ui/api/v2/tasks/t-alpha-0001": {
      project_id: "p-alpha",
      project_name: "alpha",
      task: { id: "t-alpha-0001", title: "Direct task", priority: 0, updated_at: "2026-07-21T12:00:00Z" },
      task_detail: {},
    },
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow": { detail: null },
    "/ui/api/v2/projects/p-alpha/flows": { flows: [], default_flow_id: "" },
  });

  await harness.app.load();

  assert.equal(harness.title.textContent, "Task · alpha");
  assert.match(harness.content.innerHTML, /t-alpha-0001 · Direct task/);
  assert.deepEqual(harness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/tasks/t-alpha-0001",
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow",
    "/ui/api/v2/projects/p-alpha/flows",
  ]);
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

test("feedback cards render handoff summaries", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = {
    id: "t-alpha-0001",
    title: "Needs feedback",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  };
  const card = {
    active_session: { id: "s-0001", state: "waiting", branch: "task/t-alpha-0001" },
    terminal_available: true,
    latest_status: { message: "Waiting on product decision" },
    handoff: {
      present: true,
      valid: true,
      summary: "Waiting for product decision before final polish.",
    },
  };
  const html = app.renderTaskCard(task, card, "in_progress", false, 0, null, "question");
  const inProgressHTML = app.renderTaskCard(task, card, "in_progress", false);

  assert.match(html, /Waiting on product decision/);
  assert.match(html, /handoff: Waiting for product decision before final polish\./);
  assert.match(html, /data-terminal="s-0001"/);
  assert.doesNotMatch(inProgressHTML, /handoff: Waiting for product decision before final polish\./);
});

test("browser smoke loads board and done preview", async () => {
  for (const path of ["/ui/board", "/ui/"]) {
    const harness = await browserSmokeHarness(path, {
      "/ui/api/v2/board": {
        boards: [{
          project_id: "p-alpha",
          project_name: "alpha",
          board: {
            Unscheduled: [{
              id: "t-alpha-0001",
              title: "Unscheduled task",
              state: "unscheduled",
              priority: 1,
            }, {
              id: "t-alpha-0002",
              title: "Discovered task",
              state: "unscheduled",
              priority: 2,
              created_by: "agent",
            }],
            InProgress: [{
              id: "t-alpha-0003",
              title: "Waiting task",
              state: "in_progress",
              priority: 1,
            }, {
              id: "t-alpha-0004",
              title: "Merge task",
              state: "in_progress",
              priority: 1,
            }],
          },
          lane_states: {
            "t-alpha-0001": "unscheduled",
            "t-alpha-0002": "unscheduled",
            "t-alpha-0003": "in_progress",
            "t-alpha-0004": "in_progress",
          },
          wait_reasons: {
            "t-alpha-0003": "question",
          },
          task_cards: {
            "t-alpha-0001": { tags: [{ slug: "planned" }] },
            "t-alpha-0002": { tags: [{ slug: "agent" }] },
            "t-alpha-0003": {
              active_session: { id: "s-0003", state: "waiting" },
              latest_status: { message: "Need a decision", kind: "question" },
            },
            "t-alpha-0004": {
              change: { id: "ch-0004", branch: "task/t-alpha-0004", head_sha: "abcdef1234567890" },
              review_state: "approved",
            },
          },
        }],
      },
      "/ui/api/v2/done?limit=20": { done: [] },
    });

    await harness.app.load();

    assert.equal(harness.title.textContent, "Board");
    for (const pattern of [/Unscheduled task/, /Discovered task/, /Waiting task/, /Merge task/]) {
      assert.match(harness.content.innerHTML, pattern);
    }
    assert.equal(harness.activeNavHref(), "/ui/board");
    // The board also fetches the Done lane preview.
    assert.deepEqual(harness.fetchCalls, ["/ui/api/v2/projects", "/ui/api/v2/board", "/ui/api/v2/done?limit=20"]);
    assert.equal(harness.status.textContent, "");
  }
});

test("browser smoke loads task and change deep links", async () => {
  const taskHarness = await browserSmokeHarness("/ui/projects/p-alpha/tasks/t-alpha-0001", {
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001": {
      project_id: "p-alpha",
      task: {
        id: "t-alpha-0001",
        title: "Task detail",
        body: "Task body\n\n## Requirements\n- Done",
        schedule_state: "up_next",
        triage_state: "accepted",
        priority: 3,
        requires_human_review: false,
        auto_merge: true,
        created_by: "human",
        updated_at: "2026-06-07T12:00:00Z",
      },
      task_detail: {
        tags: [{ slug: "web-ui" }],
        required_checks: { total: 1, satisfied: 1 },
        review_state: "approved",
        checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied" }],
      },
      status_log: [{ message: "Ready for review", created_at: "2026-06-07T12:01:00Z" }],
    },
    "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow": { detail: null },
  });

  await taskHarness.app.load();

  assert.equal(taskHarness.title.textContent, "Task");
  assert.match(taskHarness.content.innerHTML, /Task detail/);
  assert.match(taskHarness.content.innerHTML, /web-ui/);
  assert.deepEqual(taskHarness.fetchCalls, ["/ui/api/v2/projects", "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001", "/ui/api/v2/projects/p-alpha/tasks/t-alpha-0001/workflow", "/ui/api/v2/projects/p-alpha/flows"]);

  const changeHarness = await browserSmokeHarness("/ui/changes/ch-0001", {
    "/ui/api/v2/changes/ch-0001": {
      change: {
        id: "ch-0001",
        branch: "task/t-alpha-0001",
        base: "main",
        head_sha: "abcdef1234567890",
        updated_at: "2026-06-07T12:00:00Z",
      },
      task: { id: "t-alpha-0001", title: "Change detail" },
      checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied" }],
      required_checks: { total: 1, satisfied: 1 },
      review_state: "approved",
      can_merge: true,
      threads: [{
        state: "open",
        file_path: "app.go",
        line: 4,
        anchor_commit_sha: "abcdef1234567890",
        context: "const value = 1",
        comments: [{ body: "Review note" }],
      }],
    },
    "/ui/api/v2/changes/ch-0001/diff": {
      change_id: "ch-0001",
      head_sha: "abcdef1234567890",
      available: true,
      total_files: 1,
      additions: 4,
      deletions: 1,
      files: [{ path: "app.go", additions: 4, deletions: 1 }],
    },
  });

  await changeHarness.app.load();

  assert.equal(changeHarness.title.textContent, "Change");
  assert.match(changeHarness.content.innerHTML, /Change detail/);
  assert.match(changeHarness.content.innerHTML, /Review note/);
  assert.match(changeHarness.diffContainer("ch-0001").innerHTML, /files 1/);
  assert.match(changeHarness.diffContainer("ch-0001").innerHTML, /app.go/);
  assert.deepEqual(changeHarness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/changes/ch-0001",
    "/ui/api/v2/changes/ch-0001/diff",
  ]);
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
  assert.match(nav.innerHTML, /data-job-status="active">6<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="queued">7<\/span>/);
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

test("phase gate panel renders the pending handoff as markdown with approve and request-changes", async () => {
  const context = await scriptContext();
  const flow = {
    phase_name: "plan",
    phase_state: "awaiting_approval",
    pending_handoff: "## Handoff\n- did a thing",
  };
  const html = context.renderPhaseGatePanel(flow, "t-alpha-0001", "p-alpha");
  assert.match(html, /data-phase-gate/);
  assert.match(html, /Phase plan awaiting approval/);
  assert.match(html, /class="human-attention-body md"/);
  assert.match(html, /<h2>Handoff<\/h2>/);
  assert.match(html, /<li>did a thing<\/li>/);
  assert.match(html, /<button class="button" data-phase-approve="t-alpha-0001" data-project="p-alpha">/);
  assert.match(html, /<form class="human-attention-reply" data-phase-request-changes="t-alpha-0001" data-project="p-alpha">/);
  assert.match(html, /<textarea name="feedback"/);
});

test("phase gate panel renders a quiet sent-back note for pending feedback", async () => {
  const context = await scriptContext();
  const flow = { phase_name: "plan", phase_state: "pending", gate_feedback: "tighten the scope" };
  const html = context.renderPhaseGatePanel(flow, "t-alpha-0001", "p-alpha");
  assert.match(html, /data-phase-gate-feedback/);
  assert.match(html, /Phase plan sent back with feedback/);
  assert.match(html, /tighten the scope/);
  assert.doesNotMatch(html, /data-phase-approve/);
});

test("phase gate panel is empty when the phase is running", async () => {
  const context = await scriptContext();
  assert.equal(context.renderPhaseGatePanel({ phase_name: "plan", phase_state: "running" }, "t-alpha-0001", "p-alpha"), "");
  assert.equal(context.renderPhaseGatePanel(null, "t-alpha-0001", "p-alpha"), "");
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

test("phase gate panel renders the pending handoff without a raw pre block", async () => {
  const context = await scriptContext();
  const flow = { phase_name: "plan", phase_state: "awaiting_approval", pending_handoff: "## Plan\n- step one" };
  const html = context.renderPhaseGatePanel(flow, "t-alpha-0001", "p-alpha");
  assert.match(html, /class="human-attention-body md"/);
  assert.match(html, /<h2>Plan<\/h2>/);
  assert.match(html, /<li>step one<\/li>/);
  assert.doesNotMatch(html, /<pre class="human-attention-body"/);
});

test("human attention panel renders the question message as markdown", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Q" };
  const statusLog = [{ id: 7, kind: "question", message: "Pick **one**:\n- a\n- b", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /<strong>one<\/strong>/);
  assert.match(html, /<li>a<\/li>/);
});

test("task read-only detail renders body requirements as markdown but keeps the edit textarea raw", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskReadOnlyDetail(
    { id: "t-alpha-0001", title: "T", body: "## Body\n- item\n\n## Requirements\n- done" },
    { taskID: "t-alpha-0001" },
  );
  assert.match(html, /<h2>Body<\/h2>/);
  assert.match(html, /<li>item<\/li>/);
  assert.match(html, /<h2>Requirements<\/h2>/);
  assert.match(html, /<li>done<\/li>/);
  assert.match(html, /<textarea[^>]*name="body"[^>]*>## Body\n- item\n\n## Requirements\n- done<\/textarea>/);
});

test("task read-only detail shows a dash placeholder for an empty body", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskReadOnlyDetail({ id: "t-alpha-0001", title: "T" }, { taskID: "t-alpha-0001" });
  assert.match(html, /Body<\/span>[\s\S]*?—/);
});

test("task card renders the latest status message as inline markdown", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderTaskCard(
    { id: "t-alpha-0001", title: "T" },
    { latest_status: { message: "**done** `sha`", kind: "progress" } },
    "working",
    false,
  );
  assert.match(html, /<strong>done<\/strong>/);
  assert.match(html, /<code>sha<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>|<pre>/);
});

test("timeline status row renders the message as inline markdown", async () => {
  const context = await scriptContext();
  const html = context.renderTimelineStatusRow({ message: "see `x` now", kind: "note", actor: "agent", created_at: "2026-06-07T12:00:00Z" });
  assert.match(html, /<code>x<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>/);
});

test("thread comment renders the body as markdown", async () => {
  const context = await scriptContext();
  const html = context.renderThreadComment({ actor: "rev", body: "- a\n- b", created_at: "2026-06-07T12:00:00Z" });
  assert.match(html, /class="md"/);
  assert.match(html, /<li>a<\/li>/);
  assert.doesNotMatch(html, /<p>- a/);
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

test("block markdown surfaces do not double-wrap the .md container", async () => {
  const context = await scriptContext();
  const comment = context.renderThreadComment({ actor: "r", body: "## H", created_at: "2026-06-07T12:00:00Z" });
  assert.doesNotMatch(comment, /class="md">\s*<div class="md"/);
  const app = new context.FlowApp();
  const detail = app.renderTaskReadOnlyDetail({ id: "t-alpha-0001", title: "T", body: "## H" }, { taskID: "t-alpha-0001" });
  assert.doesNotMatch(detail, /class="md">\s*<div class="md"/);
  const panel = context.renderPhaseGatePanel({ phase_name: "plan", phase_state: "awaiting_approval", pending_handoff: "## H" }, "t-alpha-0001", "p");
  assert.match(panel, /class="human-attention-body md"/);
  assert.doesNotMatch(panel, /class="md">\s*<div class="md"/);
});

// --- composable flows: task form, board badge, gate + flows editor ------------

test("task form flow select preselects the project default flow", async () => {
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
  assert.match(html, /<option value="fl-plan" selected>planned \(default\)<\/option>/);
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
  assert.match(html, /<option value="fl-plan" >planned \(default\)<\/option>/);
});

test("scheduled board cards show the frozen start step and ignore legacy flow cursors", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const task = { id: "t-alpha-0001", title: "Scheduled", state: "scheduled" };

  const html = app.renderTaskCard(task, {
    current_step: { key: "implement", name: "Implement", kind: "agent" },
    flow: { phase_name: "legacy plan", phase_index: 0, phase_count: 2, phase_state: "running" },
  }, "scheduled", false);

  assert.match(html, /<span class="badge" data-phase="up_next"><span class="dot"><\/span>scheduled<\/span>/);
  assert.match(html, /<div class="card-current-step" title="Implement">Implement<\/div>/);
  assert.doesNotMatch(html, /data-flow-phase|legacy plan|1\/2/);
});

test("wait reason phase_approval maps to a human label", async () => {
  const context = await scriptContext();
  assert.equal(context.waitReasonLabel("phase_approval"), "waiting for phase approval");
  assert.doesNotMatch(context.waitReasonLabel("phase_approval"), /plan/);
});

test("read-only detail renders the flow phase chain from the live flow status", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const flow = {
    flow_name: "planned",
    phases: [
      { name: "spec", gate: "human", agent_name: "Spec Writer" },
      { name: "implement", gate: "auto", agent_name: "Author" },
    ],
  };
  const html = app.renderTaskReadOnlyDetail({ id: "t-alpha-0001", title: "T" }, { taskID: "t-alpha-0001", projectID: "p-alpha", flow });
  assert.match(html, /Flow <strong>planned<\/strong>/);
  assert.match(html, /spec\(gate\) · Spec Writer -> implement · Author/);
});

test("flows editor markup opts into shared form styling and accessible row controls", async () => {
  const context = await scriptContext();
  const agentOptions = [{ name: "codex", display_name: "Codex", models: [] }];

  const agentHTML = context.renderAgentDefFormView(null, agentOptions);
  assert.match(agentHTML, /<form class="agent-def-form task-form"/);
  assert.match(agentHTML, /class="agent-def-model-fields" data-def-model-fields/);

  const inheritedHTML = context.renderAgentDefsSectionView([{ id: "ad-global", name: "shared", harness: "codex", inherited: true }], agentOptions, { editingDefID: "ad-global" });
  assert.match(inheritedHTML, /Project Agent Definitions/);
  assert.match(inheritedHTML, /badge idle">inherited/);
  assert.match(inheritedHTML, /data-edit-def="ad-global">Override/);
  assert.match(inheritedHTML, /name="def_name" value="shared" readonly required/);
  assert.doesNotMatch(inheritedHTML, /data-delete-def="ad-global"/);

  const globalHTML = context.renderGlobalAgentDefsSectionView([], agentOptions, { editingGlobalDefID: "" });
  assert.match(globalHTML, /Global Agent Definitions/);
  assert.match(globalHTML, /Every project inherits/);

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

test("parallel review editors render ordered structured rows without generic JSON", async () => {
  const context = await scriptContext();
  const agentDefs = [
    { id: "ad-code", name: "Code review", harness: "codex", model: "gpt-code" },
    { id: "ad-security", name: "Security review", harness: "claude", model: "opus" },
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
      },
    },
  }, agentDefs);

  assert.equal((html.match(/data-review-agent-row(?:\s|>)/g) || []).length, 2);
  assert.ok(html.indexOf('value="ad-code" selected') < html.indexOf('value="ad-retired" selected'));
  assert.match(html, /<option value="ad-code" selected>Code review — codex \/ gpt-code<\/option>/);
  assert.match(html, /<option value="ad-retired" selected>ad-retired \(unavailable\)<\/option>/);
  assert.equal((html.match(/name="review_agent_blocking" checked/g) || []).length, 1, "omitted blocking defaults to checked");
  assert.match(html, /Blocks approval/);
  assert.match(html, /data-review-agent-advisory >Advisory<\/span>/);
  assert.match(html, /Every listed agent runs and is awaited/);
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
  assert.doesNotMatch(editor.innerHTML, /name="node_config"/);

  kindSelect.value = "verify_change";
  listeners.get("change")({ target: kindSelect });
  assert.match(editor.innerHTML, /data-review-config-key="verify_change"/);
  assert.equal((editor.innerHTML.match(/data-review-agent-row(?:\s|>)/g) || []).length, 1);
  assert.match(editor.innerHTML, /Blocks success/);
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

test("agent def form payload uses the bare model id for codex/claude harnesses", async () => {
  const context = await scriptContext();
  const agentOptions = [{
    name: "claude",
    display_name: "Claude",
    models: [{ provider_id: "anthropic", model_id: "sonnet", qualified_id: "anthropic:sonnet", reasoning: false }],
  }];
  const form = fakeFieldForm({
    def_name: "Author",
    def_harness: "claude",
    def_model: "anthropic:sonnet",
    def_reasoning_effort: "",
    def_prompt: "",
  });

  const payload = context.agentDefPayloadFromFormView(form, agentOptions);

  assert.equal(payload.model, "sonnet");
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
    }],
  });

  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, true);
  blocking.checked = false;
  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, false);
});

test("flows view renders agent definitions and flow tables for the active project", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }] },
    "/ui/api/v2/harnesses": { agents: [{ name: "harness", display_name: "Harness" }], consoles: [] },
    "/ui/api/v2/global/agent-defs": { agent_defs: [{ id: "ad-global", name: "organization-reviewer", harness: "codex" }] },
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
  assert.match(html, /data-agent-def-form/);
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
