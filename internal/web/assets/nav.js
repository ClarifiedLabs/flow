// Top-bar navigation rendering: the dropdown trigger (current-page label plus
// compact board lane chips), the panel's links with live status badges and
// counts, and the theme-switcher icon/option assets.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { NAV } from "./config.js";

// navTriggerLabel maps a path onto the top-bar trigger's label: the board
// aliases and the task views (new task, task detail, epic, and the
// project-scoped task detail) collapse to "board", other nav destinations use
// their label, and everything else (change detail, terminal) falls back to
// "menu".
export function navTriggerLabel(path) {
  if (path === "/ui" || path === "/ui/" || path === "/ui/board") return "board";
  if (path.startsWith("/ui/tasks/")) return "board";
  if (/^\/ui\/projects\/[^/]+\/tasks\/[^/]+/.test(path)) return "board";
  const match = NAV.find(([href]) => href === path);
  return match ? match[1].toLowerCase() : "menu";
}

// renderNavTrigger paints the dropdown trigger: hamburger, current-page label,
// caret, and (once the first /v2/sidebar poll lands) the compact board lane
// chips. Before the first poll the status is empty and only the label shows.
export function renderNavTrigger(path, status) {
  return [
    `<span class="nav-trigger-icon" aria-hidden="true">\u2630</span>`,
    `<span class="nav-trigger-label">${escapeHTML(navTriggerLabel(path))}</span>`,
    `<span class="nav-trigger-caret" aria-hidden="true">\u25be</span>`,
    `<span class="nav-trigger-status">${renderNavStatus("/ui/board", status)}</span>`,
  ].join("");
}

export function renderNavLink(href, label, status) {
  return `<a href="${href}"><span class="nav-label">${escapeHTML(label)}</span>${renderNavStatus(href, status)}</a>`;
}

export function renderNavStatus(href, status) {
  if (!status) return "";
  if (href === "/ui/board") {
    const board = value(status, "board", "Board") || {};
    const groups = [
      ["queued", [
        ["unscheduled", "Unscheduled", "unscheduled"],
        ["scheduled", "Scheduled", "scheduled"],
      ]],
      ["active", [
        ["in_progress", "InProgress", "in progress"],
        ["blocked", "Blocked", "blocked"],
      ]],
    ];
    const labels = [];
    const badges = groups.map(([group, counts]) => {
      const groupBadges = counts.map(([key, fallback, label]) => {
        const count = Number(value(board, key, fallback) || 0);
        const description = `${count} ${label} task${count === 1 ? "" : "s"}`;
        labels.push(description);
        return `<span class="nav-board-status" data-board-lane="${key}" title="${escapeAttr(description)}">${count}</span>`;
      }).join("");
      return `<span class="nav-board-group" data-board-group="${group}">${groupBadges}</span>`;
    }).join("");
    return `<span class="nav-status nav-status-board" title="${escapeAttr(labels.join(", "))}" aria-label="${escapeAttr(labels.join(", "))}">${badges}</span>`;
  }
  if (href === "/ui/triage") return renderNavCount(value(status, "triage", "Triage"), "triage items");
  if (href === "/ui/feedback") return renderNavCount(value(status, "feedback", "Feedback"), "needs attention items");
  if (href === "/ui/merge") return renderNavCount(value(status, "merge", "Merge"), "merge items");
  if (href === "/ui/done") return renderNavCount(value(status, "done", "Done"), "done items");
  if (href === "/ui/workers") {
    const workers = value(status, "workers", "Workers") || {};
    const inUse = Number(value(workers, "in_use", "InUse") || 0);
    const capacity = Number(value(workers, "capacity", "Capacity") || 0);
    return `<span class="nav-status" title="${escapeAttr(`${inUse} in use of ${capacity} worker slots`)}">${inUse}/${capacity}</span>`;
  }
  if (href === "/ui/jobs") {
    const jobs = value(status, "jobs", "Jobs") || {};
    const active = Number(value(jobs, "active", "Active") || 0);
    const queued = Number(value(jobs, "queued", "Queued") || 0);
    const activeDescription = `${active} active job${active === 1 ? "" : "s"}`;
    const queuedDescription = `${queued} queued job${queued === 1 ? "" : "s"}`;
    const description = `${activeDescription}, ${queuedDescription}`;
    return `
      <span class="nav-status nav-status-jobs" title="${escapeAttr(description)}" aria-label="${escapeAttr(description)}">
        <span class="nav-job-status" data-job-status="active" title="${escapeAttr(activeDescription)}">${active}</span>
        <span class="nav-job-status" data-job-status="queued" title="${escapeAttr(queuedDescription)}">${queued}</span>
      </span>
    `;
  }
  return "";
}

export function renderNavCount(count, label) {
  const number = Number(count || 0);
  return `<span class="nav-status" title="${escapeAttr(`${number} ${label}`)}">${number}</span>`;
}

export const THEME_ICONS = {
  system: `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>`,
  light: `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>`,
  dark: `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>`,
};

export const THEME_OPTIONS = [
  ["system", "System"],
  ["light", "Light"],
  ["dark", "Dark"],
];
