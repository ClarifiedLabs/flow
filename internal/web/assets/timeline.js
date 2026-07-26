// Unified lifecycle timeline + session/change/relation rendering.

import { renderStatusKindBadge } from "./attention.js";
import { taskHref } from "./api.js";
import { cardLabelKey, renderPhaseBadge } from "./board.js";
import { formatDate, formatRelative, shortSHA } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { renderTerminalButton, renderTranscriptButton } from "./terminal.js";

export function renderTag(tag) {
  return escapeHTML(value(tag, "slug", "Slug") || value(tag, "name", "Name"));
}

export function uniqueCardTags(tags, seen) {
  const unique = [];
  for (const tag of tags || []) {
    const label = value(tag, "slug", "Slug") || value(tag, "name", "Name");
    const key = cardLabelKey(label);
    if (!key || seen.has(key)) continue;
    seen.add(key);
    unique.push(tag);
  }
  return unique;
}

export function renderRelationSummary(relations) {
  const parts = [];
  const parents = Number(value(relations, "parents", "Parents") || 0);
  const children = Number(value(relations, "children", "Children") || 0);
  const blocks = Number(value(relations, "blocks", "Blocks") || 0);
  const blockedBy = Number(value(relations, "blocked_by", "BlockedBy") || 0);
  const related = Number(value(relations, "related", "Related") || 0);
  if (parents) parts.push(["parent", parents]);
  if (children) parts.push(["child", children]);
  if (blocks) parts.push(["blocks", blocks]);
  if (blockedBy) parts.push(["blocked by", blockedBy]);
  if (related) parts.push(["related", related]);
  return parts.map(([label, count]) => `${escapeHTML(label)} ${count}`).join(" · ");
}


export function renderHandoffSummary(handoff) {
  if (!handoff) return "";
  const present = Boolean(value(handoff, "present", "Present"));
  const valid = Boolean(value(handoff, "valid", "Valid"));
  const summary = value(handoff, "summary", "Summary");
  const label = !present ? "handoff missing" : !valid ? "handoff invalid" : "handoff";
  return `<p class="card-status">${escapeHTML(label)}${summary ? `: ${renderMarkdown(summary, { inline: true })}` : ""}</p>`;
}

export function renderRelation(relation, taskID, projectID = "") {
  const source = value(relation, "source_task_id", "SourceTaskID");
  const target = value(relation, "target_task_id", "TargetTaskID");
  const related = source === taskID ? target : source;
  const direction = source === taskID ? "outbound" : "inbound";
  return `
    <article class="feed-item">
      <strong>${escapeHTML(value(relation, "kind", "Kind"))}</strong><span>${escapeHTML(direction)}</span>
      <p><a href="${escapeAttr(taskHref(projectID, related))}" data-link>${escapeHTML(related)}</a></p>
    </article>
  `;
}

export function renderTaskChange(change) {
  const changeID = value(change, "id", "ID");
  const readyAt = value(change, "ready_at", "ReadyAt");
  const mergedAt = value(change, "merged_at", "MergedAt");
  return `
    <article class="feed-item">
      <strong><a href="/ui/changes/${escapeAttr(changeID)}" data-link>${escapeHTML(changeID)}</a></strong>
      <span>${escapeHTML(value(change, "branch", "Branch"))}</span>
      <p>${escapeHTML(shortSHA(value(change, "head_sha", "HeadSHA")) || "no head")}${readyAt ? ` · ready ${escapeHTML(formatDate(readyAt))}` : ""}${mergedAt ? ` · merged ${escapeHTML(formatDate(mergedAt))}` : ""}</p>
    </article>
  `;
}


// TIMELINE_CAP is how many entries render before the "Show more" control. The
// full feed is always available behind it so history is never hidden.
export const TIMELINE_CAP = 20;

// buildTimelineEntries merges sessions, (enriched) transitions and status-log
// entries into one list sorted newest-first by timestamp. Each entry carries a
// `type` the renderer switches on plus the original record.
export function buildTimelineEntries(sessions, transitions, statusLog) {
  const entries = [];
  for (const session of sessions || []) {
    entries.push({ type: "session", time: value(session, "updated_at", "UpdatedAt") || value(session, "last_agent_activity_at", "LastAgentActivityAt"), record: session });
  }
  // Prefer the enriched timeline_transitions (which carry session_id/state/
  // head_sha) but fall back to the raw transitions feed when the backend has
  // not provided it, so the timeline degrades gracefully.
  const transitionRows = (transitions || []);
  for (const entry of transitionRows) {
    entries.push({ type: "transition", time: value(entry, "created_at", "CreatedAt"), record: entry });
  }
  for (const status of statusLog || []) {
    entries.push({ type: "status", time: value(status, "created_at", "CreatedAt"), record: status });
  }
  entries.sort((a, b) => {
    const ta = a.time ? Date.parse(a.time) : 0;
    const tb = b.time ? Date.parse(b.time) : 0;
    if (Number.isNaN(ta) && Number.isNaN(tb)) return 0;
    if (Number.isNaN(ta)) return 1;
    if (Number.isNaN(tb)) return -1;
    return tb - ta;
  });
  return groupSessionStateRuns(entries);
}

// groupSessionStateRuns collapses runs of consecutive (by time) session_state_changed
// transition rows for the same session into a single collapsible "run" entry, so a
// chatty watchdog does not flood the timeline. A single state change stays a
// plain row; only 2+ consecutive same-session changes collapse. The newest row
// of a run leads the summary so the timeline still reads newest-first.
export function groupSessionStateRuns(entries) {
  const out = [];
  let i = 0;
  while (i < entries.length) {
    const entry = entries[i];
    const isStateChanged = entry.type === "transition"
      && value(entry.record, "event_kind", "EventKind") === "session_state_changed";
    if (!isStateChanged) {
      out.push(entry);
      i += 1;
      continue;
    }
    const sessionID = value(entry.record, "session_id", "SessionID");
    const run = [entry];
    let j = i + 1;
    while (j < entries.length
      && entries[j].type === "transition"
      && value(entries[j].record, "event_kind", "EventKind") === "session_state_changed"
      && value(entries[j].record, "session_id", "SessionID") === sessionID) {
      run.push(entries[j]);
      j += 1;
    }
    if (run.length > 1 && sessionID) {
      out.push({ type: "session-run", time: entry.time, sessionID, entries: run });
    } else {
      out.push(...run);
    }
    i = j;
  }
  return out;
}

