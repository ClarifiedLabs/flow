// The dense board. Same data as the lanes; lanes become a sort rather than a
// layout, so the view stays useful once the board stops fitting on a screen.

import { taskHref } from "../api.js";
import { BOARD_FILTERS, matchesFilter, sortForAttention } from "../board-model.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { renderStepRail } from "./step-rail.js";
import { define, FlowElement } from "./base.js";

export function renderBoardTable(models, filter = "all") {
  const counts = Object.fromEntries(
    BOARD_FILTERS.map(([key]) => [key, models.filter((model) => matchesFilter(model, key)).length]),
  );
  const rows = sortForAttention(models.filter((model) => matchesFilter(model, filter)));
  return `
    <div class="chips" role="group" aria-label="Filter tasks">
      ${BOARD_FILTERS.map(
        ([key, label]) => `
        <button class="chip${filter === key ? " active" : ""}" data-board-filter="${escapeAttr(key)}"${
          filter === key ? ' aria-pressed="true"' : ""
        }>${escapeHTML(label)}<span class="chip-count">${counts[key]}</span></button>`,
      ).join("")}
      <span class="spacer"></span>
      <span class="sort-note">sort: attention, then dwell</span>
    </div>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Task</th>
            <th class="col-step">Step</th>
            <th>Happening now</th>
            <th class="col-dwell">Dwell</th>
            <th class="col-action">Action</th>
          </tr>
        </thead>
        <tbody>
          ${rows.length ? rows.map(renderRow).join("") : `<tr><td colspan="5" class="empty">No tasks</td></tr>`}
        </tbody>
      </table>
    </div>
  `;
}

function renderRow(model) {
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
      <td class="col-dwell" data-tone="${escapeAttr(model.dwellTone)}">${escapeHTML(model.dwell)}</td>
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
  if (model.lifecycleState === "unscheduled") {
    return `<button class="quiet-action" data-workflow-schedule="${escapeAttr(model.id)}"${projectAttr}>schedule</button>`;
  }
  if (model.lifecycleState === "scheduled") {
    return `<button class="quiet-action" data-workflow-reset="${escapeAttr(model.id)}"${projectAttr}>reset</button>`;
  }
  return `<a class="quiet-action" href="${href}" data-link>watch</a>`;
}

export class FlowBoardTable extends FlowElement {
  filter = "all";

  render(models) {
    if (!models) return "";
    return renderBoardTable(models, this.filter);
  }

  handleClick(event) {
    const chip = event.target.closest?.("[data-board-filter]");
    if (!chip) return;
    const next = chip.dataset.boardFilter;
    this.filter = this.filter === next ? "all" : next;
    this.invalidate();
  }
}

define("flow-board-table", FlowBoardTable);
