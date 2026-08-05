package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// ErrCapacitySlotConflict indicates that a slot cannot perform the requested
// lifecycle transition.
var ErrCapacitySlotConflict = errors.New("capacity slot conflict")

type CapacitySlotState string

const (
	CapacitySlotProvisioning CapacitySlotState = "provisioning"
	CapacitySlotUnready      CapacitySlotState = "unready"
	CapacitySlotReady        CapacitySlotState = "ready"
	CapacitySlotBound        CapacitySlotState = "bound"
	CapacitySlotClosed       CapacitySlotState = "closed"
)

// CapacitySlot is coordinator-global durable state for one provisioned,
// one-shot worker. A slot can be bound at most once.
type CapacitySlot struct {
	ID                   string              `json:"id"`
	WorkerID             string              `json:"worker_id"`
	ProviderID           string              `json:"provider_id"`
	ProfileName          string              `json:"profile_name"`
	ProviderRequestID    string              `json:"provider_request_id"`
	ProviderType         string              `json:"provider_type"`
	ProviderOptions      map[string]string   `json:"provider_options"`
	State                CapacitySlotState   `json:"state"`
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
	CapabilityCheckedAt  *time.Time          `json:"capability_checked_at,omitempty"`
	CapabilityError      string              `json:"capability_error,omitempty"`
	AssignmentID         *string             `json:"assignment_id,omitempty"`
	ProjectID            *string             `json:"project_id,omitempty"`
	CreatedAt            time.Time           `json:"created_at"`
	UpdatedAt            time.Time           `json:"updated_at"`
	ClosedAt             *time.Time          `json:"closed_at,omitempty"`
	CloseReason          *string             `json:"close_reason,omitempty"`
	LastProviderError    *string             `json:"last_provider_error,omitempty"`
	CredentialsRevokedAt *time.Time          `json:"credentials_revoked_at,omitempty"`
	CleanedAt            *time.Time          `json:"cleaned_at,omitempty"`
}

