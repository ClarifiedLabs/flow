// Client-side route table consumed by FlowApp.load(). Each entry's
// match(path) returns a truthy params object/flag when it handles the path,
// or a falsy value to fall through to the next entry. Order matters:
// specific paths first, the board as the catch-all last. render() receives
// the app instance, the load context and the matched params.

import { terminalSessionIDForPath } from "../storage.js";
import { renderWorkersView, renderJobsView } from "../diagnostics-view.js";
import { renderTerminalView } from "../terminal-view.js";
import { renderDoneRoute } from "../done-route.js";
import { renderTasksView } from "../tasks-view.js";
import { renderBoardRoute } from "../board-route.js";
import { renderTaskRoute } from "../task-route.js";
import { renderChangeRoute } from "../change-route.js";
import { renderEpicRoute } from "../epic-route.js";
import { renderFeaturesRoute, renderFeatureRoute } from "../features-route.js";
import { renderWorkItemsRoute } from "../work-items-route.js";
import { renderNewTaskView } from "../task-view.js";
import { renderFlowsView } from "../flows-view.js";

export const ROUTES = [
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/work-items$/);
      return m && { project: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderWorkItemsRoute(app, ctx, p.project),
  },
  { match: (p) => p === "/ui/work-items", render: (app, ctx) => renderWorkItemsRoute(app, ctx, "") },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/epics\/([^/]+)$/);
      return m && { project: decodeURIComponent(m[1]), epic: decodeURIComponent(m[2]) };
    },
    render: (app, ctx, p) => renderEpicRoute(app, p.epic, ctx, p.project),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/epics\/([^/]+)$/);
      return m && { epic: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderEpicRoute(app, p.epic, ctx, ""),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/features\/([^/]+)$/);
      return m && { project: decodeURIComponent(m[1]), ref: decodeURIComponent(m[2]) };
    },
    render: (app, ctx, p) => renderFeatureRoute(app, p.ref, ctx, p.project),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/features$/);
      return m && { project: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderFeaturesRoute(app, ctx, p.project),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/features\/([^/]+)$/);
      return m && { ref: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderFeatureRoute(app, p.ref, ctx, ""),
  },
  { match: (p) => p === "/ui/features", render: (app, ctx) => renderFeaturesRoute(app, ctx, "") },
  { match: (p) => p === "/ui/tasks/new", render: (app, ctx) => renderNewTaskView(app, ctx) },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/tasks\/([^/]+)$/);
      return m && { task: decodeURIComponent(m[1]) };
    },
    render: (app, ctx, p) => renderTaskRoute(app, p.task, ctx),
  },
  {
    match: (p) => {
      const m = p.match(/^\/ui\/projects\/([^/]+)\/tasks\/([^/]+)$/);
      return m && { project: decodeURIComponent(m[1]), task: decodeURIComponent(m[2]) };
    },
    render: (app, ctx, p) => renderTaskRoute(app, p.task, ctx, p.project),
  },
  { match: (p) => p.startsWith("/ui/changes/") && { id: p.split("/").pop() }, render: (app, ctx, p) => renderChangeRoute(app, p.id, ctx) },
  { match: (p) => p === "/ui/console", render: (app, ctx) => app.renderConsole(ctx) },
  { match: (p) => p === "/ui/tasks", render: (app, ctx) => renderTasksView(app, ctx) },
  { match: (p) => { const id = terminalSessionIDForPath(p); return id && { id }; }, render: (app, ctx, p) => renderTerminalView(app, p.id, ctx) },
  { match: (p) => p === "/ui/flows", render: (app, ctx) => renderFlowsView(app, ctx) },
  { match: (p) => p === "/ui/workers", render: (app, ctx) => renderWorkersView(app, ctx) },
  { match: (p) => p === "/ui/jobs", render: (app, ctx) => renderJobsView(app, ctx) },
  { match: (p) => p === "/ui/done", render: (app, ctx) => renderDoneRoute(app, ctx) },
  { match: () => true, render: (app, ctx) => renderBoardRoute(app, ctx) },
];
