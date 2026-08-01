package blob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const digestMetadataKey = "flow-sha256"

type S3Encryption string

const (
	S3EncryptionAES256 S3Encryption = "AES256"
	S3EncryptionKMS    S3Encryption = "aws:kms"
)

// S3Client is the narrow AWS SDK v2 surface used by S3Store. It permits
// hermetic fault injection without weakening the production implementation.
type S3Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	ListObjectVersions(context.Context, *s3.ListObjectVersionsInput, ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	ListMultipartUploads(context.Context, *s3.ListMultipartUploadsInput, ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type S3Options struct {
	Client S3Client
	Bucket string
	Prefix string
	// EndpointURL documents the endpoint configured on Client. Empty selects
	// normal AWS SDK endpoints. Explicit custom endpoints require HTTPS unless
	// AllowHTTP is deliberately enabled for local development.
	EndpointURL   string
	AllowHTTP     bool
	Encryption    S3Encryption
	KMSKeyID      string
	BucketKey     bool
	MaxRangeBytes int64
}

type S3Store struct {
	client        S3Client
	bucket        string
	prefix        string
	encryption    types.ServerSideEncryption
	kmsKeyID      string
	bucketKey     bool
	maxRangeBytes int64

	reconcileMu     sync.Mutex
	reconcileCursor s3ReconcileCursor
}

func NewS3(options S3Options) (*S3Store, error) {
	if options.Client == nil || strings.TrimSpace(options.Bucket) == "" {
		return nil, fmt.Errorf("%w: S3 client and bucket are required", ErrInvalidConfig)
	}
	if options.EndpointURL != "" {
		endpoint, err := url.Parse(options.EndpointURL)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && !(options.AllowHTTP && endpoint.Scheme == "http")) {
			return nil, fmt.Errorf("%w: custom S3 endpoints require HTTPS", ErrInsecureEndpoint)
		}
	}
	prefix := strings.Trim(options.Prefix, "/")
	if strings.Contains(prefix, "\\") || strings.ContainsAny(prefix, "\r\n\x00") {
		return nil, fmt.Errorf("%w: unsafe S3 prefix", ErrInvalidConfig)
	}
	if prefix != "" {
		prefix += "/"
	}
	encryption := options.Encryption
	if encryption == "" {
		encryption = S3EncryptionAES256
	}
	var sdkEncryption types.ServerSideEncryption
	switch encryption {
	case S3EncryptionAES256:
		sdkEncryption = types.ServerSideEncryptionAes256
		if options.KMSKeyID != "" {
			return nil, fmt.Errorf("%w: KMS key requires aws:kms encryption", ErrInvalidConfig)
		}
	case S3EncryptionKMS:
		sdkEncryption = types.ServerSideEncryptionAwsKms
	default:
		return nil, fmt.Errorf("%w: unsupported S3 encryption", ErrInvalidConfig)
	}
	maximum := options.MaxRangeBytes
	if maximum == 0 {
		maximum = DefaultMaxRangeBytes
	}
	if maximum < 1 {
		return nil, fmt.Errorf("%w: max range bytes must be positive", ErrInvalidConfig)
	}
	return &S3Store{
		client: options.Client, bucket: options.Bucket, prefix: prefix,
		encryption: sdkEncryption, kmsKeyID: options.KMSKeyID,
		bucketKey: options.BucketKey, maxRangeBytes: maximum,
	}, nil
}

func (s *S3Store) Begin(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("generate temporary upload ID: %w", err)
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	upload := &s3Upload{
		store: s, id: id, writer: writer, hash: sha256.New(), createdAt: time.Now().UTC(),
		cancel: cancel, putDone: make(chan struct{}), writeGate: make(chan struct{}, 1),
		cleanupGate: make(chan struct{}, 1), state: s3UploadOpen,
	}
	upload.writeGate <- struct{}{}
	upload.cleanupGate <- struct{}{}
	go func() {
		output, err := s.client.PutObject(uploadCtx, s.putInput(s.temporaryKey(id), reader, -1, Digest{}, false))
		_ = reader.CloseWithError(err)
		upload.putResult = s3PutResult{output: output, err: err}
		close(upload.putDone)
	}()
	return upload, nil
}

type s3PutResult struct {
	output *s3.PutObjectOutput
	err    error
}

type s3UploadState uint8

const (
	s3UploadOpen s3UploadState = iota
	s3UploadCompleting
	s3UploadCompleted
	s3UploadAborting
	s3UploadAborted
)

type s3Upload struct {
	stateMu sync.Mutex
	state   s3UploadState

	store       *S3Store
	id          string
	writer      *io.PipeWriter
	writeGate   chan struct{}
	cleanupGate chan struct{}
	hash        interface {
		Sum([]byte) []byte
		Write([]byte) (int, error)
	}
	size      int64
	createdAt time.Time
	cancel    context.CancelFunc
	putDone   chan struct{}
	putResult s3PutResult
}

func (u *s3Upload) Write(p []byte) (int, error) {
	<-u.writeGate
	defer func() { u.writeGate <- struct{}{} }()

	u.stateMu.Lock()
	state := u.state
	u.stateMu.Unlock()
	switch state {
	case s3UploadAborting, s3UploadAborted:
		return 0, ErrUploadAborted
	case s3UploadOpen:
	default:
		return 0, ErrUploadClosed
	}

	// Pipe I/O may block until the SDK consumes the body. Abort closes the pipe
	// without taking writeGate, so a blocked writer is always releasable.
	n, err := u.writer.Write(p)
	if n > 0 {
		_, _ = u.hash.Write(p[:n])
		u.size += int64(n)
	}
	return n, err
}

func (u *s3Upload) Complete(ctx context.Context) (Temporary, error) {
	u.stateMu.Lock()
	switch u.state {
	case s3UploadOpen:
		u.state = s3UploadCompleting
	case s3UploadAborting, s3UploadAborted:
		u.stateMu.Unlock()
		return Temporary{}, ErrUploadAborted
	default:
		u.stateMu.Unlock()
		return Temporary{}, ErrUploadClosed
	}
	u.stateMu.Unlock()

	// Seal after any write that linearized before Complete. If that write is
	// blocked, cancellation closes the pipe directly and releases it.
	select {
	case <-u.writeGate:
	case <-ctx.Done():
		u.beginAbort(ctx.Err())
		_ = u.cleanup(ctx)
		return Temporary{}, ctx.Err()
	}
	closeErr := u.writer.Close()
	var expected Digest
	copy(expected[:], u.hash.Sum(nil))
	size := u.size
	u.writeGate <- struct{}{}
	if closeErr != nil {
		if u.isAborting() {
			return Temporary{}, ErrUploadAborted
		}
		u.beginAbort(closeErr)
		_ = u.cleanup(ctx)
		return Temporary{}, fmt.Errorf("close S3 upload stream: %w", closeErr)
	}

	select {
	case <-u.putDone:
	case <-ctx.Done():
		u.beginAbort(ctx.Err())
		_ = u.cleanup(ctx)
		return Temporary{}, ctx.Err()
	}
	u.cancel()
	if u.isAborting() {
		return Temporary{}, ErrUploadAborted
	}
	result := u.putResult
	if result.err != nil {
		u.beginAbort(result.err)
		_ = u.cleanup(ctx)
		return Temporary{}, fmt.Errorf("write S3 temporary upload: %w", classifyS3Error(result.err))
	}
	if result.output != nil && result.output.ChecksumSHA256 != nil {
		if err := verifyBase64Digest(*result.output.ChecksumSHA256, expected); err != nil {
			u.beginAbort(err)
			_ = u.cleanup(ctx)
			return Temporary{}, err
		}
	}
	actual, err := u.store.Resume(ctx, u.id)
	if err != nil {
		u.beginAbort(err)
		_ = u.cleanup(ctx)
		return Temporary{}, err
	}
	if actual.Digest != expected || actual.Size != size {
		u.beginAbort(ErrChecksumMismatch)
		_ = u.cleanup(ctx)
		return Temporary{}, ErrChecksumMismatch
	}

	u.stateMu.Lock()
	if u.state != s3UploadCompleting {
		u.stateMu.Unlock()
		return Temporary{}, ErrUploadAborted
	}
	u.state = s3UploadCompleted
	u.stateMu.Unlock()
	actual.CreatedAt = u.createdAt
	return actual, nil
}

func (u *s3Upload) Abort(ctx context.Context) error {
	u.beginAbort(ErrUploadAborted)
	return u.cleanup(ctx)
}

func (u *s3Upload) beginAbort(reason error) {
	u.stateMu.Lock()
	if u.state != s3UploadAborted {
		u.state = s3UploadAborting
	}
	u.stateMu.Unlock()
	u.cancel()
	_ = u.writer.CloseWithError(reason)
}

func (u *s3Upload) isAborting() bool {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	return u.state == s3UploadAborting || u.state == s3UploadAborted
}

func (u *s3Upload) cleanup(ctx context.Context) error {
	select {
	case <-u.putDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-u.cleanupGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { u.cleanupGate <- struct{}{} }()

	u.stateMu.Lock()
	if u.state == s3UploadAborted {
		u.stateMu.Unlock()
		return nil
	}
	u.stateMu.Unlock()
	// Deletion is deliberately ordered after PutObject exits. If the caller's
	// context expires first, no delete is issued; a later Abort can safely retry.
	if err := u.store.Abort(ctx, u.id); err != nil {
		return err
	}
	u.stateMu.Lock()
	u.state = s3UploadAborted
	u.stateMu.Unlock()
	return nil
}

func (s *S3Store) Resume(ctx context.Context, id string) (Temporary, error) {
	if !validUploadID(id) {
		return Temporary{}, ErrInvalidUpload
	}
	key := s.temporaryKey(id)
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Temporary{}, fmt.Errorf("head S3 temporary upload: %w", classifyS3Error(err))
	}
	body, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Temporary{}, fmt.Errorf("open S3 temporary upload: %w", classifyS3Error(err))
	}
	digest, size, err := hashBody(ctx, body.Body)
	if err != nil {
		return Temporary{}, fmt.Errorf("verify S3 temporary upload: %w", err)
	}
	if head.ContentLength != nil && *head.ContentLength != size {
		return Temporary{}, ErrChecksumMismatch
	}
	if head.ChecksumSHA256 != nil {
		if err := verifyBase64Digest(*head.ChecksumSHA256, digest); err != nil {
			return Temporary{}, err
		}
	}
	created := time.Time{}
	if head.LastModified != nil {
		created = *head.LastModified
	}
	return Temporary{ID: id, Digest: digest, Size: size, CreatedAt: created}, nil
}

