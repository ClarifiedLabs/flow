// Regression test for the blank /ui/flows and /ui/tasks pages: a custom
// element only paints when its module has run customElements.define(), and
// that only happens when the module is reachable through the app's static
// import graph. The Flows/Tasks element conversions mounted
// <flow-flows>/<flow-tasks> without importing the defining modules, so the
// browser created unresolved elements and the views rendered nothing — no
// console error, no server log, just an empty page.
//
// This test walks the import graph from the app.js entry point and asserts
// every elements/*.js module that registers an element is reachable, so the
// next conversion that forgets the import fails here instead of silently in
// the browser.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const ASSETS = path.dirname(fileURLToPath(import.meta.url));
const ENTRY = path.join(ASSETS, "app.js");

// importSpecifiers extracts the relative module specifiers a file depends on:
// static imports (including multiline specifier lists), side-effect imports,
// re-exports, and dynamic imports.
function importSpecifiers(file) {
  const source = readFileSync(file, "utf8");
  const specifiers = [];
  const pattern = /import[^;()]*?from\s*"([^"]+)"|import\s*"([^"]+)"|import\(\s*"([^"]+)"\s*\)|export[^;]*?from\s*"([^"]+)"/g;
  for (const match of source.matchAll(pattern)) {
    const specifier = match.slice(1).find(Boolean);
    if (specifier && specifier.startsWith(".")) {
      specifiers.push(path.normalize(path.join(path.dirname(file), specifier)));
    }
  }
  return specifiers;
}

function reachableFrom(entry) {
  const reachable = new Set();
  const stack = [entry];
  while (stack.length) {
    const file = stack.pop();
    if (reachable.has(file)) continue;
    reachable.add(file);
    stack.push(...importSpecifiers(file));
  }
  return reachable;
}

test("every custom-element module is reachable from the app entry", () => {
  const reachable = reachableFrom(ENTRY);
  const elementModules = readdirSync(path.join(ASSETS, "elements"))
    .filter((name) => name.endsWith(".js"))
    .map((name) => path.join(ASSETS, "elements", name))
    .filter((file) => /\bdefine\(\s*"flow-/.test(readFileSync(file, "utf8")));
  const unreachable = elementModules
    .filter((file) => !reachable.has(file))
    .map((file) => path.relative(ASSETS, file));
  assert.deepEqual(
    unreachable,
    [],
    `custom-element modules nothing imports (their elements never register): ${unreachable.join(", ")}`,
  );
});
