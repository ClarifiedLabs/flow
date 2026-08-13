// Flows page pure-function tests: the task form's flow select, agent-def
// form payloads, flow-editor payloads, and clone naming. Element-level flows
// tests live in flows.test.mjs.

import assert from "node:assert/strict";
import { test } from "node:test";
import { SmokeElement, browserSmokeHarness, scriptContext } from "./test-helpers.mjs";

// --- composable flows: task form, board badge, gate + flows editor ------------

test("task form flow select preselects the project default flow without annotating its name", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];
  app.flowsByProject = new Map([["p-alpha", {
    flows: [
      { id: "fl-basic", name: "basic" },
      { id: "fl-plan", name: "planned" },
    ],
    defaultFlowID: "fl-plan",
  }]]);

  const html = app.renderTaskForm({ title: "", priority: 0 }, { mode: "create", projectID: "p-alpha", submitLabel: "Create" });

  assert.match(html, /<select name="flow_id" data-flow-select>/);
  assert.match(html, /<option value="fl-basic" >basic<\/option>/);
  assert.match(html, /<option value="fl-plan" selected>planned<\/option>/);
  assert.doesNotMatch(html, /\(default\)/);
});

test("task form flow select preselects the task's saved flow when editing", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.flowsByProject = new Map([["p-alpha", {
    flows: [
      { id: "fl-basic", name: "basic" },
      { id: "fl-plan", name: "planned" },
    ],
    defaultFlowID: "fl-plan",
  }]]);

  const html = app.renderTaskForm({ title: "T", flow_id: "fl-basic" }, { taskID: "t-alpha-0001", projectID: "p-alpha" });

  assert.match(html, /<option value="fl-basic" selected>basic<\/option>/);
  assert.match(html, /<option value="fl-plan" >planned<\/option>/);
  assert.doesNotMatch(html, /\(default\)/);
});

test("wait reason phase_approval maps to a human label", async () => {
  const context = await scriptContext();
  assert.equal(context.waitReasonLabel("phase_approval"), "waiting for phase approval");
  assert.doesNotMatch(context.waitReasonLabel("phase_approval"), /plan/);
});

