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
const { nowCardModel, runRows, tabBadges, isOutdatedAnchor, reviewModel, taskModel } = await import("./task-model.js");
const { reconcile } = await import("./elements/base.js");
const { renderTaskCard } = await import("./elements/task-card.js");
const { renderTaskRail } = await import("./elements/task-rail.js");
const { renderAttentionStrip } = await import("./elements/attention-strip.js");
const { renderBoardTable } = await import("./elements/board-table.js");
const { renderStepRail } = await import("./elements/step-rail.js");
const { renderRunList } = await import("./elements/run-list.js");
const { renderRunSpine } = await import("./elements/run-spine.js");
const { renderWorkflowGraph, graphCounts } = await import("./elements/workflow-graph.js");
const { renderCheckList } = await import("./elements/check-list.js");
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
await import("./elements/tab-strip.js");
const { acquireBusy, inFlight, releaseBusy, settleStatus } = await import("./actions.js");
const { handleFormSubmit } = await import("./forms.js");
await import("./elements/change.js");
await import("./elements/inline-thread.js");
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

test("board filters partition the tasks they claim to", () => {
  const running = cardModel(entry());
  const waiting = cardModel(entry({ card: { wait: { kind: "human_gate" } } }));
  const queued = cardModel(entry({ task: { state: "scheduled" } }));
  assert.ok(matchesFilter(waiting, "attention"));
  assert.ok(!matchesFilter(waiting, "running"));
  assert.ok(matchesFilter(running, "running"));
  assert.ok(matchesFilter(queued, "queued"));
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
  assert.ok(!matchesFilter(model, "running"), "nobody is working on it");
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

test("the table hides an awaiting-worker row from Running but keeps it under all", () => {
  const parked = cardModel(entry({ task: { id: "t-parked" }, laneState: "awaiting_worker" }));
  const working = cardModel(entry({ task: { id: "t-working" } }));
  const all = renderBoardTable([parked, working], "all");
  assert.match(all, /t-parked/);
  assert.match(all, /Awaiting worker/);
  assert.match(all, /rail-label is-idle">in progress</, "the step rail must not pretend work is happening");
  const running = renderBoardTable([parked, working], "running");
  assert.ok(!running.includes("t-parked"));
  assert.match(running, /t-working/);
});

test("a lane card for a queued-for-worker task wears the await tone and names the wait", async () => {
  const root = globalThis.document.body;
  const lane = mountElement(root, "flow-lane", {
    key: "in_progress",
    label: "In Progress",
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
  assert.match(html, /sort: attention, then dwell/);
});

test("the table renders the now column as markdown", () => {
  const model = cardModel(
    entry({ card: { wait: { kind: "operator_intervention", reason: "execution_failed", message: "Failed on **line 42**" } } }),
  );
  const html = renderBoardTable([model], "all");
  assert.match(html, /<td class="col-now"><div class="md">/);
  assert.match(html, /<strong>line 42<\/strong>/);
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

test("the check list renders details as markdown but keeps job id and exit code escaped", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "reviewer", verdict: "blocked", details: "**Overall**: needs work", source_job_id: "j-<_1", exit_code: 1 },
    ],
  });
  assert.match(html, /<span class="detail"><strong>Overall<\/strong>: needs work · j-&lt;_1 · exit 1<\/span>/);
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
  assert.match(open, /href="\/ui\/tasks\/t-alpha-0009"/);
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

// --- element lifecycle -----------------------------------------------------

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
  assert.equal(review.gate.changeGate, false);
  assert.equal(review.artifact.manifest.tasks.length, 2);
  assert.equal(review.session, null, "no live session without an active waiting session");
});

test("the review model falls back to the gate node config for classic waits", () => {
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
  assert.equal(review.gate.interactive, false);
  assert.equal(review.gate.instructions, "Gate instructions");
  assert.deepEqual(review.gate.outcomes, ["approved", "rejected"]);
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
  assert.match(html, /Write task plan/);
  assert.match(html, /Review the proposed implementation tasks/);
  assert.match(html, /data-workflow-respond="wnr-1"/);
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
  const app = { tasksByProject: new Map([["p-1", [{ id: "t-1", title: "Alpha" }]]]) };
  assert.equal(relationTargetSuggestionsView(app, "p-1"), `<option value="t-1" label="Alpha"></option>`);
  assert.equal(relationTargetSuggestionsView(app, "p-2"), "");
  assert.equal(relationTargetSuggestionsView({}, "p-1"), "");
});

test("the relation picker suggests the selected project's cached tasks", () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    tasksByProject: new Map([
      ["p-1", [{ id: "t-1", title: "Alpha" }, { id: "t-2", title: "" }]],
      ["p-2", [{ id: "t-9", title: "Other project" }]],
    ]),
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const options = host.querySelector("#relation-target-tasks").children;
  assert.equal(options.length, 2);
  assert.equal(options[0].getAttribute("value"), "t-1");
  assert.equal(options[0].getAttribute("label"), "Alpha");
  // A task without a title still suggests its id, with no label attribute.
  assert.equal(options[1].getAttribute("value"), "t-2");
  assert.equal(options[1].getAttribute("label"), null);
  // Suggestions stay scoped to the selected project, not the whole cache.
  assert.ok(!options.some((option) => option.getAttribute("value") === "t-9"));
});

test("the relation picker falls back to manual entry with an empty task cache", () => {
  const app = { projects: [{ id: "p-1", name: "one" }] };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  assert.equal(host.querySelector("#relation-target-tasks").children.length, 0);
  // The free-text target input is still present, so an id can be typed by hand.
  assert.ok(host.querySelector("[data-relation-target]"));
});

test("changing the create form project reloads the relation target suggestions", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    tasksByProject: new Map([["p-1", [{ id: "t-1", title: "Alpha" }]]]),
  };
  const ensured = [];
  app.ensureTasks = async (projectID) => {
    ensured.push(projectID);
    if (projectID === "p-2") app.tasksByProject.set("p-2", [{ id: "t-9", title: "Beta" }]);
    return app.tasksByProject.get(projectID) || [];
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(ensured, ["p-2"]);
  const options = form.querySelector("#relation-target-tasks").children;
  assert.deepEqual(options.map((option) => option.getAttribute("value")), ["t-9"]);
  assert.equal(options[0].getAttribute("label"), "Beta");
});

