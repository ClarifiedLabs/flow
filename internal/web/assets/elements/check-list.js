// Checks, and the fix for non-local disclosure.
//
// The old page rendered a transcript in a panel far below the row you clicked,
// often off-screen, so it read as though nothing had happened. Here the
// transcript expands as the immediate next sibling of the row that owns it,
// and nothing renders further down the page. The same rule applies anywhere
// else output is revealed: job transcripts, agent verdicts, node artifacts.

import { apiGetText } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { failureMessage } from "../actions.js";
import { define, FlowElement } from "./base.js";

export function renderCheckList(model) {
  const checks = model?.checks || [];
  if (!checks.length) return `<p class="empty">No checks</p>`;
  return `
    <div class="rows">${checks.map((check) => renderCheckRow(check, model)).join("")}</div>
    <p class="note">The transcript expands in the row that owns it — never in a panel further down the page.</p>
  `;
}

function renderCheckRow(check, model) {
  const name = value(check, "name", "Name");
  const verdict = String(value(check, "verdict", "Verdict") || "pending");
  const jobID = value(check, "source_job_id", "SourceJobID") || "";
  const exitCode = value(check, "exit_code", "ExitCode");
  const failed = verdict === "blocked" || verdict === "errored";
  const details = String(value(check, "details", "Details") || "");
  const meta = [jobID, exitCode != null ? `exit ${exitCode}` : ""].filter(Boolean).join(" · ");
  const detail = [
    details ? renderMarkdown(details, { inline: true }) : "",
    escapeHTML(meta),
  ].filter(Boolean).join(" · ");
  const projectAttr = model?.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";

  return `
    <div class="row" data-check="${escapeAttr(name)}" data-verdict="${escapeAttr(verdict)}"${failed ? " data-failed" : ""}>
      <span class="dot"></span>
      <span class="name">${escapeHTML(name)}</span>
      <span class="detail">${detail}</span>
      <span class="spacer"></span>
      ${
        failed
          ? `
        <button class="button secondary" data-transcript-toggle="${escapeAttr(name)}" data-job="${escapeAttr(jobID)}">Transcript ▾</button>
        <button class="button secondary" data-workflow-retry="${escapeAttr(model?.id || "")}"${projectAttr}>Retry</button>
        <button class="button secondary" data-workflow-skip="${escapeAttr(model?.id || "")}" data-workflow-skip-node="${escapeAttr(model?.nodeRunID || "")}"${projectAttr}>Skip</button>
      `
          : `<span class="duration">${escapeHTML(checkDuration(check))}</span>`
      }
    </div>
  `;
}

function checkDuration(check) {
  const created = value(check, "created_at", "CreatedAt");
  const updated = value(check, "updated_at", "UpdatedAt");
  if (!created || !updated) return "";
  const seconds = Math.max(0, Math.round((new Date(updated).getTime() - new Date(created).getTime()) / 1000));
  if (!Number.isFinite(seconds) || !seconds) return "";
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export function renderTranscript(text) {
  const lines = String(text || "").split("\n").slice(-400);
  const body = lines
    .map((line) => {
      const tone = /(^|\s)(FAIL|ERROR|panic:)/.test(line) ? "fail" : /^(---|===|\s*RUN)/.test(line) ? "chrome" : "";
      return `<span class="line"${tone ? ` data-tone="${tone}"` : ""}>${escapeHTML(line)}</span>`;
    })
    .join("");
  return `<div class="transcript">${body}<span class="line" data-tone="chrome">…last 10MB stored · open full transcript</span></div>`;
}

export class FlowCheckList extends FlowElement {
  // Which transcripts are open is instance state, so a poll cannot collapse
  // one out from under the person reading it.
  open = new Set();

  render(model) {
    return renderCheckList(model);
  }

  afterPaint() {
    for (const name of this.open) this.paintTranscript(name);
  }

  async handleClick(event) {
    const toggle = event.target.closest?.("[data-transcript-toggle]");
    if (!toggle) return;
    event.preventDefault();
    const name = toggle.dataset.transcriptToggle;
    if (this.open.has(name)) {
      this.open.delete(name);
      this.querySelector(`[data-transcript-for="${cssEscape(name)}"]`)?.remove();
      toggle.textContent = "Transcript ▾";
      return;
    }
    this.open.add(name);
    toggle.textContent = "Transcript ▴";
    await this.paintTranscript(name, toggle.dataset.job);
  }

  async paintTranscript(name, jobID) {
    const row = this.querySelector(`[data-check="${cssEscape(name)}"]`);
    if (!row || row.nextElementSibling?.dataset?.transcriptFor === name) return;
    const job = jobID || row.querySelector("[data-transcript-toggle]")?.dataset.job || "";

    const panel = document.createElement("div");
    panel.dataset.transcriptFor = name;
    panel.className = "transcript-panel";
    panel.innerHTML = `<div class="transcript"><span class="line" data-tone="chrome">Loading transcript…</span></div>`;
    row.after(panel);

    if (!job) {
      panel.innerHTML = renderTranscript("No transcript was recorded for this check.");
      return;
    }
    try {
      const text = await apiGetText(`/v2/jobs/${encodeURIComponent(job)}/transcript`);
      panel.innerHTML = renderTranscript(text);
    } catch (error) {
      panel.innerHTML = renderTranscript(failureMessage(error));
    }
  }
}

function cssEscape(text) {
  return String(text).replace(/["\\]/g, "\\$&");
}

define("flow-check-list", FlowCheckList);
