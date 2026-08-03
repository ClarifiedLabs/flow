#!/usr/bin/env bash

# Shared implementation for the local kind workflow. This file is sourced by
# up.sh, smoke.sh, and down.sh; it is not intended to be run directly.

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(CDPATH='' cd -- "${SCRIPT_DIR}/../.." && pwd)"
STATE_DIR="${REPO_ROOT}/.flow-kind"
DATA_DIR="${STATE_DIR}/data"
TOKEN_DIR="${STATE_DIR}/tokens"
BIN_DIR="${STATE_DIR}/bin"
GENERATED_DIR="${STATE_DIR}/generated"
CLUSTER_NAME="${FLOW_KIND_CLUSTER:-flow}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
API_HOST_PORT="${FLOW_KIND_API_HOST_PORT:-8421}"
API_NODE_PORT=30421
WORKER_SELECTOR='app.kubernetes.io/name=flow-worker,app.kubernetes.io/managed-by=flow-orchestrator'

log() {
  printf '[flow-kind] %s\n' "$*"
}

fatal() {
  printf '[flow-kind] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fatal "required command '$1' is not available on PATH"
}

validate_cluster_name() {
  case "${CLUSTER_NAME}" in
    ''|*[!A-Za-z0-9._-]*) fatal "FLOW_KIND_CLUSTER must contain only letters, digits, '.', '_', or '-'" ;;
  esac
  case "${API_HOST_PORT}" in
    ''|*[!0-9]*) fatal "FLOW_KIND_API_HOST_PORT must be an integer from 1 through 65535" ;;
  esac
  if [ "${API_HOST_PORT}" -lt 1 ] || [ "${API_HOST_PORT}" -gt 65535 ]; then
    fatal "FLOW_KIND_API_HOST_PORT must be an integer from 1 through 65535"
  fi
}

ensure_state_dirs() {
  umask 077
  mkdir -p "${STATE_DIR}" "${DATA_DIR}" "${TOKEN_DIR}" "${BIN_DIR}" "${GENERATED_DIR}"
  chmod 700 "${STATE_DIR}" "${DATA_DIR}" "${TOKEN_DIR}" "${BIN_DIR}" "${GENERATED_DIR}"
}

detect_runtime() {
  local requested candidate
  requested="${FLOW_KIND_RUNTIME:-${KIND_EXPERIMENTAL_PROVIDER:-}}"
  if [ -n "${requested}" ]; then
    case "${requested}" in
      docker|podman|nerdctl) ;;
      *) fatal "FLOW_KIND_RUNTIME/KIND_EXPERIMENTAL_PROVIDER must be docker, podman, or nerdctl" ;;
    esac
    candidate="${requested}"
    require_command "${candidate}"
    "${candidate}" info >/dev/null 2>&1 || fatal "container runtime '${candidate}' is installed but not usable; start/configure it and retry"
    RUNTIME_BIN="${candidate}"
  else
    RUNTIME_BIN=""
    for candidate in docker podman nerdctl; do
      if command -v "${candidate}" >/dev/null 2>&1 && "${candidate}" info >/dev/null 2>&1; then
        RUNTIME_BIN="${candidate}"
        break
      fi
    done
    [ -n "${RUNTIME_BIN}" ] || fatal "no usable Docker-compatible runtime found (tried docker, podman, and nerdctl)"
  fi

  if [ "${RUNTIME_BIN}" != docker ]; then
    export KIND_EXPERIMENTAL_PROVIDER="${RUNTIME_BIN}"
  fi
}

require_up_prerequisites() {
  require_command kind
  require_command kubectl
  require_command go
  detect_runtime
  validate_cluster_name
}

cluster_exists() {
  local existing
  while IFS= read -r existing; do
    [ "${existing}" = "${CLUSTER_NAME}" ] && return 0
  done <<EOF
$(kind get clusters 2>/dev/null || true)
EOF
  return 1
}

kube() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

random_token() {
  printf 'fk_%s\n' "$(od -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')"
}

ensure_nonempty_token() {
  local name path
  name="$1"
  path="${TOKEN_DIR}/${name}"
  if [ ! -s "${path}" ]; then
    random_token >"${path}"
  fi
  chmod 600 "${path}"
}

