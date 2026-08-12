// Threads-tab element tests: the task-wide review-thread record — grouping by
// change, state badges, anchors and their outdated flags, claim/resolution
// summaries, and the comment timeline — plus the task route's task-scoped
// threads fetch and its change-filtered projection for the Change tab.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

installTestDOM();

const { taskModel } = await import("./task-model.js");
const { renderThreadList } = await import("./elements/thread-list.js");
const { renderTaskRoute } = await import("./task-route.js");
await import("./elements/task-detail.js");

function thread(overrides = {}) {
  const { comments, ...rest } = overrides;
  return {
    id: "th-1",
    task_id: "t-1",
    change_id: "ch-1",
    state: "open",
    anchor_commit_sha: "head-1",
    file_path: "src/a.go",
    line: 12,
    context: "func run() {",
    created_at: "2026-07-28T10:00:00Z",
    comments: comments ?? [{ id: 1, actor: "reviewer:alice", body: "This needs a guard.", created_at: "2026-07-28T10:00:00Z" }],
    ...rest,
  };
}

// threadListModel carries two changes: ch-1 (a superseded run's branch) and
// ch-2 (the current change), so grouping and per-change outdated-anchor
// marking both have something to bite on.
function threadListModel(threads, overrides = {}) {
  return {
    projectID: "p-1",
    taskThreads: threads,
    changes: [
      { id: "ch-1", head_sha: "head-1" },
      { id: "ch-2", head_sha: "head-2" },
    ],
    change: { id: "ch-2", head_sha: "head-2" },
    ...overrides,
  };
}

test("the thread list groups threads by change in review-round order and marks the current change", () => {
  const html = renderThreadList(
    threadListModel([
      thread({ id: "th-1", change_id: "ch-1" }),
      thread({ id: "th-2", change_id: "ch-1" }),
      thread({ id: "th-3", change_id: "ch-2" }),
    ]),
  );
  const firstGroup = html.indexOf('data-change="ch-1"');
  const secondGroup = html.indexOf('data-change="ch-2"');
  assert.ok(firstGroup !== -1 && secondGroup !== -1, "one group per change");
  assert.ok(firstGroup < secondGroup, "groups keep first-seen (review-round) order");
  assert.ok(html.indexOf('data-thread="th-1"') < html.indexOf('data-thread="th-2"'), "threads keep their order inside a group");
  assert.ok(html.indexOf('data-thread="th-2"') < html.indexOf('data-thread="th-3"'), "the older change's threads come first");
  assert.match(html, /data-change="ch-2" data-current/, "the current change's group is marked");
  assert.doesNotMatch(html, /data-change="ch-1" data-current/, "a superseded change is not marked current");
  // Every thread carries the read-only jump back to the Change tab's inline
  // diff view; writing stays there.
  assert.equal(html.match(/data-focus-tab="change"/g).length, 3);
});

test("state badges carry the state and a tone per state", () => {
  const html = renderThreadList(
    threadListModel([
      thread({ id: "th-open", state: "open" }),
      thread({ id: "th-claimed", state: "claimed", claim_kind: "fixed", claimed_by: "bob" }),
      thread({ id: "th-certified", state: "certified", claim_kind: "fixed", certified_by: "carol" }),
      thread({ id: "th-reopened", state: "reopened", reopened_by: "dave" }),
    ]),
  );
  assert.match(html, /data-thread="th-open" data-state="open"[\s\S]*?data-tone="warn">open</);
  assert.match(html, /data-thread="th-claimed" data-state="claimed"[\s\S]*?data-tone="idle">claimed</);
  assert.match(html, /data-thread="th-certified" data-state="certified"[\s\S]*?data-tone="ok">certified</);
  assert.match(html, /data-thread="th-reopened" data-state="reopened"[\s\S]*?data-tone="warn">reopened</);
});

