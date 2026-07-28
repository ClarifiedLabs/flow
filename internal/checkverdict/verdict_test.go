package checkverdict

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEnforcesRoleModes(t *testing.T) {
	introduced := false
	comment := ReviewCommentReport{
		SHA: "abc", File: "main.go", Line: 1, Body: "finding", Severity: "medium",
		IntroducedByChange: &introduced, Requirement: "correctness",
	}
	tests := []struct {
		name    string
		mode    Mode
		report  VerdictReport
		wantErr string
	}{
		{
			name:   "review comments",
			mode:   ModeReview,
			report: VerdictReport{Verdict: "blocked", Comments: []ReviewCommentReport{comment}},
		},
		{
			name: "review forbids actions",
			mode: ModeReview,
			report: VerdictReport{Verdict: "blocked", Comments: []ReviewCommentReport{func() ReviewCommentReport {
				withAction := comment
				withAction.TaskAction = &ReviewTaskActionReport{Action: "create_task", Title: "Follow up", Body: "Do it"}
				return withAction
			}()}},
			wantErr: "mode review forbids comment 0 task_action",
		},
		{
			name:    "discovery forbids threads",
			mode:    ModeReviewDiscovery,
			report:  VerdictReport{Verdict: "blocked", Threads: []ThreadDecisionReport{{ID: "th-1", Decision: "certify"}}},
			wantErr: "mode review_discovery forbids threads",
		},
		{
			name: "aggregation actions",
			mode: ModeReviewAggregation,
			report: VerdictReport{Verdict: "blocked", Comments: []ReviewCommentReport{func() ReviewCommentReport {
				withAction := comment
				withAction.TaskAction = &ReviewTaskActionReport{Action: "create_task", Title: "Follow up", Body: "Do it"}
				return withAction
			}()}},
		},
		{
			name:   "verify threads",
			mode:   ModeVerify,
			report: VerdictReport{Verdict: "satisfied", Threads: []ThreadDecisionReport{{ID: "th-1", Decision: "certify"}}},
		},
		{
			name:    "verify forbids comments",
			mode:    ModeVerify,
			report:  VerdictReport{Verdict: "blocked", Comments: []ReviewCommentReport{comment}},
			wantErr: "mode verify forbids comments",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.report)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Validate(data, test.mode)
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateRejectsForbiddenFieldsEvenWhenEmpty(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    Mode
		verdict string
		wantErr string
	}{
		{
			name:    "review empty threads",
			mode:    ModeReview,
			verdict: `{"verdict":"satisfied","reason":"ready","threads":[]}`,
			wantErr: "forbids threads",
		},
		{
			name:    "discovery null task action",
			mode:    ModeReviewDiscovery,
			verdict: `{"verdict":"blocked","reason":"finding","comments":[{"sha":"abc","file":"main.go","line":1,"body":"finding","severity":"medium","introduced_by_change":false,"requirement":"correctness","task_action":null}]}`,
			wantErr: "forbids comment 0 task_action",
		},
		{
			name:    "verify empty comments",
			mode:    ModeVerify,
			verdict: `{"verdict":"satisfied","reason":"ready","comments":[]}`,
			wantErr: "forbids comments",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Validate([]byte(test.verdict), test.mode); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestSealVerdictBindsExactBytesAndIsFinal(t *testing.T) {
	directory := t.TempDir()
	verdictPath := filepath.Join(directory, VerdictFileName)
	sealPath := filepath.Join(directory, CompletionFileName)
	first := []byte("{\"verdict\":\"satisfied\",\"reason\":\"ready\"}\n")
	if err := os.WriteFile(verdictPath, first, 0o600); err != nil {
		t.Fatal(err)
	}
	context := Context{JobID: "j-1", CheckName: "review", Mode: ModeReview}
	validated, err := SealVerdict(verdictPath, sealPath, context)
	if err != nil {
		t.Fatalf("SealVerdict() error = %v", err)
	}
	if validated.Digest == "" {
		t.Fatal("SealVerdict() returned an empty digest")
	}
	info, err := os.Stat(sealPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("seal mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := SealVerdict(verdictPath, sealPath, context); err != nil {
		t.Fatalf("exact replay error = %v", err)
	}
	if err := os.WriteFile(verdictPath, []byte("{\"verdict\":\"satisfied\",\"reason\":\"changed\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SealVerdict(verdictPath, sealPath, context); err == nil || !strings.Contains(err.Error(), "seal is final") {
		t.Fatalf("changed replay error = %v, want final-seal error", err)
	}
	if err := os.WriteFile(verdictPath, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SealVerdict(verdictPath, sealPath, context); err == nil || !strings.Contains(err.Error(), "seal is final") {
		t.Fatalf("invalid changed replay error = %v, want final-seal error", err)
	}
}

func TestVerifySealRejectsDifferentContext(t *testing.T) {
	directory := t.TempDir()
	verdictPath := filepath.Join(directory, VerdictFileName)
	sealPath := filepath.Join(directory, CompletionFileName)
	if err := os.WriteFile(verdictPath, []byte(`{"verdict":"satisfied","reason":"ready"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	context := Context{JobID: "j-1", CheckName: "review", Mode: ModeReview}
	if _, err := SealVerdict(verdictPath, sealPath, context); err != nil {
		t.Fatal(err)
	}
	_, present, err := VerifySeal(sealPath, verdictPath, Context{JobID: "j-2", CheckName: "review", Mode: ModeReview})
	if !present || err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("VerifySeal() present=%v error=%v", present, err)
	}
}
