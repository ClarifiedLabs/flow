// App shell tests: theme switching, the top-bar nav dropdown, sidebar
// status badges, shortcuts, and the project picker. Harnesses are local;
// smoke DOM stubs come from test-helpers.mjs.

import assert from "node:assert/strict";
import { test } from "node:test";
import { SmokeDetails, SmokeElement, SmokeLink, SmokeNav, scriptContext } from "./test-helpers.mjs";

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
    ["/ui/tasks", "work"],
    ["/ui/console", "console"],
    ["/ui/done", "done"],
    ["/ui/flows", "flows"],
    ["/ui/workers", "workers"],
    ["/ui/jobs", "jobs"],
    ["/ui/work-items", "work"],
    ["/ui/features", "work"],
    ["/ui/tasks/new", "work"],
    ["/ui/tasks/t-0001", "work"],
    ["/ui/tasks/t-0001/epic", "work"],
    ["/ui/projects/p-alpha/tasks/t-0001", "work"],
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
  // The panel keeps every configured nav destination with the board badge markup.
  assert.equal(harness.nav.links.length, harness.context.NAV.length);
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
  // The project-picker element reports its own toggle through its data port
  // (the toggle event does not bubble out of the element's details).
  harness.app.projects = [{ id: "p-alpha" }, { id: "p-beta" }];
  harness.app.renderProjectPicker();
  harness.pickerElement.data.onOpen();
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

// tasks-route harness pieces: a mount-compatible content stub and a
// document.createElement factory that builds a real FlowTasks (no listeners —
// the content stub never connects it, so bind() never runs).
// tasksRouteImports dynamically imports the route/element modules: their
// import chain defines custom-element classes, which needs a global
// HTMLElement — only present after a scriptContext call has installed one.

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

test("Work sidebar status badge shows the unscheduled count", async () => {
  const context = await scriptContext();
  const html = context.renderNavStatus("/ui/work-items", {
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
  app.sidebar.pollingActive = true;
  app.renderShell();

  await app.refreshSidebarStatus();

  assert.deepEqual(fetchCalls, ["/ui/api/v2/sidebar"]);
  assert.match(nav.innerHTML, /title="8 done items">8<\/span>/);
  assert.match(nav.innerHTML, /title="2 in use of 5 worker slots">2\/5<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="active" title="6 active jobs">6<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="queued" title="7 queued jobs">7<\/span>/);
  assert.equal(timers[0].delay, 10000);
});


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
  // The <flow-project-picker> element's shell-facing surface: close() for the
  // nav menu's "one menu open at a time" coordination, and a data port whose
  // onOpen the element calls when its own details toggles.
  const pickerElement = new SmokeElement();
  pickerElement.close = () => {
    picker.open = false;
  };
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
      if (selector === "flow-project-picker") return pickerElement;
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
  app.sidebar.pollingActive = true;
  app.renderShell();

  return { app, context, nav, navMenu, trigger, picker, pickerElement, pushed, storage, themeButtons };
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

