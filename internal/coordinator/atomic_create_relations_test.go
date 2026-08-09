package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

func TestAtomicCreateWorkItemRelationsAcrossKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	epics := NewEpicService(env.fixture.store.DB(), testProjectID, nil)
	items := NewWorkItemService(env.fixture.store.DB(), testProjectID)

	existing, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "existing task"})
	if err != nil {
		t.Fatalf("create existing task: %v", err)
	}
	epic, err := epics.Create(ctx, CreateEpicInput{Title: "related epic", WorkItemRelations: []CreateWorkItemRelationInput{{
		SourceIsNewItem: true, TargetItemID: existing.ID, Kind: RelationBlocks,
	}}})
	if err != nil {
		t.Fatalf("create epic with task relation: %v", err)
	}
	task, err := env.tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{Task: CreateTaskInput{Title: "related task"}, WorkItemRelations: []CreateWorkItemRelationInput{{
		SourceItemID: epic.ID, TargetIsNewItem: true, Kind: RelationRelatedTo,
	}}})
	if err != nil {
		t.Fatalf("create task with epic relation: %v", err)
	}
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "related feature", WorkItemRelations: []CreateWorkItemRelationInput{{
		SourceIsNewItem: true, TargetItemID: existing.ID, Kind: RelationRelatedTo,
	}}})
	if err != nil {
		t.Fatalf("create feature with task relation: %v", err)
	}

	for _, check := range []struct {
		id, source, target string
		kind               RelationKind
	}{
		{epic.ID, epic.ID, existing.ID, RelationBlocks},
		{task.ID, epic.ID, task.ID, RelationRelatedTo},
		{feature.ID, feature.ID, existing.ID, RelationRelatedTo},
	} {
		relations, err := items.Relations(ctx, check.id)
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

func TestMalformedCreateWorkItemRelationRollsBackEverySubtype(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	epics := NewEpicService(env.fixture.store.DB(), testProjectID, nil)
	malformed := []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetIsNewItem: true, Kind: RelationRelatedTo}}

	var before int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	attempts := []struct {
		name string
		run  func() error
	}{
		{"task", func() error {
			_, err := env.tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{Task: CreateTaskInput{Title: "bad task"}, WorkItemRelations: malformed})
			return err
		}},
		{"epic", func() error {
			_, err := epics.Create(ctx, CreateEpicInput{Title: "bad epic", WorkItemRelations: malformed})
			return err
		}},
		{"feature", func() error {
			_, err := env.features.Create(ctx, CreateFeatureInput{Title: "bad feature", WorkItemRelations: malformed})
			return err
		}},
	}
	for _, attempt := range attempts {
		t.Run(attempt.name, func(t *testing.T) {
			if err := attempt.run(); err == nil {
				t.Fatal("malformed relation unexpectedly succeeded")
			}
			var after int
			if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("work item count = %d, want %d", after, before)
			}
		})
	}
}

