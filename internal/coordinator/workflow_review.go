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
	"sync"
)

// A review wait is a human_gate wait raised by an agent node itself, before it
// completes: the agent submits its artifact with `flow submit` and stays
// alive — session waiting, terminal attachable — while the human reviews.
// "changes_requested" (any outcome whose gate edge loops back to the
// submitting node) resumes the same session with the reviewer's feedback; any
// other outcome completes the agent node with the reviewed artifact and
// answers the downstream gate in one motion. The non-interactive path (flow
// complete, session finishes, gate wait opens on the gate node) is unchanged.

// ReviewWaitDetails rides on workflow_waits.details_json. Interactive waits
// carry everything the review UI needs because the current node is the agent
// node, not the gate: the gate's instructions and outcomes are copied here at
// submit time, so later flow edits cannot rewrite a review already in flight.
type ReviewWaitDetails struct {
	// Instructions is frozen with the wait so an in-flight review never changes
	// when the live flow definition is edited. It may be empty.
	Instructions string   `json:"instructions"`
	Outcomes     []string `json:"outcomes"`
	// ArtifactID is optional for an ordinary gate, but mandatory for an
	// interactive review because that review completes the submitting agent.
	ArtifactID  string `json:"artifact_id,omitempty"`
	Interactive bool   `json:"interactive"`
	GateNodeKey string `json:"gate_node_key"`
}

// ErrInvalidReviewWaitDetails identifies persisted human-gate data that is not
// in the current complete wait-details format. Stored workflow data is an
// epoch-bound contract; callers must surface this error rather than infer a
// review from a changed graph or a zero-value payload.
var (
	ErrInvalidReviewWaitDetails = errors.New("invalid review wait details")
	ErrReviewWaitIDRequired     = errors.New("review wait id is required")
)

