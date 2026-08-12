// In-flight action registry: the busy/pending machinery behind the delegated
// action table.
//
// Every mutating control gets an immediate pending state: the dispatcher
// disables the control, marks it aria-busy, and names the in-flight action on
// the status line synchronously on click. In-flight actions are tracked here
// by action and target — not by DOM node — so a poll re-render that replaces
// the button mid-flight cannot re-enable a duplicate submission. The registry
// also carries each action's pending label and busy marks, so a repaint
// re-applies disabled/aria-busy/is-busy and the status-line message to
// whatever replacement control it swapped in.

// inFlight tracks running actions by identity, not by DOM node: the board
// repaints on a 10 s poll and a re-render replaces the button mid-flight, so a
// guard stored on the node would die with it and re-enable a duplicate
// submission. Delegated forms (through formBusyKey) and the review bar
// (review:<change>) share this registry, and the render paths consult it
// (gateResponsePending) to re-suppress fresh controls while a request is out.
export const inFlight = new Set();

// inFlightEntries carries the busy metadata for each in-flight key: the
// pending label the click displayed — so a repaint can put the message back
// after the route render clears it — and the restore functions of every
// control marked busy for the key, including replacements a repaint re-marked
// through applyBusyState, so settling restores whatever is on screen then.
// Callers may also attach their own metadata (the review flow records its
// verdict). Map insertion order tracks start order, so the newest
// still-pending label is the last remaining entry.
export const inFlightEntries = new Map();

// acquireBusy registers an in-flight action and returns its entry, or null
// when the same action and target is already running — the duplicate-click
// guard. Callers attach metadata (the review flow records its verdict) and
// restore functions to the entry; releaseBusy drains it.
export function acquireBusy(busyKey, label) {
  if (inFlight.has(busyKey)) return null;
  inFlight.add(busyKey);
  const entry = { label, restores: new Set() };
  inFlightEntries.set(busyKey, entry);
  return entry;
}

// releaseBusy settles an in-flight action: the key leaves the registry first
// (the action is clickable again, and a throwing restore cannot leak the key),
// then every control marked for it — the clicked one, any sibling suppressed
// with it, and any replacement a repaint re-marked — is restored.
export function releaseBusy(busyKey) {
  const entry = inFlightEntries.get(busyKey);
  inFlight.delete(busyKey);
  inFlightEntries.delete(busyKey);
  if (!entry) return;
  for (const restore of entry.restores) restore();
}

// pendingStatus is the status line's memory across a repaint: route renders
// clear the line (setTitle), so while an action is in flight its pending label
// is put back after every load. Most recently started wins.
export function pendingStatus() {
  let label = "";
  for (const entry of inFlightEntries.values()) label = entry.label || label;
  return label;
}

// The registry never imports the action table (the table's domain modules
// import this module), so the table registers itself: actions.js calls
// registerActions(ACTIONS) once it has merged the domain tables. The key set
// drives actionKeyFor/applyBusyState; the handler lookup drives dispatch.
const ACTION_TABLE = {};
const ACTION_KEYS = new Set();
let actionSelector = "";

export function registerActions(table) {
  for (const [key, handler] of Object.entries(table)) {
    ACTION_TABLE[key] = handler;
    ACTION_KEYS.add(key);
  }
  // actionSelector matches every control the delegated table can dispatch, so
  // applyBusyState can find the replacement nodes a repaint swapped in.
  actionSelector = [...ACTION_KEYS]
    .map((key) => `[data-${key.replace(/([a-z0-9])([A-Z])/g, "$1-$2").toLowerCase()}]`)
    .join(", ");
}

export function actionHandler(key) {
  return ACTION_TABLE[key];
}

