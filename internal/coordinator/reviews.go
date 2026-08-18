package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type ReviewThreadState string

const (
	ThreadOpen      ReviewThreadState = "open"
	ThreadClaimed   ReviewThreadState = "claimed"
	ThreadCertified ReviewThreadState = "certified"
	ThreadReopened  ReviewThreadState = "reopened"
)

// ReviewDisposition records the reviewer's scope ruling on a thread: whether
// the concern was introduced by the change under review (blocks) or is
// pre-existing work that the change neither introduced nor made reachable
// (does not block, and is scheduled as a linked follow-up task). The empty
// string is the historical default and behaves like introduced_by_change for
// gating purposes.
type ReviewDisposition string

const (
	DispositionDefault            ReviewDisposition = ""
	DispositionIntroducedByChange ReviewDisposition = "introduced_by_change"
	DispositionPreexisting        ReviewDisposition = "preexisting"
)

type ReviewClaimKind string

const (
	ClaimFixed        ReviewClaimKind = "fixed"
	ClaimNotWarranted ReviewClaimKind = "not_warranted"
	ClaimSuperseded   ReviewClaimKind = "superseded"
)

type ReviewThread struct {
	ID              string            `json:"id"`
	TaskID          string            `json:"task_id"`
	ChangeID        string            `json:"change_id"`
	State           ReviewThreadState `json:"state"`
	AnchorCommitSHA string            `json:"anchor_commit_sha"`
	FilePath        string            `json:"file_path"`
	Line            int               `json:"line"`
	Context         string            `json:"context"`
	CreatedBy       string            `json:"created_by"`
	ClaimKind       *ReviewClaimKind  `json:"claim_kind,omitempty"`
	ClaimCommitSHA  *string           `json:"claim_commit_sha,omitempty"`
	ClaimedBy       *string           `json:"claimed_by,omitempty"`
	ClaimedAt       *time.Time        `json:"claimed_at,omitempty"`
	CertifiedBy     *string           `json:"certified_by,omitempty"`
	CertifiedAt     *time.Time        `json:"certified_at,omitempty"`
	ReopenedBy      *string           `json:"reopened_by,omitempty"`
	ReopenedAt      *time.Time        `json:"reopened_at,omitempty"`
	// ReopenCount is how many times the thread has been reopened after a
	// claim or certification.
	ReopenCount int `json:"reopen_count"`
	// Disposition is the reviewer's scope ruling for this thread.
	Disposition ReviewDisposition `json:"disposition,omitempty"`
	Comments    []ReviewComment   `json:"comments,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type ReviewComment struct {
	ID        int64     `json:"id"`
	ThreadID  string    `json:"thread_id"`
	Actor     string    `json:"actor"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateThreadInput struct {
	ChangeID        string
	AnchorCommitSHA string
	FilePath        string
	Line            int
	Context         string
	Body            string
	Actor           string
	Disposition     ReviewDisposition
}

type AddThreadCommentInput struct {
	ThreadID string
	Body     string
	Actor    string
}

type ClaimThreadInput struct {
	ThreadID       string
	Kind           ReviewClaimKind
	Body           string
	Actor          string
	ClaimCommitSHA string
}

type VerifyThreadInput struct {
	ThreadID string
	Body     string
	Actor    string
}

type ReviewContext struct {
	TaskID  string         `json:"task_id"`
	Threads []ReviewThread `json:"threads"`
}

type ThreadService struct {
	db        *sql.DB
	projectID string
	now       func() time.Time

	// Tasks creates the follow-up task a preexisting-disposition thread
	// schedules. Wired through the project bundle; nil leaves preexisting
	// threads ungated but without a follow-up (tests that never use the
	// disposition are unaffected).
	Tasks *TaskService

	// Checks and Runs back the atomic review submission (SubmitReview), which
	// files threads, records the verdict check, and completes the verdict's
	// human gate in one transaction. The registry wires them after all
	// services are constructed; SubmitReview requires Checks for a verdict and
	// skips the gate when Runs is nil.
	Checks *CheckService
	Runs   *WorkflowRunService

	// AfterHeadCheck, when non-nil, runs inside the review transaction right
	// after the submitted head compares equal to the change's current head and
	// before any thread or verdict writes. Tests use it to prove that a head
	// update cannot interleave between the comparison and the writes; it is
	// nil in production.
	AfterHeadCheck func() error
}

func NewThreadService(database *sql.DB) *ThreadService {
	return &ThreadService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

// NewThreadServiceForProject constructs the thread service with the project
// identity a preexisting-disposition follow-up task id is allocated under.
func NewThreadServiceForProject(database *sql.DB, projectID string) *ThreadService {
	return &ThreadService{
		db:        database,
		projectID: strings.TrimSpace(projectID),
		now:       sqlitex.UTCNow,
	}
}

func (s *ThreadService) CreateThread(ctx context.Context, input CreateThreadInput) (ReviewThread, error) {
	now := s.now().UTC()
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("begin create thread transaction: %w", err)
	}
	defer tx.Rollback()

	thread, err := createThreadTx(ctx, tx, s.projectID, input, now)
	if err != nil {
		return ReviewThread{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewThread{}, fmt.Errorf("commit create thread transaction: %w", err)
	}

	return s.GetThread(ctx, thread.ID)
}

// createThreadTx validates and files one review thread inside an open
// transaction. A duplicate concern (same change, anchor, file, line, and body)
// collapses to the existing thread, so a retry is a no-op rather than a
// duplicate thread. CreateThread wraps it in its own transaction; the atomic
// review submission runs it inside the transaction that also validates the
// change head and records the verdict.
func createThreadTx(ctx context.Context, tx reviewThreadTxer, projectID string, input CreateThreadInput, now time.Time) (ReviewThread, error) {
	input.ChangeID = strings.TrimSpace(input.ChangeID)
	input.AnchorCommitSHA = strings.TrimSpace(input.AnchorCommitSHA)
	input.FilePath = strings.TrimSpace(input.FilePath)
	input.Context = strings.TrimSpace(input.Context)
	input.Body = strings.TrimSpace(input.Body)
	input.Actor = normalizeReviewActor(input.Actor)
	if err := validateReviewDisposition(input.Disposition); err != nil {
		return ReviewThread{}, err
	}
	if input.ChangeID == "" {
		return ReviewThread{}, errors.New("change id is required")
	}
	if input.AnchorCommitSHA == "" {
		return ReviewThread{}, errors.New("anchor commit sha is required")
	}
	if input.FilePath == "" {
		return ReviewThread{}, errors.New("file path is required")
	}
	if input.Line <= 0 {
		return ReviewThread{}, errors.New("line must be positive")
	}
	if input.Body == "" {
		return ReviewThread{}, errors.New("comment body is required")
	}

	// bodyHash is the idempotency key: re-filing the same concern (same change,
	// anchor, file, line, and body) must be a no-op. The worker applies reviewer
	// concerns mechanically from the verdict file, so a transient retry would
	// otherwise double-file. A BEGIN IMMEDIATE transaction serializes the
	// lookup-then-insert so a concurrent retry sees the first insert.
	bodyHash := hashThreadBody(input.Body)
	threadID, err := randomPrefixedID("th")
	if err != nil {
		return ReviewThread{}, err
	}

	var taskID string
	if err := tx.QueryRowContext(ctx, `
SELECT task_id
FROM changes
WHERE id = ?`, input.ChangeID).Scan(&taskID); err != nil {
		return ReviewThread{}, err
	}

	var existingID string
	switch err := tx.QueryRowContext(ctx, `
SELECT id
FROM review_threads
WHERE change_id = ?
	AND anchor_commit_sha = ?
	AND file_path = ?
	AND line = ?
	AND body_hash = ?
LIMIT 1`,
		input.ChangeID,
		input.AnchorCommitSHA,
		input.FilePath,
		input.Line,
		bodyHash,
	).Scan(&existingID); {
	case err == nil:
		// Identical concern already filed; return it unchanged so the retry is a
		// no-op rather than a duplicate thread.
		return scanReviewThread(tx.QueryRowContext(ctx, reviewThreadSelectSQL+` WHERE id = ?`, existingID))
	case errors.Is(err, sql.ErrNoRows):
		// No prior thread for this key; fall through to insert.
	default:
		return ReviewThread{}, fmt.Errorf("lookup existing review thread: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_threads (
	id,
	task_id,
	change_id,
	state,
	anchor_commit_sha,
	file_path,
	line,
	context,
	created_by,
	body_hash,
	reopen_count,
	disposition,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		threadID,
		taskID,
		input.ChangeID,
		string(ThreadOpen),
		input.AnchorCommitSHA,
		input.FilePath,
		input.Line,
		input.Context,
		input.Actor,
		bodyHash,
		0,
		string(input.Disposition),
		formatTime(now),
		formatTime(now),
	); err != nil {
		return ReviewThread{}, fmt.Errorf("insert review thread: %w", err)
	}
	if _, err := insertReviewComment(ctx, tx, threadID, input.Actor, input.Body, now); err != nil {
		return ReviewThread{}, err
	}
	if input.Disposition == DispositionPreexisting {
		if err := schedulePreexistingFollowUpTx(ctx, tx, threadID, taskID, projectID, input, now); err != nil {
			return ReviewThread{}, err
		}
	}

	return scanReviewThread(tx.QueryRowContext(ctx, reviewThreadSelectSQL+` WHERE id = ?`, threadID))
}

// schedulePreexistingFollowUpTx creates exactly one follow-up task for a
// preexisting-disposition thread, inside the same transaction that files the
// thread, and links it related_to the source task. The provenance header names
// the source task, change, anchor, and thread so the follow-up is auditable
// without the review context. Idempotency rides the thread's own dedup key:
// the same concern re-filed collapses to the existing thread above, so this
// runs only on first insert.
func schedulePreexistingFollowUpTx(ctx context.Context, tx reviewThreadTxer, threadID, taskID string, projectID string, input CreateThreadInput, now time.Time) error {
	var featureID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT feature_id FROM tasks WHERE id = ?`, taskID).Scan(&featureID); err != nil {
		return fmt.Errorf("load follow-up source task: %w", err)
	}
	var nextNumber int64
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'task'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		return fmt.Errorf("allocate follow-up task id: %w", err)
	}
	id, err := formatTaskID(projectID, nextNumber)
	if err != nil {
		return err
	}
	nowText := formatTime(now)
	title := fmt.Sprintf("Follow up: preexisting concern at %s:%d", input.FilePath, input.Line)
	if len(title) > 200 {
		title = title[:200]
	}
	body := fmt.Sprintf(
		"Filed from review thread %s during change %s on task %s.\n\n"+
			"The reviewer ruled this concern preexisting: the change under review neither introduced nor made it reachable, "+
			"so it does not gate that change and is scheduled here instead.\n\n"+
			"Anchor: %s:%d (commit %s)\nContext: %s\n\n---\n\n%s",
		threadID, input.ChangeID, taskID,
		input.FilePath, input.Line, input.AnchorCommitSHA, input.Context, input.Body,
	)
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO work_items (id, kind, created_at) VALUES (?, 'task', ?)`, id, nowText); err != nil {
		return fmt.Errorf("insert follow-up work item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, feature_id, created_by, source_task_id, source_change_id,
	created_at, updated_at
) VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		id, title, body, featureID, string(ActorHuman), taskID, input.ChangeID, nowText, nowText); err != nil {
		return fmt.Errorf("insert follow-up task: %w", err)
	}
	// related_to is undirected in this schema's convention: order the endpoints
	// so a re-link cannot create a duplicate pair.
	relatedSourceID, relatedTargetID := taskID, id
	if relatedTargetID < relatedSourceID {
		relatedSourceID, relatedTargetID = relatedTargetID, relatedSourceID
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO work_item_relations (
	source_item_id, target_item_id, kind, created_by, created_at
) VALUES (?, ?, ?, ?, ?)`,
		relatedSourceID, relatedTargetID, string(RelationRelatedTo), string(ActorHuman), nowText); err != nil {
		return fmt.Errorf("relate follow-up task: %w", err)
	}
	return nil
}

// SubmitReviewInput is one human review submission: the change whose diff the
// reviewer inspected, the head SHA shown with that diff, any inline notes, and
// the verdict posted with them.
type SubmitReviewInput struct {
	ChangeID string
	HeadSHA  string
	Verdict  string // approve, request_changes, or comment
	// NodeRunID and ReviewWaitID bind a verdict that resolves an open human
	// gate to the exact persisted review round observed by the caller. They are
	// optional only for a review that records a check while no gate is open.
	NodeRunID    string
	ReviewWaitID string
	Body         string
	// CheckName is the check the verdict reports against (the web UI's human
	// review check). Required for verdicts; ignored for a bare comment.
	CheckName string
	Comments  []SubmitReviewComment
	Actor     string
}

// SubmitReviewComment is one inline note drafted against the reviewed diff.
// Anchor defaults to the inspected head when empty.
type SubmitReviewComment struct {
	FilePath string
	Line     int
	Anchor   string
	Context  string
	Body     string
	// Disposition is the reviewer's scope ruling for this comment's thread.
	Disposition ReviewDisposition
}

type SubmitReviewResult struct {
	Threads []ReviewThread
	Check   *Check
}

// ErrReviewHeadMoved refuses a review submission whose expected head no longer
// matches the change's current head: the reviewer's notes and verdict apply to
// the code they inspected, not to whatever the change has since advanced to.
var ErrReviewHeadMoved = errors.New("change head moved since the review was rendered")

// ErrReviewAnchorMismatch refuses a review submission whose inline-comment
// anchor names a commit other than the inspected head. The web client never
// sends a per-comment anchor (an empty anchor defaults to the inspected head
// below), so a non-empty mismatched anchor is a hand-crafted request trying to
// bind a thread to an arbitrary or older commit.
var ErrReviewAnchorMismatch = errors.New("inline comment anchor must match the inspected head")

// SubmitReview files a human review as one atomic unit. The change's current
// head is re-read inside the same BEGIN IMMEDIATE transaction that creates the
// inline threads, records the verdict check, and completes the verdict's
// workflow gate, so a head update cannot interleave between the comparison and
// the writes: either this submission commits for the inspected head first and
// the advance lands after it, or the advance lands first and the whole
// submission is refused with ErrReviewHeadMoved. A mismatch never leaves
// partial state behind.
func (s *ThreadService) SubmitReview(ctx context.Context, input SubmitReviewInput) (SubmitReviewResult, error) {
	input.ChangeID = strings.TrimSpace(input.ChangeID)
	input.HeadSHA = strings.TrimSpace(input.HeadSHA)
	input.Verdict = strings.TrimSpace(input.Verdict)
	input.NodeRunID = strings.TrimSpace(input.NodeRunID)
	input.ReviewWaitID = strings.TrimSpace(input.ReviewWaitID)
	if input.ChangeID == "" {
		return SubmitReviewResult{}, errors.New("change id is required")
	}
	if input.HeadSHA == "" {
		return SubmitReviewResult{}, errors.New("head sha is required")
	}
	if input.Verdict == "" {
		return SubmitReviewResult{}, errors.New("verdict is required")
	}
	input.Actor = normalizeReviewActor(input.Actor)
	now := s.now().UTC()

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return SubmitReviewResult{}, fmt.Errorf("begin review transaction: %w", err)
	}
	defer tx.Rollback()

	// The expected head is re-validated while the write transaction holds the
	// database connection, so no concurrent head update can land between this
	// comparison and the thread/verdict writes below.
	var taskID, currentHead string
	if err := tx.QueryRowContext(ctx, `
SELECT task_id, head_sha
FROM changes
WHERE id = ?`, input.ChangeID).Scan(&taskID, &currentHead); err != nil {
		return SubmitReviewResult{}, err
	}
	if input.HeadSHA != strings.TrimSpace(currentHead) {
		return SubmitReviewResult{}, ErrReviewHeadMoved
	}
	if s.AfterHeadCheck != nil {
		if err := s.AfterHeadCheck(); err != nil {
			return SubmitReviewResult{}, err
		}
	}

	// An inline comment's anchor, when non-empty, must be the commit the
	// reviewer inspected. The web client never sends one, so a mismatched
	// anchor is a hand-crafted request trying to bind a thread to an arbitrary
	// commit; refuse the whole submission before any thread is filed.
	for _, comment := range input.Comments {
		if anchor := strings.TrimSpace(comment.Anchor); anchor != "" && anchor != input.HeadSHA {
			return SubmitReviewResult{}, fmt.Errorf("%w: anchor %q does not match inspected head %q", ErrReviewAnchorMismatch, anchor, input.HeadSHA)
		}
	}

	result := SubmitReviewResult{}
	for _, comment := range input.Comments {
		anchor := strings.TrimSpace(comment.Anchor)
		if anchor == "" {
			// Default to the head the reviewer inspected, which the check above
			// guarantees is still the change's current head.
			anchor = input.HeadSHA
		}
		thread, err := createThreadTx(ctx, tx, s.projectID, CreateThreadInput{
			ChangeID:        input.ChangeID,
			AnchorCommitSHA: anchor,
			FilePath:        comment.FilePath,
			Line:            comment.Line,
			Context:         comment.Context,
			Body:            comment.Body,
			Actor:           input.Actor,
			Disposition:     comment.Disposition,
		}, now)
		if err != nil {
			return SubmitReviewResult{}, fmt.Errorf("create review thread: %w", err)
		}
		result.Threads = append(result.Threads, thread)
	}

	// A bare comment records the notes without moving the review forward.
	if input.Verdict != "comment" {
		if s.Checks == nil {
			return SubmitReviewResult{}, errors.New("review check service is not configured")
		}
		checkVerdict := CheckSatisfied
		if input.Verdict == "request_changes" {
			checkVerdict = CheckBlocked
		}
		required := true
		check, err := reportCheckTx(ctx, tx, ReportCheckInput{
			TaskID:   taskID,
			Name:     strings.TrimSpace(input.CheckName),
			Kind:     CheckKindHuman,
			Required: &required,
			Verdict:  checkVerdict,
			Details:  strings.TrimSpace(input.Body),
			Reporter: input.Actor,
		}, sqlitex.FormatTime(now))
		if err != nil {
			return SubmitReviewResult{}, err
		}
		result.Check = &check
	}

	// A bare comment records the notes without moving the review forward, so
	// it must not complete the task's human gate: only a verdict (approve or
	// request_changes) responds to the gate, mirroring the check guard above.
	if input.Verdict != "comment" && s.Runs != nil {
		if err := s.Runs.respondToReviewGateTx(ctx, tx, taskID, input.NodeRunID, input.ReviewWaitID, input.Verdict, input.Body); err != nil {
			return SubmitReviewResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return SubmitReviewResult{}, fmt.Errorf("commit review transaction: %w", err)
	}

	// The response threads carry their comments, like CreateThread's do; the
	// re-reads run after the commit so they never see the transaction's own
	// uncommitted state.
	for i := range result.Threads {
		loaded, err := s.GetThread(ctx, result.Threads[i].ID)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		result.Threads[i] = loaded
	}
	return result, nil
}

// hashThreadBody is the deterministic digest backing review-thread idempotency.
// It hashes the trimmed body so re-filing a concern with the same anchor and
// text collapses to a single thread.
func hashThreadBody(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:])
}

// threadDisputed reports whether a thread's reopen history demands an operator
// ruling instead of another author cycle: either it was reopened after being
// certified (the verifier approved a fix that did not hold) or it has been
// reopened twice or more (the fix/verify loop is not converging).
func threadDisputed(thread ReviewThread) bool {
	return thread.ReopenCount >= 2 || thread.State == ThreadReopened && threadReopenedFromCertified(thread)
}

// threadReopenedFromCertified reports whether the thread's most recent reopen
// left a certification behind. Certified-then-reopened is recorded by the
// reopen transaction's clear of certified_by; the durable signal is a
// reopen that happened while a certification existed, so the reopen count
// plus a non-nil certified marker with a reopen after it is enough. The
// ReopenThread call path records the previous state in the thread payload
// for callers that need it; here the certified-at timestamp preceding the
// reopen-at timestamp is the persisted evidence.
func threadReopenedFromCertified(thread ReviewThread) bool {
	return thread.CertifiedAt != nil && thread.ReopenedAt != nil && !thread.CertifiedAt.After(*thread.ReopenedAt)
}

// DisputedOpenThreads lists the task's open/reopened threads that demand an
// operator ruling. It is the shared predicate behind the review-gate hold and
// the author-job suppression.
func (s *ThreadService) DisputedOpenThreads(ctx context.Context, taskID string) ([]ReviewThread, error) {
	rows, err := s.db.QueryContext(ctx, disputedReviewThreadSelect, strings.TrimSpace(taskID), string(ThreadOpen), string(ThreadReopened))
	if err != nil {
		return nil, fmt.Errorf("list disputed review threads: %w", err)
	}
	defer rows.Close()
	return scanDisputedThreads(rows)
}

// DisputedOpenThreadsTx is DisputedOpenThreads for a caller that already holds
// the transaction the verdict is being applied in: the per-project database
// allows a single connection, so a second one would deadlock behind the
// caller's write transaction.
func (s *ThreadService) DisputedOpenThreadsTx(ctx context.Context, tx reviewThreadTxRows, taskID string) ([]ReviewThread, error) {
	rows, err := tx.QueryContext(ctx, disputedReviewThreadSelect, strings.TrimSpace(taskID), string(ThreadOpen), string(ThreadReopened))
	if err != nil {
		return nil, fmt.Errorf("list disputed review threads: %w", err)
	}
	defer rows.Close()
	return scanDisputedThreads(rows)
}

// disputedReviewThreadSelect loads the task's open/reopened threads with the
// fields the dispute predicate needs. A thread is disputed when it was
// reopened at or after a certification (the verifier approved a fix that did
// not hold) or has been reopened twice or more.
const disputedReviewThreadSelect = reviewThreadSelectSQL + `
WHERE task_id = ?
	AND state IN (?, ?)
	AND (reopen_count >= 2 OR (certified_at IS NOT NULL AND reopened_at IS NOT NULL AND reopened_at >= certified_at))`

func scanDisputedThreads(rows *sql.Rows) ([]ReviewThread, error) {
	var threads []ReviewThread
	for rows.Next() {
		thread, err := scanReviewThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// reviewThreadTxRows is the query surface the in-transaction dispute lookup
// needs. workflowTx satisfies it.
type reviewThreadTxRows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *ThreadService) ChangeTaskID(ctx context.Context, changeID string) (string, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return "", errors.New("change id is required")
	}

	var taskID string
	if err := s.db.QueryRowContext(ctx, `
SELECT task_id
FROM changes
WHERE id = ?`, changeID).Scan(&taskID); err != nil {
		return "", err
	}

	return taskID, nil
}

func (s *ThreadService) AddComment(ctx context.Context, input AddThreadCommentInput) (ReviewThread, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.Body = strings.TrimSpace(input.Body)
	input.Actor = normalizeReviewActor(input.Actor)
	if input.ThreadID == "" {
		return ReviewThread{}, errors.New("thread id is required")
	}
	if input.Body == "" {
		return ReviewThread{}, errors.New("comment body is required")
	}
	now := s.now().UTC()
	if _, err := insertReviewComment(ctx, s.db, input.ThreadID, input.Actor, input.Body, now); err != nil {
		return ReviewThread{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE review_threads
SET updated_at = ?
WHERE id = ?`, formatTime(now), input.ThreadID); err != nil {
		return ReviewThread{}, fmt.Errorf("touch review thread: %w", err)
	}

	return s.GetThread(ctx, input.ThreadID)
}

func (s *ThreadService) ClaimThread(ctx context.Context, input ClaimThreadInput) (ReviewThread, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.Body = strings.TrimSpace(input.Body)
	input.Actor = normalizeReviewActor(input.Actor)
	input.ClaimCommitSHA = strings.TrimSpace(input.ClaimCommitSHA)
	if input.ThreadID == "" {
		return ReviewThread{}, errors.New("thread id is required")
	}
	if err := validateClaimKind(input.Kind); err != nil {
		return ReviewThread{}, err
	}
	if input.Kind != ClaimFixed && input.Body == "" {
		return ReviewThread{}, errors.New("not_warranted and superseded claims require a rationale comment")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("begin claim thread transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE review_threads
SET state = ?,
	claim_kind = ?,
	claim_commit_sha = ?,
	claimed_by = ?,
	claimed_at = ?,
	certified_by = NULL,
	certified_at = NULL,
	reopened_by = NULL,
	reopened_at = NULL,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?, ?)`,
		string(ThreadClaimed),
		string(input.Kind),
		sqlitex.NullableNonEmptyString(input.ClaimCommitSHA),
		input.Actor,
		formatTime(now),
		formatTime(now),
		input.ThreadID,
		string(ThreadOpen),
		string(ThreadReopened),
		string(ThreadClaimed),
	)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("claim review thread: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ReviewThread{}, err
	}
	if rows == 0 {
		return ReviewThread{}, sql.ErrNoRows
	}
	if input.Body != "" {
		if _, err := insertReviewComment(ctx, tx, input.ThreadID, input.Actor, input.Body, now); err != nil {
			return ReviewThread{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ReviewThread{}, fmt.Errorf("commit claim thread transaction: %w", err)
	}

	return s.GetThread(ctx, input.ThreadID)
}

func (s *ThreadService) CertifyThread(ctx context.Context, input VerifyThreadInput) (ReviewThread, error) {
	return s.verifyThread(ctx, input, ThreadCertified)
}

func (s *ThreadService) ReopenThread(ctx context.Context, input VerifyThreadInput) (ReviewThread, error) {
	input.Body = strings.TrimSpace(input.Body)
	if input.Body == "" {
		return ReviewThread{}, errors.New("reopen requires an explanation comment")
	}
	return s.verifyThread(ctx, input, ThreadReopened)
}

func (s *ThreadService) verifyThread(ctx context.Context, input VerifyThreadInput, state ReviewThreadState) (ReviewThread, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.Body = strings.TrimSpace(input.Body)
	input.Actor = normalizeReviewActor(input.Actor)
	if input.ThreadID == "" {
		return ReviewThread{}, errors.New("thread id is required")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("begin verify thread transaction: %w", err)
	}
	defer tx.Rollback()

	// The reopen path needs the state it is leaving so escalation predicates can
	// distinguish a reopened-after-certification from a reopened-after-claim.
	var previousState string
	if state == ThreadReopened {
		if err := tx.QueryRowContext(ctx, `SELECT state FROM review_threads WHERE id = ?`, input.ThreadID).Scan(&previousState); err != nil {
			return ReviewThread{}, err
		}
	}

	var query string
	var args []any
	switch state {
	case ThreadCertified:
		query = `
UPDATE review_threads
SET state = ?,
	certified_by = ?,
	certified_at = ?,
	reopened_by = NULL,
	reopened_at = NULL,
	updated_at = ?
WHERE id = ?
	AND state = ?`
		args = []any{string(ThreadCertified), input.Actor, formatTime(now), formatTime(now), input.ThreadID, string(ThreadClaimed)}
	case ThreadReopened:
		query = `
UPDATE review_threads
SET state = ?,
	reopen_count = reopen_count + 1,
	reopened_by = ?,
	reopened_at = ?,
	updated_at = ?
WHERE id = ?
	AND state IN (?, ?)`
		args = []any{string(ThreadReopened), input.Actor, formatTime(now), formatTime(now), input.ThreadID, string(ThreadClaimed), string(ThreadCertified)}
	default:
		return ReviewThread{}, errors.New("unsupported verification state")
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("verify review thread: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ReviewThread{}, err
	}
	if rows == 0 {
		return ReviewThread{}, sql.ErrNoRows
	}
	if input.Body != "" {
		if _, err := insertReviewComment(ctx, tx, input.ThreadID, input.Actor, input.Body, now); err != nil {
			return ReviewThread{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ReviewThread{}, fmt.Errorf("commit verify thread transaction: %w", err)
	}

	return s.GetThread(ctx, input.ThreadID)
}

func (s *ThreadService) GetThread(ctx context.Context, threadID string) (ReviewThread, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ReviewThread{}, errors.New("thread id is required")
	}
	thread, err := scanReviewThread(s.db.QueryRowContext(ctx, reviewThreadSelectSQL+` WHERE id = ?`, threadID))
	if err != nil {
		return ReviewThread{}, err
	}
	comments, err := s.ListComments(ctx, thread.ID)
	if err != nil {
		return ReviewThread{}, err
	}
	thread.Comments = comments

	return thread, nil
}

func (s *ThreadService) ListThreadsForChange(ctx context.Context, changeID string) ([]ReviewThread, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return nil, errors.New("change id is required")
	}
	rows, err := s.db.QueryContext(ctx, reviewThreadSelectSQL+`
WHERE change_id = ?
ORDER BY created_at, id`, changeID)
	if err != nil {
		return nil, fmt.Errorf("list review threads: %w", err)
	}

	return s.scanThreadsWithComments(ctx, rows)
}

// ListThreadsForTask returns every review thread recorded for the task,
// across all of its changes, each carrying its full comment timeline. This is
// the task-scoped read behind GET /v2/tasks/{taskID}/threads; the agent-payload
// projection with the same scope is ReviewContextForTask below.
func (s *ThreadService) ListThreadsForTask(ctx context.Context, taskID string) ([]ReviewThread, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	rows, err := s.db.QueryContext(ctx, reviewThreadSelectSQL+`
WHERE task_id = ?
ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review threads: %w", err)
	}

	return s.scanThreadsWithComments(ctx, rows)
}

func (s *ThreadService) ReviewContextForTask(ctx context.Context, taskID string) (ReviewContext, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ReviewContext{}, errors.New("task id is required")
	}
	rows, err := s.db.QueryContext(ctx, reviewThreadSelectSQL+`
WHERE task_id = ?
	AND state IN (?, ?, ?, ?)
ORDER BY created_at, id`,
		taskID,
		string(ThreadOpen),
		string(ThreadClaimed),
		string(ThreadReopened),
		string(ThreadCertified),
	)
	if err != nil {
		return ReviewContext{}, fmt.Errorf("list review context threads: %w", err)
	}
	threads, err := s.scanThreadsWithComments(ctx, rows)
	if err != nil {
		return ReviewContext{}, err
	}

	return ReviewContext{TaskID: taskID, Threads: threads}, nil
}

func (s *ThreadService) ListComments(ctx context.Context, threadID string) ([]ReviewComment, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, errors.New("thread id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, thread_id, actor, body, created_at
FROM review_comments
WHERE thread_id = ?
ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, fmt.Errorf("list review comments: %w", err)
	}
	return scanRows(rows, scanReviewComment)
}

func (s *ThreadService) scanThreadsWithComments(ctx context.Context, rows *sql.Rows) ([]ReviewThread, error) {
	var threads []ReviewThread
	for rows.Next() {
		thread, err := scanReviewThread(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate review threads: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close review thread rows: %w", err)
	}
	for index := range threads {
		comments, err := s.ListComments(ctx, threads[index].ID)
		if err != nil {
			return nil, err
		}
		threads[index].Comments = comments
	}

	return threads, nil
}

func insertReviewComment(ctx context.Context, executor queryExecutor, threadID string, actor string, body string, createdAt time.Time) (ReviewComment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return ReviewComment{}, errors.New("comment body is required")
	}
	actor = normalizeReviewActor(actor)
	result, err := executor.ExecContext(ctx, `
INSERT INTO review_comments (
	thread_id,
	actor,
	body,
	created_at
) VALUES (?, ?, ?, ?)`,
		strings.TrimSpace(threadID),
		actor,
		body,
		formatTime(createdAt),
	)
	if err != nil {
		return ReviewComment{}, fmt.Errorf("insert review comment: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ReviewComment{}, fmt.Errorf("read review comment id: %w", err)
	}

	return ReviewComment{ID: id, ThreadID: strings.TrimSpace(threadID), Actor: actor, Body: body, CreatedAt: createdAt}, nil
}

type queryExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reviewThreadTxer is the transactional surface createThreadTx runs on: a real
// *sql.Tx from CreateThread, or the BEGIN IMMEDIATE transaction held by the
// atomic review submission.
type reviewThreadTxer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const reviewThreadSelectSQL = `
SELECT
	id,
	task_id,
	change_id,
	state,
	anchor_commit_sha,
	file_path,
	line,
	context,
	created_by,
	claim_kind,
	claim_commit_sha,
	claimed_by,
	claimed_at,
	certified_by,
	certified_at,
	reopened_by,
	reopened_at,
	reopen_count,
	disposition,
	created_at,
	updated_at
FROM review_threads`

func scanReviewThread(scanner taskScanner) (ReviewThread, error) {
	var thread ReviewThread
	var state string
	var claimKind sql.NullString
	var claimCommitSHA sql.NullString
	var claimedBy sql.NullString
	var claimedAt sql.NullString
	var certifiedBy sql.NullString
	var certifiedAt sql.NullString
	var reopenedBy sql.NullString
	var reopenedAt sql.NullString
	var reopenCount int
	var disposition string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&thread.ID,
		&thread.TaskID,
		&thread.ChangeID,
		&state,
		&thread.AnchorCommitSHA,
		&thread.FilePath,
		&thread.Line,
		&thread.Context,
		&thread.CreatedBy,
		&claimKind,
		&claimCommitSHA,
		&claimedBy,
		&claimedAt,
		&certifiedBy,
		&certifiedAt,
		&reopenedBy,
		&reopenedAt,
		&reopenCount,
		&disposition,
		&createdAt,
		&updatedAt,
	); err != nil {
		return ReviewThread{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return ReviewThread{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return ReviewThread{}, err
	}
	thread.State = ReviewThreadState(state)
	thread.CreatedAt = parsedCreatedAt
	thread.UpdatedAt = parsedUpdatedAt
	thread.ReopenCount = reopenCount
	thread.Disposition = ReviewDisposition(disposition)
	if claimKind.Valid {
		value := ReviewClaimKind(claimKind.String)
		thread.ClaimKind = &value
	}
	if claimCommitSHA.Valid {
		thread.ClaimCommitSHA = &claimCommitSHA.String
	}
	if claimedBy.Valid {
		thread.ClaimedBy = &claimedBy.String
	}
	if claimedAt.Valid {
		parsed, err := parseTime(claimedAt.String)
		if err != nil {
			return ReviewThread{}, err
		}
		thread.ClaimedAt = &parsed
	}
	if certifiedBy.Valid {
		thread.CertifiedBy = &certifiedBy.String
	}
	if certifiedAt.Valid {
		parsed, err := parseTime(certifiedAt.String)
		if err != nil {
			return ReviewThread{}, err
		}
		thread.CertifiedAt = &parsed
	}
	if reopenedBy.Valid {
		thread.ReopenedBy = &reopenedBy.String
	}
	if reopenedAt.Valid {
		parsed, err := parseTime(reopenedAt.String)
		if err != nil {
			return ReviewThread{}, err
		}
		thread.ReopenedAt = &parsed
	}

	return thread, nil
}

func scanReviewComment(scanner taskScanner) (ReviewComment, error) {
	var comment ReviewComment
	var createdAt string
	if err := scanner.Scan(&comment.ID, &comment.ThreadID, &comment.Actor, &comment.Body, &createdAt); err != nil {
		return ReviewComment{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return ReviewComment{}, err
	}
	comment.CreatedAt = parsedCreatedAt

	return comment, nil
}

func validateClaimKind(kind ReviewClaimKind) error {
	switch kind {
	case ClaimFixed, ClaimNotWarranted, ClaimSuperseded:
		return nil
	default:
		return fmt.Errorf("invalid claim kind: %s", kind)
	}
}

func validateReviewDisposition(disposition ReviewDisposition) error {
	switch disposition {
	case DispositionDefault, DispositionIntroducedByChange, DispositionPreexisting:
		return nil
	default:
		return fmt.Errorf("invalid review disposition: %s", disposition)
	}
}

func normalizeReviewActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return string(ActorHuman)
	}

	return actor
}
