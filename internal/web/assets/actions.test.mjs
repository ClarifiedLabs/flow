// Delegated-action tests: the busy registry, the dispatcher's settlement
// and provenance, and the domain handlers exercised through ActionButton and
// its gate/claim subclasses.

import assert from "node:assert/strict";
import { test } from "node:test";
import { applyBusyState, actionScope, failureMessage, gateResponsePending, handleAction, inFlight, pendingStatus, settleStatus, threadClaimPending } from "./actions.js";
import { handleFormSubmit } from "./forms.js";
import { ActionButton, InlineDOMElement, deferred, flushAsync, inlineDocument, scriptContext, statusApp } from "./test-helpers.mjs";

test("successful containment relation mutations evict hierarchy summaries before refresh", async () => {
  await scriptContext();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, status: 204, text: () => Promise.resolve("") });
  };
  const cache = new Map([["p-alpha", [{ id: "stale-container" }]]]);
  let refreshes = 0;
  const app = {
    workItemsByProject: cache,
    caches: { invalidate: (kind, id) => cache.delete(id) },
    setStatus() {},
    async refresh() {
      assert.equal(cache.has("p-alpha"), false, "refresh must not reconcile from stale hierarchy summaries");
      refreshes += 1;
      cache.set("p-alpha", [{ id: `fresh-${refreshes}` }]);
    },
  };
  const addForm = {
    tagName: "FORM",
    dataset: { workItemRelationAddForm: "e-alpha-0001", project: "p-alpha" },
    elements: {
      target_item_id: { value: "t-alpha-0001" },
      kind: { value: "parent_of" },
    },
    querySelector() { return null; },
    reportValidity() { return true; },
  };

  assert.equal(await handleFormSubmit(app, { target: addForm, preventDefault() {} }), true);
  assert.deepEqual(cache.get("p-alpha"), [{ id: "fresh-1" }]);

  const remove = new ActionButton({
    workItemRelationRemove: "e-alpha-0001",
    project: "p-alpha",
    source: "e-alpha-0001",
    target: "t-alpha-0001",
    kind: "parent_of",
  });
  assert.equal(await handleAction(app, { target: remove, preventDefault() {} }), true);
  assert.deepEqual(cache.get("p-alpha"), [{ id: "fresh-2" }]);
  assert.equal(refreshes, 2);
  assert.deepEqual(calls.map(({ path, options }) => [path, options.method]), [
    ["/ui/api/v2/projects/p-alpha/work-items/e-alpha-0001/relations", "POST"],
    ["/ui/api/v2/projects/p-alpha/work-items/e-alpha-0001/relations", "DELETE"],
  ]);
});

class GateButton extends ActionButton {
  constructor(dataset, panel) {
    super(dataset);
    this.panel = panel;
  }
  closest(selector) {
    if (selector === "[data-gate-panel]" || selector === "[data-gate-node-run]") return this.panel;
    return null;
  }
}

// A gate panel stub exposing the outcome buttons to suppressGateSiblings and a
// null feedback textarea to the workflowRespond handler.
function gatePanel() {
  return {
    buttons: [],
    querySelector() {
      return null;
    },
    querySelectorAll(selector) {
      return selector === "[data-workflow-respond]" ? this.buttons : [];
    },
  };
}

// A thread claim button: an ActionButton that also knows its claim row (the
// .claims container holding the thread's three claim buttons), so handleAction
// can suppress the whole row when one of them is clicked.
class ClaimButton extends ActionButton {
  constructor(dataset, row) {
    super(dataset);
    this.parentElement = row;
  }
}

// A claim row stub exposing a thread's claim buttons to suppressThreadClaims.
function claimRow() {
  return {
    buttons: [],
    querySelectorAll(selector) {
      return selector === "[data-thread-claim]" ? this.buttons : [];
    },
  };
}

test("manual scope review requests a typed convergence hold", async () => {
  await scriptContext();
  const app = statusApp();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({
    convergenceRequest: "t-0043",
    project: "p-alpha",
  });

  assert.equal(await handleAction(app, { target: button, preventDefault() {} }), true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0043/workflow/convergence/request");
  assert.deepEqual(JSON.parse(calls[0].options.body), {});
  assert.deepEqual(app.statuses, [
    "Starting scope review t-0043\u2026",
    "Convergence review started for t-0043",
  ]);
});

test("a convergence decision posts an explicit disposition instead of a workflow edge", async () => {
  await scriptContext();
  const app = statusApp();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({
    convergenceDecision: "t-0043",
    disposition: "accept_scope",
    evidenceFingerprint: "sha256:reviewed-evidence",
    project: "p-alpha",
  });
  button.closest = (selector) => selector === "[data-convergence-panel]"
    ? { querySelector: () => ({ value: "  reviewed scope  " }) }
    : button;

  assert.equal(await handleAction(app, { target: button, preventDefault() {} }), true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0043/workflow/convergence");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    disposition: "accept_scope",
    expected_evidence_fingerprint: "sha256:reviewed-evidence",
    note: "reviewed scope",
  });
  assert.deepEqual(app.statuses, [
    "Resolving convergence review t-0043\u2026",
    "Continuing t-0043 as-is",
  ]);
});

