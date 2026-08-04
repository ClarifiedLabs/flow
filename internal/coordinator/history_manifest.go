package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const historyManifestSchemaVersion = 1

type canonicalHistoryManifest struct {
	Format        string                             `json:"format"`
	SchemaVersion int                                `json:"schema_version"`
	Capture       canonicalHistoryManifestCapture    `json:"capture"`
	Artifacts     []canonicalHistoryManifestArtifact `json:"artifacts"`
	Transcript    canonicalHistoryManifestTranscript `json:"transcript"`
	Harness       []canonicalHistoryManifestMember   `json:"harness_members"`
	Workspace     *canonicalHistoryManifestWorkspace `json:"workspace,omitempty"`
}

type canonicalHistoryManifestCapture struct {
	ID                          string                  `json:"id"`
	ProjectID                   string                  `json:"project_id"`
	JobID                       string                  `json:"job_id"`
	LeaseID                     string                  `json:"lease_id"`
	LeaseAttempt                int64                   `json:"lease_attempt"`
	WorkerID                    string                  `json:"worker_id"`
	TaskID                      string                  `json:"task_id,omitempty"`
	ChangeID                    string                  `json:"change_id,omitempty"`
	SessionID                   string                  `json:"session_id,omitempty"`
	WorkflowRunID               string                  `json:"workflow_run_id,omitempty"`
	NodeRunID                   string                  `json:"node_run_id,omitempty"`
	NodeVisit                   int64                   `json:"node_visit,omitempty"`
	Stage                       string                  `json:"stage,omitempty"`
	Role                        string                  `json:"role"`
	HarnessName                 string                  `json:"harness_name,omitempty"`
	HarnessVersion              string                  `json:"harness_version,omitempty"`
	HarnessSchemaVersion        int                     `json:"harness_schema_version,omitempty"`
	ResumedFromCaptureID        string                  `json:"resumed_from_capture_id,omitempty"`
	ResumedFromHarnessSessionID string                  `json:"resumed_from_harness_session_id,omitempty"`
	ExecutionVerdict            HistoryExecutionVerdict `json:"execution_verdict"`
	ExecutionExitCode           *int                    `json:"execution_exit_code,omitempty"`
	ExecutionErrorCode          string                  `json:"execution_error_code,omitempty"`
}

type canonicalHistoryManifestArtifact struct {
	LogicalKey           string               `json:"logical_key"`
	Kind                 HistoryArtifactKind  `json:"kind"`
	Phase                HistoryArtifactPhase `json:"phase"`
	CheckpointGeneration int64                `json:"checkpoint_generation,omitempty"`
	CheckpointStream     string               `json:"checkpoint_stream,omitempty"`
	CheckpointTrigger    string               `json:"checkpoint_trigger,omitempty"`
	ArchiveID            string               `json:"archive_id,omitempty"`
	MediaType            string               `json:"media_type"`
	FormatVersion        int                  `json:"format_version"`
	SchemaVersion        int                  `json:"schema_version"`
	SHA256               string               `json:"sha256"`
	StoredSize           int64                `json:"stored_size"`
	LogicalSize          int64                `json:"logical_size"`
	EntryCount           int64                `json:"entry_count"`
}

type canonicalHistoryManifestTranscript struct {
	FinalEpoch    int64  `json:"final_epoch"`
	SegmentCount  int64  `json:"segment_count"`
	LogicalLength int64  `json:"logical_length"`
	SHA256        string `json:"sha256"`
}

type canonicalHistoryManifestMember struct {
	ArchiveID             string `json:"archive_id"`
	NativeSessionID       string `json:"native_session_id,omitempty"`
	NativeParentSessionID string `json:"native_parent_session_id,omitempty"`
	RelativeMemberPath    string `json:"relative_member_path"`
	MemberKind            string `json:"member_kind"`
	AgentName             string `json:"agent_name,omitempty"`
	Status                string `json:"status,omitempty"`
	Model                 string `json:"model,omitempty"`
	HarnessBuild          string `json:"harness_build,omitempty"`
	ParseStatus           string `json:"parse_status"`
}

