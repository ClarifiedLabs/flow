// Tests for task relations: the pure grouping in task-model.js, the rendered
// markup of the relations element, the unresolved-blocker flag, the empty
// state, and the add/remove controls that hit the relations endpoint.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

installTestDOM();

const { relationGroups, RELATION_GROUPS, taskModel, blockerVerdict, LIFECYCLE_UNFINISHED, LIFECYCLE_DONE } = await import("./task-model.js");
const { renderTaskRelations, RELATION_KIND_OPTIONS } = await import("./elements/task-relations.js");
const { handleFormSubmit } = await import("./forms.js");
const { handleAction, inFlight } = await import("./actions.js");

const TASK_ID = "t-me";

// oneOfEach returns one relation row for each of the five groups the detail view
// renders, all relative to TASK_ID. The rows use the snake_case spelling the
// value() helper reads, mirroring the API payload.
function oneOfEach() {
  return [
    // Someone is my parent: I am the target of a parent_of.
    { source_task_id: "t-parent", target_task_id: TASK_ID, kind: "parent_of", source_title: "Parent epic", target_title: "My task" },
    // I am the parent: the child is the target of my parent_of.
    { source_task_id: TASK_ID, target_task_id: "t-child", kind: "parent_of", source_title: "My task", target_title: "Child task" },
    // I block someone: the blocked task is the target of my blocks.
    { source_task_id: TASK_ID, target_task_id: "t-blocked", kind: "blocks", source_title: "My task", target_title: "Blocked task" },
    // Someone blocks me: the blocker is the source of a blocks I am target of.
    // The row ships the blocker's denormalized lifecycle state, as the API does.
    { source_task_id: "t-blocker", target_task_id: TASK_ID, kind: "blocks", source_title: "Blocking task", target_title: "My task", source_state: "in_progress" },
    // A symmetric related_to where I am the source.
    { source_task_id: TASK_ID, target_task_id: "t-related", kind: "related_to", source_title: "My task", target_title: "Related task" },
  ];
}

// --- grouping ----------------------------------------------------------------

test("relationGroups buckets one of each kind into five groups relative to the task", () => {
  const groups = relationGroups(oneOfEach(), TASK_ID);

  assert.equal(groups.parent.length, 1);
  assert.equal(groups.parent[0].taskID, "t-parent");
  assert.equal(groups.parent[0].title, "Parent epic");
  assert.equal(groups.parent[0].direction, "target");

  assert.equal(groups.children.length, 1);
  assert.equal(groups.children[0].taskID, "t-child");
  assert.equal(groups.children[0].title, "Child task");
  assert.equal(groups.children[0].direction, "source");

  assert.equal(groups.blocks.length, 1);
  assert.equal(groups.blocks[0].taskID, "t-blocked");
  assert.equal(groups.blocks[0].direction, "source");

  assert.equal(groups.blockedBy.length, 1);
  assert.equal(groups.blockedBy[0].taskID, "t-blocker");
  assert.equal(groups.blockedBy[0].title, "Blocking task");
  assert.equal(groups.blockedBy[0].direction, "target");

  assert.equal(groups.related.length, 1);
  assert.equal(groups.related[0].taskID, "t-related");
});

test("relationGroups reads the PascalCase spelling the Go structs serialize", () => {
  const groups = relationGroups(
    [
      { SourceTaskID: "t-blocker", TargetTaskID: TASK_ID, Kind: "blocks", SourceTitle: "Blocking task" },
      { SourceTaskID: TASK_ID, TargetTaskID: "t-child", Kind: "parent_of", TargetTitle: "Child task" },
    ],
    TASK_ID,
  );
  assert.equal(groups.blockedBy[0].taskID, "t-blocker");
  assert.equal(groups.children[0].taskID, "t-child");
});

test("relationGroups falls back to the task id when a title is missing", () => {
  const groups = relationGroups([{ source_task_id: TASK_ID, target_task_id: "t-x", kind: "blocks" }], TASK_ID);
  assert.equal(groups.blocks[0].title, "t-x");
});

test("relationGroups ignores rows the task is not part of and unknown kinds", () => {
  const groups = relationGroups(
    [
      { source_task_id: "t-a", target_task_id: "t-b", kind: "blocks" },
      { source_task_id: TASK_ID, target_task_id: "t-c", kind: "mystery" },
    ],
    TASK_ID,
  );
  for (const { key } of RELATION_GROUPS) assert.equal(groups[key].length, 0);
});

