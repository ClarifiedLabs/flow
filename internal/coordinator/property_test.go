package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// The fold invariants, kata drift-guard style: the derived state the services
// maintain incrementally must always equal a fresh fold over the raw event-ish
// rows (work item relations + subtype tables). Ops below run in randomized
// order across fixed seeds; every assertion recomputes from scratch in Go so
// the test cannot drift with the maintainer code it checks.
//
//   (a) tasks.feature_id == nearest feature ancestor (CheckConsistency covers
//       this plus cycles/subtype validity)
//   (b) UnresolvedBlockers(task) == fresh count over the relations snapshot
//   (c) all_children epic completion == derived from children + effective
//       blockers, including automatic reopen when a child un-resolves

type foldEnv struct {
	fixture  projectFixture
	tasks    *TaskService
	items    *WorkItemService
	epics    *EpicService
	features *FeatureService
	runs     *WorkflowRunService
}

func newFoldEnv(t *testing.T) *foldEnv {
	t.Helper()
	ctx := context.Background()
	fixture := newProjectFixture(t)

	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	globals := NewGlobalAgentDefService(globalStore.DB())
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global agent definitions: %v", err)
	}
	defs := NewInheritedAgentDefService(fixture.store.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(fixture.store.DB(), defs)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed default flows: %v", err)
	}

	tasks := NewTaskService(fixture.store.DB(), testProjectID)
	runs := NewWorkflowRunService(fixture.store.DB(), flows, tasks)
	features := NewFeatureService(fixture.store.DB(), tasks, fixture.project)
	features.Runs = runs
	runs.Features = features
	items := NewWorkItemService(fixture.store.DB(), testProjectID)
	epics := NewEpicService(fixture.store.DB(), testProjectID, items)

	return &foldEnv{fixture: fixture, tasks: tasks, items: items, epics: epics, features: features, runs: runs}
}

type foldItem struct {
	id   string
	kind string // work_items.kind: task|epic|feature
}

type foldEdge struct {
	source, target, kind string
}

// foldSnapshot is the raw state the invariants fold over.
type foldSnapshot struct {
	items          map[string]string // id -> kind
	edges          []foldEdge
	taskStates     map[string]string // task id -> lifecycle_state ("" = unscheduled)
	epicRows       map[string]foldEpicRow
	featureStates  map[string]string
}

type foldEpicRow struct {
	status, policy string
	automatic      bool
}

func loadFoldSnapshot(t *testing.T, ctx context.Context, env *foldEnv) foldSnapshot {
	t.Helper()
	db := env.fixture.store.DB()
	snap := foldSnapshot{
		items:         map[string]string{},
		taskStates:    map[string]string{},
		epicRows:      map[string]foldEpicRow{},
		featureStates: map[string]string{},
	}

	rows, err := db.QueryContext(ctx, `SELECT id, kind FROM work_items`)
	if err != nil {
		t.Fatalf("load work items: %v", err)
	}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatalf("scan work item: %v", err)
		}
		snap.items[id] = kind
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close work items: %v", err)
	}

	rows, err = db.QueryContext(ctx, `SELECT source_item_id, target_item_id, kind FROM work_item_relations`)
	if err != nil {
		t.Fatalf("load relations: %v", err)
	}
	for rows.Next() {
		var edge foldEdge
		if err := rows.Scan(&edge.source, &edge.target, &edge.kind); err != nil {
			t.Fatalf("scan relation: %v", err)
		}
		snap.edges = append(snap.edges, edge)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close relations: %v", err)
	}

	rows, err = db.QueryContext(ctx, `SELECT id, COALESCE(lifecycle_state, '') FROM tasks`)
	if err != nil {
		t.Fatalf("load tasks: %v", err)
	}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan task: %v", err)
		}
		snap.taskStates[id] = state
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close tasks: %v", err)
	}

	rows, err = db.QueryContext(ctx, `SELECT id, status, completion_policy, completed_automatically FROM epics`)
	if err != nil {
		t.Fatalf("load epics: %v", err)
	}
	for rows.Next() {
		var id, status, policy string
		var automatic int
		if err := rows.Scan(&id, &status, &policy, &automatic); err != nil {
			t.Fatalf("scan epic: %v", err)
		}
		snap.epicRows[id] = foldEpicRow{status: status, policy: policy, automatic: automatic != 0}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close epics: %v", err)
	}

	rows, err = db.QueryContext(ctx, `SELECT id, status FROM features`)
	if err != nil {
		t.Fatalf("load features: %v", err)
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatalf("scan feature: %v", err)
		}
		snap.featureStates[id] = status
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close features: %v", err)
	}

	return snap
}

