package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadVerdictFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	if err := os.WriteFile(path, []byte(`{"verdict":"blocked","reason":"two open threads"}`), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}

	v, ok, err := ReadVerdictFile(path)
	if err != nil {
		t.Fatalf("ReadVerdictFile err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ReadVerdictFile ok = false, want true")
	}
	if v.Verdict != "blocked" {
		t.Fatalf("verdict = %q, want blocked", v.Verdict)
	}
	if v.Reason != "two open threads" {
		t.Fatalf("reason = %q, want %q", v.Reason, "two open threads")
	}
}

func TestReadVerdictFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), VerdictFileName)
	v, ok, err := ReadVerdictFile(path)
	if err != nil {
		t.Fatalf("ReadVerdictFile err = %v, want nil", err)
	}
	if ok {
		t.Fatalf("ReadVerdictFile ok = true, want false for missing file")
	}
	if v.Verdict != "" || v.Reason != "" || v.Comments != nil || v.Threads != nil {
		t.Fatalf("verdict = %+v, want zero value", v)
	}
}

func TestReadVerdictFileInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}

	_, ok, err := ReadVerdictFile(path)
	if ok {
		t.Fatalf("ReadVerdictFile ok = true, want false for invalid json")
	}
	if err == nil {
		t.Fatalf("ReadVerdictFile err = nil, want parse error")
	}
}

func TestReadVerdictFileBadVerdict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	if err := os.WriteFile(path, []byte(`{"verdict":"maybe","reason":"unsure"}`), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}

	_, ok, err := ReadVerdictFile(path)
	if ok {
		t.Fatalf("ReadVerdictFile ok = true, want false for bad verdict")
	}
	if err == nil {
		t.Fatalf("ReadVerdictFile err = nil, want invalid-verdict error")
	}
}

func TestReadVerdictFileRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	// Valid JSON shape, but the reason alone pushes the file past the 256KB read
	// ceiling, so the read must bail before allocating the whole thing.
	huge := strings.Repeat("x", 300*1024)
	if err := os.WriteFile(path, []byte(`{"verdict":"satisfied","reason":"`+huge+`"}`), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}

	_, ok, err := ReadVerdictFile(path)
	if ok {
		t.Fatalf("ReadVerdictFile ok = true, want false for oversized file")
	}
	if err == nil {
		t.Fatalf("ReadVerdictFile err = nil, want size-limit error")
	}
}

func TestReadVerdictFileTruncatesReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	longReason := strings.Repeat("x", 5000)
	if err := os.WriteFile(path, []byte(`{"verdict":"satisfied","reason":"`+longReason+`"}`), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}

	v, ok, err := ReadVerdictFile(path)
	if err != nil {
		t.Fatalf("ReadVerdictFile err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ReadVerdictFile ok = false, want true")
	}
	if len(v.Reason) != 4096 {
		t.Fatalf("reason length = %d, want 4096", len(v.Reason))
	}
}

func writeVerdict(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, VerdictFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write verdict file: %v", err)
	}
	return path
}

func TestReadVerdictFileParsesCommentsAndThreads(t *testing.T) {
	path := writeVerdict(t, `{
		"verdict": "blocked",
		"reason": "one concern",
		"comments": [
			{"sha": " abc123 ", "file": " internal/app.go ", "line": 42, "body": " needs a guard "}
		],
		"threads": [
			{"id": "th-1", "decision": "certify", "body": "confirmed"},
			{"id": "th-2", "decision": "reopen", "body": "still missing nil check"}
		]
	}`)

	v, ok, err := ReadVerdictFile(path)
	if err != nil || !ok {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want true/nil", ok, err)
	}
	if len(v.Comments) != 1 {
		t.Fatalf("comments = %+v, want 1", v.Comments)
	}
	c := v.Comments[0]
	if c.SHA != "abc123" || c.File != "internal/app.go" || c.Line != 42 || c.Body != "needs a guard" {
		t.Fatalf("comment = %+v, want trimmed fields", c)
	}
	if len(v.Threads) != 2 {
		t.Fatalf("threads = %+v, want 2", v.Threads)
	}
	if v.Threads[0].Decision != "certify" || v.Threads[1].Decision != "reopen" {
		t.Fatalf("threads = %+v, want certify then reopen", v.Threads)
	}
}

