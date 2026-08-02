// The dense board. Same data as the lanes; lanes become a sort rather than a
// layout, so the view stays useful once the board stops fitting on a screen.

import { taskHref } from "../api.js";
import { BOARD_FILTERS, matchesFilter } from "../board-model.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { LIFECYCLE_SCHEDULED, LIFECYCLE_UNSCHEDULED } from "../lifecycle.js";
import { renderStepRail } from "./step-rail.js";
import { define, FlowElement } from "./base.js";

export function renderBoardTable(models, filter = "all", sort, sortExplicit = false) {
  const counts = Object.fromEntries(
    BOARD_FILTERS.map(([key]) => [key, models.filter((model) => matchesFilter(model, key)).length]),
  );
  const rows = models.filter((model) => matchesFilter(model, filter));
  // The board hands the table its sorted models; the headers say which key
  // that is and let the reader reverse it, sharing the header control's
  // state. The active column carries aria-sort and an arrow, and the dwell
  // column is relabelled "Last active" while the activity key is active —
  // and the cell then renders the same most-recent-of-all-signals timestamp
  // the sort compares, so a row labelled recently active never shows an
  // unrelated older dwell clock.
  const numberActive = sort?.key === "number";
  const activityActive = sort?.key === "activity";
  const dir = sort?.dir === "desc" ? "descending" : "ascending";
  const arrow = (active) => (active ? (sort?.dir === "desc" ? " \u2193" : " \u2191") : "");
  // The note names the effective order. Once the operator has picked a sort
  // (sortExplicit) the board applies that comparator directly, so the note
  // says the key and direction. Until then the board falls back to the
  // classic attention grouping (sortForAttention), so the note keeps the
  // honest "attention, then dwell" text — and no surface claims that fixed
  // order while an explicit sort is active.
  const note = sortExplicit
    ? `sort: ${sort?.key === "activity" ? "last active" : "task #"} ${sort?.dir === "desc" ? "desc" : "asc"}`
    : "sort: attention, then dwell";
  return `
    <div class="chips" role="group" aria-label="Filter tasks">
      ${BOARD_FILTERS.map(
        ([key, label]) => `
        <button class="chip${filter === key ? " active" : ""}" data-board-filter="${escapeAttr(key)}"${
          filter === key ? ' aria-pressed="true"' : ""
        }>${escapeHTML(label)}<span class="chip-count">${counts[key]}</span></button>`,
      ).join("")}
      <span class="spacer"></span>
      <span class="sort-note">${escapeHTML(note)}</span>
    </div>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th${numberActive ? ` aria-sort="${dir}"` : ""}>
              <button type="button" class="th-sort" data-board-sort-key="number" aria-label="Sort by task number">Task${arrow(numberActive)}</button>
            </th>
            <th class="col-step">Step</th>
            <th>Happening now</th>
            <th class="col-dwell"${activityActive ? ` aria-sort="${dir}"` : ""}>
              <button type="button" class="th-sort" data-board-sort-key="activity" aria-label="Sort by last active">${activityActive ? "Last active" : "Dwell"}${arrow(activityActive)}</button>
            </th>
            <th class="col-action">Action</th>
          </tr>
        </thead>
        <tbody>
          ${rows.length ? rows.map((model) => renderRow(model, activityActive)).join("") : `<tr><td colspan="5" class="empty">No tasks</td></tr>`}
        </tbody>
      </table>
    </div>
  `;
}

function renderRow(model, activityActive) {
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const href = escapeAttr(taskHref(model.projectID, model.id));
  return `
    <tr data-phase="${escapeAttr(model.phase)}"${model.needsYou ? " data-needs-you" : ""}>
      <td class="col-task">
        <span class="id">${escapeHTML(model.id)}</span>
        <a class="title" href="${href}" data-link>${escapeHTML(model.title)}</a>
      </td>
      <td class="col-step">${model.running ? renderStepRail(model) : `<span class="rail-label is-idle">${escapeHTML(model.lifecycleState.replace("_", " "))}</span>`}</td>
      <td class="col-now">${renderMarkdown(model.activity)}</td>
      <td class="col-dwell" data-tone="${escapeAttr(activityActive ? model.lastActiveTone : model.dwellTone)}">${escapeHTML(activityActive ? model.lastActive : model.dwell)}</td>
      <td class="col-action">${renderRowAction(model, href, projectAttr)}</td>
    </tr>
  `;
}

// Rows that need a human get a real button; the rest get a quiet text action,
// so the eye is drawn only to the work that is actually blocked on someone.
function renderRowAction(model, href, projectAttr) {
  if (model.held) {
    return `<button class="quiet-action" data-workflow-release="${escapeAttr(model.id)}" data-edge="resume"${projectAttr}>resume</button>`;
  }
  if (model.needsYou && model.actionLabel) {
    return model.actionLabel === "Merge"
      ? `<button class="button" data-attention-merge="${escapeAttr(model.id)}"${projectAttr}>Merge</button>`
      : `<a class="button" href="${href}" data-link>${escapeHTML(model.actionLabel)}</a>`;
  }
  if (model.lifecycleState === LIFECYCLE_UNSCHEDULED) {
    return `<button class="quiet-action" data-workflow-schedule="${escapeAttr(model.id)}"${projectAttr}>schedule</button>`;
  }
  if (model.lifecycleState === LIFECYCLE_SCHEDULED) {
    return `<button class="quiet-action" data-workflow-reset="${escapeAttr(model.id)}"${projectAttr}>reset</button>`;
  }
  return `<a class="quiet-action" href="${href}" data-link>watch</a>`;
}

export class FlowBoardTable extends FlowElement {
  filter = "all";
  // Mirrors readBoardSort's validated default; the board overwrites this with
  // the shared sort state before the table is visible.
  sort = { key: "number", dir: "asc" };
  // Mirrors readBoardSortChoice: whether the operator has picked a sort. The
  // board overwrites this too; until then the table renders the honest
  // attention-fallback note.
  sortExplicit = false;

  render(models) {
    if (!models) return "";
    return renderBoardTable(models, this.filter, this.sort, this.sortExplicit);
  }

  handleClick(event) {
    const header = event.target.closest?.("[data-board-sort-key]");
    if (header) {
      // A header click sets its key, or reverses the direction when it is
      // already the active key; the board owns the shared sort state and
      // persists every change.
      event.preventDefault();
      const key = header.dataset.boardSortKey;
      const dir = this.sort?.key === key && this.sort?.dir === "asc" ? "desc" : "asc";
      this.dispatchEvent(new CustomEvent("board-sort-change", { detail: { key, dir }, bubbles: true }));
      return;
    }
    const chip = event.target.closest?.("[data-board-filter]");
    if (!chip) return;
    const next = chip.dataset.boardFilter;
    this.filter = this.filter === next ? "all" : next;
    this.invalidate();
  }
}

define("flow-board-table", FlowBoardTable);
