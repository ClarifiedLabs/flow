// The board header's shared sort control. One control owns the sort for both
// board views: it shows the active key with its direction, clicking the key
// cycles Task number -> Last active, and clicking the direction arrow toggles
// asc/desc for the active key. The element only reports intent — the owning
// <flow-board> holds the state and persists it, so the control itself stays a
// dumb view.

import { BOARD_SORT_DIRS, BOARD_SORT_KEYS } from "../config.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement } from "./base.js";

export const BOARD_SORT_KEY_LABELS = {
  number: "Task number",
  activity: "Last active",
};

export const NEXT_BOARD_SORT_KEY = {
  number: "activity",
  activity: "number",
};

const DIR_ARROWS = { asc: "\u2191", desc: "\u2193" };
const DIR_LABELS = { asc: "ascending", desc: "descending" };

export function renderBoardSort(sort) {
  const key = BOARD_SORT_KEYS.has(sort?.key) ? sort.key : "number";
  const dir = BOARD_SORT_DIRS.has(sort?.dir) ? sort.dir : "asc";
  return `
    <button type="button" class="sort-key active" data-board-sort-key aria-pressed="true"
      aria-label="Sort by ${escapeAttr(BOARD_SORT_KEY_LABELS[key])}. Click to sort by ${escapeAttr(BOARD_SORT_KEY_LABELS[NEXT_BOARD_SORT_KEY[key]])}">
      ${escapeHTML(BOARD_SORT_KEY_LABELS[key])}
    </button>
    <button type="button" class="sort-dir" data-board-sort-dir
      aria-label="Toggle sort direction, currently ${escapeAttr(DIR_LABELS[dir])}">
      <span aria-hidden="true">${DIR_ARROWS[dir]}</span>
    </button>
  `;
}

export class FlowBoardSort extends FlowElement {
  render(sort) {
    return renderBoardSort(sort);
  }

  handleClick(event) {
    if (event.target.closest?.("[data-board-sort-key]")) {
      event.preventDefault();
      const key = NEXT_BOARD_SORT_KEY[this.data?.key] || "activity";
      this.dispatchEvent(new CustomEvent("board-sort-change", { detail: { key }, bubbles: true }));
      return;
    }
    if (event.target.closest?.("[data-board-sort-dir]")) {
      event.preventDefault();
      const dir = this.data?.dir === "desc" ? "asc" : "desc";
      this.dispatchEvent(new CustomEvent("board-sort-change", { detail: { dir }, bubbles: true }));
    }
  }
}

define("flow-board-sort", FlowBoardSort);
