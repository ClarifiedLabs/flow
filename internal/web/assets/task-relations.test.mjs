// Tests for task relations: the pure grouping in task-model.js, the rendered
// markup of the relations element, the unresolved-blocker flag, the empty
// state, and the add/remove controls that hit the relations endpoint.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

installTestDOM();

const { relationGroups, RELATION_GROUPS, taskModel } = await import("./task-model.js");
const { renderTaskRelations, RELATION_KIND_OPTIONS } = await import("./elements/task-relations.js");
const { handleFormSubmit } = await import("./forms.js");
const { handleAction } = await import("./actions.js");

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
    { source_task_id: "t-blocker", target_task_id: TASK_ID, kind: "blocks", source_title: "Blocking task", target_title: "My task" },
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
  const model = relationsModel();
  // Before the blocker's state is known there is no flag.
  assert.doesNotMatch(renderTaskRelations(model), /is-unresolved/);

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

// --- unresolved-blocker resolution (element) ---------------------------------

function taskResponse(state) {
  return { ok: true, status: 200, json: () => Promise.resolve({ task: { state } }) };
}

// settle flushes enough microtasks for the element's async blocker-state fetch
// chain (apiGet -> apiFetch -> json -> applyBlockerStates -> repaint) to run to
// completion. Over-flushing is harmless once the queue is drained.
async function settle(times = 16) {
  for (let i = 0; i < times; i++) await flush();
}

test("the element fetches each blocker's state and flags the unfinished ones", async () => {
  const calls = [];
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    calls.push(path);
    if (path.endsWith("/tasks/t-blocker")) return Promise.resolve(taskResponse("in_progress"));
    return Promise.resolve(taskResponse("done"));
  };
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await settle();

    assert.ok(calls.some((path) => path.endsWith("/tasks/t-blocker")), "blocker state is fetched");
    assert.match(element.innerHTML, /is-unresolved/, "an unfinished blocker is flagged");
    assert.equal(element.data.relationGroups.blockedBy[0].unresolved, true);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("a done blocker is not flagged", async () => {
  const previousFetch = globalThis.fetch;
  globalThis.fetch = () => Promise.resolve(taskResponse("done"));
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await settle();

    assert.doesNotMatch(element.innerHTML, /is-unresolved/);
    assert.equal(element.data.relationGroups.blockedBy[0].unresolved, false);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("a blocker that finishes is unflagged on the next refresh of the task", async () => {
  // Regression: blockerStates cached each blocker's state forever, so a
  // blocker observed as in_progress stayed flagged after it reached done.
  let blockerState = "in_progress";
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    if (path.endsWith("/tasks/t-blocker")) return Promise.resolve(taskResponse(blockerState));
    return Promise.resolve(taskResponse("done"));
  };
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await settle();
    assert.match(element.innerHTML, /is-unresolved/, "an in-progress blocker starts flagged");

    // The blocker finishes, and the task detail refreshes with a fresh model —
    // the same element instance, as the rail reuses it.
    blockerState = "done";
    element.data = relationsModel();
    await settle();

    assert.doesNotMatch(element.innerHTML, /is-unresolved/, "a finished blocker is no longer flagged");
    assert.equal(element.data.relationGroups.blockedBy[0].unresolved, false);
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("a blocker that reopens is flagged again on the next refresh", async () => {
  let blockerState = "done";
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    if (path.endsWith("/tasks/t-blocker")) return Promise.resolve(taskResponse(blockerState));
    return Promise.resolve(taskResponse("done"));
  };
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await settle();
    assert.doesNotMatch(element.innerHTML, /is-unresolved/);

    blockerState = "in_progress";
    element.data = relationsModel();
    await settle();

    assert.match(element.innerHTML, /is-unresolved/, "a reopened blocker is flagged again");
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the last known blocker state stays applied while the re-check is in flight", async () => {
  // The cache is a flicker guard, not a truth source: while the re-check has
  // not answered, the previous observation is what gets painted.
  let answer = "in_progress";
  let resolveBlocker;
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    if (path.endsWith("/tasks/t-blocker")) {
      return new Promise((resolve) => {
        resolveBlocker = () => resolve(taskResponse(answer));
      });
    }
    return Promise.resolve(taskResponse("done"));
  };
  try {
    const root = globalThis.document.body;
    const element = mountElement(root, "flow-task-relations", relationsModel());
    await settle();
    assert.equal(typeof resolveBlocker, "function", "the first re-check is in flight");
    resolveBlocker();
    await settle();
    assert.match(element.innerHTML, /is-unresolved/, "the first observation is painted");

    // A refresh arrives while the next re-check is still pending: the flag
    // holds rather than blinking off.
    element.data = relationsModel();
    await settle();
    assert.match(element.innerHTML, /is-unresolved/, "the flag holds during the re-check");

    answer = "done";
    resolveBlocker();
    await settle();
    assert.doesNotMatch(element.innerHTML, /is-unresolved/, "the answered re-check clears the flag");
    element.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
});

test("the rail forwards fresh data to the relations element even when its own markup is unchanged", async () => {
  // Regression: the rail only forwarded data in afterPaint, which the base
  // paint skips when the markup is stable — so a refresh that changed only a
  // blocker's state never reached the relations element.
  const { renderTaskRail } = await import("./elements/task-rail.js");
  await import("./elements/run-spine.js");

  let blockerState = "in_progress";
  const previousFetch = globalThis.fetch;
  globalThis.fetch = (path) => {
    if (path.endsWith("/tasks/t-blocker")) return Promise.resolve(taskResponse(blockerState));
    return Promise.resolve(taskResponse("done"));
  };
  try {
    const root = globalThis.document.body;
    const rail = mountElement(root, "flow-task-rail", relationsModel());
    await settle();
    const relations = rail.querySelector("flow-task-relations");
    assert.ok(relations, "the rail mounts the relations element");
    assert.match(relations.innerHTML, /is-unresolved/);

    blockerState = "done";
    rail.data = relationsModel();
    await settle();

    assert.equal(rail.innerHTML, renderTaskRail(relationsModel()), "the rail markup itself is unchanged");
    assert.doesNotMatch(relations.innerHTML, /is-unresolved/, "the relations element saw the fresh model");
    rail.remove();
  } finally {
    globalThis.fetch = previousFetch;
  }
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