test("a related_to where the task is the target still lands in related", () => {
  const groups = relationGroups(
    [{ source_task_id: "t-other", target_task_id: TASK_ID, kind: "related_to", source_title: "Other" }],
    TASK_ID,
  );
  assert.equal(groups.related.length, 1);
  assert.equal(groups.related[0].taskID, "t-other");
  assert.equal(groups.related[0].direction, "target");
});

// --- unresolved derivation from the payload ----------------------------------

test("relationGroups marks a blocked-by row unresolved from the blocker's denormalized state", () => {
  const groups = relationGroups(
    [
      { source_task_id: "t-live", target_task_id: TASK_ID, kind: "blocks", source_state: "in_progress" },
      { source_task_id: "t-finished", target_task_id: TASK_ID, kind: "blocks", source_state: "done" },
      { source_task_id: "t-unscheduled", target_task_id: TASK_ID, kind: "blocks", source_state: "" },
    ],
    TASK_ID,
  );
  const byID = Object.fromEntries(groups.blockedBy.map((row) => [row.taskID, row.unresolved]));
  assert.equal(byID["t-live"], true, "an unfinished blocker is unresolved");
  assert.equal(byID["t-finished"], false, "a done blocker is resolved");
  // A present empty state is the wire encoding of a valid unscheduled task, so
  // it is a confirmed blocker, matching the server's read model.
  assert.equal(byID["t-unscheduled"], true, "an unscheduled blocker is unresolved");
});

test("relationGroups reads the PascalCase lifecycle state the Go structs serialize", () => {
  const groups = relationGroups(
    [{ SourceTaskID: "t-blocker", TargetTaskID: TASK_ID, Kind: "blocks", SourceState: "done" }],
    TASK_ID,
  );
  assert.equal(groups.blockedBy[0].unresolved, false);
});

test("only blocked-by rows are ever marked unresolved", () => {
  const groups = relationGroups(
    [
      { source_task_id: "t-parent", target_task_id: TASK_ID, kind: "parent_of", source_state: "in_progress" },
      { source_task_id: TASK_ID, target_task_id: "t-child", kind: "parent_of", target_state: "in_progress" },
      { source_task_id: TASK_ID, target_task_id: "t-blocked", kind: "blocks", target_state: "in_progress" },
      { source_task_id: TASK_ID, target_task_id: "t-related", kind: "related_to", target_state: "in_progress" },
    ],
    TASK_ID,
  );
  for (const key of ["parent", "children", "blocks", "related"]) {
    for (const row of groups[key]) assert.equal(row.unresolved, false, `${key} rows are never unresolved`);
  }
});

test("taskModel exposes the grouped relations alongside the raw rows", () => {
  const model = taskModel(
    { task: { id: TASK_ID, title: "My task", state: "in_progress" }, task_detail: { relations: oneOfEach() } },
    null,
  );
  assert.equal(model.relations.length, 5);
  assert.equal(model.relationGroups.blockedBy[0].taskID, "t-blocker");
  assert.equal(model.relationGroups.parent[0].taskID, "t-parent");
});

// --- rendered markup ---------------------------------------------------------

function relationsModel(overrides = {}) {
  return {
    id: TASK_ID,
    projectID: "p-1",
    relationGroups: relationGroups(oneOfEach(), TASK_ID),
    ...overrides,
  };
}

test("the relations panel renders five labelled groups with titles and links", () => {
  const html = renderTaskRelations(relationsModel());

  for (const { label } of RELATION_GROUPS) {
    assert.match(html, new RegExp(`>${label}<`), `missing group label ${label}`);
  }
  for (const [title, id] of [
    ["Parent epic", "t-parent"],
    ["Child task", "t-child"],
    ["Blocked task", "t-blocked"],
    ["Blocking task", "t-blocker"],
    ["Related task", "t-related"],
  ]) {
    assert.match(html, new RegExp(title), `missing title ${title}`);
    assert.match(html, new RegExp(`href="/ui/tasks/${id}"`), `missing link to ${id}`);
  }
  assert.match(html, /data-link/, "relation links use the delegated router");
});

