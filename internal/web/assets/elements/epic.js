// The epic rollup: how a planned group of tasks is actually going. One
// segmented bar for the shape of it, member rows for the detail, and the
// dependency chain that is actually gating the whole thing.

import { epicHref, projectTaskHref, taskHref, workItemHref as genericWorkItemHref } from "../api.js";
import { formatDwell } from "../board-model.js";
import { phaseKey } from "../board.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { LIFECYCLE_IN_PROGRESS, LIFECYCLE_SCHEDULED, LIFECYCLE_UNSCHEDULED, lifecycleStateOf } from "../lifecycle.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";
import { renderWorkItemContext } from "../work-item-detail.js";

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
  if (value(data, "item", "Item")) return renderFirstClassEpic(data);
  const epic = value(data, "epic", "Epic") || {};
  const members = value(data, "members", "Members") || [];
  const total = Number(value(data, "total_count", "TotalCount") || members.length);
  const merged = Number(value(data, "merged_count", "MergedCount") || 0);
  const criticalPath = value(data, "critical_path", "CriticalPath") || [];
  const projectID = data.projectID || "";
  const epicID = value(epic, "id", "ID");
  // Old epic payloads still represent a real navigation context when mounted by
  // the epic route. Pure legacy callers with no currentHref keep their original
  // globally-resolvable task URLs.
  const childNavigation = data.currentHref
    ? { context: epicID, returnTo: data.currentHref }
    : {};

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
      ${members.map((member) => renderMember(member, projectID, now, childNavigation)).join("")}
    </div>
    ${renderCriticalPath(criticalPath, members, projectID, childNavigation)}
  `;
}

function workItemHref(projectID, item, navigation = {}) {
  return genericWorkItemHref(projectID, item, navigation);
}

function renderFirstClassEpic(data) {
  const epic = value(data, "epic", "Epic") || {};
  const children = value(data, "children", "Children") || [];
  const blockers = value(data, "blockers", "Blockers") || [];
  const projectID = String(data.projectID || "");
  const id = value(epic, "id", "ID");
  const childNavigation = { context: id, returnTo: data.currentHref || epicHref(projectID, id, data.navigation) };
  const status = value(epic, "status", "Status") || "open";
  const policy = value(epic, "completion_policy", "CompletionPolicy") || "all_children";
  const unresolved = blockers.filter((blocker) => !value(blocker, "resolved", "Resolved"));
  return `
    <section class="detail">
      <div class="head">
        <div class="title-row">
          <span class="epic-id">${escapeHTML(id)}</span>
          <h2>${escapeHTML(value(epic, "title", "Title"))}</h2>
          <span class="spacer"></span>
          <span class="feature-status" data-status="${escapeAttr(status)}">${escapeHTML(status)}</span>
        </div>
        <p class="legend">${children.length} direct child${children.length === 1 ? "" : "ren"} · ${escapeHTML(policy.replace(/_/g, " "))}${unresolved.length ? ` · ${unresolved.length} blocked` : ""}</p>
        ${value(epic, "body", "Body") ? `<p>${escapeHTML(value(epic, "body", "Body"))}</p>` : ""}
        ${status !== "archived" ? `<div class="actions">
          ${status === "open" ? `<button type="button" data-epic-start="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Start descendant tasks</button>
          <button type="button" class="secondary" data-epic-complete="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Complete</button>` : `<button type="button" data-epic-reopen="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Reopen</button>`}
          <button type="button" class="secondary danger" data-epic-archive="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">Archive</button>
        </div>` : ""}
      </div>
      ${status !== "archived" ? `<form class="feature-edit" data-epic-form="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">
        <input name="title" value="${escapeAttr(value(epic, "title", "Title"))}" required aria-label="Epic title">
        <textarea name="body" rows="2" aria-label="Epic body">${escapeHTML(value(epic, "body", "Body"))}</textarea>
        <input name="priority" type="number" min="0" value="${escapeAttr(value(epic, "priority", "Priority") || 0)}" aria-label="Epic priority">
        <select name="completion_policy" aria-label="Epic completion policy">
          <option value="all_children"${policy === "all_children" ? " selected" : ""}>Complete with all children</option>
          <option value="manual"${policy === "manual" ? " selected" : ""}>Manual completion</option>
        </select>
        <div><button type="submit" class="secondary">Save</button></div>
      </form>` : ""}
      ${data.hierarchy ? renderWorkItemContext({ projectID, item: value(data.hierarchy, "item", "Item") || value(data, "item", "Item"), items: data.workItems || [], ancestors: value(data.hierarchy, "ancestors", "Ancestors"), relations: value(data.hierarchy, "relations", "Relations") || [], blockers: value(data.hierarchy, "blockers", "Blockers") || [], rollup: value(data.hierarchy, "rollup", "Rollup"), attentionCount: Number(value(data.hierarchy, "attention_count", "AttentionCount") || 0), navigation: data.navigation, currentHref: data.currentHref }) : ""}
      ${!data.hierarchy && unresolved.length ? `<div class="members"><h3>Blocked by</h3>${unresolved.map((blocker) => {
        const blockerItem = value(blocker, "item", "Item") || {};
        return `<a class="member" href="${escapeAttr(workItemHref(projectID, blockerItem, data.navigation))}" data-link><span class="member-id">${escapeHTML(value(blockerItem, "id", "ID"))}</span><span class="member-title">${escapeHTML(value(blockerItem, "title", "Title"))}</span></a>`;
      }).join("")}</div>` : ""}
      ${!data.hierarchy ? `<div class="members"><h3>Children</h3>${children.length ? children.map((child) => `<a class="member" href="${escapeAttr(workItemHref(projectID, child, childNavigation))}" data-link><span class="member-id">${escapeHTML(value(child, "id", "ID"))}</span><span class="member-title">${escapeHTML(value(child, "title", "Title"))}</span><span class="member-note">${escapeHTML(value(child, "state", "State")?.status || value(child, "kind", "Kind"))}</span></a>`).join("") : `<p class="empty">No children yet.</p>`}</div>` : ""}
    </section>`;
}

function renderMember(member, projectID, now, navigation = {}) {
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
    <a class="member" href="${escapeAttr(navigation.context ? projectTaskHref(projectID, id, navigation) : taskHref(projectID, id))}" data-link
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

function renderCriticalPath(path, members, projectID, navigation = {}) {
  if (!path.length) return "";
  const byID = new Map(members.map((member) => [value(member, "id", "ID"), member]));
  const chain = path
    .map((id) => {
      const phase = statePhase(memberState(byID.get(id) || {}));
      return `<a href="${escapeAttr(navigation.context ? projectTaskHref(projectID, id, navigation) : taskHref(projectID, id))}" data-link data-phase="${escapeAttr(phase)}">${escapeHTML(shortID(id))}</a>`;
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