test("an action click marks the control busy and names the in-flight action before the request resolves", async () => {
  await scriptContext();
  const app = statusApp();
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending state is synchronous: before the network resolves the control
  // is disabled and aria-busy, and the status line names the action.
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026"]);

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Scheduled"]);
});

test("a poll re-render replacing the button re-applies the busy state instead of re-enabling it", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: first, preventDefault() {} });
  assert.equal(requests, 1);

  // The busy metadata lives outside the node: the pending label survives the
  // route render that clears the status line.
  assert.equal(pendingStatus(), "Scheduling t-0001\u2026");

  // The board's 10 s poll repaints and swaps the button node for a fresh one.
  // The repaint re-applies the in-flight state from the registry, so the
  // replacement is disabled and visibly busy — not actionable.
  const replacement = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  applyBusyState({ querySelectorAll: () => [replacement] });
  assert.equal(replacement.disabled, true);
  assert.equal(replacement.getAttribute("aria-busy"), "true");
  assert.equal(replacement.classList.contains("is-busy"), true);

  // Clicking the replacement while the first request is still in flight must
  // not issue a second request: the guard lives in the in-flight registry, not
  // on the (now discarded) node.
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;

  // Settling restores whatever control is on screen now — the repaint-marked
  // replacement — not the discarded node the click started on.
  assert.equal(replacement.disabled, false);
  assert.equal(replacement.getAttribute("aria-busy"), null);
  assert.equal(replacement.classList.contains("is-busy"), false);
  assert.equal(pendingStatus(), "");

  // Once the first request settles the action is available again.
  const second = handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("answering a gate suppresses every sibling outcome until the response settles", async () => {
  await scriptContext();
  const app = statusApp();
  let resolveRequest;
  globalThis.fetch = () =>
    new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const revise = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "changes_requested", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, revise, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });

  // Synchronously, before the request resolves, every outcome for the node run
  // is suppressed — not just the one that was clicked.
  for (const control of [approve, revise, reject]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-busy"), "true");
    assert.equal(control.classList.contains("is-busy"), true);
  }

  // The shared in-flight registry reports the gate response as pending, which
  // is exactly what the render path consults to re-suppress fresh buttons after
  // a poll repaint — so no sibling flashes enabled while the request is out.
  assert.equal(gateResponsePending("wnr-1"), true);

  resolveRequest();
  assert.equal(await handled, true);

  // Settling restores the live outcome controls and clears the pending flag.
  for (const control of [approve, revise, reject]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.equal(gateResponsePending("wnr-1"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "Feedback sent"]);
  assert.equal(inFlight.size, 0);
});

test("answering a gate posts the rendered review round wait id", async () => {
  await scriptContext();
  const app = statusApp();
  let requestBody = null;
  globalThis.fetch = (path, options) => {
    requestBody = JSON.parse(options.body);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  panel.buttons.push(approve);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(await handled, true);

  // The response carries the wait id of the round that was rendered, so a
  // stale panel cannot decide a later round reopened on the same node run.
  assert.deepEqual(requestBody, {
    node_run_id: "wnr-1",
    review_wait_id: "ww-1",
    outcome: "approved",
    feedback: "",
  });
});

test("an incomplete gate has no actionable response without a review wait id", async () => {
  await scriptContext();
  const app = statusApp();
  let fetches = 0;
  globalThis.fetch = () => {
    fetches += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  panel.buttons.push(approve);

  assert.equal(await handleAction(app, { target: approve, preventDefault() {} }), true);
  assert.equal(fetches, 0);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1…", "This review wait is no longer actionable"]);
  assert.equal(approve.disabled, false);
});

test("approving a card posts the review round wait id the card observed", async () => {
  await scriptContext();
  let refreshed = false;
  const app = {
    setStatus() {},
    async refresh() {
      refreshed = true;
    },
  };
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    if (options?.method === "GET") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          detail: {
            run: { current_node_run_id: "wnr-1", current_node_key: "plan" },
            open_wait: {
              id: "ww-1",
              node_run_id: "wnr-1",
              kind: "human_gate",
              details: { interactive: true, gate_node_key: "review", artifact_id: "a-1", outcomes: ["approved", "changes_requested", "rejected"] },
            },
          },
        }),
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ cardApprove: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });
  assert.equal(await handled, true);

  assert.equal(calls.length, 2);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/workflow");
  assert.equal(calls[1].path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/workflow/respond");
  // The approval carries the wait id of the round the card's detail fetch
  // observed, so a stale card click cannot decide a later round reopened on
  // the same node run before the POST lands.
  assert.deepEqual(JSON.parse(calls[1].options.body), {
    node_run_id: "wnr-1",
    review_wait_id: "ww-1",
    outcome: "approved",
  });
  assert.equal(refreshed, true);
});

