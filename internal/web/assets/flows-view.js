// Flows settings view: manage a project's agent definitions and flows. Agent
// definitions pick a harness + model/effort + prompt; flows chain ordered work
// phases (each an agent def with an auto/human gate) plus a review-agent set
// and an optional fix agent. Both are project-owned rows edited live here,
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
  // Keep this project's flow cache warm so the issue form renders its Flow
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
    <form class="agent-def-form issue-form" data-agent-def-form data-def-id="${escapeAttr(defID)}">
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
        const reviewCount = (value(flow, "review_agents", "ReviewAgents") || []).length;
        return `
          <tr>
            <td>${escapeHTML(value(flow, "name", "Name"))}${isDefault ? ` <span class="badge ok">default</span>` : ""}${builtin ? ` <span class="badge idle">builtin</span>` : ""}</td>
            <td>${renderFlowPhaseChainView(value(flow, "phases", "Phases") || [])}</td>
            <td>${reviewCount}</td>
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
          <thead><tr><th>Name</th><th>Phases</th><th>Reviews</th><th></th></tr></thead>
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

export function agentDefOptionsHTML(agentDefs, selectedID, options = {}) {
  const selected = String(selectedID || "").trim();
  const empty = options.includeEmpty
    ? `<option value="" ${selected ? "" : "selected"}>${escapeHTML(options.emptyLabel || "")}</option>`
    : "";
  return empty + (agentDefs || []).map((def) => {
    const id = value(def, "id", "ID");
    const name = value(def, "name", "Name") || id;
    return `<option value="${escapeAttr(id)}" ${id === selected ? "selected" : ""}>${escapeHTML(name)}</option>`;
  }).join("");
}

export function renderPhaseRowView(agentDefs, phase = {}) {
  const name = value(phase, "name", "Name");
  const agentDefID = value(phase, "agent_def_id", "AgentDefID");
  const gate = value(phase, "gate", "Gate") === "human" ? "human" : "auto";
  return `
    <div class="flow-row" data-phase-row>
      <input name="phase_name" placeholder="Phase name" value="${escapeAttr(name)}" aria-label="Phase name">
      <select name="phase_agent_def_id" aria-label="Phase agent">${agentDefOptionsHTML(agentDefs, agentDefID, { includeEmpty: true, emptyLabel: "Select agent" })}</select>
      <select name="phase_gate" aria-label="Phase gate">
        <option value="auto" ${gate === "auto" ? "selected" : ""}>auto</option>
        <option value="human" ${gate === "human" ? "selected" : ""}>human</option>
      </select>
      <div class="flow-row-controls">
        <button type="button" class="button secondary icon-button" data-phase-row-up title="Move phase up" aria-label="Move phase up">&uarr;</button>
        <button type="button" class="button secondary icon-button" data-phase-row-down title="Move phase down" aria-label="Move phase down">&darr;</button>
        <button type="button" class="button secondary icon-button" data-phase-row-remove title="Remove phase" aria-label="Remove phase">&times;</button>
      </div>
    </div>
  `;
}

export function renderReviewRowView(agentDefs, review = {}) {
  const role = value(review, "role", "Role") === "verifier" ? "verifier" : "reviewer";
  const agentDefID = value(review, "agent_def_id", "AgentDefID");
  const required = Boolean(value(review, "required", "Required"));
  return `
    <div class="flow-row" data-review-row>
      <select name="review_role" aria-label="Review role">
        <option value="reviewer" ${role === "reviewer" ? "selected" : ""}>reviewer</option>
        <option value="verifier" ${role === "verifier" ? "selected" : ""}>verifier</option>
      </select>
      <select name="review_agent_def_id" aria-label="Review agent">${agentDefOptionsHTML(agentDefs, agentDefID, { includeEmpty: true, emptyLabel: "Select agent" })}</select>
      <label class="check"><input type="checkbox" name="review_required" ${required ? "checked" : ""}><span>required</span></label>
      <div class="flow-row-controls">
        <button type="button" class="button secondary icon-button" data-review-row-remove title="Remove review agent" aria-label="Remove review agent">&times;</button>
      </div>
    </div>
  `;
}

export function renderFlowEditorView(flow, agentDefs) {
  const flowID = value(flow, "id", "ID");
  const name = value(flow, "name", "Name");
  const description = value(flow, "description", "Description");
  const fixAgentDefID = value(flow, "fix_agent_def_id", "FixAgentDefID");
  const phases = value(flow, "phases", "Phases") || [];
  const reviewAgents = value(flow, "review_agents", "ReviewAgents") || [];
  const phaseRows = (phases.length ? phases : [{}]).map((phase) => renderPhaseRowView(agentDefs, phase)).join("");
  const reviewRows = reviewAgents.map((review) => renderReviewRowView(agentDefs, review)).join("");
  return `
    <form class="flow-editor issue-form" data-flow-editor data-flow-id="${escapeAttr(flowID)}">
      <label><span>Name</span><input name="flow_name" value="${escapeAttr(name)}" required></label>
      <label><span>Description</span><textarea name="flow_description" rows="2">${escapeHTML(description)}</textarea></label>
      <div class="flow-rows-head wide">Phases</div>
      <div class="flow-row-list wide" data-phase-rows>${phaseRows}</div>
      <div class="flow-row-actions wide"><button type="button" class="button secondary" data-add-phase>Add phase</button></div>
      <div class="flow-rows-head wide">Review Agents</div>
      <div class="flow-row-list wide" data-review-rows>${reviewRows}</div>
      <div class="flow-row-actions wide"><button type="button" class="button secondary" data-add-review>Add review agent</button></div>
      <label class="wide"><span>Fix Agent</span>
        <select name="fix_agent_def_id">${agentDefOptionsHTML(agentDefs, fixAgentDefID, { includeEmpty: true, emptyLabel: "(last phase's agent)" })}</select>
      </label>
      <div class="form-actions">
        <button class="button" type="submit">${flowID ? "Save flow" : "Create flow"}</button>
        ${flowID ? `<button class="button secondary" type="button" data-flow-cancel>Cancel</button>` : ""}
      </div>
    </form>
  `;
}

// flowPayloadFromEditorView reads the flow editor into the API body, preserving
// phase/review row order (querySelectorAll is document order). Blank rows (no
// name and no agent) are dropped.
export function flowPayloadFromEditorView(form) {
  const readValue = (root, selector) => {
    const element = root.querySelector(selector);
    return element ? String(element.value ?? "") : "";
  };
  const readChecked = (root, selector) => {
    const element = root.querySelector(selector);
    return element ? Boolean(element.checked) : false;
  };
  const phases = Array.from(form.querySelectorAll("[data-phase-row]"))
    .map((row) => ({
      name: readValue(row, '[name="phase_name"]').trim(),
      agent_def_id: readValue(row, '[name="phase_agent_def_id"]'),
      gate: readValue(row, '[name="phase_gate"]') === "human" ? "human" : "auto",
    }))
    .filter((phase) => phase.name || phase.agent_def_id);
  const review_agents = Array.from(form.querySelectorAll("[data-review-row]"))
    .map((row) => ({
      role: readValue(row, '[name="review_role"]') === "verifier" ? "verifier" : "reviewer",
      agent_def_id: readValue(row, '[name="review_agent_def_id"]'),
      required: readChecked(row, '[name="review_required"]'),
    }))
    .filter((review) => review.agent_def_id);
  return {
    name: readValue(form, '[name="flow_name"]').trim(),
    description: readValue(form, '[name="flow_description"]').trim(),
    fix_agent_def_id: readValue(form, '[name="fix_agent_def_id"]'),
    phases,
    review_agents,
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

  const phaseRowHTML = renderPhaseRowView(agentDefs, {});
  const reviewRowHTML = renderReviewRowView(agentDefs, {});
  if (typeof form.addEventListener === "function") {
    form.addEventListener("click", (event) => {
      const target = event.target;
      if (!target || typeof target.closest !== "function") return;
      if (target.closest("[data-add-phase]")) {
        event.preventDefault();
        form.querySelector("[data-phase-rows]")?.insertAdjacentHTML("beforeend", phaseRowHTML);
      } else if (target.closest("[data-add-review]")) {
        event.preventDefault();
        form.querySelector("[data-review-rows]")?.insertAdjacentHTML("beforeend", reviewRowHTML);
      } else if (target.closest("[data-phase-row-remove]")) {
        event.preventDefault();
        target.closest("[data-phase-row]")?.remove();
      } else if (target.closest("[data-review-row-remove]")) {
        event.preventDefault();
        target.closest("[data-review-row]")?.remove();
      } else if (target.closest("[data-phase-row-up]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-phase-row]"), -1);
      } else if (target.closest("[data-phase-row-down]")) {
        event.preventDefault();
        moveRowView(target.closest("[data-phase-row]"), 1);
      } else if (target.closest("[data-flow-cancel]")) {
        event.preventDefault();
        state.editingFlowID = "";
        reload();
      }
    });
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (form.reportValidity && !form.reportValidity()) return;
    const payload = flowPayloadFromEditorView(form);
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
