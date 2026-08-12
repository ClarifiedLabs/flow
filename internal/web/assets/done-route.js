// Done route: fetch the first /v2/done page and mount <flow-done>. The
// element owns the accumulated pages, the outcome filter and the density
// toggle from there.

import { apiGet } from "./api.js";
import { mount } from "./elements/base.js";
import { doneQuery } from "./elements/done.js";
import { readDoneOutcome } from "./storage.js";

export async function renderDoneRoute(app, context) {
  const data = await apiGet("/v2/done" + doneQuery(app.selectedProjectIDs(), readDoneOutcome()));
  if (context && !app.isActiveLoad(context)) return false;
  app.setTitle("Done");
  mount(app.querySelector(".content"), "flow-done", {
    page: data,
    projectBadge: (app.projects || []).length > 1,
  });
  return true;
}