// ancestors returns the item itself plus every transitive parent_of ancestor.
func (snap foldSnapshot) ancestors(id string) map[string]bool {
	seen := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range snap.edges {
			if edge.kind == string(RelationParentOf) && edge.target == current && !seen[edge.source] {
				seen[edge.source] = true
				queue = append(queue, edge.source)
			}
		}
	}
	return seen
}

func (snap foldSnapshot) terminal(id string) bool {
	switch snap.items[id] {
	case "task":
		return snap.taskStates[id] == string(LifecycleDone)
	case "epic":
		status := snap.epicRows[id].status
		return status == string(EpicCompleted) || status == string(EpicArchived)
	case "feature":
		status := snap.featureStates[id]
		return status == string(FeatureLanded) || status == string(FeatureArchived)
	}
	return false
}

// freshUnresolvedBlockers mirrors TaskService.UnresolvedBlockers row-for-row:
// one row per blocks edge whose source is a non-done task and whose target is
// the task or one of its ancestors.
func (snap foldSnapshot) freshUnresolvedBlockers(taskID string) int {
	targets := snap.ancestors(taskID)
	count := 0
	for _, edge := range snap.edges {
		if edge.kind != string(RelationBlocks) || !targets[edge.target] {
			continue
		}
		if snap.items[edge.source] == "task" && snap.taskStates[edge.source] != string(LifecycleDone) {
			count++
		}
	}
	return count
}

// freshEffectiveBlockers mirrors effectiveUnresolvedBlockerCountTx: distinct
// non-terminal blockers of any kind targeting the item or an ancestor.
func (snap foldSnapshot) freshEffectiveBlockers(itemID string) int {
	targets := snap.ancestors(itemID)
	sources := map[string]bool{}
	for _, edge := range snap.edges {
		if edge.kind != string(RelationBlocks) || !targets[edge.target] {
			continue
		}
		if !snap.terminal(edge.source) {
			sources[edge.source] = true
		}
	}
	return len(sources)
}

// freshEpicShouldComplete mirrors epicEligibleTx plus the childCount>0 rule
// the reconciler applies. parent_of edges are stored (source=parent,
// target=child), so the epic's children are the targets of its own edges.
func (snap foldSnapshot) freshEpicShouldComplete(epicID string) bool {
	children := 0
	allTerminal := true
	for _, edge := range snap.edges {
		if edge.kind == string(RelationParentOf) && edge.source == epicID {
			children++
			if !snap.terminal(edge.target) {
				allTerminal = false
			}
		}
	}
	return children > 0 && allTerminal && snap.freshEffectiveBlockers(epicID) == 0
}

