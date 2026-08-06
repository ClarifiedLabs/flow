package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const (
	ReviewScopeDecisionWaitSchemaVersion = 1
	MaxReviewScopeDecisionRounds         = 3
)

type ReviewScopeDecisionChoice string

const (
	ReviewScopeFixInTask     ReviewScopeDecisionChoice = "fix_in_task"
	ReviewScopeOutOfScope    ReviewScopeDecisionChoice = "out_of_scope"
	ReviewScopeDeferFollowUp ReviewScopeDecisionChoice = "defer_follow_up"
)

var ReviewScopeDecisionChoices = []ReviewScopeDecisionChoice{
	ReviewScopeFixInTask, ReviewScopeOutOfScope, ReviewScopeDeferFollowUp,
}

type ReviewScopeSourceCheck struct {
	Name          string `json:"name"`
	DetailsSHA256 string `json:"details_sha256"`
}

type ReviewScopeDecisionWaitDetails struct {
	SchemaVersion        int                         `json:"schema_version"`
	AggregationCheckID   int64                       `json:"aggregation_check_id"`
	AggregationCheckName string                      `json:"aggregation_check_name"`
	SourceJobID          string                      `json:"source_job_id"`
	NodeRunID            string                      `json:"node_run_id"`
	NodeAttempt          int                         `json:"node_attempt"`
	SourceHeadSHA        string                      `json:"source_head_sha"`
	Report               checkverdict.VerdictReport  `json:"report"`
	ReportSHA256         string                      `json:"report_sha256"`
	DecisionKey          string                      `json:"decision_key"`
	Question             string                      `json:"question"`
	Rationale            string                      `json:"rationale"`
	CommentIndexes       []int                       `json:"comment_indexes"`
	AllowedChoices       []ReviewScopeDecisionChoice `json:"allowed_choices"`
	SourceChecks         []ReviewScopeSourceCheck    `json:"source_checks"`
}

type RequestReviewScopeDecisionInput struct {
	TaskID      string
	CheckName   string
	LeaseID     string
	SourceJobID string
	WorkerID    string
	Report      checkverdict.VerdictReport
}

type RequestReviewScopeDecisionResult struct {
	Wait     WorkflowWait `json:"wait"`
	Replayed bool         `json:"replayed,omitempty"`
}

type ResolveReviewScopeDecisionInput struct {
	TaskID         string
	WaitID         string
	Choice         ReviewScopeDecisionChoice
	Guidance       string
	Actor          Actor
	IdempotencyKey string
}

type ResolveReviewScopeDecisionResult struct {
	Result   string               `json:"result"`
	Ruling   *OwnerRuling         `json:"ruling,omitempty"`
	Wait     WorkflowWait         `json:"wait"`
	Run      WorkflowRun          `json:"run"`
	Delivery *OwnerRulingDelivery `json:"delivery,omitempty"`
}

type reviewScopeDecisionRequestedPayload struct {
	WaitID       string `json:"wait_id"`
	DecisionKey  string `json:"decision_key"`
	SourceJobID  string `json:"source_job_id"`
	NodeRunID    string `json:"node_run_id"`
	ReportSHA256 string `json:"report_sha256"`
}

type reviewScopeDecisionResolvedPayload struct {
	WaitID      string                    `json:"wait_id"`
	DecisionKey string                    `json:"decision_key"`
	Choice      ReviewScopeDecisionChoice `json:"choice"`
	Guidance    string                    `json:"guidance,omitempty"`
	RulingID    string                    `json:"ruling_id"`
}

