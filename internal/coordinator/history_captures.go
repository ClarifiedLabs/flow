package coordinator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrHistoryCaptureNotFound     = errors.New("history capture not found")
	ErrHistoryArtifactNotFound    = errors.New("history artifact not found")
	ErrHistoryUnauthorized        = errors.New("history capture upload grant is unauthorized")
	ErrHistoryConflict            = errors.New("history capture immutable metadata conflict")
	ErrHistoryVersionConflict     = errors.New("history capture version conflict")
	ErrHistoryInvalidTransition   = errors.New("invalid history capture state transition")
	ErrHistoryIncomplete          = errors.New("history capture is incomplete")
	ErrHistoryPublicationPending  = errors.New("history artifact publication is pending")
	ErrHistoryGrantNoLongerUsable = errors.New("history capture upload grant is no longer usable")
)

const (
	maxHistoryCodeLength    = 64
	maxHistoryMessageLength = 1024
	maxHistoryActorLength   = 255
	maxHistoryLogicalKey    = 512
)

type HistoryCaptureState string

const (
	HistoryCaptureReserved  HistoryCaptureState = "reserved"
	HistoryCaptureRunning   HistoryCaptureState = "running"
	HistoryCaptureQuiescing HistoryCaptureState = "quiescing"
	HistoryCaptureSealed    HistoryCaptureState = "sealed"
	HistoryCaptureUploading HistoryCaptureState = "uploading"
	HistoryCaptureComplete  HistoryCaptureState = "complete"
	HistoryCaptureBlocked   HistoryCaptureState = "blocked"
	HistoryCaptureLost      HistoryCaptureState = "lost"
	HistoryCaptureWaived    HistoryCaptureState = "waived"
)

type HistoryExecutionVerdict string

const (
	HistoryExecutionPending   HistoryExecutionVerdict = "pending"
	HistoryExecutionSucceeded HistoryExecutionVerdict = "succeeded"
	HistoryExecutionFailed    HistoryExecutionVerdict = "failed"
	HistoryExecutionCancelled HistoryExecutionVerdict = "cancelled"
	HistoryExecutionCrashed   HistoryExecutionVerdict = "crashed"
)

type HistoryArtifactKind string

const (
	HistoryArtifactTranscriptSegment HistoryArtifactKind = "transcript_segment"
	HistoryArtifactHarnessRoot       HistoryArtifactKind = "harness_root"
	HistoryArtifactManifest          HistoryArtifactKind = "manifest"
)

type HistoryArtifactPhase string

const (
	HistoryArtifactCheckpoint HistoryArtifactPhase = "checkpoint"
	HistoryArtifactFinal      HistoryArtifactPhase = "final"
)

type HistoryPublicationState string

const (
	HistoryPublicationPending   HistoryPublicationState = "pending"
	HistoryPublicationCommitted HistoryPublicationState = "committed"
)

// HistoryCapture is safe for authorized metadata projections. It deliberately
// contains neither the upload-grant hash nor any blob key/temporary upload ID.
type HistoryCapture struct {
	ID             string
	ProjectID      string
	JobID          string
	LeaseID        string
	LeaseAttempt   int64
	WorkerID       string
	TaskID         string
	SessionID      string
	WorkflowRunID  string
	NodeRunID      string
	NodeVisit      int64
	Stage          string
	Role           string
	HarnessName    string
	HarnessVersion string

	ExpectedTranscript  bool
	ExpectedHarness     bool
	State               HistoryCaptureState
	ExecutionVerdict    HistoryExecutionVerdict
	ExecutionExitCode   *int
	ExecutionErrorCode  string
	ExecutionRecordedAt *time.Time

	ExpectedSetDeclaredAt          *time.Time
	ExpectedFinalArtifactCount     *int64
	ExpectedTranscriptEpoch        *int64
	ExpectedTranscriptSegmentCount *int64
	ExpectedTranscriptLength       *int64
	ExpectedTranscriptSHA256       string

	LastHintAt                        *time.Time
	CheckpointHintCount               int64
	CheckpointCoalescedCount          int64
	CheckpointDirtyGeneration         int64
	LastCheckpointAttemptGeneration   int64
	LastCheckpointCommittedGeneration int64
	LastCheckpointBytes               int64
	LastCheckpointEntries             int64

	ErrorCode    string
	ErrorMessage string
	WaiverReason string
	Version      int64
	ReservedAt   time.Time
	UpdatedAt    time.Time
	RunningAt    *time.Time
	QuiescingAt  *time.Time
	SealedAt     *time.Time
	UploadingAt  *time.Time
	CompletedAt  *time.Time
	BlockedAt    *time.Time
	LostAt       *time.Time
	WaivedAt     *time.Time
}

type ReserveHistoryCaptureInput struct {
	ProjectID          string
	JobID              string
	LeaseID            string
	LeaseAttempt       int64
	WorkerID           string
	TaskID             string
	SessionID          string
	WorkflowRunID      string
	NodeRunID          string
	NodeVisit          int64
	Stage              string
	Role               string
	HarnessName        string
	HarnessVersion     string
	ExpectedTranscript bool
	ExpectedHarness    bool
}

type ReserveHistoryCaptureResult struct {
	Capture     HistoryCapture
	UploadGrant string // revealed only when Created is true
	Created     bool
}

type HistoryCaptureListOptions struct {
	TaskID string
	JobID  string
	Limit  int
}

type HistoryCaptureService struct {
	db    *sql.DB
	blobs blob.Store
	now   func() time.Time
}

func NewHistoryCaptureService(database *sql.DB, blobs blob.Store) *HistoryCaptureService {
	return &HistoryCaptureService{db: database, blobs: blobs, now: sqlitex.UTCNow}
}

func (s *HistoryCaptureService) Reserve(ctx context.Context, input ReserveHistoryCaptureInput) (ReserveHistoryCaptureResult, error) {
	input = normalizeReserveHistoryCaptureInput(input)
	if err := validateReserveHistoryCaptureInput(input); err != nil {
		return ReserveHistoryCaptureResult{}, err
	}
	captureID, err := randomHistoryID("hc")
	if err != nil {
		return ReserveHistoryCaptureResult{}, err
	}
	grant, err := randomHistorySecret()
	if err != nil {
		return ReserveHistoryCaptureResult{}, err
	}
	grantHash := hashHistoryGrant(grant)
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ReserveHistoryCaptureResult{}, fmt.Errorf("begin history capture reservation: %w", err)
	}
	defer tx.Rollback()

	existing, err := getHistoryCaptureByAttempt(ctx, tx, input.JobID, input.LeaseID)
	if err == nil {
		if !sameReservation(existing, input) {
			return ReserveHistoryCaptureResult{}, fmt.Errorf("%w: lease attempt attribution differs", ErrHistoryConflict)
		}
		return ReserveHistoryCaptureResult{Capture: existing}, nil
	}
	if !errors.Is(err, ErrHistoryCaptureNotFound) {
		return ReserveHistoryCaptureResult{}, err
	}

	var nodeVisit any
	if input.NodeVisit > 0 {
		nodeVisit = input.NodeVisit
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_captures (
    id, project_id, job_id, lease_id, lease_attempt, worker_id,
    task_id, session_id, workflow_run_id, node_run_id, node_visit,
    stage, role, harness_name, harness_version,
    expected_transcript, expected_harness, upload_grant_hash,
    reserved_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		captureID, input.ProjectID, input.JobID, input.LeaseID, input.LeaseAttempt, input.WorkerID,
		input.TaskID, input.SessionID, input.WorkflowRunID, input.NodeRunID, nodeVisit,
		input.Stage, input.Role, input.HarnessName, input.HarnessVersion,
		boolToInt(input.ExpectedTranscript), boolToInt(input.ExpectedHarness), grantHash,
		nowText, nowText)
	if err != nil {
		if isUniqueViolation(err, "history_captures.job_id, history_captures.lease_attempt") {
			return ReserveHistoryCaptureResult{}, fmt.Errorf("%w: lease attempt number already reserved", ErrHistoryConflict)
		}
		return ReserveHistoryCaptureResult{}, fmt.Errorf("insert history capture: %w", err)
	}
	if input.ExpectedTranscript {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO history_transcript_streams (capture_id, created_at, updated_at)
VALUES (?, ?, ?)`, captureID, nowText, nowText); err != nil {
			return ReserveHistoryCaptureResult{}, fmt.Errorf("create transcript stream: %w", err)
		}
	}
	if err := appendHistoryCaptureEvent(ctx, tx, captureID, "reserved", "", string(HistoryCaptureReserved), 0, "system", "", nil, now); err != nil {
		return ReserveHistoryCaptureResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReserveHistoryCaptureResult{}, fmt.Errorf("commit history capture reservation: %w", err)
	}
	capture, err := s.Get(ctx, captureID)
	if err != nil {
		return ReserveHistoryCaptureResult{}, err
	}
	return ReserveHistoryCaptureResult{Capture: capture, UploadGrant: grant, Created: true}, nil
}

func normalizeReserveHistoryCaptureInput(input ReserveHistoryCaptureInput) ReserveHistoryCaptureInput {
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.JobID = strings.TrimSpace(input.JobID)
	input.LeaseID = strings.TrimSpace(input.LeaseID)
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.WorkflowRunID = strings.TrimSpace(input.WorkflowRunID)
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.Stage = strings.TrimSpace(input.Stage)
	input.Role = strings.TrimSpace(input.Role)
	input.HarnessName = strings.TrimSpace(input.HarnessName)
	input.HarnessVersion = strings.TrimSpace(input.HarnessVersion)
	return input
}

func validateReserveHistoryCaptureInput(input ReserveHistoryCaptureInput) error {
	for name, value := range map[string]string{
		"project id": input.ProjectID, "job id": input.JobID, "lease id": input.LeaseID,
		"worker id": input.WorkerID, "role": input.Role,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if input.LeaseAttempt < 1 {
		return errors.New("lease attempt must be positive")
	}
	if input.NodeVisit < 0 {
		return errors.New("node visit cannot be negative")
	}
	if len(input.Role) > 64 || len(input.Stage) > 128 || len(input.ProjectID) > 255 || len(input.JobID) > 255 || len(input.LeaseID) > 255 || len(input.WorkerID) > 255 {
		return errors.New("history capture attribution exceeds its storage bound")
	}
	return nil
}

func sameReservation(c HistoryCapture, input ReserveHistoryCaptureInput) bool {
	return c.ProjectID == input.ProjectID && c.JobID == input.JobID && c.LeaseID == input.LeaseID &&
		c.LeaseAttempt == input.LeaseAttempt && c.WorkerID == input.WorkerID && c.TaskID == input.TaskID &&
		c.SessionID == input.SessionID && c.WorkflowRunID == input.WorkflowRunID && c.NodeRunID == input.NodeRunID &&
		c.NodeVisit == input.NodeVisit && c.Stage == input.Stage && c.Role == input.Role &&
		c.HarnessName == input.HarnessName && c.HarnessVersion == input.HarnessVersion &&
		c.ExpectedTranscript == input.ExpectedTranscript && c.ExpectedHarness == input.ExpectedHarness
}

func (s *HistoryCaptureService) AuthenticateUploadGrant(ctx context.Context, captureID, grant string) error {
	var hash, state string
	var revoked sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT upload_grant_hash, state, upload_grant_revoked_at
FROM history_captures WHERE id = ?`, strings.TrimSpace(captureID)).Scan(&hash, &state, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHistoryUnauthorized
		}
		return fmt.Errorf("read history upload grant: %w", err)
	}
	if !historyGrantMatches(hash, grant) {
		return ErrHistoryUnauthorized
	}
	if revoked.Valid || state == string(HistoryCaptureComplete) || state == string(HistoryCaptureWaived) {
		return ErrHistoryGrantNoLongerUsable
	}
	return nil
}