func (s *S3Store) Publish(ctx context.Context, temporary Temporary, key Key) (Object, error) {
	if !key.valid() {
		return Object{}, ErrInvalidKey
	}
	if !temporary.valid() {
		return Object{}, ErrInvalidUpload
	}
	actual, err := s.Resume(ctx, temporary.ID)
	if err != nil {
		return Object{}, err
	}
	if actual.Digest != temporary.Digest || actual.Size != temporary.Size {
		return Object{}, ErrChecksumMismatch
	}
	source, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.temporaryKey(temporary.ID)), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Object{}, fmt.Errorf("open S3 publication source: %w", classifyS3Error(err))
	}
	defer source.Body.Close()
	output, putErr := s.client.PutObject(ctx, s.putInput(s.objectKey(key), source.Body, temporary.Size, temporary.Digest, true))
	if putErr != nil {
		existing, headErr := s.Head(ctx, key)
		if headErr == nil {
			if existing.Digest == temporary.Digest && existing.Size == temporary.Size {
				if err := s.Abort(ctx, temporary.ID); err != nil {
					return Object{}, err
				}
				return existing, nil
			}
			return Object{}, ErrConflict
		}
		if errors.Is(classifyS3Error(putErr), ErrConflict) {
			return Object{}, ErrConflict
		}
		return Object{}, fmt.Errorf("conditionally publish S3 blob: %w", classifyS3Error(putErr))
	}
	if output != nil && output.ChecksumSHA256 != nil {
		if err := verifyBase64Digest(*output.ChecksumSHA256, temporary.Digest); err != nil {
			return Object{}, err
		}
	}
	published, err := s.Head(ctx, key)
	if err != nil {
		return Object{}, err
	}
	if published.Digest != temporary.Digest || published.Size != temporary.Size {
		return Object{}, ErrChecksumMismatch
	}
	if err := s.Abort(ctx, temporary.ID); err != nil {
		return Object{}, err
	}
	return published, nil
}

