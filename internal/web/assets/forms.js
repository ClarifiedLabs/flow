// Delegated form submission, the counterpart to actions.js. A form declares
// what it is with a data-* attribute; one submit listener on <flow-app>
// dispatches. Same reason as the click table: forms live inside elements that
// replace their own innerHTML.

import { apiPatch, apiPost, taskAPIBase, taskHref } from "./api.js";
import { value } from "./normalize.js";
import { uploadTaskAttachment } from "./task.js";

export const FORMS = {
  async taskForm(app, form) {
    const mode = form.dataset.taskFormMode || "edit";
    const priority = Number(form.elements.priority?.value || 0);
    if (!Number.isInteger(priority)) {
      app.setStatus("Priority must be a whole number");
      return;
    }
    const payload = {
      title: form.elements.title.value.trim(),
      body: form.elements.body.value,
      priority,
      flow_id: form.elements.flow_id ? form.elements.flow_id.value : "",
    };
    if (!payload.title) {
      app.setStatus("Task title is required");
      return;
    }
    if (mode !== "create") {
      await apiPatch(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.taskForm)}`, payload);
      await app.refresh();
      return;
    }

    const formProject = form.elements.project ? form.elements.project.value : form.dataset.project || "";
    if (!formProject) {
      app.setStatus("Project is required");
      return;
    }
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
  },

  async attentionReplyForm(app, form) {
    const message = String(form.elements.message?.value || "").trim();
    if (!message) {
      app.setStatus("A reply is required");
      return;
    }
    const statusLogID = Number(form.dataset.statusLogId || 0);
    await apiPost(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(form.dataset.task)}/attention/reply`, {
      message,
      ...(statusLogID ? { status_log_id: statusLogID } : {}),
    });
    await app.refresh();
  },

  async threadReplyForm(app, form) {
    const body = String(form.elements.body?.value || "").trim();
    if (!body) return;
    await apiPost(`/v2/threads/${encodeURIComponent(form.dataset.threadReplyForm)}/comments`, { body });
    await app.refresh();
  },
};

export async function handleFormSubmit(app, event) {
  const form = event.target;
  if (!form || form.tagName !== "FORM") return false;
  const key = Object.keys(form.dataset || {}).find((name) => Object.hasOwn(FORMS, name));
  if (!key) return false;

  event.preventDefault();
  if (form.reportValidity && !form.reportValidity()) return true;
  try {
    await FORMS[key](app, form);
  } catch (error) {
    app.setStatus(error.message || String(error));
  }
  return true;
}
