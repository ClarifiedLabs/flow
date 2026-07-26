package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	flowweb "github.com/ClarifiedLabs/flow/internal/web"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

const (
	nativeHookStateLoopWindow              = 30 * time.Second
	nativeHookStateLoopTransitionThreshold = 6
	nativeHookStateLoopTransitionLimit     = 20
	nativeHookStateLoopStatusMessage       = "Flow detected repeated native-hook session state changes and left the session waiting for human attention."
)

const (
	// terminalClipboardAssetName is the OSC 52 → browser clipboard bridge script
	// served from the terminal path and injected into ttyd's terminal page.
	terminalClipboardAssetName = "terminal-clipboard.js"
	// terminalClipboardScriptTagFormat is the <script> tag injected into ttyd's
	// terminal HTML page; %s is the terminal proxy base path (session or job).
	terminalClipboardScriptTagFormat = `<script src="%s/terminal-clipboard.js"></script>`
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
			http.Redirect(w, r, terminal.TerminalProxyPath(sessionID), http.StatusSeeOther)
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
		ps.handleSessionTerminalProxy(w, r, sessionID, suffix)
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
		http.Redirect(w, r, terminal.JobTerminalProxyPath(jobID), http.StatusSeeOther)
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
	ps.handleJobTerminalProxy(w, r, jobID, suffix)
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
			s.handleSessionTerminalProxy(w, r, sessionID, parts[2:])
		case http.MethodPost:
			if len(parts) != 2 {
				writeError(w, http.StatusNotFound, "not_found", "resource not found")
				return
			}
			if err := checkSessionTokenScope(principal, sessionID); err != nil {
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
	registered, err := s.sessions.RegisterTerminalTarget(r.Context(), sessionID, request.TargetURL, request.TmuxSocketPath)
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
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_access_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionTerminalAccessResponse{Access: access})
}

func (s *projectServer) handleSessionTerminalProxy(w http.ResponseWriter, r *http.Request, sessionID string, suffix []string) {
	if len(suffix) == 1 && suffix[0] == terminalClipboardAssetName {
		s.serveTerminalClipboardAsset(w)
		return
	}
	registered, err := s.sessions.TerminalTarget(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_proxy_failed", err.Error())
		return
	}
	s.proxyTerminalTarget(w, r, registered.TargetURL, suffix, fmt.Sprintf(terminalClipboardScriptTagFormat, terminal.TerminalProxyPath(sessionID)))
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
	registered, err := s.sessions.RegisterJobTerminalTarget(r.Context(), jobID, request.LeaseID, request.TargetURL, request.TmuxSocketPath)
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
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_access_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, jobTerminalAccessResponse{Access: access})
}

func (s *projectServer) handleJobTerminalProxy(w http.ResponseWriter, r *http.Request, jobID string, suffix []string) {
	if len(suffix) == 1 && suffix[0] == terminalClipboardAssetName {
		s.serveTerminalClipboardAsset(w)
		return
	}
	registered, err := s.sessions.JobTerminalTarget(r.Context(), jobID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "terminal_not_found", "terminal target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_proxy_failed", err.Error())
		return
	}
	s.proxyTerminalTarget(w, r, registered.TargetURL, suffix, fmt.Sprintf(terminalClipboardScriptTagFormat, terminal.JobTerminalProxyPath(jobID)))
}

