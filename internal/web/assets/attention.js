// Human-attention panel, status/transition rendering and the lifecycle SVG
// chart.

import { phaseKey, renderPhaseBadge } from "./board.js";
import { formatDate } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { projectButtonAttr } from "./issue.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";

export function renderHumanAttentionPanel(issue, statusLog, projectID, activeSession) {
  const issueID = value(issue, "id", "ID");
  let html = "";

  const body = value(issue, "plan_body", "PlanBody");
  const approvedAt = value(issue, "plan_approved_at", "PlanApprovedAt");
  if (body && !approvedAt) {
    const submittedAt = value(issue, "plan_submitted_at", "PlanSubmittedAt");
    html += `
      <section class="human-attention-panel">
        <div class="human-attention-head">
          <div>
            <h3>Plan Review</h3>
            ${submittedAt ? `<p class="meta-quiet">${escapeHTML(formatDate(submittedAt))}</p>` : ""}
          </div>
          <div class="actions">
            <button class="button" data-plan-approve="${escapeAttr(issueID)}"${projectButtonAttr(projectID)}>Approve</button>
            <button class="button secondary" data-plan-reject="${escapeAttr(issueID)}"${projectButtonAttr(projectID)}>Reject</button>
          </div>
        </div>
        ${renderMarkdown(body, { className: "human-attention-body md" })}
      </section>
    `;
  }

  const question = latestStatusOfKind(statusLog, "question");
  if (question && value(activeSession, "state", "State") === "waiting") {
    const statusID = value(question, "id", "ID");
    html += `
      <section class="human-attention-panel">
        <div class="human-attention-head">
          <div>
            <h3>Needs Human Response</h3>
            <p class="meta-quiet">${escapeHTML(formatDate(value(question, "created_at", "CreatedAt")))}</p>
          </div>
        </div>
        ${renderMarkdown(value(question, "message", "Message"), { className: "human-attention-body md" })}
        <form class="human-attention-reply" data-attention-reply-form="${escapeAttr(issueID)}" data-status-log-id="${escapeAttr(statusID)}"${projectButtonAttr(projectID)}>
          <textarea name="message" rows="3" placeholder="Reply"></textarea>
          <button class="button" type="submit">Send Reply</button>
        </form>
      </section>
    `;
  }

  return html;
}

export function latestStatusOfKind(statusLog, kind) {
  return (statusLog || []).find((entry) => value(entry, "kind", "Kind") === kind) || null;
}

export const STATUS_KIND_BADGE = {
  blocker: "danger",
  question: "warn",
  plan: "action",
  progress: "run",
  note: "idle",
};

export function renderStatusKindBadge(kind) {
  const slug = String(kind || "note");
  const variant = STATUS_KIND_BADGE[slug] || "idle";
  return `<span class="badge ${variant}">${escapeHTML(slug)}</span>`;
}

export function renderStatus(entry) {
  const message = value(entry, "message", "Message");
  const actor = value(entry, "actor", "Actor");
  const kind = value(entry, "kind", "Kind");
  const createdAt = formatDate(value(entry, "created_at", "CreatedAt"));
  return `<article class="feed-item"><strong>${escapeHTML(actor)}</strong>${renderStatusKindBadge(kind)}<span>${escapeHTML(createdAt)}</span><p>${escapeHTML(message)}</p></article>`;
}

export function renderTransition(entry) {
  const eventKind = value(entry, "event_kind", "EventKind");
  const fromPhase = value(entry, "from_phase", "FromPhase") || "—";
  const toPhase = value(entry, "to_phase", "ToPhase");
  const guardResult = value(entry, "guard_result", "GuardResult");
  const actor = value(entry, "actor", "Actor");
  const createdAt = formatDate(value(entry, "created_at", "CreatedAt"));
  const meta = [actor, guardResult].filter(Boolean).map(escapeHTML).join(" · ");
  return `<article class="feed-item"><strong>${escapeHTML(eventKind)}</strong><span>${escapeHTML(createdAt)}</span><p class="phase-flow">${renderPhaseBadge(fromPhase)}<span class="arrow">→</span>${renderPhaseBadge(toPhase)}${meta ? `<span class="muted">(${meta})</span>` : ""}</p></article>`;
}

