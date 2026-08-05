package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// A hold is an operator taking the run: the executor stops advancing it to the
// next node, but in-flight worker jobs keep running so an operator can attach a
// terminal and work in the same tree the agent was using.
//
// Holds are deliberately not modelled as a workflow_waits row. The waits table
// carries a single-open-row constraint (idx_workflow_waits_one_open), so a hold
// could not coexist with the human gate it is most useful to pause at, and
// resolveOpenWaitTx — which every node completion calls — would clear it as a
// side effect.

// ReleaseEdge names how a held run is handed back to the executor.
type ReleaseEdge string

const (
	// ReleaseResume drops the hold and leaves the run exactly where it was.
	ReleaseResume ReleaseEdge = "resume"
	// ReleaseSubmit completes the current node with an operator-supplied
	// artifact and takes the node's success edge.
	ReleaseSubmit ReleaseEdge = "submit"
	// ReleaseSatisfy completes the current node with no new artifact, taking
	// the success edge on the strength of the operator's say-so.
	ReleaseSatisfy ReleaseEdge = "satisfy"
	// ReleaseMerge abandons the remaining nodes and jumps to the terminal.
	ReleaseMerge ReleaseEdge = "merge"
)

// ErrWorkflowNotHeld reports a release against a run no operator holds.
var ErrWorkflowNotHeld = errors.New("workflow run is not held")

// ConvergenceFile describes changed-path evidence attached to a scope hold.
type ConvergenceFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

const (
	convergenceMessageFileLimit = 20
	convergencePayloadFileLimit = 100
	convergencePayloadByteLimit = 64 * 1024
	convergenceDisplayPathLimit = 320
)

// HoldForConvergence pauses an oversized change once per workflow run before
// automated reviewers are dispatched. The typed evidence captures immutable
// Git identities; the durable transition marker prevents the same run from
// being stopped again on every review visit.
func (s *WorkflowRunService) HoldForConvergence(
	ctx context.Context,
	evidence ConvergenceEvidence,
) (WorkflowRun, bool, error) {
	return s.requestConvergenceReview(ctx, evidence, ActorSystem, true)
}

// RequestConvergenceReview creates the same typed hold at an owner's request,
// without requiring the automatic size threshold to have been exceeded. Unlike
// the automatic guard, a later manual request is allowed after an earlier
// convergence review was resolved.
func (s *WorkflowRunService) RequestConvergenceReview(
	ctx context.Context,
	evidence ConvergenceEvidence,
	actor Actor,
) (WorkflowRun, bool, error) {
	if actor == "" {
		actor = ActorHuman
	}
	return s.requestConvergenceReview(ctx, evidence, actor, false)
}

func (s *WorkflowRunService) requestConvergenceReview(
	ctx context.Context,
	evidence ConvergenceEvidence,
	actor Actor,
	automatic bool,
) (WorkflowRun, bool, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, strings.TrimSpace(evidence.TaskID)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, false, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, false, err
	}
	var prior int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'workflow_convergence_review_requested'`,
		run.ID,
	).Scan(&prior); err != nil {
		return WorkflowRun{}, false, err
	}
	if automatic && (prior > 0 || run.Held()) {
		return run, false, nil
	}

	now := s.now().UTC()
	evidence, err = normalizeConvergenceEvidence(run, evidence, now)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	if run.CurrentNodeRunID != evidence.NodeRunID {
		return WorkflowRun{}, false, fmt.Errorf("%w: convergence evidence node run is not active", ErrWorkflowConflict)
	}
	if err := validateConvergenceProjectionTx(ctx, tx, run, evidence); err != nil {
		return WorkflowRun{}, false, err
	}
	if !automatic {
		active, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
		if err != nil {
			return WorkflowRun{}, false, err
		}
		if active != nil {
			if active.Fingerprint == evidence.Fingerprint {
				return run, false, nil
			}
			return WorkflowRun{}, false, fmt.Errorf("%w: workflow already has a different active convergence review", ErrWorkflowConflict)
		}
	}
	// A typed convergence hold remains system-enforced even when a human
	// requested it. The transition and status actor retain who initiated it.
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs
SET held_at = COALESCE(held_at, ?), held_by = ?, version = version + 1
WHERE id = ?`,
		sqlitex.FormatTime(now), string(ActorSystem), run.ID,
	); err != nil {
		return WorkflowRun{}, false, err
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, EventKind: "workflow_convergence_review_requested",
		PayloadJSON: string(payload), Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, false, err
	}
	message := convergenceHoldMessage(
		evidence.Files, evidence.Additions, evidence.Deletions,
		evidence.MaxFiles, evidence.MaxLines, evidence.ChangedFiles, automatic,
	)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO status_log (task_id, actor, message, kind, created_at)