test("flows editor markup opts into shared form styling and accessible row controls", async () => {
  const context = await scriptContext();
  const agentOptions = [{ name: "harness", display_name: "Harness", models: [] }];

  const inheritedDef = { id: "ad-global", name: "shared", harness: "harness", prompt: "Shared prompt", inherited: true };
  const inheritedReadHTML = context.renderAgentDefsSectionView([inheritedDef], agentOptions, { editingDefID: "" });
  assert.match(inheritedReadHTML, /Project Agent Definitions/);
  assert.match(inheritedReadHTML, /badge idle">inherited/);
  assert.match(inheritedReadHTML, /data-edit-def="ad-global">Override/);
  assert.match(inheritedReadHTML, /data-add-def/);
  assert.doesNotMatch(inheritedReadHTML, /data-agent-def-form/);

  const inheritedEditHTML = context.renderAgentDefsSectionView([inheritedDef], agentOptions, { editingDefID: "ad-global" });
  assert.match(inheritedEditHTML, /<form class="agent-def-table-form" data-agent-def-form data-def-id="ad-global">/);
  assert.match(inheritedEditHTML, /name="def_name" value="shared" aria-label="Name" readonly required/);
  assert.match(inheritedEditHTML, /name="def_harness" data-def-harness aria-label="Harness"/);
  assert.match(inheritedEditHTML, /name="def_model" data-def-model aria-label="Model"/);
  assert.match(inheritedEditHTML, /name="def_reasoning_effort" data-def-reasoning aria-label="Effort"/);
  assert.match(inheritedEditHTML, /type="submit">Save<\/button>[\s\S]*data-def-cancel>Cancel<\/button>/);
  assert.match(inheritedEditHTML, /data-agent-def-edit-row>[\s\S]*<\/tr>\s*<tr class="agent-def-prompt-row" data-agent-def-prompt-row>[\s\S]*Shared prompt/);
  assert.doesNotMatch(inheritedEditHTML, /data-delete-def="ad-global"/);

  const globalHTML = context.renderGlobalAgentDefsSectionView([], agentOptions, { editingGlobalDefID: "" });
  assert.match(globalHTML, /Global Agent Definitions/);
  assert.match(globalHTML, /Every project inherits/);
  assert.match(globalHTML, /<tr class="agent-def-add-row">[\s\S]*data-add-def[\s\S]*>\+<\/button>/);
  assert.doesNotMatch(globalHTML, /data-agent-def-form|No agent definitions/);

  const globalCreateHTML = context.renderGlobalAgentDefsSectionView([], agentOptions, { editingGlobalDefID: context.NEW_AGENT_DEF_STATE });
  assert.match(globalCreateHTML, /data-agent-def-form data-def-id=""/);
  assert.match(globalCreateHTML, /name="def_name" value="" aria-label="Name" required/);
  assert.match(globalCreateHTML, /data-agent-def-edit-row>[\s\S]*<\/tr>\s*<tr class="agent-def-prompt-row"/);
  assert.doesNotMatch(globalCreateHTML, /data-add-def/);

  const flowHTML = context.renderFlowEditorView({
    name: "custom",
    start_node: "plan",
    nodes: [{ key: "plan", name: "Plan", kind: "agent" }, { key: "done", name: "Done", kind: "terminal" }],
    edges: [{ from: "plan", outcome: "completed", to: "done" }],
  }, []);
  assert.match(flowHTML, /<form class="flow-editor task-form"/);
  assert.match(flowHTML, /class="flow-row-list wide" data-node-cards/);
  assert.match(flowHTML, /class="flow-row-actions wide"><button[^>]+data-add-node/);
  assert.match(flowHTML, /class="flow-row-list wide" data-edge-rows/);
  assert.match(flowHTML, /class="flow-row-actions wide"><button[^>]+data-add-edge/);
  assert.match(flowHTML, /class="workflow-chart flow-graph-preview" data-graph-preview/);
  assert.match(flowHTML, /<svg[^>]*aria-label="custom workflow definition"/);
  assert.match(flowHTML, /data-node="plan"/);
  assert.match(flowHTML, /data-edge-outcome="completed"/);

  const nodeHTML = context.renderNodeCardView({ key: "plan", name: "Plan", kind: "agent" });
  assert.match(nodeHTML, /class="flow-row flow-node-card" data-node-card/);
  assert.match(nodeHTML, /aria-label="Node key"/);
  assert.match(nodeHTML, /placeholder="Short display name \(e\.g\. Implement\)"/);
  assert.match(nodeHTML, /aria-label="Node name"/);
  assert.match(nodeHTML, /aria-label="Trusted node kind"/);
  assert.match(nodeHTML, /aria-label="Strict node configuration JSON"/);
  assert.match(nodeHTML, /class="flow-row-controls"/);
  assert.match(nodeHTML, /title="Move node up"/);
  assert.match(nodeHTML, /title="Remove node"/);

  const edgeHTML = context.renderEdgeRowView({ from: "plan", outcome: "done", to: "verify" }, ["plan", "verify"]);
  assert.match(edgeHTML, /data-edge-row/);
  assert.match(edgeHTML, /aria-label="From node"/);
  assert.match(edgeHTML, /aria-label="Target node"/);
  assert.match(edgeHTML, /title="Remove transition"/);
});


function fakeFieldForm(fields) {
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (match && match[1] in fields) return { value: fields[match[1]] };
      return null;
    },
    querySelectorAll() {
      return [];
    },
  };
}

function fakeFlowRow(fields) {
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (!match) return null;
      const name = match[1];
      if (!(name in fields)) return null;
      if (name === "review_agent_blocking") {
        return typeof fields[name] === "object" ? fields[name] : { checked: fields[name] };
      }
      return { value: fields[name] };
    },
    querySelectorAll(selector) {
      if (selector === "[data-review-agent-row]") return (fields.review_agents || []).map(fakeFlowRow);
      return [];
    },
  };
}

