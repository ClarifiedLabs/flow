// app.js is the entry module and the composition root: FlowApp wires the
// route table, the pollers, and the per-project caches together and owns the
// shell chrome (top bar, nav, statusbar, shortcuts, delegated actions). The
// pieces with their own state live alongside: app/settle.js (the settle-burst
// state machine), app/sidebar.js (the /v2/sidebar poll loop), app/caches.js
// (the per-project caches), app/routes.js (the route table), and the actions/
// registry+dispatcher. The test surface that used to be this module's
// re-exports lives in test-surface.mjs.
import { value } from "./normalize.js";
import { DEFAULT_AGENT_HARNESSES, DEFAULT_CONSOLE_HARNESSES, NAV } from "./config.js";
import { apiGet, featuresAPIBase, flowsAPIBase, taskAPIBase, workItemsAPIBase } from "./api.js";
import { applyThemePreference, pollConfigForPath, readSelectedProjects, readThemePreference, writeSelectedProjects, writeThemePreference, writeWorkProject } from "./storage.js";
import { renderNavLink, renderNavTrigger, THEME_ICONS, THEME_OPTIONS } from "./nav.js";
import { isWorkPath, workViewHref } from "./work-nav.js";
import { normalizeHarnessOptions } from "./models/harness-catalog.js";
import { openTerminalWindow, closeTerminalDialog, hideInlineTerminal, closeTerminalModalLayers } from "./terminal.js";
import { pollDelay, Poller } from "./poller.js";
import { stopConsolePollView, renderConsoleView } from "./console-view.js";
import { createTaskView } from "./board-route.js";
import { applyBusyState, failureMessage, handleAction, pendingStatus } from "./actions.js";
import { handleFormSubmit } from "./forms.js";
import { renderTaskFormView } from "./task-view.js";
import { openInlineTerminalView, showTranscriptView } from "./terminal-view.js";
import { ROUTES } from "./app/routes.js";
import { SettleBurst } from "./app/settle.js";
import { SidebarStatus } from "./app/sidebar.js";
import { ProjectCaches } from "./app/caches.js";
import "./elements/project-picker.js";

export class FlowApp extends HTMLElement {
  constructor() {
    super();
    this.mainPoll = new Poller();
    this.consolePoll = new Poller();
    this.settle = new SettleBurst({
      isActiveLoad: (context) => this.isActiveLoad(context),
      reload: (options) => this.load(options),
      getGeneration: () => this.loadGeneration || 0,
      getPath: () => window.location.pathname,
      getLoadsInFlight: () => this.loadsInFlight || 0,
      isPollingActive: () => this.pollingActive !== false,
    });
    this.sidebar = new SidebarStatus({
      fetchSidebar: () => apiGet("/v2/sidebar" + this.projectQuery()),
      render: (data) => this.renderSidebarStatus(data),
      hasChrome: () => typeof this.querySelector === "function" && Boolean(this.querySelector(".nav")),
    });
    this.caches = new ProjectCaches();
    // Flows are project-owned: the task form's Flow selector and the
    // read-only flow summary render synchronously from this cache. Mutations
    // invalidate one project's entry (or a view seeds it from its own fetch).
    // The tasks cache feeds the new-task relation picker's suggestions; its
    // fallback keeps the picker in manual-entry mode.
    this.caches.register("flows", {
      fetch: async (id) => {
        const data = await apiGet(flowsAPIBase(id));
        return {
          flows: data.flows || data.Flows || [],
          defaultFlowID: data.default_flow_id || data.DefaultFlowID || "",
        };
      },
      fallback: () => ({ flows: [], defaultFlowID: "" }),
    });
    this.caches.register("features", {
      fetch: async (id) => (await apiGet(featuresAPIBase(id) + "?status=all")).features || [],
      fallback: () => [],
    });
    this.caches.register("workItems", {
      fetch: async (id) => (await apiGet(workItemsAPIBase(id))).items || [],
      fallback: () => [],
    });
    this.caches.register("tasks", {
      fetch: async (id) => (await apiGet(taskAPIBase(id))).tasks || [],
      fallback: () => [],
    });
  }

  connectedCallback() {
    this.pollFailures = 0;
    this.loadGeneration = this.loadGeneration || 0;
    this.changeDiffCache = this.changeDiffCache || new Map();
    this.pollingActive = true;
    this.sidebar.start();
    this.themePreference = readThemePreference();
    this.renderShell();
    this.bindDelegatedActions();
    window.addEventListener("popstate", () => this.load());
    const loadResult = this.load();
    Promise.resolve(loadResult).finally(() => this.refreshSidebarStatus());
  }

