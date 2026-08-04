package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
	"github.com/ClarifiedLabs/flow/internal/worker/historycapture"
)

const (
	historyShutdownBudget          = 30 * time.Second
	historyCompletedRetentionCount = 1024
	historyCompletedRetentionAge   = 30 * 24 * time.Hour
)

type historyCaptureManager struct {
	outbox                  *historycapture.Outbox
	client                  *flowclient.Client
	cfg                     config.WorkerConfig
	policy                  config.ResolvedWorkerHistory
	replayInterval          time.Duration
	recoverySensitiveValues [][]byte
	ready                   atomic.Bool
	lifecycleMu             sync.Mutex
	activeMu                sync.RWMutex
	active                  map[string]struct{}
}

func newHistoryCaptureManager(client *flowclient.Client, cfg config.WorkerConfig, policy config.ResolvedWorkerHistory) (*historyCaptureManager, error) {
	if client == nil {
		return nil, errors.New("history capture client is required")
	}
	outboxOptions := historycapture.OptionsFromConfig(policy.OutboxPath, policy.Transcript, policy.Archive)
	outboxOptions.SensitiveDataKey = []byte(strings.TrimSpace(cfg.Token))
	outbox, err := historycapture.New(outboxOptions)
	if err != nil {
		return nil, err
	}
	recoverySensitiveValues := make([][]byte, 0, 2)
	for _, value := range []string{cfg.Token, os.Getenv("HARNESS_MODEL_PROXY_API_KEY")} {
		if value = strings.TrimSpace(value); value != "" {
			recoverySensitiveValues = append(recoverySensitiveValues, []byte(value))
		}
	}
	manager := &historyCaptureManager{outbox: outbox, client: client, cfg: cfg, policy: policy, replayInterval: policy.ReplayInterval, recoverySensitiveValues: recoverySensitiveValues, active: map[string]struct{}{}}
	return manager, nil
}

func (m *historyCaptureManager) CanClaim() bool {
	return m != nil && m.ready.Load()
}

func (m *historyCaptureManager) ProtectedJobIDs() (map[string]struct{}, error) {
	entries, err := m.outbox.ListPending(context.Background())
	if err != nil {
		return nil, err
	}
	protected := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		protected[entry.Capture.JobID] = struct{}{}
	}
	return protected, nil
}

func (m *historyCaptureManager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(m.replayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Replay(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("worker history outbox replay failed", "error", err)
			}
		}
	}
}

// Replay first converts crash-left reservations into immutable crashed stages,
// then resumes their idempotent protocol publication.
func (m *historyCaptureManager) Replay(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.replayLocked(ctx)
}

