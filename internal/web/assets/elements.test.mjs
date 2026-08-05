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

// --- task detail -----------------------------------------------------------

test("the run spine gives a repeat visit its own row rather than a crossing edge", () => {
  const rows = runRows({
    run: { snapshot: { nodes: [{ key: "implement", name: "Implement" }, { key: "checks", name: "Checks" }] } },
    node_runs: [
      { id: "n1", node_key: "implement", visit: 1, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
      { id: "n2", node_key: "checks", visit: 1, state: "failed", outcome: "failed", kind: "automated_checks", name: "Checks" },
      { id: "n3", node_key: "implement", visit: 2, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
    ],
  });
  assert.equal(rows.length, 3);
  assert.equal(rows[2].tag, "visit 2");
  assert.match(rows[1].loop, /looped back to Implement/);
});

test("the run spine names the nodes still ahead", () => {
  const rows = runRows({
    run: { snapshot: { nodes: [{ key: "a", name: "A" }, { key: "b", name: "B" }] } },
    node_runs: [{ id: "n1", node_key: "a", visit: 1, state: "succeeded", name: "A" }],
  });
  assert.equal(rows.length, 2);
  assert.equal(rows[1].state, "future");
});

test("the run spine marks the visit the run is on now as current after a loop-back", () => {
  // Regression: the spine collapsed repeat visits by keeping the *first* one,
  // so a merge conflict that looped the run back to implement left every node
  // reading "done" and none current — contradicting the Overview tab, which
  // showed the run implementing.
  const rows = runRows({
    run: {
      current_node_run_id: "n3",
      snapshot: {
        nodes: [
          { key: "implement", name: "Implement" },
          { key: "merge", name: "Merge change" },
          { key: "merged", name: "Merged" },
        ],
      },
    },
    node_runs: [
      { id: "n1", node_key: "implement", visit: 1, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
      { id: "n2", node_key: "merge", visit: 1, state: "succeeded", outcome: "conflict", kind: "merge_change", name: "Merge change" },
      { id: "n3", node_key: "implement", visit: 2, state: "running", kind: "agent", name: "Implement" },
    ],
  });
  const html = renderRunSpine({ rows, runSequence: 1 });
  // One row per node, kept in graph order even though implement's latest
  // visit is its second.
  assert.equal((html.match(/<li /g) || []).length, 3);
  const states = [...html.matchAll(/<li data-state="([^"]+)">\s*<span class="dot"><\/span>\s*<span class="name">([^<]+)<\/span>/g)].map(
    (match) => [match[2], match[1]],
  );
  assert.deepEqual(states, [
    ["Implement", "current"],
    ["Merge change", "done"],
    ["Merged", "future"],
  ]);
});

test("the current run row lists its fanned-out agents, not just the node", () => {
  const html = renderRunList([
    {
      id: "n1",
      name: "Review the change",
      tag: "change review",
      state: "current",
      duration: "12m",
      jobs: [
        { id: "j-0912", name: "security-review", state: "running" },
        { id: "j-0913", name: "performance-review", state: "finished", verdict: "satisfied" },
      ],
      loop: "",
    },
  ]);
  assert.match(html, /security-review/);
  assert.match(html, /performance-review/);
  assert.match(html, /satisfied/);
});

test("the graph shows a visit count only when there was more than one visit", () => {
  const model = {
    snapshot: {
      nodes: [
        { key: "implement", name: "Implement", kind: "agent" },
        { key: "checks", name: "Checks", kind: "automated_checks" },
      ],
      edges: [{ from: "implement", outcome: "completed", to: "checks" }],
    },
    run: { current_node_key: "checks" },
    transitionCounts: [{ from: "implement", outcome: "completed", to: "checks", count: 1 }],
  };
  assert.ok(!renderWorkflowGraph(model).includes("×1"), "a single visit should not be annotated");

  model.transitionCounts = [{ from: "implement", outcome: "completed", to: "checks", count: 2 }];
  assert.match(renderWorkflowGraph(model), /×2/);
});

test("back edges route into their own channel below the row", () => {
  const html = renderWorkflowGraph({
    snapshot: {
      nodes: [
        { key: "implement", name: "Implement", kind: "agent" },
        { key: "checks", name: "Checks", kind: "automated_checks" },
        { key: "review", name: "Review", kind: "change_review" },
      ],
      edges: [
        { from: "implement", outcome: "completed", to: "checks" },
        { from: "checks", outcome: "failed", to: "implement" },
        { from: "review", outcome: "changes_requested", to: "implement" },
      ],
    },
    run: { current_node_key: "review" },
    transitionCounts: [],
  });
  // Two back edges, each dropping to its own y before returning: no shared
  // lane means no edge can cross a node.
  const channels = [...html.matchAll(/V(\d+) H/g)].map((match) => match[1]);
  assert.equal(new Set(channels).size, channels.length, `back edges share a channel: ${channels}`);
  assert.equal(channels.length, 2);
});

test("graph counts fold per-edge tallies into per-node visits", () => {
  const { nodeVisits, takenEdges } = graphCounts([
    { from: "a", outcome: "ok", to: "b", count: 2 },
    { from: "b", outcome: "fail", to: "a", count: 1 },
  ]);
  assert.equal(nodeVisits.get("b"), 2);
  assert.equal(takenEdges.get("a|ok|b"), 2);
});

test("the Now card's review-thread age dwells on the task model clock, not the wall clock", () => {
  // A fixed model clock far from the real wall clock: the Now card's thread
  // age must use the same `now` as the rest of the model, or the two drift
  // apart (here the real clock would report the thread as days old while the
  // model says 1h).
  const now = Date.parse("2026-07-29T12:00:00Z");
  const model = taskModel(
    {
      task: { id: "t-0001", title: "Fix the thing" },
      task_detail: {},
      threads: [
        {
          id: "th-0021",
          state: "open",
          created_at: "2026-07-29T11:00:00Z",
          file_path: "internal/lifecycle/engine.go",
          line: 212,
          comments: [{ body: "Pausing for the operator leaves the lease live." }],
        },
      ],
    },
    null,
    { now },
  );
  const card = nowCardModel(model);
  assert.equal(card.heading, "Now · 1 review thread block the merge");
  assert.equal(card.age, "1h", "thread age must use the model clock");
});

test("the Now card renders for an open wait and stays away otherwise", () => {
  const base = { activity: "Reviewing the change", stepName: "review", dwell: "12m", threads: [], openThreads: 0 };
  assert.equal(nowCardModel({ ...base, wait: null }), null);
  const card = nowCardModel({ ...base, wait: { message: "Ship it?" }, waitKind: "gate" });
  assert.equal(card.tone, "await");
  assert.equal(card.body, "Ship it?");
});

test("the Now card surfaces a review thread that blocks the merge", () => {
  const card = nowCardModel({
    wait: null,
    openThreads: 1,
    threads: [
      {
        id: "th-0021",
        state: "open",
        file_path: "internal/lifecycle/engine.go",
        line: 212,
        anchor_commit_sha: "old",
        comments: [{ body: "Pausing for the operator leaves the lease live." }],
      },
    ],
    change: { head_sha: "new" },
  });
  assert.match(card.heading, /1 review thread/);
  assert.equal(card.locus, "internal/lifecycle/engine.go:212");
  assert.ok(card.outdated, "an anchor behind the head must be flagged");
});

test("an anchor matching the head is not flagged as outdated", () => {
  assert.ok(!isOutdatedAnchor({ anchor_commit_sha: "abc" }, { head_sha: "abc" }));
  assert.ok(!isOutdatedAnchor({}, { head_sha: "abc" }));
});

test("tab badges are toned by whether the number is good news", () => {
  const badges = tabBadges({
    change: { id: "c-1" },
    openThreads: 1,
    checks: [{ verdict: "satisfied" }, { verdict: "blocked" }],
    checksSatisfied: 1,
    transitions: [{}],
    statusLog: [],
    terminalAvailable: true,
  });
  assert.equal(badges.change.tone, "warn");
  assert.equal(badges.checks.text, "1/2");
  assert.equal(badges.checks.tone, "danger");
  assert.ok(badges.terminal.live);
});

test("a failed check row carries the disclosure, retry and skip", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "unit", verdict: "satisfied", details: "go test ./..." },
      { name: "lifecycle-test", verdict: "blocked", source_job_id: "j-0912", exit_code: 1 },
    ],
  });
  assert.match(html, /data-transcript-toggle="lifecycle-test"/);
  assert.match(html, /data-failed/);
  assert.match(html, /Retry/);
  assert.match(html, /Skip/);
});

test("the check list renders details as block markdown but keeps job id and exit code escaped", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "reviewer", verdict: "blocked", details: "**Overall**: needs work", source_job_id: "j-<_1", exit_code: 1 },
    ],
  });
  assert.match(html, /<div class="detail">\s*<div class="md"><p><strong>Overall<\/strong>: needs work<\/p><\/div>\s*<span class="meta">j-&lt;_1 · exit 1<\/span>\s*<\/div>/);
});

test("multi-paragraph check details render as block markdown, not one collapsed line", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      {
        name: "reviewer",
        verdict: "blocked",
        details:
          "**Overall**: needs work\n\nPlease fix the escaping in renderCheckRow before merging.\n\n- fix the escaping in renderCheckRow\n- add a regression test",
        source_job_id: "j-0912",
        exit_code: 1,
      },
    ],
  });
  const detail = html.match(/<div class="detail">([\s\S]*?)<\/div>\s*<span class="actions">/);
  assert.ok(detail, "the detail column wraps the block markdown");
  assert.match(detail[1], /<div class="md"><p><strong>Overall<\/strong>: needs work<\/p>/);
  assert.match(detail[1], /<p>Please fix the escaping in renderCheckRow before merging\.<\/p>/);
  assert.match(detail[1], /<ul>\s*<li>fix the escaping in renderCheckRow<\/li>\s*<li>add a regression test<\/li>\s*<\/ul>/);
  assert.doesNotMatch(detail[1], /needs work Please fix the escaping/);
  assert.doesNotMatch(detail[1], /needs work · fix the escaping/);
  assert.match(detail[1], /<span class="meta">j-0912 · exit 1<\/span>/);
  assert.match(html, /data-transcript-toggle="reviewer"/);
  assert.match(html, /Retry/);
  assert.match(html, /Skip/);
});

test("satisfied check rows still show the duration next to block details", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "unit", verdict: "satisfied", details: "go test ./...", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:03:00Z" },
    ],
  });
  assert.match(html, /<div class="md"><p>go test \.\/...<\/p><\/div>/);
  assert.match(html, /<span class="duration">3m 0s<\/span>/);
});

// --- findings registry ------------------------------------------------------

function findingsRegistry(overrides = {}) {
  const { findings = [], follow_ups = [], summary = {} } = overrides;
  return { findings, follow_ups, summary };
}

test("the findings view renders one row per finding with resolution labels and summary counts", () => {
  const html = renderFindings({
    projectID: "p-1",
    findings: findingsRegistry({
      findings: [
        { id: "th-1", change_id: "ch-1", file_path: "src/a.go", line: 12, state: "claimed", claim_kind: "fixed", claimed_by: "alice", finding: "**leak** on line 12" },
        { id: "th-2", change_id: "ch-1", file_path: "src/b.go", line: 3, state: "claimed", claim_kind: "not_warranted", claimed_by: "bob", finding: "style nit" },
        { id: "th-3", change_id: "ch-2", file_path: "src/c.go", line: 0, state: "certified", certified_by: "carol", finding: "handled elsewhere" },
        { id: "th-4", change_id: "ch-2", file_path: "", line: 0, state: "open", finding: "still open" },
      ],
      summary: { resolved_fixed: 1, resolved_not_warranted: 1, certified: 1, unresolved: 1 },
    }),
  });
  // One row per finding, in registry order.
  assert.match(html, /data-finding="th-1"/);
  assert.match(html, /data-finding="th-2"/);
  assert.match(html, /data-finding="th-3"/);
  assert.match(html, /data-finding="th-4"/);
  assert.ok(html.indexOf('data-finding="th-1"') < html.indexOf('data-finding="th-2"'));
  // Resolution labels: claim kind + actor, certified-by, unresolved.
  assert.match(html, /fixed by alice/);
  assert.match(html, /not warranted by bob/);
  assert.match(html, /certified by carol/);
  assert.match(html, />unresolved<\/span>/);
  // State badges.
  assert.match(html, /data-state="claimed"/);
  assert.match(html, /data-state="certified"/);
  assert.match(html, /data-state="open"/);
  // File:line anchor into the change, when present.
  assert.match(html, /<a class="anchor" href="\/ui\/changes\/ch-1" data-link>src\/a.go:12<\/a>/);
  // Finding bodies render as block markdown.
  assert.match(html, /<strong>leak<\/strong>/);
  // The summary line carries a count per resolution bucket, zeroes included.
  assert.match(html, /data-bucket="resolved_fixed">fixed 1<\/span>/);
  assert.match(html, /data-bucket="resolved_not_warranted">not warranted 1<\/span>/);
  assert.match(html, /data-bucket="resolved_superseded">superseded 0<\/span>/);
  assert.match(html, /data-bucket="certified">certified 1<\/span>/);
  assert.match(html, /data-bucket="unresolved">unresolved 1<\/span>/);
  assert.match(html, /data-bucket="deferred_to_task">deferred 0<\/span>/);
});

test("deferred follow-up findings render links to their target tasks", () => {
  const html = renderFindings({
    projectID: "p-1",
    findings: findingsRegistry({
      findings: [{ id: "th-1", state: "open", finding: "still open" }],
      follow_ups: [
        { action: "create_task", check_name: "review-aggregator", target_task_id: "t-0099", target_task_title: "Handle lint fallout" },
      ],
      summary: { unresolved: 1, deferred_to_task: 1 },
    }),
  });
  assert.match(html, /deferred to/);
  assert.match(html, /href="\/ui\/tasks\/t-0099" data-link/);
  assert.match(html, /Handle lint fallout/);
  assert.match(html, /t-0099/);
  // The follow-ups section only appears when something was deferred.
  assert.match(html, /<h4>Follow-ups<\/h4>/);
  const noDeferrals = renderFindings({
    findings: findingsRegistry({ findings: [{ id: "th-1", state: "open", finding: "x" }], summary: { unresolved: 1 } }),
  });
  assert.doesNotMatch(noDeferrals, /Follow-ups/);
});

test("an empty findings registry renders the empty state, not an error", () => {
  assert.match(renderFindings({ findings: findingsRegistry() }), /No review findings recorded/);
  // A task with zero changes carries no registry at all.
  assert.match(renderFindings({}), /No review findings recorded/);
  assert.match(renderFindings(null), /No review findings recorded/);
  // A failed read is its own state, not a false "nothing found".
  assert.match(renderFindings({ findings: { error: "findings load failed" } }), /Findings unavailable/);
});

test("finding bodies render as block markdown and escape raw HTML", () => {
  const html = renderFindings({
    findings: findingsRegistry({
      findings: [{ id: "th-1", state: "open", finding: "First line\n\n<script>alert(1)</script> **bold**" }],
      summary: { unresolved: 1 },
    }),
  });
  assert.match(html, /<div class="md">/);
  assert.ok(!html.includes("<script>"), "raw HTML in a finding body must be neutralized");
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(html, /<strong>bold<\/strong>/);
});

test("the findings element keeps its instance across a poll repaint", async () => {
  const root = globalThis.document.body;
  const registry = () => findingsRegistry({
    findings: [{ id: "th-1", state: "open", finding: "body" }],
    summary: { unresolved: 1 },
  });
  const element = mountElement(root, "flow-findings-list", { projectID: "p-1", findings: registry() });
  await flush();
  assert.match(element.innerHTML, /data-finding="th-1"/);
  // A poll delivers a brand-new model for the same task; the element instance
  // survives and repaints in place instead of being rebuilt.
  const before = element;
  element.data = { projectID: "p-1", findings: registry() };
  await flush();
  assert.strictEqual(element, before);
  assert.match(element.innerHTML, /data-finding="th-1"/);
  element.remove();
});

test("the findings tab sits next to checks and tab badges count unresolved findings", () => {
  const strip = renderTabStrip("checks", { findings: { text: "2", tone: "warn" } });
  assert.ok(strip.indexOf('data-tab="checks"') < strip.indexOf('data-tab="findings"'));
  assert.match(strip, /data-tab="findings">\s*Findings/);

  const model = taskModel({
    task: { id: "t-1", title: "T" },
    task_detail: {},
    findings: { findings: [], follow_ups: [], summary: { unresolved: 2 } },
  });
  assert.equal(tabBadges(model).findings.text, "2");
  assert.equal(tabBadges(model).findings.tone, "warn");
  const clean = taskModel({ task: { id: "t-1", title: "T" }, task_detail: {} });
  assert.equal(tabBadges(clean).findings, undefined);
});

test("handing back names every edge the executor can take", () => {
  const html = renderHeldPanel({
    held: true,
    id: "t-0042",
    stepName: "review",
    taskConsole: { session: { id: "s-0413", worker_id: "w-local-1" } },
  });
  assert.match(html, /Held by you/);
  assert.match(html, /paused at review · the workflow will not advance/);
  for (const [edge] of HAND_BACK_EDGES) {
    assert.match(html, new RegExp(`data-edge="${edge}"`), `missing hand-back edge ${edge}`);
  }
  assert.match(html, /s-0413 · w-local-1 · tmux/);
});

test("a typed convergence hold renders immutable evidence and explicit dispositions", () => {
  const html = renderHeldPanel({
    held: true,
    heldBy: "system",
    id: "t-0043",
    stepName: "review",
    convergenceEvidence: {
      schema_version: 1,
      fingerprint: "sha256:reviewed-evidence",
      source_branch: "task/t-0043",
      source_head_sha: "1234567890abcdef1234567890abcdef12345678",
      target_base_branch: "main",
      target_base_tip_sha: "abcdef1234567890abcdef1234567890abcdef12",
      files: 8,
      additions: 420,
      deletions: 160,
      review_cycles_used: 2,
      review_cycle_budget: 3,
      changed_files: [{ path: "internal/<unsafe>.go", additions: 200, deletions: 10 }],
      changed_files_omitted: 7,
    },
  });
  assert.match(html, /Convergence review/);
  assert.match(html, /task\/t-0043@1234567890ab/);
  assert.match(html, /main@abcdef123456/);
  assert.match(html, /8 files · \+420\/-160/);
  assert.match(html, /internal\/&lt;unsafe&gt;\.go/);
  assert.match(html, /data-convergence-note/);
  assert.equal((html.match(/data-evidence-fingerprint="sha256:reviewed-evidence"/g) || []).length, 4);
  for (const disposition of ["accept_scope", "repair_branch", "promote", "cancel"]) {
    assert.match(html, new RegExp(`data-disposition="${disposition}"`), `missing disposition ${disposition}`);
  }
  assert.doesNotMatch(html, /data-workflow-release/);
});

test("held_by system alone does not masquerade as typed convergence evidence", () => {
  const html = renderHeldPanel({ held: true, heldBy: "system", id: "t-0043", stepName: "review" });
  assert.match(html, /Held by you/);
  assert.match(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-convergence-decision/);
});

test("task rail sends typed convergence holds to their dedicated review panel", () => {
  const html = renderTaskRail({
    id: "t-0043",
    runID: "wr-0043",
    held: true,
    stepCount: 3,
    stepName: "review",
    convergenceEvidence: { schema_version: 1, fingerprint: "sha256:evidence" },
  });
  assert.match(html, /Scope decision required at review/);
  assert.match(html, /use the review panel to continue/);
  assert.doesNotMatch(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-workflow-reset/);
});

test("task rail suppresses invalid controls while typed evidence is temporarily unavailable", () => {
  const html = renderTaskRail({
    id: "t-0043",
    runID: "wr-0043",
    held: true,
    heldBy: "system",
    systemHeld: true,
    canRequestConvergence: true,
    stepCount: 3,
    stepName: "review",
  });
  assert.match(html, /Review state is refreshing at review/);
  assert.doesNotMatch(html, /data-convergence-request/);
  assert.doesNotMatch(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-workflow-reset/);
});

test("task model projects exact manual convergence eligibility and typed evidence", () => {
  const eligible = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "abc123" }] },
  }, {
    detail: { run: { id: "wr-0042", current_artifact_id: "wa-1", snapshot: { nodes: [] } } },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(eligible.canRequestConvergence, true);

  const stale = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "new-head" }] },
  }, {
    detail: { run: { id: "wr-0042", current_artifact_id: "wa-1", snapshot: { nodes: [] } } },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(stale.canRequestConvergence, false);

  const systemHeld = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "abc123" }] },
  }, {
    detail: {
      run: {
        id: "wr-0042",
        current_artifact_id: "wa-1",
        held_at: "2026-07-31T17:00:00Z",
        held_by: "system",
        snapshot: { nodes: [] },
      },
    },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(systemHeld.systemHeld, true);
  assert.equal(systemHeld.canRequestConvergence, false);

  const evidence = { schema_version: 1, fingerprint: "sha256:evidence" };
  const model = taskModel({
    task: { id: "t-0043", title: "Oversized" },
    task_detail: { convergence_evidence: evidence },
  }, {
    detail: { run: { held_at: "2026-07-30T12:00:00Z", held_by: "system", snapshot: { nodes: [] } } },
  });
  assert.equal(model.convergenceEvidence, evidence);
  assert.equal(model.activity, "Held for convergence review");
});

