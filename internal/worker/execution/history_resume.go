package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
)

func validateHistoryResumePayload(value HistoryResumePayload) error {
	for name, field := range map[string]string{
		"resume id": value.ID, "source capture id": value.SourceCaptureID,
		"native session id": value.NativeSessionID, "Harness artifact id": value.HarnessArtifactID,
		"workspace artifact id": value.WorkspaceArtifactID, "required head commit": value.RequiredHeadCommit,
		"source Harness build": value.SourceHarnessBuild,
	} {
		if strings.TrimSpace(field) == "" {
			return fmt.Errorf("history resume %s is required", name)
		}
	}
	for name, digest := range map[string]string{"Harness": value.HarnessSHA256, "workspace": value.WorkspaceSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			return fmt.Errorf("history resume %s digest is invalid", name)
		}
	}
	if value.RequiredHarnessSchemaVersion != historyarchive.SupportedHarnessNativeSchema {
		return fmt.Errorf("history resume requires unsupported Harness schema %d (worker supports %d)", value.RequiredHarnessSchemaVersion, historyarchive.SupportedHarnessNativeSchema)
	}
	if value.SessionRelativeDir != "" {
		if strings.Contains(value.SessionRelativeDir, "\\") || path.IsAbs(value.SessionRelativeDir) ||
			path.Clean(value.SessionRelativeDir) != value.SessionRelativeDir || value.SessionRelativeDir == "." ||
			strings.HasPrefix(value.SessionRelativeDir, "../") {
			return errors.New("history resume session path is unsafe")
		}
		if err := historyarchive.ValidatePath(value.SessionRelativeDir, historyarchive.DefaultLimits().MaxPathBytes); err != nil {
			return fmt.Errorf("history resume session path: %w", err)
		}
	}
	return nil
}

type historyResumeArchives struct {
	harnessPath   string
	workspacePath string
}

func (a historyResumeArchives) cleanup() {
	_ = os.Remove(a.harnessPath)
	_ = os.Remove(a.workspacePath)
}

// prepareHistoryResumeArchives authenticates, bounds, hashes, and fully
// validates both immutable archives before prepareWorktree can mutate the job
// checkout. The later restore repeats archive validation while extracting.
func prepareHistoryResumeArchives(ctx context.Context, input RunInput, resume HistoryResumePayload, attemptDirectory string) (historyResumeArchives, error) {
	if resume.RequiredHeadCommit != strings.TrimSpace(resume.RequiredHeadCommit) || resume.SourceHarnessBuild != strings.TrimSpace(resume.SourceHarnessBuild) {
		return historyResumeArchives{}, errors.New("history resume compatibility values are not canonical")
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: input.Config.CoordinatorURL, Token: input.Config.Token})
	if err != nil {
		return historyResumeArchives{}, err
	}
	limits := historyarchive.DefaultLimits()
	archives := historyResumeArchives{}
	success := false
	defer func() {
		if !success {
			archives.cleanup()
		}
	}()
	archives.harnessPath, err = downloadResumeArtifact(ctx, client, input, resume.SourceCaptureID, resume.HarnessArtifactID, resume.HarnessSHA256, attemptDirectory, "harness.tar", limits.MaxStoredBytes)
	if err != nil {
		return historyResumeArchives{}, fmt.Errorf("download Harness archive: %w", err)
	}
	archives.workspacePath, err = downloadResumeArtifact(ctx, client, input, resume.SourceCaptureID, resume.WorkspaceArtifactID, resume.WorkspaceSHA256, attemptDirectory, "workspace.tar", limits.MaxStoredBytes)
	if err != nil {
		return historyResumeArchives{}, fmt.Errorf("download workspace archive: %w", err)
	}
	harnessInspection, err := inspectResumeArchive(ctx, archives.harnessPath, limits)
	if err != nil {
		return historyResumeArchives{}, fmt.Errorf("inspect Harness archive: %w", err)
	}
	if err := validateResumeHarnessInspection(harnessInspection, resume); err != nil {
		return historyResumeArchives{}, err
	}
	workspaceInspection, err := inspectResumeArchive(ctx, archives.workspacePath, limits)
	if err != nil {
		return historyResumeArchives{}, fmt.Errorf("inspect workspace archive: %w", err)
	}
	if workspaceInspection.Workspace == nil || workspaceInspection.Workspace.HeadCommit != resume.RequiredHeadCommit {
		return historyResumeArchives{}, errors.New("history resume workspace archive does not match the required head")
	}
	success = true
	return archives, nil
}

