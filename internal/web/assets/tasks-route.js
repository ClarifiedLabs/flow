// Tasks route: mount <flow-tasks>, sync its URL/storage-seeded state, fetch
// the aggregate read, and hand the payload over. The element owns the view
// state (chips, project/root/search filters, selection) from there; the view
// intentionally does not poll.

import { apiGet, workItemsAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { tasksQueryView } from "./elements/tasks.js";
import { value } from "./normalize.js";
import { writeWorkProject } from "./storage.js";
import { buildWorkItemIndex } from "./work-item-model.js";
import { activeWorkProject } from "./work-nav.js";

export async function renderTasksRoute(app, context) {
  // Mount first: the element seeds its filter state from the URL and storage
  // (or keeps it, when mount() reuses it across a same-route reload), and the
  // fetch below is built from that state.
  const element = mount(app.querySelector(".content"), "flow-tasks");
  element.syncLocation();

  const projectIDs = element.workProjectIDs();
  const tasksRequest = element.tasksState.size > 0
    ? apiGet("/v2/tasks" + tasksQueryView(element, element.tasksState, { q: element.tasksQuery }))
    : Promise.resolve(null);
  // The root scope is intentionally client-side: /v2/tasks remains the one
  // aggregate state/search/project query, while the fetched work-item
  // summaries classify each result under its top-level container.
  const [data, workPayloads] = await Promise.all([
    tasksRequest,
    Promise.all(projectIDs.map(async (projectID) => [projectID, await apiGet(workItemsAPIBase(projectID))])),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Tasks");
  const workProject = activeWorkProject(app, element.tasksProject);
  if (workProject) writeWorkProject(workProject);
  // The bulk flow dropdown reads the per-project flow cache; warm it now so a
  // selection renders its options synchronously. Flows are project-owned, so
  // there is nothing to warm while the filter is on all projects.
  if (String(element.tasksProject || "").trim()) await app.ensureFlows(element.tasksProject);
  if (context && !app.isActiveLoad(context)) return false;
  element.data = {
    tasks: value(data, "tasks", "Tasks") || [],
    workIndexes: new Map(workPayloads.map(([projectID, payload]) => [projectID, buildWorkItemIndex(payload)])),
    projectBadge: (app.projects || []).length > 1,
  };
  return true;
}
