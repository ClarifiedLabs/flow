// Flow editor view functions: the node cards, edge rows, review-agent rows,
// the config editors, the payload readers, the live selector/graph refresh,
// and the clone payload builders. Pure (string in, markup/payload out, plus
// the form-subtree surgeries the flow editor's controls perform); the
// <flow-flows> element and the flows-route share them.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { renderWorkflowGraph } from "../workflow-graph.js";

export function agentDefOptionsHTML(agentDefs, selectedID, options = {}) {
  const selected = String(selectedID || "").trim();
  const hasSelected = (agentDefs || []).some((def) => String(value(def, "id", "ID")) === selected);
  const empty = options.includeEmpty
    ? `<option value="" ${selected ? "" : "selected"}>${escapeHTML(options.emptyLabel || "")}</option>`
    : "";
  const unavailable = selected && !hasSelected
    ? `<option value="${escapeAttr(selected)}" selected>${escapeHTML(`${selected} (unavailable)`)}</option>`
    : "";
  return empty + unavailable + (agentDefs || []).map((def) => {
    const id = value(def, "id", "ID");
    const name = value(def, "name", "Name") || id;
    const harness = String(value(def, "harness", "Harness") || "").trim();
    const model = String(value(def, "model", "Model") || "").trim();
    const runtime = [harness, model].filter(Boolean).join(" / ");
    const label = runtime ? `${name} — ${runtime}` : name;
    return `<option value="${escapeAttr(id)}" ${id === selected ? "selected" : ""}>${escapeHTML(label)}</option>`;
  }).join("");
}

const NODE_KINDS = ["agent", "automated_checks", "change_review", "human_gate", "verify_change", "materialize_task_set", "merge_change", "finalize_rebase", "terminal"];
const REVIEW_NODE_CONFIG_KEYS = {
  change_review: "change_review",
  verify_change: "verify_change",
};

export function reviewNodeConfigKeyView(kind) {
  return REVIEW_NODE_CONFIG_KEYS[kind] || "";
}

function reviewConfigFromNodeView(node, kind) {
  const config = value(node, "config", "Config") || {};
  const configKey = reviewNodeConfigKeyView(kind);
  const pascalKey = configKey === "change_review" ? "ChangeReview" : "VerifyChange";
  return value(config, configKey, pascalKey) || {};
}

function reviewAgentsFromNodeView(node, kind) {
  const agents = value(reviewConfigFromNodeView(node, kind), "agents", "Agents");
  return Array.isArray(agents) ? agents : [];
}

function reviewAggregatorAgentDefIDFromNodeView(node, kind) {
  if (kind !== "change_review") return "";
  return value(reviewConfigFromNodeView(node, kind), "aggregator_agent_def_id", "AggregatorAgentDefID") || "";
}

// Review-agent configuration uses the canonical blocking field. Omission is
// the graph default (blocking); noncanonical aliases are deliberately ignored.
export function reviewAgentBlockingView(agent = {}) {
  if (agent.blocking !== undefined && agent.blocking !== null) return Boolean(agent.blocking);
  return true;
}

export function renderReviewAgentRowView(agent = {}, agentDefs = [], configKey = "change_review") {
  const agentDefID = value(agent, "agent_def_id", "AgentDefID");
  const blocking = reviewAgentBlockingView(agent);
  const blockingLabel = configKey === "verify_change" ? "Blocks success" : "Blocks approval";
  return `
    <div class="flow-row flow-review-agent-row" data-review-agent-row>
      <select name="review_agent_def_id" aria-label="Agent definition" required>
        ${agentDefOptionsHTML(agentDefs, agentDefID, { includeEmpty: true, emptyLabel: "Select agent definition" })}
      </select>
      <label><input type="checkbox" name="review_agent_blocking" ${blocking ? "checked" : ""}> ${blockingLabel}</label>
      <span class="muted" data-review-agent-advisory ${blocking ? "hidden" : ""}>Advisory</span>
      <div class="flow-row-controls">
        <button type="button" class="button secondary icon-button" data-review-agent-up title="Move agent up">&uarr;</button>
        <button type="button" class="button secondary icon-button" data-review-agent-down title="Move agent down">&darr;</button>
        <button type="button" class="button secondary icon-button" data-review-agent-remove title="Remove agent">&times;</button>
      </div>
    </div>
  `;
}

