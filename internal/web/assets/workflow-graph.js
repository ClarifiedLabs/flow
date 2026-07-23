// Generic workflow graph rendering shared by task run details and the Flows
// definition editor. The layout is deterministic and dependency-free so the
// server can continue to serve the web UI as static ES modules.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

const NODE_WIDTH = 150;
const NODE_HEIGHT = 52;
const COLUMN_WIDTH = 244;
const ROW_HEIGHT = 92;
const PAD_X = 28;
const PAD_Y = 46;

export function workflowEdgeKey(from, outcome, to) {
  return JSON.stringify([String(from || ""), String(outcome || ""), String(to || "")]);
}

// workflowTransitionCounts counts only actual outcome-driven node transitions.
// Lifecycle-only rows such as task_scheduled/workflow_completed have no outcome
// and are deliberately excluded; otherwise the terminal edge would be counted
// twice (once for node_completed and once for workflow_completed).
export function workflowTransitionCounts(transitions) {
  const counts = new Map();
  for (const transition of transitions || []) {
    const from = value(transition, "from_node_key", "FromNodeKey");
    const outcome = value(transition, "outcome", "Outcome");
    const to = value(transition, "to_node_key", "ToNodeKey");
    if (!from || !outcome || !to) continue;
    const key = workflowEdgeKey(from, outcome, to);
    counts.set(key, (counts.get(key) || 0) + 1);
  }
  return counts;
}

function workflowNodes(graph) {
  const seen = new Set();
  const nodes = [];
  for (const source of value(graph || {}, "nodes", "Nodes") || []) {
    const key = String(value(source, "key", "Key") || "").trim();
    if (!key || seen.has(key)) continue;
    seen.add(key);
    nodes.push({
      key,
      name: String(value(source, "name", "Name") || key),
      kind: String(value(source, "kind", "Kind") || ""),
      index: nodes.length,
    });
  }
  return nodes;
}

function workflowEdges(graph, nodesByKey) {
  return (value(graph || {}, "edges", "Edges") || []).map((source) => ({
    from: String(value(source, "from", "From") || "").trim(),
    outcome: String(value(source, "outcome", "Outcome") || "").trim(),
    to: String(value(source, "to", "To") || "").trim(),
  })).filter((edge) => edge.from && edge.to && nodesByKey.has(edge.from) && nodesByKey.has(edge.to));
}

// Assign each node to its shortest-path distance from the start. Cycles become
// back edges, while branches naturally share a column. Valid stored workflows
// are reachable; the fallback also keeps partially-authored editor graphs
// visible instead of dropping disconnected nodes.
function workflowRanks(nodes, edges, requestedStart) {
  const nodeKeys = new Set(nodes.map((node) => node.key));
  const start = nodeKeys.has(requestedStart) ? requestedStart : nodes[0]?.key || "";
  const outgoing = new Map(nodes.map((node) => [node.key, []]));
  for (const edge of edges) outgoing.get(edge.from)?.push(edge.to);

  const ranks = new Map();
  if (start) ranks.set(start, 0);
  const queue = start ? [start] : [];
  for (let cursor = 0; cursor < queue.length; cursor += 1) {
    const from = queue[cursor];
    const nextRank = ranks.get(from) + 1;
    for (const to of outgoing.get(from) || []) {
      if (ranks.has(to)) continue;
      ranks.set(to, nextRank);
      queue.push(to);
    }
  }

  let fallbackRank = ranks.size ? Math.max(...ranks.values()) + 1 : 0;
  for (const node of nodes) {
    if (ranks.has(node.key)) continue;
    ranks.set(node.key, fallbackRank);
    fallbackRank += 1;
  }
  return { ranks, start };
}

