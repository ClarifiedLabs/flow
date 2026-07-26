// The compact run spine in the rail: a gutter of dots joined by a line, one
// row per step. Enough to see where the run is without leaving the page you
// are on.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderRunSpine(model) {
  if (!model?.rows?.length) return "";
  // Collapse repeat visits: the rail answers "where is it", not "what
  // happened" — the Overview tab's run list is the history.
  const seen = new Set();
  const steps = [];
  for (const row of model.rows) {
    if (seen.has(row.nodeKey)) continue;
    seen.add(row.nodeKey);
    steps.push(row);
  }
  const runLabel = model.runSequence ? `Run ${model.runSequence}` : "Run";
  return `
    <div class="spine-head"><span class="caption">${escapeHTML(runLabel)}</span></div>
    <ol class="spine">
      ${steps.map(renderSpineRow).join("")}
    </ol>
  `;
}

function renderSpineRow(row) {
  const note = row.state === "current" ? row.duration : row.state === "done" ? "done" : row.state === "failed" ? "failed" : "";
  return `
    <li data-state="${escapeAttr(row.state)}">
      <span class="dot"></span>
      <span class="name">${escapeHTML(row.name)}</span>
      <span class="note">${escapeHTML(note)}</span>
    </li>
  `;
}

export class FlowRunSpine extends FlowElement {
  render(model) {
    return renderRunSpine(model);
  }
}

define("flow-run-spine", FlowRunSpine);