  disconnectedCallback() {
    this.pollingActive = false;
    this.loadGeneration = (this.loadGeneration || 0) + 1;
    // A disconnected app owns no settle burst: supersede any burst still in
    // flight so its ticks cannot re-arm after the app went away.
    this.settle.supersede();
    this.clearPolling();
    this.sidebar.stop();
    stopConsolePollView(this);
  }

  renderShell() {
    this.themePreference = applyThemePreference(this.themePreference || readThemePreference());
    const path = (typeof window !== "undefined" && window.location?.pathname) || "/ui/board";
    this.innerHTML = `
      <div class="shell">
        <header class="topbar">
          <a class="brand" href="/ui/board" data-link>flow<span class="brand-cursor">_</span></a>
          <details class="nav-menu">
            <summary class="button secondary nav-trigger">${renderNavTrigger(path, this.sidebarStatus)}</summary>
            <div class="nav-panel">
              <nav class="nav"></nav>
              <div class="nav-footer">
                <span class="nav-footer-label">theme</span>
                <div class="theme-switcher" role="group" aria-label="Theme">
                  ${THEME_OPTIONS.map(([value, label]) => `
                    <button type="button" data-theme-option="${value}" title="${label}" aria-label="${label} theme">${THEME_ICONS[value]}</button>
                  `).join("")}
                </div>
              </div>
            </div>
          </details>
          <h1></h1>
          <div class="topbar-actions">
            <flow-project-picker hidden></flow-project-picker>
            <button class="button" data-action="new-task">New Task</button>
            <button class="button secondary" data-action="refresh">Refresh</button>
          </div>
        </header>
        <main class="main">
          <section class="content"></section>
          <footer class="statusbar" data-state="idle">
            <span class="sb-live"><span class="sb-dot"></span><span class="sb-label">idle</span></span>
            <div class="status" role="status"></div>
            <span class="sb-meta"></span>
          </footer>
        </main>
      </div>
    `;
    this.querySelectorAll("[data-theme-option]").forEach((button) => {
      button.addEventListener("click", () => this.setTheme(button.dataset.themeOption));
    });
    this.syncThemeButtons();
    // One menu open at a time: opening the nav dropdown collapses the project
    // picker and vice versa (the picker reports its own toggle, see
    // renderProjectPicker).
    const navMenu = this.querySelector(".nav-menu");
    navMenu.addEventListener("toggle", () => {
      if (!navMenu.open) return;
      this.querySelector("flow-project-picker")?.close?.();
    });
    this.querySelector('[data-action="refresh"]').addEventListener("click", () => {
      this.refreshSidebarStatus();
      this.load();
    });
    this.querySelector('[data-action="new-task"]').addEventListener("click", () => this.createTask());
    this.renderNav();
  }

  // closeNavMenu collapses the top-bar nav dropdown, reporting whether it
  // closed an open menu so callers (Escape, outside clicks) can stop there.
  closeNavMenu() {
    const menu = typeof this.querySelector === "function" ? this.querySelector(".nav-menu") : null;
    if (!menu || !menu.open) return false;
    menu.open = false;
    return true;
  }

  // handleMenuDismiss closes the nav dropdown when a click lands outside it.
  handleMenuDismiss(event) {
    const menu = typeof this.querySelector === "function" ? this.querySelector(".nav-menu") : null;
    if (!menu || !menu.open) return;
    if (event.target?.closest?.(".nav-menu")) return;
    menu.open = false;
  }

