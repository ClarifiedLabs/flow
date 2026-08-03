// Shared planning context for epic, feature and task detail surfaces.
// Containment is rendered here; dependency links stay in task-relations.js so
// the two concepts are never presented as one undifferentiated relation list.

import { apiGet, workItemAPIPath, workItemContextAPIPath, workItemHref, workItemsAPIBase } from "./api.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import {
  buildWorkItemIndex,
  descendantTaskRollup,
  effectiveFeaturePath,
  workItemAncestors,
  workItemDescendants,
  workItemID,
  workItemKind,
  workItemState,
} from "./work-item-model.js";
import { renderTaskRelations } from "./elements/task-relations.js";

// Keep the bounded context read model for safe ancestry/navigation, while the
// project list supplies complete Move destinations (including unrelated open
// containers). The existing app cache prevents duplicate list reads.
export async function loadWorkItemContext(app, projectID, itemID, { legacyTree = false } = {}) {
  const projectItems = (typeof app?.ensureWorkItems === "function"
    ? app.ensureWorkItems(projectID)
    : apiGet(workItemsAPIBase(projectID)).then((data) => value(data, "items", "Items") || []))
    .catch(() => []);
  try {
    const hierarchy = await apiGet(workItemContextAPIPath(projectID, itemID));
    const contextItems = [
      ...(value(hierarchy, "ancestors", "Ancestors") || []),
      ...(value(hierarchy, "children", "Children") || []),
    ];
    const workItems = await projectItems;
    return { hierarchy, contextItems, workItems: workItems.length ? workItems : contextItems, bounded: true };
  } catch {
    const [hierarchy, workItems] = await Promise.all([
      apiGet(workItemAPIPath(projectID, itemID, legacyTree ? "/tree" : "")).catch(() => null),
      projectItems,
    ]);
    const contextItems = hierarchy ? [
      ...(value(hierarchy, "ancestors", "Ancestors") || []),
      ...(value(hierarchy, "children", "Children") || []),
    ] : [];
    return { hierarchy, contextItems, workItems: workItems.length ? workItems : contextItems, bounded: false };
  }
}

function capabilities(item) {
  return value(item || {}, "capabilities", "Capabilities") || {};
}

export function validParentCandidates(items, itemID = "") {
  const index = buildWorkItemIndex({ items: items || [] });
  const excluded = new Set([String(itemID || ""), ...workItemDescendants(index, itemID).map(workItemID)]);
  return index.items.filter((candidate) => {
    const id = workItemID(candidate);
    const canContain = value(capabilities(candidate), "can_contain", "CanContain");
    return id && !excluded.has(id) && !workItemState(candidate).terminal &&
      (canContain === true || (canContain == null && workItemKind(candidate) !== "task"));
  });
}

export function parentPickerOptions(items, itemID = "", selectedID = "") {
  const selected = String(selectedID || "");
  return [`<option value="">No parent (top level)</option>`, ...validParentCandidates(items, itemID).map((item) => {
    const id = workItemID(item);
    return `<option value="${escapeAttr(id)}"${id === selected ? " selected" : ""}>${escapeHTML(value(item, "title", "Title") || id)} · ${escapeHTML(workItemKind(item))}</option>`;
  })].join("");
}

export function renderBreadcrumb(projectID, item, items, navigation = {}) {
  const index = buildWorkItemIndex({ items: [...(items || []), item] });
  const chain = [...workItemAncestors(index, workItemID(item)).reverse(), item];
  if (chain.length < 2) return "";
  return `<nav class="work-breadcrumb" aria-label="Direct parent chain"><span class="caption">Direct parent</span>${chain.map((entry, index) => {
    const label = value(entry, "title", "Title") || workItemID(entry);
    return index === chain.length - 1
      ? `<span aria-current="page">${escapeHTML(label)}</span>`
      : `<a href="${escapeAttr(workItemHref(projectID, entry, navigation))}" data-link>${escapeHTML(label)}</a><span aria-hidden="true">/</span>`;
  }).join("")}</nav>`;
}

