// <flow-diagnostics> tests: the jobs table's project filter and global sort
// (ported from app.test.mjs when the view became an element), plus the
// filter/sort controls' delegated-change repaint.

import assert from "node:assert/strict";
import test from "node:test";
import { flush, installTestDOM, mountElement } from "./test-dom.mjs";

const root = installTestDOM();
await import("./elements/diagnostics.js");

test("jobs view shows project column, filters by project, and sorts by updated", async () => {
  const element = mountElement(root, "flow-diagnostics", {
    kind: "jobs",
    jobs: [
      // Intentionally out of updated order across two projects to prove the
      // view re-sorts globally rather than trusting server order.
      { id: "j-old", state: "finished", role: "ci", updated_at: "2026-06-01T00:00:00Z" },
      { id: "j-mid", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
      { id: "j-new", state: "failed", role: "reviewer", updated_at: "2026-06-09T00:00:00Z" },
    ],
    diagnostics: {
      "j-old": { project_name: "beta" },
      "j-mid": { project_name: "alpha" },
      "j-new": { project_name: "beta" },
    },
  });
  await flush();

  const html = element.innerHTML;
  // Project column renders the per-job project name.
  assert.match(html, /<th>Project<\/th>/);
  assert.match(html, /alpha/);
  assert.match(html, /beta/);
  // Default sort is updated desc, so j-new (Jun 9) precedes j-mid (Jun 5)
  // which precedes j-old (Jun 1).
  const newIdx = html.indexOf("j-new");
  const midIdx = html.indexOf("j-mid");
  const oldIdx = html.indexOf("j-old");
  assert.ok(newIdx > -1 && midIdx > -1 && oldIdx > -1, "all job rows rendered");
  assert.ok(newIdx < midIdx, "j-new before j-mid");
  assert.ok(midIdx < oldIdx, "j-mid before j-old");
  // Filter and sort controls are present with the default selection.
  assert.match(html, /data-jobs-filter/);
  assert.match(html, /data-jobs-sort-field/);
  assert.match(html, /data-jobs-sort-order/);
  assert.match(html, /<option value="updated" selected>Updated<\/option>/);
  assert.match(html, /<option value="desc" selected>Newest first<\/option>/);
  // State colors render via row tint classes.
  assert.match(html, /class="row-ok"/);
  assert.match(html, /class="row-run"/);
  assert.match(html, /class="row-danger"/);
});

test("jobs view filter selects only the chosen project", async () => {
  const element = mountElement(root, "flow-diagnostics", {
    kind: "jobs",
    jobs: [
      { id: "j-a", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
      { id: "j-b", state: "running", role: "author", updated_at: "2026-06-09T00:00:00Z" },
    ],
    diagnostics: {
      "j-a": { project_name: "alpha" },
      "j-b": { project_name: "beta" },
    },
  });
  // Pretend the user picked the "beta" project filter before this render.
  element.filter = "beta";
  await flush();

  const html = element.innerHTML;
  assert.match(html, /j-b/);
  assert.doesNotMatch(html, /j-a/);
  // The beta option is the selected one.
  assert.match(html, /<option value="beta" selected>beta<\/option>/);
});

test("jobs sort controls repaint the table without a remount", async () => {
  const element = mountElement(root, "flow-diagnostics", {
    kind: "jobs",
    jobs: [
      { id: "j-a", state: "running", role: "author", created_at: "2026-06-09T00:00:00Z", updated_at: "2026-06-05T00:00:00Z" },
      { id: "j-b", state: "running", role: "author", created_at: "2026-06-01T00:00:00Z", updated_at: "2026-06-09T00:00:00Z" },
    ],
    diagnostics: {},
  });
  await flush();
  assert.ok(element.innerHTML.indexOf("j-b") < element.innerHTML.indexOf("j-a"), "updated desc by default");

  const fieldSelect = element.querySelector("[data-jobs-sort-field]");
  fieldSelect.value = "created";
  fieldSelect.dispatchEvent(new Event("change", { bubbles: true }));
  await flush();
  assert.ok(element.innerHTML.indexOf("j-a") < element.innerHTML.indexOf("j-b"), "created field resorts");

  const orderSelect = element.querySelector("[data-jobs-sort-order]");
  orderSelect.value = "asc";
  orderSelect.dispatchEvent(new Event("change", { bubbles: true }));
  await flush();
  assert.ok(element.innerHTML.indexOf("j-b") < element.innerHTML.indexOf("j-a"), "oldest first reverses the rows");
});

test("workers view renders the queue summary and one row per worker", async () => {
  const element = mountElement(root, "flow-diagnostics", {
    kind: "workers",
    workers: [{ id: "w-1", state: "active" }],
    diagnostics: {},
    queue: {},
  });
  await flush();
  assert.match(element.innerHTML, /<th>Worker<\/th>/);
  assert.match(element.innerHTML, /w-1/);
});