test("the activity feed merges transitions and status entries newest first", () => {
  const entries = activityEntries({
    transitions: [
      { event_kind: "node_completed", from_node_key: "checks", to_node_key: "review", outcome: "passed", created_at: "2026-01-01T10:00:00Z" },
    ],
    statusLog: [{ kind: "note", message: "Implementing the retry budget", created_at: "2026-01-01T09:00:00Z" }],
  });
  assert.equal(entries.length, 2);
  assert.equal(entries[0].text, "checks → review · passed");
  assert.match(renderActivityFeed({ transitions: [], statusLog: [] }), /No activity yet/);
});

// --- change review ---------------------------------------------------------

test("every diff line carries a comment affordance in its gutter", () => {
  const html = renderDiffFile({
    path: "internal/lifecycle/engine.go",
    hunks: [
      {
        header: "@@ -210,6 +210,8 @@",
        lines: [
          { kind: "context", new_line: 210, text: "if run.Budget <= 0 {" },
          { kind: "delete", old_line: 211, text: "  return ErrBudgetExhausted" },
          { kind: "add", new_line: 211, text: "  return e.pauseForOperator(ctx, run)" },
        ],
      },
    ],
  });
  assert.equal((html.match(/data-comment-line=/g) || []).length, 3);
  assert.match(html, /aria-label="Comment on line 210"/);
});

test("a deleted line keeps its minus mark and deletion styling hook", () => {
  // The server (internal/git parseDiffLine) emits kind "delete" for '-' lines;
  // the gutter mark and the diff.module.css [data-kind="delete"] rules both
  // hang off that exact string.
  const html = renderDiffFile({
    path: "a.go",
    hunks: [
      {
        header: "@@ -1,2 +1,2 @@",
        lines: [
          { kind: "delete", old_line: 1, text: "old" },
          { kind: "add", new_line: 1, text: "new" },
        ],
      },
    ],
  });
  assert.match(html, /<div class="line" data-kind="delete">[\s\S]*?>−<\/button>/);
  assert.match(html, /<div class="line" data-kind="add">[\s\S]*?>\+<\/button>/);
});

test("a drafted line keeps its note and says so", () => {
  const drafts = new Map([["a.go:12", { path: "a.go", line: 12, body: "cap the wait" }]]);
  const html = renderDiffFile(
    { path: "a.go", hunks: [{ header: "@@", lines: [{ kind: "context", new_line: 12, text: "x" }] }] },
    { drafts },
  );
  assert.match(html, /data-drafted/);
  assert.match(html, /comment on line 12/);
  assert.match(html, /cap the wait/);
});

test("the review bar states how many notes ride along with the verdict", () => {
  assert.match(renderReviewBar(3), /3 pending inline notes will be posted with this/);
  assert.match(renderReviewBar(1), /1 pending inline note will be posted/);
  assert.match(renderReviewBar(0), /placeholder="Overall comment"/);
});

// --- epic ------------------------------------------------------------------

test("epic members reduce to the one word the rollup groups by", () => {
  assert.equal(memberState({ needs_you: true }), "needs you");
  assert.equal(memberState({ resolution: "merged" }), "merged");
  assert.equal(memberState({ blocked_by: ["t-1"] }), "blocked");
  assert.equal(memberState({ state: "in_progress" }), "working");
});

test("feature count and divergence labels compress to one short legend", () => {
  assert.equal(featureCountsLabel({ open: 1, scheduled: 2, in_progress: 1, done: 3 }), "1 open · 2 scheduled · 1 working · 3 done");
  assert.equal(featureCountsLabel({}), "no tasks");
  assert.equal(featureDivergenceLabel({ ahead: 0, behind: 0 }), "up to date");
  assert.equal(featureDivergenceLabel({ ahead: 3, behind: 2 }), "3 ahead · 2 behind");
  assert.equal(featureDivergenceLabel(null), "");
});

test("features list sorts open before landed and renders counts, divergence and the create form", () => {
  const html = renderFeatures({
    projectID: "p-alpha",
    features: [
      {
        feature: { id: "f-alpha-0002", title: "billing rework", status: "landed", landed_at: "2026-01-01T00:00:00Z" },
        counts: { done: 4 },
        branch_state: { ahead: 0, behind: 0 },
      },
      {
        feature: { id: "f-alpha-0001", title: "payments", status: "open" },
        counts: { open: 2, in_progress: 1 },
        branch_state: { ahead: 3, behind: 2 },
        running_rebase: { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009" },
      },
    ],
  });
  const openIndex = html.indexOf("payments");
  const landedIndex = html.indexOf("billing rework");
  assert.ok(openIndex !== -1 && landedIndex !== -1 && openIndex < landedIndex, "open feature sorts first");
  assert.match(html, /href="\/ui\/projects\/p-alpha\/features\/f-alpha-0001"/);
  assert.match(html, /2 open · 1 working/);
  assert.match(html, /3 ahead · 2 behind/);
  assert.match(html, /rebasing/);
  assert.match(html, /data-feature-form="" data-project="p-alpha"/);
  assert.match(html, /data-landed/);
});

test("feature detail renders actions for an open feature and none once landed", () => {
  const open = renderFeature({
    projectID: "p-alpha",
    feature: { id: "f-alpha-0001", title: "payments", status: "open", branch: "feature/f-alpha-0001", body: "payment work" },
    counts: { open: 1 },
    branch_state: { ahead: 1, behind: 2 },
    running_rebase: { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009" },
    tasks: [
      { id: "t-alpha-0003", title: "scoped work" },
      { id: "t-alpha-0004", title: "merged work", state: "done", done_resolution: "merged" },
    ],
    rebases: [
      { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009", old_tip_sha: "abc123", created_at: "2026-01-01T00:00:00Z" },
    ],
  });
  assert.match(open, /data-feature-rebase="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /data-feature-land="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /data-feature-archive="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /Rebase in progress/);
  assert.match(open, /href="\/ui\/projects\/p-alpha\/tasks\/t-alpha-0009\?context=f-alpha-0001&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Ffeatures%2Ff-alpha-0001"/);
  assert.match(open, /done \(merged\)/);
  assert.match(open, /data-feature-form="f-alpha-0001" data-project="p-alpha"/);

  const landed = renderFeature({
    projectID: "p-alpha",
    feature: { id: "f-alpha-0001", title: "payments", status: "landed", branch: "feature/f-alpha-0001", landed_at: "2026-01-02T00:00:00Z", land_sha: "def456" },
    counts: { done: 2 },
  });
  assert.doesNotMatch(landed, /data-feature-rebase/);
  assert.doesNotMatch(landed, /data-feature-form/);
  assert.match(landed, /landed/);
});

test("the epic rollup bar and its legend are built from one grouping", () => {
  const html = renderEpic({
    epic: { id: "t-0030", title: "Lifecycle hardening" },
    total_count: 3,
    merged_count: 1,
    members: [
      { id: "t-0039", title: "A", needs_you: true, dwell_since: new Date(Date.now() - 41 * 60_000).toISOString() },
      { id: "t-0036", title: "B", resolution: "merged" },
      { id: "t-0043", title: "C", state: "in_progress", step_index: 1, step_count: 6, step_name: "implement" },
    ],
    critical_path: ["t-0036", "t-0043"],
  });
  assert.match(html, /1 needs you/);
  assert.match(html, /1 merged/);
  assert.match(html, /data-merged/);
  assert.match(html, /implement 1\/6/);
  assert.match(html, /Critical path/);
});

test("legacy epic member and critical-path task links preserve epic context when route-mounted", () => {
  const html = renderEpic({
    projectID: "p-alpha",
    currentHref: "/ui/projects/p-alpha/epics/e-1?return=%2Fui%2Fprojects%2Fp-alpha%2Fwork-items",
    epic: { id: "e-1", title: "Legacy epic" },
    members: [{ id: "t-1", title: "Member" }],
    critical_path: ["t-1"],
  });
  const expected = /href="\/ui\/projects\/p-alpha\/tasks\/t-1\?context=e-1&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Fepics%2Fe-1%3Freturn%3D%252Fui%252Fprojects%252Fp-alpha%252Fwork-items"/g;
  assert.equal((html.match(expected) || []).length, 2);
  assert.doesNotMatch(html, /href="\/ui\/tasks\/t-1"/);
});

test("an epic member note dwells on the render clock, not the wall clock", () => {
  // The epic surface has no injected model clock; renderEpic captures one
  // clock for the whole render. A fixed clock far from the real wall clock
  // must drive the member note (the real clock would report the dwell as days
  // old while the render says 1h).
  const now = Date.parse("2026-07-29T12:00:00Z");
  const html = renderEpic(
    {
      epic: { id: "t-0030", title: "Lifecycle hardening" },
      members: [
        { id: "t-0039", title: "A", needs_you: true, dwell_since: "2026-07-29T11:00:00Z" },
        { id: "t-0036", title: "B", resolution: "merged" },
      ],
    },
    now,
  );
  assert.match(html, /needs you 1h/);
});

// --- element lifecycle -----------------------------------------------------

test("the throughput strip renders all eight buckets in ascending window order", () => {
  const html = renderThroughputStrip({
    buckets: [
      { window: "15m", count: 3 },
      { window: "30m", count: 8 },
      { window: "1h", count: 11 },
      { window: "2h", count: 17 },
      { window: "4h", count: 23 },
      { window: "6h", count: 28 },
      { window: "12h", count: 35 },
      { window: "24h", count: 40 },
    ],
  });

  // Windows in ascending order, each with its cumulative count.
  const windows = [...html.matchAll(/<span class="window">([^<]+)<\/span>/g)].map((match) => match[1]);
  assert.deepEqual(windows, ["15m", "30m", "1h", "2h", "4h", "6h", "12h", "24h"]);
  const counts = [...html.matchAll(/<span class="count">([^<]+)<\/span>/g)].map((match) => match[1]);
  assert.deepEqual(counts, ["3", "8", "11", "17", "23", "28", "35", "40"]);

  // Leading success marker plus a trailing link to the completed work.
  assert.match(html, /<span class="mark"[^>]*>✓<\/span>/);
  assert.match(html, /<span class="label">Done<\/span>/);
  assert.match(html, /href="\/ui\/tasks\?state=done"/);
});

test("null or absent stats render an empty (hidden) throughput strip", () => {
  assert.equal(renderThroughputStrip(null), "");
  assert.equal(renderThroughputStrip(undefined), "");
  assert.equal(renderThroughputStrip({}), "");
  assert.equal(renderThroughputStrip({ buckets: [] }), "");
});

test("the throughput strip element hides itself while stats are null", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-throughput-strip", null);
  await flush();

  assert.equal(strip.hasAttribute("hidden"), true);
  assert.equal(strip.innerHTML, "");

  // A poll hands the existing element fresh data: the instance survives, so
  // a chip under the pointer keeps its hover and focus.
  const first = strip;
  strip.data = { buckets: [{ window: "1h", count: 11 }] };
  await flush();
  assert.equal(first, strip);
  assert.equal(strip.hasAttribute("hidden"), false);
  assert.match(strip.innerHTML, />11<\/span>/);
  assert.match(strip.innerHTML, />1h<\/span>/);
  strip.remove();
});

test("the board feeds the throughput strip fresh stats on every poll", async () => {
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", {
    entries: [],
    showProject: false,
    stats: { buckets: [{ window: "15m", count: 3 }] },
  });
  await flush();

  const strip = board.querySelector("flow-throughput-strip");
  assert.ok(strip, "the board renders the throughput strip between the attention strip and the surface");
  assert.equal(strip.hasAttribute("hidden"), false);
  assert.match(strip.innerHTML, />3<\/span>/);
  assert.match(strip.innerHTML, />15m<\/span>/);

  // A poll sets fresh data on the same board element: the strip keeps its
  // instance — so a chip under the pointer keeps its hover and focus — and
  // picks up the new counts.
  board.data = {
    entries: [],
    showProject: false,
    stats: { buckets: [{ window: "15m", count: 3 }, { window: "30m", count: 8 }] },
  };
  await flush();
  assert.equal(board.querySelector("flow-throughput-strip"), strip, "a poll must not re-create the strip");
  assert.match(strip.innerHTML, />8<\/span>/);
  assert.match(strip.innerHTML, />30m<\/span>/);
  board.remove();
});

test("reconcile keeps the element for a surviving key so its state outlives a poll", async () => {
  const root = globalThis.document.body;
  const container = globalThis.document.createElement("div");
  root.appendChild(container);

  const first = reconcile(container, [{ id: "a" }, { id: "b" }], { tag: "flow-task-card", key: (item) => item.id });
  first[0].marker = "kept";

  const second = reconcile(container, [{ id: "b" }, { id: "a" }, { id: "c" }], {
    tag: "flow-task-card",
    key: (item) => item.id,
  });
  const kept = second.find((element) => element.dataset.key === "a");
  assert.equal(kept.marker, "kept", "a surviving key must keep its element instance");
  assert.equal(second.length, 3);
  assert.deepEqual(
    container.children.map((child) => child.dataset.key),
    ["b", "a", "c"],
    "reconcile reorders in place",
  );

  const third = reconcile(container, [{ id: "a" }], { tag: "flow-task-card", key: (item) => item.id });
  assert.equal(third.length, 1);
  assert.equal(container.children.length, 1, "removed keys must be dropped");
  container.remove();
});

test("the tab strip keeps the active tab across a repaint", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  strip.select("checks");
  await flush();
  assert.equal(strip.active, "checks");

  // A poll sets fresh data; the tab the reader chose must survive it.
  strip.data = { badges: { checks: { text: "3/4", tone: "danger" } } };
  await flush();
  assert.equal(strip.active, "checks");
  assert.match(strip.innerHTML, /data-tab="checks"[^>]*aria-selected="true"/);
  strip.remove();
});

test("a delegated listener survives the element replacing its own innerHTML", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  let changes = 0;
  strip.addEventListener("tab-change", () => {
    changes += 1;
  });
  for (const key of ["checks", "activity", "overview"]) {
    strip.select(key);
    await flush();
  }
  assert.equal(changes, 3, "the click listener must not be lost to repaints");
  strip.remove();
});

test("an unchanged render does not rewrite the DOM", async () => {
  const root = globalThis.document.body;
  const lane = mountElement(root, "flow-lane", { key: "queued", label: "Scheduled", cards: [] });
  await flush();
  const painted = lane.innerHTML;

  let writes = 0;
  const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(lane), "innerHTML");
  Object.defineProperty(lane, "innerHTML", {
    get: () => painted,
    set(value) {
      writes += 1;
      descriptor.set.call(lane, value);
    },
    configurable: true,
  });

  lane.data = { key: "queued", label: "Scheduled", cards: [] };
  await flush();
  assert.equal(writes, 0, "identical markup should not be rewritten; focus and scroll would be lost");
  lane.remove();
});

test("a click bubbles to the app root, which is where actions are dispatched", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  let seen = null;
  root.addEventListener("click", (event) => {
    seen = event.target;
  });
  const tab = strip.querySelector('[data-tab="activity"]');
  tab.dispatchEvent(new TestEvent("click", { bubbles: true }));
  assert.equal(seen, tab);
  strip.remove();
});

// --- review panel: gates, plans, questions ----------------------------------

function reviewFixture(overrides = {}) {
  const wait = {
    id: "ww-1",
    kind: "human_gate",
    node_run_id: "wnr-1",
    message: "Review the proposed implementation tasks.",
    created_at: "2026-07-28T10:00:00Z",
    details: {
      instructions: "Review the proposed implementation tasks.",
      outcomes: ["approved", "changes_requested", "rejected"],
      artifact_id: "wa-1",
      interactive: true,
      gate_node_key: "review",
    },
  };
  const artifacts = [
    {
      id: "wa-1",
      kind: "task_set",
      summary_markdown: "# Plan\n\nSplit storage.",
      created_at: "2026-07-28T09:59:00Z",
      payload: {
        schema_version: 1,
        tasks: [
          { key: "one", title: "Split storage", body: "carve it up", tag_slugs: ["db"] },
          { key: "two", title: "Migrate", body: "move the data" },
        ],
        dependencies: [{ blocker: "one", blocked: "two" }],
      },
    },
  ];
  return {
    wait,
    artifacts,
    run: { current_artifact_id: "wa-1" },
    currentNode: { key: "plan", name: "Write task plan" },
    statusLog: [],
    activeSession: null,
    ...overrides,
  };
}

test("the review model reads the interactive gate from the wait details", () => {
  const review = reviewModel(reviewFixture());
  assert.equal(review.gate.interactive, true);
  assert.deepEqual(review.gate.outcomes, ["approved", "changes_requested", "rejected"]);
  assert.equal(review.gate.artifactID, "wa-1");
  assert.equal(review.gate.nodeRunID, "wnr-1");
  assert.equal(review.gate.waitID, "ww-1", "the gate carries the immutable review round wait id");
  assert.equal(review.gate.changeGate, false);
  assert.equal(review.artifact.manifest.tasks.length, 2);
  assert.equal(review.session, null, "no live session without an active waiting session");
});

test("the review model rejects frozen details with unknown fields", () => {
  const review = reviewModel(
    reviewFixture({
      wait: {
        id: "ww-unknown",
        kind: "human_gate",
        node_run_id: "wnr-unknown",
        details: {
          instructions: "Review the change",
          outcomes: ["approved", "changes_requested"],
          interactive: false,
          gate_node_key: "review",
          legacy_required: true,
        },
      },
    }),
  );
  assert.equal(review.gate, null, "unknown frozen-detail fields make the wait non-actionable");
});

test("the review model does not reconstruct an incomplete classic wait from the graph", () => {
  const review = reviewModel(
    reviewFixture({
      wait: { kind: "human_gate", node_run_id: "wnr-9", message: "Review the change" },
      currentNode: {
        key: "review",
        name: "Review plan",
        config: { human_gate: { instructions: "Gate instructions", outcomes: ["approved", "rejected"] } },
      },
    }),
  );
  assert.equal(review.gate, null);
});

test("a change artifact marks the gate as a change gate", () => {
  const review = reviewModel(
    reviewFixture({ artifacts: [{ id: "wa-1", kind: "change", summary_markdown: "s", payload: {} }] }),
  );
  assert.equal(review.gate.changeGate, true);
});

test("the review panel renders the gate, the plan, and one button per outcome", () => {
  const model = { id: "t-0001", projectID: "p-1", review: reviewModel(reviewFixture()) };
  const html = renderReviewPanel(model);
  assert.match(html, /Review the proposed implementation tasks/);
  assert.match(html, /data-workflow-respond="wnr-1"/);
  assert.match(html, /data-review-wait="ww-1"/);
  assert.match(html, /data-task="t-0001"/);
  assert.match(html, /data-outcome="approved"/);
  assert.match(html, /data-outcome="changes_requested"/);
  assert.match(html, /data-outcome="rejected"/);
  assert.match(html, /data-gate-comment="t-0001"/);
  assert.match(html, /data-workflow-feedback/);
  assert.match(html, /Split storage/);
  assert.match(html, /← one/, "the dependency reads as blocked-by");
  assert.match(html, />db</, "tag slugs render");
  assert.match(html, /agent is live/, "interactive gates say the agent is live");
});

test("a repaint while a gate response is pending keeps every outcome suppressed", () => {
  const model = { id: "t-0001", projectID: "p-1", review: reviewModel(reviewFixture()) };
  const idle = renderReviewPanel(model);
  assert.match(idle, /data-gate-node-run="wnr-1"/);
  assert.doesNotMatch(idle, /is-busy/, "no outcome is suppressed while idle");

  // A response for this node run is in flight when the board's poll repaints
  // the panel; the fresh buttons must re-derive their suppression from the
  // shared in-flight registry instead of flashing enabled.
  inFlight.add("workflowRespond:wnr-1");
  try {
    const pending = renderReviewPanel(model);
    const outcomes = pending.match(/<button[^>]*data-workflow-respond="wnr-1"[^>]*>/g) || [];
    assert.equal(outcomes.length, 3, "one button per gate outcome");
    for (const button of outcomes) {
      assert.match(button, /disabled/, "each outcome stays disabled during the repaint");
      assert.match(button, /aria-busy="true"/, "each outcome stays aria-busy during the repaint");
      assert.match(button, /is-busy/, "each outcome stays visually suppressed during the repaint");
    }
  } finally {
    inFlight.delete("workflowRespond:wnr-1");
  }

  // Once the response settles, the next repaint restores the live controls.
  assert.doesNotMatch(renderReviewPanel(model), /is-busy/);
});

