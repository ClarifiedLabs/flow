// Pure work-item projections shared by Work overview, container details and Tasks.
// The API may return either a flat `items` list or nested WorkItemResponse trees;
// normalize once here so every view agrees about ancestry, rollups and relations.

import { value } from "./normalize.js";

export const WORK_ITEM_KINDS = new Set(["epic", "feature", "task"]);
export const WORK_ITEM_FILTERS = new Set(["all", "open", "blocked", "completed"]);
export const WORK_ITEM_VIEWS = new Set(["overview", "tree"]);
// Kept for the later Tasks grouping slice; the overview does not consume it.
export const WORK_ITEM_GROUPS = new Set(["container", "feature", "none"]);
export const TASK_CONTAINER_STANDALONE = "standalone";
export const TASK_CONTAINER_UNKNOWN = "unknown";
export const WORK_ITEM_RELATION_GROUPS = [
  { key: "parent", label: "Parent" },
  { key: "children", label: "Children" },
  { key: "blocks", label: "Blocks" },
  { key: "blockedBy", label: "Blocked by" },
  { key: "related", label: "Related" },
];

export function workItemID(item) {
  return String(value(item || {}, "id", "ID") || "");
}

export function workItemKind(item) {
  const kind = String(value(item || {}, "kind", "Kind") || "task");
  return WORK_ITEM_KINDS.has(kind) ? kind : "task";
}

export function workItemState(item) {
  const state = value(item || {}, "state", "State");
  if (state && typeof state === "object") {
    return {
      status: String(value(state, "status", "Status") || "unknown"),
      terminal: Boolean(value(state, "terminal", "Terminal")),
      successful: Boolean(value(state, "successful", "Successful")),
    };
  }
  const status = String(state || "unscheduled");
  return { status, terminal: status === "done", successful: false };
}

export function workItemParentID(item) {
  return String(value(item || {}, "parent_item_id", "ParentItemID") || "");
}

export function workItemFeatureID(item) {
  return String(value(item || {}, "effective_feature_id", "EffectiveFeatureID") || "");
}

export function workItemCompare(a, b) {
  const kindOrder = { epic: 0, feature: 1, task: 2 };
  const kind = kindOrder[workItemKind(a)] - kindOrder[workItemKind(b)];
  if (kind) return kind;
  const priority = Number(value(b, "priority", "Priority") || 0) - Number(value(a, "priority", "Priority") || 0);
  if (priority) return priority;
  const title = String(value(a, "title", "Title") || workItemID(a)).localeCompare(String(value(b, "title", "Title") || workItemID(b)));
  return title || workItemID(a).localeCompare(workItemID(b));
}

// Portfolio ordering puts work that needs attention ahead of passive backlog,
// while retaining deterministic kind/priority/title tie breakers.
export function actionableWorkItemCompare(a, b) {
  const attention = (item) => {
    const state = workItemState(item);
    const blockers = Number(value(item, "unresolved_blockers", "UnresolvedBlockers") || 0);
    if (blockers > 0) return 0;
    if (state.status === "in_progress") return 1;
    if (state.status === "scheduled") return 2;
    if (!state.terminal) return 3;
    return 4;
  };
  const rank = attention(a) - attention(b);
  if (rank) return rank;
  const updated = Date.parse(value(b, "updated_at", "UpdatedAt") || 0) - Date.parse(value(a, "updated_at", "UpdatedAt") || 0);
  return (Number.isFinite(updated) && updated) || workItemCompare(a, b);
}

function flattenPayload(payload) {
  const flat = [];
  const seenObjects = new Set();
  const visit = (entry, inheritedParent = "") => {
    if (!entry || typeof entry !== "object" || seenObjects.has(entry)) return;
    seenObjects.add(entry);
    const item = value(entry, "item", "Item") || entry;
    if (!item || typeof item !== "object") return;
    const id = workItemID(item);
    if (id) {
      if (inheritedParent && !workItemParentID(item)) flat.push({ ...item, parent_item_id: inheritedParent });
      else flat.push(item);
    }
    const children = value(entry, "children", "Children") || [];
    for (const child of Array.isArray(children) ? children : []) visit(child, id || inheritedParent);
  };
  const source = value(payload || {}, "items", "Items") || (Array.isArray(payload) ? payload : [payload]);
  for (const entry of Array.isArray(source) ? source : []) visit(entry);
  return flat;
}

