// Delegated-form tests: submission busy state, form busy keys, and the
// in-flight registry shared with the action dispatcher.

import assert from "node:assert/strict";
import { test } from "node:test";
import { applyBusyState, inFlight } from "./actions.js";
import { formBusyKey, handleFormSubmit } from "./forms.js";
import { ActionButton, deferred, flushAsync, scriptContext, statusApp } from "./test-helpers.mjs";

test("a form submission marks its submit control busy and guards against a duplicate submit", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  const handled = handleFormSubmit(app, { target: form, preventDefault() {} });

  assert.equal(requests, 1);
  assert.equal(submitButton.disabled, true);
  assert.equal(submitButton.getAttribute("aria-busy"), "true");
  assert.deepEqual(app.statuses, ["Saving task\u2026"]);

  // A second submit while the first is in flight must not issue another request.
  await handleFormSubmit(app, { target: form, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;
  assert.equal(submitButton.disabled, false);
  assert.equal(submitButton.getAttribute("aria-busy"), null);
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Task updated"]);
});

test("a poll re-render replacing the form re-applies the busy state to the replacement submitter", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const makeForm = (submitter) => ({
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitter : null;
    },
  });

  const firstSubmit = new ActionButton({});
  const handled = handleFormSubmit(app, { target: makeForm(firstSubmit), preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(firstSubmit.disabled, true);
  assert.deepEqual(app.statuses, ["Saving task\u2026"]);

  // The poll swaps the form for a fresh one. The repaint re-applies the
  // in-flight state from the registry, so the replacement's submit control is
  // disabled and visibly busy — not apparently actionable but inert.
  const replacementSubmit = new ActionButton({});
  const replacementForm = makeForm(replacementSubmit);
  applyBusyState({ querySelectorAll: (selector) => (selector === "form" ? [replacementForm] : []) });
  assert.equal(replacementSubmit.disabled, true);
  assert.equal(replacementSubmit.getAttribute("aria-busy"), "true");
  assert.equal(replacementSubmit.classList.contains("is-busy"), true);

  // Submitting the replacement while the first request is in flight must not
  // issue a second request: the guard lives in the registry, not on the node.
  await handleFormSubmit(app, { target: replacementForm, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;

  // Settling restores whatever control is on screen now — the repaint-marked
  // replacement — not the discarded original form's submitter.
  assert.equal(replacementSubmit.disabled, false);
  assert.equal(replacementSubmit.getAttribute("aria-busy"), null);
  assert.equal(replacementSubmit.classList.contains("is-busy"), false);
  assert.equal(inFlight.size, 0);

  // Once the first submission settles the form is submittable again.
  const second = handleFormSubmit(app, { target: replacementForm, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("a validation-cancelled form submit replaces the pending label with the validation error", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "   " },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The pending label is replaced by the validation error, which stays visible
  // rather than being cleared.
  assert.deepEqual(app.statuses, ["Saving task\u2026", "Task title is required"]);
  assert.equal(requests, 0, "a validation-cancelled submit issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(submitButton.getAttribute("aria-busy"), null);
  assert.equal(inFlight.size, 0);
});

test("a backed-out form submit clears the pending label it created", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { threadReplyForm: "th-0001" },
    elements: {
      body: { value: "   " },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The handler backed out with nothing to show, so the pending label is cleared.
  assert.deepEqual(app.statuses, ["Posting reply\u2026", ""]);
  assert.equal(requests, 0, "a backed-out submit issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(inFlight.size, 0);
});

test("an empty relation target keeps its validation failure visible when it is the final submission", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  globalThis.fetch = () => {
    requests += 1;
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const submitButton = new ActionButton({});
  const form = {
    tagName: "FORM",
    dataset: { relationAddForm: "t-0001", project: "p-alpha" },
    elements: {
      kind: { value: "blocked_by" },
      target_task_id: { value: "   " },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? submitButton : null;
    },
  };

  await handleFormSubmit(app, { target: form, preventDefault() {} });

  // The validation failure is the final mutation's outcome, so it must remain
  // on the status line rather than being cleared by settlement.
  assert.deepEqual(app.statuses, ["Adding relation\u2026", "Target task ID is required"]);
  assert.equal(requests, 0, "an empty relation target issues no request");
  assert.equal(submitButton.disabled, false);
  assert.equal(inFlight.size, 0);
});

// An attachment form carries enough surface for handleFormSubmit's pending
// state plus the attachmentForm handler: a dataset, a submit control, a file
// input, a stage select, and a reset() the handler calls on success.
function attachmentFormFixture(dataset) {
  const submitButton = new ActionButton({});
  return {
    submitButton,
    form: {
      tagName: "FORM",
      dataset,
      elements: {
        file: { files: [new Blob(["attachment body"], { type: "text/plain" })] },
        stage: { value: "initial" },
      },
      reportValidity() {
        return true;
      },
      querySelector(selector) {
        return selector === '[type="submit"]' ? submitButton : null;
      },
      reset() {},
    },
  };
}

test("attachment forms for distinct task targets submit concurrently", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  const first = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });
  const second = attachmentFormFixture({ attachmentForm: "", task: "t-0002", project: "p-alpha" });

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // The second uploader targets a different task, so its busy identity is
  // distinct and it is not suppressed by the first form's in-flight upload.
  const secondHandled = handleFormSubmit(app, { target: second.form, preventDefault() {} });
  assert.equal(requests, 2, "a distinct task target is not blocked by the in-flight upload");
  assert.equal(second.submitButton.disabled, true);
  assert.equal(first.submitButton.disabled, true, "the first form stays busy while its upload is in flight");

  resolvers[0]();
  resolvers[1]();
  await firstHandled;
  await secondHandled;
  assert.equal(inFlight.size, 0);
});

test("a poll re-render replacing an attachment form cannot re-enable a duplicate submission", async () => {
  await scriptContext();
  const app = statusApp();
  let requests = 0;
  let resolveRequest;
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolveRequest = () => resolve({ ok: true, json: () => Promise.resolve({}) });
    });
  };
  const first = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });

  const handled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);

  // The task view repaints on a 10 s poll and swaps the form node for a fresh
  // one carrying the same target identity; its submit control starts enabled.
  const replacement = attachmentFormFixture({ attachmentForm: "", task: "t-0001", project: "p-alpha" });
  assert.equal(replacement.submitButton.disabled, false);

  // Submitting the replacement while the first upload is in flight must not
  // issue a second request: the guard lives in the in-flight registry keyed by
  // target identity, not on the (now discarded) node.
  await handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 1, "no duplicate request while the first is in flight");

  resolveRequest();
  await handled;
  assert.equal(inFlight.size, 0);

  // Once the first upload settles, the same target can submit again.
  const second = handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 2);
  resolveRequest();
  await second;
  assert.equal(inFlight.size, 0);
});

