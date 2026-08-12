// Review tab projection: the open human gate or agent question, the
// artifact under review, the discussion so far, and the live agent session
// when the review is interactive.

import { value } from "../normalize.js";

// parseWaitDetails accepts only the complete immutable human-gate contract.
// A missing or malformed payload is deliberately not reconstructed from the
// current graph: it is not safe to answer a wait whose frozen details are gone.
export function parseWaitDetails(wait) {
  let details = value(wait || {}, "details", "Details");
  if (typeof details === "string") {
    try {
      details = JSON.parse(details);
    } catch {
      return null;
    }
  }
  if (!details || typeof details !== "object" || Array.isArray(details)) return null;
  const allowed = new Set(["instructions", "outcomes", "artifact_id", "interactive", "gate_node_key"]);
  if (Object.keys(details).some((key) => !allowed.has(key))) return null;
  if (typeof details.interactive !== "boolean") return null;
  const gateNodeKey = String(details.gate_node_key || "").trim();
  if (!gateNodeKey || !Array.isArray(details.outcomes) || details.outcomes.length === 0) return null;
  const outcomes = details.outcomes.map((outcome) => String(outcome || "").trim());
  if (outcomes.some((outcome) => !outcome) || new Set(outcomes).size !== outcomes.length) return null;
  const artifactID = String(details.artifact_id || "").trim();
  if (details.interactive && !artifactID) return null;
  return {
    instructions: typeof details.instructions === "string" ? details.instructions : "",
    outcomes,
    artifact_id: artifactID,
    interactive: details.interactive,
    gate_node_key: gateNodeKey,
  };
}

// reviewModel is the Review tab's projection: the open human gate or agent
// question, the artifact under review, the discussion so far, and the live
// agent session when the review is interactive. Null when there is nothing
// to review and nothing under review.
export function reviewModel({ wait, currentNode, run, artifacts, statusLog, activeSession }) {
  const waitKind = String(value(wait, "kind", "Kind") || "");
  const details = parseWaitDetails(wait);

  let gate = null;
  if (waitKind === "human_gate" && details) {
    const artifactID = details.artifact_id;
    const artifact = artifacts.find((candidate) => String(value(candidate, "id", "ID")) === artifactID) || null;
    const nodeRunID = String(value(wait, "node_run_id", "NodeRunID") || "").trim();
    const waitID = String(value(wait, "id", "ID") || "").trim();
    if (nodeRunID && waitID) {
      gate = {
        nodeRunID,
        waitID,
        heading: String(value(wait, "message", "Message") || "Review"),
        instructions: details.instructions || String(value(wait, "message", "Message") || ""),
        outcomes: details.outcomes,
        interactive: details.interactive,
        artifactID,
        changeGate: String(value(artifact || {}, "kind", "Kind")) === "change",
      };
    }
  }

  let question = null;
  if (waitKind === "agent_request") {
    const entry = (statusLog || []).find((candidate) => value(candidate, "kind", "Kind") === "question") || {};
    question = {
      message: String(value(wait, "message", "Message") || ""),
      statusLogID: value(entry, "id", "ID") || "",
    };
  }

  let artifact = null;
  const artifactID = gate?.artifactID || String(value(run, "current_artifact_id", "CurrentArtifactID") || "");
  const found =
    artifacts.find((candidate) => String(value(candidate, "id", "ID")) === artifactID) ||
    [...artifacts].reverse().find((candidate) => String(value(candidate, "kind", "Kind")) === "task_set") ||
    null;
  if (found) {
    let manifest = null;
    let payload = value(found, "payload", "Payload");
    if (typeof payload === "string") {
      try {
        payload = JSON.parse(payload);
      } catch {
        payload = null;
      }
    }
    if (String(value(found, "kind", "Kind")) === "task_set" && payload && Array.isArray(payload.tasks)) manifest = payload;
    artifact = {
      id: String(value(found, "id", "ID") || ""),
      kind: String(value(found, "kind", "Kind") || ""),
      summary: String(value(found, "summary_markdown", "SummaryMarkdown") || ""),
      manifest,
      createdAt: value(found, "created_at", "CreatedAt"),
    };
  }

  const waitSince = new Date(value(wait, "created_at", "CreatedAt") || 0).getTime();
  const comments = (statusLog || [])
    .filter((entry) => {
      if (!wait) return false;
      const at = new Date(value(entry, "created_at", "CreatedAt") || 0).getTime();
      return Number.isFinite(at) && at >= waitSince;
    })
    .slice(-20)
    .map((entry) => ({
      id: value(entry, "id", "ID"),
      actor: String(value(entry, "actor", "Actor") || ""),
      kind: String(value(entry, "kind", "Kind") || "note"),
      message: String(value(entry, "message", "Message") || ""),
      createdAt: value(entry, "created_at", "CreatedAt"),
    }));

  let session = null;
  if (gate?.interactive && activeSession) {
    const sessionID = String(value(activeSession, "id", "ID") || "");
    if (sessionID) {
      session = {
        id: sessionID,
        state: String(value(activeSession, "state", "State") || ""),
        terminalAvailable: Boolean(value(activeSession, "terminal_available", "TerminalAvailable")),
      };
    }
  }

  if (!gate && !question && !artifact) return null;
  return { gate, question, artifact, comments, session };
}
