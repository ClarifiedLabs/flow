package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
)

// fakeImageDownloader records the bytes returned for each attachment id.
type fakeImageDownloader struct {
	bodies  map[string]string
	errs    map[string]error
	calls   []downloadCall
	written map[string]string
}

type downloadCall struct {
	IssueID      string
	AttachmentID string
}

func (f *fakeImageDownloader) DownloadIssueAttachment(ctx context.Context, issueID, attachmentID string, dst io.Writer) error {
	f.calls = append(f.calls, downloadCall{IssueID: issueID, AttachmentID: attachmentID})
	if err, ok := f.errs[attachmentID]; ok {
		return err
	}
	body := f.bodies[attachmentID]
	if _, err := io.WriteString(dst, body); err != nil {
		return err
	}
	f.written[attachmentID] = body
	return nil
}

func newFakeImageDownloader() *fakeImageDownloader {
	return &fakeImageDownloader{
		bodies:  map[string]string{},
		errs:    map[string]error{},
		written: map[string]string{},
	}
}

func harnessAuthorPayload(command string) JobPayload {
	return JobPayload{
		Entrypoint: &Entrypoint{
			Argv:    []string{command},
			CWD:     ".",
			Shell:   true,
			Harness: flowharness.Harness,
		},
		AgentHarness: flowharness.Harness,
	}
}

func TestMaterializeImagesDownloadsEveryImageForAnyHarness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, harness := range []string{flowharness.Harness, flowharness.Claude, flowharness.Codex} {
		downloader := newFakeImageDownloader()
		downloader.bodies["att-0001"] = "png-bytes"
		downloader.bodies["att-0002"] = "gif-bytes"
		payload := JobPayload{
			AgentHarness: harness,
			ImageAttachments: []coordinator.IssueImageAttachment{
				{ID: "att-0001", Filename: "shot.png"},
				{ID: "att-0002", Filename: "anim.gif"},
			},
		}
		worktree := t.TempDir()
		if err := materializeImages(ctx, downloader, "i-0001", payload, worktree); err != nil {
			t.Fatalf("harness %s: materialize: %v", harness, err)
		}
		// Every harness materializes the files.
		for id, want := range downloader.bodies {
			dir := filepath.Join(worktree, imageAttachmentsRelDir)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("harness %s: read attachments dir: %v", harness, err)
			}
			var found string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), id+"-") {
					found = filepath.Join(dir, entry.Name())
					break
				}
			}
			if found == "" {
				t.Fatalf("harness %s: missing materialized file for %s in %v", harness, id, entries)
			}
			got, err := os.ReadFile(found)
			if err != nil {
				t.Fatalf("harness %s: read materialized file: %v", harness, err)
			}
			if string(got) != want {
				t.Fatalf("harness %s: file %s = %q, want %q", harness, id, string(got), want)
			}
		}
	}
}

func TestMaterializeImagesInjectsFlagsOnlyForHarness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	command := `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  if [ -n "${FLOW_HARNESS_HOOKS:-}" ]; then
    harness --hooks "$FLOW_HARNESS_HOOKS" --provider openrouter -i "$prompt"
  else
    harness --provider openrouter -i "$prompt"
  fi
  code=$?
fi
exit "$code"`

	t.Run("harness gets --image flags before -i in both branches", func(t *testing.T) {
		t.Parallel()
		downloader := newFakeImageDownloader()
		downloader.bodies["att-0001"] = "png-bytes"
		downloader.bodies["att-0002"] = "gif-bytes"
		payload := harnessAuthorPayload(command)
		payload.ImageAttachments = []coordinator.IssueImageAttachment{
			{ID: "att-0001", Filename: "shot.png"},
			{ID: "att-0002", Filename: "anim.gif"},
		}
		worktree := t.TempDir()
		if err := materializeImages(ctx, downloader, "i-0001", payload, worktree); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		got := payload.Entrypoint.Argv[0]
		// Both prompt-bearing invocations get the flags before -i.
		wantFlags := "--image '.flow/attachments/att-0001-shot.png' --image '.flow/attachments/att-0002-anim.gif' -i \"$prompt\""
		if occurrences := strings.Count(got, wantFlags); occurrences != 2 {
			t.Fatalf("expected --image flags before -i in both branches; got %d occurrences in:\n%s", occurrences, got)
		}
	})

	t.Run("claude materializes files but keeps original argv", func(t *testing.T) {
		t.Parallel()
		downloader := newFakeImageDownloader()
		downloader.bodies["att-0001"] = "png-bytes"
		payload := harnessAuthorPayload(command)
		payload.AgentHarness = flowharness.Claude
		payload.Entrypoint.Harness = flowharness.Claude
		payload.Entrypoint.Argv = []string{`claude --dangerously-skip-permissions -p "$prompt"`}
		payload.ImageAttachments = []coordinator.IssueImageAttachment{
			{ID: "att-0001", Filename: "shot.png"},
		}
		worktree := t.TempDir()
		if err := materializeImages(ctx, downloader, "i-0001", payload, worktree); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		got := payload.Entrypoint.Argv[0]
		if strings.Contains(got, "--image") {
			t.Fatalf("claude argv should not contain --image: %s", got)
		}
		if got != `claude --dangerously-skip-permissions -p "$prompt"` {
			t.Fatalf("claude argv changed unexpectedly: %s", got)
		}
		// File still materialized.
		if _, err := os.Stat(filepath.Join(worktree, imageAttachmentsRelDir, "att-0001-shot.png")); err != nil {
			t.Fatalf("claude did not materialize image: %v", err)
		}
	})
}

