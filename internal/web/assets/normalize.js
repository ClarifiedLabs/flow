// Reads the first present (non-null, non-undefined) of several keys off a Go
// JSON object, tolerating both snake_case and PascalCase spellings. This is the
// single most-used helper in the UI, so it lives in its own dependency-free leaf
// that every other module can import without risking a cycle.
export function value(object, ...keys) {
  if (!object) return "";
  for (const key of keys) {
    if (object[key] !== undefined && object[key] !== null) return object[key];
  }
  return "";
}
