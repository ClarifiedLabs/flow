// Board-area element tests: the board/dwell/card models, lanes, blockers,
// board rendering and sorting. Split from elements.test.mjs.

// Tests for the custom-element UI: the pure render functions as strings, and
// the element lifecycle behaviours that string tests cannot reach — keyed
// reconcile preserving instance state across a poll, delegated listeners
// surviving a repaint, and disclosure staying local to its row.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement, TestEvent, TestNode } from "./test-dom.mjs";

installTestDOM();

const { cardModel, dwellTone, formatDwell, matchesFilter, sortForAttention, waitActionLabel, waitReasonText, waitingOnBlockers, waitingOnOmitted } =
  await import("./board-model.js");
const { compareBoardCards, lastActivityMs } = await import("./board-model.js");
const { readBoardSort, writeBoardSort } = await import("./storage.js");
const { BOARD_SORT_STORAGE_KEY, BOARD_VIEW_STORAGE_KEY } = await import("./config.js");
const { renderBoardSort } = await import("./elements/board-sort.js");
const { nowCardModel, runRows, tabBadges, isOutdatedAnchor, reviewModel, taskModel } = await import("./task-model.js");
const { mount, reconcile } = await import("./elements/base.js");
const { renderTaskCard } = await import("./elements/task-card.js");
const { renderTaskRail } = await import("./elements/task-rail.js");
const { renderAttentionStrip } = await import("./elements/attention-strip.js");
const { renderThroughputStrip } = await import("./elements/throughput-strip.js");
const { renderBoardTable } = await import("./elements/board-table.js");
const { boardEntries } = await import("./elements/board.js");
const { LANES } = await import("./config.js");
const { renderStepRail } = await import("./elements/step-rail.js");
const { renderRunList } = await import("./elements/run-list.js");
const { renderRunSpine } = await import("./elements/run-spine.js");
const { renderWorkflowGraph, graphCounts } = await import("./elements/workflow-graph.js");
const { renderCheckList } = await import("./elements/check-list.js");
const { renderFindings } = await import("./elements/findings-list.js");
const { renderHeldPanel, HAND_BACK_EDGES } = await import("./elements/held-panel.js");
const { renderDiffFile } = await import("./elements/diff.js");
const { renderReviewBar } = await import("./elements/review-bar.js");
const { renderEpic, memberState } = await import("./elements/epic.js");
const { renderFeatures, featureCountsLabel, featureDivergenceLabel } = await import("./elements/features.js");
const { renderFeature } = await import("./elements/feature.js");
const { renderNowCard } = await import("./elements/now-card.js");
const { renderReviewPanel } = await import("./elements/review-panel.js");
const { renderActivityFeed, activityEntries } = await import("./elements/activity-feed.js");
const { renderTaskFormView, bindRelationsPickerView, bindTaskFlowControlsView, relationTargetSuggestionsView } = await import("./task-view.js");
await import("./elements/lane.js");
await import("./elements/board.js");
await import("./elements/board-sort.js");
const { renderTabStrip } = await import("./elements/tab-strip.js");
const { acquireBusy, handleAction, inFlight, releaseBusy, settleStatus } = await import("./actions.js");
const { handleFormSubmit } = await import("./forms.js");
const { diffUnavailable } = await import("./elements/change.js");
const { renderChangeRoute } = await import("./change-route.js");
const { renderInlineThread } = await import("./elements/inline-thread.js");
const { renderOwnerRulingsPanel } = await import("./elements/task-detail-view.js");
await import("./elements/task-detail.js");

const HOUR = 3600_000;

function entry(overrides = {}) {
  const { task = {}, card = {}, ...rest } = overrides;
  return {
    task: { id: "t-0001", title: "Retry budget for failed check nodes", state: "in_progress", priority: 1, ...task },
    card: { step_index: 3, step_count: 6, dwell_since: new Date(Date.now() - 12 * 60_000).toISOString(), ...card },
    laneState: "working",
    blocked: false,
    project: { id: "p-1", name: "flow" },
    ...rest,
  };
}

// --- board model -----------------------------------------------------------

test("dwell renders one unit and no 'ago' so a column of them lines up", () => {
  const now = Date.now();
  assert.equal(formatDwell(new Date(now - 41 * 60_000).toISOString(), now), "41m");
  assert.equal(formatDwell(new Date(now - 2 * HOUR).toISOString(), now), "2h");
  assert.equal(formatDwell(new Date(now - 26 * HOUR).toISOString(), now), "1d");
  assert.equal(formatDwell("", now), "");
});

test("dwell tone grades queued work more leniently than a running task", () => {
  const now = Date.now();
  const threeHoursAgo = new Date(now - 3 * HOUR).toISOString();
  assert.equal(dwellTone("running", threeHoursAgo, now), "danger");
  assert.equal(dwellTone("queued", threeHoursAgo, now), "warn");
  assert.equal(dwellTone("unscheduled", threeHoursAgo, now), "muted");
});

test("a waiting task is amber and a failed one is red, not both blocked", () => {
  const gate = cardModel(entry({ card: { wait: { kind: "human_gate", message: "Ship it?" } } }));
  const failed = cardModel(entry({ card: { wait: { kind: "operator_intervention", reason: "execution_failed" } } }));
  assert.equal(gate.phase, "await");
  assert.equal(failed.phase, "danger");
});

test("held outranks blocked so the board says who owns the task", () => {
  const model = cardModel(entry({ card: { held: true, wait: { kind: "human_gate" } } }));
  assert.equal(model.phase, "triage");
  assert.match(model.activity, /Held by you/);
});

test("held work leaves the attention strip: you are already on it", () => {
  const held = cardModel(entry({ task: { id: "t-held" }, card: { held: true, wait: { kind: "human_gate", message: "Ship it?" } } }));
  const waiting = cardModel(entry({ task: { id: "t-waiting" }, card: { wait: { kind: "human_gate", message: "Ship it?" } } }));
  assert.ok(waiting.needsYou, "an unheld gate still needs a human");
  assert.ok(!held.needsYou, "held work would otherwise drown the tasks actually waiting");
  assert.equal(held.actionLabel, "Resume");
  assert.match(held.reason, /Held by you/);
  // The board feeds the strip only what needs a human, so a board whose sole
  // open item is held stays silent.
  assert.equal(renderAttentionStrip([held, waiting].filter((model) => model.needsYou)).includes(held.id), false);
  assert.equal(renderAttentionStrip([held].filter((model) => model.needsYou)), "");
});

test("a system convergence hold stays in the attention strip", () => {
  const held = cardModel(entry({
    task: { id: "t-convergence" },
    card: { held: true, held_by: "system" },
  }));
  assert.equal(held.needsYou, true);
  assert.match(held.reason, /Convergence review required/);
  assert.match(held.activity, /split or re-scope/);
});

