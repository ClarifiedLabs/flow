// Regression tests for the automation-budget modal: the GUI used to collect
// only the count (window.prompt) and POST {additional} without instructions,
// which the server always rejects — so the operation could never succeed from
// the web UI. workflowBudget must open a modal instead of prompting, and
// budgetGrant must send {additional, instructions} and show the disposition
// inline instead of failing on the status bar.

import assert from "node:assert/strict";
import { test } from "node:test";
import { ActionButton, InlineDOMElement, inlineDocument, scriptContext } from "./test-helpers.mjs";
import { handleAction } from "./actions.js";
import {
  budgetModalHost,
  budgetValidationError,
  renderBudgetModal,
} from "./budget-modal.js";

// A minimal inline DOM that can host the modal layer: querySelector(".main")
// resolves to the host itself, which is what budgetModalHost looks for, and
// the layer's own querySelector surface is extended in appendChild with the
// value-bearing fields budgetGrant parses out of the rendered markup.
class ModalHost extends InlineDOMElement {
  constructor(fields = {}) {
    super("main");
    this.fields = fields;
    this.modals = [];
    this.querySelector = (selector) => (selector === ".main" ? this : null);
  }
  appendChild(child) {
    child.parentElement = this;
    this.children.push(child);
    this.modals.push(child);
    const original = child.querySelector?.bind(child);
    if (typeof original === "function") {
      child.querySelector = (query) => (query in this.fields ? { value: this.fields[query] } : original(query));
    }
    return child;
  }
}

// The app stand-in: a recording status line plus the modal host the handlers
// render into. The budget button reports the host as its closest flow-app,
// which is how the browser-side control reaches the app root.
function budgetApp(fields = {}) {
  const statuses = [];
  const host = new ModalHost(fields);
  return {
    statuses,
    host,
    setStatus(message) {
      statuses.push(message);
    },
    refresh() {},
  };
}

function budgetButton(dataset, host) {
  const button = new ActionButton(dataset);
  button.closest = (selector) => (selector === "flow-app" ? host : null);
  return button;
}

// A grant button that reports the open modal layer as its closest layer, the
// way a real click inside the rendered dialog does.
function grantButton(dataset, layer) {
  const button = new ActionButton(dataset);
  button.closest = (selector) => (selector === "[data-budget-modal-layer]" ? layer : null);
  return button;
}

const RUN = {
  state: "running",
  current_node_key: "review",
  transitions_used: 13,
  transition_budget: 100,
  review_cycles_used: 5,
  review_cycle_budget: 5,
};

test("renderBudgetModal marks the instructions required and keeps every control off the submit path", () => {
  const html = renderBudgetModal({ taskID: "t-0001", kind: "review-cycles", used: 3, total: 3 });

  assert.match(html, /Grant review cycles/);
  assert.match(html, /Review cycles 3\/3/);
  assert.match(html, /Instructions \(required\)/);
  assert.match(html, /Required\. Recorded with the decision and delivered to the next author session\./);
  assert.match(html, /data-budget-instructions/);
  assert.equal(/<form/.test(html), false, "an unkeyed form would fall through to a native GET submit");
  assert.equal(/type="(submit|button)"/.test(html), true);
  for (const control of html.match(/<(button|input|textarea)[^>]*>/g) || []) {
    if (control.startsWith("<button")) {
      assert.match(control, /type="button"/, `control must not submit: ${control}`);
    }
  }
  assert.match(html, /data-budget-cancel/);
  assert.match(html, /data-budget-grant="t-0001"/);
});

test("renderBudgetModal switches copy, defaults, and totals for the transitions kind", () => {
  const html = renderBudgetModal({ taskID: "t-0002", kind: "transitions", used: 13, total: 50 });

  assert.match(html, /Extend automation budget/);
  assert.match(html, /Transitions 13\/50/);
  assert.match(html, /Additional transitions/);
  assert.match(html, /value="50"/);
  assert.match(html, /data-budget-grant="t-0002"/);
  const review = renderBudgetModal({ taskID: "t-0003", kind: "review-cycles" });
  assert.match(review, /value="2"/);
  assert.match(review, /Additional cycles/);
});

test("renderBudgetModal shows no usage line when the totals are unknown", () => {
  const html = renderBudgetModal({ taskID: "t-0004", kind: "review-cycles" });
  assert.doesNotMatch(html, /Review cycles 0\/0/);
});

