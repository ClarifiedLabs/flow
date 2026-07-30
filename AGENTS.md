# Repository Instructions

- Use conventional commits style for Git commit messages.
- When fixing a bug, add regression tests. Do not add regression tests for feature or behavior changes.

## Kubernetes and telemetry

- Every flow binary serves an unauthenticated telemetry port (default `127.0.0.1:8422`) with `/readyz`, `/livez`, and `/metrics` (stdlib-only Prometheus exposition from `internal/metrics`). Keep it cluster-internal. See `docs/kubernetes.md` and `k8s/` for the reference deployment (`flow-server`, ephemeral `flow-worker --ephemeral`, and the `flow-orchestrator` Deployment autoscaler).

## Test Isolation

- Go tests are hermetic by construction: every test package's TestMain routes through `internal/testenv`, which clears the environment and substitutes temporary HOME/XDG/TMP directories before any test runs. Plain `go test ./...` and targeted runs like `go test ./internal/config -run TestLoadClient -count=1` are both safe.
- New test packages must add a `testmain_test.go` containing `func TestMain(m *testing.M) { testenv.Main(m) }`; a root-level test enforces this.
