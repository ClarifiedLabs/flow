package historycapture

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
	"golang.org/x/sys/unix"
)

const (
	maxStateBytes                   = 16 << 20
	reservationTempDirPrefix        = ".reservation-"
	maxSensitiveValues              = 16
	maxSensitiveValueBytes          = 64 << 10
	maxSensitiveTotalBytes          = 256 << 10
	sensitiveValuesEncoding         = 1
	sensitiveValuesNonceBytes       = 12
	sensitiveValuesTagBytes         = 16
	minSensitiveValuesPlaintext     = 1 + 2 + 4 + 1
	maxSensitiveValuesPlaintext     = 1 + 2 + maxSensitiveValues*4 + maxSensitiveTotalBytes
	sensitiveDataKeyDeriveContext   = "flow/historycapture/sensitive-data-key/v1\x00"
	sensitiveValuesAADContext       = "flow/historycapture/sensitive-values/v1\x00"
	terminalCredentialAADContext    = "flow/historycapture/terminal-credential/v1\x00"
	terminalCredentialNonceBytes    = 12
	terminalCredentialTagBytes      = 16
	maxTerminalCredentialPlaintext  = 64 << 10
	minTerminalCredentialCiphertext = 1 + terminalCredentialTagBytes
)

// Outbox serializes local mutations. A worker must use one Outbox instance for
// a directory; the durable protocol makes restart/replay safe.
type Outbox struct {
	mu                  sync.Mutex
	options             Options
	root                string
	sensitiveDataKey    [sha256.Size]byte
	hasSensitiveDataKey bool
}

func New(options Options) (*Outbox, error) {
	if strings.TrimSpace(options.Dir) == "" || options.SegmentBytes <= 0 || options.MaxOutstandingBytes <= 0 || options.MaxOutstandingEntries <= 0 {
		return nil, fmt.Errorf("%w: every outbox limit and directory is required", ErrInvalidOutbox)
	}
	if options.ArchiveLimits == (historyarchive.Limits{}) {
		options.ArchiveLimits = historyarchive.DefaultLimits()
	}
	root, err := filepath.Abs(options.Dir)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: outbox root is not a directory", ErrInvalidOutbox)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, statErr
	} else if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	key, hasKey := deriveSensitiveDataKey(options.SensitiveDataKey)
	options.SensitiveDataKey = nil
	return &Outbox{options: options, root: root, sensitiveDataKey: key, hasSensitiveDataKey: hasKey}, nil
}

// RecordReservation creates or refreshes an outbox entry. It is intended to be
// called immediately after ReserveHistoryCapture, before worker execution.
func (o *Outbox) RecordReservation(ctx context.Context, input Reservation) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	capture := input.Response.Capture
	if err := validateCaptureID(capture.ID); err != nil || strings.TrimSpace(input.Response.UploadGrant) == "" || capture.JobID == "" || capture.LeaseID == "" || !capture.ExpectedTranscript {
		return Entry{}, fmt.Errorf("%w: invalid reservation", ErrInvalidOutbox)
	}
	sources, err := normalizeSources(input.Sources, capture.ExpectedHarness)
	if err != nil {
		return Entry{}, err
	}
	sensitiveValues, err := normalizeSensitiveValues(input.SensitiveValues)
	if err != nil {
		return Entry{}, err
	}
	if len(sensitiveValues) != 0 && !o.hasSensitiveDataKey {
		return Entry{}, fmt.Errorf("%w: sensitive data key is required", ErrInvalidOutbox)
	}
	entries, err := o.loadAllLocked(ctx)
	if err != nil {
		return Entry{}, err
	}
	var existing *Entry
	for index := range entries {
		if entries[index].Capture.ID == capture.ID {
			existing = &entries[index]
			break
		}
	}
	if existing == nil {
		pending := 0
		for _, entry := range entries {
			if entry.Status != statusComplete {
				pending++
			}
		}
		if pending >= o.options.MaxOutstandingEntries {
			return Entry{}, fmt.Errorf("%w: outstanding capture count", ErrLimitExceeded)
		}
		entry := Entry{FormatVersion: stateFormatVersion, Protocol: contract.ProtocolVersion, Status: statusReserved,
			Capture: capture, UploadGrant: input.Response.UploadGrant, RemoteVersion: capture.Version, RemoteState: capture.State, Sources: sources}
		entry.SensitiveValuesCiphertext, err = o.encryptSensitiveValues(&entry, sensitiveValues)
		if err != nil {
			return Entry{}, err
		}
		if err := o.createEntryLocked(ctx, &entry); err != nil {
			return Entry{}, err
		}
		return cloneEntry(entry)
	}
	if existing.Capture.ProjectID != capture.ProjectID || existing.Capture.JobID != capture.JobID || existing.Capture.LeaseID != capture.LeaseID ||
		existing.Capture.LeaseAttempt != capture.LeaseAttempt || existing.Capture.ExpectedTranscript != capture.ExpectedTranscript || existing.Capture.ExpectedHarness != capture.ExpectedHarness ||
		existing.Capture.HarnessName != capture.HarnessName || existing.Capture.HarnessVersion != capture.HarnessVersion ||
		existing.Capture.HarnessSchemaVersion != capture.HarnessSchemaVersion {
		return Entry{}, fmt.Errorf("%w: reservation metadata changed", ErrInvalidOutbox)
	}
	if existing.Status == statusReserved || existing.Status == statusFinalized {
		durableValues, decryptErr := o.decryptSensitiveValues(existing)
		if decryptErr != nil {
			return Entry{}, decryptErr
		}
		if !sameSensitiveValues(durableValues, sensitiveValues) {
			return Entry{}, fmt.Errorf("%w: reservation sensitive values changed", ErrInvalidOutbox)
		}
	}
	if existing.Status == statusReserved {
		existing.Sources = sources
	}
	existing.Capture = capture
	existing.UploadGrant = input.Response.UploadGrant
	existing.RemoteVersion = capture.Version
	existing.RemoteState = capture.State
	if capture.State == "complete" {
		existing.Status, existing.Progress.Completed = statusComplete, true
		if err := o.completeLocked(ctx, existing); err != nil {
			return Entry{}, err
		}
		return cloneEntry(*existing)
	}
	if err := o.saveLocked(ctx, existing); err != nil {
		return Entry{}, err
	}
	return cloneEntry(*existing)
}

// UpdateSources replaces capture source paths until staging makes them
// immutable. It supports crash recovery when execution failed before a usable
// worktree or transcript was created.
func (o *Outbox) UpdateSources(ctx context.Context, captureID string, sources SourcePaths) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	if entry.Status != statusReserved && entry.Status != statusFinalized {
		return Entry{}, fmt.Errorf("%w: sources are immutable after staging", ErrInvalidOutbox)
	}
	normalized, err := normalizeSources(sources, entry.Capture.ExpectedHarness)
	if err != nil {
		return Entry{}, err
	}
	entry.Sources = normalized
	if err := o.saveLocked(ctx, &entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry)
}

