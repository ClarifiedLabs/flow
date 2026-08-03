// First-class, project-scoped epic route.

import { apiGet, epicsAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { defaultCreateProject } from "./task-view.js";
import "./elements/epic.js";

export async function renderEpicRoute(app, epicID, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) {
    app.setTitle("Epic");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  const data = await apiGet(`${epicsAPIBase(id)}/${encodeURIComponent(epicID)}`);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Epic");
  mount(app.querySelector(".content"), "flow-epic", { ...data, projectID: id });
  return true;
}
