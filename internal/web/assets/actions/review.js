// Review handlers: convergence and scope decisions, owner rulings, gate
// approval from cards, human-review checks, merges, and title edits.

import { apiGet, apiPatch, apiPost, taskAPIBase } from "../api.js";
import { parseWaitDetails } from "../task-model.js";
import { CANCELLED, workflowPath } from "./dispatch.js";

export const reviewActions = {
  async convergenceRequest(app, element, dataset) {
    const taskID = dataset.convergenceRequest;
    await apiPost(workflowPath(dataset, taskID, "/workflow/convergence/request"), {});
    await app.refresh();
    return `Convergence review started for ${taskID}`;
  },

  async ownerRuling(app, element, dataset) {
    const taskID = dataset.ownerRuling;
    const panel = element.closest?.("[data-owner-ruling-panel]");
    const body = String(panel?.querySelector("[data-owner-ruling-body]")?.value || "").trim();
    const supersedesID = String(panel?.querySelector("[data-owner-ruling-supersedes]")?.value || "").trim();
    if (!body) throw new Error("Enter owner guidance before recording a ruling");
    const idempotencyKey = globalThis.crypto?.randomUUID?.() || `owner-ruling-${Date.now()}-${Math.random()}`;
    const result = await apiPost(
      workflowPath(dataset, taskID, "/workflow/rulings"),
      { body, ...(supersedesID ? { supersedes_id: supersedesID } : {}) },
      { idempotencyKey },
    );
    await app.refresh();
    const rulingID = result?.ruling?.ruling_id || result?.Ruling?.RulingID || "";
    return rulingID ? `Recorded owner ruling ${rulingID}` : `Recorded owner ruling for ${taskID}`;
  },
  async convergenceDecision(app, element, dataset) {
    const taskID = dataset.convergenceDecision;
    const disposition = dataset.disposition || "";
    if (disposition === "cancel" && !window.confirm("Cancel this oversized implementation? Its Git evidence will remain available.")) {
      return CANCELLED;
    }
    if (disposition === "promote" && !window.confirm("Promote this implementation to a clean-base feature planning workflow?")) {
      return CANCELLED;
    }
    const note = String(
      element.closest?.("[data-convergence-panel]")?.querySelector("[data-convergence-note]")?.value || "",
    ).trim();
    if (disposition === "return_to_author" && !note) {
      throw new Error("A decision note is required to return the change to the author");
    }
    await apiPost(workflowPath(dataset, taskID, "/workflow/convergence"), {
      disposition,
      expected_evidence_fingerprint: dataset.evidenceFingerprint || "",
      ...(note ? { note } : {}),
    });
    await app.refresh();
    switch (disposition) {
      case "accept_scope":
        return `Continuing ${taskID} as-is`;
      case "repair_branch":
        return `Opened ${taskID} for branch repair`;
      case "return_to_author":
        return `Returned ${taskID} to the author`;
      case "promote":
        return `Promoting ${taskID} to a feature`;
      case "cancel":
        return `Cancelled ${taskID}`;
      default:
        return `Resolved convergence review for ${taskID}`;
    }
  },

  async reviewScopeDecision(app, element, dataset) {
    const taskID = dataset.reviewScopeDecision;
    const waitID = dataset.waitId || "";
    const guidance = String(
      element.closest?.("[data-review-scope-panel]")?.querySelector("[data-review-scope-guidance]")?.value || "",
    ).trim();
    const idempotencyKey = globalThis.crypto?.randomUUID?.() || `review-decision-${Date.now()}-${Math.random()}`;
    await apiPost(
      workflowPath(dataset, taskID, `/workflow/review-scope-decisions/${encodeURIComponent(waitID)}/resolve`),
      { choice: dataset.choice || "", ...(guidance ? { guidance } : {}) },
      { idempotencyKey },
    );
    await app.refresh();
    return `Resolved review scope decision for ${taskID}`;
  },

  async attentionMerge(app, element, dataset) {
    const taskID = dataset.attentionMerge;
    const change = await resolveReadyChange(dataset.project, taskID);
    if (!change) return `${taskID} has no ready change to merge`;
    await apiPost(`/v2/changes/${encodeURIComponent(change)}/merge`, {});
    await app.refresh();
    return `Merged ${taskID}`;
  },

  async cardMerge(app, element, dataset) {
    return reviewActions.attentionMerge(app, element, { ...dataset, attentionMerge: dataset.cardMerge });
  },

  async cardApprove(app, element, dataset) {
    const taskID = dataset.cardApprove;
    const detail = await apiGet(workflowPath(dataset, taskID, "/workflow")).catch(() => null);
    const wait = detail?.detail?.open_wait;
    const nodeRunID = String(wait?.node_run_id || "").trim();
    const reviewWaitID = String(wait?.id || "").trim();
    const outcome = firstGateOutcome(detail);
    if (!nodeRunID || !reviewWaitID || !outcome) return `${taskID} has no actionable open gate to approve`;
    await apiPost(workflowPath(dataset, taskID, "/workflow/respond"), {
      node_run_id: nodeRunID,
      // review_wait_id binds the card approval to the review round this
      // detail fetch observed: another reviewer may resolve that round and
      // reopen a fresh wait on the same node run before the POST lands, and
      // the server then rejects the stale approval instead of deciding the
      // newer round.
      review_wait_id: reviewWaitID,
      outcome,
    });
    await app.refresh();
    return "Approved";
  },

  async taskEdit(app, element, dataset) {
    const nextTitle = window.prompt("Title", dataset.taskTitle || "");
    if (nextTitle === null) return CANCELLED;
    const title = nextTitle.trim();
    if (!title) return "Task title is required";
    await apiPatch(workflowPath(dataset, dataset.taskEdit), { title });
    await app.refresh();
    return "Title updated";
  },

  async mergeChange(app, element, dataset) {
    await apiPost(`/v2/changes/${encodeURIComponent(dataset.mergeChange)}/merge`, {});
    await app.refresh();
    return "Merged";
  },

  async humanReviewApprove(app, element, dataset) {
    await apiPost(`${workflowPath(dataset, dataset.humanReviewApprove)}/checks/${encodeURIComponent(dataset.checkName)}`, {
      kind: "human",
      required: true,
      verdict: "satisfied",
      reporter: "human",
    });
    await app.refresh();
    return "Check satisfied";
  },
};

function firstGateOutcome(detail) {
  // The frozen wait is authoritative. Do not reconstruct outcomes from the
  // current graph: a malformed or legacy wait is not actionable.
  return parseWaitDetails(detail?.detail?.open_wait)?.outcomes?.[0] || "";
}

async function resolveReadyChange(projectID, taskID) {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}`).catch(() => null);
  const detail = data?.task_detail || data?.TaskDetail || {};
  const ready = detail.ready_change || detail.ReadyChange;
  return ready?.id || ready?.ID || "";
}