// buildWorkItemIndex is deliberately cycle/orphan tolerant. Invalid parent links
// become roots and cycle members are recorded, ensuring a corrupt row can never
// recurse forever or make the rest of a project's work disappear.
export function buildWorkItemIndex(payload) {
  const byID = new Map();
  for (const item of flattenPayload(payload)) {
    const id = workItemID(item);
    if (id && !byID.has(id)) byID.set(id, item);
  }
  const children = new Map();
  const parent = new Map();
  const orphans = [];
  for (const item of byID.values()) {
    const id = workItemID(item);
    const parentID = workItemParentID(item);
    if (!parentID) continue;
    if (parentID === id || !byID.has(parentID)) {
      orphans.push(item);
      continue;
    }
    parent.set(id, parentID);
    if (!children.has(parentID)) children.set(parentID, []);
    children.get(parentID).push(item);
  }
  const cycles = new Set();
  for (const id of byID.keys()) {
    const path = [];
    const positions = new Map();
    let current = id;
    while (parent.has(current)) {
      if (positions.has(current)) {
        for (const member of path.slice(positions.get(current))) cycles.add(member);
        break;
      }
      positions.set(current, path.length);
      path.push(current);
      current = parent.get(current);
    }
  }
  if (cycles.size) {
    for (const id of cycles) {
      const parentID = parent.get(id);
      parent.delete(id);
      const siblings = children.get(parentID) || [];
      children.set(parentID, siblings.filter((item) => workItemID(item) !== id));
    }
  }
  for (const list of children.values()) list.sort(workItemCompare);
  const roots = [...byID.values()].filter((item) => !parent.has(workItemID(item))).sort(workItemCompare);
  return { items: [...byID.values()], byID, children, parent, roots, orphans, cycles };
}

export function workItemAncestors(index, itemOrID) {
  const result = [];
  const seen = new Set();
  let id = typeof itemOrID === "string" ? itemOrID : workItemID(itemOrID);
  while (index.parent.has(id)) {
    const parentID = index.parent.get(id);
    if (seen.has(parentID)) break;
    seen.add(parentID);
    const item = index.byID.get(parentID);
    if (!item) break;
    result.push(item);
    id = parentID;
  }
  return result;
}

export function workItemDescendants(index, itemOrID) {
  const rootID = typeof itemOrID === "string" ? itemOrID : workItemID(itemOrID);
  const result = [];
  const seen = new Set([rootID]);
  const pending = [...(index.children.get(rootID) || [])];
  while (pending.length) {
    const item = pending.shift();
    const id = workItemID(item);
    if (!id || seen.has(id)) continue;
    seen.add(id);
    result.push(item);
    pending.unshift(...(index.children.get(id) || []));
  }
  return result;
}

export function nearestContainer(index, itemOrID) {
  const item = typeof itemOrID === "string" ? index.byID.get(itemOrID) : itemOrID;
  if (!item) return null;
  if (workItemKind(item) !== "task") return item;
  return workItemAncestors(index, item).find((candidate) => workItemKind(candidate) !== "task") || null;
}

export function topContainer(index, itemOrID) {
  const item = typeof itemOrID === "string" ? index.byID.get(itemOrID) : itemOrID;
  if (!item) return null;
  const chain = [item, ...workItemAncestors(index, item)];
  return [...chain].reverse().find((candidate) => workItemKind(candidate) !== "task") || null;
}

function emptyTaskRollup() {
  return { total: 0, closed: 0, successful: 0, unsuccessful: 0, inProgress: 0, scheduled: 0, unscheduled: 0, otherOpen: 0, blocked: 0 };
}

function addTaskToRollup(rollup, task) {
  const state = workItemState(task);
  rollup.total += 1;
  if (state.terminal) {
    rollup.closed += 1;
    if (state.successful) rollup.successful += 1;
    else rollup.unsuccessful += 1;
  } else if (state.status === "in_progress") rollup.inProgress += 1;
  else if (state.status === "scheduled") rollup.scheduled += 1;
  else if (state.status === "unscheduled" || state.status === "open") rollup.unscheduled += 1;
  else rollup.otherOpen += 1;
  if (Number(value(task, "unresolved_blockers", "UnresolvedBlockers") || 0) > 0) rollup.blocked += 1;
}

