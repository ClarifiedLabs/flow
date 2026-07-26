// The review bar. Replaces the single comment box at the top of the page with
// one verdict that carries every inline note the reviewer drafted while
// reading — which is what makes inline commenting worth having.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderReviewBar(pendingCount) {
  const placeholder = pendingCount
    ? `Overall comment — ${pendingCount} pending inline note${pendingCount === 1 ? "" : "s"} will be posted with this`
    : "Overall comment";
  return `
    <span class="caption">Finish review</span>
    <input name="body" type="text" data-review-body placeholder="${escapeAttr(placeholder)}" autocomplete="off" />
    <button class="button secondary" data-review-verdict="comment">Comment</button>
    <button class="button secondary" data-review-verdict="request_changes">Request changes</button>
    <button class="button" data-review-verdict="approve">Approve</button>
  `;
}

export class FlowReviewBar extends FlowElement {
  render(payload) {
    return renderReviewBar(payload?.pendingCount || 0);
  }

  get body() {
    return String(this.querySelector("[data-review-body]")?.value || "").trim();
  }
}

define("flow-review-bar", FlowReviewBar);
