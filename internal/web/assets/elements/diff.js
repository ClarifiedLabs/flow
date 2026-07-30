// The diff pane, and the two changes that make review work.
//
// First: the gutter mark is a button, and it is the add-a-comment affordance.
// Reviewing without a way to point at a line is the whole reason the old page
// only ever collected one comment.
//
// Second: long lines wrap with a hanging indent instead of scrolling
// sideways. On the one screen whose job is reading changed lines, nothing
// should be off-screen — and wrapping keeps inline threads aligned to the pane.
//
// Diff lines are plain markup, not elements. A three-thousand-line change must
// not become three thousand custom elements; only the thread anchors are
// promoted.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";
import "./inline-thread.js";

export function renderDiffFile(file, { threads = [], drafts = new Map() } = {}) {
  if (!file) return `<p class="empty">Select a file</p>`;
  if (value(file, "binary", "Binary")) return `<p class="empty">Binary file</p>`;
  const hunks = value(file, "hunks", "Hunks") || [];
  if (!hunks.length) return `<p class="empty">No hunks</p>`;
  const path = value(file, "path", "Path");
  const byLine = threadsByLine(threads, path);
  return hunks.map((hunk) => renderHunk(hunk, { path, byLine, drafts })).join("");
}

function threadsByLine(threads, path) {
  const byLine = new Map();
  for (const thread of threads) {
    if (value(thread, "file_path", "FilePath") !== path) continue;
    const line = Number(value(thread, "line", "Line") || 0);
    if (!byLine.has(line)) byLine.set(line, []);
    byLine.get(line).push(thread);
  }
  return byLine;
}

function renderHunk(hunk, context) {
  const lines = value(hunk, "lines", "Lines") || [];
  return `
    <div class="hunk">
      <div class="hunk-head">${escapeHTML(value(hunk, "header", "Header") || "")}</div>
      ${lines.map((line) => renderLine(line, context)).join("")}
    </div>
  `;
}

function renderLine(line, { path, byLine, drafts }) {
  const kind = String(value(line, "kind", "Kind") || "context");
  const newLine = value(line, "new_line", "NewLine");
  const oldLine = value(line, "old_line", "OldLine");
  const number = newLine ?? oldLine ?? "";
  const mark = kind === "add" ? "+" : kind === "delete" ? "−" : " ";
  const draftKey = `${path}:${number}`;
  const draft = drafts.get(draftKey);
  const anchored = byLine.get(Number(number)) || [];

  return `
    <div class="line" data-kind="${escapeAttr(kind)}">
      <span class="lineno">${escapeHTML(String(number))}</span>
      <button class="gutter" data-comment-line="${escapeAttr(String(number))}" data-comment-path="${escapeAttr(path)}"
        title="Comment on line ${escapeAttr(String(number))}" aria-label="Comment on line ${escapeAttr(String(number))}"${draft ? " data-drafted" : ""}>${escapeHTML(mark)}</button>
      <code>${escapeHTML(value(line, "text", "Text") || "")}</code>
      ${draft ? `<span class="draft-note">comment on line ${escapeHTML(String(number))}</span>` : ""}
    </div>
    ${draft ? renderDraft(draftKey, draft) : ""}
    ${anchored.map((thread) => `<flow-inline-thread data-thread="${escapeAttr(value(thread, "id", "ID"))}"></flow-inline-thread>`).join("")}
  `;
}

function renderDraft(key, draft) {
  return `
    <div class="draft" data-draft="${escapeAttr(key)}">
      <textarea data-draft-body rows="2" placeholder="Leave a note on this line…">${escapeHTML(draft.body || "")}</textarea>
      <div class="draft-actions">
        <button class="button secondary" data-draft-cancel="${escapeAttr(key)}">Discard</button>
        <span class="draft-hint">posts with your review</span>
      </div>
    </div>
  `;
}

export class FlowDiff extends FlowElement {
  render(payload) {
    if (!payload) return "";
    return renderDiffFile(payload.file, { threads: payload.threads, drafts: payload.drafts });
  }

  afterPaint() {
    const threads = this.data?.threads || [];
    const byID = new Map(threads.map((thread) => [value(thread, "id", "ID"), thread]));
    for (const host of this.querySelectorAll("flow-inline-thread")) {
      host.data = { thread: byID.get(host.dataset.thread), change: this.data?.change };
    }
  }
}

define("flow-diff", FlowDiff);
