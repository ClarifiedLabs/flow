// The board route. Owns the lanes/table toggle and the shared card projection;
// the lanes and the table render the same models so they cannot disagree about
// what state a task is in.

import { activityGroupOf, cardModel } from "../board-model.js";
import { laneTasks } from "../board.js";
import { LANES } from "../config.js";
import { value } from "../normalize.js";
import { readBoardView, writeBoardView } from "../storage.js";
import { define, FlowElement, mount, reconcile } from "./base.js";
import "./attention-strip.js";
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

  render(data) {
    if (!data) return `<div class="empty">Loading board</div>`;
    this.setAttribute("data-view", this.view);
    return `
      <flow-attention-strip></flow-attention-strip>
      <flow-throughput-strip></flow-throughput-strip>
      <div class="surface"></div>
    `;
  }

  // The base paint skips the write — and with it afterPaint — when the board
  // markup is unchanged, but a poll that only changed the numbers leaves that
  // markup identical while the strips and the lanes still need the fresh
  // data. Forward on every paint attempt, not just on writes (same pattern as
  // the task rail); the strips and lanes are fed in place, so their instances
  // — and the hover and focus on them — survive the poll.
  paint() {
    super.paint();
    this.syncChildren();
  }

  afterPaint() {
    this.syncChildren();
  }

  syncChildren() {
    const entries = this.data?.entries || [];
    const models = entries.map((entry) => entry.model);
    const attention = this.querySelector("flow-attention-strip");
    if (attention) attention.data = models.filter((model) => model.needsYou);

    const throughput = this.querySelector("flow-throughput-strip");
    if (throughput) throughput.data = this.data?.stats;

    const surface = this.querySelector(".surface");
    if (!surface) return;
    if (this.view === "table") {
      mount(surface, "flow-board-table", models);
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
