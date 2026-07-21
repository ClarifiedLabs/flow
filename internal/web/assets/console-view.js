// Interactive agent Console view: project resolution, the session list, and the
// console's own URL-guarded refresh polling (separate from the main poll loop).

import { apiDelete, apiGet, apiPost, consoleAPIPath, consoleState, taskConsoleAPIPath } from "./api.js";
import { CONSOLE_REFRESH_MS, DEFAULT_CONSOLE_HARNESSES } from "./config.js";
import { renderHarnessOptions } from "./harness-models.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { terminalSelectionHint } from "./terminal.js";

export async function renderConsoleView(app, context) {
  app.setTitle("Console");
  await app.ensureHarnesses();
  if (context && !app.isActiveLoad(context)) return false;
  const params = new URLSearchParams(window.location.search);
  const selectedProject = params.get("project") || "";
  const selectedTask = params.get("task") || "";
  const project = resolveConsoleProjectView(app, selectedProject);
  if (!project) {
    if (context && !app.isActiveLoad(context)) return false;
    renderConsoleProjectChooserView(app);
    return true;
  }

  const data = await apiGet(selectedTask ? taskConsoleAPIPath(project.id, selectedTask) : consoleAPIPath(project.id));
  if (context && !app.isActiveLoad(context)) return false;
  const job = data.job || data.Job || null;
  const session = data.session || data.Session || null;
  const active = Boolean(data.active || data.Active || job || session);
  const projectID = data.project_id || data.ProjectID || project.id || "";
  const terminalAvailable = Boolean(data.terminal_available || data.TerminalAvailable);

  let terminal = "";
  if (session && terminalAvailable) {
    const sessionID = value(session, "id", "ID");
    const accessData = await apiPost(`/v2/sessions/${encodeURIComponent(sessionID)}/terminal-token`, {});
    if (context && !app.isActiveLoad(context)) return false;
    const access = accessData.access || accessData.Access || {};
    const loginPath = value(access, "login_path", "LoginPath");
    if (loginPath) {
      terminal = `
        <div class="terminal-bezel">
          <div class="terminal-titlebar"><span class="dot"></span><span>${escapeHTML(selectedTask ? `console ${selectedTask}` : `console ${project.name || projectID}`)}</span>${terminalSelectionHint}</div>
          <iframe class="terminal-frame" title="${escapeAttr(selectedTask ? "Task console terminal" : "Console terminal")}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>
        </div>
      `;
    }
  } else if (active) {
    terminal = `<div class="empty">Console is starting</div>`;
  }

  app.querySelector(".content").innerHTML = `
    <section class="detail terminal-detail">
      <div class="detail-head">
        <div>
          <p class="eyebrow">${escapeHTML(project.name || projectID)}</p>
          <h2>${escapeHTML(selectedTask ? `Console · ${selectedTask}` : "Console")}</h2>
          <p class="meta">${escapeHTML(active ? consoleState(job, session) : "not running")}</p>
        </div>
        <div class="actions console-actions">
          ${active ? `<button class="button secondary" data-release-console data-project="${escapeAttr(projectID)}" data-task="${escapeAttr(selectedTask)}">Release Console</button>` : `
            <label>Harness
              <select data-console-harness>
                ${renderHarnessOptions((app.harnesses && app.harnesses.consoles) || DEFAULT_CONSOLE_HARNESSES, "claude")}
              </select>
            </label>
            <button class="button" data-start-console data-project="${escapeAttr(projectID)}" data-task="${escapeAttr(selectedTask)}">Start Console</button>`}
        </div>
      </div>
      ${terminal}
    </section>
  `;
  if (active) scheduleConsolePollView(app, projectID, selectedTask, { terminalAvailable });
  return true;
}

export function resolveConsoleProjectView(app, selectedProject) {
  const projects = app.projects || [];
  if (selectedProject) {
    const match = projects.find((project) => value(project, "id", "ID") === selectedProject);
    return {
      id: selectedProject,
      name: match ? value(match, "name", "Name") || selectedProject : selectedProject,
    };
  }

  const selected = app.selectedProjectIDs();
  if (selected.length === 1) {
    const match = projects.find((project) => value(project, "id", "ID") === selected[0]);
    return {
      id: selected[0],
      name: match ? value(match, "name", "Name") || selected[0] : selected[0],
    };
  }

  if (projects.length === 1) {
    return {
      id: value(projects[0], "id", "ID"),
      name: value(projects[0], "name", "Name") || value(projects[0], "id", "ID"),
    };
  }

  return null;
}

export function renderConsoleProjectChooserView(app) {
  const projects = app.projects || [];
  if (!projects.length) {
    app.querySelector(".content").innerHTML = `<div class="empty">No projects</div>`;
    return;
  }
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>Select Project</h2>
          <p class="meta">Choose a project for its Console session.</p>
        </div>
      </div>
      <div class="list">${projects.map((project) => {
        const id = value(project, "id", "ID");
        const name = value(project, "name", "Name") || id;
        return `<a class="row" href="/ui/console?project=${encodeURIComponent(id)}" data-link><span>${escapeHTML(name)}</span><span>${escapeHTML(id)}</span></a>`;
      }).join("")}</div>
    </section>
  `;
}

export function scheduleConsolePollView(app, projectID, taskID = "", state = {}) {
  stopConsolePollView(app);
  const hadTerminal = Boolean(state.terminalAvailable);
  app.consolePoll.arm(CONSOLE_REFRESH_MS, async () => {
    if (!isCurrentConsoleTargetView(app, projectID, taskID)) return;
    try {
      const data = await apiGet(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID));
      if (!isCurrentConsoleTargetView(app, projectID, taskID)) return;
      const job = data.job || data.Job || null;
      const session = data.session || data.Session || null;
      const active = Boolean(data.active || data.Active || job || session);
      const terminalAvailable = Boolean(data.terminal_available || data.TerminalAvailable);
      if (!active || (!hadTerminal && terminalAvailable)) {
        await app.load({ fromPoll: true });
        return;
      }
      scheduleConsolePollView(app, projectID, taskID, { terminalAvailable });
    } catch (error) {
      if (!isCurrentConsoleTargetView(app, projectID, taskID)) return;
      app.setStatus(`console refresh failed: ${error.message || String(error)}`);
      scheduleConsolePollView(app, projectID, taskID, { terminalAvailable: hadTerminal });
    }
  });
}

export function stopConsolePollView(app) {
  app.consolePoll.clear();
}

export function isCurrentConsoleProjectView(app, projectID) {
  if (window.location.pathname !== "/ui/console") return false;
  const selectedProject = new URLSearchParams(window.location.search).get("project") || "";
  return !selectedProject || selectedProject === projectID;
}

export function isCurrentConsoleTargetView(app, projectID, taskID = "") {
  if (!isCurrentConsoleProjectView(app, projectID)) return false;
  const selectedTask = new URLSearchParams(window.location.search).get("task") || "";
  return !selectedTask || selectedTask === taskID;
}

export async function startConsoleView(app, projectID, harness, taskID = "") {
  await apiPost(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID), { harness });
  await app.load();
  app.setStatus("console starting");
}

export async function releaseConsoleView(app, projectID, taskID = "") {
  await apiDelete(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID));
  await app.load();
  app.setStatus("console released");
}
