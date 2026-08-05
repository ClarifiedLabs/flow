package worker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type JobRole string

const (
	RoleAuthor   JobRole = "author"
	RoleReviewer JobRole = "reviewer"
	RoleVerifier JobRole = "verifier"
	RoleCI       JobRole = "ci"
	RoleConsole  JobRole = "console"
)

type JobState string

const (
	JobQueued   JobState = "queued"
	JobClaimed  JobState = "claimed"
	JobRunning  JobState = "running"
	JobFinished JobState = "finished"
	JobFailed   JobState = "failed"
	JobCrashed  JobState = "crashed"
	JobCanceled JobState = "canceled"
)

type CapacityBucket string

const (
	BucketPersistentAgent CapacityBucket = "persistent_agent"
	BucketEphemeral       CapacityBucket = "ephemeral"
)

type Worker struct {
	ID            string              `json:"id"`
	Labels        map[string]string   `json:"labels"`
	Taints        []scheduler.Taint   `json:"taints"`
	HarnessModels []flowharness.Model `json:"harness_models,omitempty"`
	// CapacityBucket is the worker's singular assignment-derived bucket. Workers
	// are one-shot processes created for one assignment; they never advertise or
	// select buckets themselves.
	CapacityBucket  CapacityBucket `json:"capacity_bucket"`
	Status          string         `json:"status"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	LastHeartbeatAt *time.Time     `json:"last_heartbeat_at"`
	ExpiresAt       *time.Time     `json:"expires_at"`
}

// HarnessRequirement is the runtime capability a job needs. Reasoning effort
// is intentionally absent: Harness presents one portable reasoning interface
// for every model and reasoning never participates in worker scheduling.
type HarnessRequirement struct {
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
}

type Job struct {
	ID             string                 `json:"id"`
	TaskID         *string                `json:"task_id"`
	ChangeID       *string                `json:"change_id"`
	WorkflowRunID  *string                `json:"workflow_run_id,omitempty"`
	NodeRunID      *string                `json:"node_run_id,omitempty"`
	Role           JobRole                `json:"role"`
	State          JobState               `json:"state"`
	CapacityBucket CapacityBucket         `json:"capacity_bucket"`
	Priority       int                    `json:"priority"`
	Selector       map[string]string      `json:"selector"`
	Harness        HarnessRequirement     `json:"harness_requirement,omitempty"`
	Tolerations    []scheduler.Toleration `json:"tolerations"`
	Payload        map[string]any         `json:"payload"`
	TranscriptPath string                 `json:"transcript_path,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
}

