package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type fakeS3Object struct {
	data        []byte
	metadata    map[string]string
	checksum    string
	modified    time.Time
	etag        string
	version     string
	encryption  types.ServerSideEncryption
	kmsKey      string
	conditional bool
}

type fakeS3 struct {
	mu              sync.Mutex
	objects         map[string]fakeS3Object
	multiparts      []types.MultipartUpload
	aborted         []string
	badNextChecksum bool
	puts            []fakeS3Object
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: make(map[string]fakeS3Object)} }

func (f *fakeS3) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(&contextReader{ctx: ctx, reader: input.Body})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	checksum := base64.StdEncoding.EncodeToString(digest[:])
	if input.ChecksumSHA256 != nil && *input.ChecksumSHA256 != checksum {
		return nil, &smithy.GenericAPIError{Code: "BadDigest", Message: "checksum mismatch"}
	}
	key := aws.ToString(input.Key)
	f.mu.Lock()
	defer f.mu.Unlock()
	if aws.ToString(input.IfNoneMatch) == "*" {
		if _, exists := f.objects[key]; exists {
			return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"}
		}
	}
	metadata := make(map[string]string, len(input.Metadata))
	for k, v := range input.Metadata {
		metadata[strings.ToLower(k)] = v
	}
	object := fakeS3Object{
		data: append([]byte(nil), data...), metadata: metadata, checksum: checksum,
		modified: time.Now().UTC(), etag: fmt.Sprintf("etag-%d", len(f.objects)+1),
		version: fmt.Sprintf("v%d", len(f.objects)+1), encryption: input.ServerSideEncryption,
		kmsKey: aws.ToString(input.SSEKMSKeyId), conditional: aws.ToString(input.IfNoneMatch) == "*",
	}
	f.objects[key] = object
	f.puts = append(f.puts, object)
	if f.badNextChecksum {
		f.badNextChecksum = false
		return &s3.PutObjectOutput{ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(make([]byte, 32)))}, nil
	}
	return &s3.PutObjectOutput{ChecksumSHA256: aws.String(checksum), ETag: aws.String(object.etag), VersionId: aws.String(object.version)}, nil
}

func (f *fakeS3) GetObject(ctx context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	object, ok := f.objects[aws.ToString(input.Key)]
	f.mu.Unlock()
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	data := object.data
	if input.Range != nil {
		var start, end int
		if _, err := fmt.Sscanf(*input.Range, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || start >= len(data) {
			return nil, &smithy.GenericAPIError{Code: "InvalidRange", Message: "range"}
		}
		if end >= len(data) {
			end = len(data) - 1
		}
		data = data[start : end+1]
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data))), ChecksumSHA256: aws.String(object.checksum)}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(object.data))), ChecksumSHA256: aws.String(object.checksum),
		Metadata: object.metadata, LastModified: aws.Time(object.modified), ETag: aws.String(object.etag), VersionId: aws.String(object.version),
		ServerSideEncryption: object.encryption,
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := aws.ToString(input.Prefix)
	keys := make([]string, 0)
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit := len(keys)
	if input.MaxKeys != nil && limit > int(*input.MaxKeys) {
		limit = int(*input.MaxKeys)
	}
	output := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(limit < len(keys))}
	for _, key := range keys[:limit] {
		object := f.objects[key]
		output.Contents = append(output.Contents, types.Object{Key: aws.String(key), LastModified: aws.Time(object.modified), Size: aws.Int64(int64(len(object.data)))})
	}
	return output, nil
}

func (f *fakeS3) ListMultipartUploads(_ context.Context, input *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	output := &s3.ListMultipartUploadsOutput{IsTruncated: aws.Bool(false)}
	for _, upload := range f.multiparts {
		if strings.HasPrefix(aws.ToString(upload.Key), aws.ToString(input.Prefix)) {
			output.Uploads = append(output.Uploads, upload)
		}
	}
	return output, nil
}

func (f *fakeS3) AbortMultipartUpload(_ context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, aws.ToString(input.UploadId))
	return &s3.AbortMultipartUploadOutput{}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}

func TestS3ConfigurationRequiresTLSAndEncryption(t *testing.T) {
	client := newFakeS3()
	if _, err := NewS3(S3Options{Client: client, Bucket: "bucket", EndpointURL: "http://objects.example"}); !errors.Is(err, ErrInsecureEndpoint) {
		t.Fatalf("NewS3(http) error = %v", err)
	}
	if _, err := NewS3(S3Options{Client: client, Bucket: "bucket", EndpointURL: "http://127.0.0.1:9000", AllowHTTP: true}); err != nil {
		t.Fatalf("explicit development HTTP should work: %v", err)
	}
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket", EndpointURL: "https://objects.example", Encryption: S3EncryptionKMS, KMSKeyID: "key-id"})
	if err != nil {
		t.Fatal(err)
	}
	temporary := completeUpload(t, context.Background(), store, []byte("encrypted"))
	client.mu.Lock()
	put := client.puts[0]
	client.mu.Unlock()
	if put.encryption != types.ServerSideEncryptionAwsKms || put.kmsKey != "key-id" {
		t.Fatalf("temporary encryption = %q KMS %q", put.encryption, put.kmsKey)
	}
	if err := store.Abort(context.Background(), temporary.ID); err != nil {
		t.Fatal(err)
	}
}

