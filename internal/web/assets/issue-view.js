// Issue views: detail page, read-only summary, the create/edit form and its
// per-agent harness/model controls, and the edit-form toggle.

import { apiGet, issueAPIBase } from "./api.js";
import { renderHumanAttentionPanel, renderLifecycleChart } from "./attention.js";
import { renderPhaseBadge, renderReviewBadge } from "./board.js";
import { DEFAULT_AGENT_HARNESSES } from "./config.js";
import { renderCheck } from "./diff.js";
import { formatDate } from "./format.js";
import { HARNESS_REASONING_UNAVAILABLE, findHarnessModel, harnessDefaultArgs, harnessModels, harnessReasoningLevelValues, normalizeHarnessArgs, normalizeHarnessModelList, parseHarnessSelectionArgs, parseJSONAttribute, renderHarnessArgsField, renderHarnessModelControls, renderHarnessModelFields, renderHarnessModelOptions, renderHarnessOptions, renderHarnessReasoningInto, renderShellArgString, resolveHarnessSelection, serializeHarnessModelSelection } from "./harness-models.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { currentIssueState, projectButtonAttr, renderAttachmentUploadForm, renderIssueAttachment, renderIssueStateForm } from "./issue.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { normalizeIssueAgentDefaults, readIssueAgentDefaults } from "./storage.js";
import { renderTerminalButton } from "./terminal.js";
import { renderIssueChange, renderRelation, renderTag, renderTimeline } from "./timeline.js";

export async function renderNewIssueView(app, context) {
  if (context && !app.isActiveLoad(context)) return false;
  await app.ensureHarnesses();
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("New Issue");
  app.querySelector(".content").innerHTML = `
    <section class="detail">
      <div class="detail-head">
        <div>
          <h2>New Issue</h2>
        </div>
      </div>
      ${renderIssueFormView(app, {
        ...readIssueAgentDefaults(),
        priority: 0,
        requires_human_review: true,
        auto_merge: false,
      }, { mode: "create", submitLabel: "Create" })}
    </section>
  `;
  app.bindIssueActions(() => renderNewIssueView(app, context));
  return true;
}

