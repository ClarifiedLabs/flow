package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

// ErrAssignmentConflict indicates that an assignment cannot perform the
// requested state transition. Callers may use errors.Is to map it to a
// non-retryable conflict response.
var ErrAssignmentConflict = errors.New("worker assignment conflict")

type AssignmentState string

const (
	AssignmentPending AssignmentState = "pending"
	AssignmentClaimed AssignmentState = "claimed"
	AssignmentClosed  AssignmentState = "closed"
)

// Assignment is the durable, project-local binding between one provisioned
// worker and one job. Profile fields are immutable scheduling snapshots.
type Assignment struct {
	ID                   string              `json:"id"`
	WorkerID             string              `json:"worker_id"`
	JobID                string              `json:"job_id"`
	ProviderID           string              `json:"provider_id"`
	ProfileName          string              `json:"profile_name"`
	ProviderRequestID    string              `json:"provider_request_id"`
	ProviderType         string              `json:"provider_type"`
	ProviderOptions      map[string]string   `json:"provider_options"`
	State                AssignmentState     `json:"state"`
	Role                 JobRole             `json:"role"`
	CapacityBucket       CapacityBucket      `json:"capacity_bucket"`
	ProfileLabels        map[string]string   `json:"profile_labels"`
	ProfileTaints        []scheduler.Taint   `json:"profile_taints"`
	ProfileHarnessModels []flowharness.Model `json:"profile_harness_models"`
	AllowedRoles         []JobRole           `json:"allowed_roles"`
	AllowedBuckets       []CapacityBucket    `json:"allowed_buckets"`
	RequiredSelector     map[string]string   `json:"required_selector"`
	StartupDeadline      time.Time           `json:"startup_deadline"`
	RetryCount           int                 `json:"retry_count"`
	NextRetryAt          *time.Time          `json:"next_retry_at,omitempty"`
	LastAttemptAt        *time.Time          `json:"last_attempt_at,omitempty"`
	CreatedAt            time.Time           `json:"created_at"`
	UpdatedAt            time.Time           `json:"updated_at"`
	ClosedAt             *time.Time          `json:"closed_at,omitempty"`
	CloseReason          *string             `json:"close_reason,omitempty"`
	LastProviderError    *string             `json:"last_provider_error,omitempty"`
	ClaimedLeaseID       *string             `json:"claimed_lease_id,omitempty"`
	CredentialsRevokedAt *time.Time          `json:"credentials_revoked_at,omitempty"`
	CleanedAt            *time.Time          `json:"cleaned_at,omitempty"`
}

// AssignmentCandidateFilter describes a virtual provisioner profile. Empty
// role or bucket allowlists allow all current values.
type AssignmentCandidateFilter struct {
	ProfileLabels        map[string]string
	ProfileTaints        []scheduler.Taint
	ProfileHarnessModels []flowharness.Model
	AllowedRoles         []JobRole
	AllowedBuckets       []CapacityBucket
	RequiredSelector     map[string]string
}

// ReserveAssignmentInput reserves a specified queued job. Role and bucket are
// read from the job so callers cannot persist a contradictory snapshot.
type ReserveAssignmentInput struct {
	ID                   string
	WorkerID             string
	JobID                string
	ProviderID           string
	ProfileName          string
	ProviderRequestID    string
	ProviderType         string
	ProviderOptions      map[string]string
	ProfileLabels        map[string]string
	ProfileTaints        []scheduler.Taint
	ProfileHarnessModels []flowharness.Model
	AllowedRoles         []JobRole
	AllowedBuckets       []CapacityBucket
	RequiredSelector     map[string]string
	StartupDeadline      time.Time
}

// AssignmentFilter supports provisioner recovery and API list operations.
type AssignmentFilter struct {
	ProviderID   string
	ProfileName  string
	WorkerID     string
	JobID        string
	States       []AssignmentState
	OpenOnly     bool
	NeedsCleanup bool
}

// ClaimAssignmentInput contains coordinator-global capacity state and the
// actual registered worker. The caller must serialize this with generic claims
// across project databases.
type ClaimAssignmentInput struct {
	AssignmentID  string
	Worker        Worker
	Used          scheduler.Capacity
	LeaseDuration time.Duration
}

// AbandonAssignmentInput records a definitive pre-claim provider failure.
type AbandonAssignmentInput struct {
	AssignmentID  string
	CloseReason   string
	ProviderError string
}

// AssignmentAttemptInput records a provider launch attempt and optional
// backoff. It is intentionally valid only while the assignment is pending.
type AssignmentAttemptInput struct {
	AssignmentID  string
	NextRetryAt   *time.Time
	ProviderError string
}

type assignmentProfile struct {
	labels   map[string]string
	taints   []scheduler.Taint
	models   []flowharness.Model
	roles    []JobRole
	buckets  []CapacityBucket
	required map[string]string
}