type CreateCapacitySlotInput struct {
	ID                   string
	WorkerID             string
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

type CapacitySlotFilter struct {
	ProviderIDs  []string
	ProviderID   string
	ProfileName  string
	WorkerID     string
	States       []CapacitySlotState
	OpenOnly     bool
	NeedsCleanup bool
}

// CapacitySlots stores slot lifecycle state in the coordinator-global DB.
type CapacitySlots struct {
	db  *sql.DB
	now func() time.Time
}

func NewCapacitySlots(database *sql.DB) *CapacitySlots {
	return &CapacitySlots{db: database, now: sqlitex.UTCNow}
}

func (s *CapacitySlots) Create(ctx context.Context, input CreateCapacitySlotInput) (CapacitySlot, error) {
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.ProfileName = strings.TrimSpace(input.ProfileName)
	input.ProviderRequestID = strings.TrimSpace(input.ProviderRequestID)
	input.ProviderType = strings.TrimSpace(input.ProviderType)
	if input.ProviderID == "" || input.ProfileName == "" || input.ProviderRequestID == "" || input.ProviderType == "" {
		return CapacitySlot{}, errors.New("provider, profile, provider request, and provider type are required")
	}
	if input.StartupDeadline.IsZero() || !input.StartupDeadline.After(s.now().UTC()) {
		return CapacitySlot{}, errors.New("a future startup deadline is required")
	}
	profile, err := normalizeAssignmentProfile(input.ProfileLabels, input.ProfileTaints, input.ProfileHarnessModels, input.AllowedRoles, input.AllowedBuckets, input.RequiredSelector)
	if err != nil {
		return CapacitySlot{}, err
	}
	if strings.TrimSpace(input.ID) == "" {
		input.ID, err = randomID("cs")
		if err != nil {
			return CapacitySlot{}, err
		}
	}
	if strings.TrimSpace(input.WorkerID) == "" {
		input.WorkerID, err = randomID("w-prov")
		if err != nil {
			return CapacitySlot{}, err
		}
	}
	if input.ProviderOptions == nil {
		input.ProviderOptions = map[string]string{}
	}

	existing, err := s.FindByRequest(ctx, input.ProviderID, input.ProviderRequestID)
	if err == nil {
		if existing.ProfileName != input.ProfileName || existing.ProviderType != input.ProviderType || !reflect.DeepEqual(existing.ProviderOptions, input.ProviderOptions) {
			return CapacitySlot{}, slotConflict("provider request is already owned by another profile or provider descriptor")
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CapacitySlot{}, err
	}

	optionsJSON, err := encodeStringMap(input.ProviderOptions)
	if err != nil {
		return CapacitySlot{}, err
	}
	labelsJSON, _ := encodeStringMap(profile.labels)
	taintsJSON, _ := encodeTaints(profile.taints)
	modelsJSON, _ := encodeHarnessModels(profile.models)
	rolesJSON, _ := encodeJobRoles(profile.roles)
	bucketsJSON, _ := encodeCapacityBuckets(profile.buckets)
	requiredJSON, _ := encodeStringMap(profile.required)
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO capacity_slots (
	id, worker_id, provider_id, profile_name, provider_request_id, provider_type,
	provider_options_json, state, profile_labels_json, profile_taints_json,
	profile_harness_models_json, allowed_roles_json, allowed_buckets_json,
	required_selector_json, startup_deadline, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 'provisioning', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(input.ID), strings.TrimSpace(input.WorkerID), input.ProviderID,
		input.ProfileName, input.ProviderRequestID, input.ProviderType, optionsJSON,
		labelsJSON, taintsJSON, modelsJSON, rolesJSON, bucketsJSON, requiredJSON,
		formatTime(input.StartupDeadline.UTC()), formatTime(now), formatTime(now))
	if err != nil {
		return CapacitySlot{}, slotConflict("worker or provider request already has a capacity slot")
	}
	return s.Get(ctx, input.ID)
}

func (s *CapacitySlots) Get(ctx context.Context, id string) (CapacitySlot, error) {
	return scanCapacitySlot(s.db.QueryRowContext(ctx, capacitySlotSelectSQL+"\nWHERE id = ?", strings.TrimSpace(id)))
}

func (s *CapacitySlots) FindByWorker(ctx context.Context, workerID string) (CapacitySlot, error) {
	return scanCapacitySlot(s.db.QueryRowContext(ctx, capacitySlotSelectSQL+"\nWHERE worker_id = ?", strings.TrimSpace(workerID)))
}

func (s *CapacitySlots) FindByRequest(ctx context.Context, providerID, requestID string) (CapacitySlot, error) {
	return scanCapacitySlot(s.db.QueryRowContext(ctx, capacitySlotSelectSQL+"\nWHERE provider_id = ? AND provider_request_id = ?", strings.TrimSpace(providerID), strings.TrimSpace(requestID)))
}

func (s *CapacitySlots) List(ctx context.Context, filter CapacitySlotFilter) ([]CapacitySlot, error) {
	var where []string
	var args []any
	add := func(clause string, value any) { where = append(where, clause); args = append(args, value) }
	if value := strings.TrimSpace(filter.ProviderID); value != "" {
		add("provider_id = ?", value)
	}
	if value := strings.TrimSpace(filter.ProfileName); value != "" {
		add("profile_name = ?", value)
	}
	if value := strings.TrimSpace(filter.WorkerID); value != "" {
		add("worker_id = ?", value)
	}
	if len(filter.ProviderIDs) > 0 {
		placeholders := make([]string, 0, len(filter.ProviderIDs))
		for _, value := range filter.ProviderIDs {
			if value = strings.TrimSpace(value); value != "" {
				placeholders = append(placeholders, "?")
				args = append(args, value)
			}
		}
		if len(placeholders) > 0 {
			where = append(where, "provider_id IN ("+strings.Join(placeholders, ", ")+")")
		}
	}
	if filter.OpenOnly {
		where = append(where, "state != 'closed'")
	}
	if filter.NeedsCleanup {
		where = append(where, "state = 'closed' AND cleaned_at IS NULL")
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			if err := validateCapacitySlotState(state); err != nil {
				return nil, err
			}
			placeholders[i] = "?"
			args = append(args, string(state))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	query := capacitySlotSelectSQL
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list capacity slots: %w", err)
	}
	defer rows.Close()
	var result []CapacitySlot
	for rows.Next() {
		slot, err := scanCapacitySlot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, slot)
	}
	return result, rows.Err()
}

// RecordAttempt records one provider launch attempt. A successful attempt has
// an empty provider error and nil retry time.
func (s *CapacitySlots) RecordAttempt(ctx context.Context, id, providerError string, nextRetryAt *time.Time) (CapacitySlot, error) {
	now := s.now().UTC()
	current, err := s.Get(ctx, id)
	if err != nil {
		return CapacitySlot{}, err
	}
	nextState := current.State
	startupDeadline := current.StartupDeadline
	if strings.TrimSpace(providerError) == "" && nextRetryAt == nil && current.State == CapacitySlotUnready {
		nextState = CapacitySlotProvisioning
		startupWindow := current.StartupDeadline.Sub(current.CreatedAt)
		if startupWindow <= 0 {
			startupWindow = 2 * time.Minute
		}
		startupDeadline = now.Add(startupWindow)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET retry_count = retry_count + 1, last_attempt_at = ?, next_retry_at = ?,
	last_provider_error = NULLIF(?, ''), state = ?, startup_deadline = ?,
	capability_checked_at = CASE WHEN ? = 'provisioning' AND state = 'unready' THEN NULL ELSE capability_checked_at END,
	capability_error = CASE WHEN ? = 'provisioning' AND state = 'unready' THEN '' ELSE capability_error END,
	updated_at = ?
WHERE id = ? AND state IN ('provisioning', 'unready', 'ready')`, formatTime(now), nullableTime(nextRetryAt), strings.TrimSpace(providerError),
		string(nextState), formatTime(startupDeadline), string(nextState), string(nextState), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("record capacity slot attempt: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return CapacitySlot{}, slotConflict("capacity slot is not launchable")
	}
	return s.Get(ctx, id)
}

// RecordCapabilities moves an unbound slot to ready or unready based on the
// worker's live, runtime-derived capability report.
func (s *CapacitySlots) RecordCapabilities(ctx context.Context, id string, capabilityErr error) (CapacitySlot, error) {
	now := s.now().UTC()
	state := CapacitySlotReady
	message := ""
	if capabilityErr != nil {
		state = CapacitySlotUnready
		message = capabilityErr.Error()
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET state = ?, capability_checked_at = ?, capability_error = ?, updated_at = ?
WHERE id = ? AND state IN ('provisioning', 'unready', 'ready')`, string(state), formatTime(now), message, formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("record slot capabilities: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return CapacitySlot{}, slotConflict("bound or closed slot capabilities cannot change")
	}
	return s.Get(ctx, id)
}

// RefreshBoundCapabilities records the mandatory pre-claim capability check
// without reopening or otherwise changing a bound slot.
func (s *CapacitySlots) RefreshBoundCapabilities(ctx context.Context, id string) (CapacitySlot, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET capability_checked_at = ?, capability_error = '', updated_at = ?
WHERE id = ? AND state = 'bound'`, formatTime(now), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("refresh bound slot capabilities: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return CapacitySlot{}, slotConflict("capacity slot is not bound")
	}
	return s.Get(ctx, id)
}

func (s *CapacitySlots) Bind(ctx context.Context, id, projectID, assignmentID string) (CapacitySlot, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET state = 'bound', project_id = ?, assignment_id = ?, updated_at = ?
WHERE id = ? AND state = 'ready'`, strings.TrimSpace(projectID), strings.TrimSpace(assignmentID), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("bind capacity slot: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return CapacitySlot{}, slotConflict("capacity slot is not ready")
	}
	return s.Get(ctx, id)
}

// RepairBinding is used after a coordinator crash between the project-local
// assignment commit and the global slot update.
func (s *CapacitySlots) RepairBinding(ctx context.Context, id, projectID, assignmentID string) (CapacitySlot, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET state = 'bound', project_id = ?, assignment_id = ?, updated_at = ?
WHERE id = ? AND state IN ('provisioning', 'unready', 'ready') AND assignment_id IS NULL`,
		strings.TrimSpace(projectID), strings.TrimSpace(assignmentID), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("repair capacity slot binding: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		current, getErr := s.Get(ctx, id)
		if getErr == nil && current.State == CapacitySlotBound && current.ProjectID != nil && current.AssignmentID != nil && *current.ProjectID == strings.TrimSpace(projectID) && *current.AssignmentID == strings.TrimSpace(assignmentID) {
			return current, nil
		}
		return CapacitySlot{}, slotConflict("capacity slot binding cannot be repaired")
	}
	return s.Get(ctx, id)
}

func (s *CapacitySlots) Close(ctx context.Context, id, reason, providerError string) (CapacitySlot, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE capacity_slots
SET state = 'closed', closed_at = ?, close_reason = ?,
	last_provider_error = COALESCE(NULLIF(?, ''), last_provider_error), updated_at = ?
WHERE id = ? AND state != 'closed'`, formatTime(now), strings.TrimSpace(reason), strings.TrimSpace(providerError), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, fmt.Errorf("close capacity slot: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return s.Get(ctx, id)
	}
	return s.Get(ctx, id)
}

func (s *CapacitySlots) MarkCredentialsRevoked(ctx context.Context, id string) (CapacitySlot, error) {
	now := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE capacity_slots SET credentials_revoked_at = COALESCE(credentials_revoked_at, ?), updated_at = ? WHERE id = ? AND state = 'closed'`, formatTime(now), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, err
	}
	return s.Get(ctx, id)
}

func (s *CapacitySlots) MarkCleaned(ctx context.Context, id string) (CapacitySlot, error) {
	now := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE capacity_slots SET cleaned_at = COALESCE(cleaned_at, ?), updated_at = ? WHERE id = ? AND state = 'closed' AND credentials_revoked_at IS NOT NULL`, formatTime(now), formatTime(now), strings.TrimSpace(id))
	if err != nil {
		return CapacitySlot{}, err
	}
	return s.Get(ctx, id)
}

// WorkerSatisfiesSlot checks only capability identity and exact model IDs.
// Model reasoning metadata is deliberately ignored: Harness normalizes all
// reasoning levels and reasoning never affects scheduling.
func WorkerSatisfiesSlot(slot CapacitySlot, actual Worker) error {
	for key, value := range slot.ProfileLabels {
		if actual.Labels[key] != value {
			return fmt.Errorf("worker is missing profile label %s=%s", key, value)
		}
	}
	for _, expected := range slot.ProfileTaints {
		found := false
		for _, value := range actual.Taints {
			if reflect.DeepEqual(value, expected) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("worker is missing a profile taint")
		}
	}
	for _, expected := range slot.ProfileHarnessModels {
		if !HarnessModelAvailable(actual.HarnessModels, expected.Harness, expected.QualifiedID) {
			return fmt.Errorf("worker is missing profile harness model %s", expected.QualifiedID)
		}
	}
	return nil
}

func HarnessModelAvailable(models []flowharness.Model, harnessName, qualifiedID string) bool {
	harnessName = strings.ToLower(strings.TrimSpace(harnessName))
	qualifiedID = strings.TrimSpace(qualifiedID)
	for _, model := range models {
		if strings.ToLower(strings.TrimSpace(model.Harness)) == harnessName && strings.TrimSpace(model.QualifiedID) == qualifiedID {
			return true
		}
	}
	return false
}

func slotConflict(message string) error {
	return fmt.Errorf("%w: %s", ErrCapacitySlotConflict, message)
}

func validateCapacitySlotState(state CapacitySlotState) error {
	switch state {
	case CapacitySlotProvisioning, CapacitySlotUnready, CapacitySlotReady, CapacitySlotBound, CapacitySlotClosed:
		return nil
	default:
		return fmt.Errorf("unsupported capacity slot state %q", state)
	}
}

const capacitySlotSelectSQL = `
SELECT id, worker_id, provider_id, profile_name, provider_request_id, provider_type,
	provider_options_json, state, profile_labels_json, profile_taints_json,
	profile_harness_models_json, allowed_roles_json, allowed_buckets_json,
	required_selector_json, startup_deadline, retry_count, next_retry_at,
	last_attempt_at, capability_checked_at, capability_error, assignment_id,
	project_id, created_at, updated_at, closed_at, close_reason,
	last_provider_error, credentials_revoked_at, cleaned_at
FROM capacity_slots`

func scanCapacitySlot(row scanner) (CapacitySlot, error) {
	var slot CapacitySlot
	var state, optionsJSON, labelsJSON, taintsJSON, modelsJSON, rolesJSON, bucketsJSON, requiredJSON string
	var startupDeadline, createdAt, updatedAt string
	var nextRetryAt, lastAttemptAt, capabilityCheckedAt, assignmentID, projectID sql.NullString
	var closedAt, closeReason, providerError, revokedAt, cleanedAt sql.NullString
	if err := row.Scan(&slot.ID, &slot.WorkerID, &slot.ProviderID, &slot.ProfileName,
		&slot.ProviderRequestID, &slot.ProviderType, &optionsJSON, &state, &labelsJSON,
		&taintsJSON, &modelsJSON, &rolesJSON, &bucketsJSON, &requiredJSON,
		&startupDeadline, &slot.RetryCount, &nextRetryAt, &lastAttemptAt,
		&capabilityCheckedAt, &slot.CapabilityError, &assignmentID, &projectID,
		&createdAt, &updatedAt, &closedAt, &closeReason, &providerError, &revokedAt,
		&cleanedAt); err != nil {
		return CapacitySlot{}, fmt.Errorf("scan capacity slot: %w", err)
	}
	var err error
	slot.State = CapacitySlotState(state)
	if slot.ProviderOptions, err = decodeStringMap(optionsJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.ProfileLabels, err = decodeStringMap(labelsJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.ProfileTaints, err = decodeTaints(taintsJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.ProfileHarnessModels, err = decodeHarnessModels(modelsJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.AllowedRoles, err = decodeJobRoles(rolesJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.AllowedBuckets, err = decodeCapacityBuckets(bucketsJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.RequiredSelector, err = decodeStringMap(requiredJSON); err != nil {
		return CapacitySlot{}, err
	}
	if slot.StartupDeadline, err = parseTime(startupDeadline); err != nil {
		return CapacitySlot{}, err
	}
	if slot.CreatedAt, err = parseTime(createdAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.NextRetryAt, err = nullableParsedTime(nextRetryAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.LastAttemptAt, err = nullableParsedTime(lastAttemptAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.CapabilityCheckedAt, err = nullableParsedTime(capabilityCheckedAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.ClosedAt, err = nullableParsedTime(closedAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.CredentialsRevokedAt, err = nullableParsedTime(revokedAt); err != nil {
		return CapacitySlot{}, err
	}
	if slot.CleanedAt, err = nullableParsedTime(cleanedAt); err != nil {
		return CapacitySlot{}, err
	}
	slot.AssignmentID = nullableStringPointer(assignmentID)
	slot.ProjectID = nullableStringPointer(projectID)
	slot.CloseReason = nullableStringPointer(closeReason)
	slot.LastProviderError = nullableStringPointer(providerError)
	return slot, nil
}
