// Tests for the shared lifecycle vocabulary (lifecycle.js) and the board
// modules that derive their lanes, phases and filters from it. The Go-side
// parity test (internal/web/lifecycle_parity_test.go) keeps the vocabulary
// itself aligned with the server's LifecycleState constants; these tests keep
// the board modules aligned with the vocabulary, so a new server lifecycle
// state fails loudly here and in the Go test instead of silently rendering as
// a wrong lane, phase or filter.

import assert from "node:assert/strict";
import test from "node:test";

import {
  LIFECYCLE_DONE,
  LIFECYCLE_IN_PROGRESS,
  LIFECYCLE_SCHEDULED,
  LIFECYCLE_STATES,
  LIFECYCLE_UNSCHEDULED,
  isLifecycleState,
  lifecycleStateOf,
} from "./lifecycle.js";
import { LANES, TASKS_STATES } from "./config.js";
import { phaseKey } from "./board.js";
import { activityGroupOf, cardModel, phaseSlug } from "./board-model.js";
import { installTestDOM } from "./test-dom.mjs";

// elements/tasks-view.js defines a custom-element class at module scope, which
// needs a global HTMLElement; a bare constructor is enough here (the chip
// vocabulary is all these tests read).
globalThis.HTMLElement ??= class {};
const { TASKS_STATE_FILTERS } = await import("./elements/tasks-view.js");

installTestDOM();

// epic.js pulls in the custom-element base class, which needs the test DOM
// installed first, so it is imported after installTestDOM().
const { memberState } = await import("./elements/epic.js");
const { renderNavStatus } = await import("./nav.js");

// --- vocabulary -------------------------------------------------------------

test("the shared vocabulary is exactly the four board lifecycle states", () => {
  assert.deepEqual(
    [...LIFECYCLE_STATES].sort(),
    ["done", "in_progress", "scheduled", "unscheduled"],
  );
  assert.equal(LIFECYCLE_UNSCHEDULED, "unscheduled");
  assert.equal(LIFECYCLE_SCHEDULED, "scheduled");
  assert.equal(LIFECYCLE_IN_PROGRESS, "in_progress");
  assert.equal(LIFECYCLE_DONE, "done");
});

test("isLifecycleState fails closed outside the vocabulary", () => {
  for (const state of LIFECYCLE_STATES) {
    assert.equal(isLifecycleState(state), true, state);
  }
  for (const state of ["", "paused", "blocked", "working", "done ", "Done", null, undefined, 5]) {
    assert.equal(isLifecycleState(state), false, String(state));
  }
});

test("lifecycleStateOf reads both wire spellings and normalizes absent or unknown states to unscheduled", () => {
  assert.equal(lifecycleStateOf({ state: "in_progress" }), "in_progress");
  assert.equal(lifecycleStateOf({ State: "scheduled" }), "scheduled");
  assert.equal(lifecycleStateOf({}), LIFECYCLE_UNSCHEDULED);
  assert.equal(lifecycleStateOf({ state: null }), LIFECYCLE_UNSCHEDULED);
  assert.equal(lifecycleStateOf(null), LIFECYCLE_UNSCHEDULED);
  // A lifecycle state the server has not shipped the board vocabulary for
  // reads as unscheduled rather than being bucketed into a lane or phase it
  // does not belong to.
  assert.equal(lifecycleStateOf({ state: "paused" }), LIFECYCLE_UNSCHEDULED);
});

// --- board modules derive their vocabulary from the shared set --------------

test("config.js derives the board lanes and filter states from the vocabulary", () => {
  assert.equal(LANES[0][0], LIFECYCLE_SCHEDULED);
  assert.deepEqual([...TASKS_STATES].sort(), [...LIFECYCLE_STATES].sort());
});

test("the Tasks view filter chips cover exactly the vocabulary, in order", () => {
  const chipKeys = TASKS_STATE_FILTERS.filter(([key]) => key !== "all").map(([key]) => key);
  assert.deepEqual(chipKeys, [...LIFECYCLE_STATES]);
});

test("phaseKey and phaseSlug map every vocabulary state to a non-empty phase", () => {
  for (const state of LIFECYCLE_STATES) {
    assert.notEqual(phaseKey(state), "", state);
    assert.notEqual(phaseSlug(state), "", state);
  }
});

test("phaseKey maps every lifecycle constant to its board phase", () => {
  assert.equal(phaseKey(LIFECYCLE_UNSCHEDULED), "backlog");
  assert.equal(phaseKey(LIFECYCLE_SCHEDULED), "up_next");
  assert.equal(phaseKey(LIFECYCLE_IN_PROGRESS), "authoring");
  assert.equal(phaseKey(LIFECYCLE_DONE), "dead");
});

// --- fail-closed rendering for unknown states -------------------------------

test("a state outside the vocabulary cannot land in a board lane, phase or filter", () => {
  const entry = { task: { state: "paused" }, card: {}, laneState: "", blocked: false };
  assert.equal(activityGroupOf(entry), "");
  const model = cardModel(entry);
  assert.equal(model.lifecycleState, LIFECYCLE_UNSCHEDULED);
  assert.equal(model.scheduled, false);
  assert.equal(model.running, false);
  // Unscheduled dwell thresholds never stall, so an unknown state gets no
  // false stall warning.
  assert.equal(model.dwellTone, "muted");
});

// --- epic and nav call sites ------------------------------------------------

test("epic memberState buckets lifecycle states from the shared vocabulary", () => {
  assert.equal(memberState({ state: LIFECYCLE_SCHEDULED }), "queued");
  assert.equal(memberState({ state: LIFECYCLE_IN_PROGRESS }), "working");
  assert.equal(memberState({ State: LIFECYCLE_IN_PROGRESS }), "working");
  assert.equal(memberState({ state: LIFECYCLE_DONE }), LIFECYCLE_UNSCHEDULED);
  assert.equal(memberState({}), LIFECYCLE_UNSCHEDULED);
  // A state the server has not shipped the vocabulary for fails closed into
  // the unscheduled bucket rather than a wrong working/queued group.
  assert.equal(memberState({ state: "paused" }), LIFECYCLE_UNSCHEDULED);
});

test("nav lane counts render from the shared vocabulary keys", () => {
  // The payload is keyed by the vocabulary constants themselves, so a badge
  // only renders if nav.js reads the same constants (a hard-coded literal
  // that drifted from the vocabulary would read zero and fail the match).
  const board = {
    [LIFECYCLE_UNSCHEDULED]: 2,
    [LIFECYCLE_SCHEDULED]: 3,
    [LIFECYCLE_IN_PROGRESS]: 4,
    blocked: 1,
  };
  const boardHTML = renderNavStatus("/ui/board", { board });
  assert.match(boardHTML, new RegExp(`data-board-lane="${LIFECYCLE_SCHEDULED}" title="3 scheduled tasks">3`));
  assert.match(boardHTML, new RegExp(`data-board-lane="${LIFECYCLE_IN_PROGRESS}" title="4 in progress tasks">4`));
  // The board badge reads no unscheduled lane; the consolidated Work badge
  // does, and both key off the vocabulary.
  assert.doesNotMatch(boardHTML, new RegExp(`data-board-lane="${LIFECYCLE_UNSCHEDULED}"`));
  assert.match(renderNavStatus("/ui/work-items", { board }), new RegExp(`title="2 ${LIFECYCLE_UNSCHEDULED} tasks">2`));
  assert.match(renderNavStatus("/ui/done", { [LIFECYCLE_DONE]: 7 }), new RegExp(`title="7 done items">7`));
});
