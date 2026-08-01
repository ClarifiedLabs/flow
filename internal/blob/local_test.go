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
	for _, directory := range []string{store.root, store.temporaryRoot, store.objectRoot} {
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

func TestLocalPreservesCallerRootModeAndRejectsUnsafeLayout(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "caller-owned")
	if err := os.Mkdir(parent, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o751); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocal(parent, LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("caller-owned root mode = %o, want unchanged 751", info.Mode().Perm())
	}
	for _, path := range []string{store.root, store.temporaryRoot, store.objectRoot} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != privateDirectoryMode {
			t.Fatalf("store directory %s mode = %o, want 700", path, info.Mode().Perm())
		}
	}

	unsafeRoot := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeLayout := filepath.Join(unsafeRoot, localLayoutName)
	if err := os.Mkdir(unsafeLayout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeLayout, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(unsafeRoot, LocalOptions{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewLocal(unsafe layout) error = %v, want ErrInvalidConfig", err)
	}
	info, err = os.Stat(unsafeLayout)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("unsafe layout was chmodded to %o", info.Mode().Perm())
	}
}

func TestLocalRejectsSymlinkedRootLayoutAndChildren(t *testing.T) {
	outside := t.TempDir()
	t.Run("configured root", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "root")
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLocal(root, LocalOptions{}); err == nil {
			t.Fatal("NewLocal accepted a symlinked configured root")
		}
	})
	for _, node := range []string{localLayoutName, localTemporaryName, localObjectsName} {
		t.Run(node, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, localLayoutName)
			if node == localLayoutName {
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, privateDirectoryMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(path, node)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewLocal(root, LocalOptions{}); err == nil {
				t.Fatalf("NewLocal accepted symlinked %s node", node)
			}
		})
	}
}

func TestLocalNamespaceSwapCannotEscape(t *testing.T) {
	store, err := NewLocal(t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, _ := ParseKey("0123456789abcdef0123456789abcdef/0123456789abcdef0123456789abcdef")
	temporary := completeUpload(t, ctx, store, []byte("inside"))
	if _, err := store.Publish(ctx, temporary, key); err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}

	namespacePath := filepath.Join(store.objectRoot, "0123456789abcdef0123456789abcdef")
	movedPath := namespacePath + ".moved"
	outside := t.TempDir()
	if err := os.Rename(namespacePath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, namespacePath); err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil || string(got) != "inside" {
		t.Fatalf("already-open anchored object = %q, %v", got, readErr)
	}
	if _, err := store.Head(ctx, key); err == nil {
		t.Fatal("Head followed a swapped namespace symlink")
	}

	secondKey, _ := ParseKey("0123456789abcdef0123456789abcdef/fedcba9876543210fedcba9876543210")
	second := completeUpload(t, ctx, store, []byte("must-not-escape"))
	if _, err := store.Publish(ctx, second, secondKey); err == nil {
		t.Fatal("Publish followed a swapped namespace symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "fedcba9876543210fedcba9876543210")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Publish wrote outside store: %v", err)
	}
}

