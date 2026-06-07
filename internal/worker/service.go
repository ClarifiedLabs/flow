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
	ID                      string              `json:"id"`
	Labels                  map[string]string   `json:"labels"`
	Taints                  []scheduler.Taint   `json:"taints"`
	HarnessModels           []flowharness.Model `json:"harness_models,omitempty"`
	CapacityPersistentAgent int                 `json:"capacity_persistent_agent"`
	CapacityEphemeral       int                 `json:"capacity_ephemeral"`
	Status                  string              `json:"status"`
	CreatedAt               time.Time           `json:"created_at"`
	UpdatedAt               time.Time           `json:"updated_at"`
	LastHeartbeatAt         *time.Time          `json:"last_heartbeat_at"`
	ExpiresAt               *time.Time          `json:"expires_at"`
}

type Job struct {
	ID             string                 `json:"id"`
	IssueID        *string                `json:"issue_id"`
	ChangeID       *string                `json:"change_id"`
	Role           JobRole                `json:"role"`
	State          JobState               `json:"state"`
	CapacityBucket CapacityBucket         `json:"capacity_bucket"`
	Priority       int                    `json:"priority"`
	Selector       map[string]string      `json:"selector"`
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
	ID                      string
	Labels                  map[string]string
	Taints                  []scheduler.Taint
	HarnessModels           []flowharness.Model
	CapacityPersistentAgent int
	CapacityEphemeral       int
	HeartbeatTTL            time.Duration
}

type EnqueueJobInput struct {
	IssueID        *string
	ChangeID       *string
	Role           JobRole
	CapacityBucket CapacityBucket
	Priority       int
	RunsOn         map[string]string
	Requires       []string
	Size           string
	Tolerations    []scheduler.Toleration
	Payload        map[string]any
}