test("a failed suggestion reload leaves the picker in manual-entry mode", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    tasksByProject: new Map([["p-1", [{ id: "t-1", title: "Alpha" }]]]),
  };
  app.ensureTasks = async (projectID) => app.tasksByProject.get(projectID) || [];
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  const projectSelect = form.querySelector('[name="project"]');
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.equal(form.querySelector("#relation-target-tasks").children.length, 0);
  assert.ok(form.querySelector("[data-relation-target]"), "the manual target input remains");
});

test("changing the create form project reloads the flow selector with that project's flows and default", async () => {
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    featuresByProject: new Map([[ "p-1", [] ]]),
  };
  const ensuredFlows = [];
  app.ensureFlows = async (projectID) => {
    ensuredFlows.push(projectID);
    if (projectID === "p-2") app.flowsByProject.set("p-2", { flows: [{ id: "fl-9", name: "Beta flow" }, { id: "fl-8", name: "Gamma flow" }], defaultFlowID: "fl-8" });
    return app.flowsByProject.get(projectID);
  };
  app.ensureFeatures = async (projectID) => {
    if (projectID === "p-2") app.featuresByProject.set("p-2", [{ feature: { id: "ft-9", title: "Beta feature", status: "open" } }]);
    return app.featuresByProject.get(projectID) || [];
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
  const featureOptions = form.querySelector('[name="feature_id"]').children;
  assert.deepEqual(featureOptions.map((option) => option.getAttribute("value")), ["", "ft-9"], "the feature picker follows the project too");
  const relationOptions = form.querySelector("#relation-target-tasks").children;
  assert.equal(relationOptions.length, 0, "p-2 has no cached tasks, so suggestions stay empty");
});

