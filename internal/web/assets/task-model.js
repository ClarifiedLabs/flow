// Pure derivations for task detail. The rail, the Now card, the run spine and
// the tabs all read from one projection, so the page cannot contradict itself
// about what the task is doing. taskModel below is the whole page in one
// object; the per-surface projections live under models/ and are re-exported
// here so existing importers keep working.

import { workflowActivityLabel } from "./board.js";
import { formatDwell, waitKindOf } from "./board-model.js";
import { value } from "./normalize.js";
import { buildWorkItemIndex, groupWorkItemRelations, taskWorkContext, workItemAncestors, workItemID } from "./work-item-model.js";
import { projectWorkHref, resolveWorkNavigation } from "./work-nav.js";
import { runRows } from "./models/task-run.js";
import { reviewModel } from "./models/review.js";
import { epicParent, relationGroups } from "./models/relations.js";

export { nodeState, nodeDuration, runRows } from "./models/task-run.js";
export { parseWaitDetails, reviewModel } from "./models/review.js";
export { blockerVerdict, epicParent, LIFECYCLE_DONE, LIFECYCLE_UNFINISHED, RELATION_GROUPS, relationGroups } from "./models/relations.js";
export { LIFECYCLE_TARGET_OPTIONS, lifecycleModel, lifecycleOptionsForModel } from "./models/lifecycle-options.js";
export { isOutdatedAnchor, nowCardModel, tabBadges } from "./models/now-card.js";

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
  // taskThreads is the task-wide thread list across every change; threads
  // above is the current change's subset that the Change tab and the Now card
  // read. Payloads that only carry threads (older servers, bare test models)
  // fall back to it so the tab and its badge never see less than the change.
  const taskThreads = value(data, "task_threads", "TaskThreads") || threads;
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
    taskThreads,
    taskOpenThreads: taskThreads.filter((thread) => value(thread, "state", "State") === "open").length,
    changes,
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
