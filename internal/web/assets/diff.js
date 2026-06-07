// Review thread, check/verdict and diff (file/hunk/line) rendering.

import { formatDate, shortSHA } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { projectButtonAttr } from "./issue.js";
import { renderMarkdown } from "./markdown.js";
import { value } from "./normalize.js";
import { DIFF_MODES } from "./config.js";

export function renderThread(thread, changeHeadSHA) {
  const threadID = value(thread, "id", "ID");
  const state = value(thread, "state", "State");
  const filePath = value(thread, "file_path", "FilePath");
  const line = value(thread, "line", "Line");
  const anchor = value(thread, "anchor_commit_sha", "AnchorCommitSHA");
  const claimKind = value(thread, "claim_kind", "ClaimKind");
  const claimCommitSHA = value(thread, "claim_commit_sha", "ClaimCommitSHA");
  const context = value(thread, "context", "Context");
  const comments = value(thread, "comments", "Comments") || [];
  const location = `${filePath}:${Number(line || 0)}`;
  const anchorText = anchor ? ` @ ${shortSHA(anchor)}` : "";
  const outdatedAnchor = Boolean(anchor && changeHeadSHA && anchor !== changeHeadSHA);
  const claimActions = threadID && (state === "open" || state === "reopened") ? `
    <div class="actions">
      <button class="button secondary" data-thread-claim="${escapeAttr(threadID)}" data-claim-kind="fixed" data-claim-commit="${escapeAttr(changeHeadSHA || "")}">Fixed</button>
      <button class="button secondary" data-thread-claim="${escapeAttr(threadID)}" data-claim-kind="not_warranted">Not warranted</button>
      <button class="button secondary" data-thread-claim="${escapeAttr(threadID)}" data-claim-kind="superseded">Superseded</button>
    </div>
  ` : "";
  const replyAction = threadID ? `<div class="actions"><button class="button secondary" data-thread-reply="${escapeAttr(threadID)}">Reply</button></div>` : "";
  return `
    <article class="feed-item">
      <strong>${escapeHTML(state)}</strong><span>${escapeHTML(location + anchorText)}</span>
      ${outdatedAnchor ? `<span class="badge warn">outdated anchor</span>` : ""}
      ${claimKind ? `<span class="badge idle">claim ${escapeHTML(claimKind)}</span>` : ""}
      ${claimCommitSHA ? `<span class="badge idle">claim head ${escapeHTML(shortSHA(claimCommitSHA))}</span>` : ""}
      ${context ? `<pre class="thread-context">${escapeHTML(context)}</pre>` : ""}
      ${comments.length ? `<div class="feed thread-comments">${comments.map(renderThreadComment).join("")}</div>` : ""}
      ${claimActions}
      ${replyAction}
    </article>
  `;
}

export function renderThreadComment(comment) {
  return `
    <article class="feed-item">
      <strong>${escapeHTML(value(comment, "actor", "Actor"))}</strong>
      <span>${escapeHTML(formatDate(value(comment, "created_at", "CreatedAt")))}</span>
      ${renderMarkdown(value(comment, "body", "Body"))}
    </article>
  `;
}

