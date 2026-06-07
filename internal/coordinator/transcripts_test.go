package coordinator

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptStoreRoundTrip(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())

	content := "line one\nline two\n"
	path, err := store.Save("sess-abc", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if path == "" {
		t.Fatalf("Save returned empty path")
	}

	reader, err := store.Open("sess-abc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if string(got) != content {
		t.Fatalf("transcript = %q, want %q", got, content)
	}
}

func TestTranscriptStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewTranscriptStore(filepath.Join(dir, "transcripts"))

	path, err := store.Save("sess-perm", strings.NewReader("data"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("transcript file mode = %o, want 600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat transcript dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("transcript dir mode = %o, want 700", perm)
	}
}

func TestTranscriptStoreRejectsPathTraversal(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())

	badIDs := []string{"../escape", "..", "a/b", "", "with space", "with\x00null", "with;semicolon"}
	for _, id := range badIDs {
		if _, err := store.Save(id, strings.NewReader("x")); err == nil {
			t.Fatalf("Save(%q) succeeded, want rejection", id)
		}
		if _, err := store.Open(id); err == nil {
			t.Fatalf("Open(%q) succeeded, want rejection", id)
		}
	}
}

func TestTranscriptStoreTruncatesToLast10MB(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())

	// Build content larger than the 10MB cap: a recognizable head that must be
	// dropped, then a tail of exactly the cap that must be retained.
	tail := bytes.Repeat([]byte("T"), transcriptMaxBytes)
	head := bytes.Repeat([]byte("H"), 1024)
	full := append(append([]byte{}, head...), tail...)

	path, err := store.Save("sess-big", bytes.NewReader(full))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stored transcript: %v", err)
	}
	if len(stored) != transcriptMaxBytes {
		t.Fatalf("stored size = %d, want %d", len(stored), transcriptMaxBytes)
	}
	if !bytes.Equal(stored, tail) {
		t.Fatalf("stored bytes are not the last %d bytes of the upload", transcriptMaxBytes)
	}
}

func TestTranscriptStoreOpenMissing(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())

	_, err := store.Open("sess-missing")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing = %v, want fs.ErrNotExist", err)
	}
}
