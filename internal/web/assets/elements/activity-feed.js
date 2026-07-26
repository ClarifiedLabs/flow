// The unified timeline, now behind a tab instead of dominating the page. One
// row per event: what kind, what happened in prose, and how long ago.

import { formatRelative } from "../format.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

// TONE_BY_KIND grades an event by whether it is good news, a blocker, or just
// the machine moving.
const TONE_BY_KIND = {
  verdict: "ok",
  check: "ok",
  job: "accent",
  transition: "accent",
  blocker: "danger",
  progress: "muted",
};

// activityEntries merges the workflow transition log and the status log into
// one feed, newest first.
export function activityEntries(model) {
  const entries = [];
  for (const transition of model?.transitions || []) {
    const eventKind = String(value(transition, "event_kind", "EventKind") || "");
    entries.push({
      kind: activityKind(eventKind),
      text: transitionText(transition, eventKind),
      time: value(transition, "created_at", "CreatedAt"),
    });
  }
  for (const status of model?.statusLog || []) {
    const kind = String(value(status, "kind", "Kind") || "note");
    entries.push({
      kind: kind === "blocker" ? "blocker" : kind === "question" ? "blocker" : "progress",
      text: value(status, "message", "Message"),
      time: value(status, "created_at", "CreatedAt"),
    });
  }
  return entries
    .filter((entry) => entry.text)
    .sort((left, right) => (Date.parse(right.time || "") || 0) - (Date.parse(left.time || "") || 0));
}

function activityKind(eventKind) {
  if (eventKind.includes("hold")) return "blocker";
  if (eventKind.includes("check")) return "check";
  if (eventKind.includes("job")) return "job";
  if (eventKind.includes("failed") || eventKind.includes("error")) return "blocker";
  return "transition";
}

function transitionText(transition, eventKind) {
  const from = value(transition, "from_node_key", "FromNodeKey");
  const to = value(transition, "to_node_key", "ToNodeKey");
  const outcome = value(transition, "outcome", "Outcome");
  if (from && to && from !== to) {
    return `${from} → ${to}${outcome ? ` · ${outcome}` : ""}`;
  }
  return eventKind.replaceAll("_", " ");
}

export function renderActivityFeed(model) {
  const entries = activityEntries(model);
  if (!entries.length) return `<p class="empty">No activity yet</p>`;
  return entries
    .map(
      (entry) => `
      <div class="row" data-tone="${escapeAttr(TONE_BY_KIND[entry.kind] || "muted")}">
        <span class="kind">${escapeHTML(entry.kind)}</span>
        <span class="text">${escapeHTML(entry.text)}</span>
        <span class="spacer"></span>
        <time datetime="${escapeAttr(entry.time || "")}">${escapeHTML(formatRelative(entry.time))}</time>
      </div>
    `,
    )
    .join("");
}

export class FlowActivityFeed extends FlowElement {
  render(model) {
    return renderActivityFeed(model);
  }
}

define("flow-activity-feed", FlowActivityFeed);