func TestMaterializeImagesSkipOnErrorKeepsOtherImagesAndOriginalArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	downloader := newFakeImageDownloader()
	downloader.bodies["att-0001"] = "png-bytes"
	downloader.errs["att-0002"] = errors.New("network down")
	payload := harnessAuthorPayload(`harness -i "$prompt"`)
	payload.ImageAttachments = []coordinator.IssueImageAttachment{
		{ID: "att-0001", Filename: "shot.png"},
		{ID: "att-0002", Filename: "missing.jpg"},
	}
	worktree := t.TempDir()
	if err := materializeImages(ctx, downloader, "i-0001", payload, worktree); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Only the successful image is injected.
	got := payload.Entrypoint.Argv[0]
	want := "--image '.flow/attachments/att-0001-shot.png' -i \"$prompt\""
	if !strings.Contains(got, want) {
		t.Fatalf("expected only att-0001 injected; got:\n%s", got)
	}
	if strings.Contains(got, "att-0002") {
		t.Fatalf("failed download should not be referenced in argv:\n%s", got)
	}
	// The successful file exists; the failed one does not.
	if _, err := os.Stat(filepath.Join(worktree, imageAttachmentsRelDir, "att-0001-shot.png")); err != nil {
		t.Fatalf("successful image not materialized: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(worktree, imageAttachmentsRelDir)); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "att-0002") {
				t.Fatalf("failed download left a file: %s", entry.Name())
			}
		}
	}
}

func TestMaterializeImagesNoopWhenNoAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	downloader := newFakeImageDownloader()
	payload := harnessAuthorPayload(`harness -i "$prompt"`)
	worktree := t.TempDir()
	if err := materializeImages(ctx, downloader, "i-0001", payload, worktree); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(downloader.calls) != 0 {
		t.Fatalf("expected no downloads, got %+v", downloader.calls)
	}
	if got := payload.Entrypoint.Argv[0]; got != `harness -i "$prompt"` {
		t.Fatalf("argv changed with no attachments: %s", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, imageAttachmentsRelDir)); err == nil {
		t.Fatalf("attachments dir created with no attachments")
	}
}

func TestInjectImageFlagsHandlesMissingEntrypoint(t *testing.T) {
	t.Parallel()
	payload := JobPayload{}
	// Should not panic on a nil entrypoint.
	injectImageFlags(payload, []materializedImage{{relPath: ".flow/attachments/x.png"}})
}

func TestSanitizeImageAttachmentFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		id       string
		filename string
		want     string
	}{
		{"prefixed", "att-0001", "shot.png", "att-0001-shot.png"},
		{"empty filename defaults", "att-0001", "", "att-0001-image"},
		{"empty id", "", "shot.png", "shot.png"},
		{"strips path", "att-0002", "sub/dir/anim.gif", "att-0002-anim.gif"},
		{"dot filename", "att-0003", ".", "att-0003-image"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeImageAttachmentFilename(tc.id, tc.filename); got != tc.want {
				t.Fatalf("sanitizeImageAttachmentFilename(%q, %q) = %q, want %q", tc.id, tc.filename, got, tc.want)
			}
		})
	}
}

func TestDownloadImageAttachmentUsesIssueIDAndWritesBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	downloader := newFakeImageDownloader()
	downloader.bodies["att-0001"] = "png-bytes"
	worktree := t.TempDir()
	destDir := filepath.Join(worktree, imageAttachmentsRelDir)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	relPath, err := downloadImageAttachment(ctx, downloader, "i-0009", coordinator.IssueImageAttachment{ID: "att-0001", Filename: "shot.png"}, destDir, worktree)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if len(downloader.calls) != 1 || downloader.calls[0].IssueID != "i-0009" || downloader.calls[0].AttachmentID != "att-0001" {
		t.Fatalf("downloader calls = %+v, want one call for i-0009/att-0001", downloader.calls)
	}
	absPath := filepath.Join(worktree, relPath)
	got, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if !bytes.Equal(got, []byte("png-bytes")) {
		t.Fatalf("file = %q, want png-bytes", string(got))
	}
}