// FindAssignmentCandidate returns the first eligible, unassigned queued job in
// normal queue order (priority descending, then oldest first).
func (s *Service) FindAssignmentCandidate(ctx context.Context, filter AssignmentCandidateFilter) (Job, bool, error) {
	profile, err := normalizeAssignmentProfile(filter.ProfileLabels, filter.ProfileTaints, filter.ProfileHarnessModels, filter.AllowedRoles, filter.AllowedBuckets, filter.RequiredSelector)
	if err != nil {
		return Job{}, false, err
	}
	buckets := profile.buckets
	if len(buckets) == 0 {
		buckets = []CapacityBucket{BucketPersistentAgent, BucketEphemeral}
	}
	candidates, err := queuedJobCandidates(ctx, s.db, buckets)
	if err != nil {
		return Job{}, false, err
	}
	for _, job := range candidates {
		active, err := jobStillQueueable(ctx, s.db, job.ID)
		if err != nil {
			return Job{}, false, err
		}
		if !active {
			continue
		}
		eligible, err := assignmentProfileEligible(job, profile)
		if err != nil {
			return Job{}, false, err
		}
		if eligible {
			return job, true, nil
		}
	}
	return Job{}, false, nil
}

// ReserveAssignment atomically reserves a specified still-eligible queued job
// without changing its state or queue timestamps. Repeating a provider request
// returns the original row.
func (s *Service) ReserveAssignment(ctx context.Context, input ReserveAssignmentInput) (Assignment, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.ProfileName = strings.TrimSpace(input.ProfileName)
	input.ProviderRequestID = strings.TrimSpace(input.ProviderRequestID)
	input.ProviderType = strings.TrimSpace(input.ProviderType)
	if input.ProviderOptions == nil {
		input.ProviderOptions = map[string]string{}
	}
	if input.JobID == "" || input.ProviderID == "" || input.ProfileName == "" || input.ProviderRequestID == "" || input.ProviderType == "" {
		return Assignment{}, errors.New("job, provider, profile, provider request, and provider type are required")
	}
	if input.StartupDeadline.IsZero() {
		return Assignment{}, errors.New("startup deadline is required")
	}
	profile, err := normalizeAssignmentProfile(input.ProfileLabels, input.ProfileTaints, input.ProfileHarnessModels, input.AllowedRoles, input.AllowedBuckets, input.RequiredSelector)
	if err != nil {
		return Assignment{}, err
	}
	if strings.TrimSpace(input.ID) == "" {
		input.ID, err = randomID("a")
		if err != nil {
			return Assignment{}, err
		}
	}
	if strings.TrimSpace(input.WorkerID) == "" {
		input.WorkerID, err = randomID("w-prov")
		if err != nil {
			return Assignment{}, err
		}
	}

	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return Assignment{}, fmt.Errorf("begin reserve assignment transaction: %w", err)
	}
	defer tx.Rollback()

	existing, err := findAssignmentByRequestTx(ctx, tx, input.ProviderID, input.ProviderRequestID)
	if err == nil {
		if existing.JobID != input.JobID || existing.ProfileName != input.ProfileName || existing.ProviderType != input.ProviderType || !reflect.DeepEqual(existing.ProviderOptions, input.ProviderOptions) {
			return Assignment{}, assignmentConflict("provider request is already bound to another job, profile, or provider descriptor")
		}
		if err := tx.Commit(ctx); err != nil {
			return Assignment{}, fmt.Errorf("commit repeated assignment reservation: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, err
	}

	job, err := getJobTx(ctx, tx, input.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Assignment{}, assignmentConflict("reserved job does not exist")
		}
		return Assignment{}, err
	}
	active, err := jobStillQueueable(ctx, tx, job.ID)
	if err != nil {
		return Assignment{}, err
	}
	if !active {
		return Assignment{}, assignmentConflict("reserved job is not an active queued job")
	}
	eligible, err := assignmentProfileEligible(job, profile)
	if err != nil {
		return Assignment{}, err
	}
	if !eligible {
		return Assignment{}, assignmentConflict("reserved job is not eligible for the profile")
	}
	if !input.StartupDeadline.UTC().After(s.now().UTC()) {
		return Assignment{}, assignmentConflict("startup deadline has elapsed")
	}

	labelsJSON, _ := encodeStringMap(profile.labels)
	taintsJSON, _ := encodeTaints(profile.taints)
	modelsJSON, _ := encodeHarnessModels(profile.models)
	rolesJSON, _ := encodeJobRoles(profile.roles)
	bucketsJSON, _ := encodeCapacityBuckets(profile.buckets)
	requiredJSON, _ := encodeStringMap(profile.required)
	providerOptionsJSON, err := encodeStringMap(input.ProviderOptions)
	if err != nil {
		return Assignment{}, err
	}
	now := s.now().UTC()
	_, err = tx.ExecContext(ctx, `
INSERT INTO worker_assignments (
	id, worker_id, job_id, provider_id, profile_name, provider_request_id,
	provider_type, provider_options_json, state, role, capacity_bucket, profile_labels_json, profile_taints_json,
	profile_harness_models_json, allowed_roles_json, allowed_buckets_json,
	required_selector_json, startup_deadline, retry_count, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		strings.TrimSpace(input.ID), strings.TrimSpace(input.WorkerID), job.ID,
		input.ProviderID, input.ProfileName, input.ProviderRequestID, input.ProviderType, providerOptionsJSON,
		string(job.Role), string(job.CapacityBucket), labelsJSON, taintsJSON,
		modelsJSON, rolesJSON, bucketsJSON, requiredJSON,
		formatTime(input.StartupDeadline.UTC()), formatTime(now), formatTime(now))
	if err != nil {
		return Assignment{}, assignmentConflict("job, worker, or provider request is already assigned")
	}
	assignment, err := getAssignmentTx(ctx, tx, input.ID)
	if err != nil {
		return Assignment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Assignment{}, fmt.Errorf("commit assignment reservation: %w", err)
	}
	return assignment, nil
}

func (s *Service) GetAssignment(ctx context.Context, assignmentID string) (Assignment, error) {
	return getAssignmentTx(ctx, s.db, strings.TrimSpace(assignmentID))
}

func (s *Service) FindAssignmentByWorker(ctx context.Context, workerID string) (Assignment, bool, error) {
	assignment, err := findAssignmentByWorkerTx(ctx, s.db, strings.TrimSpace(workerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, false, nil
	}
	return assignment, err == nil, err
}

func (s *Service) FindAssignmentByRequest(ctx context.Context, providerID, providerRequestID string) (Assignment, bool, error) {
	assignment, err := findAssignmentByRequestTx(ctx, s.db, strings.TrimSpace(providerID), strings.TrimSpace(providerRequestID))
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, false, nil
	}
	return assignment, err == nil, err
}

func (s *Service) ListAssignments(ctx context.Context, filter AssignmentFilter) ([]Assignment, error) {
	var where []string
	var args []any
	add := func(clause string, arg any) { where = append(where, clause); args = append(args, arg) }
	if value := strings.TrimSpace(filter.ProviderID); value != "" {
		add("provider_id = ?", value)
	}
	if value := strings.TrimSpace(filter.ProfileName); value != "" {
		add("profile_name = ?", value)
	}
	if value := strings.TrimSpace(filter.WorkerID); value != "" {
		add("worker_id = ?", value)
	}
	if value := strings.TrimSpace(filter.JobID); value != "" {
		add("job_id = ?", value)
	}
	if filter.OpenOnly {
		where = append(where, "state IN ('pending', 'claimed')")
	}
	if filter.NeedsCleanup {
		where = append(where, "state = 'closed' AND cleaned_at IS NULL")
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			if err := validateAssignmentState(state); err != nil {
				return nil, err
			}
			placeholders[i] = "?"
			args = append(args, string(state))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	query := assignmentSelectSQL
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list worker assignments: %w", err)
	}
	defer rows.Close()
	var assignments []Assignment
	for rows.Next() {
		assignment, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker assignments: %w", err)
	}
	return assignments, nil
}

// CountOpenAssignments counts pending and claimed assignments for one provider
// profile. Cross-project concurrency is enforced by summing this result while
// holding the coordinator claim mutex.
func (s *Service) CountOpenAssignments(ctx context.Context, providerID, profileName string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM worker_assignments
WHERE provider_id = ? AND profile_name = ? AND state IN ('pending', 'claimed')`,
		strings.TrimSpace(providerID), strings.TrimSpace(profileName)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count open worker assignments: %w", err)
	}
	return count, nil
}

