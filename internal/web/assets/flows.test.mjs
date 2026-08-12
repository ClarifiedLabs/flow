// <flow-flows> element tests, ported from app.test.mjs when the view became an
// element: the agent-def catalog's inline edit/create, the flow editor's
// row controls and kind switch, and the clone single-flight behavior.

import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";
import { flushAsync } from "./test-helpers.mjs";

const root = installTestDOM();
const { NEW_AGENT_DEF_STATE } = await import("./elements/flows-view.js");
await import("./elements/flows.js");

const AGENT_OPTIONS = [{ name: "harness", display_name: "Harness" }];
const AUTHOR_DEF = { id: "ad-author", name: "author", harness: "harness", model: "anthropic:opus" };

function flowsApp() {
  const app = document.createElement("flow-app");
  app.loads = 0;
  app.statuses = [];
  app.load = async () => { app.loads += 1; };
  app.setStatus = (message) => app.statuses.push(message);
  app.selectedProjectIDs = () => [];
  app.projects = [{ id: "p-alpha", name: "alpha" }];
  root.appendChild(app);
  return app;
}

function flowsPayload(overrides = {}) {
  return {
    project: { id: "p-alpha", name: "alpha" },
    projects: [{ id: "p-alpha", name: "alpha" }],
    globalAgentDefs: [],
    agentDefs: [AUTHOR_DEF],
    flows: [],
    defaultFlowID: "",
    agentOptions: AGENT_OPTIONS,
    ...overrides,
  };
}

const REVIEW_FLOW = {
  id: "fl-1",
  name: "coding",
  start_node: "implement",
  nodes: [
    { key: "implement", name: "Implement", kind: "agent", config: { agent: { agent_def_id: "ad-author" } } },
    { key: "review", name: "Review", kind: "change_review", config: { change_review: { agents: [{ agent_def_id: "ad-author", blocking: true }], aggregator_agent_def_id: "ad-author" } } },
  ],
  edges: [{ from: "implement", outcome: "done", to: "review" }],
};

test("agent definition table actions enter inline edit and create modes", async () => {
  const app = flowsApp();
  const element = mountElement(app, "flow-flows", flowsPayload({ agentDefs: [{ id: "ad-review", name: "reviewer", harness: "harness" }] }));
  await flush();

  element.querySelector('[data-edit-def="ad-review"]').click();
  await flush();
  assert.equal(element.editingDefID, "ad-review");
  assert.equal(app.loads, 1);

  element.querySelector("[data-agent-defs-section]").querySelector("[data-add-def]").click();
  await flush();
  assert.equal(element.editingDefID, NEW_AGENT_DEF_STATE);
  assert.equal(app.loads, 2);

  // The same action in the global catalog edits its own key.
  element.querySelector("[data-global-agent-defs-section]").querySelector("[data-add-def]").click();
  await flush();
  assert.equal(element.editingGlobalDefID, NEW_AGENT_DEF_STATE);
  assert.equal(app.loads, 3);
});

test("parallel review controls add, remove, and reorder agent rows", async () => {
  const app = flowsApp();
  const element = mountElement(app, "flow-flows", flowsPayload({ flows: [REVIEW_FLOW] }));
  element.editingFlowID = "fl-1";
  await flush();

  const rows = () => element.querySelectorAll("[data-review-agent-row]");
  assert.equal(rows().length, 1, "the stored agent row renders");

  element.querySelector("[data-add-review-agent]").click();
  await flush();
  assert.equal(rows().length, 2, "Add agent appends a structured row");
  const appended = rows()[1];
  const blocking = appended.querySelector('[name="review_agent_blocking"]');
  assert.ok(blocking, "the appended row carries its blocking checkbox");
  assert.ok(blocking.hasAttribute("checked"), "the appended row defaults to blocking");

  element.querySelectorAll("[data-review-agent-remove]")[1].click();
  await flush();
  assert.equal(rows().length, 1, "remove drops the row again");

  element.querySelector("[data-add-review-agent]").click();
  await flush();
  const [first, second] = rows();
  first.querySelector("[data-review-agent-down]").click();
  await flush();
  const reordered = rows();
  assert.equal(reordered[0], second, "moving down swaps the rows");
  assert.equal(reordered[1], first);
});