func (m *historyCaptureManager) replayLocked(ctx context.Context) error {
	m.ready.Store(false)
	entries, err := m.outbox.ListPending(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if m.isActive(entry.Capture.ID) || entry.Status == "staged" || entry.Status == "complete" {
			continue
		}
		workerexec.QuiesceJobHistorySources(m.cfg, entry.Capture.JobID)
		var recoveryErr error
		if entry.Status == "reserved" {
			exitCode := -1
			_, recoveryErr = m.outbox.RecordFinal(ctx, entry.Capture.ID, historycapture.Final{Verdict: "crashed", ExitCode: &exitCode, ErrorCode: "worker_restarted"})
		}
		if recoveryErr == nil {
			recoveryErr = m.outbox.RecordVerdict(ctx, m.client, entry.Capture.ID)
		}
		if recoveryErr == nil {
			entry, recoveryErr = m.outbox.Get(ctx, entry.Capture.ID)
		}
		var sources historycapture.SourcePaths
		if recoveryErr == nil {
			sources, recoveryErr = m.recoverSources(ctx, entry)
		}
		if recoveryErr == nil {
			_, recoveryErr = m.outbox.UpdateSources(ctx, entry.Capture.ID, sources)
		}
		if recoveryErr == nil {
			recoveryErr = m.validateSourcesForUse(entry.Capture, sources)
		}
		if recoveryErr == nil {
			final := historycapture.Final{Verdict: entry.Final.Verdict, ExitCode: entry.Final.ExitCode, ErrorCode: entry.Final.ErrorCode, WorkspaceBaseRef: entry.Final.WorkspaceBaseRef, SensitiveValues: m.recoverySensitiveValues}
			_, recoveryErr = m.outbox.Stage(ctx, entry.Capture.ID, final)
		}
		if recoveryErr != nil {
			failures = append(failures, fmt.Errorf("stage recovered history capture %s: %w", entry.Capture.ID, recoveryErr))
		}
	}
	entries, err = m.outbox.ListPending(ctx)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, entry := range entries {
			if m.isActive(entry.Capture.ID) || entry.Status != "staged" {
				continue
			}
			if publishErr := m.outbox.Publish(ctx, m.client, entry.Capture.ID); publishErr != nil {
				failures = append(failures, fmt.Errorf("publish history capture %s: %w", entry.Capture.ID, publishErr))
			}
		}
	}
	entries, err = m.outbox.ListPending(ctx)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, entry := range entries {
			if m.isActive(entry.Capture.ID) || entry.Status != "complete" || entry.Terminal == nil {
				continue
			}
			if terminalErr := m.reconcileTerminal(ctx, entry); terminalErr != nil {
				failures = append(failures, fmt.Errorf("reconcile terminal lifecycle for capture %s: %w", entry.Capture.ID, terminalErr))
				continue
			}
			if terminalErr := m.outbox.AcknowledgeTerminal(ctx, entry.Capture.ID); terminalErr != nil {
				failures = append(failures, fmt.Errorf("acknowledge terminal lifecycle for capture %s: %w", entry.Capture.ID, terminalErr))
			}
		}
	}
	if joined := errors.Join(failures...); joined != nil {
		return joined
	}
	if _, err := m.outbox.PruneCompleted(ctx, historycapture.CompletedRetention{
		MaxEntries:      historyCompletedRetentionCount,
		CompletedBefore: time.Now().UTC().Add(-historyCompletedRetentionAge),
	}); err != nil {
		return fmt.Errorf("prune completed history outbox tombstones: %w", err)
	}
	m.ready.Store(true)
	return nil
}

