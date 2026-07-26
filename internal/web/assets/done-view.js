// Done (closed tasks) view: paginated history with outcome/density controls.
// Accumulates pages on the app instance (app.doneEntries / app.doneCursors).

import { apiGet, taskHref } from "./api.js";
import { doneClosedAtMs, flattenDonePage, phaseKey, renderPhaseBadge } from "./board.js";
import { cardModel } from "./board-model.js";
import { reconcile } from "./elements/base.js";
import "./elements/task-card.js";
import { formatDate } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { readDoneDensity, readDoneOutcome, writeDoneDensity, writeDoneOutcome } from "./storage.js";

export async function renderDoneView(app, context) {
  if (!app.doneOutcome) app.doneOutcome = readDoneOutcome();
  if (!app.doneDensity) app.doneDensity = readDoneDensity();
  const data = await apiGet("/v2/done" + doneQueryView(app, app.doneOutcome));
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Done");
  app.doneProjectBadge = (app.projects || []).length > 1;
  app.doneEntries = [];
  app.doneCursors = {};
  appendDoneDataView(app, data);
  app.querySelector(".content").innerHTML = `
    <div class="done-view">
      ${renderDoneControlsView(app)}
      <div class="done-list"></div>
      <div class="done-more"></div>
    </div>
  `;
  renderDoneListView(app);
  bindDoneControlsView(app);
  return true;
}

export function doneQueryView(app, outcome, extra = {}) {
  const params = new URLSearchParams();
  for (const id of app.selectedProjectIDs()) params.append("project", id);
  if (outcome && outcome !== "all") params.set("outcome", outcome);
  for (const [key, val] of Object.entries(extra)) {
    if (val !== undefined && val !== null && val !== "") params.set(key, val);
  }
  const query = params.toString();
  return query ? "?" + query : "";
}

export function appendDoneDataView(app, data) {
  const { entries } = flattenDonePage(data, app.doneProjectBadge);
  app.doneEntries.push(...entries);
  for (const entry of value(data, "done", "Done") || []) {
    const projectID = value(entry, "project_id", "ProjectID") || "";
    const nextBefore = value(entry, "next_before", "NextBefore");
    if (nextBefore) {
      app.doneCursors[projectID] = { before: nextBefore, beforeID: value(entry, "next_before_id", "NextBeforeID") || "" };
    } else {
      delete app.doneCursors[projectID];
    }
  }
}

export function renderDoneControlsView(app) {
  const outcomes = [["all", "All"], ["completed", "Completed"], ["merged", "Merged"], ["rejected", "Rejected"], ["abandoned", "Abandoned"], ["cancelled", "Cancelled"], ["failed", "Failed"]];
  const filters = outcomes.map(([key, label]) =>
    `<button class="chip${app.doneOutcome === key ? " active" : ""}" data-done-outcome="${escapeAttr(key)}"${app.doneOutcome === key ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`
  ).join("");
  const densities = [["extended", "Extended"], ["compact", "Compact"]];
  const toggle = densities.map(([key, label]) =>
    `<button class="chip${app.doneDensity === key ? " active" : ""}" data-done-density="${escapeAttr(key)}"${app.doneDensity === key ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`
  ).join("");
  return `
    <div class="done-controls">
      <div class="done-filters" role="group" aria-label="Filter by outcome">${filters}</div>
      <div class="done-density" role="group" aria-label="Card density">${toggle}</div>
    </div>
  `;
}

export function renderDoneListView(app) {
  const list = app.querySelector(".done-list");
  if (!list) return;
  list.dataset.density = app.doneDensity;
  const entries = [...app.doneEntries].sort((a, b) => doneClosedAtMs(b.task) - doneClosedAtMs(a.task));
  if (!entries.length) {
    list.innerHTML = `<div class="empty">No closed tasks</div>`;
  } else if (app.doneDensity === "compact") {
    list.innerHTML = entries.map((entry) => renderDoneRowView(app, entry)).join("");
  } else {
    // Extended density reuses the board card, so a closed task looks like the
    // task it was rather than like a different kind of thing.
    list.innerHTML = "";
    reconcile(list, entries.map((entry) => doneCardModel(app, entry)), {
      tag: "flow-task-card",
      key: (model) => `${model.projectID}:${model.id}`,
    });
  }
  const more = app.querySelector(".done-more");
  if (more) {
    more.innerHTML = Object.keys(app.doneCursors).length
      ? `<button class="button secondary" data-done-more>Load more</button>`
      : "";
  }
}

