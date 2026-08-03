package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestCreateAPIsConvertAtomicCrossKindRelations(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	makeExchangeHooksInert(t, fixture.Project.ExchangePath)
	seedAPIMain(t, fixture.Project.ExchangePath)

	existing, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "relation anchor"})
	if err != nil {
		t.Fatalf("create relation anchor: %v", err)
	}
	var epic contract.EpicResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost,
		"/v2/projects/"+fixture.Project.ID+"/epics",
		contract.CreateEpicRequest{Title: "API epic", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
			SourceIsNewItem: true, TargetItemID: existing.ID, Kind: string(coordinator.RelationBlocks),
		}}}, http.StatusCreated, &epic)

	var task taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "API task", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
			SourceItemID: epic.Epic.ID, TargetIsNewItem: true, Kind: string(coordinator.RelationRelatedTo),
		}}}, http.StatusCreated, &task)

	var feature contract.FeatureResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost,
		"/v2/projects/"+fixture.Project.ID+"/features",
		contract.CreateFeatureRequest{Title: "API feature", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
			SourceIsNewItem: true, TargetItemID: existing.ID, Kind: string(coordinator.RelationRelatedTo),
		}}}, http.StatusCreated, &feature)

	for _, check := range []struct {
		id, source, target string
		kind               coordinator.RelationKind
	}{
		{epic.Epic.ID, epic.Epic.ID, existing.ID, coordinator.RelationBlocks},
		{task.Task.ID, epic.Epic.ID, task.Task.ID, coordinator.RelationRelatedTo},
		{feature.Feature.ID, feature.Feature.ID, existing.ID, coordinator.RelationRelatedTo},
	} {
		relations, err := fixture.Bundle.WorkItems.Relations(ctx, check.id)
		if err != nil {
			t.Fatalf("relations for %s: %v", check.id, err)
		}
		found := false
		for _, relation := range relations {
			if relation.Source.ID == check.source && relation.Target.ID == check.target && relation.Kind == check.kind {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("relations for %s = %+v, want %s -[%s]-> %s", check.id, relations, check.source, check.kind, check.target)
		}
	}
}

func TestCreateAPIMalformedWorkItemRelationRollsBack(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	items, err := fixture.Bundle.WorkItems.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := len(items)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "must roll back", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
			SourceIsNewItem: true, TargetIsNewItem: true, Kind: string(coordinator.RelationRelatedTo),
		}}}, http.StatusBadRequest, nil)
	items, err = fixture.Bundle.WorkItems.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != before {
		t.Fatalf("work items after malformed create = %d, want %d", len(items), before)
	}
}

func TestBoundTaskCredentialsConstrainGenericCreateRelations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope coordinator.TokenScope
		valid func(string) contract.CreateWorkItemRelationRequest
	}{
		{name: "session", scope: coordinator.TokenScopeSession, valid: func(bound string) contract.CreateWorkItemRelationRequest {
			return contract.CreateWorkItemRelationRequest{SourceIsNewItem: true, TargetItemID: bound, Kind: string(coordinator.RelationRelatedTo)}
		}},
		{name: "console", scope: coordinator.TokenScopeConsole, valid: func(bound string) contract.CreateWorkItemRelationRequest {
			return contract.CreateWorkItemRelationRequest{SourceItemID: bound, TargetIsNewItem: true, Kind: string(coordinator.RelationRelatedTo)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTestFixture(t)
			ctx := context.Background()
			bound, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: tc.name + " bound task"})
			if err != nil {
				t.Fatal(err)
			}
			unrelated, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: tc.name + " unrelated task"})
			if err != nil {
				t.Fatal(err)
			}
			token := tc.name + "-generic-create-token"
			if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
				Token: token, Scope: tc.scope, Subject: tc.name + "-generic-create", ProjectID: &fixture.Project.ID, SourceTaskID: &bound.ID,
			}); err != nil {
				t.Fatal(err)
			}
			var created taskResponse
			doJSONRequestAs(t, fixture.Server, token, http.MethodPost, "/v2/tasks", createTaskRequest{
				Title: tc.name + " allowed generic relation", WorkItemRelations: []contract.CreateWorkItemRelationRequest{tc.valid(bound.ID)},
			}, http.StatusCreated, &created)
			relations, err := fixture.Bundle.WorkItems.Relations(ctx, created.Task.ID)
			if err != nil || len(relations) != 1 {
				t.Fatalf("allowed generic relations = %+v, err=%v", relations, err)
			}

			itemsBefore, err := fixture.Bundle.WorkItems.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			unrelatedBefore, err := fixture.Bundle.WorkItems.Relations(ctx, unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			doJSONRequestAs(t, fixture.Server, token, http.MethodPost, "/v2/tasks", createTaskRequest{
				Title: tc.name + " rejected generic relation", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
					SourceIsNewItem: true, TargetItemID: unrelated.ID, Kind: string(coordinator.RelationRelatedTo),
				}},
			}, http.StatusForbidden, nil)
			itemsAfter, err := fixture.Bundle.WorkItems.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			unrelatedAfter, err := fixture.Bundle.WorkItems.Relations(ctx, unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(itemsAfter) != len(itemsBefore) || len(unrelatedAfter) != len(unrelatedBefore) {
				t.Fatalf("rejected create committed state: items %d->%d, relations %d->%d", len(itemsBefore), len(itemsAfter), len(unrelatedBefore), len(unrelatedAfter))
			}
		})
	}
}

func TestFeatureCreateMapsMissingRelatedWorkItemToNotFound(t *testing.T) {
	fixture := newTestFixture(t)
	makeExchangeHooksInert(t, fixture.Project.ExchangePath)
	seedAPIMain(t, fixture.Project.ExchangePath)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost,
		"/v2/projects/"+fixture.Project.ID+"/features",
		contract.CreateFeatureRequest{Title: "missing relation feature", WorkItemRelations: []contract.CreateWorkItemRelationRequest{{
			SourceIsNewItem: true, TargetItemID: "task-missing", Kind: string(coordinator.RelationRelatedTo),
		}}}, http.StatusNotFound, nil)
}
