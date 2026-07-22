// Task views: detail page, read-only summary, the create/edit form and its
// flow selector, and the edit-form toggle.

import { apiGet, taskAPIBase } from "./api.js";
import { renderHumanAttentionPanel } from "./attention.js";
import { renderPhaseBadge, renderReviewBadge } from "./board.js";
import { renderCheck } from "./diff.js";
import { formatDate } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { projectButtonAttr, renderAttachmentUploadForm, renderTaskAttachment } from "./task.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { renderTerminalButton } from "./terminal.js";
import { renderTaskChange, renderRelation, renderTag, renderTimeline } from "./timeline.js";

export async function renderNewTaskView(app, context) {
  if (context && !app.isActiveLoad(context)) return false;
  const defaultProject = defaultCreateProject(app, "");
  if (defaultProject) await app.ensureFlows(defaultProject);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("New Task");
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>New Task</h2>
        </div>
      </div>
      ${renderTaskFormView(app, { priority: 0 }, { mode: "create", submitLabel: "Create" })}
    </section>
  `;
  app.bindTaskActions(() => renderNewTaskView(app, context));
  return true;
}

// defaultCreateProject picks the project whose flows the create form should
// offer first: an explicit id, then the single active project, then the sole
// registered project.
export function defaultCreateProject(app, projectID) {
  const explicit = String(projectID || "").trim();
  if (explicit) return explicit;
  const projects = app.projects || [];
  const selected = app.selectedProjectIDs();
  if (selected.length === 1) return selected[0];
  if (projects.length === 1) return value(projects[0], "id", "ID");
  return "";
}

// flowSelectOptionsView renders the <option>s for the flow selector from the
// per-project flow cache (app.ensureFlows). The project default is marked
// "(default)"; it is preselected for create mode and as the edit-mode
// fallback. Falls back to a single "Project default" option when no flows are
// loaded for the project yet.
export function flowSelectOptionsView(app, projectID, selectedFlowID) {
  const cache = (app.flowsByProject && app.flowsByProject.get(String(projectID || "").trim())) || { flows: [], defaultFlowID: "" };
  const flows = cache.flows || [];
  const defaultFlowID = cache.defaultFlowID || "";
  if (!flows.length) {
    return `<option value="" selected>Project default</option>`;
  }
  const selected = String(selectedFlowID || "").trim() || defaultFlowID;
  return flows.map((flow) => {
    const id = value(flow, "id", "ID");
    const name = value(flow, "name", "Name") || id;
    const isDefault = id === defaultFlowID || Boolean(value(flow, "default", "Default"));
    const label = isDefault ? `${name} (default)` : name;
    return `<option value="${escapeAttr(id)}" ${id === selected ? "selected" : ""}>${escapeHTML(label)}</option>`;
  }).join("");
}

export function renderTaskFormView(app, task, options = {}) {
  const mode = options.mode || "edit";
  const taskID = options.taskID || "";
  const submitLabel = options.submitLabel || "Save";
  const projectID = options.projectID || "";
  const projects = app.projects || [];
  const defaultProject = defaultCreateProject(app, projectID);
  const projectOptions = projects.map((project) => {
    const id = value(project, "id", "ID");
    const name = value(project, "name", "Name") || id;
    return `<option value="${escapeAttr(id)}" ${id === defaultProject ? "selected" : ""}>${escapeHTML(name)}</option>`;
  }).join("");
  const projectField = mode === "create"
    ? `
      <label class="task-field-project">
        <span>Project</span>
        <select name="project" required>
          ${projectOptions || `<option value="" selected>No projects available</option>`}
        </select>
      </label>`
    : "";
  const selectedFlowID = value(task, "flow_id", "FlowID");
  const flowOptions = flowSelectOptionsView(app, defaultProject, selectedFlowID);
  return `
    <form class="task-form" data-task-form="${escapeAttr(taskID)}" data-task-form-mode="${escapeAttr(mode)}"${projectID ? ` data-project="${escapeAttr(projectID)}"` : (mode === "create" && projects.length === 1 ? ` data-project="${escapeAttr(value(projects[0], "id", "ID"))}"` : "")}>
      ${projectField}
      <label class="task-field-priority">
        <span>Priority</span>
        <input name="priority" type="number" min="0" step="1" value="${Number(value(task, "priority", "Priority") || 0)}">
      </label>
      <label class="task-field-flow">
        <span>Flow</span>
        <select name="flow_id" data-flow-select>
          ${flowOptions}
        </select>
      </label>
      <label class="task-field-title wide">
        <span>Title</span>
        <input name="title" value="${escapeAttr(value(task, "title", "Title"))}" required>
      </label>
      <label class="wide">
        <span>Body</span>
        <textarea name="body" rows="8">${escapeHTML(value(task, "body", "Body"))}</textarea>
      </label>
      ${mode === "create" ? `
      <label class="wide">
        <span>Attachments</span>
        <input name="attachments" type="file" multiple>
      </label>` : ""}
      ${mode === "create" ? `
      <label class="check wide">
        <input name="queue_task" type="checkbox" checked>
        <span>Queue after creation</span>
      </label>` : ""}
      <div class="form-actions">
        <button class="button" type="submit">${escapeHTML(submitLabel)}</button>
      </div>
    </form>
  `;
}

// renderFlowSummaryLineView describes the task's flow as a one-line summary:
// the flow name plus its phase chain (e.g. "spec(gate) -> implement", each
// phase optionally annotated with its agent). It prefers the live flow status
// (data.flow, which carries the frozen phases + agent names); when the task is
// not yet scheduled it falls back to the selected/default flow name from the
// per-project flow cache.
export function renderFlowSummaryLineView(app, task, flow, projectID) {
  const phases = value(flow, "phases", "Phases") || [];
  if (flow && phases.length) {
    const flowName = value(flow, "flow_name", "FlowName");
    const chain = phases.map((phase) => {
      const name = value(phase, "name", "Name");
      const gate = value(phase, "gate", "Gate") === "human" ? "(gate)" : "";
      const agentName = value(phase, "agent_name", "AgentName");
      return `${escapeHTML(name)}${gate}${agentName ? ` · ${escapeHTML(agentName)}` : ""}`;
    }).join(" -> ");
    return `Flow <strong>${escapeHTML(flowName || "")}</strong> · ${chain}`;
  }
  const flowID = String(value(task, "flow_id", "FlowID") || "").trim();
  const cache = (app.flowsByProject && app.flowsByProject.get(String(projectID || "").trim())) || { flows: [], defaultFlowID: "" };
  const targetID = flowID || cache.defaultFlowID;
  const match = (cache.flows || []).find((candidate) => value(candidate, "id", "ID") === targetID);
  if (match) {
    const name = value(match, "name", "Name") || value(match, "id", "ID");
    const isDefault = value(match, "id", "ID") === cache.defaultFlowID;
    return `Flow <strong>${escapeHTML(name)}</strong>${!flowID && isDefault ? " (default)" : ""}`;
  }
  return `<span class="muted">No flow</span>`;
}

export function renderTaskReadOnlyDetailView(app, task, options = {}) {
  const taskID = options.taskID || "";
  const projectID = options.projectID || "";
  const flow = options.flow || null;
  const priority = Number(value(task, "priority", "Priority") || 0);
  const body = value(task, "body", "Body") || "";
  const title = value(task, "title", "Title") || "";
  const flowLine = renderFlowSummaryLineView(app, task, flow, projectID);
  return `
    <div class="task-read-only-detail" data-task-read-only>
      <div class="task-read-only-head">
        <h3>Detail</h3>
        <button class="button secondary" type="button" data-task-edit-toggle${projectButtonAttr(projectID)}>Edit</button>
      </div>
      <div class="task-read-only-body" data-task-read-only-body>
        <p class="meta-quiet">p${priority}</p>
        <p class="task-read-only-flow">${flowLine}</p>
        <p class="task-read-only-field"><span class="meta-quiet">Title</span><br>${escapeHTML(title)}</p>
        <div class="task-read-only-field"><span class="meta-quiet">Body</span>${body ? renderMarkdown(body) : "<br><span class=\"muted\">—</span>"}</div>
      </div>
      <div class="task-read-only-form" data-task-edit-form hidden>
        ${renderTaskFormView(app, task, { taskID, projectID })}
      </div>
    </div>
  `;
}

export async function renderTaskView(app, id, context, projectID = "") {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;
  const resolvedProject = data.project_id || data.ProjectID || projectID;
  const workflowData = await apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/workflow`).catch(() => null);
  await app.ensureFlows(resolvedProject);
  if (context && !app.isActiveLoad(context)) return false;
  const flow = data.flow || data.Flow || null;
  const projectName = data.project_name || data.ProjectName || "";
  app.setTitle(projectName ? `Task · ${projectName}` : "Task");
  const task = data.task || data.Task;
  const taskID = value(task, "id", "ID");
  const lifecycleState = value(task, "state", "State") || "unscheduled";
  const statusLog = data.status_log || data.StatusLog || [];
  const detail = data.task_detail || data.TaskDetail || {};
  const tags = value(detail, "tags", "Tags") || [];
  const relations = value(detail, "relations", "Relations") || [];
  const sessions = value(detail, "sessions", "Sessions") || [];
  const changes = value(detail, "changes", "Changes") || [];
  const readyChange = value(detail, "ready_change", "ReadyChange");
  const checks = value(detail, "checks", "Checks") || [];
  const attachments = value(detail, "attachments", "Attachments") || [];
  const transitions = value(detail, "transitions", "Transitions") || [];
  // Enriched transitions carry decoded session_id/session_state/head_sha/
  // change_id on session-related rows; fall back to the raw transitions for
  // the unified timeline when the backend payload is absent.
  const timelineTransitions = value(detail, "timeline_transitions", "TimelineTransitions") || transitions;
  const activeSession = value(detail, "active_session", "ActiveSession");
  const terminalAvailable = Boolean(value(detail, "terminal_available", "TerminalAvailable") || value(activeSession, "terminal_available", "TerminalAvailable"));
  const terminalJobID = value(detail, "terminal_job_id", "TerminalJobID");
  const reviewState = value(detail, "review_state", "ReviewState");
  const requiredChecks = value(detail, "required_checks", "RequiredChecks") || {};
  const taskConsole = value(detail, "task_console", "TaskConsole") || {};
  const taskConsoleJob = value(taskConsole, "job", "Job") || null;
  const taskConsoleSession = value(taskConsole, "session", "Session") || null;
  const taskConsoleActive = Boolean(value(taskConsole, "active", "Active") || taskConsoleJob || taskConsoleSession);
  const checkTotal = Number(value(requiredChecks, "total", "Total") || 0);
  const checkSatisfied = Number(value(requiredChecks, "satisfied", "Satisfied") || 0);
  const activeSessionID = value(activeSession, "id", "ID");
  const activeSessionTerminalAvailable = Boolean(value(activeSession, "terminal_available", "TerminalAvailable"));
  const pauseResumeHTML = "";
  const taskConsoleHref = `/ui/console?project=${encodeURIComponent(resolvedProject || "")}&task=${encodeURIComponent(taskID)}`;
  const taskConsoleHTML = lifecycleState === "done" || !taskConsoleActive
    ? ""
    : `<a class="button secondary" href="${escapeAttr(taskConsoleHref)}" data-link>Open Console</a><button class="button secondary" data-release-task-console="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Release Console</button>`;
  const tagsHTML = tags.length ? `<h3>Tags</h3><p class="meta-quiet">${tags.map(renderTag).filter(Boolean).join(" · ")}</p>` : "";
  const relationsHTML = relations.length ? `<h3>Relationships</h3><div class="feed">${relations.map((relation) => renderRelation(relation, taskID, resolvedProject)).join("")}</div>` : "";
  const readyChangeHTML = readyChange ? `<h3>Ready Change</h3><div class="feed">${renderTaskChange(readyChange)}</div>` : "";
  const changesHTML = changes.length ? `<h3>Changes</h3><div class="feed">${changes.map(renderTaskChange).join("")}</div>` : "";
  const checksHTML = checks.length ? `<h3>Checks</h3><div class="check-list">${checks.map((check) => renderCheck(check, taskID, resolvedProject)).join("")}</div>` : "";
  const attachmentsHTML = attachments.length ? `<h3>Attachments</h3><div class="attachment-list">${attachments.map((attachment) => renderTaskAttachment(attachment, taskID, resolvedProject)).join("")}</div>` : "";
  const attachmentUploadHTML = renderAttachmentUploadForm(taskID, resolvedProject);
  const attentionHTML = renderHumanAttentionPanel(task, statusLog, resolvedProject, activeSession);
  const workflowDetail = value(workflowData || {}, "detail", "Detail") || null;
  const workflowRun = value(workflowDetail || {}, "run", "Run") || null;
  const workflowWait = value(workflowDetail || {}, "open_wait", "OpenWait") || null;
  const workflowNodeRuns = value(workflowDetail || {}, "node_runs", "NodeRuns") || [];
  const snapshot = value(workflowRun || {}, "snapshot", "Snapshot") || {};
  const currentNodeKey = value(workflowRun || {}, "current_node_key", "CurrentNodeKey") || "";
  const currentNode = (value(snapshot, "nodes", "Nodes") || []).find((node) => value(node, "key", "Key") === currentNodeKey) || null;
  const gateConfig = value(value(currentNode || {}, "config", "Config") || {}, "human_gate", "HumanGate") || null;
  const gateOutcomes = value(gateConfig || {}, "outcomes", "Outcomes") || [];
  const workflowActions = lifecycleState === "unscheduled"
    ? `<button class="button" data-workflow-schedule="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Schedule</button>`
    : lifecycleState === "done"
      ? `<button class="button" data-workflow-reopen="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Reopen</button>`
      : `<button class="button secondary" data-workflow-reset="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Reset</button><button class="button secondary" data-workflow-done="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Done</button>`;
  const gatePanelHTML = workflowWait && value(workflowWait, "kind", "Kind") === "human_gate"
    ? `<section class="human-attention-panel"><div><h3>${escapeHTML(value(currentNode, "name", "Name") || "Human action required")}</h3><p>${escapeHTML(value(gateConfig, "instructions", "Instructions") || value(workflowWait, "message", "Message") || "Choose the next workflow outcome.")}</p><textarea data-workflow-feedback rows="3" placeholder="Optional feedback for the next node"></textarea></div><div class="actions">${gateOutcomes.map((outcome) => `<button class="button${outcome === "changes_requested" ? " secondary" : ""}" data-workflow-respond="${escapeAttr(value(workflowWait, "node_run_id", "NodeRunID"))}" data-task="${escapeAttr(taskID)}" data-outcome="${escapeAttr(outcome)}"${projectButtonAttr(resolvedProject)}>${escapeHTML(String(outcome).replaceAll("_", " "))}</button>`).join("")}</div></section>`
    : workflowWait && value(workflowWait, "kind", "Kind") === "operator_intervention"
      ? `<section class="human-attention-panel"><div><h3>Workflow paused</h3><p>${escapeHTML(value(workflowWait, "message", "Message") || "Operator action is required.")}</p></div><button class="button" data-workflow-budget="${escapeAttr(taskID)}"${projectButtonAttr(resolvedProject)}>Extend budget</button></section>`
      : "";
  const workflowHTML = workflowRun
    ? `<h3>Workflow</h3><p class="meta-quiet">${escapeHTML(value(snapshot, "flow_name", "FlowName") || value(workflowRun, "flow_id", "FlowID") || "Workflow")} · ${escapeHTML(currentNodeKey || "complete")} · transitions ${Number(value(workflowRun, "transitions_used", "TransitionsUsed") || 0)}/${Number(value(workflowRun, "transition_budget", "TransitionBudget") || 0)}</p><div class="feed">${workflowNodeRuns.map((nodeRun) => `<article class="feed-item"><strong>${escapeHTML(value(nodeRun, "node_key", "NodeKey"))}</strong><span>${escapeHTML(value(nodeRun, "state", "State"))}</span>${value(nodeRun, "outcome", "Outcome") ? `<p>${escapeHTML(value(nodeRun, "outcome", "Outcome"))}</p>` : ""}</article>`).join("")}</div>`
    : "";
  // The standalone Sessions and Status feeds are gone: they are folded into
  // the unified Timeline below, which removes the column-height imbalance
  // (the tall sessions list used to dominate the editor column) and the
  // duplicated session lifecycle shown in the old transitions feed.
  const timelineHTML = renderTimeline({ sessions, transitions: timelineTransitions, statusLog });
  const lifecycleHTML = (timelineTransitions.length || sessions.length || statusLog.length)
    ? `<h3>Activity</h3><div class="lifecycle-timeline">${timelineHTML}</div>`
    : "";
  // Read-only task detail (title, body and flow) with an Edit toggle that
  // reveals the full form without letting the timeline overwhelm task text.
  const readOnlyDetailHTML = renderTaskReadOnlyDetailView(app, task, { taskID, projectID: resolvedProject, flow });
  const editorHTML = [
    tagsHTML,
    relationsHTML,
    readOnlyDetailHTML,
    attachmentsHTML,
    attachmentUploadHTML,
  ].filter(Boolean).join("");
  const activityHTML = [readyChangeHTML, changesHTML].filter(Boolean).join("");
  const systemHTML = [workflowHTML, checksHTML].filter(Boolean).join("");
  const lifecycleSectionHTML = lifecycleHTML ? `<div class="task-detail-lifecycle">${lifecycleHTML}</div>` : "";
  const detailColumns = [
    `<div class="task-detail-column task-detail-editor">${editorHTML}</div>`,
    activityHTML ? `<div class="task-detail-column task-detail-activity">${activityHTML}</div>` : "",
    systemHTML ? `<div class="task-detail-column task-detail-system">${systemHTML}</div>` : "",
    lifecycleSectionHTML,
  ].filter(Boolean).join("");
  app.querySelector(".content").innerHTML = `
    <section class="detail task-detail">
      <div class="detail-head task-detail-head">
        <div>
          <h2>${escapeHTML(taskID)} · ${escapeHTML(value(task, "title", "Title"))}</h2>
          <div class="meta">
            ${renderPhaseBadge(lifecycleState)}
            ${reviewState ? renderReviewBadge(reviewState) : ""}
            ${checkTotal ? `<span class="badge ${checkSatisfied === checkTotal ? "ok" : "idle"}">checks ${checkSatisfied}/${checkTotal}</span>` : ""}
          </div>
          <p class="meta-quiet">p${Number(value(task, "priority", "Priority") || 0)}${flowHeaderMeta(flow) ? ` · ${escapeHTML(flowHeaderMeta(flow))}` : ""}</p>
        </div>
        <div class="actions">
          ${workflowActions}
          ${activeSessionID && (activeSessionTerminalAvailable || (terminalAvailable && !terminalJobID)) ? renderTerminalButton("session", activeSessionID) : ""}
          ${terminalJobID && terminalAvailable && !(activeSessionID && (activeSessionTerminalAvailable || (terminalAvailable && !terminalJobID))) ? renderTerminalButton("job", terminalJobID) : ""}
          ${pauseResumeHTML}
          ${taskConsoleHTML}
        </div>
      </div>
      <div class="summary-grid">
        <div><span>Created</span><strong>${escapeHTML(value(task, "created_by", "CreatedBy"))}</strong></div>
        <div><span>Source Task</span><strong>${escapeHTML(value(task, "source_task_id", "SourceTaskID") || "")}</strong></div>
        <div><span>Source Change</span><strong>${escapeHTML(value(task, "source_change_id", "SourceChangeID") || "")}</strong></div>
        <div><span>Updated</span><strong>${escapeHTML(formatDate(value(task, "updated_at", "UpdatedAt")))}</strong></div>
      </div>
      ${gatePanelHTML}
      ${attentionHTML}
      <div class="task-detail-grid">${detailColumns}</div>
    </section>
  `;
  app.bindTaskActions(() => renderTaskView(app, id, undefined, resolvedProject));
  return true;
}

