// Review thread, check/verdict and diff (file/hunk/line) rendering.

import { formatDate, shortSHA } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { projectButtonAttr } from "./task.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { DIFF_MODES } from "./config.js";


export function renderCheck(check, taskID = "", projectID = "", showHumanReviewAction = true) {
  const name = value(check, "name", "Name");
  const verdict = value(check, "verdict", "Verdict");
  const kind = value(check, "kind", "Kind");
  const required = value(check, "required", "Required");
  const details = value(check, "details", "Details");
  const checkTaskID = taskID || value(check, "task_id", "TaskID");
  const approveAction = showHumanReviewAction && canApproveHumanReview(check, checkTaskID)
    ? renderHumanReviewApproveButton(check, checkTaskID, projectID, "button check-action")
    : "";
  return `
    <article class="check-row">
      <div>
        <strong>${escapeHTML(name)}</strong>
        <span>${escapeHTML(kind)}${required ? " · required" : ""}</span>
      </div>
      <div class="check-state">
        ${renderVerdictBadge(verdict)}
        ${approveAction}
      </div>
      ${details ? renderMarkdown(details) : ""}
    </article>
  `;
}

export function renderHumanReviewApproveButton(check, taskID, projectID = "", classes = "button secondary") {
  const name = value(check, "name", "Name");
  return `<button class="${escapeAttr(classes)}" data-human-review-approve="${escapeAttr(taskID)}" data-check-name="${escapeAttr(name)}"${projectButtonAttr(projectID)}>Approve</button>`;
}

export function renderVerdictBadge(verdict) {
  const raw = String(verdict || "");
  const cls = raw === "satisfied"
    ? "ok"
    : ["blocked", "errored", "failed", "rejected"].includes(raw)
      ? "danger"
      : "idle";
  return `<span class="badge ${cls}">${escapeHTML(raw.replaceAll("_", " ") || "pending")}</span>`;
}

export function canApproveHumanReview(check, taskID) {
  const name = value(check, "name", "Name");
  const kind = value(check, "kind", "Kind");
  const required = Boolean(value(check, "required", "Required"));
  const verdict = value(check, "verdict", "Verdict");
  return Boolean(taskID)
    && name === "human-review"
    && kind === "human"
    && required
    && verdict !== "satisfied";
}


function diffWidthStyle(name, chars) {
  const width = Math.max(1, Math.ceil(Number(chars) || 1));
  return ` style="${name}: ${width}ch;"`;
}


function diffUnifiedWidthChars(hunk) {
  const header = value(hunk, "header", "Header");
  const lines = value(hunk, "lines", "Lines") || [];
  return Math.max(
    textLength(header),
    ...lines.map((line) => {
      const kind = value(line, "kind", "Kind");
      const prefix = kind === "add" || kind === "delete" ? 1 : kind === "meta" ? 0 : 1;
      return prefix + textLength(value(line, "text", "Text"));
    }),
    1,
  );
}

function diffFileSplitWidthChars(hunks) {
  return hunks.reduce((max, hunk) => Math.max(max, diffSplitWidthChars(hunk)), 1);
}


function textLength(text) {
  return String(text || "").length;
}

function diffSplitGutter(num) {
  if (num === "" || num === null || num === undefined) return "";
  return String(num);
}

function renderDiffSplitContextRow(line) {
  const oldNum = value(line, "old_line", "OldLine");
  const newNum = value(line, "new_line", "NewLine");
  const text = value(line, "text", "Text");
  return `<tr class="diff-row-context"><td class="diff-col-old"><span class="diff-line-num">${escapeHTML(diffSplitGutter(oldNum))}</span><span class="diff-text">${escapeHTML(text)}</span></td><td class="diff-col-new"><span class="diff-line-num">${escapeHTML(diffSplitGutter(newNum))}</span><span class="diff-text">${escapeHTML(text)}</span></td></tr>`;
}

function renderDiffSplitDeleteRow(line) {
  const oldNum = value(line, "old_line", "OldLine");
  const text = value(line, "text", "Text");
  return `<tr class="diff-row-delete"><td class="diff-col-old"><span class="diff-line-num">${escapeHTML(diffSplitGutter(oldNum))}</span><span class="diff-text diff-del">${escapeHTML(text)}</span></td><td class="diff-col-new empty"><span class="diff-line-num"> </span><span class="diff-text"></span></td></tr>`;
}

function renderDiffSplitAddRow(line) {
  const newNum = value(line, "new_line", "NewLine");
  const text = value(line, "text", "Text");
  return `<tr class="diff-row-add"><td class="diff-col-old empty"><span class="diff-line-num"> </span><span class="diff-text"></span></td><td class="diff-col-new"><span class="diff-line-num">${escapeHTML(diffSplitGutter(newNum))}</span><span class="diff-text diff-add">${escapeHTML(text)}</span></td></tr>`;
}

function renderDiffSplitPairRow(deleteLine, addLine) {
  const oldNum = value(deleteLine, "old_line", "OldLine");
  const oldText = value(deleteLine, "text", "Text");
  const newNum = value(addLine, "new_line", "NewLine");
  const newText = value(addLine, "text", "Text");
  return `<tr class="diff-row-pair"><td class="diff-col-old"><span class="diff-line-num">${escapeHTML(diffSplitGutter(oldNum))}</span><span class="diff-text diff-del">${escapeHTML(oldText)}</span></td><td class="diff-col-new"><span class="diff-line-num">${escapeHTML(diffSplitGutter(newNum))}</span><span class="diff-text diff-add">${escapeHTML(newText)}</span></td></tr>`;
}

function renderDiffSplitMetaRow(line) {
  const text = value(line, "text", "Text");
  return `<tr class="diff-row-meta"><td colspan="2"><span class="diff-text diff-meta">${escapeHTML(text)}</span></td></tr>`;
}

