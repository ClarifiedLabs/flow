package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TransitionLogEntry is one row of the append-only lifecycle transition log,
// surfaced by the timeline endpoint, the web Lifecycle section, and the
// `flow transitions` CLI.
type TransitionLogEntry struct {
	Seq         int64     `json:"seq"`
	IssueID     string    `json:"issue_id"`
	FromPhase   string    `json:"from_phase"`
	EventKind   string    `json:"event_kind"`
	GuardResult string    `json:"guard_result"`
	ToPhase     string    `json:"to_phase"`
	Actor       string    `json:"actor"`
	CreatedAt   time.Time `json:"created_at"`
}

// SessionTimelineEntry is a TransitionLogEntry augmented with fields decoded
// from its payload_json so the web UI timeline can attribute a transition to
// a specific session and render its terminal/transcript controls. Only
// session-related events (session_ready, session_state_changed) populate the
// session fields; other rows carry the base entry unchanged.
type SessionTimelineEntry struct {
	TransitionLogEntry
	SessionID    string `json:"session_id,omitempty"`
	SessionState string `json:"session_state,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	ChangeID     string `json:"change_id,omitempty"`
}

// SessionStateTransition is a decoded session_state_changed transition row.
type SessionStateTransition struct {
	FromPhase string
	ToPhase   string
	State     SessionRuntimeState
	CreatedAt time.Time
}

// TransitionService reads the lifecycle transition log written by the engine.
type TransitionService struct {
	db *sql.DB
}

func NewTransitionService(database *sql.DB) *TransitionService {
	return &TransitionService{db: database}
}

// ListForIssue returns an issue's transition history, most recent first, capped
// at limit (defaulting to 100).
func (s *TransitionService) ListForIssue(ctx context.Context, issueID string, limit int) ([]TransitionLogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, issue_id, from_phase, event_kind, guard_result, to_phase, actor, created_at
FROM transitions
WHERE issue_id = ?
ORDER BY seq DESC
LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, fmt.Errorf("list transitions: %w", err)
	}
	defer rows.Close()

	var entries []TransitionLogEntry
	for rows.Next() {
		var entry TransitionLogEntry
		var createdAt string
		if err := rows.Scan(&entry.Seq, &entry.IssueID, &entry.FromPhase, &entry.EventKind,
			&entry.GuardResult, &entry.ToPhase, &entry.Actor, &createdAt); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		entry.CreatedAt = parsed
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transitions: %w", err)
	}

	return entries, nil
}

// ListForIssueWithPayload returns an issue's transition history (most recent
// first, capped at limit) with session-related rows enriched from their
// payload_json. session_ready rows expose the head_sha (and session_id/change_id
// from the audit envelope); session_state_changed rows expose the session_state
// transition plus session_id/change_id. Non-session rows are returned unchanged.
// The UI timeline uses this to correlate transition rows to a specific session
// and render its terminal/transcript controls even outside the top-N session
// list.
func (s *TransitionService) ListForIssueWithPayload(ctx context.Context, issueID string, limit int) ([]SessionTimelineEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, issue_id, from_phase, event_kind, payload_json, guard_result, to_phase, actor, created_at
FROM transitions
WHERE issue_id = ?
ORDER BY seq DESC
LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, fmt.Errorf("list transitions with payload: %w", err)
	}
	defer rows.Close()

	var entries []SessionTimelineEntry
	for rows.Next() {
		var rawPayload string
		var createdAt string
		var entry SessionTimelineEntry
		if err := rows.Scan(&entry.Seq, &entry.IssueID, &entry.FromPhase, &entry.EventKind,
			&rawPayload, &entry.GuardResult, &entry.ToPhase, &entry.Actor, &createdAt); err != nil {
			return nil, fmt.Errorf("scan transition with payload: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		entry.CreatedAt = parsed
		decodeTimelinePayload(&entry, rawPayload)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transitions with payload: %w", err)
	}

	return entries, nil
}

// decodeTimelinePayload reads the session fields the UI timeline needs from a
// transition row's payload_json. It is intentionally permissive: malformed or
// empty payloads leave the entry unchanged so history is never hidden.
func decodeTimelinePayload(entry *SessionTimelineEntry, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || (entry.EventKind != "session_ready" && entry.EventKind != "session_state_changed") {
		return
	}
	var payload struct {
		HeadSHA      string              `json:"head_sha"`
		SessionState SessionRuntimeState `json:"session_state"`
		Audit        struct {
			SessionID string `json:"session_id"`
			ChangeID  string `json:"change_id"`
		} `json:"audit"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return
	}
	entry.SessionID = strings.TrimSpace(payload.Audit.SessionID)
	entry.ChangeID = strings.TrimSpace(payload.Audit.ChangeID)
	if entry.EventKind == "session_ready" {
		entry.HeadSHA = strings.TrimSpace(payload.HeadSHA)
	}
	if entry.EventKind == "session_state_changed" {
		entry.SessionState = strings.TrimSpace(string(payload.SessionState))
	}
}

