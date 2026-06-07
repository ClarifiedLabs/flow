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

configure_git_url_rewrite() {
  local from="${FLOW_WORKER_GIT_URL_REWRITE_FROM:-}"
  local to="${FLOW_WORKER_GIT_URL_REWRITE_TO:-}"

  if [ -z "$from" ] || [ -z "$to" ]; then
    return 0
  fi

  from="${from%/}/"
  to="${to%/}/"
  git config --global "url.${to}.insteadOf" "$from"
}

configure_worker_terminal() {
  local public_base_url="${FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL:-}"
  local bind_address="${FLOW_WORKER_TERMINAL_BIND_ADDRESS:-0.0.0.0}"

  if [ -z "$public_base_url" ]; then
    return 0
  fi
  if [ "$public_base_url" = "auto" ]; then
    local first_address
    read -r first_address _ < <(hostname -i)
    if [ -z "$first_address" ]; then
      echo "could not determine worker container IP for terminal public_base_url" >&2
      exit 1
    fi
    public_base_url="http://${first_address}"
  fi

  export FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL="${public_base_url%/}"
  export FLOW_WORKER_TERMINAL_BIND_ADDRESS="$bind_address"
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

configure_worker_terminal "$@"
configure_git_url_rewrite

if should_start_dockerd; then
  start_dockerd_rootless
fi

exec "$@"
