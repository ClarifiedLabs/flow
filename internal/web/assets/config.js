// App-wide constants: navigation/lane config, API prefix, poll intervals,
// localStorage keys, enum sets and agent defaults. A dependency-free leaf.
// Feature-specific constants (theme/terminal icons, harness flags, lifecycle
// layout, etc.) live with their owning module, not here.

import { LIFECYCLE_SCHEDULED, LIFECYCLE_STATES } from "./lifecycle.js";

// LANES are the board's live columns. Unscheduled work is no longer a board
// lane; it lives in the Tasks view (/ui/tasks). In-progress work is split into
// two lanes — Working (actively executing) and Waiting (idle) — that both read
// the board's InProgress list; boardEntries buckets each task into exactly one
// of them via activityGroupOf. The third element of each triple is the JSON
// field name laneTasks reads off the board payload: coordinator.Board has no
// json tags, so /v2/board emits the Go field names verbatim ("Scheduled",
// "InProgress"). The scheduled lane key is the shared lifecycle state so a new
// server state cannot silently leave the lane vocabulary stale.
export const LANES = [
  [LIFECYCLE_SCHEDULED, "Scheduled", "Scheduled"],
  ["working", "Working", "InProgress"],
  ["waiting", "Waiting", "InProgress"],
];

export const NAV = [
  ["/ui/board", "Board"],
  ["/ui/tasks", "Tasks"],
  ["/ui/features", "Features"],
  ["/ui/console", "Console"],
  ["/ui/done", "Done"],
  ["/ui/flows", "Flows"],
  ["/ui/workers", "Workers"],
  ["/ui/jobs", "Jobs"],
];

export const API_PREFIX = "/ui/api";

export const BOARD_POLL_MS = 10000;

export const CHANGE_POLL_MS = 15000;

export const DIAGNOSTICS_POLL_MS = 30000;

export const SIDEBAR_STATUS_POLL_MS = 10000;

export const CONSOLE_REFRESH_MS = 2000;

export const MAX_POLL_BACKOFF_MS = 120000;

// SETTLE_BURST_DELAYS_MS paces the settle burst: the short, bounded series of
// follow-up reloads of the current route after a successful action-triggered
// refresh. Mutations such as schedule or approve advance the workflow
// synchronously inside the request, but their visible follow-on effects — the
// agent session starting, the next gate opening, checks beginning — complete
// asynchronously seconds later, so the single immediate refresh renders a
// state that is already on its way out. Each entry is an offset from the
// completed action refresh; the burst is bounded to this list.
export const SETTLE_BURST_DELAYS_MS = [1500, 4000];

export const THEME_STORAGE_KEY = "flow.ui.theme";

export const PROJECT_STORAGE_KEY = "flow.ui.projects";

export const DONE_DENSITY_STORAGE_KEY = "flow.ui.doneDensity";

export const DONE_OUTCOME_STORAGE_KEY = "flow.ui.doneOutcome";

export const TASKS_STATE_STORAGE_KEY = "flow.ui.tasksState";

export const TASKS_PROJECT_STORAGE_KEY = "flow.ui.tasksProject";

export const TASKS_QUERY_STORAGE_KEY = "flow.ui.tasksQuery";

export const DIFF_MODE_STORAGE_KEY = "flow.ui.diffMode";

// The board's old Done lane stored its scope under flow.ui.boardDone.v1; the
// key is no longer read and may linger harmlessly in old browsers.
export const BOARD_VIEW_STORAGE_KEY = "flow.ui.boardView";

export const DIAGRAM_MODE_STORAGE_KEY = "flow.ui.diagramMode";

export const DONE_DENSITIES = new Set(["extended", "compact"]);

export const DIFF_MODES = new Set(["unified", "split"]);

export const BOARD_VIEWS = new Set(["lanes", "table"]);

export const DIAGRAM_MODES = new Set(["run", "graph"]);

// TASK_TABS is the task detail work surface. Order is the reading order of the
// page: what needs you, what is happening, then the change, then the evidence,
// then the findings registry, then the log.
export const TASK_TABS = [
  ["review", "Review"],
  ["overview", "Overview"],
  ["change", "Change"],
  ["checks", "Checks"],
  ["findings", "Findings"],
  ["activity", "Activity"],
  ["terminal", "Terminal"],
  ["detail", "Detail"],
];

export const DONE_OUTCOMES = new Set(["all", "completed", "merged", "rejected", "abandoned", "cancelled", "failed"]);

// TASKS_STATES are the selectable lifecycle filters in the Tasks view; they
// combine, and TASKS_ALL_STATE is the convenience chip that selects or clears
// every one of them at once (it is not itself a stored selection). The set is
// the shared lifecycle vocabulary, so a new server state shows up here the
// moment lifecycle.js catches up with the server constants.
export const TASKS_ALL_STATE = "all";
export const TASKS_STATES = new Set(LIFECYCLE_STATES);

export const THEME_PREFERENCES = new Set(["system", "light", "dark"]);

export const DEFAULT_AGENT_HARNESSES = [];

export const DEFAULT_CONSOLE_HARNESSES = [
  { name: "shell", display_name: "Shell" },
];

export const TASK_TAB_KEYS = new Set(TASK_TABS.map(([key]) => key));