test("the review panel renders gate instructions as markdown", () => {
  const model = {
    id: "t-0001",
    projectID: "p-1",
    review: reviewModel(
      reviewFixture({
        wait: {
          id: "ww-1",
          kind: "human_gate",
          node_run_id: "wnr-1",
          message: "Review the proposed implementation tasks.",
          created_at: "2026-07-28T10:00:00Z",
          details: {
            instructions: "Check the **plan** and visit https://example.com",
            outcomes: ["approved"],
            artifact_id: "wa-1",
            interactive: true,
            gate_node_key: "review",
          },
        },
      }),
    ),
  };
  const html = renderReviewPanel(model);
  assert.match(html, /<div class="instructions"><div class="md">/);
  assert.match(html, /<strong>plan<\/strong>/);
  assert.match(html, /<a href="https:\/\/example\.com"/);
});

test("the review panel answers an agent question with a reply form", () => {
  const review = reviewModel(
    reviewFixture({
      wait: { kind: "agent_request", message: "Which datastore?", created_at: "2026-07-28T10:00:00Z" },
      statusLog: [{ id: 7, kind: "question", message: "Which datastore?", created_at: "2026-07-28T10:00:00Z" }],
    }),
  );
  assert.equal(review.gate, null);
  assert.equal(review.question.statusLogID, 7);
  const html = renderReviewPanel({ id: "t-0001", projectID: "p-1", review });
  assert.match(html, /Which datastore\?/);
  assert.match(html, /data-attention-reply-form="t-0001"/);
  assert.match(html, /data-status-log-id="7"/);
});

test("the review panel keeps the discussion and offers the live session", () => {
  const review = reviewModel(
    reviewFixture({
      statusLog: [
        { id: 1, kind: "note", actor: "human", message: "Can task two shrink?", created_at: "2026-07-28T10:01:00Z" },
        { id: 2, kind: "note", actor: "human", message: "too old", created_at: "2026-07-27T10:00:00Z" },
      ],
      activeSession: { id: "s-0001", state: "waiting", terminal_available: true },
    }),
  );
  assert.deepEqual(review.comments.map((comment) => comment.id), [1], "only the discussion since the wait opened");
  assert.equal(review.session.id, "s-0001");
  const html = renderReviewPanel({ id: "t-0001", projectID: "p-1", review });
  assert.match(html, /Can task two shrink\?/);
  assert.match(html, /Work with the agent/);
  assert.match(html, /s-0001/);
  assert.doesNotMatch(html, /too old/);
});

test("nothing to review renders the empty state", () => {
  assert.equal(reviewModel({ wait: null, artifacts: [], statusLog: [] }), null);
  assert.match(renderReviewPanel({ review: null }), /Nothing to review/);
});

// --- review panel: live terminal lifecycle ----------------------------------
//
// The live terminal is minted once per session and must survive the panel
// repainting around it (task-detail polling rewrites the model on every
// discussion change). These tests drive the element, not just the render
// string, because the preservation is a DOM-lifecycle behaviour.

function reviewPanelModel(sessionID, { state = "waiting", comment = "", terminalAvailable = true } = {}) {
  const statusLog = comment
    ? [{ id: 1, kind: "note", actor: "human", message: comment, created_at: "2026-07-28T10:01:00Z" }]
    : [];
  return {
    id: "t-0001",
    projectID: "p-1",
    review: reviewModel(reviewFixture({ activeSession: { id: sessionID, state, terminal_available: terminalAvailable }, statusLog })),
  };
}

function stubTerminalTokenFetch(loginPathFor) {
  const calls = [];
  globalThis.fetch = (path) => {
    if (String(path).includes("/terminal-token")) calls.push(String(path));
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ access: { login_path: loginPathFor(path) } }) });
  };
  return calls;
}

// settle lets the mint-terminal promise chain (fetch -> json -> invalidate ->
// repaint) run to completion; each flush is one microtask, and the chain spans
// several.
async function settle(times = 8) {
  for (let i = 0; i < times; i += 1) await flush();
}

// collectText concatenates the text of a live subtree. The review panel
// reconciles its sections in place (moving nodes rather than reassigning
// innerHTML), so assertions about what is actually on screen read the live tree
// instead of the element's last-assigned innerHTML string.
function collectText(node) {
  if (!node) return "";
  let out = node.textContent || "";
  for (const child of node.children || []) out += collectText(child);
  return out;
}

test("a same-session changed-model repaint keeps the live terminal iframe", async () => {
  const root = globalThis.document.body;
  const calls = stubTerminalTokenFetch(() => "/ui/terminal/login?tok=abc");
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001"));
  await settle();

  const firstIframe = panel.querySelector("iframe");
  assert.ok(firstIframe, "the minted terminal iframe is mounted");
  assert.equal(firstIframe.getAttribute("src"), "/ui/terminal/login?tok=abc");
  assert.equal(firstIframe.loadCount, 1, "the iframe loaded once on mount");

  // A poll delivers a changed model for the same waiting session (a new
  // comment landed). The panel repaints, but the terminal must not reload.
  panel.data = reviewPanelModel("s-0001", { comment: "Can task two shrink?" });
  await flush();

  assert.match(collectText(panel), /Can task two shrink\?/, "the changed content repaints");
  assert.strictEqual(panel.querySelector("iframe"), firstIframe, "the same iframe node survives the repaint");
  assert.equal(firstIframe.loadCount, 1, "the preserved iframe is never reloaded (browser-observable continuity)");
  assert.equal(calls.length, 1, "terminal access is minted once, not per repaint");
  panel.remove();
});

test("removing an intermediate section does not move unchanged retained sections", async () => {
  const root = globalThis.document.body;
  stubTerminalTokenFetch(() => "/ui/terminal/login?tok=abc");
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001", { comment: "First comment" }));
  await settle();

  const gate = panel.querySelector('[data-section="gate"]');
  const live = panel.querySelector('[data-section="live"]');
  const firstIframe = panel.querySelector("iframe");
  assert.ok(gate && live && firstIframe, "the panel mounts gate, comments and live");
  assert.deepEqual(
    panel.children.map((child) => child.getAttribute("data-section")),
    ["gate", "comments", "live"],
    "the initial order includes the intermediate comments section",
  );

  // Record every wrapper the reconciler repositions. An unchanged retained
  // section must not be moved just because its former neighbour is stale.
  const moved = [];
  const originalInsertBefore = TestNode.prototype.insertBefore;
  TestNode.prototype.insertBefore = function (child, reference) {
    if (child.parentElement === panel) moved.push(child);
    return originalInsertBefore.call(this, child, reference);
  };
  try {
    // The discussion empties out: [gate, comments, live] -> [gate, live].
    panel.data = reviewPanelModel("s-0001");
    await flush();
  } finally {
    TestNode.prototype.insertBefore = originalInsertBefore;
  }

  assert.ok(!moved.includes(gate), "the unchanged gate section must not be repositioned");
  assert.deepEqual(
    panel.children.map((child) => child.getAttribute("data-section")),
    ["gate", "live"],
    "the retained sections keep their order after the removal",
  );
  assert.strictEqual(panel.querySelector("iframe"), firstIframe, "the terminal iframe survives the removal");
  assert.equal(firstIframe.loadCount, 1, "the preserved iframe is never reloaded");
  panel.remove();
});

test("a session-identity change remints the terminal for the new session", async () => {
  const root = globalThis.document.body;
  const calls = stubTerminalTokenFetch((path) =>
    path.includes("s-0002") ? "/ui/terminal/login?tok=two" : "/ui/terminal/login?tok=one",
  );
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001"));
  await settle();

  const firstIframe = panel.querySelector("iframe");
  assert.equal(firstIframe.getAttribute("src"), "/ui/terminal/login?tok=one");

  // The session under review changes identity: the old terminal is replaced
  // with freshly minted access for the new session.
  panel.data = reviewPanelModel("s-0002");
  await settle();

  const secondIframe = panel.querySelector("iframe");
  assert.ok(secondIframe, "a terminal is mounted for the new session");
  assert.notStrictEqual(secondIframe, firstIframe, "the new session gets a freshly minted iframe");
  assert.equal(secondIframe.getAttribute("src"), "/ui/terminal/login?tok=two");
  assert.equal(secondIframe.parentElement.getAttribute("data-terminal-session"), "s-0002");
  assert.equal(calls.length, 2, "each session identity mints its own access");
  assert.ok(calls[0].includes("s-0001") && calls[1].includes("s-0002"));
  panel.remove();
});

test("a session leaving the waiting state removes the terminal", async () => {
  const root = globalThis.document.body;
  stubTerminalTokenFetch(() => "/ui/terminal/login?tok=abc");
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001"));
  await settle();
  assert.ok(panel.querySelector("iframe"), "the terminal is mounted while waiting");

  // The agent resumes: the session is no longer waiting, so the terminal goes.
  panel.data = reviewPanelModel("s-0001", { state: "working" });
  await flush();

  assert.equal(panel.querySelector("iframe"), null, "the terminal is removed once the session leaves waiting");
  assert.equal(panel.querySelector('[data-section="live"]'), null, "the live-session section is removed");
  assert.doesNotMatch(collectText(panel), /Work with the agent/);
  panel.remove();
});

test("a session transition never exposes a prior session's terminal credential", async () => {
  const root = globalThis.document.body;
  const calls = stubTerminalTokenFetch((path) =>
    path.includes("s-0002") ? "/ui/terminal/login?tok=two" : "/ui/terminal/login?tok=one",
  );
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001"));
  await settle();
  assert.equal(panel.querySelector("iframe").getAttribute("src"), "/ui/terminal/login?tok=one");

  // Session B arrives while its terminal is not yet available. The panel must
  // reset A's minted access before rendering — it may not show B's section with
  // A's session-bound login URL.
  panel.data = reviewPanelModel("s-0002", { terminalAvailable: false });
  await flush();

  assert.match(collectText(panel), /s-0002/, "the new session is shown");
  assert.doesNotMatch(collectText(panel), /tok=one/, "the prior session's credential is gone");
  assert.equal(panel.querySelector("iframe"), null, "no iframe renders until B's terminal is available");
  assert.equal(calls.length, 1, "no access is minted for an unavailable terminal");

  // Once B's terminal becomes available, it mints and mounts B's own access.
  panel.data = reviewPanelModel("s-0002", { terminalAvailable: true });
  await settle();

  const iframe = panel.querySelector("iframe");
  assert.ok(iframe, "B's terminal mounts once available");
  assert.equal(iframe.getAttribute("src"), "/ui/terminal/login?tok=two");
  assert.equal(iframe.parentElement.getAttribute("data-terminal-session"), "s-0002");
  assert.equal(calls.length, 2);
  assert.ok(calls[1].includes("s-0002"));
  panel.remove();
});

test("a hostile terminal-token rejection leaves a safe visible error on the review panel", async () => {
  const root = globalThis.document.body;
  // The terminal-token POST rejects with a Proxy whose prototype lookup
  // throws: the mint catch must format it without throwing, or terminalError
  // would never be set and the panel would show nothing.
  globalThis.fetch = () => Promise.reject(new Proxy({}, {
    getPrototypeOf() {
      throw new Error("prototype trap");
    },
  }));
  const panel = mountElement(root, "flow-review-panel", reviewPanelModel("s-0001"));
  await settle();

  assert.equal(panel.terminalError, "Request failed", "the mint failure formats to a safe fallback");
  assert.match(collectText(panel), /Request failed/, "the safe failure message is visible on the panel");
  assert.match(collectText(panel), /Retry/, "the terminal error offers a retry");
  panel.remove();
});

test("the Answer action targets the review tab, never the empty checks tab", () => {
  const model = {
    wait: { kind: "human_gate", message: "Review the proposed implementation tasks." },
    waitKind: "gate",
    activity: "Write task plan working",
    dwell: "2m",
    stepName: "Write task plan",
    review: { gate: { changeGate: false } },
  };
  const card = nowCardModel(model);
  assert.equal(card.heading, "Now · waiting for your review");
  assert.equal(card.actions[0].tab, "review");
  const html = renderNowCard(card, { id: "t-0001", projectID: "p-1" });
  assert.match(html, /data-focus-tab="review"/);
  assert.doesNotMatch(html, /data-focus-tab="checks"/);
});

test("a change gate's Answer action goes to the change tab", () => {
  const card = nowCardModel({
    wait: { kind: "human_gate", message: "Review the change" },
    waitKind: "gate",
    activity: "Human change review",
    dwell: "1m",
    stepName: "Human change review",
    review: { gate: { changeGate: true } },
  });
  assert.equal(card.actions[0].tab, "change");
});

test("an open gate badges the review tab", () => {
  const badges = tabBadges({ review: { gate: { nodeRunID: "wnr-1" } }, checks: [], transitions: [], statusLog: [] });
  assert.equal(badges.review.text, "!");
  assert.equal(badges.review.tone, "warn");
  assert.equal(tabBadges({ checks: [], transitions: [], statusLog: [] }).review, undefined);
});
test("the relation picker adds and removes rows", () => {
  const app = { projects: [{ id: "p-1", name: "flow" }], tasks: [] };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form);
  const rows = form.querySelector("[data-relation-rows]");
  const addButton = form.querySelector("[data-relation-add]");

  assert.equal(rows.children.length, 1, "the picker starts with one row");
  addButton.click();
  addButton.click();
  assert.equal(rows.children.length, 3);
  const firstRow = rows.children[0];
  assert.ok(firstRow.querySelector("[data-relation-target]"));

  firstRow.querySelector("[data-relation-remove]").click();
  assert.equal(rows.children.length, 2);
});

test("relation picker rows default to a source-outward kind, initial and added", () => {
  const app = { projects: [{ id: "p-1", name: "flow" }], tasks: [] };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form);
  const rows = form.querySelector("[data-relation-rows]");
  const addButton = form.querySelector("[data-relation-add]");

  const kindOf = (row) => {
    const select = row.querySelector("[data-relation-kind]");
    return select.children.find((option) => option.getAttribute("selected") !== null).getAttribute("value");
  };
  const kinds = (rows) => Array.from(rows.children).map(kindOf);

  // The initial row defaults to the source-outward kind, and every row added
  // later does too, so several default rows with distinct targets never trip
  // the one-parent validation.
  assert.deepEqual(kinds(rows), ["related_to"]);
  addButton.click();
  addButton.click();
  assert.deepEqual(kinds(rows), ["related_to", "related_to", "related_to"]);
});

test("relationTargetSuggestionsView is project-scoped and title-optional", () => {
  const app = { workItemsByProject: new Map([["p-1", [
    { id: "t-1", kind: "task", title: "Alpha" },
    { id: "e-1", kind: "epic", title: "Plan" },
    { id: "f-1", kind: "feature", title: "Ship" },
  ]]]) };
  assert.equal(relationTargetSuggestionsView(app, "p-1"), `<option value="t-1" label="task · Alpha"></option><option value="e-1" label="epic · Plan"></option><option value="f-1" label="feature · Ship"></option>`);
  assert.equal(relationTargetSuggestionsView(app, "p-2"), "");
  assert.equal(relationTargetSuggestionsView({}, "p-1"), "");
});

test("the relation picker suggests the selected project's cached tasks", () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    workItemsByProject: new Map([
      ["p-1", [{ id: "t-1", kind: "task", title: "Alpha" }, { id: "e-2", kind: "epic", title: "Plan" }, { id: "f-2", kind: "feature", title: "Ship" }]],
      ["p-2", [{ id: "t-9", kind: "task", title: "Other project" }]],
    ]),
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const options = host.querySelector("#relation-target-work-items").children;
  assert.equal(options.length, 3);
  assert.deepEqual(options.map((option) => option.getAttribute("value")), ["t-1", "e-2", "f-2"]);
  assert.deepEqual(options.map((option) => option.getAttribute("label")), ["task · Alpha", "epic · Plan", "feature · Ship"]);
  // Suggestions stay scoped to the selected project, not the whole cache.
  assert.ok(!options.some((option) => option.getAttribute("value") === "t-9"));
});

test("the relation picker falls back to manual entry with an empty task cache", () => {
  const app = { projects: [{ id: "p-1", name: "one" }] };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  assert.equal(host.querySelector("#relation-target-work-items").children.length, 0);
  // The free-text target input is still present, so an id can be typed by hand.
  assert.ok(host.querySelector("[data-relation-target]"));
});

test("changing the create form project reloads the relation target suggestions", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map(),
    workItemsByProject: new Map([["p-1", [{ id: "t-1", kind: "task", title: "Alpha" }]]]),
  };
  const ensured = [];
  app.ensureFlows = async () => {};
  app.ensureWorkItems = async (projectID) => {
    ensured.push(projectID);
    if (projectID === "p-2") app.workItemsByProject.set("p-2", [{ id: "t-9", kind: "task", title: "Beta" }]);
    return app.workItemsByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(ensured, ["p-2"]);
  const options = form.querySelector("#relation-target-work-items").children;
  assert.deepEqual(options.map((option) => option.getAttribute("value")), ["t-9"]);
  assert.equal(options[0].getAttribute("label"), "task · Beta");
});

test("a failed suggestion reload leaves the picker in manual-entry mode", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map(),
    workItemsByProject: new Map([["p-1", [{ id: "t-1", kind: "task", title: "Alpha" }]]]),
  };
  app.ensureFlows = async () => {};
  app.ensureWorkItems = async (projectID) => app.workItemsByProject.get(projectID) || [];
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.equal(form.querySelector("#relation-target-work-items").children.length, 0);
  assert.ok(form.querySelector("[data-relation-target]"), "the manual target input remains");
});

test("a rejected suggestion reload leaves the picker in manual-entry mode without an unhandled rejection", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map(),
    workItemsByProject: new Map([["p-1", [{ id: "t-1", kind: "task", title: "Alpha" }]]]),
  };
  app.ensureFlows = async () => {};
  app.ensureWorkItems = async (projectID) => {
    if (projectID === "p-2") throw new Error("work items fetch failed");
    return app.workItemsByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.equal(form.querySelector("#relation-target-work-items").children.length, 0, "the prior project's suggestions are not kept");
  assert.ok(form.querySelector("[data-relation-target]"), "the manual target input remains");
});

test("changing the create form project reloads the flow selector and parent picker", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    workItemsByProject: new Map([[ "p-1", [] ]]),
  };
  const ensuredFlows = [];
  app.ensureFlows = async (projectID) => {
    ensuredFlows.push(projectID);
    if (projectID === "p-2") app.flowsByProject.set("p-2", { flows: [{ id: "fl-9", name: "Beta flow" }, { id: "fl-8", name: "Gamma flow" }], defaultFlowID: "fl-8" });
    return app.flowsByProject.get(projectID);
  };
  app.ensureWorkItems = async (projectID) => {
    if (projectID === "p-2") app.workItemsByProject.set("p-2", [{ id: "ft-9", kind: "feature", title: "Beta feature", state: { status: "open", terminal: false }, capabilities: { can_contain: true } }]);
    return app.workItemsByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(ensuredFlows, ["p-2"]);
  const options = form.querySelector('[name="flow_id"]').children;
  assert.deepEqual(options.map((option) => option.getAttribute("value")), ["fl-9", "fl-8"]);
  assert.equal(options[1].hasAttribute("selected"), true, "the new project's default flow is selected");
  assert.equal(options[0].hasAttribute("selected"), false, "the new project's non-default flow is not selected");
  const parentOptions = form.querySelector("#task-parent-items").children;
  assert.deepEqual(parentOptions.map((option) => option.getAttribute("value")), ["ft-9"], "the parent picker follows the project too");
  assert.equal(form.querySelector('[name="feature_id"]'), null, "legacy feature assignment is not editable");
  const relationOptions = form.querySelector("#relation-target-work-items").children;
  assert.deepEqual(relationOptions.map((option) => option.getAttribute("value")), ["ft-9"], "relation suggestions follow the project too");
});

test("rapid project switches cannot repaint the flow select or relation suggestions with stale-project data", async () => {
  let resolveP2Flows;
  let resolveP2Items;
  const p2FlowsGate = new Promise((resolve) => { resolveP2Flows = resolve; });
  const p2ItemsGate = new Promise((resolve) => { resolveP2Items = resolve; });
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }, { id: "p-3", name: "three" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    workItemsByProject: new Map([[ "p-1", [{ id: "t-1", title: "One task" }] ]]),
  };
  app.ensureFlows = (projectID) => {
    if (projectID === "p-2") {
      return p2FlowsGate.then(() => {
        const cache = { flows: [{ id: "fl-2", name: "Two flow" }], defaultFlowID: "fl-2" };
        app.flowsByProject.set("p-2", cache);
        return cache;
      });
    }
    if (projectID === "p-3") {
      const cache = { flows: [{ id: "fl-3", name: "Three flow" }], defaultFlowID: "fl-3" };
      app.flowsByProject.set("p-3", cache);
      return Promise.resolve(cache);
    }
    return Promise.resolve(app.flowsByProject.get(projectID));
  };
  app.ensureWorkItems = (projectID) => {
    if (projectID === "p-2") {
      return p2ItemsGate.then(() => {
        const tasks = [{ id: "t-2", title: "Two task" }];
        app.workItemsByProject.set("p-2", tasks);
        return tasks;
      });
    }
    if (projectID === "p-3") {
      const tasks = [{ id: "t-3", title: "Three task" }];
      app.workItemsByProject.set("p-3", tasks);
      return Promise.resolve(tasks);
    }
    return Promise.resolve(app.workItemsByProject.get(projectID));
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  const flowSelect = form.querySelector('[name="flow_id"]');
  const datalist = form.querySelector("#relation-target-work-items");
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  projectSelect.value = "p-3";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the newest project's flows are shown");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the newest project's work items are suggested");
  // The p-2 loads land late: neither control may repaint with p-2's data.
  resolveP2Flows();
  resolveP2Items();
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the stale flow load does not repaint");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the stale work-item load does not repaint");
});

