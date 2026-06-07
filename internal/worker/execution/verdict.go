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

// VerdictReport is the structured outcome a check job may write to
// FLOW_VERDICT_FILE; it takes precedence over the exit-code mapping. Reviewer
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

// ReviewCommentReport is one reviewer concern to file as an anchored review
// thread. SHA/File/Line locate the concern; Body is the comment text.
type ReviewCommentReport struct {
	SHA  string `json:"sha"`
	File string `json:"file"`
	Line int    `json:"line"`
	Body string `json:"body"`
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
// when the file is absent (the common case: the job relied on its exit code),
// and (zero, false, err) when the file exists but is unreadable, unparseable,
// or carries a verdict, comment, or thread decision that fails validation.
// Callers fall back to the exit-code mapping on the false/err path and must
// surface err to job stdout (never silently swallow it).
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
		if len(c.Body) > verdictReasonMaxBytes {
			c.Body = c.Body[:verdictReasonMaxBytes]
		}
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