func inspectResumeArchive(ctx context.Context, archivePath string, limits historyarchive.Limits) (historyarchive.Inspection, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return historyarchive.Inspection{}, err
	}
	inspection, inspectErr := historyarchive.Inspect(ctx, file, limits)
	closeErr := file.Close()
	if inspectErr != nil {
		return historyarchive.Inspection{}, inspectErr
	}
	return inspection, closeErr
}

func validateResumeHarnessInspection(inspection historyarchive.Inspection, resume HistoryResumePayload) error {
	if inspection.Harness == nil || inspection.Harness.HarnessBuild != resume.SourceHarnessBuild {
		return errors.New("history resume Harness archive metadata does not match the source build")
	}
	memberMatches := 0
	for _, member := range inspection.Harness.Members {
		if member.NativeSessionID != resume.NativeSessionID {
			continue
		}
		relativeDir := path.Dir(member.RelativeMemberPath)
		if relativeDir == "." {
			relativeDir = ""
		}
		if member.ParseStatus != "parsed" || member.HarnessBuild != resume.SourceHarnessBuild ||
			member.NativeSchemaVersion != resume.RequiredHarnessSchemaVersion || relativeDir != resume.SessionRelativeDir {
			return errors.New("history resume native session metadata does not match the coordinator selection")
		}
		memberMatches++
	}
	if memberMatches != 1 {
		return errors.New("history resume native session is not unique in the restored archive")
	}
	return nil
}

func restoreHistoryResume(ctx context.Context, resume HistoryResumePayload, archives historyResumeArchives, worktree, nativeSessionRoot string) error {
	if nativeSessionRoot == "" {
		return errors.New("history resume native session root is unavailable")
	}
	limits := historyarchive.DefaultLimits()
	harnessFile, err := os.Open(archives.harnessPath)
	if err != nil {
		return err
	}
	inspection, extractErr := historyarchive.Extract(ctx, harnessFile, nativeSessionRoot, limits)
	closeErr := harnessFile.Close()
	if extractErr != nil {
		return extractErr
	}
	if closeErr != nil {
		_ = os.RemoveAll(nativeSessionRoot)
		return closeErr
	}
	validHarness := false
	defer func() {
		if !validHarness {
			_ = os.RemoveAll(nativeSessionRoot)
		}
	}()
	if err := validateResumeHarnessInspection(inspection, resume); err != nil {
		return err
	}
	statePath := filepath.Join(nativeSessionRoot, filepath.FromSlash(resume.SessionRelativeDir), "state.json")
	stateInfo, err := os.Lstat(statePath)
	if err != nil || !stateInfo.Mode().IsRegular() {
		return errors.New("history resume native session state is missing or unsafe")
	}

	workspaceFile, err := os.Open(archives.workspacePath)
	if err != nil {
		return err
	}
	manifest, restoreErr := historyarchive.RestoreWorkspace(ctx, workspaceFile, worktree, limits, "git")
	closeErr = workspaceFile.Close()
	if restoreErr != nil {
		return restoreErr
	}
	if closeErr != nil {
		return closeErr
	}
	if manifest.HeadCommit != resume.RequiredHeadCommit {
		return fmt.Errorf("history resume workspace head %q does not match required head %q", manifest.HeadCommit, resume.RequiredHeadCommit)
	}
	validHarness = true
	return nil
}

func downloadResumeArtifact(ctx context.Context, client *flowclient.Client, input RunInput, captureID, artifactID, expectedDigest, directory, name string, maxBytes int64) (string, error) {
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	pathName := file.Name()
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(pathName)
		}
	}()
	hash := sha256.New()
	writer := &boundedResumeWriter{writer: io.MultiWriter(file, hash), remaining: maxBytes}
	if err := client.DownloadHistoryResumeArtifact(ctx, captureID, artifactID, input.Job.ID, input.Lease.ID, writer); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedDigest {
		return "", fmt.Errorf("artifact digest mismatch: got %s, want %s", actual, expectedDigest)
	}
	success = true
	return pathName, nil
}

type boundedResumeWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedResumeWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: resume artifact stored bytes", historyarchive.ErrLimitExceeded)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
