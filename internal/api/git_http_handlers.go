package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

const gitHTTPPrefix = "/git/projects/"

func (s *Server) serveGitHTTPRequest(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, gitHTTPPrefix) {
		return false
	}

	projectID, pathInfo, ok := parseGitHTTPPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	bundle, ok := s.registry.Bundle(projectID)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	exchangePath := strings.TrimSpace(bundle.Project.ExchangePath)
	if exchangePath == "" {
		http.Error(w, "project exchange path is not configured", http.StatusInternalServerError)
		return true
	}

	principal, err := s.authenticateGit(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="Flow Git"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return true
	}
	if err := authorizeGitHTTPPrincipal(principal, projectID, gitHTTPWriteRequest(r, pathInfo)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return true
	}

	if err := serveGitHTTPBackend(r.Context(), w, r, exchangePath, pathInfo, gitPrincipalActor(principal)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	return true
}

func parseGitHTTPPath(requestPath string) (string, string, bool) {
	rest := strings.TrimPrefix(requestPath, gitHTTPPrefix)
	projectID, suffix, ok := strings.Cut(rest, "/")
	if !ok || strings.TrimSpace(projectID) == "" {
		return "", "", false
	}
	if suffix != "exchange.git" && !strings.HasPrefix(suffix, "exchange.git/") {
		return "", "", false
	}

	return projectID, "/" + suffix, true
}

func (s *Server) authenticateGit(r *http.Request) (coordinator.Principal, error) {
	if username, password, ok := r.BasicAuth(); ok {
		if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
			return coordinator.Principal{}, errors.New("missing git credentials")
		}
		return s.authenticateToken(r.Context(), password)
	}

	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(header, authScheme) {
		return s.authenticateToken(r.Context(), strings.TrimSpace(strings.TrimPrefix(header, authScheme)))
	}

	return coordinator.Principal{}, errors.New("missing git credentials")
}

func authorizeGitHTTPPrincipal(principal coordinator.Principal, projectID string, write bool) error {
	if principal.IsProjectBound() && strings.TrimSpace(*principal.ProjectID) != strings.TrimSpace(projectID) {
		return errors.New("credential is not valid for this project")
	}

	switch principal.Scope {
	case coordinator.TokenScopeOwner, coordinator.TokenScopeWorker:
		return nil
	case coordinator.TokenScopeSession:
		return nil
	case coordinator.TokenScopeConsole:
		if !write {
			return nil
		}
	}

	return errors.New("credential is not allowed to access git exchanges")
}

func gitPrincipalActor(principal coordinator.Principal) string {
	switch principal.Scope {
	case coordinator.TokenScopeOwner:
		return "owner"
	case coordinator.TokenScopeWorker:
		subject := strings.TrimSpace(principal.Subject)
		if subject == "" {
			return "worker"
		}
		return "worker:" + subject
	case coordinator.TokenScopeSession:
		subject := strings.TrimSpace(principal.Subject)
		if subject == "" {
			return "session"
		}
		return "session:" + subject
	case coordinator.TokenScopeConsole:
		subject := strings.TrimSpace(principal.Subject)
		if subject == "" {
			return "console"
		}
		return "console:" + subject
	default:
		return principal.Actor()
	}
}

func gitHTTPWriteRequest(r *http.Request, pathInfo string) bool {
	if strings.TrimSpace(r.URL.Query().Get("service")) == "git-receive-pack" {
		return true
	}

	return strings.HasSuffix(pathInfo, "/git-receive-pack")
}

func serveGitHTTPBackend(ctx context.Context, w http.ResponseWriter, r *http.Request, exchangePath string, pathInfo string, principal string) error {
	cmd := exec.CommandContext(ctx, "git", "http-backend")
	cmd.Env = append(cmd.Environ(),
		"GIT_PROJECT_ROOT="+filepath.Dir(exchangePath),
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+pathInfo,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_USER="+principal,
		"REMOTE_ADDR="+remoteAddr(r),
		"FLOW_GIT_PRINCIPAL="+principal,
	)
	if contentType := strings.TrimSpace(r.Header.Get("Content-Type")); contentType != "" {
		cmd.Env = append(cmd.Env, "CONTENT_TYPE="+contentType)
	}
	if r.ContentLength >= 0 {
		cmd.Env = append(cmd.Env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}
	cmd.Stdin = r.Body

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.Len() > 0 {
		return writeCGIResponse(w, stdout.Bytes())
	}
	if err != nil {
		details := strings.TrimSpace(stderr.String())
		if details == "" {
			details = err.Error()
		}
		return fmt.Errorf("git http-backend failed: %s", details)
	}

	return errors.New("git http-backend returned an empty response")
}

func remoteAddr(r *http.Request) string {
	host, _, ok := strings.Cut(strings.TrimSpace(r.RemoteAddr), ":")
	if ok && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func writeCGIResponse(w http.ResponseWriter, response []byte) error {
	headerBytes, body, ok := bytes.Cut(response, []byte("\r\n\r\n"))
	if !ok {
		headerBytes, body, ok = bytes.Cut(response, []byte("\n\n"))
	}
	if !ok {
		return errors.New("git http-backend returned a malformed CGI response")
	}

	status := http.StatusOK
	for _, line := range bytes.Split(headerBytes, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		key, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			return fmt.Errorf("git http-backend returned malformed header %q", string(line))
		}
		name := string(bytes.TrimSpace(key))
		text := string(bytes.TrimSpace(value))
		if strings.EqualFold(name, "Status") {
			fields := strings.Fields(text)
			if len(fields) > 0 {
				parsed, err := strconv.Atoi(fields[0])
				if err != nil {
					return fmt.Errorf("git http-backend returned malformed status %q", text)
				}
				status = parsed
			}
			continue
		}
		w.Header().Add(name, text)
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}
