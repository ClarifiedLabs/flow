// The features routes: list and detail. Features are project-scoped; with no
// project in the path the routes use the same default project as the new-task
// form (first selected, then first registered).

import { apiGet, featuresAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { defaultCreateProject } from "./task-view.js";
import "./elements/features.js";
import "./elements/feature.js";

export async function renderFeaturesRoute(app, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) {
    app.setTitle("Features");
    const content = app.querySelector(".content");
    if (content) content.innerHTML = `<section class="detail"><p>No project registered yet.</p></section>`;
    return true;
  }
  const data = await apiGet(`${featuresAPIBase(id)}?status=all`);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Features");
  const project = (app.projects || []).find((candidate) => (candidate.id || candidate.ID) === id);
  mount(app.querySelector(".content"), "flow-features", {
    ...data,
    projectID: id,
    projectName: (app.projects || []).length > 1 ? (project?.name || project?.Name || "") : "",
  });
  return true;
}

export async function renderFeatureRoute(app, ref, context, projectID = "") {
  const id = defaultCreateProject(app, projectID);
  if (!id) return renderFeaturesRoute(app, context, projectID);
  const data = await apiGet(`${featuresAPIBase(id)}/${encodeURIComponent(ref)}`);
  if (context && !app.isActiveLoad(context)) return false;
  const feature = data.feature || data.Feature || {};
  app.setTitle(feature.title || feature.Title || "Feature");
  mount(app.querySelector(".content"), "flow-feature", { ...data, projectID: id });
  return true;
}
