// Flows route: resolve the project, fetch the agent-def catalogs and the
// flows, keep the task form's flow cache warm, and mount <flow-flows>. The
// element owns the edit-in-place state and every control from there.

import { agentDefsAPIBase, apiGet, flowsAPIBase, globalAgentDefsAPIBase } from "./api.js";
import { DEFAULT_AGENT_HARNESSES } from "./config.js";
import { mount } from "./elements/base.js";
import { resolveFlowsProjectView } from "./elements/flows-view.js";
// Side-effect import: registers <flow-flows>. Without it mount() creates an
// unresolved element and the view paints nothing.
import "./elements/flows.js";

export async function renderFlowsRoute(app, context) {
  app.setTitle("Flows");
  await app.ensureProjects();
  // Worker capabilities are dynamic. Re-fetch them on navigation/manual refresh
  // so a catalog cached during worker startup does not persist for the lifetime
  // of the page.
  await app.ensureHarnesses({ refresh: true });
  if (context && !app.isActiveLoad(context)) return false;
  const params = new URLSearchParams(window.location.search);
  const project = resolveFlowsProjectView(app, params.get("project") || "");
  if (!project) {
    if (context && !app.isActiveLoad(context)) return false;
    mount(app.querySelector(".content"), "flow-flows", { chooser: true, projects: app.projects || [] });
    return true;
  }

  const [globalDefsData, defsData, flowsData] = await Promise.all([
    apiGet(globalAgentDefsAPIBase()),
    apiGet(agentDefsAPIBase(project.id)),
    apiGet(flowsAPIBase(project.id)),
  ]);
  if (context && !app.isActiveLoad(context)) return false;
  const globalAgentDefs = globalDefsData.agent_defs || globalDefsData.AgentDefs || [];
  const agentDefs = defsData.agent_defs || defsData.AgentDefs || [];
  const flows = flowsData.flows || flowsData.Flows || [];
  const defaultFlowID = flowsData.default_flow_id || flowsData.DefaultFlowID || "";
  // Keep this project's flow cache warm so the task form renders its Flow
  // selector without an extra round trip.
  app.caches?.seed?.("flows", project.id, { flows, defaultFlowID });

  mount(app.querySelector(".content"), "flow-flows", {
    project,
    projects: app.projects || [],
    globalAgentDefs,
    agentDefs,
    flows,
    defaultFlowID,
    agentOptions: (app.harnesses && app.harnesses.agents) || DEFAULT_AGENT_HARNESSES,
  });
  return true;
}
