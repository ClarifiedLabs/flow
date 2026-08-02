// The board route. Owns the lanes/table toggle and the shared card projection;
// the lanes and the table render the same models so they cannot disagree about
// what state a task is in.

import { activityGroupOf, cardModel, compareBoardCards, sortForAttention } from "../board-model.js";
import { laneTasks } from "../board.js";
import { BOARD_SORT_DIRS, BOARD_SORT_KEYS, LANES } from "../config.js";
import { value } from "../normalize.js";
import { readBoardSort, readBoardSortChoice, readBoardView, writeBoardSort, writeBoardView } from "../storage.js";
import { define, FlowElement, mount, reconcile } from "./base.js";
import "./attention-strip.js";
import "./board-sort.js";
import "./board-table.js";
import "./lane.js";
import "./throughput-strip.js";

// The split in-progress lanes get their own empty copy so an idle board reads
// as "No active work" / "Nothing waiting" rather than "No tasks" twice over.
const LANE_EMPTY_LABELS = {
  working: "No active work",
  waiting: "Nothing waiting",
};

// boardEntries flattens the aggregate per-project board payload into the flat
// list of entries the card projection expects.
export function boardEntries(data, { showProject = false } = {}) {
  const boards = value(data || {}, "boards", "Boards") || [];
  const entries = [];
  for (const projectBoard of boards) {
    const board = value(projectBoard, "board", "Board") || {};
    const cards = value(projectBoard, "task_cards", "TaskCards") || {};
    const laneStates = value(projectBoard, "lane_states", "LaneStates") || {};
    const blockedIDs = new Set(value(projectBoard, "blocked_ids", "BlockedIDs") || []);
    const project = {
      id: value(projectBoard, "project_id", "ProjectID") || "",
      name: value(projectBoard, "project_name", "ProjectName") || "",
    };
    for (const [key, , field] of LANES) {
      for (const task of laneTasks(board, key, field)) {
        const taskID = value(task, "id", "ID");
        const entry = {
          lane: key,
          task,
          card: cards[taskID] || {},
          laneState: laneStates[taskID] || "",
          blocked: blockedIDs.has(taskID),
          project,
        };
        // Working and Waiting are two presentations of the same InProgress
        // list: activityGroupOf decides which one owns the task, so each
        // in-progress task lands in exactly one lane.
        if ((key === "working" || key === "waiting") && activityGroupOf(entry) !== key) continue;
        entries.push(entry);
      }
    }
  }
  return entries.map((entry) => ({ ...entry, model: cardModel(entry, { showProject }) }));
}

export class FlowBoard extends FlowElement {
  view = readBoardView();
  // readBoardSort always returns a validated sort: the stored preference, or
  // the default { key: "number", dir: "asc" }, which the control and the
  // table headers display. The default is NOT a comparator sort: the server
  // sends the aggregate board project-grouped and does not order keyed ids
  // by trailing task number, so a global number sort would reorder today's
  // payload. Until the operator picks a sort (sortExplicit) the lanes keep
  // the server's order; the table falls back to its classic attention
  // grouping instead (see forward()) — the split keeps each surface honest
  // without expanding the sort feature.
  sort = readBoardSort();
  sortExplicit = readBoardSortChoice() !== null;

  render(data) {
    if (!data) return `<div class="empty">Loading board</div>`;
    this.setAttribute("data-view", this.view);
    return `
      <div class="board-header">
        <div class="view-toggle" role="group" aria-label="Board view">
          <button type="button" class="chip${this.view === "lanes" ? " active" : ""}" data-board-view="lanes" aria-pressed="${this.view === "lanes"}">Lanes</button>
          <button type="button" class="chip${this.view === "table" ? " active" : ""}" data-board-view="table" aria-pressed="${this.view === "table"}">Table</button>
        </div>
        <flow-board-sort></flow-board-sort>
      </div>
      <flow-attention-strip></flow-attention-strip>
      <flow-throughput-strip></flow-throughput-strip>
      <div class="surface"></div>
    `;
  }

