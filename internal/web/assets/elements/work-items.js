// Typed task/epic/feature organization tree. Execution remains on the task
// board; this view explains hierarchy, inherited branch scope, and blockers.

import { FlowElement } from "./base.js";
import { epicHref, featureHref, taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";

function itemHref(item, projectID) {
  const id = value(item, "id", "ID");
  switch (value(item, "kind", "Kind")) {
    case "epic": return epicHref(projectID, id);
    case "feature": return featureHref(projectID, id);
    default: return taskHref(projectID, id);
  }
}

function itemNode(item, children, projectID) {
  const id = value(item, "id", "ID");
  const kind = value(item, "kind", "Kind") || "task";
  const title = value(item, "title", "Title") || id;
  const state = value(item, "state", "State") || {};
  const status = value(state, "status", "Status") || "unknown";
  const capabilities = value(item, "capabilities", "Capabilities") || {};
  const canStart = Boolean(value(capabilities, "can_start", "CanStart"));
  const terminal = Boolean(value(state, "terminal", "Terminal"));
  const blockers = Number(value(item, "unresolved_blockers", "UnresolvedBlockers") || 0);
  const feature = value(item, "effective_feature_id", "EffectiveFeatureID");
  const descendants = children.get(id) || [];
  const start = canStart && !terminal
    ? `<button type="button" class="secondary" data-${kind}-start="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Start</button>`
    : "";
  return `<li>
    <div class="feature-row" data-kind="${escapeAttr(kind)}">
      <span class="feature-status" data-status="${escapeAttr(status)}">${escapeHTML(kind)}</span>
      <a class="feature-title" href="${escapeAttr(itemHref(item, projectID))}" data-link>${escapeHTML(title)}</a>
      <span class="feature-id">${escapeHTML(id)}</span>
      <span class="feature-counts">${escapeHTML(status)}</span>
      ${blockers ? `<span class="feature-rebasing">${blockers} blocker${blockers === 1 ? "" : "s"}</span>` : ""}
      ${feature && feature !== id ? `<span class="feature-divergence">branch ${escapeHTML(feature)}</span>` : ""}
      ${start}
    </div>
    ${descendants.length ? `<ul>${descendants.map((child) => itemNode(child, children, projectID)).join("")}</ul>` : ""}
  </li>`;
}

export function renderWorkItems(data) {
  const items = (value(data, "items", "Items") || []).slice();
  const projectID = String(data.projectID || "");
  const byID = new Map(items.map((item) => [value(item, "id", "ID"), item]));
  const children = new Map();
  const roots = [];
  for (const item of items) {
    const parent = value(item, "parent_item_id", "ParentItemID");
    if (!parent || !byID.has(parent)) {
      roots.push(item);
      continue;
    }
    if (!children.has(parent)) children.set(parent, []);
    children.get(parent).push(item);
  }
  const compare = (a, b) => String(value(a, "title", "Title")).localeCompare(String(value(b, "title", "Title")));
  roots.sort(compare);
  for (const list of children.values()) list.sort(compare);
  const projectName = String(data.projectName || "");
  return `<section class="detail">
    <div class="head"><div class="title-row">
      <h2>Work Items${projectName ? ` · ${escapeHTML(projectName)}` : ""}</h2>
      <span class="spacer"></span><span class="count">${items.length} total</span>
    </div></div>
    <p class="legend">Organizational hierarchy across executable tasks, aggregate epics, and branch-backed features.</p>
    <form class="feature-create" data-epic-form="" data-project="${escapeAttr(projectID)}">
      <input name="title" placeholder="Epic title" required aria-label="Epic title">
      <textarea name="body" rows="2" placeholder="What this epic coordinates (optional)" aria-label="Epic body"></textarea>
      <input name="parent_item_id" placeholder="Parent epic or feature ID (optional)" aria-label="Parent work item ID">
      <input name="priority" type="number" min="0" value="0" aria-label="Epic priority">
      <select name="completion_policy" aria-label="Epic completion policy">
        <option value="all_children">Complete with all children</option>
        <option value="manual">Manual completion</option>
      </select>
      <div><button type="submit">Create epic</button></div>
    </form>
    ${roots.length ? `<ul class="members">${roots.map((item) => itemNode(item, children, projectID)).join("")}</ul>` : `<p class="empty">No work items yet.</p>`}
  </section>`;
}

class FlowWorkItems extends FlowElement {
  render() {
    return renderWorkItems(this.data || {});
  }
}

customElements.define("flow-work-items", FlowWorkItems);
