// Delegated form submission, the counterpart to actions.js. A form declares
// what it is with a data-* attribute; one submit listener on <flow-app>
// dispatches. Same reason as the click table: forms live inside elements that
// replace their own innerHTML.

import { apiPatch, apiPost, taskAPIBase, taskHref } from "./api.js";
import { value } from "./normalize.js";
import { uploadTaskAttachment } from "./task.js";
import { inFlight, markBusy } from "./actions.js";

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
    if (!payload.title) return "Task title is required";
    if (mode !== "create") {
      await apiPatch(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.taskForm)}`, payload);
      await app.refresh();
      return "Task updated";
    }

    const formProject = form.elements.project ? form.elements.project.value : form.dataset.project || "";
    if (!formProject) return "Project is required";
    const data = await apiPost(taskAPIBase(formProject), payload);
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
  async relationAddForm(app, form) {
    const taskID = String(form.dataset.relationAddForm || "").trim();
    const kind = String(form.elements.kind?.value || "").trim();
    const targetTaskID = String(form.elements.target_task_id?.value || "").trim();
    if (!targetTaskID) {
      app.setStatus("Target task ID is required");
      return;
    }
    await apiPost(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(taskID)}/relations`, {
      target_task_id: targetTaskID,
      kind,
    });
    form.reset();
    await app.refresh();
  },
};

const FORM_PENDING_LABELS = {
  taskForm: "Saving task",
  attachmentForm: "Uploading attachment",
  attentionReplyForm: "Sending reply",
  threadReplyForm: "Posting reply",
};

// handleFormSubmit gives delegated form submissions the same pending state
// and duplicate-submit guard as the action buttons: the submit control is
// marked busy synchronously, the status line names the in-flight submission,
// and the in-flight registry (keyed by form kind and target, not by DOM node)
// blocks a re-submit while the first one is running — even across a poll
// re-render that replaced the form.
export async function handleFormSubmit(app, event) {
  const form = event.target;
  if (!form || form.tagName !== "FORM") return false;
  const key = Object.keys(form.dataset || {}).find((name) => Object.hasOwn(FORMS, name));
  if (!key) return false;

  event.preventDefault();
  if (form.reportValidity && !form.reportValidity()) return true;
  const busyKey = `form:${key}:${String(form.dataset[key] ?? "")}`;
  if (inFlight.has(busyKey)) return true;
  inFlight.add(busyKey);
  const submitter = typeof form.querySelector === "function" ? form.querySelector('[type="submit"]') : null;
  const restore = submitter ? markBusy(submitter) : () => {};
  app.setStatus?.(`${FORM_PENDING_LABELS[key] || "Submitting"}…`);
  try {
    const result = await FORMS[key](app, form);
    if (typeof result === "string" && result) app.setStatus?.(result);
    else if (result === CANCELLED) app.setStatus?.("");
  } catch (error) {
    app.setStatus?.(error.message || String(error));
  } finally {
    inFlight.delete(busyKey);
    restore();
  }
  return true;
}
