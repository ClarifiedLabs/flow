// The task detail route: fetch task, workflow and change threads, project them
// into one model, hand to <flow-task-detail>.

import { apiGet, taskAPIBase, workItemContextAPIPath } from "./api.js";
import { failureMessage } from "./actions/dispatch.js";
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

  // One task-scoped threads fetch feeds two projections: the Change tab and
  // Now card read the current change's subset, and the Threads tab reads the
  // full cross-change record. The findings tab reads the per-task findings
  // registry, fetched alongside so the tab paints from the model instead of
  // loading on open.
  const [workflowData, threadData, findingsData, planning] = await Promise.all([
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/workflow`).catch(() => null),
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/threads`).catch(() => null),
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/findings`).catch((error) => ({ error: failureMessage(error) })),
    loadWorkItemContext(app, resolvedProject, id),
    typeof app.ensureFlows === "function"
      ? Promise.resolve().then(() => app.ensureFlows(resolvedProject)).catch(() => null)
      : null,
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

  // The Change tab's inline diff and the Now card keep seeing exactly the
  // current change's threads, filtered client-side out of the task-wide list,
  // so an old-change thread anchored on the same file cannot leak into the
  // current diff. task_threads is the unfiltered record the Threads tab reads.
  const taskThreads = value(threadData || {}, "threads", "Threads") || [];
  const model = taskModel(
    {
      ...data,
      threads: taskThreads.filter((thread) => String(value(thread, "change_id", "ChangeID") || "") === String(changeID || "")),
      task_threads: taskThreads,
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
