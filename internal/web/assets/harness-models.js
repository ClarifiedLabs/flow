// Per-harness model/provider/reasoning selection: catalog normalization, the
// selection form UI, and agent-arg (de)serialization for launches.

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
    const providerID = String(value(item, "provider_id", "ProviderID") || "").trim();
    const modelID = String(value(item, "model_id", "ModelID") || "").trim();
    if (!providerID || !modelID) continue;
    const qualifiedID = String(value(item, "qualified_id", "QualifiedID") || `${providerID}:${modelID}`).trim() || `${providerID}:${modelID}`;
    if (seen.has(qualifiedID)) continue;
    seen.add(qualifiedID);
    models.push({
      provider_id: providerID,
      provider_name: String(value(item, "provider_name", "ProviderName") || providerID).trim() || providerID,
      model_id: modelID,
      qualified_id: qualifiedID,
      model_name: String(value(item, "model_name", "ModelName") || modelID).trim() || modelID,
      context_window: Number(value(item, "context_window", "ContextWindow") || 0),
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

// renderHarnessModelControls renders the provider/model/reasoning controls for a
// single harness's model list. Re-rendered per harness by bindHarnessModelControls.
export function renderHarnessModelControls(models, selection) {
  if (!models.length) return "";
  const selected = selection || parseHarnessSelectionArgs([], models);
  const selectedModel = selected.qualified_id ? findHarnessModel(models, selected.qualified_id) : null;
  const providers = uniqueHarnessProviders(models);
  const requestedProvider = selected.provider || (selectedModel && selectedModel.provider_id) || models[0].provider_id;
  const selectedProvider = providers.some((provider) => provider.id === requestedProvider) ? requestedProvider : models[0].provider_id;
  const selectedModelID = selectedModel && selectedModel.provider_id === selectedProvider ? selectedModel.qualified_id : "";
  return `
    <label>
      <span>Provider</span>
      <select name="harness_provider">
        ${providers.map((provider) => `<option value="${escapeAttr(provider.id)}" ${provider.id === selectedProvider ? "selected" : ""}>${escapeHTML(provider.name)}</option>`).join("")}
      </select>
    </label>
    <label>
      <span>Model</span>
      <select name="harness_model">
        ${renderHarnessModelOptions(models, selectedProvider, selectedModelID)}
      </select>
    </label>
    <div class="harness-reasoning" data-harness-reasoning-controls>
      ${renderHarnessReasoningControls(selectedModelID ? selectedModel : null, selected)}
    </div>
  `;
}

export function renderHarnessModelOptions(models, provider, selectedQualifiedID) {
  const selectedID = String(selectedQualifiedID || "").trim();
  const visibleModels = models.filter((model) => !provider || model.provider_id === provider);
  const selectedVisible = visibleModels.some((model) => model.qualified_id === selectedID);
  return `
        <option value="" ${selectedVisible ? "" : "selected"}>Default model</option>
        ${visibleModels.map((model) => `<option value="${escapeAttr(model.qualified_id)}" data-provider="${escapeAttr(model.provider_id)}" ${model.qualified_id === selectedID ? "selected" : ""}>${escapeHTML(harnessModelLabel(model))}</option>`).join("")}`;
}

export function harnessModelLabel(model) {
  return `${model.model_name || model.model_id}${model.context_window ? ` (${formatTokenCount(model.context_window)} ctx)` : ""}`;
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

export function renderHarnessReasoningControls(model, selection = {}) {
  const modes = [{ value: "default", label: "Provider default" }];
  const effort = reasoningOption(model, "effort");
  const budget = reasoningOption(model, "budget_tokens");
  if (model && model.reasoning && model.reasoning.supported) {
    if (supportsReasoningToggle(model)) {
      modes.push({ value: "on", label: "On" }, { value: "off", label: "Off" });
    }
    if (effort && effort.values.length) {
      modes.push({ value: "effort", label: "Effort" });
    }
    if (budget) {
      modes.push({ value: "budget", label: "Token budget" });
    }
  }
  const selectedMode = modes.some((mode) => mode.value === selection.reasoning_mode) ? selection.reasoning_mode : "default";
  const effortValue = selection.reasoning_effort || (effort && effort.values[0]) || "";
  const budgetValue = selection.reasoning_budget_tokens == null ? "" : String(selection.reasoning_budget_tokens);
  return `
    <label>
      <span>Reasoning</span>
      <select name="harness_reasoning_mode">
        ${modes.map((mode) => `<option value="${escapeAttr(mode.value)}" ${mode.value === selectedMode ? "selected" : ""}>${escapeHTML(mode.label)}</option>`).join("")}
      </select>
    </label>
    <label data-harness-reasoning-effort-field${selectedMode === "effort" ? "" : " hidden"}>
      <span>Effort</span>
      <select name="harness_reasoning_effort">
        ${(effort?.values || []).map((value) => `<option value="${escapeAttr(value)}" ${value === effortValue ? "selected" : ""}>${escapeHTML(value)}</option>`).join("")}
      </select>
    </label>
    <label data-harness-reasoning-budget-field${selectedMode === "budget" ? "" : " hidden"}>
      <span>Budget tokens</span>
      <input name="harness_reasoning_budget_tokens" type="number" min="${escapeAttr(String(budget?.min ?? 0))}" ${budget?.max == null ? "" : `max="${escapeAttr(String(budget.max))}"`} step="1" value="${escapeAttr(budgetValue)}">
    </label>
  `;
}

export function renderHarnessReasoningInto(fieldset, model, preserve) {
  if (!fieldset || typeof fieldset.querySelector !== "function") return;
  const container = fieldset.querySelector("[data-harness-reasoning-controls]");
  if (!container) return;
  const current = preserve ? readHarnessReasoningSelection(fieldset) : {};
  container.innerHTML = renderHarnessReasoningControls(model, current);
  const mode = fieldset.querySelector('[name="harness_reasoning_mode"]');
  if (mode && typeof mode.addEventListener === "function") {
    mode.addEventListener("change", () => syncHarnessReasoningVisibility(fieldset));
  }
  syncHarnessReasoningVisibility(fieldset);
}

export function readHarnessReasoningSelection(root) {
  if (!root || typeof root.querySelector !== "function") return {};
  const mode = root.querySelector('[name="harness_reasoning_mode"]')?.value || "default";
  return {
    reasoning_mode: mode,
    reasoning_effort: root.querySelector('[name="harness_reasoning_effort"]')?.value || "",
    reasoning_budget_tokens: integerOrNull(root.querySelector('[name="harness_reasoning_budget_tokens"]')?.value),
  };
}

export function syncHarnessReasoningVisibility(root) {
  if (!root || typeof root.querySelector !== "function") return;
  const mode = root.querySelector('[name="harness_reasoning_mode"]')?.value || "default";
  const effort = root.querySelector("[data-harness-reasoning-effort-field]");
  const budget = root.querySelector("[data-harness-reasoning-budget-field]");
  if (effort) effort.hidden = mode !== "effort";
  if (budget) budget.hidden = mode !== "budget";
}

// HARNESS_SELECTION_FLAGS maps each harness to the flag names it uses to carry a
// Flow-managed model/reasoning selection, so parse/strip/serialize agree on the
// per-harness spelling:
//   - harness: --provider/--model + --reasoning-effort/-budget-tokens/-enabled
//   - claude:  --model + --effort
//   - codex:   --model (or -m) + -c model_reasoning_effort=<level>
export const HARNESS_SELECTION_FLAGS = {
  harness: new Set(["provider", "model", "reasoning-effort", "reasoning-enabled", "reasoning-budget-tokens"]),
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
    if (name === "reasoning-effort") {
      selection.reasoning_mode = "effort";
      selection.reasoning_effort = value;
      selection.reasoning_budget_tokens = null;
      return true;
    }
    if (name === "reasoning-enabled") {
      const normalized = value.toLowerCase();
      if (normalized === "true" || normalized === "false") {
        selection.reasoning_mode = normalized === "true" ? "on" : "off";
        selection.reasoning_effort = "";
        selection.reasoning_budget_tokens = null;
        return true;
      }
      return false;
    }
    if (name === "reasoning-budget-tokens") {
      const tokens = integerOrNull(value);
      if (tokens != null) {
        selection.reasoning_mode = "budget";
        selection.reasoning_budget_tokens = tokens;
        selection.reasoning_effort = "";
        return true;
      }
      return false;
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
    ? ["--provider", model.provider_id, "--model", model.model_id]
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
      args.push("--reasoning-effort", effort);
    }
  } else if (harness === "harness" && mode === "budget") {
    const budget = parseReasoningBudget(reasoning.budget, model);
    args.push("--reasoning-budget-tokens", String(budget));
  } else if (harness === "harness" && mode === "on") {
    args.push("--reasoning-enabled", "true");
  } else if (harness === "harness" && mode === "off") {
    args.push("--reasoning-enabled", "false");
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
    if (!parsed || !parsed.value || !applyHarnessSelectionFlag(selection, harness, parsed.name, parsed.value)) {
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
