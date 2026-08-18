// Task-area element tests: task detail, findings, change review, epic, and
// the element lifecycle conventions. Split from elements.test.mjs.

// Tests for the custom-element UI: the pure render functions as strings, and
// the element lifecycle behaviours that string tests cannot reach — keyed
// reconcile preserving instance state across a poll, delegated listeners
// surviving a repaint, and disclosure staying local to its row.

import assert from "node:assert/strict";
import test from "node:test";

import { flush, installTestDOM, mountElement, TestEvent, TestNode } from "./test-dom.mjs";

installTestDOM();

const { cardModel, dwellTone, formatDwell, matchesFilter, sortForAttention, waitActionLabel, waitReasonText, waitingOnBlockers, waitingOnOmitted } =
  await import("./board-model.js");
const { compareBoardCards, lastActivityMs } = await import("./board-model.js");
const { readBoardSort, writeBoardSort } = await import("./storage.js");
const { BOARD_SORT_STORAGE_KEY, BOARD_VIEW_STORAGE_KEY } = await import("./config.js");
const { renderBoardSort } = await import("./elements/board-sort.js");
const { nowCardModel, runRows, tabBadges, isOutdatedAnchor, reviewModel, taskModel } = await import("./task-model.js");
const { mount, reconcile } = await import("./elements/base.js");
const { renderTaskCard } = await import("./elements/task-card.js");
const { renderTaskRail } = await import("./elements/task-rail.js");
const { renderAttentionStrip } = await import("./elements/attention-strip.js");
const { renderThroughputStrip } = await import("./elements/throughput-strip.js");
const { renderBoardTable } = await import("./elements/board-table.js");
const { boardEntries } = await import("./elements/board.js");
const { LANES } = await import("./config.js");
const { renderStepRail } = await import("./elements/step-rail.js");
const { renderRunList } = await import("./elements/run-list.js");
const { renderRunSpine } = await import("./elements/run-spine.js");
const { renderWorkflowGraph, graphCounts } = await import("./elements/workflow-graph.js");
const { renderCheckList } = await import("./elements/check-list.js");
const { renderFindings } = await import("./elements/findings-list.js");
const { renderHeldPanel, HAND_BACK_EDGES } = await import("./elements/held-panel.js");
const { renderDiffFile } = await import("./elements/diff.js");
const { renderReviewBar } = await import("./elements/review-bar.js");
const { renderEpic, memberState } = await import("./elements/epic.js");
const { renderFeatures, featureCountsLabel, featureDivergenceLabel } = await import("./elements/features.js");
const { renderFeature } = await import("./elements/feature.js");
const { renderNowCard } = await import("./elements/now-card.js");
const { renderReviewPanel } = await import("./elements/review-panel.js");
const { renderActivityFeed, activityEntries } = await import("./elements/activity-feed.js");
const { renderTaskFormView, bindRelationsPickerView, bindTaskFlowControlsView, relationTargetSuggestionsView } = await import("./task-view.js");
await import("./elements/lane.js");
await import("./elements/board.js");
await import("./elements/board-sort.js");
const { renderTabStrip } = await import("./elements/tab-strip.js");
const { acquireBusy, handleAction, inFlight, releaseBusy, settleStatus } = await import("./actions.js");
const { handleFormSubmit } = await import("./forms.js");
const { diffUnavailable } = await import("./elements/change.js");
const { renderChangeRoute } = await import("./change-route.js");
const { renderInlineThread } = await import("./elements/inline-thread.js");
const { renderOwnerRulingsPanel } = await import("./elements/task-detail-view.js");
await import("./elements/task-detail.js");

const HOUR = 3600_000;

function entry(overrides = {}) {
  const { task = {}, card = {}, ...rest } = overrides;
  return {
    task: { id: "t-0001", title: "Retry budget for failed check nodes", state: "in_progress", priority: 1, ...task },
    card: { step_index: 3, step_count: 6, dwell_since: new Date(Date.now() - 12 * 60_000).toISOString(), ...card },
    laneState: "working",
    blocked: false,
    project: { id: "p-1", name: "flow" },
    ...rest,
  };
}

// --- task detail -----------------------------------------------------------

