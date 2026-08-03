package coordinator

import (
	"context"
	"errors"
	"testing"
)

func TestWorkItemMoveManyAtomicValidationAndOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	items := NewWorkItemService(fixture.store.DB(), fixture.project.ID)
	epics := NewEpicService(fixture.store.DB(), fixture.project.ID, items)
	tasks := NewTaskService(fixture.store.DB(), fixture.project.ID)

	oldParent, err := epics.Create(ctx, CreateEpicInput{Title: "Old", CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	newParent, err := epics.Create(ctx, CreateEpicInput{Title: "New", CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	first, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "First", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Second", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatal(err)
	}

	_, err = items.MoveMany(ctx, []string{second.ID, "missing-item", first.ID}, newParent.ID, ActorHuman)
	var validation *WorkItemMoveValidationError
	if !errors.As(err, &validation) || !hasMoveIssue(validation.Issues, "missing-item", "item_not_found") {
		t.Fatalf("MoveMany error = %#v, want item_not_found validation issue", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		item, getErr := items.Get(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.ParentItemID != oldParent.ID {
			t.Fatalf("%s parent after rejected move = %q, want %q", id, item.ParentItemID, oldParent.ID)
		}
	}

	moved, err := items.MoveMany(ctx, []string{second.ID, first.ID}, newParent.ID, ActorHuman)
	if err != nil {
		t.Fatalf("MoveMany: %v", err)
	}
	if len(moved) != 2 || moved[0].ID != second.ID || moved[1].ID != first.ID {
		t.Fatalf("MoveMany result order = %+v, want [%s %s]", moved, second.ID, first.ID)
	}
	for _, item := range moved {
		if item.ParentItemID != newParent.ID {
			t.Fatalf("%s parent = %q, want %q", item.ID, item.ParentItemID, newParent.ID)
		}
	}
}

func TestWorkItemMoveManyRejectsCyclesAndInvalidParents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	items := NewWorkItemService(fixture.store.DB(), fixture.project.ID)
	epics := NewEpicService(fixture.store.DB(), fixture.project.ID, items)
	tasks := NewTaskService(fixture.store.DB(), fixture.project.ID)

	ancestor, err := epics.Create(ctx, CreateEpicInput{Title: "Ancestor", CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	descendant, err := epics.Create(ctx, CreateEpicInput{Title: "Descendant", ParentItemID: ancestor.ID, CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	assertMoveIssue(t, items, []string{ancestor.ID}, descendant.ID, "parent_descendant")
	assertMoveIssue(t, items, []string{ancestor.ID, descendant.ID}, "", "selected_ancestor_descendant")

	leaf, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Leaf"})
	if err != nil {
		t.Fatal(err)
	}
	assertMoveIssue(t, items, []string{ancestor.ID}, leaf.ID, "parent_not_container")

	closed, err := epics.Create(ctx, CreateEpicInput{Title: "Closed", CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB().ExecContext(ctx, `UPDATE epics SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, closed.ID); err != nil {
		t.Fatal(err)
	}
	assertMoveIssue(t, items, []string{leaf.ID}, closed.ID, "parent_closed")

	blockerParent, err := epics.Create(ctx, CreateEpicInput{Title: "Blocking parent", CreatedBy: ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	if err := items.Link(ctx, blockerParent.ID, leaf.ID, RelationBlocks, ActorHuman); err != nil {
		t.Fatal(err)
	}
	assertMoveIssue(t, items, []string{leaf.ID}, blockerParent.ID, "dependency_cycle")
}

func assertMoveIssue(t *testing.T, items *WorkItemService, ids []string, parentID, code string) {
	t.Helper()
	_, err := items.MoveMany(context.Background(), ids, parentID, ActorHuman)
	var validation *WorkItemMoveValidationError
	if !errors.As(err, &validation) || !hasMoveIssue(validation.Issues, "", code) {
		t.Fatalf("MoveMany(%v, %q) error = %#v, want issue %q", ids, parentID, err, code)
	}
}

func hasMoveIssue(issues []WorkItemMoveIssue, itemID, code string) bool {
	for _, issue := range issues {
		if issue.Code == code && (itemID == "" || issue.ItemID == itemID) {
			return true
		}
	}
	return false
}