// Start durably records the remote running transition before user code starts.
// Repeating it after an ambiguous response is safe because coordinator
// transitions are idempotent for the same expected version and actor.
func (o *Outbox) Start(ctx context.Context, client Client, captureID string) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	if entry.Status != statusReserved {
		return Entry{}, fmt.Errorf("%w: cannot start a staged capture", ErrInvalidOutbox)
	}
	if entry.RemoteState == "running" {
		return cloneEntry(entry)
	}
	if entry.RemoteState != "reserved" {
		return Entry{}, fmt.Errorf("%w: cannot start from remote state %q", ErrInvalidOutbox, entry.RemoteState)
	}
	updated, err := client.TransitionHistoryCapture(ctx, entry.Capture.ID, entry.UploadGrant, contract.TransitionHistoryCaptureRequest{To: "running", ExpectedVersion: entry.RemoteVersion})
	if err != nil {
		return Entry{}, err
	}
	if err := applyRemote(&entry, updated); err != nil {
		return Entry{}, err
	}
	if entry.RemoteState != "running" {
		return Entry{}, fmt.Errorf("%w: start transition acknowledgement differs", ErrInvalidOutbox)
	}
	if err := o.saveLocked(ctx, &entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry)
}

// RecordFinal checkpoints the immutable execution result before any source
// recovery or archive construction can fail.
func (o *Outbox) RecordFinal(ctx context.Context, captureID string, final Final) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	staged := stagedFinal{Verdict: strings.TrimSpace(final.Verdict), ExitCode: cloneInt(final.ExitCode), ErrorCode: strings.TrimSpace(final.ErrorCode), WorkspaceBaseRef: strings.TrimSpace(final.WorkspaceBaseRef)}
	if err := validateFinal(staged); err != nil {
		return Entry{}, err
	}
	terminal, err := normalizeTerminal(final.Terminal)
	if err != nil {
		return Entry{}, err
	}
	if entry.Status == statusComplete {
		matches, matchErr := o.terminalMatches(&entry, terminal)
		if matchErr != nil {
			return Entry{}, matchErr
		}
		if !matches {
			return Entry{}, fmt.Errorf("%w: terminal action differs from immutable checkpoint", ErrInvalidOutbox)
		}
		return cloneEntry(entry)
	}
	if entry.Status != statusReserved {
		matches, matchErr := o.terminalMatches(&entry, terminal)
		if matchErr != nil {
			return Entry{}, matchErr
		}
		if !sameFinal(entry.Final, staged) || !matches {
			return Entry{}, fmt.Errorf("%w: final result differs from immutable checkpoint", ErrInvalidOutbox)
		}
		return cloneEntry(entry)
	}
	durableTerminal := terminalWithoutCredential(terminal)
	credential, err := o.encryptTerminalCredential(&entry, terminal)
	if err != nil {
		return Entry{}, err
	}
	entry.Final, entry.Terminal, entry.TerminalCredentialCiphertext, entry.Status = staged, durableTerminal, credential, statusFinalized
	if err := o.saveLocked(ctx, &entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry)
}

// Stage captures all final bytes and protocol metadata locally. RecordFinal
// must have checkpointed the execution result first.
func (o *Outbox) Stage(ctx context.Context, captureID string, final Final) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	if entry.Status == statusComplete {
		return cloneEntry(entry)
	}
	staged := stagedFinal{Verdict: strings.TrimSpace(final.Verdict), ExitCode: cloneInt(final.ExitCode), ErrorCode: strings.TrimSpace(final.ErrorCode), WorkspaceBaseRef: strings.TrimSpace(final.WorkspaceBaseRef)}
	if err := validateFinal(staged); err != nil {
		return Entry{}, err
	}
	if entry.Status == statusStaged {
		if !sameFinal(entry.Final, staged) {
			return Entry{}, fmt.Errorf("%w: final result differs from immutable stage", ErrInvalidOutbox)
		}
		return cloneEntry(entry)
	}
	finalSensitiveValues, err := normalizeSensitiveValues(final.SensitiveValues)
	if err != nil {
		return Entry{}, err
	}
	durableSensitiveValues, err := o.decryptSensitiveValues(&entry)
	if err != nil {
		return Entry{}, err
	}
	sensitiveValues, err := mergeSensitiveValues(durableSensitiveValues, finalSensitiveValues)
	if err != nil {
		return Entry{}, err
	}
	if entry.Status == statusReserved {
		entry.Final, entry.Status = staged, statusFinalized
		if err := o.saveLocked(ctx, &entry); err != nil {
			return Entry{}, err
		}
	}
	if entry.Status != statusFinalized || !sameFinal(entry.Final, staged) {
		return Entry{}, fmt.Errorf("%w: execution result was not checkpointed", ErrInvalidOutbox)
	}
	entryDir, _ := o.entryDir(captureID)
	if err := removeOrphanPayloads(entryDir); err != nil {
		return Entry{}, err
	}
	otherBytes, err := o.otherOutstandingBytesLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	remaining := o.options.MaxOutstandingBytes - otherBytes
	if remaining < 0 {
		return Entry{}, fmt.Errorf("%w: outstanding staged bytes", ErrLimitExceeded)
	}
	stagedCapacity := remaining
	artifacts, seal, err := o.stageTranscript(ctx, entryDir, entry.Sources.Transcript, sensitiveValues, remaining)
	if err != nil {
		_ = removeOrphanPayloads(entryDir)
		return Entry{}, err
	}
	remaining, err = consumeOutstandingBytes(remaining, seal.LogicalLength)
	if err != nil {
		_ = removeOrphanPayloads(entryDir)
		return Entry{}, err
	}
	cleanup := func() {
		for _, artifact := range artifacts {
			_ = os.Remove(filepath.Join(entryDir, artifact.Path))
		}
		_ = os.Remove(filepath.Join(entryDir, workspaceFileName))
		_ = os.Remove(filepath.Join(entryDir, harnessFileName))
	}
	workspaceLimits := boundedArchiveLimits(o.options.ArchiveLimits, remaining)
	workspace, summary, err := o.stageWorkspace(ctx, entryDir, entry.Sources.Worktree, Final{WorkspaceBaseRef: entry.Final.WorkspaceBaseRef, SensitiveValues: sensitiveValues}, workspaceLimits)
	if err != nil {
		cleanup()
		if workspaceLimits.MaxStoredBytes < o.options.ArchiveLimits.MaxStoredBytes && errors.Is(err, historyarchive.ErrLimitExceeded) {
			err = fmt.Errorf("%w: workspace bytes: %w", ErrLimitExceeded, err)
		}
		return Entry{}, err
	}
	artifacts = append(artifacts, workspace)
	remaining, err = consumeOutstandingBytes(remaining, workspace.StoredSize)
	if err != nil {
		cleanup()
		return Entry{}, err
	}
	var harnessMembers *contract.RegisterHistoryHarnessMembersRequest
	if entry.Capture.ExpectedHarness {
		harnessLimits := boundedArchiveLimits(o.options.ArchiveLimits, remaining)
		var harness artifactRecord
		harness, harnessMembers, err = o.stageHarness(ctx, entryDir, &entry, sensitiveValues, harnessLimits)
		if err != nil {
			cleanup()
			if harnessLimits.MaxStoredBytes < o.options.ArchiveLimits.MaxStoredBytes && errors.Is(err, historyarchive.ErrLimitExceeded) {
				err = fmt.Errorf("%w: Harness bytes: %w", ErrLimitExceeded, err)
			}
			return Entry{}, err
		}
		artifacts = append(artifacts, harness)
		remaining, err = consumeOutstandingBytes(remaining, harness.StoredSize)
		if err != nil {
			cleanup()
			return Entry{}, err
		}
	}

	var stagedBytes int64
	for _, artifact := range artifacts {
		stagedBytes += artifact.StoredSize
		if stagedBytes < 0 {
			cleanup()
			return Entry{}, fmt.Errorf("%w: staged byte overflow", ErrLimitExceeded)
		}
	}
	if stagedBytes > stagedCapacity {
		cleanup()
		return Entry{}, fmt.Errorf("%w: outstanding staged bytes", ErrLimitExceeded)
	}
	expected := []contract.HistoryFinalArtifactExpectation{
		{LogicalKey: "manifest/final", Kind: "manifest"},
		{LogicalKey: "workspace/final", Kind: "workspace_snapshot"},
	}
	if entry.Capture.ExpectedHarness {
		expected = append(expected, contract.HistoryFinalArtifactExpectation{LogicalKey: "harness/final", Kind: "harness_root"})
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].LogicalKey < expected[j].LogicalKey })
	entry.Status, entry.Artifacts = statusStaged, artifacts
	entry.SensitiveValuesCiphertext = nil
	entry.TranscriptSeal, entry.WorkspaceSummary, entry.HarnessMembers = &seal, &summary, harnessMembers
	entry.ExpectedSet = &contract.DeclareHistoryExpectedSetRequest{Artifacts: expected, TranscriptSeal: &seal}
	if err := o.saveLocked(ctx, &entry); err != nil {
		cleanup()
		return Entry{}, err
	}
	return cloneEntry(entry)
}

