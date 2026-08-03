import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

const root = installTestDOM();

const {
  collectCreateWorkItemRelations,
  relationPickerRowView,
  workItemRelationSuggestionsView,
} = await import("./create-relations.js");
const { handleFormSubmit } = await import("./forms.js");
const { renderTaskFormView } = await import("./task-view.js");
const { renderFeature } = await import("./elements/feature.js");
const { renderEpic } = await import("./elements/epic.js");
await import("./elements/features.js");
await import("./elements/work-items.js");

const summaries = [
  { id: "t-one", kind: "task", title: "Task one", state: { status: "open" } },
  { id: "e-one", kind: "epic", title: "Epic one", state: { status: "open" }, capabilities: { can_contain: true } },
  { id: "f-one", kind: "feature", title: "Feature one", state: { status: "open" }, capabilities: { can_contain: true } },
];

function fakeRow(kind, target) {
  return {
    querySelector(selector) {
      if (selector === "[data-relation-kind]") return { value: kind };
      if (selector === "[data-relation-target]") return { value: target };
      return null;
    },
  };
}

function fakeForm(rows) {
  return { querySelectorAll: () => rows };
}

function relationRow(form, index = 0) {
  return form.querySelectorAll("[data-relation-row]")[index];
}

function setRelation(form, kind, target, index = 0) {
  const row = relationRow(form, index);
  row.querySelector("[data-relation-kind]").value = kind;
  row.querySelector("[data-relation-target]").value = target;
  return row;
}

function testApp() {
  const app = document.createElement("flow-app");
  app.statuses = [];
  app.refreshCalls = 0;
  app.workItemsByProject = new Map([["p-alpha", summaries]]);
  app.featuresByProject = new Map([["p-alpha", []]]);
  app.setStatus = (message) => app.statuses.push(message);
  app.refresh = async () => { app.refreshCalls += 1; };
  app.load = async () => { app.refreshCalls += 1; };
  root.appendChild(app);
  return app;
}

function workItemsData(extra = {}) {
  return {
    projectID: "p-alpha",
    projects: [{ id: "p-alpha", name: "Alpha" }],
    preferences: { view: "overview", filter: "all", completedCollapsed: true, collapsed: new Set() },
    items: summaries,
    ...extra,
  };
}

function epicCreateForm(element) {
  return element.querySelector(".work-create").querySelector('[data-epic-form=""]');
}

function featureData(extra = {}) {
  return {
    projectID: "p-alpha",
    projects: [{ id: "p-alpha", name: "Alpha" }],
    features: [],
    workItems: summaries,
    ...extra,
  };
}

test("relation kind and target controls have explicit row-specific accessible labels", () => {
  const first = relationPickerRowView();
  const later = relationPickerRowView(4);
  assert.match(first, /data-relation-kind aria-label="Relation 1 kind"/);
  assert.match(first, /data-relation-target aria-label="Relation 1 target work item"/);
  assert.match(later, /data-relation-kind aria-label="Relation 4 kind"/);
  assert.match(later, /data-relation-target aria-label="Relation 4 target work item"/);
});

test("generic relation suggestions label task, epic, and feature summaries", () => {
  const html = workItemRelationSuggestionsView(summaries);
  assert.match(html, /value="t-one" label="task · Task one"/);
  assert.match(html, /value="e-one" label="epic · Epic one"/);
  assert.match(html, /value="f-one" label="feature · Feature one"/);
});

test("generic create relations orient exactly one new endpoint and validate duplicate parents and rows", () => {
  assert.deepEqual(collectCreateWorkItemRelations(fakeForm([
    fakeRow("blocks", "e-one"),
    fakeRow("related_to", "f-one"),
    fakeRow("parent_of", "e-parent"),
  ])), [
    { target_item_id: "e-one", source_is_new_item: true, kind: "blocks" },
    { target_item_id: "f-one", source_is_new_item: true, kind: "related_to" },
    { source_item_id: "e-parent", target_is_new_item: true, kind: "parent_of" },
  ]);

  assert.match(collectCreateWorkItemRelations(fakeForm([
    fakeRow("blocks", "e-one"),
    fakeRow("blocks", "e-one"),
  ])).message, /Duplicate relation/);
  assert.match(collectCreateWorkItemRelations(fakeForm([
    fakeRow("parent_of", "e-one"),
    fakeRow("parent_of", "f-one"),
  ])).message, /only one parent/);
  assert.match(collectCreateWorkItemRelations(fakeForm([
    fakeRow("parent_of", "e-one"),
  ]), "f-one").message, /either in Parent or Relations/);
});

