// <flow-flows>: the Flows settings page — global/project agent definitions
// and the project's flows. Agent definitions pick a harness + model/effort +
// prompt; flows are editable graphs of trusted node kinds with explicit
// outcome transitions. The route (flows-route.js) fetches and mounts; the
// element owns the edit-in-place state (was app.flowsView) and every control
// through delegated listeners, so the in-progress form survives repaints.
//
// The pure render/payload functions keep their signatures; the element
// provides the "state" argument's fields (editingDefID, editingGlobalDefID,
// editingFlowID, cloneInFlight) as its own.

import { agentDefsAPIBase, apiDelete, apiGet, apiPatch, apiPost, flowsAPIBase, globalAgentDefsAPIBase } from "../api.js";
import { define, FlowElement } from "./base.js";
import { findHarnessModel, harnessModels } from "../models/harness-catalog.js";
import { value } from "../normalize.js";
import { failureMessage } from "../actions/dispatch.js";

import { renderFlowsChooserMarkup, renderFlowsProjectSwitchView, renderGlobalAgentDefsSectionView, renderAgentDefsSectionView, renderFlowsSectionView, NEW_AGENT_DEF_STATE, agentDefPayloadFromFormView, renderAgentDefModelSelectView, renderAgentDefReasoningSelectView, renderReasoningOptionsView, flowsProjectOptionsView } from "./flows-view.js";
import { renderNodeCardView, renderEdgeRowView, renderReviewAgentRowView, renderNodeConfigEditorView, flowPayloadFromEditorView, refreshGraphSelectorsView, graphNodeKeysView, moveRowView, cloneFlowView, reviewNodeConfigKeyView } from "./flow-editor-view.js";

export class FlowFlows extends FlowElement {
  // The edit-in-place state (was app.flowsView): which agent-def row and flow
  // are open in their inline editors, and the clone single-flight guard.
  editingDefID = "";
  editingGlobalDefID = "";
  editingFlowID = "";
  cloneInFlight = false;

  // The app services the handlers need, delegated to the enclosing <flow-app>.
  get projects() {
    return this.data?.projects || [];
  }

  selectedProjectIDs() {
    return this.app?.selectedProjectIDs?.() || [];
  }

  setStatus(message) {
    this.app?.setStatus?.(message);
  }

  async load() {
    return this.app?.load?.();
  }

  // data: { chooser, projects } or { project, projects, globalAgentDefs,
  // agentDefs, flows, defaultFlowID, agentOptions }.
  render() {
    const payload = this.data;
    if (!payload) return "";
    if (payload.chooser) return renderFlowsChooserMarkup(flowsProjectOptionsView(this));
    return `
      <section class="detail flows-detail">
        <div class="detail-head">
          <div>
            ${renderFlowsProjectSwitchView(this, payload.project)}
            <h2>Flows</h2>
          </div>
        </div>
        ${renderGlobalAgentDefsSectionView(payload.globalAgentDefs, payload.agentOptions, this)}
        ${renderAgentDefsSectionView(payload.agentDefs, payload.agentOptions, this)}
        ${renderFlowsSectionView(payload.flows, payload.agentDefs, payload.defaultFlowID, this)}
      </section>
    `;
  }

  bind() {
    // Click delegation is the base class's (it calls handleClick); bind adds
    // the other event types.
    this.addEventListener("change", (event) => this.handleChange(event));
    this.addEventListener("input", (event) => {
      const form = event.target?.closest?.("[data-flow-editor]");
      if (form) refreshGraphSelectorsView(form);
    });
    this.addEventListener("submit", (event) => this.handleSubmit(event));
  }

  // The agent-def catalog's editing key and API base come from whichever
  // section the control sits in (project vs global).
  catalogContext(target) {
    if (target.closest?.("[data-global-agent-defs-section]")) {
      return { editingKey: "editingGlobalDefID", apiBase: globalAgentDefsAPIBase() };
    }
    if (target.closest?.("[data-agent-defs-section]")) {
      return { editingKey: "editingDefID", apiBase: agentDefsAPIBase(this.data.project.id) };
    }
    return null;
  }

