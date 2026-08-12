// Diagnostics routes: fetch the payload and mount <flow-diagnostics>. The
// jobs view's filter/sort selections live on the element.

import { apiGet } from "./api.js";
import { mount } from "./elements/base.js";
import "./elements/diagnostics.js";

export async function renderWorkersRoute(app, context) {
  const data = await apiGet("/v2/workers");
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Workers");
  mount(app.querySelector(".content"), "flow-diagnostics", { kind: "workers", ...data });
  return true;
}

export async function renderJobsRoute(app, context) {
  const data = await apiGet("/v2/jobs");
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Jobs");
  mount(app.querySelector(".content"), "flow-diagnostics", { kind: "jobs", ...data });
  return true;
}
