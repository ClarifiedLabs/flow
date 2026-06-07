package execution

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
)

// imageAttachmentsRelDir is the worktree-relative directory the worker
// materializes image attachments into. It lives under .flow so it is a Flow
// session artifact and is reusable by any agent harness, not just the harness
// CLI. The materialized binaries are untracked and never committed with the
// change: prepareWorktree writes a .flow/attachments/ entry to the worktree's
// .git/info/exclude so a blanket `git add -A` cannot stage them. The exclude is
// scoped to this directory only (not all of .flow/) so .flow/checks and
// .flow/session remain committable Flow artifacts.
const imageAttachmentsRelDir = ".flow/attachments"

// imageAttachmentDownloadTimeout bounds a single image download. It is a var so
// tests can shrink it.
var imageAttachmentDownloadTimeout = 60 * time.Second

// imageAttachmentDownloader is the subset of the flow client the worker uses to
// fetch image attachment bytes. *flowclient.Client satisfies it; tests inject a
// fake.
type imageAttachmentDownloader interface {
	DownloadIssueAttachment(ctx context.Context, issueID, attachmentID string, dst io.Writer) error
}

// materializeImageAttachments downloads the coordinator-stamped image
// attachments into the worktree for ANY author job (regardless of harness) and,
// only when the resolved harness is the harness CLI, re-stamps the entrypoint
// argv with --image <relpath> flags rendered before -p "$prompt". It is
// best-effort: a download failure is logged and skipped so a missing image
// never fails the job, and flag injection only references images that were
// actually written.
func materializeImageAttachments(ctx context.Context, input RunInput, payload JobPayload, worktree string) error {
	if len(payload.ImageAttachments) == 0 {
		return nil
	}
	issueID := ""
	if input.Job.IssueID != nil {
		issueID = strings.TrimSpace(*input.Job.IssueID)
	}
	token := strings.TrimSpace(input.Config.Token)
	if issueID == "" || token == "" || strings.TrimSpace(input.Config.CoordinatorURL) == "" {
		// Without an issue id, worker token, or coordinator URL there is no way
		// to fetch the bytes. Leave the entrypoint untouched.
		slog.Debug("worker image attachments skipped; missing issue id, worker token, or coordinator url", "job_id", input.Job.ID)
		return nil
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL:       input.Config.CoordinatorURL,
		Token:           token,
		ProtocolVersion: input.Config.ProtocolVersion,
	})
	if err != nil {
		return err
	}
	return materializeImages(ctx, client.WithProject(payload.ProjectID), issueID, payload, worktree)
}

// materializeImages is the testable core of materializeImageAttachments: it
// downloads each image via downloader and, for the harness CLI, injects the
// --image flags. Splitting the client construction out lets tests drive the
// download + injection logic with a fake downloader.
func materializeImages(ctx context.Context, downloader imageAttachmentDownloader, issueID string, payload JobPayload, worktree string) error {
	destDir := filepath.Join(worktree, imageAttachmentsRelDir)
	var materialized []materializedImage
	for _, descriptor := range payload.ImageAttachments {
		relPath, err := downloadImageAttachment(ctx, downloader, issueID, descriptor, destDir, worktree)
		if err != nil {
			// Skip-on-error: a single failed download must not poison the whole
			// job or block the remaining images. The entrypoint keeps its
			// original argv for any image that was not written.
			slog.Warn("worker image attachment download failed; skipping", "attachment_id", descriptor.ID, "error", err)
			continue
		}
		materialized = append(materialized, materializedImage{relPath: relPath})
	}
	if len(materialized) == 0 {
		return nil
	}
	if resolveHarness(tmuxInput{Payload: payload, Entrypoint: payloadEntrypoint(payload)}) == flowharness.Harness {
		injectImageFlags(payload, materialized)
	}
	return nil
}

// materializedImage pairs a downloaded image with its worktree-relative path.
type materializedImage struct {
	relPath string
}

func downloadImageAttachment(ctx context.Context, downloader imageAttachmentDownloader, issueID string, descriptor coordinator.IssueImageAttachment, destDir, worktree string) (string, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", fmt.Errorf("create image attachments directory: %w", err)
	}
	filename := sanitizeImageAttachmentFilename(descriptor.ID, descriptor.Filename)
	destPath := filepath.Join(destDir, filename)
	tmp, err := os.CreateTemp(destDir, ".image-*")
	if err != nil {
		return "", fmt.Errorf("create temp image file: %w", err)
	}
	tmpPath := tmp.Name()
	// Remove the temp file if any step below fails before the rename finalizes it.
	defer os.Remove(tmpPath)
	downloadCtx, cancel := context.WithTimeout(ctx, imageAttachmentDownloadTimeout)
	defer cancel()
	if err := downloader.DownloadIssueAttachment(downloadCtx, issueID, descriptor.ID, tmp); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download attachment %s: %w", descriptor.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close image file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", fmt.Errorf("finalize image file: %w", err)
	}
	relPath, err := filepath.Rel(worktree, destPath)
	if err != nil {
		return "", fmt.Errorf("resolve image relpath: %w", err)
	}
	return relPath, nil
}

// sanitizeImageAttachmentFilename builds a safe, collision-resistant filename
// for a materialized image by prefixing the attachment id (already restricted
// to [A-Za-z0-9._-]) to the base filename. The id prefix keeps attachments with
// the same filename distinct and makes the on-disk file traceable to its id.
func sanitizeImageAttachmentFilename(id, filename string) string {
	id = strings.TrimSpace(id)
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == ".." {
		filename = "image"
	}
	if id == "" {
		return filename
	}
	return id + "-" + filename
}

// injectImageFlags re-stamps the harness entrypoint argv with
// --image <relpath> flags, rendered before -p "$prompt". The harness author
// command is a shell script whose prompt-bearing invocations all end with
// ` -p "$prompt"` (the hooks and no-hooks branches), so a single ReplaceAll
// covers every invocation. Non-harness entrypoints are never passed in.
func injectImageFlags(payload JobPayload, materialized []materializedImage) {
	if payload.Entrypoint == nil || len(payload.Entrypoint.Argv) == 0 {
		return
	}
	flagString := renderImageFlags(materialized)
	if flagString == "" {
		return
	}
	const promptAnchor = ` -p "$prompt"`
	payload.Entrypoint.Argv[0] = strings.ReplaceAll(payload.Entrypoint.Argv[0], promptAnchor, " "+flagString+promptAnchor)
}

// renderImageFlags renders the --image <relpath> pairs as a shell-quoted,
// space-separated token string suitable for splicing into the harness shell
// command. Paths are quoted so a worktree path containing spaces or shell
// metacharacters cannot break the command.
func renderImageFlags(materialized []materializedImage) string {
	parts := make([]string, 0, len(materialized)*2)
	for _, image := range materialized {
		parts = append(parts, "--image", shellQuote(image.relPath))
	}
	return strings.Join(parts, " ")
}
