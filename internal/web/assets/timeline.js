// Unified lifecycle timeline + session/change/relation rendering.

import { renderStatusKindBadge } from "./attention.js";
import { issueHref } from "./api.js";
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

export function renderDiffStatText(stats, unavailableReason, headSHA) {
  const parts = [];
  if (headSHA) parts.push(`head ${escapeHTML(shortSHA(headSHA))}`);
  if (stats) {
    parts.push(`files ${Number(value(stats, "total_files", "TotalFiles") || 0)}`);
    parts.push(`<span class="diff-add">+${Number(value(stats, "additions", "Additions") || 0)}</span>`);
    parts.push(`<span class="diff-del">-${Number(value(stats, "deletions", "Deletions") || 0)}</span>`);
  } else if (unavailableReason) {
    parts.push(`<span title="${escapeAttr(unavailableReason)}">diff unavailable</span>`);
  }
  return parts.join(" · ");
}

export function renderHandoffSummary(handoff) {
  if (!handoff) return "";
  const present = Boolean(value(handoff, "present", "Present"));
  const valid = Boolean(value(handoff, "valid", "Valid"));
  const summary = value(handoff, "summary", "Summary");
  const label = !present ? "handoff missing" : !valid ? "handoff invalid" : "handoff";
  return `<p class="card-status">${escapeHTML(label)}${summary ? `: ${renderMarkdown(summary, { inline: true })}` : ""}</p>`;
}

export function renderRelation(relation, issueID, projectID = "") {
  const source = value(relation, "source_issue_id", "SourceIssueID");
  const target = value(relation, "target_issue_id", "TargetIssueID");
  const related = source === issueID ? target : source;
  const direction = source === issueID ? "outbound" : "inbound";
  return `
    <article class="feed-item">
      <strong>${escapeHTML(value(relation, "kind", "Kind"))}</strong><span>${escapeHTML(direction)}</span>
      <p><a href="${escapeAttr(issueHref(projectID, related))}" data-link>${escapeHTML(related)}</a></p>
    </article>
  `;
}

