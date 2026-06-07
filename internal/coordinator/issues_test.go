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
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
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
	if issue.ScheduleState != ScheduleBacklog {
		t.Fatalf("ScheduleState = %q, want backlog", issue.ScheduleState)
	}
	if issue.TriageState != TriageAccepted {
		t.Fatalf("TriageState = %q, want accepted", issue.TriageState)
	}
	if issue.CreatedBy != ActorHuman {
		t.Fatalf("CreatedBy = %q, want human", issue.CreatedBy)
	}
	if issue.AgentHarness != flowharness.Codex {
		t.Fatalf("AgentHarness = %q, want codex", issue.AgentHarness)
	}
	if issue.PlanMode {
		t.Fatal("PlanMode = true, want default false")
	}
	if _, err := service.ScheduleIssue(ctx, issue.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
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
	if reopenedIssue.ScheduleState != ScheduleUpNext {
		t.Fatalf("reopened ScheduleState = %q, want up_next", reopenedIssue.ScheduleState)
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
		{Title: "invalid schedule", ScheduleState: ScheduleState("later")},
		{Title: "closed create", ScheduleState: ScheduleClosed},
		{Title: "agent without session", CreatedBy: ActorAgent},
		{Title: "triage up next", TriageState: TriagePending, ScheduleState: ScheduleUpNext},
		{Title: "unknown harness", AgentHarness: "opencode"},
	}

	for _, input := range cases {
		if _, err := service.CreateIssue(ctx, input); err == nil {
			t.Fatalf("CreateIssue(%+v) succeeded, want error", input)
		}
	}
}

func TestCreateIssueAcceptsHarnessAgent(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	issue, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:        "Harness agent",
		AgentHarness: flowharness.Harness,
	})
	if err != nil {
		t.Fatalf("CreateIssue with harness agent: %v", err)
	}
	if issue.AgentHarness != flowharness.Harness {
		t.Fatalf("AgentHarness = %q, want %q", issue.AgentHarness, flowharness.Harness)
	}
}

func TestIssueHarnessArgsPersistAndPatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "flow.db")
	store, service := newIssueService(t, dbPath)

	issue, err := service.CreateIssue(ctx, CreateIssueInput{
		Title: "Args issue",
		HarnessArgs: flowharness.Args{
			Codex:  []string{"--model", "gpt-5"},
			Claude: []string{"--model", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if got := issue.HarnessArgs.Codex; len(got) != 2 || got[1] != "gpt-5" {
		t.Fatalf("created codex args = %#v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := flowdb.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()
	reopenedService := NewIssueService(reopened.DB())
	reopenedIssue, err := reopenedService.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get reopened issue: %v", err)
	}
	if got := reopenedIssue.HarnessArgs.Claude; len(got) != 2 || got[0] != "--model" || got[1] != "sonnet" {
		t.Fatalf("reopened claude args = %#v", got)
	}

	harnessArgs := []string{"--profile", "review"}
	clearCodex := []string{}
	edited, err := reopenedService.EditIssue(ctx, issue.ID, EditIssueInput{
		HarnessArgs: &flowharness.ArgsPatch{
			Codex:   &clearCodex,
			Harness: &harnessArgs,
		},
	})
	if err != nil {
		t.Fatalf("edit issue harness args: %v", err)
	}
	if len(edited.HarnessArgs.Codex) != 0 {
		t.Fatalf("codex args = %#v, want cleared", edited.HarnessArgs.Codex)
	}
	if got := edited.HarnessArgs.Claude; len(got) != 2 || got[1] != "sonnet" {
		t.Fatalf("claude args changed unexpectedly: %#v", got)
	}
	if got := edited.HarnessArgs.Harness; len(got) != 2 || got[1] != "review" {
		t.Fatalf("harness args = %#v", got)
	}
}

func TestIssueHarnessArgsRejectManagedFlags(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	if _, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:       "Bad args",
		HarnessArgs: flowharness.Args{Codex: []string{"-c", "hooks.Stop=[]"}},
	}); err == nil {
		t.Fatal("CreateIssue accepted managed codex hook config")
	}
	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Patch target"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	claudeArgs := []string{"--settings", "/tmp/settings.json"}
	if _, err := service.EditIssue(ctx, issue.ID, EditIssueInput{
		HarnessArgs: &flowharness.ArgsPatch{Claude: &claudeArgs},
	}); err == nil {
		t.Fatal("EditIssue accepted managed claude settings flag")
	}
}

func TestEditScheduleCloseAndTriageTransitions(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Original"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	renamed := "Renamed"
	priority := 5
	autoMerge := true
	planMode := true
	agentHarness := flowharness.Claude
	edited, err := service.EditIssue(ctx, issue.ID, EditIssueInput{
		Title:        &renamed,
		Priority:     &priority,
		AutoMerge:    &autoMerge,
		PlanMode:     &planMode,
		AgentHarness: &agentHarness,
	})
	if err != nil {
		t.Fatalf("edit issue: %v", err)
	}
	if edited.Title != renamed || edited.Priority != priority || !edited.AutoMerge || !edited.PlanMode || edited.AgentHarness != flowharness.Claude {
		t.Fatalf("edited issue mismatch: %+v", edited)
	}
	invalidHarness := "opencode"
	if _, err := service.EditIssue(ctx, issue.ID, EditIssueInput{AgentHarness: &invalidHarness}); err == nil {
		t.Fatal("edited issue with invalid agent harness, want error")
	}

	scheduled, err := service.ScheduleIssue(ctx, issue.ID, ScheduleUpNext)
	if err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	if scheduled.ScheduleState != ScheduleUpNext {
		t.Fatalf("ScheduleState = %q, want up_next", scheduled.ScheduleState)
	}

	closed, err := service.CloseIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("close issue: %v", err)
	}
	if closed.ScheduleState != ScheduleClosed {
		t.Fatalf("ScheduleState = %q, want closed", closed.ScheduleState)
	}
	if closed.ClosedAt == nil {
		t.Fatal("ClosedAt is nil")
	}

	sessionID := "s-agent"
	triage, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:              "Agent discovery",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &sessionID,
	})
	if err != nil {
		t.Fatalf("create triage issue: %v", err)
	}
	if _, err := service.ScheduleIssue(ctx, triage.ID, ScheduleUpNext); err == nil {
		t.Fatal("scheduled triage issue, want error")
	}
	accepted, err := service.AcceptTriage(ctx, triage.ID)
	if err != nil {
		t.Fatalf("accept triage: %v", err)
	}
	if accepted.TriageState != TriageAccepted {
		t.Fatalf("TriageState = %q, want accepted", accepted.TriageState)
	}

	rejectSessionID := "s-agent-reject"
	rejected, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:              "Rejected discovery",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &rejectSessionID,
	})
	if err != nil {
		t.Fatalf("create rejected candidate: %v", err)
	}
	rejected, err = service.RejectTriage(ctx, rejected.ID)
	if err != nil {
		t.Fatalf("reject triage: %v", err)
	}
	if rejected.TriageState != TriageRejected || rejected.ScheduleState != ScheduleClosed {
		t.Fatalf("rejected issue state mismatch: %+v", rejected)
	}
}