func assertFoldInvariants(t *testing.T, ctx context.Context, env *foldEnv, where string) {
	t.Helper()
	snap := loadFoldSnapshot(t, ctx, env)

	// (a) feature cache + subtype/parent/cycle validity.
	report, err := env.items.CheckConsistency(ctx)
	if err != nil {
		t.Fatalf("%s: check consistency: %v", where, err)
	}
	if !report.Healthy {
		t.Fatalf("%s: consistency issues: %+v", where, report.Issues)
	}

	// (b) per-task blocker derivation.
	for taskID := range snap.taskStates {
		maintained, err := env.tasks.UnresolvedBlockers(ctx, taskID)
		if err != nil {
			t.Fatalf("%s: unresolved blockers for %s: %v", where, taskID, err)
		}
		if fresh := snap.freshUnresolvedBlockers(taskID); len(maintained) != fresh {
			t.Fatalf("%s: UnresolvedBlockers(%s) = %d, fresh fold = %d", where, taskID, len(maintained), fresh)
		}
	}

	// (c) all_children epic completion fold. Auto-completed epics track the
	// derivation exactly; manual completions (completed_automatically=0) are
	// sticky by design and exempt from the !shouldComplete direction.
	for epicID, row := range snap.epicRows {
		if row.policy != string(EpicAllChildren) || row.status == string(EpicArchived) {
			continue
		}
		shouldComplete := snap.freshEpicShouldComplete(epicID)
		diagnose := func() string {
			var b strings.Builder
			fmt.Fprintf(&b, "epic %s status=%s automatic=%t shouldComplete=%t\n", epicID, row.status, row.automatic, shouldComplete)
			for _, edge := range snap.edges {
				if edge.kind == string(RelationParentOf) && edge.source == epicID {
					fmt.Fprintf(&b, "  child %s kind=%s terminal=%t\n", edge.target, snap.items[edge.target], snap.terminal(edge.target))
				}
				if edge.kind == string(RelationBlocks) {
					fmt.Fprintf(&b, "  blocks %s -> %s (source terminal=%t)\n", edge.source, edge.target, snap.terminal(edge.source))
				}
			}
			return b.String()
		}
		switch {
		case shouldComplete && row.status != string(EpicCompleted):
			t.Fatalf("%s: epic %s should be completed (children terminal, no blockers) but is %s\n%s", where, epicID, row.status, diagnose())
		case !shouldComplete && row.status == string(EpicCompleted) && row.automatic:
			t.Fatalf("%s: epic %s auto-completed but derivation no longer holds (child reopened or blocker added)\n%s", where, epicID, diagnose())
		case !shouldComplete && row.status != string(EpicOpen) && row.status != string(EpicCompleted):
			t.Fatalf("%s: epic %s in unexpected status %s\n%s", where, epicID, row.status, diagnose())
		}
	}
}