function mergeTaskRollup(target, source) {
  for (const key of Object.keys(target)) target[key] += source[key];
}

// Computes every container's descendant-task and direct-child status in one
// bounded postorder traversal. The index has already severed corrupt cycles.
export function workItemRollups(index) {
  const rollups = new Map();
  const visit = (item) => {
    const id = workItemID(item);
    const tasks = emptyTaskRollup();
    const direct = { total: 0, closed: 0, ready: 0, blocked: 0 };
    let blockedDescendants = 0;
    for (const child of index.children.get(id) || []) {
      const childRollup = visit(child);
      direct.total += 1;
      const childState = workItemState(child);
      const childBlockers = Number(value(child, "unresolved_blockers", "UnresolvedBlockers") || 0);
      if (childState.terminal) direct.closed += 1;
      else if (childBlockers > 0) direct.blocked += 1;
      else direct.ready += 1;
      if (workItemKind(child) === "task") addTaskToRollup(tasks, child);
      else mergeTaskRollup(tasks, childRollup.tasks);
      blockedDescendants += childRollup.blockedDescendants + (childBlockers > 0 ? 1 : 0);
    }
    const result = { tasks, direct, blockedDescendants };
    rollups.set(id, result);
    return result;
  };
  for (const root of index.roots) visit(root);
  return rollups;
}

export function descendantTaskRollup(index, itemOrID) {
  const item = typeof itemOrID === "string" ? index.byID.get(itemOrID) : itemOrID;
  if (!item) return emptyTaskRollup();
  if (workItemKind(item) === "task") {
    const rollup = emptyTaskRollup();
    addTaskToRollup(rollup, item);
    return rollup;
  }
  return workItemRollups(index).get(workItemID(item))?.tasks || emptyTaskRollup();
}

export function effectiveFeaturePath(index, itemOrID) {
  const item = typeof itemOrID === "string" ? index.byID.get(itemOrID) : itemOrID;
  if (!item) return [];
  const featureID = workItemFeatureID(item);
  if (!featureID) return [];
  const feature = index.byID.get(featureID);
  if (!feature) return [{ id: featureID, item: null }];
  return [...workItemAncestors(index, feature).reverse(), feature]
    .filter((entry) => workItemKind(entry) !== "task")
    .map((entry) => ({ id: workItemID(entry), item: entry }));
}

// A task can be directly contained by one chain while executing on a feature
// inherited through a different chain. Keep both projections separate: callers
// may render both, but must never append the task to its effective feature path
// and thereby imply a second direct parent.
export function taskWorkContext(index, itemOrID, requestedContext = "") {
  const task = typeof itemOrID === "string" ? index.byID.get(itemOrID) : itemOrID;
  if (!task) return { directAncestors: [], effectiveFeaturePath: [], validContextIDs: new Set(), contextItem: null };
  const directAncestors = workItemAncestors(index, task).reverse();
  const featurePath = effectiveFeaturePath(index, task);
  const validContextIDs = new Set([
    ...directAncestors.map(workItemID),
    // An effective_feature_id can outlive its summary. It remains useful as a
    // neutral label, but cannot validate requested navigation until the feature
    // (and therefore its ancestry) actually resolved.
    ...featurePath.filter((entry) => entry.item).map((entry) => entry.id),
  ]);
  const contextID = String(requestedContext || "").trim();
  return {
    directAncestors,
    effectiveFeaturePath: featurePath,
    validContextIDs,
    contextItem: contextID && validContextIDs.has(contextID) ? index.byID.get(contextID) || null : null,
  };
}

export function matchesWorkItem(item, { filter = "open", query = "", kinds = WORK_ITEM_KINDS } = {}) {
  if (kinds && !kinds.has(workItemKind(item))) return false;
  const state = workItemState(item);
  const blockers = Number(value(item, "unresolved_blockers", "UnresolvedBlockers") || 0);
  if (filter === "open" && state.terminal) return false;
  if (filter === "blocked" && (state.terminal || blockers < 1)) return false;
  if (filter === "completed" && !state.terminal) return false;
  const needle = String(query || "").trim().toLocaleLowerCase();
  if (!needle) return true;
  return [workItemID(item), value(item, "title", "Title"), value(item, "body", "Body")]
    .some((part) => String(part || "").toLocaleLowerCase().includes(needle));
}

