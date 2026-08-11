# Local security model

Flow is a developer-local control plane. Every listener binds to loopback by
default, and the browser-facing surface is hardened against drive-by web
attacks. This document is the evidence-anchored audit of that posture; the
pinning tests listed at the end fail if any guarantee regresses.

## Trust boundaries

| # | Boundary | Default bind | Authentication | Evidence |
|---|----------|--------------|----------------|----------|
| 1 | Main API (`/v2/*`) | `127.0.0.1:8421` | Bearer token with scope (`owner`, `hook`, `worker`, `session`, `console`); constant-time token compare | `internal/config/config.go` (`ListenAddr`), `internal/api/web_auth_idempotency.go` (`authenticate`, `authenticateToken`, `tokenMatches`) |
| 2 | Browser UI + web API (`/ui`, `/ui/api/v2/*`) | same listener | Single-use bootstrap → session cookie (HttpOnly, SameSite=Lax, Path=/ui) + CSRF cookie (JS-readable) + `X-Flow-CSRF` header (double-submit) | `internal/api/web_auth_idempotency.go` (`handleWebLogin`, `authenticate`, `webAPIRequiresCSRF`), `internal/coordinator/web_sessions.go` (`Authenticate`) |
| 3 | Git HTTP exchange (`/git/*`) | same listener | HTTP Basic or Bearer; scope check, then worker-slot binding and session liveness checks before any repo access | `internal/api/git_http_handlers.go` (`serveGitHTTPRequest`, `authenticateGit`, `authorizeGitHTTPPrincipal`) |
| 4 | Terminal proxies (`/v2/sessions/*/terminal*`, job terminals) | same listener | Per-session/job access tokens (query param → HttpOnly cookie); owner/worker scope checks on control routes | `internal/api/session_terminal_handlers.go`, `internal/api/worker_terminal_handlers.go` |
| 5 | Telemetry (`/readyz`, `/livez`, `/metrics`) | `127.0.0.1:8422` | **Unauthenticated by design**; GET-only mux with exactly three routes | `internal/config/orchestrator.go` (`DefaultTelemetryListen`), `internal/metrics/probes.go` (`Mux`) |

## Attacker model

Capabilities assumed:

- A malicious web page open in the developer's browser (drive-by).
- An unprivileged process on the same host attempting cross-user access.
- A network neighbor **only if** the operator explicitly rebinds a listener
  off loopback (a deliberate, visible configuration choice).

Non-capabilities (keep severity calibrated):

- No internet exposure by default; there is no TLS terminator, proxy, or port
  forward in the default deployment.
- Browsers cannot read responses cross-origin (no CORS headers are ever
  emitted) and cannot authenticate cross-site writes (SameSite=Lax cookies
  plus a custom CSRF header that forms cannot set).
- The session cookie is scoped to `Path=/ui`, so it is never sent to
  bearer-token routes or to other services on the same port.

## Hardening guarantees (audited 2026-08)

1. **No CORS anywhere.** No handler sets `Access-Control-*` headers on any
   response, including error responses, preflights, and the login flow.
2. **CSRF enforced on the whole web API.** Every `/ui/api/*` request —
   including reads, except GET/HEAD attachment downloads — must present the
   `X-Flow-CSRF` header matching the session's stored token hash; a forged or
   missing token yields 401 (`WebSessionService.Authenticate`).
3. **Browser forms cannot mutate state.** Mutation handlers decode JSON
   strictly, so `application/x-www-form-urlencoded` posts are rejected even
   with a valid session and CSRF header.
4. **Cookies are SameSite=Lax, HttpOnly (session), Path=/ui.** Bootstrap
   tokens are single-use and short-lived; sessions expire and reaping touches
   `last_seen_at` only on successful authentication.
5. **Telemetry is loopback-only by default and serves nothing else.** The mux
   404s every other path and 405s non-GET methods. Kubernetes manifests
   override the bind to `:8422` for cluster-internal scraping
   (`docs/kubernetes.md`, `k8s/`); that is the supported non-loopback case.
6. **Git exchange auth happens before routing side effects.** Authentication,
   scope authorization, worker-slot binding, and session-liveness checks all
   run before the smart-HTTP backend is invoked.
7. **Terminal access is least-privilege.** Browser terminals use per-session
   tokens, not the owner token; worker terminals require the matching worker
   token and, where applicable, a live lease.

## Residual risks and non-goals

- **Telemetry port is unauthenticated.** Acceptable on loopback; if the
  operator binds it to a non-loopback address outside the supported
  Kubernetes topology, process metrics and probe state become network-visible.
  Treat the bind address as a security-sensitive setting.
- **Bearer tokens live in config files on disk.** Protection relies on host
  filesystem permissions; flow does not encrypt them at rest.
- **CSRF comparison uses hash equality**, not a constant-time compare. The
  compared values are SHA-256 hashes of high-entropy random tokens, so timing
  leakage is not exploitable; noted for completeness.
- **Loopback is the security perimeter.** Anything that tunnels the API port
  (SSH forwards, proxies) inherits full owner access if it carries the owner
  token; flow does not attempt to detect or prevent that.

## Pinning tests

| Guarantee | Test |
|-----------|------|
| Cookies SameSite=Lax + Path=/ui | `internal/api/web_hardening_test.go` `TestWebUICookiesAreSameSiteLax` |
| Forged/missing CSRF rejected (read + write) | `TestWebAPIRejectsMismatchedCSRF`, plus `server_test.go` `TestWebUIBootstrapLoginAndCookieAuth` |
| Form-encoded posts rejected, no state change | `TestWebAPIRejectsFormEncodedPost` |
| No `Access-Control-*` headers on any path | `TestServerNeverSetsCORSHeaders` |
| Telemetry mux surface is exactly three GET routes | `internal/metrics/probes_test.go` `TestMuxServesNothingBeyondTelemetrySurface` |
| Telemetry default bind is loopback | `internal/config/orchestrator_test.go` `TestDefaultTelemetryListenIsLoopback` |
| Bootstrap single-use, session expiry | `internal/coordinator/web_sessions_test.go` |
| Git HTTP auth/scope/slot ordering | `internal/api/git_http_handlers_test.go` |
| Terminal token + worker scope checks | `internal/api/session_terminal_handlers_test.go`, `worker_terminal_handlers_test.go` |