test("formBusyKey gives concurrent task forms distinct, stable busy identities", () => {
  // A boolean data-attachment-form carries an empty primary value, so two
  // uploaders on different tasks must not collapse onto one key.
  assert.notEqual(
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0002", project: "p-alpha" } }),
  );
  // The same target is stable across a repaint that rebuilds the dataset.
  assert.equal(
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
    formBusyKey("attachmentForm", { dataset: { attachmentForm: "", task: "t-0001", project: "p-alpha" } }),
  );
  // Edit forms for two tasks, and a create form (empty data-task-form) versus
  // an edit form, each get their own identity so concurrent forms do not
  // suppress one another.
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" } }),
    formBusyKey("taskForm", { dataset: { taskForm: "t-0002", taskFormMode: "edit", project: "p-alpha" } }),
  );
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" } }),
  );
  // A create form rendered with several projects carries no data-project; its
  // mutation target is the selected project, so two creates for different
  // projects must not collapse onto `form:taskForm::`.
  assert.notEqual(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-beta" } } }),
  );
  // The selected-project identity is stable across a repaint that rebuilds the
  // form node with the same selection.
  assert.equal(
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
    formBusyKey("taskForm", { dataset: { taskForm: "", taskFormMode: "create" }, elements: { project: { value: "p-alpha" } } }),
  );
});