// RecentSessionStateTransitions returns recent session_state_changed entries
// for one session, newest first. Session ids are stored in transition audit
// payloads, so rows without matching audit data are ignored.
func (s *TransitionService) RecentSessionStateTransitions(ctx context.Context, issueID string, sessionID string, since time.Time, limit int) ([]SessionStateTransition, error) {
	issueID = strings.TrimSpace(issueID)
	sessionID = strings.TrimSpace(sessionID)
	if issueID == "" {
		return nil, fmt.Errorf("issue id is required")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT from_phase, to_phase, payload_json, created_at
FROM transitions
WHERE issue_id = ?
	AND event_kind = 'session_state_changed'
	AND created_at >= ?
ORDER BY seq DESC
LIMIT ?`, issueID, formatTime(since.UTC()), limit)
	if err != nil {
		return nil, fmt.Errorf("list session state transitions: %w", err)
	}
	defer rows.Close()

	var transitions []SessionStateTransition
	for rows.Next() {
		var transition SessionStateTransition
		var rawPayload string
		var createdAt string
		if err := rows.Scan(&transition.FromPhase, &transition.ToPhase, &rawPayload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan session state transition: %w", err)
		}
		parsedCreatedAt, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		var payload struct {
			SessionState SessionRuntimeState `json:"session_state"`
			Audit        struct {
				SessionID string `json:"session_id"`
			} `json:"audit"`
		}
		if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
			return nil, fmt.Errorf("decode session state transition payload: %w", err)
		}
		if strings.TrimSpace(payload.Audit.SessionID) != sessionID {
			continue
		}
		transition.State = payload.SessionState
		transition.CreatedAt = parsedCreatedAt
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session state transitions: %w", err)
	}

	return transitions, nil
}

// EdgeCount is one observed from→to phase transition and how many times it
// occurred for an issue.
type EdgeCount struct {
	FromPhase string `json:"from_phase"`
	ToPhase   string `json:"to_phase"`
	Count     int    `json:"count"`
}

// LifecycleGraphSummary is the aggregation backing the web UI's lifecycle
// flow chart: per-edge transition counts, the issue's current phase, and the
// reviewer/verifier "sent back" tallies. The tallies come from check_reported
// payloads because phase pairs alone cannot attribute a bounce to a reviewer
// or verifier (both land the issue in critique).
type LifecycleGraphSummary struct {
	CurrentPhase  string      `json:"current_phase"`
	Edges         []EdgeCount `json:"edges"`
	ReviewerSends int         `json:"reviewer_sends"`
	VerifierSends int         `json:"verifier_sends"`
}

// GraphSummaryForIssue aggregates the full transition log for one issue. It
// runs uncapped queries (unlike ListForIssue) so counts stay accurate for
// long-lived issues. Initial rows (from_phase=”) and self-loops are excluded
// from edges: the former is graph entry, and the engine logs a row on every
// step even when the phase is unchanged.
func (s *TransitionService) GraphSummaryForIssue(ctx context.Context, issueID string) (LifecycleGraphSummary, error) {
	var summary LifecycleGraphSummary

	rows, err := s.db.QueryContext(ctx, `
SELECT from_phase, to_phase, COUNT(*)
FROM transitions
WHERE issue_id = ? AND from_phase <> '' AND from_phase <> to_phase
GROUP BY from_phase, to_phase`, issueID)
	if err != nil {
		return summary, fmt.Errorf("count transition edges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var edge EdgeCount
		if err := rows.Scan(&edge.FromPhase, &edge.ToPhase, &edge.Count); err != nil {
			return summary, fmt.Errorf("scan transition edge: %w", err)
		}
		summary.Edges = append(summary.Edges, edge)
	}
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("iterate transition edges: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `
SELECT to_phase FROM transitions WHERE issue_id = ? ORDER BY seq DESC LIMIT 1`, issueID).
		Scan(&summary.CurrentPhase)
	if err != nil && err != sql.ErrNoRows {
		return summary, fmt.Errorf("read current phase: %w", err)
	}

	checkRows, err := s.db.QueryContext(ctx, `
SELECT payload_json FROM transitions WHERE issue_id = ? AND event_kind = 'check_reported'`, issueID)
	if err != nil {
		return summary, fmt.Errorf("read check payloads: %w", err)
	}
	defer checkRows.Close()
	for checkRows.Next() {
		var raw string
		if err := checkRows.Scan(&raw); err != nil {
			return summary, fmt.Errorf("scan check payload: %w", err)
		}
		// Local struct rather than lifecycle.EventPayload: coordinator must not
		// import lifecycle (lifecycle imports coordinator).
		var payload struct {
			CheckKind string `json:"check_kind"`
			Verdict   string `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return summary, fmt.Errorf("decode check payload: %w", err)
		}
		if payload.Verdict != string(CheckBlocked) {
			continue
		}
		// A blocked check counts as a "sent back" whether or not it was a
		// required gate: a non-required reviewer bounce still bounced the work.
		switch payload.CheckKind {
		case string(CheckKindReviewer):
			summary.ReviewerSends++
		case string(CheckKindVerifier):
			summary.VerifierSends++
		}
	}
	if err := checkRows.Err(); err != nil {
		return summary, fmt.Errorf("iterate check payloads: %w", err)
	}

	return summary, nil
}
