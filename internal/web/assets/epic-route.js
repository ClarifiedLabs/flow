// First-class, project-scoped epic route.

import { apiGet, epicsAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { loadWorkItemContext } from "./work-item-detail.js";
import { defaultCreateProject } from "./task-view.js";
import { workItemID } from "./work-item-model.js";
import { currentWorkReturnTarget, resolveWorkNavigation, workNavigationState } from "./work-nav.js";
import "./elements/epic.js";

export async function renderEpicRoute(app, epicID, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) {
    app.setTitle("Epic");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  const [data, planning] = await Promise.all([
    apiGet(`${epicsAPIBase(id)}/${encodeURIComponent(epicID)}`),
    loadWorkItemContext(app, id, epicID, { legacyTree: true }),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  const hierarchyItem = planning.hierarchy?.item || planning.hierarchy?.Item || {};
  const ancestors = planning.hierarchy?.ancestors || planning.hierarchy?.Ancestors || [];
  const navigation = resolveWorkNavigation(workNavigationState(window.location.search, id), [workItemID(hierarchyItem), ...ancestors.map(workItemID)], id);
  app.setTitle("Epic");
  mount(app.querySelector(".content"), "flow-epic", { ...data, hierarchy: planning.hierarchy, workItems: planning.workItems, projectID: id, navigation, currentHref: currentWorkReturnTarget(window.location, id) });
  return true;
}
