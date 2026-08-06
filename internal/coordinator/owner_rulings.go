package coordinator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const (
	OwnerRulingSchemaVersion = 1
	OwnerRulingEventKind     = "workflow_owner_ruling_recorded"
	MaxOwnerRulingBodyBytes  = 4 * 1024
	MaxOwnerGuidanceBytes    = 2 * 1024
)

type OwnerRulingSource string

const (
	OwnerRulingSourceOwner               OwnerRulingSource = "owner"
	OwnerRulingSourceReviewScopeDecision OwnerRulingSource = "review_scope_decision"
	OwnerRulingSourceConvergenceReturn   OwnerRulingSource = "convergence_return"
)

type OwnerRulingDecision struct {
	WaitID         string `json:"wait_id"`
	DecisionKey    string `json:"decision_key"`
	Choice         string `json:"choice"`
	CommentIndexes []int  `json:"comment_indexes"`
	ReportSHA256   string `json:"report_sha256"`
}

type OwnerRuling struct {
	SchemaVersion int                  `json:"schema_version"`
	RulingID      string               `json:"ruling_id"`
	Body          string               `json:"body"`
	Source        OwnerRulingSource    `json:"source"`
	SupersedesID  string               `json:"supersedes_id,omitempty"`
	NodeRunID     string               `json:"node_run_id,omitempty"`
	Decision      *OwnerRulingDecision `json:"decision,omitempty"`
	WorkflowRunID string               `json:"workflow_run_id"`
	TaskID        string               `json:"task_id"`
	Actor         string               `json:"actor"`
	CreatedAt     time.Time            `json:"created_at"`
}

type OwnerRulingDelivery struct {
	TargetedSessions  int      `json:"targeted_sessions"`
	QueuedSessions    int      `json:"queued_sessions"`
	DuplicateSessions int      `json:"duplicate_sessions"`
	Warnings          []string `json:"warnings,omitempty"`
}

type RecordOwnerRulingInput struct {
	TaskID         string
	Body           string
	Source         OwnerRulingSource
	SupersedesID   string
	NodeRunID      string
	Decision       *OwnerRulingDecision
	Actor          Actor
	IdempotencyKey string
}

type RecordOwnerRulingResult struct {
	Ruling   OwnerRuling         `json:"ruling"`
	Delivery OwnerRulingDelivery `json:"delivery"`
}

type ownerRulingPayload struct {
	SchemaVersion int                  `json:"schema_version"`
	RulingID      string               `json:"ruling_id"`
	Body          string               `json:"body"`
	Source        OwnerRulingSource    `json:"source"`
	SupersedesID  string               `json:"supersedes_id,omitempty"`
	NodeRunID     string               `json:"node_run_id,omitempty"`
	Decision      *OwnerRulingDecision `json:"decision,omitempty"`
}