export function actionKeyFor(element) {
  for (const key of Object.keys(element.dataset || {})) {
    if (ACTION_KEYS.has(key)) return key;
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
// approvals until the first request settles. relationRemove deletes one
// stored row identified by project, source, target, and kind, so the busy
// identity includes them all — keying on the source task alone would
// conflate two relation rows of one task, suppressing the second removal as
// a duplicate and letting a repaint mark its row busy. The console controls
// act on the (project, task) console pair — the attribute carries the task,
// which is empty for a project console — so the busy identity adds both from
// the dataset: the project console and a task console of the same project
// (or the same task id in two projects) must not suppress each other. The
// three claim buttons of a thread all carry the same data-thread-claim value,
// so the base key already names the whole thread's claim operation: one
// pending claim suppresses every sibling claim for that thread, while a
// different thread's claims keep their own key and stay independent.
export function actionBusyKey(key, dataset) {
  const base = `${key}:${String(dataset?.[key] ?? "")}`;
  if (key === "humanReviewApprove") {
    const check = String(dataset?.checkName ?? "");
    return check ? `${base}:${check}` : base;
  }
  if (key === "relationRemove") {
    return [base, dataset?.project, dataset?.target, dataset?.kind].map((part) => String(part ?? "")).join(":");
  }
  if (key === "startConsole" || key === "releaseConsole") {
    return [base, dataset?.project, dataset?.task].map((part) => String(part ?? "")).join(":");
  }
  return base;
}

// formBusyKey names an in-flight delegated form submission by the mutation it
// will run: the FORMS table key, the project, and the target task. Keying on
// the form's data-attribute value alone breaks down for the two shapes that
// share one attribute across targets — a boolean data-attachment-form
// collapses every task's uploader onto one key, and a multi-project create
// form carries no data-project at all, so two create forms for different
// projects would collide and one in-flight create would swallow the other.
// The identity therefore comes from the same fields the request itself uses:
// the project comes from the form's project select when one is present
// (multi-project create) and otherwise from data-project, and the target task
// comes from data-task, which every per-task form carries, falling back to
// the attribute's own value for forms like taskForm whose data value already
// is the task id. The thread reply form's data value is the thread id, not a
// task id, so it is read directly. Attention reply forms go one level deeper:
// the same task can carry several pending status-log questions, each with its
// own reply form, so the key also carries the question's data-status-log-id —
// two replies for different questions on one task may submit concurrently,
// while a duplicate reply for the same question (including a replacement form
// a repaint re-rendered) stays suppressed. Like every other part of the key,
// the status-log id is a data-attribute value, not the DOM node itself, so the
// identity is stable across re-renders. formBusyKey lives here, next to the
// registry, so applyBusyState can re-mark a replacement form's busy control
// without importing forms.js (which already imports this module).
export function formBusyKey(key, form) {
  const dataset = form?.dataset || {};
  const selectedProject = String(form?.elements?.project?.value ?? "");
  const project = selectedProject || String(dataset.project ?? "");
  const target =
    key === "threadReplyForm"
      ? String(dataset.threadReplyForm ?? "")
      : String(dataset.task ?? dataset[key] ?? "");
  const question =
    key === "attentionReplyForm" ? `:${String(dataset.statusLogId ?? "")}` : "";
  return `form:${key}:${project}:${target}${question}`;
}

// formBusyControl picks the live control a pending form submission marks
// busy: the submit control when the form has one, otherwise the first
// editable field. Buttonless forms submit implicitly — the inline thread
// reply form posts on Enter and carries only a text input — and leaving that
// field enabled would show an apparently actionable control whose submission
// the in-flight guard then silently rejects.
export function formBusyControl(form) {
  if (typeof form?.querySelector !== "function") return null;
  const submitter = form.querySelector('[type="submit"]');
  if (submitter) return submitter;
  if (typeof form.querySelectorAll !== "function") return null;
  for (const field of form.querySelectorAll("input, textarea, select")) {
    if (field.getAttribute?.("type") !== "hidden") return field;
  }
  return null;
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

// applyBusyState re-marks controls under root whose action is still in flight.
// A poll re-render replaces a busy control with a freshly enabled node; the
// busy state lives in the in-flight registry, not on the discarded node, so a
// repaint (an element's own paint or a route load) calls this to keep the
// replacement disabled and visibly busy — and to register its restore, so the
// action settling re-enables whatever is on screen rather than a dead node.
export function applyBusyState(root) {
  if (!inFlight.size || typeof root?.querySelectorAll !== "function") return;
  for (const element of root.querySelectorAll(actionSelector)) {
    const key = actionKeyFor(element);
    if (!key) continue;
    const entry = inFlightEntries.get(actionBusyKey(key, element.dataset));
    if (!entry || element.disabled || element.classList?.contains?.("is-busy")) continue;
    entry.restores.add(markBusy(element));
  }
  // Delegated form controls carry no action attribute, so find them through
  // their form: any dataset key producing an in-flight form:<key>:<project>:
  // <target> busy key marks the form's busy control (its submitter, or its first
  // editable field for a buttonless form like the thread reply input) busy.
  // Without this a repaint would render the replacement form enabled —
  // apparently actionable, yet inert because the in-flight guard keeps
  // rejecting its submission.
  for (const form of root.querySelectorAll("form")) {
    const control = formBusyControl(form);
    if (!control || control.disabled || control.classList?.contains?.("is-busy")) continue;
    for (const key of Object.keys(form.dataset || {})) {
      const entry = inFlightEntries.get(formBusyKey(key, form));
      if (!entry) continue;
      entry.restores.add(markBusy(control));
      break;
    }
  }
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

export function suppressGateOutcomes(element) {
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
export function restoreLiveGateOutcomes(element, nodeRunID) {
  const doc = globalThis.document;
  if (!doc?.querySelectorAll || !nodeRunID) return;
  // Node-run ids are server-provided and may contain quotes or CSS selector
  // metacharacters, so never interpolate one into a selector: fetch every
  // outcome control and filter by dataset value instead.
  for (const control of doc.querySelectorAll("[data-workflow-respond]")) {
    if (control.dataset?.workflowRespond !== nodeRunID) continue;
    control.disabled = false;
    control.removeAttribute?.("aria-busy");
    control.classList?.remove("is-busy");
  }
}

// The three claim buttons of one thread all carry the same data-thread-claim
// value, so they share a single in-flight key. Marking only the clicked one
// busy leaves its siblings looking enabled until a repaint; suppress every
// live same-thread claim control — the inline row's siblings and any Now-card
// claim for the same open thread — and hand back their restores so the pending
// state reads consistently across every surface and unwinds on settle.
function threadClaimControls(element) {
  const threadID = element.dataset?.threadClaim;
  const doc = globalThis.document;
  if (threadID && typeof doc?.querySelectorAll === "function") {
    // Thread ids are server-provided and may contain quotes or CSS selector
    // metacharacters, so never interpolate one into a selector: fetch every
    // claim control and filter by dataset value instead. An interpolated id
    // that happens to form a valid selector could otherwise suppress another
    // thread's claims.
    const all = [...doc.querySelectorAll("[data-thread-claim]")]
      .filter((control) => control.dataset?.threadClaim === threadID);
    if (all.length) return all;
  }
  const scope = element.closest?.(".claims") ?? element.parentElement;
  if (!scope?.querySelectorAll) return [];
  return [...scope.querySelectorAll("[data-thread-claim]")];
}

export function suppressThreadClaims(element) {
  return threadClaimControls(element)
    .filter((control) => control !== element)
    .map((control) => markBusy(control));
}

// A poll repaint rebuilds a thread's claim row while the claim POST is still
// in flight, so renderClaims re-emits every claim button disabled and replaces
// the controls whose restores were captured at click time. Clearing the
// in-flight key on settle only unwinds those now-detached originals; the live
// replacements stay disabled until a later poll. Re-enable whatever claim
// controls are live in the document for this thread now, so a repaint that
// landed mid-flight cannot strand the claims — on failure in particular, when
// no refresh follows to rebuild the row.
export function restoreLiveThreadClaims(threadID) {
  const doc = globalThis.document;
  if (!doc?.querySelectorAll || !threadID) return;
  // Thread ids are server-provided and may contain quotes or CSS selector
  // metacharacters, so never interpolate one into a selector: fetch every
  // claim control and filter by dataset value instead, so a hostile id cannot
  // throw out of the settlement path or re-enable another thread's controls.
  for (const control of doc.querySelectorAll("[data-thread-claim]")) {
    if (control.dataset?.threadClaim !== threadID) continue;
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

// Render-time counterpart to suppressThreadClaims: a poll repaint rebuilds a
// thread's claim row from scratch, so the fresh claim buttons re-derive their
// suppression from the shared in-flight registry instead of waiting for the
// next click.
export function threadClaimPending(threadID) {
  return inFlight.has(`threadClaim:${threadID}`);
}
