package lifecycle

import (
	"context"
	"math/rand"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// TestEngineReplayDeterminism asserts the FSM is a pure function of (events,
// initial state): replaying the same event sequence from the same starting state
// produces an identical transition log. This guards the determinism the Effects
// seam is meant to preserve.
func TestEngineReplayDeterminism(t *testing.T) {
	events := []Event{
		{Kind: EventScheduleIssue, Payload: EventPayload{Schedule: coordinator.ScheduleBacklog}},
		{Kind: EventScheduleIssue, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}},
		{Kind: EventTriageIssue, Payload: EventPayload{Triage: coordinator.TriageAccepted}},
	}

	run := func() []string {
		eng, fake, store, issueID := newEngineTest(t)
		fake.issue.TriageState = coordinator.TriageAccepted
		ctx := context.Background()
		for _, ev := range events {
			ev.IssueID = issueID
			if _, err := eng.Step(ctx, ev); err != nil {
				t.Fatalf("step %s: %v", ev.Kind, err)
			}
		}
		rows, err := store.DB().Query(
			`SELECT event_kind, from_phase, to_phase FROM transitions WHERE issue_id = ? ORDER BY seq`, issueID)
		if err != nil {
			t.Fatalf("query transitions: %v", err)
		}
		defer rows.Close()
		var log []string
		for rows.Next() {
			var kind, from, to string
			if err := rows.Scan(&kind, &from, &to); err != nil {
				t.Fatalf("scan: %v", err)
			}
			log = append(log, kind+":"+from+"->"+to)
		}
		return log
	}

	a := run()
	b := run()
	if len(a) == 0 {
		t.Fatalf("expected transitions to be recorded")
	}
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic replay at %d: %q vs %q\n a=%v\n b=%v", i, a[i], b[i], a, b)
		}
	}
}

// TestEngineReplayDeterminismGenerated extends the hand-written replay test above
// to GENERATED histories: for each seed it draws a batch from the interleave
// catalog, applies it twice against two fresh stores in the SAME order, and
// asserts the two transition logs are byte-identical. Where the test above pins a
// single curated sequence, this fans the same purity claim — the FSM is a
// deterministic function of (ordered events, initial state) — across the catalog,
// so a future non-deterministic effect (a map iteration leaking into a phase, a
// clock read, an unkeyed follow-up) is caught on at least one seed. (This is the
// same-order companion to interleave_test.go's Property 1, which permutes the
// order; together they pin both axes.)
func TestEngineReplayDeterminismGenerated(t *testing.T) {
	t.Parallel()
	cat := catalog()
	seeds := propSeeds(t)
	ctx := context.Background()

	runOrdered := func(evs []Event) []string {
		run := newPropRun(t)
		for _, ev := range evs {
			if _, err := run.eng.Step(ctx, ev); err != nil {
				// Skip illegal events for this phase, mirroring the handlers.
				continue
			}
		}
		_ = run.eng.Tick(ctx)
		return run.transitionLog(t)
	}

	for s := 0; s < seeds; s++ {
		seed := int64(s)
		rng := rand.New(rand.NewSource(seed + 40_000))
		evs := batchEvents(generateBatch(rng, cat))

		a := runOrdered(evs)
		b := runOrdered(evs)
		if len(a) != len(b) {
			t.Fatalf("seed=%d non-deterministic length: %v vs %v", seed, a, b)
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("seed=%d non-deterministic replay at %d: %q vs %q\n a=%v\n b=%v",
					seed, i, a[i], b[i], a, b)
			}
		}
	}
}
