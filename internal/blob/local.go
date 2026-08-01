package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
	localLayoutName      = ".flow-blobs"
	localTemporaryName   = "tmp"
	localObjectsName     = "objects"
)

type LocalOptions struct {
	MaxRangeBytes int64
}

type Local struct {
	// root is the dedicated store-owned layout. rootDir, rather than this path,
	// anchors every operation against directory replacement and symlink races.
	root          string
	temporaryRoot string
	objectRoot    string
	rootDir       *os.File
	maxRangeBytes int64

	mu               sync.Mutex
	closed           bool
	closeErr         error
	activeUploads    map[string]struct{}
	completedUploads map[string]time.Time

	// Reconciliation directory descriptors retain scan position across bounded
	// passes. reconcileMu serializes both their use and closure.
	reconcileMu            sync.Mutex
	reconcileTemporary     *os.File
	reconcileObjects       *os.File
	reconcileNamespace     *os.File
	reconcileNamespaceName string
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

	callerRoot, err := openOrCreateAbsoluteDirectory(absolute)
	if err != nil {
		return nil, fmt.Errorf("open local blob parent without symlinks: %w", err)
	}
	layout, layoutCreated, err := openPrivateDirectoryAt(callerRoot, localLayoutName, true)
	if err == nil && layoutCreated {
		err = callerRoot.Sync()
	}
	_ = callerRoot.Close()
	if err != nil {
		if layout != nil {
			_ = layout.Close()
		}
		return nil, fmt.Errorf("open private local blob layout: %w", err)
	}
	for _, name := range []string{localTemporaryName, localObjectsName} {
		directory, created, childErr := openPrivateDirectoryAt(layout, name, true)
		if childErr == nil && created {
			childErr = layout.Sync()
		}
		if childErr != nil {
			if directory != nil {
				_ = directory.Close()
			}
			_ = layout.Close()
			return nil, fmt.Errorf("open private local blob %s directory: %w", name, childErr)
		}
		_ = directory.Close()
	}

	layoutPath := filepath.Join(absolute, localLayoutName)
	return &Local{
		root: layoutPath, temporaryRoot: filepath.Join(layoutPath, localTemporaryName),
		objectRoot: filepath.Join(layoutPath, localObjectsName), rootDir: layout,
		maxRangeBytes: maximum, activeUploads: make(map[string]struct{}), completedUploads: make(map[string]time.Time),
	}, nil
}

// openOrCreateAbsoluteDirectory anchors the configured root through its parent
// and refuses a symlink in the configured root itself. Existing caller-owned
// directories are never chmodded. Parent paths may contain platform-managed
// symlinks (for example /var on Darwin); all store traversal below the opened
// descriptor remains no-follow and cannot escape it.
func openOrCreateAbsoluteDirectory(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, ErrInvalidConfig
	}
	if clean == string(filepath.Separator) {
		fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), clean), nil
	}
	parentPath, name := filepath.Split(clean)
	parentPath = filepath.Clean(parentPath)
	if err := os.MkdirAll(parentPath, privateDirectoryMode); err != nil {
		return nil, err
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()
	created := false
	if err := unix.Mkdirat(int(parent.Fd()), name, privateDirectoryMode); err == nil {
		created = true
	} else if !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if created {
		if err := unix.Fchmod(fd, privateDirectoryMode); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		if err := parent.Sync(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), clean), nil
}

func openPrivateDirectoryAt(parent *os.File, name string, create bool) (*os.File, bool, error) {
	created := false
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), name, privateDirectoryMode); err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return nil, false, err
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	if created {
		if err := unix.Fchmod(fd, privateDirectoryMode); err != nil {
			_ = unix.Close(fd)
			return nil, false, err
		}
	}
	directory := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	if err := validatePrivateNode(directory, unix.S_IFDIR, privateDirectoryMode); err != nil {
		_ = directory.Close()
		return nil, false, err
	}
	return directory, created, nil
}

