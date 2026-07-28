package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
)

func configureCheckCompletion(t *testing.T, mode checkverdict.Mode) (string, string) {
	t.Helper()
	directory := t.TempDir()
	verdictPath := filepath.Join(directory, checkverdict.VerdictFileName)
	completionPath := filepath.Join(directory, checkverdict.CompletionFileName)
	t.Setenv("FLOW_COMPLETION_PROTOCOL", checkverdict.CompletionProtocol)
	t.Setenv("FLOW_JOB_ID", "j-test")
	t.Setenv("FLOW_CHECK_NAME", "review")
	t.Setenv("FLOW_CHECK_MODE", string(mode))
	t.Setenv("FLOW_VERDICT_FILE", verdictPath)
	t.Setenv("FLOW_COMPLETION_FILE", completionPath)
	return verdictPath, completionPath
}

func TestRunCompleteCheckReturnsSchemaFeedbackWithoutSealing(t *testing.T) {
	verdictPath, completionPath := configureCheckCompletion(t, checkverdict.ModeReviewAggregation)
	introduced := "false"
	contents := `{"verdict":"blocked","reason":"finding","comments":[{"sha":"abc","file":"main.go","line":3,"body":"problem","severity":"medium","introduced_by_change":` + introduced + `}]}`
	if err := os.WriteFile(verdictPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runComplete(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("runComplete() = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "comment 0 missing requirement") {
		t.Fatalf("stderr = %q, want missing-requirement feedback", stderr.String())
	}
	if _, err := os.Stat(completionPath); !os.IsNotExist(err) {
		t.Fatalf("completion seal exists after invalid verdict: %v", err)
	}
}

func TestRunCompleteCheckSealsAndReplays(t *testing.T) {
	verdictPath, completionPath := configureCheckCompletion(t, checkverdict.ModeReview)
	if err := os.WriteFile(verdictPath, []byte(`{"verdict":"satisfied","reason":"ready"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var stdout, stderr bytes.Buffer
		if code := runComplete(nil, &stdout, &stderr); code != 0 {
			t.Fatalf("runComplete() attempt %d = %d; stderr=%s", attempt, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "j-test\treview\tsatisfied") {
			t.Fatalf("stdout = %q", stdout.String())
		}
	}
	if _, err := os.Stat(completionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verdictPath, []byte(`{"verdict":"blocked","reason":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runComplete(nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "seal is final") {
		t.Fatalf("changed runComplete() = %d stderr=%q", code, stderr.String())
	}
}

func TestRunCompleteCheckRejectsAuthorFlagsAndIncompleteContext(t *testing.T) {
	configureCheckCompletion(t, checkverdict.ModeVerify)
	var stdout, stderr bytes.Buffer
	if code := runComplete([]string{"--summary-file", "SUMMARY.md"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runComplete(flags) = %d, want 2", code)
	}
	t.Setenv("FLOW_CHECK_MODE", "")
	stdout.Reset()
	stderr.Reset()
	if code := runComplete(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "FLOW_CHECK_MODE") {
		t.Fatalf("runComplete(incomplete) = %d stderr=%q", code, stderr.String())
	}
}
