// One delegated action table for the whole app.
//
// Every interactive control carries a data-* attribute naming what it does and
// holding its primary id; `data-project` scopes the call. A single listener on
// <flow-app> dispatches them. That is what replaces the old re-query-and-attach
// pass after every repaint: elements now own their own innerHTML, so listeners
// attached to their children would not survive a poll, while a listener on the
// app root does.
//
// Every mutating control also gets an immediate pending state: handleAction
// disables the control, marks it aria-busy, and names the in-flight action on
// the status line synchronously on click. In-flight actions are tracked in a
// registry keyed by action and target — not by DOM node — so a poll re-render
// that replaces the button mid-flight cannot re-enable a duplicate submission.

import { apiDelete, apiGet, apiPatch, apiPost, taskAPIBase, taskConsoleAPIPath } from "./api.js";
import { parseWaitDetails } from "./task-model.js";

const workflowPath = (dataset, id, suffix = "") =>
  `${taskAPIBase(dataset.project)}/${encodeURIComponent(id)}${suffix}`;

// A handler returns CANCELLED when the user backed out of a confirm/prompt
// dialog: handleAction then restores the control and clears the pending label
// it wrote on click, so no stale "Resetting t-0001…" lingers when no request
// went out. Any other string a handler returns is the confirmation message
// handleAction shows *after* the handler's own refresh, so it survives the
// re-render (routes call setTitle, which clears the status line).
export const CANCELLED = Symbol("cancelled");

