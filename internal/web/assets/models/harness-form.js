// Harness selection form UI and agent-arg (de)serialization for launches:
// the model/reasoning fieldset and controls, the selection flag vocabulary,
// and the shell-arg renderers. Catalog queries live in harness-catalog.js.

import { escapeAttr, escapeHTML } from "../html.js";
import {
  HARNESS_REASONING_UNAVAILABLE,
  findHarnessModel,
  findHarnessModelByParts,
  harnessModelLabel,
  harnessModels,
  harnessReasoningLevelValues,
  normalizeArgList,
  normalizeHarnessModelList,
  normalizeHarnessOptions,
  splitOnce,
  splitQualifiedModel,
  uniqueHarnessProviders,
} from "./harness-catalog.js";

// renderHarnessModelFields renders the model/reasoning fieldset for the active
// harness. It embeds every harness's model catalog and parsed selection so
// bindHarnessModelControls can re-render the inner controls when the agent
// harness changes without another round trip.
export function renderHarnessModelFields(options, selectionByHarness, agentHarness) {
  const catalog = {};
  for (const name of ["harness"]) {
    const models = harnessModels(options, name);
    if (models.length) catalog[name] = models;
  }
  if (!Object.keys(catalog).length) return "";
  const models = catalog[agentHarness] || [];
  const selection = (selectionByHarness && selectionByHarness[agentHarness]) || null;
  const hidden = models.length ? "" : " hidden";
  return `
    <fieldset class="harness-model-fields wide" data-harness-model-fields data-harness-model-catalog="${escapeAttr(JSON.stringify(catalog))}" data-harness-model-selections="${escapeAttr(JSON.stringify(selectionByHarness || {}))}"${hidden}>
      <div data-harness-model-controls>
        ${renderHarnessModelControls(models, selection)}
      </div>
    </fieldset>
  `;
}

// renderHarnessModelControls renders the model/reasoning controls for a single
// harness's model list. Re-rendered per harness by bindHarnessModelControls.
export function renderHarnessModelControls(models, selection) {
  if (!models.length) return "";
  const selected = selection || parseHarnessSelectionArgs([], models);
  const selectedModel = selected.qualified_id ? findHarnessModel(models, selected.qualified_id) : null;
  const selectedModelID = selectedModel ? selectedModel.qualified_id : "";
  return `
    <label>
      <span>Model</span>
      <select name="harness_model">
        ${renderHarnessModelOptions(models, selectedModelID)}
      </select>
    </label>
    <div class="harness-reasoning" data-harness-reasoning-controls>
      ${renderHarnessReasoningControls(selectedModelID ? selectedModel : null, selected)}
    </div>
  `;
}

export function renderHarnessModelOptions(models, selectedQualifiedID) {
  const selectedID = String(selectedQualifiedID || "").trim();
  const visibleModels = normalizeHarnessModelList(models);
  const selectedVisible = visibleModels.some((model) => model.qualified_id === selectedID);
  const includeProvider = uniqueHarnessProviders(visibleModels).length > 1;
  return `
        <option value="" ${selectedVisible ? "" : "selected"}>Default model</option>
        ${visibleModels.map((model) => `<option value="${escapeAttr(model.qualified_id)}" ${model.qualified_id === selectedID ? "selected" : ""}>${escapeHTML(harnessModelLabel(model, includeProvider))}</option>`).join("")}`;
}
export function renderHarnessReasoningControls(model, selection = {}) {
  const values = harnessReasoningLevelValues(model);
  const mode = String(selection.reasoning_mode || "default").trim() || "default";
  const selected = String(selection.reasoning_effort || "").trim();
  const unrepresentableLegacyMode = mode !== "default" && mode !== "effort";
  const selectedValue = unrepresentableLegacyMode
    ? HARNESS_REASONING_UNAVAILABLE
    : (mode === "default" ? "" : (values.includes(selected) ? selected : HARNESS_REASONING_UNAVAILABLE));
  const unavailableOption = `<option value="${HARNESS_REASONING_UNAVAILABLE}" selected>${HARNESS_REASONING_UNAVAILABLE}</option>`;
  return `
    <label>
      <span>Reasoning Level</span>
      <select name="harness_reasoning_effort">
        ${values.length
          ? `${selectedValue === HARNESS_REASONING_UNAVAILABLE ? unavailableOption : ""}<option value="" ${selectedValue === "" ? "selected" : ""}>Default</option>${values.map((value) => `<option value="${escapeAttr(value)}" ${value === selectedValue ? "selected" : ""}>${escapeHTML(value)}</option>`).join("")}`
          : unavailableOption}
      </select>
    </label>
  `;
}