function fakeFlowEditor(spec) {
  const top = {
    flow_name: spec.flow_name,
    flow_description: spec.flow_description,
    start_node: spec.start_node ?? "",
    transition_budget: spec.transition_budget ?? "50",
  };
  const nodeCards = (spec.nodes || []).map(fakeFlowRow);
  const edgeRows = (spec.edges || []).map(fakeFlowRow);
  return {
    querySelector(selector) {
      const match = selector.match(/^\[name="([^"]+)"\]$/);
      if (match && match[1] in top) return { value: top[match[1]] };
      return null;
    },
    querySelectorAll(selector) {
      if (selector === "[data-node-card]") return nodeCards;
      if (selector === "[data-edge-row]") return edgeRows;
      return [];
    },
  };
}

test("agent def form payload stores plain harness target id and effort strings", async () => {
  const context = await scriptContext();
  const agentOptions = [{
    name: "harness",
    display_name: "Harness",
    models: [{
      provider_id: "anthropic",
      provider_name: "Anthropic",
      model_id: "claude-opus-4-8",
      qualified_id: "anthropic:claude-opus-4-8",
      target_id: "anthropic:claude-opus-4-8",
      model_name: "Claude Opus 4.8",
      reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
    }],
  }];
  const form = fakeFieldForm({
    def_name: "Reviewer",
    def_harness: "harness",
    def_model: "anthropic:claude-opus-4-8",
    def_reasoning_effort: "high",
    def_prompt: "review carefully",
  });

  const payload = context.agentDefPayloadFromFormView(form, agentOptions);

  assert.deepEqual(payload, {
    name: "Reviewer",
    harness: "harness",
    model: "anthropic:claude-opus-4-8",
    reasoning_effort: "high",
    prompt: "review carefully",
  });
});

test("agent def form payload stores the bare model id when the catalog model has no target id", async () => {
  const context = await scriptContext();
  const agentOptions = [{
    name: "harness",
    display_name: "Harness",
    models: [{ provider_id: "anthropic", model_id: "sonnet", qualified_id: "anthropic:sonnet", reasoning: false }],
  }];
  const form = fakeFieldForm({
    def_name: "Author",
    def_harness: "harness",
    def_model: "anthropic:sonnet",
    def_reasoning_effort: "",
    def_prompt: "",
  });

  const payload = context.agentDefPayloadFromFormView(form, agentOptions);

  assert.equal(payload.model, "anthropic:sonnet");
  assert.equal(payload.reasoning_effort, "");
});

test("flow editor payload keeps node and edge rows in document order", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Custom",
    flow_description: "two nodes",
    start_node: "plan",
    transition_budget: "50",
    nodes: [
      { node_key: "plan", node_name: "Plan", node_kind: "agent", node_config: '{"agent_def_id":"ad-plan"}' },
      { node_key: "verify", node_name: "Verify", node_kind: "automated_checks", node_config: "{}" },
    ],
    edges: [
      { edge_from: "plan", edge_outcome: "done", edge_to: "verify" },
      { edge_from: "verify", edge_outcome: "pass", edge_to: "plan" },
    ],
  });

  const payload = context.flowPayloadFromEditorView(form);

  assert.deepEqual(payload, {
    name: "Custom",
    description: "two nodes",
    start_node: "plan",
    transition_budget: 50,
    nodes: [
      { key: "plan", name: "Plan", kind: "agent", config: { agent_def_id: "ad-plan" } },
      { key: "verify", name: "Verify", kind: "automated_checks", config: {} },
    ],
    edges: [
      { from: "plan", outcome: "done", to: "verify" },
      { from: "verify", outcome: "pass", to: "plan" },
    ],
  });
});

