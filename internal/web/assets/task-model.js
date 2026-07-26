// Pure derivations for task detail. The rail, the Now card, the run spine and
// the tabs all read from one projection, so the page cannot contradict itself
// about what the task is doing.

import { workflowActivityLabel } from "./board.js";
import { formatDwell, waitKindOf } from "./board-model.js";
import { value } from "./normalize.js";

// nodeState collapses a node run into the four states the run spine draws.
export function nodeState(nodeRun, currentNodeRunID) {
  const state = String(value(nodeRun, "state", "State") || "");
  if (value(nodeRun, "id", "ID") === currentNodeRunID) return "current";
  if (state === "succeeded") return "done";
  if (state === "failed" || state === "cancelled") return "failed";
  if (state === "queued" || state === "running" || state === "waiting") return "current";
  return "future";
}

export function nodeDuration(nodeRun) {
  const started = value(nodeRun, "started_at", "StartedAt") || value(nodeRun, "created_at", "CreatedAt");
  const ended = value(nodeRun, "completed_at", "CompletedAt");
  if (!started) return "";
  const from = new Date(started).getTime();
  const to = ended ? new Date(ended).getTime() : Date.now();
  if (Number.isNaN(from) || Number.isNaN(to)) return "";
  return formatDwell(started, to);
}

// runRows turns the node run history into one row per node *visit*, in
// execution order. This is the fix for the tangled graph: a loop or a retry
// becomes an extra row rather than an edge crossing the diagram.
export function runRows(detail) {
  const run = value(detail || {}, "run", "Run") || {};
  const nodeRuns = value(detail || {}, "node_runs", "NodeRuns") || [];
  const snapshot = value(run, "snapshot", "Snapshot") || {};
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  const currentNodeRunID = String(value(run, "current_node_run_id", "CurrentNodeRunID") || "");

  const rows = nodeRuns.map((nodeRun, index) => {
    const visit = Number(value(nodeRun, "visit", "Visit") || 1);
    const kind = String(value(nodeRun, "kind", "Kind") || "");
    const state = nodeState(nodeRun, currentNodeRunID);
    const outcome = String(value(nodeRun, "outcome", "Outcome") || "");
    const error = String(value(nodeRun, "error", "Error") || "");
    // The name is shown once and the kind is a tag, never a second line of
    // prose repeating the name.
    return {
      id: value(nodeRun, "id", "ID"),
      nodeKey: value(nodeRun, "node_key", "NodeKey"),
      name: value(nodeRun, "name", "Name") || value(nodeRun, "node_key", "NodeKey"),
      tag: visit > 1 ? `visit ${visit}` : kind.replaceAll("_", " "),
      kind,
      visit,
      state,
      outcome: state === "failed" ? error || outcome || "failed" : outcome,
      duration: nodeDuration(nodeRun),
      jobs: value(nodeRun, "jobs", "Jobs") || [],
      loop: loopBackLabel(nodeRuns, index, nodes),
      artifactID: value(nodeRun, "output_artifact_id", "OutputArtifactID") || "",
    };
  });

  // Nodes the run has not reached yet, in graph order, so the spine shows
  // what is still ahead rather than stopping at the present.
  const visited = new Set(rows.map((row) => row.nodeKey));
  for (const node of nodes) {
    const key = value(node, "key", "Key");
    if (visited.has(key)) continue;
    rows.push({
      id: `future:${key}`,
      nodeKey: key,
      name: value(node, "name", "Name") || key,
      tag: String(value(node, "kind", "Kind") || "").replaceAll("_", " "),
      kind: value(node, "kind", "Kind"),
      state: "future",
      outcome: "",
      duration: "",
      jobs: [],
      loop: "",
    });
  }
  return rows;
}