type Lease struct {
	ID             string         `json:"id"`
	JobID          string         `json:"job_id"`
	WorkerID       string         `json:"worker_id"`
	CapacityBucket CapacityBucket `json:"capacity_bucket"`
	LeasedAt       time.Time      `json:"leased_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	ReleasedAt     *time.Time     `json:"released_at"`
	RenewalCount   int            `json:"renewal_count"`
}

type ClaimedJob struct {
	Job   Job
	Lease Lease
}

type RegisterWorkerInput struct {
	ID             string
	Labels         map[string]string
	Taints         []scheduler.Taint
	HarnessModels  []flowharness.Model
	CapacityBucket CapacityBucket
	HeartbeatTTL   time.Duration
}

type EnqueueJobInput struct {
	TaskID                                 *string
	ChangeID                               *string
	WorkflowRunID                          *string
	NodeRunID                              *string
	RequireHeldWorkflowRunID               *string
	RequireHeldWorkflowEvidenceFingerprint *string
	Role                                   JobRole
	CapacityBucket                         CapacityBucket
	Priority                               int
	RunsOn                                 map[string]string
	Requires                               []string
	Size                                   string
	Harness                                HarnessRequirement
	Tolerations                            []scheduler.Toleration
	Payload                                map[string]any
	dispatchKey                            string
}

type ClaimInput struct {
	WorkerID      string
	LeaseDuration time.Duration
}

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(database *sql.DB) *Service {
	return &Service{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *Service) EnqueueJob(ctx context.Context, input EnqueueJobInput) (Job, error) {
	if err := validateJobRole(input.Role); err != nil {
		return Job{}, err
	}
	if err := validateJobPayload(input.Role, input.Payload); err != nil {
		return Job{}, err
	}
	if input.Role == RoleAuthor && (input.TaskID == nil || strings.TrimSpace(*input.TaskID) == "") {
		return Job{}, errors.New("author jobs require task id")
	}
	if err := s.validateJobChange(ctx, input.TaskID, input.ChangeID); err != nil {
		return Job{}, err
	}
	if input.RequireHeldWorkflowEvidenceFingerprint != nil && input.RequireHeldWorkflowRunID == nil {
		return Job{}, errors.New("held workflow evidence fingerprint requires workflow run id")
	}
	if input.RequireHeldWorkflowRunID != nil {
		if input.WorkflowRunID == nil || strings.TrimSpace(*input.WorkflowRunID) != strings.TrimSpace(*input.RequireHeldWorkflowRunID) {
			return Job{}, errors.New("held workflow precondition must match job workflow run")
		}
	}
	if input.RequireHeldWorkflowEvidenceFingerprint != nil {
		expectedFingerprint := strings.TrimSpace(*input.RequireHeldWorkflowEvidenceFingerprint)
		if expectedFingerprint == "" || input.ChangeID == nil || strings.TrimSpace(*input.ChangeID) == "" {
			return Job{}, errors.New("held workflow evidence precondition requires fingerprint and change id")
		}
		payload := make(map[string]any, len(input.Payload)+2)
		for key, value := range input.Payload {
			payload[key] = value
		}
		if raw, ok := payload["convergence_evidence_fingerprint"]; ok {
			if supplied := strings.TrimSpace(fmt.Sprint(raw)); supplied != expectedFingerprint {
				return Job{}, errors.New("job payload convergence fingerprint does not match held workflow precondition")
			}
		}
		expectedRunID := strings.TrimSpace(*input.RequireHeldWorkflowRunID)
		if raw, ok := payload["convergence_workflow_run_id"]; ok {
			if supplied := strings.TrimSpace(fmt.Sprint(raw)); supplied != expectedRunID {
				return Job{}, errors.New("job payload convergence workflow does not match held workflow precondition")
			}
		}
		payload["convergence_evidence_fingerprint"] = expectedFingerprint
		payload["convergence_workflow_run_id"] = expectedRunID
		input.Payload = payload
	}
	if input.NodeRunID != nil && input.WorkflowRunID == nil {
		return Job{}, errors.New("node jobs require workflow run id")
	}
	if err := validateCapacityBucket(input.CapacityBucket); err != nil {
		return Job{}, err
	}
	input.Harness.Harness = strings.ToLower(strings.TrimSpace(input.Harness.Harness))
	input.Harness.Model = strings.TrimSpace(input.Harness.Model)
	if input.Harness.Model != "" && input.Harness.Harness == "" {
		return Job{}, errors.New("required model requires a harness")
	}
	if input.Harness.Harness != "" {
		if err := flowharness.ValidateAgentName(input.Harness.Harness); err != nil {
			return Job{}, fmt.Errorf("job harness requirement: %w", err)
		}
	}
	selector, err := scheduler.CompileSelector(scheduler.SelectorInput{
		RunsOn:   input.RunsOn,
		Requires: input.Requires,
		Size:     input.Size,
	})
	if err != nil {
		return Job{}, err
	}
	requirements := selector.Requirements()
	if input.Harness.Harness != "" {
		key := flowharness.AgentHarnessLabel(input.Harness.Harness)
		if current, exists := requirements[key]; exists && current != "true" {
			return Job{}, fmt.Errorf("job selector conflicts with harness requirement %s", input.Harness.Harness)
		}
		requirements[key] = "true"
	}
	selectorJSON, err := encodeStringMap(requirements)
	if err != nil {
		return Job{}, err
	}
	tolerationsJSON, err := encodeTolerations(input.Tolerations)
	if err != nil {
		return Job{}, err
	}
	payload, err := encodeAnyMap(input.Payload)
	if err != nil {
		return Job{}, err
	}

	id, err := randomID("j")
	if err != nil {
		return Job{}, err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (
	id,
	task_id,
	change_id,
	workflow_run_id,
	node_run_id,
	role,
	state,
	capacity_bucket,
	priority,
	selector_json,
	required_harness,
	required_model,
	tolerations_json,
	payload_json,
	dispatch_key,
	created_at,
	updated_at
)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE (
	? IS NULL OR EXISTS (
		SELECT 1 FROM workflow_runs AS wr
		WHERE wr.id = ? AND wr.task_id = ? AND wr.held_at IS NOT NULL
			AND wr.state IN ('scheduled', 'running', 'waiting')
			AND (
				? IS NULL OR EXISTS (
					SELECT 1
					FROM workflow_transitions AS wt
					WHERE wt.workflow_run_id = wr.id
						AND wt.event_kind = 'workflow_convergence_review_requested'
						AND wt.seq = (
							SELECT MAX(latest.seq)
							FROM workflow_transitions AS latest
							WHERE latest.workflow_run_id = wr.id
								AND latest.event_kind = 'workflow_convergence_review_requested'
						)
						AND json_extract(wt.payload_json, '$.fingerprint') = ?
						AND json_extract(wt.payload_json, '$.change_id') = ?
						AND EXISTS (
							SELECT 1 FROM changes AS c
							WHERE c.id = json_extract(wt.payload_json, '$.change_id')
								AND c.task_id = wr.task_id
								AND c.workflow_run_id = wr.id
								AND c.branch = json_extract(wt.payload_json, '$.source_branch')
								AND c.base = json_extract(wt.payload_json, '$.target_base_branch')
								AND c.head_sha = json_extract(wt.payload_json, '$.source_head_sha')
								AND json_extract(?, '$.convergence_workflow_run_id') = wr.id
								AND json_extract(?, '$.convergence_source_head_sha') = c.head_sha
						)
				)
			)
	)
) AND (
	? IS NULL OR EXISTS (
		SELECT 1 FROM tasks WHERE id = ? AND done_at IS NULL
	)
) AND (
	? IS NULL OR EXISTS (
		SELECT 1 FROM workflow_runs AS active_wr
		WHERE active_wr.id = ?
			AND active_wr.task_id = ?
			AND active_wr.state IN ('scheduled', 'running', 'waiting')
	)
) AND (
	? IS NULL OR EXISTS (
		SELECT 1
		FROM workflow_node_runs AS active_nr
		JOIN workflow_runs AS node_wr ON node_wr.id = active_nr.workflow_run_id
		WHERE active_nr.id = ?
			AND active_nr.workflow_run_id = ?
			AND active_nr.state IN ('queued', 'running', 'waiting')
			AND node_wr.current_node_run_id = active_nr.id
			AND node_wr.state IN ('scheduled', 'running', 'waiting')
	)
)`,
		id,
		nullableString(input.TaskID),
		nullableString(input.ChangeID),
		nullableString(input.WorkflowRunID),
		nullableString(input.NodeRunID),
		string(input.Role),
		string(JobQueued),
		string(input.CapacityBucket),
		input.Priority,
		selectorJSON,
		input.Harness.Harness,
		input.Harness.Model,
		tolerationsJSON,
		payload,
		input.dispatchKey,
		formatTime(now),
		formatTime(now),
		nullableString(input.RequireHeldWorkflowRunID),
		nullableString(input.RequireHeldWorkflowRunID),
		nullableString(input.TaskID),
		nullableString(input.RequireHeldWorkflowEvidenceFingerprint),
		nullableString(input.RequireHeldWorkflowEvidenceFingerprint),
		nullableString(input.ChangeID),
		payload,
		payload,
		nullableString(input.TaskID),
		nullableString(input.TaskID),
		nullableString(input.WorkflowRunID),
		nullableString(input.WorkflowRunID),
		nullableString(input.TaskID),
		nullableString(input.NodeRunID),
		nullableString(input.NodeRunID),
		nullableString(input.WorkflowRunID),
	)
	if err != nil {
		return Job{}, fmt.Errorf("enqueue job: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Job{}, fmt.Errorf("read enqueued job rows: %w", err)
	}
	if rows == 0 {
		return Job{}, errors.New("job precondition is no longer active")
	}

	return s.GetJob(ctx, id)
}

