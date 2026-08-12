// Console handlers: per-task console start/release plus the Console
// view's own controls, whose request and reload live in the helpers below.

import { apiDelete, apiPost, consoleAPIPath, taskConsoleAPIPath } from "../api.js";

// startConsoleView/releaseConsoleView are the Console view's own controls'
// request-and-reload pair. Their target is the console pair (project, task)
// — the task empty for a project console — and Start posts the harness picked
// in the view's select. The pending state and the status line are the
// dispatcher's, as for every action.
export async function startConsoleView(app, projectID, harness, taskID = "") {
  await apiPost(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID), { harness });
  await app.load();
}

export async function releaseConsoleView(app, projectID, taskID = "") {
  await apiDelete(taskID ? taskConsoleAPIPath(projectID, taskID) : consoleAPIPath(projectID));
  await app.load();
}

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
