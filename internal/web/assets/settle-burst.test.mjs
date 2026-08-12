// Settle-burst tests, ported from app.test.mjs when the state machine moved
// to app/settle.js. The harness still drives a real FlowApp — FlowApp wires
// the SettleBurst through its load ports, so the app-level assertions
// exercise the class through the exact ports production uses.

import assert from "node:assert/strict";
import { test } from "node:test";
import { actionScope, handleAction, inFlight } from "./actions.js";
import { ActionButton, consoleDocument, consoleImports, deferred, mountableContent, scriptContext } from "./test-helpers.mjs";

// A settle-burst harness: a real FlowApp with a recording setTimeout and a
// stub load() that counts invocations and mimics the real load's contract —
// the generation moves synchronously as the load starts and the returned
// context carries the path the load started on — so refresh() and the burst
// ticks observe the same load-generation movement as with the real load. The
// status line is stubbed because handleAction writes to it.
async function settleBurstHarness(pathname = "/ui/board") {
  const timers = [];
  const cleared = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout(id) {
      cleared.push(id);
    },
  });
  context.window.location.pathname = pathname;
  const app = new context.FlowApp();
  app.pollingActive = true;
  const status = { textContent: "" };
  app.querySelector = (selector) => (selector === ".status" ? status : null);
  let loads = 0;
  app.load = async (options = {}) => {
    loads += 1;
    // Mirror the real load's burst supersession (see FlowApp.load()): a load
    // that is not the active burst's own reload cancels the pending
    // settle-burst timeout and retires the burst identity.
    app.settle.cancelUnless(options.burst);
    const loadContext = {
      generation: (app.loadGeneration || 0) + 1,
      path: context.window.location.pathname,
    };
    app.loadGeneration = loadContext.generation;
    return loadContext;
  };
  return {
    app,
    context,
    status,
    timers,
    cleared,
    loads: () => loads,
    // actionRefresh runs a refresh the way an action handler does: the
    // dispatcher hands the handler an action-scoped app whose refresh carries
    // the ACTION_SETTLE provenance token — which is how refresh() tells an
    // action-triggered refresh (arm the settle burst) from an ordinary one
    // (stay one load).
    async actionRefresh() {
      await actionScope(app).refresh();
    },
    // fire runs a pending timer callback the way the browser would — Poller's
    // wrapper invokes the async burst tick without awaiting it — then flushes
    // the microtask queue so the tick settles.
    async fire(index) {
      timers[index].callback();
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

test("a successful action arms a bounded settle burst of follow-up reloads", async () => {
  const harness = await settleBurstHarness();
  globalThis.fetch = (path) => {
    assert.equal(path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/schedule");
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay, secondDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  assert.equal(harness.status.textContent, "Scheduled");
  assert.equal(harness.loads(), 1, "the action triggers the immediate refresh");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.loads(), 2, "the first burst tick reloads the route");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");
  // Delays are absolute offsets from the action's refresh; the one-shot
  // Poller re-arms per tick, so the second arm waits out only the delta.
  assert.equal(harness.timers[1].delay, secondDelay - firstDelay);

  await harness.fire(1);
  assert.equal(harness.loads(), 3, "the second burst tick reloads the route");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("navigating away before the burst fires cancels the pending settle-burst timeout", async () => {
  const harness = await settleBurstHarness("/ui/board");
  await harness.actionRefresh();
  assert.equal(harness.loads(), 1);
  assert.equal(harness.timers.length, 1);
  assert.equal(harness.app.settle.poll.timer, 1, "the burst tick is pending before navigation");

  // Opening another route starts a newer load through the same load() the nav
  // click, popstate, and shortcut handlers call: the pending burst timeout is
  // cancelled outright — not left live until it fires — and the burst
  // identity is retired.
  harness.context.window.location.pathname = "/ui/jobs";
  await harness.app.load();
  assert.equal(harness.loads(), 2);
  assert.deepEqual(harness.cleared, [1], "navigation cancels the pending settle-burst timeout");
  assert.equal(harness.app.settle.poll.timer, 0, "no settle timer is left armed after navigation");

  // Even if the browser had already queued the cancelled callback, it neither
  // reloads the new route nor re-arms another tick.
  await harness.fire(0);
  assert.equal(harness.loads(), 2, "the cancelled burst tick never reloads the new route");
  assert.equal(harness.timers.length, 1, "the burst ends instead of arming another tick");
});

test("disconnect cancels every pending settle-burst timeout", async () => {
  const harness = await settleBurstHarness();
  await harness.actionRefresh();
  assert.equal(harness.timers.length, 1);

  harness.app.disconnectedCallback();
  assert.deepEqual(harness.cleared, [1], "the pending burst timer is cancelled on disconnect");

  // A callback already queued in the browser when the disconnect landed must
  // stay inert: it neither reloads nor re-arms after the app went away.
  harness.timers[0].callback();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.loads(), 1, "the disconnected burst tick does not reload");
  assert.equal(harness.timers.length, 1, "the disconnected burst tick re-arms nothing");
});

test("navigating while a burst tick awaits its reload ends the burst", async () => {
  const harness = await settleBurstHarness("/ui/board");
  await harness.actionRefresh();
  assert.equal(harness.timers.length, 1);

  const gate = deferred();
  const baseLoad = harness.app.load;
  let hold = true;
  harness.app.load = async (options) => {
    const loadContext = await baseLoad(options);
    if (hold) {
      hold = false;
      await gate.promise;
    }
    return loadContext;
  };

  harness.fire(0);
  harness.context.window.location.pathname = "/ui/jobs";
  await harness.app.load();
  gate.resolve();
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(harness.timers.length, 1, "the superseded tick arms no timer on the new route");
  assert.equal(harness.loads(), 3, "the burst adds no reload beyond its own superseded tick");
});

test("an older burst tick awaiting its reload cannot displace a newer burst's timer", async () => {
  const harness = await settleBurstHarness();
  await harness.actionRefresh();
  assert.equal(harness.timers.length, 1, "the first burst arms its first tick");

  // Hold the first burst's tick in flight so the second action schedules its
  // burst while the older tick is still awaiting its reload — the race that
  // used to let the older continuation overwrite settlePoll's timer handle
  // and orphan the newer burst's timeout.
  const gate = deferred();
  const baseLoad = harness.app.load;
  let hold = true;
  harness.app.load = async (options) => {
    const loadContext = await baseLoad(options);
    if (hold) {
      hold = false;
      await gate.promise;
    }
    return loadContext;
  };

  harness.fire(0);
  await harness.actionRefresh();
  assert.equal(harness.timers.length, 2, "the newer burst arms its first tick");
  assert.equal(harness.app.settle.poll.timer, 2, "the newer burst owns the settle timer");

  gate.resolve();
  // Flush past the microtask queue so the superseded continuation has run.
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.app.settle.poll.timer, 2, "the superseded continuation leaves the newer burst's timer owned");
  assert.equal(harness.timers.length, 2, "the superseded continuation re-arms nothing");
  assert.equal(harness.loads(), 3, "the superseded continuation reloads nothing");

  await harness.fire(1);
  assert.equal(harness.loads(), 4, "the newer burst's tick reloads the route");
  assert.equal(harness.timers.length, 3, "the newer burst continues to its next tick");
});

test("a second action's burst supersedes the pending ticks of the first", async () => {
  const harness = await settleBurstHarness();
  await harness.actionRefresh();
  assert.equal(harness.timers.length, 1, "the first burst arms its first tick");

  // A second action on the same route arms a new burst: the first burst's
  // still-pending timer is cancelled rather than left to fire into the newer
  // burst's ownership.
  await harness.actionRefresh();
  assert.deepEqual(harness.cleared, [1], "the superseded burst's pending timer is cancelled");
  assert.equal(harness.timers.length, 2, "the newer burst arms its own first tick");
  assert.equal(harness.app.settle.poll.timer, 2, "the newer burst owns the settle timer");

  // Even if the browser had already queued the older burst's callback, it
  // neither reloads nor re-arms into the newer burst — and its wrapper must
  // not erase ownership of the newer burst's still-pending timer handle.
  harness.timers[0].callback();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.loads(), 2, "the superseded tick does not reload");
  assert.equal(harness.timers.length, 2, "the superseded tick re-arms nothing");
  assert.equal(harness.app.settle.poll.timer, 2, "the stale wrapper leaves the newer burst's timer owned");

  // The newer burst's pending timeout stays cancellable: a navigation or a
  // disconnect clears it outright instead of leaving it live but untracked.
  await harness.app.load();
  assert.deepEqual(harness.cleared, [1, 2], "a navigation cancels the newer burst's pending timer");
  assert.equal(harness.app.settle.poll.timer, 0, "no settle timer remains armed after the navigation");

  // A tick already queued when the navigation landed stays inert as well.
  harness.timers[1].callback();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.loads(), 3, "a tick queued before the navigation does not reload after it");
  assert.equal(harness.timers.length, 2, "the queued tick re-arms nothing");
});

test("a failed action does not schedule a settle burst", async () => {
  const harness = await settleBurstHarness();
  globalThis.fetch = () => Promise.resolve({
    ok: false,
    json: () => Promise.resolve({ error: { message: "boom" } }),
  });
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  assert.equal(harness.status.textContent, "boom");
  assert.equal(harness.loads(), 0, "a failed action never reaches the refresh");
  assert.equal(harness.timers.length, 0, "no settle burst follows a failed action");
});

test("a refresh with no action in flight does not arm the settle burst", async () => {
  const harness = await settleBurstHarness();

  // The board's Done filter change is the ordinary case: it only re-reads
  // the current route and carries no settle provenance, so it must stay the
  // single load it always was.
  await harness.app.refresh();

  assert.equal(harness.loads(), 1, "an ordinary refresh stays a single load");
  assert.equal(harness.timers.length, 0, "no burst timers without the action's provenance");
});

test("an unrelated refresh during a pending action arms no burst, even if the action fails", async () => {
  const harness = await settleBurstHarness();
  const post = deferred();
  globalThis.fetch = () => post.promise;
  const button = new ActionButton({ workflowSchedule: "t-0001", project: "p-alpha" });
  const action = handleAction(harness.app, { target: button, preventDefault() {} });

  // The board's Done filter change while the action's POST is still pending:
  // overlapping an in-flight action is not provenance — this refresh carries
  // no token, so it stays a single load whether or not the action succeeds.
  assert.equal(inFlight.size, 1, "the action is still in flight");
  await harness.app.refresh();
  assert.equal(harness.loads(), 1, "the unrelated refresh runs its single load");
  assert.equal(harness.timers.length, 0, "no burst timers without the action's provenance");

  post.resolve({ ok: false, json: () => Promise.resolve({ error: { message: "boom" } }) });
  await action;
  assert.equal(harness.status.textContent, "boom");
  assert.equal(harness.loads(), 1, "the failed action never reaches a refresh");
  assert.equal(harness.timers.length, 0, "the failed action arms no burst either");
});

test("navigating away during the action's immediate refresh cancels the settle burst", async () => {
  const harness = await settleBurstHarness("/ui/board");
  // Hold the action's immediate load in flight so the navigation lands while
  // the refresh is still awaiting it; the load's generation and path were
  // already captured, exactly like the real load.
  const gate = deferred();
  const baseLoad = harness.app.load;
  let holdLoad = true;
  harness.app.load = async (options) => {
    const loadContext = await baseLoad(options);
    if (holdLoad) {
      holdLoad = false;
      await gate.promise;
    }
    return loadContext;
  };

  const refresh = harness.actionRefresh();
  // Opening another route starts a newer load: the generation bumps and the
  // pathname changes while the action's refresh is still awaiting its own —
  // now stale — load.
  harness.context.window.location.pathname = "/ui/jobs";
  await harness.app.load();
  gate.resolve();
  await refresh;

  assert.equal(harness.loads(), 2);
  assert.equal(harness.timers.length, 0, "a superseded refresh arms no burst on the new route");
});

test("a burst tick that finds a load in flight skips its reload instead of overlapping", async () => {
  const harness = await settleBurstHarness();
  await harness.actionRefresh();
  assert.equal(harness.loads(), 1);

  harness.app.loadsInFlight = 1;
  await harness.fire(0);
  assert.equal(harness.loads(), 1, "the tick skips rather than overlap the running load");
  assert.equal(harness.timers.length, 2, "the burst still arms its next tick");

  harness.app.loadsInFlight = 0;
  await harness.fire(1);
  assert.equal(harness.loads(), 2, "the next tick reloads once no load is in flight");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

// A console-action harness: a real FlowApp (real load, real settle-burst
// machinery) parked on /ui/console with the project and harness registries
// preloaded, a recording setTimeout, and a fetch stub that answers the
// start/release mutations and serves every console state reload as an
// inactive console. /ui/console has no regular poll (pollConfigForPath) and
// an inactive console schedules no console poll, so every timer recorded
// after the action belongs to the settle burst. The Console view's
// startConsole/releaseConsole handlers reload with app.load() instead of
// refresh(), so these tests pin the provenance stamping of handler-owned
// loads: a successful Console Start or Release must arm the burst exactly
// like a refresh()-based action, while the GET reload a burst tick performs
// (fromPoll, untokened) must not re-arm it.
async function consoleActionHarness() {
  const timers = [];
  const fetches = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = mountableContent();
  const { FlowConsole } = await consoleImports();
  const context = await scriptContext({
    location: { pathname: "/ui/console", search: "" },
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    document: consoleDocument(FlowConsole),
    URLSearchParams,
    fetch(path, options) {
      fetches.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ active: false, project_id: "p-alpha" }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.projects = [{ id: "p-alpha", name: "Alpha" }];
  app.harnesses = { agents: [], consoles: [{ name: "harness", display_name: "Harness" }] };
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === "[data-console-harness]") return { value: "harness" };
    return null;
  };
  app.querySelectorAll = () => [];
  return {
    app,
    context,
    status,
    timers,
    fetches,
    // consoleGets counts the console state reloads (the GETs), ignoring the
    // start/release mutations themselves.
    consoleGets: () => fetches.filter((call) => call.options.method === "GET").length,
    async fire(index) {
      timers[index].callback();
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

test("a successful console start performs its reload and arms the settle burst", async () => {
  const harness = await consoleActionHarness();
  // The Console view's project-level Start button: data-start-console is
  // empty (no task), the console target lives in data-project/data-task.
  const button = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay, secondDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  const post = harness.fetches.find((call) => call.options.method === "POST");
  assert.equal(post.path, "/ui/api/v2/projects/p-alpha/console");
  assert.deepEqual(JSON.parse(post.options.body), { harness: "harness" });
  assert.equal(harness.status.textContent, "Console starting");
  assert.equal(harness.consoleGets(), 1, "the start performs its immediate reload");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.consoleGets(), 2, "the first burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");
  assert.equal(harness.timers[1].delay, secondDelay - firstDelay);

  await harness.fire(1);
  assert.equal(harness.consoleGets(), 3, "the second burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("a successful console release performs its reload and arms the settle burst", async () => {
  const harness = await consoleActionHarness();
  const button = new ActionButton({ releaseConsole: "t-0001", project: "p-alpha", task: "t-0001" });

  await handleAction(harness.app, { target: button, preventDefault() {} });

  const [firstDelay] = harness.context.SETTLE_BURST_DELAYS_MS;
  const mutation = harness.fetches.find((call) => call.options.method === "DELETE");
  assert.equal(mutation.path, "/ui/api/v2/projects/p-alpha/tasks/t-0001/console");
  assert.equal(harness.status.textContent, "Console released");
  assert.equal(harness.consoleGets(), 1, "the release performs its immediate reload");
  assert.equal(harness.timers.length, 1, "the first burst tick is pending");
  assert.equal(harness.timers[0].delay, firstDelay);

  await harness.fire(0);
  assert.equal(harness.consoleGets(), 2, "the first burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the second burst tick is pending");

  await harness.fire(1);
  assert.equal(harness.consoleGets(), 3, "the second burst tick reloads the console view");
  assert.equal(harness.timers.length, 2, "the burst is bounded");
});

test("an older burst tick awaiting its real reload cannot displace a newer burst's timer", async () => {
  const harness = await consoleActionHarness();
  const startButton = new ActionButton({ startConsole: "", project: "p-alpha", task: "" });

  await handleAction(harness.app, { target: startButton, preventDefault() {} });
  assert.equal(harness.timers.length, 1, "the first burst arms its first tick");

  // Hold the first burst's tick in flight so a second action schedules its
  // burst while the older tick is still awaiting its reload — the race that
  // used to let the older continuation overwrite settlePoll's timer handle
  // and orphan the newer burst's timeout. The wrapper only delays handing
  // the completed load's context back, so the tick's reload itself runs
  // through the real FlowApp.load() and its supersede/clear block, and the
  // second action's load goes through the same real path.
  const gate = deferred();
  const baseLoad = harness.app.load.bind(harness.app);
  let hold = true;
  harness.app.load = async (options) => {
    const loadContext = await baseLoad(options);
    if (hold) {
      hold = false;
      await gate.promise;
    }
    return loadContext;
  };

  harness.fire(0);
  await handleAction(harness.app, { target: startButton, preventDefault() {} });
  assert.equal(harness.timers.length, 2, "the newer burst arms its first tick");
  assert.equal(harness.app.settle.poll.timer, 2, "the newer burst owns the settle timer");

  gate.resolve();
  // Flush past the microtask queue so the superseded continuation has run.
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.app.settle.poll.timer, 2, "the superseded continuation leaves the newer burst's timer owned");
  assert.equal(harness.timers.length, 2, "the superseded continuation re-arms nothing");
  assert.equal(harness.consoleGets(), 3, "the superseded continuation reloads nothing");

  await harness.fire(1);
  assert.equal(harness.consoleGets(), 4, "the newer burst's tick reloads the console view");
  assert.equal(harness.timers.length, 3, "the newer burst continues to its next tick");
});

