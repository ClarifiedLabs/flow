// Workers and jobs diagnostics element. The routes fetch /v2/workers or
// /v2/jobs and mount the element with the payload; the jobs view's filter and
// sort selections are element fields, so they survive the poll's remount
// (mount() reuses the element and only its data moves).

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { renderJobRow, renderQueueSummary, renderWorkerRow } from "../queue.js";
import { define, FlowElement } from "./base.js";

// JOBS_COLUMN_COUNT matches the number of <th> in the jobs table head.
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
export function filterAndSortJobs(jobs, diagnostics, filter, sort) {
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

export function renderWorkersTable({ workers = [], diagnostics = {}, queue = {} } = {}) {
  return `
    ${renderQueueSummary(queue)}
    <section class="table-wrap">
      <table>
        <thead><tr><th>Worker</th><th>Status</th><th>Capacity</th><th>Live</th><th>Labels</th><th>Taints</th><th>Heartbeat</th></tr></thead>
        <tbody>${workers.map((worker) => renderWorkerRow(worker, diagnostics[value(worker, "id", "ID")] || {})).join("") || `<tr><td colspan="7">No workers</td></tr>`}</tbody>
      </table>
    </section>
  `;
}

export function renderJobsTable({ jobs = [], diagnostics = {}, filter = "__all__", sort = { field: "updated", order: "desc" } } = {}) {
  const projectNames = Array.from(new Set(
    jobs
      .map((job) => value(diagnostics[value(job, "id", "ID")] || {}, "project_name", "ProjectName") || "")
      .filter(Boolean),
  )).sort();

  const visible = filterAndSortJobs(jobs, diagnostics, filter, sort);

  const projectOptions = ["__all__", ...projectNames]
    .map((name) => `<option value="${escapeAttr(name)}"${name === filter ? " selected" : ""}>${escapeHTML(name === "__all__" ? "All projects" : name)}</option>`)
    .join("");
  const sortFieldOptions = [["updated", "Updated"], ["created", "Created"]]
    .map(([val, label]) => `<option value="${escapeAttr(val)}"${val === sort.field ? " selected" : ""}>${escapeHTML(label)}</option>`)
    .join("");
  const sortOrderOptions = [["desc", "Newest first"], ["asc", "Oldest first"]]
    .map(([val, label]) => `<option value="${escapeAttr(val)}"${val === sort.order ? " selected" : ""}>${escapeHTML(label)}</option>`)
    .join("");

  return `
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
}

export class FlowDiagnostics extends FlowElement {
  // The jobs view's selections. Instance fields, so a poll remount (new data,
  // same element) keeps them.
  filter = "__all__";
  sort = { field: "updated", order: "desc" };

  // data: { kind: "workers"|"jobs", ...payload }
  render() {
    const data = this.data || {};
    if (data.kind === "workers") {
      return renderWorkersTable({
        workers: data.workers || data.Workers || [],
        diagnostics: data.diagnostics || data.Diagnostics || {},
        queue: data.queue || data.Queue || {},
      });
    }
    return renderJobsTable({
      jobs: data.jobs || data.Jobs || [],
      diagnostics: data.diagnostics || data.Diagnostics || {},
      filter: this.filter,
      sort: this.sort,
    });
  }

  bind() {
    // Delegated on the element so the controls' listeners survive repaints.
    this.addEventListener("change", (event) => {
      if (event.target?.closest?.("[data-jobs-filter]")) {
        this.filter = event.target.closest("[data-jobs-filter]").value;
      } else if (event.target?.closest?.("[data-jobs-sort-field]")) {
        this.sort.field = event.target.closest("[data-jobs-sort-field]").value === "created" ? "created" : "updated";
      } else if (event.target?.closest?.("[data-jobs-sort-order]")) {
        this.sort.order = event.target.closest("[data-jobs-sort-order]").value === "asc" ? "asc" : "desc";
      } else {
        return;
      }
      this.invalidate();
    });
  }
}

define("flow-diagnostics", FlowDiagnostics);
