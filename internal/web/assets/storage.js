// Typed localStorage read/write helpers for UI preferences (projects, theme,
// done-view and board-done config, agent defaults) plus pure path -> route /
// poll-config parsing.

import { BOARD_DONE_COUNTS, BOARD_DONE_STORAGE_KEY, BOARD_DONE_WINDOWS, BOARD_POLL_MS, CHANGE_POLL_MS, DIAGNOSTICS_POLL_MS, DIFF_MODES, DIFF_MODE_STORAGE_KEY, DONE_DENSITIES, DONE_DENSITY_STORAGE_KEY, DONE_OUTCOMES, DONE_OUTCOME_STORAGE_KEY, ISSUE_AGENT_DEFAULTS_STORAGE_KEY, MAX_POLL_BACKOFF_MS, PROJECT_STORAGE_KEY, THEME_PREFERENCES, THEME_STORAGE_KEY } from "./config.js";
import { normalizeHarnessArgs } from "./harness-models.js";
import { value } from "./normalize.js";

export function readSelectedProjects() {
  try {
    const raw = window.localStorage.getItem(PROJECT_STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((id) => typeof id === "string" && id) : [];
  } catch (error) {
    return [];
  }
}

export function writeSelectedProjects(ids) {
  try {
    if (!ids || !ids.length) {
      window.localStorage.removeItem(PROJECT_STORAGE_KEY);
    } else {
      window.localStorage.setItem(PROJECT_STORAGE_KEY, JSON.stringify(ids));
    }
  } catch (error) {
    // Selection persistence is best-effort.
  }
}

export function boardDoneConfig() {
  const fallback = { mode: "count", count: 20, within: "7d", outcome: "all" };
  try {
    const raw = window.localStorage?.getItem(BOARD_DONE_STORAGE_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) || {};
    return {
      mode: parsed.mode === "within" ? "within" : "count",
      count: BOARD_DONE_COUNTS.includes(parsed.count) ? parsed.count : 20,
      within: BOARD_DONE_WINDOWS.includes(parsed.within) ? parsed.within : "7d",
      outcome: DONE_OUTCOMES.has(parsed.outcome) ? parsed.outcome : "all",
    };
  } catch {
    return fallback;
  }
}

export function writeBoardDoneConfig(config) {
  try {
    window.localStorage?.setItem(BOARD_DONE_STORAGE_KEY, JSON.stringify(config));
  } catch {
    // Persistence is best-effort.
  }
}

export function readDoneDensity() {
  try {
    const raw = window.localStorage?.getItem(DONE_DENSITY_STORAGE_KEY);
    return DONE_DENSITIES.has(raw) ? raw : "extended";
  } catch {
    return "extended";
  }
}

export function writeDoneDensity(density) {
  try {
    if (DONE_DENSITIES.has(density)) window.localStorage?.setItem(DONE_DENSITY_STORAGE_KEY, density);
  } catch {
    // Persistence is best-effort.
  }
}

export function readDiffMode() {
  try {
    const raw = window.localStorage?.getItem(DIFF_MODE_STORAGE_KEY);
    return DIFF_MODES.has(raw) ? raw : "unified";
  } catch {
    return "unified";
  }
}

export function writeDiffMode(mode) {
  try {
    if (DIFF_MODES.has(mode)) window.localStorage?.setItem(DIFF_MODE_STORAGE_KEY, mode);
  } catch {
    // Persistence is best-effort.
  }
}

export function readDoneOutcome() {
  try {
    const raw = window.localStorage?.getItem(DONE_OUTCOME_STORAGE_KEY);
    return DONE_OUTCOMES.has(raw) ? raw : "all";
  } catch {
    return "all";
  }
}

export function writeDoneOutcome(outcome) {
  try {
    if (DONE_OUTCOMES.has(outcome)) window.localStorage?.setItem(DONE_OUTCOME_STORAGE_KEY, outcome);
  } catch {
    // Persistence is best-effort.
  }
}

export function readIssueAgentDefaults() {
  try {
    const raw = window.localStorage?.getItem(ISSUE_AGENT_DEFAULTS_STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    return normalizeIssueAgentDefaults(parsed);
  } catch (_error) {
    return {};
  }
}

export function writeIssueAgentDefaults(defaults) {
  const normalized = normalizeIssueAgentDefaults(defaults);
  try {
    if (!window.localStorage || typeof window.localStorage.setItem !== "function") return false;
    window.localStorage.setItem(ISSUE_AGENT_DEFAULTS_STORAGE_KEY, JSON.stringify({
      version: 1,
      ...normalized,
    }));
    return true;
  } catch (_error) {
    return false;
  }
}

export function normalizeIssueAgentDefaults(raw) {
  const harnessArgs = normalizeHarnessArgs(value(raw, "harness_args", "HarnessArgs"));
  return {
    agent_harness: String(value(raw, "agent_harness", "AgentHarness") || "codex").trim() || "codex",
    harness_args: harnessArgs,
  };
}

export function routeFilter(path) {
  if (path === "/ui/triage") return { lane: "backlog", state: "triage", label: "Triage" };
  if (path === "/ui/feedback") return { lane: "needs_attention", label: "Needs Attention" };
  if (path === "/ui/merge") return { lane: "needs_attention", state: "ready_to_merge", label: "Merge" };
  return null;
}

export function terminalSessionIDForPath(path) {
  const match = String(path || "").match(/^\/ui\/sessions\/([^/]+)\/terminal$/);
  if (!match) return "";
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return "";
  }
}

export function pollConfigForPath(path) {
  if (path === "/ui" || path === "/ui/" || path === "/ui/board" || path === "/ui/triage" || path === "/ui/feedback" || path === "/ui/merge") {
    return { interval: BOARD_POLL_MS, maxInterval: BOARD_POLL_MS, backoff: false };
  }
  if (path.startsWith("/ui/changes/")) {
    return { interval: CHANGE_POLL_MS, maxInterval: CHANGE_POLL_MS, backoff: false };
  }
  if (path === "/ui/workers" || path === "/ui/jobs") {
    return { interval: DIAGNOSTICS_POLL_MS, maxInterval: MAX_POLL_BACKOFF_MS, backoff: true };
  }

  return null;
}

export function normalizeThemePreference(theme) {
  return THEME_PREFERENCES.has(theme) ? theme : "system";
}

export function readThemePreference() {
  try {
    return normalizeThemePreference(window.localStorage?.getItem(THEME_STORAGE_KEY));
  } catch {
    return "system";
  }
}

export function writeThemePreference(theme) {
  const preference = normalizeThemePreference(theme);
  try {
    if (preference === "system") {
      window.localStorage?.removeItem(THEME_STORAGE_KEY);
    } else {
      window.localStorage?.setItem(THEME_STORAGE_KEY, preference);
    }
  } catch {
    // Keep the in-page theme working when storage is unavailable.
  }
  return preference;
}

export function applyThemePreference(theme) {
  const preference = normalizeThemePreference(theme);
  const root = document.documentElement;
  if (!root) return preference;
  if (preference === "system") {
    root.removeAttribute?.("data-theme");
  } else {
    root.setAttribute?.("data-theme", preference);
  }
  return preference;
}
