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

type fakeS3Version struct {
	key          string
	version      string
	modified     time.Time
	deleteMarker bool
}

type fakeS3 struct {
	mu              sync.Mutex
	objects         map[string]fakeS3Object
	versions        []fakeS3Version
	deletedVersions []string
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
	key, versionID := aws.ToString(input.Key), aws.ToString(input.VersionId)
	if input.VersionId != nil {
		f.deletedVersions = append(f.deletedVersions, key+"@"+versionID)
		for index := len(f.versions) - 1; index >= 0; index-- {
			if f.versions[index].key == key && f.versions[index].version == versionID {
				f.versions = append(f.versions[:index], f.versions[index+1:]...)
			}
		}
		if object, ok := f.objects[key]; ok && (object.version == versionID || object.version == "" && versionID == "null") {
			delete(f.objects, key)
		}
		return &s3.DeleteObjectOutput{}, nil
	}
	delete(f.objects, key)
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
	start := sort.SearchStrings(keys, aws.ToString(input.ContinuationToken))
	if input.ContinuationToken != nil && start < len(keys) && keys[start] == *input.ContinuationToken {
		start++
	}
	limit := len(keys) - start
	if input.MaxKeys != nil && limit > int(*input.MaxKeys) {
		limit = int(*input.MaxKeys)
	}
	end := start + limit
	output := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	for _, key := range keys[start:end] {
		object := f.objects[key]
		output.Contents = append(output.Contents, types.Object{Key: aws.String(key), LastModified: aws.Time(object.modified), Size: aws.Int64(int64(len(object.data)))})
	}
	if end < len(keys) && end > start {
		output.NextContinuationToken = aws.String(keys[end-1])
	}
	return output, nil
}

func (f *fakeS3) ListObjectVersions(_ context.Context, input *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type listedVersion struct {
		key, version string
		modified     time.Time
		deleteMarker bool
	}
	prefix := aws.ToString(input.Prefix)
	listed := make([]listedVersion, 0, len(f.objects)+len(f.versions))
	for key, object := range f.objects {
		if strings.HasPrefix(key, prefix) {
			version := object.version
			if version == "" {
				version = "null"
			}
			listed = append(listed, listedVersion{key: key, version: version, modified: object.modified})
		}
	}
	for _, version := range f.versions {
		if strings.HasPrefix(version.key, prefix) {
			listed = append(listed, listedVersion{
				key: version.key, version: version.version, modified: version.modified, deleteMarker: version.deleteMarker,
			})
		}
	}
	sort.Slice(listed, func(i, j int) bool {
		return listed[i].key < listed[j].key || listed[i].key == listed[j].key && listed[i].version < listed[j].version
	})
	markerKey, markerVersion := aws.ToString(input.KeyMarker), aws.ToString(input.VersionIdMarker)
	start := sort.Search(len(listed), func(i int) bool {
		return listed[i].key > markerKey || listed[i].key == markerKey && listed[i].version > markerVersion
	})
	limit := len(listed) - start
	if input.MaxKeys != nil && limit > int(*input.MaxKeys) {
		limit = int(*input.MaxKeys)
	}
	end := start + limit
	output := &s3.ListObjectVersionsOutput{IsTruncated: aws.Bool(end < len(listed))}
	for _, version := range listed[start:end] {
		if version.deleteMarker {
			output.DeleteMarkers = append(output.DeleteMarkers, types.DeleteMarkerEntry{
				Key: aws.String(version.key), VersionId: aws.String(version.version), LastModified: aws.Time(version.modified),
			})
		} else {
			output.Versions = append(output.Versions, types.ObjectVersion{
				Key: aws.String(version.key), VersionId: aws.String(version.version), LastModified: aws.Time(version.modified),
			})
		}
	}
	if end < len(listed) && end > start {
		last := listed[end-1]
		output.NextKeyMarker = aws.String(last.key)
		output.NextVersionIdMarker = aws.String(last.version)
	}
	return output, nil
}

