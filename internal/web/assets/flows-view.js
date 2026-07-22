// Flows settings view: manage a project's agent definitions and flows. Agent
// definitions pick a harness + model/effort + prompt; flows are editable
// graphs of trusted node kinds with explicit outcome transitions. Both are project-owned rows edited live here,
// resolving the active project the same way console-view does.

import { agentDefsAPIBase, apiDelete, apiGet, apiPatch, apiPost, flowsAPIBase } from "./api.js";
import { DEFAULT_AGENT_HARNESSES } from "./config.js";
import { HARNESS_REASONING_UNAVAILABLE, findHarnessModel, harnessModels, harnessReasoningLevelValues, renderHarnessModelOptions, renderHarnessOptions } from "./harness-models.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export async function renderFlowsView(app, context) {
  app.setTitle("Flows");
  await app.ensureProjects();
  await app.ensureHarnesses();
  if (context && !app.isActiveLoad(context)) return false;
  const params = new URLSearchParams(window.location.search);
  const project = resolveFlowsProjectView(app, params.get("project") || "");
  if (!project) {
    renderFlowsProjectChooserView(app);
    return true;
  }

  const [defsData, flowsData] = await Promise.all([
    apiGet(agentDefsAPIBase(project.id)),
    apiGet(flowsAPIBase(project.id)),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  const agentDefs = defsData.agent_defs || defsData.AgentDefs || [];
  const flows = flowsData.flows || flowsData.Flows || [];
  const defaultFlowID = flowsData.default_flow_id || flowsData.DefaultFlowID || "";
  // Keep this project's flow cache warm so the task form renders its Flow
  // selector without an extra round trip.
  if (!app.flowsByProject) app.flowsByProject = new Map();
  app.flowsByProject.set(project.id, { flows, defaultFlowID });

  const state = flowsViewState(app);
  const agentOptions = (app.harnesses && app.harnesses.agents) || DEFAULT_AGENT_HARNESSES;

  app.querySelector(".content").innerHTML = `
    <section class="detail flows-detail">
      <div class="detail-head">
        <div>
          ${renderFlowsProjectSwitchView(app, project)}
          <h2>Flows</h2>
        </div>
      </div>
      ${renderAgentDefsSectionView(agentDefs, agentOptions, state)}
      ${renderFlowsSectionView(flows, agentDefs, defaultFlowID, state)}
    </section>
  `;
  bindAgentDefsSectionView(app, project, agentDefs, agentOptions, state);
  bindFlowsSectionView(app, project, flows, agentDefs, state);
  return true;
}

export function flowsViewState(app) {
  if (!app.flowsView) app.flowsView = { editingDefID: "", editingFlowID: "" };
  return app.flowsView;
}

export function resolveFlowsProjectView(app, selectedProject) {
  const projects = app.projects || [];
  if (selectedProject) {
    const match = projects.find((project) => value(project, "id", "ID") === selectedProject);
    return { id: selectedProject, name: match ? value(match, "name", "Name") || selectedProject : selectedProject };
  }
  const selected = app.selectedProjectIDs();
  if (selected.length === 1) {
    const match = projects.find((project) => value(project, "id", "ID") === selected[0]);
    return { id: selected[0], name: match ? value(match, "name", "Name") || selected[0] : selected[0] };
  }
  if (projects.length === 1) {
    return { id: value(projects[0], "id", "ID"), name: value(projects[0], "name", "Name") || value(projects[0], "id", "ID") };
  }
  return null;
}

export function renderFlowsProjectChooserView(app) {
  const projects = flowsProjectOptionsView(app);
  if (!projects.length) {
    app.querySelector(".content").innerHTML = `<div class="empty">No projects</div>`;
    return;
  }
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>Select Project</h2>
          <p class="meta">Choose a project to manage its flows.</p>
        </div>
      </div>
      <div class="project-choice-list">${projects.map((project) => `
        <a class="project-choice" href="/ui/flows?project=${encodeURIComponent(project.id)}" data-link>
          <span>${escapeHTML(project.name)}</span>
        </a>
      `).join("")}</div>
    </section>
  `;
}

export function renderFlowsProjectSwitchView(app, project) {
  const currentName = project.name || project.id;
  const projects = flowsProjectOptionsView(app);
  if (projects.length <= 1) {
    return `<p class="eyebrow">${escapeHTML(currentName)}</p>`;
  }
  return `
    <details class="project-switcher">
      <summary aria-label="Switch project">${escapeHTML(currentName)}</summary>
      <div class="project-switcher-menu">
        ${projects.map((option) => `
          <a class="project-switcher-item" href="/ui/flows?project=${encodeURIComponent(option.id)}" data-link${option.id === project.id ? ` aria-current="page"` : ""}>
            ${escapeHTML(option.name)}
          </a>
        `).join("")}
      </div>
    </details>
  `;
}

function flowsProjectOptionsView(app) {
  return (app.projects || []).map((project) => {
    const id = value(project, "id", "ID");
    return {
      id,
      name: value(project, "name", "Name") || id,
    };
  }).filter((project) => project.id);
}

// --- Agent definitions ---------------------------------------------------------

export function renderAgentDefsSectionView(agentDefs, agentOptions, state) {
  const editingDef = (agentDefs || []).find((def) => value(def, "id", "ID") === state.editingDefID) || null;
  const rows = (agentDefs || []).length
    ? agentDefs.map((def) => {
        const id = value(def, "id", "ID");
        const builtin = Boolean(value(def, "builtin", "Builtin"));
        return `
          <tr>
            <td>${escapeHTML(value(def, "name", "Name"))}${builtin ? ` <span class="badge idle">builtin</span>` : ""}</td>
            <td>${escapeHTML(value(def, "harness", "Harness"))}</td>
            <td>${escapeHTML(value(def, "model", "Model") || "default")}</td>
            <td>${escapeHTML(value(def, "reasoning_effort", "ReasoningEffort") || "—")}</td>
            <td>
              <div class="actions table-actions">
                <button class="button secondary" type="button" data-edit-def="${escapeAttr(id)}">Edit</button>
                ${builtin ? "" : `<button class="button secondary" type="button" data-delete-def="${escapeAttr(id)}">Delete</button>`}
              </div>
            </td>
          </tr>`;
      }).join("")
    : `<tr><td colspan="5">No agent definitions</td></tr>`;
  return `
    <section class="flows-section" data-agent-defs-section>
      <h3>Agent Definitions</h3>
      <div class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Harness</th><th>Model</th><th>Effort</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      ${renderAgentDefFormView(editingDef, agentOptions)}
    </section>
  `;
}

export function renderAgentDefFormView(def, agentOptions) {
  const defID = value(def, "id", "ID");
  const name = value(def, "name", "Name");
  const harness = def ? value(def, "harness", "Harness") : (agentOptions[0] ? agentOptions[0].name : "codex");
  const prompt = value(def, "prompt", "Prompt");
  const models = harnessModels(agentOptions, harness);
  const selectedQID = resolveAgentDefModelQID(models, harness, value(def, "model", "Model"));
  const selectedEffort = value(def, "reasoning_effort", "ReasoningEffort");
  return `
    <form class="agent-def-form task-form" data-agent-def-form data-def-id="${escapeAttr(defID)}">
      <label><span>Name</span><input name="def_name" value="${escapeAttr(name)}" required></label>
      <label><span>Harness</span>
        <select name="def_harness" data-def-harness>${renderHarnessOptions(agentOptions, harness, true)}</select>
      </label>
      <div class="agent-def-model-fields" data-def-model-fields>
        ${renderAgentDefModelFieldsView(agentOptions, harness, selectedQID, selectedEffort)}
      </div>
      <label class="wide"><span>Prompt</span><textarea name="def_prompt" rows="4">${escapeHTML(prompt)}</textarea></label>
      <div class="form-actions">
        <button class="button" type="submit">${defID ? "Save agent" : "Create agent"}</button>
        ${defID ? `<button class="button secondary" type="button" data-def-cancel>Cancel</button>` : ""}
      </div>
    </form>
  `;
}

export function renderAgentDefModelFieldsView(agentOptions, harness, selectedQID, selectedEffort) {
  const models = harnessModels(agentOptions, harness);
  const model = findHarnessModel(models, selectedQID);
  return `
    <label><span>Model</span>
      <select name="def_model" data-def-model>${renderHarnessModelOptions(models, model ? model.qualified_id : "")}</select>
    </label>
    <label><span>Reasoning</span>
      <select name="def_reasoning_effort" data-def-reasoning>${renderReasoningOptionsView(model, selectedEffort)}</select>
    </label>
  `;
}

export function renderReasoningOptionsView(model, selectedEffort) {
  const values = harnessReasoningLevelValues(model);
  const selected = String(selectedEffort || "").trim();
  const known = values.includes(selected);
  const unavailable = selected && !known
    ? `<option value="${HARNESS_REASONING_UNAVAILABLE}" selected>${HARNESS_REASONING_UNAVAILABLE}</option>`
    : "";
  return `${unavailable}<option value="" ${selected && (known || !unavailable) ? "" : "selected"}>Default</option>${values.map((v) => `<option value="${escapeAttr(v)}" ${v === selected ? "selected" : ""}>${escapeHTML(v)}</option>`).join("")}`;
}

// resolveAgentDefModelQID maps a stored agent-def model string back onto a
// catalog model's qualified id so the picker can preselect it. Harness stores
// the target id (== qualified id); codex/claude store the bare model id.
export function resolveAgentDefModelQID(models, harness, defModel) {
  const raw = String(defModel || "").trim();
  if (!raw) return "";
  const byQID = (models || []).find((model) => model.qualified_id === raw || model.target_id === raw);
  if (byQID) return byQID.qualified_id;
  const byModelID = (models || []).filter((model) => model.model_id === raw);
  return byModelID.length === 1 ? byModelID[0].qualified_id : "";
}

// agentDefPayloadFromFormView reads the agent-def form into the API body. The
// picked catalog model is stored as the plain model string the harness expects
// (target id for harness, bare model id for codex/claude), NOT serialized args.
export function agentDefPayloadFromFormView(form, agentOptions) {
  const readValue = (selector) => {
    const element = form.querySelector(selector);
    return element ? String(element.value ?? "") : "";
  };
  const harness = readValue('[name="def_harness"]').trim();
  const models = harnessModels(agentOptions, harness);
  const model = findHarnessModel(models, readValue('[name="def_model"]').trim());
  const modelValue = model
    ? (harness === "harness" ? (model.target_id || model.qualified_id || model.model_id) : model.model_id)
    : "";
  const effort = readValue('[name="def_reasoning_effort"]').trim();
  return {
    name: readValue('[name="def_name"]').trim(),
    harness,
    model: modelValue,
    reasoning_effort: effort && effort !== HARNESS_REASONING_UNAVAILABLE ? effort : "",
    prompt: readValue('[name="def_prompt"]'),
  };
}

export function bindAgentDefsSectionView(app, project, agentDefs, agentOptions, state) {
  const section = app.querySelector("[data-agent-defs-section]");
  if (!section) return;
  const reload = () => app.load();

  section.querySelectorAll("[data-edit-def]").forEach((button) => {
    button.addEventListener("click", () => {
      state.editingDefID = button.dataset.editDef || "";
      reload();
    });
  });
  section.querySelectorAll("[data-delete-def]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (typeof window.confirm === "function" && !window.confirm("Delete this agent definition?")) return;
      try {
        await apiDelete(`${agentDefsAPIBase(project.id)}/${encodeURIComponent(button.dataset.deleteDef)}`);
        state.editingDefID = "";
        await reload();
      } catch (error) {
        app.setStatus(error.message || String(error));
      }
    });
  });

  const form = section.querySelector("[data-agent-def-form]");
  if (!form) return;
  const harnessSelect = form.querySelector("[data-def-harness]");
  const modelFields = form.querySelector("[data-def-model-fields]");
  const bindModelChange = () => {
    const modelSelect = form.querySelector("[data-def-model]");
    if (modelSelect && typeof modelSelect.addEventListener === "function") {
      modelSelect.addEventListener("change", () => {
        const models = harnessModels(agentOptions, harnessSelect ? harnessSelect.value : "");
        const model = findHarnessModel(models, modelSelect.value);
        const reasoning = form.querySelector("[data-def-reasoning]");
        if (reasoning) reasoning.innerHTML = renderReasoningOptionsView(model, "");
      });
    }
  };
  if (harnessSelect && typeof harnessSelect.addEventListener === "function" && modelFields) {
    harnessSelect.addEventListener("change", () => {
      modelFields.innerHTML = renderAgentDefModelFieldsView(agentOptions, harnessSelect.value, "", "");
      bindModelChange();
    });
  }
  bindModelChange();

  const cancel = form.querySelector("[data-def-cancel]");
  if (cancel && typeof cancel.addEventListener === "function") {
    cancel.addEventListener("click", () => {
      state.editingDefID = "";
      reload();
    });
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (form.reportValidity && !form.reportValidity()) return;
    const payload = agentDefPayloadFromFormView(form, agentOptions);
    if (!payload.name) {
      app.setStatus("Agent definition name is required");
      return;
    }
    const defID = form.dataset.defId || "";
    try {
      if (defID) {
        await apiPatch(`${agentDefsAPIBase(project.id)}/${encodeURIComponent(defID)}`, payload);
      } else {
        await apiPost(agentDefsAPIBase(project.id), payload);
      }
      state.editingDefID = "";
      await reload();
      app.setStatus(defID ? "agent definition saved" : "agent definition created");
    } catch (error) {
      app.setStatus(error.message || String(error));
    }
  });
}

// --- Flows ---------------------------------------------------------------------

export function renderFlowsSectionView(flows, agentDefs, defaultFlowID, state) {
  const editingFlow = (flows || []).find((flow) => value(flow, "id", "ID") === state.editingFlowID) || null;
  const rows = (flows || []).length
    ? flows.map((flow) => {
        const id = value(flow, "id", "ID");
        const isDefault = id === defaultFlowID || Boolean(value(flow, "default", "Default"));
        const builtin = Boolean(value(flow, "builtin", "Builtin"));
        const nodes = value(flow, "nodes", "Nodes") || [];
        return `
          <tr>
            <td>${escapeHTML(value(flow, "name", "Name"))}${isDefault ? ` <span class="badge ok">default</span>` : ""}${builtin ? ` <span class="badge idle">builtin</span>` : ""}</td>
            <td>${renderFlowGraphSummaryView(flow)}</td>
            <td>${nodes.length}</td>
            <td>
              <div class="actions table-actions">
                <button class="button secondary" type="button" data-edit-flow="${escapeAttr(id)}">Edit</button>
                ${isDefault ? "" : `<button class="button secondary" type="button" data-default-flow="${escapeAttr(id)}">Set default</button>`}
                ${isDefault ? "" : `<button class="button secondary" type="button" data-delete-flow="${escapeAttr(id)}">Delete</button>`}
              </div>
            </td>
          </tr>`;
      }).join("")
    : `<tr><td colspan="4">No flows</td></tr>`;
  return `
    <section class="flows-section" data-flows-section>
      <h3>Flows</h3>
      <div class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Graph</th><th>Nodes</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      ${renderFlowEditorView(editingFlow, agentDefs)}
    </section>
  `;
}

export function renderFlowPhaseChainView(phases) {
  return (phases || []).map((phase) => {
    const name = value(phase, "name", "Name");
    const gate = value(phase, "gate", "Gate") === "human" ? "(gate)" : "";
    return `${escapeHTML(name)}${gate}`;
  }).join(" -> ");
}

export function renderFlowGraphSummaryView(flow) {
  const start = value(flow, "start_node", "StartNode");
  const edges = value(flow, "edges", "Edges") || [];
  if (!start) return "—";
  const transitions = edges.slice(0, 3).map((edge) => `${value(edge, "from", "From")}.${value(edge, "outcome", "Outcome")} → ${value(edge, "to", "To")}`);
  return escapeHTML([`start: ${start}`, ...transitions, edges.length > 3 ? `+${edges.length - 3} more` : ""].filter(Boolean).join(" · "));
}

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

const NODE_KINDS = ["agent", "automated_checks", "change_review", "human_gate", "verify_change", "materialize_task_set", "merge_change", "terminal"];
const REVIEW_NODE_CONFIG_KEYS = {
  change_review: "change_review",
  verify_change: "verify_change",
};

function reviewNodeConfigKeyView(kind) {
  return REVIEW_NODE_CONFIG_KEYS[kind] || "";
}

function reviewAgentsFromNodeView(node, kind) {
  const config = value(node, "config", "Config") || {};
  const configKey = reviewNodeConfigKeyView(kind);
  const pascalKey = configKey === "change_review" ? "ChangeReview" : "VerifyChange";
  const reviewConfig = value(config, configKey, pascalKey) || {};
  const agents = value(reviewConfig, "agents", "Agents");
  return Array.isArray(agents) ? agents : [];
}

// Saved legacy entries may still contain required. Treat it only as an input
// alias; structured editor payloads always use the canonical blocking field.
export function reviewAgentBlockingView(agent = {}) {
  if (agent.blocking !== undefined && agent.blocking !== null) return Boolean(agent.blocking);
  if (agent.Blocking !== undefined && agent.Blocking !== null) return Boolean(agent.Blocking);
  if (agent.required !== undefined && agent.required !== null) return Boolean(agent.required);
  if (agent.Required !== undefined && agent.Required !== null) return Boolean(agent.Required);
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
  return `
    <div class="flow-review-agent-config" data-review-agent-config data-review-config-key="${configKey}">
      <p class="muted">Every listed agent runs and is awaited. ${configKey === "verify_change" ? "Blocks success" : "Blocks approval"} only controls whether that agent's findings can veto the node.</p>
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
      <div class="wide"><span>Read-only graph preview</span><pre class="flow-graph-preview" data-graph-preview>${escapeHTML(renderFlowGraphSummaryView(flow))}</pre></div>
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
      config = { [reviewConfigKey]: { agents } };
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

export function bindFlowsSectionView(app, project, flows, agentDefs, state) {
  const section = app.querySelector("[data-flows-section]");
  if (!section) return;
  const reload = () => app.load();

  section.querySelectorAll("[data-edit-flow]").forEach((button) => {
    button.addEventListener("click", () => {
      state.editingFlowID = button.dataset.editFlow || "";
      reload();
    });
  });
  section.querySelectorAll("[data-default-flow]").forEach((button) => {
    button.addEventListener("click", async () => {
      try {
        await apiPost(`${flowsAPIBase(project.id)}/${encodeURIComponent(button.dataset.defaultFlow)}/default`, {});
        await reload();
        app.setStatus("default flow updated");
      } catch (error) {
        app.setStatus(error.message || String(error));
      }
    });
  });
  section.querySelectorAll("[data-delete-flow]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (typeof window.confirm === "function" && !window.confirm("Delete this flow?")) return;
      try {
        await apiDelete(`${flowsAPIBase(project.id)}/${encodeURIComponent(button.dataset.deleteFlow)}`);
        state.editingFlowID = "";
        await reload();
      } catch (error) {
        app.setStatus(error.message || String(error));
      }
    });
  });

  const form = section.querySelector("[data-flow-editor]");
  if (!form) return;

  if (typeof form.addEventListener === "function") {
    form.addEventListener("click", (event) => {
      const target = event.target;
      if (!target || typeof target.closest !== "function") return;
      if (target.closest("[data-add-node]")) {
        event.preventDefault();
        form.querySelector("[data-node-cards]")?.insertAdjacentHTML("beforeend", renderNodeCardView({}, agentDefs));
        refreshGraphSelectorsView(form);
      } else if (target.closest("[data-add-review-agent]")) {
        event.preventDefault();
        const config = target.closest("[data-review-agent-config]");
        config?.querySelector("[data-review-agent-rows]")?.insertAdjacentHTML("beforeend", renderReviewAgentRowView({}, agentDefs, config.dataset?.reviewConfigKey));
      } else if (target.closest("[data-add-edge]")) {
        event.preventDefault();
        form.querySelector("[data-edge-rows]")?.insertAdjacentHTML("beforeend", renderEdgeRowView({}, graphNodeKeysView(form)));
      } else if (target.closest("[data-node-remove]")) {
        event.preventDefault();
        target.closest("[data-node-card]")?.remove();
        refreshGraphSelectorsView(form);
      } else if (target.closest("[data-edge-remove]")) {
        event.preventDefault();
        target.closest("[data-edge-row]")?.remove();
      } else if (target.closest("[data-review-agent-remove]")) {
        event.preventDefault();
        target.closest("[data-review-agent-row]")?.remove();
      } else if (target.closest("[data-review-agent-up]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-review-agent-row]"), -1);
      } else if (target.closest("[data-review-agent-down]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-review-agent-row]"), 1);
      } else if (target.closest("[data-node-up]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-node-card]"), -1);
      } else if (target.closest("[data-node-down]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-node-card]"), 1);
      } else if (target.closest("[data-flow-cancel]")) {
        event.preventDefault();
        state.editingFlowID = "";
        reload();
      }
    });
  }
  form.addEventListener("change", (event) => {
    const target = event.target;
    if (!target) return;
    if (target.name === "review_agent_blocking" && typeof target.closest === "function") {
      const marker = target.closest("[data-review-agent-row]")?.querySelector("[data-review-agent-advisory]");
      if (marker) marker.hidden = Boolean(target.checked);
      return;
    }
    if (target.name !== "node_kind" || typeof target.closest !== "function") return;
    const card = target.closest("[data-node-card]");
    const editor = card?.querySelector("[data-node-config-editor]");
    if (!editor) return;
    const configKey = reviewNodeConfigKeyView(target.value);
    const config = configKey ? { [configKey]: { agents: [{}] } } : {};
    editor.innerHTML = renderNodeConfigEditorView({ kind: target.value, config }, agentDefs);
  });
  form.addEventListener("input", () => refreshGraphSelectorsView(form));

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (form.reportValidity && !form.reportValidity()) return;
    let payload;
    try {
      payload = flowPayloadFromEditorView(form);
    } catch (error) {
      app.setStatus(`Invalid node configuration JSON: ${error.message || error}`);
      return;
    }
    if (!payload.name) {
      app.setStatus("Flow name is required");
      return;
    }
    const flowID = form.dataset.flowId || "";
    try {
      if (flowID) {
        await apiPatch(`${flowsAPIBase(project.id)}/${encodeURIComponent(flowID)}`, payload);
      } else {
        await apiPost(flowsAPIBase(project.id), payload);
      }
      state.editingFlowID = "";
      await reload();
      app.setStatus(flowID ? "flow saved" : "flow created");
    } catch (error) {
      app.setStatus(error.message || String(error));
    }
  });
}

function graphNodeKeysView(form) {
  return Array.from(form.querySelectorAll('[name="node_key"]')).map((input) => String(input.value || "").trim()).filter(Boolean);
}

function refreshGraphSelectorsView(form) {
  const keys = graphNodeKeysView(form);
  for (const select of form.querySelectorAll('[name="start_node"], [name="edge_from"], [name="edge_to"]')) {
    const selected = select.value;
    select.innerHTML = `${select.name === "start_node" ? "" : '<option value="">Select node</option>'}${keys.map((key) => `<option value="${escapeAttr(key)}" ${key === selected ? "selected" : ""}>${escapeHTML(key)}</option>`).join("")}`;
  }
  const preview = form.querySelector("[data-graph-preview]");
  if (preview) {
    const edges = Array.from(form.querySelectorAll("[data-edge-row]")).map((row) => `${row.querySelector('[name="edge_from"]')?.value || "?"}.${row.querySelector('[name="edge_outcome"]')?.value || "?"} → ${row.querySelector('[name="edge_to"]')?.value || "?"}`);
    preview.textContent = [`start: ${form.querySelector('[name="start_node"]')?.value || "?"}`, ...edges].join("\n");
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