func (s *HistoryCaptureService) Get(ctx context.Context, captureID string) (HistoryCapture, error) {
	return scanHistoryCapture(s.db.QueryRowContext(ctx, historyCaptureSelect+` WHERE id = ?`, strings.TrimSpace(captureID)))
}

func (s *HistoryCaptureService) List(ctx context.Context, options HistoryCaptureListOptions) ([]HistoryCapture, error) {
	limit := options.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return nil, errors.New("history capture list limit must be between 1 and 500")
	}
	query := historyCaptureSelect + ` WHERE 1 = 1`
	var args []any
	if taskID := strings.TrimSpace(options.TaskID); taskID != "" {
		query += ` AND task_id = ?`
		args = append(args, taskID)
	}
	if jobID := strings.TrimSpace(options.JobID); jobID != "" {
		query += ` AND job_id = ?`
		args = append(args, jobID)
	}
	query += ` ORDER BY reserved_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list history captures: %w", err)
	}
	defer rows.Close()
	var captures []HistoryCapture
	for rows.Next() {
		capture, err := scanHistoryCapture(rows)
		if err != nil {
			return nil, err
		}
		captures = append(captures, capture)
	}
	return captures, rows.Err()
}

const historyCaptureSelect = `
SELECT id, project_id, job_id, lease_id, lease_attempt, worker_id,
       task_id, session_id, workflow_run_id, node_run_id, COALESCE(node_visit, 0),
       stage, role, harness_name, harness_version,
       expected_transcript, expected_harness, state,
       execution_verdict, execution_exit_code, execution_error_code, execution_recorded_at,
       expected_set_declared_at, expected_final_artifact_count,
       expected_transcript_epoch, expected_transcript_segment_count,
       expected_transcript_length, COALESCE(expected_transcript_sha256, ''),
       last_hint_at, checkpoint_hint_count, checkpoint_coalesced_count,
       checkpoint_dirty_generation, last_checkpoint_attempt_generation,
       last_checkpoint_committed_generation, last_checkpoint_bytes, last_checkpoint_entries,
       error_code, error_message, waiver_reason, version,
       reserved_at, updated_at, running_at, quiescing_at, sealed_at, uploading_at,
       completed_at, blocked_at, lost_at, waived_at
FROM history_captures`

type historyRowScanner interface{ Scan(...any) error }

func scanHistoryCapture(row historyRowScanner) (HistoryCapture, error) {
	var c HistoryCapture
	var expectedTranscript, expectedHarness int
	var state, verdict string
	var exitCode sql.NullInt64
	var executionAt, declaredAt, lastHint sql.NullString
	var expectedCount, expectedEpoch, expectedSegments, expectedLength sql.NullInt64
	var reservedAt, updatedAt string
	var runningAt, quiescingAt, sealedAt, uploadingAt, completedAt, blockedAt, lostAt, waivedAt sql.NullString
	if err := row.Scan(
		&c.ID, &c.ProjectID, &c.JobID, &c.LeaseID, &c.LeaseAttempt, &c.WorkerID,
		&c.TaskID, &c.SessionID, &c.WorkflowRunID, &c.NodeRunID, &c.NodeVisit,
		&c.Stage, &c.Role, &c.HarnessName, &c.HarnessVersion,
		&expectedTranscript, &expectedHarness, &state,
		&verdict, &exitCode, &c.ExecutionErrorCode, &executionAt,
		&declaredAt, &expectedCount, &expectedEpoch, &expectedSegments, &expectedLength, &c.ExpectedTranscriptSHA256,
		&lastHint, &c.CheckpointHintCount, &c.CheckpointCoalescedCount,
		&c.CheckpointDirtyGeneration, &c.LastCheckpointAttemptGeneration,
		&c.LastCheckpointCommittedGeneration, &c.LastCheckpointBytes, &c.LastCheckpointEntries,
		&c.ErrorCode, &c.ErrorMessage, &c.WaiverReason, &c.Version,
		&reservedAt, &updatedAt, &runningAt, &quiescingAt, &sealedAt, &uploadingAt,
		&completedAt, &blockedAt, &lostAt, &waivedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryCapture{}, ErrHistoryCaptureNotFound
		}
		return HistoryCapture{}, fmt.Errorf("scan history capture: %w", err)
	}
	c.ExpectedTranscript = expectedTranscript != 0
	c.ExpectedHarness = expectedHarness != 0
	c.State = HistoryCaptureState(state)
	c.ExecutionVerdict = HistoryExecutionVerdict(verdict)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		c.ExecutionExitCode = &value
	}
	c.ExpectedFinalArtifactCount = nullableInt64Pointer(expectedCount)
	c.ExpectedTranscriptEpoch = nullableInt64Pointer(expectedEpoch)
	c.ExpectedTranscriptSegmentCount = nullableInt64Pointer(expectedSegments)
	c.ExpectedTranscriptLength = nullableInt64Pointer(expectedLength)
	var err error
	if c.ReservedAt, err = sqlitex.ParseTime(reservedAt); err != nil {
		return HistoryCapture{}, err
	}
	if c.UpdatedAt, err = sqlitex.ParseTime(updatedAt); err != nil {
		return HistoryCapture{}, err
	}
	for source, target := range map[*sql.NullString]**time.Time{
		&executionAt: &c.ExecutionRecordedAt, &declaredAt: &c.ExpectedSetDeclaredAt, &lastHint: &c.LastHintAt,
		&runningAt: &c.RunningAt, &quiescingAt: &c.QuiescingAt, &sealedAt: &c.SealedAt,
		&uploadingAt: &c.UploadingAt, &completedAt: &c.CompletedAt, &blockedAt: &c.BlockedAt,
		&lostAt: &c.LostAt, &waivedAt: &c.WaivedAt,
	} {
		if source.Valid {
			parsed, parseErr := sqlitex.ParseTime(source.String)
			if parseErr != nil {
				return HistoryCapture{}, parseErr
			}
			*target = &parsed
		}
	}
	return c, nil
}

func nullableInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	v := value.Int64
	return &v
}

func getHistoryCaptureByAttempt(ctx context.Context, tx *sqlitex.Tx, jobID, leaseID string) (HistoryCapture, error) {
	return scanHistoryCapture(tx.QueryRowContext(ctx, historyCaptureSelect+` WHERE job_id = ? AND lease_id = ?`, jobID, leaseID))
}

func getHistoryCaptureTx(ctx context.Context, tx *sqlitex.Tx, captureID string) (HistoryCapture, error) {
	return scanHistoryCapture(tx.QueryRowContext(ctx, historyCaptureSelect+` WHERE id = ?`, captureID))
}

type TransitionHistoryCaptureInput struct {
	To              HistoryCaptureState
	ExpectedVersion int64
	Actor           string
	ErrorCode       string
	ErrorMessage    string
}

func (s *HistoryCaptureService) Transition(ctx context.Context, captureID string, input TransitionHistoryCaptureInput) (HistoryCapture, error) {
	captureID = strings.TrimSpace(captureID)
	input.Actor = strings.TrimSpace(input.Actor)
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	input.ErrorMessage = strings.TrimSpace(input.ErrorMessage)
	if err := validateHistoryBounded(input.Actor, maxHistoryActorLength, "actor", true); err != nil {
		return HistoryCapture{}, err
	}
	if err := validateHistoryBounded(input.ErrorCode, maxHistoryCodeLength, "error code", false); err != nil {
		return HistoryCapture{}, err
	}
	if err := validateHistoryBounded(input.ErrorMessage, maxHistoryMessageLength, "error message", false); err != nil {
		return HistoryCapture{}, err
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCapture{}, err
	}
	defer tx.Rollback()
	capture, err := getHistoryCaptureTx(ctx, tx, captureID)
	if err != nil {
		return HistoryCapture{}, err
	}
	if capture.Version != input.ExpectedVersion {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	if capture.State == input.To {
		return capture, nil
	}
	if !legalHistoryTransition(capture.State, input.To) {
		return HistoryCapture{}, fmt.Errorf("%w: %s -> %s", ErrHistoryInvalidTransition, capture.State, input.To)
	}
	now := s.now().UTC()
	timestampColumn := map[HistoryCaptureState]string{
		HistoryCaptureRunning: "running_at", HistoryCaptureQuiescing: "quiescing_at",
		HistoryCaptureSealed: "sealed_at", HistoryCaptureUploading: "uploading_at",
		HistoryCaptureBlocked: "blocked_at", HistoryCaptureLost: "lost_at",
	}[input.To]
	if timestampColumn == "" {
		return HistoryCapture{}, fmt.Errorf("%w: use Complete or Waive for terminal state %s", ErrHistoryInvalidTransition, input.To)
	}
	query := fmt.Sprintf(`UPDATE history_captures