test("flow editor payload reads each node and edge row as authored", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Sparse",
    flow_description: "",
    start_node: "",
    transition_budget: "50",
    nodes: [
      { node_key: "plan", node_name: "Plan", node_kind: "agent", node_config: "{}" },
      { node_key: "", node_name: "", node_kind: "agent", node_config: "{}" },
    ],
    edges: [
      { edge_from: "plan", edge_outcome: "done", edge_to: "" },
    ],
  });

  const payload = context.flowPayloadFromEditorView(form);

  // Rows are submitted as authored; the editor does not drop blank rows.
  assert.deepEqual(payload.nodes, [
    { key: "plan", name: "Plan", kind: "agent", config: {} },
    { key: "", name: "", kind: "agent", config: {} },
  ]);
  assert.deepEqual(payload.edges, [{ from: "plan", outcome: "done", to: "" }]);
});

test("parallel review payload preserves agent order and emits canonical blocking only", async () => {
  const context = await scriptContext();
  const form = fakeFlowEditor({
    flow_name: "Parallel review",
    flow_description: "",
    start_node: "review",
    nodes: [
      {
        node_key: "review",
        node_name: "Review",
        node_kind: "change_review",
        review_agents: [
          { review_agent_def_id: "ad-code", review_agent_blocking: true },
          { review_agent_def_id: "ad-security", review_agent_blocking: false },
        ],
        review_aggregator_agent_def_id: "ad-aggregator",
      },
      {
        node_key: "verify",
        node_name: "Verify",
        node_kind: "verify_change",
        review_agents: [
          { review_agent_def_id: "ad-verifier", review_agent_blocking: true },
        ],
      },
    ],
    edges: [],
  });

  const payload = context.flowPayloadFromEditorView(form);

  assert.deepEqual(payload.nodes, [
    {
      key: "review",
      name: "Review",
      kind: "change_review",
      config: {
        change_review: {
          agents: [
            { agent_def_id: "ad-code", blocking: true },
            { agent_def_id: "ad-security", blocking: false },
          ],
          aggregator_agent_def_id: "ad-aggregator",
        },
      },
    },
    {
      key: "verify",
      name: "Verify",
      kind: "verify_change",
      config: {
        verify_change: {
          agents: [{ agent_def_id: "ad-verifier", blocking: true }],
        },
      },
    },
  ]);
  assert.doesNotMatch(JSON.stringify(payload), /"required"/);
});

test("parallel review blocking checkbox toggles an agent to advisory", async () => {
  const context = await scriptContext();
  const blocking = { checked: true };
  const form = fakeFlowEditor({
    flow_name: "Advisory review",
    start_node: "review",
    nodes: [{
      node_key: "review",
      node_name: "Review",
      node_kind: "change_review",
      review_agents: [{ review_agent_def_id: "ad-security", review_agent_blocking: blocking }],
      review_aggregator_agent_def_id: "ad-aggregator",
    }],
  });

  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, true);
  blocking.checked = false;
  assert.equal(context.flowPayloadFromEditorView(form).nodes[0].config.change_review.agents[0].blocking, false);
});

