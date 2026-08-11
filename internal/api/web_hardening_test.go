package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebUICookiesAreSameSiteLax pins the browser CSRF posture: both the
// session and CSRF cookies must be SameSite=Lax so cross-site requests never
// carry them, and scoped to /ui so they are not sent to bearer-token routes.
func TestWebUICookiesAreSameSiteLax(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)

	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", login.Code)
	}

	setCookies := login.Header().Values("Set-Cookie")
	if len(setCookies) != 2 {
		t.Fatalf("Set-Cookie headers = %v, want session and csrf cookies", setCookies)
	}
	for _, header := range setCookies {
		if !strings.Contains(header, "SameSite=Lax") {
			t.Fatalf("cookie missing SameSite=Lax: %q", header)
		}
		if !strings.Contains(header, "Path=/ui") {
			t.Fatalf("cookie missing Path=/ui scoping: %q", header)
		}
		if strings.Contains(header, "SameSite=None") {
			t.Fatalf("cookie must never be SameSite=None: %q", header)
		}
	}
}

// TestWebAPIRejectsMismatchedCSRF pins that a valid session cookie with a
// forged or missing CSRF header is rejected on both reads and writes.
func TestWebAPIRejectsMismatchedCSRF(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	sessionCookie, _ := loginWebUI(t, fixture)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		csrf   string
	}{
		{name: "read forged csrf", method: http.MethodGet, path: "/ui/api/v2/board", csrf: "forged-token"},
		{name: "write forged csrf", method: http.MethodPost, path: "/ui/api/v2/tasks", csrf: "forged-token"},
		{name: "write missing csrf", method: http.MethodPost, path: "/ui/api/v2/tasks", csrf: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"title":"forged"}`))
			req.AddCookie(sessionCookie)
			if tc.csrf != "" {
				req.Header.Set(webCSRFHeader, tc.csrf)
			}
			fixture.Server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestWebAPIRejectsFormEncodedPost pins that browser-form content types are
// never accepted on the mutation surface: a cross-origin form cannot set
// custom headers, but even with a valid CSRF header a urlencoded body must
// not create anything.
func TestWebAPIRejectsFormEncodedPost(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	sessionCookie, csrfCookie := loginWebUI(t, fixture)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/api/v2/tasks", strings.NewReader("title=form-encoded-task"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(webCSRFHeader, csrfCookie.Value)
	req.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(recorder, req)
	if recorder.Code >= 200 && recorder.Code < 300 {
		t.Fatalf("form-encoded post status = %d, want rejection; body: %s", recorder.Code, recorder.Body.String())
	}

	tasks := httptest.NewRecorder()
	tasksRequest := httptest.NewRequest(http.MethodGet, "/ui/api/v2/board", nil)
	tasksRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	tasksRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(tasks, tasksRequest)
	if strings.Contains(tasks.Body.String(), "form-encoded-task") {
		t.Fatalf("form-encoded post created a task: %s", tasks.Body.String())
	}
}

// TestServerNeverSetsCORSHeaders pins that no response path — bearer API,
// cookie-authenticated web API, preflight, or login — ever emits
// Access-Control-* headers. Cross-origin browser access must stay impossible.
func TestServerNeverSetsCORSHeaders(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	sessionCookie, csrfCookie := loginWebUI(t, fixture)

	build := []struct {
		name  string
		build func() *http.Request
	}{
		{
			name: "bearer api with origin",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/v2/board", nil)
				req.Header.Set("Authorization", "Bearer owner-token")
				req.Header.Set("Origin", "https://attacker.example")
				return req
			},
		},
		{
			name: "web api with origin",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/ui/api/v2/board", nil)
				req.Header.Set("Origin", "https://attacker.example")
				req.Header.Set(webCSRFHeader, csrfCookie.Value)
				req.AddCookie(sessionCookie)
				return req
			},
		},
		{
			name: "web api preflight",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodOptions, "/ui/api/v2/tasks", nil)
				req.Header.Set("Origin", "https://attacker.example")
				req.Header.Set("Access-Control-Request-Method", "POST")
				return req
			},
		},
		{
			name: "login with origin",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/ui/login?token=invalid", nil)
				req.Header.Set("Origin", "https://attacker.example")
				return req
			},
		},
		{
			name: "telemetry-style probe path on main server",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/metrics", nil)
			},
		},
	}

	for _, tc := range build {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			fixture.Server.ServeHTTP(recorder, tc.build())
			for name := range recorder.Header() {
				if strings.HasPrefix(strings.ToLower(name), "access-control-") {
					t.Fatalf("response sets CORS header %q: %q", name, recorder.Header().Get(name))
				}
			}
		})
	}
}