  bind() {
    // The sort control and the table headers only report intent; the board
    // owns the sort state, applies it to both views, and persists it.
    this.addEventListener("board-sort-change", (event) => {
      const detail = event.detail || {};
      const key = BOARD_SORT_KEYS.has(detail.key) ? detail.key : this.sort.key;
      const dir = BOARD_SORT_DIRS.has(detail.dir) ? detail.dir : this.sort.dir;
      if (key === this.sort.key && dir === this.sort.dir) return;
      this.sort = { key, dir };
      // Any control or table-header operation is an explicit sort: from here
      // on the comparator applies cross-project, even if the choice lands on
      // the default key and direction.
      this.sortExplicit = true;
      writeBoardSort(this.sort);
      const surface = this.querySelector(".surface");
      const table = surface && surface.querySelector("flow-board-table");
      if (table) {
        // Re-sort the mounted table in place: rebuilding the surface would
        // discard the table's own filter state, and an active filter chip
        // must survive a header sort. The table renders the models in the
        // order it is handed, so pass it the freshly sorted list.
        const models = this.sortedModels();
        const sortControl = this.querySelector("flow-board-sort");
        if (sortControl) sortControl.data = this.sort;
        const attention = this.querySelector("flow-attention-strip");
        if (attention) attention.data = models.filter((model) => model.needsYou);
        table.sort = this.sort;
        table.sortExplicit = this.sortExplicit;
        table.data = models;
        return;
      }
      this.invalidate();
    });
  }

  // The models both views render, in the order the current sort asks for.
  // An explicit sort applies cross-project — the aggregate board is one flat
  // list. Without an explicit choice the entries keep the server's order:
  // the server sends the board project-grouped (its ListTasks ordering does
  // not sort keyed ids by trailing task number), so the lanes show a true
  // no-op on today's payload and only the table falls back to attention.
  sortedModels() {
    return this.sortedEntries().map((entry) => entry.model);
  }

  sortedEntries() {
    const entries = this.data?.entries || [];
    if (!this.sortExplicit) return entries;
    const { key = "number", dir = "asc" } = this.sort || {};
    // compareBoardCards sorts the card models; map the sorted order back to
    // the entries so the lanes can group them.
    const entryByID = new Map(entries.map((entry) => [entry.model.id, entry]));
    return compareBoardCards(
      entries.map((entry) => entry.model),
      { key, dir },
    )
      .map((model) => entryByID.get(model.id))
      .filter(Boolean);
  }

  // The board header never changes with the data, so the base paint skips the
  // write — and with it afterPaint — on every poll, leaving the mounted lanes
  // and table on the old models. Forward the fresh, sorted models on every
  // paint attempt, not just on writes, so a poll re-sorts and refreshes both
  // views in place.
  paint() {
    super.paint();
    this.forward();
  }

  afterPaint() {
    this.forward();
  }

  forward() {
    const entries = this.sortedEntries();
    const models = entries.map((entry) => entry.model);
    const attention = this.querySelector("flow-attention-strip");
    if (attention) attention.data = models.filter((model) => model.needsYou);

    const sortControl = this.querySelector("flow-board-sort");
    if (sortControl) sortControl.data = this.sort;
    const throughput = this.querySelector("flow-throughput-strip");
    if (throughput) throughput.data = this.data?.stats;

    const surface = this.querySelector(".surface");
    if (!surface) return;
    if (this.view === "table") {
      // Attention grouping rule: an explicit operator sort applies directly;
      // sortForAttention is only the default-state fallback. The lanes are
      // server-ordered by default, so the dense table — whose pre-sort
      // behaviour was attention-first — keeps that honest grouping until the
      // operator picks a sort, and the table's note says which one is live.
      const tableModels = this.sortExplicit ? models : sortForAttention(models);
      const table = mount(surface, "flow-board-table", tableModels);
      if (table) {
        table.sort = this.sort;
        table.sortExplicit = this.sortExplicit;
      }
      return;
    }
    this.paintLanes(surface, entries);
  }

  paintLanes(surface, entries) {
    if (!surface.classList.contains("lanes")) {
      surface.className = "surface lanes";
      surface.innerHTML = "";
    }
    const lanes = LANES.map(([key, label]) => ({
      key,
      label,
      emptyLabel: LANE_EMPTY_LABELS[key],
      cards: entries.filter((entry) => entry.lane === key).map((entry) => entry.model),
    }));
    reconcile(surface, lanes, { tag: "flow-lane", key: (lane) => lane.key });
  }

  handleClick(event) {
    const view = event.target.closest?.("[data-board-view]");
    if (!view) return;
    event.preventDefault();
    this.toggleView(view.dataset.boardView);
  }

  // toggleView is bound to the topbar control and to `v`. The choice persists,
  // because which view suits you is a property of how you work, not of a
  // session.
  toggleView(next) {
    const view = next || (this.view === "lanes" ? "table" : "lanes");
    if (view === this.view) return;
    this.view = view;
    writeBoardView(view);
    // The surface swaps wholesale between views, so drop the cached markup.
    const surface = this.querySelector(".surface");
    if (surface) surface.className = "surface";
    this.invalidate();
  }
}

define("flow-board", FlowBoard);
