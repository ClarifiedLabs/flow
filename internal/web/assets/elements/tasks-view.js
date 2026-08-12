// Tasks view pure functions: the filter/query builders, the container
// grouping, and the controls/list/bulk-bar renderers. They take the "view"
// shape — the <flow-tasks> element provides it, and so do the stand-ins in
// tasks-view.test.mjs.

import { failureMessage } from "../actions/dispatch.js";
import { apiPatch, apiPost, taskAPIBase, taskHref, workItemHref } from "../api.js";
import { phaseKey, renderPhaseBadge } from "../board.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { LIFECYCLE_DONE, LIFECYCLE_IN_PROGRESS, LIFECYCLE_SCHEDULED, LIFECYCLE_UNSCHEDULED, lifecycleStateOf } from "../lifecycle.js";
import { value } from "../normalize.js";
import { readTasksListView, readTasksProject, readTasksQuery, readTasksState, writeTasksListView, writeTasksProject, writeTasksQuery, writeTasksState, writeWorkProject } from "../storage.js";
import { classifyTaskContainer, TASK_CONTAINER_STANDALONE, workItemCompare, workItemKind } from "../work-item-model.js";
import { renderWorkNav, workViewHref } from "../work-nav.js";

// TASKS_STATE_FILTERS are the lifecycle chips. The four state chips combine
// (the server ORs repeatable state params); "all" is a shortcut that selects
// every state at once, and selecting no states matches no tasks. The state
// keys come from the shared lifecycle vocabulary so a new server state cannot
// silently skip the filter chips.
export const TASKS_STATE_FILTERS = [
  ["all", "All"],
  [LIFECYCLE_UNSCHEDULED, "Unscheduled"],
  [LIFECYCLE_SCHEDULED, "Scheduled"],
  [LIFECYCLE_IN_PROGRESS, "In Progress"],
  [LIFECYCLE_DONE, "Done"],
];

// TASKS_SELECTABLE_STATES are the four filterable lifecycle states: everything
// in TASKS_STATE_FILTERS except the "all" shortcut.
const TASKS_SELECTABLE_STATES = TASKS_STATE_FILTERS.slice(1).map(([key]) => key);

// tasksStateFromLocation seeds the lifecycle filter from ?state= deep-link
// params (the board's throughput strip links /ui/tasks?state=done, for
// example). Only selectable lifecycle states are honored and valid values
// combine like chip clicks; when none of the params name a selectable state,
// the caller falls back to the persisted filter instead of an empty selection.
export function tasksWorkProjectIDs(app) {
  const own = String(app.tasksProject || "").trim();
  if (own) return [own];
  const selected = app.selectedProjectIDs();
  const source = selected.length ? selected : (app.projects || []).map((project) => value(project, "id", "ID"));
  return [...new Set(source.map((id) => String(id || "").trim()).filter(Boolean))];
}

export function taskContainerContext(app, task) {
  let projectID = String(value(task, "project_id", "ProjectID") || app.tasksProject || "").trim();
  const indexes = app.tasksWorkIndexes || new Map();
  if (!projectID && indexes.size === 1) projectID = indexes.keys().next().value;
  const classification = classifyTaskContainer(indexes.get(projectID), String(value(task, "id", "ID") || ""));
  return { ...classification, projectID };
}

// The root scope is intentionally client-side: /v2/tasks remains the one
// aggregate state/search/project query, while the fetched work-item summaries
// classify each result under its top-level container. Standalone and unknown
// are real filter values so malformed/missing hierarchy never disappears.
export function filteredTasksView(app) {
  const tasks = app.tasksList || [];
  const root = String(app.tasksRoot || "").trim();
  return root ? tasks.filter((task) => taskContainerContext(app, task).id === root) : tasks;
}

