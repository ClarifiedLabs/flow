// Delegated form submission, the counterpart to actions.js. A form declares
// what it is with a data-* attribute; one submit listener on <flow-app>
// dispatches. Same reason as the click table: forms live inside elements that
// replace their own innerHTML.

import { apiPatch, apiPost, epicsAPIBase, featuresAPIBase, taskAPIBase, taskHref, workItemAPIPath } from "./api.js";
import { value } from "./normalize.js";
import { uploadTaskAttachment } from "./task.js";
import { collectCreateWorkItemRelations } from "./create-relations.js";
import {
  acquireBusy,
  formBusyControl,
  formBusyKey,
  markBusy,
  releaseBusy,
} from "./actions/registry.js";
import { actionScope, failureMessage, settleStatus } from "./actions/dispatch.js";

// formBusyKey lives in actions.js next to the in-flight registry (forms.js
// already imports actions.js, so the dependency stays one-way); it is
// re-exported below for callers that think of it as part of the
// delegated-forms API.
export { formBusyKey };

// FORMS handlers return the confirmation message for the status line, or a
// validation message the dispatcher shows in place of the pending label. They
// return CANCELLED when the user backed out with nothing to show — the
// dispatcher then clears the pending label it wrote on submit, so no stale
// "Saving task…" lingers when nothing was sent.
const CANCELLED = Symbol("cancelled");

