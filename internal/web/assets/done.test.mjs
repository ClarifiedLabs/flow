// <flow-done> tests: the query builder, the controls/rows renderers, and the
// element's page accumulation (first page replaces, Load more appends).

import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

const root = installTestDOM();
const { doneQuery, renderDoneControls, renderDoneRow } = await import("./elements/done.js");

function page(tasks, { cursors = true } = {}) {
  const outcomes = {};
  const cards = {};
  for (const task of tasks) {
    outcomes[task.id] = "done:completed";
    cards[task.id] = {};
  }
  return {
    done: [{
      project_id: "p-alpha",
      project_name: "Alpha",
      tasks,
      outcomes,
      task_cards: cards,
      ...(cursors ? { next_before: "2026-06-01T00:00:00Z", next_before_id: tasks[tasks.length - 1]?.id || "" } : {}),
    }],
  };
}

test("doneQuery combines the project selection with the outcome filter and cursor extras", () => {
  assert.equal(doneQuery(["p-a", "p-b"], "merged"), "?project=p-a&project=p-b&outcome=merged");
  assert.equal(doneQuery([], "all"), "");
  assert.equal(
    doneQuery(["p-a"], "failed", { project: "p-a", before: "t-1", before_id: "" }),
    "?project=p-a&outcome=failed&before=t-1",
  );
});

test("renderDoneControls marks the active outcome and density chips", () => {
  const html = renderDoneControls("merged", "compact");
  assert.match(html, /data-done-outcome="merged" aria-pressed="true"/);
  assert.match(html, /data-done-density="compact" aria-pressed="true"/);
  assert.doesNotMatch(html, /data-done-outcome="all" aria-pressed/);
});

test("renderDoneRow links the task, badges the outcome, and joins the meta", () => {
  const html = renderDoneRow({
    task: { id: "t-0001", title: "Ship it", done_at: "2026-06-02T10:00:00Z" },
    card: { change: { id: "ch-0009" } },
    laneState: "done:completed",
    project: { id: "p-alpha", name: "Alpha", badge: true },
  }, true);
  assert.match(html, /href="\/ui\/tasks\/t-0001"/);
  assert.match(html, /ch-0009/);
  assert.match(html, /card-project-badge/);
  assert.match(html, /data-phase="dead"/);
});

test("flow-done renders the first page and Load more appends the next", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  window.localStorage.setItem("flow.ui.doneDensity", "compact");
  globalThis.fetch = (path) => {
    calls.push(String(path));
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve(page([{ id: "t-0003", title: "Older", done_at: "2026-05-30T09:00:00Z" }], { cursors: false })),
    });
  };
  try {
    const element = mountElement(root, "flow-done", {
      page: page([
        { id: "t-0001", title: "Newer", done_at: "2026-06-02T10:00:00Z" },
        { id: "t-0002", title: "Middle", done_at: "2026-06-01T10:00:00Z" },
      ]),
      projectBadge: false,
    });
    await flush();
    assert.equal(element.querySelectorAll(".done-row").length, 2);
    assert.ok(element.querySelector("[data-done-more]"), "cursor page offers Load more");

    element.querySelector("[data-done-more]").click();
    await flush();
    await new Promise((resolve) => setImmediate(resolve));
    await flush();

    assert.equal(calls.length, 1, "Load more fetches exactly the next page");
    assert.match(calls[0], /before=2026-06-01/);
    assert.equal(element.querySelectorAll(".done-row").length, 3, "the next page appends");
    assert.equal(element.querySelector("[data-done-more]"), null, "an exhausted cursor hides Load more");
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("flow-done density toggle repaints locally without a fetch", async () => {
  const originalFetch = globalThis.fetch;
  let fetches = 0;
  window.localStorage.setItem("flow.ui.doneDensity", "compact");
  globalThis.fetch = () => {
    fetches += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  try {
    const element = mountElement(root, "flow-done", {
      page: page([{ id: "t-0001", title: "Row", done_at: "2026-06-02T10:00:00Z" }], { cursors: false }),
      projectBadge: false,
    });
    await flush();
    assert.equal(element.querySelectorAll(".done-row").length, 1, "compact is the default density");

    element.querySelector('[data-done-density="extended"]').click();
    await flush();
    assert.equal(element.querySelectorAll(".done-row").length, 0, "extended swaps rows for cards");
    assert.equal(element.querySelectorAll("flow-task-card").length, 1);
    assert.equal(fetches, 0, "the toggle never refetches");
  } finally {
    globalThis.fetch = originalFetch;
  }
});