VALUES (?, ?, ?, ?, ?)`,
		run.TaskID,
		string(actor),
		message,
		StatusKindPlan,
		sqlitex.FormatTime(now),
	); err != nil {
		return WorkflowRun{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRun{}, false, err
	}
	held, err := s.Get(ctx, run.ID)
	return held, true, err
}

func convergenceHoldMessage(files int, additions int, deletions int, maxFiles int, maxLines int, changedFiles []ConvergenceFile, automatic bool) string {
	var message strings.Builder
	if automatic {
		fmt.Fprintf(
			&message,
			"Convergence review required before automated review: this change touches %d files and %d lines (+%d/-%d; automatic threshold: %d files or %d lines).",
			files,
			additions+deletions,
			additions,
			deletions,
			maxFiles,
			maxLines,
		)
	} else {
		fmt.Fprintf(
			&message,
			"Convergence review requested manually: this change touches %d files and %d lines (+%d/-%d; automatic threshold: %d files or %d lines).",
			files,
			additions+deletions,
			additions,
			deletions,
			maxFiles,
			maxLines,
		)
	}

	if len(changedFiles) > 0 {
		reported := min(len(changedFiles), convergenceMessageFileLimit)
		message.WriteString("\n\n### Largest changed files\n")
		for _, file := range changedFiles[:reported] {
			path, truncated := convergenceDisplayPath(file.Path)
			suffix := ""
			if truncated {
				suffix = " (path truncated)"
			}
			if file.Binary {
				fmt.Fprintf(&message, "- `%s` — binary%s\n", path, suffix)
				continue
			}
			fmt.Fprintf(&message, "- `%s` — +%d/-%d%s\n", path, file.Additions, file.Deletions, suffix)
		}
		if omitted := files - reported; omitted > 0 {
			fmt.Fprintf(&message, "- _%d more changed files omitted._\n", omitted)
		}
	}

	message.WriteString("\nBefore resuming:\n")
	message.WriteString("1. Compare the changed paths with the task's declared scope.\n")
	message.WriteString("2. Split independent work into follow-up tasks; keep task-caused correctness requirements here.\n")
	message.WriteString("3. Repair the branch first if the diff contains unrelated reversions, merge fallout, or base updates.\n")
	message.WriteString("4. Resume only when the remaining change is one coherent review unit.")
	return message.String()
}

func sortConvergenceFiles(changedFiles []ConvergenceFile) []ConvergenceFile {
	filesByChurn := append([]ConvergenceFile(nil), changedFiles...)
	sort.Slice(filesByChurn, func(i, j int) bool {
		iLines := filesByChurn[i].Additions + filesByChurn[i].Deletions
		jLines := filesByChurn[j].Additions + filesByChurn[j].Deletions
		if iLines == jLines {
			return filesByChurn[i].Path < filesByChurn[j].Path
		}
		return iLines > jLines
	})
	return filesByChurn
}

func convergencePayloadFiles(totalFiles int, changedFiles []ConvergenceFile) ([]ConvergenceFile, int) {
	payloadBytes := 2 // JSON array brackets.
	payloadFiles := make([]ConvergenceFile, 0, min(len(changedFiles), convergencePayloadFileLimit))
	for _, file := range changedFiles {
		encoded, err := json.Marshal(file)
		separatorBytes := 0
		if len(payloadFiles) > 0 {
			separatorBytes = 1
		}
		if err != nil || len(payloadFiles) >= convergencePayloadFileLimit || payloadBytes+separatorBytes+len(encoded) > convergencePayloadByteLimit {
			continue
		}
		payloadFiles = append(payloadFiles, file)
		payloadBytes += separatorBytes + len(encoded)
	}
	return payloadFiles, max(totalFiles, len(changedFiles)) - len(payloadFiles)
}

func convergenceDisplayPath(path string) (string, bool) {
	quoted := strconv.QuoteToASCII(path)
	path = strings.ReplaceAll(quoted[1:len(quoted)-1], "`", `\x60`)
	if len(path) <= convergenceDisplayPathLimit {
		return path, false
	}
	return path[:convergenceDisplayPathLimit] + "…", true
}