test("every row carries a remove control naming the exact stored relation", () => {
  const html = renderTaskRelations(relationsModel());
  // The blocked-by row's stored source is the blocker, so the remove button
  // targets the blocker's path and names the current task as the target.
  assert.match(html, /data-relation-remove="t-blocker"[^>]*data-kind="blocks"[^>]*data-target="t-me"/);
  // A row where the current task is the source removes via its own path.
  assert.match(html, /data-relation-remove="t-me"[^>]*data-kind="parent_of"[^>]*data-target="t-child"/);
});

test("the add form offers the three outward relation kinds", () => {
  const html = renderTaskRelations(relationsModel());
  assert.match(html, /data-relation-add-form="t-me"/);
  for (const [kind] of RELATION_KIND_OPTIONS) {
    assert.match(html, new RegExp(`<option value="${kind}"`), `missing kind option ${kind}`);
  }
});

test("no relations renders a defined empty state, not a broken section", () => {
  const html = renderTaskRelations(relationsModel({ relationGroups: relationGroups([], TASK_ID) }));
  assert.match(html, /No relations yet/);
  assert.doesNotMatch(html, /rel-group/);
  // The add form is still present so the empty state is a starting point.
  assert.match(html, /data-relation-add-form="t-me"/);
});

test("a blocked-by row is only flagged when its blocker is unresolved", () => {
  // A done blocker derives unresolved=false, so nothing is flagged.
  const model = {
    id: TASK_ID,
    projectID: "p-1",
    relationGroups: relationGroups(
      [{ source_task_id: "t-blocker", target_task_id: TASK_ID, kind: "blocks", source_title: "Blocking task", source_state: "done" }],
      TASK_ID,
    ),
  };
  assert.doesNotMatch(renderTaskRelations(model), /is-unresolved/);

  // Flip the derived flag (as a non-done blocker would) and the flag appears.
  model.relationGroups.blockedBy[0].unresolved = true;
  const flagged = renderTaskRelations(model);
  assert.match(flagged, /is-unresolved/);
  assert.match(flagged, /blocking/);

  model.relationGroups.blockedBy[0].unresolved = false;
  assert.doesNotMatch(renderTaskRelations(model), /is-unresolved/);
});

test("only the blocked-by group is ever flagged unresolved", () => {
  const model = relationsModel();
  for (const { key } of RELATION_GROUPS) {
    for (const item of model.relationGroups[key]) item.unresolved = true;
  }
  const html = renderTaskRelations(model);
  // Exactly one flagged row: the blocked-by one.
  assert.equal((html.match(/is-unresolved/g) || []).length, 1);
});

test("a blocked-by row whose state is unknown renders a neutral marker, not the blocking flag", () => {
  const model = relationsModel();
  model.relationGroups.blockedBy[0].unresolved = null;
  const html = renderTaskRelations(model);
  assert.match(html, /is-unknown/, "an unknown blocker is marked neutrally");
  assert.match(html, /rel-flag-unknown/, "the unknown marker uses the neutral style");
  assert.match(html, />unknown</, "the row names the unknown state");
  assert.doesNotMatch(html, /is-unresolved/, "an unknown blocker is not the confirmed blocking state");
  assert.doesNotMatch(html, />blocking</, "an unknown blocker does not carry the blocking label");

  // Only the blocked-by group can be unknown: null on another group is ignored.
  const other = relationsModel();
  other.relationGroups.blocks[0].unresolved = null;
  assert.doesNotMatch(renderTaskRelations(other), /is-unknown/);
});

// --- unresolved-blocker resolution (payload-driven) --------------------------

// blockedByRelations builds one blocked-by relation row per lifecycle state, so
// a test can describe a panel with several blockers without repeating the row
// shape. source_state is the denormalized lifecycle state the API ships in the
// relation payload — the only input the unresolved flag needs. taskID is the
// viewed task the rows are relative to; it defaults to TASK_ID.
function blockedByRelations(states, taskID = TASK_ID) {
  return states.map((state, index) => ({
    source_task_id: `t-blocker-${index}`,
    target_task_id: taskID,
    kind: "blocks",
    source_title: `Blocker ${index}`,
    source_state: state,
  }));
}

function modelWithBlockerRelations(relations, taskID = TASK_ID) {
  return {
    id: taskID,
    projectID: "p-1",
    relationGroups: relationGroups(relations, taskID),
  };
}