func TestSetIssueStateReopensClosedIssue(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	issue, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Accidentally closed"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	closed, err := service.CloseIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("close issue: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Fatal("closed ClosedAt is nil")
	}

	reopened, err := service.SetIssueState(ctx, issue.ID, IssueStateBacklog)
	if err != nil {
		t.Fatalf("set issue state backlog: %v", err)
	}
	if reopened.ScheduleState != ScheduleBacklog || reopened.TriageState != TriageAccepted {
		t.Fatalf("reopened issue state = %s/%s, want backlog/accepted", reopened.ScheduleState, reopened.TriageState)
	}
	if reopened.ClosedAt != nil {
		t.Fatalf("reopened ClosedAt = %v, want nil", reopened.ClosedAt)
	}

	triage, err := service.SetIssueState(ctx, issue.ID, IssueStateTriage)
	if err != nil {
		t.Fatalf("set issue state triage: %v", err)
	}
	if triage.ScheduleState != ScheduleBacklog || triage.TriageState != TriagePending || triage.ClosedAt != nil {
		t.Fatalf("triage issue state mismatch: %+v", triage)
	}

	rejected, err := service.SetIssueState(ctx, issue.ID, IssueStateRejected)
	if err != nil {
		t.Fatalf("set issue state rejected: %v", err)
	}
	if rejected.ScheduleState != ScheduleClosed || rejected.TriageState != TriageRejected || rejected.ClosedAt == nil {
		t.Fatalf("rejected issue state mismatch: %+v", rejected)
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

func TestBoardDerivationPlacesIssuesInExpectedLanes(t *testing.T) {
	ctx := context.Background()
	_, service := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))

	backlog, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Backlog"})
	if err != nil {
		t.Fatalf("create backlog: %v", err)
	}
	upNext, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Up next"})
	if err != nil {
		t.Fatalf("create up next: %v", err)
	}
	if _, err := service.ScheduleIssue(ctx, upNext.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule up next: %v", err)
	}

	sessionID := "s-triage"
	triage, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:              "Needs triage",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &sessionID,
	})
	if err != nil {
		t.Fatalf("create triage: %v", err)
	}

	acceptedSessionID := "s-accepted"
	acceptedFromTriage, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:              "Accepted discovery",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &acceptedSessionID,
	})
	if err != nil {
		t.Fatalf("create accepted from triage: %v", err)
	}
	if _, err := service.AcceptTriage(ctx, acceptedFromTriage.ID); err != nil {
		t.Fatalf("accept triage: %v", err)
	}

	rejectedSessionID := "s-rejected"
	rejected, err := service.CreateIssue(ctx, CreateIssueInput{
		Title:              "Rejected discovery",
		CreatedBy:          ActorAgent,
		CreatedBySessionID: &rejectedSessionID,
	})
	if err != nil {
		t.Fatalf("create rejected: %v", err)
	}
	if _, err := service.RejectTriage(ctx, rejected.ID); err != nil {
		t.Fatalf("reject triage: %v", err)
	}

	blocker, blocked := createTwoIssues(t, service, "Blocker", "Blocked")
	if err := service.LinkIssues(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	blockedUpNext, err := service.CreateIssue(ctx, CreateIssueInput{Title: "Blocked up next"})
	if err != nil {
		t.Fatalf("create blocked up next: %v", err)
	}
	if _, err := service.ScheduleIssue(ctx, blockedUpNext.ID, ScheduleUpNext); err != nil {
		t.Fatalf("schedule blocked up next: %v", err)
	}
	if err := service.LinkIssues(ctx, blocker.ID, blockedUpNext.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocked up next: %v", err)
	}

	result, err := service.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board: %v", err)
	}
	assertIssueIDs(t, result.Board.Backlog, []string{backlog.ID, triage.ID, acceptedFromTriage.ID, blocker.ID})
	assertIssueIDs(t, result.Board.UpNext, []string{upNext.ID})
	assertIssueIDs(t, result.Board.InProgress, []string{})
	assertIssueIDs(t, result.Board.NeedsAttention, []string{blocked.ID, blockedUpNext.ID})
	wantStates := map[string]LaneState{
		backlog.ID:            LaneStateBacklog,
		upNext.ID:             LaneStateUpNext,
		triage.ID:             LaneStateTriage,
		acceptedFromTriage.ID: LaneStateBacklog,
		blocker.ID:            LaneStateBacklog,
		blocked.ID:            LaneStateBacklog,
		blockedUpNext.ID:      LaneStateUpNext,
	}
	for id, want := range wantStates {
		if got := result.LaneStates[id]; got != want {
			t.Errorf("lane state[%s] = %q, want %q", id, got, want)
		}
	}
	if _, ok := result.LaneStates[rejected.ID]; ok {
		t.Errorf("rejected issue %s appears on the board", rejected.ID)
	}
	assertBlockedIDs(t, result.BlockedIDs, []string{blocked.ID, blockedUpNext.ID})

	if _, err := service.CloseIssue(ctx, blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	result, err = service.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board after close: %v", err)
	}
	assertBlockedIDs(t, result.BlockedIDs, []string{})
	assertIssueIDs(t, result.Board.Backlog, []string{backlog.ID, triage.ID, acceptedFromTriage.ID, blocked.ID})
	assertIssueIDs(t, result.Board.UpNext, []string{upNext.ID, blockedUpNext.ID})
	assertIssueIDs(t, result.Board.NeedsAttention, []string{})
}

