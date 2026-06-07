// Terminal/transcript route + inline-terminal actions. These open ttyd frames
// (and transcript surfaces) mounted into the app's DOM.

import { apiGetText, apiPost } from "./api.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { closeInlineTerminal, inlineTerminalMount, renderTerminalPopOutButton, renderTerminalSurface, terminalMount, terminalSelectionHint } from "./terminal.js";

export async function renderTerminalView(app, sessionID, context) {
  app.setTitle("Terminal");
  app.querySelector(".content").innerHTML = `<section class="detail"><div class="empty">Connecting terminal</div></section>`;
  const data = await apiPost(`/v1/sessions/${encodeURIComponent(sessionID)}/terminal-token`, {});
  if (context && !app.isActiveLoad(context)) return false;
  const access = data.access || data.Access || {};
  const loginPath = value(access, "login_path", "LoginPath");
  if (!loginPath) throw new Error("Terminal URL is unavailable");
  app.querySelector(".content").innerHTML = `
    <section class="detail terminal-detail">
      <div class="detail-head">
        <div>
          <h2>${escapeHTML(sessionID)}</h2>
        </div>
        <div class="actions">
          ${terminalSelectionHint}
          ${renderTerminalPopOutButton(loginPath)}
        </div>
      </div>
      <div class="terminal-bezel">
        <div class="terminal-titlebar"><span class="dot"></span><span>session ${escapeHTML(sessionID)}</span>${terminalSelectionHint}</div>
        <iframe class="terminal-frame" title="Terminal ${escapeAttr(sessionID)}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>
      </div>
    </section>
  `;
  return true;
}

export async function openInlineTerminalView(app, button, kind, id) {
  const terminalID = String(id || "").trim();
  if (!terminalID) return;
  const target = terminalMount(button, app);
  if (
    target.mount?.dataset?.inlineTerminalMode === "terminal"
    && target.mount.dataset.inlineTerminalKind === kind
    && target.mount.dataset.inlineTerminalId === terminalID
  ) {
    if (target.presentation === "modal") {
      target.mount.remove();
    } else {
      closeInlineTerminal(button, app);
    }
    app.setStatus("");
    return;
  }
  target.mount.dataset.inlineTerminalMode = "terminal";
  target.mount.dataset.inlineTerminalKind = kind;
  target.mount.dataset.inlineTerminalId = terminalID;
  target.mount.innerHTML = renderTerminalSurface(target.presentation, kind, terminalID, `<div class="empty">Connecting terminal</div>`);
  try {
    const path = kind === "job"
      ? `/v1/jobs/${encodeURIComponent(terminalID)}/terminal-token`
      : `/v1/sessions/${encodeURIComponent(terminalID)}/terminal-token`;
    const data = await apiPost(path, {});
    const access = data.access || data.Access || {};
    const loginPath = value(access, "login_path", "LoginPath");
    if (!loginPath) throw new Error("Terminal URL is unavailable");
    target.mount.innerHTML = renderTerminalSurface(
      target.presentation,
      kind,
      terminalID,
      `<iframe class="terminal-frame" title="Terminal ${escapeAttr(terminalID)}" src="${escapeAttr(loginPath)}" referrerpolicy="no-referrer"></iframe>`,
      loginPath,
    );
    app.setStatus("");
  } catch (error) {
    const message = error.message || String(error);
    target.mount.innerHTML = renderTerminalSurface(target.presentation, kind, terminalID, `<div class="empty">${escapeHTML(message)}</div>`);
    app.setStatus(message);
  }
}

export async function showTranscriptView(app, button, kind, id) {
  const transcriptID = String(id || "").trim();
  if (!transcriptID) return;
  const path = kind === "job"
    ? `/v1/jobs/${encodeURIComponent(transcriptID)}/transcript`
    : `/v1/sessions/${encodeURIComponent(transcriptID)}/transcript`;
  const mount = inlineTerminalMount(button, app);
  mount.dataset.inlineTerminalMode = "transcript";
  mount.dataset.inlineTerminalKind = kind;
  mount.dataset.inlineTerminalId = transcriptID;
  mount.innerHTML = `<div class="empty">Loading transcript</div>`;
  try {
    const text = await apiGetText(path);
    mount.innerHTML = `<pre class="transcript-view">${escapeHTML(text)}</pre>`;
    app.setStatus("");
  } catch (error) {
    const message = error.message || String(error);
    mount.innerHTML = `<div class="empty">${escapeHTML(message)}</div>`;
    app.setStatus(message);
  }
}