export function taskContainerGroupsView(app, tasks = filteredTasksView(app)) {
  const groups = new Map();
  for (const task of tasks) {
    const context = taskContainerContext(app, task);
    // Container IDs normally are globally unique, but project is part of the
    // key so an aggregate multi-project list remains lossless if they are not.
    const key = `${context.projectID}\u0000${context.id}`;
    if (!groups.has(key)) groups.set(key, { ...context, tasks: [] });
    groups.get(key).tasks.push(task);
  }
  const rank = (group) => group.item ? 0 : group.id === TASK_CONTAINER_STANDALONE ? 1 : 2;
  return [...groups.values()].sort((a, b) => {
    const order = rank(a) - rank(b);
    if (order) return order;
    if (a.item && b.item) {
      const itemOrder = workItemCompare(a.item, b.item);
      if (itemOrder) return itemOrder;
    }
    return a.projectID.localeCompare(b.projectID) || a.id.localeCompare(b.id);
  });
}

export function tasksRootFromSearch(search = window.location.search) {
  return String(new URLSearchParams(String(search || "").replace(/^\?/, "")).get("root") || "").trim();
}

// Preserve all existing deep-link controls while changing only root. In
// particular, state and project stay in the browser query and root never leaks
// into the aggregate /v2/tasks request built by tasksQueryView.
export function tasksRootHref(search, root) {
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  const id = String(root || "").trim();
  if (id) params.set("root", id);
  else params.delete("root");
  const query = params.toString();
  return `/ui/tasks${query ? `?${query}` : ""}`;
}

export function tasksStateFromLocation() {
  const params = new URLSearchParams(window.location.search);
  const states = new Set(params.getAll("state").filter((key) => TASKS_SELECTABLE_STATES.includes(key)));
  return states.size > 0 ? states : null;
}

// tasksQueryView builds the aggregate /v2/tasks query: repeatable project from
// the topbar selection, one state param per selected lifecycle chip (the
// server ORs them), and any extras (q). The view short-circuits an empty state
// selection before this is called, because the server reads an absent state
// filter as "no filtering" rather than "match nothing". When the in-view
// project filter is set it narrows the fetch to exactly that project instead —
// repeatable project params are a union server-side, so sending both it and
// the topbar selection would list the task set twice.
export function tasksQueryView(app, state, extra = {}) {
  const params = new URLSearchParams();
  const own = String(app.tasksProject || "").trim();
  const projects = own ? [own] : app.selectedProjectIDs();
  for (const id of projects) params.append("project", id);
  for (const key of TASKS_SELECTABLE_STATES) {
    if (state && state.has(key)) params.append("state", key);
  }
  for (const [key, val] of Object.entries(extra)) {
    if (val !== undefined && val !== null && String(val).trim() !== "") params.set(key, val);
  }
  const query = params.toString();
  return query ? "?" + query : "";
}

export function renderTasksControlsView(app) {
  const states = app.tasksState || new Set();
  const chips = TASKS_STATE_FILTERS.map(([key, label]) => {
    const active = key === "all" ? states.size === TASKS_SELECTABLE_STATES.length : states.has(key);
    return `<button class="chip${active ? " active" : ""}" data-tasks-state="${escapeAttr(key)}"${active ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`;
  }).join("");
  const projectOptions = [`<option value="">All projects</option>`]
    .concat((app.projects || []).map((project) => {
      const id = String(value(project, "id", "ID"));
      const name = String(value(project, "name", "Name") || id);
      return `<option value="${escapeAttr(id)}"${id === app.tasksProject ? " selected" : ""}>${escapeHTML(name)}</option>`;
    })).join("");
  const rootGroups = taskContainerGroupsView(app, app.tasksList || []);
  const rootOptions = [`<option value="">All containers</option>`]
    .concat(rootGroups.map((group) => renderTasksRootOptionView(app, group))).join("");
  // Keep an arbitrary root deep link visible even when the current aggregate
  // state/search filters have no task under it.
  const root = String(app.tasksRoot || "").trim();
  const hasRoot = rootGroups.some((group) => group.id === root);
  const missingRoot = root && !hasRoot
    ? `<option value="${escapeAttr(root)}" selected>Unknown container · ${escapeHTML(root)}</option>`
    : "";
  const layout = app.tasksLayout === "container" ? "container" : "flat";
  return `
    <div class="tasks-controls">
      <div class="tasks-filters" role="group" aria-label="Filter by state">${chips}</div>
      <div class="tasks-tools">
        <div class="tasks-layout" role="group" aria-label="Task list layout">
          <button type="button" class="chip${layout === "flat" ? " active" : ""}" data-tasks-layout="flat" aria-pressed="${layout === "flat"}">Flat</button>
          <button type="button" class="chip${layout === "container" ? " active" : ""}" data-tasks-layout="container" aria-pressed="${layout === "container"}">By container</button>
        </div>
        <select class="tasks-project" data-tasks-project aria-label="Filter by project">${projectOptions}</select>
        <select class="tasks-root" data-tasks-root aria-label="Filter by top-level container">${rootOptions}${missingRoot}</select>
        <input class="tasks-search" data-tasks-search type="search" placeholder="Search title and body" value="${escapeAttr(app.searchDraft ?? app.tasksQuery)}" aria-label="Search tasks">
      </div>
    </div>
  `;
}

