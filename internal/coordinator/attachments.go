package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	IssueAttachmentMaxBytes        = 25 << 20 // 25 MiB
	defaultAttachmentContentType   = "application/octet-stream"
	attachmentStorageDirectoryMode = 0o700
	attachmentStorageFileMode      = 0o600
)

// issueAttachmentInlineSafeImageTypes is the set of raster image media types
// that are safe to render inline and that Flow treats as image attachments for
// harness --image injection. SVG is intentionally excluded (it can carry
// script). Kept in sync with the api package's inline-safe rendering set.
var issueAttachmentInlineSafeImageTypes = map[string]struct{}{
	"image/avif": {},
	"image/bmp":  {},
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// IsImageContentType reports whether contentType is one of the raster image
// media types Flow treats as an image attachment (avif/bmp/gif/jpeg/png/webp).
// It parses the media type so parameters (e.g. "image/png; charset=utf-8") and
// surrounding whitespace are tolerated, and lower-cases it. SVG is excluded.
// It is the shared definition the coordinator (payload stamping) and the
// worker (materialization filtering) agree on.
func IsImageContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return false
	}
	_, ok := issueAttachmentInlineSafeImageTypes[strings.ToLower(mediaType)]
	return ok
}

type IssueAttachmentStage string

const (
	IssueAttachmentStageInitial  IssueAttachmentStage = "initial"
	IssueAttachmentStageAuthor   IssueAttachmentStage = "author"
	IssueAttachmentStageReviewer IssueAttachmentStage = "reviewer"
	IssueAttachmentStageVerifier IssueAttachmentStage = "verifier"
)

type IssueAttachment struct {
	ID          string               `json:"id"`
	IssueID     string               `json:"issue_id"`
	Stage       IssueAttachmentStage `json:"stage"`
	Filename    string               `json:"filename"`
	ContentType string               `json:"content_type"`
	SizeBytes   int64                `json:"size_bytes"`
	StorageKey  string               `json:"-"`
	CreatedBy   Actor                `json:"created_by"`
	CreatedAt   time.Time            `json:"created_at"`
}

type CreateIssueAttachmentInput struct {
	IssueID     string
	Stage       IssueAttachmentStage
	Filename    string
	ContentType string
	CreatedBy   Actor
	Reader      io.Reader
}

type IssueAttachmentStore struct {
	dir string
}

type storedIssueAttachment struct {
	StorageKey string
	SizeBytes  int64
}

var attachmentStorageKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func NewIssueAttachmentStore(dir string) *IssueAttachmentStore {
	return &IssueAttachmentStore{dir: dir}
}

func (s *IssueAttachmentStore) Save(storageKey string, r io.Reader) (storedIssueAttachment, error) {
	if s == nil {
		return storedIssueAttachment{}, errors.New("attachment store is not configured")
	}
	if r == nil {
		return storedIssueAttachment{}, errors.New("attachment reader is required")
	}
	attachmentPath, err := s.pathFor(storageKey)
	if err != nil {
		return storedIssueAttachment{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r, IssueAttachmentMaxBytes+1))
	if err != nil {
		return storedIssueAttachment{}, fmt.Errorf("read attachment: %w", err)
	}
	if len(data) > IssueAttachmentMaxBytes {
		return storedIssueAttachment{}, fmt.Errorf("attachment exceeds %d bytes", IssueAttachmentMaxBytes)
	}
	if err := writeFileAtomic(s.dir, attachmentPath, data, attachmentStorageDirectoryMode, attachmentStorageFileMode, ".attachment-*"); err != nil {
		return storedIssueAttachment{}, err
	}

	return storedIssueAttachment{StorageKey: storageKey, SizeBytes: int64(len(data))}, nil
}

// writeFileAtomic writes data to dest by staging it in a temp file under dir and
// renaming it into place, so a partial or failed write never replaces an
// existing complete file. dir is created with dirMode if missing and the file is
// written with fileMode. Shared by the attachment and transcript stores.
func writeFileAtomic(dir, dest string, data []byte, dirMode, fileMode os.FileMode, tmpPattern string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("set file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("finalize file: %w", err)
	}

	return nil
}

