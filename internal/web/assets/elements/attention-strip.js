// The "Needs you" strip. Only rendered when something is actually waiting on a
// human — the board's normal state is silence, and a permanently-present empty
// strip would teach people to stop reading it.

import { taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderAttentionStrip(models) {
  if (!models || !models.length) return "";
  // Oldest wait first: the thing that has been ignored longest is the thing
  // most likely to be forgotten.
  const rows = [...models].sort((left, right) => {
    const leftMs = Date.parse(left.dwellSince || "") || 0;
    const rightMs = Date.parse(right.dwellSince || "") || 0;
    return leftMs - rightMs;
  });
  const oldest = rows[0]?.dwell || "";
  return `
    <div class="head">
      <span class="label">Needs you</span>
      <span class="count">${rows.length}</span>
      <span class="spacer"></span>
      ${oldest ? `<span class="oldest">oldest ${escapeHTML(oldest)}</span>` : ""}
    </div>
    ${rows.map(renderAttentionRow).join("")}
  `;
}

function renderAttentionRow(model) {
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const action = model.actionLabel
    ? model.actionLabel === "Merge"
      ? `<button class="button" data-attention-merge="${escapeAttr(model.id)}"${projectAttr}>Merge</button>`
      : `<a class="button" href="${escapeAttr(taskHref(model.projectID, model.id))}" data-link>${escapeHTML(model.actionLabel)}</a>`
    : "";
  return `
    <div class="row" data-phase="${escapeAttr(model.phase)}">
      <span class="dot"></span>
      <a class="id" href="${escapeAttr(taskHref(model.projectID, model.id))}" data-link>${escapeHTML(model.id)}</a>
      <span class="reason">${escapeHTML(model.reason)}</span>
      <span class="spacer"></span>
      <span class="age" data-tone="${escapeAttr(model.dwellTone)}">${escapeHTML(model.dwell)}</span>
      ${action}
    </div>
  `;
}

export class FlowAttentionStrip extends FlowElement {
  render(models) {
    const html = renderAttentionStrip(models);
    this.toggleAttribute("hidden", !html);
    return html;
  }
}

define("flow-attention-strip", FlowAttentionStrip);