// ReleaseWorkflowInput carries a hand-back. ArtifactID is required by
// ReleaseSubmit and ignored otherwise.
type ReleaseWorkflowInput struct {
	TaskID     string
	Edge       ReleaseEdge
	ArtifactID string
	Actor      Actor
}

// Hold stops the executor from advancing the task's active run. Holding an
// already-held run is a no-op so a double click cannot stack transitions.
func (s *WorkflowRunService) Hold(ctx context.Context, taskID string, actor Actor) (WorkflowRun, error) {
	if actor == "" {
		actor = ActorHuman
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, strings.TrimSpace(taskID)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, err
	}
	if run.Held() {
		return run, nil
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = ?, held_by = ?, version = version + 1 WHERE id = ?`,
		sqlitex.FormatTime(now), string(actor), run.ID); err != nil {
		return WorkflowRun{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, EventKind: "workflow_hold_requested",
		Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return s.Get(ctx, run.ID)
}

// Release hands a held run back to the executor along the named edge.
func (s *WorkflowRunService) Release(ctx context.Context, input ReleaseWorkflowInput) (CompleteWorkflowNodeResult, error) {
	if input.Actor == "" {
		input.Actor = ActorHuman
	}
	taskID := strings.TrimSpace(input.TaskID)
	switch input.Edge {
	case ReleaseResume, ReleaseSubmit, ReleaseSatisfy, ReleaseMerge:
	default:
		return CompleteWorkflowNodeResult{}, fmt.Errorf("unknown release edge %q", input.Edge)
	}
	switch input.Edge {
	case ReleaseResume:
		run, _, err := s.clearHold(ctx, taskID, input.Actor, input.Edge)
		if err != nil {
			return CompleteWorkflowNodeResult{}, err
		}
		return CompleteWorkflowNodeResult{Run: run}, nil
	case ReleaseMerge:
		return s.releaseMerge(ctx, taskID, input.Actor)
	case ReleaseSubmit, ReleaseSatisfy:
		return s.releaseComplete(ctx, input)
	default:
		panic("validated release edge reached unexpected dispatch")
	}
}

// clearHold drops a resume hold in its own transaction. Edges that also change
// a node use releaseComplete or releaseMerge so the hold release cannot commit
// separately from the node transition.
func (s *WorkflowRunService) clearHold(ctx context.Context, taskID string, actor Actor, edge ReleaseEdge) (WorkflowRun, *WorkflowNodeRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	defer tx.Rollback()
	run, nodeRun, err := s.releaseHoldTx(ctx, tx, taskID, actor, edge)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, nil, err
	}
	return run, nodeRun, nil
}

// releaseHoldTx validates and records a hold hand-back without committing it.
// Callers that take an edge must retain this transaction through that edge so a
// human wait cannot appear after the hold release and before node completion.
func (s *WorkflowRunService) releaseHoldTx(ctx context.Context, tx workflowTx, taskID string, actor Actor, edge ReleaseEdge) (WorkflowRun, *WorkflowNodeRun, error) {
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, nil, ErrWorkflowRunNotFound
		}
		return WorkflowRun{}, nil, err
	}
	if !run.Held() {
		return WorkflowRun{}, nil, ErrWorkflowNotHeld
	}
	convergenceEvidence, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	if convergenceEvidence != nil {
		return WorkflowRun{}, nil, fmt.Errorf("%w: convergence hold requires an explicit disposition", ErrWorkflowConflict)
	}
	// Releasing a hold may resume a human gate, but it may not choose that
	// gate's outcome. A release request has no review-round identity; allowing
	// submit, satisfy, or merge here would let a stale operator action resolve
	// whichever human gate happens to be open. Resume leaves the persisted wait
	// intact so the caller must use Respond with its node-run and review-wait IDs.
	if edge != ReleaseResume {
		wait, waiting, err := openWaitTx(ctx, tx, run.ID)
		if err != nil {
			return WorkflowRun{}, nil, err
		}
		if !waiting && run.State == WorkflowRunWaiting {
			return WorkflowRun{}, nil, fmt.Errorf("%w: release edge %q cannot resolve a workflow waiting without its persisted wait", ErrWorkflowConflict, edge)
		}
		currentNode, currentNodeKnown := run.Snapshot.Node(run.CurrentNodeKey)
		if (waiting && wait.Kind == WorkflowWaitHumanGate) || (currentNodeKnown && currentNode.Kind == NodeHumanGate) {
			return WorkflowRun{}, nil, fmt.Errorf("%w: release edge %q cannot resolve a human gate; resume it and respond with node_run_id and review_wait_id", ErrWorkflowConflict, edge)
		}
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`, run.ID); err != nil {
		return WorkflowRun{}, nil, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, Outcome: string(edge),
		EventKind: "workflow_hold_released", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return WorkflowRun{}, nil, err
	}

	var nodeRun *WorkflowNodeRun
	if strings.TrimSpace(run.CurrentNodeRunID) != "" {
		active, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, nil, err
		}
		if err == nil {
			nodeRun = &active
		}
	}
	run.HeldAt = nil
	run.HeldBy = ""
	return run, nodeRun, nil
}

