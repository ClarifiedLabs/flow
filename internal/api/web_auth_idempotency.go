package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowweb "github.com/ClarifiedLabs/flow/internal/web"
)

func (s *Server) serveWebAPIRequest(w http.ResponseWriter, r *http.Request) bool {
	if !isWebAPIPath(r.URL.Path) {
		return false
	}

	apiPath := strings.TrimPrefix(r.URL.Path, webAPIPrefix)
	if !strings.HasPrefix(apiPath, "/v1/") {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return true
	}

	principal, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return true
	}

	apiRequest := requestWithPath(r, apiPath)
	if s.shouldUseIdempotency(apiRequest, principal) {
		s.serveIdempotent(w, apiRequest, principal)
		return true
	}

	s.dispatch(w, apiRequest, principal)
	return true
}

func (s *Server) serveWebRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/ui" && !strings.HasPrefix(r.URL.Path, "/ui/") {
		return false
	}
	if r.URL.Path == "/ui/login" {
		s.handleWebLogin(w, r)
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/ui/assets/") {
		s.handleWebAsset(w, r)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return true
	}
	if r.URL.Path == "/ui" {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
		return true
	}
	contents, err := flowweb.IndexHTML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "web_unavailable", err.Error())
		return true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return true
	}
	_, _ = w.Write(contents)
	return true
}

func (s *Server) handleWebAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/assets/")
	contents, contentType, ok := flowweb.Asset(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	if r.URL.Query().Get("v") != "" {
		// Versioned entry assets (index.html loads app.js/app.css with ?v=HASH)
		// carry their cache key in the URL, so they can be cached immutably.
		w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	} else {
		// Unversioned requests — notably the browser's native ES module imports
		// (import "./markdown.js") — must revalidate via ETag so an edited module
		// is never served stale from an immutable cache.
		etag := flowweb.AssetETag(contents)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(contents)
}

func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if s.webSessions == nil {
		writeError(w, http.StatusInternalServerError, "web_sessions_unavailable", "web sessions are not configured")
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	session, err := s.webSessions.ConsumeBootstrap(r.Context(), token, defaultWebSessionTTL)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	cookieMaxAge := int(defaultWebSessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookie,
		Value:    session.Token,
		Path:     "/ui",
		MaxAge:   cookieMaxAge,
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     webCSRFCookie,
		Value:    session.CSRFToken,
		Path:     "/ui",
		MaxAge:   cookieMaxAge,
		Expires:  session.ExpiresAt,
		HttpOnly: false,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) handleWebBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.webSessions == nil {
		writeError(w, http.StatusInternalServerError, "web_sessions_unavailable", "web sessions are not configured")
		return
	}
	bootstrap, err := s.webSessions.CreateBootstrap(r.Context(), defaultWebBootstrapTTL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "web_bootstrap_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, webBootstrapResponse{
		LoginPath: webLoginPath(bootstrap.Token),
		ExpiresAt: bootstrap.ExpiresAt,
	})
}

func webLoginPath(token string) string {
	query := url.Values{}
	query.Set("token", token)
	return "/ui/login?" + query.Encode()
}

func (s *Server) checkProtocol(r *http.Request) error {
	requested := strings.TrimSpace(r.Header.Get(protocolHeader))
	if requested == "" || requested == s.protocolVersion {
		return nil
	}

	return fmt.Errorf("client protocol %s is not supported; server protocol is %s", requested, s.protocolVersion)
}

func (s *Server) authenticate(r *http.Request) (coordinator.Principal, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, authScheme) {
		if header == "" && s.webSessions != nil && isWebAPIPath(r.URL.Path) {
			cookie, err := r.Cookie(webSessionCookie)
			if err == nil {
				csrfToken := strings.TrimSpace(r.Header.Get(webCSRFHeader))
				if csrfToken == "" {
					csrfToken = strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
				}
				session, err := s.webSessions.Authenticate(r.Context(), cookie.Value, csrfToken, webAPIRequiresCSRF(r))
				if err != nil {
					return coordinator.Principal{}, err
				}
				return coordinator.Principal{
					Scope:        coordinator.TokenScopeOwner,
					Subject:      "web",
					TokenHash:    coordinator.HashToken(cookie.Value),
					WebSessionID: session.ID,
				}, nil
			}
		}
		return coordinator.Principal{}, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, authScheme))
	if token == "" {
		return coordinator.Principal{}, errors.New("missing bearer token")
	}

	return s.authenticateToken(r.Context(), token)
}

func (s *Server) authenticateToken(ctx context.Context, token string) (coordinator.Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return coordinator.Principal{}, errors.New("missing bearer token")
	}

	if s.credentials != nil {
		principal, err := s.credentials.Authenticate(ctx, token)
		if err == nil {
			return principal, nil
		}
		return coordinator.Principal{}, err
	}

	if tokenMatches(token, s.ownerToken) {
		return coordinator.Principal{
			Scope:     coordinator.TokenScopeOwner,
			Subject:   defaultOwnerSubject,
			TokenHash: coordinator.HashToken(token),
		}, nil
	}
	if tokenMatches(token, s.hookToken) {
		return coordinator.Principal{
			Scope:     coordinator.TokenScopeHook,
			Subject:   defaultHookSubject,
			TokenHash: coordinator.HashToken(token),
		}, nil
	}

	return coordinator.Principal{}, errors.New("invalid bearer token")
}

