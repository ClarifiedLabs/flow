// <flow-tasks> element tests: chip clicks reload through the app while the
// element keeps its identity (and its selection), the search draft survives a
// selection repaint, and a bulk action fans out per selected task.

import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

const root = installTestDOM();
await import("./elements/tasks.js");

function tasksApp() {
  const app = document.createElement("flow-app");
  app.loads = 0;
  app.statuses = [];
  app.load = async () => { app.loads += 1; };
  app.setStatus = (message) => app.statuses.push(message);
  app.selectedProjectIDs = () => [];
  app.projects = [];
  root.appendChild(app);
  return app;
}

function payload(tasks) {
  return { tasks, workIndexes: new Map(), projectBadge: false };
}

test("a state chip click persists the selection and reloads the route", async () => {
  const app = tasksApp();
  const element = mountElement(app, "flow-tasks", payload([
    { id: "t-0001", title: "One", state: "unscheduled", project_id: "p-alpha" },
  ]));
  await flush();
  assert.ok(element.querySelector("[data-tasks-state]"), "the chips render");

  element.querySelector('[data-tasks-state="unscheduled"]').click();
  await flush();
  assert.equal(app.loads, 1, "the chip reloads through the app");
  assert.equal(element.tasksState.has("unscheduled"), false, "the chip toggled its state off");
  assert.equal(element.tasksState.size, 3, "the other states stay selected");
  assert.deepEqual(JSON.parse(window.localStorage.getItem("flow.ui.tasksState")).sort(), ["done", "in_progress", "scheduled"], "the toggle persists");
});

test("the search draft survives a selection repaint", async () => {
  const app = tasksApp();
  const element = mountElement(app, "flow-tasks", payload([
    { id: "t-0001", title: "One", state: "unscheduled", project_id: "p-alpha" },
    { id: "t-0002", title: "Two", state: "unscheduled", project_id: "p-alpha" },
  ]));
  await flush();

  const search = element.querySelector("[data-tasks-search]");
  search.value = "in progress typing";
  search.dispatchEvent(new Event("input", { bubbles: true }));
  await flush();

  const box = element.querySelector('[data-tasks-select="t-0001"]');
  box.checked = true;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await flush();

  const repainted = element.querySelector("[data-tasks-search]");
  assert.equal(repainted.getAttribute("value"), "in progress typing", "the draft survives the wholesale repaint");
  assert.equal(element.tasksSelected.has("t-0001"), true);
  assert.equal(app.loads, 0, "selection never reloads");
});

test("a bulk schedule fans out per selected task and clears the succeeded", async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path: String(path), method: options?.method || "GET" });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  try {
    const app = tasksApp();
    const element = mountElement(app, "flow-tasks", payload([
      { id: "t-0001", title: "One", state: "unscheduled", project_id: "p-alpha" },
      { id: "t-0002", title: "Two", state: "unscheduled", project_id: "p-alpha" },
    ]));
    await flush();

    const selectAll = element.querySelector("[data-tasks-select-all]");
    selectAll.checked = true;
    selectAll.dispatchEvent(new Event("change", { bubbles: true }));
    await flush();
    assert.ok(element.querySelector(".tasks-bulk-bar"), "the bulk bar shows once rows are selected");

    element.querySelector('[data-tasks-apply="schedule"]').click();
    await flush();
    await new Promise((resolve) => setImmediate(resolve));
    await flush();

    assert.deepEqual(calls.map((call) => `${call.method} ${call.path}`), [
      "POST /ui/api/v2/projects/p-alpha/tasks/t-0001/schedule",
      "POST /ui/api/v2/projects/p-alpha/tasks/t-0002/schedule",
    ]);
    assert.equal(app.loads, 1, "the bulk action reloads once at the end");
    assert.equal(element.tasksSelected.size, 0, "succeeded tasks leave the selection");
    assert.match(app.statuses.at(-1), /schedule: updated 2 tasks/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
