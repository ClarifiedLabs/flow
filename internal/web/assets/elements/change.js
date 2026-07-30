// Change review. One panel in three bands: header, body (file list + diff),
// review bar. The element owns the selected file and the reviewer's pending
// inline notes, so neither survives only as long as the next poll.

import { apiPost } from "../api.js";
import { beginInFlight, failureMessage, inFlight, markBusy, settleStatus } from "../actions.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { readDiffMode, writeDiffMode } from "../storage.js";
import { define, FlowElement } from "./base.js";
import "./diff.js";
import "./review-bar.js";

export function renderFileList(files, { selected, threads = [] }) {
  return files
    .map((file) => {
      const path = value(file, "path", "Path");
      const fileThreads = threads.filter((thread) => value(thread, "file_path", "FilePath") === path);
      const open = fileThreads.filter((thread) => value(thread, "state", "State") === "open").length;
      const badge = open
        ? `<span class="badge" data-tone="warn">${open}</span>`
        : fileThreads.length
          ? `<span class="badge" data-tone="ok">✓</span>`
          : "";
      return `
        <button class="file${path === selected ? " is-selected" : ""}" data-file="${escapeAttr(path)}">
          <span class="path">${escapeHTML(path)}</span>
          ${badge}
        </button>
      `;
    })
    .join("");
}

export class FlowChange extends FlowElement {
  selected = "";
  // Pending inline notes live here until the reviewer submits a verdict; that
  // is the difference between a comment box and a review.
  drafts = new Map();
  mode = readDiffMode();

  render(data) {
    if (!data) return `<div class="empty">Loading change</div>`;
    const change = value(data, "change", "Change") || {};
    const task = value(data, "task", "Task") || {};
    const diff = data.diff || {};
    const files = value(diff, "files", "Files") || [];
    const threads = value(data, "threads", "Threads") || [];
    if (!this.selected || !files.some((file) => value(file, "path", "Path") === this.selected)) {
      this.selected = value(files[0] || {}, "path", "Path") || "";
    }
    const reviewState = value(data, "review_state", "ReviewState");

    return `
      <div class="head">
        <span class="change-id">${escapeHTML(value(change, "id", "ID"))}</span>
        <a class="task-link" href="/ui/tasks/${escapeAttr(value(task, "id", "ID"))}" data-link>${escapeHTML(value(task, "id", "ID"))}</a>
        <span class="summary">${escapeHTML(summaryLine(change, diff))}</span>
        <span class="spacer"></span>
        ${reviewState ? `<span class="review-state" data-state="${escapeAttr(reviewState)}">${escapeHTML(String(reviewState).replaceAll("_", " "))}</span>` : ""}
        <div class="segmented" role="group" aria-label="Diff mode">
          <button data-diff-mode="unified"${this.mode === "unified" ? ' aria-pressed="true"' : ""}>Unified</button>
          <button data-diff-mode="split"${this.mode === "split" ? ' aria-pressed="true"' : ""}>Split</button>
        </div>
      </div>
      <div class="body">
        <div class="files">${renderFileList(files, { selected: this.selected, threads })}</div>
        <div class="pane"><flow-diff></flow-diff></div>
      </div>
      <flow-review-bar></flow-review-bar>
    `;
  }

  afterPaint() {
    const data = this.data;
    if (!data) return;
    const files = value(data.diff || {}, "files", "Files") || [];
    const file = files.find((candidate) => value(candidate, "path", "Path") === this.selected);
    const diff = this.querySelector("flow-diff");
    if (diff) {
      diff.data = {
        file,
        threads: value(data, "threads", "Threads") || [],
        change: value(data, "change", "Change") || {},
        drafts: this.drafts,
      };
    }
    const bar = this.querySelector("flow-review-bar");
    if (bar) bar.data = { pendingCount: this.drafts.size };
  }

