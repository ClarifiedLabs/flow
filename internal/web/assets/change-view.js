// Change (branch diff) detail view. Caches the rendered diff per head SHA on
// the app instance (app.changeDiffCache).

import { apiGet, issueHref } from "./api.js";
import { renderReviewBadge } from "./board.js";
import { canApproveHumanReview, renderCheck, renderDiffSummary, renderHumanReviewApproveButton, renderThread } from "./diff.js";
import { formatDate, shortSHA } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { readDiffMode } from "./storage.js";

export async function renderChangeView(app, id, context) {
  const data = await apiGet(`/v1/changes/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Change");
  const change = data.change || data.Change || {};
  const issue = data.issue || data.Issue || {};
  const checks = data.checks || data.Checks || [];
  const threads = data.threads || data.Threads || [];
  const requiredChecks = data.required_checks || data.RequiredChecks || {};
  const reviewState = value(data, "review_state", "ReviewState");
  const canMerge = Boolean(value(data, "can_merge", "CanMerge"));
  const mergeBlockedReason = value(data, "merge_blocked_reason", "MergeBlockedReason");
  const changeID = value(change, "id", "ID") || id;
  const changeProjectID = data.project_id || data.ProjectID || "";
  const issueID = value(issue, "id", "ID");
  const headSHA = value(change, "head_sha", "HeadSHA");
  const checkTotal = Number(value(requiredChecks, "total", "Total") || 0);
  const checkSatisfied = Number(value(requiredChecks, "satisfied", "Satisfied") || 0);
  const humanReviewCheck = checks.find((check) => canApproveHumanReview(check, issueID));
  const humanReviewAction = humanReviewCheck ? renderHumanReviewApproveButton(humanReviewCheck, issueID, changeProjectID) : "";
  app.querySelector(".content").innerHTML = `
    <section class="detail change-detail">
      <div class="detail-head">
        <div>
          <h2>${escapeHTML(changeID)} · ${escapeHTML(value(issue, "title", "Title") || "Change")}</h2>
          <div class="meta">
            ${reviewState ? renderReviewBadge(reviewState) : ""}
            ${checkTotal ? `<span class="badge ${checkSatisfied === checkTotal ? "ok" : "idle"}">required ${checkSatisfied}/${checkTotal}</span>` : ""}
            ${value(change, "merged_at", "MergedAt") ? `<span class="badge" data-phase="merged"><span class="dot"></span>merged</span>` : ""}
          </div>
          <p class="meta-quiet">${[
            issueID ? `<a href="${escapeAttr(issueHref(changeProjectID, issueID))}" data-link>${escapeHTML(issueID)}</a>` : "",
            value(change, "ready_at", "ReadyAt") ? `ready ${escapeHTML(formatDate(value(change, "ready_at", "ReadyAt")))}` : "",
            value(change, "merged_at", "MergedAt") ? `merged ${escapeHTML(formatDate(value(change, "merged_at", "MergedAt")))}` : "",
          ].filter(Boolean).join(" · ")}</p>
        </div>
        <div class="actions">
          ${canMerge ? `<button class="button" data-merge-change="${escapeAttr(changeID)}">Merge</button>` : ""}
          ${humanReviewAction}
        </div>
      </div>
      <div class="summary-grid">
        <div><span>Branch</span><strong>${escapeHTML(value(change, "branch", "Branch"))}</strong></div>
        <div><span>Base</span><strong>${escapeHTML(value(change, "base", "Base"))}</strong></div>
        <div><span>Head</span><strong>${escapeHTML(shortSHA(value(change, "head_sha", "HeadSHA")))}</strong></div>
        <div><span>Updated</span><strong>${escapeHTML(formatDate(value(change, "updated_at", "UpdatedAt")))}</strong></div>
      </div>
      ${mergeBlockedReason ? `<p class="card-status">${escapeHTML(mergeBlockedReason)}</p>` : ""}
      <h3>Diff</h3>
      <div data-change-diff="${escapeAttr(changeID)}">${headSHA ? `<div class="empty">Loading diff</div>` : `<div class="empty">Diff unavailable</div>`}</div>
      <h3>Checks</h3>
      ${checks.length ? `<div class="check-list">${checks.map((check) => renderCheck(check, issueID, changeProjectID)).join("")}</div>` : `<div class="empty">No checks</div>`}
      <h3>Review Threads</h3>
      ${threads.length ? `<div class="feed">${threads.map((thread) => renderThread(thread, headSHA)).join("")}</div>` : `<div class="empty">No review threads</div>`}
    </section>
  `;
  app.bindIssueActions(() => renderChangeView(app, id));
  if (headSHA && await renderChangeDiffView(app, changeID, headSHA, context) === false) return false;
  return true;
}

export async function renderChangeDiffView(app, changeID, headSHA, context) {
  const container = app.querySelector(`[data-change-diff="${escapeAttr(changeID)}"]`);
  if (!container) return true;
  const cacheKey = `${changeID}:${headSHA}`;
  app.changeDiffCache = app.changeDiffCache || new Map();
  let diff = app.changeDiffCache.get(cacheKey);
  if (!diff) {
    diff = await apiGet(`/v1/changes/${encodeURIComponent(changeID)}/diff`);
    if (context && !app.isActiveLoad(context)) return false;
    if (value(diff, "head_sha", "HeadSHA") && value(diff, "head_sha", "HeadSHA") !== headSHA) {
      container.innerHTML = `<div class="empty">Diff changed; waiting for refresh</div>`;
      return true;
    }
    app.changeDiffCache.set(cacheKey, diff);
  }
  if (context && !app.isActiveLoad(context)) return false;
  const mode = readDiffMode();
  container.innerHTML = renderDiffSummary(diff, mode);
  if (typeof container.setAttribute === "function") container.setAttribute("data-diff-cache-key", cacheKey);
  container.querySelectorAll?.("[data-diff-mode-toggle] button").forEach((button) => {
    button.addEventListener("click", () => app.toggleDiffMode(button));
  });
  return true;
}
