// Package blob provides immutable, streaming blob storage primitives.
//
// Keys are opaque, scope-derived identifiers. Authorization and the mapping
// from a key to a project or capture remain the coordinator's responsibility;
// callers must never use Head or digest comparisons as a cross-scope oracle.
package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// DefaultMaxRangeBytes bounds one range request when a backend-specific
	// value is not configured.
	DefaultMaxRangeBytes  int64 = 8 << 20
	defaultReconcileLimit       = 1000
	maxReconcileLimit           = 10000
)

var (
	ErrNotFound         = errors.New("blob not found")
	ErrConflict         = errors.New("blob key already contains different content")
	ErrInvalidKey       = errors.New("invalid blob key")
	ErrInvalidRange     = errors.New("invalid blob byte range")
	ErrInvalidUpload    = errors.New("invalid temporary upload")
	ErrUploadClosed     = errors.New("temporary upload is closed")
	ErrUploadAborted    = errors.New("temporary upload is aborted")
	ErrStoreClosed      = errors.New("blob store is closed")
	ErrChecksumMismatch = errors.New("blob checksum mismatch")
	ErrInsecureEndpoint = errors.New("insecure object-store endpoint")
	ErrInvalidConfig    = errors.New("invalid blob-store configuration")
)

// Digest is a SHA-256 content digest computed by a backend while bytes stream
// through it. It is metadata, not an object key.
type Digest [sha256.Size]byte

func (d Digest) String() string { return hex.EncodeToString(d[:]) }

func parseDigest(value string) (Digest, error) {
	var digest Digest
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return Digest{}, ErrChecksumMismatch
	}
	copy(digest[:], decoded)
	return digest, nil
}

// Key is an opaque, path-safe, scope-derived object identifier. Its two fixed
// hexadecimal components prevent caller-controlled path traversal.
type Key struct{ value string }

// NewKey returns a fresh key in a namespace derived from scope. The digest of
// the content is deliberately not part of the key.
func NewKey(scope string) (Key, error) {
	if strings.TrimSpace(scope) == "" {
		return Key{}, fmt.Errorf("%w: scope is required", ErrInvalidKey)
	}
	scopeDigest := sha256.Sum256([]byte(scope))
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return Key{}, fmt.Errorf("generate blob key: %w", err)
	}
	return ParseKey(hex.EncodeToString(scopeDigest[:16]) + "/" + hex.EncodeToString(random))
}

// ParseKey validates a previously persisted opaque key.
func ParseKey(value string) (Key, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !isLowerHex(parts[0], 32) || !isLowerHex(parts[1], 32) {
		return Key{}, fmt.Errorf("%w: malformed opaque key", ErrInvalidKey)
	}
	return Key{value: value}, nil
}

func (k Key) String() string { return k.value }

func (k Key) valid() bool {
	parsed, err := ParseKey(k.value)
	return err == nil && parsed == k
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func newUploadID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func validUploadID(id string) bool { return isLowerHex(id, 32) }

// Temporary identifies a completed private upload that has not necessarily
// been published. ID is opaque but persistable for commit/response-loss repair.
type Temporary struct {
	ID        string
	Digest    Digest
	Size      int64
	CreatedAt time.Time
}

func (t Temporary) valid() bool { return validUploadID(t.ID) && t.Size >= 0 }

// Object describes one immutable published blob.
type Object struct {
	Key       Key
	Digest    Digest
	Size      int64
	Modified  time.Time
	ETag      string
	VersionID string
}

// ByteRange is a bounded half-open request [Offset, Offset+Length).
type ByteRange struct {
	Offset int64
	Length int64
}

// RangeReader reports the actual returned half-open range. End never exceeds
// the object size, and Body is additionally limited against backend over-read.
type RangeReader struct {
	Body  io.ReadCloser
	Start int64
	End   int64
	Size  int64
}

// Upload accepts one sequential stream. Complete seals the private temporary;
// Abort is idempotent and should be used whenever publication is abandoned.
type Upload interface {
	io.Writer
	Complete(context.Context) (Temporary, error)
	Abort(context.Context) error
}

// Store is the low-level internal blob interface. Published keys are immutable.
type Store interface {
	Begin(context.Context) (Upload, error)
	Resume(context.Context, string) (Temporary, error)
	Publish(context.Context, Temporary, Key) (Object, error)
	Head(context.Context, Key) (Object, error)
	Open(context.Context, Key) (io.ReadCloser, error)
	OpenRange(context.Context, Key, ByteRange) (RangeReader, error)
	Abort(context.Context, string) error
	Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error)
}

// Closer is implemented by stores that retain resources requiring explicit
// release. Callers may type-assert it without requiring every backend to close.
type Closer interface {
	Close() error
}

// ReconcileRequest carries coordinator metadata into a conservative bounded
// scan. Stale temporaries not in LiveTemporaryIDs may be removed. Published
// objects absent from both key sets are only reported, never deleted.
type ReconcileRequest struct {
	Before           time.Time
	LiveTemporaryIDs map[string]struct{}
	ReferencedKeys   map[Key]struct{}
	PendingKeys      map[Key]struct{}
	Limit            int
}

// ReconcileResult is suitable for metrics and a coordinator's two-pass orphan
// quarantine. Truncated asks the caller to schedule another bounded scan.
type ReconcileResult struct {
	RemovedTemporaryIDs []string
	AbortedMultipartIDs []string
	Orphans             []Object
	Truncated           bool
}

func normalizeReconcileRequest(request ReconcileRequest) (ReconcileRequest, error) {
	if request.Before.IsZero() {
		return request, fmt.Errorf("%w: reconciliation cutoff is required", ErrInvalidConfig)
	}
	if request.Limit == 0 {
		request.Limit = defaultReconcileLimit
	}
	if request.Limit < 0 || request.Limit > maxReconcileLimit {
		return request, fmt.Errorf("%w: reconciliation limit must be between 1 and %d", ErrInvalidConfig, maxReconcileLimit)
	}
	return request, nil
}

func validateRange(byteRange ByteRange, size, maximum int64) (int64, error) {
	if byteRange.Offset < 0 || byteRange.Length <= 0 || byteRange.Length > maximum || byteRange.Offset >= size {
		return 0, ErrInvalidRange
	}
	end := byteRange.Offset + byteRange.Length
	if end < byteRange.Offset {
		return 0, ErrInvalidRange
	}
	if end > size {
		end = size
	}
	return end, nil
}

type contextReadCloser struct {
	ctx context.Context
	io.Reader
	closer io.Closer
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.Reader.Read(p)
	}
}

func (r *contextReadCloser) Close() error { return r.closer.Close() }
