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

const issueAttachmentUploadLimit = coordinator.IssueAttachmentMaxBytes + (1 << 20)

func (s *projectServer) handleIssueAttachmentsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
				writeError(w, http.StatusForbidden, "forbidden", "attachment read requires owner, session, or worker token")
				return
			}
			s.handleListIssueAttachments(w, r, issueID)
		case http.MethodPost:
			if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeWorker) {
				writeError(w, http.StatusForbidden, "forbidden", "attachment upload requires owner, session, or worker token")
				return
			}
			if err := s.checkIssueAttachmentWriteScope(r, principal, issueID); err != nil {
				writeAttachmentScopeError(w, err)
				return
			}
			s.handleUploadIssueAttachment(w, r, principal, issueID)
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
		s.handleDownloadIssueAttachment(w, r, issueID, parts[0])
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}

func (s *projectServer) handleListIssueAttachments(w http.ResponseWriter, r *http.Request, issueID string) {
	if _, err := s.issues.GetIssue(r.Context(), issueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "issue_not_found", "issue not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "issue_lookup_failed", err.Error())
		return
	}
	attachments, err := s.issues.ListIssueAttachments(r.Context(), issueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attachments_list_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueAttachmentsResponse{Attachments: attachments})
}

func (s *projectServer) handleUploadIssueAttachment(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, issueID string) {
	if s.attachments == nil {
		writeError(w, http.StatusInternalServerError, "attachments_unavailable", "attachment store is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, issueAttachmentUploadLimit)
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

	attachment, err := s.issues.CreateIssueAttachment(r.Context(), coordinator.CreateIssueAttachmentInput{
		IssueID:     issueID,
		Stage:       coordinator.IssueAttachmentStage(r.FormValue("stage")),
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		CreatedBy:   attachmentActorForPrincipal(principal),
		Reader:      file,
	}, s.attachments)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "issue_not_found", "issue not found")
			return
		}
		writeError(w, http.StatusBadRequest, "attachment_upload_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, issueAttachmentResponse{Attachment: attachment})
}

func (s *projectServer) handleDownloadIssueAttachment(w http.ResponseWriter, r *http.Request, issueID string, attachmentID string) {
	if s.attachments == nil {
		writeError(w, http.StatusInternalServerError, "attachments_unavailable", "attachment store is not configured")
		return
	}
	attachment, err := s.issues.GetIssueAttachment(r.Context(), issueID, attachmentID)
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

	contentType, inlineSafe := issueAttachmentResponseContentType(attachment.ContentType)
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

func issueAttachmentResponseContentType(contentType string) (string, bool) {
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

func (s *projectServer) checkIssueAttachmentWriteScope(r *http.Request, principal coordinator.Principal, issueID string) error {
	issueID = strings.TrimSpace(issueID)
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return nil
	case coordinator.TokenScopeSession:
		if principal.SourceIssueID == nil || strings.TrimSpace(*principal.SourceIssueID) != issueID {
			return errors.New("session token cannot attach files to a different issue")
		}
		return nil
	case coordinator.TokenScopeWorker:
		return s.checkWorkerIssueLease(r, principal, issueID)
	default:
		return errors.New("attachment upload requires owner, session, or worker token")
	}
}

func (s *projectServer) checkWorkerIssueLease(r *http.Request, principal coordinator.Principal, issueID string) error {
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
	if job.IssueID == nil || strings.TrimSpace(*job.IssueID) != strings.TrimSpace(issueID) {
		return errAttachmentLeaseForbidden
	}

	return nil
}

var (
	errAttachmentLeaseRequired  = errors.New("lease_id is required for worker attachment uploads")
	errAttachmentLeaseForbidden = errors.New("lease does not belong to this issue")
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
