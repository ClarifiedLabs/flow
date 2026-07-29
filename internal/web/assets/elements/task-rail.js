// The sticky context rail. Identity, the current step, the run so far, the
// controls that steer it, and the fixed facts — all of it stays put while the
// work surface scrolls, so you never lose track of which task you are looking
// at or what it is doing.

import { taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";
import "./run-spine.js";
import "./task-relations.js";

export function renderTaskRail(model) {
  if (!model) return "";
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  return `
    <div class="identity">
      <span class="task-id">${escapeHTML(model.id)}</span>
      <h2>${escapeHTML(model.title)}</h2>
      <p class="quiet">${escapeHTML(railQuietMeta(model))}</p>
    </div>
    ${renderCurrentStep(model)}
    <flow-run-spine></flow-run-spine>
    ${renderRunControls(model, projectAttr)}
    ${renderFacts(model)}
    <flow-task-relations></flow-task-relations>
    ${model.epicID ? `<a class="epic" href="${escapeAttr(taskHref(model.projectID, model.epicID))}/epic" data-link><span class="caption">Epic</span>${escapeHTML(model.epicID)}</a>` : ""}
  `;
}

function railQuietMeta(model) {
  return [`p${model.priority}`, model.createdBy, model.projectName].filter(Boolean).join(" · ");
}

// The current-step card is the answer to "what is it doing right now", stated
// as a sentence rather than as a node key.
function renderCurrentStep(model) {
  if (!model.stepCount) return "";
  if (model.held) {
    const session = value(model.taskConsole || {}, "session", "Session");
    return `
      <div class="current" data-phase="triage">
        <span class="caption">Held by you</span>
        <strong>Paused at ${escapeHTML(model.stepName)}</strong>
        <span class="current-meta">manual session${session ? ` · ${escapeHTML(value(session, "id", "ID"))}` : ""}</span>
      </div>
    `;
  }
  const meta = [
    model.stepName,
    model.stepCount ? `${model.stepIndex}/${model.stepCount}` : "",
    model.dwell,
    model.budgetTotal ? `budget ${model.budgetUsed}/${model.budgetTotal}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return `
    <div class="current" data-phase="${escapeAttr(model.waitKind ? "await" : "authoring")}">
      <span class="caption">Current step</span>
      <strong>${escapeHTML(model.activity)}</strong>
      <span class="current-meta">${escapeHTML(meta)}</span>
    </div>
  `;
}

// Run controls are the knobs for steering an in-flight task. Held runs get a
// different set, because the only useful thing to do with a held run is decide
// how to give it back.
function renderRunControls(model, projectAttr) {
  if (!model.runID) return "";
  const id = escapeAttr(model.id);
  const controls = model.held
    ? `
      <button class="button" data-workflow-release="${id}" data-edge="resume"${projectAttr}>Resume</button>
      <button class="button secondary" data-workflow-reset="${id}"${projectAttr}>Reset</button>
    `
    : `
      <button class="button secondary" data-workflow-hold="${id}"${projectAttr}>Pause</button>
      <button class="button" data-workflow-take-over="${id}"${projectAttr}>Take over</button>
      <button class="button secondary" data-workflow-skip="${id}" data-workflow-skip-node="${escapeAttr(model.nodeRunID)}"${projectAttr}>Skip step</button>
      <button class="button secondary" data-workflow-reset="${id}"${projectAttr}>Reset</button>
    `;
  return `<div class="controls"><span class="caption">Run controls</span><div class="control-row">${controls}</div></div>`;
}

// Fixed facts, absolute rather than relative: a grid you read off, not a feed.
function renderFacts(model) {
  const change = model.change || {};
  const session = model.activeSession || {};
  const facts = [
    ["Branch", value(change, "branch", "Branch") || value(session, "branch", "Branch")],
    ["Head", shortSHA(value(change, "head_sha", "HeadSHA"))],
    ["Budget", model.budgetTotal ? `${model.budgetUsed}/${model.budgetTotal}` : ""],
    ["Worker", value(session, "worker_id", "WorkerID")],
  ].filter(([, fact]) => fact);
  if (!facts.length) return "";
  return `
    <div class="facts">
      ${facts
        .map(
          ([label, fact]) =>
            `<div><span class="caption">${escapeHTML(label)}</span><span class="fact">${escapeHTML(fact)}</span></div>`,
        )
        .join("")}
    </div>
  `;
}

function shortSHA(sha) {
  const text = String(sha || "");
  return text.length > 12 ? text.slice(0, 12) : text;
}

export class FlowTaskRail extends FlowElement {
  render(model) {
    return renderTaskRail(model);
  }

  // The base paint skips the write — and with it afterPaint — when the markup
  // is unchanged, but a refresh that only changed a blocker's state leaves the
  // rail markup identical while the relations child still needs the fresh
  // model. Forward on every paint attempt, not just on writes.
  paint() {
    super.paint();
    this.syncChildren();
  }

  afterPaint() {
    this.syncChildren();
  }

  syncChildren() {
    const spine = this.querySelector("flow-run-spine");
    if (spine) spine.data = this.data;
    const relations = this.querySelector("flow-task-relations");
    if (relations) relations.data = this.data;
  }
}

define("flow-task-rail", FlowTaskRail);
