// Action dispatch: finds the control behind a delegated event, runs its
// handler from the registered action table, and settles the busy/pending
// state the registry tracked. The registry never imports this module; the
// domain tables do.

import { taskAPIBase } from "../api.js";
import {
  acquireBusy,
  actionBusyKey,
  actionHandler,
  actionKeyFor,
  inFlightEntries,
  markBusy,
  releaseBusy,
  restoreLiveGateOutcomes,
  restoreLiveThreadClaims,
  suppressGateOutcomes,
  suppressThreadClaims,
} from "./registry.js";

export const workflowPath = (dataset, id, suffix = "") =>
  `${taskAPIBase(dataset.project)}/${encodeURIComponent(id)}${suffix}`;

// A handler returns CANCELLED when the user backed out of a confirm/prompt
// dialog: handleAction then restores the control and clears the pending label
// it wrote on click, so no stale "Resetting t-0001…" lingers when no request
// went out. Any other string a handler returns is the confirmation message
// handleAction shows *after* the handler's own refresh, so it survives the
// re-render (routes call setTitle, which clears the status line).
export const CANCELLED = Symbol("cancelled");

// ACTION_SETTLE is the settle-burst provenance token. FlowApp arms the
// follow-up reload burst only for a reload — a refresh() or a load() — that
// arrives carrying this token, and only a dispatcher can send one: the
// action, form, and review dispatchers run their handlers against an
// action-scoped app (see actionScope) whose refresh and load stamp the token
// on. A token-carrying reload therefore proves it belongs to one specific
// successful action run — an unrelated reload that merely *overlaps* an
// in-flight action (the board's Done filter firing while a slow POST is
// pending) carries no token and arms nothing, whether or not that action
// goes on to fail. A failed action never reaches its reload at all: the
// rejection unwinds the handler first.
export const ACTION_SETTLE = Symbol("action-settle");

