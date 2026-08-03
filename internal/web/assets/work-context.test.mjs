import assert from "node:assert/strict";
import test from "node:test";
import { installTestDOM } from "./test-dom.mjs";

installTestDOM();
const { FORMS } = await import("./forms.js");
const { projectTaskHref, taskHref, workItemHref } = await import("./api.js");
const { renderFeature } = await import("./elements/feature.js");
const { renderTaskRail } = await import("./elements/task-rail.js");
const { taskModel } = await import("./task-model.js");
const { renderTaskRelations } = await import("./elements/task-relations.js");
const { loadWorkItemContext, validParentCandidates } = await import("./work-item-detail.js");

function item(id, kind, parent = "", terminal = false) {
  return { id, kind, title: id, parent_item_id: parent, state: { status: terminal ? "done" : "open", terminal }, capabilities: { can_contain: kind !== "task" } };
}

test("nested task creation sends canonical parent_item_id and never feature_id", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url: String(url), body: JSON.parse(options.body) };
    return new Response(JSON.stringify({ task: { id: "t-new" }, project_id: "p-one" }), { status: 200, headers: { "content-type": "application/json" } });
  };
  globalThis.history = { pushState() {} };
  const app = { workItemsByProject: new Map(), async load() {} };
  const form = {
    dataset: { taskFormMode: "create" },
    elements: {
      title: { value: "Nested task" }, body: { value: "" }, priority: { value: "1" }, flow_id: { value: "flow" },
      project: { value: "p-one" }, parent_item_id: { value: "epic-child" },
      // A stale legacy control must not be able to flatten containment.
      feature_id: { value: "feature-root" }, attachments: { files: [] }, queue_task: { checked: false },
    },
    querySelectorAll() { return []; },
  };
  await FORMS.taskForm(app, form);
  assert.equal(request.url.endsWith("/v2/projects/p-one/tasks"), true);
  assert.equal(request.body.parent_item_id, "epic-child");
  assert.equal(Object.hasOwn(request.body, "feature_id"), false);
});