export function renderEffectiveFeatureContext(projectID, item, items, navigation = {}) {
  const index = buildWorkItemIndex({ items: [...(items || []), item] });
  const path = effectiveFeaturePath(index, item);
  if (!path.length) return "";
  return `<nav class="work-effective-feature" aria-label="Effective feature context"><span class="caption">Effective feature</span>${path.map((entry, position) => {
    const label = entry.item ? (value(entry.item, "title", "Title") || entry.id) : entry.id;
    const separator = position < path.length - 1 ? `<span aria-hidden="true">/</span>` : "";
    return entry.item
      ? `<a href="${escapeAttr(workItemHref(projectID, entry.item, navigation))}" data-link>${escapeHTML(label)}</a>${separator}`
      : `<span>${escapeHTML(label)}</span>${separator}`;
  }).join("")}</nav>`;
}

export function renderWorkBackLink(navigation = {}) {
  const href = String(navigation.returnTo || "").trim();
  return href ? `<a class="work-back" href="${escapeAttr(href)}" data-link>Back to Work context</a>` : "";
}

export function renderMoveControl(projectID, item, items) {
  const id = workItemID(item);
  const parentID = String(value(item, "parent_item_id", "ParentItemID") || "");
  return `<details class="work-context-control"><summary>Move</summary>
    <form class="work-move-form" data-move-work-item-form="${escapeAttr(id)}" data-project="${escapeAttr(projectID)}">
      <label><span>New parent</span><select name="parent_item_id">${parentPickerOptions(items, id, parentID)}</select></label>
      <button type="submit" class="secondary">Move item</button>
    </form>
  </details>`;
}

export function renderAddChildControl(projectID, item, items) {
  const id = workItemID(item);
  const canContain = value(capabilities(item), "can_contain", "CanContain");
  if (workItemState(item).terminal || (canContain !== true && workItemKind(item) === "task")) return "";
  const options = parentPickerOptions(items, "", id);
  const taskURL = `/ui/tasks/new?project=${encodeURIComponent(projectID)}&parent=${encodeURIComponent(id)}`;
  return `<details class="work-context-control"><summary>Add child</summary>
    <div class="work-add-child">
      <a class="button secondary" href="${escapeAttr(taskURL)}" data-link>New task</a>
      <form data-feature-form="" data-project="${escapeAttr(projectID)}">
        <input name="title" placeholder="Feature title" required aria-label="Feature title"><input name="body" type="hidden" value="">
        <select name="parent_item_id" aria-label="Feature parent">${options}</select><button type="submit" class="secondary">Add feature</button>
      </form>
      <form data-epic-form="" data-project="${escapeAttr(projectID)}">
        <input name="title" placeholder="Epic title" required aria-label="Epic title"><input name="body" type="hidden" value=""><input name="priority" type="hidden" value="0"><input name="completion_policy" type="hidden" value="all_children">
        <select name="parent_item_id" aria-label="Epic parent">${options}</select><button type="submit" class="secondary">Add epic</button>
      </form>
    </div>
  </details>`;
}

function hierarchyRows(index, projectID, parentID, navigation) {
  return (index.children.get(parentID) || []).map((child) => `<li>
    <a href="${escapeAttr(workItemHref(projectID, child, navigation))}" data-link>${escapeHTML(value(child, "title", "Title") || workItemID(child))}</a>
    <span>${escapeHTML(workItemKind(child))} · ${escapeHTML(workItemState(child).status.replaceAll("_", " "))}</span>
    ${(index.children.get(workItemID(child)) || []).length ? `<ul>${hierarchyRows(index, projectID, workItemID(child), navigation)}</ul>` : ""}
  </li>`).join("");
}