export function renderIssueFormView(app, issue, options = {}) {
  const mode = options.mode || "edit";
  const issueID = options.issueID || "";
  const submitLabel = options.submitLabel || "Save";
  const issueHarnessArgs = normalizeHarnessArgs(value(issue, "harness_args", "HarnessArgs"));
  const agentOptions = (app.harnesses && app.harnesses.agents) || DEFAULT_AGENT_HARNESSES;
  const agentHarness = resolveHarnessSelection(agentOptions, value(issue, "agent_harness", "AgentHarness") || "codex", mode !== "create");
  const selectionByHarness = {
    codex: parseHarnessSelectionArgs(issueHarnessArgs.codex, harnessModels(agentOptions, "codex"), "codex"),
    claude: parseHarnessSelectionArgs(issueHarnessArgs.claude, harnessModels(agentOptions, "claude"), "claude"),
    harness: parseHarnessSelectionArgs(issueHarnessArgs.harness, harnessModels(agentOptions, "harness"), "harness"),
  };
  const agentArgsByHarness = {
    codex: renderShellArgString(selectionByHarness.codex.additional_args),
    claude: renderShellArgString(selectionByHarness.claude.additional_args),
    harness: renderShellArgString(selectionByHarness.harness.additional_args),
  };
  const agentDefaultsByHarness = {
    codex: renderShellArgString(harnessDefaultArgs(agentOptions, "codex")),
    claude: renderShellArgString(harnessDefaultArgs(agentOptions, "claude")),
    harness: renderShellArgString(harnessDefaultArgs(agentOptions, "harness")),
  };
  const agentArgs = (selectionByHarness[agentHarness] || {}).additional_args || issueHarnessArgs[agentHarness];
  const projectID = options.projectID || "";
  const projects = app.projects || [];
  const selectedProjects = app.selectedProjectIDs();
  const defaultProject = projectID || (selectedProjects.length === 1 ? selectedProjects[0] : (projects.length === 1 ? value(projects[0], "id", "ID") : ""));
  const projectOptions = projects.map((project) => {
    const id = value(project, "id", "ID");
    const name = value(project, "name", "Name") || id;
    return `<option value="${escapeAttr(id)}" ${id === defaultProject ? "selected" : ""}>${escapeHTML(name)}</option>`;
  }).join("");
  const projectField = mode === "create"
    ? `
      <label class="issue-field-project">
        <span>Project</span>
        <select name="project" required>
          ${projectOptions || `<option value="" selected>No projects available</option>`}
        </select>
      </label>`
    : "";
  return `
    <form class="issue-form" data-issue-form="${escapeAttr(issueID)}" data-issue-form-mode="${escapeAttr(mode)}"${projectID ? ` data-project="${escapeAttr(projectID)}"` : (mode === "create" && projects.length === 1 ? ` data-project="${escapeAttr(value(projects[0], "id", "ID"))}"` : "")}>
      ${projectField}
      <label class="issue-field-priority">
        <span>Priority</span>
        <input name="priority" type="number" min="0" step="1" value="${Number(value(issue, "priority", "Priority") || 0)}">
      </label>
      <label class="issue-field-agent">
        <span>Agent</span>
        <select name="agent_harness">
          ${renderHarnessOptions(agentOptions, agentHarness, mode !== "create")}
        </select>
      </label>
      ${renderHarnessModelFields(agentOptions, selectionByHarness, agentHarness)}
      ${renderHarnessArgsField("agent", "Additional Agent Args", agentArgs, harnessDefaultArgs(agentOptions, agentHarness), { values: agentArgsByHarness, defaults: agentDefaultsByHarness })}
      <label class="issue-field-title wide">
        <span>Title</span>
        <input name="title" value="${escapeAttr(value(issue, "title", "Title"))}" required>
      </label>
      <label class="wide">
        <span>Body</span>
        <textarea name="body" rows="8">${escapeHTML(value(issue, "body", "Body"))}</textarea>
      </label>
      <label class="wide">
        <span>Acceptance Criteria</span>
        <textarea name="acceptance_criteria" rows="6">${escapeHTML(value(issue, "acceptance_criteria", "AcceptanceCriteria"))}</textarea>
      </label>
      ${mode === "create" ? `
      <label class="wide">
        <span>Attachments</span>
        <input name="attachments" type="file" multiple>
      </label>` : ""}
      <label class="check">
        <input name="requires_human_review" type="checkbox" ${value(issue, "requires_human_review", "RequiresHumanReview") ? "checked" : ""}>
        <span>Human review</span>
      </label>
      <label class="check">
        <input name="auto_merge" type="checkbox" ${value(issue, "auto_merge", "AutoMerge") ? "checked" : ""}>
        <span>Auto merge</span>
      </label>
      ${mode === "create" ? `
      <label class="check wide">
        <input name="plan_mode" type="checkbox" ${value(issue, "plan_mode", "PlanMode") ? "checked" : ""}>
        <span>Plan mode</span>
      </label>
      <label class="check wide">
        <input name="queue_issue" type="checkbox" checked>
        <span>Queue after creation</span>
      </label>` : ""}
      <div class="form-actions">
        <button class="button" type="submit">${escapeHTML(submitLabel)}</button>
        ${mode === "create" ? `<button class="button secondary" type="button" data-save-agent-defaults>Save as defaults</button>` : ""}
      </div>
    </form>
  `;
}