func ParseReviewScopeDecisionWaitDetails(data []byte) (ReviewScopeDecisionWaitDetails, error) {
	var details ReviewScopeDecisionWaitDetails
	if err := json.Unmarshal(data, &details); err != nil {
		return details, err
	}
	if details.SchemaVersion != ReviewScopeDecisionWaitSchemaVersion {
		return details, fmt.Errorf("unsupported schema version %d", details.SchemaVersion)
	}
	if details.AggregationCheckID < 1 || strings.TrimSpace(details.AggregationCheckName) == "" ||
		strings.TrimSpace(details.SourceJobID) == "" || strings.TrimSpace(details.NodeRunID) == "" ||
		details.NodeAttempt < 1 || strings.TrimSpace(details.SourceHeadSHA) == "" ||
		strings.TrimSpace(details.ReportSHA256) == "" || strings.TrimSpace(details.DecisionKey) == "" {
		return details, errors.New("review-scope decision details are incomplete")
	}
	if len(details.AllowedChoices) != len(ReviewScopeDecisionChoices) {
		return details, errors.New("review-scope decision choices are invalid")
	}
	for index := range ReviewScopeDecisionChoices {
		if details.AllowedChoices[index] != ReviewScopeDecisionChoices[index] {
			return details, errors.New("review-scope decision choices are invalid")
		}
	}
	canonical, err := json.Marshal(details.Report)
	if err != nil {
		return details, err
	}
	if digestBytes(canonical) != details.ReportSHA256 {
		return details, errors.New("review-scope decision report digest does not match")
	}
	if _, err := checkverdict.Validate(canonical, checkverdict.ModeReviewAggregation); err != nil {
		return details, fmt.Errorf("invalid review-scope decision report: %w", err)
	}
	request := details.Report.DecisionRequest
	if request == nil || request.Key != details.DecisionKey || request.Question != details.Question ||
		request.Rationale != details.Rationale || !equalInts(request.CommentIndexes, details.CommentIndexes) {
		return details, errors.New("review-scope decision request projection does not match report")
	}
	return details, nil
}

