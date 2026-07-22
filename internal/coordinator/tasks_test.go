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

func TestCreateTaskAllocatesIDAndPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "flow.db")
	store, service := newTaskService(t, dbPath)

	task, err := service.CreateTask(ctx, CreateTaskInput{
		Title:    "Build task domain",
		Body:     "Persist tasks in SQLite.",
		Priority: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.ID != "t-test-0001" {
		t.Fatalf("task.ID = %q, want t-test-0001", task.ID)
	}
	if task.State != nil {
		t.Fatalf("State = %v, want unscheduled", task.State)
	}
	if task.CreatedBy != ActorHuman {
		t.Fatalf("CreatedBy = %q, want human", task.CreatedBy)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := flowdb.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()

	reopenedTask, err := NewTaskService(reopened.DB(), "p-test").GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get reopened task: %v", err)
	}
	if reopenedTask.State != nil {
		t.Fatalf("reopened State = %v, want unscheduled", reopenedTask.State)
	}
	if reopenedTask.Title != "Build task domain" {
		t.Fatalf("reopened Title = %q", reopenedTask.Title)
	}
	if reopenedTask.Body != "Persist tasks in SQLite." {
		t.Fatalf("reopened Body = %q", reopenedTask.Body)
	}
}

func TestConcurrentTaskCreationAllocatesUniqueIDs(t *testing.T) {
	ctx := context.Background()
	_, service := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	const taskCount = 80
	ids := make(chan string, taskCount)
	errs := make(chan error, taskCount)

	var wg sync.WaitGroup
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			task, err := service.CreateTask(ctx, CreateTaskInput{
				Title: "Concurrent task " + string(rune('A'+index%26)),
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- task.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatalf("create task concurrently: %v", err)
	}

	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate task id allocated: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != taskCount {
		t.Fatalf("created %d tasks, want %d", len(seen), taskCount)
	}

	for i := 1; i <= taskCount; i++ {
		id, err := formatTaskID("p-test", int64(i))
		if err != nil {
			t.Fatalf("format allocated task id: %v", err)
		}
		if !seen[id] {
			t.Fatalf("missing allocated task id %s", id)
		}
	}
}

func TestTaskAttachmentRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, service := newTaskService(t, filepath.Join(dir, "flow.db"))
	store := NewTaskAttachmentStore(filepath.Join(dir, "attachments"))
	task, err := service.CreateTask(ctx, CreateTaskInput{Title: "Attachment target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	attachment, err := service.CreateTaskAttachment(ctx, CreateTaskAttachmentInput{
		TaskID:      task.ID,
		Stage:       TaskAttachmentStageReviewer,
		Filename:    `screenshots\review.png`,
		ContentType: "image/png",
		CreatedBy:   ActorAgent,
		Reader:      strings.NewReader("png-data"),
	}, store)
	if err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	if attachment.ID != "att-0001" || attachment.TaskID != task.ID || attachment.Stage != TaskAttachmentStageReviewer {
		t.Fatalf("attachment identity = %+v", attachment)
	}
	if attachment.Filename != "review.png" || attachment.ContentType != "image/png" || attachment.SizeBytes != int64(len("png-data")) || attachment.CreatedBy != ActorAgent {
		t.Fatalf("attachment metadata = %+v", attachment)
	}

	attachments, err := service.ListTaskAttachments(ctx, task.ID)
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

func TestTaskAttachmentRejectsInvalidStage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, service := newTaskService(t, filepath.Join(dir, "flow.db"))
	store := NewTaskAttachmentStore(filepath.Join(dir, "attachments"))
	task, err := service.CreateTask(ctx, CreateTaskInput{Title: "Attachment target"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	_, err = service.CreateTaskAttachment(ctx, CreateTaskAttachmentInput{
		TaskID:   task.ID,
		Stage:    TaskAttachmentStage("qa"),
		Filename: "note.txt",
		Reader:   strings.NewReader("note"),
	}, store)
	if err == nil {
		t.Fatal("CreateTaskAttachment with invalid stage succeeded")
	}
}

func TestInvalidTaskMetadataIsRejected(t *testing.T) {
	ctx := context.Background()
	_, service := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	cases := []CreateTaskInput{
		{Title: ""},
		{Title: "negative priority", Priority: -1},
		{Title: "agent without session", CreatedBy: ActorAgent},
	}

	for _, input := range cases {
		if _, err := service.CreateTask(ctx, input); err == nil {
			t.Fatalf("CreateTask(%+v) succeeded, want error", input)
		}
	}
}

func TestTagsCanBeCreatedAppliedQueriedAndRejectDuplicates(t *testing.T) {
	ctx := context.Background()
	_, service := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	task, err := service.CreateTask(ctx, CreateTaskInput{Title: "Tagged task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
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
	if err := service.TagTask(ctx, task.ID, tag.ID, ActorHuman); err != nil {
		t.Fatalf("tag task: %v", err)
	}

	tags, err := service.TagsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("tags for task: %v", err)
	}
	if len(tags) != 1 || tags[0].Slug != "backend" {
		t.Fatalf("tags = %+v, want backend", tags)
	}

	filtered, err := service.ListTasks(ctx, TaskFilter{TagSlugs: []string{"backend"}})
	if err != nil {
		t.Fatalf("list tasks by tag: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != task.ID {
		t.Fatalf("filtered tasks = %+v, want %s", filtered, task.ID)
	}

	if err := service.UntagTask(ctx, task.ID, tag.ID); err != nil {
		t.Fatalf("untag task: %v", err)
	}
	tags, err = service.TagsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("tags after untag: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("tags after untag = %+v, want empty", tags)
	}
}

func TestTaskRelationsRejectCyclesAndDuplicateParents(t *testing.T) {
	ctx := context.Background()
	_, service := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))

	parentTree := createTasks(t, service, "parent", "child", "grandchild")
	parent, child, grandchild := parentTree[0], parentTree[1], parentTree[2]
	if err := service.LinkTasks(ctx, parent.ID, child.ID, RelationParentOf, ActorHuman); err != nil {
		t.Fatalf("link parent child: %v", err)
	}
	if err := service.LinkTasks(ctx, child.ID, grandchild.ID, RelationParentOf, ActorHuman); err != nil {
		t.Fatalf("link child grandchild: %v", err)
	}
	if err := service.LinkTasks(ctx, grandchild.ID, parent.ID, RelationParentOf, ActorHuman); err == nil {
		t.Fatal("parent_of cycle was accepted")
	}
	if err := service.LinkTasks(ctx, parent.ID, grandchild.ID, RelationParentOf, ActorHuman); err == nil {
		t.Fatal("second direct parent was accepted")
	}
	if err := service.LinkTasks(ctx, parent.ID, parent.ID, RelationRelatedTo, ActorHuman); err == nil {
		t.Fatal("self relation was accepted")
	}

	relations, err := service.RelationsForTask(ctx, child.ID)
	if err != nil {
		t.Fatalf("relations for task: %v", err)
	}
	if len(relations) != 2 {
		t.Fatalf("relations for child = %+v, want 2", relations)
	}

	blockTree := createTasks(t, service, "blocker", "blocked", "blocked grandchild")
	blocker, blocked, blockedGrandchild := blockTree[0], blockTree[1], blockTree[2]
	if err := service.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if err := service.LinkTasks(ctx, blocker.ID, blocked.ID, RelationBlocks, ActorHuman); err == nil {
		t.Fatal("duplicate blocks relation was accepted")
	}
	if err := service.LinkTasks(ctx, blocked.ID, blockedGrandchild.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link second blocker: %v", err)
	}
	if err := service.LinkTasks(ctx, blockedGrandchild.ID, blocker.ID, RelationBlocks, ActorHuman); err == nil {
		t.Fatal("blocks cycle was accepted")
	}

	if err := service.UnlinkTasks(ctx, blocked.ID, blockedGrandchild.ID, RelationBlocks); err != nil {
		t.Fatalf("unlink blocker: %v", err)
	}
	relations, err = service.RelationsForTask(ctx, blockedGrandchild.ID)
	if err != nil {
		t.Fatalf("relations after unlink: %v", err)
	}
	if len(relations) != 0 {
		t.Fatalf("relations after unlink = %+v, want empty", relations)
	}
}

func newTaskService(t *testing.T, dbPath string) (*flowdb.Store, *TaskService) {
	t.Helper()

	store, err := flowdb.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store, NewTaskService(store.DB(), "p-test")
}

func insertChangeForTest(t *testing.T, database *sql.DB, taskID string, changeID string, branch string, merged bool) {
	t.Helper()

	mergedAt := any(nil)
	if merged {
		mergedAt = "2026-01-01T00:00:00Z"
	}
	if _, err := database.ExecContext(context.Background(), `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?, ?)`,
		changeID,
		taskID,
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

func createTasks(t *testing.T, service *TaskService, titles ...string) []Task {
	t.Helper()

	tasks := make([]Task, len(titles))
	for i, title := range titles {
		task, err := service.CreateTask(context.Background(), CreateTaskInput{Title: title})
		if err != nil {
			t.Fatalf("create task %q: %v", title, err)
		}
		tasks[i] = task
	}

	return tasks
}

func createTwoTasks(t *testing.T, service *TaskService, firstTitle, secondTitle string) (Task, Task) {
	t.Helper()

	tasks := createTasks(t, service, firstTitle, secondTitle)
	return tasks[0], tasks[1]
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

func assertTaskIDs(t *testing.T, tasks []Task, want []string) {
	t.Helper()

	got := make([]string, len(tasks))
	for i, task := range tasks {
		got[i] = task.ID
	}
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("task ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("task ids = %v, want %v", got, want)
		}
	}
}
