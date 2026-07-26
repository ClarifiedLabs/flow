// The work-surface tabs. The active tab lives on the element instance, so a
// ten-second poll no longer throws you back to Overview mid-read — which was
// the single most disruptive thing about the old page.

import { TASK_TABS } from "../config.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export function renderTabStrip(active, badges = {}) {
  return TASK_TABS.map(([key, label]) => {
    const badge = badges[key];
    return `
      <button role="tab" data-tab="${escapeAttr(key)}"${key === active ? ' aria-selected="true"' : ""}>
        ${escapeHTML(label)}${renderBadge(badge)}
      </button>
    `;
  }).join("");
}

function renderBadge(badge) {
  if (!badge) return "";
  if (badge.live) return `<span class="live" aria-hidden="true"></span>`;
  if (!badge.text) return "";
  return `<span class="badge" data-tone="${escapeAttr(badge.tone || "")}">${escapeHTML(badge.text)}</span>`;
}

export class FlowTabStrip extends FlowElement {
  active = "overview";

  render(payload) {
    this.setAttribute("role", "tablist");
    return renderTabStrip(this.active, payload?.badges || {});
  }

  handleClick(event) {
    const tab = event.target.closest?.("[data-tab]");
    if (!tab) return;
    this.select(tab.dataset.tab);
  }

  select(key) {
    if (!key || key === this.active) return;
    this.active = key;
    this.invalidate();
    this.dispatchEvent(new CustomEvent("tab-change", { detail: key, bubbles: true }));
  }
}

define("flow-tab-strip", FlowTabStrip);
