// The epic route. An epic is a task whose flow produced other tasks; this is
// the zoomed-out read of that group.

import { apiGet, taskAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import "./elements/epic.js";

export async function renderEpicRoute(app, taskID, context, projectID = "") {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}/epic`);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Epic");
  mount(app.querySelector(".content"), "flow-epic", { ...data, projectID });
  return true;
}
