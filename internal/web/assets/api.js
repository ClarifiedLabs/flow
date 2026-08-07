// HTTP client for the JSON API (CSRF-aware fetch wrappers) plus URL/path
// builders and the CSRF cookie reader.

import { API_PREFIX } from "./config.js";
import { value } from "./normalize.js";

export async function apiGet(path) {
  return apiFetch(path, {
    method: "GET",
    headers: { "X-Flow-CSRF": readCookie("flow_ui_csrf") },
  });
}

export async function apiGetText(path) {
  const response = await fetch(`${API_PREFIX}${path}`, {
    method: "GET",
    credentials: "include",
    headers: { "X-Flow-CSRF": readCookie("flow_ui_csrf") },
  });
  if (!response.ok) {
    let message = `Request failed: ${response.status}`;
    try {
      const body = await response.json();
      message = body.error?.message || body.Error?.Message || message;
    } catch {
      message = (await response.text()) || message;
    }
    throw new Error(message);
  }
  return response.text();
}

export async function apiPost(path, body, { idempotencyKey = "" } = {}) {
  const key = String(idempotencyKey || "").trim();
  return apiFetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Flow-CSRF": readCookie("flow_ui_csrf"),
      ...(key ? { "Idempotency-Key": key } : {}),
    },
    body: JSON.stringify(body),
  });
}

export async function apiPostForm(path, body) {
  return apiFetch(path, {
    method: "POST",
    headers: { "X-Flow-CSRF": readCookie("flow_ui_csrf") },
    body,
  });
}

export async function apiPatch(path, body) {
  return apiFetch(path, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", "X-Flow-CSRF": readCookie("flow_ui_csrf") },
    body: JSON.stringify(body),
  });
}