test("the run spine gives a repeat visit its own row rather than a crossing edge", () => {
  const rows = runRows({
    run: { snapshot: { nodes: [{ key: "implement", name: "Implement" }, { key: "checks", name: "Checks" }] } },
    node_runs: [
      { id: "n1", node_key: "implement", visit: 1, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
      { id: "n2", node_key: "checks", visit: 1, state: "failed", outcome: "failed", kind: "automated_checks", name: "Checks" },
      { id: "n3", node_key: "implement", visit: 2, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
    ],
  });
  assert.equal(rows.length, 3);
  assert.equal(rows[2].tag, "visit 2");
  assert.match(rows[1].loop, /looped back to Implement/);
});

test("the run spine names the nodes still ahead", () => {
  const rows = runRows({
    run: { snapshot: { nodes: [{ key: "a", name: "A" }, { key: "b", name: "B" }] } },
    node_runs: [{ id: "n1", node_key: "a", visit: 1, state: "succeeded", name: "A" }],
  });
  assert.equal(rows.length, 2);
  assert.equal(rows[1].state, "future");
});

test("the run spine marks the visit the run is on now as current after a loop-back", () => {
  // Regression: the spine collapsed repeat visits by keeping the *first* one,
  // so a merge conflict that looped the run back to implement left every node
  // reading "done" and none current — contradicting the Overview tab, which
  // showed the run implementing.
  const rows = runRows({
    run: {
      current_node_run_id: "n3",
      snapshot: {
        nodes: [
          { key: "implement", name: "Implement" },
          { key: "merge", name: "Merge change" },
          { key: "merged", name: "Merged" },
        ],
      },
    },
    node_runs: [
      { id: "n1", node_key: "implement", visit: 1, state: "succeeded", outcome: "completed", kind: "agent", name: "Implement" },
      { id: "n2", node_key: "merge", visit: 1, state: "succeeded", outcome: "conflict", kind: "merge_change", name: "Merge change" },
      { id: "n3", node_key: "implement", visit: 2, state: "running", kind: "agent", name: "Implement" },
    ],
  });
  const html = renderRunSpine({ rows, runSequence: 1 });
  // One row per node, kept in graph order even though implement's latest
  // visit is its second.
  assert.equal((html.match(/<li /g) || []).length, 3);
  const states = [...html.matchAll(/<li data-state="([^"]+)">\s*<span class="dot"><\/span>\s*<span class="name">([^<]+)<\/span>/g)].map(
    (match) => [match[2], match[1]],
  );
  assert.deepEqual(states, [
    ["Implement", "current"],
    ["Merge change", "done"],
    ["Merged", "future"],
  ]);
});

test("the current run row lists its fanned-out agents, not just the node", () => {
  const html = renderRunList([
    {
      id: "n1",
      name: "Review the change",
      tag: "change review",
      state: "current",
      duration: "12m",
      jobs: [
        { id: "j-0912", name: "security-review", state: "running" },
        { id: "j-0913", name: "performance-review", state: "finished", verdict: "satisfied" },
      ],
      loop: "",
    },
  ]);
  assert.match(html, /security-review/);
  assert.match(html, /performance-review/);
  assert.match(html, /satisfied/);
});

test("the graph shows a visit count only when there was more than one visit", () => {
  const model = {
    snapshot: {
      nodes: [
        { key: "implement", name: "Implement", kind: "agent" },
        { key: "checks", name: "Checks", kind: "automated_checks" },
      ],
      edges: [{ from: "implement", outcome: "completed", to: "checks" }],
    },
    run: { current_node_key: "checks" },
    transitionCounts: [{ from: "implement", outcome: "completed", to: "checks", count: 1 }],
  };
  assert.ok(!renderWorkflowGraph(model).includes("×1"), "a single visit should not be annotated");

  model.transitionCounts = [{ from: "implement", outcome: "completed", to: "checks", count: 2 }];
  assert.match(renderWorkflowGraph(model), /×2/);
});

test("back edges route into their own channel below the row", () => {
  const html = renderWorkflowGraph({
    snapshot: {
      nodes: [
        { key: "implement", name: "Implement", kind: "agent" },
        { key: "checks", name: "Checks", kind: "automated_checks" },
        { key: "review", name: "Review", kind: "change_review" },
      ],
      edges: [
        { from: "implement", outcome: "completed", to: "checks" },
        { from: "checks", outcome: "failed", to: "implement" },
        { from: "review", outcome: "changes_requested", to: "implement" },
      ],
    },
    run: { current_node_key: "review" },
    transitionCounts: [],
  });
  // Two back edges, each dropping to its own y before returning: no shared
  // lane means no edge can cross a node.
  const channels = [...html.matchAll(/V(\d+) H/g)].map((match) => match[1]);
  assert.equal(new Set(channels).size, channels.length, `back edges share a channel: ${channels}`);
  assert.equal(channels.length, 2);
});

test("graph counts fold per-edge tallies into per-node visits", () => {
  const { nodeVisits, takenEdges } = graphCounts([
    { from: "a", outcome: "ok", to: "b", count: 2 },
    { from: "b", outcome: "fail", to: "a", count: 1 },
  ]);
  assert.equal(nodeVisits.get("b"), 2);
  assert.equal(takenEdges.get("a|ok|b"), 2);
});

test("the Now card's review-thread age dwells on the task model clock, not the wall clock", () => {
  // A fixed model clock far from the real wall clock: the Now card's thread
  // age must use the same `now` as the rest of the model, or the two drift
  // apart (here the real clock would report the thread as days old while the
  // model says 1h).
  const now = Date.parse("2026-07-29T12:00:00Z");
  const model = taskModel(
    {
      task: { id: "t-0001", title: "Fix the thing" },
      task_detail: {},
      threads: [
        {
          id: "th-0021",
          state: "open",
          created_at: "2026-07-29T11:00:00Z",
          file_path: "internal/lifecycle/engine.go",
          line: 212,
          comments: [{ body: "Pausing for the operator leaves the lease live." }],
        },
      ],
    },
    null,
    { now },
  );
  const card = nowCardModel(model);
  assert.equal(card.heading, "Now · 1 review thread block the merge");
  assert.equal(card.age, "1h", "thread age must use the model clock");
});

test("the Now card renders for an open wait and stays away otherwise", () => {
  const base = { activity: "Reviewing the change", stepName: "review", dwell: "12m", threads: [], openThreads: 0 };
  assert.equal(nowCardModel({ ...base, wait: null }), null);
  const card = nowCardModel({ ...base, wait: { message: "Ship it?" }, waitKind: "gate" });
  assert.equal(card.tone, "await");
  assert.equal(card.body, "Ship it?");
});

async function saveTaskHumanReviewTransition(persisted, checked, taskID) {
  const requests = [];
  let refreshes = 0;
  globalThis.fetch = (path, options) => {
    requests.push({ path: String(path), options });
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
  };
  const model = taskModel({
    task: {
      id: taskID,
      title: "Review policy task",
      body: "Policy details",
      priority: 2,
      flow_id: "fl-coding",
      requires_human_review: persisted,
    },
    project_id: "p-1",
    task_detail: {},
  });
  const detail = mountElement(globalThis.document.body, "flow-task-detail", model);
  await flush();

  const edit = detail.querySelector("[data-task-edit]");
  assert.ok(edit, "task detail exposes its edit action");
  edit.click();
  await flush();

  const form = detail.querySelector("[data-task-form]");
  assert.ok(form, "the edit action renders the existing task form");
  assert.equal(form.dataset.taskFormMode, "edit");
  const reviewPolicy = form.elements.requires_human_review;
  assert.equal(reviewPolicy.hasAttribute("checked"), persisted, "the checkbox reflects the persisted policy");

  // The test DOM parses value attributes but intentionally does not emulate
  // HTMLInputElement's reflected value/checked properties, so fill the controls
  // the same way a browser user would before submitting.
  form.elements.title.value = "Review policy task";
  form.elements.body.value = "Policy details";
  form.elements.priority.value = "2";
  form.elements.flow_id.value = "fl-coding";
  reviewPolicy.checked = checked;

  const app = {
    setStatus() {},
    async refresh() {
      refreshes += 1;
    },
  };
  assert.equal(await handleFormSubmit(app, { target: form, preventDefault() {} }), true);
  await flush();

  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, `/ui/api/v2/projects/p-1/tasks/${taskID}`);
  assert.equal(requests[0].options.method, "PATCH");
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    title: "Review policy task",
    body: "Policy details",
    priority: 2,
    flow_id: "fl-coding",
    requires_human_review: checked,
  });
  assert.equal(refreshes, 1, "saving refreshes task detail");
  assert.equal(detail.querySelector("[data-task-editor]"), null, "saving returns to task detail");
  detail.remove();
}

test("task detail edits human review policy from checked to unchecked", async () => {
  await saveTaskHumanReviewTransition(true, false, "t-review-off");
});

test("task detail edits human review policy from unchecked to checked", async () => {
  await saveTaskHumanReviewTransition(false, true, "t-review-on");
});

