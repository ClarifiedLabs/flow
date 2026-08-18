// Review-area element tests: the review panel (gates, plans, questions,
// the live terminal lifecycle), the review verdict pending state, and the
// inline thread forms. Split from elements.test.mjs.

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
  assert.deepEqual(posted.comments, [{ file_path: "a.go", line: 1, body: "note against h1", disposition: "introduced_by_change" }]);

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
  assert.deepEqual(posted[1].comments, [{ file_path: "a.go", line: 1, body: "note against h2", disposition: "introduced_by_change" }], "the h2 submission carries the h2 draft");

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