func TestWorkItemFoldInvariantsUnderRandomOps(t *testing.T) {
	t.Parallel()
	for _, seed := range []int64{20260811, 7, 999331} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newFoldEnv(t)
			rng := rand.New(rand.NewSource(seed))

			var itemsList []foldItem
			addItem := func(id, kind string) { itemsList = append(itemsList, foldItem{id: id, kind: kind}) }
			containers := func() []foldItem {
				var out []foldItem
				for _, item := range itemsList {
					if item.kind == "epic" || item.kind == "feature" {
						out = append(out, item)
					}
				}
				return out
			}
			randomParent := func() string {
				cs := containers()
				if len(cs) == 0 || rng.Intn(2) == 0 {
					return ""
				}
				return cs[rng.Intn(len(cs))].id
			}

			const steps = 40
			for step := 0; step < steps; step++ {
				where := fmt.Sprintf("seed %d step %d", seed, step)
				op := rng.Intn(10)
				switch {
				case op <= 1: // create task
					task, err := env.tasks.CreateTask(ctx, CreateTaskInput{
						Title: fmt.Sprintf("fold task s%d-%d", seed, step), Priority: rng.Intn(4), ParentItemID: randomParent(),
					})
					if err == nil {
						addItem(task.ID, "task")
					} else {
						t.Logf("%s: create task rejected: %v", where, err)
					}
				case op == 2: // create epic
					policy := EpicAllChildren
					if rng.Intn(2) == 0 {
						policy = EpicManual
					}
					epic, err := env.epics.Create(ctx, CreateEpicInput{
						Title: fmt.Sprintf("fold epic s%d-%d", seed, step), ParentItemID: randomParent(), CompletionPolicy: policy,
					})
					if err == nil {
						addItem(epic.ID, "epic")
					} else {
						t.Logf("%s: create epic rejected: %v", where, err)
					}
				case op == 3: // create feature
					feature, err := env.features.Create(ctx, CreateFeatureInput{
						Title: fmt.Sprintf("fold feature s%d-%d", seed, step), ParentItemID: randomParent(),
					})
					if err == nil {
						addItem(feature.ID, "feature")
					} else {
						t.Logf("%s: create feature rejected: %v", where, err)
					}
				case op == 4 && len(itemsList) >= 2: // link blocks
					source := itemsList[rng.Intn(len(itemsList))]
					target := itemsList[rng.Intn(len(itemsList))]
					if err := env.items.Link(ctx, source.id, target.id, RelationBlocks, ActorHuman); err != nil {
						t.Logf("%s: link %s->%s rejected: %v", where, source.id, target.id, err)
					}
				case op == 5: // unlink an existing blocks edge
					snap := loadFoldSnapshot(t, ctx, env)
					var blockEdges []foldEdge
					for _, edge := range snap.edges {
						if edge.kind == string(RelationBlocks) {
							blockEdges = append(blockEdges, edge)
						}
					}
					if len(blockEdges) > 0 {
						edge := blockEdges[rng.Intn(len(blockEdges))]
						if err := env.items.Unlink(ctx, edge.source, edge.target, RelationBlocks); err != nil {
							t.Logf("%s: unlink %s->%s rejected: %v", where, edge.source, edge.target, err)
						}
					}
				case op == 6 && len(itemsList) > 0: // reparent
					item := itemsList[rng.Intn(len(itemsList))]
					if err := env.items.Move(ctx, item.id, randomParent(), ActorHuman); err != nil {
						t.Logf("%s: move %s rejected: %v", where, item.id, err)
					}
				case op == 7: // force-done a random open task
					snap := loadFoldSnapshot(t, ctx, env)
					var open []string
					for id, state := range snap.taskStates {
						if state != string(LifecycleDone) {
							open = append(open, id)
						}
					}
					if len(open) > 0 {
						sort.Strings(open)
						id := open[rng.Intn(len(open))]
						if _, err := env.runs.ForceDone(ctx, id, ResolutionCompleted, "fold op", ActorHuman); err != nil {
							t.Logf("%s: force-done %s rejected: %v", where, id, err)
						}
					}
				case op == 8: // reopen a random done task
					snap := loadFoldSnapshot(t, ctx, env)
					var done []string
					for id, state := range snap.taskStates {
						if state == string(LifecycleDone) {
							done = append(done, id)
						}
					}
					if len(done) > 0 {
						sort.Strings(done)
						id := done[rng.Intn(len(done))]
						if _, err := env.runs.Reopen(ctx, id, ActorHuman); err != nil {
							t.Logf("%s: reopen %s rejected: %v", where, id, err)
						}
					}
				case op == 9: // epic lifecycle transitions
					snap := loadFoldSnapshot(t, ctx, env)
					var epicIDs []string
					for id := range snap.epicRows {
						epicIDs = append(epicIDs, id)
					}
					if len(epicIDs) > 0 {
						sort.Strings(epicIDs)
						id := epicIDs[rng.Intn(len(epicIDs))]
						var err error
						switch rng.Intn(3) {
						case 0:
							_, err = env.epics.Complete(ctx, id, ActorHuman)
						case 1:
							_, err = env.epics.Reopen(ctx, id, ActorHuman)
						default:
							_, err = env.epics.Archive(ctx, id, ActorHuman)
						}
						if err != nil {
							t.Logf("%s: epic transition on %s rejected: %v", where, id, err)
						}
					}
				}
				assertFoldInvariants(t, ctx, env, where)
			}
		})
	}
}

