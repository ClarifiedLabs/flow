// Typed localStorage read/write helpers for UI preferences (projects, theme,
// done-view config) plus pure path -> route / poll-config parsing.

import { BOARD_SORT_DIRS, BOARD_SORT_KEYS, BOARD_SORT_STORAGE_KEY, BOARD_VIEWS, BOARD_VIEW_STORAGE_KEY, DIAGRAM_MODES, DIAGRAM_MODE_STORAGE_KEY, BOARD_POLL_MS, CHANGE_POLL_MS, DIAGNOSTICS_POLL_MS, DIFF_MODES, DIFF_MODE_STORAGE_KEY, DONE_DENSITIES, DONE_DENSITY_STORAGE_KEY, DONE_OUTCOMES, DONE_OUTCOME_STORAGE_KEY, MAX_POLL_BACKOFF_MS, PROJECT_STORAGE_KEY, TASKS_ALL_STATE, TASKS_PROJECT_STORAGE_KEY, TASKS_QUERY_STORAGE_KEY, TASKS_STATE_STORAGE_KEY, TASKS_STATES, TASKS_LIST_VIEWS, TASKS_LIST_VIEW_STORAGE_KEY, WORK_PREFERENCES_STORAGE_KEY, WORK_PROJECT_STORAGE_KEY, THEME_PREFERENCES, THEME_STORAGE_KEY } from "./config.js";
import { WORK_ITEM_FILTERS, WORK_ITEM_VIEWS } from "./work-item-model.js";

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

// Which board view you prefer is a property of how you work, not of a visit,
// so it persists. The diagram mode is per-session: it tracks what you happen
// to be doing with one task.
export function readBoardView() {
  try {
    const raw = window.localStorage?.getItem(BOARD_VIEW_STORAGE_KEY);
    return BOARD_VIEWS.has(raw) ? raw : "lanes";
  } catch {
    return "lanes";
  }
}

export function writeBoardView(view) {
  try {
    window.localStorage?.setItem(BOARD_VIEW_STORAGE_KEY, view);
  } catch {
    // Persistence is best-effort.
  }
}

// The board sort is shared by both board views, so the choice persists like
// the view toggle does. The stored shape is validated on read; an unset or
// corrupt value falls back to the default { key: "number", dir: "asc" }.
// readBoardSortChoice distinguishes the two: null means no preference was
// stored, and the board then keeps the server's order in the lanes while the
// table falls back to its attention grouping (sortForAttention) until the
// operator picks a sort.
export function readBoardSortChoice() {
  try {
    const raw = window.localStorage?.getItem(BOARD_SORT_STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) || {};
      if (BOARD_SORT_KEYS.has(parsed.key) && BOARD_SORT_DIRS.has(parsed.dir)) {
        return { key: parsed.key, dir: parsed.dir };
      }
    }
  } catch {
    // Fall through to the validated default.
  }
  return null;
}

export function readBoardSort() {
  return readBoardSortChoice() || { key: "number", dir: "asc" };
}

export function writeBoardSort(sort) {
  try {
    window.localStorage?.setItem(BOARD_SORT_STORAGE_KEY, JSON.stringify(sort));
  } catch {
    // Persistence is best-effort.
  }
}

export function readDiagramMode() {
  try {
    const raw = window.sessionStorage?.getItem(DIAGRAM_MODE_STORAGE_KEY);
    return DIAGRAM_MODES.has(raw) ? raw : "run";
  } catch {
    return "run";
  }
}

