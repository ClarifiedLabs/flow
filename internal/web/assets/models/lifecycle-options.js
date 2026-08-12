// Lifecycle transition options for the lifecycle control.

// LIFECYCLE_TARGET_OPTIONS is the single vocabulary the lifecycle control
// enumerates. It mirrors the server's AllLifecycleTransitionTargets (coordinator/phase.go)
// and DoneResolution constants (flow_graph.go). The parity test
// lifecycle_parity_test.go ensures they stay in lockstep.
export const LIFECYCLE_TARGET_OPTIONS = [
  { value: "backlog", label: "Backlog \u00b7 Unscheduled", done: false, group: "Active" },
  { value: "unscheduled", label: "Backlog \u00b7 Unscheduled", done: false, group: "Active" },
  { value: "up_next", label: "Up Next (Scheduled)", done: false, group: "Active" },
  { value: "scheduled", label: "Up Next (Scheduled)", done: false, group: "Active" },
  { value: "working", label: "Working (In Progress)", done: false, group: "Active" },
  { value: "triage", label: "Triage", done: false, group: "Active" },
  { value: "critique", label: "Critique \u00b7 Needs Changes", done: false, group: "Review" },
  { value: "acceptance", label: "Acceptance", done: false, group: "Review" },
  { value: "approved", label: "Approved", done: false, group: "Review" },
  { value: "retry", label: "Retry failed step", done: false, group: "Active" },
  { value: "skip", label: "Skip failed step", done: false, group: "Active" },
  { value: "hold", label: "Pause (Hold)", done: false, group: "Active" },
  { value: "resume", label: "Resume (Release)", done: false, group: "Active" },
  { value: "reset", label: "Reset to Unscheduled", done: false, group: "Active" },
  { value: "schedule", label: "Schedule", done: false, group: "Active" },
  { value: "reopen", label: "Reopen", done: false, group: "Active" },
  { value: "done:completed", label: "Done \u2014 Completed", done: true, group: "Terminal" },
  { value: "done:rejected", label: "Done \u2014 Rejected", done: true, group: "Terminal" },
  { value: "done:abandoned", label: "Done \u2014 Abandoned", done: true, group: "Terminal" },
  { value: "done:cancelled", label: "Done \u2014 Cancelled", done: true, group: "Terminal" },
  { value: "done:failed", label: "Done \u2014 Failed", done: true, group: "Terminal" },
];

export function lifecycleModel(model) {
  if (!model) return null;
  const phase = String(model.lifecyclePhase || "").trim() || String(model.lifecycleState || "unscheduled");
  return {
    phase,
    lifecycleState: model.lifecycleState,
    runState: model.runState,
    waitKind: model.waitKind,
    held: Boolean(model.held),
    runID: String(model.runID || ""),
    nodeRunID: String(model.nodeRunID || ""),
    isDone: model.lifecycleState === "done",
    hasActiveRun: Boolean(model.runID),
    convergenceEvidence: model.convergenceEvidence || null,
    canRetry: !model.held && !model.convergenceEvidence && Boolean(model.runID),
    canSkip: !model.held && Boolean(model.nodeRunID),
  };
}

export function lifecycleOptionsForModel(model) {
  const lm = lifecycleModel(model);
  if (!lm) return [];
  const current = String(lm.phase || lm.lifecycleState || "").trim().toLowerCase();
  return LIFECYCLE_TARGET_OPTIONS.map((option) => {
    let disabled = false;
    let reason = "";
    const target = option.value.toLowerCase();
    if (target === current || (target.startsWith("done:") && lm.isDone)) {
      disabled = false;
    }
    if ((target === "retry" || target === "skip") && !lm.hasActiveRun) {
      disabled = true;
      reason = "No active workflow run";
    }
    if ((target === "skip") && !lm.nodeRunID) {
      disabled = true;
      reason = "No retryable step";
    }
    if ((target === "hold" || target === "pause") && lm.held) {
      disabled = true;
      reason = "Already held";
    }
    if ((target === "resume" || target === "release") && !lm.held) {
      disabled = true;
      reason = "Not held";
    }
    if (lm.convergenceEvidence && (target === "retry" || target === "skip" || target === "reset")) {
      disabled = true;
      reason = "Convergence hold requires explicit disposition";
    }
    if (target === "reopen" && !lm.isDone) {
      disabled = true;
      reason = "Only Done tasks can be reopened";
    }
    return { ...option, disabled, reason };
  });
}