export function toggleTaskEditFormView(app, button) {
  const container = button.closest("[data-task-read-only]");
  if (!container) return;
  const body = container.querySelector("[data-task-read-only-body]");
  const formWrap = container.querySelector("[data-task-edit-form]");
  if (!body || !formWrap) return;
  const editing = formWrap.hidden;
  body.hidden = editing;
  formWrap.hidden = !editing;
  button.textContent = editing ? "Cancel" : "Edit";
  button.dataset.taskEditToggleState = editing ? "editing" : "";
  if (editing) {
    const form = formWrap.querySelector("[data-task-form]");
    if (form) bindTaskFlowControlsView(app, form);
  }
}

// bindTaskFlowControlsView refreshes the flow selector when the create form's
// project select changes: it fetches (and caches) that project's flows, then
// re-renders the flow <option>s for the newly chosen project.
export function bindTaskFlowControlsView(app, form) {
  const projectSelect = form?.elements?.project;
  const flowSelect = form?.elements?.flow_id;
  if (!projectSelect || !flowSelect || typeof projectSelect.addEventListener !== "function") return;
  projectSelect.addEventListener("change", async () => {
    const projectID = String(projectSelect.value || "").trim();
    if (projectID) await app.ensureFlows(projectID);
    flowSelect.innerHTML = flowSelectOptionsView(app, projectID, "");
  });
}

// flowHeaderMeta condenses the live flow status into the task header's meta
// line: "<flow name> · <phase> <n>/<count>" (1-based). Empty when there is no
// flow cursor yet.
export function flowHeaderMeta(flow) {
  if (!flow) return "";
  const flowName = value(flow, "flow_name", "FlowName");
  const phaseName = value(flow, "phase_name", "PhaseName");
  const phaseCount = Number(value(flow, "phase_count", "PhaseCount") || 0);
  const phaseIndex = Number(value(flow, "phase_index", "PhaseIndex") || 0);
  const phasePart = phaseName && phaseCount ? `${phaseName} ${phaseIndex + 1}/${phaseCount}` : phaseName;
  return [flowName, phasePart].filter(Boolean).join(" · ");
}
