// The change route. Fetches the change, its threads and its diff together so
// the review page arrives whole rather than filling in underneath the reader.

import { apiGet } from "./api.js";
import { mount } from "./elements/base.js";
import { value } from "./normalize.js";
import "./elements/change.js";

export async function renderChangeRoute(app, id, context) {
  const data = await apiGet(`/v2/changes/${encodeURIComponent(id)}`);
  if (context && !app.isActiveLoad(context)) return false;

  const change = value(data, "change", "Change") || {};
  const headSHA = value(change, "head_sha", "HeadSHA");
  const diff = headSHA ? await apiGet(`/v2/changes/${encodeURIComponent(id)}/diff`).catch(() => null) : null;
  if (context && !app.isActiveLoad(context)) return false;

  app.setTitle("Change");
  mount(app.querySelector(".content"), "flow-change", { ...data, diff: diff || {} });
  return true;
}
