// The review bar. Replaces the single comment box at the top of the page with
// one verdict that carries every inline note the reviewer drafted while
// reading — which is what makes inline commenting worth having.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderReviewBar(pendingCount, busyVerdict = "") {
  const placeholder = pendingCount
    ? `Overall comment — ${pendingCount} pending inline note${pendingCount === 1 ? "" : "s"} will be posted with this`
    : "Overall comment";
  // While a verdict is in flight every verdict control is suppressed — the
  // change is the mutation target, so a contradictory verdict must not race
  // the pending submission — and the in-flight one carries the busy styling.
  // Rendered into the markup (not painted on after the fact) so a repaint
  // mid-flight cannot hand the reviewer an enabled verdict button.
  const verdictButton = (verdict, label, { primary = false } = {}) => {
    const busy = verdict === busyVerdict;
    const classes = `button${primary ? "" : " secondary"}${busy ? " is-busy" : ""}`;
    return `<button class="${classes}" data-review-verdict="${verdict}"${busy ? ' aria-busy="true"' : ""}${busyVerdict ? " disabled" : ""}>${label}</button>`;
  };
  return `
    <span class="caption">Finish review</span>
    <input name="body" type="text" data-review-body placeholder="${escapeAttr(placeholder)}" autocomplete="off"${busyVerdict ? " disabled" : ""} />
    ${verdictButton("comment", "Comment")}
    ${verdictButton("request_changes", "Request changes")}
    ${verdictButton("approve", "Approve", { primary: true })}
  `;
}

export class FlowReviewBar extends FlowElement {
  render(payload) {
    // The busy verdict comes from the owning change at render time: the
    // in-flight registry lives outside the DOM, so a repaint (poll, draft
    // change) reproduces the suppression instead of losing it.
    return renderReviewBar(payload?.pendingCount || 0, this.closest?.("flow-change")?.reviewBusyVerdict || "");
  }

  get body() {
    return String(this.querySelector("[data-review-body]")?.value || "").trim();
  }
}

define("flow-review-bar", FlowReviewBar);
