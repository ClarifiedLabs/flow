package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	Instructions string   `json:"instructions,omitempty"`
	Outcomes     []string `json:"outcomes,omitempty"`
	ArtifactID   string   `json:"artifact_id,omitempty"`
	Interactive  bool     `json:"interactive,omitempty"`
	GateNodeKey  string   `json:"gate_node_key,omitempty"`
}

// ParseReviewWaitDetails decodes wait details, tolerating empty or legacy
// payloads (which unmarshal to the zero value).
func ParseReviewWaitDetails(raw json.RawMessage) ReviewWaitDetails {
	var details ReviewWaitDetails
	if len(raw) == 0 {
		return details
	}
	_ = json.Unmarshal(raw, &details)
	return details
}

func humanGateWaitDetails(node FlowNodeSnapshot, artifactID string) ReviewWaitDetails {
	details := ReviewWaitDetails{ArtifactID: strings.TrimSpace(artifactID)}
	if node.Config.HumanGate != nil {
		details.Instructions = strings.TrimSpace(node.Config.HumanGate.Instructions)
		details.Outcomes = append([]string(nil), node.Config.HumanGate.Outcomes...)
	}
	return details
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

	details := humanGateWaitDetails(gateNode, input.ArtifactID)
	details.Interactive = true
	details.GateNodeKey = gateNode.Key
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
	if waiting && wait.NodeRunID == nodeRun.ID {
		return ReviewStatusResult{State: ReviewStateWaiting, ArtifactID: ParseReviewWaitDetails(wait.Details).ArtifactID}, nil
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

// RespondReview applies the human's verdict to an interactive review wait.
// An outcome whose gate edge loops back to the submitting node (convention:
// "changes_requested") resolves the wait and hands the same node run — and
// the same live agent session — back to the agent with the feedback. Any
// other outcome completes the agent node with the reviewed artifact and
// answers the downstream gate, so the run flows on exactly as if the review
// had happened at the gate node itself.
func (s *WorkflowRunService) RespondReview(ctx context.Context, taskID, nodeRunID, outcome, feedback string, actor Actor) (RespondReviewResult, error) {
	nodeRunID = strings.TrimSpace(nodeRunID)
	outcome = strings.TrimSpace(outcome)
	feedback = strings.TrimSpace(feedback)
	if outcome == "" {
		return RespondReviewResult{}, errors.New("review outcome is required")
	}
	if actor == "" {
		actor = ActorHuman
	}
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
	details := ParseReviewWaitDetails(wait.Details)
	if !details.Interactive {
		return RespondReviewResult{}, fmt.Errorf("%w: wait is not an interactive review", ErrWorkflowConflict)
	}
	if strings.TrimSpace(details.ArtifactID) == "" {
		return RespondReviewResult{}, fmt.Errorf("%w: review wait has no artifact to complete with", ErrWorkflowConflict)
	}
	if len(details.Outcomes) > 0 {
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
	}
	sourceNode, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || sourceNode.Kind != NodeAgent {
		return RespondReviewResult{}, fmt.Errorf("%w: reviewed node is not an agent node", ErrWorkflowConflict)
	}
	// A gate edge that loops back to the submitting node means "revise": the
	// agent is still alive, so the same visit continues instead of a new one.
	revise := false
	if details.GateNodeKey != "" {
		if target, ok := run.Snapshot.Target(details.GateNodeKey, outcome); ok && target == nodeRun.NodeKey {
			revise = true
		}
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
	if err := tx.Commit(); err != nil {
		return RespondReviewResult{}, err
	}

	// Terminal verdict: complete the agent node with the reviewed artifact,
	// then answer the gate it advanced into. Both go through CompleteNode so
	// the transition history matches a review held at the gate node itself.
	first, err := s.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: nodeRunID, Outcome: "completed", ArtifactID: details.ArtifactID, Actor: actor,
		Payload:        map[string]any{"feedback": feedback, "review_outcome": outcome},
		IdempotencyKey: "review-complete:" + wait.ID,
	})
	if err != nil {
		return RespondReviewResult{}, err
	}
	latest, err := s.Get(ctx, first.Run.ID)
	if err != nil {
		return RespondReviewResult{}, err
	}
	if latest.CurrentNodeRunID == "" || latest.CurrentNodeKey != details.GateNodeKey {
		return RespondReviewResult{}, fmt.Errorf("%w: review gate %q is not current after completing the agent node", ErrWorkflowConflict, details.GateNodeKey)
	}
	second, err := s.CompleteNode(ctx, CompleteWorkflowNodeInput{
		NodeRunID: latest.CurrentNodeRunID, Outcome: outcome, Actor: actor,
		Payload:        map[string]any{"feedback": feedback},
		IdempotencyKey: "review-gate:" + wait.ID,
	})
	if err != nil {
		return RespondReviewResult{}, err
	}
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
