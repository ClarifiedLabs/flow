// Unified lifecycle control. Single authoritative surface for moving a task to any
// legal lifecycle coordinate from the task detail rail.

import { lifecycleOptionsForModel } from "../task-model.js";
import { escapeAttr, escapeHTML } from "../html.js";

export function renderLifecycleControl(model) {
  if (!model) return "";
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const id = escapeAttr(model.id);
  const options = lifecycleOptionsForModel(model);
  const currentState = String(model.lifecycleState || "unscheduled");
  const waitLabel = model.waitKind ? ` · ${escapeHTML(model.waitKind)}` : "";
  const heldLabel = model.held ? " · held" : "";
  const badge = `<span class="badge" data-phase="${escapeAttr(model.lifecycleState)}">${escapeHTML(currentState)}${waitLabel}${heldLabel}</span>`;
  let grouped = "";
  const groups = { Active: [], Review: [], Terminal: [] };
  for (const option of options) {
    const bucket = option.group || "Active";
    if (!groups[bucket]) groups[bucket] = [];
    groups[bucket].push(option);
  }
  for (const groupName of ["Active", "Review", "Terminal"]) {
    const members = groups[groupName] || [];
    if (!members.length) continue;
    const inner = members.map((option) => {
      const disabled = option.disabled ? " disabled" : "";
      const title = option.reason ? ` title="${escapeAttr(option.reason)}"` : "";
      const current = currentState === option.value || (option.value.startsWith("done:") && currentState === "done") ? " selected" : "";
      return `<option value="${escapeAttr(option.value)}"${disabled}${title}${current}>${escapeHTML(option.label)}${option.reason ? ` (${escapeHTML(option.reason)})` : ""}</option>`;
    }).join("");
    grouped += `<optgroup label="${escapeAttr(groupName)}">${inner}</optgroup>`;
  }
  const captionText = model.held
    ? "Held: resume, or reset to unscheduled. Done: pick a resolution."
    : "Pick any lifecycle target: backlog, scheduled, working, retry/skip, done, or reopen.";
  return `
    <div class="controls" data-lifecycle-control>
      <span class="caption">Lifecycle</span>
      <div class="lifecycle-current">${badge}</div>
      <div class="control-row">
        <label class="select">
          <span class="caption">Target</span>
          <select data-lifecycle-target>${grouped}</select>
        </label>
        <label class="check"><input type="checkbox" data-lifecycle-force /> Force</label>
        <button class="button" data-lifecycle-transition="${id}"${projectAttr}>Transition</button>
      </div>
      <label><span class="caption">Note (optional)</span><textarea rows="2" data-lifecycle-note placeholder="Optional note for done transitions"></textarea></label>
      <p class="caption">${escapeHTML(captionText)}</p>
    </div>
  `;
}

export function selectedLifecycleTarget(container) {
  const select = container?.querySelector?.("[data-lifecycle-target]");
  return select ? String(select.value || "").trim() : "";
}

export function lifecycleForce(container) {
  const box = container?.querySelector?.("[data-lifecycle-force]");
  return Boolean(box?.checked);
}

export function lifecycleNote(container) {
  const area = container?.querySelector?.("[data-lifecycle-note]");
  return area ? String(area.value || "").trim() : "";
}
