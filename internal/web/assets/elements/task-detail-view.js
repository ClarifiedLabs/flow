// View helpers for <flow-task-detail>: the owner-rulings panel, the task
// terminal target/frame, and the change-cache identity keys. Pure functions,
// kept out of the element module so the class stays wiring-only.

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { renderTerminalPopOutButton, terminalSelectionHint } from "../terminal.js";

export function renderOwnerRulingsPanel(model) {
  if (!model?.runID) return "";
  const rulings = Array.isArray(model.activeRulings) ? model.activeRulings : [];
  const recordable = ["scheduled", "running", "waiting"].includes(String(model.runState || ""));
  const projectAttr = model.projectID ? ` data-project="${escapeAttr(model.projectID)}"` : "";
  const options = rulings.map((ruling) => {
    const id = String(value(ruling, "ruling_id", "RulingID") || "");
    return id ? `<option value="${escapeAttr(id)}">Replace ${escapeHTML(id)}</option>` : "";
  }).join("");
  const list = rulings.length
    ? `<ol class="activity-list">${rulings.map((ruling) => {
      const id = String(value(ruling, "ruling_id", "RulingID") || "");
      const source = String(value(ruling, "source", "Source") || "owner");
      const body = String(value(ruling, "body", "Body") || "");
      return `<li><div><code>${escapeHTML(id)}</code> <span class="badge">${escapeHTML(source.replaceAll("_", " "))}</span></div><p>${escapeHTML(body)}</p></li>`;
    }).join("")}</ol>`
    : `<p class="empty">No active rulings for this run</p>`;
  return `
    <section class="section" data-owner-ruling-panel>
      <h3>Owner ruling / scope guidance</h3>
      <p class="caption">Durable policy for this workflow run. Every later author, reviewer, and verifier receives it.</p>
      ${list}
      ${recordable ? `
        <label><span>Ruling</span><textarea rows="3" data-owner-ruling-body placeholder="Clarify requirements or task scope"></textarea></label>
        ${options ? `<label><span>Replacement</span><select data-owner-ruling-supersedes><option value="">Add alongside active rulings</option>${options}</select></label>` : ""}
        <button class="button" type="button" data-owner-ruling="${escapeAttr(model.id)}"${projectAttr}>Record ruling</button>
      ` : `<p class="caption">This workflow run is no longer active. Update the task body before scheduling a new run.</p>`}
    </section>
  `;
}

export function taskTerminalTarget(model) {
  if (!model?.terminalAvailable) return null;
  const session = model.activeSession || {};
  const sessionID = value(session, "id", "ID");
  if (sessionID && value(session, "terminal_available", "TerminalAvailable")) {
    return { kind: "session", id: String(sessionID) };
  }
  if (model.terminalJobID) return { kind: "job", id: String(model.terminalJobID) };
  return null;
}

export function renderTaskTerminal(target, loginPath) {
  const label = target.kind === "job" ? "Job terminal" : "Session terminal";
  return `
    <section class="task-terminal" aria-label="${escapeAttr(label)} ${escapeAttr(target.id)}">
      <div class="terminal-tab-head">
        <div><strong>${escapeHTML(label)}</strong><span>${escapeHTML(target.id)}</span></div>
        <div class="actions">
          ${terminalSelectionHint}
          ${renderTerminalPopOutButton(loginPath)}
        </div>
      </div>
      <div class="terminal-bezel">
        <div class="terminal-titlebar"><span class="dot"></span><span>${escapeHTML(target.kind)} ${escapeHTML(target.id)}</span></div>
        <iframe class="terminal-frame" title="${escapeAttr(label)} ${escapeAttr(target.id)}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>
      </div>
    </section>
  `;
}

// changeModelKey identifies what the cached change load belongs to: this
// task, this change, at this head. Polls deliver a brand-new model object
// every interval, so object identity cannot gate the cache reset — that would
// re-fetch the change (and rebuild the Change tab) on every poll. Only one of
// the three moving does. A same-key model instead marks the cache stale, and
// the Change tab revalidates it in place: another reviewer's comment, a
// review-state flip, or a permission change becomes visible without losing
// the selected file or the pending inline notes.
export function changeModelKey(model) {
  const change = model?.change;
  return [model?.id, value(change, "id", "ID"), value(change, "head_sha", "HeadSHA")]
    .map((part) => String(part || ""))
    .join(":");
}

// changeHeadKey builds the canonical 2-part `${id}:${head}` key that names a
// change at a head. Every construction site (poll-head comparison in render,
// paintChange's model key, reconcileChangeHead's model key, revalidateChange's
// pending marker, and adoptChangeHead's re-key) must produce the exact same
// string so the poll-head equality comparison stays in sync with the pending
// marker; a one-off separator, ordering, or coercion change at any site would
// silently desynchronize them with no compile- or test-time signal. All sites
// route through this helper so the format can only drift in one place.
export function changeHeadKey(id, head) {
  return `${String(id || "")}:${String(head || "")}`;
}
