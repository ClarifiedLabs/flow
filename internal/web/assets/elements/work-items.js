// Container-first Work overview. The list response is projected into a safe
// forest once; rendering never follows raw parent IDs, so malformed cycles and
// orphaned rows remain visible without risking recursive loops.

import { FlowElement, define } from "./base.js";
import { failureMessage } from "../actions/dispatch.js";
import { apiPatch, workItemHref, workItemParentsAPIPath } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { writeWorkPreferences } from "../storage.js";
import { renderWorkNav } from "../work-nav.js";
import { renderAddChildControl, renderMoveControl } from "../work-item-detail.js";
import { handleRelationsPickerClick, relationsPickerView } from "../create-relations.js";
import {
  actionableWorkItemCompare,
  buildWorkItemIndex,
  effectiveFeaturePath,
  visibleWorkItemTree,
  workItemDescendants,
  workItemID,
  workItemKind,
  workItemRollups,
  workItemState,
} from "../work-item-model.js";

const FILTERS = [
  ["all", "All"],
  ["open", "Open"],
  ["blocked", "Blocked"],
  ["completed", "Completed"],
];

function plural(count, word) {
  return `${count} ${word}${count === 1 ? "" : "s"}`;
}

function overviewEntries(data) {
  const source = value(data || {}, "items", "Items") || [];
  return source.filter((entry) => value(entry || {}, "item", "Item"));
}

function workItemSource(data, outlineItems = null) {
  if (outlineItems != null) return outlineItems;
  const entries = overviewEntries(data);
  return entries.length
    ? entries.map((entry) => value(entry, "item", "Item"))
    : (value(data || {}, "items", "Items") || []);
}

function renderSelectionControl(item, context) {
  const id = workItemID(item);
  const title = value(item, "title", "Title") || id;
  return `<label class="work-select"><input type="checkbox" data-work-select="${escapeAttr(id)}" aria-label="Select ${escapeAttr(title)}"${context.selected.has(id) ? " checked" : ""}><span class="visually-hidden">Select ${escapeHTML(title)}</span></label>`;
}

export function moveDestinationCandidates(items, selectedIDs) {
  const index = buildWorkItemIndex({ items: items || [] });
  const excluded = new Set(selectedIDs || []);
  for (const id of selectedIDs || []) {
    for (const descendant of workItemDescendants(index, id)) excluded.add(workItemID(descendant));
  }
  return index.items.filter((item) => {
    const kind = workItemKind(item);
    const canContain = value(value(item, "capabilities", "Capabilities") || {}, "can_contain", "CanContain");
    return !excluded.has(workItemID(item)) && !workItemState(item).terminal &&
      (canContain === true || ((canContain == null || canContain === "") && (kind === "epic" || kind === "feature")));
  });
}

function authoritativeRollup(entry) {
  const rollup = value(entry || {}, "rollup", "Rollup") || {};
  const tasks = value(rollup, "descendant_tasks", "DescendantTasks") || {};
  const direct = value(rollup, "direct_children", "DirectChildren") || {};
  const successful = Number(value(tasks, "successful_terminal", "SuccessfulTerminal") || 0);
  const unsuccessful = Number(value(tasks, "unsuccessful_terminal", "UnsuccessfulTerminal") || 0);
  return {
    tasks: {
      total: Number(value(tasks, "total", "Total") || 0),
      closed: successful + unsuccessful,
      successful,
      unsuccessful,
      inProgress: Number(value(tasks, "in_progress", "InProgress") || 0),
      scheduled: Number(value(tasks, "scheduled", "Scheduled") || 0),
      unscheduled: Number(value(tasks, "unscheduled", "Unscheduled") || 0),
      otherOpen: Number(value(tasks, "other_open", "OtherOpen") || 0),
      blocked: Number(value(tasks, "effectively_blocked", "EffectivelyBlocked") || 0),
    },
    direct: {
      total: Number(value(direct, "total", "Total") || 0),
      closed: Number(value(direct, "terminal", "Terminal") || 0),
    },
  };
}