test("a failed gate response restores every suppressed sibling outcome", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(reject.disabled, true, "the sibling is suppressed while the response is pending");

  rejectRequest();
  assert.equal(await handled, true);

  // Failure restores every outcome control, leaving the gate fully actionable.
  assert.equal(approve.disabled, false);
  assert.equal(reject.disabled, false);
  assert.equal(reject.getAttribute("aria-busy"), null);
  assert.equal(reject.classList.contains("is-busy"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
});

test("a repaint mid-flight leaves no live outcome disabled after a failed settlement", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const reject = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  panel.buttons.push(approve, reject);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);
  assert.equal(reject.disabled, true, "the sibling is suppressed while the response is pending");

  // A poll repaints while the response is still pending: the render path
  // re-emits every outcome disabled (the shared key is still in flight) and
  // swaps them in for the now-detached originals the click captured.
  const liveApprove = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const liveReject = new GateButton({ workflowRespond: "wnr-1", reviewWait: "ww-1", task: "t-0001", outcome: "rejected", project: "p-alpha" }, panel);
  for (const control of [liveApprove, liveReject]) {
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-busy");
  }
  panel.buttons = [liveApprove, liveReject];
  globalThis.document = {
    querySelectorAll: (selector) => (selector === "[data-workflow-respond]" ? panel.buttons : []),
  };

  rejectRequest();
  assert.equal(await handled, true);

  // Failure clears the shared key but issues no refresh, so nothing else
  // repaints the panel. The live replacement outcomes must be restored
  // directly — not left disabled/aria-busy/is-busy until a later poll.
  for (const control of [liveApprove, liveReject]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.equal(gateResponsePending("wnr-1"), false);
  assert.deepEqual(app.statuses, ["Sending feedback wnr-1\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
});

test("a thread claim stays single-flight across a repaint and re-enables on success", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const row = claimRow();
  const fixed = new ClaimButton({ threadClaim: "th-0001", claimKind: "fixed" }, row);
  const notWarranted = new ClaimButton({ threadClaim: "th-0001", claimKind: "not_warranted" }, row);
  row.buttons.push(fixed, notWarranted);

  const handled = handleAction(app, { target: fixed, preventDefault() {} });
  assert.equal(requests, 1);

  // Synchronously, before the request resolves, every claim button for the
  // thread is suppressed — not just the one that was clicked.
  assert.equal(fixed.disabled, true);
  assert.equal(notWarranted.disabled, true);
  assert.equal(notWarranted.getAttribute("aria-busy"), "true");
  assert.equal(notWarranted.classList.contains("is-busy"), true);

  // The shared in-flight registry reports the claim as pending — exactly what
  // the render path consults to re-suppress fresh buttons after a repaint.
  assert.equal(threadClaimPending("th-0001"), true);

  // A different thread's claims keep their own key and stay actionable.
  assert.equal(threadClaimPending("th-0002"), false);
  const other = new ActionButton({ threadClaim: "th-0002", claimKind: "fixed" });
  const otherHandled = handleAction(app, { target: other, preventDefault() {} });
  assert.equal(requests, 2, "a different thread's claim is not blocked by the in-flight one");

  // A repaint swaps the row for fresh enabled nodes carrying the same thread;
  // clicking a replacement while the first request is still in flight must
  // not issue a third request.
  const replacement = new ClaimButton({ threadClaim: "th-0001", claimKind: "fixed" }, row);
  row.buttons = [replacement];
  applyBusyState({ querySelectorAll: () => [replacement] });
  assert.equal(replacement.disabled, true, "the repaint re-applies the busy state to the replacement");
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 2, "no duplicate claim while the first is in flight");

  resolvers[0]();
  resolvers[1]();
  await handled;
  await otherHandled;

  // Settling restores whatever controls are on screen now, and the action is
  // available again once the registry drains.
  assert.equal(replacement.disabled, false);
  assert.equal(replacement.getAttribute("aria-busy"), null);
  assert.equal(replacement.classList.contains("is-busy"), false);
  assert.equal(threadClaimPending("th-0001"), false);
  // th-0001's settlement lands while th-0002's claim is still pending, so
  // settleStatus keeps the still-pending label on the line instead of showing
  // the confirmation early; the final settlement shows "Thread claimed".
  assert.deepEqual(app.statuses, [
    "Claiming thread th-0001\u2026",
    "Claiming thread th-0002\u2026",
    "Claiming thread th-0002\u2026",
    "Thread claimed",
  ]);
  assert.equal(inFlight.size, 0);
});

test("a repaint mid-flight leaves no live claim disabled after a failed settlement", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const row = claimRow();
  const fixed = new ClaimButton({ threadClaim: "th-0001", claimKind: "fixed" }, row);
  const notWarranted = new ClaimButton({ threadClaim: "th-0001", claimKind: "not_warranted" }, row);
  row.buttons.push(fixed, notWarranted);

  const handled = handleAction(app, { target: fixed, preventDefault() {} });
  assert.equal(fixed.disabled, true);
  assert.equal(notWarranted.disabled, true, "the sibling claim is suppressed while the claim is pending");

  // A poll repaints while the claim is still pending: the render path re-emits
  // every claim button disabled (the shared key is still in flight) and swaps
  // them in for the now-detached originals the click captured.
  const liveFixed = new ClaimButton({ threadClaim: "th-0001", claimKind: "fixed" }, row);
  const liveNotWarranted = new ClaimButton({ threadClaim: "th-0001", claimKind: "not_warranted" }, row);
  for (const control of [liveFixed, liveNotWarranted]) {
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-busy");
  }
  row.buttons = [liveFixed, liveNotWarranted];
  globalThis.document = {
    cookie: "flow_ui_csrf=csrf-token",
    querySelectorAll: (selector) => (selector === "[data-thread-claim]" ? row.buttons : []),
  };

  rejectRequest();
  assert.equal(await handled, true);

  // Failure clears the shared key but issues no refresh, so nothing else
  // repaints the row. The live replacement claims must be restored directly —
  // not left disabled/aria-busy/is-busy until a later poll.
  for (const control of [liveFixed, liveNotWarranted]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.equal(threadClaimPending("th-0001"), false);
  assert.deepEqual(app.statuses, ["Claiming thread th-0001\u2026", "boom"]);
  assert.equal(inFlight.size, 0);
});

