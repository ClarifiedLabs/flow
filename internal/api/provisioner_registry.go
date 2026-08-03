package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

type provisionerAssignmentRecord struct {
	Project    coordinator.Project
	Assignment worker.Assignment
	bundle     *ProjectBundle
}

type reserveProvisionerAssignmentInput struct {
	ProviderID        string
	ProviderRequestID string
	ProfileName       string
	ProviderType      string
	ProviderOptions   map[string]string
	MaxConcurrency    int
	Candidate         worker.AssignmentCandidateFilter
	StartupTimeout    time.Duration
}

type listProvisionerAssignmentsInput struct {
	ProjectID         string
	ProviderRequestID string
	ProviderIDs       []string
	Filter            worker.AssignmentFilter
}

// ReserveProvisionerAssignment selects and reserves an exact candidate while
// holding the same cross-project mutex used by generic worker claims.
func (r *Registry) ReserveProvisionerAssignment(ctx context.Context, input reserveProvisionerAssignmentInput) (provisionerAssignmentRecord, string, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()

	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.ProviderRequestID = strings.TrimSpace(input.ProviderRequestID)
	input.ProfileName = strings.TrimSpace(input.ProfileName)
	input.ProviderType = strings.TrimSpace(input.ProviderType)
	if input.ProviderOptions == nil {
		input.ProviderOptions = map[string]string{}
	}
	if input.ProviderID == "" || input.ProviderRequestID == "" || input.ProfileName == "" || input.ProviderType == "" {
		return provisionerAssignmentRecord{}, "", false, errors.New("provider_id, provider_request_id, profile_name, and provider_type are required")
	}
	if input.MaxConcurrency <= 0 {
		return provisionerAssignmentRecord{}, "", false, errors.New("max_concurrency must be positive")
	}
	if input.StartupTimeout <= 0 {
		return provisionerAssignmentRecord{}, "", false, errors.New("startup_timeout_seconds must be positive")
	}

	existing, found, err := r.findAssignmentByRequestLocked(ctx, input.ProviderID, input.ProviderRequestID)
	if err != nil {
		return provisionerAssignmentRecord{}, "", false, err
	}
	if found {
		if existing.Assignment.ProfileName != input.ProfileName || existing.Assignment.ProviderType != input.ProviderType || !reflect.DeepEqual(existing.Assignment.ProviderOptions, input.ProviderOptions) {
			return provisionerAssignmentRecord{}, "", false, fmt.Errorf("%w: provider request is already bound to another profile or provider descriptor", worker.ErrAssignmentConflict)
		}
		if existing.Assignment.State == worker.AssignmentClosed {
			return provisionerAssignmentRecord{}, "", false, fmt.Errorf("%w: provider request belongs to a closed assignment", worker.ErrAssignmentConflict)
		}
		token, err := r.credentials.ReplaceSubjectToken(ctx, coordinator.CredentialInput{Scope: coordinator.TokenScopeWorker, Subject: existing.Assignment.WorkerID})
		return existing, token, err == nil, err
	}

	open := 0
	for _, bundle := range r.All() {
		count, err := bundle.Queue.CountOpenAssignments(ctx, input.ProviderID, input.ProfileName)
		if err != nil {
			return provisionerAssignmentRecord{}, "", false, fmt.Errorf("count assignments in project %s: %w", bundle.Project.ID, err)
		}
		open += count
	}
	if open >= input.MaxConcurrency {
		return provisionerAssignmentRecord{}, "", false, nil
	}

	var selected *provisionerAssignmentRecord
	var selectedJob worker.Job
	for _, bundle := range r.All() {
		job, ok, err := bundle.Queue.FindAssignmentCandidate(ctx, input.Candidate)
		if err != nil {
			return provisionerAssignmentRecord{}, "", false, fmt.Errorf("find assignment candidate in project %s: %w", bundle.Project.ID, err)
		}
		if !ok {
			continue
		}
		record := provisionerAssignmentRecord{Project: bundle.Project, bundle: bundle}
		if selected == nil || job.Priority > selectedJob.Priority ||
			(job.Priority == selectedJob.Priority && (job.CreatedAt.Before(selectedJob.CreatedAt) ||
				(job.CreatedAt.Equal(selectedJob.CreatedAt) && bundle.Project.ID < selected.Project.ID))) {
			selected = &record
			selectedJob = job
		}
	}
	if selected == nil {
		return provisionerAssignmentRecord{}, "", false, nil
	}

	assignment, err := selected.bundle.Queue.ReserveAssignment(ctx, worker.ReserveAssignmentInput{
		JobID:                selectedJob.ID,
		ProviderID:           input.ProviderID,
		ProviderRequestID:    input.ProviderRequestID,
		ProfileName:          input.ProfileName,
		ProviderType:         input.ProviderType,
		ProviderOptions:      input.ProviderOptions,
		ProfileLabels:        input.Candidate.ProfileLabels,
		ProfileTaints:        input.Candidate.ProfileTaints,
		ProfileHarnessModels: input.Candidate.ProfileHarnessModels,
		AllowedRoles:         input.Candidate.AllowedRoles,
		AllowedBuckets:       input.Candidate.AllowedBuckets,
		RequiredSelector:     input.Candidate.RequiredSelector,
		StartupDeadline:      time.Now().UTC().Add(input.StartupTimeout),
	})
	if err != nil {
		return provisionerAssignmentRecord{}, "", false, err
	}
	selected.Assignment = assignment
	token, err := r.credentials.CreateToken(ctx, coordinator.CredentialInput{Scope: coordinator.TokenScopeWorker, Subject: assignment.WorkerID})
	if err != nil {
		_ = r.credentials.RevokeSubjectCredentials(ctx, coordinator.TokenScopeWorker, assignment.WorkerID)
		_, _ = selected.bundle.Queue.AbandonAssignment(ctx, worker.AbandonAssignmentInput{AssignmentID: assignment.ID, CloseReason: "credential_issue_failed", ProviderError: err.Error()})
		return provisionerAssignmentRecord{}, "", false, fmt.Errorf("issue assignment worker credential: %w", err)
	}
	return *selected, token, true, nil
}