test("a rejected flow reload leaves the flow and parent controls on explicit defaults", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    workItemsByProject: new Map([[ "p-1", [] ]]),
  };
  app.ensureFlows = async (projectID) => {
    if (projectID === "p-2") throw new Error("flows fetch failed");
    return app.flowsByProject.get(projectID);
  };
  app.ensureWorkItems = async (projectID) => {
    if (projectID === "p-2") throw new Error("work items fetch failed");
    return app.workItemsByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  const flowOptions = form.querySelector('[name="flow_id"]').children;
  assert.equal(flowOptions.length, 1, "the flow select falls back to the project-default option");
  assert.equal(flowOptions[0].getAttribute("value"), "");
  assert.deepEqual(form.querySelector("#task-parent-items").children, [], "the parent picker falls back to no parent");
  assert.equal(form.querySelector("[data-inferred-feature]").textContent, "No feature inferred");
});

test("a rejected work-item reload leaves the parent picker on the explicit default", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    workItemsByProject: new Map([[ "p-1", [] ]]),
  };
  app.ensureFlows = async (projectID) => {
    if (projectID === "p-2") app.flowsByProject.set("p-2", { flows: [{ id: "fl-9", name: "Beta flow" }], defaultFlowID: "fl-9" });
    return app.flowsByProject.get(projectID);
  };
  app.ensureWorkItems = async (projectID) => {
    if (projectID === "p-2") throw new Error("work items fetch failed");
    return app.workItemsByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  const flowOptions = form.querySelector('[name="flow_id"]').children;
  assert.deepEqual(flowOptions.map((option) => option.getAttribute("value")), ["fl-9"], "the flow select still shows the new project's flows");
  assert.deepEqual(form.querySelector("#task-parent-items").children, [], "the parent picker falls back to no parent");
  assert.equal(form.querySelector("[data-inferred-feature]").textContent, "No feature inferred");
});

test("a rejected load during rapid switches never repaints with the stale project's state", async () => {
  let rejectP2Flows;
  const p2FlowsGate = new Promise((resolve, reject) => { rejectP2Flows = reject; });
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }, { id: "p-3", name: "three" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    workItemsByProject: new Map([[ "p-1", [{ id: "t-1", title: "One task" }] ]]),
  };
  app.ensureFlows = (projectID) => {
    if (projectID === "p-2") return p2FlowsGate;
    if (projectID === "p-3") {
      const cache = { flows: [{ id: "fl-3", name: "Three flow" }], defaultFlowID: "fl-3" };
      app.flowsByProject.set("p-3", cache);
      return Promise.resolve(cache);
    }
    return Promise.resolve(app.flowsByProject.get(projectID));
  };
  app.ensureWorkItems = (projectID) => {
    if (projectID === "p-2") throw new Error("work items fetch failed");
    if (projectID === "p-3") {
      const tasks = [{ id: "t-3", title: "Three task" }];
      app.workItemsByProject.set("p-3", tasks);
      return Promise.resolve(tasks);
    }
    return Promise.resolve(app.workItemsByProject.get(projectID));
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  const flowSelect = form.querySelector('[name="flow_id"]');
  const datalist = form.querySelector("#relation-target-work-items");
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  projectSelect.value = "p-3";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the newest project's flows are shown");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the newest project's work items are suggested");
  // The p-2 flow load rejects late: no control may repaint with p-2's state.
  rejectP2Flows(new Error("flows fetch failed"));
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the stale rejected flow load does not repaint");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the stale rejected work-item load does not repaint");
});
// --- review verdict pending state (flow-change / submitReview) --------------

function reviewChangeData() {
  return {
    change: { id: "ch-0001", head_sha: "abc123def456" },
    task: { id: "t-0001" },
    // The diff names the head it was rendered for, matching the change
    // metadata head — the production shape both routes supply.
    diff: {
      head_sha: "abc123def456",
      files: [{ path: "a.go", hunks: [{ header: "@@ -1 +1 @@", lines: [{ kind: "add", new_line: 1, text: "x" }] }] }],
    },
    threads: [],
    review_state: "in_review",
  };
}

// Mounts a flow-change inside a fake <flow-app> so the element's `app` getter
// resolves to a node that records status messages.
function mountChange(root, data) {
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const change = mountElement(appNode, "flow-change", data);
  return { appNode, change, statuses };
}

function stubReviewFetch(handler) {
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return handler(path, options);
  };
  return calls;
}

test("a review verdict marks the button busy and names the in-flight submission", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const bodyInput = change.querySelector("[data-review-body]");
  const pending = change.handleClick({ target: approve, preventDefault() {} });

  // Synchronous pending state, before the network resolves.
  assert.equal(approve.disabled, true);
  assert.equal(approve.getAttribute("aria-busy"), "true");
  assert.equal(approve.classList.contains("is-busy"), true);
  assert.equal(bodyInput.disabled, true, "the overall-comment input is disabled with the verdict");
  assert.deepEqual(statuses, ["Approving\u2026"]);

  resolveRequest();
  await pending;
  assert.equal(approve.disabled, false);
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(approve.classList.contains("is-busy"), false);
  assert.equal(bodyInput.disabled, false, "the overall-comment input is restored on success");
  assert.deepEqual(statuses, ["Approving\u2026", "Approved"]);
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, "abc123def456", "the submission carries the head displayed with the diff");
  assert.equal(posted.verdict, "approve");
  change.remove();
  appNode.remove();
});

test("a change-review verdict carries the exact observed human-gate identity", async () => {
  const root = globalThis.document.body;
  const calls = stubReviewFetch(() => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }));
  const data = reviewChangeData();
  data.open_wait = { id: "ww-7", kind: "human_gate", node_run_id: "wnr-7" };
  const { appNode, change } = mountChange(root, data);
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  await change.handleClick({ target: approve, preventDefault() {} });

  assert.deepEqual(JSON.parse(calls[0].options.body), {
    verdict: "approve",
    body: "",
    comments: [],
    head_sha: "abc123def456",
    node_run_id: "wnr-7",
    review_wait_id: "ww-7",
  });
  change.remove();
  appNode.remove();
});

test("a successful same-head submission clears the submitted overall comment from the fresh review bar", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall feedback";
  // The settle refresh repaints the change with fresh data for the same head,
  // the way a poll would in production. The review bar is replaced, and the
  // same-key repaint must not capture the consumed text back into the fresh
  // input.
  appNode.refresh = async () => {
    change.data = { ...reviewChangeData(), review_state: "approved" };
    await flush();
  };

  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.body, "overall feedback", "the submission carries the typed overall comment");

  resolveRequest();
  await pending;
  await flush();

  const repaintedInput = change.querySelector("[data-review-body]");
  assert.ok(repaintedInput && repaintedInput !== bodyInput, "the settle refresh replaced the comment input");
  assert.equal(repaintedInput.value, "", "the submitted overall comment does not reappear in the fresh review bar");
  assert.deepEqual(statuses, ["Approving\u2026", "Approved"]);
  change.remove();
  appNode.remove();
});

test("an empty comment reports 'Nothing to post' when no mutation is pending", async () => {
  const root = globalThis.document.body;
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const comment = change.querySelector('[data-review-verdict="comment"]');
  await change.handleClick({ target: comment, preventDefault() {} });

  assert.deepEqual(statuses, ["Nothing to post"], "validation stays visible with no mutation pending");
  assert.equal(inFlight.size, 0, "validation does not register an in-flight key");
  assert.equal(comment.hasAttribute("disabled"), false, "validation leaves the verdict controls enabled");
  change.remove();
  appNode.remove();
});

test("an empty comment while another mutation is pending keeps that mutation's label", async () => {
  const root = globalThis.document.body;
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  // A distinct change-view mutation (a gate response, say) is in flight with
  // its pending label on the shared status line.
  const otherKey = "workflowRespond:wnr-1";
  const entry = acquireBusy(otherKey, "Sending feedback wnr-1\u2026");
  assert.ok(entry, "the sibling mutation acquired its busy key");
  try {
    const comment = change.querySelector('[data-review-verdict="comment"]');
    await change.handleClick({ target: comment, preventDefault() {} });

    // The validation must not hide the sibling's pending label…
    assert.equal(statuses.includes("Nothing to post"), false, "validation must not overwrite the pending label");
    assert.equal(statuses[statuses.length - 1], "Sending feedback wnr-1\u2026", "the pending label stays on the line");
    assert.equal(inFlight.size, 1, "only the sibling mutation is in flight");

    // …and the sibling's settlement still reports its final result.
    settleStatus(appNode, otherKey, "Feedback sent");
    releaseBusy(otherKey);
    assert.equal(statuses[statuses.length - 1], "Feedback sent", "the settled mutation's result stays visible");
    assert.equal(inFlight.size, 0);
  } finally {
    releaseBusy(otherKey);
  }
  change.remove();
  appNode.remove();
});

test("a duplicate review verdict is suppressed while the first is in flight", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const first = change.handleClick({ target: approve, preventDefault() {} });
  assert.equal(requests, 1);

  // A second click on the same verdict while the first is still running.
  await change.handleClick({ target: approve, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate review submission while in flight");

  resolveRequest();
  await first;
  assert.equal(inFlight.size, 0);
  change.remove();
  appNode.remove();
});

test("a different verdict cannot start while another review submission is in flight", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const requestChanges = change.querySelector('[data-review-verdict="request_changes"]');
  const first = change.handleClick({ target: approve, preventDefault() {} });
  assert.equal(requests, 1);

  // The change is the mutation target: while the approve submission is in
  // flight the other verdict controls are suppressed too, not just the one
  // that was clicked.
  assert.equal(approve.disabled, true);
  assert.equal(approve.getAttribute("aria-busy"), "true");
  assert.equal(requestChanges.disabled, true);

  // Clicking a different verdict must not issue a competing submission: the
  // in-flight key is the change, not the verdict.
  await change.handleClick({ target: requestChanges, preventDefault() {} });
  assert.equal(requests, 1, "no competing verdict while the first is in flight");

  resolveRequest();
  await first;
  assert.equal(inFlight.size, 0);
  assert.equal(approve.disabled, false);
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(requestChanges.disabled, false);
  change.remove();
  appNode.remove();
});

test("a repaint while a review is in flight keeps the verdict controls suppressed", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const bodyInput = change.querySelector("[data-review-body]");
  const first = change.handleClick({ target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(bodyInput.disabled, true, "the overall-comment input is disabled synchronously");

  // A poll (or any re-render) replaces the review bar's buttons mid-flight.
  change.invalidate();
  await flush();

  const repaintedApprove = change.querySelector('[data-review-verdict="approve"]');
  const repaintedRequestChanges = change.querySelector('[data-review-verdict="request_changes"]');
  const repaintedBodyInput = change.querySelector("[data-review-body]");
  assert.ok(repaintedApprove && repaintedApprove !== approve, "the bar was replaced by the repaint");
  assert.equal(repaintedApprove.hasAttribute("disabled"), true);
  assert.equal(repaintedApprove.getAttribute("aria-busy"), "true");
  assert.equal(repaintedApprove.classList.contains("is-busy"), true);
  assert.equal(repaintedRequestChanges.hasAttribute("disabled"), true);
  assert.equal(repaintedRequestChanges.getAttribute("aria-busy"), null);
  assert.ok(repaintedBodyInput && repaintedBodyInput !== bodyInput, "the comment input was replaced by the repaint");
  assert.equal(repaintedBodyInput.hasAttribute("disabled"), true, "the repainted input stays disabled");

  resolveRequest();
  await first;
  assert.equal(repaintedApprove.disabled, false);
  assert.equal(repaintedApprove.hasAttribute("disabled"), false);
  assert.equal(repaintedApprove.getAttribute("aria-busy"), null);
  assert.equal(repaintedRequestChanges.disabled, false);
  assert.equal(repaintedBodyInput.disabled, false, "the repainted input is restored on settle");
  assert.equal(inFlight.size, 0);
  change.remove();
  appNode.remove();
});

test("an unsubmitted overall comment survives a same-head repaint of the review bar", async () => {
  const root = globalThis.document.body;
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "still typing the overall comment";

  // A poll or same-head metadata revalidation repaints the change while the
  // reviewer is still typing, outside any submission. The inline drafts map
  // cannot see the overall-comment input, so the live value must be captured
  // before the write and restored into the fresh bar.
  change.data = { ...reviewChangeData(), review_state: "changes_requested" };
  await flush();

  const repaintedInput = change.querySelector("[data-review-body]");
  assert.ok(repaintedInput && repaintedInput !== bodyInput, "the repaint replaced the comment input");
  assert.equal(repaintedInput.value, "still typing the overall comment", "the unsubmitted overall comment survives the repaint");
  change.remove();
  appNode.remove();
});

test("a diff-head move under unchanged metadata drops the old head's overall comment", async () => {
  const root = globalThis.document.body;
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall feedback against h1";

  // The change and diff GETs are not atomic: the diff advances to a new head
  // while the metadata still names the old one. The element re-renders with
  // the diff's head — what the reviewer now sees — and the old head's comment
  // must not ride into the new head's bar, where a later submission would
  // post it against code the reviewer never inspected.
  change.data = {
    ...reviewChangeData(),
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();

  const repaintedInput = change.querySelector("[data-review-body]");
  assert.ok(repaintedInput && repaintedInput !== bodyInput, "the repaint replaced the comment input");
  assert.equal(repaintedInput.value, "", "the old head's overall comment does not ride into the new head's bar");
  change.remove();
  appNode.remove();
});

test("a review submission binds to the head displayed when the head moves between render and submit", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  // The change advances while the reviewer is reading: the element re-renders
  // with the new head, so the submission must carry the new displayed head.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "def456789abc" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, "def456789abc", "the submission carries the head currently displayed");

  resolveRequest();
  await pending;
  change.remove();
  appNode.remove();
});

test("a submission in the queued-paint window binds to the head still rendered, not the model's newer head", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  // The reviewer writes against h1, which is what the DOM renders.
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "feedback written against h1";
  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h1" });

  // The change advances: the model is h2, but the repaint is only queued, so
  // the DOM still renders h1's diff and review controls. Submitting in this
  // window must bind to the head actually on screen — the server accepts
  // feedback against the named head, so naming h2 would let h1 feedback land
  // on code the reviewer never inspected.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "def456789abc" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  assert.equal(change.querySelector(".head").dataset.head, "abc123def456", "the rendered head is still h1 before the queued paint");

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, "abc123def456", "the submission names the head rendered on screen, not the model's newer head");
  assert.equal(posted.body, "feedback written against h1");
  assert.deepEqual(posted.comments, [{ file_path: "a.go", line: 1, body: "note against h1" }]);

  resolveRequest();
  await pending;
  // The queued repaint lands once the submission settles.
  await flush();
  assert.equal(change.querySelector(".head").dataset.head, "def456789abc", "the queued repaint shows the new head");
  change.remove();
  appNode.remove();
});

test("a head change between renders drops drafts written against the old head", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  // The reviewer drafts an inline note while reading h1's diff.
  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h1" });
  assert.equal(change.drafts.size, 1);

  // The change advances and the standalone change page re-renders with h2.
  // The element survives the poll (drafts are meant to outlive re-renders), but
  // notes composed against h1 must not ride along: the server would accept and
  // anchor them to h2 even though the reviewer never inspected h2.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "def456789abc" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();

  assert.equal(change.drafts.size, 0, "drafts written against the old head are dropped on the head change");
  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.deepEqual(posted.comments, [], "the h2 submission carries no h1 draft notes");
  assert.equal(posted.head_sha, "def456789abc", "the submission names the newly displayed head");

  resolveRequest();
  await pending;
  change.remove();
  appNode.remove();
});

test("the submission names the diff head when it differs from the metadata head", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  // A route that failed to verify its pair would show one head's metadata
  // above another head's diff; the submission must still name the head the
  // reviewer actually saw — the diff's — so the server can refuse it as stale
  // instead of attaching the review to the metadata head.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "abc123def456" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, "def456789abc", "the submission carries the diff head");

  resolveRequest();
  await pending;
  change.remove();
  appNode.remove();
});

test("a submission in the queued-paint window binds to the rendered diff head when the diff response names a newer head", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  const { appNode, change } = mountChange(root, reviewChangeData());
  await flush();

  // The head moves between the metadata and diff fetches: the model's diff
  // response already names h2, but the repaint is only queued, so the DOM
  // still renders h1's diff. The submission must bind to the head of the diff
  // currently rendered — h1 — not the newer diff head the model just
  // received; the server refuses h1 as stale instead of accepting h2-bound
  // feedback for a diff the reviewer never saw.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "abc123def456" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  assert.equal(change.querySelector(".head").dataset.head, "abc123def456", "the rendered diff head is still h1 before the queued paint");

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, "abc123def456", "the submission names the diff currently rendered, not the model's newer diff head");

  resolveRequest();
  await pending;
  // The queued repaint lands once the submission settles, showing the newer
  // diff head the metadata/diff race delivered.
  await flush();
  assert.equal(change.querySelector(".head").dataset.head, "def456789abc", "the queued repaint shows the newer diff head");
  change.remove();
  appNode.remove();
});

test("a stale-head conflict keeps the drafts and shows the conflict message", async () => {
  const root = globalThis.document.body;
  stubReviewFetch(() =>
    Promise.resolve({
      ok: false,
      status: 409,
      json: () => Promise.resolve({ error: { message: "change head moved; reload and re-review" } }),
    }),
  );
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note on the inspected head" });
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall feedback";
  const approve = change.querySelector('[data-review-verdict="approve"]');
  await change.handleClick({ target: approve, preventDefault() {} });

  assert.deepEqual(statuses, ["Approving\u2026", "change head moved; reload and re-review"]);
  assert.equal(change.drafts.get("a.go:1").body, "note on the inspected head", "the conflict keeps the draft notes");
  assert.equal(bodyInput.value, "overall feedback", "the conflict keeps the review body");
  assert.equal(approve.disabled, false);
  assert.equal(inFlight.size, 0);
  change.remove();
  appNode.remove();
});

