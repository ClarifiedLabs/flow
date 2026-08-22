// The automation-budget modal: when a workflow run waits on an operator
// intervention for `review_cycle_limit` or `transition_budget_exhausted`, the
// board card and the task page's Now card offer to extend it. The extension
// POST is rejected server-side without operator instructions (they are both
// the recorded rationale and the payload the next author session reads), so
// the count and the instructions are collected in one modal instead of a
// window.prompt that could never carry them.
//
// The modal is mounted at app level (a child of .main), not inside the
// originating flow-task-card/flow-now-card: those elements repaint their own
// innerHTML on every poll, which would destroy the half-filled form mid-edit.
// .main is written once by renderShell() and survives polls, and load() only
// rewrites .content.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

// MaxFlowTransitionBudget on the server (internal/coordinator/flow_graph.go).
// Both budget kinds share the same ceiling.
export const MAX_BUDGET = 500;

export const BUDGET_KINDS = {
  "review-cycles": {
    title: "Grant review cycles",
    submitLabel: "Grant cycles",
    additionalLabel: "Additional cycles",
    contextLabel: "Review cycles",
    noun: "review cycles",
    defaultAdditional: 2,
  },
  transitions: {
    title: "Extend automation budget",
    submitLabel: "Extend budget",
    additionalLabel: "Additional transitions",
    contextLabel: "Transitions",
    noun: "transitions",
    defaultAdditional: 50,
  },
};

// budgetCopy resolves the per-kind labels, defaulting to transitions so an
// unknown data-workflow-budget-kind still renders a working modal.
export function budgetCopy(kind) {
  return BUDGET_KINDS[String(kind || "").trim()] || BUDGET_KINDS.transitions;
}

export function budgetKindName(kind) {
  return String(kind || "").trim() === "review-cycles" ? "review-cycles" : "transitions";
}

// budgetDefaults normalizes a render state: task/project ids, the kind and its
// copy, the optional usage context line, the pending flag, the inline error,
// the field prefills (kept when a rejection re-renders the form), and the
// disposition view that replaces the form once the POST settles.
export function budgetDefaults(state = {}) {
  const copy = budgetCopy(state.kind);
  return {
    taskID: String(state.taskID || "").trim(),
    projectID: String(state.projectID || "").trim(),
    kind: budgetKindName(state.kind),
    used: state.used,
    total: state.total,
    additional: state.additional,
    instructions: state.instructions,
    pending: Boolean(state.pending),
    error: String(state.error || "").trim(),
    result: state.result || null,
    ...copy,
  };
}

// budgetRunTotals reads the authoritative totals off the POST response's run
// for the kind that was extended, so the disposition needs no extra GET.
export function budgetRunTotals(run, kind) {
  if (budgetKindName(kind) === "review-cycles") {
    return {
      used: Number(value(run, "review_cycles_used", "ReviewCyclesUsed") || 0),
      total: Number(value(run, "review_cycle_budget", "ReviewCycleBudget") || 0),
    };
  }
  return {
    used: Number(value(run, "transitions_used", "TransitionsUsed") || 0),
    total: Number(value(run, "transition_budget", "TransitionBudget") || 0),
  };
}

// budgetModalResult normalizes the POST response into the disposition state.
export function budgetModalResult(run, additional, kind) {
  return {
    additional: Number(additional || 0),
    totals: budgetRunTotals(run, kind),
    run: run || {},
  };
}

// budgetValidationError mirrors the server's contract (ExtendBudget): an
// integer count in 1..MAX_BUDGET and non-blank instructions. Validating here
// means the operator sees the requirement in the modal instead of a 400 on the
// status bar, and no request goes out for input the server must reject.
export function budgetValidationError(additionalRaw, instructions) {
  const text = String(additionalRaw ?? "").trim();
  const additional = Number(text);
  if (!text || !Number.isInteger(additional) || additional < 1 || additional > MAX_BUDGET) {
    return `Additional budget must be a whole number between 1 and ${MAX_BUDGET}`;
  }
  if (!String(instructions ?? "").trim()) {
    return "Instructions are required to extend an automation budget";
  }
  return "";
}

// renderBudgetModal is the pure string builder for both the entry form and
// the post-request disposition view, so tests can assert on it without a DOM.
// There is deliberately no <form> wrapper: an unkeyed form would fall through
// handleFormSubmit to a native GET submit, and every control is type="button"
// so nothing inside can submit one implicitly either.
export function renderBudgetModal(state = {}) {
  const model = budgetDefaults(state);
  return `
    <div class="budget-modal-panel">
      <h2 id="budget-modal-title">${escapeHTML(model.title)} for <code>${escapeHTML(model.taskID)}</code></h2>
      ${model.result ? renderBudgetResult(model) : renderBudgetForm(model)}
    </div>
  `;
}