func (f *fakeS3) ListMultipartUploads(_ context.Context, input *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uploads := make([]types.MultipartUpload, 0, len(f.multiparts))
	for _, upload := range f.multiparts {
		if strings.HasPrefix(aws.ToString(upload.Key), aws.ToString(input.Prefix)) {
			uploads = append(uploads, upload)
		}
	}
	sort.Slice(uploads, func(i, j int) bool {
		leftKey, rightKey := aws.ToString(uploads[i].Key), aws.ToString(uploads[j].Key)
		return leftKey < rightKey || leftKey == rightKey && aws.ToString(uploads[i].UploadId) < aws.ToString(uploads[j].UploadId)
	})
	markerKey, markerUpload := aws.ToString(input.KeyMarker), aws.ToString(input.UploadIdMarker)
	start := sort.Search(len(uploads), func(i int) bool {
		key, upload := aws.ToString(uploads[i].Key), aws.ToString(uploads[i].UploadId)
		return key > markerKey || key == markerKey && upload > markerUpload
	})
	limit := len(uploads) - start
	if input.MaxUploads != nil && limit > int(*input.MaxUploads) {
		limit = int(*input.MaxUploads)
	}
	end := start + limit
	output := &s3.ListMultipartUploadsOutput{IsTruncated: aws.Bool(end < len(uploads)), Uploads: append([]types.MultipartUpload(nil), uploads[start:end]...)}
	if end < len(uploads) && end > start {
		last := uploads[end-1]
		output.NextKeyMarker = aws.String(aws.ToString(last.Key))
		output.NextUploadIdMarker = aws.String(aws.ToString(last.UploadId))
	}
	return output, nil
}

func (f *fakeS3) AbortMultipartUpload(_ context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(input.UploadId)
	f.aborted = append(f.aborted, id)
	for index, upload := range f.multiparts {
		if aws.ToString(upload.UploadId) == id && aws.ToString(upload.Key) == aws.ToString(input.Key) {
			f.multiparts = append(f.multiparts[:index], f.multiparts[index+1:]...)
			break
		}
	}
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

type stalledPutS3 struct {
	*fakeS3
	started chan struct{}
	exited  chan struct{}
}

func (f *stalledPutS3) PutObject(ctx context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	close(f.started)
	<-ctx.Done()
	close(f.exited)
	return nil, ctx.Err()
}

type lateCommitS3 struct {
	*fakeS3
	started chan struct{}
	events  []string
}

func (f *lateCommitS3) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	close(f.started)
	<-ctx.Done()
	f.mu.Lock()
	f.objects[aws.ToString(input.Key)] = fakeS3Object{modified: time.Now().UTC()}
	f.events = append(f.events, "put-exit")
	f.mu.Unlock()
	return &s3.PutObjectOutput{}, nil
}

func (f *lateCommitS3) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "delete")
	delete(f.objects, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

type emptyPageS3 struct {
	*fakeS3
	mu    sync.Mutex
	calls int
}

func (f *emptyPageS3) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &s3.ListObjectsV2Output{
		IsTruncated: aws.Bool(true), NextContinuationToken: aws.String(fmt.Sprintf("empty-%d", f.calls)),
	}, nil
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

func TestS3AbortReleasesBlockedWrite(t *testing.T) {
	client := &stalledPutS3{fakeS3: newFakeS3(), started: make(chan struct{}), exited: make(chan struct{})}
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-client.started
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := upload.Write([]byte("blocked"))
		writeDone <- writeErr
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("Write returned before Abort: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := upload.Abort(ctx); err != nil {
		t.Fatalf("Abort() = %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked Write unexpectedly succeeded")
		}
	case <-ctx.Done():
		t.Fatal("blocked Write was not released")
	}
	select {
	case <-client.exited:
	default:
		t.Fatal("Abort returned before PutObject exited")
	}
}