ensure_tokens() {
  ensure_nonempty_token owner
  ensure_nonempty_token hook
  ensure_nonempty_token orchestrator

  case "${FLOW_KIND_ENABLE_JOIN_TOKEN:-0}" in
    1|true|TRUE|yes|YES)
      JOIN_TOKEN_ENABLED=1
      ensure_nonempty_token worker-join
      ;;
    0|false|FALSE|no|NO)
      JOIN_TOKEN_ENABLED=0
      rm -f "${TOKEN_DIR}/worker-join"
      ;;
    *) fatal "FLOW_KIND_ENABLE_JOIN_TOKEN must be true/false or 1/0" ;;
  esac
}

resolve_images() {
  local revision
  revision="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || true)"
  [ -n "${revision}" ] || revision="$(cksum "${REPO_ROOT}/go.sum" | awk '{print $1}')"
  IMAGE_TAG="${FLOW_KIND_IMAGE_TAG:-kind-${revision}}"
  case "${IMAGE_TAG}" in
    ''|*[!A-Za-z0-9_.-]*) fatal "FLOW_KIND_IMAGE_TAG must contain only letters, digits, '.', '_', or '-'" ;;
  esac
  SERVER_IMAGE="flow-server:${IMAGE_TAG}"
  WORKER_IMAGE="flow-worker:${IMAGE_TAG}"
  ORCHESTRATOR_IMAGE="flow-orchestrator:${IMAGE_TAG}"
}

write_kind_config() {
  cat >"${GENERATED_DIR}/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: ${DATA_DIR}
        containerPath: /var/local-path-provisioner
    extraPortMappings:
      - containerPort: ${API_NODE_PORT}
        hostPort: ${API_HOST_PORT}
        listenAddress: "127.0.0.1"
        protocol: TCP
EOF
  chmod 600 "${GENERATED_DIR}/kind.yaml"
}

validate_cluster_shape() {
  local node ports
  node="${CLUSTER_NAME}-control-plane"
  "${RUNTIME_BIN}" exec "${node}" test -f /var/local-path-provisioner/.flow-kind-mount >/dev/null 2>&1 ||
    fatal "existing cluster '${CLUSTER_NAME}' lacks the ${DATA_DIR} data mount; run scripts/kind/down.sh and retry"
  ports="$("${RUNTIME_BIN}" port "${node}" "${API_NODE_PORT}/tcp" 2>/dev/null || true)"
  printf '%s\n' "${ports}" | grep -F "127.0.0.1:${API_HOST_PORT}" >/dev/null 2>&1 ||
    fatal "existing cluster '${CLUSTER_NAME}' lacks host API mapping 127.0.0.1:${API_HOST_PORT}; run scripts/kind/down.sh and retry"
}

ensure_cluster() {
  : >"${DATA_DIR}/.flow-kind-mount"
  chmod 600 "${DATA_DIR}/.flow-kind-mount"
  write_kind_config
  if cluster_exists; then
    log "reusing kind cluster '${CLUSTER_NAME}'"
    validate_cluster_shape
  else
    log "creating kind cluster '${CLUSTER_NAME}' (this is the only step that configures the host mount and port mapping)"
    kind create cluster --name "${CLUSTER_NAME}" --config "${GENERATED_DIR}/kind.yaml"
    validate_cluster_shape
  fi
  kube cluster-info >/dev/null
}

build_and_load_images() {
  local image target
  for target in flow-server flow-worker flow-orchestrator; do
    case "${target}" in
      flow-server) image="${SERVER_IMAGE}" ;;
      flow-worker) image="${WORKER_IMAGE}" ;;
      flow-orchestrator) image="${ORCHESTRATOR_IMAGE}" ;;
    esac
    log "building ${image} (target ${target})"
    "${RUNTIME_BIN}" build --target "${target}" --tag "${image}" "${REPO_ROOT}"
  done
  log "loading local images into '${CLUSTER_NAME}'"
  kind load docker-image --name "${CLUSTER_NAME}" "${SERVER_IMAGE}" "${WORKER_IMAGE}" "${ORCHESTRATOR_IMAGE}"
  cat >"${STATE_DIR}/images.env" <<EOF
FLOW_KIND_IMAGE_TAG=${IMAGE_TAG}
FLOW_KIND_SERVER_IMAGE=${SERVER_IMAGE}
FLOW_KIND_WORKER_IMAGE=${WORKER_IMAGE}
FLOW_KIND_ORCHESTRATOR_IMAGE=${ORCHESTRATOR_IMAGE}
EOF
  chmod 600 "${STATE_DIR}/images.env"
}

