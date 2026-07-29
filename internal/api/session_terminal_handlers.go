package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	flowweb "github.com/ClarifiedLabs/flow/internal/web"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	"github.com/coder/websocket"
)

const (
	nativeHookStateLoopWindow              = 30 * time.Second
	nativeHookStateLoopTransitionThreshold = 6
	nativeHookStateLoopTransitionLimit     = 20
	nativeHookStateLoopStatusMessage       = "Flow detected repeated native-hook session state changes and left the session waiting for human attention."
)

const (
	terminalHTMLAssetName = "terminal.html"
)

func (s *Server) serveTerminalBrowserRequest(w http.ResponseWriter, r *http.Request) bool {
	// Terminal pages authenticate with their own access tokens/cookies, so
	// the owning project is resolved purely from the (random, unique)
	// session or job id.
	scanPrincipal := coordinator.Principal{Scope: coordinator.TokenScopeOwner}
	sessionID, kind, suffix, ok := parseSessionTerminalPath(r.URL.Path)
	if ok {
		ps, found := s.bundleForSession(r.Context(), scanPrincipal, sessionID)
		if !found {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return true
		}
		if kind == "terminal-login" {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
				return true
			}
			token := strings.TrimSpace(r.URL.Query().Get("token"))
			if err := ps.sessions.ValidateTerminalAccess(r.Context(), sessionID, token); err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return true
			}
			http.SetCookie(w, &http.Cookie{
				Name:     terminalAccessCookie,
				Value:    token,
				Path:     terminal.TerminalProxyPath(sessionID),
				MaxAge:   int(defaultTerminalAccessTTL.Seconds()),
				HttpOnly: true,
				Secure:   r.TLS != nil,
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, terminal.TerminalProxyPath(sessionID)+"/", http.StatusSeeOther)
			return true
		}
		if kind != "terminal" || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			return false
		}
		cookie, err := r.Cookie(terminalAccessCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing terminal access cookie")
			return true
		}
		if err := ps.sessions.ValidateTerminalAccess(r.Context(), sessionID, cookie.Value); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return true
		}
		ps.handleSessionTerminalBrowser(w, r, sessionID, suffix)
		return true
	}

	jobID, kind, suffix, ok := parseJobTerminalPath(r.URL.Path)
	if !ok {
		return false
	}
	ps, found := s.bundleForJob(r.Context(), scanPrincipal, jobID)
	if !found {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return true
	}
	if kind == "terminal-login" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return true
		}
		token := strings.TrimSpace(r.URL.Query().Get("token"))
		if err := ps.sessions.ValidateJobTerminalAccess(r.Context(), jobID, token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return true
		}
		http.SetCookie(w, &http.Cookie{
			Name:     terminalAccessCookie,
			Value:    token,
			Path:     terminal.JobTerminalProxyPath(jobID),
			MaxAge:   int(defaultTerminalAccessTTL.Seconds()),
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, terminal.JobTerminalProxyPath(jobID)+"/", http.StatusSeeOther)
		return true
	}
	if kind != "terminal" || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		return false
	}
	cookie, err := r.Cookie(terminalAccessCookie)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing terminal access cookie")
		return true
	}
	if err := ps.sessions.ValidateJobTerminalAccess(r.Context(), jobID, cookie.Value); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return true
	}
	ps.handleJobTerminalBrowser(w, r, jobID, suffix)
	return true
}

// parseTerminalPath parses a /{prefix}{id}/terminal[-login]/... path into its
// id, the terminal verb, and any trailing proxy path segments.
func parseTerminalPath(path, prefix string) (string, string, []string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", "", nil, false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		return "", "", nil, false
	}
	if parts[1] != "terminal" && parts[1] != "terminal-login" {
		return "", "", nil, false
	}

	return strings.TrimSpace(parts[0]), parts[1], parts[2:], true
}

func parseSessionTerminalPath(path string) (string, string, []string, bool) {
	return parseTerminalPath(path, "/v2/sessions/")
}

