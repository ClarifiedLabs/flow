// Sidebar navigation rendering (links, live status badges and counts) and the
// theme-switcher icon/option assets.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export function renderNavLink(href, label, status) {
  return `<a href="${href}"><span class="nav-label">${escapeHTML(label)}</span>${renderNavStatus(href, status)}</a>`;
}

export function renderNavStatus(href, status) {
  if (!status) return "";
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
    return `
      <span class="nav-status nav-status-jobs" title="${escapeAttr(`${active} active, ${queued} queued`)}">
        <span class="nav-job-status" data-job-status="active">${active}</span>
        <span class="nav-job-status" data-job-status="queued">${queued}</span>
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
