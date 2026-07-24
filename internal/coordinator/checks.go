package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type CheckKind string

const (
	CheckKindCI       CheckKind = "ci"
	CheckKindReviewer CheckKind = "reviewer"
	CheckKindVerifier CheckKind = "verifier"
	CheckKindHuman    CheckKind = "human"
)

type CheckVerdict string

const (
	CheckPending   CheckVerdict = "pending"
	CheckSatisfied CheckVerdict = "satisfied"
	CheckBlocked   CheckVerdict = "blocked"
	CheckSkipped   CheckVerdict = "skipped"
	CheckErrored   CheckVerdict = "errored"
)

type ReviewState string

const (
	ReviewInReview         ReviewState = "in_review"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewApproved         ReviewState = "approved"
	ReviewMerged           ReviewState = "merged"
)

// ErrCheckReportLeaseInvalid indicates that a worker lost its job lease before
// its check result could be committed.
var ErrCheckReportLeaseInvalid = errors.New("worker check report lease is no longer live")

const (
	AutoMergeCheckName             = "auto-merge"
	AutoMergeConflictDetailsPrefix = "auto-merge failed:"
	// AutoMergeTransientDetailsPrefix marks an auto-merge blocked by repeated
	// transient (non-conflict) failures, distinguishable from a real conflict
	// for humans. It extends AutoMergeConflictDetailsPrefix so the
	// reset-on-new-revision retirement (which matches that prefix) still
	// applies.
	AutoMergeTransientDetailsPrefix = AutoMergeConflictDetailsPrefix + " retries exhausted:"
)

type Check struct {
	ID          int64        `json:"id"`
	TaskID      string       `json:"task_id"`
	Name        string       `json:"name"`
	Kind        CheckKind    `json:"kind"`
	Required    bool         `json:"required"`
	Verdict     CheckVerdict `json:"verdict"`
	ExitCode    *int         `json:"exit_code"`
	Details     string       `json:"details"`
	SourceJobID *string      `json:"source_job_id"`
	Reporter    string       `json:"reporter"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

type ReportCheckInput struct {
	TaskID        string
	Name          string
	Kind          CheckKind
	Required      *bool
	Verdict       CheckVerdict
	ExitCode      *int
	Details       string
	SourceJobID   *string
	Reporter      string
	WorkerID      string
	WorkerLeaseID string
}

type CheckService struct {
	db  *sql.DB
	now func() time.Time
}

func NewCheckService(database *sql.DB) *CheckService {
	return &CheckService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *CheckService) ReportCheck(ctx context.Context, input ReportCheckInput) (Check, error) {
	input, err := normalizeReportCheckInput(input)
	if err != nil {
		return Check{}, err
	}
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	input.WorkerLeaseID = strings.TrimSpace(input.WorkerLeaseID)
	if (input.WorkerID == "") != (input.WorkerLeaseID == "") {
		return Check{}, errors.New("worker id and lease id must be provided together")
	}
	if err := s.validateSourceJob(ctx, input.TaskID, input.SourceJobID); err != nil {
		return Check{}, err
	}
	if err := s.crossCheckReviewThreads(ctx, &input); err != nil {
		return Check{}, err
	}

	nowText := formatTime(s.now().UTC())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO checks (
	task_id,
	name,
	kind,
	required,
	verdict,
	exit_code,
	details,
	source_job_id,
	reporter,
	created_at,
	updated_at
) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE ? = '' OR EXISTS (
	SELECT 1
	FROM leases AS l
	JOIN jobs AS j ON j.id = l.job_id
	JOIN changes AS c ON c.id = j.change_id
	WHERE l.id = ?
		AND l.worker_id = ?
		AND l.job_id = ?
		AND l.released_at IS NULL
		AND l.expires_at > ?
		AND j.task_id = ?
		AND j.state IN ('claimed', 'running')
		AND json_type(j.payload_json, '$.head_sha') = 'text'
		AND TRIM(json_extract(j.payload_json, '$.head_sha')) <> ''
		AND TRIM(json_extract(j.payload_json, '$.head_sha')) = TRIM(c.head_sha)
)
ON CONFLICT(task_id, name) DO UPDATE SET
	kind = excluded.kind,
	required = excluded.required,
	verdict = excluded.verdict,
	exit_code = excluded.exit_code,
	details = excluded.details,
	source_job_id = excluded.source_job_id,
	reporter = excluded.reporter,
	updated_at = excluded.updated_at
RETURNING`+checkColumns,
		input.TaskID,
		input.Name,
		string(input.Kind),
		boolInt(*input.Required),
		string(input.Verdict),
		nullableInt(input.ExitCode),
		input.Details,
		nullableStringValue(input.SourceJobID),
		input.Reporter,
		nowText,
		nowText,
		input.WorkerLeaseID,
		input.WorkerLeaseID,
		input.WorkerID,
		nullableStringValue(input.SourceJobID),
		nowText,
		input.TaskID,
	)

	check, err := scanCheck(row)
	if errors.Is(err, sql.ErrNoRows) && input.WorkerLeaseID != "" {
		return Check{}, ErrCheckReportLeaseInvalid
	}
	if err != nil {
		return Check{}, fmt.Errorf("report check: %w", err)
	}

	return check, nil
}