  // One delegated listener for the whole app. Elements own their own innerHTML
  // and repaint on poll, so a listener attached to their children would not
  // survive; a listener on the app root does.
  bindDelegatedActions() {
    if (typeof this.addEventListener !== "function") return;
    if (this.delegatedActionsBound) return;
    this.delegatedActionsBound = true;
    this.addEventListener("click", async (event) => {
      if (event.defaultPrevented) return;
      if (await handleAction(this, event)) return;

      const terminal = event.target?.closest?.("[data-terminal], [data-job-terminal]");
      if (terminal && this.contains(terminal)) {
        event.preventDefault();
        const kind = terminal.dataset.terminal ? "session" : "job";
        await this.openInlineTerminal(terminal, kind, terminal.dataset.terminal || terminal.dataset.jobTerminal);
        return;
      }
      const transcript = event.target?.closest?.("[data-session-transcript], [data-job-transcript]");
      if (transcript && this.contains(transcript)) {
        event.preventDefault();
        const kind = transcript.dataset.sessionTranscript ? "session" : "job";
        await showTranscriptView(this, transcript, kind, transcript.dataset.sessionTranscript || transcript.dataset.jobTranscript);
      }
    });
    this.addEventListener("submit", async (event) => {
      if (event.defaultPrevented) return;
      await handleFormSubmit(this, event);
    });
    this.addEventListener("change", (event) => {
      const picker = event.target?.closest?.("[data-work-project]");
      if (!picker) return;
      const projectID = String(picker.value || "").trim();
      writeWorkProject(projectID);
      const href = workViewHref(picker.dataset.workView || "overview", projectID, window.location.search);
      history.pushState({}, "", href);
      this.load();
    });
    // The board's view toggle is reachable from the keyboard anywhere on the
    // board, and Escape/b always take you back to it.
    document.addEventListener("keydown", (event) => this.handleShortcut(event));
    // Clicks outside the top-bar nav dropdown collapse it.
    document.addEventListener("click", (event) => this.handleMenuDismiss(event));
  }

  handleShortcut(event) {
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    const tag = event.target?.tagName || "";
    if (/^(INPUT|TEXTAREA|SELECT)$/i.test(tag) || event.target?.isContentEditable) return;
    if (event.key === "v") {
      this.querySelector("flow-board")?.toggleView();
      return;
    }
    if (event.key === "b" || event.key === "Escape") {
      // Escape on an open nav dropdown just closes it; the board shortcut
      // still applies once the menu is shut.
      if (event.key === "Escape" && this.closeNavMenu()) return;
      if (window.location.pathname === "/ui/board") return;
      history.pushState({}, "", "/ui/board");
      this.load();
    }
  }

  // refresh re-runs the current route. Action handlers call it rather than
  // knowing which view they were pressed in. A successful action's refresh —
  // one the dispatcher scoped with the ACTION_SETTLE provenance token — also
  // arms a short settle burst of follow-up reloads once its own load
  // completes (see app/settle.js); an ordinary refresh carries no token and
  // stays the single load it always was.
  async refresh(options = {}) {
    const context = await this.load();
    this.settle.maybeArm(options, context);
  }

  renderNav() {
    const nav = this.querySelector(".nav");
    nav.innerHTML = NAV.map(([href, label]) => renderNavLink(href, label, this.sidebarStatus)).join("");
    nav.querySelectorAll("a").forEach((link) => {
      link.addEventListener("click", (event) => {
        event.preventDefault();
        this.closeNavMenu();
        history.pushState({}, "", link.getAttribute("href"));
        this.load();
      });
    });
  }

  renderSidebarStatus(data) {
    this.sidebarStatus = data || {};
    this.renderNav();
    this.updateActiveNav();
  }

  refreshSidebarStatus() {
    return this.sidebar.refresh();
  }

  setTheme(theme) {
    const preference = writeThemePreference(theme);
    this.themePreference = applyThemePreference(preference);
    this.syncThemeButtons();
  }

  syncThemeButtons() {
    this.querySelectorAll("[data-theme-option]").forEach((button) => {
      button.setAttribute("aria-pressed", String(button.dataset.themeOption === this.themePreference));
    });
  }

  // ensureProjects loads the project registry; callers can force a refresh
  // before rendering project-sensitive flows such as task creation.
  async ensureProjects(options = {}) {
    if (this.projects && !options.refresh) return this.projects;
    try {
      const data = await apiGet("/v2/projects");
      this.projects = data.projects || data.Projects || [];
    } catch (error) {
      this.projects = [];
    }
    this.renderProjectPicker();
    return this.projects;
  }

  async ensureHarnesses(options = {}) {
    if (this.harnesses && !options.refresh) return this.harnesses;
    try {
      const data = await apiGet("/v2/harnesses");
      const agents = data.agents || data.Agents;
      const consoles = data.consoles || data.Consoles;
      if (!Array.isArray(agents) || !Array.isArray(consoles)) throw new Error("invalid harness options");
      this.harnesses = {
        agents: normalizeHarnessOptions(agents, []),
        consoles: normalizeHarnessOptions(consoles, []),
      };
      this.harnessesError = null;
    } catch (error) {
      this.harnesses = {
        agents: DEFAULT_AGENT_HARNESSES,
        consoles: DEFAULT_CONSOLE_HARNESSES,
      };
      this.harnessesError = error;
    }
    return this.harnesses;
  }