func parseJobTerminalPath(path string) (string, string, []string, bool) {
	return parseTerminalPath(path, "/v2/jobs/")
}

func (s *projectServer) handleSessionPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if s.sessions == nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "session service is not configured")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/sessions/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	sessionID := strings.TrimSpace(parts[0])
	if parts[1] == "attach" {
		if len(parts) != 2 {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleSessionAttach(w, r, sessionID)
		return
	}
	if parts[1] == "terminal" {
		switch r.Method {
		case http.MethodGet:
			if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
				return
			}
			s.handleSessionTerminalBrowser(w, r, sessionID, parts[2:])
		case http.MethodPost:
			if len(parts) != 2 {
				writeError(w, http.StatusNotFound, "not_found", "resource not found")
				return
			}
			if err := checkSessionScope(principal, sessionID); err != nil {
				writeError(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
			s.handleSessionTerminalRegister(w, r, sessionID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if parts[1] == "terminal-token" {
		if len(parts) != 2 {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		s.handleSessionTerminalAccess(w, r, sessionID)
		return
	}
	if parts[1] == "transcript" {
		if len(parts) != 2 {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		switch r.Method {
		case http.MethodPut:
			// A session token may upload only its own transcript; owner tokens
			// may upload on behalf of any session.
			if err := checkSessionScope(principal, sessionID); err != nil {
				writeError(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
			s.handleSessionTranscriptUpload(w, r, sessionID)
		case http.MethodGet:
			if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
				return
			}
			s.handleSessionTranscriptDownload(w, r, sessionID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if parts[1] == "messages" {
		if !requireScope(w, principal, "worker token is required", coordinator.TokenScopeWorker) {
			return
		}
		if len(parts) == 2 && r.Method == http.MethodGet {
			s.handleSessionMessages(w, r, sessionID)
			return
		}
		if len(parts) == 4 && parts[3] == "delivered" && r.Method == http.MethodPost {
			s.handleSessionMessageDelivered(w, r, sessionID, parts[2])
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if parts[1] == "process-exit" {
		if len(parts) != 2 {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireScope(w, principal, "worker token is required", coordinator.TokenScopeWorker) {
			return
		}
		s.handleSessionProcessExit(w, r, sessionID)
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := checkSessionScope(principal, sessionID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}

	switch parts[1] {
	case "event":
		s.handleSessionEvent(w, r, sessionID, principal)
	case "signal":
		s.handleSessionSignal(w, r, sessionID, principal)
	case "status":
		s.handleSessionStatus(w, r, sessionID, principal)
	case "ready":
		currentSession, err := s.sessions.GetSession(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ready_session_failed", err.Error())
			return
		}
		if currentSession.WorkflowRunID != "" {
			writeError(w, http.StatusConflict, "workflow_conflict", "workflow sessions finish with flow complete")
			return
		}
		if currentSession.Role == flowworker.RoleConsole {
			writeError(w, http.StatusBadRequest, "ready_session_failed", "Console sessions are released with /v2/console")
			return
		}
		var request readySessionRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if changeID := strings.TrimSpace(currentSession.ChangeID); changeID != "" {
			if headSHA := strings.TrimSpace(request.HeadSHA); headSHA != "" {
				if _, err := s.sessions.UpdateChangeHead(r.Context(), changeID, headSHA); err != nil {
					writeError(w, http.StatusBadRequest, "ready_session_failed", err.Error())
					return
				}
			}
			if _, err := s.sessions.ReadyChange(r.Context(), changeID); err != nil {
				writeError(w, http.StatusBadRequest, "ready_session_failed", err.Error())
				return
			}
		}
		session, err := s.sessions.ReadyAuthorSession(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ready_session_failed", err.Error())
			return
		}
		s.touchAgentActivity(r.Context(), sessionID)
		writeJSON(w, http.StatusOK, sessionResponse{Session: session})
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (s *projectServer) handleSessionAttach(w http.ResponseWriter, r *http.Request, sessionID string) {
	info, err := s.sessions.AttachInfo(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "attach_session_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, attachResponse{Attach: info})
}

func (s *projectServer) handleSessionTerminalRegister(w http.ResponseWriter, r *http.Request, sessionID string) {
	var request sessionTerminalRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	registered, err := s.sessions.RegisterTerminal(r.Context(), sessionID, request.TmuxSocketPath)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "register_terminal_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionTerminalResponse{Terminal: registered})
}

func (s *projectServer) handleSessionTerminalAccess(w http.ResponseWriter, r *http.Request, sessionID string) {
	access, err := s.sessions.CreateTerminalAccess(r.Context(), sessionID, defaultTerminalAccessTTL)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_access_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionTerminalAccessResponse{Access: access})
}

func (s *projectServer) handleSessionTerminalBrowser(w http.ResponseWriter, r *http.Request, sessionID string, suffix []string) {
	if isWebSocketUpgrade(r) {
		s.handleSessionTerminalStream(w, r, sessionID)
		return
	}
	if len(suffix) == 0 || (len(suffix) == 1 && suffix[0] == "") {
		s.serveTerminalPage(w, r, terminal.TerminalProxyPath(sessionID))
		return
	}
	s.serveTerminalAsset(w, suffix)
}

func (s *projectServer) handleJobTerminalRegister(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, jobID string) {
	var request jobTerminalRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := s.checkLeaseOwner(r, principal, request.LeaseID); err != nil {
		writeLeaseAuthError(w, err)
		return
	}
	registered, err := s.sessions.RegisterJobTerminal(r.Context(), jobID, request.LeaseID, request.TmuxSocketPath)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "register_terminal_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, jobTerminalResponse{Terminal: registered})
}

func (s *projectServer) handleJobTerminalAccess(w http.ResponseWriter, r *http.Request, jobID string) {
	access, err := s.sessions.CreateJobTerminalAccess(r.Context(), jobID, defaultTerminalAccessTTL)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_access_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, jobTerminalAccessResponse{Access: access})
}

func (s *projectServer) handleJobTerminalBrowser(w http.ResponseWriter, r *http.Request, jobID string, suffix []string) {
	if isWebSocketUpgrade(r) {
		s.handleJobTerminalStream(w, r, jobID)
		return
	}
	if len(suffix) == 0 || (len(suffix) == 1 && suffix[0] == "") {
		s.serveTerminalPage(w, r, terminal.JobTerminalProxyPath(jobID))
		return
	}
	s.serveTerminalAsset(w, suffix)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (s *projectServer) serveTerminalPage(w http.ResponseWriter, r *http.Request, wsPath string) {
	contents, contentType, ok := flowweb.Asset(terminalHTMLAssetName)
	if !ok {
		writeError(w, http.StatusNotFound, "terminal_page_not_found", "terminal page not found")
		return
	}
	wsURL := websocketURL(r, wsPath)
	scriptTag := `<script id="terminal-page-data" data-ws-url="` + html.EscapeString(wsURL) + `"></script>`
	rendered := strings.Replace(string(contents), "<!--TERMINAL_PAGE_DATA-->", scriptTag, 1)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", terminalSandboxCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(rendered))
}

func websocketURL(r *http.Request, path string) string {
	scheme := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host + path
}

func (s *projectServer) serveTerminalAsset(w http.ResponseWriter, suffix []string) {
	name := strings.Join(suffix, "/")
	if name == "" || strings.Contains(name, "..") {
		writeError(w, http.StatusNotFound, "terminal_asset_not_found", "terminal asset not found")
		return
	}
	contents, contentType, ok := flowweb.Asset(name)
	if !ok {
		writeError(w, http.StatusNotFound, "terminal_asset_not_found", "terminal asset not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", terminalSandboxCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(contents)
}

func (s *projectServer) handleSessionTerminalStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx := r.Context()
	registered, err := s.sessions.TerminalTarget(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal not registered")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", err.Error())
		return
	}
	session, err := s.sessions.GetSession(ctx, sessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", err.Error())
		return
	}
	lease, err := s.workers.GetLease(ctx, session.LeaseID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", err.Error())
		return
	}
	if lease.ReleasedAt != nil || time.Now().After(lease.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "terminal_not_live", "terminal is not live")
		return
	}
	s.openTerminalStream(w, r, session.JobID, lease.WorkerID, registered.TmuxSocketPath)
}

func (s *projectServer) handleJobTerminalStream(w http.ResponseWriter, r *http.Request, jobID string) {
	ctx := r.Context()
	registered, err := s.sessions.JobTerminalTarget(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal not registered")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", err.Error())
		return
	}
	lease, err := s.workers.GetLease(ctx, registered.LeaseID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", err.Error())
		return
	}
	if lease.ReleasedAt != nil || time.Now().After(lease.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "terminal_not_live", "terminal is not live")
		return
	}
	s.openTerminalStream(w, r, jobID, lease.WorkerID, registered.TmuxSocketPath)
}

func (s *projectServer) openTerminalStream(w http.ResponseWriter, r *http.Request, jobID, workerID, tmuxSocketPath string) {
	tmuxSocketPath = strings.TrimSpace(tmuxSocketPath)
	if tmuxSocketPath == "" {
		writeError(w, http.StatusBadRequest, "terminal_stream_failed", "tmux socket path is required")
		return
	}
	browserConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		slog.WarnContext(r.Context(), "accept browser terminal WebSocket", "error", err)
		return
	}

	cols, rows := uint16(80), uint16(24)
	if msgType, data, err := browserConn.Read(r.Context()); err == nil && msgType == websocket.MessageText {
		var attach terminalAttachMessage
		if err := json.Unmarshal(data, &attach); err == nil && attach.Type == "attach" {
			if attach.Cols > 0 {
				cols = attach.Cols
			}
			if attach.Rows > 0 {
				rows = attach.Rows
			}
		}
	}

	streamID, err := coordinator.NewTerminalStreamID()
	if err != nil {
		_ = browserConn.Close(websocket.StatusInternalError, "failed to allocate stream")
		return
	}

	if err := s.Server.workerTerminals.OpenStream(streamID, workerID, browserConn, jobID, tmuxSocketPath, cols, rows); err != nil {
		slog.WarnContext(r.Context(), "open terminal stream", "stream_id", streamID, "worker_id", workerID, "error", err)
		_ = browserConn.Close(websocket.StatusPolicyViolation, err.Error())
	}
}

type terminalAttachMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// transcriptUploadLimit bounds the request body the coordinator will read for
// a transcript upload. The store keeps only the last 10MB, so a generous cap
// above that absorbs a worker that uploads slightly more than the tail.
const transcriptUploadLimit = 12 << 20 // 12 MiB

func (s *projectServer) handleSessionTranscriptUpload(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.transcripts == nil {
		writeError(w, http.StatusInternalServerError, "transcripts_unavailable", "transcript store is not configured")
		return
	}
	if _, err := s.sessions.GetSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
		return
	}

	body := http.MaxBytesReader(w, r.Body, transcriptUploadLimit)
	path, err := s.transcripts.Save(sessionID, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "transcript_upload_failed", err.Error())
		return
	}
	if err := s.sessions.SetSessionTranscriptPath(r.Context(), sessionID, path); err != nil {
		writeError(w, http.StatusInternalServerError, "transcript_record_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *projectServer) handleSessionTranscriptDownload(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.transcripts == nil {
		writeError(w, http.StatusInternalServerError, "transcripts_unavailable", "transcript store is not configured")
		return
	}
	if _, err := s.sessions.GetSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
		return
	}
	s.serveTranscript(w, sessionID)
}

func (s *projectServer) handleJobTranscriptUpload(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, jobID string) {
	if s.transcripts == nil {
		writeError(w, http.StatusInternalServerError, "transcripts_unavailable", "transcript store is not configured")
		return
	}
	if _, err := s.workers.GetJob(r.Context(), jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job_not_found", "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "job_lookup_failed", err.Error())
		return
	}
	// Worker tokens must hold a live lease on this job; owner tokens skip the
	// lease check.
	if principal.Scope == coordinator.TokenScopeWorker {
		leaseID := strings.TrimSpace(r.URL.Query().Get("lease_id"))
		if leaseID == "" {
			writeError(w, http.StatusBadRequest, "lease_id_required", "worker transcript uploads require lease_id")
			return
		}
		if err := s.checkLiveJobLease(r, principal, jobID, leaseID); err != nil {
			writeLeaseAuthError(w, err)
			return
		}
	}

	body := http.MaxBytesReader(w, r.Body, transcriptUploadLimit)
	path, err := s.transcripts.Save(jobID, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "transcript_upload_failed", err.Error())
		return
	}
	if err := s.workers.SetJobTranscriptPath(r.Context(), jobID, path); err != nil {
		writeError(w, http.StatusInternalServerError, "transcript_record_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *projectServer) handleJobTranscriptDownload(w http.ResponseWriter, r *http.Request, jobID string) {
	if s.transcripts == nil {
		writeError(w, http.StatusInternalServerError, "transcripts_unavailable", "transcript store is not configured")
		return
	}
	if _, err := s.workers.GetJob(r.Context(), jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job_not_found", "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "job_lookup_failed", err.Error())
		return
	}
	s.serveTranscript(w, jobID)
}

// checkLiveJobLease verifies the worker principal holds the named, still-live
// lease and that the lease is for the target job — the upload counterpart of
// checkReportScope's worker branch.
func (s *projectServer) checkLiveJobLease(r *http.Request, principal coordinator.Principal, jobID string, leaseID string) error {
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
	if lease.WorkerID != strings.TrimSpace(principal.Subject) || lease.JobID != jobID || lease.ReleasedAt != nil {
		return errWorkerLeaseForbidden
	}

	return nil
}

func (s *projectServer) serveTranscript(w http.ResponseWriter, id string) {
	reader, err := s.transcripts.Open(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "transcript_not_found", "transcript not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "transcript_read_failed", err.Error())
		return
	}
	defer reader.Close()

	transcript, err := terminal.NormalizeTranscript(reader)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "transcript_read_failed", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(transcript)
}
func (s *projectServer) handleSessionEvent(w http.ResponseWriter, r *http.Request, sessionID string, principal coordinator.Principal) {
	var request sessionEventRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	state := coordinator.SessionRuntimeState(strings.TrimSpace(request.State))
	switch state {
	case coordinator.SessionWorking, coordinator.SessionWaiting:
	default:
		writeError(w, http.StatusBadRequest, "invalid_session_state", "session event state must be working or waiting")
		return
	}
	source := strings.TrimSpace(request.Source)
	if !validSessionSignalSource(source) {
		writeError(w, http.StatusBadRequest, "invalid_session_source", "session event source must be empty, watchdog, or native_hook")
		return
	}

	s.applySessionStateSignal(w, r, sessionID, principal, state, source, "session_event_failed")
}

func (s *projectServer) handleSessionSignal(w http.ResponseWriter, r *http.Request, sessionID string, principal coordinator.Principal) {
	var request sessionSignalRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	signal := coordinator.SessionSignalKind(strings.TrimSpace(request.Signal))
	switch signal {
	case coordinator.SessionSignalWorking, coordinator.SessionSignalWaiting, coordinator.SessionSignalActivity:
	default:
		writeError(w, http.StatusBadRequest, "invalid_session_signal", "session signal must be working, waiting, or activity")
		return
	}
	source := strings.TrimSpace(request.Source)
	if !validSessionSignalSource(source) {
		writeError(w, http.StatusBadRequest, "invalid_session_source", "session signal source must be empty, watchdog, or native_hook")
		return
	}

	s.applySessionSignal(w, r, sessionID, principal, signal, source, "session_signal_failed")
}

func validSessionSignalSource(source string) bool {
	switch source {
	case "", coordinator.SessionEventSourceWatchdog, coordinator.SessionEventSourceNativeHook:
		return true
	default:
		return false
	}
}

func (s *projectServer) applySessionSignal(w http.ResponseWriter, r *http.Request, sessionID string, principal coordinator.Principal, signal coordinator.SessionSignalKind, source string, failureCode string) {
	switch signal {
	case coordinator.SessionSignalActivity:
		s.touchAgentActivity(r.Context(), sessionID)
		session, err := s.sessions.GetSession(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusBadRequest, failureCode, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sessionResponse{Session: session})
	case coordinator.SessionSignalWorking:
		s.applySessionStateSignal(w, r, sessionID, principal, coordinator.SessionWorking, source, failureCode)
	case coordinator.SessionSignalWaiting:
		s.applySessionStateSignal(w, r, sessionID, principal, coordinator.SessionWaiting, source, failureCode)
	default:
		writeError(w, http.StatusBadRequest, "invalid_session_signal", "session signal must be working, waiting, or activity")
	}
}

func (s *projectServer) applySessionStateSignal(w http.ResponseWriter, r *http.Request, sessionID string, principal coordinator.Principal, state coordinator.SessionRuntimeState, source string, failureCode string) {
	// The watchdog re-reports the same state every poll cycle; without this
	// fast path the engine would log a session_state_changed transition per
	// poll. A no-op same-state report returns the session unchanged.
	session, err := s.sessions.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, failureCode, err.Error())
		return
	}
	if session.RuntimeState == state {
		// Even a repeated same-state report proves the agent is alive, so it
		// counts as agent activity.
		s.touchAgentActivity(r.Context(), sessionID)
		writeJSON(w, http.StatusOK, sessionResponse{Session: session})
		return
	}
	if session.Role == flowworker.RoleConsole {
		updated, err := s.sessions.UpdateConsoleSessionState(r.Context(), sessionID, state)
		if err != nil {
			writeError(w, http.StatusBadRequest, failureCode, err.Error())
			return
		}
		s.touchAgentActivity(r.Context(), sessionID)
		writeJSON(w, http.StatusOK, sessionResponse{Session: updated})
		return
	}
	if session.WorkflowRunID != "" {
		updated, err := s.sessions.UpdateSessionState(r.Context(), sessionID, state)
		if err != nil {
			writeError(w, http.StatusBadRequest, failureCode, err.Error())
			return
		}
		s.touchAgentActivity(r.Context(), sessionID)
		writeJSON(w, http.StatusOK, sessionResponse{Session: updated})
		return
	}

	updated, err := s.sessions.UpdateSessionState(r.Context(), sessionID, state)
	if err != nil {
		writeError(w, http.StatusBadRequest, failureCode, err.Error())
		return
	}
	s.touchAgentActivity(r.Context(), sessionID)
	writeJSON(w, http.StatusOK, sessionResponse{Session: updated})
}


// touchAgentActivity records agent-level liveness best-effort: a failure to
// stamp last_agent_activity_at is logged and swallowed so it never fails the
// user request. The worker lease heartbeat is the durable liveness signal; this
// column is an advisory progress marker.
func (s *projectServer) touchAgentActivity(ctx context.Context, sessionID string) {
	if s.sessions == nil {
		return
	}
	if err := s.sessions.TouchAgentActivity(ctx, sessionID); err != nil {
		slog.Warn("touch agent activity failed", "session_id", sessionID, "error", err)
	}
}

func (s *projectServer) handleSessionStatus(w http.ResponseWriter, r *http.Request, sessionID string, principal coordinator.Principal) {
	if s.status == nil {
		writeError(w, http.StatusInternalServerError, "status_unavailable", "status service is not configured")
		return
	}
	session, err := s.sessions.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "status_failed", err.Error())
		return
	}
	if session.Role == flowworker.RoleConsole {
		writeError(w, http.StatusBadRequest, "status_failed", "flow status is task-scoped and unsupported in Console")
		return
	}

	var request sessionStatusRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	entry, err := s.status.WriteSessionStatus(r.Context(), sessionID, request.Message, principal.Actor(), request.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, "status_failed", err.Error())
		return
	}
	if statusKindAwaitsHuman(entry.Kind) {
		if session.WorkflowRunID != "" && session.NodeRunID != "" && s.workflowRuns != nil {
			err = s.workflowRuns.RequestAgentInput(r.Context(), session.NodeRunID, entry.Message, coordinator.ActorAgent)
		} else {
			err = s.awaitHumanForSessionStatus(r, sessionID, principal, entry.Kind)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "status_session_event_failed", err.Error())
			return
		}
	}
	s.touchAgentActivity(r.Context(), sessionID)

	writeJSON(w, http.StatusOK, statusResponse{Status: entry})
}

func (s *projectServer) handleSessionProcessExit(w http.ResponseWriter, r *http.Request, sessionID string) {
	var request sessionProcessExitRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	session, err := s.sessions.MarkPersistentSessionExited(r.Context(), coordinator.MarkPersistentSessionExitedInput{
		SessionID: sessionID,
		LeaseID:   request.LeaseID,
		ExitCode:  request.ExitCode,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "process_exit_failed", err.Error())
		return
	}
	if session.WorkflowRunID != "" && s.workflowExecutor != nil {
		if err := s.workflowExecutor.Advance(r.Context(), session.WorkflowRunID); err != nil {
			writeError(w, http.StatusBadRequest, "process_exit_workflow_failed", err.Error())
			return
		}
	} else if session.Role == flowworker.RoleAuthor {
		if _, err := s.sessions.ReconcileCrashedAuthorSessions(r.Context()); err != nil {
			writeError(w, http.StatusBadRequest, "process_exit_reconcile_failed", err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, sessionResponse{Session: session})
}

func (s *projectServer) handleSessionMessages(w http.ResponseWriter, r *http.Request, sessionID string) {
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer")
			return
		}
		limit = parsed
	}
	messages, err := s.sessions.ListPendingSessionMessages(r.Context(), coordinator.ListPendingSessionMessagesInput{
		SessionID: sessionID,
		LeaseID:   r.URL.Query().Get("lease_id"),
		Limit:     limit,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "list_session_messages_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionMessagesResponse{Messages: messages})
}

func (s *projectServer) handleSessionMessageDelivered(w http.ResponseWriter, r *http.Request, sessionID string, messageID string) {
	var request sessionMessageDeliveredRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	message, err := s.sessions.MarkSessionMessageDelivered(r.Context(), coordinator.MarkSessionMessageDeliveredInput{
		SessionID: sessionID,
		MessageID: messageID,
		LeaseID:   request.LeaseID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "deliver_session_message_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionMessageResponse{Message: message, Queued: false})
}

func statusKindAwaitsHuman(kind string) bool {
	switch strings.TrimSpace(kind) {
	case coordinator.StatusKindPlan, coordinator.StatusKindQuestion, coordinator.StatusKindBlocker:
		return true
	default:
		return false
	}
}

func (s *projectServer) awaitHumanForSessionStatus(r *http.Request, sessionID string, principal coordinator.Principal, kind string) error {
	if s.sessions == nil {
		return errors.New("session service is not configured")
	}
	session, err := s.sessions.GetSession(r.Context(), sessionID)
	if err != nil {
		return err
	}
	switch session.RuntimeState {
	case coordinator.SessionWaiting:
		return s.sessions.ProtectHumanWaitFromWatchdog(r.Context(), sessionID, kind)
	case coordinator.SessionStarting, coordinator.SessionWorking:
	default:
		return nil
	}
	if _, err := s.sessions.UpdateSessionState(r.Context(), sessionID, coordinator.SessionWaiting); err != nil {
		return err
	}
	return s.sessions.ProtectHumanWaitFromWatchdog(r.Context(), sessionID, kind)
}