func (s *WorkflowRunService) RecordOwnerRuling(ctx context.Context, input RecordOwnerRulingInput) (RecordOwnerRulingResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.Body = strings.TrimSpace(input.Body)
	input.SupersedesID = strings.TrimSpace(input.SupersedesID)
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.TaskID == "" {
		return RecordOwnerRulingResult{}, errors.New("task id is required")
	}
	if input.Body == "" {
		return RecordOwnerRulingResult{}, errors.New("ruling body is required")
	}
	if len([]byte(input.Body)) > MaxOwnerRulingBodyBytes {
		return RecordOwnerRulingResult{}, fmt.Errorf("ruling body exceeds %d bytes", MaxOwnerRulingBodyBytes)
	}
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 255 {
		return RecordOwnerRulingResult{}, errors.New("idempotency key is required and must not exceed 255 characters")
	}
	if input.Source == "" {
		input.Source = OwnerRulingSourceOwner
	}
	if err := validateOwnerRulingSource(input.Source); err != nil {
		return RecordOwnerRulingResult{}, err
	}
	if input.Actor == "" {
		input.Actor = ActorHuman
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')
ORDER BY run_sequence DESC LIMIT 1`, input.TaskID))
	if errors.Is(err, sql.ErrNoRows) {
		return RecordOwnerRulingResult{}, ErrNoActiveWorkflowRun
	}
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	transitions, err := workflowTransitionsForProjection(ctx, tx, run.ID)
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	active, err := ProjectOwnerRulings(transitions)
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	if input.SupersedesID != "" {
		var target *OwnerRuling
		for index := range active {
			if active[index].RulingID == input.SupersedesID {
				target = &active[index]
				break
			}
		}
		if target == nil {
			return RecordOwnerRulingResult{}, fmt.Errorf("%w: superseded ruling is not active in this workflow run", ErrWorkflowConflict)
		}
		if target.Body == input.Body {
			return RecordOwnerRulingResult{}, fmt.Errorf("%w: replacement ruling must have a different body", ErrWorkflowConflict)
		}
	}
	if existing, ok, err := ownerRulingByIdempotencyKey(ctx, tx, run.ID, input.IdempotencyKey); err != nil {
		return RecordOwnerRulingResult{}, err
	} else if ok {
		if existing.Body != input.Body || existing.Source != input.Source || existing.SupersedesID != input.SupersedesID || existing.NodeRunID != input.NodeRunID {
			return RecordOwnerRulingResult{}, fmt.Errorf("%w: idempotency key was already used for a different ruling", ErrWorkflowConflict)
		}
		return RecordOwnerRulingResult{Ruling: existing}, nil
	}
	rulingID, err := randomPrefixedID("rule")
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	now := s.now().UTC()
	payload := ownerRulingPayload{
		SchemaVersion: OwnerRulingSchemaVersion,
		RulingID:      rulingID, Body: input.Body, Source: input.Source,
		SupersedesID: input.SupersedesID, NodeRunID: input.NodeRunID, Decision: input.Decision,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return RecordOwnerRulingResult{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID,
		FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
		EventKind: OwnerRulingEventKind, PayloadJSON: string(payloadJSON), Actor: string(input.Actor),
		IdempotencyKey: input.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return RecordOwnerRulingResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RecordOwnerRulingResult{}, err
	}
	ruling := ownerRulingFromPayload(payload, run.ID, run.TaskID, string(input.Actor), now)
	delivery := s.deliverOwnerRuling(ctx, ruling)
	s.observeOwnerRuling(ruling, delivery)
	return RecordOwnerRulingResult{Ruling: ruling, Delivery: delivery}, nil
}

func (s *WorkflowRunService) observeOwnerRuling(ruling OwnerRuling, delivery OwnerRulingDelivery) {
	if s.metrics == nil {
		return
	}
	s.metrics.OwnerRulings.Inc(map[string]string{"source": string(ruling.Source)})
	if delivery.TargetedSessions > 0 {
		s.metrics.RulingDeliveries.Add(float64(delivery.TargetedSessions), map[string]string{"result": "attempted"})
	}
	if delivery.QueuedSessions > 0 {
		s.metrics.RulingDeliveries.Add(float64(delivery.QueuedSessions), map[string]string{"result": "succeeded"})
	}
	if delivery.DuplicateSessions > 0 {
		s.metrics.RulingDeliveries.Add(float64(delivery.DuplicateSessions), map[string]string{"result": "duplicate"})
	}
	if len(delivery.Warnings) > 0 {
		s.metrics.RulingDeliveries.Add(float64(len(delivery.Warnings)), map[string]string{"result": "failed"})
	}
}

func validateOwnerRulingSource(source OwnerRulingSource) error {
	switch source {
	case OwnerRulingSourceOwner, OwnerRulingSourceReviewScopeDecision, OwnerRulingSourceConvergenceReturn:
		return nil
	default:
		return fmt.Errorf("invalid owner ruling source %q", source)
	}
}

func ownerRulingByIdempotencyKey(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, runID, key string) (OwnerRuling, bool, error) {
	var payloadJSON, taskID, actor, createdAt string
	err := queryer.QueryRowContext(ctx, `
SELECT payload_json, task_id, actor, created_at
FROM workflow_transitions
WHERE workflow_run_id = ? AND idempotency_key = ? AND event_kind = ?`, runID, key, OwnerRulingEventKind).
		Scan(&payloadJSON, &taskID, &actor, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerRuling{}, false, nil
	}
	if err != nil {
		return OwnerRuling{}, false, err
	}
	payload, err := decodeOwnerRulingPayload([]byte(payloadJSON))
	if err != nil {
		return OwnerRuling{}, false, err
	}
	created, err := sqlitex.ParseTime(createdAt)
	if err != nil {
		return OwnerRuling{}, false, err
	}
	return ownerRulingFromPayload(payload, runID, taskID, actor, created), true, nil
}

func (s *WorkflowRunService) deliverOwnerRuling(ctx context.Context, ruling OwnerRuling) OwnerRulingDelivery {
	delivery := OwnerRulingDelivery{}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM sessions
WHERE workflow_run_id = ?
	AND role IN ('author', 'reviewer', 'verifier')
	AND runtime_state IN ('starting', 'working', 'waiting')
ORDER BY created_at, id`, ruling.WorkflowRunID)
	if err != nil {
		delivery.Warnings = append(delivery.Warnings, "list live sessions: "+err.Error())
		return delivery
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			delivery.Warnings = append(delivery.Warnings, "read live session: "+err.Error())
			continue
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Err(); err != nil {
		delivery.Warnings = append(delivery.Warnings, "list live sessions: "+err.Error())
	}
	delivery.TargetedSessions = len(sessionIDs)
	messageBody := fmt.Sprintf("Owner ruling [%s; source=%s]: %s", ruling.RulingID, ruling.Source, ruling.Body)
	for _, sessionID := range sessionIDs {
		messageID, idErr := randomPrefixedID("sm")
		if idErr != nil {
			delivery.Warnings = append(delivery.Warnings, fmt.Sprintf("session %s: %v", sessionID, idErr))
			continue
		}
		result, insertErr := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO session_messages
	(id, session_id, actor, body, source_kind, source_id, state, created_at)
VALUES (?, ?, 'owner', ?, 'owner_ruling', ?, 'pending', ?)`,
			messageID, sessionID, messageBody, ruling.RulingID, formatTime(s.now().UTC()))
		if insertErr != nil {
			delivery.Warnings = append(delivery.Warnings, fmt.Sprintf("session %s: %v", sessionID, insertErr))
			continue
		}
		if count, countErr := result.RowsAffected(); countErr != nil {
			delivery.Warnings = append(delivery.Warnings, fmt.Sprintf("session %s: %v", sessionID, countErr))
		} else if count == 0 {
			delivery.DuplicateSessions++
		} else {
			delivery.QueuedSessions++
		}
	}
	return delivery
}

func workflowTransitionsForProjection(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, runID string) ([]WorkflowTransition, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT seq, task_id, workflow_run_id, from_task_state, to_task_state,
	from_node_key, to_node_key, outcome, event_kind, payload_json, actor, created_at
FROM workflow_transitions WHERE workflow_run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var transitions []WorkflowTransition
	for rows.Next() {
		var transition WorkflowTransition
		var projectedRunID sql.NullString
		var payloadJSON, createdAt string
		if err := rows.Scan(&transition.Sequence, &transition.TaskID, &projectedRunID,
			&transition.FromTaskState, &transition.ToTaskState, &transition.FromNodeKey,
			&transition.ToNodeKey, &transition.Outcome, &transition.EventKind,
			&payloadJSON, &transition.Actor, &createdAt); err != nil {
			return nil, err
		}
		transition.WorkflowRunID = projectedRunID.String
		transition.Payload = json.RawMessage(payloadJSON)
		if transition.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
			return nil, err
		}
		transitions = append(transitions, transition)
	}
	return transitions, rows.Err()
}

func ProjectOwnerRulings(transitions []WorkflowTransition) ([]OwnerRuling, error) {
	active := make([]OwnerRuling, 0)
	indexes := map[string]int{}
	seen := map[string]struct{}{}
	for _, transition := range transitions {
		if transition.EventKind != OwnerRulingEventKind {
			continue
		}
		payload, err := decodeOwnerRulingPayload(transition.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode owner ruling transition %d: %w", transition.Sequence, err)
		}
		if transition.WorkflowRunID == "" || transition.TaskID == "" {
			return nil, fmt.Errorf("decode owner ruling transition %d: run and task projections are required", transition.Sequence)
		}
		if _, duplicate := seen[payload.RulingID]; duplicate {
			return nil, fmt.Errorf("decode owner ruling transition %d: duplicate ruling id %q", transition.Sequence, payload.RulingID)
		}
		seen[payload.RulingID] = struct{}{}
		if payload.SupersedesID != "" {
			index, ok := indexes[payload.SupersedesID]
			if !ok {
				return nil, fmt.Errorf("decode owner ruling transition %d: superseded ruling %q is not active", transition.Sequence, payload.SupersedesID)
			}
			delete(indexes, payload.SupersedesID)
			active = append(active[:index], active[index+1:]...)
			for id, current := range indexes {
				if current > index {
					indexes[id] = current - 1
				}
			}
		}
		ruling := ownerRulingFromPayload(payload, transition.WorkflowRunID, transition.TaskID, transition.Actor, transition.CreatedAt)
		indexes[ruling.RulingID] = len(active)
		active = append(active, ruling)
	}
	return active, nil
}

func decodeOwnerRulingPayload(data []byte) (ownerRulingPayload, error) {
	var payload ownerRulingPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return ownerRulingPayload{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ownerRulingPayload{}, errors.New("owner ruling payload has trailing data")
	}
	if payload.SchemaVersion != OwnerRulingSchemaVersion {
		return ownerRulingPayload{}, fmt.Errorf("unsupported schema version %d", payload.SchemaVersion)
	}
	payload.RulingID = strings.TrimSpace(payload.RulingID)
	payload.Body = strings.TrimSpace(payload.Body)
	payload.SupersedesID = strings.TrimSpace(payload.SupersedesID)
	payload.NodeRunID = strings.TrimSpace(payload.NodeRunID)
	if payload.RulingID == "" || payload.Body == "" || len([]byte(payload.Body)) > MaxOwnerRulingBodyBytes {
		return ownerRulingPayload{}, errors.New("ruling id and valid body are required")
	}
	if payload.SupersedesID == payload.RulingID {
		return ownerRulingPayload{}, errors.New("ruling cannot supersede itself")
	}
	if err := validateOwnerRulingSource(payload.Source); err != nil {
		return ownerRulingPayload{}, err
	}
	return payload, nil
}

func ownerRulingFromPayload(payload ownerRulingPayload, runID, taskID, actor string, createdAt time.Time) OwnerRuling {
	return OwnerRuling{
		SchemaVersion: payload.SchemaVersion, RulingID: payload.RulingID, Body: payload.Body,
		Source: payload.Source, SupersedesID: payload.SupersedesID, NodeRunID: payload.NodeRunID,
		Decision: payload.Decision, WorkflowRunID: runID, TaskID: taskID, Actor: actor, CreatedAt: createdAt,
	}
}
