import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement, TestEvent } from "./test-dom.mjs";

const root = installTestDOM();

const { moveDestinationCandidates, renderTaskProgress, renderWorkItems } = await import("./elements/work-items.js");

const data = {
  projectID: "p-alpha",
  projects: [{ id: "p-alpha", name: "Alpha" }, { id: "p-beta", name: "Beta" }],
  preferences: { view: "overview", filter: "all", completedCollapsed: true, collapsed: new Set() },
  items: [
    { id: "e-open", kind: "epic", title: "Open epic", priority: 2, state: { status: "open" } },
    { id: "f-pay", kind: "feature", title: "Payments", parent_item_id: "e-open", effective_feature_id: "f-pay", state: { status: "open" } },
    { id: "t-done", kind: "task", title: "Done child", parent_item_id: "f-pay", effective_feature_id: "f-pay", state: { status: "done", terminal: true, successful: true } },
    { id: "t-blocked", kind: "task", title: "Blocked child", parent_item_id: "e-open", state: { status: "scheduled" }, unresolved_blockers: 2 },
    { id: "t-solo", kind: "task", title: "Standalone", state: { status: "unscheduled" } },
    { id: "e-done", kind: "epic", title: "Completed epic", state: { status: "done", terminal: true, successful: true } },
  ],
};

test("container-first overview renders progress, direct status, blockers, branch context, and subnav", () => {
  const html = renderWorkItems(data);
  assert.ok(html.indexOf("Open epic") < html.indexOf("Standalone"));
  assert.match(html, /aria-label="1 of 2 descendant tasks complete, 0 in progress, 1 scheduled, 0 unscheduled, 1 blocked task"/);
  assert.match(html, /0\/2 direct children closed · 1 ready · 1 blocked/);
  assert.match(html, /2 effective blockers/);
  assert.match(html, /On branch Payments/);
  assert.match(html, /aria-label="Work views"/);
  assert.match(html, /Overview<\/a>/);
  assert.doesNotMatch(html, /role="tree"/);
  assert.doesNotMatch(html, /class="work-children" role="group"/);
  assert.match(html, /\/ui\/projects\/p-alpha\/features/);
});

test("overview descendant task links preserve their nearest container and exact return", () => {
  const html = renderWorkItems({
    ...data,
    currentHref: "/ui/projects/p-alpha/work-items?view=tree&q=pay#results",
    preferences: { ...data.preferences, view: "tree" },
    view: "tree",
  });
  assert.match(html, /href="\/ui\/projects\/p-alpha\/tasks\/t-done\?context=f-pay&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Fwork-items%3Fview%3Dtree%26q%3Dpay%23results"/);
  assert.match(html, /href="\/ui\/projects\/p-alpha\/tasks\/t-blocked\?context=e-open&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Fwork-items%3Fview%3Dtree%26q%3Dpay%23results"/);
});

test("completed roots are collapsed by default and open for completed filters", () => {
  const collapsed = renderWorkItems(data);
  assert.match(collapsed, /Completed · 1/);
  assert.doesNotMatch(collapsed, />Completed epic<\/a>/);
  const completed = renderWorkItems({ ...data, filter: "completed" });
  assert.match(completed, />Completed epic<\/a>/);
  assert.doesNotMatch(completed, />Standalone<\/a>/);
});

test("search keeps matching descendants in hierarchy context and escapes content", () => {
  const html = renderWorkItems({ ...data, query: "blocked child", items: [...data.items, { id: "bad", kind: "task", title: "<script>" }] });
  assert.match(html, /Open epic/);
  assert.match(html, /Blocked child/);
  assert.doesNotMatch(html, />Done child<\/a>/);
  assert.doesNotMatch(html, /<script>/);
});

test("orphan and cycle rows render once with safe hierarchy warnings", () => {
  const html = renderWorkItems({ ...data, items: [
    { id: "orphan", kind: "task", title: "Orphan", parent_item_id: "missing", state: { status: "open" } },
    { id: "a", kind: "epic", title: "Cycle A", parent_item_id: "b", state: { status: "open" } },
    { id: "b", kind: "feature", title: "Cycle B", parent_item_id: "a", state: { status: "open" } },
  ] });
  assert.equal((html.match(/>Orphan<\/a>/g) || []).length, 1);
  assert.equal((html.match(/>Cycle A<\/a>/g) || []).length, 1);
  assert.match(html, /parent unavailable/);
  assert.match(html, /cycle detached/);
});

