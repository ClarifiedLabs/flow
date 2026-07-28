// The Review tab: the one place that answers "what does this task need from
// me". It renders the open human gate with the artifact under review (the
// planner's summary and task-set manifest), the feedback box and one button
// per gate outcome, the discussion so far, and — for interactive reviews — a
// terminal into the live agent session so the reviewer can work with the
// agent directly. Agent questions land here too: question, reply box, done.
//
// Outcome buttons ride the global data-workflow-respond action, comments the
// data-gate-comment action; both take the feedback textarea from the closest
// [data-gate-panel], so this element never wires its own listeners for them.

import { apiPost } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { formatDate } from "../format.js";
import { define, FlowElement } from "./base.js";

export function renderReviewPanel(model) {
  const review = model?.review;
  if (!review) return `<p class="empty">Nothing to review</p>`;
  const parts = [];
  if (review.gate) parts.push(renderGate(model, review));
  if (review.question) parts.push(renderQuestion(model, review));
  if (review.artifact && !review.gate) parts.push(renderArtifact(review.artifact, !review.gate));
  if (review.comments.length) parts.push(renderComments(review.comments));
  if (review.session && review.session.state === "waiting") parts.push(renderLiveSession(review.session));
  return parts.join("");
}

function projectAttrOf(model) {
  return model?.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
}

function renderGate(model, review) {
  const gate = review.gate;
  const projectAttr = projectAttrOf(model);
  const outcomes = gate.outcomes.length ? gate.outcomes : ["approved", "changes_requested"];
  const changeID = String(value(model?.change || {}, "id", "ID") || "");
  return `
    <section class="gate" data-gate-panel>
      <div class="gate-head">
        <h3>${escapeHTML(gate.heading)}</h3>
        ${gate.interactive ? `<span class="badge" data-tone="action">agent is live</span>` : ""}
      </div>
      <p class="instructions">${escapeHTML(gate.instructions)}</p>
      ${gate.changeGate ? renderChangePointer(changeID) : review.artifact ? renderArtifact(review.artifact, false) : ""}
      <textarea data-workflow-feedback rows="4" placeholder="Feedback — recorded with your decision, and delivered to the agent when it is still in its session"></textarea>
      <div class="actions">
        ${outcomes
          .map(
            (outcome, index) => `
          <button class="button${index === 0 ? "" : " secondary"}" data-workflow-respond="${escapeAttr(gate.nodeRunID)}" data-task="${escapeAttr(model.id)}" data-outcome="${escapeAttr(outcome)}"${projectAttr}>${escapeHTML(String(outcome).replaceAll("_", " "))}</button>`,
          )
          .join("")}
        <button class="button secondary" data-gate-comment="${escapeAttr(model.id)}"${projectAttr}>Comment only</button>
      </div>
    </section>
  `;
}

function renderChangePointer(changeID) {
  const href = changeID ? `/ui/changes/${escapeAttr(changeID)}` : "";
  return `
    <p class="note">This gate reviews a change — read the diff and answer it from the
    ${href ? `<a href="${href}" data-link>change view</a>` : "change view"} or the Change tab.</p>
  `;
}

function renderQuestion(model, review) {
  const question = review.question;
  return `
    <section class="gate question" data-gate-panel>
      <div class="gate-head"><h3>Question from the agent</h3></div>
      <div class="prose">${renderMarkdown(question.message)}</div>
      <form class="reply" data-attention-reply-form="${escapeAttr(model.id)}" data-task="${escapeAttr(model.id)}" data-status-log-id="${escapeAttr(question.statusLogID)}"${projectAttrOf(model)}>
        <textarea name="message" rows="3" placeholder="Reply"></textarea>
        <button class="button" type="submit">Send reply</button>
      </form>
    </section>
  `;
}

function renderArtifact(artifact, standalone) {
  return `
    <section class="artifact">
      <div class="gate-head">
        <h3>${standalone ? "Plan under review" : "The plan"}</h3>
        ${artifact.createdAt ? `<span class="locus">${escapeHTML(formatDate(artifact.createdAt))}</span>` : ""}
      </div>
      ${artifact.summary ? `<div class="prose">${renderMarkdown(artifact.summary)}</div>` : ""}
      ${artifact.manifest ? renderManifest(artifact.manifest) : ""}
    </section>
  `;
}