test("the anchor links file:line to its change and the anchored code line renders escaped", () => {
  const html = renderThreadList(threadListModel([thread({ context: "if (x < y) {" })]));
  assert.match(html, /<a class="anchor" href="\/ui\/changes\/ch-1" data-link>src\/a.go:12<\/a>/);
  assert.match(html, /<pre class="context">if \(x &lt; y\) \{<\/pre>/);
  // No file → no anchor; no context → no pre.
  const bare = renderThreadList(threadListModel([thread({ file_path: "", line: 0, context: "" })]));
  assert.doesNotMatch(bare, /class="anchor"/);
  assert.doesNotMatch(bare, /class="context"/);
});

test("an anchor behind its change's head is flagged outdated, per change", () => {
  const html = renderThreadList(
    threadListModel([
      thread({ id: "th-stale", change_id: "ch-1", anchor_commit_sha: "older-head" }),
      thread({ id: "th-current", change_id: "ch-1", anchor_commit_sha: "head-1" }),
      thread({ id: "th-stale-2", change_id: "ch-2", anchor_commit_sha: "head-1" }),
    ]),
  );
  const stale = html.slice(html.indexOf('data-thread="th-stale"'), html.indexOf('data-thread="th-current"'));
  assert.match(stale, /outdated anchor/, "an anchor that is not the change's head is flagged");
  const current = html.slice(html.indexOf('data-thread="th-current"'), html.indexOf('data-thread="th-stale-2"'));
  assert.doesNotMatch(current, /outdated anchor/, "an anchor matching the change's head is not flagged");
  // The flag is read against the thread's own change, not the current one:
  // ch-2's head is head-2, so its head-1-anchored thread is outdated even
  // though head-1 is current for ch-1.
  const other = html.slice(html.indexOf('data-thread="th-stale-2"'));
  assert.match(other, /outdated anchor/);
});

test("the resolution line summarizes the claim, certification, and reopen", () => {
  const html = renderThreadList(
    threadListModel([
      thread({
        id: "th-fixed",
        state: "claimed",
        claim_kind: "fixed",
        claim_commit_sha: "1234567890abcdef1234567890abcdef12345678",
        claimed_by: "bob",
      }),
      thread({ id: "th-nw", state: "claimed", claim_kind: "not_warranted", claimed_by: "alice" }),
      thread({ id: "th-cert", state: "certified", claim_kind: "fixed", claimed_by: "bob", certified_by: "carol" }),
      thread({ id: "th-re", state: "reopened", reopened_by: "dave" }),
      thread({ id: "th-open" }),
    ]),
  );
  assert.match(html, /claimed fixed at 1234567890ab by bob/, "claim kind, short commit, and claimant");
  assert.match(html, /claimed not warranted by alice/);
  assert.match(html, /claimed fixed by bob · certified by carol/);
  assert.match(html, /reopened by dave/);
  const open = html.slice(html.indexOf('data-thread="th-open"'));
  assert.doesNotMatch(open, /class="resolution"/, "an open thread has no resolution line yet");
});

test("the comment timeline renders every comment in order, as markdown", () => {
  const html = renderThreadList(
    threadListModel([
      thread({
        comments: [
          { id: 1, actor: "reviewer:alice", body: "This needs a **guard**.", created_at: "2026-07-28T10:00:00Z" },
          { id: 2, actor: "author:bob", body: "Fixed in the next push.", created_at: "2026-07-28T11:00:00Z" },
        ],
      }),
    ]),
  );
  assert.ok(html.indexOf("reviewer:alice") < html.indexOf("author:bob"), "timeline order is comment order");
  assert.match(html, /<strong>guard<\/strong>/, "comment bodies render as block markdown");
  assert.match(html, /Fixed in the next push\./);
  assert.match(html, /<span class="meta">\d+[smhd] ago<\/span>/, "each comment carries its relative timestamp");
});

test("a threadless task renders the empty state", () => {
  assert.match(renderThreadList(threadListModel([])), /No review threads recorded/);
  assert.match(renderThreadList({}), /No review threads recorded/);
  assert.match(renderThreadList(null), /No review threads recorded/);
});

test("the thread-list element keeps its instance across a poll repaint", async () => {
  const root = globalThis.document.body;
  const model = () => threadListModel([thread({ id: "th-1" })]);
  const element = mountElement(root, "flow-thread-list", model());
  await flush();
  assert.match(element.innerHTML, /data-thread="th-1"/);
  // A poll delivers a brand-new model for the same task; the element instance
  // survives and repaints in place instead of being rebuilt.
  const before = element;
  element.data = model();
  await flush();
  assert.strictEqual(element, before);
  assert.match(element.innerHTML, /data-thread="th-1"/);
  element.remove();
});

test("the threads tab of task detail mounts the thread list", async () => {
  const root = globalThis.document.body;
  const model = taskModel(
    {
      task: { id: "t-0001", title: "Fix the thing", state: "working" },
      project_id: "p-1",
      task_detail: {
        ready_change: { id: "ch-2", head_sha: "head-2" },
        changes: [
          { id: "ch-1", head_sha: "head-1" },
          { id: "ch-2", head_sha: "head-2" },
        ],
      },
      task_threads: [thread({ id: "th-1", change_id: "ch-1" }), thread({ id: "th-2", change_id: "ch-2", anchor_commit_sha: "head-2" })],
    },
    null,
  );
  const detail = mountElement(root, "flow-task-detail", model);
  await flush();
  detail.querySelector("flow-tab-strip").select("threads");
  await flush();
  const list = detail.querySelector("flow-thread-list");
  assert.ok(list, "the threads tab mounts the thread-list element");
  assert.match(list.innerHTML, /data-change="ch-1"/);
  assert.match(list.innerHTML, /data-thread="th-2"/);
  detail.remove();
});

// --- the task route's task-scoped fetch -------------------------------------

test("the task route fetches task-scoped threads and projects the change-filtered subset", async () => {
  const calls = [];
  globalThis.fetch = (path) => {
    calls.push(String(path));
    const respond = (payload) => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(payload) });
    const url = String(path);
    if (url.endsWith("/v2/tasks/t-0001")) {
      return respond({
        task: { id: "t-0001", title: "Fix the thing", state: "in_progress" },
        project_id: "p-1",
        project_name: "flow",
        task_detail: {
          ready_change: { id: "ch-0002", head_sha: "head-2" },
          changes: [{ id: "ch-0002", head_sha: "head-2" }],
        },
      });
    }
    if (url.endsWith("/workflow")) return respond({});
    if (url.endsWith("/threads")) {
      return respond({
        threads: [
          { id: "th-old", change_id: "ch-0001", state: "certified", file_path: "a.go", line: 1, comments: [{ actor: "alice", body: "old round" }] },
          { id: "th-new", change_id: "ch-0002", state: "open", file_path: "b.go", line: 2, comments: [{ actor: "bob", body: "new round" }] },
        ],
      });
    }
    if (url.endsWith("/findings")) return respond({ findings: [], follow_ups: [], summary: {} });
    if (url.endsWith("/context")) return respond({ item: { id: "t-0001" } });
    return Promise.resolve({ ok: false, status: 404, json: () => Promise.resolve({ error: { message: `missing stub for ${url}` } }) });
  };

  const content = globalThis.document.createElement("div");
  const app = {
    setTitle() {},
    isActiveLoad: () => true,
    querySelector: (selector) => (selector === ".content" ? content : null),
    ensureWorkItems: () => Promise.resolve([]),
  };
  assert.equal(await renderTaskRoute(app, "t-0001", null), true);

  const threadsFetch = calls.find((path) => path.endsWith("/threads"));
  assert.equal(threadsFetch, "/ui/api/v2/projects/p-1/tasks/t-0001/threads", "the fetch is task-scoped");
  assert.ok(!calls.some((path) => path.includes("/v2/changes/")), "no per-change threads fetch anymore");

  const mounted = content.firstElementChild;
  assert.ok(mounted, "the route mounts the task detail element");
  assert.deepEqual(
    mounted.data.threads.map((candidate) => candidate.id),
    ["th-new"],
    "the Change tab and Now card see only the current change's threads",
  );
  assert.deepEqual(
    mounted.data.taskThreads.map((candidate) => candidate.id),
    ["th-old", "th-new"],
    "the Threads tab sees the full cross-change record",
  );
  mounted.remove();
});