// ParseReviewWaitDetails strictly decodes the immutable details of a human-gate
// wait. Every current record explicitly identifies whether it is interactive,
// its gate node, and every offered outcome. Instructions and an ordinary
// gate's artifact are deliberately optional; interactive reviews require their
// submitted artifact.
func ParseReviewWaitDetails(raw json.RawMessage) (ReviewWaitDetails, error) {
	var decoded struct {
		Instructions *string   `json:"instructions"`
		Outcomes     *[]string `json:"outcomes"`
		ArtifactID   *string   `json:"artifact_id"`
		Interactive  *bool     `json:"interactive"`
		GateNodeKey  *string   `json:"gate_node_key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return ReviewWaitDetails{}, fmt.Errorf("%w: decode JSON: %v", ErrInvalidReviewWaitDetails, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ReviewWaitDetails{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidReviewWaitDetails)
		}
		return ReviewWaitDetails{}, fmt.Errorf("%w: trailing JSON: %v", ErrInvalidReviewWaitDetails, err)
	}
	if decoded.Interactive == nil {
		return ReviewWaitDetails{}, fmt.Errorf("%w: interactive is required", ErrInvalidReviewWaitDetails)
	}
	if decoded.GateNodeKey == nil || strings.TrimSpace(*decoded.GateNodeKey) == "" {
		return ReviewWaitDetails{}, fmt.Errorf("%w: gate_node_key is required", ErrInvalidReviewWaitDetails)
	}
	if *decoded.GateNodeKey != strings.TrimSpace(*decoded.GateNodeKey) {
		return ReviewWaitDetails{}, fmt.Errorf("%w: gate_node_key must use canonical values", ErrInvalidReviewWaitDetails)
	}
	if decoded.Outcomes == nil || len(*decoded.Outcomes) == 0 {
		return ReviewWaitDetails{}, fmt.Errorf("%w: outcomes are required", ErrInvalidReviewWaitDetails)
	}

	outcomes := make([]string, len(*decoded.Outcomes))
	seen := make(map[string]struct{}, len(outcomes))
	for index, outcome := range *decoded.Outcomes {
		canonical := strings.TrimSpace(outcome)
		if canonical == "" {
			return ReviewWaitDetails{}, fmt.Errorf("%w: outcome %d is blank", ErrInvalidReviewWaitDetails, index+1)
		}
		if outcome != canonical {
			return ReviewWaitDetails{}, fmt.Errorf("%w: outcome %d must use canonical values", ErrInvalidReviewWaitDetails, index+1)
		}
		if _, duplicate := seen[outcome]; duplicate {
			return ReviewWaitDetails{}, fmt.Errorf("%w: outcome %q is repeated", ErrInvalidReviewWaitDetails, outcome)
		}
		seen[outcome] = struct{}{}
		outcomes[index] = outcome
	}

	details := ReviewWaitDetails{
		Outcomes:    outcomes,
		Interactive: *decoded.Interactive,
		GateNodeKey: *decoded.GateNodeKey,
	}
	if decoded.Instructions != nil {
		if *decoded.Instructions != strings.TrimSpace(*decoded.Instructions) {
			return ReviewWaitDetails{}, fmt.Errorf("%w: instructions must use canonical values", ErrInvalidReviewWaitDetails)
		}
		details.Instructions = *decoded.Instructions
	}
	if decoded.ArtifactID != nil {
		if *decoded.ArtifactID != strings.TrimSpace(*decoded.ArtifactID) {
			return ReviewWaitDetails{}, fmt.Errorf("%w: artifact_id must use canonical values", ErrInvalidReviewWaitDetails)
		}
		details.ArtifactID = *decoded.ArtifactID
	}
	if details.Interactive && details.ArtifactID == "" {
		return ReviewWaitDetails{}, fmt.Errorf("%w: interactive review requires artifact_id", ErrInvalidReviewWaitDetails)
	}
	return details, nil
}

// humanGateWaitDetails builds the complete frozen contract for a new human
// gate. It validates the exact representation that will be persisted, including
// the interactive artifact requirement, before any wait state is written.
func humanGateWaitDetails(node FlowNodeSnapshot, artifactID string, interactive bool) (ReviewWaitDetails, error) {
	if node.Config.HumanGate == nil {
		return ReviewWaitDetails{}, fmt.Errorf("human gate node %q has no configuration", node.Key)
	}
	details := ReviewWaitDetails{
		Instructions: strings.TrimSpace(node.Config.HumanGate.Instructions),
		Outcomes:     append([]string(nil), node.Config.HumanGate.Outcomes...),
		ArtifactID:   strings.TrimSpace(artifactID),
		Interactive:  interactive,
		GateNodeKey:  strings.TrimSpace(node.Key),
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return ReviewWaitDetails{}, fmt.Errorf("encode human gate details: %w", err)
	}
	validated, err := ParseReviewWaitDetails(encoded)
	if err != nil {
		return ReviewWaitDetails{}, fmt.Errorf("construct human gate details: %w", err)
	}
	return validated, nil
}

// Review wait states reported to polling clients (`flow submit`).
const (
	ReviewStateWaiting  = "waiting"
	ReviewStateResolved = "resolved"
	ReviewStateNone     = "none"
)

type SubmitForReviewInput struct {
	NodeRunID  string
	ArtifactID string
	// SessionID, when set, asserts the artifact belongs to this session. The
	// API layer sets it from the session credential; owner calls leave it
	// empty.
	SessionID string
	Actor     Actor
}

type ReviewStatusResult struct {
	State      string `json:"state"`
	Outcome    string `json:"outcome,omitempty"`
	Feedback   string `json:"feedback,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
}

type RespondReviewResult struct {
	Result     CompleteWorkflowNodeResult
	Outcome    string
	ArtifactID string
	// SessionID names the session that produced the reviewed artifact, for
	// callers that reconcile session lifecycle. It stays alive on
	// "changes_requested" (the agent revises), so it is only returned for
	// terminal outcomes. The agent finalizes the session itself through the
	// idempotent complete endpoint after the verdict reaches it.
	SessionID string
}

// SubmitForReview parks an active agent node on a human review of its
// artifact without completing the node: the run waits on the agent node run,
// the agent's job and session stay alive, and the wait carries the downstream
// gate's review contract. The agent polls ReviewStatus until the human
// responds through RespondReview.
func (s *WorkflowRunService) SubmitForReview(ctx context.Context, input SubmitForReviewInput) (WorkflowWait, error) {
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.ArtifactID = strings.TrimSpace(input.ArtifactID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	if input.NodeRunID == "" || input.ArtifactID == "" {
		return WorkflowWait{}, errors.New("node run id and artifact id are required")
	}
	if input.Actor == "" {
		input.Actor = ActorAgent
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowWait{}, err
	}
	defer tx.Rollback()

	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, input.NodeRunID))
	if err != nil {
		return WorkflowWait{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return WorkflowWait{}, err
	}
	sourceNode, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || sourceNode.Kind != NodeAgent {
		return WorkflowWait{}, fmt.Errorf("%w: only agent nodes can be submitted for review", ErrWorkflowConflict)
	}
	if run.State != WorkflowRunRunning || run.CurrentNodeRunID != nodeRun.ID || nodeRun.State != WorkflowNodeRunning {
		return WorkflowWait{}, fmt.Errorf("%w: node run is not active", ErrWorkflowConflict)
	}
	if _, waiting, err := openWaitTx(ctx, tx, run.ID); err != nil {
		return WorkflowWait{}, err
	} else if waiting {
		return WorkflowWait{}, fmt.Errorf("%w: workflow is already waiting", ErrWorkflowConflict)
	}
	if sourceNode.Config.Agent == nil {
		return WorkflowWait{}, errors.New("agent node has no configuration")
	}
	var artifactRunID, artifactNodeRunID, artifactKind, artifactSessionID string
	if err := tx.QueryRowContext(ctx, `
SELECT workflow_run_id, node_run_id, kind, COALESCE(session_id, '') FROM workflow_artifacts WHERE id = ?`, input.ArtifactID).
		Scan(&artifactRunID, &artifactNodeRunID, &artifactKind, &artifactSessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowWait{}, ErrWorkflowArtifactNotFound
		}
		return WorkflowWait{}, err
	}
	if artifactRunID != run.ID || artifactNodeRunID != nodeRun.ID || ArtifactKind(artifactKind) != sourceNode.Config.Agent.Artifact {
		return WorkflowWait{}, errors.New("artifact does not satisfy the active agent node contract")
	}
	if input.SessionID != "" && artifactSessionID != input.SessionID {
		return WorkflowWait{}, fmt.Errorf("%w: session credential does not own the workflow artifact", ErrWorkflowConflict)
	}
	gateKey, ok := run.Snapshot.Target(nodeRun.NodeKey, "completed")
	if !ok {
		return WorkflowWait{}, fmt.Errorf("%w: agent node has no completed edge to review against", ErrWorkflowConflict)
	}
	gateNode, ok := run.Snapshot.Node(gateKey)
	if !ok || gateNode.Kind != NodeHumanGate {
		return WorkflowWait{}, fmt.Errorf("%w: agent node is not followed by a human review gate; complete the node instead", ErrWorkflowConflict)
	}

	details, err := humanGateWaitDetails(gateNode, input.ArtifactID, true)
	if err != nil {
		return WorkflowWait{}, err
	}
	message := details.Instructions
	if message == "" {
		message = "Review the submitted work"
	}
	now := s.now().UTC()
	if err := enterWaitWithDetailsTx(ctx, tx, &run, &nodeRun, WorkflowWaitHumanGate, details, message, input.Actor, now); err != nil {
		return WorkflowWait{}, err
	}
	payload, err := json.Marshal(map[string]any{"artifact_id": input.ArtifactID, "gate_node_key": gateNode.Key})
	if err != nil {
		return WorkflowWait{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
		EventKind: "review_submitted", PayloadJSON: string(payload), Actor: string(input.Actor), CreatedAt: now,
	}); err != nil {
		return WorkflowWait{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowWait{}, err
	}
	wait, waiting, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
	if err != nil {
		return WorkflowWait{}, err
	}
	if !waiting {
		return WorkflowWait{}, errors.New("review wait was not recorded")
	}
	return wait, nil
}

// ReviewStatus reports where one node's review stands: still waiting on the
// human, resolved (with the verdict and feedback), or — when the wait ended
// without a review marker, for example an operator skip — none. `flow submit`
// polls it from inside the agent session.
func (s *WorkflowRunService) ReviewStatus(ctx context.Context, taskID, nodeRunID string) (ReviewStatusResult, error) {
	nodeRun, ok, err := s.GetNodeRun(ctx, strings.TrimSpace(nodeRunID))
	if err != nil {
		return ReviewStatusResult{}, err
	}
	if !ok {
		return ReviewStatusResult{}, ErrWorkflowRunNotFound
	}
	run, err := s.Get(ctx, nodeRun.WorkflowRunID)
	if err != nil {
		return ReviewStatusResult{}, err
	}
	if run.TaskID != strings.TrimSpace(taskID) {
		return ReviewStatusResult{}, ErrWorkflowRunNotFound
	}
	wait, waiting, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
	if err != nil {
		return ReviewStatusResult{}, err
	}
	if waiting && wait.Kind == WorkflowWaitHumanGate {
		details, err := ParseReviewWaitDetails(wait.Details)
		if err != nil {
			return ReviewStatusResult{}, fmt.Errorf("decode review wait %q: %w", wait.ID, err)
		}
		if wait.NodeRunID == nodeRun.ID && details.Interactive {
			return ReviewStatusResult{State: ReviewStateWaiting, ArtifactID: details.ArtifactID}, nil
		}
	}
	var payload string
	err = s.db.QueryRowContext(ctx, `
SELECT payload_json FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'review_responded' AND from_node_key = ?
ORDER BY created_at DESC, rowid DESC LIMIT 1`, run.ID, nodeRun.NodeKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewStatusResult{State: ReviewStateNone}, nil
	}
	if err != nil {
		return ReviewStatusResult{}, err
	}
	var marker struct {
		Outcome    string `json:"outcome"`
		Feedback   string `json:"feedback"`
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal([]byte(payload), &marker); err != nil {
		return ReviewStatusResult{}, fmt.Errorf("decode review marker: %w", err)
	}
	return ReviewStatusResult{
		State:      ReviewStateResolved,
		Outcome:    marker.Outcome,
		Feedback:   marker.Feedback,
		ArtifactID: marker.ArtifactID,
	}, nil
}

// reviewLockEntry is one task's per-task review lock. refs counts the
// acquirers that currently hold or are queued for the entry mutex, so the
// entry is only removed from reviewLocks once nobody references it: a queued
// acquirer increments refs before it blocks, so it can never wake onto an
// entry that was dropped and replaced by a second mutex.
type reviewLockEntry struct {
	mu   sync.Mutex
	refs int
}

// reviewLock returns a release func for the per-task human review lock. The
// whole RespondReview — the wait-id validation inside the transaction, the
// marker transaction, and the terminal CompleteNode calls — runs under it, so
// a concurrent decision cannot resolve the validated wait and reopen a fresh
// round on the same node run in the gap between the validation and the
// completion.
func (s *WorkflowRunService) reviewLock(taskID string) func() {
	s.reviewLocksMu.Lock()
	if s.reviewLocks == nil {
		s.reviewLocks = make(map[string]*reviewLockEntry)
	}
	entry, ok := s.reviewLocks[taskID]
	if !ok {
		entry = &reviewLockEntry{}
		s.reviewLocks[taskID] = entry
	}
	entry.refs++
	s.reviewLocksMu.Unlock()

	if s.reviewLockAcquireGate != nil {
		s.reviewLockAcquireGate()
	}
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		if s.reviewLockCleanupGate != nil {
			s.reviewLockCleanupGate()
		}
		// Drop the entry only when no acquirer — the releasing holder or any
		// queued waiter — still references it, so the map does not grow with
		// every task the service ever reviews. Counting references instead of
		// probing the mutex keeps the per-task serialization intact: TryLock
		// can win the lock before an already queued acquirer wakes, and
		// deleting then lets a later request build a second mutex while the
		// queued request still holds the first.
		s.reviewLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.reviewLocks, taskID)
		}
		s.reviewLocksMu.Unlock()
	}
}

