// Unified project organization route. Typed summaries carry their direct
// parent id, so the browser can assemble the whole forest in one request.

import { apiGet, workItemsAPIBase, workItemsOverviewAPIPath } from "./api.js";
import { mount } from "./elements/base.js";
import { WORK_ITEM_FILTERS, WORK_ITEM_VIEWS } from "./work-item-model.js";
import { readWorkPreferences, writeWorkProject } from "./storage.js";
import { activeWorkProject, currentWorkReturnTarget } from "./work-nav.js";
import "./elements/work-items.js";

export function workRouteState(search, preferences) {
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  const filter = params.get("filter");
  const view = params.get("view");
  return {
    filter: WORK_ITEM_FILTERS.has(filter) ? filter : preferences.filter,
    view: WORK_ITEM_VIEWS.has(view) ? view : preferences.view,
    query: String(params.get("q") || ""),
  };
}

export async function renderWorkItemsRoute(app, context, projectID = "") {
  const params = new URLSearchParams(window.location.search);
  const id = activeWorkProject(app, projectID || params.get("project") || "");
  if (!id) {
    app.setTitle("Work");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  writeWorkProject(id);
  const data = await apiGet(workItemsOverviewAPIPath(id));
  if (context && !app.isActiveLoad(context)) return false;
  const preferences = readWorkPreferences(id);
  const state = workRouteState(window.location.search, preferences);
  app.setTitle("Work");
  mount(app.querySelector(".content"), "flow-work-items", {
    ...data,
    ...state,
    preferences,
    projectID: id,
    projects: app.projects || [],
    search: window.location.search,
    currentHref: currentWorkReturnTarget(window.location, id),
    loadOutline: () => apiGet(workItemsAPIBase(id)),
  });
  return true;
}
