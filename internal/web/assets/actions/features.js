// Feature and epic handlers. Feature actions mutate the shared feature
// branch: rebase moves it onto the integration target (a conflict creates a
// rebase task that blocks the feature's other tasks), land squash-merges it
// into that target, archive closes the feature without landing.

import { apiPost, epicsAPIBase, featuresAPIBase } from "../api.js";
import { CANCELLED } from "./dispatch.js";

export const featureActions = {
  async featureRebase(app, element, dataset) {
    const result = await apiPost(
      `${featuresAPIBase(dataset.project)}/${encodeURIComponent(dataset.featureRebase)}/rebase`, {},
    );
    app.featuresByProject?.delete(dataset.project);
    await app.refresh();
    const kind = result?.result?.kind || "";
    if (kind === "rebase_task_created") {
      return `Rebase conflicted — created rebase task ${result.result.rebase_task_id}`;
    }
    if (kind === "rebased") return "Feature branch rebased";
    return "Feature branch already up to date";
  },

  async featureLand(app, element, dataset) {
    if (!window.confirm("Squash-merge the feature branch into its integration target and mark the feature landed?")) return CANCELLED;
    await apiPost(
      `${featuresAPIBase(dataset.project)}/${encodeURIComponent(dataset.featureLand)}/land`, {},
    );
    app.featuresByProject?.delete(dataset.project);
    await app.refresh();
    return "Feature landed";
  },

  async featureArchive(app, element, dataset) {
    if (!window.confirm("Archive this feature? Its branch is kept for audit.")) return CANCELLED;
    await apiPost(
      `${featuresAPIBase(dataset.project)}/${encodeURIComponent(dataset.featureArchive)}/archive`, {},
    );
    app.featuresByProject?.delete(dataset.project);
    await app.refresh();
    return "Feature archived";
  },

  async featureStart(app, element, dataset) {
    await apiPost(`${featuresAPIBase(dataset.project)}/${encodeURIComponent(dataset.featureStart)}/start`, {});
    await app.refresh();
    return "Feature tasks scheduled";
  },

  async epicStart(app, element, dataset) {
    await apiPost(`${epicsAPIBase(dataset.project)}/${encodeURIComponent(dataset.epicStart)}/start`, {});
    await app.refresh();
    return "Epic tasks scheduled";
  },

  async epicComplete(app, element, dataset) {
    await apiPost(`${epicsAPIBase(dataset.project)}/${encodeURIComponent(dataset.epicComplete)}/complete`, {});
    await app.refresh();
    return "Epic completed";
  },

  async epicReopen(app, element, dataset) {
    await apiPost(`${epicsAPIBase(dataset.project)}/${encodeURIComponent(dataset.epicReopen)}/reopen`, {});
    await app.refresh();
    return "Epic reopened";
  },

  async epicArchive(app, element, dataset) {
    if (!window.confirm("Archive this epic?")) return CANCELLED;
    await apiPost(`${epicsAPIBase(dataset.project)}/${encodeURIComponent(dataset.epicArchive)}/archive`, {});
    await app.refresh();
    return "Epic archived";
  },
};