function renderBudgetForm(model) {
  const additional = Number.isFinite(Number(model.additional)) && String(model.additional).trim() !== ""
    ? Number(model.additional)
    : model.defaultAdditional;
  const disabled = model.pending ? " disabled" : "";
  const busy = model.pending ? ' aria-busy="true"' : "";
  // The grant control carries the project so the POST lands on the
  // project-scoped route in multi-project deployments.
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  return `
    ${contextLine(model) ? `<p class="budget-modal-context">${escapeHTML(contextLine(model))}</p>` : ""}
    <div class="budget-modal-form">
      <label>
        <span>${escapeHTML(model.additionalLabel)}</span>
        <input
          type="number"
          min="1"
          max="${MAX_BUDGET}"
          step="1"
          value="${escapeAttr(String(additional))}"
          data-budget-additional${disabled}
        >
      </label>
      <label>
        <span>Instructions (required)</span>
        <textarea rows="4" data-budget-instructions placeholder="Why the additional budget is warranted and what the next session should do"${disabled}>${escapeHTML(String(model.instructions || ""))}</textarea>
        <span class="budget-modal-hint">Required. Recorded with the decision and delivered to the next author session.</span>
      </label>
      ${model.error ? `<div class="budget-modal-error" role="alert">${escapeHTML(model.error)}</div>` : ""}
      <div class="budget-modal-actions">
        <button class="button secondary" type="button" data-budget-cancel${disabled}>Cancel</button>
        <button class="button" type="button" data-budget-grant="${escapeAttr(model.taskID)}"${projectAttr}${disabled}${busy}>${escapeHTML(model.submitLabel)}</button>
      </div>
    </div>
  `;
}

function renderBudgetResult(model) {
  const result = model.result || {};
  const granted = Number(result.additional || 0);
  const totals = result.totals || { used: 0, total: 0 };
  const runState = String(value(result.run, "state", "State") || "").trim();
  const nodeKey = String(value(result.run, "current_node_key", "CurrentNodeKey") || "").trim();
  return `
    <p class="budget-modal-context">Granted ${escapeHTML(String(granted))} ${escapeHTML(model.noun)}</p>
    <dl class="budget-modal-totals">
      <div><dt>${escapeHTML(model.contextLabel)}</dt><dd>${escapeHTML(`${totals.used}/${totals.total}`)}</dd></div>
      <div><dt>Run state</dt><dd>${escapeHTML(runState || "unknown")}</dd></div>
      ${nodeKey ? `<div><dt>Current node</dt><dd><code>${escapeHTML(nodeKey)}</code></dd></div>` : ""}
    </dl>
    <div class="budget-modal-actions">
      <button class="button" type="button" data-budget-done>Done</button>
    </div>
  `;
}

// contextLine names where the run stands ("Review cycles 3/3") when the
// originating surface could carry usage; unknown usage renders nothing rather
// than a wrong zero.
function contextLine(model) {
  const used = Number(model.used);
  const total = Number(model.total);
  if (!Number.isFinite(used) || !Number.isFinite(total) || total <= 0) return "";
  return `${model.contextLabel} ${used}/${total}`;
}

// budgetModalHost is the app-level mount: .main when the shell has rendered
// (written once, survives polls), .content for pre-shell renders, root
// otherwise. Defensive ?. keeps the helper usable from the inline test DOM.
export function budgetModalHost(root) {
  const main = root?.querySelector?.(".main");
  if (main) return main;
  return root?.querySelector?.(".content") || root;
}

// renderBudgetModalInto (re)renders the modal into a host, creating the
// <dialog> layer on first use. The cancel (Escape) handler reads the pending
// flag off the layer itself, so a re-render with a new state cannot leave a
// stale closure deciding whether Escape is allowed.
export function renderBudgetModalInto(host, state) {
  if (!host || typeof host.appendChild !== "function") return null;
  let layer = budgetModalLayer(host);
  if (!layer) {
    layer = document.createElement("dialog");
    layer.className = "budget-modal";
    layer.dataset.budgetModalLayer = "true";
    layer.setAttribute("aria-modal", "true");
    layer.setAttribute("aria-labelledby", "budget-modal-title");
    layer.addEventListener("cancel", (event) => {
      if (layer.dataset.budgetPending === "true") {
        event.preventDefault();
        return;
      }
      closeBudgetModalLayer(layer);
    });
    host.appendChild(layer);
  }
  if (state?.pending) layer.dataset.budgetPending = "true";
  else delete layer.dataset.budgetPending;
  // The kind lives on the layer itself: budgetGrant re-reads it from there
  // because the submit control's dataset only names the task and project.
  layer.dataset.budgetKind = budgetKindName(state?.kind);
  layer.innerHTML = renderBudgetModal(state);
  if (!layer.hasAttribute?.("open")) {
    if (typeof layer.showModal === "function") layer.showModal();
    else layer.setAttribute("open", "");
  }
  return layer;
}

function budgetModalLayer(host) {
  for (const child of Array.from(host?.children || [])) {
    if (child.dataset?.budgetModalLayer === "true") return child;
  }
  return null;
}

// openBudgetModal renders the entry form for a budget wait. Returns the layer.
export function openBudgetModal(root, state) {
  return renderBudgetModalInto(budgetModalHost(root), state);
}

// closeBudgetModal removes the modal wherever it is mounted, reporting whether
// one was open. Called on navigation, on Cancel/Done, and by the action's own
// refresh so a re-render can restore the disposition view.
export function closeBudgetModal(root) {
  const hosts = [root?.querySelector?.(".main"), root?.querySelector?.(".content"), root];
  let closed = false;
  const seen = new Set();
  for (const host of hosts) {
    if (!host || seen.has(host)) continue;
    seen.add(host);
    for (const child of Array.from(host?.children || [])) {
      if (child.dataset?.budgetModalLayer === "true") {
        closeBudgetModalLayer(child);
        closed = true;
      }
    }
  }
  return closed;
}

function closeBudgetModalLayer(layer) {
  if (!layer) return false;
  if (typeof layer.close === "function" && layer.hasAttribute?.("open")) {
    try {
      layer.close();
    } catch {
      // close() throws on an already-closed dialog; removal is the same end state
    }
  }
  layer.remove?.();
  return true;
}
