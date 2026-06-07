// Node test for the per-harness model selection serialize/parse/strip logic in
// app.js. There is no browser test harness in this repo, so the entry module is
// loaded as native ESM: install the minimal DOM stubs its load-time side effects
// touch (customElements.define / document listeners / HTMLElement), then import
// the named exports. Run with: node internal/web/assets/harness_models.test.mjs
import assert from "node:assert";

const jsonEq = (actual, expected, msg) => assert.strictEqual(JSON.stringify(actual), JSON.stringify(expected), msg);

globalThis.customElements = { define() {} };
globalThis.document = { addEventListener() {} };
globalThis.window = {};
globalThis.history = {};
globalThis.HTMLElement = class {};

const {
  parseHarnessSelectionArgs,
  stripHarnessSelectionArgs,
  serializeHarnessModelSelection,
} = await import("./app.js");

assert.strictEqual(typeof parseHarnessSelectionArgs, "function", "parseHarnessSelectionArgs loaded");
assert.strictEqual(typeof serializeHarnessModelSelection, "function", "serializeHarnessModelSelection loaded");
assert.strictEqual(typeof stripHarnessSelectionArgs, "function", "stripHarnessSelectionArgs loaded");

const effort = (values) => ({ supported: true, options: [{ type: "effort", values }] });
const catalog = {
  harness: [{
    provider_id: "anthropic", provider_name: "Anthropic", model_id: "claude-opus-4-8",
    qualified_id: "anthropic:claude-opus-4-8", model_name: "Opus", reasoning: effort(["low", "high"]),
  }, {
    provider_id: "anthropic", provider_name: "Anthropic", model_id: "claude-budget",
    qualified_id: "anthropic:claude-budget", reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0 }] },
  }],
  claude: [{
    provider_id: "anthropic", model_id: "claude-opus-4-8", qualified_id: "anthropic:claude-opus-4-8",
    reasoning: effort(["low", "medium", "high", "xhigh", "max"]),
  }],
  codex: [{
    provider_id: "openai", model_id: "gpt-5.5", qualified_id: "openai:gpt-5.5",
    reasoning: effort(["low", "medium", "high", "xhigh"]),
  }],
};

let passed = 0;
function check(name, fn) {
  fn();
  passed += 1;
}

// --- serialize spellings ---------------------------------------------------
check("claude serializes --model + --effort", () => {
  const args = serializeHarnessModelSelection("claude", catalog.claude[0], { mode: "effort", effort: "high" });
  jsonEq(args, ["--model", "claude-opus-4-8", "--effort", "high"]);
});
check("codex serializes --model + -c model_reasoning_effort", () => {
  const args = serializeHarnessModelSelection("codex", catalog.codex[0], { mode: "effort", effort: "xhigh" });
  jsonEq(args, ["--model", "gpt-5.5", "-c", "model_reasoning_effort=xhigh"]);
});
check("harness serializes --provider/--model + --reasoning-effort", () => {
  const args = serializeHarnessModelSelection("harness", catalog.harness[0], { mode: "effort", effort: "high" });
  jsonEq(args, ["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning-effort", "high"]);
});
check("default mode emits only the model", () => {
  jsonEq(serializeHarnessModelSelection("claude", catalog.claude[0], { mode: "default" }), ["--model", "claude-opus-4-8"]);
  jsonEq(serializeHarnessModelSelection("codex", catalog.codex[0], { mode: "default" }), ["--model", "gpt-5.5"]);
});
check("harness budget + toggle spellings", () => {
  jsonEq(
    serializeHarnessModelSelection("harness", catalog.harness[1], { mode: "budget", budget: "2048" }),
    ["--provider", "anthropic", "--model", "claude-budget", "--reasoning-budget-tokens", "2048"],
  );
  jsonEq(
    serializeHarnessModelSelection("harness", catalog.harness[0], { mode: "on" }),
    ["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning-enabled", "true"],
  );
});

// --- serialize -> parse round-trips ---------------------------------------
function roundTrip(harness, model, reasoning, expect) {
  const args = serializeHarnessModelSelection(harness, model, reasoning);
  const sel = parseHarnessSelectionArgs(args, catalog[harness], harness);
  assert.strictEqual(sel.qualified_id, expect.qualified_id, `${harness} qualified_id`);
  assert.strictEqual(sel.reasoning_mode, expect.reasoning_mode, `${harness} reasoning_mode`);
  assert.strictEqual(sel.reasoning_effort, expect.reasoning_effort || "", `${harness} reasoning_effort`);
  jsonEq(sel.additional_args, [], `${harness} additional_args empty`);
}
check("claude round-trips effort", () => roundTrip("claude", catalog.claude[0], { mode: "effort", effort: "max" },
  { qualified_id: "anthropic:claude-opus-4-8", reasoning_mode: "effort", reasoning_effort: "max" }));
check("codex round-trips effort", () => roundTrip("codex", catalog.codex[0], { mode: "effort", effort: "high" },
  { qualified_id: "openai:gpt-5.5", reasoning_mode: "effort", reasoning_effort: "high" }));
check("harness round-trips effort", () => roundTrip("harness", catalog.harness[0], { mode: "effort", effort: "low" },
  { qualified_id: "anthropic:claude-opus-4-8", reasoning_mode: "effort", reasoning_effort: "low" }));

// --- additional-arg preservation ------------------------------------------
check("codex keeps unrelated -c overrides as additional args", () => {
  const sel = parseHarnessSelectionArgs(
    ["--model", "gpt-5.5", "--foo", "bar", "-c", "sandbox=workspace-write"],
    catalog.codex, "codex",
  );
  assert.strictEqual(sel.qualified_id, "openai:gpt-5.5");
  jsonEq(sel.additional_args, ["--foo", "bar", "-c", "sandbox=workspace-write"]);
});
check("codex consumes -c model_reasoning_effort but not other -c", () => {
  const sel = parseHarnessSelectionArgs(
    ["-c", "model_reasoning_effort=high", "-c", "other=1"],
    catalog.codex, "codex",
  );
  assert.strictEqual(sel.reasoning_mode, "effort");
  assert.strictEqual(sel.reasoning_effort, "high");
  jsonEq(sel.additional_args, ["-c", "other=1"]);
});
check("claude leaves unknown flags in additional args", () => {
  const sel = parseHarnessSelectionArgs(["--model", "claude-opus-4-8", "--verbose"], catalog.claude, "claude");
  jsonEq(sel.additional_args, ["--verbose"]);
});

// --- strip ----------------------------------------------------------------
check("strip removes only per-harness selection flags", () => {
  jsonEq(
    stripHarnessSelectionArgs(["--model", "gpt-5.5", "-c", "model_reasoning_effort=high", "--foo", "-c", "other=1"], "codex"),
    ["--foo", "-c", "other=1"],
  );
  jsonEq(
    stripHarnessSelectionArgs(["--model", "claude-opus-4-8", "--effort", "high", "extra"], "claude"),
    ["extra"],
  );
  jsonEq(
    stripHarnessSelectionArgs(["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning-effort", "low", "tail"], "harness"),
    ["tail"],
  );
});

console.log(`ok - ${passed} harness model serialization checks passed`);