test("a pending claim suppresses same-thread controls outside the clicked row and restores them on failure", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const row = claimRow();
  const fixed = new ClaimButton({ threadClaim: "th-0001", claimKind: "fixed" }, row);
  const notWarranted = new ClaimButton({ threadClaim: "th-0001", claimKind: "not_warranted" }, row);
  row.buttons.push(fixed, notWarranted);
  // The Now card carries its own claim controls for the same open thread in a
  // different surface; they must read as busy too, not just the clicked row.
  const cardFixed = new ActionButton({ threadClaim: "th-0001", claimKind: "fixed" });
  const cardNotWarranted = new ActionButton({ threadClaim: "th-0001", claimKind: "not_warranted" });
  globalThis.document = {
    cookie: "flow_ui_csrf=csrf-token",
    querySelectorAll: (selector) =>
      selector === "[data-thread-claim]" ? [...row.buttons, cardFixed, cardNotWarranted] : [],
  };

  const handled = handleAction(app, { target: fixed, preventDefault() {} });
  for (const control of [fixed, notWarranted, cardFixed, cardNotWarranted]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-busy"), "true");
    assert.equal(control.classList.contains("is-busy"), true);
  }
  assert.equal(threadClaimPending("th-0001"), true);

  rejectRequest();
  assert.equal(await handled, true);
  for (const control of [fixed, notWarranted, cardFixed, cardNotWarranted]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-busy"), null);
    assert.equal(control.classList.contains("is-busy"), false);
  }
  assert.deepEqual(app.statuses, ["Claiming thread th-0001\u2026", "boom"]);
  assert.equal(threadClaimPending("th-0001"), false);
  assert.equal(inFlight.size, 0);
});