function readinessButton(readiness, label, attributes) {
  if (!readiness) return "";
  const allowed = Boolean(value(readiness, "allowed", "Allowed"));
  const denial = String(value(readiness, "denial_text", "DenialText") || "");
  return `<button type="button" class="secondary" ${attributes}${allowed ? "" : ` disabled aria-disabled="true" title="${escapeAttr(denial)}"`}>${escapeHTML(label)}</button>`;
}

function renderAuthoritativeContext(entry, item, projectID) {
  if (!entry) return "";
  const id = workItemID(item);
  const kind = workItemKind(item);
  const attention = Number(value(entry, "attention_count", "AttentionCount") || 0);
  const feature = value(entry, "feature", "Feature");
  const actions = value(entry, "actions", "Actions") || {};
  const featureText = feature
    ? (value(feature, "git_available", "GitAvailable")
      ? `${value(feature, "branch", "Branch") || id} → ${value(feature, "integration_target", "IntegrationTarget") || "base"} · ${Number(value(feature, "ahead", "Ahead") || 0)} ahead · ${Number(value(feature, "behind", "Behind") || 0)} behind`
      : `${value(feature, "branch", "Branch") || id} · Git status unavailable`)
    : "";
  const startAttr = kind === "feature" ? `data-feature-start="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}"` : `data-epic-start="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}"`;
  return `${attention ? `<span class="work-attention">${plural(attention, "task")} need attention</span>` : ""}
    ${featureText ? `<span class="work-feature-state">${escapeHTML(featureText)}</span>` : ""}
    <div class="work-readiness-actions">
      ${readinessButton(value(actions, "start", "Start"), "Start descendants", startAttr)}
      ${kind === "epic" ? readinessButton(value(actions, "complete", "Complete"), "Complete", `data-epic-complete="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}"`) : ""}
      ${kind === "feature" ? readinessButton(value(actions, "rebase", "Rebase"), "Rebase", `data-feature-rebase="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}"`) : ""}
      ${kind === "feature" ? readinessButton(value(actions, "land", "Land"), "Land", `data-feature-land="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}"`) : ""}
    </div>`;
}

function taskProgressLabel(rollup) {
  if (!rollup.total) return "No descendant tasks";
  const parts = [
    `${rollup.closed} of ${rollup.total} descendant tasks complete`,
    `${rollup.inProgress} in progress`,
    `${rollup.scheduled} scheduled`,
    `${rollup.unscheduled + rollup.otherOpen} unscheduled`,
  ];
  if (rollup.blocked) parts.push(plural(rollup.blocked, "blocked task"));
  return parts.join(", ");
}

export function renderTaskProgress(rollup) {
  const label = taskProgressLabel(rollup);
  if (!rollup.total) return `<p class="work-progress-empty">${label}</p>`;
  const segments = [
    ["successful", rollup.successful],
    ["unsuccessful", rollup.unsuccessful],
    ["working", rollup.inProgress],
    ["scheduled", rollup.scheduled],
    ["open", rollup.unscheduled + rollup.otherOpen],
  ];
  const breakdown = `${rollup.closed}/${rollup.total} descendant tasks complete · ${rollup.successful} successful · ${rollup.unsuccessful} unsuccessful · ${rollup.inProgress} in progress · ${rollup.scheduled} scheduled · ${rollup.unscheduled + rollup.otherOpen} unscheduled${rollup.blocked ? ` · ${plural(rollup.blocked, "blocked task")}` : ""}`;
  return `<div class="work-progress-wrap">
    <div class="work-progress" role="img" aria-label="${escapeAttr(label)}">${segments
      .filter(([, count]) => count > 0)
      .map(([state, count]) => `<span data-progress="${state}" style="width:${(count / rollup.total) * 100}%" aria-hidden="true"></span>`)
      .join("")}</div>
    <span class="work-progress-text">${escapeHTML(breakdown)}</span>
  </div>`;
}