// doneCardModel is the board card plus what a history view needs: which change
// the task produced, and when it closed.
function doneCardModel(app, entry) {
  const model = cardModel(entry, { showProject: app.doneProjectBadge });
  const changeID = value(value(entry.card, "change", "Change"), "id", "ID");
  const closedAt = formatDate(value(entry.task, "done_at", "DoneAt"));
  model.extra = [changeID, closedAt].filter(Boolean);
  return model;
}

export function renderDoneRowView(app, entry) {
  const { task, card, laneState, project } = entry;
  const taskID = value(task, "id", "ID");
  const title = value(task, "title", "Title");
  const projectID = project && project.id ? project.id : "";
  const phaseSlug = phaseKey(laneState) || "dead";
  const change = value(card, "change", "Change");
  const changeID = value(change, "id", "ID");
  const closedAt = formatDate(value(task, "done_at", "DoneAt"));
  const meta = [
    project && project.badge && project.name ? `<span class="card-project-badge">${escapeHTML(project.name)}</span>` : "",
    changeID ? `<a href="/ui/changes/${escapeAttr(changeID)}" data-link>${escapeHTML(changeID)}</a>` : "",
    closedAt ? escapeHTML(closedAt) : "",
  ].filter(Boolean).join(" · ");
  return `
    <div class="done-row" data-phase="${escapeAttr(phaseSlug)}">
      <a class="done-row-title" href="${escapeAttr(taskHref(projectID, taskID))}" data-link>${escapeHTML(taskID)} · ${escapeHTML(title)}</a>
      <span class="done-row-badges">${renderPhaseBadge(laneState)}</span>
      ${meta ? `<span class="done-row-meta">${meta}</span>` : ""}
    </div>
  `;
}

export function bindDoneControlsView(app) {
  const view = app.querySelector(".done-view");
  if (!view) return;
  view.querySelectorAll("[data-done-outcome]").forEach((button) => {
    button.addEventListener("click", () => {
      if (app.doneOutcome === button.dataset.doneOutcome) return;
      app.doneOutcome = button.dataset.doneOutcome;
      writeDoneOutcome(app.doneOutcome);
      app.load();
    });
  });
  view.querySelectorAll("[data-done-density]").forEach((button) => {
    button.addEventListener("click", () => {
      if (app.doneDensity === button.dataset.doneDensity) return;
      app.doneDensity = button.dataset.doneDensity;
      writeDoneDensity(app.doneDensity);
      view.querySelectorAll("[data-done-density]").forEach((other) => {
        const active = other === button;
        other.classList.toggle("active", active);
        if (active) other.setAttribute("aria-pressed", "true");
        else other.removeAttribute("aria-pressed");
      });
      renderDoneListView(app);
    });
  });
  view.addEventListener("click", (event) => {
    const moreButton = event.target.closest("[data-done-more]");
    if (!moreButton) return;
    event.preventDefault();
    loadMoreDoneView(app);
  });
}

export async function loadMoreDoneView(app) {
  const cursors = Object.entries(app.doneCursors);
  if (!cursors.length) return;
  try {
    const pages = await Promise.all(cursors.map(([projectID, cursor]) =>
      apiGet("/v2/done" + doneQueryView(app, app.doneOutcome, {
        project: projectID,
        before: cursor.before,
        before_id: cursor.beforeID,
      }))
    ));
    for (const page of pages) appendDoneDataView(app, page);
    renderDoneListView(app);
  } catch (error) {
    app.setStatus(error.message || String(error));
  }
}