strip_flow_tokens_secret() {
  awk '
    function flush() {
      if (doc == "") return
      if (!(doc ~ /(^|\n)kind:[[:space:]]*Secret[[:space:]]*(\n|$)/ && doc ~ /(^|\n)[[:space:]]*name:[[:space:]]*flow-tokens[[:space:]]*(\n|$)/)) {
        if (!first) print "---"
        printf "%s", doc
        first = 0
      }
      doc = ""
    }
    BEGIN { first = 1 }
    /^---[[:space:]]*$/ { flush(); next }
    { doc = doc $0 ORS }
    END { flush() }
  ' "$1"
}

render_manifests() {
  local server_without_secret
  server_without_secret="${GENERATED_DIR}/server-without-secret.yaml"
  strip_flow_tokens_secret "${REPO_ROOT}/k8s/server.yaml" >"${server_without_secret}"
  if [ "${JOIN_TOKEN_ENABLED}" -eq 0 ]; then
    sed -E \
      -e 's/owner hook worker-join orchestrator/owner hook orchestrator/' \
      -e '/^[[:space:]]*- --worker-join-token-file[[:space:]]*$/,+1d' \
      "${server_without_secret}" >"${server_without_secret}.tmp"
    mv "${server_without_secret}.tmp" "${server_without_secret}"
  fi

  grep -E '^[[:space:]]*image:.*flow-server' "${server_without_secret}" >/dev/null ||
    fatal "k8s/server.yaml has no flow-server image to replace"
  sed -E "s|^([[:space:]]*)image:.*flow-server[^[:space:]]*|\\1image: ${SERVER_IMAGE}|" \
    "${server_without_secret}" >"${GENERATED_DIR}/server.yaml"

  grep -E '^[[:space:]]*image:.*flow-worker' "${REPO_ROOT}/k8s/orchestrator.yaml" >/dev/null ||
    fatal "k8s/orchestrator.yaml has no flow-worker image to replace"
  grep -E '^[[:space:]]*image:.*flow-orchestrator' "${REPO_ROOT}/k8s/orchestrator.yaml" >/dev/null ||
    fatal "k8s/orchestrator.yaml has no flow-orchestrator image to replace"
  sed -E \
    -e "s|^([[:space:]]*)image:.*flow-worker[^[:space:]]*|\\1image: ${WORKER_IMAGE}|" \
    -e "s|^([[:space:]]*)image:.*flow-orchestrator[^[:space:]]*|\\1image: ${ORCHESTRATOR_IMAGE}|" \
    "${REPO_ROOT}/k8s/orchestrator.yaml" >"${GENERATED_DIR}/orchestrator.yaml"

  cat >"${GENERATED_DIR}/flow-server-host.yaml" <<EOF
apiVersion: v1
kind: Service
metadata:
  name: flow-server-host
  namespace: flow
  labels:
    app: flow-server
    app.kubernetes.io/managed-by: flow-kind
spec:
  type: NodePort
  selector:
    app: flow-server
  ports:
    - name: api
      port: 8421
      targetPort: api
      nodePort: ${API_NODE_PORT}
EOF
  chmod 600 "${GENERATED_DIR}/"*.yaml
}

write_client_config() {
  cat >"${STATE_DIR}/client.yaml" <<EOF
server_url: http://127.0.0.1:${API_HOST_PORT}
token_file: ${TOKEN_DIR}/owner
data_dir: ${STATE_DIR}/client-data
EOF
  chmod 600 "${STATE_DIR}/client.yaml"
}

apply_tokens_secret() {
  local args
  args=(
    --from-file=owner="${TOKEN_DIR}/owner"
    --from-file=hook="${TOKEN_DIR}/hook"
    --from-file=orchestrator="${TOKEN_DIR}/orchestrator"
  )
  if [ "${JOIN_TOKEN_ENABLED}" -eq 1 ]; then
    args+=(--from-file=worker-join="${TOKEN_DIR}/worker-join")
  fi
  kube create secret generic flow-tokens --namespace flow "${args[@]}" \
    --dry-run=client -o yaml | kube apply -f -
}

worker_resource_count() {
  local kind output
  kind="$1"
  output="$(kube -n flow get "${kind}" -l "${WORKER_SELECTOR}" -o name 2>/dev/null || true)"
  if [ -z "${output}" ]; then
    printf '0\n'
  else
    printf '%s\n' "${output}" | awk 'NF { n++ } END { print n+0 }'
  fi
}

assert_no_worker_resources() {
  local jobs pods
  jobs="$(worker_resource_count jobs)"
  pods="$(worker_resource_count pods)"
  [ "${jobs}" -eq 0 ] && [ "${pods}" -eq 0 ] ||
    fatal "expected zero idle assignment workers, found ${jobs} Job(s) and ${pods} Pod(s)"
}