// releaseComplete hands a held non-human node back along submit or satisfy in
// one immediate transaction. The human-wait check, hold-release audit row, and
// node transition therefore either all commit together or all roll back.
func (s *WorkflowRunService) releaseComplete(ctx context.Context, input ReleaseWorkflowInput) (CompleteWorkflowNodeResult, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()

	run, nodeRun, err := s.releaseHoldTx(ctx, tx, strings.TrimSpace(input.TaskID), input.Actor, input.Edge)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if nodeRun == nil {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: run has no active node to hand back", ErrWorkflowConflict)
	}
	// Validate the held state before checking submit's payload. A generic release
	// of a human gate must consistently report the routing conflict (and never
	// expose whether an unrelated artifact was supplied).
	if input.Edge == ReleaseSubmit && strings.TrimSpace(input.ArtifactID) == "" {
		return CompleteWorkflowNodeResult{}, errors.New("submit requires an artifact")
	}
	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("snapshot node %q not found", nodeRun.NodeKey)
	}
	outcome, ok := workflowSuccessOutcome(node)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: node kind %q has no success edge to take", ErrWorkflowConflict, node.Kind)
	}
	completion := CompleteWorkflowNodeInput{
		NodeRunID: nodeRun.ID,
		Outcome:   outcome,
		Actor:     input.Actor,
		Payload:   map[string]any{"released_edge": string(input.Edge)},
	}
	switch input.Edge {
	case ReleaseSubmit:
		completion.ArtifactID = strings.TrimSpace(input.ArtifactID)
	case ReleaseSatisfy:
		// No artifact: the operator asserts the node is done, so waive the
		// agent-node artifact contract and carry the run's current artifact.
		completion.OperatorSatisfied = true
	default:
		return CompleteWorkflowNodeResult{}, fmt.Errorf("unknown release edge %q", input.Edge)
	}
	result, err := s.completeNodeTx(ctx, tx, completion, false, nil)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	latest, err := s.Get(ctx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	result.Run = latest
	result.Done = latest.State == WorkflowRunCompleted
	return result, nil
}