  // The per-project caches (flows, features, work items, tasks) live in
  // app/caches.js; the ensure* methods keep their call sites reading
  // naturally.
  async ensureFlows(projectID, options = {}) {
    return this.caches.ensure("flows", projectID, options);
  }

  async ensureFeatures(projectID, options = {}) {
    return this.caches.ensure("features", projectID, options);
  }

  async ensureWorkItems(projectID, options = {}) {
    return this.caches.ensure("workItems", projectID, options);
  }

  async ensureTasks(projectID, options = {}) {
    return this.caches.ensure("tasks", projectID, options);
  }

  selectedProjectIDs() {
    const projects = this.projects || [];
    const stored = readSelectedProjects();
    if (!stored.length) return [];
    const known = new Set(projects.map((project) => value(project, "id", "ID")));
    const selected = stored.filter((id) => known.has(id));
    return selected.length === projects.length ? [] : selected;
  }

  projectQuery() {
    const selected = this.selectedProjectIDs();
    if (!selected.length) return "";
    return "?" + selected.map((id) => `project=${encodeURIComponent(id)}`).join("&");
  }

  renderProjectPicker() {
    const picker = this.querySelector("flow-project-picker");
    if (!picker) return;
    const projects = this.projects || [];
    picker.data = {
      projects,
      selected: this.selectedProjectIDs(),
      onOpen: () => this.closeNavMenu(),
      onChange: (ids) => {
        writeSelectedProjects(ids.length === projects.length ? [] : ids);
        this.renderProjectPicker();
        this.refreshSidebarStatus();
        this.load();
      },
    };
  }

  async load(options = {}) {
    this.clearPolling();
    stopConsolePollView(this);
    // Navigation and every other load that is not the active settle burst's
    // own reload supersedes the burst (see app/settle.js).
    this.settle.cancelUnless(options.burst);
    this.updateActiveNav();
    const path = window.location.pathname;
    // Leaving the tasks route drops the retained lifecycle filter (and the
    // deep-link marker), so the next visit re-seeds it from ?state= params or
    // the persisted filter instead of carrying a stale selection across routes
    // — the throughput strip's /ui/tasks?state=done link must win over a
    // filter kept from a previous Tasks visit (see renderTasksView).
    if (path !== "/ui/tasks") {
      this.tasksState = undefined;
      this.tasksStateSearch = undefined;
    }
    if (!options.fromPoll) closeTerminalModalLayers(this);
    const content = this.querySelector(".content");
    if (content && content.dataset) {
      content.dataset.refresh = options.fromPoll ? "poll" : "nav";
    }
    const context = {
      generation: (this.loadGeneration || 0) + 1,
      path,
    };
    this.loadGeneration = context.generation;
    // Track in-flight loads so a settle-burst tick can skip its reload rather
    // than overlap a load that is still running (see app/settle.js).
    this.loadsInFlight = (this.loadsInFlight || 0) + 1;
    try {
      await this.ensureProjects({ refresh: path === "/ui/tasks/new" || path === "/ui/flows" });
      for (const route of ROUTES) {
        const params = route.match(path);
        if (!params) continue;
        if (await route.render(this, context, params) === false) return;
        this.finishLoad(context);
        // A handler-owned reload (the Console view's start and release) arms
        // its settle burst here; refresh() gates itself off the returned
        // context instead (see app/settle.js).
        this.settle.maybeArm(options, context);
        return context;
      }
    } catch (error) {
      if (!this.isActiveLoad(context)) return;
      this.setStatus(failureMessage(error));
      this.pollFailures = options.fromPoll ? this.pollFailures + 1 : 1;
      this.setPollState("error", this.pollFailures > 1 ? `retry ${this.pollFailures}` : "error");
      this.schedulePolling(path);
    } finally {
      this.loadsInFlight -= 1;
    }
  }

  finishLoad(context) {
    if (!this.isActiveLoad(context)) return false;
    this.pollFailures = 0;
    if (pollConfigForPath(context.path)) {
      this.setPollState("live", "live");
    } else {
      this.setPollState("idle", "static");
    }
    this.schedulePolling(context.path);
    // The route render cleared the status line (setTitle) and may have
    // replaced controls whose action is still in flight: put the busy state
    // and the pending message back, so a poll cannot make a running action
    // look finished — or hand back a clickable control for it.
    applyBusyState(this);
    const pending = pendingStatus();
    if (pending) this.setStatus(pending);
    return true;
  }

