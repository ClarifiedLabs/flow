// Change review. One panel in three bands: header, body (file list + diff),
// review bar. The element owns the selected file and the reviewer's pending
// inline notes, so neither survives only as long as the next poll.

import { apiPost } from "../api.js";
import { acquireBusy, inFlight, inFlightEntries, markBusy, releaseBusy } from "../actions/registry.js";
import { ACTION_SETTLE, failureMessage, settleStatus } from "../actions/dispatch.js";
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

// diffUnavailable reports whether a /diff response is not a usable diff: a
// failed fetch (null), or the server's explicit no-diff answer (HTTP 200
// naming the head it would diff, with available:false and an
// unavailable_reason). Shared with the task detail's change tab so both
// routes classify every /diff response shape identically.
export function diffUnavailable(diff) {
  return !diff || diff.available === false || Boolean(value(diff, "unavailable_reason", "UnavailableReason"));
}

export class FlowChange extends FlowElement {
  selected = "";
  // Pending inline notes live here until the reviewer submits a verdict; that
  // is the difference between a comment box and a review.
  drafts = new Map();
  // The head the pending drafts were written against. The standalone change
  // route reuses this element across polls, so drafts outlive re-renders by
  // design — but only for one head: render() clears them when the displayed
  // head changes, so notes composed against uninspected code can never ride
  // along in a submission that names a newer head.
  _draftHead = "";
  // The unsubmitted overall comment. It lives in the review bar's input
  // between repaints, but the input is not a [data-draft] node, so
  // captureDrafts cannot see it — captureReviewBody reads it back into this
  // field before any repaint, and the re-rendered bar restores it.
  _reviewBody = "";

  // The head whose code is on screen right now: the diff response names the
  // head the server rendered (what the reviewer actually saw), and the change
  // metadata must name the same head (both routes verify the pair before
  // rendering). Empty when the data names no head at all.
  get _displayedHead() {
    return String(value(this.data?.diff || {}, "head_sha", "HeadSHA") || value(this.data?.change || {}, "head_sha", "HeadSHA") || "");
  }

  // The head whose code is actually painted on screen right now. Setting
  // `data` only schedules the repaint on a microtask, so the model can advance
  // to a newer head while the rendered diff and review controls still show the
  // old one — and _displayedHead would already name the new head. A submission
  // must bind to what the reviewer saw, so it reads the painted head (the
  // data-head attribute render() stamped into the markup) and falls back to
  // the model's head only before the first paint, when nothing is on screen.
  get _paintedHead() {
    const node = this.querySelector(".head[data-head]");
    return node ? node.dataset.head : this._displayedHead;
  }
  mode = readDiffMode();

