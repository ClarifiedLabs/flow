package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// seedRunningRebaseRow inserts a running feature_rebases row directly, standing
// in for a rebase whose process died before it could publish. The returned
// FeatureRebase mirrors what RunningRebase would load.
func seedRunningRebaseRow(t *testing.T, env *featureTestEnv, feature Feature, oldTip string, createdOffset time.Duration) FeatureRebase {
	t.Helper()
	ctx := context.Background()
	created := time.Now().UTC().Add(createdOffset)
	id := "rb-seed-" + strings.ToLower(strings.NewReplacer("_", "", "/", "", "-", "").Replace(t.Name()))
	if len(id) > 48 {
		id = id[:48]
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
INSERT INTO feature_rebases (
	id, feature_id, old_tip_sha, target_base, target_base_sha, new_tip_sha, state, created_at, restrict_blocked_to
) VALUES (?, ?, ?, 'main', ?, '', 'running', ?, '')`,
		id, feature.ID, oldTip, env.branchTip(t, "main"), formatTime(created)); err != nil {
		t.Fatalf("seed running rebase row: %v", err)
	}
	// The redo row markRebaseStale opens carries the same task_id, so the
	// seeded row must reference a real task to satisfy the foreign key.
	task, err := env.tasks.CreateTask(context.Background(), CreateTaskInput{
		Title: "rebase " + feature.Title, FeatureID: &feature.ID, CreatedBy: ActorSystem,
	})
	if err != nil {
		t.Fatalf("create rebase task: %v", err)
	}
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
UPDATE feature_rebases SET task_id = ? WHERE id = ?`, task.ID, id); err != nil {
		t.Fatalf("attach rebase task: %v", err)
	}
	rebase, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("load seeded running rebase: %+v, %v, %v", rebase, found, err)
	}
	return rebase
}

// seedPublicationIntent inserts a rebase_publications row directly, standing in
// for a publication intent that was durably recorded and whose process then
// crashed before (or after) the push.
func seedPublicationIntent(t *testing.T, env *featureTestEnv, rebase FeatureRebase, newTip string) {
	t.Helper()
	if _, err := env.fixture.store.DB().ExecContext(context.Background(), `
INSERT INTO rebase_publications (id, rebase_id, old_tip_sha, new_tip_sha, recorded_at)
VALUES (?, ?, ?, ?, ?)`,
		"rbp-seed-"+rebase.ID, rebase.ID, rebase.OldTipSHA, newTip, formatTime(rebase.CreatedAt.Add(time.Second))); err != nil {
		t.Fatalf("seed publication intent: %v", err)
	}
}

// seedGitEvent records a git event directly, standing in for an observed ref
// update (the merge-push case, or an undrained spool line).
func seedGitEvent(t *testing.T, env *featureTestEnv, ref, oldSHA, newSHA, actor string) {
	t.Helper()
	if _, err := env.gitEvents.Record(context.Background(), GitEvent{
		OldSHA: oldSHA, NewSHA: newSHA, Ref: ref, Actor: actor, ObservedAt: time.Now().UTC(),
	}, GitEventSourceSpool); err != nil {
		t.Fatalf("seed git event: %v", err)
	}
}

