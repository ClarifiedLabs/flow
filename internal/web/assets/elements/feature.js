// The feature detail: header with status and branch, the live divergence
// against the base, rebase/land/archive actions, the assigned tasks, and the
// rebase history. The finalize path and the block semantics live server-side;
// this page only reports and triggers them.

import { FlowElement } from "./base.js";
import { epicHref, featureHref, projectTaskHref, taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { formatDate, formatRelative, shortSHA } from "../format.js";
import { featureCountsLabel, featureDivergenceLabel } from "./features.js";
import { renderWorkItemContext } from "../work-item-detail.js";

function taskStateLabel(task) {
  const state = value(task, "state", "State");
  if (!state) return "open";
  const resolution = value(task, "done_resolution", "DoneResolution");
  return resolution ? `${state} (${resolution})` : String(state).replace(/_/g, " ");
}

function taskRow(task, projectID, navigation = {}) {
  const id = value(task, "id", "ID");
  const title = value(task, "title", "Title") || id;
  const state = taskStateLabel(task);
  return `
    <a class="member" href="${escapeAttr(projectTaskHref(projectID, id, navigation))}" ${state.startsWith("done") ? "data-merged" : ""} data-link>
      <span class="member-id">${escapeHTML(id)}</span>
      <span class="member-title">${escapeHTML(title)}</span>
      <span class="member-note">${escapeHTML(state)}</span>
    </a>`;
}

function rebaseRow(rebase, projectID, navigation = {}) {
  const id = value(rebase, "id", "ID");
  const state = value(rebase, "state", "State") || "";
  const taskID = value(rebase, "task_id", "TaskID");
  const oldTip = shortSHA(value(rebase, "old_tip_sha", "OldTipSHA"));
  const newTip = shortSHA(value(rebase, "new_tip_sha", "NewTipSHA"));
  const created = value(rebase, "created_at", "CreatedAt");
  return `
    <div class="member" data-state="${escapeAttr(state)}">
      <span class="member-id">${escapeHTML(id)}</span>
      <span class="member-title">
        ${escapeHTML(state)}${newTip ? ` · ${escapeHTML(oldTip)} → ${escapeHTML(newTip)}` : ` · from ${escapeHTML(oldTip)}`}
      </span>
      ${taskID ? `<a class="member-note" href="${escapeAttr(projectTaskHref(projectID, taskID, navigation))}" data-link>${escapeHTML(taskID)}</a>` : ""}
      <span class="member-note">${escapeHTML(formatRelative(created))}</span>
    </div>`;
}

export function renderFeature(data) {
  const projectID = String(data.projectID || "");
  const feature = value(data, "feature", "Feature") || {};
  const counts = value(data, "counts", "Counts") || {};
  const branchState = value(data, "branch_state", "BranchState");
  const running = value(data, "running_rebase", "RunningRebase");
  const tasks = value(data, "tasks", "Tasks") || [];
  const rebases = value(data, "rebases", "Rebases") || [];
  const children = value(data, "children", "Children") || [];
  const blockers = value(data, "blockers", "Blockers") || [];

  const id = value(feature, "id", "ID");
  const title = value(feature, "title", "Title") || id;
  const childNavigation = { context: id, returnTo: data.currentHref || featureHref(projectID, id, data.navigation) };
  const body = value(feature, "body", "Body");
  const status = value(feature, "status", "Status") || "open";
  const branch = value(feature, "branch", "Branch");
  const open = status === "open";
  const landedAt = value(feature, "landed_at", "LandedAt");
  const landSHA = value(feature, "land_sha", "LandSHA");
  const integrationID = value(feature, "integration_feature_id", "IntegrationFeatureID") || "";
  const targetLabel = integrationID || "project base";
  const divergence = featureDivergenceLabel(branchState);
  const behind = Number(branchState?.behind || 0);

  return `
    <section class="detail">
      <div class="head">
        <div class="title-row">
          <h2>${escapeHTML(title)}</h2>
          <span class="feature-status" data-status="${escapeAttr(status)}">${escapeHTML(status)}</span>
          <span class="spacer"></span>
          <span class="feature-id">${escapeHTML(id)}</span>
        </div>
        <div class="legend">
          <span class="legend-item"><span class="mono">${escapeHTML(branch)}</span></span>
          <span class="legend-item">branches from / lands into ${escapeHTML(targetLabel)}</span>
          <span class="legend-item">${escapeHTML(featureCountsLabel(counts))}</span>
          ${divergence ? `<span class="legend-item" ${behind ? "data-behind" : ""}>${escapeHTML(divergence)}</span>` : ""}
          ${landedAt ? `<span class="legend-item">landed ${escapeHTML(formatDate(landedAt))}${landSHA ? ` at ${escapeHTML(shortSHA(landSHA))}` : ""}</span>` : ""}
        </div>
        ${body ? `<p class="feature-body">${escapeHTML(body)}</p>` : ""}
        ${running ? `
          <p class="rebase-banner" role="status">
            Rebase in progress${value(running, "task_id", "TaskID") ? ` — <a href="${escapeAttr(projectTaskHref(projectID, value(running, "task_id", "TaskID"), childNavigation))}" data-link>${escapeHTML(value(running, "task_id", "TaskID"))}</a>` : ""}.
            The feature's other tasks are blocked until it finishes.
          </p>` : ""}
        ${open ? `
          <div class="actions">
            <button type="button" data-feature-start="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Start descendant tasks</button>
            <button type="button" class="secondary" data-feature-rebase="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Rebase onto ${escapeHTML(targetLabel)}</button>
            <button type="button" data-feature-land="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Land into ${escapeHTML(targetLabel)}</button>
            <button type="button" class="secondary danger" data-feature-archive="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Archive</button>
          </div>` : ""}
      </div>
      ${open ? `
        <form class="feature-edit" data-feature-form="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">
          <input name="title" value="${escapeAttr(title)}" required aria-label="Feature title">
          <textarea name="body" rows="2" placeholder="What this feature is (optional)" aria-label="Feature body">${escapeHTML(body)}</textarea>
          <div>
            <button type="submit" class="secondary">Save</button>
          </div>
        </form>` : ""}
      <div class="members">
        <h3>Used by</h3>
        ${tasks.length ? tasks.map((task) => taskRow(task, projectID, childNavigation)).join("") : `<p class="empty">No tasks use this feature yet.</p>`}
      </div>
      ${data.hierarchy ? renderWorkItemContext({ projectID, item: value(data.hierarchy, "item", "Item") || value(data, "item", "Item"), items: data.workItems || [], ancestors: value(data.hierarchy, "ancestors", "Ancestors"), relations: value(data.hierarchy, "relations", "Relations") || [], blockers: value(data.hierarchy, "blockers", "Blockers") || [], rollup: value(data.hierarchy, "rollup", "Rollup"), attentionCount: Number(value(data.hierarchy, "attention_count", "AttentionCount") || 0), navigation: data.navigation, currentHref: data.currentHref }) : ""}
      ${!data.hierarchy && children.length ? `<div class="members"><h3>Child containers</h3>${children.map((child) => {
        const childID = value(child, "id", "ID");
        const kind = value(child, "kind", "Kind");
        const href = kind === "feature" ? featureHref(projectID, childID, childNavigation) : kind === "epic" ? epicHref(projectID, childID, childNavigation) : projectTaskHref(projectID, childID, childNavigation);
        return `<a class="member" href="${escapeAttr(href)}" data-link><span class="member-id">${escapeHTML(childID)}</span><span class="member-title">${escapeHTML(value(child, "title", "Title"))}</span><span class="member-note">${escapeHTML(kind)}</span></a>`;
      }).join("")}</div>` : ""}
      ${blockers.some((blocker) => !value(blocker, "resolved", "Resolved")) ? `<div class="members"><h3>Blocked by</h3>${blockers.filter((blocker) => !value(blocker, "resolved", "Resolved")).map((blocker) => {
        const item = value(blocker, "item", "Item") || {};
        return `<div class="member"><span class="member-id">${escapeHTML(value(item, "id", "ID"))}</span><span class="member-title">${escapeHTML(value(item, "title", "Title"))}</span></div>`;
      }).join("")}</div>` : ""}
      ${rebases.length ? `
        <div class="members">
          <h3>Rebases</h3>
          ${rebases.map((rebase) => rebaseRow(rebase, projectID, childNavigation)).join("")}
        </div>` : ""}
    </section>`;
}

class FlowFeature extends FlowElement {
  render() {
    return renderFeature(this.data || {});
  }
}

customElements.define("flow-feature", FlowFeature);

// featureHref is re-exported so feature-route can build list links without a
// second api.js import chain.
export { featureHref };