func TestS3AbortDeletesOnlyAfterLatePutExit(t *testing.T) {
	client := &lateCommitS3{fakeS3: newFakeS3(), started: make(chan struct{})}
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-client.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := upload.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if strings.Join(client.events, ",") != "put-exit,delete" {
		t.Fatalf("operation order = %v, want PUT exit before delete", client.events)
	}
	if len(client.objects) != 0 {
		t.Fatalf("late PUT recreated temporary object: %v", client.objects)
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

func TestS3ReconcileAdvancesBoundedPagination(t *testing.T) {
	client := newFakeS3()
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket", Prefix: "flow"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().UTC()
	before := old.Add(time.Hour)
	liveID := strings.Repeat("a", 32)
	recentID := strings.Repeat("b", 32)
	staleID := strings.Repeat("c", 32)
	for id, modified := range map[string]time.Time{liveID: old, recentID: recent, staleID: old} {
		client.objects[store.temporaryKey(id)] = fakeS3Object{data: []byte(id), modified: modified}
	}
	client.multiparts = []types.MultipartUpload{
		{Key: aws.String(store.temporaryKey(liveID)), UploadId: aws.String("multipart-live"), Initiated: aws.Time(old)},
		{Key: aws.String(store.temporaryKey(recentID)), UploadId: aws.String("multipart-recent"), Initiated: aws.Time(recent)},
		{Key: aws.String(store.temporaryKey(staleID)), UploadId: aws.String("multipart-stale"), Initiated: aws.Time(old)},
	}
	referenced, _ := ParseKey(strings.Repeat("0", 32) + "/" + strings.Repeat("0", 32))
	pending, _ := ParseKey(strings.Repeat("1", 32) + "/" + strings.Repeat("1", 32))
	orphan, _ := ParseKey(strings.Repeat("2", 32) + "/" + strings.Repeat("2", 32))
	for _, key := range []Key{referenced, pending, orphan} {
		data := []byte(key.String())
		digest := Digest(sha256.Sum256(data))
		client.objects[store.objectKey(key)] = fakeS3Object{
			data: data, modified: old, checksum: base64.StdEncoding.EncodeToString(digest[:]),
			metadata: map[string]string{digestMetadataKey: digest.String()},
		}
	}

	request := ReconcileRequest{
		Before: before, Limit: 2, LiveTemporaryIDs: map[string]struct{}{liveID: {}},
		ReferencedKeys: map[Key]struct{}{referenced: {}}, PendingKeys: map[Key]struct{}{pending: {}},
	}
	var removed, aborted []string
	var orphans []Object
	calls := 0
	for {
		calls++
		result, reconcileErr := store.Reconcile(context.Background(), request)
		if reconcileErr != nil {
			t.Fatal(reconcileErr)
		}
		removed = append(removed, result.RemovedTemporaryIDs...)
		aborted = append(aborted, result.AbortedMultipartIDs...)
		orphans = append(orphans, result.Orphans...)
		if !result.Truncated {
			break
		}
		if calls > 8 {
			t.Fatal("bounded reconciliation did not finish")
		}
	}
	if strings.Join(removed, ",") != staleID {
		t.Fatalf("removed temporaries = %v", removed)
	}
	if strings.Join(aborted, ",") != "multipart-stale" {
		t.Fatalf("aborted multiparts = %v", aborted)
	}
	if len(orphans) != 1 || orphans[0].Key != orphan {
		t.Fatalf("orphans = %+v", orphans)
	}
	if calls < 5 {
		t.Fatalf("reconciliation used %d calls, want multiple bounded pages", calls)
	}
	if _, err := store.Head(context.Background(), orphan); err != nil {
		t.Fatalf("published orphan was deleted: %v", err)
	}
}

func TestS3ReconcileDeletesStaleTemporaryVersionsAndDeleteMarkers(t *testing.T) {
	client := newFakeS3()
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket", Prefix: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	staleID := strings.Repeat("a", 32)
	liveID := strings.Repeat("b", 32)
	old := time.Now().Add(-2 * time.Hour)
	client.versions = []fakeS3Version{
		{key: "tenant/tmp/" + staleID, version: "stale-object", modified: old},
		{key: "tenant/tmp/" + staleID, version: "stale-marker", modified: old, deleteMarker: true},
		{key: "tenant/tmp/" + liveID, version: "live-object", modified: old},
		{key: "tenant/tmp/" + liveID, version: "live-marker", modified: old, deleteMarker: true},
	}

	result, err := store.Reconcile(context.Background(), ReconcileRequest{
		Before: time.Now().Add(-time.Hour), Limit: 16,
		LiveTemporaryIDs: map[string]struct{}{liveID: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatal("version cleanup unexpectedly truncated")
	}
	sort.Strings(client.deletedVersions)
	wantDeleted := []string{
		"tenant/tmp/" + staleID + "@stale-marker",
		"tenant/tmp/" + staleID + "@stale-object",
	}
	if fmt.Sprint(client.deletedVersions) != fmt.Sprint(wantDeleted) {
		t.Fatalf("deleted versions = %v, want %v", client.deletedVersions, wantDeleted)
	}
	if len(client.versions) != 2 || client.versions[0].key != "tenant/tmp/"+liveID || client.versions[1].key != "tenant/tmp/"+liveID {
		t.Fatalf("remaining versions = %+v, want only protected live history", client.versions)
	}
}

func TestS3ReconcileBoundsAdvancingEmptyPages(t *testing.T) {
	client := &emptyPageS3{fakeS3: newFakeS3()}
	store, err := NewS3(S3Options{Client: client, Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile(context.Background(), ReconcileRequest{Before: time.Now(), Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("empty truncated pages should leave reconciliation truncated")
	}
	client.mu.Lock()
	calls := client.calls
	client.mu.Unlock()
	if calls != 3 {
		t.Fatalf("ListObjectsV2 calls = %d, want bounded 3", calls)
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