export async function apiDelete(path, body) {
  return apiFetch(path, {
    method: "DELETE",
    headers: body === undefined
      ? { "X-Flow-CSRF": readCookie("flow_ui_csrf") }
      : { "Content-Type": "application/json", "X-Flow-CSRF": readCookie("flow_ui_csrf") },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

export function consoleAPIPath(projectID) {
  const id = String(projectID || "").trim();
  return id ? `/v2/projects/${encodeURIComponent(id)}/console` : "/v2/console";
}

export function taskConsoleAPIPath(projectID, taskID) {
  const base = taskAPIBase(projectID);
  return `${base}/${encodeURIComponent(taskID)}/console`;
}

export function consoleState(job, session) {
  const sessionState = value(session, "runtime_state", "RuntimeState") || value(session, "state", "State");
  if (sessionState) return sessionState;
  return value(job, "state", "State") || "starting";
}

export async function apiFetch(path, options) {
  const response = await fetch(`${API_PREFIX}${path}`, { credentials: "include", ...options });
  if (!response.ok) {
    let message = `Request failed: ${response.status}`;
    let details = null;
    try {
      details = await response.json();
      message = details.error?.message || details.Error?.Message || message;
    } catch {
      const text = await response.text();
      message = text || message;
    }
    const error = new Error(message);
    const body = details?.error || details?.Error || {};
    error.code = body.code || body.Code || "";
    error.issues = body.issues || body.Issues || [];
    throw error;
  }
  // Some mutating endpoints (task relation add/remove) answer 204 with no body;
  // parsing that as JSON would throw after the request already succeeded.
  if (response.status === 204) return null;
  return response.json();
}

// taskAPIBase scopes project-owned collection/action calls when a project is
// known. Canonical task IDs can also use the globally resolvable /v2/tasks
// route directly.
export function taskAPIBase(projectID) {
  const id = String(projectID || "").trim();
  return id ? `/v2/projects/${encodeURIComponent(id)}/tasks` : "/v2/tasks";
}

// taskRelationsAPIPath builds the per-task relations endpoint used to link and
// unlink relations after a task exists.
export function taskRelationsAPIPath(projectID, taskID) {
  return `${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}/relations`;
}

export function lifecycleTransitionPath(projectID, taskID) {
  return `${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}/lifecycle/transition`;
}

// flowsAPIBase / agentDefsAPIBase scope the flow-configuration endpoints to a
// project, mirroring taskAPIBase: an explicit project id yields the
// project-scoped path, an empty id falls back to the unscoped route that only
// works for single-project/implicit principals.
export function flowsAPIBase(projectID) {
  const id = String(projectID || "").trim();
  return id ? `/v2/projects/${encodeURIComponent(id)}/flows` : "/v2/flows";
}

export function agentDefsAPIBase(projectID) {
  const id = String(projectID || "").trim();
  return id ? `/v2/projects/${encodeURIComponent(id)}/agent-defs` : "/v2/agent-defs";
}

export function globalAgentDefsAPIBase() {
  return "/v2/global/agent-defs";
}

export function withWorkNavigation(href, navigation = {}) {
  if (!href || href === "#") return href || "#";
  const source = navigation && typeof navigation === "object" ? navigation : {};
  const contextValue = source.context || source.contextID || "";
  const returnValue = source.returnTo || source.return || "";
  // Navigation state is URL text. Refuse objects instead of stringifying them
  // into a visible and unusable ?context=[object+Object] deep link.
  const context = typeof contextValue === "string" ? contextValue.trim() : "";
  const returnTo = typeof returnValue === "string" ? returnValue.trim() : "";
  if (!context && !returnTo) return href;
  const [path, hash = ""] = String(href).split("#", 2);
  const [pathname, search = ""] = path.split("?", 2);
  const params = new URLSearchParams(search);
  if (context) params.set("context", context);
  if (returnTo) params.set("return", returnTo);
  const query = params.toString();
  return `${pathname}${query ? `?${query}` : ""}${hash ? `#${hash}` : ""}`;
}

export function taskHref(projectID, taskID, navigation = {}) {
  const task = String(taskID || "").trim();
  return task ? withWorkNavigation(`/ui/tasks/${encodeURIComponent(task)}`, navigation) : "#";
}

// projectTaskHref is deliberately separate from taskHref: taskHref is a legacy,
// globally-resolvable deep link used throughout the execution UI, while planning
// surfaces must retain the active project when task IDs are ambiguous.
export function projectTaskHref(projectID, taskID, navigation = {}) {
  const task = String(taskID || "").trim();
  const project = String(projectID || "").trim();
  if (!task) return "#";
  return project
    ? withWorkNavigation(`/ui/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(task)}`, navigation)
    : taskHref("", task, navigation);
}

// featuresAPIBase is the project-scoped features endpoint; features never
// leave their project, so the id is required.
export function featuresAPIBase(projectID) {
  const id = String(projectID || "").trim();
  if (!id) throw new Error("project id is required");
  return `/v2/projects/${encodeURIComponent(id)}/features`;
}

export function featureHref(projectID, ref, navigation = {}) {
  const feature = String(ref || "").trim();
  const id = String(projectID || "").trim();
  if (!feature) return "#";
  return withWorkNavigation(id
    ? `/ui/projects/${encodeURIComponent(id)}/features/${encodeURIComponent(feature)}`
    : `/ui/features/${encodeURIComponent(feature)}`, navigation);
}

export function epicsAPIBase(projectID) {
  const id = String(projectID || "").trim();
  if (!id) throw new Error("project id is required");
  return `/v2/projects/${encodeURIComponent(id)}/epics`;
}

export function workItemsAPIBase(projectID) {
  const id = String(projectID || "").trim();
  if (!id) throw new Error("project id is required");
  return `/v2/projects/${encodeURIComponent(id)}/work-items`;
}

export function workItemAPIPath(projectID, itemID, suffix = "") {
  return `${workItemsAPIBase(projectID)}/${encodeURIComponent(itemID)}${suffix}`;
}

export function workItemParentsAPIPath(projectID) {
  return `${workItemsAPIBase(projectID)}/parents`;
}

export function workItemsOverviewAPIPath(projectID) {
  return `${workItemsAPIBase(projectID)}/overview?terminal=false`;
}

export function workItemContextAPIPath(projectID, itemID) {
  return workItemAPIPath(projectID, itemID, "/context");
}

export function workItemHref(projectID, item, navigation = {}) {
  const id = value(item || {}, "id", "ID");
  switch (value(item || {}, "kind", "Kind")) {
    case "epic": return epicHref(projectID, id, navigation);
    case "feature": return featureHref(projectID, id, navigation);
    default: return projectTaskHref(projectID, id, navigation);
  }
}

export function epicHref(projectID, ref, navigation = {}) {
  const epic = String(ref || "").trim();
  const id = String(projectID || "").trim();
  if (!epic) return "#";
  return withWorkNavigation(id
    ? `/ui/projects/${encodeURIComponent(id)}/epics/${encodeURIComponent(epic)}`
    : `/ui/epics/${encodeURIComponent(epic)}`, navigation);
}

export function attachmentHref(projectID, taskID, attachmentID) {
  return `${API_PREFIX}${taskAPIBase(projectID)}/${encodeURIComponent(taskID)}/attachments/${encodeURIComponent(attachmentID)}`;
}

export function isImageContentType(contentType) {
  return ["image/avif", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp"].includes(
    String(contentType || "").toLowerCase().split(";")[0].trim()
  );
}

export function readCookie(name) {
  return document.cookie.split(";").map((part) => part.trim()).find((part) => part.startsWith(`${name}=`))?.slice(name.length + 1) || "";
}
