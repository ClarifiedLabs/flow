// Pure derivations for task detail. The rail, the Now card, the run spine and
// the tabs all read from one projection, so the page cannot contradict itself
// about what the task is doing.

import { workflowActivityLabel } from "./board.js";
import { formatDwell, waitKindOf } from "./board-model.js";
import { value } from "./normalize.js";
import { buildWorkItemIndex, groupWorkItemRelations, taskWorkContext, workItemAncestors, workItemID } from "./work-item-model.js";
import { projectWorkHref, resolveWorkNavigation } from "./work-nav.js";

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

// parseWaitDetails accepts only the complete immutable human-gate contract.
// A missing or malformed payload is deliberately not reconstructed from the
// current graph: it is not safe to answer a wait whose frozen details are gone.
export function parseWaitDetails(wait) {
  let details = value(wait || {}, "details", "Details");
  if (typeof details === "string") {
    try {
      details = JSON.parse(details);
    } catch {
      return null;
    }
  }
  if (!details || typeof details !== "object" || Array.isArray(details)) return null;
  const allowed = new Set(["instructions", "outcomes", "artifact_id", "interactive", "gate_node_key"]);
  if (Object.keys(details).some((key) => !allowed.has(key))) return null;
  if (typeof details.interactive !== "boolean") return null;
  const gateNodeKey = String(details.gate_node_key || "").trim();
  if (!gateNodeKey || !Array.isArray(details.outcomes) || details.outcomes.length === 0) return null;
  const outcomes = details.outcomes.map((outcome) => String(outcome || "").trim());
  if (outcomes.some((outcome) => !outcome) || new Set(outcomes).size !== outcomes.length) return null;
  const artifactID = String(details.artifact_id || "").trim();
  if (details.interactive && !artifactID) return null;
  return {
    instructions: typeof details.instructions === "string" ? details.instructions : "",
    outcomes,
    artifact_id: artifactID,
    interactive: details.interactive,
    gate_node_key: gateNodeKey,
  };
}

// reviewModel is the Review tab's projection: the open human gate or agent
// question, the artifact under review, the discussion so far, and the live
// agent session when the review is interactive. Null when there is nothing
// to review and nothing under review.
export function reviewModel({ wait, currentNode, run, artifacts, statusLog, activeSession }) {
  const waitKind = String(value(wait, "kind", "Kind") || "");
  const details = parseWaitDetails(wait);

  let gate = null;
  if (waitKind === "human_gate" && details) {
    const artifactID = details.artifact_id;
    const artifact = artifacts.find((candidate) => String(value(candidate, "id", "ID")) === artifactID) || null;
    const nodeRunID = String(value(wait, "node_run_id", "NodeRunID") || "").trim();
    const waitID = String(value(wait, "id", "ID") || "").trim();
    if (nodeRunID && waitID) {
      gate = {
        nodeRunID,
        waitID,
        heading: String(value(wait, "message", "Message") || "Review"),
        instructions: details.instructions || String(value(wait, "message", "Message") || ""),
        outcomes: details.outcomes,
        interactive: details.interactive,
        artifactID,
        changeGate: String(value(artifact || {}, "kind", "Kind")) === "change",
      };
    }
  }

  let question = null;
  if (waitKind === "agent_request") {
    const entry = (statusLog || []).find((candidate) => value(candidate, "kind", "Kind") === "question") || {};
    question = {
      message: String(value(wait, "message", "Message") || ""),
      statusLogID: value(entry, "id", "ID") || "",
    };
  }

  let artifact = null;
  const artifactID = gate?.artifactID || String(value(run, "current_artifact_id", "CurrentArtifactID") || "");
  const found =
    artifacts.find((candidate) => String(value(candidate, "id", "ID")) === artifactID) ||
    [...artifacts].reverse().find((candidate) => String(value(candidate, "kind", "Kind")) === "task_set") ||
    null;
  if (found) {
    let manifest = null;
    let payload = value(found, "payload", "Payload");
    if (typeof payload === "string") {
      try {
        payload = JSON.parse(payload);
      } catch {
        payload = null;
      }
    }
    if (String(value(found, "kind", "Kind")) === "task_set" && payload && Array.isArray(payload.tasks)) manifest = payload;
    artifact = {
      id: String(value(found, "id", "ID") || ""),
      kind: String(value(found, "kind", "Kind") || ""),
      summary: String(value(found, "summary_markdown", "SummaryMarkdown") || ""),
      manifest,
      createdAt: value(found, "created_at", "CreatedAt"),
    };
  }

  const waitSince = new Date(value(wait, "created_at", "CreatedAt") || 0).getTime();
  const comments = (statusLog || [])
    .filter((entry) => {
      if (!wait) return false;
      const at = new Date(value(entry, "created_at", "CreatedAt") || 0).getTime();
      return Number.isFinite(at) && at >= waitSince;
    })
    .slice(-20)
    .map((entry) => ({
      id: value(entry, "id", "ID"),
      actor: String(value(entry, "actor", "Actor") || ""),
      kind: String(value(entry, "kind", "Kind") || "note"),
      message: String(value(entry, "message", "Message") || ""),
      createdAt: value(entry, "created_at", "CreatedAt"),
    }));

  let session = null;
  if (gate?.interactive && activeSession) {
    const sessionID = String(value(activeSession, "id", "ID") || "");
    if (sessionID) {
      session = {
        id: sessionID,
        state: String(value(activeSession, "state", "State") || ""),
        terminalAvailable: Boolean(value(activeSession, "terminal_available", "TerminalAvailable")),
      };
    }
  }

  if (!gate && !question && !artifact) return null;
  return { gate, question, artifact, comments, session };
}