test("feature create sends generic relations in its sole POST and reconciles once", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (path, options) => {
    calls.push({ path, options });
    return { ok: true, status: 201, json: async () => ({ feature: { id: "f-new" } }) };
  };
  const app = testApp();
  try {
    const element = mountElement(app, "flow-features", featureData());
    await flush();
    const form = element.querySelector('[data-feature-form=""]');
    form.elements.title.value = "New feature";
    form.elements.body.value = "Body";
    setRelation(form, "blocks", "t-one");

    await handleFormSubmit(app, { target: form, preventDefault() {} });

    assert.equal(calls.length, 1, "no post-create relation call");
    assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/features");
    assert.deepEqual(JSON.parse(calls[0].options.body).work_item_relations, [
      { target_item_id: "t-one", source_is_new_item: true, kind: "blocks" },
    ]);
    assert.equal(app.refreshCalls, 1);
  } finally {
    app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("epic create sends child-of orientation in its sole POST and reconciles once", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (path, options) => {
    calls.push({ path, options });
    return { ok: true, status: 201, json: async () => ({ epic: { id: "e-new" } }) };
  };
  const app = testApp();
  try {
    const element = mountElement(app, "flow-work-items", workItemsData());
    await flush();
    const form = epicCreateForm(element);
    form.elements.title.value = "New epic";
    setRelation(form, "parent_of", "e-one");

    await handleFormSubmit(app, { target: form, preventDefault() {} });

    assert.equal(calls.length, 1, "no post-create relation call");
    assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/epics");
    assert.deepEqual(JSON.parse(calls[0].options.body).work_item_relations, [
      { source_item_id: "e-one", target_is_new_item: true, kind: "parent_of" },
    ]);
    assert.equal(app.refreshCalls, 1);
  } finally {
    app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("feature and epic create reject canonical-parent duplication before requesting", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (...args) => { calls.push(args); throw new Error("must not fetch"); };
  const app = testApp();
  try {
    const features = mountElement(app, "flow-features", featureData());
    const work = mountElement(app, "flow-work-items", workItemsData());
    await flush();
    for (const form of [features.querySelector('[data-feature-form=""]'), epicCreateForm(work)]) {
      form.elements.title.value = "Duplicate parent";
      form.elements.parent_item_id.value = "f-one";
      setRelation(form, "parent_of", "e-one");
      await handleFormSubmit(app, { target: form, preventDefault() {} });
    }
    assert.equal(calls.length, 0);
    assert.match(app.statuses.at(-1), /either in Parent or Relations/);
  } finally {
    app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("feature and epic create preserve authored rows when the atomic POST fails", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (path, options) => {
    calls.push({ path, options });
    return {
      ok: false,
      status: 422,
      json: async () => ({ error: { message: "relation rejected" } }),
      text: async () => "",
    };
  };
  const app = testApp();
  try {
    const features = mountElement(app, "flow-features", featureData());
    const work = mountElement(app, "flow-work-items", workItemsData());
    await flush();
    for (const form of [features.querySelector('[data-feature-form=""]'), epicCreateForm(work)]) {
      form.elements.title.value = "Retry me";
      const row = setRelation(form, "related_to", "t-one");
      await handleFormSubmit(app, { target: form, preventDefault() {} });
      assert.equal(relationRow(form), row);
      assert.equal(row.querySelector("[data-relation-target]").value, "t-one");
    }
    assert.equal(calls.length, 2, "one failed create request per form and no follow-up");
    assert.equal(app.refreshCalls, 0);
    assert.match(app.statuses.at(-1), /relation rejected/);
  } finally {
    app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("repainting feature and work-item elements keep one delegated picker handler", async () => {
  const app = testApp();
  try {
    const features = mountElement(app, "flow-features", featureData());
    const work = mountElement(app, "flow-work-items", workItemsData());
    await flush();
    for (const [element, next] of [
      [features, featureData({ search: "?changed=1" })],
      [work, workItemsData({ search: "?changed=1" })],
    ]) {
      element.querySelector("[data-relation-add]").click();
      assert.equal(element.querySelectorAll("[data-relation-row]").length, 2);
      assert.equal(element.querySelectorAll("[data-relation-kind]").at(-1).getAttribute("aria-label"), "Relation 2 kind");
      assert.equal(element.querySelectorAll("[data-relation-target]").at(-1).getAttribute("aria-label"), "Relation 2 target work item");
      element.data = next;
      await flush();
      element.querySelector("[data-relation-add]").click();
      assert.equal(element.querySelectorAll("[data-relation-row]").length, 2, "one click appends exactly one row after repaint");
      element.querySelector("[data-relation-remove]").click();
      assert.equal(element.querySelectorAll("[data-relation-row]").length, 1);
    }
  } finally {
    app.remove();
  }
});

test("generic picker remains create-only for task, feature, and epic forms", () => {
  const app = {
    projects: [{ id: "p-alpha", name: "Alpha" }],
    selectedProjectIDs: () => ["p-alpha"],
    workItemsByProject: new Map([["p-alpha", summaries]]),
    flowsByProject: new Map(),
  };
  assert.match(renderTaskFormView(app, {}, { mode: "create", projectID: "p-alpha" }), /data-relation-picker/);
  assert.doesNotMatch(renderTaskFormView(app, { title: "Task" }, { mode: "edit", projectID: "p-alpha", taskID: "t-one" }), /data-relation-picker/);
  assert.doesNotMatch(renderFeature({ item: { id: "f-one" }, feature: { id: "f-one", title: "Feature" }, projectID: "p-alpha" }), /data-relation-picker/);
  assert.doesNotMatch(renderEpic({ item: { id: "e-one" }, epic: { id: "e-one", title: "Epic" }, projectID: "p-alpha" }), /data-relation-picker/);
});