func openPrivateRegularAt(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, privateFileMode)
	if err != nil {
		return nil, err
	}
	if flags&unix.O_CREAT != 0 && flags&unix.O_EXCL != 0 {
		if err := unix.Fchmod(fd, privateFileMode); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	if err := validatePrivateNode(file, unix.S_IFREG, privateFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validatePrivateNode(file *os.File, nodeType uint32, mode uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if uint32(stat.Mode)&unix.S_IFMT != nodeType || uint32(stat.Mode)&0o777 != mode || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: unsafe pre-existing local blob node", ErrInvalidConfig)
	}
	return nil
}

func (s *Local) Close() error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.resetReconcileCursors()
	s.closeErr = s.rootDir.Close()
	return s.closeErr
}

func (s *Local) storeDirectory(name string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	directory, _, err := openPrivateDirectoryAt(s.rootDir, name, false)
	return directory, err
}

func (s *Local) beginDirectory(id string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	directory, _, err := openPrivateDirectoryAt(s.rootDir, localTemporaryName, false)
	if err != nil {
		return nil, err
	}
	s.activeUploads[id] = struct{}{}
	return directory, nil
}

func (s *Local) finishUpload(id string) {
	s.mu.Lock()
	delete(s.activeUploads, id)
	delete(s.completedUploads, id)
	s.mu.Unlock()
}

func (s *Local) completeUpload(id string, completedAt time.Time) {
	s.mu.Lock()
	delete(s.activeUploads, id)
	s.completedUploads[id] = completedAt
	s.mu.Unlock()
}

func (s *Local) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Local) Begin(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("generate temporary upload ID: %w", err)
	}
	temporary, err := s.beginDirectory(id)
	if err != nil {
		return nil, fmt.Errorf("open local temporary directory: %w", err)
	}
	defer temporary.Close()
	file, err := openPrivateRegularAt(temporary, id, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		s.finishUpload(id)
		return nil, fmt.Errorf("create local temporary upload: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(int(temporary.Fd()), id, 0)
		s.finishUpload(id)
		return nil, fmt.Errorf("fsync local temporary directory: %w", err)
	}
	return &localUpload{store: s, id: id, file: file, hash: sha256.New()}, nil
}

type localUpload struct {
	mu    sync.Mutex
	store *Local
	id    string
	file  *os.File
	hash  interface {
		Sum([]byte) []byte
		Write([]byte) (int, error)
	}
	size      int64
	completed bool
	aborted   bool
}

func (u *localUpload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.store.isClosed() {
		return 0, ErrStoreClosed
	}
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
	if u.store.isClosed() {
		return Temporary{}, ErrStoreClosed
	}
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
	completedAt := time.Now().UTC()
	timestamp := unix.NsecToTimeval(completedAt.UnixNano())
	if err := unix.Futimes(int(u.file.Fd()), []unix.Timeval{timestamp, timestamp}); err != nil {
		_ = u.file.Close()
		u.store.finishUpload(u.id)
		return Temporary{}, fmt.Errorf("refresh local temporary upload timestamp: %w", err)
	}
	if err := u.file.Sync(); err != nil {
		_ = u.file.Close()
		u.store.finishUpload(u.id)
		return Temporary{}, fmt.Errorf("fsync local temporary upload: %w", err)
	}
	if err := u.file.Close(); err != nil {
		u.store.finishUpload(u.id)
		return Temporary{}, fmt.Errorf("close local temporary upload: %w", err)
	}
	u.store.completeUpload(u.id, completedAt)
	var digest Digest
	copy(digest[:], u.hash.Sum(nil))
	return Temporary{ID: u.id, Digest: digest, Size: u.size, CreatedAt: completedAt}, nil
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
	u.store.finishUpload(u.id)
	return u.store.Abort(ctx, u.id)
}

func (s *Local) Resume(ctx context.Context, id string) (Temporary, error) {
	if err := ctx.Err(); err != nil {
		return Temporary{}, err
	}
	if !validUploadID(id) {
		return Temporary{}, ErrInvalidUpload
	}
	file, temporaryDirectory, err := s.openTemporary(id)
	if err != nil {
		return Temporary{}, err
	}
	defer temporaryDirectory.Close()
	defer file.Close()
	return inspectTemporaryFile(ctx, id, file)
}

