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
import { TASK_TAB_KEYS } from "../config.js";
import { define, FlowElement, mount } from "./base.js";
import "./activity-feed.js";
import "./change.js";
import "./check-list.js";
import "./held-panel.js";
import "./now-card.js";
import "./review-panel.js";
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

// CHANGE_AHEAD_LAG_POLLS bounds how many polls may keep naming the
// pre-adoption head before the ahead-cache exemption expires. A revalidation
// or load can fetch a head the poll has not reported yet; the exemption keeps
// that fresh pair on screen for the adoption repaint plus a short lag window in
// case the poll is about to catch up. A SHA has no ordering, though, so a model
// that keeps naming the pre-adoption head past this window is a rollback/ABA
// rather than lag, and the cache must reset so the current head reloads and the
// adopted head's drafts drop instead of pinning the stale pair forever.
const CHANGE_AHEAD_LAG_POLLS = 1;

export class FlowTaskDetail extends FlowElement {
  diagram = readDiagramMode();
  activePanel = "";
  panelKey = "";
  panelMarkup = "";
  deepLinked = false;
  renderedModel = null;
  renderedChangeKey = "";
  changeGeneration = 0;
  changeKey = "";
  changeData = null;
  changeError = "";
  changePromise = null;
  changeStale = false;
  // changeRetry marks a failed change load that a later same-head task poll
  // should retry. loadChange failure leaves changeData null and changeError
  // set, so render() cannot mark the cache stale — there is no cached pair to
  // revalidate — and without this marker paintChange's error branch keeps the
  // error card until the user clicks Retry or the head changes. A same-key
  // poll sets it; paintChange clears it when it retries the load, and
  // resetChangeLoad clears it with the rest of the cache.
  changeRetry = false;
  // changeAheadKey names the model head the cache was ahead of when it fetched
  // a head the poll has not reported yet (a revalidation or load fetched
  // ahead). While the model still names that exact head, a repaint must not
  // throw the fresh pair away. It is cleared as soon as the model catches up
  // (or moves past it), so a later head the ahead cache does not cover reloads
  // the change instead of being pinned behind a stale ahead flag.
  changeAheadKey = "";
  // changeAheadSeen counts the polls that have kept naming changeAheadKey since
  // the cache moved ahead of the model. It bounds the exemption: the adoption
  // repaint plus CHANGE_AHEAD_LAG_POLLS polls may keep the fresh pair, but a
  // model that keeps naming the pre-adoption head past that window is a
  // rollback/ABA, not lag, and the cache resets to reload the current head.
  changeAheadSeen = 0;
  // changePendingKey names the head an in-flight revalidation has fetched
  // metadata for but not yet verified a diff against. It is set only while
  // revalidateChange awaits the moved head's /diff, and carries no data — the
  // unverified head is never rendered. Its sole job is to tell a task poll that
  // reports that exact head, in the window before the diff verifies, to keep the
  // still-coherent prior pair instead of resetting it: a reset would flash
  // "Loading change", start a second full change+diff load, and invalidate the
  // in-flight revalidation by generation. The moment the pair verifies,
  // adoptChangeHead re-keys the cache (and the ahead window takes over); if the
  // attempt gives up, the marker clears and the cache stays stale for the next
  // poll. A different head, or the lag window expiring, resets the cache.
  changePendingKey = "";
  // changePendingSeen records that a task poll observed the pending head while
  // its diff was still verifying. Once the pending head has been observed, a
  // later poll that diverges back to the cached head is a rollback: the pending
  // head is no longer current, so the stale in-flight revalidation must not
  // adopt it over the model's head when its diff finally lands. paintChange
  // clears the pending marker on that divergent poll (which revalidateChange
  // checks before adopting); the coherent pair stays on screen, and the cache
  // stays stale so the next poll revalidates the current head.
  changePendingSeen = false;
  terminalGeneration = 0;
  terminalKey = "";
  terminalLoginPath = "";
  terminalError = "";
  terminalPromise = null;