export function renderCheck(check, issueID = "", projectID = "") {
  const name = value(check, "name", "Name");
  const verdict = value(check, "verdict", "Verdict");
  const kind = value(check, "kind", "Kind");
  const required = value(check, "required", "Required");
  const details = value(check, "details", "Details");
  const checkIssueID = issueID || value(check, "issue_id", "IssueID");
  const approveAction = canApproveHumanReview(check, checkIssueID)
    ? renderHumanReviewApproveButton(check, checkIssueID, projectID, "button check-action")
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

export function renderHumanReviewApproveButton(check, issueID, projectID = "", classes = "button secondary") {
  const name = value(check, "name", "Name");
  return `<button class="${escapeAttr(classes)}" data-human-review-approve="${escapeAttr(issueID)}" data-check-name="${escapeAttr(name)}"${projectButtonAttr(projectID)}>Approve</button>`;
}

export function renderVerdictBadge(verdict) {
  const raw = String(verdict || "");
  const cls = raw === "satisfied"
    ? "ok"
    : ["blocked", "failed", "rejected"].includes(raw)
      ? "danger"
      : "idle";
  return `<span class="badge ${cls}">${escapeHTML(raw.replaceAll("_", " ") || "pending")}</span>`;
}

export function canApproveHumanReview(check, issueID) {
  const name = value(check, "name", "Name");
  const kind = value(check, "kind", "Kind");
  const required = Boolean(value(check, "required", "Required"));
  const verdict = value(check, "verdict", "Verdict");
  return Boolean(issueID)
    && name === "human-review"
    && kind === "human"
    && required
    && verdict !== "satisfied";
}

export function renderDiffSummary(diff, mode = "unified") {
  const available = Boolean(value(diff, "available", "Available"));
  if (!available) {
    return `<div class="empty">${escapeHTML(value(diff, "unavailable_reason", "UnavailableReason") || "Diff unavailable")}</div>`;
  }
  const files = value(diff, "files", "Files") || [];
  const additions = Number(value(diff, "additions", "Additions") || 0);
  const deletions = Number(value(diff, "deletions", "Deletions") || 0);
  const normalizedMode = DIFF_MODES.has(mode) ? mode : "unified";
  const widthChars = diffFilesWidthChars(files, normalizedMode);
  return `
    <div class="diff-mode-bar" data-diff-mode-toggle>
      <button type="button" class="button secondary" data-diff-mode="unified" aria-pressed="${normalizedMode === "unified"}">Unified</button>
      <button type="button" class="button secondary" data-diff-mode="split" aria-pressed="${normalizedMode === "split"}">Split</button>
    </div>
    <div class="summary-strip">
      <span>files ${Number(value(diff, "total_files", "TotalFiles") || files.length)}</span>
      <span>+${additions}</span>
      <span>-${deletions}</span>
    </div>
    <div class="table-wrap compact">
      <table>
        <thead><tr><th>File</th><th>Add</th><th>Del</th></tr></thead>
        <tbody>${files.map(renderDiffFileRow).join("") || `<tr><td colspan="3">No file changes</td></tr>`}</tbody>
      </table>
    </div>
    <div class="diff-content" data-diff-mode="${escapeAttr(normalizedMode)}">${files.map((file) => renderDiffFileHunks(file, normalizedMode, widthChars)).join("")}</div>
  `;
}

export function renderDiffFileRow(file) {
  const binary = Boolean(value(file, "binary", "Binary"));
  return `
    <tr>
      <td>${escapeHTML(value(file, "path", "Path"))}</td>
      <td>${binary ? "binary" : Number(value(file, "additions", "Additions") || 0)}</td>
      <td>${binary ? "binary" : Number(value(file, "deletions", "Deletions") || 0)}</td>
    </tr>
  `;
}

export function renderDiffFileHunks(file, mode = "unified", widthChars = null) {
  const hunks = value(file, "hunks", "Hunks") || [];
  if (!hunks.length) return "";
  const normalizedMode = DIFF_MODES.has(mode) ? mode : "unified";
  const resolvedWidthChars = widthChars ?? (normalizedMode === "split"
    ? diffFileSplitWidthChars(hunks)
    : diffFileUnifiedWidthChars(hunks));
  return `
    <article class="feed-item diff-file">
      <strong>${escapeHTML(value(file, "path", "Path"))}</strong>
      ${hunks.map((hunk) => renderDiffHunk(hunk, normalizedMode, resolvedWidthChars)).join("")}
    </article>
  `;
}

export function renderDiffHunk(hunk, mode = "unified", widthChars = null) {
  if (mode === "split") return renderDiffHunkSplit(hunk, widthChars);
  const header = value(hunk, "header", "Header");
  const lines = value(hunk, "lines", "Lines") || [];
  const renderedLines = [
    header ? `<span class="diff-unified-line diff-hunk-header">${escapeHTML(header)}</span>` : "",
    ...lines.map(renderDiffLine),
  ].filter(Boolean);
  return `<pre class="diff-unified"${diffWidthStyle("--diff-unified-width", widthChars ?? diffUnifiedWidthChars(hunk))}>${renderedLines.join("")}</pre>`;
}

function renderDiffHunkSplit(hunk, widthChars = null) {
  const header = value(hunk, "header", "Header");
  const lines = value(hunk, "lines", "Lines") || [];
  const rows = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const kind = value(line, "kind", "Kind");
    if (kind === "meta") {
      rows.push(renderDiffSplitMetaRow(line));
      i += 1;
      continue;
    }
    if (kind === "context") {
      rows.push(renderDiffSplitContextRow(line));
      i += 1;
      continue;
    }
    if (kind === "delete") {
      const deletes = [];
      while (i < lines.length && value(lines[i], "kind", "Kind") === "delete") {
        deletes.push(lines[i]);
        i += 1;
      }
      const adds = [];
      while (i < lines.length && value(lines[i], "kind", "Kind") === "add") {
        adds.push(lines[i]);
 i += 1;
      }
      const pairs = Math.min(deletes.length, adds.length);
      for (let p = 0; p < pairs; p++) rows.push(renderDiffSplitPairRow(deletes[p], adds[p]));
      for (let p = pairs; p < deletes.length; p++) rows.push(renderDiffSplitDeleteRow(deletes[p]));
      for (let p = pairs; p < adds.length; p++) rows.push(renderDiffSplitAddRow(adds[p]));
      continue;
    }
    if (kind === "add") {
      const adds = [];
      while (i < lines.length && value(lines[i], "kind", "Kind") === "add") {
        adds.push(lines[i]);
        i += 1;
      }
      for (const add of adds) rows.push(renderDiffSplitAddRow(add));
      continue;
    }
    rows.push(renderDiffSplitContextRow(line));
    i += 1;
  }
  return `<div class="diff-split-wrap"><table class="diff-split"${diffWidthStyle("--diff-split-width", widthChars ?? diffSplitWidthChars(hunk))}><thead><tr><th class="diff-col-old">Old</th><th class="diff-col-new">New</th></tr></thead><tbody>${header ? `<tr class="diff-hunk-header"><td colspan="2">${escapeHTML(header)}</td></tr>` : ""}${rows.join("")}</tbody></table></div>`;
}