  render(data) {
    if (!data) return `<div class="empty">Loading change</div>`;
    const change = value(data, "change", "Change") || {};
    const task = value(data, "task", "Task") || {};
    const diff = data.diff || {};
    // Drafts are bound to the head they were written against, mirroring the
    // head submitReview names: the diff head (what the reviewer saw), falling
    // back to the change metadata head. A re-render that shows a different
    // head clears the drafts — the reviewer's notes were composed against
    // code that is no longer on screen, and the server would accept them
    // against the new head.
    const headSHA = this._displayedHead;
    if (headSHA && headSHA !== this._draftHead) {
      this.drafts.clear();
      this._draftHead = headSHA;
    }
    const files = value(diff, "files", "Files") || [];
    const threads = value(data, "threads", "Threads") || [];
    if (!this.selected || !files.some((file) => value(file, "path", "Path") === this.selected)) {
      this.selected = value(files[0] || {}, "path", "Path") || "";
    }
    const reviewState = value(data, "review_state", "ReviewState");
    // A diff with no head — or the server's explicit unavailable response
    // (HTTP 200 naming the head with available:false and an
    // unavailable_reason) — is a pending pair: the change has no head yet
    // (authoring in progress) or /diff was unavailable when the pair loaded.
    // Render the metadata with an explicit no-diff-yet state; the task detail
    // retries the diff on later polls and swaps this out for the real diff.
    // A headed unavailable response names the reason the server could not
    // compute the diff (e.g. "merge service is not configured"): surface it
    // so the empty state explains itself instead of reading as a change with
    // no files.
    const pending = !value(diff, "head_sha", "HeadSHA") || diffUnavailable(diff);
    const unavailableReason = value(diff, "unavailable_reason", "UnavailableReason");
    const pendingNote = unavailableReason && value(diff, "head_sha", "HeadSHA")
      ? `The diff is not available: ${unavailableReason}`
      : value(change, "head_sha", "HeadSHA")
        ? "The diff is not available yet; it will appear here once it is."
        : "No diff yet — this change has no head yet.";

    // The full displayed head rides in the markup so the paint identity (the
    // byte-compared render output in FlowElement.paint) moves whenever the head
    // does. The visible summary abbreviates the head to 12 characters, so two
    // heads sharing that prefix with the same file list and diff totals would
    // otherwise render byte-identical markup and skip the repaint — leaving the
    // old head's diff on screen while _displayedHead — and the submission —
    // name the new one.
    return `
      <div class="head" data-head="${escapeAttr(headSHA)}">
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
        ${pending
          ? `<div class="empty" data-change-pending>${escapeHTML(pendingNote)}</div>`
          : `<div class="files">${renderFileList(files, { selected: this.selected, threads })}</div>
             <div class="pane"><flow-diff></flow-diff></div>`}
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
    if (bar) bar.data = { pendingCount: this.drafts.size, body: this._reviewBody };
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

  // The change key (id:displayed-head) this element last painted. Drafts are
  // anchored to the rendered change's files and lines, so the key separates
  // the two repaint kinds that land on this same element:
  //   - the same key: a data-driven repaint (a metadata revalidation that
  //     flips the review state or a thread badge) replaces the draft editors,
  //     so the live textarea values must be read back into the drafts map
  //     before the write;
  //   - a different (or vanished) key: a genuine move. The standalone
  //     /ui/changes/:id route reuses this element via mount(), so the old
  //     head's drafts are dropped instead of captured (task-detail rebuilds
  //     the element on a head move, which has the same effect from a fresh
  //     drafts map). A response that names no head is not the change the
  //     drafts were anchored to, so it clears too.
  #paintedKey = "";

  paint() {
    // The key must read the same shape render() and afterPaint() accept: the
    // payload can spell the change either `change` or `Change`, and a key
    // derived only from the lowercase form would be "" for a PascalCase
    // payload — turning every repaint, including a genuine head move, into a
    // same-key capture.
    const change = value(this.data || {}, "change", "Change") || {};
    const id = value(change, "id", "ID") || "";
    // The head side of the key is the *displayed* head — the diff response's
    // own head_sha when the diff names one, falling back to the change
    // metadata's — the same head render() anchors the drafts to and
    // submitReview() posts against. The two GETs are not atomic: the metadata
    // can lag (or lead) the diff. Keying on the metadata head alone would
    // treat a diff-head move under unchanged metadata as a same-key repaint
    // and capture the old head's overall comment into the new head's bar;
    // keying on the displayed head moves the key exactly when the code on
    // screen changes.
    const head = this._displayedHead;
    const key = id && head ? `${id}:${head}` : "";
    if (key !== this.#paintedKey) {
      this.drafts.clear();
      // The overall comment is anchored to the same displayed head as the
      // inline drafts: a moved head must not carry it into the new head's bar.
      this._reviewBody = "";
      // FlowElement skips the write when the rendered HTML is unchanged, and
      // the head summary abbreviates the SHA, so a moved head can render
      // byte-identical markup. Force the write so the clear lands and a stale
      // draft editor or review-bar count cannot stay mounted in the DOM.
      this.invalidate();
    } else {
      this.captureDrafts();
      // The overall-comment input is not a [data-draft] node, so the inline
      // capture cannot see it; read its live value back the same way before
      // the write replaces the bar.
      this.captureReviewBody();
    }
    super.paint();
    this.#paintedKey = key;
  }

  // Drafts live in the DOM between repaints, so read them back before any
  // re-render that would otherwise discard what was typed.
  captureDrafts() {
    for (const node of this.querySelectorAll("[data-draft]")) {
      const draft = this.drafts.get(node.dataset.draft);
      if (draft) draft.body = String(node.querySelector("[data-draft-body]")?.value || "");
    }
  }

  // The overall-comment input is not a [data-draft] node, so captureDrafts
  // leaves it alone. Read its live value back before any same-head repaint
  // that would replace the review bar and discard what was typed.
  captureReviewBody() {
    const input = this.querySelector("[data-review-body]");
    this._reviewBody = input ? String(input.value || "") : "";
  }

  // submitReview gets the same pending state as the action buttons: the
  // verdict button is marked busy and the overall-comment input is disabled
  // synchronously, the status line names the in-flight submission, and the
  // shared in-flight registry (keyed by change, not by DOM node) blocks a
  // duplicate verdict while the first is running — even if a poll re-render
  // replaced the button or the input.
  async submitReview(verdict, button) {
    this.captureDrafts();
    const changeID = value(this.data?.change || {}, "id", "ID");
    if (!changeID) return;
    // The head whose diff is on screen. render() stamps the data-head
    // attribute from the displayed head — the diff response's own head_sha
    // when the diff names one (the head the server rendered, what the
    // reviewer actually saw), falling back to the change metadata's head when
    // no diff is rendered. The two GETs are not atomic, so the metadata head
    // can be older than the diff on screen; binding to it would post a
    // head_sha the rendered diff does not match. The painted stamp is read
    // rather than the model because a poll can set the model to a newer head
    // while the repaint is still queued — the rendered diff and review
    // controls then show the old head, and this submission's body and drafts
    // were captured from them. Binding to the model's newer head would let
    // feedback written against the rendered code be accepted as a review of
    // code the reviewer never saw. The server rejects the submission with a
    // conflict if the change advanced past the named head, keeping this
    // review's threads and verdict attached to the code that was actually
    // inspected.
    const headSHA = this._paintedHead;
    const openWait = value(this.data || {}, "open_wait", "OpenWait") || {};
    const openWaitKind = String(value(openWait, "kind", "Kind") || "");
    const nodeRunID = String(value(openWait, "node_run_id", "NodeRunID") || "").trim();
    const reviewWaitID = String(value(openWait, "id", "ID") || "").trim();
    const observedGate = openWaitKind === "human_gate" && nodeRunID && reviewWaitID;
    const comments = [...this.drafts.values()]
      .filter((draft) => draft.body.trim())
      .map((draft) => ({ file_path: draft.path, line: draft.line, body: draft.body.trim() }));
    const body = this.querySelector("flow-review-bar")?.body || "";
    const busyKey = `review:${changeID}`;
    if (!comments.length && !body && verdict === "comment") {
      // Validation feedback goes through the same shared status arbitration as
      // every settle: while another mutation is in flight its pending label
      // stays visible instead of this message hiding it. Validation never
      // acquires the review key, so a review already in flight keeps its own
      // label too.
      if (!inFlight.has(busyKey)) settleStatus(this.app, busyKey, "Nothing to post");
      return;
    }
    // The change — not the individual verdict — is the review's mutation
    // target: every verdict posts to the same review endpoint and reports to
    // the same review gate, so while one verdict is in flight the others are
    // suppressed too, and a contradictory verdict cannot race it.
    const entry = acquireBusy(busyKey, `${reviewPendingLabel(verdict)}…`);
    if (!entry) return;
    entry.verdict = verdict;
    if (button) markBusy(button);
    for (const control of this.querySelectorAll("[data-review-verdict]")) {
      if (control !== button) control.disabled = true;
    }
    // The overall-comment input is not a mutation target, but it reads as part
    // of the same submission: disable it synchronously too, so a poll or
    // repaint is not what makes it look pending. Its value stays in the DOM
    // while disabled, and the restore below hands it back on settle.
    const bodyInput = this.querySelector("[data-review-body]");
    if (bodyInput) bodyInput.disabled = true;
    // Restore by re-querying the live DOM rather than the nodes marked above:
    // a repaint (poll, draft change) replaces the bar mid-flight, and whatever
    // is on screen when the submission settles is what must be re-enabled.
    entry.restores.add(() => {
      for (const control of this.querySelectorAll("[data-review-verdict]")) {
        control.disabled = false;
        control.removeAttribute?.("disabled");
        control.removeAttribute?.("aria-busy");
        control.classList?.remove("is-busy");
      }
      const input = this.querySelector("[data-review-body]");
      if (input) {
        input.disabled = false;
        input.removeAttribute?.("disabled");
      }
    });
    this.app?.setStatus(entry.label);
    try {
      await apiPost(`/v2/changes/${encodeURIComponent(changeID)}/review`, {
        verdict,
        body,
        comments,
        head_sha: headSHA,
        ...(verdict !== "comment" && observedGate ? { node_run_id: nodeRunID, review_wait_id: reviewWaitID } : {}),
      });
      // The server recorded this review against the head the submission named.
      // Settlement belongs to that head's display: a poll may have repainted
      // the change to a newer head while the request was out, and the reviewer
      // could already be drafting against it — so clear the submitted drafts
      // only while that head is still on screen.
      if (this._paintedHead === headSHA) {
        this.drafts.clear();
        // The submitted body is consumed: drop the captured draft and the
        // live input value, or the refresh's same-key repaint would capture
        // the submitted text back into the fresh bar.
        this._reviewBody = "";
        const input = this.querySelector("[data-review-body]");
        if (input) input.value = "";
      }
      // The verdict flow is its own dispatcher (acquireBusy/POST/settleStatus
      // run inline here), so stamp the refresh with the settle-burst
      // provenance token directly instead of going through actionScope.
      await this.app?.refresh({ settle: ACTION_SETTLE });
      // settleStatus keeps a still-pending sibling's label visible instead of
      // showing this verdict's result early.
      if (this._paintedHead === headSHA) {
        settleStatus(this.app, busyKey, reviewMessage(verdict, comments.length));
      } else {
        // The review landed for the head the reviewer inspected, but that head
        // is no longer displayed: drop the pending label without claiming the
        // newer head's bar — the refresh's data carries the recorded outcome.
        settleStatus(this.app, busyKey, "");
      }
    } catch (error) {
      // failureMessage is total, so settleStatus always runs and the key always
      // drains — even for a non-Error rejection such as `reject(null)`.
      settleStatus(this.app, busyKey, failureMessage(error));
      // A repaint during the request replaces the comment input; give the
      // reviewer their unsubmitted words back alongside the error — but only
      // while the change still displays the head this submission was made
      // against. A head move re-renders the element (and drops the inline
      // drafts); restoring the rejected h1 body into the h2 review bar would
      // let a later submission post feedback on code the reviewer never
      // inspected, naming the newer head.
      const input = this.querySelector("[data-review-body]");
      if (this._paintedHead === headSHA && input && !input.value) input.value = body;
    } finally {
      releaseBusy(busyKey);
    }
  }

  // reviewBusyVerdict is the verdict currently being submitted for this
  // change, or "" when idle. The review bar reads it at render time, so a
  // repaint while a submission is in flight reproduces the suppressed verdict
  // controls instead of handing back enabled ones.
  get reviewBusyVerdict() {
    const changeID = value(this.data?.change || {}, "id", "ID");
    if (!changeID) return "";
    return inFlightEntries.get(`review:${changeID}`)?.verdict || "";
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
