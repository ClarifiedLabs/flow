package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMuxServesTelemetryEndpoints(t *testing.T) {
	reg := New()
	reg.Counter("flow_test_requests_total", "test counter").Inc(nil)
	mux := Mux(reg, func() bool { return true })

	for _, path := range []string{"/livez", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /missing status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestMuxReadyzMapsReadinessToStatus(t *testing.T) {
	ready := false
	mux := Mux(New(), func() bool { return ready })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready: GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("not ready: body = %q, want ok:false", rec.Body.String())
	}

	ready = true
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready: GET /readyz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("ready: body = %q, want ok:true", rec.Body.String())
	}
}

func TestMuxNilReadyFuncIsAlwaysReady(t *testing.T) {
	mux := Mux(New(), nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMuxServesNothingBeyondTelemetrySurface pins the unauthenticated port's
// attack surface: only the three telemetry routes exist, and they are
// read-only (GET-only).
func TestMuxServesNothingBeyondTelemetrySurface(t *testing.T) {
	mux := Mux(New(), func() bool { return true })

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/admin", http.StatusNotFound},
		{http.MethodGet, "/v2/tasks", http.StatusNotFound},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/livez", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}
