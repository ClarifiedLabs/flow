// The step rail: N segments, one per node in the frozen graph, plus the
// current step's name and position. It is the board's answer to "how far in is
// this", readable without reading any words.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

// Long flows would turn the rail into a row of hairlines, so cap the segments
// drawn and let the n/N label carry the exact position.
const MAX_SEGMENTS = 12;

export function renderStepRail({ stepIndex, stepCount, stepName, phase, label = "" }) {
  if (!stepCount) {
    return label ? `<span class="rail-label is-idle">${escapeHTML(label)}</span>` : "";
  }
  const segments = Math.min(stepCount, MAX_SEGMENTS);
  // Map the true position onto the drawn segments so a 20-node flow still
  // fills its rail proportionally.
  const filled = Math.round((stepIndex / stepCount) * segments);
  let marks = "";
  for (let index = 0; index < segments; index += 1) {
    const state = index < filled - 1 ? "done" : index === filled - 1 ? "current" : "todo";
    marks += `<i data-seg="${state}"></i>`;
  }
  const text = stepName ? `${stepName} · ${stepIndex}/${stepCount}` : `${stepIndex}/${stepCount}`;
  return `
    <span class="rail" data-phase="${escapeAttr(phase || "")}" aria-hidden="true">${marks}</span>
    <span class="rail-label">${escapeHTML(text)}</span>
  `;
}

export class FlowStepRail extends FlowElement {
  render(model) {
    if (!model) return "";
    this.setAttribute("data-phase", model.phase || "");
    return renderStepRail(model);
  }
}

define("flow-step-rail", FlowStepRail);