test("cloneFlowView builds a create payload that copies the graph under a new name", async () => {
  const context = await scriptContext();

  const payload = context.cloneFlowView({
    id: "fl-1",
    name: "coding",
    description: "Ship it",
    start_node: "implement",
    transition_budget: 75,
    builtin: true,
    default: true,
    nodes: [
      { id: "fn-1", key: "implement", name: "Implement", kind: "agent", position: 0, config: { agent: { agent_def_id: "ad-author", workspace: "change", artifact: "change" } } },
      { id: "fn-2", key: "review", name: "Review", kind: "change_review", position: 1, config: { change_review: { agents: [{ agent_def_id: "ad-reviewer", blocking: true }], aggregator_agent_def_id: "ad-aggregator" } } },
    ],
    edges: [{ from: "implement", outcome: "done", to: "review" }],
  });

  assert.equal(payload.name, "coding (copy)");
  // The initial copy name is reused when it is available, and an incremented
  // deterministic suffix is chosen when the project already has the copy.
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "coding (copy)"]).name, "coding (copy 2)");
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "coding (copy)", "coding (copy 2)"]).name, "coding (copy 3)");
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "other"]).name, "coding (copy)");
  // Existing-name matching is exact/case-sensitive, mirroring the server's
  // flows.name UNIQUE collation (SQLite default BINARY, no NOCASE): a
  // case-variant name does not occupy the copy slot.
  assert.equal(context.cloneFlowName("coding", ["CODING (copy)"]), "coding (copy)");
  assert.equal(context.cloneFlowName("coding", ["coding", "Coding (copy)"]), "coding (copy)");
  assert.equal(context.cloneFlowName("coding", ["coding (copy)"]), "coding (copy 2)");
  assert.equal(context.cloneFlowView({ name: "coding" }, ["coding", "CODING (copy)"]).name, "coding (copy)");
  assert.equal(payload.description, "Ship it");
  assert.equal(payload.start_node, "implement");
  assert.equal(payload.transition_budget, 75);
  // Server-assigned ids/positions and the builtin/default flags are dropped so
  // the copy is a fresh, editable flow.
  assert.doesNotMatch(JSON.stringify(payload), /"id"|"position"|builtin|default/);
  assert.deepEqual(payload.nodes, [
    { key: "implement", name: "Implement", kind: "agent", config: { agent: { agent_def_id: "ad-author", workspace: "change", artifact: "change" } } },
    { key: "review", name: "Review", kind: "change_review", config: { change_review: { agents: [{ agent_def_id: "ad-reviewer", blocking: true }], aggregator_agent_def_id: "ad-aggregator" } } },
  ]);
  assert.deepEqual(payload.edges, [{ from: "implement", outcome: "done", to: "review" }]);
});


test("flows view renders agent definitions and flow tables for the active project", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }] },
    "/ui/api/v2/harnesses": { agents: [{ name: "harness", display_name: "Harness" }], consoles: [] },
    "/ui/api/v2/global/agent-defs": { agent_defs: [{ id: "ad-global", name: "organization-reviewer", harness: "harness" }] },
    "/ui/api/v2/projects/p-alpha/agent-defs": {
      agent_defs: [{ id: "ad-1", name: "author", harness: "harness", model: "anthropic:opus", reasoning_effort: "high", builtin: true }],
    },
    "/ui/api/v2/projects/p-alpha/flows": {
      flows: [{
        id: "fl-1",
        name: "default flow",
        default: true,
        start_node: "plan",
        nodes: [{ key: "plan", name: "Plan", kind: "agent" }, { key: "implement", name: "Implement", kind: "agent" }],
        edges: [{ from: "plan", outcome: "done", to: "implement" }],
      }],
      default_flow_id: "fl-1",
    },
  });

  await harness.app.load();

  const html = harness.content.firstElementChild.innerHTML;
  assert.equal(harness.title.textContent, "Flows");
  assert.match(html, /Global Agent Definitions/);
  assert.match(html, /organization-reviewer/);
  assert.match(html, /Project Agent Definitions/);
  assert.match(html, /author/);
  assert.match(html, /builtin/);
  assert.equal((html.match(/data-add-def/g) || []).length, 2);
  assert.doesNotMatch(html, /data-agent-def-form/);
  assert.match(html, /default flow/);
  assert.match(html, /start: plan · plan\.done → implement/);
  assert.match(html, /class="flows-table"/);
  assert.match(html, /<th class="flow-name-column">Name<\/th><th class="flow-graph-column">Graph<\/th>/);
  assert.match(html, /<td class="flow-name-column">default flow/);
  assert.match(html, /<td class="flow-graph-column"><div class="workflow-chart compact">/);
  assert.match(html, /class="workflow-chart compact"/);
  assert.match(html, /<svg[^>]*aria-label="default flow workflow definition"/);
  assert.match(html, /data-node="implement"/);
  assert.match(html, /data-flow-editor/);
  // Every flow row offers a Clone action to seed a custom copy.
  assert.match(html, /data-clone-flow="fl-1">Clone</);
  // Keeps the project's flow cache warm for the task form.
  assert.deepEqual(harness.app.flowsByProject.get("p-alpha").defaultFlowID, "fl-1");
});