type canonicalHistoryManifestWorkspace struct {
	ArchiveSchemaVersion int    `json:"archive_schema_version"`
	Branch               string `json:"branch,omitempty"`
	Detached             bool   `json:"detached"`
	BaseRef              string `json:"base_ref,omitempty"`
	BaseCommit           string `json:"base_commit,omitempty"`
	HeadCommit           string `json:"head_commit"`
	StagedCount          int64  `json:"staged_count"`
	UnstagedCount        int64  `json:"unstaged_count"`
	UntrackedCount       int64  `json:"untracked_count"`
	InventoryDigest      string `json:"inventory_digest"`
	ValidationStatus     string `json:"validation_status"`
}

// GenerateCanonicalManifest constructs and publishes the one coordinator-owned
// final manifest. Producer-supplied bytes never enter this path.
func (s *HistoryCaptureService) GenerateCanonicalManifest(ctx context.Context, captureID string) (HistoryArtifact, error) {
	capture, err := s.Get(ctx, captureID)
	if err != nil {
		return HistoryArtifact{}, err
	}
	if existing, getErr := s.GetArtifact(ctx, capture.ID, "manifest/final"); getErr == nil {
		if existing.Kind != HistoryArtifactManifest || existing.Phase != HistoryArtifactFinal || existing.PublicationState != HistoryPublicationCommitted {
			return HistoryArtifact{}, fmt.Errorf("%w: canonical manifest logical key has incompatible metadata", ErrHistoryConflict)
		}
		return existing, nil
	} else if !errors.Is(getErr, ErrHistoryArtifactNotFound) {
		return HistoryArtifact{}, getErr
	}
	if capture.ExpectedSetDeclaredAt == nil {
		return HistoryArtifact{}, fmt.Errorf("%w: final expected set is not declared", ErrHistoryIncomplete)
	}
	readinessTx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return HistoryArtifact{}, err
	}
	if err := verifyCaptureCompletenessTx(ctx, readinessTx, capture, false); err != nil {
		readinessTx.Rollback()
		return HistoryArtifact{}, err
	}
	if _, err := readinessTx.ExecContext(ctx, `
INSERT INTO history_manifest_locks (capture_id, created_at)
VALUES (?, ?)
ON CONFLICT(capture_id) DO NOTHING`, capture.ID, sqlitex.FormatTime(s.now().UTC())); err != nil {
		readinessTx.Rollback()
		return HistoryArtifact{}, err
	}
	if err := readinessTx.Commit(ctx); err != nil {
		return HistoryArtifact{}, err
	}

	artifacts, err := s.ListArtifacts(ctx, capture.ID)
	if err != nil {
		return HistoryArtifact{}, err
	}
	members, err := s.ListHarnessArchiveMembers(ctx, capture.ID)
	if err != nil {
		return HistoryArtifact{}, err
	}
	manifest := canonicalHistoryManifest{
		Format: "flow-history-manifest", SchemaVersion: historyManifestSchemaVersion,
		Capture: canonicalHistoryManifestCapture{
			ID: capture.ID, ProjectID: capture.ProjectID, JobID: capture.JobID, LeaseID: capture.LeaseID,
			LeaseAttempt: capture.LeaseAttempt, WorkerID: capture.WorkerID, TaskID: capture.TaskID,
			ChangeID: capture.ChangeID, SessionID: capture.SessionID, WorkflowRunID: capture.WorkflowRunID,
			NodeRunID: capture.NodeRunID, NodeVisit: capture.NodeVisit, Stage: capture.Stage, Role: capture.Role,
			HarnessName: capture.HarnessName, HarnessVersion: capture.HarnessVersion,
			HarnessSchemaVersion: capture.HarnessSchemaVersion, ResumedFromCaptureID: capture.ResumedFromCaptureID,
			ResumedFromHarnessSessionID: capture.ResumedFromHarnessSessionID,
			ExecutionVerdict:            capture.ExecutionVerdict, ExecutionExitCode: capture.ExecutionExitCode,
			ExecutionErrorCode: capture.ExecutionErrorCode,
		},
		Artifacts: make([]canonicalHistoryManifestArtifact, 0, len(artifacts)),
		Harness:   make([]canonicalHistoryManifestMember, 0, len(members)),
	}
	if capture.ExpectedTranscriptSegmentCount == nil || capture.ExpectedTranscriptLength == nil || capture.ExpectedTranscriptSHA256 == "" {
		return HistoryArtifact{}, fmt.Errorf("%w: transcript expectation must be declared before manifest generation", ErrHistoryIncomplete)
	}
	finalEpoch := int64(-1)
	if capture.ExpectedTranscriptEpoch != nil {
		finalEpoch = *capture.ExpectedTranscriptEpoch
	}
	manifest.Transcript = canonicalHistoryManifestTranscript{
		FinalEpoch: finalEpoch, SegmentCount: *capture.ExpectedTranscriptSegmentCount,
		LogicalLength: *capture.ExpectedTranscriptLength, SHA256: capture.ExpectedTranscriptSHA256,
	}
	for _, artifact := range artifacts {
		if artifact.Kind == HistoryArtifactManifest {
			continue
		}
		if artifact.PublicationState != HistoryPublicationCommitted {
			return HistoryArtifact{}, ErrHistoryPublicationPending
		}
		manifest.Artifacts = append(manifest.Artifacts, canonicalHistoryManifestArtifact{
			LogicalKey: artifact.LogicalKey, Kind: artifact.Kind, Phase: artifact.Phase,
			CheckpointGeneration: artifact.CheckpointGeneration, CheckpointStream: artifact.CheckpointStream,
			CheckpointTrigger: artifact.CheckpointTrigger, ArchiveID: artifact.ArchiveID, MediaType: artifact.MediaType,
			FormatVersion: artifact.FormatVersion, SchemaVersion: artifact.SchemaVersion,
			SHA256: artifact.SHA256, StoredSize: artifact.StoredSize, LogicalSize: artifact.LogicalSize,
			EntryCount: artifact.EntryCount,
		})
		if artifact.Kind == HistoryArtifactWorkspaceSnapshot && artifact.Phase == HistoryArtifactFinal {
			summary, summaryErr := s.GetWorkspaceSummary(ctx, artifact.ID)
			if summaryErr != nil {
				if errors.Is(summaryErr, sql.ErrNoRows) {
					return HistoryArtifact{}, fmt.Errorf("%w: final workspace summary is missing", ErrHistoryIncomplete)
				}
				return HistoryArtifact{}, summaryErr
			}
			manifest.Workspace = &canonicalHistoryManifestWorkspace{
				ArchiveSchemaVersion: summary.ArchiveSchemaVersion, Branch: summary.Branch,
				Detached: summary.Detached, BaseRef: summary.BaseRef, BaseCommit: summary.BaseCommit,
				HeadCommit: summary.HeadCommit, StagedCount: summary.StagedCount,
				UnstagedCount: summary.UnstagedCount, UntrackedCount: summary.UntrackedCount,
				InventoryDigest: summary.InventoryDigest, ValidationStatus: summary.ValidationStatus,
			}
		}
	}
	for _, member := range members {
		manifest.Harness = append(manifest.Harness, canonicalHistoryManifestMember{
			ArchiveID: member.ArchiveID, NativeSessionID: member.NativeSessionID,
			NativeParentSessionID: member.NativeParentSessionID, RelativeMemberPath: member.RelativeMemberPath,
			MemberKind: member.MemberKind, AgentName: member.AgentName, Status: member.Status,
			Model: member.Model, HarnessBuild: member.HarnessBuild, ParseStatus: member.ParseStatus,
		})
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return HistoryArtifact{}, err
	}
	upload, err := s.BeginCoordinatorUpload(ctx, capture.ID)
	if err != nil {
		return HistoryArtifact{}, err
	}
	if _, err := upload.Write(body); err != nil {
		cleanupCtx, cleanupCancel := detachedHistoryUploadCleanupContext(ctx)
		_ = upload.Abort(cleanupCtx)
		cleanupCancel()
		return HistoryArtifact{}, err
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		cleanupCtx, cleanupCancel := detachedHistoryUploadCleanupContext(ctx)
		_ = upload.Abort(cleanupCtx)
		cleanupCancel()
		return HistoryArtifact{}, err
	}
	return s.PublishCanonicalManifest(ctx, capture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/vnd.flow.history-manifest+json", FormatVersion: 1,
		SchemaVersion: historyManifestSchemaVersion, LogicalSize: int64(len(body)),
		EntryCount: int64(len(manifest.Artifacts) + len(manifest.Harness)),
	}, temporary)
}
