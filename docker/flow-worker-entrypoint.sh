#!/usr/bin/env bash
set -Eeuo pipefail

should_start_dockerd() {
  case "${FLOW_WORKER_DOCKERD:-auto}" in
  1 | true | TRUE | yes | YES | on | ON)
    return 0
    ;;
  0 | false | FALSE | no | NO | off | OFF)
    return 1
    ;;
  "" | auto)
    if [ -S /var/run/docker.sock ] && docker info >/dev/null 2>&1; then
      return 1
    fi
    return 0
    ;;
  *)
    echo "invalid FLOW_WORKER_DOCKERD=${FLOW_WORKER_DOCKERD}; expected auto, true, or false" >&2
    exit 64
    ;;
  esac
}

wait_for_dockerd() {
  local pid="$1"

  for _ in {1..60}; do
    if docker info >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      wait "$pid"
      return 1
    fi
    sleep 1
  done

  echo "timed out waiting for dockerd" >&2
  return 1
}

start_dockerd_rootless() {
  mkdir -p "$XDG_RUNTIME_DIR" "$HOME/.local/share/docker"
  chmod 0700 "$XDG_RUNTIME_DIR"

  local extra_args=()
  if [ -n "${FLOW_WORKER_DOCKERD_ARGS:-}" ]; then
    read -r -a extra_args <<<"${FLOW_WORKER_DOCKERD_ARGS}"
  fi

  local log_path="${FLOW_WORKER_DOCKERD_LOG:-$HOME/.local/share/docker/dockerd.log}"
  dockerd-rootless.sh --host="$DOCKER_HOST" "${extra_args[@]}" >"$log_path" 2>&1 &
  wait_for_dockerd "$!"
}

if should_start_dockerd; then
  start_dockerd_rootless
fi

exec "$@"
