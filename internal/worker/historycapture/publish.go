package historycapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
)

func abandonHistoryUpload(ctx context.Context, client Client, entry *Entry, temporaryUploadID string) error {
	if temporaryUploadID == "" {
		return nil
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return client.AbandonHistoryArtifactUpload(cleanupContext, entry.Capture.ID, entry.UploadGrant, temporaryUploadID)
}

// RecordVerdict publishes the already-checkpointed execution result before
// archive construction. It is idempotent across response loss and restarts.
func (o *Outbox) RecordVerdict(ctx context.Context, client Client, captureID string) error {
	if client == nil {
		return errors.New("history capture client is required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return err
	}
	if entry.Status != statusFinalized && entry.Status != statusStaged {
		if entry.Status == statusComplete {
			return nil
		}
		return fmt.Errorf("%w: execution result is not finalized", ErrInvalidOutbox)
	}
	return o.recordVerdictLocked(ctx, client, &entry)
}

func (o *Outbox) recordVerdictLocked(ctx context.Context, client Client, entry *Entry) error {
	if entry.Progress.VerdictRecorded {
		return nil
	}
	if err := o.transitionToState(ctx, client, entry, "running"); err != nil {
		return err
	}
	request := contract.RecordHistoryExecutionVerdictRequest{Verdict: entry.Final.Verdict, ExitCode: cloneInt(entry.Final.ExitCode), ErrorCode: entry.Final.ErrorCode, ExpectedVersion: entry.RemoteVersion}
	updated, err := client.RecordHistoryExecutionVerdict(ctx, entry.Capture.ID, entry.UploadGrant, request)
	if err != nil {
		return err
	}
	if err := applyRemote(entry, updated); err != nil {
		return err
	}
	entry.Progress.VerdictRecorded = true
	return o.saveLocked(ctx, entry)
}

// Publish replays one staged capture through protocol 7. Every acknowledged
// remote operation is durably checkpointed before the next operation begins.
func (o *Outbox) Publish(ctx context.Context, client Client, captureID string) error {
	if client == nil {
		return errors.New("history capture client is required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return err
	}
	if entry.Status == statusComplete || entry.Progress.Completed {
		return nil
	}
	if entry.Status != statusStaged {
		return ErrNotStaged
	}
	if err := o.recordVerdictLocked(ctx, client, &entry); err != nil {
		return err
	}
	if err := o.transitionToState(ctx, client, &entry, "uploading"); err != nil {
		return err
	}
	entryDir, _ := o.entryDir(captureID)
	for index := range entry.Artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		artifact := &entry.Artifacts[index]
		if artifact.TemporaryUploadID == "" {
			root, err := os.OpenRoot(entryDir)
			if err != nil {
				return err
			}
			file, err := root.Open(artifact.Path)
			if err != nil {
				root.Close()
				return err
			}
			upload, uploadErr := client.UploadHistoryArtifactBytes(ctx, entry.Capture.ID, entry.UploadGrant, file)
			closeErr := file.Close()
			rootErr := root.Close()
			if uploadErr != nil {
				return uploadErr
			}
			// net/http closes an os.File request body after sending it. Fake clients
			// may leave it open, so tolerate either ownership convention.
			if closeErr != nil && errors.Is(closeErr, os.ErrClosed) {
				closeErr = nil
			}
			if closeErr != nil || rootErr != nil {
				return errors.Join(closeErr, rootErr, abandonHistoryUpload(ctx, client, &entry, upload.TemporaryUploadID))
			}
			if upload.TemporaryUploadID == "" || upload.SHA256 != artifact.SHA256 || upload.StoredSize != artifact.StoredSize {
				return errors.Join(
					fmt.Errorf("%w: upload acknowledgement differs from staged payload", ErrInvalidOutbox),
					abandonHistoryUpload(ctx, client, &entry, upload.TemporaryUploadID),
				)
			}
			artifact.TemporaryUploadID = upload.TemporaryUploadID
			artifact.Publish.TemporaryUploadID = upload.TemporaryUploadID
			if err := o.saveLocked(ctx, &entry); err != nil {
				// A completed server upload consumes quota even when the local
				// acknowledgement cannot be checkpointed. Compensate before
				// returning so replay starts with a fresh bounded upload.
				artifact.TemporaryUploadID = ""
				artifact.Publish.TemporaryUploadID = ""
				cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				rollbackErr := o.saveLocked(cleanupContext, &entry)
				abandonErr := client.AbandonHistoryArtifactUpload(cleanupContext, entry.Capture.ID, entry.UploadGrant, upload.TemporaryUploadID)
				return errors.Join(err, rollbackErr, abandonErr)
			}
		}
		if artifact.PublishedArtifactID == "" {
			published, err := client.PublishHistoryArtifact(ctx, entry.Capture.ID, entry.UploadGrant, artifact.Publish)
			if err != nil {
				return err
			}
			if published.ID == "" || published.CaptureID != entry.Capture.ID || published.LogicalKey != artifact.Publish.LogicalKey ||
				published.Kind != artifact.Publish.Kind || published.SHA256 != artifact.SHA256 || published.StoredSize != artifact.StoredSize {
				return fmt.Errorf("%w: artifact acknowledgement differs from stage", ErrInvalidOutbox)
			}
			artifact.PublishedArtifactID = published.ID
			if err := o.saveLocked(ctx, &entry); err != nil {
				return err
			}
		}
		if !artifact.Registered {
			switch artifact.Publish.Kind {
			case "transcript_segment":
				if artifact.Segment == nil {
					return fmt.Errorf("%w: transcript segment index is absent", ErrInvalidOutbox)
				}
				if err := client.RegisterHistoryTranscriptSegment(ctx, entry.Capture.ID, entry.UploadGrant, *artifact.Segment); err != nil {
					return err
				}
			case "workspace_snapshot":
				if entry.WorkspaceSummary == nil {
					return fmt.Errorf("%w: workspace summary is absent", ErrInvalidOutbox)
				}
				if _, err := client.RegisterHistoryWorkspaceSummary(ctx, entry.Capture.ID, entry.UploadGrant, *entry.WorkspaceSummary); err != nil {
					return err
				}
			case "harness_root":
				if entry.HarnessMembers == nil {
					return fmt.Errorf("%w: Harness member index is absent", ErrInvalidOutbox)
				}
				if err := client.RegisterHistoryHarnessMembers(ctx, entry.Capture.ID, entry.UploadGrant, *entry.HarnessMembers); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: unsupported staged artifact kind", ErrInvalidOutbox)
			}
			artifact.Registered = true
			if err := o.saveLocked(ctx, &entry); err != nil {
				return err
			}
		}
	}
	if !entry.Progress.TranscriptSealed {
		if err := client.SealHistoryTranscript(ctx, entry.Capture.ID, entry.UploadGrant, *entry.TranscriptSeal); err != nil {
			return err
		}
		entry.Progress.TranscriptSealed = true
		if err := o.saveLocked(ctx, &entry); err != nil {
			return err
		}
	}
	if !entry.Progress.ExpectedDeclared {
		request := *entry.ExpectedSet
		request.ExpectedVersion = entry.RemoteVersion
		updated, err := client.DeclareHistoryExpectedSet(ctx, entry.Capture.ID, entry.UploadGrant, request)
		if err != nil {
			return err
		}
		if err := applyRemote(&entry, updated); err != nil {
			return err
		}
		entry.Progress.ExpectedDeclared = true
		entry.ExpectedSet.ExpectedVersion = request.ExpectedVersion
		if err := o.saveLocked(ctx, &entry); err != nil {
			return err
		}
	}
	if !entry.Progress.ManifestGenerated {
		manifest, err := client.GenerateHistoryManifest(ctx, entry.Capture.ID, entry.UploadGrant)
		if err != nil {
			return err
		}
		if manifest.ID == "" || manifest.CaptureID != entry.Capture.ID || manifest.LogicalKey != "manifest/final" || manifest.Kind != "manifest" {
			return fmt.Errorf("%w: coordinator manifest acknowledgement differs", ErrInvalidOutbox)
		}
		entry.Progress.ManifestGenerated = true
		if err := o.saveLocked(ctx, &entry); err != nil {
			return err
		}
	}
	completed, err := client.CompleteHistoryCapture(ctx, entry.Capture.ID, entry.UploadGrant, entry.RemoteVersion)
	if err != nil {
		return err
	}
	if err := applyRemote(&entry, completed); err != nil {
		return err
	}
	if entry.RemoteState != "complete" {
		return fmt.Errorf("%w: completion did not return complete state", ErrInvalidOutbox)
	}
	entry.Progress.Completed, entry.Status = true, statusComplete
	return o.completeLocked(ctx, &entry)
}

func (o *Outbox) transitionToState(ctx context.Context, client Client, entry *Entry, target string) error {
	for entry.RemoteState != target {
		var next string
		switch entry.RemoteState {
		case "reserved":
			next = "running"
		case "running":
			next = "quiescing"
		case "quiescing":
			next = "sealed"
		case "sealed", "blocked", "lost":
			next = "uploading"
		case "complete":
			entry.Progress.Completed, entry.Status = true, statusComplete
			return o.completeLocked(ctx, entry)
		default:
			return fmt.Errorf("%w: cannot publish from remote state %q", ErrInvalidOutbox, entry.RemoteState)
		}
		updated, err := client.TransitionHistoryCapture(ctx, entry.Capture.ID, entry.UploadGrant, contract.TransitionHistoryCaptureRequest{To: next, ExpectedVersion: entry.RemoteVersion})
		if err != nil {
			return err
		}
		if err := applyRemote(entry, updated); err != nil {
			return err
		}
		if entry.RemoteState != next {
			return fmt.Errorf("%w: transition acknowledgement differs", ErrInvalidOutbox)
		}
		if err := o.saveLocked(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func applyRemote(entry *Entry, capture contract.HistoryCapture) error {
	if capture.ID != entry.Capture.ID || capture.JobID != entry.Capture.JobID || capture.LeaseID != entry.Capture.LeaseID || capture.Version < entry.RemoteVersion {
		return fmt.Errorf("%w: remote capture acknowledgement differs", ErrInvalidOutbox)
	}
	entry.Capture = capture
	entry.RemoteVersion = capture.Version
	entry.RemoteState = capture.State
	return nil
}

// ReplayAll validates the outbox and attempts every pending entry in stable
// capture-ID order. Independent failures are joined; cancellation stops early.
func (o *Outbox) ReplayAll(ctx context.Context, client Client) error {
	pending, err := o.ListPending(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range pending {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := o.Publish(ctx, client, entry.Capture.ID); err != nil {
			failures = append(failures, fmt.Errorf("publish history capture %s: %w", entry.Capture.ID, err))
		}
	}
	return errors.Join(failures...)
}
