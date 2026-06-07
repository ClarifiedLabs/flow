package coordinator

import (
	"context"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newStatusTestService(t *testing.T) (*StatusService, string) {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	issue, err := NewIssueService(store.DB()).CreateIssue(ctx, CreateIssueInput{Title: "status"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	return NewStatusService(store.DB()), issue.ID
}

func TestWriteStatusWithKind(t *testing.T) {
	ctx := context.Background()
	service, issueID := newStatusTestService(t)

	entry, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "stuck on X", Kind: "blocker"})
	if err != nil {
		t.Fatalf("write status: %v", err)
	}
	if entry.Kind != "blocker" {
		t.Fatalf("entry.Kind = %q, want %q", entry.Kind, "blocker")
	}
}

func TestWriteStatusDefaultsKindNote(t *testing.T) {
	ctx := context.Background()
	service, issueID := newStatusTestService(t)

	entry, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "progressing"})
	if err != nil {
		t.Fatalf("write status: %v", err)
	}
	if entry.Kind != "note" {
		t.Fatalf("entry.Kind = %q, want %q", entry.Kind, "note")
	}
}

func TestWriteStatusRejectsBadKind(t *testing.T) {
	ctx := context.Background()
	service, issueID := newStatusTestService(t)

	if _, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "boom", Kind: "urgent"}); err == nil {
		t.Fatalf("expected error for invalid kind, got nil")
	}
}

func TestWriteStatusAcceptsAllKinds(t *testing.T) {
	ctx := context.Background()
	service, issueID := newStatusTestService(t)

	for _, kind := range []string{"note", "progress", "plan", "blocker", "question"} {
		entry, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "msg", Kind: kind})
		if err != nil {
			t.Fatalf("write status kind %q: %v", kind, err)
		}
		if entry.Kind != kind {
			t.Fatalf("entry.Kind = %q, want %q", entry.Kind, kind)
		}
	}
}

func TestListRecentByKind(t *testing.T) {
	ctx := context.Background()
	service, issueID := newStatusTestService(t)

	if _, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "note one", Kind: "note"}); err != nil {
		t.Fatalf("write note: %v", err)
	}
	blocker, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "blocked here", Kind: "blocker"})
	if err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	question, err := service.Write(ctx, WriteStatusInput{IssueID: issueID, Message: "which way?", Kind: "question"})
	if err != nil {
		t.Fatalf("write question: %v", err)
	}

	entries, err := service.ListRecentByKind(ctx, []string{"blocker", "question"}, 10)
	if err != nil {
		t.Fatalf("list recent by kind: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (%+v)", len(entries), entries)
	}
	// Most recent first.
	if entries[0].ID != question.ID || entries[1].ID != blocker.ID {
		t.Fatalf("entries order = [%d %d], want [%d %d]", entries[0].ID, entries[1].ID, question.ID, blocker.ID)
	}
	for _, entry := range entries {
		if entry.Kind != "blocker" && entry.Kind != "question" {
			t.Fatalf("unexpected kind %q in filtered list", entry.Kind)
		}
	}
}