func isWebAPIPath(path string) bool {
	return path == webAPIPrefix || strings.HasPrefix(path, webAPIPrefix+"/")
}

func webAPIRequiresCSRF(r *http.Request) bool {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && isWebIssueAttachmentDownloadPath(r.URL.Path) {
		return false
	}

	return true
}

func isWebIssueAttachmentDownloadPath(requestPath string) bool {
	apiPath := strings.TrimPrefix(requestPath, webAPIPrefix)
	parts := strings.Split(strings.Trim(apiPath, "/"), "/")
	if len(parts) == 5 && parts[0] == "v1" && parts[1] == "issues" && parts[3] == "attachments" && strings.TrimSpace(parts[4]) != "" {
		return true
	}
	if len(parts) == 7 && parts[0] == "v1" && parts[1] == "projects" && parts[3] == "issues" && parts[5] == "attachments" && strings.TrimSpace(parts[6]) != "" {
		return true
	}

	return false
}

func requestWithPath(r *http.Request, path string) *http.Request {
	clone := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = path
	urlCopy.RawPath = ""
	clone.URL = &urlCopy
	return clone
}

func tokenMatches(token string, expected string) bool {
	token = strings.TrimSpace(token)
	expected = strings.TrimSpace(expected)
	if token == "" || expected == "" || len(token) != len(expected) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (s *Server) shouldUseIdempotency(r *http.Request, principal coordinator.Principal) bool {
	if strings.TrimSpace(r.Header.Get(idempotencyHeader)) == "" {
		return false
	}
	if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeSession, coordinator.TokenScopeConsole) {
		return false
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return false
	}

	return strings.HasPrefix(r.URL.Path, "/v1/issues") || strings.HasPrefix(r.URL.Path, "/v1/projects/") || strings.HasPrefix(r.URL.Path, "/v1/changes") || strings.HasPrefix(r.URL.Path, "/v1/sessions") || r.URL.Path == "/v1/jobs" || r.URL.Path == "/v1/reconcile"
}

// idempotencyFor picks the record store for a principal: session tokens use
// their project's table, everything else the coordinator-global one. The
// store is chosen before routing, so it must not depend on the request path.
func (s *Server) idempotencyFor(principal coordinator.Principal) *coordinator.IdempotencyService {
	if principal.IsProjectBound() {
		if bundle, ok := s.registry.Bundle(*principal.ProjectID); ok {
			return bundle.Idempotency
		}
	}

	return s.registry.GlobalIdempotency()
}

func (s *Server) serveIdempotent(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body_failed", err.Error())
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	principalKey := principal.IdempotencyPrincipalKey()
	idempotency := s.idempotencyFor(principal)
	unlock, err := idempotency.Lock(principalKey, idempotencyKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "idempotency_lock_failed", err.Error())
		return
	}
	defer unlock()

	requestHash := coordinator.RequestHash(r.Method, r.URL.RequestURI(), body)
	record, reserved, err := idempotency.Reserve(r.Context(), coordinator.IdempotencyRecord{
		PrincipalKey:   principalKey,
		IdempotencyKey: idempotencyKey,
		Method:         r.Method,
		Path:           r.URL.RequestURI(),
		RequestHash:    requestHash,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "idempotency_reserve_failed", err.Error())
		return
	}
	if !reserved {
		if record.Method != r.Method || record.Path != r.URL.RequestURI() || record.RequestHash != requestHash {
			writeError(w, http.StatusConflict, "idempotency_key_conflict", "idempotency key was already used for a different request")
			return
		}
		if record.StatusCode == coordinator.IdempotencyPendingStatus {
			writeError(w, http.StatusConflict, "idempotency_request_in_progress", "idempotency key is already being processed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(record.StatusCode)
		_, _ = w.Write(record.ResponseBody)
		return
	}

	capture := newResponseCapture()
	s.dispatch(capture, r, principal)
	if capture.statusCode >= 200 && capture.statusCode < 300 {
		if err := idempotency.Complete(r.Context(), coordinator.IdempotencyRecord{
			PrincipalKey:   principalKey,
			IdempotencyKey: idempotencyKey,
			Method:         r.Method,
			Path:           r.URL.RequestURI(),
			RequestHash:    requestHash,
			StatusCode:     capture.statusCode,
			ResponseBody:   capture.body.Bytes(),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "idempotency_save_failed", err.Error())
			return
		}
	} else {
		if err := idempotency.Cancel(r.Context(), principalKey, idempotencyKey, r.Method, r.URL.RequestURI(), requestHash); err != nil {
			writeError(w, http.StatusInternalServerError, "idempotency_cancel_failed", err.Error())
			return
		}
	}
	capture.flush(w)
}