func (o *Outbox) stageTranscript(ctx context.Context, entryDir, source string, sensitiveValues [][]byte, maxBytes int64) ([]artifactRecord, contract.HistoryTranscriptSeal, error) {
	file, err := openRegular(source)
	if err != nil {
		return nil, contract.HistoryTranscriptSeal{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, contract.HistoryTranscriptSeal{}, err
	}
	if info.Size() > maxBytes {
		return nil, contract.HistoryTranscriptSeal{}, fmt.Errorf("%w: transcript bytes", ErrLimitExceeded)
	}
	streamHash := sha256.New()
	buffer := make([]byte, int(o.options.SegmentBytes))
	var artifacts []artifactRecord
	var scanTail []byte
	maxSensitiveLength := longestSensitiveValue(sensitiveValues)
	var offset int64
	for sequence := int64(0); ; sequence++ {
		if err := ctx.Err(); err != nil {
			return nil, contract.HistoryTranscriptSeal{}, err
		}
		n, readErr := io.ReadFull(file, buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil, contract.HistoryTranscriptSeal{}, readErr
		}
		if n == 0 {
			break
		}
		if int64(n) > maxBytes-offset {
			return nil, contract.HistoryTranscriptSeal{}, fmt.Errorf("%w: transcript bytes", ErrLimitExceeded)
		}
		chunk := buffer[:n]
		scanBytes := append(append([]byte(nil), scanTail...), chunk...)
		if containsSensitiveValue(scanBytes, sensitiveValues) {
			return nil, contract.HistoryTranscriptSeal{}, historyarchive.ErrSensitiveContent
		}
		if maxSensitiveLength > 1 {
			keep := maxSensitiveLength - 1
			if keep > len(scanBytes) {
				keep = len(scanBytes)
			}
			scanTail = append(scanTail[:0], scanBytes[len(scanBytes)-keep:]...)
		}
		if _, err := streamHash.Write(chunk); err != nil {
			return nil, contract.HistoryTranscriptSeal{}, err
		}
		name := fmt.Sprintf("transcript-%012d.bin", sequence)
		if err := atomicWrite(entryDir, name, chunk); err != nil {
			return nil, contract.HistoryTranscriptSeal{}, err
		}
		digest := sha256.Sum256(chunk)
		end := offset + int64(n)
		artifacts = append(artifacts, artifactRecord{
			Path: name, SHA256: hex.EncodeToString(digest[:]), StoredSize: int64(n),
			Publish: contract.PublishHistoryArtifactRequest{LogicalKey: fmt.Sprintf("transcript/%012d", sequence), Kind: "transcript_segment", Phase: "final", MediaType: "text/plain; charset=utf-8", FormatVersion: 1, SchemaVersion: 1, LogicalSize: int64(n), EntryCount: 1},
			Segment: &contract.RegisterHistoryTranscriptSegmentRequest{ArtifactLogicalKey: fmt.Sprintf("transcript/%012d", sequence), Epoch: 0, Sequence: sequence, StartOffset: offset, EndOffset: end, UncompressedSize: int64(n), Encoding: "identity"},
		})
		offset = end
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
	}
	seal := contract.HistoryTranscriptSeal{FinalEpoch: -1, SegmentCount: int64(len(artifacts)), LogicalLength: offset, SHA256: hex.EncodeToString(streamHash.Sum(nil))}
	if len(artifacts) > 0 {
		seal.FinalEpoch = 0
	}
	return artifacts, seal, nil
}

func (o *Outbox) stageWorkspace(ctx context.Context, entryDir, repo string, final Final, limits historyarchive.Limits) (artifactRecord, contract.RegisterHistoryWorkspaceSummaryRequest, error) {
	path, file, err := createPayloadTemp(entryDir, workspaceFileName)
	if err != nil {
		return artifactRecord{}, contract.RegisterHistoryWorkspaceSummaryRequest{}, err
	}
	artifact, _, writeErr := historyarchive.WriteWorkspace(ctx, file, repo, historyarchive.WorkspaceOptions{Limits: limits, BaseRef: final.WorkspaceBaseRef, SensitiveValues: final.SensitiveValues})
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return artifactRecord{}, contract.RegisterHistoryWorkspaceSummaryRequest{}, writeErr
	}
	inspection, err := inspectArchive(ctx, path, limits)
	if err != nil || inspection.Workspace == nil || inspection.Kind != historyarchive.ArchiveWorkspace || inspection.SHA256 != artifact.SHA256 || inspection.StoredBytes != artifact.StoredBytes {
		_ = os.Remove(path)
		if err == nil {
			err = fmt.Errorf("%w: workspace archive inspection differs", ErrInvalidOutbox)
		}
		return artifactRecord{}, contract.RegisterHistoryWorkspaceSummaryRequest{}, err
	}
	if err := commitPayload(entryDir, path, workspaceFileName); err != nil {
		return artifactRecord{}, contract.RegisterHistoryWorkspaceSummaryRequest{}, err
	}
	manifest := inspection.Workspace
	record := artifactRecord{Path: workspaceFileName, SHA256: inspection.SHA256, StoredSize: inspection.StoredBytes,
		Publish: contract.PublishHistoryArtifactRequest{LogicalKey: "workspace/final", Kind: "workspace_snapshot", Phase: "final", ArchiveID: inspection.SHA256, MediaType: "application/vnd.flow.workspace+tar+gzip", FormatVersion: historyarchive.WorkspaceFormatVersion, SchemaVersion: historyarchive.WorkspaceSchemaVersion, LogicalSize: inspection.LogicalBytes, EntryCount: int64(inspection.EntryCount)}}
	summary := contract.RegisterHistoryWorkspaceSummaryRequest{ArtifactLogicalKey: "workspace/final", ArchiveSchemaVersion: manifest.SchemaVersion,
		Branch: manifest.Branch, Detached: manifest.Detached, BaseRef: manifest.BaseRef, BaseCommit: manifest.BaseCommit, HeadCommit: manifest.HeadCommit,
		StagedCount: int64(len(manifest.StagedPaths)), UnstagedCount: int64(len(manifest.UnstagedPaths)), UntrackedCount: int64(len(manifest.Untracked)), InventoryDigest: manifest.InventoryDigest, ValidationStatus: "valid"}
	return record, summary, nil
}