// ACTIONS maps a dataset key to what pressing that control does. Handlers
// receive the app (for status and refresh), the element, and its dataset, and
// return the confirmation message for the status line (or CANCELLED).
export const ACTIONS = {
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
    const feedback = String(
      element.closest("[data-gate-panel]")?.querySelector("[data-workflow-feedback]")?.value || "",
    ).trim();
    await apiPost(workflowPath(dataset, dataset.task, "/workflow/respond"), {
      node_run_id: dataset.workflowRespond,
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

  async workflowBudget(app, element, dataset) {
    const reviewCycles = dataset.workflowBudgetKind === "review-cycles";
    const additional = Number(
      window.prompt(reviewCycles ? "Additional review-author cycles" : "Additional workflow transitions", reviewCycles ? "2" : "50"),
    );
    if (!Number.isInteger(additional) || additional < 1) return CANCELLED;
    await apiPost(workflowPath(dataset, dataset.workflowBudget, "/workflow/budget"), { additional });
    await app.refresh();
    return reviewCycles ? `Granted ${additional} review cycles` : `Extended budget by ${additional}`;
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
      sessionError = error.message || String(error);
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

  async attentionMerge(app, element, dataset) {
    const taskID = dataset.attentionMerge;
    const change = await resolveReadyChange(dataset.project, taskID);
    if (!change) return `${taskID} has no ready change to merge`;
    await apiPost(`/v2/changes/${encodeURIComponent(change)}/merge`, {});
    await app.refresh();
    return `Merged ${taskID}`;
  },

  async cardMerge(app, element, dataset) {
    return ACTIONS.attentionMerge(app, element, { ...dataset, attentionMerge: dataset.cardMerge });
  },

  async cardApprove(app, element, dataset) {
    const taskID = dataset.cardApprove;
    const detail = await apiGet(workflowPath(dataset, taskID, "/workflow")).catch(() => null);
    const nodeRunID = detail?.detail?.run?.current_node_run_id || detail?.detail?.open_wait?.node_run_id || "";
    const outcome = firstGateOutcome(detail);
    if (!nodeRunID || !outcome) return `${taskID} has no open gate to approve`;
    await apiPost(workflowPath(dataset, taskID, "/workflow/respond"), { node_run_id: nodeRunID, outcome });
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

  async startTaskConsole(app, element, dataset) {
    await apiPost(taskConsoleAPIPath(dataset.project, dataset.startTaskConsole), { harness: "harness" });
    await app.refresh();
    return "Task console starting";
  },

  async releaseTaskConsole(app, element, dataset) {
    await apiDelete(taskConsoleAPIPath(dataset.project, dataset.releaseTaskConsole));
    await app.refresh();
    return "Task console released";
  },

  async threadClaim(app, element, dataset) {
    const body = dataset.claimKind === "fixed" ? "" : (window.prompt("Why?") || "").trim();
    if (dataset.claimKind !== "fixed" && !body) return CANCELLED;
    await apiPost(`/v2/threads/${encodeURIComponent(dataset.threadClaim)}/claims`, {
      kind: dataset.claimKind,
      body,
    });
    await app.refresh();
    return "Thread claimed";
  },

  // relationRemove unlinks one stored relation row. The button carries the row's
  // source task (the path) and the target/kind (the body), so it removes the
  // exact relation regardless of which side the viewed task sits on.
  async relationRemove(app, element, dataset) {
    await apiDelete(`${taskAPIBase(dataset.project)}/${encodeURIComponent(dataset.relationRemove)}/relations`, {
      target_task_id: dataset.target,
      kind: dataset.kind,
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
  // Interactive review waits carry the gate's outcomes with them — the
  // current node is the agent node then, so its own config has none.
  const fromWait = parseWaitDetails(detail?.detail?.open_wait).outcomes?.[0];
  if (fromWait) return fromWait;
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

// inFlight tracks running actions by identity, not by DOM node: the board
// repaints on a 10 s poll and a re-render replaces the button mid-flight, so a
// guard stored on the node would die with it and re-enable a duplicate
// submission. Forms and the review bar share this registry through the
// actionBusyKey helper.
export const inFlight = new Set();

// The status line is shared by every in-flight mutation, so settling one key
// must not clobber the pending label of another key that is still running.
// inFlightLabels records the pending label each key put on the line; when a
// key settles, settleStatus re-shows the newest label still in flight and only
// reveals the settling key's own result once nothing is pending. Map insertion
// order tracks start order, so the newest still-pending label is the last
// remaining entry.
const inFlightLabels = new Map();

// beginInFlight registers a key as running and remembers the pending label it
// displayed, so a later settlement can restore it while it is still in flight.
export function beginInFlight(key, label) {
  inFlight.add(key);
  inFlightLabels.set(key, label);
}

// endInFlight forgets a settled key and returns the pending label of the newest
// key still in flight ("" when nothing remains pending).
export function endInFlight(key) {
  inFlight.delete(key);
  inFlightLabels.delete(key);
  let pending = "";
  for (const label of inFlightLabels.values()) pending = label;
  return pending;
}

// settleStatus arbitrates the shared status line when a mutation finishes.
// While another mutation is still in flight it keeps that mutation's pending
// label visible and suppresses this key's outcome. Once this is the final
// mutation to settle, its outcome decides the line: a non-empty string (a
// confirmation, validation, or error message) is shown; an empty string — the
// explicit clear a backed-out handler asks for — clears it; and undefined, a
// silent success, leaves the pending label in place rather than blanking it.
export function settleStatus(app, key, message) {
  const stillPending = endInFlight(key);
  if (stillPending) {
    app?.setStatus?.(stillPending);
    return;
  }
  if (message === undefined) return;
  app?.setStatus?.(message);
}

export function actionKeyFor(element) {
  for (const key of Object.keys(element.dataset || {})) {
    if (Object.hasOwn(ACTIONS, key)) return key;
  }
  return "";
}

// actionBusyKey names an in-flight action: its table key plus the target id
// the dataset carries, so "schedule t-0001" stays blocked even when the
// button node carrying it is replaced. Some actions act on a sub-target the
// table key alone does not distinguish: a task can render several human
// checks at once, each approve button targeting a distinct data-check-name,
// so the busy identity includes the check name — keying on the task alone
// would mark every sibling check busy and suppress their independent
// approvals until the first request settles.
export function actionBusyKey(key, dataset) {
  const base = `${key}:${String(dataset?.[key] ?? "")}`;
  if (key === "humanReviewApprove") {
    const check = String(dataset?.checkName ?? "");
    return check ? `${base}:${check}` : base;
  }
  return base;
}

// markBusy gives a mutating control its synchronous pending state: disabled,
// aria-busy for assistive tech, and the is-busy class the stylesheets dim.
// Returns a restore function that puts the control back.
export function markBusy(element) {
  element.disabled = true;
  element.setAttribute?.("aria-busy", "true");
  element.classList?.add("is-busy");
  return () => {
    element.disabled = false;
    element.removeAttribute?.("aria-busy");
    element.classList?.remove("is-busy");
  };
}

// The outcome buttons for one gate all carry the same data-workflow-respond
// node-run id, so they share a single in-flight key. Marking only the clicked
// one busy leaves its siblings looking enabled until a repaint; suppress the
// whole set and hand back their restores so the pending state reads
// consistently across every outcome and unwinds on settle.
function gateOutcomeControls(element) {
  const scope = element.closest?.("[data-gate-panel]") ?? element.closest?.("[data-gate-node-run]");
  if (!scope?.querySelectorAll) return [];
  return [...scope.querySelectorAll("[data-workflow-respond]")];
}

function suppressGateOutcomes(element) {
  return gateOutcomeControls(element)
    .filter((control) => control !== element)
    .map((control) => markBusy(control));
}

// A poll repaint rebuilds the gate panel while the response is still in
// flight, so renderGateOutcomeButtons re-emits every outcome disabled and
// replaces the controls whose restores were captured at click time. Clearing
// the in-flight key on settle only unwinds those now-detached originals; the
// live replacements stay disabled until a later poll. Re-enable whatever
// outcome controls are live in the document for this node run now, so a
// repaint that landed mid-flight cannot strand the gate — on failure in
// particular, when no refresh follows to rebuild the panel.
function restoreLiveGateOutcomes(element, nodeRunID) {
  const doc = globalThis.document;
  if (!doc?.querySelectorAll || !nodeRunID) return;
  for (const control of doc.querySelectorAll(`[data-workflow-respond="${nodeRunID}"]`)) {
    control.disabled = false;
    control.removeAttribute?.("aria-busy");
    control.classList?.remove("is-busy");
  }
}

// Render-time counterpart to suppressGateOutcomes: a poll repaint rebuilds the
// gate panel from scratch, so the fresh outcome buttons re-derive their
// suppression from the shared in-flight registry instead of waiting for the
// next click.
export function gateResponsePending(nodeRunID) {
  return inFlight.has(`workflowRespond:${nodeRunID}`);
}

const PENDING_LABELS = {
  workflowSchedule: "Scheduling",
  workflowReset: "Resetting",
  workflowDone: "Closing out",
  workflowReopen: "Reopening",
  workflowRespond: "Sending feedback",
  gateComment: "Sending comment",
  workflowBudget: "Extending budget",
  workflowRetry: "Retrying",
  workflowSkip: "Skipping step",
  workflowHold: "Holding",
  workflowTakeOver: "Taking over",
  workflowRelease: "Releasing",
  attentionMerge: "Merging",
  cardMerge: "Merging",
  cardApprove: "Approving",
  taskEdit: "Saving",
  mergeChange: "Merging",
  humanReviewApprove: "Satisfying check",
  startTaskConsole: "Starting console",
  releaseTaskConsole: "Releasing console",
  threadClaim: "Claiming thread",
};

// failureMessage renders an arbitrary rejection value as a status-line string.
// A promise may reject with something that is not an Error (fetch middleware,
// aborts, or a bare `reject(null)`), so reading `error.message` directly can
// itself throw. The settlement catch paths must stay total: if formatting the
// failure threw, settleStatus would never run and the in-flight key would leak,
// permanently disabling the control after a repaint.
export function failureMessage(error) {
  if (error instanceof Error) return error.message || error.name || "Failed";
  if (error === null || error === undefined) return "Request failed";
  if (typeof error === "string") return error;
  try {
    const text = String(error);
    if (text && text !== "[object Object]") return text;
  } catch {
    // An exotic rejection value can throw even on String(); fall through.
  }
  return "Request failed";
}

// pendingLabel names the in-flight action on the status line: "Scheduling
// t-0001…". Unknown keys fall back to a humanized form so a new action is
// never silent while it runs.
export function pendingLabel(key, dataset, label = PENDING_LABELS[key] || humanizeKey(key)) {
  const target = String(dataset?.[key] || "").trim();
  return target ? `${label} ${target}…` : `${label}…`;
}

function humanizeKey(key) {
  const words = String(key).replace(/([a-z0-9])([A-Z])/g, "$1 $2").toLowerCase().split(" ");
  return words.map((word, index) => (index === 0 ? word[0].toUpperCase() + word.slice(1) : word)).join(" ");
}

// handleAction finds the nearest ancestor carrying a known action attribute
// and runs it. The control is marked busy synchronously on click and the
// status line names the in-flight action; the in-flight registry blocks a
// second submission for the same action and target until the first settles —
// even if a poll re-render replaced the button node in between. Success and
// failure both land on the status line and restore the control. Returns true
// when it handled the event.
export async function handleAction(app, event) {
  const element = event.target?.closest?.("[data-action-key]") || findActionElement(event.target);
  if (!element) return false;
  const key = actionKeyFor(element);
  if (!key) return false;

  event.preventDefault?.();
  const busyKey = actionBusyKey(key, element.dataset);
  if (element.disabled || inFlight.has(busyKey)) return true;
  const restore = markBusy(element);
  // Every outcome button for a gate shares one in-flight key, so a sibling
  // stays clickable-looking until a repaint even though its click would be
  // rejected. Suppress the whole set synchronously so no sibling appears
  // enabled while the shared response is pending; the restores run on settle.
  const restoreSiblings = key === "workflowRespond" ? suppressGateOutcomes(element) : [];
  const pending = pendingLabel(key, element.dataset);
  beginInFlight(busyKey, pending);
  app.setStatus?.(pending);
  try {
    const result = await ACTIONS[key](app, element, element.dataset);
    // settleStatus arbitrates the shared status line: it keeps a still-pending
    // sibling's label visible instead of showing this result early, and
    // distinguishes a confirmation message from CANCELLED (an explicit clear)
    // and undefined (a silent success that leaves the pending label in place).
    settleStatus(app, busyKey, result === CANCELLED ? "" : result);
  } catch (error) {
    // failureMessage is total, so settleStatus always runs and the key always
    // drains — even for a non-Error rejection such as `reject(null)`.
    settleStatus(app, busyKey, failureMessage(error));
  } finally {
    restore();
    for (const restoreSibling of restoreSiblings) restoreSibling();
    // A repaint mid-flight swapped the suppressed controls for fresh disabled
    // ones; the restores above only reach the detached originals, so bring the
    // live replacements back now that the key is cleared.
    if (key === "workflowRespond") restoreLiveGateOutcomes(element, element.dataset?.workflowRespond);
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