test("feature Used by links retain project, context, and a predictable return without changing legacy taskHref", () => {
  assert.equal(taskHref("p-one", "t-1"), "/ui/tasks/t-1");
  assert.equal(projectTaskHref("p-one", "t-1"), "/ui/projects/p-one/tasks/t-1");
  const html = renderFeature({
    projectID: "p-one",
    currentHref: "/ui/projects/p-one/features/f-1?context=e-1&return=%2Fui%2Fprojects%2Fp-one%2Fwork-items",
    feature: { id: "f-1", title: "Feature", status: "open" },
    tasks: [{ id: "t-1", title: "Task" }],
  });
  assert.match(html, /<h3>Used by<\/h3>/);
  assert.match(html, /href="\/ui\/projects\/p-one\/tasks\/t-1\?context=f-1&amp;return=%2Fui%2Fprojects%2Fp-one%2Ffeatures%2Ff-1%3Fcontext%3De-1%26return%3D%252Fui%252Fprojects%252Fp-one%252Fwork-items"/);
  assert.doesNotMatch(html, /href="\/ui\/tasks\/t-1/);
});

test("task detail labels canonical direct parents separately from feature-within-epic execution context", () => {
  const directEpic = item("e-direct", "epic");
  directEpic.title = "Direct epic";
  const contextEpic = item("e-context", "epic");
  contextEpic.title = "Context epic";
  const feature = item("f-context", "feature", "e-context");
  feature.title = "Context feature";
  feature.effective_feature_id = "f-context";
  const task = item("t-1", "task", "e-direct");
  task.title = "Task";
  task.effective_feature_id = "f-context";
  const model = taskModel({
    task,
    project_id: "p-one",
    work_item: { item: task, ancestors: [directEpic] },
    work_items: [directEpic, contextEpic, feature, task],
    navigation: { context: "f-context", returnTo: "/ui/projects/p-one/features/f-context" },
  }, null);
  const html = renderTaskRail(model);
  const direct = html.match(/<nav class="work-breadcrumb"[\s\S]*?<\/nav>/)?.[0] || "";
  const effective = html.match(/<nav class="work-effective-feature"[\s\S]*?<\/nav>/)?.[0] || "";
  assert.match(direct, /Direct parent/);
  assert.match(direct, /Direct epic/);
  assert.doesNotMatch(direct, /Context feature|Context epic/);
  assert.match(effective, /Effective feature/);
  assert.match(effective, /Context epic[\s\S]*Context feature/);
  assert.doesNotMatch(effective, />Task</);
  assert.match(html, /href="\/ui\/projects\/p-one\/features\/f-context" data-link>Back to Work context<\/a>/);
  assert.match(html, /context=f-context&amp;return=%2Fui%2Fprojects%2Fp-one%2Ffeatures%2Ff-context/);
});

test("missing effective summaries and unrelated requested contexts fall back to project Work", () => {
  const task = { ...item("t-1", "task"), effective_feature_id: "f-stale" };
  for (const context of ["f-stale", "missing"]) {
    const model = taskModel({
      task,
      project_id: "p-one",
      work_item: { item: task },
      work_items: [task],
      navigation: { context, returnTo: "/ui/projects/p-one/features/f-stale" },
    }, null);
    assert.equal(model.navigation.contextValid, false);
    assert.equal(model.navigation.context, "");
    assert.equal(model.navigation.returnTo, "/ui/projects/p-one/work-items");
  }
});

test("href navigation ignores object-valued state instead of emitting object Object", () => {
  const href = workItemHref("p-one", item("t-1", "task"), { context: {}, returnTo: {} });
  assert.equal(href, "/ui/projects/p-one/tasks/t-1");
  assert.doesNotMatch(href, /object/i);
});

test("move candidates exclude the item, descendants, tasks, and closed containers", () => {
  const candidates = validParentCandidates([
    item("e-root", "epic"), item("e-moving", "epic", "e-root"), item("f-child", "feature", "e-moving"),
    item("t-leaf", "task", "e-root"), item("e-closed", "epic", "", true), item("f-valid", "feature"),
  ], "e-moving").map((entry) => entry.id);
  assert.deepEqual(candidates, ["e-root", "f-valid"]);
});

test("bounded detail context uses the project list cache for unrelated Move containers", async () => {
  const originalFetch = globalThis.fetch;
  const moving = item("e-moving", "epic", "e-root");
  const child = item("f-child", "feature", "e-moving");
  const unrelatedEpic = item("e-unrelated", "epic");
  const unrelatedFeature = item("f-unrelated", "feature", "e-unrelated");
  const closed = item("e-closed", "epic", "", true);
  const projectItems = [item("e-root", "epic"), moving, child, unrelatedEpic, unrelatedFeature, closed, item("t-loose", "task")];
  const ensured = [];
  globalThis.fetch = async (path) => {
    assert.equal(path, "/ui/api/v2/projects/p-one/work-items/e-moving/context");
    return new Response(JSON.stringify({ item: moving, ancestors: [projectItems[0]], children: [child] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };
  try {
    const planning = await loadWorkItemContext({
      ensureWorkItems: async (projectID) => {
        ensured.push(projectID);
        return projectItems;
      },
    }, "p-one", "e-moving");
    assert.equal(planning.bounded, true);
    assert.deepEqual(ensured, ["p-one"]);
    assert.deepEqual(planning.contextItems.map((entry) => entry.id), ["e-root", "f-child"], "safe navigation remains bounded");
    assert.deepEqual(validParentCandidates(planning.workItems, "e-moving").map((entry) => entry.id), [
      "e-root", "e-unrelated", "f-unrelated",
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("generic relation rendering separates dependencies and uses cross-kind endpoint controls", () => {
  const html = renderTaskRelations({
    id: "t-1", projectID: "p-one", genericRelations: true,
    relations: [{ source: item("e-blocker", "epic"), target: item("t-1", "task"), kind: "blocks", resolved: false }],
  });
  assert.match(html, /Dependencies/);
  assert.match(html, /e-blocker/);
  assert.match(html, /data-work-item-relation-remove="t-1"/);
  assert.match(html, /data-work-item-relation-add-form="t-1"/);
  assert.doesNotMatch(html, /value="parent_of"/);
});