SET state = ?, %s = COALESCE(%s, ?), error_code = ?, error_message = ?,
    version = version + 1, updated_at = ?
WHERE id = ? AND version = ?`, timestampColumn, timestampColumn)
	result, err := tx.ExecContext(ctx, query, string(input.To), sqlitex.FormatTime(now), input.ErrorCode, input.ErrorMessage,
		sqlitex.FormatTime(now), captureID, input.ExpectedVersion)
	if err != nil {
		return HistoryCapture{}, fmt.Errorf("transition history capture: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	if err := appendHistoryCaptureEvent(ctx, tx, captureID, "state_transition", string(capture.State), string(input.To), capture.Version+1, input.Actor, input.ErrorCode,
		map[string]any{"message": input.ErrorMessage}, now); err != nil {
		return HistoryCapture{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCapture{}, err
	}
	return s.Get(ctx, captureID)
}

func legalHistoryTransition(from, to HistoryCaptureState) bool {
	normal := map[HistoryCaptureState]HistoryCaptureState{
		HistoryCaptureReserved:  HistoryCaptureRunning,
		HistoryCaptureRunning:   HistoryCaptureQuiescing,
		HistoryCaptureQuiescing: HistoryCaptureSealed,
		HistoryCaptureSealed:    HistoryCaptureUploading,
	}
	if normal[from] == to {
		return true
	}
	if to == HistoryCaptureBlocked || to == HistoryCaptureLost {
		return from != HistoryCaptureComplete && from != HistoryCaptureWaived
	}
	if from == HistoryCaptureBlocked || from == HistoryCaptureLost {
		return to == HistoryCaptureRunning || to == HistoryCaptureQuiescing || to == HistoryCaptureSealed || to == HistoryCaptureUploading
	}
	return false
}

type RecordHistoryExecutionVerdictInput struct {
	Verdict         HistoryExecutionVerdict
	ExitCode        *int
	ErrorCode       string
	ExpectedVersion int64
	Actor           string
}

func (s *HistoryCaptureService) RecordExecutionVerdict(ctx context.Context, captureID string, input RecordHistoryExecutionVerdictInput) (HistoryCapture, error) {
	input.Actor = strings.TrimSpace(input.Actor)
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	if err := validateHistoryBounded(input.Actor, maxHistoryActorLength, "actor", true); err != nil {
		return HistoryCapture{}, err
	}
	if err := validateHistoryBounded(input.ErrorCode, maxHistoryCodeLength, "execution error code", false); err != nil {
		return HistoryCapture{}, err
	}
	if input.Verdict != HistoryExecutionSucceeded && input.Verdict != HistoryExecutionFailed && input.Verdict != HistoryExecutionCancelled && input.Verdict != HistoryExecutionCrashed {
		return HistoryCapture{}, errors.New("invalid history execution verdict")
	}
	if input.Verdict == HistoryExecutionSucceeded && input.ExitCode != nil && *input.ExitCode != 0 {
		return HistoryCapture{}, errors.New("successful execution cannot have a nonzero exit code")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCapture{}, err
	}
	defer tx.Rollback()
	capture, err := getHistoryCaptureTx(ctx, tx, strings.TrimSpace(captureID))
	if err != nil {
		return HistoryCapture{}, err
	}
	if capture.ExecutionVerdict != HistoryExecutionPending {
		if capture.ExecutionVerdict == input.Verdict && sameOptionalInt(capture.ExecutionExitCode, input.ExitCode) && capture.ExecutionErrorCode == input.ErrorCode {
			return capture, nil
		}
		return HistoryCapture{}, fmt.Errorf("%w: execution verdict already recorded", ErrHistoryConflict)
	}
	if capture.Version != input.ExpectedVersion {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	now := s.now().UTC()
	_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET execution_verdict = ?, execution_exit_code = ?, execution_error_code = ?,
    execution_recorded_at = ?, version = version + 1, updated_at = ?
WHERE id = ? AND version = ?`, string(input.Verdict), optionalIntValue(input.ExitCode), input.ErrorCode,
		sqlitex.FormatTime(now), sqlitex.FormatTime(now), capture.ID, input.ExpectedVersion)
	if err != nil {
		return HistoryCapture{}, err
	}
	if err := appendHistoryCaptureEvent(ctx, tx, capture.ID, "execution_verdict_recorded", string(capture.State), string(capture.State), capture.Version+1,
		input.Actor, input.ErrorCode, map[string]any{"verdict": input.Verdict, "exit_code": input.ExitCode}, now); err != nil {
		return HistoryCapture{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCapture{}, err
	}
	return s.Get(ctx, capture.ID)
}

func sameOptionalInt(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
func optionalIntValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *HistoryCaptureService) MarkBlocked(ctx context.Context, captureID string, expectedVersion int64, actor, code, message string) (HistoryCapture, error) {
	return s.Transition(ctx, captureID, TransitionHistoryCaptureInput{To: HistoryCaptureBlocked, ExpectedVersion: expectedVersion, Actor: actor, ErrorCode: code, ErrorMessage: message})
}
func (s *HistoryCaptureService) MarkLost(ctx context.Context, captureID string, expectedVersion int64, actor, code, message string) (HistoryCapture, error) {
	return s.Transition(ctx, captureID, TransitionHistoryCaptureInput{To: HistoryCaptureLost, ExpectedVersion: expectedVersion, Actor: actor, ErrorCode: code, ErrorMessage: message})
}