function diffWidthStyle(name, chars) {
  const width = Math.max(1, Math.ceil(Number(chars) || 1));
  return ` style="${name}: ${width}ch;"`;
}

function diffFilesWidthChars(files, mode) {
  const widthForHunks = mode === "split" ? diffFileSplitWidthChars : diffFileUnifiedWidthChars;
  return files.reduce((max, file) => {
    const hunks = value(file, "hunks", "Hunks") || [];
    if (!hunks.length) return max;
    return Math.max(max, widthForHunks(hunks));
  }, 1);
}

function diffFileUnifiedWidthChars(hunks) {
  return hunks.reduce((max, hunk) => Math.max(max, diffUnifiedWidthChars(hunk)), 1);
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

function diffSplitWidthChars(hunk) {
  const header = value(hunk, "header", "Header");
  const lines = value(hunk, "lines", "Lines") || [];
  let maxSideText = 0;
  let maxFullRowText = textLength(header);
  for (const line of lines) {
    const kind = value(line, "kind", "Kind");
    const length = textLength(value(line, "text", "Text"));
    if (kind === "meta") {
      maxFullRowText = Math.max(maxFullRowText, length);
    } else {
      maxSideText = Math.max(maxSideText, length);
    }
  }
  // Split mode uses fixed-width columns; size both sides for the longest line so
  // either column can show that line without wrapping when space is available.
  return Math.max(maxFullRowText, (maxSideText + 4) * 2, 1);
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

export function renderDiffLine(line) {
  const kind = value(line, "kind", "Kind");
  const prefix = kind === "add" ? "+" : kind === "delete" ? "-" : kind === "meta" ? "" : " ";
  const cls = kind === "add" ? " diff-add" : kind === "delete" ? " diff-del" : kind === "meta" ? " diff-meta" : "";
  return `<span class="diff-unified-line${cls}">${escapeHTML(`${prefix}${value(line, "text", "Text")}`)}</span>`;
}
