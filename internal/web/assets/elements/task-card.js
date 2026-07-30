// The board's task card. A fixed four-part structure — meta row, title, step
// rail, activity, quiet meta — so the eye lands in the same place on every
// card. Actions leave the resting card except where the card is asking the
// reader for something.

import { taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { renderStepRail } from "./step-rail.js";
import { define, FlowElement } from "./base.js";

export function renderTaskCard(model) {
  if (!model) return "";
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  return `
    <div class="meta">
      <span class="id">${escapeHTML(model.id)}</span>
      <span class="spacer"></span>
      <span class="dwell" data-tone="${escapeAttr(model.dwellTone)}">${escapeHTML(model.dwellLabel)}</span>
    </div>
    <a class="title" href="${escapeAttr(taskHref(model.projectID, model.id))}" data-link>${escapeHTML(model.title)}</a>
    ${model.running ? `<div class="step">${renderStepRail(model)}</div>` : ""}
    ${model.activity ? `<div class="activity">${renderMarkdown(model.activity)}</div>` : ""}
    ${renderWaitingOn(model)}
    ${renderQuietMeta(model)}
    ${renderCardActions(model, projectAttr)}
  `;
}

// A scheduled card that cannot start names what it is waiting on, linking each
// blocker so the reader can jump straight to the task in the way. Only the
// model's waitingOn list drives it, so a card with no live blockers renders
// nothing here. When the read model bounded the list it says how many titles
// were left off ("+N more") so the reader knows the line is not the full set.
function renderWaitingOn(model) {
  const blockers = model.waitingOn || [];
  if (!blockers.length) return "";
  const links = blockers
    .map((blocker) => {
      const label = blocker.title || blocker.id;
      return `<a href="${escapeAttr(taskHref(model.projectID, blocker.id))}" data-link>${escapeHTML(label)}</a>`;
    })
    .join(", ");
  const omitted = Number(model.waitingOnOmitted || 0);
  const suffix = omitted > 0 ? `, +${omitted} more` : "";
  return `<p class="waiting-on">waiting on ${links}${suffix}</p>`;
}

// The quiet line is trimmed hard: priority, diff stat, reviewer count, and the
// project only when more than one is registered. Branch, change id, session
// state and timestamps live on task detail.
function renderQuietMeta(model) {
  const additions = Number(model.diffStats?.additions ?? model.diffStats?.Additions ?? 0);
  const deletions = Number(model.diffStats?.deletions ?? model.diffStats?.Deletions ?? 0);
  const total = Number(model.checks?.total ?? model.checks?.Total ?? 0);
  const satisfied = Number(model.checks?.satisfied ?? model.checks?.Satisfied ?? 0);
  const parts = [
    model.projectName ? `<span class="project">${escapeHTML(model.projectName)}</span>` : "",
    `p${model.priority}`,
    additions || deletions ? `+${additions} −${deletions}` : "",
    total ? `checks ${satisfied}/${total}` : "",
    // `extra` is how a surface with different needs adds to the line without
    // the card guessing: the Done page wants the change and the closing date,
    // which are noise on a live board.
    ...(model.extra || []).map((fragment) => escapeHTML(fragment)),
  ].filter(Boolean);
  return parts.length ? `<p class="quiet">${parts.join(" · ")}</p>` : "";
}

// A card only carries buttons when it is the thing asking: a waiting card
// shows the question, a failed one offers the retry, an unscheduled one the
// schedule. Everything else surfaces on hover.
function renderCardActions(model, projectAttr) {
  const id = escapeAttr(model.id);
  if (model.waitKind === "gate" || model.waitKind === "question") {
    // Answer deep-links the tab that can actually answer: the Review tab
    // renders the gate panel (or the agent's question) with its feedback box.
    return `
      <div class="ask">
        <div class="reason">${renderMarkdown(model.reason)}</div>
        <div class="actions">
          <a class="button" href="${escapeAttr(taskHref(model.projectID, model.id))}?tab=review" data-link>Answer</a>
          <button class="button secondary" data-card-approve="${id}"${projectAttr}>Approve</button>
        </div>
      </div>
    `;
  }
  if (model.waitKind === "budget") {
    const reviewCycles = String(model.wait?.reason || model.wait?.Reason || "") === "review_cycle_limit";
    return `
      <div class="actions">
        <button class="button" data-workflow-budget="${id}" data-workflow-budget-kind="${reviewCycles ? "review-cycles" : "transitions"}"${projectAttr}>${reviewCycles ? "Grant cycles" : "Extend budget"}</button>
        <a class="button secondary" href="${escapeAttr(taskHref(model.projectID, model.id))}" data-link>Details</a>
      </div>
    `;
  }
  if (model.waitKind === "failed") {
    return `
      <div class="actions">
        <button class="button" data-workflow-retry="${id}"${projectAttr}>Retry</button>
        <a class="button secondary" href="${escapeAttr(taskHref(model.projectID, model.id))}" data-link>Transcript</a>
      </div>
    `;
  }
  if (model.lifecycleState === "unscheduled") {
    return `<div class="actions"><button class="button" data-workflow-schedule="${id}"${projectAttr}>Schedule</button></div>`;
  }
  if (model.readyToMerge) {
    return `<div class="actions"><button class="button" data-card-merge="${id}"${projectAttr}>Merge</button></div>`;
  }
  return `
    <div class="actions on-hover">
      <button class="button secondary" data-workflow-reset="${id}"${projectAttr}>Reset</button>
      <button class="button secondary" data-workflow-hold="${id}"${projectAttr}>Pause</button>
    </div>
  `;
}

export class FlowTaskCard extends FlowElement {
  render(model) {
    if (!model) return "";
    this.setAttribute("data-phase", model.phase);
    this.toggleAttribute("data-needs-you", model.needsYou);
    return renderTaskCard(model);
  }
}

define("flow-task-card", FlowTaskCard);
