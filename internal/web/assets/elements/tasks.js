// <flow-tasks>: a flat or container-grouped, filterable list of tasks across
// every visible project. The route (tasks-route.js) mounts the element, syncs
// its URL/storage-seeded state, fetches the aggregate read, and hands the
// payload over; the element owns the view state (chips, project/root/search
// filters, selection) and every control. The pure view functions live in
// tasks-view.js; the view intentionally does not poll.

import { failureMessage } from "../actions/dispatch.js";
import { define, FlowElement } from "./base.js";
import { value } from "../normalize.js";
import {
  readTasksListView,
  readTasksProject,
  readTasksQuery,
  readTasksState,
  writeTasksListView,
  writeTasksProject,
  writeTasksQuery,
  writeTasksState,
  writeWorkProject,
} from "../storage.js";
import {
  applyTasksBulkAction,
  filteredTasksView,
  pruneTasksSelectionView,
  renderTasksBulkBarMarkup,
  renderTasksControlsView,
  renderTasksListMarkup,
  tasksRootFromSearch,
  tasksRootHref,
  tasksStateFromLocation,
  tasksWorkProjectIDs,
  toggleTasksState,
} from "./tasks-view.js";
import { renderWorkNav, workViewHref } from "../work-nav.js";

export class FlowTasks extends FlowElement {
  // The view state the old view stashed on the app (app.tasksState et al.) is
  // element state now: mount() reuses the element across same-route reloads so
  // chip selections survive, and leaving the route discards the element — the
  // next visit re-seeds from the URL and storage (syncLocation), which is what
  // replacing app.js's leaving-/ui/tasks reset means in element terms.
  tasksState = undefined;
  tasksProject = undefined;
  tasksRoot = undefined;
  tasksLayout = undefined;
  tasksQuery = undefined;
  tasksSelected = undefined;
  // The payload fields the last route payload set (see render).
  tasksList = [];
  tasksWorkIndexes = new Map();
  tasksProjectBadge = false;
  // The search text the user has typed but not yet applied: kept across
  // repaints so a selection click cannot clobber an in-progress edit.
  searchDraft = undefined;
  stateSearch = undefined;
  projectSearch = undefined;
  rootSearch = undefined;
  payload = null;

  // The app services the pure view functions need, delegated to the enclosing
  // <flow-app> so the functions keep one "view" argument shape.
  get projects() {
    return this.app?.projects || [];
  }

  selectedProjectIDs() {
    return this.app?.selectedProjectIDs?.() || [];
  }

  get flowsByProject() {
    return this.app?.flowsByProject;
  }

  setStatus(message) {
    this.app?.setStatus?.(message);
  }

  async load(options) {
    return this.app?.load?.(options);
  }

  // syncLocation seeds the view state from the URL and storage whenever the
  // location's query changed since the last sync. The route calls it after
  // every mount; connectedCallback calls it for standalone mounts.
  syncLocation() {
    const search = window.location.search || "";
    if (this.tasksState === undefined || this.stateSearch !== search) {
      this.tasksState = tasksStateFromLocation() || readTasksState();
    }
    const projectFromURL = String(new URLSearchParams(search).get("project") || "").trim();
    if (this.tasksProject === undefined) this.tasksProject = projectFromURL || readTasksProject();
    else if (projectFromURL && this.projectSearch !== search) this.tasksProject = projectFromURL;
    if (this.rootSearch !== search) {
      this.tasksRoot = tasksRootFromSearch(search);
    }
    this.stateSearch = this.projectSearch = this.rootSearch = search;
    if (this.tasksLayout === undefined) this.tasksLayout = readTasksListView();
    if (this.tasksQuery === undefined) this.tasksQuery = readTasksQuery();
    if (!this.tasksSelected) this.tasksSelected = new Set();
  }

  connectedCallback() {
    this.syncLocation();
    super.connectedCallback();
  }

  // workProjectIDs is the aggregate read's project scope: the in-view project
  // filter when set, else the topbar selection, else every registered project.
  workProjectIDs() {
    return tasksWorkProjectIDs(this);
  }

