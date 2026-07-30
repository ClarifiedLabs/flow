// app.js is the entry module: it imports what the FlowApp controller uses and
// re-exports each split-out module's API, so the whole UI surface stays
// reachable from this one entry point (and the browser loads the module graph
// by following these imports).
import { value } from "./normalize.js";
import { escapeHTML, escapeAttr } from "./html.js";
import { NAV, SIDEBAR_STATUS_POLL_MS, MAX_POLL_BACKOFF_MS, DEFAULT_AGENT_HARNESSES, DEFAULT_CONSOLE_HARNESSES } from "./config.js";
import { apiGet, apiPost, taskConsoleAPIPath, taskAPIBase, taskHref, flowsAPIBase } from "./api.js";
import { readSelectedProjects, writeSelectedProjects, terminalSessionIDForPath, pollConfigForPath, readThemePreference, writeThemePreference, applyThemePreference } from "./storage.js";
import { renderNavLink, renderNavTrigger, THEME_ICONS, THEME_OPTIONS } from "./nav.js";
import { normalizeHarnessOptions } from "./harness-models.js";
import { openTerminalWindow, closeTerminalDialog, hideInlineTerminal, closeTerminalModalLayers } from "./terminal.js";
import { pollDelay, Poller } from "./poller.js";
import { renderWorkersView, renderJobsView } from "./diagnostics-view.js";
import { renderTerminalView, openInlineTerminalView, showTranscriptView } from "./terminal-view.js";
import { renderConsoleView, stopConsolePollView } from "./console-view.js";
import { renderDoneView } from "./done-view.js";
import { createTaskView, renderBoardRoute } from "./board-route.js";
import { renderTaskRoute } from "./task-route.js";
import { renderChangeRoute } from "./change-route.js";
import { renderEpicRoute } from "./epic-route.js";
import { applyBusyState, handleAction, pendingStatus } from "./actions.js";
import { handleFormSubmit } from "./forms.js";
import { renderNewTaskView, renderTaskFormView, bindTaskFlowControlsView } from "./task-view.js";
import { renderFlowsView } from "./flows-view.js";

export * from "./normalize.js";
export * from "./html.js";
export * from "./markdown.js";
export * from "./format.js";
export * from "./config.js";
export * from "./api.js";
export * from "./storage.js";
export * from "./nav.js";
export * from "./harness-models.js";
export * from "./terminal.js";
export * from "./board.js";
export * from "./board-model.js";
export * from "./task-model.js";
export * from "./queue.js";
export * from "./diagnostics-view.js";
export * from "./diff.js";
export * from "./timeline.js";
export * from "./attention.js";
export * from "./task.js";
export * from "./poller.js";
export * from "./flows-view.js";
export * from "./workflow-graph.js";

// Client-side route table consumed by load(). Each entry's match(path) returns a
// truthy params object/flag when it handles the path, or a falsy value to fall
// through to the next entry. Order matters: specific paths first, the board as
// the catch-all last. render() receives the app instance, the load context and
// the matched params.
const ROUTES = [
  { match: (p) => p === "/ui/tasks/new", render: (app, ctx) => renderNewTaskView(app, ctx) },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/tasks\/([^/]+)$/);
      return m && { task: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderTaskRoute(app, p.task, ctx),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/tasks\/([^/]+)$/);
      return m && { project: decodeURIComponent(m[1]), task: decodeURIComponent(m[2]) };
    },
    render: (app, ctx, p) => renderTaskRoute(app, p.task, ctx, p.project),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/tasks\/([^/]+)\/epic$/);
      return m && { task: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderEpicRoute(app, p.task, ctx),
  },
  { match: (p) => p.startsWith("/ui/changes/") && { id: p.split("/").pop() }, render: (app, ctx, p) => renderChangeRoute(app, p.id, ctx) },
  { match: (p) => p === "/ui/console", render: (app, ctx) => app.renderConsole(ctx) },
  { match: (p) => { const id = terminalSessionIDForPath(p); return id && { id }; }, render: (app, ctx, p) => renderTerminalView(app, p.id, ctx) },
  { match: (p) => p === "/ui/flows", render: (app, ctx) => renderFlowsView(app, ctx) },
  { match: (p) => p === "/ui/workers", render: (app, ctx) => renderWorkersView(app, ctx) },
  { match: (p) => p === "/ui/jobs", render: (app, ctx) => renderJobsView(app, ctx) },
  { match: (p) => p === "/ui/done", render: (app, ctx) => renderDoneView(app, ctx) },
  { match: () => true, render: (app, ctx) => renderBoardRoute(app, ctx) },
];

