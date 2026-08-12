// Change route tests: metadata/diff head coherence for the standalone
// route (loadChangeRouteModule drives a fresh module instance per test so its
// caches start empty).

import assert from "node:assert/strict";
import { test } from "node:test";
import { InlineDOMElement, RepaintingInlineDOMElement, deferred, findInlineTerminal, flushAsync, inlineDocument, normalize, scriptContext } from "./test-helpers.mjs";

// changeRouteModulePromise caches the route module instance per test file so
// each loadChangeRouteModule() call returns a fresh module for that test.
let changeRouteModulePromise;

function loadChangeRouteModule() {
  changeRouteModulePromise = changeRouteModulePromise || import("./change-route.js");
  return changeRouteModulePromise;
}

function changeRouteHarness() {
  const content = new InlineDOMElement("section");
  const app = {
    setTitle() {},
    querySelector(selector) {
      return selector === ".content" ? content : null;
    },
  };
  return { app, content };
}

test("change route mounts only a metadata/diff pair naming the same head", async () => {
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      fetchCalls.push(path);
      if (path.endsWith("/diff")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ change_id: "ch-0001", head_sha: "abc123", total_files: 2 }),
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  assert.equal(await renderChangeRoute(app, "ch-0001", null), true);
  const mounted = content.children[0].data;
  assert.equal(mounted.change.head_sha, "abc123");
  assert.equal(mounted.diff.head_sha, "abc123");
  assert.deepEqual(fetchCalls, ["/ui/api/v2/changes/ch-0001", "/ui/api/v2/changes/ch-0001/diff"]);
});

test("change route retries a coherent pair when the head moves between reads", async () => {
  let metadataCalls = 0;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        // The change advanced between the metadata read and this diff read:
        // the diff answers for the head the server now holds, not the one the
        // metadata named. The pair must be re-read, never mounted mixed.
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ change_id: "ch-0001", head_sha: "new-head", total_files: 1 }),
        });
      }
      metadataCalls += 1;
      const head = metadataCalls === 1 ? "old-head" : "new-head";
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: head } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  assert.equal(await renderChangeRoute(app, "ch-0001", null), true);
  assert.equal(metadataCalls, 2, "metadata is re-read after the diff answered for the new head");
  assert.equal(content.children.length, 1, "only the verified pair mounts");
  const mounted = content.children[0].data;
  assert.equal(mounted.change.head_sha, "new-head");
  assert.equal(mounted.diff.head_sha, "new-head");
});

test("change route never mounts a mixed-head pair when the head keeps moving", async () => {
  let metadataCalls = 0;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ change_id: "ch-0001", head_sha: "new-head" }),
        });
      }
      metadataCalls += 1;
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: `head-${metadataCalls}` } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  await assert.rejects(renderChangeRoute(app, "ch-0001", null), /advanced while it was loading/);
  assert.equal(metadataCalls, 3, "three reads are attempted before giving up");
  assert.equal(content.children.length, 0, "no unverified pair ever mounts");
});

test("change route reports a persistently failing diff fetch as unavailable, not a head move", async () => {
  let diffCalls = 0;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        diffCalls += 1;
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  await assert.rejects(renderChangeRoute(app, "ch-0001", null), (error) => {
    assert.match(error.message, /diff is not available/, "a failed diff fetch reports the diff as unavailable");
    assert.doesNotMatch(error.message, /advanced while it was loading/, "a stable head is not reported as a head move");
    return true;
  });
  assert.equal(diffCalls, 3, "three diff reads are attempted before giving up");
  assert.equal(content.children.length, 0, "no unverified pair ever mounts");
});

test("change route reports a persistently headless diff as unavailable, not a head move", async () => {
  let diffCalls = 0;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        diffCalls += 1;
        // The server answered but its diff names no head, so it cannot verify
        // the pair: that is an unavailable diff, not a moved head.
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ change_id: "ch-0001" }) });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  await assert.rejects(renderChangeRoute(app, "ch-0001", null), (error) => {
    assert.match(error.message, /diff is not available/, "a headless diff reports the diff as unavailable");
    assert.doesNotMatch(error.message, /advanced while it was loading/, "a stable head is not reported as a head move");
    return true;
  });
  assert.equal(diffCalls, 3, "three diff reads are attempted before giving up");
  assert.equal(content.children.length, 0, "no unverified pair ever mounts");
});

test("change route reports the last unverified read's cause, not an earlier diff failure", async () => {
  let metadataCalls = 0;
  let diffCalls = 0;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        diffCalls += 1;
        return Promise.reject(new Error("network down"));
      }
      metadataCalls += 1;
      // The first read is this change; the later reads answer for another
      // change, so the retries end on the mismatch path, not on the failed
      // diff. The terminal message must name the mismatch, not the earlier
      // diff outage.
      const id = metadataCalls === 1 ? "ch-0001" : "ch-9999";
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id, head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  await assert.rejects(renderChangeRoute(app, "ch-0001", null), (error) => {
    assert.match(error.message, /advanced while it was loading/, "a mismatch that ends the retries reports the head-move error");
    assert.doesNotMatch(error.message, /diff is not available/, "an earlier failed diff does not leak into the terminal cause");
    return true;
  });
  assert.equal(diffCalls, 1, "only the first attempt reaches the diff fetch");
  assert.equal(metadataCalls, 3, "three reads are attempted before giving up");
  assert.equal(content.children.length, 0, "no unverified pair ever mounts");
});

test("change route retries a failed diff fetch instead of mounting an empty diff", async () => {
  let diffFailures = 1;
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        if (diffFailures > 0) {
          diffFailures -= 1;
          return Promise.reject(new Error("network down"));
        }
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ change_id: "ch-0001", head_sha: "abc123", total_files: 1 }),
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  assert.equal(await renderChangeRoute(app, "ch-0001", null), true);
  const mounted = content.children[0].data;
  assert.equal(mounted.change.head_sha, "abc123");
  assert.equal(mounted.diff.head_sha, "abc123");
});

test("change route mounts a headless change explicitly with an empty diff and no diff fetch", async () => {
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  assert.equal(await renderChangeRoute(app, "ch-0001", null), true);
  assert.deepEqual(fetchCalls, ["/ui/api/v2/changes/ch-0001"]);
  const mounted = content.children[0].data;
  assert.deepEqual(mounted.diff, {});
});

test("change route mounts a verified head when the server's diff is explicitly unavailable", async () => {
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path) {
      if (path.endsWith("/diff")) {
        // The server's no-diff response still names the head it would diff, so
        // it verifies the pair and installs as an empty diff.
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "abc123",
            available: false,
            unavailable_reason: "diff not captured",
          }),
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ change: { id: "ch-0001", head_sha: "abc123" } }) });
    },
  });
  const { renderChangeRoute } = await loadChangeRouteModule();
  const { app, content } = changeRouteHarness();

  assert.equal(await renderChangeRoute(app, "ch-0001", null), true);
  const mounted = content.children[0].data;
  assert.equal(mounted.change.head_sha, "abc123");
  assert.equal(mounted.diff.head_sha, "abc123");
  assert.equal(mounted.diff.available, false);
});