func (o *Outbox) stageHarness(ctx context.Context, entryDir string, entry *Entry, sensitiveValues [][]byte, limits historyarchive.Limits) (artifactRecord, *contract.RegisterHistoryHarnessMembersRequest, error) {
	path, file, err := createPayloadTemp(entryDir, harnessFileName)
	if err != nil {
		return artifactRecord{}, nil, err
	}
	artifact, _, writeErr := historyarchive.WriteHarness(ctx, file, entry.Sources.NativeSessionRoot, historyarchive.HarnessOptions{Limits: limits, HarnessBuild: entry.Capture.HarnessVersion, RootSessionID: entry.Sources.NativeSessionID, SensitiveValues: sensitiveValues})
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return artifactRecord{}, nil, writeErr
	}
	inspection, err := inspectArchive(ctx, path, limits)
	if err != nil || inspection.Harness == nil || inspection.Kind != historyarchive.ArchiveHarness || inspection.SHA256 != artifact.SHA256 || inspection.StoredBytes != artifact.StoredBytes {
		_ = os.Remove(path)
		if err == nil {
			err = fmt.Errorf("%w: Harness archive inspection differs", ErrInvalidOutbox)
		}
		return artifactRecord{}, nil, err
	}
	manifest := inspection.Harness
	if manifest.HarnessBuild != entry.Capture.HarnessVersion ||
		(entry.Sources.NativeSessionID != "" && manifest.RootSessionID != entry.Sources.NativeSessionID) || len(manifest.Members) == 0 {
		_ = os.Remove(path)
		return artifactRecord{}, nil, fmt.Errorf("%w: Harness archive identity differs", ErrInvalidOutbox)
	}
	members := make([]contract.HistoryHarnessMemberInput, 0, len(manifest.Members))
	roots := 0
	for _, member := range manifest.Members {
		if member.ParseStatus != "parsed" || member.HarnessBuild != entry.Capture.HarnessVersion || member.NativeSessionID == "" {
			_ = os.Remove(path)
			return artifactRecord{}, nil, fmt.Errorf("%w: Harness member is not parsed and build-matched", ErrInvalidOutbox)
		}
		if member.MemberKind == "root" {
			roots++
			if member.NativeSessionID != manifest.RootSessionID || member.NativeParentSessionID != "" {
				_ = os.Remove(path)
				return artifactRecord{}, nil, fmt.Errorf("%w: invalid Harness root member", ErrInvalidOutbox)
			}
		}
		members = append(members, contract.HistoryHarnessMemberInput{NativeSessionID: member.NativeSessionID, NativeParentSessionID: member.NativeParentSessionID,
			RelativeMemberPath: member.RelativeMemberPath, MemberKind: member.MemberKind, AgentName: member.AgentName, Status: member.Status,
			Model: member.Model, HarnessBuild: member.HarnessBuild, ParseStatus: member.ParseStatus})
	}
	entry.Sources.NativeSessionID = manifest.RootSessionID
	if roots != 1 {
		_ = os.Remove(path)
		return artifactRecord{}, nil, fmt.Errorf("%w: Harness archive must have exactly one root member", ErrInvalidOutbox)
	}
	if err := commitPayload(entryDir, path, harnessFileName); err != nil {
		return artifactRecord{}, nil, err
	}
	record := artifactRecord{Path: harnessFileName, SHA256: inspection.SHA256, StoredSize: inspection.StoredBytes,
		Publish: contract.PublishHistoryArtifactRequest{LogicalKey: "harness/final", Kind: "harness_root", Phase: "final", ArchiveID: inspection.SHA256, MediaType: "application/vnd.flow.harness+tar+gzip", FormatVersion: historyarchive.HarnessFormatVersion, SchemaVersion: historyarchive.HarnessSchemaVersion, LogicalSize: inspection.LogicalBytes, EntryCount: int64(inspection.EntryCount)}}
	return record, &contract.RegisterHistoryHarnessMembersRequest{ArtifactLogicalKey: "harness/final", Members: members}, nil
}

// Get validates and returns one durable capture entry.
func (o *Outbox) Get(ctx context.Context, captureID string) (Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry)
}

// ListPending validates and returns all entries requiring publication or
// terminal lifecycle reconciliation in capture-ID order.
func (o *Outbox) ListPending(ctx context.Context) ([]Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := o.loadAllLocked(ctx)
	if err != nil {
		return nil, err
	}
	pending := entries[:0]
	for _, entry := range entries {
		if entry.Status != statusComplete || entry.Terminal != nil {
			copy, _ := cloneEntry(entry)
			pending = append(pending, copy)
		}
	}
	return pending, nil
}

// PruneCompleted removes only validated, scrubbed completion tombstones. It
// first removes entries older than CompletedBefore (when set), then removes the
// oldest remaining entries until at most MaxEntries remain. Completion time and
// capture ID provide a deterministic ordering; tombstones without a coordinator
// completion timestamp sort before timestamped tombstones. Pending entries and
// their payloads are never selected.
func (o *Outbox) PruneCompleted(ctx context.Context, retention CompletedRetention) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if retention.MaxEntries <= 0 {
		return 0, fmt.Errorf("%w: completed retention max entries must be positive", ErrInvalidOutbox)
	}
	entries, err := o.loadAllLocked(ctx)
	if err != nil {
		return 0, err
	}
	type tombstone struct {
		id          string
		completedAt time.Time
	}
	completed := make([]tombstone, 0, len(entries))
	for _, entry := range entries {
		if entry.Status != statusComplete || entry.Terminal != nil {
			continue
		}
		var completedAt time.Time
		if entry.Capture.CompletedAt != "" {
			completedAt, err = time.Parse(time.RFC3339Nano, entry.Capture.CompletedAt)
			if err != nil {
				return 0, fmt.Errorf("%w: invalid completion timestamp for %q", ErrInvalidOutbox, entry.Capture.ID)
			}
		}
		completed = append(completed, tombstone{id: entry.Capture.ID, completedAt: completedAt})
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].completedAt.Equal(completed[j].completedAt) {
			return completed[i].id < completed[j].id
		}
		return completed[i].completedAt.Before(completed[j].completedAt)
	})
	remove := make([]bool, len(completed))
	remaining := len(completed)
	if !retention.CompletedBefore.IsZero() {
		for index, entry := range completed {
			if !entry.completedAt.IsZero() && entry.completedAt.Before(retention.CompletedBefore) {
				remove[index] = true
				remaining--
			}
		}
	}
	for index := 0; remaining > retention.MaxEntries; index++ {
		if !remove[index] {
			remove[index] = true
			remaining--
		}
	}
	removed := 0
	for index, entry := range completed {
		if !remove[index] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		entryDir, err := o.entryDir(entry.id)
		if err != nil {
			return removed, err
		}
		if err := os.RemoveAll(entryDir); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		if err := syncDir(o.root); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (o *Outbox) otherOutstandingBytesLocked(ctx context.Context, except string) (int64, error) {
	entries, err := o.loadAllLocked(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		entryDir, err := o.entryDir(entry.Capture.ID)
		if err != nil {
			return 0, err
		}
		if entry.Status == statusStaged {
			err = removeStagePayloadTemps(entryDir)
		} else {
			err = removeOrphanPayloads(entryDir)
		}
		if err != nil {
			return 0, err
		}
		if entry.Capture.ID == except || entry.Status == statusComplete {
			continue
		}
		for _, artifact := range entry.Artifacts {
			total += artifact.StoredSize
			if total < 0 {
				return 0, fmt.Errorf("%w: byte overflow", ErrLimitExceeded)
			}
		}
	}
	return total, nil
}

func (o *Outbox) loadAllLocked(ctx context.Context) ([]Entry, error) {
	dirs, err := os.ReadDir(o.root)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(dirs))
	removedReservationTemp := false
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(dir.Name(), reservationTempDirPrefix) {
			path, pathErr := containedPath(o.root, dir.Name())
			info, statErr := os.Lstat(path)
			if pathErr != nil || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
				return nil, fmt.Errorf("%w: unsafe incomplete reservation", ErrInvalidOutbox)
			}
			if err := os.RemoveAll(path); err != nil {
				return nil, err
			}
			removedReservationTemp = true
			continue
		}
		if !dir.IsDir() || validateCaptureID(dir.Name()) != nil {
			return nil, fmt.Errorf("%w: unexpected outbox object", ErrInvalidOutbox)
		}
		entry, err := o.loadLocked(ctx, dir.Name())
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if removedReservationTemp {
		if err := syncDir(o.root); err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Capture.ID < entries[j].Capture.ID })
	return entries, nil
}