func (r *Registry) ListProvisionerAssignments(ctx context.Context, input listProvisionerAssignmentsInput) ([]provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()

	allowedProviders := make(map[string]struct{}, len(input.ProviderIDs))
	for _, providerID := range input.ProviderIDs {
		if providerID = strings.TrimSpace(providerID); providerID != "" {
			allowedProviders[providerID] = struct{}{}
		}
	}
	var records []provisionerAssignmentRecord
	for _, bundle := range r.All() {
		if projectID := strings.TrimSpace(input.ProjectID); projectID != "" && bundle.Project.ID != projectID {
			continue
		}
		assignments, err := bundle.Queue.ListAssignments(ctx, input.Filter)
		if err != nil {
			return nil, fmt.Errorf("list assignments in project %s: %w", bundle.Project.ID, err)
		}
		for _, assignment := range assignments {
			if len(allowedProviders) > 0 {
				if _, ok := allowedProviders[assignment.ProviderID]; !ok {
					continue
				}
			}
			if requestID := strings.TrimSpace(input.ProviderRequestID); requestID != "" && assignment.ProviderRequestID != requestID {
				continue
			}
			records = append(records, provisionerAssignmentRecord{Project: bundle.Project, Assignment: assignment, bundle: bundle})
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].Assignment.CreatedAt.Equal(records[j].Assignment.CreatedAt) {
			return records[i].Assignment.CreatedAt.Before(records[j].Assignment.CreatedAt)
		}
		if records[i].Project.ID != records[j].Project.ID {
			return records[i].Project.ID < records[j].Project.ID
		}
		return records[i].Assignment.ID < records[j].Assignment.ID
	})
	return records, nil
}