func (s *HistoryCaptureService) Waive(ctx context.Context, captureID string, expectedVersion int64, actor, reason string) (HistoryCapture, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if err := validateHistoryBounded(actor, maxHistoryActorLength, "actor", true); err != nil {
		return HistoryCapture{}, err
	}
	if err := validateHistoryBounded(reason, maxHistoryMessageLength, "waiver reason", true); err != nil {
		return HistoryCapture{}, err
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCapture{}, err
	}
	defer tx.Rollback()
	capture, err := getHistoryCaptureTx(ctx, tx, strings.TrimSpace(captureID))
	if err != nil {
		return HistoryCapture{}, err
	}
	if capture.State == HistoryCaptureWaived {
		return capture, nil
	}
	if capture.State == HistoryCaptureComplete {
		return HistoryCapture{}, ErrHistoryInvalidTransition
	}
	if capture.Version != expectedVersion {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	now := s.now().UTC()
	_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET state = 'waived', waived_at = ?, waiver_reason = ?, upload_grant_revoked_at = ?,
    version = version + 1, updated_at = ?
WHERE id = ? AND version = ?`, sqlitex.FormatTime(now), reason, sqlitex.FormatTime(now), sqlitex.FormatTime(now), capture.ID, expectedVersion)
	if err != nil {
		return HistoryCapture{}, err
	}
	if err := appendHistoryCaptureEvent(ctx, tx, capture.ID, "waived", string(capture.State), string(HistoryCaptureWaived), capture.Version+1,
		actor, "", map[string]any{"reason": reason}, now); err != nil {
		return HistoryCapture{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCapture{}, err
	}
	return s.Get(ctx, capture.ID)
}

// HistoryArtifact is safe for authorized API projections. InternalHistoryArtifact
// is intentionally separate because BlobKey and TemporaryUploadID are coordinator-only.
type HistoryArtifact struct {
	ID                     string
	CaptureID              string
	LogicalKey             string
	Kind                   HistoryArtifactKind
	Phase                  HistoryArtifactPhase
	CheckpointGeneration   int64
	CheckpointTrigger      string
	ArchiveID              string
	MediaType              string
	FormatVersion          int
	SchemaVersion          int
	SHA256                 string
	StoredSize             int64
	LogicalSize            int64
	EntryCount             int64
	PublicationState       HistoryPublicationState
	SupersededByArtifactID string
	PendingAt              time.Time
	CommittedAt            *time.Time
	CreatedAt              time.Time
}

type InternalHistoryArtifact struct {
	HistoryArtifact
	TemporaryUploadID string
	BlobKey           blob.Key
}

type PublishHistoryArtifactInput struct {
	LogicalKey           string
	Kind                 HistoryArtifactKind
	Phase                HistoryArtifactPhase
	CheckpointGeneration int64
	CheckpointTrigger    string
	ArchiveID            string
	MediaType            string
	FormatVersion        int
	SchemaVersion        int
	LogicalSize          int64
	EntryCount           int64
}

func (s *HistoryCaptureService) BeginUpload(ctx context.Context, captureID, grant string) (blob.Upload, error) {
	if s.blobs == nil {
		return nil, errors.New("history blob store is not configured")
	}
	if err := s.AuthenticateUploadGrant(ctx, captureID, grant); err != nil {
		return nil, err
	}
	return s.blobs.Begin(ctx)
}

func (s *HistoryCaptureService) PublishArtifact(ctx context.Context, captureID, grant string, input PublishHistoryArtifactInput, temporary blob.Temporary) (HistoryArtifact, error) {
	if s.blobs == nil {
		return HistoryArtifact{}, errors.New("history blob store is not configured")
	}
	input = normalizePublishHistoryArtifactInput(input)
	if err := validatePublishHistoryArtifactInput(input); err != nil {
		return HistoryArtifact{}, err
	}
	canonical, err := s.blobs.Resume(ctx, temporary.ID)
	if err != nil {
		// A lost response can mean the temporary was already consumed. Existing
		// pending metadata plus Head is authoritative in that case.
		if !errors.Is(err, blob.ErrNotFound) {
			return HistoryArtifact{}, fmt.Errorf("inspect history temporary upload: %w", err)
		}
		canonical = temporary
	}
	if canonical.ID != temporary.ID || canonical.Digest != temporary.Digest || canonical.Size != temporary.Size {
		return HistoryArtifact{}, fmt.Errorf("%w: temporary upload metadata differs", ErrHistoryConflict)
	}
	internal, _, err := s.ensurePendingArtifact(ctx, strings.TrimSpace(captureID), grant, input, canonical)
	if err != nil {
		return HistoryArtifact{}, err
	}
	committed, err := s.finishArtifactPublication(ctx, internal, false)
	if err != nil {
		return HistoryArtifact{}, err
	}
	return committed.HistoryArtifact, nil
}

func normalizePublishHistoryArtifactInput(input PublishHistoryArtifactInput) PublishHistoryArtifactInput {
	input.LogicalKey = strings.TrimSpace(input.LogicalKey)
	input.CheckpointTrigger = strings.TrimSpace(input.CheckpointTrigger)
	input.ArchiveID = strings.TrimSpace(input.ArchiveID)
	input.MediaType = strings.TrimSpace(input.MediaType)
	if input.FormatVersion == 0 {
		input.FormatVersion = 1
	}
	if input.SchemaVersion == 0 {
		input.SchemaVersion = 1
	}
	return input
}

func validatePublishHistoryArtifactInput(input PublishHistoryArtifactInput) error {
	if input.LogicalKey == "" || len(input.LogicalKey) > maxHistoryLogicalKey {
		return errors.New("history artifact logical key is required and must be bounded")
	}
	if input.MediaType == "" || len(input.MediaType) > 255 {
		return errors.New("history artifact media type is required and must be bounded")
	}
	if input.Kind != HistoryArtifactTranscriptSegment && input.Kind != HistoryArtifactHarnessRoot && input.Kind != HistoryArtifactManifest {
		return errors.New("invalid history artifact kind")
	}
	if input.Phase != HistoryArtifactCheckpoint && input.Phase != HistoryArtifactFinal {
		return errors.New("invalid history artifact phase")
	}
	if input.Phase == HistoryArtifactCheckpoint {
		if input.CheckpointGeneration < 1 {
			return errors.New("checkpoint generation must be positive")
		}
		if input.Kind == HistoryArtifactTranscriptSegment {
			return errors.New("transcript segments cannot be checkpoint artifacts")
		}
	} else if input.CheckpointGeneration != 0 || input.CheckpointTrigger != "" {
		return errors.New("final artifacts cannot carry checkpoint metadata")
	}
	if input.FormatVersion < 1 || input.SchemaVersion < 1 || input.LogicalSize < 0 || input.EntryCount < 0 {
		return errors.New("invalid history artifact size/version metadata")
	}
	if len(input.CheckpointTrigger) > maxHistoryCodeLength || len(input.ArchiveID) > 255 {
		return errors.New("history artifact metadata exceeds its storage bound")
	}
	return nil
}

func (s *HistoryCaptureService) ensurePendingArtifact(ctx context.Context, captureID, grant string, input PublishHistoryArtifactInput, temporary blob.Temporary) (InternalHistoryArtifact, bool, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, captureID, grant, false)
	if err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	existing, err := getInternalHistoryArtifactTx(ctx, tx, captureID, input.LogicalKey)
	if err == nil {
		if !sameArtifactDeclaration(existing, input, temporary) {
			_ = appendHistoryUploadEvent(ctx, tx, captureID, existing.ID, input.LogicalKey, "conflict", "immutable_metadata", s.now().UTC())
			_ = tx.Commit(ctx)
			return InternalHistoryArtifact{}, false, fmt.Errorf("%w: logical key %q already has different metadata", ErrHistoryConflict, input.LogicalKey)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrHistoryArtifactNotFound) {
		return InternalHistoryArtifact{}, false, err
	}
	artifactID, err := randomHistoryID("ha")
	if err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	key, err := blob.NewKey("history/" + capture.ProjectID + "/" + capture.ID)
	if err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	var generation any
	if input.Phase == HistoryArtifactCheckpoint {
		generation = input.CheckpointGeneration
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_artifacts (
    id, capture_id, logical_key, kind, phase, checkpoint_generation,
    checkpoint_trigger, archive_id, media_type, format_version, schema_version,
    sha256, stored_size, logical_size, entry_count, publication_state,
    temporary_upload_id, blob_key, pending_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		artifactID, captureID, input.LogicalKey, string(input.Kind), string(input.Phase), generation,
		input.CheckpointTrigger, input.ArchiveID, input.MediaType, input.FormatVersion, input.SchemaVersion,
		temporary.Digest.String(), temporary.Size, input.LogicalSize, input.EntryCount,
		temporary.ID, key.String(), nowText, nowText)
	if err != nil {
		return InternalHistoryArtifact{}, false, fmt.Errorf("insert pending history artifact: %w", err)
	}
	if input.Phase == HistoryArtifactCheckpoint {
		_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET last_checkpoint_attempt_generation = MAX(last_checkpoint_attempt_generation, ?), updated_at = ?
WHERE id = ?`, input.CheckpointGeneration, nowText, captureID)
		if err != nil {
			return InternalHistoryArtifact{}, false, err
		}
	}
	if err := appendHistoryUploadEvent(ctx, tx, captureID, artifactID, input.LogicalKey, "pending", "", now); err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalHistoryArtifact{}, false, err
	}
	artifact, err := s.getInternalArtifact(ctx, captureID, input.LogicalKey)
	return artifact, true, err
}

func sameArtifactDeclaration(existing InternalHistoryArtifact, input PublishHistoryArtifactInput, temporary blob.Temporary) bool {
	return existing.SHA256 == temporary.Digest.String() && existing.StoredSize == temporary.Size &&
		existing.Kind == input.Kind && existing.Phase == input.Phase && existing.CheckpointGeneration == input.CheckpointGeneration &&
		existing.CheckpointTrigger == input.CheckpointTrigger && existing.ArchiveID == input.ArchiveID && existing.MediaType == input.MediaType &&
		existing.FormatVersion == input.FormatVersion && existing.SchemaVersion == input.SchemaVersion &&
		existing.LogicalSize == input.LogicalSize && existing.EntryCount == input.EntryCount
}

func (s *HistoryCaptureService) ReconcileArtifact(ctx context.Context, captureID, grant, logicalKey string) (HistoryArtifact, error) {
	if err := s.AuthenticateUploadGrant(ctx, captureID, grant); err != nil {
		return HistoryArtifact{}, err
	}
	internal, err := s.getInternalArtifact(ctx, strings.TrimSpace(captureID), strings.TrimSpace(logicalKey))
	if err != nil {
		return HistoryArtifact{}, err
	}
	if internal.PublicationState == HistoryPublicationCommitted {
		return internal.HistoryArtifact, nil
	}
	committed, err := s.finishArtifactPublication(ctx, internal, true)
	if err != nil {
		return HistoryArtifact{}, err
	}
	return committed.HistoryArtifact, nil
}

func (s *HistoryCaptureService) finishArtifactPublication(ctx context.Context, artifact InternalHistoryArtifact, reconciliation bool) (InternalHistoryArtifact, error) {
	if artifact.PublicationState == HistoryPublicationCommitted {
		return artifact, nil
	}
	object, err := s.blobs.Head(ctx, artifact.BlobKey)
	if err != nil && !errors.Is(err, blob.ErrNotFound) {
		return InternalHistoryArtifact{}, err
	}
	if errors.Is(err, blob.ErrNotFound) {
		temporary, resumeErr := s.blobs.Resume(ctx, artifact.TemporaryUploadID)
		if resumeErr != nil {
			return InternalHistoryArtifact{}, fmt.Errorf("%w: resume pending artifact: %v", ErrHistoryPublicationPending, resumeErr)
		}
		if temporary.Digest.String() != artifact.SHA256 || temporary.Size != artifact.StoredSize {
			return InternalHistoryArtifact{}, fmt.Errorf("%w: pending temporary metadata differs", ErrHistoryConflict)
		}
		object, err = s.blobs.Publish(ctx, temporary, artifact.BlobKey)
		if err != nil {
			_ = s.recordUploadFailure(ctx, artifact, historyBlobErrorCode(err))
			return InternalHistoryArtifact{}, fmt.Errorf("publish history artifact: %w", err)
		}
	}
	if object.Digest.String() != artifact.SHA256 || object.Size != artifact.StoredSize {
		_ = s.recordUploadFailure(ctx, artifact, "published_metadata_conflict")
		return InternalHistoryArtifact{}, fmt.Errorf("%w: published object metadata differs", ErrHistoryConflict)
	}
	return s.commitArtifactPublication(ctx, artifact, reconciliation)
}