// taskModel is the whole page in one object.
export function taskModel(data, workflowData, { now = Date.now() } = {}) {
  const task = value(data, "task", "Task") || {};
  const detail = value(data, "task_detail", "TaskDetail") || {};
  const generic = value(data, "work_item", "WorkItem");
  const genericItem = value(generic || {}, "item", "Item") || {};
  const genericRelations = value(generic || {}, "relations", "Relations") || [];
  const genericChildren = value(generic || {}, "children", "Children") || [];
  const workIndex = buildWorkItemIndex({ items: [...(value(data, "work_items", "WorkItems") || []), genericItem] });
  const boundedAncestors = value(generic || {}, "ancestors", "Ancestors");
  const ancestors = Array.isArray(boundedAncestors)
    ? boundedAncestors
    : workItemAncestors(workIndex, workItemID(genericItem)).reverse();
  const requestedNavigation = value(data, "navigation", "Navigation") || {};
  const planningContext = taskWorkContext(workIndex, genericItem, requestedNavigation.context);
  const navigation = resolveWorkNavigation(
    requestedNavigation,
    planningContext.validContextIDs,
    value(data, "project_id", "ProjectID") || "",
  );
  if (!navigation.contextValid) navigation.returnTo = projectWorkHref(value(data, "project_id", "ProjectID") || "");
  const workflow = value(workflowData || {}, "detail", "Detail") || {};
  const run = value(workflow, "run", "Run") || {};
  const snapshot = value(run, "snapshot", "Snapshot") || {};
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  const wait = value(workflow, "open_wait", "OpenWait");
  const scopeDecision = String(value(wait || {}, "kind", "Kind") || "") === "review_scope_decision"
    ? value(wait || {}, "details", "Details") || null
    : null;
  const held = Boolean(value(run, "held_at", "HeldAt"));
  const heldBy = String(value(run, "held_by", "HeldBy") || "");
  const systemHeld = held && heldBy === "system";
  const convergenceEvidence =
    value(workflow, "convergence_evidence", "ConvergenceEvidence") ||
    value(detail, "convergence_evidence", "ConvergenceEvidence") ||
    null;
  const currentNodeKey = String(value(run, "current_node_key", "CurrentNodeKey") || "");
  const currentNode = nodes.find((node) => value(node, "key", "Key") === currentNodeKey) || {};
  const stepIndex = nodes.findIndex((node) => value(node, "key", "Key") === currentNodeKey) + 1;
  const checks = value(detail, "checks", "Checks") || [];
  const threads = value(data, "threads", "Threads") || [];
  const artifacts = value(workflowData || {}, "artifacts", "Artifacts") || [];
  const changes = value(detail, "changes", "Changes") || [];
  const currentArtifactID = String(value(run, "current_artifact_id", "CurrentArtifactID") || "");
  const currentArtifact = artifacts.find((candidate) => String(value(candidate, "id", "ID")) === currentArtifactID) || null;
  let currentArtifactPayload = value(currentArtifact || {}, "payload", "Payload");
  if (typeof currentArtifactPayload === "string") {
    try {
      currentArtifactPayload = JSON.parse(currentArtifactPayload);
    } catch {
      currentArtifactPayload = null;
    }
  }
  const currentChangeID = String(value(currentArtifactPayload || {}, "change_id", "ChangeID") || "");
  const currentChange = changes.find((candidate) => String(value(candidate, "id", "ID")) === currentChangeID) || null;
  const canRequestConvergence = Boolean(
    !convergenceEvidence &&
      !systemHeld &&
      currentArtifactID &&
      String(value(currentArtifact || {}, "kind", "Kind")) === "change" &&
      currentChange &&
      String(value(currentArtifactPayload || {}, "head_sha", "HeadSHA")) &&
      String(value(currentArtifactPayload || {}, "head_sha", "HeadSHA")) === String(value(currentChange, "head_sha", "HeadSHA") || ""),
  );
  const statusLog = value(data, "status_log", "StatusLog") || [];
  const review = reviewModel({
    wait,
    currentNode,
    run,
    artifacts,
    statusLog,
    activeSession: value(detail, "active_session", "ActiveSession"),
  });

  const stepName = value(currentNode, "name", "Name") || currentNodeKey.replaceAll("_", " ");
  const activity = held
    ? convergenceEvidence || systemHeld
      ? "Held for convergence review"
      : "Held by you"
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
    heldBy,
    systemHeld,
    convergenceEvidence,
    scopeDecision,
    activeRulings: value(workflow, "active_rulings", "ActiveRulings") || [],
    canRequestConvergence,
    wait,
    waitKind: waitKindOf(wait),
    activity,
    stepName,
    stepIndex: stepIndex > 0 ? stepIndex : 0,
    stepCount: nodes.length,
    stepKind: value(currentNode, "kind", "Kind"),
    dwell: formatDwell(value(wait, "created_at", "CreatedAt") || value(task, "updated_at", "UpdatedAt"), now),
    // The clock the model was built with. Every elapsed-time field on the page
    // — dwell above and the Now card's review-thread age — derives from this
    // one `now`, so a fixed model clock cannot drift between them.
    now,
    budgetUsed: Number(value(run, "transitions_used", "TransitionsUsed") || 0),
    budgetTotal: Number(value(run, "transition_budget", "TransitionBudget") || 0),

    checks,
    checksSatisfied: checks.filter((check) => value(check, "verdict", "Verdict") === "satisfied").length,
    // findings is the task's review findings registry read model (the
    // TaskFindingsResponse the task route fetches alongside the workflow), or
    // null when the read failed or was not attached.
    findings: value(data, "findings", "Findings") || null,
    threads,
    openThreads: threads.filter((thread) => value(thread, "state", "State") === "open").length,
    change: value(detail, "ready_change", "ReadyChange") || changes[0],
    activeSession: value(detail, "active_session", "ActiveSession"),
    terminalAvailable: Boolean(value(detail, "terminal_available", "TerminalAvailable")),
    terminalJobID: value(detail, "terminal_job_id", "TerminalJobID"),
    taskConsole: value(detail, "task_console", "TaskConsole"),
    transitions: value(detail, "transitions", "Transitions") || [],
    statusLog,
    review,
    sessions: value(detail, "sessions", "Sessions") || [],
    // Generic context is canonical for planning: it preserves cross-kind
    // ancestry, blockers and relation endpoint summaries. Legacy task relations
    // remain the fallback for older servers and API consumers.
    workItem: genericItem,
    ancestors,
    directAncestors: planningContext.directAncestors,
    effectiveFeaturePath: planningContext.effectiveFeaturePath,
    contextItem: planningContext.contextItem,
    navigation,
    children: genericChildren.map((child) => value(child, "item", "Item") || child),
    blockers: value(generic || {}, "blockers", "Blockers") || [],
    relations: generic ? genericRelations : value(detail, "relations", "Relations") || [],
    relationGroups: generic
      ? groupWorkItemRelations(genericRelations, workItemID(genericItem) || value(task, "id", "ID"))
      : relationGroups(value(detail, "relations", "Relations") || [], value(task, "id", "ID")),
    genericRelations: Boolean(generic),
    workItems: workIndex.items,
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

// RELATION_GROUPS is the order the relations panel lists its sections in. Each
// group is one direction of one relation kind, read from the current task's
// point of view.
export const RELATION_GROUPS = [
  { key: "parent", label: "Parent" },
  { key: "children", label: "Children" },
  { key: "blocks", label: "Blocks" },
  { key: "blockedBy", label: "Blocked by" },
  { key: "related", label: "Related" },
];

// The lifecycle vocabulary below mirrors the server's task lifecycle
// (LifecycleState in internal/coordinator/tasks.go) and is the single source of
// truth for the task-relations verdict. "" is the wire encoding of a valid
// unscheduled task: the relation payload's SourceState is a non-pointer
// LifecycleState, so an unscheduled blocker ships a *present* empty state and —
// like the server's blocked-by read model, which clears a blocker only once it
// is done — is a confirmed blocker. Every other member of LIFECYCLE_UNFINISHED
// is a non-done lifecycle state the server serializes. The Go parity test
// TestTaskRelationsLifecycleParity (internal/web) parses these exports and
// fails if they drift from the Go constants — enumerated exhaustively in
// coordinator.AllLifecycleStates — so adding a server lifecycle state cannot
// silently leave this allowlist stale.
export const LIFECYCLE_UNFINISHED = new Set(["", "scheduled", "in_progress"]);

export const LIFECYCLE_DONE = "done";

// blockerVerdict reads the denormalized lifecycle state a relation payload ships
// for the blocker side and reduces it to the tri-state the relations row renders:
// true is a confirmed unfinished blocker, false a confirmed done one, and null an
// unknown one. A missing or null state is a malformed payload the read model
// never emits, and a state that is present but outside the lifecycle vocabulary
// (whitespace, an unknown token) or not a string at all is one we cannot trust —
// both render unknown rather than being read as a confirmed non-done blocker.
export function blockerVerdict(relation) {
  const state = relation == null ? undefined : relation["source_state"] ?? relation["SourceState"];
  if (state === LIFECYCLE_DONE) return false;
  if (typeof state !== "string") return null; // absent, null, or not a string: malformed.
  if (LIFECYCLE_UNFINISHED.has(state)) return true;
  return null; // any other string is outside the lifecycle vocabulary.
}

// relationGroups turns the flat, direction-agnostic relation rows the API
// returns into the five lists a reader expects, each holding the *other* task
// relative to the one being viewed. A relation row names a source and a target;
// which side is "the other task" flips depending on which side the current task
// is on, so each group records the direction too — the add/remove controls need
// it to reconstruct the exact row the server stores.
export function relationGroups(relations, taskID) {
  const groups = {};
  for (const group of RELATION_GROUPS) groups[group.key] = [];
  const id = String(taskID || "");

  for (const relation of relations || []) {
    const kind = String(value(relation, "kind", "Kind") || "");
    const source = String(value(relation, "source_task_id", "SourceTaskID") || "");
    const target = String(value(relation, "target_task_id", "TargetTaskID") || "");
    const sourceTitle = String(value(relation, "source_title", "SourceTitle") || "");
    const targetTitle = String(value(relation, "target_title", "TargetTitle") || "");

    // The current task is the source: the other task is the target.
    if (source === id) {
      if (kind === "parent_of") {
        groups.children.push(entry(target, targetTitle, kind, "source"));
      } else if (kind === "blocks") {
        groups.blocks.push(entry(target, targetTitle, kind, "source"));
      } else if (kind === "related_to") {
        groups.related.push(entry(target, targetTitle, kind, "source"));
      }
      continue;
    }

    // The current task is the target: the other task is the source.
    if (target === id) {
      if (kind === "parent_of") {
        groups.parent.push(entry(source, sourceTitle, kind, "target"));
      } else if (kind === "blocks") {
        // A blocker only stops mattering once it is done; its denormalized
        // lifecycle state decides whether the row is a confirmed blocker, a
        // finished one, or an unknown one (see blockerVerdict).
        groups.blockedBy.push(entry(source, sourceTitle, kind, "target", blockerVerdict(relation)));
      } else if (kind === "related_to") {
        groups.related.push(entry(source, sourceTitle, kind, "target"));
      }
    }
  }
  return groups;
}

// entry is one row in a relation group. direction says which side of the stored
// relation the current task sits on, so the remove control can reconstruct the
// exact source/target pair the server stores. unresolved flags a blocker that
// has not finished; only blocked-by rows ever set it, from the relation's
// denormalized lifecycle state.
function entry(taskID, title, kind, direction, unresolved = false) {
  return {
    taskID,
    title: title || taskID,
    kind,
    direction,
    unresolved,
  };
}

// nowCardModel decides whether the page has something to say at the top. It is
// rendered when a workflow wait is open or an open review thread blocks the
// merge, and not otherwise: a permanent card would stop being read.
export function nowCardModel(model) {
  if (model.wait) {
    const message = String(value(model.wait, "message", "Message") || "").trim();
    return {
      tone: model.waitKind === "failed" || model.waitKind === "budget" ? "danger" : "await",
      heading:
        model.waitKind === "failed"
          ? "Now · workflow step failed"
          : model.waitKind === "gate"
            ? "Now · waiting for your review"
            : `Now · ${model.activity.toLowerCase()}`,
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
      // The thread age uses the model's clock: it is the same elapsed-time
      // rendering as dwell, so the two must not drift under a fixed clock.
      // Models built without a clock (bare test payloads) fall back to the
      // wall clock, matching the pre-clock behavior.
      age: formatDwell(value(thread, "created_at", "CreatedAt"), model.now ?? Date.now()),
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
      return [{ label: "Answer", key: "focus-gate", primary: true, tab: model.review?.gate?.changeGate ? "change" : "review" }];
    case "question":
      return [{ label: "Answer", key: "focus-gate", primary: true, tab: "review" }];
    case "failed":
      return [
        { label: "Retry", key: "workflow-retry", primary: true },
        { label: "Skip step", key: "workflow-skip" },
        { label: "Transcript", key: "open-transcript" },
      ];
    case "budget":
      return [
        {
          label: String(value(model.wait, "reason", "Reason") || "") === "review_cycle_limit" ? "Grant cycles" : "Extend budget",
          key: "workflow-budget",
          budgetKind:
            String(value(model.wait, "reason", "Reason") || "") === "review_cycle_limit"
              ? "review-cycles"
              : "transitions",
          primary: true,
        },
      ];
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
// LIFECYCLE_TARGET_OPTIONS is the single vocabulary the lifecycle control
// enumerates. It mirrors the server's AllLifecycleTransitionTargets (coordinator/phase.go)
// and DoneResolution constants (flow_graph.go). The parity test
// lifecycle_parity_test.go ensures they stay in lockstep.
export const LIFECYCLE_TARGET_OPTIONS = [
  { value: "backlog", label: "Backlog \u00b7 Unscheduled", done: false, group: "Active" },
  { value: "unscheduled", label: "Backlog \u00b7 Unscheduled", done: false, group: "Active" },
  { value: "up_next", label: "Up Next (Scheduled)", done: false, group: "Active" },
  { value: "scheduled", label: "Up Next (Scheduled)", done: false, group: "Active" },
  { value: "working", label: "Working (In Progress)", done: false, group: "Active" },
  { value: "triage", label: "Triage", done: false, group: "Active" },
  { value: "critique", label: "Critique \u00b7 Needs Changes", done: false, group: "Review" },
  { value: "acceptance", label: "Acceptance", done: false, group: "Review" },
  { value: "approved", label: "Approved", done: false, group: "Review" },
  { value: "retry", label: "Retry failed step", done: false, group: "Active" },
  { value: "skip", label: "Skip failed step", done: false, group: "Active" },
  { value: "hold", label: "Pause (Hold)", done: false, group: "Active" },
  { value: "resume", label: "Resume (Release)", done: false, group: "Active" },
  { value: "reset", label: "Reset to Unscheduled", done: false, group: "Active" },
  { value: "schedule", label: "Schedule", done: false, group: "Active" },
  { value: "reopen", label: "Reopen", done: false, group: "Active" },
  { value: "done:completed", label: "Done \u2014 Completed", done: true, group: "Terminal" },
  { value: "done:rejected", label: "Done \u2014 Rejected", done: true, group: "Terminal" },
  { value: "done:abandoned", label: "Done \u2014 Abandoned", done: true, group: "Terminal" },
  { value: "done:cancelled", label: "Done \u2014 Cancelled", done: true, group: "Terminal" },
  { value: "done:failed", label: "Done \u2014 Failed", done: true, group: "Terminal" },
];

export function lifecycleModel(model) {
  if (!model) return null;
  const phase = String(model.lifecyclePhase || "").trim() || String(model.lifecycleState || "unscheduled");
  return {
    phase,
    lifecycleState: model.lifecycleState,
    runState: model.runState,
    waitKind: model.waitKind,
    held: Boolean(model.held),
    runID: String(model.runID || ""),
    nodeRunID: String(model.nodeRunID || ""),
    isDone: model.lifecycleState === "done",
    hasActiveRun: Boolean(model.runID),
    convergenceEvidence: model.convergenceEvidence || null,
    canRetry: !model.held && !model.convergenceEvidence && Boolean(model.runID),
    canSkip: !model.held && Boolean(model.nodeRunID),
  };
}

export function lifecycleOptionsForModel(model) {
  const lm = lifecycleModel(model);
  if (!lm) return [];
  const current = String(lm.phase || lm.lifecycleState || "").trim().toLowerCase();
  return LIFECYCLE_TARGET_OPTIONS.map((option) => {
    let disabled = false;
    let reason = "";
    const target = option.value.toLowerCase();
    if (target === current || (target.startsWith("done:") && lm.isDone)) {
      disabled = false;
    }
    if ((target === "retry" || target === "skip") && !lm.hasActiveRun) {
      disabled = true;
      reason = "No active workflow run";
    }
    if ((target === "skip") && !lm.nodeRunID) {
      disabled = true;
      reason = "No retryable step";
    }
    if ((target === "hold" || target === "pause") && lm.held) {
      disabled = true;
      reason = "Already held";
    }
    if ((target === "resume" || target === "release") && !lm.held) {
      disabled = true;
      reason = "Not held";
    }
    if (lm.convergenceEvidence && (target === "retry" || target === "skip" || target === "reset")) {
      disabled = true;
      reason = "Convergence hold requires explicit disposition";
    }
    if (target === "reopen" && !lm.isDone) {
      disabled = true;
      reason = "Only Done tasks can be reopened";
    }
    return { ...option, disabled, reason };
  });
}

export function tabBadges(model) {
  const badges = {};
  if (model.review?.gate || model.review?.question) badges.review = { text: "!", tone: "warn" };
  if (model.change) badges.change = { text: String(model.openThreads || ""), tone: model.openThreads ? "warn" : "" };
  if (model.checks.length) {
    const ok = model.checksSatisfied === model.checks.length;
    badges.checks = { text: `${model.checksSatisfied}/${model.checks.length}`, tone: ok ? "ok" : "danger" };
  }
  const unresolvedFindings = Number(value(model.findings?.summary, "unresolved", "Unresolved") || 0);
  if (unresolvedFindings > 0) badges.findings = { text: String(unresolvedFindings), tone: "warn" };
  const activity = model.transitions.length + model.statusLog.length;
  if (activity) badges.activity = { text: String(activity), tone: "idle" };
  if (model.terminalAvailable) badges.terminal = { live: true };
  return badges;
}
