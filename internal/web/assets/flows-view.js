// Flows settings view: manage global/project agent definitions and project
// flows. Agent definitions pick a harness + model/effort + prompt; flows are
// editable graphs of trusted node kinds with explicit outcome transitions.

import { agentDefsAPIBase, apiDelete, apiGet, apiPatch, apiPost, flowsAPIBase, globalAgentDefsAPIBase } from "./api.js";
import { DEFAULT_AGENT_HARNESSES } from "./config.js";
import { HARNESS_REASONING_UNAVAILABLE, findHarnessModel, harnessModels, harnessReasoningLevelValues, renderHarnessModelOptions, renderHarnessOptions } from "./harness-models.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { renderWorkflowGraph } from "./workflow-graph.js";

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

  const [globalDefsData, defsData, flowsData] = await Promise.all([
    apiGet(globalAgentDefsAPIBase()),
    apiGet(agentDefsAPIBase(project.id)),
    apiGet(flowsAPIBase(project.id)),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  const globalAgentDefs = globalDefsData.agent_defs || globalDefsData.AgentDefs || [];
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
      ${renderGlobalAgentDefsSectionView(globalAgentDefs, agentOptions, state)}
      ${renderAgentDefsSectionView(agentDefs, agentOptions, state)}
      ${renderFlowsSectionView(flows, agentDefs, defaultFlowID, state)}
    </section>
  `;
  bindGlobalAgentDefsSectionView(app, globalAgentDefs, agentOptions, state);
  bindAgentDefsSectionView(app, project, agentDefs, agentOptions, state);
  bindFlowsSectionView(app, project, flows, agentDefs, state);
  return true;
}

export function flowsViewState(app) {
  if (!app.flowsView) app.flowsView = { editingDefID: "", editingGlobalDefID: "", editingFlowID: "" };
  if (app.flowsView.editingGlobalDefID === undefined) app.flowsView.editingGlobalDefID = "";
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

export const NEW_AGENT_DEF_STATE = "__new_agent_def__";

export function renderAgentDefsSectionView(agentDefs, agentOptions, state) {
  return renderAgentDefsCatalogView(agentDefs, agentOptions, state, {
    editingKey: "editingDefID",
    sectionAttribute: "data-agent-defs-section",
    title: "Project Agent Definitions",
    description: "Inherited definitions are available to this project's flows. Editing one creates a project override with the same name.",
  });
}

export function renderGlobalAgentDefsSectionView(agentDefs, agentOptions, state) {
  return renderAgentDefsCatalogView(agentDefs, agentOptions, state, {
    editingKey: "editingGlobalDefID",
    sectionAttribute: "data-global-agent-defs-section",
    title: "Global Agent Definitions",
    description: "Every project inherits these definitions unless it has a same-name project override.",
  });
}

function renderAgentDefsCatalogView(agentDefs, agentOptions, state, options) {
  const editingID = state[options.editingKey] || "";
  const creating = editingID === NEW_AGENT_DEF_STATE;
  const editingDef = creating
    ? null
    : (agentDefs || []).find((def) => value(def, "id", "ID") === editingID) || null;
  const hasEditor = creating || Boolean(editingDef);
  const rows = (agentDefs || []).map((def) => {
    if (def === editingDef) return renderAgentDefEditRowsView(def, agentOptions);
    return renderAgentDefReadRowView(def);
  }).join("");
  const table = `
    <div class="table-wrap">
      <table class="agent-def-table">
        <thead><tr><th>Name</th><th>Harness</th><th>Model</th><th>Effort</th><th class="agent-def-actions-column"></th></tr></thead>
        <tbody>${rows}${creating ? renderAgentDefEditRowsView(null, agentOptions) : renderAgentDefAddRowView()}</tbody>
      </table>
    </div>`;
  return `
    <section class="flows-section" ${options.sectionAttribute}>
      <h3>${escapeHTML(options.title)}</h3>
      <p class="meta">${escapeHTML(options.description)}</p>
      ${hasEditor
        ? `<form class="agent-def-table-form" data-agent-def-form data-def-id="${escapeAttr(value(editingDef, "id", "ID"))}">${table}</form>`
        : table}
    </section>
  `;
}

function renderAgentDefReadRowView(def) {
  const id = value(def, "id", "ID");
  const builtin = Boolean(value(def, "builtin", "Builtin"));
  const inherited = Boolean(value(def, "inherited", "Inherited"));
  return `
    <tr>
      <td>${escapeHTML(value(def, "name", "Name"))}${renderAgentDefBadgesView(def)}</td>
      <td>${escapeHTML(value(def, "harness", "Harness"))}</td>
      <td>${escapeHTML(value(def, "model", "Model") || "default")}</td>
      <td>${escapeHTML(value(def, "reasoning_effort", "ReasoningEffort") || "—")}</td>
      <td>
        <div class="actions table-actions">
          <button class="button secondary" type="button" data-edit-def="${escapeAttr(id)}">${inherited ? "Override" : "Edit"}</button>
          ${builtin || inherited ? "" : `<button class="button secondary" type="button" data-delete-def="${escapeAttr(id)}">Delete</button>`}
        </div>
      </td>
    </tr>`;
}

export function renderAgentDefEditRowsView(def, agentOptions) {
  const name = value(def, "name", "Name");
  const inherited = Boolean(value(def, "inherited", "Inherited"));
  const harness = def ? value(def, "harness", "Harness") : (agentOptions[0] ? value(agentOptions[0], "name", "Name") : "harness");
  const prompt = value(def, "prompt", "Prompt");
  const models = harnessModels(agentOptions, harness);
  const selectedQID = resolveAgentDefModelQID(models, harness, value(def, "model", "Model"));
  const selectedEffort = value(def, "reasoning_effort", "ReasoningEffort");
  return `
    <tr class="agent-def-edit-row" data-agent-def-edit-row>
      <td>
        <div class="agent-def-name-editor">
          <input name="def_name" value="${escapeAttr(name)}" aria-label="Name"${inherited ? " readonly" : ""} required>
          ${renderAgentDefBadgesView(def)}
        </div>
      </td>
      <td><select name="def_harness" data-def-harness aria-label="Harness">${renderHarnessOptions(agentOptions, harness, true)}</select></td>
      ${renderAgentDefModelFieldsView(agentOptions, harness, selectedQID, selectedEffort)}
      <td>
        <div class="actions table-actions">
          <button class="button" type="submit">Save</button>
          <button class="button secondary" type="button" data-def-cancel>Cancel</button>
        </div>
      </td>
    </tr>
    <tr class="agent-def-prompt-row" data-agent-def-prompt-row>
      <td colspan="5">
        <label class="agent-def-prompt-field"><span>Prompt</span><textarea name="def_prompt" rows="5">${escapeHTML(prompt)}</textarea></label>
      </td>
    </tr>`;
}

function renderAgentDefBadgesView(def) {
  const builtin = Boolean(value(def, "builtin", "Builtin"));
  const inherited = Boolean(value(def, "inherited", "Inherited"));
  return `${builtin ? ` <span class="badge idle">builtin</span>` : ""}${inherited ? ` <span class="badge idle">inherited</span>` : ""}`;
}

function renderAgentDefAddRowView() {
  return `
    <tr class="agent-def-add-row">
      <td colspan="4"></td>
      <td><button class="button secondary icon-button" type="button" data-add-def title="Add agent definition" aria-label="Add agent definition">+</button></td>
    </tr>`;
}

export function renderAgentDefModelFieldsView(agentOptions, harness, selectedQID, selectedEffort) {
  const models = harnessModels(agentOptions, harness);
  const model = findHarnessModel(models, selectedQID);
  return `
    <td data-def-model-cell>${renderAgentDefModelSelectView(models, model)}</td>
    <td data-def-reasoning-cell>${renderAgentDefReasoningSelectView(model, selectedEffort)}</td>
  `;
}

function renderAgentDefModelSelectView(models, model) {
  return `<select name="def_model" data-def-model aria-label="Model">${renderHarnessModelOptions(models, model ? model.qualified_id : "")}</select>`;
}

function renderAgentDefReasoningSelectView(model, selectedEffort) {
  return `<select name="def_reasoning_effort" data-def-reasoning aria-label="Effort">${renderReasoningOptionsView(model, selectedEffort)}</select>`;
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
// the target id (== qualified id).
export function resolveAgentDefModelQID(models, harness, defModel) {
  const raw = String(defModel || "").trim();
  if (!raw) return "";
  const byQID = (models || []).find((model) => model.qualified_id === raw || model.target_id === raw);
  if (byQID) return byQID.qualified_id;
  const byModelID = (models || []).filter((model) => model.model_id === raw);
  return byModelID.length === 1 ? byModelID[0].qualified_id : "";
}

// agentDefPayloadFromFormView reads the agent-def form into the API body. The
// picked catalog model is stored as the plain model target id the harness
// expects, NOT serialized args.
export function agentDefPayloadFromFormView(form, agentOptions) {
  const readValue = (selector) => {
    const element = form.querySelector(selector);
    return element ? String(element.value ?? "") : "";
  };
  const harness = readValue('[name="def_harness"]').trim();
  const models = harnessModels(agentOptions, harness);
  const model = findHarnessModel(models, readValue('[name="def_model"]').trim());
  const modelValue = model ? (model.target_id || model.qualified_id || model.model_id) : "";
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
  bindAgentDefsCatalogView(app, agentOptions, state, {
    apiBase: agentDefsAPIBase(project.id),
    editingKey: "editingDefID",
    selector: "[data-agent-defs-section]",
  });
}

export function bindGlobalAgentDefsSectionView(app, agentDefs, agentOptions, state) {
  bindAgentDefsCatalogView(app, agentOptions, state, {
    apiBase: globalAgentDefsAPIBase(),
    editingKey: "editingGlobalDefID",
    selector: "[data-global-agent-defs-section]",
  });
}

function bindAgentDefsCatalogView(app, agentOptions, state, options) {
  const section = app.querySelector(options.selector);
  if (!section) return;
  const reload = () => app.load();

  section.querySelectorAll("[data-edit-def]").forEach((button) => {
    button.addEventListener("click", () => {
      state[options.editingKey] = button.dataset.editDef || "";
      reload();
    });
  });
  section.querySelectorAll("[data-add-def]").forEach((button) => {
    button.addEventListener("click", () => {
      state[options.editingKey] = NEW_AGENT_DEF_STATE;
      reload();
    });
  });
  section.querySelectorAll("[data-delete-def]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (typeof window.confirm === "function" && !window.confirm("Delete this agent definition?")) return;
      try {
        await apiDelete(`${options.apiBase}/${encodeURIComponent(button.dataset.deleteDef)}`);
        state[options.editingKey] = "";
        await reload();
      } catch (error) {
        app.setStatus(error.message || String(error));
      }
    });
  });

  const form = section.querySelector("[data-agent-def-form]");
  if (!form) return;
  const harnessSelect = form.querySelector("[data-def-harness]");
  const modelCell = form.querySelector("[data-def-model-cell]");
  const reasoningCell = form.querySelector("[data-def-reasoning-cell]");
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
  if (harnessSelect && typeof harnessSelect.addEventListener === "function" && modelCell && reasoningCell) {
    harnessSelect.addEventListener("change", () => {
      const models = harnessModels(agentOptions, harnessSelect.value);
      modelCell.innerHTML = renderAgentDefModelSelectView(models, null);
      reasoningCell.innerHTML = renderAgentDefReasoningSelectView(null, "");
      bindModelChange();
    });
  }
  bindModelChange();

  const cancel = form.querySelector("[data-def-cancel]");
  if (cancel && typeof cancel.addEventListener === "function") {
    cancel.addEventListener("click", () => {
      state[options.editingKey] = "";
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
        await apiPatch(`${options.apiBase}/${encodeURIComponent(defID)}`, payload);
      } else {
        await apiPost(options.apiBase, payload);
      }
      state[options.editingKey] = "";
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
            <td class="flow-name-column">${escapeHTML(value(flow, "name", "Name"))}${isDefault ? ` <span class="badge ok">default</span>` : ""}${builtin ? ` <span class="badge idle">builtin</span>` : ""}</td>
            <td class="flow-graph-column"><div class="workflow-chart compact">${renderWorkflowGraph(flow, { ariaLabel: `${value(flow, "name", "Name") || id} workflow definition` })}</div><p class="flow-graph-summary">${renderFlowGraphSummaryView(flow)}</p></td>
            <td class="flow-nodes-column">${nodes.length}</td>
            <td class="flow-actions-column">
              <div class="actions table-actions">
                <button class="button secondary" type="button" data-edit-flow="${escapeAttr(id)}">Edit</button>
                <button class="button secondary" type="button" data-clone-flow="${escapeAttr(id)}">Clone</button>
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
        <table class="flows-table">
          <thead><tr><th class="flow-name-column">Name</th><th class="flow-graph-column">Graph</th><th class="flow-nodes-column">Nodes</th><th class="flow-actions-column"></th></tr></thead>
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

const NODE_KINDS = ["agent", "automated_checks", "change_review", "human_gate", "verify_change", "materialize_task_set", "merge_change", "finalize_rebase", "terminal"];
const REVIEW_NODE_CONFIG_KEYS = {
  change_review: "change_review",
  verify_change: "verify_change",
};

function reviewNodeConfigKeyView(kind) {
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

// cloneFlowView builds a create-flow payload from an existing flow so it can
// be saved as a new, editable copy: a starting point for a custom version.
// Node ids/positions and the builtin/default flags are dropped (the server
// assigns fresh ids; a new flow is never builtin or default), and the name gets
// a copy suffix that the author can rename before saving.
export function cloneFlowView(flow) {
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
    name: name + " (copy)",
    description: value(flow, "description", "Description") || "",
    start_node: value(flow, "start_node", "StartNode") || "",
    transition_budget: Number(value(flow, "transition_budget", "TransitionBudget") || 0) || undefined,
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
  section.querySelectorAll("[data-clone-flow]").forEach((button) => {
    button.addEventListener("click", async () => {
      const source = (flows || []).find((flow) => value(flow, "id", "ID") === button.dataset.cloneFlow);
      if (!source) return;
      try {
        const response = await apiPost(flowsAPIBase(project.id), cloneFlowView(source));
        const created = value(response, "flow", "Flow") || response || {};
        state.editingFlowID = value(created, "id", "ID") || "";
        await reload();
        app.setStatus("flow cloned; rename and edit your copy");
      } catch (error) {
        app.setStatus(error.message || String(error));
      }
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
        refreshGraphSelectorsView(form);
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
        refreshGraphSelectorsView(form);
      } else if (target.closest("[data-node-down]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-node-card]"), 1);
        refreshGraphSelectorsView(form);
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
    const reviewConfig = { agents: [{}] };
    if (configKey === "change_review") reviewConfig.aggregator_agent_def_id = "";
    const config = configKey ? { [configKey]: reviewConfig } : {};
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