// crossCheckReviewThreads overrides a reviewer's satisfied verdict to blocked
// when the task's ready change still has unresolved review threads. A reviewer
// agent that files actionable threads but then reports satisfied is
// contradicting itself; the cross-check keeps the recorded verdict honest so
// the lifecycle does not approve a change with open critique. It applies to
// reviewer checks only — CI and verifier verdicts stand on their own.
func (s *CheckService) crossCheckReviewThreads(ctx context.Context, input *ReportCheckInput) error {
	if input.Kind != CheckKindReviewer || input.Verdict != CheckSatisfied {
		return nil
	}
	open, err := s.countOpenReviewThreads(ctx, input.TaskID)
	if err != nil {
		return err
	}
	if open == 0 {
		return nil
	}
	input.Verdict = CheckBlocked
	prefix := fmt.Sprintf("open review threads (%d): ", open)
	if strings.TrimSpace(input.Details) == "" {
		input.Details = strings.TrimSpace(prefix)
	} else {
		input.Details = prefix + input.Details
	}

	return nil
}

// countOpenReviewThreads counts unresolved review threads on the task's latest
// ready, unmerged change. open and reopened threads are unresolved (claimed and
// certified threads represent author/verifier progress); a change with none is
// clear of outstanding critique.
func (s *CheckService) countOpenReviewThreads(ctx context.Context, taskID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM review_threads
WHERE state IN (?, ?)
	AND change_id = (
		SELECT id
		FROM changes
		WHERE task_id = ?
			AND ready_at IS NOT NULL
			AND merged_at IS NULL
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 1
	)`,
		string(ThreadOpen),
		string(ThreadReopened),
		taskID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count open review threads: %w", err)
	}

	return count, nil
}

func (s *CheckService) GetCheck(ctx context.Context, taskID string, name string) (Check, error) {
	taskID = strings.TrimSpace(taskID)
	name = strings.TrimSpace(name)
	if taskID == "" {
		return Check{}, errors.New("task id is required")
	}
	if name == "" {
		return Check{}, errors.New("check name is required")
	}

	row := s.db.QueryRowContext(ctx, checkSelectSQL+`
WHERE task_id = ? AND name = ?`, taskID, name)

	return scanCheck(row)
}

func (s *CheckService) ListChecks(ctx context.Context, taskID string) ([]Check, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}

	rows, err := s.db.QueryContext(ctx, checkSelectSQL+`
WHERE task_id = ?
ORDER BY required DESC, name`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	return scanRows(rows, scanCheck)
}

func (s *CheckService) ReviewState(ctx context.Context, taskID string) (ReviewState, error) {
	return reviewStateForTask(ctx, s.db, taskID)
}

func (s *CheckService) CritiqueSatisfied(ctx context.Context, taskID string) (bool, error) {
	return critiqueSatisfiedForTask(ctx, s.db, taskID)
}

// VerifierPending reports whether the task has at least one required
// verifier-kind check that has not yet been satisfied. Verifier checks run in
// the acceptance phase; skipped and non-required verifiers are deliberately
// excluded, mirroring critiqueSatisfiedForTask's required-only scope. It is
// the verifier half of the acceptance gate.
func (s *CheckService) VerifierPending(ctx context.Context, taskID string) (bool, error) {
	return verifierPendingForTask(ctx, s.db, taskID)
}

// critiqueSatisfiedForTask reports whether every required critique-kind check
// (CI/reviewer/human) is satisfied. It is the package-level predicate shared by
// CheckService.CritiqueSatisfied and the acceptance gate so the SQL lives once.
func critiqueSatisfiedForTask(ctx context.Context, database *sql.DB, taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, errors.New("task id is required")
	}
	var count int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM checks
WHERE task_id = ?
	AND required = 1
	AND kind IN (?, ?, ?)
	AND verdict != ?`,
		taskID,
		string(CheckKindCI),
		string(CheckKindReviewer),
		string(CheckKindHuman),
		string(CheckSatisfied),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check critique state: %w", err)
	}

	return count == 0, nil
}