  setPollState(state, label) {
    const bar = typeof this.querySelector === "function" ? this.querySelector(".statusbar") : null;
    if (!bar) return;
    if (bar.dataset) bar.dataset.state = state;
    const labelElement = bar.querySelector ? bar.querySelector(".sb-label") : null;
    if (labelElement) labelElement.textContent = label;
  }

  isActiveLoad(context) {
    return this.pollingActive !== false
      && context
      && context.generation === this.loadGeneration
      && window.location.pathname === context.path;
  }

  clearPolling() {
    this.mainPoll.clear();
  }

  schedulePolling(path) {
    this.clearPolling();
    const config = pollConfigForPath(path);
    const meta = typeof this.querySelector === "function" ? this.querySelector(".sb-meta") : null;
    if (!config) {
      if (meta) meta.textContent = "";
      return;
    }
    const delay = pollDelay(config.interval, config.backoff ? this.pollFailures : 0, config.maxInterval);
    if (meta) meta.textContent = `poll ${Math.round(delay / 1000)}s`;
    this.mainPoll.arm(delay, () => this.load({ fromPoll: true }));
  }

  updateActiveNav() {
    const path = window.location.pathname;
    this.querySelectorAll(".nav a").forEach((link) => {
      const href = link.getAttribute("href");
      const active = href === path || (href === "/ui/board" && (path === "/ui" || path === "/ui/")) || (href === "/ui/work-items" && isWorkPath(path));
      if (active) {
        link.setAttribute("aria-current", "page");
      } else {
        link.removeAttribute("aria-current");
      }
    });
    // The trigger mirrors the route (label) and the latest board lane chips.
    const trigger = typeof this.querySelector === "function" ? this.querySelector(".nav-trigger") : null;
    if (trigger) trigger.innerHTML = renderNavTrigger(path, this.sidebarStatus);
  }

  createTask() {
    return createTaskView(this);
  }

  renderTaskForm(task, options) {
    return renderTaskFormView(this, task, options);
  }

  renderConsole(context) {
    return renderConsoleView(this, context);
  }

  openInlineTerminal(button, kind, id) {
    return openInlineTerminalView(this, button, kind, id);
  }

  setTitle(title) {
    this.querySelector("h1").textContent = title;
    this.setStatus("");
  }

  setStatus(message) {
    this.querySelector(".status").textContent = message;
  }
}

// Legacy *ByProject Map views over the per-project caches. Readers
// (task-view.js, tasks-view.js, create-relations.js) and tests still name the
// Maps directly; writes go through app.caches. get forwards to the live store
// and set re-seats it, so assigning a fresh Map (the test suites' habit)
// keeps the cache and the view in sync.
for (const [field, kind] of [
  ["flowsByProject", "flows"],
  ["featuresByProject", "features"],
  ["workItemsByProject", "workItems"],
  ["tasksByProject", "tasks"],
]) {
  Object.defineProperty(FlowApp.prototype, field, {
    get() {
      return this.caches.store(kind);
    },
    set(map) {
      this.caches.setStore(kind, map);
    },
  });
}

customElements.define("flow-app", FlowApp);

if (typeof globalThis !== "undefined") {
  globalThis.FlowApp = FlowApp;
}

document.addEventListener("click", (event) => {
  const popOut = event.target?.closest?.("[data-terminal-popout]");
  if (popOut) {
    event.preventDefault();
    openTerminalWindow(popOut.dataset.terminalPopout);
    return;
  }

  const close = event.target?.closest?.("[data-terminal-close]");
  if (close) {
    event.preventDefault();
    closeTerminalDialog(close);
    return;
  }

  const hide = event.target?.closest?.("[data-terminal-hide]");
  if (hide) {
    event.preventDefault();
    hideInlineTerminal(hide);
    return;
  }

  const link = event.target?.closest?.("a[data-link]");
  if (!link) return;
  event.preventDefault();
  history.pushState({}, "", link.getAttribute("href"));
  document.querySelector("flow-app").load();
});

document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  const modal = document.querySelector?.("[data-terminal-modal-layer]");
  if (!modal) return;
  event.preventDefault();
  modal.remove();
});