// Canonical lifecycle topology for the issue flow chart, on a fixed grid.
// Node keys are the Phase strings from internal/coordinator/phase.go.
// Pending (agent-created) issues start in triage and move to backlog when
// accepted; owner-created issues start accepted in backlog, so their first
// logged transition enters backlog directly. acceptance is a real derived
// phase (critique satisfied, verifier pending); the merge step is synchronous,
// so no issue rests in a merging phase and approved leads straight to
// merged_closed.
export const LIFECYCLE_NODES = [
  { key: "triage", col: 0, row: 1 },
  { key: "backlog", col: 1, row: 1 },
  { key: "up_next", col: 2, row: 1 },
  { key: "planning", col: 3, row: 0 },
  { key: "authoring", col: 3, row: 1 },
  { key: "critique", col: 4, row: 1 },
  { key: "acceptance", col: 5, row: 1 },
  { key: "approved", col: 6, row: 1 },
  { key: "merged_closed", col: 7, row: 1 },
  { key: "rejected_closed", col: 0, row: 2 },
  { key: "abandoned", col: 4, row: 2 },
];

// Happy path, the planning detour, the reviewer/verifier sent-back edges, and
// the closed-issue exits. Plan approval returns to up_next so a separate
// implementation session can start. The engine emits critique→acceptance when
// the verifier gate opens, acceptance→approved when it closes, and
// approved→merged_closed on the synchronous merge; critique→approved stays for
// suites with no verifier check. Observed transitions outside this list still
// render as dashed overlay edges so history is never hidden.
export const LIFECYCLE_EDGES = [
  { from: "triage", to: "backlog" },
  { from: "backlog", to: "up_next" },
  { from: "up_next", to: "backlog" },
  { from: "up_next", to: "planning" },
  { from: "planning", to: "up_next" },
  { from: "up_next", to: "authoring" },
  { from: "authoring", to: "critique" },
  { from: "critique", to: "acceptance" },
  { from: "acceptance", to: "approved" },
  { from: "critique", to: "approved" },
  { from: "approved", to: "merged_closed" },
  { from: "critique", to: "authoring" },
  { from: "acceptance", to: "authoring" },
  { from: "triage", to: "rejected_closed" },
  { from: "critique", to: "abandoned" },
];

export const LC_NODE_W = 96;

export const LC_NODE_H = 34;

export const LC_COL_W = 130;

export const LC_ROW_H = 86;

export const LC_PAD = 14;

export function lifecycleNodeBox(node) {
  const x = LC_PAD + node.col * LC_COL_W;
  const y = LC_PAD + node.row * LC_ROW_H;
  return { x, y, cx: x + LC_NODE_W / 2, cy: y + LC_NODE_H / 2 };
}

// lifecycleEdgeShape routes an edge from grid geometry alone: forward
// same-row edges run straight through the column gap (or arc over the row
// when they skip columns, so they clear the nodes in between), backward edges
// bow underneath the row, same-column edges run vertically (offset so an
// opposing pair does not overlap), and anything else takes a shallow curve
// between the node bottoms.
export function lifecycleEdgeShape(fromNode, toNode) {
  const a = lifecycleNodeBox(fromNode);
  const b = lifecycleNodeBox(toNode);
  if (fromNode.row === toNode.row && toNode.col === fromNode.col + 1) {
    const y = a.cy;
    return { d: `M ${a.x + LC_NODE_W} ${y} L ${b.x} ${y}`, labelX: (a.x + LC_NODE_W + b.x) / 2, labelY: y - 5, anchor: "middle" };
  }
  if (fromNode.row === toNode.row && toNode.col > fromNode.col) {
    const midX = (a.cx + b.cx) / 2;
    const ctrlY = a.y - 24 - 7 * (toNode.col - fromNode.col);
    return { d: `M ${a.cx} ${a.y} Q ${midX} ${ctrlY} ${b.cx} ${b.y}`, labelX: midX, labelY: (a.y + ctrlY) / 2 - 3, anchor: "middle" };
  }
  if (fromNode.row === toNode.row && toNode.col < fromNode.col) {
    const startY = a.y + LC_NODE_H;
    const midX = (a.cx + b.cx) / 2;
    const ctrlY = startY + 30 + 12 * (fromNode.col - toNode.col);
    return { d: `M ${a.cx} ${startY} Q ${midX} ${ctrlY} ${b.cx} ${startY}`, labelX: midX, labelY: (startY + ctrlY) / 2 + 11, anchor: "middle" };
  }
  if (fromNode.col === toNode.col) {
    const up = toNode.row < fromNode.row;
    const x = a.cx + (up ? -20 : 20);
    const startY = up ? a.y : a.y + LC_NODE_H;
    const endY = up ? b.y + LC_NODE_H : b.y;
    return { d: `M ${x} ${startY} L ${x} ${endY}`, labelX: x + (up ? -6 : 6), labelY: (startY + endY) / 2 + 3, anchor: up ? "end" : "start" };
  }
  const startY = a.y + LC_NODE_H;
  const endY = b.y + LC_NODE_H;
  const midX = (a.cx + b.cx) / 2;
  const ctrlY = Math.max(startY, endY) + 40;
  return { d: `M ${a.cx} ${startY} Q ${midX} ${ctrlY} ${b.cx} ${endY}`, labelX: midX, labelY: (startY + endY) / 4 + ctrlY / 2 + 11, anchor: "middle" };
}