function progressMarkup(index, id, serverRollup = null) {
  const tasks = value(serverRollup || {}, "descendant_tasks", "DescendantTasks");
  const rollup = tasks ? {
    total: Number(value(tasks, "total", "Total") || 0),
    successful: Number(value(tasks, "successful_terminal", "SuccessfulTerminal") || 0),
    unsuccessful: Number(value(tasks, "unsuccessful_terminal", "UnsuccessfulTerminal") || 0),
    inProgress: Number(value(tasks, "in_progress", "InProgress") || 0),
    scheduled: Number(value(tasks, "scheduled", "Scheduled") || 0),
    unscheduled: Number(value(tasks, "unscheduled", "Unscheduled") || 0),
    otherOpen: Number(value(tasks, "other_open", "OtherOpen") || 0),
  } : descendantTaskRollup(index, id);
  rollup.closed = rollup.closed ?? rollup.successful + rollup.unsuccessful;
  const open = Math.max(0, rollup.total - rollup.closed - rollup.inProgress - rollup.scheduled);
  if (!rollup.total) return `<p class="work-progress-empty">No descendant tasks</p>`;
  const breakdown = `${rollup.closed}/${rollup.total} descendant tasks complete · ${rollup.successful} successful · ${rollup.unsuccessful} unsuccessful · ${rollup.inProgress} in progress · ${rollup.scheduled} scheduled · ${open} unscheduled`;
  return `<div class="work-progress-wrap"><div class="work-progress" role="img" aria-label="${escapeAttr(breakdown)}"><span data-progress="successful" style="width:${rollup.successful / rollup.total * 100}%"></span><span data-progress="unsuccessful" style="width:${rollup.unsuccessful / rollup.total * 100}%"></span><span data-progress="working" style="width:${rollup.inProgress / rollup.total * 100}%"></span><span data-progress="scheduled" style="width:${rollup.scheduled / rollup.total * 100}%"></span>${open ? `<span data-progress="open" style="width:${open / rollup.total * 100}%"></span>` : ""}</div><span class="work-progress-text">${escapeHTML(breakdown)}</span></div>`;
}

export function renderWorkItemContext({ projectID = "", item = {}, items = [], ancestors = null, relations = [], blockers = [], rollup = null, attentionCount = 0, navigation = {}, currentHref = "" } = {}) {
  const all = [...items, ...(ancestors || []), item];
  const index = buildWorkItemIndex({ items: all });
  const id = workItemID(item);
  const children = index.children.get(id) || [];
  const unresolved = blockers.filter((blocker) => !value(blocker, "resolved", "Resolved"));
  const childNavigation = { context: id, returnTo: currentHref || workItemHref(projectID, item, navigation) };
  return `<section class="work-detail-context">
    ${renderWorkBackLink(navigation)}
    ${renderBreadcrumb(projectID, item, all, navigation)}
    ${attentionCount ? `<p class="work-attention">${escapeHTML(`${attentionCount} task${attentionCount === 1 ? "" : "s"} need attention`)}</p>` : ""}
    ${workItemKind(item) !== "task" ? progressMarkup(index, id, rollup) : ""}
    <div class="work-context-actions">${renderAddChildControl(projectID, item, all)}${renderMoveControl(projectID, item, all)}</div>
    <div class="members work-containment"><h3>Contains</h3>${children.length ? `<ul class="work-detail-tree">${hierarchyRows(index, projectID, id, childNavigation)}</ul>` : `<p class="empty">No children yet.</p>`}</div>
    ${unresolved.length ? `<div class="members"><h3>Effective blockers</h3>${unresolved.map((blocker) => {
      const blockerItem = value(blocker, "item", "Item") || {};
      return `<a class="member" href="${escapeAttr(workItemHref(projectID, blockerItem, navigation))}" data-link><span class="member-id">${escapeHTML(workItemID(blockerItem))}</span><span class="member-title">${escapeHTML(value(blockerItem, "title", "Title"))}</span></a>`;
    }).join("")}</div>` : ""}
    ${renderTaskRelations({ id, projectID, relationGroups: null, relations, genericRelations: true, navigation })}
  </section>`;
}