test("a delayed conflict for the old head does not restore review text into the new head's bar", async () => {
  const root = globalThis.document.body;
  let resolveFirst;
  let resolveSecond;
  let calls = 0;
  const posted = [];
  const reviewCalls = stubReviewFetch(() => {
    calls += 1;
    if (calls === 1) {
      // The h1 submission hangs while the reviewer's approval is in flight.
      return new Promise((resolve) => {
        resolveFirst = () =>
          resolve({ ok: false, status: 409, json: () => Promise.resolve({ error: { message: "change head moved; reload and re-review" } }) });
      });
    }
    return new Promise((resolve) => {
      resolveSecond = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  // The reviewer writes an overall review against h1 and submits it.
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall feedback written against h1";
  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h1" });
  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  posted.push(JSON.parse(reviewCalls[reviewCalls.length - 1].options.body));
  assert.equal(posted[0].head_sha, "abc123def456");
  assert.equal(posted[0].body, "overall feedback written against h1");

  // While the submission hangs, the change advances: the poll re-renders h2,
  // which replaces the review bar and drops the h1 drafts.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "def456789abc" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();
  assert.equal(change.drafts.size, 0, "the h2 repaint drops the h1 drafts");
  const repaintedInput = change.querySelector("[data-review-body]");
  assert.equal(repaintedInput.value, "", "the h2 repaint leaves an empty review bar");

  // The delayed 409 for the h1 submission lands after the repaint. It must
  // not restore the h1 body into the h2 review bar.
  resolveFirst();
  await pending;
  assert.deepEqual(statuses, ["Approving\u2026", "change head moved; reload and re-review"]);
  assert.equal(change.querySelector("[data-review-body]").value, "", "the rejected h1 body is not restored into the h2 review bar");
  assert.equal(change.drafts.size, 0, "the rejected h1 drafts stay dropped");

  // A subsequent approval on h2 must post h2 feedback — not the h1 text — and
  // name the head the reviewer is looking at.
  const secondApprove = change.querySelector('[data-review-verdict="approve"]');
  const second = change.handleClick({ target: secondApprove, preventDefault() {} });
  posted.push(JSON.parse(reviewCalls[reviewCalls.length - 1].options.body));
  assert.equal(posted[1].head_sha, "def456789abc", "the h2 submission names the displayed head");
  assert.equal(posted[1].body, "", "the h2 submission carries no h1 review text");
  assert.deepEqual(posted[1].comments, [], "the h2 submission carries no h1 draft notes");

  resolveSecond();
  await second;
  change.remove();
  appNode.remove();
});

test("a head move that shares the summary prefix still repaints the diff and binds the submission to the full head", async () => {
  const root = globalThis.document.body;
  let resolveRequest;
  const calls = stubReviewFetch(
    () =>
      new Promise((resolve) => {
        resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      }),
  );
  // Two full SHAs whose 12-character summary prefixes collide, with the same
  // file list and diff totals: only the full head and the diff content differ.
  // The paint identity must still move — otherwise the flow-diff child keeps
  // showing h1's content while _displayedHead — and the submission — name h2.
  const h1 = "abcdef0123450000000000000000000000000000";
  const h2 = "abcdef012345ffffffffffffffffffffffffffff";
  const changeData = (head, marker) => ({
    change: { id: "ch-0001", head_sha: head },
    task: { id: "t-0001" },
    diff: {
      head_sha: head,
      total_files: 1,
      additions: 1,
      deletions: 0,
      files: [{ path: "a.go", hunks: [{ header: "@@ -1 +1 @@", lines: [{ kind: "add", new_line: 1, text: marker }] }] }],
    },
    threads: [],
    review_state: "in_review",
  });
  const { appNode, change } = mountChange(root, changeData(h1, "old source"));
  await flush();
  await flush();
  assert.match(change.querySelector("flow-diff").innerHTML, /old source/, "h1's diff is on screen");

  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h1" });

  // The change advances to h2 with the same summary line, file list, and
  // totals: the panel must repaint anyway so the diff on screen is h2's.
  change.data = changeData(h2, "new source");
  await flush();
  await flush();

  assert.match(change.querySelector("flow-diff").innerHTML, /new source/, "the diff repaints to the newly displayed head");
  assert.doesNotMatch(change.querySelector("flow-diff").innerHTML, /old source/, "the old head's diff is gone");
  assert.equal(change.drafts.size, 0, "the head move drops the drafts written against the old head");

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  const posted = JSON.parse(calls[0].options.body);
  assert.equal(posted.head_sha, h2, "the submission names the full head whose diff is on screen");

  resolveRequest();
  await pending;
  change.remove();
  appNode.remove();
});

test("a delayed successful submission for the old head does not clear the new head's drafts or claim its bar", async () => {
  const root = globalThis.document.body;
  let resolveFirst;
  let resolveSecond;
  let calls = 0;
  const posted = [];
  const reviewCalls = stubReviewFetch(() => {
    calls += 1;
    if (calls === 1) {
      // The h1 submission hangs while the reviewer's approval is in flight.
      return new Promise((resolve) => {
        resolveFirst = () => resolve({ ok: true, json: () => Promise.resolve({}) });
      });
    }
    return new Promise((resolve) => {
      resolveSecond = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  // The reviewer writes an overall review against h1 and submits it.
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall feedback written against h1";
  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h1" });
  const approve = change.querySelector('[data-review-verdict="approve"]');
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  posted.push(JSON.parse(reviewCalls[reviewCalls.length - 1].options.body));
  assert.equal(posted[0].head_sha, "abc123def456");
  assert.equal(posted[0].body, "overall feedback written against h1");

  // While the submission hangs, the change advances: the poll re-renders h2,
  // which replaces the review bar and drops the h1 drafts.
  change.data = {
    ...reviewChangeData(),
    change: { id: "ch-0001", head_sha: "def456789abc" },
    diff: { ...reviewChangeData().diff, head_sha: "def456789abc" },
  };
  await flush();
  assert.equal(change.drafts.size, 0, "the h2 repaint drops the h1 drafts");

  // The reviewer starts drafting against h2 while the h1 submission is still out.
  change.drafts.set("a.go:1", { path: "a.go", line: 1, body: "note against h2" });

  // The delayed h1 success lands after the repaint. Its settlement belongs to
  // the head it named: it must not wipe the h2 drafts, and the success must
  // not be announced over the h2 bar.
  resolveFirst();
  await pending;
  assert.equal(change.drafts.size, 1, "the h1 settlement leaves the h2 drafts in place");
  assert.equal(change.drafts.get("a.go:1").body, "note against h2");
  assert.equal(change.querySelector("[data-review-body]").value, "", "the h2 review bar stays empty");
  assert.deepEqual(statuses, ["Approving\u2026", ""], "the h1 success is not announced over the h2 bar");

  // A subsequent approval on h2 posts the h2 draft and names h2.
  const secondApprove = change.querySelector('[data-review-verdict="approve"]');
  const second = change.handleClick({ target: secondApprove, preventDefault() {} });
  posted.push(JSON.parse(reviewCalls[reviewCalls.length - 1].options.body));
  assert.equal(posted[1].head_sha, "def456789abc", "the h2 submission names the displayed head");
  assert.equal(posted[1].body, "", "the h2 submission carries no h1 review text");
  assert.deepEqual(posted[1].comments, [{ file_path: "a.go", line: 1, body: "note against h2" }], "the h2 submission carries the h2 draft");

  resolveSecond();
  await second;
  change.remove();
  appNode.remove();
});

test("a failed review keeps the error on the status line and restores the button", async () => {
  const root = globalThis.document.body;
  stubReviewFetch(() =>
    Promise.resolve({
      ok: false,
      status: 409,
      json: () => Promise.resolve({ error: { message: "change already merged" } }),
    }),
  );
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  const bodyInput = change.querySelector("[data-review-body]");
  const pending = change.handleClick({ target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(bodyInput.disabled, true, "the overall-comment input is disabled while pending");
  await pending;

  assert.deepEqual(statuses, ["Approving\u2026", "change already merged"]);
  assert.equal(approve.disabled, false);
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(bodyInput.disabled, false, "the overall-comment input is restored after failure");
  assert.equal(inFlight.size, 0);
  change.remove();
  appNode.remove();
});

// --- inline thread reply pending state (buttonless form) --------------------

function inlineThreadData(id = "th-0001") {
  const now = new Date().toISOString();
  return {
    thread: {
      id,
      state: "open",
      created_at: now,
      comments: [{ actor: "reviewer", body: "Please add a test", created_at: now }],
    },
    change: { id: "ch-0001", head_sha: "abc123def456" },
  };
}

// The production thread reply form (renderInlineThread) has no submit
// control: a lone text input that submits implicitly on Enter. Its pending
// state must land on that input, and a repaint replacement must inherit it.
test("an inline thread reply marks its text input busy and suppresses a duplicate Enter", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData());
  await flush();

  const form = thread.querySelector("form[data-thread-reply-form]");
  assert.ok(form, "the thread renders its reply form");
  assert.equal(form.querySelector('[type="submit"]'), null, "the reply form is buttonless");
  const input = form.querySelector("input");
  input.value = "On it";

  const pending = handleFormSubmit(appNode, { target: form, preventDefault() {} });

  // Synchronous pending state on the live control, before the network
  // resolves: the input is disabled, aria-busy, and visibly busy, and the
  // status line names the in-flight reply.
  assert.equal(input.disabled, true);
  assert.equal(input.getAttribute("aria-busy"), "true");
  assert.equal(input.classList.contains("is-busy"), true);
  assert.deepEqual(statuses, ["Posting reply\u2026"]);

  // A repaint replacing the form re-applies the busy state to the
  // replacement's input — the replacement must not look actionable.
  thread.invalidate();
  await flush();
  const repaintedForm = thread.querySelector("form[data-thread-reply-form]");
  const repaintedInput = repaintedForm && repaintedForm.querySelector("input");
  assert.ok(repaintedInput && repaintedInput !== input, "the reply form was replaced by the repaint");
  assert.equal(repaintedInput.disabled, true);
  assert.equal(repaintedInput.getAttribute("aria-busy"), "true");
  assert.equal(repaintedInput.classList.contains("is-busy"), true);

  // A second Enter while the first reply is still in flight issues no
  // duplicate request.
  await handleFormSubmit(appNode, { target: repaintedForm, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate reply while the first is in flight");

  resolveRequest();
  await pending;

  // Settling restores whatever control is on screen now — the repaint-marked
  // replacement — and the confirmation reaches the status line.
  assert.equal(repaintedInput.disabled, false);
  assert.equal(repaintedInput.getAttribute("aria-busy"), null);
  assert.equal(repaintedInput.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, ["Posting reply\u2026", "Reply posted"]);
  assert.equal(inFlight.size, 0);
  thread.remove();
  appNode.remove();
});

// --- inline thread claim pending state -------------------------------------

// The render path must suppress the claim row itself while the shared claim
// key is in flight: a poll repaint rebuilds the row from scratch, and only the
// registry (not the discarded clicked node) survives to re-suppress the fresh
// buttons. Different threads keep their own key, so one thread's pending claim
// never touches another thread's buttons.
test("claim buttons render disabled while that thread's claim is pending", () => {
  const data = inlineThreadData();
  const html = renderInlineThread(data.thread, data.change);
  assert.match(html, /data-thread-claim="th-0001" data-claim-kind="fixed"/);
  assert.doesNotMatch(html, /data-thread-claim="th-0001"[^>]*disabled/);

  inFlight.add("threadClaim:th-0001");
  try {
    const pendingHtml = renderInlineThread(data.thread, data.change);
    for (const kind of ["fixed", "not_warranted", "superseded"]) {
      assert.match(pendingHtml, new RegExp(`data-thread-claim="th-0001" data-claim-kind="${kind}" disabled aria-busy="true"`));
    }
    assert.equal((pendingHtml.match(/class="button secondary is-busy"/g) || []).length, 3);

    const otherHtml = renderInlineThread(inlineThreadData("th-0002").thread, data.change);
    assert.doesNotMatch(otherHtml, /data-thread-claim="th-0002"[^>]*disabled/, "another thread's claims stay enabled");
  } finally {
    inFlight.delete("threadClaim:th-0001");
  }
});

// The Now card can show claim buttons for the same open thread the Change tab's
// inline row does. While the thread's claim is pending, the card's controls
// must render disabled too — an unchanged poll otherwise repaints an
// apparently actionable second claim surface.
test("the Now card renders its thread-claim controls disabled while that thread's claim is pending", () => {
  const card = nowCardModel({
    wait: null,
    openThreads: 1,
    threads: [{ id: "th-0001", state: "open", file_path: "internal/lifecycle/engine.go", line: 212, comments: [] }],
    change: { head_sha: "abc" },
  });
  assert.ok(card, "an open thread produces a Now card");
  const html = renderNowCard(card, { id: "t-0001", projectID: "p-1" });
  assert.match(html, /data-thread-claim="th-0001" data-claim-kind="fixed"/);
  assert.doesNotMatch(html, /data-thread-claim="th-0001"[^>]*disabled/);

  inFlight.add("threadClaim:th-0001");
  try {
    const pendingHtml = renderNowCard(card, { id: "t-0001", projectID: "p-1" });
    for (const kind of ["fixed", "not_warranted"]) {
      assert.match(pendingHtml, new RegExp(`data-thread-claim="th-0001" data-claim-kind="${kind}" disabled aria-busy="true"`));
    }
    assert.equal((pendingHtml.match(/is-busy/g) || []).length, 2, "both card claim buttons carry the is-busy class");

    const other = nowCardModel({
      wait: null,
      openThreads: 1,
      threads: [{ id: "th-0002", state: "open", file_path: "a.go", line: 2, comments: [] }],
      change: { head_sha: "abc" },
    });
    const otherHtml = renderNowCard(other, { id: "t-0001", projectID: "p-1" });
    assert.doesNotMatch(otherHtml, /data-thread-claim="th-0002"[^>]*disabled/, "another thread's card claims stay enabled");
  } finally {
    inFlight.delete("threadClaim:th-0001");
  }
});

test("an inline thread claim stays single-flight across a repaint and re-enables on success", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData());
  const other = mountElement(appNode, "flow-inline-thread", inlineThreadData("th-0002"));
  await flush();

  const first = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  const pending = handleAction(appNode, { target: first, preventDefault() {} });
  assert.equal(requests, 1);

  // The clicked claim and its siblings for the same thread are all suppressed
  // synchronously, before the request resolves.
  assert.equal(first.disabled, true);
  assert.equal(first.getAttribute("aria-busy"), "true");
  assert.equal(first.classList.contains("is-busy"), true);
  const sibling = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="not_warranted"]');
  assert.equal(sibling.disabled, true, "a sibling claim is suppressed while one is pending");
  assert.deepEqual(statuses, ["Claiming thread th-0001\u2026"]);

  // A different thread's claims stay enabled while th-0001's POST is pending.
  const otherButton = other.querySelector('[data-thread-claim="th-0002"][data-claim-kind="fixed"]');
  assert.equal(otherButton.hasAttribute("disabled"), false);

  // A poll repaint replaces the claim row: the fresh buttons render disabled
  // because the render path consults the shared registry, and clicking a
  // replacement issues no duplicate POST.
  thread.invalidate();
  await flush();
  const repainted = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  assert.ok(repainted && repainted !== first, "the claim row was replaced by the repaint");
  assert.equal(repainted.hasAttribute("disabled"), true, "the replacement renders disabled while the claim is pending");
  assert.equal(repainted.hasAttribute("aria-busy"), true);
  assert.equal(repainted.classList.contains("is-busy"), true);
  await handleAction(appNode, { target: repainted, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate claim while the first is in flight");

  // The repaint replaced the controls whose restores the click captured, so
  // settlement re-enables whatever is live in the document for this thread.
  // Route the settle-time restore through the fake document to the live row.
  const docQuery = globalThis.document.querySelectorAll;
  globalThis.document.querySelectorAll = (selector) =>
    selector === "[data-thread-claim]" ? thread.querySelectorAll(selector) : [];
  try {
    resolveRequest();
    await pending;
  } finally {
    globalThis.document.querySelectorAll = docQuery;
  }

  assert.equal(repainted.disabled, false, "the repaint replacement re-enables on success");
  assert.equal(repainted.getAttribute("aria-busy"), null);
  assert.equal(repainted.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, ["Claiming thread th-0001\u2026", "Thread claimed"]);
  assert.equal(inFlight.size, 0);
  thread.remove();
  other.remove();
  appNode.remove();
});

test("a failed thread claim leaves no live replacement disabled", async () => {
  const root = globalThis.document.body;
  let rejectRequest;
  stubReviewFetch(() => new Promise((resolve, reject) => {
    rejectRequest = () => reject(new Error("boom"));
  }));
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData());
  await flush();

  const first = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  const pending = handleAction(appNode, { target: first, preventDefault() {} });

  // A repaint mid-flight replaces the row with render-time-disabled buttons
  // that never registered a restore (applyBusyState skips already-busy nodes).
  thread.invalidate();
  await flush();
  const repainted = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  assert.ok(repainted && repainted !== first, "the claim row was replaced by the repaint");
  assert.equal(repainted.hasAttribute("disabled"), true);

  const docQuery = globalThis.document.querySelectorAll;
  globalThis.document.querySelectorAll = (selector) =>
    selector === "[data-thread-claim]" ? thread.querySelectorAll(selector) : [];
  try {
    rejectRequest();
    await pending;
  } finally {
    globalThis.document.querySelectorAll = docQuery;
  }

  // Failure issues no refresh, so nothing else repaints the row; the live
  // replacements must be restored directly, not left busy until a later poll.
  assert.equal(repainted.disabled, false);
  assert.equal(repainted.getAttribute("aria-busy"), null);
  assert.equal(repainted.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, ["Claiming thread th-0001\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
  thread.remove();
  appNode.remove();
});

test("a thread claim whose id contains selector metacharacters still suppresses and re-enables across a repaint", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  // Interpolated into a selector this id closes the attribute and opens a
  // second one matching any control whose data-thread-claim is "th-0002";
  // escapeAttr keeps it a plain dataset value in the real DOM instead.
  const threadID = 'th-1"][data-thread-claim="th-0002';
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData(threadID));
  const other = mountElement(appNode, "flow-inline-thread", inlineThreadData("th-0002"));
  await flush();

  const fixed = [...thread.querySelectorAll("[data-thread-claim]")]
    .find((button) => button.dataset?.claimKind === "fixed");
  assert.ok(fixed, "the hostile id survives escapeAttr rendering");
  assert.equal(fixed.dataset.threadClaim, threadID);

  const pending = handleAction(appNode, { target: fixed, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(fixed.disabled, true);
  assert.equal(fixed.classList.contains("is-busy"), true);
  for (const button of thread.querySelectorAll("[data-thread-claim]")) {
    assert.equal(button.disabled, true, "every same-thread claim is suppressed despite the hostile id");
  }
  const otherFixed = [...other.querySelectorAll("[data-thread-claim]")]
    .find((button) => button.dataset?.claimKind === "fixed");
  assert.equal(otherFixed.hasAttribute("disabled"), false, "a different thread's claim is not suppressed by an injected selector");
  assert.equal(inFlight.has(`threadClaim:${threadID}`), true);

  // A poll repaint replaces the claim row: the fresh buttons render disabled
  // because the render path consults the shared registry, and settlement must
  // restore the live replacement for this thread only.
  thread.invalidate();
  await flush();
  const repainted = [...thread.querySelectorAll("[data-thread-claim]")]
    .find((button) => button.dataset?.claimKind === "fixed");
  assert.ok(repainted && repainted !== fixed, "the claim row was replaced by the repaint");
  assert.equal(repainted.hasAttribute("disabled"), true, "the replacement renders disabled while the claim is pending");
  assert.equal(repainted.hasAttribute("aria-busy"), true);

  // Route the settle-time restore through the fake document to the live row:
  // any selector built from the thread id must be rejected, only the broad
  // control selector is legal.
  const docQuery = globalThis.document.querySelectorAll;
  globalThis.document.querySelectorAll = (selector) =>
    selector === "[data-thread-claim]" ? thread.querySelectorAll(selector) : [];
  try {
    resolveRequest();
    await pending;
  } finally {
    globalThis.document.querySelectorAll = docQuery;
  }

  assert.equal(repainted.disabled, false, "the repaint replacement re-enables on success despite the hostile id");
  assert.equal(repainted.getAttribute("aria-busy"), null);
  assert.equal(repainted.classList.contains("is-busy"), false);
  assert.equal(otherFixed.hasAttribute("disabled"), false, "a different thread's claim stays enabled after settlement");
  assert.deepEqual(statuses, [`Claiming thread ${threadID}\u2026`, "Thread claimed"]);
  assert.equal(inFlight.size, 0);
  thread.remove();
  other.remove();
  appNode.remove();
});

test("an inline claim suppresses the Now-card claim controls across an unchanged poll", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let resolveRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  });
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);

  // The Change tab's inline thread and the Now card show claim controls for
  // the same open thread at the same time.
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData());
  const model = {
    wait: null,
    openThreads: 1,
    threads: [{
      id: "th-0001",
      state: "open",
      file_path: "internal/lifecycle/engine.go",
      line: 212,
      created_at: new Date().toISOString(),
      comments: [{ actor: "reviewer", body: "Please add a test" }],
    }],
    change: { head_sha: "abc123def456" },
  };
  const card = mountElement(appNode, "flow-now-card", { card: nowCardModel(model), model });
  await flush();

  const cardClaim = card.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  assert.ok(cardClaim, "the Now card renders a claim control for the open thread");
  assert.equal(cardClaim.hasAttribute("disabled"), false);

  const inlineClaim = thread.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  const pending = handleAction(appNode, { target: inlineClaim, preventDefault() {} });
  assert.equal(requests, 1);

  // The card's same-thread claim is suppressed at click time, not just the
  // clicked row: suppression is document-wide across surfaces.
  assert.equal(cardClaim.disabled, true);
  assert.equal(cardClaim.getAttribute("aria-busy"), "true");
  assert.equal(cardClaim.classList.contains("is-busy"), true);

  // An unchanged poll (identical card payload) must not leave the card's claim
  // enabled: the render path consults the shared registry, so the repaint
  // re-emits the card's claims disabled even though the payload didn't change.
  card.data = { card: nowCardModel(model), model };
  await flush();
  const repainted = card.querySelector('[data-thread-claim="th-0001"][data-claim-kind="fixed"]');
  assert.ok(repainted && repainted !== cardClaim, "the unchanged poll repainted the card's claim control");
  assert.equal(repainted.hasAttribute("disabled"), true, "the Now-card claim renders disabled while the claim is pending");
  assert.equal(repainted.getAttribute("aria-busy"), "true");
  assert.equal(repainted.classList.contains("is-busy"), true);

  resolveRequest();
  await pending;

  // Settlement restores the live replacement in the card and the inline row.
  assert.equal(repainted.disabled, false);
  assert.equal(repainted.getAttribute("aria-busy"), null);
  assert.equal(repainted.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, ["Claiming thread th-0001\u2026", "Thread claimed"]);
  assert.equal(inFlight.size, 0);
  card.remove();
  thread.remove();
  appNode.remove();
});

test("a thread claim with a selector-hostile id stays single-flight and restores cleanly", async () => {
  const root = globalThis.document.body;
  let requests = 0;
  let rejectRequest;
  stubReviewFetch(() => {
    requests += 1;
    return new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  });
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const threadID = 'th-1"][data-unrelated]';
  const thread = mountElement(appNode, "flow-inline-thread", inlineThreadData(threadID));
  await flush();

  const first = thread.querySelector('[data-thread-claim][data-claim-kind="fixed"]');
  assert.ok(first, "the hostile id still renders a claim control");
  const pending = handleAction(appNode, { target: first, preventDefault() {} });
  assert.equal(requests, 1, "the claim POST starts despite the hostile id");
  assert.equal(first.disabled, true);

  // A repaint mid-flight rebuilds the row. The render path derives suppression
  // from the registry (a Map key, never a selector), so the replacement
  // renders disabled even though the id cannot appear in a CSS selector, and
  // clicking it issues no duplicate POST.
  thread.invalidate();
  await flush();
  const repainted = thread.querySelector('[data-thread-claim][data-claim-kind="fixed"]');
  assert.ok(repainted && repainted !== first, "the claim row was replaced by the repaint");
  assert.equal(repainted.hasAttribute("disabled"), true);
  assert.equal(repainted.hasAttribute("aria-busy"), true);
  await handleAction(appNode, { target: repainted, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate claim while the first is in flight");

  rejectRequest();
  await pending;

  // The settle-time restore scans the document with the broad selector and
  // matches by dataset value, so the live replacement re-enables despite the
  // hostile id.
  assert.equal(repainted.disabled, false, "the live replacement restores after failure");
  assert.equal(repainted.getAttribute("aria-busy"), null);
  assert.equal(repainted.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, [`Claiming thread ${threadID}\u2026`, "boom"]);
  assert.equal(inFlight.size, 0);
  thread.remove();
  appNode.remove();
});

test("a non-Error review rejection still drains the registry and shows a final failure", async () => {
  const root = globalThis.document.body;
  stubReviewFetch(() => Promise.reject(null));
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  await change.handleClick({ target: approve, preventDefault() {} });

  assert.deepEqual(statuses, ["Approving\u2026", "Request failed"]);
  assert.equal(approve.disabled, false, "the verdict control is restored");
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a non-Error rejection");
  change.remove();
  appNode.remove();
});

// A verdict POST can reject with a Proxy whose traps throw while the
// settlement path merely formats it; the review submission must still settle
// the key, restore the control, and show a safe failure message.
test("a hostile review rejection still drains the registry and shows a safe failure", async () => {
  const root = globalThis.document.body;
  const hostile = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") throw new Error("message trap");
      return Reflect.get(target, prop);
    },
  });
  stubReviewFetch(() => Promise.reject(hostile));
  const { appNode, change, statuses } = mountChange(root, reviewChangeData());
  await flush();

  const approve = change.querySelector('[data-review-verdict="approve"]');
  await change.handleClick({ target: approve, preventDefault() {} });

  assert.deepEqual(statuses, ["Approving\u2026", "Request failed"]);
  assert.equal(approve.disabled, false, "the verdict control is restored");
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a hostile rejection");
  change.remove();
  appNode.remove();
});

// --- Change-tab revalidation lifecycle (flow-task-detail) -------------------
//
// A task poll delivers a brand-new model object every interval, so the Change
// tab caches its /v2/changes/:id + /diff pair behind a task:change:head key and
// revalidates in place on a same-key poll. These tests pin the head-move
// behaviour: a revalidation that discovers a newer head re-keys the cache to
// it, so the poll that reports that same head neither reloads nor flashes
// "Loading change", while a genuine later head change still reloads and drops
// old-head drafts.

function taskDetailModel(head) {
  return taskModel({
    task: { id: "t-0001", title: "Fix the thing", state: "working" },
    project_id: "p-1",
    task_detail: { ready_change: { id: "ch-0001", head_sha: head } },
  }, null);
}

function changeDetailResponse(head, files) {
  return {
    change: { id: "ch-0001", head_sha: head },
    task: { id: "t-0001" },
    threads: [],
    review_state: "in_review",
    diff: { head_sha: head, files },
  };
}

function diffFiles(marker) {
  return [{ path: `${marker}.go`, hunks: [{ header: "@@ -1 +1 @@", lines: [{ kind: "add", new_line: 1, text: marker }] }] }];
}

// stubChangeFetch serves the change and diff endpoints from a mutable `state`
// ({ head, files }) and records every request path, so a test can assert both
// what is on screen and exactly which fetches ran.
function stubChangeFetch(state) {
  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    const head = state.head;
    const files = state.files;
    if (path.endsWith("/diff")) return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ head_sha: head, files }) });
    return Promise.resolve({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: head }, task: { id: "t-0001" }, threads: [], review_state: state.reviewState ?? "in_review" }),
    });
  };
  return calls;
}