test("task detail preserves an unsaved draft when Edit task is clicked again", async () => {
  const model = taskModel({
    task: { id: "t-edit-draft", title: "Persisted title", body: "Persisted body", priority: 1 },
    project_id: "p-1",
    task_detail: {},
  });
  const detail = mountElement(globalThis.document.body, "flow-task-detail", model);
  await flush();

  detail.querySelector("[data-task-edit]").click();
  await flush();
  const form = detail.querySelector("[data-task-form]");
  form.elements.title.value = "Unsaved title";

  detail.querySelector("[data-task-edit]").click();
  await flush();

  assert.equal(detail.querySelector("[data-task-form]"), form, "the repeated action must not reconstruct the form");
  assert.equal(form.elements.title.value, "Unsaved title");
  detail.remove();
});

test("task detail exits edit mode when its task identity changes", async () => {
  const first = taskModel({
    task: { id: "t-edit-first", title: "First task", body: "First body", priority: 1 },
    project_id: "p-1",
    task_detail: {},
  });
  const second = taskModel({
    task: { id: "t-edit-second", title: "Second task", body: "Second body", priority: 2 },
    project_id: "p-1",
    task_detail: {},
  });
  const detail = mountElement(globalThis.document.body, "flow-task-detail", first);
  await flush();

  detail.querySelector("[data-task-edit]").click();
  await flush();
  assert.ok(detail.querySelector("[data-task-editor]"));

  detail.data = second;
  await flush();

  assert.equal(detail.querySelector("[data-task-editor]"), null);
  assert.ok(detail.querySelector("flow-tab-strip"), "the new task renders its detail view");
  assert.equal(detail.querySelector("flow-task-rail").data.id, "t-edit-second");
  assert.equal(detail.editingTaskID, "", "the previous task's editor state is discarded");
  detail.remove();
});

test("the Now card surfaces a review thread that blocks the merge", () => {
  const card = nowCardModel({
    wait: null,
    openThreads: 1,
    threads: [
      {
        id: "th-0021",
        state: "open",
        file_path: "internal/lifecycle/engine.go",
        line: 212,
        anchor_commit_sha: "old",
        comments: [{ body: "Pausing for the operator leaves the lease live." }],
      },
    ],
    change: { head_sha: "new" },
  });
  assert.match(card.heading, /1 review thread/);
  assert.equal(card.locus, "internal/lifecycle/engine.go:212");
  assert.ok(card.outdated, "an anchor behind the head must be flagged");
});

test("an anchor matching the head is not flagged as outdated", () => {
  assert.ok(!isOutdatedAnchor({ anchor_commit_sha: "abc" }, { head_sha: "abc" }));
  assert.ok(!isOutdatedAnchor({}, { head_sha: "abc" }));
});

test("tab badges are toned by whether the number is good news", () => {
  const badges = tabBadges({
    change: { id: "c-1" },
    openThreads: 1,
    checks: [{ verdict: "satisfied" }, { verdict: "blocked" }],
    checksSatisfied: 1,
    transitions: [{}],
    statusLog: [],
    terminalAvailable: true,
  });
  assert.equal(badges.change.tone, "warn");
  assert.equal(badges.checks.text, "1/2");
  assert.equal(badges.checks.tone, "danger");
  assert.ok(badges.terminal.live);
});

test("a failed check row carries the disclosure, retry and skip", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "unit", verdict: "satisfied", details: "go test ./..." },
      { name: "lifecycle-test", verdict: "blocked", source_job_id: "j-0912", exit_code: 1 },
    ],
  });
  assert.match(html, /data-transcript-toggle="lifecycle-test"/);
  assert.match(html, /data-failed/);
  assert.match(html, /Retry/);
  assert.match(html, /Skip/);
});

test("the check list renders details as block markdown but keeps job id and exit code escaped", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "reviewer", verdict: "blocked", details: "**Overall**: needs work", source_job_id: "j-<_1", exit_code: 1 },
    ],
  });
  assert.match(html, /<div class="detail">\s*<div class="md"><p><strong>Overall<\/strong>: needs work<\/p><\/div>\s*<span class="meta">j-&lt;_1 · exit 1<\/span>\s*<\/div>/);
});

test("multi-paragraph check details render as block markdown, not one collapsed line", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      {
        name: "reviewer",
        verdict: "blocked",
        details:
          "**Overall**: needs work\n\nPlease fix the escaping in renderCheckRow before merging.\n\n- fix the escaping in renderCheckRow\n- add a regression test",
        source_job_id: "j-0912",
        exit_code: 1,
      },
    ],
  });
  const detail = html.match(/<div class="detail">([\s\S]*?)<\/div>\s*<span class="actions">/);
  assert.ok(detail, "the detail column wraps the block markdown");
  assert.match(detail[1], /<div class="md"><p><strong>Overall<\/strong>: needs work<\/p>/);
  assert.match(detail[1], /<p>Please fix the escaping in renderCheckRow before merging\.<\/p>/);
  assert.match(detail[1], /<ul>\s*<li>fix the escaping in renderCheckRow<\/li>\s*<li>add a regression test<\/li>\s*<\/ul>/);
  assert.doesNotMatch(detail[1], /needs work Please fix the escaping/);
  assert.doesNotMatch(detail[1], /needs work · fix the escaping/);
  assert.match(detail[1], /<span class="meta">j-0912 · exit 1<\/span>/);
  assert.match(html, /data-transcript-toggle="reviewer"/);
  assert.match(html, /Retry/);
  assert.match(html, /Skip/);
});

