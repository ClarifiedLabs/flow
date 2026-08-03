// storage.js unit tests for the Tasks view's state-chip selection. The
// persisted form is a JSON array of lifecycle states; the legacy single values
// ("all" or one state key) still load so existing users keep their filter.

import test from "node:test";
import assert from "node:assert/strict";
import { installTestDOM } from "./test-dom.mjs";
import { readBoardSort, readBoardSortChoice, readTasksListView, readTasksState, readWorkPreferences, readWorkProject, writeBoardSort, writeTasksListView, writeTasksState, writeWorkPreferences, writeWorkProject } from "./storage.js";
import { BOARD_SORT_STORAGE_KEY, TASKS_LIST_VIEW_STORAGE_KEY, WORK_PREFERENCES_STORAGE_KEY, WORK_PROJECT_STORAGE_KEY } from "./config.js";

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

test("Tasks list layout storage validates reads and writes", () => {
  window.localStorage.removeItem(TASKS_LIST_VIEW_STORAGE_KEY);
  assert.equal(readTasksListView(), "flat");
  window.localStorage.setItem(TASKS_LIST_VIEW_STORAGE_KEY, "container");
  assert.equal(readTasksListView(), "container");
  window.localStorage.setItem(TASKS_LIST_VIEW_STORAGE_KEY, "tree");
  assert.equal(readTasksListView(), "flat", "a corrupt persisted layout falls back to flat");

  writeTasksListView("container");
  assert.equal(window.localStorage.getItem(TASKS_LIST_VIEW_STORAGE_KEY), "container");
  writeTasksListView("tree");
  assert.equal(window.localStorage.getItem(TASKS_LIST_VIEW_STORAGE_KEY), "container", "an invalid write is ignored");
  writeTasksListView("flat");
  assert.equal(readTasksListView(), "flat");
});

test("Work project storage trims values and clears empty selections", () => {
  writeWorkProject("  p-alpha  ");
  assert.equal(readWorkProject(), "p-alpha");
  writeWorkProject("");
  assert.equal(window.localStorage.getItem(WORK_PROJECT_STORAGE_KEY), null);
});

test("Work preferences are validated and isolated per project", () => {
  window.localStorage.removeItem(WORK_PREFERENCES_STORAGE_KEY);
  assert.deepEqual(readWorkPreferences("p-a"), { view: "overview", filter: "all", completedCollapsed: true, collapsed: new Set() });
  writeWorkPreferences("p-a", { view: "tree", filter: "blocked", completedCollapsed: false, collapsed: new Set([" e-1 ", "e-1"]) });
  writeWorkPreferences("p-b", { view: "overview", filter: "completed", completedCollapsed: true, collapsed: new Set(["f-1"]) });
  assert.deepEqual(readWorkPreferences("p-a"), { view: "tree", filter: "blocked", completedCollapsed: false, collapsed: new Set(["e-1"]) });
  assert.deepEqual(readWorkPreferences("p-b").collapsed, new Set(["f-1"]));
});

test("Work preferences reject corrupt shapes without leaking between projects", () => {
  window.localStorage.setItem(WORK_PREFERENCES_STORAGE_KEY, JSON.stringify({ "p-a": { view: "bogus", filter: "ready", completedCollapsed: "no", collapsed: ["", 4, "e-1"] } }));
  assert.deepEqual(readWorkPreferences("p-a"), { view: "overview", filter: "all", completedCollapsed: true, collapsed: new Set(["e-1"]) });
  window.localStorage.setItem(WORK_PREFERENCES_STORAGE_KEY, "[]");
  writeWorkPreferences("p-a", { view: "tree", filter: "open", completedCollapsed: false, collapsed: [] });
  assert.deepEqual(readWorkPreferences("p-a").view, "tree");
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