func (s *S3Store) putInput(key string, body io.Reader, size int64, digest Digest, immutable bool) *s3.PutObjectInput {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body,
		ChecksumAlgorithm:    types.ChecksumAlgorithmSha256,
		ServerSideEncryption: s.encryption,
	}
	if s.kmsKeyID != "" {
		input.SSEKMSKeyId = aws.String(s.kmsKeyID)
	}
	if s.bucketKey {
		input.BucketKeyEnabled = aws.Bool(true)
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	if immutable {
		input.IfNoneMatch = aws.String("*")
		input.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(digest[:]))
		input.Metadata = map[string]string{digestMetadataKey: digest.String()}
	}
	// Omitting ACL is intentional: S3 objects are private by default and this
	// also works with bucket-owner-enforced buckets where ACL headers are denied.
	return input
}

func (s *S3Store) Head(ctx context.Context, key Key) (Object, error) {
	if !key.valid() {
		return Object{}, ErrInvalidKey
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(key)), ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return Object{}, fmt.Errorf("head S3 blob: %w", classifyS3Error(err))
	}
	var digest Digest
	value := output.Metadata[digestMetadataKey]
	if value != "" {
		digest, err = parseDigest(value)
	} else if output.ChecksumSHA256 != nil {
		digest, err = digestFromBase64(*output.ChecksumSHA256)
	} else {
		err = ErrChecksumMismatch
	}
	if err != nil {
		return Object{}, fmt.Errorf("read S3 blob digest: %w", err)
	}
	object := Object{Key: key, Digest: digest, ETag: aws.ToString(output.ETag), VersionID: aws.ToString(output.VersionId)}
	if output.ContentLength != nil {
		object.Size = *output.ContentLength
	}
	if output.LastModified != nil {
		object.Modified = *output.LastModified
	}
	return object, nil
}