test("satisfied check rows still show the duration next to block details", () => {
  const html = renderCheckList({
    id: "t-0042",
    checks: [
      { name: "unit", verdict: "satisfied", details: "go test ./...", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:03:00Z" },
    ],
  });
  assert.match(html, /<div class="md"><p>go test \.\/...<\/p><\/div>/);
  assert.match(html, /<span class="duration">3m 0s<\/span>/);
});

// --- findings registry ------------------------------------------------------

function findingsRegistry(overrides = {}) {
  const { findings = [], follow_ups = [], follow_up_sets = [], summary = {} } = overrides;
  return { findings, follow_ups, follow_up_sets, summary };
}

test("the findings view renders one row per finding with resolution labels and summary counts", () => {
  const html = renderFindings({
    projectID: "p-1",
    findings: findingsRegistry({
      findings: [
        { id: "th-1", change_id: "ch-1", file_path: "src/a.go", line: 12, state: "claimed", claim_kind: "fixed", claimed_by: "alice", finding: "**leak** on line 12" },
        { id: "th-2", change_id: "ch-1", file_path: "src/b.go", line: 3, state: "claimed", claim_kind: "not_warranted", claimed_by: "bob", finding: "style nit" },
        { id: "th-3", change_id: "ch-2", file_path: "src/c.go", line: 0, state: "certified", certified_by: "carol", finding: "handled elsewhere" },
        { id: "th-4", change_id: "ch-2", file_path: "", line: 0, state: "open", finding: "still open" },
      ],
      summary: { resolved_fixed: 1, resolved_not_warranted: 1, certified: 1, unresolved: 1 },
    }),
  });
  // One row per finding, in registry order.
  assert.match(html, /data-finding="th-1"/);
  assert.match(html, /data-finding="th-2"/);
  assert.match(html, /data-finding="th-3"/);
  assert.match(html, /data-finding="th-4"/);
  assert.ok(html.indexOf('data-finding="th-1"') < html.indexOf('data-finding="th-2"'));
  // Resolution labels: claim kind + actor, certified-by, unresolved.
  assert.match(html, /fixed by alice/);
  assert.match(html, /not warranted by bob/);
  assert.match(html, /certified by carol/);
  assert.match(html, />unresolved<\/span>/);
  // State badges.
  assert.match(html, /data-state="claimed"/);
  assert.match(html, /data-state="certified"/);
  assert.match(html, /data-state="open"/);
  // File:line anchor into the change, when present.
  assert.match(html, /<a class="anchor" href="\/ui\/changes\/ch-1" data-link>src\/a.go:12<\/a>/);
  // Finding bodies render as block markdown.
  assert.match(html, /<strong>leak<\/strong>/);
  // The summary line carries a count per resolution bucket, zeroes included.
  assert.match(html, /data-bucket="resolved_fixed">fixed 1<\/span>/);
  assert.match(html, /data-bucket="resolved_not_warranted">not warranted 1<\/span>/);
  assert.match(html, /data-bucket="resolved_superseded">superseded 0<\/span>/);
  assert.match(html, /data-bucket="certified">certified 1<\/span>/);
  assert.match(html, /data-bucket="unresolved">unresolved 1<\/span>/);
  assert.match(html, /data-bucket="deferred_to_task">deferred 0<\/span>/);
});

test("deferred follow-up findings render links to their target tasks", () => {
  const html = renderFindings({
    projectID: "p-1",
    findings: findingsRegistry({
      findings: [{ id: "th-1", state: "open", finding: "still open" }],
      follow_ups: [
        { action: "create_task", check_name: "review-aggregator", target_task_id: "t-0099", target_task_title: "Handle lint fallout" },
      ],
      summary: { unresolved: 1, deferred_to_task: 1 },
    }),
  });
  assert.match(html, /deferred to/);
  assert.match(html, /href="\/ui\/tasks\/t-0099" data-link/);
  assert.match(html, /Handle lint fallout/);
  assert.match(html, /t-0099/);
  // The follow-ups section only appears when something was deferred.
  assert.match(html, /<h4>Legacy follow-ups<\/h4>/);
  const noDeferrals = renderFindings({
    findings: findingsRegistry({ findings: [{ id: "th-1", state: "open", finding: "x" }], summary: { unresolved: 1 } }),
  });
  assert.doesNotMatch(noDeferrals, /Follow-ups/);
});

test("organized review follow-up sets render provenance, plans, dispositions, targets, and errors", () => {
  const html = renderFindings({
    projectID: "p-1",
    findings: findingsRegistry({
      follow_up_sets: [{
        id: "rfus-1", source_change_id: "ch-1", revision: 3, state: "attention",
        organizer_task_id: "t-organizer", organizer_task_title: "Organize follow-ups",
        active_plan_artifact_id: "wa-plan", last_error: "materialization needs retry",
        plan: { id: "rfpr-3", state: "failed" },
        batches: [{
          id: "rfub-1", check_name: "review.aggregate.node.1", source_job_id: "j-review",
          reviewed_head_sha: "1234567890abcdef", proposals: [{
            id: "rfp-1", file_path: "src/a.go", line: 14, severity: "medium", body: "**durable** concern",
            suggested_action: "create_task", suggested_title: "Proposed fix", suggested_body: "Implement **carefully**.",
            disposition: {
              disposition: "use_existing_task", target_task_id: "t-42", target_task_title: "Existing fix",
              target_feature_id: "f-1", target_parent_id: "e-1", target_blocker_ids: ["t-10"],
              rationale: "Same root issue",
            },
          }, {
            id: "rfp-2", file_path: "src/b.go", line: 2, severity: "low", body: "pending concern",
            suggested_action: "use_existing_task", suggested_task_id: "t-84", suggested_title: "Prior fix",
          }],
        }],
      }],
    }),
  });
  assert.match(html, /data-follow-up-set="rfus-1" data-state="attention"/);
  assert.match(html, /revision 3 · plan failed/);
  assert.match(html, /href="\/ui\/tasks\/t-organizer" data-link/);
  assert.match(html, /wa-plan/);
  assert.match(html, /materialization needs retry/);
  assert.match(html, /review\.aggregate\.node\.1 · job j-review · head 1234567890ab/);
  assert.match(html, /href="\/ui\/changes\/ch-1" data-link>src\/a\.go:14/);
  assert.match(html, /<strong>durable<\/strong> concern/);
  assert.match(html, /data-suggested-action="create_task"/);
  assert.match(html, /Suggested new task<\/strong> · Proposed fix/);
  assert.match(html, /Implement <strong>carefully<\/strong>\./);
  assert.match(html, /data-suggested-action="use_existing_task"/);
  assert.match(html, /href="\/ui\/tasks\/t-84" data-link>Prior fix<\/a>/);
  assert.match(html, /data-disposition="use_existing_task"/);
  assert.match(html, /reused existing task/);
  assert.match(html, /href="\/ui\/tasks\/t-42" data-link/);
  assert.match(html, /feature f-1 · parent e-1 · blocked by t-10/);
  assert.match(html, /Awaiting organizer disposition/);
});

test("an empty findings registry renders the empty state, not an error", () => {
  assert.match(renderFindings({ findings: findingsRegistry() }), /No review findings recorded/);
  // A task with zero changes carries no registry at all.
  assert.match(renderFindings({}), /No review findings recorded/);
  assert.match(renderFindings(null), /No review findings recorded/);
  // A failed read is its own state, not a false "nothing found".
  assert.match(renderFindings({ findings: { error: "findings load failed" } }), /Findings unavailable/);
});

test("finding bodies render as block markdown and escape raw HTML", () => {
  const html = renderFindings({
    findings: findingsRegistry({
      findings: [{ id: "th-1", state: "open", finding: "First line\n\n<script>alert(1)</script> **bold**" }],
      summary: { unresolved: 1 },
    }),
  });
  assert.match(html, /<div class="md">/);
  assert.ok(!html.includes("<script>"), "raw HTML in a finding body must be neutralized");
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(html, /<strong>bold<\/strong>/);
});

test("the findings element keeps its instance across a poll repaint", async () => {
  const root = globalThis.document.body;
  const registry = () => findingsRegistry({
    findings: [{ id: "th-1", state: "open", finding: "body" }],
    summary: { unresolved: 1 },
  });
  const element = mountElement(root, "flow-findings-list", { projectID: "p-1", findings: registry() });
  await flush();
  assert.match(element.innerHTML, /data-finding="th-1"/);
  // A poll delivers a brand-new model for the same task; the element instance
  // survives and repaints in place instead of being rebuilt.
  const before = element;
  element.data = { projectID: "p-1", findings: registry() };
  await flush();
  assert.strictEqual(element, before);
  assert.match(element.innerHTML, /data-finding="th-1"/);
  element.remove();
});

test("the findings tab sits next to checks and tab badges count unresolved findings", () => {
  const strip = renderTabStrip("checks", { findings: { text: "2", tone: "warn" } });
  assert.ok(strip.indexOf('data-tab="checks"') < strip.indexOf('data-tab="findings"'));
  assert.match(strip, /data-tab="findings">\s*Findings/);

  const model = taskModel({
    task: { id: "t-1", title: "T" },
    task_detail: {},
    findings: { findings: [], follow_ups: [], summary: { unresolved: 2 } },
  });
  assert.equal(tabBadges(model).findings.text, "2");
  assert.equal(tabBadges(model).findings.tone, "warn");
  const clean = taskModel({ task: { id: "t-1", title: "T" }, task_detail: {} });
  assert.equal(tabBadges(clean).findings, undefined);
});

// --- threads tab --------------------------------------------------------------

test("the threads tab sits between the change and the checks", () => {
  const strip = renderTabStrip("change", {});
  assert.ok(strip.indexOf('data-tab="change"') < strip.indexOf('data-tab="threads"'));
  assert.ok(strip.indexOf('data-tab="threads"') < strip.indexOf('data-tab="checks"'));
  assert.match(strip, /data-tab="threads">\s*Threads/);
});

test("the task model projects the change-filtered threads and the task-wide record", () => {
  const model = taskModel({
    task: { id: "t-1", title: "T" },
    task_detail: {},
    threads: [{ id: "th-new", change_id: "ch-2", state: "open" }],
    task_threads: [
      { id: "th-old", change_id: "ch-1", state: "certified" },
      { id: "th-open-old", change_id: "ch-1", state: "open" },
      { id: "th-new", change_id: "ch-2", state: "open" },
    ],
  });
  assert.deepEqual(model.threads.map((thread) => thread.id), ["th-new"], "the change-scoped subset passes through");
  assert.equal(model.openThreads, 1, "openThreads stays change-scoped");
  assert.deepEqual(model.taskThreads.map((thread) => thread.id), ["th-old", "th-open-old", "th-new"]);
  assert.equal(model.taskOpenThreads, 2, "taskOpenThreads counts the task-wide list");

  // Payloads without the task-wide field (older servers, bare test models)
  // fall back to the change's threads, so the tab never sees less than the diff.
  const legacy = taskModel({
    task: { id: "t-1", title: "T" },
    task_detail: {},
    threads: [{ id: "th-1", state: "open" }],
  });
  assert.deepEqual(legacy.taskThreads.map((thread) => thread.id), ["th-1"]);
  assert.equal(legacy.taskOpenThreads, 1);
});

test("the threads tab badge counts task-wide open threads, distinct from the change badge", () => {
  const badges = tabBadges({
    change: { id: "c-1" },
    openThreads: 1,
    taskOpenThreads: 3,
    checks: [],
    transitions: [],
    statusLog: [],
  });
  assert.equal(badges.change.text, "1", "the change badge stays change-scoped");
  assert.deepEqual(badges.threads, { text: "3", tone: "warn" });

  // No open threads anywhere → no badge: an all-resolved record is quiet.
  const quiet = tabBadges({ change: { id: "c-1" }, openThreads: 0, taskOpenThreads: 0, checks: [], transitions: [], statusLog: [] });
  assert.equal(quiet.threads, undefined);
});

test("handing back names every edge the executor can take", () => {
  const html = renderHeldPanel({
    held: true,
    id: "t-0042",
    stepName: "review",
    taskConsole: { session: { id: "s-0413", worker_id: "w-local-1" } },
  });
  assert.match(html, /Held by you/);
  assert.match(html, /paused at review · the workflow will not advance/);
  for (const [edge] of HAND_BACK_EDGES) {
    assert.match(html, new RegExp(`data-edge="${edge}"`), `missing hand-back edge ${edge}`);
  }
  assert.match(html, /s-0413 · w-local-1 · tmux/);
});

test("a typed convergence hold renders immutable evidence and explicit dispositions", () => {
  const html = renderHeldPanel({
    held: true,
    heldBy: "system",
    id: "t-0043",
    stepName: "review",
    convergenceEvidence: {
      schema_version: 1,
      fingerprint: "sha256:reviewed-evidence",
      source_branch: "task/t-0043",
      source_head_sha: "1234567890abcdef1234567890abcdef12345678",
      target_base_branch: "main",
      target_base_tip_sha: "abcdef1234567890abcdef1234567890abcdef12",
      files: 8,
      additions: 420,
      deletions: 160,
      review_cycles_used: 2,
      review_cycle_budget: 3,
      changed_files: [{ path: "internal/<unsafe>.go", additions: 200, deletions: 10 }],
      changed_files_omitted: 7,
    },
  });
  assert.match(html, /Convergence review/);
  assert.match(html, /task\/t-0043@1234567890ab/);
  assert.match(html, /main@abcdef123456/);
  assert.match(html, /8 files · \+420\/-160/);
  assert.match(html, /internal\/&lt;unsafe&gt;\.go/);
  assert.match(html, /data-convergence-note/);
  assert.equal((html.match(/data-evidence-fingerprint="sha256:reviewed-evidence"/g) || []).length, 5);
  for (const disposition of ["accept_scope", "return_to_author", "repair_branch", "promote", "cancel"]) {
    assert.match(html, new RegExp(`data-disposition="${disposition}"`), `missing disposition ${disposition}`);
  }
  assert.doesNotMatch(html, /data-workflow-release/);
});

test("task detail renders active owner rulings and a run-scoped replacement form", () => {
  const html = renderOwnerRulingsPanel({
    id: "t-guided",
    projectID: "p-1",
    runID: "wr-1",
    runState: "running",
    activeRulings: [{ ruling_id: "rule-1", source: "review_scope_decision", body: "Keep compatibility work out of this change." }],
  });
  assert.match(html, /Owner ruling \/ scope guidance/);
  assert.match(html, /rule-1/);
  assert.match(html, /Keep compatibility work out of this change/);
  assert.match(html, /data-owner-ruling="t-guided"/);
  assert.match(html, /data-project="p-1"/);
  assert.match(html, /data-owner-ruling-supersedes/);
  assert.match(html, /value="rule-1"/);
  assert.equal(renderOwnerRulingsPanel({ id: "t-unscheduled" }), "");
});

test("held_by system alone does not masquerade as typed convergence evidence", () => {
  const html = renderHeldPanel({ held: true, heldBy: "system", id: "t-0043", stepName: "review" });
  assert.match(html, /Held by you/);
  assert.match(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-convergence-decision/);
});

test("task rail sends typed convergence holds to their dedicated review panel", () => {
  const html = renderTaskRail({
    id: "t-0043",
    runID: "wr-0043",
    held: true,
    stepCount: 3,
    stepName: "review",
    convergenceEvidence: { schema_version: 1, fingerprint: "sha256:evidence" },
  });
  assert.match(html, /Scope decision required at review/);
  assert.match(html, /use the review panel to continue/);
  assert.doesNotMatch(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-workflow-reset/);
});

test("task rail suppresses invalid controls while typed evidence is temporarily unavailable", () => {
  const html = renderTaskRail({
    id: "t-0043",
    runID: "wr-0043",
    held: true,
    heldBy: "system",
    systemHeld: true,
    canRequestConvergence: true,
    stepCount: 3,
    stepName: "review",
  });
  assert.match(html, /Review state is refreshing at review/);
  assert.doesNotMatch(html, /data-convergence-request/);
  assert.doesNotMatch(html, /data-workflow-release/);
  assert.doesNotMatch(html, /data-workflow-reset/);
});

test("task model projects exact manual convergence eligibility and typed evidence", () => {
  const eligible = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "abc123" }] },
  }, {
    detail: { run: { id: "wr-0042", current_artifact_id: "wa-1", snapshot: { nodes: [] } } },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(eligible.canRequestConvergence, true);

  const stale = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "new-head" }] },
  }, {
    detail: { run: { id: "wr-0042", current_artifact_id: "wa-1", snapshot: { nodes: [] } } },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(stale.canRequestConvergence, false);

  const systemHeld = taskModel({
    task: { id: "t-0042", title: "Scoped change" },
    task_detail: { changes: [{ id: "ch-1", head_sha: "abc123" }] },
  }, {
    detail: {
      run: {
        id: "wr-0042",
        current_artifact_id: "wa-1",
        held_at: "2026-07-31T17:00:00Z",
        held_by: "system",
        snapshot: { nodes: [] },
      },
    },
    artifacts: [{ id: "wa-1", kind: "change", payload: { change_id: "ch-1", head_sha: "abc123" } }],
  });
  assert.equal(systemHeld.systemHeld, true);
  assert.equal(systemHeld.canRequestConvergence, false);

  const evidence = { schema_version: 1, fingerprint: "sha256:evidence" };
  const model = taskModel({
    task: { id: "t-0043", title: "Oversized" },
    task_detail: { convergence_evidence: evidence },
  }, {
    detail: { run: { held_at: "2026-07-30T12:00:00Z", held_by: "system", snapshot: { nodes: [] } } },
  });
  assert.equal(model.convergenceEvidence, evidence);
  assert.equal(model.activity, "Held for convergence review");
});

