// Console handlers: per-task console start/release plus the Console
// view's own controls, whose request and reload live in console-view.js.

import { apiDelete, apiPost, taskConsoleAPIPath } from "../api.js";
import { releaseConsoleView, startConsoleView } from "../console-view.js";

export const consoleActions = {
  async startTaskConsole(app, element, dataset) {
    await apiPost(taskConsoleAPIPath(dataset.project, dataset.startTaskConsole), { harness: "harness" });
    await app.refresh();
    return "Task console starting";
  },

  async releaseTaskConsole(app, element, dataset) {
    await apiDelete(taskConsoleAPIPath(dataset.project, dataset.releaseTaskConsole));
    await app.refresh();
    return "Task console released";
  },

  // startConsole/releaseConsole are the Console view's own controls. Their
  // target is the console pair (project, task) — the task empty for a project
  // console — and Start posts the harness picked in the view's select. The
  // request and the reload live in the view module; the pending state and the
  // status line are the dispatcher's, as for every action.
  async startConsole(app, element, dataset) {
    const harness = app.querySelector?.("[data-console-harness]")?.value || "harness";
    await startConsoleView(app, dataset.project || "", harness, dataset.task || "");
    return "Console starting";
  },

  async releaseConsole(app, element, dataset) {
    await releaseConsoleView(app, dataset.project || "", dataset.task || "");
    return "Console released";
  },
};