export function renderIssueReadOnlyDetailView(app, issue, options = {}) {
  const issueID = options.issueID || "";
  const projectID = options.projectID || "";
  const agentHarness = value(issue, "agent_harness", "AgentHarness") || "codex";
  const issueHarnessArgs = normalizeHarnessArgs(value(issue, "harness_args", "HarnessArgs"));
  const agentOptions = (app.harnesses && app.harnesses.agents) || DEFAULT_AGENT_HARNESSES;
  const selection = parseHarnessSelectionArgs(issueHarnessArgs[agentHarness], harnessModels(agentOptions, agentHarness), agentHarness);
  const selectionArgs = renderShellArgString(selection.additional_args);
  const modelLabel = selection.model || "";
  const reasoningLabel = selection.reasoning_effort || "";
  const defaultArgs = renderShellArgString(harnessDefaultArgs(agentOptions, agentHarness));
  const requiresHumanReview = value(issue, "requires_human_review", "RequiresHumanReview") ? "required" : "optional";
  const autoMerge = value(issue, "auto_merge", "AutoMerge") ? "on" : "off";
  const priority = Number(value(issue, "priority", "Priority") || 0);
  const body = value(issue, "body", "Body") || "";
  const acceptanceCriteria = value(issue, "acceptance_criteria", "AcceptanceCriteria") || "";
  const title = value(issue, "title", "Title") || "";
  const agentConfigParts = [
    `<strong>${escapeHTML(agentHarness)}</strong>`,
    modelLabel ? `model ${escapeHTML(modelLabel)}` : "",
    reasoningLabel ? `reasoning ${escapeHTML(reasoningLabel)}` : "",
    selectionArgs ? `args ${escapeHTML(selectionArgs)}` : "",
  ].filter(Boolean).join(" · ");
  const defaultsLine = defaultArgs ? `<p class="meta-quiet">Coordinator defaults: ${escapeHTML(defaultArgs)}</p>` : "";
  return `
    <div class="issue-read-only-detail" data-issue-read-only>
      <div class="issue-read-only-head">
        <h3>Detail</h3>
        <button class="button secondary" type="button" data-issue-edit-toggle${projectButtonAttr(projectID)}>Edit</button>
      </div>
      <div class="issue-read-only-body" data-issue-read-only-body>
        <p class="meta-quiet">p${priority} · human review ${escapeHTML(requiresHumanReview)} · auto merge ${escapeHTML(autoMerge)}</p>
        <p class="issue-read-only-agent">${agentConfigParts || "<span class=\"muted\">No agent configuration</span>"}</p>
        ${defaultsLine}
        <p class="issue-read-only-field"><span class="meta-quiet">Title</span><br>${escapeHTML(title)}</p>
        <div class="issue-read-only-field"><span class="meta-quiet">Body</span>${body ? renderMarkdown(body) : "<br><span class=\"muted\">—</span>"}</div>
        <div class="issue-read-only-field"><span class="meta-quiet">Acceptance Criteria</span>${acceptanceCriteria ? renderMarkdown(acceptanceCriteria) : "<br><span class=\"muted\">—</span>"}</div>
      </div>
      <div class="issue-read-only-form" data-issue-edit-form hidden>
        ${renderIssueFormView(app, issue, { issueID, projectID })}
      </div>
    </div>
  `;
}

