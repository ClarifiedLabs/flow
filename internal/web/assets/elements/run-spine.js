// The compact run spine in the rail: a gutter of dots joined by a line, one
// row per step. Enough to see where the run is without leaving the page you
// are on.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderRunSpine(model) {
  if (!model?.rows?.length) return "";
  // Collapse repeat visits: the rail answers "where is it", not "what
  // happened" — the Overview tab's run list is the history. Keep the *latest*
  // visit of each node, at its first position: a loop-back (say a merge
  // conflict sends the run back to implement) re-runs nodes, and the row the
  // run sits on now must be the one the spine draws as current — the first
  // pass is stale and must not leave every node reading "done".
  const positionByKey = new Map();
  const steps = [];
  for (const row of model.rows) {
    const position = positionByKey.get(row.nodeKey);
    if (position === undefined) {
      positionByKey.set(row.nodeKey, steps.length);
      steps.push(row);
    } else {
      steps[position] = row;
    }
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