export class FlowApp extends HTMLElement {
  constructor() {
    super();
    this.mainPoll = new Poller();
    this.sidebarPoll = new Poller();
    this.consolePoll = new Poller();
  }

  connectedCallback() {
    this.pollFailures = 0;
    this.sidebarStatusFailures = 0;
    this.sidebarStatusGeneration = this.sidebarStatusGeneration || 0;
    this.loadGeneration = this.loadGeneration || 0;
    this.changeDiffCache = this.changeDiffCache || new Map();
    this.pollingActive = true;
    this.sidebarStatusPollingActive = true;
    this.themePreference = readThemePreference();
    this.renderShell();
    this.bindDelegatedActions();
    window.addEventListener("popstate", () => this.load());
    const loadResult = this.load();
    Promise.resolve(loadResult).finally(() => this.refreshSidebarStatus());
  }

  disconnectedCallback() {
    this.pollingActive = false;
    this.sidebarStatusPollingActive = false;
    this.loadGeneration = (this.loadGeneration || 0) + 1;
    this.sidebarStatusGeneration = (this.sidebarStatusGeneration || 0) + 1;
    this.clearPolling();
    this.clearSidebarStatusPolling();
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
            <details class="project-picker" hidden>
              <summary class="button secondary"></summary>
              <div class="project-picker-menu"></div>
            </details>
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
    // picker and vice versa.
    const navMenu = this.querySelector(".nav-menu");
    navMenu.addEventListener("toggle", () => {
      if (!navMenu.open) return;
      const picker = this.querySelector(".project-picker");
      if (picker) picker.open = false;
    });
    const projectPicker = this.querySelector(".project-picker");
    projectPicker.addEventListener("toggle", () => {
      if (!projectPicker.open) return;
      this.closeNavMenu();
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
  // knowing which view they were pressed in.
  refresh() {
    return this.load();
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

  async refreshSidebarStatus() {
    if (this.sidebarStatusPollingActive === false) return false;
    const nav = typeof this.querySelector === "function" ? this.querySelector(".nav") : null;
    if (!nav) return false;
    this.clearSidebarStatusPolling();
    const context = {
      generation: (this.sidebarStatusGeneration || 0) + 1,
    };
    this.sidebarStatusGeneration = context.generation;

    try {
      const data = await apiGet("/v2/sidebar" + this.projectQuery());
      if (!this.isActiveSidebarStatus(context)) return false;
      this.renderSidebarStatus(data);
      this.sidebarStatusFailures = 0;
      this.scheduleSidebarStatusPolling();
      return true;
    } catch (error) {
      if (!this.isActiveSidebarStatus(context)) return false;
      this.sidebarStatusFailures = (this.sidebarStatusFailures || 0) + 1;
      this.scheduleSidebarStatusPolling();
      return false;
    }
  }

  isActiveSidebarStatus(context) {
    return this.sidebarStatusPollingActive !== false
      && context
      && context.generation === this.sidebarStatusGeneration;
  }

  clearSidebarStatusPolling() {
    this.sidebarPoll.clear();
  }

  scheduleSidebarStatusPolling() {
    if (this.sidebarStatusPollingActive === false) return;
    const nav = typeof this.querySelector === "function" ? this.querySelector(".nav") : null;
    if (!nav) return;
    const delay = pollDelay(SIDEBAR_STATUS_POLL_MS, this.sidebarStatusFailures, MAX_POLL_BACKOFF_MS);
    this.sidebarPoll.arm(delay, () => this.refreshSidebarStatus());
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

  // ensureFlows loads (and per-project caches) a project's flows so the task
  // form's Flow selector and the read-only flow summary can render
  // synchronously. Flows are project-owned, so the cache is keyed by project
  // id; pass { refresh: true } after a mutation to invalidate one project.
  async ensureFlows(projectID, options = {}) {
    const id = String(projectID || "").trim();
    if (!id) return { flows: [], defaultFlowID: "" };
    if (!this.flowsByProject) this.flowsByProject = new Map();
    if (this.flowsByProject.has(id) && !options.refresh) return this.flowsByProject.get(id);
    let result;
    try {
      const data = await apiGet(flowsAPIBase(id));
      result = {
        flows: data.flows || data.Flows || [],
        defaultFlowID: data.default_flow_id || data.DefaultFlowID || "",
      };
    } catch (error) {
      result = { flows: [], defaultFlowID: "" };
    }
    this.flowsByProject.set(id, result);
    return result;
  }

  // ensureTasks loads (and per-project caches) a project's task list so the
  // new-task relation picker can offer target-task suggestions synchronously.
  // Like ensureFlows the cache is keyed by project id; a failed or empty fetch
  // caches an empty list so the picker falls back to manual entry instead of
  // re-fetching on every render.
  async ensureTasks(projectID, options = {}) {
    const id = String(projectID || "").trim();
    if (!id) return [];
    if (!this.tasksByProject) this.tasksByProject = new Map();
    if (this.tasksByProject.has(id) && !options.refresh) return this.tasksByProject.get(id);
    let tasks;
    try {
      const data = await apiGet(taskAPIBase(id));
      tasks = data.tasks || data.Tasks || [];
    } catch (error) {
      tasks = [];
    }
    this.tasksByProject.set(id, tasks);
    return tasks;
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
    const picker = this.querySelector(".project-picker");
    if (!picker) return;
    const projects = this.projects || [];
    if (projects.length <= 1) {
      picker.hidden = true;
      return;
    }
    picker.hidden = false;
    const selected = this.selectedProjectIDs();
    const selectedSet = new Set(selected);
    const summary = picker.querySelector("summary");
    summary.textContent = selected.length ? `Projects: ${selected.length}/${projects.length}` : "Projects: All";
    const menu = picker.querySelector(".project-picker-menu");
    menu.innerHTML = `
      <label class="project-picker-item"><input type="checkbox" data-project-all ${selected.length ? "" : "checked"}><span>All projects</span></label>
      ${projects.map((project) => {
        const id = value(project, "id", "ID");
        const name = value(project, "name", "Name") || id;
        const checked = !selected.length || selectedSet.has(id);
        return `<label class="project-picker-item"><input type="checkbox" data-project-option="${escapeAttr(id)}" ${checked ? "checked" : ""}><span>${escapeHTML(name)}</span></label>`;
      }).join("")}
    `;
    menu.querySelector("[data-project-all]").addEventListener("change", () => {
      writeSelectedProjects([]);
      this.renderProjectPicker();
      this.refreshSidebarStatus();
      this.load();
    });
    menu.querySelectorAll("[data-project-option]").forEach((input) => {
      input.addEventListener("change", () => {
        const ids = Array.from(menu.querySelectorAll("[data-project-option]"))
          .filter((option) => option.checked)
          .map((option) => option.dataset.projectOption);
        writeSelectedProjects(ids.length === projects.length ? [] : ids);
        this.renderProjectPicker();
        this.refreshSidebarStatus();
        this.load();
      });
    });
  }

  async load(options = {}) {
    this.clearPolling();
    stopConsolePollView(this);
    this.updateActiveNav();
    const path = window.location.pathname;
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
    try {
      await this.ensureProjects({ refresh: path === "/ui/tasks/new" || path === "/ui/flows" });
      for (const route of ROUTES) {
        const params = route.match(path);
        if (!params) continue;
        if (await route.render(this, context, params) === false) return;
        this.finishLoad(context);
        return;
      }
    } catch (error) {
      if (!this.isActiveLoad(context)) return;
      this.setStatus(error.message || String(error));
      this.pollFailures = options.fromPoll ? this.pollFailures + 1 : 1;
      this.setPollState("error", this.pollFailures > 1 ? `retry ${this.pollFailures}` : "error");
      this.schedulePolling(path);
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
      const active = href === path || (href === "/ui/board" && (path === "/ui" || path === "/ui/"));
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

  // refreshBoardDoneLane re-fetches and replaces only the board's Done column,
  // leaving the four active lanes (and the board poll) untouched.

  createTask() {
    return createTaskView(this);
  }


  // doneQuery combines the active project selection with the outcome filter and
  // any extra params (cursor, single-project scope for load-more).

  // appendDoneData flattens an aggregate /v2/done page onto the accumulator and
  // records each project's keyset cursor (or clears it when exhausted).

  // loadMoreDone fetches the next (older) page for every project that still has
  // a cursor, scoping each request to that project so keyset paging stays exact.

  renderTaskForm(task, options) {
    return renderTaskFormView(this, task, options);
  }


  renderConsole(context) {
    return renderConsoleView(this, context);
  }

  renderFlows(context) {
    return renderFlowsView(this, context);
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
