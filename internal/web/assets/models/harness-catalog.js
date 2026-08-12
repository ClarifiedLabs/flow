// Harness model/reasoning catalog: normalization of the server's harness
// options and models, and the pure catalog queries the selection UI and the
// agent-arg (de)serialization share.

import { formatTokenCount } from "../format.js";
import { value } from "../normalize.js";

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
export function parseJSONAttribute(raw, fallback) {
  if (!raw) return fallback;
  try {
    return JSON.parse(raw);
  } catch (_error) {
    return fallback;
  }
}