test("switching to either parallel review kind initializes its structured config", async () => {
  const app = flowsApp();
  const element = mountElement(app, "flow-flows", flowsPayload({ flows: [REVIEW_FLOW] }));
  element.editingFlowID = "fl-1";
  await flush();

  const card = element.querySelectorAll("[data-node-card]")[0];
  const editor = card.querySelector("[data-node-config-editor]");
  assert.ok(card.querySelector('[name="node_config"]'), "an agent node starts on the JSON editor");

  const kindSelect = card.querySelector('[name="node_kind"]');
  kindSelect.value = "change_review";
  kindSelect.dispatchEvent(new Event("change", { bubbles: true }));
  await flush();
  assert.match(editor.innerHTML, /data-review-config-key="change_review"/);
  assert.equal((editor.innerHTML.match(/data-review-agent-row(?:\s|>)/g) || []).length, 1);
  assert.match(editor.innerHTML, /name="review_agent_def_id"[^>]*required/);
  assert.match(editor.innerHTML, /name="review_aggregator_agent_def_id"[^>]*required/);
  assert.doesNotMatch(editor.innerHTML, /name="node_config"/);

  kindSelect.value = "verify_change";
  kindSelect.dispatchEvent(new Event("change", { bubbles: true }));
  await flush();
  assert.match(editor.innerHTML, /data-review-config-key="verify_change"/);
  assert.match(editor.innerHTML, /Blocks success/);
  assert.doesNotMatch(editor.innerHTML, /review_aggregator_agent_def_id/);
  assert.doesNotMatch(editor.innerHTML, /name="node_config"/);
});

function cloneFetch(flows, tracking = {}) {
  const originalFetch = globalThis.fetch;
  const fetchCalls = [];
  globalThis.fetch = (path, options = {}) => {
    fetchCalls.push({ path: String(path), options });
    if ((options.method || "GET") === "GET") {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ flows: typeof flows === "function" ? flows() : flows }) });
    }
    if (tracking.holdFirstPost && !tracking.held) {
      tracking.held = true;
      return new Promise((resolve) => {
        tracking.releaseFirstPost = () => resolve({ ok: true, status: 201, json: () => Promise.resolve({ flow: { id: "fl-new" } }) });
      });
    }
    return Promise.resolve({ ok: true, status: 201, json: () => Promise.resolve({ flow: { id: tracking.nextID || "fl-new" } }) });
  };
  return { fetchCalls, restore: () => { globalThis.fetch = originalFetch; } };
}

test("clone flow button posts a copied create payload and opens the new flow editor", async () => {
  const app = flowsApp();
  const flows = [REVIEW_FLOW];
  const { fetchCalls, restore } = cloneFetch(flows);
  try {
    const element = mountElement(app, "flow-flows", flowsPayload({ flows }));
    await flush();

    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    await flush();

    // The handler re-reads the current flow list before posting the clone.
    assert.equal(fetchCalls[0].path, "/ui/api/v2/projects/p-alpha/flows");
    const post = fetchCalls.find((call) => call.options.method === "POST");
    const body = JSON.parse(post.options.body);
    assert.equal(body.name, "coding (copy)");
    assert.deepEqual(body.nodes[0], { key: "implement", name: "Implement", kind: "agent", config: { agent: { agent_def_id: "ad-author" } } });
    assert.deepEqual(body.edges, [{ from: "implement", outcome: "done", to: "review" }]);
    // The created flow's id is unwrapped from the {flow} envelope and opened.
    assert.equal(element.editingFlowID, "fl-new");
    assert.equal(app.loads, 1);
    assert.deepEqual(app.statuses, ["flow cloned; rename and edit your copy"]);
  } finally {
    restore();
  }
});