// ExpirePendingProvisionerAssignments releases elapsed startup reservations
// under the same cross-project serialization used by reservation and claims.
func (r *Registry) ExpirePendingProvisionerAssignments(ctx context.Context) (int, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	var total int
	var errs []error
	for _, bundle := range r.All() {
		expired, err := bundle.Queue.ExpirePendingAssignments(ctx)
		total += expired
		if err != nil {
			errs = append(errs, fmt.Errorf("expire assignments in project %s: %w", bundle.Project.ID, err))
		}
	}
	return total, errors.Join(errs...)
}

func (r *Registry) GetProvisionerAssignment(ctx context.Context, assignmentID string) (provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	return r.findAssignmentByIDLocked(ctx, assignmentID)
}

func (r *Registry) AbandonProvisionerAssignment(ctx context.Context, assignmentID, providerError string) (provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	record, err := r.findAssignmentByIDLocked(ctx, assignmentID)
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	// Fence the provisioned process before releasing its reservation. If the
	// database transition fails, a retry can mint a replacement token only by
	// repeating the same durable provider request; abandoning again revokes it.
	if err := r.credentials.RevokeSubjectCredentials(ctx, coordinator.TokenScopeWorker, record.Assignment.WorkerID); err != nil {
		return provisionerAssignmentRecord{}, err
	}
	assignment, err := record.bundle.Queue.AbandonAssignment(ctx, worker.AbandonAssignmentInput{AssignmentID: record.Assignment.ID, CloseReason: "provider_failed", ProviderError: providerError})
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	record.Assignment = assignment
	return record, nil
}

func (r *Registry) RecordProvisionerAssignmentAttempt(ctx context.Context, assignmentID, providerError string, nextRetryAt *time.Time) (provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	record, err := r.findAssignmentByIDLocked(ctx, assignmentID)
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	assignment, err := record.bundle.Queue.RecordAssignmentAttempt(ctx, worker.AssignmentAttemptInput{AssignmentID: record.Assignment.ID, ProviderError: providerError, NextRetryAt: nextRetryAt})
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	record.Assignment = assignment
	return record, nil
}

func (r *Registry) RevokeProvisionerAssignmentCredentials(ctx context.Context, assignmentID string) (provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	record, err := r.findAssignmentByIDLocked(ctx, assignmentID)
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	if err := r.revokeProvisionerAssignmentCredentialsLocked(ctx, &record); err != nil {
		return provisionerAssignmentRecord{}, err
	}
	return record, nil
}

func (r *Registry) revokeProvisionerAssignmentCredentialsLocked(ctx context.Context, record *provisionerAssignmentRecord) error {
	if record.Assignment.State != worker.AssignmentClosed {
		return fmt.Errorf("%w: open assignment credentials cannot be revoked", worker.ErrAssignmentConflict)
	}
	if record.Assignment.CredentialsRevokedAt != nil {
		return nil
	}
	if err := r.credentials.RevokeSubjectCredentials(ctx, coordinator.TokenScopeWorker, record.Assignment.WorkerID); err != nil {
		return err
	}
	assignment, err := record.bundle.Queue.MarkAssignmentCredentialsRevoked(ctx, record.Assignment.ID)
	if err != nil {
		return err
	}
	record.Assignment = assignment
	return nil
}

func (r *Registry) CleanProvisionerAssignment(ctx context.Context, assignmentID string) (provisionerAssignmentRecord, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	record, err := r.findAssignmentByIDLocked(ctx, assignmentID)
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	if record.Assignment.State != worker.AssignmentClosed {
		return provisionerAssignmentRecord{}, fmt.Errorf("%w: open assignment cannot be cleaned", worker.ErrAssignmentConflict)
	}
	if err := r.revokeProvisionerAssignmentCredentialsLocked(ctx, &record); err != nil {
		return provisionerAssignmentRecord{}, err
	}
	if err := r.directory.DeleteWorker(ctx, record.Assignment.WorkerID); err != nil {
		return provisionerAssignmentRecord{}, err
	}
	assignment, err := record.bundle.Queue.MarkAssignmentCleaned(ctx, record.Assignment.ID)
	if err != nil {
		return provisionerAssignmentRecord{}, err
	}
	record.Assignment = assignment
	return record, nil
}

