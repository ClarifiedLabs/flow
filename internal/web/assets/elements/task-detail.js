// Task detail. Two parts: a context rail that never scrolls away, and one work
// surface with the Now card above the tabs.
//
// This replaces a single-column stack of eight sections that required heavy
// scrolling to find anything. Everything that was a section is now either
// pinned (the rail), always-visible-when-relevant (the Now card), or behind a
// tab that remembers where you were.

import { apiGet, apiPost } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { nowCardModel, tabBadges } from "../task-model.js";
import { readDiagramMode, writeDiagramMode } from "../storage.js";
import { renderTerminalPopOutButton, terminalSelectionHint } from "../terminal.js";
import { define, FlowElement } from "./base.js";
import "./activity-feed.js";
import "./change.js";
import "./check-list.js";
import "./held-panel.js";
import "./now-card.js";
import "./run-list.js";
import "./tab-strip.js";
import "./task-rail.js";
import "./workflow-graph.js";

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

export class FlowTaskDetail extends FlowElement {
  diagram = readDiagramMode();
  activePanel = "";
  renderedModel = null;
  changeGeneration = 0;
  changeKey = "";
  changeData = null;
  changeError = "";
  changePromise = null;
  terminalGeneration = 0;
  terminalKey = "";
  terminalLoginPath = "";
  terminalError = "";
  terminalPromise = null;

  render(model) {
    if (!model) return `<div class="empty">Loading task</div>`;
    if (model !== this.renderedModel) {
      this.renderedModel = model;
      this.resetChangeLoad();
    }
    return `
      <flow-task-rail></flow-task-rail>
      <div class="surface">
        <flow-held-panel></flow-held-panel>
        <flow-now-card></flow-now-card>
        <flow-tab-strip></flow-tab-strip>
        <div class="panel" role="tabpanel"></div>
      </div>
    `;
  }

  bind() {
    this.addEventListener("tab-change", () => this.paintPanel());
  }

  afterPaint() {
    const model = this.data;
    if (!model) return;
    this.querySelector("flow-task-rail").data = model;
    this.querySelector("flow-held-panel").data = model;
    this.querySelector("flow-now-card").data = { card: nowCardModel(model), model };
    this.querySelector("flow-tab-strip").data = { badges: tabBadges(model) };
    this.paintPanel();
  }

  handleClick(event) {
    const retryChange = event.target.closest?.("[data-change-retry]");
    if (retryChange) {
      event.preventDefault();
      this.changeError = "";
      this.paintPanel();
      return;
    }
    const retryTerminal = event.target.closest?.("[data-terminal-retry]");
    if (retryTerminal) {
      event.preventDefault();
      this.terminalError = "";
      this.paintPanel();
      return;
    }
    const focus = event.target.closest?.("[data-focus-tab]");
    if (focus) {
      event.preventDefault();
      this.querySelector("flow-tab-strip")?.select(focus.dataset.focusTab);
      return;
    }
    const diagram = event.target.closest?.("[data-diagram]");
    if (diagram) {
      event.preventDefault();
      this.diagram = diagram.dataset.diagram;
      writeDiagramMode(this.diagram);
      this.paintPanel();
    }
  }

  paintPanel() {
    const model = this.data;
    const panel = this.querySelector(".panel");
    if (!model || !panel) return;
    const active = this.querySelector("flow-tab-strip")?.active || "overview";
    if (this.activePanel === "terminal" && active !== "terminal") {
      // Login URLs are short-lived. Discard one after its iframe is removed so
      // returning to the tab always mints fresh terminal access.
      this.resetTerminalLoad(this.terminalKey);
    }
    this.activePanel = active;
    panel.dataset.tab = active;

    switch (active) {
      case "overview":
        panel.innerHTML = this.overviewMarkup(model);
        this.paintOverview(model);
        break;
      case "checks":
        panel.innerHTML = `<flow-check-list></flow-check-list>`;
        panel.firstElementChild.data = model;
        break;
      case "activity":
        panel.innerHTML = `<flow-activity-feed></flow-activity-feed>`;
        panel.firstElementChild.data = model;
        break;
      case "change":
        this.paintChange(model, panel);
        break;
      case "terminal":
        this.paintTerminal(model, panel);
        break;
      default:
        panel.innerHTML = this.detailMarkup(model);
    }
  }

  overviewMarkup(model) {
    return `
      <section class="section">
        <div class="section-head">
          <h3>Workflow</h3>
          <span class="spacer"></span>
          <div class="segmented" role="group" aria-label="Diagram">
            <button data-diagram="run"${this.diagram === "run" ? ' aria-pressed="true"' : ""}>Run</button>
            <button data-diagram="graph"${this.diagram === "graph" ? ' aria-pressed="true"' : ""}>Graph</button>
          </div>
        </div>
        <div class="diagram"></div>
      </section>
      <section class="section">
        <h3>Requirements</h3>
        <div class="requirements prose">${model.body ? renderMarkdown(model.body) : `<p class="empty">No description</p>`}</div>
      </section>
    `;
  }

