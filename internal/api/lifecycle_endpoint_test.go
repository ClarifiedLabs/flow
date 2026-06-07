package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
)

// TestEngineEveryBundleHasAnEngine is the regression for the nil-engine panic:
// every project bundle the registry opens must carry a lifecycle engine, so a
// check report drives the engine cascade without panicking.
func TestEngineEveryBundleHasAnEngine(t *testing.T) {
	ctx := context.Background()
	fixture := newTestFixture(t)

	if fixture.Bundle.Engine == nil {
		t.Fatalf("expected bundle to carry a lifecycle engine")
	}

	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Minimal server issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	required := true
	var resp checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", "POST", "/v1/issues/"+issue.ID+"/checks/ci",
		reportCheckRequest{Kind: "ci", Required: &required, Verdict: "satisfied", Reporter: "test"}, 200, &resp)
	if resp.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("check verdict = %q, want satisfied", resp.Check.Verdict)
	}
}

// TestListTransitionsEndpoint verifies the lifecycle timeline endpoint surfaces
// the transitions the engine records as handlers drive it.
func TestListTransitionsEndpoint(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Transitions issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", "POST", "/v1/issues/"+issue.ID+"/schedule",
		scheduleIssueRequest{State: "up_next"}, 200, nil)

	var resp transitionsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", "GET", "/v1/issues/"+issue.ID+"/transitions", nil, 200, &resp)
	if len(resp.Transitions) == 0 {
		t.Fatalf("expected at least one transition")
	}
	found := false
	for _, tr := range resp.Transitions {
		if tr.EventKind == "schedule_issue" && tr.ToPhase == "up_next" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected schedule_issue -> up_next transition, got %+v", resp.Transitions)
	}

	// Hook tokens cannot read transition history.
	doJSONRequestAs(t, fixture.Server, "hook-token", "GET", "/v1/issues/"+issue.ID+"/transitions", nil, 403, nil)
}

// TestIssueDetailLifecycleGraph verifies the issue-detail payload carries the
// aggregated lifecycle graph (edge counts + current phase) for the web chart.
func TestIssueDetailLifecycleGraph(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Graph issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	// Two schedule hops so at least one real phase-pair edge exists (the first
	// engine event initialises workflow_state from an empty from_phase, which
	// the aggregation excludes).
	doJSONRequestAs(t, fixture.Server, "owner-token", "POST", "/v1/issues/"+issue.ID+"/schedule",
		scheduleIssueRequest{State: "up_next"}, 200, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", "POST", "/v1/issues/"+issue.ID+"/schedule",
		scheduleIssueRequest{State: "backlog"}, 200, nil)

	var resp issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", "GET", "/v1/issues/"+issue.ID, nil, 200, &resp)
	if resp.Detail == nil {
		t.Fatalf("expected issue_detail in owner response")
	}
	graph := resp.Detail.LifecycleGraph
	if graph == nil {
		t.Fatalf("expected lifecycle_graph in issue detail")
	}
	if graph.CurrentPhase != "backlog" {
		t.Errorf("current phase = %q, want backlog", graph.CurrentPhase)
	}
	found := false
	for _, edge := range graph.Edges {
		if edge.FromPhase == "up_next" && edge.ToPhase == "backlog" && edge.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected up_next->backlog edge with count 1, got %+v", graph.Edges)
	}
}

// TestIssueDetailEnrichedTimeline verifies the issue-detail payload carries an
// enriched timeline (timeline_transitions) where session_ready and
// session_state_changed rows expose the decoded session_id, session_state,
// head_sha and change_id from their payload_json. This is what lets the web UI
// timeline render a transition row's terminal/transcript controls for the exact
// session, even outside the top-N session list.
func TestIssueDetailEnrichedTimeline(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Timeline enrichment issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	running := startRunningAuthorSession(t, fixture, issue.ID)
	sessionID := running.Session.ID
	changeID := running.Session.ChangeID
	// session/ready validates the head sha against the project's exchange, so
	// point the fixture at a real git exchange with a commit on the issue branch.
	exchangePath, headSHA := createCheckConfigExchange(t)
	repointFixtureExchange(t, fixture, exchangePath)
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, changeID, headSHA); err != nil {
		t.Fatalf("record change head before ready: %v", err)
	}

	// session_state_changed -> waiting.
	var event sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+sessionID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)

	// session_ready with a head sha.
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+sessionID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)

	var resp issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusOK, &resp)
	if resp.Detail == nil {
		t.Fatalf("expected issue_detail in owner response")
	}
	if len(resp.Detail.TimelineTransitions) == 0 {
		t.Fatalf("expected enriched timeline_transitions, got none")
	}
	if len(resp.Detail.Transitions) != len(resp.Detail.TimelineTransitions) {
		t.Fatalf("timeline_transitions length = %d, want %d (parity with transitions)",
			len(resp.Detail.TimelineTransitions), len(resp.Detail.Transitions))
	}

	var stateChanged *coordinator.SessionTimelineEntry
	var ready *coordinator.SessionTimelineEntry
	for i := range resp.Detail.TimelineTransitions {
		entry := &resp.Detail.TimelineTransitions[i]
		switch entry.EventKind {
		case string(lifecycle.EventSessionStateChanged):
			stateChanged = entry
		case string(lifecycle.EventSessionReady):
			ready = entry
		}
	}

	if stateChanged == nil {
		t.Fatalf("expected a session_state_changed timeline entry, got %+v", resp.Detail.TimelineTransitions)
	}
	if stateChanged.SessionID != sessionID {
		t.Errorf("session_state_changed session_id = %q, want %q", stateChanged.SessionID, sessionID)
	}
	if stateChanged.SessionState != string(coordinator.SessionWaiting) {
		t.Errorf("session_state_changed session_state = %q, want %q", stateChanged.SessionState, coordinator.SessionWaiting)
	}
	if stateChanged.ChangeID != changeID {
		t.Errorf("session_state_changed change_id = %q, want %q", stateChanged.ChangeID, changeID)
	}

	if ready == nil {
		t.Fatalf("expected a session_ready timeline entry, got %+v", resp.Detail.TimelineTransitions)
	}
	if ready.SessionID != sessionID {
		t.Errorf("session_ready session_id = %q, want %q", ready.SessionID, sessionID)
	}
	if ready.HeadSHA != headSHA {
		t.Errorf("session_ready head_sha = %q, want %q", ready.HeadSHA, headSHA)
	}
	if ready.ChangeID != changeID {
		t.Errorf("session_ready change_id = %q, want %q", ready.ChangeID, changeID)
	}

	// Non-session transitions carry the base entry but no enriched session fields.
	for _, entry := range resp.Detail.TimelineTransitions {
		if entry.EventKind == string(lifecycle.EventSessionReady) || entry.EventKind == string(lifecycle.EventSessionStateChanged) {
			continue
		}
		if entry.SessionID != "" || entry.SessionState != "" || entry.HeadSHA != "" {
			t.Errorf("non-session transition %q leaked enriched fields: %+v", entry.EventKind, entry)
		}
	}
}