// loopBackLabel names the moment a run went backwards, so the reason for a
// repeat visit sits under the row that caused it.
function loopBackLabel(nodeRuns, index, nodes) {
  const next = nodeRuns[index + 1];
  if (!next) return "";
  const currentKey = value(nodeRuns[index], "node_key", "NodeKey");
  const nextKey = value(next, "node_key", "NodeKey");
  const order = nodes.map((node) => value(node, "key", "Key"));
  const from = order.indexOf(currentKey);
  const to = order.indexOf(nextKey);
  if (from < 0 || to < 0 || to >= from) return "";
  const target = nodes[to];
  const visit = Number(value(next, "visit", "Visit") || 1);
  return `↺ looped back to ${value(target, "name", "Name") || nextKey}${visit > 1 ? ` · ×${visit - 1}` : ""}`;
}

// taskModel is the whole page in one object.
export function taskModel(data, workflowData, { now = Date.now() } = {}) {
  const task = value(data, "task", "Task") || {};
  const detail = value(data, "task_detail", "TaskDetail") || {};
  const workflow = value(workflowData || {}, "detail", "Detail") || {};
  const run = value(workflow, "run", "Run") || {};
  const snapshot = value(run, "snapshot", "Snapshot") || {};
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  const wait = value(workflow, "open_wait", "OpenWait");
  const held = Boolean(value(run, "held_at", "HeldAt"));
  const currentNodeKey = String(value(run, "current_node_key", "CurrentNodeKey") || "");
  const currentNode = nodes.find((node) => value(node, "key", "Key") === currentNodeKey) || {};
  const stepIndex = nodes.findIndex((node) => value(node, "key", "Key") === currentNodeKey) + 1;
  const checks = value(detail, "checks", "Checks") || [];
  const threads = value(data, "threads", "Threads") || [];

  const stepName = value(currentNode, "name", "Name") || currentNodeKey.replaceAll("_", " ");
  const activity = held
    ? "Held by you"
    : workflowActivityLabel(stepName, value(currentNode, "kind", "Kind")) || "Working";

  return {
    id: value(task, "id", "ID"),
    title: value(task, "title", "Title"),
    body: value(task, "body", "Body"),
    projectID: value(data, "project_id", "ProjectID") || "",
    projectName: value(data, "project_name", "ProjectName") || "",
    priority: Number(value(task, "priority", "Priority") || 0),
    lifecycleState: value(task, "state", "State") || "unscheduled",
    createdBy: value(task, "created_by", "CreatedBy"),

    run,
    runID: value(run, "id", "ID"),
    runSequence: Number(value(run, "run_sequence", "RunSequence") || 0),
    runState: value(run, "state", "State"),
    nodeRunID: value(run, "current_node_run_id", "CurrentNodeRunID"),
    snapshot,
    transitionCounts: value(workflow, "transition_counts", "TransitionCounts") || [],
    rows: runRows(workflow),

    held,
    heldBy: value(run, "held_by", "HeldBy"),
    wait,
    waitKind: waitKindOf(wait),
    activity,
    stepName,
    stepIndex: stepIndex > 0 ? stepIndex : 0,
    stepCount: nodes.length,
    stepKind: value(currentNode, "kind", "Kind"),
    dwell: formatDwell(value(wait, "created_at", "CreatedAt") || value(task, "updated_at", "UpdatedAt"), now),
    budgetUsed: Number(value(run, "transitions_used", "TransitionsUsed") || 0),
    budgetTotal: Number(value(run, "transition_budget", "TransitionBudget") || 0),

    checks,
    checksSatisfied: checks.filter((check) => value(check, "verdict", "Verdict") === "satisfied").length,
    threads,
    openThreads: threads.filter((thread) => value(thread, "state", "State") === "open").length,
    change: value(detail, "ready_change", "ReadyChange") || (value(detail, "changes", "Changes") || [])[0],
    activeSession: value(detail, "active_session", "ActiveSession"),
    terminalAvailable: Boolean(value(detail, "terminal_available", "TerminalAvailable")),
    terminalJobID: value(detail, "terminal_job_id", "TerminalJobID"),
    taskConsole: value(detail, "task_console", "TaskConsole"),
    transitions: value(detail, "transitions", "Transitions") || [],
    statusLog: value(data, "status_log", "StatusLog") || [],
    sessions: value(detail, "sessions", "Sessions") || [],
    relations: value(detail, "relations", "Relations") || [],
    attachments: value(detail, "attachments", "Attachments") || [],
    epicID: epicParent(value(detail, "relations", "Relations") || [], value(task, "id", "ID")),
  };
}

