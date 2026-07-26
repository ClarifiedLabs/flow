// One board lane: a heading with its count and a keyed list of task cards.
// The lane reconciles rather than re-rendering so a card that is still on
// screen keeps its element — and its hover/focus state — across a poll.

import { escapeAttr, escapeHTML } from "../html.js";
import { define, FlowElement, reconcile } from "./base.js";
import "./task-card.js";

export class FlowLane extends FlowElement {
  render(lane) {
    if (!lane) return "";
    this.setAttribute("data-lane", lane.key || "");
    return `
      <h2><span class="marker">▸</span>${escapeHTML(lane.label)} · ${lane.cards.length}</h2>
      ${lane.cards.length ? `<div class="cards"></div>` : `<div class="empty">No tasks</div>`}
    `;
  }

  afterPaint() {
    const lane = this.data;
    if (!lane?.cards?.length) return;
    reconcile(this.querySelector(".cards"), lane.cards, {
      tag: "flow-task-card",
      key: (model) => `${model.projectID}:${model.id}`,
    });
  }
}

define("flow-lane", FlowLane);

export function laneAccentAttr(key) {
  return ` data-lane="${escapeAttr(key)}"`;
}