// modelWithBlockers builds a relations model for the supplied viewed task ID, so
// a test can hand one element instance models for distinct tasks in sequence.
function modelWithBlockers(states, taskID = TASK_ID) {
  return modelWithBlockerRelations(blockedByRelations(states, taskID), taskID);
}

// settle flushes the microtask repaints a data assignment schedules. The panel
// no longer fetches, so a handful is plenty; over-flushing is harmless.
async function settle(times = 4) {
  for (let i = 0; i < times; i++) await flush();
}

test("the panel flags every non-done blocker straight from the payload, with no fetch", async () => {
  const calls = [];
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    calls.push(path);
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
  };
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", modelWithBlockers(["in_progress", "done", "scheduled"]));
    await settle();

    assert.equal(calls.length, 0, "opening the panel makes no lifecycle-resolution requests");
    // Three blockers, but only the two that have not reached done are flagged.
    assert.equal(element.querySelectorAll(".rel-row").length, 3);
    assert.equal(element.querySelectorAll('[data-unresolved="true"]').length, 2);
    assert.equal((element.innerHTML.match(/is-unresolved/g) || []).length, 2);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the number of lifecycle-resolution requests is constant regardless of blocker count", async () => {
  // Acceptance: a task with N blockers must not fan out into N requests. The
  // panel reads the denormalized state, so the count is zero — and, crucially,
  // does not grow as blockers are added.
  const requestCount = async (states) => {
    const calls = [];
    const previousFetch = globalThis.fetch;
    globalThis.fetch = (path) => {
      calls.push(path);
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
    };
    try {
      const root = globalThis.document.body;
      const element = mountElement(root, "flow-task-relations", modelWithBlockers(states));
      await settle();
      element.remove();
      return calls.length;
    } finally {
      globalThis.fetch = previousFetch;
    }
  };

  const one = await requestCount(["in_progress"]);
  const many = await requestCount(["in_progress", "in_progress", "in_progress", "done", "scheduled"]);
  assert.equal(one, 0, "a single blocker costs no lifecycle request");
  assert.equal(many, one, "the request count does not grow with the number of blockers");
});

// blockerVerdict is the pure read of the denormalized lifecycle state the
// relation payload ships, and it is what decides whether a blocked-by row is a
// confirmed blocker, a finished one, or an unknown one.

test("blockerVerdict reads the lifecycle vocabulary the API serializes", () => {
  // done clears the flag; the other real states confirm an unfinished blocker.
  assert.equal(blockerVerdict({ source_state: "done" }), false);
  assert.equal(blockerVerdict({ source_state: "scheduled" }), true);
  assert.equal(blockerVerdict({ source_state: "in_progress" }), true);
});

test("blockerVerdict treats a present empty state as a confirmed blocker", () => {
  // The wire encoding of a valid unscheduled task is a *present* empty state
  // (the server's SourceState is a non-pointer LifecycleState); like the
  // server's read model, that is a blocker until proven done, never unknown.
  assert.equal(blockerVerdict({ source_state: "" }), true);
  assert.equal(blockerVerdict({ SourceState: "" }), true);
});

test("blockerVerdict treats a missing or null state as unknown, not blocking", () => {
  // The server always serializes SourceState, so a payload without it — or with
  // an explicit null — is malformed lifecycle data, not an unscheduled task; it
  // must render unknown rather than the confirmed red blocking state.
  assert.equal(blockerVerdict({}), null);
  assert.equal(blockerVerdict({ source_state: null }), null);
  assert.equal(blockerVerdict({ SourceState: null }), null);
});

test("blockerVerdict renders a malformed lifecycle value unknown, not blocking", () => {
  // A state that is present but outside the lifecycle vocabulary — whitespace,
  // an unknown token, or a non-string — is a payload we cannot trust, so it is
  // unknown rather than read as a confirmed non-done blocker.
  assert.equal(blockerVerdict({ source_state: "   " }), null);
  assert.equal(blockerVerdict({ source_state: "bogus" }), null);
  assert.equal(blockerVerdict({ source_state: "DONE" }), null);
  assert.equal(blockerVerdict({ source_state: {} }), null);
  assert.equal(blockerVerdict({ SourceState: "bogus" }), null);
});