test("flows view offers a project chooser when several projects are active", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
  });
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  assert.match(harness.content.firstElementChild.innerHTML, /Select Project/);
  assert.equal((harness.content.firstElementChild.innerHTML.match(/class="project-choice"/g) || []).length, 2);
  assert.match(harness.content.firstElementChild.innerHTML, /\/ui\/flows\?project=p-alpha/);
  assert.match(harness.content.firstElementChild.innerHTML, /\/ui\/flows\?project=p-beta/);
  assert.doesNotMatch(harness.content.firstElementChild.innerHTML, /<span>p-alpha<\/span>/);
  assert.doesNotMatch(harness.content.firstElementChild.innerHTML, /<span>p-beta<\/span>/);
});

test("flows route refreshes a stale project registry before choosing a project", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
  });
  harness.app.projects = [{ id: "p-alpha", name: "alpha" }];
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  assert.match(harness.content.firstElementChild.innerHTML, /Select Project/);
  assert.equal((harness.content.firstElementChild.innerHTML.match(/class="project-choice"/g) || []).length, 2);
  assert.deepEqual(harness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/harnesses",
  ]);
});

test("flows route refreshes a harness catalog cached before workers became ready", async () => {
  const harness = await browserSmokeHarness("/ui/flows", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }] },
    "/ui/api/v2/harnesses": {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{ target_id: "openai:gpt-5", display_name: "GPT-5", provider_label: "OpenAI", model_label: "gpt-5", reasoning: true }],
      }],
      consoles: [],
    },
    "/ui/api/v2/global/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-alpha/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-alpha/flows": { flows: [], default_flow_id: "" },
  });
  harness.app.harnesses = {
    agents: [{ name: "harness", display_name: "Harness", models: [] }],
    consoles: [],
  };

  await harness.app.load();

  assert.equal(harness.app.harnesses.agents[0].models[0].qualified_id, "openai:gpt-5");
  assert.equal(harness.fetchCalls.filter((path) => path === "/ui/api/v2/harnesses").length, 1);
});

test("flows view renders the active project name as a project switcher", async () => {
  const harness = await browserSmokeHarness("/ui/flows?project=p-beta", {
    "/ui/api/v2/projects": { projects: [{ id: "p-alpha", name: "alpha" }, { id: "p-beta", name: "beta" }] },
    "/ui/api/v2/harnesses": { agents: [], consoles: [] },
    "/ui/api/v2/global/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-beta/agent-defs": { agent_defs: [] },
    "/ui/api/v2/projects/p-beta/flows": { flows: [], default_flow_id: "" },
  });
  harness.app.renderProjectPicker = () => {};

  await harness.app.load();

  const html = harness.content.firstElementChild.innerHTML;
  assert.match(html, /class="project-switcher"/);
  assert.match(html, /<summary aria-label="Switch project">beta<\/summary>/);
  assert.match(html, /\/ui\/flows\?project=p-alpha/);
  assert.match(html, /\/ui\/flows\?project=p-beta/);
  assert.match(html, /aria-current="page"/);
  assert.deepEqual(harness.fetchCalls, [
    "/ui/api/v2/projects",
    "/ui/api/v2/harnesses",
    "/ui/api/v2/global/agent-defs",
    "/ui/api/v2/projects/p-beta/agent-defs",
    "/ui/api/v2/projects/p-beta/flows",
  ]);
});

// --- change route: metadata/diff head coherence ---------------------------------

let changeRouteModulePromise;
// change-route.js (like app.js) extends HTMLElement at module scope, so it can
// only be imported once the test context has installed the global stubs.
