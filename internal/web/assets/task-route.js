// The task detail route: fetch task, workflow and change threads, project them
// into one model, hand to <flow-task-detail>.

import { apiGet, taskAPIBase, workItemContextAPIPath } from "./api.js";
import { failureMessage } from "./actions.js";
import { mount } from "./elements/base.js";
import { taskModel } from "./task-model.js";
import { value } from "./normalize.js";
import { workItemFeatureID, workItemID } from "./work-item-model.js";
import { workNavigationState } from "./work-nav.js";
import { loadWorkItemContext } from "./work-item-detail.js";
import "./elements/task-detail.js";

export async function renderTaskRoute(app, id, context, projectID = "") {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;
  const resolvedProject = value(data, "project_id", "ProjectID") || projectID;

  const detail = value(data, "task_detail", "TaskDetail") || {};
  const change = value(detail, "ready_change", "ReadyChange") || (value(detail, "changes", "Changes") || [])[0];
  const changeID = value(change || {}, "id", "ID");

  // The Now card needs to know whether an open review thread blocks the merge,
  // which lives with the change rather than with the task. The findings tab
  // reads the per-task findings registry, fetched alongside so the tab paints
  // from the model instead of loading on open.
  const [workflowData, threadData, findingsData, planning] = await Promise.all([
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/workflow`).catch(() => null),
    changeID ? apiGet(`/v2/changes/${encodeURIComponent(changeID)}/threads`).catch(() => null) : Promise.resolve(null),
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/findings`).catch((error) => ({ error: failureMessage(error) })),
    loadWorkItemContext(app, resolvedProject, id),
  ]);
  if (context && !app.isActiveLoad(context)) return false;

  const projectName = value(data, "project_name", "ProjectName");
  app.setTitle(projectName ? `Task · ${projectName}` : "Task");

  const navigation = workNavigationState(window.location.search, resolvedProject);
  const planningItem = value(planning.hierarchy || {}, "item", "Item") || {};
  const knownItems = [...(planning.workItems || []), planningItem];
  const knownIDs = new Set(knownItems.map(workItemID));
  const contextIDs = new Set([navigation.context, workItemFeatureID(planningItem)]);
  const extraContexts = await Promise.all([...contextIDs]
    .filter((contextID) => contextID && !knownIDs.has(contextID))
    .map((contextID) => apiGet(workItemContextAPIPath(resolvedProject, contextID)).catch(() => null)));
  if (context && !app.isActiveLoad(context)) return false;
  const extraItems = extraContexts.flatMap((hierarchy) => hierarchy ? [
    ...(value(hierarchy, "ancestors", "Ancestors") || []),
    value(hierarchy, "item", "Item"),
  ].filter(Boolean) : []);

  const model = taskModel(
    {
      ...data,
      threads: value(threadData || {}, "threads", "Threads") || [],
      findings: findingsData,
      work_item: planning.hierarchy,
      work_items: [...knownItems, ...extraItems],
      navigation,
    },
    workflowData,
  );
  mount(app.querySelector(".content"), "flow-task-detail", model);
  return true;
}
