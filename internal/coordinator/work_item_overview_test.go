package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkItemOverviewRollsUpMixedNesting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	items := NewWorkItemService(store.DB(), "p-test")
	epics := NewEpicService(store.DB(), "p-test", items)

	root, err := epics.Create(ctx, CreateEpicInput{Title: "root", CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create root epic: %v", err)
	}
	nested, err := epics.Create(ctx, CreateEpicInput{Title: "nested", ParentItemID: root.ID, CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create nested epic: %v", err)
	}
	direct, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "direct", ParentItemID: root.ID})
	if err != nil {
		t.Fatalf("create direct task: %v", err)
	}
	indirect, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "indirect", ParentItemID: nested.ID})
	if err != nil {
		t.Fatalf("create indirect task: %v", err)
	}

	overviews, err := items.Overview(ctx, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	byID := make(map[string]WorkItemOverview, len(overviews))
	for _, overview := range overviews {
		byID[overview.Item.ID] = overview
	}
	got := byID[root.ID]
	if got.Rollup.DirectChildren != (WorkItemDirectChildren{Total: 2}) {
		t.Fatalf("root direct children = %+v, want two open immediate children", got.Rollup.DirectChildren)
	}
	if got.Rollup.DescendantTasks != (WorkItemDescendantTasks{Total: 2, Unscheduled: 2}) {
		t.Fatalf("root descendant tasks = %+v, want direct and nested tasks", got.Rollup.DescendantTasks)
	}
	if len(got.DescendantTaskIDs) != 2 || got.DescendantTaskIDs[0] != direct.ID || got.DescendantTaskIDs[1] != indirect.ID {
		t.Fatalf("root descendant task ids = %v, want [%s %s]", got.DescendantTaskIDs, direct.ID, indirect.ID)
	}
	if nestedRollup := byID[nested.ID].Rollup; nestedRollup.DirectChildren.Total != 1 || nestedRollup.DescendantTasks.Total != 1 {
		t.Fatalf("nested rollup = %+v, want one direct descendant task", nestedRollup)
	}
}

func TestAddDescendantTaskLifecycleRollup(t *testing.T) {
	t.Parallel()
	var got WorkItemDescendantTasks
	for _, item := range []WorkItemSummary{
		{State: WorkItemState{}},
		{State: WorkItemState{Status: "scheduled"}},
		{State: WorkItemState{Status: "in_progress"}},
		{UnresolvedBlockers: 1, State: WorkItemState{Status: "paused"}},
		{State: WorkItemState{Status: "done", Terminal: true, Successful: true}},
		{State: WorkItemState{Status: "done", Terminal: true}},
	} {
		addDescendantTask(&got, item)
	}
	want := WorkItemDescendantTasks{
		Total: 6, Unscheduled: 1, Scheduled: 1, InProgress: 1, OtherOpen: 1,
		SuccessfulTerminal: 1, UnsuccessfulTerminal: 1, EffectivelyBlocked: 1,
	}
	if got != want {
		t.Fatalf("lifecycle rollup = %+v, want %+v", got, want)
	}
}

func TestWorkItemContextOrdersAncestorsAndReportsBlockerProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, tasks := newTaskService(t, filepath.Join(t.TempDir(), "flow.db"))
	items := NewWorkItemService(store.DB(), "p-test")
	epics := NewEpicService(store.DB(), "p-test", items)

	root, err := epics.Create(ctx, CreateEpicInput{Title: "root", CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create root epic: %v", err)
	}
	parent, err := epics.Create(ctx, CreateEpicInput{Title: "parent", ParentItemID: root.ID, CompletionPolicy: EpicManual})
	if err != nil {
		t.Fatalf("create parent epic: %v", err)
	}
	child, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "child", ParentItemID: parent.ID})
	if err != nil {
		t.Fatalf("create child task: %v", err)
	}
	blocker, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "blocker"})
	if err != nil {
		t.Fatalf("create blocker task: %v", err)
	}
	if err := items.Link(ctx, blocker.ID, parent.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatalf("link inherited blocker: %v", err)
	}

	view, err := items.Context(ctx, child.ID)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if len(view.Ancestors) != 2 || view.Ancestors[0].ID != root.ID || view.Ancestors[1].ID != parent.ID {
		t.Fatalf("ancestors = %+v, want root-to-parent order [%s %s]", view.Ancestors, root.ID, parent.ID)
	}
	if len(view.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want inherited blocker", view.Blockers)
	}
	got := view.Blockers[0]
	if got.Item.ID != blocker.ID || got.Direct || got.ViaItem == nil || got.ViaItem.ID != parent.ID || got.Resolved {
		t.Fatalf("blocker provenance = %+v, want unresolved %s via %s", got, blocker.ID, parent.ID)
	}

	if _, err := items.Context(ctx, "t-p-test-9999"); !errors.Is(err, ErrWorkItemNotFound) {
		t.Fatalf("missing context error = %v, want ErrWorkItemNotFound", err)
	}
}
