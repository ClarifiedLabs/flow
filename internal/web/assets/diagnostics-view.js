// Workers and jobs diagnostics views. Functions take the FlowApp instance and
// render into it; the class keeps thin shim methods that delegate here.

import { apiGet } from "./api.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { renderJobRow, renderQueueSummary, renderWorkerRow } from "./queue.js";

export async function renderWorkersView(app, context) {
  const data = await apiGet("/v1/workers");
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Workers");
  const workers = data.workers || data.Workers || [];
  const diagnostics = data.diagnostics || data.Diagnostics || {};
  const queue = data.queue || data.Queue || {};
  app.querySelector(".content").innerHTML = `
    ${renderQueueSummary(queue)}
    <section class="table-wrap">
      <table>
        <thead><tr><th>Worker</th><th>Status</th><th>Capacity</th><th>Live</th><th>Labels</th><th>Taints</th><th>Heartbeat</th></tr></thead>
        <tbody>${workers.map((worker) => renderWorkerRow(worker, diagnostics[value(worker, "id", "ID")] || {})).join("") || `<tr><td colspan="7">No workers</td></tr>`}</tbody>
      </table>
    </section>
  `;
  return true;
}

// JOBS_COLUMN_COUNT matches the number of <th> in renderJobsView's table head.
const JOBS_COLUMN_COUNT = 9;

// jobSortKey extracts the value used to order job rows for a given sort field.
function jobSortKey(job, diagnostics, field) {
  if (field === "created") return value(job, "created_at", "CreatedAt") || "";
  return value(job, "updated_at", "UpdatedAt") || "";
}

// filterAndSortJobs applies the current project filter and sort selection to
// the loaded jobs before rendering. The server already returns jobs sorted by
// updated_at desc, but the UI re-applies the chosen sort so toggling the
// control is immediate and survives poll refreshes.
function filterAndSortJobs(jobs, diagnostics, filter, sort) {
  const field = sort.field === "created" ? "created" : "updated";
  const descending = sort.order !== "asc";
  const filtered = jobs.filter((job) => {
    if (filter === "" || filter === "__all__") return true;
    const name = value(diagnostics[value(job, "id", "ID")] || {}, "project_name", "ProjectName") || "";
    return name === filter;
  });
  const sorted = filtered.slice().sort((a, b) => {
    const ka = jobSortKey(a, diagnostics, field);
    const kb = jobSortKey(b, diagnostics, field);
    if (ka !== kb) return descending ? (ka < kb ? 1 : -1) : (ka < kb ? -1 : 1);
    // Stable tiebreaker: id descending (matches the server default) or ascending.
    const ia = value(a, "id", "ID") || "";
    const ib = value(b, "id", "ID") || "";
    return descending ? (ia < ib ? 1 : -1) : (ia < ib ? -1 : 1);
  });
  return sorted;
}

export async function renderJobsView(app, context) {
  const data = await apiGet("/v1/jobs");
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Jobs");
  const jobs = data.jobs || data.Jobs || [];
  const diagnostics = data.diagnostics || data.Diagnostics || {};

  // Per-view filter/sort state lives on the app so it survives poll refreshes.
  if (!app.jobsView) app.jobsView = { filter: "__all__", sort: { field: "updated", order: "desc" } };
  const view = app.jobsView;

  const projectNames = Array.from(new Set(
    jobs
      .map((job) => value(diagnostics[value(job, "id", "ID")] || {}, "project_name", "ProjectName") || "")
      .filter(Boolean),
  )).sort();

  const visible = filterAndSortJobs(jobs, diagnostics, view.filter, view.sort);

  const refresh = () => renderJobsView(app, context);
  const projectOptions = ["__all__", ...projectNames]
    .map((name) => `<option value="${escapeAttr(name)}"${name === view.filter ? " selected" : ""}>${escapeHTML(name === "__all__" ? "All projects" : name)}</option>`)
    .join("");
  const sortFieldOptions = [["updated", "Updated"], ["created", "Created"]]
    .map(([val, label]) => `<option value="${escapeAttr(val)}"${val === view.sort.field ? " selected" : ""}>${escapeHTML(label)}</option>`)
    .join("");
  const sortOrderOptions = [["desc", "Newest first"], ["asc", "Oldest first"]]
    .map(([val, label]) => `<option value="${escapeAttr(val)}"${val === view.sort.order ? " selected" : ""}>${escapeHTML(label)}</option>`)
    .join("");

  app.querySelector(".content").innerHTML = `
    <section class="table-wrap">
      <div class="jobs-controls">
        <label>Project <select data-jobs-filter>${projectOptions}</select></label>
        <label>Sort by <select data-jobs-sort-field>${sortFieldOptions}</select></label>
        <label>Order <select data-jobs-sort-order>${sortOrderOptions}</select></label>
      </div>
      <table>
        <thead><tr><th>Job</th><th>State</th><th>Project</th><th>Role</th><th>Target</th><th>Worker</th><th>Lease</th><th>Tmux</th><th>Updated</th></tr></thead>
        <tbody>${visible.map((job) => renderJobRow(job, diagnostics[value(job, "id", "ID")] || {})).join("") || `<tr><td colspan="${JOBS_COLUMN_COUNT}">No jobs</td></tr>`}</tbody>
      </table>
    </section>
  `;

  // Wire filter/sort controls. Some lightweight test stubs return objects
  // without addEventListener, so guard each binding rather than throwing.
  const bindChange = (selector, apply) => {
    const select = app.querySelector(selector);
    if (select && typeof select.addEventListener === "function") {
      select.addEventListener("change", () => {
        apply(select.value);
        renderJobsTable(app, jobs, diagnostics, view, context);
      });
    }
  };
  bindChange("[data-jobs-filter]", (value) => { view.filter = value; });
  bindChange("[data-jobs-sort-field]", (value) => { view.sort.field = value === "created" ? "created" : "updated"; });
  bindChange("[data-jobs-sort-order]", (value) => { view.sort.order = value === "asc" ? "asc" : "desc"; });

  app.bindIssueActions(refresh);
  return true;
}

// renderJobsTable re-renders just the table body (and selection state) without
// refetching, so changing a filter/sort control is instant.
function renderJobsTable(app, jobs, diagnostics, view, context) {
  const visible = filterAndSortJobs(jobs, diagnostics, view.filter, view.sort);
  const body = app.querySelector(".table-wrap table tbody");
  if (body) {
    body.innerHTML = visible.map((job) => renderJobRow(job, diagnostics[value(job, "id", "ID")] || {})).join("") || `<tr><td colspan="${JOBS_COLUMN_COUNT}">No jobs</td></tr>`;
  }
}
