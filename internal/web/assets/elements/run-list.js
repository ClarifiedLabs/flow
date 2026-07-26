// The run, not the graph. One row per node *visit* in execution order, so a
// loop or a retry is an extra row rather than an edge crossing the diagram.
// This is the fix for "the SVG is messy": nothing here can overlap.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

export function renderRunList(rows) {
  if (!rows?.length) return `<p class="empty">No steps have run yet</p>`;
  return rows.map(renderRunRow).join("");
}

function renderRunRow(row) {
  if (row.state === "current") return renderCurrentRow(row);
  return `
    <div class="row" data-state="${escapeAttr(row.state)}"${row.artifactID ? ` data-artifact="${escapeAttr(row.artifactID)}"` : ""}>
      <span class="gutter"><span class="dot"></span></span>
      <span class="name">${escapeHTML(row.name)}</span>
      ${row.tag ? `<span class="tag">${escapeHTML(row.tag)}</span>` : ""}
      <span class="spacer"></span>
      ${row.outcome ? `<span class="outcome">${escapeHTML(row.outcome)}</span>` : ""}
      ${row.duration ? `<span class="duration">${escapeHTML(row.duration)}</span>` : ""}
    </div>
    ${row.loop ? `<div class="loop"><span class="elbow"></span>${escapeHTML(row.loop)}</div>` : ""}
  `;
}

// The current node is a card rather than a row, with one line per fanned-out
// child agent: on a review node the interesting question is which reviewer is
// still thinking, not that "review" is running.
function renderCurrentRow(row) {
  return `
    <div class="row is-current" data-state="current">
      <span class="gutter"><span class="dot"></span></span>
      <div class="current">
        <div class="current-head">
          <strong>${escapeHTML(row.name)}</strong>
          <span class="spacer"></span>
          <span class="duration">${escapeHTML(row.duration ? `running ${row.duration}` : "running")}</span>
        </div>
        ${row.jobs.map(renderChildJob).join("")}
      </div>
    </div>
  `;
}

function renderChildJob(job) {
  const state = String(value(job, "state", "State") || "");
  const verdict = String(value(job, "verdict", "Verdict") || "");
  const live = state === "running" || state === "claimed";
  const note = verdict && verdict !== "pending" ? verdict : `${state}${value(job, "id", "ID") ? ` · ${value(job, "id", "ID")}` : ""}`;
  return `
    <div class="child"${live ? " data-live" : ""}>
      <span class="child-dot"></span>
      <span class="child-name">${escapeHTML(value(job, "name", "Name") || value(job, "role", "Role") || "agent")}</span>
      <span class="spacer"></span>
      <span class="child-note">${escapeHTML(note)}</span>
    </div>
  `;
}

export class FlowRunList extends FlowElement {
  render(rows) {
    return renderRunList(rows);
  }
}

define("flow-run-list", FlowRunList);