test("rapid project switches cannot repaint the flow select or relation suggestions with stale-project data", async () => {
  let resolveP2Flows;
  let resolveP2Tasks;
  const p2FlowsGate = new Promise((resolve) => { resolveP2Flows = resolve; });
  const p2TasksGate = new Promise((resolve) => { resolveP2Tasks = resolve; });
  const app = {
    projects: [{ id: "p-1", name: "one" }, { id: "p-2", name: "two" }, { id: "p-3", name: "three" }],
    flowsByProject: new Map([[ "p-1", { flows: [{ id: "fl-1", name: "One flow" }], defaultFlowID: "fl-1" } ]]),
    tasksByProject: new Map([[ "p-1", [{ id: "t-1", title: "One task" }] ]]),
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
  app.ensureTasks = (projectID) => {
    if (projectID === "p-2") {
      return p2TasksGate.then(() => {
        const tasks = [{ id: "t-2", title: "Two task" }];
        app.tasksByProject.set("p-2", tasks);
        return tasks;
      });
    }
    if (projectID === "p-3") {
      const tasks = [{ id: "t-3", title: "Three task" }];
      app.tasksByProject.set("p-3", tasks);
      return Promise.resolve(tasks);
    }
    return Promise.resolve(app.tasksByProject.get(projectID));
  };
  const host = document.createElement("div");
  host.innerHTML = renderTaskFormView(app, { priority: 0 }, { mode: "create", projectID: "p-1", submitLabel: "Create" });
  const form = host.querySelector("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
  const projectSelect = form.querySelector('[name="project"]');
  const flowSelect = form.querySelector('[name="flow_id"]');
  const datalist = form.querySelector("#relation-target-tasks");
  projectSelect.value = "p-2";
  projectSelect.dispatchEvent(new TestEvent("change"));
  projectSelect.value = "p-3";
  projectSelect.dispatchEvent(new TestEvent("change"));
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the newest project's flows are shown");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the newest project's tasks are suggested");
  // The p-2 loads land late: neither control may repaint with p-2's data.
  resolveP2Flows();
  resolveP2Tasks();
  await flush();
  await flush();
  assert.deepEqual(flowSelect.children.map((option) => option.getAttribute("value")), ["fl-3"], "the stale flow load does not repaint");
  assert.deepEqual(datalist.children.map((option) => option.getAttribute("value")), ["t-3"], "the stale task load does not repaint");
});
// --- review verdict pending state (flow-change / submitReview) --------------

function reviewChangeData() {
  return {
    change: { id: "ch-0001", head_sha: "abc123def456" },
    task: { id: "t-0001" },
    diff: {
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
  stubReviewFetch(
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

function inlineThreadData() {
  const now = new Date().toISOString();
  return {
    thread: {
      id: "th-0001",
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
      json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: head }, task: { id: "t-0001" }, threads: [], review_state: "in_review" }),
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
  // does not move.
  resolveDiff();
  await settleChange(detail);
  assert.match(changePanelHTML(detail), /h1\.go/, "the current head stays on screen after the stale diff lands");
  assert.doesNotMatch(changePanelHTML(detail), /h2\.go/, "the rolled-back head is never rendered — one head only");
  assert.equal(detail.changeKey, "ch-0001:h1", "the stale revalidation does not adopt the rolled-back head");
  assert.equal(detail.changeAheadKey, "", "no ahead window opens for the rolled-back head");

  // The cache stayed stale, so the next same-head poll revalidates the current
  // head in place and confirms it.
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

test("a load that never verifies shows a retryable error instead of an unverified pair", async () => {
  const root = globalThis.document.body;
  const calls = scriptedChangeFetch([
    { change: changeResponse("h1"), diff: diffResponse("h2", diffFiles("h2")) },
  ]);
  const detail = await mountTaskDetail(root, "h1");
  await settleChange(detail);

  // Every attempt's diff names another head, so no pair verifies: the tab shows
  // a retryable error rather than metadata with a diff verified for another
  // head.
  const panel = detail.querySelector(".panel");
  assert.match(panel.innerHTML, /advanced while it was loading/, "an unverified load fails with a retryable error");
  assert.match(panel.innerHTML, /data-change-retry/, "the error offers a retry");
  assert.doesNotMatch(panel.innerHTML, /h2\.go/, "no unverified diff is rendered");
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