func (s *HistoryCaptureService) commitArtifactPublication(ctx context.Context, artifact InternalHistoryArtifact, reconciliation bool) (InternalHistoryArtifact, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return InternalHistoryArtifact{}, err
	}
	defer tx.Rollback()
	current, err := getInternalHistoryArtifactByIDTx(ctx, tx, artifact.ID)
	if err != nil {
		return InternalHistoryArtifact{}, err
	}
	if current.PublicationState == HistoryPublicationCommitted {
		return current, nil
	}
	if current.SHA256 != artifact.SHA256 || current.StoredSize != artifact.StoredSize || current.BlobKey != artifact.BlobKey {
		return InternalHistoryArtifact{}, ErrHistoryConflict
	}
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	_, err = tx.ExecContext(ctx, `
UPDATE history_artifacts
SET publication_state = 'committed', committed_at = ?
WHERE id = ? AND publication_state = 'pending'`, nowText, artifact.ID)
	if err != nil {
		return InternalHistoryArtifact{}, err
	}
	if artifact.Phase == HistoryArtifactCheckpoint {
		_, err = tx.ExecContext(ctx, `
UPDATE history_artifacts
SET superseded_by_artifact_id = ?, superseded_at = ?
WHERE capture_id = ? AND phase = 'checkpoint' AND kind = ?
  AND checkpoint_generation < ? AND publication_state = 'committed'
  AND superseded_by_artifact_id IS NULL`, artifact.ID, nowText, artifact.CaptureID, string(artifact.Kind), artifact.CheckpointGeneration)
		if err != nil {
			return InternalHistoryArtifact{}, err
		}
		_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET last_checkpoint_committed_generation = MAX(last_checkpoint_committed_generation, ?),
    last_checkpoint_bytes = CASE WHEN ? >= last_checkpoint_committed_generation THEN ? ELSE last_checkpoint_bytes END,
    last_checkpoint_entries = CASE WHEN ? >= last_checkpoint_committed_generation THEN ? ELSE last_checkpoint_entries END,
    updated_at = ?
WHERE id = ?`, artifact.CheckpointGeneration, artifact.CheckpointGeneration, artifact.StoredSize,
			artifact.CheckpointGeneration, artifact.EntryCount, nowText, artifact.CaptureID)
		if err != nil {
			return InternalHistoryArtifact{}, err
		}
	}
	eventKind := "committed"
	if reconciliation {
		eventKind = "reconciled"
	}
	if err := appendHistoryUploadEvent(ctx, tx, artifact.CaptureID, artifact.ID, artifact.LogicalKey, eventKind, "", now); err != nil {
		return InternalHistoryArtifact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalHistoryArtifact{}, err
	}
	return s.getInternalArtifact(ctx, artifact.CaptureID, artifact.LogicalKey)
}

func (s *HistoryCaptureService) recordUploadFailure(ctx context.Context, artifact InternalHistoryArtifact, code string) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendHistoryUploadEvent(ctx, tx, artifact.CaptureID, artifact.ID, artifact.LogicalKey, "failed", code, s.now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func historyBlobErrorCode(err error) string {
	switch {
	case errors.Is(err, blob.ErrConflict):
		return "blob_conflict"
	case errors.Is(err, blob.ErrChecksumMismatch):
		return "checksum_mismatch"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "blob_publish_failed"
	}
}

func (s *HistoryCaptureService) getInternalArtifact(ctx context.Context, captureID, logicalKey string) (InternalHistoryArtifact, error) {
	return scanInternalHistoryArtifact(s.db.QueryRowContext(ctx, historyArtifactSelect+` WHERE capture_id = ? AND logical_key = ?`, captureID, logicalKey))
}

func (s *HistoryCaptureService) GetArtifact(ctx context.Context, captureID, logicalKey string) (HistoryArtifact, error) {
	artifact, err := s.getInternalArtifact(ctx, strings.TrimSpace(captureID), strings.TrimSpace(logicalKey))
	return artifact.HistoryArtifact, err
}

const historyArtifactSelect = `
SELECT id, capture_id, logical_key, kind, phase, COALESCE(checkpoint_generation, 0),
       checkpoint_trigger, archive_id, media_type, format_version, schema_version,
       sha256, stored_size, logical_size, entry_count, publication_state,
       temporary_upload_id, blob_key, COALESCE(superseded_by_artifact_id, ''),
       pending_at, committed_at, created_at
FROM history_artifacts`

func scanInternalHistoryArtifact(row historyRowScanner) (InternalHistoryArtifact, error) {
	var a InternalHistoryArtifact
	var kind, phase, publication, key, pendingAt, createdAt string
	var committedAt sql.NullString
	if err := row.Scan(&a.ID, &a.CaptureID, &a.LogicalKey, &kind, &phase, &a.CheckpointGeneration,
		&a.CheckpointTrigger, &a.ArchiveID, &a.MediaType, &a.FormatVersion, &a.SchemaVersion,
		&a.SHA256, &a.StoredSize, &a.LogicalSize, &a.EntryCount, &publication,
		&a.TemporaryUploadID, &key, &a.SupersededByArtifactID, &pendingAt, &committedAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InternalHistoryArtifact{}, ErrHistoryArtifactNotFound
		}
		return InternalHistoryArtifact{}, fmt.Errorf("scan history artifact: %w", err)
	}
	a.Kind, a.Phase, a.PublicationState = HistoryArtifactKind(kind), HistoryArtifactPhase(phase), HistoryPublicationState(publication)
	parsedKey, err := blob.ParseKey(key)
	if err != nil {
		return InternalHistoryArtifact{}, err
	}
	a.BlobKey = parsedKey
	if a.PendingAt, err = sqlitex.ParseTime(pendingAt); err != nil {
		return InternalHistoryArtifact{}, err
	}
	if a.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
		return InternalHistoryArtifact{}, err
	}
	if committedAt.Valid {
		value, parseErr := sqlitex.ParseTime(committedAt.String)
		if parseErr != nil {
			return InternalHistoryArtifact{}, parseErr
		}
		a.CommittedAt = &value
	}
	return a, nil
}

func getInternalHistoryArtifactTx(ctx context.Context, tx *sqlitex.Tx, captureID, logicalKey string) (InternalHistoryArtifact, error) {
	return scanInternalHistoryArtifact(tx.QueryRowContext(ctx, historyArtifactSelect+` WHERE capture_id = ? AND logical_key = ?`, captureID, logicalKey))
}
func getInternalHistoryArtifactByIDTx(ctx context.Context, tx *sqlitex.Tx, artifactID string) (InternalHistoryArtifact, error) {
	return scanInternalHistoryArtifact(tx.QueryRowContext(ctx, historyArtifactSelect+` WHERE id = ?`, artifactID))
}

type RegisterTranscriptSegmentInput struct {
	ArtifactLogicalKey string
	Epoch              int64
	Sequence           int64
	StartOffset        int64
	EndOffset          int64
	Encoding           string
}

type HistoryTranscriptSegment struct {
	CaptureID        string
	Epoch            int64
	Sequence         int64
	StartOffset      int64
	EndOffset        int64
	UncompressedSize int64
	StoredSize       int64
	SHA256           string
	Encoding         string
	ArtifactID       string
	SealedAt         time.Time
}

func (s *HistoryCaptureService) RegisterTranscriptSegment(ctx context.Context, captureID, grant string, input RegisterTranscriptSegmentInput) (HistoryTranscriptSegment, error) {
	input.ArtifactLogicalKey = strings.TrimSpace(input.ArtifactLogicalKey)
	input.Encoding = strings.TrimSpace(input.Encoding)
	if input.ArtifactLogicalKey == "" || input.Epoch < 0 || input.Sequence < 0 || input.StartOffset < 0 || input.EndOffset <= input.StartOffset || (input.Encoding != "identity" && input.Encoding != "gzip") {
		return HistoryTranscriptSegment{}, errors.New("invalid transcript segment metadata")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryTranscriptSegment{}, err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, false)
	if err != nil {
		return HistoryTranscriptSegment{}, err
	}
	if !capture.ExpectedTranscript {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: capture does not expect a transcript", ErrHistoryConflict)
	}
	existing, err := getTranscriptSegmentTx(ctx, tx, capture.ID, input.Epoch, input.Sequence)
	if err == nil {
		artifact, artifactErr := getInternalHistoryArtifactTx(ctx, tx, capture.ID, input.ArtifactLogicalKey)
		if artifactErr == nil && sameTranscriptSegment(existing, input, artifact) {
			return existing, nil
		}
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript epoch/sequence already registered", ErrHistoryConflict)
	}
	if !errors.Is(err, ErrHistoryArtifactNotFound) {
		return HistoryTranscriptSegment{}, err
	}
	artifact, err := getInternalHistoryArtifactTx(ctx, tx, capture.ID, input.ArtifactLogicalKey)
	if err != nil {
		return HistoryTranscriptSegment{}, err
	}
	if artifact.Kind != HistoryArtifactTranscriptSegment || artifact.Phase != HistoryArtifactFinal || artifact.PublicationState != HistoryPublicationCommitted {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript artifact must be committed final segment", ErrHistoryConflict)
	}
	uncompressed := input.EndOffset - input.StartOffset
	if artifact.StoredSize < 0 || artifact.LogicalSize != uncompressed {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript artifact sizes differ", ErrHistoryConflict)
	}
	var streamState string
	var count, length int64
	var lastEpoch, lastSequence sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT state, segment_count, logical_length, last_epoch, last_sequence
FROM history_transcript_streams WHERE capture_id = ?`, capture.ID).Scan(&streamState, &count, &length, &lastEpoch, &lastSequence); err != nil {
		return HistoryTranscriptSegment{}, err
	}
	if streamState != "open" {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript is sealed", ErrHistoryConflict)
	}
	if input.StartOffset != length {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript offset is not contiguous", ErrHistoryConflict)
	}
	if count == 0 {
		if input.Epoch != 0 || input.Sequence != 0 {
			return HistoryTranscriptSegment{}, fmt.Errorf("%w: first transcript segment must be epoch 0 sequence 0", ErrHistoryConflict)
		}
	} else if input.Epoch == lastEpoch.Int64 {
		if input.Sequence != lastSequence.Int64+1 {
			return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript sequence is not contiguous", ErrHistoryConflict)
		}
	} else if input.Epoch == lastEpoch.Int64+1 {
		if input.Sequence != 0 {
			return HistoryTranscriptSegment{}, fmt.Errorf("%w: a new transcript epoch must start at sequence 0", ErrHistoryConflict)
		}
	} else {
		return HistoryTranscriptSegment{}, fmt.Errorf("%w: transcript epoch is not contiguous", ErrHistoryConflict)
	}
	now := s.now().UTC()
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_transcript_segments (
    capture_id, epoch, sequence, start_offset, end_offset, uncompressed_size,
    stored_size, sha256, encoding, artifact_id, sealed_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, capture.ID, input.Epoch, input.Sequence,
		input.StartOffset, input.EndOffset, uncompressed, artifact.StoredSize, artifact.SHA256,
		input.Encoding, artifact.ID, sqlitex.FormatTime(now), sqlitex.FormatTime(now))
	if err != nil {
		return HistoryTranscriptSegment{}, fmt.Errorf("register transcript segment: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE history_transcript_streams
SET segment_count = segment_count + 1, logical_length = ?, last_epoch = ?, last_sequence = ?, updated_at = ?
WHERE capture_id = ? AND state = 'open'`, input.EndOffset, input.Epoch, input.Sequence, sqlitex.FormatTime(now), capture.ID)
	if err != nil {
		return HistoryTranscriptSegment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryTranscriptSegment{}, err
	}
	return s.getTranscriptSegment(ctx, capture.ID, input.Epoch, input.Sequence)
}

func sameTranscriptSegment(existing HistoryTranscriptSegment, input RegisterTranscriptSegmentInput, artifact InternalHistoryArtifact) bool {
	return existing.StartOffset == input.StartOffset && existing.EndOffset == input.EndOffset && existing.Encoding == input.Encoding && existing.ArtifactID == artifact.ID && existing.SHA256 == artifact.SHA256 && existing.StoredSize == artifact.StoredSize
}

const transcriptSegmentSelect = `
SELECT capture_id, epoch, sequence, start_offset, end_offset, uncompressed_size,
       stored_size, sha256, encoding, artifact_id, sealed_at
FROM history_transcript_segments`

func scanTranscriptSegment(row historyRowScanner) (HistoryTranscriptSegment, error) {
	var segment HistoryTranscriptSegment
	var sealedAt string
	if err := row.Scan(&segment.CaptureID, &segment.Epoch, &segment.Sequence, &segment.StartOffset,
		&segment.EndOffset, &segment.UncompressedSize, &segment.StoredSize, &segment.SHA256,
		&segment.Encoding, &segment.ArtifactID, &sealedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryTranscriptSegment{}, ErrHistoryArtifactNotFound
		}
		return HistoryTranscriptSegment{}, err
	}
	parsed, err := sqlitex.ParseTime(sealedAt)
	segment.SealedAt = parsed
	return segment, err
}
func getTranscriptSegmentTx(ctx context.Context, tx *sqlitex.Tx, captureID string, epoch, sequence int64) (HistoryTranscriptSegment, error) {
	return scanTranscriptSegment(tx.QueryRowContext(ctx, transcriptSegmentSelect+` WHERE capture_id = ? AND epoch = ? AND sequence = ?`, captureID, epoch, sequence))
}
func (s *HistoryCaptureService) getTranscriptSegment(ctx context.Context, captureID string, epoch, sequence int64) (HistoryTranscriptSegment, error) {
	return scanTranscriptSegment(s.db.QueryRowContext(ctx, transcriptSegmentSelect+` WHERE capture_id = ? AND epoch = ? AND sequence = ?`, captureID, epoch, sequence))
}

type TranscriptSeal struct {
	FinalEpoch    int64 // -1 only for an empty stream
	SegmentCount  int64
	LogicalLength int64
	SHA256        string
}

func (s *HistoryCaptureService) SealTranscript(ctx context.Context, captureID, grant string, seal TranscriptSeal) error {
	seal.SHA256 = strings.TrimSpace(seal.SHA256)
	if seal.SegmentCount < 0 || seal.LogicalLength < 0 || len(seal.SHA256) != 64 || !isLowerHexHistory(seal.SHA256) {
		return errors.New("invalid transcript seal")
	}
	if seal.SegmentCount == 0 && (seal.FinalEpoch != -1 || seal.LogicalLength != 0) {
		return errors.New("empty transcript seal must use final epoch -1 and length 0")
	}
	if seal.SegmentCount > 0 && seal.FinalEpoch < 0 {
		return errors.New("nonempty transcript seal needs a final epoch")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, false)
	if err != nil {
		return err
	}
	if !capture.ExpectedTranscript {
		return ErrHistoryConflict
	}
	var state string
	var count, length int64
	var lastEpoch sql.NullInt64
	var digest sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT state, segment_count, logical_length, last_epoch, stream_sha256
FROM history_transcript_streams WHERE capture_id = ?`, capture.ID).Scan(&state, &count, &length, &lastEpoch, &digest); err != nil {
		return err
	}
	if state == "sealed" {
		actualEpoch := int64(-1)
		if lastEpoch.Valid {
			actualEpoch = lastEpoch.Int64
		}
		if count == seal.SegmentCount && length == seal.LogicalLength && actualEpoch == seal.FinalEpoch && digest.String == seal.SHA256 {
			return nil
		}
		return fmt.Errorf("%w: transcript already sealed differently", ErrHistoryConflict)
	}
	actualEpoch := int64(-1)
	if lastEpoch.Valid {
		actualEpoch = lastEpoch.Int64
	}
	if count != seal.SegmentCount || length != seal.LogicalLength || actualEpoch != seal.FinalEpoch {
		return fmt.Errorf("%w: transcript seal does not match registered segments", ErrHistoryIncomplete)
	}
	now := sqlitex.FormatTime(s.now().UTC())
	_, err = tx.ExecContext(ctx, `
UPDATE history_transcript_streams
SET state = 'sealed', stream_sha256 = ?, sealed_at = ?, updated_at = ?
WHERE capture_id = ? AND state = 'open'`, seal.SHA256, now, now, capture.ID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type FinalArtifactExpectation struct {
	LogicalKey string
	Kind       HistoryArtifactKind
}

type DeclareHistoryExpectedSetInput struct {
	Artifacts       []FinalArtifactExpectation
	TranscriptSeal  *TranscriptSeal
	ExpectedVersion int64
	Actor           string
}

func (s *HistoryCaptureService) DeclareExpectedSet(ctx context.Context, captureID, grant string, input DeclareHistoryExpectedSetInput) (HistoryCapture, error) {
	input.Actor = strings.TrimSpace(input.Actor)
	if err := validateHistoryBounded(input.Actor, maxHistoryActorLength, "actor", true); err != nil {
		return HistoryCapture{}, err
	}
	expectations, err := normalizeFinalExpectations(input.Artifacts)
	if err != nil {
		return HistoryCapture{}, err
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCapture{}, err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, false)
	if err != nil {
		return HistoryCapture{}, err
	}
	if capture.ExpectedSetDeclaredAt != nil {
		matches, compareErr := expectedSetMatchesTx(ctx, tx, capture, expectations, input.TranscriptSeal)
		if compareErr != nil {
			return HistoryCapture{}, compareErr
		}
		if matches {
			return capture, nil
		}
		return HistoryCapture{}, fmt.Errorf("%w: final expected set already declared differently", ErrHistoryConflict)
	}
	if capture.Version != input.ExpectedVersion {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	if capture.State != HistoryCaptureQuiescing && capture.State != HistoryCaptureSealed && capture.State != HistoryCaptureUploading && capture.State != HistoryCaptureBlocked {
		return HistoryCapture{}, fmt.Errorf("%w: expected set cannot be declared from %s", ErrHistoryInvalidTransition, capture.State)
	}
	if capture.ExpectedTranscript {
		if input.TranscriptSeal == nil {
			return HistoryCapture{}, errors.New("expected transcript seal is required")
		}
		if err := verifyTranscriptSealTx(ctx, tx, capture.ID, *input.TranscriptSeal); err != nil {
			return HistoryCapture{}, err
		}
	} else if input.TranscriptSeal != nil {
		return HistoryCapture{}, fmt.Errorf("%w: capture does not expect a transcript", ErrHistoryConflict)
	}
	harnessCount := 0
	for _, expected := range expectations {
		if expected.Kind == HistoryArtifactHarnessRoot {
			harnessCount++
		}
	}
	if !capture.ExpectedHarness && harnessCount != 0 {
		return HistoryCapture{}, fmt.Errorf("%w: capture does not expect Harness roots", ErrHistoryConflict)
	}
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	for _, expected := range expectations {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO history_capture_expected_artifacts (capture_id, logical_key, kind, created_at)
VALUES (?, ?, ?, ?)`, capture.ID, expected.LogicalKey, string(expected.Kind), nowText); err != nil {
			return HistoryCapture{}, err
		}
	}
	var epoch, segments, length, digest any
	if input.TranscriptSeal != nil {
		if input.TranscriptSeal.FinalEpoch >= 0 {
			epoch = input.TranscriptSeal.FinalEpoch
		}
		segments, length, digest = input.TranscriptSeal.SegmentCount, input.TranscriptSeal.LogicalLength, strings.TrimSpace(input.TranscriptSeal.SHA256)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET expected_set_declared_at = ?, expected_final_artifact_count = ?,
    expected_transcript_epoch = ?, expected_transcript_segment_count = ?,
    expected_transcript_length = ?, expected_transcript_sha256 = ?,
    version = version + 1, updated_at = ?
WHERE id = ? AND version = ?`, nowText, len(expectations), epoch, segments, length, digest, nowText, capture.ID, input.ExpectedVersion)
	if err != nil {
		return HistoryCapture{}, err
	}
	if err := appendHistoryCaptureEvent(ctx, tx, capture.ID, "expected_set_declared", string(capture.State), string(capture.State), capture.Version+1,
		input.Actor, "", map[string]any{"artifact_count": len(expectations), "transcript_segments": segments}, now); err != nil {
		return HistoryCapture{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCapture{}, err
	}
	return s.Get(ctx, capture.ID)
}

func normalizeFinalExpectations(values []FinalArtifactExpectation) ([]FinalArtifactExpectation, error) {
	result := append([]FinalArtifactExpectation(nil), values...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		result[index].LogicalKey = strings.TrimSpace(result[index].LogicalKey)
		if result[index].LogicalKey == "" || len(result[index].LogicalKey) > maxHistoryLogicalKey {
			return nil, errors.New("final artifact logical key is required and must be bounded")
		}
		if result[index].Kind != HistoryArtifactHarnessRoot && result[index].Kind != HistoryArtifactManifest {
			return nil, errors.New("only final Harness roots and manifests belong in the declared expected set")
		}
		if _, ok := seen[result[index].LogicalKey]; ok {
			return nil, errors.New("duplicate final artifact logical key")
		}
		seen[result[index].LogicalKey] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LogicalKey < result[j].LogicalKey })
	return result, nil
}

func verifyTranscriptSealTx(ctx context.Context, tx *sqlitex.Tx, captureID string, seal TranscriptSeal) error {
	var state, digest string
	var count, length int64
	var epoch sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT state, segment_count, logical_length, last_epoch, COALESCE(stream_sha256, '')
FROM history_transcript_streams WHERE capture_id = ?`, captureID).Scan(&state, &count, &length, &epoch, &digest); err != nil {
		return err
	}
	actualEpoch := int64(-1)
	if epoch.Valid {
		actualEpoch = epoch.Int64
	}
	if state != "sealed" || count != seal.SegmentCount || length != seal.LogicalLength || actualEpoch != seal.FinalEpoch || digest != strings.TrimSpace(seal.SHA256) {
		return fmt.Errorf("%w: transcript seal differs from the sealed stream", ErrHistoryIncomplete)
	}
	return nil
}

func expectedSetMatchesTx(ctx context.Context, tx *sqlitex.Tx, capture HistoryCapture, expected []FinalArtifactExpectation, seal *TranscriptSeal) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT logical_key, kind FROM history_capture_expected_artifacts
WHERE capture_id = ? ORDER BY logical_key`, capture.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var actual []FinalArtifactExpectation
	for rows.Next() {
		var value FinalArtifactExpectation
		if err := rows.Scan(&value.LogicalKey, &value.Kind); err != nil {
			return false, err
		}
		actual = append(actual, value)
	}
	if len(actual) != len(expected) {
		return false, nil
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false, nil
		}
	}
	if capture.ExpectedTranscript {
		if seal == nil || capture.ExpectedTranscriptSegmentCount == nil || capture.ExpectedTranscriptLength == nil {
			return false, nil
		}
		actualEpoch := int64(-1)
		if capture.ExpectedTranscriptEpoch != nil {
			actualEpoch = *capture.ExpectedTranscriptEpoch
		}
		return actualEpoch == seal.FinalEpoch && *capture.ExpectedTranscriptSegmentCount == seal.SegmentCount && *capture.ExpectedTranscriptLength == seal.LogicalLength && capture.ExpectedTranscriptSHA256 == strings.TrimSpace(seal.SHA256), nil
	}
	return seal == nil, nil
}

func (s *HistoryCaptureService) Complete(ctx context.Context, captureID, grant string, expectedVersion int64, actor string) (HistoryCapture, error) {
	actor = strings.TrimSpace(actor)
	if err := validateHistoryBounded(actor, maxHistoryActorLength, "actor", true); err != nil {
		return HistoryCapture{}, err
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCapture{}, err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, true)
	if err != nil {
		return HistoryCapture{}, err
	}
	if capture.State == HistoryCaptureComplete {
		return capture, nil
	}
	if capture.Version != expectedVersion {
		return HistoryCapture{}, ErrHistoryVersionConflict
	}
	if capture.State != HistoryCaptureUploading && capture.State != HistoryCaptureSealed && capture.State != HistoryCaptureBlocked && capture.State != HistoryCaptureLost {
		return HistoryCapture{}, fmt.Errorf("%w: cannot complete from %s", ErrHistoryInvalidTransition, capture.State)
	}
	if capture.ExpectedSetDeclaredAt == nil {
		return HistoryCapture{}, fmt.Errorf("%w: final expected set is not declared", ErrHistoryIncomplete)
	}
	if err := verifyCaptureCompletenessTx(ctx, tx, capture); err != nil {
		return HistoryCapture{}, err
	}
	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET state = 'complete', completed_at = ?, upload_grant_revoked_at = ?,
    error_code = '', error_message = '', version = version + 1, updated_at = ?
WHERE id = ? AND version = ?`, nowText, nowText, nowText, capture.ID, expectedVersion)
	if err != nil {
		return HistoryCapture{}, err
	}
	if err := appendHistoryCaptureEvent(ctx, tx, capture.ID, "completed", string(capture.State), string(HistoryCaptureComplete), capture.Version+1,
		actor, "", nil, now); err != nil {
		return HistoryCapture{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCapture{}, err
	}
	return s.Get(ctx, capture.ID)
}

func verifyCaptureCompletenessTx(ctx context.Context, tx *sqlitex.Tx, capture HistoryCapture) error {
	var missing int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM history_capture_expected_artifacts AS expected
LEFT JOIN history_artifacts AS artifact
  ON artifact.capture_id = expected.capture_id AND artifact.logical_key = expected.logical_key
WHERE expected.capture_id = ?
  AND (artifact.id IS NULL OR artifact.kind != expected.kind OR artifact.phase != 'final'
       OR artifact.publication_state != 'committed')`, capture.ID).Scan(&missing); err != nil {
		return err
	}
	if missing != 0 {
		return fmt.Errorf("%w: %d declared final artifacts are not committed", ErrHistoryIncomplete, missing)
	}
	var unexpected int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM history_artifacts AS artifact
LEFT JOIN history_capture_expected_artifacts AS expected
  ON expected.capture_id = artifact.capture_id AND expected.logical_key = artifact.logical_key
WHERE artifact.capture_id = ? AND artifact.phase = 'final' AND artifact.kind != 'transcript_segment'
  AND (expected.logical_key IS NULL OR expected.kind != artifact.kind)`, capture.ID).Scan(&unexpected); err != nil {
		return err
	}
	if unexpected != 0 {
		return fmt.Errorf("%w: final artifact set contains %d undeclared artifacts", ErrHistoryIncomplete, unexpected)
	}
	var actualExpected int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_capture_expected_artifacts WHERE capture_id = ?`, capture.ID).Scan(&actualExpected); err != nil {
		return err
	}
	if capture.ExpectedFinalArtifactCount == nil || actualExpected != *capture.ExpectedFinalArtifactCount {
		return fmt.Errorf("%w: expected artifact count differs", ErrHistoryIncomplete)
	}
	if capture.ExpectedTranscript {
		if capture.ExpectedTranscriptSegmentCount == nil || capture.ExpectedTranscriptLength == nil {
			return fmt.Errorf("%w: transcript expectation is absent", ErrHistoryIncomplete)
		}
		var state, digest string
		var count, length int64
		var epoch sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
SELECT state, segment_count, logical_length, last_epoch, COALESCE(stream_sha256, '')
FROM history_transcript_streams WHERE capture_id = ?`, capture.ID).Scan(&state, &count, &length, &epoch, &digest); err != nil {
			return err
		}
		actualEpoch := int64(-1)
		if epoch.Valid {
			actualEpoch = epoch.Int64
		}
		expectedEpoch := int64(-1)
		if capture.ExpectedTranscriptEpoch != nil {
			expectedEpoch = *capture.ExpectedTranscriptEpoch
		}
		if state != "sealed" || count != *capture.ExpectedTranscriptSegmentCount || length != *capture.ExpectedTranscriptLength || actualEpoch != expectedEpoch || digest != capture.ExpectedTranscriptSHA256 {
			return fmt.Errorf("%w: transcript stream does not match declared seal", ErrHistoryIncomplete)
		}
		var uncommittedSegments int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM history_transcript_segments AS segment
JOIN history_artifacts AS artifact ON artifact.id = segment.artifact_id
WHERE segment.capture_id = ? AND artifact.publication_state != 'committed'`, capture.ID).Scan(&uncommittedSegments); err != nil {
			return err
		}
		if uncommittedSegments != 0 {
			return fmt.Errorf("%w: transcript has uncommitted segment artifacts", ErrHistoryIncomplete)
		}
	} else {
		var segments int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_transcript_segments WHERE capture_id = ?`, capture.ID).Scan(&segments); err != nil {
			return err
		}
		if segments != 0 {
			return fmt.Errorf("%w: unexpected transcript segments", ErrHistoryIncomplete)
		}
	}
	return nil
}

type CheckpointHintInput struct {
	SourceEvent     string
	Count           int64
	CoalescedCount  int64
	DirtyGeneration int64
	WorkerOutcome   string
	ErrorCode       string
}

type HistoryCheckpointHint struct {
	CaptureID       string
	SourceEvent     string
	FirstReceivedAt time.Time
	LastReceivedAt  time.Time
	HintCount       int64
	CoalescedCount  int64
	DirtyGeneration int64
	WorkerOutcome   string
	ErrorCode       string
	Version         int64
}

func (s *HistoryCaptureService) RecordCheckpointHint(ctx context.Context, captureID, grant string, input CheckpointHintInput) (HistoryCheckpointHint, error) {
	input.SourceEvent = strings.TrimSpace(input.SourceEvent)
	input.WorkerOutcome = strings.TrimSpace(input.WorkerOutcome)
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	if input.Count == 0 {
		input.Count = 1
	}
	validOutcome := map[string]bool{"pending": true, "coalesced": true, "segment_requested": true, "checkpoint_started": true, "checkpoint_committed": true, "failed": true, "ignored": true}
	if input.SourceEvent == "" || len(input.SourceEvent) > 64 || !validOutcome[input.WorkerOutcome] || input.Count < 1 || input.CoalescedCount < 0 || input.CoalescedCount > input.Count || input.DirtyGeneration < 0 || len(input.ErrorCode) > 64 {
		return HistoryCheckpointHint{}, errors.New("invalid checkpoint hint metadata")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, false)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	now := sqlitex.FormatTime(s.now().UTC())
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_checkpoint_hints (
    capture_id, source_event, first_received_at, last_received_at,
    hint_count, coalesced_count, dirty_generation, worker_outcome, error_code
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(capture_id, source_event) DO UPDATE SET
    last_received_at = excluded.last_received_at,
    hint_count = history_checkpoint_hints.hint_count + excluded.hint_count,
    coalesced_count = history_checkpoint_hints.coalesced_count + excluded.coalesced_count,
    dirty_generation = MAX(history_checkpoint_hints.dirty_generation, excluded.dirty_generation),
    worker_outcome = excluded.worker_outcome,
    error_code = excluded.error_code,
    version = history_checkpoint_hints.version + 1`, capture.ID, input.SourceEvent, now, now,
		input.Count, input.CoalescedCount, input.DirtyGeneration, input.WorkerOutcome, input.ErrorCode)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE history_captures
