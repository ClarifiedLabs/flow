// The lifecycle vocabulary shared by the board modules (board-model.js,
// config.js, board.js, and the board elements that render cardModel). The
// server owns the truth: LifecycleState in internal/coordinator/tasks.go
// defines scheduled, in_progress and done. The board adds exactly one
// client-side derived state of its own — unscheduled — which is how a task
// with no lifecycle state reads on the wire (the API emits an absent state
// for backlog work, and the server's tasks filter recognizes "unscheduled"
// as "lifecycle_state IS NULL").
//
// This module is the single place those strings live: lanes, phases, filters
// and dwell all read from it instead of hard-coding the literals. The Go
// parity test (internal/web/lifecycle_parity_test.go) reads LIFECYCLE_STATES
// mechanically and fails the build if it drifts from the server constants,
// so a new LifecycleState cannot silently leave the board vocabulary stale.
// Keep the set as references to the exported constants below — the parity
// test resolves them by name.

import { value } from "./normalize.js";

export const LIFECYCLE_UNSCHEDULED = "unscheduled";
export const LIFECYCLE_SCHEDULED = "scheduled";
export const LIFECYCLE_IN_PROGRESS = "in_progress";
export const LIFECYCLE_DONE = "done";

export const LIFECYCLE_STATES = new Set([
  LIFECYCLE_UNSCHEDULED,
  LIFECYCLE_SCHEDULED,
  LIFECYCLE_IN_PROGRESS,
  LIFECYCLE_DONE,
]);

// isLifecycleState is the fail-closed membership check: anything outside the
// vocabulary is not a lifecycle state the board knows how to render.
export function isLifecycleState(value) {
  return typeof value === "string" && LIFECYCLE_STATES.has(value);
}

// lifecycleStateOf reads a task's lifecycle state the way the board does and
// normalizes it onto the vocabulary: an absent state is unscheduled (backlog
// work), and a value outside the vocabulary — a malformed payload or a state
// the server has not shipped the board vocabulary for — also reads as
// unscheduled rather than being bucketed into a lane, dwell kind or phase it
// does not belong to.
export function lifecycleStateOf(task) {
  const state = value(task, "state", "State");
  return isLifecycleState(state) ? state : LIFECYCLE_UNSCHEDULED;
}
