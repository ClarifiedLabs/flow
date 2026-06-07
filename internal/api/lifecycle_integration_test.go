package api

import (
	"context"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// TestEngineReportCheckHumanReviewIntegration exercises the lifecycle engine
// end-to-end through the real API server and coordinator services: readying a
// human-review change, then approving the human review, must record the check as
// satisfied without auto-merging (the issue requires human merge), exactly as the
// pre-engine handler did.
func TestEngineReportCheckHumanReviewIntegration(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	_, exchangePath := readyApprovedMergeChange(t, fixture, "Engine integration change")
	humanReviewStarted := startAuthorSessionForStatusTestWithWorker(t, fixture, "Engine integration human review", "w-engine-human")
	humanReviewHead := pushBrowserSmokeBranch(t, exchangePath, humanReviewStarted.Change.Branch)
	doJSONRequestAs(t, fixture.Server, humanReviewStarted.Token, "POST", "/v1/sessions/"+humanReviewStarted.Session.ID+"/ready", readySessionRequest{
		HeadSHA: humanReviewHead,
	}, 200, nil)
	issueID := humanReviewStarted.Session.IssueID

	required := true
	var resp checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", "POST",
		"/v1/issues/"+issueID+"/checks/human-review",
		reportCheckRequest{Kind: "human", Required: &required, Verdict: "satisfied", Details: "approved via web UI", Reporter: "web-ui"},
		200, &resp)
	if resp.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("human review verdict = %q, want satisfied", resp.Check.Verdict)
	}
	if resp.Check.Details != "approved via web UI" {
		t.Fatalf("human review details = %q, want %q", resp.Check.Details, "approved via web UI")
	}

	change, err := fixture.Sessions.GetChange(ctx, humanReviewStarted.Change.ID)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	if change.MergedAt != nil {
		t.Fatalf("approving a human review must not auto-merge (mergedAt=%v)", change.MergedAt)
	}

	// The engine recorded the transition in the append-only log.
	var transitions int
	if err := fixture.DB.QueryRow(`SELECT COUNT(*) FROM transitions WHERE issue_id = ? AND event_kind = 'check_reported'`, issueID).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions == 0 {
		t.Fatalf("expected a check_reported transition to be logged")
	}
}