func (s *Local) openTemporary(id string) (*os.File, *os.File, error) {
	temporary, err := s.storeDirectory(localTemporaryName)
	if err != nil {
		return nil, nil, fmt.Errorf("open local temporary directory: %w", err)
	}
	file, err := openPrivateRegularAt(temporary, id, unix.O_RDONLY)
	if err != nil {
		_ = temporary.Close()
		if errors.Is(err, unix.ENOENT) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("open local temporary upload: %w", err)
	}
	return file, temporary, nil
}

func inspectTemporaryFile(ctx context.Context, id string, file *os.File) (Temporary, error) {
	info, err := file.Stat()
	if err != nil {
		return Temporary{}, fmt.Errorf("stat local temporary upload: %w", err)
	}
	hash := sha256.New()
	reader := &contextReadCloser{ctx: ctx, Reader: file, closer: io.NopCloser(nilReader{})}
	if _, err := io.Copy(hash, reader); err != nil {
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
	source, temporaryDirectory, err := s.openTemporary(temporary.ID)
	if err != nil {
		return Object{}, err
	}
	defer temporaryDirectory.Close()
	defer source.Close()
	actual, err := inspectTemporaryFile(ctx, temporary.ID, source)
	if err != nil {
		return Object{}, err
	}
	if actual.Digest != temporary.Digest || actual.Size != temporary.Size {
		return Object{}, ErrChecksumMismatch
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("stat local publication source: %w", err)
	}
	namespace, objectName, err := s.openObjectNamespace(key, true)
	if err != nil {
		return Object{}, err
	}
	defer namespace.Close()

	if err := unix.Linkat(int(temporaryDirectory.Fd()), temporary.ID, int(namespace.Fd()), objectName, 0); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return Object{}, fmt.Errorf("atomically publish local blob: %w", err)
		}
		existingFile, openErr := openPrivateRegularAt(namespace, objectName, unix.O_RDONLY)
		if openErr != nil {
			return Object{}, fmt.Errorf("open existing local blob: %w", openErr)
		}
		existing, headErr := headLocalFile(ctx, key, existingFile)
		_ = existingFile.Close()
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

	destination, err := openPrivateRegularAt(namespace, objectName, unix.O_RDONLY)
	if err != nil {
		return Object{}, fmt.Errorf("verify published local blob: %w", err)
	}
	defer destination.Close()
	destinationInfo, err := destination.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("stat published local blob: %w", err)
	}
	if !os.SameFile(sourceInfo, destinationInfo) {
		return Object{}, fmt.Errorf("%w: publication source changed during link", ErrInvalidUpload)
	}
	if err := namespace.Sync(); err != nil {
		return Object{}, fmt.Errorf("fsync published blob directory: %w", err)
	}
	currentNamespace, _, err := s.openObjectNamespace(key, false)
	if err != nil {
		return Object{}, fmt.Errorf("verify current local object namespace: %w", err)
	}
	currentInfo, currentErr := currentNamespace.Stat()
	_ = currentNamespace.Close()
	namespaceInfo, namespaceErr := namespace.Stat()
	if currentErr != nil || namespaceErr != nil || !os.SameFile(currentInfo, namespaceInfo) {
		return Object{}, fmt.Errorf("%w: object namespace changed during publish", ErrInvalidUpload)
	}
	object, err := headLocalFile(ctx, key, destination)
	if err != nil {
		return Object{}, err
	}
	if err := s.Abort(ctx, temporary.ID); err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *Local) openObjectNamespace(key Key, create bool) (*os.File, string, error) {
	if !key.valid() {
		return nil, "", ErrInvalidKey
	}
	parts := strings.Split(key.value, "/")
	objects, err := s.storeDirectory(localObjectsName)
	if err != nil {
		return nil, "", fmt.Errorf("open local object root: %w", err)
	}
	namespace, created, err := openPrivateDirectoryAt(objects, parts[0], create)
	if err != nil {
		_ = objects.Close()
		if errors.Is(err, unix.ENOENT) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("open local object namespace: %w", err)
	}
	if created {
		if err := objects.Sync(); err != nil {
			_ = namespace.Close()
			_ = objects.Close()
			return nil, "", fmt.Errorf("fsync local object root: %w", err)
		}
	}
	_ = objects.Close()
	return namespace, parts[1], nil
}