test("a gate response settles safely when the node-run id contains selector metacharacters", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  const nodeRunID = 'wnr-1"][data-x]:nth-child(1)';
  const panel = gatePanel();
  const approve = new GateButton({ workflowRespond: nodeRunID, reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  panel.buttons.push(approve);

  const handled = handleAction(app, { target: approve, preventDefault() {} });
  assert.equal(approve.disabled, true);

  // A poll repaint swaps in a fresh disabled outcome for the same node run
  // and one for a different node run; only the exact match may be restored.
  const liveApprove = new GateButton({ workflowRespond: nodeRunID, reviewWait: "ww-1", task: "t-0001", outcome: "approved", project: "p-alpha" }, panel);
  const otherGate = new GateButton({ workflowRespond: "wnr-2", reviewWait: "ww-2", task: "t-0002", outcome: "approved", project: "p-alpha" }, panel);
  for (const control of [liveApprove, otherGate]) {
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-busy");
  }
  panel.buttons = [liveApprove, otherGate];
  // Like a real document, any selector built from the node-run id is invalid
  // and throws; only the broad control selector is legal.
  globalThis.document = {
    querySelectorAll(selector) {
      if (selector !== "[data-workflow-respond]") throw new Error(`invalid selector: ${selector}`);
      return panel.buttons;
    },
  };

  rejectRequest();
  assert.equal(await handled, true);

  assert.equal(liveApprove.disabled, false, "the exact match is restored despite the hostile id");
  assert.equal(liveApprove.getAttribute("aria-busy"), null);
  assert.equal(liveApprove.classList.contains("is-busy"), false);
  assert.equal(otherGate.disabled, true, "a different node run's outcome stays suppressed");
  assert.equal(otherGate.getAttribute("aria-busy"), "true");
  assert.equal(otherGate.classList.contains("is-busy"), true);
  assert.equal(gateResponsePending(nodeRunID), false);
  assert.equal(inFlight.size, 0);
});

test("a thread claim stays single-flight when the thread id contains selector metacharacters", async () => {
  await scriptContext();
  const app = statusApp();
  let rejectRequest;
  globalThis.fetch = () =>
    new Promise((resolve, reject) => {
      rejectRequest = () => reject(new Error("boom"));
    });
  // Interpolated into a selector this id closes the attribute and opens a
  // second one, so a naive querySelectorAll throws before the claim POST.
  const threadID = 'th-1"][data-unrelated]';
  const row = claimRow();
  const fixed = new ClaimButton({ threadClaim: threadID, claimKind: "fixed" }, row);
  const notWarranted = new ClaimButton({ threadClaim: threadID, claimKind: "not_warranted" }, row);
  row.buttons.push(fixed, notWarranted);

  const handled = handleAction(app, { target: fixed, preventDefault() {} });
  assert.equal(fixed.disabled, true);
  assert.equal(notWarranted.disabled, true, "the sibling claim is suppressed despite the hostile id");
  assert.equal(threadClaimPending(threadID), true);

  // A poll repaint swaps in fresh disabled claims for the same thread and one
  // for a different thread; only the exact match may be restored. Like a real
  // document, any selector built from the thread id is invalid and throws;
  // only the broad control selector is legal.
  const liveFixed = new ClaimButton({ threadClaim: threadID, claimKind: "fixed" }, row);
  const liveNotWarranted = new ClaimButton({ threadClaim: threadID, claimKind: "not_warranted" }, row);
  const other = new ActionButton({ threadClaim: "th-0002", claimKind: "fixed" });
  for (const control of [liveFixed, liveNotWarranted, other]) {
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-busy");
  }
  row.buttons = [liveFixed, liveNotWarranted];
  globalThis.document = {
    cookie: "flow_ui_csrf=csrf-token",
    querySelectorAll(selector) {
      if (selector !== "[data-thread-claim]") throw new Error(`invalid selector: ${selector}`);
      return [...row.buttons, other];
    },
  };

  rejectRequest();
  assert.equal(await handled, true);

  assert.equal(liveFixed.disabled, false, "the exact match is restored despite the hostile id");
  assert.equal(liveFixed.getAttribute("aria-busy"), null);
  assert.equal(liveFixed.classList.contains("is-busy"), false);
  assert.equal(liveNotWarranted.disabled, false);
  assert.equal(other.disabled, true, "a different thread's claim stays suppressed");
  assert.equal(other.getAttribute("aria-busy"), "true");
  assert.equal(other.classList.contains("is-busy"), true);
  assert.equal(threadClaimPending(threadID), false);
  assert.deepEqual(app.statuses, [`Claiming thread ${threadID}\u2026`, "boom"]);
  assert.equal(inFlight.size, 0);
});

test("a thread id that forms a valid injected selector cannot suppress another thread's claims", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  // Interpolated into a selector, this id closes the attribute and opens a
  // second attribute matching any control whose data-thread-claim is "th-1".
  const threadID = 'th-1"][data-thread-claim="th-0002';
  const row = claimRow();
  const fixed = new ClaimButton({ threadClaim: threadID, claimKind: "fixed" }, row);
  const other = new ActionButton({ threadClaim: "th-1", claimKind: "fixed" });
  row.buttons.push(fixed, other);
  globalThis.document = {
    cookie: "flow_ui_csrf=csrf-token",
    querySelectorAll: (selector) =>
      selector === "[data-thread-claim]" ? [...row.buttons, other] : [],
  };

  const handled = handleAction(app, { target: fixed, preventDefault() {} });
  assert.equal(requests, 1, "the claim POST starts");
  assert.equal(fixed.disabled, true);
  // The old interpolated selector would have matched "th-1" too; the dataset
  // filter keeps the other thread's claims independently actionable.
  assert.equal(other.disabled, false, "a different thread's claim is not suppressed");
  assert.equal(other.getAttribute("aria-busy"), null);
  assert.equal(other.classList.contains("is-busy"), false);
  assert.equal(threadClaimPending(threadID), true);
  assert.equal(threadClaimPending("th-1"), false);

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(other.disabled, false, "a different thread's claim is not re-enabled by the settlement");
  assert.equal(other.getAttribute("aria-busy"), null);
  assert.equal(other.classList.contains("is-busy"), false);
  assert.equal(threadClaimPending(threadID), false);
  assert.equal(inFlight.size, 0);
});

test("approving one human check does not block a different check on the same task", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const checkA = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });
  const checkB = new ActionButton({ humanReviewApprove: "t-0001", checkName: "docs", project: "p-alpha" });

  const approveA = handleAction(app, { target: checkA, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(checkA.disabled, true);

  // A different check on the same task has its own busy identity, so approving
  // it issues its own request instead of being suppressed by the in-flight one.
  const approveB = handleAction(app, { target: checkB, preventDefault() {} });
  assert.equal(requests, 2, "a distinct check is not blocked by the in-flight one");
  assert.equal(checkB.disabled, true);
  assert.equal(checkA.disabled, true, "the first check stays busy while its request is in flight");

  resolvers[0]();
  resolvers[1]();
  await approveA;
  await approveB;
  assert.equal(checkA.disabled, false);
  assert.equal(checkB.disabled, false);
  assert.equal(inFlight.size, 0);
});

test("a duplicate approval of the same human check stays suppressed while its request is in flight", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });

  const handled = handleAction(app, { target: first, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.disabled, true);
  assert.equal(first.getAttribute("aria-busy"), "true");
  assert.equal(first.classList.contains("is-busy"), true);

  // A poll repaint swaps the button for a fresh enabled node carrying the same
  // task and check; clicking it must not issue a second request.
  const replacement = new ActionButton({ humanReviewApprove: "t-0001", checkName: "tests", project: "p-alpha" });
  assert.equal(replacement.disabled, false);
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "the same check's duplicate approval is suppressed");

  resolveRequest();
  await handled;

  // Once the request settles the check is available again.
  const second = handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("a failed action keeps the error on the status line and restores the control", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () =>
    Promise.resolve({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: "workflow is locked" } }),
    });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "workflow is locked"]);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on failure");
});