function taskContainerLabelView(group) {
  if (group.item) return String(value(group.item, "title", "Title") || group.id);
  return group.id === TASK_CONTAINER_STANDALONE ? "Standalone" : "Unknown";
}

function renderTasksRootOptionView(app, group) {
  const project = (app.projects || []).find((candidate) => String(value(candidate, "id", "ID")) === group.projectID);
  const projectLabel = app.tasksProjectBadge && project ? `${value(project, "name", "Name") || group.projectID} · ` : "";
  const label = `${projectLabel}${taskContainerLabelView(group)}${group.item ? ` · ${group.id}` : ""}`;
  return `<option value="${escapeAttr(group.id)}"${group.id === app.tasksRoot ? " selected" : ""}>${escapeHTML(label)}</option>`;
}

export function renderTasksListView(app) {
  const list = app.querySelector(".tasks-list");
  if (!list) return;
  list.innerHTML = renderTasksListMarkup(app);
}

export function renderTasksListMarkup(app) {
  const tasks = filteredTasksView(app);
  if (!tasks.length) {
    // An empty state selection matches no tasks by design; call that out so it
    // is not mistaken for a filter that matched nothing.
    const noStates = !app.tasksState || app.tasksState.size === 0;
    return noStates
      ? `<div class="empty">No states selected — pick All or a state chip to show tasks</div>`
      : app.tasksRoot && (app.tasksList || []).length
        ? `<div class="empty">No tasks in this container</div>`
        : `<div class="empty">No tasks</div>`;
  }
  const selected = app.tasksSelected || new Set();
  const rows = app.tasksLayout === "container"
    ? taskContainerGroupsView(app, tasks).map((group) => renderTasksGroupView(app, group)).join("")
    : `<div class="tasks-flat-list" role="list">${tasks.map((task) => renderTaskRowView(app, task, { showContainer: true })).join("")}</div>`;
  return `
    <label class="tasks-select-all"><input type="checkbox" data-tasks-select-all${tasks.every((task) => selected.has(String(value(task, "id", "ID")))) ? " checked" : ""}> Select all ${tasks.length} visible</label>
    ${rows}
  `;
}

export function renderTasksGroupView(app, group) {
  const label = taskContainerLabelView(group);
  const kind = group.item ? workItemKind(group.item) : group.kind;
  const title = group.item && group.projectID
    ? `<a href="${escapeAttr(workItemHref(group.projectID, group.item))}" data-link>${escapeHTML(label)}</a>`
    : `<span>${escapeHTML(label)}</span>`;
  return `<section class="tasks-group" data-tasks-group="${escapeAttr(group.id)}">
    <header class="tasks-group-header">
      <span class="tasks-group-kind">${escapeHTML(kind)}</span>
      ${title}
      ${group.item ? `<span class="tasks-group-id">${escapeHTML(group.id)}</span>` : ""}
      <span class="tasks-group-count">${group.tasks.length} task${group.tasks.length === 1 ? "" : "s"}</span>
    </header>
    <div class="tasks-group-list" role="list">${group.tasks.map((task) => renderTaskRowView(app, task, { showContainer: false })).join("")}</div>
  </section>`;
}

