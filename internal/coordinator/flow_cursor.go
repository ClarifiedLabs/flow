package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// FlowPhaseState is the cursor's position state within its current work phase.
type FlowPhaseState string

const (
	// FlowPhasePending: the phase is queued to run (job ensured or about to be).
	FlowPhasePending FlowPhaseState = "pending"
	// FlowPhaseRunning: reserved for surfacing an actively-running phase.
	FlowPhaseRunning FlowPhaseState = "running"
	// FlowPhaseAwaitingApproval: the phase finished behind a human gate; the
	// flow is paused until the handoff is approved or sent back.
	FlowPhaseAwaitingApproval FlowPhaseState = "awaiting_approval"
	// FlowPhaseCompleted: the final phase finished and the change entered
	// review; the work pipeline is done.
	FlowPhaseCompleted FlowPhaseState = "completed"
)

// FlowCursor is an issue's frozen position within its flow: the snapshot
// resolved at schedule time plus the current phase index and state.
type FlowCursor struct {
	IssueID      string         `json:"issue_id"`
	Snapshot     FlowSnapshot   `json:"snapshot"`
	PhaseIndex   int            `json:"phase_index"`
	PhaseState   FlowPhaseState `json:"phase_state"`
	GateFeedback string         `json:"gate_feedback,omitempty"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// CurrentPhase returns the snapshot phase under the cursor, or false when the
// index is out of range (defensive: a well-formed cursor is always in range).
func (c FlowCursor) CurrentPhase() (FlowPhaseSnapshot, bool) {
	return c.Snapshot.PhaseAt(c.PhaseIndex)
}

// OnFinalPhase reports whether the cursor is on the last work phase.
func (c FlowCursor) OnFinalPhase() bool {
	return c.PhaseIndex >= len(c.Snapshot.Phases)-1
}

// IndicatesWorking reports whether the cursor alone (no active session) keeps
// the issue in the working phase: paused at a human gate, or mid-pipeline past
// the first phase.
func (c FlowCursor) IndicatesWorking() bool {
	return cursorIndicatesWorking(c.PhaseIndex, c.PhaseState)
}

// PhaseHandoff is one work phase's completion artifact.
type PhaseHandoff struct {
	IssueID    string    `json:"issue_id"`
	PhaseIndex int       `json:"phase_index"`
	PhaseName  string    `json:"phase_name"`
	Content    string    `json:"content"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type StorePhaseHandoffInput struct {
	IssueID    string
	PhaseIndex int
	PhaseName  string
	Content    string
	HeadSHA    string
}

// FlowCursorService owns issue_flow_cursor and issue_phase_handoffs. Cursor
// mutations are compare-and-swap on the phase index so the lifecycle engine's
// at-least-once event delivery stays idempotent: a replayed advance/pause
// simply matches zero rows.
type FlowCursorService struct {
	db    *sql.DB
	flows *FlowService
	now   func() time.Time
}

func NewFlowCursorService(database *sql.DB, flows *FlowService) *FlowCursorService {
	return &FlowCursorService{db: database, flows: flows, now: sqlitex.UTCNow}
}

// GetCursor loads the issue's cursor; ok is false when the issue has none.
func (s *FlowCursorService) GetCursor(ctx context.Context, issueID string) (FlowCursor, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT issue_id, flow_snapshot_json, phase_index, phase_state, gate_feedback, updated_at
FROM issue_flow_cursor WHERE issue_id = ?`, issueID)

	var cursor FlowCursor
	var snapshotJSON, updatedAt string
	var state string
	err := row.Scan(&cursor.IssueID, &snapshotJSON, &cursor.PhaseIndex, &state, &cursor.GateFeedback, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FlowCursor{}, false, nil
	}
	if err != nil {
		return FlowCursor{}, false, fmt.Errorf("load flow cursor: %w", err)
	}
	cursor.PhaseState = FlowPhaseState(state)
	if err := json.Unmarshal([]byte(snapshotJSON), &cursor.Snapshot); err != nil {
		return FlowCursor{}, false, fmt.Errorf("decode flow snapshot for %s: %w", issueID, err)
	}
	if cursor.UpdatedAt, err = sqlitex.ParseTime(updatedAt); err != nil {
		return FlowCursor{}, false, fmt.Errorf("parse flow cursor updated_at: %w", err)
	}
	return cursor, true, nil
}

// EnsureCursor returns the issue's cursor, freezing one from the issue's flow
// (or the project default) on first use — i.e. when the issue is first
// scheduled into work. A missing or deleted flow falls back to the project
// default; ok is false when no flow could be resolved at all (a project with
// no flows configured), which callers treat as the implicit single-phase flow.
func (s *FlowCursorService) EnsureCursor(ctx context.Context, issueID string) (FlowCursor, bool, error) {
	cursor, ok, err := s.GetCursor(ctx, issueID)
	if err != nil {
		return FlowCursor{}, false, err
	}
	if ok {
		return cursor, true, nil
	}

	var flowID sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT flow_id FROM issues WHERE id = ?`, issueID).Scan(&flowID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FlowCursor{}, false, fmt.Errorf("issue %s not found", issueID)
		}
		return FlowCursor{}, false, fmt.Errorf("load issue flow id: %w", err)
	}

	snapshot, err := s.flows.ResolveSnapshot(ctx, flowID.String)
	if err != nil && flowID.Valid && strings.TrimSpace(flowID.String) != "" {
		// The selected flow may have been deleted since the issue chose it;
		// fall back to the project default.
		snapshot, err = s.flows.ResolveSnapshot(ctx, "")
	}
	if err != nil {
		// No resolvable flow (e.g. a project with no flows configured): the
		// caller runs the implicit single-phase behavior without a cursor.
		return FlowCursor{}, false, nil
	}

	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return FlowCursor{}, false, fmt.Errorf("encode flow snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO issue_flow_cursor (issue_id, flow_snapshot_json, phase_index, phase_state, gate_feedback, updated_at)
VALUES (?, ?, 0, ?, '', ?)
ON CONFLICT(issue_id) DO NOTHING`,
		issueID, string(snapshotJSON), string(FlowPhasePending), sqlitex.FormatTime(s.now().UTC())); err != nil {
		return FlowCursor{}, false, fmt.Errorf("insert flow cursor: %w", err)
	}

	return s.GetCursor(ctx, issueID)
}

// AdvanceCursor moves the cursor from fromIndex to the next phase (state
// pending, feedback cleared). Returns false when the cursor is no longer at
// fromIndex — an idempotent replay no-op.
func (s *FlowCursorService) AdvanceCursor(ctx context.Context, issueID string, fromIndex int) (bool, error) {
	return s.casCursor(ctx, issueID, fromIndex, `
UPDATE issue_flow_cursor
SET phase_index = phase_index + 1, phase_state = ?, gate_feedback = '', updated_at = ?
WHERE issue_id = ? AND phase_index = ?`, string(FlowPhasePending))
}

// PauseCursor parks the cursor at atIndex awaiting human approval.
func (s *FlowCursorService) PauseCursor(ctx context.Context, issueID string, atIndex int) (bool, error) {
	return s.casCursor(ctx, issueID, atIndex, `
UPDATE issue_flow_cursor
SET phase_state = ?, gate_feedback = '', updated_at = ?
WHERE issue_id = ? AND phase_index = ?`, string(FlowPhaseAwaitingApproval))
}

// ResumeCursor sends a gate-paused phase back to work with feedback. Returns
// false unless the cursor is at atIndex awaiting approval.
func (s *FlowCursorService) ResumeCursor(ctx context.Context, issueID string, atIndex int, feedback string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE issue_flow_cursor
SET phase_state = ?, gate_feedback = ?, updated_at = ?
WHERE issue_id = ? AND phase_index = ? AND phase_state = ?`,
		string(FlowPhasePending), strings.TrimSpace(feedback), sqlitex.FormatTime(s.now().UTC()),
		issueID, atIndex, string(FlowPhaseAwaitingApproval))
	if err != nil {
		return false, fmt.Errorf("resume flow cursor: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// CompleteCursor marks the work pipeline finished at atIndex (the final phase
// readied its change into review).
func (s *FlowCursorService) CompleteCursor(ctx context.Context, issueID string, atIndex int) (bool, error) {
	return s.casCursor(ctx, issueID, atIndex, `
UPDATE issue_flow_cursor
SET phase_state = ?, gate_feedback = '', updated_at = ?
WHERE issue_id = ? AND phase_index = ?`, string(FlowPhaseCompleted))
}

func (s *FlowCursorService) casCursor(ctx context.Context, issueID string, index int, query, state string) (bool, error) {
	result, err := s.db.ExecContext(ctx, query, state, sqlitex.FormatTime(s.now().UTC()), issueID, index)
	if err != nil {
		return false, fmt.Errorf("update flow cursor: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ClearFlowState removes the issue's cursor and phase handoffs (issue reset:
// the next ensure re-freezes a fresh snapshot from the live flow).
func (s *FlowCursorService) ClearFlowState(ctx context.Context, issueID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM issue_flow_cursor WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("clear flow cursor: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM issue_phase_handoffs WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("clear phase handoffs: %w", err)
	}
	return nil
}

// StorePhaseHandoff upserts one phase's completion artifact.
func (s *FlowCursorService) StorePhaseHandoff(ctx context.Context, input StorePhaseHandoffInput) error {
	now := sqlitex.FormatTime(s.now().UTC())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO issue_phase_handoffs (issue_id, phase_index, phase_name, content, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(issue_id, phase_index) DO UPDATE SET
	phase_name = excluded.phase_name,
	content = excluded.content,
	head_sha = excluded.head_sha,
	updated_at = excluded.updated_at`,
		input.IssueID, input.PhaseIndex, strings.TrimSpace(input.PhaseName),
		input.Content, strings.TrimSpace(input.HeadSHA), now, now)
	if err != nil {
		return fmt.Errorf("store phase handoff: %w", err)
	}
	return nil
}

// PhaseHandoff returns the handoff stored for one phase index.
func (s *FlowCursorService) PhaseHandoff(ctx context.Context, issueID string, phaseIndex int) (PhaseHandoff, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT issue_id, phase_index, phase_name, content, head_sha, created_at, updated_at
FROM issue_phase_handoffs WHERE issue_id = ? AND phase_index = ?`, issueID, phaseIndex)
	handoff, err := scanPhaseHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PhaseHandoff{}, false, nil
	}
	if err != nil {
		return PhaseHandoff{}, false, err
	}
	return handoff, true, nil
}

// PhaseHandoffs returns every stored handoff for the issue in phase order.
func (s *FlowCursorService) PhaseHandoffs(ctx context.Context, issueID string) ([]PhaseHandoff, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT issue_id, phase_index, phase_name, content, head_sha, created_at, updated_at
FROM issue_phase_handoffs WHERE issue_id = ? ORDER BY phase_index`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list phase handoffs: %w", err)
	}
	defer rows.Close()

	var handoffs []PhaseHandoff
	for rows.Next() {
		handoff, err := scanPhaseHandoff(rows)
		if err != nil {
			return nil, err
		}
		handoffs = append(handoffs, handoff)
	}
	return handoffs, rows.Err()
}

func scanPhaseHandoff(row rowScanner) (PhaseHandoff, error) {
	var handoff PhaseHandoff
	var createdAt, updatedAt string
	if err := row.Scan(&handoff.IssueID, &handoff.PhaseIndex, &handoff.PhaseName,
		&handoff.Content, &handoff.HeadSHA, &createdAt, &updatedAt); err != nil {
		return PhaseHandoff{}, err
	}
	var err error
	if handoff.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
		return PhaseHandoff{}, fmt.Errorf("parse phase handoff created_at: %w", err)
	}
	if handoff.UpdatedAt, err = sqlitex.ParseTime(updatedAt); err != nil {
		return PhaseHandoff{}, fmt.Errorf("parse phase handoff updated_at: %w", err)
	}
	return handoff, nil
}

// cursorStateForIssue is the lightweight cursor read shared by the phase
// derivations and wait-reason overlay: index and state without decoding the
// snapshot JSON.
func cursorStateForIssue(ctx context.Context, db *sql.DB, issueID string) (int, FlowPhaseState, bool, error) {
	var index int
	var state string
	err := db.QueryRowContext(ctx,
		`SELECT phase_index, phase_state FROM issue_flow_cursor WHERE issue_id = ?`, issueID).Scan(&index, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("load cursor state: %w", err)
	}
	return index, FlowPhaseState(state), true, nil
}

// cursorIndicatesWorking reports whether the cursor alone (no active session)
// keeps the issue in the working phase: paused at a human gate, or mid-pipeline
// past the first phase.
func cursorIndicatesWorking(index int, state FlowPhaseState) bool {
	if state == FlowPhaseAwaitingApproval {
		return true
	}
	return index > 0 && state != FlowPhaseCompleted
}
