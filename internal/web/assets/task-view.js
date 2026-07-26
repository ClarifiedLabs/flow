// The new-task page: the create form and its flow selector.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

const SKIPPABLE_WORKFLOW_STEP_KINDS = new Set(["automated_checks", "change_review", "verify_change"]);

export function workflowStepCanBeSkipped(kind) {
  return SKIPPABLE_WORKFLOW_STEP_KINDS.has(String(kind || ""));
}

export async function renderNewTaskView(app, context) {
  if (context && !app.isActiveLoad(context)) return false;
  const defaultProject = defaultCreateProject(app, "");
  if (defaultProject) await app.ensureFlows(defaultProject);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("New Task");
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>New Task</h2>
        </div>
      </div>
      ${renderTaskFormView(app, { priority: 0 }, { mode: "create", submitLabel: "Create" })}
    </section>
  `;
  return true;
}

// defaultCreateProject picks the project whose flows the create form should
// offer first: an explicit id, then the first active project, then the first
// registered project. Choosing a concrete project keeps the initial project
// and flow selects in sync when several projects are available.
export function defaultCreateProject(app, projectID) {
  const explicit = String(projectID || "").trim();
  if (explicit) return explicit;
  const projects = app.projects || [];
  const selected = app.selectedProjectIDs();
  if (selected.length) return selected[0];
  return projects.length ? value(projects[0], "id", "ID") : "";
}

// flowSelectOptionsView renders the <option>s for the flow selector from the
// per-project flow cache (app.ensureFlows). The project default is preselected
// for create mode and as the edit-mode fallback. Falls back to a single
// "Project default" option when no flows are loaded for the project yet.
export function flowSelectOptionsView(app, projectID, selectedFlowID) {
  const cache = (app.flowsByProject && app.flowsByProject.get(String(projectID || "").trim())) || { flows: [], defaultFlowID: "" };
  const flows = cache.flows || [];
  const defaultFlowID = cache.defaultFlowID || "";
  if (!flows.length) {
    return `<option value="" selected>Project default</option>`;
  }
  const selected = String(selectedFlowID || "").trim() || defaultFlowID;
  return flows.map((flow) => {
    const id = value(flow, "id", "ID");
    const name = value(flow, "name", "Name") || id;
    return `<option value="${escapeAttr(id)}" ${id === selected ? "selected" : ""}>${escapeHTML(name)}</option>`;
  }).join("");
}

export function renderTaskFormView(app, task, options = {}) {
  const mode = options.mode || "edit";
  const taskID = options.taskID || "";
  const submitLabel = options.submitLabel || "Save";
  const projectID = options.projectID || "";
  const projects = app.projects || [];
  const defaultProject = defaultCreateProject(app, projectID);
  const projectOptions = projects.map((project) => {
    const id = value(project, "id", "ID");
    const name = value(project, "name", "Name") || id;
    return `<option value="${escapeAttr(id)}" ${id === defaultProject ? "selected" : ""}>${escapeHTML(name)}</option>`;
  }).join("");
  const projectField = mode === "create"
    ? `
      <label class="task-field-project">
        <span>Project</span>
        <select name="project" required>
          ${projectOptions || `<option value="" selected>No projects available</option>`}
        </select>
      </label>`
    : "";
  const selectedFlowID = value(task, "flow_id", "FlowID");
  const flowOptions = flowSelectOptionsView(app, defaultProject, selectedFlowID);
  return `
    <form class="task-form" data-task-form="${escapeAttr(taskID)}" data-task-form-mode="${escapeAttr(mode)}"${projectID ? ` data-project="${escapeAttr(projectID)}"` : (mode === "create" && projects.length === 1 ? ` data-project="${escapeAttr(value(projects[0], "id", "ID"))}"` : "")}>
      ${projectField}
      <label class="task-field-priority">
        <span>Priority</span>
        <input name="priority" type="number" min="0" step="1" value="${Number(value(task, "priority", "Priority") || 0)}">
      </label>
      <label class="task-field-flow">
        <span>Flow</span>
        <select name="flow_id" data-flow-select>
          ${flowOptions}
        </select>
      </label>
      <label class="task-field-title wide">
        <span>Title</span>
        <input name="title" value="${escapeAttr(value(task, "title", "Title"))}" required>
      </label>
      <label class="wide">
        <span>Body</span>
        <textarea name="body" rows="8">${escapeHTML(value(task, "body", "Body"))}</textarea>
      </label>
      ${mode === "create" ? `
      <label class="wide">
        <span>Attachments</span>
        <input name="attachments" type="file" multiple>
      </label>` : ""}
      ${mode === "create" ? `
      <label class="check wide">
        <input name="queue_task" type="checkbox" checked>
        <span>Queue after creation</span>
      </label>` : ""}
      <div class="form-actions">
        <button class="button" type="submit">${escapeHTML(submitLabel)}</button>
      </div>
    </form>
  `;
}

// renderFlowSummaryLineView describes the task's flow as a one-line summary:
// the flow name plus its phase chain (e.g. "spec(gate) -> implement", each
// phase optionally annotated with its agent). It prefers the live flow status
// (data.flow, which carries the frozen phases + agent names); when the task is
// not yet scheduled it falls back to the selected/default flow name from the
// per-project flow cache.
export function renderFlowSummaryLineView(app, task, flow, projectID) {
  const phases = value(flow, "phases", "Phases") || [];
  if (flow && phases.length) {
    const flowName = value(flow, "flow_name", "FlowName");
    const chain = phases.map((phase) => {
      const name = value(phase, "name", "Name");
      const gate = value(phase, "gate", "Gate") === "human" ? "(gate)" : "";
      const agentName = value(phase, "agent_name", "AgentName");
      return `${escapeHTML(name)}${gate}${agentName ? ` · ${escapeHTML(agentName)}` : ""}`;
    }).join(" -> ");
    return `Flow <strong>${escapeHTML(flowName || "")}</strong> · ${chain}`;
  }
  const flowID = String(value(task, "flow_id", "FlowID") || "").trim();
  const cache = (app.flowsByProject && app.flowsByProject.get(String(projectID || "").trim())) || { flows: [], defaultFlowID: "" };
  const targetID = flowID || cache.defaultFlowID;
  const match = (cache.flows || []).find((candidate) => value(candidate, "id", "ID") === targetID);
  if (match) {
    const name = value(match, "name", "Name") || value(match, "id", "ID");
    const isDefault = value(match, "id", "ID") === cache.defaultFlowID;
    return `Flow <strong>${escapeHTML(name)}</strong>${!flowID && isDefault ? " (default)" : ""}`;
  }
  return `<span class="muted">No flow</span>`;
}


// bindTaskFlowControlsView refreshes the flow selector when the create form's
// project select changes: it fetches (and caches) that project's flows, then
// re-renders the flow <option>s for the newly chosen project.
export function bindTaskFlowControlsView(app, form) {
  const projectSelect = form?.elements?.project;
  const flowSelect = form?.elements?.flow_id;
  if (!projectSelect || !flowSelect || typeof projectSelect.addEventListener !== "function") return;
  projectSelect.addEventListener("change", async () => {
    const projectID = String(projectSelect.value || "").trim();
    if (projectID) await app.ensureFlows(projectID);
    flowSelect.innerHTML = flowSelectOptionsView(app, projectID, "");
  });
}

// flowHeaderMeta condenses the live flow status into the task header's meta
// line: "<flow name> · <phase> <n>/<count>" (1-based). Empty when there is no
// flow cursor yet.
export function flowHeaderMeta(flow) {
  if (!flow) return "";
  const flowName = value(flow, "flow_name", "FlowName");
  const phaseName = value(flow, "phase_name", "PhaseName");
  const phaseCount = Number(value(flow, "phase_count", "PhaseCount") || 0);
  const phaseIndex = Number(value(flow, "phase_index", "PhaseIndex") || 0);
  const phasePart = phaseName && phaseCount ? `${phaseName} ${phaseIndex + 1}/${phaseCount}` : phaseName;
  return [flowName, phasePart].filter(Boolean).join(" · ");
}