// RespondReview applies the human's verdict to an interactive review wait.
// An outcome whose gate edge loops back to the submitting node (convention:
// "changes_requested") resolves the wait and hands the same node run — and
// the same live agent session — back to the agent with the feedback. Any
// other outcome completes the agent node with the reviewed artifact and
// answers the downstream gate, so the run flows on exactly as if the review
// had happened at the gate node itself.
//
// expectedWaitID is the immutable review round this response answers. It is
// required and re-asserted against the wait read inside the transaction: an
// interactive changes_requested round reopens a fresh wait on the SAME node
// run, so a response bound to an earlier round must not decide the later
// round's artifact.
//
// The whole decision runs under the per-task human review lock and one
// transaction, so the validated wait id stays authoritative through the
// terminal agent and derived-gate transitions: a concurrent decision cannot
// resolve the round and reopen a fresh wait in between.
func (s *WorkflowRunService) RespondReview(ctx context.Context, taskID, nodeRunID, expectedWaitID, outcome, feedback string, actor Actor) (RespondReviewResult, error) {
	taskID = strings.TrimSpace(taskID)
	nodeRunID = strings.TrimSpace(nodeRunID)
	expectedWaitID = strings.TrimSpace(expectedWaitID)
	outcome = strings.TrimSpace(outcome)
	feedback = strings.TrimSpace(feedback)
	if expectedWaitID == "" {
		return RespondReviewResult{}, ErrReviewWaitIDRequired
	}
	if outcome == "" {
		return RespondReviewResult{}, errors.New("review outcome is required")
	}
	if actor == "" {
		actor = ActorHuman
	}
	if s.reviewLockGate != nil {
		s.reviewLockGate()
	}
	unlock := s.reviewLock(taskID)
	defer unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RespondReviewResult{}, err
	}
	defer tx.Rollback()

	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, nodeRunID))
	if err != nil {
		return RespondReviewResult{}, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, nodeRun.WorkflowRunID))
	if err != nil {
		return RespondReviewResult{}, err
	}
	if run.TaskID != strings.TrimSpace(taskID) {
		return RespondReviewResult{}, ErrWorkflowRunNotFound
	}
	wait, waiting, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return RespondReviewResult{}, err
	}
	if !waiting || wait.Kind != WorkflowWaitHumanGate || wait.NodeRunID != nodeRun.ID {
		return RespondReviewResult{}, fmt.Errorf("%w: task is not waiting on that node", ErrWorkflowConflict)
	}
	// The response must answer the review round it was bound to, not just any
	// wait on the node run: a changes_requested round reopens a fresh wait on
	// the same node run, so a stale round-N response that raced a concurrent
	// decision would otherwise pass the node-run check and decide round N+1
	// with round N's verdict.
	if expectedWaitID != wait.ID {
		return RespondReviewResult{}, fmt.Errorf("%w: task is not waiting on that review round", ErrWorkflowConflict)
	}
	details, err := ParseReviewWaitDetails(wait.Details)
	if err != nil {
		return RespondReviewResult{}, fmt.Errorf("decode review wait %q: %w", wait.ID, err)
	}
	if !details.Interactive {
		return RespondReviewResult{}, fmt.Errorf("%w: wait is not an interactive review", ErrWorkflowConflict)
	}
	offered := false
	for _, candidate := range details.Outcomes {
		if candidate == outcome {
			offered = true
			break
		}
	}
	if !offered {
		return RespondReviewResult{}, fmt.Errorf("%w: outcome %q is not offered by this review gate", ErrWorkflowConflict, outcome)
	}
	sourceNode, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || sourceNode.Kind != NodeAgent {
		return RespondReviewResult{}, fmt.Errorf("%w: reviewed node is not an agent node", ErrWorkflowConflict)
	}
	gateNode, ok := run.Snapshot.Node(details.GateNodeKey)
	if !ok || gateNode.Kind != NodeHumanGate {
		return RespondReviewResult{}, fmt.Errorf("%w: review wait names an invalid human gate", ErrWorkflowConflict)
	}
	if target, ok := run.Snapshot.Target(nodeRun.NodeKey, "completed"); !ok || target != details.GateNodeKey {
		return RespondReviewResult{}, fmt.Errorf("%w: review wait does not belong to the reviewed node", ErrWorkflowConflict)
	}
	// A gate edge that loops back to the submitting node means "revise": the
	// agent is still alive, so the same visit continues instead of a new one.
	revise := false
	if target, ok := run.Snapshot.Target(details.GateNodeKey, outcome); ok && target == nodeRun.NodeKey {
		revise = true
	}

	now := s.now().UTC()
	markerKey := "review:" + wait.ID
	var recorded int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_transitions WHERE workflow_run_id = ? AND idempotency_key = ?`, run.ID, markerKey).Scan(&recorded); err != nil {
		return RespondReviewResult{}, err
	}
	if recorded == 0 {
		payload, err := json.Marshal(map[string]any{"outcome": outcome, "feedback": feedback, "artifact_id": details.ArtifactID})
		if err != nil {
			return RespondReviewResult{}, err
		}
		if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
			TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
			ToTaskState: string(LifecycleInProgress), FromNodeKey: nodeRun.NodeKey, ToNodeKey: nodeRun.NodeKey,
			Outcome: outcome, EventKind: "review_responded", PayloadJSON: string(payload),
			Actor: string(actor), IdempotencyKey: markerKey, CreatedAt: now,
		}); err != nil {
			return RespondReviewResult{}, err
		}
	}

	if revise {
		if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
			return RespondReviewResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = ? WHERE id = ?`, string(WorkflowNodeRunning), nodeRun.ID); err != nil {
			return RespondReviewResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = ?, version = version + 1 WHERE id = ?`, string(WorkflowRunRunning), run.ID); err != nil {
			return RespondReviewResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return RespondReviewResult{}, err
		}
		latest, err := s.Get(ctx, run.ID)
		if err != nil {
			return RespondReviewResult{}, err
		}
		return RespondReviewResult{
			Result:     CompleteWorkflowNodeResult{Run: latest, Done: latest.State == WorkflowRunCompleted},
			Outcome:    outcome,
			ArtifactID: details.ArtifactID,
		}, nil
	}
	// Terminal verdict: complete the agent and the derived ordinary gate in the
	// same transaction that validated the interactive wait. The derived wait is
	// a fresh persisted record, not an externally supplied response target, so
	// read and strictly validate its exact identity before consuming it. This
	// prevents a transient, unbound downstream gate from ever becoming visible
	// to another response path.
	if _, err := s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
		NodeRunID:       nodeRunID,
		Outcome:         "completed",
		ArtifactID:      details.ArtifactID,
		Actor:           actor,
		Payload:         map[string]any{"feedback": feedback, "review_outcome": outcome},
		IdempotencyKey:  "review-complete:" + wait.ID,
		humanGateWaitID: wait.ID,
	}, false, nil); err != nil {
		return RespondReviewResult{}, err
	}
	advanced, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ?`, run.ID))
	if err != nil {
		return RespondReviewResult{}, err
	}
	if advanced.CurrentNodeRunID == "" || advanced.CurrentNodeKey != details.GateNodeKey {
		return RespondReviewResult{}, fmt.Errorf("%w: review gate %q is not current after completing the agent node", ErrWorkflowConflict, details.GateNodeKey)
	}
	gateWait, waiting, err := openWaitTx(ctx, tx, advanced.ID)
	if err != nil {
		return RespondReviewResult{}, err
	}
	if !waiting || gateWait.Kind != WorkflowWaitHumanGate || gateWait.NodeRunID != advanced.CurrentNodeRunID || gateWait.ID == "" {
		return RespondReviewResult{}, fmt.Errorf("%w: derived review gate is not waiting on its persisted review round", ErrWorkflowConflict)
	}
	gateDetails, err := ParseReviewWaitDetails(gateWait.Details)
	if err != nil {
		return RespondReviewResult{}, fmt.Errorf("decode derived review wait %q: %w", gateWait.ID, err)
	}
	if gateDetails.Interactive || gateDetails.GateNodeKey != details.GateNodeKey {
		return RespondReviewResult{}, fmt.Errorf("%w: derived review wait does not belong to the ordinary review gate", ErrWorkflowConflict)
	}
	gateOffersOutcome := false
	for _, candidate := range gateDetails.Outcomes {
		if candidate == outcome {
			gateOffersOutcome = true
			break
		}
	}
	if !gateOffersOutcome {
		return RespondReviewResult{}, fmt.Errorf("%w: outcome %q is not offered by the derived review gate", ErrWorkflowConflict, outcome)
	}
	if s.reviewTerminalGate != nil {
		s.reviewTerminalGate()
	}
	second, err := s.completeNodeTx(ctx, tx, CompleteWorkflowNodeInput{
		NodeRunID:       gateWait.NodeRunID,
		Outcome:         outcome,
		Actor:           actor,
		Payload:         map[string]any{"feedback": feedback},
		IdempotencyKey:  "human:" + gateWait.ID + ":" + outcome,
		humanGateWaitID: gateWait.ID,
	}, false, nil)
	if err != nil {
		return RespondReviewResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RespondReviewResult{}, err
	}
	latest, err := s.Get(ctx, run.ID)
	if err != nil {
		return RespondReviewResult{}, err
	}
	second.Run = latest
	second.Done = latest.State == WorkflowRunCompleted
	var sessionID string
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(session_id, '') FROM workflow_artifacts WHERE id = ?`, details.ArtifactID).Scan(&sessionID); err != nil {
		return RespondReviewResult{}, err
	}
	return RespondReviewResult{
		Result:     second,
		Outcome:    outcome,
		ArtifactID: details.ArtifactID,
		SessionID:  sessionID,
	}, nil
}
