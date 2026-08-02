// The epic rollup: how a planned group of tasks is actually going. One
// segmented bar for the shape of it, member rows for the detail, and the
// dependency chain that is actually gating the whole thing.

import { taskHref } from "../api.js";
import { formatDwell } from "../board-model.js";
import { phaseKey } from "../board.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { LIFECYCLE_IN_PROGRESS, LIFECYCLE_SCHEDULED, LIFECYCLE_UNSCHEDULED, lifecycleStateOf } from "../lifecycle.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

// memberState reduces a member to the one word the rollup groups by. The
// lifecycle-derived buckets read the member's state through the shared
// vocabulary (lifecycleStateOf), so a state the server has not shipped the
// vocabulary for fails closed into the unscheduled bucket instead of landing
// in a wrong working/queued group.
export function memberState(member) {
  if (value(member, "needs_you", "NeedsYou")) return "needs you";
  const resolution = String(value(member, "resolution", "Resolution") || "");
  if (resolution === "merged") return "merged";
  if (resolution) return resolution;
  if (value(member, "held", "Held")) return "held";
  if ((value(member, "blocked_by", "BlockedBy") || []).length) return "blocked";
  const state = lifecycleStateOf(member);
  if (state === LIFECYCLE_IN_PROGRESS) return "working";
  if (state === LIFECYCLE_SCHEDULED) return "queued";
  return LIFECYCLE_UNSCHEDULED;
}

const STATE_PHASE = {
  "needs you": "await",
  merged: "merged",
  held: "triage",
  blocked: "blocked",
  working: "authoring",
  queued: "up_next",
  [LIFECYCLE_UNSCHEDULED]: "backlog",
  completed: "merged",
  ready: "approved",
};

function statePhase(state) {
  return STATE_PHASE[state] || phaseKey(state) || "backlog";
}

// The epic surface has no injected model clock — the route mounts the raw
// payload — so the wall clock is captured once per render and shared by every
// member's dwell note: one render, one clock, and a fixed clock can be injected
// for tests.
export function renderEpic(data, now = Date.now()) {
  if (!data) return "";
  const epic = value(data, "epic", "Epic") || {};
  const members = value(data, "members", "Members") || [];
  const total = Number(value(data, "total_count", "TotalCount") || members.length);
  const merged = Number(value(data, "merged_count", "MergedCount") || 0);
  const criticalPath = value(data, "critical_path", "CriticalPath") || [];
  const projectID = data.projectID || "";

  // Group once and reuse for both the bar and its legend, so the two can never
  // disagree about the counts.
  const groups = new Map();
  for (const member of members) {
    const state = memberState(member);
    groups.set(state, (groups.get(state) || 0) + 1);
  }
  const ordered = [...groups.entries()];

  return `
    <div class="head">
      <div class="title-row">
        <span class="epic-id">${escapeHTML(value(epic, "id", "ID"))}</span>
        <h2>${escapeHTML(value(epic, "title", "Title"))}</h2>
        <span class="spacer"></span>
        <span class="count">${total} task${total === 1 ? "" : "s"} · ${merged} merged</span>
      </div>
      <div class="rollup" role="img" aria-label="${escapeAttr(ordered.map(([state, count]) => `${count} ${state}`).join(", "))}">
        ${ordered
          .map(
            ([state, count]) =>
              `<span data-phase="${escapeAttr(statePhase(state))}" style="flex:${count}"></span>`,
          )
          .join("")}
      </div>
      <p class="legend">
        ${ordered
          .map(
            ([state, count]) =>
              `<span class="legend-item${state === "needs you" ? " is-attention" : ""}" data-phase="${escapeAttr(statePhase(state))}"><i></i>${count} ${escapeHTML(state)}</span>`,
          )
          .join("")}
      </p>
    </div>
    <div class="members">
      ${members.map((member) => renderMember(member, projectID, now)).join("")}
    </div>
    ${renderCriticalPath(criticalPath, members, projectID)}
  `;
}

function renderMember(member, projectID, now) {
  const state = memberState(member);
  const id = value(member, "id", "ID");
  const blockedBy = value(member, "blocked_by", "BlockedBy") || [];
  const stepIndex = Number(value(member, "step_index", "StepIndex") || 0);
  const stepCount = Number(value(member, "step_count", "StepCount") || 0);
  const needsYou = Boolean(value(member, "needs_you", "NeedsYou"));

  let note = state;
  if (needsYou) note = `needs you ${formatDwell(value(member, "dwell_since", "DwellSince"), now)}`;
  else if (blockedBy.length) note = `blocked by ${blockedBy.map((blocker) => shortID(blocker)).join(", ")}`;
  else if (stepCount) note = `${value(member, "step_name", "StepName") || "step"} ${stepIndex}/${stepCount}`;

  return `
    <a class="member" href="${escapeAttr(taskHref(projectID, id))}" data-link
       data-phase="${escapeAttr(statePhase(state))}"
       ${needsYou ? "data-needs-you" : ""}
       ${state === "merged" ? "data-merged" : ""}>
      <span class="member-id">${escapeHTML(id)}</span>
      <span class="member-title">${escapeHTML(value(member, "title", "Title"))}</span>
      <span class="spacer"></span>
      <span class="member-note">${escapeHTML(note)}</span>
    </a>
  `;
}

function renderCriticalPath(path, members, projectID) {
  if (!path.length) return "";
  const byID = new Map(members.map((member) => [value(member, "id", "ID"), member]));
  const chain = path
    .map((id) => {
      const phase = statePhase(memberState(byID.get(id) || {}));
      return `<a href="${escapeAttr(taskHref(projectID, id))}" data-link data-phase="${escapeAttr(phase)}">${escapeHTML(shortID(id))}</a>`;
    })
    .join('<span class="arrow">→</span>');
  return `
    <div class="critical">
      <span class="critical-label">Critical path</span>
      ${chain}
      <span class="spacer"></span>
      <a class="critical-action" href="/ui/board" data-link>Open as board filter</a>
    </div>
  `;
}

// Identifiers are content and appear everywhere, but a chain of full ids is
// unreadable; the trailing ordinal is enough to follow one.
function shortID(id) {
  const parts = String(id || "").split("-");
  return parts.length > 1 ? parts[parts.length - 1] : String(id || "");
}

export class FlowEpic extends FlowElement {
  render(data) {
    return renderEpic(data);
  }
}

define("flow-epic", FlowEpic);