func (m *historyCaptureManager) Reserve(ctx context.Context, job flowworker.Job, lease flowworker.Lease, session *coordinator.Session, prep workerexec.ExecutionPreparation, sensitiveValues [][]byte) (string, error) {
	if m == nil {
		return "", errors.New("history capture manager is required")
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	harnessName := strings.TrimSpace(prep.HarnessName)
	harnessVersion := ""
	harnessSchema := 0
	if harnessName != "" && harnessName != flowharness.Agents && harnessName != flowharness.Shell {
		var err error
		harnessVersion, err = flowharness.BuildVersion(ctx, harnessName)
		if err != nil {
			return "", err
		}
		harnessSchema = historyarchive.SupportedHarnessNativeSchema
	} else {
		harnessName = ""
	}
	request := contract.ReserveHistoryCaptureRequest{
		JobID:                job.ID,
		LeaseID:              lease.ID,
		HarnessName:          harnessName,
		HarnessVersion:       harnessVersion,
		HarnessSchemaVersion: harnessSchema,
		ExpectedTranscript:   true,
		ExpectedHarness:      harnessName != "",
	}
	if session != nil {
		request.SessionID = session.ID
	}
	response, err := m.client.ReserveHistoryCapture(ctx, request)
	if err != nil {
		return "", err
	}
	sources := historycapture.SourcePaths{Worktree: prep.Worktree, Transcript: prep.TranscriptPath, NativeSessionRoot: prep.NativeSessionRoot}
	if err := m.validateSources(response.Capture, sources); err != nil {
		return "", err
	}
	_, err = m.outbox.RecordReservation(ctx, historycapture.Reservation{
		Response:        response,
		Sources:         sources,
		SensitiveValues: m.captureSensitiveValues(sensitiveValues),
	})
	if err != nil {
		return "", err
	}
	if _, err := m.outbox.Start(ctx, m.client, response.Capture.ID); err != nil {
		return "", err
	}
	m.activeMu.Lock()
	m.active[response.Capture.ID] = struct{}{}
	m.activeMu.Unlock()
	m.ready.Store(false)
	return response.Capture.ID, nil
}

func (m *historyCaptureManager) Finalize(ctx context.Context, captureID string, result workerexec.RunResult, hostInterrupted bool, terminal *historycapture.TerminalAction, sensitiveValues [][]byte, stdout io.Writer) error {
	if m == nil || captureID == "" {
		return errors.New("history capture was not reserved")
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	entry, err := m.outbox.Get(ctx, captureID)
	if err != nil {
		return err
	}
	if entry.Status == "complete" {
		return nil
	}
	verdict := "failed"
	if hostInterrupted {
		verdict = "crashed"
	} else if result.FinalState == flowworker.JobFinished && result.Err == nil && result.ExitCode == 0 {
		verdict = "succeeded"
	} else if result.FinalState == flowworker.JobCanceled {
		verdict = "cancelled"
	}
	exitCode := result.ExitCode
	if hostInterrupted {
		terminal = nil
	}
	final := historycapture.Final{Verdict: verdict, ExitCode: &exitCode, Terminal: terminal, SensitiveValues: m.captureSensitiveValues(sensitiveValues)}
	if result.Payload.Base != "" && result.Worktree != "" && workspaceArchivable(ctx, result.Worktree) {
		refSources := entry.Sources
		refSources.Worktree = result.Worktree
		if err := m.validateSources(entry.Capture, refSources); err != nil {
			return err
		}
		if err := m.validateSourceForUse(result.Worktree, "workspace", historySourceDirectory); err != nil {
			return err
		}
		if workspaceRefExists(ctx, result.Worktree, result.Payload.Base) {
			final.WorkspaceBaseRef = result.Payload.Base
		}
	}
	if _, err := m.outbox.RecordFinal(ctx, captureID, final); err != nil {
		return err
	}
	if err := m.outbox.RecordVerdict(ctx, m.client, captureID); err != nil {
		return err
	}
	sources, err := m.recoverSources(ctx, entry)
	if err != nil {
		return err
	}
	if result.Worktree != "" && workspaceArchivable(ctx, result.Worktree) {
		resultSources := sources
		resultSources.Worktree = result.Worktree
		if err := m.validateSources(entry.Capture, resultSources); err != nil {
			return err
		}
		if err := m.validateSourceForUse(result.Worktree, "workspace", historySourceDirectory); err != nil {
			return err
		}
		sources.Worktree = result.Worktree
	}
	if result.TranscriptPath != "" {
		sources.Transcript = result.TranscriptPath
	}
	if err := m.validateSources(entry.Capture, sources); err != nil {
		return err
	}
	if _, err := m.outbox.UpdateSources(ctx, captureID, sources); err != nil {
		return err
	}
	if err := m.validateSourcesForUse(entry.Capture, sources); err != nil {
		return err
	}
	if _, err := m.outbox.Stage(ctx, captureID, final); err != nil {
		return err
	}
	if err := retryFinalHistoryPublication(ctx, stdout, func() error {
		return m.outbox.Publish(ctx, m.client, captureID)
	}); err != nil {
		return err
	}
	// Lifecycle reconciliation deliberately remains with the live job path. The
	// detached capture context may outlive host cancellation or authoritative
	// lease loss, so replaying the terminal action here could apply a stale result.
	return nil
}

func (m *historyCaptureManager) captureSensitiveValues(values [][]byte) [][]byte {
	merged := make([][]byte, 0, len(m.recoverySensitiveValues)+len(values))
	merged = append(merged, m.recoverySensitiveValues...)
	merged = append(merged, values...)
	return merged
}

func retryFinalHistoryPublication(ctx context.Context, stdout io.Writer, publish func() error) error {
	return retryTransientOperationContext(ctx, "publish final history capture", stdout, publish)
}

func (m *historyCaptureManager) discardTerminal(ctx context.Context, captureID string) error {
	if m == nil || captureID == "" {
		return nil
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if err := m.outbox.DiscardTerminal(ctx, captureID); err != nil {
		return err
	}
	m.deactivate(captureID)
	return nil
}

func (m *historyCaptureManager) acknowledgeTerminal(ctx context.Context, captureID string) error {
	if m == nil || captureID == "" {
		return nil
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if err := m.outbox.AcknowledgeTerminal(ctx, captureID); err != nil {
		return err
	}
	m.deactivate(captureID)
	return nil
}

func (m *historyCaptureManager) deactivate(captureID string) {
	if m == nil || captureID == "" {
		return
	}
	m.activeMu.Lock()
	delete(m.active, captureID)
	m.activeMu.Unlock()
}

func (m *historyCaptureManager) reconcileTerminal(ctx context.Context, entry historycapture.Entry) error {
	if entry.Terminal == nil {
		return nil
	}
	action, err := m.outbox.ResolveTerminal(ctx, entry.Capture.ID)
	if err != nil {
		return fmt.Errorf("resolve terminal action: %w", err)
	}
	return m.reconcileTerminalAction(ctx, entry, action)
}

func (m *historyCaptureManager) reconcileTerminalAction(ctx context.Context, entry historycapture.Entry, action *historycapture.TerminalAction) error {
	if action == nil {
		return nil
	}
	// A canceled console job may still be holding the owner's process-exit
	// fence, so job/lease finality alone cannot acknowledge that action.
	if action.Kind != historycapture.TerminalConsoleExit {
		finalized, err := m.terminalAlreadyFinalized(ctx, entry)
		if err != nil {
			return err
		}
		if finalized {
			return nil
		}
	}
	switch action.Kind {
	case historycapture.TerminalReleaseLease:
		released, err := m.client.ReleaseLeaseContext(ctx, flowclient.ReleaseLeaseInput{LeaseID: entry.Capture.LeaseID, FinalState: flowworker.JobState(action.FinalState)})
		if err == nil && released.ID != entry.Capture.JobID {
			err = errors.New("lease release acknowledged a different job")
		}
		if err == nil {
			return nil
		}
		if finalized, statusErr := m.terminalAlreadyFinalized(ctx, entry); statusErr == nil && finalized {
			return nil
		}
		return err
	case historycapture.TerminalSessionExit:
		_, err := m.client.ReportSessionProcessExit(ctx, flowclient.ReportSessionProcessExitInput{
			SessionID: action.SessionID, LeaseID: entry.Capture.LeaseID, ExitCode: action.ExitCode,
		})
		if err == nil {
			return nil
		}
		if finalized, statusErr := m.terminalAlreadyFinalized(ctx, entry); statusErr == nil && finalized {
			return nil
		}
		return err
	case historycapture.TerminalConsoleExit:
		// A live console must be released with its session token. If an owner
		// already stopped it, that token has been revoked and the worker must
		// instead clear the canceled job's process-exit fence. Both coordinator
		// operations are idempotent after the session reaches a terminal state.
		if _, err := m.client.ReportSessionProcessExit(ctx, flowclient.ReportSessionProcessExitInput{
			SessionID: action.SessionID, LeaseID: entry.Capture.LeaseID, ExitCode: action.ExitCode,
		}); err == nil {
			return nil
		}
		consoleClient, err := flowclient.New(config.ClientConfig{ServerURL: m.cfg.CoordinatorURL, Token: action.SessionToken})
		if err != nil {
			return err
		}
		return consoleClient.ReleaseConsole(ctx)
	default:
		return errors.New("unknown terminal lifecycle action")
	}
}

func (m *historyCaptureManager) terminalAlreadyFinalized(ctx context.Context, entry historycapture.Entry) (bool, error) {
	status, err := m.client.WorkerJobStatus(ctx, flowclient.WorkerJobStatusInput{LeaseID: entry.Capture.LeaseID})
	if err != nil {
		return false, err
	}
	if status.Job.ID != entry.Capture.JobID || status.Lease.ID != entry.Capture.LeaseID {
		return false, errors.New("terminal lifecycle status differs from captured job and lease")
	}
	return status.Lease.ReleasedAt != nil || flowworker.IsTerminalJobState(status.Job.State), nil
}

func (m *historyCaptureManager) isActive(captureID string) bool {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	_, ok := m.active[captureID]
	return ok
}

func (m *historyCaptureManager) recoverSources(ctx context.Context, entry historycapture.Entry) (historycapture.SourcePaths, error) {
	sources := entry.Sources
	if err := m.validateSources(entry.Capture, sources); err != nil {
		return historycapture.SourcePaths{}, err
	}
	if err := ensureRegularFile(sources.Transcript); err != nil {
		return historycapture.SourcePaths{}, err
	}
	if err := m.validateSources(entry.Capture, sources); err != nil {
		return historycapture.SourcePaths{}, err
	}
	if err := m.validateSourceForUse(sources.Transcript, "transcript", historySourceRegularFile); err != nil {
		return historycapture.SourcePaths{}, err
	}
	if workspaceArchivable(ctx, sources.Worktree) {
		if err := m.validateSourcesForUse(entry.Capture, sources); err != nil {
			return historycapture.SourcePaths{}, err
		}
		return sources, nil
	}
	fallback := filepath.Join(filepath.Dir(sources.Transcript), "fallback-workspace")
	sources.Worktree = fallback
	if err := m.createFallbackWorkspace(ctx, entry.Capture, sources); err != nil {
		return historycapture.SourcePaths{}, err
	}
	if err := m.validateSourcesForUse(entry.Capture, sources); err != nil {
		return historycapture.SourcePaths{}, err
	}
	return sources, nil
}

type historySourceKind uint8

const (
	historySourceDirectory historySourceKind = iota
	historySourceRegularFile
)

func (m *historyCaptureManager) validateSources(capture contract.HistoryCapture, sources historycapture.SourcePaths) error {
	expectedRoot := workerexec.HistoryAttemptDir(m.cfg.WorkDir, capture.JobID, capture.LeaseID)
	expectedWorktree := workerexec.HistoryWorktreePath(m.cfg.WorkDir, capture.JobID)
	fallbackWorktree := filepath.Join(expectedRoot, "fallback-workspace")
	worktree, err := filepath.Abs(sources.Worktree)
	if err != nil {
		return err
	}
	expectedWorktree, err = filepath.Abs(expectedWorktree)
	if err != nil {
		return err
	}
	fallbackWorktree, err = filepath.Abs(fallbackWorktree)
	if err != nil {
		return err
	}
	if worktree != expectedWorktree && worktree != fallbackWorktree {
		return fmt.Errorf("history capture %s has worktree outside its Flow-owned job roots", capture.ID)
	}
	expectedTranscript, err := filepath.Abs(filepath.Join(expectedRoot, "transcript.log"))
	if err != nil {
		return err
	}
	transcript, err := filepath.Abs(sources.Transcript)
	if err != nil {
		return err
	}
	if transcript != expectedTranscript {
		return fmt.Errorf("history capture %s has transcript outside its Flow-owned attempt root", capture.ID)
	}
	if capture.ExpectedHarness {
		expectedNativeRoot, err := filepath.Abs(filepath.Join(expectedRoot, "harness-session"))
		if err != nil {
			return err
		}
		nativeRoot, err := filepath.Abs(sources.NativeSessionRoot)
		if err != nil {
			return err
		}
		if nativeRoot != expectedNativeRoot {
			return fmt.Errorf("history capture %s has Harness source outside its Flow-owned attempt root", capture.ID)
		}
	} else if sources.NativeSessionRoot != "" || sources.NativeSessionID != "" {
		return fmt.Errorf("history capture %s has unexpected Harness sources", capture.ID)
	}
	if err := m.validateSourcePath(worktree, "workspace", historySourceDirectory, false); err != nil {
		return fmt.Errorf("history capture %s: %w", capture.ID, err)
	}
	if err := m.validateSourcePath(transcript, "transcript", historySourceRegularFile, false); err != nil {
		return fmt.Errorf("history capture %s: %w", capture.ID, err)
	}
	if capture.ExpectedHarness {
		if err := m.validateSourcePath(sources.NativeSessionRoot, "Harness source", historySourceDirectory, false); err != nil {
			return fmt.Errorf("history capture %s: %w", capture.ID, err)
		}
	}
	return nil
}

func (m *historyCaptureManager) validateSourcesForUse(capture contract.HistoryCapture, sources historycapture.SourcePaths) error {
	if err := m.validateSources(capture, sources); err != nil {
		return err
	}
	if err := m.validateSourceForUse(sources.Transcript, "transcript", historySourceRegularFile); err != nil {
		return err
	}
	if err := m.validateSourceForUse(sources.Worktree, "workspace", historySourceDirectory); err != nil {
		return err
	}
	if capture.ExpectedHarness {
		if err := m.validateSourceForUse(sources.NativeSessionRoot, "Harness source", historySourceDirectory); err != nil {
			return err
		}
	}
	return nil
}

func (m *historyCaptureManager) validateSourceForUse(path, label string, kind historySourceKind) error {
	return m.validateSourcePath(path, label, kind, true)
}

func (m *historyCaptureManager) validateSourcePath(path, label string, kind historySourceKind, requireLeaf bool) error {
	root, err := filepath.Abs(m.cfg.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve worker root: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve history %s: %w", label, err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("history %s is outside the worker root", label)
	}
	components := []string{root}
	if relative != "." {
		current := root
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			components = append(components, current)
		}
	}
	for index, component := range components {
		info, err := os.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if requireLeaf {
				return fmt.Errorf("history %s does not exist", label)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect history %s path: %w", label, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("history %s path traverses symlink %s", label, component)
		}
		leaf := index == len(components)-1
		if !leaf || kind == historySourceDirectory {
			if !info.IsDir() {
				return fmt.Errorf("history %s parent %s is not a directory", label, component)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("history %s is not a regular file", label)
		}
	}
	return nil
}

func ensureRegularFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("history transcript path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return createErr
		}
		return file.Close()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("history transcript is not a regular file")
	}
	return nil
}

func workspaceRefExists(ctx context.Context, path, ref string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(ref) == "" {
		return false
	}
	command := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--verify", ref+"^{commit}")
	return command.Run() == nil
}

func workspaceArchivable(ctx context.Context, path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	command := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--verify", "HEAD")
	return command.Run() == nil
}

func (m *historyCaptureManager) createFallbackWorkspace(ctx context.Context, capture contract.HistoryCapture, sources historycapture.SourcePaths) error {
	if err := m.validateSources(capture, sources); err != nil {
		return err
	}
	path := sources.Worktree
	if workspaceArchivable(ctx, path) {
		return m.validateSourceForUse(path, "fallback workspace", historySourceDirectory)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	commands := [][]string{
		{"init", "-q"},
		{"config", "user.name", "Flow History"},
		{"config", "user.email", "history@flow.invalid"},
		{"commit", "--allow-empty", "-qm", "Capture unavailable workspace"},
		{"branch", "-M", "main"},
	}
	for _, args := range commands {
		if err := m.validateSourceForUse(path, "fallback workspace", historySourceDirectory); err != nil {
			return err
		}
		command := exec.CommandContext(ctx, "git", append([]string{"-C", path}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("prepare fallback history workspace: %s: %w", strings.TrimSpace(string(output)), err)
		}
	}
	return nil
}
