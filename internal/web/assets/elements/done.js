// Done (closed tasks) element: paginated history with outcome/density
// controls. The route fetches the first page and mounts the element; the
// element owns the accumulated pages (Load more appends), the outcome filter
// (which reloads the route) and the density toggle (a local repaint).

import { apiGet, taskHref } from "../api.js";
import { doneClosedAtMs, flattenDonePage, phaseKey, renderPhaseBadge } from "../board.js";
import { cardModel } from "../board-model.js";
import { failureMessage } from "../actions/dispatch.js";
import { define, FlowElement, reconcile } from "./base.js";
import "./task-card.js";
import { formatDate } from "../format.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { readDoneDensity, readDoneOutcome, writeDoneDensity, writeDoneOutcome } from "../storage.js";

// doneQuery builds the /v2/done query from the project selection, the outcome
// filter and any extra params (cursor, single-project scope for load-more).
export function doneQuery(selectedProjectIDs, outcome, extra = {}) {
  const params = new URLSearchParams();
  for (const id of selectedProjectIDs) params.append("project", id);
  if (outcome && outcome !== "all") params.set("outcome", outcome);
  for (const [key, val] of Object.entries(extra)) {
    if (val !== undefined && val !== null && val !== "") params.set(key, val);
  }
  const query = params.toString();
  return query ? "?" + query : "";
}

export function renderDoneControls(outcome, density) {
  const outcomes = [["all", "All"], ["completed", "Completed"], ["merged", "Merged"], ["rejected", "Rejected"], ["abandoned", "Abandoned"], ["cancelled", "Cancelled"], ["failed", "Failed"]];
  const filters = outcomes.map(([key, label]) =>
    `<button class="chip${outcome === key ? " active" : ""}" data-done-outcome="${escapeAttr(key)}"${outcome === key ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`
  ).join("");
  const densities = [["extended", "Extended"], ["compact", "Compact"]];
  const toggle = densities.map(([key, label]) =>
    `<button class="chip${density === key ? " active" : ""}" data-done-density="${escapeAttr(key)}"${density === key ? ' aria-pressed="true"' : ""}>${escapeHTML(label)}</button>`
  ).join("");
  return `
    <div class="done-controls">
      <div class="done-filters" role="group" aria-label="Filter by outcome">${filters}</div>
      <div class="done-density" role="group" aria-label="Card density">${toggle}</div>
    </div>
  `;
}

export function renderDoneRow(entry, projectBadge) {
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

export class FlowDone extends FlowElement {
  // The persisted view prefs are element state, not app state: leaving the
  // route discards the element, and the next visit re-seeds from storage.
  outcome = readDoneOutcome();
  density = readDoneDensity();
  // The accumulated pages and the keyset cursors that page them. A fresh
  // first page from the route replaces both; Load more appends.
  entries = [];
  cursors = {};
  page = null;

  // data: { page, projectBadge } — the route's first /v2/done page plus
  // whether several projects are in play (badges on the rows).
  render() {
    const { page, projectBadge } = this.data || {};
    if (page && page !== this.page) {
      this.page = page;
      this.projectBadge = Boolean(projectBadge);
      this.entries = [];
      this.cursors = {};
      this.appendPage(page);
    }
    const sorted = [...this.entries].sort((a, b) => doneClosedAtMs(b.task) - doneClosedAtMs(a.task));
    const list = !sorted.length
      ? `<div class="empty">No closed tasks</div>`
      : this.density === "compact"
        ? sorted.map((entry) => renderDoneRow(entry, this.projectBadge)).join("")
        : "";
    const more = Object.keys(this.cursors).length
      ? `<button class="button secondary" data-done-more>Load more</button>`
      : "";
    return `
      <div class="done-view">
        ${renderDoneControls(this.outcome, this.density)}
        <div class="done-list" data-density="${escapeAttr(this.density)}" data-count="${sorted.length}">${list}</div>
        <div class="done-more">${more}</div>
      </div>
    `;
  }

  afterPaint() {
    if (this.density !== "extended") return;
    const list = this.querySelector(".done-list");
    if (!list) return;
    // Extended density reuses the board card, so a closed task looks like the
    // task it was rather than like a different kind of thing.
    const sorted = [...this.entries].sort((a, b) => doneClosedAtMs(b.task) - doneClosedAtMs(a.task));
    reconcile(list, sorted.map((entry) => this.doneCardModel(entry)), {
      tag: "flow-task-card",
      key: (model) => `${model.projectID}:${model.id}`,
    });
  }

  // doneCardModel is the board card plus what a history view needs: which
  // change the task produced, and when it closed.
  doneCardModel(entry) {
    const model = cardModel(entry, { showProject: this.projectBadge });
    const changeID = value(value(entry.card, "change", "Change"), "id", "ID");
    const closedAt = formatDate(value(entry.task, "done_at", "DoneAt"));
    model.extra = [changeID, closedAt].filter(Boolean);
    return model;
  }

  // appendPage flattens an aggregate /v2/done page onto the accumulator and
  // records each project's keyset cursor (or clears it when exhausted).
  appendPage(data) {
    const { entries } = flattenDonePage(data, this.projectBadge);
    this.entries.push(...entries);
    for (const entry of value(data, "done", "Done") || []) {
      const projectID = value(entry, "project_id", "ProjectID") || "";
      const nextBefore = value(entry, "next_before", "NextBefore");
      if (nextBefore) {
        this.cursors[projectID] = { before: nextBefore, beforeID: value(entry, "next_before_id", "NextBeforeID") || "" };
      } else {
        delete this.cursors[projectID];
      }
    }
  }

  bind() {
    this.addEventListener("click", (event) => {
      const outcomeButton = event.target.closest?.("[data-done-outcome]");
      if (outcomeButton) {
        if (this.outcome === outcomeButton.dataset.doneOutcome) return;
        this.outcome = outcomeButton.dataset.doneOutcome;
        writeDoneOutcome(this.outcome);
        this.app?.load();
        return;
      }
      const densityButton = event.target.closest?.("[data-done-density]");
      if (densityButton) {
        if (this.density === densityButton.dataset.doneDensity) return;
        this.density = densityButton.dataset.doneDensity;
        writeDoneDensity(this.density);
        this.invalidate();
        return;
      }
      const moreButton = event.target.closest?.("[data-done-more]");
      if (moreButton) {
        event.preventDefault();
        this.loadMore();
      }
    });
  }

  // loadMore fetches the next (older) page for every project that still has a
  // cursor, scoping each request to that project so keyset paging stays exact.
  async loadMore() {
    const cursors = Object.entries(this.cursors);
    if (!cursors.length) return;
    try {
      const pages = await Promise.all(cursors.map(([projectID, cursor]) =>
        apiGet("/v2/done" + doneQuery(this.app?.selectedProjectIDs?.() || [], this.outcome, {
          project: projectID,
          before: cursor.before,
          before_id: cursor.beforeID,
        }))
      ));
      for (const page of pages) this.appendPage(page);
      this.invalidate();
    } catch (error) {
      this.app?.setStatus?.(failureMessage(error));
    }
  }
}

define("flow-done", FlowDone);
