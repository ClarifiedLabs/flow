// One delegated action table for the whole app.
//
// Every interactive control carries a data-* attribute naming what it does and
// holding its primary id; `data-project` scopes the call. A single listener on
// <flow-app> dispatches them. That is what replaces the old re-query-and-attach
// pass after every repaint: elements now own their own innerHTML, so listeners
// attached to their children would not survive a poll, while a listener on the
// app root does.
//
// The table's domain handlers live under actions/ (workflow, review, features,
// console, relations); the busy/pending registry lives in actions/registry.js
// and the dispatcher in actions/dispatch.js. This module merges the domain
// tables into ACTIONS, registers them with the registry, and re-exports the
// registry/dispatch API so existing importers keep working. New code should
// import the actions/* module that owns what it needs.

import { registerActions } from "./actions/registry.js";
import { workflowActions } from "./actions/workflow.js";
import { reviewActions } from "./actions/review.js";
import { featureActions } from "./actions/features.js";
import { consoleActions } from "./actions/console.js";
import { relationActions } from "./actions/relations.js";

// ACTIONS maps a dataset key to what pressing that control does. Handlers
// receive the app (for status and refresh), the element, and its dataset, and
// return the confirmation message for the status line (or CANCELLED).
export const ACTIONS = {
  ...workflowActions,
  ...reviewActions,
  ...featureActions,
  ...consoleActions,
  ...relationActions,
};

// The registry cannot import this module (the domain tables import it), so
// the merged table is pushed into it: actionKeyFor/applyBusyState key off the
// registered names and the dispatcher looks handlers up by name.
registerActions(ACTIONS);

export {
  acquireBusy,
  actionBusyKey,
  actionKeyFor,
  applyBusyState,
  formBusyControl,
  formBusyKey,
  gateResponsePending,
  inFlight,
  inFlightEntries,
  markBusy,
  pendingStatus,
  releaseBusy,
  threadClaimPending,
} from "./actions/registry.js";

export {
  ACTION_SETTLE,
  CANCELLED,
  actionScope,
  failureMessage,
  handleAction,
  pendingLabel,
  settleStatus,
  workflowPath,
} from "./actions/dispatch.js";
