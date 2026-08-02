// Human-attention panel, status/transition rendering and the lifecycle SVG
// chart.

import { phaseKey, renderPhaseBadge } from "./board.js";
import { renderGateOutcomeButtons } from "./elements/review-panel.js";
import { formatDate } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { projectButtonAttr } from "./task.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";

export function renderHumanAttentionPanel(task, statusLog, projectID, activeSession) {
  const taskID = value(task, "id", "ID");
  let html = "";

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
        <form class="human-attention-reply" data-attention-reply-form="${escapeAttr(taskID)}" data-status-log-id="${escapeAttr(statusID)}"${projectButtonAttr(projectID)}>
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

// workflowCurrentArtifact returns the artifact flowing through the current
// workflow node. Callers use its kind to place human gates beside the artifact
// being reviewed (for example, change gates belong on change detail).
export function workflowCurrentArtifact(workflowData) {
  const detail = value(workflowData || {}, "detail", "Detail") || {};
  const run = value(detail, "run", "Run") || {};
  const currentArtifactID = String(value(run, "current_artifact_id", "CurrentArtifactID") || "").trim();
  if (!currentArtifactID) return null;
  const artifacts = value(workflowData || {}, "artifacts", "Artifacts") || [];
  return artifacts.find((artifact) => String(value(artifact, "id", "ID") || "").trim() === currentArtifactID) || null;
}

// workflowChangeArtifactID extracts the change referenced by a workflow change
// artifact. RawMessage payloads arrive as objects in the browser, while tests
// and other callers may provide their JSON string representation.
export function workflowChangeArtifactID(artifact) {
  if (value(artifact || {}, "kind", "Kind") !== "change") return "";
  let payload = value(artifact || {}, "payload", "Payload") || {};
  if (typeof payload === "string") {
    try {
      payload = JSON.parse(payload);
    } catch {
      return "";
    }
  }
  return String(value(payload || {}, "change_id", "ChangeID") || "").trim();
}

// renderWorkflowHumanGatePanel renders a generic workflow human gate. Placement
// is decided by the caller from workflowCurrentArtifact: task-only artifacts
// stay on task detail, while change artifacts are rendered on change detail.
export function renderWorkflowHumanGatePanel(workflowData, taskID, projectID) {
  const detail = value(workflowData || {}, "detail", "Detail") || {};
  const wait = value(detail, "open_wait", "OpenWait") || null;
  if (!wait || value(wait, "kind", "Kind") !== "human_gate") return "";

  const run = value(detail, "run", "Run") || {};
  const snapshot = value(run, "snapshot", "Snapshot") || {};
  const currentNodeKey = String(value(run, "current_node_key", "CurrentNodeKey") || "").trim();
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  const currentNode = Array.isArray(nodes)
    ? nodes.find((node) => String(value(node, "key", "Key") || "").trim() === currentNodeKey) || {}
    : {};
  const config = value(currentNode, "config", "Config") || {};
  const gateConfig = value(config, "human_gate", "HumanGate") || {};
  const outcomes = value(gateConfig, "outcomes", "Outcomes") || [];
  const nodeRunID = value(wait, "node_run_id", "NodeRunID");
  return `<section class="human-attention-panel" data-gate-node-run="${escapeAttr(nodeRunID)}"><div><h3>${escapeHTML(value(currentNode, "name", "Name") || "Human action required")}</h3><p>${escapeHTML(value(gateConfig, "instructions", "Instructions") || value(wait, "message", "Message") || "Choose the next workflow outcome.")}</p><textarea data-workflow-feedback rows="3" placeholder="Optional feedback for the next node"></textarea></div><div class="actions">${renderGateOutcomeButtons(outcomes, { nodeRunID, waitID: String(value(wait, "id", "ID") || ""), taskID, projectAttr: projectButtonAttr(projectID) })}</div></section>`;
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


export const LC_NODE_W = 96;

export const LC_NODE_H = 34;

export const LC_COL_W = 130;

export const LC_ROW_H = 86;

export const LC_PAD = 14;