func TestCreateRejectsLegacyAndGenericParentDeclarationsAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	epics := NewEpicService(env.fixture.store.DB(), testProjectID, nil)
	parentA, err := epics.Create(ctx, CreateEpicInput{Title: "parent A"})
	if err != nil {
		t.Fatal(err)
	}
	parentB, err := epics.Create(ctx, CreateEpicInput{Title: "parent B"})
	if err != nil {
		t.Fatal(err)
	}

	for _, subtype := range []string{"task", "epic", "feature"} {
		for _, parent := range []struct {
			name, legacy string
		}{
			{name: "duplicate", legacy: parentA.ID},
			{name: "conflicting", legacy: parentB.ID},
		} {
			t.Run(subtype+"_"+parent.name, func(t *testing.T) {
				title := subtype + " " + parent.name + " parent declaration"
				var before int
				if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				relations := []CreateWorkItemRelationInput{{
					SourceItemID: parentA.ID, TargetIsNewItem: true, Kind: RelationParentOf,
				}}
				switch subtype {
				case "task":
					_, err = env.tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
						Task: CreateTaskInput{Title: title, ParentItemID: parent.legacy}, WorkItemRelations: relations,
					})
				case "epic":
					_, err = epics.Create(ctx, CreateEpicInput{Title: title, ParentItemID: parent.legacy, WorkItemRelations: relations})
				case "feature":
					_, err = env.features.Create(ctx, CreateFeatureInput{Title: title, ParentItemID: parent.legacy, WorkItemRelations: relations})
				}
				if err == nil || !strings.Contains(err.Error(), "declares a parent in both") {
					t.Fatalf("create error = %v, want explicit duplicate parent declaration", err)
				}
				var after, subtypeRows, intents int
				if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&after); err != nil {
					t.Fatal(err)
				}
				query := `SELECT COUNT(*) FROM ` + subtype + `s WHERE title = ?`
				if err := env.fixture.store.DB().QueryRowContext(ctx, query, title).Scan(&subtypeRows); err != nil {
					t.Fatal(err)
				}
				if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_creation_intents WHERE title = ?`, title).Scan(&intents); err != nil {
					t.Fatal(err)
				}
				if after != before || subtypeRows != 0 || intents != 0 {
					t.Fatalf("after rejected create: work_items=%d want %d, subtype=%d, intents=%d", after, before, subtypeRows, intents)
				}
			})
		}
	}
}

func TestTaskCreateRejectsFeatureIDAndGenericParentConflictAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	featureA, err := env.features.Create(ctx, CreateFeatureInput{Title: "feature parent A"})
	if err != nil {
		t.Fatal(err)
	}
	featureB, err := env.features.Create(ctx, CreateFeatureInput{Title: "feature parent B"})
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = env.tasks.CreateTaskWithDetails(ctx, CreateTaskWithDetailsInput{
		Task: CreateTaskInput{Title: "conflicting feature compatibility parent", FeatureID: &featureA.ID},
		WorkItemRelations: []CreateWorkItemRelationInput{{
			SourceItemID: featureB.ID, TargetIsNewItem: true, Kind: RelationParentOf,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "feature_id must match") {
		t.Fatalf("create error = %v, want feature parent mismatch", err)
	}
	var after, tasks int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE title = 'conflicting feature compatibility parent'`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if after != before || tasks != 0 {
		t.Fatalf("after rejected task: work_items=%d want %d, tasks=%d", after, before, tasks)
	}
}

func TestFeatureCreateIntentReplaysStoredRelationsExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	anchor, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "durable relation anchor"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "different relation anchor"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_durable_feature BEFORE INSERT ON features
WHEN NEW.title = 'durable feature' BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END;`); err != nil {
		t.Fatal(err)
	}
	input := CreateFeatureInput{
		Title: "durable feature", Body: "same body", OperationKey: "durable-operation",
		WorkItemRelations: []CreateWorkItemRelationInput{
			{SourceIsNewItem: true, TargetItemID: anchor.ID, Kind: RelationRelatedTo},
			{SourceIsNewItem: true, TargetItemID: other.ID, Kind: RelationBlocks},
		},
	}
	if _, err := env.features.Create(ctx, input); err == nil || !strings.Contains(err.Error(), "forced finalization failure") {
		t.Fatalf("forced create error = %v", err)
	}
	var intentID, state, payload string
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT id, state, relation_payload_json FROM feature_creation_intents WHERE operation_key = ?`, input.OperationKey).Scan(&intentID, &state, &payload); err != nil {
		t.Fatal(err)
	}
	if state != "ref_created" || payload == "[]" {
		t.Fatalf("intent state/payload = %q/%q", state, payload)
	}
	conflict := input
	conflict.WorkItemRelations = []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetItemID: other.ID, Kind: RelationRelatedTo}}
	if _, err := env.features.Create(ctx, conflict); !errors.Is(err, ErrFeatureCreationConflict) {
		t.Fatalf("operation-key conflict = %v, want ErrFeatureCreationConflict", err)
	}
	conflict.OperationKey = ""
	if _, err := env.features.Create(ctx, conflict); !errors.Is(err, ErrFeatureCreationConflict) {
		t.Fatalf("title/body/parent recovery conflict = %v, want ErrFeatureCreationConflict", err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `DROP TRIGGER fail_durable_feature`); err != nil {
		t.Fatal(err)
	}
	feature, err := env.features.Create(ctx, input)
	if err != nil {
		t.Fatalf("recover identical create: %v", err)
	}
	if feature.ID != intentID {
		t.Fatalf("recovered feature id = %q, want intent id %q", feature.ID, intentID)
	}
	if replayed, err := env.features.Create(ctx, input); err != nil || replayed.ID != feature.ID {
		t.Fatalf("completed replay = %+v, %v", replayed, err)
	}
	var count int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations
WHERE ((source_item_id = ? AND target_item_id = ?) OR (source_item_id = ? AND target_item_id = ?))
	AND kind = 'related_to'`, feature.ID, anchor.ID, anchor.ID, feature.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("durable related_to count = %d, want 1", count)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations WHERE source_item_id = ? OR target_item_id = ?`, feature.ID, feature.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("durable total relation count = %d, want every requested relation exactly once", count)
	}
}

