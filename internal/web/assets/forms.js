// Delegated form submission, the counterpart to actions.js. A form declares
// what it is with a data-* attribute; one submit listener on <flow-app>
// dispatches. Same reason as the click table: forms live inside elements that
// replace their own innerHTML.

import { apiPatch, apiPost, taskAPIBase, taskHref } from "./api.js";
import { value } from "./normalize.js";
import { uploadTaskAttachment } from "./task.js";
import { beginInFlight, failureMessage, inFlight, markBusy, settleStatus } from "./actions.js";

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
    // Relation rows ride in the create payload so the whole create is atomic.
    // A child-of row makes the new task the relation target (chosen parent
    // parent_of new task): it is sent with target_is_new_task set and a blank
    // target, which the server resolves to the new task inside the create
    // transaction. A parent that cannot be linked fails the whole create, so
    // nothing is committed, the form stays put, and resubmitting creates exactly
    // one task instead of a duplicate plus an orphan. There is no second,
    // post-create link request that could partially succeed.
    const collected = collectRelationRows(form);
    if (collected instanceof Error) return collected.message;
    const relations = [
      ...collected.create,
      ...collected.childOf.map((relation) => ({
        source_task_id: relation.target_task_id,
        target_task_id: "",
        kind: relation.kind,
        target_is_new_task: true,
      })),
    ];
    if (relations.length) payload.relations = relations;
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
    // Return the validation message (rather than writing it and returning
    // undefined) so the dispatcher keeps it visible when this is the final
    // in-flight submission. The success path below still returns undefined to
    // clear the line, and CANCELLED remains the distinct backed-out sentinel.
    if (!targetTaskID) return "Target task ID is required";
    await apiPost(`${taskAPIBase(form.dataset.project)}/${encodeURIComponent(taskID)}/relations`, {
      target_task_id: targetTaskID,
      kind,
    });
    form.reset();
    await app.refresh();
  },
};

// formBusyKey names an in-flight form submission by its stable mutation
// identity, not by DOM node: the form kind plus the project and the task (or
// thread) target it mutates. Keying on the form kind and its primary dataset
// value alone collides across distinct targets — a boolean data-attachment-form
// and a create-mode data-task-form both carry an empty primary value, so two
// uploaders on different tasks would share `form:attachmentForm:` and a pending
// upload on one would suppress an unrelated uploader after navigation. Scoping
// the key to project plus target lets independent forms run concurrently while
// a duplicate submission of the same target stays blocked, even across a poll
// repaint that replaced the form node.
//
// The project is the form's effective selected project, not just data-project:
// a create form rendered with several projects intentionally carries no
// data-project (renderTaskFormView puts the mutation target in the project
// <select>), so reading the dataset alone would collapse every multi-project
// create onto `form:taskForm::` and a pending create for one project would
// suppress an unrelated project's create. Preferring the selected project keeps
// per-project creates distinct; the dataset value remains the fallback for
// forms that carry it (edit forms, single-project creates). The identity is
// derived only from the form's data and fields — never the node — so a repaint
// that rebuilds the same form recomputes the same key and stays blocked.
export function formBusyKey(key, form) {
  const dataset = form?.dataset || {};
  const selectedProject = String(form?.elements?.project?.value ?? "");
  const project = selectedProject || String(dataset.project ?? "");
  const target =
    key === "threadReplyForm"
      ? String(dataset.threadReplyForm ?? "")
      : String(dataset.task ?? dataset[key] ?? "");
  return `form:${key}:${project}:${target}`;
}

// collectRelationRows reads the create form's relation picker rows and splits
// them by direction. blocks/related_to rows make the new task the source, so
// they become `relations` payload entries ({target_task_id, kind}; the server
// defaults the source to the new task). parent_of ("child of") rows make the
// new task the *target*, so they are returned separately and applied after
// creation via the link endpoint. Rows with a blank target are dropped so they
// can never produce a 400; a duplicate (kind, target) pair is rejected
// outright. At most one child-of row is accepted: a task has exactly one
// parent, and because child-of links are applied after the create POST has
// committed, a second distinct child-of row would leave a partially related
// task instead of failing the submission — so it is rejected before any
// request goes out. Returns {create, childOf}, or an Error describing the
// first problem.
export function collectRelationRows(form) {
  const rows = typeof form.querySelectorAll === "function"
    ? form.querySelectorAll("[data-relation-row]")
    : [];
  const create = [];
  const childOf = [];
  const seen = new Set();
  for (const row of rows) {
    const target = String(row.querySelector?.("[data-relation-target]")?.value || "").trim();
    if (!target) continue;
    const kind = String(row.querySelector?.("[data-relation-kind]")?.value || "").trim();
    const key = `${kind}\u0000${target}`;
    if (seen.has(key)) {
      return new Error(`Duplicate relation: ${kind || "relation"} ${target}`);
    }
    seen.add(key);
    if (kind === "parent_of") {
      if (childOf.length) {
        return new Error("A task can have only one parent; remove the extra child-of rows");
      }
      childOf.push({ target_task_id: target, kind });
    } else {
      create.push({ target_task_id: target, kind });
    }
  }
  return { create, childOf };
}

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
  const busyKey = formBusyKey(key, form);
  if (inFlight.has(busyKey)) return true;
  const submitter = typeof form.querySelector === "function" ? form.querySelector('[type="submit"]') : null;
  const restore = submitter ? markBusy(submitter) : () => {};
  const pending = `${FORM_PENDING_LABELS[key] || "Submitting"}…`;
  beginInFlight(busyKey, pending);
  app.setStatus?.(pending);
  try {
    const result = await FORMS[key](app, form);
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
    restore();
  }
  return true;
}