// EnqueueJobWithDispatchKey durably deduplicates a trusted internal dispatch.
// Terminal jobs release the partial unique key so failed work can be retried.
func (s *Service) EnqueueJobWithDispatchKey(ctx context.Context, dispatchKey string, input EnqueueJobInput) (Job, bool, error) {
	dispatchKey = strings.TrimSpace(dispatchKey)
	if dispatchKey == "" {
		return Job{}, false, errors.New("dispatch key is required")
	}
	input.dispatchKey = dispatchKey
	job, err := s.EnqueueJob(ctx, input)
	if err == nil {
		return job, true, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: jobs.") {
		return Job{}, false, err
	}
	row := s.db.QueryRowContext(ctx, jobSelectSQL+`
WHERE dispatch_key = ?
	AND state IN (?, ?, ?)
LIMIT 1`, dispatchKey, string(JobQueued), string(JobClaimed), string(JobRunning))
	existing, lookupErr := scanJob(row)
	if lookupErr != nil {
		return Job{}, false, err
	}
	return existing, false, nil
}

func (s *Service) GetJob(ctx context.Context, jobID string) (Job, error) {
	row := s.db.QueryRowContext(ctx, jobSelectSQL+`
WHERE id = ?`, jobID)

	return scanJob(row)
}

// SetJobTranscriptPath records where the coordinator stored the job's tmux
// transcript. It is keyed by job id.
func (s *Service) SetJobTranscriptPath(ctx context.Context, jobID string, path string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("job id is required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET transcript_path = ?,
	updated_at = ?
WHERE id = ?`,
		strings.TrimSpace(path),
		formatTime(s.now().UTC()),
		jobID,
	)
	if err != nil {
		return fmt.Errorf("set job transcript path: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read job transcript update rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

func (s *Service) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelectSQL+`
ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}

	return jobs, nil
}

func (s *Service) LiveAuthorJobForTask(ctx context.Context, taskID string) (Job, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Job{}, false, errors.New("task id is required")
	}

	row := s.db.QueryRowContext(ctx, jobSelectSQL+`
WHERE task_id = ?
	AND role = ?
	AND state IN (?, ?, ?)
ORDER BY created_at
LIMIT 1`,
		taskID,
		string(RoleAuthor),
		string(JobQueued),
		string(JobClaimed),
		string(JobRunning),
	)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}

	return job, true, nil
}

func (s *Service) validateJobChange(ctx context.Context, taskID *string, changeID *string) error {
	change := strings.TrimSpace(stringPointerValue(changeID))
	if change == "" {
		return nil
	}
	task := strings.TrimSpace(stringPointerValue(taskID))
	if task == "" {
		return errors.New("change jobs require task id")
	}

	var changeTaskID string
	if err := s.db.QueryRowContext(ctx, `
SELECT task_id
FROM changes
WHERE id = ?`, change).Scan(&changeTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("change not found")
		}
		return fmt.Errorf("load job change: %w", err)
	}
	if strings.TrimSpace(changeTaskID) != task {
		return errors.New("job change does not belong to task")
	}

	return nil
}