func (o *Outbox) loadLocked(ctx context.Context, captureID string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	entryDir, err := o.entryDir(captureID)
	if err != nil {
		return Entry{}, err
	}
	info, err := os.Lstat(entryDir)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, ErrNotFound
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return Entry{}, fmt.Errorf("%w: unsafe entry directory", ErrInvalidOutbox)
	}
	statePath := filepath.Join(entryDir, stateFileName)
	stateInfo, err := os.Lstat(statePath)
	if err != nil || !stateInfo.Mode().IsRegular() || stateInfo.Mode().Perm() != 0600 || stateInfo.Size() > maxStateBytes {
		return Entry{}, fmt.Errorf("%w: unsafe state file", ErrInvalidOutbox)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return Entry{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var entry Entry
	if err := decoder.Decode(&entry); err != nil {
		return Entry{}, fmt.Errorf("%w: decode state: %v", ErrInvalidOutbox, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Entry{}, fmt.Errorf("%w: trailing state JSON", ErrInvalidOutbox)
	}
	if err := o.validateEntry(ctx, entryDir, captureID, &entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (o *Outbox) validateEntry(ctx context.Context, entryDir, captureID string, entry *Entry) error {
	if entry.FormatVersion != stateFormatVersion || entry.Protocol != contract.ProtocolVersion || entry.Capture.ID != captureID || entry.RemoteVersion < 0 {
		return fmt.Errorf("%w: invalid state identity", ErrInvalidOutbox)
	}
	if entry.Status != statusReserved && entry.Status != statusFinalized && entry.Status != statusStaged && entry.Status != statusComplete {
		return fmt.Errorf("%w: invalid local state", ErrInvalidOutbox)
	}
	if err := validateSensitiveValuesCiphertext(entry.SensitiveValuesCiphertext); err != nil {
		return err
	}
	if err := validateTerminalCredentialCiphertext(entry.TerminalCredentialCiphertext); err != nil {
		return err
	}
	if (entry.Status == statusStaged || entry.Status == statusComplete) && entry.SensitiveValuesCiphertext != nil {
		return fmt.Errorf("%w: staged state retained sensitive value ciphertext", ErrInvalidOutbox)
	}
	var terminal *TerminalAction
	if entry.Terminal != nil {
		var err error
		terminal, err = normalizeDurableTerminal(entry.Terminal)
		if err != nil {
			return err
		}
		if !sameTerminal(entry.Terminal, terminal) {
			return fmt.Errorf("%w: terminal action is not normalized", ErrInvalidOutbox)
		}
	}
	if terminal == nil || terminal.Kind != TerminalConsoleExit {
		if entry.TerminalCredentialCiphertext != nil {
			return fmt.Errorf("%w: unexpected terminal credential ciphertext", ErrInvalidOutbox)
		}
	} else if entry.TerminalCredentialCiphertext == nil {
		return fmt.Errorf("%w: console action has no terminal credential ciphertext", ErrInvalidOutbox)
	}
	if entry.Status == statusReserved && entry.Terminal != nil {
		return fmt.Errorf("%w: reserved state contains a terminal action", ErrInvalidOutbox)
	}
	if entry.Status == statusComplete {
		if !entry.Progress.Completed || entry.RemoteState != "complete" || entry.UploadGrant != "" || len(entry.Artifacts) != 0 ||
			entry.Sources != (SourcePaths{}) || entry.Final != (stagedFinal{}) || entry.TranscriptSeal != nil || entry.WorkspaceSummary != nil ||
			entry.HarnessMembers != nil || entry.ExpectedSet != nil {
			return fmt.Errorf("%w: invalid completed tombstone", ErrInvalidOutbox)
		}
		return nil
	}
	if entry.UploadGrant == "" {
		return fmt.Errorf("%w: pending entry has no upload grant", ErrInvalidOutbox)
	}
	if _, err := normalizeSources(entry.Sources, entry.Capture.ExpectedHarness); err != nil {
		return err
	}
	if entry.Status == statusReserved || entry.Status == statusFinalized {
		if len(entry.Artifacts) != 0 || entry.TranscriptSeal != nil || entry.ExpectedSet != nil {
			return fmt.Errorf("%w: unstaged entry contains a stage", ErrInvalidOutbox)
		}
		if entry.Status == statusReserved && entry.Final.Verdict != "" || entry.Status == statusFinalized && validateFinal(entry.Final) != nil {
			return fmt.Errorf("%w: invalid final checkpoint", ErrInvalidOutbox)
		}
		return nil
	}
	if entry.TranscriptSeal == nil || entry.WorkspaceSummary == nil || entry.ExpectedSet == nil || len(entry.Artifacts) == 0 {
		return fmt.Errorf("%w: staged metadata is incomplete", ErrInvalidOutbox)
	}
	if entry.Capture.ExpectedHarness != (entry.HarnessMembers != nil) {
		return fmt.Errorf("%w: Harness metadata expectation differs", ErrInvalidOutbox)
	}
	seenPath, seenKey := map[string]bool{}, map[string]bool{}
	for index := range entry.Artifacts {
		artifact := &entry.Artifacts[index]
		if artifact.StoredSize < 0 || !validDigest(artifact.SHA256) || artifact.Publish.LogicalKey == "" || seenPath[artifact.Path] || seenKey[artifact.Publish.LogicalKey] {
			return fmt.Errorf("%w: invalid artifact record", ErrInvalidOutbox)
		}
		seenPath[artifact.Path], seenKey[artifact.Publish.LogicalKey] = true, true
		path, err := containedPath(entryDir, artifact.Path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != artifact.StoredSize {
			return fmt.Errorf("%w: unsafe staged payload", ErrInvalidOutbox)
		}
		if err := verifyFile(ctx, path, artifact.SHA256, artifact.StoredSize); err != nil {
			return err
		}
		if artifact.Publish.TemporaryUploadID != artifact.TemporaryUploadID {
			return fmt.Errorf("%w: upload state differs", ErrInvalidOutbox)
		}
	}
	return nil
}

// ResolveTerminal returns a cloned terminal action with any console session
// token authenticated and decrypted for authorized lifecycle replay.
func (o *Outbox) ResolveTerminal(ctx context.Context, captureID string) (*TerminalAction, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return nil, err
	}
	if entry.Terminal == nil {
		return nil, nil
	}
	terminal := *entry.Terminal
	if terminal.Kind == TerminalConsoleExit {
		plaintext, decryptErr := o.decryptTerminalCredential(&entry)
		if decryptErr != nil {
			return nil, decryptErr
		}
		terminal.SessionToken = string(plaintext)
		clear(plaintext)
	}
	resolved, err := normalizeTerminal(&terminal)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// DiscardTerminal scrubs a lifecycle action that must no longer run because
// the worker lost authoritative ownership. It is valid before or after capture
// publication and repeatable across shutdown races.
func (o *Outbox) DiscardTerminal(ctx context.Context, captureID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return err
	}
	if entry.Terminal == nil {
		return nil
	}
	entry.Terminal = nil
	entry.TerminalCredentialCiphertext = nil
	return o.saveLocked(ctx, &entry)
}

// AcknowledgeTerminal scrubs the durable post-publication lifecycle action.
// Repeating the acknowledgement is safe.
func (o *Outbox) AcknowledgeTerminal(ctx context.Context, captureID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, err := o.loadLocked(ctx, captureID)
	if err != nil {
		return err
	}
	if entry.Status != statusComplete {
		return fmt.Errorf("%w: capture publication is not complete", ErrInvalidOutbox)
	}
	if entry.Terminal == nil {
		return nil
	}
	entry.Terminal = nil
	entry.TerminalCredentialCiphertext = nil
	return o.saveLocked(ctx, &entry)
}

func (o *Outbox) completeLocked(ctx context.Context, entry *Entry) error {
	entry.Status = statusComplete
	entry.Progress.Completed = true
	entry.UploadGrant = ""
	entry.Sources = SourcePaths{}
	entry.SensitiveValuesCiphertext = nil
	entry.Final = stagedFinal{}
	entry.TranscriptSeal = nil
	entry.WorkspaceSummary = nil
	entry.HarnessMembers = nil
	entry.ExpectedSet = nil
	entry.Artifacts = nil
	if err := o.saveLocked(ctx, entry); err != nil {
		return err
	}
	entryDir, err := o.entryDir(entry.Capture.ID)
	if err != nil {
		return err
	}
	dirs, err := os.ReadDir(entryDir)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if dir.Name() == stateFileName {
			continue
		}
		path, pathErr := containedPath(entryDir, dir.Name())
		if pathErr != nil {
			return pathErr
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return syncDir(entryDir)
}

func (o *Outbox) createEntryLocked(ctx context.Context, entry *Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entryDir, err := o.entryDir(entry.Capture.ID)
	if err != nil {
		return err
	}
	data, err := marshalEntry(entry)
	if err != nil {
		return err
	}
	temporaryDir, err := os.MkdirTemp(o.root, reservationTempDirPrefix)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporaryDir)
		}
	}()
	if err := os.Chmod(temporaryDir, 0700); err != nil {
		return err
	}
	if err := atomicWrite(temporaryDir, stateFileName, data); err != nil {
		return err
	}
	if err := os.Rename(temporaryDir, entryDir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: entry directory already exists without valid state", ErrInvalidOutbox)
		}
		return err
	}
	committed = true
	return syncDir(o.root)
}

func (o *Outbox) saveLocked(ctx context.Context, entry *Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entryDir, err := o.entryDir(entry.Capture.ID)
	if err != nil {
		return err
	}
	data, err := marshalEntry(entry)
	if err != nil {
		return err
	}
	return atomicWrite(entryDir, stateFileName, data)
}

func marshalEntry(entry *Entry) ([]byte, error) {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > maxStateBytes {
		return nil, fmt.Errorf("%w: outbox state bytes", ErrLimitExceeded)
	}
	return data, nil
}

func (o *Outbox) entryDir(captureID string) (string, error) {
	if err := validateCaptureID(captureID); err != nil {
		return "", err
	}
	return containedPath(o.root, captureID)
}

func normalizeSources(s SourcePaths, harness bool) (SourcePaths, error) {
	var err error
	if s.Worktree, err = absoluteClean(s.Worktree); err != nil {
		return SourcePaths{}, fmt.Errorf("%w: worktree source", ErrInvalidOutbox)
	}
	if s.Transcript, err = absoluteClean(s.Transcript); err != nil {
		return SourcePaths{}, fmt.Errorf("%w: transcript source", ErrInvalidOutbox)
	}
	s.NativeSessionID = strings.TrimSpace(s.NativeSessionID)
	if harness {
		if s.NativeSessionRoot, err = absoluteClean(s.NativeSessionRoot); err != nil {
			return SourcePaths{}, fmt.Errorf("%w: explicit Harness native session root is required", ErrInvalidOutbox)
		}
	} else if s.NativeSessionRoot != "" || s.NativeSessionID != "" {
		return SourcePaths{}, fmt.Errorf("%w: non-Harness capture has native session source", ErrInvalidOutbox)
	}
	return s, nil
}

func absoluteClean(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("empty path")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validateCaptureID(id string) error {
	if id == "" || len(id) > 255 || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\\x00\r\n") {
		return fmt.Errorf("%w: unsafe capture ID", ErrInvalidOutbox)
	}
	return nil
}

func containedPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." || strings.ContainsRune(relative, 0) {
		return "", fmt.Errorf("%w: unsafe relative path", ErrInvalidOutbox)
	}
	joined := filepath.Join(root, relative)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes outbox", ErrInvalidOutbox)
	}
	return joined, nil
}