func TestLaneStateForPhaseMapsBoardProjection(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		state LaneState
		ok    bool
		lane  string
	}{
		{name: "backlog", phase: PhaseBacklog, state: LaneStateBacklog, ok: true, lane: "backlog"},
		{name: "triage", phase: PhaseTriage, state: LaneStateTriage, ok: true, lane: "backlog"},
		{name: "up next", phase: PhaseUpNext, state: LaneStateUpNext, ok: true, lane: "up_next"},
		{name: "planning", phase: PhasePlanning, state: LaneStatePlanning, ok: true, lane: "in_progress"},
		{name: "authoring", phase: PhaseAuthoring, state: LaneStateInProgress, ok: true, lane: "in_progress"},
		{name: "critique", phase: PhaseCritique, state: LaneStateInReview, ok: true, lane: "in_progress"},
		{name: "acceptance", phase: PhaseAcceptance, state: LaneStateInReview, ok: true, lane: "in_progress"},
		{name: "approved", phase: PhaseApproved, state: LaneStateReadyToMerge, ok: true, lane: "needs_attention"},
		{name: "merged", phase: PhaseMergedClosed, ok: false},
		{name: "rejected", phase: PhaseRejectedClosed, ok: false},
		{name: "abandoned", phase: PhaseAbandoned, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, ok := laneStateForPhase(tt.phase)
			if ok != tt.ok {
				t.Fatalf("ok = %t, want %t", ok, tt.ok)
			}
			if !ok {
				return
			}
			if state != tt.state {
				t.Fatalf("state = %q, want %q", state, tt.state)
			}
			if lane := laneForState(state); lane != tt.lane {
				t.Fatalf("lane = %q, want %q", lane, tt.lane)
			}
		})
	}
}

