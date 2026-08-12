// Run-spine projection: node run states and durations, and one row per
// node *visit* in execution order, so a loop or a retry becomes an extra row
// rather than an edge crossing the diagram.

import { formatDwell } from "../board-model.js";
import { value } from "../normalize.js";

// nodeState collapses a node run into the four states the run spine draws.
export function nodeState(nodeRun, currentNodeRunID) {
  const state = String(value(nodeRun, "state", "State") || "");
  if (value(nodeRun, "id", "ID") === currentNodeRunID) return "current";
  if (state === "succeeded") return "done";
  if (state === "failed" || state === "cancelled") return "failed";
  if (state === "queued" || state === "running" || state === "waiting") return "current";
  return "future";
}

export function nodeDuration(nodeRun) {
  const started = value(nodeRun, "started_at", "StartedAt") || value(nodeRun, "created_at", "CreatedAt");
  const ended = value(nodeRun, "completed_at", "CompletedAt");
  if (!started) return "";
  const from = new Date(started).getTime();
  const to = ended ? new Date(ended).getTime() : Date.now();
  if (Number.isNaN(from) || Number.isNaN(to)) return "";
  return formatDwell(started, to);
}

// runRows turns the node run history into one row per node *visit*, in
// execution order. This is the fix for the tangled graph: a loop or a retry
// becomes an extra row rather than an edge crossing the diagram.
export function runRows(detail) {
  const run = value(detail || {}, "run", "Run") || {};
  const nodeRuns = value(detail || {}, "node_runs", "NodeRuns") || [];
  const snapshot = value(run, "snapshot", "Snapshot") || {};
  const nodes = value(snapshot, "nodes", "Nodes") || [];
  const currentNodeRunID = String(value(run, "current_node_run_id", "CurrentNodeRunID") || "");

  const rows = nodeRuns.map((nodeRun, index) => {
    const visit = Number(value(nodeRun, "visit", "Visit") || 1);
    const kind = String(value(nodeRun, "kind", "Kind") || "");
    const state = nodeState(nodeRun, currentNodeRunID);
    const outcome = String(value(nodeRun, "outcome", "Outcome") || "");
    const error = String(value(nodeRun, "error", "Error") || "");
    // The name is shown once and the kind is a tag, never a second line of
    // prose repeating the name.
    return {
      id: value(nodeRun, "id", "ID"),
      nodeKey: value(nodeRun, "node_key", "NodeKey"),
      name: value(nodeRun, "name", "Name") || value(nodeRun, "node_key", "NodeKey"),
      tag: visit > 1 ? `visit ${visit}` : kind.replaceAll("_", " "),
      kind,
      visit,
      state,
      outcome: state === "failed" ? error || outcome || "failed" : outcome,
      duration: nodeDuration(nodeRun),
      jobs: value(nodeRun, "jobs", "Jobs") || [],
      loop: loopBackLabel(nodeRuns, index, nodes),
      artifactID: value(nodeRun, "output_artifact_id", "OutputArtifactID") || "",
    };
  });

  // Nodes the run has not reached yet, in graph order, so the spine shows
  // what is still ahead rather than stopping at the present.
  const visited = new Set(rows.map((row) => row.nodeKey));
  for (const node of nodes) {
    const key = value(node, "key", "Key");
    if (visited.has(key)) continue;
    rows.push({
      id: `future:${key}`,
      nodeKey: key,
      name: value(node, "name", "Name") || key,
      tag: String(value(node, "kind", "Kind") || "").replaceAll("_", " "),
      kind: value(node, "kind", "Kind"),
      state: "future",
      outcome: "",
      duration: "",
      jobs: [],
      loop: "",
    });
  }
  return rows;
}

// loopBackLabel names the moment a run went backwards, so the reason for a
// repeat visit sits under the row that caused it.
function loopBackLabel(nodeRuns, index, nodes) {
  const next = nodeRuns[index + 1];
  if (!next) return "";
  const currentKey = value(nodeRuns[index], "node_key", "NodeKey");
  const nextKey = value(next, "node_key", "NodeKey");
  const order = nodes.map((node) => value(node, "key", "Key"));
  const from = order.indexOf(currentKey);
  const to = order.indexOf(nextKey);
  if (from < 0 || to < 0 || to >= from) return "";
  const target = nodes[to];
  const visit = Number(value(next, "visit", "Visit") || 1);
  return `↺ looped back to ${value(target, "name", "Name") || nextKey}${visit > 1 ? ` · ×${visit - 1}` : ""}`;
}