func TestFeatureCreateIntentCleansExpectedOrphanAfterTargetDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	anchor, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "deleted target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_deleted_target_feature BEFORE INSERT ON features
WHEN NEW.title = 'deleted target feature' BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END;`); err != nil {
		t.Fatal(err)
	}
	input := CreateFeatureInput{Title: "deleted target feature", WorkItemRelations: []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetItemID: anchor.ID, Kind: RelationRelatedTo}}}
	if _, err := env.features.Create(ctx, input); err == nil {
		t.Fatal("forced create unexpectedly succeeded")
	}
	var id, branch string
	var owned bool
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT id, branch, ref_created_by_intent FROM feature_creation_intents WHERE title = ?`, input.Title).Scan(&id, &branch, &owned); err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("created feature ref is not recorded as owned by its intent")
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `DROP TRIGGER fail_deleted_target_feature; DELETE FROM work_items WHERE id = ?`, anchor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.features.Create(ctx, input); !errors.Is(err, ErrWorkItemNotFound) {
		t.Fatalf("deleted-target retry = %v, want ErrWorkItemNotFound", err)
	}
	if _, exists, err := flowgit.BranchTip(ctx, env.exchangePath(), branch); err != nil || exists {
		t.Fatalf("orphan branch exists=%v err=%v, want safely deleted", exists, err)
	}
	var intents, features, items int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_creation_intents WHERE id = ?`, id).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM features WHERE id = ?`, id).Scan(&features); err != nil {
		t.Fatal(err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE id = ?`, id).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if intents != 0 || features != 0 || items != 0 {
		t.Fatalf("cleanup counts intent/feature/item = %d/%d/%d", intents, features, items)
	}
}

func TestFeatureCreateIntentPreservesUnexpectedOrphanTip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	anchor, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "changed target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_unexpected_tip_feature BEFORE INSERT ON features
WHEN NEW.title = 'unexpected tip feature' BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END;`); err != nil {
		t.Fatal(err)
	}
	input := CreateFeatureInput{Title: "unexpected tip feature", WorkItemRelations: []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetItemID: anchor.ID, Kind: RelationRelatedTo}}}
	if _, err := env.features.Create(ctx, input); err == nil {
		t.Fatal("forced create unexpectedly succeeded")
	}
	var id, branch string
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT id, branch FROM feature_creation_intents WHERE title = ?`, input.Title).Scan(&id, &branch); err != nil {
		t.Fatal(err)
	}
	unexpected := env.advanceMain(t, "unexpected.txt", "unexpected\n", "unexpected tip")
	if err := flowgit.UpdateRef(ctx, env.exchangePath(), "refs/heads/"+branch, unexpected); err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `DROP TRIGGER fail_unexpected_tip_feature; DELETE FROM work_items WHERE id = ?`, anchor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.features.Create(ctx, input); !errors.Is(err, ErrWorkItemNotFound) || !strings.Contains(err.Error(), "preserving ref and intent") {
		t.Fatalf("unexpected-tip retry error = %v", err)
	}
	if tip, exists, err := flowgit.BranchTip(ctx, env.exchangePath(), branch); err != nil || !exists || tip != unexpected {
		t.Fatalf("preserved branch = %q exists=%v err=%v, want %q", tip, exists, err, unexpected)
	}
	var intents int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_creation_intents WHERE id = ? AND state = 'ref_created'`, id).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 1 {
		t.Fatalf("preserved intent count = %d, want 1", intents)
	}
}