func TestS3StreamingLifecycleRangeAndImmutablePublish(t *testing.T) {
	client := newFakeS3()
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket", Prefix: "flow", MaxRangeBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, _ := NewKey("capture:s3")
	content := []byte("abcdefgh")
	temporary := completeUpload(t, ctx, store, content)
	object, err := store.Publish(ctx, temporary, key)
	if err != nil {
		t.Fatal(err)
	}
	if object.Size != 8 || object.Digest != Digest(sha256.Sum256(content)) || object.VersionID == "" {
		t.Fatalf("object = %+v", object)
	}
	client.mu.Lock()
	puts := append([]fakeS3Object(nil), client.puts...)
	client.mu.Unlock()
	if len(puts) != 2 || puts[0].encryption != types.ServerSideEncryptionAes256 || puts[1].encryption != types.ServerSideEncryptionAes256 || !puts[1].conditional {
		t.Fatalf("puts do not enforce default SSE and conditional final create: %+v", puts)
	}
	if _, exists := client.objects[store.temporaryKey(temporary.ID)]; exists {
		t.Fatal("published temporary was not removed")
	}
	reader, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(reader)
	reader.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("Open() = %q", got)
	}
	ranged, err := store.OpenRange(ctx, key, ByteRange{Offset: 3, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if string(got) != "defg" {
		t.Fatalf("OpenRange() = %q", got)
	}
	if _, err := store.OpenRange(ctx, key, ByteRange{Length: 5}); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("oversized range error = %v", err)
	}

	same := completeUpload(t, ctx, store, content)
	if _, err := store.Publish(ctx, same, key); err != nil {
		t.Fatalf("idempotent publish = %v", err)
	}
	different := completeUpload(t, ctx, store, []byte("different"))
	if _, err := store.Publish(ctx, different, key); !errors.Is(err, ErrConflict) {
		t.Fatalf("different publish error = %v", err)
	}
}

func TestS3AbortWaitsForStreamingPut(t *testing.T) {
	client := newFakeS3()
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := upload.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for key := range client.objects {
		if strings.Contains(key, "/tmp/") || strings.HasPrefix(key, "tmp/") {
			t.Fatalf("aborted upload remains at %q", key)
		}
	}
}

func TestS3ChecksumFailureAndReconciliation(t *testing.T) {
	client := newFakeS3()
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket", Prefix: "flow"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client.badNextChecksum = true
	upload, _ := store.Begin(ctx)
	_, _ = upload.Write([]byte("corrupt response"))
	if _, err := upload.Complete(ctx); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Complete() error = %v, want ErrChecksumMismatch", err)
	}

	old := time.Now().Add(-2 * time.Hour)
	staleID := strings.Repeat("a", 32)
	liveID := strings.Repeat("b", 32)
	for id, data := range map[string]string{staleID: "stale", liveID: "live"} {
		digest := sha256.Sum256([]byte(data))
		client.objects[store.temporaryKey(id)] = fakeS3Object{data: []byte(data), checksum: base64.StdEncoding.EncodeToString(digest[:]), modified: old}
	}
	client.multiparts = []types.MultipartUpload{{Key: aws.String(store.temporaryKey(strings.Repeat("c", 32))), UploadId: aws.String("multipart-1"), Initiated: aws.Time(old)}}
	orphanKey, _ := NewKey("s3-orphan")
	orphan := completeUpload(t, ctx, store, []byte("orphan"))
	if _, err := store.Publish(ctx, orphan, orphanKey); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile(ctx, ReconcileRequest{Before: old.Add(time.Hour), LiveTemporaryIDs: map[string]struct{}{liveID: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.RemovedTemporaryIDs, ",") != staleID || strings.Join(result.AbortedMultipartIDs, ",") != "multipart-1" {
		t.Fatalf("reconcile result = %+v", result)
	}
	if len(result.Orphans) != 1 || result.Orphans[0].Key != orphanKey {
		t.Fatalf("orphans = %+v", result.Orphans)
	}
	if _, err := store.Head(ctx, orphanKey); err != nil {
		t.Fatalf("orphan must only be reported: %v", err)
	}
	if _, ok := client.objects[store.temporaryKey(liveID)]; !ok {
		t.Fatal("live temporary was removed")
	}
}

func TestClassifyS3Errors(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{{"NoSuchKey", ErrNotFound}, {"PreconditionFailed", ErrConflict}, {"BadDigest", ErrChecksumMismatch}}
	for index, test := range cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			err := classifyS3Error(&smithy.GenericAPIError{Code: test.code, Message: "test"})
			if !errors.Is(err, test.want) {
				t.Fatalf("classify = %v, want %v", err, test.want)
			}
		})
	}
}