func deriveSensitiveDataKey(material []byte) ([sha256.Size]byte, bool) {
	if len(material) == 0 {
		return [sha256.Size]byte{}, false
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(sensitiveDataKeyDeriveContext))
	_, _ = hash.Write(material)
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	return key, true
}

func (o *Outbox) encryptSensitiveValues(entry *Entry, values [][]byte) (*sensitiveValuesCiphertext, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if !o.hasSensitiveDataKey {
		return nil, fmt.Errorf("%w: sensitive data key is required", ErrInvalidOutbox)
	}
	block, err := aes.NewCipher(o.sensitiveDataKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	plaintext := encodeSensitiveValues(values)
	defer clear(plaintext)
	return &sensitiveValuesCiphertext{
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plaintext, sensitiveValuesAAD(entry)),
	}, nil
}

func (o *Outbox) decryptSensitiveValues(entry *Entry) ([][]byte, error) {
	encrypted := entry.SensitiveValuesCiphertext
	if encrypted == nil {
		return nil, nil
	}
	if err := validateSensitiveValuesCiphertext(encrypted); err != nil {
		return nil, err
	}
	if !o.hasSensitiveDataKey {
		return nil, fmt.Errorf("%w: sensitive data key is required for encrypted state", ErrInvalidOutbox)
	}
	block, err := aes.NewCipher(o.sensitiveDataKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, encrypted.Nonce, encrypted.Ciphertext, sensitiveValuesAAD(entry))
	if err != nil {
		return nil, fmt.Errorf("%w: sensitive value ciphertext authentication failed", ErrInvalidOutbox)
	}
	defer clear(plaintext)
	return decodeSensitiveValues(plaintext)
}