// cancelStaleQueuedJobsTx cancels queued jobs whose task, workflow run,
// workflow node run, or held convergence precondition is no longer active, so
// stale work never waits for a lease to claim it. It also closes assignments
// bound to terminal jobs.
func cancelStaleQueuedJobsTx(ctx context.Context, tx txExecutor, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE state = ?
	AND (
		(task_id IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM tasks AS t WHERE t.id = jobs.task_id AND t.done_at IS NULL
		))
		OR (workflow_run_id IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM workflow_runs AS wr
			WHERE wr.id = jobs.workflow_run_id
				AND wr.task_id = jobs.task_id
				AND wr.state IN ('scheduled', 'running', 'waiting')
		))
		OR (node_run_id IS NOT NULL AND NOT EXISTS (
			SELECT 1
			FROM workflow_node_runs AS nr
			JOIN workflow_runs AS wr ON wr.id = nr.workflow_run_id
			WHERE nr.id = jobs.node_run_id
				AND nr.workflow_run_id = jobs.workflow_run_id
				AND nr.state IN ('queued', 'running', 'waiting')
				AND wr.current_node_run_id = nr.id
				AND wr.state IN ('scheduled', 'running', 'waiting')
		))
		OR (
			json_type(payload_json, '$.convergence_evidence_fingerprint') IS NOT NULL
			AND NOT EXISTS (
				SELECT 1
				FROM workflow_runs AS held_wr
				JOIN workflow_transitions AS wt ON wt.workflow_run_id = held_wr.id
				JOIN changes AS c ON c.id = jobs.change_id
				WHERE held_wr.id = jobs.workflow_run_id
					AND held_wr.task_id = jobs.task_id
					AND held_wr.held_at IS NOT NULL
					AND held_wr.state IN ('scheduled', 'running', 'waiting')
					AND wt.event_kind = 'workflow_convergence_review_requested'
					AND wt.seq = (
						SELECT MAX(latest.seq)
						FROM workflow_transitions AS latest
						WHERE latest.workflow_run_id = held_wr.id
							AND latest.event_kind = 'workflow_convergence_review_requested'
					)
					AND json_extract(wt.payload_json, '$.fingerprint') = json_extract(jobs.payload_json, '$.convergence_evidence_fingerprint')
					AND json_extract(wt.payload_json, '$.change_id') = jobs.change_id
					AND c.task_id = held_wr.task_id
					AND c.workflow_run_id = held_wr.id
					AND c.branch = json_extract(wt.payload_json, '$.source_branch')
					AND c.base = json_extract(wt.payload_json, '$.target_base_branch')
					AND c.head_sha = json_extract(wt.payload_json, '$.source_head_sha')
					AND json_extract(jobs.payload_json, '$.convergence_workflow_run_id') = held_wr.id
					AND json_extract(jobs.payload_json, '$.convergence_source_head_sha') = c.head_sha
			)
		)
	)`, string(JobCanceled), formatTime(now), string(JobQueued)); err != nil {
		return fmt.Errorf("cancel stale queued jobs: %w", err)
	}
	return closeTerminalAssignmentsTx(ctx, tx, now)
}

func (s *Service) MarkJobRunning(ctx context.Context, leaseID string) (Job, error) {
	jobID, err := s.liveLeaseJobID(ctx, leaseID)
	if err != nil {
		return Job{}, err
	}

	result, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE id = ? AND state = ?`,
		string(JobRunning),
		formatTime(s.now().UTC()),
		jobID,
		string(JobClaimed),
	)
	if err != nil {
		return Job{}, fmt.Errorf("mark job running: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Job{}, fmt.Errorf("read mark running rows affected: %w", err)
	}
	if rows == 0 {
		return Job{}, errors.New("job is not claimed")
	}

	return s.GetJob(ctx, jobID)
}

func (s *Service) RenewLease(ctx context.Context, leaseID string, duration time.Duration) (Lease, error) {
	if duration <= 0 {
		return Lease{}, errors.New("lease duration must be positive")
	}

	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?,
	renewal_count = renewal_count + 1
WHERE id = ?
	AND released_at IS NULL
	AND expires_at > ?`,
		formatTime(now.Add(duration)),
		leaseID,
		formatTime(now),
	)
	if err != nil {
		return Lease{}, fmt.Errorf("renew lease: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Lease{}, fmt.Errorf("read renew rows affected: %w", err)
	}
	if rows == 0 {
		return Lease{}, sql.ErrNoRows
	}

	return s.GetLease(ctx, leaseID)
}

// ExtendActiveLeaseDeadlines protects work that was already active when the
// coordinator started. It moves every unreleased claimed/running lease's
// deadline forward to at least the supplied time, without shortening leases
// that already extend beyond it. The startup path calls this before serving
// traffic or running crash recovery so reconnecting workers retain exclusive
// ownership during the configured grace window.
func (s *Service) ExtendActiveLeaseDeadlines(ctx context.Context, deadline time.Time) (int, error) {
	deadline = deadline.UTC()
	if !deadline.After(s.now().UTC()) {
		return 0, errors.New("active lease deadline must be in the future")
	}
	formattedDeadline := formatTime(deadline)
	result, err := s.db.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE released_at IS NULL
	AND expires_at < ?
	AND EXISTS (
		SELECT 1
		FROM jobs
		WHERE jobs.id = leases.job_id
			AND jobs.state IN (?, ?)
	)`,
		formattedDeadline,
		formattedDeadline,
		string(JobClaimed),
		string(JobRunning),
	)
	if err != nil {
		return 0, fmt.Errorf("extend active lease deadlines: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read extended active lease rows affected: %w", err)
	}
	return int(rows), nil
}

