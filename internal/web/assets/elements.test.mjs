// Tests for the custom-element UI: the pure render functions as strings, and
// the element lifecycle behaviours that string tests cannot reach — keyed
// reconcile preserving instance state across a poll, delegated listeners
// surviving a repaint, and disclosure staying local to its row.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement, TestEvent } from "./test-dom.mjs";

installTestDOM();

const { cardModel, dwellTone, formatDwell, matchesFilter, sortForAttention, waitActionLabel, waitReasonText, waitingOnBlockers, waitingOnOmitted } =
  await import("./board-model.js");
const { nowCardModel, runRows, tabBadges, isOutdatedAnchor, reviewModel } = await import("./task-model.js");
const { reconcile } = await import("./elements/base.js");
const { renderTaskCard } = await import("./elements/task-card.js");
const { renderAttentionStrip } = await import("./elements/attention-strip.js");
const { renderBoardTable } = await import("./elements/board-table.js");
const { renderStepRail } = await import("./elements/step-rail.js");
const { renderRunList } = await import("./elements/run-list.js");
const { renderWorkflowGraph, graphCounts } = await import("./elements/workflow-graph.js");
const { renderCheckList } = await import("./elements/check-list.js");
const { renderHeldPanel, HAND_BACK_EDGES } = await import("./elements/held-panel.js");
const { renderDiffFile } = await import("./elements/diff.js");
const { renderReviewBar } = await import("./elements/review-bar.js");
const { renderEpic, memberState } = await import("./elements/epic.js");
const { renderNowCard } = await import("./elements/now-card.js");
const { renderReviewPanel } = await import("./elements/review-panel.js");
const { renderActivityFeed, activityEntries } = await import("./elements/activity-feed.js");
const { renderTaskFormView, bindRelationsPickerView } = await import("./task-view.js");
await import("./elements/lane.js");
await import("./elements/tab-strip.js");
const { inFlight } = await import("./actions.js");
await import("./elements/change.js");

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
  assert.match(html, /<a href="\/ui\/tasks\/t-c"[^>]*>C<\/a><\/p>/);
  assert.ok(!html.includes("more"), "at the limit means no +N more suffix");
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

test("a convergence hold explains the scope decision", () => {
  const html = renderHeldPanel({
    held: true,
    heldBy: "system",
    id: "t-0043",
    stepName: "review",
    statusLog: [{
      kind: "plan",
      message: "Convergence review required before automated review: this change touches 8 files.",
    }],
  });
  assert.match(html, /Convergence review/);
  assert.match(html, /touches 8 files/);
  assert.match(html, /data-edge="resume"/);
});

test("the held panel renders convergence message as markdown", () => {
  const html = renderHeldPanel({
    held: true,
    heldBy: "system",
    id: "t-0043",
    stepName: "review",
    statusLog: [{
      kind: "plan",
      message: "Convergence **review** required.",
    }],
  });
  assert.match(html, /<div class="prose"><div class="md">/);
  assert.match(html, /<strong>review<\/strong>/);
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
          { kind: "del", old_line: 211, text: "  return ErrBudgetExhausted" },
          { kind: "add", new_line: 211, text: "  return e.pauseForOperator(ctx, run)" },
        ],
      },
    ],
  });
  assert.equal((html.match(/data-comment-line=/g) || []).length, 3);
  assert.match(html, /aria-label="Comment on line 210"/);
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
  assert.equal(firstRow.querySelector("[data-relation-kind]").children[0].getAttribute("value"), "parent_of");
  assert.ok(firstRow.querySelector("[data-relation-target]"));

  firstRow.querySelector("[data-relation-remove]").click();
  assert.equal(rows.children.length, 2);
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
  const pending = change.handleClick({ target: approve, preventDefault() {} });

  // Synchronous pending state, before the network resolves.
  assert.equal(approve.disabled, true);
  assert.equal(approve.getAttribute("aria-busy"), "true");
  assert.equal(approve.classList.contains("is-busy"), true);
  assert.deepEqual(statuses, ["Approving\u2026"]);

  resolveRequest();
  await pending;
  assert.equal(approve.disabled, false);
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(approve.classList.contains("is-busy"), false);
  assert.deepEqual(statuses, ["Approving\u2026", "Approved"]);
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
  await change.handleClick({ target: approve, preventDefault() {} });

  assert.deepEqual(statuses, ["Approving\u2026", "change already merged"]);
  assert.equal(approve.disabled, false);
  assert.equal(approve.getAttribute("aria-busy"), null);
  assert.equal(inFlight.size, 0);
  change.remove();
  appNode.remove();
});