// releaseMerge clears a held run and jumps to its terminal node in one
// BEGIN IMMEDIATE transaction. In particular, a concurrently submitted
// interactive review either commits before this transaction (and preserves the
// hold by making the merge conflict) or loses to the completed terminal jump;
// there is no committed hold-release state between those outcomes.
func (s *WorkflowRunService) releaseMerge(ctx context.Context, taskID string, actor Actor) (CompleteWorkflowNodeResult, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	defer tx.Rollback()

	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, taskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CompleteWorkflowNodeResult{}, ErrWorkflowRunNotFound
		}
		return CompleteWorkflowNodeResult{}, err
	}
	if !run.Held() {
		return CompleteWorkflowNodeResult{}, ErrWorkflowNotHeld
	}
	convergenceEvidence, err := activeConvergenceEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if convergenceEvidence != nil {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: convergence hold requires an explicit disposition", ErrWorkflowConflict)
	}
	wait, waiting, err := openWaitTx(ctx, tx, run.ID)
	if err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	currentNode, currentNodeKnown := run.Snapshot.Node(run.CurrentNodeKey)
	if (waiting && wait.Kind == WorkflowWaitHumanGate) ||
		(!waiting && run.State == WorkflowRunWaiting) ||
		(currentNodeKnown && currentNode.Kind == NodeHumanGate) {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: release merge cannot resolve a human gate or a workflow waiting without its persisted wait; resume it and respond with node_run_id and review_wait_id", ErrWorkflowConflict)
	}
	terminalNode, ok := run.Snapshot.TerminalForResolution(ResolutionMerged)
	if !ok {
		return CompleteWorkflowNodeResult{}, fmt.Errorf("%w: flow has no merged terminal node", ErrWorkflowConflict)
	}

	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_runs SET held_at = NULL, held_by = '', version = version + 1 WHERE id = ?`, run.ID); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey,
		ToNodeKey: run.CurrentNodeKey, Outcome: string(ReleaseMerge),
		EventKind: "workflow_hold_released", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	run.HeldAt = nil
	run.HeldBy = ""
	if err := s.jumpToTerminalTx(ctx, tx, &run, terminalNode, actor, now); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteWorkflowNodeResult{}, err
	}
	completed, err := s.Get(ctx, run.ID)
	return CompleteWorkflowNodeResult{Run: completed, Done: true}, err
}

// jumpToTerminalTx abandons the remaining nodes and closes the run at terminal
// while its caller owns the hold-release transaction.
func (s *WorkflowRunService) jumpToTerminalTx(ctx context.Context, tx workflowTx, run *WorkflowRun, terminalNode FlowNodeSnapshot, actor Actor, now time.Time) error {
	fromNodeKey := run.CurrentNodeKey

	// Retire whatever the run was in the middle of before closing it out.
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, completed_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'running', 'waiting')`,
		string(WorkflowNodeCancelled), sqlitex.FormatTime(now), run.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND state IN ('queued', 'claimed', 'running')`,
		sqlitex.FormatTime(now), run.ID); err != nil {
		return err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return err
	}

	terminalRun, err := createNodeRunTx(ctx, tx, *run, terminalNode.Key, 1, run.CurrentArtifactID, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE workflow_node_runs SET state = ?, started_at = ?, completed_at = ? WHERE id = ?`,
		string(WorkflowNodeSucceeded), sqlitex.FormatTime(now), sqlitex.FormatTime(now), terminalRun.ID); err != nil {
		return err
	}
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromTaskState: string(LifecycleInProgress),
		ToTaskState: string(LifecycleInProgress), FromNodeKey: fromNodeKey, ToNodeKey: terminalNode.Key,
		Outcome: string(ReleaseMerge), EventKind: "node_jumped", Actor: string(actor), CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := s.completeTerminalTx(ctx, tx, run, terminalNode, "operator", now); err != nil {
		return err
	}
	return nil
}