// verifierPendingForTask is the package-level predicate behind
// CheckService.VerifierPending; see that method for semantics.
func verifierPendingForTask(ctx context.Context, database *sql.DB, taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, errors.New("task id is required")
	}
	var count int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM checks
WHERE task_id = ?
	AND kind = ?
	AND required = 1
	AND verdict NOT IN (?, ?)`,
		taskID,
		string(CheckKindVerifier),
		string(CheckSatisfied),
		string(CheckSkipped),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check verifier pending state: %w", err)
	}

	return count > 0, nil
}

// acceptancePendingForTask reports whether the task sits in the acceptance
// gate: every required critique-kind check is satisfied AND at least one
// verifier-kind check is not yet satisfied. It is the single source of truth for
// "acceptance", shared by CheckConfigService.AcceptancePending (which the
// lifecycle engine reads) and the coordinator's DerivePhase, so the two
// derivations never disagree.
func acceptancePendingForTask(ctx context.Context, database *sql.DB, taskID string) (bool, error) {
	critiqueSatisfied, err := critiqueSatisfiedForTask(ctx, database, taskID)
	if err != nil {
		return false, err
	}
	if !critiqueSatisfied {
		return false, nil
	}
	return verifierPendingForTask(ctx, database, taskID)
}

func (s *CheckService) ResetAutomatedChecksForNewRevision(ctx context.Context, taskID string) (int, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return 0, errors.New("task id is required")
	}
	nowText := formatTime(s.now().UTC())
	retiredAutoMerge, err := s.retireAutoMergeConflictCheckForNewRevision(ctx, taskID, nowText)
	if err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE checks
SET verdict = ?,
	exit_code = NULL,
	details = ?,
	source_job_id = NULL,
	updated_at = ?
WHERE task_id = ?
	AND kind IN (?, ?, ?)
	AND verdict != ?
	AND NOT (
		name = ?
		AND kind = ?
		AND reporter = ?
		AND (details LIKE ? OR details = ?)
	)`,
		string(CheckPending),
		"reset after new author revision",
		nowText,
		taskID,
		string(CheckKindCI),
		string(CheckKindReviewer),
		string(CheckKindVerifier),
		string(CheckPending),
		AutoMergeCheckName,
		string(CheckKindCI),
		"coordinator",
		AutoMergeConflictDetailsPrefix+"%",
		"reset after new author revision",
	)
	if err != nil {
		return 0, fmt.Errorf("reset automated checks for new revision: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read reset rows affected: %w", err)
	}

	return int(rows) + retiredAutoMerge, nil
}

func (s *CheckService) retireAutoMergeConflictCheckForNewRevision(ctx context.Context, taskID string, nowText string) (int, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE checks
SET required = 0,
	verdict = ?,
	exit_code = NULL,
	details = ?,
	source_job_id = NULL,
	updated_at = ?
WHERE task_id = ?
	AND name = ?
	AND kind = ?
	AND reporter = ?
	AND verdict = ?
	AND details LIKE ?`,
		string(CheckSkipped),
		"reset after new author revision",
		nowText,
		taskID,
		AutoMergeCheckName,
		string(CheckKindCI),
		"coordinator",
		string(CheckBlocked),
		AutoMergeConflictDetailsPrefix+"%",
	)
	if err != nil {
		return 0, fmt.Errorf("retire auto-merge conflict check: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read retired auto-merge rows affected: %w", err)
	}

	return int(rows), nil
}

func (s *CheckService) validateSourceJob(ctx context.Context, taskID string, sourceJobID *string) error {
	if sourceJobID == nil {
		return nil
	}

	var sourceTaskID sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT task_id
FROM jobs
WHERE id = ?`, *sourceJobID).Scan(&sourceTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("source job not found")
		}
		return fmt.Errorf("load source job: %w", err)
	}
	if !sourceTaskID.Valid || strings.TrimSpace(sourceTaskID.String) != taskID {
		return errors.New("source job does not belong to check task")
	}

	return nil
}