export function visibleWorkItemTree(index, options = {}) {
  const visible = new Set();
  for (const item of index.items) {
    if (!matchesWorkItem(item, options)) continue;
    visible.add(workItemID(item));
    for (const ancestor of workItemAncestors(index, item)) visible.add(workItemID(ancestor));
  }
  return visible;
}

function endpointSummary(relation, side) {
  return value(relation, side, side[0].toUpperCase() + side.slice(1)) || {};
}

export function groupWorkItemRelations(relations, itemID) {
  const groups = Object.fromEntries(WORK_ITEM_RELATION_GROUPS.map(({ key }) => [key, []]));
  const id = String(itemID || "");
  for (const relation of relations || []) {
    const kind = String(value(relation, "kind", "Kind") || "");
    const source = endpointSummary(relation, "source");
    const target = endpointSummary(relation, "target");
    const sourceID = workItemID(source);
    const targetID = workItemID(target);
    let group = "";
    let other = null;
    let direction = "";
    if (sourceID === id) {
      group = kind === "parent_of" ? "children" : kind === "blocks" ? "blocks" : kind === "related_to" ? "related" : "";
      other = target;
      direction = "source";
    } else if (targetID === id) {
      group = kind === "parent_of" ? "parent" : kind === "blocks" ? "blockedBy" : kind === "related_to" ? "related" : "";
      other = source;
      direction = "target";
    }
    if (group && other) groups[group].push({ item: other, kind, direction, resolved: Boolean(value(relation, "resolved", "Resolved")), sourceID, targetID });
  }
  return groups;
}

// Missing summaries and malformed parent chains must not be mistaken for
// genuinely standalone tasks. Keep that defensive distinction shared by every
// Tasks projection that consumes a work-item index.
export function classifyTaskContainer(index, taskOrID) {
  const id = typeof taskOrID === "string" ? taskOrID : workItemID(taskOrID);
  const item = index?.byID?.get(id);
  if (!id || !item || String(value(item, "kind", "Kind") || "") !== "task") {
    return { id: TASK_CONTAINER_UNKNOWN, item: null, kind: TASK_CONTAINER_UNKNOWN };
  }
  const malformed = new Set([...(index.orphans || []).map(workItemID), ...(index.cycles || [])]);
  const ancestry = [item, ...workItemAncestors(index, id)];
  if (ancestry.some((entry) => malformed.has(workItemID(entry)))) {
    return { id: TASK_CONTAINER_UNKNOWN, item: null, kind: TASK_CONTAINER_UNKNOWN };
  }
  if (!workItemParentID(item)) {
    return { id: TASK_CONTAINER_STANDALONE, item: null, kind: TASK_CONTAINER_STANDALONE };
  }
  const container = topContainer(index, id);
  if (!container || workItemKind(container) === "task") {
    return { id: TASK_CONTAINER_UNKNOWN, item: null, kind: TASK_CONTAINER_UNKNOWN };
  }
  return { id: workItemID(container), item: container, kind: "container" };
}

export function groupTasksByContainer(index, tasks, mode = "container") {
  if (mode === "none") return [{ id: "all", item: null, tasks: [...tasks] }];
  const groups = new Map();
  for (const task of tasks) {
    let classification;
    if (mode === "feature") {
      const indexed = index.byID.get(workItemID(task)) || task;
      const container = index.byID.get(workItemFeatureID(indexed)) || null;
      classification = container
        ? { id: workItemID(container), item: container, kind: "container" }
        : classifyTaskContainer(index, task);
    } else {
      classification = classifyTaskContainer(index, task);
    }
    if (!groups.has(classification.id)) groups.set(classification.id, { ...classification, tasks: [] });
    groups.get(classification.id).tasks.push(task);
  }
  const rank = (group) => group.item ? 0 : group.id === TASK_CONTAINER_STANDALONE ? 1 : 2;
  return [...groups.values()].sort((a, b) => {
    const order = rank(a) - rank(b);
    if (order) return order;
    if (!a.item || !b.item) return a.id.localeCompare(b.id);
    return workItemCompare(a.item, b.item);
  });
}
