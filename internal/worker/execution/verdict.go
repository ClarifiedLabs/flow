package execution

import "github.com/ClarifiedLabs/flow/internal/checkverdict"

// VerdictReport is the structured outcome a reviewer or verifier job writes to
// FLOW_VERDICT_FILE. It is the only source of a reviewer/verifier result.
// jobs may additionally carry the blocking concerns they want filed as review
// threads, and verifier jobs the certify/reopen decisions they reached, so the
// worker applies them mechanically instead of relying on the agent to remember
// to run flow comment / flow thread per item.
type VerdictReport = checkverdict.VerdictReport

// ReviewCommentReport is one classified reviewer concern. Task-caused unique
// critical/high concerns become anchored review threads; the rest remain
// non-blocking follow-up context.
type ReviewCommentReport = checkverdict.ReviewCommentReport

// ReviewTaskActionReport tells the final review aggregator how to retain one
// unique, actionable, non-blocking finding as durable work. CreateTask carries
// a self-contained task specification; UseExistingTask names an open task that
// already covers the same root issue.
type ReviewTaskActionReport = checkverdict.ReviewTaskActionReport

// BlocksApproval reports whether this finding is allowed to veto the current
// change. Reviewers still report pre-existing, lower-severity, and duplicate
// findings, but those are retained as follow-up context instead of extending
// the current task's author-review loop.
// ThreadDecisionReport is one verifier decision on an existing review thread.
// Decision is "certify" or "reopen"; reopen requires a Body explaining why.
type ThreadDecisionReport = checkverdict.ThreadDecisionReport

// VerdictFileName is the basename of the per-job verdict file the worker
// exports as FLOW_VERDICT_FILE. A check job writes it before its entrypoint
// exits to report a structured verdict instead of relying on the exit code.
const VerdictFileName = checkverdict.VerdictFileName

// Retained as package-local aliases for the execution package's existing
// boundary tests. Validation itself lives in internal/checkverdict.
const (
	verdictReasonMaxBytes     = 4096
	verdictMaxComments        = 50
	verdictMaxThreadDecisions = 100
)

// ReadVerdictFile reads a check job's structured verdict from path. It returns
// (report, true, nil) when a valid verdict file is present, (zero, false, nil)
// when the file is absent,
// and (zero, false, err) when the file exists but is unreadable, unparseable,
// or carries a verdict, comment, or thread decision that fails validation.
// Callers must treat absence or invalid contents as an execution error for
// reviewer/verifier jobs and surface err to job stdout.
func ReadVerdictFile(path string) (VerdictReport, bool, error) {
	return checkverdict.ReadFile(path)
}