func (s *Service) ReleaseLease(ctx context.Context, leaseID string, finalState JobState) (Job, error) {
	if !IsTerminalJobState(finalState) {
		return Job{}, errors.New("released jobs require a terminal final state")
	}

	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return Job{}, fmt.Errorf("begin release transaction: %w", err)
	}
	defer tx.Rollback()

	lease, err := getLeaseTx(ctx, tx, leaseID)
	if err != nil {
		return Job{}, err
	}
	if lease.ReleasedAt != nil {
		job, getErr := getJobTx(ctx, tx, lease.JobID)
		if getErr != nil {
			return Job{}, getErr
		}
		if job.State == finalState {
			return job, nil
		}
		return Job{}, errors.New("lease is already released with a different final state")
	}

	now := s.now().UTC()
	if !lease.ExpiresAt.After(now) {
		return Job{}, errors.New("lease is expired")
	}
	result, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = ?
WHERE id = ? AND released_at IS NULL`, formatTime(now), leaseID)
	if err != nil {
		return Job{}, fmt.Errorf("release lease: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Job{}, fmt.Errorf("read release rows affected: %w", err)
	}
	if rows == 0 {
		return Job{}, errors.New("lease is already released")
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE id = ?`, string(finalState), formatTime(now), lease.JobID); err != nil {
		return Job{}, fmt.Errorf("finish leased job: %w", err)
	}
	if err := closeAssignmentsForJobTx(ctx, tx, lease.JobID, string(finalState), now); err != nil {
		return Job{}, err
	}

	job, err := getJobTx(ctx, tx, lease.JobID)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("commit release transaction: %w", err)
	}

	return job, nil
}

// CancelLiveJobsForTask cancels every queued, claimed, or running job of the
// given role for the task, releasing any live leases so the lease sweeper does
// not later mark the canceled jobs crashed. It returns the canceled job IDs.
// Workers running a canceled job observe the terminal state through their
// session reconciler and stop the job's session themselves.
func (s *Service) CancelLiveJobsForTask(ctx context.Context, taskID string, role JobRole) ([]string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}

	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("begin cancel transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM jobs
WHERE task_id = ?
	AND role = ?
	AND state IN (?, ?, ?)
ORDER BY created_at, id`,
		taskID,
		string(role),
		string(JobQueued),
		string(JobClaimed),
		string(JobRunning),
	)
	if err != nil {
		return nil, fmt.Errorf("select live jobs for task: %w", err)
	}
	var jobIDs []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan live job: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close live jobs rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live jobs: %w", err)
	}
	if len(jobIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty cancel transaction: %w", err)
		}
		return nil, nil
	}

	now := formatTime(s.now().UTC())
	for _, jobID := range jobIDs {
		if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = ?
WHERE job_id = ? AND released_at IS NULL`, now, jobID); err != nil {
			return nil, fmt.Errorf("release lease for canceled job: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE id = ?`, string(JobCanceled), now, jobID); err != nil {
			return nil, fmt.Errorf("cancel job: %w", err)
		}
		if err := closeAssignmentsForJobTx(ctx, tx, jobID, string(JobCanceled), s.now().UTC()); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit cancel transaction: %w", err)
	}

	return jobIDs, nil
}

func (s *Service) SweepExpiredLeases(ctx context.Context) (int, error) {
	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("begin sweep transaction: %w", err)
	}
	defer tx.Rollback()

	now := s.now().UTC()
	rows, err := tx.QueryContext(ctx, `
SELECT job_id
FROM leases
WHERE released_at IS NULL
	AND expires_at <= ?`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("select expired leases: %w", err)
	}
	var jobIDs []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired lease: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired leases rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired leases: %w", err)
	}
	if len(jobIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit empty sweep transaction: %w", err)
		}
		return 0, nil
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE leases
SET released_at = ?
WHERE released_at IS NULL
	AND expires_at <= ?`, formatTime(now), formatTime(now)); err != nil {
		return 0, fmt.Errorf("release expired leases: %w", err)
	}
	for _, jobID := range jobIDs {
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE id = ? AND state IN (?, ?)`,
			string(JobCrashed),
			formatTime(now),
			jobID,
			string(JobClaimed),
			string(JobRunning),
		); err != nil {
			return 0, fmt.Errorf("mark expired job crashed: %w", err)
		}
		if err := closeAssignmentsForJobTx(ctx, tx, jobID, string(JobCrashed), now); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit sweep transaction: %w", err)
	}

	return len(jobIDs), nil
}

func (s *Service) GetLease(ctx context.Context, leaseID string) (Lease, error) {
	row := s.db.QueryRowContext(ctx, leaseSelectSQL+`
WHERE id = ?`, leaseID)

	return scanLease(row)
}

