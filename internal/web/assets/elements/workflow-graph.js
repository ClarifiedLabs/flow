// The map view: the flow definition, not this run.
//
// The old graph wrapped nodes onto rows, drew back edges as cubic curves that
// crossed whatever was in the way, and printed the node's kind under its name
// even when the two said the same thing. Here the nodes sit in one row and
// scroll horizontally, each back edge drops into its own reserved channel
// below the row so nothing ever crosses a node, and the kind is a caption
// rather than a repeat.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

const NODE_WIDTH = 126;
const NODE_HEIGHT = 46;
const NODE_GAP = 44;
const COLUMN = NODE_WIDTH + NODE_GAP;
const TOP = 28;
const CHANNEL_TOP = NODE_HEIGHT + TOP + 46;
const CHANNEL_GAP = 32;

// visitCounts collapses the server's per-edge tallies into a per-node visit
// count and a taken-edge set.
export function graphCounts(transitionCounts = []) {
  const nodeVisits = new Map();
  const takenEdges = new Map();
  for (const count of transitionCounts) {
    const from = value(count, "from", "From");
    const to = value(count, "to", "To");
    const outcome = value(count, "outcome", "Outcome");
    const times = Number(value(count, "count", "Count") || 0);
    takenEdges.set(`${from}|${outcome}|${to}`, times);
    nodeVisits.set(to, (nodeVisits.get(to) || 0) + times);
  }
  return { nodeVisits, takenEdges };
}

export function renderWorkflowGraph(model) {
  const snapshot = model?.snapshot || {};
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  if (!nodes.length) return `<p class="empty">No workflow nodes</p>`;

  const edges = value(snapshot, "edges", "Edges") || [];
  const { nodeVisits, takenEdges } = graphCounts(model?.transitionCounts);
  const currentKey = value(model?.run || {}, "current_node_key", "CurrentNodeKey");
  const index = new Map(nodes.map((node, position) => [value(node, "key", "Key"), position]));

  // Reserve one channel lane per back edge, deepest first, so two back edges
  // can never share a line.
  const backEdges = edges.filter((edge) => {
    const from = index.get(value(edge, "from", "From"));
    const to = index.get(value(edge, "to", "To"));
    return from !== undefined && to !== undefined && to <= from;
  });
  const channelFor = new Map(backEdges.map((edge, lane) => [edgeKey(edge), CHANNEL_TOP + lane * CHANNEL_GAP]));

  const width = nodes.length * COLUMN;
  const height = CHANNEL_TOP + Math.max(1, backEdges.length) * CHANNEL_GAP + 18;

  return `
    <div class="scroll">
      <svg viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="Workflow definition">
        <defs>
          <marker id="wg-arrow" viewBox="0 0 6 6" refX="5" refY="3" markerWidth="5" markerHeight="5" orient="auto">
            <path d="M0 0 L6 3 L0 6 z" fill="currentColor" />
          </marker>
        </defs>
        ${edges.map((edge) => renderEdge(edge, index, takenEdges, channelFor)).join("")}
        ${nodes.map((node, position) => renderNode(node, position, { currentKey, nodeVisits })).join("")}
      </svg>
    </div>
    <p class="legend">
      <span class="key is-taken"><i></i>taken this run</span>
      <span class="key is-possible"><i></i>possible, not taken</span>
      <span class="key is-unreached"><i></i>not yet reached</span>
    </p>
  `;
}

function edgeKey(edge) {
  return `${value(edge, "from", "From")}|${value(edge, "outcome", "Outcome")}|${value(edge, "to", "To")}`;
}

function renderNode(node, position, { currentKey, nodeVisits }) {
  const key = value(node, "key", "Key");
  const name = value(node, "name", "Name") || key;
  const kind = String(value(node, "kind", "Kind") || "").replaceAll("_", " ");
  const visits = nodeVisits.get(key) || 0;
  const isCurrent = key === currentKey;
  const reached = visits > 0 || isCurrent || position === 0;
  const x = position * COLUMN;

  return `
    <g class="node${isCurrent ? " is-current" : ""}${reached ? "" : " is-unreached"}" data-node="${escapeAttr(key)}">
      ${isCurrent ? `<rect class="halo" x="${x - 5}" y="${TOP - 5}" width="${NODE_WIDTH + 10}" height="${NODE_HEIGHT + 10}" rx="9" />` : ""}
      <rect x="${x}" y="${TOP}" width="${NODE_WIDTH}" height="${NODE_HEIGHT}" rx="4" />
      <text class="name" x="${x + 10}" y="${TOP + 19}">${escapeHTML(clip(name, 17))}</text>
      <text class="kind" x="${x + 10}" y="${TOP + 34}">${escapeHTML(clip(kind, 16))}</text>
      ${visits > 1 ? `<text class="visits" x="${x + NODE_WIDTH - 10}" y="${TOP + 34}">×${visits}</text>` : ""}
      <title>${escapeHTML(name)} (${escapeHTML(key)})</title>
    </g>
  `;
}

function renderEdge(edge, index, takenEdges, channelFor) {
  const fromPos = index.get(value(edge, "from", "From"));
  const toPos = index.get(value(edge, "to", "To"));
  if (fromPos === undefined || toPos === undefined) return "";
  const outcome = String(value(edge, "outcome", "Outcome") || "");
  const taken = takenEdges.get(edgeKey(edge)) || 0;
  const classes = `edge${taken ? " is-taken" : " is-untaken"}`;
  const midY = TOP + NODE_HEIGHT / 2;

  if (toPos > fromPos) {
    const startX = fromPos * COLUMN + NODE_WIDTH;
    const endX = toPos * COLUMN;
    return `
      <g class="${classes}">
        <path d="M${startX} ${midY} H${endX - 6}" marker-end="url(#wg-arrow)" />
        ${taken && outcome ? `<text class="edge-label" x="${(startX + endX) / 2}" y="${midY - 6}">${escapeHTML(outcome)}</text>` : ""}
      </g>
    `;
  }

  // A back edge drops out of the source's bottom edge, runs along its own
  // channel, and comes up into the target's bottom edge. It never crosses a
  // node because the channel is below the whole row.
  const channelY = channelFor.get(edgeKey(edge)) || CHANNEL_TOP;
  const startX = fromPos * COLUMN + NODE_WIDTH / 2;
  const endX = toPos * COLUMN + NODE_WIDTH / 2;
  const bottom = TOP + NODE_HEIGHT;
  const label = [outcome, taken > 1 ? `×${taken}` : ""].filter(Boolean).join(" ");
  return `
    <g class="${classes}">
      <path d="M${startX} ${bottom} V${channelY} H${endX} V${bottom + 6}" marker-end="url(#wg-arrow)" />
      ${label ? `<text class="edge-label" x="${(startX + endX) / 2}" y="${channelY + 13}">${escapeHTML(label)}</text>` : ""}
    </g>
  `;
}

function clip(text, max) {
  const value = String(text || "");
  return value.length > max ? `${value.slice(0, max - 1)}…` : value;
}

export class FlowWorkflowGraph extends FlowElement {
  render(model) {
    return renderWorkflowGraph(model);
  }
}

define("flow-workflow-graph", FlowWorkflowGraph);
