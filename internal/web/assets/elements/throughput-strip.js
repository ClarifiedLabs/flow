// The throughput strip: how much work has landed recently, rendered from the
// pre-aggregated /v2/stats/completions buckets. It is a pure renderer — the
// board's reconcile-in-place poll hands it fresh data on the existing element,
// so a chip keeps its hover and focus while the numbers on it change. While
// stats are null or still loading it renders nothing: an empty indicator is
// silent, like the attention strip.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

const DONE_HREF = "/ui/tasks?state=done";

export function renderThroughputStrip(data) {
  const buckets = value(data, "buckets", "Buckets");
  if (!Array.isArray(buckets) || !buckets.length) return "";
  return `
    <div class="head">
      <span class="mark" aria-hidden="true">✓</span>
      <span class="label">Done</span>
      ${buckets.map(renderChip).join("")}
      <span class="spacer"></span>
      <a class="all" href="${DONE_HREF}" data-link>view all</a>
    </div>
  `;
}

function renderChip(bucket) {
  const count = Number(value(bucket, "count", "Count")) || 0;
  const window = value(bucket, "window", "Window");
  return `
    <a class="chip" href="${DONE_HREF}" data-link title="${escapeAttr(`${count} done in the last ${window}`)}">
      <span class="count">${count}</span>
      <span class="sep"> · </span>
      <span class="window">${escapeHTML(String(window))}</span>
    </a>
  `;
}

export class FlowThroughputStrip extends FlowElement {
  render(data) {
    const html = renderThroughputStrip(data);
    this.toggleAttribute("hidden", !html);
    return html;
  }
}

define("flow-throughput-strip", FlowThroughputStrip);
