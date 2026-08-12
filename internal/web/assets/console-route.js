// Console route: resolve the project, fetch the console state (and the
// terminal token when one is live), and mount <flow-console>. The element
// owns the console refresh poll from there.

import { apiGet, apiPost, consoleAPIPath, taskConsoleAPIPath } from "./api.js";
import { DEFAULT_CONSOLE_HARNESSES } from "./config.js";
import { renderHarnessOptions } from "./models/harness-form.js";
import { value } from "./normalize.js";
import { mount } from "./elements/base.js";
import { resolveConsoleProject } from "./elements/console.js";

export async function renderConsoleRoute(app, context) {
  app.setTitle("Console");
  await app.ensureHarnesses();
  if (context && !app.isActiveLoad(context)) return false;
  const params = new URLSearchParams(window.location.search);
  const selectedTask = params.get("task") || "";
  const project = resolveConsoleProject(app, params.get("project") || "");
  if (!project) {
    if (context && !app.isActiveLoad(context)) return false;
    mount(app.querySelector(".content"), "flow-console", { chooser: true, projects: app.projects || [] });
    return true;
  }

  const data = await apiGet(selectedTask ? taskConsoleAPIPath(project.id, selectedTask) : consoleAPIPath(project.id));
  if (context && !app.isActiveLoad(context)) return false;
  const job = data.job || data.Job || null;
  const session = data.session || data.Session || null;
  const active = Boolean(data.active || data.Active || job || session);
  const projectID = data.project_id || data.ProjectID || project.id || "";
  const terminalAvailable = Boolean(data.terminal_available || data.TerminalAvailable);

  let loginPath = "";
  if (session && terminalAvailable) {
    const sessionID = value(session, "id", "ID");
    const accessData = await apiPost(`/v2/sessions/${encodeURIComponent(sessionID)}/terminal-token`, {});
    if (context && !app.isActiveLoad(context)) return false;
    loginPath = value(accessData.access || accessData.Access || {}, "login_path", "LoginPath");
  }

  mount(app.querySelector(".content"), "flow-console", {
    project,
    projectID,
    selectedTask,
    job,
    session,
    active,
    terminalAvailable,
    loginPath,
    harnessOptions: renderHarnessOptions((app.harnesses && app.harnesses.consoles) || DEFAULT_CONSOLE_HARNESSES, "harness"),
  });
  return true;
}
