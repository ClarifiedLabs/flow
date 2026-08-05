package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// HistoryResume is the durable, owner-requested link between one immutable
// source capture and the author job that restores it.
type HistoryResume struct {
	ID                       string
	SourceCaptureID          string
	SourceNativeSessionID    string
	RequestedNativeSessionID string
	HarnessArtifactID        string
	HarnessSHA256            string
	WorkspaceArtifactID      string
	WorkspaceSHA256          string
	TargetTaskID             string
	TargetChangeID           string
	JobID                    string
	RequiredHeadCommit       string
	SourceHarnessBuild       string
	RequiredHarnessSchema    int
	State                    string
	RequestedBy              string
	IdempotencyKey           string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

type CreateHistoryResumeInput struct {
	SourceCaptureID string
	NativeSessionID string
	IdempotencyKey  string
	RequestedBy     string
}

// CreateResume derives every restore coordinate from committed coordinator
// metadata. The owner selects only a native session already indexed inside the
// source capture and supplies an idempotency key.
func (s *HistoryCaptureService) CreateResume(ctx context.Context, queue *flowworker.Service, project Project, input CreateHistoryResumeInput) (HistoryResume, bool, error) {
	if queue == nil {
		return HistoryResume{}, false, errors.New("history resume queue is not configured")
	}
	input.SourceCaptureID = strings.TrimSpace(input.SourceCaptureID)
	input.NativeSessionID = strings.TrimSpace(input.NativeSessionID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.RequestedBy = strings.TrimSpace(input.RequestedBy)
	if input.SourceCaptureID == "" || input.IdempotencyKey == "" || input.RequestedBy == "" {
		return HistoryResume{}, false, errors.New("source capture, idempotency key, and requester are required")
	}
	if len(input.NativeSessionID) > 255 || len(input.IdempotencyKey) > 255 || len(input.RequestedBy) > 255 {
		return HistoryResume{}, false, errors.New("history resume metadata exceeds its storage bound")
	}
	if existing, err := s.getResumeByIdentity(ctx, input.SourceCaptureID, input.RequestedBy, input.IdempotencyKey); err == nil {
		if existing.RequestedNativeSessionID != input.NativeSessionID {
			return HistoryResume{}, false, fmt.Errorf("%w: resume retry uses a different native session selector", ErrHistoryConflict)
		}
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return HistoryResume{}, false, err
	}

	source, err := s.Get(ctx, input.SourceCaptureID)
	if err != nil {
		return HistoryResume{}, false, err
	}
	if source.ProjectID != project.ID || source.State != HistoryCaptureComplete || !source.ExpectedHarness ||
		source.HarnessName == "" || source.HarnessVersion == "" || source.HarnessSchemaVersion < 1 {
		return HistoryResume{}, false, fmt.Errorf("%w: source capture is not a completed native Harness capture", ErrHistoryConflict)
	}
	if source.TaskID == "" || source.ChangeID == "" {
		return HistoryResume{}, false, fmt.Errorf("%w: source capture has no task/change target", ErrHistoryConflict)
	}

	artifacts, err := s.ListArtifacts(ctx, source.ID)
	if err != nil {
		return HistoryResume{}, false, err
	}
	var harnessArtifact, workspaceArtifact HistoryArtifact
	var harnessCount, workspaceCount int
	for _, artifact := range artifacts {
		if artifact.Phase != HistoryArtifactFinal || artifact.PublicationState != HistoryPublicationCommitted {
			continue
		}
		switch artifact.Kind {
		case HistoryArtifactHarnessRoot:
			harnessArtifact, harnessCount = artifact, harnessCount+1
		case HistoryArtifactWorkspaceSnapshot:
			workspaceArtifact, workspaceCount = artifact, workspaceCount+1
		}
	}
	if harnessCount != 1 || workspaceCount != 1 {
		return HistoryResume{}, false, fmt.Errorf("%w: resume requires exactly one committed Harness and workspace artifact", ErrHistoryIncomplete)
	}
	workspace, err := s.GetWorkspaceSummary(ctx, workspaceArtifact.ID)
	if err != nil || workspace.ValidationStatus != "valid" || workspace.HeadCommit == "" {
		if err != nil {
			return HistoryResume{}, false, err
		}
		return HistoryResume{}, false, fmt.Errorf("%w: resume workspace metadata is not valid", ErrHistoryIncomplete)
	}

	members, err := s.ListHarnessArchiveMembers(ctx, source.ID)
	if err != nil {
		return HistoryResume{}, false, err
	}
	var selected HarnessArchiveMember
	matches := 0
	for _, member := range members {
		if member.ArtifactID != harnessArtifact.ID || member.ParseStatus != "parsed" || member.HarnessBuild != source.HarnessVersion {
			continue
		}
		if input.NativeSessionID == "" {
			if member.MemberKind != "root" {
				continue
			}
		} else if member.NativeSessionID != input.NativeSessionID {
			continue
		}
		selected, matches = member, matches+1
	}
	if matches != 1 || selected.NativeSessionID == "" {
		return HistoryResume{}, false, fmt.Errorf("%w: selected native session is not a unique parsed member of the source archive", ErrHistoryConflict)
	}

	sourceJob, err := queue.GetJob(ctx, source.JobID)
	if err != nil {
		return HistoryResume{}, false, fmt.Errorf("load source history job: %w", err)
	}
	entrypoint, ok := sourceJob.Payload["entrypoint"].(map[string]any)
	if !ok || len(entrypoint) == 0 {
		return HistoryResume{}, false, fmt.Errorf("%w: source job has no reusable managed entrypoint", ErrHistoryConflict)
	}
	branch, _ := sourceJob.Payload["branch"].(string)
	base, _ := sourceJob.Payload["base"].(string)
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if branch == "" || base == "" {
		return HistoryResume{}, false, fmt.Errorf("%w: source job has no branch/base coordinates", ErrHistoryConflict)
	}
	var currentHead string
	if err := s.db.QueryRowContext(ctx, `SELECT head_sha FROM changes WHERE id = ? AND task_id = ?`, source.ChangeID, source.TaskID).Scan(&currentHead); err != nil {
		return HistoryResume{}, false, fmt.Errorf("load resume change: %w", err)
	}
	if strings.TrimSpace(currentHead) != workspace.HeadCommit {
		return HistoryResume{}, false, fmt.Errorf("%w: current change head differs from the captured workspace head", ErrHistoryConflict)
	}

	resumeID := deterministicHistoryResumeID(source.ID, input.RequestedBy, input.IdempotencyKey)
	relativeSessionDir := path.Dir(selected.RelativeMemberPath)
	if relativeSessionDir == "." {
		relativeSessionDir = ""
	}
	payload := copyPayload(sourceJob.Payload)
	delete(payload, "session_id")
	delete(payload, "workflow_run_id")
	delete(payload, "node_run_id")
	payload["entrypoint"] = copyPayload(entrypoint)
	payload["branch"] = branch
	payload["base"] = base
	payload["change_id"] = source.ChangeID
	payload["head_sha"] = workspace.HeadCommit
	payload["agent_harness"] = source.HarnessName
	payload["inject_initial_prompt"] = false
	payload["resumed_from_capture_id"] = source.ID
	payload["resumed_from_harness_session_id"] = selected.NativeSessionID
	payload["history_resume"] = map[string]any{
		"id":                              resumeID,
		"source_capture_id":               source.ID,
		"native_session_id":               selected.NativeSessionID,
		"session_relative_dir":            relativeSessionDir,
		"harness_artifact_id":             harnessArtifact.ID,
		"harness_sha256":                  harnessArtifact.SHA256,
		"workspace_artifact_id":           workspaceArtifact.ID,
		"workspace_sha256":                workspaceArtifact.SHA256,
		"required_head_commit":            workspace.HeadCommit,
		"source_harness_build":            source.HarnessVersion,
		"required_harness_schema_version": source.HarnessSchemaVersion,
	}
	stampProjectPayload(payload, project)
	dispatchDigest := sha256.Sum256([]byte(source.ID + "\x00" + input.RequestedBy + "\x00" + input.IdempotencyKey))
	dispatchKey := "history-resume:" + hex.EncodeToString(dispatchDigest[:])
	jobID := "j-" + hex.EncodeToString(dispatchDigest[:8])
	compiledSelector, err := scheduler.CompileSelector(scheduler.SelectorInput{RunsOn: map[string]string{
		flowharness.AgentHarnessLabel(source.HarnessName): "true",
	}})
	if err != nil {
		return HistoryResume{}, false, fmt.Errorf("compile history resume selector: %w", err)
	}
	selectorJSON, err := json.Marshal(compiledSelector.Requirements())
	if err != nil {
		return HistoryResume{}, false, fmt.Errorf("encode history resume selector: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return HistoryResume{}, false, fmt.Errorf("encode history resume payload: %w", err)
	}

	now := s.now().UTC()
	nowText := sqlitex.FormatTime(now)
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryResume{}, false, err
	}
	defer tx.Rollback()
	if existing, lookupErr := scanHistoryResume(tx.QueryRowContext(ctx, historyResumeSelect+`
WHERE source_capture_id = ? AND requested_by = ? AND idempotency_key = ?`, source.ID, input.RequestedBy, input.IdempotencyKey)); lookupErr == nil {
		if existing.RequestedNativeSessionID != input.NativeSessionID {
			return HistoryResume{}, false, fmt.Errorf("%w: resume retry uses a different native session selector", ErrHistoryConflict)
		}
		return existing, false, nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return HistoryResume{}, false, lookupErr
	}
	jobResult, err := tx.ExecContext(ctx, `
INSERT INTO jobs (
    id, task_id, change_id, role, state, capacity_bucket, priority,
    selector_json, required_harness, required_model, tolerations_json, payload_json, dispatch_key, created_at, updated_at
)
SELECT ?, ?, ?, 'author', 'queued', 'persistent_agent', ?, ?, ?, ?, '[]', ?, ?, ?, ?
WHERE EXISTS (
    SELECT 1
    FROM changes AS c
    JOIN tasks AS t ON t.id = c.task_id
    WHERE c.id = ? AND c.task_id = ? AND c.head_sha = ? AND t.done_at IS NULL
)`, jobID, source.TaskID, source.ChangeID, sourceJob.Priority, string(selectorJSON), source.HarnessName, sourceJob.Harness.Model, string(payloadJSON),
		dispatchKey, nowText, nowText, source.ChangeID, source.TaskID, workspace.HeadCommit)
	if err != nil {
		return HistoryResume{}, false, fmt.Errorf("enqueue history resume: %w", err)
	}
	if affected, rowsErr := jobResult.RowsAffected(); rowsErr != nil {
		return HistoryResume{}, false, rowsErr
	} else if affected != 1 {
		return HistoryResume{}, false, fmt.Errorf("%w: resume target changed before enqueue", ErrHistoryConflict)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO history_resumes (
    id, source_capture_id, source_native_session_id, requested_native_session_id,
    source_harness_artifact_id, source_harness_sha256, source_workspace_artifact_id, source_workspace_sha256,
    target_task_id, target_change_id, job_id, required_head_commit,
    required_harness_build, required_harness_schema_version, state,
    requested_by, idempotency_key, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?)`,
		resumeID, source.ID, selected.NativeSessionID, input.NativeSessionID, harnessArtifact.ID, harnessArtifact.SHA256,
		workspaceArtifact.ID, workspaceArtifact.SHA256, source.TaskID, source.ChangeID, jobID,
		workspace.HeadCommit, source.HarnessVersion, source.HarnessSchemaVersion,
		input.RequestedBy, input.IdempotencyKey, nowText, nowText); err != nil {
		return HistoryResume{}, false, err
	}
	if err := appendHistoryCaptureEvent(ctx, tx, source.ID, "resume_requested", string(source.State), string(source.State), source.Version,
		input.RequestedBy, "", map[string]any{"resume_id": resumeID, "job_id": jobID, "native_session_id": selected.NativeSessionID}, now); err != nil {
		return HistoryResume{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoryResume{}, false, err
	}
	resume, err := s.GetResume(ctx, resumeID)
	return resume, true, err
}

func deterministicHistoryResumeID(captureID, requestedBy, key string) string {
	digest := sha256.Sum256([]byte(captureID + "\x00" + requestedBy + "\x00" + key))
	return "hr-" + hex.EncodeToString(digest[:16])
}

func (s *HistoryCaptureService) GetResume(ctx context.Context, id string) (HistoryResume, error) {
	return scanHistoryResume(s.db.QueryRowContext(ctx, historyResumeSelect+` WHERE id = ?`, strings.TrimSpace(id)))
}

func (s *HistoryCaptureService) getResumeByIdentity(ctx context.Context, captureID, requestedBy, key string) (HistoryResume, error) {
	return scanHistoryResume(s.db.QueryRowContext(ctx, historyResumeSelect+`
WHERE source_capture_id = ? AND requested_by = ? AND idempotency_key = ?`, captureID, requestedBy, key))
}

func (s *HistoryCaptureService) ResumeLineageForJob(ctx context.Context, jobID string) (HistoryResume, error) {
	return scanHistoryResume(s.db.QueryRowContext(ctx, historyResumeSelect+` WHERE job_id = ?`, strings.TrimSpace(jobID)))
}

const historyResumeSelect = `
SELECT id, source_capture_id, source_native_session_id, requested_native_session_id,
       source_harness_artifact_id, source_harness_sha256, source_workspace_artifact_id, source_workspace_sha256,
       target_task_id, target_change_id, job_id, required_head_commit,
       required_harness_build, required_harness_schema_version, state,
       requested_by, idempotency_key, created_at, updated_at
FROM history_resumes`

func scanHistoryResume(row historyRowScanner) (HistoryResume, error) {
	var value HistoryResume
	var created, updated string
	if err := row.Scan(&value.ID, &value.SourceCaptureID, &value.SourceNativeSessionID,
		&value.RequestedNativeSessionID, &value.HarnessArtifactID, &value.HarnessSHA256, &value.WorkspaceArtifactID,
		&value.WorkspaceSHA256, &value.TargetTaskID, &value.TargetChangeID, &value.JobID,
		&value.RequiredHeadCommit, &value.SourceHarnessBuild, &value.RequiredHarnessSchema,
		&value.State, &value.RequestedBy, &value.IdempotencyKey, &created, &updated); err != nil {
		return HistoryResume{}, err
	}
	var err error
	if value.CreatedAt, err = sqlitex.ParseTime(created); err != nil {
		return HistoryResume{}, err
	}
	if value.UpdatedAt, err = sqlitex.ParseTime(updated); err != nil {
		return HistoryResume{}, err
	}
	return value, nil
}
