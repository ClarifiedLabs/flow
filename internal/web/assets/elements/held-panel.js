// Held work. Manual intervention and convergence decisions are first-class
// lifecycle states: the run has stopped and handing it back is an explicit
// choice of which edge to take.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { TERMINAL_ICON } from "../terminal.js";
import { define, FlowElement } from "./base.js";

// HAND_BACK_EDGES map one-for-one onto executor actions, so the button says
// what will happen rather than something vague like "continue".
export const HAND_BACK_EDGES = [
  ["resume", "Resume at", true],
  ["submit", "Send to review", false],
  ["satisfy", "Mark step done", false],
  ["merge", "Skip to merge", false],
];

export function renderHeldPanel(model) {
  if (!model?.held) return "";
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const session = value(model.taskConsole || {}, "session", "Session");
  const sessionID = value(session || {}, "id", "ID");
  const workerID = value(session || {}, "worker_id", "WorkerID");
  const convergenceHold = String(model.heldBy || "") === "system";
  const latestPlan = (model.statusLog || []).find(
    (entry) => String(value(entry, "kind", "Kind") || "") === "plan",
  );
  const convergenceMessage = convergenceHold
    ? String(value(latestPlan || {}, "message", "Message") || "")
    : "";

  return `
    <div class="head">
      <span class="badge"><span class="dot"></span>${convergenceHold ? "Convergence review" : "Held by you"}</span>
      <span class="line">paused at ${escapeHTML(model.stepName)} · the workflow will not advance</span>
    </div>
    ${convergenceMessage ? `<div class="prose">${escapeHTML(convergenceMessage)}</div>` : ""}
    ${sessionID ? renderSession(sessionID, workerID) : ""}
    <div class="hand-back">
      <span class="caption">Hand back</span>
      ${HAND_BACK_EDGES.map(([edge, label, primary]) => {
        const text = edge === "resume" ? `${label} ${model.stepName}` : label;
        return `<button class="button${primary ? "" : " secondary"}" data-workflow-release="${escapeAttr(model.id)}" data-edge="${escapeAttr(edge)}"${projectAttr}>${escapeHTML(text)}</button>`;
      }).join("")}
    </div>
  `;
}

function renderSession(sessionID, workerID) {
  return `
    <div class="session">
      <div class="chrome">
        ${TERMINAL_ICON}
        <span>${escapeHTML(sessionID)}${workerID ? ` · ${escapeHTML(workerID)}` : ""} · tmux</span>
        <span class="spacer"></span>
        <button class="pop-out" data-terminal-popout data-session="${escapeAttr(sessionID)}">Pop out</button>
      </div>
      <div class="frame" data-terminal="${escapeAttr(sessionID)}" data-inline-terminal-anchor></div>
    </div>
  `;
}

export class FlowHeldPanel extends FlowElement {
  render(model) {
    const html = renderHeldPanel(model);
    this.toggleAttribute("hidden", !html);
    return html;
  }
}

define("flow-held-panel", FlowHeldPanel);
