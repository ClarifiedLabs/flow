// Task relations: who this task depends on and who depends on it. The flat,
// direction-agnostic rows the API returns are grouped by the model into the five
// lists a reader expects (parent, children, blocks, blocked-by, related); this
// element renders them as links, lets you add or remove a relation, and flags
// the blockers that have not finished yet.
//
// The grouping is pure and lives in task-model.js so it is testable as data.
// Whether a blocker is done comes for free: the relation payload denormalizes
// each side's lifecycle state, so the grouping marks a blocked-by row
// unresolved at projection time. No per-blocker fetch — a task with N blockers
// costs no extra round trips when its relations panel opens.

import { taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
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
  // The blocker flag is derived from the relation payload's denormalized
  // lifecycle state during grouping, so this element is a plain render shell:
  // every refresh of the task view carries fresh blocker states and paints them
  // directly, with no per-blocker fetch to drive or dedupe.
  render(model) {
    return renderTaskRelations(model);
  }
}

define("flow-task-relations", FlowTaskRelations);
