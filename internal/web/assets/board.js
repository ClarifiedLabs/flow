// Board/kanban rendering helpers: lane bucketing, phase/review/state badges,
// card labels, and done-page flattening.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export function doneClosedAtMs(issue) {
  const ms = Date.parse(value(issue, "closed_at", "ClosedAt"));
  return Number.isNaN(ms) ? 0 : ms;
}

// flattenDonePage turns an aggregate /v1/done page into render-ready entries
// (newest closed first) plus each project's keyset cursor.
export function flattenDonePage(data, projectBadge) {
  const entries = [];
  const cursors = {};
  const projects = value(data, "done", "Done") || [];
  for (const entry of projects) {
    const projectID = value(entry, "project_id", "ProjectID") || "";
    const projectName = value(entry, "project_name", "ProjectName") || "";
    const issues = value(entry, "issues", "Issues") || [];
    const outcomes = value(entry, "outcomes", "Outcomes") || {};
    const cards = value(entry, "issue_cards", "IssueCards") || {};
    const project = { id: projectID, name: projectName, badge: projectBadge };
    for (const issue of issues) {
      const issueID = value(issue, "id", "ID");
      entries.push({ issue, card: cards[issueID] || {}, laneState: outcomes[issueID] || "", project });
    }
    const nextBefore = value(entry, "next_before", "NextBefore");
    if (nextBefore) cursors[projectID] = { before: nextBefore, beforeID: value(entry, "next_before_id", "NextBeforeID") || "" };
  }
  entries.sort((a, b) => doneClosedAtMs(b.issue) - doneClosedAtMs(a.issue));
  return { entries, cursors };
}

export function laneIssues(board, key, field) {
  return board[field] || board[key] || [];
}

// phaseKey maps lifecycle, schedule, and lane states onto the design system's
// phase color slugs (the [data-phase] attribute values in app.module.css).
export function phaseKey(state) {
  switch (String(state || "")) {
    case "triage":
      return "triage";
    case "backlog":
      return "backlog";
    case "up_next":
      return "up_next";
    case "planning":
      return "planning";
    case "authoring":
    case "in_progress":
      return "authoring";
    case "needs_attention":
    case "changes_requested":
      return "blocked";
    case "critique":
    case "in_review":
    case "acceptance":
      return "critique";
    case "approved":
    case "ready_to_merge":
      return "approved";
    case "merged_closed":
    case "merged":
      return "merged";
    case "rejected_closed":
    case "abandoned":
    case "closed":
      return "dead";
    case "blocked":
      return "blocked";
    default:
      return "";
  }
}

export function waitReasonLabel(reason) {
  switch (String(reason || "")) {
    case "plan_approval":
      return "waiting for plan approval";
    case "manual_merge":
      return "waiting for merge";
    case "question":
      return "waiting for response";
    case "human_review":
      return "waiting for human review";
    case "blocked":
      return "blocked";
    default:
      return String(reason || "").replaceAll("_", " ");
  }
}

export function renderPhaseBadge(state) {
  const label = String(state || "").replaceAll("_", " ");
  if (!label || label === "—") {
    return `<span class="badge idle">—</span>`;
  }
  const slug = phaseKey(state);
  if (!slug) {
    return `<span class="badge idle">${escapeHTML(label)}</span>`;
  }
  return `<span class="badge" data-phase="${escapeAttr(slug)}"><span class="dot"></span>${escapeHTML(label)}</span>`;
}

export function renderReviewBadge(reviewState) {
  const cls = reviewState === "approved" ? "ok" : reviewState === "changes_requested" ? "warn" : "idle";
  return `<span class="badge ${cls}">${escapeHTML(String(reviewState).replaceAll("_", " "))}</span>`;
}

export function cardLabelKey(label) {
  return String(label || "")
    .trim()
    .replace(/^(?:[-*\u2022]\s+)+/, "")
    .toLowerCase()
    .replace(/[_-]+/g, " ")
    .replace(/\s+/g, " ");
}

export function renderUniqueCardLabel(seen, label, render) {
  const key = cardLabelKey(label);
  if (!key || seen.has(key)) return "";
  seen.add(key);
  return render();
}

export function renderStateBadge(state) {
  const raw = String(state || "");
  if (!raw) return "";
  const normalized = raw.toLowerCase();
  const cls = ["ready", "online", "ok", "completed", "succeeded", "done", "finished", "satisfied", "healthy"].includes(normalized)
    ? "ok"
    : ["failed", "error", "dead", "lost", "expired", "crashed"].includes(normalized)
      ? "danger"
      : ["canceled", "cancelled"].includes(normalized)
        ? "warn"
        : ["running", "starting", "active", "working", "leased", "live"].includes(normalized)
          ? "run"
          : "idle";
  return `<span class="badge ${cls}">${escapeHTML(raw.replaceAll("_", " "))}</span>`;
}
