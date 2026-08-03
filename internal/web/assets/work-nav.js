// Shared local navigation for the planning surfaces. Existing route names stay
// canonical; this only consolidates their presentation under Work.

import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";
import { readWorkProject } from "./storage.js";

export const WORK_TABS = [
  ["overview", "/ui/work-items", "Overview"],
  ["tasks", "/ui/tasks", "Tasks"],
  ["branches", "/ui/features", "Branches"],
];

export function activeWorkProject(app, explicit = "") {
  const projects = app?.projects || [];
  const ids = new Set(projects.map((project) => String(value(project, "id", "ID") || "")));
  const requested = String(explicit || "").trim();
  if (requested && ids.has(requested)) return requested;
  const selected = typeof app?.selectedProjectIDs === "function" ? app.selectedProjectIDs().filter((id) => ids.has(id)) : [];
  const stored = readWorkProject();
  if (stored && ids.has(stored) && (!selected.length || selected.includes(stored))) return stored;
  if (selected.length) return selected[0];
  return projects.length ? String(value(projects[0], "id", "ID") || "") : "";
}

export function isWorkPath(path) {
  return /^\/ui\/(?:work-items|tasks(?:\/|$)|features(?:\/|$)|epics(?:\/|$))/.test(path)
    || /^\/ui\/projects\/[^/]+\/(?:work-items|tasks(?:\/|$)|features(?:\/|$)|epics(?:\/|$))/.test(path);
}

export function projectWorkHref(projectID) {
  const id = String(projectID || "").trim();
  return id ? `/ui/projects/${encodeURIComponent(id)}/work-items` : "/ui/work-items";
}

// Return targets are navigation, not arbitrary redirects. Normalize a valid
// same-origin /ui target back to a path and reject protocols, protocol-relative
// URLs, and non-UI paths. Rejection always lands on the active project's Work
// overview, so a stale bookmark cannot send the operator away from Flow.
export function safeWorkReturnTarget(target, projectID, origin = "") {
  const fallback = projectWorkHref(projectID);
  const raw = String(target || "").trim();
  if (!raw) return fallback;
  const baseOrigin = String(origin || globalThis.window?.location?.origin || "http://flow.local");
  try {
    const url = new URL(raw, baseOrigin);
    if (url.origin !== new URL(baseOrigin).origin) return fallback;
    if (url.pathname !== "/ui" && !url.pathname.startsWith("/ui/")) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}

export function workNavigationState(search, projectID, origin = "") {
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  return {
    context: String(params.get("context") || "").trim(),
    returnTo: safeWorkReturnTarget(params.get("return"), projectID, origin),
  };
}

export function currentWorkReturnTarget(location = globalThis.window?.location, projectID = "") {
  const target = `${location?.pathname || ""}${location?.search || ""}${location?.hash || ""}`;
  return safeWorkReturnTarget(target, projectID, location?.origin || "");
}

export function resolveWorkNavigation(navigation, validContextIDs, projectID) {
  const requested = String(navigation?.context || "").trim();
  const valid = new Set([...(validContextIDs || [])].map((id) => String(id || "")));
  if (requested && !valid.has(requested)) {
    return { context: "", returnTo: projectWorkHref(projectID), contextValid: false };
  }
  return {
    context: requested,
    returnTo: safeWorkReturnTarget(navigation?.returnTo, projectID),
    contextValid: true,
  };
}

export function workViewHref(view, projectID, search = "") {
  const id = String(projectID || "").trim();
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  if (view === "tasks") {
    if (id) params.set("project", id);
    else params.delete("project");
    const query = params.toString();
    return `/ui/tasks${query ? `?${query}` : ""}`;
  }
  params.delete("project");
  const path = view === "branches"
    ? (id ? `/ui/projects/${encodeURIComponent(id)}/features` : "/ui/features")
    : (id ? `/ui/projects/${encodeURIComponent(id)}/work-items` : "/ui/work-items");
  const query = params.toString();
  return `${path}${query ? `?${query}` : ""}`;
}

export function renderWorkNav({ active, projects = [], projectID, search = "", crumbs = [] }) {
  const project = projects.find((candidate) => String(value(candidate, "id", "ID")) === String(projectID));
  const name = value(project || {}, "name", "Name") || projectID;
  return `<header class="work-header">
    <div class="work-heading">
      <div><span class="work-eyebrow">Active Work project</span><h2>${escapeHTML(name || "Work")}</h2></div>
      ${projects.length ? `<label class="work-project"><span>Project</span><select data-work-project data-work-view="${escapeAttr(active)}" aria-label="Active Work project">${projects.map((candidate) => {
        const id = String(value(candidate, "id", "ID") || "");
        const label = value(candidate, "name", "Name") || id;
        return `<option value="${escapeAttr(id)}"${id === projectID ? " selected" : ""}>${escapeHTML(label)}</option>`;
      }).join("")}</select></label>` : ""}
    </div>
    <nav class="work-subnav" aria-label="Work views">${WORK_TABS.map(([key, , label]) => `<a href="${escapeAttr(workViewHref(key, projectID, search))}" data-link${key === active ? ' aria-current="page"' : ""}>${label}</a>`).join("")}</nav>
    ${crumbs.length ? `<nav class="work-breadcrumbs" aria-label="Breadcrumb">${crumbs.map((crumb, index) => index === crumbs.length - 1 ? `<span aria-current="page">${escapeHTML(crumb.label)}</span>` : `<a href="${escapeAttr(crumb.href)}" data-link>${escapeHTML(crumb.label)}</a>`).join('<span aria-hidden="true">/</span>')}</nav>` : ""}
  </header>`;
}