  render(model) {
    if (!model) return `<div class="empty">Loading task</div>`;
    const changeKey = changeModelKey(model);
    if (changeKey === this.renderedChangeKey) {
      // A fresh model for the same task/change/head is a poll or a refresh.
      // The cached change predates it, so the next Change-tab paint
      // revalidates the cache in place rather than serving it forever. A cache
      // that already fetched ahead of the poll is fresh; leave it alone.
      // paintChange clears changeAheadKey once the model catches up (or moves
      // past it), and bounds how long a pre-adoption head may keep the ahead
      // cache, so subsequent same-head polls revalidate normally and a
      // persistent rollback eventually reloads the current head.
      if (this.changeData && !this.changeAheadKey) this.changeStale = true;
      // A failed load has no pair to revalidate, but the same-key poll still
      // means the failure may be transient: mark it for a retry on the next
      // Change-tab paint instead of leaving the error card up until the user
      // clicks Retry or the head changes.
      if (this.changeError && !this.changeData) this.changeRetry = true;
    } else {
      this.renderedChangeKey = changeKey;
      // A different task/change/head normally resets the cache. A cache that
      // fetched ahead of the poll is fresh and survives — but only while the
      // ahead window is open; the reconciliation below expires it for a
      // persistent rollback. A revalidation that has fetched a new head's
      // metadata but not yet verified its diff (changePendingKey) likewise
      // survives a poll reporting that head until the diff lands.
      if (!((this.changeAheadKey || this.changePendingKey) && this.changeData)) this.resetChangeLoad();
    }
    // While the Change tab is active, paintChange reconciles the cached pair
    // against the model head on every paint (and resets it when it no longer
    // matches). While another tab is open, paintChange never runs, so reconcile
    // here instead — on every poll, same-key or not: a moved-head revalidation
    // can be verifying its diff, or have adopted a head the poll has not
    // reported, while the tab is hidden, and the pending/ahead state (including
    // a rollback back to the cached head, and the bounded ahead window for a
    // persistent pre-adoption head) must be honoured so a stale revalidation
    // cannot later adopt a head the model has already left. The cache survives
    // only when it still matches this head; otherwise it resets, dropping
    // old-head drafts.
    if (this.querySelector("flow-tab-strip")?.active !== "change" && !this.reconcileChangeHead(model)) {
      this.resetChangeLoad();
    }
    this.renderedModel = model;
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

  // Poll-fresh models must reach the children even though the shell markup is
  // stable: the base paint skips unchanged markup wholesale, so the children
  // sync on every paint attempt, not just on writes.
  paint() {
    const hadShell = this.renderedModel != null;
    super.paint();
    if (hadShell) this.syncChildren();
  }

  bind() {
    this.addEventListener("tab-change", () => this.paintPanel());
  }

  afterPaint() {
    this.syncChildren();
  }

  syncChildren() {
    const model = this.data;
    if (!model) return;
    const rail = this.querySelector("flow-task-rail");
    if (!rail) return;
    rail.data = model;
    this.querySelector("flow-held-panel").data = model;
    this.querySelector("flow-now-card").data = { card: nowCardModel(model), model };
    const strip = this.querySelector("flow-tab-strip");
    strip.data = { badges: tabBadges(model) };
    if (!this.deepLinked) {
      this.deepLinked = true;
      const wanted = new URLSearchParams(window.location.search).get("tab");
      if (wanted && TASK_TAB_KEYS.has(wanted)) strip.select(wanted);
    }
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
    if (active !== this.activePanel) {
      this.panelKey = "";
      this.panelMarkup = "";
    }
    this.activePanel = active;
    panel.dataset.tab = active;

    switch (active) {
      case "review":
        mount(panel, "flow-review-panel", model);
        break;
      case "overview":
        this.paintOverviewPanel(model, panel);
        break;
      case "checks":
        mount(panel, "flow-check-list", model);
        break;
      case "activity":
        mount(panel, "flow-activity-feed", model);
        break;
      case "change":
        this.paintChange(model, panel);
        break;
      case "terminal":
        this.paintTerminal(model, panel);
        break;
      default:
        this.paintStatic(panel, "detail", this.detailMarkup(model));
    }
  }

  // paintStatic swaps panel markup only when it actually changed, so a poll
  // never clobbers element state — an open transcript, a draft review note, a
  // terminal frame — with an identical rewrite.
  paintStatic(panel, key, markup) {
    if (this.panelKey === key && this.panelMarkup === markup) return false;
    panel.innerHTML = markup;
    this.panelKey = key;
    this.panelMarkup = markup;
    return true;
  }

  paintOverviewPanel(model, panel) {
    this.paintStatic(panel, "overview", this.overviewMarkup(model));
    const host = panel.querySelector(".diagram");
    if (!host) return;
    const tag = this.diagram === "graph" ? "flow-workflow-graph" : "flow-run-list";
    mount(host, tag, this.diagram === "graph" ? model : model.rows);
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

  resetChangeLoad(key = "") {
    this.changeGeneration += 1;
    this.changeKey = key;
    this.changeData = null;
    this.changeError = "";
    this.changePromise = null;
    this.changeStale = false;
    this.changeRetry = false;
    this.changeAheadKey = "";
    this.changeAheadSeen = 0;
    this.changePendingKey = "";
    this.changePendingSeen = false;
  }

  paintChange(model, panel) {
    const change = model.change;
    if (!change) {
      this.paintStatic(panel, "change:empty", `<p class="empty">No change yet</p>`);
      return;
    }
    const id = String(value(change, "id", "ID") || "");
    const head = String(value(change, "head_sha", "HeadSHA") || "");
    const modelKey = `${id}:${head}`;
    // Reconcile the cached pair against the model head before deciding whether
    // to keep it. This is shared with render(), which runs it for polls that
    // arrive while another tab is open, so a moved-head revalidation that is
    // verifying its diff — or has adopted a head the poll has not reported —
    // is honoured on every poll (including a rollback back to the cached head,
    // and the bounded ahead window for a persistent pre-adoption head). When
    // the cache no longer matches this head, it resets below and reloads,
    // dropping old-head drafts.
    if (!this.reconcileChangeHead(model)) {
      // A different change, a genuine later head the cache never fetched, or a
      // rollback that outlasted the ahead window: reload and drop old-head
      // drafts.
      this.resetChangeLoad(modelKey);
    }
    const key = this.changeKey;

    if (this.changeData) {
      if (this.panelKey !== `change:${key}:data`) {
        panel.innerHTML = `<flow-change></flow-change>`;
        this.panelKey = `change:${key}:data`;
        this.panelMarkup = "";
      }
      // Reusing the element keeps the selected file and the reviewer's pending
      // inline notes; the element's own paint skips unchanged markup. A poll
      // marks the cache stale (render), so revalidate it in place — the fresh
      // copy lands on this same element, not a rebuilt one.
      panel.firstElementChild.data = this.changeData;
      if (this.changeStale && !this.changePromise) {
        const cachedHead = String(value(this.changeData.change || {}, "head_sha", "HeadSHA") || "");
        this.revalidateChange(id, key, cachedHead);
      }
      return;
    }
    if (this.changeError) {
      // A later same-head poll marked the failed load for retry: drop the
      // error and fall through to a fresh loadChange for the same key. The
      // fetched pair is verified before installing, and a head move between
      // polls is caught by reconcile above, so the retry cannot mix heads.
      if (this.changeRetry) {
        this.changeError = "";
        this.changeRetry = false;
      } else {
        this.paintStatic(panel, `change:${key}:error`, `<div class="empty">${escapeHTML(this.changeError)} <button class="button secondary" type="button" data-change-retry>Retry</button></div>`);
        return;
      }
    }
    this.paintStatic(panel, `change:${key}:loading`, `<div class="empty">Loading change</div>`);
    if (!this.changePromise) this.loadChange(id, key);
  }

  // reconcileChangeHead keeps the cached change/diff pair honest against the
  // model's head and reports whether the cache may be kept. It runs on every
  // model poll — from paintChange when the Change tab is active, and from
  // render() for polls that arrive while another tab is open — so a moved-head
  // revalidation that is verifying its diff (or has adopted a head the poll has
  // not reported) is reconciled on every poll regardless of the active tab.
  // Without that, polls delivered to a hidden tab would skip the pending
  // observation and the rollback invalidation, and a stale revalidation could
  // adopt a head the model has already rolled back from. It never loads or
  // repaints; it only adjusts the cache markers. It returns true when the cache
  // still matches the model head (caught up, pending, or inside the ahead
  // window) and false when it must be reset and reloaded.
  reconcileChangeHead(model) {
    const change = model.change;
    if (!change) return false;
    const id = String(value(change, "id", "ID") || "");
    if (!id) return false;
    const modelKey = `${id}:${String(value(change, "head_sha", "HeadSHA") || "")}`;
    if (modelKey === this.changeKey) {
      // The model has caught up to the cached head. If a moved-head
      // revalidation is still verifying a different head that a poll already
      // reported, the model has since diverged back to the cached head — a
      // rollback. Clear the pending marker so the stale revalidation bails
      // instead of adopting the head the model has left. The generation is
      // deliberately not bumped: the cached pair is still coherent for this
      // head, so it stays on screen, and the revalidation's finally block still
      // clears changePromise. The cache stays stale, so the next same-head poll
      // revalidates the current head. A poll that never reported the pending
      // head leaves the marker alone.
      if (this.changePendingKey && this.changePendingSeen) {
        this.changePendingKey = "";
        this.changePendingSeen = false;
      }
      this.changeAheadKey = "";
      this.changeAheadSeen = 0;
      return true;
    }
    if (this.changePendingKey === modelKey && Boolean(this.changeData)) {
      // The poll reports exactly the head a moved-head revalidation is
      // verifying. Keep the prior coherent pair — no reset, no reload, no flash
      // — and record that this head was observed, so a later poll that diverges
      // back to the cached head is recognized as a rollback and invalidates the
      // pending work. The marker clears the moment the pair verifies
      // (adoptChangeHead) or the attempt gives up.
      this.changePendingSeen = true;
      return true;
    }
    if (this.changeAheadKey === modelKey && this.changeAheadSeen <= CHANGE_AHEAD_LAG_POLLS && Boolean(this.changeData)) {
      // The cache sits one verified head ahead of this exact model head (a
      // revalidation/load adopted it before the poll reported it). Keep the
      // fresh pair for the adoption repaint plus a bounded lag window; a SHA
      // has no ordering, so a model that keeps naming the pre-adoption head
      // past that window is a rollback/ABA and falls through to a reset.
      this.changeAheadSeen += 1;
      return true;
    }
    return false;
  }

  // loadChange fetches the change and its diff as one consistent pair. The
  // change can advance between the two GETs, and /diff answers for the head
  // the server then holds — installing that diff under the earlier metadata
  // would show the new head's code under the old head's name, and let a
  // verdict target code the reviewer never saw. A pair only installs once it
  // is verified for one head: the metadata must name this change, and the diff
  // must name the metadata's head. The server's explicit no-diff response (a
  // 200 with no files when a diff is unavailable) still names that head, so it
  // installs as an empty diff. A headless or wrong-change metadata response, a
  // failed diff fetch, or a headless/mismatched diff is retried (up to three
  // reads); a head that keeps moving — or a response that never verifies —
  // fails with a retryable error instead of installing an unverified pair.
  // When the pair lands for a head the poll has not reported yet, the cache key
  // advances to that head so the poll that does report it finds a matching key
  // and skips a second reload and its "Loading change" flash.
  loadChange(id, key) {
    const generation = this.changeGeneration;
    this.changePromise = (async () => {
      try {
        let loaded = null;
        for (let attempt = 0; attempt < 3 && !loaded; attempt += 1) {
          const data = await apiGet(`/v2/changes/${encodeURIComponent(id)}`);
          if (generation !== this.changeGeneration || key !== this.changeKey) return;
          const change = value(data, "change", "Change") || {};
          const changeID = String(value(change, "id", "ID") || "");
          const headSHA = String(value(change, "head_sha", "HeadSHA") || "");
          // Metadata that does not name this change, or names no head, cannot
          // anchor a verified pair; skip the diff fetch and retry the read.
          if (changeID !== id || !headSHA) continue;
          const diff = await apiGet(`/v2/changes/${encodeURIComponent(id)}/diff`).catch(() => null);
          if (generation !== this.changeGeneration || key !== this.changeKey) return;
          const diffHead = String(value(diff, "head_sha", "HeadSHA") || "");
          // Only a verified diff installs: one naming the metadata's head. A
          // failed fetch, a headless diff, or one for another head verifies
          // nothing and is retried.
          if (!diff || diffHead !== headSHA) continue;
          loaded = { data, diff, headSHA };
        }
        if (!loaded) throw new Error("The change advanced while it was loading");
        this.changeData = { ...loaded.data, diff: loaded.diff };
        if (loaded.headSHA !== key.split(":").pop()) this.adoptChangeHead(id, loaded.headSHA, key);
      } catch (error) {
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        this.changeError = error.message || String(error);
      } finally {
        if (generation !== this.changeGeneration) return;
        this.changePromise = null;
        if (this.isConnected && this.querySelector("flow-tab-strip")?.active === "change") this.paintPanel();
      }
    })();
  }

  // revalidateChange freshens the cached change behind the visible Change
  // tab. The poll reported the head this cache belongs to, and the diff is
  // keyed by that head, so while the change still sits there only the change
  // itself is re-fetched, and the result lands on the existing element in
  // place. A failure keeps the cached copy on screen; the next poll marks the
  // cache stale again and retries.
  //
  // A change that advanced between the task poll and this fetch comes back
  // with a NEW head, and installing it over the cached diff would show the
  // old head's code under the new head's name — and let a verdict target code
  // the reviewer never saw. A moved head instead fetches a consistent
  // change+diff pair for the new head and re-keys the cache to it: the rebuild
  // drops drafts anchored to the old head's lines, and the poll that reports
  // that head finds a matching key, so it neither reloads nor flashes
  // "Loading change". The adoption happens only once the refreshed metadata
  // names this change at the new head AND the diff verifies against that head
  // (or names the head on an explicit, successful no-diff response). A
  // headless or wrong-change metadata response, or a missing, headless, or
  // mismatched diff, keeps the prior coherent pair in place and the cache
  // stale, so the next poll retries the revalidation; adopting on metadata
  // alone would let the matching poll's ahead-key suppression skip the
  // recovery load and render the new head's metadata under the old head's
  // diff.
  revalidateChange(id, key, head) {
    const generation = this.changeGeneration;
    this.changeStale = false;
    this.changePromise = (async () => {
      try {
        const data = await apiGet(`/v2/changes/${encodeURIComponent(id)}`);
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        const change = value(data, "change", "Change") || {};
        const changeID = String(value(change, "id", "ID") || "");
        const fetchedHead = String(value(change, "head_sha", "HeadSHA") || "");
        // A response that does not name this change, or names no head, cannot
        // replace the cached pair: keep it and retry on the next poll.
        if (changeID !== id || !fetchedHead) return;
        if (fetchedHead === head) {
          // Same head: refresh the metadata around the cached diff, which was
          // verified for this head when the pair installed.
          this.changeData = { ...data, diff: this.changeData?.diff || {} };
          return;
        }
        // The metadata reports a newer head, but the diff that proves it is still
        // in flight. Record the target head WITHOUT installing its metadata: a
        // task poll that reports this head before the diff verifies must keep the
        // coherent prior pair instead of resetting it (which would flash "Loading
        // change", start a second full load, and invalidate this revalidation by
        // generation). The marker carries no data, so an unverified head is never
        // rendered; it clears below the moment the pair verifies (adoptChangeHead
        // takes over) or the attempt gives up.
        this.changePendingKey = `${id}:${fetchedHead}`;
        this.changePendingSeen = false;
        const diff = await apiGet(`/v2/changes/${encodeURIComponent(id)}/diff`).catch(() => null);
        if (generation !== this.changeGeneration || key !== this.changeKey) return;
        const diffHead = String(value(diff, "head_sha", "HeadSHA") || "");
        // Only adopt a verified pair: a diff naming the metadata's head (the
        // server's explicit no-diff response still names it). A failed fetch, a
        // headless diff, or one for yet another head (the change moved again)
        // keeps the prior pair — still verified for its own head — and leaves
        // the cache stale for the next poll rather than mixing two heads on
        // screen. A poll that observed this pending head and then diverged back
        // to the cached head (a rollback) cleared the marker in paintChange, so
        // the stale revalidation bails here instead of adopting a head the model
        // has since left. Either way the pending marker has served its purpose.
        if (!this.changePendingKey) return;
        this.changePendingKey = "";
        this.changePendingSeen = false;
        if (!diff || diffHead !== fetchedHead) return;
        this.changeData = { ...data, diff };
        this.adoptChangeHead(id, fetchedHead, key);
      } catch {
        // Cached data beats an error flash for a background revalidation.
      } finally {
        if (generation !== this.changeGeneration) return;
        this.changePromise = null;
        if (this.isConnected && this.querySelector("flow-tab-strip")?.active === "change") this.paintPanel();
      }
    })();
  }

  // adoptChangeHead records that the cached change/diff pair belongs to a
  // (possibly newer) head than the one the key named. It re-keys the cache
  // without touching the cached data, generation, or in-flight promise, and
  // records the pre-adoption model head (key) so a repaint that still carries
  // that exact head keeps the fresh pair instead of resetting it. The ahead
  // window starts fresh here: changeAheadSeen counts the polls that keep naming
  // the pre-adoption head, so a persistent rollback eventually expires it.
  adoptChangeHead(id, head, key) {
    const next = `${id}:${head}`;
    if (next === key) return;
    this.changeKey = next;
    this.renderedChangeKey = changeModelKey({ id: this.data?.id, change: { id, head_sha: head } });
    this.changeAheadKey = key;
    this.changeAheadSeen = 0;
    // An adopted head is verified, so it is no longer pending.
    this.changePendingKey = "";
    this.changePendingSeen = false;
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
      this.paintStatic(panel, "terminal:none", `<p class="empty">No live terminal</p>`);
      return;
    }
    const key = `${target.kind}:${target.id}`;
    if (key !== this.terminalKey) this.resetTerminalLoad(key);

    if (this.terminalLoginPath) {
      if (this.panelKey !== `terminal:${key}:ready`) {
        panel.innerHTML = renderTaskTerminal(target, this.terminalLoginPath);
        this.panelKey = `terminal:${key}:ready`;
        this.panelMarkup = "";
      }
      return;
    }
    if (this.terminalError) {
      this.paintStatic(panel, `terminal:${key}:error`, `<div class="empty">${escapeHTML(this.terminalError)} <button class="button secondary" type="button" data-terminal-retry>Retry</button></div>`);
      return;
    }
    this.paintStatic(panel, `terminal:${key}:connecting`, `<div class="empty">Connecting terminal</div>`);
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
