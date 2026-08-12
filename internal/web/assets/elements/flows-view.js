// Flows page view functions: the project chooser/switcher, the agent-def
// catalogs (global and project) with their edit rows and model/reasoning
// cells, and the flows table section. Pure markup/payload functions; the
// <flow-flows> element drives them and the flows-route reads the resolver.

import { HARNESS_REASONING_UNAVAILABLE, findHarnessModel, harnessModels, harnessReasoningLevelValues } from "../models/harness-catalog.js";
import { renderHarnessModelOptions, renderHarnessOptions } from "../models/harness-form.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { renderWorkflowGraph } from "../workflow-graph.js";
import { agentDefOptionsHTML, renderFlowEditorView } from "./flow-editor-view.js";

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

export function renderFlowsChooserMarkup(projects) {
  if (!projects.length) return `<div class="empty">No projects</div>`;
  return `
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

export function flowsProjectOptionsView(app) {
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

export function renderAgentDefModelSelectView(models, model) {
  return `<select name="def_model" data-def-model aria-label="Model">${renderHarnessModelOptions(models, model ? model.qualified_id : "")}</select>`;
}

export function renderAgentDefReasoningSelectView(model, selectedEffort) {
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
                <button class="button secondary" type="button" data-clone-flow="${escapeAttr(id)}"${state.cloneInFlight ? " disabled" : ""}>Clone</button>
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

export function renderFlowGraphSummaryView(flow) {
  const start = value(flow, "start_node", "StartNode");
  const edges = value(flow, "edges", "Edges") || [];
  if (!start) return "—";
  const transitions = edges.slice(0, 3).map((edge) => `${value(edge, "from", "From")}.${value(edge, "outcome", "Outcome")} → ${value(edge, "to", "To")}`);
  return escapeHTML([`start: ${start}`, ...transitions, edges.length > 3 ? `+${edges.length - 3} more` : ""].filter(Boolean).join(" · "));
}