// ExpirePendingAssignments closes elapsed startup reservations without touching
// jobs, their queue timestamps, or retry counters.
func (s *Service) ExpirePendingAssignments(ctx context.Context) (int, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_assignments
SET state = 'closed', updated_at = ?, closed_at = ?, close_reason = 'startup_timeout'
WHERE state = 'pending' AND startup_deadline <= ?`, formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("expire pending worker assignments: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read expired worker assignment count: %w", err)
	}
	return int(rows), nil
}

// AbandonAssignment closes only a pending assignment; a claimed assignment is
// owned by its ordinary lease lifecycle.
func (s *Service) AbandonAssignment(ctx context.Context, input AbandonAssignmentInput) (Assignment, error) {
	reason := strings.TrimSpace(input.CloseReason)
	if reason == "" {
		reason = "provider_failed"
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_assignments
SET state = 'closed', updated_at = ?, closed_at = ?, close_reason = ?, last_provider_error = ?, credentials_revoked_at = ?
WHERE id = ? AND state = 'pending'`, formatTime(now), formatTime(now), reason,
		nullableTrimmedString(input.ProviderError), formatTime(now), strings.TrimSpace(input.AssignmentID))
	if err != nil {
		return Assignment{}, fmt.Errorf("abandon worker assignment: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Assignment{}, fmt.Errorf("read abandoned worker assignment count: %w", err)
	}
	if rows == 0 {
		return Assignment{}, assignmentConflict("assignment is not pending")
	}
	return s.GetAssignment(ctx, input.AssignmentID)
}