test("a console start marks the control busy, blocks a duplicate across a repaint, and confirms the start", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  app.querySelector = (selector) => (selector === "[data-console-harness]" ? { value: "shell" } : null);
  const requests = [];
  let resolveRequest;
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  // The Console view's project-level Start button: data-start-console is
  // empty (no task), the console target lives in data-project/data-task.
  const button = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending state is synchronous: the control is disabled and aria-busy
  // and the status line names the action before the POST resolves. The
  // harness is the one picked in the view's select.
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Starting console\u2026"]);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, "/ui/api/v2/projects/p-alpha/console");
  assert.deepEqual(JSON.parse(requests[0].options.body), { harness: "shell" });

  // A console repaint mid-flight swaps the button node: the registry
  // re-applies the busy state to the replacement and the repeat click is
  // swallowed without a second request.
  const replacement = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });
  applyBusyState({ querySelectorAll: () => [replacement] });
  assert.equal(replacement.disabled, true);
  assert.equal(replacement.getAttribute("aria-busy"), "true");
  assert.equal(replacement.classList.contains("is-busy"), true);
  assert.equal(await handleAction(app, { target: replacement, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "no duplicate console start while the first is in flight");

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(loads, 1, "the console view reloads after the start");
  assert.deepEqual(app.statuses, ["Starting console\u2026", "Console starting"]);
  assert.equal(replacement.disabled, false);
  assert.equal(replacement.getAttribute("aria-busy"), null);
  assert.equal(replacement.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on success");
});

test("a console release marks the control busy, suppresses a duplicate, and confirms the release", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  const requests = [];
  let resolveRequest;
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const button = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute("aria-busy"), "true");
  assert.equal(button.classList.contains("is-busy"), true);
  assert.deepEqual(app.statuses, ["Releasing console t-0001\u2026"]);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/console");
  assert.equal(requests[0].options.method, "DELETE");

  // A fresh node for the same console target is rejected by the registry
  // even before any busy marking: no second DELETE goes out.
  const repeat = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });
  assert.equal(await handleAction(app, { target: repeat, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "no duplicate console release while the first is in flight");

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(loads, 1, "the console view reloads after the release");
  assert.deepEqual(app.statuses, ["Releasing console t-0001\u2026", "Console released"]);
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on success");
});

test("a failed console start keeps the error on the status line and restores the control", async () => {
  await scriptContext();
  const app = statusApp();
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  globalThis.fetch = () =>
    Promise.resolve({ ok: false, status: 409, json: () => Promise.resolve({ error: { message: "console is locked" } }) });
  const button = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const handled = handleAction(app, { target: button, preventDefault() {} });
  assert.equal(button.disabled, true);
  assert.deepEqual(app.statuses, ["Starting console t-0001\u2026"]);

  assert.equal(await handled, true);
  assert.deepEqual(app.statuses, ["Starting console t-0001\u2026", "console is locked"]);
  assert.equal(loads, 0, "a rejected start never reaches the reload");
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on failure");
});