async function settleChange(detail) {
  // Let the in-flight change/revalidation fetch and its follow-up repaint run.
  await detail.changePromise;
  await flush();
}

// The panel shell holds a <flow-change> child; the rendered diff lives inside
// that child, not in the panel's own (verbatim) markup.
function changePanelHTML(detail) {
  return detail.querySelector("flow-change")?.innerHTML || "";
}

async function mountTaskDetail(root, head) {
  const detail = mountElement(root, "flow-task-detail", taskDetailModel(head));
  await flush();
  detail.querySelector("flow-tab-strip").select("change");
  await flush();
  return detail;
}

test("a same-head task poll revalidates in place without a reload or a loading flash", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A poll reports the same head with a brand-new model object.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  const after = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(after, [...initial, "/ui/api/v2/changes/ch-0001"], "a same-head poll revalidates the change only; no /diff re-fetch");
  assert.match(changePanelHTML(detail), /h1\.go/, "the change stays rendered");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "no loading flash on a same-head poll");
  detail.remove();
});

test("a same-head metadata refresh that changes markup keeps an unblurred inline draft", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /in review/, "the initial metadata renders the review-state badge");

  // Open an inline draft and type into it without blurring: the keystrokes
  // live only in the DOM until the next capture, so a repaint that replaces
  // the editor would discard them.
  const change = detail.querySelector("flow-change");
  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "unblurred note";
  assert.equal(change.drafts.get("h1.go:1").body, "", "the keystrokes have not been captured yet");

  // The refreshed metadata changes the rendered markup (a review-state flip),
  // so the revalidation's same-head repaint rewrites the element's DOM.
  state.reviewState = "changes_requested";
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /changes requested/, "the refreshed review state rendered");
  const fresh = detail.querySelector("flow-change").querySelector("[data-draft-body]");
  assert.notEqual(fresh, textarea, "the markup-changing repaint replaced the draft editor");
  assert.match(fresh.textContent, /unblurred note/, "the unblurred draft text survives the repaint");
  assert.equal(detail.querySelector("flow-change").drafts.get("h1.go:1").body, "unblurred note", "the live value is captured into the drafts map");
  detail.remove();
});

test("a same-head metadata refresh that changes markup keeps an unsubmitted overall comment", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  const change = detail.querySelector("flow-change");
  const bodyInput = change.querySelector("[data-review-body]");
  bodyInput.value = "overall comment still being written";

  // The refreshed metadata changes the rendered markup (a review-state flip),
  // so the revalidation's same-head repaint rewrites the review bar — and must
  // restore the unsubmitted overall comment into the fresh input.
  state.reviewState = "changes_requested";
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /changes requested/, "the refreshed review state rendered");
  const fresh = detail.querySelector("flow-change").querySelector("[data-review-body]");
  assert.notEqual(fresh, bodyInput, "the markup-changing repaint replaced the comment input");
  assert.equal(fresh.value, "overall comment still being written", "the unsubmitted overall comment survives the revalidation");
  detail.remove();
});

test("a no-op same-head refresh keeps an unblurred draft for a later markup-changing repaint", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /in review/, "the initial metadata renders the review-state badge");

  // Open an inline draft and type into it without blurring: the keystrokes
  // live only in the DOM until the next capture, so a repaint that replaces
  // the editor would discard them.
  const change = detail.querySelector("flow-change");
  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "unblurred note";
  assert.equal(change.drafts.get("h1.go:1").body, "", "the keystrokes have not been captured yet");

  // A poll delivers identical metadata (same head, same review state): the
  // revalidation repaints byte-identical markup, so FlowElement skips the DOM
  // write. The same-key paint must still read the live textarea back into the
  // drafts map — otherwise the next markup-changing repaint rebuilds the
  // editor from an empty draft and loses what was typed.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.equal(change.querySelector("[data-draft-body]"), textarea, "the byte-identical repaint did not replace the editor");
  assert.equal(change.drafts.get("h1.go:1").body, "unblurred note", "the no-op paint captured the live textarea value");

  // A later metadata refresh changes the rendered markup (a review-state
  // flip), so its same-head repaint rewrites the element's DOM from the map.
  state.reviewState = "changes_requested";
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /changes requested/, "the refreshed review state rendered");
  const fresh = detail.querySelector("flow-change").querySelector("[data-draft-body]");
  assert.notEqual(fresh, textarea, "the markup-changing repaint replaced the draft editor");
  assert.match(fresh.textContent, /unblurred note/, "the unblurred draft text survives the no-op repaint and the rebuild");
  assert.equal(detail.querySelector("flow-change").drafts.get("h1.go:1").body, "unblurred note", "the drafts map keeps the captured value");
  detail.remove();
});

test("a standalone route head move drops an unblurred draft instead of retaining it", async () => {
  // renderChangeRoute() mounts into the content container, reusing the
  // existing flow-change element, so a route refresh from h1 to a genuine
  // later head lands on the same instance. Old-head drafts must drop there
  // too: a head move is not a same-head metadata revalidation, so an
  // unblurred h1 textarea must not be captured into the h2 diff or review.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const change = mount(content, "flow-change", changeDetailResponse("h1", diffFiles("h1")));
  await flush();
  assert.match(change.innerHTML, /h1\.go/, "the h1 diff renders");

  // Open an inline draft and type into it without blurring, as on the
  // task-detail same-head path.
  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "old-head note";
  assert.equal(change.drafts.get("h1.go:1").body, "", "the keystrokes have not been captured yet");

  // A route refresh to a genuine later head reuses the same element (mount).
  const reused = mount(content, "flow-change", changeDetailResponse("h2", diffFiles("h2")));
  await flush();

  assert.equal(reused, change, "mount reuses the flow-change element on the standalone route");
  assert.match(change.innerHTML, /h2\.go/, "the new head's diff renders");
  assert.doesNotMatch(change.innerHTML, /h1\.go/, "the old head's diff is gone");
  assert.equal(change.drafts.size, 0, "the head move dropped the old-head draft");
  assert.equal(change.querySelector("[data-draft-body]"), null, "no draft editor survives into the new head");
  assert.equal(change.querySelector("flow-review-bar").data.pendingCount, 0, "the review bar counts no carried-over draft");
  change.remove();
  appNode.remove();
});

test("a PascalCase Change payload head move drops an unblurred draft instead of retaining it", async () => {
  // render()/afterPaint() accept the PascalCase `Change` shape via value(), so
  // the paint key must too: a key derived only from the lowercase `change` form
  // is "" for this payload, which used to send every repaint — including a
  // genuine head move — down the same-key capture path.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const change = mount(content, "flow-change", {
    Change: { id: "ch-0001", HeadSHA: "h1" },
    diff: { head_sha: "h1", files: diffFiles("h1") },
    threads: [],
    review_state: "in_review",
  });
  await flush();
  assert.match(change.innerHTML, /h1\.go/, "the PascalCase-shaped h1 diff renders");

  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "old-head note";

  mount(content, "flow-change", {
    Change: { id: "ch-0001", HeadSHA: "h2" },
    diff: { head_sha: "h2", files: diffFiles("h2") },
    threads: [],
    review_state: "in_review",
  });
  await flush();

  assert.match(change.innerHTML, /h2\.go/, "the new head's diff renders");
  assert.equal(change.drafts.size, 0, "the head move dropped the old-head draft");
  assert.equal(change.querySelector("[data-draft-body]"), null, "no draft editor survives into the new head");
  change.remove();
  appNode.remove();
});

test("a headless change response drops an unblurred draft instead of retaining it", async () => {
  // A response that names no head is not the change the drafts were anchored
  // to: the empty paint key must clear drafts rather than capture them.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const change = mount(content, "flow-change", changeDetailResponse("h1", diffFiles("h1")));
  await flush();
  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "old-head note";

  mount(content, "flow-change", { change: { id: "ch-0001" }, task: { id: "t-0001" }, threads: [], review_state: "in_review" });
  await flush();

  assert.equal(change.drafts.size, 0, "the headless response dropped the old-head draft");
  assert.equal(change.querySelector("[data-draft-body]"), null, "no draft editor survives into the headless response");
  assert.equal(change.querySelector("flow-review-bar").data.pendingCount, 0, "the review bar counts no carried-over draft");
  change.remove();
  appNode.remove();
});

test("a head move that renders byte-identical markup still clears the draft editor and pending count", async () => {
  // The head summary abbreviates the SHA to 12 characters, so a moved head
  // sharing that prefix renders the same flow-change innerHTML. FlowElement
  // skips the write for identical HTML, so the paint key change must force it
  // or the old draft editor and review-bar count stay mounted in the DOM.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);
  const h1 = "aaaaaaaaaaaa11111111111111111111111111111111";
  const h2 = "aaaaaaaaaaaa22222222222222222222222222222222";
  const files = diffFiles("same");

  const change = mount(content, "flow-change", changeDetailResponse(h1, files));
  await flush();
  change.handleClick({ target: change.querySelector("[data-comment-line]"), preventDefault() {} });
  await flush();
  const textarea = change.querySelector("[data-draft-body]");
  assert.ok(textarea, "the draft editor is on screen");
  textarea.value = "old-head note";
  assert.equal(change.drafts.get("same.go:1").body, "", "the keystrokes have not been captured yet");

  mount(content, "flow-change", changeDetailResponse(h2, files));
  await flush();

  assert.equal(change.drafts.size, 0, "the head move dropped the old-head draft");
  assert.equal(change.querySelector("[data-draft-body]"), null, "the forced write removed the draft editor");
  assert.equal(change.querySelector("flow-review-bar").data.pendingCount, 0, "the review bar counts no carried-over draft");
  change.remove();
  appNode.remove();
});

test("the standalone change route retries a metadata/diff pair until it is verified for one head", async () => {
  // The change advanced between the two GETs: the metadata names h1 while
  // /diff answers for h2. Mounting that pair would show h2's code under h1's
  // metadata — and a same-key repaint would carry an h1 draft onto the h2
  // diff. The route must retry to a coherent pair instead.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const script = [
    { change: changeResponse("h1"), diff: diffResponse("h2", diffFiles("h2")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ];
  const calls = [];
  let index = 0;
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      // The diff pairs with the change fetch that preceded it (index already
      // advanced past it).
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(script[Math.max(0, index - 1)].diff) });
    }
    const step = script[Math.min(index, script.length - 1)];
    index += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.change) });
  };

  const rendered = await renderChangeRoute({ setTitle() {}, querySelector: () => content }, "ch-0001");
  assert.equal(rendered, true, "the route mounts once the pair verifies");
  const change = content.querySelector("flow-change");
  assert.ok(change, "a verified pair mounts");
  assert.match(change.innerHTML, /h2\.go/, "the retried pair renders the diff the metadata now names");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")),
    [
      "/ui/api/v2/changes/ch-0001",
      "/ui/api/v2/changes/ch-0001/diff",
      "/ui/api/v2/changes/ch-0001",
      "/ui/api/v2/changes/ch-0001/diff",
    ],
    "the mismatched pair is retried as a fresh read",
  );
  change.remove();
  appNode.remove();
});

test("the standalone change route refuses to mount a pair whose diff never names its metadata head", async () => {
  // Metadata stays on h1 while /diff keeps answering for h2: after the
  // retries the route fails instead of mounting the unverified pair, which
  // would carry the h1 same-key capture onto the h2 diff.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(diffResponse("h2", diffFiles("h2"))) });
    }
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(changeResponse("h1")) });
  };

  await assert.rejects(
    renderChangeRoute({ setTitle() {}, querySelector: () => content }, "ch-0001"),
    /advanced while it was loading/,
  );
  assert.equal(content.querySelector("flow-change"), null, "no unverified pair is mounted");
  assert.equal(calls.filter((path) => path.includes("/v2/changes/ch-0001")).length, 6, "all three attempts read a fresh pair");
  appNode.remove();
});

test("the standalone change route mounts a headless change as-is with no diff fetch", async () => {
  // Metadata that names no head cannot anchor a verified pair, but the change
  // is still real: the route mounts it as-is with an explicit empty diff
  // instead of fetching /diff or retrying into the "advanced" error. The
  // element-level headless handling is covered above; this exercises the
  // route's own branch, including that no /diff request is issued.
  const root = globalThis.document.body;
  const appNode = globalThis.document.createElement("flow-app");
  const content = globalThis.document.createElement("div");
  appNode.appendChild(content);
  root.appendChild(appNode);

  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(changeResponse("")) });
  };

  const rendered = await renderChangeRoute({ setTitle() {}, querySelector: () => content }, "ch-0001");
  assert.equal(rendered, true, "the route resolves rather than throwing on a headless change");
  const change = content.querySelector("flow-change");
  assert.ok(change, "the headless change's metadata mounts as flow-change");
  assert.equal(change.data.change.id, "ch-0001", "the metadata mounts as-is");
  assert.deepEqual(change.data.diff, {}, "the headless change mounts with an explicit empty diff");
  assert.deepEqual(
    calls,
    ["/ui/api/v2/changes/ch-0001"],
    "a headless change fetches metadata only — no /diff request",
  );
  change.remove();
  appNode.remove();
});

test("a revalidation head move re-keys the cache so the matching poll does not reload or flash", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));

  // The change advances server-side before the poll reports it; the next
  // same-head poll revalidates and discovers the new head.
  state.head = "h2";
  state.files = diffFiles("h2");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h2\.go/, "the revalidation renders the new head's diff");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "the old head's diff is gone — one head only");

  // The following poll reports the head the cache already holds.
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h2\.go/);
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the matching poll must not flash the loader");
  const reloads = calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length);
  assert.deepEqual(reloads, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "only the revalidation fetched; the matching poll fetched nothing");
  detail.remove();
});

