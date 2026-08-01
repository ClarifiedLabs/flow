// Pure derivations shared by the board's lane and table views and by the task
// rail. No DOM, no rendering — just the shape of a card.

import { value } from "./normalize.js";
import { phaseKey, workflowActivityLabel } from "./board.js";

// Dwell thresholds, in minutes. Past warn a task reads amber, past stall it
// reads red. Queued work is allowed to sit longer than running work before it
// counts as stuck: nothing is wrong with a queue, only with a stalled one.
export const DWELL_THRESHOLDS = {
  running: { warn: 45, stall: 120 },
  waiting: { warn: 30, stall: 120 },
  failed: { warn: 0, stall: 0 },
  queued: { warn: 120, stall: 480 },
  unscheduled: { warn: Infinity, stall: Infinity },
};

// formatDwell renders an elapsed time the way the board wants it: one unit, no
// "ago", tabular so a column of them lines up. 41m, 2h, 1d.
export function formatDwell(since, now = Date.now()) {
  if (!since) return "";
  const started = new Date(since).getTime();
  if (Number.isNaN(started)) return "";
  const seconds = Math.max(0, Math.round((now - started) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export function dwellMinutes(since, now = Date.now()) {
  if (!since) return 0;
  const started = new Date(since).getTime();
  if (Number.isNaN(started)) return 0;
  return Math.max(0, (now - started) / 60000);
}

// dwellTone grades how long is too long for the state the task is in.
export function dwellTone(kind, since, now = Date.now()) {
  const thresholds = DWELL_THRESHOLDS[kind] || DWELL_THRESHOLDS.running;
  const minutes = dwellMinutes(since, now);
  if (minutes >= thresholds.stall) return "danger";
  if (minutes >= thresholds.warn) return "warn";
  return "muted";
}

// waitKindOf collapses a durable wait into the four situations the UI has
// distinct copy and actions for.
export function waitKindOf(wait) {
  if (!wait) return "";
  const kind = String(value(wait, "kind", "Kind") || "");
  const reason = String(value(wait, "reason", "Reason") || "");
  if (reason === "transition_budget_exhausted" || reason === "review_cycle_limit") return "budget";
  if (kind === "operator_intervention") return "failed";
  if (kind === "human_gate") return "gate";
  if (kind === "agent_request") return "question";
  return "";
}

// waitReasonText is the line the attention strip prints. It is derived, never
// free text: a gate shows the agent's actual question, a failure shows the
// error verbatim, and a task at the end of its flow says so.
export function waitReasonText(card, { readyToMerge = false } = {}) {
  const wait = value(card, "wait", "Wait");
  const message = String(value(wait, "message", "Message") || "").trim();
  switch (waitKindOf(wait)) {
    case "gate":
    case "question":
      return message || "Waiting for a human decision";
    case "failed":
      return message || "Workflow step failed";
    case "budget":
      return message || "Automation budget exhausted";
    default:
      return readyToMerge ? "Checks and review passed — ready to merge" : message;
  }
}

// waitActionLabel names the button. Bare imperatives, one word where possible.
export function waitActionLabel(card, { readyToMerge = false, held = false } = {}) {
  // A held task is already yours; the only thing to do with it is give it back.
  if (held) return "Resume";
  switch (waitKindOf(value(card, "wait", "Wait"))) {
    case "gate":
      return "Answer";
    case "question":
      return "Answer";
    case "failed":
      return "Retry";
    case "budget":
      return String(value(value(card, "wait", "Wait"), "reason", "Reason") || "") === "review_cycle_limit"
        ? "Grant cycles"
        : "Extend budget";
    default:
      return readyToMerge ? "Merge" : "";
  }
}

// phaseSlug resolves a card state to a [data-phase] value. `await` and
// `danger` are tones the redesign introduced for waits and failures; the rest
// fall through to the shared lifecycle mapping.
export function phaseSlug(state) {
  if (state === "await" || state === "danger") return state;
  return phaseKey(state) || "backlog";
}

// isReadyToMerge reports a task parked at the end of its flow with everything
// green — the "nothing left but the button" state.
export function isReadyToMerge(entry) {
  const card = entry.card || {};
  const stepIndex = Number(value(card, "step_index", "StepIndex") || 0);
  const stepCount = Number(value(card, "step_count", "StepCount") || 0);
  const checks = value(card, "required_checks", "RequiredChecks") || {};
  const total = Number(value(checks, "total", "Total") || 0);
  const satisfied = Number(value(checks, "satisfied", "Satisfied") || 0);
  return Boolean(
    stepCount && stepIndex >= stepCount - 1 && total > 0 && satisfied === total && !value(card, "wait", "Wait"),
  );
}

// cardModel is the single projection every board surface renders from. Doing
// this once means the lane card, the table row and the attention strip cannot
// disagree about what state a task is in.
export function cardModel(entry, { now = Date.now(), showProject = false } = {}) {
  const task = entry.task || {};
  const card = entry.card || {};
  const wait = value(card, "wait", "Wait");
  const lifecycleState = value(task, "state", "State") || "unscheduled";
  const held = Boolean(value(card, "held", "Held")) || entry.laneState === "held";
  const heldBy = String(value(card, "held_by", "HeldBy") || "");
  const convergenceHold = held && heldBy === "system";
  const waitKind = waitKindOf(wait);
  // Queued for a worker: the task is in progress but its job sits unclaimed
  // in the queue. The board must not pretend anyone is working on it, so this
  // one fact drives the phase tone, the activity line, the dwell thresholds
  // and the Running filter all at once.
  const queuedForWorker = entry.laneState === "awaiting_worker";
  // A task parked in the queue may independently satisfy the ready-to-merge
  // heuristic, but it is not actionable until a worker claims it: hold back the
  // Merge action, the attention flag, and the "ready to merge" copy so the card
  // keeps its amber await presentation and cannot contradict its own "Awaiting
  // worker" line. Only held/gate/question/failed waits outrank this.
  const readyToMerge = isReadyToMerge(entry) && !queuedForWorker;

  // A manual hold gets the triage tone so the board tells the truth about who
  // owns the task. A convergence hold is different: the system parked the task
  // on a human scope decision, so it reads as blocked — which is what it is.
  // A task waiting on a person is amber (await), a failed one is red;
  // collapsing both into "blocked" hid the difference that matters most.
  // A task parked in the queue borrows the same amber await tone: it is
  // waiting too, just on a worker rather than a person.
  const phaseState = held
    ? convergenceHold
      ? "blocked"
      : "triage"
    : waitKind === "gate" || waitKind === "question"
      ? "await"
      : waitKind === "failed" || waitKind === "budget"
        ? "danger"
        : readyToMerge
          ? "approved"
          : queuedForWorker
            ? "await"
            : entry.blocked
              ? "blocked"
              : entry.laneState || lifecycleState;
  const stepIndex = Number(value(card, "step_index", "StepIndex") || 0);
  const stepCount = Number(value(card, "step_count", "StepCount") || 0);
  const currentStep = value(card, "current_step", "CurrentStep") || {};
  const stepName =
    String(value(currentStep, "name", "Name") || "").trim() ||
    String(value(currentStep, "key", "Key") || "")
      .replace(/[_-]+/g, " ")
      .trim();

  // A task parked in the queue is queued work whatever the lifecycle says:
  // it gets the generous queued dwell thresholds, not the running ones.
  let dwellKind = "running";
  if (queuedForWorker) dwellKind = "queued";
  else if (lifecycleState === "unscheduled") dwellKind = "unscheduled";
  else if (lifecycleState === "scheduled") dwellKind = "queued";
  else if (waitKind === "failed" || waitKind === "budget") dwellKind = "failed";
  else if (waitKind) dwellKind = "waiting";

  const dwellSince = value(card, "dwell_since", "DwellSince") || value(task, "updated_at", "UpdatedAt");
  const dwell = formatDwell(dwellSince, now);

  // A scheduled task that cannot start says what it is waiting on. The read
  // model already drops resolved blockers, so anything left in the summary is
  // live; the card only surfaces it while the task is queued, not once it is
  // running (the step rail owns the story then).
  const waitingOn = waitingOnBlockers(card, lifecycleState);

  // The card labels the number when the number needs explaining: a waiting or
  // stalled task says why, everything else just shows the elapsed time.
  let dwellLabel = dwell;
  if (dwellKind === "failed" && dwell) dwellLabel = `stalled ${dwell}`;
  else if (dwellKind === "waiting" && dwell) dwellLabel = `waiting ${dwell}`;

  return {
    id: value(task, "id", "ID"),
    title: value(task, "title", "Title"),
    projectID: entry.project?.id || "",
    projectName: showProject ? entry.project?.name || "" : "",
    lifecycleState,
    laneState: entry.laneState || "",
    queuedForWorker,
    phase: phaseSlug(phaseState),
    held,
    heldBy,
    blocked: Boolean(entry.blocked),
    wait,
    waitKind,
    readyToMerge,
    // A manual hold leaves the attention strip because the owner already
    // initiated it. A system convergence hold stays visible until the owner
    // makes the requested scope decision.
    needsYou: convergenceHold || (!held && Boolean(waitKind === "gate" || waitKind === "failed" || waitKind === "budget" || readyToMerge)),
    reason: held
      ? convergenceHold
        ? "Convergence review required — decide whether to split or re-scope"
        : "Held by you — the workflow will not advance"
      : waitReasonText(card, { readyToMerge }),
    actionLabel: waitActionLabel(card, { readyToMerge, held }),
    stepIndex,
    stepCount,
    stepName,
    scheduled: lifecycleState !== "unscheduled",
    running: Boolean(stepCount) && lifecycleState === "in_progress" && !queuedForWorker,
    activity: activityLine(card, {
      held,
      convergenceHold,
      waitKind,
      queuedForWorker,
      dwellSince,
      now,
      stepName,
      currentStep,
      lifecycleState,
    }),
    dwell,
    dwellLabel,
    dwellSince,
    dwellTone: dwellTone(dwellKind, dwellSince, now),
    priority: Number(value(task, "priority", "Priority") || 0),
    diffStats: value(card, "diff_stats", "DiffStats"),
    checks: value(card, "required_checks", "RequiredChecks") || {},
    blockers: value(card, "blockers", "Blockers") || {},
    waitingOn,
    waitingOnOmitted: waitingOnOmitted(card, lifecycleState),
    terminalAvailable: Boolean(value(card, "terminal_available", "TerminalAvailable")),
    terminalJobID: value(card, "terminal_job_id", "TerminalJobID"),
    activeSession: value(card, "active_session", "ActiveSession"),
    change: value(card, "change", "Change"),
  };
}

// waitingOnBlockers lists the unresolved blockers a scheduled card renders as
// "waiting on …". Only scheduled work carries it: an in-progress task is past
// its blockers, and an unscheduled one has not been asked to start yet. Each
// entry keeps the blocker's id and title so the card can link to it.
export function waitingOnBlockers(card, lifecycleState) {
  if (String(lifecycleState || "") !== "scheduled") return [];
  const blockers = value(card, "blockers", "Blockers") || {};
  const tasks = value(blockers, "tasks", "Tasks") || [];
  return tasks
    .map((blocker) => ({
      id: String(value(blocker, "id", "ID") || ""),
      title: String(value(blocker, "title", "Title") || ""),
    }))
    .filter((blocker) => blocker.id || blocker.title);
}

// waitingOnOmitted counts the live blockers the read model left off the card
// because the display is bounded; the card renders it as "+N more" so the
// reader knows titles were omitted rather than guessing the list is complete.
// It mirrors waitingOnBlockers: only scheduled work carries it.
export function waitingOnOmitted(card, lifecycleState) {
  if (String(lifecycleState || "") !== "scheduled") return 0;
  const blockers = value(card, "blockers", "Blockers") || {};
  return Number(value(blockers, "omitted", "Omitted") || 0);
}

// activityLine is the one line of prose describing what is happening now,
// present progressive, derived mechanically from the node name.
function activityLine(card, {
  held,
  convergenceHold,
  waitKind,
  queuedForWorker,
  dwellSince,
  now,
  stepName,
  currentStep,
  lifecycleState,
}) {
  if (held) {
    return convergenceHold
      ? "Held for convergence review — decide whether to split or re-scope"
      : "Held by you — the workflow will not advance";
  }
  const wait = value(card, "wait", "Wait");
  const message = String(value(wait, "message", "Message") || "").trim();
  if (waitKind === "failed" || waitKind === "budget") return message || "Workflow step failed";
  if (waitKind === "gate" || waitKind === "question") return message || "Waiting for a human decision";
  if (lifecycleState === "unscheduled") return "";
  if (lifecycleState === "scheduled") return "Queued for a worker";
  if (queuedForWorker) {
    const dwell = formatDwell(dwellSince, now);
    return dwell ? `Awaiting worker · ${dwell}` : "Awaiting worker";
  }
  const kind = String(value(currentStep, "kind", "Kind") || "");
  return workflowActivityLabel(stepName, kind) || "Working";
}

// sortForAttention orders the board's table and the epic's member list:
// whatever needs a human first, oldest wait at the top, then everything else
// by how long it has been sitting.
export function sortForAttention(models) {
  return [...models].sort((left, right) => {
    if (left.needsYou !== right.needsYou) return left.needsYou ? -1 : 1;
    const leftMs = Date.parse(left.dwellSince || "") || 0;
    const rightMs = Date.parse(right.dwellSince || "") || 0;
    if (leftMs !== rightMs) return leftMs - rightMs;
    return String(left.id).localeCompare(String(right.id));
  });
}

// BOARD_FILTERS are the table view's chips. `all` is implicit in the design's
// "Needs you / Running / Queued" set; unscheduled work lives in the Tasks view.
export const BOARD_FILTERS = [
  ["attention", "Needs you"],
  ["running", "Running"],
  ["queued", "Queued"],
];

export function matchesFilter(model, filter) {
  switch (filter) {
    case "attention":
      return model.needsYou;
    case "running":
      // Queued-for-worker tasks are in progress on paper but nobody is
      // working on them, so they do not count as running.
      return model.lifecycleState === "in_progress" && !model.needsYou && !model.queuedForWorker;
    case "queued":
      return model.lifecycleState === "scheduled";
    default:
      return true;
  }
}
