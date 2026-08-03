import assert from "node:assert/strict";
import test from "node:test";

import {
  actionableWorkItemCompare,
  buildWorkItemIndex,
  descendantTaskRollup,
  effectiveFeaturePath,
  groupTasksByContainer,
  groupWorkItemRelations,
  nearestContainer,
  topContainer,
  visibleWorkItemTree,
  workItemAncestors,
  workItemRollups,
  workItemDescendants,
} from "./work-item-model.js";

const fixture = {
  items: [
    { id: "e-1", kind: "epic", title: "Epic", priority: 3, state: { status: "open" } },
    { id: "f-1", kind: "feature", title: "Feature", parent_item_id: "e-1", state: { status: "open" } },
    { id: "t-1", kind: "task", title: "Done", parent_item_id: "f-1", effective_feature_id: "f-1", state: { status: "done", terminal: true, successful: true } },
    { id: "t-2", kind: "task", title: "Blocked", parent_item_id: "e-1", state: { status: "scheduled" }, unresolved_blockers: 1 },
    { id: "t-3", kind: "task", title: "Standalone", state: { status: "unscheduled" } },
  ],
};

test("buildWorkItemIndex derives stable ancestry, descendants and containers", () => {
  const index = buildWorkItemIndex(fixture);
  assert.deepEqual(index.roots.map((item) => item.id), ["e-1", "t-3"]);
  assert.deepEqual(workItemAncestors(index, "t-1").map((item) => item.id), ["f-1", "e-1"]);
  assert.deepEqual(workItemDescendants(index, "e-1").map((item) => item.id), ["f-1", "t-1", "t-2"]);
  assert.equal(nearestContainer(index, "t-1").id, "f-1");
  assert.equal(topContainer(index, "t-1").id, "e-1");
});

test("nested generic detail payloads flatten without losing inferred parentage", () => {
  const index = buildWorkItemIndex({
    item: { id: "e-1", kind: "epic" },
    children: [{ item: { id: "f-1", kind: "feature" }, children: [{ item: { id: "t-1", kind: "task" } }] }],
  });
  assert.deepEqual(workItemAncestors(index, "t-1").map((item) => item.id), ["f-1", "e-1"]);
});

test("cycles, duplicate IDs, self parents and missing parents fail open as roots", () => {
  const index = buildWorkItemIndex({ items: [
    { id: "a", kind: "epic", parent_item_id: "b" },
    { id: "b", kind: "feature", parent_item_id: "a" },
    { id: "self", kind: "epic", parent_item_id: "self" },
    { id: "lost", kind: "task", parent_item_id: "missing" },
    { id: "lost", kind: "task", title: "duplicate" },
  ] });
  assert.deepEqual([...index.cycles].sort(), ["a", "b"]);
  assert.deepEqual(index.roots.map((item) => item.id).sort(), ["a", "b", "lost", "self"]);
  assert.equal(index.byID.get("lost").title, undefined);
});

test("descendant task rollups are task-only and preserve unknown open states", () => {
  const index = buildWorkItemIndex({ items: [...fixture.items, { id: "t-4", kind: "task", parent_item_id: "e-1", state: { status: "paused" } }] });
  assert.deepEqual(descendantTaskRollup(index, "e-1"), {
    total: 3, closed: 1, successful: 1, unsuccessful: 0,
    inProgress: 0, scheduled: 1, unscheduled: 0, otherOpen: 1, blocked: 1,
  });
});

test("filter/search keeps ancestors visible so matching descendants retain context", () => {
  const index = buildWorkItemIndex(fixture);
  assert.deepEqual([...visibleWorkItemTree(index, { filter: "blocked", query: "blocked" })].sort(), ["e-1", "t-2"]);
  assert.deepEqual([...visibleWorkItemTree(index, { filter: "open", query: "done" })], []);
});

test("postorder rollups expose descendant progress and direct child readiness", () => {
  const index = buildWorkItemIndex(fixture);
  const rollups = workItemRollups(index);
  assert.deepEqual(rollups.get("e-1"), {
    tasks: { total: 2, closed: 1, successful: 1, unsuccessful: 0, inProgress: 0, scheduled: 1, unscheduled: 0, otherOpen: 0, blocked: 1 },
    direct: { total: 2, closed: 0, ready: 1, blocked: 1 },
    blockedDescendants: 1,
  });
  assert.deepEqual(effectiveFeaturePath(index, "t-1").map((entry) => entry.id), ["e-1", "f-1"]);
});

test("completed filtering and actionable ordering are deterministic", () => {
  const index = buildWorkItemIndex(fixture);
  assert.deepEqual([...visibleWorkItemTree(index, { filter: "completed" })].sort(), ["e-1", "f-1", "t-1"]);
  const sorted = [fixture.items[4], fixture.items[3], fixture.items[1]].sort(actionableWorkItemCompare);
  assert.deepEqual(sorted.map((item) => item.id), ["t-2", "f-1", "t-3"]);
});

test("generic relation grouping uses endpoint summaries across item kinds", () => {
  const groups = groupWorkItemRelations([
    { source: { id: "e-1", kind: "epic", title: "Epic" }, target: { id: "t-1", kind: "task", title: "Task" }, kind: "parent_of" },
    { source: { id: "f-1", kind: "feature", title: "Feature" }, target: { id: "t-1", kind: "task", title: "Task" }, kind: "blocks" },
    { source: { id: "t-1", kind: "task", title: "Task" }, target: { id: "e-2", kind: "epic", title: "Other" }, kind: "related_to" },
  ], "t-1");
  assert.equal(groups.parent[0].item.kind, "epic");
  assert.equal(groups.blockedBy[0].item.kind, "feature");
  assert.equal(groups.related[0].item.kind, "epic");
});

test("task grouping uses top-level containers, effective feature, and standalone fallback", () => {
  const index = buildWorkItemIndex(fixture);
  const tasks = fixture.items.filter((item) => item.kind === "task");
  assert.deepEqual(groupTasksByContainer(index, tasks, "container").map((group) => [group.id, group.tasks.map((item) => item.id)]), [
    ["e-1", ["t-1", "t-2"]], ["standalone", ["t-3"]],
  ]);
  assert.deepEqual(groupTasksByContainer(index, tasks, "feature").map((group) => group.id), ["e-1", "f-1", "standalone"]);
});