export function renderHarnessReasoningInto(fieldset, model, preserve) {
  if (!fieldset || typeof fieldset.querySelector !== "function") return;
  const container = fieldset.querySelector("[data-harness-reasoning-controls]");
  if (!container) return;
  const current = preserve ? readHarnessReasoningSelection(fieldset) : {};
  container.innerHTML = renderHarnessReasoningControls(model, current);
}

export function readHarnessReasoningSelection(root) {
  if (!root || typeof root.querySelector !== "function") return {};
  const effort = String(root.querySelector('[name="harness_reasoning_effort"]')?.value || "").trim();
  return {
    reasoning_mode: effort === HARNESS_REASONING_UNAVAILABLE
      ? HARNESS_REASONING_UNAVAILABLE
      : (effort ? "effort" : "default"),
    reasoning_effort: effort === HARNESS_REASONING_UNAVAILABLE ? "" : effort,
    reasoning_budget_tokens: null,
  };
}

export function syncHarnessReasoningVisibility(_root) {
  // Legacy no-op retained for callers/tests that import the old helper. The UI now
  // exposes one Reasoning Level selector instead of separate mode/effort/budget
  // controls, so there is no conditional visibility to sync.
}

// HARNESS_SELECTION_FLAGS maps the harness to the flag names it uses to carry
// a Flow-managed model/reasoning selection. The harness CLI treats
// provider:model as a target id, but we still parse old stored --provider args.
//   - harness: --model provider:model + --reasoning <profile>
export const HARNESS_SELECTION_FLAGS = {
  harness: new Set(["provider", "model", "reasoning", "reasoning-effort", "reasoning-enabled", "reasoning-budget-tokens"]),
};

export function harnessSelectionFlags(harness) {
  return HARNESS_SELECTION_FLAGS[harness] || HARNESS_SELECTION_FLAGS.harness;
}

// applyHarnessSelectionFlag records one recognized selection flag onto selection
// and returns true. It returns false when the token is not a selection flag, so
// the caller can keep it as an additional arg.
export function applyHarnessSelectionFlag(selection, harness, name, value) {
  if (!value) return false;
  if (name === "provider" && harness === "harness") {
    selection.provider = value;
    return true;
  }
  if (name === "model" || name === "m") {
    const split = splitQualifiedModel(value);
    if (split) {
      selection.provider = split.provider;
      selection.model = split.model;
    } else {
      selection.model = value;
    }
    return true;
  }
  if (harness === "harness") {
    if (name === "reasoning" || name === "reasoning-effort") {
      selection.reasoning_mode = "effort";
      selection.reasoning_effort = value;
      selection.reasoning_budget_tokens = null;
      return true;
    }
    if (name === "reasoning-enabled") {
      selection.reasoning_mode = "legacy";
      selection.reasoning_effort = "";
      selection.reasoning_budget_tokens = null;
      return true;
    }
    if (name === "reasoning-budget-tokens") {
      selection.reasoning_mode = "legacy";
      selection.reasoning_budget_tokens = null;
      selection.reasoning_effort = "";
      return true;
    }
    return false;
  }
  return false;
}

// serializeHarnessModelSelection renders the harness argv tokens for a model +
// reasoning choice. It is the inverse of parseHarnessSelectionArgs.
export function serializeHarnessModelSelection(_harness, model, reasoning) {
  const args = ["--model", model.target_id || model.qualified_id || model.model_id];
  const mode = reasoning.mode || "default";
  if (mode === "effort") {
    const effort = String(reasoning.effort || "").trim();
    if (!effort) throw new Error("Reasoning effort is required");
    args.push("--reasoning", effort);
  } else if (mode !== "default") {
    throw new Error("Unsupported reasoning option");
  }
  return args;
}

