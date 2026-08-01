// Tests for the board's pure sort logic: task-number parsing, last-activity
// resolution, and the stable board comparator shared by the lane and table
// views. No DOM: these functions only read card models.

import assert from "node:assert/strict";
import test from "node:test";

const { activityGroupOf, cardModel, compareBoardCards, lastActivityMs, taskNumber } = await import("./board-model.js");

function model(overrides = {}) {
  const { task = {}, card = {}, ...rest } = overrides;
  return cardModel({
    task: { id: "t-0001", title: "Board sort task", state: "in_progress", updated_at: "2026-01-01T00:00:00Z", ...task },
    card: { dwell_since: "2026-01-02T00:00:00Z", ...card },
    laneState: "working",
    blocked: false,
    project: { id: "p-1", name: "flow" },
    ...rest,
  });
}

// --- activityGroupOf --------------------------------------------------------

test("activityGroupOf puts every in-progress lane state in exactly one lane", () => {
  const inProgress = { task: { state: "in_progress" } };
  assert.equal(activityGroupOf({ ...inProgress, laneState: "working" }), "working");
  assert.equal(activityGroupOf({ ...inProgress, laneState: "awaiting_worker" }), "working");
  for (const state of ["blocked", "held", "ready_to_merge", "changes_requested", "in_review", "triage", "in_progress"]) {
    assert.equal(activityGroupOf({ ...inProgress, laneState: state }), "waiting", state);
  }
});

test("activityGroupOf leaves non-in-progress states out of the split", () => {
  for (const state of ["scheduled", "unscheduled", "up_next", "backlog", ""]) {
    assert.equal(activityGroupOf({ task: { state: "scheduled" }, laneState: state }), "", state);
  }
});

test("activityGroupOf waits on an in-progress task with no derived state rather than dropping it", () => {
  assert.equal(activityGroupOf({ task: { state: "in_progress" }, laneState: "" }), "waiting");
});

// --- taskNumber -------------------------------------------------------------

test("taskNumber parses the numeric task id suffix", () => {
  assert.equal(taskNumber("t-flow-0042"), 42);
  assert.equal(taskNumber("t-9"), 9);
  assert.equal(taskNumber("t-abc-1"), 1);
  assert.equal(taskNumber("t-flow-0"), 0);
});

test("taskNumber compares numerically, not as strings", () => {
  assert.ok(taskNumber("t-x-9") < taskNumber("t-x-42"));
  assert.ok(taskNumber("t-x-100") > taskNumber("t-x-99"));
});

test("taskNumber returns 0 for ids without a numeric suffix", () => {
  assert.equal(taskNumber("t-new"), 0);
  assert.equal(taskNumber(""), 0);
  assert.equal(taskNumber(undefined), 0);
});

// --- lastActivityMs ---------------------------------------------------------

test("lastActivityMs picks the most recent timestamp on the model", () => {
  const m = model({
    task: { updated_at: "2026-01-01T00:00:00Z" },
    card: {
      dwell_since: "2026-01-02T00:00:00Z",
      last_agent_activity_at: "2026-01-03T00:00:00Z",
    },
  });
  assert.equal(lastActivityMs(m), Date.parse("2026-01-03T00:00:00Z"));
});

test("lastActivityMs prefers dwell_since over task.updated_at", () => {
  const m = model({
    task: { updated_at: "2026-01-01T00:00:00Z" },
    card: { dwell_since: "2026-01-02T00:00:00Z" },
  });
  assert.equal(lastActivityMs(m), Date.parse("2026-01-02T00:00:00Z"));
});

test("lastActivityMs falls back to task.updated_at", () => {
  const m = model({ task: { updated_at: "2026-01-01T00:00:00Z" }, card: { dwell_since: "" } });
  assert.equal(lastActivityMs(m), Date.parse("2026-01-01T00:00:00Z"));
});

test("lastActivityMs returns 0 when no timestamp is present", () => {
  const m = model({ task: { updated_at: "" }, card: { dwell_since: "" } });
  assert.equal(lastActivityMs(m), 0);
});

// --- compareBoardCards ------------------------------------------------------

function cards() {
  return [
    model({ task: { id: "t-x-9", updated_at: "2026-01-01T00:00:00Z" }, card: { dwell_since: "" } }),
    model({ task: { id: "t-x-42", updated_at: "2026-01-02T00:00:00Z" }, card: { dwell_since: "" } }),
    model({ task: { id: "t-x-100", updated_at: "2026-01-03T00:00:00Z" }, card: { dwell_since: "" } }),
    model({ task: { id: "t-x-10", updated_at: "2026-01-04T00:00:00Z" }, card: { dwell_since: "" } }),
  ];
}

test("compareBoardCards sorts by task number ascending by default", () => {
  const ids = compareBoardCards(cards()).map((m) => m.id);
  assert.deepEqual(ids, ["t-x-9", "t-x-10", "t-x-42", "t-x-100"]);
});

test("compareBoardCards sorts by task number descending", () => {
  const ids = compareBoardCards(cards(), { key: "number", dir: "desc" }).map((m) => m.id);
  assert.deepEqual(ids, ["t-x-100", "t-x-42", "t-x-10", "t-x-9"]);
});

test("compareBoardCards sorts by activity most recent first by default", () => {
  const ids = compareBoardCards(cards(), { key: "activity" }).map((m) => m.id);
  assert.deepEqual(ids, ["t-x-10", "t-x-100", "t-x-42", "t-x-9"]);
});

test("compareBoardCards sorts by activity ascending", () => {
  const ids = compareBoardCards(cards(), { key: "activity", dir: "asc" }).map((m) => m.id);
  assert.deepEqual(ids, ["t-x-9", "t-x-42", "t-x-100", "t-x-10"]);
});

test("compareBoardCards breaks activity ties by task number ascending", () => {
  const models = [
    model({ task: { id: "t-x-42", updated_at: "2026-01-02T00:00:00Z" }, card: {} }),
    model({ task: { id: "t-x-9", updated_at: "2026-01-02T00:00:00Z" }, card: {} }),
  ];
  const ids = compareBoardCards(models, { key: "activity" }).map((m) => m.id);
  assert.deepEqual(ids, ["t-x-9", "t-x-42"]);
});

test("compareBoardCards is stable and does not reshuffle unchanged cards", () => {
  const input = cards();
  const first = compareBoardCards(input, { key: "activity" });
  const second = compareBoardCards(first, { key: "activity" });
  assert.deepEqual(second.map((m) => m.id), first.map((m) => m.id));
});

// --- cardModel threading ----------------------------------------------------

test("cardModel threads lastAgentActivityAt from the task card", () => {
  const m = model({ card: { last_agent_activity_at: "2026-01-05T00:00:00Z" } });
  assert.equal(m.lastAgentActivityAt, "2026-01-05T00:00:00Z");
  assert.equal(lastActivityMs(m), Date.parse("2026-01-05T00:00:00Z"));
});

test("cardModel threads task.updated_at", () => {
  const m = model({ task: { updated_at: "2026-01-06T00:00:00Z" }, card: {} });
  assert.equal(m.updatedAt, "2026-01-06T00:00:00Z");
});
