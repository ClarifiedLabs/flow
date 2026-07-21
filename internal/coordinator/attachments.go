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
	TaskAttachmentMaxBytes         = 25 << 20 // 25 MiB
	defaultAttachmentContentType   = "application/octet-stream"
	attachmentStorageDirectoryMode = 0o700
	attachmentStorageFileMode      = 0o600
)

// taskAttachmentInlineSafeImageTypes is the set of raster image media types
// that are safe to render inline and that Flow treats as image attachments for
// harness --image injection. SVG is intentionally excluded (it can carry
// script). Kept in sync with the api package's inline-safe rendering set.
var taskAttachmentInlineSafeImageTypes = map[string]struct{}{
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
	_, ok := taskAttachmentInlineSafeImageTypes[strings.ToLower(mediaType)]
	return ok
}

type TaskAttachmentStage string

const (
	TaskAttachmentStageInitial  TaskAttachmentStage = "initial"
	TaskAttachmentStageAuthor   TaskAttachmentStage = "author"
	TaskAttachmentStageReviewer TaskAttachmentStage = "reviewer"
	TaskAttachmentStageVerifier TaskAttachmentStage = "verifier"
)

type TaskAttachment struct {
	ID          string              `json:"id"`
	TaskID      string              `json:"task_id"`
	Stage       TaskAttachmentStage `json:"stage"`
	Filename    string              `json:"filename"`
	ContentType string              `json:"content_type"`
	SizeBytes   int64               `json:"size_bytes"`
	StorageKey  string              `json:"-"`
	CreatedBy   Actor               `json:"created_by"`
	CreatedAt   time.Time           `json:"created_at"`
}

type CreateTaskAttachmentInput struct {
	TaskID      string
	Stage       TaskAttachmentStage
	Filename    string
	ContentType string
	CreatedBy   Actor
	Reader      io.Reader
}

type TaskAttachmentStore struct {
	dir string
}

type storedTaskAttachment struct {
	StorageKey string
	SizeBytes  int64
}

var attachmentStorageKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func NewTaskAttachmentStore(dir string) *TaskAttachmentStore {
	return &TaskAttachmentStore{dir: dir}
}