func TestFeatureCreateConcurrentRetryDoesNotRegressCompletedIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	anchor, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "concurrent retry anchor"})
	if err != nil {
		t.Fatal(err)
	}
	input := CreateFeatureInput{
		Title: "concurrent retry feature", OperationKey: "concurrent-retry-operation",
		WorkItemRelations: []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetItemID: anchor.ID, Kind: RelationRelatedTo}},
	}
	firstPaused := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hookCalls atomic.Int32
	env.features.CreateAfterIntentCommitTestHook = func() {
		if hookCalls.Add(1) == 1 {
			close(firstPaused)
			<-releaseFirst
		}
	}
	type result struct {
		feature Feature
		err     error
	}
	firstResult := make(chan result, 1)
	go func() {
		feature, err := env.features.Create(ctx, input)
		firstResult <- result{feature: feature, err: err}
	}()
	select {
	case <-firstPaused:
	case got := <-firstResult:
		t.Fatalf("first create returned before retry window: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first create retry window")
	}

	second, err := env.features.Create(ctx, input)
	if err != nil {
		close(releaseFirst)
		t.Fatalf("concurrent retry: %v", err)
	}
	close(releaseFirst)
	first := <-firstResult
	if first.err != nil || first.feature.ID != second.ID {
		t.Fatalf("first create after completed retry = %+v, want feature %q", first, second.ID)
	}
	var state string
	var relationCount int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT state FROM feature_creation_intents WHERE operation_key = ?`, input.OperationKey).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM work_item_relations WHERE ((source_item_id = ? AND target_item_id = ?) OR (source_item_id = ? AND target_item_id = ?)) AND kind = 'related_to'`, second.ID, anchor.ID, anchor.ID, second.ID).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || relationCount != 1 {
		t.Fatalf("completed retry state/relation count = %q/%d, want completed/1", state, relationCount)
	}
}

