// The new-task page: the create form and its flow selector.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { buildWorkItemIndex, workItemDescendants, workItemFeatureID, workItemID, workItemKind, workItemState } from "./work-item-model.js";
import {
  bindRelationsPickerView as bindCreateRelationsPickerView,
  relationsPickerView,
  refreshRelationsPickerSuggestions,
} from "./create-relations.js";

export { relationTargetSuggestionsView } from "./create-relations.js";

const SKIPPABLE_WORKFLOW_STEP_KINDS = new Set(["automated_checks", "change_review", "verify_change"]);

export function workflowStepCanBeSkipped(kind) {
  return SKIPPABLE_WORKFLOW_STEP_KINDS.has(String(kind || ""));
}

export async function renderNewTaskView(app, context) {
  if (context && !app.isActiveLoad(context)) return false;
  const params = new URLSearchParams(window.location.search);
  const defaultProject = defaultCreateProject(app, params.get("project") || "");
  const parentItemID = String(params.get("parent") || "").trim();
  if (defaultProject) await app.ensureFlows(defaultProject);
  if (defaultProject && typeof app.ensureWorkItems === "function") await app.ensureWorkItems(defaultProject);
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
      ${renderTaskFormView(app, { priority: 0, parent_item_id: parentItemID }, { mode: "create", submitLabel: "Create", projectID: defaultProject })}
    </section>
  `;
  const form = content.querySelector?.("[data-task-form]");
  bindRelationsPickerView(form, app);
  bindTaskFlowControlsView(app, form);
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

export function workItemParentOptionsView(app, projectID, selectedParentID = "", movingItemID = "") {
  const items = (app.workItemsByProject && app.workItemsByProject.get(String(projectID || "").trim())) || [];
  const index = buildWorkItemIndex({ items });
  const excluded = new Set([String(movingItemID || ""), ...workItemDescendants(index, movingItemID).map(workItemID)]);
  const selected = String(selectedParentID || "").trim();
  return index.items
    .filter((item) => {
      const id = workItemID(item);
      const capabilities = value(item, "capabilities", "Capabilities") || {};
      const canContain = value(capabilities, "can_contain", "CanContain");
      return id && !excluded.has(id) && (canContain === true || (canContain == null && workItemKind(item) !== "task")) && (!workItemState(item).terminal || id === selected);
    })
    .map((item) => `<option value="${escapeAttr(workItemID(item))}">${escapeHTML(value(item, "title", "Title") || workItemID(item))} · ${escapeHTML(workItemKind(item))}</option>`)
    .join("");
}

export function inferredFeatureView(app, projectID, parentItemID) {
  const items = (app.workItemsByProject && app.workItemsByProject.get(String(projectID || "").trim())) || [];
  const index = buildWorkItemIndex({ items });
  const parent = index.byID.get(String(parentItemID || "").trim());
  if (!parent) return "No feature inferred";
  const featureID = workItemKind(parent) === "feature" ? workItemID(parent) : workItemFeatureID(parent);
  const feature = index.byID.get(featureID);
  return featureID ? `Feature: ${value(feature || {}, "title", "Title") || featureID}` : "No feature inferred";
}

// The task-create page is static, but uses the same delegated row behavior as
// the repainting work-item elements.
export function bindRelationsPickerView(form) {
  bindCreateRelationsPickerView(form);
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
  const selectedParentID = String(value(task, "parent_item_id", "ParentItemID") || "");
  const requiresHumanReview = Boolean(value(task, "requires_human_review", "RequiresHumanReview"));
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
      ${mode === "create" ? `<label class="task-field-feature">
        <span>Parent</span>
        <input name="parent_item_id" data-parent-item-input list="task-parent-items" value="${escapeAttr(selectedParentID)}" placeholder="No parent">
        <datalist id="task-parent-items">${workItemParentOptionsView(app, defaultProject, selectedParentID)}</datalist>
        <small data-inferred-feature>${escapeHTML(inferredFeatureView(app, defaultProject, selectedParentID))}</small>
      </label>` : ""}
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
      ${mode === "create" ? relationsPickerView((app.workItemsByProject && app.workItemsByProject.get(defaultProject)) || []) : ""}
      <div class="form-actions task-form-actions">
        <label class="check">
          <input name="requires_human_review" type="checkbox"${requiresHumanReview ? " checked" : ""}>
          <span>Require human review</span>
        </label>
        ${mode === "create" ? `
        <label class="check">
          <input name="queue_task" type="checkbox" checked>
          <span>Queue after creation</span>
        </label>` : ""}
        <button class="button" type="submit">${escapeHTML(submitLabel)}</button>
      </div>
    </form>
  `;
}

// bindTaskFlowControlsView refreshes the flow selector when the create form's
// project select changes: it fetches that project's flows and work-item
// summaries, then repaints the flow, canonical parent, and relation suggestion
// controls together. A failed load leaves explicit defaults/manual ID entry.
export function bindTaskFlowControlsView(app, form) {
  const projectSelect = form?.elements?.project;
  const flowSelect = form?.elements?.flow_id;
  if (!projectSelect || !flowSelect || typeof projectSelect.addEventListener !== "function") return;
  projectSelect.addEventListener("change", async () => {
    const projectID = String(projectSelect.value || "").trim();
    try {
      if (projectID) {
        await app.ensureFlows(projectID);
        if (typeof app.ensureWorkItems === "function") await app.ensureWorkItems(projectID);
      }
    } catch {
      // Best-effort refresh: a rejected load must not reject the listener or
      // leave the prior project's options painted as current; the repaint
      // below then renders the new project's (empty) cache as the explicit
      // default options.
    }
    // A rapid project switch could resolve out of order; only repaint when
    // this load is still for the selected project so the flow (and feature)
    // options never disagree with the project select.
    if (String(projectSelect.value || "").trim() !== projectID) return;
    flowSelect.innerHTML = flowSelectOptionsView(app, projectID, "");
    const parentInput = form.elements.parent_item_id;
    const datalist = form.querySelector?.("#task-parent-items");
    if (datalist) datalist.innerHTML = workItemParentOptionsView(app, projectID, parentInput?.value || "");
    refreshRelationsPickerSuggestions(form, app, projectID);
    const inferred = form.querySelector?.("[data-inferred-feature]");
    if (inferred) inferred.textContent = inferredFeatureView(app, projectID, parentInput?.value || "");
  });
  const parentInput = form.elements.parent_item_id;
  if (parentInput && typeof parentInput.addEventListener === "function") parentInput.addEventListener("input", () => {
    const inferred = form.querySelector?.("[data-inferred-feature]");
    if (inferred) inferred.textContent = inferredFeatureView(app, projectSelect.value, parentInput.value);
  });
}
