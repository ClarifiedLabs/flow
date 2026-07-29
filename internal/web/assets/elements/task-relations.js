// Task relations: who this task depends on and who depends on it. The flat,
// direction-agnostic rows the API returns are grouped by the model into the five
// lists a reader expects (parent, children, blocks, blocked-by, related); this
// element renders them as links, lets you add or remove a relation, and flags
// the blockers that have not finished yet.
//
// The grouping is pure and lives in task-model.js so it is testable as data.
// The one thing the grouping cannot know — whether a blocker is done — is
// resolved here, because it needs a fetch per blocker. A blocker's state is not
// in the detail payload, so every refresh of the task view asks for it again;
// the last answer is kept only as a flicker guard while a re-check is in
// flight, never as a permanent cache. The element writes the answer back onto
// the group rows and repaints.

import { apiGet, taskAPIBase, taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { RELATION_GROUPS } from "../task-model.js";
import { define, FlowElement } from "./base.js";

// RELATION_KIND_OPTIONS is what the add-relation selector offers. The verb reads
// from the current task outward: picking "blocks" and a target means this task
// blocks the target; the server stores the row with this task as the source.
export const RELATION_KIND_OPTIONS = [
  ["blocks", "blocks"],
  ["parent_of", "parent of"],
  ["related_to", "related to"],
];

export function renderTaskRelations(model) {
  if (!model) return "";
  const groups = model.relationGroups || {};
  const total = RELATION_GROUPS.reduce((sum, group) => sum + (groups[group.key]?.length || 0), 0);

  return `
    <div class="relations">
      <span class="caption">Relations</span>
      ${
        total === 0
          ? `<p class="rel-empty">No relations yet</p>`
          : RELATION_GROUPS.map((group) => renderGroup(group, groups[group.key] || [], model)).join("")
      }
      ${renderAddForm(model.id || "", model.projectID || "")}
    </div>
  `;
}

function renderGroup(group, items, model) {
  if (!items.length) return "";
  return `
    <div class="rel-group" data-rel-group="${escapeAttr(group.key)}">
      <span class="rel-label">${escapeHTML(group.label)}<span class="rel-count">${items.length}</span></span>
      <ul class="rel-list">
        ${items.map((item) => renderRelationRow(item, group.key, model)).join("")}
      </ul>
    </div>
  `;
}

function renderRelationRow(item, groupKey, model) {
  const taskID = model.id || "";
  const projectID = model.projectID || "";
  // Only a blocker that has not finished is worth flagging: a done blocker is
  // history, not an obstacle.
  const unresolved = groupKey === "blockedBy" && item.unresolved;
  const source = item.direction === "source" ? taskID : item.taskID;
  const target = item.direction === "source" ? item.taskID : taskID;
  return `
    <li class="rel-row${unresolved ? " is-unresolved" : ""}"${unresolved ? ` data-unresolved="true"` : ""}>
      <a class="rel-link" href="${escapeAttr(taskHref(projectID, item.taskID))}" data-link>
        <span class="rel-title">${escapeHTML(item.title)}</span>
        <span class="rel-id">${escapeHTML(item.taskID)}</span>
      </a>
      ${unresolved ? `<span class="rel-flag">blocking</span>` : ""}
      <button class="rel-remove" type="button" aria-label="${escapeAttr(`Remove relation to ${item.title}`)}"
        data-relation-remove="${escapeAttr(source)}" data-project="${escapeAttr(projectID)}"
        data-kind="${escapeAttr(item.kind)}" data-target="${escapeAttr(target)}">×</button>
    </li>
  `;
}

function renderAddForm(taskID, projectID) {
  return `
    <form class="rel-add" data-relation-add-form="${escapeAttr(taskID)}" data-project="${escapeAttr(projectID)}">
      <select name="kind" aria-label="Relation kind">
        ${RELATION_KIND_OPTIONS.map(([kind, label]) => `<option value="${escapeAttr(kind)}">${escapeHTML(label)}</option>`).join("")}
      </select>
      <input name="target_task_id" type="text" placeholder="Task ID" aria-label="Target task ID" required />
      <button class="button secondary" type="submit">Link</button>
    </form>
  `;
}

export class FlowTaskRelations extends FlowElement {
  // blockerStates remembers the last observed lifecycle state of each blocker
  // so the flag survives a repaint while a re-check is in flight. Keyed by
  // task id; "" means the fetch failed and we treat the blocker as unresolved
  // rather than silently clearing the flag.
  blockerStates = new Map();
  // blockerFetches dedupes in-flight lookups per blocker id within one refresh
  // of the task view, so the repaint cascade a refresh triggers (the
  // invalidate after applying a new state) costs one request per blocker.
  blockerFetches = new Map();
  #generation = 0;

  // Each assignment of `data` is one refresh of the task view. Re-checks are
  // keyed to that generation: a refresh always asks the server again — caching
  // the answer forever would keep the flag standing after a blocker finishes —
  // but the paints one refresh triggers share the fetch already in flight.
  set data(next) {
    this.#generation += 1;
    super.data = next;
  }

  get data() {
    return super.data;
  }

  render(model) {
    return renderTaskRelations(model);
  }

  // The base paint skips the write — and with it afterPaint — when the markup
  // is unchanged, but a refresh that only changed a blocker's state leaves the
  // markup identical while the blocker states still need re-checking. Drive
  // that from every paint attempt, not just from writes.
  paint() {
    super.paint();
    this.afterPaint();
  }

  afterPaint() {
    if (this.applyBlockerStates()) {
      this.invalidate();
      return;
    }
    this.refreshBlockerStates();
  }

  // applyBlockerStates writes the cached blocker states onto the current model's
  // blocked-by rows and reports whether anything changed, so the caller knows a
  // repaint is owed.
  applyBlockerStates() {
    const blockers = this.data?.relationGroups?.blockedBy || [];
    let changed = false;
    for (const blocker of blockers) {
      const state = this.blockerStates.get(blocker.taskID);
      if (state === undefined) continue;
      const unresolved = state !== "done";
      if (blocker.unresolved !== unresolved) {
        blocker.unresolved = unresolved;
        changed = true;
      }
    }
    return changed;
  }

  // refreshBlockerStates re-checks every blocker on every refresh. A blocker's
  // state is the one fact the detail payload does not carry, so each fresh
  // model the task view receives asks for it again. The last known state stays
  // applied while the re-check is in flight, so the flag never flickers away
  // only to return.
  refreshBlockerStates() {
    const model = this.data;
    if (!model) return;
    const blockers = model.relationGroups?.blockedBy || [];
    const ids = [...new Set(blockers.map((blocker) => blocker.taskID).filter(Boolean))];
    for (const cached of [...this.blockerStates.keys()]) {
      if (!ids.includes(cached)) this.blockerStates.delete(cached);
    }
    for (const cached of [...this.blockerFetches.keys()]) {
      if (!ids.includes(cached)) this.blockerFetches.delete(cached);
    }
    if (!ids.length) return;
    const projectID = model.projectID || "";
    const taskID = model.id;
    for (const id of ids) {
      this.fetchBlockerState(projectID, id).then((state) => {
        // Navigation may have moved us to another task while the fetch was in
        // flight; drop the result rather than painting stale flags onto a
        // different task.
        if (!this.isConnected || this.data?.id !== taskID) return;
        this.blockerStates.set(id, state);
        if (this.applyBlockerStates()) this.invalidate();
      });
    }
  }

  fetchBlockerState(projectID, id) {
    const inflight = this.blockerFetches.get(id);
    if (inflight && inflight.generation === this.#generation) return inflight.promise;
    const promise = apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(id)}`)
      .then((data) => String(value(value(data, "task", "Task") || {}, "state", "State") || ""))
      .catch(() => "");
    this.blockerFetches.set(id, { promise, generation: this.#generation });
    return promise;
  }
}

define("flow-task-relations", FlowTaskRelations);
