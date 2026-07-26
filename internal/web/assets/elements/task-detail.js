// Task detail. Two parts: a context rail that never scrolls away, and one work
// surface with the Now card above the tabs.
//
// This replaces a single-column stack of eight sections that required heavy
// scrolling to find anything. Everything that was a section is now either
// pinned (the rail), always-visible-when-relevant (the Now card), or behind a
// tab that remembers where you were.

import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { nowCardModel, tabBadges } from "../task-model.js";
import { readDiagramMode, writeDiagramMode } from "../storage.js";
import { define, FlowElement } from "./base.js";
import "./activity-feed.js";
import "./check-list.js";
import "./held-panel.js";
import "./now-card.js";
import "./run-list.js";
import "./tab-strip.js";
import "./task-rail.js";
import "./workflow-graph.js";

export class FlowTaskDetail extends FlowElement {
  diagram = readDiagramMode();

  render(model) {
    if (!model) return `<div class="empty">Loading task</div>`;
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
        panel.innerHTML = this.changeMarkup(model);
        break;
      case "terminal":
        panel.innerHTML = this.terminalMarkup(model);
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

  changeMarkup(model) {
    const change = model.change;
    if (!change) return `<p class="empty">No change yet</p>`;
    const id = value(change, "id", "ID");
    return `
      <div class="change-link">
        <a class="button" href="/ui/changes/${escapeAttr(id)}" data-link>Open ${escapeHTML(id)}</a>
        <span class="quiet">${escapeHTML(value(change, "branch", "Branch") || "")}</span>
      </div>
    `;
  }

  terminalMarkup(model) {
    if (!model.terminalAvailable) return `<p class="empty">No live terminal</p>`;
    const sessionID = value(model.activeSession || {}, "id", "ID");
    const target = sessionID
      ? ` data-terminal="${escapeAttr(sessionID)}"`
      : ` data-job-terminal="${escapeAttr(model.terminalJobID)}"`;
    return `<div class="terminal-host" data-inline-terminal-anchor><button class="button"${target}>Open terminal</button></div>`;
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
