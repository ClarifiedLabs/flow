#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=common.sh disable=SC1091
source "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

usage() {
  cat <<EOF
Usage: scripts/kind/down.sh [--delete-data]

Deletes the kind cluster named by FLOW_KIND_CLUSTER (default: flow).
The persistent host data in .flow-kind/data is retained unless the explicit,
destructive --delete-data option is supplied. Repeated invocations are safe.
EOF
}

delete_data=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --delete-data) delete_data=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fatal "unknown argument: $1" ;;
  esac
  shift
done

require_command kind
validate_cluster_name
detect_runtime

if cluster_exists; then
  log "DESTRUCTIVE: deleting kind cluster '${CLUSTER_NAME}'"
  kind delete cluster --name "${CLUSTER_NAME}"
else
  log "kind cluster '${CLUSTER_NAME}' is already absent"
fi

if [ "${delete_data}" -eq 1 ]; then
  log "DESTRUCTIVE: removing persistent data ${DATA_DIR}"
  rm -rf "${DATA_DIR}"
else
  log "retaining persistent data ${DATA_DIR} (use --delete-data to remove it)"
fi