test("clone flow button posts an incremented copy name when the copy already exists", async () => {
  const app = flowsApp();
  const flows = [
    { id: "fl-1", name: "coding", nodes: [], edges: [] },
    { id: "fl-2", name: "coding (copy)", nodes: [], edges: [] },
  ];
  const { fetchCalls, restore } = cloneFetch(flows);
  try {
    const element = mountElement(app, "flow-flows", flowsPayload({ flows }));
    await flush();

    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    await flush();

    const post = fetchCalls.find((call) => call.options.method === "POST");
    assert.equal(JSON.parse(post.options.body).name, "coding (copy 2)");
    assert.equal(element.editingFlowID, "fl-new");
    assert.equal(app.loads, 1);
  } finally {
    restore();
  }
});

test("rapid clone clicks are single-flighted and re-read the current flow list at click time", async () => {
  const app = flowsApp();
  // The server-side list grows as clones land; the element's payload list
  // does not (it is only refreshed when the reload re-mounts).
  let currentFlows = [{ id: "fl-1", name: "coding", nodes: [], edges: [] }];
  const tracking = { holdFirstPost: true, nextID: "fl-new-2" };
  const { fetchCalls, restore } = cloneFetch(() => currentFlows, tracking);
  try {
    const element = mountElement(app, "flow-flows", flowsPayload({ flows: [{ id: "fl-1", name: "coding", nodes: [], edges: [] }] }));
    await flush();
    const button = element.querySelector('[data-clone-flow="fl-1"]');

    // Two clicks land before the first clone's reload settles.
    button.click();
    button.click();

    // The duplicate click is single-flighted: only the first click re-read
    // the list, and no second POST was submitted while the first was in flight.
    assert.equal(fetchCalls.length, 1);
    await flushAsync();
    const posts = fetchCalls.filter((call) => call.options.method === "POST");
    assert.equal(posts.length, 1);
    assert.equal(JSON.parse(posts[0].options.body).name, "coding (copy)");

    // The server has created the clone, but this payload has not refreshed yet.
    currentFlows = [
      { id: "fl-1", name: "coding", nodes: [], edges: [] },
      { id: "fl-new", name: "coding (copy)", nodes: [], edges: [] },
    ];
    tracking.releaseFirstPost();
    await flushAsync();
    await flush();
    assert.equal(element.editingFlowID, "fl-new");
    assert.equal(app.loads, 1);
    assert.deepEqual(app.statuses, ["flow cloned; rename and edit your copy"]);

    // A later click re-reads the current list instead of the stale payload,
    // so the next clone picks the incremented suffix.
    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    const allPosts = fetchCalls.filter((call) => call.options.method === "POST");
    assert.equal(allPosts.length, 2);
    assert.equal(JSON.parse(allPosts[1].options.body).name, "coding (copy 2)");
  } finally {
    restore();
  }
});

