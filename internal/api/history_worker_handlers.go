package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

const (
	historyUploadGrantHeader = "Flow-History-Upload-Grant"
	historyResumeJobHeader   = "Flow-History-Resume-Job"
	historyResumeLeaseHeader = "Flow-History-Resume-Lease"
)

func (s *Server) handleWorkerHistoryPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireScope(w, principal, "worker token is required", coordinator.TokenScopeWorker) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/history/captures"), "/")
	if rest == "" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleWorkerReserveHistoryCapture(w, r, principal)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 4 && parts[1] == "artifacts" && parts[3] == "resume-content" {
		s.handleWorkerHistoryResumeArtifact(w, r, principal, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[2]))
		return
	}
	if (len(parts) != 2 && len(parts) != 3) || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	project, capture, ok := s.bundleForHistoryCapture(w, r, principal, strings.TrimSpace(parts[0]))
	if !ok {
		return
	}
	if capture.WorkerID != strings.TrimSpace(principal.Subject) {
		writeError(w, http.StatusForbidden, "history_capture_forbidden", "history capture belongs to another worker")
		return
	}
	grant := strings.TrimSpace(r.Header.Get(historyUploadGrantHeader))
	if grant == "" {
		writeError(w, http.StatusUnauthorized, "history_grant_required", "history upload grant is required")
		return
	}
	if len(parts) == 3 {
		if parts[1] == "uploads" && strings.TrimSpace(parts[2]) != "" {
			project.handleWorkerHistoryUploadAbandon(w, r, capture.ID, grant, strings.TrimSpace(parts[2]))
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	switch parts[1] {
	case "uploads":
		project.handleWorkerHistoryUpload(w, r, capture.ID, grant)
	case "artifacts":
		project.handleWorkerHistoryArtifact(w, r, capture, grant)
	case "transcript-segments":
		project.handleWorkerHistoryTranscriptSegment(w, r, capture.ID, grant)
	case "transcript-seal":
		project.handleWorkerHistoryTranscriptSeal(w, r, capture.ID, grant)
	case "expected-set":
		project.handleWorkerHistoryExpectedSet(w, r, capture, grant, principal)
	case "workspace-summary":
		project.handleWorkerHistoryWorkspaceSummary(w, r, capture.ID, grant)
	case "harness-members":
		project.handleWorkerHistoryHarnessMembers(w, r, capture.ID, grant)
	case "verdict":
		project.handleWorkerHistoryVerdict(w, r, capture, grant, principal)
	case "transition":
		project.handleWorkerHistoryTransition(w, r, capture, grant, principal)
	case "manifest":
		project.handleWorkerHistoryManifest(w, r, capture.ID, grant)
	case "complete":
		project.handleWorkerHistoryComplete(w, r, capture, grant, principal)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *Server) handleWorkerHistoryResumeArtifact(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, captureID, artifactID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if captureID == "" || artifactID == "" {
		writeError(w, http.StatusNotFound, "history_artifact_not_found", "history artifact not found")
		return
	}
	project, capture, ok := s.bundleForHistoryCapture(w, r, principal, captureID)
	if !ok {
		return
	}
	jobID := strings.TrimSpace(r.Header.Get(historyResumeJobHeader))
	leaseID := strings.TrimSpace(r.Header.Get(historyResumeLeaseHeader))
	if jobID == "" || leaseID == "" {
		writeError(w, http.StatusUnauthorized, "history_resume_lease_required", "resume job and lease are required")
		return
	}
	lease, err := project.workers.GetLease(r.Context(), leaseID)
	if err != nil || lease.JobID != jobID || lease.WorkerID != strings.TrimSpace(principal.Subject) ||
		lease.ReleasedAt != nil || !lease.ExpiresAt.After(time.Now().UTC()) {
		writeError(w, http.StatusForbidden, "history_resume_lease_forbidden", "an active matching resume lease is required")
		return
	}
	resume, err := project.history.ResumeLineageForJob(r.Context(), jobID)
	if err != nil || resume.SourceCaptureID != capture.ID ||
		(artifactID != resume.HarnessArtifactID && artifactID != resume.WorkspaceArtifactID) {
		writeError(w, http.StatusForbidden, "history_resume_artifact_forbidden", "artifact is not part of the claimed resume")
		return
	}
	artifact, err := project.history.GetArtifactByID(r.Context(), capture.ID, artifactID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	project.serveHistoryArtifactContent(w, r, capture.ID, artifact)
}

func (s *Server) bundleForHistoryCapture(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, captureID string) (*projectServer, coordinator.HistoryCapture, bool) {
	for _, bundle := range s.scopedBundles(principal) {
		capture, err := bundle.HistoryCaptures.Get(r.Context(), captureID)
		if err == nil {
			return s.forBundle(bundle), capture, true
		}
		if !errors.Is(err, coordinator.ErrHistoryCaptureNotFound) {
			writeHistoryError(w, err)
			return nil, coordinator.HistoryCapture{}, false
		}
	}
	writeError(w, http.StatusNotFound, "history_capture_not_found", "history capture not found")
	return nil, coordinator.HistoryCapture{}, false
}

func (s *Server) handleWorkerReserveHistoryCapture(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var input contract.ReserveHistoryCaptureRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_reservation", err.Error())
		return
	}
	project, ok := s.bundleForLease(r.Context(), principal, strings.TrimSpace(input.LeaseID))
	if !ok {
		writeError(w, http.StatusNotFound, "lease_not_found", "lease not found")
		return
	}
	lease, err := project.workers.GetLease(r.Context(), strings.TrimSpace(input.LeaseID))
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	if lease.JobID != strings.TrimSpace(input.JobID) || lease.WorkerID != strings.TrimSpace(principal.Subject) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(time.Now().UTC()) {
		writeError(w, http.StatusForbidden, "history_lease_forbidden", "an active matching worker lease is required")
		return
	}
	job, err := project.workers.GetJob(r.Context(), lease.JobID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	var leaseAttempt int64
	if err := project.projectDB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM leases WHERE job_id = ? AND leased_at <= ?`, job.ID, lease.LeasedAt.Format(time.RFC3339Nano)).Scan(&leaseAttempt); err != nil {
		writeHistoryError(w, err)
		return
	}
	var resumedFromCaptureID, resumedFromNativeSessionID string
	resume, err := project.history.ResumeLineageForJob(r.Context(), job.ID)
	if err == nil {
		resumedFromCaptureID = resume.SourceCaptureID
		resumedFromNativeSessionID = resume.SourceNativeSessionID
		sourceCapture, sourceErr := project.history.Get(r.Context(), resume.SourceCaptureID)
		if sourceErr != nil {
			writeHistoryError(w, sourceErr)
			return
		}
		if !input.ExpectedTranscript || !input.ExpectedHarness ||
			strings.TrimSpace(input.HarnessName) != sourceCapture.HarnessName ||
			strings.TrimSpace(input.HarnessVersion) != resume.RequiredHarnessBuild ||
			input.HarnessSchemaVersion != resume.RequiredHarnessSchema {
			writeError(w, http.StatusConflict, "history_resume_mismatch", "resume history requirements do not match coordinator metadata")
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeHistoryError(w, err)
		return
	}
	reserve := coordinator.ReserveHistoryCaptureInput{
		ProjectID: project.project.ID, JobID: job.ID, LeaseID: lease.ID, LeaseAttempt: leaseAttempt,
		WorkerID: lease.WorkerID, TaskID: historyStringPointer(job.TaskID), ChangeID: historyStringPointer(job.ChangeID),
		WorkflowRunID: historyStringPointer(job.WorkflowRunID), NodeRunID: historyStringPointer(job.NodeRunID),
		SessionID: historyPayloadString(job.Payload, "session_id"), Stage: historyPayloadString(job.Payload, "stage"),
		Role: string(job.Role), NodeVisit: input.NodeVisit, HarnessName: input.HarnessName,
		HarnessVersion: input.HarnessVersion, HarnessSchemaVersion: input.HarnessSchemaVersion,
		ResumedFromCaptureID: resumedFromCaptureID, ResumedFromHarnessSessionID: resumedFromNativeSessionID,
		ExpectedTranscript: input.ExpectedTranscript, ExpectedHarness: input.ExpectedHarness,
	}
	result, err := project.history.Reserve(r.Context(), reserve)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, contract.ReserveHistoryCaptureResponse{
		Capture: projectHistoryCapture(result.Capture), UploadGrant: result.UploadGrant,
		Created: result.Created, GrantRotated: result.GrantRotated,
	})
}

func (s *projectServer) projectDB() *sql.DB {
	bundle, ok := s.registry.Bundle(s.project.ID)
	if !ok {
		return nil
	}
	return bundle.Store.DB()
}

func historyStringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func historyPayloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func (s *projectServer) handleWorkerHistoryUpload(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	upload, err := s.history.BeginUpload(r.Context(), captureID, grant)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	if _, err := io.Copy(upload, r.Body); err != nil {
		_ = upload.Abort(context.WithoutCancel(r.Context()))
		writeHistoryError(w, err)
		return
	}
	temporary, err := upload.Complete(r.Context())
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, contract.HistoryUploadResponse{
		TemporaryUploadID: temporary.ID, SHA256: temporary.Digest.String(), StoredSize: temporary.Size,
	})
}

func (s *projectServer) handleWorkerHistoryUploadAbandon(w http.ResponseWriter, r *http.Request, captureID, grant, temporaryUploadID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.history.AbandonUpload(r.Context(), captureID, grant, temporaryUploadID); err != nil {
		writeHistoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *projectServer) handleWorkerHistoryArtifact(w http.ResponseWriter, r *http.Request, capture coordinator.HistoryCapture, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.PublishHistoryArtifactRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_artifact", err.Error())
		return
	}
	artifact, err := s.history.PublishTemporaryArtifact(r.Context(), capture.ID, grant, input.TemporaryUploadID, coordinator.PublishHistoryArtifactInput{
		LogicalKey: input.LogicalKey, Kind: coordinator.HistoryArtifactKind(input.Kind), Phase: coordinator.HistoryArtifactPhase(input.Phase),
		CheckpointGeneration: input.CheckpointGeneration, CheckpointTrigger: input.CheckpointTrigger,
		CheckpointStream: input.CheckpointStream, ArchiveID: input.ArchiveID, MediaType: input.MediaType,
		FormatVersion: input.FormatVersion, SchemaVersion: input.SchemaVersion,
		LogicalSize: input.LogicalSize, EntryCount: input.EntryCount,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projectHistoryArtifact(artifact, capture.ProjectID))
}

func (s *projectServer) handleWorkerHistoryTranscriptSegment(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.RegisterHistoryTranscriptSegmentRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_transcript_segment", err.Error())
		return
	}
	segment, err := s.history.RegisterTranscriptSegment(r.Context(), captureID, grant, coordinator.RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: input.ArtifactLogicalKey, Epoch: input.Epoch, Sequence: input.Sequence,
		StartOffset: input.StartOffset, EndOffset: input.EndOffset, Encoding: input.Encoding,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, segment)
}

func (s *projectServer) handleWorkerHistoryTranscriptSeal(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.HistoryTranscriptSeal
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_transcript_seal", err.Error())
		return
	}
	err := s.history.SealTranscript(r.Context(), captureID, grant, coordinator.TranscriptSeal{
		FinalEpoch: input.FinalEpoch, SegmentCount: input.SegmentCount,
		LogicalLength: input.LogicalLength, SHA256: input.SHA256,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, input)
}

func (s *projectServer) handleWorkerHistoryExpectedSet(w http.ResponseWriter, r *http.Request, capture coordinator.HistoryCapture, grant string, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.DeclareHistoryExpectedSetRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_expected_set", err.Error())
		return
	}
	artifacts := make([]coordinator.FinalArtifactExpectation, 0, len(input.Artifacts))
	for _, artifact := range input.Artifacts {
		artifacts = append(artifacts, coordinator.FinalArtifactExpectation{
			LogicalKey: artifact.LogicalKey, Kind: coordinator.HistoryArtifactKind(artifact.Kind),
		})
	}
	var seal *coordinator.TranscriptSeal
	if input.TranscriptSeal != nil {
		seal = &coordinator.TranscriptSeal{
			FinalEpoch: input.TranscriptSeal.FinalEpoch, SegmentCount: input.TranscriptSeal.SegmentCount,
			LogicalLength: input.TranscriptSeal.LogicalLength, SHA256: input.TranscriptSeal.SHA256,
		}
	}
	updated, err := s.history.DeclareExpectedSet(r.Context(), capture.ID, grant, coordinator.DeclareHistoryExpectedSetInput{
		Artifacts: artifacts, TranscriptSeal: seal, ExpectedVersion: input.ExpectedVersion,
		Actor: "worker:" + strings.TrimSpace(principal.Subject),
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(updated))
}

func (s *projectServer) handleWorkerHistoryWorkspaceSummary(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.RegisterHistoryWorkspaceSummaryRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_workspace_summary", err.Error())
		return
	}
	summary, err := s.history.RegisterWorkspaceSummary(r.Context(), captureID, grant, coordinator.RegisterHistoryWorkspaceSummaryInput{
		ArtifactLogicalKey: input.ArtifactLogicalKey, ArchiveSchemaVersion: input.ArchiveSchemaVersion,
		Branch: input.Branch, Detached: input.Detached, BaseRef: input.BaseRef, BaseCommit: input.BaseCommit,
		HeadCommit: input.HeadCommit, StagedCount: input.StagedCount, UnstagedCount: input.UnstagedCount,
		UntrackedCount: input.UntrackedCount, InventoryDigest: input.InventoryDigest,
		ValidationStatus: input.ValidationStatus,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projectHistoryWorkspaceSummary(summary))
}

func (s *projectServer) handleWorkerHistoryHarnessMembers(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.RegisterHistoryHarnessMembersRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_harness_members", err.Error())
		return
	}
	members := make([]coordinator.HarnessArchiveMemberInput, 0, len(input.Members))
	for _, member := range input.Members {
		members = append(members, coordinator.HarnessArchiveMemberInput{
			NativeSessionID: member.NativeSessionID, NativeParentSessionID: member.NativeParentSessionID,
			RelativeMemberPath: member.RelativeMemberPath, MemberKind: member.MemberKind,
			AgentName: member.AgentName, Status: member.Status, Model: member.Model,
			HarnessBuild: member.HarnessBuild, ParseStatus: member.ParseStatus,
		})
	}
	if err := s.history.RegisterHarnessArchiveMembers(r.Context(), captureID, grant, input.ArtifactLogicalKey, members); err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"registered": len(members)})
}

func (s *projectServer) handleWorkerHistoryVerdict(w http.ResponseWriter, r *http.Request, capture coordinator.HistoryCapture, grant string, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.history.AuthenticateUploadGrant(r.Context(), capture.ID, grant); err != nil {
		writeHistoryError(w, err)
		return
	}
	var input contract.RecordHistoryExecutionVerdictRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_verdict", err.Error())
		return
	}
	updated, err := s.history.RecordExecutionVerdict(r.Context(), capture.ID, coordinator.RecordHistoryExecutionVerdictInput{
		Verdict: coordinator.HistoryExecutionVerdict(input.Verdict), ExitCode: input.ExitCode,
		ErrorCode: input.ErrorCode, ExpectedVersion: input.ExpectedVersion,
		Actor: "worker:" + strings.TrimSpace(principal.Subject),
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(updated))
}

func (s *projectServer) handleWorkerHistoryTransition(w http.ResponseWriter, r *http.Request, capture coordinator.HistoryCapture, grant string, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.history.AuthenticateUploadGrant(r.Context(), capture.ID, grant); err != nil {
		writeHistoryError(w, err)
		return
	}
	var input contract.TransitionHistoryCaptureRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_transition", err.Error())
		return
	}
	updated, err := s.history.Transition(r.Context(), capture.ID, coordinator.TransitionHistoryCaptureInput{
		To: coordinator.HistoryCaptureState(input.To), ExpectedVersion: input.ExpectedVersion,
		Actor: "worker:" + strings.TrimSpace(principal.Subject), ErrorCode: input.ErrorCode,
		ErrorMessage: input.ErrorMessage,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(updated))
}

func (s *projectServer) handleWorkerHistoryManifest(w http.ResponseWriter, r *http.Request, captureID, grant string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.history.AuthenticateUploadGrant(r.Context(), captureID, grant); err != nil {
		writeHistoryError(w, err)
		return
	}
	artifact, err := s.history.GenerateCanonicalManifest(r.Context(), captureID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projectHistoryArtifact(artifact, s.project.ID))
}

func (s *projectServer) handleWorkerHistoryComplete(w http.ResponseWriter, r *http.Request, capture coordinator.HistoryCapture, grant string, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input contract.CompleteHistoryCaptureRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_completion", err.Error())
		return
	}
	updated, err := s.history.Complete(r.Context(), capture.ID, grant, input.ExpectedVersion, "worker:"+strings.TrimSpace(principal.Subject))
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(updated))
}
