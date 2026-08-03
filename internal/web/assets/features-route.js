// The features routes: list and detail. Features are project-scoped; with no
// project in the path the routes use the same default project as the new-task
// form (first selected, then first registered).

import { apiGet, featuresAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { loadWorkItemContext } from "./work-item-detail.js";
import { defaultCreateProject } from "./task-view.js";
import { writeWorkProject } from "./storage.js";
import { activeWorkProject, currentWorkReturnTarget, resolveWorkNavigation, workNavigationState } from "./work-nav.js";
import { workItemID } from "./work-item-model.js";
import "./elements/features.js";
import "./elements/feature.js";

export async function renderFeaturesRoute(app, context, projectID = "") {
  const params = new URLSearchParams(window.location.search);
  const id = activeWorkProject(app, projectID || params.get("project") || "");
  if (!id) {
    app.setTitle("Branches");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  writeWorkProject(id);
  const [data, workItems] = await Promise.all([
    apiGet(`${featuresAPIBase(id)}?status=all`),
    typeof app.ensureWorkItems === "function" ? app.ensureWorkItems(id) : [],
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Branches");
  mount(app.querySelector(".content"), "flow-features", {
    ...data,
    workItems,
    projectID: id,
    projects: app.projects || [],
    search: window.location.search,
  });
  return true;
}

export async function renderFeatureRoute(app, ref, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) return renderFeaturesRoute(app, context, projectID);
  const [data, planning] = await Promise.all([
    apiGet(`${featuresAPIBase(id)}/${encodeURIComponent(ref)}`),
    loadWorkItemContext(app, id, ref, { legacyTree: true }),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  const feature = data.feature || data.Feature || {};
  const hierarchyItem = planning.hierarchy?.item || planning.hierarchy?.Item || {};
  const ancestors = planning.hierarchy?.ancestors || planning.hierarchy?.Ancestors || [];
  const navigation = resolveWorkNavigation(workNavigationState(window.location.search, id), [workItemID(hierarchyItem), ...ancestors.map(workItemID)], id);
  app.setTitle(feature.title || feature.Title || "Feature");
  mount(app.querySelector(".content"), "flow-feature", { ...data, hierarchy: planning.hierarchy, workItems: planning.workItems, projectID: id, navigation, currentHref: currentWorkReturnTarget(window.location, id) });
  return true;
}
