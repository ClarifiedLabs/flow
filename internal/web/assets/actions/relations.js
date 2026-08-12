// Relation handlers: thread claims and relation-row removal.

import { apiDelete, apiPost, taskAPIBase, workItemAPIPath } from "../api.js";
import { CANCELLED } from "./dispatch.js";

export const relationActions = {
  async threadClaim(app, element, dataset) {
    const body = dataset.claimKind === "fixed" ? "" : (window.prompt("Why?") || "").trim();
    if (dataset.claimKind !== "fixed" && !body) return CANCELLED;
    await apiPost(`/v2/threads/${encodeURIComponent(dataset.threadClaim)}/claims`, {
      kind: dataset.claimKind,
      body,
    });
    await app.refresh();
    return "Thread claimed";
  },

  // relationRemove unlinks one stored relation row. The button carries the row's
  // source task (the path) and the target/kind (the body), so it removes the
  // exact relation regardless of which side the viewed task sits on.
  async workItemRelationRemove(app, element, dataset) {
    await apiDelete(workItemAPIPath(dataset.project, dataset.workItemRelationRemove, "/relations"), {
      source_item_id: dataset.source,
      target_item_id: dataset.target,
      kind: dataset.kind,
    });
    if (dataset.kind === "parent_of") app.workItemsByProject?.delete(dataset.project);
    await app.refresh();
    return "Relation removed";
  },

  async relationRemove(app, element, dataset) {
    await apiDelete(`${taskAPIBase(dataset.project)}/${encodeURIComponent(dataset.relationRemove)}/relations`, {
      target_task_id: dataset.target,
      kind: dataset.kind,
    });
    await app.refresh();
    return "Relation removed";
  },
};