test("the activity feed merges transitions and status entries newest first", () => {
  const entries = activityEntries({
    transitions: [
      { event_kind: "node_completed", from_node_key: "checks", to_node_key: "review", outcome: "passed", created_at: "2026-01-01T10:00:00Z" },
    ],
    statusLog: [{ kind: "note", message: "Implementing the retry budget", created_at: "2026-01-01T09:00:00Z" }],
  });
  assert.equal(entries.length, 2);
  assert.equal(entries[0].text, "checks → review · passed");
  assert.match(renderActivityFeed({ transitions: [], statusLog: [] }), /No activity yet/);
});

// --- change review ---------------------------------------------------------

test("every diff line carries a comment affordance in its gutter", () => {
  const html = renderDiffFile({
    path: "internal/lifecycle/engine.go",
    hunks: [
      {
        header: "@@ -210,6 +210,8 @@",
        lines: [
          { kind: "context", new_line: 210, text: "if run.Budget <= 0 {" },
          { kind: "delete", old_line: 211, text: "  return ErrBudgetExhausted" },
          { kind: "add", new_line: 211, text: "  return e.pauseForOperator(ctx, run)" },
        ],
      },
    ],
  });
  assert.equal((html.match(/data-comment-line=/g) || []).length, 3);
  assert.match(html, /aria-label="Comment on line 210"/);
});

