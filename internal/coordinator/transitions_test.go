package coordinator

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestGraphSummaryForIssue(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	issue, err := NewIssueService(store.DB()).CreateIssue(ctx, CreateIssueInput{Title: "graph"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	seq := 0
	insert := func(from, to, eventKind, payload string) {
		t.Helper()
		seq++
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO transitions (issue_id, from_phase, event_kind, payload_json, to_phase, actor, created_at)
VALUES (?, ?, ?, ?, ?, 'owner:test', ?)`,
			issue.ID, from, eventKind, payload, to, fmt.Sprintf("2026-06-10T00:00:%02dZ", seq)); err != nil {
			t.Fatalf("insert transition %d: %v", seq, err)
		}
	}

	// Initial row (from_phase='') must not become an edge.
	insert("", "backlog", "schedule_issue", "{}")
	insert("backlog", "up_next", "schedule_issue", "{}")
	insert("up_next", "authoring", "ensure_author_job", "{}")
	// Self-loop must not become an edge.
	insert("authoring", "authoring", "session_ready", "{}")
	insert("authoring", "critique", "session_ready", "{}")
	// Reviewer sends it back twice; one satisfied reviewer check must not count.
	insert("critique", "critique", "check_reported", `{"check_kind":"reviewer","verdict":"blocked"}`)
	insert("critique", "authoring", "ensure_fix_author_job", "{}")
	insert("authoring", "critique", "session_ready", "{}")
	insert("critique", "critique", "check_reported", `{"check_kind":"reviewer","verdict":"blocked"}`)
	insert("critique", "authoring", "ensure_fix_author_job", "{}")
	insert("authoring", "critique", "session_ready", "{}")
	insert("critique", "critique", "check_reported", `{"check_kind":"reviewer","verdict":"satisfied"}`)
	// Verifier sends it back once.
	insert("critique", "critique", "check_reported", `{"check_kind":"verifier","verdict":"blocked"}`)
	insert("critique", "authoring", "ensure_fix_author_job", "{}")
	insert("authoring", "critique", "session_ready", "{}")

	summary, err := NewTransitionService(store.DB()).GraphSummaryForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("graph summary: %v", err)
	}

	if summary.CurrentPhase != "critique" {
		t.Errorf("CurrentPhase = %q, want %q", summary.CurrentPhase, "critique")
	}
	if summary.ReviewerSends != 2 {
		t.Errorf("ReviewerSends = %d, want 2", summary.ReviewerSends)
	}
	if summary.VerifierSends != 1 {
		t.Errorf("VerifierSends = %d, want 1", summary.VerifierSends)
	}

	wantEdges := map[string]int{
		"backlog|up_next":    1,
		"up_next|authoring":  1,
		"authoring|critique": 4,
		"critique|authoring": 3,
	}
	gotEdges := make(map[string]int, len(summary.Edges))
	for _, edge := range summary.Edges {
		gotEdges[edge.FromPhase+"|"+edge.ToPhase] = edge.Count
	}
	if len(gotEdges) != len(wantEdges) {
		t.Errorf("edges = %v, want %v", gotEdges, wantEdges)
	}
	for key, want := range wantEdges {
		if gotEdges[key] != want {
			t.Errorf("edge %s = %d, want %d", key, gotEdges[key], want)
		}
	}
}

func TestGraphSummaryForIssueEmptyHistory(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	issue, err := NewIssueService(store.DB()).CreateIssue(ctx, CreateIssueInput{Title: "untouched"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	summary, err := NewTransitionService(store.DB()).GraphSummaryForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("graph summary: %v", err)
	}
	if summary.CurrentPhase != "" {
		t.Errorf("CurrentPhase = %q, want empty", summary.CurrentPhase)
	}
	if len(summary.Edges) != 0 {
		t.Errorf("Edges = %v, want none", summary.Edges)
	}
	if summary.ReviewerSends != 0 || summary.VerifierSends != 0 {
		t.Errorf("sends = %d/%d, want 0/0", summary.ReviewerSends, summary.VerifierSends)
	}
}
