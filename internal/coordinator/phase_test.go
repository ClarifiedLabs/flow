package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestDerivePhaseReachableStates(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	issues := NewIssueService(store.DB())

	mustPhase := func(id string, want Phase) {
		t.Helper()
		issue, err := issues.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("get issue %s: %v", id, err)
		}
		got, err := issues.PhaseForIssue(ctx, issue)
		if err != nil {
			t.Fatalf("derive phase %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("PhaseForIssue(%s) = %q, want %q", id, got, want)
		}
	}

	// Accepted + backlog (CreateIssue defaults).
	backlog, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "backlog"})
	if err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	mustPhase(backlog.ID, PhaseBacklog)

	// Accepted + up_next.
	upNext, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "up next"})
	if err != nil {
		t.Fatalf("create up_next issue: %v", err)
	}
	if _, err := issues.ScheduleIssue(ctx, upNext.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule up_next: %v", err)
	}
	mustPhase(upNext.ID, PhaseUpNext)

	// Rejected => rejected_closed.
	rejected, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "rejected"})
	if err != nil {
		t.Fatalf("create rejected issue: %v", err)
	}
	if _, err := issues.RejectTriage(ctx, rejected.ID); err != nil {
		t.Fatalf("reject triage: %v", err)
	}
	mustPhase(rejected.ID, PhaseRejectedClosed)

	// Closed without merge or rejection => abandoned.
	abandoned, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "abandoned"})
	if err != nil {
		t.Fatalf("create abandoned issue: %v", err)
	}
	if _, err := issues.CloseIssue(ctx, abandoned.ID); err != nil {
		t.Fatalf("close issue: %v", err)
	}
	mustPhase(abandoned.ID, PhaseAbandoned)
}

// TestDerivePhaseAcceptanceMirror exercises the in-review + ready-change branch
// of the coordinator's DerivePhase, asserting it emits the same acceptance
// phase the lifecycle engine's derivePhase does for the identical gate states.
func TestDerivePhaseAcceptanceMirror(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	issues := NewIssueService(store.DB())
	checks := NewCheckService(store.DB())

	// An accepted issue (CreateIssue default) with a ready, unmerged change and
	// no active author session lands in the in-review/critique/acceptance branch.
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "acceptance mirror"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`,
		"ch-acceptance",
		issue.ID,
		"issue/"+issue.ID,
		"1111111111111111111111111111111111111111",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert ready change: %v", err)
	}

	required := true
	report := func(name string, kind CheckKind, verdict CheckVerdict) {
		t.Helper()
		if _, err := checks.ReportCheck(ctx, ReportCheckInput{
			IssueID:  issue.ID,
			Name:     name,
			Kind:     kind,
			Required: &required,
			Verdict:  verdict,
		}); err != nil {
			t.Fatalf("report %s: %v", name, err)
		}
	}
	mustPhase := func(want Phase) {
		t.Helper()
		current, err := issues.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		got, err := issues.PhaseForIssue(ctx, current)
		if err != nil {
			t.Fatalf("derive phase: %v", err)
		}
		if got != want {
			t.Fatalf("DerivePhase = %q, want %q", got, want)
		}
	}

	// Critique not yet satisfied (CI still pending), no verifier: in_review with
	// the gate closed stays in critique.
	report("unit", CheckKindCI, CheckPending)
	mustPhase(PhaseCritique)

	// Critique satisfied, verifier pending: in_review with the gate open is
	// acceptance.
	report("unit", CheckKindCI, CheckSatisfied)
	report("verifier", CheckKindVerifier, CheckPending)
	mustPhase(PhaseAcceptance)

	// Verifier blocked flips the review state to changes_requested -> critique.
	report("verifier", CheckKindVerifier, CheckBlocked)
	mustPhase(PhaseCritique)

	// Verifier satisfied completes the required suite -> approved.
	report("verifier", CheckKindVerifier, CheckSatisfied)
	mustPhase(PhaseApproved)
}
