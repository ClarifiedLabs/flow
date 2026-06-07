// Embedded ttyd terminal surfaces: inline frames, modal/pop-out windows and
// transcript views, plus their launch buttons and icons.

import { escapeAttr, escapeHTML } from "./html.js";

export function terminalMount(button, root) {
  if (button.closest?.("tr")) {
    return { mount: inlineTerminalMount(button, root), presentation: "inline" };
  }
  if (button.closest?.(".card, .feed-item")) {
    return { mount: terminalModalMount(root), presentation: "modal" };
  }
  return { mount: inlineTerminalMount(button, root), presentation: "inline" };
}

export function terminalModalMount(root) {
  const host = terminalModalHost(root);
  let mount = directTerminalModalChild(host);
  if (!mount) {
    mount = document.createElement("div");
    mount.className = "terminal-modal-layer";
    mount.dataset.terminalModalLayer = "true";
    host.appendChild(mount);
  }
  return mount;
}

export function terminalModalHost(root) {
  const main = root.querySelector?.(".main");
  if (main && elementHasClass(main, "main")) return main;
  return root.querySelector?.(".content") || root;
}

export function directTerminalModalChild(host) {
  for (const child of Array.from(host?.children || [])) {
    if (child.dataset?.terminalModalLayer === "true") return child;
  }
  return null;
}

export function inlineTerminalMount(button, root) {
  const row = button.closest?.("tr");
  if (row) {
    let terminalRow = row.nextElementSibling?.dataset?.inlineTerminalRow === "true" ? row.nextElementSibling : null;
    if (!terminalRow) {
      terminalRow = document.createElement("tr");
      terminalRow.className = "inline-terminal-row";
      terminalRow.dataset.inlineTerminalRow = "true";
      const cell = document.createElement("td");
      cell.colSpan = Math.max(1, row.cells?.length || row.querySelectorAll?.("td, th")?.length || 1);
      const mount = document.createElement("div");
      mount.dataset.inlineTerminal = "true";
      terminalRow.appendChild(cell);
      cell.appendChild(mount);
      row.after(terminalRow);
    }
    return terminalRow.querySelector("[data-inline-terminal]");
  }

  const detailHead = button.closest?.(".detail-head");
  if (detailHead?.parentElement && elementHasClass(detailHead.parentElement, "detail")) {
    let mount = detailHead.nextElementSibling?.dataset?.inlineTerminal === "true" ? detailHead.nextElementSibling : null;
    if (!mount) {
      mount = document.createElement("div");
      mount.dataset.inlineTerminal = "true";
      detailHead.after(mount);
    }
    return mount;
  }

  const host = button.closest?.(".card, .feed-item, .detail") || button.parentElement || root.querySelector(".content");
  let mount = directInlineTerminalChild(host);
  if (!mount) {
    mount = document.createElement("div");
    mount.dataset.inlineTerminal = "true";
    host.appendChild(mount);
  }
  return mount;
}

export function closeInlineTerminal(button, root) {
  const row = button.closest?.("tr");
  if (row) {
    const terminalRow = row.nextElementSibling?.dataset?.inlineTerminalRow === "true" ? row.nextElementSibling : null;
    if (!terminalRow) return false;
    terminalRow.remove();
    return true;
  }

  const detailHead = button.closest?.(".detail-head");
  if (detailHead?.parentElement && elementHasClass(detailHead.parentElement, "detail")) {
    const mount = detailHead.nextElementSibling?.dataset?.inlineTerminal === "true" ? detailHead.nextElementSibling : null;
    if (!mount) return false;
    mount.remove();
    return true;
  }

  const host = button.closest?.(".card, .feed-item, .detail") || button.parentElement || root.querySelector(".content");
  const mount = directInlineTerminalChild(host);
  if (!mount) return false;
  mount.remove();
  return true;
}

export function directInlineTerminalChild(host) {
  for (const child of Array.from(host?.children || [])) {
    if (child.dataset?.inlineTerminal === "true") return child;
  }
  return null;
}

export function elementHasClass(element, className) {
  if (element.classList?.contains(className)) return true;
  return String(element.className || "").split(/\s+/).includes(className);
}

export function renderTerminalSurface(presentation, kind, id, body, loginPath = "") {
  if (presentation === "modal") {
    return renderTerminalDialog(kind, id, body, loginPath);
  }
  return renderInlineTerminal(kind, id, body, loginPath);
}

export function renderInlineTerminal(kind, id, body, loginPath = "") {
  const label = kind === "job" ? "Job terminal" : "Session terminal";
  return `
    <section class="inline-terminal" aria-label="${escapeAttr(label)} ${escapeAttr(id)}">
      <div class="inline-terminal-head">
        <div>
          <strong>${escapeHTML(label)}</strong>
          <span>${escapeHTML(id)}</span>
        </div>
        <div class="actions">
          ${renderTerminalActions("data-terminal-hide", loginPath)}
        </div>
      </div>
      ${body}
    </section>
  `;
}