function branchContext(index, item) {
  const id = workItemID(item);
  const path = effectiveFeaturePath(index, item);
  if (!path.length) return "";
  const feature = path.at(-1);
  const label = feature.item ? (value(feature.item, "title", "Title") || feature.id) : feature.id;
  const own = workItemKind(item) === "feature" && feature.id === id;
  return `<span class="work-branch" title="${escapeAttr(own ? "This feature owns the branch" : "Effective branch inherited through containment")}">${own ? "Branch" : "On branch"} ${escapeHTML(label)}</span>`;
}

function hierarchyWarning(index, item) {
  const id = workItemID(item);
  if (index.cycles.has(id)) return `<span class="work-warning">cycle detached</span>`;
  if (index.orphans.some((candidate) => workItemID(candidate) === id)) return `<span class="work-warning">parent unavailable</span>`;
  return "";
}

function renderHierarchyNode(index, item, context, depth = 0) {
  const id = workItemID(item);
  const kind = workItemKind(item);
  const state = workItemState(item);
  const title = value(item, "title", "Title") || id;
  const blockers = Number(value(item, "unresolved_blockers", "UnresolvedBlockers") || 0);
  const children = (index.children.get(id) || []).filter((child) => context.visible.has(workItemID(child)));
  const expanded = context.query || !context.collapsed.has(id);
  const childID = `work-children-${id.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
  const navigation = { context: kind === "task" ? context.navigationContext : "", returnTo: context.returnTo };
  const childContext = kind === "task" ? context : { ...context, navigationContext: id };
  return `<li class="work-node" data-kind="${escapeAttr(kind)}" data-depth="${depth}">
    <div class="work-node-row">
      ${renderSelectionControl(item, context)}
      ${children.length ? `<button type="button" class="work-disclosure" data-work-toggle="${escapeAttr(id)}" aria-label="${expanded ? "Collapse" : "Expand"} ${escapeAttr(title)}" aria-expanded="${expanded}" aria-controls="${escapeAttr(childID)}"><span aria-hidden="true">${expanded ? "−" : "+"}</span></button>` : `<span class="work-disclosure-spacer" aria-hidden="true"></span>`}
      <span class="work-kind">${escapeHTML(kind)}</span>
      <a href="${escapeAttr(workItemHref(context.projectID, item, navigation))}" data-link class="work-node-title">${escapeHTML(title)}</a>
      <span class="work-id">${escapeHTML(id)}</span>
      <span class="work-state" data-state="${escapeAttr(state.status)}">${escapeHTML(state.status.replaceAll("_", " "))}</span>
      ${blockers ? `<span class="work-blockers">${plural(blockers, "effective blocker")}</span>` : ""}
      ${branchContext(index, item)}
      ${hierarchyWarning(index, item)}
      <div class="work-context-actions">${renderAddChildControl(context.projectID, item, index.items)}${renderMoveControl(context.projectID, item, index.items)}</div>
    </div>
    ${children.length && expanded ? `<ul id="${escapeAttr(childID)}" class="work-children">${children.map((child) => renderHierarchyNode(index, child, childContext, depth + 1)).join("")}</ul>` : ""}
  </li>`;
}

function hasOpenDescendant(index, item) {
  return workItemDescendants(index, item).some((descendant) => !workItemState(descendant).terminal);
}

function renderContainerCard(index, item, context) {
  const id = workItemID(item);
  const state = workItemState(item);
  const kind = workItemKind(item);
  const title = value(item, "title", "Title") || id;
  const priority = Number(value(item, "priority", "Priority") || 0);
  const blockers = Number(value(item, "unresolved_blockers", "UnresolvedBlockers") || 0);
  const entry = context.overview.get(id);
  const rollup = entry ? authoritativeRollup(entry) : (context.rollups.get(id) || { tasks: {}, direct: { total: 0, closed: 0 }, blockedDescendants: 0 });
  const direct = rollup.direct;
  const contradiction = state.terminal && (entry ? rollup.tasks.closed < rollup.tasks.total || direct.closed < direct.total : hasOpenDescendant(index, item));
  const directLabel = direct.total
    ? `${direct.closed}/${direct.total} direct children closed${entry ? "" : ` · ${direct.ready} ready · ${direct.blocked} blocked`}`
    : "No direct children";
  const expanded = context.outlineLoaded && (context.query || !context.collapsed.has(id));
  const children = context.outlineLoaded ? (index.children.get(id) || []).filter((child) => context.visible.has(workItemID(child))) : [];
  const hasHierarchy = direct.total > 0 || children.length > 0;
  const childID = `work-card-children-${id.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
  const navigation = { returnTo: context.returnTo };
  const childContext = { ...context, navigationContext: id };
  return `<article class="work-card" data-kind="${escapeAttr(kind)}" data-state="${escapeAttr(state.status)}">
    <header class="work-card-head">
      <div class="work-card-title">
        ${renderSelectionControl(item, context)}
        <span class="work-kind">${escapeHTML(kind)}</span>
        <a href="${escapeAttr(workItemHref(context.projectID, item, navigation))}" data-link>${escapeHTML(title)}</a>
        <span class="work-id">${escapeHTML(id)}</span>
      </div>
      <div class="work-card-badges">
        <span class="work-state" data-state="${escapeAttr(state.status)}">${escapeHTML(state.status.replaceAll("_", " "))}</span>
        ${priority ? `<span>p${priority}</span>` : ""}
        ${blockers ? `<span class="work-blockers">${plural(blockers, "effective blocker")}</span>` : ""}
      </div>
    </header>
    ${renderTaskProgress(rollup.tasks)}
    <div class="work-card-context">
      <span>${escapeHTML(directLabel)}</span>
      ${!entry && rollup.blockedDescendants ? `<span>${plural(rollup.blockedDescendants, "blocked descendant")}</span>` : ""}
      ${renderAuthoritativeContext(entry, item, context.projectID)}
      ${branchContext(index, item)}
      ${hierarchyWarning(index, item)}
    </div>
    ${contradiction ? `<p class="work-contradiction" role="status">This container is closed but still has open descendants.</p>` : ""}
    <div class="work-context-actions">${renderAddChildControl(context.projectID, item, index.items)}${renderMoveControl(context.projectID, item, index.items)}</div>
    ${hasHierarchy ? `<div class="work-card-hierarchy">
      <button type="button" class="work-expand" data-work-toggle="${escapeAttr(id)}" aria-expanded="${expanded}" aria-controls="${escapeAttr(childID)}">${expanded ? "Hide" : "Show"} hierarchy · ${direct.total || children.length} direct</button>
      ${expanded ? `<ul id="${escapeAttr(childID)}" class="work-tree">${children.map((child) => renderHierarchyNode(index, child, childContext, 1)).join("")}</ul>` : ""}
    </div>` : ""}
  </article>`;
}

function renderBulkMoveDialog(items, local) {
  if (!local?.moveDialogOpen) return "";
  const selected = local.selected || new Set();
  const destination = String(local.moveDestination || "");
  const candidates = moveDestinationCandidates(items, selected);
  const issues = local.moveIssues || [];
  return `<dialog class="work-move-dialog" aria-label="Move selected work items">
    <form data-work-move-form>
      <h2>Move selected</h2>
      <p>${plural(selected.size, "work item")} selected</p>
      <label><span>Destination</span><select name="parent_item_id" data-work-move-destination>
        <option value=""${destination ? "" : " selected"}>No parent (top level)</option>
        ${candidates.map((item) => {
          const id = workItemID(item);
          return `<option value="${escapeAttr(id)}"${id === destination ? " selected" : ""}>${escapeHTML(value(item, "title", "Title") || id)} · ${escapeHTML(workItemKind(item))}</option>`;
        }).join("")}
      </select></label>
      ${local.moveError ? `<div class="work-move-error" role="alert"><p>${escapeHTML(local.moveError)}</p>${issues.length ? `<ul class="work-move-issues">${issues.map((issue) => {
        const itemID = String(value(issue || {}, "item_id", "ItemID") || "");
        const code = String(value(issue || {}, "code", "Code") || "");
        const message = String(value(issue || {}, "message", "Message") || "");
        return `<li data-work-move-issue data-item-id="${escapeAttr(itemID)}" data-code="${escapeAttr(code)}">${itemID ? `<span class="work-move-issue-item">${escapeHTML(itemID)}</span>` : ""}<code>${escapeHTML(code)}</code><span>${escapeHTML(message)}</span></li>`;
      }).join("")}</ul>` : ""}</div>` : ""}
      <div class="work-move-actions">
        <button type="button" class="secondary" data-work-move-cancel>Cancel</button>
        <button type="submit" data-work-move-continue${local.movePending ? ` disabled aria-disabled="true" aria-busy="true"` : ""}>Continue</button>
      </div>
    </form>
  </dialog>`;
}

function renderEpicForm(projectID, workItems) {
  return `<details class="work-create"><summary>New epic</summary>
    <form class="feature-create" data-epic-form="" data-project="${escapeAttr(projectID)}">
      <input name="title" placeholder="Epic title" required aria-label="Epic title">
      <textarea name="body" rows="2" placeholder="What this epic coordinates (optional)" aria-label="Epic body"></textarea>
      <input name="parent_item_id" placeholder="Parent epic or feature ID (optional)" aria-label="Parent work item ID">
      <input name="priority" type="number" min="0" value="0" aria-label="Epic priority">
      <select name="completion_policy" aria-label="Epic completion policy"><option value="all_children">Complete with all children</option><option value="manual">Manual completion</option></select>
      ${relationsPickerView(workItems)}
      <div><button type="submit">Create epic</button></div>
    </form>
  </details>`;
}

export function renderWorkItems(data, local = null) {
  const projectID = String(data?.projectID || "");
  const preferences = local?.preferences || data?.preferences || {};
  const filter = local?.filter || data?.filter || preferences.filter || "all";
  const view = local?.view || data?.view || preferences.view || "overview";
  const query = local?.query ?? data?.query ?? "";
  const collapsed = local?.collapsed || preferences.collapsed || new Set();
  const completedCollapsed = local?.completedCollapsed ?? preferences.completedCollapsed ?? true;
  const entries = overviewEntries(data);
  const overview = new Map(entries.map((entry) => [workItemID(value(entry, "item", "Item") || {}), entry]));
  const summaries = entries.map((entry) => value(entry, "item", "Item"));
  const outlineItems = local?.outlineItems;
  const sourceItems = workItemSource(data, outlineItems);
  const selected = local?.selected || new Set();
  const index = buildWorkItemIndex({ items: sourceItems });
  const portfolioIndex = entries.length && view !== "tree" ? buildWorkItemIndex({ items: summaries }) : index;
  const visible = visibleWorkItemTree(index, { filter, query });
  const portfolioVisible = visibleWorkItemTree(portfolioIndex, { filter, query });
  const rollups = workItemRollups(index);
  const context = { projectID, visible, rollups, overview, collapsed, selected, outlineLoaded: outlineItems != null || !entries.length, query: String(query).trim(), returnTo: data?.currentHref || "", navigationContext: "" };
  const roots = portfolioIndex.roots.filter((item) => portfolioVisible.has(workItemID(item))).sort((a, b) => {
    const attention = Number(value(overview.get(workItemID(b)) || {}, "attention_count", "AttentionCount") || 0) - Number(value(overview.get(workItemID(a)) || {}, "attention_count", "AttentionCount") || 0);
    return attention || actionableWorkItemCompare(a, b);
  });
  const completed = [];
  const containers = [];
  const standalone = [];
  for (const item of roots) {
    const actuallyCompleted = workItemState(item).terminal && !hasOpenDescendant(index, item);
    if (actuallyCompleted) completed.push(item);
    else if (workItemKind(item) === "task") standalone.push(item);
    else containers.push(item);
  }
  const shown = roots.length;
  const forceCompletedOpen = filter === "completed" || Boolean(String(query).trim());
  const completedOpen = forceCompletedOpen || !completedCollapsed;
  return `<section class="work-overview" data-view="${escapeAttr(view)}">
    ${renderWorkNav({ active: "overview", projects: data?.projects || [], projectID, search: data?.search || "" })}
    <div class="work-toolbar">
      <div class="work-filters" role="group" aria-label="Filter Work items">${FILTERS.map(([key, label]) => `<button type="button" class="chip${key === filter ? " active" : ""}" data-work-filter="${key}" aria-pressed="${key === filter}">${label}</button>`).join("")}</div>
      <label class="work-search"><span class="visually-hidden">Search Work</span><input type="search" data-work-search placeholder="Search title, ID, or body" value="${escapeAttr(query)}"></label>
      <div class="work-view-toggle" role="group" aria-label="Work layout"><button type="button" data-work-view-mode="overview" aria-pressed="${view === "overview"}">Overview</button><button type="button" data-work-view-mode="tree" aria-pressed="${view === "tree"}">Tree</button></div>
      <button type="button" class="secondary" data-work-move-selected${selected.size ? "" : ` disabled aria-disabled="true"`}>Move selected${selected.size ? ` (${selected.size})` : ""}</button>
      <span class="work-count">${shown} top-level · ${entries.length ? `${entries.length} active` : `${index.items.length} total`}</span>
    </div>
    ${local?.outlineLoading ? `<p class="work-outline-status" role="status">Loading hierarchy…</p>` : ""}
    ${local?.outlineError ? `<p class="work-outline-error" role="alert">Hierarchy unavailable: ${escapeHTML(local.outlineError)} <button type="button" class="secondary" data-work-outline-retry>Retry</button></p>` : ""}
    ${containers.length ? `<section class="work-section"><h3>Containers</h3><div class="work-cards">${containers.map((item) => renderContainerCard(index, item, context)).join("")}</div></section>` : ""}
    ${standalone.length ? `<section class="work-section"><h3>Standalone tasks</h3><ul class="work-tree work-standalone">${standalone.map((item) => renderHierarchyNode(index, item, context)).join("")}</ul></section>` : ""}
    ${completed.length ? `<section class="work-section work-completed"><button type="button" class="work-completed-toggle" data-work-completed-toggle aria-expanded="${completedOpen}" aria-controls="work-completed-list">Completed · ${completed.length}</button>${completedOpen ? `<div id="work-completed-list" class="work-cards">${completed.map((item) => workItemKind(item) === "task" ? `<ul class="work-tree">${renderHierarchyNode(index, item, context)}</ul>` : renderContainerCard(index, item, context)).join("")}</div>` : ""}</section>` : ""}
    ${!shown ? `<p class="empty">${sourceItems.length ? "No work items match these filters." : "No work items yet."}</p>` : ""}
    ${renderEpicForm(projectID, sourceItems)}
    ${renderBulkMoveDialog(sourceItems, { ...local, selected })}
  </section>`;
}

export class FlowWorkItems extends FlowElement {
  render(data) {
    const projectID = String(data?.projectID || "");
    const projectChanged = this.stateProject !== projectID;
    if (projectChanged || this.sourcePreferences !== data?.preferences) {
      this.stateProject = projectID;
      this.sourcePreferences = data?.preferences;
      const preferences = data?.preferences || {};
      this.preferences = {
        view: data?.view || preferences.view || "overview",
        filter: data?.filter || preferences.filter || "all",
        completedCollapsed: preferences.completedCollapsed ?? true,
        collapsed: new Set(preferences.collapsed || []),
      };
      this.query = String(data?.query || "");
      this.outlineItems = null;
      this.outlineError = "";
      this.outlineLoading = false;
    }
    if (projectChanged || !this.selectedIDs) {
      this.selectedIDs = new Set();
      this.moveDialogOpen = false;
      this.moveDestination = "";
      this.moveError = "";
      this.moveIssues = [];
      this.movePending = false;
    }
    const available = new Set(workItemSource(data, this.outlineItems).map(workItemID));
    for (const id of this.selectedIDs) {
      if (!available.has(id)) this.selectedIDs.delete(id);
    }
    return renderWorkItems(data || {}, {
      ...this.preferences,
      query: this.query,
      outlineItems: this.outlineItems,
      outlineError: this.outlineError,
      outlineLoading: this.outlineLoading,
      selected: this.selectedIDs,
      moveDialogOpen: this.moveDialogOpen,
      moveDestination: this.moveDestination,
      moveError: this.moveError,
      moveIssues: this.moveIssues,
      movePending: this.movePending,
    });
  }

  bind() {
    this.addEventListener("input", (event) => {
      const destination = event.target?.closest?.("[data-work-move-destination]");
      if (destination) {
        this.moveDestination = String(destination.value || "");
        return;
      }
      const search = event.target?.closest?.("[data-work-search]");
      if (!search) return;
      this.query = String(search.value || "");
      this.restoreSearch = { start: search.selectionStart, end: search.selectionEnd };
      this.updateURL();
      this.invalidate();
    });
    this.addEventListener("submit", (event) => {
      if (!event.target?.closest?.("[data-work-move-form]")) return;
      event.preventDefault();
      this.submitMove();
    });
    this.addEventListener("cancel", (event) => {
      if (!event.target?.closest?.(".work-move-dialog")) return;
      event.preventDefault();
      this.closeMoveDialog();
    });
  }

  handleClick(event) {
    if (handleRelationsPickerClick(this, event)) return;
    const selection = event.target?.closest?.("[data-work-select]");
    if (selection) {
      const id = String(selection.dataset.workSelect || "");
      if (this.selectedIDs.has(id)) this.selectedIDs.delete(id);
      else this.selectedIDs.add(id);
      this.restoreControl = { name: "workSelect", value: id };
      this.invalidate();
      return;
    }
    if (event.target?.closest?.("[data-work-move-selected]")) {
      if (!this.selectedIDs.size) return;
      this.moveDialogOpen = true;
      this.moveDestination = "";
      this.moveError = "";
      this.moveIssues = [];
      this.focusMoveDialog = true;
      this.invalidate();
      return;
    }
    if (event.target?.closest?.("[data-work-move-cancel]")) {
      this.closeMoveDialog();
      return;
    }
    if (event.target?.closest?.("[data-work-move-continue]")) {
      event.preventDefault();
      this.submitMove();
      return;
    }
    const filter = event.target?.closest?.("[data-work-filter]");
    if (filter) {
      this.preferences.filter = filter.dataset.workFilter;
      this.persist();
      this.updateURL();
      this.invalidate();
      return;
    }
    const view = event.target?.closest?.("[data-work-view-mode]");
    if (view) {
      this.restoreControl = { name: "workViewMode", value: view.dataset.workViewMode };
      this.preferences.view = view.dataset.workViewMode;
      this.persist();
      this.updateURL();
      this.invalidate();
      if (this.preferences.view === "tree") this.ensureOutline();
      return;
    }
    const toggle = event.target?.closest?.("[data-work-toggle]");
    if (toggle) {
      const id = toggle.dataset.workToggle;
      this.restoreControl = { name: "workToggle", value: id };
      if (this.hasAuthoritativeOverview() && this.outlineItems == null) {
        this.ensureOutline();
        return;
      }
      if (this.preferences.collapsed.has(id)) this.preferences.collapsed.delete(id);
      else this.preferences.collapsed.add(id);
      this.persist();
      this.invalidate();
      return;
    }
    if (event.target?.closest?.("[data-work-outline-retry]")) {
      this.ensureOutline();
      return;
    }
    if (event.target?.closest?.("[data-work-completed-toggle]")) {
      this.preferences.completedCollapsed = !this.preferences.completedCollapsed;
      this.persist();
      this.invalidate();
    }
  }

  closeMoveDialog() {
    this.moveDialogOpen = false;
    this.moveError = "";
    this.moveIssues = [];
    this.focusMoveDialog = false;
    this.restoreControl = { name: "workMoveSelected", value: "" };
    this.invalidate();
  }

  async submitMove() {
    if (this.movePending || !this.selectedIDs.size) return;
    const form = this.querySelector("[data-work-move-form]");
    this.moveDestination = String(form?.elements?.parent_item_id?.value ?? this.moveDestination ?? "").trim();
    const itemIDs = [...this.selectedIDs];
    this.movePending = true;
    this.moveError = "";
    this.moveIssues = [];
    this.invalidate();
    try {
      await apiPatch(workItemParentsAPIPath(this.stateProject), {
        item_ids: itemIDs,
        parent_item_id: this.moveDestination,
      });
      this.selectedIDs.clear();
      this.moveDialogOpen = false;
      this.movePending = false;
      this.invalidate();
      const app = this.app;
      app?.caches?.invalidate?.("workItems", this.stateProject);
      if (typeof app?.refresh === "function") await app.refresh();
    } catch (error) {
      this.movePending = false;
      this.moveError = failureMessage(error);
      this.moveIssues = Array.isArray(error?.issues) ? error.issues : [];
      this.invalidate();
    }
  }

  persist() {
    writeWorkPreferences(this.stateProject, this.preferences);
  }

  updateURL() {
    if (!globalThis.window?.location || !globalThis.history?.replaceState) return;
    const params = new URLSearchParams(window.location.search);
    if (this.preferences.filter === "all") params.delete("filter");
    else params.set("filter", this.preferences.filter);
    if (this.preferences.view === "overview") params.delete("view");
    else params.set("view", this.preferences.view);
    if (this.query.trim()) params.set("q", this.query.trim());
    else params.delete("q");
    const query = params.toString();
    history.replaceState({}, "", `${window.location.pathname}${query ? `?${query}` : ""}`);
  }

  hasAuthoritativeOverview() {
    return overviewEntries(this.data).length > 0;
  }

  async ensureOutline() {
    if (!this.hasAuthoritativeOverview() || this.outlineItems != null || this.outlineLoading) return;
    const loader = this.data?.loadOutline;
    if (typeof loader !== "function") return;
    this.outlineLoading = true;
    this.outlineError = "";
    this.invalidate();
    try {
      const payload = await loader();
      this.outlineItems = value(payload || {}, "items", "Items") || [];
    } catch (error) {
      this.outlineError = failureMessage(error);
    } finally {
      this.outlineLoading = false;
      this.invalidate();
    }
  }

  afterPaint() {
    if (this.restoreSearch) {
      const input = this.querySelector("[data-work-search]");
      input?.focus?.();
      input?.setSelectionRange?.(this.restoreSearch.start, this.restoreSearch.end);
      this.restoreSearch = null;
    }
    if (this.restoreControl) {
      const { name, value: expected } = this.restoreControl;
      const control = [...this.querySelectorAll(`[data-${name.replace(/[A-Z]/g, (letter) => `-${letter.toLowerCase()}`)}]`)]
        .find((candidate) => candidate.dataset?.[name] === expected);
      control?.focus?.();
      if (!this.outlineLoading) this.restoreControl = null;
    }
    const dialog = this.querySelector(".work-move-dialog");
    if (dialog && !dialog.hasAttribute("open")) {
      if (typeof dialog.showModal === "function") dialog.showModal();
      else dialog.setAttribute("open", "");
    }
    if (dialog && this.focusMoveDialog) {
      dialog.querySelector("[data-work-move-destination]")?.focus?.();
      this.focusMoveDialog = false;
    }
    if (this.preferences?.view === "tree") this.ensureOutline();
  }
}

define("flow-work-items", FlowWorkItems);