func (s *S3Store) Open(ctx context.Context, key Key) (io.ReadCloser, error) {
	if !key.valid() {
		return nil, ErrInvalidKey
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(key)), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return nil, fmt.Errorf("open S3 blob: %w", classifyS3Error(err))
	}
	return &contextReadCloser{ctx: ctx, Reader: output.Body, closer: output.Body}, nil
}

func (s *S3Store) OpenRange(ctx context.Context, key Key, byteRange ByteRange) (RangeReader, error) {
	object, err := s.Head(ctx, key)
	if err != nil {
		return RangeReader{}, err
	}
	end, err := validateRange(byteRange, object.Size, s.maxRangeBytes)
	if err != nil {
		return RangeReader{}, err
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", byteRange.Offset, end-1)
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(key)), Range: aws.String(rangeHeader), ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return RangeReader{}, fmt.Errorf("open S3 blob range: %w", classifyS3Error(err))
	}
	limited := io.LimitReader(output.Body, end-byteRange.Offset)
	body := &contextReadCloser{ctx: ctx, Reader: limited, closer: output.Body}
	return RangeReader{Body: body, Start: byteRange.Offset, End: end, Size: object.Size}, nil
}

func (s *S3Store) Abort(ctx context.Context, id string) error {
	if !validUploadID(id) {
		return ErrInvalidUpload
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.temporaryKey(id))})
	if err != nil && !errors.Is(classifyS3Error(err), ErrNotFound) {
		return fmt.Errorf("delete S3 temporary upload: %w", classifyS3Error(err))
	}
	return nil
}

type s3ReconcileCursor struct {
	phase                   uint8
	versionKeyMarker        *string
	versionIDMarker         *string
	multipartKeyMarker      *string
	multipartUploadMarker   *string
	publishedContinuationID *string
}

