package coordinator

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestCreateIssueAllocatesIDAndPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "flow.db")
	store, service := newIssueService(t, dbPath)

	issue, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:    "Build issue domain",
		Body:     "Persist issues in SQLite.",
		Priority: 3,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if issue.ID != "i-0001" {
		t.Fatalf("issue.ID = %q, want i-0001", issue.ID)
	}
	if issue.State != nil {
		t.Fatalf("State = %v, want unscheduled", issue.State)
	}
	if issue.CreatedBy != ActorHuman {
		t.Fatalf("CreatedBy = %q, want human", issue.CreatedBy)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := flowdb.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()

	reopenedIssue, err := NewIssueService(reopened.DB()).GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get reopened issue: %v", err)
	}
	if reopenedIssue.State != nil {
		t.Fatalf("reopened State = %v, want unscheduled", reopenedIssue.State)
	}
	if reopenedIssue.Title != "Build issue domain" {
		t.Fatalf("reopened Title = %q", reopenedIssue.Title)
	}
}

func TestConcurrentIssueCreationAllocatesUniqueIDs(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	const issueCount = 80
	ids := make(chan string, issueCount)
	errs := make(chan error, issueCount)

	var wg sync.WaitGroup
	for i := 0; i < issueCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			issue, err := service.CreateIssue(ctx, CreateIssueInput{
				Title: "Concurrent issue " + string(rune('A'+index%26)),
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- issue.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatalf("create issue concurrently: %v", err)
	}

	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate issue id allocated: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != issueCount {
		t.Fatalf("created %d issues, want %d", len(seen), issueCount)
	}

	for i := 1; i <= issueCount; i++ {
		id := formatIssueID(int64(i))
		if !seen[id] {
			t.Fatalf("missing allocated issue id %s", id)
		}
	}
}

func TestIssueAttachmentRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, service := newIssueService(t, filepath.Join(dir, "flow.db"))
	store := NewIssueAttachmentStore(filepath.Join(dir, "attachments"))
	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Attachment target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	attachment, err := service.CreateIssueAttachment(ctx, CreateIssueAttachmentInput{
		IssueID:     issue.ID,
		Stage:       IssueAttachmentStageReviewer,
		Filename:    `screenshots\review.png`,
		ContentType: "image/png",
		CreatedBy:   ActorAgent,
		Reader:      strings.NewReader("png-data"),
	}, store)
	if err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	if attachment.ID != "att-0001" || attachment.IssueID != issue.ID || attachment.Stage != IssueAttachmentStageReviewer {
		t.Fatalf("attachment identity = %+v", attachment)
	}
	if attachment.Filename != "review.png" || attachment.ContentType != "image/png" || attachment.SizeBytes != int64(len("png-data")) || attachment.CreatedBy != ActorAgent {
		t.Fatalf("attachment metadata = %+v", attachment)
	}

	attachments, err := service.ListIssueAttachments(ctx, issue.ID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(attachments) != 1 || attachments[0].ID != attachment.ID {
		t.Fatalf("attachments = %+v, want %s", attachments, attachment.ID)
	}

	reader, err := store.Open(attachment.StorageKey)
	if err != nil {
		t.Fatalf("open attachment: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if string(data) != "png-data" {
		t.Fatalf("attachment data = %q", string(data))
	}
}

func TestIssueAttachmentRejectsInvalidStage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, service := newIssueService(t, filepath.Join(dir, "flow.db"))
	store := NewIssueAttachmentStore(filepath.Join(dir, "attachments"))
	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Attachment target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	_, err = service.CreateIssueAttachment(ctx, CreateIssueAttachmentInput{
		IssueID:  issue.ID,
		Stage:    IssueAttachmentStage("qa"),
		Filename: "note.txt",
		Reader:   strings.NewReader("note"),
	}, store)
	if err == nil {
		t.Fatal("CreateIssueAttachment with invalid stage succeeded")
	}
}

func TestInvalidIssueMetadataIsRejected(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	cases := []CreateIssueInput{
		{Title: ""},
		{Title: "negative priority", Priority: -1},
		{Title: "agent without session", CreatedBy: ActorAgent},
	}

	for _, input := range cases {
		if _, err := service.CreateIssue(ctx, input); err == nil {
			t.Fatalf("CreateIssue(%+v) succeeded, want error", input)
		}
	}
}

func TestTagsCanBeCreatedAppliedQueriedAndRejectDuplicates(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Tagged issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	tag, err := service.CreateTag(ctx, CreateTagInput{
		Slug:      "backend",
		Name:      "Backend",
		CreatedBy: ActorHuman,
	})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if _, err := service.CreateTag(ctx, CreateTagInput{Slug: "backend", Name: "Duplicate"}); err == nil {
		t.Fatal("duplicate tag slug was accepted")
	}
	if _, err := service.CreateTag(ctx, CreateTagInput{Slug: "BadSlug", Name: "Bad"}); err == nil {
		t.Fatal("invalid tag slug was accepted")
	}
	if err := service.TagIssue(ctx, issue.ID, tag.ID, ActorHuman); err != nil {
		t.Fatalf("tag issue: %v", err)
	}

	tags, err := service.TagsForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("tags for issue: %v", err)
	}
	if len(tags) != 1 || tags[0].Slug != "backend" {
		t.Fatalf("tags = %+v, want backend", tags)
	}

	filtered, err := service.ListIssues(ctx, IssueFilter{TagSlugs: []string{"backend"}})
	if err != nil {
		t.Fatalf("list issues by tag: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != issue.ID {
		t.Fatalf("filtered issues = %+v, want %s", filtered, issue.ID)
	}

	if err := service.UntagIssue(ctx, issue.ID, tag.ID); err != nil {
		t.Fatalf("untag issue: %v", err)
	}
	tags, err = service.TagsForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("tags after untag: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("tags after untag = %+v, want empty", tags)
	}
}

func TestIssueRelationsRejectCyclesAndDuplicateParents(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	parentTree := createIssues(t, service, "parent", "child", "grandchild")
	parent, child, grandchild := parentTree[0], parentTree[1], parentTree[2]
	if err := service.LinkIssues(ctx, parent.ID, child.ID, RelationParentOf, ActorHuman); err != nil {
		t.Fatalf("link parent child: %v", err)
	}
	if err := service.LinkIssues(ctx, child.ID, grandchild.ID, RelationParentOf, ActorHuman); err != nil {
		t.Fatalf("link child grandchild: %v", err)
	}
	if err := service.LinkIssues(ctx, grandchild.ID, parent.ID, RelationParentOf, ActorHuman); err == nil {
		t.Fatal("parent_of cycle was accepted")
	}
	if err := service.LinkIssues(ctx, parent.ID, grandchild.ID, RelationParentOf, ActorHuman); err == nil {
		t.Fatal("second direct parent was accepted")
	}
	if err := service.LinkIssues(ctx, parent.ID, parent.ID, RelationRelatedTo, ActorHuman); err == nil {
		t.Fatal("self relation was accepted")
	}

	relations, err := service.RelationsForIssue(ctx, child.ID)
	if err != nil {
		t.Fatalf("relations for issue: %v", err)
	}
	if len(relations) != 2 {
		t.Fatalf("relations for child = %+v, want 2", relations)
	}

	blockTree := createIssues(t, service, "blocker", "blocked", "blocked grandchild")
	blocker, blocked, blockedGrandchild := blockTree[0], blockTree[1], blockTree[2]
	if err := service.LinkIssues(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if err := service.LinkIssues(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err == nil {
		t.Fatal("duplicate blocks relation was accepted")
	}
	if err := service.LinkIssues(ctx, blocked.ID, blockedGrandchild.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link second blocker: %v", err)
	}
	if err := service.LinkIssues(ctx, blockedGrandchild.ID, blocker.ID, RelationBlocks, ActorHuman); err == nil {
		t.Fatal("blocks cycle was accepted")
	}

	if err := service.UnlinkIssues(ctx, blocked.ID, blockedGrandchild.ID, RelationBlocks); err != nil {
		t.Fatalf("unlink blocker: %v", err)
	}
	relations, err = service.RelationsForIssue(ctx, blockedGrandchild.ID)
	if err != nil {
		t.Fatalf("relations after unlink: %v", err)
	}
	if len(relations) != 0 {
		t.Fatalf("relations after unlink = %+v, want empty", relations)
	}
}

func newIssueService(t *testing.T, dbPath string) (*flowdb.Store, *IssueService) {
	t.Helper()

	store, err := flowdb.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store, NewIssueService(store.DB())
}

func insertChangeForTest(t *testing.T, database *sql.DB, issueID string, changeID string, branch string, merged bool) {
	t.Helper()

	mergedAt := any(nil)
	if merged {
		mergedAt = "2026-01-01T00:00:00Z"
	}
	if _, err := database.ExecContext(context.Background(), `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?, ?)`,
		changeID,
		issueID,
		branch,
		"1111111111111111111111111111111111111111",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		mergedAt,
	); err != nil {
		t.Fatalf("insert change %s: %v", changeID, err)
	}
}

func createIssues(t *testing.T, service *IssueService, titles ...string) []Issue {
	t.Helper()

	issues := make([]Issue, len(titles))
	for i, title := range titles {
		issue, err := service.CreateIssue(context.Background(), CreateIssueInput{Title: title})
		if err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		issues[i] = issue
	}

	return issues
}

func createTwoIssues(t *testing.T, service *IssueService, firstTitle, secondTitle string) (Issue, Issue) {
	t.Helper()

	issues := createIssues(t, service, firstTitle, secondTitle)
	return issues[0], issues[1]
}

func assertBlockedIDs(t *testing.T, got, want []string) {
	t.Helper()

	got = append([]string(nil), got...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("blocked ids = %v, want %v", got, want)
	}
}

func assertIssueIDs(t *testing.T, issues []Issue, want []string) {
	t.Helper()

	got := make([]string, len(issues))
	for i, issue := range issues {
		got[i] = issue.ID
	}
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("issue ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("issue ids = %v, want %v", got, want)
		}
	}
}