test("a deleted line keeps its minus mark and deletion styling hook", () => {
  // The server (internal/git parseDiffLine) emits kind "delete" for '-' lines;
  // the gutter mark and the diff.module.css [data-kind="delete"] rules both
  // hang off that exact string.
  const html = renderDiffFile({
    path: "a.go",
    hunks: [
      {
        header: "@@ -1,2 +1,2 @@",
        lines: [
          { kind: "delete", old_line: 1, text: "old" },
          { kind: "add", new_line: 1, text: "new" },
        ],
      },
    ],
  });
  assert.match(html, /<div class="line" data-kind="delete">[\s\S]*?>−<\/button>/);
  assert.match(html, /<div class="line" data-kind="add">[\s\S]*?>\+<\/button>/);
});

test("a drafted line keeps its note and says so", () => {
  const drafts = new Map([["a.go:12", { path: "a.go", line: 12, body: "cap the wait" }]]);
  const html = renderDiffFile(
    { path: "a.go", hunks: [{ header: "@@", lines: [{ kind: "context", new_line: 12, text: "x" }] }] },
    { drafts },
  );
  assert.match(html, /data-drafted/);
  assert.match(html, /comment on line 12/);
  assert.match(html, /cap the wait/);
});

test("a drafted line offers an explicit scope ruling that defaults to blocking", () => {
  const drafts = new Map([["a.go:12", { path: "a.go", line: 12, body: "cap the wait", disposition: "introduced_by_change" }]]);
  const html = renderDiffFile(
    { path: "a.go", hunks: [{ header: "@@", lines: [{ kind: "context", new_line: 12, text: "x" }] }] },
    { drafts },
  );
  assert.match(html, /checked data-draft-disposition="introduced_by_change"|data-draft-disposition="introduced_by_change" checked/);
  assert.match(html, /data-draft-disposition="preexisting"/);
  assert.match(html, /Introduced by this change/);
  assert.match(html, /Pre-existing/);

  // A pre-existing ruling survives a repaint instead of resetting to blocking.
  const carried = new Map([["a.go:12", { path: "a.go", line: 12, body: "predates this", disposition: "preexisting" }]]);
  const reRendered = renderDiffFile(
    { path: "a.go", hunks: [{ header: "@@", lines: [{ kind: "context", new_line: 12, text: "x" }] }] },
    { drafts: carried },
  );
  assert.match(reRendered, /checked data-draft-disposition="preexisting"|data-draft-disposition="preexisting" checked/);
  assert.doesNotMatch(reRendered, /checked data-draft-disposition="introduced_by_change"|data-draft-disposition="introduced_by_change" checked/);
});