// renderLifecycleChart draws the full canonical lifecycle as inline SVG:
// every phase node (current one highlighted), taken edges solid with ×count
// labels, untaken edges dimmed, and observed non-canonical edges overlaid
// dashed. The reviewer/verifier sent-back tallies render as a legend because
// they come from check payloads and cannot be attributed to a single edge.
export function renderLifecycleChart(graph) {
  const currentPhase = value(graph || {}, "current_phase", "CurrentPhase") || "";
  const reviewerSends = Number(value(graph || {}, "reviewer_sends", "ReviewerSends") || 0);
  const verifierSends = Number(value(graph || {}, "verifier_sends", "VerifierSends") || 0);
  const counts = new Map();
  for (const edge of value(graph || {}, "edges", "Edges") || []) {
    const from = value(edge, "from_phase", "FromPhase");
    const to = value(edge, "to_phase", "ToPhase");
    counts.set(`${from}|${to}`, Number(value(edge, "count", "Count") || 0));
  }

  const nodesByKey = new Map(LIFECYCLE_NODES.map((node) => [node.key, node]));
  const edgeMarkup = (fromNode, toNode, count, extra) => {
    const shape = lifecycleEdgeShape(fromNode, toNode);
    const classes = ["lifecycle-edge", count > 0 ? "is-taken" : "is-untaken"];
    if (extra) classes.push("is-extra");
    const marker = count > 0 ? "lc-arrow" : "lc-arrow-dim";
    const label = count > 0 ? `<text class="lifecycle-edge-label" x="${shape.labelX}" y="${shape.labelY}" text-anchor="${shape.anchor}">×${count}</text>` : "";
    return `<path class="${classes.join(" ")}" d="${shape.d}" marker-end="url(#${marker})"/>${label}`;
  };

  const parts = [];
  const canonical = new Set();
  for (const edge of LIFECYCLE_EDGES) {
    canonical.add(`${edge.from}|${edge.to}`);
    parts.push(edgeMarkup(nodesByKey.get(edge.from), nodesByKey.get(edge.to), counts.get(`${edge.from}|${edge.to}`) || 0, false));
  }
  for (const [key, count] of counts) {
    if (canonical.has(key) || count <= 0) continue;
    const [from, to] = key.split("|");
    const fromNode = nodesByKey.get(from);
    const toNode = nodesByKey.get(to);
    if (!fromNode || !toNode) continue;
    parts.push(edgeMarkup(fromNode, toNode, count, true));
  }
  for (const node of LIFECYCLE_NODES) {
    const box = lifecycleNodeBox(node);
    const classes = ["lifecycle-node"];
    const isCurrent = node.key === currentPhase;
    if (isCurrent) classes.push("is-current");
    const halo = isCurrent ? `<rect class="lifecycle-current-halo" x="${box.x - 5}" y="${box.y - 5}" width="${LC_NODE_W + 10}" height="${LC_NODE_H + 10}" rx="12" aria-hidden="true"/>` : "";
    parts.push(`<g class="${classes.join(" ")}" data-node="${escapeAttr(node.key)}" data-phase="${escapeAttr(phaseKey(node.key))}">${halo}<rect x="${box.x}" y="${box.y}" width="${LC_NODE_W}" height="${LC_NODE_H}" rx="8"/><text x="${box.cx}" y="${box.cy + 3.5}" text-anchor="middle">${escapeHTML(node.key.replaceAll("_", " "))}</text></g>`);
  }

  const width = LC_PAD * 2 + 8 * LC_COL_W + LC_NODE_W;
  const height = LC_PAD * 2 + 2 * LC_ROW_H + LC_NODE_H;
  const legend = reviewerSends || verifierSends
    ? `<p class="lifecycle-legend">Sent back — reviewer ×${reviewerSends} · verifier ×${verifierSends}</p>`
    : "";
  const defs = `<defs><marker id="lc-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path class="lifecycle-arrow" d="M0 0L8 4L0 8z"/></marker><marker id="lc-arrow-dim" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path class="lifecycle-arrow dim" d="M0 0L8 4L0 8z"/></marker></defs>`;
  return `<svg viewBox="0 0 ${width} ${height}" role="img" aria-label="Issue lifecycle">${defs}${parts.join("")}</svg>${legend}`;
}