func (o *Outbox) encryptTerminalCredential(entry *Entry, terminal *TerminalAction) (*terminalCredentialCiphertext, error) {
	if terminal == nil || terminal.Kind != TerminalConsoleExit {
		return nil, nil
	}
	if !o.hasSensitiveDataKey {
		return nil, fmt.Errorf("%w: sensitive data key is required for terminal credential", ErrInvalidOutbox)
	}
	block, err := aes.NewCipher(o.sensitiveDataKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	plaintext := []byte(terminal.SessionToken)
	defer clear(plaintext)
	return &terminalCredentialCiphertext{
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plaintext, terminalCredentialAAD(entry, terminal)),
	}, nil
}

func (o *Outbox) decryptTerminalCredential(entry *Entry) ([]byte, error) {
	encrypted := entry.TerminalCredentialCiphertext
	if entry.Terminal == nil || entry.Terminal.Kind != TerminalConsoleExit || encrypted == nil {
		return nil, fmt.Errorf("%w: console terminal credential is unavailable", ErrInvalidOutbox)
	}
	if err := validateTerminalCredentialCiphertext(encrypted); err != nil {
		return nil, err
	}
	if !o.hasSensitiveDataKey {
		return nil, fmt.Errorf("%w: sensitive data key is required for encrypted terminal credential", ErrInvalidOutbox)
	}
	block, err := aes.NewCipher(o.sensitiveDataKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, encrypted.Nonce, encrypted.Ciphertext, terminalCredentialAAD(entry, entry.Terminal))
	if err != nil {
		return nil, fmt.Errorf("%w: terminal credential ciphertext authentication failed", ErrInvalidOutbox)
	}
	if len(plaintext) == 0 || len(plaintext) > maxTerminalCredentialPlaintext {
		clear(plaintext)
		return nil, fmt.Errorf("%w: invalid terminal credential plaintext", ErrInvalidOutbox)
	}
	return plaintext, nil
}

func validateTerminalCredentialCiphertext(encrypted *terminalCredentialCiphertext) error {
	if encrypted == nil {
		return nil
	}
	if len(encrypted.Nonce) != terminalCredentialNonceBytes ||
		len(encrypted.Ciphertext) < minTerminalCredentialCiphertext ||
		len(encrypted.Ciphertext) > maxTerminalCredentialPlaintext+terminalCredentialTagBytes {
		return fmt.Errorf("%w: invalid terminal credential ciphertext shape", ErrInvalidOutbox)
	}
	return nil
}

func validateSensitiveValuesCiphertext(encrypted *sensitiveValuesCiphertext) error {
	if encrypted == nil {
		return nil
	}
	if len(encrypted.Nonce) != sensitiveValuesNonceBytes ||
		len(encrypted.Ciphertext) < minSensitiveValuesPlaintext+sensitiveValuesTagBytes ||
		len(encrypted.Ciphertext) > maxSensitiveValuesPlaintext+sensitiveValuesTagBytes {
		return fmt.Errorf("%w: invalid sensitive value ciphertext shape", ErrInvalidOutbox)
	}
	return nil
}

func encodeSensitiveValues(values [][]byte) []byte {
	capacity := 3
	for _, value := range values {
		capacity += 4 + len(value)
	}
	plaintext := make([]byte, 0, capacity)
	plaintext = append(plaintext, sensitiveValuesEncoding)
	plaintext = binary.BigEndian.AppendUint16(plaintext, uint16(len(values)))
	for _, value := range values {
		plaintext = binary.BigEndian.AppendUint32(plaintext, uint32(len(value)))
		plaintext = append(plaintext, value...)
	}
	return plaintext
}

func decodeSensitiveValues(plaintext []byte) ([][]byte, error) {
	if len(plaintext) < 3 || plaintext[0] != sensitiveValuesEncoding {
		return nil, fmt.Errorf("%w: invalid sensitive value plaintext", ErrInvalidOutbox)
	}
	count := int(binary.BigEndian.Uint16(plaintext[1:3]))
	if count == 0 || count > maxSensitiveValues {
		return nil, fmt.Errorf("%w: invalid sensitive value count", ErrInvalidOutbox)
	}
	values := make([][]byte, 0, count)
	offset, total := 3, 0
	for range count {
		if len(plaintext)-offset < 4 {
			return nil, fmt.Errorf("%w: truncated sensitive value length", ErrInvalidOutbox)
		}
		length := binary.BigEndian.Uint32(plaintext[offset : offset+4])
		offset += 4
		if length == 0 || length > maxSensitiveValueBytes || int(length) > len(plaintext)-offset || total > maxSensitiveTotalBytes-int(length) {
			return nil, fmt.Errorf("%w: invalid sensitive value bounds", ErrInvalidOutbox)
		}
		value := append([]byte(nil), plaintext[offset:offset+int(length)]...)
		values = append(values, value)
		offset += int(length)
		total += int(length)
	}
	if offset != len(plaintext) {
		return nil, fmt.Errorf("%w: trailing sensitive value plaintext", ErrInvalidOutbox)
	}
	normalized, err := normalizeSensitiveValues(values)
	if err != nil || !sameSensitiveValues(values, normalized) {
		return nil, fmt.Errorf("%w: sensitive values are not normalized", ErrInvalidOutbox)
	}
	return normalized, nil
}

func sensitiveValuesAAD(entry *Entry) []byte {
	aad := make([]byte, 0, len(sensitiveValuesAADContext)+64)
	aad = append(aad, sensitiveValuesAADContext...)
	aad = binary.BigEndian.AppendUint32(aad, uint32(entry.FormatVersion))
	for _, value := range []string{entry.Protocol, entry.Capture.ID, entry.Capture.ProjectID, entry.Capture.JobID, entry.Capture.LeaseID} {
		aad = binary.BigEndian.AppendUint32(aad, uint32(len(value)))
		aad = append(aad, value...)
	}
	aad = binary.BigEndian.AppendUint64(aad, uint64(entry.Capture.LeaseAttempt))
	return aad
}

func terminalCredentialAAD(entry *Entry, terminal *TerminalAction) []byte {
	aad := make([]byte, 0, len(terminalCredentialAADContext)+96)
	aad = append(aad, terminalCredentialAADContext...)
	aad = binary.BigEndian.AppendUint32(aad, uint32(entry.FormatVersion))
	for _, value := range []string{entry.Protocol, entry.Capture.ID, entry.Capture.ProjectID, entry.Capture.JobID, entry.Capture.LeaseID} {
		aad = binary.BigEndian.AppendUint32(aad, uint32(len(value)))
		aad = append(aad, value...)
	}
	aad = binary.BigEndian.AppendUint64(aad, uint64(entry.Capture.LeaseAttempt))
	for _, value := range []string{terminal.Kind, terminal.SessionID} {
		aad = binary.BigEndian.AppendUint32(aad, uint32(len(value)))
		aad = append(aad, value...)
	}
	aad = binary.BigEndian.AppendUint64(aad, uint64(terminal.ExitCode))
	return aad
}

func mergeSensitiveValues(first, second [][]byte) ([][]byte, error) {
	combined := make([][]byte, 0, len(first)+len(second))
	combined = append(combined, first...)
	combined = append(combined, second...)
	return normalizeSensitiveValues(combined)
}

func sameSensitiveValues(first, second [][]byte) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if !bytes.Equal(first[index], second[index]) {
			return false
		}
	}
	return true
}

