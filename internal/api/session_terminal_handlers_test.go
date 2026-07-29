package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTerminalLoginRedirectUsesTrailingSlash verifies that the terminal login
// flow redirects to the terminal page with a trailing slash. Without the slash,
// relative asset references in terminal.html (e.g. ./vendor/xterm/xterm.css)
// resolve outside the terminal route and fall through to API authentication,
// returning 401 and leaving the terminal blank.
func TestTerminalLoginRedirectUsesTrailingSlash(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Terminal trailing slash task")
	if _, err := fixture.Sessions.RegisterTerminal(context.Background(), started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	access, err := fixture.Sessions.CreateTerminalAccess(context.Background(), started.Session.ID, defaultTerminalAccessTTL)
	if err != nil {
		t.Fatalf("create terminal access: %v", err)
	}

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, access.LoginPath, nil)
	fixture.Server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}

	wantLocation := "/v2/sessions/" + started.Session.ID + "/terminal/"
	if got := login.Header().Get("Location"); got != wantLocation {
		t.Fatalf("login location = %q, want %q", got, wantLocation)
	}

	var terminalCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == terminalAccessCookie {
			terminalCookie = cookie
			break
		}
	}
	if terminalCookie == nil {
		t.Fatalf("terminal access cookie not set; cookies = %+v", login.Result().Cookies())
	}

	pathMatches := func(cookiePath, requestPath string) bool {
		if cookiePath == requestPath {
			return true
		}
		if !strings.HasPrefix(requestPath, cookiePath) {
			return false
		}
		return strings.HasSuffix(cookiePath, "/") || requestPath[len(cookiePath)] == '/'
	}

	if !pathMatches(terminalCookie.Path, wantLocation) {
		t.Fatalf("terminal access cookie path %q does not match redirected page %q", terminalCookie.Path, wantLocation)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	loginURL, err := url.Parse("http://example.com" + access.LoginPath)
	if err != nil {
		t.Fatalf("parse login URL: %v", err)
	}
	jar.SetCookies(loginURL, login.Result().Cookies())

	page := httptest.NewRecorder()
	pageRequest := httptest.NewRequest(http.MethodGet, wantLocation, nil)
	pageURL, err := url.Parse("http://example.com" + wantLocation)
	if err != nil {
		t.Fatalf("parse terminal page URL: %v", err)
	}
	for _, c := range jar.Cookies(pageURL) {
		pageRequest.AddCookie(c)
	}
	fixture.Server.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("terminal page status = %d, want 200; body: %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, `<div id="terminal">`) {
		t.Fatalf("terminal page body missing terminal div; body: %s", body)
	}

	// Relative asset URLs from /v2/sessions/{id}/terminal/ must resolve inside
	// the terminal route, otherwise they fall through to API auth and return 401.
	assets := []string{
		"vendor/xterm/xterm.css",
		"vendor/xterm/xterm.js",
		"vendor/xterm/xterm-addon-fit.js",
		"vendor/xterm/xterm-addon-web-links.js",
		"terminal-clipboard.js",
		"terminal-page.js",
	}
	for _, asset := range assets {
		assetURLPath := wantLocation + asset
		if !pathMatches(terminalCookie.Path, assetURLPath) {
			t.Fatalf("terminal access cookie path %q does not match asset URL %q", terminalCookie.Path, assetURLPath)
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, assetURLPath, nil)
		assetURL, err := url.Parse("http://example.com" + assetURLPath)
		if err != nil {
			t.Fatalf("parse asset URL %q: %v", assetURLPath, err)
		}
		for _, c := range jar.Cookies(assetURL) {
			req.AddCookie(c)
		}
		fixture.Server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("terminal asset %s status = %d, want 200; body: %s", asset, rec.Code, rec.Body.String())
		}
	}
}