  // data: { tasks, workIndexes, projectBadge } — the route's aggregate read.
  render() {
    const data = this.data;
    if (!data) return "";
    if (data !== this.payload) {
      this.payload = data;
      this.tasksList = data.tasks || [];
      this.tasksWorkIndexes = data.workIndexes || new Map();
      this.tasksProjectBadge = Boolean(data.projectBadge);
      // A refresh or root change can drop tasks from the visible list;
      // selection only ever names rows currently shown.
      pruneTasksSelectionView(this);
    }
    const workProject = String(this.tasksProject || "").trim();
    return `
      <div class="tasks-view">
        ${renderWorkNav({ active: "tasks", projects: this.projects, projectID: workProject, search: window.location.search })}
        ${renderTasksControlsView(this)}
        <div class="tasks-bulk"${this.tasksSelected && this.tasksSelected.size ? "" : " hidden"}>${this.tasksSelected && this.tasksSelected.size ? renderTasksBulkBarMarkup(this) : ""}</div>
        <div class="tasks-list">${renderTasksListMarkup(this)}</div>
      </div>
    `;
  }

  handleClick(event) {
    const stateButton = event.target.closest?.("[data-tasks-state]");
    if (stateButton) {
      this.tasksState = toggleTasksState(this.tasksState, stateButton.dataset.tasksState);
      writeTasksState(this.tasksState);
      this.load();
      return;
    }
    const layoutButton = event.target.closest?.("[data-tasks-layout]");
    if (layoutButton) {
      const layout = layoutButton.dataset.tasksLayout;
      if (layout !== "flat" && layout !== "container") return;
      this.tasksLayout = layout;
      writeTasksListView(layout);
      this.invalidate();
      return;
    }
    if (event.target.closest?.("[data-tasks-clear]")) {
      this.tasksSelected = new Set();
      this.invalidate();
      return;
    }
    const applyButton = event.target.closest?.("[data-tasks-apply]");
    if (applyButton) applyTasksBulkAction(this, applyButton.dataset.tasksApply, this);
  }

  bind() {
    this.addEventListener("change", (event) => {
      const projectSelect = event.target.closest?.("[data-tasks-project]");
      if (projectSelect) {
        this.tasksProject = String(projectSelect.value || "").trim();
        this.tasksRoot = "";
        writeTasksProject(this.tasksProject);
        if (this.tasksProject) writeWorkProject(this.tasksProject);
        if (globalThis.history?.replaceState) {
          const projectHref = workViewHref("tasks", this.tasksProject, window.location.search);
          history.replaceState({}, "", tasksRootHref(projectHref.split("?")[1] || "", ""));
        }
        this.load();
        return;
      }
      const rootSelect = event.target.closest?.("[data-tasks-root]");
      if (rootSelect) {
        this.tasksRoot = String(rootSelect.value || "").trim();
        if (globalThis.history?.replaceState) history.replaceState({}, "", tasksRootHref(window.location.search, this.tasksRoot));
        this.rootSearch = window.location.search;
        pruneTasksSelectionView(this);
        this.invalidate();
        return;
      }
      const search = event.target.closest?.("[data-tasks-search]");
      if (search) {
        this.applySearch(search.value);
        return;
      }
      // The shared Work-project picker is handled by FlowApp after this event
      // bubbles. Clear a root from the old project first so it cannot hide
      // every task in the newly selected project.
      if (event.target.closest?.("[data-work-project]")) {
        this.tasksRoot = "";
        if (globalThis.history?.replaceState) history.replaceState({}, "", tasksRootHref(window.location.search, ""));
      }
      const selectAll = event.target.closest?.("[data-tasks-select-all]");
      if (selectAll) {
        this.tasksSelected = selectAll.checked
          ? new Set(filteredTasksView(this).map((task) => String(value(task, "id", "ID"))))
          : new Set();
        this.invalidate();
        return;
      }
      const box = event.target.closest?.("[data-tasks-select]");
      if (!box) return;
      const id = String(box.dataset.tasksSelect || "");
      if (!id) return;
      if (box.checked) {
        this.tasksSelected.add(id);
      } else {
        this.tasksSelected.delete(id);
      }
      this.invalidate();
    });

    // The search input reports its text on every keystroke so a repaint keeps
    // the draft; Enter (and the change event above) applies it.
    this.addEventListener("input", (event) => {
      const search = event.target.closest?.("[data-tasks-search]");
      if (search) this.searchDraft = String(search.value ?? "");
    });
    this.addEventListener("keydown", (event) => {
      const search = event.target.closest?.("[data-tasks-search]");
      if (search && event.key === "Enter") this.applySearch(search.value);
    });
  }

  applySearch(raw) {
    const next = String(raw || "").trim();
    this.searchDraft = undefined;
    if (next === this.tasksQuery) return;
    this.tasksQuery = next;
    writeTasksQuery(this.tasksQuery);
    this.load();
  }
}

define("flow-tasks", FlowTasks);