  paintOverview(model) {
    const host = this.querySelector(".diagram");
    if (!host) return;
    const tag = this.diagram === "graph" ? "flow-workflow-graph" : "flow-run-list";
    host.innerHTML = `<${tag}></${tag}>`;
    host.firstElementChild.data = this.diagram === "graph" ? model : model.rows;
  }

  resetChangeLoad(key = "") {
    this.changeGeneration += 1;
    this.changeKey = key;
    this.changeData = null;
    this.changeError = "";
    this.changePromise = null;
  }

  paintChange(model, panel) {
    const change = model.change;
    if (!change) {
      panel.innerHTML = `<p class="empty">No change yet</p>`;
      return;
    }
    const id = String(value(change, "id", "ID") || "");
    const head = String(value(change, "head_sha", "HeadSHA") || "");
    const key = `${id}:${head}`;
    if (key !== this.changeKey) this.resetChangeLoad(key);

    if (this.changeData) {
      panel.innerHTML = `<flow-change></flow-change>`;
      panel.firstElementChild.data = this.changeData;
      return;
    }
    if (this.changeError) {
      panel.innerHTML = `<div class="empty">${escapeHTML(this.changeError)} <button class="button secondary" type="button" data-change-retry>Retry</button></div>`;
      return;
    }
    panel.innerHTML = `<div class="empty">Loading change</div>`;
    if (!this.changePromise) this.loadChange(id, key);
  }

  loadChange(id, key) {
    const generation = this.changeGeneration;
    this.changePromise = (async () => {
      try {
        const data = await apiGet(`/v2/changes/${encodeURIComponent(id)}`);
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        const change = value(data, "change", "Change") || {};
        const headSHA = value(change, "head_sha", "HeadSHA");
        const diff = headSHA
          ? await apiGet(`/v2/changes/${encodeURIComponent(id)}/diff`).catch(() => null)
          : null;
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        this.changeData = { ...data, diff: diff || {} };
      } catch (error) {
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        this.changeError = error.message || String(error);
      } finally {
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        this.changePromise = null;
        if (this.isConnected && this.querySelector("flow-tab-strip")?.active === "change") this.paintPanel();
      }
    })();
  }

  resetTerminalLoad(key) {
    this.terminalGeneration += 1;
    this.terminalKey = key;
    this.terminalLoginPath = "";
    this.terminalError = "";
    this.terminalPromise = null;
  }

  paintTerminal(model, panel) {
    const target = taskTerminalTarget(model);
    if (!target) {
      panel.innerHTML = `<p class="empty">No live terminal</p>`;
      return;
    }
    const key = `${target.kind}:${target.id}`;
    if (key !== this.terminalKey) this.resetTerminalLoad(key);

    if (this.terminalLoginPath) {
      panel.innerHTML = renderTaskTerminal(target, this.terminalLoginPath);
      return;
    }
    if (this.terminalError) {
      panel.innerHTML = `<div class="empty">${escapeHTML(this.terminalError)} <button class="button secondary" type="button" data-terminal-retry>Retry</button></div>`;
      return;
    }
    panel.innerHTML = `<div class="empty">Connecting terminal</div>`;
    if (!this.terminalPromise) this.loadTerminal(target, key);
  }

  loadTerminal(target, key) {
    const generation = this.terminalGeneration;
    this.terminalPromise = (async () => {
      try {
        const path = target.kind === "job"
          ? `/v2/jobs/${encodeURIComponent(target.id)}/terminal-token`
          : `/v2/sessions/${encodeURIComponent(target.id)}/terminal-token`;
        const data = await apiPost(path, {});
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
        if (this.isConnected && this.querySelector("flow-tab-strip")?.active === "terminal") this.paintPanel();
      }
    })();
  }

  detailMarkup(model) {
    const rows = [
      ["Task", model.id],
      ["Project", model.projectName],
      ["State", String(model.lifecycleState).replaceAll("_", " ")],
      ["Priority", `p${model.priority}`],
      ["Created by", model.createdBy],
      ["Run", model.runSequence ? `run ${model.runSequence} · ${model.runState}` : ""],
    ].filter(([, fact]) => fact);
    return `
      <dl class="facts">
        ${rows.map(([label, fact]) => `<div><dt>${escapeHTML(label)}</dt><dd>${escapeHTML(fact)}</dd></div>`).join("")}
      </dl>
    `;
  }
}

define("flow-task-detail", FlowTaskDetail);