// appendSpooledEvent writes one JSONL line into the exchange's post-receive
// spool without draining it, standing in for a hook that fired but whose
// coordinator crashed before draining.
func appendSpooledEvent(t *testing.T, exchangePath, ref, oldSHA, newSHA, actor string) {
	t.Helper()
	spool := flowgit.SpoolPath(exchangePath)
	if err := os.MkdirAll(filepath.Dir(spool), 0o755); err != nil {
		t.Fatalf("create spool dir: %v", err)
	}
	line := `{"old_sha":"` + oldSHA + `","new_sha":"` + newSHA + `","ref":"` + ref +
		`","actor":"` + actor + `","observed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	file, err := os.OpenFile(spool, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatalf("append spool event: %v", err)
	}
}

// s_resolveRebasePublicationForTest drives the resolver directly so a test
// can assert its decision for one seeded row without the request-level
// recursion that a redo row would trigger.
func s_resolveRebasePublicationForTest(t *testing.T, env *featureTestEnv, feature Feature, rebase FeatureRebase) (RebasePublicationOutcome, error) {
	t.Helper()
	return env.features.resolveRebasePublication(context.Background(), feature, rebase)
}

// TestRebaseRecoveryMergePushDoesNotFinalize is the recovery-wins
// interleaving: the feature ref advanced by a merge push from another actor
// while a conflicted rebase's row was still running. The old observational heal
// (tip != OldTipSHA) finalized the row; evidence-carrying finalization must not.
func TestRebaseRecoveryMergePushDoesNotFinalize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature := setupConflictedFeature(t, env)
	oldTip := env.branchTip(t, feature.Branch)

	rebase := seedRunningRebaseRow(t, env, feature, oldTip, 0)

	// Another actor merges into the feature branch; the push is observed as a
	// git event, but no publication intent exists for this rebase.
	clone := env.cloneExchange(t)
	movedTip := commitOnBranch(t, clone, feature.Branch, "merged.txt", "merged\n", "merge by another actor")
	seedGitEvent(t, env, "refs/heads/"+feature.Branch, oldTip, movedTip, "coordinator")

	// Duplicate retry: the stale resolution closes the seeded row and opens a
	// redo row against the moved tip, so the request reports the new conflicted
	// rebase task rather than an in-flight hold.
	outcome, err := s_resolveRebasePublicationForTest(t, env, feature, rebase)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if outcome != RebasePublicationStale {
		t.Fatalf("resolve outcome = %q, want stale", outcome)
	}

	// The stale resolution opened a redo row against the moved tip for the same
	// task, so a duplicate request is held with the existing in-flight
	// rebase — never a second rebase, and never a finalize.
	if _, err := env.features.RebaseOnMain(ctx, feature); !errors.Is(err, ErrFeatureRebaseRunning) {
		t.Fatalf("duplicate retry error = %v, want ErrFeatureRebaseRunning", err)
	}
	redo, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found {
		t.Fatalf("redo row after stale = %+v, %v, %v", redo, found, err)
	}
	if redo.OldTipSHA != movedTip || redo.TaskID != rebase.TaskID {
		t.Fatalf("redo row = %+v, want old tip %s task %s", redo, movedTip, rebase.TaskID)
	}

	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	for _, row := range rebases {
		if row.ID == rebase.ID && row.State == RebaseFinalized {
			t.Fatalf("seeded rebase row %s was finalized from a merge push by another actor", row.ID)
		}
		if row.ID == rebase.ID && row.State != RebaseStale {
			t.Fatalf("seeded rebase row %s state = %q, want stale", row.ID, row.State)
		}
	}
}

// TestRebaseRecoverySpooledEventFinalizes is the clean crash-recovery
// interleaving: the intent was recorded, the push landed, and the event is
// spooled but undrained. The retry drains and finalizes with evidence.
func TestRebaseRecoverySpooledEventFinalizes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "recovery"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	oldTip := commitOnBranch(t, clone, feature.Branch, "feature.txt", "one\n", "feature work")
	env.advanceMain(t, "base.txt", "base\n", "base work")

	// Compute the rebased head without publishing, then record intent and push
	// by hand from that clone, leaving the event only in the spool (undrained).
	rebased, err := flowgit.RebaseBranchCloned(ctx, flowgit.RebaseOntoInput{
		ExchangePath: env.exchangePath(), Branch: feature.Branch, Onto: "main",
		ExpectedOldSHA: oldTip,
	})
	if err != nil {
		t.Fatalf("rebase branch: %v", err)
	}
	defer os.RemoveAll(rebased.Worktree)
	rebase := seedRunningRebaseRow(t, env, feature, oldTip, 0)
	if _, err := env.fixture.store.DB().ExecContext(ctx, `
UPDATE feature_rebases SET task_id = NULL WHERE id = ?`, rebase.ID); err != nil {
		t.Fatalf("make running rebase task-less: %v", err)
	}
	rebase, found, err := env.features.RunningRebase(ctx, feature.ID)
	if err != nil || !found || rebase.TaskID != "" {
		t.Fatalf("load task-less running rebase: %+v, %v, %v", rebase, found, err)
	}
	seedPublicationIntent(t, env, rebase, rebased.HeadSHA)
	if err := flowgit.PushBranchCompareAndSwap(ctx, rebased.Worktree, "refs/heads/"+feature.Branch, oldTip); err != nil {
		t.Fatalf("push rebased head: %v", err)
	}
	appendSpooledEvent(t, env.exchangePath(), "refs/heads/"+feature.Branch, oldTip, rebased.HeadSHA, "coordinator")

	// The retry must drain the spool, finalize with the event as evidence, and
	// report the published intent tip rather than the running row's empty tip.
	result, err := env.features.RebaseOnMain(ctx, feature)
	if err != nil {
		t.Fatalf("retry rebase: %v", err)
	}
	if result.Kind != RebaseRebased || result.NewTipSHA != rebased.HeadSHA {
		t.Fatalf("retry result = %+v, want rebased tip %s", result, rebased.HeadSHA)
	}
	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	var confirmed string
	for _, row := range rebases {
		if row.ID == rebase.ID && row.State != RebaseFinalized {
			t.Fatalf("seeded rebase row state = %q, want finalized", row.State)
		}
	}
	err = env.fixture.store.DB().QueryRowContext(ctx, `
SELECT COALESCE(confirmed_event_hash, '') FROM rebase_publications WHERE rebase_id = ? AND new_tip_sha = ?`,
		rebase.ID, rebased.HeadSHA).Scan(&confirmed)
	if err != nil {
		t.Fatalf("load publication confirmation: %v", err)
	}
	if confirmed == "" {
		t.Fatal("publication intent has no confirmed_event_hash after recovery finalization")
	}
}

// TestRebaseRecoveryIntentWithoutPushFails is the intent-without-push
// interleaving: intent recorded, tip unchanged, no event. The row must be
// failed (not stuck running) so a retry can proceed.
func TestRebaseRecoveryIntentWithoutPushFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "intent only"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	oldTip := commitOnBranch(t, clone, feature.Branch, "feature.txt", "one\n", "feature work")
	env.advanceMain(t, "base.txt", "base\n", "base work")

	rebased, err := flowgit.RebaseBranch(ctx, flowgit.RebaseOntoInput{
		ExchangePath: env.exchangePath(), Branch: feature.Branch, Onto: "main",
		ExpectedOldSHA: oldTip,
	})
	if err != nil {
		t.Fatalf("rebase branch: %v", err)
	}
	rebase := seedRunningRebaseRow(t, env, feature, oldTip, 0)
	seedPublicationIntent(t, env, rebase, rebased.HeadSHA)

	// No push, no event, tip unchanged.
	if _, err := env.features.RebaseOnMain(ctx, feature); err != nil {
		t.Fatalf("retry rebase: %v", err)
	}
	rebases, err := env.features.ListRebases(ctx, feature.ID)
	if err != nil {
		t.Fatalf("list rebases: %v", err)
	}
	for _, row := range rebases {
		if row.ID == rebase.ID && row.State != RebaseFailed {
			t.Fatalf("seeded rebase row state = %q, want failed", row.State)
		}
	}
	if _, found, err := env.features.RunningRebase(ctx, feature.ID); err != nil || found {
		t.Fatalf("running rebase after recovery = %v, %v, want none", found, err)
	}
}

// TestStampRebaseDoneRefusesFinalized is the choke-point guard: the generic
// stamping helper must refuse to finalize, so no future call site can bypass
// the evidence requirement by accident.
func TestStampRebaseDoneRefusesFinalized(t *testing.T) {
	t.Parallel()
	env := newFeatureTestEnv(t)
	if err := env.features.stampRebaseDone(context.Background(), "rb-any", RebaseFinalized, "abc"); !errors.Is(err, ErrRebaseFinalizationRequiresEvidence) {
		t.Fatalf("stampRebaseDone(finalized) error = %v, want ErrRebaseFinalizationRequiresEvidence", err)
	}
}

// TestRebaseBranchLeavesExchangeRefUnmoved proves the rebase/push split: the
// branch variant computes the rebased head without moving the exchange ref,
// while the onto variant publishes it.
func TestRebaseBranchLeavesExchangeRefUnmoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newFeatureTestEnv(t)
	feature, err := env.features.Create(ctx, CreateFeatureInput{Title: "split"})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	clone := env.cloneExchange(t)
	oldTip := commitOnBranch(t, clone, feature.Branch, "feature.txt", "one\n", "feature work")
	env.advanceMain(t, "base.txt", "base\n", "base work")

	result, err := flowgit.RebaseBranch(ctx, flowgit.RebaseOntoInput{
		ExchangePath: env.exchangePath(), Branch: feature.Branch, Onto: "main",
		ExpectedOldSHA: oldTip,
	})
	if err != nil {
		t.Fatalf("rebase branch: %v", err)
	}
	if result.HeadSHA == oldTip {
		t.Fatal("rebase branch returned the old tip")
	}
	if tip := env.branchTip(t, feature.Branch); tip != oldTip {
		t.Fatalf("exchange ref = %s, want untouched %s", tip, oldTip)
	}
}