export const FORMS = {
  async taskForm(app, form) {
    const mode = form.dataset.taskFormMode || "edit";
    const priority = Number(form.elements.priority?.value || 0);
    if (!Number.isInteger(priority)) return "Priority must be a whole number";
    const payload = {
      title: form.elements.title.value.trim(),
      body: form.elements.body.value,
      priority,
      flow_id: form.elements.flow_id ? form.elements.flow_id.value : "",
    };
    if (form.elements.requires_human_review) {
      payload.requires_human_review = Boolean(form.elements.requires_human_review.checked);
    }
    if (!payload.title) return "Task title is required";
    if (mode !== "create") {
      await apiPatch(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.taskForm)}`, payload);
      form.closest?.("flow-task-detail")?.finishEditing?.();
      await app.refresh();
      return "Task updated";
    }
    const parentItemID = String(form.elements.parent_item_id?.value || "").trim();
    if (parentItemID) payload.parent_item_id = parentItemID;

    const formProject = form.elements.project ? form.elements.project.value : form.dataset.project || "";
    if (!formProject) return "Project is required";
    // Generic relation rows ride in the original create POST so cross-kind
    // links are atomic with task creation. Exactly one endpoint is marked new.
    const relations = collectCreateWorkItemRelations(form, parentItemID);
    if (relations instanceof Error) return relations.message;
    if (relations.length) payload.work_item_relations = relations;
    const data = await apiPost(taskAPIBase(formProject), payload);
    app.caches?.invalidate?.("workItems", formProject);
    const task = data.task || data.Task || {};
    const taskID = value(task, "id", "ID");
    if (!taskID) throw new Error("Created task ID unavailable");
    const createdProject = data.project_id || data.ProjectID || formProject;
    history.pushState({}, "", taskHref(createdProject, taskID));
    for (const file of Array.from(form.elements.attachments?.files || [])) {
      await uploadTaskAttachment(createdProject, taskID, file, "initial");
    }
    if (form.elements.queue_task?.checked) {
      await apiPost(`${taskAPIBase(createdProject)}/${encodeURIComponent(taskID)}/schedule`, {});
    }
    await app.load();
    return "Task created";
  },

  // featureForm creates (blank data-feature-form on the list page) or edits
  // (data-feature-form = id on the detail page) a feature. Both invalidate
  // the features cache so pickers refetch.
  async featureForm(app, form) {
    const featureID = String(form.dataset.featureForm || "").trim();
    const projectID = String(form.dataset.project || "").trim();
    if (!projectID) return "Project is required";
    const title = String(form.elements.title?.value || "").trim();
    if (!title) return "Feature title is required";
    const body = String(form.elements.body?.value || "");
    if (featureID) {
      await apiPatch(`${featuresAPIBase(projectID)}/${encodeURIComponent(featureID)}`, { title, body });
      app.caches?.invalidate?.("features", projectID);
      app.caches?.invalidate?.("workItems", projectID);
      await app.refresh();
      return "Feature updated";
    }
    const parentItemID = String(form.elements.parent_item_id?.value || "").trim();
    const relations = collectCreateWorkItemRelations(form, parentItemID);
    if (relations instanceof Error) return relations.message;
    const payload = {
      title,
      body,
      ...(parentItemID ? { parent_item_id: parentItemID } : {}),
      ...(relations.length ? { work_item_relations: relations } : {}),
    };
    await apiPost(featuresAPIBase(projectID), payload);
    app.caches?.invalidate?.("features", projectID);
    app.caches?.invalidate?.("workItems", projectID);
    await app.refresh();
    return "Feature created";
  },

  async epicForm(app, form) {
    const epicID = String(form.dataset.epicForm || "").trim();
    const projectID = String(form.dataset.project || "").trim();
    if (!projectID) return "Project is required";
    const title = String(form.elements.title?.value || "").trim();
    if (!title) return "Epic title is required";
    const priority = Number(form.elements.priority?.value || 0);
    if (!Number.isInteger(priority) || priority < 0) return "Priority must be a non-negative whole number";
    const payload = {
      title,
      body: String(form.elements.body?.value || ""),
      priority,
      completion_policy: String(form.elements.completion_policy?.value || "all_children"),
    };
    if (epicID) {
      await apiPatch(`${epicsAPIBase(projectID)}/${encodeURIComponent(epicID)}`, payload);
      app.caches?.invalidate?.("workItems", projectID);
      await app.refresh();
      return "Epic updated";
    }
    const parentItemID = String(form.elements.parent_item_id?.value || "").trim();
    if (parentItemID) payload.parent_item_id = parentItemID;
    const relations = collectCreateWorkItemRelations(form, parentItemID);
    if (relations instanceof Error) return relations.message;
    if (relations.length) payload.work_item_relations = relations;
    await apiPost(epicsAPIBase(projectID), payload);
    app.caches?.invalidate?.("workItems", projectID);
    await app.refresh();
    return "Epic created";
  },

  async attachmentForm(app, form) {
    await uploadTaskAttachment(
      form.dataset.project,
      form.dataset.task,
      form.elements.file?.files?.[0],
      form.elements.stage.value,
    );
    form.reset();
    await app.refresh();
    return "Attachment uploaded";
  },

  async attentionReplyForm(app, form) {
    const message = String(form.elements.message?.value || "").trim();
    if (!message) return "A reply is required";
    const statusLogID = Number(form.dataset.statusLogId || 0);
    await apiPost(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.task)}/attention/reply`, {
      message,
      ...(statusLogID ? { status_log_id: statusLogID } : {}),
    });
    await app.refresh();
    return "Reply sent";
  },

  async threadReplyForm(app, form) {
    const body = String(form.elements.body?.value || "").trim();
    if (!body) return CANCELLED;
    await apiPost(`/v2/threads/${encodeURIComponent(form.dataset.threadReplyForm)}/comments`, { body });
    await app.refresh();
    return "Reply posted";
  },

  // relationAddForm links the viewed task (the relation source) to a target task
  // with the chosen kind. The server defaults the source to the task in the
  // path, so the payload only needs the target and the kind.
  async moveWorkItemForm(app, form) {
    const itemID = String(form.dataset.moveWorkItemForm || "").trim();
    const projectID = String(form.dataset.project || "").trim();
    await apiPatch(workItemAPIPath(projectID, itemID, "/parent"), {
      parent_item_id: String(form.elements.parent_item_id?.value || "").trim(),
    });
    app.caches?.invalidate?.("workItems", projectID);
    await app.refresh();
    return "Work item moved";
  },

  async workItemRelationAddForm(app, form) {
    const itemID = String(form.dataset.workItemRelationAddForm || "").trim();
    const targetID = String(form.elements.target_item_id?.value || "").trim();
    const kind = String(form.elements.kind?.value || "").trim();
    if (!targetID) return "Target work item ID is required";
    const projectID = String(form.dataset.project || "").trim();
    await apiPost(workItemAPIPath(projectID, itemID, "/relations"), {
      source_item_id: itemID,
      target_item_id: targetID,
      kind,
    });
    if (kind === "parent_of") app.caches?.invalidate?.("workItems", projectID);
    await app.refresh();
    return "Relation added";
  },

  async relationAddForm(app, form) {
    const taskID = String(form.dataset.relationAddForm || "").trim();
    const kind = String(form.elements.kind?.value || "").trim();
    const targetTaskID = String(form.elements.target_task_id?.value || "").trim();
    // Return the validation message (rather than writing it and returning
    // undefined) so the dispatcher keeps it visible when this is the final
    // in-flight submission; CANCELLED remains the distinct backed-out sentinel.
    if (!targetTaskID) return "Target task ID is required";
    await apiPost(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(taskID)}/relations`, {
      target_task_id: targetTaskID,
      kind,
    });
    form.reset();
    await app.refresh();
    return "Relation added";
  },
};

const FORM_PENDING_LABELS = {
  taskForm: "Saving task",
  featureForm: "Saving feature",
  epicForm: "Saving epic",
  attachmentForm: "Uploading attachment",
  attentionReplyForm: "Sending reply",
  threadReplyForm: "Posting reply",
  relationAddForm: "Adding relation",
  workItemRelationAddForm: "Adding relation",
  moveWorkItemForm: "Moving work item",
};

// handleFormSubmit gives delegated form submissions the same pending state
// and duplicate-submit guard as the action buttons: the form's busy control —
// its submit control, or its first editable field when a buttonless form
// (like the inline thread reply) submits implicitly — is marked busy
// synchronously, the status line names the in-flight submission, and the
// in-flight registry (keyed by form kind and target, not by DOM node) blocks
// a re-submit while the first one is running — even across a poll re-render
// that replaced the form. A repaint re-marks the replacement form's control
// through applyBusyState, which matches forms by the same formBusyKey.
export async function handleFormSubmit(app, event) {
  const form = event.target;
  if (!form || form.tagName !== "FORM") return false;
  const key = Object.keys(form.dataset || {}).find((name) => Object.hasOwn(FORMS, name));
  if (!key) return false;

  event.preventDefault();
  if (form.reportValidity && !form.reportValidity()) return true;
  const busyKey = formBusyKey(key, form);
  const entry = acquireBusy(busyKey, `${FORM_PENDING_LABELS[key] || "Submitting"}…`);
  if (!entry) return true;
  const control = formBusyControl(form);
  if (control) entry.restores.add(markBusy(control));
  app.setStatus?.(entry.label);
  try {
    // The handler runs against the action-scoped app so its own refresh
    // carries the settle-burst provenance token (see actionScope).
    const result = await FORMS[key](actionScope(app), form);
    // settleStatus arbitrates the shared status line: it keeps a still-pending
    // sibling's label visible instead of showing this submission's result
    // early, and distinguishes a confirmation message from CANCELLED (an
    // explicit clear) and undefined (a silent success that leaves the pending
    // label in place).
    settleStatus(app, busyKey, result === CANCELLED ? "" : result);
  } catch (error) {
    // failureMessage is total, so settleStatus always runs and the key always
    // drains — even for a non-Error rejection such as `reject(null)`.
    settleStatus(app, busyKey, failureMessage(error));
  } finally {
    releaseBusy(busyKey);
  }
  return true;
}
