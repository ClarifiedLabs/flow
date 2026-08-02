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
import { failureMessage, gateResponsePending } from "../actions.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { formatDate } from "../format.js";
import { define, FlowElement } from "./base.js";

// reviewPanelSections is the panel's content as an ordered list of keyed
// sections. FlowReviewPanel.paint reconciles these by key so a content-only
// repaint rewrites just the sections that changed and never disturbs the
// mounted live terminal. renderReviewPanel serialises the same list to one
// string for the initial paint and the markup tests.
export function reviewPanelSections(model) {
  const review = model?.review;
  if (!review) return [];
  const sections = [];
  if (review.gate) sections.push({ key: "gate", html: renderGate(model, review) });
  if (review.question) sections.push({ key: "question", html: renderQuestion(model, review) });
  if (review.artifact && !review.gate) sections.push({ key: "artifact", html: renderArtifact(review.artifact, !review.gate) });
  if (review.comments.length) sections.push({ key: "comments", html: renderComments(review.comments) });
  if (review.session && review.session.state === "waiting") sections.push({ key: "live", html: renderLiveSession(review.session) });
  return sections;
}

export function renderReviewPanel(model) {
  if (!model?.review) return `<p class="empty">Nothing to review</p>`;
  return reviewPanelSections(model)
    .map((section) => `<div data-section="${section.key}">${section.html}</div>`)
    .join("");
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
    <section class="gate" data-gate-panel data-gate-node-run="${escapeAttr(gate.nodeRunID)}">
      <div class="gate-head">
        <h3>${escapeHTML(gate.heading)}</h3>
        ${gate.interactive ? `<span class="badge" data-tone="action">agent is live</span>` : ""}
      </div>
      <div class="instructions">${renderMarkdown(gate.instructions)}</div>
      ${gate.changeGate ? renderChangePointer(changeID) : review.artifact ? renderArtifact(review.artifact, false) : ""}
      <textarea data-workflow-feedback rows="4" placeholder="Feedback — recorded with your decision, and delivered to the agent when it is still in its session"></textarea>
      <div class="actions">
        ${renderGateOutcomeButtons(outcomes, {
          nodeRunID: gate.nodeRunID,
          waitID: gate.waitID,
          taskID: model.id,
          projectAttr,
          secondaryFrom: 1,
        })}
        <button class="button secondary" data-gate-comment="${escapeAttr(model.id)}"${projectAttr}>Comment only</button>
      </div>
    </section>
  `;
}

// renderGateOutcomeButton renders one gate outcome control. The button carries
// the wait id of the review round it answers: the action posts it back as
// review_wait_id so the response stays bound to the round that was rendered,
// even when a poll repaint shows a newer round on the same node run. A poll
// repaint rebuilds the panel while a response for this node run is still in
// flight, so the fresh button re-derives its suppressed state from the shared
// in-flight registry instead of flashing enabled until the next click.
function renderGateOutcomeButton(outcome, { nodeRunID, waitID = "", taskID, projectAttr = "", secondary = false, pending = false } = {}) {
  const classes = ["button", secondary ? "secondary" : "", pending ? "is-busy" : ""].filter(Boolean).join(" ");
  const busyAttrs = pending ? ` disabled aria-busy="true"` : "";
  const waitAttr = waitID ? ` data-review-wait="${escapeAttr(waitID)}"` : "";
  return `<button class="${classes}" data-workflow-respond="${escapeAttr(nodeRunID)}"${waitAttr} data-task="${escapeAttr(taskID)}" data-outcome="${escapeAttr(outcome)}"${projectAttr}${busyAttrs}>${escapeHTML(String(outcome).replaceAll("_", " "))}</button>`;
}

// renderGateOutcomeButtons renders one control per outcome; while the shared
// gate response is pending every outcome is suppressed together.
export function renderGateOutcomeButtons(outcomes, { nodeRunID, waitID = "", taskID, projectAttr = "", secondaryFrom = null } = {}) {
  const pending = gateResponsePending(nodeRunID);
  return (Array.isArray(outcomes) ? outcomes : [])
    .map((outcome, index) =>
      renderGateOutcomeButton(outcome, {
        nodeRunID,
        waitID,
        taskID,
        projectAttr,
        secondary: secondaryFrom !== null ? index >= secondaryFrom : outcome === "changes_requested",
        pending,
      }),
    )
    .join("");
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
// never bounces the connection. The bezel starts empty; syncTerminal fills it.
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
    <div class="terminal-bezel" data-terminal-session="${escapeAttr(sessionID)}">
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
  // The sections currently painted, as [{ key, html }]; null until the first
  // paint. The reconciler compares against this record — not the live DOM — so
  // syncTerminal's mutations inside the live section never look like a content
  // change and never trigger a rewrite that would reload the terminal iframe.
  #paintedSections = null;

  render(model) {
    return renderReviewPanel(model);
  }

  // paint reconciles the panel section by section instead of rewriting the whole
  // element. A changed-model poll (task-detail polling rewrites the model on
  // every discussion change) updates only the sections whose markup changed and
  // leaves the rest — crucially the live terminal — connected. Removing and
  // re-appending the terminal iframe would reload it in a browser (disconnecting
  // an iframe tears down its nested browsing context), which is exactly the
  // bounce this element exists to prevent.
  paint() {
    // Key the terminal to the session under review before rendering anything,
    // so a session transition can never expose a prior session's credential.
    this.ensureTerminalLoad();

    const sections = reviewPanelSections(this.data);
    if (!sections.length) {
      this.#paintedSections = null;
      const html = renderReviewPanel(this.data);
      if (this.innerHTML !== html) this.innerHTML = html;
      this.syncTerminal();
      return;
    }

    if (!this.#paintedSections) {
      // First paint (or a rebuild after an empty state): write it all at once.
      this.#paintedSections = sections.map((section) => ({ key: section.key, html: section.html }));
      this.innerHTML = sections.map((section) => `<div data-section="${section.key}">${section.html}</div>`).join("");
      this.syncTerminal();
      return;
    }

    const wrappers = new Map();
    for (const child of Array.from(this.children)) {
      const key = child.getAttribute?.("data-section");
      if (key) wrappers.set(key, child);
    }
    const stored = new Map(this.#paintedSections.map((section) => [section.key, section.html]));

    // Drop stale wrappers before positioning. Removing them afterwards would
    // treat a stale section as the successor of an unchanged retained section,
    // moving that wrapper only because the stale one sat between it and its
    // desired neighbour (e.g. [gate, comments, live] -> [gate, live] used to
    // shuffle gate past comments before comments was removed).
    const desired = new Set(sections.map((section) => section.key));
    for (const [key, wrapper] of wrappers) {
      if (!desired.has(key)) wrapper.remove();
    }

    // Walk the desired order back to front, positioning each section just
    // before the one after it. A section whose markup is unchanged is left
    // completely untouched; only changed sections rewrite their interior.
    let reference = null;
    for (let index = sections.length - 1; index >= 0; index -= 1) {
      const { key, html } = sections[index];
      let wrapper = wrappers.get(key);
      if (!wrapper) {
        wrapper = document.createElement("div");
        wrapper.setAttribute("data-section", key);
        wrapper.innerHTML = html;
      } else if (stored.get(key) !== html) {
        wrapper.innerHTML = html;
      }
      if (wrapper.parentElement !== this || wrapper.nextElementSibling !== reference) {
        this.insertBefore(wrapper, reference);
      }
      reference = wrapper;
    }

    this.#paintedSections = sections.map((section) => ({ key: section.key, html: section.html }));
    this.syncTerminal();
  }

  // ensureTerminalLoad keys the mint to the session under review and starts it
  // once. The session identity is keyed and reset *before* the availability
  // guard, so a changed session is always reset even while its terminal is not
  // yet available — a session transition never reuses a prior login URL.
  ensureTerminalLoad() {
    const session = this.data?.review?.session || null;
    const waiting = session && session.state === "waiting" ? session : null;
    const key = waiting ? `session:${waiting.id}` : "";
    if (key !== this.terminalKey) this.resetTerminalLoad(key);
    if (!waiting || !waiting.terminalAvailable) return;
    if (!this.terminalLoginPath && !this.terminalError && !this.terminalPromise) this.loadTerminal(waiting.id, key);
  }

  // syncTerminal matches the mounted terminal to the session under review. When
  // the bezel already carries the live iframe for the current session it is left
  // untouched so the connection is never reloaded. Any other case — a changed
  // session identity, a session that left the waiting state, or a pre-iframe
  // state that must now show the minted iframe / connecting / error notice —
  // rebuilds the bezel content.
  syncTerminal() {
    const session = this.data?.review?.session || null;
    const waiting = session && session.state === "waiting" ? session : null;
    // The test DOM's querySelector only supports simple selectors, so find the
    // live section first and then the bezel within it.
    const live = this.querySelector('[data-section="live"]');
    const placeholder = live?.querySelector("[data-terminal-bezel]");
    if (!placeholder) return;

    const mounted = placeholder.querySelector("[data-terminal-session]");
    if (waiting && mounted?.querySelector("iframe") && mounted.getAttribute("data-terminal-session") === waiting.id) {
      return;
    }
    placeholder.innerHTML = waiting ? this.terminalStateMarkup(waiting) : "";
  }

  // terminalStateMarkup renders what the bezel holds while the iframe is not yet
  // mounted (connecting / unavailable / mint error), or the iframe itself once
  // access is minted. The iframe is only ever created here, once per session.
  terminalStateMarkup(session) {
    if (this.terminalLoginPath) return renderReviewTerminal(session.id, this.terminalLoginPath);
    if (this.terminalError) {
      return `<div class="empty">${escapeHTML(this.terminalError)} <button class="button secondary" type="button" data-review-terminal-retry>Retry</button></div>`;
    }
    if (!session.terminalAvailable) return `<div class="empty">Terminal not available yet</div>`;
    return `<div class="empty">Connecting terminal</div>`;
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
        this.terminalError = failureMessage(error);
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
