package lifecycle

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

// unconfirmedInboxCount counts inbox rows that have not yet been confirmed.
func unconfirmedInboxCount(t *testing.T, store *flowdb.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM event_inbox WHERE confirmed_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count unconfirmed inbox rows: %v", err)
	}
	return n
}

func inboxRowCount(t *testing.T, store *flowdb.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM event_inbox`).Scan(&n); err != nil {
		t.Fatalf("count inbox rows: %v", err)
	}
	return n
}

// TestStepConfirmsInboxRowOnSuccess covers numbered case 1: a successful Step
// journals an inbox row and confirms it, leaving nothing unconfirmed.
func TestStepConfirmsInboxRowOnSuccess(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if n := inboxRowCount(t, store); n != 1 {
		t.Fatalf("inbox rows = %d, want 1 (the journaled external event)", n)
	}
	if n := unconfirmedInboxCount(t, store); n != 0 {
		t.Fatalf("unconfirmed inbox rows = %d, want 0 after a successful step", n)
	}
	// The benign no-op path (replay of an already-applied keyed event) must also
	// confirm its inbox row rather than leave it dangling.
	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "k-noop", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("noop step: %v", err)
	}
	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "k-noop", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("replay step: %v", err)
	}
	if n := unconfirmedInboxCount(t, store); n != 0 {
		t.Fatalf("unconfirmed inbox rows = %d, want 0 after benign replay", n)
	}
}

// TestRedeliverInboxAppliesCrashedRow covers numbered case 2: an unconfirmed
// inbox row (a Step that crashed before its cascade committed) is redelivered by
// Tick after the grace window; the event applies (transition row appears) and the
// row confirms.
func TestRedeliverInboxAppliesCrashedRow(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	// Simulate a crash mid-Step: an inbox row exists, but no transition committed.
	ev := Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "inbox:in-crashed", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}
	raw, err := json.Marshal(toStoredEvent(ev))
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	// created_at is well before now so it is past the grace window.
	stale := formatTime(eng.now().Add(-time.Hour))
	if _, err := store.DB().Exec(`
INSERT INTO event_inbox (id, issue_id, event_json, idempotency_key, created_at, attempts, last_error, confirmed_at)
VALUES ('in-crashed', ?, ?, 'inbox:in-crashed', ?, 0, '', NULL)`, issueID, string(raw), stale); err != nil {
		t.Fatalf("seed crashed inbox row: %v", err)
	}

	if n := transitionCount(t, store, issueID); n != 0 {
		t.Fatalf("transitions = %d, want 0 before redelivery", n)
	}
	if err := eng.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !fake.called("ScheduleIssue") {
		t.Fatalf("redelivery did not apply the crashed event: %v", fake.calls)
	}
	if n := scheduleTransitionCount(t, store, issueID); n != 1 {
		t.Fatalf("schedule transitions = %d, want 1 after redelivery", n)
	}
	if n := unconfirmedInboxCount(t, store); n != 0 {
		t.Fatalf("unconfirmed inbox rows = %d, want 0 after redelivery confirms", n)
	}
}

// TestRedeliverInboxAlreadyAppliedConfirmsWithoutDuplicate covers numbered case
// 3: redelivery of an event whose transition already exists (keyed replay) must
// confirm the inbox row without re-running the action or appending a duplicate
// transition.
func TestRedeliverInboxAlreadyAppliedConfirmsWithoutDuplicate(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	// First, a real Step applies the event (transition keyed "inbox:in-dup").
	if _, err := eng.Step(ctx, Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "inbox:in-dup", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}); err != nil {
		t.Fatalf("apply step: %v", err)
	}
	appliedCalls := countCalls(fake.calls, "ScheduleIssue")
	scheduleRows := scheduleTransitionCount(t, store, issueID)

	// Now seed a stale unconfirmed inbox row carrying the SAME idempotency key, as
	// if the original Step's confirm had been lost to a crash.
	ev := Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "inbox:in-dup", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}
	raw, err := json.Marshal(toStoredEvent(ev))
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	stale := formatTime(eng.now().Add(-time.Hour))
	if _, err := store.DB().Exec(`
INSERT INTO event_inbox (id, issue_id, event_json, idempotency_key, created_at, attempts, last_error, confirmed_at)
VALUES ('in-dup', ?, ?, 'inbox:in-dup', ?, 0, '', NULL)`, issueID, string(raw), stale); err != nil {
		t.Fatalf("seed dup inbox row: %v", err)
	}

	if err := eng.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := countCalls(fake.calls, "ScheduleIssue"); got != appliedCalls {
		t.Fatalf("ScheduleIssue calls grew from %d to %d on replay (replay must skip the action)", appliedCalls, got)
	}
	if got := scheduleTransitionCount(t, store, issueID); got != scheduleRows {
		t.Fatalf("schedule transitions grew from %d to %d on replay (replay guard must dedupe)", scheduleRows, got)
	}
	if n := unconfirmedInboxCount(t, store); n != 0 {
		t.Fatalf("unconfirmed inbox rows = %d, want 0 (replay still confirms)", n)
	}
}

// TestRedeliverInboxParksPoisonRow covers numbered case 4: a poison row whose
// event_json cannot be parsed is retried until the attempt cap, then parked
// (confirmed) with its last_error retained.
func TestRedeliverInboxParksPoisonRow(t *testing.T) {
	eng, _, store, issueID := newEngineTest(t)
	ctx := context.Background()

	stale := formatTime(eng.now().Add(-time.Hour))
	if _, err := store.DB().Exec(`
INSERT INTO event_inbox (id, issue_id, event_json, idempotency_key, created_at, attempts, last_error, confirmed_at)
VALUES ('in-poison', ?, '{not json', 'inbox:in-poison', ?, 0, '', NULL)`, issueID, stale); err != nil {
		t.Fatalf("seed poison inbox row: %v", err)
	}

	// Each tick redelivers the poison row; it should keep bumping attempts and stay
	// unconfirmed until the cap, then park.
	for i := 0; i < maxInboxAttempts; i++ {
		if err := eng.redeliverInbox(ctx); err == nil {
			t.Fatalf("redeliver %d: expected an error from the poison row", i)
		}
	}

	var attempts int
	var lastError string
	var confirmed *string
	if err := store.DB().QueryRow(
		`SELECT attempts, last_error, confirmed_at FROM event_inbox WHERE id = 'in-poison'`).Scan(&attempts, &lastError, &confirmed); err != nil {
		t.Fatalf("read poison row: %v", err)
	}
	if confirmed == nil {
		t.Fatalf("poison row not parked after %d attempts", maxInboxAttempts)
	}
	if attempts < maxInboxAttempts {
		t.Fatalf("poison row attempts = %d, want >= %d", attempts, maxInboxAttempts)
	}
	if lastError == "" {
		t.Fatalf("poison row last_error cleared; want the parse error retained")
	}

	// A parked poison row no longer surfaces on subsequent redeliveries.
	if err := eng.redeliverInbox(ctx); err != nil {
		t.Fatalf("redeliver after park still errors on the poison row: %v", err)
	}
}

// TestRedeliverInboxSkipsYoungRows covers numbered case 5: an unconfirmed row
// younger than the grace window is NOT redelivered (it may be an in-flight Step).
func TestRedeliverInboxSkipsYoungRows(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleBacklog

	ev := Event{Kind: EventScheduleIssue, IssueID: issueID, IdempotencyKey: "inbox:in-young", Payload: EventPayload{Schedule: coordinator.ScheduleUpNext}}
	raw, err := json.Marshal(toStoredEvent(ev))
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	// created_at = now: inside the grace window, so redelivery must skip it.
	fresh := formatTime(eng.now())
	if _, err := store.DB().Exec(`
INSERT INTO event_inbox (id, issue_id, event_json, idempotency_key, created_at, attempts, last_error, confirmed_at)
VALUES ('in-young', ?, ?, 'inbox:in-young', ?, 0, '', NULL)`, issueID, string(raw), fresh); err != nil {
		t.Fatalf("seed young inbox row: %v", err)
	}

	if err := eng.redeliverInbox(ctx); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if fake.called("ScheduleIssue") {
		t.Fatalf("young (in-flight) inbox row was redelivered: %v", fake.calls)
	}
	if n := unconfirmedInboxCount(t, store); n != 1 {
		t.Fatalf("unconfirmed inbox rows = %d, want 1 (young row left alone)", n)
	}
}

// TestTimerDispatchDoesNotJournalInbox covers numbered case 6: timer-dispatched
// events keep their own claim/confirm and must NOT create inbox rows (no
// double-journaling).
func TestTimerDispatchDoesNotJournalInbox(t *testing.T) {
	eng, fake, store, issueID := newEngineTest(t)
	ctx := context.Background()
	fake.issue.TriageState = coordinator.TriageAccepted
	fake.issue.ScheduleState = coordinator.ScheduleUpNext

	if _, err := eng.ScheduleTimer(ctx, issueID, EventEnsureAuthorJob, eng.now().Add(-time.Minute), EventPayload{}); err != nil {
		t.Fatalf("schedule timer: %v", err)
	}
	if _, err := eng.DrainDueTimers(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !fake.called("EnsureAuthorJob") {
		t.Fatalf("timer did not dispatch")
	}
	if n := inboxRowCount(t, store); n != 0 {
		t.Fatalf("inbox rows = %d, want 0 (timers must not double-journal)", n)
	}
}

// TestStoredEventRoundTripPreservesFields covers numbered case 7: the inbox's
// Event JSON round-trip preserves every field a transition reads — kind, the
// routing ids, the actor principal, and the payload.
func TestStoredEventRoundTripPreservesFields(t *testing.T) {
	projectID := "proj-1"
	sourceIssue := "iss-src"
	required := true
	exit := 2
	jobID := "job-9"
	original := Event{
		Kind:      EventCheckReported,
		IssueID:   "iss-1",
		ChangeID:  "chg-1",
		ThreadID:  "thr-1",
		SessionID: "ses-1",
		Actor: coordinator.Principal{
			Scope:         coordinator.TokenScopeSession,
			Subject:       "ses-1",
			TokenHash:     "deadbeef",
			ProjectID:     &projectID,
			SourceIssueID: &sourceIssue,
		},
		IdempotencyKey: "inbox:in-rt",
		Audit: EventAudit{
			Method:       "POST",
			Path:         "/v1/issues/iss-1/checks/ci",
			Principal:    "session:ses-1",
			ProjectID:    projectID,
			ProjectName:  "demo",
			IssueID:      "iss-1",
			ChangeID:     "chg-1",
			ThreadID:     "thr-1",
			SessionID:    "ses-1",
			UserAgent:    "FlowTest/1.0",
			WebSessionID: "web-1",
		},
		Payload: EventPayload{
			Name:        "ci",
			CheckKind:   coordinator.CheckKindCI,
			Required:    &required,
			Verdict:     coordinator.CheckBlocked,
			ExitCode:    &exit,
			Details:     "boom",
			SourceJobID: &jobID,
			Reporter:    "worker",
			HeadSHA:     "abc123",
			Schedule:    coordinator.ScheduleUpNext,
			Triage:      coordinator.TriageAccepted,
			ThreadKind:  coordinator.ClaimFixed,
			Body:        "fixed it",
		},
	}

	raw, err := json.Marshal(toStoredEvent(original))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var stored storedEvent
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := stored.toEvent()

	if got.Kind != original.Kind ||
		got.IssueID != original.IssueID ||
		got.ChangeID != original.ChangeID ||
		got.ThreadID != original.ThreadID ||
		got.SessionID != original.SessionID ||
		got.IdempotencyKey != original.IdempotencyKey {
		t.Fatalf("identity fields not preserved:\n got  %+v\n want %+v", got, original)
	}
	if got.Actor.Scope != original.Actor.Scope ||
		got.Actor.Subject != original.Actor.Subject ||
		got.Actor.TokenHash != original.Actor.TokenHash ||
		got.Actor.ProjectID == nil || *got.Actor.ProjectID != projectID ||
		got.Actor.SourceIssueID == nil || *got.Actor.SourceIssueID != sourceIssue {
		t.Fatalf("actor not preserved:\n got  %+v\n want %+v", got.Actor, original.Actor)
	}
	if got.Actor.Actor() != original.Actor.Actor() {
		t.Fatalf("actor label = %q, want %q", got.Actor.Actor(), original.Actor.Actor())
	}
	if got.Audit != original.Audit {
		t.Fatalf("audit not preserved:\n got  %+v\n want %+v", got.Audit, original.Audit)
	}
	// Compare the payload field-by-field through its own JSON to keep the test
	// robust to additive payload fields.
	gotPayload, _ := json.Marshal(got.Payload)
	wantPayload, _ := json.Marshal(original.Payload)
	if string(gotPayload) != string(wantPayload) {
		t.Fatalf("payload not preserved:\n got  %s\n want %s", gotPayload, wantPayload)
	}
}

// TestStoredEventCoversEveryEventField guards the storedEvent mirror against
// drift: if someone adds a field to Event without mirroring it in storedEvent
// (and in toStoredEvent/toEvent), redelivery would silently drop that field on
// every crashed inbox row. Rather than wait for a production data-loss bug, this
// reflects over Event's field set and asserts each name has a counterpart in
// storedEvent. The allowlist maps the (currently 1:1) names; update it — and the
// round-trip test above — when you intentionally add or rename a field.
func TestStoredEventCoversEveryEventField(t *testing.T) {
	// eventToStored maps each Event field name to its storedEvent counterpart.
	// Identical names map to themselves; the entries are explicit so a rename is a
	// deliberate edit here, not a silent pass.
	eventToStored := map[string]string{
		"Kind":           "Kind",
		"IssueID":        "IssueID",
		"ChangeID":       "ChangeID",
		"ThreadID":       "ThreadID",
		"SessionID":      "SessionID",
		"Actor":          "Actor",
		"IdempotencyKey": "IdempotencyKey",
		"Audit":          "Audit",
		"Payload":        "Payload", // embedded whole; round-trip test asserts contents
	}

	storedFields := map[string]bool{}
	storedType := reflect.TypeOf(storedEvent{})
	for i := 0; i < storedType.NumField(); i++ {
		storedFields[storedType.Field(i).Name] = true
	}

	eventType := reflect.TypeOf(Event{})
	for i := 0; i < eventType.NumField(); i++ {
		name := eventType.Field(i).Name
		mapped, ok := eventToStored[name]
		if !ok {
			t.Fatalf("Event field %q has no storedEvent mapping: add it to storedEvent, "+
				"toStoredEvent/toEvent, this allowlist, and the storedEvent round-trip test, "+
				"or redelivery will silently drop it", name)
		}
		if !storedFields[mapped] {
			t.Fatalf("Event field %q maps to storedEvent field %q, which does not exist: "+
				"fix storedEvent / toStoredEvent / toEvent so redelivery preserves it", name, mapped)
		}
	}

	// Catch the reverse drift too: a storedEvent field with no Event source is
	// dead weight (or a copy-paste bug) and should be cleaned up.
	mappedStored := map[string]bool{}
	for _, v := range eventToStored {
		mappedStored[v] = true
	}
	for name := range storedFields {
		if !mappedStored[name] {
			t.Fatalf("storedEvent field %q has no Event source in the allowlist: "+
				"remove it or map a real Event field to it", name)
		}
	}
}

func TestTransitionPayloadJSONEmbedsAuditWithoutDroppingPayload(t *testing.T) {
	required := true
	raw, err := transitionPayloadJSON(Event{
		Kind:    EventCheckReported,
		IssueID: "iss-1",
		Audit: EventAudit{
			Method:    "POST",
			Path:      "/v1/issues/iss-1/checks/ci",
			Principal: "owner:web",
			ProjectID: "proj-1",
			IssueID:   "iss-1",
		},
		Payload: EventPayload{
			Name:       "ci",
			Required:   &required,
			Verdict:    coordinator.CheckSatisfied,
			Details:    "ok",
			Reporter:   "worker",
			ThreadKind: coordinator.ClaimFixed,
		},
	})
	if err != nil {
		t.Fatalf("transition payload json: %v", err)
	}

	var decoded struct {
		Name     string     `json:"name"`
		Required *bool      `json:"required"`
		Verdict  string     `json:"verdict"`
		Details  string     `json:"details"`
		Reporter string     `json:"reporter"`
		Audit    EventAudit `json:"audit"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal transition payload: %v", err)
	}
	if decoded.Name != "ci" || decoded.Required == nil || !*decoded.Required || decoded.Verdict != string(coordinator.CheckSatisfied) || decoded.Details != "ok" || decoded.Reporter != "worker" {
		t.Fatalf("payload fields not preserved in %s", raw)
	}
	if decoded.Audit.Method != "POST" ||
		decoded.Audit.Path != "/v1/issues/iss-1/checks/ci" ||
		decoded.Audit.Principal != "owner:web" ||
		decoded.Audit.ProjectID != "proj-1" ||
		decoded.Audit.IssueID != "iss-1" {
		t.Fatalf("audit not embedded in transition payload: %+v", decoded.Audit)
	}
}