func (s *IssueAttachmentStore) Open(storageKey string) (io.ReadCloser, error) {
	if s == nil {
		return nil, errors.New("attachment store is not configured")
	}
	attachmentPath, err := s.pathFor(storageKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(attachmentPath)
	if err != nil {
		return nil, err
	}

	return file, nil
}

func (s *IssueAttachmentStore) Remove(storageKey string) error {
	if s == nil {
		return errors.New("attachment store is not configured")
	}
	attachmentPath, err := s.pathFor(storageKey)
	if err != nil {
		return err
	}
	if err := os.Remove(attachmentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (s *IssueAttachmentStore) pathFor(storageKey string) (string, error) {
	storageKey = strings.TrimSpace(storageKey)
	if storageKey == "." || storageKey == ".." || !attachmentStorageKeyPattern.MatchString(storageKey) {
		return "", fmt.Errorf("invalid attachment storage key %q", storageKey)
	}
	if strings.TrimSpace(s.dir) == "" {
		return "", errors.New("attachment directory is required")
	}

	return filepath.Join(s.dir, storageKey), nil
}

func (s *IssueService) CreateIssueAttachment(ctx context.Context, input CreateIssueAttachmentInput, store *IssueAttachmentStore) (IssueAttachment, error) {
	normalized, err := normalizeCreateIssueAttachmentInput(input)
	if err != nil {
		return IssueAttachment{}, err
	}
	if store == nil {
		return IssueAttachment{}, errors.New("attachment store is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IssueAttachment{}, fmt.Errorf("begin create attachment transaction: %w", err)
	}
	defer tx.Rollback()

	if err := issueExistsInTx(ctx, tx, normalized.IssueID); err != nil {
		return IssueAttachment{}, err
	}
	id, err := allocateIssueAttachmentID(ctx, tx)
	if err != nil {
		return IssueAttachment{}, err
	}

	stored, err := store.Save(id, normalized.Reader)
	if err != nil {
		return IssueAttachment{}, err
	}
	storedCommitted := false
	defer func() {
		if !storedCommitted {
			_ = store.Remove(stored.StorageKey)
		}
	}()

	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO issue_attachments (
	id,
	issue_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		normalized.IssueID,
		string(normalized.Stage),
		normalized.Filename,
		normalized.ContentType,
		stored.SizeBytes,
		stored.StorageKey,
		string(normalized.CreatedBy),
		nowText,
	); err != nil {
		return IssueAttachment{}, fmt.Errorf("insert issue attachment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return IssueAttachment{}, fmt.Errorf("commit create attachment: %w", err)
	}
	storedCommitted = true

	return s.GetIssueAttachment(ctx, normalized.IssueID, id)
}

func (s *IssueService) ListIssueAttachments(ctx context.Context, issueID string) ([]IssueAttachment, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, errors.New("issue id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	issue_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
FROM issue_attachments
WHERE issue_id = ?
ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue attachments: %w", err)
	}
	defer rows.Close()

	return scanIssueAttachments(rows)
}

func (s *IssueService) GetIssueAttachment(ctx context.Context, issueID string, attachmentID string) (IssueAttachment, error) {
	issueID = strings.TrimSpace(issueID)
	attachmentID = strings.TrimSpace(attachmentID)
	if issueID == "" {
		return IssueAttachment{}, errors.New("issue id is required")
	}
	if attachmentID == "" {
		return IssueAttachment{}, errors.New("attachment id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT
	id,
	issue_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
FROM issue_attachments
WHERE issue_id = ? AND id = ?`, issueID, attachmentID)

	return scanIssueAttachment(row)
}

func normalizeCreateIssueAttachmentInput(input CreateIssueAttachmentInput) (CreateIssueAttachmentInput, error) {
	input.IssueID = strings.TrimSpace(input.IssueID)
	if input.IssueID == "" {
		return CreateIssueAttachmentInput{}, errors.New("issue id is required")
	}
	if input.Stage == "" {
		input.Stage = IssueAttachmentStageInitial
	}
	if err := validateIssueAttachmentStage(input.Stage); err != nil {
		return CreateIssueAttachmentInput{}, err
	}
	filename := cleanAttachmentFilename(input.Filename)
	if filename == "" {
		return CreateIssueAttachmentInput{}, errors.New("attachment filename is required")
	}
	input.Filename = filename
	input.ContentType = strings.TrimSpace(input.ContentType)
	if input.ContentType == "" {
		input.ContentType = defaultAttachmentContentType
	}
	if len(input.ContentType) > 255 {
		return CreateIssueAttachmentInput{}, errors.New("attachment content type is too long")
	}
	if input.CreatedBy == "" {
		input.CreatedBy = ActorHuman
	}
	if err := validateActor(input.CreatedBy); err != nil {
		return CreateIssueAttachmentInput{}, err
	}
	if input.Reader == nil {
		return CreateIssueAttachmentInput{}, errors.New("attachment reader is required")
	}

	return input, nil
}

func validateIssueAttachmentStage(stage IssueAttachmentStage) error {
	switch stage {
	case IssueAttachmentStageInitial, IssueAttachmentStageAuthor, IssueAttachmentStageReviewer, IssueAttachmentStageVerifier:
		return nil
	default:
		return fmt.Errorf("invalid attachment stage %q", stage)
	}
}

func cleanAttachmentFilename(filename string) string {
	filename = strings.TrimSpace(strings.ReplaceAll(filename, "\\", "/"))
	filename = path.Base(filename)
	if filename == "." || filename == "/" {
		return ""
	}

	return strings.TrimSpace(filename)
}

func allocateIssueAttachmentID(ctx context.Context, tx *sql.Tx) (string, error) {
	var nextNumber int
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'issue_attachment'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate issue attachment id: %w", err)
	}

	return formatIssueAttachmentID(nextNumber), nil
}

func formatIssueAttachmentID(number int) string {
	return fmt.Sprintf("att-%04d", number)
}

func issueExistsInTx(ctx context.Context, tx *sql.Tx, issueID string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM issues WHERE id = ?", issueID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return fmt.Errorf("lookup issue: %w", err)
	}

	return nil
}

type issueAttachmentScanner interface {
	Scan(dest ...any) error
}

func scanIssueAttachment(scanner issueAttachmentScanner) (IssueAttachment, error) {
	var attachment IssueAttachment
	var stage string
	var createdBy string
	var createdAt string
	if err := scanner.Scan(
		&attachment.ID,
		&attachment.IssueID,
		&stage,
		&attachment.Filename,
		&attachment.ContentType,
		&attachment.SizeBytes,
		&attachment.StorageKey,
		&createdBy,
		&createdAt,
	); err != nil {
		return IssueAttachment{}, err
	}
	attachment.Stage = IssueAttachmentStage(stage)
	attachment.CreatedBy = Actor(createdBy)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return IssueAttachment{}, err
	}
	attachment.CreatedAt = parsedCreatedAt

	return attachment, nil
}

func scanIssueAttachments(rows *sql.Rows) ([]IssueAttachment, error) {
	var attachments []IssueAttachment
	for rows.Next() {
		attachment, err := scanIssueAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan issue attachment: %w", err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue attachments: %w", err)
	}

	return attachments, nil
}
