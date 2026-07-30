// Board/kanban rendering helpers: lane bucketing, phase/review/state badges,
// card labels, and done-page flattening.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export function doneClosedAtMs(task) {
  const ms = Date.parse(value(task, "done_at", "DoneAt"));
  return Number.isNaN(ms) ? 0 : ms;
}

// flattenDonePage turns an aggregate /v2/done page into render-ready entries
// (newest closed first) plus each project's keyset cursor.
export function flattenDonePage(data, projectBadge) {
  const entries = [];
  const cursors = {};
  const projects = value(data, "done", "Done") || [];
  for (const entry of projects) {
    const projectID = value(entry, "project_id", "ProjectID") || "";
    const projectName = value(entry, "project_name", "ProjectName") || "";
    const tasks = value(entry, "tasks", "Tasks") || [];
    const outcomes = value(entry, "outcomes", "Outcomes") || {};
    const cards = value(entry, "task_cards", "TaskCards") || {};
    const project = { id: projectID, name: projectName, badge: projectBadge };
    for (const task of tasks) {
      const taskID = value(task, "id", "ID");
      entries.push({ task, card: cards[taskID] || {}, laneState: outcomes[taskID] || "", project });
    }
    const nextBefore = value(entry, "next_before", "NextBefore");
    if (nextBefore) cursors[projectID] = { before: nextBefore, beforeID: value(entry, "next_before_id", "NextBeforeID") || "" };
  }
  entries.sort((a, b) => doneClosedAtMs(b.task) - doneClosedAtMs(a.task));
  return { entries, cursors };
}

export function laneTasks(board, key, field) {
  return board[field] || board[key] || [];
}

// phaseKey maps lifecycle, schedule, and lane states onto the design system's
// phase color slugs (the [data-phase] attribute values in app.module.css).
export function phaseKey(state) {
  switch (String(state || "")) {
    case "unscheduled":
      return "backlog";
    case "scheduled":
      return "up_next";
    case "working":
      return "authoring";
    case "done":
    case "completed":
    case "cancelled":
    case "failed":
      return "dead";
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
    case "phase_approval":
      return "waiting for phase approval";
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

const WORKFLOW_ACTION_VERBS = new Set([
  "analyze", "analyse", "approve", "assess", "audit", "benchmark", "build",
  "check", "configure", "create", "deploy", "design", "document", "draft",
  "execute", "fetch", "finalise", "finalize", "fix", "generate", "implement",
  "inspect", "integrate", "investigate", "materialize", "merge", "migrate",
  "monitor", "optimize", "optimise", "package", "plan", "prepare", "process",
  "publish", "queue", "refactor", "release", "research", "resolve", "review",
  "run", "schedule", "specify", "sync", "synchronize", "test", "triage",
  "update", "validate", "verify", "write",
]);

const DOUBLE_FINAL_CONSONANT_VERBS = new Set([
  "begin", "commit", "cut", "get", "plan", "put", "run", "set", "stop", "submit",
]);

function activityObject(label) {
  if (/^[A-Z][a-z]/.test(label)) return label[0].toLowerCase() + label.slice(1);
  return label;
}

function progressiveVerb(word) {
  const lower = word.toLowerCase();
  let progressive;
  if (lower.endsWith("ing")) {
    progressive = lower;
  } else if (lower.endsWith("ie")) {
    progressive = `${lower.slice(0, -2)}ying`;
  } else if (lower === "sync") {
    progressive = "syncing";
  } else if (lower.endsWith("c")) {
    progressive = `${lower}king`;
  } else if (lower.endsWith("e") && !/(?:ee|oe|ye)$/.test(lower)) {
    progressive = `${lower.slice(0, -1)}ing`;
  } else if (DOUBLE_FINAL_CONSONANT_VERBS.has(lower)) {
    progressive = `${lower}${lower.at(-1)}ing`;
  } else {
    progressive = `${lower}ing`;
  }
  return /^[A-Z]/.test(word) ? progressive[0].toUpperCase() + progressive.slice(1) : progressive;
}

// workflowActivityLabel turns an active workflow step into a concise description
// of the work in progress. Node kinds disambiguate noun-style names used by the
// built-in flow (for example, "Code review" and "Automated checks").
export function workflowActivityLabel(name, kind = "") {
  const label = String(name || "").trim().replace(/\s+/g, " ");
  if (!label) return "";

  const normalizedKind = String(kind || "").trim().toLowerCase();
  const first = label.match(/^([A-Za-z]+)(.*)$/);
  const firstWord = first ? first[1].toLowerCase() : "";
  if (normalizedKind === "automated_checks" && !["check", "execute", "run", "test", "verify"].includes(firstWord)) {
    return `Running ${activityObject(label)}`;
  }
  if (normalizedKind === "change_review" && firstWord !== "review") {
    const reviewObject = label.match(/^(.+?)\s+review$/i);
    return `Reviewing ${activityObject(reviewObject ? reviewObject[1] : label)}`;
  }
  if (normalizedKind === "verify_change" && firstWord !== "verify") {
    const verificationObject = label.match(/^(.+?)\s+verification$/i);
    return `Verifying ${activityObject(verificationObject ? verificationObject[1] : label)}`;
  }
  if (normalizedKind === "merge_change" && firstWord !== "merge") {
    const mergeObject = label.match(/^(.+?)\s+merge$/i);
    return `Merging ${activityObject(mergeObject ? mergeObject[1] : label)}`;
  }
  if (normalizedKind === "finalize_rebase" && !["finalize", "publish"].includes(firstWord)) {
    return `Finalizing ${activityObject(label)}`;
  }
  if (normalizedKind === "human_gate") {
    return `Waiting for ${activityObject(label)}`;
  }
  if (!first || !WORKFLOW_ACTION_VERBS.has(firstWord)) {
    return `Working on ${activityObject(label)}`;
  }
  return `${progressiveVerb(first[1])}${first[2]}`;
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