function nodeConfigValue(node) {
  const config = value(node, "config", "Config") || {};
  return JSON.stringify(config, null, 2);
}

export function renderNodeConfigEditorView(node = {}, agentDefs = []) {
  const kind = value(node, "kind", "Kind") || "agent";
  const configKey = reviewNodeConfigKeyView(kind);
  if (!configKey) {
    return `<textarea name="node_config" rows="6" spellcheck="false" aria-label="Strict node configuration JSON">${escapeHTML(nodeConfigValue(node))}</textarea>`;
  }
  const agents = reviewAgentsFromNodeView(node, kind);
  const policy = configKey === "verify_change"
    ? "Every listed agent runs and is awaited. Blocks success controls whether that agent's findings can veto the node."
    : "Reviewers run in parallel and are awaited. The selected final review aggregator then validates and synthesizes their reports. Blocks approval controls whether a reviewer's candidates may become aggregate blockers.";
  const aggregator = configKey === "change_review"
    ? `<label class="flow-review-aggregator-field"><span>Final review aggregator</span><select name="review_aggregator_agent_def_id" aria-label="Final review aggregator" required>${agentDefOptionsHTML(agentDefs, reviewAggregatorAgentDefIDFromNodeView(node, kind), { includeEmpty: true, emptyLabel: "Select aggregator agent definition" })}</select></label>`
    : "";
  return `
    <div class="flow-review-agent-config" data-review-agent-config data-review-config-key="${configKey}">
      <p class="muted">${policy}</p>
      ${aggregator}
      <div class="flow-row-list" data-review-agent-rows>${agents.map((agent) => renderReviewAgentRowView(agent, agentDefs, configKey)).join("")}</div>
      <div class="flow-row-actions"><button type="button" class="button secondary" data-add-review-agent>Add agent</button></div>
    </div>
  `;
}

export function renderNodeCardView(node = {}, agentDefs = []) {
  const key = value(node, "key", "Key");
  const name = value(node, "name", "Name");
  const kind = value(node, "kind", "Kind") || "agent";
  return `
    <article class="flow-row flow-node-card" data-node-card>
      <input name="node_key" placeholder="stable-node-key" value="${escapeAttr(key)}" aria-label="Node key" required>
      <input name="node_name" placeholder="Short display name (e.g. Implement)" value="${escapeAttr(name)}" aria-label="Node name" required>
      <select name="node_kind" aria-label="Trusted node kind">${NODE_KINDS.map((candidate) => `<option value="${candidate}" ${candidate === kind ? "selected" : ""}>${candidate}</option>`).join("")}</select>
      <div data-node-config-editor>${renderNodeConfigEditorView(node, agentDefs)}</div>
      <div class="flow-row-controls">
        <button type="button" class="button secondary icon-button" data-node-up title="Move node up">&uarr;</button>
        <button type="button" class="button secondary icon-button" data-node-down title="Move node down">&darr;</button>
        <button type="button" class="button secondary icon-button" data-node-remove title="Remove node">&times;</button>
      </div>
    </article>
  `;
}

export function renderEdgeRowView(edge = {}, nodeKeys = []) {
  const from = value(edge, "from", "From");
  const outcome = value(edge, "outcome", "Outcome");
  const to = value(edge, "to", "To");
  const options = (selected) => `<option value="">Select node</option>${nodeKeys.map((key) => `<option value="${escapeAttr(key)}" ${key === selected ? "selected" : ""}>${escapeHTML(key)}</option>`).join("")}`;
  return `<div class="flow-row" data-edge-row>
    <select name="edge_from" aria-label="From node">${options(from)}</select>
    <input name="edge_outcome" placeholder="outcome" value="${escapeAttr(outcome)}" required>
    <select name="edge_to" aria-label="Target node">${options(to)}</select>
    <button type="button" class="button secondary icon-button" data-edge-remove title="Remove transition">&times;</button>
  </div>`;
}