test("a convergence hold reads as blocked; a manual hold stays triage", () => {
  const convergence = cardModel(entry({
    task: { id: "t-convergence" },
    card: { held: true, held_by: "system" },
    blocked: true,
  }));
  const manual = cardModel(entry({
    task: { id: "t-manual" },
    card: { held: true, held_by: "human" },
  }));
  // The system parked this task on a human scope decision, so the card says
  // blocked; a manual hold is owned by the operator, so it stays triage.
  assert.equal(convergence.phase, "blocked");
  assert.equal(manual.phase, "triage");
});

test("the attention reason is the agent's own words, never a generic label", () => {
  const question = "Should the done cursor include merged tasks?";
  assert.equal(waitReasonText({ wait: { kind: "human_gate", message: question } }), question);
  assert.equal(
    waitReasonText({ wait: { kind: "operator_intervention", message: "worker w-local-1 heartbeat expired" } }),
    "worker w-local-1 heartbeat expired",
  );
  assert.equal(waitReasonText({}, { readyToMerge: true }), "Checks and review passed — ready to merge");
});

test("the action label follows the wait kind", () => {
  assert.equal(waitActionLabel({ wait: { kind: "human_gate" } }), "Answer");
  assert.equal(waitActionLabel({ wait: { kind: "operator_intervention" } }), "Retry");
  assert.equal(waitActionLabel({ wait: { reason: "transition_budget_exhausted" } }), "Extend budget");
  assert.equal(waitActionLabel({ wait: { reason: "review_cycle_limit" } }), "Grant cycles");
  assert.equal(waitActionLabel({}, { readyToMerge: true }), "Merge");
  assert.equal(waitActionLabel({}), "");
});

test("attention sorts first, then longest-waiting", () => {
  const now = Date.now();
  const models = [
    { id: "c", needsYou: false, dwellSince: new Date(now - HOUR).toISOString() },
    { id: "a", needsYou: true, dwellSince: new Date(now - 10 * 60_000).toISOString() },
    { id: "b", needsYou: true, dwellSince: new Date(now - 2 * HOUR).toISOString() },
  ];
  assert.deepEqual(
    sortForAttention(models).map((model) => model.id),
    ["b", "a", "c"],
  );
});

test("board filters mirror the lane split they bucket by", () => {
  const working = cardModel(entry({ laneState: "working" }));
  const waiting = cardModel(entry({ laneState: "blocked", card: { wait: { kind: "human_gate" } } }));
  const queued = cardModel(entry({ task: { state: "scheduled" }, laneState: "scheduled" }));
  assert.ok(matchesFilter(waiting, "attention"));
  assert.ok(matchesFilter(waiting, "waiting"));
  assert.ok(!matchesFilter(waiting, "working"));
  assert.ok(matchesFilter(working, "working"));
  assert.ok(!matchesFilter(working, "waiting"));
  assert.ok(matchesFilter(queued, "queued"));
  assert.ok(!matchesFilter(queued, "waiting"));
});

test("a task parked in the queue reads as awaiting a worker, not working", () => {
  const now = Date.now();
  const fiftyMinutesAgo = new Date(now - 50 * 60_000).toISOString();
  const model = cardModel(
    entry({ laneState: "awaiting_worker", card: { dwell_since: fiftyMinutesAgo } }),
    { now },
  );
  assert.equal(model.lifecycleState, "in_progress");
  assert.equal(model.queuedForWorker, true);
  assert.equal(model.phase, "await");
  assert.equal(model.activity, `Awaiting worker · ${model.dwell}`);
  assert.match(model.activity, /Awaiting worker · 50m/);
  // Fifty minutes in the queue is ordinary queue time, not a stall: the dwell
  // is graded on the queued thresholds, and the label carries no prefix.
  assert.equal(model.dwellTone, "muted");
  assert.equal(model.dwellLabel, model.dwell);
  assert.doesNotMatch(model.dwellLabel, /stalled|waiting/);
  assert.equal(model.running, false);
  // The lane split deliberately keeps awaiting-worker in Working — a worker is
  // expected imminently — so the Working chip matches it, and the card's amber
  // "Awaiting worker" line still names the real stall.
  assert.ok(matchesFilter(model, "working"), "awaiting-worker rides with Working");
  assert.ok(!matchesFilter(model, "waiting"), "it is not idle");
  assert.ok(!matchesFilter(model, "queued"), "Queued still means lifecycle scheduled only");
  assert.ok(matchesFilter(model, "all"), "it stays visible on the board");
});

test("an awaiting-worker card without a dwell clock still names the wait", () => {
  const model = cardModel(entry({ laneState: "awaiting_worker", card: { dwell_since: "" } }));
  assert.equal(model.activity, "Awaiting worker");
});

test("an awaiting-worker card dwells on the board clock, not the wall clock", () => {
  // A fixed model clock far from the real wall clock: the activity line must
  // use the same `now` as dwell, or the two fields drift apart (here the real
  // clock would report the queue as days old while the model says 1h).
  const now = Date.parse("2026-07-29T12:00:00Z");
  const dwellSince = "2026-07-29T11:00:00Z";
  const model = cardModel(entry({ laneState: "awaiting_worker", card: { dwell_since: dwellSince } }), { now });
  assert.equal(model.dwell, "1h");
  assert.equal(model.activity, "Awaiting worker · 1h");
  assert.equal(model.activity, `Awaiting worker · ${model.dwell}`);
});

test("a long queue warms on queue time, not running time", () => {
  const now = Date.now();
  const threeHoursAgo = new Date(now - 3 * HOUR).toISOString();
  const model = cardModel(entry({ laneState: "awaiting_worker", card: { dwell_since: threeHoursAgo } }), { now });
  assert.equal(model.dwellTone, "warn", "3h in the queue is a warning, not a danger");
});

test("held, gate and failed waits outrank the queued-for-worker tone", () => {
  const held = cardModel(entry({ laneState: "awaiting_worker", card: { held: true } }));
  assert.equal(held.phase, "triage");
  assert.match(held.activity, /Held by you/);

  const gate = cardModel(entry({ laneState: "awaiting_worker", card: { wait: { kind: "human_gate", message: "Ship it?" } } }));
  assert.equal(gate.phase, "await");
  assert.equal(gate.activity, "Ship it?");

  const failed = cardModel(
    entry({ laneState: "awaiting_worker", card: { wait: { kind: "operator_intervention", reason: "execution_failed" } } }),
  );
  assert.equal(failed.phase, "danger");
  assert.match(failed.activity, /Workflow step failed/);
});

test("a parked task that is otherwise ready to merge keeps the await presentation", () => {
  // in_progress at its last step with the required checks satisfied is normally
  // ready to merge. Parked in the queue, the amber await tone and the "Awaiting
  // worker" line must win: no Merge action, no attention flag, no merge copy.
  const checks = {
    step_index: 5,
    step_count: 6,
    required_checks: { total: 2, satisfied: 2 },
  };
  const model = cardModel(entry({ laneState: "awaiting_worker", card: { ...checks } }));
  assert.equal(model.queuedForWorker, true);
  assert.equal(model.phase, "await", "the await tone wins over ready-to-merge");
  assert.equal(model.readyToMerge, false, "no Merge action while the job is unclaimed");
  assert.equal(model.actionLabel, "");
  assert.equal(model.needsYou, false);
  assert.match(model.activity, /Awaiting worker/);
  assert.doesNotMatch(model.reason, /ready to merge/);
  assert.ok(!renderTaskCard(model).includes("data-card-merge"), "the lane card must not advertise Merge");

  // Sanity: the very same entry, once claimed, is ready to merge — so it is the
  // queue gate, not a malformed entry, that holds the action back.
  const claimed = cardModel(entry({ card: { ...checks } }));
  assert.equal(claimed.readyToMerge, true);
  assert.equal(claimed.actionLabel, "Merge");
  assert.equal(claimed.needsYou, true);
});