test("a poll that arrives while a moved-head revalidation verifies its diff keeps the prior pair and does not reload", async () => {
  const root = globalThis.document.body;
  // The revalidation's /diff is deferred so a task poll can report the new head
  // in the window after the metadata GET returns it but before the diff
  // verifies — the exact ordering that used to reset the coherent cache.
  let resolveDiff;
  const diffGate = new Promise((resolve) => {
    resolveDiff = resolve;
  });
  // The initial load serves a coherent h1 pair; the revalidation serves h2
  // metadata whose diff is deferred behind diffGate until the test releases it.
  const script = [
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")), defer: true },
  ];
  const calls = [];
  let index = 0;
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      // The diff pairs with the change fetch that preceded it (index already
      // advanced past it).
      const step = script[Math.max(0, Math.min(index - 1, script.length - 1))];
      const response = { ok: true, status: 200, json: () => Promise.resolve(step.diff) };
      if (step.defer) return diffGate.then(() => response);
      return Promise.resolve(response);
    }
    const step = script[Math.min(index, script.length - 1)];
    index += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.change) });
  };
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A same-head poll revalidates; the metadata comes back for h2 and the diff
  // fetch starts but has not verified yet. Pump microtasks until the
  // revalidation has fetched the moved head's metadata and parked on its
  // deferred diff (bounded, so a regression fails the assert rather than hangs).
  detail.data = taskDetailModel("h1");
  for (let i = 0; i < 50 && detail.changePendingKey !== "ch-0001:h2"; i++) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the moved head is pending while its diff verifies");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key does not move before the diff verifies");
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior coherent pair stays on screen");
  const generationBeforePoll = detail.changeGeneration;

  // The task poll reports h2 in that window. It must not reset the cache: no
  // "Loading change" flash, no second full load, and the in-flight revalidation
  // is not invalidated.
  detail.data = taskDetailModel("h2");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior pair stays on screen through the matching poll");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the matching poll must not flash the loader");
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the pending marker survives the matching poll");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key is unchanged by the matching poll");
  assert.equal(detail.changeGeneration, generationBeforePoll, "the matching poll did not reset the cache (the revalidation is not invalidated)");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "only the revalidation fetched; the matching poll fetched nothing",
  );

  // The deferred diff verifies: the revalidation adopts h2 and re-keys the
  // cache, so the already-reported head renders without another fetch.
  resolveDiff();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the verified head renders once its diff lands");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "one head only");
  assert.equal(detail.changeKey, "ch-0001:h2", "the cache re-keys to the verified head");
  assert.equal(detail.changePendingKey, "", "the pending marker clears once the pair verifies");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the revalidation's single pair is the only fetch; the matching poll added none",
  );

  // A later same-head poll revalidates in place, as usual once caught up.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("repeated polls for a moved head while its revalidation verifies leave no stale bit behind after adoption", async () => {
  const root = globalThis.document.body;
  // The revalidation's /diff is deferred so several task polls can report the
  // new head in the window after the metadata GET returns it but before the
  // diff verifies. Every poll after the first finds the rendered key already
  // on the new head; those polls must not mark the cache stale — the in-flight
  // revalidation already covers that head and adopts it once the diff lands,
  // and a stale bit set in the window would survive adoption (which clears the
  // pending marker but not changeStale) and fire an extra same-head metadata
  // GET.
  const { calls, release } = deferredDiffFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")), defer: true },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A same-head poll revalidates and discovers h2; its diff is held.
  detail.data = taskDetailModel("h1");
  await flush();
  for (let i = 0; i < 50 && detail.changePendingKey !== "ch-0001:h2"; i++) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the moved head is pending while its diff verifies");

  // Two more polls report the pending head. The first re-keys the rendered
  // change; the second (and any later) matches it, which used to mark the
  // cache stale because only the ahead key was excluded.
  detail.data = taskDetailModel("h2");
  await flush();
  assert.equal(detail.changePendingSeen, true, "the first matching poll observes the pending head");
  assert.equal(detail.changeStale, false, "the first matching poll does not mark the cache stale");
  detail.data = taskDetailModel("h2");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior pair stays on screen through repeated matching polls");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "repeated matching polls must not flash the loader");
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the pending marker survives repeated matching polls");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key is unchanged by repeated matching polls");
  assert.equal(detail.changeStale, false, "a poll for the pending head must not mark the cache stale");

  // The deferred diff verifies: adoption clears the pending marker, and no
  // stale bit is left behind to trigger an extra metadata GET.
  release();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the verified head renders once its diff lands");
  assert.equal(detail.changeKey, "ch-0001:h2", "the cache re-keys to the verified head");
  assert.equal(detail.changePendingKey, "", "the pending marker clears once the pair verifies");
  assert.equal(detail.changeStale, false, "adoption leaves no stale bit behind");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the revalidation's single pair is the only fetch; repeated matching polls added none",
  );

  // A later ordinary same-head poll revalidates metadata in place as usual.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a poll for the cached head while a revalidation verifies a different head stays stale and queues the retry after the rejected diff", async () => {
  const root = globalThis.document.body;
  // The revalidation's /diff is deferred so an ordinary poll can report the
  // cached head in the window after the revalidation fetched a different
  // head's metadata but before its diff verifies. That poll is NOT for the
  // pending head — the in-flight revalidation does not cover it — so it must
  // still mark the cache stale. When the diff then fails to verify, the
  // pending marker clears but the poll's stale bit survives and queues the
  // documented retry on the next paint; the final script entry serves that
  // retry with the current head.
  const { calls, release } = deferredDiffFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h3", diffFiles("h3")), defer: true },
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A same-head poll revalidates and discovers h2; its diff is held.
  detail.data = taskDetailModel("h1");
  await flush();
  for (let i = 0; i < 50 && detail.changePendingKey !== "ch-0001:h2"; i++) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the moved head is pending while its diff verifies");

  // The model still reports the cached head h1 in that window. The pending
  // exemption must not cover this poll: it is not for the pending head, so
  // the cache is stale for it, and only its stale bit can queue the retry
  // once the in-flight attempt gives up. No second fetch may start while the
  // revalidation is already in flight, though.
  const inFlight = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior pair stays on screen through the cached-head poll");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the cached-head poll must not flash the loader");
  assert.equal(detail.changePendingKey, "ch-0001:h2", "a poll for the cached head leaves the pending marker alone");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key is unchanged");
  assert.equal(detail.changeStale, true, "a poll for the cached head marks the cache stale even while a different head is pending");
  assert.equal(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).length,
    inFlight,
    "the cached-head poll queues no fetch while the revalidation is in flight",
  );

  // The deferred diff comes back for yet another head (h3): it verifies
  // nothing, so the attempt gives up. The cached-head poll's stale bit
  // survives the cleared pending marker and queues the documented retry on
  // the next paint — a metadata-only GET for the current head.
  release();
  await settleChange(detail);
  if (detail.changePromise) await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "a rejected diff keeps the prior coherent pair on screen");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go|h3\.go/, "the unverified heads are never rendered — one head only");
  assert.equal(detail.changePendingKey, "", "the rejected diff clears the pending marker");
  assert.equal(detail.changeKey, "ch-0001:h1", "the rejected diff does not move the cache key");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff", "/ui/api/v2/changes/ch-0001"],
    "the cached-head poll's stale bit queues one metadata-only retry after the rejected diff",
  );

  // The retry refreshed the pair in place; a later same-head poll revalidates
  // metadata in place as usual.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a poll that rolls back to the cached head while a moved-head revalidation verifies keeps the current head and drops the stale revalidation", async () => {
  const root = globalThis.document.body;
  // The revalidation's /diff is deferred so the model can report the new head
  // and then roll back to the cached head before that diff verifies — the
  // ordering that used to let the stale revalidation adopt the rolled-back head
  // over the model's current head.
  let resolveDiff;
  const diffGate = new Promise((resolve) => {
    resolveDiff = resolve;
  });
  const script = [
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")), defer: true },
    // After the rollback the cache stays stale, so the next same-head poll
    // revalidates the current head; the server still holds h1.
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
  ];
  const calls = [];
  let index = 0;
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      const step = script[Math.max(0, Math.min(index - 1, script.length - 1))];
      const response = { ok: true, status: 200, json: () => Promise.resolve(step.diff) };
      if (step.defer) return diffGate.then(() => response);
      return Promise.resolve(response);
    }
    const step = script[Math.min(index, script.length - 1)];
    index += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.change) });
  };
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A same-head poll revalidates; the metadata comes back for h2 and the diff
  // fetch parks behind diffGate. Pump until the pending marker is set (bounded,
  // so a regression fails the assert rather than hangs).
  detail.data = taskDetailModel("h1");
  for (let i = 0; i < 50 && detail.changePendingKey !== "ch-0001:h2"; i++) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the moved head is pending while its diff verifies");

  // The poll reports h2 in the verification window: the prior pair stays, and
  // the observation is recorded so a later rollback can be recognized.
  detail.data = taskDetailModel("h2");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior pair stays on screen through the matching poll");
  assert.equal(detail.changePendingSeen, true, "the matching poll observes the pending head");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key is unchanged by the matching poll");

  // Before the diff verifies, the model rolls back to h1. This divergent poll
  // must invalidate the stale h2 revalidation: the pending marker clears, but
  // the coherent h1 pair stays on screen with no reload and no flash.
  const generationBeforeRollback = detail.changeGeneration;
  detail.data = taskDetailModel("h1");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the rollback keeps the current head on screen");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the rollback must not flash the loader");
  assert.equal(detail.changePendingKey, "", "the rollback clears the pending marker");
  assert.equal(detail.changePendingSeen, false, "the rollback clears the pending observation");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key stays on the current head");
  assert.equal(detail.changeGeneration, generationBeforeRollback, "the rollback did not reset the cache (the pair is still coherent)");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "only the revalidation fetched; the rollback poll fetched nothing",
  );

  // The deferred h2 diff finally lands. The stale revalidation must NOT adopt
  // it over the model's current h1 head: the pair stays h1 and the cache key
  // does not move. The rollback left the cache stale, so the invalidated
  // attempt's next paint queues the documented metadata-only retry for the
  // current head.
  resolveDiff();
  await settleChange(detail);
  if (detail.changePromise) await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the current head stays on screen after the stale diff lands");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the rolled-back head is never rendered — one head only");
  assert.equal(detail.changeKey, "ch-0001:h1", "the stale revalidation does not adopt the rolled-back head");
  assert.equal(detail.changeAheadKey, "", "no ahead window opens for the rolled-back head");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff", "/ui/api/v2/changes/ch-0001"],
    "the rollback's stale bit queues one metadata-only retry after the invalidated attempt",
  );

  // The retry refreshed the pair in place; a later same-head poll revalidates
  // the current head in place and confirms it.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the revalidated current head stays rendered");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a rollback to the cached head after observing the pending head leaves the cache stale so the rejected diff queues the retry", async () => {
  const root = globalThis.document.body;
  // The revalidation's /diff is deferred so the model can report the moved head
  // (observing it) and then roll back to the cached head before that diff
  // verifies. The rollback clears the observed pending marker, so the in-flight
  // revalidation can no longer adopt h2 — but the rollback poll is a fresh
  // model for the cached head that the revalidation does not cover, so it must
  // leave the cache stale. Without that bit, the failed/mismatched diff would
  // end the attempt with nothing queued and the documented retry would be lost.
  // The final script entry serves that retry with the current head.
  const { calls, release } = deferredDiffFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h3", diffFiles("h3")), defer: true },
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");
  const initial = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(initial, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "one change+diff pair on first load");

  // A same-head poll revalidates and discovers h2; its diff is held.
  detail.data = taskDetailModel("h1");
  await flush();
  for (let i = 0; i < 50 && detail.changePendingKey !== "ch-0001:h2"; i += 1) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the moved head is pending while its diff verifies");

  // The model reports h2 (observing the pending head), then rolls back to the
  // cached h1 before the diff verifies.
  detail.data = taskDetailModel("h2");
  await flush();
  assert.equal(detail.changePendingSeen, true, "the h2 poll observed the pending head");
  const inFlight = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  assert.match(changePanelHTML(detail), /h1\.go/, "the rollback keeps the current head on screen");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the rollback must not flash the loader");
  assert.equal(detail.changePendingKey, "", "the rollback clears the observed pending marker");
  assert.equal(detail.changePendingSeen, false, "the rollback clears the pending observation");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache key stays on the current head");
  assert.equal(detail.changeStale, true, "the rollback leaves the cache stale for the invalidated revalidation");
  assert.equal(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).length,
    inFlight,
    "the rollback poll queues no fetch while the revalidation is in flight",
  );

  // The deferred h2 diff comes back for yet another head (h3): it verifies
  // nothing, so the invalidated attempt gives up. The rollback's stale bit then
  // queues the documented metadata-only retry for the current head.
  release();
  await settleChange(detail);
  if (detail.changePromise) await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the rejected diff keeps the prior coherent pair on screen");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go|h3\.go/, "the unverified heads are never rendered — one head only");
  assert.equal(detail.changePendingKey, "", "the rejected diff leaves the pending marker clear");
  assert.equal(detail.changeKey, "ch-0001:h1", "the rejected diff does not move the cache key");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(initial.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff", "/ui/api/v2/changes/ch-0001"],
    "the rollback's stale bit queues one metadata-only retry after the rejected diff",
  );

  // The retry refreshed the pair in place; a later same-head poll revalidates
  // metadata in place as usual.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

// The pending/ahead reconciliation above must run on every model poll, not only
// while the Change tab is the one being painted. A moved-head revalidation can
// be verifying its diff — or have adopted a head the poll has not reported —
// while another tab is open; polls delivered to that hidden tab must still
// observe the pending head and invalidate a rollback, or a stale revalidation
// would adopt a head the model has already rolled back from.

// deferredDiffFetch serves a scripted sequence of change/diff responses; an
// entry flagged `defer` holds its /diff behind a gate the test releases, so a
// task poll can land in the window after a revalidation fetched a moved head's
// metadata but before its diff verifies.
function deferredDiffFetch(script) {
  let resolveDiff;
  const diffGate = new Promise((resolve) => {
    resolveDiff = resolve;
  });
  const calls = [];
  let index = 0;
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      const step = script[Math.max(0, Math.min(index - 1, script.length - 1))];
      const response = { ok: true, status: 200, json: () => Promise.resolve(step.diff) };
      if (step.defer) return diffGate.then(() => response);
      return Promise.resolve(response);
    }
    const step = script[Math.min(index, script.length - 1)];
    index += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.change) });
  };
  return { calls, release: resolveDiff };
}

test("a hidden-tab poll that rolls back to the cached head while a revalidation verifies keeps the current head and drops the stale revalidation", async () => {
  const root = globalThis.document.body;
  // Initial load serves a coherent h1 pair; the revalidation serves h2 metadata
  // whose diff is deferred; the server then rolls back to h1, so the caught-up
  // poll revalidates the current head in place (no re-adoption).
  const { calls, release } = deferredDiffFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")), defer: true },
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");

  // A same-head poll revalidates and discovers h2; its diff is held.
  detail.data = taskDetailModel("h1");
  await flush();
  for (let i = 0; i < 20 && detail.changePendingKey !== "ch-0001:h2"; i += 1) await flush();
  assert.equal(detail.changePendingKey, "ch-0001:h2", "the revalidation parked on its moved head's deferred diff");

  // The reviewer leaves the Change tab while the diff verifies.
  detail.querySelector("flow-tab-strip").select("overview");
  await flush();

  // Polls now land on a hidden tab: h2 (observes the pending head), then h1
  // (rolls back to the cached head).
  detail.data = taskDetailModel("h2");
  await flush();
  assert.equal(detail.changePendingSeen, true, "the hidden-tab h2 poll observed the pending head");
  detail.data = taskDetailModel("h1");
  await flush();
  assert.equal(detail.changePendingKey, "", "the hidden-tab rollback cleared the pending marker");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cached pair is still the current head");

  // The stale diff finally lands: the revalidation must not adopt h2 over the
  // model's current h1 head.
  release();
  await settleChange(detail);
  assert.equal(detail.changeKey, "ch-0001:h1", "the stale revalidation does not adopt the rolled-back head");
  assert.equal(detail.changeAheadKey, "", "no ahead window opens for the rolled-back head");

  // Reopening the Change tab renders the current head's pair — not the stale
  // h2 pair — and a caught-up same-head poll revalidates in place.
  detail.querySelector("flow-tab-strip").select("change");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the current head renders on reopen");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the rolled-back head is never rendered — one head only");
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("hidden-tab polls that keep naming the pre-adoption head expire the ahead window and reload the current head", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // A revalidation adopts h2 while the poll still reports h1.
  state.head = "h2";
  state.files = diffFiles("h2");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.equal(detail.changeKey, "ch-0001:h2", "the revalidation adopted the new head");
  assert.equal(detail.changeAheadKey, "ch-0001:h1", "the ahead window names the pre-adoption head");

  // A reviewer's pending inline note anchored to the adopted head's diff.
  detail.querySelector("flow-change").drafts.set("h2.go:1", "adopted-head note");

  // The reviewer leaves the Change tab; the server rolls back to h1 before any
  // poll reports h2.
  detail.querySelector("flow-tab-strip").select("overview");
  await flush();
  state.head = "h1";
  state.files = diffFiles("h1");

  // The first hidden-tab h1 poll is inside the ahead window: it keeps the
  // adopted pair (no reset) but counts against the bounded lag.
  detail.data = taskDetailModel("h1");
  await flush();
  assert.equal(detail.changeKey, "ch-0001:h2", "the first hidden-tab poll keeps the ahead pair");

  // A persistent pre-adoption head is a rollback, not lag: the window expires
  // on the next hidden-tab poll and the adopted pair is dropped (the cache no
  // longer holds h2; it reloads the current head when the tab reopens).
  detail.data = taskDetailModel("h1");
  await flush();
  assert.notEqual(detail.changeKey, "ch-0001:h2", "the expired ahead window drops the adopted head");
  assert.equal(detail.changeAheadKey, "", "the ahead marker is gone");

  // Reopening the Change tab reloads the current head and drops the adopted
  // head's drafts.
  detail.querySelector("flow-tab-strip").select("change");
  await flush();
  await settleChange(detail);
  assert.equal(detail.changeKey, "ch-0001:h1", "the reopened tab reloads the current head");
  assert.match(changePanelHTML(detail), /h1\.go/, "the current head's diff is rendered on reopen");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the adopted head's diff is gone — one head only");
  assert.equal(detail.querySelector("flow-change").drafts.size, 0, "the reset rebuilds the element, dropping the adopted head's drafts");
  detail.remove();
});

test("a poll that skips the revalidation's adopted head reloads the later head and drops old-head drafts", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // Revalidation adopts h2 while the poll still reports h1.
  state.head = "h2";
  state.files = diffFiles("h2");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the revalidation renders the adopted head's diff");

  // A reviewer's pending inline note anchored to the adopted head's diff.
  detail.querySelector("flow-change").drafts.set("h2.go:1", "adopted-head note");
  const adopted = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;

  // The change advances to h3 entirely between poll intervals, so the next
  // poll reports h3 directly and never reports h2. The ahead cache must not
  // pin the tab to h2: it has to reload h3 and drop the h2 draft.
  state.head = "h3";
  state.files = diffFiles("h3");
  detail.data = taskDetailModel("h3");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h3\.go/, "the skipped-intermediate poll renders the later head's diff");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the adopted head's diff is gone — one head only");
  assert.equal(detail.querySelector("flow-change").drafts.size, 0, "a genuine later head rebuilds the element, dropping old-head drafts");
  const h3Reload = calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(adopted);
  assert.deepEqual(h3Reload, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"], "the skipped-intermediate poll fetched a fresh change+diff pair");

  // Once the model catches up, the ahead window is over: a same-head poll
  // revalidates in place instead of staying pinned behind a stale ahead flag.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h3");
  await flush();
  await settleChange(detail);
  const revalidate = calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp);
  assert.deepEqual(revalidate, ["/ui/api/v2/changes/ch-0001"], "a caught-up same-head poll revalidates the change only");
  assert.match(changePanelHTML(detail), /h3\.go/, "the change stays rendered");
  detail.remove();
});

test("a genuine later head change reloads the pair and drops old-head drafts", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // A reviewer's pending inline note anchored to the old head's diff.
  detail.querySelector("flow-change").drafts.set("h1.go:1", "old-head note");

  // The poll itself reports the new head.
  state.head = "h2";
  state.files = diffFiles("h2");
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h2\.go/, "the new head's diff is rendered");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "the old head's diff is gone — one head only");
  assert.equal(detail.querySelector("flow-change").drafts.size, 0, "a head move rebuilds the element, dropping old-head drafts");
  detail.remove();
});

test("a rollback to the pre-adoption head reloads the current head and drops the adopted head's drafts", async () => {
  const root = globalThis.document.body;
  const state = { head: "h1", files: diffFiles("h1") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // Revalidation adopts h2 while the poll still reports h1.
  state.head = "h2";
  state.files = diffFiles("h2");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the revalidation renders the adopted head's diff");

  // A reviewer's pending inline note anchored to the adopted head's diff.
  detail.querySelector("flow-change").drafts.set("h2.go:1", "adopted-head note");

  // The server rolls back to h1 before any poll reports h2. The first h1 poll
  // after the adoption is inside the ahead window (the adoption repaint), so it
  // keeps the fresh pair without a reload.
  state.head = "h1";
  state.files = diffFiles("h1");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the adoption repaint keeps the ahead pair");

  // The model keeps naming the pre-adoption head: a rollback, not lag. The
  // bounded exemption must expire and the tab must reload the current head.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h1\.go/, "the rollback reloads the current head's diff");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the stale adopted head's diff is gone — one head only");
  assert.equal(detail.querySelector("flow-change").drafts.size, 0, "the rollback rebuilds the element, dropping the adopted head's drafts");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache re-keys to the current head");
  assert.equal(detail.changeAheadKey, "", "the ahead exemption is cleared once the model rolls back");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the cached current-head pair renders without a loading flash");

  // Once caught up, a same-head poll revalidates in place again.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a coherent load that returns a different head than the model reloads on the next same-head poll", async () => {
  const root = globalThis.document.body;
  // The server already holds h2, but the task poll still reports h1: the
  // initial load fetches a coherent h2 pair and adopts it ahead of the model.
  const state = { head: "h2", files: diffFiles("h2") };
  const calls = stubChangeFetch(state);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the coherent load renders the head the server actually holds");
  assert.equal(detail.changeKey, "ch-0001:h2", "the load adopts the fetched head");

  detail.querySelector("flow-change").drafts.set("h2.go:1", "adopted-head note");

  // The server rolls back to h1 while the model keeps reporting h1. The first
  // h1 poll after the adoption is inside the ahead window, so it keeps the
  // adopted pair; the next one expires the exemption and reloads h1.
  state.head = "h1";
  state.files = diffFiles("h1");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the adoption repaint keeps the ahead pair");
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);

  assert.match(changePanelHTML(detail), /h1\.go/, "the persistent model head reloads and renders");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the adopted head's diff is gone — one head only");
  assert.equal(detail.querySelector("flow-change").drafts.size, 0, "the reload drops the adopted head's drafts");
  assert.equal(detail.changeKey, "ch-0001:h1", "the cache re-keys to the model's head");
  assert.equal(detail.changeAheadKey, "", "the ahead exemption is cleared");
  const diffs = calls.filter((path) => path.includes("/v2/changes/ch-0001/diff"));
  assert.equal(diffs.length, 2, "the rollback re-fetched the model's head with its own diff after the exemption expired");
  detail.remove();
});