func (s *WorkflowRunService) RequestReviewScopeDecision(ctx context.Context, input RequestReviewScopeDecisionInput) (RequestReviewScopeDecisionResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.CheckName = strings.TrimSpace(input.CheckName)
	input.LeaseID = strings.TrimSpace(input.LeaseID)
	input.SourceJobID = strings.TrimSpace(input.SourceJobID)
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	canonical, err := json.Marshal(input.Report)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	validated, err := checkverdict.Validate(canonical, checkverdict.ModeReviewAggregation)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if validated.DecisionRequest == nil {
		return RequestReviewScopeDecisionResult{}, errors.New("review aggregation report has no decision_request")
	}
	canonical, _ = json.Marshal(validated)

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state IN ('scheduled', 'running', 'waiting')`, input.TaskID))
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	nodeRun, err := scanWorkflowNodeRun(tx.QueryRowContext(ctx, workflowNodeRunSelect+` WHERE id = ?`, run.CurrentNodeRunID))
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	node, ok := run.Snapshot.Node(nodeRun.NodeKey)
	if !ok || node.Kind != NodeChangeReview {
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: scope decisions require the active running change-review node", ErrWorkflowConflict)
	}
	if existing, ok, err := requestedScopeDecisionForJobTx(ctx, tx, run.ID, input.SourceJobID); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	} else if ok {
		wait, waiting, err := scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE id = ?`, existing.WaitID))
		if err != nil || !waiting {
			return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: replayed decision wait is unavailable", ErrWorkflowConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return RequestReviewScopeDecisionResult{}, err
		}
		return RequestReviewScopeDecisionResult{Wait: wait, Replayed: true}, nil
	}
	if run.State != WorkflowRunRunning || nodeRun.State != WorkflowNodeRunning {
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: workflow is not running the aggregation job", ErrWorkflowConflict)
	}
	var jobTaskID, jobRunID, jobNodeRunID, jobRole, jobState, jobPayload string
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(task_id,''), COALESCE(workflow_run_id,''), COALESCE(node_run_id,''), role, state, payload_json
FROM jobs WHERE id = ?`, input.SourceJobID).
		Scan(&jobTaskID, &jobRunID, &jobNodeRunID, &jobRole, &jobState, &jobPayload); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(jobPayload), &payload); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	aggregation, _ := payload["review_aggregation"].(bool)
	jobCheckName, _ := payload["check_name"].(string)
	jobHead, _ := payload["head_sha"].(string)
	if jobTaskID != run.TaskID || jobRunID != run.ID || jobNodeRunID != nodeRun.ID || jobRole != "reviewer" ||
		(jobState != "claimed" && jobState != "running") || !aggregation || strings.TrimSpace(jobCheckName) != input.CheckName {
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: source job is not the live aggregation job", ErrWorkflowConflict)
	}
	var leaseWorkerID, leaseExpires string
	var leaseReleased sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT worker_id, expires_at, released_at FROM leases WHERE id = ? AND job_id = ?`, input.LeaseID, input.SourceJobID).
		Scan(&leaseWorkerID, &leaseExpires, &leaseReleased); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	expires, err := sqlitex.ParseTime(leaseExpires)
	if err != nil || leaseWorkerID != input.WorkerID || leaseReleased.Valid || !s.now().UTC().Before(expires) {
		return RequestReviewScopeDecisionResult{}, errors.New("worker does not own the live aggregation lease")
	}
	var changeHead string
	if err := tx.QueryRowContext(ctx, `SELECT head_sha FROM changes WHERE task_id = ? AND workflow_run_id = ?`, run.TaskID, run.ID).Scan(&changeHead); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if strings.TrimSpace(jobHead) == "" || strings.TrimSpace(jobHead) != strings.TrimSpace(changeHead) {
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: aggregation source head is stale", ErrWorkflowConflict)
	}
	if err := verifyPinnedChangeHeadTx(ctx, tx, run.CurrentArtifactID); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	var checkID int64
	var checkVerdict string
	var checkSourceJob sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id, verdict, source_job_id FROM checks WHERE task_id = ? AND name = ?`, run.TaskID, input.CheckName).
		Scan(&checkID, &checkVerdict, &checkSourceJob); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if checkVerdict != string(CheckPending) || !checkSourceJob.Valid || checkSourceJob.String != input.SourceJobID {
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: aggregation check is no longer pending for this job", ErrWorkflowConflict)
	}
	transitions, err := workflowTransitionsForProjection(ctx, tx, run.ID)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	activeRulings, err := ProjectOwnerRulings(transitions)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	for _, ruling := range activeRulings {
		if ruling.Source == OwnerRulingSourceReviewScopeDecision && ruling.Decision != nil && ruling.Decision.DecisionKey == validated.DecisionRequest.Key {
			if s.metrics != nil {
				s.metrics.DecisionRejections.Inc(map[string]string{"reason": "repeated_key"})
			}
			return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: repeated active review-scope decision key", ErrWorkflowConflict)
		}
	}
	var accepted int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'workflow_review_scope_decision_resolved'
	AND json_extract(payload_json, '$.node_run_id') = ?`, run.ID, nodeRun.ID).Scan(&accepted); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if accepted >= MaxReviewScopeDecisionRounds {
		if s.metrics != nil {
			s.metrics.DecisionRejections.Inc(map[string]string{"reason": "attempt_limit"})
		}
		return RequestReviewScopeDecisionResult{}, fmt.Errorf("%w: review-scope decision attempt limit reached", ErrWorkflowConflict)
	}
	waitID, err := randomPrefixedID("ww")
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	sourceChecks, err := reviewScopeSourceChecksTx(ctx, tx, run.TaskID, nodeRun.ID, input.CheckName)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	details := ReviewScopeDecisionWaitDetails{
		SchemaVersion:      ReviewScopeDecisionWaitSchemaVersion,
		AggregationCheckID: checkID, AggregationCheckName: input.CheckName,
		SourceJobID: input.SourceJobID, NodeRunID: nodeRun.ID, NodeAttempt: nodeRun.Attempt,
		SourceHeadSHA: changeHead, Report: validated, ReportSHA256: digestBytes(canonical),
		DecisionKey: validated.DecisionRequest.Key, Question: validated.DecisionRequest.Question,
		Rationale: validated.DecisionRequest.Rationale, CommentIndexes: append([]int(nil), validated.DecisionRequest.CommentIndexes...),
		AllowedChoices: append([]ReviewScopeDecisionChoice(nil), ReviewScopeDecisionChoices...), SourceChecks: sourceChecks,
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_waits
	(id, workflow_run_id, node_run_id, kind, reason, details_json, message, state, created_by, created_at)
VALUES (?, ?, ?, ?, '', ?, ?, 'open', 'agent', ?)`, waitID, run.ID, nodeRun.ID,
		string(WorkflowWaitReviewScopeDecision), string(detailsJSON), validated.DecisionRequest.Question, sqlitex.FormatTime(now)); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = 'waiting' WHERE id = ?`, nodeRun.ID); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = 'waiting', version = version + 1 WHERE id = ?`, run.ID); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	eventPayload, _ := json.Marshal(reviewScopeDecisionRequestedPayload{
		WaitID: waitID, DecisionKey: details.DecisionKey, SourceJobID: input.SourceJobID,
		NodeRunID: nodeRun.ID, ReportSHA256: details.ReportSHA256,
	})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
		EventKind: "workflow_review_scope_decision_requested", PayloadJSON: string(eventPayload), Actor: string(ActorAgent),
		IdempotencyKey: "review-scope-request:" + input.SourceJobID, CreatedAt: now,
	}); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RequestReviewScopeDecisionResult{}, err
	}
	if s.metrics != nil {
		s.metrics.DecisionsOpened.Inc(nil)
	}
	wait := WorkflowWait{ID: waitID, WorkflowRunID: run.ID, NodeRunID: nodeRun.ID, Kind: WorkflowWaitReviewScopeDecision,
		Details: detailsJSON, Message: details.Question, CreatedBy: ActorAgent, CreatedAt: now}
	return RequestReviewScopeDecisionResult{Wait: wait}, nil
}

func (s *WorkflowRunService) ResolveReviewScopeDecision(ctx context.Context, input ResolveReviewScopeDecisionInput) (ResolveReviewScopeDecisionResult, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.WaitID = strings.TrimSpace(input.WaitID)
	input.Guidance = strings.TrimSpace(input.Guidance)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if len([]byte(input.Guidance)) > MaxOwnerGuidanceBytes {
		return ResolveReviewScopeDecisionResult{}, fmt.Errorf("guidance exceeds %d bytes", MaxOwnerGuidanceBytes)
	}
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 255 {
		return ResolveReviewScopeDecisionResult{}, errors.New("idempotency key is required and must not exceed 255 characters")
	}
	if err := validateReviewScopeChoice(input.Choice); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if input.Actor == "" {
		input.Actor = ActorHuman
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	defer tx.Rollback()
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+`
WHERE task_id = ? AND state = 'waiting'`, input.TaskID))
	if err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	wait, ok, err := scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE id = ? AND workflow_run_id = ? AND state = 'open'`, input.WaitID, run.ID))
	if err != nil || !ok || wait.Kind != WorkflowWaitReviewScopeDecision {
		if err == nil {
			err = fmt.Errorf("%w: review-scope decision wait is not open", ErrWorkflowConflict)
		}
		return ResolveReviewScopeDecisionResult{}, err
	}
	details, err := ParseReviewScopeDecisionWaitDetails(wait.Details)
	if err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	var currentHead, changeID string
	if err := tx.QueryRowContext(ctx, `SELECT id, head_sha FROM changes WHERE task_id = ? AND workflow_run_id = ?`, run.TaskID, run.ID).Scan(&changeID, &currentHead); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	now := s.now().UTC()
	if currentHead != details.SourceHeadSHA {
		if err := restartReviewScopeDecisionForStaleHeadTx(ctx, tx, &run, wait, details, changeID, currentHead, input.Actor, now); err != nil {
			return ResolveReviewScopeDecisionResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ResolveReviewScopeDecisionResult{}, err
		}
		if s.metrics != nil {
			s.metrics.DecisionReruns.Inc(map[string]string{"kind": "full_discovery"})
		}
		run, err = s.Get(ctx, run.ID)
		return ResolveReviewScopeDecisionResult{Result: "stale_restarted", Wait: wait, Run: run}, err
	}
	body := reviewScopeRulingBody(input.Choice, details, input.Guidance)
	rulingID, err := randomPrefixedID("rule")
	if err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	rulingPayload := ownerRulingPayload{
		SchemaVersion: OwnerRulingSchemaVersion, RulingID: rulingID, Body: body,
		Source: OwnerRulingSourceReviewScopeDecision, NodeRunID: details.NodeRunID,
		Decision: &OwnerRulingDecision{WaitID: wait.ID, DecisionKey: details.DecisionKey, Choice: string(input.Choice),
			CommentIndexes: append([]int(nil), details.CommentIndexes...), ReportSHA256: details.ReportSHA256},
	}
	rulingJSON, _ := json.Marshal(rulingPayload)
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
		EventKind: OwnerRulingEventKind, PayloadJSON: string(rulingJSON), Actor: string(input.Actor),
		IdempotencyKey: "ruling:" + input.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	eventJSON, _ := json.Marshal(map[string]any{
		"wait_id": wait.ID, "decision_key": details.DecisionKey, "choice": input.Choice,
		"guidance": input.Guidance, "ruling_id": rulingID, "node_run_id": details.NodeRunID,
	})
	if err := insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
		EventKind: "workflow_review_scope_decision_resolved", PayloadJSON: string(eventJSON), Actor: string(input.Actor),
		IdempotencyKey: input.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE checks SET verdict = 'pending', source_job_id = NULL, details = '', exit_code = NULL, reporter = '', updated_at = ?
