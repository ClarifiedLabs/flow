// app.js is the entry module: it imports what the FlowApp controller uses and
// re-exports each split-out module's API, so the whole UI surface stays
// reachable from this one entry point (and the browser loads the module graph
// by following these imports).
import { value } from "./normalize.js";
import { escapeHTML, escapeAttr } from "./html.js";
import { NAV, SIDEBAR_STATUS_POLL_MS, MAX_POLL_BACKOFF_MS, DEFAULT_AGENT_HARNESSES, DEFAULT_CONSOLE_HARNESSES } from "./config.js";
import { apiGet, apiPost, apiPatch, apiDelete, issueConsoleAPIPath, issueAPIBase, issueHref } from "./api.js";
import { readSelectedProjects, writeSelectedProjects, writeIssueAgentDefaults, routeFilter, terminalSessionIDForPath, pollConfigForPath, readThemePreference, writeThemePreference, applyThemePreference, readDiffMode, writeDiffMode } from "./storage.js";
import { renderNavLink, THEME_ICONS, THEME_OPTIONS } from "./nav.js";
import { normalizeHarnessOptions } from "./harness-models.js";
import { openTerminalWindow, closeTerminalDialog, hideInlineTerminal, closeTerminalModalLayers } from "./terminal.js";
import { uploadIssueAttachment } from "./issue.js";
import { pollDelay, Poller } from "./poller.js";
import { renderWorkersView, renderJobsView } from "./diagnostics-view.js";
import { renderChangeView, renderChangeDiffView } from "./change-view.js";
import { renderDiffSummary } from "./diff.js";
import { renderTerminalView, openInlineTerminalView, showTranscriptView } from "./terminal-view.js";
import { renderConsoleView, stopConsolePollView, startConsoleView, releaseConsoleView } from "./console-view.js";
import { renderDoneView } from "./done-view.js";
import { renderBoardView, createIssueView, renderIssueCardView } from "./board-view.js";
import { renderNewIssueView, renderIssueFormView, renderIssueReadOnlyDetailView, renderIssueView, toggleIssueEditFormView, bindAgentArgControlsView, issueAgentPayloadFromFormView, issueAgentDefaultsFromFormView, bindHarnessModelControlsView } from "./issue-view.js";

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
export * from "./queue.js";
export * from "./diagnostics-view.js";
export * from "./diff.js";
export * from "./timeline.js";
export * from "./attention.js";
export * from "./issue.js";
export * from "./poller.js";

// Client-side route table consumed by load(). Each entry's match(path) returns a
// truthy params object/flag when it handles the path, or a falsy value to fall
// through to the next entry. Order matters: specific paths first, the board as
// the catch-all last. render() receives the app instance, the load context and
// the matched params.
const ROUTES = [
  { match: (p) => p === "/ui/issues/new", render: (app, ctx) => renderNewIssueView(app, ctx) },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/issues\/([^/]+)$/);
      return m && { project: decodeURIComponent(m[1]), issue: decodeURIComponent(m[2]) };
    },
    render: (app, ctx, p) => app.renderIssue(p.issue, ctx, p.project),
  },
  { match: (p) => p.startsWith("/ui/issues/") && { unscopedIssue: true }, render: (app) => renderUnscopedIssueRoute(app) },
  { match: (p) => p.startsWith("/ui/changes/") && { id: p.split("/").pop() }, render: (app, ctx, p) => app.renderChange(p.id, ctx) },
  { match: (p) => p === "/ui/console", render: (app, ctx) => app.renderConsole(ctx) },
  { match: (p) => { const id = terminalSessionIDForPath(p); return id && { id }; }, render: (app, ctx, p) => renderTerminalView(app, p.id, ctx) },
  { match: (p) => p === "/ui/workers", render: (app, ctx) => renderWorkersView(app, ctx) },
  { match: (p) => p === "/ui/jobs", render: (app, ctx) => renderJobsView(app, ctx) },
  { match: (p) => p === "/ui/done", render: (app, ctx) => renderDoneView(app, ctx) },
  { match: () => true, render: (app, ctx) => renderBoardView(app, routeFilter(ctx.path), ctx) },
];

