#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=common.sh disable=SC1091
source "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

require_up_prerequisites
ensure_state_dirs
resolve_images
ensure_tokens
ensure_cluster
build_and_load_images
render_manifests
write_client_config

log "building the local owner CLI"
go build -o "${BIN_DIR}/flow" "${REPO_ROOT}/cmd/flow"
chmod 700 "${BIN_DIR}/flow"

log "applying server resources and private Secrets"
kube create namespace flow --dry-run=client -o yaml | kube apply -f -
apply_tokens_secret
apply_harness_model_proxy_secret
kube apply -f "${GENERATED_DIR}/server.yaml"
kube apply -f "${GENERATED_DIR}/flow-server-host.yaml"
kube -n flow rollout restart deployment/flow-server
kube -n flow rollout status deployment/flow-server --timeout="${FLOW_KIND_READY_TIMEOUT:-180s}"

log "applying orchestrator resources after the server is ready"
kube apply -f "${GENERATED_DIR}/orchestrator.yaml"
kube -n flow rollout restart deployment/flow-orchestrator
kube -n flow rollout status deployment/flow-orchestrator --timeout="${FLOW_KIND_READY_TIMEOUT:-180s}"
kube -n flow wait --for=condition=available deployment/flow-server deployment/flow-orchestrator --timeout="${FLOW_KIND_READY_TIMEOUT:-180s}"

log "checking the host-only API mapping with the local client config"
"${BIN_DIR}/flow" jobs --config "${STATE_DIR}/client.yaml" >/dev/null

log "ready: API http://127.0.0.1:${API_HOST_PORT} (telemetry remains cluster-internal)"
log "client: ${BIN_DIR}/flow --config ${STATE_DIR}/client.yaml ..."
log "smoke: ${SCRIPT_DIR}/smoke.sh"