// --- ahead-cache rollback / ABA ---------------------------------------------
//
// A SHA has no ordering, so a model that keeps naming the pre-adoption head is
// a rollback (or ABA), not lag. The ahead exemption may cover the adoption
// repaint and a bounded lag window only; once that budget is exhausted the
// cache must reset so the current head reloads and the adopted head's drafts
// drop. Without the bound, a server that returns to the pre-adoption head
// satisfies the exemption on every poll and the tab stays pinned to the stale
// adopted head forever.

// --- unverified revalidation/load responses --------------------------------
//
// A refreshed metadata response can be headless, name the wrong change, or
// arrive with a diff that failed, came back headless, or names another head.
// None of those may replace the cached pair: an unverified metadata head must
// never become the current head, or the matching poll's ahead-key suppression
// would skip the recovery load and render that metadata under a diff verified
// for a different head. The cache keeps its prior coherent pair and the next
// poll retries.

function changeResponse(head, { changeID = "ch-0001" } = {}) {
  return { change: { id: changeID, head_sha: head }, task: { id: "t-0001" }, threads: [], review_state: "in_review" };
}

function diffResponse(head, files) {
  return { head_sha: head, files };
}

// scriptedChangeFetch serves the change and diff endpoints from a `script`
// array of { change, diff } entries, one per change fetch; `diff` may be a
// body, "fail" (a rejected fetch), or undefined (skip the diff fetch). When
// the script runs out, the last entry repeats.
function scriptedChangeFetch(script) {
  const calls = [];
  let index = 0;
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      // The diff pairs with the change fetch that preceded it (index already
      // advanced past it).
      const step = script[Math.max(0, Math.min(index - 1, script.length - 1))];
      if (step.diff === "fail") return Promise.reject(new Error("boom"));
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.diff) });
    }
    const step = script[Math.min(index, script.length - 1)];
    index += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(step.change) });
  };
  return calls;
}

test("a headless revalidation response keeps the cached pair and recovers on the matching poll", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: { change: { id: "ch-0001" }, task: { id: "t-0001" }, threads: [], review_state: "in_review" } },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");

  // The revalidation's metadata comes back without a head. It must not replace
  // the cached pair or advance the cache key.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the headless response keeps the prior pair on screen");
  assert.equal(detail.changeKey, "ch-0001:h1", "a headless response does not move the cache key");
  assert.equal(detail.changeAheadKey, "", "a headless response does not set the ahead key");

  // The poll reports the new head: a full reload fetches a verified h2 pair.
  const before = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the matching poll recovers the new head's diff");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "one head only");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(before),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the matching poll fetched a fresh change+diff pair",
  );
  detail.remove();
});

test("a wrong-change revalidation response keeps the cached pair and recovers on the matching poll", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2", { changeID: "ch-9999" }) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // The revalidation's metadata names another change. It must not replace the
  // cached pair or move the cache key.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the wrong-change response keeps the prior pair on screen");
  assert.equal(detail.changeKey, "ch-0001:h1", "a wrong-change response does not move the cache key");

  const before = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the matching poll recovers the new head's diff");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "one head only");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(before),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the matching poll fetched a fresh change+diff pair",
  );
  detail.remove();
});

test("a revalidation whose diff fails or comes back headless keeps the prior pair and retries on the next poll", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: "fail" },
    { change: changeResponse("h2"), diff: { files: diffFiles("h2") } },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");

  // The moved head's diff fetch fails: the h1 pair stays and no head is adopted.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "a failed diff keeps the prior pair on screen");
  assert.equal(detail.changeKey, "ch-0001:h1", "a failed diff does not move the cache key");
  assert.equal(detail.changeAheadKey, "", "a failed diff does not set the ahead key");

  // The next same-head poll retries; now the diff comes back without a head, so
  // it verifies nothing and the pair still does not move.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "a headless diff keeps the prior pair on screen");
  assert.equal(detail.changeKey, "ch-0001:h1", "a headless diff does not move the cache key");

  // The third poll gets a diff naming h2: the pair adopts and the matching poll
  // neither reloads nor flashes.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "a verified diff adopts the moved head");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "one head only");

  const before = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the matching poll keeps the adopted pair");
  assert.doesNotMatch(changePanelHTML(detail), /Loading change/, "the matching poll must not flash the loader");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(before),
    [],
    "the matching poll fetched nothing; the adopted pair is fresh",
  );

  // Once the model has caught up, the ahead window is over: the next same-head
  // poll revalidates in place again.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a caught-up same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a revalidation whose diff names yet another head keeps the prior pair until a verified one lands", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: diffResponse("h3", diffFiles("h3")) },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // The change moved to h2, but /diff answered for h3 (it moved again between
  // the two GETs). The mismatched diff must not install under h2's metadata.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "a mismatched diff keeps the prior pair on screen");
  assert.doesNotMatch(changePanelHTML(detail), /h3\.go/, "the other head's diff is never rendered");
  assert.equal(detail.changeKey, "ch-0001:h1", "a mismatched diff does not move the cache key");

  // The next poll retries and lands a verified h2 pair.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "a verified diff adopts the moved head");
  assert.doesNotMatch(changePanelHTML(detail), /h1\.go/, "one head only");

  const before = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h2");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the matching poll keeps the adopted pair");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(before),
    [],
    "the matching poll fetched nothing; the adopted pair is fresh",
  );
  detail.remove();
});

test("a load whose metadata never names the selected change shows a retryable error", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1", { changeID: "ch-9999" }) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // Every attempt's metadata names another change, so nothing can install: the
  // tab shows a retryable error rather than another change's metadata.
  const panel = detail.querySelector(".panel");
  assert.match(panel.innerHTML, /advanced while it was loading/, "an unverified load fails with a retryable error");
  assert.match(panel.innerHTML, /data-change-retry/, "the error offers a retry");
  assert.doesNotMatch(panel.innerHTML, /ch-9999/, "the other change's metadata is never rendered");
  assert.equal(
    calls.filter((path) => path.endsWith("/v2/changes/ch-0001")).length,
    3,
    "the load retried its bounded number of reads",
  );

  // A retry that lands a verified pair installs it.
  scriptedChangeFetch([{ change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) }]);
  panel.querySelector("[data-change-retry]").dispatchEvent(new TestEvent("click", { bubbles: true }));
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the retry renders the verified pair");
  detail.remove();
});

test("a headless selected change renders its metadata with an explicit pending diff", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("") },
  ]);
  const detail = await mountTaskDetail(root, "");
  await settleChange(detail);

  // No head exists yet, so no diff can verify: the tab renders the change's
  // metadata with an explicit pending diff instead of a terminal retry error.
  const html = changePanelHTML(detail);
  assert.match(html, /ch-0001/, "the headless change's metadata is rendered");
  assert.match(html, /t-0001/, "the change's task link is rendered");
  assert.match(html, /No diff yet/, "the pending diff state is explicit");
  assert.doesNotMatch(html, /advanced while it was loading/, "a headless change is not a loading error");
  assert.doesNotMatch(detail.querySelector(".panel").innerHTML, /data-change-retry/, "a headless change does not offer a futile retry");
  assert.deepEqual(
    calls.filter((path) => path.endsWith("/v2/changes/ch-0001")),
    ["/ui/api/v2/changes/ch-0001"],
    "a headless load fetches metadata once and skips the diff",
  );

  // Authoring finishes: the next poll reports a head and the cache reloads the
  // verified pair.
  scriptedChangeFetch([{ change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) }]);
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "a head appears and the verified pair loads");
  assert.doesNotMatch(changePanelHTML(detail), /No diff yet/, "the pending state clears once a diff exists");
  detail.remove();
});

test("a persistently unavailable diff renders a recoverable pending state instead of claiming the change advanced", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: "fail" },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // The metadata names a head but /diff keeps failing: the tab renders the
  // metadata with an explicit pending diff rather than the misleading
  // "advanced while loading" error, and no diff from any head is shown.
  const html = changePanelHTML(detail);
  assert.match(html, /ch-0001/, "the headed metadata is rendered");
  assert.match(html, /not available yet/, "the unavailable diff is explicit");
  assert.doesNotMatch(html, /advanced while it was loading/, "an unavailable diff is not a head move");
  assert.doesNotMatch(detail.querySelector(".panel").innerHTML, /data-change-retry/, "the pending state does not offer a terminal retry");
  assert.doesNotMatch(html, /h1\.go/, "no diff is rendered while it is unavailable");
  assert.equal(detail.changeKey, "ch-0001:h1", "the pending pair keeps the model head's key");
  assert.deepEqual(
    calls.filter((path) => path.startsWith("/ui/api/v2/changes/ch-0001")),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the load fetched one metadata+diff attempt before installing the pending pair",
  );

  // The next poll's revalidation retries the diff; it comes back naming a
  // different head, which must not install under this metadata.
  scriptedChangeFetch([{ change: changeResponse("h1"), diff: diffResponse("h2", diffFiles("h2")) }]);
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /not available yet/, "a mismatched diff keeps the pending state");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the other head's diff is never rendered");

  // The diff comes back for the right head on a later poll: the verified pair
  // replaces the pending state.
  scriptedChangeFetch([{ change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) }]);
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the recovered diff renders");
  assert.doesNotMatch(changePanelHTML(detail), /not available yet/, "the pending state clears once the diff lands");
  detail.remove();
});

test("a load whose diff the server explicitly reports unavailable installs a recoverable pending pair", async () => {
  const root = globalThis.document.body;
  // The API's no-diff answer is HTTP 200 naming the head it would diff, with
  // available:false and an unavailable_reason. It must not install as a
  // verified empty diff: the tab renders the pending state and keeps retrying
  // /diff on later same-head polls until the diff becomes available.
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: { head_sha: "h1", available: false, unavailable_reason: "diff not captured" } },
    { change: changeResponse("h1"), diff: { head_sha: "h1", available: false, unavailable_reason: "diff not captured" } },
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // The headed metadata installs with an explicit pending diff, not an empty
  // "verified" diff: the unavailable response named the head, so without the
  // available:false guard it would have passed the head-equality check.
  const html = changePanelHTML(detail);
  assert.match(html, /ch-0001/, "the headed metadata is rendered");
  assert.match(html, /data-change-pending/, "an explicit unavailable response renders the pending diff state");
  assert.match(html, /The diff is not available: diff not captured/, "the empty state surfaces the server's unavailable_reason");
  assert.doesNotMatch(html, /class="files"/, "no bare empty file list renders for an unavailable diff");
  assert.doesNotMatch(html, /advanced while it was loading/, "an unavailable diff is not a head move");
  assert.doesNotMatch(detail.querySelector(".panel").innerHTML, /data-change-retry/, "the pending state does not offer a terminal retry");
  assert.doesNotMatch(html, /h1\.go/, "no diff is rendered while it is unavailable");
  assert.equal(detail.changeKey, "ch-0001:h1", "the pending pair keeps the model head's key");
  assert.deepEqual(
    calls.filter((path) => path.startsWith("/ui/api/v2/changes/ch-0001")),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the load fetched one metadata+diff attempt before installing the pending pair",
  );

  // The next same-head poll's revalidation retries the diff; the server still
  // reports it explicitly unavailable, so the pending pair stays and the diff
  // is retried again rather than being accepted as an empty verified diff.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /The diff is not available: diff not captured/, "a still-unavailable diff keeps the pending state with its reason");
  assert.equal(
    calls.filter((path) => path.endsWith("/diff")).length,
    2,
    "the same-head poll retried the explicitly unavailable diff",
  );

  // The diff becomes available; the same-head poll installs the verified pair
  // in place.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the recovered diff renders");
  assert.doesNotMatch(changePanelHTML(detail), /diff not captured/, "the pending state clears once the diff lands");
  detail.remove();
});

test("a headed unavailable diff renders the server's reason instead of a bare empty file list", async () => {
  const root = globalThis.document.body;
  // The standalone change route mounts the raw /diff answer, so flow-change
  // itself must surface an explicit unavailable response (HTTP 200 naming the
  // head with available:false and an unavailable_reason) as a reasoned empty
  // state — not a bare empty file list that reads as "no files".
  const { appNode, change } = mountChange(root, {
    change: changeResponse("h1"),
    task: { id: "t-0001" },
    threads: [],
    review_state: "in_review",
    diff: { head_sha: "h1", available: false, unavailable_reason: "merge service is not configured" },
  });
  await flush();
  const html = change.innerHTML;
  assert.match(html, /data-change-pending/, "an explicit unavailable response renders the pending empty state");
  assert.match(html, /The diff is not available: merge service is not configured/, "the empty state includes the server's reason");
  assert.doesNotMatch(html, /class="files"/, "no bare empty file list renders");
  assert.doesNotMatch(html, /<flow-diff>/, "no diff pane renders");
  change.remove();
  appNode.remove();
});

test("a reason-less unavailable diff still renders the explicit pending state on the standalone route", async () => {
  const root = globalThis.document.body;
  // The server always pairs available:false with an unavailable_reason, but a
  // reason-less {head_sha, available:false} response must classify identically
  // on the standalone route and the task-detail route (both share the
  // diffUnavailable predicate): pending, with the generic no-diff-yet note —
  // there is no reason to surface — never a bare empty file list.
  assert.equal(diffUnavailable({ head_sha: "h1", available: false }), true, "available:false alone marks a diff unavailable");
  assert.equal(diffUnavailable({ head_sha: "h1", available: false, unavailable_reason: "merge service is not configured" }), true, "a reasoned unavailable diff stays unavailable");
  assert.equal(diffUnavailable({ head_sha: "h1", available: true, files: [], total_files: 0, additions: 0, deletions: 0 }), false, "an available empty diff is usable");
  assert.equal(diffUnavailable(null), true, "a failed fetch is unavailable");
  const { appNode, change } = mountChange(root, { ...changeResponse("h1"), diff: { head_sha: "h1", available: false } });
  await flush();
  const html = change.innerHTML;
  assert.match(html, /data-change-pending/, "a reason-less available:false response renders the pending state");
  assert.match(html, /The diff is not available yet; it will appear here once it is\./, "the pending note falls back to the generic message");
  assert.doesNotMatch(html, /class="files"/, "no bare empty file list renders");
  assert.doesNotMatch(html, /<flow-diff>/, "no diff pane renders");
});

test("a hostile unavailable_reason renders as escaped text, not an injected element", async () => {
  const root = globalThis.document.body;
  // The reason is server-controlled text (it can carry err.Error() from
  // exchangePathForChange), so markup inside it must render inert: the empty
  // state shows the escaped text and parses no element from the reason.
  const hostile = "<img src=x onerror=alert(1)>";
  const { appNode, change } = mountChange(root, {
    change: changeResponse("h1"),
    task: { id: "t-0001" },
    threads: [],
    review_state: "in_review",
    diff: { head_sha: "h1", available: false, unavailable_reason: hostile },
  });
  await flush();
  const html = change.innerHTML;
  assert.match(html, /data-change-pending/, "an explicit unavailable response renders the pending empty state");
  assert.match(html, /The diff is not available: &lt;img src=x onerror=alert\(1\)&gt;/, "the hostile reason is HTML-escaped in the empty state");
  assert.doesNotMatch(html, /<img/, "no raw img tag appears in the rendered markup");
  assert.equal(change.querySelector("img"), null, "no img element is parsed from the reason");
  assert.equal(
    change.querySelector(".empty")?.textContent,
    "The diff is not available: &lt;img src=x onerror=alert(1)&gt;",
    "the empty state's text node carries the escaped reason (no element is parsed from it)",
  );
  change.remove();
  appNode.remove();
});

test("a genuinely empty but available diff still renders the normal empty file list", async () => {
  const root = globalThis.document.body;
  const { appNode, change } = mountChange(root, {
    change: changeResponse("h1"),
    task: { id: "t-0001" },
    threads: [],
    review_state: "in_review",
    diff: { head_sha: "h1", available: true, files: [], total_files: 0, additions: 0, deletions: 0 },
  });
  await flush();
  const html = change.innerHTML;
  assert.doesNotMatch(html, /data-change-pending/, "an available empty diff is not a pending state");
  assert.doesNotMatch(html, /not available/, "no unavailable note renders");
  assert.match(html, /class="files"/, "the file list container renders");
  assert.match(html, /<flow-diff>/, "the diff pane renders");
  change.remove();
  appNode.remove();
});

test("a revalidation whose diff the server explicitly reports unavailable keeps the prior pair until a verified one lands", async () => {
  const root = globalThis.document.body;
  // The change moves to h2, but /diff answers with the explicit unavailable
  // response for h2. That must not adopt h2 with an empty "verified" diff:
  // the h1 pair stays on screen and the next poll retries the revalidation.
  scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h1", diffFiles("h1")) },
    { change: changeResponse("h2"), diff: { head_sha: "h2", available: false, unavailable_reason: "diff not captured" } },
    { change: changeResponse("h2"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the loaded diff is on screen");

  // The moved head's diff is explicitly unavailable: the prior pair stays, no
  // head is adopted, and no empty "verified" diff renders under h2.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the prior verified pair stays");
  assert.doesNotMatch(changePanelHTML(detail), /not available yet/, "the cached pair is not replaced by the unavailable response");
  assert.equal(detail.changeKey, "ch-0001:h1", "no head is adopted from an unavailable diff");

  // The diff becomes available for h2; the next poll adopts the verified pair.
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h2\.go/, "the verified h2 pair is adopted once its diff lands");
  assert.equal(detail.changeKey, "ch-0001:h2", "the cache re-keys to the adopted head");
  detail.remove();
});

test("a later same-head poll retries a failed initial change load", async () => {
  const root = globalThis.document.body;
  // The first change fetch fails transiently; the server recovers before the
  // next poll.
  let fail = true;
  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(diffResponse("h1", diffFiles("h1"))) });
    }
    if (fail) return Promise.reject(new Error("network hiccup"));
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(changeResponse("h1")) });
  };
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  const panel = detail.querySelector(".panel");
  assert.match(panel.innerHTML, /network hiccup/, "the failed load shows its error");
  assert.match(panel.innerHTML, /data-change-retry/, "the error offers a retry");
  const failed = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(failed, ["/ui/api/v2/changes/ch-0001"], "the failed load made one change read and no diff read");

  // The transient failure clears; the next same-head poll retries the load on
  // its own instead of keeping the error card.
  fail = false;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the same-head poll retried the load and rendered the pair");
  assert.doesNotMatch(panel.innerHTML, /network hiccup/, "the error card is gone");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(failed.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the retry fetched one fresh change+diff pair",
  );

  // The recovered pair is an ordinary cached pair: the next same-head poll
  // revalidates in place instead of loading again.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a recovered same-head poll revalidates the change only",
  );
  detail.remove();
});

test("a later same-head poll retries a load whose diff fetch failed", async () => {
  const root = globalThis.document.body;
  // Every diff read of the first load fails transiently; the diff endpoint
  // recovers before the next poll.
  let failDiff = true;
  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    if (path.endsWith("/diff")) {
      if (failDiff) return Promise.reject(new Error("diff boom"));
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(diffResponse("h1", diffFiles("h1"))) });
    }
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(changeResponse("h1")) });
  };
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // The metadata names a head but its diff fetch failed: the load installs a
  // pending pair instead of a terminal "advanced" error, so the same-head poll
  // can retry the diff in place.
  const panel = detail.querySelector(".panel");
  assert.match(changePanelHTML(detail), /not available yet/, "a load whose diff fetch failed shows the pending diff state");
  assert.doesNotMatch(panel.innerHTML, /advanced while it was loading/, "an unavailable diff is not a head move");
  assert.doesNotMatch(panel.innerHTML, /data-change-retry/, "the pending state does not offer a terminal retry");
  const failed = calls.filter((path) => path.includes("/v2/changes/ch-0001"));
  assert.deepEqual(
    failed,
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the failed load made one change read and one diff read",
  );

  // The diff endpoint recovers; the next same-head poll's revalidation retries
  // the diff and installs the verified pair in place.
  failDiff = false;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the same-head poll retried the diff and rendered the pair");
  assert.doesNotMatch(changePanelHTML(detail), /not available yet/, "the pending state clears once the diff lands");
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(failed.length),
    ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"],
    "the retry revalidated in place with one fresh change+diff pair",
  );

  // The recovered pair is an ordinary cached pair: the next same-head poll
  // revalidates the change only.
  const caughtUp = calls.filter((path) => path.includes("/v2/changes/ch-0001")).length;
  detail.data = taskDetailModel("h1");
  await flush();
  await settleChange(detail);
  assert.deepEqual(
    calls.filter((path) => path.includes("/v2/changes/ch-0001")).slice(caughtUp),
    ["/ui/api/v2/changes/ch-0001"],
    "a recovered same-head poll revalidates the change only",
  );
  detail.remove();
});
