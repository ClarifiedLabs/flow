// Node test for the harness model selection serialize/parse/strip logic in
// the models/harness-* modules. Loaded as native ESM: install the minimal DOM
// stubs their import chain's load-time side effects touch, then import the
// named exports. Run with: make js-test
import assert from "node:assert";

const jsonEq = (actual, expected, msg) => assert.strictEqual(JSON.stringify(actual), JSON.stringify(expected), msg);

globalThis.customElements = { define() {} };
globalThis.document = { addEventListener() {} };
globalThis.window = {};
globalThis.history = {};
globalThis.HTMLElement = class {};

const { normalizeHarnessModelList } = await import("./models/harness-catalog.js");
const {
  parseHarnessSelectionArgs,
  stripHarnessSelectionArgs,
  serializeHarnessModelSelection,
} = await import("./models/harness-form.js");

assert.strictEqual(typeof normalizeHarnessModelList, "function", "normalizeHarnessModelList loaded");
assert.strictEqual(typeof parseHarnessSelectionArgs, "function", "parseHarnessSelectionArgs loaded");
assert.strictEqual(typeof serializeHarnessModelSelection, "function", "serializeHarnessModelSelection loaded");
assert.strictEqual(typeof stripHarnessSelectionArgs, "function", "stripHarnessSelectionArgs loaded");

const profile = (values) => ({ supported: true, options: [{ type: "profile", values }] });
const catalog = {
  harness: [{
    target_id: "anthropic:claude-opus-4-8", provider_id: "anthropic", provider_name: "Anthropic", model_id: "claude-opus-4-8",
    qualified_id: "anthropic:claude-opus-4-8", model_name: "Opus", reasoning: profile(["none", "minimal", "low", "high", "max"]),
  }, {
    target_id: "openrouter:openai/gpt-5.5", provider_id: "openrouter", provider_name: "OpenRouter", model_id: "openai/gpt-5.5",
    qualified_id: "openrouter:openai/gpt-5.5", reasoning: true,
  }],
};

let passed = 0;
function check(name, fn) {
  fn();
  passed += 1;
}

// --- serialize spellings ---------------------------------------------------
check("normalizes harness v0.0.19 target models with boolean reasoning", () => {
  const models = normalizeHarnessModelList([{
    target_id: "openrouter:openai/gpt-5.5",
    display_name: "OpenAI GPT-5.5",
    provider_label: "OpenRouter",
    model_label: "openai/gpt-5.5",
    reasoning: true,
  }]);
  assert.strictEqual(models[0].provider_id, "openrouter");
  assert.strictEqual(models[0].provider_name, "OpenRouter");
  assert.strictEqual(models[0].model_id, "openai/gpt-5.5");
  assert.strictEqual(models[0].qualified_id, "openrouter:openai/gpt-5.5");
  assert.strictEqual(models[0].model_name, "OpenAI GPT-5.5");
  assert.strictEqual(models[0].reasoning.options[0].type, "profile");
  jsonEq(models[0].reasoning.options[0].values, ["none", "minimal", "low", "medium", "high", "xhigh", "max"]);
});
check("harness serializes target --model + --reasoning profile", () => {
  const args = serializeHarnessModelSelection("harness", catalog.harness[0], { mode: "effort", effort: "high" });
  jsonEq(args, ["--model", "anthropic:claude-opus-4-8", "--reasoning", "high"]);
});
check("default mode emits only the model", () => {
  jsonEq(serializeHarnessModelSelection("harness", catalog.harness[0], { mode: "default" }), ["--model", "anthropic:claude-opus-4-8"]);
});

// --- serialize -> parse round-trips ---------------------------------------
function roundTrip(model, reasoning, expect) {
  const args = serializeHarnessModelSelection("harness", model, reasoning);
  const sel = parseHarnessSelectionArgs(args, catalog.harness, "harness");
  assert.strictEqual(sel.qualified_id, expect.qualified_id, "qualified_id");
  assert.strictEqual(sel.reasoning_mode, expect.reasoning_mode, "reasoning_mode");
  assert.strictEqual(sel.reasoning_effort, expect.reasoning_effort || "", "reasoning_effort");
  jsonEq(sel.additional_args, [], "additional_args empty");
}
check("harness round-trips reasoning profile", () => roundTrip(catalog.harness[0], { mode: "effort", effort: "low" },
  { qualified_id: "anthropic:claude-opus-4-8", reasoning_mode: "effort", reasoning_effort: "low" }));

// --- additional-arg preservation ------------------------------------------
check("harness leaves unknown flags in additional args", () => {
  const sel = parseHarnessSelectionArgs(["--model", "anthropic:claude-opus-4-8", "--verbose"], catalog.harness, "harness");
  assert.strictEqual(sel.qualified_id, "anthropic:claude-opus-4-8");
  jsonEq(sel.additional_args, ["--verbose"]);
});
check("harness parses old provider plus bare model args", () => {
  const sel = parseHarnessSelectionArgs(["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning", "high"], catalog.harness, "harness");
  assert.strictEqual(sel.qualified_id, "anthropic:claude-opus-4-8");
  assert.strictEqual(sel.reasoning_effort, "high");
  jsonEq(sel.additional_args, []);
});
check("harness strips legacy budget reasoning flags from managed selection", () => {
  const sel = parseHarnessSelectionArgs(
    ["--model", "openrouter:openai/gpt-5.5", "--reasoning-budget-tokens", "2048", "--label", "fast"],
    catalog.harness, "harness",
  );
  assert.strictEqual(sel.qualified_id, "openrouter:openai/gpt-5.5");
  assert.strictEqual(sel.reasoning_mode, "legacy");
  jsonEq(sel.additional_args, ["--label", "fast"]);
});
check("harness strips legacy toggle reasoning flags from managed selection", () => {
  const sel = parseHarnessSelectionArgs(
    ["--model", "anthropic:claude-opus-4-8", "--reasoning-enabled", "true"],
    catalog.harness, "harness",
  );
  assert.strictEqual(sel.qualified_id, "anthropic:claude-opus-4-8");
  assert.strictEqual(sel.reasoning_mode, "legacy");
  jsonEq(sel.additional_args, []);
});

// --- strip ----------------------------------------------------------------
check("strip removes only the harness selection flags", () => {
  jsonEq(
    stripHarnessSelectionArgs(["--model", "anthropic:claude-opus-4-8", "--reasoning", "low", "tail"], "harness"),
    ["tail"],
  );
  jsonEq(
    stripHarnessSelectionArgs(["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning", "low", "tail"], "harness"),
    ["tail"],
  );
  jsonEq(
    stripHarnessSelectionArgs(["--model", "anthropic:claude-opus-4-8", "--foo", "bar"], "harness"),
    ["--foo", "bar"],
  );
});

console.log(`ok - ${passed} harness model serialization checks passed`);
