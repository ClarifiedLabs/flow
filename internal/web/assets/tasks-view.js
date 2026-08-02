// Tasks view: a flat, filterable list of tasks across every visible project.
// Lifecycle-state chips combine (Unscheduled, Scheduled, In Progress and Done
// can be selected together; "All" selects or clears all four at once), and an
// in-view project filter (composing with the topbar project picker) and a
// title/body search all narrow the aggregate /v2/tasks read; checkboxes select
// rows for bulk actions (priority, flow, schedule, reset, retry) that fan out
// client-side over the existing per-task endpoints — the /ui/api proxy wraps
// each call in the same idempotency handling as the one-off buttons. The view
// does not poll, so the selection and the search box are never clobbered
// mid-edit; refresh is the topbar's manual button.

import { apiGet, apiPatch, apiPost, taskAPIBase, taskHref } from "./api.js";
import { phaseKey, renderPhaseBadge } from "./board.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { readTasksProject, readTasksQuery, readTasksState, writeTasksProject, writeTasksQuery, writeTasksState } from "./storage.js";

// TASKS_STATE_FILTERS are the lifecycle chips. The four state chips combine
// (the server ORs repeatable state params); "all" is a shortcut that selects
// every state at once, and selecting no states matches no tasks.
export const TASKS_STATE_FILTERS = [
  ["all", "All"],
  ["unscheduled", "Unscheduled"],
  ["scheduled", "Scheduled"],
  ["in_progress", "In Progress"],
  ["done", "Done"],
];

// TASKS_SELECTABLE_STATES are the four filterable lifecycle states: everything
// in TASKS_STATE_FILTERS except the "all" shortcut.
const TASKS_SELECTABLE_STATES = TASKS_STATE_FILTERS.slice(1).map(([key]) => key);

// tasksStateFromLocation seeds the lifecycle filter from ?state= deep-link
// params (the board's throughput strip links /ui/tasks?state=done, for
// example). Only selectable lifecycle states are honored and valid values
// combine like chip clicks; when none of the params name a selectable state,
// the caller falls back to the persisted filter instead of an empty selection.
function tasksStateFromLocation() {
  const params = new URLSearchParams(window.location.search);
  const states = new Set(params.getAll("state").filter((key) => TASKS_SELECTABLE_STATES.includes(key)));
  return states.size > 0 ? states : null;
}

export async function renderTasksView(app, context) {
  // Seed the lifecycle filter from ?state= deep-link params whenever the URL
  // changed since the last render (or the app is fresh). A navigation to
  // /ui/tasks?state=done — including the in-app data-link navigation from the
  // board's throughput strip, which reuses this FlowApp — must win over a
  // filter retained from a previous visit; load() clears the retained filter
  // on leaving /ui/tasks so every arrival re-seeds. A re-render under an
  // unchanged URL (a chip click reloads through the same load()) keeps the
  // in-view selection instead of clobbering it back to the deep link.
  const search = window.location.search;
  if (!app.tasksState || app.tasksStateSearch !== search) {
    app.tasksState = tasksStateFromLocation() || readTasksState();
    app.tasksStateSearch = search;
  }
  if (app.tasksProject === undefined) app.tasksProject = readTasksProject();
  if (app.tasksQuery === undefined) app.tasksQuery = readTasksQuery();
  if (!app.tasksSelected) app.tasksSelected = new Set();
  // An empty state selection matches no tasks, so there is nothing to fetch;
  // every other combination maps to repeatable state params on /v2/tasks.
  let data = null;
  if (app.tasksState.size > 0) {
    data = await apiGet("/v2/tasks" + tasksQueryView(app, app.tasksState, { q: app.tasksQuery }));
    if (context && !app.isActiveLoad(context)) return false;
  }
  app.setTitle("Tasks");
  app.tasksProjectBadge = (app.projects || []).length > 1;
  app.tasksList = value(data, "tasks", "Tasks") || [];
  // A refresh can drop tasks that moved out of the filter; the selection only
  // ever names rows the list still shows.
  const visible = new Set(app.tasksList.map((task) => String(value(task, "id", "ID"))));
  app.tasksSelected = new Set([...app.tasksSelected].filter((id) => visible.has(id)));
  // The bulk flow dropdown reads the per-project flow cache; warm it now so a
  // selection renders its options synchronously. Flows are project-owned, so
  // there is nothing to warm while the filter is on all projects.
  if (String(app.tasksProject || "").trim()) await app.ensureFlows(app.tasksProject);
  if (context && !app.isActiveLoad(context)) return false;
  app.querySelector(".content").innerHTML = `
    <div class="tasks-view">
      ${renderTasksControlsView(app)}
      <div class="tasks-bulk" hidden></div>
      <div class="tasks-list"></div>
    </div>
  `;
  renderTasksListView(app);
  renderTasksBulkBarView(app);
  bindTasksControlsView(app);
  return true;
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
  return `
    <div class="tasks-controls">
      <div class="tasks-filters" role="group" aria-label="Filter by state">${chips}</div>
      <div class="tasks-tools">
        <select class="tasks-project" data-tasks-project aria-label="Filter by project">${projectOptions}</select>
        <input class="tasks-search" data-tasks-search type="search" placeholder="Search title and body" value="${escapeAttr(app.tasksQuery)}" aria-label="Search tasks">
      </div>
    </div>
  `;
}