// objectPath remains an internal diagnostic/test helper; storage operations do
// not use the returned path and are anchored to rootDir instead.
func (s *Local) objectPath(key Key, createParent bool) (string, error) {
	namespace, objectName, err := s.openObjectNamespace(key, createParent)
	if err != nil {
		return "", err
	}
	_ = namespace.Close()
	parts := strings.Split(key.value, "/")
	return filepath.Join(s.objectRoot, parts[0], objectName), nil
}

func (s *Local) Head(ctx context.Context, key Key) (Object, error) {
	file, err := s.openObject(key)
	if err != nil {
		return Object{}, err
	}
	defer file.Close()
	return headLocalFile(ctx, key, file)
}

func (s *Local) openObject(key Key) (*os.File, error) {
	namespace, objectName, err := s.openObjectNamespace(key, false)
	if err != nil {
		return nil, err
	}
	defer namespace.Close()
	file, err := openPrivateRegularAt(namespace, objectName, unix.O_RDONLY)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open local blob: %w", err)
	}
	return file, nil
}

func headLocalFile(ctx context.Context, key Key, file *os.File) (Object, error) {
	info, err := file.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("stat local blob: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Object{}, fmt.Errorf("seek local blob: %w", err)
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
	file, err := s.openObject(key)
	if err != nil {
		return nil, err
	}
	return &contextReadCloser{ctx: ctx, Reader: file, closer: file}, nil
}

func (s *Local) OpenRange(ctx context.Context, key Key, byteRange ByteRange) (RangeReader, error) {
	file, err := s.openObject(key)
	if err != nil {
		return RangeReader{}, err
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	delete(s.activeUploads, id)
	delete(s.completedUploads, id)
	temporary, _, err := openPrivateDirectoryAt(s.rootDir, localTemporaryName, false)
	if err != nil {
		return fmt.Errorf("open local temporary directory: %w", err)
	}
	defer temporary.Close()
	if err := unix.Unlinkat(int(temporary.Fd()), id, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove local temporary upload: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("fsync local temporary directory: %w", err)
	}
	return nil
}

func (s *Local) removeReconciledTemporary(ctx context.Context, temporary *os.File, id string, before time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrStoreClosed
	}
	if _, active := s.activeUploads[id]; active {
		return false, nil
	}
	if completedAt, completed := s.completedUploads[id]; completed && !completedAt.Before(before) {
		return false, nil
	}
	if err := unix.Unlinkat(int(temporary.Fd()), id, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("remove local temporary upload: %w", err)
	}
	delete(s.completedUploads, id)
	if err := temporary.Sync(); err != nil {
		return false, fmt.Errorf("fsync local temporary directory: %w", err)
	}
	return true, nil
}

func readDirectoryBatch(directory *os.File, limit int) ([]os.FileInfo, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	entries, err := directory.Readdir(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	return entries, errors.Is(err, io.EOF) || len(entries) < limit, nil
}

func (s *Local) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	request, err := normalizeReconcileRequest(request)
	if err != nil {
		return ReconcileResult{}, err
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	result := ReconcileResult{}
	if s.isClosed() {
		return result, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	examined := 0
	fail := func(err error) (ReconcileResult, error) {
		s.resetReconcileCursors()
		return result, err
	}

	if s.reconcileTemporary == nil {
		directory, err := s.storeDirectory(localTemporaryName)
		if err != nil {
			return fail(fmt.Errorf("open local temporary directory: %w", err))
		}
		s.reconcileTemporary = directory
	}
	for examined < request.Limit {
		entries, exhausted, err := readDirectoryBatch(s.reconcileTemporary, request.Limit-examined)
		if err != nil {
			return fail(fmt.Errorf("list local temporary uploads: %w", err))
		}
		examined += len(entries)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			id := entry.Name()
			if !entry.Mode().IsRegular() || !validUploadID(id) || !entry.ModTime().Before(request.Before) {
				continue
			}
			if _, live := request.LiveTemporaryIDs[id]; live {
				continue
			}
			removed, removeErr := s.removeReconciledTemporary(ctx, s.reconcileTemporary, id, request.Before)
			if removeErr != nil {
				return fail(removeErr)
			}
			if removed {
				result.RemovedTemporaryIDs = append(result.RemovedTemporaryIDs, id)
			}
		}
		if exhausted {
			if err := s.reconcileTemporary.Close(); err != nil {
				return fail(err)
			}
			s.reconcileTemporary = nil
			break
		}
		result.Truncated = true
		return result, nil
	}
	if examined >= request.Limit {
		result.Truncated = true
		return result, nil
	}

	if s.reconcileObjects == nil {
		directory, err := s.storeDirectory(localObjectsName)
		if err != nil {
			return fail(fmt.Errorf("open local object root: %w", err))
		}
		s.reconcileObjects = directory
	}
	for examined < request.Limit {
		if s.reconcileNamespace == nil {
			namespaces, exhausted, readErr := readDirectoryBatch(s.reconcileObjects, 1)
			if readErr != nil {
				return fail(fmt.Errorf("list local object namespaces: %w", readErr))
			}
			if len(namespaces) == 0 && exhausted {
				if err := s.reconcileObjects.Close(); err != nil {
					return fail(err)
				}
				s.reconcileObjects = nil
				return result, nil
			}
			examined++
			namespaceEntry := namespaces[0]
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			if !namespaceEntry.IsDir() || !isLowerHex(namespaceEntry.Name(), 32) {
				if examined >= request.Limit {
					result.Truncated = true
					return result, nil
				}
				continue
			}
			namespace, _, openErr := openPrivateDirectoryAt(s.reconcileObjects, namespaceEntry.Name(), false)
			if openErr != nil {
				return fail(fmt.Errorf("open local object namespace: %w", openErr))
			}
			s.reconcileNamespace = namespace
			s.reconcileNamespaceName = namespaceEntry.Name()
			if examined >= request.Limit {
				result.Truncated = true
				return result, nil
			}
		}

		files, exhausted, readErr := readDirectoryBatch(s.reconcileNamespace, request.Limit-examined)
		if readErr != nil {
			return fail(fmt.Errorf("list local object namespace: %w", readErr))
		}
		examined += len(files)
		for _, entry := range files {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			if !entry.Mode().IsRegular() || !isLowerHex(entry.Name(), 32) {
				continue
			}
			key, parseErr := ParseKey(s.reconcileNamespaceName + "/" + entry.Name())
			if parseErr != nil {
				continue
			}
			if _, ok := request.ReferencedKeys[key]; ok {
				continue
			}
			if _, ok := request.PendingKeys[key]; ok {
				continue
			}
			file, openErr := openPrivateRegularAt(s.reconcileNamespace, entry.Name(), unix.O_RDONLY)
			if openErr != nil {
				return fail(fmt.Errorf("open local published blob: %w", openErr))
			}
			object, headErr := headLocalFile(ctx, key, file)
			_ = file.Close()
			if headErr != nil {
				return fail(headErr)
			}
			if object.Modified.Before(request.Before) {
				result.Orphans = append(result.Orphans, object)
			}
		}
		if exhausted {
			if err := s.reconcileNamespace.Close(); err != nil {
				return fail(err)
			}
			s.reconcileNamespace = nil
			s.reconcileNamespaceName = ""
		}
		if examined >= request.Limit {
			result.Truncated = true
			return result, nil
		}
	}
	result.Truncated = true
	return result, nil
}

func (s *Local) resetReconcileCursors() {
	if s.reconcileNamespace != nil {
		_ = s.reconcileNamespace.Close()
		s.reconcileNamespace = nil
	}
	s.reconcileNamespaceName = ""
	if s.reconcileObjects != nil {
		_ = s.reconcileObjects.Close()
		s.reconcileObjects = nil
	}
	if s.reconcileTemporary != nil {
		_ = s.reconcileTemporary.Close()
		s.reconcileTemporary = nil
	}
}
