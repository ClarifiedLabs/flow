package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeyIsOpaqueScopedAndPathSafe(t *testing.T) {
	first, err := NewKey("project:p/capture:c")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewKey("project:p/capture:c")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("fresh keys should differ")
	}
	if first.String()[:32] != second.String()[:32] {
		t.Fatal("same scope should share only an opaque namespace")
	}
	for _, value := range []string{"../secret", "aa/bb", first.String() + "/extra", "ABCDEF0123456789ABCDEF0123456789/0123456789abcdef0123456789abcdef"} {
		if _, err := ParseKey(value); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("ParseKey(%q) error = %v, want ErrInvalidKey", value, err)
		}
	}
}

func TestLocalLifecycleRangesAndImmutability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store, err := NewLocal(root, LocalOptions{MaxRangeBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{root, filepath.Join(root, "tmp"), filepath.Join(root, "objects")} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode = %o, want 700", info.Mode().Perm())
		}
	}
	ctx := context.Background()
	key, _ := NewKey("capture:one")
	content := []byte("abcdefgh")
	temporary := completeUpload(t, ctx, store, content)
	wantDigest := Digest(sha256.Sum256(content))
	if temporary.Digest != wantDigest || temporary.Size != int64(len(content)) {
		t.Fatalf("temporary = %+v, want digest %s size %d", temporary, wantDigest, len(content))
	}
	resumed, err := store.Resume(ctx, temporary.ID)
	if err != nil || resumed.Digest != temporary.Digest {
		t.Fatalf("Resume() = %+v, %v", resumed, err)
	}
	object, err := store.Publish(ctx, temporary, key)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != wantDigest || object.Size != int64(len(content)) {
		t.Fatalf("published object = %+v", object)
	}
	objectPath, _ := store.objectPath(key, false)
	info, err := os.Stat(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("object mode = %o, want 600", info.Mode().Perm())
	}
	reader, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Open() = %q, %v", got, err)
	}
	ranged, err := store.OpenRange(ctx, key, ByteRange{Offset: 2, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if err != nil || string(got) != "cdef" || ranged.Start != 2 || ranged.End != 6 || ranged.Size != 8 {
		t.Fatalf("OpenRange() = %q %+v, %v", got, ranged, err)
	}
	for _, invalid := range []ByteRange{{Offset: -1, Length: 1}, {Offset: 0, Length: 0}, {Offset: 8, Length: 1}, {Offset: 0, Length: 5}} {
		if _, err := store.OpenRange(ctx, key, invalid); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("OpenRange(%+v) error = %v", invalid, err)
		}
	}

	same := completeUpload(t, ctx, store, content)
	if _, err := store.Publish(ctx, same, key); err != nil {
		t.Fatalf("same-content Publish() = %v", err)
	}
	different := completeUpload(t, ctx, store, []byte("different"))
	if _, err := store.Publish(ctx, different, key); !errors.Is(err, ErrConflict) {
		t.Fatalf("different-content Publish() error = %v, want ErrConflict", err)
	}
	if _, err := store.Resume(ctx, different.ID); err != nil {
		t.Fatalf("conflicting temporary should remain abortable: %v", err)
	}
	if err := store.Abort(ctx, different.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Abort(ctx, different.ID); err != nil {
		t.Fatalf("Abort should be idempotent: %v", err)
	}
}

func TestLocalUploadStateCancellationAndReconciliation(t *testing.T) {
	store, err := NewLocal(t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	upload, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(store.temporaryRoot, temporary.ID), old, old); err != nil {
		t.Fatal(err)
	}
	live := completeUpload(t, ctx, store, []byte("live"))
	if err := os.Chtimes(filepath.Join(store.temporaryRoot, live.ID), old, old); err != nil {
		t.Fatal(err)
	}
	orphanKey, _ := NewKey("orphan")
	orphan := completeUpload(t, ctx, store, []byte("orphan"))
	if _, err := store.Publish(ctx, orphan, orphanKey); err != nil {
		t.Fatal(err)
	}
	pendingKey, _ := NewKey("pending")
	pending := completeUpload(t, ctx, store, []byte("pending"))
	if _, err := store.Publish(ctx, pending, pendingKey); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile(ctx, ReconcileRequest{
		Before: old.Add(time.Hour), LiveTemporaryIDs: map[string]struct{}{live.ID: {}},
		PendingKeys: map[Key]struct{}{pendingKey: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedTemporaryIDs) != 1 || result.RemovedTemporaryIDs[0] != temporary.ID {
		t.Fatalf("removed temporaries = %v", result.RemovedTemporaryIDs)
	}
	if len(result.Orphans) != 1 || result.Orphans[0].Key != orphanKey {
		t.Fatalf("orphans = %+v", result.Orphans)
	}
	if _, err := store.Head(ctx, orphanKey); err != nil {
		t.Fatalf("reconciliation must not delete published orphan: %v", err)
	}
	if _, err := store.Resume(ctx, live.ID); err != nil {
		t.Fatalf("live temporary was removed: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Open(cancelled, orphanKey); err != nil {
		t.Fatal(err)
	}
	reader, _ := store.Open(cancelled, orphanKey)
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Read() error = %v", err)
	}
	reader.Close()
}

func completeUpload(t *testing.T, ctx context.Context, store Store, content []byte) Temporary {
	t.Helper()
	upload, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	midpoint := len(content) / 2
	for _, part := range [][]byte{content[:midpoint], content[midpoint:]} {
		if _, err := upload.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return temporary
}