export function renderTaskRowView(app, task, { showContainer = app.tasksLayout !== "container" } = {}) {
  const id = String(value(task, "id", "ID"));
  const title = String(value(task, "title", "Title"));
  // Task.state is null for unscheduled work; lifecycleStateOf normalizes both
  // an absent state and any out-of-vocabulary state onto the shared vocabulary.
  const state = lifecycleStateOf(task);
  const projectID = String(value(task, "project_id", "ProjectID"));
  const projectName = String(value(task, "project_name", "ProjectName"));
  const flowID = String(value(task, "flow_id", "FlowID"));
  const priority = Number(value(task, "priority", "Priority") || 0);
  const checked = app.tasksSelected && app.tasksSelected.has(id) ? " checked" : "";
  const context = showContainer ? taskContainerContext(app, task) : null;
  const breadcrumb = context?.item && context.projectID
    ? `<nav class="tasks-row-breadcrumb" aria-label="Top-level container"><a href="${escapeAttr(workItemHref(context.projectID, context.item))}" data-link>${escapeHTML(value(context.item, "title", "Title") || context.id)}</a><span aria-hidden="true">/</span></nav>`
    : "";
  const meta = [
    app.tasksProjectBadge && projectName ? `<span class="card-project-badge">${escapeHTML(projectName)}</span>` : "",
    flowID ? escapeHTML(flowID) : "",
    priority ? `p${priority}` : "",
  ].filter(Boolean).join(" · ");
  return `
    <div class="tasks-row" role="listitem" data-phase="${escapeAttr(phaseKey(state))}" data-task-row="${escapeAttr(id)}">
      <input type="checkbox" class="tasks-select" data-tasks-select="${escapeAttr(id)}" aria-label="Select ${escapeAttr(id)}"${checked}>
      ${breadcrumb}
      <a class="tasks-row-title" href="${escapeAttr(taskHref(projectID, id))}" data-link>${escapeHTML(id)} · ${escapeHTML(title)}</a>
      <span class="tasks-row-badges">${renderPhaseBadge(state)}</span>
      ${meta ? `<span class="tasks-row-meta">${meta}</span>` : ""}
    </div>
  `;
}

// renderTasksBulkBarView paints the bulk-action bar into its .tasks-bulk
// container, which stays hidden until the selection is non-empty.
export function renderTasksBulkBarView(app) {
  const container = app.querySelector(".tasks-bulk");
  if (!container) return;
  const count = app.tasksSelected ? app.tasksSelected.size : 0;
  container.hidden = count === 0;
  container.innerHTML = count ? renderTasksBulkBarMarkup(app) : "";
}

export function renderTasksBulkBarMarkup(app) {
  const count = app.tasksSelected ? app.tasksSelected.size : 0;
  return `
    <div class="tasks-bulk-bar" role="group" aria-label="Bulk actions">
      <span class="tasks-bulk-count">${count} selected</span>
      <span class="tasks-bulk-field">
        <input type="number" min="0" step="1" placeholder="Priority" data-tasks-bulk-priority aria-label="Bulk priority">
        <button class="button secondary" data-tasks-apply="priority">Set priority</button>
      </span>
      <span class="tasks-bulk-field">
        <select data-tasks-bulk-flow aria-label="Bulk flow">${bulkFlowOptionsView(app)}</select>
        <button class="button secondary" data-tasks-apply="flow">Set flow</button>
      </span>
      <button class="button secondary" data-tasks-apply="schedule">Schedule</button>
      <button class="button secondary" data-tasks-apply="reset">Reset</button>
      <button class="button secondary" data-tasks-apply="retry">Retry</button>
      <button class="button secondary" data-tasks-clear>Clear</button>
    </div>
  `;
}

// bulkFlowOptionsView renders the bulk flow dropdown: a disabled placeholder
// plus the in-view project's cached flows. flowSelectOptionsView would
// preselect the project default — a bulk action must only ever apply a flow
// the user explicitly picked, so apply refuses an empty value.
export function bulkFlowOptionsView(app) {
  const projectID = String(app.tasksProject || "").trim();
  const cache = (projectID && app.flowsByProject && app.flowsByProject.get(projectID)) || { flows: [] };
  const options = (cache.flows || []).map((flow) => {
    const id = String(value(flow, "id", "ID"));
    const name = String(value(flow, "name", "Name") || id);
    return `<option value="${escapeAttr(id)}">${escapeHTML(name)}</option>`;
  }).join("");
  const hint = projectID ? "Choose a flow" : "Pick a project above to choose a flow";
  return `<option value="" selected disabled>${escapeHTML(hint)}</option>${options}`;
}

