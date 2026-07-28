// The Now card: the single most important thing on the page, above the tabs so
// it is never scrolled past. It renders when a workflow wait is open or an
// open review thread blocks the merge, and not otherwise — a card that is
// always there stops being read.

import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { define, FlowElement } from "./base.js";

export function renderNowCard(card, model) {
  if (!card) return "";
  const projectAttr = model?.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  return `
    <div class="head">
      <strong>${escapeHTML(card.heading)}</strong>
      <span class="spacer"></span>
      ${card.age ? `<span class="age">opened ${escapeHTML(card.age)} ago</span>` : ""}
    </div>
    <div class="body">
      <div class="who">
        <span class="actor">${escapeHTML(card.actor || "")}</span>
        ${card.locus ? `<span class="locus">${escapeHTML(card.locus)}</span>` : ""}
        <span class="spacer"></span>
        ${card.outdated ? `<span class="flag">outdated anchor</span>` : ""}
      </div>
      ${card.body ? `<div class="prose">${renderMarkdown(card.body)}</div>` : ""}
      <div class="actions">
        ${(card.actions || []).map((action) => renderNowAction(action, card, model, projectAttr)).join("")}
      </div>
    </div>
  `;
}

function renderNowAction(action, card, model, projectAttr) {
  const classes = action.primary ? "button" : "button secondary";
  const id = escapeAttr(model?.id || "");
  switch (action.key) {
    case "workflow-retry":
      return `<button class="${classes}" data-workflow-retry="${id}"${projectAttr}>${escapeHTML(action.label)}</button>`;
    case "workflow-skip":
      return `<button class="${classes}" data-workflow-skip="${id}" data-workflow-skip-node="${escapeAttr(model?.nodeRunID || "")}"${projectAttr}>${escapeHTML(action.label)}</button>`;
    case "workflow-budget":
      return `<button class="${classes}" data-workflow-budget="${id}" data-workflow-budget-kind="${escapeAttr(action.budgetKind || "transitions")}"${projectAttr}>${escapeHTML(action.label)}</button>`;
    case "thread-claim":
      return `<button class="${classes}" data-thread-claim="${escapeAttr(card.threadID || "")}" data-claim-kind="${escapeAttr(action.kind)}">${escapeHTML(action.label)}</button>`;
    case "open-change":
      return `<a class="${classes}" href="/ui/changes/${escapeAttr(model?.change?.id || model?.change?.ID || "")}" data-link>${escapeHTML(action.label)}</a>`;
    case "focus-gate":
      return `<button class="${classes}" data-focus-tab="${escapeAttr(action.tab || "review")}">${escapeHTML(action.label)}</button>`;
    default:
      return `<button class="${classes}">${escapeHTML(action.label)}</button>`;
  }
}

export class FlowNowCard extends FlowElement {
  render(payload) {
    const html = renderNowCard(payload?.card, payload?.model);
    this.toggleAttribute("hidden", !html);
    if (payload?.card) this.setAttribute("data-tone", payload.card.tone || "await");
    return html;
  }
}

define("flow-now-card", FlowNowCard);