// TestSpoolDrainReplayIsIdempotent proves kata's ingest discipline on the
// post-receive spool: duplicated and reordered lines dedup to the same
// git_events rows (event_hash content addressing), re-draining is a no-op,
// and the derived change projection converges to the same branch tip.
func TestSpoolDrainReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newProjectFixture(t)
	tasks := NewTaskService(fixture.store.DB(), testProjectID)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "spool replay task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	branch := "task/" + task.ID
	if err := runReconcileGit(fixture.repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	pushEnv := []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}
	writeReconcileFile(t, fixture.repoPath, "one.txt", "one\n")
	if err := runReconcileGit(fixture.repoPath, nil, "add", "one.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "first"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, pushEnv, "push", fixture.project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("first push: %v", err)
	}
	firstSHA, err := reconcileGitOutput(fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read first tip: %v", err)
	}
	writeReconcileFile(t, fixture.repoPath, "two.txt", "two\n")
	if err := runReconcileGit(fixture.repoPath, nil, "add", "two.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, nil, "commit", "-m", "second"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := runReconcileGit(fixture.repoPath, pushEnv, "push", fixture.project.ExchangePath, branch+":"+branch); err != nil {
		t.Fatalf("second push: %v", err)
	}
	tip, err := reconcileGitOutput(fixture.repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read branch tip: %v", err)
	}

	// Craft the spool the hook would have written: one event per push,
	// then duplicate every line and reverse the order — the shape a
	// crashed-and-replayed ingest produces.
	zeroSHA := strings.Repeat("0", 40)
	observed := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	events := []flowgit.HookEvent{
		{OldSHA: zeroSHA, NewSHA: firstSHA, Ref: branch, Actor: "worker:w-local", ObservedAt: observed},
		{OldSHA: firstSHA, NewSHA: tip, Ref: branch, Actor: "worker:w-local", ObservedAt: observed.Add(time.Second)},
	}
	var lines []string
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("encode hook event: %v", err)
		}
		lines = append(lines, string(encoded))
	}
	var replayed []string
	for i := len(lines) - 1; i >= 0; i-- {
		replayed = append(replayed, lines[i], lines[i])
	}
	spoolPath := flowgit.SpoolPath(fixture.project.ExchangePath)
	if err := os.MkdirAll(filepath.Dir(spoolPath), 0o755); err != nil {
		t.Fatalf("create spool dir: %v", err)
	}
	if err := os.WriteFile(spoolPath, []byte(strings.Join(replayed, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write replayed spool: %v", err)
	}

	eventService := NewGitEventService(fixture.store.DB())
	inserted, err := eventService.DrainSpooled(ctx, fixture.project.ExchangePath)
	if err != nil {
		t.Fatalf("drain replayed spool: %v", err)
	}
	if inserted != len(lines) {
		t.Fatalf("inserted = %d, want %d unique events", inserted, len(lines))
	}
	stored, err := eventService.List(ctx)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) != len(lines) {
		t.Fatalf("stored events = %d, want %d", len(stored), len(lines))
	}

	// Re-draining the same spool inserts nothing and errors nothing.
	again, err := eventService.DrainSpooled(ctx, fixture.project.ExchangePath)
	if err != nil {
		t.Fatalf("re-drain: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-drain inserted = %d, want 0", again)
	}

	// The derived change head converges to the real branch tip, and a second
	// reconcile pass changes nothing.
	reconciler := NewReconcileService(fixture.store.DB())
	if _, err := reconciler.Reconcile(ctx, fixture.project); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var head string
	if err := fixture.store.DB().QueryRowContext(ctx, `
SELECT head_sha FROM changes WHERE task_id = ? AND branch = ?`, task.ID, branch).Scan(&head); err != nil {
		t.Fatalf("load change head: %v", err)
	}
	if head != tip {
		t.Fatalf("change head = %s, want branch tip %s", head, tip)
	}
	second, err := reconciler.Reconcile(ctx, fixture.project)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(second.UpdatedChanges) != 0 || second.ChangesCreated != 0 {
		t.Fatalf("second reconcile = %+v, want no changes", second)
	}
	if err := fixture.store.DB().QueryRowContext(ctx, `
SELECT head_sha FROM changes WHERE task_id = ? AND branch = ?`, task.ID, branch).Scan(&head); err != nil {
		t.Fatalf("reload change head: %v", err)
	}
	if head != tip {
		t.Fatalf("change head after replay = %s, want %s", head, tip)
	}
}
