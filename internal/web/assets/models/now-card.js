// Now card projection and tab badges for the task page.

import { formatDwell } from "../board-model.js";
import { value } from "../normalize.js";

// nowCardModel decides whether the page has something to say at the top. It is
// rendered when a workflow wait is open or an open review thread blocks the
// merge, and not otherwise: a permanent card would stop being read.
export function nowCardModel(model) {
  if (model.wait) {
    const message = String(value(model.wait, "message", "Message") || "").trim();
    return {
      tone: model.waitKind === "failed" || model.waitKind === "budget" ? "danger" : "await",
      heading:
        model.waitKind === "failed"
          ? "Now · workflow step failed"
          : model.waitKind === "gate"
            ? "Now · waiting for your review"
            : `Now · ${model.activity.toLowerCase()}`,
      age: model.dwell,
      actor: model.stepName,
      body: message,
      actions: nowActions(model),
    };
  }
  if (model.openThreads) {
    const thread = model.threads.find((candidate) => value(candidate, "state", "State") === "open");
    return {
      tone: "warn",
      heading: `Now · ${model.openThreads} review thread${model.openThreads === 1 ? "" : "s"} block the merge`,
      // The thread age uses the model's clock: it is the same elapsed-time
      // rendering as dwell, so the two must not drift under a fixed clock.
      // Models built without a clock (bare test payloads) fall back to the
      // wall clock, matching the pre-clock behavior.
      age: formatDwell(value(thread, "created_at", "CreatedAt"), model.now ?? Date.now()),
      actor: value(thread, "actor", "Actor") || "review",
      locus: `${value(thread, "file_path", "FilePath")}:${value(thread, "line", "Line")}`,
      threadID: value(thread, "id", "ID"),
      body: firstThreadBody(thread),
      outdated: isOutdatedAnchor(thread, model.change),
      actions: [
        { label: "Claim fixed", key: "thread-claim", kind: "fixed", primary: true },
        { label: "Not warranted", key: "thread-claim", kind: "not_warranted" },
        { label: "Open in diff", key: "open-change" },
      ],
    };
  }
  return null;
}

function nowActions(model) {
  switch (model.waitKind) {
    case "gate":
      return [{ label: "Answer", key: "focus-gate", primary: true, tab: model.review?.gate?.changeGate ? "change" : "review" }];
    case "question":
      return [{ label: "Answer", key: "focus-gate", primary: true, tab: "review" }];
    case "failed":
      return [
        { label: "Retry", key: "workflow-retry", primary: true },
        { label: "Skip step", key: "workflow-skip" },
        { label: "Transcript", key: "open-transcript" },
      ];
    case "budget":
      return [
        {
          label: String(value(model.wait, "reason", "Reason") || "") === "review_cycle_limit" ? "Grant cycles" : "Extend budget",
          key: "workflow-budget",
          budgetKind:
            String(value(model.wait, "reason", "Reason") || "") === "review_cycle_limit"
              ? "review-cycles"
              : "transitions",
          primary: true,
        },
      ];
    default:
      return [];
  }
}

function firstThreadBody(thread) {
  const comments = value(thread || {}, "comments", "Comments") || [];
  return value(comments[0] || {}, "body", "Body") || "";
}

// An anchor that no longer matches the change head means the reviewer was
// reading an older revision; saying so is the difference between a stale
// comment and a wrong one.
export function isOutdatedAnchor(thread, change) {
  const anchor = String(value(thread || {}, "anchor_commit_sha", "AnchorCommitSHA") || "");
  const head = String(value(change || {}, "head_sha", "HeadSHA") || "");
  return Boolean(anchor && head && anchor !== head);
}

// tabBadges are the counts the tab strip carries, with the tone that tells you
// whether the number is good news.
export function tabBadges(model) {
  const badges = {};
  if (model.review?.gate || model.review?.question) badges.review = { text: "!", tone: "warn" };
  if (model.change) badges.change = { text: String(model.openThreads || ""), tone: model.openThreads ? "warn" : "" };
  if (model.checks.length) {
    const ok = model.checksSatisfied === model.checks.length;
    badges.checks = { text: `${model.checksSatisfied}/${model.checks.length}`, tone: ok ? "ok" : "danger" };
  }
  const unresolvedFindings = Number(value(model.findings?.summary, "unresolved", "Unresolved") || 0);
  if (unresolvedFindings > 0) badges.findings = { text: String(unresolvedFindings), tone: "warn" };
  const activity = model.transitions.length + model.statusLog.length;
  if (activity) badges.activity = { text: String(activity), tone: "idle" };
  if (model.terminalAvailable) badges.terminal = { live: true };
  return badges;
}
