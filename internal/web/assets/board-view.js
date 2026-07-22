// Board (kanban) view: lanes, task cards, the inline Done-lane preview and its
// controls, plus the new-task create action.

import { apiGet, taskHref } from "./api.js";
import { renderStatusKindBadge } from "./attention.js";
import { flattenDonePage, laneTasks, phaseKey, renderPhaseBadge, renderUniqueCardLabel } from "./board.js";
import { LANES } from "./config.js";
import { formatDate } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { boardDoneConfig, writeBoardDoneConfig } from "./storage.js";
import { renderTerminalButton } from "./terminal.js";
import { renderDiffStatText, renderHandoffSummary, renderRelationSummary, renderTag, uniqueCardTags } from "./timeline.js";
import { doneQueryView, renderDoneRowView } from "./done-view.js";

export async function renderBoardView(app, filter, context) {
  const showDone = !filter;
  const [data, doneData] = await Promise.all([
    apiGet("/v2/board" + app.projectQuery()),
    showDone ? apiGet("/v2/done" + boardDoneQueryView(app)).catch(() => null) : Promise.resolve(null),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle(filter ? filter.label : "Board");
  // The aggregate response carries one board per project; keep each card tied
  // to its board for project badges, scoped actions, and overlay maps.
  const boards = data.boards || data.Boards || [];
  const showProjectBadge = (app.projects || []).length > 1;
  const lanes = filter ? LANES.filter(([key]) => key === filter.lane) : LANES;
  const lanesHTML = lanes.map(([key, label, field]) => {
    const entries = [];
    for (const projectBoard of boards) {
      const board = projectBoard.board || projectBoard.Board || {};
      const cards = projectBoard.task_cards || projectBoard.TaskCards || {};
      const laneStates = projectBoard.lane_states || projectBoard.LaneStates || {};
      const waitReasons = projectBoard.wait_reasons || projectBoard.WaitReasons || {};
      const blockedIDs = new Set(projectBoard.blocked_ids || projectBoard.BlockedIDs || []);
      const project = {
        id: projectBoard.project_id || projectBoard.ProjectID || "",
        name: projectBoard.project_name || projectBoard.ProjectName || "",
        badge: showProjectBadge,
      };
      let tasks = laneTasks(board, key, field);
      if (filter && filter.state) tasks = tasks.filter((task) => laneStates[value(task, "id", "ID")] === filter.state);
      for (const task of tasks) {
        const taskID = value(task, "id", "ID");
        entries.push({
          task,
          card: cards[taskID] || {},
          laneState: laneStates[taskID] || "",
          waitReason: waitReasons[taskID] || "",
          blocked: blockedIDs.has(taskID),
          project,
        });
      }
    }
    return renderLaneView(app, filter ? filter.label : label, entries, key);
  }).join("");
  app.querySelector(".content").innerHTML = `
    <div class="board">
      ${lanesHTML}
      ${showDone ? renderBoardDoneLaneView(app, doneData) : ""}
    </div>
  `;
  app.bindTaskActions(() => renderBoardView(app, filter));
  if (showDone) bindBoardDoneControlsView(app, filter);
  return true;
}

export function boardDoneQueryView(app) {
  const config = boardDoneConfig();
  const extra = config.mode === "within" ? { within: config.within } : { limit: config.count };
  return doneQueryView(app, config.outcome, extra);
}

export function renderBoardDoneLaneView(app, doneData) {
  const { entries } = doneData ? flattenDonePage(doneData, (app.projects || []).length > 1) : { entries: [] };
  const rows = entries.length
    ? entries.map((entry) => renderDoneRowView(app, entry)).join("")
    : `<div class="empty">No closed tasks</div>`;
  return `
    <section class="lane" data-lane="done">
      <h2>Done · ${entries.length}</h2>
      ${renderBoardDoneControlsView(app, boardDoneConfig())}
      <div class="cards">${rows}</div>
    </section>
  `;
}

export function renderBoardDoneControlsView(app, config) {
  const outcomes = [["all", "All"], ["completed", "Completed"], ["merged", "Merged"], ["rejected", "Rejected"], ["abandoned", "Abandoned"], ["cancelled", "Cancelled"], ["failed", "Failed"]];
  const outcomeChips = outcomes.map(([key, label]) =>
    `<button class="chip${config.outcome === key ? " active" : ""}" data-board-done-outcome="${escapeAttr(key)}"${config.outcome === key ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`
  ).join("");
  const scopeOption = (mode, val, label) => {
    const selected = config.mode === mode && (mode === "count" ? config.count === val : config.within === val);
    return `<option value="${escapeAttr(mode + ":" + val)}"${selected ? " selected" : ""}>${escapeHTML(label)}</option>`;
  };
  return `
    <div class="board-done-controls">
      <select class="board-done-scope" data-board-done-scope aria-label="How many done items to show">
        ${scopeOption("count", 10, "Last 10")}
        ${scopeOption("count", 20, "Last 20")}
        ${scopeOption("count", 50, "Last 50")}
        ${scopeOption("within", "1d", "Past 24h")}
        ${scopeOption("within", "7d", "Past 7d")}
        ${scopeOption("within", "30d", "Past 30d")}
      </select>
      <div class="board-done-filters" role="group" aria-label="Filter by outcome">${outcomeChips}</div>
    </div>
  `;
}

export function bindBoardDoneControlsView(app, filter) {
  const lane = app.querySelector('.lane[data-lane="done"]');
  if (!lane) return;
  const scope = lane.querySelector("[data-board-done-scope]");
  if (scope) {
    scope.addEventListener("change", () => {
      const [mode, val] = String(scope.value).split(":");
      const config = boardDoneConfig();
      if (mode === "within") {
        config.mode = "within";
        config.within = val;
      } else {
        config.mode = "count";
        config.count = Number(val) || 20;
      }
      writeBoardDoneConfig(config);
      refreshBoardDoneLaneView(app, filter);
    });
  }
  lane.querySelectorAll("[data-board-done-outcome]").forEach((button) => {
    button.addEventListener("click", () => {
      const config = boardDoneConfig();
      if (config.outcome === button.dataset.boardDoneOutcome) return;
      config.outcome = button.dataset.boardDoneOutcome;
      writeBoardDoneConfig(config);
      refreshBoardDoneLaneView(app, filter);
    });
  });
}

export async function refreshBoardDoneLaneView(app, filter) {
  const lane = app.querySelector('.lane[data-lane="done"]');
  if (!lane) return;
  let doneData = null;
  try {
    doneData = await apiGet("/v2/done" + boardDoneQueryView(app));
  } catch (error) {
    app.setStatus(error.message || String(error));
    return;
  }
  lane.outerHTML = renderBoardDoneLaneView(app, doneData);
  bindBoardDoneControlsView(app, filter);
}

export async function createTaskView(app) {
  history.pushState({}, "", "/ui/tasks/new");
  await app.load();
}

export function renderLaneView(app, label, entries, laneKey = "") {
  const renderedCards = entries.length
    ? entries.map((entry, index) => renderTaskCardView(app, entry.task, entry.card, entry.laneState, entry.blocked, Math.min(index, 8), entry.project, entry.waitReason)).join("")
    : `<div class="empty">No tasks</div>`;
  return `<section class="lane" data-lane="${escapeAttr(laneKey)}"><h2>${escapeHTML(label)} · ${entries.length}</h2><div class="cards">${renderedCards}</div></section>`;
}

export function renderTaskCardView(app, task, card, laneState, blocked, stagger = 0, project = null, waitReason = "") {
  const projectID = project && project.id ? project.id : "";
  const projectAttr = projectID ? ` data-project="${escapeAttr(projectID)}"` : "";
  const taskID = value(task, "id", "ID");
  const title = value(task, "title", "Title");
  const lifecycleState = value(task, "state", "State") || "unscheduled";
  const priority = Number(value(task, "priority", "Priority") || 0);
  const updatedAt = formatDate(value(task, "updated_at", "UpdatedAt"));
  const source = value(task, "created_by", "CreatedBy");
  const activeSession = value(card, "active_session", "ActiveSession");
  const terminalAvailable = Boolean(value(card, "terminal_available", "TerminalAvailable") || value(activeSession, "terminal_available", "TerminalAvailable"));
  const change = value(card, "change", "Change");
  const diffStats = value(card, "diff_stats", "DiffStats");
  const diffUnavailableReason = value(card, "diff_unavailable_reason", "DiffUnavailableReason");
  const handoff = value(card, "handoff", "Handoff");
  const currentStep = value(card, "current_step", "CurrentStep") || {};
  const currentStepKey = String(value(currentStep, "key", "Key") || "").trim();
  const frozenStepName = String(value(currentStep, "name", "Name") || "");
  const currentStepName = frozenStepName.trim() ? frozenStepName : currentStepKey.replace(/[_-]+/g, " ").replace(/\s+/g, " ");
  const checks = value(card, "required_checks", "RequiredChecks") || {};
  const latestStatus = value(card, "latest_status", "LatestStatus");
  const terminalJobID = value(card, "terminal_job_id", "TerminalJobID");
  const blockers = value(card, "blockers", "Blockers") || {};
  const blockerTasks = value(blockers, "tasks", "Tasks") || [];
  const tags = value(card, "tags", "Tags") || [];
  const relations = value(card, "relations", "Relations") || {};
  const changeID = value(change, "id", "ID");
  const changeHeadSHA = value(change, "head_sha", "HeadSHA");
  const branch = value(change, "branch", "Branch") || value(activeSession, "branch", "Branch");
  const sessionID = value(activeSession, "id", "ID");
  const activeState = value(activeSession, "state", "State");
  const sessionTerminalAvailable = Boolean(value(activeSession, "terminal_available", "TerminalAvailable"));
  const checkTotal = Number(value(checks, "total", "Total") || 0);
  const checkSatisfied = Number(value(checks, "satisfied", "Satisfied") || 0);
  const statusMessage = value(latestStatus, "message", "Message");
  const statusKind = value(latestStatus, "kind", "Kind");
  const phaseState = laneState || lifecycleState;
  const cardState = blocked ? "blocked" : phaseState;
  const phaseSlug = phaseKey(cardState) || "unscheduled";
  const cardLabelKeys = new Set();
  const stateBadge = renderUniqueCardLabel(cardLabelKeys, cardState || "—", () => renderPhaseBadge(cardState));
  const checkProgress = checkTotal
    ? renderUniqueCardLabel(cardLabelKeys, `checks ${checkSatisfied}/${checkTotal}`, () => `checks ${checkSatisfied}/${checkTotal}`)
    : "";
  const statusKindBadge = statusMessage && statusKind && statusKind !== "note"
    ? renderUniqueCardLabel(cardLabelKeys, statusKind, () => renderStatusKindBadge(statusKind))
    : "";
  const visibleTags = uniqueCardTags(tags, cardLabelKeys);
  const handoffSummary = waitReason ? renderHandoffSummary(handoff) : "";
  const blockerText = blockerTasks.map((blocker) => `${value(blocker, "id", "ID")} ${value(blocker, "title", "Title")}`.trim()).join(", ");
  const scheduleButton = lifecycleState === "unscheduled"
    ? `<button class="button" data-workflow-schedule="${escapeAttr(taskID)}"${projectAttr}>Schedule</button>`
    : "";
  const resetButton = lifecycleState === "scheduled" || lifecycleState === "in_progress"
    ? `<button class="button secondary" data-workflow-reset="${escapeAttr(taskID)}"${projectAttr}>Reset</button>`
    : "";
  const doneButton = lifecycleState !== "done"
    ? `<button class="button secondary" data-workflow-done="${escapeAttr(taskID)}"${projectAttr}>Done</button>`
    : "";
  const terminalButton = sessionID && (sessionTerminalAvailable || (terminalAvailable && !terminalJobID))
    ? renderTerminalButton("session", sessionID, { iconOnly: true })
    : terminalJobID && terminalAvailable
      ? renderTerminalButton("job", terminalJobID, { iconOnly: true })
    : "";
  const quiet = [
    project && project.badge && project.name ? `<span class="card-project-badge">${escapeHTML(project.name)}</span>` : "",
    `p${priority}`,
    source ? escapeHTML(source) : "",
    checkProgress,
    visibleTags.map(renderTag).filter(Boolean).join(" · "),
    renderRelationSummary(relations),
    changeID ? `<a href="/ui/changes/${escapeAttr(changeID)}" data-link>${escapeHTML(changeID)}</a>` : "",
    branch ? escapeHTML(branch) : "",
    activeState ? escapeHTML(activeState) : "",
    updatedAt ? escapeHTML(updatedAt) : "",
  ].filter(Boolean).join(" · ");
  return `
    <article class="card" data-phase="${escapeAttr(phaseSlug)}" style="--i:${Number(stagger) || 0}">
      <a class="card-title" href="${escapeAttr(taskHref(projectID, taskID))}" data-link>${escapeHTML(taskID)} · ${escapeHTML(title)}</a>
      <div class="meta">
        ${stateBadge}
      </div>
      ${currentStepName ? `<div class="card-current-step" title="${escapeAttr(currentStepName)}">${escapeHTML(currentStepName)}</div>` : ""}
      ${quiet ? `<p class="meta-quiet">${quiet}</p>` : ""}
      ${blockerText ? `<p class="card-status">${escapeHTML(blockerText)}</p>` : ""}
      ${statusMessage ? `<p class="card-status">${statusKindBadge}${renderMarkdown(statusMessage, { inline: true })}</p>` : ""}
      ${handoffSummary}
      ${scheduleButton || resetButton || doneButton || terminalButton ? `<div class="actions">${scheduleButton}${resetButton}${doneButton}${terminalButton}</div>` : ""}
    </article>
  `;
}