// serveTerminalClipboardAsset serves the OSC 52 → browser clipboard bridge
// script from the terminal path so the injected <script src> is same-origin
// with ttyd's page. It sits behind the terminal access cookie / owner-token
// auth already enforced on the terminal path.
func (s *projectServer) serveTerminalClipboardAsset(w http.ResponseWriter) {
	contents, contentType, ok := flowweb.Asset(terminalClipboardAssetName)
	if !ok {
		writeError(w, http.StatusNotFound, "terminal_asset_not_found", "terminal clipboard asset not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", terminalSandboxCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(contents)
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
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(transcript)
}

func (s *Server) proxyTerminalTarget(w http.ResponseWriter, r *http.Request, targetURL string, suffix []string, clipboardScriptTag string) {
	target, err := url.Parse(targetURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "terminal_proxy_failed", err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			incomingQuery := request.URL.RawQuery
			request.URL.Scheme = target.Scheme
			request.URL.Host = target.Host
			request.URL.Path = terminalProxyPath(target.Path, suffix)
			request.URL.RawPath = ""
			if target.RawQuery == "" {
				request.URL.RawQuery = incomingQuery
			} else if incomingQuery == "" {
				request.URL.RawQuery = target.RawQuery
			} else {
				request.URL.RawQuery = target.RawQuery + "&" + incomingQuery
			}
			request.Host = target.Host
			request.Header.Del("Authorization")
			request.Header.Del(protocolHeader)
			// The terminal HTML page is rewritten below (a clipboard-bridge
			// <script> is injected), so strip the conditional and encoding
			// request headers that would otherwise let the browser or upstream
			// serve a cached, un-injected copy or a body we cannot rewrite.
			request.Header.Del("Accept-Encoding")
			request.Header.Del("If-None-Match")
			request.Header.Del("If-Modified-Since")
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Set("Content-Security-Policy", terminalSandboxCSP)
			// Only rewrite the terminal HTML page itself: status 200, an HTML
			// content type, and no upstream encoding we would have to decode
			// first (Accept-Encoding is stripped in the Director, so this is a
			// belt-and-braces guard). Non-HTML responses — /pty (WebSocket 101),
			// /auth_token, and ttyd static assets — pass through untouched.
			if clipboardScriptTag != "" &&
				response.StatusCode == http.StatusOK &&
				strings.Contains(response.Header.Get("Content-Type"), "text/html") &&
				response.Header.Get("Content-Encoding") == "" {
				if err := injectTerminalClipboardScript(response, clipboardScriptTag); err != nil {
					return err
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "terminal_proxy_failed", err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// injectTerminalClipboardScript appends the OSC 52 → browser clipboard bridge
// <script> to ttyd's terminal HTML page. ttyd serves exactly one HTML page (the
// terminal UI), so the text/html content-type guard in ModifyResponse is enough
// to leave /pty (WebSocket 101), /auth_token, and static assets untouched. The
// body length and validators change, so Content-Length is recomputed, ETag and
// Last-Modified are dropped, and the page is marked no-store so browsers never
// serve a cached, un-injected copy.
func injectTerminalClipboardScript(response *http.Response, scriptTag string) error {
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	// Insert immediately before </body> (case-insensitive); append at the end if
	// the tag is absent.
	injected := insertBeforeClosingBody(body, scriptTag)
	response.Body = io.NopCloser(bytes.NewReader(injected))
	response.ContentLength = int64(len(injected))
	response.Header.Set("Content-Length", strconv.Itoa(len(injected)))
	response.Header.Del("ETag")
	response.Header.Del("Last-Modified")
	response.Header.Set("Cache-Control", "no-store")
	return nil
}

// insertBeforeClosingBody returns body with scriptTag inserted immediately
// before the first case-insensitive </body> occurrence, or appended if none is
// present.
func insertBeforeClosingBody(body []byte, scriptTag string) []byte {
	lower := bytes.ToLower(body)
	marker := []byte("</body>")
	index := bytes.Index(lower, marker)
	if index < 0 {
		return append(append([]byte{}, body...), scriptTag...)
	}
	out := make([]byte, 0, len(body)+len(scriptTag))
	out = append(out, body[:index]...)
	out = append(out, scriptTag...)
	out = append(out, body[index:]...)
	return out
}

func terminalProxyPath(base string, suffix []string) string {
	cleanBase := "/" + strings.Trim(strings.TrimSpace(base), "/")
	cleanSuffix := strings.Trim(strings.Join(suffix, "/"), "/")
	if cleanSuffix == "" {
		return cleanBase
	}
	if cleanBase == "/" {
		return "/" + cleanSuffix
	}

	return cleanBase + "/" + cleanSuffix
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