export function parseHarnessSelectionArgs(args, models = [], harness = "harness") {
  const flags = harnessSelectionFlags(harness);
  const selection = {
    provider: "",
    model: "",
    qualified_id: "",
    reasoning_mode: "default",
    reasoning_effort: "",
    reasoning_budget_tokens: null,
    additional_args: [],
  };
  const input = normalizeArgList(args);
  for (let i = 0; i < input.length; i += 1) {
    const parsed = readFlagValue(input, i, flags);
    const applied = parsed && parsed.value && applyHarnessSelectionFlag(selection, harness, parsed.name, parsed.value);
    if (!applied) {
      selection.additional_args.push(input[i]);
      if (parsed && parsed.consumedNext) selection.additional_args.push(input[i + 1]);
    }
    if (parsed && parsed.consumedNext) i += 1;
  }
  const model = findHarnessModelByParts(models, selection.provider, selection.model);
  if (model) {
    selection.provider = model.provider_id;
    selection.model = model.model_id;
    selection.qualified_id = model.qualified_id;
  }
  return selection;
}

export function isLegacyHarnessReasoningFlag(harness, name) {
  return harness === "harness" && (name === "reasoning-enabled" || name === "reasoning-budget-tokens");
}

export function stripHarnessSelectionArgs(args, harness = "harness") {
  const flags = harnessSelectionFlags(harness);
  const input = normalizeArgList(args);
  const stripped = [];
  for (let i = 0; i < input.length; i += 1) {
    const parsed = readFlagValue(input, i, flags);
    if (parsed && parsed.value && applyHarnessSelectionFlag({}, harness, parsed.name, parsed.value)) {
      if (parsed.consumedNext) i += 1;
      continue;
    }
    stripped.push(input[i]);
  }
  return stripped;
}

export function readFlagValue(args, index, flags) {
  const raw = args[index];
  if (!raw || !raw.startsWith("-")) return null;
  const trimmed = raw.replace(/^-+/, "");
  const [name, inlineValue] = splitOnce(trimmed, "=");
  if (!flags.has(name)) return null;
  if (inlineValue != null) return { name, value: inlineValue, consumedNext: false };
  if (index + 1 >= args.length || args[index + 1].startsWith("-")) {
    return { name, value: "", consumedNext: false };
  }
  return { name, value: args[index + 1], consumedNext: true };
}
export function renderHarnessArgsField(name, label, args, defaultArgs, options = {}) {
  const defaults = renderShellArgString(defaultArgs);
  const valuesAttr = options.values ? ` data-agent-args-values="${escapeAttr(JSON.stringify(options.values))}"` : "";
  const defaultsAttr = options.defaults ? ` data-agent-args-default-values="${escapeAttr(JSON.stringify(options.defaults))}"` : "";
  return `
    <label class="wide">
      <span>${escapeHTML(label)}</span>
      <textarea name="${escapeAttr(name)}_args" rows="2"${valuesAttr}${defaultsAttr}>${escapeHTML(renderShellArgString(normalizeArgList(args)))}</textarea>
      <p class="meta-quiet" data-agent-args-defaults${defaults ? "" : " hidden"}>${defaults ? `Coordinator defaults: ${escapeHTML(defaults)}` : ""}</p>
    </label>
  `;
}
export function renderShellArgString(args) {
  return normalizeArgList(args).map(renderShellArg).join(" ");
}

export function renderShellArg(arg) {
  const value = String(arg ?? "");
  if (value && /^[A-Za-z0-9_@%+=:,./-]+$/.test(value)) return value;
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

export function renderHarnessOptions(options, selected, includeMissing = false) {
  const normalized = normalizeHarnessOptions(options, []);
  const selectedValue = String(selected || "").trim();
  const hasSelected = normalized.some((option) => option.name === selectedValue);
  const rendered = normalized.map((option) => {
    const selectedAttr = option.name === selectedValue ? " selected" : "";
    return `<option value="${escapeAttr(option.name)}"${selectedAttr}>${escapeHTML(option.display_name)}</option>`;
  });
  if (includeMissing && selectedValue && !hasSelected) {
    rendered.unshift(`<option value="${escapeAttr(selectedValue)}" selected>${escapeHTML(selectedValue)}</option>`);
  }
  return rendered.join("");
}

export function resolveHarnessSelection(options, selected, includeMissing = false) {
  const normalized = normalizeHarnessOptions(options, []);
  const selectedValue = String(selected || "").trim();
  if (normalized.some((option) => option.name === selectedValue)) return selectedValue;
  if (includeMissing && selectedValue) return selectedValue;
  if (normalized.length) return normalized[0].name;
  return selectedValue || "harness";
}
