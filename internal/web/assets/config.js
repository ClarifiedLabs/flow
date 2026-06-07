// App-wide constants: navigation/lane config, API prefix, poll intervals,
// localStorage keys, enum sets and agent defaults. A dependency-free leaf.
// Feature-specific constants (theme/terminal icons, harness flags, lifecycle
// layout, etc.) live with their owning module, not here.

export const LANES = [
  ["backlog", "Backlog", "Backlog"],
  ["up_next", "Up Next", "UpNext"],
  ["in_progress", "In Progress", "InProgress"],
  ["needs_attention", "Needs Attention", "NeedsAttention"],
];

export const NAV = [
  ["/ui/board", "Board"],
  ["/ui/console", "Console"],
  ["/ui/triage", "Triage"],
  ["/ui/feedback", "Needs Attention"],
  ["/ui/merge", "Merge"],
  ["/ui/done", "Done"],
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

export const THEME_STORAGE_KEY = "flow.ui.theme";

export const PROJECT_STORAGE_KEY = "flow.ui.projects";

export const ISSUE_AGENT_DEFAULTS_STORAGE_KEY = "flow.ui.issueAgentDefaults.v1";

export const DONE_DENSITY_STORAGE_KEY = "flow.ui.doneDensity";

export const DONE_OUTCOME_STORAGE_KEY = "flow.ui.doneOutcome";

export const BOARD_DONE_STORAGE_KEY = "flow.ui.boardDone.v1";

export const DIFF_MODE_STORAGE_KEY = "flow.ui.diffMode";

export const DONE_DENSITIES = new Set(["extended", "compact"]);

export const DIFF_MODES = new Set(["unified", "split"]);

export const DONE_OUTCOMES = new Set(["all", "merged", "rejected", "abandoned"]);

export const THEME_PREFERENCES = new Set(["system", "light", "dark"]);

export const DEFAULT_AGENT_HARNESSES = [];

export const DEFAULT_CONSOLE_HARNESSES = [
  { name: "shell", display_name: "Shell" },
];

export const ISSUE_STATE_OPTIONS = [
  ["triage", "Triage"],
  ["backlog", "Backlog"],
  ["up_next", "Up Next"],
  ["closed", "Closed"],
  ["rejected", "Rejected"],
];

export const BOARD_DONE_COUNTS = [10, 20, 50];

export const BOARD_DONE_WINDOWS = ["1d", "7d", "30d"];