export function renderIssueChange(change) {
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

export function renderIssueSession(session) {
  // Compact session row for the unified timeline. The raw session id is no
  // longer the headline (it is largely meaningless and the issue id is already
  // on the page): the row leads with the state, then worker/branch, with the
  // transcript/terminal controls only when the backend reports them ready.
  const sessionID = value(session, "id", "ID");
  const terminalAvailable = Boolean(value(session, "terminal_available", "TerminalAvailable"));
  const transcriptAvailable = Boolean(value(session, "transcript_available", "TranscriptAvailable"));
  const buttons = [];
  if (sessionID && terminalAvailable) buttons.push(renderTerminalButton("session", sessionID, { iconOnly: true }));
  if (sessionID && transcriptAvailable) buttons.push(renderTranscriptButton("session", sessionID, { iconOnly: true }));
  const actions = buttons.length ? `<div class="actions timeline-row-actions">${buttons.join("")}</div>` : "";
  const state = value(session, "state", "State");
  const updatedAt = value(session, "updated_at", "UpdatedAt");
  const workerID = value(session, "worker_id", "WorkerID");
  const branch = value(session, "branch", "Branch");
  const meta = [workerID, branch].filter(Boolean).map(escapeHTML).join(" · ");
  const lastActivity = value(session, "last_agent_activity_at", "LastAgentActivityAt");
  const activity = lastActivity ? `<span class="muted">active ${escapeHTML(formatRelative(lastActivity))}</span>` : "";
  return timelineRow({
    glyph: "session",
    time: updatedAt,
    title: `<span class="badge idle session-state">${escapeHTML(state || "session")}</span>`,
    meta,
    extra: activity,
    actions,
  });
}

// timelineRow renders one merged timeline feed-item with a leading glyph, a
// relative timestamp (absolute on hover via title) and optional actions.
export function timelineRow({ glyph, time, title, meta, extra, actions }) {
  const timestamp = time ? `<time title="${escapeAttr(formatDate(time))}">${escapeHTML(formatRelative(time))}</time>` : "";
  const metaHTML = meta ? `<span class="muted">${meta}</span>` : "";
  const extraHTML = extra || "";
  const glyphHTML = glyph ? `<span class="timeline-glyph timeline-glyph-${escapeAttr(glyph)}" aria-hidden="true"></span>` : "";
  return `
    <article class="feed-item timeline-row" data-timeline-glyph="${escapeAttr(glyph || "")}">
      ${glyphHTML}
      <div class="timeline-row-body">
        <div class="timeline-row-head">
          <strong>${title || ""}</strong>
          ${timestamp}
        </div>
        ${metaHTML || extraHTML ? `<p>${metaHTML}${extraHTML ? (metaHTML ? " · " : "") + extraHTML : ""}</p>` : ""}
        ${actions || ""}
      </div>
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

// renderTimelineRow renders a single merged timeline entry by type. Session
// transitions (session_ready / session_state_changed) get a session glyph and,
// when the backend enriched them with a session_id, terminal/transcript
// controls for that exact session.
export function renderTimelineRow(entry) {
  if (entry.type === "session") return renderIssueSession(entry.record);
  if (entry.type === "status") return renderTimelineStatusRow(entry.record);
  if (entry.type === "session-run") return renderSessionStateRun(entry);
  return renderTimelineTransitionRow(entry.record);
}

// renderSessionStateRun renders a collapsed run of consecutive
// session_state_changed rows for one session. The summary leads with the count
// and the session's latest state; the individual rows are hidden behind a
// toggle until expanded.
export function renderSessionStateRun(run) {
  const rows = (run.entries || []).map((child) => renderTimelineTransitionRow(child.record)).join("");
  const newest = run.entries && run.entries[0] ? run.entries[0] : null;
  const sessionState = newest ? value(newest.record, "session_state", "SessionState") : "";
  const sessionID = run.sessionID || "";
  const count = (run.entries || []).length;
  const time = newest ? newest.time : run.time;
  return `
    <article class="feed-item timeline-row timeline-run" data-timeline-glyph="session">
      <span class="timeline-glyph timeline-glyph-session" aria-hidden="true"></span>
      <div class="timeline-row-body">
        <div class="timeline-row-head">
          <strong>
            <button class="button-link timeline-run-toggle" type="button" data-timeline-run-toggle aria-expanded="false">${escapeHTML(`${count} session state changes`)}</button>
            ${sessionState ? `<span class="muted">→ ${escapeHTML(sessionState)}</span>` : ""}
          </strong>
          ${time ? `<time title="${escapeAttr(formatDate(time))}">${escapeHTML(formatRelative(time))}</time>` : ""}
        </div>
        <div class="timeline-run-rows" hidden>${rows}</div>
      </div>
    </article>
  `;
}

export function renderTimelineStatusRow(status) {
  const message = value(status, "message", "Message");
  const actor = value(status, "actor", "Actor");
  const kind = value(status, "kind", "Kind");
  const createdAt = value(status, "created_at", "CreatedAt");
  return timelineRow({
    glyph: "status",
    time: createdAt,
    title: `${renderStatusKindBadge(kind)}<span class="timeline-actor">${escapeHTML(actor || "")}</span>`,
    meta: renderMarkdown(message || "", { inline: true }),
  });
}

export function renderTimelineTransitionRow(entry) {
  const eventKind = value(entry, "event_kind", "EventKind");
  const createdAt = value(entry, "created_at", "CreatedAt");
  const isSession = eventKind === "session_ready" || eventKind === "session_state_changed";
  const glyph = isSession ? "session" : "transition";
  const fromPhase = value(entry, "from_phase", "FromPhase") || "—";
  const toPhase = value(entry, "to_phase", "ToPhase");
  const guardResult = value(entry, "guard_result", "GuardResult");
  const actor = value(entry, "actor", "Actor");
  const sessionID = value(entry, "session_id", "SessionID");
  const sessionState = value(entry, "session_state", "SessionState");
  const headSHA = value(entry, "head_sha", "HeadSHA");
  const changeID = value(entry, "change_id", "ChangeID");
  let title;
  let metaParts = [];
  let actions = "";
  if (isSession) {
    const label = eventKind === "session_ready" ? "session ready" : "session state";
    title = `<span class="badge idle session-state">${escapeHTML(label)}</span>`;
    if (sessionState) title += ` <span class="muted">→ ${escapeHTML(sessionState)}</span>`;
    if (headSHA) metaParts.push(`head ${escapeHTML(shortSHA(headSHA))}`);
    if (changeID) metaParts.push(`<a href="/ui/changes/${escapeAttr(changeID)}" data-link>${escapeHTML(changeID)}</a>`);
    // Only render controls when the backend enriched the row with a session id;
    // terminal/transcript availability is checked server-side for the top-N
    // sessions list, so for out-of-list rows we offer a transcript (best-effort
    // GET) but no terminal to avoid a button that cannot open.
    if (sessionID) {
      const buttons = [];
      buttons.push(renderTranscriptButton("session", sessionID, { iconOnly: true }));
      if (buttons.length) actions = `<div class="actions timeline-row-actions">${buttons.join("")}</div>`;
    }
  } else {
    title = `<span class="badge idle">${escapeHTML(eventKind)}</span>`;
    metaParts.push(`${renderPhaseBadge(fromPhase)}<span class="arrow">→</span>${renderPhaseBadge(toPhase)}`);
  }
  if (actor) metaParts.push(`<span class="muted">${escapeHTML(actor)}</span>`);
  if (guardResult) metaParts.push(`<span class="muted">${escapeHTML(guardResult)}</span>`);
  return timelineRow({ glyph, time: createdAt, title, meta: metaParts.join(" · "), actions });
}

// renderTimeline renders the unified, full-width timeline that replaces the
// activity-column Sessions/Status feeds and the standalone Lifecycle
// transitions feed. It merges sessions + transitions + status entries by
// timestamp (newest first), caps the rendered rows at TIMELINE_CAP with a
// "Show more" control that expands to the full feed.
export function renderTimeline({ sessions, transitions, statusLog }) {
  const entries = buildTimelineEntries(sessions, transitions, statusLog);
  if (!entries.length) return `<div class="empty">No timeline activity yet</div>`;
  const visible = entries.slice(0, TIMELINE_CAP);
  const hiddenCount = entries.length - visible.length;
  const rowsHTML = visible.map(renderTimelineRow).join("");
  const moreHTML = hiddenCount > 0
    ? `<button class="button secondary timeline-show-more" type="button" data-timeline-show-more>Show ${hiddenCount} more</button>`
    : "";
  const hiddenHTML = hiddenCount > 0
    ? `<div class="timeline-hidden" hidden>${entries.slice(TIMELINE_CAP).map(renderTimelineRow).join("")}</div>`
    : "";
  return `<div class="feed timeline-feed" data-timeline>${rowsHTML}${hiddenHTML}${moreHTML}</div>`;
}