export function renderTerminalDialog(kind, id, body, loginPath = "") {
  const label = kind === "job" ? "Job terminal" : "Session terminal";
  return `
    <div class="terminal-modal" role="dialog" aria-modal="true" aria-label="${escapeAttr(label)} ${escapeAttr(id)}">
      <section class="terminal-modal-panel">
        <div class="terminal-modal-head">
          <div>
            <strong>${escapeHTML(label)}</strong>
            <span>${escapeHTML(id)}</span>
          </div>
          <div class="actions">
            ${renderTerminalActions("data-terminal-close", loginPath)}
          </div>
        </div>
        ${body}
      </section>
    </div>
  `;
}

export function renderTerminalActions(hideAttribute, loginPath) {
  return `
    ${terminalSelectionHint}
    ${renderTerminalPopOutButton(loginPath)}
    <button class="button secondary" type="button" ${hideAttribute}>Hide</button>
  `;
}

export function renderTerminalPopOutButton(loginPath) {
  return loginPath
    ? `<button class="button secondary" type="button" data-terminal-popout="${escapeAttr(loginPath)}">Pop out</button>`
    : "";
}

// terminalSelectionHint documents the reliable copy path. tmux owns mouse
// selection (mouse is on so wheel scrolling works), so a plain drag selection
// vanishes on mouse-up in the browser and is not copied. tmux does emit OSC 52
// (see set-clipboard), but the ttyd terminal shipped here has no OSC 52 handler,
// so that path does not auto-copy in this deployment. Shift+drag bypasses tmux
// selection for a native browser selection that Ctrl/Cmd+C copies on every
// transport; that is what this hint teaches.
export const terminalSelectionHint = '<span class="terminal-hint">Shift+drag to select</span>';

export function openTerminalWindow(loginPath) {
  const url = String(loginPath || "").trim();
  if (!url) return null;
  return window.open(url, "_blank", terminalWindowFeatures());
}

export function terminalWindowFeatures() {
  const availableWidth = Number(window.screen?.availWidth || window.innerWidth || 1280);
  const availableHeight = Number(window.screen?.availHeight || window.innerHeight || 900);
  const width = Math.min(1400, Math.max(960, Math.floor(availableWidth * 0.88)));
  const height = Math.min(920, Math.max(640, Math.floor(availableHeight * 0.88)));
  const left = Math.max(0, Math.floor((availableWidth - width) / 2));
  const top = Math.max(0, Math.floor((availableHeight - height) / 2));
  return `popup=yes,noopener,noreferrer,width=${width},height=${height},left=${left},top=${top},resizable=yes,scrollbars=yes`;
}

export function closeTerminalDialog(source) {
  const modal = source.closest?.("[data-terminal-modal-layer]");
  if (!modal) return false;
  modal.remove();
  return true;
}

export function hideInlineTerminal(source) {
  const mount = source.closest?.("[data-inline-terminal=\"true\"]");
  if (!mount) return false;
  mount.remove();
  return true;
}

export function closeTerminalModalLayers(root) {
  let closed = false;
  const hosts = [terminalModalHost(root), root.querySelector?.(".content"), root];
  for (const host of hosts) {
    for (const child of Array.from(host?.children || [])) {
      if (child.dataset?.terminalModalLayer === "true") {
        child.remove();
        closed = true;
      }
    }
  }
  return closed;
}

export function renderTerminalButton(kind, id, options = {}) {
  const attribute = kind === "job" ? "data-job-terminal" : "data-terminal";
  const classes = options.iconOnly ? "button secondary terminal-button icon-button" : "button secondary terminal-button";
  const label = "Open terminal";
  const labelAttributes = options.iconOnly
    ? ` aria-label="${escapeAttr(label)}" title="${escapeAttr(label)}"`
    : "";
  const contents = options.iconOnly
    ? `${TERMINAL_ICON}<span class="visually-hidden">${escapeHTML(label)}</span>`
    : "Terminal";
  return `<button class="${classes}" ${attribute}="${escapeAttr(id)}"${labelAttributes}>${contents}</button>`;
}

export function renderTranscriptButton(kind, id, options = {}) {
  const attribute = kind === "job" ? "data-job-transcript" : "data-session-transcript";
  if (!options.iconOnly) {
    return `<button class="button secondary transcript-button" ${attribute}="${escapeAttr(id)}">Transcript</button>`;
  }
  const label = "View transcript";
  return `<button class="button secondary transcript-button icon-button" ${attribute}="${escapeAttr(id)}" aria-label="${escapeAttr(label)}" title="${escapeAttr(label)}">${TRANSCRIPT_ICON}<span class="visually-hidden">${escapeHTML(label)}</span></button>`;
}

export const TERMINAL_ICON = `<svg class="button-icon" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8"/><path d="M12 17v4"/></svg>`;

export const TRANSCRIPT_ICON = `<svg class="button-icon" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 5h16v11H4z"/><path d="M8 19h8"/><path d="M12 16v3"/><path d="M8 9l3 2-3 2"/><path d="M13 13h3"/></svg>`;