// renderManifest lays the task-set JSON out as the list of tasks it will
// become: key, title, tags, and what each task is blocked by.
function renderManifest(manifest) {
  const dependencies = Array.isArray(manifest.dependencies) ? manifest.dependencies : [];
  const blockedBy = new Map();
  for (const dep of dependencies) {
    const list = blockedBy.get(dep.blocked) || [];
    list.push(dep.blocker);
    blockedBy.set(dep.blocked, list);
  }
  return `
    <div class="plan-tasks">
      ${(manifest.tasks || [])
        .map((task) => {
          const tags = (task.tag_slugs || []).map((tag) => `<span class="tag">${escapeHTML(tag)}</span>`).join("");
          const deps = (blockedBy.get(task.key) || []).map((key) => `<span class="tag">← ${escapeHTML(key)}</span>`).join("");
          return `
        <div class="plan-task">
          <span class="key">${escapeHTML(task.key || "")}</span>
          <span class="title">${escapeHTML(task.title || "")}</span>
          ${task.flow_id ? `<span class="tag">${escapeHTML(task.flow_id)}</span>` : ""}
          ${tags}${deps}
        </div>`;
        })
        .join("")}
    </div>
  `;
}

function renderComments(comments) {
  return `
    <section class="comments">
      <div class="gate-head"><h3>Discussion</h3></div>
      ${comments
        .map(
          (comment) => `
        <div class="comment">
          <span class="actor">${escapeHTML(comment.actor || "human")}</span>
          <span class="locus">${escapeHTML(formatDate(comment.createdAt))}</span>
          <div class="prose">${renderMarkdown(comment.message)}</div>
        </div>`,
        )
        .join("")}
    </section>
  `;
}

// The live session renders as a bezel the element fills with an iframe once
// it has minted terminal access — mint-once, like the Terminal tab, so a poll
// never bounces the connection.
function renderLiveSession(session) {
  return `
    <section class="live-session" data-live-session="${escapeAttr(session.id)}">
      <div class="gate-head">
        <h3>Work with the agent</h3>
        <span class="locus">${escapeHTML(session.id)}</span>
      </div>
      <p class="note">The agent is holding its session while you review — comments above are delivered to it, or talk to it directly:</p>
      <div class="bezel" data-terminal-bezel></div>
    </section>
  `;
}

export function renderReviewTerminal(sessionID, loginPath) {
  return `
    <div class="terminal-bezel">
      <div class="terminal-titlebar"><span class="dot"></span><span>session ${escapeHTML(sessionID)}</span></div>
      <iframe class="terminal-frame" title="Session terminal ${escapeAttr(sessionID)}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>
    </div>
  `;
}

export class FlowReviewPanel extends FlowElement {
  terminalKey = "";
  terminalLoginPath = "";
  terminalError = "";
  terminalPromise = null;
  terminalGeneration = 0;

  render(model) {
    const session = model?.review?.session || null;
    let html = renderReviewPanel(model);
    if (session && session.state === "waiting") {
      const bezel = this.terminalLoginPath
        ? renderReviewTerminal(session.id, this.terminalLoginPath)
        : this.terminalError
          ? `<div class="empty">${escapeHTML(this.terminalError)} <button class="button secondary" type="button" data-review-terminal-retry>Retry</button></div>`
          : `<div class="empty">Connecting terminal</div>`;
      html = html.replace(`<div class="bezel" data-terminal-bezel></div>`, bezel);
    }
    return html;
  }

  afterPaint() {
    const session = this.data?.review?.session || null;
    if (!session || session.state !== "waiting" || !session.terminalAvailable) return;
    const key = `session:${session.id}`;
    if (key !== this.terminalKey) this.resetTerminalLoad(key);
    if (!this.terminalLoginPath && !this.terminalError && !this.terminalPromise) this.loadTerminal(session.id, key);
  }

  resetTerminalLoad(key = "") {
    this.terminalGeneration += 1;
    this.terminalKey = key;
    this.terminalLoginPath = "";
    this.terminalError = "";
    this.terminalPromise = null;
  }

  loadTerminal(sessionID, key) {
    const generation = this.terminalGeneration;
    this.terminalPromise = (async () => {
      try {
        const data = await apiPost(`/v2/sessions/${encodeURIComponent(sessionID)}/terminal-token`, {});
        if (generation !== this.terminalGeneration || key !== this.terminalKey) return;
        const access = value(data, "access", "Access") || {};
        const loginPath = value(access, "login_path", "LoginPath");
        if (!loginPath) throw new Error("Terminal URL is unavailable");
        this.terminalLoginPath = String(loginPath);
      } catch (error) {
        if (generation !== this.terminalGeneration || key !== this.terminalKey) return;
        this.terminalError = error.message || String(error);
      } finally {
        if (generation !== this.terminalGeneration || key !== this.terminalKey) return;
        this.terminalPromise = null;
        if (this.isConnected) this.invalidate();
      }
    })();
  }

  handleClick(event) {
    if (event.target.closest?.("[data-review-terminal-retry]")) {
      event.preventDefault();
      this.terminalError = "";
      this.invalidate();
    }
  }
}

define("flow-review-panel", FlowReviewPanel);
