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

type ReviewClaimKind string

const (
	ClaimFixed        ReviewClaimKind = "fixed"
	ClaimNotWarranted ReviewClaimKind = "not_warranted"
	ClaimSuperseded   ReviewClaimKind = "superseded"
)

type ReviewThread struct {
	ID              string            `json:"id"`
	IssueID         string            `json:"issue_id"`
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
	Comments        []ReviewComment   `json:"comments,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
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
	IssueID string         `json:"issue_id"`
	Threads []ReviewThread `json:"threads"`
}

type ThreadService struct {
	db  *sql.DB
	now func() time.Time
}

func NewThreadService(database *sql.DB) *ThreadService {
	return &ThreadService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *ThreadService) CreateThread(ctx context.Context, input CreateThreadInput) (ReviewThread, error) {
	input.ChangeID = strings.TrimSpace(input.ChangeID)
	input.AnchorCommitSHA = strings.TrimSpace(input.AnchorCommitSHA)
	input.FilePath = strings.TrimSpace(input.FilePath)
	input.Context = strings.TrimSpace(input.Context)
	input.Body = strings.TrimSpace(input.Body)
	input.Actor = normalizeReviewActor(input.Actor)
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
	now := s.now().UTC()
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ReviewThread{}, fmt.Errorf("begin create thread transaction: %w", err)
	}
	defer tx.Rollback()

	var issueID string
	if err := tx.QueryRowContext(ctx, `
SELECT issue_id
FROM changes
WHERE id = ?`, input.ChangeID).Scan(&issueID); err != nil {
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
		tx.Rollback()
		return s.GetThread(ctx, existingID)
	case errors.Is(err, sql.ErrNoRows):
		// No prior thread for this key; fall through to insert.
	default:
		return ReviewThread{}, fmt.Errorf("lookup existing review thread: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_threads (
	id,
	issue_id,
	change_id,
	state,
	anchor_commit_sha,
	file_path,
	line,
	context,
	created_by,
	body_hash,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		threadID,
		issueID,
		input.ChangeID,
		string(ThreadOpen),
		input.AnchorCommitSHA,
		input.FilePath,
		input.Line,
		input.Context,
		input.Actor,
		bodyHash,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return ReviewThread{}, fmt.Errorf("insert review thread: %w", err)
	}
	if _, err := insertReviewComment(ctx, tx, threadID, input.Actor, input.Body, now); err != nil {
		return ReviewThread{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewThread{}, fmt.Errorf("commit create thread transaction: %w", err)
	}

	return s.GetThread(ctx, threadID)
}

// hashThreadBody is the deterministic digest backing review-thread idempotency.
// It hashes the trimmed body so re-filing a concern with the same anchor and
// text collapses to a single thread.
func hashThreadBody(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:])
}

func (s *ThreadService) ChangeIssueID(ctx context.Context, changeID string) (string, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return "", errors.New("change id is required")
	}

	var issueID string
	if err := s.db.QueryRowContext(ctx, `
SELECT issue_id
FROM changes
WHERE id = ?`, changeID).Scan(&issueID); err != nil {
		return "", err
	}

	return issueID, nil
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

func (s *ThreadService) ReviewContextForIssue(ctx context.Context, issueID string) (ReviewContext, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ReviewContext{}, errors.New("issue id is required")
	}
	rows, err := s.db.QueryContext(ctx, reviewThreadSelectSQL+`
WHERE issue_id = ?
	AND state IN (?, ?, ?, ?)
ORDER BY created_at, id`,
		issueID,
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

	return ReviewContext{IssueID: issueID, Threads: threads}, nil
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

const reviewThreadSelectSQL = `
SELECT
	id,
	issue_id,
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
	created_at,
	updated_at
FROM review_threads`

func scanReviewThread(scanner issueScanner) (ReviewThread, error) {
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
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&thread.ID,
		&thread.IssueID,
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

func scanReviewComment(scanner issueScanner) (ReviewComment, error) {
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

func normalizeReviewActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return string(ActorHuman)
	}

	return actor
}