func normalizeReportCheckInput(input ReportCheckInput) (ReportCheckInput, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.TaskID == "" {
		return ReportCheckInput{}, errors.New("task id is required")
	}
	input.Name = strings.TrimSpace(input.Name)
	if err := validateCheckName(input.Name); err != nil {
		return ReportCheckInput{}, err
	}
	if input.Kind == "" {
		input.Kind = CheckKindCI
	}
	if err := validateCheckKind(input.Kind); err != nil {
		return ReportCheckInput{}, err
	}
	if input.Required == nil {
		required := true
		input.Required = &required
	}
	if input.Verdict == "" {
		if input.Kind == CheckKindReviewer || input.Kind == CheckKindVerifier {
			return ReportCheckInput{}, errors.New("reviewer and verifier checks require an explicit structured verdict")
		}
		input.Verdict = verdictForExitCode(input.ExitCode)
	}
	if err := validateCheckVerdict(input.Verdict); err != nil {
		return ReportCheckInput{}, err
	}
	input.Reporter = strings.TrimSpace(input.Reporter)
	if input.Reporter == "" {
		input.Reporter = "system"
	}
	if input.SourceJobID != nil {
		sourceJobID := strings.TrimSpace(*input.SourceJobID)
		if sourceJobID == "" {
			input.SourceJobID = nil
		} else {
			input.SourceJobID = &sourceJobID
		}
	}

	return input, nil
}

func verdictForExitCode(exitCode *int) CheckVerdict {
	if exitCode == nil {
		return CheckPending
	}
	if *exitCode == 0 {
		return CheckSatisfied
	}

	return CheckBlocked
}

func validateCheckName(name string) error {
	if name == "" {
		return errors.New("check name is required")
	}
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '_' ||
			r == '.'
		if !valid {
			return fmt.Errorf("invalid check name: %s", name)
		}
	}

	return nil
}

func validateCheckKind(kind CheckKind) error {
	switch kind {
	case CheckKindCI, CheckKindReviewer, CheckKindVerifier, CheckKindHuman:
		return nil
	default:
		return fmt.Errorf("invalid check kind: %s", kind)
	}
}

func validateCheckVerdict(verdict CheckVerdict) error {
	switch verdict {
	case CheckPending, CheckSatisfied, CheckBlocked, CheckSkipped, CheckErrored:
		return nil
	default:
		return fmt.Errorf("invalid check verdict: %s", verdict)
	}
}

func reviewStateForTask(ctx context.Context, database *sql.DB, taskID string) (ReviewState, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", errors.New("task id is required")
	}

	var state string
	if err := database.QueryRowContext(ctx, `
SELECT review_state
FROM task_review_state
WHERE task_id = ?`, taskID).Scan(&state); err != nil {
		return "", fmt.Errorf("load review state: %w", err)
	}

	return ReviewState(state), nil
}

// checkColumns is the canonical check column list, shared by the readers and
// the upsert's RETURNING clause so they cannot drift. checkSelectSQL wraps it as
// a full projection; callers append WHERE/ORDER clauses.
const checkColumns = `
	id,
	task_id,
	name,
	kind,
	required,
	verdict,
	exit_code,
	details,
	source_job_id,
	reporter,
	created_at,
	updated_at`

const checkSelectSQL = "\nSELECT" + checkColumns + "\nFROM checks"

func scanCheck(scanner taskScanner) (Check, error) {
	var check Check
	var kind string
	var required int
	var verdict string
	var exitCode sql.NullInt64
	var sourceJobID sql.NullString
	var createdAt string
	var updatedAt string

	if err := scanner.Scan(
		&check.ID,
		&check.TaskID,
		&check.Name,
		&kind,
		&required,
		&verdict,
		&exitCode,
		&check.Details,
		&sourceJobID,
		&check.Reporter,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Check{}, fmt.Errorf("scan check: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Check{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Check{}, err
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		check.ExitCode = &value
	}

	check.Kind = CheckKind(kind)
	check.Required = required == 1
	check.Verdict = CheckVerdict(verdict)
	check.SourceJobID = nullableStringPointer(sourceJobID)
	check.CreatedAt = parsedCreatedAt
	check.UpdatedAt = parsedUpdatedAt
	return check, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}

	return *value
}