// epicParent finds the task that planned this one, which is where the rail's
// EPIC link goes.
export function epicParent(relations, taskID) {
  for (const relation of relations) {
    const kind = value(relation, "kind", "Kind");
    const source = value(relation, "source_task_id", "SourceTaskID");
    const target = value(relation, "target_task_id", "TargetTaskID");
    if (kind === "parent_of" && target === taskID) return source;
  }
  return "";
}

// nowCardModel decides whether the page has something to say at the top. It is
// rendered when a workflow wait is open or an open review thread blocks the
// merge, and not otherwise: a permanent card would stop being read.
export function nowCardModel(model) {
  if (model.wait) {
    const message = String(value(model.wait, "message", "Message") || "").trim();
    return {
      tone: model.waitKind === "failed" || model.waitKind === "budget" ? "danger" : "await",
      heading: model.waitKind === "failed" ? "Now · workflow step failed" : `Now · ${model.activity.toLowerCase()}`,
      age: model.dwell,
      actor: model.stepName,
      body: message,
      actions: nowActions(model),
    };
  }
  if (model.openThreads) {
    const thread = model.threads.find((candidate) => value(candidate, "state", "State") === "open");
    return {
      tone: "warn",
      heading: `Now · ${model.openThreads} review thread${model.openThreads === 1 ? "" : "s"} block the merge`,
      age: formatDwell(value(thread, "created_at", "CreatedAt")),
      actor: value(thread, "actor", "Actor") || "review",
      locus: `${value(thread, "file_path", "FilePath")}:${value(thread, "line", "Line")}`,
      threadID: value(thread, "id", "ID"),
      body: firstThreadBody(thread),
      outdated: isOutdatedAnchor(thread, model.change),
      actions: [
        { label: "Claim fixed", key: "thread-claim", kind: "fixed", primary: true },
        { label: "Not warranted", key: "thread-claim", kind: "not_warranted" },
        { label: "Open in diff", key: "open-change" },
      ],
    };
  }
  return null;
}

function nowActions(model) {
  switch (model.waitKind) {
    case "gate":
    case "question":
      return [{ label: "Answer", key: "focus-gate", primary: true }];
    case "failed":
      return [
        { label: "Retry", key: "workflow-retry", primary: true },
        { label: "Skip step", key: "workflow-skip" },
        { label: "Transcript", key: "open-transcript" },
      ];
    case "budget":
      return [{ label: "Extend budget", key: "workflow-budget", primary: true }];
    default:
      return [];
  }
}

function firstThreadBody(thread) {
  const comments = value(thread || {}, "comments", "Comments") || [];
  return value(comments[0] || {}, "body", "Body") || "";
}

// An anchor that no longer matches the change head means the reviewer was
// reading an older revision; saying so is the difference between a stale
// comment and a wrong one.
export function isOutdatedAnchor(thread, change) {
  const anchor = String(value(thread || {}, "anchor_commit_sha", "AnchorCommitSHA") || "");
  const head = String(value(change || {}, "head_sha", "HeadSHA") || "");
  return Boolean(anchor && head && anchor !== head);
}

// tabBadges are the counts the tab strip carries, with the tone that tells you
// whether the number is good news.
export function tabBadges(model) {
  const badges = {};
  if (model.change) badges.change = { text: String(model.openThreads || ""), tone: model.openThreads ? "warn" : "" };
  if (model.checks.length) {
    const ok = model.checksSatisfied === model.checks.length;
    badges.checks = { text: `${model.checksSatisfied}/${model.checks.length}`, tone: ok ? "ok" : "danger" };
  }
  const activity = model.transitions.length + model.statusLog.length;
  if (activity) badges.activity = { text: String(activity), tone: "idle" };
  if (model.terminalAvailable) badges.terminal = { live: true };
  return badges;
}