test("blockerVerdict agrees with the exported lifecycle vocabulary", () => {
  // The vocabulary is the anchor the Go parity test checks against the server's
  // constants; the verdict must treat every unfinished member as a confirmed
  // blocker and the done state as finished.
  for (const state of LIFECYCLE_UNFINISHED) {
    assert.equal(blockerVerdict({ source_state: state }), true, `vocabulary state ${JSON.stringify(state)} must confirm a blocker`);
  }
  assert.equal(blockerVerdict({ source_state: LIFECYCLE_DONE }), false, "the done state must clear a blocker");
});

test("a blocked-by row with a malformed state renders unknown, not the blocking flag", async () => {
  const root = globalThis.document.body;
  const element = mountElement(root, "flow-task-relations", modelWithBlockers(["bogus"]));
  await settle();

  assert.match(element.innerHTML, /is-unknown/, "a malformed state is marked unknown");
  assert.match(element.innerHTML, /rel-flag-unknown/, "the unknown marker uses the neutral style");
  assert.match(element.innerHTML, />unknown</, "the row names the unknown state");
  assert.doesNotMatch(element.innerHTML, /is-unresolved/, "a malformed state is not the confirmed blocking state");
  assert.doesNotMatch(element.innerHTML, />blocking</, "a malformed state does not carry the blocking label");
  assert.equal(element.data.relationGroups.blockedBy[0].unresolved, null);
  element.remove();
});

test("a blocked-by row with a missing source_state renders unknown, not the blocking flag", async () => {
  // The server always serializes SourceState, so a payload without the field is
  // malformed lifecycle data; it must render the neutral unknown marker rather
  // than the confirmed red blocking state.
  const root = globalThis.document.body;
  const element = mountElement(
    root,
    "flow-task-relations",
    modelWithBlockerRelations([{ source_task_id: "t-blocker", target_task_id: TASK_ID, kind: "blocks", source_title: "Blocker" }]),
  );
  await settle();

  assert.match(element.innerHTML, /is-unknown/, "a missing state is marked unknown");
  assert.match(element.innerHTML, /rel-flag-unknown/, "the unknown marker uses the neutral style");
  assert.doesNotMatch(element.innerHTML, /is-unresolved/, "a missing state is not the confirmed blocking state");
  assert.doesNotMatch(element.innerHTML, />blocking</, "a missing state does not carry the blocking label");
  assert.equal(element.data.relationGroups.blockedBy[0].unresolved, null);
  element.remove();
});

test("a blocked-by row with a null source_state renders unknown, not the blocking flag", async () => {
  // An explicit null is malformed lifecycle data in the same way an absent
  // field is: not the present empty string the server serializes for a valid
  // unscheduled task, so it renders unknown rather than blocking.
  const root = globalThis.document.body;
  const element = mountElement(
    root,
    "flow-task-relations",
    modelWithBlockerRelations([{ source_task_id: "t-blocker", target_task_id: TASK_ID, kind: "blocks", source_title: "Blocker", source_state: null }]),
  );
  await settle();

  assert.match(element.innerHTML, /is-unknown/, "a null state is marked unknown");
  assert.match(element.innerHTML, /rel-flag-unknown/, "the unknown marker uses the neutral style");
  assert.doesNotMatch(element.innerHTML, /is-unresolved/, "a null state is not the confirmed blocking state");
  assert.doesNotMatch(element.innerHTML, />blocking</, "a null state does not carry the blocking label");
  assert.equal(element.data.relationGroups.blockedBy[0].unresolved, null);
  element.remove();
});

test("a blocked-by row with an empty (unscheduled) state renders the blocking flag", async () => {
  // The denormalized payload ships "" for a valid unscheduled blocker; that is a
  // confirmed unfinished blocker, so it keeps the red marker rather than the
  // neutral unknown one.
  const root = globalThis.document.body;
  const element = mountElement(root, "flow-task-relations", modelWithBlockers([""]));
  await settle();

  assert.match(element.innerHTML, /is-unresolved/, "an unscheduled blocker is confirmed blocking");
  assert.match(element.innerHTML, />blocking</, "the row carries the blocking label");
  assert.doesNotMatch(element.innerHTML, /is-unknown/, "an unscheduled blocker is not unknown");
  assert.equal(element.data.relationGroups.blockedBy[0].unresolved, true);
  element.remove();
});