test("the review bar states how many notes ride along with the verdict", () => {
  assert.match(renderReviewBar(3), /3 pending inline notes will be posted with this/);
  assert.match(renderReviewBar(1), /1 pending inline note will be posted/);
  assert.match(renderReviewBar(0), /placeholder="Overall comment"/);
});

// --- epic ------------------------------------------------------------------

test("epic members reduce to the one word the rollup groups by", () => {
  assert.equal(memberState({ needs_you: true }), "needs you");
  assert.equal(memberState({ resolution: "merged" }), "merged");
  assert.equal(memberState({ blocked_by: ["t-1"] }), "blocked");
  assert.equal(memberState({ state: "in_progress" }), "working");
});

test("feature count and divergence labels compress to one short legend", () => {
  assert.equal(featureCountsLabel({ open: 1, scheduled: 2, in_progress: 1, done: 3 }), "1 open · 2 scheduled · 1 working · 3 done");
  assert.equal(featureCountsLabel({}), "no tasks");
  assert.equal(featureDivergenceLabel({ ahead: 0, behind: 0 }), "up to date");
  assert.equal(featureDivergenceLabel({ ahead: 3, behind: 2 }), "3 ahead · 2 behind");
  assert.equal(featureDivergenceLabel(null), "");
});

test("features list sorts open before landed and renders counts, divergence and the create form", () => {
  const html = renderFeatures({
    projectID: "p-alpha",
    features: [
      {
        feature: { id: "f-alpha-0002", title: "billing rework", status: "landed", landed_at: "2026-01-01T00:00:00Z" },
        counts: { done: 4 },
        branch_state: { ahead: 0, behind: 0 },
      },
      {
        feature: { id: "f-alpha-0001", title: "payments", status: "open" },
        counts: { open: 2, in_progress: 1 },
        branch_state: { ahead: 3, behind: 2 },
        running_rebase: { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009" },
      },
    ],
  });
  const openIndex = html.indexOf("payments");
  const landedIndex = html.indexOf("billing rework");
  assert.ok(openIndex !== -1 && landedIndex !== -1 && openIndex < landedIndex, "open feature sorts first");
  assert.match(html, /href="\/ui\/projects\/p-alpha\/features\/f-alpha-0001"/);
  assert.match(html, /2 open · 1 working/);
  assert.match(html, /3 ahead · 2 behind/);
  assert.match(html, /rebasing/);
  assert.match(html, /data-feature-form="" data-project="p-alpha"/);
  assert.match(html, /data-landed/);
});

test("feature detail renders actions for an open feature and none once landed", () => {
  const open = renderFeature({
    projectID: "p-alpha",
    feature: { id: "f-alpha-0001", title: "payments", status: "open", branch: "feature/f-alpha-0001", body: "payment work" },
    counts: { open: 1 },
    branch_state: { ahead: 1, behind: 2 },
    running_rebase: { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009" },
    tasks: [
      { id: "t-alpha-0003", title: "scoped work" },
      { id: "t-alpha-0004", title: "merged work", state: "done", done_resolution: "merged" },
    ],
    rebases: [
      { id: "rb-alpha-0001", state: "running", task_id: "t-alpha-0009", old_tip_sha: "abc123", created_at: "2026-01-01T00:00:00Z" },
    ],
  });
  assert.match(open, /data-feature-rebase="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /data-feature-land="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /data-feature-archive="f-alpha-0001" data-project="p-alpha"/);
  assert.match(open, /Rebase in progress/);
  assert.match(open, /href="\/ui\/projects\/p-alpha\/tasks\/t-alpha-0009\?context=f-alpha-0001&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Ffeatures%2Ff-alpha-0001"/);
  assert.match(open, /done \(merged\)/);
  assert.match(open, /data-feature-form="f-alpha-0001" data-project="p-alpha"/);

  const landed = renderFeature({
    projectID: "p-alpha",
    feature: { id: "f-alpha-0001", title: "payments", status: "landed", branch: "feature/f-alpha-0001", landed_at: "2026-01-02T00:00:00Z", land_sha: "def456" },
    counts: { done: 2 },
  });
  assert.doesNotMatch(landed, /data-feature-rebase/);
  assert.doesNotMatch(landed, /data-feature-form/);
  assert.match(landed, /landed/);
});

test("the epic rollup bar and its legend are built from one grouping", () => {
  const html = renderEpic({
    epic: { id: "t-0030", title: "Lifecycle hardening" },
    total_count: 3,
    merged_count: 1,
    members: [
      { id: "t-0039", title: "A", needs_you: true, dwell_since: new Date(Date.now() - 41 * 60_000).toISOString() },
      { id: "t-0036", title: "B", resolution: "merged" },
      { id: "t-0043", title: "C", state: "in_progress", step_index: 1, step_count: 6, step_name: "implement" },
    ],
    critical_path: ["t-0036", "t-0043"],
  });
  assert.match(html, /1 needs you/);
  assert.match(html, /1 merged/);
  assert.match(html, /data-merged/);
  assert.match(html, /implement 1\/6/);
  assert.match(html, /Critical path/);
});

test("legacy epic member and critical-path task links preserve epic context when route-mounted", () => {
  const html = renderEpic({
    projectID: "p-alpha",
    currentHref: "/ui/projects/p-alpha/epics/e-1?return=%2Fui%2Fprojects%2Fp-alpha%2Fwork-items",
    epic: { id: "e-1", title: "Legacy epic" },
    members: [{ id: "t-1", title: "Member" }],
    critical_path: ["t-1"],
  });
  const expected = /href="\/ui\/projects\/p-alpha\/tasks\/t-1\?context=e-1&amp;return=%2Fui%2Fprojects%2Fp-alpha%2Fepics%2Fe-1%3Freturn%3D%252Fui%252Fprojects%252Fp-alpha%252Fwork-items"/g;
  assert.equal((html.match(expected) || []).length, 2);
  assert.doesNotMatch(html, /href="\/ui\/tasks\/t-1"/);
});

test("an epic member note dwells on the render clock, not the wall clock", () => {
  // The epic surface has no injected model clock; renderEpic captures one
  // clock for the whole render. A fixed clock far from the real wall clock
  // must drive the member note (the real clock would report the dwell as days
  // old while the render says 1h).
  const now = Date.parse("2026-07-29T12:00:00Z");
  const html = renderEpic(
    {
      epic: { id: "t-0030", title: "Lifecycle hardening" },
      members: [
        { id: "t-0039", title: "A", needs_you: true, dwell_since: "2026-07-29T11:00:00Z" },
        { id: "t-0036", title: "B", resolution: "merged" },
      ],
    },
    now,
  );
  assert.match(html, /needs you 1h/);
});

// --- element lifecycle -----------------------------------------------------

test("the throughput strip renders all eight buckets in ascending window order", () => {
  const html = renderThroughputStrip({
    buckets: [
      { window: "15m", count: 3 },
      { window: "30m", count: 8 },
      { window: "1h", count: 11 },
      { window: "2h", count: 17 },
      { window: "4h", count: 23 },
      { window: "6h", count: 28 },
      { window: "12h", count: 35 },
      { window: "24h", count: 40 },
    ],
  });

  // Windows in ascending order, each with its cumulative count.
  const windows = [...html.matchAll(/<span class="window">([^<]+)<\/span>/g)].map((match) => match[1]);
  assert.deepEqual(windows, ["15m", "30m", "1h", "2h", "4h", "6h", "12h", "24h"]);
  const counts = [...html.matchAll(/<span class="count">([^<]+)<\/span>/g)].map((match) => match[1]);
  assert.deepEqual(counts, ["3", "8", "11", "17", "23", "28", "35", "40"]);

  // Leading success marker plus a trailing link to the completed work.
  assert.match(html, /<span class="mark"[^>]*>✓<\/span>/);
  assert.match(html, /<span class="label">Done<\/span>/);
  assert.match(html, /href="\/ui\/tasks\?state=done"/);
});

test("null or absent stats render an empty (hidden) throughput strip", () => {
  assert.equal(renderThroughputStrip(null), "");
  assert.equal(renderThroughputStrip(undefined), "");
  assert.equal(renderThroughputStrip({}), "");
  assert.equal(renderThroughputStrip({ buckets: [] }), "");
});

test("the throughput strip element hides itself while stats are null", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-throughput-strip", null);
  await flush();

  assert.equal(strip.hasAttribute("hidden"), true);
  assert.equal(strip.innerHTML, "");

  // A poll hands the existing element fresh data: the instance survives, so
  // a chip under the pointer keeps its hover and focus.
  const first = strip;
  strip.data = { buckets: [{ window: "1h", count: 11 }] };
  await flush();
  assert.equal(first, strip);
  assert.equal(strip.hasAttribute("hidden"), false);
  assert.match(strip.innerHTML, />11<\/span>/);
  assert.match(strip.innerHTML, />1h<\/span>/);
  strip.remove();
});

test("the board feeds the throughput strip fresh stats on every poll", async () => {
  const root = globalThis.document.body;
  const board = mountElement(root, "flow-board", {
    entries: [],
    showProject: false,
    stats: { buckets: [{ window: "15m", count: 3 }] },
  });
  await flush();

  const strip = board.querySelector("flow-throughput-strip");
  assert.ok(strip, "the board renders the throughput strip between the attention strip and the surface");
  assert.equal(strip.hasAttribute("hidden"), false);
  assert.match(strip.innerHTML, />3<\/span>/);
  assert.match(strip.innerHTML, />15m<\/span>/);

  // A poll sets fresh data on the same board element: the strip keeps its
  // instance — so a chip under the pointer keeps its hover and focus — and
  // picks up the new counts.
  board.data = {
    entries: [],
    showProject: false,
    stats: { buckets: [{ window: "15m", count: 3 }, { window: "30m", count: 8 }] },
  };
  await flush();
  assert.equal(board.querySelector("flow-throughput-strip"), strip, "a poll must not re-create the strip");
  assert.match(strip.innerHTML, />8<\/span>/);
  assert.match(strip.innerHTML, />30m<\/span>/);
  board.remove();
});

test("reconcile keeps the element for a surviving key so its state outlives a poll", async () => {
  const root = globalThis.document.body;
  const container = globalThis.document.createElement("div");
  root.appendChild(container);

  const first = reconcile(container, [{ id: "a" }, { id: "b" }], { tag: "flow-task-card", key: (item) => item.id });
  first[0].marker = "kept";

  const second = reconcile(container, [{ id: "b" }, { id: "a" }, { id: "c" }], {
    tag: "flow-task-card",
    key: (item) => item.id,
  });
  const kept = second.find((element) => element.dataset.key === "a");
  assert.equal(kept.marker, "kept", "a surviving key must keep its element instance");
  assert.equal(second.length, 3);
  assert.deepEqual(
    container.children.map((child) => child.dataset.key),
    ["b", "a", "c"],
    "reconcile reorders in place",
  );

  const third = reconcile(container, [{ id: "a" }], { tag: "flow-task-card", key: (item) => item.id });
  assert.equal(third.length, 1);
  assert.equal(container.children.length, 1, "removed keys must be dropped");
  container.remove();
});

test("the tab strip keeps the active tab across a repaint", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  strip.select("checks");
  await flush();
  assert.equal(strip.active, "checks");

  // A poll sets fresh data; the tab the reader chose must survive it.
  strip.data = { badges: { checks: { text: "3/4", tone: "danger" } } };
  await flush();
  assert.equal(strip.active, "checks");
  assert.match(strip.innerHTML, /data-tab="checks"[^>]*aria-selected="true"/);
  strip.remove();
});

