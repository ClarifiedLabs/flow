package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestBulkMoveWorkItemsAPIOrderAndAtomicValidation(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	oldParent, err := fixture.Bundle.Epics.Create(ctx, coordinator.CreateEpicInput{Title: "Old parent", CreatedBy: coordinator.ActorHuman})
	if err != nil {
		t.Fatalf("create old parent: %v", err)
	}
	newParent, err := fixture.Bundle.Epics.Create(ctx, coordinator.CreateEpicInput{Title: "New parent", CreatedBy: coordinator.ActorHuman})
	if err != nil {
		t.Fatalf("create new parent: %v", err)
	}
	first, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "First", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatalf("create first task: %v", err)
	}
	second, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Second", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}

	path := "/v2/projects/" + fixture.Project.ID + "/work-items/parents"
	var moved contract.MoveWorkItemsResponse
	doJSONRequest(t, fixture.Server, http.MethodPatch, path, contract.MoveWorkItemsRequest{
		ItemIDs: []string{second.ID, first.ID}, ParentItemID: newParent.ID,
	}, http.StatusOK, &moved)
	if len(moved.Items) != 2 || moved.Items[0].ID != second.ID || moved.Items[1].ID != first.ID {
		t.Fatalf("moved items = %+v, want request order [%s %s]", moved.Items, second.ID, first.ID)
	}
	for _, item := range moved.Items {
		if item.ParentItemID != newParent.ID {
			t.Fatalf("moved item %s parent = %q, want %q", item.ID, item.ParentItemID, newParent.ID)
		}
	}

	var rejected contract.ErrorResponse
	doJSONRequest(t, fixture.Server, http.MethodPatch, path, contract.MoveWorkItemsRequest{
		ItemIDs: []string{first.ID, "missing-item", second.ID}, ParentItemID: oldParent.ID,
	}, http.StatusUnprocessableEntity, &rejected)
	if rejected.Error.Code != "work_item_move_invalid" || len(rejected.Error.Issues) != 1 {
		t.Fatalf("validation error = %+v, want one structured work_item_move_invalid issue", rejected.Error)
	}
	issue := rejected.Error.Issues[0]
	if issue.ItemID != "missing-item" || issue.Code != "item_not_found" || issue.Message == "" {
		t.Fatalf("validation issue = %+v, want missing-item item_not_found with a message", issue)
	}
	for _, id := range []string{first.ID, second.ID} {
		item, getErr := fixture.Bundle.WorkItems.Get(ctx, id)
		if getErr != nil {
			t.Fatalf("get %s after rejected move: %v", id, getErr)
		}
		if item.ParentItemID != newParent.ID {
			t.Fatalf("%s parent after rejected move = %q, want %q", id, item.ParentItemID, newParent.ID)
		}
	}
}

func TestBulkMoveWorkItemsAPITaskBoundConsoleSingleton(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	oldParent, err := fixture.Bundle.Epics.Create(ctx, coordinator.CreateEpicInput{Title: "Old parent", CreatedBy: coordinator.ActorHuman})
	if err != nil {
		t.Fatalf("create old parent: %v", err)
	}
	newParent, err := fixture.Bundle.Epics.Create(ctx, coordinator.CreateEpicInput{Title: "New parent", CreatedBy: coordinator.ActorHuman})
	if err != nil {
		t.Fatalf("create new parent: %v", err)
	}
	bound, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Bound task", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatalf("create bound task: %v", err)
	}
	other, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Other task", ParentItemID: oldParent.ID})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token: "bulk-move-bound-console", Scope: coordinator.TokenScopeConsole, Subject: "c-bulk-move",
		ProjectID: &fixture.Project.ID, SourceTaskID: &bound.ID,
	}); err != nil {
		t.Fatalf("store task-bound console token: %v", err)
	}

	path := "/v2/projects/" + fixture.Project.ID + "/work-items/parents"
	var moved contract.MoveWorkItemsResponse
	doJSONRequestAs(t, fixture.Server, "bulk-move-bound-console", http.MethodPatch, path, contract.MoveWorkItemsRequest{
		ItemIDs: []string{bound.ID}, ParentItemID: newParent.ID,
	}, http.StatusOK, &moved)
	if len(moved.Items) != 1 || moved.Items[0].ID != bound.ID || moved.Items[0].ParentItemID != newParent.ID {
		t.Fatalf("singleton move = %+v, want bound task %s under %s", moved.Items, bound.ID, newParent.ID)
	}

	var forbidden contract.ErrorResponse
	doJSONRequestAs(t, fixture.Server, "bulk-move-bound-console", http.MethodPatch, path, contract.MoveWorkItemsRequest{
		ItemIDs: []string{bound.ID, other.ID}, ParentItemID: newParent.ID,
	}, http.StatusForbidden, &forbidden)
	if forbidden.Error.Code != "forbidden" {
		t.Fatalf("multi-item error = %+v, want forbidden", forbidden.Error)
	}
}
