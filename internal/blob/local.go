package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
)

type LocalOptions struct {
	MaxRangeBytes int64
}

type Local struct {
	root          string
	temporaryRoot string
	objectRoot    string
	maxRangeBytes int64
}

func NewLocal(root string, options LocalOptions) (*Local, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: local root is required", ErrInvalidConfig)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve local blob root: %w", err)
	}
	maximum := options.MaxRangeBytes
	if maximum == 0 {
		maximum = DefaultMaxRangeBytes
	}
	if maximum < 1 {
		return nil, fmt.Errorf("%w: max range bytes must be positive", ErrInvalidConfig)
	}
	store := &Local{
		root: absolute, temporaryRoot: filepath.Join(absolute, "tmp"),
		objectRoot: filepath.Join(absolute, "objects"), maxRangeBytes: maximum,
	}
	for _, directory := range []string{store.root, store.temporaryRoot, store.objectRoot} {
		if err := os.MkdirAll(directory, privateDirectoryMode); err != nil {
			return nil, fmt.Errorf("create local blob directory: %w", err)
		}
		if err := os.Chmod(directory, privateDirectoryMode); err != nil {
			return nil, fmt.Errorf("secure local blob directory: %w", err)
		}
	}
	return store, nil
}

func (s *Local) Begin(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("generate temporary upload ID: %w", err)
	}
	path := filepath.Join(s.temporaryRoot, id)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("create local temporary upload: %w", err)
	}
	return &localUpload{store: s, id: id, path: path, file: file, hash: sha256.New(), createdAt: time.Now().UTC()}, nil
}

type localUpload struct {
	mu    sync.Mutex
	store *Local
	id    string
	path  string
	file  *os.File
	hash  interface {
		Sum([]byte) []byte
		Write([]byte) (int, error)
	}
	size      int64
	createdAt time.Time
	completed bool
	aborted   bool
}

func (u *localUpload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.aborted {
		return 0, ErrUploadAborted
	}
	if u.completed {
		return 0, ErrUploadClosed
	}
	n, err := u.file.Write(p)
	if n > 0 {
		_, _ = u.hash.Write(p[:n])
		u.size += int64(n)
	}
	return n, err
}

func (u *localUpload) Complete(ctx context.Context) (Temporary, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Temporary{}, err
	}
	if u.aborted {
		return Temporary{}, ErrUploadAborted
	}
	if u.completed {
		return Temporary{}, ErrUploadClosed
	}
	u.completed = true
	if err := u.file.Sync(); err != nil {
		_ = u.file.Close()
		return Temporary{}, fmt.Errorf("fsync local temporary upload: %w", err)
	}
	if err := u.file.Close(); err != nil {
		return Temporary{}, fmt.Errorf("close local temporary upload: %w", err)
	}
	var digest Digest
	copy(digest[:], u.hash.Sum(nil))
	return Temporary{ID: u.id, Digest: digest, Size: u.size, CreatedAt: u.createdAt}, nil
}

func (u *localUpload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.aborted {
		return nil
	}
	u.aborted = true
	if !u.completed {
		_ = u.file.Close()
	}
	return u.store.Abort(ctx, u.id)
}

func (s *Local) Resume(ctx context.Context, id string) (Temporary, error) {
	if err := ctx.Err(); err != nil {
		return Temporary{}, err
	}
	if !validUploadID(id) {
		return Temporary{}, ErrInvalidUpload
	}
	return s.inspectTemporary(ctx, id)
}

func (s *Local) inspectTemporary(ctx context.Context, id string) (Temporary, error) {
	path := filepath.Join(s.temporaryRoot, id)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Temporary{}, ErrNotFound
		}
		return Temporary{}, fmt.Errorf("open local temporary upload: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Temporary{}, fmt.Errorf("stat local temporary upload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Temporary{}, ErrInvalidUpload
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReadCloser{ctx: ctx, Reader: file, closer: io.NopCloser(nilReader{})}); err != nil {
		return Temporary{}, fmt.Errorf("hash local temporary upload: %w", err)
	}
	var digest Digest
	copy(digest[:], hash.Sum(nil))
	return Temporary{ID: id, Digest: digest, Size: info.Size(), CreatedAt: info.ModTime()}, nil
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

func (s *Local) Publish(ctx context.Context, temporary Temporary, key Key) (Object, error) {
	if !key.valid() {
		return Object{}, ErrInvalidKey
	}
	if !temporary.valid() {
		return Object{}, ErrInvalidUpload
	}
	actual, err := s.inspectTemporary(ctx, temporary.ID)
	if err != nil {
		return Object{}, err
	}
	if actual.Digest != temporary.Digest || actual.Size != temporary.Size {
		return Object{}, ErrChecksumMismatch
	}
	destination, err := s.objectPath(key, true)
	if err != nil {
		return Object{}, err
	}
	source := filepath.Join(s.temporaryRoot, temporary.ID)
	if err := os.Link(source, destination); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return Object{}, fmt.Errorf("atomically publish local blob: %w", err)
		}
		existing, headErr := s.Head(ctx, key)
		if headErr != nil {
			return Object{}, headErr
		}
		if existing.Digest != temporary.Digest || existing.Size != temporary.Size {
			return Object{}, ErrConflict
		}
		if err := s.Abort(ctx, temporary.ID); err != nil {
			return Object{}, err
		}
		return existing, nil
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return Object{}, fmt.Errorf("fsync published blob directory: %w", err)
	}
	if err := s.Abort(ctx, temporary.ID); err != nil {
		return Object{}, err
	}
	return s.Head(ctx, key)
}

