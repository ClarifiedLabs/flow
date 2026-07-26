// The task detail route: fetch task, workflow and change threads, project them
// into one model, hand to <flow-task-detail>.

import { apiGet, taskAPIBase } from "./api.js";
import { mount } from "./elements/base.js";
import { taskModel } from "./task-model.js";
import { value } from "./normalize.js";
import "./elements/task-detail.js";

export async function renderTaskRoute(app, id, context, projectID = "") {
  const data = await apiGet(`${taskAPIBase(projectID)}/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;
  const resolvedProject = value(data, "project_id", "ProjectID") || projectID;

  const detail = value(data, "task_detail", "TaskDetail") || {};
  const change = value(detail, "ready_change", "ReadyChange") || (value(detail, "changes", "Changes") || [])[0];
  const changeID = value(change || {}, "id", "ID");

  // The Now card needs to know whether an open review thread blocks the merge,
  // which lives with the change rather than with the task.
  const [workflowData, threadData] = await Promise.all([
    apiGet(`${taskAPIBase(resolvedProject)}/${encodeURIComponent(id)}/workflow`).catch(() => null),
    changeID ? apiGet(`/v2/changes/${encodeURIComponent(changeID)}/threads`).catch(() => null) : Promise.resolve(null),
  ]);
  if (context && !app.isActiveLoad(context)) return false;

  const projectName = value(data, "project_name", "ProjectName");
  app.setTitle(projectName ? `Task · ${projectName}` : "Task");

  const model = taskModel(
    { ...data, threads: value(threadData || {}, "threads", "Threads") || [] },
    workflowData,
  );
  mount(app.querySelector(".content"), "flow-task-detail", model);
  return true;
}
