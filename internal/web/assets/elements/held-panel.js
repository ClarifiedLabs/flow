// Held work. Manual intervention and convergence decisions are first-class
// lifecycle states: the run has stopped and handing it back is an explicit
// choice of which edge to take.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { TERMINAL_ICON } from "../terminal.js";
import { define, FlowElement } from "./base.js";

// HAND_BACK_EDGES map one-for-one onto executor actions, so the button says
// what will happen rather than something vague like "continue".
export const HAND_BACK_EDGES = [
  ["resume", "Resume at", true],
  ["submit", "Send to review", false],
  ["satisfy", "Mark step done", false],
  ["merge", "Skip to merge", false],
];

export function renderHeldPanel(model) {
  if (!model?.held) return "";
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const session = value(model.taskConsole || {}, "session", "Session");
  const sessionID = value(session || {}, "id", "ID");
  const workerID = value(session || {}, "worker_id", "WorkerID");
  const convergenceEvidence = model.convergenceEvidence || null;
  if (convergenceEvidence) {
    return renderConvergencePanel(model, convergenceEvidence, projectAttr, sessionID, workerID);
  }

  return `
    <div class="head">
      <span class="badge"><span class="dot"></span>Held by you</span>
      <span class="line">paused at ${escapeHTML(model.stepName)} · the workflow will not advance</span>
    </div>
    ${sessionID ? renderSession(sessionID, workerID) : ""}
    <div class="hand-back">
      <span class="caption">Hand back</span>
      ${HAND_BACK_EDGES.map(([edge, label, primary]) => {
        const text = edge === "resume" ? `${label} ${model.stepName}` : label;
        return `<button class="button${primary ? "" : " secondary"}" data-workflow-release="${escapeAttr(model.id)}" data-edge="${escapeAttr(edge)}"${projectAttr}>${escapeHTML(text)}</button>`;
      }).join("")}
    </div>
  `;
}

function renderConvergencePanel(model, evidence, projectAttr, sessionID, workerID) {
  const files = Number(value(evidence, "files", "Files") || 0);
  const additions = Number(value(evidence, "additions", "Additions") || 0);
  const deletions = Number(value(evidence, "deletions", "Deletions") || 0);
  const changedFiles = value(evidence, "changed_files", "ChangedFiles") || [];
  const omitted = Number(value(evidence, "changed_files_omitted", "ChangedFilesOmitted") || 0);
  const sourceBranch = String(value(evidence, "source_branch", "SourceBranch") || "");
  const sourceSHA = String(value(evidence, "source_head_sha", "SourceHeadSHA") || "");
  const baseBranch = String(value(evidence, "target_base_branch", "TargetBaseBranch") || "");
  const baseSHA = String(value(evidence, "target_base_tip_sha", "TargetBaseTipSHA") || "");
  const reviewUsed = Number(value(evidence, "review_cycles_used", "ReviewCyclesUsed") || 0);
  const reviewBudget = Number(value(evidence, "review_cycle_budget", "ReviewCycleBudget") || 0);
  const fingerprint = String(value(evidence, "fingerprint", "Fingerprint") || "");
  const fingerprintAttr = ` data-evidence-fingerprint="${escapeAttr(fingerprint)}"`;

  return `
    <div class="head">
      <span class="badge"><span class="dot"></span>Convergence review</span>
      <span class="line">paused at ${escapeHTML(model.stepName)} · choose the source implementation's disposition</span>
    </div>
    <div class="evidence-summary">
      <div><span>Source</span><code>${escapeHTML(sourceBranch)}@${escapeHTML(shortSHA(sourceSHA))}</code></div>
      <div><span>Clean base</span><code>${escapeHTML(baseBranch)}@${escapeHTML(shortSHA(baseSHA))}</code></div>
      <div><span>Scope</span><strong>${files} files · +${additions}/-${deletions}</strong></div>
      <div><span>Review cycles</span><strong>${reviewUsed}/${reviewBudget}</strong></div>
    </div>
    ${changedFiles.length ? `<ul class="evidence-files">${changedFiles.map((file) => {
      const path = String(value(file, "path", "Path") || "");
      const added = Number(value(file, "additions", "Additions") || 0);
      const deleted = Number(value(file, "deletions", "Deletions") || 0);
      return `<li><code>${escapeHTML(path)}</code><span>+${added}/-${deleted}</span></li>`;
    }).join("")}${omitted ? `<li class="omitted">${omitted} more changed files retained by digest</li>` : ""}</ul>` : ""}
    ${sessionID ? renderSession(sessionID, workerID) : ""}
    <div class="hand-back convergence-actions" data-convergence-panel>
      <label class="convergence-note"><span>Decision note (optional)</span><textarea rows="2" data-convergence-note placeholder="Record why this disposition is appropriate"></textarea></label>
      <span class="caption">Disposition</span>
      <button class="button" data-convergence-decision="${escapeAttr(model.id)}" data-disposition="accept_scope"${fingerprintAttr}${projectAttr}>Continue as-is</button>
      <button class="button secondary" data-convergence-decision="${escapeAttr(model.id)}" data-disposition="repair_branch"${fingerprintAttr}${projectAttr}>Repair branch</button>
      <button class="button secondary" data-convergence-decision="${escapeAttr(model.id)}" data-disposition="promote"${fingerprintAttr}${projectAttr}>Promote to feature</button>
      <button class="button secondary danger" data-convergence-decision="${escapeAttr(model.id)}" data-disposition="cancel"${fingerprintAttr}${projectAttr}>Cancel implementation</button>
    </div>
  `;
}

function shortSHA(sha) {
  return sha ? sha.slice(0, 12) : "unknown";
}

function renderSession(sessionID, workerID) {
  return `
    <div class="session">
      <div class="chrome">
        ${TERMINAL_ICON}
        <span>${escapeHTML(sessionID)}${workerID ? ` · ${escapeHTML(workerID)}` : ""} · tmux</span>
        <span class="spacer"></span>
        <button class="pop-out" data-terminal-popout data-session="${escapeAttr(sessionID)}">Pop out</button>
      </div>
      <div class="frame" data-terminal="${escapeAttr(sessionID)}" data-inline-terminal-anchor></div>
    </div>
  `;
}

export class FlowHeldPanel extends FlowElement {
  render(model) {
    const html = renderHeldPanel(model);
    this.toggleAttribute("hidden", !html);
    return html;
  }
}

define("flow-held-panel", FlowHeldPanel);