  async handleClick(event) {
    const file = event.target.closest?.("[data-file]");
    if (file) {
      event.preventDefault();
      this.selected = file.dataset.file;
      this.invalidate();
      return;
    }
    const mode = event.target.closest?.("[data-diff-mode]");
    if (mode) {
      event.preventDefault();
      this.mode = mode.dataset.diffMode;
      writeDiffMode(this.mode);
      this.invalidate();
      return;
    }
    const gutter = event.target.closest?.("[data-comment-line]");
    if (gutter) {
      event.preventDefault();
      this.startDraft(gutter.dataset.commentPath, gutter.dataset.commentLine);
      return;
    }
    const cancel = event.target.closest?.("[data-draft-cancel]");
    if (cancel) {
      event.preventDefault();
      this.captureDrafts();
      this.drafts.delete(cancel.dataset.draftCancel);
      this.invalidate();
      return;
    }
    const verdict = event.target.closest?.("[data-review-verdict]");
    if (verdict) {
      event.preventDefault();
      await this.submitReview(verdict.dataset.reviewVerdict, verdict);
    }
  }

  startDraft(path, line) {
    this.captureDrafts();
    const key = `${path}:${line}`;
    if (!this.drafts.has(key)) this.drafts.set(key, { path, line: Number(line), body: "" });
    this.invalidate();
    queueMicrotask(() => this.querySelector(`[data-draft="${cssEscape(key)}"] textarea`)?.focus());
  }

  // Drafts live in the DOM between repaints, so read them back before any
  // re-render that would otherwise discard what was typed.
  captureDrafts() {
    for (const node of this.querySelectorAll("[data-draft]")) {
      const draft = this.drafts.get(node.dataset.draft);
      if (draft) draft.body = String(node.querySelector("[data-draft-body]")?.value || "");
    }
  }

  // submitReview gets the same pending state as the action buttons: the
  // verdict button is marked busy synchronously, the status line names the
  // in-flight submission, and the shared in-flight registry (keyed by change,
  // not by DOM node) blocks a duplicate verdict while the first is running —
  // even if a poll re-render replaced the button.
  async submitReview(verdict, button) {
    this.captureDrafts();
    const changeID = value(this.data?.change || {}, "id", "ID");
    if (!changeID) return;
    const comments = [...this.drafts.values()]
      .filter((draft) => draft.body.trim())
      .map((draft) => ({ file_path: draft.path, line: draft.line, body: draft.body.trim() }));
    const body = this.querySelector("flow-review-bar")?.body || "";
    if (!comments.length && !body && verdict === "comment") {
      this.app?.setStatus("Nothing to post");
      return;
    }
    const busyKey = `review:${changeID}:${verdict}`;
    if (inFlight.has(busyKey)) return;
    const restore = button ? markBusy(button) : () => {};
    const pending = `${reviewPendingLabel(verdict)}…`;
    beginInFlight(busyKey, pending);
    this.app?.setStatus(pending);
    try {
      await apiPost(`/v2/changes/${encodeURIComponent(changeID)}/review`, { verdict, body, comments });
      this.drafts.clear();
      await this.app?.refresh();
      // settleStatus keeps a still-pending sibling's label visible instead of
      // showing this verdict's result early.
      settleStatus(this.app, busyKey, reviewMessage(verdict, comments.length));
    } catch (error) {
      // failureMessage is total, so settleStatus always runs and the key always
      // drains — even for a non-Error rejection such as `reject(null)`.
      settleStatus(this.app, busyKey, failureMessage(error));
    } finally {
      restore();
    }
  }
}

function reviewPendingLabel(verdict) {
  if (verdict === "approve") return "Approving";
  if (verdict === "request_changes") return "Requesting changes";
  return "Posting comment";
}

function reviewMessage(verdict, count) {
  const notes = count ? ` with ${count} inline note${count === 1 ? "" : "s"}` : "";
  if (verdict === "approve") return `Approved${notes}`;
  if (verdict === "request_changes") return `Requested changes${notes}`;
  return `Posted ${count || 0} comment${count === 1 ? "" : "s"}`;
}

function summaryLine(change, diff) {
  const head = String(value(change, "head_sha", "HeadSHA") || "").slice(0, 12);
  const files = Number(value(diff, "total_files", "TotalFiles") || 0);
  const additions = Number(value(diff, "additions", "Additions") || 0);
  const deletions = Number(value(diff, "deletions", "Deletions") || 0);
  return [head, files ? `${files} file${files === 1 ? "" : "s"}` : "", `+${additions} −${deletions}`]
    .filter(Boolean)
    .join(" · ");
}

function cssEscape(text) {
  return String(text).replace(/["\\]/g, "\\$&");
}

define("flow-change", FlowChange);
