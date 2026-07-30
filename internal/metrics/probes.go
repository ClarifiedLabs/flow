package metrics

import (
	"encoding/json"
	"net/http"
)

// probeBody is the JSON body served by the readiness and liveness probes.
type probeBody struct {
	OK bool `json:"ok"`
}

// Mux returns an http.Handler serving the shared telemetry endpoints:
// GET /metrics (Prometheus exposition from reg), plus the unauthenticated
// Kubernetes probes GET /readyz and GET /livez. readyFn returning false maps
// to 503; a nil readyFn is treated as always ready.
func Mux(reg *Registry, readyFn func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeProbe(w, http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if readyFn == nil || readyFn() {
			writeProbe(w, http.StatusOK)
			return
		}
		writeProbe(w, http.StatusServiceUnavailable)
	})
	return mux
}

func writeProbe(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(probeBody{OK: status == http.StatusOK})
}