func normalizeSensitiveValues(values [][]byte) ([][]byte, error) {
	result := make([][]byte, 0, len(values))
	total := 0
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		if len(value) > maxSensitiveValueBytes || len(result) >= maxSensitiveValues || total > maxSensitiveTotalBytes-len(value) {
			return nil, fmt.Errorf("%w: sensitive value bounds", ErrLimitExceeded)
		}
		duplicate := false
		for _, existing := range result {
			if bytes.Equal(existing, value) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		result = append(result, append([]byte(nil), value...))
		total += len(value)
	}
	return result, nil
}

func containsSensitiveValue(data []byte, values [][]byte) bool {
	for _, value := range values {
		if len(value) > 0 && bytes.Contains(data, value) {
			return true
		}
	}
	return false
}

func longestSensitiveValue(values [][]byte) int {
	longest := 0
	for _, value := range values {
		if len(value) > longest {
			longest = len(value)
		}
	}
	return longest
}

func validateFinal(final stagedFinal) error {
	switch final.Verdict {
	case "succeeded", "failed", "cancelled", "crashed":
	default:
		return fmt.Errorf("%w: verdict must be non-pending", ErrInvalidOutbox)
	}
	if len(final.ErrorCode) > 64 || final.Verdict == "succeeded" && final.ExitCode != nil && *final.ExitCode != 0 {
		return fmt.Errorf("%w: invalid execution result", ErrInvalidOutbox)
	}
	return nil
}

func sameFinal(a, b stagedFinal) bool {
	return a.Verdict == b.Verdict && a.ErrorCode == b.ErrorCode && a.WorkspaceBaseRef == b.WorkspaceBaseRef && (a.ExitCode == nil && b.ExitCode == nil || a.ExitCode != nil && b.ExitCode != nil && *a.ExitCode == *b.ExitCode)
}

func normalizeTerminal(value *TerminalAction) (*TerminalAction, error) {
	return normalizeTerminalAction(value, true)
}

func normalizeDurableTerminal(value *TerminalAction) (*TerminalAction, error) {
	return normalizeTerminalAction(value, false)
}

func normalizeTerminalAction(value *TerminalAction, requireConsoleCredential bool) (*TerminalAction, error) {
	if value == nil {
		return nil, nil
	}
	terminal := *value
	terminal.Kind = strings.TrimSpace(terminal.Kind)
	terminal.FinalState = strings.TrimSpace(terminal.FinalState)
	terminal.SessionID = strings.TrimSpace(terminal.SessionID)
	terminal.SessionToken = strings.TrimSpace(terminal.SessionToken)
	if len(terminal.SessionID) > 255 || len(terminal.SessionToken) > maxTerminalCredentialPlaintext {
		return nil, fmt.Errorf("%w: terminal action exceeds bounds", ErrInvalidOutbox)
	}
	switch terminal.Kind {
	case TerminalReleaseLease:
		if terminal.SessionID != "" || terminal.SessionToken != "" {
			return nil, fmt.Errorf("%w: lease release has session credentials", ErrInvalidOutbox)
		}
		switch terminal.FinalState {
		case "finished", "failed", "canceled", "crashed":
		default:
			return nil, fmt.Errorf("%w: invalid terminal job state", ErrInvalidOutbox)
		}
	case TerminalSessionExit:
		if terminal.SessionID == "" || terminal.SessionToken != "" || terminal.FinalState != "" {
			return nil, fmt.Errorf("%w: invalid session exit action", ErrInvalidOutbox)
		}
	case TerminalConsoleExit:
		if terminal.SessionID == "" || terminal.FinalState != "" || requireConsoleCredential == (terminal.SessionToken == "") {
			return nil, fmt.Errorf("%w: invalid console exit action", ErrInvalidOutbox)
		}
	default:
		return nil, fmt.Errorf("%w: invalid terminal action kind", ErrInvalidOutbox)
	}
	return &terminal, nil
}

func terminalWithoutCredential(terminal *TerminalAction) *TerminalAction {
	if terminal == nil {
		return nil
	}
	result := *terminal
	result.SessionToken = ""
	return &result
}

func (o *Outbox) terminalMatches(entry *Entry, terminal *TerminalAction) (bool, error) {
	if !sameTerminal(entry.Terminal, terminalWithoutCredential(terminal)) {
		return false, nil
	}
	if terminal == nil || terminal.Kind != TerminalConsoleExit {
		return true, nil
	}
	plaintext, err := o.decryptTerminalCredential(entry)
	if err != nil {
		return false, err
	}
	defer clear(plaintext)
	candidate := []byte(terminal.SessionToken)
	defer clear(candidate)
	return subtle.ConstantTimeCompare(plaintext, candidate) == 1, nil
}

func sameTerminal(a, b *TerminalAction) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneEntry(entry Entry) (Entry, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, err
	}
	var result Entry
	err = json.Unmarshal(data, &result)
	return result, err
}

func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: source is not a regular file", ErrInvalidOutbox)
	}
	file := os.NewFile(uintptr(fd), path)
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: source is not a regular file", ErrInvalidOutbox)
	}
	return file, nil
}

func inspectArchive(ctx context.Context, path string, limits historyarchive.Limits) (historyarchive.Inspection, error) {
	file, err := os.Open(path)
	if err != nil {
		return historyarchive.Inspection{}, err
	}
	defer file.Close()
	return historyarchive.Inspect(ctx, file, limits)
}

func createPayloadTemp(dir, target string) (string, *os.File, error) {
	suffix := make([]byte, 12)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "."+target+".tmp-"+hex.EncodeToString(suffix))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	return path, file, err
}

func commitPayload(dir, temporary, target string) error {
	if err := os.Rename(temporary, filepath.Join(dir, target)); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDir(dir)
}

func atomicWrite(dir, name string, data []byte) error {
	temporary, file, err := createPayloadTemp(dir, name)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(dir, name)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	ok = true
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func verifyFile(ctx context.Context, path, expected string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		return err
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != expected {
		return fmt.Errorf("%w: staged payload digest differs", ErrInvalidOutbox)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func consumeOutstandingBytes(remaining, used int64) (int64, error) {
	if used < 0 || used > remaining {
		return 0, fmt.Errorf("%w: outstanding staged bytes", ErrLimitExceeded)
	}
	return remaining - used, nil
}

func boundedArchiveLimits(limits historyarchive.Limits, remaining int64) historyarchive.Limits {
	if remaining < limits.MaxStoredBytes {
		limits.MaxStoredBytes = remaining
	}
	return limits
}

func removeOrphanPayloads(dir string) error {
	return removeMatchingPayloads(dir, func(name string) bool {
		return isStagePayload(name) || isStagePayloadTemp(name)
	})
}

func removeStagePayloadTemps(dir string) error {
	return removeMatchingPayloads(dir, isStagePayloadTemp)
}

func removeMatchingPayloads(dir string, match func(string) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		if !match(entry.Name()) {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("%w: payload path is a directory", ErrInvalidOutbox)
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return syncDir(dir)
	}
	return nil
}

func isStagePayload(name string) bool {
	if name == workspaceFileName || name == harnessFileName {
		return true
	}
	const prefix, suffix = "transcript-", ".bin"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	sequence := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if len(sequence) != 12 {
		return false
	}
	for _, digit := range sequence {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func isStagePayloadTemp(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	name = strings.TrimPrefix(name, ".")
	const marker = ".tmp-"
	index := strings.LastIndex(name, marker)
	if index < 1 || !isStagePayload(name[:index]) {
		return false
	}
	suffix := name[index+len(marker):]
	if len(suffix) != 24 || strings.ToLower(suffix) != suffix {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}