export function renderFlowEditorView(flow, agentDefs) {
  const flowID = value(flow, "id", "ID");
  const name = value(flow, "name", "Name");
  const description = value(flow, "description", "Description");
  const startNode = value(flow, "start_node", "StartNode");
  const transitionBudget = Number(value(flow, "transition_budget", "TransitionBudget") || 50);
  const nodes = value(flow, "nodes", "Nodes") || [];
  const edges = value(flow, "edges", "Edges") || [];
  const nodeKeys = nodes.map((node) => value(node, "key", "Key")).filter(Boolean);
  return `
    <form class="flow-editor task-form" data-flow-editor data-flow-id="${escapeAttr(flowID)}">
      <label><span>Name</span><input name="flow_name" value="${escapeAttr(name)}" required></label>
      <label><span>Description</span><textarea name="flow_description" rows="2">${escapeHTML(description)}</textarea></label>
      <label><span>Start node</span><select name="start_node">${nodeKeys.map((key) => `<option value="${escapeAttr(key)}" ${key === startNode ? "selected" : ""}>${escapeHTML(key)}</option>`).join("")}</select></label>
      <label><span>Transition budget</span><input name="transition_budget" type="number" min="1" max="500" value="${transitionBudget}"></label>
      <div class="flow-rows-head wide">Trusted node cards</div>
      <div class="flow-row-list wide" data-node-cards>${nodes.map((node) => renderNodeCardView(node, agentDefs)).join("")}</div>
      <div class="flow-row-actions wide"><button type="button" class="button secondary" data-add-node>Add node</button></div>
      <div class="flow-rows-head wide">Outcome transitions</div>
      <div class="flow-row-list wide" data-edge-rows>${edges.map((edge) => renderEdgeRowView(edge, nodeKeys)).join("")}</div>
      <div class="flow-row-actions wide"><button type="button" class="button secondary" data-add-edge>Add transition</button></div>
      <div class="wide"><span>Read-only graph preview</span><div class="workflow-chart flow-graph-preview" data-graph-preview>${renderWorkflowGraph(flow, { ariaLabel: `${name || "New flow"} workflow definition` })}</div></div>
      <div class="form-actions">
        <button class="button" type="submit">${flowID ? "Save flow" : "Create flow"}</button>
        ${flowID ? `<button class="button secondary" type="button" data-flow-cancel>Cancel</button>` : ""}
      </div>
    </form>
  `;
}

export function flowPayloadFromEditorView(form) {
  const readValue = (root, selector) => {
    const element = root.querySelector(selector);
    return element ? String(element.value ?? "") : "";
  };
  const nodes = Array.from(form.querySelectorAll("[data-node-card]")).map((card) => {
    const kind = readValue(card, '[name="node_kind"]');
    const reviewConfigKey = reviewNodeConfigKeyView(kind);
    let config;
    if (reviewConfigKey) {
      const agents = Array.from(card.querySelectorAll("[data-review-agent-row]")).map((row) => {
        const blocking = row.querySelector('[name="review_agent_blocking"]');
        return {
          agent_def_id: readValue(row, '[name="review_agent_def_id"]'),
          blocking: blocking ? Boolean(blocking.checked) : true,
        };
      });
      const reviewConfig = { agents };
      if (reviewConfigKey === "change_review") {
        reviewConfig.aggregator_agent_def_id = readValue(card, '[name="review_aggregator_agent_def_id"]');
      }
      config = { [reviewConfigKey]: reviewConfig };
    } else {
      config = JSON.parse(readValue(card, '[name="node_config"]') || "{}");
    }
    return {
      key: readValue(card, '[name="node_key"]').trim(),
      name: readValue(card, '[name="node_name"]').trim(),
      kind,
      config,
    };
  });
  const edges = Array.from(form.querySelectorAll("[data-edge-row]")).map((row) => ({
    from: readValue(row, '[name="edge_from"]'),
    outcome: readValue(row, '[name="edge_outcome"]').trim(),
    to: readValue(row, '[name="edge_to"]'),
  }));
  return {
    name: readValue(form, '[name="flow_name"]').trim(),
    description: readValue(form, '[name="flow_description"]').trim(),
    start_node: readValue(form, '[name="start_node"]'),
    transition_budget: Number(readValue(form, '[name="transition_budget"]') || 50),
    nodes,
    edges,
  };
}