func (s *S3Store) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	request, err := normalizeReconcileRequest(request)
	if err != nil {
		return ReconcileResult{}, err
	}

	// The cursor is store-local because the public reconciliation contract has no
	// cursor field. Calls are serialized so every bounded invocation advances the
	// scan rather than repeatedly inspecting page one.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	result := ReconcileResult{}
	inspected := 0
	for inspected < request.Limit {
		switch s.reconcileCursor.phase {
		case 0:
			remaining := request.Limit - inspected
			page, listErr := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
				Bucket: aws.String(s.bucket), Prefix: aws.String(s.temporaryPrefix()),
				MaxKeys: aws.Int32(int32(remaining)), KeyMarker: s.reconcileCursor.versionKeyMarker,
				VersionIdMarker: s.reconcileCursor.versionIDMarker,
			})
			if listErr != nil {
				return result, fmt.Errorf("list S3 temporary upload versions: %w", classifyS3Error(listErr))
			}
			entryCount := len(page.Versions) + len(page.DeleteMarkers)
			if entryCount > remaining {
				return result, fmt.Errorf("list S3 temporary upload versions: server exceeded requested page size")
			}
			if entryCount == 0 && aws.ToBool(page.IsTruncated) {
				// Count an advancing empty page as one unit of remote work so a
				// malformed or unusual service cannot create an unbounded loop.
				inspected++
			}
			for _, version := range page.Versions {
				inspected++
				id := strings.TrimPrefix(aws.ToString(version.Key), s.temporaryPrefix())
				if !validUploadID(id) || version.LastModified == nil || !version.LastModified.Before(request.Before) {
					continue
				}
				if _, live := request.LiveTemporaryIDs[id]; live {
					continue
				}
				if removeErr := s.removeTemporaryVersion(ctx, version.Key, version.VersionId, false); removeErr != nil {
					return result, removeErr
				}
				result.RemovedTemporaryIDs = append(result.RemovedTemporaryIDs, id)
			}
			for _, marker := range page.DeleteMarkers {
				inspected++
				id := strings.TrimPrefix(aws.ToString(marker.Key), s.temporaryPrefix())
				if !validUploadID(id) || marker.LastModified == nil || !marker.LastModified.Before(request.Before) {
					continue
				}
				if _, live := request.LiveTemporaryIDs[id]; live {
					continue
				}
				if removeErr := s.removeTemporaryVersion(ctx, marker.Key, marker.VersionId, true); removeErr != nil {
					return result, removeErr
				}
				result.RemovedTemporaryIDs = append(result.RemovedTemporaryIDs, id)
			}
			if aws.ToBool(page.IsTruncated) {
				nextKey, nextVersion, progressErr := nextS3VersionMarkers(
					s.reconcileCursor.versionKeyMarker, s.reconcileCursor.versionIDMarker,
					page.NextKeyMarker, page.NextVersionIdMarker,
				)
				if progressErr != nil {
					return result, fmt.Errorf("list S3 temporary upload versions: %w", progressErr)
				}
				s.reconcileCursor.versionKeyMarker = nextKey
				s.reconcileCursor.versionIDMarker = nextVersion
			} else {
				s.reconcileCursor.versionKeyMarker = nil
				s.reconcileCursor.versionIDMarker = nil
				s.reconcileCursor.phase = 1
			}
		case 1:
			remaining := request.Limit - inspected
			page, listErr := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
				Bucket: aws.String(s.bucket), Prefix: aws.String(s.temporaryPrefix()), MaxUploads: aws.Int32(int32(remaining)),
				KeyMarker: s.reconcileCursor.multipartKeyMarker, UploadIdMarker: s.reconcileCursor.multipartUploadMarker,
			})
			if listErr != nil {
				return result, fmt.Errorf("list S3 multipart uploads: %w", classifyS3Error(listErr))
			}
			if len(page.Uploads) > remaining {
				return result, fmt.Errorf("list S3 multipart uploads: server exceeded requested page size")
			}
			if len(page.Uploads) == 0 && aws.ToBool(page.IsTruncated) {
				inspected++
			}
			for _, upload := range page.Uploads {
				inspected++
				id := strings.TrimPrefix(aws.ToString(upload.Key), s.temporaryPrefix())
				if !validUploadID(id) || upload.Initiated == nil || !upload.Initiated.Before(request.Before) {
					continue
				}
				if _, live := request.LiveTemporaryIDs[id]; live {
					continue
				}
				_, abortErr := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
					Bucket: aws.String(s.bucket), Key: upload.Key, UploadId: upload.UploadId,
				})
				if abortErr != nil {
					return result, fmt.Errorf("abort stale S3 multipart upload: %w", classifyS3Error(abortErr))
				}
				result.AbortedMultipartIDs = append(result.AbortedMultipartIDs, aws.ToString(upload.UploadId))
			}
			if aws.ToBool(page.IsTruncated) {
				nextKey, nextUpload, progressErr := nextS3MultipartMarkers(
					s.reconcileCursor.multipartKeyMarker, s.reconcileCursor.multipartUploadMarker,
					page.NextKeyMarker, page.NextUploadIdMarker,
				)
				if progressErr != nil {
					return result, fmt.Errorf("list S3 multipart uploads: %w", progressErr)
				}
				s.reconcileCursor.multipartKeyMarker = nextKey
				s.reconcileCursor.multipartUploadMarker = nextUpload
			} else {
				s.reconcileCursor.multipartKeyMarker = nil
				s.reconcileCursor.multipartUploadMarker = nil
				s.reconcileCursor.phase = 2
			}
		case 2:
			remaining := request.Limit - inspected
			page, listErr := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(s.bucket), Prefix: aws.String(s.objectPrefix()),
				MaxKeys: aws.Int32(int32(remaining)), ContinuationToken: s.reconcileCursor.publishedContinuationID,
			})
			if listErr != nil {
				return result, fmt.Errorf("list S3 published blobs: %w", classifyS3Error(listErr))
			}
			if len(page.Contents) > remaining {
				return result, fmt.Errorf("list S3 published blobs: server exceeded requested page size")
			}
			if len(page.Contents) == 0 && aws.ToBool(page.IsTruncated) {
				inspected++
			}
			for _, listed := range page.Contents {
				inspected++
				value := strings.TrimPrefix(aws.ToString(listed.Key), s.objectPrefix())
				key, parseErr := ParseKey(value)
				if parseErr != nil {
					continue
				}
				if _, ok := request.ReferencedKeys[key]; ok {
					continue
				}
				if _, ok := request.PendingKeys[key]; ok {
					continue
				}
				object, headErr := s.Head(ctx, key)
				if headErr != nil {
					return result, headErr
				}
				result.Orphans = append(result.Orphans, object)
			}
			if aws.ToBool(page.IsTruncated) {
				next, progressErr := nextS3Token(s.reconcileCursor.publishedContinuationID, page.NextContinuationToken)
				if progressErr != nil {
					return result, fmt.Errorf("list S3 published blobs: %w", progressErr)
				}
				s.reconcileCursor.publishedContinuationID = next
			} else {
				s.reconcileCursor = s3ReconcileCursor{}
				result.Truncated = false
				return result, nil
			}
		default:
			s.reconcileCursor = s3ReconcileCursor{}
		}
	}
	result.Truncated = true
	return result, nil
}

