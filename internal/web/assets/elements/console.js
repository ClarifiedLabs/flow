// <flow-console>: the interactive agent Console page — project resolution,
// the session state, the terminal frame, and the console's own URL-guarded
// refresh polling (separate from the main poll loop). The route fetches the
// console state and mounts; the element owns the poll from there, and the
// poll dies with the element on navigation (its disconnectedCallback clears
// the timer — no app-level stop call left).

import { apiGet, consoleAPIPath, consoleState, taskConsoleAPIPath } from "../api.js";
import { CONSOLE_REFRESH_MS } from "../config.js";
import { failureMessage } from "../actions/dispatch.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { Poller } from "../poller.js";
import { terminalSelectionHint } from "../terminal.js";
import { define, FlowElement } from "./base.js";

// resolveConsoleProject picks the console's project: the URL's explicit
// choice, the topbar's single selection, or the registry's only entry. Null
// when the choice is ambiguous (the chooser renders instead).
export function resolveConsoleProject(app, selectedProject) {
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

export function renderConsoleChooserMarkup(projects = []) {
  if (!projects.length) return `<div class="empty">No projects</div>`;
  return `
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

// renderConsoleMarkup is the console page: the header with its Start/Release
// controls above the terminal frame (or the starting placeholder).
export function renderConsoleMarkup({ project, projectID, selectedTask = "", job, session, active, terminalAvailable, loginPath = "", harnessOptions = "" }) {
  let terminal = "";
  if (session && terminalAvailable && loginPath) {
    terminal = `
      <div class="terminal-bezel">
        <div class="terminal-titlebar"><span class="dot"></span><span>${escapeHTML(selectedTask ? `console ${selectedTask}` : `console ${project.name || projectID}`)}</span>${terminalSelectionHint}</div>
        <iframe class="terminal-frame" title="${escapeAttr(selectedTask ? "Task console terminal" : "Console terminal")}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>
      </div>
    `;
  } else if (active) {
    terminal = `<div class="empty">Console is starting</div>`;
  }
  return `
    <section class="detail terminal-detail">
      <div class="detail-head">
        <div>
          <p class="eyebrow">${escapeHTML(project.name || projectID)}</p>
          <h2>${escapeHTML(selectedTask ? `Console · ${selectedTask}` : "Console")}</h2>
          <p class="meta">${escapeHTML(active ? consoleState(job, session) : "not running")}</p>
        </div>
        <div class="actions console-actions">
          ${active ? `<button class="button secondary" data-release-console="${escapeAttr(selectedTask)}" data-project="${escapeAttr(projectID)}" data-task="${escapeAttr(selectedTask)}">Release Console</button>` : `
            <label>Harness
              <select data-console-harness>
                ${harnessOptions}
              </select>
            </label>
            <button class="button" data-start-console="${escapeAttr(selectedTask)}" data-project="${escapeAttr(projectID)}" data-task="${escapeAttr(selectedTask)}">Start Console</button>`}
        </div>
      </div>
      ${terminal}
    </section>
  `;
}

export class FlowConsole extends FlowElement {
  // The console poll is the element's own Poller; it stops when the element
  // disconnects (navigation) and re-arms when a fresh payload arrives (the
  // route's remount reuses the element, so only its data moves).
  poll = new Poller();
  payload = null;

  // The poll arms from the payload itself, not from paint: a mount whose
  // element never connects (a superseded load's, or a test stub's) must not
  // leave a live timer, and a connected one re-arms on every fresh payload.
  set data(next) {
    super.data = next;
    if (next && next !== this.payload) {
      this.payload = next;
      this.syncPoll(next);
    }
  }

  get data() {
    return super.data;
  }

  // data: { chooser, projects } for the project chooser, or
  // { project, projectID, selectedTask, job, session, active,
  //   terminalAvailable, loginPath, harnessOptions } for the console itself.
  render() {
    const payload = this.payload;
    if (!payload) return "";
    if (payload.chooser) return renderConsoleChooserMarkup(payload.projects || []);
    return renderConsoleMarkup(payload);
  }

  disconnectedCallback() {
    this.poll.clear();
  }

  // syncPoll re-arms the refresh loop for an active console and stops it for
  // an inactive one.
  syncPoll(payload) {
    this.poll.clear();
    if (!payload.active) return;
    const { projectID, selectedTask: taskID = "" } = payload;
    const hadTerminal = Boolean(payload.terminalAvailable);
    this.poll.arm(CONSOLE_REFRESH_MS, async () => {
      if (!this.isConnected) return;
      if (!this.currentTarget(projectID, taskID)) return;
      try {
        const data = await apiGet(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID));
        if (!this.currentTarget(projectID, taskID)) return;
        const job = data.job || data.Job || null;
        const session = data.session || data.Session || null;
        const active = Boolean(data.active || data.Active || job || session);
        const terminalAvailable = Boolean(data.terminal_available || data.TerminalAvailable);
        if (!active || (!hadTerminal && terminalAvailable)) {
          // Another load (a refresh, navigation, or settle-burst tick) started
          // while this GET was in flight: skip the redundant reload rather
          // than overlap it, and re-arm so the transition is still picked up
          // by a later tick (the in-flight load's own render re-arms the poll
          // too).
          if (this.app?.loadsInFlight) {
            this.armAgain(projectID, taskID, terminalAvailable);
            return;
          }
          await this.app?.load?.({ fromPoll: true });
          return;
        }
        this.armAgain(projectID, taskID, terminalAvailable);
      } catch (error) {
        if (!this.currentTarget(projectID, taskID)) return;
        this.app?.setStatus?.(`console refresh failed: ${failureMessage(error)}`);
        this.armAgain(projectID, taskID, hadTerminal);
      }
    });
  }

  armAgain(projectID, taskID, terminalAvailable) {
    if (!this.isConnected) return;
    this.syncPoll({ active: true, projectID, selectedTask: taskID, terminalAvailable });
  }

  // currentTarget guards the loop against navigations: the console it polls
  // must still be the one the URL names.
  currentTarget(projectID, taskID = "") {
    if (window.location.pathname !== "/ui/console") return false;
    const params = new URLSearchParams(window.location.search);
    const selectedProject = params.get("project") || "";
    if (selectedProject && selectedProject !== projectID) return false;
    const selectedTask = params.get("task") || "";
    return !selectedTask || selectedTask === taskID;
  }
}

define("flow-console", FlowConsole);
