// The features list: one row per feature with its task counts, branch
// divergence and status, plus the create form. A feature groups tasks behind
// one long-lived branch in the project's exchange; open features sort first,
// then landed, then archived (dimmed).

import { FlowElement } from "./base.js";
import { featureHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { formatRelative } from "../format.js";
import { renderWorkNav } from "../work-nav.js";
import { handleRelationsPickerClick, relationsPickerView } from "../create-relations.js";

const STATUS_RANK = { open: 0, landed: 1, archived: 2 };

// featureCountsLabel compresses the four lifecycle counts into one short
// legend, omitting zero groups: "1 open · 2 working · 3 done".
export function featureCountsLabel(counts) {
  const parts = [];
  const open = Number(counts.open || 0);
  const scheduled = Number(counts.scheduled || 0);
  const working = Number(counts.in_progress || counts.inProgress || 0);
  const done = Number(counts.done || 0);
  if (open) parts.push(`${open} open`);
  if (scheduled) parts.push(`${scheduled} scheduled`);
  if (working) parts.push(`${working} working`);
  if (done) parts.push(`${done} done`);
  return parts.length ? parts.join(" · ") : "no tasks";
}

// featureDivergenceLabel is the ahead/behind summary against the base branch.
export function featureDivergenceLabel(state) {
  if (!state) return "";
  const ahead = Number(state.ahead || 0);
  const behind = Number(state.behind || 0);
  if (!ahead && !behind) return "up to date";
  const parts = [];
  if (ahead) parts.push(`${ahead} ahead`);
  if (behind) parts.push(`${behind} behind`);
  return parts.join(" · ");
}

function featureRow(entry, projectID) {
  const feature = value(entry, "feature", "Feature") || {};
  const counts = value(entry, "counts", "Counts") || {};
  const branchState = value(entry, "branch_state", "BranchState");
  const running = value(entry, "running_rebase", "RunningRebase");
  const id = value(feature, "id", "ID");
  const title = value(feature, "title", "Title") || id;
  const status = value(feature, "status", "Status") || "open";
  const divergence = featureDivergenceLabel(branchState);
  const behind = Number(branchState?.behind || 0);

  return `
    <a class="feature-row" href="${escapeAttr(featureHref(projectID, id))}" ${status !== "open" ? `data-${status}` : ""}>
      <span class="feature-title">${escapeHTML(title)}</span>
      <span class="feature-id">${escapeHTML(id)}</span>
      <span class="feature-status" data-status="${escapeAttr(status)}">${escapeHTML(status)}</span>
      <span class="feature-counts">${escapeHTML(featureCountsLabel(counts))}</span>
      <span class="feature-divergence" ${behind ? "data-behind" : ""}>${escapeHTML(divergence)}</span>
      ${running ? `<span class="feature-rebasing">rebasing</span>` : ""}
    </a>`;
}

export function renderFeatures(data) {
  const projectID = String(data.projectID || "");
  const entries = (value(data, "features", "Features") || []).slice();
  entries.sort((a, b) => {
    const fa = value(a, "feature", "Feature") || {};
    const fb = value(b, "feature", "Feature") || {};
    const rank = (STATUS_RANK[value(fa, "status", "Status")] ?? 0) - (STATUS_RANK[value(fb, "status", "Status")] ?? 0);
    if (rank) return rank;
    return String(value(fa, "title", "Title")).localeCompare(String(value(fb, "title", "Title")));
  });

  const latest = entries
    .map((entry) => value(value(entry, "feature", "Feature") || {}, "updated_at", "UpdatedAt"))
    .filter(Boolean)
    .sort()
    .pop();
  return `
    ${renderWorkNav({ active: "branches", projects: data.projects || [], projectID, search: data.search || "" })}
    <section class="detail">
      <div class="head">
        <div class="title-row">
          <h2>Branches</h2>
          <span class="spacer"></span>
          ${latest ? `<span class="count">updated ${escapeHTML(formatRelative(latest))}</span>` : ""}
        </div>
      </div>
      <form class="feature-create" data-feature-form="" data-project="${escapeAttr(projectID)}">
        <input name="title" placeholder="Feature title" required aria-label="Feature title">
        <textarea name="body" rows="2" placeholder="What this feature is (optional)" aria-label="Feature body"></textarea>
        <input name="parent_item_id" placeholder="Parent epic or feature ID (optional)" aria-label="Parent work item ID">
        ${relationsPickerView(data.workItems || [])}
        <div>
          <button type="submit">Create feature</button>
        </div>
      </form>
      <div class="members">
        ${entries.length
          ? entries.map((entry) => featureRow(entry, projectID)).join("")
          : `<p class="empty">No features yet. A feature groups tasks behind one branch you rebase and land together.</p>`}
      </div>
    </section>`;
}

class FlowFeatures extends FlowElement {
  render() {
    return renderFeatures(this.data || {});
  }

  handleClick(event) {
    handleRelationsPickerClick(this, event);
  }
}

customElements.define("flow-features", FlowFeatures);
