// Per-harness model/reasoning selection: catalog normalization, the selection
// form UI, and agent-arg (de)serialization for launches.

import { formatTokenCount } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export function normalizeHarnessOptions(options, fallback) {
  const source = Array.isArray(options) && options.length ? options : fallback;
  const seen = new Set();
  const normalized = [];
  for (const option of source) {
    const name = String(value(option, "name", "Name") || "").trim();
    if (!name || seen.has(name)) continue;
    const displayName = String(value(option, "display_name", "DisplayName") || name).trim() || name;
    normalized.push({
      name,
      display_name: displayName,
      default_args: normalizeArgList(value(option, "default_args", "DefaultArgs")),
      models: normalizeHarnessModelList(value(option, "models", "Models")),
    });
    seen.add(name);
  }
  return normalized.length ? normalized : fallback;
}

export function normalizeHarnessModelList(raw) {
  if (!Array.isArray(raw)) return [];
  const seen = new Set();
  const models = [];
  for (const item of raw) {
    const targetID = String(value(item, "target_id", "TargetID") || "").trim();
    const targetParts = splitQualifiedModel(targetID);
    const providerLabel = String(value(item, "provider_label", "ProviderLabel") || "").trim();
    const modelLabel = String(value(item, "model_label", "ModelLabel") || "").trim();
    const providerID = String(value(item, "provider_id", "ProviderID") || (targetParts && targetParts.provider) || providerLabel).trim();
    const modelID = String(value(item, "model_id", "ModelID") || modelLabel || (targetParts && targetParts.model) || "").trim();
    if (!providerID || !modelID) continue;
    const qualifiedID = String(value(item, "qualified_id", "QualifiedID") || targetID || `${providerID}:${modelID}`).trim() || `${providerID}:${modelID}`;
    if (seen.has(qualifiedID)) continue;
    seen.add(qualifiedID);
    models.push({
      target_id: targetID || qualifiedID,
      display_name: String(value(item, "display_name", "DisplayName") || "").trim(),
      provider_label: providerLabel,
      model_label: modelLabel,
      provider_id: providerID,
      provider_name: String(value(item, "provider_name", "ProviderName") || providerLabel || providerID).trim() || providerID,
      model_id: modelID,
      qualified_id: qualifiedID,
      model_name: String(value(item, "model_name", "ModelName") || value(item, "display_name", "DisplayName") || modelLabel || modelID).trim() || modelID,
      context_window: Number(value(item, "context_window", "ContextWindow") || 0),
      input_modalities: Array.isArray(value(item, "input_modalities", "InputModalities")) ? value(item, "input_modalities", "InputModalities").map((entry) => String(entry || "").trim()).filter(Boolean) : [],
      server_tools: Array.isArray(value(item, "server_tools", "ServerTools")) ? value(item, "server_tools", "ServerTools").map((entry) => String(entry || "").trim()).filter(Boolean) : [],
      price_per_million_tokens_usd: value(item, "price_per_million_tokens_usd", "PricePerMillionTokensUSD") || null,
      reasoning: normalizeHarnessReasoning(value(item, "reasoning", "Reasoning")),
    });
  }
  models.sort((a, b) => a.provider_id === b.provider_id
    ? a.model_id.localeCompare(b.model_id)
    : a.provider_id.localeCompare(b.provider_id));
  return models;
}

export function normalizeHarnessReasoning(raw) {
  if (raw === true) {
    return { supported: true, options: [{ type: "profile", values: [...HARNESS_REASONING_PROFILES] }] };
  }
  if (raw === false || raw == null) {
    return { supported: false, options: [] };
  }
  const supported = Boolean(value(raw, "supported", "Supported"));
  const options = Array.isArray(value(raw, "options", "Options"))
    ? value(raw, "options", "Options")
      .map((option) => ({
        type: String(value(option, "type", "Type") || "").trim(),
        values: Array.isArray(value(option, "values", "Values"))
          ? value(option, "values", "Values").map((item) => String(item || "").trim()).filter(Boolean)
          : [],
        min: integerOrNull(value(option, "min", "Min")),
        max: integerOrNull(value(option, "max", "Max")),
      }))
      .filter((option) => option.type)
    : [];
  return { supported, options };
}

export function normalizeHarnessArgs(raw) {
  return {
    codex: normalizeArgList(value(raw, "codex", "Codex")),
    claude: normalizeArgList(value(raw, "claude", "Claude")),
    harness: normalizeArgList(value(raw, "harness", "Harness")),
  };
}

export function normalizeArgList(raw) {
  if (!Array.isArray(raw)) return [];
  return raw.map((arg) => String(arg || "")).filter((arg) => arg.trim());
}

export function harnessDefaultArgs(options, name) {
  const normalized = normalizeHarnessOptions(options, []);
  const option = normalized.find((candidate) => candidate.name === name);
  return option ? normalizeArgList(option.default_args) : [];
}

export function harnessModels(options, name = "harness") {
  const normalized = normalizeHarnessOptions(options, []);
  const option = normalized.find((candidate) => candidate.name === name);
  return option ? normalizeHarnessModelList(option.models) : [];
}