test("a delegated listener survives the element replacing its own innerHTML", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  let changes = 0;
  strip.addEventListener("tab-change", () => {
    changes += 1;
  });
  for (const key of ["checks", "activity", "overview"]) {
    strip.select(key);
    await flush();
  }
  assert.equal(changes, 3, "the click listener must not be lost to repaints");
  strip.remove();
});

test("an unchanged render does not rewrite the DOM", async () => {
  const root = globalThis.document.body;
  const lane = mountElement(root, "flow-lane", { key: "queued", label: "Scheduled", cards: [] });
  await flush();
  const painted = lane.innerHTML;

  let writes = 0;
  const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(lane), "innerHTML");
  Object.defineProperty(lane, "innerHTML", {
    get: () => painted,
    set(value) {
      writes += 1;
      descriptor.set.call(lane, value);
    },
    configurable: true,
  });

  lane.data = { key: "queued", label: "Scheduled", cards: [] };
  await flush();
  assert.equal(writes, 0, "identical markup should not be rewritten; focus and scroll would be lost");
  lane.remove();
});

test("a click bubbles to the app root, which is where actions are dispatched", async () => {
  const root = globalThis.document.body;
  const strip = mountElement(root, "flow-tab-strip", { badges: {} });
  await flush();

  let seen = null;
  root.addEventListener("click", (event) => {
    seen = event.target;
  });
  const tab = strip.querySelector('[data-tab="activity"]');
  tab.dispatchEvent(new TestEvent("click", { bubbles: true }));
  assert.equal(seen, tab);
  strip.remove();
});

