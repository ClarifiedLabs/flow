package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// VerdictReport is the structured outcome a reviewer or verifier job writes to
// FLOW_VERDICT_FILE. It is the only source of a reviewer/verifier result.
// jobs may additionally carry the blocking concerns they want filed as review
// threads, and verifier jobs the certify/reopen decisions they reached, so the
// worker applies them mechanically instead of relying on the agent to remember
// to run flow comment / flow thread per item.
type VerdictReport struct {
	Verdict  string                 `json:"verdict"`
	Reason   string                 `json:"reason"`
	Comments []ReviewCommentReport  `json:"comments,omitempty"`
	Threads  []ThreadDecisionReport `json:"threads,omitempty"`
}

// ReviewCommentReport is one classified reviewer concern. Task-caused unique
// critical/high concerns become anchored review threads; the rest remain
// non-blocking follow-up context.
type ReviewCommentReport struct {
	SHA                string                  `json:"sha"`
	File               string                  `json:"file"`
	Line               int                     `json:"line"`
	Body               string                  `json:"body"`
	Severity           string                  `json:"severity"`
	IntroducedByChange *bool                   `json:"introduced_by_change"`
	Requirement        string                  `json:"requirement"`
	DuplicateOf        string                  `json:"duplicate_of,omitempty"`
	FollowUp           string                  `json:"follow_up,omitempty"`
	TaskAction         *ReviewTaskActionReport `json:"task_action,omitempty"`
}