func (s *Local) objectPath(key Key, createParent bool) (string, error) {
	if !key.valid() {
		return "", ErrInvalidKey
	}
	path := filepath.Join(s.objectRoot, filepath.FromSlash(key.value))
	relative, err := filepath.Rel(s.objectRoot, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return "", ErrInvalidKey
	}
	if createParent {
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, privateDirectoryMode); err != nil {
			return "", fmt.Errorf("create local object namespace: %w", err)
		}
		if err := os.Chmod(parent, privateDirectoryMode); err != nil {
			return "", fmt.Errorf("secure local object namespace: %w", err)
		}
		if err := syncDirectory(s.objectRoot); err != nil {
			return "", fmt.Errorf("fsync local object root: %w", err)
		}
	}
	return path, nil
}

func (s *Local) Head(ctx context.Context, key Key) (Object, error) {
	path, err := s.objectPath(key, false)
	if err != nil {
		return Object{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Object{}, ErrNotFound
		}
		return Object{}, fmt.Errorf("open local blob: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("stat local blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Object{}, ErrNotFound
	}
	hash := sha256.New()
	reader := &contextReadCloser{ctx: ctx, Reader: file, closer: io.NopCloser(nilReader{})}
	if _, err := io.Copy(hash, reader); err != nil {
		return Object{}, fmt.Errorf("hash local blob: %w", err)
	}
	var digest Digest
	copy(digest[:], hash.Sum(nil))
	return Object{Key: key, Digest: digest, Size: info.Size(), Modified: info.ModTime()}, nil
}

func (s *Local) Open(ctx context.Context, key Key) (io.ReadCloser, error) {
	path, err := s.objectPath(key, false)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open local blob: %w", err)
	}
	return &contextReadCloser{ctx: ctx, Reader: file, closer: file}, nil
}

func (s *Local) OpenRange(ctx context.Context, key Key, byteRange ByteRange) (RangeReader, error) {
	path, err := s.objectPath(key, false)
	if err != nil {
		return RangeReader{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RangeReader{}, ErrNotFound
		}
		return RangeReader{}, fmt.Errorf("open local blob range: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return RangeReader{}, fmt.Errorf("stat local blob range: %w", err)
	}
	end, err := validateRange(byteRange, info.Size(), s.maxRangeBytes)
	if err != nil {
		file.Close()
		return RangeReader{}, err
	}
	section := io.NewSectionReader(file, byteRange.Offset, end-byteRange.Offset)
	body := &contextReadCloser{ctx: ctx, Reader: section, closer: file}
	return RangeReader{Body: body, Start: byteRange.Offset, End: end, Size: info.Size()}, nil
}

func (s *Local) Abort(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validUploadID(id) {
		return ErrInvalidUpload
	}
	if err := os.Remove(filepath.Join(s.temporaryRoot, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove local temporary upload: %w", err)
	}
	if err := syncDirectory(s.temporaryRoot); err != nil {
		return fmt.Errorf("fsync local temporary directory: %w", err)
	}
	return nil
}

func (s *Local) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	request, err := normalizeReconcileRequest(request)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{}
	entries, err := os.ReadDir(s.temporaryRoot)
	if err != nil {
		return result, fmt.Errorf("list local temporary uploads: %w", err)
	}
	for _, entry := range entries {
		if len(result.RemovedTemporaryIDs)+len(result.Orphans) >= request.Limit {
			result.Truncated = true
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		id := entry.Name()
		if entry.IsDir() || !validUploadID(id) {
			continue
		}
		if _, live := request.LiveTemporaryIDs[id]; live {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return result, fmt.Errorf("stat local temporary upload: %w", err)
		}
		if !info.ModTime().Before(request.Before) {
			continue
		}
		if err := s.Abort(ctx, id); err != nil {
			return result, err
		}
		result.RemovedTemporaryIDs = append(result.RemovedTemporaryIDs, id)
	}
	err = filepath.WalkDir(s.objectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if len(result.RemovedTemporaryIDs)+len(result.Orphans) >= request.Limit {
			result.Truncated = true
			return fs.SkipAll
		}
		relative, err := filepath.Rel(s.objectRoot, path)
		if err != nil {
			return err
		}
		key, err := ParseKey(filepath.ToSlash(relative))
		if err != nil {
			return nil
		}
		if _, ok := request.ReferencedKeys[key]; ok {
			return nil
		}
		if _, ok := request.PendingKeys[key]; ok {
			return nil
		}
		object, err := s.Head(ctx, key)
		if err != nil {
			return err
		}
		result.Orphans = append(result.Orphans, object)
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("scan local published blobs: %w", err)
	}
	return result, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