function workflowLayout(graph) {
  const nodes = workflowNodes(graph);
  const nodesByKey = new Map(nodes.map((node) => [node.key, node]));
  const edges = workflowEdges(graph, nodesByKey);
  const requestedStart = String(value(graph || {}, "start_node", "StartNode") || "").trim();
  const { ranks, start } = workflowRanks(nodes, edges, requestedStart);
  const columns = new Map();
  for (const node of nodes) {
    const rank = ranks.get(node.key);
    if (!columns.has(rank)) columns.set(rank, []);
    columns.get(rank).push(node);
  }
  const maxRows = Math.max(1, ...Array.from(columns.values(), (column) => column.length));
  const maxRank = Math.max(0, ...ranks.values());
  const positions = new Map();
  for (const [rank, column] of columns) {
    const top = PAD_Y + ((maxRows - column.length) * ROW_HEIGHT) / 2;
    column.forEach((node, row) => {
      const x = PAD_X + rank * COLUMN_WIDTH;
      const y = top + row * ROW_HEIGHT;
      positions.set(node.key, { x, y, cx: x + NODE_WIDTH / 2, cy: y + NODE_HEIGHT / 2, rank });
    });
  }

  const pairs = new Map();
  for (const edge of edges) {
    const key = JSON.stringify([edge.from, edge.to]);
    if (!pairs.has(key)) pairs.set(key, []);
    pairs.get(key).push(edge);
  }
  const pairIndexes = new Map();
  let hasBackEdge = false;
  let maxBackIndex = 0;
  for (const pair of pairs.values()) {
    pair.forEach((edge, index) => pairIndexes.set(edge, { index, count: pair.length }));
    const first = pair[0];
    const from = positions.get(first.from);
    const to = positions.get(first.to);
    if (from && to && to.rank <= from.rank) {
      hasBackEdge = true;
      maxBackIndex = Math.max(maxBackIndex, pair.length - 1);
    }
  }

  const width = PAD_X * 2 + maxRank * COLUMN_WIDTH + NODE_WIDTH;
  const baseHeight = PAD_Y * 2 + (maxRows - 1) * ROW_HEIGHT + NODE_HEIGHT;
  const height = baseHeight + (hasBackEdge ? 72 + maxBackIndex * 14 : 0);
  return { nodes, edges, positions, pairIndexes, start, width, height };
}

function edgeShape(from, to, parallel) {
  const index = parallel?.index || 0;
  const count = parallel?.count || 1;
  if (from.x === to.x && from.y === to.y) {
    const radius = 34 + index * 13;
    const startX = from.x + NODE_WIDTH;
    const startY = from.cy - 10;
    const endY = from.cy + 10;
    return {
      d: `M ${startX} ${startY} C ${startX + radius} ${startY - radius}, ${startX + radius} ${endY + radius}, ${startX} ${endY}`,
      labelX: startX + radius + 4,
      labelY: from.cy + 4,
      anchor: "start",
    };
  }
  if (to.rank > from.rank) {
    const offset = (index - (count - 1) / 2) * 18;
    const startX = from.x + NODE_WIDTH;
    const endX = to.x;
    const span = Math.max(36, (endX - startX) * 0.45);
    return {
      d: `M ${startX} ${from.cy} C ${startX + span} ${from.cy + offset}, ${endX - span} ${to.cy + offset}, ${endX} ${to.cy}`,
      labelX: (startX + endX) / 2,
      labelY: (from.cy + to.cy) / 2 + offset - 8,
      anchor: "middle",
    };
  }

  const startX = from.cx;
  const startY = from.y + NODE_HEIGHT;
  const endX = to.cx;
  const endY = to.y + NODE_HEIGHT;
  const bendY = Math.max(startY, endY) + 44 + index * 14;
  return {
    d: `M ${startX} ${startY} C ${startX} ${bendY}, ${endX} ${bendY}, ${endX} ${endY}`,
    labelX: (startX + endX) / 2,
    labelY: bendY - 7,
    anchor: "middle",
  };
}

function markerID(layout) {
  let hash = 2166136261;
  const source = `${layout.nodes.map((node) => node.key).join("|")}::${layout.edges.map((edge) => workflowEdgeKey(edge.from, edge.outcome, edge.to)).join("|")}`;
  for (let i = 0; i < source.length; i += 1) {
    hash ^= source.charCodeAt(i);
    hash = Math.imul(hash, 16777619);
  }
  return `workflow-arrow-${(hash >>> 0).toString(36)}`;
}

function shortLabel(text, maxLength = 24) {
  const chars = Array.from(String(text || ""));
  return chars.length <= maxLength ? chars.join("") : `${chars.slice(0, maxLength - 1).join("")}…`;
}