SET last_hint_at = ?, checkpoint_hint_count = checkpoint_hint_count + ?,
    checkpoint_coalesced_count = checkpoint_coalesced_count + ?,
    checkpoint_dirty_generation = MAX(checkpoint_dirty_generation, ?), updated_at = ?
WHERE id = ?`, now, input.Count, input.CoalescedCount, input.DirtyGeneration, now, capture.ID)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryCheckpointHint{}, err
	}
	return s.GetCheckpointHint(ctx, capture.ID, input.SourceEvent)
}

func (s *HistoryCaptureService) GetCheckpointHint(ctx context.Context, captureID, sourceEvent string) (HistoryCheckpointHint, error) {
	var value HistoryCheckpointHint
	var first, last string
	err := s.db.QueryRowContext(ctx, `
SELECT capture_id, source_event, first_received_at, last_received_at,
       hint_count, coalesced_count, dirty_generation, worker_outcome, error_code, version
FROM history_checkpoint_hints WHERE capture_id = ? AND source_event = ?`, captureID, sourceEvent).Scan(
		&value.CaptureID, &value.SourceEvent, &first, &last, &value.HintCount,
		&value.CoalescedCount, &value.DirtyGeneration, &value.WorkerOutcome, &value.ErrorCode, &value.Version)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	value.FirstReceivedAt, err = sqlitex.ParseTime(first)
	if err != nil {
		return HistoryCheckpointHint{}, err
	}
	value.LastReceivedAt, err = sqlitex.ParseTime(last)
	return value, err
}

type HarnessArchiveMemberInput struct {
	NativeSessionID       string
	NativeParentSessionID string
	RelativeMemberPath    string
	MemberKind            string
	AgentName             string
	Status                string
	Model                 string
	HarnessBuild          string
	ParseStatus           string
}

func (s *HistoryCaptureService) RegisterHarnessArchiveMembers(ctx context.Context, captureID, grant, artifactLogicalKey string, members []HarnessArchiveMemberInput) error {
	if len(members) > 100000 {
		return errors.New("too many Harness archive members")
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	capture, err := authenticateHistoryGrantTx(ctx, tx, strings.TrimSpace(captureID), grant, false)
	if err != nil {
		return err
	}
	artifact, err := getInternalHistoryArtifactTx(ctx, tx, capture.ID, strings.TrimSpace(artifactLogicalKey))
	if err != nil {
		return err
	}
	if artifact.Kind != HistoryArtifactHarnessRoot || artifact.PublicationState != HistoryPublicationCommitted || artifact.ArchiveID == "" {
		return ErrHistoryConflict
	}
	now := sqlitex.FormatTime(s.now().UTC())
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		member = normalizeHarnessMember(member)
		if err := validateHarnessMember(member); err != nil {
			return err
		}
		if _, ok := seen[member.RelativeMemberPath]; ok {
			return ErrHistoryConflict
		}
		seen[member.RelativeMemberPath] = struct{}{}
		id, err := randomHistoryID("hm")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO harness_archive_members (
    id, capture_id, artifact_id, archive_id, native_session_id,
    native_parent_session_id, relative_member_path, member_kind,
    agent_name, status, model, harness_build, parse_status, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(artifact_id, relative_member_path) DO NOTHING`, id, capture.ID, artifact.ID, artifact.ArchiveID,
			member.NativeSessionID, member.NativeParentSessionID, member.RelativeMemberPath, member.MemberKind,
			member.AgentName, member.Status, member.Model, member.HarnessBuild, member.ParseStatus, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func normalizeHarnessMember(value HarnessArchiveMemberInput) HarnessArchiveMemberInput {
	value.NativeSessionID = strings.TrimSpace(value.NativeSessionID)
	value.NativeParentSessionID = strings.TrimSpace(value.NativeParentSessionID)
	value.RelativeMemberPath = strings.TrimSpace(value.RelativeMemberPath)
	value.MemberKind = strings.TrimSpace(value.MemberKind)
	value.AgentName = strings.TrimSpace(value.AgentName)
	value.Status = strings.TrimSpace(value.Status)
	value.Model = strings.TrimSpace(value.Model)
	value.HarnessBuild = strings.TrimSpace(value.HarnessBuild)
	value.ParseStatus = strings.TrimSpace(value.ParseStatus)
	return value
}
func validateHarnessMember(value HarnessArchiveMemberInput) error {
	if value.RelativeMemberPath == "" || len(value.RelativeMemberPath) > 4096 || strings.HasPrefix(value.RelativeMemberPath, "/") || strings.Contains(value.RelativeMemberPath, "..") {
		return errors.New("invalid Harness member path")
	}
	if value.MemberKind != "root" && value.MemberKind != "delegated_child" {
		return errors.New("invalid Harness member kind")
	}
	validParse := map[string]bool{"parsed": true, "partial": true, "missing": true, "invalid": true, "unsupported": true}
	if !validParse[value.ParseStatus] {
		return errors.New("invalid Harness member parse status")
	}
	if len(value.NativeSessionID) > 255 || len(value.NativeParentSessionID) > 255 || len(value.AgentName) > 255 || len(value.Status) > 128 || len(value.Model) > 255 || len(value.HarnessBuild) > 255 {
		return errors.New("Harness member metadata exceeds its storage bound")
	}
	return nil
}

