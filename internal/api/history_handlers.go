package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

const (
	historyListDefaultLimit = 50
	historyListMaxLimit     = 200
	historyCursorVersion    = 1
)

type historyCursor struct {
	Version       int    `json:"v"`
	SnapshotUntil string `json:"snapshot_until"`
	ReservedAt    string `json:"reserved_at"`
	CaptureID     string `json:"capture_id"`
	Filter        string `json:"filter"`
}

func (s *projectServer) handleHistoryPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
		return
	}
	if s.history == nil {
		writeError(w, http.StatusServiceUnavailable, "history_unavailable", "history service is not configured")
		return
	}

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/history/captures"), "/")
	if rest == "" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleListHistoryCaptures(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	captureID := strings.TrimSpace(parts[0])
	if captureID == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	switch {
	case len(parts) == 1:
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleGetHistoryCapture(w, r, captureID)
	case len(parts) == 2 && parts[1] == "manifest":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return
		}
		s.handleGetHistoryManifest(w, r, captureID)
	case len(parts) == 2 && parts[1] == "events":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleListHistoryEvents(w, r, captureID)
	case len(parts) == 2 && parts[1] == "waive":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleWaiveHistoryCapture(w, r, captureID, principal)
	case len(parts) == 3 && parts[1] == "upload-grant" && parts[2] == "revoke":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleRevokeHistoryUploadGrant(w, r, captureID, principal)
	case len(parts) == 2 && parts[1] == "resume":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleResumeHistoryCapture(w, r, captureID, principal)
	case len(parts) == 4 && parts[1] == "artifacts" && parts[3] == "content":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return
		}
		s.handleGetHistoryArtifactContent(w, r, captureID, strings.TrimSpace(parts[2]))
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *projectServer) handleListHistoryCaptures(w http.ResponseWriter, r *http.Request) {
	allowed := map[string]bool{
		"task_id": true, "job_id": true, "session_id": true, "capture_id": true,
		"state": true, "since": true, "until": true, "resumable": true,
		"limit": true, "cursor": true,
	}
	query := r.URL.Query()
	for key := range query {
		if !allowed[key] {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", "unknown history query parameter: "+key)
			return
		}
	}
	for _, key := range []string{"since", "until", "resumable", "limit", "cursor"} {
		if len(query[key]) > 1 {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", key+" may be specified only once")
			return
		}
	}
	for _, key := range []string{"task_id", "job_id", "session_id", "capture_id", "state"} {
		if len(query[key]) > 50 {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", key+" may be specified at most 50 times")
			return
		}
	}

	limit := historyListDefaultLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > historyListMaxLimit {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", "limit must be an integer between 1 and 200")
			return
		}
		limit = parsed
	}
	options := coordinator.HistoryCaptureListOptions{
		TaskIDs: query["task_id"], JobIDs: query["job_id"], SessionIDs: query["session_id"],
		CaptureIDs: query["capture_id"], Limit: limit + 1,
	}
	validStates := map[coordinator.HistoryCaptureState]bool{
		coordinator.HistoryCaptureReserved: true, coordinator.HistoryCaptureRunning: true,
		coordinator.HistoryCaptureQuiescing: true, coordinator.HistoryCaptureSealed: true,
		coordinator.HistoryCaptureUploading: true, coordinator.HistoryCaptureComplete: true,
		coordinator.HistoryCaptureBlocked: true, coordinator.HistoryCaptureLost: true,
		coordinator.HistoryCaptureWaived: true,
	}
	for _, raw := range query["state"] {
		state := coordinator.HistoryCaptureState(strings.TrimSpace(raw))
		if !validStates[state] {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", "invalid history capture state: "+raw)
			return
		}
		options.States = append(options.States, state)
	}
	var err error
	if options.Since, err = parseOptionalHistoryTime(query.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_filter", "invalid since: "+err.Error())
		return
	}
	if options.Until, err = parseOptionalHistoryTime(query.Get("until")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_filter", "invalid until: "+err.Error())
		return
	}
	if raw := strings.TrimSpace(query.Get("resumable")); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_history_filter", "resumable must be true or false")
			return
		}
		options.Resumable = &value
	}

	filterQuery := cloneHistoryQueryWithoutCursor(query)
	filterIdentity := filterQuery.Encode()
	snapshotUntil := time.Now().UTC()
	if options.Until != nil {
		snapshotUntil = options.Until.UTC()
	}
	if rawCursor := strings.TrimSpace(query.Get("cursor")); rawCursor != "" {
		cursor, decodeErr := decodeHistoryCursor(rawCursor)
		if decodeErr != nil || cursor.Filter != filterIdentity {
			writeError(w, http.StatusBadRequest, "invalid_history_cursor", "history cursor is invalid or does not match the filters")
			return
		}
		snapshotUntil, err = time.Parse(time.RFC3339Nano, cursor.SnapshotUntil)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_history_cursor", "history cursor contains an invalid snapshot time")
			return
		}
		before, parseErr := time.Parse(time.RFC3339Nano, cursor.ReservedAt)
		if parseErr != nil || strings.TrimSpace(cursor.CaptureID) == "" {
			writeError(w, http.StatusBadRequest, "invalid_history_cursor", "history cursor contains an invalid keyset")
			return
		}
		options.BeforeTime, options.BeforeID = &before, cursor.CaptureID
	}
	options.Until = &snapshotUntil
	if options.Since != nil && !snapshotUntil.After(*options.Since) {
		writeError(w, http.StatusBadRequest, "invalid_history_filter", "until must be after since")
		return
	}

	captures, err := s.history.List(r.Context(), options)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	availability, err := s.history.CountAvailability(r.Context(), options)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	hasMore := len(captures) > limit
	if hasMore {
		captures = captures[:limit]
	}
	response := contract.HistoryCapturesResponse{
		SnapshotUntil: snapshotUntil.Format(time.RFC3339Nano),
		Captures:      make([]contract.HistoryCapture, 0, len(captures)),
		Availability: contract.HistoryAvailability{
			Total: availability.Total, Complete: availability.Complete, Resumable: availability.Resumable,
			Blocked: availability.Blocked, Lost: availability.Lost, Waived: availability.Waived,
		},
	}
	for _, capture := range captures {
		response.Captures = append(response.Captures, projectHistoryCapture(capture))
	}
	if hasMore && len(captures) > 0 {
		last := captures[len(captures)-1]
		response.NextCursor, err = encodeHistoryCursor(historyCursor{
			Version: historyCursorVersion, SnapshotUntil: response.SnapshotUntil,
			ReservedAt: last.ReservedAt.UTC().Format(time.RFC3339Nano), CaptureID: last.ID,
			Filter: filterIdentity,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "history_cursor_failed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func cloneHistoryQueryWithoutCursor(source url.Values) url.Values {
	clone := make(url.Values, len(source))
	for key, values := range source {
		if key == "cursor" {
			continue
		}
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func parseOptionalHistoryTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, errors.New("must be RFC3339")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func encodeHistoryCursor(cursor historyCursor) (string, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func decodeHistoryCursor(value string) (historyCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return historyCursor{}, err
	}
	var cursor historyCursor
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return historyCursor{}, err
	}
	if cursor.Version != historyCursorVersion {
		return historyCursor{}, errors.New("unsupported cursor version")
	}
	return cursor, nil
}

func (s *projectServer) handleGetHistoryCapture(w http.ResponseWriter, r *http.Request, captureID string) {
	capture, err := s.history.Get(r.Context(), captureID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	artifacts, err := s.history.ListArtifacts(r.Context(), capture.ID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	members, err := s.history.ListHarnessArchiveMembers(r.Context(), capture.ID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	response := contract.HistoryCaptureResponse{
		Capture: projectHistoryCapture(capture), Artifacts: make([]contract.HistoryArtifact, 0, len(artifacts)),
		HarnessMembers: make([]contract.HistoryHarnessMember, 0, len(members)),
	}
	for _, artifact := range artifacts {
		response.Artifacts = append(response.Artifacts, projectHistoryArtifact(artifact, capture.ProjectID))
		if artifact.Kind == coordinator.HistoryArtifactWorkspaceSnapshot && artifact.Phase == coordinator.HistoryArtifactFinal {
			summary, summaryErr := s.history.GetWorkspaceSummary(r.Context(), artifact.ID)
			if summaryErr != nil {
				writeHistoryError(w, summaryErr)
				return
			}
			projected := projectHistoryWorkspaceSummary(summary)
			response.WorkspaceSummary = &projected
		}
	}
	for _, member := range members {
		response.HarnessMembers = append(response.HarnessMembers, contract.HistoryHarnessMember{
			ArtifactID: member.ArtifactID, ArchiveID: member.ArchiveID,
			NativeSessionID: member.NativeSessionID, NativeParentSessionID: member.NativeParentSessionID,
			RelativeMemberPath: member.RelativeMemberPath, MemberKind: member.MemberKind,
			AgentName: member.AgentName, Status: member.Status, Model: member.Model,
			HarnessBuild: member.HarnessBuild, ParseStatus: member.ParseStatus,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleListHistoryEvents(w http.ResponseWriter, r *http.Request, captureID string) {
	if _, err := s.history.Get(r.Context(), captureID); err != nil {
		writeHistoryError(w, err)
		return
	}
	events, err := s.history.ListEvents(r.Context(), captureID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	response := contract.HistoryCaptureEventsResponse{Events: make([]contract.HistoryCaptureEvent, 0, len(events))}
	for _, event := range events {
		details := json.RawMessage(event.DetailsJSON)
		if !json.Valid(details) {
			writeError(w, http.StatusInternalServerError, "history_event_invalid", "history event details are invalid")
			return
		}
		response.Events = append(response.Events, contract.HistoryCaptureEvent{
			ID: event.ID, CaptureID: event.CaptureID, EventKind: event.EventKind,
			FromState: event.FromState, ToState: event.ToState, CaptureVersion: event.CaptureVersion,
			Actor: event.Actor, Code: event.Code, Details: details,
			OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleWaiveHistoryCapture(w http.ResponseWriter, r *http.Request, captureID string, principal coordinator.Principal) {
	var input contract.WaiveHistoryCaptureRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_waiver", err.Error())
		return
	}
	capture, err := s.history.Waive(r.Context(), captureID, input.ExpectedVersion, historyOwnerActor(principal), input.Reason)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(capture))
}

func (s *projectServer) handleRevokeHistoryUploadGrant(w http.ResponseWriter, r *http.Request, captureID string, principal coordinator.Principal) {
	var input contract.RevokeHistoryUploadGrantRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_revoke", err.Error())
		return
	}
	capture, err := s.history.RevokeUploadGrant(r.Context(), captureID, input.ExpectedVersion, historyOwnerActor(principal), input.Reason)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHistoryCapture(capture))
}

func historyOwnerActor(principal coordinator.Principal) string {
	subject := strings.TrimSpace(principal.Subject)
	if subject == "" {
		return "owner"
	}
	return "owner:" + subject
}

func (s *projectServer) handleResumeHistoryCapture(w http.ResponseWriter, r *http.Request, captureID string, principal coordinator.Principal) {
	capture, err := s.history.Get(r.Context(), captureID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	var input contract.ResumeHistoryCaptureRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_history_resume", err.Error())
		return
	}
	resume, created, err := s.history.CreateResume(r.Context(), s.workers, s.project, coordinator.CreateHistoryResumeInput{
		SourceCaptureID: capture.ID,
		NativeSessionID: input.NativeSessionID,
		IdempotencyKey:  input.IdempotencyKey,
		RequestedBy:     principal.Subject,
	})
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, contract.ResumeHistoryCaptureResponse{
		ID: resume.ID, SourceCaptureID: resume.SourceCaptureID,
		SourceNativeSessionID: resume.SourceNativeSessionID,
		JobID:                 resume.JobID, State: resume.State,
		RequiredHeadCommit:    resume.RequiredHeadCommit,
		RequiredHarnessBuild:  resume.RequiredHarnessBuild,
		RequiredHarnessSchema: resume.RequiredHarnessSchema,
		Created:               created,
	})
}

func (s *projectServer) handleGetHistoryManifest(w http.ResponseWriter, r *http.Request, captureID string) {
	if _, err := s.history.Get(r.Context(), captureID); err != nil {
		writeHistoryError(w, err)
		return
	}
	artifacts, err := s.history.ListArtifacts(r.Context(), captureID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	for _, artifact := range artifacts {
		if artifact.Kind == coordinator.HistoryArtifactManifest && artifact.Phase == coordinator.HistoryArtifactFinal {
			s.serveHistoryArtifactContent(w, r, captureID, artifact)
			return
		}
	}
	writeError(w, http.StatusNotFound, "history_manifest_not_found", "history capture manifest not found")
}

func (s *projectServer) handleGetHistoryArtifactContent(w http.ResponseWriter, r *http.Request, captureID, artifactID string) {
	if artifactID == "" {
		writeError(w, http.StatusNotFound, "history_artifact_not_found", "history artifact not found")
		return
	}
	artifact, err := s.history.GetArtifactByID(r.Context(), captureID, artifactID)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	s.serveHistoryArtifactContent(w, r, captureID, artifact)
}

func (s *projectServer) serveHistoryArtifactContent(w http.ResponseWriter, r *http.Request, captureID string, artifact coordinator.HistoryArtifact) {
	if artifact.PublicationState != coordinator.HistoryPublicationCommitted {
		writeHistoryError(w, coordinator.ErrHistoryPublicationPending)
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("ETag", `"`+artifact.SHA256+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, artifact.ID))

	byteRange, partial, err := parseHistoryRange(r.Header.Get("Range"), artifact.StoredSize)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", artifact.StoredSize))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", err.Error())
		return
	}
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.Offset, byteRange.Offset+byteRange.Length-1, artifact.StoredSize))
		w.Header().Set("Content-Length", strconv.FormatInt(byteRange.Length, 10))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusPartialContent)
			return
		}
		_, reader, openErr := s.history.OpenArtifactRange(r.Context(), captureID, artifact.ID, byteRange)
		if openErr != nil {
			writeHistoryError(w, openErr)
			return
		}
		defer reader.Body.Close()
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.Copy(w, reader.Body)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(artifact.StoredSize, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, body, openErr := s.history.OpenArtifact(r.Context(), captureID, artifact.ID)
	if openErr != nil {
		writeHistoryError(w, openErr)
		return
	}
	defer body.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func parseHistoryRange(header string, size int64) (blob.ByteRange, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return blob.ByteRange{}, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") || size <= 0 {
		return blob.ByteRange{}, false, errors.New("only one satisfiable byte range is supported")
	}
	value := strings.TrimPrefix(header, "bytes=")
	startText, endText, ok := strings.Cut(value, "-")
	if !ok {
		return blob.ByteRange{}, false, errors.New("invalid byte range")
	}
	var start, end int64
	var err error
	if startText == "" {
		suffix, parseErr := strconv.ParseInt(endText, 10, 64)
		if parseErr != nil || suffix <= 0 {
			return blob.ByteRange{}, false, errors.New("invalid byte range suffix")
		}
		if suffix > size {
			suffix = size
		}
		start, end = size-suffix, size-1
	} else {
		start, err = strconv.ParseInt(startText, 10, 64)
		if err != nil || start < 0 || start >= size {
			return blob.ByteRange{}, false, errors.New("byte range starts outside the artifact")
		}
		if endText == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(endText, 10, 64)
			if err != nil || end < start {
				return blob.ByteRange{}, false, errors.New("invalid byte range end")
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return blob.ByteRange{Offset: start, Length: end - start + 1}, true, nil
}

func projectHistoryCapture(c coordinator.HistoryCapture) contract.HistoryCapture {
	value := contract.HistoryCapture{
		ID: c.ID, ProjectID: c.ProjectID, JobID: c.JobID, LeaseID: c.LeaseID,
		LeaseAttempt: c.LeaseAttempt, WorkerID: c.WorkerID, TaskID: c.TaskID,
		ChangeID: c.ChangeID, SessionID: c.SessionID, WorkflowRunID: c.WorkflowRunID,
		NodeRunID: c.NodeRunID, NodeVisit: c.NodeVisit, Stage: c.Stage, Role: c.Role,
		HarnessName: c.HarnessName, HarnessVersion: c.HarnessVersion,
		HarnessSchemaVersion: c.HarnessSchemaVersion,
		ResumedFromCaptureID: c.ResumedFromCaptureID, ResumedFromHarnessSessionID: c.ResumedFromHarnessSessionID,
		ExpectedTranscript: c.ExpectedTranscript, ExpectedHarness: c.ExpectedHarness,
		State: string(c.State), ExecutionVerdict: string(c.ExecutionVerdict),
		ExecutionExitCode: c.ExecutionExitCode, ExecutionErrorCode: c.ExecutionErrorCode,
		ErrorCode: c.ErrorCode, ErrorMessage: c.ErrorMessage, WaiverReason: c.WaiverReason,
		Version: c.Version, Resumable: c.Resumable,
		ReservedAt: c.ReservedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if c.CompletedAt != nil {
		value.CompletedAt = c.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return value
}

func projectHistoryArtifact(a coordinator.HistoryArtifact, projectID string) contract.HistoryArtifact {
	value := contract.HistoryArtifact{
		ID: a.ID, CaptureID: a.CaptureID, LogicalKey: a.LogicalKey,
		Kind: string(a.Kind), Phase: string(a.Phase), CheckpointGeneration: a.CheckpointGeneration,
		CheckpointTrigger: a.CheckpointTrigger, CheckpointStream: a.CheckpointStream,
		ArchiveID: a.ArchiveID, MediaType: a.MediaType, FormatVersion: a.FormatVersion,
		SchemaVersion: a.SchemaVersion, SHA256: a.SHA256, StoredSize: a.StoredSize,
		LogicalSize: a.LogicalSize, EntryCount: a.EntryCount,
		PublicationState: string(a.PublicationState), SupersededByArtifactID: a.SupersededByArtifactID,
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if a.CommittedAt != nil {
		value.CommittedAt = a.CommittedAt.UTC().Format(time.RFC3339Nano)
	}
	if a.PublicationState == coordinator.HistoryPublicationCommitted {
		value.ContentPath = "/v2/projects/" + url.PathEscape(projectID) + "/history/captures/" + url.PathEscape(a.CaptureID) + "/artifacts/" + url.PathEscape(a.ID) + "/content"
	}
	return value
}

func projectHistoryWorkspaceSummary(s coordinator.HistoryWorkspaceSummary) contract.HistoryWorkspaceSummary {
	return contract.HistoryWorkspaceSummary{
		ArtifactID: s.ArtifactID, ArchiveSchemaVersion: s.ArchiveSchemaVersion,
		Branch: s.Branch, Detached: s.Detached, BaseRef: s.BaseRef, BaseCommit: s.BaseCommit,
		HeadCommit: s.HeadCommit, StagedCount: s.StagedCount, UnstagedCount: s.UnstagedCount,
		UntrackedCount: s.UntrackedCount, InventoryDigest: s.InventoryDigest,
		ValidationStatus: s.ValidationStatus,
	}
}

func writeHistoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coordinator.ErrHistoryCaptureNotFound):
		writeError(w, http.StatusNotFound, "history_capture_not_found", "history capture not found")
	case errors.Is(err, coordinator.ErrHistoryArtifactNotFound):
		writeError(w, http.StatusNotFound, "history_artifact_not_found", "history artifact not found")
	case errors.Is(err, coordinator.ErrHistoryPublicationPending):
		writeError(w, http.StatusConflict, "history_artifact_pending", "history artifact is not committed")
	case errors.Is(err, coordinator.ErrHistoryUnauthorized):
		writeError(w, http.StatusUnauthorized, "history_grant_unauthorized", "history upload grant is unauthorized")
	case errors.Is(err, coordinator.ErrHistoryGrantNoLongerUsable):
		writeError(w, http.StatusGone, "history_grant_expired", "history upload grant is no longer usable")
	case errors.Is(err, coordinator.ErrHistoryConflict), errors.Is(err, coordinator.ErrHistoryVersionConflict), errors.Is(err, coordinator.ErrHistoryIntentNotActive), errors.Is(err, coordinator.ErrHistoryIncomplete):
		writeError(w, http.StatusConflict, "history_conflict", err.Error())
	case errors.Is(err, coordinator.ErrHistoryInvalidTransition):
		writeError(w, http.StatusBadRequest, "invalid_history_transition", err.Error())
	case errors.Is(err, coordinator.ErrHistoryUploadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "history_upload_too_large", err.Error())
	case errors.Is(err, coordinator.ErrHistoryStorageCapacity):
		writeError(w, http.StatusInsufficientStorage, "history_storage_capacity", err.Error())
	case errors.Is(err, blob.ErrNotFound), errors.Is(err, blob.ErrInvalidUpload):
		writeError(w, http.StatusConflict, "history_upload_not_found", "history temporary upload is unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "history_failed", err.Error())
	}
}