func TestReadVerdictFileRejectsTooManyComments(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"verdict":"blocked","reason":"x","comments":[`)
	for i := 0; i < verdictMaxComments+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"sha":"abc","file":"a.go","line":1,"body":"b"}`)
	}
	sb.WriteString(`]}`)

	_, ok, err := ReadVerdictFile(writeVerdict(t, sb.String()))
	if ok || err == nil {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want false/non-nil for over-cap comments", ok, err)
	}
}

func TestReadVerdictFileRejectsTooManyThreadDecisions(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"verdict":"satisfied","reason":"x","threads":[`)
	for i := 0; i < verdictMaxThreadDecisions+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"th","decision":"certify"}`)
	}
	sb.WriteString(`]}`)

	_, ok, err := ReadVerdictFile(writeVerdict(t, sb.String()))
	if ok || err == nil {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want false/non-nil for over-cap threads", ok, err)
	}
}

func TestReadVerdictFileRejectsUnknownDecision(t *testing.T) {
	path := writeVerdict(t, `{"verdict":"satisfied","reason":"x","threads":[{"id":"th-1","decision":"maybe"}]}`)
	_, ok, err := ReadVerdictFile(path)
	if ok || err == nil {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want false/non-nil for unknown decision", ok, err)
	}
}

func TestReadVerdictFileRejectsReopenWithoutBody(t *testing.T) {
	path := writeVerdict(t, `{"verdict":"blocked","reason":"x","threads":[{"id":"th-1","decision":"reopen","body":"  "}]}`)
	_, ok, err := ReadVerdictFile(path)
	if ok || err == nil {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want false/non-nil for reopen without body", ok, err)
	}
}

func TestReadVerdictFileRejectsCommentMissingAnchor(t *testing.T) {
	path := writeVerdict(t, `{"verdict":"blocked","reason":"x","comments":[{"sha":"abc","file":"a.go","line":0,"body":"b"}]}`)
	_, ok, err := ReadVerdictFile(path)
	if ok || err == nil {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want false/non-nil for non-positive line", ok, err)
	}
}

func TestReadVerdictFileTruncatesCommentAndThreadBodies(t *testing.T) {
	long := strings.Repeat("y", 5000)
	path := writeVerdict(t, `{"verdict":"blocked","reason":"x","comments":[{"sha":"abc","file":"a.go","line":1,"body":"`+long+`"}],"threads":[{"id":"th-1","decision":"reopen","body":"`+long+`"}]}`)
	v, ok, err := ReadVerdictFile(path)
	if err != nil || !ok {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want true/nil", ok, err)
	}
	if len(v.Comments[0].Body) != verdictReasonMaxBytes {
		t.Fatalf("comment body length = %d, want %d", len(v.Comments[0].Body), verdictReasonMaxBytes)
	}
	if len(v.Threads[0].Body) != verdictReasonMaxBytes {
		t.Fatalf("thread body length = %d, want %d", len(v.Threads[0].Body), verdictReasonMaxBytes)
	}
}

func TestReadVerdictFileAllowsLargerFileUnderNewCeiling(t *testing.T) {
	// A ~70KB reason exceeded the old 64KB ceiling but fits the 256KB one; it
	// parses and the reason is truncated to the per-field bound.
	reason := strings.Repeat("x", 70*1024)
	path := writeVerdict(t, `{"verdict":"satisfied","reason":"`+reason+`"}`)
	v, ok, err := ReadVerdictFile(path)
	if err != nil || !ok {
		t.Fatalf("ReadVerdictFile ok=%v err=%v, want true/nil under raised ceiling", ok, err)
	}
	if len(v.Reason) != verdictReasonMaxBytes {
		t.Fatalf("reason length = %d, want %d", len(v.Reason), verdictReasonMaxBytes)
	}
}
