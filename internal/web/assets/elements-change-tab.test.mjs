// Change-tab element tests: the flow-task-detail change cache lifecycle —
// revalidation, head adoption, rollback/ABA, and unverified responses.
// Split from elements.test.mjs.

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


function mountChange(root, data) {
  const appNode = globalThis.document.createElement("flow-app");
  const statuses = [];
  appNode.setStatus = (message) => statuses.push(message);
  appNode.refresh = () => {};
  root.appendChild(appNode);
  const change = mountElement(appNode, "flow-change", data);
  return { appNode, change, statuses };
}

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