test("a blocker that finishes is unflagged on the next refresh of the task", async () => {
  const root = globalThis.document.body;
  const element = mountElement(root, "flow-task-relations", modelWithBlockers(["in_progress"]));
  await settle();
  assert.match(element.innerHTML, /is-unresolved/, "an in-progress blocker starts flagged");

  // The blocker finishes, and the task detail refreshes with a fresh model
  // carrying the new denormalized state — the same element instance, as the
  // rail reuses it.
  element.data = modelWithBlockers(["done"]);
  await settle();

  assert.doesNotMatch(element.innerHTML, /is-unresolved/, "a finished blocker is no longer flagged");
  element.remove();
});

test("a blocker that reopens is flagged again on the next refresh", async () => {
  const root = globalThis.document.body;
  const element = mountElement(root, "flow-task-relations", modelWithBlockers(["done"]));
  await settle();
  assert.doesNotMatch(element.innerHTML, /is-unresolved/);

  element.data = modelWithBlockers(["in_progress"]);
  await settle();

  assert.match(element.innerHTML, /is-unresolved/, "a reopened blocker is flagged again");
  element.remove();
});

test("navigating the mounted element to another task drops the previous task's blocker flags", async () => {
  // Acceptance: navigation guards must keep one task's blocker state from
  // leaking onto another. The panel is a stateless render shell, so the
  // guarantee is that each paint reflects only the model it was handed: when
  // the rail reuses the same element instance for a different task, the old
  // task's unresolved markers must not survive into the new task's view.
  const root = globalThis.document.body;
  const element = mountElement(root, "flow-task-relations", modelWithBlockers(["in_progress"], "t-task-a"));
  await settle();
  assert.equal(element.data.id, "t-task-a", "the mounted view is task A");
  assert.match(element.innerHTML, /is-unresolved/, "task A's live blocker starts flagged");

  // Navigate to a different task whose blockers are all done. Same element
  // instance, fresh model — as the rail reuses it across tasks. The second
  // model is built for a distinct viewed task ID, so this is a true
  // cross-task navigation, not a same-task refresh.
  element.data = modelWithBlockers(["done", "done"], "t-task-b");
  await settle();

  assert.equal(element.data.id, "t-task-b", "the mounted view is now task B");
  // The panel repainted for task B: the add form and the blocked-by rows are
  // relative to the new task, not the stale task A.
  assert.match(element.innerHTML, /data-relation-add-form="t-task-b"/, "the add form names task B");
  assert.match(element.innerHTML, /data-relation-remove="t-blocker-0"[^>]*data-target="t-task-b"/, "the blocked-by rows target task B");
  assert.doesNotMatch(element.innerHTML, /is-unresolved/, "task A's flags do not leak across navigation");
  assert.equal(element.querySelectorAll('[data-unresolved="true"]').length, 0);
  element.remove();
});

test("the rail forwards fresh data to the relations element even when its own markup is unchanged", async () => {
  // Regression: the rail only forwarded data in afterPaint, which the base
  // paint skips when the markup is stable — so a refresh that changed only a
  // blocker's state never reached the relations element.
  const { renderTaskRail } = await import("./elements/task-rail.js");
  await import("./elements/run-spine.js");

  const root = globalThis.document.body;
  const rail = mountElement(root, "flow-task-rail", modelWithBlockers(["in_progress"]));
  await settle();
  const relations = rail.querySelector("flow-task-relations");
  assert.ok(relations, "the rail mounts the relations element");
  assert.match(relations.innerHTML, /is-unresolved/);

  const markupBefore = rail.innerHTML;
  rail.data = modelWithBlockers(["done"]);
  await settle();

  assert.equal(rail.innerHTML, markupBefore, "the rail markup itself is unchanged");
  assert.equal(rail.innerHTML, renderTaskRail(modelWithBlockers(["done"])), "and still matches the model");
  assert.doesNotMatch(relations.innerHTML, /is-unresolved/, "the relations element saw the fresh model");
  rail.remove();
});

// --- add / remove controls ---------------------------------------------------