// renderWorkflowGraph draws an arbitrary trusted workflow definition. Supplying
// transitionCounts turns it into a run chart: traversed edges are emphasized,
// every edge gains an ×count, and activeNode is highlighted.
export function renderWorkflowGraph(graph, options = {}) {
  const layout = workflowLayout(graph || {});
  if (!layout.nodes.length) return `<div class="empty workflow-graph-empty">No workflow nodes</div>`;

  const activeNode = String(options.activeNode || "").trim();
  const counts = options.transitionCounts instanceof Map ? options.transitionCounts : null;
  const label = options.ariaLabel || "Workflow graph";
  const baseMarkerID = markerID(layout);
  const takenMarkerID = `${baseMarkerID}-taken`;
  const dimMarkerID = `${baseMarkerID}-dim`;
  const parts = [];

  for (const edge of layout.edges) {
    const from = layout.positions.get(edge.from);
    const to = layout.positions.get(edge.to);
    const shape = edgeShape(from, to, layout.pairIndexes.get(edge));
    const count = counts ? counts.get(workflowEdgeKey(edge.from, edge.outcome, edge.to)) || 0 : null;
    const stateClass = counts ? (count > 0 ? "is-taken" : "is-untaken") : "";
    const classes = ["workflow-edge", stateClass].filter(Boolean);
    const groupClasses = ["workflow-edge-group", stateClass].filter(Boolean);
    const marker = counts ? (count > 0 ? takenMarkerID : dimMarkerID) : baseMarkerID;
    const outcome = edge.outcome || "transition";
    const countLabel = counts ? ` ×${count}` : "";
    parts.push(`<g class="${groupClasses.join(" ")}" data-edge-from="${escapeAttr(edge.from)}" data-edge-outcome="${escapeAttr(edge.outcome)}" data-edge-to="${escapeAttr(edge.to)}"><path class="${classes.join(" ")}" d="${shape.d}" marker-end="url(#${marker})"/><text class="workflow-edge-label" x="${shape.labelX}" y="${shape.labelY}" text-anchor="${shape.anchor}">${escapeHTML(outcome)}${escapeHTML(countLabel)}</text></g>`);
  }

  for (const node of layout.nodes) {
    const box = layout.positions.get(node.key);
    const isCurrent = node.key === activeNode;
    const isStart = node.key === layout.start;
    const classes = ["workflow-node"];
    if (isCurrent) classes.push("is-current");
    if (isStart) classes.push("is-start");
    const halo = isCurrent ? `<rect class="workflow-current-halo" x="${box.x - 6}" y="${box.y - 6}" width="${NODE_WIDTH + 12}" height="${NODE_HEIGHT + 12}" rx="12" aria-hidden="true"/>` : "";
    const startLabel = isStart ? `<text class="workflow-node-start" x="${box.x}" y="${box.y - 8}">start</text>` : "";
    const kindLabel = node.kind ? shortLabel(node.kind.replaceAll("_", " "), 26) : shortLabel(node.key, 26);
    parts.push(`<g class="${classes.join(" ")}" data-node="${escapeAttr(node.key)}">${startLabel}${halo}<rect x="${box.x}" y="${box.y}" width="${NODE_WIDTH}" height="${NODE_HEIGHT}" rx="8"/><text class="workflow-node-name" x="${box.cx}" y="${box.cy - 3}" text-anchor="middle">${escapeHTML(shortLabel(node.name))}</text><text class="workflow-node-kind" x="${box.cx}" y="${box.cy + 14}" text-anchor="middle">${escapeHTML(kindLabel)}</text><title>${escapeHTML(`${node.name} (${node.key})`)}</title></g>`);
  }

  const defs = `<defs><marker id="${baseMarkerID}" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path class="workflow-arrow" d="M0 0L8 4L0 8z"/></marker><marker id="${takenMarkerID}" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path class="workflow-arrow taken" d="M0 0L8 4L0 8z"/></marker><marker id="${dimMarkerID}" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path class="workflow-arrow dim" d="M0 0L8 4L0 8z"/></marker></defs>`;
  return `<svg viewBox="0 0 ${layout.width} ${layout.height}" width="${layout.width}" height="${layout.height}" role="img" aria-label="${escapeAttr(label)}">${defs}${parts.join("")}</svg>`;
}