test("segmented progress always has a textual accessible equivalent", () => {
  const html = renderTaskProgress({ total: 3, closed: 1, successful: 1, unsuccessful: 0, inProgress: 1, scheduled: 0, unscheduled: 1, otherOpen: 0, blocked: 0 });
  assert.match(html, /role="img"/);
  assert.match(html, /1\/3 descendant tasks complete · 1 successful · 0 unsuccessful · 1 in progress · 0 scheduled · 1 unscheduled/);
  assert.match(html, /data-progress="successful"/);
  assert.match(html, /data-progress="working"/);
});

function interactiveWorkItems(overrides = {}) {
  const app = document.createElement("flow-app");
  app.workItemsByProject = new Map([["p-alpha", data.items]]);
  // The element evicts through the app's cache port; back it with the same
  // Map the assertions read.
  app.caches = { invalidate: (kind, id) => app.workItemsByProject.delete(id) };
  app.refreshCalls = 0;
  app.refresh = async () => { app.refreshCalls += 1; };
  root.appendChild(app);
  const element = mountElement(app, "flow-work-items", {
    ...data,
    ...overrides,
    preferences: {
      ...data.preferences,
      ...(overrides.preferences || {}),
    },
  });
  return { app, element };
}

test("list and tree instances isolate selection while each preserves it across repaint and mode toggle", async () => {
  const list = interactiveWorkItems();
  const tree = interactiveWorkItems({ preferences: { view: "tree" }, view: "tree" });
  await flush();

  list.element.querySelector('[data-work-select="t-solo"]').click();
  await flush();
  assert.ok(list.element.querySelector('[data-work-select="t-solo"]').hasAttribute("checked"));
  assert.ok(!list.element.querySelector("[data-work-move-selected]").hasAttribute("disabled"));
  assert.ok(!tree.element.querySelector('[data-work-select="t-solo"]').hasAttribute("checked"));
  assert.ok(tree.element.querySelector("[data-work-move-selected]").hasAttribute("disabled"));

  list.element.data = { ...data, preferences: { ...data.preferences } };
  await flush();
  assert.ok(list.element.querySelector('[data-work-select="t-solo"]').hasAttribute("checked"), "poll repaint preserves selection");
  list.element.querySelector('[data-work-view-mode="tree"]').click();
  await flush();
  assert.ok(list.element.querySelector('[data-work-select="t-solo"]').hasAttribute("checked"), "mode toggle preserves selection");

  list.element.data = { ...data, items: data.items.filter((item) => item.id !== "t-solo"), preferences: { ...data.preferences, view: "tree" } };
  await flush();
  assert.ok(list.element.querySelector("[data-work-move-selected]").hasAttribute("disabled"), "items absent from the current model are pruned");

  list.app.remove();
  tree.app.remove();
});

test("selection repaint and both dialog close paths restore deterministic focus", async () => {
  const mounted = interactiveWorkItems();
  try {
    await flush();
    const originalSelection = mounted.element.querySelector('[data-work-select="t-solo"]');
    originalSelection.focus();
    originalSelection.click();
    await flush();

    const repaintedSelection = mounted.element.querySelector('[data-work-select="t-solo"]');
    assert.notEqual(repaintedSelection, originalSelection);
    assert.equal(document.activeElement, repaintedSelection, "checkbox repaint restores the same selection control");

    const moveButton = mounted.element.querySelector("[data-work-move-selected]");
    moveButton.focus();
    moveButton.click();
    await flush();
    const destination = mounted.element.querySelector("[data-work-move-destination]");
    assert.equal(document.activeElement, destination, "opening Move selected focuses its destination control");

    mounted.element.querySelector("[data-work-move-cancel]").click();
    await flush();
    assert.equal(document.activeElement, mounted.element.querySelector("[data-work-move-selected]"), "Cancel restores the Move selected trigger");

    mounted.element.querySelector("[data-work-move-selected]").click();
    await flush();
    const dialog = mounted.element.querySelector(".work-move-dialog");
    dialog.dispatchEvent(new TestEvent("cancel", { bubbles: true }));
    await flush();
    assert.equal(document.activeElement, mounted.element.querySelector("[data-work-move-selected]"), "Escape cancellation restores the Move selected trigger");
  } finally {
    mounted.app.remove();
  }
});

