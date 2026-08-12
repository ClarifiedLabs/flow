// The Threads tab: every review thread recorded for the task, across all of
// its changes — the full cross-change discussion record behind
// GET /v2/tasks/{id}/threads. Threads group under the change they anchor to,
// so certified history on superseded run branches stays visible next to the
// current round, and anchors that no longer match their change's head are
// flagged as outdated evidence rather than hidden.
//
// v1 is read-only: the tab is a record, not a workspace. Commenting and
// claiming stay in the Change tab's inline diff view (and the Review tab),
// which already exist; each thread carries a data-focus-tab="change" control
// that jumps there. Like findings-list.js this is a plain render shell: the
// list travels inside the task model, so a poll repaint paints the same list
// through the same element instance (task-detail mounts by tag, reusing the
// element), and the base class's identical-markup skip leaves the DOM — and
// the reader's scroll position — untouched.

import { formatRelative, shortSHA } from "../format.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { isOutdatedAnchor } from "../task-model.js";
import { define, FlowElement } from "./base.js";

export function renderThreadList(model) {
  const threads = model?.taskThreads || [];
  if (!threads.length) return `<p class="empty">No review threads recorded</p>`;
  const changes = new Map(
    (model?.changes || []).map((change) => [String(value(change, "id", "ID") || ""), change]),
  );
  const currentID = String(value(model?.change || {}, "id", "ID") || "");
  return groupByChange(threads)
    .map((group) => renderGroup(group, changes, currentID))
    .join("");
}

// groupByChange buckets threads under their change in first-seen order. The
// server orders threads by created_at, so the groups read as the review
// rounds they were: the earliest change's threads first.
function groupByChange(threads) {
  const groups = [];
  const byChange = new Map();
  for (const thread of threads) {
    const changeID = String(value(thread, "change_id", "ChangeID") || "");
    let group = byChange.get(changeID);
    if (!group) {
      group = { changeID, threads: [] };
      byChange.set(changeID, group);
      groups.push(group);
    }
    group.threads.push(thread);
  }
  return groups;
}

function renderGroup(group, changes, currentID) {
  const change = changes.get(group.changeID) || null;
  const current = group.changeID !== "" && group.changeID === currentID;
  return `
    <section class="group" data-change="${escapeAttr(group.changeID)}"${current ? " data-current" : ""}>
      <h4 class="group-head">
        <span class="change-id">${escapeHTML(group.changeID || "unknown change")}</span>
        ${current ? `<span class="current">current</span>` : ""}
      </h4>
      <div class="threads">${group.threads.map((thread) => renderThread(thread, change)).join("")}</div>
    </section>
  `;
}

function renderThread(thread, change) {
  const id = String(value(thread, "id", "ID") || "");
  const state = String(value(thread, "state", "State") || "open");
  const comments = value(thread, "comments", "Comments") || [];
  return `
    <article class="thread" data-thread="${escapeAttr(id)}" data-state="${escapeAttr(state)}">
      <div class="head">
        <span class="badge" data-tone="${escapeAttr(stateTone(state))}">${escapeHTML(state)}</span>
        ${renderAnchor(thread)}
        ${isOutdatedAnchor(thread, change) ? `<span class="flag">outdated anchor</span>` : ""}
        <span class="spacer"></span>
        <button class="button secondary" type="button" data-focus-tab="change">Open in diff</button>
      </div>
      ${renderContext(thread)}
      ${renderResolution(thread)}
      <div class="timeline">${comments.map(renderComment).join("")}</div>
    </article>
  `;
}

// stateTone colours the state badge by what the state means to the reader:
// open and reopened threads still need attention, a claimed thread is parked
// on the author, a certified one is verified.
function stateTone(state) {
  if (state === "certified") return "ok";
  if (state === "claimed") return "idle";
  return "warn";
}

// renderAnchor links a thread to its change when it carries a file (and
// line). The anchor is the change route; there is no per-line deep link.
function renderAnchor(thread) {
  const file = value(thread, "file_path", "FilePath") || "";
  if (!file) return "";
  const line = Number(value(thread, "line", "Line")) || 0;
  const label = line > 0 ? `${file}:${line}` : file;
  const changeID = value(thread, "change_id", "ChangeID") || "";
  if (!changeID) return `<span class="anchor">${escapeHTML(label)}</span>`;
  return `<a class="anchor" href="${escapeAttr(`/ui/changes/${encodeURIComponent(changeID)}`)}" data-link>${escapeHTML(label)}</a>`;
}

// renderContext shows the code line the reviewer anchored to, when the thread
// carries one.
function renderContext(thread) {
  const context = String(value(thread, "context", "Context") || "").trim();
  if (!context) return "";
  return `<pre class="context">${escapeHTML(context)}</pre>`;
}

// CLAIM_LABELS names the claim kinds the same way the CLI does.
const CLAIM_LABELS = { fixed: "fixed", not_warranted: "not warranted", superseded: "superseded" };

// renderResolution is the one-line ledger of how the thread moved: the claim
// (kind, short commit, actor), then certification or the reopen.
function renderResolution(thread) {
  const state = String(value(thread, "state", "State") || "open");
  const parts = [];
  if (state === "claimed" || state === "certified") {
    const kind = String(value(thread, "claim_kind", "ClaimKind") || "");
    const by = String(value(thread, "claimed_by", "ClaimedBy") || "");
    const sha = shortSHA(value(thread, "claim_commit_sha", "ClaimCommitSHA"));
    parts.push(`claimed ${CLAIM_LABELS[kind] || kind || "claimed"}${sha ? ` at ${sha}` : ""}${by ? ` by ${by}` : ""}`);
  }
  if (state === "certified") {
    const by = String(value(thread, "certified_by", "CertifiedBy") || "");
    parts.push(`certified${by ? ` by ${by}` : ""}`);
  }
  if (state === "reopened") {
    const by = String(value(thread, "reopened_by", "ReopenedBy") || "");
    parts.push(`reopened${by ? ` by ${by}` : ""}`);
  }
  if (!parts.length) return "";
  return `<p class="resolution" data-tone="${escapeAttr(stateTone(state))}">${escapeHTML(parts.join(" · "))}</p>`;
}

function renderComment(comment) {
  return `
    <div class="comment">
      <div class="comment-head">
        <span class="actor">${escapeHTML(value(comment, "actor", "Actor") || "")}</span>
        <span class="meta">${escapeHTML(formatRelative(value(comment, "created_at", "CreatedAt")))}</span>
      </div>
      <div class="prose">${renderMarkdown(value(comment, "body", "Body") || "")}</div>
    </div>
  `;
}

export class FlowThreadList extends FlowElement {
  render(model) {
    return renderThreadList(model);
  }
}

define("flow-thread-list", FlowThreadList);