function renderUnscopedIssueRoute(app) {
  app.setTitle("Issue");
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <h2>Project-scoped issue URL required</h2>
      <p class="meta-quiet">Issue IDs are scoped to projects. Use /ui/projects/&lt;project-id&gt;/issues/&lt;issue-id&gt;.</p>
    </section>
  `;
  return true;
}

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
    this.innerHTML = `
      <div class="shell">
        <aside class="sidebar">
          <p class="brand">flow<span class="brand-cursor">_</span></p>
          <nav class="nav"></nav>
          <div class="theme-switcher" role="group" aria-label="Theme">
            ${THEME_OPTIONS.map(([value, label]) => `
              <button type="button" data-theme-option="${value}" title="${label}" aria-label="${label} theme">${THEME_ICONS[value]}</button>
            `).join("")}
          </div>
        </aside>
        <main class="main">
          <div class="topbar">
            <h1></h1>
            <div class="topbar-actions">
              <details class="project-picker" hidden>
                <summary class="button secondary"></summary>
                <div class="project-picker-menu"></div>
              </details>
              <button class="button" data-action="new-issue">New Issue</button>
              <button class="button secondary" data-action="refresh">Refresh</button>
            </div>
          </div>
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
    this.querySelector('[data-action="refresh"]').addEventListener("click", () => {
      this.refreshSidebarStatus();
      this.load();
    });
    this.querySelector('[data-action="new-issue"]').addEventListener("click", () => this.createIssue());
    this.renderNav();
  }

  bindDelegatedActions() {
    if (typeof this.addEventListener !== "function") return;
    if (this.delegatedActionsBound) return;
    this.delegatedActionsBound = true;
    this.addEventListener("click", async (event) => {
      const button = event.target?.closest?.("[data-human-review-approve]");
      if (!button || !this.contains(button) || event.defaultPrevented) return;
      event.preventDefault();
      await this.approveHumanReview(button, () => this.load());
    });
    this.addEventListener("click", async (event) => {
      const start = event.target?.closest?.("[data-start-console]");
      if (start && this.contains(start) && !event.defaultPrevented) {
        event.preventDefault();
        const harness = this.querySelector("[data-console-harness]")?.value || "claude";
        await this.startConsole(start.dataset.project || "", harness, start.dataset.issue || "");
        return;
      }

      const release = event.target?.closest?.("[data-release-console]");
      if (release && this.contains(release) && !event.defaultPrevented) {
        event.preventDefault();
        await releaseConsoleView(this, release.dataset.project || "", release.dataset.issue || "");
      }
    });
  }

  renderNav() {
    const nav = this.querySelector(".nav");
    nav.innerHTML = NAV.map(([href, label]) => renderNavLink(href, label, this.sidebarStatus)).join("");
    nav.querySelectorAll("a").forEach((link) => {
      link.addEventListener("click", (event) => {
        event.preventDefault();
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
      const data = await apiGet("/v1/sidebar" + this.projectQuery());
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
  // before rendering project-sensitive flows such as issue creation.
  async ensureProjects(options = {}) {
    if (this.projects && !options.refresh) return this.projects;
    try {
      const data = await apiGet("/v1/projects");
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
      const data = await apiGet("/v1/harnesses");
      const agents = data.agents || data.Agents;
      const consoles = data.consoles || data.Consoles;
      if (!Array.isArray(agents) || !Array.isArray(consoles)) throw new Error("invalid harness options");
      this.harnesses = {
        agents: normalizeHarnessOptions(agents, []),
        consoles: normalizeHarnessOptions(consoles, []),
      };
    } catch (error) {
      this.harnesses = {
        agents: DEFAULT_AGENT_HARNESSES,
        consoles: DEFAULT_CONSOLE_HARNESSES,
      };
    }
    return this.harnesses;
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
      await this.ensureProjects({ refresh: path === "/ui/issues/new" });
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
  }

  // refreshBoardDoneLane re-fetches and replaces only the board's Done column,
  // leaving the four active lanes (and the board poll) untouched.

  createIssue() {
    return createIssueView(this);
  }

  renderIssueCard(issue, card, laneState, blocked, stagger, project, waitReason) {
    return renderIssueCardView(this, issue, card, laneState, blocked, stagger, project, waitReason);
  }

  // doneQuery combines the active project selection with the outcome filter and
  // any extra params (cursor, single-project scope for load-more).

  // appendDoneData flattens an aggregate /v1/done page onto the accumulator and
  // records each project's keyset cursor (or clears it when exhausted).

  // loadMoreDone fetches the next (older) page for every project that still has
  // a cursor, scoping each request to that project so keyset paging stays exact.

  renderIssueForm(issue, options) {
    return renderIssueFormView(this, issue, options);
  }

  // renderIssueReadOnlyDetail renders the title, body, acceptance criteria and
  // agent configuration as a read-only summary with an Edit button that reveals
  // the full edit form. Replacing the always-visible form with this collapsed
  // view keeps the editor column short, so the (now timeline-merged) sessions
  // list can no longer overwhelm/cover the agent config and the issue text.
  renderIssueReadOnlyDetail(issue, options) {
    return renderIssueReadOnlyDetailView(this, issue, options);
  }

  renderIssue(id, context, projectID) {
    return renderIssueView(this, id, context, projectID);
  }

  renderChange(id, context) {
    return renderChangeView(this, id, context);
  }

  renderChangeDiff(changeID, headSHA, context) {
    return renderChangeDiffView(this, changeID, headSHA, context);
  }

  renderConsole(context) {
    return renderConsoleView(this, context);
  }

  startConsole(projectID, harness, issueID) {
    return startConsoleView(this, projectID, harness, issueID);
  }

  bindIssueActions(refresh) {
    this.installLifecycleActions(refresh);
    this.installReviewActions(refresh);
    this.installConsoleActions(refresh);
    this.installThreadActions(refresh);
    this.installFormActions(refresh);
    this.installTerminalActions();
    this.installToggleActions();
  }

  // installLifecycleActions wires issue lifecycle transitions (merge/triage/schedule/close/pause/resume/state).
  installLifecycleActions(refresh) {
    this.querySelectorAll("[data-merge]").forEach((button) => {
      button.addEventListener("click", async () => {
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.merge)}/merge`, {});
        await refresh();
      });
    });
    this.querySelectorAll("[data-triage]").forEach((button) => {
      button.addEventListener("click", async () => {
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.issue)}/triage`, { state: button.dataset.triage });
        await refresh();
      });
    });
    this.querySelectorAll("[data-issue-edit]").forEach((button) => {
      button.addEventListener("click", async () => {
        const nextTitle = window.prompt("Title", button.dataset.issueTitle || "");
        if (nextTitle === null) return;
        const title = nextTitle.trim();
        if (!title) {
          this.setStatus("Issue title is required");
          return;
        }
        try {
          await apiPatch(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.issueEdit)}`, { title });
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-schedule]").forEach((button) => {
      button.addEventListener("click", async () => {
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.issue)}/schedule`, { state: button.dataset.schedule });
        await refresh();
      });
    });
    this.querySelectorAll("[data-close]").forEach((button) => {
      button.addEventListener("click", async () => {
        if (!window.confirm("Close this issue?")) return;
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.close)}/close`, {});
        await refresh();
      });
    });
    this.querySelectorAll("[data-pause]").forEach((button) => {
      button.addEventListener("click", async () => {
        if (!window.confirm("Pause this task?")) return;
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.pause)}/pause`, {});
        await refresh();
      });
    });
    this.querySelectorAll("[data-resume]").forEach((button) => {
      button.addEventListener("click", async () => {
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.resume)}/resume`, {});
        await refresh();
      });
    });
    this.querySelectorAll("[data-retry-crash]").forEach((button) => {
      button.addEventListener("click", async () => {
        await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.retryCrash)}/retry`, {});
        await refresh();
      });
    });
    this.querySelectorAll("[data-issue-state-form]").forEach((form) => {
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const issueID = form.dataset.issueStateForm;
        const state = form.elements.state.value;
        try {
          await apiPost(`${issueAPIBase(form.dataset.project)}/${encodeURIComponent(issueID)}/state`, { state });
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
  }

  // installReviewActions wires review, plan and human-review approval actions.
  installReviewActions(refresh) {
    this.querySelectorAll("[data-merge-change]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          await apiPost(`/v1/changes/${encodeURIComponent(button.dataset.mergeChange)}/merge`, {});
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-review-run]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.reviewRun)}/review/run`, {});
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-review-cycles-approve]").forEach((button) => {
      button.addEventListener("click", async () => {
        const instructions = (window.prompt("Instructions for the next author run") || "").trim();
        if (!instructions) {
          this.setStatus("Approval instructions are required");
          return;
        }
        try {
          await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.reviewCyclesApprove)}/review-cycles/approve`, { instructions });
          await refresh();
          this.setStatus("review cycles approved");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-plan-approve]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.planApprove)}/plan/approve`, {});
          await refresh();
          this.setStatus("plan approved");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-plan-reject]").forEach((button) => {
      button.addEventListener("click", async () => {
        const comments = (window.prompt("Plan rejection comments") || "").trim();
        if (!comments) {
          this.setStatus("Rejection comments are required");
          return;
        }
        try {
          await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.planReject)}/plan/reject`, { comments });
          await refresh();
          this.setStatus("plan rejected");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-human-review-approve]").forEach((button) => {
      button.addEventListener("click", async (event) => {
        event?.preventDefault?.();
        await this.approveHumanReview(button, refresh);
      });
    });
  }

  // installConsoleActions wires issue console start/release.
  installConsoleActions(refresh) {
    this.querySelectorAll("[data-start-issue-console]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          await apiPost(issueConsoleAPIPath(button.dataset.project, button.dataset.startIssueConsole), { harness: "claude" });
          await refresh();
          this.setStatus("issue console starting");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-release-issue-console]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          await apiDelete(issueConsoleAPIPath(button.dataset.project, button.dataset.releaseIssueConsole));
          await refresh();
          this.setStatus("issue console released");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
  }

  // installThreadActions wires review-thread and human-attention reply actions.
  installThreadActions(refresh) {
    this.querySelectorAll("[data-attention-reply-form]").forEach((form) => {
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const message = (form.elements.message?.value || "").trim();
        if (!message) {
          this.setStatus("Reply message is required");
          return;
        }
        const statusLogID = Number(form.dataset.statusLogId || 0);
        const payload = { message };
        if (statusLogID > 0) payload.status_log_id = statusLogID;
        try {
          await apiPost(`${issueAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.attentionReplyForm)}/attention/reply`, payload);
          await refresh();
          this.setStatus("reply sent");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-thread-claim]").forEach((button) => {
      button.addEventListener("click", async () => {
        const kind = button.dataset.claimKind;
        const body = kind === "fixed" ? "" : (window.prompt("Rationale") || "").trim();
        if (kind !== "fixed" && !body) {
          this.setStatus("Thread claim rationale is required");
          return;
        }
        try {
          await apiPost(`/v1/threads/${encodeURIComponent(button.dataset.threadClaim)}/claims`, {
            kind,
            body,
            claim_commit_sha: button.dataset.claimCommit || "",
          });
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-thread-reply]").forEach((button) => {
      button.addEventListener("click", async () => {
        const body = (window.prompt("Reply") || "").trim();
        if (!body) {
          this.setStatus("Thread reply is required");
          return;
        }
        try {
          await apiPost(`/v1/threads/${encodeURIComponent(button.dataset.threadReply)}/comments`, { body });
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
  }

  // installFormActions wires the issue create/edit form and attachment upload form.
  installFormActions(refresh) {
    this.querySelectorAll("[data-issue-form]").forEach((form) => {
      bindHarnessModelControlsView(this, form);
      bindAgentArgControlsView(this, form);
      form.querySelector?.("[data-save-agent-defaults]")?.addEventListener("click", () => {
        try {
          const saved = writeIssueAgentDefaults(issueAgentDefaultsFromFormView(this, form));
          this.setStatus(saved ? "Agent defaults saved" : "Unable to save agent defaults");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        if (form.reportValidity && !form.reportValidity()) return;
        const mode = form.dataset.issueFormMode || "edit";
        const priority = Number(form.elements.priority.value || 0);
        if (!Number.isInteger(priority) || priority < 0) {
          this.setStatus("Priority must be a non-negative integer");
          return;
        }
        let agentSettings;
        let agentDefaults;
        try {
          agentSettings = issueAgentPayloadFromFormView(this, form);
          if (mode === "create") agentDefaults = issueAgentDefaultsFromFormView(this, form);
        } catch (error) {
          this.setStatus(error.message || String(error));
          return;
        }
        const payload = {
          title: form.elements.title.value.trim(),
          body: form.elements.body.value,
          acceptance_criteria: form.elements.acceptance_criteria.value,
          priority,
          requires_human_review: form.elements.requires_human_review.checked,
          auto_merge: form.elements.auto_merge.checked,
          agent_harness: agentSettings.agent_harness,
          harness_args: agentSettings.harness_args,
        };
        if (form.elements.plan_mode) {
          payload.plan_mode = form.elements.plan_mode.checked;
        }
        if (!payload.title) {
          this.setStatus("Issue title is required");
          return;
        }
        try {
          if (mode === "create") {
            payload.schedule_state = form.elements.queue_issue && form.elements.queue_issue.checked ? "up_next" : "backlog";
            const formProject = form.elements.project ? form.elements.project.value : (form.dataset.project || "");
            if (!formProject) {
              this.setStatus("Project is required");
              return;
            }
            const data = await apiPost(issueAPIBase(formProject), payload);
            const issue = data.issue || data.Issue || {};
            const issueID = value(issue, "id", "ID");
            if (!issueID) {
              throw new Error("Created issue ID unavailable");
            }
            const createdProject = data.project_id || data.ProjectID || formProject;
            writeIssueAgentDefaults(agentDefaults);
            history.pushState({}, "", issueHref(createdProject, issueID));
            const files = Array.from(form.elements.attachments?.files || []);
            for (const file of files) {
              await uploadIssueAttachment(createdProject, issueID, file, "initial");
            }
            await this.load();
          } else {
            const issueID = form.dataset.issueForm;
            await apiPatch(`${issueAPIBase(form.dataset.project)}/${encodeURIComponent(issueID)}`, payload);
            await refresh();
          }
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-attachment-form]").forEach((form) => {
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        if (form.reportValidity && !form.reportValidity()) return;
        const issueID = form.dataset.issue;
        const file = form.elements.file?.files?.[0];
        try {
          await uploadIssueAttachment(form.dataset.project, issueID, file, form.elements.stage.value);
          form.reset();
          await refresh();
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
  }

  // installTerminalActions wires embedded terminal and transcript launch buttons.
  installTerminalActions() {
    this.querySelectorAll("[data-terminal]").forEach((button) => {
      button.addEventListener("click", async () => {
        await this.openInlineTerminal(button, "session", button.dataset.terminal);
      });
    });
    this.querySelectorAll("[data-job-terminal]").forEach((button) => {
      button.addEventListener("click", async () => {
        await this.openInlineTerminal(button, "job", button.dataset.jobTerminal);
      });
    });
    this.querySelectorAll("[data-job-attach]").forEach((button) => {
      button.addEventListener("click", async () => {
        try {
          const data = await apiGet(`/v1/jobs/${encodeURIComponent(button.dataset.jobAttach)}/attach`);
          const attach = data.attach || data.Attach || {};
          const command = value(attach, "command", "Command") || [];
          const tmuxSession = value(attach, "tmux_session", "TmuxSession");
          const text = Array.isArray(command) && command.length ? command.join(" ") : (tmuxSession ? `tmux attach-session -t ${tmuxSession}` : "");
          this.setStatus(text || "Attach command unavailable");
        } catch (error) {
          this.setStatus(error.message || String(error));
        }
      });
    });
    this.querySelectorAll("[data-session-transcript]").forEach((button) => {
      button.addEventListener("click", async () => {
        await showTranscriptView(this, button, "session", button.dataset.sessionTranscript);
      });
    });
    this.querySelectorAll("[data-job-transcript]").forEach((button) => {
      button.addEventListener("click", async () => {
        await showTranscriptView(this, button, "job", button.dataset.jobTranscript);
      });
    });
  }

  // installToggleActions wires in-place UI toggles (edit form, timeline expand/collapse).
  installToggleActions() {
    this.querySelectorAll("[data-issue-edit-toggle]").forEach((button) => {
      button.addEventListener("click", () => toggleIssueEditFormView(this, button));
    });
    this.querySelectorAll("[data-timeline-show-more]").forEach((button) => {
      button.addEventListener("click", () => this.expandTimeline(button));
    });
    this.querySelectorAll("[data-timeline-run-toggle]").forEach((button) => {
      button.addEventListener("click", () => this.toggleTimelineRun(button));
    });
    this.querySelectorAll("[data-diff-mode-toggle] button").forEach((button) => {
      button.addEventListener("click", () => this.toggleDiffMode(button));
    });
  }

  // toggleDiffMode switches the change detail diff between unified and split
  // rendering. It persists the preference and re-renders the cached diff payload
  // in the new mode without refetching it from the API.
  toggleDiffMode(button) {
    const mode = button.getAttribute("data-diff-mode");
    if (!mode) return;
    writeDiffMode(mode);
    const container = button.closest?.("[data-change-diff]");
    if (!container) return;
    const cacheKey = container.getAttribute?.("data-diff-cache-key");
    const diff = cacheKey && this.changeDiffCache?.get(cacheKey);
    if (!diff) return;
    container.innerHTML = renderDiffSummary(diff, mode);
    container.querySelectorAll?.("[data-diff-mode-toggle] button").forEach((btn) => {
      btn.addEventListener("click", () => this.toggleDiffMode(btn));
    });
  }

  // toggleIssueEditForm swaps the read-only detail summary for the full edit
  // form (and back), re-binding the form's harness controls so they work after
  // being revealed from a hidden container.

  // expandTimeline reveals the capped timeline rows behind the "Show more"
  // control and removes the control once everything is visible.
  expandTimeline(button) {
    const feed = button.closest("[data-timeline]");
    if (!feed) return;
    const hidden = feed.querySelector(".timeline-hidden");
    if (hidden) hidden.hidden = false;
    button.remove();
  }

  // toggleTimelineRun expands/collapses a grouped run of session_state_changed
  // rows.
  toggleTimelineRun(button) {
    const run = button.closest(".timeline-run");
    if (!run) return;
    const rows = run.querySelector(".timeline-run-rows");
    if (!rows) return;
    const expanded = !rows.hidden;
    rows.hidden = expanded;
    button.setAttribute("aria-expanded", String(!expanded));
  }

  async approveHumanReview(button, refresh) {
    try {
      await apiPost(`${issueAPIBase(button.dataset.project)}/${encodeURIComponent(button.dataset.humanReviewApprove)}/checks/${encodeURIComponent(button.dataset.checkName || "human-review")}`, {
        kind: "human",
        required: true,
        verdict: "satisfied",
        details: "approved via web UI",
        reporter: "web-ui",
      });
      await refresh();
    } catch (error) {
      this.setStatus(error.message || String(error));
    }
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
