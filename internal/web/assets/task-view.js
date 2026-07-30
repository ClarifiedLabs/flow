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
  if (defaultProject && typeof app.ensureFeatures === "function") await app.ensureFeatures(defaultProject);
  // Load the default project's tasks too, so the relation picker's target-task
  // suggestions are populated even on a direct visit with an empty cache. A
  // failed or empty load just leaves the picker in manual-entry mode.
  if (defaultProject && typeof app.ensureTasks === "function") await app.ensureTasks(defaultProject);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("New Task");
  const content = app.querySelector(".content");
  content.innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>New Task</h2>
        </div>
      </div>
      ${renderTaskFormView(app, { priority: 0 }, { mode: "create", submitLabel: "Create" })}
    </section>
  `;
  bindRelationsPickerView(content.querySelector?.("[data-task-form]"), app);
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

// featureSelectOptionsView renders the <option>s for the feature picker from
// the per-project feature cache (app.ensureFeatures). Open features only,
// except the currently-assigned one, which stays selectable even after the
// feature landed so an edit form never silently reassigns.
export function featureSelectOptionsView(app, projectID, selectedFeatureID) {
  const features = (app.featuresByProject && app.featuresByProject.get(String(projectID || "").trim())) || [];
  const selected = String(selectedFeatureID || "").trim();
  const options = [`<option value=""${selected ? "" : " selected"}>No feature</option>`];
  for (const entry of features) {
    const feature = value(entry, "feature", "Feature") || entry;
    const id = value(feature, "id", "ID");
    const title = value(feature, "title", "Title") || id;
    const status = value(feature, "status", "Status") || "open";
    if (status !== "open" && id !== selected) continue;
    options.push(`<option value="${escapeAttr(id)}" ${id === selected ? "selected" : ""}>${escapeHTML(title)}</option>`);
  }
  return options.join("");
}

// RELATION_KIND_OPTIONS lists the relation kinds the create picker offers,
// labelled from the new task's perspective. "child of X" means the new task
// becomes X's child. parent_of stores source = parent, so a child-of row must
// make the new task the relation *target*; the create payload cannot express
// that for owner tokens (a blank target is rejected), so such rows are applied
// after creation via the link endpoint (X parent_of new-task). blocks and
// related_to store source = the new task, so they go straight into the create
// payload as {target_task_id, kind}.
export const RELATION_KIND_OPTIONS = [
  { kind: "parent_of", label: "child of" },
  { kind: "blocks", label: "blocks" },
  { kind: "related_to", label: "related to" },
];

// relationKindOptionsView renders the picker's kind <option>s, optionally
// preselecting one kind.
export function relationKindOptionsView(selectedKind = "") {
  return RELATION_KIND_OPTIONS.map((option) =>
    `<option value="${escapeAttr(option.kind)}" ${option.kind === selectedKind ? "selected" : ""}>${escapeHTML(option.label)}</option>`,
  ).join("");
}

// relationPickerRowView renders one picker row: a kind select, a target task
// id input, and a remove button. Rows are appended to [data-relation-rows]
// after render, so the markup here is the initial empty state.
export function relationPickerRowView() {
  return `
    <div class="relation-row" data-relation-row>
      <select name="relation_kind" data-relation-kind>
        ${relationKindOptionsView()}
      </select>
      <input name="relation_target" data-relation-target placeholder="Task id" list="relation-target-tasks">
      <button class="button secondary relation-remove" type="button" data-relation-remove aria-label="Remove relation">&times;</button>
    </div>
  `;
}

// relationTargetSuggestionsView renders the datalist <option>s offering the
// selected project's existing tasks as relation targets. The value is the task
// id (what gets submitted); when a title is present the label shows it so a
// reader can tell tasks apart. The list is read from the per-project task cache
// (app.ensureTasks) so it stays project-scoped; a missing cache entry yields no
// options and the input falls back to manual entry.
export function relationTargetSuggestionsView(app, projectID) {
  const id = String(projectID || "").trim();
  const tasks = (app && app.tasksByProject && app.tasksByProject.get(id)) || [];
  return tasks.map((task) => {
    const taskID = value(task, "id", "ID");
    const title = value(task, "title", "Title");
    return `<option value="${escapeAttr(taskID)}"${title ? ` label="${escapeAttr(title)}"` : ""}></option>`;
  }).join("");
}

// relationsPickerView renders the create-mode-only picker: a labelled list of
// relation rows plus an "Add relation" button, and a datalist of the selected
// project's existing task ids so the target input can autocomplete. It starts
// with one row so the picker's shape is visible immediately; a row left with a
// blank target is dropped on submit.
export function relationsPickerView(app, projectID) {
  return `
      <div class="wide relation-picker" data-relation-picker>
        <span class="relation-picker-label">Relations</span>
        <div class="relation-rows" data-relation-rows>${relationPickerRowView()}</div>
        <div class="relation-picker-actions">
          <button class="button secondary" type="button" data-relation-add>Add relation</button>
        </div>
        <datalist id="relation-target-tasks">${relationTargetSuggestionsView(app, projectID)}</datalist>
      </div>`;
}

// bindRelationRowRemove wires a row's remove button to drop the row.
function bindRelationRowRemove(row) {
  const removeButton = row.querySelector?.("[data-relation-remove]");
  if (removeButton && typeof removeButton.addEventListener === "function") {
    removeButton.addEventListener("click", () => row.remove());
  }
}

// bindRelationsPickerView wires the create form's relation picker: the add
// button appends a row, each row's remove button drops it. Rows are created by
// parsing relationPickerRowView into a detached container so the markup stays
// the single source of truth. The new-task page is static (no poll), so these
// direct listeners are never lost to a repaint.
//
// When app is supplied and the form can switch projects, changing the project
// select reloads that project's task suggestions into the shared datalist, so
// the target-task autocomplete stays scoped to the chosen project. The refresh
// is best-effort: a failed or empty load leaves the datalist empty and the
// input in manual-entry mode.
export function bindRelationsPickerView(form, app) {
  if (!form || typeof form.querySelector !== "function") return;
  const rows = form.querySelector("[data-relation-rows]");
  const addButton = form.querySelector("[data-relation-add]");
  if (!rows || !addButton || typeof addButton.addEventListener !== "function") return;
  for (const row of rows.querySelectorAll?.("[data-relation-row]") || []) bindRelationRowRemove(row);
  addButton.addEventListener("click", () => {
    const container = document.createElement("div");
    container.innerHTML = relationPickerRowView();
    const row = container.firstElementChild;
    if (!row) return;
    bindRelationRowRemove(row);
    rows.appendChild(row);
    row.querySelector("[data-relation-target]")?.focus?.();
  });
  const projectSelect = form.querySelector('[name="project"]');
  if (app && projectSelect && typeof app.ensureTasks === "function" && typeof projectSelect.addEventListener === "function") {
    projectSelect.addEventListener("change", async () => {
      const projectID = String(projectSelect.value || "").trim();
      await app.ensureTasks(projectID);
      // A rapid project switch could resolve out of order; only repaint when
      // this load is still for the selected project so the suggestions never
      // disagree with the project select.
      if (String(projectSelect.value || "").trim() !== projectID) return;
      const datalist = form.querySelector("#relation-target-tasks");
      if (datalist) datalist.innerHTML = relationTargetSuggestionsView(app, projectID);
    });
  }
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
      <label class="task-field-feature">
        <span>Feature</span>
        <select name="feature_id" data-feature-select aria-label="Feature">
          ${featureSelectOptionsView(app, defaultProject, value(task, "feature_id", "FeatureID"))}
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
      ${mode === "create" ? relationsPickerView(app, defaultProject) : ""}
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
    if (projectID) {
      await app.ensureFlows(projectID);
      if (typeof app.ensureFeatures === "function") await app.ensureFeatures(projectID);
    }
    flowSelect.innerHTML = flowSelectOptionsView(app, projectID, "");
    const featureSelect = form.elements.feature_id;
    if (featureSelect) featureSelect.innerHTML = featureSelectOptionsView(app, projectID, "");
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