func (s *Service) ListLeases(ctx context.Context) ([]Lease, error) {
	rows, err := s.db.QueryContext(ctx, leaseSelectSQL+`
ORDER BY released_at IS NOT NULL, leased_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	defer rows.Close()

	var leases []Lease
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate leases: %w", err)
	}

	return leases, nil
}

func (s *Service) liveLeaseJobID(ctx context.Context, leaseID string) (string, error) {
	var jobID string
	if err := s.db.QueryRowContext(ctx, `
SELECT job_id
FROM leases
WHERE id = ?
	AND released_at IS NULL
	AND expires_at > ?`, leaseID, formatTime(s.now().UTC())).Scan(&jobID); err != nil {
		return "", fmt.Errorf("load live lease: %w", err)
	}

	return jobID, nil
}

type txExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var beginImmediate = sqlitex.BeginImmediate

// jobSelectSQL is the canonical full-column job projection scanned by scanJob;
// callers append their own WHERE/ORDER/LIMIT clauses.
const jobSelectSQL = `
SELECT
	id,
	task_id,
	change_id,
	workflow_run_id,
	node_run_id,
	role,
	state,
	capacity_bucket,
	priority,
	selector_json,
	required_harness,
	required_model,
	tolerations_json,
	payload_json,
	transcript_path,
	created_at,
	updated_at
FROM jobs`

func getJobTx(ctx context.Context, tx txExecutor, jobID string) (Job, error) {
	row := tx.QueryRowContext(ctx, jobSelectSQL+`
WHERE id = ?`, jobID)

	return scanJob(row)
}

// leaseSelectSQL is the canonical full-column lease projection scanned by
// scanLease; callers append their own WHERE/ORDER clauses.
const leaseSelectSQL = `
SELECT
	id,
	job_id,
	worker_id,
	capacity_bucket,
	leased_at,
	expires_at,
	released_at,
	renewal_count
FROM leases`

func getLeaseTx(ctx context.Context, tx txExecutor, leaseID string) (Lease, error) {
	row := tx.QueryRowContext(ctx, leaseSelectSQL+`
WHERE id = ?`, leaseID)

	return scanLease(row)
}

// UsedCapacity reports the worker's live leases in this project's database,
// grouped by capacity bucket. The registry sums it over every project to
// enforce the one-live-lease rule for one-shot workers.
func (s *Service) UsedCapacity(ctx context.Context, workerID string) (scheduler.Capacity, error) {
	return usedCapacity(ctx, s.db, strings.TrimSpace(workerID))
}

func usedCapacity(ctx context.Context, tx txExecutor, workerID string) (scheduler.Capacity, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT capacity_bucket, COUNT(*)
FROM leases
WHERE worker_id = ?
	AND released_at IS NULL
GROUP BY capacity_bucket`, workerID)
	if err != nil {
		return scheduler.Capacity{}, fmt.Errorf("load used capacity: %w", err)
	}
	defer rows.Close()

	var used scheduler.Capacity
	for rows.Next() {
		var bucket string
		var count int
		if err := rows.Scan(&bucket, &count); err != nil {
			return scheduler.Capacity{}, fmt.Errorf("scan used capacity: %w", err)
		}
		switch CapacityBucket(bucket) {
		case BucketPersistentAgent:
			used.PersistentAgent = count
		case BucketEphemeral:
			used.Ephemeral = count
		}
	}
	if err := rows.Err(); err != nil {
		return scheduler.Capacity{}, fmt.Errorf("iterate used capacity: %w", err)
	}

	return used, nil
}

func queuedJobCandidates(ctx context.Context, tx txExecutor, buckets []CapacityBucket) ([]Job, error) {
	query, args := queuedJobsQuery(buckets)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select queued jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued jobs: %w", err)
	}

	return jobs, nil
}

func queuedJobsQuery(buckets []CapacityBucket) (string, []any) {
	placeholders := make([]string, len(buckets))
	args := make([]any, len(buckets))
	for i, bucket := range buckets {
		placeholders[i] = "?"
		args[i] = string(bucket)
	}

	return jobSelectSQL + `
WHERE state = 'queued'
	AND capacity_bucket IN (` + strings.Join(placeholders, ", ") + `)
	AND NOT EXISTS (
		SELECT 1 FROM worker_assignments AS assignment
		WHERE assignment.job_id = jobs.id AND assignment.state IN ('pending', 'claimed')
	)
ORDER BY priority DESC, created_at ASC, id ASC
`, args
}