test("the table's Working chip carries awaiting-worker rows and Waiting carries idle ones", () => {
  const parked = cardModel(entry({ task: { id: "t-parked" }, laneState: "awaiting_worker" }));
  const idle = cardModel(entry({ task: { id: "t-idle" }, laneState: "blocked", card: { wait: { kind: "human_gate" } } }));
  const working = cardModel(entry({ task: { id: "t-working" }, laneState: "working" }));
  const all = renderBoardTable([parked, idle, working], "all");
  assert.match(all, /t-parked/);
  assert.match(all, /Awaiting worker/);
  assert.match(all, /rail-label is-idle">in progress</, "the step rail must not pretend work is happening");
  const workingView = renderBoardTable([parked, idle, working], "working");
  assert.ok(workingView.includes("t-parked"), "awaiting-worker rows stay with Working, matching their lane");
  assert.ok(workingView.includes("t-working"));
  assert.ok(!workingView.includes("t-idle"));
  const waitingView = renderBoardTable([parked, idle, working], "waiting");
  assert.ok(waitingView.includes("t-idle"), "a blocked row matches Waiting, matching its lane");
  assert.ok(!waitingView.includes("t-parked"));
  assert.ok(!waitingView.includes("t-working"));
});

test("a lane card for a queued-for-worker task wears the await tone and names the wait", async () => {
  const root = globalThis.document.body;
  const lane = mountElement(root, "flow-lane", {
    key: "working",
    label: "Working",
    cards: [cardModel(entry({ laneState: "awaiting_worker" }))],
  });
  await flush();
  const card = lane.querySelector("flow-task-card");
  assert.ok(card, "the lane renders the card");
  assert.equal(card.getAttribute("data-phase"), "await");
  assert.match(card.innerHTML, /Awaiting worker/);
  assert.ok(!card.innerHTML.includes('class="step"'), "no rail while the job sits unclaimed");
  lane.remove();
});

// --- lane split -------------------------------------------------------------

function boardPayload(tasks, laneStates) {
  return {
    boards: [{
      project_id: "p-1",
      project_name: "flow",
      // The server marshals coordinator.Board without json tags, so the wire
      // keys are the PascalCase Go field names; the fixture must match them or
      // the lane split tests would pin a shape /v2/board never emits.
      board: {
        Scheduled: [{ id: "t-sched", title: "Scheduled task", state: "scheduled" }],
        InProgress: tasks,
      },
      task_cards: Object.fromEntries(tasks.map((task) => [task.id, { dwell_since: new Date().toISOString() }])),
      lane_states: laneStates,
    }],
  };
}

test("LANES wire keys match the /v2/board PascalCase lane keys", () => {
  // coordinator.Board marshals without json tags, so /v2/board emits the Go
  // field names verbatim and laneTasks reads each lane's list by the exact
  // third element of the LANES triple. If these drift (t-flow-0136 renamed
  // them to snake_case) every lane renders empty against the served payload
  // while fixture-built tests stay green. The coordinator test
  // TestBoardWireKeysPinTheBoardLaneContract pins the same keys Go-side.
  assert.deepEqual(
    LANES.map(([, , field]) => field),
    ["Scheduled", "InProgress", "InProgress"],
  );
});

test("boardEntries buckets every in-progress task into exactly one lane", () => {
  const entries = boardEntries(boardPayload(
    [
      { id: "t-work", title: "Executing", state: "in_progress" },
      { id: "t-parked", title: "Awaiting worker", state: "in_progress" },
      { id: "t-wait", title: "Blocked", state: "in_progress" },
      { id: "t-held", title: "Held", state: "in_progress" },
    ],
    {
      "t-work": "working",
      "t-parked": "awaiting_worker",
      "t-wait": "blocked",
      "t-held": "held",
    },
  ));
  const byLane = (lane) => entries.filter((entry) => entry.lane === lane).map((entry) => entry.task.id);
  assert.deepEqual(byLane("scheduled"), ["t-sched"]);
  assert.deepEqual(byLane("working"), ["t-work", "t-parked"], "awaiting-worker rides with Working");
  assert.deepEqual(byLane("waiting"), ["t-wait", "t-held"]);
  // Exactly one lane per task: the two InProgress lanes must not duplicate.
  assert.deepEqual(entries.map((entry) => entry.task.id).sort(), ["t-held", "t-parked", "t-sched", "t-wait", "t-work"]);
});

test("lane headers count the Working and Waiting cards", async () => {
  const root = globalThis.document.body;
  const entries = boardEntries(boardPayload(
    [
      { id: "t-work", title: "Executing", state: "in_progress" },
      { id: "t-parked", title: "Awaiting worker", state: "in_progress" },
      { id: "t-wait", title: "Blocked", state: "in_progress" },
    ],
    { "t-work": "working", "t-parked": "awaiting_worker", "t-wait": "blocked" },
  ));
  const board = mountElement(root, "flow-board", { entries });
  await flush();
  const lanes = board.querySelectorAll("flow-lane");
  assert.equal(lanes.length, 3, "scheduled, working, waiting");
  const header = (lane) => [...lanes].find((node) => node.getAttribute("data-lane") === lane).innerHTML;
  assert.match(header("scheduled"), /Scheduled · 1/);
  assert.match(header("working"), /Working · 2/);
  assert.match(header("waiting"), /Waiting · 1/);
  board.remove();
});

test("an idle split lane says No active work or Nothing waiting", async () => {
  const root = globalThis.document.body;
  const entries = boardEntries(boardPayload([], {}));
  const board = mountElement(root, "flow-board", { entries });
  await flush();
  const lanes = board.querySelectorAll("flow-lane");
  const working = [...lanes].find((node) => node.getAttribute("data-lane") === "working");
  const waiting = [...lanes].find((node) => node.getAttribute("data-lane") === "waiting");
  assert.match(working.innerHTML, /No active work/);
  assert.match(waiting.innerHTML, /Nothing waiting/);
  board.remove();
});

// --- waiting on blockers ----------------------------------------------------

test("a scheduled card with a live blocker carries it as waiting on", () => {
  const model = cardModel(entry({
    task: { state: "scheduled" },
    card: { blockers: { count: 1, tasks: [{ id: "t-0009", title: "Finish dependency" }] } },
  }));
  assert.deepEqual(model.waitingOn, [{ id: "t-0009", title: "Finish dependency" }]);
});

test("a scheduled card with no blockers carries nothing to wait on", () => {
  const empty = cardModel(entry({ task: { state: "scheduled" }, card: { blockers: { count: 0 } } }));
  const absent = cardModel(entry({ task: { state: "scheduled" } }));
  assert.deepEqual(empty.waitingOn, []);
  assert.deepEqual(absent.waitingOn, []);
});

test("waiting on only applies while the task is scheduled", () => {
  const blockers = { count: 1, tasks: [{ id: "t-0009", title: "Finish dependency" }] };
  // The read model already drops resolved blockers; the card only surfaces what
  // is left while the task is queued. An in-progress task is past its blockers.
  assert.deepEqual(waitingOnBlockers({ blockers }, "scheduled").length, 1);
  assert.deepEqual(waitingOnBlockers({ blockers }, "in_progress"), []);
  assert.deepEqual(waitingOnBlockers({ blockers }, "unscheduled"), []);
});

test("a resolved blocker disappears from the card", () => {
  // Once the blocker reaches done the read model empties the task list, so the
  // card has nothing left to wait on.
  const model = cardModel(entry({
    task: { state: "scheduled" },
    card: { blockers: { count: 0, tasks: [] } },
  }));
  assert.deepEqual(model.waitingOn, []);
});

test("the card carries the read model's priority-ordered blocker titles in order", () => {
  // The Go read model ranks live blockers (priority, then recency) before
  // bounding the display; the model must hand those titles to the card in the
  // same order so the most important blockers read first.
  const model = cardModel(entry({
    task: { state: "scheduled" },
    card: {
      blockers: {
        count: 4,
        omitted: 1,
        tasks: [
          { id: "t-high", title: "High priority blocker" },
          { id: "t-mid", title: "Mid priority blocker" },
          { id: "t-overflow", title: "Overflow blocker" },
        ],
      },
    },
  }));
  assert.deepEqual(model.waitingOn, [
    { id: "t-high", title: "High priority blocker" },
    { id: "t-mid", title: "Mid priority blocker" },
    { id: "t-overflow", title: "Overflow blocker" },
  ]);
  assert.equal(model.waitingOnOmitted, 1);
});

test("waiting-on overflow only counts while the task is scheduled", () => {
  const blockers = { count: 4, omitted: 1, tasks: [{ id: "t-0009", title: "x" }] };
  assert.equal(waitingOnOmitted({ blockers }, "scheduled"), 1);
  assert.equal(waitingOnOmitted({ blockers }, "in_progress"), 0);
  assert.equal(waitingOnOmitted({ blockers }, "unscheduled"), 0);
  assert.equal(waitingOnOmitted({ blockers: { count: 1 } }, "scheduled"), 0);
});

// --- board rendering -------------------------------------------------------

test("the card shows the title alone; the id moved to the meta row", () => {
  const html = renderTaskCard(cardModel(entry()));
  assert.match(html, /<a class="title"[^>]*>Retry budget for failed check nodes<\/a>/);
  assert.match(html, /<span class="id">t-0001<\/span>/);
  assert.ok(!/title"[^>]*>t-0001 ·/.test(html), "the id must not be prefixed onto the title again");
});

test("a board card renders its canonical top-level container and exact group count", () => {
  const model = cardModel(entry({
    card: { container: { id: "e-0001", kind: "epic", title: "Launch", task_count: 2 } },
  }));
  assert.deepEqual(model.container, { id: "e-0001", kind: "epic", title: "Launch", taskCount: 2 });
  const html = renderTaskCard(model);
  assert.match(html, /href="\/ui\/projects\/p-1\/epics\/e-0001"/);
  assert.match(html, />Launch · 2 tasks<\/a>/);
});

test("a scheduled card with a blocker names what it is waiting on and links to it", () => {
  const html = renderTaskCard(cardModel(entry({
    task: { state: "scheduled" },
    card: { blockers: { count: 1, tasks: [{ id: "t-0009", title: "Finish dependency" }] } },
  })));
  assert.match(html, /<p class="waiting-on">waiting on <a href="\/ui\/tasks\/t-0009"[^>]*>Finish dependency<\/a><\/p>/);
});

test("a card with no blockers renders no waiting-on indicator", () => {
  const html = renderTaskCard(cardModel(entry({ task: { state: "scheduled" }, card: { blockers: { count: 0 } } })));
  assert.ok(!html.includes("waiting-on"), "no blockers means no waiting-on line");
});

test("a card past the blocker limit discloses the overflow as +N more", () => {
  const html = renderTaskCard(cardModel(entry({
    task: { state: "scheduled" },
    card: {
      blockers: {
        count: 4,
        omitted: 1,
        tasks: [
          { id: "t-high", title: "High priority blocker" },
          { id: "t-mid", title: "Mid priority blocker" },
          { id: "t-overflow", title: "Overflow blocker" },
        ],
      },
    },
  })));
  // The shown titles keep the priority order and each links to its task; the
  // omitted blocker is disclosed, not dropped.
  assert.match(
    html,
    /<p class="waiting-on">waiting on <a href="\/ui\/tasks\/t-high"[^>]*>High priority blocker<\/a>, <a href="\/ui\/tasks\/t-mid"[^>]*>Mid priority blocker<\/a>, <a href="\/ui\/tasks\/t-overflow"[^>]*>Overflow blocker<\/a>, \+1 more<\/p>/,
  );
});

test("a card at the blocker limit renders no overflow suffix", () => {
  const html = renderTaskCard(cardModel(entry({
    task: { state: "scheduled" },
    card: {
      blockers: {
        count: 3,
        tasks: [
          { id: "t-a", title: "A" },
          { id: "t-b", title: "B" },
          { id: "t-c", title: "C" },
        ],
      },
    },
  })));
  const waitingOn = /<p class="waiting-on">[\s\S]*?<\/p>/.exec(html)?.[0];
  assert.ok(waitingOn, "the card renders a waiting-on line");
  assert.match(waitingOn, /<a href="\/ui\/tasks\/t-c"[^>]*>C<\/a><\/p>/);
  assert.ok(!waitingOn.includes("more"), "at the limit means no +N more suffix");
});

test("the card drops branch, change id and timestamps to the detail page", () => {
  const html = renderTaskCard(
    cardModel(entry({ card: { change: { id: "c-0091", branch: "task/run-2" }, active_session: { state: "working" } } })),
  );
  assert.ok(!html.includes("c-0091"), "change id belongs on task detail");
  assert.ok(!html.includes("task/run-2"), "branch belongs on task detail");
});

test("a waiting card shows the question with Answer and Approve", () => {
  const html = renderTaskCard(cardModel(entry({ card: { wait: { kind: "human_gate", message: "Ship it?" } } })));
  assert.match(html, /class="ask"/);
  assert.match(html, /Ship it\?/);
  assert.match(html, />Answer</);
  assert.match(html, /data-card-approve="t-0001"/);
});

test("the card renders activity as markdown", () => {
  const html = renderTaskCard(
    cardModel(entry({ card: { wait: { kind: "operator_intervention", reason: "execution_failed", message: "Failed on **line 42**" } } })),
  );
  assert.match(html, /<div class="activity"><div class="md">/);
  assert.match(html, /<strong>line 42<\/strong>/);
});

test("the card renders the ask reason as markdown", () => {
  const html = renderTaskCard(cardModel(entry({ card: { wait: { kind: "human_gate", message: "Ship **it**?" } } })));
  assert.match(html, /<div class="reason"><div class="md">/);
  assert.match(html, /<strong>it<\/strong>/);
});

test("a failed card offers Retry and its transcript", () => {
  const html = renderTaskCard(
    cardModel(entry({ card: { wait: { kind: "operator_intervention", reason: "execution_failed" } } })),
  );
  assert.match(html, /data-workflow-retry="t-0001"/);
  assert.match(html, />Transcript</);
});

test("a review-cycle wait offers the cycle grant action instead of retry", () => {
  const html = renderTaskCard(
    cardModel(entry({ card: { wait: { kind: "operator_intervention", reason: "review_cycle_limit" } } })),
  );
  assert.match(html, /data-workflow-budget="t-0001"/);
  assert.match(html, /data-workflow-budget-kind="review-cycles"/);
  assert.match(html, />Grant cycles</);
  assert.ok(!html.includes("data-workflow-retry"));
});

test("a resting card keeps its secondary controls out of the way", () => {
  const html = renderTaskCard(cardModel(entry()));
  assert.match(html, /class="actions on-hover"/);
});

test("task run controls offer a manual scope review for a pinned change", () => {
  const html = renderTaskRail({
    id: "t-0001",
    runID: "wr-0001",
    canRequestConvergence: true,
  });
  assert.match(html, /data-convergence-request="t-0001"/);
  assert.match(html, />Review scope</);

  const withoutChange = renderTaskRail({ id: "t-0001", runID: "wr-0001" });
  assert.ok(!withoutChange.includes("data-convergence-request"));
});

test("the step rail draws one segment per node, not a fixed six", () => {
  const html = renderStepRail({ stepIndex: 2, stepCount: 4, stepName: "checks", phase: "authoring" });
  assert.equal((html.match(/<i data-seg=/g) || []).length, 4);
  assert.match(html, /checks · 2\/4/);
});

test("the step rail caps its segments but keeps the true position in the label", () => {
  const html = renderStepRail({ stepIndex: 15, stepCount: 20, stepName: "verify", phase: "authoring" });
  assert.equal((html.match(/<i data-seg=/g) || []).length, 12);
  assert.match(html, /verify · 15\/20/);
});

test("the attention strip stays silent when nothing needs a human", () => {
  assert.equal(renderAttentionStrip([]), "");
  assert.equal(renderAttentionStrip(null), "");
});

test("the attention strip puts the oldest wait first", () => {
  const now = Date.now();
  const html = renderAttentionStrip([
    cardModel(entry({ task: { id: "t-new" }, card: { wait: { kind: "human_gate" }, dwell_since: new Date(now - 60_000).toISOString() } })),
    cardModel(entry({ task: { id: "t-old" }, card: { wait: { kind: "human_gate" }, dwell_since: new Date(now - 4 * HOUR).toISOString() } })),
  ]);
  assert.ok(html.indexOf("t-old") < html.indexOf("t-new"), "oldest wait should lead");
  assert.match(html, /oldest 4h/);
});

test("the table marks attention rows and offers quiet actions elsewhere", () => {
  const models = [
    cardModel(entry({ task: { id: "t-wait" }, card: { wait: { kind: "human_gate" } } })),
    cardModel(entry({ task: { id: "t-run" } })),
  ];
  const html = renderBoardTable(models, "all");
  assert.match(html, /data-needs-you/);
  assert.match(html, /class="quiet-action"/);
});

test("the table headers carry the sort key and reflect its direction", () => {
  const model = cardModel(entry({ task: { id: "t-0001" } }));
  const byNumber = renderBoardTable([model], "all", { key: "number", dir: "asc" }, true);
  assert.match(byNumber, /<th aria-sort="ascending">/);
  assert.match(byNumber, /data-board-sort-key="number"/);
  assert.match(byNumber, /data-board-sort-key="activity"/);
  assert.match(byNumber, />Task ↑</);
  assert.match(byNumber, />Dwell</, "the dwell column keeps its name while number sorts");
  const byActivity = renderBoardTable([model], "all", { key: "activity", dir: "desc" }, true);
  assert.match(byActivity, /<th class="col-dwell" aria-sort="descending">/);
  assert.match(byActivity, />Last active ↓</, "the dwell column is relabelled while activity sorts");
  const byDefault = renderBoardTable([model], "all", { key: "number", dir: "asc" });
  assert.match(byDefault, /sort: attention, then dwell/, "the unset default names the attention fallback");
  assert.doesNotMatch(byDefault, /sort: task #|sort: last active/, "the default does not claim a key sort");
  assert.doesNotMatch(byActivity, /sort: attention, then dwell/, "an explicit sort never claims the fixed attention order");
});

test("the table sort note names the effective key and direction", () => {
  const model = cardModel(entry({ task: { id: "t-0001" } }));
  assert.match(
    renderBoardTable([model], "all", { key: "number", dir: "asc" }, true),
    /sort: task # asc/,
  );
  assert.match(
    renderBoardTable([model], "all", { key: "number", dir: "desc" }, true),
    /sort: task # desc/,
  );
  assert.match(
    renderBoardTable([model], "all", { key: "activity", dir: "asc" }, true),
    /sort: last active asc/,
  );
  assert.match(
    renderBoardTable([model], "all", { key: "activity", dir: "desc" }, true),
    /sort: last active desc/,
  );
});

test("the table renders the now column as markdown", () => {
  const model = cardModel(
    entry({ card: { wait: { kind: "operator_intervention", reason: "execution_failed", message: "Failed on **line 42**" } } }),
  );
  const html = renderBoardTable([model], "all");
  assert.match(html, /<td class="col-now"><div class="md">/);
  assert.match(html, /<strong>line 42<\/strong>/);
});

// --- board sort -------------------------------------------------------------

function sortEntry(id, lane, dwellSince, project = { id: "p-alpha", name: "Alpha" }) {
  const entryData = {
    lane,
    task: { id, title: id, state: lane === "scheduled" ? "scheduled" : "in_progress" },
    card: { dwell_since: dwellSince },
    laneState: lane === "scheduled" ? "scheduled" : lane === "working" ? "working" : "blocked",
    blocked: false,
    project,
  };
  return { ...entryData, model: cardModel(entryData) };
}

function sortBoardEntries(dwellAgoMs = HOUR) {
  const now = Date.now();
  // The fixture arrives in the server's order — ascending task number per
  // lane — which the unset default keeps untouched in the lanes (a no-op);
  // the table's attention fallback reads the same cards oldest-dwell-first.
  return [
    sortEntry("t-0001", "scheduled", new Date(now - dwellAgoMs).toISOString()),
    sortEntry("t-0002", "scheduled", new Date(now - 2 * dwellAgoMs).toISOString()),
    sortEntry("t-0003", "working", new Date(now - 3 * dwellAgoMs).toISOString()),
    sortEntry("t-0004", "working", new Date(now - 4 * dwellAgoMs).toISOString()),
    sortEntry("t-0005", "waiting", new Date(now - 5 * dwellAgoMs).toISOString()),
    sortEntry("t-0006", "waiting", new Date(now - 6 * dwellAgoMs).toISOString()),
  ];
}

function lane(board, key) {
  // reconcile keys lanes via dataset.key, which is a plain property in the
  // test DOM rather than a data-* attribute.
  return [...board.querySelectorAll("flow-lane")].find((laneElement) => laneElement.dataset.key === key);
}

function laneIDs(board, key) {
  return lane(board, key).data.cards.map((model) => model.id);
}

test("compareBoardCards orders by last active and breaks ties by number", () => {
  const now = Date.now();
  const older = cardModel(entry({ task: { id: "t-0002" }, card: { dwell_since: new Date(now - 2 * HOUR).toISOString() } }));
  const newer = cardModel(entry({ task: { id: "t-0001" }, card: { dwell_since: new Date(now - HOUR).toISOString() } }));
  assert.equal(lastActivityMs(newer), now - HOUR);
  assert.deepEqual(
    compareBoardCards([older, newer], { key: "activity", dir: "asc" }).map((model) => model.id),
    ["t-0002", "t-0001"],
    "oldest activity sorts first ascending",
  );
  assert.deepEqual(
    compareBoardCards([older, newer], { key: "activity", dir: "desc" }).map((model) => model.id),
    ["t-0001", "t-0002"],
    "newest activity sorts first descending",
  );
  const tie = cardModel(entry({ task: { id: "t-0003" }, card: { dwell_since: new Date(now - HOUR).toISOString() } }));
  // Ties break by ascending task number in both directions: descending must
  // reorder by the key only, and equal keys keep t-0001 ahead of t-0003.
  assert.deepEqual(
    compareBoardCards([tie, newer], { key: "activity", dir: "desc" }).map((model) => model.id),
    ["t-0001", "t-0003"],
    "equal activity keeps the ascending-number tie-break when descending (t-0001 leads)",
  );
  assert.deepEqual(
    compareBoardCards([newer, tie], { key: "activity", dir: "asc" }).map((model) => model.id),
    ["t-0001", "t-0003"],
    "ascending breaks the same tie by ascending number",
  );
});

test("last active is the most recent of dwell, agent activity, and updated_at", () => {
  const now = Date.now();
  // A card the agent is actively working carries a newer last_agent_activity_at
  // than its dwell clock; it must sort as newer than a card with only an
  // older dwell clock.
  const working = cardModel(
    entry({
      task: { id: "t-0002" },
      card: {
        dwell_since: new Date(now - 2 * HOUR).toISOString(),
        last_agent_activity_at: new Date(now - 5 * 60_000).toISOString(),
      },
    }),
  );
  const idle = cardModel(entry({ task: { id: "t-0001" }, card: { dwell_since: new Date(now - HOUR).toISOString() } }));
  assert.equal(working.lastAgentActivityAt, new Date(now - 5 * 60_000).toISOString(), "cardModel projects the card's last agent activity");
  assert.equal(lastActivityMs(working), now - 5 * 60_000, "agent activity wins over the dwell clock");
  assert.deepEqual(
    compareBoardCards([working, idle], { key: "activity", dir: "desc" }).map((model) => model.id),
    ["t-0002", "t-0001"],
    "a card with newer agent activity sorts ahead of a card with an older dwell clock",
  );
  assert.deepEqual(
    compareBoardCards([working, idle], { key: "activity", dir: "asc" }).map((model) => model.id),
    ["t-0001", "t-0002"],
    "the same card sorts behind when ascending",
  );

  // A card with no dwell clock at all falls back to its task's updated_at
  // instead of comparing as 0, and a merely touched task outranks a stale one.
  const touched = cardModel(
    entry({
      task: { id: "t-0004", updated_at: new Date(now - 10 * 60_000).toISOString() },
      card: { dwell_since: "" },
    }),
  );
  const stale = cardModel(entry({ task: { id: "t-0003" }, card: { dwell_since: new Date(now - 3 * HOUR).toISOString() } }));
  assert.equal(touched.updatedAt, new Date(now - 10 * 60_000).toISOString(), "cardModel projects the task's updated_at");
  assert.equal(lastActivityMs(touched), now - 10 * 60_000, "a card without a dwell clock still has a last-active time");
  assert.deepEqual(
    compareBoardCards([touched, stale], { key: "activity", dir: "desc" }).map((model) => model.id),
    ["t-0004", "t-0003"],
    "a card with only a newer updated_at sorts ahead of an older dwell clock",
  );
  assert.equal(lastActivityMs({ id: "t-0005" }), 0, "a model with no timestamps at all compares as 0");
});

test("the sort control shows the active key highlighted with its direction", () => {
  const byDefault = renderBoardSort();
  assert.match(byDefault, />\s*Task number\s*</, "the default sort is Task number");
  assert.match(byDefault, />↑</);
  const byNumber = renderBoardSort({ key: "number", dir: "asc" });
  assert.match(byNumber, /data-board-sort-key/);
  assert.doesNotMatch(byNumber, /aria-pressed/, "the key button is not a pressed toggle: clicking it replaces it with the other key");
  assert.match(byNumber, /Currently sorting by Task number/, "the label states the active key");
  assert.match(byNumber, />\s*Task number\s*</);
  assert.match(byNumber, />↑</);
  assert.match(byNumber, /currently ascending/);
  const byActivity = renderBoardSort({ key: "activity", dir: "desc" });
  assert.match(byActivity, />\s*Last active\s*</);
  assert.match(byActivity, />↓</);
  assert.match(byActivity, /currently descending/);
});

test("the sort control cycles keys and toggles direction through real buttons", async () => {
  const root = globalThis.document.body;
  const control = mountElement(root, "flow-board-sort", { key: "number", dir: "asc" });
  await flush();
  const changes = [];
  control.addEventListener("board-sort-change", (event) => changes.push(event.detail));
  control.querySelector("[data-board-sort-key]").click();
  control.querySelector("[data-board-sort-dir]").click();
  assert.deepEqual(changes, [{ key: "activity" }, { dir: "desc" }]);
  control.remove();
});

test("the board sort control reorders both live lanes and persists", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", { entries: sortBoardEntries() });
  await flush();

  assert.deepEqual(laneIDs(board, "scheduled"), ["t-0001", "t-0002"], "the unset default is a no-op: the lanes keep the server's order");
  assert.deepEqual(laneIDs(board, "working"), ["t-0003", "t-0004"]);
  assert.deepEqual(laneIDs(board, "waiting"), ["t-0005", "t-0006"]);
  const control = board.querySelector("flow-board-sort");
  assert.match(control.innerHTML, />\s*Task number\s*</, "the control shows the active default key");
  assert.match(control.innerHTML, />↑</, "with the ascending direction");

  control.querySelector("[data-board-sort-key]").click();
  await flush();
  assert.deepEqual(laneIDs(board, "scheduled"), ["t-0002", "t-0001"], "activity asc puts the oldest first");
  assert.deepEqual(laneIDs(board, "working"), ["t-0004", "t-0003"]);
  assert.deepEqual(laneIDs(board, "waiting"), ["t-0006", "t-0005"]);
  assert.deepEqual(readBoardSort(), { key: "activity", dir: "asc" }, "the click persisted");

  board.querySelector("flow-board-sort").querySelector("[data-board-sort-dir]").click();
  await flush();
  assert.deepEqual(laneIDs(board, "scheduled"), ["t-0001", "t-0002"], "activity desc puts the newest first");
  assert.deepEqual(readBoardSort(), { key: "activity", dir: "desc" });
  board.remove();
});

test("a fresh board loads the persisted sort", async () => {
  writeBoardSort({ key: "activity", dir: "desc" });
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", { entries: sortBoardEntries() });
  await flush();
  assert.deepEqual(laneIDs(board, "scheduled"), ["t-0001", "t-0002"], "activity desc loads across a reload");
  board.remove();
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
});

test("the table view consumes the same sort and its headers set it back", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", { entries: sortBoardEntries() });
  await flush();

  board.querySelector('[data-board-view="table"]').click();
  await flush();
  const rowIDs = () =>
    [...board.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  assert.deepEqual(
    rowIDs(),
    ["t-0006", "t-0005", "t-0004", "t-0003", "t-0002", "t-0001"],
    "the table opens on the attention fallback (oldest dwell first)",
  );

  let table = board.querySelector("flow-board-table");
  assert.match(table.innerHTML, /sort: attention, then dwell/, "the unset default names the attention fallback");
  assert.match(table.innerHTML, /<th aria-sort="ascending">/, "the Task column shows the active direction");
  assert.match(table.innerHTML, />Task ↑</);
  assert.match(table.innerHTML, />Dwell</, "the dwell column keeps its name while number sorts");
  table.querySelector('[data-board-sort-key="activity"]').click();
  await flush();
  assert.deepEqual(rowIDs(), ["t-0006", "t-0005", "t-0004", "t-0003", "t-0002", "t-0001"], "the table header sets the shared sort, oldest activity first");
  assert.deepEqual(readBoardSort(), { key: "activity", dir: "asc" });
  table = board.querySelector("flow-board-table");
  assert.match(table.innerHTML, />\s*Last active/, "the dwell header is relabelled while activity sorts");
  assert.match(table.innerHTML, /sort: last active asc/, "the note names the chosen key and direction");

  board.querySelector('[data-board-view="lanes"]').click();
  await flush();
  assert.deepEqual(laneIDs(board, "scheduled"), ["t-0002", "t-0001"], "the lanes pick up the table-chosen sort");
  board.remove();
});

test("the table's Last active column shows the timestamp it sorts by, not the dwell clock", () => {
  const now = Date.now();
  // This card's dwell clock is old, but its agent activity is fresh: the
  // activity sort ranks it by the fresh timestamp, so under the relabelled
  // "Last active" header the cell must show the fresh elapsed time — never
  // the unrelated older dwell duration.
  const active = cardModel(
    entry({
      task: { id: "t-0002" },
      card: {
        dwell_since: new Date(now - 2 * HOUR).toISOString(),
        last_agent_activity_at: new Date(now - 5 * 60_000).toISOString(),
      },
    }),
  );
  const quiet = cardModel(entry({ task: { id: "t-0001" }, card: { dwell_since: new Date(now - HOUR).toISOString() } }));
  assert.equal(active.lastActiveMs, now - 5 * 60_000, "lastActiveMs is the most recent of all signals");
  assert.equal(active.lastActive, "5m", "the projected Last active value derives from that timestamp");
  assert.equal(active.dwell, "2h", "the dwell clock itself stays the older value");

  const byActivity = renderBoardTable([active, quiet], "all", { key: "activity", dir: "desc" });
  assert.match(byActivity, />Last active/, "the header is relabelled while activity sorts");
  assert.match(byActivity, /<td class="col-dwell"[^>]*>5m<\/td>/, "the cell shows the sort timestamp (5m), not the dwell clock (2h)");
  assert.doesNotMatch(byActivity, />2h<\/td>/, "the unrelated dwell duration is not shown under Last active");

  const byNumber = renderBoardTable([active, quiet], "all", { key: "number", dir: "asc" });
  assert.match(byNumber, />Dwell</, "the column keeps its Dwell name while number sorts");
  assert.match(byNumber, />2h<\/td>/, "and the cell shows the dwell clock as before");
});

test("a table header sort keeps the active filter", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", { entries: sortBoardEntries() });
  await flush();

  board.querySelector('[data-board-view="table"]').click();
  await flush();
  const rowIDs = () =>
    [...board.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  const chip = board.querySelector('[data-board-filter="queued"]');

  chip.click();
  await flush();
  assert.deepEqual(rowIDs(), ["t-0002", "t-0001"], "the queued filter selects the scheduled tasks in the attention fallback order (oldest dwell first)");

  board.querySelector('[data-board-sort-key="activity"]').click();
  await flush();
  assert.deepEqual(rowIDs(), ["t-0002", "t-0001"], "the explicit activity sort keeps the same oldest-first order here");
  const chipAfter = board.querySelector('[data-board-filter="queued"]');
  assert.equal(chipAfter.getAttribute("aria-pressed"), "true", "the filter chip stays active after sorting");
  board.remove();
});

test("an unset or corrupt sort keeps the lanes' server order and the table's attention fallback", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  window.localStorage.removeItem(BOARD_VIEW_STORAGE_KEY);
  const root = globalThis.document.body;
  // The server sends the aggregate board project by project (Alpha before
  // Beta), and its ListTasks ordering does not sort keyed ids by trailing
  // task number. A global number sort would move t-beta-0002 ahead of
  // t-alpha-0003; the unset default keeps the lanes on that payload exactly
  // — while the table falls back to its attention grouping (oldest dwell
  // first: t-beta-0002, t-alpha-0001, t-alpha-0003) and the control and the
  // table headers still display the default Task number ascending state.
  const now = Date.now();
  const alpha = { id: "p-alpha", name: "Alpha" };
  const beta = { id: "p-beta", name: "Beta" };
  const grouped = [
    sortEntry("t-alpha-0001", "scheduled", new Date(now - 2 * HOUR).toISOString(), alpha),
    sortEntry("t-alpha-0003", "scheduled", new Date(now - HOUR).toISOString(), alpha),
    sortEntry("t-beta-0002", "scheduled", new Date(now - 3 * HOUR).toISOString(), beta),
  ];
  const board = mountElement(root, "flow-board", { entries: grouped });
  await flush();
  assert.deepEqual(
    laneIDs(board, "scheduled"),
    ["t-alpha-0001", "t-alpha-0003", "t-beta-0002"],
    "the lanes keep the server's project-grouped order — the default is a no-op there",
  );
  const control = board.querySelector("flow-board-sort");
  assert.match(control.innerHTML, />\s*Task number\s*</, "the control still displays the default key");
  assert.match(control.innerHTML, />↑</, "and the ascending direction");
  board.querySelector('[data-board-view="table"]').click();
  await flush();
  const rowIDs = () =>
    [...board.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  assert.deepEqual(
    rowIDs(),
    ["t-beta-0002", "t-alpha-0001", "t-alpha-0003"],
    "the table falls back to the attention grouping (oldest dwell first) instead",
  );
  assert.match(
    board.querySelector("flow-board-table").innerHTML,
    /sort: attention, then dwell/,
    "and the note names that fallback rather than a key sort",
  );
  assert.match(
    board.querySelector("flow-board-table").innerHTML,
    /<th aria-sort="ascending">/,
    "the Task column still shows the default direction",
  );
  board.remove();

  // A corrupt stored value is treated as unset: the same default applies.
  window.localStorage.setItem(BOARD_SORT_STORAGE_KEY, "{not json");
  window.localStorage.removeItem(BOARD_VIEW_STORAGE_KEY);
  const corrupt = mountElement(root, "flow-board", { entries: grouped });
  await flush();
  assert.deepEqual(
    laneIDs(corrupt, "scheduled"),
    ["t-alpha-0001", "t-alpha-0003", "t-beta-0002"],
    "a corrupt preference falls back to the no-op default in the lanes",
  );
  corrupt.remove();
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
});

test("a user sort is cross-project, persists, and survives a reload", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  window.localStorage.removeItem(BOARD_VIEW_STORAGE_KEY);
  const root = globalThis.document.body;
  const now = Date.now();
  const alpha = { id: "p-alpha", name: "Alpha" };
  const beta = { id: "p-beta", name: "Beta" };
  const grouped = [
    sortEntry("t-alpha-0001", "scheduled", new Date(now - 2 * HOUR).toISOString(), alpha),
    sortEntry("t-alpha-0003", "scheduled", new Date(now - HOUR).toISOString(), alpha),
    sortEntry("t-beta-0002", "scheduled", new Date(now - 3 * HOUR).toISOString(), beta),
  ];
  const board = mountElement(root, "flow-board", { entries: grouped });
  await flush();

  // The board opens with the unset default: the lanes stay on the server's
  // project-grouped order and the table falls back to attention. Clicking the
  // Task header is an explicit user sort, so it applies cross-project from
  // then on.
  board.querySelector('[data-board-view="table"]').click();
  await flush();
  const rowIDs = () =>
    [...board.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  assert.deepEqual(
    rowIDs(),
    ["t-beta-0002", "t-alpha-0001", "t-alpha-0003"],
    "the table opens on the attention fallback (oldest dwell first)",
  );
  board.querySelector('[data-board-sort-key="number"]').click();
  await flush();
  assert.deepEqual(
    rowIDs(),
    ["t-alpha-0003", "t-beta-0002", "t-alpha-0001"],
    "clicking the Task header reverses the direction cross-project",
  );
  assert.deepEqual(readBoardSort(), { key: "number", dir: "desc" }, "the reversal is persisted");
  assert.match(
    board.querySelector("flow-board-table").innerHTML,
    /sort: task # desc/,
    "the note switches to the explicit key and direction",
  );

  board.querySelector('[data-board-view="lanes"]').click();
  await flush();
  assert.deepEqual(
    laneIDs(board, "scheduled"),
    ["t-alpha-0003", "t-beta-0002", "t-alpha-0001"],
    "the lanes apply the same cross-project sort",
  );
  board.remove();

  // Reload: the persisted sort still applies cross-project.
  const reloaded = mountElement(root, "flow-board", { entries: grouped });
  await flush();
  assert.deepEqual(
    laneIDs(reloaded, "scheduled"),
    ["t-alpha-0003", "t-beta-0002", "t-alpha-0001"],
    "the persisted sort survives a reload and stays cross-project",
  );

  // An explicit sort keeps applying cross-project even when it lands back on
  // the default key and direction: once the user has picked a sort, ascending
  // number is a comparator sort — unlike the unset no-op default.
  reloaded.querySelector('[data-board-view="table"]').click();
  await flush();
  const reloadedRows = () =>
    [...reloaded.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  reloaded.querySelector('[data-board-sort-key="number"]').click();
  await flush();
  assert.deepEqual(
    reloadedRows(),
    ["t-alpha-0001", "t-beta-0002", "t-alpha-0003"],
    "an explicit ascending number sort reorders cross-project",
  );
  assert.deepEqual(readBoardSort(), { key: "number", dir: "asc" }, "the explicit ascending choice is persisted");
  reloaded.remove();
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
});

test("a poll that replaces board data refreshes the mounted lanes and the table", async () => {
  window.localStorage.removeItem(BOARD_SORT_STORAGE_KEY);
  window.localStorage.removeItem(BOARD_VIEW_STORAGE_KEY);
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", { entries: sortBoardEntries() });
  await flush();

  // Activate Last active so the poll has a sort to apply to both views.
  board.querySelector("flow-board-sort").querySelector("[data-board-sort-key]").click();
  await flush();
  const cardIDs = (key) =>
    [...lane(board, key).querySelectorAll("flow-task-card")].map((card) => card.data.id);
  assert.deepEqual(cardIDs("scheduled"), ["t-0002", "t-0001"], "activity asc puts the oldest first");

  // The route only replaces board.data on a poll; the header markup never
  // changes, so the base paint skips the write. The mounted lanes must still
  // re-sort and refresh their cards: t-0001's activity ages past t-0002's.
  const now = Date.now();
  const aged = sortBoardEntries();
  aged[0] = sortEntry("t-0001", "scheduled", new Date(now - 3 * HOUR).toISOString());
  board.data = { entries: aged };
  await flush();
  assert.deepEqual(
    cardIDs("scheduled"),
    ["t-0001", "t-0002"],
    "a poll re-applies the sort to the mounted lanes without any interaction",
  );

  // Same poll with the table mounted: opening the table on the polled data,
  // then replacing the data again, must reorder the rows in place.
  board.querySelector('[data-board-view="table"]').click();
  await flush();
  const rowIDs = () =>
    [...board.querySelector("tbody").children].map((row) => row.querySelector(".id").textContent);
  assert.deepEqual(
    rowIDs(),
    ["t-0006", "t-0005", "t-0004", "t-0001", "t-0003", "t-0002"],
    "the table opens on the polled, sorted data",
  );

  const refreshed = sortBoardEntries();
  refreshed[0] = sortEntry("t-0001", "scheduled", new Date(now - HOUR).toISOString());
  board.data = { entries: refreshed };
  await flush();
  assert.deepEqual(
    rowIDs(),
    ["t-0006", "t-0005", "t-0004", "t-0003", "t-0002", "t-0001"],
    "a poll re-applies the sort to the mounted table without any interaction",
  );
  board.remove();
});

