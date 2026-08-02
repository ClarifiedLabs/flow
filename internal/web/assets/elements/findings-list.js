// The Findings tab: the per-task review findings registry, rendered from the
// read model the task route fetches alongside the workflow (the
// /v2/tasks/{id}/findings response). One row per review thread — state badge,
// file:line anchor into the change, the finding body as block markdown, and
// its resolution — plus the resolution-bucket summary line and a follow-ups
// section for findings that became separate tasks.
//
// Like check-list.js this is a plain render shell: the registry travels inside
// the task model, so a poll repaint paints the same registry through the same
// element instance (task-detail mounts by tag, reusing the element), and the
// base class's identical-markup skip leaves the DOM — and the reader's scroll
// position — untouched. Nothing here expands asynchronously, so there is no
// expanded state a poll could collapse.

import { taskHref } from "../api.js";
import { escapeAttr, escapeHTML } from "../html.js";
import { renderMarkdown } from "../markdown.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

// SUMMARY_BUCKETS is the summary line's order and label for each resolution
// bucket in the registry summary, matching coordinator.TaskFindingsSummary.
export const SUMMARY_BUCKETS = [
  ["resolved_fixed", "fixed"],
  ["resolved_not_warranted", "not warranted"],
  ["resolved_superseded", "superseded"],
  ["certified", "certified"],
  ["unresolved", "unresolved"],
  ["deferred_to_task", "deferred"],
];

export function renderFindings(model) {
  const registry = model?.findings;
  if (!registry) return `<p class="empty">No review findings recorded</p>`;
  if (registry.error) return `<p class="empty">Findings unavailable</p>`;
  const findings = registry.findings || [];
  const followUps = registry.follow_ups || [];
  // A task with zero changes has an empty registry; that is the same quiet
  // state as a task whose findings all resolved — not an error.
  if (!findings.length && !followUps.length) return `<p class="empty">No review findings recorded</p>`;
  return `
    <p class="summary">${renderSummary(registry.summary || {})}</p>
    <div class="rows">${findings.map(renderFindingRow).join("")}</div>
    ${followUps.length ? renderFollowUps(followUps, model) : ""}
  `;
}

function renderSummary(summary) {
  return SUMMARY_BUCKETS.map(([key, label]) => {
    const count = Number(value(summary, key) || 0);
    return `<span data-bucket="${escapeAttr(key)}">${escapeHTML(label)} ${count}</span>`;
  }).join("");
}

function renderFindingRow(finding) {
  const id = value(finding, "id", "ID") || "";
  const state = String(value(finding, "state", "State") || "open");
  const resolution = resolutionOf(finding);
  return `
    <div class="finding" data-finding="${escapeAttr(id)}" data-state="${escapeAttr(state)}">
      <div class="head">
        <span class="badge" data-tone="${escapeAttr(stateTone(state))}">${escapeHTML(state)}</span>
        ${renderAnchor(finding)}
        <span class="spacer"></span>
        <span class="resolution" data-tone="${escapeAttr(resolution.tone)}">${escapeHTML(resolution.text)}</span>
      </div>
      <div class="body">${renderMarkdown(value(finding, "finding", "Finding") || "")}</div>
    </div>
  `;
}

// stateTone colours the state badge by what the state means to the reader:
// open and reopened threads still need attention, a claimed thread is parked
// on the author, a certified one is verified.
function stateTone(state) {
  if (state === "certified") return "ok";
  if (state === "claimed") return "idle";
  return "warn";
}

// CLAIM_LABELS names the claim kinds the same way the CLI does.
const CLAIM_LABELS = { fixed: "fixed", not_warranted: "not warranted", superseded: "superseded" };

function resolutionOf(finding) {
  const state = String(value(finding, "state", "State") || "open");
  if (state === "certified") {
    const by = value(finding, "certified_by", "CertifiedBy");
    return { text: by ? `certified by ${by}` : "certified", tone: "ok" };
  }
  if (state === "claimed") {
    const kind = String(value(finding, "claim_kind", "ClaimKind") || "");
    const label = CLAIM_LABELS[kind] || "claimed";
    const by = value(finding, "claimed_by", "ClaimedBy");
    return { text: by ? `${label} by ${by}` : label, tone: kind === "fixed" ? "ok" : "idle" };
  }
  if (state === "reopened") return { text: "unresolved · reopened", tone: "warn" };
  return { text: "unresolved", tone: "warn" };
}

// renderAnchor links a finding to its change when the registry carries a file
// (and line). The anchor is the change route; there is no per-line deep link.
function renderAnchor(finding) {
  const file = value(finding, "file_path", "FilePath") || "";
  if (!file) return "";
  const line = Number(value(finding, "line", "Line")) || 0;
  const label = line > 0 ? `${file}:${line}` : file;
  const changeID = value(finding, "change_id", "ChangeID") || "";
  if (!changeID) return `<span class="anchor">${escapeHTML(label)}</span>`;
  return `<a class="anchor" href="${escapeAttr(`/ui/changes/${encodeURIComponent(changeID)}`)}" data-link>${escapeHTML(label)}</a>`;
}

function renderFollowUps(followUps, model) {
  const projectID = model?.projectID || "";
  return `
    <section class="follow-ups">
      <h4>Follow-ups</h4>
      <ul class="follow-list">
        ${followUps.map((followUp) => renderFollowUp(followUp, projectID)).join("")}
      </ul>
    </section>
  `;
}

function renderFollowUp(followUp, projectID) {
  const target = value(followUp, "target_task_id", "TargetTaskID") || "";
  const title = value(followUp, "target_task_title", "TargetTaskTitle") || target;
  const check = value(followUp, "check_name", "CheckName") || "";
  const meta = ["deferred", check].filter(Boolean).join(" · ");
  const link = target
    ? `<a class="follow-link" href="${escapeAttr(taskHref(projectID, target))}" data-link>
        <span class="title">${escapeHTML(title)}</span>
        <span class="id">${escapeHTML(target)}</span>
      </a>`
    : `<span class="follow-link">${escapeHTML(title)}</span>`;
  return `
    <li class="follow-up">
      <span class="defer">deferred to</span>
      ${link}
      <span class="meta">${escapeHTML(meta)}</span>
    </li>
  `;
}

export class FlowFindings extends FlowElement {
  render(model) {
    return renderFindings(model);
  }
}

define("flow-findings-list", FlowFindings);
