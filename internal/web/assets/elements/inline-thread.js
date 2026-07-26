// A review thread, rendered as the next sibling of the line it anchors to.
// Anchoring is the point: a thread listed in a panel below the diff makes the
// reader hold a file and a line number in their head while they scroll.

import { formatRelative } from "../format.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

export function renderInlineThread(thread, change) {
  if (!thread) return "";
  const id = value(thread, "id", "ID");
  const state = String(value(thread, "state", "State") || "open");
  const comments = value(thread, "comments", "Comments") || [];
  const first = comments[0] || {};
  const anchor = String(value(thread, "anchor_commit_sha", "AnchorCommitSHA") || "");
  const head = String(value(change || {}, "head_sha", "HeadSHA") || "");
  const outdated = Boolean(anchor && head && anchor !== head);

  return `
    <div class="card" data-state="${escapeAttr(state)}">
      <div class="head">
        <span class="actor">${escapeHTML(value(first, "actor", "Actor") || value(thread, "actor", "Actor") || "review")}</span>
        <span class="meta">${escapeHTML(id)} · ${escapeHTML(formatRelative(value(thread, "created_at", "CreatedAt")))}</span>
        <span class="spacer"></span>
        ${outdated ? `<span class="flag">outdated anchor</span>` : ""}
        <span class="state">${escapeHTML(state === "open" ? "open · blocks merge" : state)}</span>
      </div>
      ${comments.map(renderComment).join("")}
      <form class="reply" data-thread-reply-form="${escapeAttr(id)}">
        <input name="body" type="text" placeholder="Reply…" autocomplete="off" />
      </form>
      ${state === "open" ? renderClaims(id) : ""}
    </div>
  `;
}

function renderComment(comment) {
  return `
    <div class="comment">
      <div class="comment-head">
        <span class="actor">${escapeHTML(value(comment, "actor", "Actor") || "")}</span>
        <span class="meta">${escapeHTML(formatRelative(value(comment, "created_at", "CreatedAt")))}</span>
      </div>
      <div class="prose">${renderMarkdown(value(comment, "body", "Body") || "")}</div>
    </div>
  `;
}

// The same verbs the CLI uses, so a thread claimed from the terminal and one
// claimed from the browser mean the same thing.
function renderClaims(id) {
  return `
    <div class="claims">
      <button class="button secondary" data-thread-claim="${escapeAttr(id)}" data-claim-kind="fixed">Claim fixed</button>
      <button class="button secondary" data-thread-claim="${escapeAttr(id)}" data-claim-kind="not_warranted">Not warranted</button>
      <button class="button secondary" data-thread-claim="${escapeAttr(id)}" data-claim-kind="superseded">Superseded</button>
    </div>
  `;
}

export class FlowInlineThread extends FlowElement {
  render(payload) {
    return renderInlineThread(payload?.thread, payload?.change);
  }
}

define("flow-inline-thread", FlowInlineThread);