func TestLocalRejectsSymlinkedNamespace(t *testing.T) {
	store, err := NewLocal(t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ParseKey("abcdef0123456789abcdef0123456789/abcdef0123456789abcdef0123456789")
	if err := os.Symlink(t.TempDir(), filepath.Join(store.objectRoot, "abcdef0123456789abcdef0123456789")); err != nil {
		t.Fatal(err)
	}
	temporary := completeUpload(t, context.Background(), store, []byte("data"))
	if _, err := store.Publish(context.Background(), temporary, key); err == nil {
		t.Fatal("Publish accepted a symlinked namespace")
	}
	if _, err := store.Open(context.Background(), key); err == nil {
		t.Fatal("Open accepted a symlinked namespace")
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

func TestLocalReconcilePreservesActiveUploadOlderThanCutoff(t *testing.T) {
	store, err := NewLocal(t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	upload, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local := upload.(*localUpload)
	if _, err := upload.Write([]byte("still-open")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(store.temporaryRoot, local.id), old, old); err != nil {
		t.Fatal(err)
	}

	result, err := store.Reconcile(ctx, ReconcileRequest{Before: old.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedTemporaryIDs) != 0 {
		t.Fatalf("active temporary was removed: %v", result.RemovedTemporaryIDs)
	}
	if _, err := os.Stat(filepath.Join(store.temporaryRoot, local.id)); err != nil {
		t.Fatalf("active temporary no longer exists: %v", err)
	}
	if _, err := upload.Write([]byte("-more")); err != nil {
		t.Fatalf("active upload is no longer writable: %v", err)
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(filepath.Join(store.temporaryRoot, temporary.ID), old, old); err != nil {
		t.Fatal(err)
	}
	result, err = store.Reconcile(ctx, ReconcileRequest{Before: old.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedTemporaryIDs) != 1 || result.RemovedTemporaryIDs[0] != temporary.ID {
		t.Fatalf("completed temporary removal = %v, want %s", result.RemovedTemporaryIDs, temporary.ID)
	}
}

func TestLocalCloseIsIdempotentAndRejectsOperations(t *testing.T) {
	store, err := NewLocal(t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var _ Closer = store

	ctx := context.Background()
	objectKey, _ := NewKey("close-object")
	objectTemporary := completeUpload(t, ctx, store, []byte("published"))
	if _, err := store.Publish(ctx, objectTemporary, objectKey); err != nil {
		t.Fatal(err)
	}
	unpublished := completeUpload(t, ctx, store, []byte("unpublished"))
	active, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(ctx, objectKey)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if _, err := store.rootDir.Stat(); err == nil {
		t.Fatal("root directory descriptor remains open after Close")
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "published" {
		t.Fatalf("reader opened before Close = %q, %v", got, err)
	}

	checks := []struct {
		name string
		call func() error
	}{
		{name: "Begin", call: func() error { _, err := store.Begin(ctx); return err }},
		{name: "Resume", call: func() error { _, err := store.Resume(ctx, unpublished.ID); return err }},
		{name: "Publish", call: func() error { _, err := store.Publish(ctx, unpublished, objectKey); return err }},
		{name: "Head", call: func() error { _, err := store.Head(ctx, objectKey); return err }},
		{name: "Open", call: func() error { _, err := store.Open(ctx, objectKey); return err }},
		{name: "OpenRange", call: func() error {
			_, err := store.OpenRange(ctx, objectKey, ByteRange{Length: 1})
			return err
		}},
		{name: "Abort", call: func() error { return store.Abort(ctx, unpublished.ID) }},
		{name: "Reconcile", call: func() error {
			_, err := store.Reconcile(ctx, ReconcileRequest{Before: time.Now(), Limit: 1})
			return err
		}},
		{name: "Upload.Write", call: func() error { _, err := active.Write([]byte("x")); return err }},
		{name: "Upload.Complete", call: func() error { _, err := active.Complete(ctx); return err }},
		{name: "Upload.Abort", call: func() error { return active.Abort(ctx) }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); !errors.Is(err, ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
		})
	}
}

func TestLocalReconcileBoundsExaminedEntries(t *testing.T) {
	t.Run("temporaries", func(t *testing.T) {
		store, err := NewLocal(t.TempDir(), LocalOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()
		live := make(map[string]struct{})
		for i := 0; i < 4; i++ {
			temporary := completeUpload(t, ctx, store, []byte{byte(i)})
			live[temporary.ID] = struct{}{}
		}

		result, err := store.Reconcile(ctx, ReconcileRequest{
			Before: time.Now().Add(time.Hour), LiveTemporaryIDs: live, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated {
			t.Fatal("Reconcile did not report a scan bounded before all temporary entries")
		}
		if len(result.RemovedTemporaryIDs) != 0 {
			t.Fatalf("live temporaries removed: %v", result.RemovedTemporaryIDs)
		}
	})

	t.Run("published objects", func(t *testing.T) {
		store, err := NewLocal(t.TempDir(), LocalOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()
		keys := make([]Key, 0, 4)
		for i := 0; i < 4; i++ {
			key, err := NewKey("bounded-object-namespace")
			if err != nil {
				t.Fatal(err)
			}
			temporary := completeUpload(t, ctx, store, []byte{byte(i)})
			if _, err := store.Publish(ctx, temporary, key); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, key)
		}

		result, err := store.Reconcile(ctx, ReconcileRequest{
			Before: time.Now().Add(time.Hour), Limit: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated {
			t.Fatal("Reconcile did not report a scan bounded before all object entries")
		}
		if len(result.Orphans) != 2 {
			t.Fatalf("reported orphans = %d, want 2 object entries after examining one namespace", len(result.Orphans))
		}
		for _, key := range keys {
			if _, err := store.Head(ctx, key); err != nil {
				t.Fatalf("reconciliation deleted published object %s: %v", key, err)
			}
		}
	})
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