export function renderTasksListView(app) {
  const list = app.querySelector(".tasks-list");
  if (!list) return;
  const tasks = app.tasksList || [];
  if (!tasks.length) {
    // An empty state selection matches no tasks by design; call that out so it
    // is not mistaken for a filter that matched nothing.
    const noStates = !app.tasksState || app.tasksState.size === 0;
    list.innerHTML = noStates
      ? `<div class="empty">No states selected — pick All or a state chip to show tasks</div>`
      : `<div class="empty">No tasks</div>`;
    return;
  }
  list.innerHTML = `
    <label class="tasks-select-all"><input type="checkbox" data-tasks-select-all${tasks.every((task) => app.tasksSelected.has(String(value(task, "id", "ID")))) ? " checked" : ""}> Select all</label>
    ${tasks.map((task) => renderTaskRowView(app, task)).join("")}
  `;
}

export function renderTaskRowView(app, task) {
  const id = String(value(task, "id", "ID"));
  const title = String(value(task, "title", "Title"));
  // Task.state is null for unscheduled work.
  const state = String(value(task, "state", "State") || "unscheduled");
  const projectID = String(value(task, "project_id", "ProjectID"));
  const projectName = String(value(task, "project_name", "ProjectName"));
  const flowID = String(value(task, "flow_id", "FlowID"));
  const priority = Number(value(task, "priority", "Priority") || 0);
  const checked = app.tasksSelected && app.tasksSelected.has(id) ? " checked" : "";
  const meta = [
    app.tasksProjectBadge && projectName ? `<span class="card-project-badge">${escapeHTML(projectName)}</span>` : "",
    flowID ? escapeHTML(flowID) : "",
    priority ? `p${priority}` : "",
  ].filter(Boolean).join(" · ");
  return `
    <div class="tasks-row" data-phase="${escapeAttr(phaseKey(state))}" data-task-row="${escapeAttr(id)}">
      <input type="checkbox" class="tasks-select" data-tasks-select="${escapeAttr(id)}" aria-label="Select ${escapeAttr(id)}"${checked}>
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
  if (!count) {
    container.innerHTML = "";
    return;
  }
  container.innerHTML = `
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

export function bindTasksControlsView(app) {
  const view = app.querySelector(".tasks-view");
  if (!view) return;
  view.querySelectorAll("[data-tasks-state]").forEach((button) => {
    button.addEventListener("click", () => {
      app.tasksState = toggleTasksState(app.tasksState, button.dataset.tasksState);
      writeTasksState(app.tasksState);
      app.load();
    });
  });
  const projectSelect = view.querySelector("[data-tasks-project]");
  if (projectSelect) {
    projectSelect.addEventListener("change", () => {
      app.tasksProject = String(projectSelect.value || "").trim();
      writeTasksProject(app.tasksProject);
      app.load();
    });
  }
  const search = view.querySelector("[data-tasks-search]");
  if (search) {
    const apply = () => {
      const next = String(search.value || "").trim();
      if (next === app.tasksQuery) return;
      app.tasksQuery = next;
      writeTasksQuery(app.tasksQuery);
      app.load();
    };
    search.addEventListener("keydown", (event) => {
      if (event.key === "Enter") apply();
    });
    search.addEventListener("change", apply);
  }
  view.addEventListener("change", (event) => {
    const selectAll = event.target.closest("[data-tasks-select-all]");
    if (selectAll) {
      app.tasksSelected = selectAll.checked
        ? new Set((app.tasksList || []).map((task) => String(value(task, "id", "ID"))))
        : new Set();
      renderTasksListView(app);
      renderTasksBulkBarView(app);
      return;
    }
    const box = event.target.closest("[data-tasks-select]");
    if (!box) return;
    const id = String(box.dataset.tasksSelect || "");
    if (!id) return;
    if (box.checked) {
      app.tasksSelected.add(id);
    } else {
      app.tasksSelected.delete(id);
    }
    renderTasksBulkBarView(app);
  });
  view.addEventListener("click", (event) => {
    if (event.target.closest("[data-tasks-clear]")) {
      app.tasksSelected = new Set();
      renderTasksListView(app);
      renderTasksBulkBarView(app);
      return;
    }
    const applyButton = event.target.closest("[data-tasks-apply]");
    if (applyButton) applyTasksBulkAction(app, applyButton.dataset.tasksApply, view);
  });
}

// applyTasksBulkAction fans one bulk action out over the selected tasks via
// the existing per-task endpoints, then refreshes the list and reports
// per-task failures. Succeeded tasks leave the selection; failed ones stay
// selected so they can be fixed up and retried.
export async function applyTasksBulkAction(app, action, view) {
  const selected = app.tasksSelected || new Set();
  const tasks = (app.tasksList || []).filter((task) => selected.has(String(value(task, "id", "ID"))));
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
      failed.push(`${id}: ${result.reason?.message || result.reason}`);
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
