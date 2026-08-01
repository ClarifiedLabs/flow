// storage.js unit tests for the Tasks view's state-chip selection. The
// persisted form is a JSON array of lifecycle states; the legacy single values
// ("all" or one state key) still load so existing users keep their filter.

import test from "node:test";
import assert from "node:assert/strict";
import { installTestDOM } from "./test-dom.mjs";
import { readTasksState, writeTasksState } from "./storage.js";

installTestDOM();

const KEY = "flow.ui.tasksState";
const ALL = new Set(["unscheduled", "scheduled", "in_progress", "done"]);

test("readTasksState defaults to every state selected", () => {
  window.localStorage.removeItem(KEY);
  assert.deepEqual(readTasksState(), ALL);
});

test("readTasksState loads a persisted JSON array selection", () => {
  window.localStorage.setItem(KEY, JSON.stringify(["scheduled", "done"]));
  assert.deepEqual(readTasksState(), new Set(["scheduled", "done"]));
  window.localStorage.setItem(KEY, JSON.stringify([]));
  assert.deepEqual(readTasksState(), new Set());
});

test("readTasksState loads the legacy single-state values", () => {
  window.localStorage.setItem(KEY, "all");
  assert.deepEqual(readTasksState(), ALL);
  window.localStorage.setItem(KEY, "in_progress");
  assert.deepEqual(readTasksState(), new Set(["in_progress"]));
});

test("readTasksState ignores unknown values", () => {
  window.localStorage.setItem(KEY, JSON.stringify(["bogus", "done"]));
  assert.deepEqual(readTasksState(), new Set(["done"]));
  window.localStorage.setItem(KEY, "bogus");
  assert.deepEqual(readTasksState(), ALL);
  window.localStorage.setItem(KEY, "{}");
  assert.deepEqual(readTasksState(), ALL);
});

test("writeTasksState persists the selection as a JSON array", () => {
  writeTasksState(new Set(["unscheduled", "in_progress"]));
  assert.equal(window.localStorage.getItem(KEY), '["unscheduled","in_progress"]');
  writeTasksState(new Set());
  assert.equal(window.localStorage.getItem(KEY), "[]");
  writeTasksState(new Set(["bogus", "done"]));
  assert.equal(window.localStorage.getItem(KEY), '["done"]');
});