test("renderBudgetModal renders the disposition view from the POST response", () => {
  const html = renderBudgetModal({
    taskID: "t-0005",
    kind: "review-cycles",
    result: { additional: 2, totals: { used: 5, total: 5 }, run: RUN },
  });

  assert.match(html, /Granted 2 review cycles/);
  assert.match(html, /Review cycles<\/dt><dd>5\/5/);
  assert.match(html, /Run state<\/dt><dd>running/);
  assert.match(html, /Current node<\/dt><dd><code>review<\/code>/);
  assert.match(html, /data-budget-done/);
  assert.doesNotMatch(html, /data-budget-instructions/);
});

// modalContext installs the inline document (with a working createElement)
// the modal layer needs, then hands back the action surface. Every
// handler-driven test opens the modal through it.
async function modalContext() {
  return scriptContext({}, { document: inlineDocument() });
}

test("opening the budget modal issues no request", async () => {
  await modalContext();
  const app = budgetApp();
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ run: RUN }) });
  };
  const button = budgetButton({ workflowBudget: "t-0006", workflowBudgetKind: "review-cycles", project: "p-alpha" }, app.host);

  assert.equal(await handleAction(app, { target: button, preventDefault() {} }), true);
  assert.deepEqual(calls, [], "opening the modal must not fetch");
  assert.equal(app.host.modals.length, 1, "the modal layer is mounted");
  assert.match(app.host.modals[0].innerHTML, /data-budget-instructions/);
  assert.match(app.host.modals[0].innerHTML, /Grant review cycles/);
  assert.equal(app.host.modals[0].dataset.budgetKind, "review-cycles");
  // CANCELLED clears the pending label the click wrote.
  assert.deepEqual(app.statuses, ["Extending budget t-0006…", ""]);
});

test("granting posts the count and the instructions and shows the new totals inline", async () => {
  await modalContext();
  const app = budgetApp({ "[data-budget-additional]": "2", "[data-budget-instructions]": "keep iterating on the review findings" });
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ run: RUN }) });
  };
  let refreshed = 0;
  app.refresh = () => {
    refreshed += 1;
  };
  const open = budgetButton({ workflowBudget: "t-0007", workflowBudgetKind: "review-cycles", project: "p-alpha" }, app.host);
  await handleAction(app, { target: open, preventDefault() {} });
  const layer = app.host.modals[0];
  const grant = grantButton({ budgetGrant: "t-0007", project: "p-alpha" }, layer);

  assert.equal(await handleAction(app, { target: grant, preventDefault() {} }), true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/ui/api/v2/projects/p-alpha/tasks/t-0007/workflow/budget");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    additional: 2,
    instructions: "keep iterating on the review findings",
  });
  assert.equal(refreshed, 1);
  assert.match(layer.innerHTML, /Granted 2 review cycles/);
  assert.match(layer.innerHTML, /Review cycles<\/dt><dd>5\/5/);
  assert.match(layer.innerHTML, /data-budget-done/);
  assert.deepEqual(app.statuses.slice(-1), ["Granted 2 review cycles"]);
});

test("granting without the project scopes the POST to the global task route", async () => {
  await modalContext();
  const app = budgetApp({ "[data-budget-additional]": "50", "[data-budget-instructions]": "unblock" });
  const calls = [];
  globalThis.fetch = (path, options) => {
    calls.push({ path, options });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ run: RUN }) });
  };
  const open = budgetButton({ workflowBudget: "t-0008", workflowBudgetKind: "transitions" }, app.host);
  await handleAction(app, { target: open, preventDefault() {} });
  const layer = app.host.modals[0];
  assert.equal(layer.dataset.budgetKind, "transitions");

  const grant = grantButton({ budgetGrant: "t-0008" }, layer);
  await handleAction(app, { target: grant, preventDefault() {} });
  assert.equal(calls[0].path, "/ui/api/v2/tasks/t-0008/workflow/budget");
  assert.deepEqual(JSON.parse(calls[0].options.body), { additional: 50, instructions: "unblock" });
  assert.match(layer.innerHTML, /Granted 50 transitions/);
  assert.match(layer.innerHTML, /Transitions<\/dt><dd>13\/100/);
});