// workflowSuccessOutcome names the edge a non-human node takes when an
// operator hands it back as satisfied.
func workflowSuccessOutcome(node FlowNodeSnapshot) (string, bool) {
	switch node.Kind {
	case NodeAgent, NodeMaterializeTaskSet:
		return "completed", true
	case NodeAutomatedChecks, NodeVerifyChange:
		return "passed", true
	case NodeChangeReview:
		return "approved", true
	case NodeMergeChange:
		return "merged", true
	case NodeFinalizeRebase:
		return "finalized", true
	}
	return "", false
}

// WorkflowCardState is the per-task workflow summary the board renders: how
// long the task has sat where it is, how far through the flow it is, what it is
// waiting on, and whether an operator holds it.
type WorkflowCardState struct {
	RunID      string        `json:"run_id,omitempty"`
	StepIndex  int           `json:"step_index"`
	StepCount  int           `json:"step_count"`
	StepKey    string        `json:"step_key,omitempty"`
	DwellSince time.Time     `json:"dwell_since"`
	Wait       *WorkflowWait `json:"wait,omitempty"`
	Held       bool          `json:"held,omitempty"`
	HeldBy     string        `json:"held_by,omitempty"`
}

// CardState summarises the task's active run for the board. It reports false
// when the task has no active run (unscheduled, or already done).
func (s *WorkflowRunService) CardState(ctx context.Context, taskID string) (WorkflowCardState, bool, error) {
	run, ok, err := s.ActiveForTask(ctx, strings.TrimSpace(taskID))
	if err != nil || !ok {
		return WorkflowCardState{}, false, err
	}

	state := WorkflowCardState{
		RunID:     run.ID,
		StepCount: len(run.Snapshot.Nodes),
		StepKey:   run.CurrentNodeKey,
		Held:      run.Held(),
		HeldBy:    run.HeldBy,
	}
	if index, ok := run.Snapshot.NodeIndex(run.CurrentNodeKey); ok {
		state.StepIndex = index + 1
	}

	wait, waiting, err := scanWorkflowWaitMaybe(s.db.QueryRowContext(ctx, workflowWaitSelect+`
WHERE workflow_run_id = ? AND state = 'open'`, run.ID))
	if err != nil {
		return WorkflowCardState{}, false, err
	}
	if waiting {
		state.Wait = &wait
	}

	state.DwellSince, err = s.dwellSince(ctx, run, state.Wait)
	if err != nil {
		return WorkflowCardState{}, false, err
	}
	return state, true, nil
}

// dwellSince answers "how long has this task been where it is". An open wait is
// the most specific answer; otherwise it is when the run entered its current
// node, falling back through the node run's own clock to the run's.
func (s *WorkflowRunService) dwellSince(ctx context.Context, run WorkflowRun, wait *WorkflowWait) (time.Time, error) {
	if wait != nil {
		return wait.CreatedAt, nil
	}
	if run.Held() {
		return *run.HeldAt, nil
	}

	var enteredAt sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT created_at FROM workflow_transitions
WHERE workflow_run_id = ? AND to_node_key = ?
ORDER BY seq DESC LIMIT 1`, run.ID, run.CurrentNodeKey).Scan(&enteredAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	if enteredAt.Valid {
		return sqlitex.ParseTime(enteredAt.String)
	}

	if strings.TrimSpace(run.CurrentNodeRunID) != "" {
		nodeRun, err := scanWorkflowNodeRun(s.db.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, err
		}
		if err == nil {
			if nodeRun.StartedAt != nil {
				return *nodeRun.StartedAt, nil
			}
			return nodeRun.CreatedAt, nil
		}
	}
	if run.StartedAt != nil {
		return *run.StartedAt, nil
	}
	return run.CreatedAt, nil
}