// ReviewTaskActionReport tells the final review aggregator how to retain one
// unique, actionable, non-blocking finding as durable work. CreateTask carries
// a self-contained task specification; UseExistingTask names an open task that
// already covers the same root issue.
type ReviewTaskActionReport struct {
	Action string `json:"action"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

// BlocksApproval reports whether this finding is allowed to veto the current
// change. Reviewers still report pre-existing, lower-severity, and duplicate
// findings, but those are retained as follow-up context instead of extending
// the current task's author-review loop.
func (r ReviewCommentReport) BlocksApproval() bool {
	if r.IntroducedByChange == nil || !*r.IntroducedByChange || r.DuplicateOf != "" {
		return false
	}
	return r.Severity == "critical" || r.Severity == "high"
}

// ThreadDecisionReport is one verifier decision on an existing review thread.
// Decision is "certify" or "reopen"; reopen requires a Body explaining why.
type ThreadDecisionReport struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Body     string `json:"body"`
}

// VerdictFileName is the basename of the per-job verdict file the worker
// exports as FLOW_VERDICT_FILE. A check job writes it before its entrypoint
// exits to report a structured verdict instead of relying on the exit code.
const VerdictFileName = ".flow-verdict.json"

// verdictReasonMaxBytes bounds the free-text reason so a runaway agent cannot
// flood the coordinator's check details column. It also bounds each review
// comment and thread-decision body.
const verdictReasonMaxBytes = 4096

const verdictTaskTitleMaxBytes = 256

// verdictMaxComments and verdictMaxThreadDecisions cap how many actions a single
// job can carry so a buggy or adversarial job cannot enqueue an unbounded number
// of coordinator writes. A real review round stays well under these.
const (
	verdictMaxComments        = 50
	verdictMaxThreadDecisions = 100
)

// verdictFileMaxBytes caps how much of the job-controlled verdict file we read.
// A valid verdict (verdict + a <= 4096-byte reason, plus up to 50 comments and
// 100 thread decisions whose bodies are each <= 4096 bytes) fits under this; a
// file larger than the ceiling is treated as a parse error so a buggy or
// adversarial job cannot force a large allocation.
const verdictFileMaxBytes = 256 * 1024

// ReadVerdictFile reads a check job's structured verdict from path. It returns
// (report, true, nil) when a valid verdict file is present, (zero, false, nil)
// when the file is absent,
// and (zero, false, err) when the file exists but is unreadable, unparseable,
// or carries a verdict, comment, or thread decision that fails validation.
// Callers must treat absence or invalid contents as an execution error for
// reviewer/verifier jobs and surface err to job stdout.
func ReadVerdictFile(path string) (VerdictReport, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return VerdictReport{}, false, nil
	}
	if err != nil {
		return VerdictReport{}, false, err
	}
	defer file.Close()

	// Read one byte past the ceiling so we can tell "exactly at the limit" from
	// "overflowed" and reject the latter instead of silently parsing a truncated
	// prefix.
	data, err := io.ReadAll(io.LimitReader(file, verdictFileMaxBytes+1))
	if err != nil {
		return VerdictReport{}, false, err
	}
	if len(data) > verdictFileMaxBytes {
		return VerdictReport{}, false, fmt.Errorf("parse verdict file: exceeds %d bytes", verdictFileMaxBytes)
	}
	var v VerdictReport
	if err := json.Unmarshal(data, &v); err != nil {
		return VerdictReport{}, false, fmt.Errorf("parse verdict file: %w", err)
	}
	switch v.Verdict {
	case "satisfied", "blocked":
	default:
		return VerdictReport{}, false, fmt.Errorf("invalid verdict %q (want satisfied|blocked)", v.Verdict)
	}
	if len(v.Reason) > verdictReasonMaxBytes {
		v.Reason = v.Reason[:verdictReasonMaxBytes]
	}
	if err := normalizeVerdictComments(&v); err != nil {
		return VerdictReport{}, false, err
	}
	if v.Verdict == "satisfied" {
		for i, comment := range v.Comments {
			if comment.BlocksApproval() {
				return VerdictReport{}, false, fmt.Errorf(
					"verdict file: satisfied verdict includes blocking comment %d",
					i,
				)
			}
		}
	}
	if err := normalizeVerdictThreads(&v); err != nil {
		return VerdictReport{}, false, err
	}
	return v, true, nil
}

// normalizeVerdictComments validates and trims the reviewer concerns, enforcing
// the count cap, the required anchor fields a thread needs, and the per-body
// size bound. A malformed comment fails the whole file so the worker falls back
// to the exit code rather than filing a half-formed concern.
func normalizeVerdictComments(v *VerdictReport) error {
	if len(v.Comments) > verdictMaxComments {
		return fmt.Errorf("verdict file: %d comments exceeds cap of %d", len(v.Comments), verdictMaxComments)
	}
	for i := range v.Comments {
		c := &v.Comments[i]
		c.SHA = strings.TrimSpace(c.SHA)
		c.File = strings.TrimSpace(c.File)
		c.Body = strings.TrimSpace(c.Body)
		c.Severity = strings.ToLower(strings.TrimSpace(c.Severity))
		c.Requirement = strings.TrimSpace(c.Requirement)
		c.DuplicateOf = strings.TrimSpace(c.DuplicateOf)
		c.FollowUp = strings.TrimSpace(c.FollowUp)
		if c.SHA == "" {
			return fmt.Errorf("verdict file: comment %d missing sha", i)
		}
		if c.File == "" {
			return fmt.Errorf("verdict file: comment %d missing file", i)
		}
		if c.Line <= 0 {
			return fmt.Errorf("verdict file: comment %d line must be positive", i)
		}
		if c.Body == "" {
			return fmt.Errorf("verdict file: comment %d missing body", i)
		}
		switch c.Severity {
		case "critical", "high", "medium", "low":
		default:
			return fmt.Errorf("verdict file: comment %d has invalid severity %q (want critical|high|medium|low)", i, c.Severity)
		}
		if c.IntroducedByChange == nil {
			return fmt.Errorf("verdict file: comment %d missing introduced_by_change", i)
		}
		if c.Requirement == "" {
			return fmt.Errorf("verdict file: comment %d missing requirement", i)
		}
		if c.DuplicateOf != "" && !strings.HasPrefix(c.DuplicateOf, "th-") {
			return fmt.Errorf("verdict file: comment %d has invalid duplicate_of %q", i, c.DuplicateOf)
		}
		if len(c.Body) > verdictReasonMaxBytes {
			c.Body = c.Body[:verdictReasonMaxBytes]
		}
		if len(c.Requirement) > verdictReasonMaxBytes {
			c.Requirement = c.Requirement[:verdictReasonMaxBytes]
		}
		if len(c.FollowUp) > verdictReasonMaxBytes {
			c.FollowUp = c.FollowUp[:verdictReasonMaxBytes]
		}
		if err := normalizeReviewTaskAction(i, c); err != nil {
			return err
		}
	}
	return nil
}

func normalizeReviewTaskAction(index int, comment *ReviewCommentReport) error {
	action := comment.TaskAction
	if action == nil {
		return nil
	}
	action.Action = strings.TrimSpace(action.Action)
	action.Title = strings.TrimSpace(action.Title)
	action.Body = strings.TrimSpace(action.Body)
	action.TaskID = strings.TrimSpace(action.TaskID)

	if comment.BlocksApproval() {
		return fmt.Errorf("verdict file: comment %d blocking finding cannot declare task_action", index)
	}
	if comment.DuplicateOf != "" {
		return fmt.Errorf("verdict file: comment %d review-thread duplicate cannot declare task_action", index)
	}
	switch action.Action {
	case "create_task":
		if action.Title == "" {
			return fmt.Errorf("verdict file: comment %d create_task requires title", index)
		}
		if action.Body == "" {
			return fmt.Errorf("verdict file: comment %d create_task requires body", index)
		}
		if action.TaskID != "" {
			return fmt.Errorf("verdict file: comment %d create_task must not specify task_id", index)
		}
		if len(action.Title) > verdictTaskTitleMaxBytes {
			return fmt.Errorf("verdict file: comment %d task title exceeds %d bytes", index, verdictTaskTitleMaxBytes)
		}
		if len(action.Body) > verdictReasonMaxBytes {
			return fmt.Errorf("verdict file: comment %d task body exceeds %d bytes", index, verdictReasonMaxBytes)
		}
	case "use_existing_task":
		if action.TaskID == "" {
			return fmt.Errorf("verdict file: comment %d use_existing_task requires task_id", index)
		}
		if action.Title != "" || action.Body != "" {
			return fmt.Errorf("verdict file: comment %d use_existing_task must not specify title or body", index)
		}
	default:
		return fmt.Errorf(
			"verdict file: comment %d has invalid task_action %q (want create_task|use_existing_task)",
			index,
			action.Action,
		)
	}
	return nil
}

// normalizeVerdictThreads validates and trims the verifier decisions, enforcing
// the count cap, the certify|reopen vocabulary, the reopen-requires-body rule,
// and the per-body size bound.
func normalizeVerdictThreads(v *VerdictReport) error {
	if len(v.Threads) > verdictMaxThreadDecisions {
		return fmt.Errorf("verdict file: %d thread decisions exceeds cap of %d", len(v.Threads), verdictMaxThreadDecisions)
	}
	for i := range v.Threads {
		d := &v.Threads[i]
		d.ID = strings.TrimSpace(d.ID)
		d.Decision = strings.TrimSpace(d.Decision)
		d.Body = strings.TrimSpace(d.Body)
		if d.ID == "" {
			return fmt.Errorf("verdict file: thread decision %d missing id", i)
		}
		switch d.Decision {
		case "certify", "reopen":
		default:
			return fmt.Errorf("verdict file: thread decision %d has invalid decision %q (want certify|reopen)", i, d.Decision)
		}
		if d.Decision == "reopen" && d.Body == "" {
			return fmt.Errorf("verdict file: thread decision %d reopen requires a body", i)
		}
		if len(d.Body) > verdictReasonMaxBytes {
			d.Body = d.Body[:verdictReasonMaxBytes]
		}
	}
	return nil
}
