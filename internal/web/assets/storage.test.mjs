// storage.js unit tests for the Tasks view's state-chip selection. The
// persisted form is a JSON array of lifecycle states; the legacy single values
// ("all" or one state key) still load so existing users keep their filter.

import test from "node:test";
import assert from "node:assert/strict";
import { installTestDOM } from "./test-dom.mjs";
import { readBoardSort, readBoardSortChoice, readTasksState, writeBoardSort, writeTasksState } from "./storage.js";
import { BOARD_SORT_STORAGE_KEY } from "./config.js";

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

test("readBoardSort defaults to Task number ascending", () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" }, "unset means the default sort, a no-op on today's order");
  assert.equal(readBoardSortChoice(), null, "and no choice was stored");
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, "bogus");
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" }, "corrupt values fall back to the default");
  assert.equal(readBoardSortChoice(), null, "a corrupt value is not a choice");
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, "{}");
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" });
  assert.equal(readBoardSortChoice(), null);
});

test("readBoardSort loads a persisted sort and rejects bad shapes", () => {
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, JSON.stringify({ key: "activity", dir: "desc" }));
  assert.deepEqual(readBoardSort(), { key: "activity", dir: "desc" });
  assert.deepEqual(readBoardSortChoice(), { key: "activity", dir: "desc" }, "a stored valid shape is the choice");
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, JSON.stringify({ key: "activity", dir: "sideways" }));
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" }, "an invalid direction falls back to the default");
  assert.equal(readBoardSortChoice(), null, "an invalid direction is not a choice");
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, JSON.stringify({ key: "priority", dir: "asc" }));
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" }, "an invalid key falls back to the default");
  assert.equal(readBoardSortChoice(), null, "an invalid key is not a choice");
});

test("writeBoardSort persists the chosen sort", () => {
  writeBoardSort({ key: "activity", dir: "asc" });
  assert.equal(window.localStorage.getItem(BOARD_SORT_STORAGE_KEY), '{"key":"activity","dir":"asc"}');
  assert.deepEqual(readBoardSort(), { key: "activity", dir: "asc" });
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
});