test("a create task form and an edit task form submit concurrently", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) }));
    });
  };
  const createSubmit = new ActionButton({});
  const createForm = {
    tagName: "FORM",
    // A create form rendered with several projects carries no data-project; the
    // mutation target is the selected project in the project <select>.
    dataset: { taskForm: "", taskFormMode: "create" },
    elements: {
      project: { value: "p-alpha" },
      priority: { value: "1" },
      flow_id: { value: "fl-coding" },
      title: { value: "New task" },
      body: { value: "Body" },
      attachments: { files: [] },
      queue_task: { checked: false },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? createSubmit : null;
    },
  };
  const editSubmit = new ActionButton({});
  const editForm = {
    tagName: "FORM",
    dataset: { taskForm: "t-0001", taskFormMode: "edit", project: "p-alpha" },
    elements: {
      priority: { value: "1" },
      title: { value: "Renamed" },
      body: { value: "Body" },
      flow_id: { value: "fl-coding" },
    },
    reportValidity() {
      return true;
    },
    querySelector(selector) {
      return selector === '[type="submit"]' ? editSubmit : null;
    },
  };

  const createHandled = handleFormSubmit(app, { target: createForm, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(createSubmit.disabled, true);

  // The edit form targets an existing task, so its busy identity differs from
  // the create form's and it is not suppressed by the in-flight create.
  const editHandled = handleFormSubmit(app, { target: editForm, preventDefault() {} });
  assert.equal(requests, 2, "a concurrent edit form is not blocked by the in-flight create");
  assert.equal(editSubmit.disabled, true);

  resolvers[0]();
  resolvers[1]();
  await createHandled;
  await editHandled;
  assert.equal(inFlight.size, 0);
});

test("multi-project create forms for different projects submit concurrently", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({ task: { id: "t-alpha-0001" } }) }));
    });
  };
  // The real multi-project create form shape from renderTaskFormView: no
  // data-project, with the mutation target in the project <select>. Two such
  // forms selected for different projects must not share `form:taskForm::`.
  function createFormFixture(projectID) {
    const submitButton = new ActionButton({});
    return {
      submitButton,
      form: {
        tagName: "FORM",
        dataset: { taskForm: "", taskFormMode: "create" },
        elements: {
          project: { value: projectID },
          priority: { value: "1" },
          flow_id: { value: "fl-coding" },
          title: { value: "New task" },
          body: { value: "Body" },
          attachments: { files: [] },
          queue_task: { checked: false },
        },
        reportValidity() {
          return true;
        },
        querySelector(selector) {
          return selector === '[type="submit"]' ? submitButton : null;
        },
      },
    };
  }
  const first = createFormFixture("p-alpha");
  const second = createFormFixture("p-beta");

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // The second create targets a different project, so its busy identity is
  // distinct and it is not suppressed by the first form's in-flight create.
  const secondHandled = handleFormSubmit(app, { target: second.form, preventDefault() {} });
  assert.equal(requests, 2, "a distinct project create is not blocked by the in-flight create");
  assert.equal(second.submitButton.disabled, true);
  assert.equal(first.submitButton.disabled, true, "the first form stays busy while its create is in flight");

  resolvers[0]();
  resolvers[1]();
  await firstHandled;
  await secondHandled;
  assert.equal(inFlight.size, 0);
});