test("starting one console does not suppress a different console target", async () => {
  await scriptContext();
  const app = statusApp();
  app.load = async () => {};
  app.querySelector = () => null;
  const requests = [];
  const resolvers = new Map();
  globalThis.fetch = (path, options) => {
    requests.push({ path, options });
    return new Promise((resolve) => {
      resolvers.set(path, () => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const taskConsole = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  const taskStart = handleAction(app, { target: taskConsole, preventDefault() {} });
  assert.equal(requests.length, 1);
  assert.equal(taskConsole.disabled, true);

  // The same console target stays blocked while its start is in flight...
  const repeat = new ActionButton({ startConsole: "t-0001", project: "p-alpha", task: "t-0001" });
  assert.equal(await handleAction(app, { target: repeat, preventDefault() {} }), true);
  assert.equal(requests.length, 1, "the same console target stays suppressed");

  // ...but the project console is a different target: its start proceeds.
  const projectConsole = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });
  const projectStart = handleAction(app, { target: projectConsole, preventDefault() {} });
  assert.equal(requests.length, 2, "a distinct console target is not blocked");
  assert.equal(requests[1].path, "/ui/api/v2/projects/p-alpha/console");
  assert.equal(projectConsole.disabled, true);

  resolvers.get("/ui/api/v2/projects/p-alpha/tasks/t-0001/console")();
  assert.equal(await taskStart, true);
  resolvers.get("/ui/api/v2/projects/p-alpha/console")();
  assert.equal(await projectStart, true);
  // The task console settles first, but the project console is still in
  // flight, so settlement keeps its pending label on the line and reveals the
  // confirmation only when the final start settles.
  assert.deepEqual(app.statuses, [
    "Starting console t-0001\u2026",
    "Starting console\u2026",
    "Starting console\u2026",
    "Console starting",
  ]);
  assert.equal(taskConsole.disabled, false);
  assert.equal(projectConsole.disabled, false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains once both settle");
});

// A promise can reject with a value that is not an Error (a bare reject(null),
// an abort, or fetch middleware). Formatting that failure must stay total: if
// reading error.message threw before settleStatus ran, the in-flight key would
// leak and a repainted control would stay disabled forever.
test("a non-Error action rejection still drains the registry and shows a final failure", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () => Promise.reject(null);
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Request failed"]);
  assert.equal(button.disabled, false, "the control is restored");
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a non-Error rejection");

  // A repainted replacement is accepted again, proving the key did not leak.
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
  };
  const replacement = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  await handleAction(app, { target: replacement, preventDefault() {} });
  assert.equal(requests, 1, "the replacement control is not rejected by a leaked key");
  assert.equal(inFlight.size, 0);
});

test("a non-Error form rejection still drains the registry and shows a final failure", async () => {
  await scriptContext();
  const app = statusApp();
  globalThis.fetch = () => Promise.reject(null);
  const submitter = new ActionButton();
  const form = {
    tagName: "FORM",
    dataset: { project: "p-alpha", taskForm: "t-0001", taskFormMode: "edit" },
    elements: {
      priority: { value: "0" },
      title: { value: "Renamed" },
      body: { value: "" },
      flow_id: { value: "" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitter : null;
    },
  };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Request failed"]);
  assert.equal(submitter.disabled, false, "the submit control is restored");
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a non-Error rejection");
});

test("failureMessage formats normal rejections and survives hostile proxies", () => {
  assert.equal(failureMessage(new Error("boom")), "boom");
  assert.equal(failureMessage(new Error()), "Error");
  assert.equal(failureMessage("plain failure"), "plain failure");
  assert.equal(failureMessage(null), "Request failed");
  assert.equal(failureMessage(undefined), "Request failed");

  // A rejected Proxy whose prototype lookup throws aborts the instanceof
  // check; one whose message getter throws aborts the message read. Both must
  // still format to a safe fallback instead of throwing.
  const noPrototype = new Proxy({}, {
    getPrototypeOf() {
      throw new Error("prototype trap");
    },
  });
  assert.equal(failureMessage(noPrototype), "Request failed");
  const noMessage = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") throw new Error("message trap");
      return Reflect.get(target, prop);
    },
  });
  assert.equal(failureMessage(noMessage), "Request failed");

  // A getter can return a hostile non-string value instead of throwing. The
  // formatter must coerce inside the guard: returning the raw value would
  // make the status line's textContent assignment throw on stringification
  // later, after the key already drained.
  const hostileValue = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") {
        return {
          toString() {
            throw new Error("stringification trap");
          },
        };
      }
      return Reflect.get(target, prop);
    },
  });
  assert.equal(failureMessage(hostileValue), "Request failed");
  // A non-string message that stringifies cleanly still renders as text.
  const stringableValue = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") return { toString: () => "stringable message" };
      return Reflect.get(target, prop);
    },
  });
  assert.equal(failureMessage(stringableValue), "stringable message");
});

// A promise can reject with a Proxy whose traps throw while the settlement
// path merely formats it: getPrototypeOf (the instanceof check in
// failureMessage) or the message getter. Formatting must stay total so
// settleStatus runs, the key drains, the control is restored, and a safe
// failure message replaces the pending label.
test("an action rejection whose prototype lookup throws still drains the registry", async () => {
  await scriptContext();
  const app = statusApp();
  const hostile = new Proxy({}, {
    getPrototypeOf() {
      throw new Error("prototype trap");
    },
  });
  globalThis.fetch = () => Promise.reject(hostile);
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Request failed"]);
  assert.equal(button.disabled, false, "the control is restored");
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a hostile rejection");
});

test("an action rejection whose message getter throws still drains the registry", async () => {
  await scriptContext();
  const app = statusApp();
  const hostile = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") throw new Error("message trap");
      return Reflect.get(target, prop);
    },
  });
  globalThis.fetch = () => Promise.reject(hostile);
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Request failed"]);
  assert.equal(button.disabled, false, "the control is restored");
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a hostile rejection");
});

test("an action rejection whose message is a hostile non-string still drains the registry", async () => {
  await scriptContext();
  const app = statusApp();
  // The message getter returns a truthy object whose stringification throws:
  // the old formatter returned that raw value and the status line threw on
  // textContent assignment. It must coerce inside failureMessage instead.
  const hostile = new Proxy(new Error("boom"), {
    get(target, prop) {
      if (prop === "message") {
        return {
          toString() {
            throw new Error("stringification trap");
          },
        };
      }
      return Reflect.get(target, prop);
    },
  });
  globalThis.fetch = () => Promise.reject(hostile);
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(app, { target: button, preventDefault() {} });

  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Request failed"]);
  assert.equal(button.disabled, false, "the control is restored");
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a hostile rejection");
});

test("a hostile data-load rejection still leaves a safe status message", async () => {
  const status = { textContent: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/done" },
    setTimeout() {},
    clearTimeout() {},
  }, {
    fetch(path) {
      if (path === "/ui/api/v2/projects") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ projects: [] }) });
      }
      // The done list GET rejects with a Proxy whose prototype lookup throws:
      // the load catch must format it without throwing, or the status line
      // would never report the failure.
      return Promise.reject(new Proxy({}, {
        getPrototypeOf() {
          throw new Error("prototype trap");
        },
      }));
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelector = (selector) => {
    if (selector === "h1") return { textContent: "" };
    if (selector === ".status") return status;
    if (selector === ".content") return new InlineDOMElement("div");
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(status.textContent, "Request failed", "the hostile rejection formats to a safe fallback");
  assert.equal(app.pollFailures, 1, "the load failure is recorded as a poll failure");
});