type HistoryCaptureEvent struct {
	ID             string
	CaptureID      string
	EventKind      string
	FromState      string
	ToState        string
	CaptureVersion int64
	Actor          string
	Code           string
	DetailsJSON    string
	OccurredAt     time.Time
}

func (s *HistoryCaptureService) ListEvents(ctx context.Context, captureID string) ([]HistoryCaptureEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, capture_id, event_kind, from_state, to_state, capture_version,
       actor, code, details_json, occurred_at
FROM history_capture_events WHERE capture_id = ? ORDER BY occurred_at, id`, strings.TrimSpace(captureID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []HistoryCaptureEvent
	for rows.Next() {
		var event HistoryCaptureEvent
		var occurred string
		if err := rows.Scan(&event.ID, &event.CaptureID, &event.EventKind, &event.FromState, &event.ToState,
			&event.CaptureVersion, &event.Actor, &event.Code, &event.DetailsJSON, &occurred); err != nil {
			return nil, err
		}
		event.OccurredAt, err = sqlitex.ParseTime(occurred)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func authenticateHistoryGrantTx(ctx context.Context, tx *sqlitex.Tx, captureID, grant string, allowComplete bool) (HistoryCapture, error) {
	capture, err := getHistoryCaptureTx(ctx, tx, captureID)
	if err != nil {
		if errors.Is(err, ErrHistoryCaptureNotFound) {
			return HistoryCapture{}, ErrHistoryUnauthorized
		}
		return HistoryCapture{}, err
	}
	var storedHash string
	var revoked sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT upload_grant_hash, upload_grant_revoked_at FROM history_captures WHERE id = ?`, captureID).Scan(&storedHash, &revoked); err != nil {
		return HistoryCapture{}, err
	}
	if !historyGrantMatches(storedHash, grant) {
		return HistoryCapture{}, ErrHistoryUnauthorized
	}
	if capture.State == HistoryCaptureWaived || revoked.Valid && !(allowComplete && capture.State == HistoryCaptureComplete) {
		return HistoryCapture{}, ErrHistoryGrantNoLongerUsable
	}
	return capture, nil
}