func (r *Registry) RegisterWorker(ctx context.Context, input worker.RegisterWorkerInput) (worker.Worker, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	record, found, err := r.findAssignmentByWorkerLocked(ctx, input.ID)
	if err != nil {
		return worker.Worker{}, err
	}
	if !found {
		return r.directory.RegisterWorker(ctx, input)
	}
	if err := validateAssignedWorkerRegistration(ctx, record, &input); err != nil {
		return worker.Worker{}, err
	}
	return r.directory.RegisterWorker(ctx, input)
}

func validateAssignedWorkerRegistration(ctx context.Context, record provisionerAssignmentRecord, input *worker.RegisterWorkerInput) error {
	assignment := record.Assignment
	if assignment.State == worker.AssignmentClosed {
		return fmt.Errorf("%w: assignment is closed", worker.ErrAssignmentConflict)
	}
	for key, value := range assignment.ProfileLabels {
		if input.Labels[key] != value {
			return fmt.Errorf("%w: registered worker is missing profile label %s", worker.ErrAssignmentConflict, key)
		}
	}
	for _, expected := range assignment.ProfileTaints {
		if !containsValue(input.Taints, expected) {
			return fmt.Errorf("%w: registered worker is missing a profile taint", worker.ErrAssignmentConflict)
		}
	}
	for _, expected := range assignment.ProfileHarnessModels {
		if !containsHarnessModel(input.HarnessModels, expected) {
			return fmt.Errorf("%w: registered worker is missing profile harness model %s", worker.ErrAssignmentConflict, expected.QualifiedID)
		}
	}
	job, err := record.bundle.Queue.GetJob(ctx, assignment.JobID)
	if err != nil {
		return err
	}
	if job.Role != assignment.Role || job.CapacityBucket != assignment.CapacityBucket {
		return fmt.Errorf("%w: assigned job no longer matches its snapshot", worker.ErrAssignmentConflict)
	}
	if assignment.CapacityBucket == worker.BucketPersistentAgent {
		if input.CapacityPersistentAgent <= 0 {
			return fmt.Errorf("%w: worker does not accept the assigned capacity bucket", worker.ErrAssignmentConflict)
		}
		input.CapacityPersistentAgent, input.CapacityEphemeral = 1, 0
	} else {
		if input.CapacityEphemeral <= 0 {
			return fmt.Errorf("%w: worker does not accept the assigned capacity bucket", worker.ErrAssignmentConflict)
		}
		input.CapacityPersistentAgent, input.CapacityEphemeral = 0, 1
	}
	selector, err := scheduler.NewSelector(job.Selector)
	if err != nil {
		return err
	}
	eligible, err := scheduler.Eligible(scheduler.Job{Selector: selector, Tolerations: job.Tolerations, CapacityBucket: scheduler.CapacityBucket(job.CapacityBucket)}, scheduler.Worker{
		Labels: input.Labels, Taints: input.Taints,
		Capacity: scheduler.Capacity{PersistentAgent: input.CapacityPersistentAgent, Ephemeral: input.CapacityEphemeral},
	})
	if err != nil {
		return err
	}
	if !eligible {
		return fmt.Errorf("%w: registered worker is not eligible for the assigned job", worker.ErrAssignmentConflict)
	}
	return nil
}

func containsValue[T any](values []T, expected T) bool {
	for _, value := range values {
		if reflect.DeepEqual(value, expected) {
			return true
		}
	}
	return false
}

func containsHarnessModel(values []flowharness.Model, expected flowharness.Model) bool {
	for _, value := range values {
		if reflect.DeepEqual(value, expected) {
			return true
		}
	}
	return false
}

