package coordinator

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// transcriptMaxBytes caps the size of a stored transcript. Uploads larger than
// this keep only the trailing bytes (the most recent tmux output), mirroring
// the worker's tail upload.
const transcriptMaxBytes = 10 << 20 // 10 MiB

// transcriptIDPattern restricts transcript ids to characters that cannot escape
// the store directory or be interpreted by a shell; session and job ids are
// already drawn from this alphabet.
var transcriptIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// TranscriptStore owns a single directory of transcript files (one per session
// or job id). It enforces the 10MB cap and rejects ids that could traverse out
// of its directory.
type TranscriptStore struct {
	dir string
}

// NewTranscriptStore returns a store rooted at dir. The directory is created
// lazily on the first Save with 0700 permissions.
func NewTranscriptStore(dir string) *TranscriptStore {
	return &TranscriptStore{dir: dir}
}

func (s *TranscriptStore) pathFor(id string) (string, error) {
	// "." and ".." pass the character allowlist but are filesystem-relative
	// references; reject them explicitly so an id never resolves to the
	// directory itself or its parent.
	if id == "." || id == ".." || !transcriptIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid transcript id %q", id)
	}
	return filepath.Join(s.dir, id+".log"), nil
}

// Save writes the transcript for id, keeping at most the last transcriptMaxBytes
// bytes of r. It returns the path the transcript was written to. The file is
// created 0600 and the store directory 0700.
func (s *TranscriptStore) Save(id string, r io.Reader) (string, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return "", err
	}
	data, err := readTail(r, transcriptMaxBytes)
	if err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}
	// Write atomically via a temp file so a partial upload never replaces a
	// previously complete transcript.
	if err := writeFileAtomic(s.dir, path, data, 0o700, 0o600, ".transcript-*"); err != nil {
		return "", err
	}

	return path, nil
}

// Open returns a reader over the stored transcript for id. It returns an error
// wrapping fs.ErrNotExist when no transcript exists for the id.
func (s *TranscriptStore) Open(id string) (io.ReadCloser, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	return file, nil
}

// readTail reads all of r but retains only the trailing max bytes, so a stream
// far larger than the cap is bounded by the cap in memory.
func readTail(r io.Reader, max int) ([]byte, error) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > max {
				// Drop everything but the trailing max bytes to keep memory
				// bounded while still consuming the whole reader.
				buf = append(buf[:0], buf[len(buf)-max:]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	return bytes.Clone(buf), nil
}
