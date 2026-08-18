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
  const followUpSets = registry.follow_up_sets || [];
  // A task with zero changes has an empty registry; that is the same quiet
  // state as a task whose findings all resolved — not an error.
  if (!findings.length && !followUps.length && !followUpSets.length) return `<p class="empty">No review findings recorded</p>`;
  return `
    <p class="summary">${renderSummary(registry.summary || {})}</p>
    ${findings.length ? `<div class="rows">${findings.map(renderFindingRow).join("")}</div>` : ""}
    ${followUpSets.length ? renderFollowUpSets(followUpSets, model) : ""}
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

function renderFollowUpSets(sets, model) {
  const projectID = model?.projectID || "";
  return `
    <section class="follow-ups organized">
      <h4>Review follow-up sets</h4>
      <div class="set-list">${sets.map((set) => renderFollowUpSet(set, projectID)).join("")}</div>
    </section>
  `;
}

function renderFollowUpSet(set, projectID) {
  const id = value(set, "id", "ID") || "";
  const revision = Number(value(set, "revision", "Revision") || 0);
  const state = String(value(set, "state", "State") || "open");
  const organizerTaskID = value(set, "organizer_task_id", "OrganizerTaskID") || "";
  const organizerTitle = value(set, "organizer_task_title", "OrganizerTaskTitle") || organizerTaskID;
  const artifactID = value(set, "active_plan_artifact_id", "ActivePlanArtifactID") || "";
  const error = value(set, "last_error", "LastError") || value(set?.plan, "materialization_error", "MaterializationError") || "";
  const plan = set?.plan;
  const planState = value(plan, "state", "State") || "";
  const batches = set?.batches || [];
  return `
    <article class="follow-set" data-follow-up-set="${escapeAttr(id)}" data-state="${escapeAttr(state)}">
      <div class="head">
        <span class="badge" data-tone="${escapeAttr(followUpStateTone(state))}">${escapeHTML(state.replaceAll("_", " "))}</span>
        <strong>${escapeHTML(id)}</strong>
        <span class="meta">revision ${revision}${planState ? ` · plan ${escapeHTML(planState.replaceAll("_", " "))}` : ""}</span>
      </div>
      <div class="set-links">
        ${organizerTaskID ? `organizer <a href="${escapeAttr(taskHref(projectID, organizerTaskID))}" data-link>${escapeHTML(organizerTitle)}</a> <span class="id">${escapeHTML(organizerTaskID)}</span>` : "organizer not started"}
        ${artifactID ? `<span>plan artifact <span class="id">${escapeHTML(artifactID)}</span></span>` : ""}
      </div>
      ${error ? `<p class="set-error">${escapeHTML(error)}</p>` : ""}
      <div class="batch-list">${batches.map((batch) => renderFollowUpBatch(batch, set, projectID)).join("")}</div>
    </article>
  `;
}

function followUpStateTone(state) {
  if (state === "materialized" || state === "closed") return "ok";
  if (state === "attention") return "warn";
  return "idle";
}

function renderFollowUpBatch(batch, set, projectID) {
  const id = value(batch, "id", "ID") || "";
  const check = value(batch, "check_name", "CheckName") || "";
  const job = value(batch, "source_job_id", "SourceJobID") || "";
  const head = value(batch, "reviewed_head_sha", "ReviewedHeadSHA") || "";
  const proposals = batch?.proposals || [];
  return `
    <section class="follow-batch" data-follow-up-batch="${escapeAttr(id)}">
      <div class="batch-head"><strong>${escapeHTML(id)}</strong><span>${escapeHTML(check)} · job ${escapeHTML(job)} · head ${escapeHTML(shortID(head))}</span></div>
      ${proposals.map((proposal) => renderProposal(proposal, set, projectID)).join("")}
    </section>
  `;
}

function renderProposal(proposal, set, projectID) {
  const id = value(proposal, "id", "ID") || "";
  const severity = value(proposal, "severity", "Severity") || "";
  const file = value(proposal, "file_path", "FilePath") || "";
  const line = Number(value(proposal, "line", "Line") || 0);
  const changeID = value(set, "source_change_id", "SourceChangeID") || "";
  const location = file ? `${file}${line ? `:${line}` : ""}` : "";
  const anchor = location && changeID
    ? `<a class="anchor" href="${escapeAttr(`/ui/changes/${encodeURIComponent(changeID)}`)}" data-link>${escapeHTML(location)}</a>`
    : `<span class="anchor">${escapeHTML(location)}</span>`;
  return `
    <article class="proposal" data-proposal="${escapeAttr(id)}">
      <div class="head"><span class="badge">${escapeHTML(severity)}</span>${anchor}<span class="meta">${escapeHTML(id)}</span></div>
      <div class="body">${renderMarkdown(value(proposal, "body", "Body") || "")}</div>
      ${renderSuggestion(proposal, projectID)}
      ${renderDisposition(proposal?.disposition, projectID)}
    </article>
  `;
}

function renderSuggestion(proposal, projectID) {
  const action = value(proposal, "suggested_action", "SuggestedAction") || "";
  const title = value(proposal, "suggested_title", "SuggestedTitle") || "";
  const body = value(proposal, "suggested_body", "SuggestedBody") || "";
  const taskID = value(proposal, "suggested_task_id", "SuggestedTaskID") || "";
  if (!action) return "";
  const label = action === "create_task" ? "Suggested new task" : action === "use_existing_task" ? "Suggested existing task" : `Suggested ${action}`;
  const target = taskID
    ? `<a href="${escapeAttr(taskHref(projectID, taskID))}" data-link>${escapeHTML(title || taskID)}</a> <span class="id">${escapeHTML(taskID)}</span>`
    : escapeHTML(title);
  return `
    <div class="suggestion" data-suggested-action="${escapeAttr(action)}">
      <strong>${escapeHTML(label)}</strong>${target ? ` · ${target}` : ""}
      ${body ? `<div>${renderMarkdown(body)}</div>` : ""}
    </div>
  `;
}

const DISPOSITION_LABELS = {
  create_task: "created task",
  use_existing_task: "reused existing task",
  merge_with_proposal: "merged with proposal",
  covered_by_source: "covered by source task",
  discard_duplicate: "discarded duplicate",
};

function renderDisposition(disposition, projectID) {
  if (!disposition) return `<p class="disposition pending">Awaiting organizer disposition</p>`;
  const kind = String(value(disposition, "disposition", "Disposition") || "");
  const target = value(disposition, "target_task_id", "TargetTaskID") || "";
  const title = value(disposition, "target_task_title", "TargetTaskTitle") || target;
  const canonical = value(disposition, "canonical_proposal_id", "CanonicalProposal") || "";
  const rationale = value(disposition, "rationale", "Rationale") || "";
  const feature = value(disposition, "target_feature_id", "TargetFeatureID") || "";
  const parent = value(disposition, "target_parent_id", "TargetParentID") || "";
  const blockers = value(disposition, "target_blocker_ids", "TargetBlockerIDs") || [];
  const targetLink = target
    ? `<a href="${escapeAttr(taskHref(projectID, target))}" data-link>${escapeHTML(title)}</a> <span class="id">${escapeHTML(target)}</span>`
    : "";
  const graph = [feature && `feature ${feature}`, parent && `parent ${parent}`, blockers.length && `blocked by ${blockers.join(", ")}`].filter(Boolean).join(" · ");
  return `
    <div class="disposition" data-disposition="${escapeAttr(kind)}">
      <div><strong>${escapeHTML(DISPOSITION_LABELS[kind] || kind)}</strong>${targetLink ? ` · ${targetLink}` : ""}${canonical ? ` · ${escapeHTML(canonical)}` : ""}</div>
      ${rationale ? `<p>${escapeHTML(rationale)}</p>` : ""}
      ${graph ? `<p class="meta">${escapeHTML(graph)}</p>` : ""}
    </div>
  `;
}

function shortID(id) {
  const text = String(id || "");
  return text.length > 12 ? text.slice(0, 12) : text;
}

function renderFollowUps(followUps, model) {
  const projectID = model?.projectID || "";
  return `
    <section class="follow-ups">
      <h4>Legacy follow-ups</h4>
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
  const meta = ["legacy", "deferred", check].filter(Boolean).join(" · ");
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