  async handleClick(event) {
    const target = event.target;
    if (!target || typeof target.closest !== "function") return;

    // --- agent-def catalog controls -----------------------------------------
    const catalog = this.catalogContext(target);
    if (catalog) {
      const editButton = target.closest("[data-edit-def]");
      if (editButton) {
        this[catalog.editingKey] = editButton.dataset.editDef || "";
        await this.load();
        return;
      }
      if (target.closest("[data-add-def]")) {
        this[catalog.editingKey] = NEW_AGENT_DEF_STATE;
        await this.load();
        return;
      }
      const deleteButton = target.closest("[data-delete-def]");
      if (deleteButton) {
        if (typeof window.confirm === "function" && !window.confirm("Delete this agent definition?")) return;
        try {
          await apiDelete(`${catalog.apiBase}/${encodeURIComponent(deleteButton.dataset.deleteDef)}`);
          this[catalog.editingKey] = "";
          await this.load();
        } catch (error) {
          this.setStatus(failureMessage(error));
        }
        return;
      }
      if (target.closest("[data-def-cancel]")) {
        this[catalog.editingKey] = "";
        await this.load();
        return;
      }
    }

    // --- flow table controls -------------------------------------------------
    if (target.closest("[data-flows-section]")) {
      const editFlow = target.closest("[data-edit-flow]");
      if (editFlow) {
        this.editingFlowID = editFlow.dataset.editFlow || "";
        await this.load();
        return;
      }
      const cloneFlow = target.closest("[data-clone-flow]");
      if (cloneFlow) {
        await this.cloneFlow(cloneFlow);
        return;
      }
      const defaultFlow = target.closest("[data-default-flow]");
      if (defaultFlow) {
        try {
          await apiPost(`${flowsAPIBase(this.data.project.id)}/${encodeURIComponent(defaultFlow.dataset.defaultFlow)}/default`, {});
          await this.load();
          this.setStatus("default flow updated");
        } catch (error) {
          this.setStatus(failureMessage(error));
        }
        return;
      }
      const deleteFlow = target.closest("[data-delete-flow]");
      if (deleteFlow) {
        if (typeof window.confirm === "function" && !window.confirm("Delete this flow?")) return;
        try {
          await apiDelete(`${flowsAPIBase(this.data.project.id)}/${encodeURIComponent(deleteFlow.dataset.deleteFlow)}`);
          this.editingFlowID = "";
          await this.load();
        } catch (error) {
          this.setStatus(failureMessage(error));
        }
        return;
      }
    }

    // --- flow editor row controls (DOM surgery, no repaint) ------------------
    const form = target.closest("[data-flow-editor]");
    if (!form) return;
    const agentDefs = this.data.agentDefs;
    event.preventDefault();
    if (target.closest("[data-add-node]")) {
      form.querySelector("[data-node-cards]")?.insertAdjacentHTML("beforeend", renderNodeCardView({}, agentDefs));
      refreshGraphSelectorsView(form);
    } else if (target.closest("[data-add-review-agent]")) {
      const config = target.closest("[data-review-agent-config]");
      config?.querySelector("[data-review-agent-rows]")?.insertAdjacentHTML("beforeend", renderReviewAgentRowView({}, agentDefs, config.dataset?.reviewConfigKey));
    } else if (target.closest("[data-add-edge]")) {
      form.querySelector("[data-edge-rows]")?.insertAdjacentHTML("beforeend", renderEdgeRowView({}, graphNodeKeysView(form)));
    } else if (target.closest("[data-node-remove]")) {
      target.closest("[data-node-card]")?.remove();
      refreshGraphSelectorsView(form);
    } else if (target.closest("[data-edge-remove]")) {
      target.closest("[data-edge-row]")?.remove();
      refreshGraphSelectorsView(form);
    } else if (target.closest("[data-review-agent-remove]")) {
      target.closest("[data-review-agent-row]")?.remove();
    } else if (target.closest("[data-review-agent-up]")) {
      moveRowView(target.closest("[data-review-agent-row]"), -1);
    } else if (target.closest("[data-review-agent-down]")) {
      moveRowView(target.closest("[data-review-agent-row]"), 1);
    } else if (target.closest("[data-node-up]")) {
      moveRowView(target.closest("[data-node-card]"), -1);
      refreshGraphSelectorsView(form);
    } else if (target.closest("[data-node-down]")) {
      moveRowView(target.closest("[data-node-card]"), 1);
      refreshGraphSelectorsView(form);
    } else if (target.closest("[data-flow-cancel]")) {
      this.editingFlowID = "";
      await this.load();
    }
  }

  // cloneFlow posts a copied create payload and opens the copy in the editor.
  // The single-flight guard lives on the element (the old app.flowsView
  // guard): rapid clicks cannot race two copies onto the same "<name> (copy)"
  // suffix, and the render disables clone buttons while a clone is pending.
  async cloneFlow(button) {
    if (this.cloneInFlight) return;
    const project = this.data.project;
    const flows = this.data.flows;
    const source = (flows || []).find((flow) => value(flow, "id", "ID") === button.dataset.cloneFlow);
    if (!source) return;
    this.cloneInFlight = true;
    this.invalidate();
    try {
      // Re-read the active project's flows at click time so the copy name
      // reflects flows created since this payload arrived. If the re-read
      // fails, fall back to the payload list: the server still guards
      // uniqueness and rejects a collision with a surfaced error and no
      // partial mutation.
      let existingNames = (flows || []).map((flow) => value(flow, "name", "Name"));
      try {
        const data = await apiGet(flowsAPIBase(project.id));
        existingNames = (data.flows || data.Flows || []).map((flow) => value(flow, "name", "Name"));
      } catch {
        // Keep the payload's names.
      }
      const response = await apiPost(flowsAPIBase(project.id), cloneFlowView(source, existingNames));
      const created = value(response, "flow", "Flow") || response || {};
      this.editingFlowID = value(created, "id", "ID") || "";
      await this.load();
      this.setStatus("flow cloned; rename and edit your copy");
    } catch (error) {
      this.setStatus(failureMessage(error));
    } finally {
      this.cloneInFlight = false;
      this.invalidate();
    }
  }