test("clone clicks stay single-flighted across a repaint while the first clone is pending", async () => {
  const app = flowsApp();
  // The server-side list grows as clones land; the element's payload list
  // does not (it is only refreshed when the reload re-mounts).
  let serverFlows = [{ id: "fl-1", name: "coding", nodes: [], edges: [] }];
  const tracking = { holdFirstPost: true, nextID: "fl-new-2" };
  const { fetchCalls, restore } = cloneFetch(() => serverFlows, tracking);
  try {
    const element = mountElement(app, "flow-flows", flowsPayload({ flows: [{ id: "fl-1", name: "coding", nodes: [], edges: [] }] }));
    await flush();
    const posts = () => fetchCalls.filter((call) => call.options.method === "POST");

    // First clone click: the click-time re-read finds no copy yet, and the
    // POST is held pending while the element repaints.
    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    assert.equal(posts().length, 1);
    assert.equal(JSON.parse(posts()[0].options.body).name, "coding (copy)");

    // The pending clone's repaint disabled the fresh button; a click on it is
    // single-flighted: no second "<name> (copy)" POST goes out.
    await flush();
    const button = element.querySelector('[data-clone-flow="fl-1"]');
    assert.equal(button.hasAttribute("disabled"), true, "the guard disables clone buttons while a clone is pending");
    button.click();
    await flushAsync();
    assert.equal(posts().length, 1);

    // The pending clone lands; the server list grows to include the copy.
    serverFlows = [
      { id: "fl-1", name: "coding", nodes: [], edges: [] },
      { id: "fl-new", name: "coding (copy)", nodes: [], edges: [] },
    ];
    tracking.releaseFirstPost();
    await flushAsync();
    await flush();
    assert.equal(element.editingFlowID, "fl-new");
    assert.equal(app.loads, 1);
    assert.deepEqual(app.statuses, ["flow cloned; rename and edit your copy"]);
    assert.equal(element.querySelector('[data-clone-flow="fl-1"]').hasAttribute("disabled"), false, "settling re-enables the buttons");

    // A later click re-reads the current list and picks the incremented
    // suffix instead of colliding with the just-created flow.
    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    assert.equal(posts().length, 2);
    assert.equal(JSON.parse(posts()[1].options.body).name, "coding (copy 2)");
  } finally {
    restore();
  }
});

test("clone flow button surfaces a server-side name collision without partial mutation", async () => {
  const app = flowsApp();
  const originalFetch = globalThis.fetch;
  const fetchCalls = [];
  globalThis.fetch = (path, options = {}) => {
    fetchCalls.push({ path: String(path), options });
    if ((options.method || "GET") === "GET") {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ flows: [{ id: "fl-1", name: "coding", nodes: [], edges: [] }] }) });
    }
    return Promise.resolve({
      ok: false,
      status: 409,
      json: () => Promise.resolve({ error: { message: "a flow with this name already exists" } }),
    });
  };
  try {
    const element = mountElement(app, "flow-flows", flowsPayload({ flows: [{ id: "fl-1", name: "coding", nodes: [], edges: [] }] }));
    await flush();

    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    await flush();

    // The collision is surfaced, and nothing was mutated or reloaded.
    assert.deepEqual(app.statuses, ["a flow with this name already exists"]);
    assert.equal(element.editingFlowID, "");
    assert.equal(app.loads, 0);
    assert.equal(fetchCalls.filter((call) => call.options.method === "POST").length, 1);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("clone flow button falls back to the payload names when the click-time re-read rejects", async () => {
  const app = flowsApp();
  const originalFetch = globalThis.fetch;
  const fetchCalls = [];
  globalThis.fetch = (path, options = {}) => {
    fetchCalls.push({ path: String(path), options });
    if ((options.method || "GET") === "GET") {
      return Promise.reject(new Error("flows list unavailable"));
    }
    return Promise.resolve({ ok: true, status: 201, json: () => Promise.resolve({ flow: { id: "fl-new" } }) });
  };
  try {
    // The payload list already contains the first copy; the click-time
    // re-read rejects, so the handler must fall back to these names and still
    // submit the clone with the next available suffix.
    const flows = [
      { id: "fl-1", name: "coding", nodes: [], edges: [] },
      { id: "fl-2", name: "coding (copy)", nodes: [], edges: [] },
    ];
    const element = mountElement(app, "flow-flows", flowsPayload({ flows }));
    await flush();

    element.querySelector('[data-clone-flow="fl-1"]').click();
    await flushAsync();
    await flush();

    assert.equal(fetchCalls.length, 2);
    const post = fetchCalls.find((call) => call.options.method === "POST");
    assert.equal(JSON.parse(post.options.body).name, "coding (copy 2)");
    // The clone proceeds exactly like a successful re-read.
    assert.equal(element.editingFlowID, "fl-new");
    assert.equal(app.loads, 1);
    assert.deepEqual(app.statuses, ["flow cloned; rename and edit your copy"]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