test("a hostile form rejection still drains the registry and shows a safe failure", async () => {
  await scriptContext();
  const app = statusApp();
  const hostile = new Proxy({}, {
    getPrototypeOf() {
      throw new Error("prototype trap");
    },
  });
  globalThis.fetch = () => Promise.reject(hostile);
  const submitter = new ActionButton();
  const form = {
    tagName: "FORM",
    dataset: { project: "p-alpha", taskForm: "t-0001", taskFormMode: "edit" },
    elements: {
      priority: { value: "0" },
      title: { value: "Renamed" },
      body: { value: "" },
      flow_id: { value: "" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitter : null;
    },
  };

  const handled = await handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(handled, true);
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Request failed"]);
  assert.equal(submitter.disabled, false, "the submit control is restored");
  assert.equal(inFlight.size, 0, "the in-flight registry drains on a hostile rejection");
});

// Two distinct mutations may be in flight at once; the shared status line must
// keep naming a still-pending mutation after an earlier one settles, and only
// reveal a result once nothing is pending. These cover out-of-order success
// and failure settlement.
function controllableFetch() {
  const pending = [];
  globalThis.fetch = () =>
    new Promise((resolve) => {
      pending.push((ok = true, body = {}) =>
        resolve({ ok, status: ok ? 200 : 409, json: () => Promise.resolve(body) }),
      );
    });
  return pending;
}

test("settling one action keeps a still-pending sibling's label on the status line", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });
  assert.deepEqual(app.statuses, ["Scheduling t-0001\u2026", "Scheduling t-0002\u2026"]);
  assert.equal(inFlight.size, 2);

  // The second mutation settles first: its "Scheduled" must not clobber the
  // first mutation's still-pending label.
  pending[1](true);
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0001\u2026");
  assert.equal(inFlight.size, 1);

  // When the final mutation settles, its own result stays visible.
  pending[0](true);
  await first;
  assert.deepEqual(app.statuses, [
    "Scheduling t-0001\u2026",
    "Scheduling t-0002\u2026",
    "Scheduling t-0001\u2026",
    "Scheduled",
  ]);
  assert.equal(inFlight.size, 0);
});

test("an early failure does not hide a still-pending sibling's label", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });

  // The first mutation fails while the second is still running: the failure is
  // suppressed in favour of the second mutation's pending label.
  pending[0](false, { error: { message: "workflow is locked" } });
  await first;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0002\u2026");
  assert.equal(inFlight.size, 1);

  // The final mutation's success then stays visible.
  pending[1](true);
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduled");
  assert.equal(inFlight.size, 0);
});

test("the final mutation's failure stays visible after a sibling already settled", async () => {
  await scriptContext();
  const app = statusApp();
  const pending = controllableFetch();
  const taskA = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const taskB = new ActionButton({ workflowSchedule: "t-0002", project: "p-alpha" });

  const first = handleAction(app, { target: taskA, preventDefault() {} });
  const second = handleAction(app, { target: taskB, preventDefault() {} });

  // The first mutation succeeds while the second is pending: its result is
  // suppressed and the second's pending label stays.
  pending[0](true);
  await first;
  assert.equal(app.statuses[app.statuses.length - 1], "Scheduling t-0002\u2026");

  // The final mutation fails: with nothing left pending, the failure shows.
  pending[1](false, { error: { message: "boom" } });
  await second;
  assert.equal(app.statuses[app.statuses.length - 1], "boom");
  assert.equal(inFlight.size, 0);
});

test("a cancelled confirm clears the pending label and issues no request", async () => {
  await scriptContext({ confirm: () => false });
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowReset: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });

  // The pending label is written synchronously on click, before the confirm.
  assert.deepEqual(app.statuses, ["Resetting t-0001\u2026"]);
  assert.equal(button.disabled, true);

  assert.equal(await handled, true);
  // Backing out of the confirm clears the pending label the click created and
  // restores the control — no request went out.
  assert.deepEqual(app.statuses, ["Resetting t-0001\u2026", ""]);
  assert.equal(requests, 0, "a cancelled confirm issues no request");
  assert.equal(button.disabled, false);
  assert.equal(button.getAttribute("aria-busy"), null);
  assert.equal(button.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0);
});

test("a cancelled prompt clears the pending label and issues no request", async () => {
  await scriptContext({ prompt: () => null });
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowDone: "t-0001", project: "p-alpha" });

  const handled = handleAction(app, { target: button, preventDefault() {} });
  assert.deepEqual(app.statuses, ["Closing out t-0001\u2026"]);

  assert.equal(await handled, true);
  assert.deepEqual(app.statuses, ["Closing out t-0001\u2026", ""]);
  assert.equal(requests, 0, "a cancelled prompt issues no request");
  assert.equal(button.disabled, false);
  assert.equal(inFlight.size, 0);
});