  handleChange(event) {
    const target = event.target;
    if (!target || typeof target.closest !== "function") return;

    // The agent-def form's harness/model selects re-render the dependent
    // cells in place (the form's unsaved state is the DOM).
    const defForm = target.closest("[data-agent-def-form]");
    if (defForm) {
      const agentOptions = this.data.agentOptions;
      if (target.closest("[data-def-harness]")) {
        const harnessSelect = defForm.querySelector("[data-def-harness]");
        const modelCell = defForm.querySelector("[data-def-model-cell]");
        const reasoningCell = defForm.querySelector("[data-def-reasoning-cell]");
        if (modelCell && reasoningCell) {
          const models = harnessModels(agentOptions, harnessSelect ? harnessSelect.value : "");
          modelCell.innerHTML = renderAgentDefModelSelectView(models, null);
          reasoningCell.innerHTML = renderAgentDefReasoningSelectView(null, "");
        }
        return;
      }
      if (target.closest("[data-def-model]")) {
        const harnessSelect = defForm.querySelector("[data-def-harness]");
        const models = harnessModels(agentOptions, harnessSelect ? harnessSelect.value : "");
        const model = findHarnessModel(models, target.value);
        const reasoning = defForm.querySelector("[data-def-reasoning]");
        if (reasoning) reasoning.innerHTML = renderReasoningOptionsView(model, "");
        return;
      }
    }

    // The blocking checkbox toggles its advisory marker.
    if (target.getAttribute?.("name") === "review_agent_blocking") {
      const marker = target.closest("[data-review-agent-row]")?.querySelector("[data-review-agent-advisory]");
      if (marker) marker.hidden = Boolean(target.checked);
      return;
    }

    // A node-kind switch re-renders the node's config editor with the kind's
    // structured default.
    if (target.getAttribute?.("name") !== "node_kind") return;
    const card = target.closest("[data-node-card]");
    const editor = card?.querySelector("[data-node-config-editor]");
    if (!editor) return;
    const configKey = reviewNodeConfigKeyView(target.value);
    const reviewConfig = { agents: [{}] };
    if (configKey === "change_review") reviewConfig.aggregator_agent_def_id = "";
    const config = configKey ? { [configKey]: reviewConfig } : {};
    editor.innerHTML = renderNodeConfigEditorView({ kind: target.value, config }, this.data.agentDefs);
  }

  async handleSubmit(event) {
    const target = event.target;
    if (!target || typeof target.closest !== "function") return;

    const defForm = target.closest("[data-agent-def-form]");
    if (defForm) {
      event.preventDefault();
      const catalog = this.catalogContext(defForm);
      if (defForm.reportValidity && !defForm.reportValidity()) return;
      const payload = agentDefPayloadFromFormView(defForm, this.data.agentOptions);
      if (!payload.name) {
        this.setStatus("Agent definition name is required");
        return;
      }
      const defID = defForm.dataset.defId || "";
      try {
        if (defID) {
          await apiPatch(`${catalog.apiBase}/${encodeURIComponent(defID)}`, payload);
        } else {
          await apiPost(catalog.apiBase, payload);
        }
        this[catalog.editingKey] = "";
        await this.load();
        this.setStatus(defID ? "agent definition saved" : "agent definition created");
      } catch (error) {
        this.setStatus(failureMessage(error));
      }
      return;
    }

    const flowForm = target.closest("[data-flow-editor]");
    if (flowForm) {
      event.preventDefault();
      if (flowForm.reportValidity && !flowForm.reportValidity()) return;
      let payload;
      try {
        payload = flowPayloadFromEditorView(flowForm);
      } catch (error) {
        this.setStatus(`Invalid node configuration JSON: ${failureMessage(error)}`);
        return;
      }
      if (!payload.name) {
        this.setStatus("Flow name is required");
        return;
      }
      const flowID = flowForm.dataset.flowId || "";
      const base = flowsAPIBase(this.data.project.id);
      try {
        if (flowID) {
          await apiPatch(`${base}/${encodeURIComponent(flowID)}`, payload);
        } else {
          await apiPost(base, payload);
        }
        this.editingFlowID = "";
        await this.load();
        this.setStatus(flowID ? "flow saved" : "flow created");
      } catch (error) {
        this.setStatus(failureMessage(error));
      }
    }
  }
}

define("flow-flows", FlowFlows);