type ClaimInput struct {
	WorkerID      string
	Buckets       []CapacityBucket
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
	if input.Role == RoleAuthor && (input.IssueID == nil || strings.TrimSpace(*input.IssueID) == "") {
		return Job{}, errors.New("author jobs require issue id")
	}
	if err := s.validateJobChange(ctx, input.IssueID, input.ChangeID); err != nil {
		return Job{}, err
	}
	if err := validateCapacityBucket(input.CapacityBucket); err != nil {
		return Job{}, err
	}
	selector, err := scheduler.CompileSelector(scheduler.SelectorInput{
		RunsOn:   input.RunsOn,
		Requires: input.Requires,
		Size:     input.Size,
	})
	if err != nil {
		return Job{}, err
	}
	selectorJSON, err := encodeStringMap(selector.Requirements())
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
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (
	id,
	issue_id,
	change_id,
	role,
	state,
	capacity_bucket,
	priority,
	selector_json,
	tolerations_json,
	payload_json,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		nullableString(input.IssueID),
		nullableString(input.ChangeID),
		string(input.Role),
		string(JobQueued),
		string(input.CapacityBucket),
		input.Priority,
		selectorJSON,
		tolerationsJSON,
		payload,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return Job{}, fmt.Errorf("enqueue job: %w", err)
	}

	return s.GetJob(ctx, id)
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

func (s *Service) LiveAuthorJobForIssue(ctx context.Context, issueID string) (Job, bool, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return Job{}, false, errors.New("issue id is required")
	}

	row := s.db.QueryRowContext(ctx, jobSelectSQL+`
WHERE issue_id = ?
	AND role = ?
	AND state IN (?, ?, ?)
ORDER BY created_at
LIMIT 1`,
		issueID,
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

func (s *Service) validateJobChange(ctx context.Context, issueID *string, changeID *string) error {
	change := strings.TrimSpace(stringPointerValue(changeID))
	if change == "" {
		return nil
	}
	issue := strings.TrimSpace(stringPointerValue(issueID))
	if issue == "" {
		return errors.New("change jobs require issue id")
	}

	var changeIssueID string
	if err := s.db.QueryRowContext(ctx, `
SELECT issue_id
FROM changes
WHERE id = ?`, change).Scan(&changeIssueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("change not found")
		}
		return fmt.Errorf("load job change: %w", err)
	}
	if strings.TrimSpace(changeIssueID) != issue {
		return errors.New("job change does not belong to issue")
	}

	return nil
}

// claimQueuedJob claims the next eligible queued job in this project's
// database for the given worker. Capacity has already been checked against
// the aggregate of live leases across all projects by ClaimAcrossProjects;
// the worker row and aggregate used capacity are passed in because the
// workers table lives in the coordinator's global database.
func (s *Service) claimQueuedJob(ctx context.Context, worker Worker, buckets []CapacityBucket, used scheduler.Capacity, leaseDuration time.Duration) (ClaimedJob, bool, error) {
	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return ClaimedJob{}, false, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer tx.Rollback()

	candidates, err := queuedJobCandidates(ctx, tx, buckets)
	if err != nil {
		return ClaimedJob{}, false, err
	}
	var selected Job
	for _, candidate := range candidates {
		eligible, err := eligibleForWorker(candidate, worker, used)
		if err != nil {
			return ClaimedJob{}, false, err
		}
		if eligible {
			selected = candidate
			break
		}
	}
	if selected.ID == "" {
		return ClaimedJob{}, false, nil
	}

	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = ?, updated_at = ?
WHERE id = ? AND state = ?`,
		string(JobClaimed),
		formatTime(now),
		selected.ID,
		string(JobQueued),
	)
	if err != nil {
		return ClaimedJob{}, false, fmt.Errorf("claim job: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ClaimedJob{}, false, fmt.Errorf("read claim rows affected: %w", err)
	}
	if rows == 0 {
		return ClaimedJob{}, false, nil
	}

	job, err := getJobTx(ctx, tx, selected.ID)
	if err != nil {
		return ClaimedJob{}, false, err
	}
	leaseID, err := randomID("l")
	if err != nil {
		return ClaimedJob{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO leases (
	id,
	job_id,
	worker_id,
	capacity_bucket,
	leased_at,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		leaseID,
		job.ID,
		worker.ID,
		string(job.CapacityBucket),
		formatTime(now),
		formatTime(now.Add(leaseDuration)),
	); err != nil {
		return ClaimedJob{}, false, fmt.Errorf("create lease: %w", err)
	}
	lease, err := getLeaseTx(ctx, tx, leaseID)
	if err != nil {
		return ClaimedJob{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ClaimedJob{}, false, fmt.Errorf("commit claim transaction: %w", err)
	}

	return ClaimedJob{Job: job, Lease: lease}, true, nil
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
		return Job{}, errors.New("lease is already released")
	}

	now := s.now().UTC()
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

	job, err := getJobTx(ctx, tx, lease.JobID)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("commit release transaction: %w", err)
	}

	return job, nil
}

// CancelLiveJobsForIssue cancels every queued, claimed, or running job of the
// given role for the issue, releasing any live leases so the lease sweeper does
// not later mark the canceled jobs crashed. It returns the canceled job IDs.
// Workers running a canceled job observe the terminal state through their
// session reconciler and stop the job's session themselves.
func (s *Service) CancelLiveJobsForIssue(ctx context.Context, issueID string, role JobRole) ([]string, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, errors.New("issue id is required")
	}

	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("begin cancel transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM jobs
WHERE issue_id = ?
	AND role = ?
	AND state IN (?, ?, ?)
ORDER BY created_at, id`,
		issueID,
		string(role),
		string(JobQueued),
		string(JobClaimed),
		string(JobRunning),
	)
	if err != nil {
		return nil, fmt.Errorf("select live jobs for issue: %w", err)
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
	issue_id,
	change_id,
	role,
	state,
	capacity_bucket,
	priority,
	selector_json,
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
// grouped by capacity bucket. ClaimAcrossProjects sums it over every project
// to enforce the worker's global capacity.
func (s *Service) UsedCapacity(ctx context.Context, workerID string) (scheduler.Capacity, error) {
	return usedCapacity(ctx, s.db, strings.TrimSpace(workerID))
}

// OldestQueuedAt returns the creation time of the oldest queued job in the
// given buckets, or nil when no job is queued. ClaimAcrossProjects uses it to
// order project queues fairly.
func (s *Service) OldestQueuedAt(ctx context.Context, buckets []CapacityBucket) (*time.Time, error) {
	if len(buckets) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(buckets))
	args := make([]any, 0, len(buckets)+1)
	args = append(args, string(JobQueued))
	for i, bucket := range buckets {
		placeholders[i] = "?"
		args = append(args, string(bucket))
	}

	var createdAt sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT MIN(created_at)
FROM jobs
WHERE state = ?
	AND capacity_bucket IN (`+strings.Join(placeholders, ", ")+`)`, args...).Scan(&createdAt); err != nil {
		return nil, fmt.Errorf("read oldest queued job: %w", err)
	}
	if !createdAt.Valid || strings.TrimSpace(createdAt.String) == "" {
		return nil, nil
	}

	oldest, err := parseTime(createdAt.String)
	if err != nil {
		return nil, err
	}

	return &oldest, nil
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
ORDER BY priority DESC, created_at ASC, id ASC
`, args
}

func eligibleForWorker(job Job, worker Worker, used scheduler.Capacity) (bool, error) {
	selector, err := scheduler.NewSelector(job.Selector)
	if err != nil {
		return false, err
	}

	return scheduler.Eligible(scheduler.Job{
		Selector:       selector,
		Tolerations:    job.Tolerations,
		CapacityBucket: scheduler.CapacityBucket(job.CapacityBucket),
	}, scheduler.Worker{
		Labels: worker.Labels,
		Taints: worker.Taints,
		Capacity: scheduler.Capacity{
			PersistentAgent: worker.CapacityPersistentAgent,
			Ephemeral:       worker.CapacityEphemeral,
		},
		Used: used,
	})
}

func (w Worker) capacityFor(bucket CapacityBucket) int {
	switch bucket {
	case BucketPersistentAgent:
		return w.CapacityPersistentAgent
	case BucketEphemeral:
		return w.CapacityEphemeral
	default:
		return 0
	}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanWorker(row scanner) (Worker, error) {
	var worker Worker
	var labelsJSON string
	var taintsJSON string
	var harnessModelsJSON string
	var createdAt string
	var updatedAt string
	var lastHeartbeatAt sql.NullString
	var expiresAt sql.NullString
	if err := row.Scan(
		&worker.ID,
		&labelsJSON,
		&taintsJSON,
		&harnessModelsJSON,
		&worker.CapacityPersistentAgent,
		&worker.CapacityEphemeral,
		&worker.Status,
		&createdAt,
		&updatedAt,
		&lastHeartbeatAt,
		&expiresAt,
	); err != nil {
		return Worker{}, fmt.Errorf("scan worker: %w", err)
	}

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
	var issueID sql.NullString
	var changeID sql.NullString
	var role string
	var state string
	var bucket string
	var selectorJSON string
	var tolerationsJSON string
	var payloadJSON string
	var transcriptPath string
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&job.ID,
		&issueID,
		&changeID,
		&role,
		&state,
		&bucket,
		&job.Priority,
		&selectorJSON,
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

	job.IssueID = nullableStringPointer(issueID)
	job.ChangeID = nullableStringPointer(changeID)
	job.Role = JobRole(role)
	job.State = JobState(state)
	job.CapacityBucket = CapacityBucket(bucket)
	job.Selector = selector
	job.Tolerations = tolerations
	job.Payload = payload
	job.TranscriptPath = transcriptPath
	job.CreatedAt = parsedCreatedAt
	job.UpdatedAt = parsedUpdatedAt
	return job, nil
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