test("the add control POSTs {target_task_id, kind} to the relations endpoint and reloads", async () => {
  const fetchCalls = [];
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path, options) => {
    fetchCalls.push({ path, options });
    return Promise.resolve({ ok: true, status: 204, json: () => Promise.resolve(null) });
  };
  let refreshed = 0;
  try {
    const form = {
      tagName: "FORM",
      dataset: { project: "p-1", relationAddForm: TASK_ID },
      elements: {
        kind: { value: "blocks" },
        target_task_id: { value: "t-new" },
      },
      reportValidity: () => true,
      reset() {},
    };
    const handled = await handleFormSubmit({ setStatus() {}, async refresh() { refreshed += 1; } }, { target: form, preventDefault() {} });

    assert.equal(handled, true);
    assert.equal(fetchCalls.length, 1);
    assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-1/tasks/t-me/relations");
    assert.equal(fetchCalls[0].options.method, "POST");
    assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { target_task_id: "t-new", kind: "blocks" });
    assert.equal(refreshed, 1, "the detail view reloads after adding");
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the remove control DELETEs the exact relation row and reloads", async () => {
  const fetchCalls = [];
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path, options) => {
    fetchCalls.push({ path, options });
    return Promise.resolve({ ok: true, status: 204, json: () => Promise.resolve(null) });
  };
  let refreshed = 0;
  try {
    const root = globalThis.document.body;
    const model = relationsModel();
    model.relationGroups.blockedBy[0].unresolved = true;
    const element = mountElement(root, "flow-task-relations", model);
    await flush();

    const button = element.querySelector('[data-relation-remove="t-blocker"]');
    assert.ok(button, "the blocked-by row has a remove button");

    const handled = await handleAction(
      { setStatus() {}, async refresh() { refreshed += 1; } },
      { target: button, preventDefault() {} },
    );

    assert.equal(handled, true);
    const deletes = fetchCalls.filter((call) => call.options.method === "DELETE");
    assert.equal(deletes.length, 1);
    assert.equal(deletes[0].path, "/ui/api/v2/projects/p-1/tasks/t-blocker/relations");
    assert.deepEqual(JSON.parse(deletes[0].options.body), { target_task_id: TASK_ID, kind: "blocks" });
    assert.equal(refreshed, 1, "the detail view reloads after removing");
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the add control names the in-flight submission and confirms the added relation", async () => {
  // Regression: relationAddForm returned undefined, so once the refresh
  // cleared the pending status the dispatcher had no confirmation to show and
  // the status line went blank on success.
  const previousFetch = globalThis.fetch;
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, status: 204, json: () => Promise.resolve(null) });
    });
  try {
    const statuses = [];
    const form = {
      tagName: "FORM",
      dataset: { project: "p-1", relationAddForm: TASK_ID },
      elements: {
        kind: { value: "blocks" },
        target_task_id: { value: "t-new" },
      },
      reportValidity: () => true,
      reset() {},
    };
    const app = { setStatus: (message) => statuses.push(message), async refresh() {} };

    const handled = handleFormSubmit(app, { target: form, preventDefault() {} });

    // The pending status is synchronous, before the request resolves.
    assert.deepEqual(statuses, ["Adding relation\u2026"]);

    resolveRequest();
    assert.equal(await handled, true);
    // The confirmation is written after the handler's refresh, so it survives
    // the re-render clearing the status line.
    assert.deepEqual(statuses, ["Adding relation\u2026", "Relation added"]);
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the add control reports the missing target on the status line without a request", async () => {
  const previousFetch = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, status: 204, json: () => Promise.resolve(null) });
  };
  try {
    const statuses = [];
    const form = {
      tagName: "FORM",
      dataset: { project: "p-1", relationAddForm: TASK_ID },
      elements: {
        kind: { value: "blocks" },
        target_task_id: { value: " " },
      },
      reportValidity: () => true,
      reset() {},
    };
    const app = { setStatus: (message) => statuses.push(message), async refresh() {} };

    assert.equal(await handleFormSubmit(app, { target: form, preventDefault() {} }), true);
    assert.equal(requests, 0, "no request goes out without a target");
    assert.deepEqual(statuses, ["Adding relation\u2026", "Target task ID is required"]);
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the remove control names the in-flight action and confirms the removed relation", async () => {
  // Regression: relationRemove returned undefined, so once the refresh cleared
  // the pending status the dispatcher had no confirmation to show and the
  // status line went blank on success.
  const previousFetch = globalThis.fetch;
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, status: 204, json: () => Promise.resolve(null) });
    });
  try {
    const statuses = [];
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await flush();

    const button = element.querySelector('[data-relation-remove="t-blocker"]');
    assert.ok(button, "the blocked-by row has a remove button");

    const handled = handleAction(
      { setStatus: (message) => statuses.push(message), async refresh() {} },
      { target: button, preventDefault() {} },
    );

    // The pending state is synchronous: the control is busy and the status
    // line names the action before the request resolves.
    assert.equal(button.disabled, true);
    assert.deepEqual(statuses, ["Removing relation t-blocker\u2026"]);

    resolveRequest();
    assert.equal(await handled, true);
    // The confirmation is written after the handler's refresh, so it survives
    // the re-render clearing the status line, and the control is restored.
    assert.deepEqual(statuses, ["Removing relation t-blocker\u2026", "Relation removed"]);
    assert.equal(button.disabled, false);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("distinct relation removals sharing a source task proceed while an exact duplicate stays blocked", async () => {
  // Regression: actionBusyKey keyed relationRemove on the source task alone,
  // but the DELETE mutation is identified by project, source, target, and
  // kind. Two rows with the same source — here the task blocks one task and is
  // related to another — shared an in-flight key, so starting one removal
  // silently rejected the other as a duplicate, and a repaint marked the
  // unrelated row busy.
  const previousFetch = globalThis.fetch;
  const fetchCalls = [];
  const resolvers = [];
  globalThis.fetch = (path, options) => {
    fetchCalls.push({ path, options });
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, status: 204, json: () => Promise.resolve(null) }));
    });
  };
  try {
    const root = globalThis.document.body;
    // No blocked-by rows, so no blocker-state lookups interleave: every fetch
    // recorded here is a removal.
    const model = relationsModel({
      relationGroups: relationGroups(
        [
          { source_task_id: TASK_ID, target_task_id: "t-blocked", kind: "blocks", target_title: "Blocked task" },
          { source_task_id: TASK_ID, target_task_id: "t-linked", kind: "related_to", target_title: "Linked task" },
        ],
        TASK_ID,
      ),
    });
    const element = mountElement(root, "flow-task-relations", model);
    await flush();

    const blocksButton = element.querySelector('[data-relation-remove="t-me"][data-kind="blocks"]');
    assert.ok(blocksButton, "the blocks row carries a remove control");
    const app = { setStatus() {}, async refresh() {} };

    const first = handleAction(app, { target: blocksButton, preventDefault() {} });
    assert.equal(blocksButton.disabled, true, "the first removal marks its control busy");
    assert.equal(fetchCalls.length, 1);

    // A poll repaint swaps in fresh nodes mid-flight; the busy state comes
    // from the registry, keyed by the full mutation identity.
    element.invalidate();
    await flush();
    const duplicate = element.querySelector('[data-relation-remove="t-me"][data-kind="blocks"]');
    const related = element.querySelector('[data-relation-remove="t-me"][data-kind="related_to"]');
    assert.ok(duplicate && related, "the repaint re-rendered both rows");
    assert.equal(duplicate.disabled, true, "the exact duplicate stays busy through the repaint");
    assert.ok(!related.disabled, "the distinct row is not conflated with the in-flight removal");

    // The exact duplicate (same project, source, target, kind) stays blocked.
    await handleAction(app, { target: duplicate, preventDefault() {} });
    assert.equal(fetchCalls.length, 1, "the exact duplicate issues no second request");

    // A distinct row on the same source is a different mutation and proceeds.
    const second = handleAction(app, { target: related, preventDefault() {} });
    assert.equal(related.disabled, true, "the second removal marks its own control busy");
    assert.equal(fetchCalls.length, 2, "the distinct removal is not suppressed by the shared source");
    assert.deepEqual(
      fetchCalls.map((call) => JSON.parse(call.options.body)),
      [
        { target_task_id: "t-blocked", kind: "blocks" },
        { target_task_id: "t-linked", kind: "related_to" },
      ],
    );

    for (const resolve of resolvers) resolve();
    assert.equal(await first, true);
    assert.equal(await second, true);
    assert.equal(duplicate.disabled, false, "settling restores the control on screen");
    assert.equal(related.disabled, false);
    assert.equal(inFlight.size, 0);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});