func (r *Registry) claimWorkerLocked(ctx context.Context, input worker.ClaimInput) (worker.ProjectClaim, bool, error) {
	record, assigned, err := r.findAssignmentByWorkerLocked(ctx, input.WorkerID)
	if err != nil {
		return worker.ProjectClaim{}, false, err
	}
	if !assigned {
		queues := make([]worker.ProjectQueue, 0)
		for _, bundle := range r.All() {
			queues = append(queues, worker.ProjectQueue{ProjectID: bundle.Project.ID, Queue: bundle.Queue})
		}
		return worker.ClaimAcrossProjects(ctx, r.directory, queues, input)
	}

	registered, err := r.directory.GetWorker(ctx, input.WorkerID)
	if err != nil {
		return worker.ProjectClaim{}, false, err
	}
	var used scheduler.Capacity
	for _, bundle := range r.All() {
		projectUsed, err := bundle.Queue.UsedCapacity(ctx, input.WorkerID)
		if err != nil {
			return worker.ProjectClaim{}, false, fmt.Errorf("aggregate assignment worker capacity for project %s: %w", bundle.Project.ID, err)
		}
		used.PersistentAgent += projectUsed.PersistentAgent
		used.Ephemeral += projectUsed.Ephemeral
	}
	claimed, err := record.bundle.Queue.ClaimAssignment(ctx, worker.ClaimAssignmentInput{
		AssignmentID:  record.Assignment.ID,
		Worker:        registered,
		Used:          used,
		LeaseDuration: input.LeaseDuration,
	})
	if err != nil {
		return worker.ProjectClaim{}, false, err
	}
	return worker.ProjectClaim{ProjectID: record.Project.ID, Job: claimed.Job, Lease: claimed.Lease}, true, nil
}

func (r *Registry) findAssignmentByIDLocked(ctx context.Context, assignmentID string) (provisionerAssignmentRecord, error) {
	assignmentID = strings.TrimSpace(assignmentID)
	if assignmentID == "" {
		return provisionerAssignmentRecord{}, errors.New("assignment id is required")
	}
	var found *provisionerAssignmentRecord
	for _, bundle := range r.All() {
		assignment, err := bundle.Queue.GetAssignment(ctx, assignmentID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return provisionerAssignmentRecord{}, err
		}
		if found != nil {
			return provisionerAssignmentRecord{}, fmt.Errorf("assignment id exists in multiple projects")
		}
		record := provisionerAssignmentRecord{Project: bundle.Project, Assignment: assignment, bundle: bundle}
		found = &record
	}
	if found == nil {
		return provisionerAssignmentRecord{}, sql.ErrNoRows
	}
	return *found, nil
}

func (r *Registry) findAssignmentByWorkerLocked(ctx context.Context, workerID string) (provisionerAssignmentRecord, bool, error) {
	workerID = strings.TrimSpace(workerID)
	var found *provisionerAssignmentRecord
	for _, bundle := range r.All() {
		assignment, ok, err := bundle.Queue.FindAssignmentByWorker(ctx, workerID)
		if err != nil {
			return provisionerAssignmentRecord{}, false, err
		}
		if !ok {
			continue
		}
		if found != nil {
			return provisionerAssignmentRecord{}, false, fmt.Errorf("worker assignment exists in multiple projects")
		}
		record := provisionerAssignmentRecord{Project: bundle.Project, Assignment: assignment, bundle: bundle}
		found = &record
	}
	if found == nil {
		return provisionerAssignmentRecord{}, false, nil
	}
	return *found, true, nil
}

func (r *Registry) findAssignmentByRequestLocked(ctx context.Context, providerID, requestID string) (provisionerAssignmentRecord, bool, error) {
	var found *provisionerAssignmentRecord
	for _, bundle := range r.All() {
		assignment, ok, err := bundle.Queue.FindAssignmentByRequest(ctx, providerID, requestID)
		if err != nil {
			return provisionerAssignmentRecord{}, false, err
		}
		if !ok {
			continue
		}
		if found != nil {
			return provisionerAssignmentRecord{}, false, fmt.Errorf("provider request exists in multiple projects")
		}
		record := provisionerAssignmentRecord{Project: bundle.Project, Assignment: assignment, bundle: bundle}
		found = &record
	}
	if found == nil {
		return provisionerAssignmentRecord{}, false, nil
	}
	return *found, true, nil
}