export async function renderIssueView(app, id, context, projectID = "") {
  const data = await apiGet(`${issueAPIBase(projectID)}/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;
  await app.ensureHarnesses();
  if (context && !app.isActiveLoad(context)) return false;
  const resolvedProject = data.project_id || data.ProjectID || projectID;
  const projectName = data.project_name || data.ProjectName || "";
  app.setTitle(projectName ? `Issue · ${projectName}` : "Issue");
  const issue = data.issue || data.Issue;
  const issueID = value(issue, "id", "ID");
  const scheduleState = value(issue, "schedule_state", "ScheduleState");
  const triageState = value(issue, "triage_state", "TriageState");
  const statusLog = data.status_log || data.StatusLog || [];
  const detail = data.issue_detail || data.IssueDetail || {};
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
  const lifecycleGraph = value(detail, "lifecycle_graph", "LifecycleGraph");
  const activeSession = value(detail, "active_session", "ActiveSession");
  const terminalAvailable = Boolean(value(detail, "terminal_available", "TerminalAvailable") || value(activeSession, "terminal_available", "TerminalAvailable"));
  const terminalJobID = value(detail, "terminal_job_id", "TerminalJobID");
  const reviewState = value(detail, "review_state", "ReviewState");
  const requiredChecks = value(detail, "required_checks", "RequiredChecks") || {};
  const reviewCycleBudget = value(detail, "review_cycle_budget", "ReviewCycleBudget") || {};
  const waitReason = value(detail, "wait_reason", "WaitReason") || "";
  const crashRetryAvailable = Boolean(value(detail, "crash_retry_available", "CrashRetryAvailable"));
  const reviewCycleExhausted = Boolean(value(reviewCycleBudget, "exhausted", "Exhausted"));
  const reviewCycleGrant = Number(value(reviewCycleBudget, "default_grant_cycles", "DefaultGrantCycles") || 5);
  const issueConsole = value(detail, "issue_console", "IssueConsole") || {};
  const issueConsoleJob = value(issueConsole, "job", "Job") || null;
  const issueConsoleSession = value(issueConsole, "session", "Session") || null;
  const issueConsoleActive = Boolean(value(issueConsole, "active", "Active") || issueConsoleJob || issueConsoleSession);
  const checkTotal = Number(value(requiredChecks, "total", "Total") || 0);
  const checkSatisfied = Number(value(requiredChecks, "satisfied", "Satisfied") || 0);
  const activeSessionID = value(activeSession, "id", "ID");
  const activeSessionTerminalAvailable = Boolean(value(activeSession, "terminal_available", "TerminalAvailable"));
  const paused = Boolean(value(detail, "paused", "Paused"));
  const pauseResumeHTML = scheduleState === "closed"
    ? ""
    : activeSessionID
      ? `<button class="button secondary" data-pause="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Pause</button>`
      : paused
        ? `<button class="button" data-resume="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Resume</button>`
        : "";
  const issueConsoleHref = `/ui/console?project=${encodeURIComponent(resolvedProject || "")}&issue=${encodeURIComponent(issueID)}`;
  const issueConsoleHTML = scheduleState === "closed" || !(reviewCycleExhausted || paused || issueConsoleActive)
    ? ""
    : issueConsoleActive
      ? `<a class="button secondary" href="${escapeAttr(issueConsoleHref)}" data-link>Open Console</a><button class="button secondary" data-release-issue-console="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Release Console</button>`
      : `<button class="button secondary" data-start-issue-console="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Start Console</button>`;
  const reviewCycleApproveHTML = reviewCycleExhausted
    ? `<button class="button" data-review-cycles-approve="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Approve ${escapeHTML(String(reviewCycleGrant))} Cycles</button>`
    : "";
  const crashRetryHTML = waitReason === "crash_loop" || crashRetryAvailable
    ? `<button class="button" data-retry-crash="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Retry</button>`
    : "";
  const tagsHTML = tags.length ? `<h3>Tags</h3><p class="meta-quiet">${tags.map(renderTag).filter(Boolean).join(" · ")}</p>` : "";
  const relationsHTML = relations.length ? `<h3>Relationships</h3><div class="feed">${relations.map((relation) => renderRelation(relation, issueID, resolvedProject)).join("")}</div>` : "";
  const readyChangeHTML = readyChange ? `<h3>Ready Change</h3><div class="feed">${renderIssueChange(readyChange)}</div>` : "";
  const changesHTML = changes.length ? `<h3>Changes</h3><div class="feed">${changes.map(renderIssueChange).join("")}</div>` : "";
  const checksHTML = checks.length ? `<h3>Checks</h3><div class="check-list">${checks.map((check) => renderCheck(check, issueID, resolvedProject)).join("")}</div>` : "";
  const attachmentsHTML = attachments.length ? `<h3>Attachments</h3><div class="attachment-list">${attachments.map((attachment) => renderIssueAttachment(attachment, issueID, resolvedProject)).join("")}</div>` : "";
  const attachmentUploadHTML = renderAttachmentUploadForm(issueID, resolvedProject);
  const attentionHTML = renderHumanAttentionPanel(issue, statusLog, resolvedProject, activeSession);
  // The standalone Sessions and Status feeds are gone: they are folded into
  // the unified Timeline below, which removes the column-height imbalance
  // (the tall sessions list used to dominate the editor column) and the
  // duplicated session lifecycle shown in the old transitions feed.
  const lifecycleGraphHTML = lifecycleGraph ? `<div class="lifecycle-chart">${renderLifecycleChart(lifecycleGraph)}</div>` : "";
  const timelineHTML = renderTimeline({ sessions, transitions: timelineTransitions, statusLog });
  const lifecycleHTML = (lifecycleGraphHTML || timelineTransitions.length || sessions.length || statusLog.length)
    ? `<h3>Lifecycle</h3>${lifecycleGraphHTML}<div class="lifecycle-timeline">${timelineHTML}</div>`
    : "";
  // Read-only detail (title/body/acceptance criteria/agent config) with an
  // Edit toggle that reveals the full form. Directly fixes the issue where a
  // tall sessions list covered up the agent config, title, body and criteria.
  const readOnlyDetailHTML = renderIssueReadOnlyDetailView(app, issue, { issueID, projectID: resolvedProject });
  const editorHTML = [
    tagsHTML,
    relationsHTML,
    readOnlyDetailHTML,
    attachmentsHTML,
    attachmentUploadHTML,
  ].filter(Boolean).join("");
  const activityHTML = [readyChangeHTML, changesHTML].filter(Boolean).join("");
  const systemHTML = checksHTML;
  const lifecycleSectionHTML = lifecycleHTML ? `<div class="issue-detail-lifecycle">${lifecycleHTML}</div>` : "";
  const detailColumns = [
    `<div class="issue-detail-column issue-detail-editor">${editorHTML}</div>`,
    activityHTML ? `<div class="issue-detail-column issue-detail-activity">${activityHTML}</div>` : "",
    systemHTML ? `<div class="issue-detail-column issue-detail-system">${systemHTML}</div>` : "",
    lifecycleSectionHTML,
  ].filter(Boolean).join("");
  app.querySelector(".content").innerHTML = `
    <section class="detail issue-detail">
      <div class="detail-head issue-detail-head">
        <div>
          <h2>${escapeHTML(issueID)} · ${escapeHTML(value(issue, "title", "Title"))}</h2>
          <div class="meta">
            ${renderPhaseBadge(triageState === "triage" ? "triage" : scheduleState)}
            ${reviewState ? renderReviewBadge(reviewState) : ""}
            ${checkTotal ? `<span class="badge ${checkSatisfied === checkTotal ? "ok" : "idle"}">checks ${checkSatisfied}/${checkTotal}</span>` : ""}
          </div>
          <p class="meta-quiet">p${Number(value(issue, "priority", "Priority") || 0)} · ${escapeHTML(value(issue, "agent_harness", "AgentHarness") || "codex")} · human review ${value(issue, "requires_human_review", "RequiresHumanReview") ? "required" : "optional"} · auto merge ${value(issue, "auto_merge", "AutoMerge") ? "on" : "off"}</p>
        </div>
        <div class="actions">
          ${renderIssueStateForm(issueID, currentIssueState(scheduleState, triageState), resolvedProject)}
          ${readyChange ? `<button class="button secondary" data-review-run="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Run review</button>` : ""}
          ${triageState === "triage" ? `<button class="button" data-triage="accepted" data-issue="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Accept</button><button class="button secondary" data-triage="rejected" data-issue="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Reject</button>` : ""}
          ${scheduleState !== "up_next" && scheduleState !== "closed" ? `<button class="button secondary" data-schedule="up_next" data-issue="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Queue</button>` : ""}
          ${scheduleState !== "backlog" && scheduleState !== "closed" ? `<button class="button secondary" data-schedule="backlog" data-issue="${escapeAttr(issueID)}"${projectButtonAttr(resolvedProject)}>Backlog</button>` : ""}
          ${activeSessionID && (activeSessionTerminalAvailable || (terminalAvailable && !terminalJobID)) ? renderTerminalButton("session", activeSessionID) : ""}
          ${terminalJobID && terminalAvailable && !(activeSessionID && (activeSessionTerminalAvailable || (terminalAvailable && !terminalJobID))) ? renderTerminalButton("job", terminalJobID) : ""}
          ${crashRetryHTML}
          ${pauseResumeHTML}
          ${issueConsoleHTML}
          ${reviewCycleApproveHTML}
        </div>
      </div>
      <div class="summary-grid">
        <div><span>Created</span><strong>${escapeHTML(value(issue, "created_by", "CreatedBy"))}</strong></div>
        <div><span>Source Issue</span><strong>${escapeHTML(value(issue, "source_issue_id", "SourceIssueID") || "")}</strong></div>
        <div><span>Source Change</span><strong>${escapeHTML(value(issue, "source_change_id", "SourceChangeID") || "")}</strong></div>
        <div><span>Updated</span><strong>${escapeHTML(formatDate(value(issue, "updated_at", "UpdatedAt")))}</strong></div>
      </div>
      ${attentionHTML}
      <div class="issue-detail-grid">${detailColumns}</div>
    </section>
  `;
  app.bindIssueActions(() => renderIssueView(app, id, undefined, resolvedProject));
  return true;
}

export function toggleIssueEditFormView(app, button) {
  const container = button.closest("[data-issue-read-only]");
  if (!container) return;
  const body = container.querySelector("[data-issue-read-only-body]");
  const formWrap = container.querySelector("[data-issue-edit-form]");
  if (!body || !formWrap) return;
  const editing = formWrap.hidden;
  body.hidden = editing;
  formWrap.hidden = !editing;
  button.textContent = editing ? "Cancel" : "Edit";
  button.dataset.issueEditToggleState = editing ? "editing" : "";
  if (editing) {
    const form = formWrap.querySelector("[data-issue-form]");
    if (form) {
      bindHarnessModelControlsView(app, form);
      bindAgentArgControlsView(app, form);
    }
  }
}

export function bindAgentArgControlsView(app, form) {
  const agentSelect = form?.elements?.agent_harness;
  const argsField = form?.elements?.agent_args;
  if (!agentSelect || !argsField || typeof agentSelect.addEventListener !== "function") return;
  const savedArgs = parseJSONAttribute(argsField.dataset?.agentArgsValues, {});
  const defaults = parseJSONAttribute(argsField.dataset?.agentArgsDefaultValues, {});
  const defaultsElement = typeof form.querySelector === "function" ? form.querySelector("[data-agent-args-defaults]") : null;
  let currentHarness = agentSelect.value;
  const persistSavedArgs = () => {
    if (argsField.dataset) argsField.dataset.agentArgsValues = JSON.stringify(savedArgs);
  };
  const sync = () => {
    savedArgs[currentHarness] = String(argsField.value || "");
    currentHarness = agentSelect.value;
    argsField.value = String(savedArgs[currentHarness] || "");
    persistSavedArgs();
    if (defaultsElement) {
      const defaultArgs = String(defaults[currentHarness] || "");
      defaultsElement.textContent = defaultArgs ? `Coordinator defaults: ${defaultArgs}` : "";
      defaultsElement.hidden = !defaultArgs;
    }
  };
  agentSelect.addEventListener("change", sync);
}

export function issueAgentPayloadFromFormView(app, form) {
  const agentHarness = String(form.elements.agent_harness?.value || "codex").trim() || "codex";
  const rawAgentArgs = String(form.elements.agent_args?.value || "");
  const harnessArgs = { codex: [], claude: [], harness: [] };
  const selectionArgs = harnessSelectionArgsFromFormView(app, form, agentHarness);
  const additionalArgs = rawAgentArgs.trim() ? [rawAgentArgs] : [];
  harnessArgs[agentHarness] = [...selectionArgs, ...additionalArgs];
  return { agent_harness: agentHarness, harness_args: harnessArgs };
}

export function issueAgentDefaultsFromFormView(app, form) {
  const agentHarness = String(form.elements.agent_harness?.value || "codex").trim() || "codex";
  const argsField = form.elements.agent_args;
  const savedArgs = parseJSONAttribute(argsField?.dataset?.agentArgsValues, {});
  savedArgs[agentHarness] = String(argsField?.value || "");
  const selectionArgs = harnessSelectionArgsFromFormView(app, form, agentHarness);
  const harnessArgs = { codex: [], claude: [], harness: [] };
  for (const name of ["codex", "claude", "harness"]) {
    const additional = String(savedArgs[name] || "").trim() ? [String(savedArgs[name] || "")] : [];
    const selection = name === agentHarness ? selectionArgs : [];
    harnessArgs[name] = [...selection, ...additional];
  }
  return normalizeIssueAgentDefaults({ agent_harness: agentHarness, harness_args: harnessArgs });
}

export function bindHarnessModelControlsView(app, form) {
  if (!form || typeof form.querySelector !== "function") return;
  const fieldset = form.querySelector("[data-harness-model-fields]");
  if (!fieldset) return;
  const catalog = parseJSONAttribute(fieldset.dataset?.harnessModelCatalog, {});
  const savedSelections = parseJSONAttribute(fieldset.dataset?.harnessModelSelections, {});
  const controls = fieldset.querySelector("[data-harness-model-controls]");
  const agentSelect = form.elements.agent_harness;
  const modelsFor = (harness) => normalizeHarnessModelList(catalog[harness] || []);

  // bindInner (re)wires the model/reasoning listeners after a render.
  const bindInner = () => {
    const harness = agentSelect?.value || "";
    const models = modelsFor(harness);
    const modelSelect = form.elements.harness_model;
    const syncReasoning = (preserve = true) => {
      const model = findHarnessModel(models, modelSelect?.value || "");
      renderHarnessReasoningInto(fieldset, model, preserve);
    };
    const syncModelOptions = (preserveReasoning = true) => {
      if (modelSelect) {
        const currentModel = findHarnessModel(models, modelSelect.value);
        const selectedID = currentModel ? currentModel.qualified_id : "";
        modelSelect.innerHTML = renderHarnessModelOptions(models, selectedID);
        modelSelect.value = selectedID;
      }
      syncReasoning(preserveReasoning);
    };
    if (modelSelect && typeof modelSelect.addEventListener === "function") {
      modelSelect.addEventListener("change", () => syncModelOptions(false));
    }
    syncModelOptions(true);
  };

  const renderForHarness = (harness) => {
    if (!controls) return;
    const models = modelsFor(harness);
    fieldset.hidden = models.length === 0;
    controls.innerHTML = renderHarnessModelControls(models, savedSelections[harness] || null);
    bindInner();
  };

  if (agentSelect && typeof agentSelect.addEventListener === "function") {
    agentSelect.addEventListener("change", () => renderForHarness(agentSelect.value));
  }
  // The active harness's controls were rendered server-side; just wire them up.
  bindInner();
}

export function harnessSelectionArgsFromFormView(app, form, harness) {
  if (!form) return [];
  const selectedHarness = harness || String(form.elements.agent_harness?.value || "").trim();
  if (!selectedHarness) return [];
  const modelValue = String(form.elements.harness_model?.value || "").trim();
  if (!modelValue) return [];
  const models = harnessModels((app.harnesses && app.harnesses.agents) || DEFAULT_AGENT_HARNESSES, selectedHarness);
  const model = findHarnessModel(models, modelValue);
  if (!model) return [];
  const values = harnessReasoningLevelValues(model);
  const selectedLevel = String(form.elements.harness_reasoning_effort?.value || "").trim();
  const effort = selectedLevel && selectedLevel !== HARNESS_REASONING_UNAVAILABLE && values.includes(selectedLevel)
    ? selectedLevel
    : (selectedLevel === HARNESS_REASONING_UNAVAILABLE ? "" : values[0] || "");
  return serializeHarnessModelSelection(selectedHarness, model, effort
    ? { mode: "effort", effort }
    : { mode: "default" });
}