WHERE id = ? AND task_id = ?`, sqlitex.FormatTime(now), details.AggregationCheckID, run.TaskID); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = 'queued', attempt = attempt + 1 WHERE id = ?`, details.NodeRunID); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = 'running', version = version + 1 WHERE id = ?`, run.ID); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, input.Actor, now); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	ruling := ownerRulingFromPayload(rulingPayload, run.ID, run.TaskID, string(input.Actor), now)
	delivery := s.deliverOwnerRuling(ctx, ruling)
	s.observeOwnerRuling(ruling, delivery)
	if s.metrics != nil {
		s.metrics.DecisionsResolved.Inc(map[string]string{"choice": string(input.Choice)})
		s.metrics.DecisionReruns.Inc(map[string]string{"kind": "aggregation_only"})
	}
	run, err = s.Get(ctx, run.ID)
	if err != nil {
		return ResolveReviewScopeDecisionResult{}, err
	}
	return ResolveReviewScopeDecisionResult{Result: "resolved", Ruling: &ruling, Wait: wait, Run: run, Delivery: &delivery}, nil
}

// ReconcileReviewScopeDecisionHeads invalidates waits whose immutable review
// head no longer matches the change projection. It runs before the executor's
// ordinary active-run scan because waiting runs are intentionally absent from
// that scan.
func (s *WorkflowRunService) ReconcileReviewScopeDecisionHeads(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT w.id, w.workflow_run_id
FROM workflow_waits w
WHERE w.kind = ? AND w.state = 'open'
ORDER BY w.created_at, w.id`, string(WorkflowWaitReviewScopeDecision))
	if err != nil {
		return nil, err
	}
	var candidates [][2]string
	for rows.Next() {
		var candidate [2]string
		if err := rows.Scan(&candidate[0], &candidate[1]); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var restarted []string
	for _, candidate := range candidates {
		tx, err := sqlitex.BeginImmediate(ctx, s.db)
		if err != nil {
			return restarted, err
		}
		run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE id = ? AND state = 'waiting'`, candidate[1]))
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			continue
		}
		if err != nil {
			tx.Rollback()
			return restarted, err
		}
		wait, ok, err := scanWorkflowWaitMaybe(tx.QueryRowContext(ctx, workflowWaitSelect+` WHERE id = ? AND workflow_run_id = ? AND state = 'open'`, candidate[0], run.ID))
		if err != nil || !ok || wait.Kind != WorkflowWaitReviewScopeDecision {
			tx.Rollback()
			if err != nil {
				return restarted, err
			}
			continue
		}
		details, err := ParseReviewScopeDecisionWaitDetails(wait.Details)
		if err != nil {
			tx.Rollback()
			return restarted, err
		}
		var changeID, currentHead string
		if err := tx.QueryRowContext(ctx, `SELECT id, head_sha FROM changes WHERE task_id = ? AND workflow_run_id = ?`, run.TaskID, run.ID).Scan(&changeID, &currentHead); err != nil {
			tx.Rollback()
			return restarted, err
		}
		if currentHead == details.SourceHeadSHA {
			tx.Rollback()
			continue
		}
		now := s.now().UTC()
		if err := restartReviewScopeDecisionForStaleHeadTx(ctx, tx, &run, wait, details, changeID, currentHead, ActorSystem, now); err != nil {
			tx.Rollback()
			return restarted, err
		}
		if err := tx.Commit(ctx); err != nil {
			return restarted, err
		}
		if s.metrics != nil {
			s.metrics.DecisionReruns.Inc(map[string]string{"kind": "full_discovery"})
		}
		restarted = append(restarted, run.ID)
	}
	return restarted, nil
}

func restartReviewScopeDecisionForStaleHeadTx(ctx context.Context, tx workflowTx, run *WorkflowRun, wait WorkflowWait, details ReviewScopeDecisionWaitDetails, changeID, currentHead string, actor Actor, now time.Time) error {
	if strings.TrimSpace(currentHead) == "" {
		return fmt.Errorf("%w: current change head is empty", ErrWorkflowConflict)
	}
	var oldSummary, oldBase string
	if err := tx.QueryRowContext(ctx, `SELECT summary_markdown, base_revision FROM workflow_artifacts WHERE id = ?`, run.CurrentArtifactID).Scan(&oldSummary, &oldBase); err != nil {
		return err
	}
	payload, err := canonicalArtifactPayload(ArtifactChange, json.RawMessage(fmt.Sprintf(`{"change_id":%q,"head_sha":%q}`, changeID, currentHead)))
	if err != nil {
		return err
	}
	artifactID, err := randomPrefixedID("wa")
	if err != nil {
		return err
	}
	digest := artifactDigest(ArtifactChange, oldSummary, payload, oldBase)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_artifacts
	(id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown, payload_json, payload_sha256, base_revision, client_key, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifactID, run.ID, details.NodeRunID,
		"system:review-scope-stale:"+wait.ID, string(ArtifactChange), oldSummary, string(payload), digest, oldBase,
		currentHead, sqlitex.FormatTime(now)); err != nil {
		return err
	}
	nowText := sqlitex.FormatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'canceled', updated_at = ?
WHERE workflow_run_id = ? AND node_run_id = ? AND state IN ('queued','claimed','running')`, nowText, run.ID, details.NodeRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET released_at = COALESCE(released_at, ?)
WHERE job_id IN (SELECT id FROM jobs WHERE workflow_run_id = ? AND node_run_id = ?) AND released_at IS NULL`, nowText, run.ID, details.NodeRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET runtime_state = 'abandoned', updated_at = ?, finished_at = COALESCE(finished_at, ?)
WHERE workflow_run_id = ? AND node_run_id = ? AND runtime_state IN ('starting','working','waiting')`, nowText, nowText, run.ID, details.NodeRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE checks SET verdict = 'pending', source_job_id = NULL, exit_code = NULL,
	details = CASE WHEN name = ? THEN '' ELSE ? END, reporter = '', updated_at = ?
WHERE task_id = ? AND name LIKE ?`, details.AggregationCheckName, ReviewDiscoveryDetailsMarker, nowText, run.TaskID, "%.node."+details.NodeRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET state = 'queued', attempt = attempt + 1, input_artifact_id = ? WHERE id = ?`, artifactID, details.NodeRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state = 'running', current_artifact_id = ?, version = version + 1 WHERE id = ?`, artifactID, run.ID); err != nil {
		return err
	}
	if err := resolveOpenWaitTx(ctx, tx, run.ID, actor, now); err != nil {
		return err
	}
	payloadJSON, _ := json.Marshal(map[string]any{
		"wait_id": wait.ID, "decision_key": details.DecisionKey, "old_head_sha": details.SourceHeadSHA,
		"new_head_sha": currentHead, "node_run_id": details.NodeRunID,
	})
	return insertWorkflowTransitionTx(ctx, tx, workflowTransitionInput{
		TaskID: run.TaskID, WorkflowRunID: run.ID, FromNodeKey: run.CurrentNodeKey, ToNodeKey: run.CurrentNodeKey,
		EventKind: "workflow_review_scope_decision_stale", PayloadJSON: string(payloadJSON), Actor: string(actor),
		CreatedAt: now,
	})
}

func reviewScopeRulingBody(choice ReviewScopeDecisionChoice, details ReviewScopeDecisionWaitDetails, guidance string) string {
	var body string
	switch choice {
	case ReviewScopeFixInTask:
		body = "Owner scope decision: the referenced findings are in scope for this task and are normal blockers when valid."
	case ReviewScopeOutOfScope:
		body = "Owner scope decision: the referenced findings are out of scope for this change; do not file them or create follow-up work."
	case ReviewScopeDeferFollowUp:
		body = "Owner scope decision: the referenced findings are out of scope for this change and may only be retained as non-blocking follow-up work."
	}
	body += " Decision key: " + details.DecisionKey + "."
	body += " Question: " + details.Question
	if guidance != "" {
		body += " Owner guidance: " + guidance
	}
	return body
}

func validateReviewScopeChoice(choice ReviewScopeDecisionChoice) error {
	for _, allowed := range ReviewScopeDecisionChoices {
		if choice == allowed {
			return nil
		}
	}
	return fmt.Errorf("invalid review-scope decision choice %q", choice)
}

func reviewScopeSourceChecksTx(ctx context.Context, tx workflowTx, taskID, nodeRunID, aggregationName string) ([]ReviewScopeSourceCheck, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, details FROM checks
WHERE task_id = ? AND name LIKE ? AND name != ? ORDER BY name`, taskID, "%.node."+nodeRunID, aggregationName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ReviewScopeSourceCheck
	for rows.Next() {
		var name, details string
		if err := rows.Scan(&name, &details); err != nil {
			return nil, err
		}
		result = append(result, ReviewScopeSourceCheck{Name: name, DetailsSHA256: digestBytes([]byte(details))})
	}
	return result, rows.Err()
}

func requestedScopeDecisionForJobTx(ctx context.Context, tx workflowTx, runID, jobID string) (reviewScopeDecisionRequestedPayload, bool, error) {
	var payloadJSON string
	err := tx.QueryRowContext(ctx, `SELECT payload_json FROM workflow_transitions
WHERE workflow_run_id = ? AND event_kind = 'workflow_review_scope_decision_requested'
	AND json_extract(payload_json, '$.source_job_id') = ? ORDER BY seq DESC LIMIT 1`, runID, jobID).Scan(&payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return reviewScopeDecisionRequestedPayload{}, false, nil
	}
	if err != nil {
		return reviewScopeDecisionRequestedPayload{}, false, err
	}
	var payload reviewScopeDecisionRequestedPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return payload, false, err
	}
	return payload, true, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