test("attention reply busy keys scope to question, task, and project", () => {
  const base = { attentionReplyForm: "t-alpha-0001", task: "t-alpha-0001", project: "p-alpha", statusLogId: "7" };
  const questionA = formBusyKey("attentionReplyForm", { dataset: base });
  assert.equal(questionA, "form:attentionReplyForm:p-alpha:t-alpha-0001:7");
  // A different pending question on the same task is a different busy target.
  assert.notEqual(questionA, formBusyKey("attentionReplyForm", { dataset: { ...base, statusLogId: "9" } }));
  // Project/task isolation is unchanged for attention replies.
  assert.notEqual(questionA, formBusyKey("attentionReplyForm", { dataset: { ...base, task: "t-alpha-0002" } }));
  assert.notEqual(questionA, formBusyKey("attentionReplyForm", { dataset: { ...base, project: "p-beta" } }));
  // A form without a status-log id still keys on project and task alone, and
  // other forms' keys are untouched.
  assert.equal(
    formBusyKey("attentionReplyForm", { dataset: { attentionReplyForm: "t-alpha-0001", project: "p-alpha" } }),
    "form:attentionReplyForm:p-alpha:t-alpha-0001:",
  );
  assert.equal(formBusyKey("taskForm", { dataset: { taskForm: "t-alpha-0001", project: "p-alpha" } }), "form:taskForm:p-alpha:t-alpha-0001");
  assert.equal(formBusyKey("threadReplyForm", { dataset: { threadReplyForm: "th-1", project: "p-alpha" } }), "form:threadReplyForm:p-alpha:th-1");
});

test("attention reply forms for different questions on one task submit concurrently", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  // The review panel renders both data-task and the attribute; the attention
  // panel renders only the attribute with the same value.
  function replyForm(statusLogID) {
    const submitButton = new ActionButton({});
    return {
      submitButton,
      form: {
        tagName: "FORM",
        dataset: {
          attentionReplyForm: "t-alpha-0001",
          task: "t-alpha-0001",
          project: "p-alpha",
          statusLogId: String(statusLogID),
        },
        elements: { message: { value: `reply ${statusLogID}` } },
        reportValidity() {
          return true;
        },
        querySelector(selector) {
          return selector === '[type="submit"]' ? submitButton : null;
        },
      },
    };
  }
  const first = replyForm(7);
  const second = replyForm(9);

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // The second question is a different status-log target, so its reply is not
  // suppressed by the in-flight reply to the first.
  const secondHandled = handleFormSubmit(app, { target: second.form, preventDefault() {} });
  assert.equal(requests, 2, "a reply to a different question is not blocked by the in-flight reply");
  assert.equal(second.submitButton.disabled, true);

  resolvers[0]();
  resolvers[1]();
  await firstHandled;
  await secondHandled;
  assert.equal(inFlight.size, 0);
});

test("a replacement attention reply form for the same question stays suppressed", async () => {
  await scriptContext({}, {
    history: { pushState() {} },
  });
  const app = statusApp();
  app.load = async () => {};
  let requests = 0;
  const resolvers = [];
  globalThis.fetch = () => {
    requests += 1;
    return new Promise((resolve) => {
      resolvers.push(() => resolve({ ok: true, json: () => Promise.resolve({}) }));
    });
  };
  function replyForm() {
    const submitButton = new ActionButton({});
    return {
      submitButton,
      form: {
        tagName: "FORM",
        dataset: {
          attentionReplyForm: "t-alpha-0001",
          task: "t-alpha-0001",
          project: "p-alpha",
          statusLogId: "7",
        },
        elements: { message: { value: "same reply" } },
        reportValidity() {
          return true;
        },
        querySelector(selector) {
          return selector === '[type="submit"]' ? submitButton : null;
        },
      },
    };
  }
  const first = replyForm();
  const replacement = replyForm();

  const firstHandled = handleFormSubmit(app, { target: first.form, preventDefault() {} });
  assert.equal(requests, 1);
  assert.equal(first.submitButton.disabled, true);

  // A repaint replaces the form node, but the busy identity is derived from
  // the data attributes, so the replacement still collides with the in-flight
  // reply to the same status-log question.
  const duplicateHandled = handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 1, "a replacement form for the same question stays suppressed");
  assert.equal(replacement.submitButton.disabled, false);

  resolvers[0]();
  await firstHandled;
  await duplicateHandled;
  assert.equal(inFlight.size, 0);

  // Once the first request settles, the replacement can submit.
  const retryHandled = handleFormSubmit(app, { target: replacement.form, preventDefault() {} });
  assert.equal(requests, 2, "the replacement submits after the in-flight reply settles");
  resolvers[1]();
  await retryHandled;
  assert.equal(inFlight.size, 0);
});