func TestBoardProjectionUsesDerivedPhasesForReviewApprovedAndClosed(t *testing.T) {
	ctx := context.Background()
	store, issues := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))
	checks := NewCheckService(store.DB())

	underReview, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Under review"})
	if err != nil {
		t.Fatalf("create review issue: %v", err)
	}
	insertChangeForTest(t, store.DB(), underReview.ID, "ch-review", "issue/review", false)
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID: underReview.ID,
		Name:    "unit",
		Kind:    CheckKindCI,
		Verdict: CheckPending,
	}); err != nil {
		t.Fatalf("report pending check: %v", err)
	}

	approved, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Approved"})
	if err != nil {
		t.Fatalf("create approved issue: %v", err)
	}
	insertChangeForTest(t, store.DB(), approved.ID, "ch-approved", "issue/approved", false)
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID: approved.ID,
		Name:    "unit",
		Kind:    CheckKindCI,
		Verdict: CheckSatisfied,
	}); err != nil {
		t.Fatalf("report satisfied check: %v", err)
	}

	rejected, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Rejected"})
	if err != nil {
		t.Fatalf("create rejected issue: %v", err)
	}
	if _, err := issues.RejectTriage(ctx, rejected.ID); err != nil {
		t.Fatalf("reject issue: %v", err)
	}

	merged, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Merged"})
	if err != nil {
		t.Fatalf("create merged issue: %v", err)
	}
	insertChangeForTest(t, store.DB(), merged.ID, "ch-merged", "issue/merged", true)
	if _, err := issues.CloseIssue(ctx, merged.ID); err != nil {
		t.Fatalf("close merged issue: %v", err)
	}

	abandoned, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Abandoned"})
	if err != nil {
		t.Fatalf("create abandoned issue: %v", err)
	}
	if _, err := issues.CloseIssue(ctx, abandoned.ID); err != nil {
		t.Fatalf("close abandoned issue: %v", err)
	}

	result, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board: %v", err)
	}
	assertIssueIDs(t, result.Board.InProgress, []string{underReview.ID})
	assertIssueIDs(t, result.Board.NeedsAttention, []string{approved.ID})
	if got := result.LaneStates[underReview.ID]; got != LaneStateInReview {
		t.Fatalf("under review lane state = %q, want in_review", got)
	}
	if got := result.LaneStates[approved.ID]; got != LaneStateReadyToMerge {
		t.Fatalf("approved lane state = %q, want ready_to_merge", got)
	}
	if got := result.WaitReasons[approved.ID]; got != WaitReasonManualMerge {
		t.Fatalf("approved wait reason = %q, want manual_merge", got)
	}
	for _, id := range []string{rejected.ID, merged.ID, abandoned.ID} {
		if _, ok := result.LaneStates[id]; ok {
			t.Fatalf("closed issue %s appeared in board lane states", id)
		}
	}
}

func TestBoardResultRoutesPendingHumanReviewToNeedsAttention(t *testing.T) {
	ctx := context.Background()
	store, issues := newIssueService(t, filepath.Join(t.TempDir(), "flow.db"))
	checks := NewCheckService(store.DB())

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Human review"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	insertChangeForTest(t, store.DB(), issue.ID, "ch-human-review", "issue/human-review", false)
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID: issue.ID,
		Name:    "reviewer",
		Kind:    CheckKindReviewer,
		Verdict: CheckSatisfied,
	}); err != nil {
		t.Fatalf("report reviewer check: %v", err)
	}
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID: issue.ID,
		Name:    "human-review",
		Kind:    CheckKindHuman,
		Verdict: CheckPending,
	}); err != nil {
		t.Fatalf("report human review check: %v", err)
	}

	result, err := issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("derive board: %v", err)
	}
	assertIssueIDs(t, result.Board.InProgress, []string{})
	assertIssueIDs(t, result.Board.NeedsAttention, []string{issue.ID})
	if got := result.LaneStates[issue.ID]; got != LaneStateInReview {
		t.Fatalf("lane state = %q, want in_review", got)
	}
	if got := result.WaitReasons[issue.ID]; got != WaitReasonHumanReview {
		t.Fatalf("wait reason = %q, want human_review", got)
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
