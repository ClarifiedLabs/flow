package coordinator

import (
	"context"
	"testing"
)

// TestPreexistingDispositionDoesNotGate proves the scope rule end to end: a
// thread filed with the preexisting disposition is excluded from the open
// review-thread count, so it cannot flip a reviewer verdict to blocked or hold
// the review edge, while the default and introduced_by_change dispositions
// still block.
func TestPreexistingDispositionDoesNotGate(t *testing.T) {
	t.Parallel()
	store, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	checks := NewCheckService(store.DB())
	projectID := "p-test"

	thread := func(t *testing.T, disposition ReviewDisposition, body string) ReviewThread {
		t.Helper()
		created, err := threads.CreateThread(ctx, CreateThreadInput{
			ChangeID:        change.ID,
			AnchorCommitSHA: "abc123",
			FilePath:        "internal/app.go",
			Line:            42,
			Body:            body,
			Actor:           "reviewer:alice",
			Disposition:     disposition,
		})
		if err != nil {
			t.Fatalf("create thread (%s): %v", disposition, err)
		}
		if created.Disposition != disposition {
			t.Fatalf("stored disposition = %q, want %q", created.Disposition, disposition)
		}
		return created
	}

	preexisting := thread(t, DispositionPreexisting, "preexisting race")
	open, err := checks.countOpenReviewThreads(ctx, change.TaskID)
	if err != nil {
		t.Fatalf("count with preexisting: %v", err)
	}
	if open != 0 {
		t.Fatalf("open threads = %d, want 0 (preexisting does not gate)", open)
	}

	thread(t, DispositionDefault, "default blocks")
	open, err = checks.countOpenReviewThreads(ctx, change.TaskID)
	if err != nil {
		t.Fatalf("count with default: %v", err)
	}
	if open != 1 {
		t.Fatalf("open threads = %d, want 1 (default blocks)", open)
	}

	thread(t, DispositionIntroducedByChange, "introduced blocks")
	open, err = checks.countOpenReviewThreads(ctx, change.TaskID)
	if err != nil {
		t.Fatalf("count with introduced: %v", err)
	}
	if open != 2 {
		t.Fatalf("open threads = %d, want 2 (introduced blocks)", open)
	}
	_ = preexisting
	_ = projectID
}

// TestPreexistingThreadCreatesExactlyOneLinkedFollowUp proves the follow-up
// task is created once, carries provenance, and is linked related_to the
// source task; a comment-dedup retry of the same concern does not create a
// second task.
func TestPreexistingThreadCreatesExactlyOneLinkedFollowUp(t *testing.T) {
	t.Parallel()
	store, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()

	input := CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            42,
		Context:         "func run()",
		Body:            "This race predates the change.",
		Actor:           "reviewer:alice",
		Disposition:     DispositionPreexisting,
	}
	first, err := threads.CreateThread(ctx, input)
	if err != nil {
		t.Fatalf("create preexisting thread: %v", err)
	}

	// The dedup retry collapses onto the same thread.
	retry, err := threads.CreateThread(ctx, input)
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry thread id = %s, want %s", retry.ID, first.ID)
	}

	// Exactly one follow-up task exists for this thread, with provenance and a
	// related_to link to the source task.
	var followUps []Task
	rows, err := store.DB().QueryContext(ctx, `
SELECT id, title, body, COALESCE(source_task_id, ''), COALESCE(source_change_id, '')
FROM tasks WHERE source_change_id = ?`, change.ID)
	if err != nil {
		t.Fatalf("list follow-ups: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var task Task
		var sourceTask, sourceChange string
		if err := rows.Scan(&task.ID, &task.Title, &task.Body, &sourceTask, &sourceChange); err != nil {
			t.Fatalf("scan follow-up: %v", err)
		}
		task.SourceTaskID = &sourceTask
		task.SourceChangeID = &sourceChange
		followUps = append(followUps, task)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate follow-ups: %v", err)
	}
	if len(followUps) != 1 {
		t.Fatalf("follow-up tasks = %d, want exactly 1", len(followUps))
	}
	followUp := followUps[0]
	if followUp.SourceTaskID == nil || *followUp.SourceTaskID != change.TaskID {
		t.Fatalf("follow-up source task = %v, want %s", followUp.SourceTaskID, change.TaskID)
	}
	if followUp.SourceChangeID == nil || *followUp.SourceChangeID != change.ID {
		t.Fatalf("follow-up source change = %v, want %s", followUp.SourceChangeID, change.ID)
	}
	if !contains(followUp.Body, first.ID) || !contains(followUp.Body, "internal/app.go:42") {
		t.Fatalf("follow-up body lacks provenance:\n%s", followUp.Body)
	}
	if contains(followUp.Body, change.TaskID) == false {
		t.Fatalf("follow-up body lacks source task id:\n%s", followUp.Body)
	}

	// The related_to link exists between the source task and the follow-up.
	var linked int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE kind = 'related_to'
	AND ((source_item_id = ? AND target_item_id = ?) OR (source_item_id = ? AND target_item_id = ?))`,
		change.TaskID, followUp.ID, followUp.ID, change.TaskID).Scan(&linked); err != nil {
		t.Fatalf("count relation: %v", err)
	}
	if linked != 1 {
		t.Fatalf("related_to links = %d, want 1", linked)
	}
}

// TestPreexistingThreadFromSubmitReview proves the atomic submission path also
// files the disposition and schedules the follow-up.
func TestPreexistingThreadFromSubmitReview(t *testing.T) {
	t.Parallel()
	store, threads, change := newThreadServiceFixture(t)
	ctx := context.Background()
	threads.Checks = NewCheckService(store.DB())

	result, err := threads.SubmitReview(ctx, SubmitReviewInput{
		ChangeID:  change.ID,
		HeadSHA:   change.HeadSHA,
		Verdict:   "request_changes",
		CheckName: "human-review",
		Comments: []SubmitReviewComment{
			{FilePath: "internal/app.go", Line: 7, Body: "Preexisting concern.", Disposition: DispositionPreexisting},
			{FilePath: "internal/other.go", Line: 9, Body: "Introduced concern."},
		},
		Actor: "owner",
	})
	if err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if len(result.Threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(result.Threads))
	}
	var followUps int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM tasks WHERE source_change_id = ?`, change.ID).Scan(&followUps); err != nil {
		t.Fatalf("count follow-ups: %v", err)
	}
	if followUps != 1 {
		t.Fatalf("follow-ups = %d, want 1 (only the preexisting thread schedules one)", followUps)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
