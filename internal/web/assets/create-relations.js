// Shared create-time work-item relation picker and payload helpers. This module
// deliberately depends only on rendering primitives so task and repainting
// work-item elements can reuse it without importing each other.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export const RELATION_KIND_OPTIONS = [
  { kind: "blocks", label: "blocks" },
  { kind: "related_to", label: "related to" },
  { kind: "parent_of", label: "child of" },
];

export const DEFAULT_RELATION_KIND = "related_to";

export function relationKindOptionsView(selectedKind = "") {
  return RELATION_KIND_OPTIONS.map((option) =>
    `<option value="${escapeAttr(option.kind)}" ${option.kind === selectedKind ? "selected" : ""}>${escapeHTML(option.label)}</option>`,
  ).join("");
}

export function relationPickerRowView(rowIndex = 1) {
  const index = Math.max(1, Number(rowIndex) || 1);
  return `
    <div class="relation-row" data-relation-row data-relation-row-index="${index}">
      <select name="relation_kind" data-relation-kind aria-label="Relation ${index} kind">
        ${relationKindOptionsView(DEFAULT_RELATION_KIND)}
      </select>
      <input name="relation_target" data-relation-target aria-label="Relation ${index} target work item" placeholder="Work item ID" list="relation-target-work-items">
      <button class="button secondary relation-remove" type="button" data-relation-remove aria-label="Remove relation ${index}">&times;</button>
    </div>
  `;
}

function summary(entry) {
  return value(entry || {}, "item", "Item") || entry || {};
}

// Work-item summaries include tasks, epics, and features. The input remains
// free text, so an empty/unavailable cache is a manual-ID fallback.
export function workItemRelationSuggestionsView(items) {
  return (items || []).map(summary).map((item) => {
    const id = String(value(item, "id", "ID") || "").trim();
    if (!id) return "";
    const kind = String(value(item, "kind", "Kind") || "work item");
    const title = String(value(item, "title", "Title") || id);
    return `<option value="${escapeAttr(id)}" label="${escapeAttr(`${kind} · ${title}`)}"></option>`;
  }).join("");
}

export function relationTargetSuggestionsView(app, projectID) {
  const id = String(projectID || "").trim();
  const items = (app?.workItemsByProject && app.workItemsByProject.get(id)) || [];
  return workItemRelationSuggestionsView(items);
}

export function relationsPickerView(items) {
  return `
      <div class="wide relation-picker" data-relation-picker>
        <span class="relation-picker-label">Relations</span>
        <div class="relation-rows" data-relation-rows>${relationPickerRowView()}</div>
        <div class="relation-picker-actions">
          <button class="button secondary" type="button" data-relation-add>Add relation</button>
        </div>
        <datalist id="relation-target-work-items">${workItemRelationSuggestionsView(items)}</datalist>
      </div>`;
}

// Repainting custom elements call this from their one delegated click listener.
// No listeners are attached to generated rows, so a repaint cannot leak them.
export function handleRelationsPickerClick(root, event) {
  const remove = event.target?.closest?.("[data-relation-remove]");
  if (remove && root.contains?.(remove)) {
    remove.closest?.("[data-relation-row]")?.remove();
    return true;
  }
  const add = event.target?.closest?.("[data-relation-add]");
  if (!add || !root.contains?.(add)) return false;
  const picker = add.closest?.("[data-relation-picker]");
  const rows = picker?.querySelector?.("[data-relation-rows]");
  if (!rows) return true;
  const indexes = [...rows.querySelectorAll("[data-relation-row]")]
    .map((row) => Number(row.dataset?.relationRowIndex || 0));
  const container = document.createElement("div");
  container.innerHTML = relationPickerRowView(Math.max(0, ...indexes) + 1);
  const row = container.firstElementChild;
  if (row) {
    rows.appendChild(row);
    row.querySelector?.("[data-relation-target]")?.focus?.();
  }
  return true;
}

// The static task-create view uses the same delegated behavior. A marker keeps
// accidental rebinding idempotent.
export function bindRelationsPickerView(form) {
  if (!form || typeof form.addEventListener !== "function" || form.dataset?.relationsPickerBound === "true") return;
  if (form.dataset) form.dataset.relationsPickerBound = "true";
  form.addEventListener("click", (event) => handleRelationsPickerClick(form, event));
}

export function refreshRelationsPickerSuggestions(form, app, projectID) {
  const datalist = form?.querySelector?.("#relation-target-work-items");
  if (datalist) datalist.innerHTML = relationTargetSuggestionsView(app, projectID);
}

// Every generic create relation marks exactly one endpoint as the new item.
// Outward dependency/association rows use the new item as source. A child-of
// row stores existing parent -> new child. Containment also has the canonical
// parent_item_id field, so declaring both forms of parent is rejected locally.
export function collectCreateWorkItemRelations(form, parentItemID = "") {
  const rows = typeof form?.querySelectorAll === "function"
    ? form.querySelectorAll("[data-relation-row]")
    : [];
  const relations = [];
  const seen = new Set();
  let parentRows = 0;
  for (const row of rows) {
    const existingID = String(row.querySelector?.("[data-relation-target]")?.value || "").trim();
    if (!existingID) continue;
    const kind = String(row.querySelector?.("[data-relation-kind]")?.value || "").trim();
    const key = `${kind}\u0000${existingID}`;
    if (seen.has(key)) return new Error(`Duplicate relation: ${kind || "relation"} ${existingID}`);
    seen.add(key);
    if (kind === "parent_of") {
      parentRows += 1;
      if (parentRows > 1) return new Error("A work item can have only one parent; remove the extra child-of rows");
      relations.push({ source_item_id: existingID, target_is_new_item: true, kind });
    } else {
      relations.push({ target_item_id: existingID, source_is_new_item: true, kind });
    }
  }
  if (parentRows && String(parentItemID || "").trim()) {
    return new Error("Choose a parent either in Parent or Relations, not both");
  }
  return relations;
}