test("blank instructions render the inline error and never reach the network", async () => {
  await modalContext();
  const app = budgetApp({ "[data-budget-additional]": "2", "[data-budget-instructions]": "   " });
  const calls = [];
  globalThis.fetch = () => {
    calls.push(1);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ run: RUN }) });
  };
  const open = budgetButton({ workflowBudget: "t-0009", workflowBudgetKind: "review-cycles" }, app.host);
  await handleAction(app, { target: open, preventDefault() {} });
  const layer = app.host.modals[0];
  const grant = grantButton({ budgetGrant: "t-0009" }, layer);

  assert.equal(await handleAction(app, { target: grant, preventDefault() {} }), true);
  assert.deepEqual(calls, [], "the server's blank-instructions 400 must never be reachable from the GUI");
  assert.match(layer.innerHTML, /role="alert"/);
  assert.match(layer.innerHTML, /Instructions are required to extend an automation budget/);
  // The form is still there for the retry.
  assert.match(layer.innerHTML, /data-budget-instructions/);
  assert.match(layer.innerHTML, /data-budget-grant="t-0009"/);
});

test("a rejected grant renders the failure inline, keeps the typed input, and stays open", async () => {
  await modalContext();
  const app = budgetApp({ "[data-budget-additional]": "2", "[data-budget-instructions]": "one more pass" });
  let resolveRequest;
  globalThis.fetch = () => new Promise((resolve) => {
    resolveRequest = () => resolve({
      ok: false,
      status: 409,
      json: () => Promise.resolve({ error: { code: "workflow_conflict", message: "workflow is not waiting on an automation budget" } }),
    });
  });
  let refreshed = 0;
  app.refresh = () => {
    refreshed += 1;
  };
  const open = budgetButton({ workflowBudget: "t-0010", workflowBudgetKind: "review-cycles", project: "p-alpha" }, app.host);
  await handleAction(app, { target: open, preventDefault() {} });
  const layer = app.host.modals[0];
  const grant = grantButton({ budgetGrant: "t-0010", project: "p-alpha" }, layer);

  const handled = handleAction(app, { target: grant, preventDefault() {} });
  // While the POST is in flight the layer is flagged pending, so the dialog's
  // cancel (Escape) handler refuses to drop the outcome.
  assert.equal(layer.dataset.budgetPending, "true");
  assert.equal(layer.innerHTML.includes("role=\"alert\""), false, "no error is shown before the response");

  resolveRequest();
  assert.equal(await handled, true);
  assert.equal(refreshed, 0, "a rejected grant must not refresh");
  assert.equal(layer.dataset.budgetPending, undefined, "the re-render clears the pending flag");
  assert.equal(app.host.modals.length, 1, "the modal stays open");
  assert.match(layer.innerHTML, /role="alert"/);
  assert.match(layer.innerHTML, /workflow is not waiting on an automation budget/);
  assert.match(layer.innerHTML, /data-budget-instructions/);
  assert.deepEqual(app.statuses.slice(-1), ["workflow is not waiting on an automation budget"]);
});

test("budgetValidationError mirrors the server's contract", () => {
  assert.equal(budgetValidationError("2", "ok"), "");
  assert.match(budgetValidationError("", "ok"), /whole number between 1 and 500/);
  assert.match(budgetValidationError("0", "ok"), /whole number between 1 and 500/);
  assert.match(budgetValidationError("501", "ok"), /whole number between 1 and 500/);
  assert.match(budgetValidationError("2.5", "ok"), /whole number between 1 and 500/);
  assert.match(budgetValidationError("2", "  "), /Instructions are required/);
});

test("budgetModalHost prefers .main and falls back defensively", () => {
  const main = new InlineDOMElement("main");
  const content = new InlineDOMElement("section");
  const root = new InlineDOMElement("flow-app");
  root.appendChild(main);
  main.appendChild(content);
  root.querySelector = (selector) => (selector === ".main" ? main : null);
  const shallow = new InlineDOMElement("flow-app");
  shallow.querySelector = (selector) => (selector === ".main" ? null : selector === ".content" ? content : null);

  assert.equal(budgetModalHost(root), main);
  assert.equal(budgetModalHost(shallow), content);
  assert.equal(budgetModalHost(undefined), undefined);
});

test("a grant button outside an open modal reports instead of posting", async () => {
  await modalContext();
  const app = budgetApp();
  const calls = [];
  globalThis.fetch = () => {
    calls.push(1);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ run: RUN }) });
  };
  const orphan = grantButton({ budgetGrant: "t-0011" }, null);

  assert.equal(await handleAction(app, { target: orphan, preventDefault() {} }), true);
  assert.deepEqual(calls, []);
  assert.deepEqual(app.statuses.slice(-1), ["The budget prompt is no longer open"]);
});

