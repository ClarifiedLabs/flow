// Worker/job/queue table rendering plus the taint formatter (which depends on
// the value() key reader).

import { renderStateBadge } from "./board.js";
import { formatDate, formatLabels } from "./format.js";
import { issueHref } from "./api.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { renderTerminalButton, renderTranscriptButton } from "./terminal.js";

export function renderQueueSummary(queue) {
  const queued = Number(value(queue, "queued", "Queued") || 0);
  const persistent = Number(value(queue, "persistent_agent", "PersistentAgent") || 0);
  const ephemeral = Number(value(queue, "ephemeral", "Ephemeral") || 0);
  const author = Number(value(queue, "author", "Author") || 0);
  const reviewer = Number(value(queue, "reviewer", "Reviewer") || 0);
  const verifier = Number(value(queue, "verifier", "Verifier") || 0);
  const ci = Number(value(queue, "ci", "CI") || 0);
  return `
    <div class="summary-strip">
      <span>queued ${queued}</span>
      <span>persistent ${persistent}</span>
      <span>ephemeral ${ephemeral}</span>
      <span>author ${author}</span>
      <span>reviewer ${reviewer}</span>
      <span>verifier ${verifier}</span>
      <span>ci ${ci}</span>
    </div>
  `;
}

export function renderWorkerRow(worker, diagnostics = {}) {
  const liveJobs = Number(value(diagnostics, "live_jobs", "LiveJobs") || 0);
  const livePersistent = Number(value(diagnostics, "live_persistent_agent", "LivePersistentAgent") || 0);
  const liveEphemeral = Number(value(diagnostics, "live_ephemeral", "LiveEphemeral") || 0);
  const expiredJobs = Number(value(diagnostics, "expired_unreleased_jobs", "ExpiredUnreleasedJobs") || 0);
  const expiredPersistent = Number(value(diagnostics, "expired_unreleased_persistent_agent", "ExpiredUnreleasedPersistentAgent") || 0);
  const expiredEphemeral = Number(value(diagnostics, "expired_unreleased_ephemeral", "ExpiredUnreleasedEphemeral") || 0);
  const heldText = expiredJobs ? ` · expired ${expiredJobs} · held ${expiredPersistent}/${expiredEphemeral}` : "";
  return `
    <tr>
      <td>${escapeHTML(value(worker, "id", "ID"))}</td>
      <td>${renderStateBadge(value(worker, "status", "Status"))}</td>
      <td>${Number(value(worker, "capacity_persistent_agent", "CapacityPersistentAgent") || 0)} / ${Number(value(worker, "capacity_ephemeral", "CapacityEphemeral") || 0)}</td>
      <td>${liveJobs} jobs · ${livePersistent}/${liveEphemeral}${escapeHTML(heldText)}</td>
      <td>${escapeHTML(formatLabels(value(worker, "labels", "Labels")))}</td>
      <td>${escapeHTML(formatTaints(value(worker, "taints", "Taints")))}</td>
      <td>${escapeHTML(formatDate(value(worker, "last_heartbeat_at", "LastHeartbeatAt", "last_seen_at", "LastSeenAt")))}</td>
    </tr>
  `;
}

// jobStateClass maps a worker job state to the badge/tint class used elsewhere
// in the web UI (ok=green, danger=red, run=yellow/orange, warn=amber, idle=grey).
export function jobStateClass(state) {
  const normalized = String(state || "").toLowerCase();
  if (["finished"].includes(normalized)) return "ok";
  if (["failed", "crashed"].includes(normalized)) return "danger";
  if (["canceled"].includes(normalized)) return "warn";
  if (["running"].includes(normalized)) return "run";
  return "idle";
}

export function renderJobRow(job, diagnostics = {}) {
  const jobID = value(job, "id", "ID");
  const issueID = value(job, "issue_id", "IssueID");
  const projectID = value(diagnostics, "project_id", "ProjectID");
  const projectName = value(diagnostics, "project_name", "ProjectName");
  const change = value(diagnostics, "change", "Change");
  const changeID = value(change, "id", "ID") || value(job, "change_id", "ChangeID");
  const lease = value(diagnostics, "lease", "Lease");
  const session = value(diagnostics, "session", "Session");
  const leaseStatus = value(diagnostics, "lease_status", "LeaseStatus") || (Boolean(value(diagnostics, "live_lease", "LiveLease")) ? "live" : "released");
  const tmuxSession = value(diagnostics, "tmux_session", "TmuxSession");
  const workerID = value(lease, "worker_id", "WorkerID");
  const leaseID = value(lease, "id", "ID");
  const sessionState = value(session, "state", "State");
  const sessionID = value(session, "id", "ID");
  const sessionTerminalAvailable = Boolean(value(session, "terminal_available", "TerminalAvailable"));
  const jobTerminalAvailable = Boolean(value(diagnostics, "terminal_available", "TerminalAvailable"));
  const terminalButton = sessionID && sessionTerminalAvailable
    ? renderTerminalButton("session", sessionID)
    : jobID && jobTerminalAvailable
      ? renderTerminalButton("job", jobID)
    : "";
  const attachButton = jobID && tmuxSession
    ? `<button class="button secondary" data-job-attach="${escapeAttr(jobID)}">Attach</button>`
    : "";
  const sessionTranscriptAvailable = Boolean(value(session, "transcript_available", "TranscriptAvailable"));
  const jobTranscriptAvailable = Boolean(value(diagnostics, "transcript_available", "TranscriptAvailable"));
  const transcriptButton = sessionID && sessionTranscriptAvailable
    ? renderTranscriptButton("session", sessionID)
    : jobID && jobTranscriptAvailable
      ? renderTranscriptButton("job", jobID)
    : "";
  const tmuxCell = [
    tmuxSession ? escapeHTML(tmuxSession) : "",
    terminalButton || attachButton || transcriptButton ? `<div class="actions table-actions">${terminalButton}${attachButton}${transcriptButton}</div>` : "",
  ].filter(Boolean).join("");
  const target = [
    issueID ? `<a href="${escapeAttr(issueHref(projectID, issueID))}" data-link>${escapeHTML(issueID)}</a>` : "",
    changeID ? `<a href="/ui/changes/${escapeAttr(changeID)}" data-link>${escapeHTML(changeID)}</a>` : "",
  ].filter(Boolean).join("<br>");
  const stateClass = jobStateClass(value(job, "state", "State"));
  return `
    <tr class="row-${stateClass}">
      <td>${escapeHTML(jobID)}</td>
      <td>${renderStateBadge(value(job, "state", "State"))}</td>
      <td>${escapeHTML(projectName || "")}</td>
      <td>${escapeHTML(value(job, "role", "Role"))}<br><span class="muted">${escapeHTML(value(job, "capacity_bucket", "CapacityBucket"))}</span></td>
      <td>${target || ""}</td>
      <td>${escapeHTML(workerID || "")}${sessionState ? `<br><span class="muted">${escapeHTML(sessionState)}</span>` : ""}</td>
      <td>${escapeHTML(leaseID || "")}${leaseID ? `<br><span class="muted">${escapeHTML(leaseStatus)}</span>` : ""}</td>
      <td>${tmuxCell}</td>
      <td>${escapeHTML(formatDate(value(job, "updated_at", "UpdatedAt")))}</td>
    </tr>
  `;
}

export function formatTaints(taints) {
  if (!Array.isArray(taints) || taints.length === 0) return "";
  return taints.map((taint) => `${value(taint, "key", "Key")}=${value(taint, "value", "Value")}:${value(taint, "effect", "Effect")}`).sort().join(", ");
}