func (s *TaskAttachmentStore) Save(storageKey string, r io.Reader) (storedTaskAttachment, error) {
	if s == nil {
		return storedTaskAttachment{}, errors.New("attachment store is not configured")
	}
	if r == nil {
		return storedTaskAttachment{}, errors.New("attachment reader is required")
	}
	attachmentPath, err := s.pathFor(storageKey)
	if err != nil {
		return storedTaskAttachment{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r, TaskAttachmentMaxBytes+1))
	if err != nil {
		return storedTaskAttachment{}, fmt.Errorf("read attachment: %w", err)
	}
	if len(data) > TaskAttachmentMaxBytes {
		return storedTaskAttachment{}, fmt.Errorf("attachment exceeds %d bytes", TaskAttachmentMaxBytes)
	}
	if err := writeFileAtomic(s.dir, attachmentPath, data, attachmentStorageDirectoryMode, attachmentStorageFileMode, ".attachment-*"); err != nil {
		return storedTaskAttachment{}, err
	}

	return storedTaskAttachment{StorageKey: storageKey, SizeBytes: int64(len(data))}, nil
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

func (s *TaskAttachmentStore) Open(storageKey string) (io.ReadCloser, error) {
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

func (s *TaskAttachmentStore) Remove(storageKey string) error {
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

func (s *TaskAttachmentStore) pathFor(storageKey string) (string, error) {
	storageKey = strings.TrimSpace(storageKey)
	if storageKey == "." || storageKey == ".." || !attachmentStorageKeyPattern.MatchString(storageKey) {
		return "", fmt.Errorf("invalid attachment storage key %q", storageKey)
	}
	if strings.TrimSpace(s.dir) == "" {
		return "", errors.New("attachment directory is required")
	}

	return filepath.Join(s.dir, storageKey), nil
}

func (s *TaskService) CreateTaskAttachment(ctx context.Context, input CreateTaskAttachmentInput, store *TaskAttachmentStore) (TaskAttachment, error) {
	normalized, err := normalizeCreateTaskAttachmentInput(input)
	if err != nil {
		return TaskAttachment{}, err
	}
	if store == nil {
		return TaskAttachment{}, errors.New("attachment store is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskAttachment{}, fmt.Errorf("begin create attachment transaction: %w", err)
	}
	defer tx.Rollback()

	if err := taskExistsInTx(ctx, tx, normalized.TaskID); err != nil {
		return TaskAttachment{}, err
	}
	id, err := allocateTaskAttachmentID(ctx, tx)
	if err != nil {
		return TaskAttachment{}, err
	}

	stored, err := store.Save(id, normalized.Reader)
	if err != nil {
		return TaskAttachment{}, err
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
INSERT INTO task_attachments (
	id,
	task_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		normalized.TaskID,
		string(normalized.Stage),
		normalized.Filename,
		normalized.ContentType,
		stored.SizeBytes,
		stored.StorageKey,
		string(normalized.CreatedBy),
		nowText,
	); err != nil {
		return TaskAttachment{}, fmt.Errorf("insert task attachment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return TaskAttachment{}, fmt.Errorf("commit create attachment: %w", err)
	}
	storedCommitted = true

	return s.GetTaskAttachment(ctx, normalized.TaskID, id)
}

func (s *TaskService) ListTaskAttachments(ctx context.Context, taskID string) ([]TaskAttachment, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	task_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
FROM task_attachments
WHERE task_id = ?
ORDER BY created_at ASC, id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task attachments: %w", err)
	}
	defer rows.Close()

	return scanTaskAttachments(rows)
}

func (s *TaskService) GetTaskAttachment(ctx context.Context, taskID string, attachmentID string) (TaskAttachment, error) {
	taskID = strings.TrimSpace(taskID)
	attachmentID = strings.TrimSpace(attachmentID)
	if taskID == "" {
		return TaskAttachment{}, errors.New("task id is required")
	}
	if attachmentID == "" {
		return TaskAttachment{}, errors.New("attachment id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT
	id,
	task_id,
	stage,
	filename,
	content_type,
	size_bytes,
	storage_key,
	created_by,
	created_at
FROM task_attachments
WHERE task_id = ? AND id = ?`, taskID, attachmentID)

	return scanTaskAttachment(row)
}

func normalizeCreateTaskAttachmentInput(input CreateTaskAttachmentInput) (CreateTaskAttachmentInput, error) {
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.TaskID == "" {
		return CreateTaskAttachmentInput{}, errors.New("task id is required")
	}
	if input.Stage == "" {
		input.Stage = TaskAttachmentStageInitial
	}
	if err := validateTaskAttachmentStage(input.Stage); err != nil {
		return CreateTaskAttachmentInput{}, err
	}
	filename := cleanAttachmentFilename(input.Filename)
	if filename == "" {
		return CreateTaskAttachmentInput{}, errors.New("attachment filename is required")
	}
	input.Filename = filename
	input.ContentType = strings.TrimSpace(input.ContentType)
	if input.ContentType == "" {
		input.ContentType = defaultAttachmentContentType
	}
	if len(input.ContentType) > 255 {
		return CreateTaskAttachmentInput{}, errors.New("attachment content type is too long")
	}
	if input.CreatedBy == "" {
		input.CreatedBy = ActorHuman
	}
	if err := validateActor(input.CreatedBy); err != nil {
		return CreateTaskAttachmentInput{}, err
	}
	if input.Reader == nil {
		return CreateTaskAttachmentInput{}, errors.New("attachment reader is required")
	}

	return input, nil
}

func validateTaskAttachmentStage(stage TaskAttachmentStage) error {
	switch stage {
	case TaskAttachmentStageInitial, TaskAttachmentStageAuthor, TaskAttachmentStageReviewer, TaskAttachmentStageVerifier:
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

func allocateTaskAttachmentID(ctx context.Context, tx *sql.Tx) (string, error) {
	var nextNumber int
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = 'task_attachment'
RETURNING next_number - 1`).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate task attachment id: %w", err)
	}

	return formatTaskAttachmentID(nextNumber), nil
}

func formatTaskAttachmentID(number int) string {
	return fmt.Sprintf("att-%04d", number)
}

func taskExistsInTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE id = ?", taskID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return fmt.Errorf("lookup task: %w", err)
	}

	return nil
}

type taskAttachmentScanner interface {
	Scan(dest ...any) error
}

func scanTaskAttachment(scanner taskAttachmentScanner) (TaskAttachment, error) {
	var attachment TaskAttachment
	var stage string
	var createdBy string
	var createdAt string
	if err := scanner.Scan(
		&attachment.ID,
		&attachment.TaskID,
		&stage,
		&attachment.Filename,
		&attachment.ContentType,
		&attachment.SizeBytes,
		&attachment.StorageKey,
		&createdBy,
		&createdAt,
	); err != nil {
		return TaskAttachment{}, err
	}
	attachment.Stage = TaskAttachmentStage(stage)
	attachment.CreatedBy = Actor(createdBy)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return TaskAttachment{}, err
	}
	attachment.CreatedAt = parsedCreatedAt

	return attachment, nil
}

func scanTaskAttachments(rows *sql.Rows) ([]TaskAttachment, error) {
	var attachments []TaskAttachment
	for rows.Next() {
		attachment, err := scanTaskAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task attachment: %w", err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task attachments: %w", err)
	}

	return attachments, nil
}
