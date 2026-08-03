// Task relations: who this task depends on and who depends on it. The flat,
// direction-agnostic rows the API returns are grouped by the model into the five
// lists a reader expects (parent, children, blocks, blocked-by, related); this
// element renders them as links, lets you add or remove a relation, and flags
// the blockers that have not finished yet.
//
// The grouping is pure and lives in task-model.js so it is testable as data.
// Whether a blocker is done comes for free: the relation payload denormalizes
// each side's lifecycle state, so the grouping marks a blocked-by row at
// projection time. No per-blocker fetch — a task with N blockers costs no extra
// round trips when its relations panel opens.
//
// A blocker's derived state has three outcomes, and the row renders all three
// distinctly: a confirmed non-done blocker is flagged as blocking, a confirmed
// done blocker is left alone, and a value we cannot trust is unknown, rendered
// neutrally rather than as the red confirmed blocking state, so a bad payload
// is not mistaken for a real obstacle. Presence is part of the contract: the
// wire encoding of a valid unscheduled task is a *present* empty string (the
// server's SourceState is a non-pointer LifecycleState), so "" is a confirmed
// blocker, matching the server's own read model. An absent or null state, a
// state that is present but outside the lifecycle vocabulary (whitespace, an
// unknown token), or one that is not a string at all is malformed and renders
// unknown.

import { taskHref, workItemHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { RELATION_GROUPS } from "../task-model.js";
import { groupWorkItemRelations, WORK_ITEM_RELATION_GROUPS, workItemID } from "../work-item-model.js";
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
  const generic = Boolean(model.genericRelations);
  const groupDefinitions = generic ? WORK_ITEM_RELATION_GROUPS.filter(({ key }) => !["parent", "children"].includes(key)) : RELATION_GROUPS;
  const groups = model.relationGroups || (generic ? groupWorkItemRelations(model.relations || [], model.id) : {});
  const total = groupDefinitions.reduce((sum, group) => sum + (groups[group.key]?.length || 0), 0);

  return `
    <div class="relations">
      <span class="caption">${generic ? "Dependencies" : "Relations"}</span>
      ${
        total === 0
          ? `<p class="rel-empty">No ${generic ? "dependencies" : "relations"} yet</p>`
          : groupDefinitions.map((group) => renderGroup(group, groups[group.key] || [], model)).join("")
      }
      ${renderAddForm(model.id || "", model.projectID || "", generic)}
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
  const generic = Boolean(model.genericRelations);
  const other = item.item || {};
  const otherID = generic ? workItemID(other) : item.taskID;
  const otherTitle = generic ? (other.title || other.Title || otherID) : item.title;
  // Only a blocker that has not finished is worth flagging: a done blocker is
  // history, not an obstacle. A blocker whose state could not be confirmed is
  // unknown — it may well be blocking, but saying so for certain would be a
  // lie, so it gets the neutral unknown marker instead of the red flag.
  const isBlocker = groupKey === "blockedBy";
  const unresolved = isBlocker && (generic ? !item.resolved : item.unresolved === true);
  const unknown = !generic && isBlocker && item.unresolved === null;
  const rowClass = unresolved ? " is-unresolved" : unknown ? " is-unknown" : "";
  const source = generic ? item.sourceID : (item.direction === "source" ? taskID : otherID);
  const target = generic ? item.targetID : (item.direction === "source" ? otherID : taskID);
  return `
    <li class="rel-row${rowClass}"${unresolved ? ` data-unresolved="true"` : ""}>
      <a class="rel-link" href="${escapeAttr(generic ? workItemHref(projectID, other, model.navigation) : taskHref(projectID, otherID, model.navigation))}" data-link>
        <span class="rel-title">${escapeHTML(otherTitle)}</span>
        <span class="rel-id">${escapeHTML(otherID)}</span>
      </a>
      ${unresolved ? `<span class="rel-flag">blocking</span>` : unknown ? `<span class="rel-flag rel-flag-unknown">unknown</span>` : ""}
      <button class="rel-remove" type="button" aria-label="${escapeAttr(`Remove relation to ${otherTitle}`)}"
        ${generic ? `data-work-item-relation-remove="${escapeAttr(taskID)}" data-source="${escapeAttr(source)}"` : `data-relation-remove="${escapeAttr(source)}"`}
        data-project="${escapeAttr(projectID)}" data-kind="${escapeAttr(item.kind)}" data-target="${escapeAttr(target)}">×</button>
    </li>
  `;
}

function renderAddForm(taskID, projectID, generic = false) {
  const options = generic ? RELATION_KIND_OPTIONS.filter(([kind]) => kind !== "parent_of") : RELATION_KIND_OPTIONS;
  return `
    <form class="rel-add" ${generic ? "data-work-item-relation-add-form" : "data-relation-add-form"}="${escapeAttr(taskID)}" data-project="${escapeAttr(projectID)}">
      <select name="kind" aria-label="Relation kind">
        ${options.map(([kind, label]) => `<option value="${escapeAttr(kind)}">${escapeHTML(label)}</option>`).join("")}
      </select>
      <input name="${generic ? "target_item_id" : "target_task_id"}" type="text" placeholder="${generic ? "Work item ID" : "Task ID"}" aria-label="Target ${generic ? "work item" : "task"} ID" required />
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