export function writeDiagramMode(mode) {
  try {
    window.sessionStorage?.setItem(DIAGRAM_MODE_STORAGE_KEY, mode);
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

// The Tasks view's chips, in-view project filter and search text are working
// preferences too, so they persist the same way the Done outcome does. The
// state chips combine, so the selection is a set of lifecycle states stored as
// a JSON array; the legacy single values ("all" or one state key) still load.
export function readTasksState() {
  const all = new Set(TASKS_STATES);
  try {
    const raw = window.localStorage?.getItem(TASKS_STATE_STORAGE_KEY);
    if (!raw) return all;
    if (raw === TASKS_ALL_STATE) return all;
    if (TASKS_STATES.has(raw)) return new Set([raw]);
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) return new Set(parsed.filter((key) => TASKS_STATES.has(key)));
    return all;
  } catch {
    return all;
  }
}

export function writeTasksState(state) {
  try {
    const keys = [...(state || [])].filter((key) => TASKS_STATES.has(key));
    window.localStorage?.setItem(TASKS_STATE_STORAGE_KEY, JSON.stringify(keys));
  } catch {
    // Persistence is best-effort.
  }
}

export function readTasksListView() {
  try {
    const raw = window.localStorage?.getItem(TASKS_LIST_VIEW_STORAGE_KEY);
    return TASKS_LIST_VIEWS.has(raw) ? raw : "flat";
  } catch {
    return "flat";
  }
}

export function writeTasksListView(view) {
  try {
    if (TASKS_LIST_VIEWS.has(view)) window.localStorage?.setItem(TASKS_LIST_VIEW_STORAGE_KEY, view);
  } catch {
    // Persistence is best-effort.
  }
}

export function readTasksProject() {
  try {
    return String(window.localStorage?.getItem(TASKS_PROJECT_STORAGE_KEY) || "");
  } catch {
    return "";
  }
}

export function writeTasksProject(project) {
  try {
    const id = String(project || "").trim();
    if (id) {
      window.localStorage?.setItem(TASKS_PROJECT_STORAGE_KEY, id);
    } else {
      window.localStorage?.removeItem(TASKS_PROJECT_STORAGE_KEY);
    }
  } catch {
    // Persistence is best-effort.
  }
}

export function readTasksQuery() {
  try {
    return String(window.localStorage?.getItem(TASKS_QUERY_STORAGE_KEY) || "");
  } catch {
    return "";
  }
}

export function writeTasksQuery(query) {
  try {
    const text = String(query || "").trim();
    if (text) {
      window.localStorage?.setItem(TASKS_QUERY_STORAGE_KEY, text);
    } else {
      window.localStorage?.removeItem(TASKS_QUERY_STORAGE_KEY);
    }
  } catch {
    // Persistence is best-effort.
  }
}

export function readWorkProject() {
  try { return String(window.localStorage?.getItem(WORK_PROJECT_STORAGE_KEY) || "").trim(); } catch { return ""; }
}

export function writeWorkProject(projectID) {
  try {
    const id = String(projectID || "").trim();
    if (id) window.localStorage?.setItem(WORK_PROJECT_STORAGE_KEY, id);
    else window.localStorage?.removeItem(WORK_PROJECT_STORAGE_KEY);
  } catch { /* Persistence is best-effort. */ }
}

export function defaultWorkPreferences() {
  return { view: "overview", filter: "all", completedCollapsed: true, collapsed: new Set() };
}

function validPreferenceMap(candidate) {
  return candidate && typeof candidate === "object" && !Array.isArray(candidate) ? candidate : {};
}

export function readWorkPreferences(projectID) {
  const fallback = defaultWorkPreferences();
  const id = String(projectID || "").trim();
  if (!id) return fallback;
  try {
    const all = validPreferenceMap(JSON.parse(window.localStorage?.getItem(WORK_PREFERENCES_STORAGE_KEY) || "{}"));
    const raw = validPreferenceMap(Object.prototype.hasOwnProperty.call(all, id) ? all[id] : {});
    return {
      view: WORK_ITEM_VIEWS.has(raw.view) ? raw.view : fallback.view,
      filter: WORK_ITEM_FILTERS.has(raw.filter) ? raw.filter : fallback.filter,
      completedCollapsed: typeof raw.completedCollapsed === "boolean" ? raw.completedCollapsed : fallback.completedCollapsed,
      collapsed: new Set(Array.isArray(raw.collapsed) ? raw.collapsed.filter((entry) => typeof entry === "string" && entry.trim()).map((entry) => entry.trim()) : []),
    };
  } catch { return fallback; }
}

export function writeWorkPreferences(projectID, preferences) {
  const id = String(projectID || "").trim();
  if (!id) return;
  try {
    const parsed = JSON.parse(window.localStorage?.getItem(WORK_PREFERENCES_STORAGE_KEY) || "{}");
    const all = validPreferenceMap(parsed);
    all[id] = {
      view: WORK_ITEM_VIEWS.has(preferences?.view) ? preferences.view : "overview",
      filter: WORK_ITEM_FILTERS.has(preferences?.filter) ? preferences.filter : "all",
      completedCollapsed: typeof preferences?.completedCollapsed === "boolean" ? preferences.completedCollapsed : true,
      collapsed: [...new Set([...(preferences?.collapsed || [])].filter((entry) => typeof entry === "string" && entry.trim()).map((entry) => entry.trim()))],
    };
    window.localStorage?.setItem(WORK_PREFERENCES_STORAGE_KEY, JSON.stringify(all));
  } catch { /* Persistence is best-effort. */ }
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
  if (path === "/ui" || path === "/ui/" || path === "/ui/board") {
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