test("bulk move sends one ordered PATCH and reconciles exactly once", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (path, options) => {
    calls.push({ path, options });
    return { ok: true, status: 200, json: async () => ({ items: [] }) };
  };
  const mounted = interactiveWorkItems({ items: [...data.items, { id: "e-target", kind: "epic", title: "Target", state: { status: "open" } }] });
  try {
    await flush();
    mounted.element.querySelector('[data-work-select="t-solo"]').click();
    await flush();
    mounted.element.querySelector('[data-work-select="t-blocked"]').click();
    await flush();
    mounted.element.querySelector("[data-work-move-selected]").click();
    await flush();

    const dialog = mounted.element.querySelector(".work-move-dialog");
    assert.ok(dialog?.hasAttribute("open"));
    assert.equal(dialog.querySelector("p").textContent, "2 work items selected");
    const destination = dialog.querySelector("[data-work-move-destination]");
    destination.value = "e-target";
    await mounted.element.submitMove();
    await flush();

    assert.equal(calls.length, 1);
    assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/work-items/parents");
    assert.equal(calls[0].options.method, "PATCH");
    assert.deepEqual(JSON.parse(calls[0].options.body), {
      item_ids: ["t-solo", "t-blocked"],
      parent_item_id: "e-target",
    });
    assert.equal(mounted.app.refreshCalls, 1);
    assert.equal(mounted.app.workItemsByProject.has("p-alpha"), false);
    assert.equal(mounted.element.querySelector(".work-move-dialog"), null);
    assert.ok(mounted.element.querySelector("[data-work-move-selected]").hasAttribute("disabled"));
  } finally {
    mounted.app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("structured bulk validation keeps selection and destination for one-request retry", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (path, options) => {
    calls.push({ path, options });
    if (calls.length === 1) {
      return {
        ok: false,
        status: 422,
        json: async () => ({
          error: {
            code: "invalid_work_item_move",
            message: "Move validation failed",
            issues: [{ item_id: "t-solo", code: "dependency_cycle", message: "Moving this item would create a cycle" }],
          },
        }),
      };
    }
    return { ok: true, status: 200, json: async () => ({ items: [] }) };
  };
  const mounted = interactiveWorkItems();
  try {
    await flush();
    mounted.element.querySelector('[data-work-select="t-solo"]').click();
    await flush();
    mounted.element.querySelector("[data-work-move-selected]").click();
    await flush();
    mounted.element.querySelector("[data-work-move-destination]").value = "e-open";

    await mounted.element.submitMove();
    await flush();

    assert.equal(calls.length, 1, "validation failure is still one bulk request");
    assert.equal(mounted.app.refreshCalls, 0);
    assert.ok(mounted.element.querySelector('[data-work-select="t-solo"]').hasAttribute("checked"));
    assert.ok(mounted.element.querySelector(".work-move-dialog").hasAttribute("open"));
    const selectedOption = mounted.element.querySelectorAll('option[value="e-open"]').find((option) => option.hasAttribute("selected"));
    assert.ok(selectedOption);
    const issue = mounted.element.querySelector('[data-work-move-issue][data-item-id="t-solo"][data-code="dependency_cycle"]');
    assert.ok(issue);
    assert.equal(issue.querySelector("code").textContent, "dependency_cycle");
    assert.equal(issue.querySelectorAll("span").at(-1).textContent, "Moving this item would create a cycle");

    mounted.element.querySelector("[data-work-move-destination]").value = "e-open";
    await mounted.element.submitMove();
    await flush();
    assert.equal(calls.length, 2);
    assert.deepEqual(JSON.parse(calls[1].options.body).item_ids, ["t-solo"]);
    assert.equal(mounted.app.refreshCalls, 1);
    assert.ok(mounted.element.querySelector("[data-work-move-selected]").hasAttribute("disabled"));
  } finally {
    mounted.app.remove();
    globalThis.fetch = originalFetch;
  }
});

test("move destinations are open containers outside selected subtrees", () => {
  const items = [...data.items, { id: "e-target", kind: "epic", title: "Target", state: { status: "open" } }];
  assert.deepEqual(moveDestinationCandidates(items, new Set(["e-open"])).map((item) => item.id), ["e-target"]);
});
