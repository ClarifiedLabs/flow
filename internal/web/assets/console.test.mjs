// <flow-console> tests: the console poll's skip/re-arm policy (ported from
// app.test.mjs when the loop moved into the element) and the page markup.

import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";
import { deferred } from "./test-helpers.mjs";

const root = installTestDOM();
const { renderConsoleChooserMarkup, renderConsoleMarkup, resolveConsoleProject } = await import("./elements/console.js");

// consoleHarness mounts a connected <flow-console> under a stub <flow-app>
// with a recording poll stub swapped in before the payload arms the loop, and
// a fetch stub that hands each console-state GET a deferred response in call
// order (or rejects every GET with rejectWith when given).
function consoleHarness(rejectWith) {
  const timers = [];
  const responses = [];
  const originalFetch = globalThis.fetch;
  const originalPathname = window.location.pathname;
  window.location.pathname = "/ui/console";
  globalThis.fetch = () => {
    if (rejectWith !== undefined) return Promise.reject(rejectWith);
    const response = deferred();
    responses.push(response);
    return response.promise;
  };

  const app = document.createElement("flow-app");
  app.loads = 0;
  app.loadsInFlight = 0;
  app.status = "";
  app.load = async () => { app.loads += 1; };
  app.setStatus = (message) => { app.status = message; };
  root.appendChild(app);

  const element = document.createElement("flow-console");
  // Replace the real Poller with a recording stub before data arrives, so the
  // recorded callback is the poll's own async function (awaitable directly).
  element.poll = {
    arm(delay, callback) {
      timers.push({ callback, delay });
    },
    clear() {},
  };
  app.appendChild(element);

  return {
    app,
    element,
    timers,
    responses,
    mount(payload) {
      element.data = { project: { id: "p-alpha", name: "Alpha" }, projectID: "p-alpha", selectedTask: "", ...payload };
    },
    consoleState(active, terminalAvailable) {
      return {
        ok: true,
        json: () => Promise.resolve({ active, terminal_available: terminalAvailable, project_id: "p-alpha" }),
      };
    },
    restore() {
      globalThis.fetch = originalFetch;
      window.location.pathname = originalPathname;
      element.remove();
      app.remove();
    },
  };
}

test("a poll response overlapping another load skips its reload and keeps polling", async () => {
  const harness = consoleHarness();
  try {
    // An active console without a terminal arms the poll.
    harness.mount({ active: true, terminalAvailable: false });
    assert.equal(harness.timers.length, 1);

    // The poll fires and its state GET hangs; meanwhile another load is
    // still in flight when the poll's response arrives announcing a terminal.
    const pollReload = harness.timers[0].callback();
    harness.app.loadsInFlight = 1;
    harness.responses[0].resolve(harness.consoleState(true, true));
    await pollReload;

    assert.equal(harness.app.loads, 0, "the poll response does not start a load while another load is in flight");
    assert.equal(harness.timers.length, 2, "the skipped poll re-arms instead of stopping");

    // The overlapping load completes; the route render's fresh payload
    // re-arms the poll.
    harness.app.loadsInFlight = 0;
    harness.mount({ active: true, terminalAvailable: true });
    assert.equal(harness.timers.length, 3, "the completed load's payload re-arms the console poll");

    // With no load in flight, a later poll response still reloads once: the
    // console going inactive triggers exactly one load and no further polling.
    const inactiveReload = harness.timers[2].callback();
    harness.responses[1].resolve(harness.consoleState(false, false));
    await inactiveReload;
    assert.equal(harness.app.loads, 1, "a later poll response reloads once when no load is active");
    harness.mount({ active: false, terminalAvailable: false });
    await flush();
    assert.equal(harness.timers.length, 3, "an inactive console schedules no further poll");
  } finally {
    harness.restore();
  }
});