// renderHarnessModelFields renders the model/reasoning fieldset for the active
// harness. It embeds every harness's model catalog and parsed selection so
// bindHarnessModelControls can re-render the inner controls when the agent
// harness changes without another round trip.
export function renderHarnessModelFields(options, selectionByHarness, agentHarness) {
  const catalog = {};
  for (const name of ["codex", "claude", "harness"]) {
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

export function harnessModelLabel(model, includeProvider = false) {
  const provider = model.provider_name || model.provider_id;
  const label = `${model.model_name || model.model_id}${model.context_window ? ` (${formatTokenCount(model.context_window)} ctx)` : ""}`;
  return includeProvider && provider ? `${provider} / ${label}` : label;
}

export function uniqueHarnessProviders(models) {
  const seen = new Set();
  const providers = [];
  for (const model of models) {
    if (seen.has(model.provider_id)) continue;
    seen.add(model.provider_id);
    providers.push({ id: model.provider_id, name: model.provider_name || model.provider_id });
  }
  return providers;
}

export const HARNESS_REASONING_UNAVAILABLE = "unavailable";
export const HARNESS_REASONING_PROFILES = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];

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

// HARNESS_SELECTION_FLAGS maps each harness to the flag names it uses to carry
// a Flow-managed model/reasoning selection. The harness CLI treats
// provider:model as a target id, but we still parse old stored --provider args.
//   - harness: --model provider:model + --reasoning <profile>
//   - claude:  --model + --effort
//   - codex:   --model (or -m) + -c model_reasoning_effort=<level>
export const HARNESS_SELECTION_FLAGS = {
  harness: new Set(["provider", "model", "reasoning", "reasoning-effort", "reasoning-enabled", "reasoning-budget-tokens"]),
  claude: new Set(["model", "m", "effort"]),
  codex: new Set(["model", "m", "c", "config"]),
};

export const CODEX_REASONING_EFFORT_KEY = "model_reasoning_effort";

export function harnessSelectionFlags(harness) {
  return HARNESS_SELECTION_FLAGS[harness] || HARNESS_SELECTION_FLAGS.harness;
}

// applyHarnessSelectionFlag records one recognized selection flag onto selection
// and returns true. It returns false when the token is not a selection flag for
// this harness (e.g. a codex `-c key=value` that is not model_reasoning_effort),
// so the caller can keep it as an additional arg.
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
  if (harness === "claude" && name === "effort") {
    selection.reasoning_mode = "effort";
    selection.reasoning_effort = value;
    selection.reasoning_budget_tokens = null;
    return true;
  }
  if (harness === "codex" && (name === "c" || name === "config")) {
    const [key, effort] = splitOnce(value, "=");
    if (key === CODEX_REASONING_EFFORT_KEY && effort) {
      selection.reasoning_mode = "effort";
      selection.reasoning_effort = effort;
      selection.reasoning_budget_tokens = null;
      return true;
    }
    return false;
  }
  return false;
}

// serializeHarnessModelSelection renders the per-harness argv tokens for a model
// + reasoning choice. It is the inverse of parseHarnessSelectionArgs.
export function serializeHarnessModelSelection(harness, model, reasoning) {
  const args = harness === "harness"
    ? ["--model", model.target_id || model.qualified_id || model.model_id]
    : ["--model", model.model_id];
  const mode = reasoning.mode || "default";
  if (mode === "effort") {
    const effort = String(reasoning.effort || "").trim();
    if (!effort) throw new Error("Reasoning effort is required");
    if (harness === "claude") {
      args.push("--effort", effort);
    } else if (harness === "codex") {
      args.push("-c", `${CODEX_REASONING_EFFORT_KEY}=${effort}`);
    } else {
      args.push("--reasoning", effort);
    }
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

export function splitOnce(value, separator) {
  const index = String(value).indexOf(separator);
  if (index < 0) return [value, null];
  return [value.slice(0, index), value.slice(index + separator.length)];
}

export function splitQualifiedModel(model) {
  const [provider, bareModel] = splitOnce(String(model || "").trim(), ":");
  if (!provider || !bareModel) return null;
  if (!/^[a-zA-Z0-9._-]+$/.test(provider)) return null;
  return { provider: provider.toLowerCase(), model: bareModel };
}

export function findHarnessModel(models, qualifiedID) {
  const id = String(qualifiedID || "").trim();
  if (!id) return null;
  return normalizeHarnessModelList(models).find((model) => model.qualified_id === id) || null;
}

export function findHarnessModelByParts(models, provider, modelID) {
  const normalized = normalizeHarnessModelList(models);
  provider = String(provider || "").trim();
  modelID = String(modelID || "").trim();
  if (provider && modelID) {
    return normalized.find((model) => model.provider_id === provider && model.model_id === modelID) || null;
  }
  if (!modelID) return null;
  const matches = normalized.filter((model) => model.model_id === modelID);
  return matches.length === 1 ? matches[0] : null;
}

export function reasoningOption(model, type) {
  const options = model?.reasoning?.options || [];
  return options.find((option) => option.type === type) || null;
}

export function harnessReasoningLevelValues(model) {
  if (!model?.reasoning?.supported) return [];
  return reasoningOption(model, "profile")?.values || reasoningOption(model, "effort")?.values || [];
}

export function supportsReasoningToggle(model) {
  if (!model?.reasoning?.supported) return false;
  const options = model.reasoning.options || [];
  return options.length === 0 || options.some((option) => option.type === "toggle");
}

export function parseReasoningBudget(raw, model) {
  const budget = integerOrNull(raw);
  if (budget == null) throw new Error("Reasoning budget must be a non-negative integer");
  const option = reasoningOption(model, "budget_tokens");
  if (!option) throw new Error("Selected model does not support reasoning budget tokens");
  if (option.min != null && budget < option.min) {
    throw new Error(`Reasoning budget must be at least ${option.min}`);
  }
  if (option.max != null && budget > option.max) {
    throw new Error(`Reasoning budget must be at most ${option.max}`);
  }
  return budget;
}

export function integerOrNull(value) {
  if (value === null || value === undefined || value === "") return null;
  const number = Number(value);
  return Number.isInteger(number) && number >= 0 ? number : null;
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

export function parseJSONAttribute(raw, fallback) {
  if (!raw) return fallback;
  try {
    return JSON.parse(raw);
  } catch (_error) {
    return fallback;
  }
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
  return selectedValue || "codex";
}