func eligibleForWorker(job Job, worker Worker, used scheduler.Capacity) (bool, error) {
	selector, err := scheduler.NewSelector(job.Selector)
	if err != nil {
		return false, err
	}

	persistent := worker.CapacityBucket == BucketPersistentAgent || (worker.CapacityBucket == "" && job.CapacityBucket == BucketPersistentAgent)
	ephemeral := worker.CapacityBucket == BucketEphemeral || (worker.CapacityBucket == "" && job.CapacityBucket == BucketEphemeral)
	return scheduler.Eligible(scheduler.Job{
		Selector:       selector,
		Tolerations:    job.Tolerations,
		CapacityBucket: scheduler.CapacityBucket(job.CapacityBucket),
	}, scheduler.Worker{
		Labels: worker.Labels,
		Taints: worker.Taints,
		Capacity: scheduler.Capacity{
			PersistentAgent: boolToInt(persistent),
			Ephemeral:       boolToInt(ephemeral),
		},
		Used: used,
	})
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// CancelStaleQueuedJobs cancels queued jobs whose task, workflow run,
// workflow node run, or held convergence precondition is no longer active and
// closes assignments bound to terminal jobs. It commits unconditionally so
// cleanup survives a later claim failure, matching the pre-claim pass the
// general queue claim performed.
func (s *Service) CancelStaleQueuedJobs(ctx context.Context) error {
	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return fmt.Errorf("begin stale job cancellation transaction: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if err := cancelStaleQueuedJobsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit stale job cancellation: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanWorker(row scanner) (Worker, error) {
	var worker Worker
	var labelsJSON string
	var taintsJSON string
	var harnessModelsJSON string
	var capacityBucket string
	var createdAt string
	var updatedAt string
	var lastHeartbeatAt sql.NullString
	var expiresAt sql.NullString
	if err := row.Scan(
		&worker.ID,
		&labelsJSON,
		&taintsJSON,
		&harnessModelsJSON,
		&capacityBucket,
		&worker.Status,
		&createdAt,
		&updatedAt,
		&lastHeartbeatAt,
		&expiresAt,
	); err != nil {
		return Worker{}, fmt.Errorf("scan worker: %w", err)
	}
	if capacityBucket != "" {
		if err := validateCapacityBucket(CapacityBucket(capacityBucket)); err != nil {
			return Worker{}, fmt.Errorf("worker %s capacity bucket: %w", worker.ID, err)
		}
	}
	worker.CapacityBucket = CapacityBucket(capacityBucket)

	labels, err := decodeStringMap(labelsJSON)
	if err != nil {
		return Worker{}, err
	}
	taints, err := decodeTaints(taintsJSON)
	if err != nil {
		return Worker{}, err
	}
	harnessModels, err := decodeHarnessModels(harnessModelsJSON)
	if err != nil {
		return Worker{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Worker{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Worker{}, err
	}

	worker.Labels = labels
	worker.Taints = taints
	worker.HarnessModels = harnessModels
	worker.CreatedAt = parsedCreatedAt
	worker.UpdatedAt = parsedUpdatedAt
	worker.LastHeartbeatAt, err = nullableParsedTime(lastHeartbeatAt)
	if err != nil {
		return Worker{}, err
	}
	worker.ExpiresAt, err = nullableParsedTime(expiresAt)
	if err != nil {
		return Worker{}, err
	}

	return worker, nil
}

func scanJob(row scanner) (Job, error) {
	var job Job
	var taskID sql.NullString
	var changeID sql.NullString
	var workflowRunID sql.NullString
	var nodeRunID sql.NullString
	var role string
	var state string
	var bucket string
	var selectorJSON string
	var requiredHarness string
	var requiredModel string
	var tolerationsJSON string
	var payloadJSON string
	var transcriptPath string
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&job.ID,
		&taskID,
		&changeID,
		&workflowRunID,
		&nodeRunID,
		&role,
		&state,
		&bucket,
		&job.Priority,
		&selectorJSON,
		&requiredHarness,
		&requiredModel,
		&tolerationsJSON,
		&payloadJSON,
		&transcriptPath,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Job{}, fmt.Errorf("scan job: %w", err)
	}

	payload, err := decodeAnyMap(payloadJSON)
	if err != nil {
		return Job{}, err
	}
	if err := validateJobPayload(JobRole(role), payload); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", job.ID, err)
	}
	selector, err := decodeStringMap(selectorJSON)
	if err != nil {
		return Job{}, err
	}
	tolerations, err := decodeTolerations(tolerationsJSON)
	if err != nil {
		return Job{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Job{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Job{}, err
	}

	job.TaskID = nullableStringPointer(taskID)
	job.ChangeID = nullableStringPointer(changeID)
	job.WorkflowRunID = nullableStringPointer(workflowRunID)
	job.NodeRunID = nullableStringPointer(nodeRunID)
	job.Role = JobRole(role)
	job.State = JobState(state)
	job.CapacityBucket = CapacityBucket(bucket)
	job.Selector = selector
	job.Harness = HarnessRequirement{Harness: requiredHarness, Model: requiredModel}
	job.Tolerations = tolerations
	job.Payload = payload
	job.TranscriptPath = transcriptPath
	job.CreatedAt = parsedCreatedAt
	job.UpdatedAt = parsedUpdatedAt
	return job, nil
}

// validateJobPayload enforces the complete-payload contract: producers stamp
// every key a consumer needs, so consumers never reconstruct missing values.
// An absent or wrong-typed key marks the job corrupt instead of silently
// changing behavior. False and zero are valid explicit values.
func validateJobPayload(role JobRole, payload map[string]any) error {
	switch role {
	case RoleAuthor:
		harness, ok := payload["agent_harness"].(string)
		if !ok || strings.TrimSpace(harness) == "" {
			return errors.New("author job payload requires a nonblank agent_harness")
		}
		if err := flowharness.ValidateAgentName(harness); err != nil {
			return fmt.Errorf("author job payload: %w", err)
		}
		if _, err := PayloadPhaseIndex(payload); err != nil {
			return fmt.Errorf("author job payload: %w", err)
		}
	case RoleConsole:
		harness, ok := payload["console_harness"].(string)
		if !ok || strings.TrimSpace(harness) == "" {
			return errors.New("console job payload requires a nonblank console_harness")
		}
		if err := flowharness.ValidateConsoleName(harness); err != nil {
			return fmt.Errorf("console job payload: %w", err)
		}
	case RoleReviewer, RoleVerifier, RoleCI:
		if _, ok := payload["blocking"].(bool); !ok {
			return errors.New("check job payload requires an explicit boolean blocking")
		}
	}
	return nil
}

// PayloadPhaseIndex reads the explicitly stamped phase index,
// distinguishing a valid zero from an absent or wrong-typed key. JSON
// round-trips numbers as float64.
func PayloadPhaseIndex(payload map[string]any) (int, error) {
	value, ok := payload["phase_index"]
	if !ok || value == nil {
		return 0, errors.New("phase_index is required")
	}
	switch typed := value.(type) {
	case int:
		if typed < 0 {
			return 0, errors.New("phase_index must be >= 0")
		}
		return typed, nil
	case int64:
		if typed < 0 {
			return 0, errors.New("phase_index must be >= 0")
		}
		return int(typed), nil
	case float64:
		if typed < 0 || typed != float64(int(typed)) {
			return 0, errors.New("phase_index must be a non-negative integer")
		}
		return int(typed), nil
	default:
		return 0, fmt.Errorf("phase_index must be an integer, got %T", value)
	}
}

func scanLease(row scanner) (Lease, error) {
	var lease Lease
	var bucket string
	var leasedAt string
	var expiresAt string
	var releasedAt sql.NullString
	if err := row.Scan(
		&lease.ID,
		&lease.JobID,
		&lease.WorkerID,
		&bucket,
		&leasedAt,
		&expiresAt,
		&releasedAt,
		&lease.RenewalCount,
	); err != nil {
		return Lease{}, fmt.Errorf("scan lease: %w", err)
	}

	parsedLeasedAt, err := parseTime(leasedAt)
	if err != nil {
		return Lease{}, err
	}
	parsedExpiresAt, err := parseTime(expiresAt)
	if err != nil {
		return Lease{}, err
	}

	lease.CapacityBucket = CapacityBucket(bucket)
	lease.LeasedAt = parsedLeasedAt
	lease.ExpiresAt = parsedExpiresAt
	lease.ReleasedAt, err = nullableParsedTime(releasedAt)
	if err != nil {
		return Lease{}, err
	}

	return lease, nil
}

func validateJobRole(role JobRole) error {
	switch role {
	case RoleAuthor, RoleReviewer, RoleVerifier, RoleCI, RoleConsole:
		return nil
	default:
		return fmt.Errorf("invalid job role: %s", role)
	}
}

func validateCapacityBucket(bucket CapacityBucket) error {
	switch bucket {
	case BucketPersistentAgent, BucketEphemeral:
		return nil
	default:
		return fmt.Errorf("invalid capacity bucket: %s", bucket)
	}
}

// IsTerminalJobState reports whether state is a terminal (no further work) job
// state. It is the single definition shared by the worker store, execution, and
// tmux reaping.
func IsTerminalJobState(state JobState) bool {
	switch state {
	case JobFinished, JobFailed, JobCrashed, JobCanceled:
		return true
	default:
		return false
	}
}

func normalizeLabelsJSON(value map[string]string) (string, error) {
	labels, err := scheduler.NormalizeLabels(value)
	if err != nil {
		return "", err
	}

	return encodeStringMap(labels)
}

func encodeHarnessModels(value []flowharness.Model) (string, error) {
	normalized, err := flowharness.NormalizeModels(value)
	if err != nil {
		return "", err
	}
	if normalized == nil {
		normalized = []flowharness.Model{}
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode harness models: %w", err)
	}
	return string(encoded), nil
}

func decodeHarnessModels(value string) ([]flowharness.Model, error) {
	if strings.TrimSpace(value) == "" {
		value = "[]"
	}
	var decoded []flowharness.Model
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode harness models: %w", err)
	}
	normalized, err := flowharness.NormalizeModels(decoded)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func encodeStringMap(value map[string]string) (string, error) {
	if value == nil {
		value = map[string]string{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode string map: %w", err)
	}

	return string(encoded), nil
}

func decodeStringMap(value string) (map[string]string, error) {
	var decoded map[string]string
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode string map: %w", err)
	}
	if decoded == nil {
		decoded = map[string]string{}
	}

	return decoded, nil
}

func encodeTaints(value []scheduler.Taint) (string, error) {
	normalized := make([]scheduler.Taint, 0, len(value))
	for _, taint := range value {
		item, err := scheduler.NormalizeTaint(taint)
		if err != nil {
			return "", err
		}
		normalized = append(normalized, item)
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode taints: %w", err)
	}

	return string(encoded), nil
}

func decodeTaints(value string) ([]scheduler.Taint, error) {
	var decoded []scheduler.Taint
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode taints: %w", err)
	}
	if decoded == nil {
		decoded = []scheduler.Taint{}
	}

	return decoded, nil
}

func encodeTolerations(value []scheduler.Toleration) (string, error) {
	normalized := make([]scheduler.Toleration, 0, len(value))
	for _, toleration := range value {
		item, err := scheduler.NormalizeToleration(toleration)
		if err != nil {
			return "", err
		}
		normalized = append(normalized, item)
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode tolerations: %w", err)
	}

	return string(encoded), nil
}

func decodeTolerations(value string) ([]scheduler.Toleration, error) {
	var decoded []scheduler.Toleration
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode tolerations: %w", err)
	}
	if decoded == nil {
		decoded = []scheduler.Toleration{}
	}

	return decoded, nil
}

func encodeAnyMap(value map[string]any) (string, error) {
	if value == nil {
		value = map[string]any{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode payload: %w", err)
	}

	return string(encoded), nil
}

func decodeAnyMap(value string) (map[string]any, error) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	if decoded == nil {
		decoded = map[string]any{}
	}

	return decoded, nil
}

var nullableString = sqlitex.NullableString

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

var nullableStringPointer = sqlitex.NullableStringPointer

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}

	return formatTime(*value)
}

func nullableParsedTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func randomID(prefix string) (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}

	return prefix + "-" + hex.EncodeToString(bytes), nil
}

var (
	formatTime = sqlitex.FormatTime
	parseTime  = sqlitex.ParseTime
)
