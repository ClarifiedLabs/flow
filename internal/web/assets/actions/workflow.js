// Workflow run lifecycle handlers: schedule/reset/done/reopen, gate
// responses, budgets, retries, holds, and take-over/release.

import { apiPost, taskConsoleAPIPath } from "../api.js";
import {
  budgetDefaults,
  budgetKindName,
  budgetModalResult,
  budgetValidationError,
  openBudgetModal,
  renderBudgetModalInto,
} from "../budget-modal.js";
import { CANCELLED, failureMessage, workflowPath } from "./dispatch.js";

export const workflowActions = {
  async lifecycleTransition(app, element, dataset) {
    const id = String(dataset.lifecycleTransition || "").trim();
    if (!id) return "Select a task first";
    const container = element.closest?.("[data-lifecycle-control]") || document.querySelector?.("[data-lifecycle-control]");
    const select = container?.querySelector?.("[data-lifecycle-target]");
    const target = String(select?.value || "").trim();
    if (!target) return CANCELLED;
    const force = Boolean(container?.querySelector?.("[data-lifecycle-force]")?.checked);
    const note = String(container?.querySelector?.("[data-lifecycle-note]")?.value || "").trim();
    const nodeRunID = String(dataset.nodeRunId || "").trim();
    const confirmText = target.startsWith("done:") || target === "done" || target === "completed" || target === "rejected" || target === "abandoned" || target === "cancelled" || target === "failed"
      ? `Mark ${id} as ${target.replace(/^done:/, "")} ?`
      : target === "reset" || target === "backlog" || target === "unscheduled"
        ? `Reset ${id} to unscheduled?`
        : target === "reopen"
          ? `Reopen ${id}?`
          : `Transition ${id} to ${target}?`;
    if (!window.confirm(confirmText)) return CANCELLED;
    const body = { target, ...(note ? { note } : {}), ...(force ? { force: true } : {}), ...(nodeRunID ? { node_run_id: nodeRunID } : {}) };
    const idempotencyKey = globalThis.crypto?.randomUUID?.() || `lifecycle-${Date.now()}-${Math.random()}`;
    await apiPost(workflowPath(dataset, id, "/lifecycle/transition"), body, { idempotencyKey });
    await app.refresh();
    return `Transitioned ${id} to ${target}`;
  },

  async workflowSchedule(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowSchedule, "/schedule"), {});
    await app.refresh();
    return "Scheduled";
  },
  async workflowReset(app, element, dataset) {
    if (!window.confirm("Cancel this workflow run and return the task to Unscheduled?")) return CANCELLED;
    await apiPost(workflowPath(dataset, dataset.workflowReset, "/reset"), {});
    await app.refresh();
    return "Reset to unscheduled";
  },

  async workflowDone(app, element, dataset) {
    const resolution = (
      window.prompt("Done resolution: completed, rejected, abandoned, cancelled, or failed", "completed") || ""
    ).trim();
    if (!resolution) return CANCELLED;
    await apiPost(workflowPath(dataset, dataset.workflowDone, "/done"), { resolution });
    await app.refresh();
    return `Marked done: ${resolution}`;
  },

  async workflowReopen(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowReopen, "/reopen"), {});
    await app.refresh();
    return "Reopened";
  },

  async workflowRespond(app, element, dataset) {
    const reviewWaitID = String(dataset.reviewWait || "").trim();
    if (!reviewWaitID) return "This review wait is no longer actionable";
    const feedback = String(
      element.closest("[data-gate-panel]")?.querySelector("[data-workflow-feedback]")?.value || "",
    ).trim();
    // review_wait_id binds the response to the review round that was rendered:
    // an interactive changes_requested round reopens a fresh wait on the same
    // node run, so a response posted from a stale panel must not resolve the
    // newer round. The server re-asserts the binding under the review lock.
    await apiPost(workflowPath(dataset, dataset.task, "/workflow/respond"), {
      node_run_id: dataset.workflowRespond,
      review_wait_id: reviewWaitID,
      outcome: dataset.outcome || "",
      feedback,
    });
    await app.refresh();
    return "Feedback sent";
  },

  // gateComment records plan feedback without a verdict. When the reviewed
  // agent is still in its session the comment is queued into it, so the
  // reviewer can discuss the plan before deciding.
  async gateComment(app, element, dataset) {
    const textarea = element.closest("[data-gate-panel]")?.querySelector("[data-workflow-feedback]");
    const message = String(textarea?.value || "").trim();
    if (!message) return "Write a comment first";
    const result = await apiPost(workflowPath(dataset, dataset.gateComment, "/workflow/comment"), { message });
    if (textarea) textarea.value = "";
    await app.refresh();
    return result?.queued ? "Comment sent to the agent" : "Comment recorded";
  },

  // workflowBudget opens the modal that collects the extension. The POST is
  // rejected server-side without operator instructions — they are both the
  // recorded rationale and the payload the next author session reads — so a
  // bare window.prompt could never complete this operation from the GUI.
  // Opening is not a mutation: the handler returns CANCELLED so the dispatcher
  // clears the pending label without a request having gone out.
  async workflowBudget(app, element, dataset) {
    const taskID = String(dataset.workflowBudget || "").trim();
    if (!taskID) return CANCELLED;
    // The modal mounts at app level (.main), which polls never rewrite: the
    // originating card repaints its own innerHTML, so a form inside it would
    // be destroyed mid-edit. The control lives inside <flow-app>, so that is
    // the root; the document covers any detached-control edge case.
    const root = element?.closest?.("flow-app") || globalThis.document;
    openBudgetModal(root, budgetDefaults({
      taskID,
      projectID: dataset.project,
      kind: dataset.workflowBudgetKind,
      used: dataset.workflowBudgetUsed,
      total: dataset.workflowBudgetTotal,
    }));
    return CANCELLED;
  },

  // budgetGrant submits the open modal. Validation is local (integer 1..500,
  // non-blank instructions) so the server's contract can never surface as a
  // cryptic 400; a rejection renders inline, keeps the typed input, and
  // propagates so the status bar agrees with the modal. On success the run's
  // authoritative totals come off the POST response, so no extra GET is
  // needed — the refresh that follows tears the layer down and this handler
  // re-renders the disposition view right after it settles.
  async budgetGrant(app, element, dataset) {
    const layer = element?.closest?.("[data-budget-modal-layer]");
    if (!layer) return "The budget prompt is no longer open";
    // The host outlives the layer: app.refresh() re-runs the route, whose load
    // tears the modal down, so the disposition view is re-rendered into the
    // still-live host from the POST response the server already sent.
    const host = layer.parentElement;
    const taskID = String(dataset.budgetGrant || "").trim();
    const projectID = String(dataset.project || "").trim();
    const kind = budgetKindName(layer.dataset.budgetKind);
    const additionalRaw = layer.querySelector("[data-budget-additional]")?.value;
    const instructionsRaw = layer.querySelector("[data-budget-instructions]")?.value;
    const additional = Number(additionalRaw);
    const instructions = String(instructionsRaw || "").trim();
    const invalid = budgetValidationError(additionalRaw, instructions);
    if (invalid) {
      renderBudgetModalInto(host, {
        taskID,
        projectID,
        kind,
        additional: additionalRaw,
        instructions: instructionsRaw,
        error: invalid,
      });
      return invalid;
    }
    // Flag the layer as pending for the dialog's own cancel (Escape) handler:
    // a dismissal mid-flight would drop the disposition this request is about
    // to render. The re-renders below clear it.
    layer.dataset.budgetPending = "true";
    let result;
    try {
      result = await apiPost(workflowPath(dataset, taskID, "/workflow/budget"), { additional, instructions });
    } catch (error) {
      // Keep exactly what the operator typed so a fixable rejection — a
      // ceiling breach, a concurrent grant's 409 — is one edit from a retry.
      const message = failureMessage(error);
      renderBudgetModalInto(host, {
        taskID,
        projectID,
        kind,
        additional,
        instructions,
        error: message,
      });
      throw new Error(message);
    }
    await app.refresh();
    renderBudgetModalInto(host, {
      taskID,
      projectID,
      kind,
      result: budgetModalResult(result?.run, additional, kind),
    });
    return kind === "review-cycles" ? `Granted ${additional} review cycles` : `Extended budget by ${additional}`;
  },

  async workflowRetry(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowRetry, "/workflow/retry"), {
      refresh_agent_runtime: dataset.workflowRetryRefresh === "true",
    });
    await app.refresh();
    return "Retry queued";
  },

  async workflowSkip(app, element, dataset) {
    if (!window.confirm("Skip this workflow step and continue?")) return CANCELLED;
    await apiPost(workflowPath(dataset, dataset.workflowSkip, "/workflow/skip"), {
      node_run_id: dataset.workflowSkipNode || "",
    });
    await app.refresh();
    return "Step skipped";
  },

  // Hold and release are the operator's grip on a run. Holding stops the
  // executor advancing it; releasing names which edge to take on the way out.
  async workflowHold(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowHold, "/workflow/hold"), {});
    await app.refresh();
    return `${dataset.workflowHold} is held by you — the workflow will not advance`;
  },
  async workflowTakeOver(app, element, dataset) {
    const taskID = dataset.workflowTakeOver;
    await apiPost(workflowPath(dataset, taskID, "/workflow/hold"), {});
    // Take over additionally opens (or attaches to) an interactive session, so
    // there is somewhere to actually do the work.
    let sessionError = "";
    try {
      await apiPost(taskConsoleAPIPath(dataset.project, taskID), { harness: "shell" });
    } catch (error) {
      // failureMessage is total even for a hostile rejection value, so the
      // session note never aborts the take-over settlement.
      sessionError = failureMessage(error);
    }
    await app.refresh();
    return sessionError || `Opened an interactive session on ${taskID}`;
  },

  async workflowRelease(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowRelease, "/workflow/release"), {
      edge: dataset.edge || "resume",
      artifact_id: dataset.artifactId || "",
    });
    await app.refresh();
    return releaseMessage(dataset.workflowRelease, dataset.edge);
  },
};

function releaseMessage(taskID, edge) {
  switch (edge) {
    case "submit":
      return `Sent ${taskID} on with your artifact`;
    case "satisfy":
      return `Marked the current step done on ${taskID}`;
    case "merge":
      return `Skipped ${taskID} to merge`;
    default:
      return `Resumed ${taskID}`;
  }
}
