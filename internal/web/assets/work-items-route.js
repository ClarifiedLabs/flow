// Unified project organization route. Typed summaries carry their direct
// parent id, so the browser can assemble the whole forest in one request.

import { apiGet, workItemsAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { defaultCreateProject } from "./task-view.js";
import "./elements/work-items.js";

export async function renderWorkItemsRoute(app, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) {
    app.setTitle("Work Items");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  const data = await apiGet(workItemsAPIBase(id));
  if (context && !app.isActiveLoad(context)) return false;
  const project = (app.projects || []).find((candidate) => (candidate.id || candidate.ID) === id);
  app.setTitle("Work Items");
  mount(app.querySelector(".content"), "flow-work-items", {
    ...data,
    projectID: id,
    projectName: (app.projects || []).length > 1 ? (project?.name || project?.Name || "") : "",
  });
  return true;
}