func (s *Service) RecordAssignmentAttempt(ctx context.Context, input AssignmentAttemptInput) (Assignment, error) {
	now := s.now().UTC()
	var next any
	if input.NextRetryAt != nil {
		next = formatTime(input.NextRetryAt.UTC())
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_assignments
SET retry_count = retry_count + 1, last_attempt_at = ?, next_retry_at = ?,
	last_provider_error = ?, updated_at = ?
WHERE id = ? AND state = 'pending'`, formatTime(now), next,
		nullableTrimmedString(input.ProviderError), formatTime(now), strings.TrimSpace(input.AssignmentID))
	if err != nil {
		return Assignment{}, fmt.Errorf("record worker assignment attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Assignment{}, fmt.Errorf("read worker assignment attempt count: %w", err)
	}
	if rows == 0 {
		return Assignment{}, assignmentConflict("assignment is not pending")
	}
	return s.GetAssignment(ctx, input.AssignmentID)
}

// MarkAssignmentCredentialsRevoked durably records that direct credentials are
// fenced before provider resource deletion. Repeated calls preserve the first timestamp.
func (s *Service) MarkAssignmentCredentialsRevoked(ctx context.Context, assignmentID string) (Assignment, error) {
	assignmentID = strings.TrimSpace(assignmentID)
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_assignments SET credentials_revoked_at = ?, updated_at = ?
WHERE id = ? AND state = 'closed' AND credentials_revoked_at IS NULL`, formatTime(now), formatTime(now), assignmentID)
	if err != nil {
		return Assignment{}, fmt.Errorf("mark worker assignment credentials revoked: %w", err)
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil {
		return Assignment{}, fmt.Errorf("read revoked worker assignment count: %w", rowErr)
	} else if rows == 0 {
		assignment, getErr := s.GetAssignment(ctx, assignmentID)
		if getErr != nil {
			return Assignment{}, getErr
		}
		if assignment.State != AssignmentClosed {
			return Assignment{}, assignmentConflict("open assignment credentials cannot be marked revoked")
		}
		return assignment, nil
	}
	return s.GetAssignment(ctx, assignmentID)
}

// MarkAssignmentCleaned records provider cleanup exactly once. Repeated calls
// return the same timestamp.
func (s *Service) MarkAssignmentCleaned(ctx context.Context, assignmentID string) (Assignment, error) {
	assignmentID = strings.TrimSpace(assignmentID)
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_assignments SET cleaned_at = ?, updated_at = ?
WHERE id = ? AND state = 'closed' AND credentials_revoked_at IS NOT NULL AND cleaned_at IS NULL`, formatTime(now), formatTime(now), assignmentID)
	if err != nil {
		return Assignment{}, fmt.Errorf("mark worker assignment cleaned: %w", err)
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil {
		return Assignment{}, fmt.Errorf("read cleaned worker assignment count: %w", rowErr)
	} else if rows == 0 {
		assignment, getErr := s.GetAssignment(ctx, assignmentID)
		if getErr != nil {
			return Assignment{}, getErr
		}
		if assignment.State != AssignmentClosed {
			return Assignment{}, assignmentConflict("open assignment cannot be cleaned")
		}
		if assignment.CredentialsRevokedAt == nil {
			return Assignment{}, assignmentConflict("assignment credentials must be revoked before cleanup")
		}
		return assignment, nil
	}
	return s.GetAssignment(ctx, assignmentID)
}

// CloseAssignment closes assignment history only after its job is terminal.
// Normal release/cancellation/sweep transactions call the same helper directly.
func (s *Service) CloseAssignment(ctx context.Context, assignmentID string) (Assignment, error) {
	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return Assignment{}, fmt.Errorf("begin close assignment transaction: %w", err)
	}
	defer tx.Rollback()
	assignment, err := getAssignmentTx(ctx, tx, strings.TrimSpace(assignmentID))
	if err != nil {
		return Assignment{}, err
	}
	if assignment.State == AssignmentClosed {
		if err := tx.Commit(ctx); err != nil {
			return Assignment{}, err
		}
		return assignment, nil
	}
	job, err := getJobTx(ctx, tx, assignment.JobID)
	if err != nil {
		return Assignment{}, err
	}
	if !IsTerminalJobState(job.State) {
		return Assignment{}, assignmentConflict("assignment job is not terminal")
	}
	now := s.now().UTC()
	if err := closeAssignmentsForJobTx(ctx, tx, job.ID, string(job.State), now); err != nil {
		return Assignment{}, err
	}
	assignment, err = getAssignmentTx(ctx, tx, assignment.ID)
	if err != nil {
		return Assignment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Assignment{}, fmt.Errorf("commit close assignment transaction: %w", err)
	}
	return assignment, nil
}

// ClaimAssignment claims exactly the reserved job. It never falls through to a
// generic queue candidate.
func (s *Service) ClaimAssignment(ctx context.Context, input ClaimAssignmentInput) (ClaimedJob, error) {
	input.AssignmentID = strings.TrimSpace(input.AssignmentID)
	input.Worker.ID = strings.TrimSpace(input.Worker.ID)
	if input.AssignmentID == "" || input.Worker.ID == "" {
		return ClaimedJob{}, errors.New("assignment and worker ids are required")
	}
	if input.LeaseDuration <= 0 {
		return ClaimedJob{}, errors.New("lease duration must be positive")
	}

	tx, err := beginImmediate(ctx, s.db)
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("begin assignment claim transaction: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC()
	assignment, err := getAssignmentTx(ctx, tx, input.AssignmentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ClaimedJob{}, assignmentConflict("assignment does not exist")
		}
		return ClaimedJob{}, err
	}
	if assignment.WorkerID != input.Worker.ID {
		return ClaimedJob{}, assignmentConflict("assignment belongs to another worker")
	}
	if assignment.State == AssignmentClosed {
		return ClaimedJob{}, assignmentConflict("assignment is closed")
	}
	if assignment.State == AssignmentClaimed {
		claimed, err := claimedAssignmentResult(ctx, tx, assignment, now)
		if err != nil {
			return ClaimedJob{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ClaimedJob{}, fmt.Errorf("commit repeated assignment claim: %w", err)
		}
		return claimed, nil
	}
	if assignment.State != AssignmentPending || !assignment.StartupDeadline.After(now) {
		return ClaimedJob{}, assignmentConflict("assignment is not claimable")
	}
	job, err := getJobTx(ctx, tx, assignment.JobID)
	if err != nil {
		return ClaimedJob{}, assignmentConflict("assigned job does not exist")
	}
	active, err := jobStillQueueable(ctx, tx, job.ID)
	if err != nil {
		return ClaimedJob{}, err
	}
	if !active {
		return ClaimedJob{}, assignmentConflict("assigned job is no longer queued or was canceled")
	}
	if job.Role != assignment.Role || job.CapacityBucket != assignment.CapacityBucket {
		return ClaimedJob{}, assignmentConflict("assigned job no longer matches its snapshot")
	}
	profile := assignmentProfile{assignment.ProfileLabels, assignment.ProfileTaints, assignment.ProfileHarnessModels, assignment.AllowedRoles, assignment.AllowedBuckets, assignment.RequiredSelector}
	eligible, err := assignmentProfileEligible(job, profile)
	if err != nil {
		return ClaimedJob{}, err
	}
	if !eligible {
		return ClaimedJob{}, assignmentConflict("stored profile is not eligible for the assigned job")
	}
	matches, err := workerSatisfiesProfileSnapshot(input.Worker, assignment)
	if err != nil {
		return ClaimedJob{}, err
	}
	if !matches {
		return ClaimedJob{}, assignmentConflict("registered worker does not satisfy the profile snapshot")
	}
	localUsed, err := usedCapacity(ctx, tx, input.Worker.ID)
	if err != nil {
		return ClaimedJob{}, err
	}
	if localUsed.PersistentAgent+localUsed.Ephemeral > 0 || input.Used.PersistentAgent+input.Used.Ephemeral > 0 {
		return ClaimedJob{}, assignmentConflict("worker already has a live lease")
	}
	eligible, err = eligibleForWorker(job, input.Worker, input.Used)
	if err != nil {
		return ClaimedJob{}, err
	}
	if !eligible {
		return ClaimedJob{}, assignmentConflict("registered worker is not eligible for the assigned job")
	}

	leaseID, err := randomID("l")
	if err != nil {
		return ClaimedJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?)`, leaseID, job.ID, input.Worker.ID, string(job.CapacityBucket),
		formatTime(now), formatTime(now.Add(input.LeaseDuration))); err != nil {
		return ClaimedJob{}, fmt.Errorf("create assignment lease: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		string(JobClaimed), formatTime(now), job.ID, string(JobQueued))
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("claim assigned job: %w", err)
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil || rows != 1 {
		if rowErr != nil {
			return ClaimedJob{}, rowErr
		}
		return ClaimedJob{}, assignmentConflict("assigned job was claimed concurrently")
	}
	result, err = tx.ExecContext(ctx, `
UPDATE worker_assignments
SET state = 'claimed', claimed_lease_id = ?, updated_at = ?
WHERE id = ? AND state = 'pending'`, leaseID, formatTime(now), assignment.ID)
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("mark assignment claimed: %w", err)
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil || rows != 1 {
		if rowErr != nil {
			return ClaimedJob{}, rowErr
		}
		return ClaimedJob{}, assignmentConflict("assignment was claimed concurrently")
	}
	job, err = getJobTx(ctx, tx, job.ID)
	if err != nil {
		return ClaimedJob{}, err
	}
	lease, err := getLeaseTx(ctx, tx, leaseID)
	if err != nil {
		return ClaimedJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimedJob{}, fmt.Errorf("commit assignment claim: %w", err)
	}
	return ClaimedJob{Job: job, Lease: lease}, nil
}

func claimedAssignmentResult(ctx context.Context, tx txExecutor, assignment Assignment, now time.Time) (ClaimedJob, error) {
	if assignment.ClaimedLeaseID == nil {
		return ClaimedJob{}, assignmentConflict("claimed assignment has no lease")
	}
	lease, err := getLeaseTx(ctx, tx, *assignment.ClaimedLeaseID)
	if err != nil {
		return ClaimedJob{}, assignmentConflict("claimed assignment lease does not exist")
	}
	if lease.WorkerID != assignment.WorkerID || lease.JobID != assignment.JobID || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return ClaimedJob{}, assignmentConflict("claimed assignment lease is not live")
	}
	job, err := getJobTx(ctx, tx, assignment.JobID)
	if err != nil {
		return ClaimedJob{}, assignmentConflict("claimed assignment job does not exist")
	}
	if job.State != JobClaimed && job.State != JobRunning {
		return ClaimedJob{}, assignmentConflict("claimed assignment job is not live")
	}
	return ClaimedJob{Job: job, Lease: lease}, nil
}

func normalizeAssignmentProfile(labels map[string]string, taints []scheduler.Taint, models []flowharness.Model, roles []JobRole, buckets []CapacityBucket, required map[string]string) (assignmentProfile, error) {
	labelsJSON, err := normalizeLabelsJSON(labels)
	if err != nil {
		return assignmentProfile{}, err
	}
	normalizedLabels, _ := decodeStringMap(labelsJSON)
	taintsJSON, err := encodeTaints(taints)
	if err != nil {
		return assignmentProfile{}, err
	}
	normalizedTaints, _ := decodeTaints(taintsJSON)
	modelsJSON, err := encodeHarnessModels(models)
	if err != nil {
		return assignmentProfile{}, err
	}
	normalizedModels, _ := decodeHarnessModels(modelsJSON)
	normalizedRoles, err := normalizeJobRoles(roles)
	if err != nil {
		return assignmentProfile{}, err
	}
	normalizedBuckets, err := normalizeCapacityBuckets(buckets)
	if err != nil {
		return assignmentProfile{}, err
	}
	selector, err := scheduler.NewSelector(required)
	if err != nil {
		return assignmentProfile{}, err
	}
	return assignmentProfile{normalizedLabels, normalizedTaints, normalizedModels, normalizedRoles, normalizedBuckets, selector.Requirements()}, nil
}

func assignmentProfileEligible(job Job, profile assignmentProfile) (bool, error) {
	if !jobRoleAllowed(job.Role, profile.roles) || !capacityBucketAllowed(job.CapacityBucket, profile.buckets) || !selectorContains(job.Selector, profile.required) {
		return false, nil
	}
	worker := Worker{Labels: profile.labels, Taints: profile.taints}
	if capacityBucketAllowed(BucketPersistentAgent, profile.buckets) || len(profile.buckets) == 0 {
		worker.CapacityPersistentAgent = 1
	}
	if capacityBucketAllowed(BucketEphemeral, profile.buckets) || len(profile.buckets) == 0 {
		worker.CapacityEphemeral = 1
	}
	return eligibleForWorker(job, worker, scheduler.Capacity{})
}

func workerSatisfiesProfileSnapshot(worker Worker, assignment Assignment) (bool, error) {
	labels, err := scheduler.NormalizeLabels(worker.Labels)
	if err != nil {
		return false, err
	}
	for key, value := range assignment.ProfileLabels {
		if labels[key] != value {
			return false, nil
		}
	}
	actualTaints := make([]scheduler.Taint, 0, len(worker.Taints))
	for _, taint := range worker.Taints {
		normalized, err := scheduler.NormalizeTaint(taint)
		if err != nil {
			return false, err
		}
		actualTaints = append(actualTaints, normalized)
	}
	for _, expected := range assignment.ProfileTaints {
		found := false
		for _, actual := range actualTaints {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	actualModelsJSON, err := encodeHarnessModels(worker.HarnessModels)
	if err != nil {
		return false, err
	}
	actualModels, _ := decodeHarnessModels(actualModelsJSON)
	for _, expected := range assignment.ProfileHarnessModels {
		found := false
		for _, actual := range actualModels {
			if reflect.DeepEqual(actual, expected) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func jobStillQueueable(ctx context.Context, tx txExecutor, jobID string) (bool, error) {
	var active bool
	err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1 FROM jobs AS j
	WHERE j.id = ? AND j.state = 'queued'
		AND (j.task_id IS NULL OR EXISTS (SELECT 1 FROM tasks AS t WHERE t.id = j.task_id AND t.done_at IS NULL))
		AND (j.workflow_run_id IS NULL OR EXISTS (
			SELECT 1 FROM workflow_runs AS wr WHERE wr.id = j.workflow_run_id AND wr.task_id = j.task_id
				AND wr.state IN ('scheduled', 'running', 'waiting')))
		AND (j.node_run_id IS NULL OR EXISTS (
			SELECT 1 FROM workflow_node_runs AS nr JOIN workflow_runs AS wr ON wr.id = nr.workflow_run_id
			WHERE nr.id = j.node_run_id AND nr.workflow_run_id = j.workflow_run_id
				AND nr.state IN ('queued', 'running', 'waiting') AND wr.current_node_run_id = nr.id
				AND wr.state IN ('scheduled', 'running', 'waiting')))
		AND (json_type(j.payload_json, '$.convergence_evidence_fingerprint') IS NULL OR EXISTS (
			SELECT 1 FROM workflow_runs AS held_wr
			JOIN workflow_transitions AS wt ON wt.workflow_run_id = held_wr.id
			JOIN changes AS c ON c.id = j.change_id
			WHERE held_wr.id = j.workflow_run_id AND held_wr.task_id = j.task_id AND held_wr.held_at IS NOT NULL
				AND held_wr.state IN ('scheduled', 'running', 'waiting')
				AND wt.event_kind = 'workflow_convergence_review_requested'
				AND wt.seq = (SELECT MAX(latest.seq) FROM workflow_transitions AS latest
					WHERE latest.workflow_run_id = held_wr.id AND latest.event_kind = 'workflow_convergence_review_requested')
				AND json_extract(wt.payload_json, '$.fingerprint') = json_extract(j.payload_json, '$.convergence_evidence_fingerprint')
				AND json_extract(wt.payload_json, '$.change_id') = j.change_id AND c.task_id = held_wr.task_id
				AND c.workflow_run_id = held_wr.id AND c.branch = json_extract(wt.payload_json, '$.source_branch')
				AND c.base = json_extract(wt.payload_json, '$.target_base_branch')
				AND c.head_sha = json_extract(wt.payload_json, '$.source_head_sha')
				AND json_extract(j.payload_json, '$.convergence_workflow_run_id') = held_wr.id
				AND json_extract(j.payload_json, '$.convergence_source_head_sha') = c.head_sha))
)`, jobID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check queued job cancellation state: %w", err)
	}
	return active, nil
}

func closeAssignmentsForJobTx(ctx context.Context, tx txExecutor, jobID, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE worker_assignments
SET state = 'closed', updated_at = ?, closed_at = ?, close_reason = ?
WHERE job_id = ? AND state IN ('pending', 'claimed')`, formatTime(now), formatTime(now), strings.TrimSpace(reason), jobID)
	if err != nil {
		return fmt.Errorf("close worker assignment for job: %w", err)
	}
	return nil
}

func closeTerminalAssignmentsTx(ctx context.Context, tx txExecutor, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE worker_assignments
SET state = 'closed', updated_at = ?, closed_at = ?,
	close_reason = (SELECT jobs.state FROM jobs WHERE jobs.id = worker_assignments.job_id)
WHERE state IN ('pending', 'claimed') AND EXISTS (
	SELECT 1 FROM jobs WHERE jobs.id = worker_assignments.job_id
		AND jobs.state IN ('finished', 'failed', 'crashed', 'canceled'))`, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("close terminal worker assignments: %w", err)
	}
	return nil
}

const assignmentSelectSQL = `
SELECT id, worker_id, job_id, provider_id, profile_name, provider_request_id,
	provider_type, provider_options_json, state, role, capacity_bucket, profile_labels_json, profile_taints_json,
	profile_harness_models_json, allowed_roles_json, allowed_buckets_json,
	required_selector_json, startup_deadline, retry_count, next_retry_at,
	last_attempt_at, created_at, updated_at, closed_at, close_reason,
	last_provider_error, claimed_lease_id, credentials_revoked_at, cleaned_at
FROM worker_assignments`

func getAssignmentTx(ctx context.Context, tx txExecutor, assignmentID string) (Assignment, error) {
	return scanAssignment(tx.QueryRowContext(ctx, assignmentSelectSQL+"\nWHERE id = ?", assignmentID))
}

func findAssignmentByWorkerTx(ctx context.Context, tx txExecutor, workerID string) (Assignment, error) {
	return scanAssignment(tx.QueryRowContext(ctx, assignmentSelectSQL+"\nWHERE worker_id = ?", workerID))
}

func findAssignmentByRequestTx(ctx context.Context, tx txExecutor, providerID, requestID string) (Assignment, error) {
	return scanAssignment(tx.QueryRowContext(ctx, assignmentSelectSQL+"\nWHERE provider_id = ? AND provider_request_id = ?", providerID, requestID))
}

func scanAssignment(row scanner) (Assignment, error) {
	var assignment Assignment
	var state, role, bucket string
	var providerOptionsJSON, labelsJSON, taintsJSON, modelsJSON, rolesJSON, bucketsJSON, requiredJSON string
	var startupDeadline, createdAt, updatedAt string
	var nextRetryAt, lastAttemptAt, closedAt, credentialsRevokedAt, cleanedAt sql.NullString
	var closeReason, lastProviderError, claimedLeaseID sql.NullString
	if err := row.Scan(&assignment.ID, &assignment.WorkerID, &assignment.JobID, &assignment.ProviderID,
		&assignment.ProfileName, &assignment.ProviderRequestID, &assignment.ProviderType, &providerOptionsJSON, &state, &role, &bucket,
		&labelsJSON, &taintsJSON, &modelsJSON, &rolesJSON, &bucketsJSON, &requiredJSON,
		&startupDeadline, &assignment.RetryCount, &nextRetryAt, &lastAttemptAt, &createdAt,
		&updatedAt, &closedAt, &closeReason, &lastProviderError, &claimedLeaseID, &credentialsRevokedAt, &cleanedAt); err != nil {
		return Assignment{}, fmt.Errorf("scan worker assignment: %w", err)
	}
	var err error
	assignment.State, assignment.Role, assignment.CapacityBucket = AssignmentState(state), JobRole(role), CapacityBucket(bucket)
	if assignment.ProviderOptions, err = decodeStringMap(providerOptionsJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.ProfileLabels, err = decodeStringMap(labelsJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.ProfileTaints, err = decodeTaints(taintsJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.ProfileHarnessModels, err = decodeHarnessModels(modelsJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.AllowedRoles, err = decodeJobRoles(rolesJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.AllowedBuckets, err = decodeCapacityBuckets(bucketsJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.RequiredSelector, err = decodeStringMap(requiredJSON); err != nil {
		return Assignment{}, err
	}
	if assignment.StartupDeadline, err = parseTime(startupDeadline); err != nil {
		return Assignment{}, err
	}
	if assignment.CreatedAt, err = parseTime(createdAt); err != nil {
		return Assignment{}, err
	}
	if assignment.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Assignment{}, err
	}
	if assignment.NextRetryAt, err = nullableParsedTime(nextRetryAt); err != nil {
		return Assignment{}, err
	}
	if assignment.LastAttemptAt, err = nullableParsedTime(lastAttemptAt); err != nil {
		return Assignment{}, err
	}
	if assignment.ClosedAt, err = nullableParsedTime(closedAt); err != nil {
		return Assignment{}, err
	}
	if assignment.CredentialsRevokedAt, err = nullableParsedTime(credentialsRevokedAt); err != nil {
		return Assignment{}, err
	}
	if assignment.CleanedAt, err = nullableParsedTime(cleanedAt); err != nil {
		return Assignment{}, err
	}
	assignment.CloseReason = nullableStringPointer(closeReason)
	assignment.LastProviderError = nullableStringPointer(lastProviderError)
	assignment.ClaimedLeaseID = nullableStringPointer(claimedLeaseID)
	return assignment, nil
}

func normalizeJobRoles(values []JobRole) ([]JobRole, error) {
	seen := map[JobRole]bool{}
	result := make([]JobRole, 0, len(values))
	for _, value := range values {
		if err := validateJobRole(value); err != nil {
			return nil, err
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, nil
}

func normalizeCapacityBuckets(values []CapacityBucket) ([]CapacityBucket, error) {
	seen := map[CapacityBucket]bool{}
	result := make([]CapacityBucket, 0, len(values))
	for _, value := range values {
		if err := validateCapacityBucket(value); err != nil {
			return nil, err
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, nil
}

func encodeJobRoles(values []JobRole) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode job roles: %w", err)
	}
	return string(encoded), nil
}
func decodeJobRoles(value string) ([]JobRole, error) {
	var decoded []JobRole
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode job roles: %w", err)
	}
	return normalizeJobRoles(decoded)
}
func encodeCapacityBuckets(values []CapacityBucket) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode capacity buckets: %w", err)
	}
	return string(encoded), nil
}
func decodeCapacityBuckets(value string) ([]CapacityBucket, error) {
	var decoded []CapacityBucket
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode capacity buckets: %w", err)
	}
	return normalizeCapacityBuckets(decoded)
}
func validateAssignmentState(state AssignmentState) error {
	switch state {
	case AssignmentPending, AssignmentClaimed, AssignmentClosed:
		return nil
	default:
		return fmt.Errorf("invalid assignment state: %s", state)
	}
}
func jobRoleAllowed(value JobRole, allowed []JobRole) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, item := range allowed {
		if item == value {
			return true
		}
	}
	return false
}
func capacityBucketAllowed(value CapacityBucket, allowed []CapacityBucket) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, item := range allowed {
		if item == value {
			return true
		}
	}
	return false
}
func selectorContains(selector, required map[string]string) bool {
	for key, value := range required {
		if selector[key] != value {
			return false
		}
	}
	return true
}
func nullableTrimmedString(value string) any {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return nil
}
func assignmentConflict(message string) error {
	return fmt.Errorf("%w: %s", ErrAssignmentConflict, message)
}