func TestFeatureCreateCleanupPreservesPreExistingSameTipRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	anchor, err := env.tasks.CreateTask(ctx, CreateTaskInput{Title: "same-tip anchor"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_same_tip_feature BEFORE INSERT ON features
WHEN NEW.title = 'same-tip feature' BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END;`); err != nil {
		t.Fatal(err)
	}
	var seeded atomic.Bool
	env.features.CreateAfterIntentCommitTestHook = func() {
		if seeded.Swap(true) {
			return
		}
		var branch, targetSHA string
		if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT branch, target_sha FROM feature_creation_intents WHERE title = 'same-tip feature'`).Scan(&branch, &targetSHA); err != nil {
			t.Fatal(err)
		}
		if err := flowgit.UpdateRef(ctx, env.exchangePath(), "refs/heads/"+branch, targetSHA); err != nil {
			t.Fatal(err)
		}
	}
	input := CreateFeatureInput{Title: "same-tip feature", WorkItemRelations: []CreateWorkItemRelationInput{{SourceIsNewItem: true, TargetItemID: anchor.ID, Kind: RelationRelatedTo}}}
	if _, err := env.features.Create(ctx, input); err == nil {
		t.Fatal("forced create unexpectedly succeeded")
	}
	env.features.CreateAfterIntentCommitTestHook = nil
	var id, branch, targetSHA string
	var owned bool
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT id, branch, target_sha, ref_created_by_intent FROM feature_creation_intents WHERE title = ?`, input.Title).Scan(&id, &branch, &targetSHA, &owned); err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("pre-existing same-tip ref was incorrectly attributed to intent")
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `DROP TRIGGER fail_same_tip_feature; DELETE FROM work_items WHERE id = ?`, anchor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.features.Create(ctx, input); !errors.Is(err, ErrWorkItemNotFound) {
		t.Fatalf("same-tip cleanup retry = %v, want ErrWorkItemNotFound", err)
	}
	if tip, exists, err := flowgit.BranchTip(ctx, env.exchangePath(), branch); err != nil || !exists || tip != targetSHA {
		t.Fatalf("pre-existing branch tip=%q exists=%v err=%v, want preserved %q", tip, exists, err, targetSHA)
	}
	var intents int
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM feature_creation_intents WHERE id = ?`, id).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Fatalf("cleaned unowned intent count = %d, want 0", intents)
	}
}

func TestFeatureCreateGenericParentActorSurvivesIntentReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	epics := NewEpicService(env.fixture.store.DB(), testProjectID, nil)
	parent, err := epics.Create(ctx, CreateEpicInput{Title: "actor parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_actor_feature BEFORE INSERT ON features
WHEN NEW.title = 'actor feature' BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END;`); err != nil {
		t.Fatal(err)
	}
	input := CreateFeatureInput{
		Title: "actor feature", OperationKey: "actor-feature-replay", CreatedBy: ActorHuman,
		WorkItemRelations: []CreateWorkItemRelationInput{{SourceItemID: parent.ID, TargetIsNewItem: true, Kind: RelationParentOf, CreatedBy: ActorAgent}},
	}
	if _, err := env.features.Create(ctx, input); err == nil {
		t.Fatal("forced create unexpectedly succeeded")
	}
	var payload string
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT relation_payload_json FROM feature_creation_intents WHERE operation_key = ?`, input.OperationKey).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	relations, err := decodeCreateRelationPayload(payload)
	if err != nil || len(relations) != 1 || relations[0].CreatedBy != ActorAgent {
		t.Fatalf("stored relation payload = %+v, err=%v; want agent actor", relations, err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `DROP TRIGGER fail_actor_feature`); err != nil {
		t.Fatal(err)
	}
	feature, err := env.features.Create(ctx, input)
	if err != nil {
		t.Fatalf("replay actor feature: %v", err)
	}
	if replayed, err := env.features.Create(ctx, input); err != nil || replayed.ID != feature.ID {
		t.Fatalf("completed actor replay = %+v, %v", replayed, err)
	}
	var persistedActor string
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT created_by FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'parent_of'`, parent.ID, feature.ID).Scan(&persistedActor); err != nil {
		t.Fatal(err)
	}
	if persistedActor != string(ActorAgent) {
		t.Fatalf("generic parent actor = %q, want %q", persistedActor, ActorAgent)
	}

	canonical, err := env.features.Create(ctx, CreateFeatureInput{Title: "canonical parent actor", ParentItemID: parent.ID, CreatedBy: ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fixture.store.DB().QueryRowContext(ctx, `SELECT created_by FROM work_item_relations WHERE source_item_id = ? AND target_item_id = ? AND kind = 'parent_of'`, parent.ID, canonical.ID).Scan(&persistedActor); err != nil {
		t.Fatal(err)
	}
	if persistedActor != string(ActorSystem) {
		t.Fatalf("canonical parent actor = %q, want surrounding create actor %q", persistedActor, ActorSystem)
	}
}