// toggleTasksState is the chip click model: a state chip flips its membership
// in the selection; the "all" chip selects every state when any is missing and
// clears all of them when all are already selected.
export function toggleTasksState(state, key) {
  const next = new Set(state);
  if (key === "all") {
    return next.size === TASKS_SELECTABLE_STATES.length ? new Set() : new Set(TASKS_SELECTABLE_STATES);
  }
  if (next.has(key)) next.delete(key);
  else next.add(key);
  return next;
}

export function pruneTasksSelectionView(app) {
  const visible = new Set(filteredTasksView(app).map((task) => String(value(task, "id", "ID"))));
  app.tasksSelected = new Set([...(app.tasksSelected || [])].filter((id) => visible.has(id)));
}

// applyTasksBulkAction fans one bulk action out over the selected tasks via
// the existing per-task endpoints, then refreshes the list and reports
// per-task failures. Rejected reasons render through failureMessage, so a
// hostile rejection value (a Proxy whose message getter throws) cannot abort
// the status report. Succeeded tasks leave the selection; failed ones stay
// selected so they can be fixed up and retried.
export async function applyTasksBulkAction(app, action, view) {
  pruneTasksSelectionView(app);
  const selected = app.tasksSelected;
  const tasks = filteredTasksView(app).filter((task) => selected.has(String(value(task, "id", "ID"))));
  if (!tasks.length) return;
  let requests;
  switch (action) {
    case "priority": {
      const priority = Number(view.querySelector("[data-tasks-bulk-priority]")?.value);
      if (!Number.isInteger(priority) || priority < 0) {
        app.setStatus("Priority must be a whole number ≥ 0");
        return;
      }
      requests = tasks.map((task) => apiPatch(taskBulkPathView(task), { priority }));
      break;
    }
    case "flow": {
      const flowID = String(view.querySelector("[data-tasks-bulk-flow]")?.value || "").trim();
      if (!flowID) {
        app.setStatus("Choose a flow to apply");
        return;
      }
      requests = tasks.map((task) => apiPatch(taskBulkPathView(task), { flow_id: flowID }));
      break;
    }
    case "schedule":
      requests = tasks.map((task) => apiPost(taskBulkPathView(task, "/schedule"), {}));
      break;
    case "reset":
      requests = tasks.map((task) => apiPost(taskBulkPathView(task, "/reset"), {}));
      break;
    case "retry":
      requests = tasks.map((task) => apiPost(taskBulkPathView(task, "/workflow/retry"), {}));
      break;
    default:
      return;
  }
  app.setStatus(`${action}: updating ${tasks.length} task${tasks.length === 1 ? "" : "s"}…`);
  const results = await Promise.allSettled(requests);
  const failed = [];
  results.forEach((result, index) => {
    const id = String(value(tasks[index], "id", "ID"));
    if (result.status === "rejected") {
      failed.push(`${id}: ${failureMessage(result.reason)}`);
    } else {
      selected.delete(id);
    }
  });
  await app.load();
  const succeeded = tasks.length - failed.length;
  app.setStatus(failed.length
    ? `${action}: ${succeeded} updated, ${failed.length} failed — ${failed.join("; ")}`
    : `${action}: updated ${succeeded} task${succeeded === 1 ? "" : "s"}`);
}

// taskBulkPathView is the per-task endpoint for one selected row: scoped to
// the row's own project when the aggregate listing named one (flows and
// permissions are project-owned), falling back to the globally resolvable
// /v2/tasks route otherwise — the same rule the one-off actions use.
export function taskBulkPathView(task, suffix = "") {
  const projectID = String(value(task, "project_id", "ProjectID"));
  const id = String(value(task, "id", "ID"));
  return `${taskAPIBase(projectID)}/${encodeURIComponent(id)}${suffix}`;
}

