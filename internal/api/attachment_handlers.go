package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

const taskAttachmentUploadLimit = coordinator.TaskAttachmentMaxBytes + (1 << 20)

func (s *projectServer) handleTaskAttachmentsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
				writeError(w, http.StatusForbidden, "forbidden", "attachment read requires owner, session, or worker token")
				return
			}
			s.handleListTaskAttachments(w, r, taskID)
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
				writeError(w, http.StatusForbidden, "forbidden", "attachment upload requires owner, session, or worker token")
				return
			}
			if err := s.checkTaskAttachmentWriteScope(r, principal, taskID); err != nil {
				writeAttachmentScopeError(w, err)
				return
			}
			s.handleUploadTaskAttachment(w, r, principal, taskID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}

	if len(parts) == 1 && r.Method == http.MethodGet {
		if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
			writeError(w, http.StatusForbidden, "forbidden", "attachment read requires owner, session, or worker token")
			return
		}
		s.handleDownloadTaskAttachment(w, r, taskID, parts[0])
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}

func (s *projectServer) handleListTaskAttachments(w http.ResponseWriter, r *http.Request, taskID string) {
	if _, err := s.tasks.GetTask(r.Context(), taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "task_not_found", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "task_lookup_failed", err.Error())
		return
	}
	attachments, err := s.tasks.ListTaskAttachments(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attachments_list_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, taskAttachmentsResponse{Attachments: attachments})
}

func (s *projectServer) handleUploadTaskAttachment(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, taskID string) {
	if s.attachments == nil {
		writeError(w, http.StatusInternalServerError, "attachments_unavailable", "attachment store is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, taskAttachmentUploadLimit)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attachment_upload", err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachment_file_required", "multipart field \"file\" is required")
		return
	}
	defer file.Close()

	attachment, err := s.tasks.CreateTaskAttachment(r.Context(), coordinator.CreateTaskAttachmentInput{
		TaskID:      taskID,
		Stage:       coordinator.TaskAttachmentStage(r.FormValue("stage")),
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		CreatedBy:   attachmentActorForPrincipal(principal),
		Reader:      file,
	}, s.attachments)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "task_not_found", "task not found")
			return
		}
		writeError(w, http.StatusBadRequest, "attachment_upload_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, taskAttachmentResponse{Attachment: attachment})
}

func (s *projectServer) handleDownloadTaskAttachment(w http.ResponseWriter, r *http.Request, taskID string, attachmentID string) {
	if s.attachments == nil {
		writeError(w, http.StatusInternalServerError, "attachments_unavailable", "attachment store is not configured")
		return
	}
	attachment, err := s.tasks.GetTaskAttachment(r.Context(), taskID, attachmentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "attachment_not_found", "attachment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "attachment_lookup_failed", err.Error())
		return
	}
	reader, err := s.attachments.Open(attachment.StorageKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "attachment_not_found", "attachment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "attachment_read_failed", err.Error())
		return
	}
	defer reader.Close()

	contentType, inlineSafe := taskAttachmentResponseContentType(attachment.ContentType)
	disposition := "attachment"
	if inlineSafe && r.URL.Query().Get("download") != "1" {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(attachment.SizeBytes))
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": attachment.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

func taskAttachmentResponseContentType(contentType string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "application/octet-stream", false
	}
	mediaType = strings.ToLower(mediaType)
	if coordinator.IsImageContentType(mediaType) {
		return mediaType, true
	}
	return "application/octet-stream", false
}

func (s *projectServer) checkTaskAttachmentWriteScope(r *http.Request, principal coordinator.Principal, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession:
		if principal.SourceTaskID == nil || strings.TrimSpace(*principal.SourceTaskID) != taskID {
			return errors.New("session token cannot attach files to a different task")
		}
		return nil
	case coordinator.TokenScopeWorker:
		return s.checkWorkerTaskLease(r, principal, taskID)
	default:
		return errors.New("attachment upload requires owner, session, or worker token")
	}
}

func (s *projectServer) checkWorkerTaskLease(r *http.Request, principal coordinator.Principal, taskID string) error {
	leaseID := strings.TrimSpace(r.URL.Query().Get("lease_id"))
	if leaseID == "" {
		return errAttachmentLeaseRequired
	}
	if err := s.sweepExpiredLeases(r.Context()); err != nil {
		return err
	}
	lease, err := s.workers.GetLease(r.Context(), leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if lease.WorkerID != strings.TrimSpace(principal.Subject) || lease.ReleasedAt != nil {
		return errWorkerLeaseForbidden
	}
	job, err := s.workers.GetJob(r.Context(), lease.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if job.TaskID == nil || strings.TrimSpace(*job.TaskID) != strings.TrimSpace(taskID) {
		return errAttachmentLeaseForbidden
	}

	return nil
}

var (
	errAttachmentLeaseRequired  = errors.New("lease_id is required for worker attachment uploads")
	errAttachmentLeaseForbidden = errors.New("lease does not belong to this task")
)

func writeAttachmentScopeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAttachmentLeaseRequired):
		writeError(w, http.StatusBadRequest, "lease_id_required", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "lease_not_found", "lease not found")
	case errors.Is(err, errWorkerLeaseForbidden), errors.Is(err, errAttachmentLeaseForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	default:
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	}
}

func attachmentActorForPrincipal(principal coordinator.Principal) coordinator.Actor {
	switch principal.Scope {
	case coordinator.TokenScopeSession, coordinator.TokenScopeWorker:
		return coordinator.ActorAgent
	default:
		return coordinator.ActorHuman
	}
}