// cloneFlowName picks an available deterministic copy name. The initial copy
// keeps the classic "<name> (copy)" suffix; when that name is already taken,
// an incremented suffix ("<name> (copy 2)", "<name> (copy 3)", ...) is chosen
// so repeated clones of the same source do not collide with the flow-name
// uniqueness rule. Existing-name matching is exact (case-sensitive), mirroring
// the server's flows.name UNIQUE collation (SQLite default BINARY, no NOCASE),
// so a case-variant like "Coding (copy)" does not occupy "coding (copy)".
export function cloneFlowName(name, existingNames = []) {
  const base = name || "flow";
  const taken = new Set(existingNames);
  if (!taken.has(`${base} (copy)`)) return `${base} (copy)`;
  for (let index = 2; ; index++) {
    const candidate = `${base} (copy ${index})`;
    if (!taken.has(candidate)) return candidate;
  }
}

// cloneFlowView builds a create-flow payload from an existing flow so it can
// be saved as a new, editable copy: a starting point for a custom version.
// Node ids/positions and the builtin/default flags are dropped (the server
// assigns fresh ids; a new flow is never builtin or default), and the name gets
// a copy suffix that the author can rename before saving. When existingNames
// (the active project's flow names) already contains the initial copy name, an
// available incremented suffix is used instead.
export function cloneFlowView(flow, existingNames = []) {
  const name = value(flow, "name", "Name") || "flow";
  const nodes = (value(flow, "nodes", "Nodes") || []).map((node) => ({
    key: value(node, "key", "Key"),
    name: value(node, "name", "Name"),
    kind: value(node, "kind", "Kind"),
    config: value(node, "config", "Config") || {},
  }));
  const edges = (value(flow, "edges", "Edges") || []).map((edge) => ({
    from: value(edge, "from", "From"),
    outcome: value(edge, "outcome", "Outcome"),
    to: value(edge, "to", "To"),
  }));
  return {
    name: cloneFlowName(name, existingNames),
    description: value(flow, "description", "Description") || "",
    start_node: value(flow, "start_node", "StartNode") || "",
    transition_budget: Number(value(flow, "transition_budget", "TransitionBudget") || 0) || undefined,
    nodes,
    edges,
  };
}

export function graphNodeKeysView(form) {
  return Array.from(form.querySelectorAll('[name="node_key"]')).map((input) => String(input.value || "").trim()).filter(Boolean);
}

export function refreshGraphSelectorsView(form) {
  const keys = graphNodeKeysView(form);
  for (const select of form.querySelectorAll('[name="start_node"], [name="edge_from"], [name="edge_to"]')) {
    const selected = select.value;
    select.innerHTML = `${select.name === "start_node" ? "" : '<option value="">Select node</option>'}${keys.map((key) => `<option value="${escapeAttr(key)}" ${key === selected ? "selected" : ""}>${escapeHTML(key)}</option>`).join("")}`;
  }
  const preview = form.querySelector("[data-graph-preview]");
  if (preview) {
    const graph = {
      start_node: form.querySelector('[name="start_node"]')?.value || "",
      nodes: Array.from(form.querySelectorAll("[data-node-card]")).map((card) => ({
        key: card.querySelector('[name="node_key"]')?.value || "",
        name: card.querySelector('[name="node_name"]')?.value || "",
        kind: card.querySelector('[name="node_kind"]')?.value || "",
      })),
      edges: Array.from(form.querySelectorAll("[data-edge-row]")).map((row) => ({
        from: row.querySelector('[name="edge_from"]')?.value || "",
        outcome: row.querySelector('[name="edge_outcome"]')?.value || "",
        to: row.querySelector('[name="edge_to"]')?.value || "",
      })),
    };
    preview.innerHTML = renderWorkflowGraph(graph, { ariaLabel: "Workflow definition preview" });
  }
}

// moveRowView reorders a flow editor row within its container by swapping it
// with the adjacent sibling in the given direction (-1 up, +1 down).
export function moveRowView(row, direction) {
  if (!row || !row.parentNode) return;
  const sibling = direction < 0 ? row.previousElementSibling : row.nextElementSibling;
  if (!sibling) return;
  if (direction < 0) {
    row.parentNode.insertBefore(row, sibling);
  } else {
    row.parentNode.insertBefore(sibling, row);
  }
}

