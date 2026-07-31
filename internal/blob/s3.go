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
		cancel: cancel, result: make(chan s3PutResult, 1),
	}
	go func() {
		output, err := s.client.PutObject(uploadCtx, s.putInput(s.temporaryKey(id), reader, -1, Digest{}, false))
		_ = reader.CloseWithError(err)
		upload.result <- s3PutResult{output: output, err: err}
	}()
	return upload, nil
}

type s3PutResult struct {
	output *s3.PutObjectOutput
	err    error
}

type s3Upload struct {
	mu     sync.Mutex
	store  *S3Store
	id     string
	writer *io.PipeWriter
	hash   interface {
		Sum([]byte) []byte
		Write([]byte) (int, error)
	}
	size      int64
	createdAt time.Time
	cancel    context.CancelFunc
	result    chan s3PutResult
	closed    bool
	aborted   bool
}

func (u *s3Upload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.aborted {
		return 0, ErrUploadAborted
	}
	if u.closed {
		return 0, ErrUploadClosed
	}
	n, err := u.writer.Write(p)
	if n > 0 {
		_, _ = u.hash.Write(p[:n])
		u.size += int64(n)
	}
	return n, err
}

func (u *s3Upload) Complete(ctx context.Context) (Temporary, error) {
	u.mu.Lock()
	if u.aborted {
		u.mu.Unlock()
		return Temporary{}, ErrUploadAborted
	}
	if u.closed {
		u.mu.Unlock()
		return Temporary{}, ErrUploadClosed
	}
	u.closed = true
	closeErr := u.writer.Close()
	var expected Digest
	copy(expected[:], u.hash.Sum(nil))
	size := u.size
	u.mu.Unlock()
	if closeErr != nil {
		u.cancel()
		return Temporary{}, fmt.Errorf("close S3 upload stream: %w", closeErr)
	}
	var result s3PutResult
	select {
	case result = <-u.result:
	case <-ctx.Done():
		u.cancel()
		_ = u.store.Abort(context.WithoutCancel(ctx), u.id)
		return Temporary{}, ctx.Err()
	}
	u.cancel()
	if result.err != nil {
		return Temporary{}, fmt.Errorf("write S3 temporary upload: %w", classifyS3Error(result.err))
	}
	if result.output != nil && result.output.ChecksumSHA256 != nil {
		if err := verifyBase64Digest(*result.output.ChecksumSHA256, expected); err != nil {
			_ = u.store.Abort(context.WithoutCancel(ctx), u.id)
			return Temporary{}, err
		}
	}
	actual, err := u.store.Resume(ctx, u.id)
	if err != nil {
		return Temporary{}, err
	}
	if actual.Digest != expected || actual.Size != size {
		_ = u.store.Abort(context.WithoutCancel(ctx), u.id)
		return Temporary{}, ErrChecksumMismatch
	}
	actual.CreatedAt = u.createdAt
	return actual, nil
}

func (u *s3Upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	if u.aborted {
		u.mu.Unlock()
		return nil
	}
	u.aborted = true
	u.cancel()
	_ = u.writer.CloseWithError(ErrUploadAborted)
	closed := u.closed
	u.mu.Unlock()
	// Do not race deletion against a still-finishing PutObject. Complete has
	// already consumed the result when closed is true; otherwise cancellation
	// and closing the pipe make a conforming SDK client return promptly.
	if !closed {
		select {
		case <-u.result:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return u.store.Abort(ctx, u.id)
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

func (s *S3Store) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	request, err := normalizeReconcileRequest(request)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{}
	maxKeys := int32(request.Limit + 1)
	temporaries, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.temporaryPrefix()), MaxKeys: aws.Int32(maxKeys),
	})
	if err != nil {
		return result, fmt.Errorf("list S3 temporary uploads: %w", classifyS3Error(err))
	}
	for _, object := range temporaries.Contents {
		if len(result.RemovedTemporaryIDs)+len(result.AbortedMultipartIDs)+len(result.Orphans) >= request.Limit {
			result.Truncated = true
			return result, nil
		}
		id := strings.TrimPrefix(aws.ToString(object.Key), s.temporaryPrefix())
		if !validUploadID(id) || object.LastModified == nil || !object.LastModified.Before(request.Before) {
			continue
		}
		if _, live := request.LiveTemporaryIDs[id]; live {
			continue
		}
		if err := s.Abort(ctx, id); err != nil {
			return result, err
		}
		result.RemovedTemporaryIDs = append(result.RemovedTemporaryIDs, id)
	}
	multiparts, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.temporaryPrefix()), MaxUploads: aws.Int32(maxKeys),
	})
	if err != nil {
		return result, fmt.Errorf("list S3 multipart uploads: %w", classifyS3Error(err))
	}
	for _, upload := range multiparts.Uploads {
		if len(result.RemovedTemporaryIDs)+len(result.AbortedMultipartIDs)+len(result.Orphans) >= request.Limit {
			result.Truncated = true
			return result, nil
		}
		id := strings.TrimPrefix(aws.ToString(upload.Key), s.temporaryPrefix())
		if !validUploadID(id) || upload.Initiated == nil || !upload.Initiated.Before(request.Before) {
			continue
		}
		if _, live := request.LiveTemporaryIDs[id]; live {
			continue
		}
		_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: upload.Key, UploadId: upload.UploadId,
		})
		if err != nil {
			return result, fmt.Errorf("abort stale S3 multipart upload: %w", classifyS3Error(err))
		}
		result.AbortedMultipartIDs = append(result.AbortedMultipartIDs, aws.ToString(upload.UploadId))
	}
	objects, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.objectPrefix()), MaxKeys: aws.Int32(maxKeys),
	})
	if err != nil {
		return result, fmt.Errorf("list S3 published blobs: %w", classifyS3Error(err))
	}
	for _, listed := range objects.Contents {
		if len(result.RemovedTemporaryIDs)+len(result.AbortedMultipartIDs)+len(result.Orphans) >= request.Limit {
			result.Truncated = true
			return result, nil
		}
		value := strings.TrimPrefix(aws.ToString(listed.Key), s.objectPrefix())
		key, err := ParseKey(value)
		if err != nil {
			continue
		}
		if _, ok := request.ReferencedKeys[key]; ok {
			continue
		}
		if _, ok := request.PendingKeys[key]; ok {
			continue
		}
		object, err := s.Head(ctx, key)
		if err != nil {
			return result, err
		}
		result.Orphans = append(result.Orphans, object)
	}
	result.Truncated = aws.ToBool(temporaries.IsTruncated) || aws.ToBool(multiparts.IsTruncated) || aws.ToBool(objects.IsTruncated)
	return result, nil
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