func (s *S3Store) removeTemporaryVersion(ctx context.Context, key, versionID *string, deleteMarker bool) error {
	if deleteMarker && aws.ToString(versionID) == "" {
		return errors.New("delete stale S3 temporary marker: missing version ID")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: key, VersionId: versionID,
	})
	if err != nil && !errors.Is(classifyS3Error(err), ErrNotFound) {
		return fmt.Errorf("delete stale S3 temporary version: %w", classifyS3Error(err))
	}
	return nil
}

func nextS3Token(previous, next *string) (*string, error) {
	value := aws.ToString(next)
	if value == "" || value == aws.ToString(previous) {
		return nil, errors.New("truncated response did not advance continuation token")
	}
	return aws.String(value), nil
}

func nextS3VersionMarkers(previousKey, previousVersion, nextKey, nextVersion *string) (*string, *string, error) {
	key := aws.ToString(nextKey)
	version := aws.ToString(nextVersion)
	if key == "" || (key == aws.ToString(previousKey) && version == aws.ToString(previousVersion)) {
		return nil, nil, errors.New("truncated response did not advance version markers")
	}
	return aws.String(key), aws.String(version), nil
}

func nextS3MultipartMarkers(previousKey, previousUpload, nextKey, nextUpload *string) (*string, *string, error) {
	key := aws.ToString(nextKey)
	upload := aws.ToString(nextUpload)
	if key == "" || (key == aws.ToString(previousKey) && upload == aws.ToString(previousUpload)) {
		return nil, nil, errors.New("truncated response did not advance multipart markers")
	}
	return aws.String(key), aws.String(upload), nil
}

func (s *S3Store) temporaryPrefix() string       { return s.prefix + "tmp/" }
func (s *S3Store) temporaryKey(id string) string { return s.temporaryPrefix() + id }
func (s *S3Store) objectPrefix() string          { return s.prefix + "objects/" }
func (s *S3Store) objectKey(key Key) string      { return s.objectPrefix() + key.String() }

func hashBody(ctx context.Context, body io.ReadCloser) (Digest, int64, error) {
	defer body.Close()
	hash := sha256.New()
	reader := &contextReadCloser{ctx: ctx, Reader: body, closer: io.NopCloser(nilReader{})}
	size, err := io.Copy(hash, reader)
	if err != nil {
		return Digest{}, 0, err
	}
	var digest Digest
	copy(digest[:], hash.Sum(nil))
	return digest, size, nil
}

func verifyBase64Digest(value string, expected Digest) error {
	actual, err := digestFromBase64(value)
	if err != nil || actual != expected {
		return ErrChecksumMismatch
	}
	return nil
}

func digestFromBase64(value string) (Digest, error) {
	var digest Digest
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return Digest{}, ErrChecksumMismatch
	}
	copy(digest[:], decoded)
	return digest, nil
}

func classifyS3Error(err error) error {
	if err == nil {
		return nil
	}
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.HTTPStatusCode() {
		case 404:
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		case 409, 412:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		case "PreconditionFailed", "ConditionalRequestConflict":
			return fmt.Errorf("%w: %v", ErrConflict, err)
		case "BadDigest", "InvalidDigest":
			return fmt.Errorf("%w: %v", ErrChecksumMismatch, err)
		}
	}
	return err
}
