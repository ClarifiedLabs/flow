import assert from "node:assert/strict";
import test from "node:test";
import { installTestDOM } from "./test-dom.mjs";

installTestDOM();

const {
  activeWorkProject,
  isWorkPath,
  projectWorkHref,
  renderWorkNav,
  resolveWorkNavigation,
  safeWorkReturnTarget,
  workNavigationState,
  workViewHref,
} = await import("./work-nav.js");
const { workRouteState } = await import("./work-items-route.js");

test("Work route recognition covers list and deep links without claiming unrelated routes", () => {
  for (const path of ["/ui/work-items", "/ui/tasks", "/ui/tasks/t-1", "/ui/features/f-1", "/ui/projects/p-a/epics/e-1", "/ui/projects/p-a/tasks/t-1"]) assert.equal(isWorkPath(path), true, path);
  for (const path of ["/ui/board", "/ui/done", "/ui/changes/c-1"]) assert.equal(isWorkPath(path), false, path);
});

test("shared Work navigation keeps project context and practical query deep links", () => {
  assert.equal(workViewHref("overview", "p a", "?state=done&project=old"), "/ui/projects/p%20a/work-items?state=done");
  assert.equal(workViewHref("tasks", "p a", "?state=done"), "/ui/tasks?state=done&project=p+a");
  assert.equal(workViewHref("branches", "p a"), "/ui/projects/p%20a/features");
  const html = renderWorkNav({ active: "tasks", projects: [{ id: "p-a", name: "Alpha" }, { id: "p-b", name: "Beta" }], projectID: "p-b", search: "?state=done" });
  assert.match(html, /Active Work project/);
  assert.match(html, /<option value="p-b" selected>Beta<\/option>/);
  assert.match(html, /href="\/ui\/tasks\?state=done&amp;project=p-b"[^>]*aria-current="page"/);
});

test("active Work project honors explicit, valid stored/selected, then registered fallback", () => {
  window.localStorage.setItem("flow.ui.workProject", "p-b");
  const app = { projects: [{ id: "p-a" }, { id: "p-b" }], selectedProjectIDs: () => ["p-a", "p-b"] };
  assert.equal(activeWorkProject(app, "p-a"), "p-a");
  assert.equal(activeWorkProject(app), "p-b");
  window.localStorage.setItem("flow.ui.workProject", "missing");
  assert.equal(activeWorkProject(app), "p-a");
});

test("return targets stay same-origin under /ui and normalize query/hash", () => {
  const origin = "https://flow.example:8443";
  for (const [target, expected] of [
    ["/ui/tasks?state=done#queue", "/ui/tasks?state=done#queue"],
    ["https://flow.example:8443/ui/projects/p-a/features/f-1?tab=work#child", "/ui/projects/p-a/features/f-1?tab=work#child"],
    ["/ui/%66eatures/f-1", "/ui/%66eatures/f-1"],
  ]) assert.equal(safeWorkReturnTarget(target, "p-a", origin), expected, target);
});

test("external, encoded, protocol-relative, and non-UI returns fall back to project Work", () => {
  const origin = "https://flow.example";
  const fallback = projectWorkHref("p a");
  for (const target of [
    "https://evil.example/ui/tasks",
    "//evil.example/ui/tasks",
    "https:%2F%2Fevil.example%2Fui%2Ftasks",
    "%2F%2Fevil.example%2Fui%2Ftasks",
    "/v2/tasks/t-1",
    "/ui/../v2/tasks/t-1",
    "javascript:alert(1)",
  ]) assert.equal(safeWorkReturnTarget(target, "p a", origin), fallback, target);
});

test("navigation parsing preserves valid state and stale context discards its return", () => {
  assert.deepEqual(
    workNavigationState("?context=f-1&return=%2Fui%2Ftasks%3Froot%3De-1%23group", "p-a"),
    { context: "f-1", returnTo: "/ui/tasks?root=e-1#group" },
  );
  assert.deepEqual(
    resolveWorkNavigation({ context: "f-stale", returnTo: "/ui/projects/p-a/features/f-stale" }, ["e-1", "f-1"], "p-a"),
    { context: "", returnTo: "/ui/projects/p-a/work-items", contextValid: false },
  );
});

test("overview route query validates filter/view while preserving search", () => {
  const preferences = { filter: "open", view: "overview" };
  assert.deepEqual(workRouteState("?filter=blocked&view=tree&q=payments", preferences), { filter: "blocked", view: "tree", query: "payments" });
  assert.deepEqual(workRouteState("?filter=ready&view=graph", preferences), { filter: "open", view: "overview", query: "" });
});
