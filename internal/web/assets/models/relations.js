// Task-relations projection: the flat, direction-agnostic relation rows
// the API returns, grouped from the viewed task's point of view, plus the
// lifecycle vocabulary the blocker verdict keys on.

import { LIFECYCLE_DONE } from "../lifecycle.js";
import { value } from "../normalize.js";

// LIFECYCLE_DONE is the one state that clears a blocker. Its canonical home
// is the shared lifecycle vocabulary in lifecycle.js (guarded by
// lifecycle_parity_test.go); it is re-exported here so the relations API
// surface keeps naming it.
export { LIFECYCLE_DONE };

// epicParent finds the task that planned this one, which is where the rail's
// EPIC link goes.
export function epicParent(relations, taskID) {
  for (const relation of relations) {
    const kind = value(relation, "kind", "Kind");
    const source = value(relation, "source_task_id", "SourceTaskID");
    const target = value(relation, "target_task_id", "TargetTaskID");
    if (kind === "parent_of" && target === taskID) return source;
  }
  return "";
}

// RELATION_GROUPS is the order the relations panel lists its sections in. Each
// group is one direction of one relation kind, read from the current task's
// point of view.
export const RELATION_GROUPS = [
  { key: "parent", label: "Parent" },
  { key: "children", label: "Children" },
  { key: "blocks", label: "Blocks" },
  { key: "blockedBy", label: "Blocked by" },
  { key: "related", label: "Related" },
];

// The lifecycle vocabulary below mirrors the server's task lifecycle
// (LifecycleState in internal/coordinator/tasks.go) and is the single source of
// truth for the task-relations verdict. "" is the wire encoding of a valid
// unscheduled task: the relation payload's SourceState is a non-pointer
// LifecycleState, so an unscheduled blocker ships a *present* empty state and —
// like the server's blocked-by read model, which clears a blocker only once it
// is done — is a confirmed blocker. Every other member of LIFECYCLE_UNFINISHED
// is a non-done lifecycle state the server serializes. The Go parity test
// TestTaskRelationsLifecycleParity (internal/web) parses these exports and
// fails if they drift from the Go constants — enumerated exhaustively in
// coordinator.AllLifecycleStates — so adding a server lifecycle state cannot
// silently leave this allowlist stale.
export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);

// blockerVerdict reads the denormalized lifecycle state a relation payload ships
// for the blocker side and reduces it to the tri-state the relations row renders:
// true is a confirmed unfinished blocker, false a confirmed done one, and null an
// unknown one. A missing or null state is a malformed payload the read model
// never emits, and a state that is present but outside the lifecycle vocabulary
// (whitespace, an unknown token) or not a string at all is one we cannot trust —
// both render unknown rather than being read as a confirmed non-done blocker.
export function blockerVerdict(relation) {
  const state = relation == null ? undefined : relation["source_state"] ?? relation["SourceState"];
  if (state === LIFECYCLE_DONE) return false;
  if (typeof state !== "string") return null; // absent, null, or not a string: malformed.
  if (LIFECYCLE_UNFINISHED.has(state)) return true;
  return null; // any other string is outside the lifecycle vocabulary.
}

// relationGroups turns the flat, direction-agnostic relation rows the API
// returns into the five lists a reader expects, each holding the *other* task
// relative to the one being viewed. A relation row names a source and a target;
// which side is "the other task" flips depending on which side the current task
// is on, so each group records the direction too — the add/remove controls need
// it to reconstruct the exact row the server stores.
export function relationGroups(relations, taskID) {
  const groups = {};
  for (const group of RELATION_GROUPS) groups[group.key] = [];
  const id = String(taskID || "");

  for (const relation of relations || []) {
    const kind = String(value(relation, "kind", "Kind") || "");
    const source = String(value(relation, "source_task_id", "SourceTaskID") || "");
    const target = String(value(relation, "target_task_id", "TargetTaskID") || "");
    const sourceTitle = String(value(relation, "source_title", "SourceTitle") || "");
    const targetTitle = String(value(relation, "target_title", "TargetTitle") || "");

    // The current task is the source: the other task is the target.
    if (source === id) {
      if (kind === "parent_of") {
        groups.children.push(entry(target, targetTitle, kind, "source"));
      } else if (kind === "blocks") {
        groups.blocks.push(entry(target, targetTitle, kind, "source"));
      } else if (kind === "related_to") {
        groups.related.push(entry(target, targetTitle, kind, "source"));
      }
      continue;
    }

    // The current task is the target: the other task is the source.
    if (target === id) {
      if (kind === "parent_of") {
        groups.parent.push(entry(source, sourceTitle, kind, "target"));
      } else if (kind === "blocks") {
        // A blocker only stops mattering once it is done; its denormalized
        // lifecycle state decides whether the row is a confirmed blocker, a
        // finished one, or an unknown one (see blockerVerdict).
        groups.blockedBy.push(entry(source, sourceTitle, kind, "target", blockerVerdict(relation)));
      } else if (kind === "related_to") {
        groups.related.push(entry(source, sourceTitle, kind, "target"));
      }
    }
  }
  return groups;
}

// entry is one row in a relation group. direction says which side of the stored
// relation the current task sits on, so the remove control can reconstruct the
// exact source/target pair the server stores. unresolved flags a blocker that
// has not finished; only blocked-by rows ever set it, from the relation's
// denormalized lifecycle state.
function entry(taskID, title, kind, direction, unresolved = false) {
  return {
    taskID,
    title: title || taskID,
    kind,
    direction,
    unresolved,
  };
}