func appendHistoryCaptureEvent(ctx context.Context, tx *sqlitex.Tx, captureID, kind, from, to string, version int64, actor, code string, details any, now time.Time) error {
	id, err := randomHistoryID("he")
	if err != nil {
		return err
	}
	body := []byte("{}")
	if details != nil {
		body, err = json.Marshal(details)
		if err != nil {
			return err
		}
	}
	if len(body) > 4096 {
		return errors.New("history capture event details exceed storage bound")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_capture_events (
    id, capture_id, event_kind, from_state, to_state, capture_version,
    actor, code, details_json, occurred_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, captureID, kind, from, to, version,
		actor, code, string(body), sqlitex.FormatTime(now))
	if err != nil {
		return fmt.Errorf("append history capture event: %w", err)
	}
	return nil
}

func appendHistoryUploadEvent(ctx context.Context, tx *sqlitex.Tx, captureID, artifactID, logicalKey, kind, code string, now time.Time) error {
	id, err := randomHistoryID("hu")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO history_upload_events (
    id, capture_id, artifact_id, logical_key, event_kind, error_code, occurred_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, captureID, artifactID, logicalKey, kind, code, sqlitex.FormatTime(now))
	return err
}

func validateHistoryBounded(value string, limit int, name string, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
}

func randomHistoryID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate history %s id: %w", prefix, err)
	}
	return prefix + "-" + hex.EncodeToString(value), nil
}
func randomHistorySecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate history upload grant: %w", err)
	}
	return hex.EncodeToString(value), nil
}
func hashHistoryGrant(grant string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(grant)))
	return hex.EncodeToString(sum[:])
}
func historyGrantMatches(storedHash, grant string) bool {
	candidate := hashHistoryGrant(grant)
	return len(storedHash) == len(candidate) && subtle.ConstantTimeCompare([]byte(storedHash), []byte(candidate)) == 1
}
func isLowerHexHistory(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