// actionScope hands an action handler the app with its settle provenance
// attached: every property and method forwards to the real app — bound to it,
// so `this` inside app methods is never the scope — except refresh() and
// load(), which stamp the call with ACTION_SETTLE. load() is stamped because
// some handlers own their concluding reload instead of going through
// refresh(): the Console view's start and release helpers reload with
// app.load() (console-view.js), and so does the task-create form (forms.js).
// That is how FlowApp tells the handler's own reload (the one a successful
// action arms the settle burst for) apart from any ordinary reload — polls,
// navigation, manual refreshes — without a line of code in the handlers
// themselves.
export function actionScope(app) {
  if (!app) return app;
  return new Proxy(app, {
    get(target, prop) {
      if (prop === "refresh") {
        return (options = {}) => target.refresh({ ...options, settle: ACTION_SETTLE });
      }
      if (prop === "load") {
        return (options = {}) => target.load({ ...options, settle: ACTION_SETTLE });
      }
      const value = Reflect.get(target, prop, target);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
}

// settleStatus arbitrates the shared status line when a mutation finishes.
// While another mutation is still in flight it keeps that mutation's pending
// label visible and suppresses this key's outcome. Once this is the final
// mutation to settle, its outcome decides the line: a non-empty string (a
// confirmation, validation, or error message) is shown; an empty string — the
// explicit clear a backed-out handler asks for — clears it; and undefined, a
// silent success, leaves the pending label in place rather than blanking it.
// The settling key is still registered (its restores run right after), so it
// is excluded from the still-pending lookup.
export function settleStatus(app, key, message) {
  let stillPending = "";
  for (const [otherKey, entry] of inFlightEntries) {
    if (otherKey !== key && entry.label) stillPending = entry.label;
  }
  if (stillPending) {
    app?.setStatus?.(stillPending);
    return;
  }
  if (message === undefined) return;
  app?.setStatus?.(message);
}

const PENDING_LABELS = {
  lifecycleTransition: "Transitioning",
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
  convergenceRequest: "Starting scope review",
  ownerRuling: "Recording owner ruling",
  workflowTakeOver: "Taking over",
  workflowRelease: "Releasing",
  convergenceDecision: "Resolving convergence review",
  reviewScopeDecision: "Resolving review scope decision",
  attentionMerge: "Merging",
  cardMerge: "Merging",
  cardApprove: "Approving",
  taskEdit: "Saving",
  mergeChange: "Merging",
  humanReviewApprove: "Satisfying check",
  startTaskConsole: "Starting console",
  releaseTaskConsole: "Releasing console",
  startConsole: "Starting console",
  releaseConsole: "Releasing console",
  threadClaim: "Claiming thread",
  relationRemove: "Removing relation",
  workItemRelationRemove: "Removing relation",
};

// failureMessage renders an arbitrary rejection value as a status-line string.
// A promise may reject with something that is not an Error (fetch middleware,
// aborts, or a bare `reject(null)`), so reading `error.message` directly can
// itself throw. A rejected Proxy can go further and throw from its
// getPrototypeOf trap (the instanceof check) or from the message/name getters.
// The settlement catch paths must stay total: if formatting the failure threw,
// settleStatus would never run and the in-flight key would leak, permanently
// disabling the control after a repaint.
export function failureMessage(error) {
  // Even the Error branch needs the guard: instanceof walks the rejection's
  // prototype chain and message/name run its getters, either of which a
  // hostile rejection can make throw. The getters can also hand back a
  // non-string value; coercing inside the guard keeps the return value a
  // string, so the status line's textContent assignment cannot throw on a
  // value whose stringification is hostile.
  try {
    if (error instanceof Error) {
      const message = error.message;
      if (message) return String(message);
      const name = error.name;
      if (name) return String(name);
      return "Failed";
    }
  } catch {
    // A hostile rejection value can throw even on instanceof, or its
    // message/name can resist stringification; fall through.
  }
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
// even if a poll re-render replaced the button node in between (repaints
// re-apply the busy state through applyBusyState). Success and failure both
// land on the status line and restore the control. Returns true when it
// handled the event.
export async function handleAction(app, event) {
  const element = event.target?.closest?.("[data-action-key]") || findActionElement(event.target);
  if (!element) return false;
  const key = actionKeyFor(element);
  if (!key) return false;

  event.preventDefault?.();
  const busyKey = actionBusyKey(key, element.dataset);
  if (element.disabled) return true;
  const entry = acquireBusy(busyKey, pendingLabel(key, element.dataset));
  if (!entry) return true;
  entry.restores.add(markBusy(element));
  // Every outcome button for a gate shares one in-flight key, so a sibling
  // stays clickable-looking until a repaint even though its click would be
  // rejected. Suppress the whole set synchronously so no sibling appears
  // enabled while the shared response is pending; their restores join the
  // entry's, so settling re-enables them wherever a repaint left them.
  if (key === "workflowRespond") {
    for (const restoreSibling of suppressGateOutcomes(element)) entry.restores.add(restoreSibling);
  }
  // The three claim buttons of a thread share one in-flight key too, so a
  // sibling claim stays clickable-looking until a repaint even though its
  // click would be rejected. Suppress every live same-thread claim control
  // synchronously — the row's siblings and any Now-card claim for the same
  // open thread — so no claim appears enabled while the shared one is pending.
  if (key === "threadClaim") {
    for (const restoreSibling of suppressThreadClaims(element)) entry.restores.add(restoreSibling);
  }
  app.setStatus?.(entry.label);
  try {
    // The handler runs against the action-scoped app so its own refresh
    // carries the settle-burst provenance token (see actionScope).
    const result = await actionHandler(key)(actionScope(app), element, element.dataset);
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
    releaseBusy(busyKey);
    // A repaint mid-flight swapped the suppressed controls for fresh ones the
    // renderer emitted already-disabled; those were never marked through
    // markBusy, so no restore reaches them. Bring the live replacements back
    // now that the key is cleared.
    if (key === "workflowRespond") restoreLiveGateOutcomes(element, element.dataset?.workflowRespond);
    if (key === "threadClaim") restoreLiveThreadClaims(element.dataset?.threadClaim);
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