test("console poll transitions reload exactly once when no load is in flight", async () => {
  const harness = consoleHarness();
  try {
    harness.mount({ active: true, terminalAvailable: false });
    assert.equal(harness.timers.length, 1);

    // Terminal availability appearing reloads the console once; the route's
    // fresh payload re-arms.
    const terminalPoll = harness.timers[0].callback();
    harness.responses[0].resolve(harness.consoleState(true, true));
    await terminalPoll;
    assert.equal(harness.app.loads, 1, "the terminal-availability transition reloads once");
    harness.mount({ active: true, terminalAvailable: true });
    assert.equal(harness.timers.length, 2, "the active console keeps polling");

    // The console going inactive reloads once and stops polling.
    const inactivePoll = harness.timers[1].callback();
    harness.responses[1].resolve(harness.consoleState(false, false));
    await inactivePoll;
    assert.equal(harness.app.loads, 2, "the inactive-console transition reloads once");
    harness.mount({ active: false, terminalAvailable: false });
    await flush();
    assert.equal(harness.timers.length, 2, "an inactive console schedules no further poll");
  } finally {
    harness.restore();
  }
});

test("a hostile console refresh rejection still reports a safe status and keeps polling", async () => {
  // The console state GET rejects with a Proxy whose prototype lookup throws:
  // the poll catch must format it without throwing, or the "console refresh
  // failed" status and the re-arm would never run.
  const harness = consoleHarness(new Proxy({}, {
    getPrototypeOf() {
      throw new Error("prototype trap");
    },
  }));
  try {
    harness.mount({ active: true, terminalAvailable: false });
    assert.equal(harness.timers.length, 1);

    await harness.timers[0].callback();

    assert.equal(harness.app.status, "console refresh failed: Request failed");
    assert.equal(harness.timers.length, 2, "the failed console refresh re-arms the poll");
  } finally {
    harness.restore();
  }
});

test("navigation stops the console poll with the element's disconnect", async () => {
  const harness = consoleHarness();
  try {
    harness.mount({ active: true, terminalAvailable: false });
    await flush();
    let cleared = 0;
    harness.element.poll.clear = () => { cleared += 1; };
    harness.element.remove();
    assert.equal(cleared, 1, "disconnecting the element clears its timer");
  } finally {
    harness.restore();
  }
});

test("resolveConsoleProject picks the URL project, the single selection, or the only project", () => {
  const projects = [{ id: "p-alpha", name: "Alpha" }, { id: "p-beta", name: "Beta" }];
  assert.deepEqual(resolveConsoleProject({ projects, selectedProjectIDs: () => [] }, "p-beta"), { id: "p-beta", name: "Beta" });
  assert.deepEqual(resolveConsoleProject({ projects, selectedProjectIDs: () => ["p-alpha"] }, ""), { id: "p-alpha", name: "Alpha" });
  assert.deepEqual(resolveConsoleProject({ projects: [projects[0]], selectedProjectIDs: () => [] }, ""), { id: "p-alpha", name: "Alpha" });
  assert.equal(resolveConsoleProject({ projects, selectedProjectIDs: () => [] }, ""), null, "an ambiguous registry renders the chooser");
});

test("console markup renders start controls when idle and the terminal when live", () => {
  const idle = renderConsoleMarkup({
    project: { id: "p-alpha", name: "Alpha" },
    projectID: "p-alpha",
    active: false,
    harnessOptions: '<option value="harness" selected>Harness</option>',
  });
  assert.match(idle, /data-start-console/);
  assert.match(idle, /not running/);

  const live = renderConsoleMarkup({
    project: { id: "p-alpha", name: "Alpha" },
    projectID: "p-alpha",
    active: true,
    session: { id: "s-0001" },
    terminalAvailable: true,
    loginPath: "/v2/sessions/s-0001/terminal-login?token=abc",
  });
  assert.match(live, /data-release-console/);
  assert.match(live, /terminal-login\?token=abc/);

  assert.match(renderConsoleChooserMarkup([{ id: "p-alpha", name: "Alpha" }]), /\/ui\/console\?project=p-alpha/);
  assert.match(renderConsoleChooserMarkup([]), /No projects/);
});
