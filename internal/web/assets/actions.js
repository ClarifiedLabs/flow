// One delegated action table for the whole app.
//
// Every interactive control carries a data-* attribute naming what it does and
// holding its primary id; `data-project` scopes the call. A single listener on
// <flow-app> dispatches them. That is what replaces the old re-query-and-attach
// pass after every repaint: elements now own their own innerHTML, so listeners
// attached to their children would not survive a poll, while a listener on the
// app root does.

import { apiDelete, apiGet, apiPatch, apiPost, taskAPIBase, taskConsoleAPIPath } from "./api.js";

const workflowPath = (dataset, id, suffix = "") =>
  `${taskAPIBase(dataset.project)}/${encodeURIComponent(id)}${suffix}`;

// ACTIONS maps a dataset key to what pressing that control does. Handlers
// receive the app (for status and refresh), the element, and its dataset.
export const ACTIONS = {
  async workflowSchedule(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowSchedule, "/schedule"), {});
    await app.refresh();
  },

  async workflowReset(app, element, dataset) {
    if (!window.confirm("Cancel this workflow run and return the task to Unscheduled?")) return;
    await apiPost(workflowPath(dataset, dataset.workflowReset, "/reset"), {});
    await app.refresh();
  },

  async workflowDone(app, element, dataset) {
    const resolution = (
      window.prompt("Done resolution: completed, rejected, abandoned, cancelled, or failed", "completed") || ""
    ).trim();
    if (!resolution) return;
    await apiPost(workflowPath(dataset, dataset.workflowDone, "/done"), { resolution });
    await app.refresh();
  },

  async workflowReopen(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowReopen, "/reopen"), {});
    await app.refresh();
  },

  async workflowRespond(app, element, dataset) {
    const feedback = String(
      element.closest("[data-gate-panel]")?.querySelector("[data-workflow-feedback]")?.value || "",
    ).trim();
    await apiPost(workflowPath(dataset, dataset.task, "/workflow/respond"), {
      node_run_id: dataset.workflowRespond,
      outcome: dataset.outcome || "",
      feedback,
    });
    await app.refresh();
  },

  async workflowBudget(app, element, dataset) {
    const reviewCycles = dataset.workflowBudgetKind === "review-cycles";
    const additional = Number(
      window.prompt(reviewCycles ? "Additional review-author cycles" : "Additional workflow transitions", reviewCycles ? "5" : "50"),
    );
    if (!Number.isInteger(additional) || additional < 1) return;
    await apiPost(workflowPath(dataset, dataset.workflowBudget, "/workflow/budget"), { additional });
    await app.refresh();
  },

  async workflowRetry(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowRetry, "/workflow/retry"), {
      refresh_agent_runtime: dataset.workflowRetryRefresh === "true",
    });
    await app.refresh();
  },

  async workflowSkip(app, element, dataset) {
    if (!window.confirm("Skip this workflow step and continue?")) return;
    await apiPost(workflowPath(dataset, dataset.workflowSkip, "/workflow/skip"), {
      node_run_id: dataset.workflowSkipNode || "",
    });
    await app.refresh();
  },

  // Hold and release are the operator's grip on a run. Holding stops the
  // executor advancing it; releasing names which edge to take on the way out.
  async workflowHold(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowHold, "/workflow/hold"), {});
    app.setStatus(`${dataset.workflowHold} is held by you — the workflow will not advance`);
    await app.refresh();
  },

  async workflowTakeOver(app, element, dataset) {
    const taskID = dataset.workflowTakeOver;
    await apiPost(workflowPath(dataset, taskID, "/workflow/hold"), {});
    // Take over additionally opens (or attaches to) an interactive session, so
    // there is somewhere to actually do the work.
    try {
      await apiPost(taskConsoleAPIPath(dataset.project, taskID), { harness: "shell" });
    } catch (error) {
      app.setStatus(error.message || String(error));
    }
    app.setStatus(`Opened an interactive session on ${taskID}`);
    await app.refresh();
  },

  async workflowRelease(app, element, dataset) {
    await apiPost(workflowPath(dataset, dataset.workflowRelease, "/workflow/release"), {
      edge: dataset.edge || "resume",
      artifact_id: dataset.artifactId || "",
    });
    app.setStatus(releaseMessage(dataset.workflowRelease, dataset.edge));
    await app.refresh();
  },

  async attentionMerge(app, element, dataset) {
    const taskID = dataset.attentionMerge;
    const change = await resolveReadyChange(dataset.project, taskID);
    if (!change) {
      app.setStatus(`${taskID} has no ready change to merge`);
      return;
    }
    await apiPost(`/v2/changes/${encodeURIComponent(change)}/merge`, {});
    app.setStatus(`Merged ${taskID}`);
    await app.refresh();
  },

  async cardMerge(app, element, dataset) {
    await ACTIONS.attentionMerge(app, element, { ...dataset, attentionMerge: dataset.cardMerge });
  },

  async cardApprove(app, element, dataset) {
    const taskID = dataset.cardApprove;
    const detail = await apiGet(workflowPath(dataset, taskID, "/workflow")).catch(() => null);
    const nodeRunID = detail?.detail?.run?.current_node_run_id || detail?.detail?.open_wait?.node_run_id || "";
    const outcome = firstGateOutcome(detail);
    if (!nodeRunID || !outcome) {
      app.setStatus(`${taskID} has no open gate to approve`);
      return;
    }
    await apiPost(workflowPath(dataset, taskID, "/workflow/respond"), { node_run_id: nodeRunID, outcome });
    await app.refresh();
  },

  async taskEdit(app, element, dataset) {
    const nextTitle = window.prompt("Title", dataset.taskTitle || "");
    if (nextTitle === null) return;
    const title = nextTitle.trim();
    if (!title) {
      app.setStatus("Task title is required");
      return;
    }
    await apiPatch(workflowPath(dataset, dataset.taskEdit), { title });
    await app.refresh();
  },

  async mergeChange(app, element, dataset) {
    await apiPost(`/v2/changes/${encodeURIComponent(dataset.mergeChange)}/merge`, {});
    await app.refresh();
  },

  async humanReviewApprove(app, element, dataset) {
    await apiPost(`${workflowPath(dataset, dataset.humanReviewApprove)}/checks/${encodeURIComponent(dataset.checkName)}`, {
      kind: "human",
      required: true,
      verdict: "satisfied",
      reporter: "human",
    });
    await app.refresh();
  },

  async startTaskConsole(app, element, dataset) {
    await apiPost(taskConsoleAPIPath(dataset.project, dataset.startTaskConsole), { harness: "claude" });
    app.setStatus("task console starting");
    await app.refresh();
  },

  async releaseTaskConsole(app, element, dataset) {
    await apiDelete(taskConsoleAPIPath(dataset.project, dataset.releaseTaskConsole));
    app.setStatus("task console released");
    await app.refresh();
  },

  async threadClaim(app, element, dataset) {
    const body = dataset.claimKind === "fixed" ? "" : (window.prompt("Why?") || "").trim();
    if (dataset.claimKind !== "fixed" && !body) return;
    await apiPost(`/v2/threads/${encodeURIComponent(dataset.threadClaim)}/claims`, {
      kind: dataset.claimKind,
      body,
    });
    await app.refresh();
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

function firstGateOutcome(detail) {
  const snapshot = detail?.detail?.run?.snapshot || {};
  const key = detail?.detail?.run?.current_node_key || "";
  const node = (snapshot.nodes || []).find((candidate) => candidate.key === key);
  return node?.config?.human_gate?.outcomes?.[0] || "";
}

async function resolveReadyChange(projectID, taskID) {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}`).catch(() => null);
  const detail = data?.task_detail || data?.TaskDetail || {};
  const ready = detail.ready_change || detail.ReadyChange;
  return ready?.id || ready?.ID || "";
}

// handleAction finds the nearest ancestor carrying a known action attribute and
// runs it. Returns true when it handled the event.
export async function handleAction(app, event) {
  const element = event.target?.closest?.("[data-action-key]") || findActionElement(event.target);
  if (!element) return false;
  const key = actionKeyFor(element);
  if (!key) return false;

  event.preventDefault?.();
  if (element.disabled) return true;
  element.disabled = true;
  try {
    await ACTIONS[key](app, element, element.dataset);
  } catch (error) {
    app.setStatus(error.message || String(error));
  } finally {
    element.disabled = false;
  }
  return true;
}

function findActionElement(target) {
  let node = target;
  while (node && node.dataset) {
    if (actionKeyFor(node)) return node;
    node = node.parentElement;
  }
  return null;
}

function actionKeyFor(element) {
  for (const key of Object.keys(element.dataset || {})) {
    if (Object.hasOwn(ACTIONS, key)) return key;
  }
  return "";
}
